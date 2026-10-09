// 本文件属于 business 层：把周历页正文里的「第 X 周 / 共 Y 周」解析清楚，
// 并用采样日期推算第 1 周到第 Y 周的周一日期。依据
// docs/decisions/2026-10-09-term-week-from-one-sample.md 与 specs/collector-full-sync/design.md 的周历公式。
// 只取这一句，个人课表等其余内容不解析；HTTP 登录与请求由采集器入口负责。

package calendar

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrNoWeekText 表示正文里没有「第 X 周 / 共 Y 周」这句：非教学周，或者页面结构变了。
var ErrNoWeekText = errors.New("周历页里没有周次文本")

// ErrBadWeekRange 表示取到的周次或总周数不是正整数，或者周次大于总周数。
var ErrBadWeekRange = errors.New("周次与总周数不合法")

// weekTextPattern 是周次那一句的匹配式。
// 实测正文写作「第6周/16周」，需求与契约写作「第 X 周 / 共 16 周」，
// 两种写法都要认，所以「共」与空白可有可无。
var weekTextPattern = regexp.MustCompile(`第\s*(\d+)\s*周\s*/\s*共?\s*(\d+)\s*周`)

// Sample 是一次周历采样的结果：采样日期落在第几周，以及这个学期一共多少周。
type Sample struct {
	Week  int // 采样日期所在的教学周次，来自「第 X 周」
	Total int // 学期总教学周数，来自「共 Y 周」
}

// ParseSample 从周历页正文里取出「第 X 周 / 共 Y 周」这一个采样。
// 取不到文本、数字不是正整数或周次大于总周数都返回失败，调用方据此把本轮标成失败，
// 而不是猜一个周次去写半截周历。
func ParseSample(body []byte) (Sample, error) {
	// 实测这句在正文里唯一，取第一处匹配即可
	match := weekTextPattern.FindSubmatch(body)
	// 非教学周或页面结构变化时这句会消失，此时必须失败
	if match == nil {
		return Sample{}, ErrNoWeekText
	}

	week, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return Sample{}, fmt.Errorf("%w: 周次 %q 不是整数", ErrBadWeekRange, match[1])
	}
	total, err := strconv.Atoi(string(match[2]))
	if err != nil {
		return Sample{}, fmt.Errorf("%w: 总周数 %q 不是整数", ErrBadWeekRange, match[2])
	}

	sample := Sample{Week: week, Total: total}
	// 范围不合法时推不出可靠的学期起点，与生成时用同一处校验判失败
	if err := validateRange(sample); err != nil {
		return Sample{}, err
	}
	return sample, nil
}

// BuildWeeks 用采样日期与采样结果生成第 1 周到第 Y 周的周历行，写成 term_week 的形状。
// sampleDate 必须是 Asia/Shanghai 的当天日期，星期几由它本地推算（周一为 1，周日为 7）。
// 采样数据不合法时返回错误，调用方不得写入。
func BuildWeeks(term string, sampleDate time.Time, sample Sample) ([]model.TermWeek, error) {
	// 采样本身不可信时先返回失败，否则会按错的周次推出整个学期的周一
	if err := validateRange(sample); err != nil {
		return nil, err
	}

	// Go 的 Weekday 里周日是 0，先换算成契约口径的 1=周一 … 7=周日
	weekday := int(sampleDate.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	// 采样日所在那一周的周一 = 采样日期减去（星期几 - 1）天
	sampleMonday := dateOnly(sampleDate).AddDate(0, 0, -(weekday - 1))
	// 第 1 周周一 = 该周一减去（周次 - 1）× 7 天
	firstMonday := sampleMonday.AddDate(0, 0, -7*(sample.Week-1))

	weeks := make([]model.TermWeek, 0, sample.Total)
	for week := 1; week <= sample.Total; week++ {
		// 第 n 周周一 = 第 1 周周一加上（n - 1）× 7 天，教学周按周一到周日连续处理
		monday := firstMonday.AddDate(0, 0, 7*(week-1))
		weeks = append(weeks, model.TermWeek{
			Term:       term,
			Week:       week,
			Monday:     monday,
			InCalendar: true, // 采样生成的行都算教学周
		})
	}
	return weeks, nil
}

// validateRange 校验周次与总周数：都必须是正整数，且周次不能超过总周数。
func validateRange(sample Sample) error {
	// 0 或负数说明这句被截断，或根本不是周次文本
	if sample.Week < 1 || sample.Total < 1 {
		return fmt.Errorf("%w: 第 %d 周 / 共 %d 周", ErrBadWeekRange, sample.Week, sample.Total)
	}
	// 周次大于总周数说明两段数字不配套，推出来的第 1 周会落在学期开始之前
	if sample.Week > sample.Total {
		return fmt.Errorf("%w: 第 %d 周 / 共 %d 周", ErrBadWeekRange, sample.Week, sample.Total)
	}
	return nil
}

// dateOnly 把时间归一成只含年月日的日期：周次推算只看年月日，落库的也是 DATE。
// 与查询侧同一套口径，避免时分秒与地点把日期挪走一天。
func dateOnly(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}
