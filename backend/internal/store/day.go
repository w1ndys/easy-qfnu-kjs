package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// DayFilter 是全天状态查询的入参。
type DayFilter struct {
	Term     string // 学期
	Week     int    // 教学周次
	Weekday  int    // 星期，1=周一 … 7=周日
	RoomID   string // 房间身份 jsbh
	Building string // 教学楼编号；空串表示全部
}

// DayRoom 是全天状态查询命中的房间，归属来自发布版本的归属快照。
type DayRoom struct {
	ID           string // 房间身份 jsbh
	Name         string // 规范化展示名
	BuildingID   string // 归属教学楼编号
	BuildingName string // 归属教学楼展示名
}

// DayNode 是轴上一小节的状态；HasData=false 表示该格没有观测。
type DayNode struct {
	Node      string // 小节编号 01..12
	Block     string // 所属大节，如 030405
	StateKey  string // 字典状态键；无观测时为空
	Label     string // 字典展示名；无观测时为空
	Available bool   // 是否可用，取自观测行
	HasData   bool   // 该格是否存在观测
}

// DayPage 是一次全天状态查询的结果与空态判定标记。
type DayPage struct {
	HasRelease         bool      // 该学期是否存在当前发布版本
	WeekHasObservation bool      // 该周是否有任何观测
	Room               *DayRoom  // nil 表示房间不存在、没有归属快照或被楼筛选排除
	Nodes              []DayNode // 按轴顺序的逐节状态；无版本时为空
}

// dayQuerier 只取全天状态查询需要的两种能力，便于与驱动其余部分解耦。
type dayQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// dayReleaseSQL 取当前发布版本及其字典、轴版本；节点集合必须按该版本的轴解释。
const dayReleaseSQL = `
SELECT release_id, dict_version, axis_version
  FROM release
 WHERE is_current AND term = $1
 LIMIT 1`

// dayRoomSQL 只在发布版本的归属快照里找房间，因此「全部」等价于白名单楼并集，
// 没有归属快照或不属于所选楼的房间都查不到行，调用方据此输出空态。
const dayRoomSQL = `
SELECT r.room_id, r.name, rrb.building_id, rrb.building_name
  FROM room r
  JOIN release_room_building rrb
    ON rrb.release_id = $1
   AND rrb.room_id = r.room_id
 WHERE r.room_id = $2
   AND ($3 = '' OR rrb.building_id = $3)`

// dayNodesSQL 以轴为左表展开 12 小节，缺观测的小节仍返回一行，由 HasData=false 表达。
// 状态名按版本字典投影，不在代码里维护第二套映射。
const dayNodesSQL = `
SELECT an.node, an.block,
       coalesce(o.state_key, ''), coalesce(ds.label, ''),
       coalesce(o.available, false), o.state_key IS NOT NULL
  FROM axis_node an
  LEFT JOIN observation o
    ON o.release_id = $1
   AND o.room_id = $3
   AND o.term = $4
   AND o.week = $5
   AND o.weekday = $6
   AND o.node = an.node
  LEFT JOIN dict_state ds
    ON ds.dict_version = $7
   AND ds.state_key = o.state_key
 WHERE an.axis_version = $2
 ORDER BY an.ordinal`

// DayStatus 返回指定房间在该日的逐节状态。
// 缺失小节保留在结果里（HasData=false），不整条请求报错；无版本、房间不可归属时返回空态标记。
func DayStatus(ctx context.Context, q dayQuerier, f DayFilter) (DayPage, error) {
	page := DayPage{Nodes: make([]DayNode, 0)}

	var releaseID string
	var dictVersion, axisVersion int
	err := q.QueryRow(ctx, dayReleaseSQL, f.Term).Scan(&releaseID, &dictVersion, &axisVersion)
	// 没有当前版本不是错误：调用方据此输出无数据空态。
	if errors.Is(err, pgx.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return DayPage{}, fmt.Errorf("%w: 定位当前发布版本失败: %w", model.ErrQueryFailed, err)
	}

	// 空态判定与空教室共用同一段 SQL：版本存在与否、该周是否有观测的口径必须一致。
	if err := q.QueryRow(ctx, availabilityFlagsSQL, f.Term, f.Week).Scan(
		&page.HasRelease, &page.WeekHasObservation); err != nil {
		return DayPage{}, fmt.Errorf("%w: 判定全天状态空态失败: %w", model.ErrQueryFailed, err)
	}

	var room DayRoom
	err = q.QueryRow(ctx, dayRoomSQL, releaseID, f.RoomID, f.Building).Scan(
		&room.ID, &room.Name, &room.BuildingID, &room.BuildingName)
	// 查不到行覆盖三种情形：房号不存在、没有归属快照、不属于所选楼。
	if errors.Is(err, pgx.ErrNoRows) {
		return page, nil
	}
	if err != nil {
		return DayPage{}, fmt.Errorf("%w: 查询全天状态房间失败: %w", model.ErrQueryFailed, err)
	}
	page.Room = &room

	if err := scanDayNodes(ctx, q, releaseID, axisVersion, dictVersion, f, &page); err != nil {
		return DayPage{}, err
	}
	return page, nil
}

// scanDayNodes 读取轴上的全部小节并扫描成结果；节点数由轴定义决定，不在这里写死 12。
func scanDayNodes(ctx context.Context, q dayQuerier, releaseID string, axisVersion, dictVersion int, f DayFilter, page *DayPage) error {
	rows, err := q.Query(ctx, dayNodesSQL,
		releaseID, axisVersion, f.RoomID, f.Term, f.Week, f.Weekday, dictVersion)
	if err != nil {
		return fmt.Errorf("%w: 查询全天状态失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	for rows.Next() {
		var node DayNode
		if err := rows.Scan(&node.Node, &node.Block, &node.StateKey, &node.Label,
			&node.Available, &node.HasData); err != nil {
			return fmt.Errorf("%w: 扫描全天状态失败: %w", model.ErrQueryFailed, err)
		}
		page.Nodes = append(page.Nodes, node)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: 遍历全天状态失败: %w", model.ErrQueryFailed, err)
	}
	return nil
}
