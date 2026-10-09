// 本文件是 business 层的单元测试：日期 → 学期/周次/星期的解析，不依赖数据库。

package query

import (
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testDate 构造只含日期的 UTC 时间，与库里 date 列的扫描结果一致。
func testDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// testWeeks 是两份学期周历：第一学期第 1 周从 2025-09-15（周一）开始，
// 第二学期第 2 周被标记为非教学周（假期）。
func testWeeks() []model.TermWeek {
	return []model.TermWeek{
		{Term: "2025-2026-1", Week: 1, Monday: testDate(2025, 9, 15), InCalendar: true},
		{Term: "2025-2026-1", Week: 2, Monday: testDate(2025, 9, 22), InCalendar: true},
		{Term: "2025-2026-2", Week: 1, Monday: testDate(2026, 2, 23), InCalendar: true},
		{Term: "2025-2026-2", Week: 2, Monday: testDate(2026, 3, 2), InCalendar: false},
	}
}

// TestResolveCalendar 覆盖周内各天、非教学周与完全没有周历的位置。
func TestResolveCalendar(t *testing.T) {
	cases := []struct {
		name       string    // 用例名称，失败时打出来
		date       time.Time // 待解析的目标日期
		term       string    // 期望的学期
		week       int       // 期望的教学周次
		weekday    int       // 期望的星期，1=周一 … 7=周日
		inCalendar bool      // 期望是否落在教学周历内
	}{
		{"周一", testDate(2025, 9, 15), "2025-2026-1", 1, 1, true},
		{"周三", testDate(2025, 9, 17), "2025-2026-1", 1, 3, true},
		{"周日仍属于同一周", testDate(2025, 9, 21), "2025-2026-1", 1, 7, true},
		{"下一周", testDate(2025, 9, 22), "2025-2026-1", 2, 1, true},
		{"新学期", testDate(2026, 2, 23), "2025-2026-2", 1, 1, true},
		{"非教学周", testDate(2026, 3, 2), "", 0, 1, false},
		{"完全不在周历内", testDate(2025, 10, 6), "", 0, 1, false},
	}

	for _, testCase := range cases {
		result := ResolveCalendar(testWeeks(), testCase.date)
		if result.Term != testCase.term || result.Week != testCase.week {
			t.Errorf("%s：term/week = %s/%d，期望 %s/%d", testCase.name, result.Term, result.Week, testCase.term, testCase.week)
		}
		if result.Weekday != testCase.weekday {
			t.Errorf("%s：weekday = %d，期望 %d", testCase.name, result.Weekday, testCase.weekday)
		}
		if result.InCalendar != testCase.inCalendar {
			t.Errorf("%s：in_calendar = %v，期望 %v", testCase.name, result.InCalendar, testCase.inCalendar)
		}
	}
}

// TestResolveCalendarPicksLatestWeek 校验同一天被两行覆盖时取更晚的那一周。
func TestResolveCalendarPicksLatestWeek(t *testing.T) {
	weeks := []model.TermWeek{
		{Term: "旧学期", Week: 9, Monday: testDate(2025, 9, 15), InCalendar: true},
		{Term: "新学期", Week: 1, Monday: testDate(2025, 9, 20), InCalendar: true},
	}

	// 2025-09-21 同时落在两行里，应当取周一更晚的新学期记录
	result := ResolveCalendar(weeks, testDate(2025, 9, 21))
	if result.Term != "新学期" || result.Week != 1 {
		t.Errorf("重叠覆盖时取了 %s/%d，期望 新学期/1", result.Term, result.Week)
	}
}
