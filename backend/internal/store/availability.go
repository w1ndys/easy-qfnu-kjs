package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// AvailabilityFilter 是空教室查询的入参。
// NodeCount 是区间内应存在的观测节次数，用于 HAVING 判定「每一节都有观测且可用」。
type AvailabilityFilter struct {
	Term      string // 学期
	Week      int    // 教学周次
	Weekday   int    // 星期，1=周一 … 7=周日
	StartNode string // 区间起始小节 01..12
	EndNode   string // 区间结束小节 01..12
	NodeCount int    // 区间内小节个数
	Building  string // 教学楼编号；空串表示全部
	Keyword   string // 已归一化的房名关键词；转义由 likePattern 负责
	Limit     int    // 每页条数
	Offset    int    // 分页偏移
}

// AvailabilityRoom 是空教室结果里的一间房，归属来自发布版本的归属快照。
type AvailabilityRoom struct {
	ID           string // 房间身份 jsbh
	Name         string // 规范化展示名
	BuildingID   string // 归属教学楼编号
	BuildingName string // 归属教学楼展示名
}

// AvailabilityPage 是一次空教室查询的结果与空态判定标记。
type AvailabilityPage struct {
	Total              int                // 分页前命中总数
	Rooms              []AvailabilityRoom // 当前页房间
	HasRelease         bool               // 该学期是否存在当前发布版本
	WeekHasObservation bool               // 该周是否有任何观测
}

// availabilityQuerier 只取空教室查询需要的两种能力，便于与驱动其余部分解耦。
type availabilityQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// availabilityFrom 是计数与分页共用的筛选段：
// 只读当前版本的观测与归属快照，并且只保留区间内每一节都有观测且可用的房间。
// release_room_building 只包含发布时白名单楼的归属，因此空 Building 等价于「白名单楼并集」。
// 缺任一节的观测会让 count(*) 小于 NodeCount，整间房被 HAVING 排除（情形 A）。
const availabilityFrom = `
  FROM observation o
  JOIN cur ON cur.release_id = o.release_id
  JOIN room r ON r.room_id = o.room_id
  JOIN release_room_building rrb
    ON rrb.release_id = o.release_id
   AND rrb.room_id = o.room_id
 WHERE o.term = $1
   AND o.week = $2
   AND o.weekday = $3
   AND o.node::text BETWEEN $4 AND $5
   AND o.available
   AND ($6 = '' OR rrb.building_id = $6)
   AND r.name ILIKE $7 ESCAPE '\'
 GROUP BY o.room_id, r.name, rrb.building_id, rrb.building_name
HAVING count(*) = $8`

// availabilityCountSQL 统计分页前的命中房间数。
const availabilityCountSQL = `
WITH cur AS (SELECT release_id FROM release WHERE is_current AND term = $1 LIMIT 1)
SELECT count(*) FROM (SELECT 1` + availabilityFrom + `) AS hit`

// availabilityPageSQL 按「房名升序、同房名按房号升序」取当前页。
const availabilityPageSQL = `
WITH cur AS (SELECT release_id FROM release WHERE is_current AND term = $1 LIMIT 1)
SELECT o.room_id, r.name, rrb.building_id, rrb.building_name` + availabilityFrom + `
 ORDER BY r.name, o.room_id
 LIMIT $9 OFFSET $10`

// availabilityFlagsSQL 只在空结果时执行：区分「没有当前版本」「该周无观测」与「筛选无命中」。
const availabilityFlagsSQL = `
SELECT
  EXISTS (SELECT 1 FROM release WHERE is_current AND term = $1),
  EXISTS (
    SELECT 1
      FROM observation o
      JOIN release rel
        ON rel.release_id = o.release_id
       AND rel.is_current
       AND rel.term = $1
     WHERE o.term = $1 AND o.week = $2
  )`

// Availability 返回区间内每一节都有观测且可用的房间，以及分页前的总数。
func Availability(ctx context.Context, q availabilityQuerier, f AvailabilityFilter) (AvailabilityPage, error) {
	args := []any{
		f.Term, f.Week, f.Weekday, f.StartNode, f.EndNode,
		f.Building, likePattern(f.Keyword), f.NodeCount,
	}

	var total int
	if err := q.QueryRow(ctx, availabilityCountSQL, args...).Scan(&total); err != nil {
		return AvailabilityPage{}, fmt.Errorf("%w: 统计空教室失败: %w", model.ErrQueryFailed, err)
	}

	page := AvailabilityPage{Total: total, Rooms: make([]AvailabilityRoom, 0)}
	if total > 0 {
		// 分页查询另有 LIMIT/OFFSET 两个参数，复制一份避免复用底层数组。
		pageArgs := append(append([]any{}, args...), f.Limit, f.Offset)
		if err := scanAvailabilityRooms(ctx, q, pageArgs, &page); err != nil {
			return AvailabilityPage{}, err
		}
		page.HasRelease = true
		page.WeekHasObservation = true
		return page, nil
	}

	if err := q.QueryRow(ctx, availabilityFlagsSQL, f.Term, f.Week).Scan(
		&page.HasRelease, &page.WeekHasObservation); err != nil {
		return AvailabilityPage{}, fmt.Errorf("%w: 判定空教室空态失败: %w", model.ErrQueryFailed, err)
	}
	return page, nil
}

// scanAvailabilityRooms 扫描分页结果；total 已由计数查询给出，这里只填房间。
func scanAvailabilityRooms(ctx context.Context, q availabilityQuerier, args []any, page *AvailabilityPage) error {
	rows, err := q.Query(ctx, availabilityPageSQL, args...)
	if err != nil {
		return fmt.Errorf("%w: 查询空教室失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	for rows.Next() {
		var room AvailabilityRoom
		if err := rows.Scan(&room.ID, &room.Name, &room.BuildingID, &room.BuildingName); err != nil {
			return fmt.Errorf("%w: 扫描空教室失败: %w", model.ErrQueryFailed, err)
		}
		page.Rooms = append(page.Rooms, room)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: 遍历空教室失败: %w", model.ErrQueryFailed, err)
	}
	return nil
}

// likePattern 把关键词包成子串匹配模式，并把 LIKE 元字符转成字面量。
// 契约只允许子串匹配，不允许通配符或正则。
func likePattern(keyword string) string {
	// 空关键词等于不过滤，直接给全匹配。
	if keyword == "" {
		return "%"
	}
	// 一次扫描完成转义：替换出的反斜杠不会被再次处理，不会发生二次转义。
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyword)
	return "%" + escaped + "%"
}
