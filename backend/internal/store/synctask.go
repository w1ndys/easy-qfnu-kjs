package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// sync_task.status 的三种取值，必须与 0001_baseline.sql 的 CHECK 约束一致。
// pending 由执行循环消费，done 与 failed 在续跑时只跳过、不再改变。
const (
	SyncTaskPending = "pending" // 待执行：续跑从第一个 pending 继续
	SyncTaskDone    = "done"    // 已完成：续跑直接跳过
	SyncTaskFailed  = "failed"  // 已失败：同一轮内不重试，保持 failed
)

// sync_task.kind 的两种取值，必须与 0001_baseline.sql 的 CHECK 约束一致。
// 两种任务分段落库：周矩阵不依赖 room，先落；占用说明的 room_id 有外键，
// 必须等本轮归属写进 room 之后再落（见 EnsureWeekMatrixTasks / EnsureCellDetailTasks）。
const (
	SyncTaskKindWeekMatrix = "week_matrix"
	SyncTaskKindCellDetail = "cell_detail"
)

// SyncTask 是执行循环关心的 sync_task 列：身份、请求参数与状态。
// week 与 room_id/weekday/block 是互斥的两组：周矩阵只有 week，
// 占用说明只有 room_id、weekday、block；读取时把 NULL 折成零值。
type SyncTask struct {
	TaskID     int64  // 任务编号，也是执行顺序：规划时按白名单顺序 × 周次升序写入
	Kind       string // week_matrix 或 cell_detail
	Required   bool   // 周矩阵必需、占用说明可选
	BuildingID string // 楼编号
	Week       int    // 教学周，仅周矩阵任务使用
	RoomID     string // 房号，仅占用说明任务使用
	Weekday    int    // 星期，仅占用说明任务使用
	Block      string // 大节编码，仅占用说明任务使用
	Status     string // pending、done 或 failed
}

// SyncTaskInput 是一条待落库的规划任务；TaskID 与 Status 由库生成或保留，
// 续跑时不能把已有的 done/failed 重置回 pending。
type SyncTaskInput struct {
	Kind       string // week_matrix 或 cell_detail
	Required   bool   // 周矩阵必需、占用说明可选
	BuildingID string // 楼编号
	Week       int    // 教学周，仅周矩阵任务使用
	RoomID     string // 房号，仅占用说明任务使用
	Weekday    int    // 星期，仅占用说明任务使用
	Block      string // 大节编码，仅占用说明任务使用
}

// RunWeekFetch 是一次成功周矩阵抓取要写进 collect_run_week 的事实。
// term 不在这里：StartCollectRun 建轮时已经把它写进 collect_run，SQL 直接从
// collect_run.term 取该列，执行循环不必为写周结果再单独传或读一次学期。
type RunWeekFetch struct {
	RunID      int64     // 轮次编号
	Week       int       // 教学周
	BuildingID string    // 楼编号
	FetchedAt  time.Time // 抓取完成时刻
	PageSHA256 string    // 页面正文的 sha256 十六进制摘要；原始 HTML 不落库
}

// RunWeekFailure 是一次失败周矩阵抓取要写进 collect_run_week 的事实。
// 与成功行共用唯一键（run_id、term、week、building_id），后写的一方覆盖前一方；
// 失败没有抓取时刻与页面哈希，只记录 ok=false 与原因。
type RunWeekFailure struct {
	RunID        int64  // 轮次编号
	Week         int    // 教学周
	BuildingID   string // 楼编号
	ErrorMessage string // 失败原因；写入 collect_run_week.error_message
}

