package stats

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// newFactsReader 用固定事实装配读取侧，并记录读取时的窗口。
func newFactsReader(t *testing.T, facts store.StatFacts, windows *store.StatWindows) *Reader {
	t.Helper()
	return NewReader(func(_ context.Context, got store.StatWindows, _ int) (store.StatFacts, error) {
		if windows != nil {
			*windows = got
		}
		return facts, nil
	})
}

// assertRange 断言窗口的两个边界日期。
func assertRange(t *testing.T, name string, got store.StatRange, from, to string) {
	t.Helper()
	if actual := got.From.Format(time.DateOnly); actual != from {
		t.Fatalf("%s 起点期望 %s，实际 %s", name, from, actual)
	}
	if actual := got.To.Format(time.DateOnly); actual != to {
		t.Fatalf("%s 终点期望 %s，实际 %s", name, to, actual)
	}
}

// TestSnapshotUsesShanghaiBusinessDate 覆盖窗口按 Asia/Shanghai 业务日期推算（Requirement 27.2）。
func TestSnapshotUsesShanghaiBusinessDate(t *testing.T) {
	// 2026-11-02 00:30 UTC 是上海 08:30：业务日期必须是 11 月 2 日而不是 11 月 1 日。
	now := time.Date(2026, 11, 2, 0, 30, 0, 0, time.UTC)
	var windows store.StatWindows
	reader := newFactsReader(t, store.StatFacts{}, &windows)

	if _, err := reader.Snapshot(context.Background(), now, ReadParams{}); err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}

	assertRange(t, "今日", windows.Today, "2026-11-02", "2026-11-02")
	assertRange(t, "近 7 日", windows.D7, "2026-10-27", "2026-11-02")
	assertRange(t, "近 30 日", windows.D30, "2026-10-04", "2026-11-02")
	// 默认峰值窗口是近 7 日。
	assertRange(t, "热力", windows.Peak, "2026-10-27", "2026-11-02")
}

// TestSnapshotPeakWindowSwitching 覆盖热力与三张 Top 10 共用的窗口在近 7 日与近 30 日之间切换
// （PRD「高峰时段分析」），同时确认 PV/UV 的近 7 日与近 30 日窗口不跟着动。
func TestSnapshotPeakWindowSwitching(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)

	var windows store.StatWindows
	reader := newFactsReader(t, store.StatFacts{}, &windows)
	if _, err := reader.Snapshot(context.Background(), now, ReadParams{PeakDays: PeakDays30}); err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}
	assertRange(t, "近 30 日热力", windows.Peak, "2026-10-04", "2026-11-02")
	// 热力窗口只改变自身：PV/UV 与节次分布的近 7 日、近 30 日窗口保持原口径。
	assertRange(t, "近 7 日", windows.D7, "2026-10-27", "2026-11-02")
	assertRange(t, "近 30 日", windows.D30, "2026-10-04", "2026-11-02")

	// 非法取值按默认 7 天处理，不报错也不返回空窗口；换一个日期确认窗口跟着走。
	later := time.Date(2026, 12, 1, 9, 0, 0, 0, shanghai)
	if _, err := reader.Snapshot(context.Background(), later, ReadParams{PeakDays: 11}); err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}
	assertRange(t, "默认热力", windows.Peak, "2026-11-25", "2026-12-01")
}

