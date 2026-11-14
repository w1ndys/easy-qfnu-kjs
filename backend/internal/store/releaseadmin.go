package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ReleaseSummary 是管理端版本列表的一行，对应 release 一行加上三个按版本聚合的计数。
//
// 顺序字段是 GeneratedAt 而不是「实际发布时刻」：release 表没有 released_at 列
// （0001 基线），版本先后的既有口径就是 generated_at（store/publish.go 的
// deleteOlderReleasesSQL 用它挑「上一版」）。ReleasedAt 从 collect_run 左连接补充，
// 只作展示，不参与排序。
//
// 三个计数都是小节/行粒度的快照值，不是详情里的「格」：
//   - WeekCount：该版有观测的周数，count(DISTINCT observation.week)。注意这与详情的
//     week_count 不是同一口径：详情的逐周语句内连接 axis_node，只有能折叠出大节的周
//     才成行，所以详情 week_count = 可折叠周数，可能小于这里的「有观测周数」；
//   - ObservationRows：该版观测行数（小节粒度），count(*)；
//   - Rooms：该版归属快照行数（release_room_building），不是全局教室目录的房号数。
type ReleaseSummary struct {
	ReleaseID       string     // 版本标识，release.release_id
	Term            string     // 学期，release.term
	GeneratedAt     time.Time  // 版本生成时间，也是列表排序键
	ReleasedAt      *time.Time // 发布时刻，collect_run.released_at；没有关联轮次时为 nil
	IsCurrent       bool       // 是否为公开查询读取的当前版本
	HasAttrConflict bool       // 该版是否有归属冲突
	WeekCount       int        // 列表口径：有观测的周数；详情口径是可折叠周数，见 ReleaseStats
	ObservationRows int64      // 观测行数（小节粒度）
	Rooms           int        // 归属快照房号数
	DictVersion     int        // 字典版本
	AxisVersion     int        // 轴版本
}

// ReleaseStats 是版本详情的总量，全部由详情的三条语句拼出，不再额外发聚合。
type ReleaseStats struct {
	ObservationRows int64 // 观测行数（小节粒度），逐周计数之和
	BlockCells      int   // 该版格数：房号 × 周 × 星期 × 大节，逐周计数之和
	WeekCount       int   // 可折叠周数，即 WeekCounts 的长度（逐周语句内连接 axis_node，见下）
	Rooms           int   // 归属快照房号数，取自头语句
	Buildings       int   // 归属快照覆盖的楼数，即 BuildingCounts 的长度
}

// ReleaseWeekCount 是某一周的覆盖面：观测行数与小节折叠后的大节格数。
type ReleaseWeekCount struct {
	Week            int   // 教学周次
	ObservationRows int64 // 该周观测行数（小节粒度）
	BlockCells      int   // 该周格数：房号 × 星期 × 大节
}

// ReleaseBuildingCount 是某一栋楼的归属快照房号数。
type ReleaseBuildingCount struct {
	BuildingID   string // 教学楼编号
	BuildingName string // 教学楼展示名，来自发布时的归属快照
	Rooms        int    // 该楼在本版的房号数
}

// ReleaseDetailView 是 GET /api/admin/releases/{release_id} 的数据：版本头、
// 总量、逐周覆盖与按楼分布。
//
// 名字不叫 ReleaseDetail：那个名字在本包已经被 publish.go 占用，指的是
// occupancy_detail 的一行（占用说明）。两者语义完全不同，这里不复用名字，
// 也不为了改名去动发布写侧的既有类型（见 design 的接口签名，实现时发现同名冲突）。
//
// WeekCounts 与 BuildingCounts 恒为非 nil 切片：没有观测或没有归属快照时是空切片，
// 序列化成 JSON 的 [] 而不是 null，前端不必区分两种「空」。
type ReleaseDetailView struct {
	Release        ReleaseSummary         // 版本头，WeekCount / ObservationRows 由逐周结果回填
	Stats          ReleaseStats           // 总量，由头语句与逐周/按楼结果拼出
	WeekCounts     []ReleaseWeekCount     // 按周次升序
	BuildingCounts []ReleaseBuildingCount // 按 building_id 升序
}

