package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ScreenRelease 是大屏的当前版本与站点规模事实。
type ScreenRelease struct {
	ID          string    // 发布版本标识
	Term        string    // 该版本所属学期
	GeneratedAt time.Time // 版本生成时间
	TotalWeeks  int       // 该学期总周数，采集窗口缺失时兜底用
	Rooms       int       // 版本归属快照里的房间数
	Buildings   int       // 版本归属快照里的教学楼数
}

// ScreenRun 是大屏需要的一轮同步事实。
// 刻意只取状态与时间：error_code / error_message 不进结构体，
// 从数据源上就杜绝大屏外泄错误码与错误原文（Requirement 33.6、CP13）。
type ScreenRun struct {
	Status            string     // 轮次状态五档之一
	StartedAt         time.Time  // 开始时刻
	FinishedAt        *time.Time // 结束时刻；仍在跑或异常终止时为 nil
	WindowStart       *int       // 采集窗口起始周；还没定窗口时为 nil
	WindowEnd         *int       // 采集窗口结束周；还没定窗口时为 nil
	UnknownCells      int        // 未知格计数
	CompositeCells    int        // 复合格计数
	AttrConflictCount int        // 归属冲突计数
}

// ScreenOccupancyCell 是「楼 × 星期 × 大节」的占用聚合格。
// Cells 是观测格总数，Occupied 是其中按字典投影不可用的格数。
type ScreenOccupancyCell struct {
	BuildingID   string // 归属快照里的教学楼编号
	BuildingName string // 归属快照里的教学楼展示名
	Weekday      int    // 星期，1=周一 … 7=周日
	BlockOrdinal int    // 大节顺序 1..5；轴里查不到归属时为 0
	Cells        int    // 该格里的观测数
	Occupied     int    // 其中按字典投影不可用的观测数
}

// ScreenFacts 是大屏聚合需要的全部事实。
// 没有当前版本时 Release 为 nil 且其余为空：空态由查询层表达成 release:null + empty:true。
type ScreenFacts struct {
	Release   *ScreenRelease        // 当前发布版本与规模
	Published *ScreenRun            // 产出当前版本的那一轮：窗口与异常计数都取自它
	LastRun   *ScreenRun            // 最近一轮同步，可能与 Published 不是同一轮
	Occupancy []ScreenOccupancyCell // 按楼 × 星期 × 大节聚合的占用格
}

// screenQuerier 只取大屏聚合需要的两种能力，便于与驱动其余部分解耦。
type screenQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// screenReleaseSQL 取唯一的当前发布版本、学期总周数与该版本归属快照的规模。
// 规模只数 release_room_building：那是随版本切换的归属快照，
// 用 room 表会把尚未进入当前版本的房间算进站点规模。
const screenReleaseSQL = `
SELECT r.release_id, r.term, r.generated_at, t.total_weeks,
       count(rrb.room_id), count(DISTINCT rrb.building_id)
  FROM release r
  JOIN term t ON t.term = r.term
  LEFT JOIN release_room_building rrb ON rrb.release_id = r.release_id
 WHERE r.is_current
 GROUP BY r.release_id, r.term, r.generated_at, t.total_weeks
 LIMIT 1`

// screenPublishedRunSQL 取产出当前版本的那一轮同步。
// 窗口与异常计数属于发布时的数据快照，之后的失败轮次不该覆盖它们。
const screenPublishedRunSQL = `
SELECT status, started_at, finished_at, window_start, window_end,
       unknown_cells, composite_cells, attr_conflict_count
  FROM collect_run
 WHERE release_id = $1
 ORDER BY started_at DESC, run_id DESC
 LIMIT 1`

// screenLastRunSQL 取最近一轮同步；排序口径与新鲜度一致（开始时刻，同刻按 run_id）。
const screenLastRunSQL = `
SELECT status, started_at, finished_at, window_start, window_end,
       unknown_cells, composite_cells, attr_conflict_count
  FROM collect_run
 ORDER BY started_at DESC, run_id DESC
 LIMIT 1`

// screenOccupancySQL 一次扫描同时给出「楼 × 星期 × 大节」的格数与占用量。
//
// 口径与公开查询一致（Requirement 33.5、CP14）：
//   - 可用性按当前版本的字典投影取，而不是另立一套判断；
//   - 字典里查不到的状态（含 unknown / composite 这类不可用状态）一律算已占用：
//     LEFT JOIN + available IS NOT TRUE 保证缺字典行时也计入占用，不会把未知格当成空闲；
//   - 归属取当前版本的 release_room_building 快照，与空教室查询的楼筛选同源。
//
// 轴节点缺失时 LEFT JOIN 出 block_ordinal = 0：这些格仍进楼口径的总数，
// 但不进 7 × 5 矩阵（矩阵列只表达 5 个大节）。
const screenOccupancySQL = `
SELECT rrb.building_id,
       rrb.building_name,
       o.weekday::int,
       coalesce(an.block_ordinal, 0)::int,
       count(*),
       count(*) FILTER (WHERE ds.available IS NOT TRUE)
  FROM observation o
  JOIN release rel
    ON rel.release_id = o.release_id
   AND rel.is_current
  JOIN release_room_building rrb
    ON rrb.release_id = o.release_id
   AND rrb.room_id = o.room_id
  LEFT JOIN axis_node an
    ON an.axis_version = rel.axis_version
   AND an.node = o.node
  LEFT JOIN dict_state ds
    ON ds.dict_version = rel.dict_version
   AND ds.state_key = o.state_key
 WHERE o.term = rel.term
 GROUP BY rrb.building_id, rrb.building_name, o.weekday, an.block_ordinal
 ORDER BY rrb.building_id, o.weekday, an.block_ordinal`

