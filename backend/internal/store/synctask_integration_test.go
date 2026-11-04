package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// syncTaskFixtureTerm 是任务执行集成测试使用的学期。
const syncTaskFixtureTerm = "2026-2027-1"

// seedSyncTaskRun 造一条带学期与房号的 running 轮次，供任务落库与周结果测试使用。
func seedSyncTaskRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()

	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		syncTaskFixtureTerm)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('r101', 'F101', 'F101', 'Q10', '综合教学楼')
ON CONFLICT (room_id) DO NOTHING`)

	var runID int64
	err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term)
VALUES (now(), 'running', $1)
RETURNING run_id`, syncTaskFixtureTerm).Scan(&runID)
	if err != nil {
		t.Fatalf("创建轮次失败: %v", err)
	}
	return runID
}

// syncTaskStatus 读一条任务的状态、失败次数与失败原因。
func syncTaskStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, taskID int64) (status string, attempts int, message string) {
	t.Helper()

	err := pool.QueryRow(ctx, `
SELECT status, attempt_count, COALESCE(error_message, '')
  FROM sync_task
 WHERE task_id = $1`, taskID).Scan(&status, &attempts, &message)
	if err != nil {
		t.Fatalf("读取任务 %d 失败: %v", taskID, err)
	}
	return status, attempts, message
}

// findWeekTask 按 kind 与周查找周矩阵任务。集成用例不依赖任务下标：
// 落库顺序由 task_id 决定，但断言身份时应按任务语义字段查找。
func findWeekTask(t *testing.T, tasks []SyncTask, week int) SyncTask {
	t.Helper()
	for _, task := range tasks {
		if task.Kind == "week_matrix" && task.Week == week {
			return task
		}
	}
	t.Fatalf("未找到第 %d 周的周矩阵任务: %+v", week, tasks)
	return SyncTask{}
}

// findCellTask 按 kind 与房号查找占用说明任务。
func findCellTask(t *testing.T, tasks []SyncTask, roomID string) SyncTask {
	t.Helper()
	for _, task := range tasks {
		if task.Kind == "cell_detail" && task.RoomID == roomID {
			return task
		}
	}
	t.Fatalf("未找到房号 %s 的占用说明任务: %+v", roomID, tasks)
	return SyncTask{}
}

