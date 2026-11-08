package store

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// openRunStatusTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 与其它集成测试一样会清空 public schema，只能指向测试库。
func openRunStatusTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过真实库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	// 从干净库开始，确保下面的轮次与学期种子都由本文件负责。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// runStatusFixtureTerm 是本文件用的学期；collect_run.term 有外键指向 term(term)。
const runStatusFixtureTerm = "2026-2027-1"

// insertRunStatusRun 直接落一条带学期的终态轮次，返回编号。
// status 取 running 之外的值，避免撞 collect_run_single_running；phase 固定 finished，
// 失败/成功都已收官。term 传 nil 可以造出「没有学期」的行，但 blocked 轮次不在这里
// 手工构造：它建轮即终态、finished_at 与 error_code 都有值，形状由 StartBlockedRun
// 决定（见下面的种子与 StartBlockedRun 集成用例）。
func insertRunStatusRun(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	startedAt time.Time,
	status string,
	term *string,
) int64 {
	t.Helper()

	var runID int64
	err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, phase, term)
VALUES ($1, $2, 'finished', $3)
RETURNING run_id`, startedAt, status, term).Scan(&runID)
	if err != nil {
		t.Fatalf("插入轮次失败: %v", err)
	}
	return runID
}

// runSummaryIDs 取出摘要的编号序列，便于按顺序断言。
func runSummaryIDs(summaries []RunSummary) []int64 {
	ids := make([]int64, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.RunID)
	}
	return ids
}

// seedRunStatusTerm 落一条学期，供 StartCollectRun 建轮使用。
func seedRunStatusTerm(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		runStatusFixtureTerm)
}

// TestRunStatusRepoAgainstRealDB 覆盖 RunStatusRepo 的真实 SQL 口径：
// 空表、排序与分页、未知 id、周结果顺序、blocked 建轮后仍能开 running 轮次。
//
// 子测试按声明顺序执行且有状态依赖（后面的用例基于前面的种子），因此不并行。
func TestRunStatusRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openRunStatusTestDB(t)
	repo := NewRunStatusRepo(pool)
	base := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

	t.Run("空表", func(t *testing.T) {
		summaries, total, err := repo.Recent(ctx, 20, 0)
		if err != nil {
			t.Fatalf("空表读取轮次列表失败: %v", err)
		}
		if summaries == nil {
			t.Fatal("空表应返回空切片而不是 nil")
		}
		if len(summaries) != 0 || total != 0 {
			t.Fatalf("空表应返回空列表与 total=0，实际 %d 条 / total=%d", len(summaries), total)
		}
		if _, found, err := repo.ByID(ctx, 1); err != nil || found {
			t.Fatalf("空表按 id 读取应返回 found=false 且无错误，实际 found=%v err=%v", found, err)
		}
		if _, found, err := repo.Running(ctx); err != nil || found {
			t.Fatalf("空表不应有进行中轮次，实际 found=%v err=%v", found, err)
		}
		weeks, err := repo.Weeks(ctx, 1)
		if err != nil {
			t.Fatalf("空表读取周结果失败: %v", err)
		}
		if weeks == nil || len(weeks) != 0 {
			t.Fatalf("空表周结果应为空切片，实际 %+v", weeks)
		}
	})

	// 四条终态轮次：A 最早；B 与 C 同一开始时刻（C 的 run_id 更大）；D 最晚。
	// 带学期的轮次有外键，先把学期种下去。
	seedRunStatusTerm(ctx, t, pool)
	term := runStatusFixtureTerm
	runA := insertRunStatusRun(ctx, t, pool, base, "failed", &term)
	runB := insertRunStatusRun(ctx, t, pool, base.Add(time.Hour), "success", &term)
	// C 是 blocked：走 startBlockedRunSQL 这条真实落库路径建，而不是手工 INSERT。
	// blocked 建轮即终态，finished_at 与 error_code 都有值、term 仍为 NULL——手工造一条
	// finished_at 为 NULL 的终态行，会让「终态带结束时刻」这条断言失去意义。
	runC, _, err := repo.StartBlockedRun(ctx, "白名单为空", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("创建 blocked 种子轮次失败: %v", err)
	}
	runD := insertRunStatusRun(ctx, t, pool, base.Add(2*time.Hour), "failed", &term)

	t.Run("排序与分页", func(t *testing.T) {
		summaries, total, err := repo.Recent(ctx, 20, 0)
		if err != nil {
			t.Fatalf("读取轮次列表失败: %v", err)
		}
		if total != 4 {
			t.Fatalf("总数期望 4，实际 %d", total)
		}
		// 同刻的 B/C 按 run_id 倒序（C 在前），不能依赖数据库的任意行序。
		want := []int64{runD, runC, runB, runA}
		if got := runSummaryIDs(summaries); !slices.Equal(got, want) {
			t.Fatalf("排序期望 %v，实际 %v", want, got)
		}
		// B 与 C 的开始时刻确实相同：这一条让上面的顺序断言真的在测 run_id 兜底。
		if !summaries[1].StartedAt.Equal(summaries[2].StartedAt) {
			t.Fatalf("B 与 C 应是同一开始时刻，实际 %s / %s",
				summaries[1].StartedAt, summaries[2].StartedAt)
		}
		if summaries[1].Term != nil {
			t.Fatalf("blocked 轮的 term 应为 nil，实际 %q", *summaries[1].Term)
		}
		if summaries[1].Status != "blocked" || summaries[1].Phase != "finished" {
			t.Fatalf("blocked 轮状态/阶段不符: %+v", summaries[1])
		}
		// blocked 建轮即终态：结束时刻与错误码都该有值，不能是一条「终态但没有收官」的行。
		if summaries[1].FinishedAt == nil || summaries[1].ErrorCode == nil {
			t.Fatalf("blocked 轮应有 finished_at 与 error_code，实际 %+v", summaries[1])
		}
		if *summaries[1].ErrorCode != whitelistEmptyErrorCode {
			t.Fatalf("blocked 轮错误码期望 %q，实际 %q", whitelistEmptyErrorCode, *summaries[1].ErrorCode)
		}

		// LIMIT/OFFSET 落在稳定序上：offset=1 取 C/B。
		page, pageTotal, err := repo.Recent(ctx, 2, 1)
		if err != nil {
			t.Fatalf("读取第二页失败: %v", err)
		}
		if pageTotal != 4 {
			t.Fatalf("分页时总数仍应是全表 4，实际 %d", pageTotal)
		}
		if wantPage := []int64{runC, runB}; !slices.Equal(runSummaryIDs(page), wantPage) {
			t.Fatalf("第二页期望 %v，实际 %v", wantPage, runSummaryIDs(page))
		}

		last, lastTotal, err := repo.Recent(ctx, 1, 3)
		if err != nil {
			t.Fatalf("读取最后一页失败: %v", err)
		}
		if lastTotal != 4 || !slices.Equal(runSummaryIDs(last), []int64{runA}) {
			t.Fatalf("最后一页期望 [%d]/total=4，实际 %v/total=%d", runA, runSummaryIDs(last), lastTotal)
		}

		beyond, beyondTotal, err := repo.Recent(ctx, 10, 10)
		if err != nil {
			t.Fatalf("读取越界页失败: %v", err)
		}
		if beyondTotal != 4 || len(beyond) != 0 || beyond == nil {
			t.Fatalf("越界页应为空切片且 total=4，实际 %+v/total=%d", beyond, beyondTotal)
		}
	})

	t.Run("未知 id", func(t *testing.T) {
		unknownID := runD + 1000

		summary, found, err := repo.ByID(ctx, unknownID)
		if err != nil {
			t.Fatalf("未知 id 不应返回错误: %v", err)
		}
		if found {
			t.Fatalf("未知 id 应返回 found=false，实际 %+v", summary)
		}
		if summary != (RunSummary{}) {
			t.Fatalf("未知 id 应返回零值摘要，实际 %+v", summary)
		}

		// 任务汇总没有 bool 通道：轮次不存在必须报错，不能给一份全零汇总。
		if _, err := repo.TaskSummary(ctx, unknownID); err == nil {
			t.Fatal("未知 id 的任务汇总应报错")
		} else if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("任务汇总错误应包 ErrRunStoreFailed，实际 %v", err)
		}
	})

	t.Run("TaskSummary 复用包内实现", func(t *testing.T) {
		summary, err := repo.TaskSummary(ctx, runD)
		if err != nil {
			t.Fatalf("读取任务汇总失败: %v", err)
		}
		// 这条轮次没有 sync_task 行：聚合返回一张全零表，但仍必须证明轮次存在。
		if summary != (RunTaskSummary{}) {
			t.Fatalf("没有任务时应返回零值汇总，实际 %+v", summary)
		}
	})

	t.Run("周结果顺序", func(t *testing.T) {
		fetched := base.Add(3 * time.Hour)
		timeout := "抓取超时"
		structure := "页面结构不符合预期"
		insertWeek := func(week int, buildingID string, ok bool, fetchedAt *time.Time, message *string) {
			t.Helper()
			mustExec(ctx, t, pool, `
