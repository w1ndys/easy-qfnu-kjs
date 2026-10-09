// 本文件属于 business 层：把目标日期解析成学期、周次与星期。
// 依据 docs/contract/db.v2.sql 的约定——每周的周一日期入库，消费方不再自己算周次。

package query

import (
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ResolveCalendar 在学期周历里找覆盖该日期的那一周。
// 找不到覆盖行就是不教学周：返回 InCalendar=false，这属于正常情况而不是错误。
func ResolveCalendar(weeks []model.TermWeek, date time.Time) model.CalendarContext {
	result := model.CalendarContext{
		Date:       dateOnly(date),
		Weekday:    weekdayNumber(date),
		InCalendar: false,
	}

	bestIndex := -1
	for index := range weeks {
		// 教学周历外的周次不参与解析，避免把假期算成某一周
		if !weeks[index].InCalendar {
			continue
		}
		// 只有周一落在该日期所在那一周内的行才算覆盖
		if !coversDate(weeks[index].Monday, date) {
			continue
		}
		// 学期切换期可能有两行覆盖同一天，取更晚的那一周（也就是新学期的记录）
		if bestIndex == -1 || weeks[index].Monday.After(weeks[bestIndex].Monday) {
			bestIndex = index
		}
	}

	// 没有任何覆盖行时不填学期与周次，前端据 InCalendar 提示不在教学周
	if bestIndex == -1 {
		return result
	}

	result.Term = weeks[bestIndex].Term
	result.Week = weeks[bestIndex].Week
	result.InCalendar = true
	return result
}

// coversDate 判断日期是否落在以 monday 开始的那一周（含周一与周日）。
func coversDate(monday, date time.Time) bool {
	// 两边都归一到日期再比较，避免时分秒带来偏差
	weekStart := dateOnly(monday)
	day := dateOnly(date)
	if day.Before(weekStart) {
		return false
	}
	return day.Before(weekStart.AddDate(0, 0, 7))
}

// dateOnly 去掉时分秒，只保留日历日期。
func dateOnly(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// weekdayNumber 返回 1=周一 … 7=周日，与契约的 weekday 字段一致。
func weekdayNumber(date time.Time) int {
	weekday := int(date.Weekday())
	// Go 里周日是 0，契约里周日是 7，需要换算
	if weekday == 0 {
		return 7
	}
	return weekday
}
