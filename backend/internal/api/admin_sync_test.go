package api

// 本文件是手动同步端点的契约测试（tasklist 第 11 项）：
//
//	POST /api/admin/sync   202 的 running / blocked 两种 data 形状、409 冲突文案（进行中
//	                       直判与输家冲突重读两条路径）、未装配 50000、500 的五段日志
//	                       文案（收敛/读轮次/读白名单/建 blocked 轮/采集启动器）、
//	                       缺会话 40101、缺 CSRF 40301、不读体且不要求 Content-Type、
//	                       并发 50 请求只 1 个 202、所有响应 no-store
//
// 这里装的是真实的 runner.Service（配替身 RunStore / WhitelistSource / Collector），
// 而不是 SyncStarter 的纯替身：API 层的错误分派依赖 runner 产出的 *BusyError 与
// model.ErrCollectorUnavailable，纯替身直接返回这些值只能证明「handler 认识它们」，
// 证明不了「runner 在这条路径上真的会给出它们」。并发用例尤其如此——「只 1 个 202」
// 是 runner 的互斥锁与 Running 判定的共同结果，让替身自己判就变成同义反复。
// 请求一律经过 s.Handler()，路由注册、adminWriteNoBody（no-store → 会话 → CSRF）
// 与 handler 都在测。

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
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 同步结果的固定夹具。两个编号与轮次读取端点那组（12/13/14）刻意不同，
// 断言里出现 15/16 时不会与「读到的摘要」混淆。
const (
	syncRunID        = 15 // 202 running 的轮次编号
	syncBlockedRunID = 16 // 202 blocked 的轮次编号
)

// 两个 started_at 夹具都用 +08:00 时区构造（runCST 来自轮次测试）：响应必须是
// UTC Z 写法，转换一旦缺失就会输出 +08:00 偏移，用例立刻红。
var (
	syncStartedAt        = time.Date(2026, 11, 8, 20, 10, 0, 0, runCST) // 12:10:00Z
	syncBlockedStartedAt = time.Date(2026, 11, 8, 20, 11, 0, 0, runCST) // 12:11:00Z
)

// whitelistEmptyReasonText 是 blocked 轮原因的契约文案（design 的 202 blocked 示例）。
// runner 里那份常量不导出，这里按契约原文另写一份：改文案必须同时改测试，
// 否则响应契约会在无人察觉的情况下漂移。
const whitelistEmptyReasonText = "教学楼白名单为空，未发起采集"

// syncRunFake 同时实现 runner.RunStore、runner.WhitelistSource 与 runner.Collector，
// 一个替身覆盖启动端口的全部依赖。
//
// 所有字段都由 mu 保护：并发用例会让 50 个请求同时经过 runner，Running / Whitelist /
// StartBlockedRun / Start 都可能被并发调用（Collector.Start 与冲突重读的 Running
// 在 runner 的锁外，见 runner.Service.mapCollectError）。
//
// 默认语义贴近真实端口：Collector.Start 成功时顺手把「刚建出的轮次」写进 running，
// 让紧随其后的请求能在 Running 里读到它——这正是「并发只 1 个 202」的机制来源。
type syncRunFake struct {
	mu sync.Mutex

	// 进行中轮次：nil 表示没有。
	running    *store.RunSummary
	runningErr error

	// 白名单：空切片与 nil 都按「空」处理（runner 的判空口径）。
	whitelist    []model.Building
	whitelistErr error

	// blocked 建轮。
	blockedID     int64
	blockedAt     time.Time
	blockedErr    error
	blockedCalls  int
	blockedReason string
	blockedNow    time.Time

	// 采集启动。startResult 是成功路径的返回值；Start 会把它变成一条 running 摘要。
	startResult runner.StartResult
	collectErr  error
	// conflictRun 只在 collectErr 包装 model.ErrRunConflict 时使用：Start 返回错误前把它
	// 写进 running，复现「赢家刚落库、输家撞唯一索引」，让 runner 的冲突重读能命中它。
	conflictRun  *store.RunSummary
	collectCalls int
	sources      []model.TriggerSource

	// convergeErr 让「收敛过期轮次」失败；Converge 只在 runner 的锁外被读，装配前写入。
	convergeErr error
}

var (
	_ runner.RunStore        = (*syncRunFake)(nil)
	_ runner.WhitelistSource = (*syncRunFake)(nil)
	_ runner.Collector       = (*syncRunFake)(nil)
)

// Running 回放进行中轮次，记录不到不存在的错误。
func (f *syncRunFake) Running(context.Context) (store.RunSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return store.RunSummary{}, false, f.runningErr
	}
	if f.running == nil {
		return store.RunSummary{}, false, nil
	}
	return *f.running, true, nil
}

