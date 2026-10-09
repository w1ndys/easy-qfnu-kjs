// 本文件属于 data 层：同步任务状态机要用的读写——轮次开关、任务清单与任务状态。
// 表结构在 migrations/0004_sync.sql，阶段推进与断点续传在 internal/collect/sync。
// 依据 specs/collector-full-sync/design.md 的 Data Models 与 requirements.md 的 5.1、5.2、5.6。

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrRunInProgress 表示已经有一轮采集处于 running：同一时刻至多一轮，拒绝开始第二轮。
var ErrRunInProgress = errors.New("已有一轮采集在运行")

// 运行状态取值：与 0002_run_log.sql 里 collect_run.status 的约束一致。
const (
	RunStatusRunning = "running" // 本轮正在进行
	RunStatusSuccess = "success" // 本轮全部任务已完成
	RunStatusFailed  = "failed"  // 本轮有任务失败或整轮停止
	RunStatusBlocked = "blocked" // 本轮被阻断（白名单为空），没有发出业务请求
)

// SyncTaskKind 是同步任务的种类，取值与 sync_task.kind 的约束一致。
type SyncTaskKind string

const (
	// KindWeekMatrix 是「楼 + 周矩阵」任务：一次请求一栋楼的一周
	KindWeekMatrix SyncTaskKind = "week_matrix"
	// KindCellDetail 是「楼 + 房间 + 格子明细」任务：一次请求一个格子的占用说明
	KindCellDetail SyncTaskKind = "cell_detail"
)

// SyncTask 是 sync_task 里的一行，也是断点续传的最小单元。
// 两类任务的列形状不同：周矩阵只带楼与周次，明细必须能定位到一个格子。
type SyncTask struct {
	ID           int64        // task_id，按它排序就是任务建立的顺序
	Kind         SyncTaskKind // 任务种类
	BuildingID   string       // 该任务请求的 jxlbh
	Week         int          // 周矩阵任务的周次；明细任务为 0
	RoomID       string       // 明细任务的房间 jsbh；周矩阵任务为空
	Weekday      int          // 明细任务的星期，1=周一 … 7=周日；周矩阵任务为 0
	Block        string       // 明细任务的大节编码；周矩阵任务为空
	AttemptCount int          // 上游请求次数（含重试），面板据此看哪些任务反复失败
}

// StartSyncRun 开启一轮采集：写一行 running 并返回轮次 id。
// 已经有一轮 running 时返回 ErrRunInProgress，不建新轮次；term 必须是已经写入 term 表的学期编号。
func (s *Store) StartSyncRun(ctx context.Context, term string) (int64, error) {
	const sqlText = `
INSERT INTO collect_run (started_at, status, term)
SELECT now(), 'running', NULLIF($1, '')
 WHERE NOT EXISTS (SELECT 1 FROM collect_run WHERE status = 'running')
RETURNING run_id`

	var runID int64
	err := s.pool.QueryRow(ctx, sqlText, term).Scan(&runID)
	// 没有行返回说明上面那句 WHERE 不成立，也就是已经有一轮在跑
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunInProgress
	}
	if err != nil {
		return 0, fmt.Errorf("开启采集轮次失败: %w", err)
	}
	return runID, nil
}

// BlockSyncRun 把一轮标成 blocked 并写下阻断原因：白名单为空时不发业务请求，只留这条记录。
func (s *Store) BlockSyncRun(ctx context.Context, runID int64, reason string) error {
	const sqlText = `
UPDATE collect_run
   SET status = 'blocked', finished_at = now(), error_message = $2
 WHERE run_id = $1`

	if _, err := s.pool.Exec(ctx, sqlText, runID, reason); err != nil {
		return fmt.Errorf("标记轮次 %d 为 blocked 失败: %w", runID, err)
	}
	return nil
}

// FinishSyncRun 结束一轮：写下最终状态与最近错误，空错误落 NULL 而不是空串。
func (s *Store) FinishSyncRun(ctx context.Context, runID int64, status string, lastError string) error {
	const sqlText = `
UPDATE collect_run
   SET status = $2, finished_at = now(), error_message = NULLIF($3, '')
 WHERE run_id = $1`

	if _, err := s.pool.Exec(ctx, sqlText, runID, status, lastError); err != nil {
		return fmt.Errorf("结束轮次 %d 失败: %w", runID, err)
	}
	return nil
}