// TestSnapshotTrafficAndLeaderboard 覆盖 PV/UV 三段口径与三张榜单的整形。
func TestSnapshotTrafficAndLeaderboard(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)
	facts := store.StatFacts{
		Today: store.StatPVUV{PV: 12, UV: 5},
		D7:    store.StatPVUV{PV: 90, UV: 33},
		D30:   store.StatPVUV{PV: 400, UV: 120},
		Buildings: []store.StatBuildingHit{
			{ID: "Q10", Name: "综合教学楼", Hits: 61},
			{ID: "Q11", Name: "实验楼", Hits: 20},
		},
		Rooms: []store.StatRoomHit{{ID: "0899", Name: "F126", Hits: 14}},
		Keywords: []store.StatKeywordHit{
			{Text: "综合楼", Hits: 94},
			{Text: "f126", Hits: 40},
		},
		Nodes: []store.StatNodeHit{{Node: "01", Hits: 7}, {Node: "02", Hits: 5}},
	}
	reader := newFactsReader(t, facts, nil)

	snapshot, err := reader.Snapshot(context.Background(), now, ReadParams{})
	if err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}

	if snapshot.Traffic.PV != (Counts{Today: 12, D7: 90, D30: 400}) {
		t.Fatalf("PV 口径不符，实际 %+v", snapshot.Traffic.PV)
	}
	if snapshot.Traffic.UV != (Counts{Today: 5, D7: 33, D30: 120}) {
		t.Fatalf("UV 口径不符，实际 %+v", snapshot.Traffic.UV)
	}
	if len(snapshot.Leaderboard.Buildings) != 2 || snapshot.Leaderboard.Buildings[0] != (BuildingHit{BuildingID: "Q10", Name: "综合教学楼", Hits: 61}) {
		t.Fatalf("教学楼榜不符，实际 %+v", snapshot.Leaderboard.Buildings)
	}
	if len(snapshot.Leaderboard.Rooms) != 1 || snapshot.Leaderboard.Rooms[0] != (RoomHit{RoomID: "0899", Name: "F126", Hits: 14}) {
		t.Fatalf("教室榜不符，实际 %+v", snapshot.Leaderboard.Rooms)
	}
	if len(snapshot.Leaderboard.Keywords) != 2 || snapshot.Leaderboard.Keywords[1] != (KeywordHit{Text: "f126", Hits: 40}) {
		t.Fatalf("关键词榜不符，实际 %+v", snapshot.Leaderboard.Keywords)
	}
	if len(snapshot.NodeDistribution) != 2 || snapshot.NodeDistribution[0] != (NodeHit{Node: "01", Hits: 7}) {
		t.Fatalf("节次分布不符，实际 %+v", snapshot.NodeDistribution)
	}
}

// TestSnapshotEmptyFactsReturnsEmptySlices 覆盖空库：榜单与趋势是空数组而不是 null。
func TestSnapshotEmptyFactsReturnsEmptySlices(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)
	reader := newFactsReader(t, store.StatFacts{}, nil)

	snapshot, err := reader.Snapshot(context.Background(), now, ReadParams{})
	if err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}
	if snapshot.Leaderboard.Buildings == nil || snapshot.Leaderboard.Rooms == nil || snapshot.Leaderboard.Keywords == nil {
		t.Fatal("榜单应为空数组而不是 nil")
	}
	if snapshot.NodeDistribution == nil {
		t.Fatal("节次分布应为空数组而不是 nil")
	}
	if snapshot.Peaks.Top == nil {
		t.Fatal("峰值时段应为空数组而不是 nil")
	}
	if len(snapshot.Peaks.Heatmap) != 7 {
		t.Fatalf("热力应固定 7 行，实际 %d 行", len(snapshot.Peaks.Heatmap))
	}
	for _, row := range snapshot.Peaks.Heatmap {
		if len(row) != 24 {
			t.Fatalf("热力每行应固定 24 列，实际 %d 列", len(row))
		}
	}
	// 空库也要给出连续的 30 天趋势，前端不必补洞。
	if len(snapshot.Trend) != TrendDays {
		t.Fatalf("趋势应固定 %d 天，实际 %d 天", TrendDays, len(snapshot.Trend))
	}
	if last := snapshot.Trend[len(snapshot.Trend)-1]; last.Day != "2026-11-02" || last.PV != 0 {
		t.Fatalf("趋势最后一天应是今天且为 0，实际 %+v", last)
	}
}