// syncTaskQuerier 只取任务落库与续跑需要的两种能力，便于用替身测试。
type syncTaskQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// insertSyncTasksByKindSQL 写入本轮某一类规划任务（$9 是 kind）。
//
// 三个细节：
//   - WHERE NOT EXISTS 让「本轮该类任务已有」时整批不写：续跑以库里的任务与状态为准，
//     不把新规划混进已经跑了一半的轮次；按 kind 判而不是按整个 run 判，
//     周矩阵与占用说明才能分两段各自只插一次；
//   - ON CONFLICT DO NOTHING 兜住同一轮重复写入，唯一索引认的是
//     sync_task_week / sync_task_cell 两条部分索引；
//   - 占用说明的 room_id 有外键指向 room(room_id)：调用方必须先把本轮归属写进 room，
//     再执行 CellDetail 段。缺房号时外键失败上抛，不静默丢任务。
//
// week、room_id、weekday、block 用 NULLIF 把规划里的零值折成 NULL，
// 以满足表上「周矩阵与占用说明字段互斥」的 CHECK 约束。
// 整批不写时执行成功但影响 0 行，这是续跑的正常路径，不能按错误处理。
const insertSyncTasksByKindSQL = `
INSERT INTO sync_task (run_id, kind, required, building_id, week, room_id, weekday, block)
SELECT $1, t.kind, t.required, t.building_id,
       NULLIF(t.week, 0), NULLIF(t.room_id, ''), NULLIF(t.weekday, 0), NULLIF(t.block, '')
  FROM unnest($2::text[], $3::boolean[], $4::text[], $5::smallint[], $6::text[], $7::smallint[], $8::text[])
       AS t(kind, required, building_id, week, room_id, weekday, block)
 WHERE NOT EXISTS (SELECT 1 FROM sync_task WHERE run_id = $1 AND kind = $9)
ON CONFLICT DO NOTHING`

// recountSyncTasksSQL 按 sync_task 的现状重算轮次计数。
// 目前由两段 Ensure 在落任务后各调用一次；它可重复执行，收尾阶段若要纠正计数，
// 调用导出的 RecountSyncTasks 即可（执行循环不会自动重算）。
// 轮次不存在时影响 0 行，RecountSyncTasks 据此报 ErrRunStoreFailed。
const recountSyncTasksSQL = `
UPDATE collect_run
   SET required_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required),
       optional_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required),
       required_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required AND status = 'done'),
       optional_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required AND status = 'done'),
       updated_at = $2
 WHERE run_id = $1`

// selectSyncTasksByKindSQL 按 task_id 升序取本轮某一类任务。task_id 由规划写入顺序
// 决定，所以「从第一个 pending 继续」有稳定顺序，不依赖任何额外的排序列。
// 按 kind 过滤是为了让两段 Ensure 各自只返回自己的任务：续跑时周矩阵段不能
// 把已存在的占用说明任务混进周矩阵执行循环。
const selectSyncTasksByKindSQL = `
SELECT task_id, kind, required, building_id, week, room_id, weekday, block, status
  FROM sync_task
 WHERE run_id = $1
   AND kind = $2
 ORDER BY task_id`

// completeSyncTaskSQL 把一条 pending 任务置为 done，并只在真的发生状态迁移时
// 给轮次计数 +1。用数据修改型 CTE 的 RETURNING 计数，而不是在外层子查询里重数
// sync_task：同一条语句里的其它部分看不到 CTE 对表的修改。
//
// 外层 UPDATE 的条件带 EXISTS：轮次或任务不存在时影响 0 行，调用方据此返回明确
// 错误；任务已 done/failed 时任务仍存在，影响 1 行但不改状态与计数，保持幂等。
const completeSyncTaskSQL = `
WITH done AS (
  UPDATE sync_task
     SET status = 'done',
         attempt_count = attempt_count + 1,
         error_message = NULL
   WHERE run_id = $1
     AND task_id = $2
     AND status = 'pending'
   RETURNING required
)
UPDATE collect_run
   SET required_done = required_done + (SELECT count(*) FROM done WHERE required),
       optional_done = optional_done + (SELECT count(*) FROM done WHERE NOT required),
       updated_at = $3
 WHERE run_id = $1
   AND EXISTS (SELECT 1 FROM sync_task WHERE run_id = $1 AND task_id = $2)`

// failSyncTaskSQL 把一条 pending 任务置为 failed 并记录原因。
// 已 done/failed 的任务不会被改写：同一条 SQL 在续跑重放时也是幂等的。
// 外层 UPDATE 同样用 EXISTS 区分「轮次/任务不存在」（0 行）与「已收官重放」（1 行）。
const failSyncTaskSQL = `
WITH failed AS (
  UPDATE sync_task
     SET status = 'failed',
         attempt_count = attempt_count + 1,
         error_message = $3
   WHERE run_id = $1
     AND task_id = $2
     AND status = 'pending'
)
UPDATE collect_run
   SET updated_at = $4
 WHERE run_id = $1
   AND EXISTS (SELECT 1 FROM sync_task WHERE run_id = $1 AND task_id = $2)`

