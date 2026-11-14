package api

// 本文件是版本管理（admin-release）在 HTTP 层的单测与契约测试：
//
//	第 5 项：WithReleaseAdmin 的装配语义（reader / invalidate 为 nil 时不 panic、恰好一次）
//	第 7 项：五个端点的路由、参数门、DTO 形状与错误映射
//
// 第 7 项的用例一律走完整的 s.Handler()：路由注册、adminRead / adminWriteNoBody 的
// 中间件链（no-store、会话、CSRF）都由真实实现承担，替身只负责数据来源。要测的是
// HTTP 契约本身——状态码、业务码、键集合、可空列的 null、时间格式、空列表的 []、
// 日志字段白名单，以及「参数非法 / 无会话 / 缺 CSRF 时仓储零调用」（CP12–CP15）。
//
// 仓储的 SQL、折叠口径与回退事务由 store 的单测与集成测试锁住（tasklist 1–6），
// 这里不重查一遍。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// TestWithReleaseAdminSkipsNilInvalidate 覆盖 CP15 的「invalidate 为 nil」分支：
// reader 与回调都不传时装配不失败，失效辅助方法直接跳过且不 panic（回退本身照常成功，
// 该路径由下面的端点契约测试断言）。
func TestWithReleaseAdminSkipsNilInvalidate(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, WithReleaseAdmin(nil, nil))

	if s.releaseAdmin != nil {
		t.Fatalf("reader 传 nil 时 releaseAdmin 应保持 nil，实际 %T", s.releaseAdmin)
	}
	if s.releaseInvalidate != nil {
		t.Fatal("invalidate 传 nil 时回调应保持 nil，实际非 nil")
	}

	// 不 panic 就是通过：nil 回调表示「未装配查询缓存」，handler 不必先判空。
	s.invalidateReleaseCaches()
}

// TestWithReleaseAdminCallsInvalidateOnce 覆盖「非 nil 回调恰好调用一次」：
// handler 在回退提交成功后只走一次失效辅助方法，清一遍三张缓存即可。
func TestWithReleaseAdminCallsInvalidateOnce(t *testing.T) {
	calls := 0
	s := New(nil, nil, nil, nil, nil, WithReleaseAdmin(nil, func() { calls++ }))

	s.invalidateReleaseCaches()

	if calls != 1 {
		t.Fatalf("非 nil 回调应恰好调用一次，实际 %d 次", calls)
	}
}

// 版本管理契约测试的固定版本标识与时刻。
//
// 两个 id 都是 `YYYYMMDDTHHMMSSZ` 形状，且新旧不同：对比用例需要一对合法且不等的
// 版本；生成时间用一个非 UTC 时区构造（CST 的 11:05:12 等于 03:05:12Z），这样即便
// 删掉 handler 里的 .UTC()，用例也会因为出现 +08:00 偏移而失败。
var (
	releaseCST = time.FixedZone("CST", 8*3600)

	releaseNewID = "20261114T030512Z"
	releaseOldID = "20261028T041500Z"
	releaseTerm  = "2026-2027-1"

	releaseNewGeneratedAt = time.Date(2026, 11, 14, 11, 5, 12, 0, releaseCST) // 03:05:12Z
	releaseOldGeneratedAt = time.Date(2026, 10, 28, 12, 15, 0, 0, releaseCST) // 04:15:00Z
	releaseNewReleasedAt  = time.Date(2026, 11, 14, 11, 6, 40, 0, releaseCST) // 03:06:40Z
)

// 响应键集合（design 的五个响应示例）。每一层都单独列出来：契约要求键集合封闭，
// 多一个或少一个都是漂移，用切片集中定义让断言与用例读的是同一份名单。
var (
	releaseSummaryKeys     = []string{"release_id", "term", "generated_at", "released_at", "is_current", "has_attr_conflict", "week_count", "observation_rows", "rooms", "dict_version", "axis_version"}
	releaseStatsKeys       = []string{"observation_rows", "block_cells", "week_count", "rooms", "buildings"}
	releaseWeekKeys        = []string{"week", "observation_rows", "block_cells"}
	releaseBuildingKeys    = []string{"building_id", "building_name", "rooms"}
	releaseSideKeys        = []string{"release_id", "term", "generated_at", "dict_version", "axis_version", "is_current", "has_attr_conflict"}
	releaseDiffSummaryKeys = []string{"cells", "rooms", "unknown", "composite", "inconsistent_blocks", "same_term"}
	diffCellsSummaryKeys   = []string{"added", "removed", "changed", "total"}
	diffRoomsSummaryKeys   = []string{"added", "removed", "shared", "building_changed"}
	stateDeltaKeys         = []string{"from", "to", "delta"}
	diffCellItemKeys       = []string{"kind", "room_id", "room_name", "week", "weekday", "block", "from_state", "to_state", "from_building", "to_building"}
	diffRoomItemKeys       = []string{"kind", "room_id", "room_name", "from_building", "from_building_name", "to_building", "to_building_name"}
	diffBuildingItemKeys   = diffRoomItemKeys
	rollbackKeys           = []string{"rolled_back_from", "current_release_id", "term"}
	anomalyKeys            = []string{"release_id", "term", "generated_at", "is_current", "has_attr_conflict", "unknown", "composite", "observed_weeks", "week_gaps", "window", "conflict_rooms", "conflict_rooms_truncated"}
	anomalyStateKeys       = []string{"cells", "ratio"}
	releaseWindowKeys      = []string{"start", "end"}
	conflictRoomKeys       = []string{"room_id", "building_id", "conflict_building_id"}
)

// releaseAdminReaderStub 是 ReleaseAdminReader 的内存替身：回放预置数据、记录调用次数与入参。
//
// 五项错误分别可注入，用来断言「某一层失败 → 50000 / 40401 / 409xx」；调用计数是
// 「参数门在查库之前」「无会话 / 缺 CSRF 时仓储零调用」的唯一证据——响应形状相同，
// 只有替身到底被调了几次才能区分两种实现顺序。
type releaseAdminReaderStub struct {
	releases    []store.ReleaseSummary
	detail      store.ReleaseDetailView
	detailFound bool
	diff        store.ReleaseDiff
	anomalies   []store.ReleaseAnomaly
	rollback    store.RollbackResult

	listErr      error
	detailErr    error
	diffErr      error
	anomaliesErr error
	rollbackErr  error

	listCalls      int
	detailCalls    int
	diffCalls      int
	anomaliesCalls int
	rollbackCalls  int

	lastDetailID   string
	lastDiffParams store.ReleaseDiffParams
	lastRollbackID string
}

// List 回放版本列表。
func (s *releaseAdminReaderStub) List(context.Context) ([]store.ReleaseSummary, error) {
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.releases, nil
}

// Detail 回放详情；detailFound=false 时按「没有这一版」返回 (零值, false, nil)。
func (s *releaseAdminReaderStub) Detail(_ context.Context, releaseID string) (store.ReleaseDetailView, bool, error) {
	s.detailCalls++
	s.lastDetailID = releaseID
	if s.detailErr != nil {
		return store.ReleaseDetailView{}, false, s.detailErr
	}
	return s.detail, s.detailFound, nil
}

// Diff 回放对比结果并记下收到的参数（参数门的断言靠它）。
func (s *releaseAdminReaderStub) Diff(_ context.Context, params store.ReleaseDiffParams) (store.ReleaseDiff, error) {
	s.diffCalls++
	s.lastDiffParams = params
	if s.diffErr != nil {
		return store.ReleaseDiff{}, s.diffErr
	}
	return s.diff, nil
}

// Anomalies 回放异常聚合。
func (s *releaseAdminReaderStub) Anomalies(context.Context) ([]store.ReleaseAnomaly, error) {
	s.anomaliesCalls++
	if s.anomaliesErr != nil {
		return nil, s.anomaliesErr
	}
	return s.anomalies, nil
}

// Rollback 回放回退结果并记下目标版本。
func (s *releaseAdminReaderStub) Rollback(_ context.Context, releaseID string) (store.RollbackResult, error) {
	s.rollbackCalls++
	s.lastRollbackID = releaseID
	if s.rollbackErr != nil {
		return store.RollbackResult{}, s.rollbackErr
	}
	return s.rollback, nil
}

