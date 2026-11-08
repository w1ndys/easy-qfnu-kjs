package runner

// 本文件用替身覆盖启动端口：共 25 个顶层函数 + 13 个运行时子用例，覆盖构造期依赖校验、
// 白名单为空（含 Collector 未装配的组合）、已有进行中轮次、running 与空白名单/未装配
// Collector 的优先级交叉、Converge 先于 Running、并发启动只建一轮、Collector 未装配、
// 冲突后的三种重读结果、各类错误原样上抛、cron 的五条分支（busy / 冲突重读命中 / 成功 /
// 空白名单 blocked / 其它失败）、启动日志的字段白名单与「日志写在锁外」，以及 started_at
// 分别取自库返回值（blocked）与 Collector 结果（running）。
// 所有用例都不连库、不发请求：RunStore、WhitelistSource、Collector 与 Converge 都是
// 本文件里的替身，日志写进内存 buffer 后按字段白名单断言。
//
// 替身刻意让 Collector.Start 在「真的建轮」时把 running 写回假仓储：否则互斥锁放开
// 之后，后续请求仍会读到「无 running」，测试就会把「没有互斥」误判成「只是多了几次
// 冲突」——那正是 CP11 要防的写法。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// testNow 是注入的固定时刻：Converge 的 cutoff 与传给 StartBlockedRun 的 now 都必须等于它，
// 用例因此不必 sleep，也不依赖运行当天的日期。
var testNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

// blockedStartedAt 是替身 StartBlockedRun 返回的 started_at：刻意比入参 now 晚一分钟。
// 结果里出现这个值，才说明 Start 用的是库的返回值，而不是直接把 now 当 started_at；
// 两者相等的话，这两种写法在测试里无法区分。
var blockedStartedAt = testNow.Add(time.Minute)

// collectorStartedAt 是替身 Collector 默认返回的开始时刻：同样刻意偏离注入时钟，
// 让「running 的 started_at 原样透传 Collector 结果」这件事可断言。
var collectorStartedAt = testNow.Add(2 * time.Minute)

// callRecorder 记录替身被调用的顺序，用来断言「Converge 先于 Running」。
type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

// add 追加一次调用；recorder 为 nil 时是空操作，普通用例不必都建一个。
func (r *callRecorder) add(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, name)
}

// snapshot 返回调用顺序的副本。
func (r *callRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// blockedRunCall 是一次 StartBlockedRun 的入参，供断言原因与时刻。
type blockedRunCall struct {
	reason string
	now    time.Time
}

// fakeRunStore 是 RunStore 的替身，并额外承担「Collector 建轮后 Running 能读到」的
// 职责。所有字段都由 mu 保护：并发用例会同时调用 Running 与 StartBlockedRun。
type fakeRunStore struct {
	recorder *callRecorder

	mu           sync.Mutex
	running      *store.RunSummary // 当前进行中轮次；nil 表示没有
	runningErr   error             // 非空时 Running 直接报错
	runningCalls int               // Running 被调用的次数
	blockedCalls []blockedRunCall  // StartBlockedRun 的入参记录
	blockedRunID int64             // 建轮成功时返回的编号
	blockedErr   error             // 非空时 StartBlockedRun 直接报错
}

func newFakeRunStore() *fakeRunStore {
	return &fakeRunStore{blockedRunID: 41}
}

// Running 依次记录调用、回放预设错误或当前进行中轮次。
func (f *fakeRunStore) Running(context.Context) (store.RunSummary, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runningCalls++
	f.recorder.add("running")

	if f.runningErr != nil {
		return store.RunSummary{}, false, f.runningErr
	}
	if f.running == nil {
		return store.RunSummary{}, false, nil
	}
	return *f.running, true, nil
}

// StartBlockedRun 记录入参；没有注入错误时返回固定的编号与 blockedStartedAt
// （比入参 now 晚一分钟，用来验证结果取自库的返回值）。
func (f *fakeRunStore) StartBlockedRun(_ context.Context, reason string, now time.Time) (int64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorder.add("blocked")
	f.blockedCalls = append(f.blockedCalls, blockedRunCall{reason: reason, now: now})

	if f.blockedErr != nil {
		return 0, time.Time{}, f.blockedErr
	}
	return f.blockedRunID, blockedStartedAt, nil
}

// setRunning 模拟「刚建出一轮 running」或「本来就有进行中轮次」。
func (f *fakeRunStore) setRunning(run store.RunSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = &run
}

// setRunningErr 让之后的 Running 直接报错。只能用在第一次 Running 之后（例如 Collector
// 的 onStart 里）：判冲突那次读库成功、冲突后重读失败，才是要覆盖的路径。
func (f *fakeRunStore) setRunningErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runningErr = err
}

// hasRunning 只供假 Collector 判唯一索引，不记录调用次数或顺序。
func (f *fakeRunStore) hasRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running != nil
}

func (f *fakeRunStore) runningCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runningCalls
}

func (f *fakeRunStore) blockedCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.blockedCalls)
}

// lastBlockedCall 返回最后一次 StartBlockedRun 的入参。
func (f *fakeRunStore) lastBlockedCall() blockedRunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blockedCalls[len(f.blockedCalls)-1]
}

// fakeCollector 是 Collector 的替身：默认路径模拟真实端口——先查有没有进行中轮次，
// 有就返回包装 model.ErrRunConflict 的错误，没有才「建轮」并返回成功。
//
// onStart 非空时完全接管 Start，用于构造「冲突重读命中」「冲突窗口已过」与
// 「冲突后重读失败」几种路径。
type fakeCollector struct {
	recorder *callRecorder
	store    *fakeRunStore
	result   StartResult // 默认成功路径返回的结果
	// startDelay 是默认路径在判唯一索引前的阻塞。并发用例把它撑到毫秒级：没有 Service
	// 的互斥锁时，已经读过 Running 的并发请求会一起挤进 Collector，调用次数必然大于 1。
	startDelay time.Duration
	onStart    func(ctx context.Context, source model.TriggerSource) (StartResult, error)

	mu      sync.Mutex
	calls   int
	sources []model.TriggerSource
}

