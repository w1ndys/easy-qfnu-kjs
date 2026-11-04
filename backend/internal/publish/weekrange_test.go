package publish

import "testing"

// TestParseWeekRange 覆盖周次范围解析的收窄契约：只认一个数字或两个数字的区间，
// 连接号必须是连接号族（- ~ ～ — –）里的一个，前后缀只允许「第」与「周」。
// 其它写法（列表、节次后缀、任意前后缀、全角数字、倒挂、0 周）一律判解析失败，不猜。
func TestParseWeekRange(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		from int
		to   int
		ok   bool
	}{
		{"标准区间", "1-16", 1, 16, true},
		{"带周字", "1-16周", 1, 16, true},
		{"带第字与空白", " 第1 - 16 周 ", 1, 16, true},
		{"半角波浪号", "3~4", 3, 4, true},
		{"全角波浪号", "3～4", 3, 4, true},
		{"全角破折号", "3—4", 3, 4, true},
		{"短破折号", "3–4", 3, 4, true},
		{"单周", "5", 5, 5, true},
		{"带周字单周", "第5周", 5, 5, true},
		{"空文本", "", 0, 0, false},
		{"纯空白", "   ", 0, 0, false},
		{"没有数字", "单周", 0, 0, false},
		{"逗号列表", "1,3,5", 0, 0, false},
		{"倒挂区间", "16-1", 0, 0, false},
		{"零周", "0-5", 0, 0, false},
		{"全角数字", "１-１６", 0, 0, false},
		{"节次后缀不收", "1-16节", 0, 0, false},
		{"任意前缀不收", "周次：1-16", 0, 0, false},
		{"三段连接", "1-2-3", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := parseWeekRange(tc.raw)
			if ok != tc.ok || from != tc.from || to != tc.to {
				t.Fatalf("parseWeekRange(%q) = (%d,%d,%v), want (%d,%d,%v)",
					tc.raw, from, to, ok, tc.from, tc.to, tc.ok)
			}
		})
	}
}
