package api

// 本文件是轮次读取端点的契约测试（tasklist 第 10 项）：
//
//	GET /api/admin/runs            {runs,total,limit,offset} 的键集合、22 键摘要、
//	                               分页默认值与边界、空表 []、参数越界 40001、no-store
//	GET /api/admin/runs/{run_id}   详情 {run,tasks,weeks} 的形状、blocked 轮的 term=null、
//	                               未知 id 40401、非正整数 run_id 40001、no-store
//
// 数据来源用替身 RunReader：本项是一段纯粹的「列 → JSON 键」映射，仓储的 SQL、
// 扫描顺序与空切片语义由 store 的单测/集成测试锁住（tasklist 第 6 项）。这里要测的
// 是 HTTP 契约本身——状态码、业务码、键集合、可空列的 null、时间格式与错误映射。
// 请求一律经过 s.Handler()，中间件链（no-store、会话）与路由注册都在测。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// runCST 是一个非 UTC 的固定时区，专门用来验证 .UTC() 归一化真的发生。
//
// 只给 UTC 的夹具测不出 formatRunTime / formatRunTimePtr 里的 UTC()：即便删掉它，
// 输出也还是 ...Z。这里用 20:00 CST（等于 12:00Z）构造 StartedAt 与 FetchedAt，
// 断言仍写 12:00:00Z / 12:01:02Z——转换一旦缺失，JSON 会变成 +08:00 偏移，用例立刻红。
var runCST = time.FixedZone("CST", 8*3600)

// 轮次测试用的固定时刻，秒级对齐，断言文里就能直接看出 RFC3339 的形状；
// 时间本身与真实部署无关，只用来锁「UTC 化 + 格式化」这一层。
var (
	runStartedAt  = time.Date(2026, 11, 8, 20, 0, 0, 0, runCST) // 12:00Z
	runUpdatedAt  = time.Date(2026, 11, 8, 12, 3, 11, 0, time.UTC)
	runBlockedAt  = time.Date(2026, 11, 8, 11, 59, 0, 0, time.UTC)
	runFinishedAt = time.Date(2026, 11, 8, 12, 5, 0, 0, time.UTC)
	runReleasedAt = time.Date(2026, 11, 8, 12, 6, 0, 0, time.UTC)
	runFetchedAt  = time.Date(2026, 11, 8, 20, 1, 2, 0, runCST) // 12:01:02Z
)

// 三个夹具的轮次编号。夹具不再接收 runID 参数：每条用例只需要一种轮次形态，
// 编号写进常量后，请求路径、byID 键与断言里的 run_id 默认都指向同一个值，不会出现
// 「路径改了、byID 键没改」的漂移。唯一例外是 TestAdminRunDetailShape：它故意让
// 路径与 byID 键（14）不同于摘要里的 run_id（12），锁住「分发给 tasks/weeks 的编号
// 取自路径参数」。
const (
	runningRunID  = 14
	blockedRunID  = 13
	finishedRunID = 12
)

// runReaderStub 是 RunReader 的内存替身：回放预置数据，记录调用次数与入参。
//
// 四项错误分别可注入，用来断言「某一层失败 → 50000」以及「未知 id 不查 tasks/weeks」。
// 调用计数是这些断言唯一的证据：响应形状相同，只有「仓储到底被调了几次、传了什么」
// 能区分「参数门在查库之前」与「查完才发现参数不对」。
//
// ByID/TaskSummary/Weeks 记编号，Recent 记分页入参：详情 handler 要把同一个路径参数
// 分发给三次查询，如果替身吞掉入参，传错编号的 bug 在响应上完全看不出来（byID 命中后
// tasks/weeks 回放的数据与编号无关）。
type runReaderStub struct {
	summaries []store.RunSummary
	total     int
	byID      map[int64]store.RunSummary
	weeks     []store.RunWeekResult
	tasks     store.RunTaskSummary

	recentErr error
	byIDErr   error
	weeksErr  error
	tasksErr  error

	recentCalls   int
	lastLimit     int
	lastOffset    int
	byIDCalls     int
	lastByIDRunID int64
	tasksCalls    int
	lastTaskRunID int64
	weeksCalls    int
	lastWeekRunID int64
}

// Recent 回放一页摘要与总数，并记下列表入参。
func (s *runReaderStub) Recent(_ context.Context, limit, offset int) ([]store.RunSummary, int, error) {
	s.recentCalls++
	s.lastLimit, s.lastOffset = limit, offset
	if s.recentErr != nil {
		return nil, 0, s.recentErr
	}
	return s.summaries, s.total, nil
}

// ByID 按编号回放摘要；不在 byID 里的编号返回 found=false、error=nil（与真实仓储一致）。
func (s *runReaderStub) ByID(_ context.Context, runID int64) (store.RunSummary, bool, error) {
	s.byIDCalls++
	s.lastByIDRunID = runID
	if s.byIDErr != nil {
		return store.RunSummary{}, false, s.byIDErr
	}
	summary, found := s.byID[runID]
	return summary, found, nil
}