// StartBlockedRun 记录原因与入参时刻，返回预设的编号与开始时刻。
// 返回的 started_at 故意不等于 runner 的时钟（testAdminNow）：只有两者不同，
// 才能证明响应里的 started_at 来自库的返回值，而不是 handler 自己用 s.clock() 拼的。
func (f *syncRunFake) StartBlockedRun(_ context.Context, reason string, now time.Time) (int64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockedCalls++
	f.blockedReason = reason
	f.blockedNow = now
	if f.blockedErr != nil {
		return 0, time.Time{}, f.blockedErr
	}
	return f.blockedID, f.blockedAt, nil
}

// Whitelist 返回白名单副本。
func (f *syncRunFake) Whitelist(context.Context) ([]model.Building, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.whitelistErr != nil {
		return nil, f.whitelistErr
	}
	return append([]model.Building(nil), f.whitelist...), nil
}

// Start 模拟采集启动器：记录来源与调用次数，成功时把新轮次写进 running。
func (f *syncRunFake) Start(_ context.Context, source model.TriggerSource) (runner.StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collectCalls++
	f.sources = append(f.sources, source)
	if f.collectErr != nil {
		// 输家路径：赢家的轮次此时已落库，先让它对 Running 可见再返回冲突错误，
		// 这样 runner.mapCollectError 的重读才会命中它（见 TestAdminSyncConflictReread）。
		if f.conflictRun != nil {
			f.running = f.conflictRun
		}
		return runner.StartResult{}, f.collectErr
	}
	result := f.startResult
	summary := store.RunSummary{
		RunID:     result.RunID,
		Status:    result.Status,
		Phase:     "week_matrix",
		StartedAt: result.StartedAt,
		UpdatedAt: result.StartedAt,
	}
	f.running = &summary
	return result, nil
}

// calls 返回采集与 blocked 建轮的调用次数、以及采集启动器收到的来源。
func (f *syncRunFake) calls() (collect, blocked int, sources []model.TriggerSource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.collectCalls, f.blockedCalls, append([]model.TriggerSource(nil), f.sources...)
}

// newAdminSyncServer 组装「有效会话 + 真实 runner.Service」的 Server。
//
// wireCollector 为 false 时 Collect 传 nil，复现「采集启动器未装配」的过渡态：
// 白名单非空的手动同步必须 50000 且零建轮（Requirement 10.5、CP16）。
// Runner 的时钟固定为 testAdminNow，替身会话以它为基准签发。
func newAdminSyncServer(t *testing.T, admin adminStore, fake *syncRunFake, wireCollector bool) *Server {
	t.Helper()
	return newAdminSyncServerWithLogger(t, quietLogger(), admin, fake, wireCollector)
}

// newAdminSyncServerWithLogger 同上，但换成调用方给的 logger。
//
// 只有需要断言日志的用例才要捕获输出：500 的五段失败分派（文案各不相同）与两条 409
// （s.fail 不写日志、缓冲必须为空）。其余用例继续用丢弃日志的 quietLogger，
// 免得日志断言散进每一条响应契约用例。
func newAdminSyncServerWithLogger(t *testing.T, logger *slog.Logger, admin adminStore, fake *syncRunFake, wireCollector bool) *Server {
	t.Helper()

	var collect runner.Collector
	if wireCollector {
		collect = fake
	}
	svc, err := runner.New(runner.Deps{
		Runs:     fake,
		Settings: fake,
		Collect:  collect,
		Converge: func(context.Context, time.Time) ([]int64, error) { return nil, fake.convergeErr },
		Log:      logger,
		Clock:    func() time.Time { return testAdminNow },
	})
	if err != nil {
		t.Fatalf("组装 runner.Service 失败: %v", err)
	}

	srv := New(logger, nil, nil, nil, nil,
		WithAdmin(admin, nil),
		WithSync(svc),
	)
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// newActiveAdminSyncServer 是「有效会话 + 装配采集启动器 + 丢弃日志」的常用组合。
//
// 只有需要断言日志的用例不走它：500 那组要捕获五段文案、还要单独构造「启动器未装配」，
// 两条 409 要断言缓冲为空，所以都直接用 newAdminSyncServerWithLogger。其余用例一律
// 装配启动器——白名单非空的成功路径不装配就只会得到 50000，断言 202 的用例会直接红。
func newActiveAdminSyncServer(t *testing.T, fake *syncRunFake) *Server {
	t.Helper()
	return newAdminSyncServer(t, activeAdminStore(), fake, true)
}

// adminSyncRequest 构造同步请求：默认没有请求体、没有 Content-Type（Requirement 8.7）。
// rawToken 为空表示不带会话 Cookie，csrf 为空表示不带 X-CSRF-Token。
func adminSyncRequest(rawToken, csrf string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/sync", nil)
	if rawToken != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: rawToken})
	}
	if csrf != "" {
		req.Header.Set(adminCSRFHeader, csrf)
	}
	return req
}