func newFakeCollector(runs *fakeRunStore) *fakeCollector {
	return &fakeCollector{
		store:  runs,
		result: StartResult{RunID: 7, Status: StatusRunning, StartedAt: collectorStartedAt},
	}
}

// Start 记录调用与来源；onStart 为 nil 时按「唯一索引 + 建轮」的语义工作。
func (f *fakeCollector) Start(ctx context.Context, source model.TriggerSource) (StartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sources = append(f.sources, source)
	f.recorder.add("collector")

	if f.onStart != nil {
		return f.onStart(ctx, source)
	}
	if f.startDelay > 0 {
		time.Sleep(f.startDelay)
	}
	if f.store.hasRunning() {
		return StartResult{}, fmt.Errorf("%w: 已有一轮采集进行中", model.ErrRunConflict)
	}
	f.store.setRunning(store.RunSummary{
		RunID:     f.result.RunID,
		Status:    f.result.Status,
		Phase:     "login",
		StartedAt: f.result.StartedAt,
	})
	return f.result, nil
}

func (f *fakeCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCollector) recordedSources() []model.TriggerSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.TriggerSource(nil), f.sources...)
}

// whitelistFunc 让用例用闭包直接给白名单或错误，不必为每种情况建结构体。
type whitelistFunc func(ctx context.Context) ([]model.Building, error)

func (f whitelistFunc) Whitelist(ctx context.Context) ([]model.Building, error) {
	return f(ctx)
}

// testLogger 丢弃日志：大多数用例只关心返回值。
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// capturedLogger 把结构化日志写进 buffer，供字段白名单断言。
func capturedLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lockProbeHandler 包一层 JSON handler，在每次真正写出日志时检查 Service 的互斥锁是否
// 空闲：启动日志必须在解锁之后记，否则 handler 的 IO（文件、网络）会让并发启动排在
// 锁后面。TryLock 是非阻塞的，锁被占时直接判失败，不会把测试挂死。
type lockProbeHandler struct {
	slog.Handler
	svc *Service // New 之前就交给 Deps.Log，组装完成后回填
	t   *testing.T
}

// Handle 探测锁状态后再转给下层 JSON handler。
func (h *lockProbeHandler) Handle(ctx context.Context, rec slog.Record) error {
	if h.svc == nil {
		h.t.Error("测试未回填 Service，无法检查日志是否在锁外")
		return h.Handler.Handle(ctx, rec)
	}
	if !h.svc.mu.TryLock() {
		h.t.Errorf("日志 %q 在锁内写出：启动日志应在解锁之后再记", rec.Message)
	} else {
		h.svc.mu.Unlock()
	}
	return h.Handler.Handle(ctx, rec)
}

// WithAttrs 保持探测包装：本包目前不用 logger.With，但漏掉它会让探测静默失效。
func (h *lockProbeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &lockProbeHandler{Handler: h.Handler.WithAttrs(attrs), svc: h.svc, t: h.t}
}

// WithGroup 同上，保持探测包装。
func (h *lockProbeHandler) WithGroup(name string) slog.Handler {
	return &lockProbeHandler{Handler: h.Handler.WithGroup(name), svc: h.svc, t: h.t}
}

// newService 用替身组装被测服务；Log 缺省时补一个丢弃日志的 logger。
func newService(t *testing.T, deps Deps) *Service {
	t.Helper()
	if deps.Log == nil {
		deps.Log = testLogger()
	}
	svc, err := New(deps)
	if err != nil {
		t.Fatalf("组装 runner 失败: %v", err)
	}
	return svc
}

// whitelistOf 返回一条非空白名单，供「能走到 Collector」的用例使用。
func whitelistOf() []model.Building {
	return []model.Building{{ID: "Q10", Name: "综合教学楼"}}
}

// emptyConverge 是不做任何收敛的成功实现。
func emptyConverge(context.Context, time.Time) ([]int64, error) {
	return nil, nil
}

// fixedClock 返回注入的固定时刻。
func fixedClock() time.Time {
	return testNow
}

// decodeLogLines 把 JSON 日志逐行解成 map，供字段断言。
func decodeLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(buf.String())
	if trimmed == "" {
		return nil
	}

	lines := make([]map[string]any, 0)
	for _, raw := range strings.Split(trimmed, "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("日志行不是合法 JSON：%q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// assertAllowedLogFields 钉住启动日志的字段白名单：除 slog 自带的 time/level/msg 外，
// 只允许 run_id、status、trigger、reason 四项（Requirement 10.3、CP1）。
func assertAllowedLogFields(t *testing.T, line map[string]any) {
	t.Helper()
	allowed := map[string]bool{
		"time": true, "level": true, "msg": true,
		"run_id": true, "status": true, "trigger": true, "reason": true,
	}
	for key := range line {
		if !allowed[key] {
			t.Fatalf("日志出现了不该有的字段 %q：%v", key, line)
		}
	}
}

// TestNewRequiresDependencies 覆盖构造期校验：四个必填依赖缺一即失败，
// 错误包装 model.ErrConfigInvalid 并点名缺的是哪一项。
func TestNewRequiresDependencies(t *testing.T) {
	validDeps := func() Deps {
		return Deps{
			Runs:     newFakeRunStore(),
			Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return nil, nil }),
			Converge: emptyConverge,
			Log:      testLogger(),
		}
	}

	cases := []struct {
		name   string
		mutate func(*Deps)
		want   string
	}{
		{"缺少 Runs", func(d *Deps) { d.Runs = nil }, "Runs"},
		{"缺少 Settings", func(d *Deps) { d.Settings = nil }, "Settings"},
		{"缺少 Converge", func(d *Deps) { d.Converge = nil }, "Converge"},
		{"缺少 Log", func(d *Deps) { d.Log = nil }, "Log"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := validDeps()
			tc.mutate(&deps)

			svc, err := New(deps)
			if err == nil {
				t.Fatalf("缺依赖时应拒绝组装，实际返回 %+v", svc)
			}
			if svc != nil {
				t.Fatalf("组装失败时不应返回服务，实际 %+v", svc)
			}
			if !errors.Is(err, model.ErrConfigInvalid) {
				t.Fatalf("应包 model.ErrConfigInvalid，实际 %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案应点名 %s，实际 %q", tc.want, err.Error())
			}
		})
	}
}

