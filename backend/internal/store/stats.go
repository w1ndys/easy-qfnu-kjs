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

// 访问统计的读写仓储：只做 SQL 与行映射。
// 匿名摘要、窗口口径与排行整形都在 internal/stats（design 依赖方向 stats → store）。
// 统计表只有计数与 visitor_hash，不含 IP、User-Agent、来源页与查询原文（CP16）。

// StatHourPV 是 (day, hour) 上的一次 PV 增量。
type StatHourPV struct {
	Day     time.Time // 业务日期，按 Asia/Shanghai 解释；编码为 date
	Hour    int       // 小时 0..23
	Weekday int       // 星期 1=周一 … 7=周日，供热力表按星期聚合
	PV      int       // 本次增量
}

// StatVisitorKey 是一条 UV 去重键：同一 (day, hour, hash) 只落一行。
type StatVisitorKey struct {
	Day  time.Time // 业务日期
	Hour int       // 小时 0..23
	Hash string    // 匿名摘要，不含原始标识
}

// StatDayHits 是某日某个编号维度键的命中增量，楼编号与房号共用这个形状。
type StatDayHits struct {
	Day  time.Time // 业务日期
	Key  string    // 楼编号或房号
	Hits int       // 本次增量
}

// StatKeywordHits 是某日某个关键词的命中增量。
type StatKeywordHits struct {
	Day     time.Time // 业务日期
	Keyword string    // 已归一化并截断的关键词
	Hits    int       // 本次增量
}

// StatNodeHits 是某日某个小节的命中增量。
type StatNodeHits struct {
	Day  time.Time // 业务日期
	Node string    // 小节编号 01..12
	Hits int       // 本次增量
}

// StatsBatch 是一批记账聚合后的增量。
// 同一批内同一主键只出现一次（PV、维度命中已在内存累计；visitor 按批内去重），
// 这样每条 upsert 语句都不会对同一行更新两次。
type StatsBatch struct {
	Hours     []StatHourPV     // 按 (day, hour) 累加 PV
	Visitors  []StatVisitorKey // 按 (day, hour, hash) 去重
	Buildings []StatDayHits    // 按 (day, building_id) 累加
	Rooms     []StatDayHits    // 按 (day, room_id) 累加
	Keywords  []StatKeywordHits
	Nodes     []StatNodeHits
}

// 写入语句统一用 unnest 展开成多行：一批记账一次往返，避免按条写入。
// 冲突时的累加写在 SQL 里，调用方只负责给增量。

const statHourUpsertSQL = `
INSERT INTO stat_hour (day, hour, weekday, pv)
SELECT d, h, w, p
  FROM unnest($1::date[], $2::smallint[], $3::smallint[], $4::integer[]) AS t(d, h, w, p)
ON CONFLICT (day, hour) DO UPDATE SET pv = stat_hour.pv + EXCLUDED.pv`

// statVisitorInsertSQL 只插入新键：同一访客同一小时重复到达时保持一行，
// 日 UV 由 count(DISTINCT visitor_hash) 求得，小时 UV 由行数求得（CP17）。
const statVisitorInsertSQL = `
INSERT INTO stat_visitor (day, hour, visitor_hash)
SELECT d, h, v
  FROM unnest($1::date[], $2::smallint[], $3::text[]) AS t(d, h, v)
ON CONFLICT DO NOTHING`

const statBuildingUpsertSQL = `
INSERT INTO stat_building_day (day, building_id, hits)
SELECT d, k, n
  FROM unnest($1::date[], $2::text[], $3::integer[]) AS t(d, k, n)
ON CONFLICT (day, building_id) DO UPDATE SET hits = stat_building_day.hits + EXCLUDED.hits`

const statRoomUpsertSQL = `
INSERT INTO stat_room_day (day, room_id, hits)
SELECT d, k, n
  FROM unnest($1::date[], $2::text[], $3::integer[]) AS t(d, k, n)
ON CONFLICT (day, room_id) DO UPDATE SET hits = stat_room_day.hits + EXCLUDED.hits`

const statKeywordUpsertSQL = `
INSERT INTO stat_keyword_day (day, keyword, hits)
SELECT d, k, n
  FROM unnest($1::date[], $2::text[], $3::integer[]) AS t(d, k, n)
ON CONFLICT (day, keyword) DO UPDATE SET hits = stat_keyword_day.hits + EXCLUDED.hits`

