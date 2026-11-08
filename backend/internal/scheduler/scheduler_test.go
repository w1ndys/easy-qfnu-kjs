package scheduler

// 本文件用假时钟 + 注入的 wait + 替身表达式来源覆盖调度循环。所有用例都不靠真实时间
// 到点，也不依赖运行当天的日期：时钟停在 2026-11-07（周六）04:00，默认表达式
// `10 4 * * 6` 的下一次触发就是当天 04:10，时长恰为 10 分钟。
//
// 断言的重点是「触发几次、按哪个表达式、什么时候退出」：
//   - 到点只触发一次，且触发 ctx 带 30 秒超时；
//   - Reload 后按新表达式重新计时，非法 Reload 保留旧计划；
//   - Reload 不把 next 排回已经触发过的分钟（CP9），也不覆盖更晚的 Reload（启动装载）；
//   - 启动期存量表达式非法/缺失/读库失败都回落 DefaultCronExpr；
//   - 触发失败只记 WARN，下一个计划时刻照常触发；
//   - ctx 取消后 Run 返回且不再触发；并发第二次 Run 立刻返回，计划只触发一次。
//
// 替身 wait 把每次「准备等多久」连同自己的定时器通道交出来，用例决定何时到点：
// 时钟推进多少、往哪个通道发信号，都由用例显式写出来，到点时机不依赖系统时间。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/cronexpr"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testStart 是注入的固定起点：周六 04:00（业务时区），默认表达式下一次触发在 04:10。
var testStart = time.Date(2026, 11, 7, 4, 0, 0, 0, model.BusinessLocation())

// fakeClock 是可推进的假时钟。mu 保护 now：Run 在另一个协程里读，用例在主协程里推进。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把时钟往后拨 d。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 把时钟拨到指定时刻。用例用它模拟「推算用的时钟快照早于实际触发时刻」——
// 锁外读时钟的旧实现、或机器时钟被 NTP 回拨，都会让下一次推算落回已经触发过的分钟。
func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

// waitRequest 是一次注入 wait 的入参与定时器通道：duration 是要等多久，ch 由用例
// 手动送达来表示「到点」。
type waitRequest struct {
	duration time.Duration
	ch       chan time.Time
}

// fakeWait 是 time.After 的替身：把请求放进带缓冲通道，用例按顺序取。
type fakeWait struct {
	requests chan waitRequest
}

func newFakeWait() *fakeWait {
	return &fakeWait{requests: make(chan waitRequest, 32)}
}

// Wait 记录时长并返回一个只有测试会写的时间通道。
func (w *fakeWait) Wait(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	w.requests <- waitRequest{duration: d, ch: ch}
	return ch
}

// next 取下一次等待请求；2 秒内没出现视为用例失败。
func (w *fakeWait) next(t *testing.T) waitRequest {
	t.Helper()
	select {
	case req := <-w.requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("等待调度器进入 wait 超时")
		return waitRequest{}
	}
}

// assertNone 断言此刻没有待处理的等待请求。用于「没有下一次触发时不该空转」。
func (w *fakeWait) assertNone(t *testing.T) {
	t.Helper()
	select {
	case req := <-w.requests:
		t.Fatalf("不应发起等待，却收到 duration=%v 的请求", req.duration)
	default:
	}
}

// triggerCall 是一次触发时观察到的 ctx 信息：deadline 用来钉住 30 秒超时，
// receivedAt 是进入触发器的时刻，两者相减不受用例断言前的调度延迟影响。
type triggerCall struct {
	deadline    time.Time
	hasDeadline bool
	receivedAt  time.Time
}

// fakeTrigger 是 Trigger 的替身：按顺序返回预设错误，并记录每次调用的 ctx 截止时间。
type fakeTrigger struct {
	mu     sync.Mutex
	calls  []triggerCall
	errs   []error
	signal chan struct{}
}

func newFakeTrigger() *fakeTrigger {
	return &fakeTrigger{signal: make(chan struct{}, 64)}
}

// invoke 作为 Trigger 注入；调用记录与错误出队都在锁内完成。
func (f *fakeTrigger) invoke(ctx context.Context) error {
	call := triggerCall{receivedAt: time.Now()}
	call.deadline, call.hasDeadline = ctx.Deadline()

	f.mu.Lock()
	f.calls = append(f.calls, call)
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	f.mu.Unlock()

	select {
	case f.signal <- struct{}{}:
	default:
	}
	return err
}

