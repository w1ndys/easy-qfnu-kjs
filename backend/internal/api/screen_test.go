package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// fakeScreenProvider 记录入参并返回预设结果。
type fakeScreenProvider struct {
	result    query.ScreenResult
	err       error
	calls     int
	gotNow    time.Time
	gotParams query.ScreenParams
}

// Screen 实现 ScreenProvider。
func (f *fakeScreenProvider) Screen(_ context.Context, now time.Time, params query.ScreenParams) (query.ScreenResult, error) {
	f.calls++
	f.gotNow = now
	f.gotParams = params
	return f.result, f.err
}

// serveScreen 打一次不带查询参数的 /api/screen。
func serveScreen(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	return serveScreenQuery(t, srv, "")
}

// serveScreenQuery 打一次带查询串的 /api/screen。
func serveScreenQuery(t *testing.T, srv *Server, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/screen"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// screenEnvelope 是 /api/screen 成功响应的数据形状。
type screenEnvelope struct {
	Code    int                `json:"code"`
	Message string             `json:"message"`
	Data    query.ScreenResult `json:"data"`
	ReqID   string             `json:"request_id"`
}

// screenMatrix 造 7 × 5 的占用率矩阵，第一格 0.4，其余为 0。
func screenMatrix() [][]float64 {
	matrix := make([][]float64, 7)
	for weekday := range matrix {
		matrix[weekday] = make([]float64, 5)
	}
	matrix[0][0] = 0.4
	return matrix
}

// screenHeatmap 造 7 × 24 的访问热力矩阵。
func screenHeatmap() [][]int {
	matrix := make([][]int, 7)
	for weekday := range matrix {
		matrix[weekday] = make([]int, 24)
	}
	matrix[0][8] = 118
	return matrix
}

// screenResultFixture 造一份完整的大屏载荷：基础块 + 访问统计块。
func screenResultFixture() query.ScreenResult {
	duration := int64(1830000)
	successAt := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC)
	return query.ScreenResult{
		Scale: query.ScreenScale{
			Rooms:     2481,
			Buildings: 26,
			Term:      "2026-2027-1",
			Week:      5,
			Window:    []int{5, 8},
		},
		Release: &query.ScreenRelease{
			ReleaseID:   "20261023T071012Z",
			GeneratedAt: time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC),
		},
		Weeks: []query.WeekFreshness{
			{Week: 5, Published: true, LastSuccessAt: &successAt},
		},
		LastRun: &query.ScreenLastRun{
			Status:       "partial",
			StartedAt:    time.Date(2026, 10, 27, 1, 0, 0, 0, time.UTC),
			DurationMS:   &duration,
			AttrConflict: true,
		},
		Anomalies: query.ScreenAnomalies{UnknownCells: 12, CompositeCells: 51, AttrConflicts: 2},
		Occupancy: query.ScreenOccupancy{
			ByBuilding: []query.ScreenBuildingOccupancy{
				{BuildingID: "Q10", Name: "综合教学楼", Cells: 14700, Occupied: 3821, Rate: 0.26},
			},
			ByWeekdayBlock: screenMatrix(),
		},
		Traffic: stats.Traffic{
			PV: stats.Counts{Today: 1284, D7: 8931, D30: 37220},
			UV: stats.Counts{Today: 263, D7: 1512, D30: 5401},
		},
		Leaderboard: stats.Leaderboard{
			Buildings: []stats.BuildingHit{{BuildingID: "Q10", Name: "综合教学楼", Hits: 612}},
			Rooms:     []stats.RoomHit{{RoomID: "0899", Name: "F126", Hits: 148}},
			Keywords:  []stats.KeywordHit{{Text: "综合楼", Hits: 94}},
		},
		Peaks:            stats.Peaks{Heatmap: screenHeatmap(), Top: []stats.Peak{{Weekday: 1, Hour: 8, Share: 0.11}}},
		Trend:            []stats.TrendPoint{{Day: "2026-10-28", PV: 1180}},
		NodeDistribution: []stats.NodeHit{{Node: "01", Hits: 4210}},
	}
}

