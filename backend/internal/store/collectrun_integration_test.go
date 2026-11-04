package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// openCollectRunTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 与其它集成测试一样会清空 public schema，只能指向测试库。
func openCollectRunTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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

	// 从干净库开始，确保下面的种子都由本用例负责。
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

// countRunningCollectRuns 数当前进行中轮次，用于断言部分唯一索引的效果。
func countRunningCollectRuns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM collect_run WHERE status = 'running'").Scan(&count); err != nil {
		t.Fatalf("统计进行中轮次失败: %v", err)
	}
	return count
}

// collectRunState 读一轮的状态、错误码与错误原因，并报告是否已收官。
func collectRunState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) (status, errorCode, errorMessage string, finished bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `
SELECT status, COALESCE(error_code, ''), COALESCE(error_message, ''), finished_at IS NOT NULL
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(&status, &errorCode, &errorMessage, &finished)
	if err != nil {
		t.Fatalf("读取轮次 %d 失败: %v", runID, err)
	}
	return status, errorCode, errorMessage, finished
}

// collectRunFixtureTerm 是 collect_run 集成测试使用的学期；collect_run.term 有
// 外键指向 term(term)，建轮前必须先落这一行。
const collectRunFixtureTerm = "2026-2027-1"

// seedCollectRunTerm 落一条学期；重复调用不报错，便于同一用例里多次建轮。
func seedCollectRunTerm(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		collectRunFixtureTerm)
}

// insertRunningCollectRun 跳过仓储直接造一条 running 轮次，用来覆盖任意的 started_at。
func insertRunningCollectRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, startedAt time.Time) int64 {
	t.Helper()
	var runID int64
	err := pool.QueryRow(ctx,
		"INSERT INTO collect_run (started_at, status) VALUES ($1, 'running') RETURNING run_id",
		startedAt).Scan(&runID)
	if err != nil {
		t.Fatalf("插入 running 轮次失败: %v", err)
	}
	return runID
}

// TestCollectRunMutexAgainstRealDB 覆盖 design CP6：部分唯一索引保证同一时刻至多一轮，
// 冲突且学期一致时仓储返回已有轮次；已收官的轮次不再占位。
func TestCollectRunMutexAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	seedCollectRunTerm(ctx, t, pool)
	repo := NewCollectRunRepo(pool)
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	first, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now)
	if err != nil {
		t.Fatalf("首轮创建失败: %v", err)
	}
	if reused {
		t.Fatal("空库首轮不应标记为复用")
	}
	if first.RunID <= 0 || first.Status != "running" {
		t.Fatalf("首轮不符: %+v", first)
	}
	if !first.StartedAt.Equal(now) {
		t.Fatalf("开始时刻期望 %s，实际 %s", now, first.StartedAt)
	}
	var storedTerm string
	if err := pool.QueryRow(ctx, "SELECT term FROM collect_run WHERE run_id = $1", first.RunID).Scan(&storedTerm); err != nil {
		t.Fatalf("读取轮次学期失败: %v", err)
	}
	if storedTerm != collectRunFixtureTerm {
		t.Fatalf("建轮时应写入学期 %q，实际 %q", collectRunFixtureTerm, storedTerm)
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("首轮后应只有 1 条进行中，实际 %d 条", got)
	}

	// 已有一轮在跑：返回同一轮次，且库里仍然只有一条 running。
	existing, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("冲突时应返回已有轮次而不是报错: %v", err)
	}
	if !reused {
		t.Fatal("冲突时应标记为复用")
	}
	if existing.RunID != first.RunID {
		t.Fatalf("应返回已有轮次 %d，实际 %d", first.RunID, existing.RunID)
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("冲突后仍应只有 1 条进行中，实际 %d 条", got)
	}

	// 本轮收官后不再占唯一索引，可以开下一轮。
	mustExec(ctx, t, pool,
		"UPDATE collect_run SET status = 'success', finished_at = $2 WHERE run_id = $1",
		first.RunID, now.Add(2*time.Minute))
	next, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("上一轮已收官时创建新一轮失败: %v", err)
	}
	if reused {
		t.Fatal("上一轮已收官时不应复用")
	}
	if next.RunID == first.RunID {
		t.Fatalf("新一轮编号不应与上一轮相同: %d", next.RunID)
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("新一轮后仍应只有 1 条进行中，实际 %d 条", got)
	}
}

// TestStartCollectRunTermMismatchAgainstRealDB 覆盖用户拍板的口径：冲突后补查到的
// 进行中轮次只有学期一致才复用。不同学期、或旧轮次根本没写学期，都返回
// ErrRunConflict——既不新建第二轮，也不把旧轮次的学期改写成本轮的值。
func TestStartCollectRunTermMismatchAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	seedCollectRunTerm(ctx, t, pool)
	// 下一个学期也先落库：让插入只可能因为部分唯一索引失败，而不是 term 外键。
	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		"2026-2027-2")
	repo := NewCollectRunRepo(pool)
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	first, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now)
	if err != nil {
		t.Fatalf("首轮创建失败: %v", err)
	}
	if reused {
		t.Fatal("空库首轮不应标记为复用")
	}

	// 同为第一学期（两侧空白也要抹平）：复用成功。
	same, reused, err := repo.StartCollectRun(ctx, "  "+collectRunFixtureTerm+"  ", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("学期一致时应复用已有轮次: %v", err)
	}
	if !reused || same.RunID != first.RunID {
		t.Fatalf("学期一致时应复用轮次 %d，实际 reused=%v run=%+v", first.RunID, reused, same)
	}

	// 不同学期：报 ErrRunConflict，不新建，也不改写旧轮次的 term。
	if _, reused, err := repo.StartCollectRun(ctx, "2026-2027-2", now.Add(2*time.Minute)); !errors.Is(err, model.ErrRunConflict) {
		t.Fatalf("学期不一致应返回 ErrRunConflict，实际 reused=%v err=%v", reused, err)
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("学期冲突后仍应只有 1 条进行中，实际 %d 条", got)
	}
	if got := storedCollectRunTerm(ctx, t, pool, first.RunID); got != collectRunFixtureTerm {
		t.Fatalf("冲突不应改写旧轮次的学期，期望 %q，实际 %q", collectRunFixtureTerm, got)
	}

	// 旧轮次 term 为 NULL：同样不可复用，NULL 也不能被本轮学期覆盖。
	mustExec(ctx, t, pool, "UPDATE collect_run SET term = NULL WHERE run_id = $1", first.RunID)
	if _, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now.Add(3*time.Minute)); !errors.Is(err, model.ErrRunConflict) {
		t.Fatalf("旧轮次没写学期时应返回 ErrRunConflict，实际 reused=%v err=%v", reused, err)
	}
	if got := storedCollectRunTerm(ctx, t, pool, first.RunID); got != "" {
		t.Fatalf("旧轮次的空学期不应被改写，实际 %q", got)
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("空学期冲突后仍应只有 1 条进行中，实际 %d 条", got)
	}
}

// storedCollectRunTerm 读轮次的学期，NULL 折成空串。
func storedCollectRunTerm(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) string {
	t.Helper()
	var term string
	if err := pool.QueryRow(ctx, "SELECT COALESCE(term, '') FROM collect_run WHERE run_id = $1", runID).Scan(&term); err != nil {
		t.Fatalf("读取轮次 %d 的学期失败: %v", runID, err)
	}
	return term
}

// TestCollectRunMutexUnderConcurrencyAgainstRealDB 覆盖 CP6 的并发路径：N 个 goroutine
// 同一刻开始一轮，唯一索引决定胜负——恰好一个新建成功，其余全部复用同一个轮次编号，
// 库里始终只有一条 running。顺序用例见 TestCollectRunMutexAgainstRealDB。
func TestCollectRunMutexUnderConcurrencyAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	seedCollectRunTerm(ctx, t, pool)
	repo := NewCollectRunRepo(pool)
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

	// 并发度不必很大：插入都必须落在同一条部分唯一索引上，撞在一起才能验证互斥。
	const concurrency = 16
	type startResult struct {
		run    CollectRun
		reused bool
		err    error
	}

	results := make([]startResult, concurrency)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(i int) {
			defer wg.Done()
			<-gate // 同一刻放行，尽量让 INSERT 撞在唯一索引上
			run, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now)
			results[i] = startResult{run: run, reused: reused, err: err}
		}(i)
	}
	close(gate)
	wg.Wait()

	var createdRunID int64
	createdCount := 0
	for i, result := range results {
		if result.err != nil {
			t.Fatalf("第 %d 个并发开始报错: %v", i, result.err)
		}
		if result.run.Status != "running" {
			t.Fatalf("第 %d 个并发开始的状态期望 running，实际 %q", i, result.run.Status)
		}
		if !result.reused {
			createdCount++
			createdRunID = result.run.RunID
		}
	}

	if createdCount != 1 {
		t.Fatalf("并发开始应恰好新建 1 次，实际 %d 次", createdCount)
	}
	if createdRunID <= 0 {
		t.Fatalf("新建轮次编号应为正，实际 %d", createdRunID)
	}

	for i, result := range results {
		if !result.reused {
			continue
		}
		if result.run.RunID != createdRunID {
			t.Fatalf("第 %d 个并发开始应复用轮次 %d，实际 %d", i, createdRunID, result.run.RunID)
		}
	}
	if got := countRunningCollectRuns(ctx, t, pool); got != 1 {
		t.Fatalf("并发结束后应只有 1 条进行中轮次，实际 %d 条", got)
	}

	// 并发只应产生一条新行：新建一次、其余复用，库里 total 恰好 1。
	var total int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM collect_run").Scan(&total); err != nil {
		t.Fatalf("统计轮次总数失败: %v", err)
	}
	if total != 1 {
		t.Fatalf("并发开始后库里应只有 1 条轮次，实际 %d 条", total)
	}
}

// TestFailStaleRunsAgainstRealDB 覆盖 Requirement 11.4 / 8.3：超过 2 小时的 running
// 被置为 failed 并记 stale_run；恰好 2 小时不算过期；收敛后可以开新一轮。
func TestFailStaleRunsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	seedCollectRunTerm(ctx, t, pool)
	repo := NewCollectRunRepo(pool)
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	cutoff := now.Add(-2 * time.Hour)

	t.Run("超过两小时被收敛", func(t *testing.T) {
		runID := insertRunningCollectRun(ctx, t, pool, now.Add(-3*time.Hour))

		runIDs, err := repo.FailStaleRuns(ctx, now, cutoff)
		if err != nil {
			t.Fatalf("收敛过期轮次失败: %v", err)
		}
		if len(runIDs) != 1 || runIDs[0] != runID {
			t.Fatalf("被收敛的轮次期望 [%d]，实际 %v", runID, runIDs)
		}

		status, errorCode, errorMessage, finished := collectRunState(ctx, t, pool, runID)
		if status != "failed" {
			t.Fatalf("过期轮次状态期望 failed，实际 %q", status)
		}
		if errorCode != "stale_run" {
			t.Fatalf("错误码期望 stale_run，实际 %q", errorCode)
		}
		if errorMessage != "过期轮次" {
			t.Fatalf("错误原因期望「过期轮次」，实际 %q", errorMessage)
		}
		if !finished {
			t.Fatal("过期轮次应写入 finished_at")
		}
		// minor 4：过期收敛与收口是同一类终止写入，phase 必须同步到 finished，
		// 不能停在崩溃时那一格（这里默认是 login）。
		if _, phase, _, _, _, _, _, _, _ := collectRunPhaseState(ctx, t, pool, runID); phase != "finished" {
			t.Fatalf("过期轮次的阶段应同步为 finished，实际 %q", phase)
		}
		if got := countRunningCollectRuns(ctx, t, pool); got != 0 {
			t.Fatalf("收敛后不应再有进行中轮次，实际 %d 条", got)
		}
	})

	t.Run("未满两小时不收敛", func(t *testing.T) {
		runID := insertRunningCollectRun(ctx, t, pool, now.Add(-time.Hour))

		runIDs, err := repo.FailStaleRuns(ctx, now, cutoff)
		if err != nil {
			t.Fatalf("收敛过期轮次失败: %v", err)
		}
		if len(runIDs) != 0 {
			t.Fatalf("未满两小时不应被收敛，实际 %v", runIDs)
		}
		status, _, _, finished := collectRunState(ctx, t, pool, runID)
		if status != "running" || finished {
			t.Fatalf("未满两小时的轮次应保持 running，实际 status=%q finished=%v", status, finished)
		}

		// 边界：恰好两小时仍不算「超过」，必须严格早于截止时刻。
		mustExec(ctx, t, pool, "UPDATE collect_run SET started_at = $2 WHERE run_id = $1", runID, now.Add(-2*time.Hour))
		runIDs, err = repo.FailStaleRuns(ctx, now, cutoff)
		if err != nil {
			t.Fatalf("边界收敛失败: %v", err)
		}
		if len(runIDs) != 0 {
			t.Fatalf("恰好两小时不应被收敛，实际 %v", runIDs)
		}

		// 再早一秒即过期。
		mustExec(ctx, t, pool, "UPDATE collect_run SET started_at = $2 WHERE run_id = $1", runID, now.Add(-2*time.Hour-time.Second))
		runIDs, err = repo.FailStaleRuns(ctx, now, cutoff)
		if err != nil {
			t.Fatalf("边界外收敛失败: %v", err)
		}
		if len(runIDs) != 1 || runIDs[0] != runID {
			t.Fatalf("超过两小时应被收敛为 [%d]，实际 %v", runID, runIDs)
		}
	})

	t.Run("收敛后可以开新一轮", func(t *testing.T) {
		next, reused, err := repo.StartCollectRun(ctx, collectRunFixtureTerm, now)
		if err != nil {
			t.Fatalf("收敛后创建新一轮失败: %v", err)
		}
		if reused || next.Status != "running" {
			t.Fatalf("收敛后应新建 running 轮次，实际 reused=%v run=%+v", reused, next)
		}
	})
}
