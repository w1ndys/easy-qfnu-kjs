package query

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// screenNow 复用新鲜度的固定时刻（上海时区 2026-10-28 12:00，周三）。
func screenNow() time.Time { return freshnessNow }

// fakeScreenLookup 用预设事实造一个存储层替身，并记录被调用次数。
type fakeScreenLookup struct {
	facts ScreenFacts
	err   error
	calls int
}

// lookup 实现 ScreenLookup。
func (f *fakeScreenLookup) lookup(context.Context) (ScreenFacts, error) {
	f.calls++
	return f.facts, f.err
}

// fakeScreenTraffic 是访问统计读取的替身。
type fakeScreenTraffic struct {
	snapshot stats.Snapshot
	err      error
	calls    int
	gotNow   time.Time
	gotParam stats.ReadParams
}

// Snapshot 实现 ScreenTrafficReader。
func (f *fakeScreenTraffic) Snapshot(_ context.Context, now time.Time, params stats.ReadParams) (stats.Snapshot, error) {
	f.calls++
	f.gotNow = now
	f.gotParam = params
	return f.snapshot, f.err
}

// screenWeekLookup 返回固定学期与周次的学期周，用来断言「当前周」的取值口径。
func screenWeekLookup(term string, week int) WeekLookup {
	return func(context.Context, time.Time) (model.TermWeek, bool, error) {
		return model.TermWeek{
			Term:       term,
			Week:       int16(week),
			Monday:     time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC),
			InCalendar: true,
		}, true, nil
	}
}

// screenTestService 把替身与真实服务组装到一起。
type screenTestService struct {
	svc     *ScreenService
	lookup  *fakeScreenLookup
	fresh   *recordingFreshness
	traffic *fakeScreenTraffic
}

// newScreenTestService 组装大屏服务与三个记录器。
func newScreenTestService(facts ScreenFacts, fresh FreshnessFacts, snapshot stats.Snapshot) *screenTestService {
	harness := &screenTestService{
		lookup:  &fakeScreenLookup{facts: facts},
		fresh:   &recordingFreshness{facts: fresh},
		traffic: &fakeScreenTraffic{snapshot: snapshot},
	}
	harness.svc = NewScreen(harness.lookup.lookup, NewFreshness(harness.fresh.lookup),
		harness.traffic, screenWeekLookup(freshnessFixtureTerm, 5))
	return harness
}

// screenOccupancyFixture 造两组楼 × 星期 × 大节的占用格：
// Q10 合计 20 格占用 5，Q11 合计 10 格占用 8。
func screenOccupancyFixture() []ScreenOccupancyCell {
	return []ScreenOccupancyCell{
		{BuildingID: "Q10", BuildingName: "综合教学楼", Weekday: 1, BlockOrdinal: 1, Cells: 10, Occupied: 4},
		{BuildingID: "Q10", BuildingName: "综合教学楼", Weekday: 1, BlockOrdinal: 2, Cells: 10, Occupied: 1},
		{BuildingID: "Q11", BuildingName: "外语楼", Weekday: 2, BlockOrdinal: 1, Cells: 10, Occupied: 8},
	}
}

// screenRunFixture 造一轮已结束的同步。
func screenRunFixture(status string, startedAt time.Time, attrConflicts int) *ScreenRunFacts {
	finished := startedAt.Add(1830 * time.Second)
	return &ScreenRunFacts{
		Status:            status,
		StartedAt:         startedAt,
		FinishedAt:        &finished,
		AttrConflictCount: attrConflicts,
	}
}

// fullScreenFacts 造一份有当前版本、采集窗口 5..8、异常计数与占用格的事实。
func fullScreenFacts() ScreenFacts {
	windowStart, windowEnd := 5, 8
	published := screenRunFixture("success", time.Date(2026, 10, 23, 6, 40, 0, 0, time.UTC), 2)
	published.WindowStart = &windowStart
	published.WindowEnd = &windowEnd
	published.UnknownCells = 12
	published.CompositeCells = 51
	lastRun := screenRunFixture("partial", time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC), 1)

	return ScreenFacts{
		Release: &ScreenReleaseFacts{
			ReleaseID:   "20261023T071012Z",
			GeneratedAt: time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC),
			Term:        freshnessFixtureTerm,
			TotalWeeks:  20,
			Rooms:       2481,
			Buildings:   26,
		},
		Published: published,
		LastRun:   lastRun,
		Occupancy: screenOccupancyFixture(),
	}
}

// screenFreshnessFacts 造窗口内的事实：第 5 周新鲜、第 7 周已发布但从未成功抓取。
func screenFreshnessFacts() FreshnessFacts {
	return FreshnessFacts{
		Weeks: []WeekFacts{
			{Week: 5, Published: true, LastSuccessAt: freshAt(-time.Hour)},
			{Week: 7, Published: true},
		},
		// partial 不算失败：第 7 周的过期只能由「没有成功记录」推出。
		LastRunStatus: "partial",
	}
}

