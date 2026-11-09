package cronexpr

import (
	"errors"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// cronPropertySeed 固定属性测试的随机种子：结果可复现，门禁的绿红不取决于当次
// 抽到哪批样本，失败时也能按同一种子重跑（与 pipeline 的既有做法一致）。
const cronPropertySeed = 20261108

// cronPropertyConfig 返回带固定种子的 quick 配置。
func cronPropertyConfig() *quick.Config {
	return &quick.Config{MaxCount: 200, Rand: rand.New(rand.NewSource(cronPropertySeed))}
}

// businessTime 把 "2006-01-02T15:04" 形状的字面量按业务时区解析成时刻。
// 表的可读性依赖这个形状：写 "2026-11-08T23:59" 比写一串 time.Date 更容易校对。
func businessTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.ParseInLocation("2006-01-02T15:04", value, model.BusinessLocation())
	if err != nil {
		t.Fatalf("测试用时刻 %q 解析失败：%v", value, err)
	}

	return parsed
}

// TestParseAcceptsLegalExpressions 钉住语法白名单：只有 `*`、`*/n`、`a`、`a-b`、
// `a-b/n` 与逗号列表能通过，且周字段接受 0–7（7 归一为周日）。
func TestParseAcceptsLegalExpressions(t *testing.T) {
	expressions := []string{
		"* * * * *",
		"*/5 * * * *",
		"10 4 * * 6", // 默认值：每周六 04:10，5 字段
		"30 8-18/2 * * 1-5",
		"0,30 * * * *",
		"0 0 * * 7", // 7 归一为周日
		"1,2,3,4,5 0-23 1-31 1-12 0-7",
		"0-59/59 0-23/23 1-31/30 1-12/11 0-7/7",
		"5 5 5 5 5",
		"*/1 * * * *",
		"1,* * * * *", // 列表里的 `*` 合法，但整个字段不算 `*`（见 TestListStarIsRestricted）
	}

	for _, expr := range expressions {
		t.Run(expr, func(t *testing.T) {
			if _, err := Parse(expr); err != nil {
				t.Fatalf("Parse(%q) 应通过，实际返回错误：%v", expr, err)
			}
		})
	}
}

// TestParseRejectsIllegalExpressions 覆盖字段数、取值越界、倒序区间、步长为 0、
// 空列表项、单值带步长、字母名称与 @ 别名、空串与前后空白。每条都断言
// errors.Is(err, ErrInvalid)，并检查文案里有期望的片段。
func TestParseRejectsIllegalExpressions(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string
	}{
		{name: "字段数只有 4 个", expr: "* * * *", want: "需要 5 个字段"},
		{name: "字段数有 6 个", expr: "* * * * * *", want: "需要 5 个字段"},
		{name: "步长为 0", expr: "*/0 * * * *", want: "第 1 个字段（分钟）步长 0 必须 ≥ 1"},
		{name: "区间步长为 0", expr: "1-2/0 * * * *", want: "步长 0 必须 ≥ 1"},
		{name: "倒序区间", expr: "5-1 * * * *", want: "区间 \"5-1\" 起点大于终点"},
		{name: "空列表项", expr: "1,,2 * * * *", want: "列表里有空项"},
		{name: "结尾逗号", expr: "1, * * * *", want: "列表里有空项"},
		{name: "单值带步长", expr: "1/2 * * * *", want: "步长只能跟在"},
		{name: "步长缺失", expr: "*/ * * * *", want: "步长 \"\" 不是数字"},
		{name: "分钟越界", expr: "60 * * * *", want: "第 1 个字段（分钟）取值 60 超出 0–59"},
		{name: "小时越界", expr: "0 24 * * *", want: "第 2 个字段（小时）取值 24 超出 0–23"},
		{name: "日越界到 32", expr: "0 0 32 * *", want: "第 3 个字段（日）取值 32 超出 1–31"},
		{name: "日越界到 0", expr: "0 0 0 * *", want: "取值 0 超出 1–31"},
		{name: "月越界到 13", expr: "0 0 * 13 *", want: "第 4 个字段（月）取值 13 超出 1–12"},
		{name: "月越界到 0", expr: "0 0 * 0 *", want: "取值 0 超出 1–12"},
		{name: "周越界到 8", expr: "0 0 * * 8", want: "第 5 个字段（周）取值 8 超出 0–7"},
		{name: "周列表越界", expr: "0 0 * * 7,8", want: "第 5 个字段（周）取值 8 超出 0–7"},
		{name: "秒级别名", expr: "@every 1h", want: "需要 5 个字段"},
		{name: "周名称", expr: "0 0 * * MON", want: "第 5 个字段（周）\"MON\" 不是数字"},
		{name: "月名称", expr: "0 0 * JAN *", want: "第 4 个字段（月）\"JAN\" 不是数字"},
		{name: "空表达式", expr: "", want: "表达式为空"},
		{name: "只有空白", expr: "   ", want: "表达式为空"},
		{name: "前导空白", expr: " * * * * *", want: "前后不得有多余空白"},
		{name: "尾随空白", expr: "* * * * * ", want: "前后不得有多余空白"},
		{name: "连续空格", expr: "*  * * * *", want: "需要 5 个字段"},
		{name: "负号开头", expr: "-1 * * * *", want: "缺少取值"},
		{name: "区间缺终点", expr: "1- * * * *", want: "缺少取值"},
		{name: "多余区间段", expr: "1-2-3 * * * *", want: "不是数字"},
		{name: "带正号", expr: "+5 * * * *", want: "不是数字"},
		{name: "步长带正号", expr: "*/+5 * * * *", want: "步长 \"+5\" 不是数字"},
		{name: "区间步长带正号", expr: "1-2/+1 * * * *", want: "步长 \"+1\" 不是数字"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Parse(testCase.expr)
			if err == nil {
				t.Fatalf("Parse(%q) 应被拒绝，实际通过", testCase.expr)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse(%q) 的错误应包装 ErrInvalid，实际 %v", testCase.expr, err)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Parse(%q) 错误文案 %q 不含 %q", testCase.expr, err.Error(), testCase.want)
			}
		})
	}
}