// calls 返回五个方法的总调用次数，供「参数门 / 鉴权失败时仓储零调用」的断言使用。
func (s *releaseAdminReaderStub) calls() int {
	return s.listCalls + s.detailCalls + s.diffCalls + s.anomaliesCalls + s.rollbackCalls
}

// 编译期钉住替身的方法集与消费侧接口一致：接口增删方法时，测试替身先失败，
// 而不是等到某个用例运行期才报「缺方法」。
var _ ReleaseAdminReader = (*releaseAdminReaderStub)(nil)

// currentReleaseSummary 造「当前版本」摘要：有发布时刻、无归属冲突、非空计数。
func currentReleaseSummary() store.ReleaseSummary {
	return store.ReleaseSummary{
		ReleaseID:       releaseNewID,
		Term:            releaseTerm,
		GeneratedAt:     releaseNewGeneratedAt,
		ReleasedAt:      &releaseNewReleasedAt,
		IsCurrent:       true,
		HasAttrConflict: false,
		WeekCount:       4,
		ObservationRows: 871320,
		Rooms:           2481,
		DictVersion:     1,
		AxisVersion:     1,
	}
}

// previousReleaseSummary 造「非当前版本」摘要：没有可关联的发布轮次（released_at 为
// null），并把 has_attr_conflict 翻成 true，让两个布尔字段的两种取值都至少走一次。
func previousReleaseSummary() store.ReleaseSummary {
	return store.ReleaseSummary{
		ReleaseID:       releaseOldID,
		Term:            releaseTerm,
		GeneratedAt:     releaseOldGeneratedAt,
		ReleasedAt:      nil,
		IsCurrent:       false,
		HasAttrConflict: true,
		WeekCount:       4,
		ObservationRows: 869904,
		Rooms:           2481,
		DictVersion:     1,
		AxisVersion:     1,
	}
}

// releaseDetailFixture 造一份非空的版本详情：两行逐周、两栋楼。
func releaseDetailFixture() store.ReleaseDetailView {
	return store.ReleaseDetailView{
		Release: currentReleaseSummary(),
		Stats: store.ReleaseStats{
			ObservationRows: 871320,
			BlockCells:      363050,
			WeekCount:       2,
			Rooms:           2481,
			Buildings:       2,
		},
		WeekCounts: []store.ReleaseWeekCount{
			{Week: 9, ObservationRows: 217830, BlockCells: 90762},
			{Week: 10, ObservationRows: 653490, BlockCells: 272288},
		},
		BuildingCounts: []store.ReleaseBuildingCount{
			{BuildingID: "Q03", BuildingName: "老文史楼", Rooms: 86},
			{BuildingID: "Q10", BuildingName: "综合教学楼", Rooms: 2395},
		},
	}
}

// releaseDiffFixture 造一份三节都有数据的对比结果：两侧版本头、分类汇总与每种
// 元素的可空字段（from_state / to_state / from_building / to_building）。
func releaseDiffFixture() store.ReleaseDiff {
	fromState, toState := "free", "class"
	fromBuilding, toBuilding := "Q10", "Q03"
	fromRoomBuilding, toRoomBuilding := "Q10", "Q03"
	fromBuildingName, toBuildingName := "综合教学楼", "老文史楼"

	return store.ReleaseDiff{
		From: store.ReleaseSide{
			ReleaseID:       releaseOldID,
			Term:            releaseTerm,
			GeneratedAt:     releaseOldGeneratedAt,
			DictVersion:     1,
			AxisVersion:     1,
			IsCurrent:       false,
			HasAttrConflict: true,
		},
		To: store.ReleaseSide{
			ReleaseID:       releaseNewID,
			Term:            releaseTerm,
			GeneratedAt:     releaseNewGeneratedAt,
			DictVersion:     1,
			AxisVersion:     1,
			IsCurrent:       true,
			HasAttrConflict: false,
		},
		Summary: store.ReleaseDiffSummary{
			Cells:              store.DiffCellsSummary{Added: 120, Removed: 35, Changed: 2048, Total: 2203},
			Rooms:              store.DiffRoomsSummary{Added: 2, Removed: 1, Shared: 2478, BuildingChanged: 3},
			Unknown:            store.StateDelta{From: 12, To: 0, Delta: -12},
			Composite:          store.StateDelta{From: 51, To: 48, Delta: -3},
			InconsistentBlocks: 0,
			SameTerm:           true,
		},
		Cells: []store.DiffCellRow{
			{
				Kind: store.DiffKindChanged, RoomID: "0899", RoomName: "F126",
				Week: 9, Weekday: 1, Block: "0102",
				FromState: &fromState, ToState: &toState,
				FromBuilding: &fromBuilding, ToBuilding: &toBuilding,
			},
			{
				// 只在 to 侧出现：from_state / from_building 必须输出 null。
				Kind: store.DiffKindAdded, RoomID: "0900", RoomName: "F127",
				Week: 9, Weekday: 1, Block: "0102",
				FromState: nil, ToState: &toState,
				FromBuilding: nil, ToBuilding: &toBuilding,
			},
		},
		Rooms: []store.DiffRoomRow{
			{
				Kind: store.DiffKindAdded, RoomID: "Q10-0999", RoomName: "F126",
				FromBuilding: nil, FromBuildingName: nil,
				ToBuilding: &toRoomBuilding, ToBuildingName: &toBuildingName,
			},
			{
				Kind: store.DiffKindRemoved, RoomID: "Q03-0101", RoomName: "JA101",
				FromBuilding: &fromRoomBuilding, FromBuildingName: &fromBuildingName,
				ToBuilding: nil, ToBuildingName: nil,
			},
		},
		Buildings: []store.DiffBuildingRow{
			{
				RoomID: "0899", RoomName: "F126",
				FromBuilding: "Q10", FromBuildingName: "综合教学楼",
				ToBuilding: "Q03", ToBuildingName: "老文史楼",
			},
		},
	}
}

// releaseAnomaliesFixture 造两版异常聚合：新版窗口非空、无冲突；旧版窗口为 null、
// 有缺周与一条冲突房号，并置截断标记，让三处可空/可空的形状都走到。
func releaseAnomaliesFixture() []store.ReleaseAnomaly {
	return []store.ReleaseAnomaly{
		{
			ReleaseID:       releaseNewID,
			Term:            releaseTerm,
			GeneratedAt:     releaseNewGeneratedAt,
			IsCurrent:       true,
			HasAttrConflict: false,
			Unknown:         store.StateRatio{Cells: 0, Ratio: 0},
			Composite:       store.StateRatio{Cells: 48, Ratio: 0.0001},
			ObservedWeeks:   []int{9, 10, 11, 12},
			WeekGaps:        []int{},
			Window:          &store.ReleaseWindow{Start: 9, End: 12},
			ConflictRooms:   []store.ConflictRoom{},
		},
		{
			ReleaseID:       releaseOldID,
			Term:            releaseTerm,
			GeneratedAt:     releaseOldGeneratedAt,
			IsCurrent:       false,
			HasAttrConflict: true,
			Unknown:         store.StateRatio{Cells: 12, Ratio: 0},
			Composite:       store.StateRatio{Cells: 51, Ratio: 0.0713},
			ObservedWeeks:   []int{5, 6, 8},
			WeekGaps:        []int{7},
			Window:          nil,
			ConflictRooms: []store.ConflictRoom{
				{RoomID: "0899", BuildingID: "Q10", ConflictBuildingID: "Q03"},
			},
			ConflictRoomsTruncated: true,
		},
	}
}

// newAdminReleasesServer 组装装了会话来源、版本仓储与缓存失效回调的 Server。
func newAdminReleasesServer(reader ReleaseAdminReader, invalidate func()) *Server {
	return newAdminReleasesServerWithLogger(nil, reader, invalidate)
}