// screenSnapshotFixture 造一份访问统计快照。
func screenSnapshotFixture() stats.Snapshot {
	heatmap := emptyHeatmap()
	heatmap[0][8] = 118
	return stats.Snapshot{
		Traffic: stats.Traffic{
			PV: stats.Counts{Today: 1284, D7: 8931, D30: 37220},
			UV: stats.Counts{Today: 263, D7: 1512, D30: 5401},
		},
		Leaderboard: stats.Leaderboard{
			Buildings: []stats.BuildingHit{{BuildingID: "Q10", Name: "综合教学楼", Hits: 612}},
			Rooms:     []stats.RoomHit{{RoomID: "0899", Name: "F126", Hits: 148}},
			Keywords:  []stats.KeywordHit{{Text: "综合楼", Hits: 94}},
		},
		Peaks:            stats.Peaks{Heatmap: heatmap, Top: []stats.Peak{{Weekday: 1, Hour: 8, Share: 0.11}}},
		Trend:            []stats.TrendPoint{{Day: "2026-10-28", PV: 1180}},
		NodeDistribution: []stats.NodeHit{{Node: "01", Hits: 4210}},
	}
}

// TestScreenShape 覆盖 Requirement 33.1–33.5 的完整形状：
// 规模、版本、逐周新鲜度、最近轮次、异常计数与两条占用口径。
func TestScreenShape(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())

	got, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}
	if got.Empty {
		t.Fatal("有当前版本时 empty 应为 false")
	}
	if got.Scale.Rooms != 2481 || got.Scale.Buildings != 26 {
		t.Fatalf("规模不符: %+v", got.Scale)
	}
	if got.Scale.Term != freshnessFixtureTerm || got.Scale.Week != 5 {
		t.Fatalf("学期与当前周不符: %+v", got.Scale)
	}
	if !reflect.DeepEqual(got.Scale.Window, []int{5, 8}) {
		t.Fatalf("采集窗口不符: %v", got.Scale.Window)
	}
	if got.Release == nil || got.Release.ReleaseID != "20261023T071012Z" {
		t.Fatalf("版本不符: %+v", got.Release)
	}
	if got.Release.GeneratedAt.Location() != time.UTC {
		t.Fatalf("生成时间必须是 UTC: %s", got.Release.GeneratedAt)
	}

	// 逐周新鲜度复用 9.6：窗口覆盖 5..8，缺口周补成未发布。
	wantParams := FreshnessParams{Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 8}
	if harness.fresh.got != wantParams {
		t.Fatalf("新鲜度入参不符: %+v", harness.fresh.got)
	}
	if len(got.Weeks) != 4 {
		t.Fatalf("窗口内应有 4 周，实际 %d: %+v", len(got.Weeks), got.Weeks)
	}
	if !got.Weeks[0].Published || got.Weeks[0].Stale {
		t.Fatalf("第 5 周应已发布且不 stale: %+v", got.Weeks[0])
	}
	if got.Weeks[1].Published || got.Weeks[1].Stale {
		t.Fatalf("第 6 周未发布且不算 stale: %+v", got.Weeks[1])
	}
	if !got.Weeks[2].Published || !got.Weeks[2].Stale {
		t.Fatalf("第 7 周已发布但无成功抓取，应 stale: %+v", got.Weeks[2])
	}

	if got.LastRun == nil {
		t.Fatal("有轮次时 last_run 不应为 null")
	}
	if got.LastRun.Status != "partial" || !got.LastRun.AttrConflict {
		t.Fatalf("最近轮次状态与冲突标记不符: %+v", got.LastRun)
	}
	if got.LastRun.StartedAt.Location() != time.UTC {
		t.Fatalf("轮次开始时间必须是 UTC: %s", got.LastRun.StartedAt)
	}
	if got.LastRun.DurationMS == nil || *got.LastRun.DurationMS != 1830000 {
		t.Fatalf("耗时应为 1830000ms（30.5 分钟），实际 %v", got.LastRun.DurationMS)
	}

	wantAnomalies := ScreenAnomalies{UnknownCells: 12, CompositeCells: 51, AttrConflicts: 2}
	if got.Anomalies != wantAnomalies {
		t.Fatalf("异常计数不符: %+v", got.Anomalies)
	}

	if len(got.Occupancy.ByBuilding) != 2 {
		t.Fatalf("应按楼号升序给出 2 栋楼: %+v", got.Occupancy.ByBuilding)
	}
	first, second := got.Occupancy.ByBuilding[0], got.Occupancy.ByBuilding[1]
	if first.BuildingID != "Q10" || first.Name != "综合教学楼" || first.Cells != 20 || first.Occupied != 5 {
		t.Fatalf("Q10 占用不符: %+v", first)
	}
	if !closeTo(first.Rate, 0.25) {
		t.Fatalf("Q10 占用率应为 0.25，实际 %v", first.Rate)
	}
	if second.BuildingID != "Q11" || second.Cells != 10 || second.Occupied != 8 || !closeTo(second.Rate, 0.8) {
		t.Fatalf("Q11 占用不符: %+v", second)
	}

	if len(got.Occupancy.ByWeekdayBlock) != 7 {
		t.Fatalf("矩阵应有 7 行，实际 %d", len(got.Occupancy.ByWeekdayBlock))
	}
	for weekday, row := range got.Occupancy.ByWeekdayBlock {
		if len(row) != 5 {
			t.Fatalf("第 %d 行应有 5 列，实际 %d", weekday+1, len(row))
		}
	}
	wantMatrix := map[[2]int]float64{
		{0, 0}: 0.4,
		{0, 1}: 0.1,
		{1, 0}: 0.8,
	}
	for weekday, row := range got.Occupancy.ByWeekdayBlock {
		for block, rate := range row {
			want := wantMatrix[[2]int{weekday, block}]
			if !closeTo(rate, want) {
				t.Fatalf("矩阵[%d][%d] 期望 %v，实际 %v", weekday, block, want, rate)
			}
		}
	}

	// 访问统计块原样来自 9.9 的快照；未传参数时按默认近 7 日窗口。
	if harness.traffic.calls != 1 {
		t.Fatalf("应只读一次访问统计，实际 %d 次", harness.traffic.calls)
	}
	if harness.traffic.gotParam.PeakDays != stats.DefaultPeakDays {
		t.Fatalf("大屏应使用默认近 7 日窗口，实际 %d", harness.traffic.gotParam.PeakDays)
	}
	if !harness.traffic.gotNow.Equal(screenNow()) {
		t.Fatalf("访问统计时钟透传不符: %s", harness.traffic.gotNow)
	}
	if got.Traffic.PV.Today != 1284 || got.Traffic.UV.D30 != 5401 {
		t.Fatalf("PV/UV 不符: %+v", got.Traffic)
	}
	if len(got.Leaderboard.Buildings) != 1 || got.Leaderboard.Keywords[0].Text != "综合楼" {
		t.Fatalf("榜单不符: %+v", got.Leaderboard)
	}
	if len(got.Peaks.Top) != 1 || !closeTo(got.Peaks.Top[0].Share, 0.11) {
		t.Fatalf("峰值时段不符: %+v", got.Peaks)
	}
	if len(got.Trend) != 1 || got.Trend[0].Day != "2026-10-28" {
		t.Fatalf("趋势不符: %+v", got.Trend)
	}
	if len(got.NodeDistribution) != 1 || got.NodeDistribution[0].Hits != 4210 {
		t.Fatalf("节次分布不符: %+v", got.NodeDistribution)
	}
}