// TestParseErrorNamesFieldPosition 逐个字段钉住「第几个 + 字段名」：
// 42201 的文案直接透出这句，位置错了管理员会去改错的字段。
func TestParseErrorNamesFieldPosition(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{expr: "60 * * * *", want: "第 1 个字段（分钟）"},
		{expr: "0 24 * * *", want: "第 2 个字段（小时）"},
		{expr: "0 0 32 * *", want: "第 3 个字段（日）"},
		{expr: "0 0 * 13 *", want: "第 4 个字段（月）"},
		{expr: "0 0 * * 8", want: "第 5 个字段（周）"},
	}

	for _, testCase := range cases {
		t.Run(testCase.want, func(t *testing.T) {
			_, err := Parse(testCase.expr)
			if err == nil {
				t.Fatalf("Parse(%q) 应被拒绝，实际通过", testCase.expr)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Parse(%q) 错误文案 %q 不含字段位置 %q", testCase.expr, err.Error(), testCase.want)
			}
		})
	}
}

// TestParseStringReturnsOriginal 钉住 String 的口径：返回管理员原写法而不是
// 重新拼一遍的最小写法。启动日志与库里存的值要能逐字对上。
func TestParseStringReturnsOriginal(t *testing.T) {
	const expr = "0,30 8-18/2 * * 1-5"

	schedule, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q) 失败：%v", expr, err)
	}
	if got := schedule.String(); got != expr {
		t.Fatalf("String() = %q，期望原写法 %q", got, expr)
	}

	var zero Schedule
	if got := zero.String(); got != "" {
		t.Fatalf("零值 Schedule 的 String() 应为空串，实际 %q", got)
	}
}

