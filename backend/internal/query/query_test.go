package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// fakeLookup 按调用方给的周次造一条学期周，用于验证整形逻辑。
func fakeLookup(week int16, term string, monday string) WeekLookup {
	return func(_ context.Context, _ time.Time) (model.TermWeek, bool, error) {
		m, err := time.Parse(time.DateOnly, monday)
		if err != nil {
			return model.TermWeek{}, false, err
		}
		return model.TermWeek{Term: term, Week: week, Monday: m, InCalendar: true}, true, nil
	}
}

// missingLookup 模拟"这天不在任何学期周"。
func missingLookup() WeekLookup {
	return func(context.Context, time.Time) (model.TermWeek, bool, error) {
		return model.TermWeek{}, false, nil
	}
}

// brokenLookup 模拟数据访问失败。
func brokenLookup() WeekLookup {
	return func(context.Context, time.Time) (model.TermWeek, bool, error) {
		return model.TermWeek{}, false, errors.New("db down")
	}
}

// TestContextWeekday 覆盖 1..7 的星期编码与业务日期换算。
func TestContextWeekday(t *testing.T) {
	// 第 5 周周一为 2026-10-19（周一），据此推一周内每一天。
	cases := []struct {
		name     string
		utcNow   string
		wantDate string
		wantDay  int
	}{
		{name: "周一", utcNow: "2026-10-19T02:00:00Z", wantDate: "2026-10-19", wantDay: 1},
		{name: "周三", utcNow: "2026-10-21T02:00:00Z", wantDate: "2026-10-21", wantDay: 3},
		{name: "周日", utcNow: "2026-10-25T02:00:00Z", wantDate: "2026-10-25", wantDay: 7},
		// UTC 16:30 已经是上海次日 00:30，验证业务日期按 +08:00 切日。
		{name: "跨日边界落到次日", utcNow: "2026-10-21T16:30:00Z", wantDate: "2026-10-22", wantDay: 4},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.utcNow)
			if err != nil {
				t.Fatalf("解析时间失败: %v", err)
			}
			svc := New(fakeLookup(5, "2026-2027-1", "2026-10-19"), nil, nil)
			info, err := svc.Context(t.Context(), now)
			if err != nil {
				t.Fatalf("期望成功，实际 err=%v", err)
			}
			if info.Date != c.wantDate {
				t.Fatalf("日期期望 %s，实际 %s", c.wantDate, info.Date)
			}
			if info.Weekday != c.wantDay {
				t.Fatalf("星期期望 %d，实际 %d", c.wantDay, info.Weekday)
			}
			if info.Week != 5 || info.Term != "2026-2027-1" || !info.InCalendar {
				t.Fatalf("学期信息不符: %+v", info)
			}
		})
	}
}

// TestContextOutsideTerm 覆盖不在学期周的日期：返回空字段而不是错误。
func TestContextOutsideTerm(t *testing.T) {
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	info, err := New(missingLookup(), nil, nil).Context(t.Context(), now)
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if info.Term != "" || info.Week != 0 || info.Weekday != 0 || info.InCalendar {
		t.Fatalf("学期外应返回空字段，实际 %+v", info)
	}
	if info.Date != "2026-07-20" {
		t.Fatalf("日期仍应给出，实际 %s", info.Date)
	}
}

// TestContextNonTeachingWeek 覆盖落在学期周里但该周被标为非教学周：
// 与「不在学期周」同口径，term/week/weekday 置零、in_calendar 为 false，只保留日期。
func TestContextNonTeachingWeek(t *testing.T) {
	nonTeaching := func(_ context.Context, _ time.Time) (model.TermWeek, bool, error) {
		return model.TermWeek{
			Term:       "2026-2027-1",
			Week:       5,
			Monday:     time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
			InCalendar: false,
		}, true, nil
	}

	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	info, err := New(nonTeaching, nil, nil).Context(t.Context(), now)
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if info.Term != "" || info.Week != 0 || info.Weekday != 0 || info.InCalendar {
		t.Fatalf("非教学周应置零，实际 %+v", info)
	}
	if info.Date != "2026-10-21" {
		t.Fatalf("日期仍应给出，实际 %s", info.Date)
	}
}

// TestContextError 覆盖数据访问失败：错误原样上抛。
func TestContextError(t *testing.T) {
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	if _, err := New(brokenLookup(), nil, nil).Context(t.Context(), now); err == nil {
		t.Fatal("期望返回错误，实际 nil")
	}
}