// TestScreenPeakDaysPassedToTraffic 覆盖 peak_days 下传：
// 缺省、显式 7、显式 30 与非法值分别归一成 7/7/30/7 后交给访问统计。
func TestScreenPeakDaysPassedToTraffic(t *testing.T) {
	cases := []struct {
		name   string
		params ScreenParams
		want   int
	}{
		{name: "缺省", params: ScreenParams{}, want: stats.PeakDays7},
		{name: "显式 7", params: ScreenParams{PeakDays: stats.PeakDays7}, want: stats.PeakDays7},
		{name: "显式 30", params: ScreenParams{PeakDays: stats.PeakDays30}, want: stats.PeakDays30},
		{name: "非法值按默认", params: ScreenParams{PeakDays: 11}, want: stats.DefaultPeakDays},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每条用例用独立服务，避免前一条的缓存命中掩盖入参。
			harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
			if _, err := harness.svc.Screen(t.Context(), screenNow(), c.params); err != nil {
				t.Fatalf("大屏查询失败: %v", err)
			}
			if harness.traffic.gotParam.PeakDays != c.want {
				t.Fatalf("peak_days 应下传 %d，实际 %d", c.want, harness.traffic.gotParam.PeakDays)
			}
		})
	}
}

// TestScreenJSONKeys 按契约键名断言 data 各层，防止漏写 json tag 时字段名变成驼峰。
func TestScreenJSONKeys(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	result, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(mustJSON(t, result), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	for _, key := range []string{
		"scale", "release", "weeks", "last_run", "anomalies", "occupancy",
		"traffic", "leaderboard", "peaks", "trend", "node_distribution", "empty",
	} {
		if _, exists := got[key]; !exists {
			t.Fatalf("缺少契约键 %q，实际键: %v", key, screenKeysOf(got))
		}
	}

	scale := got["scale"].(map[string]any)
	for _, key := range []string{"rooms", "buildings", "term", "week", "window"} {
		if _, exists := scale[key]; !exists {
			t.Fatalf("scale 缺少契约键 %q，实际键: %v", key, screenKeysOf(scale))
		}
	}
	release := got["release"].(map[string]any)
	for _, key := range []string{"release_id", "generated_at"} {
		if _, exists := release[key]; !exists {
			t.Fatalf("release 缺少契约键 %q，实际键: %v", key, screenKeysOf(release))
		}
	}
	lastRun := got["last_run"].(map[string]any)
	for _, key := range []string{"status", "started_at", "duration_ms", "attr_conflict"} {
		if _, exists := lastRun[key]; !exists {
			t.Fatalf("last_run 缺少契约键 %q，实际键: %v", key, screenKeysOf(lastRun))
		}
	}
	anomalies := got["anomalies"].(map[string]any)
	for _, key := range []string{"unknown_cells", "composite_cells", "attr_conflicts"} {
		if _, exists := anomalies[key]; !exists {
			t.Fatalf("anomalies 缺少契约键 %q，实际键: %v", key, screenKeysOf(anomalies))
		}
	}
	occupancy := got["occupancy"].(map[string]any)
	for _, key := range []string{"by_building", "by_weekday_block"} {
		if _, exists := occupancy[key]; !exists {
			t.Fatalf("occupancy 缺少契约键 %q，实际键: %v", key, screenKeysOf(occupancy))
		}
	}
	building := occupancy["by_building"].([]any)[0].(map[string]any)
	for _, key := range []string{"building_id", "name", "cells", "occupied", "rate"} {
		if _, exists := building[key]; !exists {
			t.Fatalf("by_building 缺少契约键 %q，实际键: %v", key, screenKeysOf(building))
		}
	}
}

// TestScreenNoErrorDetails 覆盖 Requirement 33.6 与 CP13：
// 响应里不出现错误码、错误原文与账号类字段。
func TestScreenNoErrorDetails(t *testing.T) {
	facts := fullScreenFacts()
	// 存储层即使有失败轮次，大屏结构体里也没有承接错误原文的字段。
	facts.LastRun = screenRunFixture("failed", time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC), 0)
	harness := newScreenTestService(facts, screenFreshnessFacts(), screenSnapshotFixture())

	result, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}
	payload := string(mustJSON(t, result))
	for _, banned := range []string{
		"error_code", "error_message", "password", "account", "cookie", "traceback",
	} {
		if strings.Contains(payload, banned) {
			t.Fatalf("大屏响应不应包含 %q: %s", banned, payload)
		}
	}
	if result.LastRun == nil || result.LastRun.Status != "failed" {
		t.Fatalf("失败轮次只展示状态，实际 %+v", result.LastRun)
	}
}