// newAdminReleasesServerWithLogger 是带日志器的版本：日志白名单用例需要一个可读缓冲，
// 其余用例传 nil 走 quietLogger。
// 时钟必须拨到 testAdminNow：替身会话以它为基准签发，用真实时间会先撞上 40101。
func newAdminReleasesServerWithLogger(logger *slog.Logger, reader ReleaseAdminReader, invalidate func()) *Server {
	if logger == nil {
		logger = quietLogger()
	}
	srv := New(logger, nil, nil, nil, nil,
		WithAdmin(activeAdminStore(), nil),
		WithReleaseAdmin(reader, invalidate),
	)
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// newAdminReleasesServerWithoutReader 组装「装了会话、没装配版本仓储」的 Server：
// 五个端点都该按 50000 返回（CP15）。
func newAdminReleasesServerWithoutReader() *Server {
	srv := New(quietLogger(), nil, nil, nil, nil, WithAdmin(activeAdminStore(), nil))
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// adminReleasesRequest 构造带会话 Cookie 的读请求；withSession=false 时不带 Cookie。
func adminReleasesRequest(target string, withSession bool) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if withSession {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: testAdminRawToken})
	}
	return req
}

// adminReleaseRollbackRequest 构造回退请求：没有请求体、不设 Content-Type（端点的契约
// 就是不要求它们），withSession / withCSRF 决定带不带 Cookie 与同步器令牌。
func adminReleaseRollbackRequest(target string, withSession, withCSRF bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, nil)
	if withSession {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: testAdminRawToken})
	}
	if withCSRF {
		req.Header.Set(adminCSRFHeader, testAdminCSRF)
	}
	return req
}