// TestTrendFillsMissingDays 覆盖逐日趋势补零与跨时区日期解释（Requirement 34.7）。
func TestTrendFillsMissingDays(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)
	// 仓储把 date 扫成 UTC 零点，整形时必须取年月日而不是时区换算。
	facts := store.StatFacts{Days: []store.StatDayPV{
		{Day: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), PV: 4},
		{Day: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), PV: 7},
	}}
	reader := newFactsReader(t, facts, nil)

	snapshot, err := reader.Snapshot(context.Background(), now, ReadParams{})
	if err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}
	if len(snapshot.Trend) != TrendDays {
		t.Fatalf("趋势应固定 %d 天，实际 %d 天", TrendDays, len(snapshot.Trend))
	}
	if snapshot.Trend[0].Day != "2026-10-04" {
		t.Fatalf("趋势首日应为 2026-10-04，实际 %s", snapshot.Trend[0].Day)
	}
	if got := snapshot.Trend[len(snapshot.Trend)-2]; got.Day != "2026-11-01" || got.PV != 4 {
		t.Fatalf("倒数第二天应为 2026-11-01 且 PV=4，实际 %+v", got)
	}
	if got := snapshot.Trend[len(snapshot.Trend)-1]; got.Day != "2026-11-02" || got.PV != 7 {
		t.Fatalf("最后一天应为 2026-11-02 且 PV=7，实际 %+v", got)
	}
	// 7 日口径就是趋势末尾 7 项，中间缺失的日补 0。
	if got := snapshot.Trend[10].PV; got != 0 {
		t.Fatalf("没有数据的日应补 0，实际 %d", got)
	}
}

// TestPeaksShareSumsToOne 覆盖热力矩阵与占比：按格占比之和为 1，峰值取前三（Requirement 34.6）。
func TestPeaksShareSumsToOne(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)
	facts := store.StatFacts{Heatmap: []store.StatHeatCell{
		{Weekday: 1, Hour: 8, PV: 10},
		{Weekday: 1, Hour: 9, PV: 5},
		{Weekday: 2, Hour: 8, PV: 3},
		{Weekday: 7, Hour: 23, PV: 2},
	}}
	reader := newFactsReader(t, facts, nil)

	snapshot, err := reader.Snapshot(context.Background(), now, ReadParams{})
	if err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}

	heatmap := snapshot.Peaks.Heatmap
	if heatmap[0][8] != 10 || heatmap[0][9] != 5 || heatmap[1][8] != 3 || heatmap[6][23] != 2 {
		t.Fatalf("热力矩阵落格不符，实际 %v", heatmap)
	}

	total := 0
	for _, row := range heatmap {
		for _, pv := range row {
			total += pv
		}
	}
	if total != 20 {
		t.Fatalf("热力总数应为 20，实际 %d", total)
	}

	// 把每一格按总数折成占比，合计必须是 1。
	sum := 0.0
	for _, row := range heatmap {
		for _, pv := range row {
			sum += float64(pv) / float64(total)
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("按格占比之和应为 1，实际 %v", sum)
	}

	if len(snapshot.Peaks.Top) != PeakTopN {
		t.Fatalf("期望 %d 个峰值时段，实际 %d 个", PeakTopN, len(snapshot.Peaks.Top))
	}
	want := []Peak{
		{Weekday: 1, Hour: 8, Share: 0.5},
		{Weekday: 1, Hour: 9, Share: 0.25},
		{Weekday: 2, Hour: 8, Share: 0.15},
	}
	for i, peak := range snapshot.Peaks.Top {
		if peak.Weekday != want[i].Weekday || peak.Hour != want[i].Hour || math.Abs(peak.Share-want[i].Share) > 1e-9 {
			t.Fatalf("第 %d 个峰值期望 %+v，实际 %+v", i+1, want[i], peak)
		}
	}
}

// TestPeaksTopTieBreak 覆盖同访问量时的稳定排序：先按星期、再按小时升序。
func TestPeaksTopTieBreak(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, shanghai)
	facts := store.StatFacts{Heatmap: []store.StatHeatCell{
		{Weekday: 3, Hour: 10, PV: 5},
		{Weekday: 1, Hour: 9, PV: 5},
		{Weekday: 2, Hour: 9, PV: 5},
	}}
	reader := newFactsReader(t, facts, nil)

	snapshot, err := reader.Snapshot(context.Background(), now, ReadParams{})
	if err != nil {
		t.Fatalf("读取访问统计失败: %v", err)
	}
	top := snapshot.Peaks.Top
	if len(top) != 3 {
		t.Fatalf("期望 3 个峰值时段，实际 %d 个", len(top))
	}
	if top[0].Weekday != 1 || top[1].Weekday != 2 || top[2].Weekday != 3 {
		t.Fatalf("同分应按星期升序，实际 %+v", top)
	}
}