// runTaskCounts 读轮次上的任务总数与完成数。
func runTaskCounts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) (requiredTotal, requiredDone, optionalTotal, optionalDone int) {
	t.Helper()

	err := pool.QueryRow(ctx, `
SELECT required_total, required_done, optional_total, optional_done
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(&requiredTotal, &requiredDone, &optionalTotal, &optionalDone)
	if err != nil {
		t.Fatalf("读取轮次 %d 计数失败: %v", runID, err)
	}
	return requiredTotal, requiredDone, optionalTotal, optionalDone
}

// ensurePlannedTasksForTest 按两段落库并拼回完整清单，模拟 pipeline 的落库顺序：
// 先周矩阵段（不依赖 room），再占用说明段（room_id 有外键）。返回顺序是
// 周矩阵任务在前、占用说明任务在后，与 task_id 顺序一致。
func ensurePlannedTasksForTest(
	ctx context.Context,
	repo SyncTaskRepo,
	runID int64,
	planned []SyncTaskInput,
	now time.Time,
) ([]SyncTask, error) {
	weekInputs, cellInputs := splitSyncTaskInputsForTest(planned)
	weeks, err := repo.EnsureWeekMatrixTasks(ctx, runID, weekInputs, now)
	if err != nil {
		return nil, err
	}
	cells, err := repo.EnsureCellDetailTasks(ctx, runID, cellInputs, now)
	if err != nil {
		return nil, err
	}
	return append(weeks, cells...), nil
}

// splitSyncTaskInputsForTest 按 kind 把规划拆成两段，保持段内顺序。
func splitSyncTaskInputsForTest(tasks []SyncTaskInput) (week, cell []SyncTaskInput) {
	for _, task := range tasks {
		if task.Kind == SyncTaskKindWeekMatrix {
			week = append(week, task)
		} else {
			cell = append(cell, task)
		}
	}
	return week, cell
}

// TestEnsureSplitSyncTasksAgainstRealDB 覆盖两段任务落库与续跑基准：
// 首次写入规划任务并回写总数；再次调用沿用库里的任务与状态，不把新规划混进来。
func TestEnsureSplitSyncTasksAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	planned := []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 21},
		{Kind: "cell_detail", BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
	}
	tasks, err := ensurePlannedTasksForTest(ctx, repo, runID, planned, now)
	if err != nil {
		t.Fatalf("落任务失败: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("应写入 3 条任务，实际 %d 条: %+v", len(tasks), tasks)
	}
	for index, task := range tasks {
		if task.Status != SyncTaskPending {
			t.Fatalf("第 %d 条新任务应为 pending，实际 %q", index+1, task.Status)
		}
		if index > 0 && task.TaskID <= tasks[index-1].TaskID {
			t.Fatalf("任务应按 task_id 升序返回: %+v", tasks)
		}
	}
	week20 := findWeekTask(t, tasks, 20)
	week21 := findWeekTask(t, tasks, 21)
	if week20.RoomID != "" || week20.Weekday != 0 || week20.Block != "" {
		t.Fatalf("周矩阵任务字段不符: %+v", week20)
	}
	if week21.BuildingID != "Q10" || week21.Week != 21 {
		t.Fatalf("第 21 周任务字段不符: %+v", week21)
	}
	cell := findCellTask(t, tasks, "r101")
	if cell.Week != 0 || cell.Weekday != 1 || cell.Block != "0102" {
		t.Fatalf("占用说明任务字段不符: %+v", cell)
	}

	requiredTotal, requiredDone, optionalTotal, optionalDone := runTaskCounts(ctx, t, pool, runID)
	if requiredTotal != 2 || requiredDone != 0 || optionalTotal != 1 || optionalDone != 0 {
		t.Fatalf("首次计数不符: required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}

	// 续跑时传入另一份规划：库里已有任务，整批不写，返回的仍是原来的 3 条。
	resumed, err := ensurePlannedTasksForTest(ctx, repo, runID, []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q99", Week: 22},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("续跑落任务失败: %v", err)
	}
	if len(resumed) != 3 {
		t.Fatalf("续跑不应加入新任务，实际 %d 条: %+v", len(resumed), resumed)
	}
	for _, task := range resumed {
		if task.BuildingID == "Q99" || task.Week == 22 {
			t.Fatalf("续跑不应把新规划写进已有轮次: %+v", task)
		}
	}
	requiredTotal, requiredDone, optionalTotal, optionalDone = runTaskCounts(ctx, t, pool, runID)
	if requiredTotal != 2 || requiredDone != 0 || optionalTotal != 1 || optionalDone != 0 {
		t.Fatalf("续跑后计数被改变: required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}
}

// TestCompleteSyncTaskWritesWeekResultAgainstRealDB 覆盖任务原文：
// 周矩阵完成时写 collect_run_week（term、fetched_at、page_sha256），并回写完成计数；
// 重复置 done 不会重复增加计数，重复写周结果只保留一行。
func TestCompleteSyncTaskWritesWeekResultAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	tasks, err := ensurePlannedTasksForTest(ctx, repo, runID, []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
		{Kind: "cell_detail", BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
	}, now)
	if err != nil {
		t.Fatalf("落任务失败: %v", err)
	}

	weekTask := findWeekTask(t, tasks, 20)

	if err := repo.RecordWeekFetch(ctx, RunWeekFetch{
		RunID: runID, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "hash-1",
	}); err != nil {
		t.Fatalf("写周结果失败: %v", err)
	}
	if err := repo.CompleteSyncTask(ctx, runID, weekTask.TaskID, now); err != nil {
		t.Fatalf("置 done 失败: %v", err)
	}

	var term, pageSHA string
	var ok bool
	var fetchedAt time.Time
	err = pool.QueryRow(ctx, `
SELECT term, ok, fetched_at, page_sha256
  FROM collect_run_week
 WHERE run_id = $1 AND week = 20 AND building_id = 'Q10'`, runID).Scan(&term, &ok, &fetchedAt, &pageSHA)
	if err != nil {
		t.Fatalf("读取周结果失败: %v", err)
	}
	if term != syncTaskFixtureTerm || !ok || !fetchedAt.Equal(now) || pageSHA != "hash-1" {
		t.Fatalf("周结果不符: term=%q ok=%v fetched_at=%s sha=%q", term, ok, fetchedAt, pageSHA)
	}

	requiredTotal, requiredDone, _, _ := runTaskCounts(ctx, t, pool, runID)
	if requiredTotal != 1 || requiredDone != 1 {
		t.Fatalf("完成计数不符: required=%d/%d", requiredDone, requiredTotal)
	}

	// 续跑读回时已完成任务保持 done，计数也不被重算回零。
	resumed, err := ensurePlannedTasksForTest(ctx, repo, runID, []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("续跑读任务失败: %v", err)
	}
	if len(resumed) != 2 || findWeekTask(t, resumed, 20).Status != SyncTaskDone {
		t.Fatalf("续跑应保留 done 状态: %+v", resumed)
	}
	_, requiredDone, _, _ = runTaskCounts(ctx, t, pool, runID)
	if requiredDone != 1 {
		t.Fatalf("续跑不应把已完成计数清零，实际 required_done=%d", requiredDone)
	}

	// 重复完成同一条任务：状态已经是 done，计数不应再加。
	if err := repo.CompleteSyncTask(ctx, runID, weekTask.TaskID, now.Add(time.Minute)); err != nil {
		t.Fatalf("重复置 done 失败: %v", err)
	}
	_, requiredDone, _, _ = runTaskCounts(ctx, t, pool, runID)
	if requiredDone != 1 {
		t.Fatalf("重复置 done 不应重复计数，实际 required_done=%d", requiredDone)
	}
	status, attempts, _ := syncTaskStatus(ctx, t, pool, weekTask.TaskID)
	if status != SyncTaskDone || attempts != 1 {
		t.Fatalf("重复完成不应改写状态或增加尝试次数: status=%q attempts=%d", status, attempts)
	}

	// 同一楼同周重写：命中主键，覆盖为最新结果而不是留下两行。
	if err := repo.RecordWeekFetch(ctx, RunWeekFetch{
		RunID: runID, Week: 20, BuildingID: "Q10", FetchedAt: now.Add(time.Minute), PageSHA256: "hash-2",
	}); err != nil {
		t.Fatalf("重写周结果失败: %v", err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM collect_run_week WHERE run_id = $1 AND week = 20 AND building_id = 'Q10'`, runID).Scan(&rows); err != nil {
		t.Fatalf("统计周结果失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("同一楼同周应只有 1 行，实际 %d 行", rows)
	}
	if err := pool.QueryRow(ctx, `
SELECT page_sha256 FROM collect_run_week WHERE run_id = $1 AND week = 20 AND building_id = 'Q10'`, runID).Scan(&pageSHA); err != nil {
		t.Fatalf("读取重写后的哈希失败: %v", err)
	}
	if pageSHA != "hash-2" {
		t.Fatalf("周结果应覆盖为最新哈希，实际 %q", pageSHA)
	}
}

// TestFailSyncTaskKeepsFailedAcrossResumeAgainstRealDB 覆盖 Requirement 11.2 / 8.4：
// 失败任务在同轮内保持 failed，续跑读回同一状态且不重试；可选任务可继续完成。
func TestFailSyncTaskKeepsFailedAcrossResumeAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	planned := []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
		{Kind: "cell_detail", BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
	}
	tasks, err := ensurePlannedTasksForTest(ctx, repo, runID, planned, now)
	if err != nil {
		t.Fatalf("落任务失败: %v", err)
	}

	weekTask := findWeekTask(t, tasks, 20)
	cellTask := findCellTask(t, tasks, "r101")

	if err := repo.FailSyncTask(ctx, runID, weekTask.TaskID, "矩阵结构失败", now); err != nil {
		t.Fatalf("置 failed 失败: %v", err)
	}
	status, attempts, message := syncTaskStatus(ctx, t, pool, weekTask.TaskID)
	if status != SyncTaskFailed || attempts != 1 || message != "矩阵结构失败" {
		t.Fatalf("失败任务状态不符: status=%q attempts=%d message=%q", status, attempts, message)
	}
	_, requiredDone, _, _ := runTaskCounts(ctx, t, pool, runID)
	if requiredDone != 0 {
		t.Fatalf("失败任务不应计入 required_done，实际 %d", requiredDone)
	}

	// 续跑：读回的任务仍是 failed，再置失败也不会增加尝试次数。
	resumed, err := ensurePlannedTasksForTest(ctx, repo, runID, planned, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("续跑读任务失败: %v", err)
	}
	if len(resumed) != 2 || findWeekTask(t, resumed, 20).Status != SyncTaskFailed {
		t.Fatalf("续跑应保留 failed 状态: %+v", resumed)
	}
	if err := repo.FailSyncTask(ctx, runID, weekTask.TaskID, "再次失败", now.Add(time.Minute)); err != nil {
		t.Fatalf("重复置 failed 失败: %v", err)
	}
	status, attempts, message = syncTaskStatus(ctx, t, pool, weekTask.TaskID)
	if status != SyncTaskFailed || attempts != 1 || message != "矩阵结构失败" {
		t.Fatalf("失败任务不应被重试改写: status=%q attempts=%d message=%q", status, attempts, message)
	}

	// 可选任务照常完成，只影响 optional_done。
	if err := repo.CompleteSyncTask(ctx, runID, cellTask.TaskID, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("完成可选任务失败: %v", err)
	}
	requiredTotal, requiredDone, optionalTotal, optionalDone := runTaskCounts(ctx, t, pool, runID)
	if requiredTotal != 1 || requiredDone != 0 || optionalTotal != 1 || optionalDone != 1 {
		t.Fatalf("失败分层前的计数不符: required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}
}

// TestRecordWeekFailureAndSuccessOverwriteAgainstRealDB 覆盖周矩阵失败的落库：
// 失败写 ok=false + 原因；同一唯一键的成功结果覆盖它；再失败又把成功行改回失败，
// 且始终只有一行。term 由 collect_run 取，Go 侧不传。
func TestRecordWeekFailureAndSuccessOverwriteAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	if err := repo.RecordWeekFailure(ctx, RunWeekFailure{
		RunID: runID, Week: 20, BuildingID: "Q10", ErrorMessage: "矩阵结构失败",
	}); err != nil {
		t.Fatalf("写失败周结果失败: %v", err)
	}

	var term, message string
	var ok, fetchedNull, pageNull bool
	readWeek := func() {
		t.Helper()
		err := pool.QueryRow(ctx, `
SELECT term, ok, fetched_at IS NULL, page_sha256 IS NULL, COALESCE(error_message, '')
  FROM collect_run_week
 WHERE run_id = $1 AND week = 20 AND building_id = 'Q10'`, runID).Scan(
			&term, &ok, &fetchedNull, &pageNull, &message)
		if err != nil {
			t.Fatalf("读取周结果失败: %v", err)
		}
	}

	readWeek()
	if term != syncTaskFixtureTerm || ok || !fetchedNull || !pageNull || message != "矩阵结构失败" {
		t.Fatalf("失败周结果不符: term=%q ok=%v fetchedNull=%v pageNull=%v message=%q",
			term, ok, fetchedNull, pageNull, message)
	}

	if err := repo.RecordWeekFetch(ctx, RunWeekFetch{
		RunID: runID, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "hash-1",
	}); err != nil {
		t.Fatalf("成功结果覆盖失败行失败: %v", err)
	}
	readWeek()
	if !ok || fetchedNull || pageNull || message != "" {
		t.Fatalf("成功结果应清掉失败原因: ok=%v fetchedNull=%v pageNull=%v message=%q",
			ok, fetchedNull, pageNull, message)
	}

	if err := repo.RecordWeekFailure(ctx, RunWeekFailure{
		RunID: runID, Week: 20, BuildingID: "Q10", ErrorMessage: "再次结构失败",
	}); err != nil {
		t.Fatalf("失败结果覆盖成功行失败: %v", err)
	}
	readWeek()
	if ok || !fetchedNull || !pageNull || message != "再次结构失败" {
		t.Fatalf("失败行应清掉抓取时刻与哈希: ok=%v fetchedNull=%v pageNull=%v message=%q",
			ok, fetchedNull, pageNull, message)
	}

	var rows int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM collect_run_week WHERE run_id = $1 AND week = 20 AND building_id = 'Q10'`,
		runID).Scan(&rows); err != nil {
		t.Fatalf("统计周结果失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("同一唯一键应只有 1 行，实际 %d 行", rows)
	}
}

// TestSyncTaskWritesRejectMissingRunOrTaskAgainstRealDB 覆盖 0 行写入：
// 轮次不存在、任务不存在都必须返回错误，而不是静默成功。
func TestSyncTaskWritesRejectMissingRunOrTaskAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	tasks, err := ensurePlannedTasksForTest(ctx, repo, runID, []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
		{Kind: "cell_detail", BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
	}, now)
	if err != nil {
		t.Fatalf("落任务失败: %v", err)
	}
	weekTask := findWeekTask(t, tasks, 20)

	const missingID int64 = 1 << 62
	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "周结果-轮次不存在",
			call: func() error {
				return repo.RecordWeekFetch(ctx, RunWeekFetch{
					RunID: missingID, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "hash",
				})
			},
		},
		{
			name: "失败周结果-轮次不存在",
			call: func() error {
				return repo.RecordWeekFailure(ctx, RunWeekFailure{
					RunID: missingID, Week: 20, BuildingID: "Q10", ErrorMessage: "结构失败",
				})
			},
		},
		{
			name: "置 done-轮次不存在",
			call: func() error {
				return repo.CompleteSyncTask(ctx, missingID, weekTask.TaskID, now)
			},
		},
		{
			name: "置 failed-轮次不存在",
			call: func() error {
				return repo.FailSyncTask(ctx, missingID, weekTask.TaskID, "结构失败", now)
			},
		},
		{
			name: "置 done-任务不存在",
			call: func() error {
				return repo.CompleteSyncTask(ctx, runID, missingID, now)
			},
		},
		{
			name: "置 failed-任务不存在",
			call: func() error {
				return repo.FailSyncTask(ctx, runID, missingID, "结构失败", now)
			},
		},
		{
			name: "重算计数-轮次不存在",
			call: func() error {
				return repo.RecountSyncTasks(ctx, missingID, now)
			},
		},
		{
			name: "落任务-轮次不存在且清单为空",
			call: func() error {
				_, err := repo.EnsureWeekMatrixTasks(ctx, missingID, nil, now)
				return err
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.call()
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
			}
		})
	}

	// 失败的任务状态写入不应把已有任务改坏。
	status, attempts, _ := syncTaskStatus(ctx, t, pool, weekTask.TaskID)
	if status != SyncTaskPending || attempts != 0 {
		t.Fatalf("落空写入不应改动任务: status=%q attempts=%d", status, attempts)
	}
}

// TestRecordWeekResultsRejectRunWithoutTermAgainstRealDB 覆盖轮次缺学期：
// 周结果 SQL 影响 0 行并返回可读错误，不撞 collect_run_week.term 的 NOT NULL。
func TestRecordWeekResultsRejectRunWithoutTermAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	// insertRunningCollectRun 不写 term，正好模拟历史/手工建出的无学期轮次。
	runID := insertRunningCollectRun(ctx, t, pool, time.Now())
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	if err := repo.RecordWeekFetch(ctx, RunWeekFetch{
		RunID: runID, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "hash",
	}); !errors.Is(err, model.ErrRunStoreFailed) || !strings.Contains(err.Error(), "学期") {
		t.Fatalf("缺学期时写周结果应报可读错误，实际 %v", err)
	}
	if err := repo.RecordWeekFailure(ctx, RunWeekFailure{
		RunID: runID, Week: 20, BuildingID: "Q10", ErrorMessage: "结构失败",
	}); !errors.Is(err, model.ErrRunStoreFailed) || !strings.Contains(err.Error(), "学期") {
		t.Fatalf("缺学期时写失败周结果应报可读错误，实际 %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM collect_run_week WHERE run_id = $1", runID).Scan(&rows); err != nil {
		t.Fatalf("统计周结果失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("缺学期不应留下周结果行，实际 %d 行", rows)
	}
}

// TestSplitEnsureTasksKeepsWeekMatrixWhenRoomMissingAgainstRealDB 覆盖两段插入的核心：
// 新库里还没有 room 时，周矩阵段照常落库；占用说明段因外键失败也不回滚周矩阵段，
// 等归属写进 room 后重试占用说明段即可成功。整批失败不再发生。
func TestSplitEnsureTasksKeepsWeekMatrixWhenRoomMissingAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewSyncTaskRepo(pool)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	weekTasks, err := repo.EnsureWeekMatrixTasks(ctx, runID, []SyncTaskInput{
		{Kind: "week_matrix", Required: true, BuildingID: "Q10", Week: 20},
	}, now)
	if err != nil {
		t.Fatalf("周矩阵段应在新库落库成功，实际 %v", err)
	}
	if len(weekTasks) != 1 {
		t.Fatalf("周矩阵段应落 1 条，实际 %d 条: %+v", len(weekTasks), weekTasks)
	}

	plannedCells := []SyncTaskInput{
		{Kind: "cell_detail", BuildingID: "Q10", RoomID: "r404", Weekday: 1, Block: "0102"},
	}
	if _, err := repo.EnsureCellDetailTasks(ctx, runID, plannedCells, now); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("缺房号时占用说明段应因外键失败返回 ErrRunStoreFailed，实际 %v", err)
	}

	// 占用说明段失败不能把已落的周矩阵任务带走，也不能留下半截占用说明任务。
	var weekCount, cellCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE kind = 'week_matrix'), count(*) FILTER (WHERE kind = 'cell_detail')
  FROM sync_task WHERE run_id = $1`, runID).Scan(&weekCount, &cellCount); err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	if weekCount != 1 || cellCount != 0 {
		t.Fatalf("周矩阵段应保留 1 条、占用说明段应为 0 条，实际 week=%d cell=%d", weekCount, cellCount)
	}

	// 归属落库（周矩阵响应的结果）之后重试占用说明段：外键满足，任务落库成功。
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id)
VALUES ('r404', 'F404', 'F404', 'Q10')
ON CONFLICT (room_id) DO NOTHING`)
	cells, err := repo.EnsureCellDetailTasks(ctx, runID, plannedCells, now)
	if err != nil {
		t.Fatalf("归属落库后占用说明段应成功，实际 %v", err)
	}
	if len(cells) != 1 || cells[0].RoomID != "r404" {
		t.Fatalf("占用说明段应返回 r404，实际 %+v", cells)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM sync_task WHERE run_id = $1", runID).Scan(&cellCount); err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	if cellCount != 2 {
		t.Fatalf("两段合计应有 2 条任务，实际 %d 条", cellCount)
	}
}