// TestNewAllowsNilCollector 钉住过渡态：Collector 未装配不是构造失败，
// 白名单为空的 blocked 路径必须照常可用（design D6）。
func TestNewAllowsNilCollector(t *testing.T) {
	svc, err := New(Deps{
		Runs:     newFakeRunStore(),
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return nil, nil }),
		Converge: emptyConverge,
		Log:      testLogger(),
	})
	if err != nil {
		t.Fatalf("Collector 为 nil 时不应拒绝组装: %v", err)
	}
	if svc == nil {
		t.Fatal("组装成功时应返回服务")
	}
}

// TestStartBlocksWhenWhitelistEmpty 覆盖空名单：建立 blocked 轮次并返回其形状，
// 绝不调用 Collector；空数组与 nil 切片都按「空」处理。
func TestStartBlocksWhenWhitelistEmpty(t *testing.T) {
	cases := []struct {
		name      string
		whitelist []model.Building
	}{
		{"空数组", []model.Building{}},
		{"nil 切片", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := newFakeRunStore()
			collector := newFakeCollector(runs)
			svc := newService(t, Deps{
				Runs:     runs,
				Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return tc.whitelist, nil }),
				Collect:  collector,
				Converge: emptyConverge,
				Clock:    fixedClock,
			})

			result, err := svc.Start(context.Background(), model.TriggerManual)
			if err != nil {
				t.Fatalf("空名单应建立 blocked 轮次而不是报错: %v", err)
			}
			if result.Status != StatusBlocked {
				t.Fatalf("状态应为 %q，实际 %q", StatusBlocked, result.Status)
			}
			if result.RunID != runs.blockedRunID {
				t.Fatalf("run_id 应取自 StartBlockedRun，实际 %d", result.RunID)
			}
			if !result.StartedAt.Equal(blockedStartedAt) {
				t.Fatalf("started_at 应取 StartBlockedRun 的返回值 %s（而非入参 now），实际 %s", blockedStartedAt, result.StartedAt)
			}
			if result.Reason != whitelistEmptyReason {
				t.Fatalf("reason 应为固定文案 %q，实际 %q", whitelistEmptyReason, result.Reason)
			}

			if got := runs.blockedCallCount(); got != 1 {
				t.Fatalf("StartBlockedRun 应调用一次，实际 %d 次", got)
			}
			call := runs.lastBlockedCall()
			if call.reason != whitelistEmptyReason {
				t.Fatalf("blocked 原因应为固定文案，实际 %q", call.reason)
			}
			if !call.now.Equal(testNow) {
				t.Fatalf("建轮时刻应取同一个 now，实际 %s", call.now)
			}
			if got := collector.callCount(); got != 0 {
				t.Fatalf("白名单为空时不应调用 Collector.Start，实际 %d 次", got)
			}
		})
	}
}

// TestStartBlocksWhenWhitelistEmptyAndCollectorMissing 覆盖「Collector 未装配 + 空白名单」
// 的组合：过渡态下 blocked 路径不依赖采集执行链（design D6），所以必须照常建立 blocked
// 轮次，而不是走白名单非空才该出现的 ErrCollectorUnavailable（CP16 的反向边界）。
func TestStartBlocksWhenWhitelistEmptyAndCollectorMissing(t *testing.T) {
	runs := newFakeRunStore()
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return nil, nil }),
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	result, err := svc.Start(context.Background(), model.TriggerManual)
	if errors.Is(err, model.ErrCollectorUnavailable) {
		t.Fatalf("空白名单不应被 Collector 未装配拦下，实际 %v", err)
	}
	if err != nil {
		t.Fatalf("空白名单应返回 blocked 轮次而不是报错，实际 %v", err)
	}
	if result.Status != StatusBlocked {
		t.Fatalf("状态应为 %q，实际 %q", StatusBlocked, result.Status)
	}
	if result.RunID != runs.blockedRunID {
		t.Fatalf("run_id 应取自 StartBlockedRun，实际 %d", result.RunID)
	}
	if !result.StartedAt.Equal(blockedStartedAt) {
		t.Fatalf("started_at 应取 StartBlockedRun 的返回值 %s（而非入参 now），实际 %s", blockedStartedAt, result.StartedAt)
	}
	if result.Reason != whitelistEmptyReason {
		t.Fatalf("reason 应为固定文案 %q，实际 %q", whitelistEmptyReason, result.Reason)
	}
	if got := runs.blockedCallCount(); got != 1 {
		t.Fatalf("StartBlockedRun 应调用一次，实际 %d 次", got)
	}
	if !runs.lastBlockedCall().now.Equal(testNow) {
		t.Fatalf("建轮时刻应取注入时刻 %s，实际 %s", testNow, runs.lastBlockedCall().now)
	}
}