// TestResolveStartup 覆盖启动期回落规则的三种结局：合法值原样、非法值回落默认值、
// 空串按缺失回落默认值。回落规则只有这一份实现，装配层与调度器都调它，所以「返回的
// 生效值能不能直接进日志」「缺失算不算非法」这两条口径都在这一层钉住。
//
// 缺失与非法各由一个标记表达：Missing 只描述「来源没给出值」，Invalid 只描述「值被拒」。
// 调用方按标记选日志级别，不用再拿 Raw 反推缺失；「是否回落」等价于 Expr != Raw，
// 不另存一份字段。
func TestResolveStartup(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantExpr    string
		wantMissing bool
		wantInvalid bool
	}{
		{
			name:     "合法值原样返回",
			raw:      "30 3 * * 1",
			wantExpr: "30 3 * * 1",
		},
		{
			name:     "默认值本身也原样返回",
			raw:      model.DefaultCronExpr,
			wantExpr: model.DefaultCronExpr,
		},
		{
			name:        "非法值回落默认值并记原因",
			raw:         "0 10 4 * * 6",
			wantExpr:    model.DefaultCronExpr,
			wantInvalid: true,
		},
		{
			name:        "空串按缺失回落默认值，不判非法",
			raw:         "",
			wantExpr:    model.DefaultCronExpr,
			wantMissing: true,
		},
		{
			name:        "纯空白同样按缺失处理",
			raw:         "   ",
			wantExpr:    model.DefaultCronExpr,
			wantMissing: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			startup := ResolveStartup(tc.raw)

			if startup.Raw != tc.raw {
				t.Fatalf("Raw = %q，期望原样保留 %q", startup.Raw, tc.raw)
			}
			if startup.Expr != tc.wantExpr {
				t.Fatalf("Expr = %q，期望 %q", startup.Expr, tc.wantExpr)
			}
			if startup.Missing != tc.wantMissing {
				t.Fatalf("Missing = %v，期望 %v", startup.Missing, tc.wantMissing)
			}
			if gotInvalid := startup.Invalid != nil; gotInvalid != tc.wantInvalid {
				t.Fatalf("Invalid = %v，期望是否有错 %v", startup.Invalid, tc.wantInvalid)
			}
			if tc.wantInvalid && !errors.Is(startup.Invalid, ErrInvalid) {
				t.Fatalf("Invalid 期望包装 ErrInvalid，实际 %v", startup.Invalid)
			}
			// 缺失与非法是互斥的两类：一个启动值不可能既「没有」又「被拒」。
			if startup.Missing && startup.Invalid != nil {
				t.Fatalf("缺失与非法同时成立：Missing=%v Invalid=%v", startup.Missing, startup.Invalid)
			}

			// 回落值本身必须能解析：默认值被改坏是代码问题，这一层不负责兜底，
			// 但至少要保证正常路径下调用方拿到的是可以直接装计划的表达式。
			if _, err := Parse(startup.Expr); err != nil {
				t.Fatalf("生效值 %q 无法解析: %v", startup.Expr, err)
			}
		})
	}
}

// TestMatches 覆盖位图匹配的各个字段、业务时区换算、秒被忽略，以及日/周的 OR 口径。
func TestMatches(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		when  time.Time
		match bool
	}{
		{
			name:  "全通配每天都命中",
			expr:  "* * * * *",
			when:  businessTime(t, "2026-11-08T13:37"),
			match: true,
		},
		{
			name:  "步长命中",
			expr:  "*/5 * * * *",
			when:  businessTime(t, "2026-11-08T13:35"),
			match: true,
		},
		{
			name:  "步长不命中",
			expr:  "*/5 * * * *",
			when:  businessTime(t, "2026-11-08T13:36"),
			match: false,
		},
		{
			name:  "周六 04:10 命中",
			expr:  "10 4 * * 6",
			when:  businessTime(t, "2026-11-07T04:10"),
			match: true,
		},
		{
			name:  "周日 04:10 不命中",
			expr:  "10 4 * * 6",
			when:  businessTime(t, "2026-11-08T04:10"),
			match: false,
		},
		{
			name:  "分钟差一分不命中",
			expr:  "10 4 * * 6",
			when:  businessTime(t, "2026-11-07T04:11"),
			match: false,
		},
		{
			name:  "周 7 等价于周日",
			expr:  "0 0 * * 7",
			when:  businessTime(t, "2026-11-08T00:00"),
			match: true,
		},
		{
			name:  "周 7 不命中周一",
			expr:  "0 0 * * 7",
			when:  businessTime(t, "2026-11-09T00:00"),
			match: false,
		},
		{
			name:  "周 0 也是周日",
			expr:  "0 0 * * 0",
			when:  businessTime(t, "2026-11-08T00:00"),
			match: true,
		},
		{
			name:  "区间步长 08:30 命中",
			expr:  "30 8-18/2 * * 1-5",
			when:  businessTime(t, "2026-11-09T08:30"),
			match: true,
		},
		{
			name:  "区间步长 09:30 不命中",
			expr:  "30 8-18/2 * * 1-5",
			when:  businessTime(t, "2026-11-09T09:30"),
			match: false,
		},
		{
			name:  "同一分钟内的秒被忽略",
			expr:  "0 0 * * *",
			when:  businessTime(t, "2026-11-09T00:00").Add(59 * time.Second),
			match: true,
		},
		{
			name:  "日与周同时受限：每月 1 日命中",
			expr:  "0 0 1 * 1",
			when:  businessTime(t, "2026-12-01T00:00"),
			match: true,
		},
		{
			name:  "日与周同时受限：周一命中",
			expr:  "0 0 1 * 1",
			when:  businessTime(t, "2026-12-07T00:00"),
			match: true,
		},
		{
			name:  "日与周同时受限：两者都不命中的普通日不命中",
			expr:  "0 0 1 * 1",
			when:  businessTime(t, "2026-12-02T00:00"),
			match: false,
		},
		{
			name:  "只有周受限时按周匹配",
			expr:  "0 0 * * 1",
			when:  businessTime(t, "2026-12-01T00:00"),
			match: false,
		},
		{
			name:  "日字段 */2 视为受限：偶数日且周一按周命中",
			expr:  "0 0 */2 * 1",
			when:  businessTime(t, "2026-11-16T00:00"),
			match: true,
		},
		{
			name:  "日字段 */2 视为受限：偶数日且非周一不命中",
			expr:  "0 0 */2 * 1",
			when:  businessTime(t, "2026-11-18T00:00"),
			match: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schedule, err := Parse(testCase.expr)
			if err != nil {
				t.Fatalf("Parse(%q) 失败：%v", testCase.expr, err)
			}
			if got := schedule.Matches(testCase.when); got != testCase.match {
				t.Fatalf("%q 在 %v 的 Matches() = %v，期望 %v",
					testCase.expr, testCase.when, got, testCase.match)
			}
		})
	}
}

