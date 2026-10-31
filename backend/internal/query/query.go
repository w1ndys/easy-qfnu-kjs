// Package query 提供公开只读查询：页面上下文、空教室、全天状态、大屏聚合。
package query

import (
	"context"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// shanghai 是业务时区。固定 +08:00 而不是 LoadLocation：
// 2026 年之后的日期没有夏令时，固定偏移与 Asia/Shanghai 完全等价，
// 也免去容器里缺 tzdata 时 LoadLocation 失败的分支。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// WeekLookup 按业务日期取覆盖该日的学期周；用函数类型便于直接注入实现或替身。
type WeekLookup func(ctx context.Context, date time.Time) (model.TermWeek, bool, error)

// ContextInfo 是页面上下文的对外形状。
type ContextInfo struct {
	Date       string `json:"date"`        // 业务日期，YYYY-MM-DD，按 Asia/Shanghai 解释
	Term       string `json:"term"`        // 学期标识；不在学期周时为空
	Week       int    `json:"week"`        // 教学周序号；不在学期周时为 0
	Weekday    int    `json:"weekday"`     // 星期，1=周一 … 7=周日；不在学期周时为 0
	InCalendar bool   `json:"in_calendar"` // 该日是否落在教学周内
}

// Service 是公开查询入口，持有数据访问依赖。
type Service struct {
	weekLookup   WeekLookup         // 按日期取学期周，由装配层注入
	availability AvailabilityLookup // 空教室读取，由装配层注入
	day          DayLookup          // 全天状态读取，由装配层注入
}

// New 组装查询服务。
func New(weekLookup WeekLookup, availability AvailabilityLookup, day DayLookup) *Service {
	return &Service{weekLookup: weekLookup, availability: availability, day: day}
}

// Context 计算某一天属于哪个学期第几周、星期几、是否在教学周内。
func (s *Service) Context(ctx context.Context, now time.Time) (ContextInfo, error) {
	local := now.In(shanghai)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)

	tw, ok, err := s.weekLookup(ctx, date)
	if err != nil {
		return ContextInfo{}, err
	}

	info := ContextInfo{Date: date.Format(time.DateOnly)}
	// 没有学期周覆盖说明这天在学期之外（寒暑假等），按空字段返回而不是报错。
	if !ok {
		return info, nil
	}

	info.Term = tw.Term
	info.Week = int(tw.Week)
	info.Weekday = weekdayOf(date, tw.Monday)
	info.InCalendar = tw.InCalendar
	return info, nil
}

// weekdayOf 用日历日算差，避免经过时刻与夏令时：1=周一 … 7=周日。
func weekdayOf(date, monday time.Time) int {
	// 两端都归一到 UTC 的 00:00，只比较年月日，时区不再参与运算。
	d := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	m := time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, time.UTC)
	days := int(d.Sub(m).Hours() / 24)
	// 覆盖区间最多 7 天，越界说明数据有问题，夹到 1..7 避免把脏数据放大。
	if days < 0 {
		return 1
	}
	if days > 6 {
		return 7
	}
	return days + 1
}