// serveAdminReleases 经完整的 Handler 跑一个请求（路由 + 中间件 + handler）。
func serveAdminReleases(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// releaseIDPath 拼出版本详情的路径。
func releaseIDPath(releaseID string) string {
	return "/api/admin/releases/" + releaseID
}

// releaseRollbackPath 拼出回退的路径。
func releaseRollbackPath(releaseID string) string {
	return releaseIDPath(releaseID) + "/rollback"
}

// releaseDiffPath 拼出对比的路径与查询串。
func releaseDiffPath(query string) string {
	return "/api/admin/releases/diff" + query
}

// diffQueryFromTo 是 from/to 都合法且不等的查询串。
func diffQueryFromTo() string {
	return "?from=" + releaseOldID + "&to=" + releaseNewID
}

// contractTotal 把 JSON 解出来的 total 断言成 float64；类型不符直接终止用例。
// 只服务 total 一个字段：contractSuccessData 已经把 data 断言成对象，这里再锁一层
// 「total 是数字而不是字符串或 null」——前端要拿它算下一页，类型错了比值为 0 更难发现。
func contractTotal(t *testing.T, value any) float64 {
	t.Helper()
	total, ok := value.(float64)
	if !ok {
		t.Fatalf("total 不是数字: %#v", value)
	}
	return total
}

// TestReleaseIDValid 锁住版本 id 形状门的纯函数边界（design D13、CP14、第 6 项移交）：
// 只有发布侧会生成的 `YYYYMMDDTHHMMSSZ` 写法通过；分隔符不对、缺 Z、日期不存在、
// 小写 z、超长/空串都拒绝。`20261314T030512Z`（月 13）与 `20260230T030512Z`（2 月 30）
// 是「正则能过、日历不存在」的关键反例——少了 time.Parse 这一层，它们会白打一次库。
func TestReleaseIDValid(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"合法", "20261114T030512Z", true},
		{"合法：年初", "20260101T000000Z", true},
		{"日期分隔符", "2026-11-14", false},
		{"缺结尾 Z", "20261114T030512", false},
		{"小写 z", "20261114T030512z", false},
		{"月 13", "20261314T030512Z", false},
		{"2 月 30", "20260230T030512Z", false},
		{"小时 25", "20261114T250512Z", false},
		{"多一位", "20261114T0305120Z", false},
		{"少一位", "2026111T030512Z0", false},
		{"前缀加号", "+20261114T030512Z", false},
		{"空串", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := releaseIDValid(tc.id); got != tc.want {
				t.Fatalf("releaseIDValid(%q) = %v，期望 %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestAdminReleasesListShape 覆盖 GET /api/admin/releases 的成功形状（Requirement 1.1–1.3）：
// data 恰好 2 个键、每条摘要恰好 11 个键、released_at 的 null 与非 null、
// generated_at 的 UTC 归一化、total 取列表长度、顺序原样透出。
func TestAdminReleasesListShape(t *testing.T) {
	reader := &releaseAdminReaderStub{
		releases: []store.ReleaseSummary{currentReleaseSummary(), previousReleaseSummary()},
	}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest("/api/admin/releases", true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "版本列表 data", data, "releases", "total")

	releases, ok := data["releases"].([]any)
	if !ok || len(releases) != 2 {
		t.Fatalf("releases 期望 2 项数组，实际 %#v", data["releases"])
	}
	if total := contractTotal(t, data["total"]); total != 2 {
		t.Fatalf("total 期望 2，实际 %v", total)
	}
	if reader.listCalls != 1 {
		t.Fatalf("List 应调用一次，实际 %d 次", reader.listCalls)
	}

	current, ok := releases[0].(map[string]any)
	if !ok {
		t.Fatalf("releases[0] 不是对象: %#v", releases[0])
	}
	assertContractKeys(t, "版本摘要", current, releaseSummaryKeys...)
	if current["release_id"] != releaseNewID {
		t.Fatalf("首条 release_id 期望 %s，实际 %v", releaseNewID, current["release_id"])
	}
	// 生成时间用 CST 构造，输出必须是 UTC：这一句同时锁住「UTC 化」与 RFC3339 格式。
	if current["generated_at"] != "2026-11-14T03:05:12Z" {
		t.Fatalf("generated_at 期望 RFC3339 UTC，实际 %v", current["generated_at"])
	}
	if current["released_at"] != "2026-11-14T03:06:40Z" {
		t.Fatalf("released_at 期望 RFC3339 UTC，实际 %v", current["released_at"])
	}
	if current["is_current"] != true || current["has_attr_conflict"] != false {
		t.Fatalf("当前版本标记不符: %#v", current)
	}
	if current["observation_rows"] != float64(871320) || current["rooms"] != float64(2481) {
		t.Fatalf("计数不符: %#v", current)
	}

	previous, ok := releases[1].(map[string]any)
	if !ok {
		t.Fatalf("releases[1] 不是对象: %#v", releases[1])
	}
	assertContractKeys(t, "版本摘要（旧版）", previous, releaseSummaryKeys...)
	// 没有可关联的发布轮次：released_at 必须是 null，不是空串或零值时刻。
	if previous["released_at"] != nil {
		t.Fatalf("无发布轮次时 released_at 期望 null，实际 %#v", previous["released_at"])
	}
	if previous["is_current"] != false || previous["has_attr_conflict"] != true {
		t.Fatalf("旧版标记不符: %#v", previous)
	}
}

// TestAdminReleasesListEmpty 覆盖空库（Requirement 1.4）：releases 是 [] 而不是 null，
// total 为 0，仍是 200 + code 0（空态不是错误）。
func TestAdminReleasesListEmpty(t *testing.T) {
	srv := newAdminReleasesServer(&releaseAdminReaderStub{}, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest("/api/admin/releases", true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "版本列表 data", data, "releases", "total")

	items, ok := data["releases"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("空库 releases 期望空数组，实际 %#v", data["releases"])
	}
	if total := contractTotal(t, data["total"]); total != 0 {
		t.Fatalf("空库 total 期望 0，实际 %v", total)
	}
}

// TestAdminReleaseDetailShape 覆盖 GET /api/admin/releases/{release_id} 的成功形状
// （Requirement 2.1–2.3）：data 4 个键、stats 5 个键、两个统计切片各 3 个键、
// 版本头与列表元素同形、路径参数原样传给仓储。
func TestAdminReleaseDetailShape(t *testing.T) {
	reader := &releaseAdminReaderStub{detail: releaseDetailFixture(), detailFound: true}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(releaseIDPath(releaseNewID), true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "版本详情 data", data, "release", "stats", "week_counts", "building_counts")

	if reader.detailCalls != 1 || reader.lastDetailID != releaseNewID {
		t.Fatalf("Detail 应收到路径参数 %s，实际 calls=%d id=%q",
			releaseNewID, reader.detailCalls, reader.lastDetailID)
	}

	release := contractMap(t, "release", data["release"])
	assertContractKeys(t, "详情 release", release, releaseSummaryKeys...)
	if release["release_id"] != releaseNewID || release["term"] != releaseTerm {
		t.Fatalf("详情版本头不符: %#v", release)
	}

	stats := contractMap(t, "stats", data["stats"])
	assertContractKeys(t, "详情 stats", stats, releaseStatsKeys...)
	if stats["block_cells"] != float64(363050) || stats["week_count"] != float64(2) {
		t.Fatalf("stats 计数不符: %#v", stats)
	}

	weeks, ok := data["week_counts"].([]any)
	if !ok || len(weeks) != 2 {
		t.Fatalf("week_counts 期望 2 项数组，实际 %#v", data["week_counts"])
	}
	week := contractMap(t, "week_counts[0]", weeks[0])
	assertContractKeys(t, "week_counts[0]", week, releaseWeekKeys...)
	if week["week"] != float64(9) || week["block_cells"] != float64(90762) {
		t.Fatalf("逐周计数不符: %#v", week)
	}

	buildings, ok := data["building_counts"].([]any)
	if !ok || len(buildings) != 2 {
		t.Fatalf("building_counts 期望 2 项数组，实际 %#v", data["building_counts"])
	}
	building := contractMap(t, "building_counts[0]", buildings[0])
	assertContractKeys(t, "building_counts[0]", building, releaseBuildingKeys...)
	if building["building_id"] != "Q03" || building["building_name"] != "老文史楼" || building["rooms"] != float64(86) {
		t.Fatalf("按楼计数不符: %#v", building)
	}
}

// TestAdminReleaseDetailEmptyCounts 覆盖「版本存在但没有观测/归属」：两个统计切片是 []
// 而不是 null（仓储已保证非 nil，这里锁住 handler 的唯一序列化入口也满足）。
func TestAdminReleaseDetailEmptyCounts(t *testing.T) {
	detail := store.ReleaseDetailView{Release: currentReleaseSummary()}
	reader := &releaseAdminReaderStub{detail: detail, detailFound: true}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(releaseIDPath(releaseNewID), true))
	data := contractSuccessData(t, rec)

	if weeks, ok := data["week_counts"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("空的 week_counts 期望 []，实际 %#v", data["week_counts"])
	}
	if buildings, ok := data["building_counts"].([]any); !ok || len(buildings) != 0 {
		t.Fatalf("空的 building_counts 期望 []，实际 %#v", data["building_counts"])
	}
}

// TestAdminReleaseDetailUnknownRelease 覆盖未知版本（Requirement 2.4）：
// 404 + 40401，固定文案「发布版本不存在」，不是 50000。
func TestAdminReleaseDetailUnknownRelease(t *testing.T) {
	reader := &releaseAdminReaderStub{detailFound: false}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(releaseIDPath(releaseNewID), true))
	body := assertContractFailure(t, rec, http.StatusNotFound, codeReleaseNotFound)
	assertNoStore(t, rec)
	if body["message"] != "发布版本不存在" {
		t.Fatalf("404 文案不符: %v", body["message"])
	}
}

// TestAdminReleaseDiffShape 覆盖 GET /api/admin/releases/diff 的缺省分支（Requirement 3.1–3.4、4.1）：
// data 9 个键、两侧版本头 7 键、汇总 6 键（含 4 个子结构）、cells 元素 10 键、
// from_state / from_building 的 null、total 由汇总推导、分页回显缺省值。
func TestAdminReleaseDiffShape(t *testing.T) {
	reader := &releaseAdminReaderStub{diff: releaseDiffFixture()}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "版本对比 data", data,
		"from", "to", "summary", "items", "total", "limit", "offset", "section", "kind")

	// 参数门把缺省补齐后原样交给仓储：section=cells、kind=all、limit=50、offset=0。
	wantParams := store.ReleaseDiffParams{
		From: releaseOldID, To: releaseNewID,
		Section: store.DiffSectionCells, Kind: store.DiffKindAll, Limit: 50, Offset: 0,
	}
	if reader.lastDiffParams != wantParams {
		t.Fatalf("Diff 收到的参数不符：\n实际 %+v\n期望 %+v", reader.lastDiffParams, wantParams)
	}

	from := contractMap(t, "from", data["from"])
	to := contractMap(t, "to", data["to"])
	assertContractKeys(t, "diff from", from, releaseSideKeys...)
	assertContractKeys(t, "diff to", to, releaseSideKeys...)
	if from["release_id"] != releaseOldID || to["release_id"] != releaseNewID {
		t.Fatalf("两侧版本标识不符: %#v / %#v", from["release_id"], to["release_id"])
	}
	if from["generated_at"] != "2026-10-28T04:15:00Z" || to["generated_at"] != "2026-11-14T03:05:12Z" {
		t.Fatalf("两侧生成时间期望 UTC：%v / %v", from["generated_at"], to["generated_at"])
	}

	summary := contractMap(t, "summary", data["summary"])
	assertContractKeys(t, "diff summary", summary, releaseDiffSummaryKeys...)
	cells := contractMap(t, "summary.cells", summary["cells"])
	assertContractKeys(t, "summary.cells", cells, diffCellsSummaryKeys...)
	if cells["added"] != float64(120) || cells["removed"] != float64(35) ||
		cells["changed"] != float64(2048) || cells["total"] != float64(2203) {
		t.Fatalf("cells 汇总不符: %#v", cells)
	}
	rooms := contractMap(t, "summary.rooms", summary["rooms"])
	assertContractKeys(t, "summary.rooms", rooms, diffRoomsSummaryKeys...)
	if rooms["building_changed"] != float64(3) {
		t.Fatalf("rooms 汇总不符: %#v", rooms)
	}
	unknown := contractMap(t, "summary.unknown", summary["unknown"])
	assertContractKeys(t, "summary.unknown", unknown, stateDeltaKeys...)
	if unknown["from"] != float64(12) || unknown["to"] != float64(0) || unknown["delta"] != float64(-12) {
		t.Fatalf("unknown 汇总不符: %#v", unknown)
	}
	if summary["same_term"] != true || summary["inconsistent_blocks"] != float64(0) {
		t.Fatalf("same_term / inconsistent_blocks 不符: %#v", summary)
	}

	items, ok := data["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items 期望 2 项数组，实际 %#v", data["items"])
	}
	changedCell := contractMap(t, "items[0]", items[0])
	assertContractKeys(t, "items[0]", changedCell, diffCellItemKeys...)
	if changedCell["kind"] != "changed" || changedCell["from_state"] != "free" || changedCell["to_state"] != "class" {
		t.Fatalf("首个差异格不符: %#v", changedCell)
	}
	addedCell := contractMap(t, "items[1]", items[1])
	if addedCell["kind"] != "added" {
		t.Fatalf("第二个差异格类别期望 added，实际 %#v", addedCell["kind"])
	}
	// 只在 to 侧出现的格：from_state 与 from_building 都是 null。
	if addedCell["from_state"] != nil || addedCell["from_building"] != nil {
		t.Fatalf("新增格的 from_* 期望 null，实际 %#v / %#v", addedCell["from_state"], addedCell["from_building"])
	}
	if addedCell["to_state"] != "class" || addedCell["to_building"] != "Q03" {
		t.Fatalf("新增格的 to_* 不符: %#v", addedCell)
	}

	// total 从汇总推导（CP6）：cells + kind=all 取 added+removed+changed。
	if total := contractTotal(t, data["total"]); total != 2203 {
		t.Fatalf("total 期望 2203，实际 %v", total)
	}
	if data["limit"] != float64(50) || data["offset"] != float64(0) {
		t.Fatalf("分页回显不符: limit=%v offset=%v", data["limit"], data["offset"])
	}
	if data["section"] != "cells" || data["kind"] != "all" {
		t.Fatalf("section / kind 回显不符: %v / %v", data["section"], data["kind"])
	}
}

// TestAdminReleaseDiffSections 覆盖 section=rooms / buildings 的元素形状与 total 映射
// （Requirement 4.3、4.4），以及 kind 回显生效值：
//   - rooms：added / removed，元素 7 键，新增的 from_* 为 null、消失的 to_* 为 null；
//   - buildings：元素 7 键且 kind 恒为 changed，total 取 building_changed；
//   - kind 过滤只影响 total 与回显，明细由仓储按同一 kind 取页（替身原样回放）。
func TestAdminReleaseDiffSections(t *testing.T) {
	reader := &releaseAdminReaderStub{diff: releaseDiffFixture()}
	srv := newAdminReleasesServer(reader, nil)

	t.Run("rooms 默认不过滤", func(t *testing.T) {
		rec := serveAdminReleases(srv, adminReleasesRequest(
			releaseDiffPath(diffQueryFromTo()+"&section=rooms"), true))
		data := contractSuccessData(t, rec)
		assertNoStore(t, rec)

		if data["section"] != "rooms" || data["kind"] != "all" {
			t.Fatalf("section / kind 回显不符: %v / %v", data["section"], data["kind"])
		}
		if total := contractTotal(t, data["total"]); total != 3 {
			t.Fatalf("rooms 的 total 期望 added+removed=3，实际 %v", total)
		}
		items, ok := data["items"].([]any)
		if !ok || len(items) != 2 {
			t.Fatalf("rooms items 期望 2 项数组，实际 %#v", data["items"])
		}
		added := contractMap(t, "items[0]", items[0])
		assertContractKeys(t, "rooms items[0]", added, diffRoomItemKeys...)
		if added["kind"] != "added" || added["from_building"] != nil ||
			added["from_building_name"] != nil || added["to_building"] != "Q03" {
			t.Fatalf("新增房号的归属字段不符: %#v", added)
		}
		removed := contractMap(t, "items[1]", items[1])
		if removed["kind"] != "removed" || removed["to_building"] != nil ||
			removed["to_building_name"] != nil || removed["from_building"] != "Q10" {
			t.Fatalf("消失房号的归属字段不符: %#v", removed)
		}
	})

	t.Run("rooms 按 added 过滤", func(t *testing.T) {
		rec := serveAdminReleases(srv, adminReleasesRequest(
			releaseDiffPath(diffQueryFromTo()+"&section=rooms&kind=added&limit=10&offset=5"), true))
		data := contractSuccessData(t, rec)

		if data["kind"] != "added" || data["limit"] != float64(10) || data["offset"] != float64(5) {
			t.Fatalf("分页与 kind 回显不符: kind=%v limit=%v offset=%v",
				data["kind"], data["limit"], data["offset"])
		}
		if total := contractTotal(t, data["total"]); total != 2 {
			t.Fatalf("rooms kind=added 的 total 期望 2，实际 %v", total)
		}
		wantParams := store.ReleaseDiffParams{
			From: releaseOldID, To: releaseNewID,
			Section: store.DiffSectionRooms, Kind: store.DiffKindAdded, Limit: 10, Offset: 5,
		}
		if reader.lastDiffParams != wantParams {
			t.Fatalf("Diff 收到的参数不符：\n实际 %+v\n期望 %+v", reader.lastDiffParams, wantParams)
		}
	})

	t.Run("buildings", func(t *testing.T) {
		rec := serveAdminReleases(srv, adminReleasesRequest(
			releaseDiffPath(diffQueryFromTo()+"&section=buildings"), true))
		data := contractSuccessData(t, rec)

		if data["section"] != "buildings" || data["kind"] != "all" {
			t.Fatalf("section / kind 回显不符: %v / %v", data["section"], data["kind"])
		}
		if total := contractTotal(t, data["total"]); total != 3 {
			t.Fatalf("buildings 的 total 期望 building_changed=3，实际 %v", total)
		}
		items, ok := data["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("buildings items 期望 1 项数组，实际 %#v", data["items"])
		}
		item := contractMap(t, "items[0]", items[0])
		assertContractKeys(t, "buildings items[0]", item, diffBuildingItemKeys...)
		// 这一节的类别恒为 changed；仓储不存 Kind，由 DTO 补。
		if item["kind"] != "changed" || item["from_building"] != "Q10" || item["to_building"] != "Q03" {
			t.Fatalf("归属变化行不符: %#v", item)
		}
	})

	t.Run("cells 按 changed 过滤", func(t *testing.T) {
		rec := serveAdminReleases(srv, adminReleasesRequest(
			releaseDiffPath(diffQueryFromTo()+"&section=cells&kind=changed"), true))
		data := contractSuccessData(t, rec)

		if data["kind"] != "changed" {
			t.Fatalf("kind 回显期望 changed，实际 %v", data["kind"])
		}
		if total := contractTotal(t, data["total"]); total != 2048 {
			t.Fatalf("cells kind=changed 的 total 期望 2048，实际 %v", total)
		}
	})
}

// TestAdminReleaseDiffEmptyPage 覆盖「offset 翻过末页」（CP6）：
// items 是 [] 而不是 null，total 仍取汇总里的真实值。
func TestAdminReleaseDiffEmptyPage(t *testing.T) {
	fixture := releaseDiffFixture()
	fixture.Cells = []store.DiffCellRow{}
	reader := &releaseAdminReaderStub{diff: fixture}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(
		releaseDiffPath(diffQueryFromTo()+"&offset=5000"), true))
	data := contractSuccessData(t, rec)

	items, ok := data["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("翻过末页的 items 期望 []，实际 %#v", data["items"])
	}
	if total := contractTotal(t, data["total"]); total != 2203 {
		t.Fatalf("翻过末页的 total 应保持 2203，实际 %v", total)
	}
}