// TestStartReturnsBusyWithoutCallingCollector 覆盖「已有进行中轮次」：
// 返回 *BusyError（包装 ErrRunBusy），文案点名 run_id 与 phase，且不调 Collector（CP14）。
func TestStartReturnsBusyWithoutCallingCollector(t *testing.T) {
	runs := newFakeRunStore()
	runs.setRunning(store.RunSummary{
		RunID:     14,
		Status:    StatusRunning,
		Phase:     "week_matrix",
		StartedAt: testNow,
	})
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if err == nil {
		t.Fatal("已有进行中轮次时应返回冲突错误")
	}
	if !errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("应能 errors.Is 命中 model.ErrRunBusy，实际 %v", err)
	}

	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("应能 errors.As 取出 *BusyError，实际类型 %T", err)
	}
	if busy.Run.RunID != 14 || busy.Run.Phase != "week_matrix" {
		t.Fatalf("冲突轮次信息不符：run_id=%d phase=%q", busy.Run.RunID, busy.Run.Phase)
	}
	for _, fragment := range []string{"run_id=14", "phase=week_matrix"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("409 文案应包含 %q，实际 %q", fragment, err.Error())
		}
	}

	if got := collector.callCount(); got != 0 {
		t.Fatalf("冲突时不应调用 Collector.Start，实际 %d 次", got)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("冲突时不应建立 blocked 轮次，实际 %d 次", got)
	}
}

// TestStartBusyTakesPrecedenceOverWhitelistAndCollector 钉住交叉优先级：判冲突先于读
// 白名单，也先于「Collector 未装配」的检查。已有进行中轮次时，无论白名单是否为空、
// Collector 是否装配，都必须是 *BusyError——既不能建立 blocked 轮次，也不能报
// ErrCollectorUnavailable（CP14 与 CP16 的边界交叉）。
func TestStartBusyTakesPrecedenceOverWhitelistAndCollector(t *testing.T) {
	cases := []struct {
		name       string
		whitelist  []model.Building
		collectNil bool
	}{
		{"空白名单", []model.Building{}, false},
		{"Collector 未装配", whitelistOf(), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := newFakeRunStore()
			runs.setRunning(store.RunSummary{
				RunID:     14,
				Status:    StatusRunning,
				Phase:     "week_matrix",
				StartedAt: testNow,
			})

			whitelistReads := 0
			collector := newFakeCollector(runs)
			deps := Deps{
				Runs: runs,
				Settings: whitelistFunc(func(context.Context) ([]model.Building, error) {
					whitelistReads++
					return tc.whitelist, nil
				}),
				Converge: emptyConverge,
				Clock:    fixedClock,
			}
			if !tc.collectNil {
				deps.Collect = collector
			}
			svc := newService(t, deps)

			result, err := svc.Start(context.Background(), model.TriggerManual)
			var busy *BusyError
			if !errors.As(err, &busy) {
				t.Fatalf("已有进行中轮次时应返回 *BusyError，实际 %v", err)
			}
			if errors.Is(err, model.ErrCollectorUnavailable) {
				t.Fatalf("busy 不应退化成 ErrCollectorUnavailable，实际 %v", err)
			}
			if !errors.Is(err, model.ErrRunBusy) {
				t.Fatalf("应能 errors.Is 命中 model.ErrRunBusy，实际 %v", err)
			}
			if busy.Run.RunID != 14 || busy.Run.Phase != "week_matrix" {
				t.Fatalf("冲突轮次信息不符：run_id=%d phase=%q", busy.Run.RunID, busy.Run.Phase)
			}
			if result != (StartResult{}) {
				t.Fatalf("busy 时不应返回启动结果，实际 %+v", result)
			}
			if whitelistReads != 0 {
				t.Fatalf("判冲突应早于读白名单，实际读了 %d 次", whitelistReads)
			}
			if got := runs.blockedCallCount(); got != 0 {
				t.Fatalf("busy 时不应建立 blocked 轮次，实际 %d 次", got)
			}
			if got := collector.callCount(); got != 0 {
				t.Fatalf("busy 时不应调用 Collector.Start，实际 %d 次", got)
			}
			if got := runs.runningCount(); got != 1 {
				t.Fatalf("判冲突应恰好读一次 running，实际 %d 次", got)
			}
		})
	}
}

// TestStartConvergesBeforeCheckingRunning 钉住调用顺序：Converge 必须先于 Running，
// 且拿到的是注入时钟的时刻（Requirement 8.3、design D8）。
func TestStartConvergesBeforeCheckingRunning(t *testing.T) {
	runs := newFakeRunStore()
	runs.setRunning(store.RunSummary{RunID: 14, Status: StatusRunning, Phase: "login"})
	recorder := &callRecorder{}
	runs.recorder = recorder

	var convergeNow time.Time
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Converge: func(_ context.Context, now time.Time) ([]int64, error) {
			convergeNow = now
			recorder.add("converge")
			return nil, nil
		},
		Clock: fixedClock,
	})

	if _, err := svc.Start(context.Background(), model.TriggerManual); !errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("应命中 busy，实际 %v", err)
	}

	calls := recorder.snapshot()
	if len(calls) < 2 {
		t.Fatalf("调用次数不足，实际 %v", calls)
	}
	if calls[0] != "converge" || calls[1] != "running" {
		t.Fatalf("调用顺序应为 converge → running，实际 %v", calls)
	}
	if !convergeNow.Equal(testNow) {
		t.Fatalf("Converge 应收到注入时刻 %s，实际 %s", testNow, convergeNow)
	}
}

// TestStartStopsWhenConvergeFails 覆盖收敛失败：不再读 running、不建轮、不调 Collector。
func TestStartStopsWhenConvergeFails(t *testing.T) {
	convergeErr := errors.New("收敛失败")
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: func(context.Context, time.Time) ([]int64, error) {
			return nil, convergeErr
		},
		Clock: fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, convergeErr) {
		t.Fatalf("应收敛失败原样上抛，实际 %v", err)
	}
	if got := runs.runningCount(); got != 0 {
		t.Fatalf("收敛失败后不应读 running，实际 %d 次", got)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("收敛失败后不应建轮，实际 %d 次", got)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("收敛失败后不应调 Collector，实际 %d 次", got)
	}
}

