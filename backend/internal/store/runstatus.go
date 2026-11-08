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

// RunSummary 是一轮采集对管理端可见的完整摘要，列与 collect_run 一一对应。
//
// 所有「本来可以为空」的列都用指针承载：blocked 轮次的 term 恒为 NULL（白名单为空时
// 通常还没有学期），窗口、版本、错误码与错误原因也只在部分轮次上有值。用值类型扫描
// 遇到 NULL 会直接报错，因此这里不做 COALESCE 折空串——空串与「没有」在面板上是
// 两种不同的展示（design 的「轮次只读视图」一节）。
//
// FinishedAt 同样是指针：running 轮次还没结束，不能拿零值时间冒充一个完成时刻。
type RunSummary struct {
	RunID             int64      // 轮次编号，collect_run.run_id
	Status            string     // 轮次状态：running/success/partial/failed/blocked/skipped
	Phase             string     // 最近推进到的阶段；终态的 phase 不总是 finished
	StartedAt         time.Time  // 开始时刻，collect_run.started_at
	FinishedAt        *time.Time // 结束时刻；running 轮次为 nil
	UpdatedAt         time.Time  // 最后一次写入时刻
	Term              *string    // 学期；blocked 轮次为 nil
	WindowStart       *int       // 教学周窗口起点；未进入窗口阶段时 nil
	WindowEnd         *int       // 教学周窗口终点；未进入窗口阶段时 nil
	WeeksTotal        int        // 窗口内周数
	RoomsTotal        int        // 本轮涉及的教室数
	RequiredTotal     int        // 必需任务总数
	RequiredDone      int        // 必需任务已完成数
	OptionalTotal     int        // 可选任务总数
	OptionalDone      int        // 可选任务已完成数
	UnknownCells      int        // 未知状态的格子数
	CompositeCells    int        // 复合状态的格子数
	AttrConflictCount int        // 归属冲突计数
	ReleaseID         *string    // 本轮发布的版本号；未发布时 nil
	ReleasedAt        *time.Time // 本轮发布时刻；未发布时 nil
	ErrorCode         *string    // 失败错误码；没有记录时 nil
	ErrorMessage      *string    // 失败原因；没有记录时 nil
}

// RunWeekResult 是 collect_run_week 里的一条「楼 × 周」抓取结果。
//
// 只表达已写入的行：没有写过的楼周不会出现在结果里（不是 ok=false）。
// FetchedAt 与 ErrorMessage 可空：抓取还没发生时没有时刻，成功的行没有错误原因。
type RunWeekResult struct {
	Week         int        // 教学周，1 起
	BuildingID   string     // 教学楼编号，对应 building_whitelist.jxlbh
	OK           bool       // 这一楼这一周是否抓取成功
	FetchedAt    *time.Time // 抓取时刻；未抓取时为 nil
	ErrorMessage *string    // 失败原因；成功时为 nil
}

// runStatusQuerier 是轮次仓储需要的最小能力：多行读、单行读、单条执行。
//
// Query 用于列表与周结果，QueryRow 用于单轮、进行中轮次、计数与 blocked 直插。
// Exec 目前没有调用点（blocked 直插走 QueryRow 的 RETURNING），保留它是因为
// TaskSummary 把同一个查询器直接交给包内的 LoadRunTaskSummary，而那个函数的形参是
// runPhaseQuerier（Exec + QueryRow）；在这里补一个用不到的方法，比在调用点造一个
// 假适配器、或把「失败数怎么算」复制成第二份实现都要清楚。
type runStatusQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// runSummaryColumns 是 RunSummary 的 22 列清单，Recent、ByID、Running 三条查询共用。
//
// 列序即扫描顺序（scanRunSummary 按同一顺序取地址），三处各写一份的话，任何一处
// 漏列或换序都只会在真实库上炸；共用一个常量让「改列」只有一处入口。
const runSummaryColumns = `run_id, status, phase, started_at, finished_at, updated_at, term,
       window_start, window_end, weeks_total, rooms_total,
       required_total, required_done, optional_total, optional_done,
       unknown_cells, composite_cells, attr_conflict_count,
       release_id, released_at, error_code, error_message`