const statNodeUpsertSQL = `
INSERT INTO stat_node_day (day, node, hits)
SELECT d, k::char(2), n
  FROM unnest($1::date[], $2::text[], $3::integer[]) AS t(d, k, n)
ON CONFLICT (day, node) DO UPDATE SET hits = stat_node_day.hits + EXCLUDED.hits`

// 读取语句：窗口一律按 $1..$2 的按日闭区间传，聚合结果统一 cast 成 bigint 便于扫描。

// statPVUVSQL 一次返回窗口 PV 与窗口 UV。
// UV 是「逐日 count(DISTINCT visitor_hash) 之和」：Requirement 34.3 规定的去重单位是自然日，
// 换日重新计，所以跨日窗口不能对同一个摘要再做一次跨日去重（摘要本身已按天加盐）。
const statPVUVSQL = `
SELECT coalesce((SELECT sum(pv) FROM stat_hour WHERE day BETWEEN $1 AND $2), 0)::bigint,
       coalesce((
         SELECT sum(c)
           FROM (
             SELECT count(DISTINCT visitor_hash) AS c
               FROM stat_visitor
              WHERE day BETWEEN $1 AND $2
              GROUP BY day
           ) AS daily
       ), 0)::bigint`

const statHeatmapSQL = `
SELECT weekday, hour, sum(pv)::bigint
  FROM stat_hour
 WHERE day BETWEEN $1 AND $2
 GROUP BY weekday, hour
 ORDER BY weekday, hour`

const statTrendSQL = `
SELECT day, sum(pv)::bigint
  FROM stat_hour
 WHERE day BETWEEN $1 AND $2
 GROUP BY day
 ORDER BY day`

// statBuildingTopSQL 只统计白名单内的楼：$4 是白名单编号数组。
// 过滤放在 SQL 里先于 LIMIT：白名单外的键（包括被伪造的楼编号）既不会出现在榜上，
// 也不会挤掉白名单楼的 Top N 名额。
const statBuildingTopSQL = `
SELECT building_id, sum(hits)::bigint
  FROM stat_building_day
 WHERE day BETWEEN $1 AND $2
   AND building_id = ANY($4::text[])
 GROUP BY building_id
 ORDER BY sum(hits) DESC, building_id ASC
 LIMIT $3`

// statRoomTopSQL 用 room 表补展示名：统计表只存房号，不存房名。
const statRoomTopSQL = `
SELECT s.room_id, coalesce(r.name, ''), sum(s.hits)::bigint
  FROM stat_room_day s
  LEFT JOIN room r ON r.room_id = s.room_id
 WHERE s.day BETWEEN $1 AND $2
 GROUP BY s.room_id, r.name
 ORDER BY sum(s.hits) DESC, coalesce(r.name, '') ASC, s.room_id ASC
 LIMIT $3`

const statKeywordTopSQL = `
SELECT keyword, sum(hits)::bigint
  FROM stat_keyword_day
 WHERE day BETWEEN $1 AND $2
 GROUP BY keyword
 ORDER BY sum(hits) DESC, keyword ASC
 LIMIT $3`

const statNodeSQL = `
SELECT node::text, sum(hits)::bigint
  FROM stat_node_day
 WHERE day BETWEEN $1 AND $2
 GROUP BY node
 ORDER BY node`

// statDeleteSQL 用一条语句清掉六张日表里早于边界的记录。
// PostgreSQL 会执行 WITH 里全部数据修改语句，即使主查询不引用它们的输出。
const statDeleteSQL = `
WITH hour AS (DELETE FROM stat_hour WHERE day < $1 RETURNING 1),
     visitor AS (DELETE FROM stat_visitor WHERE day < $1 RETURNING 1),
     building AS (DELETE FROM stat_building_day WHERE day < $1 RETURNING 1),
     room AS (DELETE FROM stat_room_day WHERE day < $1 RETURNING 1),
     keyword AS (DELETE FROM stat_keyword_day WHERE day < $1 RETURNING 1),
     node AS (DELETE FROM stat_node_day WHERE day < $1 RETURNING 1)
SELECT (SELECT count(*) FROM hour)
     + (SELECT count(*) FROM visitor)
     + (SELECT count(*) FROM building)
     + (SELECT count(*) FROM room)
     + (SELECT count(*) FROM keyword)
     + (SELECT count(*) FROM node)`