// TestMatchesConvertsToBusinessLocation 钉住「同一时刻只按 +08:00 的日历判定」：
// 同一串 UTC 时刻在业务时区是次日凌晨，必须按次日匹配。
func TestMatchesConvertsToBusinessLocation(t *testing.T) {
	schedule, err := Parse("0 4 * * *")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	// 2026-11-08T20:00Z 换算到 +08:00 是 2026-11-09 04:00。
	utcTime := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	if !schedule.Matches(utcTime) {
		t.Fatalf("Matches(%v) 应按业务时区的 04:00 命中", utcTime)
	}
}

// TestMatchesZeroSchedule 钉住零值 Schedule 不命中：未经 Parse 的零值位图是全空的，
// 若 Matches 里有「两边都星号就放行」的短路，零值就会命中所有时刻。
func TestMatchesZeroSchedule(t *testing.T) {
	var zero Schedule
	if zero.Matches(businessTime(t, "2026-11-08T00:00")) {
		t.Fatal("零值 Schedule 不应命中任何时刻")
	}
}

// TestNextBoundaries 覆盖跨日、月末、跨年、闰年 2 月 29 日、严格晚于入参，
// 以及日/周 OR 触发在 Next 上的表现。
func TestNextBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after string
		want  string
	}{
		{
			name:  "23:59 跨到次日 00:00",
			expr:  "0 0 * * *",
			after: "2026-11-08T23:59",
			want:  "2026-11-09T00:00",
		},
		{
			name:  "当天已过点则等次日",
			expr:  "30 8 * * *",
			after: "2026-11-08T09:00",
			want:  "2026-11-09T08:30",
		},
		{
			name:  "月末跨到次月 1 日",
			expr:  "0 0 1 * *",
			after: "2026-01-30T12:00",
			want:  "2026-02-01T00:00",
		},
		{
			name:  "上月无 31 日则跳到下一个有 31 日的月",
			expr:  "0 0 31 * *",
			after: "2026-04-01T00:00",
			want:  "2026-05-31T00:00",
		},
		{
			name:  "12 月 31 日跨到次年 1 月 1 日",
			expr:  "10 4 1 1 *",
			after: "2026-12-31T23:59",
			want:  "2027-01-01T04:10",
		},
		{
			name:  "闰年 2 月 29 日",
			expr:  "0 0 29 2 *",
			after: "2026-03-01T00:00",
			want:  "2028-02-29T00:00",
		},
		{
			name:  "非闰年跳过 2 月 29 日",
			expr:  "0 0 29 2 *",
			after: "2027-03-01T00:00",
			want:  "2028-02-29T00:00",
		},
		{
			name:  "2 月 29 日之后的下一个 2 月 29 日",
			expr:  "0 0 29 2 *",
			after: "2028-02-29T00:00",
			want:  "2032-02-29T00:00",
		},
		{
			name:  "周 7 落到周日",
			expr:  "0 0 * * 7",
			after: "2026-11-09T00:00",
			want:  "2026-11-15T00:00",
		},
		{
			name:  "日与周同时受限：先到的那一个",
			expr:  "0 0 1 * 1",
			after: "2026-11-17T00:00",
			want:  "2026-11-23T00:00",
		},
		{
			name:  "日与周同时受限：每月 1 日也能触发",
			expr:  "0 0 1 * 1",
			after: "2026-11-30T00:00",
			want:  "2026-12-01T00:00",
		},
		{
			name:  "入参落在命中分钟的秒上：不重复触发同一分钟",
			expr:  "10 4 * * 6",
			after: "2026-11-07T04:10",
			want:  "2026-11-14T04:10",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schedule, err := Parse(testCase.expr)
			if err != nil {
				t.Fatalf("Parse(%q) 失败：%v", testCase.expr, err)
			}

			after := businessTime(t, testCase.after)
			want := businessTime(t, testCase.want)

			got, ok := schedule.Next(after)
			if !ok {
				t.Fatalf("Next(%q, %v) 应命中 %v，实际返回 false", testCase.expr, after, want)
			}
			if !got.Equal(want) {
				t.Fatalf("Next(%q, %v) = %v，期望 %v", testCase.expr, after, got, want)
			}
			if !got.After(after) {
				t.Fatalf("Next(%q, %v) = %v，必须严格晚于入参", testCase.expr, after, got)
			}
			if !schedule.Matches(got) {
				t.Fatalf("Next(%q, %v) = %v，但 Matches 为 false", testCase.expr, after, got)
			}
		})
	}
}