// serveAdminSync 经完整的 Handler 跑一个请求（路由 + 中间件 + handler）。
func serveAdminSync(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// contractAcceptedData 断言 202 + 成功信封（code=0、message=success、request_id 合规），
// 并返回 data 对象。
//
// 不能直接用 contractSuccessData：它把状态码断言成 200，而同步的语义是「已受理、还没跑完」，
// 202 是本端点的契约（design 的 202 示例）。复用的是参数化后的 contractSuccessDataStatus：
// 信封键集合、业务码、message 与 request_id 的校验都只有一处定义，202 不会因为另写一份
// 副本而漏掉后来新增的信封检查。
func contractAcceptedData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	return contractSuccessDataStatus(t, rec, http.StatusAccepted)
}

// TestAdminSyncAcceptedShapes 覆盖 202 的两种 data 形状（Requirement 8.1、8.4）：
//
//	running  恰好 3 个键：run_id / status / started_at
//	blocked  4 个键：多一个 reason
//
// 两条用例都断言 started_at 是从替身返回值 UTC 化后的 RFC3339（夹具用 +08:00 构造），
// 以及采集启动器收到的触发来源是 manual 而不是 cron——来源混淆不会体现在响应上，
// 只会让日志里的 trigger 与管理员的操作对不上（D7）。
func TestAdminSyncAcceptedShapes(t *testing.T) {
	t.Run("running 三键", func(t *testing.T) {
		fake := &syncRunFake{
			whitelist:   []model.Building{{ID: "Q10", Name: "10号楼"}},
			startResult: runner.StartResult{RunID: syncRunID, Status: runner.StatusRunning, StartedAt: syncStartedAt},
		}
		srv := newActiveAdminSyncServer(t, fake)

		rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

		data := contractAcceptedData(t, rec)
		assertContractKeys(t, "sync data", data, "run_id", "status", "started_at")
		assertNoStore(t, rec)

		if data["run_id"] != float64(syncRunID) {
			t.Fatalf("run_id 期望 %d，实际 %#v", syncRunID, data["run_id"])
		}
		// status 按字面量断言，不复用 runner.StatusRunning：夹具用 runner 常量是为了
		// 造出「runner 会返回的形状」，断言必须盯住真正写进 JSON 的字符串。两边都用常量
		// 时，常量一旦改名或改值，夹具跟着变、断言也跟着变，契约早已漂走却仍全绿。
		if data["status"] != "running" {
			t.Fatalf("status 期望 %q，实际 %#v", "running", data["status"])
		}
		if data["started_at"] != "2026-11-08T12:10:00Z" {
			t.Fatalf("started_at 期望 2026-11-08T12:10:00Z，实际 %#v", data["started_at"])
		}
		// running 不该带 reason：多出这个键会让前端把「已启动」读成「被阻止」。
		if strings.Contains(rec.Body.String(), `"reason"`) {
			t.Fatalf("running 响应不该出现 reason，body=%s", rec.Body.String())
		}

		collect, blocked, sources := fake.calls()
		if collect != 1 || blocked != 0 {
			t.Fatalf("期望采集启动 1 次、blocked 建轮 0 次，实际 %d / %d", collect, blocked)
		}
		if len(sources) != 1 || sources[0] != model.TriggerManual {
			t.Fatalf("触发来源期望 [manual]，实际 %v", sources)
		}
	})

	t.Run("blocked 四键含 reason", func(t *testing.T) {
		fake := &syncRunFake{
			blockedID: syncBlockedRunID,
			blockedAt: syncBlockedStartedAt,
		}
		srv := newActiveAdminSyncServer(t, fake)

		rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

		data := contractAcceptedData(t, rec)
		assertContractKeys(t, "sync data", data, "run_id", "status", "started_at", "reason")
		assertNoStore(t, rec)

		if data["run_id"] != float64(syncBlockedRunID) {
			t.Fatalf("run_id 期望 %d，实际 %#v", syncBlockedRunID, data["run_id"])
		}
		// 同 running 那条：断言线上的字面量，常量只用在夹具里。
		if data["status"] != "blocked" {
			t.Fatalf("status 期望 %q，实际 %#v", "blocked", data["status"])
		}
		if data["started_at"] != "2026-11-08T12:11:00Z" {
			t.Fatalf("started_at 期望 2026-11-08T12:11:00Z，实际 %#v", data["started_at"])
		}
		if data["reason"] != whitelistEmptyReasonText {
			t.Fatalf("reason 期望 %q，实际 %#v", whitelistEmptyReasonText, data["reason"])
		}

		// 白名单为空：一行采集都不启动，只建一条 blocked 终态轮次。
		collect, blocked, _ := fake.calls()
		if collect != 0 || blocked != 1 {
			t.Fatalf("期望采集启动 0 次、blocked 建轮 1 次，实际 %d / %d", collect, blocked)
		}
		if fake.blockedReason != whitelistEmptyReasonText {
			t.Fatalf("blocked 原因期望 %q，实际 %q", whitelistEmptyReasonText, fake.blockedReason)
		}
		// now 取自 runner 的时钟：分两次取会让收敛与建轮跨过秒边界。
		if !fake.blockedNow.Equal(testAdminNow) {
			t.Fatalf("StartBlockedRun 的 now 期望 %v，实际 %v", testAdminNow, fake.blockedNow)
		}
	})
}

