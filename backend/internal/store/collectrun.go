package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// collectRunQuerier 只取 collect_run 读写需要的三种能力，便于用替身测试。
// Exec 给补学期、补窗口用：这两条是不带 RETURNING 的 UPDATE，靠 RowsAffected 判断 0 行。
type collectRunQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
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

// startCollectRunSQL 插入一条 running 轮次，并在建轮时就写入学期。
// term 不能留到后续阶段补：collect_run_week.term 是 NOT NULL，且周结果依赖它，
// 缺学期必须在建轮阶段就报错，而不是让周结果写入撞上 NOT NULL 中断整轮。
// status='running' 会触发 collect_run_single_running 部分唯一索引。
const startCollectRunSQL = `
INSERT INTO collect_run (started_at, status, term)
VALUES ($1, 'running', $2)
RETURNING run_id, started_at, status`

// startRunningRunSQL 只写 started_at 与 status，term 列不出现在 INSERT 里，留给数据库 NULL。
// runner 持锁调用 Collector.Start，登录不能放进锁里；collect_run.term 又有外键，
// 登录前拿不到学期。与 StartCollectRun 并存：那条路径仍要求建轮时就带学期。
const startRunningRunSQL = `
INSERT INTO collect_run (started_at, status)
VALUES ($1, 'running')
RETURNING run_id, started_at, status`

// setRunTermSQL / setRunWindowSQL 都带 status='running' 守卫：已收口的轮次不能再补学期或窗口，
// 否则迟到的登录结果会改写已经结束的轮次。0 行由调用方补读后报错，不把落空当成写成功。
// weeks_total 用调用方传入的值，不在 SQL 里另算：口径由 RecordWindow 保证等于 end-start+1。
const setRunTermSQL = `
UPDATE collect_run
   SET term = $2, updated_at = $3
 WHERE run_id = $1 AND status = 'running'`

const setRunWindowSQL = `
UPDATE collect_run
   SET window_start = $2, window_end = $3, weeks_total = $4, updated_at = $5
 WHERE run_id = $1 AND status = 'running'`

// runningCollectRunSQL 取当前进行中轮次及其学期；唯一索引保证至多一行，
// 仍加 ORDER BY 与 LIMIT 1 是为了索引被误删时输出仍然确定。
// term 用 COALESCE 折成空串：历史/手工建出的轮次可能没写学期，复用前要能判出这种
// 「没有学期」而不是把 NULL 扫进 string 报错。
const runningCollectRunSQL = `
SELECT run_id, started_at, status, COALESCE(term, '')
  FROM collect_run
 WHERE status = 'running'
 ORDER BY run_id
 LIMIT 1`

// failStaleRunsSQL 把 started_at 严格早于 cutoff 的 running 轮次置为 failed。
// 判定看 started_at 而不是 updated_at：崩溃留下的轮次可能从未更新过 updated_at。
// 终止条件用「<」而不是「<=」：恰好两小时不算「超过」，与 Requirement 11.4 的措辞一致。
//
// phase 同步写成 finished：过期收敛与失败收口是同一类「这一轮结束了」的写入，阶段不能
// 停在崩溃时那一格——否则管理端会把一条 failed 轮次显示成还在周矩阵阶段。
// 终态的 phase 并不总是 finished：发布事务写 success/partial 时停在 publish
// （发布是最后一个业务阶段）。判断「已结束」只看 status != 'running'，不要只认这一列。
const failStaleRunsSQL = `
UPDATE collect_run
   SET status = 'failed',
       phase = 'finished',
       finished_at = $1,
       updated_at = $1,
       error_code = '` + staleRunErrorCode + `',
       error_message = '` + staleRunErrorMessage + `'
 WHERE status = 'running'
   AND started_at < $2
RETURNING run_id`

