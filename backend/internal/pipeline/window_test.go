package pipeline

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// propertySeed 固定属性测试的随机种子：结果可复现，门禁的绿红不取决于当次
// 抽到哪批样本，失败时也能按同一种子重跑。
const propertySeed = 20261103

// propertyConfig 返回带固定种子的 quick 配置，窗口与任务规划的属性测试共用。
func propertyConfig() *quick.Config {
	return &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(propertySeed))}
}

// TestWindowForClampsToTermEnd 覆盖 Requirement 1.1 的窗口口径：
// 当前周 C 起、至多四周，且不越过学期总周数 Y（闭区间）。
func TestWindowForClampsToTermEnd(t *testing.T) {
	cases := []struct {
		name        string
		currentWeek int
		totalWeeks  int
		want        Window
	}{
		{name: "学期初取满四周", currentWeek: 1, totalWeeks: 16, want: Window{Start: 1, End: 4}},
		{name: "学期中段取满四周", currentWeek: 5, totalWeeks: 16, want: Window{Start: 5, End: 8}},
		{name: "剩余不足四周时截到学期末", currentWeek: 14, totalWeeks: 16, want: Window{Start: 14, End: 16}},
		{name: "刚好剩三周", currentWeek: 13, totalWeeks: 16, want: Window{Start: 13, End: 16}},
		{name: "最后一周只取一周", currentWeek: 16, totalWeeks: 16, want: Window{Start: 16, End: 16}},
		{name: "学期只有两周", currentWeek: 1, totalWeeks: 2, want: Window{Start: 1, End: 2}},
		{name: "int 上限也不溢出", currentWeek: math.MaxInt - 1, totalWeeks: math.MaxInt,
			want: Window{Start: math.MaxInt - 1, End: math.MaxInt}},
		{name: "int 上限单周", currentWeek: math.MaxInt, totalWeeks: math.MaxInt,
			want: Window{Start: math.MaxInt, End: math.MaxInt}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := WindowFor(testCase.currentWeek, testCase.totalWeeks)
			if got != testCase.want {
				t.Fatalf("WindowFor(%d, %d) = %+v，期望 %+v",
					testCase.currentWeek, testCase.totalWeeks, got, testCase.want)
			}
			if got.Empty() {
				t.Fatalf("WindowFor(%d, %d) 不应为空窗", testCase.currentWeek, testCase.totalWeeks)
			}
		})
	}
}

// TestWindowForEmptyWhenCalendarNotUsable 覆盖空窗的三种来源：
// 当前周小于 1、总周数小于当前周、学期没有可用周（Y=0）。
func TestWindowForEmptyWhenCalendarNotUsable(t *testing.T) {
	cases := []struct {
		name        string
		currentWeek int
		totalWeeks  int
	}{
		{name: "当前周为 0", currentWeek: 0, totalWeeks: 16},
		{name: "当前周为负", currentWeek: -3, totalWeeks: 16},
		{name: "总周数小于当前周", currentWeek: 5, totalWeeks: 4},
		{name: "学期没有可用周", currentWeek: 1, totalWeeks: 0},
		{name: "两个值都是 int 下限", currentWeek: math.MinInt, totalWeeks: math.MinInt},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := WindowFor(testCase.currentWeek, testCase.totalWeeks)
			if !got.Empty() {
				t.Fatalf("WindowFor(%d, %d) = %+v，期望空窗",
					testCase.currentWeek, testCase.totalWeeks, got)
			}
			if weeks := got.Weeks(); weeks != nil {
				t.Fatalf("空窗的 Weeks() 期望 nil，实际 %v", weeks)
			}
		})
	}
}

// TestWindowWeeksEnumeratesClosedRange 覆盖窗口内周次的枚举：
// 闭区间、升序、空窗返回 nil。
func TestWindowWeeksEnumeratesClosedRange(t *testing.T) {
	cases := []struct {
		name   string
		window Window
		want   []int
	}{
		{name: "四周窗口", window: Window{Start: 5, End: 8}, want: []int{5, 6, 7, 8}},
		{name: "单周窗口", window: Window{Start: 16, End: 16}, want: []int{16}},
		{name: "零值窗口是空窗", window: Window{}, want: nil},
		{name: "起点大于终点是空窗", window: Window{Start: 5, End: 4}, want: nil},
		{name: "起点小于一是空窗", window: Window{Start: 0, End: 3}, want: nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := testCase.window.Weeks()
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("Weeks() = %v，期望 %v", got, testCase.want)
			}
		})
	}
}