// TestAdminReleaseDiffUnknownRelease 覆盖对比的缺版本分支（Requirement 3.5 的仓储侧）：
// 仓储返回包着 ErrReleaseNotFound 的错误 → 404 + 40401，固定文案，不是 50000。
func TestAdminReleaseDiffUnknownRelease(t *testing.T) {
	reader := &releaseAdminReaderStub{
		diffErr: fmt.Errorf("%w: from 侧版本 %s 不存在", store.ErrReleaseNotFound, releaseOldID),
	}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), true))
	body := assertContractFailure(t, rec, http.StatusNotFound, codeReleaseNotFound)
	assertNoStore(t, rec)
	if body["message"] != "发布版本不存在" {
		t.Fatalf("缺版本文案期望固定文案，实际 %v", body["message"])
	}
}

// TestAdminAnomaliesShape 覆盖 GET /api/admin/anomalies 的成功形状（Requirement 7.1–7.4）：
// data 只有 releases，每个版本 12 个键，unknown/composite 两个键、window 的 null 与对象、
// 三个切片恒为数组、冲突房号 3 个键。
func TestAdminAnomaliesShape(t *testing.T) {
	reader := &releaseAdminReaderStub{anomalies: releaseAnomaliesFixture()}
	srv := newAdminReleasesServer(reader, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest("/api/admin/anomalies", true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "异常视图 data", data, "releases")

	releases, ok := data["releases"].([]any)
	if !ok || len(releases) != 2 {
		t.Fatalf("releases 期望 2 项数组，实际 %#v", data["releases"])
	}
	current := contractMap(t, "releases[0]", releases[0])
	assertContractKeys(t, "异常视图 releases[0]", current, anomalyKeys...)
	if current["release_id"] != releaseNewID || current["is_current"] != true {
		t.Fatalf("当前版本行不符: %#v", current)
	}
	if current["generated_at"] != "2026-11-14T03:05:12Z" {
		t.Fatalf("generated_at 期望 RFC3339 UTC，实际 %v", current["generated_at"])
	}
	unknown := contractMap(t, "unknown", current["unknown"])
	assertContractKeys(t, "unknown", unknown, anomalyStateKeys...)
	if unknown["cells"] != float64(0) || unknown["ratio"] != float64(0) {
		t.Fatalf("unknown 不符: %#v", unknown)
	}
	composite := contractMap(t, "composite", current["composite"])
	if composite["cells"] != float64(48) || composite["ratio"] != 0.0001 {
		t.Fatalf("composite 不符: %#v", composite)
	}
	if weeks, ok := current["observed_weeks"].([]any); !ok || len(weeks) != 4 {
		t.Fatalf("observed_weeks 期望 4 项数组，实际 %#v", current["observed_weeks"])
	}
	if gaps, ok := current["week_gaps"].([]any); !ok || len(gaps) != 0 {
		t.Fatalf("week_gaps 期望空数组，实际 %#v", current["week_gaps"])
	}
	window := contractMap(t, "window", current["window"])
	assertContractKeys(t, "window", window, releaseWindowKeys...)
	if window["start"] != float64(9) || window["end"] != float64(12) {
		t.Fatalf("window 不符: %#v", window)
	}
	if rooms, ok := current["conflict_rooms"].([]any); !ok || len(rooms) != 0 {
		t.Fatalf("conflict_rooms 期望空数组，实际 %#v", current["conflict_rooms"])
	}
	if current["conflict_rooms_truncated"] != false {
		t.Fatalf("conflict_rooms_truncated 期望 false，实际 %#v", current["conflict_rooms_truncated"])
	}

	previous := contractMap(t, "releases[1]", releases[1])
	assertContractKeys(t, "异常视图 releases[1]", previous, anomalyKeys...)
	// 没有可关联的发布轮次：window 为 null，而不是缺键或空对象。
	if previous["window"] != nil {
		t.Fatalf("无轮次时 window 期望 null，实际 %#v", previous["window"])
	}
	if gaps, ok := previous["week_gaps"].([]any); !ok || len(gaps) != 1 || gaps[0] != float64(7) {
		t.Fatalf("week_gaps 期望 [7]，实际 %#v", previous["week_gaps"])
	}
	conflicts, ok := previous["conflict_rooms"].([]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("conflict_rooms 期望 1 项数组，实际 %#v", previous["conflict_rooms"])
	}
	conflict := contractMap(t, "conflict_rooms[0]", conflicts[0])
	assertContractKeys(t, "conflict_rooms[0]", conflict, conflictRoomKeys...)
	if conflict["room_id"] != "0899" || conflict["building_id"] != "Q10" || conflict["conflict_building_id"] != "Q03" {
		t.Fatalf("冲突房号不符: %#v", conflict)
	}
	if previous["conflict_rooms_truncated"] != true {
		t.Fatalf("截断标记期望 true，实际 %#v", previous["conflict_rooms_truncated"])
	}
}

// TestAdminAnomaliesEmpty 覆盖空库（Requirement 7.5）：releases 是 []，不是 null 也不是错误。
func TestAdminAnomaliesEmpty(t *testing.T) {
	srv := newAdminReleasesServer(&releaseAdminReaderStub{}, nil)

	rec := serveAdminReleases(srv, adminReleasesRequest("/api/admin/anomalies", true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)

	if releases, ok := data["releases"].([]any); !ok || len(releases) != 0 {
		t.Fatalf("空库 releases 期望 []，实际 %#v", data["releases"])
	}
}

// TestAdminReleaseRollbackSuccess 覆盖回退成功（Requirement 5.1、6.1）：
// 200 + data 3 个键、路径参数原样传给仓储、没有请求体也不要求 Content-Type、
// 缓存失效回调恰好调用一次（CP10）。
func TestAdminReleaseRollbackSuccess(t *testing.T) {
	result := store.RollbackResult{
		RolledBackFrom:   releaseNewID,
		CurrentReleaseID: releaseOldID,
		Term:             releaseTerm,
	}
	reader := &releaseAdminReaderStub{rollback: result}
	invalidateCalls := 0
	srv := newAdminReleasesServer(reader, func() { invalidateCalls++ })

	// 请求没有请求体、没有 Content-Type：adminWriteNoBody 不该把它拒掉。
	rec := serveAdminReleases(srv, adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, true))
	data := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	assertContractKeys(t, "回退 data", data, rollbackKeys...)

	if data["rolled_back_from"] != releaseNewID ||
		data["current_release_id"] != releaseOldID ||
		data["term"] != releaseTerm {
		t.Fatalf("回退结果不符: %#v", data)
	}
	if reader.rollbackCalls != 1 || reader.lastRollbackID != releaseOldID {
		t.Fatalf("Rollback 应收到目标 %s，实际 calls=%d id=%q",
			releaseOldID, reader.rollbackCalls, reader.lastRollbackID)
	}
	if invalidateCalls != 1 {
		t.Fatalf("回退成功后缓存失效应恰好调用一次，实际 %d 次", invalidateCalls)
	}

	t.Run("invalidate 为 nil 时回退照常成功", func(t *testing.T) {
		reader := &releaseAdminReaderStub{rollback: result}
		srv := newAdminReleasesServer(reader, nil)

		rec := serveAdminReleases(srv, adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, true))
		contractSuccessData(t, rec)
		assertNoStore(t, rec)
	})
}