INSERT INTO collect_run_week (run_id, term, week, building_id, ok, fetched_at, error_message)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				runD, term, week, buildingID, ok, fetchedAt, message)
		}

		// 故意乱序插入：结果顺序必须由 ORDER BY week, building_id 决定，而不是插入顺序。
		insertWeek(3, "Q11", true, &fetched, nil)
		insertWeek(2, "Q10", true, &fetched, nil)
		insertWeek(3, "Q10", false, nil, &structure)
		insertWeek(1, "Q10", true, &fetched, nil)
		insertWeek(2, "Q11", false, nil, &timeout)

		weeks, err := repo.Weeks(ctx, runD)
		if err != nil {
			t.Fatalf("读取周结果失败: %v", err)
		}
		type wantWeek struct {
			week       int
			buildingID string
			ok         bool
			hasFetched bool
			message    *string
		}
		want := []wantWeek{
			{week: 1, buildingID: "Q10", ok: true, hasFetched: true},
			{week: 2, buildingID: "Q10", ok: true, hasFetched: true},
			{week: 2, buildingID: "Q11", ok: false, message: &timeout},
			{week: 3, buildingID: "Q10", ok: false, message: &structure},
			{week: 3, buildingID: "Q11", ok: true, hasFetched: true},
		}
		if len(weeks) != len(want) {
			t.Fatalf("周结果期望 %d 条，实际 %d 条: %+v", len(want), len(weeks), weeks)
		}
		for i := range want {
			got := weeks[i]
			if got.Week != want[i].week || got.BuildingID != want[i].buildingID || got.OK != want[i].ok {
				t.Fatalf("第 %d 条周结果期望 %+v，实际 %+v", i, want[i], got)
			}
			if (got.FetchedAt != nil) != want[i].hasFetched {
				t.Fatalf("第 %d 条 fetched_at 期望存在=%v，实际 %+v", i, want[i].hasFetched, got.FetchedAt)
			}
			if got.FetchedAt != nil && !got.FetchedAt.Equal(fetched) {
				t.Fatalf("第 %d 条 fetched_at 期望 %s，实际 %s", i, fetched, got.FetchedAt)
			}
			if (got.ErrorMessage == nil) != (want[i].message == nil) ||
				(got.ErrorMessage != nil && *got.ErrorMessage != *want[i].message) {
				t.Fatalf("第 %d 条 error_message 不符: %+v", i, got.ErrorMessage)
			}
		}

		// 没有周结果的轮次返回空切片，而不是 nil。
		empty, err := repo.Weeks(ctx, runA)
		if err != nil {
			t.Fatalf("读取空周结果失败: %v", err)
		}
		if empty == nil || len(empty) != 0 {
			t.Fatalf("没有周结果时应返回空切片，实际 %+v", empty)
		}
	})

	t.Run("blocked 后 StartCollectRun 仍能成功", func(t *testing.T) {
		now := base.Add(24 * time.Hour)
		blockedID, startedAt, err := repo.StartBlockedRun(ctx, "白名单为空", now)
		if err != nil {
			t.Fatalf("创建阻止轮次失败: %v", err)
		}
		if blockedID <= 0 {
			t.Fatalf("阻止轮次应有编号，实际 %d", blockedID)
		}
		if !startedAt.Equal(now) {
			t.Fatalf("阻止轮次的 started_at 期望 %s，实际 %s", now, startedAt)
		}

		// 第二次 blocked 也成功：blocked 轮次不占 collect_run_single_running。
		secondReason := "白名单为空（重试）"
		secondID, secondStartedAt, err := repo.StartBlockedRun(ctx, secondReason, now.Add(time.Minute))
		if err != nil {
			t.Fatalf("第二次创建阻止轮次失败: %v", err)
		}
		if secondID == blockedID {
			t.Fatalf("两次阻止轮次应是不同行，实际都是 %d", blockedID)
		}
		if !secondStartedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("第二次 started_at 不符: %s", secondStartedAt)
		}

		// 直接查库钉住 blocked 的形状：终态、固定错误码、三个时刻一致、term 为 NULL。
		var (
			status, phase, errorCode, errorMessage string
			rowStarted, rowUpdated, rowFinished    time.Time
			rowTerm                                *string
		)
		err = pool.QueryRow(ctx, `
SELECT status, phase, error_code, error_message, started_at, updated_at, finished_at, term
  FROM collect_run
 WHERE run_id = $1`, blockedID).Scan(
			&status, &phase, &errorCode, &errorMessage,
			&rowStarted, &rowUpdated, &rowFinished, &rowTerm)
		if err != nil {
			t.Fatalf("读取阻止轮次失败: %v", err)
		}
		if status != "blocked" || phase != "finished" {
			t.Fatalf("阻止轮次应是 blocked/finished，实际 %s/%s", status, phase)
		}
		if errorCode != whitelistEmptyErrorCode || errorMessage != "白名单为空" {
			t.Fatalf("阻止轮次错误码/原因不符: %q / %q", errorCode, errorMessage)
		}
		if rowTerm != nil {
			t.Fatalf("阻止轮次的 term 应为 NULL，实际 %q", *rowTerm)
		}
		if !rowStarted.Equal(now) || !rowUpdated.Equal(now) || !rowFinished.Equal(now) {
			t.Fatalf("阻止轮次三个时刻应同为 %s，实际 %s / %s / %s",
				now, rowStarted, rowUpdated, rowFinished)
		}

		// blocked 建轮之后仍能开一轮 running：唯一索引没有被阻止轮次占用。
		seedRunStatusTerm(ctx, t, pool)
		run, reused, err := StartCollectRun(ctx, pool, runStatusFixtureTerm, now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("blocked 之后开轮失败（不应占唯一索引）: %v", err)
		}
		if reused {
			t.Fatal("blocked 之后开轮不应复用已有轮次")
		}
		if run.Status != "running" {
			t.Fatalf("新建轮次状态应为 running，实际 %q", run.Status)
		}

		running, found, err := repo.Running(ctx)
		if err != nil {
			t.Fatalf("读取进行中轮次失败: %v", err)
		}
		if !found || running.RunID != run.RunID {
			t.Fatalf("进行中轮次期望 %d，实际 found=%v %+v", run.RunID, found, running)
		}
		if running.Term == nil || *running.Term != runStatusFixtureTerm {
			t.Fatalf("进行中轮次的 term 期望 %q，实际 %v", runStatusFixtureTerm, running.Term)
		}

		// Requirement 8.6：新建轮次立刻出现在列表首条（开始时刻最晚）。
		first, total, err := repo.Recent(ctx, 1, 0)
		if err != nil {
			t.Fatalf("读取列表首条失败: %v", err)
		}
		if total != 7 {
			t.Fatalf("此时应有 7 条轮次（4 条种子 + 2 条 blocked + 1 条 running），实际 %d", total)
		}
		if len(first) != 1 || first[0].RunID != run.RunID {
			t.Fatalf("列表首条期望 %d，实际 %+v", run.RunID, first)
		}
	})

	// 以下两个子测试各自插一行轮次，放在所有 total 断言之后。
	t.Run("blocked 空串原因原样落空串", func(t *testing.T) {
		now := base.Add(48 * time.Hour)
		runID, _, err := repo.StartBlockedRun(ctx, "", now)
		if err != nil {
			t.Fatalf("创建空原因的阻止轮次失败: %v", err)
		}

		// error_message 扫进指针：折成 NULL 时这里是 nil，空串时是非 nil 的 ""。
		var (
			errorCode    string
			errorMessage *string
		)
		err = pool.QueryRow(ctx, `
SELECT error_code, error_message
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(&errorCode, &errorMessage)
		if err != nil {
			t.Fatalf("读取阻止轮次失败: %v", err)
		}
		if errorCode != whitelistEmptyErrorCode {
			t.Fatalf("错误码期望 %q，实际 %q", whitelistEmptyErrorCode, errorCode)
		}
		// 空串不是 NULL：仓储不做 NULLIF，非空原因由调用方保证（见 StartBlockedRun 注释）。
		if errorMessage == nil || *errorMessage != "" {
			t.Fatalf("空串原因应落成空串而不是 NULL，实际 %v", errorMessage)
		}
	})

	t.Run("已发布轮次的 release_id 与 released_at", func(t *testing.T) {
		// 前面的种子轮次都把这两列留成 NULL，只走到指针扫描的 nil 分支；
		// 这里补一条非空行，覆盖 **string / **time.Time 的值分支。
		releaseID := "rel-runstatus-published"
		startedAt := base.Add(72 * time.Hour)
		releasedAt := startedAt.Add(time.Hour) // 与 started_at 错开，列序写反时能被发现

		mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, 1, 1, false)`, releaseID, term, releasedAt)

		var runID int64
		err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, phase, term, release_id, released_at)
VALUES ($1, 'success', 'publish', $2, $3, $4)
RETURNING run_id`, startedAt, term, releaseID, releasedAt).Scan(&runID)
		if err != nil {
			t.Fatalf("插入已发布轮次失败: %v", err)
		}

		summary, found, err := repo.ByID(ctx, runID)
		if err != nil {
			t.Fatalf("读取已发布轮次失败: %v", err)
		}
		if !found {
			t.Fatalf("已发布轮次应返回 found=true，实际 run_id=%d", runID)
		}
		if summary.ReleaseID == nil || *summary.ReleaseID != releaseID {
			t.Fatalf("release_id 期望 %q，实际 %v", releaseID, summary.ReleaseID)
		}
		if summary.ReleasedAt == nil || !summary.ReleasedAt.Equal(releasedAt) {
			t.Fatalf("released_at 期望 %s，实际 %v", releasedAt, summary.ReleasedAt)
		}

		// 列表与 ByID 共用同一份列序，顺带确认 Recent 也扫出非 NULL 值而不是 nil。
		summaries, _, err := repo.Recent(ctx, 100, 0)
		if err != nil {
			t.Fatalf("读取轮次列表失败: %v", err)
		}
		idx := slices.IndexFunc(summaries, func(s RunSummary) bool { return s.RunID == runID })
		if idx < 0 {
			t.Fatalf("已发布轮次应出现在列表里，实际 %v", runSummaryIDs(summaries))
		}
		if summaries[idx].ReleaseID == nil || summaries[idx].ReleasedAt == nil {
			t.Fatalf("列表里的已发布轮次应保留两列非空，实际 %+v", summaries[idx])
		}
	})

	t.Run("全列非零行的 22 列列序", func(t *testing.T) {
		// 22 列全部取非零 / 非空值，且同类型的相邻列各取不同的值：runSummaryColumns
		// 与 scanRunSummary 的列序一旦写反、漏列或多列，下面的逐字段断言就会读到邻列
		// 的值并报错。前面的种子只覆盖指针列为 NULL 的分支，这条补上「每个可空列都有值」
		// 以及「22 列在真实 SQL 里的顺序」。
		//
		// 这是一条逐列取互不相同值的合成行，只用于钉住列序：真实轮次里 release_id 与
		// error_code 一般不会同时非空，这里刻意都填上，才能一次锁住 22 列的位置。
		// ByID 走 runSummaryByIDSQL，而该 SQL 与 Recent、Running 共用同一个列常量与
		// scanRunSummary，所以这一条断言同时覆盖三处的列序。
		startedAt := base.Add(96 * time.Hour)
		finishedAt := startedAt.Add(7 * time.Minute) // 与 started_at / updated_at 都不同
		updatedAt := startedAt.Add(9 * time.Minute)  // 收官之后仍有写入
		releasedAt := startedAt.Add(5 * time.Minute) // 发布发生在收官之前
		releaseID := "rel-runstatus-full-columns"
		status, phase := "partial", "finished"
		termValue := runStatusFixtureTerm
		windowStart, windowEnd := 3, 17
		weeksTotal, roomsTotal := 15, 37
		requiredTotal, requiredDone := 41, 39
		optionalTotal, optionalDone := 53, 47
		unknownCells, compositeCells, attrConflictCount := 5, 7, 2
		errorCode := "cell_detail_failed"
		errorMessage := "格子详情结构不符合预期"

		mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, 1, 1, false)`, releaseID, termValue, releasedAt)

		var runID int64
		err := pool.QueryRow(ctx, `
INSERT INTO collect_run (
  started_at, finished_at, updated_at, status, phase, term,
  window_start, window_end, weeks_total, rooms_total,
  required_total, required_done, optional_total, optional_done,
  unknown_cells, composite_cells, attr_conflict_count,
  release_id, released_at, error_code, error_message)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
        $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
RETURNING run_id`,
			startedAt, finishedAt, updatedAt, status, phase, termValue,
			windowStart, windowEnd, weeksTotal, roomsTotal,
			requiredTotal, requiredDone, optionalTotal, optionalDone,
			unknownCells, compositeCells, attrConflictCount,
			releaseID, releasedAt, errorCode, errorMessage).Scan(&runID)
		if err != nil {
			t.Fatalf("插入全列非零轮次失败: %v", err)
		}

		summary, found, err := repo.ByID(ctx, runID)
		if err != nil {
			t.Fatalf("读取全列非零轮次失败: %v", err)
		}
		if !found {
			t.Fatalf("全列非零轮次应返回 found=true，实际 run_id=%d", runID)
		}

		// 逐字段断言：一组断言对应一类列，错位时能直接看出是哪几列换了位置。
		if summary.RunID != runID || summary.Status != status || summary.Phase != phase {
			t.Fatalf("编号/状态/阶段期望 %d/%q/%q，实际 %d/%q/%q",
				runID, status, phase, summary.RunID, summary.Status, summary.Phase)
		}
		if !summary.StartedAt.Equal(startedAt) {
			t.Fatalf("started_at 期望 %s，实际 %s", startedAt, summary.StartedAt)
		}
		if summary.FinishedAt == nil || !summary.FinishedAt.Equal(finishedAt) {
			t.Fatalf("finished_at 期望 %s，实际 %v", finishedAt, summary.FinishedAt)
		}
		if !summary.UpdatedAt.Equal(updatedAt) {
			t.Fatalf("updated_at 期望 %s，实际 %s", updatedAt, summary.UpdatedAt)
		}
		if summary.Term == nil || *summary.Term != termValue {
			t.Fatalf("term 期望 %q，实际 %v", termValue, summary.Term)
		}
		if !sameOptionalInt(summary.WindowStart, &windowStart) ||
			!sameOptionalInt(summary.WindowEnd, &windowEnd) {
			t.Fatalf("窗口期望 [%d,%d]，实际 %v/%v",
				windowStart, windowEnd, summary.WindowStart, summary.WindowEnd)
		}
		counters := []struct {
			column string
			got    int
			want   int
		}{
			{"weeks_total", summary.WeeksTotal, weeksTotal},
			{"rooms_total", summary.RoomsTotal, roomsTotal},
			{"required_total", summary.RequiredTotal, requiredTotal},
			{"required_done", summary.RequiredDone, requiredDone},
			{"optional_total", summary.OptionalTotal, optionalTotal},
			{"optional_done", summary.OptionalDone, optionalDone},
			{"unknown_cells", summary.UnknownCells, unknownCells},
			{"composite_cells", summary.CompositeCells, compositeCells},
			{"attr_conflict_count", summary.AttrConflictCount, attrConflictCount},
		}
		for _, counter := range counters {
			if counter.got != counter.want {
				t.Fatalf("%s 期望 %d，实际 %d（列序写反时会读到相邻列的计数）",
					counter.column, counter.want, counter.got)
			}
		}
		if summary.ReleaseID == nil || *summary.ReleaseID != releaseID {
			t.Fatalf("release_id 期望 %q，实际 %v", releaseID, summary.ReleaseID)
		}
		if summary.ReleasedAt == nil || !summary.ReleasedAt.Equal(releasedAt) {
			t.Fatalf("released_at 期望 %s，实际 %v", releasedAt, summary.ReleasedAt)
		}
		if summary.ErrorCode == nil || *summary.ErrorCode != errorCode {
			t.Fatalf("error_code 期望 %q，实际 %v", errorCode, summary.ErrorCode)
		}
		if summary.ErrorMessage == nil || *summary.ErrorMessage != errorMessage {
			t.Fatalf("error_message 期望 %q，实际 %v", errorMessage, summary.ErrorMessage)
		}
	})
}