// TaskSummary 回放任务汇总，并记下收到的轮次编号。
func (s *runReaderStub) TaskSummary(_ context.Context, runID int64) (store.RunTaskSummary, error) {
	s.tasksCalls++
	s.lastTaskRunID = runID
	if s.tasksErr != nil {
		return store.RunTaskSummary{}, s.tasksErr
	}
	return s.tasks, nil
}

// Weeks 回放逐周结果，并记下收到的轮次编号。
func (s *runReaderStub) Weeks(_ context.Context, runID int64) ([]store.RunWeekResult, error) {
	s.weeksCalls++
	s.lastWeekRunID = runID
	if s.weeksErr != nil {
		return nil, s.weeksErr
	}
	return s.weeks, nil
}

// runningRunSummary 造一轮进行中的摘要：已进入窗口、有计数、未完成、未发布、无错误。
func runningRunSummary() store.RunSummary {
	term := "2026-2027-1"
	windowStart := 9
	windowEnd := 12
	return store.RunSummary{
		RunID:     runningRunID,
		Status:    "running",
		Phase:     "week_matrix",
		StartedAt: runStartedAt,
		UpdatedAt: runUpdatedAt,
		Term:      &term,
		// 窗口是从 1 起的教学周，小整数用 short 落库；扫描后是 int。
		WindowStart:   &windowStart,
		WindowEnd:     &windowEnd,
		WeeksTotal:    4,
		RequiredTotal: 8,
		RequiredDone:  3,
		OptionalTotal: 5,
		OptionalDone:  1,
	}
}

// blockedRunSummary 造一轮「白名单为空、建立即终态」的摘要：
// term / 窗口 / 计数 / 版本全部为空，三个时刻相同（Requirement 7.2、8.4）。
func blockedRunSummary() store.RunSummary {
	code := "whitelist_empty"
	message := "教学楼白名单为空，未发起采集"
	return store.RunSummary{
		RunID:        blockedRunID,
		Status:       "blocked",
		Phase:        "finished",
		StartedAt:    runBlockedAt,
		FinishedAt:   &runBlockedAt,
		UpdatedAt:    runBlockedAt,
		ErrorCode:    &code,
		ErrorMessage: &message,
	}
}

// finishedRunSummary 造一轮已发布（partial）的摘要：覆盖 22 键里所有「有值」的分支，
// 与 running / blocked 两个「部分为空」的夹具合起来，把每个可空列的 null 与非 null
// 都至少走到一次。
func finishedRunSummary() store.RunSummary {
	term := "2026-2027-1"
	windowStart := 9
	windowEnd := 12
	releaseID := "rel_20261108_01"
	return store.RunSummary{
		RunID:             finishedRunID,
		Status:            "partial",
		Phase:             "publish",
		StartedAt:         runStartedAt,
		FinishedAt:        &runFinishedAt,
		UpdatedAt:         runFinishedAt,
		Term:              &term,
		WindowStart:       &windowStart,
		WindowEnd:         &windowEnd,
		WeeksTotal:        4,
		RoomsTotal:        120,
		RequiredTotal:     8,
		RequiredDone:      7,
		OptionalTotal:     5,
		OptionalDone:      4,
		UnknownCells:      3,
		CompositeCells:    2,
		AttrConflictCount: 1,
		ReleaseID:         &releaseID,
		ReleasedAt:        &runReleasedAt,
	}
}

// newAdminRunsServer 组装装了会话来源与轮次读取替身的 Server。
func newAdminRunsServer(reader RunReader) *Server {
	srv := New(quietLogger(), nil, nil, nil, nil,
		WithAdmin(activeAdminStore(), nil),
		WithRunStatus(reader),
	)
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// newAdminRunsServerWithoutReader 组装一个「装配了会话、没装配轮次读取」的 Server。
// 时钟必须拨到 testAdminNow：替身会话以它为基准签发，用真实时间会先撞上 40101，
// 测不到「装配缺失 → 50000」。
func newAdminRunsServerWithoutReader() *Server {
	srv := New(quietLogger(), nil, nil, nil, nil, WithAdmin(activeAdminStore(), nil))
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// adminRunsRequest 构造带有效会话 Cookie 的 GET 请求。
func adminRunsRequest(target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: testAdminRawToken})
	return req
}