// recordWeekFetchSQL 写一条成功的周矩阵抓取结果。
// term 在 StartCollectRun 建轮时写入 collect_run；这里直接从轮次取，执行循环
// 不再单独传学期。r.term IS NOT NULL 把「轮次缺学期」挡成 0 行，由调用方报
// 可读错误，避免整轮撞上 collect_run_week.term 的 NOT NULL 而中断。
// 冲突时覆盖为最新一次成功结果，并清掉历史失败原因，重放不会留下两行。
const recordWeekFetchSQL = `
INSERT INTO collect_run_week (run_id, term, week, building_id, ok, fetched_at, page_sha256)
SELECT $1, r.term, $2, $3, true, $4, $5
  FROM collect_run r
 WHERE r.run_id = $1
   AND r.term IS NOT NULL
ON CONFLICT (run_id, term, week, building_id)
DO UPDATE
   SET ok = true,
       fetched_at = EXCLUDED.fetched_at,
       page_sha256 = EXCLUDED.page_sha256,
       error_message = NULL`

// recordWeekFailureSQL 写一条失败的周矩阵结果，与成功行共用唯一键。
// 失败没有抓取时刻与页面哈希，覆盖时要把上一次成功留下的两列清空，否则会留下
// 「ok=false 却带哈希」的矛盾行。term 的取值与缺失处理同 recordWeekFetchSQL。
const recordWeekFailureSQL = `
INSERT INTO collect_run_week (run_id, term, week, building_id, ok, error_message)
SELECT $1, r.term, $2, $3, false, $4
  FROM collect_run r
 WHERE r.run_id = $1
   AND r.term IS NOT NULL
ON CONFLICT (run_id, term, week, building_id)
DO UPDATE
   SET ok = false,
       fetched_at = NULL,
       page_sha256 = NULL,
       error_message = EXCLUDED.error_message`

// EnsureWeekMatrixTasks 写入本轮规划的周矩阵任务并返回库里该段的权威清单
// （按 task_id 升序）。周矩阵的 sync_task 行没有 room_id，新库没有 room 也能落库，
// 所以它必须先于占用说明段执行，给归属落库创造前置条件。
//
// 只在该轮还没有周矩阵任务时插入；已有任务时直接读回，保留 done/failed，
// 这是断点续跑「从第一个 pending 继续、跳过 done、不重试 failed」的数据基础。
// 插入后重算轮次计数；轮次不存在时重算影响 0 行并返回 ErrRunStoreFailed。
func EnsureWeekMatrixTasks(ctx context.Context, q syncTaskQuerier, runID int64, tasks []SyncTaskInput, now time.Time) ([]SyncTask, error) {
	return ensureSyncTasksOfKind(ctx, q, runID, SyncTaskKindWeekMatrix, tasks, now)
}

// EnsureCellDetailTasks 写入本轮规划的占用说明任务并返回库里该段的权威清单。
//
// sync_task.room_id 有外键指向 room(room_id)：调用方必须先用周矩阵响应完成
// 本轮归属落库，再执行本函数。周矩阵段与占用说明段拆开写，正是为了让「新库里
// room 还不存在」不再把两段任务一起打挂：周矩阵先落、先跑、先写 room，
// 占用说明再落。缺房号时外键失败仍会包装成 ErrRunStoreFailed 上抛，不静默丢任务。
func EnsureCellDetailTasks(ctx context.Context, q syncTaskQuerier, runID int64, tasks []SyncTaskInput, now time.Time) ([]SyncTask, error) {
	return ensureSyncTasksOfKind(ctx, q, runID, SyncTaskKindCellDetail, tasks, now)
}

// ensureSyncTasksOfKind 是两段 Ensure 的共同实现：只处理 kind 这一种任务，
// 写库、重算计数、按 kind 读回。
func ensureSyncTasksOfKind(
	ctx context.Context,
	q syncTaskQuerier,
	runID int64,
	kind string,
	tasks []SyncTaskInput,
	now time.Time,
) ([]SyncTask, error) {
	for _, task := range tasks {
		if task.Kind != kind {
			return nil, fmt.Errorf("%w: 任务种类 %q 不能写进 %s 段", model.ErrRunStoreFailed, task.Kind, kind)
		}
	}
	if len(tasks) > 0 {
		if err := insertSyncTasks(ctx, q, runID, kind, tasks); err != nil {
			return nil, err
		}
	}
	if err := RecountSyncTasks(ctx, q, runID, now); err != nil {
		return nil, err
	}
	return loadSyncTasksByKind(ctx, q, runID, kind)
}