// TestSyncDataReasonByStatus 钉住 reason 键由状态决定、不由 Reason 是否为空决定。
//
// 端到端用例只走得到 runner 正常产出的两种组合（running + 空 Reason、blocked + 非空
// Reason），反向组合在 HTTP 层构造不出来，只能在函数层面补：
//
//	running + 非空 Reason  → 仍无 reason 键，否则前端会把「已启动」读成「被阻止」
//	blocked + 空 Reason    → 仍有 reason 键（空串），否则前端拿不到「为什么没采」
//
// 断言落在这条 JSON 的键集合上而不是结构体字段上：omitempty 的语义只有序列化之后才
// 是契约，直接看指针是否为 nil 只能证明赋值分支走对了，证明不了线上响应少了/多了键。
func TestSyncDataReasonByStatus(t *testing.T) {
	cases := []struct {
		name       string
		result     runner.StartResult
		wantKeys   []string
		hasReason  bool
		wantReason string
	}{
		{
			name:     "running 空 Reason 三键",
			result:   runner.StartResult{RunID: syncRunID, Status: runner.StatusRunning, StartedAt: syncStartedAt},
			wantKeys: []string{"run_id", "status", "started_at"},
		},
		{
			name: "running 非空 Reason 仍三键",
			result: runner.StartResult{
				RunID:     syncRunID,
				Status:    runner.StatusRunning,
				StartedAt: syncStartedAt,
				Reason:    "注入方违约塞进来的原因",
			},
			wantKeys: []string{"run_id", "status", "started_at"},
		},
		{
			name:       "blocked 空 Reason 仍四键",
			result:     runner.StartResult{RunID: syncBlockedRunID, Status: runner.StatusBlocked, StartedAt: syncBlockedStartedAt},
			wantKeys:   []string{"run_id", "status", "started_at", "reason"},
			hasReason:  true,
			wantReason: "",
		},
		{
			name: "blocked 非空 Reason 带原文",
			result: runner.StartResult{
				RunID:     syncBlockedRunID,
				Status:    runner.StatusBlocked,
				StartedAt: syncBlockedStartedAt,
				Reason:    whitelistEmptyReasonText,
			},
			wantKeys:   []string{"run_id", "status", "started_at", "reason"},
			hasReason:  true,
			wantReason: whitelistEmptyReasonText,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(syncData(tc.result))
			if err != nil {
				t.Fatalf("data 应可序列化: %v", err)
			}
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatalf("data 不是合法 JSON: %v, body=%s", err, raw)
			}
			assertContractKeys(t, "sync data", data, tc.wantKeys...)

			reason, hasReason := data["reason"]
			if hasReason != tc.hasReason {
				t.Fatalf("reason 键是否存在期望 %v，实际 %v（body=%s）", tc.hasReason, hasReason, raw)
			}
			if tc.hasReason && reason != tc.wantReason {
				t.Fatalf("reason 期望 %#v，实际 %#v", tc.wantReason, reason)
			}
		})
	}
}

// TestAdminSyncBusyConflict 覆盖「已有进行中轮次」→ 409 / 40901（Requirement 8.2、CP14）。
//
// 文案必须点名冲突轮次的 run_id 与 phase，且与 design 的 409 示例逐字一致——它由
// runner.BusyError.Error() 产出，本用例锁住「handler 直接透出而不是另拼一句」。
// 冲突请求不建任何轮次、不调采集启动器（两者调用数为 0），data 恒为 null
// （admin-bootstrap 的失败响应契约，不为塞冲突轮次而改）。
//
// 409 走 s.fail（客户端可理解、可等待的状态），不是 adminFail：捕获 logger 断言缓冲为空，
// 防止哪天把它误接到 500 的留痕路径上、让一次并发的正常拒绝刷出日志。
func TestAdminSyncBusyConflict(t *testing.T) {
	conflict := runningRunSummary()
	fake := &syncRunFake{
		running:   &conflict,
		whitelist: []model.Building{{ID: "Q10", Name: "10号楼"}},
	}
	var logBuf bytes.Buffer
	srv := newAdminSyncServerWithLogger(t,
		slog.New(slog.NewJSONHandler(&logBuf, nil)), activeAdminStore(), fake, true)

	rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

	body := assertContractFailure(t, rec, http.StatusConflict, codeRunBusy)
	assertNoStore(t, rec)

	want := "已有一轮采集进行中（run_id=14, phase=week_matrix），请等待其结束"
	if message, _ := body["message"].(string); message != want {
		t.Fatalf("409 文案期望 %q，实际 %q", want, message)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("409 不该写日志，实际：%s", logBuf.String())
	}

	collect, blocked, _ := fake.calls()
	if collect != 0 || blocked != 0 {
		t.Fatalf("冲突不该建轮：采集启动 %d 次、blocked 建轮 %d 次", collect, blocked)
	}
}