// serveAdminRuns 经完整的 Handler 跑一个请求（路由 + 中间件 + handler）。
func serveAdminRuns(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// runSummaryKeys 是列表元素与详情 run 块共用的 22 个键（Requirement 7.2、CP18）。
var runSummaryKeys = []string{
	"run_id", "status", "phase", "started_at", "finished_at", "updated_at",
	"term", "window_start", "window_end", "weeks_total", "rooms_total",
	"required_total", "required_done", "optional_total", "optional_done",
	"unknown_cells", "composite_cells", "attr_conflict_count",
	"release_id", "released_at", "error_code", "error_message",
}

// runTasksKeys 是详情 tasks 块的 7 个键（design 的详情响应示例）。
var runTasksKeys = []string{
	"required_total", "required_done", "required_failed",
	"optional_total", "optional_done", "optional_failed",
	"first_failure_message",
}

// runWeekKeys 是详情 weeks 元素的 5 个键（Requirement 7.4）。
var runWeekKeys = []string{"week", "building_id", "ok", "fetched_at", "error_message"}

// contractMap 把 map[string]any 里的某一项断言成对象；形状不符直接终止用例。
func contractMap(t *testing.T, what string, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s 不是对象: %#v", what, value)
	}
	return object
}

// contractSlice 把 map[string]any 里的某一项断言成数组；形状不符直接终止用例。
// JSON 的 null 解出来是 nil 接口，类型断言会失败，所以它同时能抓「空列表写成 null」。
func contractSlice(t *testing.T, what string, value any) []any {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s 不是数组: %#v", what, value)
	}
	return items
}

// TestAdminRunsListShape 覆盖 GET /api/admin/runs 的成功形状（Requirement 7.1、7.2、7.6）：
// data 恰好 4 个键、每条摘要恰好 22 个键、可空列在 running / blocked 两种轮次上的 null
// 与非 null、时间 UTC RFC3339、分页参数回显与透传、仓储返回的顺序原样输出。
func TestAdminRunsListShape(t *testing.T) {
	// total 故意取 42：它与三条夹具的 run_id（12/13/14）都不同值，页里只有 2 条却报 42
	// 也符合「还有下一页」的真实形态。若 total 与某个 run_id 同值，把 data["total"] 误写成
	// 某条摘要的 run_id 时断言仍会通过。
	const listTotal = 42
	reader := &runReaderStub{
		summaries: []store.RunSummary{runningRunSummary(), blockedRunSummary()},
		total:     listTotal,
	}
	srv := newAdminRunsServer(reader)

	rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs?limit=2"))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "runs data", data, "runs", "total", "limit", "offset")
	assertNoStore(t, rec)

	if data["total"] != float64(listTotal) || data["limit"] != float64(2) || data["offset"] != float64(0) {
		t.Fatalf("分页信息不符：total=%#v limit=%#v offset=%#v",
			data["total"], data["limit"], data["offset"])
	}
	if reader.recentCalls != 1 || reader.lastLimit != 2 || reader.lastOffset != 0 {
		t.Fatalf("列表入参不符：calls=%d limit=%d offset=%d",
			reader.recentCalls, reader.lastLimit, reader.lastOffset)
	}

	runs := contractSlice(t, "runs", data["runs"])
	if len(runs) != 2 {
		t.Fatalf("runs 应有 2 条，实际 %d 条", len(runs))
	}

	running := contractMap(t, "runs[0]", runs[0])
	assertContractKeys(t, "runs[0]", running, runSummaryKeys...)

	wantRunning := map[string]any{
		"run_id":              float64(runningRunID),
		"status":              "running",
		"phase":               "week_matrix",
		"started_at":          "2026-11-08T12:00:00Z",
		"finished_at":         nil,
		"updated_at":          "2026-11-08T12:03:11Z",
		"term":                "2026-2027-1",
		"window_start":        float64(9),
		"window_end":          float64(12),
		"weeks_total":         float64(4),
		"rooms_total":         float64(0),
		"required_total":      float64(8),
		"required_done":       float64(3),
		"optional_total":      float64(5),
		"optional_done":       float64(1),
		"unknown_cells":       float64(0),
		"composite_cells":     float64(0),
		"attr_conflict_count": float64(0),
		"release_id":          nil,
		"released_at":         nil,
		"error_code":          nil,
		"error_message":       nil,
	}
	for key, want := range wantRunning {
		if running[key] != want {
			t.Fatalf("runs[0].%s 期望 %#v，实际 %#v", key, want, running[key])
		}
	}
	// running 轮次还没结束：finished_at 必须是 null，不能是零值时间。
	if !strings.Contains(rec.Body.String(), `"finished_at":null`) {
		t.Fatalf("running 轮的 finished_at 应序列化为 null，实际 body=%s", rec.Body.String())
	}

	blocked := contractMap(t, "runs[1]", runs[1])
	assertContractKeys(t, "runs[1]", blocked, runSummaryKeys...)

	wantBlocked := map[string]any{
		"run_id":              float64(blockedRunID),
		"status":              "blocked",
		"phase":               "finished",
		"started_at":          "2026-11-08T11:59:00Z",
		"finished_at":         "2026-11-08T11:59:00Z",
		"updated_at":          "2026-11-08T11:59:00Z",
		"term":                nil,
		"window_start":        nil,
		"window_end":          nil,
		"weeks_total":         float64(0),
		"rooms_total":         float64(0),
		"required_total":      float64(0),
		"required_done":       float64(0),
		"optional_total":      float64(0),
		"optional_done":       float64(0),
		"unknown_cells":       float64(0),
		"composite_cells":     float64(0),
		"attr_conflict_count": float64(0),
		"release_id":          nil,
		"released_at":         nil,
		"error_code":          "whitelist_empty",
		"error_message":       "教学楼白名单为空，未发起采集",
	}
	for key, want := range wantBlocked {
		if blocked[key] != want {
			t.Fatalf("runs[1].%s 期望 %#v，实际 %#v", key, want, blocked[key])
		}
	}
	// blocked 轮没有学期：term 必须是 null，不能被折成空串。
	if !strings.Contains(rec.Body.String(), `"term":null`) {
		t.Fatalf("blocked 轮的 term 应序列化为 null，实际 body=%s", rec.Body.String())
	}
	// 时间夹具里的 StartedAt 来自 +08:00 的 CST：整个 body 不该出现偏移写法。
	if strings.Contains(rec.Body.String(), "+08:00") {
		t.Fatalf("时间应归一到 UTC，body 里不该出现 +08:00，实际 body=%s", rec.Body.String())
	}
}