// TestAdminReleaseRollbackFailures 覆盖回退失败的五条出口（Requirement 5.2–5.6）：
// 40401 / 40904 / 40905 / 40906 / 50000，且失败路径的缓存失效零调用（CP9、CP10）。
//
// 40904 / 40905 的文案直接取仓储错误里的具体原因（design「Error Handling」要求点名目标
// 版本与两侧学期）；40906 必须用固定文案，不能回显包在里面的 pg 唯一索引名。
//
// 另有一条「裸哨兵」用例：错误就是哨兵本身、没有「: 」包裹的具体原因时，业务码不变，文案
// 退回哨兵自身的文本。这是 releaseRollbackReason 的唯一退路，也是 store 将来改了包装格式
// 后实际会走的分支——不锁住它，改动会以「细节静默消失」的方式通过测试。
func TestAdminReleaseRollbackFailures(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		status      int
		code        int
		wantMessage []string
		// exactMessage 非空时要求 message 与它完全相等，而不只是包含若干片段：
		// 用来锁「退回哨兵文案」时不会额外拼接细节或固定文案。
		exactMessage string
	}{
		{
			name:        "目标版本不存在",
			err:         fmt.Errorf("%w: 回退目标版本 %s 不存在", store.ErrReleaseNotFound, releaseOldID),
			status:      http.StatusNotFound,
			code:        codeReleaseNotFound,
			wantMessage: []string{"发布版本不存在"},
		},
		{
			name: "目标已是当前版本",
			err: fmt.Errorf("%w: 目标版本 %s 已是当前版本，不能回退",
				store.ErrReleaseNotRollbackable, releaseOldID),
			status:      http.StatusConflict,
			code:        codeReleaseNotRollback,
			wantMessage: []string{releaseOldID, "已是当前版本"},
		},
		{
			name: "没有当前版本",
			err: fmt.Errorf("%w: 库中没有当前版本，版本 %s 不能回退",
				store.ErrReleaseNotRollbackable, releaseOldID),
			status:      http.StatusConflict,
			code:        codeReleaseNotRollback,
			wantMessage: []string{releaseOldID, "没有当前版本"},
		},
		{
			name: "跨学期",
			err: fmt.Errorf("%w: 目标版本属于学期 2025-2026-2，当前版本属于学期 2026-2027-1，不能跨学期回退",
				store.ErrReleaseTermMismatch),
			status:      http.StatusConflict,
			code:        codeReleaseTermMismatch,
			wantMessage: []string{"2025-2026-2", "2026-2027-1"},
		},
		{
			name: "并发冲突",
			err: fmt.Errorf("%w: 抬目标版本为当前失败: %w", store.ErrReleaseConflict,
				errors.New(`duplicate key value violates unique constraint "release_one_current"`)),
			status:      http.StatusConflict,
			code:        codeReleaseConflict,
			wantMessage: []string{"并发冲突", "请重试"},
		},
		{
			// 裸哨兵：err 就是哨兵本身，err.Error() 里没有「: 」。替身或将来 store 换了
			// 包装格式时（前缀不再匹配）走的正是这条分支。
			name:         "裸哨兵退回哨兵文案",
			err:          store.ErrReleaseNotRollbackable,
			status:       http.StatusConflict,
			code:         codeReleaseNotRollback,
			wantMessage:  []string{"版本不可回退"},
			exactMessage: store.ErrReleaseNotRollbackable.Error(),
		},
		{
			name:        "其它事务失败",
			err:         fmt.Errorf("%w: 提交回退事务失败: %w", model.ErrQueryFailed, errors.New("db down")),
			status:      http.StatusInternalServerError,
			code:        codeInternal,
			wantMessage: []string{"内部错误"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			reader := &releaseAdminReaderStub{rollbackErr: tc.err}
			invalidateCalls := 0
			srv := newAdminReleasesServerWithLogger(
				slog.New(slog.NewJSONHandler(&logBuf, nil)),
				reader,
				func() { invalidateCalls++ },
			)

			rec := serveAdminReleases(srv, adminReleaseRollbackRequest(
				releaseRollbackPath(releaseOldID), true, true))
			body := assertContractFailure(t, rec, tc.status, tc.code)
			assertNoStore(t, rec)

			message, ok := body["message"].(string)
			if !ok {
				t.Fatalf("失败响应 message 不是字符串: %#v", body["message"])
			}
			for _, want := range tc.wantMessage {
				if !strings.Contains(message, want) {
					t.Fatalf("失败文案 %q 应包含 %q", message, want)
				}
			}
			if tc.exactMessage != "" && message != tc.exactMessage {
				t.Fatalf("失败文案期望恰好是哨兵文案 %q，实际 %q", tc.exactMessage, message)
			}
			// 40906 的原文包着 pg 错误，不能顺着 message 泄露出去。
			if strings.Contains(rec.Body.String(), "release_one_current") ||
				strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("响应泄露了数据库细节: %s", rec.Body.String())
			}
			if invalidateCalls != 0 {
				t.Fatalf("回退失败时缓存失效应零调用，实际 %d 次", invalidateCalls)
			}
			// 4xx 是客户端可理解的状态，不该刷 ERROR 日志；50000 由下方白名单用例单独锁。
			if tc.status < http.StatusInternalServerError && logBuf.Len() != 0 {
				t.Fatalf("4xx 不应记日志，实际：%s", logBuf.String())
			}
		})
	}
}

