package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// collectRunPhaseState 读一轮的阶段、终态字段与四个计数，供集成用例断言。
func collectRunPhaseState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) (
	status, phase, errorCode, errorMessage string, finished bool,
	requiredTotal, requiredDone, optionalTotal, optionalDone int,
) {
	t.Helper()

	err := pool.QueryRow(ctx, `
SELECT status, phase, COALESCE(error_code, ''), COALESCE(error_message, ''), finished_at IS NOT NULL,
       required_total, required_done, optional_total, optional_done
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(
		&status, &phase, &errorCode, &errorMessage, &finished,
		&requiredTotal, &requiredDone, &optionalTotal, &optionalDone)
	if err != nil {
		t.Fatalf("读取轮次 %d 阶段状态失败: %v", runID, err)
	}
	return status, phase, errorCode, errorMessage, finished,
		requiredTotal, requiredDone, optionalTotal, optionalDone
}

// TestRunPhaseAndFinishAgainstRealDB 覆盖阶段推进与收口的真实库口径：
// 阶段与计数同一条 UPDATE 落库；汇总从 sync_task 现状读；收口写终态、
// phase=finished、finished_at 与错误信息，并按任务现状重算计数。
func TestRunPhaseAndFinishAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	weeks, err := repo.EnsureWeekMatrixTasks(ctx, runID, []SyncTaskInput{
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
	}, now)
	if err != nil {
		t.Fatalf("落周矩阵任务失败: %v", err)
	}
	cells, err := repo.EnsureCellDetailTasks(ctx, runID, []SyncTaskInput{
		{Kind: SyncTaskKindCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
	}, now)
	if err != nil {
		t.Fatalf("落占用说明任务失败: %v", err)
	}
	// 一条必需任务成功、一条必需任务失败、一条可选任务成功：收口应判 failed。
	if err := repo.CompleteSyncTask(ctx, runID, weeks[0].TaskID, now); err != nil {
		t.Fatalf("置 done 失败: %v", err)
	}
	if err := repo.FailSyncTask(ctx, runID, weeks[1].TaskID, "矩阵结构不符合预期", now); err != nil {
		t.Fatalf("置 failed 失败: %v", err)
	}
	if err := repo.CompleteSyncTask(ctx, runID, cells[0].TaskID, now); err != nil {
		t.Fatalf("置明细完成失败: %v", err)
	}

	if err := phaseRepo.SetRunPhase(ctx, runID, "week_matrix", []string{"login", "calendar", "week_matrix"}, now); err != nil {
		t.Fatalf("推进阶段失败: %v", err)
	}
	status, phase, _, _, finished, requiredTotal, requiredDone, optionalTotal, optionalDone :=
		collectRunPhaseState(ctx, t, pool, runID)
	if status != "running" || phase != "week_matrix" || finished {
		t.Fatalf("阶段推进不应改动状态与收口: status=%q phase=%q finished=%v", status, phase, finished)
	}
	if requiredTotal != 2 || requiredDone != 1 || optionalTotal != 1 || optionalDone != 1 {
		t.Fatalf("阶段推进应刷新计数，实际 required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}

	summary, err := phaseRepo.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		t.Fatalf("读任务汇总失败: %v", err)
	}
	wantSummary := RunTaskSummary{
		RequiredTotal:       2,
		RequiredDone:        1,
		RequiredFailed:      1,
		OptionalTotal:       1,
		OptionalDone:        1,
		FirstFailureMessage: "矩阵结构不符合预期",
	}
	if summary != wantSummary {
		t.Fatalf("任务汇总期望 %+v，实际 %+v", wantSummary, summary)
	}

	// minor 10：错误码与场景对应——这里失败的是周矩阵结构，不是学期/周历。
	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID:        runID,
		Status:       "failed",
		ErrorCode:    "matrix_structure_invalid",
		ErrorMessage: "矩阵结构不符合预期",
		FinishedAt:   now,
	}); err != nil {
		t.Fatalf("收口失败: %v", err)
	}
	var errorCode, errorMessage string
	status, phase, errorCode, errorMessage, finished, requiredTotal, requiredDone, optionalTotal, optionalDone =
		collectRunPhaseState(ctx, t, pool, runID)
	if status != "failed" || phase != "finished" || !finished {
		t.Fatalf("终态不符: status=%q phase=%q finished=%v", status, phase, finished)
	}
	if errorCode != "matrix_structure_invalid" || errorMessage != "矩阵结构不符合预期" {
		t.Fatalf("错误信息不符: code=%q message=%q", errorCode, errorMessage)
	}
	if requiredTotal != 2 || requiredDone != 1 || optionalTotal != 1 || optionalDone != 1 {
		t.Fatalf("收口应重算计数，实际 required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}

	// 收口后不再占着 collect_run_single_running，下一轮可以正常开始。
	if count := countRunningCollectRuns(ctx, t, pool); count != 0 {
		t.Fatalf("收口后不应再有进行中轮次，实际 %d 条", count)
	}
}

// TestLoadRunTaskSummaryForEmptyRunAgainstRealDB 覆盖零任务轮次：汇总返回全零且
// 不报错（轮次存在），失败原因留空。
func TestLoadRunTaskSummaryForEmptyRunAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)

	summary, err := phaseRepo.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		t.Fatalf("零任务轮次读汇总不应报错，实际 %v", err)
	}
	if summary != (RunTaskSummary{}) {
		t.Fatalf("零任务轮次汇总应全零，实际 %+v", summary)
	}

	if _, err := phaseRepo.LoadRunTaskSummary(ctx, runID+100000); err == nil {
		t.Fatal("不存在的轮次应报错，不能当成零任务")
	}
}

// TestLoadRunTaskSummaryUsesEarliestFailedTaskAgainstRealDB 覆盖 minor 5：
// 两条任务都置 failed 时，error_message 取 task_id 升序的第一条（task_id 1），
// 而不是「最后失败的那条」。刻意先置 task 2 失败、再置 task 1 失败，锁住
// 「按任务创建顺序取最早失败」这条口径。
func TestLoadRunTaskSummaryUsesEarliestFailedTaskAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	weeks, err := repo.EnsureWeekMatrixTasks(ctx, runID, []SyncTaskInput{
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
	}, now)
	if err != nil {
		t.Fatalf("落周矩阵任务失败: %v", err)
	}
	if len(weeks) != 2 {
		t.Fatalf("应落 2 条周矩阵任务，实际 %d 条", len(weeks))
	}
	// 先失败 task_id 2，再失败 task_id 1：失败发生的顺序与 task_id 相反。
	if err := repo.FailSyncTask(ctx, runID, weeks[1].TaskID, "后失败的任务", now); err != nil {
		t.Fatalf("置第二条任务失败: %v", err)
	}
	if err := repo.FailSyncTask(ctx, runID, weeks[0].TaskID, "最早失败的任务", now); err != nil {
		t.Fatalf("置第一条任务失败: %v", err)
	}

	summary, err := phaseRepo.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		t.Fatalf("读任务汇总失败: %v", err)
	}
	if summary.RequiredFailed != 2 {
		t.Fatalf("两条任务都应算失败，实际 %+v", summary)
	}
	if summary.FirstFailureMessage != "最早失败的任务" {
		t.Fatalf("失败原因应取 task_id 最小的失败任务，实际 %q", summary.FirstFailureMessage)
	}
	// 这条用例没有写结构失败码，error_code 保持为空——与「原因」是两条独立来源。
	if summary.RecordedErrorCode != "" {
		t.Fatalf("未写结构失败码时不应带出错误码，实际 %q", summary.RecordedErrorCode)
	}
}

// TestFinishRunClearsErrorsOnSuccessAgainstRealDB 覆盖成功收口：error_code 与
// error_message 用 NULLIF 折成 NULL，不把空串或上一次的失败文案留在成功轮次上。
func TestFinishRunClearsErrorsOnSuccessAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	// 先人为写上一轮的失败信息，确认成功收口会清掉。
	mustExec(ctx, t, pool, `
UPDATE collect_run
   SET error_code = 'calendar_invalid', error_message = '旧失败'
 WHERE run_id = $1`, runID)

	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID:      runID,
		Status:     "success",
		FinishedAt: now,
	}); err != nil {
		t.Fatalf("成功收口失败: %v", err)
	}
	status, phase, errorCode, errorMessage, finished, _, _, _, _ :=
		collectRunPhaseState(ctx, t, pool, runID)
	if status != "success" || phase != "finished" || !finished {
		t.Fatalf("成功终态不符: status=%q phase=%q finished=%v", status, phase, finished)
	}
	if errorCode != "" || errorMessage != "" {
		t.Fatalf("成功收口应清空错误信息，实际 code=%q message=%q", errorCode, errorMessage)
	}

	var finishedAt time.Time
	if err := pool.QueryRow(ctx, "SELECT finished_at FROM collect_run WHERE run_id = $1", runID).Scan(&finishedAt); err != nil {
		t.Fatalf("读取 finished_at 失败: %v", err)
	}
	if !finishedAt.Equal(now) {
		t.Fatalf("finished_at 期望 %s，实际 %s", now, finishedAt)
	}
}

// TestSetRunPhaseOnlyMovesForwardAgainstRealDB 覆盖 minor 2 / minor 3 的真实库口径：
// 阶段只前进，同一阶段重复推进是幂等的；回退阶段一行都不更新；收口之后不能再推进
// （终态只能由收口写）。
func TestSetRunPhaseOnlyMovesForwardAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	weekAllowed := []string{"login", "calendar", "week_matrix"}
	if err := phaseRepo.SetRunPhase(ctx, runID, "week_matrix", weekAllowed, now); err != nil {
		t.Fatalf("推进到周矩阵失败: %v", err)
	}
	// 同一阶段重复推进：幂等刷新计数，不算回退。
	if err := phaseRepo.SetRunPhase(ctx, runID, "week_matrix", weekAllowed, now); err != nil {
		t.Fatalf("重复推进同一阶段应成功: %v", err)
	}
	cellAllowed := []string{"login", "calendar", "week_matrix", "cell_detail"}
	if err := phaseRepo.SetRunPhase(ctx, runID, "cell_detail", cellAllowed, now); err != nil {
		t.Fatalf("推进到占用说明失败: %v", err)
	}

	// 回退到周矩阵：SQL 的 phase = ANY($4) 挡住，必须报错且阶段不变。
	if err := phaseRepo.SetRunPhase(ctx, runID, "week_matrix", weekAllowed, now); err == nil {
		t.Fatal("回退阶段应报错")
	}
	if _, phase, _, _, _, _, _, _, _ := collectRunPhaseState(ctx, t, pool, runID); phase != "cell_detail" {
		t.Fatalf("回退被拒后阶段应保持 cell_detail，实际 %q", phase)
	}

	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID: runID, Status: "success", FinishedAt: now,
	}); err != nil {
		t.Fatalf("收口失败: %v", err)
	}
	// 收口之后 status 不再是 running，迟到推进一行都不更新。
	publishAllowed := []string{"login", "calendar", "week_matrix", "cell_detail", "validate", "publish"}
	if err := phaseRepo.SetRunPhase(ctx, runID, "publish", publishAllowed, now); err == nil {
		t.Fatal("已收口的轮次不应还能推进阶段")
	}
	status, phase, _, _, finished, _, _, _, _ := collectRunPhaseState(ctx, t, pool, runID)
	if status != "success" || phase != "finished" || !finished {
		t.Fatalf("迟到推进不应改动终态: status=%q phase=%q finished=%v", status, phase, finished)
	}
}

// TestFinishRunRejectsRepeatFinalizeAgainstRealDB 覆盖 minor 5：同一轮重复收口
// 一行都不更新，第一次写下的终态、错误信息与 finished_at 保持原样。
func TestFinishRunRejectsRepeatFinalizeAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID:        runID,
		Status:       "failed",
		ErrorCode:    "matrix_structure_invalid",
		ErrorMessage: "矩阵结构不符合预期",
		FinishedAt:   now,
	}); err != nil {
		t.Fatalf("首次收口失败: %v", err)
	}
	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID:        runID,
		Status:       "success",
		ErrorMessage: "迟到重试",
		FinishedAt:   now.Add(time.Hour),
	}); err == nil {
		t.Fatal("重复收口应报错")
	}

	var finishedAt time.Time
	status, phase, errorCode, errorMessage, finished, _, _, _, _ := collectRunPhaseState(ctx, t, pool, runID)
	if err := pool.QueryRow(ctx, "SELECT finished_at FROM collect_run WHERE run_id = $1", runID).Scan(&finishedAt); err != nil {
		t.Fatalf("读取 finished_at 失败: %v", err)
	}
	if status != "failed" || phase != "finished" || !finished {
		t.Fatalf("重复收口不应改动终态: status=%q phase=%q finished=%v", status, phase, finished)
	}
	if errorCode != "matrix_structure_invalid" || errorMessage != "矩阵结构不符合预期" {
		t.Fatalf("重复收口不应改动错误信息: code=%q message=%q", errorCode, errorMessage)
	}
	if !finishedAt.Equal(now) {
		t.Fatalf("重复收口不应改动 finished_at，期望 %s，实际 %s", now, finishedAt)
	}
}

// TestRecordRunErrorCodeKeepsFirstAgainstRealDB 覆盖用户拍板 1 的真实库口径：
// 第一个结构失败码落库后再写别的码不覆盖；收口清空；收口之后不能再写。
func TestRecordRunErrorCodeKeepsFirstAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	phaseRepo := NewRunPhaseRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := runPhaseTestNow()

	if err := phaseRepo.RecordRunErrorCode(ctx, runID, "matrix_structure_invalid", now); err != nil {
		t.Fatalf("写错误码失败: %v", err)
	}
	if err := phaseRepo.RecordRunErrorCode(ctx, runID, "axis_changed", now); err != nil {
		t.Fatalf("写第二个错误码失败: %v", err)
	}
	_, _, errorCode, _, _, _, _, _, _ := collectRunPhaseState(ctx, t, pool, runID)
	if errorCode != "matrix_structure_invalid" {
		t.Fatalf("轮次只保留第一个结构失败码，实际 %q", errorCode)
	}

	if err := phaseRepo.FinishRun(ctx, RunFinish{
		RunID: runID, Status: "failed", ErrorCode: "matrix_structure_invalid", FinishedAt: now,
	}); err != nil {
		t.Fatalf("收口失败: %v", err)
	}
	if err := phaseRepo.RecordRunErrorCode(ctx, runID, "detail_structure_invalid", now); err == nil {
		t.Fatal("已收口的轮次不应还能写错误码")
	}
	_, _, errorCode, _, _, _, _, _, _ = collectRunPhaseState(ctx, t, pool, runID)
	if errorCode != "matrix_structure_invalid" {
		t.Fatalf("迟到写入不应改动错误码，实际 %q", errorCode)
	}
}