// TestStartConcurrentAtMostOneCollectorStart 覆盖 10 个并发手动同步：
// 互斥锁让「读 running → 建轮」不互相穿插，Collector.Start 至多被调用一次，
// 恰好一个请求成功，其余全部映射成 409 的 *BusyError（Requirement 8.5、CP11）。
//
// 假 Collector 的建轮路径会阻塞 10ms：没有互斥锁时，已经读过「无 running」的请求会
// 全部挤进 Collector，调用次数必然大于 1（用 -overlay 去掉锁可复现这条断言变红）。
func TestStartConcurrentAtMostOneCollectorStart(t *testing.T) {
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	// 撑开「读 running → 建轮」之间的窗口，让缺锁时不会被一次快速调用掩盖过去。
	collector.startDelay = 10 * time.Millisecond
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	const concurrency = 10
	start := make(chan struct{})
	results := make([]StartResult, concurrency)
	errs := make([]error, concurrency)

	var wg sync.WaitGroup
	for i := range concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = svc.Start(context.Background(), model.TriggerManual)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := collector.callCount(); got != 1 {
		t.Fatalf("并发启动应只调用一次 Collector.Start，实际 %d 次", got)
	}

	successes, busy := 0, 0
	for idx, err := range errs {
		switch {
		case err == nil:
			successes++
			if results[idx].Status != StatusRunning {
				t.Fatalf("成功请求的状态应为 %q，实际 %q", StatusRunning, results[idx].Status)
			}
		case errors.Is(err, model.ErrRunBusy):
			busy++
		default:
			t.Fatalf("第 %d 个并发请求返回了意外错误：%v", idx, err)
		}
	}
	if successes != 1 {
		t.Fatalf("应恰好一个请求启动轮次，实际 %d 个", successes)
	}
	if busy != concurrency-1 {
		t.Fatalf("其余 %d 个请求应全部返回 busy，实际 %d 个", concurrency-1, busy)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("白名单非空时不应建 blocked 轮次，实际 %d 次", got)
	}
}

// TestStartReturnsCollectorUnavailableWithoutCreatingRun 覆盖白名单非空但 Collector
// 未装配：返回 ErrCollectorUnavailable，且 blocked 与采集轮次都是零条（CP16）。
func TestStartReturnsCollectorUnavailableWithoutCreatingRun(t *testing.T) {
	runs := newFakeRunStore()
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	result, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, model.ErrCollectorUnavailable) {
		t.Fatalf("应返回 model.ErrCollectorUnavailable，实际 %v", err)
	}
	if result != (StartResult{}) {
		t.Fatalf("未装配时不应返回启动结果，实际 %+v", result)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("未装配时不应建立 blocked 轮次，实际 %d 次", got)
	}
	if got := runs.runningCount(); got != 1 {
		t.Fatalf("判冲突应恰好读一次 running，实际 %d 次", got)
	}
}

// TestStartMapsCollectorConflictToBusyWhenRunFound 覆盖「Collector 报冲突后重读命中」：
// 说明冲突真实存在，映射成 *BusyError（409），并带着重读到的轮次信息。
func TestStartMapsCollectorConflictToBusyWhenRunFound(t *testing.T) {
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	collector.onStart = func(context.Context, model.TriggerSource) (StartResult, error) {
		runs.setRunning(store.RunSummary{RunID: 17, Status: StatusRunning, Phase: "publish", StartedAt: testNow})
		return StartResult{}, fmt.Errorf("%w: 唯一索引冲突", model.ErrRunConflict)
	}
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("冲突重读命中时应映射成 ErrRunBusy，实际 %v", err)
	}
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("应能 errors.As 取出 *BusyError，实际类型 %T", err)
	}
	if busy.Run.RunID != 17 || busy.Run.Phase != "publish" {
		t.Fatalf("应使用重读到的轮次：run_id=%d phase=%q", busy.Run.RunID, busy.Run.Phase)
	}
	if got := runs.runningCount(); got != 2 {
		t.Fatalf("应读两次 running（判冲突一次、冲突后重读一次），实际 %d 次", got)
	}
}

// TestStartReturnsRetryableErrorWhenConflictRunIsGone 覆盖「冲突窗口内那轮刚好结束」：
// 重读不到 running 时不猜轮次编号，保留 ErrRunConflict 交给上层映射 50000 让管理员重试。
func TestStartReturnsRetryableErrorWhenConflictRunIsGone(t *testing.T) {
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	collector.onStart = func(context.Context, model.TriggerSource) (StartResult, error) {
		return StartResult{}, fmt.Errorf("%w: 唯一索引冲突", model.ErrRunConflict)
	}
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if err == nil {
		t.Fatal("冲突窗口已过时应返回错误而不是成功")
	}
	if errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("读不到进行中轮次时不应映射成 busy，实际 %v", err)
	}
	if !errors.Is(err, model.ErrRunConflict) {
		t.Fatalf("应保留 ErrRunConflict 供排障，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "重试") {
		t.Fatalf("文案应提示管理员重试，实际 %q", err.Error())
	}
	if got := runs.runningCount(); got != 2 {
		t.Fatalf("应读两次 running，实际 %d 次", got)
	}
}

// TestStartReturnsReadErrorWhenConflictRereadFails 覆盖「Collector 报冲突、重读又读库失败」：
// 读库失败比原冲突更能说明问题，错误类别跟着 readErr 走，不改写成 busy，也不重试 Collector。
func TestStartReturnsReadErrorWhenConflictRereadFails(t *testing.T) {
	readErr := errors.New("重读进行中轮次失败")
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	// 判冲突那次 Running 必须成功、重读才失败，所以 runningErr 只能在 Collector 开始后预置。
	collector.onStart = func(context.Context, model.TriggerSource) (StartResult, error) {
		runs.setRunningErr(readErr)
		return StartResult{}, fmt.Errorf("%w: 唯一索引冲突", model.ErrRunConflict)
	}
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, readErr) {
		t.Fatalf("应原样返回重读失败的错误，实际 %v", err)
	}
	if errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("读库失败时不应映射成 busy，实际 %v", err)
	}
	if got := collector.callCount(); got != 1 {
		t.Fatalf("冲突后的重读失败不应重试 Collector，实际调用 %d 次", got)
	}
	if got := runs.runningCount(); got != 2 {
		t.Fatalf("应读两次 running（判冲突一次、冲突后重读一次），实际 %d 次", got)
	}
}