// statExecutor 是写路径所需的最小能力；*pgxpool.Pool 与 pgx.Tx 都满足。
type statExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// statQuerier 是读路径所需的最小能力，便于与驱动其余部分解耦。
type statQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// WriteStats 把一批聚合后的增量写入六张统计表。
// 六条语句各自原子、互不阻塞：某一张表失败只影响该维度，其余维度照常写入，
// 失败原因合并后一并返回给调用方记日志（CP18 只要求失败不外溢）。
// 六段不能因为一段失败就提前返回：那会让同批的其余维度整体丢失，
// 而这批记账已经离开队列，不会再重试。
func WriteStats(ctx context.Context, q statExecutor, batch StatsBatch) error {
	var failures []error

	if len(batch.Hours) > 0 {
		days, hours, weekdays, pvs := statHourColumns(batch.Hours)
		if _, err := q.Exec(ctx, statHourUpsertSQL, days, hours, weekdays, pvs); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入小时 PV 失败: %w", model.ErrStatsFailed, err))
		}
	}
	if len(batch.Visitors) > 0 {
		days, hours, hashes := statVisitorColumns(batch.Visitors)
		if _, err := q.Exec(ctx, statVisitorInsertSQL, days, hours, hashes); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入访客摘要失败: %w", model.ErrStatsFailed, err))
		}
	}
	if len(batch.Buildings) > 0 {
		days, keys, hits := statDayHitsColumns(batch.Buildings)
		if _, err := q.Exec(ctx, statBuildingUpsertSQL, days, keys, hits); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入教学楼统计失败: %w", model.ErrStatsFailed, err))
		}
	}
	if len(batch.Rooms) > 0 {
		days, keys, hits := statDayHitsColumns(batch.Rooms)
		if _, err := q.Exec(ctx, statRoomUpsertSQL, days, keys, hits); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入教室统计失败: %w", model.ErrStatsFailed, err))
		}
	}
	if len(batch.Keywords) > 0 {
		days, keywords, hits := statKeywordColumns(batch.Keywords)
		if _, err := q.Exec(ctx, statKeywordUpsertSQL, days, keywords, hits); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入关键词统计失败: %w", model.ErrStatsFailed, err))
		}
	}
	if len(batch.Nodes) > 0 {
		days, nodes, hits := statNodeColumns(batch.Nodes)
		if _, err := q.Exec(ctx, statNodeUpsertSQL, days, nodes, hits); err != nil {
			failures = append(failures, fmt.Errorf("%w: 写入节次统计失败: %w", model.ErrStatsFailed, err))
		}
	}
	return errors.Join(failures...)
}

// 列拆解函数把行结构转成 unnest 需要的定长数组；切片长度即行数，顺序不重要。

// statHourColumns 拆出 (day, hour, weekday, pv) 四个数组。
func statHourColumns(rows []StatHourPV) ([]time.Time, []int16, []int16, []int32) {
	days := make([]time.Time, len(rows))
	hours := make([]int16, len(rows))
	weekdays := make([]int16, len(rows))
	pvs := make([]int32, len(rows))
	for i, row := range rows {
		days[i] = row.Day
		hours[i] = int16(row.Hour)
		weekdays[i] = int16(row.Weekday)
		pvs[i] = int32(row.PV)
	}
	return days, hours, weekdays, pvs
}

// statVisitorColumns 拆出 (day, hour, hash) 三个数组。
func statVisitorColumns(rows []StatVisitorKey) ([]time.Time, []int16, []string) {
	days := make([]time.Time, len(rows))
	hours := make([]int16, len(rows))
	hashes := make([]string, len(rows))
	for i, row := range rows {
		days[i] = row.Day
		hours[i] = int16(row.Hour)
		hashes[i] = row.Hash
	}
	return days, hours, hashes
}