// count 返回已触发次数。
func (f *fakeTrigger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// call 返回第 i 次触发的记录。
func (f *fakeTrigger) call(t *testing.T, i int) triggerCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.calls) {
		t.Fatalf("第 %d 次触发不存在，当前只有 %d 次", i+1, len(f.calls))
	}
	return f.calls[i]
}

// await 等到至少发生 n 次触发；超时视为用例失败。
func (f *fakeTrigger) await(t *testing.T, n int) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		if f.count() >= n {
			return
		}
		select {
		case <-f.signal:
		case <-timeout:
			t.Fatalf("等待第 %d 次触发超时，当前 %d 次", n, f.count())
		}
	}
}

// fakeExprSource 是 ExprSource 的替身：返回固定表达式或错误，并记录被读了几次。
type fakeExprSource struct {
	mu    sync.Mutex
	expr  string
	err   error
	calls int
}

func (f *fakeExprSource) CronExpr(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.expr, f.err
}

// syncBuffer 是并发安全的日志缓冲：Run 在另一个协程里写，用例在主协程里读。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestLogger 造一个写进 buf 的 JSON logger；buf 为 nil 时丢弃输出。
func newTestLogger(buf io.Writer) *slog.Logger {
	if buf == nil {
		buf = io.Discard
	}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// startRun 在后台启动调度循环，返回取消函数与退出信号。
func startRun(t *testing.T, s *Scheduler) (context.Context, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()
	return ctx, cancel, done
}

// awaitDone 等 Run 返回；超时视为用例失败。
func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("等待调度循环退出超时")
	}
}

// eventually 轮询等到 cond 为真，用于等一条日志落进缓冲。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", what)
}

// TestNewDefaultsDependencies 覆盖构造期的两个默认值：clock 为 nil 退回 time.Now、
// wait 为 nil 退回 time.After，同时默认表达式在构造后就有一份可用的计划。
func TestNewDefaultsDependencies(t *testing.T) {
	trigger := newFakeTrigger()
	clock := newFakeClock(testStart)

	s := New(newTestLogger(nil), trigger.invoke, nil, clock.Now, nil)
	if s.clock == nil {
		t.Fatal("clock 为 nil 时应退回 time.Now")
	}
	if s.wait == nil {
		t.Fatal("wait 为 nil 时应退回 time.After")
	}

	next, ok := s.Next()
	if !ok {
		t.Fatal("构造后默认表达式应能算出下一次触发")
	}
	if want := testStart.Add(10 * time.Minute); !next.Equal(want) {
		t.Fatalf("默认表达式下一次触发 = %v，期望 %v", next, want)
	}

	s2 := New(newTestLogger(nil), trigger.invoke, nil, nil, time.After)
	if s2.clock == nil {
		t.Fatal("clock 为 nil 时应退回 time.Now")
	}
}

// TestNewWithoutTriggerDoesNotPanic 覆盖未装配触发器的兜底：构造期记 ERROR，
// 到点只会得到一条 WARN，不会因为 nil 函数调用把进程打挂。
func TestNewWithoutTriggerDoesNotPanic(t *testing.T) {
	buf := &syncBuffer{}
	s := New(newTestLogger(buf), nil, &fakeExprSource{expr: model.DefaultCronExpr}, newFakeClock(testStart).Now, newFakeWait().Wait)

	s.fire(t.Context())

	logs := buf.String()
	if !strings.Contains(logs, "未配置触发器") {
		t.Fatalf("未装配触发器应记日志，实际日志: %s", logs)
	}
}

// TestFireSkipsCanceledContext 覆盖关停与到点同时就绪的兜底：ctx 已取消时 fire
// 不再调用触发器，避免关停过程中多建一轮（Requirement 9.7）。
func TestFireSkipsCanceledContext(t *testing.T) {
	trigger := newFakeTrigger()
	s := New(newTestLogger(nil), trigger.invoke, &fakeExprSource{expr: model.DefaultCronExpr}, newFakeClock(testStart).Now, newFakeWait().Wait)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.fire(ctx)

	if got := trigger.count(); got != 0 {
		t.Fatalf("ctx 已取消时不应触发，实际 %d 次", got)
	}
}