// TestScreenOK 覆盖 /api/screen 的成功响应形状与时钟透传。
func TestScreenOK(t *testing.T) {
	fake := &fakeScreenProvider{result: screenResultFixture()}
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))
	now := fixedQueryClock()
	srv.clock = func() time.Time { return now }

	rec := serveScreen(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got screenEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != 0 || got.Message != "success" {
		t.Fatalf("信封不符: %+v", got)
	}
	if len(got.ReqID) != len("req_")+32 || got.ReqID[:4] != "req_" {
		t.Fatalf("request_id 不符合 req_+32 位十六进制: %q", got.ReqID)
	}
	if got.Data.Empty {
		t.Fatal("有当前版本时 empty 应为 false")
	}
	if got.Data.Release == nil || got.Data.Release.ReleaseID != "20261023T071012Z" {
		t.Fatalf("版本不符: %+v", got.Data.Release)
	}
	if got.Data.Scale.Rooms != 2481 || got.Data.Scale.Week != 5 || len(got.Data.Scale.Window) != 2 {
		t.Fatalf("规模不符: %+v", got.Data.Scale)
	}
	if len(got.Data.Weeks) != 1 || got.Data.Weeks[0].Week != 5 {
		t.Fatalf("逐周状态不符: %+v", got.Data.Weeks)
	}
	if got.Data.LastRun == nil || got.Data.LastRun.DurationMS == nil || *got.Data.LastRun.DurationMS != 1830000 {
		t.Fatalf("最近轮次不符: %+v", got.Data.LastRun)
	}
	if got.Data.Anomalies.AttrConflicts != 2 || got.Data.Anomalies.UnknownCells != 12 {
		t.Fatalf("异常计数不符: %+v", got.Data.Anomalies)
	}
	if len(got.Data.Occupancy.ByBuilding) != 1 || len(got.Data.Occupancy.ByWeekdayBlock) != 7 {
		t.Fatalf("占用分布不符: %+v", got.Data.Occupancy)
	}
	if got.Data.Traffic.PV.Today != 1284 || got.Data.Peaks.Top[0].Hour != 8 {
		t.Fatalf("访问统计块不符: %+v / %+v", got.Data.Traffic, got.Data.Peaks)
	}
	if !fake.gotNow.Equal(now) {
		t.Fatalf("时钟透传不符: %s", fake.gotNow)
	}
}

// TestScreenPeakDaysDefault 覆盖 peak_days 缺省：不传时查询层收到近 7 日窗口。
func TestScreenPeakDaysDefault(t *testing.T) {
	fake := &fakeScreenProvider{result: screenResultFixture()}
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))

	if rec := serveScreen(t, srv); rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if fake.gotParams.PeakDays != 7 {
		t.Fatalf("缺省 peak_days 应为 7，实际 %d", fake.gotParams.PeakDays)
	}
}

// TestScreenPeakDaysExplicit 覆盖显式 7 与 30：原样下传查询层，不吞参数。
func TestScreenPeakDaysExplicit(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{name: "显式 7", raw: "peak_days=7", want: 7},
		{name: "显式 30", raw: "peak_days=30", want: 30},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeScreenProvider{result: screenResultFixture()}
			srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))

			rec := serveScreenQuery(t, srv, c.raw)
			if rec.Code != http.StatusOK {
				t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
			}
			if fake.gotParams.PeakDays != c.want {
				t.Fatalf("peak_days 应下传 %d，实际 %d", c.want, fake.gotParams.PeakDays)
			}
		})
	}
}

// TestScreenPeakDaysInvalid 覆盖非法 peak_days：与 9.5 的参数错误口径一致，
// HTTP 400 + 业务码 40001，且非法请求不进查询层。
func TestScreenPeakDaysInvalid(t *testing.T) {
	for _, raw := range []string{
		"peak_days=abc",
		"peak_days=14",
		"peak_days=0",
		"peak_days=31",
		"peak_days=-7",
		"peak_days=30.0",
	} {
		t.Run(raw, func(t *testing.T) {
			fake := &fakeScreenProvider{result: screenResultFixture()}
			srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))

			rec := serveScreenQuery(t, srv, raw)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d，body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if got.Code != 40001 || got.Message == "" {
				t.Fatalf("期望业务码 40001 与非空描述，实际 %+v", got)
			}
			if fake.calls != 0 {
				t.Fatalf("非法参数不应进入查询层，实际调用 %d 次", fake.calls)
			}
		})
	}
}