// TestAdminSyncConflictReread 覆盖「输家撞唯一索引、重读 Running 命中」→ 409 / 40901
// （D9、CP11），这是 design「并发同步」用例里输家的真实形态。
//
// 与 TestAdminSyncBusyConflict 的入口不同：那条在 startLocked 的 Running 判空处就返回
// *BusyError；这条是两个请求同时越过判空，输家建轮时撞上 collect_run_single_running
// ——Collector 返回包装 model.ErrRunConflict 的错误，runner 在锁外重读 Running，命中赢家
// 刚落库的轮次，同样折成 409，而不是把冲突漏成 50000。两段路径合起来才盖住「只 1 个 202」：
// TestAdminSyncConcurrentOnlyOneAccepted 走单进程内的锁与判空，本用例走多副本兜底。
//
// 这条 409 虽然源头是 Collector 的一次失败，但映射后仍是客户端可理解的状态：走 s.fail
// 不写日志，捕获 logger 的缓冲必须为空。
func TestAdminSyncConflictReread(t *testing.T) {
	conflict := runningRunSummary()
	fake := &syncRunFake{
		whitelist:   []model.Building{{ID: "Q10", Name: "10号楼"}},
		collectErr:  fmt.Errorf("insert collect_run: %w", model.ErrRunConflict),
		conflictRun: &conflict,
	}
	var logBuf bytes.Buffer
	srv := newAdminSyncServerWithLogger(t,
		slog.New(slog.NewJSONHandler(&logBuf, nil)), activeAdminStore(), fake, true)

	rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

	body := assertContractFailure(t, rec, http.StatusConflict, codeRunBusy)
	assertNoStore(t, rec)

	// 文案必须来自冲突重读到的赢家摘要，而不是错误的原文（那句会带 SQL/insert 细节）。
	want := "已有一轮采集进行中（run_id=14, phase=week_matrix），请等待其结束"
	if message, _ := body["message"].(string); message != want {
		t.Fatalf("409 文案期望 %q，实际 %q", want, message)
	}
	if strings.Contains(rec.Body.String(), "insert collect_run") {
		t.Fatalf("409 泄露了 Collector 错误原文: %s", rec.Body.String())
	}
	if logBuf.Len() != 0 {
		t.Fatalf("409 不该写日志，实际：%s", logBuf.String())
	}

	// 输家确实试过启动（所以才会撞上冲突），但没有建出任何轮次。
	collect, blocked, _ := fake.calls()
	if collect != 1 || blocked != 0 {
		t.Fatalf("期望采集启动 1 次（输家撞冲突）、blocked 建轮 0 次，实际 %d / %d", collect, blocked)
	}
}