// TestAdminReleaseDiffParamGate 覆盖对比的参数门（Requirement 3.5、4.5、CP12、CP14）：
// 任一非法参数都是 400 + 40001，且仓储零调用——参数门必须在查库之前。
func TestAdminReleaseDiffParamGate(t *testing.T) {
	valid := diffQueryFromTo()

	cases := []struct {
		name  string
		query string
	}{
		{"缺 from", "?to=" + releaseNewID},
		{"缺 to", "?from=" + releaseOldID},
		{"from 与 to 都缺", ""},
		{"from 形状非法", "?from=2026-10-28&to=" + releaseNewID},
		{"from 日期不存在", "?from=20261314T030512Z&to=" + releaseNewID},
		{"to 形状非法", "?from=" + releaseOldID + "&to=20261028T041500"},
		{"from 与 to 相同", "?from=" + releaseOldID + "&to=" + releaseOldID},
		{"section 非法", valid + "&section=weeks"},
		{"section 大小写不符", valid + "&section=CELLS"},
		{"kind 取值非法", valid + "&kind=same"},
		{"kind 大小写不符", valid + "&kind=ALL"},
		{"cells 不接受 shared", valid + "&kind=shared"},
		{"rooms 不接受 changed", valid + "&section=rooms&kind=changed"},
		{"buildings 不接受 added", valid + "&section=buildings&kind=added"},
		{"limit 为 0", valid + "&limit=0"},
		{"limit 越过上界", valid + "&limit=201"},
		{"limit 非整数", valid + "&limit=abc"},
		{"offset 为负", valid + "&offset=-1"},
		{"offset 非整数", valid + "&offset=abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &releaseAdminReaderStub{diff: releaseDiffFixture()}
			srv := newAdminReleasesServer(reader, nil)

			rec := serveAdminReleases(srv, adminReleasesRequest(releaseDiffPath(tc.query), true))
			assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			if reader.calls() != 0 {
				t.Fatalf("非法参数不该查库，实际调用 %d 次", reader.calls())
			}
		})
	}
}

// TestAdminReleaseParamGate 覆盖详情与回退的 release_id 形状门（Requirement 2.5、CP14）：
// 形状不合法一律 400 + 40001，且仓储零调用。
func TestAdminReleaseParamGate(t *testing.T) {
	badIDs := []string{
		"2026-10-28",
		"20261028T041500",
		"20261314T030512Z",
		"20261028T041500z",
		"20261028T041500Z0",
	}

	for _, badID := range badIDs {
		t.Run("详情 "+badID, func(t *testing.T) {
			reader := &releaseAdminReaderStub{detailFound: true, detail: releaseDetailFixture()}
			srv := newAdminReleasesServer(reader, nil)

			rec := serveAdminReleases(srv, adminReleasesRequest(releaseIDPath(badID), true))
			assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			if reader.calls() != 0 {
				t.Fatalf("非法 release_id 不该查库，实际调用 %d 次", reader.calls())
			}
		})

		t.Run("回退 "+badID, func(t *testing.T) {
			reader := &releaseAdminReaderStub{}
			invalidateCalls := 0
			srv := newAdminReleasesServer(reader, func() { invalidateCalls++ })

			rec := serveAdminReleases(srv, adminReleaseRollbackRequest(
				releaseRollbackPath(badID), true, true))
			assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			if reader.calls() != 0 {
				t.Fatalf("非法 release_id 不该查库，实际调用 %d 次", reader.calls())
			}
			if invalidateCalls != 0 {
				t.Fatalf("非法参数时缓存失效应零调用，实际 %d 次", invalidateCalls)
			}
		})
	}
}

// TestAdminReleaseEndpointsRequireSession 覆盖无会话（Requirement 8.1、CP12）：
// 五个端点都被 adminRead / adminWriteNoBody 的 withSession 挡在业务之前，
// 仓储零调用，响应 401 + 40101 且带 no-store。
func TestAdminReleaseEndpointsRequireSession(t *testing.T) {
	type target struct {
		name string
		req  *http.Request
	}
	targets := []target{
		{"列表", adminReleasesRequest("/api/admin/releases", false)},
		{"对比", adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), false)},
		{"详情", adminReleasesRequest(releaseIDPath(releaseNewID), false)},
		{"回退", adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), false, true)},
		{"异常视图", adminReleasesRequest("/api/admin/anomalies", false)},
	}

	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			reader := &releaseAdminReaderStub{
				releases:    []store.ReleaseSummary{currentReleaseSummary()},
				detail:      releaseDetailFixture(),
				detailFound: true,
				diff:        releaseDiffFixture(),
				anomalies:   releaseAnomaliesFixture(),
				rollback:    store.RollbackResult{CurrentReleaseID: releaseOldID},
			}
			invalidateCalls := 0
			srv := newAdminReleasesServer(reader, func() { invalidateCalls++ })

			rec := serveAdminReleases(srv, tc.req)
			assertContractFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
			assertNoStore(t, rec)
			if reader.calls() != 0 {
				t.Fatalf("无会话不该查库，实际调用 %d 次", reader.calls())
			}
			if invalidateCalls != 0 {
				t.Fatalf("无会话时缓存失效应零调用，实际 %d 次", invalidateCalls)
			}
		})
	}
}

// TestAdminReleaseRollbackRequiresCSRF 覆盖回退缺 CSRF（Requirement 8.1、CP12）：
// 有有效会话但缺令牌或令牌不符 → 403 + 40301，回退事务零调用、缓存失效零调用。
// 请求体为空不是拒绝原因：回退本来就不读请求体。
func TestAdminReleaseRollbackRequiresCSRF(t *testing.T) {
	cases := []struct {
		name string
		csrf bool
	}{
		{"缺 CSRF 令牌", false},
		{"CSRF 令牌不符", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &releaseAdminReaderStub{}
			invalidateCalls := 0
			srv := newAdminReleasesServer(reader, func() { invalidateCalls++ })

			req := adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, tc.csrf)
			if tc.name == "CSRF 令牌不符" {
				req.Header.Set(adminCSRFHeader, "wrong-csrf-token")
			}
			rec := serveAdminReleases(srv, req)

			assertContractFailure(t, rec, http.StatusForbidden, codeCSRFInvalid)
			assertNoStore(t, rec)
			if reader.rollbackCalls != 0 {
				t.Fatalf("CSRF 失败时回退事务应零调用，实际 %d 次", reader.rollbackCalls)
			}
			if invalidateCalls != 0 {
				t.Fatalf("CSRF 失败时缓存失效应零调用，实际 %d 次", invalidateCalls)
			}
		})
	}
}

