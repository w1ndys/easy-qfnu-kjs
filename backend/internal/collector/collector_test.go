package collector

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
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// stub 同时实现 Deps 里的全部窄接口。本项只测启动语义与后台收口，用同一个替身把
// 十一个注入点填满，比为「本项用不到的依赖」各写一个空壳更容易看出测试依赖了谁。
//
// 未被本项使用的读写方法一律 panic：将来若有实现开始调用它们，测试会立刻炸在调用点，
// 而不是拿到一个零值后悄悄通过。
type stub struct {
	mu sync.Mutex

	startCalls int              // StartRunningRun 被调用次数
	startCtx   context.Context  // 建轮时拿到的 ctx，用来钉住「请求 ctx 只用于建轮」
	startRun   store.CollectRun // 建轮返回的轮次
	startErr   error            // 建轮返回的错误

	finishes  []store.RunFinish    // FinishRun 收到的收口，按到达顺序
	finishCh  chan store.RunFinish // 可选：收口时额外投递一条，供测试等待
	finishErr error                // FinishRun 返回的错误，用来覆盖「收口本身失败」的分支
}

func (s *stub) StartRunningRun(ctx context.Context, _ time.Time) (store.CollectRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCalls++
	s.startCtx = ctx
	return s.startRun, s.startErr
}

func (s *stub) FinishRun(_ context.Context, finish store.RunFinish) error {
	s.mu.Lock()
	s.finishes = append(s.finishes, finish)
	s.mu.Unlock()
	if s.finishCh != nil {
		s.finishCh <- finish
	}
	return s.finishErr
}

// finishCalls 返回已收到收口的副本：测试在后台协程写日志之后读，仍要拿锁，
// 否则 -race 会把「断言次数」与「append」之间的访问报成数据竞争。
func (s *stub) finishCalls() []store.RunFinish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.RunFinish(nil), s.finishes...)
}

// 以下方法只用于满足接口，本项的路径上不会调用。

func (s *stub) SetRunTerm(_ context.Context, _ int64, _ string, _ time.Time) error {
	panic("stub: 第 5 项不写学期")
}

func (s *stub) SetRunWindow(_ context.Context, _ int64, _, _, _ int, _ time.Time) error {
	panic("stub: 第 5 项不写窗口")
}

func (s *stub) ReplaceTerm(_ context.Context, _ string, _ int, _, _ time.Time) error {
	panic("stub: 第 5 项不替换学期日历")
}

func (s *stub) LoadCollectSnapshot(_ context.Context) (store.CollectSnapshot, error) {
	panic("stub: 第 5 项不读字典与轴快照")
}

func (s *stub) SetRunPhase(_ context.Context, _ int64, _ string, _ []string, _ time.Time) error {
	panic("stub: 第 5 项不推进阶段")
}

func (s *stub) LoadRunPhase(_ context.Context, _ int64) (string, string, error) {
	panic("stub: 第 5 项不读阶段")
}

func (s *stub) RecordRunErrorCode(_ context.Context, _ int64, _ string, _ time.Time) error {
	panic("stub: 第 5 项不写错误码")
}

func (s *stub) LoadRunTaskSummary(_ context.Context, _ int64) (store.RunTaskSummary, error) {
	panic("stub: 第 5 项不读任务计数")
}

func (s *stub) EnsureWeekMatrixTasks(_ context.Context, _ int64, _ []store.SyncTaskInput, _ time.Time) ([]store.SyncTask, error) {
	panic("stub: 第 5 项不落任务")
}

func (s *stub) EnsureCellDetailTasks(_ context.Context, _ int64, _ []store.SyncTaskInput, _ time.Time) ([]store.SyncTask, error) {
	panic("stub: 第 5 项不落任务")
}

func (s *stub) RecordWeekFetch(_ context.Context, _ store.RunWeekFetch) error {
	panic("stub: 第 5 项不写周结果")
}

func (s *stub) RecordWeekFailure(_ context.Context, _ store.RunWeekFailure) error {
	panic("stub: 第 5 项不写周结果")
}