// TestStartPropagatesCollectorFailure 覆盖非冲突的 Collector 失败：原样上抛，
// 不改写成 busy（否则管理员会去等一个并不存在的轮次）。
func TestStartPropagatesCollectorFailure(t *testing.T) {
	collectErr := errors.New("采集启动器失败")
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	collector.onStart = func(context.Context, model.TriggerSource) (StartResult, error) {
		return StartResult{}, collectErr
	}
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, collectErr) {
		t.Fatalf("应原样上抛 Collector 失败，实际 %v", err)
	}
	if errors.Is(err, model.ErrRunBusy) {
		t.Fatalf("普通失败不应映射成 busy，实际 %v", err)
	}
	if got := runs.runningCount(); got != 1 {
		t.Fatalf("非冲突失败不应重读 running，实际读了 %d 次", got)
	}
}

// TestStartPropagatesWhitelistFailure 覆盖读白名单失败：错误上抛，
// 不建立 blocked 轮次、也不调用 Collector。
func TestStartPropagatesWhitelistFailure(t *testing.T) {
	readErr := errors.New("读取设置失败")
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return nil, readErr }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, readErr) {
		t.Fatalf("应原样上抛白名单读取失败，实际 %v", err)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("读白名单失败时不应建 blocked 轮次，实际 %d 次", got)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("读白名单失败时不应调 Collector，实际 %d 次", got)
	}
}

// TestStartPropagatesBlockedRunFailure 覆盖 blocked 建轮失败：错误上抛，
// 不返回半套结果，也不调用 Collector。
func TestStartPropagatesBlockedRunFailure(t *testing.T) {
	storeErr := errors.New("创建阻止轮次失败")
	runs := newFakeRunStore()
	runs.blockedErr = storeErr
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return []model.Building{}, nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	result, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, storeErr) {
		t.Fatalf("应原样上抛建轮失败，实际 %v", err)
	}
	if result != (StartResult{}) {
		t.Fatalf("建轮失败时不应返回结果，实际 %+v", result)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("建轮失败时不应调 Collector，实际 %d 次", got)
	}
}

// TestStartPropagatesRunningReadFailure 覆盖判冲突读库失败：错误上抛，
// 不继续读白名单、不建轮。
func TestStartPropagatesRunningReadFailure(t *testing.T) {
	readErr := errors.New("读取进行中轮次失败")
	runs := newFakeRunStore()
	runs.runningErr = readErr
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	_, err := svc.Start(context.Background(), model.TriggerManual)
	if !errors.Is(err, readErr) {
		t.Fatalf("应原样上抛读库失败，实际 %v", err)
	}
	if got := runs.blockedCallCount(); got != 0 {
		t.Fatalf("读库失败时不应建轮，实际 %d 次", got)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("读库失败时不应调 Collector，实际 %d 次", got)
	}
}

// TestStartCronLogsBusyAndReturnsNil 覆盖定时触发撞上 busy：记一条 INFO
// （msg=cron_skipped_busy，trigger=cron），返回 nil，不重试（Requirement 9.2、CP10）。
func TestStartCronLogsBusyAndReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	runs := newFakeRunStore()
	runs.setRunning(store.RunSummary{
		RunID:     14,
		Status:    StatusRunning,
		Phase:     "week_matrix",
		StartedAt: testNow,
	})
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Log:      capturedLogger(&buf),
		Clock:    fixedClock,
	})

	if err := svc.StartCron(context.Background()); err != nil {
		t.Fatalf("cron 撞上 busy 应返回 nil，实际 %v", err)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("busy 时不应调 Collector，实际 %d 次", got)
	}

	lines := decodeLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("busy 应只记一条日志，实际 %d 条：%v", len(lines), lines)
	}
	line := lines[0]
	assertAllowedLogFields(t, line)
	if line["msg"] != "cron_skipped_busy" {
		t.Fatalf("日志 msg 应为 cron_skipped_busy，实际 %v", line["msg"])
	}
	if line["trigger"] != string(model.TriggerCron) {
		t.Fatalf("日志 trigger 应为 cron，实际 %v", line["trigger"])
	}
	if line["run_id"] != float64(14) {
		t.Fatalf("日志 run_id 应为 14，实际 %v", line["run_id"])
	}
	if line["status"] != StatusRunning {
		t.Fatalf("日志 status 应为 running，实际 %v", line["status"])
	}
}

// TestStartCronStartsRunAndReturnsNil 覆盖定时触发成功：返回 nil，Collector 收到的来源
// 是 cron，且只记一条 collect_run_started（Requirement 9.8、10.3）。
func TestStartCronStartsRunAndReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Log:      capturedLogger(&buf),
		Clock:    fixedClock,
	})

	if err := svc.StartCron(context.Background()); err != nil {
		t.Fatalf("定时触发成功应返回 nil，实际 %v", err)
	}

	sources := collector.recordedSources()
	if len(sources) != 1 || sources[0] != model.TriggerCron {
		t.Fatalf("Collector 应收到一次来源 cron，实际 %v", sources)
	}

	lines := decodeLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("一次 cron 启动应只记一条日志，实际 %d 条：%v", len(lines), lines)
	}
	line := lines[0]
	assertAllowedLogFields(t, line)
	if line["msg"] != "collect_run_started" {
		t.Fatalf("日志 msg 应为 collect_run_started，实际 %v", line["msg"])
	}
	if line["trigger"] != string(model.TriggerCron) {
		t.Fatalf("日志 trigger 应为 cron，实际 %v", line["trigger"])
	}
	if line["status"] != StatusRunning {
		t.Fatalf("日志 status 应为 running，实际 %v", line["status"])
	}
	if line["run_id"] != float64(collector.result.RunID) {
		t.Fatalf("日志 run_id 应取自 Collector 结果 %d，实际 %v", collector.result.RunID, line["run_id"])
	}
	if _, hasReason := line["reason"]; hasReason {
		t.Fatalf("running 日志不应带 reason，实际 %v", line)
	}
}