// TestAdminReleaseEndpointsWithoutReader 覆盖装配缺失（Requirement 8.4、CP15）：
// WithReleaseAdmin 没被调用时五个端点仍注册，按 500 + 50000 返回，不回落 404。
// 装配检查排在参数门之前：带非法参数的对比请求也报 50000，而不是 40001。
func TestAdminReleaseEndpointsWithoutReader(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
	}{
		{"列表", adminReleasesRequest("/api/admin/releases", true)},
		{"对比", adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), true)},
		{"对比（参数也非法）", adminReleasesRequest(releaseDiffPath("?from=bad"), true)},
		{"详情", adminReleasesRequest(releaseIDPath(releaseNewID), true)},
		{"回退", adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, true)},
		{"异常视图", adminReleasesRequest("/api/admin/anomalies", true)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAdminReleasesServerWithoutReader()
			rec := serveAdminReleases(srv, tc.req)

			assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
		})
	}
}

// TestAdminReleaseReadFailures 覆盖读端点的仓储失败（design「查询/统计失败 → 50000」）：
// 四个读端点都返回 500 + 50000，响应文案固定「内部错误」，不出现错误原文。
func TestAdminReleaseReadFailures(t *testing.T) {
	dbDown := errors.New("db down")
	cases := []struct {
		name   string
		reader *releaseAdminReaderStub
		req    *http.Request
	}{
		{"列表", &releaseAdminReaderStub{listErr: dbDown}, adminReleasesRequest("/api/admin/releases", true)},
		{"详情", &releaseAdminReaderStub{detailErr: dbDown}, adminReleasesRequest(releaseIDPath(releaseNewID), true)},
		{"对比", &releaseAdminReaderStub{diffErr: dbDown}, adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), true)},
		{"异常视图", &releaseAdminReaderStub{anomaliesErr: dbDown}, adminReleasesRequest("/api/admin/anomalies", true)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAdminReleasesServer(tc.reader, nil)
			rec := serveAdminReleases(srv, tc.req)

			body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
			if body["message"] != "内部错误" {
				t.Fatalf("50000 文案期望固定「内部错误」，实际 %v", body["message"])
			}
			if strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("响应泄露了错误原文: %s", rec.Body.String())
			}
		})
	}
}

// TestAdminReleaseFailureLogWhitelist 覆盖回退 500 的日志白名单（Requirement 8.3、CP13）：
// 一条失败日志只有 request_id / 方法 / 路径 / 状态码 / 业务码五个业务字段，
// request_id 与响应里那个一致；错误原文与 SQL 细节都不进日志。
func TestAdminReleaseFailureLogWhitelist(t *testing.T) {
	var logBuf bytes.Buffer
	reader := &releaseAdminReaderStub{
		rollbackErr: fmt.Errorf("%w: 提交回退事务失败: %w",
			model.ErrQueryFailed, errors.New("db down: relation release")),
	}
	srv := newAdminReleasesServerWithLogger(
		slog.New(slog.NewJSONHandler(&logBuf, nil)), reader, nil)

	rec := serveAdminReleases(srv, adminReleaseRollbackRequest(
		releaseRollbackPath(releaseOldID), true, true))
	body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)

	entry := singleAdminLogEntry(t, &logBuf)
	if entry.Level != "ERROR" {
		t.Fatalf("回退失败应记 ERROR，实际 %s", entry.Level)
	}
	if entry.RequestID != contractRequestID(t, rec) {
		t.Fatalf("日志 request_id %q 与响应 %q 不一致",
			entry.RequestID, contractRequestID(t, rec))
	}
	if entry.Method != http.MethodPost ||
		entry.Path != releaseRollbackPath(releaseOldID) ||
		entry.Status != http.StatusInternalServerError ||
		entry.Code != codeInternal {
		t.Fatalf("日志字段不符: %+v", entry)
	}
	if body["message"] != "内部错误" {
		t.Fatalf("50000 文案不符: %v", body["message"])
	}

	// 日志字段白名单：slog 自带 level/msg/time，业务字段只有五项规定项。
	var raw map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &raw); err != nil {
		t.Fatalf("日志不是合法 JSON: %v，%s", err, logBuf.String())
	}
	allowed := map[string]bool{
		"level": true, "msg": true, "time": true,
		"request_id": true, "method": true, "path": true, "status": true, "code": true,
	}
	for key := range raw {
		if !allowed[key] {
			t.Fatalf("日志出现白名单外字段 %q：%s", key, logBuf.String())
		}
	}
	// 错误原文与 SQL 细节不进日志；路径里的版本 id 属于 path 字段本身，不算泄露。
	for _, forbidden := range []string{"db down", "relation release"} {
		if strings.Contains(logBuf.String(), forbidden) {
			t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
		}
	}
}

// TestAdminReleaseResponsesKeepNoStore 把五个端点的成功响应与各类错误响应集中扫一遍
// （Requirement 8.2）：no-store 由最外层 withAdminNoStore 按前缀兜底，这条用例防止
// 将来调整中间件链或路由注册时漏掉某条出口。
func TestAdminReleaseResponsesKeepNoStore(t *testing.T) {
	fullReader := func() *releaseAdminReaderStub {
		return &releaseAdminReaderStub{
			releases:    []store.ReleaseSummary{currentReleaseSummary()},
			detail:      releaseDetailFixture(),
			detailFound: true,
			diff:        releaseDiffFixture(),
			anomalies:   releaseAnomaliesFixture(),
			rollback: store.RollbackResult{
				RolledBackFrom: releaseNewID, CurrentReleaseID: releaseOldID, Term: releaseTerm,
			},
		}
	}

	cases := []struct {
		name   string
		reader *releaseAdminReaderStub
		srv    *Server
		req    *http.Request
		status int
	}{
		{"列表成功", fullReader(), nil, adminReleasesRequest("/api/admin/releases", true), http.StatusOK},
		{"对比成功", fullReader(), nil, adminReleasesRequest(releaseDiffPath(diffQueryFromTo()), true), http.StatusOK},
		{"详情成功", fullReader(), nil, adminReleasesRequest(releaseIDPath(releaseNewID), true), http.StatusOK},
		{"回退成功", fullReader(), nil, adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, true), http.StatusOK},
		{"异常视图成功", fullReader(), nil, adminReleasesRequest("/api/admin/anomalies", true), http.StatusOK},
		{
			"参数非法", &releaseAdminReaderStub{}, nil,
			adminReleasesRequest(releaseIDPath("2026-10-28"), true), http.StatusBadRequest,
		},
		{
			"无会话", &releaseAdminReaderStub{}, nil,
			adminReleasesRequest("/api/admin/releases", false), http.StatusUnauthorized,
		},
		{
			"缺 CSRF", &releaseAdminReaderStub{}, nil,
			adminReleaseRollbackRequest(releaseRollbackPath(releaseOldID), true, false), http.StatusForbidden,
		},
		{
			"未知版本", &releaseAdminReaderStub{}, nil,
			adminReleasesRequest(releaseIDPath(releaseNewID), true), http.StatusNotFound,
		},
		{
			"装配缺失", &releaseAdminReaderStub{}, newAdminReleasesServerWithoutReader(),
			adminReleasesRequest("/api/admin/releases", true), http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.srv
			if srv == nil {
				srv = newAdminReleasesServer(tc.reader, nil)
			}
			rec := serveAdminReleases(srv, tc.req)
			if rec.Code != tc.status {
				t.Fatalf("期望 HTTP %d，实际 %d，body=%s", tc.status, rec.Code, rec.Body.String())
			}
			assertNoStore(t, rec)
		})
	}
}