// TestScreenEmptyState 覆盖 Requirement 33.8：无当前版本时不是 500/40401，
// 而是 release:null + empty:true，其余块给空/零值。
func TestScreenEmptyState(t *testing.T) {
	harness := newScreenTestService(ScreenFacts{}, screenFreshnessFacts(), screenSnapshotFixture())

	got, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("空态不应失败: %v", err)
	}
	if !got.Empty {
		t.Fatal("无当前版本时 empty 应为 true")
	}
	if got.Release != nil {
		t.Fatalf("空态 release 应为 null，实际 %+v", got.Release)
	}
	if got.Scale.Rooms != 0 || got.Scale.Buildings != 0 || got.Scale.Term != "" || got.Scale.Week != 0 {
		t.Fatalf("空态规模应为零值: %+v", got.Scale)
	}
	if got.Scale.Window == nil || len(got.Scale.Window) != 0 {
		t.Fatalf("空态采集窗口应为空数组，实际 %v", got.Scale.Window)
	}
	if got.Weeks == nil || len(got.Weeks) != 0 {
		t.Fatalf("空态 weeks 应为空数组，实际 %v", got.Weeks)
	}
	if got.LastRun != nil {
		t.Fatalf("空态 last_run 应为 null，实际 %+v", got.LastRun)
	}
	if got.Anomalies != (ScreenAnomalies{}) {
		t.Fatalf("空态异常计数应为 0: %+v", got.Anomalies)
	}
	if got.Occupancy.ByBuilding == nil || len(got.Occupancy.ByBuilding) != 0 {
		t.Fatalf("空态 by_building 应为空数组，实际 %v", got.Occupancy.ByBuilding)
	}
	if len(got.Occupancy.ByWeekdayBlock) != 7 {
		t.Fatalf("空态矩阵仍应是 7 行，实际 %d", len(got.Occupancy.ByWeekdayBlock))
	}
	for weekday, row := range got.Occupancy.ByWeekdayBlock {
		for block, rate := range row {
			if rate != 0 {
				t.Fatalf("空态矩阵[%d][%d] 应为 0，实际 %v", weekday, block, rate)
			}
		}
	}
	if harness.traffic.calls != 0 {
		t.Fatalf("空态不该再读访问统计，实际 %d 次", harness.traffic.calls)
	}
	if got.Traffic != (stats.Traffic{}) {
		t.Fatalf("空态 PV/UV 应为零值: %+v", got.Traffic)
	}
	if len(got.Leaderboard.Buildings) != 0 || len(got.Leaderboard.Rooms) != 0 || len(got.Leaderboard.Keywords) != 0 {
		t.Fatalf("空态榜单应为空: %+v", got.Leaderboard)
	}
	if got.Trend == nil || len(got.Trend) != 0 || got.NodeDistribution == nil || len(got.NodeDistribution) != 0 {
		t.Fatalf("空态趋势与节次分布应为空数组: %+v / %+v", got.Trend, got.NodeDistribution)
	}

	// 序列化后各块是 null/[] 而不是缺键，前端不用切换分支。
	var raw map[string]any
	if err := json.Unmarshal(mustJSON(t, got), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if raw["release"] != nil {
		t.Fatalf("空态 release 应为 null: %v", raw["release"])
	}
	if raw["last_run"] != nil {
		t.Fatalf("空态 last_run 应为 null: %v", raw["last_run"])
	}
	if weeks, ok := raw["weeks"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("空态 weeks 应为 []，实际 %v", raw["weeks"])
	}
	if window, ok := raw["scale"].(map[string]any)["window"].([]any); !ok || len(window) != 0 {
		t.Fatalf("空态 window 应为 []，实际 %v", raw["scale"])
	}
}

// TestScreenCacheReusesResultWithinTTL 覆盖 Requirement 33.7 与 CP15：
// 30 秒内重复轮询复用同一份聚合结果，不重查存储与统计。
func TestScreenCacheReusesResultWithinTTL(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	first, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	// 第 29 秒仍在 30 秒窗口内，必须命中缓存。
	second, err := harness.svc.Screen(t.Context(), now.Add(29*time.Second), ScreenParams{})
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}

	if harness.lookup.calls != 1 {
		t.Fatalf("30 秒内应只查一次存储，实际 %d 次", harness.lookup.calls)
	}
	if harness.traffic.calls != 1 {
		t.Fatalf("30 秒内应只读一次访问统计，实际 %d 次", harness.traffic.calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("缓存结果与首次结果不一致:\n首次 %+v\n缓存 %+v", first, second)
	}
}