// statDayHitsColumns 拆出 (day, key, hits) 三个数组，楼与房共用。
func statDayHitsColumns(rows []StatDayHits) ([]time.Time, []string, []int32) {
	days := make([]time.Time, len(rows))
	keys := make([]string, len(rows))
	hits := make([]int32, len(rows))
	for i, row := range rows {
		days[i] = row.Day
		keys[i] = row.Key
		hits[i] = int32(row.Hits)
	}
	return days, keys, hits
}

// statKeywordColumns 拆出 (day, keyword, hits) 三个数组。
func statKeywordColumns(rows []StatKeywordHits) ([]time.Time, []string, []int32) {
	days := make([]time.Time, len(rows))
	keywords := make([]string, len(rows))
	hits := make([]int32, len(rows))
	for i, row := range rows {
		days[i] = row.Day
		keywords[i] = row.Keyword
		hits[i] = int32(row.Hits)
	}
	return days, keywords, hits
}

// statNodeColumns 拆出 (day, node, hits) 三个数组。
func statNodeColumns(rows []StatNodeHits) ([]time.Time, []string, []int32) {
	days := make([]time.Time, len(rows))
	nodes := make([]string, len(rows))
	hits := make([]int32, len(rows))
	for i, row := range rows {
		days[i] = row.Day
		nodes[i] = row.Node
		hits[i] = int32(row.Hits)
	}
	return days, nodes, hits
}

// StatRange 是一段按日闭区间窗口，From 与 To 都是业务日期。
type StatRange struct {
	From time.Time
	To   time.Time
}

// StatWindows 是读取侧需要的四段窗口。
// 拆成四段而不是在读取函数里算：窗口口径属于 internal/stats，仓储只按给定区间取值。
type StatWindows struct {
	Today StatRange // 今日
	D7    StatRange // 近 7 日：PV/UV 与节次分布共用
	D30   StatRange // 近 30 日：PV/UV 与逐日趋势共用
	Peak  StatRange // 热力（含峰值）与三张 Top 10 榜单共用：近 7 日或近 30 日
}

// StatPVUV 是一段窗口的 PV 与 UV。
type StatPVUV struct {
	PV int // 窗口内查询次数之和
	UV int // 窗口内逐日去重访客数之和
}

// StatBuildingHit 是教学楼榜的一行。
type StatBuildingHit struct {
	ID   string // 楼编号
	Name string // 白名单里的展示名；已移出白名单时为空
	Hits int    // 窗口内命中次数
}

// StatRoomHit 是教室榜的一行。
type StatRoomHit struct {
	ID   string // 房号
	Name string // room 表里的展示名；房间已不存在时为空
	Hits int    // 窗口内命中次数
}

// StatKeywordHit 是关键词榜的一行。
type StatKeywordHit struct {
	Text string // 已归一化并截断的关键词
	Hits int
}

// StatNodeHit 是节次分布的一行。
type StatNodeHit struct {
	Node string // 小节编号 01..12
	Hits int
}

// StatHeatCell 是热力表里的一格。
type StatHeatCell struct {
	Weekday int // 1=周一 … 7=周日
	Hour    int // 0..23
	PV      int
}

// StatDayPV 是逐日趋势的一行。
type StatDayPV struct {
	Day time.Time // 业务日期
	PV  int
}

// StatFacts 是读取侧一次取回的全部原始计数，整形与补零在 internal/stats。
type StatFacts struct {
	Today StatPVUV // 今日口径
	D7    StatPVUV // 近 7 日口径
	D30   StatPVUV // 近 30 日口径
	// 以下榜单已按命中降序、同分按标识升序取前 topN 行（楼按 building_id、教室按房名再房号、关键词按词面）。
	// Buildings 只含白名单内的楼：白名单外的统计行不参与排行，也不占 Top N 名额。
	Buildings []StatBuildingHit
	Rooms     []StatRoomHit
	Keywords  []StatKeywordHit
	Nodes     []StatNodeHit // 按小节升序；没有命中的小节不出现
	Heatmap   []StatHeatCell
	Days      []StatDayPV // 30 日窗口内出现过的日，按日期升序
}