// TestDefaultCronExprIsSaturday0410 把「默认 cron」与「5 字段语法」钉在同一处：
// model.DefaultCronExpr 若退回 6 段的 `0 10 4 * * 6`，Parse 会因字段数不是 5 直接
// 报错（这里先单独数一遍字段，让失败原因更直白）；若改到别的时刻，下面的
// 周六 / 04:10 断言会红。Requirement 9.5 的默认值只有这一个可执行定义。
func TestDefaultCronExprIsSaturday0410(t *testing.T) {
	if got := len(strings.Split(model.DefaultCronExpr, " ")); got != fieldCount {
		t.Fatalf("model.DefaultCronExpr = %q 应为 %d 个字段，实际 %d 个",
			model.DefaultCronExpr, fieldCount, got)
	}

	schedule, err := Parse(model.DefaultCronExpr)
	if err != nil {
		t.Fatalf("Parse(model.DefaultCronExpr = %q) 失败：%v", model.DefaultCronExpr, err)
	}

	// 2026-11-08 是周日，之后第一个周六是 11-14。
	after := businessTime(t, "2026-11-08T00:00")
	next, ok := schedule.Next(after)
	if !ok {
		t.Fatalf("model.DefaultCronExpr = %q 应能算出下一次触发（after=%v）",
			model.DefaultCronExpr, after)
	}
	if next.Weekday() != time.Saturday || next.Hour() != 4 || next.Minute() != 10 {
		t.Fatalf("Next(%v) = %v，期望落在周六 04:10", after, next)
	}
	if want := businessTime(t, "2026-11-14T04:10"); !next.Equal(want) {
		t.Fatalf("Next(%v) = %v，期望 %v", after, next, want)
	}
}

// TestNextStrictlyLaterThanAfterWithSeconds 钉住「入参带秒时也不会把同一分钟
// 再返回一次」：这是 minutes 粒度实现里最容易漏掉的一处。
func TestNextStrictlyLaterThanAfterWithSeconds(t *testing.T) {
	schedule, err := Parse("10 4 * * 6")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	after := businessTime(t, "2026-11-07T04:10").Add(30 * time.Second)
	got, ok := schedule.Next(after)
	if !ok {
		t.Fatal("Next 应命中下一个周六 04:10")
	}

	want := businessTime(t, "2026-11-14T04:10")
	if !got.Equal(want) {
		t.Fatalf("Next(%v) = %v，期望 %v", after, got, want)
	}
}

