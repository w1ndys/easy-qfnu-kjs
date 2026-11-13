package collector

import (
	"testing"
	"time"
)

// TestAnchorMonday 锁住学期第 1 周周一的换算口径（Requirement 3.2、CP4 的锚点侧）。
//
// 公式是「采样日所在周周一 − 7×(C−1)」，日期按 Asia/Shanghai（固定 +08:00）解释。
// 用例特意覆盖三种容易写错的地方：周日不是下一周的开始；采样时刻带 UTC 时区时
// 必须先换算到业务时区再取日期；跨月与跨年不能用「减 30 天」之类的近似。
func TestAnchorMonday(t *testing.T) {
	// mustShanghai 造一个业务时区下的时刻，用例表里只写日期与时分便于核对。
	mustShanghai := func(layout, value string) time.Time {
		t.Helper()
		at, err := time.ParseInLocation(layout, value, shanghai)
		if err != nil {
			t.Fatalf("解析时刻 %q 失败: %v", value, err)
		}
		return at
	}
	mustUTC := func(value string) time.Time {
		t.Helper()
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("解析 UTC 时刻 %q 失败: %v", value, err)
		}
		return at
	}

	cases := []struct {
		name        string
		now         time.Time
		currentWeek int
		want        string
	}{
		{
			name:        "设计口径：11-04 第 9 周",
			now:         mustShanghai("2006-01-02 15:04", "2026-11-04 09:30"),
			currentWeek: 9,
			want:        "2026-09-07",
		},
		{
			name:        "第 1 周就是采样日所在周的周一",
			now:         mustShanghai("2006-01-02 15:04", "2026-09-09 08:00"),
			currentWeek: 1,
			want:        "2026-09-07",
		},
		{
			name:        "周日仍算本周",
			now:         mustShanghai("2006-01-02 15:04", "2026-11-08 23:59"),
			currentWeek: 9,
			want:        "2026-09-07",
		},
		{
			name:        "跨月：十月采样反推到八月",
			now:         mustShanghai("2006-01-02 15:04", "2026-10-01 08:00"),
			currentWeek: 5,
			want:        "2026-08-31",
		},
		{
			name:        "跨年：一月采样反推到上一年九月",
			now:         mustShanghai("2006-01-02 15:04", "2027-01-06 10:00"),
			currentWeek: 18,
			want:        "2026-09-07",
		},
		{
			name:        "UTC 采样时刻按上海日期解释",
			now:         mustUTC("2026-11-01T17:00:00Z"),
			currentWeek: 1,
			want:        "2026-11-02",
		},
		{
			name:        "上海周日深夜的 UTC 前一日",
			now:         mustUTC("2026-11-01T15:59:00Z"),
			currentWeek: 1,
			want:        "2026-10-26",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := time.ParseInLocation(time.DateOnly, c.want, shanghai)
			if err != nil {
				t.Fatalf("解析期望日期 %q 失败: %v", c.want, err)
			}
			got := anchorMonday(c.now, c.currentWeek)
			// 用 Equal 比时刻而不是只比日期：返回值的时区也必须落在业务时区，
			// 否则 2026-09-07 00:00 +08:00 会变成前一天的 UTC 零点，落库时少一天。
			if !got.Equal(want) {
				t.Fatalf("第 %d 周锚点期望 %s，实际 %s", c.currentWeek, want.Format(time.RFC3339), got.Format(time.RFC3339))
			}
			if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
				t.Fatalf("锚点应折成当日零点，实际 %s", got.Format(time.RFC3339))
			}
		})
	}
}