func (s *stub) CompleteSyncTask(_ context.Context, _, _ int64, _ time.Time) error {
	panic("stub: 第 5 项不改任务状态")
}

func (s *stub) FailSyncTask(_ context.Context, _, _ int64, _ string, _ time.Time) error {
	panic("stub: 第 5 项不改任务状态")
}

func (s *stub) LoadWeekMatrixTasks(_ context.Context, _ int64) ([]store.SyncTask, error) {
	panic("stub: 第 5 项不做 hydrate")
}

func (s *stub) UpsertRoomAttribution(_ context.Context, _ store.RoomAttribution) error {
	panic("stub: 第 5 项不写归属")
}

func (s *stub) UpsertRunRooms(_ context.Context, _ int64, _ []store.RunRoom, _ time.Time) error {
	panic("stub: 第 5 项不写归属")
}

func (s *stub) LoadRunRooms(_ context.Context, _ int64) ([]store.RunRoom, error) {
	panic("stub: 第 5 项不读归属")
}

func (s *stub) RecordAttrConflicts(_ context.Context, _ int64, _ int, _ time.Time) error {
	panic("stub: 第 5 项不写冲突数")
}

func (s *stub) LoadRunFacts(_ context.Context, _ int64) (store.RunFacts, error) {
	panic("stub: 第 5 项不组装发布")
}

func (s *stub) CommitRelease(_ context.Context, _ store.ReleaseDraft) error {
	panic("stub: 第 5 项不发布")
}

func (s *stub) Load(_ context.Context) (settings.Snapshot, error) {
	panic("stub: 第 5 项不读设置")
}

func (s *stub) Reveal(_ context.Context, _ string) (string, error) {
	panic("stub: 第 5 项不解封密码")
}

func (s *stub) Login(_ context.Context, _ upstream.LoginOptions) error {
	panic("stub: 第 5 项不登录")
}

func (s *stub) FetchTerm(_ context.Context) (*upstream.TermSnapshot, error) {
	panic("stub: 第 5 项不读学期页")
}

func (s *stub) FetchWeekCalendar(_ context.Context, _ time.Time) (*upstream.WeekCalendar, error) {
	panic("stub: 第 5 项不读周历页")
}

func (s *stub) FetchBuildings(_ context.Context, _ string) ([]upstream.Building, error) {
	panic("stub: 第 5 项不读楼列表")
}

func (s *stub) FetchWeekMatrix(_ context.Context, _ upstream.WeekMatrixParams) ([]byte, error) {
	panic("stub: 第 5 项不抓矩阵")
}

func (s *stub) FetchOccupancyDetail(_ context.Context, _ upstream.OccupancyDetailParams) ([]byte, error) {
	panic("stub: 第 5 项不抓明细")
}