// TestAdminRunsListPagination 覆盖分页的默认值与闭区间边界（Requirement 7.1）：
// 省略 limit / offset 时用 20 / 0；limit=1 与 limit=100 是合法边界；显式值原样透传
// 给仓储并回显在 data 里（前端据此算下一屏）。
func TestAdminRunsListPagination(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"全部省略用默认值", "", 20, 0},
		{"只给 limit", "?limit=5", 5, 0},
		{"只给 offset", "?offset=3", 20, 3},
		{"下界 limit=1", "?limit=1", 1, 0},
		{"上界 limit=100", "?limit=100", 100, 0},
		{"显式两参", "?limit=100&offset=7", 100, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &runReaderStub{
				summaries: []store.RunSummary{runningRunSummary()},
				total:     1,
			}
			srv := newAdminRunsServer(reader)

			rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs"+tc.query))

			data := contractSuccessData(t, rec)
			assertNoStore(t, rec)

			if reader.lastLimit != tc.wantLimit || reader.lastOffset != tc.wantOffset {
				t.Fatalf("传给仓储的分页不符：limit=%d offset=%d，期望 %d/%d",
					reader.lastLimit, reader.lastOffset, tc.wantLimit, tc.wantOffset)
			}
			if data["limit"] != float64(tc.wantLimit) || data["offset"] != float64(tc.wantOffset) {
				t.Fatalf("回显的分页不符：limit=%#v offset=%#v，期望 %d/%d",
					data["limit"], data["offset"], tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// TestAdminRunsListEmpty 覆盖「一条轮次都没有」（Requirement 7.6）：200 + code 0，
// runs 是 [] 而不是 null，total 为 0。空表不是错误，也不是 40401。
func TestAdminRunsListEmpty(t *testing.T) {
	srv := newAdminRunsServer(&runReaderStub{})

	rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs"))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "runs data", data, "runs", "total", "limit", "offset")
	assertNoStore(t, rec)

	if data["total"] != float64(0) {
		t.Fatalf("空表 total 应为 0，实际 %#v", data["total"])
	}
	runs := contractSlice(t, "runs", data["runs"])
	if len(runs) != 0 {
		t.Fatalf("空表 runs 应为空数组，实际 %d 条", len(runs))
	}
	// 序列化后必须是 []，不能是 null——前端按数组遍历。
	if !strings.Contains(rec.Body.String(), `"runs":[]`) {
		t.Fatalf("空表应序列化为 []，实际 body=%s", rec.Body.String())
	}
}

// TestAdminRunsListRejectsBadParams 覆盖分页参数非法 → 400/40001（Requirement 7.1、
// design 错误表「limit/offset/run_id 非法」）：参数门在查库之前，仓储调用为 0。
//
// 「不是整数」与「越界」的文案必须不同：`offset=abc` 报「必须是整数」，`offset=-1`
// 才报「不能为负」；limit 同理分成「必须是整数」与「取值 1..100」。这里按整句等值断言，
// 混用两种文案（或把 abc 说成负数）都会立刻失败。文案点名参数，但不回显用户提交的原文。
func TestAdminRunsListRejectsBadParams(t *testing.T) {
	cases := []struct {
		name        string
		query       string
		wantMessage string
	}{
		{"limit 为 0", "?limit=0", "limit 取值 1..100"},
		{"limit 超过上界", "?limit=101", "limit 取值 1..100"},
		{"limit 为负", "?limit=-1", "limit 取值 1..100"},
		{"limit 非整数", "?limit=abc", "limit 必须是整数"},
		{"limit 是小数", "?limit=1.5", "limit 必须是整数"},
		{"limit 溢出 int64", "?limit=99999999999999999999", "limit 必须是整数"},
		{"offset 为负", "?offset=-1", "offset 不能为负"},
		{"offset 非整数", "?offset=abc", "offset 必须是整数"},
		{"offset 是小数", "?offset=0.5", "offset 必须是整数"},
		{"offset 溢出 int64", "?offset=99999999999999999999", "offset 必须是整数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &runReaderStub{
				summaries: []store.RunSummary{runningRunSummary()},
				total:     1,
			}
			srv := newAdminRunsServer(reader)

			rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs"+tc.query))

			body := assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			message, _ := body["message"].(string)
			if message != tc.wantMessage {
				t.Fatalf("40001 文案期望 %q，实际 %q", tc.wantMessage, message)
			}
			assertNoStore(t, rec)
			if reader.recentCalls != 0 {
				t.Fatalf("参数越界不该查库，实际 Recent 调用 %d 次", reader.recentCalls)
			}
		})
	}
}

// TestAdminRunsListStoreFailure 覆盖列表读库失败 → 500/50000（design 错误表）：
// 固定文案「内部错误」、SQL 细节不进响应，且响应仍带 no-store。
func TestAdminRunsListStoreFailure(t *testing.T) {
	const sqlDetail = "select collect_run: sentinel-runs-3c4d"
	reader := &runReaderStub{
		recentErr: fmt.Errorf("%s: %w", sqlDetail, model.ErrRunStoreFailed),
	}
	srv := newAdminRunsServer(reader)

	rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs"))

	body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if message, _ := body["message"].(string); message != "内部错误" {
		t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
	}
	if strings.Contains(rec.Body.String(), sqlDetail) {
		t.Fatalf("响应泄露了存储层细节: %s", rec.Body.String())
	}
	assertNoStore(t, rec)
}

// TestAdminRunDetailShape 覆盖 GET /api/admin/runs/{run_id} 的成功形状
// （Requirement 7.3、7.4）：data 恰好 3 个键；run 是 22 键摘要；tasks 是 7 键汇总
// （含必需与可选的失败数）；weeks 元素是 5 键，成功行的 error_message 为 null、
// 失败行的 fetched_at 为 null；路径里的 run_id 原样分发给 ByID / TaskSummary / Weeks。
//
// 路径参数取 14 而回放的摘要里 run_id 是 12（finishedRunID），两个值故意不同：handler
// 若把 summary.RunID 当成 tasks/weeks 的入参，替身收到的就是 12，下面的分发断言立刻红。
// 路径值与摘要值相等时这条断言什么也锁不住——那正是它要防的错配。
func TestAdminRunDetailShape(t *testing.T) {
	const detailPathRunID = 14

	failureMessage := "登录失效"
	reader := &runReaderStub{
		byID: map[int64]store.RunSummary{detailPathRunID: finishedRunSummary()},
		tasks: store.RunTaskSummary{
			RequiredTotal:       8,
			RequiredDone:        7,
			RequiredFailed:      1,
			OptionalTotal:       5,
			OptionalDone:        4,
			OptionalFailed:      1,
			FirstFailureMessage: failureMessage,
			RecordedErrorCode:   "upstream_timeout",
		},
		weeks: []store.RunWeekResult{
			{Week: 9, BuildingID: "Q10", OK: true, FetchedAt: &runFetchedAt},
			{Week: 9, BuildingID: "Q03", OK: false, ErrorMessage: &failureMessage},
		},
	}
	srv := newAdminRunsServer(reader)

	rec := serveAdminRuns(srv, adminRunsRequest(fmt.Sprintf("/api/admin/runs/%d", detailPathRunID)))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "run detail data", data, "run", "tasks", "weeks")
	assertNoStore(t, rec)

	// 详情 handler 把同一个路径参数分发给三次查询；三个 last*RunID 都必须等于路径值 14。
	// 摘要里的 run_id 是 12，若 handler 误把 summary.RunID 当入参，这里会看到 12。
	if reader.byIDCalls != 1 || reader.lastByIDRunID != detailPathRunID {
		t.Fatalf("路径参数未正确交给 ByID：byIDCalls=%d lastByIDRunID=%d，期望 %d",
			reader.byIDCalls, reader.lastByIDRunID, detailPathRunID)
	}
	if reader.tasksCalls != 1 || reader.lastTaskRunID != detailPathRunID {
		t.Fatalf("路径参数未正确交给 TaskSummary：tasksCalls=%d lastTaskRunID=%d，期望 %d",
			reader.tasksCalls, reader.lastTaskRunID, detailPathRunID)
	}
	if reader.weeksCalls != 1 || reader.lastWeekRunID != detailPathRunID {
		t.Fatalf("路径参数未正确交给 Weeks：weeksCalls=%d lastWeekRunID=%d，期望 %d",
			reader.weeksCalls, reader.lastWeekRunID, detailPathRunID)
	}

	run := contractMap(t, "run", data["run"])
	assertContractKeys(t, "run", run, runSummaryKeys...)
	// run 块回显的是摘要自己的 run_id（12），不是路径参数 14：只有两者不同，才能同时
	// 证明「响应里的 run_id 来自仓储」与「分发给 tasks/weeks 的编号来自路径」。
	for key, want := range map[string]any{
		"run_id":              float64(finishedRunID),
		"status":              "partial",
		"phase":               "publish",
		"started_at":          "2026-11-08T12:00:00Z",
		"finished_at":         "2026-11-08T12:05:00Z",
		"updated_at":          "2026-11-08T12:05:00Z",
		"term":                "2026-2027-1",
		"window_start":        float64(9),
		"window_end":          float64(12),
		"weeks_total":         float64(4),
		"rooms_total":         float64(120),
		"required_total":      float64(8),
		"required_done":       float64(7),
		"optional_total":      float64(5),
		"optional_done":       float64(4),
		"unknown_cells":       float64(3),
		"composite_cells":     float64(2),
		"attr_conflict_count": float64(1),
		"release_id":          "rel_20261108_01",
		"released_at":         "2026-11-08T12:06:00Z",
		"error_code":          nil,
		"error_message":       nil,
	} {
		if run[key] != want {
			t.Fatalf("run.%s 期望 %#v，实际 %#v", key, want, run[key])
		}
	}

	tasks := contractMap(t, "tasks", data["tasks"])
	assertContractKeys(t, "tasks", tasks, runTasksKeys...)
	for key, want := range map[string]any{
		"required_total":        float64(8),
		"required_done":         float64(7),
		"required_failed":       float64(1),
		"optional_total":        float64(5),
		"optional_done":         float64(4),
		"optional_failed":       float64(1),
		"first_failure_message": failureMessage,
	} {
		if tasks[key] != want {
			t.Fatalf("tasks.%s 期望 %#v，实际 %#v", key, want, tasks[key])
		}
	}

	weeks := contractSlice(t, "weeks", data["weeks"])
	if len(weeks) != 2 {
		t.Fatalf("weeks 应有 2 条，实际 %d 条", len(weeks))
	}
	success := contractMap(t, "weeks[0]", weeks[0])
	assertContractKeys(t, "weeks[0]", success, runWeekKeys...)
	for key, want := range map[string]any{
		"week":          float64(9),
		"building_id":   "Q10",
		"ok":            true,
		"fetched_at":    "2026-11-08T12:01:02Z",
		"error_message": nil,
	} {
		if success[key] != want {
			t.Fatalf("weeks[0].%s 期望 %#v，实际 %#v", key, want, success[key])
		}
	}
	failed := contractMap(t, "weeks[1]", weeks[1])
	assertContractKeys(t, "weeks[1]", failed, runWeekKeys...)
	for key, want := range map[string]any{
		"week":          float64(9),
		"building_id":   "Q03",
		"ok":            false,
		"fetched_at":    nil,
		"error_message": failureMessage,
	} {
		if failed[key] != want {
			t.Fatalf("weeks[1].%s 期望 %#v，实际 %#v", key, want, failed[key])
		}
	}
}