// RecountSyncTasks 按 sync_task 的现状重算轮次的 required_total/optional_total/
// required_done/optional_done，并刷新 updated_at。
//
// 它只改计数、不碰任务状态，可重复执行；两段 Ensure 落任务后各调用一次。
// 收尾阶段若需要纠正计数，调用方应显式调用本函数，不要假定执行循环会自动重算。
// 轮次不存在时 UPDATE 影响 0 行：这是调用方传错 run_id 的信号，按
// ErrRunStoreFailed 上抛，不静默当成「重算成功」。
func RecountSyncTasks(ctx context.Context, q syncTaskQuerier, runID int64, now time.Time) error {
	tag, err := q.Exec(ctx, recountSyncTasksSQL, runID, now)
	if err != nil {
		return fmt.Errorf("%w: 重算轮次计数失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法重算任务计数", model.ErrRunStoreFailed, runID)
	}
	return nil
}

// insertSyncTasks 把某一类规划任务写进 sync_task；该轮已有同类任务时整批不写。
func insertSyncTasks(ctx context.Context, q syncTaskQuerier, runID int64, kind string, tasks []SyncTaskInput) error {
	kinds := make([]string, len(tasks))
	required := make([]bool, len(tasks))
	buildingIDs := make([]string, len(tasks))
	weeks := make([]int16, len(tasks))
	roomIDs := make([]string, len(tasks))
	weekdays := make([]int16, len(tasks))
	blocks := make([]string, len(tasks))
	for index, task := range tasks {
		kinds[index] = task.Kind
		required[index] = task.Required
		buildingIDs[index] = task.BuildingID
		weeks[index] = int16(task.Week)
		roomIDs[index] = task.RoomID
		weekdays[index] = int16(task.Weekday)
		blocks[index] = task.Block
	}

	if _, err := q.Exec(ctx, insertSyncTasksByKindSQL,
		runID, kinds, required, buildingIDs, weeks, roomIDs, weekdays, blocks, kind); err != nil {
		return fmt.Errorf("%w: 写入同步任务失败: %w", model.ErrRunStoreFailed, err)
	}
	return nil
}

// loadSyncTasksByKind 读回本轮某一类任务；NULL 的周次与占用说明字段折成零值。
func loadSyncTasksByKind(ctx context.Context, q syncTaskQuerier, runID int64, kind string) ([]SyncTask, error) {
	rows, err := q.Query(ctx, selectSyncTasksByKindSQL, runID, kind)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取同步任务失败: %w", model.ErrRunStoreFailed, err)
	}
	defer rows.Close()

	tasks := make([]SyncTask, 0)
	for rows.Next() {
		var task SyncTask
		var week, weekday *int16
		var roomID, block *string
		if err := rows.Scan(&task.TaskID, &task.Kind, &task.Required, &task.BuildingID,
			&week, &roomID, &weekday, &block, &task.Status); err != nil {
			return nil, fmt.Errorf("%w: 扫描同步任务失败: %w", model.ErrRunStoreFailed, err)
		}
		if week != nil {
			task.Week = int(*week)
		}
		if roomID != nil {
			task.RoomID = *roomID
		}
		if weekday != nil {
			task.Weekday = int(*weekday)
		}
		if block != nil {
			task.Block = *block
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历同步任务失败: %w", model.ErrRunStoreFailed, err)
	}
	return tasks, nil
}

// RecordWeekFetch 写一条成功抓取的周矩阵结果（含 fetched_at 与页面 sha256），
// 同一轮同一楼同一周只保留一行。原始 HTML 不经过本函数，也不落库。
// 轮次不存在或缺学期时 SQL 影响 0 行，这里返回可读错误，不静默丢结果。
func RecordWeekFetch(ctx context.Context, q syncTaskQuerier, fetch RunWeekFetch) error {
	tag, err := q.Exec(ctx, recordWeekFetchSQL,
		fetch.RunID, fetch.Week, fetch.BuildingID, fetch.FetchedAt, fetch.PageSHA256)
	if err != nil {
		return fmt.Errorf("%w: 写入周抓取结果失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 不存在或未写学期，无法写入周 %d 楼 %s 的周结果",
			model.ErrRunStoreFailed, fetch.RunID, fetch.Week, fetch.BuildingID)
	}
	return nil
}