// Screen 读取大屏需要的版本、规模、轮次与占用事实。
// 没有当前版本时只返回空事实，不查轮次与占用：空态由查询层负责形状（Requirement 33.8）。
func Screen(ctx context.Context, q screenQuerier) (ScreenFacts, error) {
	facts := ScreenFacts{Occupancy: make([]ScreenOccupancyCell, 0)}

	release, err := screenCurrentRelease(ctx, q)
	if err != nil {
		return ScreenFacts{}, err
	}
	// 无当前版本是契约里的空态，不是错误：不返回 40401，也不把空库当故障。
	if release == nil {
		return facts, nil
	}
	facts.Release = release

	published, err := screenRunByRelease(ctx, q, release.ID)
	if err != nil {
		return ScreenFacts{}, err
	}
	facts.Published = published

	lastRun, err := screenLatestRun(ctx, q)
	if err != nil {
		return ScreenFacts{}, err
	}
	facts.LastRun = lastRun

	cells, err := screenOccupancyCells(ctx, q)
	if err != nil {
		return ScreenFacts{}, err
	}
	facts.Occupancy = cells

	return facts, nil
}

// screenCurrentRelease 读当前发布版本与规模；没有当前版本时返回 nil，不返回错误。
func screenCurrentRelease(ctx context.Context, q screenQuerier) (*ScreenRelease, error) {
	var release ScreenRelease
	err := q.QueryRow(ctx, screenReleaseSQL).Scan(
		&release.ID, &release.Term, &release.GeneratedAt, &release.TotalWeeks,
		&release.Rooms, &release.Buildings)
	// 空库或从未发布过时没有当前版本，交给调用方按空态处理。
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 查询大屏版本与规模失败: %w", model.ErrQueryFailed, err)
	}
	return &release, nil
}

// screenRunByRelease 读产出指定版本的那一轮同步；没有记录时返回 nil。
func screenRunByRelease(ctx context.Context, q screenQuerier, releaseID string) (*ScreenRun, error) {
	run, err := scanScreenRun(q.QueryRow(ctx, screenPublishedRunSQL, releaseID))
	// 版本存在却查不到产出轮次（历史数据或手工导入）不是错误：窗口与计数留空即可。
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 查询大屏发布轮次失败: %w", model.ErrQueryFailed, err)
	}
	return run, nil
}

// screenLatestRun 读最近一轮同步；一轮都没跑过时返回 nil。
func screenLatestRun(ctx context.Context, q screenQuerier) (*ScreenRun, error) {
	run, err := scanScreenRun(q.QueryRow(ctx, screenLastRunSQL))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 查询大屏最近轮次失败: %w", model.ErrQueryFailed, err)
	}
	return run, nil
}

// scanScreenRun 把一行轮次事实扫进 ScreenRun。
// row 已由调用方给出 SQL，扫描列固定为状态、时间、窗口与三个计数。
func scanScreenRun(row pgx.Row) (*ScreenRun, error) {
	var run ScreenRun
	err := row.Scan(&run.Status, &run.StartedAt, &run.FinishedAt,
		&run.WindowStart, &run.WindowEnd,
		&run.UnknownCells, &run.CompositeCells, &run.AttrConflictCount)
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// screenOccupancyCells 读当前版本的占用聚合格；没有当前版本时返回空列表。
func screenOccupancyCells(ctx context.Context, q screenQuerier) ([]ScreenOccupancyCell, error) {
	rows, err := q.Query(ctx, screenOccupancySQL)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询大屏占用分布失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	cells := make([]ScreenOccupancyCell, 0)
	for rows.Next() {
		var cell ScreenOccupancyCell
		if err := rows.Scan(&cell.BuildingID, &cell.BuildingName, &cell.Weekday,
			&cell.BlockOrdinal, &cell.Cells, &cell.Occupied); err != nil {
			return nil, fmt.Errorf("%w: 扫描大屏占用分布失败: %w", model.ErrQueryFailed, err)
		}
		cells = append(cells, cell)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历大屏占用分布失败: %w", model.ErrQueryFailed, err)
	}
	return cells, nil
}
