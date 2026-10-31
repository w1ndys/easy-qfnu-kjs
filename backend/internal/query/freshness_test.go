package query

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// freshnessNow 是新鲜度测试的固定时刻，不读真实时钟（Requirement 27 的时间口径不受本机时钟影响）。
var freshnessNow = time.Date(2026, 10, 28, 4, 0, 0, 0, time.UTC)

// freshnessFixtureTerm 是新鲜度测试用的学期。
const freshnessFixtureTerm = "2026-2027-1"

// fakeFreshness 用预设事实造一个存储层替身，并记录被调用次数。
func fakeFreshness(facts FreshnessFacts, calls *int) FreshnessLookup {
	return func(context.Context, FreshnessParams) (FreshnessFacts, error) {
		if calls != nil {
			*calls++
		}
		return facts, nil
	}
}

// oneWeekFacts 造一条只有第 5 周的事实。
func oneWeekFacts(published bool, lastSuccess *time.Time) FreshnessFacts {
	return FreshnessFacts{Weeks: []WeekFacts{{Week: 5, Published: published, LastSuccessAt: lastSuccess}}}
}

// freshAt 返回相对固定时刻偏移 d 的抓取时间；d 为负表示过去。
func freshAt(d time.Duration) *time.Time {
	at := freshnessNow.Add(d)
	return &at
}

// mustWeeks 跑一次新鲜度查询，失败直接终止用例。
func mustWeeks(t *testing.T, svc *FreshnessService, params FreshnessParams) []WeekFreshness {
	t.Helper()
	weeks, err := svc.Weeks(t.Context(), freshnessNow, params)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	return weeks
}

// TestWeeksStaleThreshold 覆盖 8 天阈值的四个边界：7 天、恰好 8 天、8 天零 1 秒、9 天。
// 边界按「超过 8 天」处理：恰好 8 天不算过期，多 1 秒才算。
func TestWeeksStaleThreshold(t *testing.T) {
	cases := []struct {
		name  string
		after time.Duration // 距最近一次成功抓取的时长
		stale bool
	}{
		{name: "7 天不算过期", after: 7 * 24 * time.Hour, stale: false},
		{name: "恰好 8 天不算过期", after: StaleAfter, stale: false},
		{name: "8 天零 1 秒过期", after: StaleAfter + time.Second, stale: true},
		{name: "9 天过期", after: 9 * 24 * time.Hour, stale: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := oneWeekFacts(true, freshAt(-c.after))
			facts.LastRunStatus = "success"
			weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
				Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
			})
			if len(weeks) != 1 {
				t.Fatalf("期望 1 周，实际 %d", len(weeks))
			}
			if weeks[0].Stale != c.stale {
				t.Fatalf("stale 期望 %v，实际 %v", c.stale, weeks[0].Stale)
			}
			if !weeks[0].Published || weeks[0].Week != 5 {
				t.Fatalf("发布状态与周次不符: %+v", weeks[0])
			}
		})
	}
}

// TestWeeksPartialIsNotFailure 覆盖 Requirement 17.4：最近一轮为「部分完成」不算失败。
// 但 partial 不豁免 8 天阈值：抓取本身超过 8 天仍然过期。
func TestWeeksPartialIsNotFailure(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		lastSuccess *time.Time
		stale       bool
	}{
		{name: "partial 且抓取新鲜不算过期", status: "partial", lastSuccess: freshAt(-time.Hour), stale: false},
		{name: "partial 但抓取超过 8 天仍过期", status: "partial", lastSuccess: freshAt(-9 * 24 * time.Hour), stale: true},
		{name: "success 同样不算失败", status: "success", lastSuccess: freshAt(-time.Hour), stale: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := oneWeekFacts(true, c.lastSuccess)
			facts.LastRunStatus = c.status
			weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
				Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
			})
			if weeks[0].Stale != c.stale {
				t.Fatalf("stale 期望 %v，实际 %v", c.stale, weeks[0].Stale)
			}
		})
	}
}

// TestWeeksFailedRunIsStale 覆盖 Requirement 17.3 的第二个触发条件：
// 最近一轮同步失败时，即使该周刚刚抓取成功也判为过期。
func TestWeeksFailedRunIsStale(t *testing.T) {
	facts := oneWeekFacts(true, freshAt(-time.Minute))
	facts.LastRunStatus = "failed"

	weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
	})
	if !weeks[0].Stale {
		t.Fatalf("最近一轮失败应判过期，实际 %+v", weeks[0])
	}
}

// TestWeeksUnpublishedIsNotStale 覆盖 published=false 不等于过期：
// 没有观测的周只由 published 表达，最近一轮失败也不把它标成过期。
func TestWeeksUnpublishedIsNotStale(t *testing.T) {
	cases := []struct {
		name        string
		status      string
		lastSuccess *time.Time
	}{
		{name: "从未发布且无抓取记录", status: "success", lastSuccess: nil},
		{name: "从未发布但抓取已超 8 天", status: "success", lastSuccess: freshAt(-9 * 24 * time.Hour)},
		{name: "从未发布且最近一轮失败", status: "failed", lastSuccess: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			facts := oneWeekFacts(false, c.lastSuccess)
			facts.LastRunStatus = c.status
			weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
				Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
			})
			if weeks[0].Stale {
				t.Fatalf("未发布的周不应标过期，实际 %+v", weeks[0])
			}
			if weeks[0].Published {
				t.Fatalf("published 期望 false，实际 %+v", weeks[0])
			}
		})
	}
}