// TestNextUsesBusinessLocation 钉住返回时刻落在业务时区：调用方直接拿去打日志或
// 落库，不能让它带着 UTC 或别的时区出去。入参本身就是 UTC。
func TestNextUsesBusinessLocation(t *testing.T) {
	schedule, err := Parse("0 0 * * *")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	// 2026-11-08T15:00Z = 2026-11-08 23:00 +08:00。
	after := time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)
	got, ok := schedule.Next(after)
	if !ok {
		t.Fatal("Next 应命中次日 00:00")
	}
	if got.Location() != model.BusinessLocation() {
		t.Fatalf("Next 返回的时区是 %v，期望业务时区", got.Location())
	}
	if want := businessTime(t, "2026-11-09T00:00"); !got.Equal(want) {
		t.Fatalf("Next(%v) = %v，期望 %v", after, got, want)
	}
}

// TestNextRejectsUnreachableDates 钉住「日历上不存在的日期」：5 年内没有命中就
// 返回 false，而不是让调用方拿着一个回绕到别的月份的时刻继续排。
func TestNextRejectsUnreachableDates(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{name: "2 月 30 日不存在", expr: "0 0 30 2 *"},
		{name: "2 月 31 日不存在", expr: "0 0 31 2 *"},
		{name: "4 月 31 日不存在", expr: "0 0 31 4 *"},
		{name: "6 月 31 日不存在", expr: "0 0 31 6 *"},
		{name: "9 月 31 日不存在", expr: "0 0 31 9 *"},
		{name: "11 月 31 日不存在", expr: "0 0 31 11 *"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schedule, err := Parse(testCase.expr)
			if err != nil {
				t.Fatalf("Parse(%q) 应通过（语法合法，只是不可达）：%v", testCase.expr, err)
			}

			if got, ok := schedule.Next(businessTime(t, "2026-01-01T00:00")); ok {
				t.Fatalf("Next(%q) 应返回 false，实际命中 %v", testCase.expr, got)
			}
		})
	}
}

// TestNextGivesUpAfterFiveYears 钉住 5 年上限本身：2096-02-29 之后的下一个
// 2 月 29 日是 2104-02-29（2100 不是闰年），距 2097 年超过 5 年，按约定返回 false。
// 这不是「日期不可达」，而是搜索上限，测试把两者的区别固定下来。
func TestNextGivesUpAfterFiveYears(t *testing.T) {
	schedule, err := Parse("0 0 29 2 *")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	after := businessTime(t, "2097-01-01T00:00")
	if got, ok := schedule.Next(after); ok {
		t.Fatalf("超过 5 年上限时应返回 false，实际命中 %v", got)
	}
}

// TestStepStarInDayOfMonthIsRestricted 钉住 Vixie 语义：日字段写 `*/2` 不等于 `*`，
// 它与周字段同时受限时按 OR 触发，所以「偶数日或周一」都命中。
// 2026-11-16 是周一且日号 16 为偶数（`*/2` 的位图是 1,3,5,… 并不含 16），
// 能命中的原因只能是周字段，说明 `*/2` 没有被当成 `*`。
func TestStepStarInDayOfMonthIsRestricted(t *testing.T) {
	schedule, err := Parse("0 0 */2 * 1")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	if !schedule.Matches(businessTime(t, "2026-11-16T00:00")) {
		t.Fatal("日字段 */2 应视为受限，周一应由周字段 OR 命中")
	}

	// 对照组：2026-11-18 是周三且日号 18 为偶数（`*/2` 从 1 起只含奇数日），
	// 两个字段都不命中，所以整体不命中。
	if schedule.Matches(businessTime(t, "2026-11-18T00:00")) {
		t.Fatal("2026-11-18（周三、偶数日）不应命中")
	}

	got, ok := schedule.Next(businessTime(t, "2026-11-15T00:00"))
	if !ok {
		t.Fatal("Next 应命中年月日 2026-11-16")
	}
	if want := businessTime(t, "2026-11-16T00:00"); !got.Equal(want) {
		t.Fatalf("Next = %v，期望 %v", got, want)
	}
}

