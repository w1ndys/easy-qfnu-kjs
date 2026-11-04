package store

import (
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestLoadRunFactsAgainstRealDB 覆盖发布组装读轮次事实的真实库口径：
// 学期、窗口与归属冲突计数如实读回；NULL 的学期与窗口折成空串与 0；
// 轮次不存在时返回 ErrRunStoreFailed，不把零值当成合法轮次。
func TestLoadRunFactsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedSyncTaskRun(ctx, t, pool)

	mustExec(ctx, t, pool, `
UPDATE collect_run
   SET window_start = 5, window_end = 8, attr_conflict_count = 3
 WHERE run_id = $1`, runID)

	facts, err := LoadRunFacts(ctx, pool, runID)
	if err != nil {
		t.Fatalf("读轮次发布事实失败: %v", err)
	}
	want := RunFacts{
		RunID:             runID,
		Term:              syncTaskFixtureTerm,
		WindowStart:       5,
		WindowEnd:         8,
		AttrConflictCount: 3,
	}
	if facts != want {
		t.Fatalf("轮次发布事实不符: got %+v want %+v", facts, want)
	}

	// 没写学期、没定窗口的轮次照样能读：NULL 折成空串与 0，由调用方判非法。
	var legacyRunID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status)
VALUES (now(), 'failed')
RETURNING run_id`).Scan(&legacyRunID); err != nil {
		t.Fatalf("创建无学期轮次失败: %v", err)
	}
	legacy, err := LoadRunFacts(ctx, pool, legacyRunID)
	if err != nil {
		t.Fatalf("读无学期轮次失败: %v", err)
	}
	if legacy.Term != "" || legacy.WindowStart != 0 || legacy.WindowEnd != 0 {
		t.Fatalf("NULL 学期/窗口应折成零值: %+v", legacy)
	}

	if _, err := LoadRunFacts(ctx, pool, runID+100000); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在应返回 ErrRunStoreFailed，实际 err=%v", err)
	}
}

// TestPublishRepoAgainstRealDB 覆盖发布仓储的三个读方法委托：
// 归属快照读 collect_run_room、任务汇总读 sync_task 现状、轮次事实读 collect_run。
func TestPublishRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	mustExec(ctx, t, pool, `
UPDATE collect_run
   SET window_start = 5, window_end = 8, attr_conflict_count = 1
 WHERE run_id = $1`, runID)

	// 归属快照：一条有效归属，一条带冲突标记。
	if err := UpsertRunRooms(ctx, pool, runID, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
	}, now); err != nil {
		t.Fatalf("写归属快照失败: %v", err)
	}
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id)
VALUES ('r102', 'F102', 'F102', 'Q11')
ON CONFLICT (room_id) DO NOTHING`)
	if err := UpsertRunRooms(ctx, pool, runID, []RunRoom{
		{RoomID: "r102", BuildingID: "Q11", Conflict: true, ConflictBuildingID: "Q12"},
	}, now); err != nil {
		t.Fatalf("写冲突归属失败: %v", err)
	}

	// 两条必需任务：一条完成、一条失败。
	tasks, err := EnsureWeekMatrixTasks(ctx, pool, runID, []SyncTaskInput{
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 5},
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 6},
	}, now)
	if err != nil {
		t.Fatalf("写周矩阵任务失败: %v", err)
	}
	if err := CompleteSyncTask(ctx, pool, runID, tasks[0].TaskID, now); err != nil {
		t.Fatalf("置任务完成失败: %v", err)
	}
	if err := FailSyncTask(ctx, pool, runID, tasks[1].TaskID, "矩阵结构不符合预期", now); err != nil {
		t.Fatalf("置任务失败失败: %v", err)
	}

	repo := NewPublishRepo(pool)

	facts, err := repo.LoadRunFacts(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读发布事实失败: %v", err)
	}
	if facts.RunID != runID || facts.WindowStart != 5 || facts.WindowEnd != 8 || facts.AttrConflictCount != 1 {
		t.Fatalf("仓储读发布事实不符: %+v", facts)
	}
	if facts.Term != syncTaskFixtureTerm {
		t.Fatalf("仓储读学期不符: %q", facts.Term)
	}

	rooms, err := repo.LoadRunRooms(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读归属快照失败: %v", err)
	}
	wantRooms := []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q11", Conflict: true, ConflictBuildingID: "Q12"},
	}
	if len(rooms) != len(wantRooms) {
		t.Fatalf("归属快照行数不符: got %+v want %+v", rooms, wantRooms)
	}
	for index := range wantRooms {
		if rooms[index] != wantRooms[index] {
			t.Fatalf("第 %d 行归属不符: got %+v want %+v", index+1, rooms[index], wantRooms[index])
		}
	}

	summary, err := repo.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读任务汇总失败: %v", err)
	}
	wantSummary := RunTaskSummary{
		RequiredTotal:       2,
		RequiredDone:        1,
		RequiredFailed:      1,
		FirstFailureMessage: "矩阵结构不符合预期",
	}
	if summary != wantSummary {
		t.Fatalf("任务汇总不符: got %+v want %+v", summary, wantSummary)
	}
}