// validDeps 返回一份除 exec/Clock 外都填满的 Deps：New 的必填校验全部通过，
// Clock 留空以顺带覆盖「nil 退回 time.Now」。
func validDeps(s *stub) Deps {
	return Deps{
		Runs:      s,
		Terms:     s,
		Snapshots: s,
		Phases:    s,
		Tasks:     s,
		TaskRead:  s,
		Rooms:     s,
		Reader:    s,
		Committer: s,
		Settings:  s,
		Upstream:  s,
		RunCtx:    context.Background(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// backgroundStartGrace 是「确认后台没有被启动」的有界等待窗口：见 TestStartReturnsRunConflict。
const backgroundStartGrace = 100 * time.Millisecond

// TestNewRejectsMissingDependencies 逐项清空 Deps，断言缺一即拒绝启动并点名依赖。
//
// 十三个必填项各一条：少校验一个，装配层就会把「运行期 panic」当成配置错误的替代品。
func TestNewRejectsMissingDependencies(t *testing.T) {
	cases := []struct {
		name  string // 子用例名
		field string // 期望出现在错误文案里的字段名
		clear func(*Deps)
	}{
		{"缺少 Runs", "Runs", func(d *Deps) { d.Runs = nil }},
		{"缺少 Terms", "Terms", func(d *Deps) { d.Terms = nil }},
		{"缺少 Snapshots", "Snapshots", func(d *Deps) { d.Snapshots = nil }},
		{"缺少 Phases", "Phases", func(d *Deps) { d.Phases = nil }},
		{"缺少 Tasks", "Tasks", func(d *Deps) { d.Tasks = nil }},
		{"缺少 TaskRead", "TaskRead", func(d *Deps) { d.TaskRead = nil }},
		{"缺少 Rooms", "Rooms", func(d *Deps) { d.Rooms = nil }},
		{"缺少 Reader", "Reader", func(d *Deps) { d.Reader = nil }},
		{"缺少 Committer", "Committer", func(d *Deps) { d.Committer = nil }},
		{"缺少 Settings", "Settings", func(d *Deps) { d.Settings = nil }},
		{"缺少 Upstream", "Upstream", func(d *Deps) { d.Upstream = nil }},
		{"缺少 RunCtx", "RunCtx", func(d *Deps) { d.RunCtx = nil }},
		{"缺少 Log", "Log", func(d *Deps) { d.Log = nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := validDeps(&stub{})
			tc.clear(&deps)

			c, err := New(deps)
			if c != nil {
				t.Fatalf("缺 %s 时 New 应返回 nil 执行器，实际返回非 nil", tc.field)
			}
			if !errors.Is(err, model.ErrConfigInvalid) {
				t.Fatalf("缺 %s 时应包装 model.ErrConfigInvalid，实际 %v", tc.field, err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("错误文案应点名缺失的依赖 %s，实际 %q", tc.field, err.Error())
			}
		})
	}
}

// TestNewAcceptsDepsWithoutClock 钉住唯一的可缺省项：Clock 为 nil 不报错，退回 time.Now。
func TestNewAcceptsDepsWithoutClock(t *testing.T) {
	c, err := New(validDeps(&stub{}))
	if err != nil {
		t.Fatalf("除 Clock 外依赖齐备时 New 不应报错，实际 %v", err)
	}
	if c.clock == nil {
		t.Fatal("Clock 为 nil 时应退回 time.Now，不能留成 nil（下一次 Start 会 panic）")
	}
}

// TestStartReturnsRunConflict 钉住唯一索引冲突的传播：建轮失败原样返回包装了
// model.ErrRunConflict 的错误，不建后台协程、不返回任何轮次身份。
func TestStartReturnsRunConflict(t *testing.T) {
	s := &stub{startErr: fmt.Errorf("%w: 已有进行中轮次", model.ErrRunConflict)}
	deps := validDeps(s)
	execCalled := make(chan struct{}, 1)
	deps.exec = func(_ context.Context, _ int64) error {
		execCalled <- struct{}{}
		return nil
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	result, startErr := c.Start(context.Background(), model.TriggerManual)
	if !errors.Is(startErr, model.ErrRunConflict) {
		t.Fatalf("冲突应保留 model.ErrRunConflict，实际 %v", startErr)
	}
	if result != (runner.StartResult{}) {
		t.Fatalf("冲突时不应返回轮次身份，实际 %+v", result)
	}
	if s.startCalls != 1 {
		t.Fatalf("建轮应调用一次 StartRunningRun，实际 %d 次", s.startCalls)
	}

	// 建轮失败是一条同步路径，后台协程根本不该被启动；但「没被启动」只能靠等一个
	// 有界窗口来证明：错写成 go c.run(...) 时，goroutine 调度可能晚于本用例返回，
	// 一次非阻塞 select 恰好会放过这种错误。100ms 足够让任何被误启动的 goroutine
	// 跑到 execCalled，又不会把用例拖慢到能被察觉。
	select {
	case <-execCalled:
		t.Fatal("建轮失败后不应启动后台执行体")
	case <-time.After(backgroundStartGrace):
	}
}

// TestStartReturnsBeforeBackgroundFinishes 钉住 D2：Start 在后台跑完前返回 running。
//
// 执行体卡在 release 上，所以「Start 已返回」与「后台未完成」可以同时成立；
// 若 Start 等后台，done 不会在超时前收到，用例会失败而不是永久挂住。
func TestStartReturnsBeforeBackgroundFinishes(t *testing.T) {
	startedAt := time.Date(2026, 11, 9, 10, 0, 0, 0, shanghai)
	s := &stub{startRun: store.CollectRun{RunID: 41, StartedAt: startedAt, Status: runner.StatusRunning}}

	entered := make(chan struct{})  // 执行体已进入
	release := make(chan struct{})  // 测试允许执行体结束
	finished := make(chan struct{}) // 执行体已结束

	deps := validDeps(s)
	deps.exec = func(_ context.Context, runID int64) error {
		if runID != 41 {
			return fmt.Errorf("后台收到 run_id=%d，期望 41", runID)
		}
		close(entered)
		<-release
		close(finished)
		return nil
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	type startOutcome struct {
		result runner.StartResult
		err    error
	}
	done := make(chan startOutcome, 1)
	go func() {
		result, startErr := c.Start(context.Background(), model.TriggerManual)
		done <- startOutcome{result: result, err: startErr}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("后台执行体没有被调用")
	}

	var outcome startOutcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 在后台执行完成前没有返回")
	}
	if outcome.err != nil {
		t.Fatalf("Start 不应报错，实际 %v", outcome.err)
	}
	if outcome.result.RunID != 41 || outcome.result.Status != runner.StatusRunning {
		t.Fatalf("Start 结果应为 {RunID:41, Status:running}，实际 %+v", outcome.result)
	}
	if !outcome.result.StartedAt.Equal(startedAt) {
		t.Fatalf("StartedAt 应取库里返回的时刻 %v，实际 %v", startedAt, outcome.result.StartedAt)
	}

	select {
	case <-finished:
		t.Fatal("Start 返回时后台已经跑完，无法证明「先返回、再执行」")
	default:
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("放行后后台执行体没有结束")
	}
}

// TestStartPropagatesRunCtxToBackgroundAfterRequestCancel 只钉「ctx 传播」这一件事：
// 请求 ctx 只传进 StartRunningRun，后台执行体拿到的是 Deps.RunCtx，请求结束并取消后
// 它仍是活的（CP11 的前半句）。
//
// 这里不断言「请求取消后轮次仍跑到真实终态」——那需要第 6–10 项的执行体先把成功发布
// 路径写出来；本用例里的 FinishRun 是替身执行体自己收口，只用来证明「后台手里的 ctx
// 还能写库」，不是执行器的收口行为。真实终态的覆盖要等第 6–10 项。
//
// 执行体等取消发生之后才检查自己手里的 ctx：如果它拿到的是请求 ctx，ctx.Err() 已是
// context.Canceled，用例会看到后台被取消；如果拿错上下文源（比如另起一个
// context.Background），RunCtx 里埋的值读不出来，同样会失败。
func TestStartPropagatesRunCtxToBackgroundAfterRequestCancel(t *testing.T) {
	type ctxKey struct{}

	startedAt := time.Date(2026, 11, 9, 10, 0, 0, 0, shanghai)
	s := &stub{
		startRun: store.CollectRun{RunID: 42, StartedAt: startedAt, Status: runner.StatusRunning},
		finishCh: make(chan store.RunFinish, 1),
	}

	runCtx := context.WithValue(context.Background(), ctxKey{}, "run-ctx")
	requestCtx, cancelRequest := context.WithCancel(context.Background())

	proceed := make(chan struct{})
	execResult := make(chan error, 1)

	deps := validDeps(s)
	deps.RunCtx = runCtx
	deps.exec = func(ctx context.Context, runID int64) error {
		<-proceed
		if got, _ := ctx.Value(ctxKey{}).(string); got != "run-ctx" {
			err := fmt.Errorf("后台执行体的上下文不是 Deps.RunCtx（拿到的标记是 %q）", got)
			execResult <- err
			return err
		}
		if err := ctx.Err(); err != nil {
			execResult <- err
			return err
		}
		execResult <- nil
		return s.FinishRun(ctx, store.RunFinish{
			RunID:      runID,
			Status:     "success",
			FinishedAt: startedAt,
		})
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	result, err := c.Start(requestCtx, model.TriggerManual)
	if err != nil {
		t.Fatalf("Start 不应报错，实际 %v", err)
	}
	if result.Status != runner.StatusRunning {
		t.Fatalf("Start 应返回 running，实际 %q", result.Status)
	}
	if s.startCtx != requestCtx {
		t.Fatal("建轮应使用请求 ctx（D9：请求 ctx 的唯一用途就是这一次写库）")
	}

	// 请求结束后取消请求 ctx，再放行后台执行体。
	cancelRequest()
	close(proceed)

	select {
	case execErr := <-execResult:
		if execErr != nil {
			t.Fatalf("后台执行体的 ctx 不应随请求 ctx 一起取消，实际 %v", execErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("后台执行体没有结束")
	}

	finish := waitFinish(t, s.finishCh)
	if finish.RunID != 42 {
		t.Fatalf("收口的轮次应为 42，实际 %d", finish.RunID)
	}
	if finish.Status != "success" {
		t.Fatalf("替身执行体在请求 ctx 取消后仍应能写收口（说明拿到的是 RunCtx），实际 status %q", finish.Status)
	}
}

// TestRunCtxCancelLeavesRunRunning 钉住「取消不收口」（CP8、Requirement 10.4）：
// Deps.RunCtx 取消后执行体返回 ctx.Err()，执行器不写任何终态——没有 FinishRun 调用，
// 轮次留在 running，未完成的任务留给下一次执行。
func TestRunCtxCancelLeavesRunRunning(t *testing.T) {
	startedAt := time.Date(2026, 11, 9, 10, 0, 0, 0, shanghai)
	s := &stub{
		startRun: store.CollectRun{RunID: 43, StartedAt: startedAt, Status: runner.StatusRunning},
		finishCh: make(chan store.RunFinish, 1),
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	logs := &safeBuffer{}
	execResult := make(chan error, 1)

	deps := validDeps(s)
	deps.RunCtx = runCtx
	deps.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	deps.exec = func(ctx context.Context, _ int64) error {
		<-ctx.Done()
		err := ctx.Err()
		execResult <- err
		return err
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	if _, err := c.Start(context.Background(), model.TriggerManual); err != nil {
		t.Fatalf("Start 不应报错，实际 %v", err)
	}

	cancelRun()
	select {
	case execErr := <-execResult:
		if !errors.Is(execErr, context.Canceled) {
			t.Fatalf("执行体应从 RunCtx 拿到 context.Canceled，实际 %v", execErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunCtx 取消后后台执行体没有退出")
	}

	// 执行体返回后 run 还会记一条 WARN；等到它出现，说明后台协程已经走完「错误只记日志、
	// 不替执行体收口」这条路径，此时再断言「一次 FinishRun 都没发生」才是确定的。
	waitLog(t, logs, "collect_run_exec_failed")

	if finishes := s.finishCalls(); len(finishes) != 0 {
		t.Fatalf("RunCtx 取消后不应写终态（轮次应保持 running），实际收口 %+v", finishes)
	}
	select {
	case finish := <-s.finishCh:
		t.Fatalf("RunCtx 取消后不应写终态，实际收到收口 %+v", finish)
	default:
	}
}

// TestStartBackgroundPanicFailsRun 钉住 Requirement 1.4 与 design 的后台 panic 行：
// panic 被 recover，轮次无码收口为 failed，原因固定「内部错误」，并记一条 ERROR。
func TestStartBackgroundPanicFailsRun(t *testing.T) {
	startedAt := time.Date(2026, 11, 9, 10, 0, 0, 0, shanghai)
	s := &stub{
		startRun: store.CollectRun{RunID: 7, StartedAt: startedAt, Status: runner.StatusRunning},
		finishCh: make(chan store.RunFinish, 1),
	}

	logs := &safeBuffer{}
	deps := validDeps(s)
	deps.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	deps.exec = func(_ context.Context, _ int64) error {
		panic("矩阵页越界")
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	if _, err := c.Start(context.Background(), model.TriggerManual); err != nil {
		t.Fatalf("Start 不应报错（panic 发生在后台），实际 %v", err)
	}

	finish := waitFinish(t, s.finishCh)
	if finish.RunID != 7 {
		t.Fatalf("收口的轮次应为 7，实际 %d", finish.RunID)
	}
	if finish.Status != "failed" {
		t.Fatalf("panic 后轮次应为 failed，实际 %q", finish.Status)
	}
	if finish.ErrorCode != "" {
		t.Fatalf("panic 不应写业务错误码，实际 %q", finish.ErrorCode)
	}
	if finish.ErrorMessage != errPanic.Error() {
		t.Fatalf("panic 的原因应为固定文案 %q，实际 %q", errPanic.Error(), finish.ErrorMessage)
	}
	if finish.FinishedAt.IsZero() {
		t.Fatal("panic 收口应写 finished_at，实际是零值")
	}

	waitLog(t, logs, "collect_run_panic")

	// 一次 panic 只对应一条 ERROR：多记会因为「日志出现 == 终态已写」的等价关系被
	// 误读成多次收口，少记则等于 panic 静默丢失。同时确认没混进别的 ERROR 行
	// （例如收口失败时漏判 err），否则「只有一条」这条断言会被无关行掩盖。
	errorLines := errorLogRecords(t, logs.String())
	panicLines, otherLines := 0, 0
	for _, record := range errorLines {
		if record["msg"] == "collect_run_panic" {
			panicLines++
		} else {
			otherLines++
		}
	}
	if panicLines != 1 {
		t.Fatalf("panic 应恰好记一条 msg=collect_run_panic 的 ERROR，实际 %d 条（全部 ERROR 行 %+v）", panicLines, errorLines)
	}
	if otherLines != 0 {
		t.Fatalf("panic 用例不应出现其它 ERROR 行，实际 %d 条（全部 ERROR 行 %+v）", otherLines, errorLines)
	}

	// panic 字段只记「类型名: 截断值」，原始载荷不整段照抄（CP12 的 panic/err 例外）。
	if got := errorLines[0]["panic"]; got != "string: 矩阵页越界" {
		t.Fatalf("panic 字段应为「类型名: 值」，期望 %q，实际 %v", "string: 矩阵页越界", got)
	}
}

// TestStartBackgroundPanicFinishFailureLogsErr 钉住 failOnPanic 的「收口失败」分支：
// FailRun 自己返回错误时，日志仍只有一条 ERROR——仍是 msg=collect_run_panic，只是多一个
// err 字段——并且不会为了补救再收口一次。
//
// 用 err 字段而不是第二条日志：收口失败意味着「库里可能还是 running」，这条 ERROR 的语义
// 从「终态已写」变成「终态没写成」，排障必须按 err 判断；多记一条 ERROR 会把「一次 panic」
// 读成「两次收口」。不重试是因为终态只写一次，第二次只会撞 FinishRun 的 status='running'
// 守卫。
func TestStartBackgroundPanicFinishFailureLogsErr(t *testing.T) {
	// 哨兵只出现在 FailRun 返回的错误里：它出现在日志的 err 字段，才能证明写的是这个错误，
	// 而不是任意一条收口失败的文案。
	const finishSentinel = "收口失败：连接已断开"

	startedAt := time.Date(2026, 11, 9, 10, 0, 0, 0, shanghai)
	s := &stub{
		startRun:  store.CollectRun{RunID: 8, StartedAt: startedAt, Status: runner.StatusRunning},
		finishErr: errors.New(finishSentinel),
	}

	logs := &safeBuffer{}
	deps := validDeps(s)
	deps.Log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelError}))
	deps.exec = func(_ context.Context, _ int64) error {
		panic("矩阵页越界")
	}

	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}

	if _, err := c.Start(context.Background(), model.TriggerManual); err != nil {
		t.Fatalf("Start 不应报错（panic 发生在后台），实际 %v", err)
	}

	// failOnPanic 先收口再记日志，所以日志一出现就说明整条 panic 路径已经走完：此时读
	// finishes 既能断言「只收口一次」，也不会与后台协程抢时序。
	waitLog(t, logs, "collect_run_panic")

	errorLines := errorLogRecords(t, logs.String())
	if len(errorLines) != 1 {
		t.Fatalf("收口失败时也应恰好一条 ERROR，实际 %d 条（全部 ERROR 行 %+v）", len(errorLines), errorLines)
	}
	record := errorLines[0]
	if record["msg"] != "collect_run_panic" {
		t.Fatalf("唯一一条 ERROR 应是 collect_run_panic，实际 %v", record["msg"])
	}
	if got := record["err"]; got != finishSentinel {
		t.Fatalf("收口失败时 err 字段应写 FailRun 返回的错误 %q，实际 %v", finishSentinel, got)
	}
	if got := record["panic"]; got != "string: 矩阵页越界" {
		t.Fatalf("panic 字段应照常记「类型名: 值」，期望 %q，实际 %v", "string: 矩阵页越界", got)
	}

	// 收口失败不重试：替身只该收到一次 FinishRun，多一次就是第二次收口。
	if finishes := s.finishCalls(); len(finishes) != 1 {
		t.Fatalf("收口失败后不应再收口一次，实际调用 %d 次：%+v", len(finishes), finishes)
	}
}

// TestPanicLogValueTruncates 钉住 panic 值的收窄口径：类型名照留，值按 rune 截到
// maxPanicLogRunes 个字符，绝不整段照抄可能来自页面的载荷（CP12 的 panic/err 例外）。
func TestPanicLogValueTruncates(t *testing.T) {
	long := strings.Repeat("页", maxPanicLogRunes+50)

	got := panicLogValue(long)
	if !strings.HasPrefix(got, "string: ") {
		t.Fatalf("panic 日志值应以类型名开头，实际 %q", got)
	}
	value := strings.TrimPrefix(got, "string: ")
	if n := len([]rune(value)); n != maxPanicLogRunes {
		t.Fatalf("panic 值应截断到 %d 个字符，实际 %d 个", maxPanicLogRunes, n)
	}
	if strings.Contains(got, long) {
		t.Fatal("panic 值超长时不应整段照抄")
	}
}

// waitFinish 等一条收口记录；后台协程没写终态时失败而不是永久挂住。
func waitFinish(t *testing.T, ch <-chan store.RunFinish) store.RunFinish {
	t.Helper()
	select {
	case finish := <-ch:
		return finish
	case <-time.After(2 * time.Second):
		t.Fatal("后台执行体没有把轮次收口")
		return store.RunFinish{}
	}
}

// waitLog 轮询到日志里出现 needle 为止。收口与记日志在同一个后台协程里先后完成，
// 断言只能等日志落地，不能假定它已经写完。
func waitLog(t *testing.T, logs *safeBuffer, needle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), needle) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("日志里没有出现 %q，实际 %q", needle, logs.String())
}

// errorLogRecords 把 JSON 日志按行解析，只返回 level=ERROR 的记录。
//
// 用解析而不是对本段文本做 Contains：文本包含分不清「一条还是两条 ERROR」，
// 也读不出 panic 字段究竟被写成了什么。
func errorLogRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("日志行不是合法 JSON：%q（%v）", line, err)
		}
		if record["level"] == "ERROR" {
			records = append(records, record)
		}
	}
	return records
}

// safeBuffer 是并发安全的日志缓冲：后台协程写、测试协程读，必须加锁。
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 编译期确认 stub 覆盖 Deps 的全部窄接口：少一个方法时这里先报错，而不是在某个
// 用例的 Deps 字面量里报「cannot use s (variable of type *stub)」。
var (
	_ RunLifecycle        = (*stub)(nil)
	_ TermCalendar        = (*stub)(nil)
	_ SnapshotReader      = (*stub)(nil)
	_ taskReader          = (*stub)(nil)
	_ upstreamAPI         = (*stub)(nil)
	_ settingsSource      = (*stub)(nil)
	_ pipeline.PhaseStore = (*stub)(nil)
	_ pipeline.TaskStore  = (*stub)(nil)
	_ pipeline.RoomStore  = (*stub)(nil)
	_ publish.Reader      = (*stub)(nil)
	_ publish.Committer   = (*stub)(nil)
)