// releaseAdminPool 是版本读取仓储需要的最小能力：多行读、单行读与只读事务。
//
// 用窄接口而不是 *pgxpool.Pool，与 publishQuerier / runStatusQuerier 同一写法：
// 单元测试可用替身验证错误口径，装配时交同一个连接池。BeginTx 是详情必需的：
// 头条、逐周计数与按楼计数要走同一个快照（发布事务会删更早版本，多语句读若跨
// 快照就会给出「头还在、明细已经没了」的半份结果）。
//
// QueryRow 是 design 为本类型声明的方法，当前两个读方法都用不到（列表只用 Query，
// 详情的单行读在事务体内）。保留它是为了与 design 的接口形状一致：后续对比/异常/
// 回退条目若需要「不进事务的单行读」，不必再回来扩接口。
type releaseAdminPool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseDetailQuerier 是详情事务体内需要的两种读能力；pgx.Tx 天然满足。
// 把事务体与开事务拆开，让「三条语句怎么拼出 stats」能用一个替身单独验证，
// 不必连库（与 publish.go 把 writeRelease 从 CommitRelease 拆出来同一理由）。
type releaseDetailQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseListSQL 列出库中全部版本，按 generated_at 倒序、同刻按版本标识倒序。
//
// 排序键与发布事务的 deleteOlderReleasesSQL 完全一致：列表首条是生成时间最新的版本，
// 而「上一版」正是它后面那条。用 released_at 排序会让列表顺序与保留口径分叉
// （版本表没有那一列，也不打算加）。
//
// 三个计数各用一个 LEFT JOIN LATERAL 聚合，而不是平铺 LEFT JOIN 三张表：
// 平铺时 observation 与 release_room_building 的行会互相放大，count(*) 直接失真；
// LATERAL 让每个聚合各自压成一行，再按 release_id 贴回来。聚合在空集上仍返回一行
// （count 为 0、max 为 NULL），所以 LEFT JOIN 的外连接语义只体现在 NULL 的
// released_at 上，COALESCE 只是把「理论上不会 NULL」的三个计数写成确定值。
//
// released_at 取 max：正常一个版本只由一轮发布，手工修数据造成重复关联时 max 让它
// 仍是一行、而不是把版本行复制成多行；没有关联轮次时为 NULL（Requirement 1.3）。
//
// 不分页：release 表上限两行（current + previous），分页没有意义；total 由调用方
// 取 len。这里也不加 LIMIT，列表语义是「全部在库版本」。
const releaseListSQL = `
SELECT r.release_id, r.term, r.generated_at, r.dict_version, r.axis_version,
       r.has_attr_conflict, r.is_current,
       run.released_at,
       COALESCE(obs.observation_rows, 0), COALESCE(obs.week_count, 0),
       COALESCE(rooms.rooms, 0)
  FROM release r
  LEFT JOIN LATERAL (
        SELECT max(cr.released_at) AS released_at
          FROM collect_run cr
         WHERE cr.release_id = r.release_id
       ) run ON true
  LEFT JOIN LATERAL (
        SELECT count(*) AS observation_rows, count(DISTINCT o.week) AS week_count
          FROM observation o
         WHERE o.release_id = r.release_id
       ) obs ON true
  LEFT JOIN LATERAL (
        SELECT count(*) AS rooms
          FROM release_room_building b
         WHERE b.release_id = r.release_id
       ) rooms ON true
 ORDER BY r.generated_at DESC, r.release_id DESC`

// scanReleaseSummary 按 releaseListSQL 的列序把一行扫进 ReleaseSummary。
//
// 形参是「扫描函数」而不是 pgx.Row：Rows.Scan 与 Row.Scan 签名一致，将来详情或其他
// 读取路径复用同一列序时不会出现「一处加了列、另一处忘了加」的偏差（与
// scanRunSummary 同一写法）。
func scanReleaseSummary(scan func(dest ...any) error) (ReleaseSummary, error) {
	var summary ReleaseSummary
	err := scan(
		&summary.ReleaseID,
		&summary.Term,
		&summary.GeneratedAt,
		&summary.DictVersion,
		&summary.AxisVersion,
		&summary.HasAttrConflict,
		&summary.IsCurrent,
		&summary.ReleasedAt,
		&summary.ObservationRows,
		&summary.WeekCount,
		&summary.Rooms,
	)
	return summary, err
}

// releaseDetailHeadSQL 读版本头：release 列 + released_at + 归属快照房号数。
//
// 有意不带观测聚合：observation 是全库最大的一张表（一版几十万到上百万行），
// week_count / observation_rows / block_cells 全部由下面那条逐周计数算出，
// 头条再扫一遍同一个版本等于白付一次全表前缀扫。
//
// 归属快照只有几千行，用标量子查询即可，不必再开一个 LATERAL。
const releaseDetailHeadSQL = `
SELECT r.release_id, r.term, r.generated_at, r.dict_version, r.axis_version,
       r.has_attr_conflict, r.is_current, run.released_at,
       (SELECT count(*) FROM release_room_building b WHERE b.release_id = r.release_id) AS rooms
  FROM release r
  LEFT JOIN LATERAL (
        SELECT max(cr.released_at) AS released_at
          FROM collect_run cr
         WHERE cr.release_id = r.release_id
       ) run ON true
 WHERE r.release_id = $1`