// ReadStatFacts 读取大屏访问统计所需的全部原始计数。
// 任一查询失败都返回错误：大屏可以整体报错，但不该拿半份数字冒充完整口径。
func ReadStatFacts(ctx context.Context, q statQuerier, windows StatWindows, topN int) (StatFacts, error) {
	// 榜单条数由调用方给定；这里只做防御性夹取，避免 LIMIT 0 或负值静默给出空榜。
	if topN < 1 {
		topN = 1
	}

	facts := StatFacts{
		Buildings: make([]StatBuildingHit, 0),
		Rooms:     make([]StatRoomHit, 0),
		Keywords:  make([]StatKeywordHit, 0),
		Nodes:     make([]StatNodeHit, 0),
		Heatmap:   make([]StatHeatCell, 0),
		Days:      make([]StatDayPV, 0),
	}

	var err error
	if facts.Today, err = statPVUV(ctx, q, windows.Today); err != nil {
		return StatFacts{}, err
	}
	if facts.D7, err = statPVUV(ctx, q, windows.D7); err != nil {
		return StatFacts{}, err
	}
	if facts.D30, err = statPVUV(ctx, q, windows.D30); err != nil {
		return StatFacts{}, err
	}
	if facts.Heatmap, err = statHeatmap(ctx, q, windows.Peak); err != nil {
		return StatFacts{}, err
	}
	if facts.Days, err = statTrend(ctx, q, windows.D30); err != nil {
		return StatFacts{}, err
	}
	// 三张榜单与热力共用 Peak 窗口：切近 7 日还是近 30 日由调用方给的区间决定。
	// 节次分布仍按 D7 取值，不随热力窗口切换。
	if facts.Buildings, err = statBuildingTop(ctx, q, windows.Peak, topN); err != nil {
		return StatFacts{}, err
	}
	if facts.Rooms, err = statRoomTop(ctx, q, windows.Peak, topN); err != nil {
		return StatFacts{}, err
	}
	if facts.Keywords, err = statKeywordTop(ctx, q, windows.Peak, topN); err != nil {
		return StatFacts{}, err
	}
	if facts.Nodes, err = statNodes(ctx, q, windows.D7); err != nil {
		return StatFacts{}, err
	}
	return facts, nil
}

// statPVUV 读一段窗口的 PV 与逐日去重 UV 之和。
func statPVUV(ctx context.Context, q statQuerier, r StatRange) (StatPVUV, error) {
	var pv, uv int64
	if err := q.QueryRow(ctx, statPVUVSQL, r.From, r.To).Scan(&pv, &uv); err != nil {
		return StatPVUV{}, fmt.Errorf("%w: 统计 PV/UV 失败: %w", model.ErrQueryFailed, err)
	}
	return StatPVUV{PV: int(pv), UV: int(uv)}, nil
}