// StartCollectRun 尝试开始一轮采集，返回本轮轮次、是否复用了已有轮次与错误。
//
// term 在插入时一并写入 collect_run.term；空学期直接报错，不把非法值交给外键
// 或让后续周结果写入撞上 NOT NULL。第二个返回值 reused 为 false 表示本轮新建；
// 为 true 表示撞上唯一索引、复用了已有的进行中轮次（Requirement 8.3、design CP6）。
//
// 复用前必须核对学期：补查到的 running 轮次 term 与传入 term（都去两侧空白）不一致，
// 或该轮次根本没写学期时返回 ErrRunConflict——既不能复用（会把本轮结果写进旧学期），
// 也不能新建（唯一索引仍被它占着），更不改写旧轮次的 term。一致才返回 Reused=true。
//
// 只应传连接池或单语句连接，不要传已开启的事务：唯一索引冲突会让事务进入
// aborted 状态，冲突后的那次 SELECT 将无法执行。
func StartCollectRun(ctx context.Context, q collectRunQuerier, term string, startedAt time.Time) (CollectRun, bool, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return CollectRun{}, false, fmt.Errorf("%w: 缺少学期，无法创建采集轮次", model.ErrRunStoreFailed)
	}

	var run CollectRun
	err := q.QueryRow(ctx, startCollectRunSQL, startedAt, term).Scan(&run.RunID, &run.StartedAt, &run.Status)
	if err == nil {
		return run, false, nil
	}
	// 只有「同一时刻至多一轮」这条索引的冲突可以退化成复用已有轮次；
	// 其余错误（连接、语法、别的约束）都必须上抛，不能吞成「复用旧轮次」。
	if !isSingleRunningViolation(err) {
		return CollectRun{}, false, fmt.Errorf("%w: 创建采集轮次失败: %w", model.ErrRunStoreFailed, err)
	}

	existing, found, findErr := runningCollectRun(ctx, q, term)
	if findErr != nil {
		return CollectRun{}, false, findErr
	}
	if !found {
		return CollectRun{}, false, fmt.Errorf("%w: 唯一索引冲突后未见进行中轮次", model.ErrRunConflict)
	}
	return existing, true, nil
}

// StartRunningRun 插入一条 status=running、term 为 NULL 的轮次。
//
// 与 StartCollectRun 并存，不改后者「空学期报错 / 冲突后按学期复用」的语义。
// 本路径在唯一索引冲突时直接返回 ErrRunConflict，不补查、不复用旧轮次、也不建第二轮：
// 执行器要的是「此刻没有别的 running」，不是「把结果写进已有轮次」。
// 只认 collect_run_single_running 的 23505；别的唯一冲突、连接或语法错误包装成
// ErrRunStoreFailed，避免把无关失败误报成轮次冲突。
func StartRunningRun(ctx context.Context, q collectRunQuerier, startedAt time.Time) (CollectRun, error) {
	var run CollectRun
	err := q.QueryRow(ctx, startRunningRunSQL, startedAt).Scan(&run.RunID, &run.StartedAt, &run.Status)
	if err == nil {
		return run, nil
	}
	if isSingleRunningViolation(err) {
		return CollectRun{}, fmt.Errorf("%w: 已有进行中轮次，不能再建一轮: %w", model.ErrRunConflict, err)
	}
	return CollectRun{}, fmt.Errorf("%w: 创建采集轮次失败: %w", model.ErrRunStoreFailed, err)
}