// TestAdminRunDetailBlockedTermNull 覆盖 blocked 轮的详情（Requirement 7.2–7.4）：
// term / 窗口 / 版本 / 错误原因之外的可空列全是 null，任务汇总全零、
// weeks 是 [] 而不是 null。这条用例专门锁「扫描到 NULL 不该折成零值」的对外表现。
func TestAdminRunDetailBlockedTermNull(t *testing.T) {
	reader := &runReaderStub{
		byID: map[int64]store.RunSummary{blockedRunID: blockedRunSummary()},
	}
	srv := newAdminRunsServer(reader)

	rec := serveAdminRuns(srv, adminRunsRequest(fmt.Sprintf("/api/admin/runs/%d", blockedRunID)))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "run detail data", data, "run", "tasks", "weeks")
	assertNoStore(t, rec)

	run := contractMap(t, "run", data["run"])
	assertContractKeys(t, "run", run, runSummaryKeys...)
	for key, want := range map[string]any{
		"run_id":        float64(blockedRunID),
		"status":        "blocked",
		"phase":         "finished",
		"started_at":    "2026-11-08T11:59:00Z",
		"finished_at":   "2026-11-08T11:59:00Z",
		"updated_at":    "2026-11-08T11:59:00Z",
		"term":          nil,
		"window_start":  nil,
		"window_end":    nil,
		"release_id":    nil,
		"released_at":   nil,
		"error_code":    "whitelist_empty",
		"error_message": "教学楼白名单为空，未发起采集",
	} {
		if run[key] != want {
			t.Fatalf("run.%s 期望 %#v，实际 %#v", key, want, run[key])
		}
	}
	if !strings.Contains(rec.Body.String(), `"term":null`) {
		t.Fatalf("blocked 轮的 term 应序列化为 null，实际 body=%s", rec.Body.String())
	}

	tasks := contractMap(t, "tasks", data["tasks"])
	assertContractKeys(t, "tasks", tasks, runTasksKeys...)
	if tasks["required_total"] != float64(0) || tasks["optional_failed"] != float64(0) {
		t.Fatalf("blocked 轮的任务汇总应全零，实际 %#v", tasks)
	}
	if tasks["first_failure_message"] != "" {
		t.Fatalf("没有失败任务时 first_failure_message 应为空串，实际 %#v", tasks["first_failure_message"])
	}

	// 没有任何已写入的周结果：空数组，不是 null，也不是错误。
	weeks := contractSlice(t, "weeks", data["weeks"])
	if len(weeks) != 0 {
		t.Fatalf("没有周结果时 weeks 应为空数组，实际 %d 条", len(weeks))
	}
	if !strings.Contains(rec.Body.String(), `"weeks":[]`) {
		t.Fatalf("空周结果应序列化为 []，实际 body=%s", rec.Body.String())
	}
}