// statHeatmap 读星期 × 小时的 PV。
func statHeatmap(ctx context.Context, q statQuerier, r StatRange) ([]StatHeatCell, error) {
	rows, err := q.Query(ctx, statHeatmapSQL, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询访问热力失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	cells := make([]StatHeatCell, 0)
	for rows.Next() {
		var cell StatHeatCell
		var pv int64
		if err := rows.Scan(&cell.Weekday, &cell.Hour, &pv); err != nil {
			return nil, fmt.Errorf("%w: 扫描访问热力失败: %w", model.ErrQueryFailed, err)
		}
		cell.PV = int(pv)
		cells = append(cells, cell)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历访问热力失败: %w", model.ErrQueryFailed, err)
	}
	return cells, nil
}

// statTrend 读逐日 PV。
func statTrend(ctx context.Context, q statQuerier, r StatRange) ([]StatDayPV, error) {
	rows, err := q.Query(ctx, statTrendSQL, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询查询趋势失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	days := make([]StatDayPV, 0)
	for rows.Next() {
		var day StatDayPV
		var pv int64
		if err := rows.Scan(&day.Day, &pv); err != nil {
			return nil, fmt.Errorf("%w: 扫描查询趋势失败: %w", model.ErrQueryFailed, err)
		}
		day.PV = int(pv)
		days = append(days, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历查询趋势失败: %w", model.ErrQueryFailed, err)
	}
	return days, nil
}

// statBuildingTop 读教学楼 Top 榜，展示名与过滤都取白名单。
// 白名单既是展示名的来源，也是公开榜的可见范围：先把白名单读出来，
// 再让 SQL 按它过滤，未收录的楼不占榜位也不出现在榜上。
func statBuildingTop(ctx context.Context, q statQuerier, r StatRange, topN int) ([]StatBuildingHit, error) {
	names, err := statBuildingNames(ctx, q)
	if err != nil {
		return nil, err
	}
	hits := make([]StatBuildingHit, 0)
	// 白名单为空时公开榜必然为空：不必再查统计表。
	if len(names) == 0 {
		return hits, nil
	}

	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	rows, err := q.Query(ctx, statBuildingTopSQL, r.From, r.To, topN, ids)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询教学楼榜失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	for rows.Next() {
		var hit StatBuildingHit
		var count int64
		if err := rows.Scan(&hit.ID, &count); err != nil {
			return nil, fmt.Errorf("%w: 扫描教学楼榜失败: %w", model.ErrQueryFailed, err)
		}
		hit.Hits = int(count)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历教学楼榜失败: %w", model.ErrQueryFailed, err)
	}
	for i := range hits {
		hits[i].Name = names[hits[i].ID]
	}
	return hits, nil
}

// statBuildingNames 读白名单并折成「楼编号 → 展示名」。
func statBuildingNames(ctx context.Context, q statQuerier) (map[string]string, error) {
	buildings, err := metaBuildingWhitelist(ctx, q)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(buildings))
	for _, building := range buildings {
		names[building.ID] = building.Name
	}
	return names, nil
}

// statRoomTop 读教室 Top 榜；房名由 SQL 从 room 表补出。
func statRoomTop(ctx context.Context, q statQuerier, r StatRange, topN int) ([]StatRoomHit, error) {
	rows, err := q.Query(ctx, statRoomTopSQL, r.From, r.To, topN)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询教室榜失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	hits := make([]StatRoomHit, 0)
	for rows.Next() {
		var hit StatRoomHit
		var count int64
		if err := rows.Scan(&hit.ID, &hit.Name, &count); err != nil {
			return nil, fmt.Errorf("%w: 扫描教室榜失败: %w", model.ErrQueryFailed, err)
		}
		hit.Hits = int(count)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历教室榜失败: %w", model.ErrQueryFailed, err)
	}
	return hits, nil
}

// statKeywordTop 读关键词榜。
func statKeywordTop(ctx context.Context, q statQuerier, r StatRange, topN int) ([]StatKeywordHit, error) {
	rows, err := q.Query(ctx, statKeywordTopSQL, r.From, r.To, topN)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询关键词榜失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	hits := make([]StatKeywordHit, 0)
	for rows.Next() {
		var hit StatKeywordHit
		var count int64
		if err := rows.Scan(&hit.Text, &count); err != nil {
			return nil, fmt.Errorf("%w: 扫描关键词榜失败: %w", model.ErrQueryFailed, err)
		}
		hit.Hits = int(count)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历关键词榜失败: %w", model.ErrQueryFailed, err)
	}
	return hits, nil
}

// statNodes 读节次分布。
func statNodes(ctx context.Context, q statQuerier, r StatRange) ([]StatNodeHit, error) {
	rows, err := q.Query(ctx, statNodeSQL, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询节次分布失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	hits := make([]StatNodeHit, 0)
	for rows.Next() {
		var hit StatNodeHit
		var count int64
		if err := rows.Scan(&hit.Node, &count); err != nil {
			return nil, fmt.Errorf("%w: 扫描节次分布失败: %w", model.ErrQueryFailed, err)
		}
		hit.Hits = int(count)
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历节次分布失败: %w", model.ErrQueryFailed, err)
	}
	return hits, nil
}

// DeleteStatsBefore 删除六张统计日表里早于 cutoff 的记录，返回删除行数。
// 边界按「早于」处理：恰好等于 cutoff 的日保留（Requirement 34.10 的 90 天）。
func DeleteStatsBefore(ctx context.Context, q statQuerier, cutoff time.Time) (int64, error) {
	var deleted int64
	if err := q.QueryRow(ctx, statDeleteSQL, cutoff).Scan(&deleted); err != nil {
		return 0, fmt.Errorf("%w: 清理过期统计失败: %w", model.ErrStatsFailed, err)
	}
	return deleted, nil
}