// TestStartCronBlocksWhenWhitelistEmpty 覆盖「定时触发 + 空白名单」：与手动同步走同一段
// 启动逻辑，建立 blocked 轮次、记一条 collect_run_blocked（trigger=cron）并返回 nil；
// 定时触发不因「无可采集楼」报错，也不调用 Collector（Requirement 9.8、CP10）。
func TestStartCronBlocksWhenWhitelistEmpty(t *testing.T) {
	var buf bytes.Buffer
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return []model.Building{}, nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Log:      capturedLogger(&buf),
		Clock:    fixedClock,
	})

	if err := svc.StartCron(context.Background()); err != nil {
		t.Fatalf("cron 遇到空白名单应返回 nil（blocked 是正常结果），实际 %v", err)
	}
	if got := collector.callCount(); got != 0 {
		t.Fatalf("空白名单不应调用 Collector，实际 %d 次", got)
	}
	if got := runs.blockedCallCount(); got != 1 {
		t.Fatalf("空白名单应建立一条 blocked 轮次，实际 %d 次", got)
	}

	lines := decodeLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("blocked 应只记一条日志，实际 %d 条：%v", len(lines), lines)
	}
	line := lines[0]
	assertAllowedLogFields(t, line)
	if line["msg"] != "collect_run_blocked" {
		t.Fatalf("日志 msg 应为 collect_run_blocked，实际 %v", line["msg"])
	}
	if line["trigger"] != string(model.TriggerCron) {
		t.Fatalf("日志 trigger 应为 cron，实际 %v", line["trigger"])
	}
	if line["status"] != StatusBlocked {
		t.Fatalf("日志 status 应为 blocked，实际 %v", line["status"])
	}
	if line["reason"] != whitelistEmptyReason {
		t.Fatalf("日志 reason 应为固定文案，实际 %v", line["reason"])
	}
	if line["run_id"] != float64(runs.blockedRunID) {
		t.Fatalf("日志 run_id 应取自 blocked 轮次 %d，实际 %v", runs.blockedRunID, line["run_id"])
	}
}

// TestStartCronMapsCollectorConflictToBusyAndReturnsNil 覆盖「cron 撞上 Collector 报
// ErrRunConflict」：重读命中说明确有一轮进行中，因此与「一开始就读到 running」同样处理
// ——记一条 cron_skipped_busy（run_id 取重读到的轮次）并返回 nil，不让调度器把这次冲突
// 当失败去记 WARN（Requirement 9.2、CP10；重读规则见 mapCollectError）。
func TestStartCronMapsCollectorConflictToBusyAndReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	collector.onStart = func(context.Context, model.TriggerSource) (StartResult, error) {
		runs.setRunning(store.RunSummary{RunID: 17, Status: StatusRunning, Phase: "publish", StartedAt: testNow})
		return StartResult{}, fmt.Errorf("%w: 唯一索引冲突", model.ErrRunConflict)
	}
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Log:      capturedLogger(&buf),
		Clock:    fixedClock,
	})

	if err := svc.StartCron(context.Background()); err != nil {
		t.Fatalf("冲突重读到进行中轮次时应返回 nil，实际 %v", err)
	}
	if got := collector.callCount(); got != 1 {
		t.Fatalf("冲突发生在 Collector 内部，应恰好调用一次，实际 %d 次", got)
	}
	// 判冲突读一次 running，冲突后再读一次——两次都成功才说明「重读命中」这条路被走到。
	if got := runs.runningCount(); got != 2 {
		t.Fatalf("应读两次 running（判冲突一次、冲突后重读一次），实际 %d 次", got)
	}

	lines := decodeLogLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("冲突重读命中应只记一条 cron_skipped_busy，实际 %d 条：%v", len(lines), lines)
	}
	line := lines[0]
	assertAllowedLogFields(t, line)
	if line["msg"] != "cron_skipped_busy" {
		t.Fatalf("日志 msg 应为 cron_skipped_busy，实际 %v", line["msg"])
	}
	if line["trigger"] != string(model.TriggerCron) {
		t.Fatalf("日志 trigger 应为 cron，实际 %v", line["trigger"])
	}
	if line["run_id"] != float64(17) {
		t.Fatalf("日志 run_id 应取重读到的轮次 17，实际 %v", line["run_id"])
	}
	if line["status"] != StatusRunning {
		t.Fatalf("日志 status 应为 running，实际 %v", line["status"])
	}
}

// TestStartCronReturnsNonBusyErrors 覆盖定时触发的其它失败：不吞、不改写，
// 由调度器记 WARN 并在下一个计划时刻继续。
func TestStartCronReturnsNonBusyErrors(t *testing.T) {
	runs := newFakeRunStore()
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Converge: emptyConverge,
		Clock:    fixedClock,
	})

	err := svc.StartCron(context.Background())
	if !errors.Is(err, model.ErrCollectorUnavailable) {
		t.Fatalf("未装配时应原样返回 ErrCollectorUnavailable，实际 %v", err)
	}
}