// TestAdminRunDetailUnknownID 覆盖未知 run_id → 404/40401（Requirement 7.5）：
// 固定文案「采集轮次不存在」，且不查任务汇总与周结果——「没有这一轮」必须在查另外
// 两张表之前返回，否则同一个事实会在另一条路径上被映射成 50000。
func TestAdminRunDetailUnknownID(t *testing.T) {
	reader := &runReaderStub{byID: map[int64]store.RunSummary{runningRunID: runningRunSummary()}}
	srv := newAdminRunsServer(reader)

	rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs/99"))

	body := assertContractFailure(t, rec, http.StatusNotFound, codeRunNotFound)
	if message, _ := body["message"].(string); message != "采集轮次不存在" {
		t.Fatalf("40401 文案应固定为「采集轮次不存在」，实际 %q", message)
	}
	assertNoStore(t, rec)
	if reader.tasksCalls != 0 || reader.weeksCalls != 0 {
		t.Fatalf("未知 id 不该查任务汇总与周结果：tasks=%d weeks=%d",
			reader.tasksCalls, reader.weeksCalls)
	}
}

// TestAdminRunDetailRejectsBadRunID 覆盖路径参数非法 → 400/40001（Requirement 7.1 的
// 「非正整数」口径）：非数字、0、负数、小数、超 int64 都拒绝，且一次库都不查。
func TestAdminRunDetailRejectsBadRunID(t *testing.T) {
	cases := []struct{ name, raw string }{
		{"非数字", "abc"},
		{"0 不是正整数", "0"},
		{"负数", "-1"},
		{"小数", "1.5"},
		{"超出 int64", "99999999999999999999"},
		{"十六进制", "0x10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &runReaderStub{byID: map[int64]store.RunSummary{runningRunID: runningRunSummary()}}
			srv := newAdminRunsServer(reader)

			rec := serveAdminRuns(srv, adminRunsRequest("/api/admin/runs/"+tc.raw))

			body := assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			if message, _ := body["message"].(string); !strings.Contains(message, "run_id") {
				t.Fatalf("40001 文案应点名 run_id，实际 %q", message)
			}
			assertNoStore(t, rec)
			if reader.byIDCalls != 0 {
				t.Fatalf("非法 run_id 不该查库，实际 ByID 调用 %d 次", reader.byIDCalls)
			}
		})
	}
}

