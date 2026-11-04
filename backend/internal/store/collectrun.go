package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// CollectRun 是一轮采集在 collect_run 里的最小身份：编号、开始时刻与状态。
// 只覆盖「开始一轮」需要的列；窗口、计数与发布结果由后续任务按需扩展，
// 不在这里一次性摊开整张表。
type CollectRun struct {
	RunID     int64     // 轮次编号，collect_run.run_id
	StartedAt time.Time // 本轮开始时刻，collect_run.started_at
	Status    string    // 轮次状态；本仓储产出时恒为 running
}

// collectRunQuerier 只取 collect_run 读写需要的两种能力，便于用替身测试。
type collectRunQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// collectRunSingleRunningIndex 是 collect_run 上的部分唯一索引名：
// status='running' 的行至多一行，由它保证 design CP6「同一时刻至多一个 running」。
const collectRunSingleRunningIndex = "collect_run_single_running"

// pgUniqueViolation 是 PostgreSQL 唯一约束冲突的 SQLSTATE。
const pgUniqueViolation = "23505"

// 过期收敛写入 collect_run 的两列：error_code 按设计固定为 stale_run，
// 原因文案与原 spec Requirement 11.4、PRD 4.3 的「过期轮次」保持一致。
const (
	staleRunErrorCode    = "stale_run"
	staleRunErrorMessage = "过期轮次"
)

// startCollectRunSQL 插入一条 running 轮次。其余列全部走表默认值；
// status='running' 会触发 collect_run_single_running 部分唯一索引。
const startCollectRunSQL = `
INSERT INTO collect_run (started_at, status)
VALUES ($1, 'running')
RETURNING run_id, started_at, status`

// runningCollectRunSQL 取当前进行中轮次；唯一索引保证至多一行，
// 仍加 ORDER BY 与 LIMIT 1 是为了索引被误删时输出仍然确定。
const runningCollectRunSQL = `
SELECT run_id, started_at, status
  FROM collect_run
 WHERE status = 'running'
 ORDER BY run_id
 LIMIT 1`

// failStaleRunsSQL 把 started_at 严格早于 cutoff 的 running 轮次置为 failed。
// 判定看 started_at 而不是 updated_at：崩溃留下的轮次可能从未更新过 updated_at。
// 终止条件用「<」而不是「<=」：恰好两小时不算「超过」，与 Requirement 11.4 的措辞一致。
const failStaleRunsSQL = `
UPDATE collect_run
   SET status = 'failed',
       finished_at = $1,
       updated_at = $1,
       error_code = '` + staleRunErrorCode + `',
       error_message = '` + staleRunErrorMessage + `'
 WHERE status = 'running'
   AND started_at < $2
RETURNING run_id`

// StartCollectRun 尝试开始一轮采集，返回本轮轮次、是否复用了已有轮次与错误。
//
// 第二个返回值 reused 为 false 表示本轮新建；为 true 表示撞上唯一索引、
// 复用了已有的进行中轮次。冲突时不再插入、也不重试，只把已有 running 轮次
// 原样返回（Requirement 8.3、design CP6）。
//
// 只应传连接池或单语句连接，不要传已开启的事务：唯一索引冲突会让事务进入
// aborted 状态，冲突后的那次 SELECT 将无法执行。
func StartCollectRun(ctx context.Context, q collectRunQuerier, startedAt time.Time) (CollectRun, bool, error) {
	var run CollectRun
	err := q.QueryRow(ctx, startCollectRunSQL, startedAt).Scan(&run.RunID, &run.StartedAt, &run.Status)
	if err == nil {
		return run, false, nil
	}
	// 只有「同一时刻至多一轮」这条索引的冲突可以退化成复用已有轮次；
	// 其余错误（连接、语法、别的约束）都必须上抛，不能吞成「复用旧轮次」。
	if !isSingleRunningViolation(err) {
		return CollectRun{}, false, fmt.Errorf("%w: 创建采集轮次失败: %w", model.ErrRunStoreFailed, err)
	}

	existing, found, findErr := runningCollectRun(ctx, q)
	if findErr != nil {
		return CollectRun{}, false, findErr
	}
	if !found {
		return CollectRun{}, false, fmt.Errorf("%w: 唯一索引冲突后未见进行中轮次", model.ErrRunConflict)
	}
	return existing, true, nil
}

// FailStaleRuns 把 started_at 早于 cutoff 的 running 轮次置为 failed，
// 记录 error_code=stale_run 与原因「过期轮次」，返回被收敛的轮次编号。
// 一次 UPDATE 完成，不需要先在 Go 里列出候选轮次，避免查出与更新之间的竞态。
func FailStaleRuns(ctx context.Context, q collectRunQuerier, now, cutoff time.Time) ([]int64, error) {
	rows, err := q.Query(ctx, failStaleRunsSQL, now, cutoff)
	if err != nil {
		return nil, fmt.Errorf("%w: 收敛过期轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	defer rows.Close()

	runIDs := make([]int64, 0)
	for rows.Next() {
		var runID int64
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("%w: 扫描过期轮次失败: %w", model.ErrRunStoreFailed, err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历过期轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	return runIDs, nil
}

// runningCollectRun 取唯一一行进行中轮次；没有时返回 found=false 而不是报错。
func runningCollectRun(ctx context.Context, q collectRunQuerier) (CollectRun, bool, error) {
	var run CollectRun
	err := q.QueryRow(ctx, runningCollectRunSQL).Scan(&run.RunID, &run.StartedAt, &run.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return CollectRun{}, false, nil
	}
	if err != nil {
		return CollectRun{}, false, fmt.Errorf("%w: 查询进行中轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	return run, true, nil
}

// isSingleRunningViolation 判断错误是否为 collect_run_single_running 索引的唯一冲突。
// 必须同时核对 SQLSTATE 与索引名：其他唯一约束冲突不能被当作「已有一轮在跑」。
func isSingleRunningViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == collectRunSingleRunningIndex
}

// CollectRunRepo 把查询器闭在内部，供 pipeline 以窄接口消费：
// pipeline 只看到两个方法，不接触 pgx、SQL 与部分唯一索引的细节。
type CollectRunRepo struct {
	q collectRunQuerier
}

// NewCollectRunRepo 绑定一个查询器，通常是 *pgxpool.Pool；不要在事务里创建并使用它。
func NewCollectRunRepo(q collectRunQuerier) CollectRunRepo {
	return CollectRunRepo{q: q}
}

// StartCollectRun 见同名函数：开始一轮，已有一轮在跑时返回该轮次。
func (r CollectRunRepo) StartCollectRun(ctx context.Context, startedAt time.Time) (CollectRun, bool, error) {
	return StartCollectRun(ctx, r.q, startedAt)
}

// FailStaleRuns 见同名函数：把 started_at 早于 cutoff 的 running 轮次置为失败。
func (r CollectRunRepo) FailStaleRuns(ctx context.Context, now, cutoff time.Time) ([]int64, error) {
	return FailStaleRuns(ctx, r.q, now, cutoff)
}