// TestScreenJSONKeys 按契约键名断言 data 各层，防止漏写 json tag 时字段名变成驼峰。
func TestScreenJSONKeys(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(&fakeScreenProvider{result: screenResultFixture()}))

	var got map[string]any
	if err := json.Unmarshal(serveScreen(t, srv).Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %v", got["data"])
	}
	for _, key := range []string{
		"scale", "release", "weeks", "last_run", "anomalies", "occupancy",
		"traffic", "leaderboard", "peaks", "trend", "node_distribution", "empty",
	} {
		if _, exists := data[key]; !exists {
			t.Fatalf("缺少契约键 %q，实际键: %v", key, keysOf(data))
		}
	}
	scale := data["scale"].(map[string]any)
	for _, key := range []string{"rooms", "buildings", "term", "week", "window"} {
		if _, exists := scale[key]; !exists {
			t.Fatalf("scale 缺少契约键 %q，实际键: %v", key, keysOf(scale))
		}
	}
	lastRun := data["last_run"].(map[string]any)
	for _, key := range []string{"status", "started_at", "duration_ms", "attr_conflict"} {
		if _, exists := lastRun[key]; !exists {
			t.Fatalf("last_run 缺少契约键 %q，实际键: %v", key, keysOf(lastRun))
		}
	}
	occupancy := data["occupancy"].(map[string]any)
	building := occupancy["by_building"].([]any)[0].(map[string]any)
	for _, key := range []string{"building_id", "name", "cells", "occupied", "rate"} {
		if _, exists := building[key]; !exists {
			t.Fatalf("by_building 缺少契约键 %q，实际键: %v", key, keysOf(building))
		}
	}
}

// TestScreenEmptyState 覆盖无当前版本：HTTP 200、code 0、release:null 与 empty:true，
// 不返回 40401，也不把空块序列化成 null。
// 夹具按 query.emptyScreenResult 的形状造：traffic 与 peaks 是对象，
// 「不是 null」落在它们的数组成员（榜单、peak.top、热力矩阵）与计数零值上。
func TestScreenEmptyState(t *testing.T) {
	matrix := make([][]float64, 7)
	for weekday := range matrix {
		matrix[weekday] = make([]float64, 5)
	}
	// 空态热力仍是固定 7 × 24 的全零矩阵，前端不必切换形状。
	heatmap := make([][]int, 7)
	for weekday := range heatmap {
		heatmap[weekday] = make([]int, 24)
	}
	fake := &fakeScreenProvider{result: query.ScreenResult{
		Scale: query.ScreenScale{Window: []int{}},
		Weeks: []query.WeekFreshness{},
		Empty: true,
		Occupancy: query.ScreenOccupancy{
			ByBuilding:     []query.ScreenBuildingOccupancy{},
			ByWeekdayBlock: matrix,
		},
		Leaderboard: stats.Leaderboard{
			Buildings: []stats.BuildingHit{},
			Rooms:     []stats.RoomHit{},
			Keywords:  []stats.KeywordHit{},
		},
		Peaks:            stats.Peaks{Heatmap: heatmap, Top: []stats.Peak{}},
		Trend:            []stats.TrendPoint{},
		NodeDistribution: []stats.NodeHit{},
	}}
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))

	rec := serveScreen(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("空态期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got["code"].(float64) != 0 {
		t.Fatalf("空态业务码期望 0，实际 %v", got["code"])
	}
	data := got["data"].(map[string]any)
	if data["release"] != nil {
		t.Fatalf("空态 release 期望 null，实际 %v", data["release"])
	}
	if data["empty"] != true {
		t.Fatalf("空态 empty 期望 true，实际 %v", data["empty"])
	}
	if weeks, ok := data["weeks"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("空态 weeks 期望空数组，实际 %v", data["weeks"])
	}
	if window, ok := data["scale"].(map[string]any)["window"].([]any); !ok || len(window) != 0 {
		t.Fatalf("空态 window 期望空数组，实际 %v", data["scale"])
	}
	occupancy := data["occupancy"].(map[string]any)
	if buildings, ok := occupancy["by_building"].([]any); !ok || len(buildings) != 0 {
		t.Fatalf("空态 by_building 期望空数组，实际 %v", occupancy["by_building"])
	}

	// 访问统计块在空态下也不能是 null：数组给 []，计数给 0。
	assertEmptyArray := func(name string, raw any) {
		t.Helper()
		items, ok := raw.([]any)
		if !ok {
			t.Fatalf("%s 期望 []（而不是 null），实际 %v", name, raw)
		}
		if len(items) != 0 {
			t.Fatalf("%s 期望空数组，实际 %v", name, items)
		}
	}

	traffic, ok := data["traffic"].(map[string]any)
	if !ok {
		t.Fatalf("空态 traffic 期望对象，实际 %v", data["traffic"])
	}
	for _, kind := range []string{"pv", "uv"} {
		counts, ok := traffic[kind].(map[string]any)
		if !ok {
			t.Fatalf("空态 traffic.%s 期望对象，实际 %v", kind, traffic[kind])
		}
		for _, window := range []string{"today", "d7", "d30"} {
			if counts[window] != float64(0) {
				t.Fatalf("空态 traffic.%s.%s 期望 0，实际 %v", kind, window, counts[window])
			}
		}
	}

	leaderboard, ok := data["leaderboard"].(map[string]any)
	if !ok {
		t.Fatalf("空态 leaderboard 期望对象，实际 %v", data["leaderboard"])
	}
	for _, key := range []string{"buildings", "rooms", "keywords"} {
		assertEmptyArray("leaderboard."+key, leaderboard[key])
	}

	peaks, ok := data["peaks"].(map[string]any)
	if !ok {
		t.Fatalf("空态 peaks 期望对象，实际 %v", data["peaks"])
	}
	assertEmptyArray("peaks.top", peaks["top"])
	rows, ok := peaks["heatmap"].([]any)
	if !ok || len(rows) != 7 {
		t.Fatalf("空态 peaks.heatmap 期望 7 行，实际 %v", peaks["heatmap"])
	}
	for weekday, row := range rows {
		cells, ok := row.([]any)
		if !ok || len(cells) != 24 {
			t.Fatalf("空态热力第 %d 行期望 24 列，实际 %v", weekday+1, row)
		}
	}

	assertEmptyArray("trend", data["trend"])
	assertEmptyArray("node_distribution", data["node_distribution"])
}