// SetRunTerm 给已建的 running 轮次补学期。WHERE 带 status='running'：收口之后不再改 term。
// 学期是否为空由 pipeline.RecordTerm 拒绝，这里不重复校验，以免两处口径分叉。
func SetRunTerm(ctx context.Context, q collectRunQuerier, runID int64, term string, now time.Time) error {
	tag, err := q.Exec(ctx, setRunTermSQL, runID, term, now)
	if err != nil {
		return fmt.Errorf("%w: 补写学期失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	return explainMissedRunningUpdate(ctx, q, runID, "补写学期")
}

// SetRunWindow 写 window_start / window_end / weeks_total。weeks_total 用调用方传入的值，
// 不在这里按 end-start+1 重算：跨度上限与溢出防护在 pipeline.RecordWindow，仓储只落库。
// 守卫与 0 行语义同 SetRunTerm。
func SetRunWindow(ctx context.Context, q collectRunQuerier, runID int64, start, end, weeksTotal int, now time.Time) error {
	tag, err := q.Exec(ctx, setRunWindowSQL, runID, start, end, weeksTotal, now)
	if err != nil {
		return fmt.Errorf("%w: 写入采集窗口失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	return explainMissedRunningUpdate(ctx, q, runID, "写入采集窗口")
}

// explainMissedRunningUpdate 在带 status='running' 守卫的 UPDATE 影响 0 行时补读状态。
// 0 行本身分不清「轮次不存在」和「已收口」：调用方需要从文案判断，不能把两种失败都说成更新失败。
// 补读只查 collect_run（复用 loadRunState），不碰别的表；读库失败沿用既有包装。
func explainMissedRunningUpdate(ctx context.Context, q collectRunQuerier, runID int64, action string) error {
	state, found, err := loadRunState(ctx, q, runID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法%s", model.ErrRunStoreFailed, runID, action)
	}
	if state.Status != "running" {
		return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能再%s", model.ErrRunStoreFailed, runID, state.Status, action)
	}
	return fmt.Errorf("%w: 轮次 %d 仍为 running 但更新 0 行，无法%s", model.ErrRunStoreFailed, runID, action)
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

// runningCollectRun 取唯一一行进行中轮次，并核对它记录的学期与本轮 term 是否一致。
// 没有进行中轮次时返回 found=false 而不是报错（那是冲突与补查之间的竞态）。
// 学期不一致（含旧轮次 term 为 NULL，读回空串）返回 ErrRunConflict：此时既不能复用
// 这条轮次，也不能新建，只能让调用方下一轮再来。两边都去空白后比较，避免同一学期
// 因为多一个空格被判成冲突。
func runningCollectRun(ctx context.Context, q collectRunQuerier, term string) (CollectRun, bool, error) {
	var run CollectRun
	var runningTerm string
	err := q.QueryRow(ctx, runningCollectRunSQL).Scan(&run.RunID, &run.StartedAt, &run.Status, &runningTerm)
	if errors.Is(err, pgx.ErrNoRows) {
		return CollectRun{}, false, nil
	}
	if err != nil {
		return CollectRun{}, false, fmt.Errorf("%w: 查询进行中轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	if strings.TrimSpace(runningTerm) != term {
		shown := strings.TrimSpace(runningTerm)
		if shown == "" {
			shown = "未写学期"
		}
		return CollectRun{}, false, fmt.Errorf("%w: 进行中轮次 %d 的学期为 %s，与本轮学期 %s 不一致，不能复用",
			model.ErrRunConflict, run.RunID, shown, term)
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
// pipeline 只看到方法，不接触 pgx、SQL 与部分唯一索引的细节。
type CollectRunRepo struct {
	q collectRunQuerier
}

// NewCollectRunRepo 绑定一个查询器，通常是 *pgxpool.Pool；不要在事务里创建并使用它。
func NewCollectRunRepo(q collectRunQuerier) CollectRunRepo {
	return CollectRunRepo{q: q}
}

// StartCollectRun 见同名函数：开始一轮，已有一轮在跑时返回该轮次。
func (r CollectRunRepo) StartCollectRun(ctx context.Context, term string, startedAt time.Time) (CollectRun, bool, error) {
	return StartCollectRun(ctx, r.q, term, startedAt)
}

// FailStaleRuns 见同名函数：把 started_at 早于 cutoff 的 running 轮次置为失败。
func (r CollectRunRepo) FailStaleRuns(ctx context.Context, now, cutoff time.Time) ([]int64, error) {
	return FailStaleRuns(ctx, r.q, now, cutoff)
}

// StartRunningRun 见同名函数：插入 running、term 留 NULL 的轮次，冲突不复用。
func (r CollectRunRepo) StartRunningRun(ctx context.Context, startedAt time.Time) (CollectRun, error) {
	return StartRunningRun(ctx, r.q, startedAt)
}

// SetRunTerm 见同名函数。
func (r CollectRunRepo) SetRunTerm(ctx context.Context, runID int64, term string, now time.Time) error {
	return SetRunTerm(ctx, r.q, runID, term, now)
}

// SetRunWindow 见同名函数。
func (r CollectRunRepo) SetRunWindow(ctx context.Context, runID int64, start, end, weeksTotal int, now time.Time) error {
	return SetRunWindow(ctx, r.q, runID, start, end, weeksTotal, now)
}
