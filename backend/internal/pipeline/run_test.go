package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// runStoreStub 记录调用顺序与参数，模拟唯一索引互斥与过期收敛。
// 真实冲突路径由 store 的集成测试覆盖，这里只验证 pipeline 的编排口径。
type runStoreStub struct {
	run      store.CollectRun
	reused   bool
	startErr error
	staleIDs []int64
	staleErr error

	calls       []string
	startedAt   time.Time
	staleNow    time.Time
	staleCutoff time.Time
}

// StartCollectRun 实现 RunStore。
func (s *runStoreStub) StartCollectRun(_ context.Context, startedAt time.Time) (store.CollectRun, bool, error) {
	s.calls = append(s.calls, "start")
	s.startedAt = startedAt
	return s.run, s.reused, s.startErr
}

// FailStaleRuns 实现 RunStore。
func (s *runStoreStub) FailStaleRuns(_ context.Context, now, cutoff time.Time) ([]int64, error) {
	s.calls = append(s.calls, "fail-stale")
	s.staleNow = now
	s.staleCutoff = cutoff
	return s.staleIDs, s.staleErr
}

// fixedClock 返回固定时刻，测试不读真实时间也不 sleep。
func fixedClock(at time.Time) Clock {
	return func() time.Time { return at }
}

// TestStartRunCreatesFirstRound 覆盖没有进行中轮次时的正常开始：先收敛、再新建。
func TestStartRunCreatesFirstRound(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	runs := &runStoreStub{
		run: store.CollectRun{RunID: 7, StartedAt: now, Status: "running"},
	}

	start, err := StartRun(context.Background(), runs, fixedClock(now))
	if err != nil {
		t.Fatalf("开始一轮不应报错，实际 %v", err)
	}
	if start.Reused {
		t.Fatal("没有进行中轮次时不应标记为复用")
	}
	if start.Run.RunID != 7 || start.Status != StatusRunning {
		t.Fatalf("轮次不符: %+v（状态 %q）", start.Run, start.Status)
	}
	if len(start.StaleRunIDs) != 0 {
		t.Fatalf("没有过期轮次时应为空，实际 %v", start.StaleRunIDs)
	}
	if len(runs.calls) != 2 || runs.calls[0] != "fail-stale" || runs.calls[1] != "start" {
		t.Fatalf("应严格先收敛再开始，实际调用顺序 %v", runs.calls)
	}
	if !runs.staleNow.Equal(now) || !runs.staleCutoff.Equal(now.Add(-StaleRunTTL)) {
		t.Fatalf("收敛参数不符: now=%s cutoff=%s", runs.staleNow, runs.staleCutoff)
	}
	if !runs.startedAt.Equal(now) {
		t.Fatalf("开始时刻期望 %s，实际 %s", now, runs.startedAt)
	}
}

// TestStartRunReturnsExistingRoundWhenOneIsRunning 覆盖 Requirement 8.3 的互斥：
// 已有一轮在跑时返回该轮次（Reused=true），不报错。
func TestStartRunReturnsExistingRoundWhenOneIsRunning(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	existingStartedAt := now.Add(-time.Hour)
	runs := &runStoreStub{
		run:    store.CollectRun{RunID: 42, StartedAt: existingStartedAt, Status: "running"},
		reused: true,
	}

	start, err := StartRun(context.Background(), runs, fixedClock(now))
	if err != nil {
		t.Fatalf("已有一轮时应返回该轮次而不是报错，实际 %v", err)
	}
	if !start.Reused {
		t.Fatal("应标记为复用已有轮次")
	}
	if start.Run.RunID != 42 || !start.Run.StartedAt.Equal(existingStartedAt) {
		t.Fatalf("应原样返回已有轮次，实际 %+v", start.Run)
	}
	if start.Status != StatusRunning {
		t.Fatalf("复用已有轮次时状态也应转成枚举 running，实际 %q", start.Status)
	}
	if len(runs.calls) != 2 {
		t.Fatalf("复用路径也要先收敛，实际调用 %v", runs.calls)
	}
}