// recentRunSummariesSQL 按开始时刻倒序列出轮次。
//
// 同一时刻可能有多轮（同一秒内重试、批量补数据），只按 started_at 排序时数据库
// 不保证同刻行的相对顺序，翻页会出现「同一条出现两次、另一条永远不出现」；
// 补 run_id DESC 让排序在全序上唯一，LIMIT/OFFSET 才是稳定的。
// 不按 run_id 单独排序：run_id 只反映插入顺序，而 started_at 才是管理员眼里的时间线。
const recentRunSummariesSQL = `
SELECT ` + runSummaryColumns + `
  FROM collect_run
 ORDER BY started_at DESC, run_id DESC
 LIMIT $1 OFFSET $2`

// countCollectRunsSQL 与列表同一次请求内取总数，供分页信息使用。
//
// collect_run 的行数很小（每轮一行），不需要近似计数或缓存；count(*) 与列表分开
// 查询而不是塞进同一行，是为了让列表的列形状保持与 ByID、Running 完全一致。
const countCollectRunsSQL = `SELECT count(*) FROM collect_run`

// runSummaryByIDSQL 按编号取一轮摘要。不存在时返回 pgx.ErrNoRows，由 ByID 转成
// found=false 而不是错误：design 明确 ErrRunNotFound 不在读取路径使用。
const runSummaryByIDSQL = `
SELECT ` + runSummaryColumns + `
  FROM collect_run
 WHERE run_id = $1`

// runningRunSummarySQL 取当前进行中轮次。
//
// collect_run_single_running 部分唯一索引保证至多一行；仍写 ORDER BY run_id 与
// LIMIT 1，是为了索引被误删时输出仍然确定（与 runningCollectRunSQL 同口径）。
// 这里选中全部 22 列而不是只取编号与阶段：冲突要回给管理员「哪一轮、跑到哪」，
// 多取几列的代价远小于再查一次库。
const runningRunSummarySQL = `
SELECT ` + runSummaryColumns + `
  FROM collect_run
 WHERE status = 'running'
 ORDER BY run_id
 LIMIT 1`

// runWeekResultsSQL 读一轮已写入的逐楼逐周结果。
//
// 排序固定 (week, building_id)：面板按周分组展示，同周内按楼编号稳定排列；
// 不按抓取顺序排，那样每次刷新的行序会跟着执行顺序漂移。
const runWeekResultsSQL = `
SELECT week, building_id, ok, fetched_at, error_message
  FROM collect_run_week
 WHERE run_id = $1
 ORDER BY week, building_id`

// whitelistEmptyErrorCode 是「白名单为空、未发起采集」这一终结原因的固定错误码
// （Requirement 8.4 / design D5）。写成常量后再拼进 SQL，与 staleRunErrorCode 同一
// 种做法：这个码是对外契约，面板与测试都按它判读，散在各处各写一份字面量的话，
// 改名不会有编译错误，只会在真实库或前端悄悄错开。
const whitelistEmptyErrorCode = "whitelist_empty"

// startBlockedRunSQL 直接以终态插入一条 blocked 轮次。
//
// 不走 StartCollectRun 的原因见 design D5：建轮需要学期，而白名单为空时通常还没有
// 学期（term 表可能为空），那条路径会因空学期报错。这里 term 留 NULL，窗口与计数
// 保持表默认值，status/phase 都落在 CHECK 允许的取值内。
//
// started_at / updated_at / finished_at 用同一个 $1：建立即终态，三个时刻在语义上
// 就是同一瞬；分成三个参数会让调用方有机会写出「结束了但还没开始」的行。
// status='blocked' 不触发部分唯一索引，所以这条轮次不占 running 槽（CP12）。
//
// error_code 固定为 whitelistEmptyErrorCode，error_message 原样接收调用方给的原因。
// 这里刻意不写 finishCollectRunSQL 那种 NULLIF 折空串：blocked 轮次一定带一条给人
// 看的原因，传空串就落一个空串（空串不是 NULL），面板上只会显示「有错误码、原因空白」。
// 非空由调用方（task 7 的 runner）保证——仓储不替它编文案、不 trim、也不校验，否则
// 「调用方忘了传」和「这次真的没有原因」就分不出来了。
const startBlockedRunSQL = `
INSERT INTO collect_run (started_at, updated_at, finished_at, status, phase, error_code, error_message)
VALUES ($1, $1, $1, 'blocked', 'finished', '` + whitelistEmptyErrorCode + `', $2)
RETURNING run_id, started_at`