// TestScreenCacheExpiresAtTTL 覆盖 CP15 的边界：恰好 30 秒即视为未命中并重新聚合。
func TestScreenCacheExpiresAtTTL(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	if _, err := harness.svc.Screen(t.Context(), now, ScreenParams{}); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := harness.svc.Screen(t.Context(), now.Add(ScreenCacheTTL), ScreenParams{}); err != nil {
		t.Fatalf("过期后查询失败: %v", err)
	}

	if harness.lookup.calls != 2 {
		t.Fatalf("TTL 到期后应重新查存储，实际 %d 次", harness.lookup.calls)
	}
}

// TestScreenCacheSeparatesBusinessDate 断言缓存键含业务日期：
// 30 秒窗口内跨零点（同一 TTL、不同日期）也要重新聚合。
func TestScreenCacheSeparatesBusinessDate(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	// 上海时区 23:59:50，11 秒后换日，仍在 30 秒 TTL 内。
	beforeMidnight := time.Date(2026, 10, 28, 15, 59, 50, 0, time.UTC)

	if _, err := harness.svc.Screen(t.Context(), beforeMidnight, ScreenParams{}); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := harness.svc.Screen(t.Context(), beforeMidnight.Add(11*time.Second), ScreenParams{}); err != nil {
		t.Fatalf("跨零点查询失败: %v", err)
	}

	if harness.lookup.calls != 2 {
		t.Fatalf("跨业务日期应重新聚合，实际调用 %d 次", harness.lookup.calls)
	}
}

// TestScreenCacheSeparatesPeakDays 断言缓存键含 peak_days：
// 同一业务日期与同一个 30 秒 TTL 内，7 与 30 是两份不同的热力与峰值，不能共用一条缓存。
func TestScreenCacheSeparatesPeakDays(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	if _, err := harness.svc.Screen(t.Context(), now, ScreenParams{PeakDays: stats.PeakDays7}); err != nil {
		t.Fatalf("近 7 日查询失败: %v", err)
	}
	// 1 秒后切到近 30 日：仍在首次写入的 30 秒 TTL 内，但窗口不同必须重新聚合。
	if _, err := harness.svc.Screen(t.Context(), now.Add(time.Second), ScreenParams{PeakDays: stats.PeakDays30}); err != nil {
		t.Fatalf("近 30 日查询失败: %v", err)
	}

	if harness.lookup.calls != 2 {
		t.Fatalf("切换峰值窗口应重新聚合，实际调用 %d 次", harness.lookup.calls)
	}
	if harness.traffic.calls != 2 {
		t.Fatalf("切换峰值窗口应重新读一次访问统计，实际 %d 次", harness.traffic.calls)
	}
	if harness.traffic.gotParam.PeakDays != stats.PeakDays30 {
		t.Fatalf("切换后应按近 30 日取数，实际 %d", harness.traffic.gotParam.PeakDays)
	}
	if screenCacheKey(now, stats.PeakDays7) == screenCacheKey(now, stats.PeakDays30) {
		t.Fatal("缓存键必须包含 peak_days")
	}

	// 退回近 7 日仍在首次写入的 TTL 内，应命中 7 日的那条缓存而不是再聚合一次。
	if _, err := harness.svc.Screen(t.Context(), now.Add(2*time.Second), ScreenParams{PeakDays: stats.PeakDays7}); err != nil {
		t.Fatalf("回到近 7 日查询失败: %v", err)
	}
	if harness.lookup.calls != 2 {
		t.Fatalf("同一峰值窗口应复用缓存，实际调用 %d 次", harness.lookup.calls)
	}
}