// TestWindowWeeksTerminatesAtIntMax 回归「End 为 int 上限时周次自增回绕」：
// 旧实现在 append(math.MaxInt) 之后 week++ 变成 math.MinInt，循环永不结束。
// 这里用末周恰为 MaxInt 的单周与三周窗口钉住终止条件。
func TestWindowWeeksTerminatesAtIntMax(t *testing.T) {
	cases := []struct {
		name   string
		window Window
		want   []int
	}{
		{
			name:   "单周窗口止于 int 上限",
			window: Window{Start: math.MaxInt, End: math.MaxInt},
			want:   []int{math.MaxInt},
		},
		{
			name:   "三周窗口止于 int 上限",
			window: Window{Start: math.MaxInt - 2, End: math.MaxInt},
			want:   []int{math.MaxInt - 2, math.MaxInt - 1, math.MaxInt},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := testCase.window.Weeks()
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("Weeks() = %v，期望 %v", got, testCase.want)
			}
		})
	}
}

// smallWeek 是属性测试的周次生成器：把随机值限制在 -5..25，
// 覆盖空窗、学期初、学期末与四周窗口，又不把样本浪费在 int 极值上
// （极值单独由上面的表驱动用例覆盖）。
type smallWeek int

// Generate 实现 testing/quick.Generator；周次范围与 quick 的 size 无关，故不接收。
func (smallWeek) Generate(rand *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(smallWeek(rand.Intn(31) - 5))
}

// TestWindowForMatchesClosedRangeDefinition 是窗口口径的属性测试：
// 任意 C、Y 下，若 C<1 或 Y<C 必须是空窗；否则窗口恒为 [C, min(Y, C+3)]，
// 且 Weeks() 恰好逐周枚举该闭区间。
func TestWindowForMatchesClosedRangeDefinition(t *testing.T) {
	property := func(current, total smallWeek) bool {
		currentWeek, totalWeeks := int(current), int(total)
		window := WindowFor(currentWeek, totalWeeks)

		if currentWeek < 1 || totalWeeks < currentWeek {
			return window.Empty() && window.Weeks() == nil
		}
		if window.Empty() || window.Start != currentWeek {
			return false
		}

		// 用 int64 计算期望末周，避免属性测试里的 C+3 溢出干扰判定。
		// 这里的 3 是字面量而非生产常量 windowWeeks：若常量被改成 5，
		// 期望必须保持 C+3，属性测试才能把变异检出来。
		wantEnd := int64(currentWeek) + 3
		if wantEnd > int64(totalWeeks) {
			wantEnd = int64(totalWeeks)
		}
		if int64(window.End) != wantEnd {
			return false
		}
		if window.End > totalWeeks || window.End < currentWeek {
			return false
		}

		weeks := window.Weeks()
		if len(weeks) != window.End-window.Start+1 {
			return false
		}
		for i, week := range weeks {
			if week != window.Start+i {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("窗口必须恒为 C..min(Y, C+3): %v", err)
	}
}

// windowStoreStub 记录 RecordWindow 传给仓储的全部参数；err 非 nil 时模拟仓储拒绝。
type windowStoreStub struct {
	calls      int
	runID      int64
	start      int
	end        int
	weeksTotal int
	now        time.Time
	err        error
}

// SetRunWindow 实现 WindowStore。
func (s *windowStoreStub) SetRunWindow(_ context.Context, runID int64, start, end, weeksTotal int, now time.Time) error {
	s.calls++
	s.runID = runID
	s.start = start
	s.end = end
	s.weeksTotal = weeksTotal
	s.now = now
	return s.err
}

// windowTestNow 是 RecordWindow 用例的固定时刻；时钟被换到 UTC 以验证落库前的时区换算。
var windowTestNow = time.Date(2026, 11, 9, 1, 30, 0, 0, time.UTC)

// TestRecordWindowRejectsEmptyWindow 覆盖空窗边界：Window{} 与 Start>End 都不落库。
func TestRecordWindowRejectsEmptyWindow(t *testing.T) {
	cases := []struct {
		name   string
		window Window
	}{
		{name: "零值窗口", window: Window{}},
		{name: "首周小于1", window: Window{Start: 0, End: 3}},
		{name: "末周早于首周", window: Window{Start: 9, End: 8}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			windows := &windowStoreStub{}
			err := RecordWindow(context.Background(), windows, 7, testCase.window, fixedClock(windowTestNow))
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("空窗应拒绝并包装 ErrRunStoreFailed，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "空") {
				t.Fatalf("错误应说明窗口为空，实际 %v", err)
			}
			if windows.calls != 0 {
				t.Fatalf("空窗不应落库，实际调用 %d 次", windows.calls)
			}
		})
	}
}

// TestRecordWindowAcceptsFourWeekSpan 覆盖 4 周边界：跨度上限本身允许（含等号）。
func TestRecordWindowAcceptsFourWeekSpan(t *testing.T) {
	windows := &windowStoreStub{}
	window := Window{Start: 9, End: 12}

	if err := RecordWindow(context.Background(), windows, 7, window, fixedClock(windowTestNow)); err != nil {
		t.Fatalf("恰好 4 周应允许落库，实际 %v", err)
	}
	if windows.calls != 1 {
		t.Fatalf("应落库一次，实际 %d 次", windows.calls)
	}
	if windows.runID != 7 || windows.start != 9 || windows.end != 12 || windows.weeksTotal != 4 {
		t.Fatalf("落库参数不符: %+v", windows)
	}
	if _, offset := windows.now.Zone(); offset != 8*60*60 {
		t.Fatalf("落库时刻应换算到 +08:00，实际 %d 秒", offset)
	}
}

// TestRecordWindowRejectsFiveWeekSpan 覆盖 5 周边界：超出一周即拒绝，且不落库。
func TestRecordWindowRejectsFiveWeekSpan(t *testing.T) {
	cases := []struct {
		name   string
		window Window
	}{
		{name: "恰好5周", window: Window{Start: 1, End: 5}},
		{name: "远宽于5周", window: Window{Start: 1, End: 16}},
		{name: "int上限也不回绕", window: Window{Start: 1, End: math.MaxInt}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			windows := &windowStoreStub{}
			err := RecordWindow(context.Background(), windows, 7, testCase.window, fixedClock(windowTestNow))
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("超宽窗口应拒绝并包装 ErrRunStoreFailed，实际 %v", err)
			}
			if !strings.Contains(err.Error(), "跨度") {
				t.Fatalf("错误应说明跨度超限，实际 %v", err)
			}
			if windows.calls != 0 {
				t.Fatalf("超宽窗口不应落库，实际调用 %d 次", windows.calls)
			}
		})
	}
}