// TestListStarIsRestricted 覆盖 parseItem 里 `base == "*"` 的分支：列表里的 `*`
// （如 `1,*`）会把位图填满，但这个字段并不**恰为** `*`，所以 domStar/dowStar
// 必须仍是 false。方向别写反：位图已满的日字段单独看**每天**都命中，若误判成
// domStar=true，`0 0 1,* * 3` 就只剩周字段生效、退化成「只在周三命中」——
// 这是欠触发而不是过触发。所以除了断言两个标记，还要钉住周三以外的日期也命中。
func TestListStarIsRestricted(t *testing.T) {
	schedule, err := Parse("0 0 1,* * 1,*")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}

	if schedule.domStar {
		t.Fatal("日字段写成列表 `1,*` 不算不受限，domStar 应为 false")
	}
	if schedule.dowStar {
		t.Fatal("周字段写成列表 `1,*` 不算不受限，dowStar 应为 false")
	}

	// 误判的后果只有换一条表达式才看得出来：日字段是 `1,*`（位图满），周字段是
	// `3`，所以每天都能由日字段命中；domStar 若误判成 true 就只按周匹配。
	// 2026-12-01 是周二、不是周三，用来区分这两种口径。
	restricted, err := Parse("0 0 1,* * 3")
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	if !restricted.Matches(businessTime(t, "2026-12-01T00:00")) {
		t.Fatal("2026-12-01（周二）应命中：`1,*` 在日字段仍算受限，每天由日字段命中")
	}
}

// TestNextAgainstBruteForceOracle 用逐分钟穷举当期望值，覆盖属性测试生成器
// 故意避开的两类输入：月份受限（命中区间可能长达一两年，按 31 天上限扫描会过慢）
// 与闰年 2 月 29 日（4 年一个周期）。这里每个用例只扫一次，代价可控。
func TestNextAgainstBruteForceOracle(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after string
	}{
		{name: "日与周 OR 触发", expr: "0 0 1 * 1", after: "2026-11-17T00:00"},
		{name: "跳过多个月才到 31 日", expr: "0 0 31 * *", after: "2026-04-01T00:00"},
		{name: "闰年 2 月 29 日", expr: "0 0 29 2 *", after: "2026-03-01T00:00"},
		{name: "区间步长加周字段", expr: "30 8-18/2 * * 1-5", after: "2026-11-07T12:00"},
		{name: "分钟与小时都带步长", expr: "*/17 */5 * * *", after: "2026-11-08T13:37"},
		{name: "跨年", expr: "0 0 1 1 *", after: "2026-01-01T00:00"},
		{name: "月份与周同时受限", expr: "0 0 * 12 1", after: "2026-11-01T00:00"},
	}

	loc := model.BusinessLocation()
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			schedule, err := Parse(testCase.expr)
			if err != nil {
				t.Fatalf("Parse(%q) 失败：%v", testCase.expr, err)
			}

			after := businessTime(t, testCase.after)
			next, ok := schedule.Next(after)
			if !ok {
				t.Fatalf("Next(%q, %v) 应命中", testCase.expr, after)
			}
			if !schedule.Matches(next) {
				t.Fatalf("Next(%q, %v) = %v，但 Matches 为 false", testCase.expr, after, next)
			}

			from := after.In(loc).Truncate(time.Minute).Add(time.Minute)
			for minute := from; minute.Before(next); minute = minute.Add(time.Minute) {
				if schedule.Matches(minute) {
					t.Fatalf("(%v, %v) 内有更早命中 %v（Next 返回 %v，表达式 %q）",
						after, next, minute, next, testCase.expr)
				}
			}
		})
	}
}

// cronSample 是 Next 属性测试的输入：一串合法表达式 + 一个随机入参时刻。
// 生成器只造「31 天内一定命中」的表达式（见 generateCronSample），
// 这样属性测试里的区间扫描封顶 31 天，不会把预算耗在极低命中的样本上。
type cronSample struct {
	expr  string
	after time.Time
}

// Generate 实现 testing/quick.Generator；size 与生成范围无关，故不接收。
func (cronSample) Generate(rnd *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(generateCronSample(rnd))
}

// generateCronSample 生成一个随机表达式与随机入参。月份恒为 `*`；日受限时取
// 1–28（每月都有），周受限时取 0–6（每周都有），日周同时受限时按 OR 口径也一定
// 在 31 天内命中，因此扫描上限保持 33 天；日周都不受限时每天都命中。
func generateCronSample(rnd *rand.Rand) cronSample {
	dayOfMonth := "*"
	dayOfWeek := "*"

	switch rnd.Intn(4) {
	case 1:
		// 日受限且避开 29–31：每月都有这些日号。
		dayOfMonth = randomField(rnd, 1, 28)
	case 2:
		// 周受限且避开 7（归一后与 0 重复）：每周都会命中。
		dayOfWeek = randomField(rnd, 0, 6)
	case 3:
		// 两者同时受限：OR 口径下仍每月至少命中一次（日号 ≤28），
		// 若实现误写成 AND，命中区间会拉长到扫描上限之外，属性测试即失败。
		dayOfMonth = randomField(rnd, 1, 28)
		dayOfWeek = randomField(rnd, 0, 6)
	}

	expr := strings.Join([]string{
		randomField(rnd, 0, 59), // 分钟
		randomField(rnd, 0, 23), // 小时
		dayOfMonth,
		"*", // 月恒为 *，保证一年内必然循环到
		dayOfWeek,
	}, " ")

	// 起点落在 2026-01-01 之后的两年内，覆盖闰年之外的常规日历，
	// 也让样本跨过月末与年末。
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, model.BusinessLocation())
	after := base.Add(time.Duration(rnd.Int63n(2*365*24*60)) * time.Minute)
	after = after.Add(time.Duration(rnd.Intn(60)) * time.Second)

	return cronSample{expr: expr, after: after}
}