// RecordWeekFailure 写一条失败的周矩阵结果（ok=false + 原因），
// 与成功行共用唯一键，后写的一方覆盖前一方。失败没有抓取时刻与页面哈希，
// 覆盖成功行时会把这两列清空。
// 轮次不存在或缺学期时同样返回可读错误，不静默丢结果。
func RecordWeekFailure(ctx context.Context, q syncTaskQuerier, failure RunWeekFailure) error {
	tag, err := q.Exec(ctx, recordWeekFailureSQL,
		failure.RunID, failure.Week, failure.BuildingID, failure.ErrorMessage)
	if err != nil {
		return fmt.Errorf("%w: 写入周失败结果失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 不存在或未写学期，无法写入周 %d 楼 %s 的失败结果",
			model.ErrRunStoreFailed, failure.RunID, failure.Week, failure.BuildingID)
	}
	return nil
}

// CompleteSyncTask 把一条 pending 任务置为 done 并回写轮次完成计数。
// 任务已经是 done/failed 时不做任何状态迁移，计数也不会重复增加。
// 轮次或任务不存在时返回可读错误，避免把一次落空的写入当成成功。
func CompleteSyncTask(ctx context.Context, q syncTaskQuerier, runID, taskID int64, completedAt time.Time) error {
	tag, err := q.Exec(ctx, completeSyncTaskSQL, runID, taskID, completedAt)
	if err != nil {
		return fmt.Errorf("%w: 标记任务完成失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 或任务 %d 不存在，无法标记完成",
			model.ErrRunStoreFailed, runID, taskID)
	}
	return nil
}

// FailSyncTask 把一条 pending 任务置为 failed 并记录原因；已失败的任务保持原状，
// 因此同一轮内失败任务不会被重试改写。
// 轮次或任务不存在时返回可读错误，避免把一次落空的写入当成成功。
func FailSyncTask(ctx context.Context, q syncTaskQuerier, runID, taskID int64, message string, failedAt time.Time) error {
	tag, err := q.Exec(ctx, failSyncTaskSQL, runID, taskID, message, failedAt)
	if err != nil {
		return fmt.Errorf("%w: 标记任务失败失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 或任务 %d 不存在，无法标记失败",
			model.ErrRunStoreFailed, runID, taskID)
	}
	return nil
}

// SyncTaskRepo 把查询器闭在内部，供 pipeline 以窄接口消费；pipeline 看不到 SQL。
type SyncTaskRepo struct {
	q syncTaskQuerier
}

// NewSyncTaskRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewSyncTaskRepo(q syncTaskQuerier) SyncTaskRepo {
	return SyncTaskRepo{q: q}
}

// EnsureWeekMatrixTasks 见同名函数。
func (r SyncTaskRepo) EnsureWeekMatrixTasks(ctx context.Context, runID int64, tasks []SyncTaskInput, now time.Time) ([]SyncTask, error) {
	return EnsureWeekMatrixTasks(ctx, r.q, runID, tasks, now)
}

// EnsureCellDetailTasks 见同名函数。
func (r SyncTaskRepo) EnsureCellDetailTasks(ctx context.Context, runID int64, tasks []SyncTaskInput, now time.Time) ([]SyncTask, error) {
	return EnsureCellDetailTasks(ctx, r.q, runID, tasks, now)
}

// RecountSyncTasks 见同名函数。
func (r SyncTaskRepo) RecountSyncTasks(ctx context.Context, runID int64, now time.Time) error {
	return RecountSyncTasks(ctx, r.q, runID, now)
}

// RecordWeekFetch 见同名函数。
func (r SyncTaskRepo) RecordWeekFetch(ctx context.Context, fetch RunWeekFetch) error {
	return RecordWeekFetch(ctx, r.q, fetch)
}

// RecordWeekFailure 见同名函数。
func (r SyncTaskRepo) RecordWeekFailure(ctx context.Context, failure RunWeekFailure) error {
	return RecordWeekFailure(ctx, r.q, failure)
}

// CompleteSyncTask 见同名函数。
func (r SyncTaskRepo) CompleteSyncTask(ctx context.Context, runID, taskID int64, completedAt time.Time) error {
	return CompleteSyncTask(ctx, r.q, runID, taskID, completedAt)
}

// FailSyncTask 见同名函数。
func (r SyncTaskRepo) FailSyncTask(ctx context.Context, runID, taskID int64, message string, failedAt time.Time) error {
	return FailSyncTask(ctx, r.q, runID, taskID, message, failedAt)
}