// TestScreenCacheKeyNormalizesPeakDays 覆盖归一化后的峰值窗口才进缓存键：
// 0（零值）与 7 是同一个口径，不该各占一条缓存、更不该命中到 30 日那份结果。
func TestScreenCacheKeyNormalizesPeakDays(t *testing.T) {
	now := screenNow()
	if got, want := screenCacheKey(now, screenPeakDays(0)), screenCacheKey(now, screenPeakDays(stats.PeakDays7)); got != want {
		t.Fatalf("零值应与近 7 日共用缓存键，实际 %+v / %+v", got, want)
	}

	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	// 先按零值（默认）取一次，再显式要 7 日：同一业务日期、同一 TTL 内只该聚合一次。
	if _, err := harness.svc.Screen(t.Context(), now, ScreenParams{}); err != nil {
		t.Fatalf("默认窗口查询失败: %v", err)
	}
	if _, err := harness.svc.Screen(t.Context(), now.Add(time.Second), ScreenParams{PeakDays: stats.PeakDays7}); err != nil {
		t.Fatalf("近 7 日查询失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("零值与 7 日应命中同一条缓存，实际聚合 %d 次", harness.lookup.calls)
	}
	if harness.traffic.calls != 1 || harness.traffic.gotParam.PeakDays != stats.PeakDays7 {
		t.Fatalf("访问统计应按近 7 日只读一次，实际 %d 次、窗口 %d", harness.traffic.calls, harness.traffic.gotParam.PeakDays)
	}
}

// TestScreenEmptyStateIsCached 覆盖空态也进缓存：没有当前版本时同样 30 秒复用，
// 空态不是「失败」，不该让大屏轮询每次都打到存储。
func TestScreenEmptyStateIsCached(t *testing.T) {
	harness := newScreenTestService(ScreenFacts{}, screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	first, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("空态查询失败: %v", err)
	}
	if !first.Empty {
		t.Fatal("无当前版本时应是空态")
	}
	second, err := harness.svc.Screen(t.Context(), now.Add(29*time.Second), ScreenParams{})
	if err != nil {
		t.Fatalf("空态缓存命中不应失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("空态也应缓存 30 秒，实际聚合 %d 次", harness.lookup.calls)
	}
	if !second.Empty || !reflect.DeepEqual(first, second) {
		t.Fatalf("空态缓存结果不一致:\n首次 %+v\n缓存 %+v", first, second)
	}
}

// TestScreenCacheHitIsIsolatedFromCallerMutation 覆盖缓存命中的深拷贝隔离：
// 调用方（这里是 handler 之后的任何一层）改写返回值里的切片、指针，
// 都不能影响下一次命中；DurationMS 这个 *int64 也在隔离范围内。
func TestScreenCacheHitIsIsolatedFromCallerMutation(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	first, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	// 篡改首次返回值的每一块可变数据：指针、切片、二维矩阵都覆盖到。
	*first.LastRun.DurationMS = 9999
	first.Weeks[0].Week = 99
	*first.Weeks[0].LastSuccessAt = time.Time{}
	first.Scale.Window[0] = 99
	first.Occupancy.ByWeekdayBlock[0][0] = 9.9
	first.Occupancy.ByBuilding[0].Cells = 9999
	first.Peaks.Heatmap[0][8] = 9999
	first.Peaks.Top[0].Share = 0.99
	first.Leaderboard.Buildings[0].Hits = 9999
	first.Leaderboard.Rooms[0].Hits = 9999
	first.Leaderboard.Keywords[0].Hits = 9999
	first.Trend[0].PV = 9999
	first.NodeDistribution[0].Hits = 9999

	second, err := harness.svc.Screen(t.Context(), now.Add(time.Second), ScreenParams{})
	if err != nil {
		t.Fatalf("缓存命中查询失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("一秒后应命中缓存，实际聚合 %d 次", harness.lookup.calls)
	}
	if got := *second.LastRun.DurationMS; got != 1830000 {
		t.Fatalf("缓存里的耗时被返回值改写污染，实际 %d", got)
	}
	if second.Weeks[0].Week != 5 || second.Weeks[0].LastSuccessAt == nil || !second.Weeks[0].LastSuccessAt.Equal(*freshAt(-time.Hour)) {
		t.Fatalf("缓存里的逐周新鲜度被改写污染: %+v", second.Weeks[0])
	}
	if !reflect.DeepEqual(second.Scale.Window, []int{5, 8}) {
		t.Fatalf("缓存里的采集窗口被改写污染: %v", second.Scale.Window)
	}
	if second.Occupancy.ByWeekdayBlock[0][0] != 0.4 || second.Occupancy.ByBuilding[0].Cells != 20 {
		t.Fatalf("缓存里的占用分布被改写污染: %+v / %v", second.Occupancy.ByBuilding, second.Occupancy.ByWeekdayBlock[0][0])
	}
	if second.Peaks.Heatmap[0][8] != 118 || second.Peaks.Top[0].Share != 0.11 {
		t.Fatalf("缓存里的热力与峰值被改写污染: %d / %v", second.Peaks.Heatmap[0][8], second.Peaks.Top[0].Share)
	}
	if second.Leaderboard.Buildings[0].Hits != 612 || second.Leaderboard.Rooms[0].Hits != 148 ||
		second.Leaderboard.Keywords[0].Hits != 94 {
		t.Fatalf("缓存里的榜单被改写污染: %+v", second.Leaderboard)
	}
	if second.Trend[0].PV != 1180 || second.NodeDistribution[0].Hits != 4210 {
		t.Fatalf("缓存里的趋势或节次分布被改写污染: %+v / %+v", second.Trend, second.NodeDistribution)
	}

	// 命中返回的也是副本：再篡改一次，第三次命中仍要拿到原件。
	*second.LastRun.DurationMS = 8888
	second.Peaks.Heatmap[0][8] = 8888
	second.Weeks = append(second.Weeks, WeekFreshness{Week: 100})
	third, err := harness.svc.Screen(t.Context(), now.Add(2*time.Second), ScreenParams{})
	if err != nil {
		t.Fatalf("第二次缓存命中失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("两次篡改都不该让缓存失效，实际聚合 %d 次", harness.lookup.calls)
	}
	if *third.LastRun.DurationMS != 1830000 || third.Peaks.Heatmap[0][8] != 118 || len(third.Weeks) != 4 {
		t.Fatalf("缓存被命中返回值的改写污染: 耗时 %d、热力 %d、周数 %d",
			*third.LastRun.DurationMS, third.Peaks.Heatmap[0][8], len(third.Weeks))
	}
}

// TestScreenCacheSkipsFailures 覆盖「失败不入缓存」：一次抖动之后仍能返回成功结果。
func TestScreenCacheSkipsFailures(t *testing.T) {
	harness := newScreenTestService(fullScreenFacts(), screenFreshnessFacts(), screenSnapshotFixture())
	harness.lookup.err = errors.New("db down")
	now := screenNow()

	if _, err := harness.svc.Screen(t.Context(), now, ScreenParams{}); err == nil {
		t.Fatal("存储失败时应返回错误")
	}

	harness.lookup.err = nil
	got, err := harness.svc.Screen(t.Context(), now.Add(time.Second), ScreenParams{})
	if err != nil {
		t.Fatalf("恢复后应成功: %v", err)
	}
	if got.Empty || got.Release == nil {
		t.Fatalf("恢复后结果不符: %+v", got)
	}
	if harness.lookup.calls != 2 {
		t.Fatalf("失败不应被缓存，实际调用 %d 次", harness.lookup.calls)
	}
}

// TestScreenOccupancyZeroDenominator 覆盖 CP14 的分母边界：没有观测的楼与格给 0 而不是 NaN。
func TestScreenOccupancyZeroDenominator(t *testing.T) {
	occupancy := screenOccupancy([]ScreenOccupancyCell{
		{BuildingID: "Q12", BuildingName: "空楼", Weekday: 3, BlockOrdinal: 4, Cells: 0, Occupied: 0},
	})

	if len(occupancy.ByBuilding) != 1 {
		t.Fatalf("应给出一栋楼: %+v", occupancy.ByBuilding)
	}
	if occupancy.ByBuilding[0].Rate != 0 {
		t.Fatalf("分母为 0 时占用率应为 0，实际 %v", occupancy.ByBuilding[0].Rate)
	}
	for weekday, row := range occupancy.ByWeekdayBlock {
		for block, rate := range row {
			if math.IsNaN(rate) {
				t.Fatalf("矩阵[%d][%d] 不该是 NaN", weekday, block)
			}
			if rate != 0 {
				t.Fatalf("没有观测的格应为 0，实际 %v", rate)
			}
		}
	}
}

// TestScreenOccupancySkipsUnmappedCells 断言轴里查不到大节的格仍进楼口径总数，
// 但不进 7 × 5 矩阵；没有楼号的格只进矩阵，不进按楼口径。
func TestScreenOccupancySkipsUnmappedCells(t *testing.T) {
	occupancy := screenOccupancy([]ScreenOccupancyCell{
		{BuildingID: "Q10", BuildingName: "综合教学楼", Weekday: 1, BlockOrdinal: 0, Cells: 4, Occupied: 1},
		{BuildingID: "", BuildingName: "", Weekday: 2, BlockOrdinal: 1, Cells: 5, Occupied: 1},
	})

	if len(occupancy.ByBuilding) != 1 || occupancy.ByBuilding[0].Cells != 4 || occupancy.ByBuilding[0].Occupied != 1 {
		t.Fatalf("无大节的格仍应计入楼口径: %+v", occupancy.ByBuilding)
	}
	if occupancy.ByWeekdayBlock[0][0] != 0 {
		t.Fatalf("无大节的格不该进矩阵: %v", occupancy.ByWeekdayBlock[0][0])
	}
	if occupancy.ByWeekdayBlock[1][0] != 0.2 {
		t.Fatalf("无楼号的格仍应进矩阵: %v", occupancy.ByWeekdayBlock[1][0])
	}
}

// TestScreenDuration 覆盖最近轮次耗时的三种口径：已结束、仍在跑、没有结束时刻。
func TestScreenDuration(t *testing.T) {
	started := time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC)
	finished := started.Add(1830 * time.Second)

	finishedRun := &ScreenRunFacts{Status: "partial", StartedAt: started, FinishedAt: &finished}
	if got := screenDuration(finishedRun, screenNow()); got == nil || *got != 1830000 {
		t.Fatalf("已结束轮次应给结束 − 开始，实际 %v", got)
	}

	// 还在跑：没有结束时刻，耗时取已经过去的时长。
	runningRun := &ScreenRunFacts{Status: runStatusRunning, StartedAt: started}
	wantElapsed := screenNow().Sub(started).Milliseconds()
	if got := screenDuration(runningRun, screenNow()); got == nil || *got != wantElapsed {
		t.Fatalf("运行中轮次应给已过去时长 %d，实际 %v", wantElapsed, got)
	}

	// 没有结束时刻又不是 running（比如启动时被收敛的过期轮次）：没有可信耗时，给 null。
	staleRun := &ScreenRunFacts{Status: "failed", StartedAt: started}
	if got := screenDuration(staleRun, screenNow()); got != nil {
		t.Fatalf("没有结束时刻的失败轮次耗时应为 null，实际 %v", *got)
	}

	// 时钟回拨等异常不该输出负耗时。
	skewedRun := &ScreenRunFacts{Status: "success", StartedAt: started, FinishedAt: &started}
	early := started.Add(-time.Hour)
	if got := screenDuration(skewedRun, early); got == nil || *got != 0 {
		t.Fatalf("负耗时应夹到 0，实际 %v", got)
	}
}

// TestScreenWeekFallbacks 覆盖当前周与采集窗口的两处兜底：
// 今天落在别的学期时当前周给 0；采集窗口缺失时退回整学期。
func TestScreenWeekFallbacks(t *testing.T) {
	facts := fullScreenFacts()
	facts.Published = nil // 没有产出轮次：窗口未知

	harness := newScreenTestService(facts, screenFreshnessFacts(), screenSnapshotFixture())
	harness.svc.weekLookup = screenWeekLookup("2025-2026-2", 9)

	got, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}
	if got.Scale.Week != 0 {
		t.Fatalf("跨学期时当前周应为 0，实际 %d", got.Scale.Week)
	}
	if len(got.Scale.Window) != 0 {
		t.Fatalf("没有产出轮次时窗口应为空，实际 %v", got.Scale.Window)
	}
	wantParams := FreshnessParams{Term: freshnessFixtureTerm, FromWeek: 1, ToWeek: 20}
	if harness.fresh.got != wantParams {
		t.Fatalf("窗口缺失时应退回整学期，实际 %+v", harness.fresh.got)
	}
	if got.Anomalies != (ScreenAnomalies{}) {
		t.Fatalf("没有产出轮次时异常计数应为 0: %+v", got.Anomalies)
	}
}

// TestScreenLastRunNil 覆盖从未跑过任何轮次：last_run 为 null 而不是伪造状态。
func TestScreenLastRunNil(t *testing.T) {
	facts := fullScreenFacts()
	facts.LastRun = nil

	harness := newScreenTestService(facts, screenFreshnessFacts(), screenSnapshotFixture())
	got, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}
	if got.LastRun != nil {
		t.Fatalf("没有轮次时 last_run 应为 null，实际 %+v", got.LastRun)
	}
}

// TestScreenNotWired 覆盖装配缺项：返回错误而不是零值载荷。
func TestScreenNotWired(t *testing.T) {
	traffic := &fakeScreenTraffic{snapshot: screenSnapshotFixture()}
	week := screenWeekLookup(freshnessFixtureTerm, 5)

	cases := []struct {
		name string
		svc  *ScreenService
	}{
		{name: "未装配事实读取", svc: NewScreen(nil, nil, traffic, week)},
		{name: "未装配新鲜度", svc: NewScreen((&fakeScreenLookup{facts: fullScreenFacts()}).lookup, nil, traffic, week)},
		{name: "未装配访问统计", svc: NewScreen((&fakeScreenLookup{facts: fullScreenFacts()}).lookup,
			NewFreshness((&recordingFreshness{facts: screenFreshnessFacts()}).lookup), nil, week)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.svc.Screen(t.Context(), screenNow(), ScreenParams{}); err == nil {
				t.Fatal("装配缺项时应返回错误")
			}
		})
	}
}