// TestWeeksPublishedWithoutFetchIsStale 覆盖已发布却查不到成功抓取记录：没有证据证明新鲜，按过期处理。
func TestWeeksPublishedWithoutFetchIsStale(t *testing.T) {
	facts := oneWeekFacts(true, nil)
	facts.LastRunStatus = "success"

	weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
	})
	if !weeks[0].Stale {
		t.Fatalf("无成功抓取记录应判过期，实际 %+v", weeks[0])
	}
	if weeks[0].LastSuccessAt != nil {
		t.Fatalf("从未成功时 last_success_at 期望 nil，实际 %v", weeks[0].LastSuccessAt)
	}
}

// TestWeeksFillsMissingWeeks 覆盖存储层只回部分周的情形：区间内每个周都要出现在结果里，且按周次升序。
func TestWeeksFillsMissingWeeks(t *testing.T) {
	facts := FreshnessFacts{
		Weeks: []WeekFacts{
			{Week: 7, Published: true, LastSuccessAt: freshAt(-time.Hour)},
			{Week: 5, Published: true, LastSuccessAt: freshAt(-2 * time.Hour)},
		},
		LastRunStatus: "success",
	}

	weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 7,
	})
	if len(weeks) != 3 {
		t.Fatalf("期望补齐成 3 周，实际 %d: %+v", len(weeks), weeks)
	}
	for i, want := range []int{5, 6, 7} {
		if weeks[i].Week != want {
			t.Fatalf("第 %d 个周次期望 %d，实际 %d", i, want, weeks[i].Week)
		}
	}
	// 第 6 周没有任何事实：未发布、从未成功、也不过期。
	if weeks[1].Published || weeks[1].LastSuccessAt != nil || weeks[1].Stale {
		t.Fatalf("缺失周应按零值补齐，实际 %+v", weeks[1])
	}
	// 第 5 周与第 7 周的时间各自独立，不串用。
	if !weeks[0].LastSuccessAt.Equal(freshnessNow.Add(-2 * time.Hour)) {
		t.Fatalf("第 5 周时间不符: %v", weeks[0].LastSuccessAt)
	}
	if !weeks[2].LastSuccessAt.Equal(freshnessNow.Add(-time.Hour)) {
		t.Fatalf("第 7 周时间不符: %v", weeks[2].LastSuccessAt)
	}
}

// TestWeeksRangeValidation 覆盖空窗口与非法区间：返回空数组且不查存储层。
func TestWeeksRangeValidation(t *testing.T) {
	cases := []struct {
		name   string
		params FreshnessParams
	}{
		{name: "起始周为 0", params: FreshnessParams{Term: freshnessFixtureTerm, FromWeek: 0, ToWeek: 5}},
		{name: "结束周早于起始周", params: FreshnessParams{Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 4}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			svc := NewFreshness(fakeFreshness(FreshnessFacts{}, &calls))
			weeks, err := svc.Weeks(t.Context(), freshnessNow, c.params)
			if err != nil {
				t.Fatalf("非法区间不应报错，实际 %v", err)
			}
			if len(weeks) != 0 {
				t.Fatalf("非法区间应返回空数组，实际 %+v", weeks)
			}
			if calls != 0 {
				t.Fatalf("非法区间不应查存储层，实际调用 %d 次", calls)
			}
		})
	}
}

// TestWeeksIgnoresOutOfRangeFacts 覆盖区间外的事实：存储层多回了周次也不能出现在结果里。
func TestWeeksIgnoresOutOfRangeFacts(t *testing.T) {
	facts := FreshnessFacts{
		Weeks:         []WeekFacts{{Week: 9, Published: true, LastSuccessAt: freshAt(-time.Hour)}},
		LastRunStatus: "success",
	}

	weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 8,
	})
	if len(weeks) != 4 {
		t.Fatalf("期望 4 周，实际 %d", len(weeks))
	}
	for _, week := range weeks {
		if week.Week == 9 {
			t.Fatalf("区间外的第 9 周不应出现: %+v", weeks)
		}
	}
}

// TestWeeksLookupError 覆盖存储层失败：错误原样上抛。
func TestWeeksLookupError(t *testing.T) {
	wantErr := errors.New("db down")
	svc := NewFreshness(func(context.Context, FreshnessParams) (FreshnessFacts, error) {
		return FreshnessFacts{}, wantErr
	})

	if _, err := svc.Weeks(t.Context(), freshnessNow, FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
	}); !errors.Is(err, wantErr) {
		t.Fatalf("期望透传存储层错误，实际 %v", err)
	}
}

// TestWeeksNotWired 覆盖未装配存储层：明确报错而不是返回一片「未发布」。
func TestWeeksNotWired(t *testing.T) {
	if _, err := NewFreshness(nil).Weeks(t.Context(), freshnessNow, FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5,
	}); err == nil {
		t.Fatal("未装配时应报错，实际 nil")
	}
}

// TestWeekFreshnessJSON 锁定对外形状：字段名、UTC 化与从未成功时的 null。
func TestWeekFreshnessJSON(t *testing.T) {
	// 存储层可能按会话时区回时刻（这里给上海时间），输出必须是 UTC。
	shanghaiAt := time.Date(2026, 10, 27, 10, 0, 0, 0, shanghai)
	facts := FreshnessFacts{
		Weeks: []WeekFacts{
			{Week: 5, Published: true, LastSuccessAt: &shanghaiAt},
			{Week: 6, Published: false},
		},
		LastRunStatus: "success",
	}

	weeks := mustWeeks(t, NewFreshness(fakeFreshness(facts, nil)), FreshnessParams{
		Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 6,
	})
	encoded, err := json.Marshal(weeks)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	want := `[{"week":5,"published":true,"last_success_at":"2026-10-27T02:00:00Z","stale":false},` +
		`{"week":6,"published":false,"last_success_at":null,"stale":false}]`
	if string(encoded) != want {
		t.Fatalf("JSON 形状不符:\n期望 %s\n实际 %s", want, encoded)
	}
}