// TestStartRunConvergesStaleRoundsBeforeCreating 覆盖 Requirement 8.3 的过期收敛：
// 崩溃留下的轮次先被置为失败，随后才允许开始新一轮。
func TestStartRunConvergesStaleRoundsBeforeCreating(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	runs := &runStoreStub{
		run:      store.CollectRun{RunID: 9, StartedAt: now, Status: "running"},
		staleIDs: []int64{3, 4},
	}

	start, err := StartRun(context.Background(), runs, fixedClock(now))
	if err != nil {
		t.Fatalf("开始一轮不应报错，实际 %v", err)
	}
	if len(start.StaleRunIDs) != 2 || start.StaleRunIDs[0] != 3 || start.StaleRunIDs[1] != 4 {
		t.Fatalf("被收敛的过期轮次期望 [3 4]，实际 %v", start.StaleRunIDs)
	}
	if start.Reused || start.Run.RunID != 9 {
		t.Fatalf("收敛后应新建轮次，实际 %+v", start)
	}
	if runs.calls[0] != "fail-stale" || runs.calls[1] != "start" {
		t.Fatalf("收敛必须先于开始，实际调用顺序 %v", runs.calls)
	}
}

// TestStartRunUsesBusinessZoneForDatabaseTimes 覆盖业务时区：无论时钟给什么时区，
// 传给仓储的时刻都换算成 Asia/Shanghai，便于按业务时区阅读落库时间。
func TestStartRunUsesBusinessZoneForDatabaseTimes(t *testing.T) {
	utcNow := time.Date(2026, 11, 4, 3, 37, 31, 0, time.UTC)
	runs := &runStoreStub{
		run: store.CollectRun{RunID: 7, StartedAt: utcNow, Status: "running"},
	}

	if _, err := StartRun(context.Background(), runs, fixedClock(utcNow)); err != nil {
		t.Fatalf("开始一轮不应报错，实际 %v", err)
	}

	want := "2026-11-04T11:37:31+08:00"
	if got := runs.startedAt.Format(time.RFC3339); got != want {
		t.Fatalf("写入时刻应换算到 Asia/Shanghai，期望 %s，实际 %s", want, got)
	}
	if got := runs.staleNow.Format(time.RFC3339); got != want {
		t.Fatalf("收敛的 now 也应换算到 Asia/Shanghai，期望 %s，实际 %s", want, got)
	}
	if !runs.startedAt.Equal(utcNow) {
		t.Fatal("换算时区不能改变绝对时刻")
	}
	if _, offset := runs.startedAt.Zone(); offset != 8*60*60 {
		t.Fatalf("期望 +08:00 偏移，实际 %d 秒", offset)
	}
}

// TestStartRunStopsWhenConvergenceFails 覆盖收敛失败：不得继续新建轮次，
// 否则过期轮次会一直占着唯一索引。
func TestStartRunStopsWhenConvergenceFails(t *testing.T) {
	convergeErr := errors.New("数据库不可用")
	runs := &runStoreStub{staleErr: convergeErr}

	_, err := StartRun(context.Background(), runs, fixedClock(time.Now()))
	if !errors.Is(err, convergeErr) {
		t.Fatalf("应上抛收敛错误，实际 %v", err)
	}
	if len(runs.calls) != 1 || runs.calls[0] != "fail-stale" {
		t.Fatalf("收敛失败后不该再尝试开始，实际调用 %v", runs.calls)
	}
}

// TestStartRunStopsWhenStartingFails 覆盖新建轮次失败：错误上抛，不吞成复用。
func TestStartRunStopsWhenStartingFails(t *testing.T) {
	startErr := errors.New("连接断开")
	runs := &runStoreStub{startErr: startErr}

	_, err := StartRun(context.Background(), runs, fixedClock(time.Now()))
	if !errors.Is(err, startErr) {
		t.Fatalf("应上抛开始错误，实际 %v", err)
	}
	if len(runs.calls) != 2 || runs.calls[1] != "start" {
		t.Fatalf("应先收敛再开始，实际调用 %v", runs.calls)
	}
}