// TestScreenWeeksEmptyWindow 覆盖学期总周数也为 0 的极端事实：新鲜度给空窗口，不报错。
func TestScreenWeeksEmptyWindow(t *testing.T) {
	facts := fullScreenFacts()
	facts.Published = nil
	facts.Release.TotalWeeks = 0

	harness := newScreenTestService(facts, screenFreshnessFacts(), screenSnapshotFixture())
	got, err := harness.svc.Screen(t.Context(), screenNow(), ScreenParams{})
	if err != nil {
		t.Fatalf("大屏查询失败: %v", err)
	}
	if got.Weeks == nil || len(got.Weeks) != 0 {
		t.Fatalf("没有可表达的周时应给空数组，实际 %v", got.Weeks)
	}
	// 空窗口由 9.6 的 Weeks 直接短路，不该再查一次存储。
	if harness.fresh.calls != 0 {
		t.Fatalf("空窗口不该查存储，实际 %d 次", harness.fresh.calls)
	}
}

// screenKeysOf 把对象键按字典序排好，失败信息里可以直接比对键集合。
func screenKeysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// mustJSON 序列化结果，失败直接终止用例。
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return payload
}

// closeTo 判断两个浮点数在 1e-9 内相等；占用率是除法结果，不做四舍五入。
func closeTo(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9
}