// TestAdminRunDetailStoreFailure 覆盖详情三处读库失败 → 500/50000（design 错误表）：
// ByID、任务汇总、周结果任一条失败都返回固定文案，不透 SQL 细节，且响应仍带 no-store。
func TestAdminRunDetailStoreFailure(t *testing.T) {
	const sqlDetail = "select sync_task: sentinel-detail-6e7f"
	storeErr := fmt.Errorf("%s: %w", sqlDetail, model.ErrRunStoreFailed)

	cases := []struct {
		name   string
		reader *runReaderStub
	}{
		{
			name: "ByID 失败",
			reader: &runReaderStub{
				byIDErr: storeErr,
			},
		},
		{
			name: "任务汇总失败",
			reader: &runReaderStub{
				byID:     map[int64]store.RunSummary{runningRunID: runningRunSummary()},
				tasksErr: storeErr,
			},
		},
		{
			name: "周结果失败",
			reader: &runReaderStub{
				byID:     map[int64]store.RunSummary{runningRunID: runningRunSummary()},
				weeksErr: storeErr,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAdminRunsServer(tc.reader)

			rec := serveAdminRuns(srv, adminRunsRequest(fmt.Sprintf("/api/admin/runs/%d", runningRunID)))

			body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			if message, _ := body["message"].(string); message != "内部错误" {
				t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
			}
			if strings.Contains(rec.Body.String(), sqlDetail) {
				t.Fatalf("响应泄露了存储层细节: %s", rec.Body.String())
			}
			assertNoStore(t, rec)
		})
	}
}