// releaseWeekCountsSQL 按周扫一次观测，同时给出观测行数与大节格数。
//
// $2 是该版自己的 axis_version（从头语句读回）：小节 → 大节的映射随轴改版变化，
// 不能用「当前」轴版本去折叠历史版本。
//
// count(DISTINCT (o.room_id, o.weekday, an.block)) 是 PostgreSQL 的行构造去重：
// 同一格内的 2–3 个小节只算一个格（CP3）。周已经在 GROUP BY 里，所以去重键不必
// 再带 week。
//
// 内连接 axis_node 决定「详情 week_count = 可折叠周数」：节点的 block 映射按该版
// 轴版本给出，查不到映射的观测行会被整行滤掉——不只格数与观测行数可能小于列表的
// count(DISTINCT observation.week) 与 count(*)，连周都可能不成行。因此详情这里报的
// 周数是「能折叠出大节的周数」，不是「有观测的周数」；两种口径各自服务列表与详情，
// 不互相替代。
//
// 排序固定按周次升序：面板按周分组展示，行序不能跟着索引或计划漂移。
const releaseWeekCountsSQL = `
SELECT o.week,
       count(*) AS observation_rows,
       count(DISTINCT (o.room_id, o.weekday, an.block)) AS block_cells
  FROM observation o
  JOIN axis_node an ON an.axis_version = $2 AND an.node = o.node
 WHERE o.release_id = $1
 GROUP BY o.week
 ORDER BY o.week`

// releaseBuildingCountsSQL 按楼统计归属快照，排序按 building_id 升序。
//
// 分组键带 building_name：同一楼编号在快照里应当只有一个展示名，万一出现两个，
// 这里如实分成两行而不是用 min() 掩盖——那是数据异常，管理端应当看见。
const releaseBuildingCountsSQL = `
SELECT b.building_id, b.building_name, count(*) AS rooms
  FROM release_room_building b
 WHERE b.release_id = $1
 GROUP BY b.building_id, b.building_name
 ORDER BY b.building_id`

// ReleaseAdminRepo 把管理端版本读取的 SQL 与只读事务闭在内部。
//
// 它是 api.ReleaseAdminReader 的结构匹配实现：store 不 import api（依赖方向见
// backend/.golangci.yml 的 store 规则），接口定义在消费侧，方法集在这里一一对应。
// 本文件当前只有列表与详情两个读方法（tasklist 第 1 项）；对比、异常与回退
// 由后续条目补到同一类型上。
type ReleaseAdminRepo struct {
	pool releaseAdminPool // 通常是 *pgxpool.Pool
}

// NewReleaseAdminRepo 绑定一个连接池。
func NewReleaseAdminRepo(pool releaseAdminPool) ReleaseAdminRepo {
	return ReleaseAdminRepo{pool: pool}
}

// List 返回库中全部版本摘要，按生成时间倒序、同刻按版本标识倒序。
//
// 没有版本时返回空切片而不是 nil：调用方与 JSON 序列化都不必区分两种「空」，
// total 由调用方取 len（空库即 0，Requirement 1.4）。
//
// 读库失败一律包 model.ErrQueryFailed（api 映射 50000）；不返回「取到一半」的
// 部分列表——rows.Err() 覆盖「遍历到一半连接断了」，此时已取到的行是残的。
func (r ReleaseAdminRepo) List(ctx context.Context) ([]ReleaseSummary, error) {
	rows, err := r.pool.Query(ctx, releaseListSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本列表失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	summaries := make([]ReleaseSummary, 0)
	for rows.Next() {
		summary, scanErr := scanReleaseSummary(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描版本列表失败: %w", model.ErrQueryFailed, scanErr)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本列表失败: %w", model.ErrQueryFailed, err)
	}
	return summaries, nil
}

// Detail 返回一个版本的详情；版本不存在时返回 (零值, false, nil)。
//
// 三条语句包在一个 REPEATABLE READ READ ONLY 事务里：
//   - REPEATABLE READ：发布事务会删更早版本（deleteOlderReleasesSQL）。多语句读若
//     各看各的快照，就会出现「头条还读得到这一版、逐周计数已经空了」的半份结果（CP7）。
//   - READ ONLY：本路径只有 SELECT，让误写在这里直接失败，而不是靠人工审查（CP2）。
//
// 「没有这个版本」是正常查询结果，不是仓储失败：handler 据此映射 404 / 40401，
// 因此不把 pgx.ErrNoRows 写进 error（与 RunStatusRepo.ByID 同口径）。
func (r ReleaseAdminRepo) Detail(ctx context.Context, releaseID string) (ReleaseDetailView, bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return ReleaseDetailView{}, false, fmt.Errorf("%w: 开启版本详情只读事务失败: %w", model.ErrQueryFailed, err)
	}
	// 只读事务不需要提交：回滚与提交一样只是结束快照并归还连接，不会产生任何写入。
	// 所有失败路径也靠这里收尾（与 CommitRelease 的 defer Rollback 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	return loadReleaseDetail(ctx, tx, releaseID)
}