// TestConvergeStaleRunsUsesTwoHourCutoff 覆盖单独调用的收敛入口（服务启动用）：
// 截止时刻恒为 now-2h，恰好两小时的轮次不在此处被判定（SQL 用严格小于）。
func TestConvergeStaleRunsUsesTwoHourCutoff(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	runs := &runStoreStub{staleIDs: []int64{11}}

	runIDs, err := ConvergeStaleRuns(context.Background(), runs, fixedClock(now))
	if err != nil {
		t.Fatalf("收敛不应报错，实际 %v", err)
	}
	if len(runIDs) != 1 || runIDs[0] != 11 {
		t.Fatalf("被收敛的轮次期望 [11]，实际 %v", runIDs)
	}
	if StaleRunTTL != 2*time.Hour {
		t.Fatalf("过期阈值应为 2 小时，实际 %s", StaleRunTTL)
	}
	if !runs.staleCutoff.Equal(now.Add(-2 * time.Hour)) {
		t.Fatalf("截止时刻期望 now-2h，实际 %s", runs.staleCutoff)
	}
	if len(runs.calls) != 1 || runs.calls[0] != "fail-stale" {
		t.Fatalf("单独收敛只应调用仓储一次，实际 %v", runs.calls)
	}
}

// TestStaleCutoffKeepsExactlyTwoHours 用假时钟锁住 `<` 边界：started_at 恰好等于
// now-2h 的轮次不收敛，早一秒才收敛。这条口径平时只在真实库用例里由 SQL 验证，
// 抽成纯函数后默认门禁（不设 KJS_TEST_DSN）也能守住。
func TestStaleCutoffKeepsExactlyTwoHours(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	cutoff := staleCutoff(now)

	if want := now.Add(-2 * time.Hour); !cutoff.Equal(want) {
		t.Fatalf("截止时刻期望 %s，实际 %s", want, cutoff)
	}
	if StaleRunTTL != 2*time.Hour {
		t.Fatalf("过期阈值应为 2 小时，实际 %s", StaleRunTTL)
	}

	// 恰好两小时：不早于截止时刻，按 `<` 判定不算过期。
	// 注意这里用 time.Before 模拟判定，不执行 store 的 SQL；SQL 里 `started_at < $2`
	// 的严格小于由 collectrun_test.go 的字面量断言与 KJS_TEST_DSN 集成用例守护。
	exactlyTwoHours := now.Add(-StaleRunTTL)
	if exactlyTwoHours.Before(cutoff) {
		t.Fatalf("恰好两小时的轮次不应被收敛，started_at=%s cutoff=%s", exactlyTwoHours, cutoff)
	}

	// 再早一秒：严格早于截止时刻，必须收敛。
	oneSecondEarlier := now.Add(-StaleRunTTL - time.Second)
	if !oneSecondEarlier.Before(cutoff) {
		t.Fatalf("早一秒的轮次应被收敛，started_at=%s cutoff=%s", oneSecondEarlier, cutoff)
	}

	// 阈值是绝对时长，时钟换到 UTC 也不改变分界后的绝对时刻。
	utcCutoff := staleCutoff(now.UTC())
	if !utcCutoff.Equal(cutoff) {
		t.Fatalf("换算时区不应改变截止时刻，期望 %s，实际 %s", cutoff, utcCutoff)
	}
}

// TestStartRunDefaultsToWallClock 覆盖装配层不传时钟的情况：退回 time.Now，
// 用同一次取时同时算 now 与 cutoff，避免跨越两小时的边沿抖动。
func TestStartRunDefaultsToWallClock(t *testing.T) {
	runs := &runStoreStub{run: store.CollectRun{RunID: 7, Status: "running"}}

	before := time.Now()
	if _, err := StartRun(context.Background(), runs, nil); err != nil {
		t.Fatalf("不传时钟也应能开始一轮，实际 %v", err)
	}
	after := time.Now()

	if runs.staleNow.Before(before) || runs.staleNow.After(after) {
		t.Fatalf("默认时钟应取真实当前时刻，实际 %s 不在 [%s, %s] 内", runs.staleNow, before, after)
	}
	if !runs.staleCutoff.Equal(runs.staleNow.Add(-StaleRunTTL)) {
		t.Fatalf("截止时刻应由同一个 now 推出，实际 now=%s cutoff=%s", runs.staleNow, runs.staleCutoff)
	}
	if _, offset := runs.staleNow.Zone(); offset != 8*60*60 {
		t.Fatalf("默认时钟也应换算到 +08:00，实际 %d 秒", offset)
	}
}