// TestScreenCacheClearedByInvalidate 覆盖 ScreenService.Invalidate 的可见性：
// 同一业务日期与峰值窗口先预热（第二次命中缓存），换一份新当前版本的事实后仍读到旧缓存；
// Invalidate 之后同一时刻再查，必须重新读取存储并返回新版本的 release_id（design D8、CP10）。
func TestScreenCacheClearedByInvalidate(t *testing.T) {
	before := fullScreenFacts()
	harness := newScreenTestService(before, screenFreshnessFacts(), screenSnapshotFixture())
	now := screenNow()

	first, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("首次大屏查询失败: %v", err)
	}
	if first.Release == nil || first.Release.ReleaseID != before.Release.ReleaseID {
		t.Fatalf("首次应返回预热版本 %q，实际 %+v", before.Release.ReleaseID, first.Release)
	}
	// 同键再查一次，确认预热真的进了缓存。
	if _, err := harness.svc.Screen(t.Context(), now, ScreenParams{}); err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("预热后同键应只查一次存储，实际 %d 次", harness.lookup.calls)
	}

	// 模拟回退：同一天、同一窗口，只有当前版本标识变了。
	after := fullScreenFacts()
	after.Release.ReleaseID = "20261114T030512Z"
	harness.lookup.facts = after

	// TTL 未到期：没有 Invalidate 时仍然读旧缓存，替身不再被调用。
	cached, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if harness.lookup.calls != 1 {
		t.Fatalf("TTL 内应命中缓存，实际查存储 %d 次", harness.lookup.calls)
	}
	if cached.Release == nil || cached.Release.ReleaseID != before.Release.ReleaseID {
		t.Fatalf("TTL 内应返回旧缓存版本 %q，实际 %+v", before.Release.ReleaseID, cached.Release)
	}

	harness.svc.Invalidate()

	refreshed, err := harness.svc.Screen(t.Context(), now, ScreenParams{})
	if err != nil {
		t.Fatalf("失效后查询失败: %v", err)
	}
	if harness.lookup.calls != 2 {
		t.Fatalf("Invalidate 后同键应重新查存储，期望 2 次，实际 %d 次", harness.lookup.calls)
	}
	if refreshed.Release == nil || refreshed.Release.ReleaseID != after.Release.ReleaseID {
		t.Fatalf("Invalidate 后应返回新版本 %q，实际 %+v", after.Release.ReleaseID, refreshed.Release)
	}
}