// EnqueueSyncTasks 把本轮的任务清单写进 sync_task，已存在的任务跳过。
// 断点续传时会重新建一次清单，靠唯一索引与 ON CONFLICT 保证不会造出第二个同样的任务。
func (s *Store) EnqueueSyncTasks(ctx context.Context, runID int64, tasks []SyncTask) error {
	// 空清单是正常情况（楼数或周数为零），不必开事务
	if len(tasks) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启任务写入事务失败: %w", err)
	}
	// 任意一步失败都整体回滚，避免留下半截任务清单
	defer func() { _ = tx.Rollback(ctx) }()

	const sqlText = `
INSERT INTO sync_task (run_id, kind, building_id, week, room_id, weekday, block)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT DO NOTHING`

	for _, task := range tasks {
		// 两类任务的空列必须落 NULL：库里的形状约束据此区分周矩阵与明细
		if _, err := tx.Exec(ctx, sqlText, runID, string(task.Kind), task.BuildingID,
			nullableNumber(task.Week), nullableText(task.RoomID),
			nullableNumber(task.Weekday), nullableText(task.Block)); err != nil {
			return fmt.Errorf("写入轮次 %d 的同步任务失败: %w", runID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交任务写入事务失败: %w", err)
	}
	return nil
}

// NextPendingSyncTask 取本轮第一个待办任务：done 的跳过，failed 的也不取。
// 重试耗尽的任务在本轮已经有结论，再取一次只会立刻再失败；它要等下一轮重新建任务。
// 明细任务是执行期间才写进来的，所以每次都要重新查一遍，不能用一次性的清单。
func (s *Store) NextPendingSyncTask(ctx context.Context, runID int64) (SyncTask, bool, error) {
	const sqlText = `
SELECT task_id, kind, building_id, COALESCE(week, 0), COALESCE(room_id, ''),
       COALESCE(weekday, 0), COALESCE(block, ''), attempt_count
  FROM sync_task
 WHERE run_id = $1 AND status = 'pending'
 ORDER BY task_id
 LIMIT 1`

	var task SyncTask
	var kind string
	err := s.pool.QueryRow(ctx, sqlText, runID).Scan(&task.ID, &kind, &task.BuildingID,
		&task.Week, &task.RoomID, &task.Weekday, &task.Block, &task.AttemptCount)
	// 没有待办任务说明本轮已经跑完，这不是错误
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncTask{}, false, nil
	}
	if err != nil {
		return SyncTask{}, false, fmt.Errorf("读取轮次 %d 的待办任务失败: %w", runID, err)
	}

	task.Kind = SyncTaskKind(kind)
	return task, true, nil
}

// MarkSyncTaskDone 把一个任务标成完成：同一轮再取待办时就会跳过它。
func (s *Store) MarkSyncTaskDone(ctx context.Context, taskID int64, attempts int) error {
	const sqlText = `
UPDATE sync_task
   SET status = 'done', attempt_count = $2, error_message = NULL
 WHERE task_id = $1`

	return s.updateTaskRow(ctx, sqlText, taskID, attempts)
}

// MarkSyncTaskFailed 把一个任务标成失败并记下请求次数与错误：其余任务继续跑。
func (s *Store) MarkSyncTaskFailed(ctx context.Context, taskID int64, attempts int, message string) error {
	const sqlText = `
UPDATE sync_task
   SET status = 'failed', attempt_count = $2, error_message = NULLIF($3, '')
 WHERE task_id = $1`

	return s.updateTaskRow(ctx, sqlText, taskID, attempts, message)
}

// SyncProgress 统计一轮的完成数与总数：完成数只数 done，失败的任务仍算未完成。
func (s *Store) SyncProgress(ctx context.Context, runID int64) (int, int, error) {
	const sqlText = `
SELECT count(*) FILTER (WHERE status = 'done'), count(*)
  FROM sync_task
 WHERE run_id = $1`

	var done, total int
	if err := s.pool.QueryRow(ctx, sqlText, runID).Scan(&done, &total); err != nil {
		return 0, 0, fmt.Errorf("统计轮次 %d 的任务进度失败: %w", runID, err)
	}
	return done, total, nil
}

// updateTaskRow 执行任务状态更新，并确认确实改到了一行：改不到说明任务 id 不对。
func (s *Store) updateTaskRow(ctx context.Context, sqlText string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sqlText, args...)
	if err != nil {
		return fmt.Errorf("更新同步任务失败: %w", err)
	}
	// 一行都没改到说明库里没有这个任务，属于调用方的错误，不能静默放过
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("同步任务不存在: %w", pgx.ErrNoRows)
	}
	return nil
}

// nullableNumber 把 0 写成 NULL：周矩阵的周次与明细的星期都用 0 表示「不适用」，
// 库里的形状约束要求这两类任务各自的空列是 NULL 而不是 0。
func nullableNumber(value int) any {
	// 0 在两类任务里都不是合法取值，所以可以安全地当作「没有这个值」
	if value == 0 {
		return nil
	}
	return value
}