// TestStartPassesThroughCollectorStartedAt 覆盖 running 路径的 started_at：手动同步与定时
// 触发都必须原样透传 Collector 返回的开始时刻，而不是拿注入时钟的 now 顶上。
//
// cron 这条也走 Start 而不是 StartCron：StartCron 只返回 error，结果里的 started_at 根本
// 没被观察；StartCron 内部正是以 model.TriggerCron 调 Start，所以这里与它走同一条路径。
func TestStartPassesThroughCollectorStartedAt(t *testing.T) {
	cases := []struct {
		name   string
		source model.TriggerSource
	}{
		{"手动同步", model.TriggerManual},
		{"定时触发", model.TriggerCron},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := newFakeRunStore()
			collector := newFakeCollector(runs)
			svc := newService(t, Deps{
				Runs:     runs,
				Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
				Collect:  collector,
				Converge: emptyConverge,
				Clock:    fixedClock,
			})

			result, err := svc.Start(context.Background(), tc.source)
			if err != nil {
				t.Fatalf("启动失败: %v", err)
			}
			if result.Status != StatusRunning {
				t.Fatalf("状态应为 %q，实际 %q", StatusRunning, result.Status)
			}
			// collectorStartedAt 刻意偏离注入时钟 testNow：两者相等的话，这条断言区分不了
			// 「透传库里的 started_at」与「把 now 当 started_at」两种写法。
			if !result.StartedAt.Equal(collectorStartedAt) {
				t.Fatalf("running 的 started_at 应原样透传 Collector 结果 %s，实际 %s", collectorStartedAt, result.StartedAt)
			}
			if got := collector.recordedSources(); len(got) != 1 || got[0] != tc.source {
				t.Fatalf("Collector 应收到来源 %q，实际 %v", tc.source, got)
			}
		})
	}
}

// TestStartLogsTriggerAndOnlyAllowedFields 覆盖启动成功与 blocked 两条日志：
// 字段只有 run_id/status/trigger/reason，trigger 按来源取 manual|cron，
// 且 Collector 收到同一个来源（Requirement 9.8、10.3）。
func TestStartLogsTriggerAndOnlyAllowedFields(t *testing.T) {
	cases := []struct {
		name       string
		whitelist  []model.Building
		source     model.TriggerSource
		wantMsg    string
		wantStatus string
		wantReason bool
	}{
		{
			name:       "手动启动成功",
			whitelist:  whitelistOf(),
			source:     model.TriggerManual,
			wantMsg:    "collect_run_started",
			wantStatus: StatusRunning,
		},
		{
			name:       "定时启动成功",
			whitelist:  whitelistOf(),
			source:     model.TriggerCron,
			wantMsg:    "collect_run_started",
			wantStatus: StatusRunning,
		},
		{
			name:       "白名单为空",
			whitelist:  []model.Building{},
			source:     model.TriggerManual,
			wantMsg:    "collect_run_blocked",
			wantStatus: StatusBlocked,
			wantReason: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			runs := newFakeRunStore()
			collector := newFakeCollector(runs)
			svc := newService(t, Deps{
				Runs:     runs,
				Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return tc.whitelist, nil }),
				Collect:  collector,
				Converge: emptyConverge,
				Log:      capturedLogger(&buf),
				Clock:    fixedClock,
			})

			_, err := svc.Start(context.Background(), tc.source)
			if err != nil {
				t.Fatalf("启动失败: %v", err)
			}

			lines := decodeLogLines(t, &buf)
			if len(lines) != 1 {
				t.Fatalf("一次启动应只记一条日志，实际 %d 条：%v", len(lines), lines)
			}
			line := lines[0]
			assertAllowedLogFields(t, line)
			if line["msg"] != tc.wantMsg {
				t.Fatalf("日志 msg 应为 %q，实际 %v", tc.wantMsg, line["msg"])
			}
			if line["trigger"] != string(tc.source) {
				t.Fatalf("日志 trigger 应为 %q，实际 %v", tc.source, line["trigger"])
			}
			if line["status"] != tc.wantStatus {
				t.Fatalf("日志 status 应为 %q，实际 %v", tc.wantStatus, line["status"])
			}

			_, hasReason := line["reason"]
			if hasReason != tc.wantReason {
				t.Fatalf("reason 是否出现不符：期望 %v，实际 %v（%v）", tc.wantReason, hasReason, line)
			}
			if tc.wantReason && line["reason"] != whitelistEmptyReason {
				t.Fatalf("blocked 日志的 reason 应为固定文案，实际 %v", line["reason"])
			}

			if tc.wantStatus == StatusRunning {
				if line["run_id"] != float64(collector.result.RunID) {
					t.Fatalf("成功日志的 run_id 应取自 Collector 结果 %d，实际 %v", collector.result.RunID, line["run_id"])
				}
				sources := collector.recordedSources()
				if len(sources) != 1 || sources[0] != tc.source {
					t.Fatalf("Collector 应收到来源 %q，实际 %v", tc.source, sources)
				}
			} else if got := collector.callCount(); got != 0 {
				t.Fatalf("blocked 路径不应调用 Collector，实际 %d 次", got)
			}
		})
	}
}

// TestStartLogsOutsideLock 钉住「启动日志在锁外」：handler 在 Handle 里用 TryLock 探测
// Service.mu，锁被占即判失败。锁内记日志会让 handler 的 IO（文件、网络）把并发启动
// 排在锁后面——这正是 startLocked 用 defer 解锁、Start 在解锁之后才 logStarted 的原因。
func TestStartLogsOutsideLock(t *testing.T) {
	var buf bytes.Buffer
	probe := &lockProbeHandler{Handler: slog.NewJSONHandler(&buf, nil), t: t}

	runs := newFakeRunStore()
	collector := newFakeCollector(runs)
	svc := newService(t, Deps{
		Runs:     runs,
		Settings: whitelistFunc(func(context.Context) ([]model.Building, error) { return whitelistOf(), nil }),
		Collect:  collector,
		Converge: emptyConverge,
		Log:      slog.New(probe),
		Clock:    fixedClock,
	})
	// handler 在 New 之前就交给了 Deps.Log，那时还没有 Service，组装完成后再回填。
	probe.svc = svc

	if _, err := svc.Start(context.Background(), model.TriggerManual); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if got := len(decodeLogLines(t, &buf)); got != 1 {
		t.Fatalf("一次启动应只记一条日志，实际 %d 条", got)
	}
}