// TestAdminRunsWithoutReader 覆盖未装配 WithRunStatus（Requirement 10.7）：
// 两个端点仍注册，按 500/50000 返回而不是 404——装配缺失不能被「这一轮不存在」掩盖。
//
// 第三个目标带 limit=0：这个参数单独看是 400/40001，未装配时必须仍是 50000，
// 证明装配检查排在参数门之前（部署缺陷优先于客户端参数错误）。
func TestAdminRunsWithoutReader(t *testing.T) {
	srv := newAdminRunsServerWithoutReader()

	for _, target := range []string{
		"/api/admin/runs",
		fmt.Sprintf("/api/admin/runs/%d", runningRunID),
		"/api/admin/runs?limit=0",
	} {
		rec := serveAdminRuns(srv, adminRunsRequest(target))

		assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
		assertNoStore(t, rec)
	}
}

// TestAdminRunsRequiresSession 覆盖无会话 → 401/40101（Requirement 10.1）：
// 两个端点都在业务之前被中间件挡住，仓储调用为 0，响应带 no-store。
func TestAdminRunsRequiresSession(t *testing.T) {
	reader := &runReaderStub{
		summaries: []store.RunSummary{runningRunSummary()},
		total:     1,
		byID:      map[int64]store.RunSummary{runningRunID: runningRunSummary()},
	}
	srv := newAdminRunsServer(reader)

	for _, target := range []string{"/api/admin/runs", fmt.Sprintf("/api/admin/runs/%d", runningRunID)} {
		// 不带 Cookie：adminRead 的 withSession 直接 40101。
		rec := serveAdminRuns(srv, httptest.NewRequest(http.MethodGet, target, nil))

		assertContractFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
		assertNoStore(t, rec)
	}
	if reader.recentCalls != 0 || reader.byIDCalls != 0 || reader.tasksCalls != 0 || reader.weeksCalls != 0 {
		t.Fatalf("无会话不该查库：recent=%d byID=%d tasks=%d weeks=%d",
			reader.recentCalls, reader.byIDCalls, reader.tasksCalls, reader.weeksCalls)
	}
}

// TestAdminRunsErrorsKeepNoStore 覆盖「错误响应也带 no-store」（Requirement 10.2）：
// 参数越界、未知 id、装配缺失三种 4xx/5xx 都在管理前缀下，缓存头不能漏。
// 形状断言已在各自用例里，这里只把三种错误集中扫一遍，防止将来改中间件链时漏掉某条出口。
func TestAdminRunsErrorsKeepNoStore(t *testing.T) {
	cases := []struct {
		name   string
		srv    *Server
		target string
		status int
		code   int
	}{
		{
			name:   "参数越界",
			srv:    newAdminRunsServer(&runReaderStub{}),
			target: "/api/admin/runs?limit=0",
			status: http.StatusBadRequest,
			code:   codeBadRequest,
		},
		{
			name:   "未知 id",
			srv:    newAdminRunsServer(&runReaderStub{}),
			target: "/api/admin/runs/99",
			status: http.StatusNotFound,
			code:   codeRunNotFound,
		},
		{
			name:   "装配缺失",
			srv:    newAdminRunsServerWithoutReader(),
			target: "/api/admin/runs",
			status: http.StatusInternalServerError,
			code:   codeInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdminRuns(tc.srv, adminRunsRequest(tc.target))
			assertContractFailure(t, rec, tc.status, tc.code)
			assertNoStore(t, rec)
		})
	}
}

// TestAdminRunsReaderErrorsAreNotSwallowed 直接对替身做一次「它确实按约定返回错误」的
// 自检：上面几条失败用例依赖 recentErr / byIDErr / tasksErr / weeksErr 真的被返回，
// 若替身把错误吞掉，那些用例会变成假绿（响应 200 而断言仍可能通过其它分支）。
// errors.Is 同时锁住了「仓储错误包着 model.ErrRunStoreFailed」这一层。
func TestAdminRunsReaderErrorsAreNotSwallowed(t *testing.T) {
	sentinel := fmt.Errorf("sentinel: %w", model.ErrRunStoreFailed)
	reader := &runReaderStub{
		recentErr: sentinel,
		byIDErr:   sentinel,
		tasksErr:  sentinel,
		weeksErr:  sentinel,
	}

	if _, _, err := reader.Recent(context.Background(), 20, 0); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("Recent 应原样返回注入的错误，实际 %v", err)
	}
	if _, _, err := reader.ByID(context.Background(), 1); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("ByID 应原样返回注入的错误，实际 %v", err)
	}
	if _, err := reader.TaskSummary(context.Background(), 1); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("TaskSummary 应原样返回注入的错误，实际 %v", err)
	}
	if _, err := reader.Weeks(context.Background(), 1); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("Weeks 应原样返回注入的错误，实际 %v", err)
	}
}