// TestScreenNoErrorDetails 覆盖 Requirement 33.6 与 CP13：
// 响应只含状态、时间与计数，不出现错误码、错误原文与账号。
func TestScreenNoErrorDetails(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(&fakeScreenProvider{result: screenResultFixture()}))

	payload := serveScreen(t, srv).Body.String()
	for _, banned := range []string{"error_code", "error_message", "password", "account", "cookie"} {
		if strings.Contains(payload, banned) {
			t.Fatalf("大屏响应不应包含 %q: %s", banned, payload)
		}
	}
}

// TestScreenFailure 覆盖查询失败：500 与业务码 50000，且不泄露内部细节。
func TestScreenFailure(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil,
		WithScreen(&fakeScreenProvider{err: errors.New("db down")}))

	rec := serveScreen(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	var got struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got.Code != 50000 {
		t.Fatalf("期望业务码 50000，实际 %d", got.Code)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
}

// TestScreenNotWired 覆盖未装配大屏服务：按内部错误处理，不 panic。
func TestScreenNotWired(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	rec := serveScreen(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未装配时期望 500，实际 %d", rec.Code)
	}
}

// TestScreenRateLimited 覆盖公开端点的限流分支：同来源第 61 次返回 429 + 42901，
// 且被拒绝的请求不再进 handler。
func TestScreenRateLimited(t *testing.T) {
	fake := &fakeScreenProvider{result: screenResultFixture()}
	srv := New(quietLogger(), nil, nil, nil, nil, WithScreen(fake))
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveScreen(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d，body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("窗口内应调用 %d 次，实际 %d 次", rateLimitMaxPerWindow, fake.calls)
	}

	assertRateLimited(t, serveScreen(t, srv))
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("被限流的请求不应再进 handler，实际调用 %d 次", fake.calls)
	}
}

// TestScreenSharesRateLimitBucketWithContext 覆盖大屏与其它公开端点共用同一个限流桶：
// context 用满额度后，screen 直接 429，而不是重新获得 60 次（与 context/query/meta 同桶）。
func TestScreenSharesRateLimitBucketWithContext(t *testing.T) {
	fake := &fakeScreenProvider{result: screenResultFixture()}
	srv := New(quietLogger(), nil, fakeCtx{info: query.ContextInfo{Date: "2026-10-28"}}, nil, nil, WithScreen(fake))
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 context 请求应放行，实际 %d", i+1, rec.Code)
		}
	}

	assertRateLimited(t, serveScreen(t, srv))
	if fake.calls != 0 {
		t.Fatalf("被限流的 screen 请求不应进 handler，实际 %d 次", fake.calls)
	}
}

// TestScreenDoesNotRecordVisit 覆盖大屏不记访问统计：
// 它按 30 秒轮询，记进去只会把 PV 变成在线时长。
func TestScreenDoesNotRecordVisit(t *testing.T) {
	recorder := &captureRecorder{}
	srv := New(quietLogger(), nil, nil, nil, nil,
		WithScreen(&fakeScreenProvider{result: screenResultFixture()}), WithStats(recorder))

	if rec := serveScreen(t, srv); rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if visits := recorder.snapshot(); len(visits) != 0 {
		t.Fatalf("大屏不该记访问统计，实际 %d 条", len(visits))
	}
}