// randomField 生成一个字段的随机写法，覆盖 `*`、`*/n`、`a`、`a-b`、`a-b/n`
// 与逗号列表六种形态；区间与步长始终落在 [lo, hi] 内。
func randomField(rnd *rand.Rand, lo, hi int) string {
	span := hi - lo + 1

	switch rnd.Intn(6) {
	case 0:
		return "*"
	case 1:
		step := 1 + rnd.Intn(span)
		return "*/" + strconv.Itoa(step)
	case 2:
		return strconv.Itoa(lo + rnd.Intn(span))
	case 3:
		start := lo + rnd.Intn(span)
		end := start + rnd.Intn(hi-start+1)
		return strconv.Itoa(start) + "-" + strconv.Itoa(end)
	case 4:
		start := lo + rnd.Intn(span)
		end := start + rnd.Intn(hi-start+1)
		step := 1 + rnd.Intn(end-start+1)
		return strconv.Itoa(start) + "-" + strconv.Itoa(end) + "/" + strconv.Itoa(step)
	default:
		first := lo + rnd.Intn(span)
		second := lo + rnd.Intn(span)
		return strconv.Itoa(first) + "," + strconv.Itoa(second)
	}
}

// TestNextProperties 是 Next 的属性测试，一次覆盖四条不变量：
//  1. Next 严格晚于入参（入参带秒时也不返回同一分钟）；
//  2. Next 落在业务时区、秒与纳秒为 0；
//  3. Matches(Next(after)) 恒真；
//  4. (after, Next(after)) 内没有更早的命中——逐分钟独立扫一遍，用 Matches 当
//     期望值，而不是拿 Next 自己的跳进结果自证。
func TestNextProperties(t *testing.T) {
	loc := model.BusinessLocation()

	property := func(sample cronSample) bool {
		schedule, err := Parse(sample.expr)
		if err != nil {
			t.Logf("随机表达式 %q 解析失败：%v", sample.expr, err)
			return false
		}
		if schedule.String() != sample.expr {
			t.Logf("String() 应返回原写法 %q", sample.expr)
			return false
		}

		next, ok := schedule.Next(sample.after)
		if !ok {
			t.Logf("表达式 %q 在 %v 之后 31 天内应命中", sample.expr, sample.after)
			return false
		}
		if !next.After(sample.after) {
			t.Logf("Next(%v) = %v 必须严格晚于入参", sample.after, next)
			return false
		}
		if next.Location() != loc || next.Second() != 0 || next.Nanosecond() != 0 {
			t.Logf("Next(%v) = %v 应落在业务时区且秒为 0", sample.after, next)
			return false
		}
		if !schedule.Matches(next) {
			t.Logf("Matches(Next(%v) = %v) 应为 true（表达式 %q）", sample.after, next, sample.expr)
			return false
		}

		// 区间扫描的起点与 Next 的起点口径一致：截到分钟再进一分钟。
		from := sample.after.In(loc).Truncate(time.Minute).Add(time.Minute)
		const scanCap = 33 * 24 * 60 // 生成器保证命中在 31 天内，留一天余量
		scanned := 0
		for minute := from; minute.Before(next); minute = minute.Add(time.Minute) {
			if scanned++; scanned > scanCap {
				t.Logf("表达式 %q 的命中区间 %v→%v 超出扫描上限", sample.expr, from, next)
				return false
			}
			if schedule.Matches(minute) {
				t.Logf("区间内有更早的命中 %v（Next 返回 %v，表达式 %q）", minute, next, sample.expr)
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, cronPropertyConfig()); err != nil {
		t.Fatal(err)
	}
}
