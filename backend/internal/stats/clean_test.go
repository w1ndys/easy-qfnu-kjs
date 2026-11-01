package stats

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCleanerDeletesBefore90Days 覆盖清理边界：删除早于「今天减 90 天」的记录（Requirement 34.10、CP19）。
func TestCleanerDeletesBefore90Days(t *testing.T) {
	now := time.Date(2026, 11, 2, 23, 30, 0, 0, shanghai)
	deleter := newFakeDeleter()
	cleaner := NewCleaner(quietLogger(), deleter, WithCleanerClock(func() time.Time { return now }))

	if _, err := cleaner.CleanupOnce(context.Background(), now); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	cutoff := deleter.lastCutoff()
	if got := cutoff.Format(time.DateOnly); got != "2026-08-04" {
		t.Fatalf("期望边界 2026-08-04，实际 %s", got)
	}
	// 边界必须是业务日期的零点，且与今天正好相差 90 天。
	if !cutoff.Equal(dayOf(now).AddDate(0, 0, -RetentionDays)) {
		t.Fatalf("边界应恰为今天减 %d 天的零点，实际 %s", RetentionDays, cutoff)
	}
	if cutoff.Hour() != 0 || cutoff.Minute() != 0 || cutoff.Second() != 0 {
		t.Fatalf("边界应是零点，实际 %s", cutoff)
	}
}

// TestCleanerRunCleansOnStartAndStops 覆盖清理任务启动即清一次、ctx 结束即退出。
func TestCleanerRunCleansOnStartAndStops(t *testing.T) {
	deleter := newFakeDeleter()
	cleaner := NewCleaner(quietLogger(), deleter,
		WithCleanerClock(func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, shanghai) }))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cleaner.Run(ctx)
	}()

	select {
	case <-deleter.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("清理任务启动后应立刻清理一次")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后清理任务应退出")
	}
}

// TestCleanerRunKeepsGoingAfterFailure 覆盖清理失败不终止任务（CP19 的「失败不影响查询与采集」）。
func TestCleanerRunKeepsGoingAfterFailure(t *testing.T) {
	deleter := newFakeDeleter()
	deleter.err = errors.New("db down")
	cleaner := NewCleaner(quietLogger(), deleter,
		WithCleanerClock(func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, shanghai) }),
		// 间隔压到 1ms：用例只需要观察到「连续两次清理」，不必等一天。
		WithCleanerInterval(time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cleaner.Run(ctx)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-deleter.calls:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次清理未发生", i+1)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后清理任务应退出")
	}
}

// TestCleanerWaitReturnsAfterRun 覆盖清理任务的收尾约定：ctx 结束后 Wait 返回，关闭流程不会被挂住。
func TestCleanerWaitReturnsAfterRun(t *testing.T) {
	deleter := newFakeDeleter()
	cleaner := NewCleaner(quietLogger(), deleter,
		WithCleanerClock(func() time.Time { return time.Date(2026, 11, 2, 0, 0, 0, 0, shanghai) }))

	ctx, cancel := context.WithCancel(context.Background())
	go cleaner.Run(ctx)

	select {
	case <-deleter.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("清理任务启动后应立刻清理一次")
	}

	cancel()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		cleaner.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后 Wait 应立即返回")
	}
}