// scanRunSummary 按 runSummaryColumns 的顺序把一行扫进 RunSummary。
//
// 形参是「扫描函数」而不是 pgx.Row：Rows.Scan 与 Row.Scan 签名一致，列表与单行
// 因此共用同一份列序，不会出现「ByID 加了列、Recent 忘了加」的偏差。
// 可空列的目标是字段地址（**T），由 pgx 的指针指针扫描计划负责 NULL → nil、
// 非 NULL → 分配；这里不手写 sql.NullString 之类的中间类型。
func scanRunSummary(scan func(dest ...any) error) (RunSummary, error) {
	var summary RunSummary
	err := scan(
		&summary.RunID,
		&summary.Status,
		&summary.Phase,
		&summary.StartedAt,
		&summary.FinishedAt,
		&summary.UpdatedAt,
		&summary.Term,
		&summary.WindowStart,
		&summary.WindowEnd,
		&summary.WeeksTotal,
		&summary.RoomsTotal,
		&summary.RequiredTotal,
		&summary.RequiredDone,
		&summary.OptionalTotal,
		&summary.OptionalDone,
		&summary.UnknownCells,
		&summary.CompositeCells,
		&summary.AttrConflictCount,
		&summary.ReleaseID,
		&summary.ReleasedAt,
		&summary.ErrorCode,
		&summary.ErrorMessage,
	)
	return summary, err
}

// RunStatusRepo 把 collect_run / collect_run_week 的读取 SQL 与 blocked 直插闭在内部。
//
// 它只被管理端消费：列表、单轮详情、冲突判定、周结果与任务汇总。SQL 全在本文件，
// 仓储不 import settings / api，避免存储层反向依赖业务层（backend/.golangci.yml
// 的 store 规则）。
type RunStatusRepo struct {
	q runStatusQuerier
}

// NewRunStatusRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewRunStatusRepo(q runStatusQuerier) RunStatusRepo {
	return RunStatusRepo{q: q}
}

// Recent 返回一页轮次摘要与全表总数，按开始时刻倒序、同刻按轮次编号倒序。
//
// limit/offset 由调用方（HTTP 层）先做范围校验，仓储只负责把它们交给 LIMIT/OFFSET；
// 越界值在这里不做二次判定，避免两处各维护一套分页口径。
//
// 列表与总数是两条语句：任一条失败都整体报错，不返回「有列表但没有总数」的半套结果。
// 没有轮次时返回空切片而不是 nil：调用方与 JSON 序列化都不必区分两种「空」。
// 所有失败都包 model.ErrRunStoreFailed（与其它读仓储一致），消息带出错阶段不带 SQL。
func (r RunStatusRepo) Recent(ctx context.Context, limit, offset int) ([]RunSummary, int, error) {
	rows, err := r.q.Query(ctx, recentRunSummariesSQL, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: 读取轮次列表失败: %w", model.ErrRunStoreFailed, err)
	}
	defer rows.Close()

	summaries := make([]RunSummary, 0)
	for rows.Next() {
		summary, scanErr := scanRunSummary(rows.Scan)
		if scanErr != nil {
			return nil, 0, fmt.Errorf("%w: 扫描轮次列表失败: %w", model.ErrRunStoreFailed, scanErr)
		}
		summaries = append(summaries, summary)
	}
	// rows.Err() 覆盖「遍历到一半连接断了」：已取到的行是残的，不能当完整一页交出去。
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("%w: 遍历轮次列表失败: %w", model.ErrRunStoreFailed, err)
	}

	var total int
	if err := r.q.QueryRow(ctx, countCollectRunsSQL).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("%w: 统计轮次总数失败: %w", model.ErrRunStoreFailed, err)
	}
	return summaries, total, nil
}