// TestRecordWindowPropagatesClosedRunError 覆盖「已收口」边界：
// 仓储的 running 守卫拒绝时，RecordWindow 原样上抛，不吞成成功。
func TestRecordWindowPropagatesClosedRunError(t *testing.T) {
	closedErr := fmt.Errorf("%w: 轮次 7 已收口（status=success），不能再写入采集窗口", model.ErrRunStoreFailed)
	windows := &windowStoreStub{err: closedErr}

	err := RecordWindow(context.Background(), windows, 7, Window{Start: 9, End: 12}, fixedClock(windowTestNow))
	if !errors.Is(err, closedErr) {
		t.Fatalf("应保留仓储原因，实际 %v", err)
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应仍可判定为轮次读写失败，实际 %v", err)
	}
	if windows.calls != 1 {
		t.Fatalf("守卫由仓储执行，应调用一次，实际 %d 次", windows.calls)
	}
}

// TestRecordWindowWritesNormalWindow 覆盖正常路径：不足 4 周的窗口按实际跨度写 weeks_total。
func TestRecordWindowWritesNormalWindow(t *testing.T) {
	windows := &windowStoreStub{}
	window := WindowFor(14, 16) // 学期末只剩 3 周：14..16

	if err := RecordWindow(context.Background(), windows, 42, window, fixedClock(windowTestNow)); err != nil {
		t.Fatalf("正常窗口应落库，实际 %v", err)
	}
	if windows.calls != 1 || windows.runID != 42 {
		t.Fatalf("应只落库一次且带轮次编号，实际 %+v", windows)
	}
	if windows.start != 14 || windows.end != 16 || windows.weeksTotal != 3 {
		t.Fatalf("窗口与 weeks_total 不符: %+v", windows)
	}
	if !windows.now.Equal(windowTestNow.In(shanghai)) {
		t.Fatalf("落库时刻期望 %s，实际 %s", windowTestNow.In(shanghai), windows.now)
	}
}