// TestAdminSyncInternalFailures 覆盖启动路上的服务端故障 → 500 / 50000（design 错误表、
// Requirement 10.5）：采集启动器未装配、收敛过期轮次失败、读进行中轮次失败、读白名单
// 失败、建 blocked 轮失败、采集启动器自身报错。
//
// 六种故障的响应文案完全相同（固定的「内部错误」）：仓储错误原文可能带 SQL 细节，
// 采集启动器的错误也可能带上游地址，一律只留在服务端上下文里、不进响应。响应与日志
// 都不出现注入的哨兵片段，且这些 500 都带 no-store。
//
// 日志侧按阶段分派：收敛、读进行中轮次、读白名单、建 blocked 轮、采集启动器五段各有
// 一条专属文案（且互不相同），未装配另有一条；连同一条失败也只记一行日志。
func TestAdminSyncInternalFailures(t *testing.T) {
	const sentinel = "sentinel-sync-9a1c"
	storeErr := fmt.Errorf("select collect_run: %s: %w", sentinel, model.ErrRunStoreFailed)
	settingErr := fmt.Errorf("select settings: %s: %w", sentinel, model.ErrSettingStoreFailed)
	whitelist := []model.Building{{ID: "Q10", Name: "10号楼"}}

	cases := []struct {
		name          string
		fake          *syncRunFake
		wireCollector bool
		wantCollect   int
		wantBlocked   int
		// wantLog 是 adminFail 写出的固定日志文案，也是这次失败的唯一类别标记。
		wantLog string
	}{
		{
			name:          "采集启动器未装配",
			fake:          &syncRunFake{whitelist: whitelist},
			wireCollector: false,
			wantCollect:   0,
			wantBlocked:   0, // 白名单非空但没装启动器：零建轮（CP16）
			wantLog:       "手动同步失败：采集启动器未装配",
		},
		{
			name:          "收敛过期轮次失败",
			fake:          &syncRunFake{whitelist: whitelist, convergeErr: storeErr},
			wireCollector: true,
			wantLog:       "手动同步失败（收敛过期轮次）",
		},
		{
			name:          "读取进行中轮次失败",
			fake:          &syncRunFake{whitelist: whitelist, runningErr: storeErr},
			wireCollector: true,
			wantLog:       "手动同步失败（读取进行中轮次）",
		},
		{
			name:          "读取白名单失败",
			fake:          &syncRunFake{whitelistErr: settingErr},
			wireCollector: true,
			wantLog:       "手动同步失败（读取白名单）",
		},
		{
			name:          "建立 blocked 轮失败",
			fake:          &syncRunFake{blockedErr: storeErr},
			wireCollector: true,
			wantBlocked:   1, // 尝试过建 blocked 轮，仓储拒绝后没有轮次
			wantLog:       "手动同步失败（建立 blocked 轮）",
		},
		{
			name: "采集启动器报错",
			fake: &syncRunFake{
				whitelist:  whitelist,
				collectErr: fmt.Errorf("start run: %s: %w", sentinel, model.ErrRunStoreFailed),
			},
			wireCollector: true,
			wantCollect:   1,
			wantLog:       "手动同步失败（采集启动器）",
		},
	}

	// 五段失败必须是五条互不相同的文案：重复一条，日志就分不出是哪一段断的，
	// 分阶段也就白做了。先把表本身检查一遍，避免某次改表把两条文案抄成一样。
	stageMsgs := make(map[string]string, 5)
	for _, tc := range cases {
		if !strings.HasPrefix(tc.wantLog, "手动同步失败（") {
			continue
		}
		if prev, ok := stageMsgs[tc.wantLog]; ok {
			t.Fatalf("%q 与 %q 的日志文案重复：%q", prev, tc.name, tc.wantLog)
		}
		stageMsgs[tc.wantLog] = tc.name
	}
	if len(stageMsgs) != 5 {
		t.Fatalf("五个阶段应有五条不同文案，实际 %d 条：%v", len(stageMsgs), stageMsgs)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			srv := newAdminSyncServerWithLogger(t,
				slog.New(slog.NewJSONHandler(&logBuf, nil)),
				activeAdminStore(), tc.fake, tc.wireCollector)

			rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

			body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			if message, _ := body["message"].(string); message != "内部错误" {
				t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
			}
			if strings.Contains(rec.Body.String(), sentinel) {
				t.Fatalf("响应泄露了内部细节: %s", rec.Body.String())
			}
			assertNoStore(t, rec)

			// 日志侧：这次失败只留一行日志，msg 是它对应的固定文案，err 原文一个字都不出现。
			if got := syncLogMsg(t, &logBuf); got != tc.wantLog {
				t.Fatalf("日志文案期望 %q，实际 %q", tc.wantLog, got)
			}
			if strings.Contains(logBuf.String(), sentinel) {
				t.Fatalf("日志泄露了 err 原文: %s", logBuf.String())
			}

			collect, blocked, _ := tc.fake.calls()
			if collect != tc.wantCollect || blocked != tc.wantBlocked {
				t.Fatalf("建轮次数不符：采集启动 %d 次、blocked %d 次，期望 %d / %d",
					collect, blocked, tc.wantCollect, tc.wantBlocked)
			}
		})
	}
}

// syncLogMsg 解析捕获到的 JSON 日志并返回唯一的 msg；不是恰好一条就判失败。
//
// 500 的失败路径只该留一行（adminFail 那一行）：多出来的一行会让排障把一次故障读成
// 两次。msg 用解析而不是 strings.Contains：文案里有全角括号与中文，按字段比对不会被
// JSON 转义或相邻字段影响，也能证明日志里没有第二段原文。
func syncLogMsg(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()

	trimmed := strings.TrimSpace(buf.String())
	lines := strings.Split(trimmed, "\n")
	if trimmed == "" || len(lines) != 1 {
		t.Fatalf("期望恰好一条日志，实际 %d 条：%q", len(lines), buf.String())
	}

	var entry struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v, line=%s", err, lines[0])
	}
	return entry.Msg
}