// ByID 按编号返回一轮摘要；不存在时返回 found=false 且 error 为 nil。
//
// 「没有这一轮」是正常的查询结果，不是仓储失败：handler 据此映射 404 / 40401。
// 这里不返回 model.ErrRunNotFound（design 的「哨兵的包装关系」明确保留、不使用），
// 也不把「未找到」写进 error——那会让上层把 404 当 500 处理。
func (r RunStatusRepo) ByID(ctx context.Context, runID int64) (RunSummary, bool, error) {
	summary, err := scanRunSummary(r.q.QueryRow(ctx, runSummaryByIDSQL, runID).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunSummary{}, false, nil
	}
	if err != nil {
		return RunSummary{}, false, fmt.Errorf("%w: 读取轮次 %d 失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	return summary, true, nil
}

// Running 返回当前进行中轮次；没有时 found=false。
//
// 唯一索引保证至多一条，这里取首条即可。Running 只在判冲突时用一次，但返回的是完整
// 摘要：409 文案要同时点明 run_id 与 phase，冲突时不必再查第二次库。
func (r RunStatusRepo) Running(ctx context.Context) (RunSummary, bool, error) {
	summary, err := scanRunSummary(r.q.QueryRow(ctx, runningRunSummarySQL).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunSummary{}, false, nil
	}
	if err != nil {
		return RunSummary{}, false, fmt.Errorf("%w: 读取进行中轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	return summary, true, nil
}

// Weeks 返回一轮已写入的逐楼逐周结果，按 (week, building_id) 升序。
//
// 没有行时返回空切片而不是 nil。轮次不存在也只会得到空切片：本方法是「列出已写入的
// 结果」，不负责判断轮次是否存在，未知 run_id 由 ByID 的 bool 通道表达。
func (r RunStatusRepo) Weeks(ctx context.Context, runID int64) ([]RunWeekResult, error) {
	rows, err := r.q.Query(ctx, runWeekResultsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取轮次 %d 的周结果失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	defer rows.Close()

	weeks := make([]RunWeekResult, 0)
	for rows.Next() {
		var week RunWeekResult
		if err := rows.Scan(&week.Week, &week.BuildingID, &week.OK, &week.FetchedAt, &week.ErrorMessage); err != nil {
			return nil, fmt.Errorf("%w: 扫描轮次 %d 的周结果失败: %w", model.ErrRunStoreFailed, runID, err)
		}
		weeks = append(weeks, week)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历轮次 %d 的周结果失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	return weeks, nil
}

// TaskSummary 直接复用包内既有的 LoadRunTaskSummary，不在这里重写聚合。
//
// 「必需/可选任务的失败数怎么算」只应有一份实现（runphase.go）；管理端详情与收口
// 判定若各写一条 SQL，两处迟早会对「最早失败原因」「已记录错误码」给出不同答案。
// 轮次不存在时沿用它的行为：返回包 ErrRunStoreFailed 的错误，而不是全零汇总。
func (r RunStatusRepo) TaskSummary(ctx context.Context, runID int64) (RunTaskSummary, error) {
	return LoadRunTaskSummary(ctx, r.q, runID)
}

// StartBlockedRun 直接插入一条以 blocked 为终态的轮次，返回编号与开始时刻。
//
// 只用于「白名单为空，立刻阻止」这一条路径（Requirement 8.4 / design D5）：
// 不读学期、不写窗口、不占 running 唯一索引。
//
// reason 原样写入 error_message：不做 trim，也不像 FinishRun 那样用 NULLIF 把空串
// 折成 NULL。口径是「仓储只负责落库，非空原因由调用方（task 7 的 runner）保证」；
// 传 "" 就落 ""，读回来是一段非 nil 的空串，而不是表示「没有原因」的 NULL。
// 仓储不在这里替调用方兜底：兜底会同时掩盖「调用方忘了传」和「这次真没有原因」。
//
// now 同时写 started_at / updated_at / finished_at，三个时刻一致。
func (r RunStatusRepo) StartBlockedRun(ctx context.Context, reason string, now time.Time) (int64, time.Time, error) {
	var runID int64
	var startedAt time.Time
	if err := r.q.QueryRow(ctx, startBlockedRunSQL, now, reason).Scan(&runID, &startedAt); err != nil {
		return 0, time.Time{}, fmt.Errorf("%w: 创建阻止轮次失败: %w", model.ErrRunStoreFailed, err)
	}
	return runID, startedAt, nil
}