// TestRunWithoutExprSourceUsesDefault 覆盖未装配表达式来源：记一条 WARN 后用
// DefaultCronExpr 继续，循环仍然可用。
func TestRunWithoutExprSourceUsesDefault(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	s := New(newTestLogger(buf), trigger.invoke, nil, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	if req := wait.next(t); req.duration != 10*time.Minute {
		t.Fatalf("未装配来源后的等待 %v，期望 10m", req.duration)
	}
	if logs := buf.String(); !strings.Contains(logs, "未配置表达式来源") {
		t.Fatalf("未装配来源应记 WARN，实际日志: %s", logs)
	}
}

// TestRunTriggersOnceAtScheduledMinute 覆盖到点触发一次、30 秒超时与触发后的重算：
// 04:00 挂起等 10 分钟，到点触发一次，随后按 04:10 重算到下一个周六同一时刻。
func TestRunTriggersOnceAtScheduledMinute(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	src := &fakeExprSource{expr: model.DefaultCronExpr}
	s := New(newTestLogger(nil), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	req := wait.next(t)
	if req.duration != 10*time.Minute {
		t.Fatalf("首次等待 %v，期望 10m", req.duration)
	}
	if got := trigger.count(); got != 0 {
		t.Fatalf("未到点不应触发，实际 %d 次", got)
	}

	next, ok := s.Next()
	if !ok || !next.Equal(testStart.Add(10*time.Minute)) {
		t.Fatalf("Next() = %v, %v，期望 04:10", next, ok)
	}

	clock.Advance(10 * time.Minute)
	req.ch <- clock.Now()

	trigger.await(t, 1)
	call := trigger.call(t, 0)
	if !call.hasDeadline {
		t.Fatal("触发 ctx 应带 30 秒超时")
	}
	if budget := call.deadline.Sub(call.receivedAt); budget > triggerTimeout || budget < triggerTimeout-time.Second {
		t.Fatalf("触发 ctx 剩余 %v，期望约 %v", budget, triggerTimeout)
	}

	// 下一次等待请求出现，说明「触发 → 重算 → 重新挂起」已经走完；此时触发次数仍是 1。
	nextReq := wait.next(t)
	if nextReq.duration != 7*24*time.Hour {
		t.Fatalf("触发后重新等待 %v，期望 7 天（下周六 04:10）", nextReq.duration)
	}
	if got := trigger.count(); got != 1 {
		t.Fatalf("到点应只触发一次，实际 %d 次", got)
	}
}

// TestReloadReplacesPlanAndWakesLoop 覆盖 Reload 唤醒等待中的循环并按新表达式重排：
// 04:00 先把默认表达式的 10 分钟等待改成 `*/5 * * * *` 的 5 分钟，到点仍只触发一次。
func TestReloadReplacesPlanAndWakesLoop(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	src := &fakeExprSource{expr: model.DefaultCronExpr}
	s := New(newTestLogger(nil), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	if req := wait.next(t); req.duration != 10*time.Minute {
		t.Fatalf("首次等待 %v，期望 10m", req.duration)
	}

	if err := s.Reload("*/5 * * * *"); err != nil {
		t.Fatalf("Reload 合法表达式失败: %v", err)
	}

	next, ok := s.Next()
	if !ok || !next.Equal(testStart.Add(5*time.Minute)) {
		t.Fatalf("Reload 后 Next() = %v, %v，期望 04:05", next, ok)
	}

	req := wait.next(t)
	if req.duration != 5*time.Minute {
		t.Fatalf("Reload 后等待 %v，期望 5m", req.duration)
	}

	clock.Advance(5 * time.Minute)
	req.ch <- clock.Now()

	trigger.await(t, 1)
	wait.next(t)
	if got := trigger.count(); got != 1 {
		t.Fatalf("Reload 后到点应只触发一次，实际 %d 次", got)
	}
}

// TestReloadRejectsInvalidAndKeepsPlan 覆盖「解析成功才替换」：非法表达式返回包装
// cronexpr.ErrInvalid 的错误，旧计划与下一次时刻都不变。
func TestReloadRejectsInvalidAndKeepsPlan(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	s := New(newTestLogger(nil), trigger.invoke, &fakeExprSource{expr: model.DefaultCronExpr}, clock.Now, wait.Wait)

	before, ok := s.Next()
	if !ok {
		t.Fatal("初始化后应有下一次触发")
	}

	for _, expr := range []string{"0 10 4 * * 6", "", "*/0 * * * *"} {
		if err := s.Reload(expr); !errors.Is(err, cronexpr.ErrInvalid) {
			t.Fatalf("Reload(%q) 的错误 = %v，期望包装 cronexpr.ErrInvalid", expr, err)
		}

		after, ok := s.Next()
		if !ok || !after.Equal(before) {
			t.Fatalf("Reload(%q) 失败后计划被改动：%v，期望 %v", expr, after, before)
		}
	}
}

// TestRunFallsBackToDefaultWhenStoredExprInvalid 覆盖启动期存量表达式非法：记一条
// ERROR 后回到 DefaultCronExpr，循环照常按 10 分钟等待（Requirement 9.6）。
func TestRunFallsBackToDefaultWhenStoredExprInvalid(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	src := &fakeExprSource{expr: "0 10 4 * * 6"} // 6 字段：旧种子形状，解析必失败
	s := New(newTestLogger(buf), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	if req := wait.next(t); req.duration != 10*time.Minute {
		t.Fatalf("非法存量回落后的等待 %v，期望 10m", req.duration)
	}

	next, ok := s.Next()
	if !ok || !next.Equal(testStart.Add(10*time.Minute)) {
		t.Fatalf("回落后 Next() = %v, %v，期望 04:10", next, ok)
	}

	logs := buf.String()
	if !strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("非法存量应记 ERROR，实际日志: %s", logs)
	}
	if !strings.Contains(logs, "0 10 4 * * 6") {
		t.Fatalf("ERROR 日志应带非法表达式原文，实际日志: %s", logs)
	}
}

// TestRunFallsBackToDefaultWhenExprMissing 覆盖表达式缺失：空串按缺失处理，静默回到
// DefaultCronExpr，不记 ERROR（缺行是正常状态，Requirement 9.5）。
func TestRunFallsBackToDefaultWhenExprMissing(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	s := New(newTestLogger(buf), trigger.invoke, &fakeExprSource{expr: ""}, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	if req := wait.next(t); req.duration != 10*time.Minute {
		t.Fatalf("缺失回落后等待 %v，期望 10m", req.duration)
	}
	if logs := buf.String(); strings.Contains(logs, `"level":"ERROR"`) {
		t.Fatalf("缺失不应记 ERROR，实际日志: %s", logs)
	}
}

// TestRunFallsBackToDefaultWhenSourceFails 覆盖读表达式失败：数据库读不出来时记 ERROR
// 并用默认值继续跑，进程不因为设置不可读而起不来。
func TestRunFallsBackToDefaultWhenSourceFails(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	src := &fakeExprSource{err: errors.New("读设置失败")}
	s := New(newTestLogger(buf), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	if req := wait.next(t); req.duration != 10*time.Minute {
		t.Fatalf("读失败回落后等待 %v，期望 10m", req.duration)
	}
	if logs := buf.String(); !strings.Contains(logs, "读设置失败") {
		t.Fatalf("读失败应进 ERROR 日志，实际日志: %s", logs)
	}
}

// TestRunContinuesAfterTriggerError 覆盖触发失败后继续：第一次触发返回错误只记 WARN，
// 下一个计划时刻照常触发，不需要人工重启（Requirement 9.3）。
func TestRunContinuesAfterTriggerError(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	trigger.errs = []error{errors.New("建轮失败")}
	buf := &syncBuffer{}
	src := &fakeExprSource{expr: "* * * * *"}
	s := New(newTestLogger(buf), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	first := wait.next(t)
	if first.duration != time.Minute {
		t.Fatalf("首次等待 %v，期望 1m", first.duration)
	}

	clock.Advance(time.Minute)
	first.ch <- clock.Now()
	trigger.await(t, 1)

	second := wait.next(t)
	if second.duration != time.Minute {
		t.Fatalf("首次失败后重新等待 %v，期望 1m", second.duration)
	}
	if logs := buf.String(); !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "建轮失败") {
		t.Fatalf("触发失败应记 WARN 并带原因，实际日志: %s", logs)
	}

	clock.Advance(time.Minute)
	second.ch <- clock.Now()
	trigger.await(t, 2)

	wait.next(t)
	if got := trigger.count(); got != 2 {
		t.Fatalf("失败后下一个时刻应照常触发，实际 %d 次", got)
	}
}

// TestRunStopsOnContextCancel 覆盖取消退出：ctx 取消后 Run 立刻返回，且不会再触发。
func TestRunStopsOnContextCancel(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	s := New(newTestLogger(nil), trigger.invoke, &fakeExprSource{expr: model.DefaultCronExpr}, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer cancel()

	wait.next(t)
	cancel()
	awaitDone(t, done)

	if got := trigger.count(); got != 0 {
		t.Fatalf("取消后不应再触发，实际 %d 次", got)
	}
}

// TestRunIgnoresConcurrentReentry 覆盖 Run 不并发重入：第二个 Run 立刻返回并记 WARN，
// 主循环不受影响，同一个到点时刻只触发一次。
func TestRunIgnoresConcurrentReentry(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	s := New(newTestLogger(buf), trigger.invoke, &fakeExprSource{expr: model.DefaultCronExpr}, clock.Now, wait.Wait)

	ctx, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	req := wait.next(t)

	second := make(chan struct{})
	go func() {
		defer close(second)
		s.Run(ctx)
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("并发第二次 Run 没有立刻返回")
	}

	if logs := buf.String(); !strings.Contains(logs, `"level":"WARN"`) {
		t.Fatalf("重复启动应记 WARN，实际日志: %s", logs)
	}

	// 主循环仍然活着：推进时钟到点能触发一次，并重新挂起。
	clock.Advance(10 * time.Minute)
	req.ch <- clock.Now()
	trigger.await(t, 1)
	wait.next(t)

	if got := trigger.count(); got != 1 {
		t.Fatalf("重入被拒后应只触发一次，实际 %d 次", got)
	}
}

// TestNextMatchesCronexpr 覆盖 Next 的口径：装载来源里的表达式后，Next 与 cronexpr
// 对同一时钟算出的结果一致，调度不自己另算一份。
func TestNextMatchesCronexpr(t *testing.T) {
	const expr = "30 8-18/2 * * 1-5"

	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	s := New(newTestLogger(nil), trigger.invoke, &fakeExprSource{expr: expr}, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	wait.next(t)

	parsed, err := cronexpr.Parse(expr)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", expr, err)
	}
	want, wantOK := parsed.Next(clock.Now())
	got, gotOK := s.Next()
	if wantOK != gotOK || !got.Equal(want) {
		t.Fatalf("Next() = %v, %v；cronexpr = %v, %v", got, gotOK, want, wantOK)
	}
}

// TestRunWaitsForReloadWhenExprHasNoNext 覆盖「合法但 5 年内不会触发」的表达式：
// 不空转、不触发，只等 Reload；新表达式到达后立即恢复计时。
func TestRunWaitsForReloadWhenExprHasNoNext(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	s := New(newTestLogger(buf), trigger.invoke, &fakeExprSource{expr: "0 0 30 2 *"}, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	eventually(t, "「没有下一次触发时刻」日志", func() bool {
		return strings.Contains(buf.String(), "没有下一次触发时刻")
	})

	if _, ok := s.Next(); ok {
		t.Fatal("不可达表达式不应给出下一次触发时刻")
	}
	wait.assertNone(t)
	if got := trigger.count(); got != 0 {
		t.Fatalf("不可达表达式不应触发，实际 %d 次", got)
	}

	if err := s.Reload("* * * * *"); err != nil {
		t.Fatalf("Reload 合法表达式失败: %v", err)
	}

	req := wait.next(t)
	if req.duration != time.Minute {
		t.Fatalf("Reload 后等待 %v，期望 1m", req.duration)
	}
}

// TestReloadDoesNotRescheduleFiredMinute 覆盖 CP9 的「同一计划分钟不触发两次」。
//
// 触发过 04:01 之后把时钟拨回 04:00:30：这份更早的时钟快照正是锁外读时钟的旧实现
// （或 NTP 回拨）会拿去算 next 的东西，算出来就是已经触发过的 04:01。带「next 严格
// 晚于上次触发时刻」的守卫后，Reload 只能从 04:01 之后起算，落点是 04:02。
func TestReloadDoesNotRescheduleFiredMinute(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	s := New(newTestLogger(nil), trigger.invoke, &fakeExprSource{expr: "* * * * *"}, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	first := wait.next(t)
	if first.duration != time.Minute {
		t.Fatalf("首次等待 %v，期望 1m", first.duration)
	}

	clock.Advance(time.Minute) // 04:01
	first.ch <- clock.Now()
	trigger.await(t, 1)
	wait.next(t) // 04:02 的等待，此刻时钟仍是 04:01

	// 拨回到 04:00:30：不设守卫时 Reload 会算出 04:01 并把它写回 next。
	clock.Set(testStart.Add(30 * time.Second))

	if err := s.Reload("* * * * *"); err != nil {
		t.Fatalf("Reload 合法表达式失败: %v", err)
	}

	next, ok := s.Next()
	if !ok || !next.Equal(testStart.Add(2*time.Minute)) {
		t.Fatalf("Reload 后 Next() = %v, %v，期望 04:02（不得排回已触发的 04:01）", next, ok)
	}

	req := wait.next(t)
	if req.duration != 90*time.Second {
		t.Fatalf("Reload 后等待 %v，期望 1m30s（04:00:30 到 04:02）", req.duration)
	}

	// 到 04:01 也不该补触发：证据是上面的 Next=04:02 与 90s 等待，而不是 count——
	// 假 wait 下只有测试向通道发信号才会进 fire，count 在这条用例里恒为 1。
	clock.Advance(time.Minute)

	// 04:02 才触发，之后照常重算到 04:03。
	clock.Advance(30 * time.Second)
	req.ch <- clock.Now()
	trigger.await(t, 2)

	nextReq := wait.next(t)
	if nextReq.duration != time.Minute {
		t.Fatalf("04:02 触发后重新等待 %v，期望 1m", nextReq.duration)
	}
	if got := trigger.count(); got != 2 {
		t.Fatalf("04:02 应触发一次（共 2 次），实际 %d 次", got)
	}
}

// blockingExprSource 让启动期读表达式停在半路：用例在这个窗口里插入一次 Reload，
// 时序由通道摆布，不依赖协程调度运气。
type blockingExprSource struct {
	entered chan struct{}
	release chan struct{}
	expr    string
}

func newBlockingExprSource(expr string) *blockingExprSource {
	return &blockingExprSource{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		expr:    expr,
	}
}

// CronExpr 先通知「已经进入读库」，再等 release 放行。
func (b *blockingExprSource) CronExpr(context.Context) (string, error) {
	close(b.entered)
	<-b.release
	return b.expr, nil
}

// TestStartupLoadDoesNotOverwriteLaterReload 覆盖启动装载与 HTTP Reload 的顺序保护：
// Run 读库期间到达的 Reload 更晚、更权威，启动装载拿到的库里旧值不能把它盖回去。
//
// 时序是确定的：启动来源堵在读库上 → Reload 生效 → 放行 → 断言计划仍是 Reload 的值。
// 库里的 `0 6 * * 6`（周六 06:00）与默认值 `10 4 * * 6`、Reload 的 `*/5 * * * *` 都不同，
// 谁赢了一眼能看出来。
func TestStartupLoadDoesNotOverwriteLaterReload(t *testing.T) {
	clock := newFakeClock(testStart)
	wait := newFakeWait()
	trigger := newFakeTrigger()
	buf := &syncBuffer{}
	src := newBlockingExprSource("0 6 * * 6")
	s := New(newTestLogger(buf), trigger.invoke, src, clock.Now, wait.Wait)

	_, cancel, done := startRun(t, s)
	defer func() {
		cancel()
		awaitDone(t, done)
	}()

	select {
	case <-src.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("启动装载没有进入读库")
	}

	if err := s.Reload("*/5 * * * *"); err != nil {
		t.Fatalf("Reload 合法表达式失败: %v", err)
	}

	// 放行启动装载，让它拿着库里那份旧值继续走完。
	close(src.release)

	req := wait.next(t)
	if req.duration != 5*time.Minute {
		t.Fatalf("启动装载覆盖了更晚的 Reload：等待 %v，期望 5m", req.duration)
	}

	next, ok := s.Next()
	if !ok || !next.Equal(testStart.Add(5*time.Minute)) {
		t.Fatalf("放行后 Next() = %v, %v，期望 04:05（Reload 的值）", next, ok)
	}

	if logs := buf.String(); !strings.Contains(logs, "启动装载期间已有更晚的 Reload") {
		t.Fatalf("应记录启动装载让位于 Reload，实际日志: %s", logs)
	}
}