// TestSyncFailureLogMsg 覆盖 500 日志文案的兜底：取不到阶段时退回不点名阶段的文案
// 「手动同步失败」，未知阶段同样退回；任何分支都不拼 err 原文。
//
// 端到端用例只走得到 runner 会产出的五个阶段；Collector 报冲突后重读 Running 读库
// 失败时 mapCollectError 直接返回裸读错误，这条路径在函数层面钉住，免得兜底文案哪天被删掉。
func TestSyncFailureLogMsg(t *testing.T) {
	raw := errors.New("select collect_run: sentinel-stage-3f9d")
	busy := &runner.BusyError{Run: store.RunSummary{RunID: 14, Phase: "week_matrix"}}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"裸错误", raw, "手动同步失败"},
		{"繁忙错误不是阶段", busy, "手动同步失败"},
		{"未知阶段", &runner.StageError{Stage: "unknown", Err: raw}, "手动同步失败"},
		{
			name: "阶段被再包一层",
			err:  fmt.Errorf("collect: %w", &runner.StageError{Stage: runner.StageCollector, Err: raw}),
			want: "手动同步失败（采集启动器）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := syncFailureLogMsg(tc.err)
			if got != tc.want {
				t.Fatalf("日志文案期望 %q，实际 %q", tc.want, got)
			}
			if strings.Contains(got, "select collect_run") || strings.Contains(got, "sentinel-stage") {
				t.Fatalf("日志文案不该带 err 原文，实际 %q", got)
			}
		})
	}
}

// TestAdminSyncWithoutService 覆盖未装配 WithSync（Requirement 10.7）：
// 端点仍注册，按 500 / 50000 返回而不是 404/405——部署缺陷不能被「端点不存在」掩盖。
//
// 这条与「采集启动器未装配」（TestAdminSyncInternalFailures 首个用例）是两种不同的
// 装配缺陷：前者是 WithSync 根本没被调用，后者是 runner 的 Collector 为 nil。两者
// 响应文案同为固定的「内部错误」，只能靠日志区分，所以这里捕获日志断言 msg 是
// 「同步服务未装配」而不是「手动同步失败：采集启动器未装配」。
func TestAdminSyncWithoutService(t *testing.T) {
	var logBuf bytes.Buffer
	srv := New(slog.New(slog.NewJSONHandler(&logBuf, nil)),
		nil, nil, nil, nil, WithAdmin(activeAdminStore(), nil))
	srv.clock = func() time.Time { return testAdminNow }

	rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))

	body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if message, _ := body["message"].(string); message != "内部错误" {
		t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
	}
	assertNoStore(t, rec)

	if got := syncLogMsg(t, &logBuf); got != "同步服务未装配" {
		t.Fatalf("日志文案期望 %q，实际 %q", "同步服务未装配", got)
	}
}

// TestAdminSyncRequiresSessionAndCSRF 覆盖 adminWriteNoBody 的两道门（Requirement 10.1、CP7）：
//
//	缺会话（无 Cookie）      → 401 / 40101，且下发清除 Cookie 的 Set-Cookie
//	缺 CSRF、CSRF 不匹配      → 403 / 40301，保留会话（不下发 Set-Cookie）
//
// 三种失败都发生在业务之前：替身的采集与 blocked 建轮调用必须为 0——否则一个没有
// CSRF 的跨站 POST 就能建轮。白名单故意非空，漏放行时会真的建出一条 running。
func TestAdminSyncRequiresSessionAndCSRF(t *testing.T) {
	cases := []struct {
		name       string
		rawToken   string
		csrf       string
		wantStatus int
		wantCode   int
	}{
		{"缺会话", "", testAdminCSRF, http.StatusUnauthorized, codeSessionInvalid},
		{"缺 CSRF", testAdminRawToken, "", http.StatusForbidden, codeCSRFInvalid},
		{"CSRF 不匹配", testAdminRawToken, "wrong-token", http.StatusForbidden, codeCSRFInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &syncRunFake{
				whitelist:   []model.Building{{ID: "Q10", Name: "10号楼"}},
				startResult: runner.StartResult{RunID: syncRunID, Status: runner.StatusRunning, StartedAt: syncStartedAt},
			}
			srv := newActiveAdminSyncServer(t, fake)

			rec := serveAdminSync(srv, adminSyncRequest(tc.rawToken, tc.csrf))

			assertContractFailure(t, rec, tc.wantStatus, tc.wantCode)
			assertNoStore(t, rec)

			if tc.wantStatus == http.StatusUnauthorized {
				assertClearedCookie(t, rec, true)
			} else {
				// CSRF 失败不代表会话失效：不能顺手把 Cookie 清掉。
				assertNoSetCookie(t, rec)
			}

			collect, blocked, _ := fake.calls()
			if collect != 0 || blocked != 0 {
				t.Fatalf("门没拦住业务：采集启动 %d 次、blocked 建轮 %d 次", collect, blocked)
			}
		})
	}
}

