package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。
// 覆盖任务状态机要用的读写：轮次开关、任务清单幂等、待办取值顺序与进度统计。

import (
	"context"
	"errors"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// startTestSyncRun 开一轮采集供用例使用，结束时先清任务再删轮次。
func startTestSyncRun(t *testing.T, s *Store) int64 {
	t.Helper()

	ctx := context.Background()
	// 学期留空：本轮还没解析出学期时 collect_run.term 就是空，不必先造学期行
	runID, err := s.StartSyncRun(ctx, "")
	if err != nil {
		t.Fatalf("开启采集轮次失败: %v", err)
	}

	t.Cleanup(func() {
		// 任务与明细都引用轮次，先清任务才能删轮次
		if _, err := s.pool.Exec(ctx, `DELETE FROM sync_task WHERE run_id = $1`, runID); err != nil {
			t.Logf("清理 sync_task 失败: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM collect_run WHERE run_id = $1`, runID); err != nil {
			t.Logf("清理 collect_run 失败: %v", err)
		}
	})
	return runID
}

// TestStartSyncRunRejectsSecondRun 断言同一时刻至多一轮：已有 running 时拒绝开新轮次。
func TestStartSyncRunRejectsSecondRun(t *testing.T) {
	s := openMigratedStore(t)
	startTestSyncRun(t, s)

	// 第二轮必须被拒绝，且不会造出第二个 running 轮次
	if _, err := s.StartSyncRun(context.Background(), ""); !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("第二轮返回 %v，期望 ErrRunInProgress", err)
	}
}

// TestSyncTaskLifecycle 断言任务清单幂等写入、待办按建立顺序取、done 与 failed 都不再取，
// 以及进度统计里失败的任务仍算未完成。
func TestSyncTaskLifecycle(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	runID := startTestSyncRun(t, s)
	roomID := createTestRoom(t, s, model.Room{ID: "it-task-room", Name: "综合教学楼201", BuildingID: "jxlbh-1"})

	tasks := []SyncTask{
		{Kind: KindWeekMatrix, BuildingID: "jxlbh-1", Week: 1},
		{Kind: KindWeekMatrix, BuildingID: "jxlbh-1", Week: 2},
		{Kind: KindCellDetail, BuildingID: "jxlbh-1", RoomID: roomID, Weekday: 3, Block: "0102"},
	}
	if err := s.EnqueueSyncTasks(ctx, runID, tasks); err != nil {
		t.Fatalf("写入任务清单失败: %v", err)
	}
	// 续跑时会再建一次同一份清单，重复的单元不该多出一行
	if err := s.EnqueueSyncTasks(ctx, runID, tasks); err != nil {
		t.Fatalf("重复写入任务清单失败: %v", err)
	}
	checkProgress(t, s, runID, 0, 3)

	// 第一个待办是第 1 周的周矩阵：明细列的 NULL 回读成零值
	first := nextTask(t, s, runID)
	if first.Kind != KindWeekMatrix || first.Week != 1 || first.RoomID != "" {
		t.Errorf("第一个待办 = %+v，期望第 1 周的周矩阵任务", first)
	}
	if err := s.MarkSyncTaskDone(ctx, first.ID, 1); err != nil {
		t.Fatalf("标记任务完成失败: %v", err)
	}

	// done 的任务跳过，下一个是第 2 周
	second := nextTask(t, s, runID)
	if second.Kind != KindWeekMatrix || second.Week != 2 {
		t.Errorf("第二个待办 = %+v，期望第 2 周的周矩阵任务", second)
	}
	if err := s.MarkSyncTaskFailed(ctx, second.ID, 4, "上游请求发出 4 次后仍失败"); err != nil {
		t.Fatalf("标记任务失败失败: %v", err)
	}
	// 失败的任务在本轮不再取：下一个是明细任务，形状必须带齐房间、星期与大节
	third := nextTask(t, s, runID)
	if third.Kind != KindCellDetail || third.RoomID != roomID || third.Weekday != 3 || third.Block != "0102" {
		t.Errorf("第三个待办 = %+v，期望 %s 的明细任务", third, roomID)
	}
	if err := s.MarkSyncTaskDone(ctx, third.ID, 2); err != nil {
		t.Fatalf("标记任务完成失败: %v", err)
	}

	// 没有待办任务时 found 为 false，本轮该结束
	if _, found, err := s.NextPendingSyncTask(ctx, runID); err != nil || found {
		t.Errorf("没有待办时返回 found=%v, err=%v，期望 false, nil", found, err)
	}
	// 失败的任务仍算未完成：完成数 2、总数 3
	checkProgress(t, s, runID, 2, 3)

	// 失败那一行要留下请求次数与错误，面板据此看哪些任务反复失败
	var attempts int
	var message string
	if err := s.pool.QueryRow(ctx, `
SELECT attempt_count, COALESCE(error_message, '') FROM sync_task WHERE task_id = $1`,
		second.ID).Scan(&attempts, &message); err != nil {
		t.Fatalf("回读失败任务失败: %v", err)
	}
	if attempts != 4 || message == "" {
		t.Errorf("失败任务 attempts=%d message=%q，期望 4 与非空错误", attempts, message)
	}
}

// nextTask 取第一个待办任务，没有时直接判失败。
func nextTask(t *testing.T, s *Store, runID int64) SyncTask {
	t.Helper()

	task, found, err := s.NextPendingSyncTask(context.Background(), runID)
	if err != nil {
		t.Fatalf("读取待办任务失败: %v", err)
	}
	if !found {
		t.Fatal("没有待办任务，期望还有一个")
	}
	return task
}

// checkProgress 断言一轮的完成数与总数。
func checkProgress(t *testing.T, s *Store, runID int64, wantDone, wantTotal int) {
	t.Helper()

	done, total, err := s.SyncProgress(context.Background(), runID)
	if err != nil {
		t.Fatalf("统计进度失败: %v", err)
	}
	if done != wantDone || total != wantTotal {
		t.Errorf("进度 = %d/%d，期望 %d/%d", done, total, wantDone, wantTotal)
	}
}
