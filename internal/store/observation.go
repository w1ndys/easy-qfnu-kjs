// 本文件属于 data 层：空教室与全天状态两条查询路径的 SQL。

package store

import (
	"context"
	"fmt"
	"strings"
)

// RoomBrief 是查询结果里的房间最小信息：jsbh 与展示名。
type RoomBrief struct {
	ID   string // 房间身份 jsbh
	Name string // 规范化展示名
}

// DayCell 是全天状态里的一格：某个房间某个节次的状态键。
type DayCell struct {
	RoomID   string // 房间身份 jsbh
	Name     string // 规范化展示名
	Node     string // 节次编号 '01'..'12'
	StateKey string // 该格的语义状态键
}

// AvailabilityFilter 是空教室查询的入参。
// NodeCount 是区间内应当可用的节次数，用于 HAVING 判定“每一节都可用”。
type AvailabilityFilter struct {
	ReleaseID string // 发布版本 id
	Term      string // 学期
	Week      int    // 教学周次
	Weekday   int    // 星期，1=周一 … 7=周日
	StartNode string // 区间起始节次 01..12
	EndNode   string // 区间结束节次 01..12
	NodeCount int    // 区间内节次个数，用于 HAVING 判定每一节都可用
	Keyword   string // 房名关键词，转义交给 likePattern
	Limit     int    // 每页条数
	Offset    int    // 分页偏移
}

// DayFilter 是全天状态查询的入参。
type DayFilter struct {
	ReleaseID string // 发布版本 id
	Term      string // 学期
	Week      int    // 教学周次
	Weekday   int    // 星期，1=周一 … 7=周日
	Keyword   string // 房名关键词，转义交给 likePattern
	Limit     int    // 每页条数
	Offset    int    // 分页偏移
}

// 两条查询共用的房间过滤条件：同一版本、同一周的同一天、房名命中关键词。
// o.available 与 HAVING 只出现在空教室查询里，所以拆成两段常量。
const roomFilterFrom = `
  FROM observation o
  JOIN room r ON r.room_id = o.room_id
 WHERE o.release_id = $1
   AND o.term = $2
   AND o.week = $3
   AND o.weekday = $4`

// availabilityGroupFrom 是空教室查询的分组段：只扫可用格子，并要求区间内节次数相等。
const availabilityGroupFrom = roomFilterFrom + `
   AND o.node BETWEEN $5 AND $6
   AND o.available
   AND r.name ILIKE $7 ESCAPE '\'
 GROUP BY o.room_id, r.name
HAVING count(*) = $8`

// Availability 返回区间内每一节都可用的房间与命中总数。
func (s *Store) Availability(ctx context.Context, f AvailabilityFilter) (int, []RoomBrief, error) {
	args := []any{f.ReleaseID, f.Term, f.Week, f.Weekday, f.StartNode, f.EndNode, likePattern(f.Keyword), f.NodeCount}

	total, err := s.countRooms(ctx, `SELECT count(*) FROM (SELECT o.room_id`+availabilityGroupFrom+`) AS hit`, args)
	if err != nil {
		return 0, nil, err
	}

	countSQL := `SELECT o.room_id, r.name` + availabilityGroupFrom + `
 ORDER BY r.name
 LIMIT $9 OFFSET $10`
	rows, err := s.pool.Query(ctx, countSQL, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return 0, nil, fmt.Errorf("查询空教室失败: %w", err)
	}
	defer rows.Close()

	rooms := make([]RoomBrief, 0, f.Limit)
	for rows.Next() {
		var room RoomBrief
		if err := rows.Scan(&room.ID, &room.Name); err != nil {
			return 0, nil, fmt.Errorf("扫描空教室失败: %w", err)
		}
		rooms = append(rooms, room)
	}
	// 遍历中途出错时不能当作读完，否则结果会缺房间
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("遍历空教室失败: %w", err)
	}
	return total, rooms, nil
}

// DayStatuses 返回当日每个命中房间的 12 小节状态与房间总数。
func (s *Store) DayStatuses(ctx context.Context, f DayFilter) (int, []DayCell, error) {
	groupFrom := roomFilterFrom + `
   AND r.name ILIKE $5 ESCAPE '\'
 GROUP BY o.room_id, r.name`

	args := []any{f.ReleaseID, f.Term, f.Week, f.Weekday, likePattern(f.Keyword)}

	total, err := s.countRooms(ctx, `SELECT count(*) FROM (SELECT o.room_id`+groupFrom+`) AS hit`, args)
	if err != nil {
		return 0, nil, err
	}

	// 先分页取房间，再回表取这些房间的 12 小节状态，保证分页按房间计算
	cellsSQL := `
WITH page AS (SELECT o.room_id, r.name` + groupFrom + `
        ORDER BY r.name
        LIMIT $6 OFFSET $7)
SELECT p.room_id, p.name, o.node, o.state_key
  FROM page p
  JOIN observation o
    ON o.release_id = $1
   AND o.term = $2
   AND o.week = $3
   AND o.weekday = $4
   AND o.room_id = p.room_id
 ORDER BY p.name, o.node`

	rows, err := s.pool.Query(ctx, cellsSQL, append(args, f.Limit, f.Offset)...)
	if err != nil {
		return 0, nil, fmt.Errorf("查询全天状态失败: %w", err)
	}
	defer rows.Close()

	cells := make([]DayCell, 0, f.Limit*12)
	for rows.Next() {
		var cell DayCell
		if err := rows.Scan(&cell.RoomID, &cell.Name, &cell.Node, &cell.StateKey); err != nil {
			return 0, nil, fmt.Errorf("扫描全天状态失败: %w", err)
		}
		cells = append(cells, cell)
	}
	// 遍历中途出错时不能当作读完，否则某些房间会缺节次
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("遍历全天状态失败: %w", err)
	}
	return total, cells, nil
}

// countRooms 执行一次只返回计数的查询，用于分页的总数。
func (s *Store) countRooms(ctx context.Context, sqlText string, args []any) (int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, sqlText, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("统计房间数失败: %w", err)
	}
	return total, nil
}

// likePattern 把关键词包成子串匹配模式。
// LIKE 的元字符要转义成字面量：契约只允许子串匹配，不允许正则或通配。
func likePattern(keyword string) string {
	// 空关键词等于不过滤，直接给全匹配
	if keyword == "" {
		return "%"
	}
	// 反斜杠必须先转义，否则会把后面补的转义符再转一次
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(keyword)
	return "%" + escaped + "%"
}