// loadReleaseDetail 是 Detail 的事务体：只负责三条读语句与拼装，不负责开事务。
//
// stats 与版本头里的 week_count / observation_rows 全部由逐周结果拼出，不额外发
// 聚合语句（design「版本详情」的约束）：
//   - stats.observation_rows = 逐周 observation_rows 之和；
//   - stats.block_cells      = 逐周 block_cells 之和；
//   - stats.week_count       = 逐周结果的行数，也就是可折叠周数（见 releaseWeekCountsSQL）；
//   - release.week_count / release.observation_rows 同上；
//   - stats.rooms            = 头语句的归属快照行数；
//   - stats.buildings        = 按楼结果的行数。
//
// 头条缺失时立刻返回 (零值, false, nil)，不再发后两条语句。
func loadReleaseDetail(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
) (ReleaseDetailView, bool, error) {
	var detail ReleaseDetailView
	err := q.QueryRow(ctx, releaseDetailHeadSQL, releaseID).Scan(
		&detail.Release.ReleaseID,
		&detail.Release.Term,
		&detail.Release.GeneratedAt,
		&detail.Release.DictVersion,
		&detail.Release.AxisVersion,
		&detail.Release.HasAttrConflict,
		&detail.Release.IsCurrent,
		&detail.Release.ReleasedAt,
		&detail.Release.Rooms,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseDetailView{}, false, nil
	}
	if err != nil {
		return ReleaseDetailView{}, false, fmt.Errorf("%w: 读取版本 %s 失败: %w", model.ErrQueryFailed, releaseID, err)
	}

	weekCounts, err := loadReleaseWeekCounts(ctx, q, releaseID, detail.Release.AxisVersion)
	if err != nil {
		return ReleaseDetailView{}, false, err
	}
	buildingCounts, err := loadReleaseBuildingCounts(ctx, q, releaseID)
	if err != nil {
		return ReleaseDetailView{}, false, err
	}

	detail.WeekCounts = weekCounts
	detail.BuildingCounts = buildingCounts
	detail.Stats = ReleaseStats{
		Rooms:     detail.Release.Rooms,
		WeekCount: len(weekCounts),
		Buildings: len(buildingCounts),
	}
	for _, week := range weekCounts {
		detail.Stats.ObservationRows += week.ObservationRows
		detail.Stats.BlockCells += week.BlockCells
	}
	detail.Release.ObservationRows = detail.Stats.ObservationRows
	detail.Release.WeekCount = detail.Stats.WeekCount
	return detail, true, nil
}

// loadReleaseWeekCounts 读逐周覆盖；没有观测时返回空切片而不是 nil。
func loadReleaseWeekCounts(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
	axisVersion int,
) ([]ReleaseWeekCount, error) {
	rows, err := q.Query(ctx, releaseWeekCountsSQL, releaseID, axisVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	defer rows.Close()

	weekCounts := make([]ReleaseWeekCount, 0)
	for rows.Next() {
		var (
			week   int16
			counts ReleaseWeekCount
		)
		if err := rows.Scan(&week, &counts.ObservationRows, &counts.BlockCells); err != nil {
			return nil, fmt.Errorf("%w: 扫描版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
		}
		counts.Week = int(week)
		weekCounts = append(weekCounts, counts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	return weekCounts, nil
}

// loadReleaseBuildingCounts 读按楼分布；没有归属快照时返回空切片而不是 nil。
func loadReleaseBuildingCounts(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
) ([]ReleaseBuildingCount, error) {
	rows, err := q.Query(ctx, releaseBuildingCountsSQL, releaseID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	defer rows.Close()

	buildingCounts := make([]ReleaseBuildingCount, 0)
	for rows.Next() {
		var count ReleaseBuildingCount
		if err := rows.Scan(&count.BuildingID, &count.BuildingName, &count.Rooms); err != nil {
			return nil, fmt.Errorf("%w: 扫描版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
		}
		buildingCounts = append(buildingCounts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	return buildingCounts, nil
}