// TestAdminSyncIgnoresBodyAndContentType 覆盖「同步不读请求体、不要求 Content-Type」
// （Requirement 8.7）：带一段非 JSON 文本、或 text/plain 的请求照样 202。
//
// 若链路误套了 withJSONBodyLimit，这两种请求会在体门处被 40001 拒掉；若 handler 误读了
// 体，非 JSON 也会失败。两种情况本用例都会红。
func TestAdminSyncIgnoresBodyAndContentType(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
	}{
		{"无 Content-Type 的非 JSON 体", "this is not json", ""},
		{"text/plain 体", `{"ignored":true}`, "text/plain"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &syncRunFake{
				whitelist:   []model.Building{{ID: "Q10", Name: "10号楼"}},
				startResult: runner.StartResult{RunID: syncRunID, Status: runner.StatusRunning, StartedAt: syncStartedAt},
			}
			srv := newActiveAdminSyncServer(t, fake)

			req := httptest.NewRequest(http.MethodPost, "/api/admin/sync", strings.NewReader(tc.body))
			req.AddCookie(&http.Cookie{Name: adminCookieName, Value: testAdminRawToken})
			req.Header.Set(adminCSRFHeader, testAdminCSRF)
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}

			rec := serveAdminSync(srv, req)

			data := contractAcceptedData(t, rec)
			assertContractKeys(t, "sync data", data, "run_id", "status", "started_at")
			assertNoStore(t, rec)
			if data["status"] != "running" {
				t.Fatalf("status 期望 running，实际 %#v", data["status"])
			}
		})
	}
}

// TestAdminSyncConcurrentOnlyOneAccepted 覆盖并发 50 个手动同步（Requirement 8.5、CP11）：
// 恰好 1 个 202，其余 49 个 409 / 40901，且采集启动器只被调用 1 次。
//
// 这里必须用真实的 runner.Service：并发互斥是它的锁与 Running 判定共同给出的结论，
// 换成 SyncStarter 替身自己数数，「只 1 个 202」就只是替身的实现细节。
// 单进程内输家先被 runner 的启动锁串行化、再被 Running 判空拦下，不经过 ErrRunConflict；
// 输家撞唯一索引那一形态（Collector 报冲突 → 重读 Running 命中）由
// TestAdminSyncConflictReread 单独覆盖。
// 会话来源用 lockedAdminStore 串行化替身的统计字段：否则 -race 会先在测试替身上
// 报 data race，把真正的并发断言淹掉。
func TestAdminSyncConcurrentOnlyOneAccepted(t *testing.T) {
	const concurrency = 50

	fake := &syncRunFake{
		whitelist:   []model.Building{{ID: "Q10", Name: "10号楼"}},
		startResult: runner.StartResult{RunID: syncRunID, Status: runner.StatusRunning, StartedAt: syncStartedAt},
	}
	srv := newAdminSyncServer(t, &lockedAdminStore{adminStore: activeAdminStore()}, fake, true)

	type result struct {
		status  int
		code    int
		message string
	}
	results := make([]result, concurrency)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 让 50 个请求尽量同时到达

			rec := serveAdminSync(srv, adminSyncRequest(testAdminRawToken, testAdminCSRF))
			var body struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Errorf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
			}
			results[i] = result{status: rec.Code, code: body.Code, message: body.Message}
		}()
	}
	close(start)
	wg.Wait()

	accepted, busy := 0, 0
	for _, got := range results {
		switch {
		case got.status == http.StatusAccepted && got.code == 0:
			accepted++
		case got.status == http.StatusConflict && got.code == codeRunBusy:
			busy++
			// 每个输家都要拿到点名冲突轮次的文案，而不是一句笼统的「已有轮次」。
			if !strings.Contains(got.message, "run_id="+fmt.Sprint(syncRunID)) ||
				!strings.Contains(got.message, "phase=week_matrix") {
				t.Fatalf("409 文案应点名 run_id 与 phase，实际 %q", got.message)
			}
		default:
			t.Fatalf("意外的响应：status=%d code=%d message=%q", got.status, got.code, got.message)
		}
	}

	if accepted != 1 {
		t.Fatalf("期望 1 个 202，实际 %d 个", accepted)
	}
	if busy != concurrency-1 {
		t.Fatalf("期望 %d 个 409，实际 %d 个", concurrency-1, busy)
	}

	collect, blocked, sources := fake.calls()
	if collect != 1 {
		t.Fatalf("采集启动器应只被调用 1 次，实际 %d 次", collect)
	}
	if blocked != 0 {
		t.Fatalf("白名单非空不该建 blocked 轮，实际 %d 次", blocked)
	}
	if len(sources) != 1 || sources[0] != model.TriggerManual {
		t.Fatalf("触发来源期望 [manual]，实际 %v", sources)
	}
}
