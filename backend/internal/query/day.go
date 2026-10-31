package query

import (
	"context"
	"fmt"
	"time"
)

// ViewDay 是 /api/query 的全天状态视图。
const ViewDay = "day"

// NodeLabelNoData 是无观测小节的展示文案。
// 它属于查询侧的兜底标签，不登记进版本化字典：字典只描述真实存在的格子状态。
const NodeLabelNoData = "无数据"

// DayParams 是全天状态查询的业务入参。
type DayParams struct {
	DateOffset int    // 目标日期相对今天的天数，业务日期按 Asia/Shanghai 解释
	RoomID     string // 房间身份 jsbh
	Building   string // 教学楼编号；空串表示全部，非空时房间必须归属于该楼
}

// DayNode 是 12 小节里的一节。
// 无观测时 StateKey 为空、HasData=false、Label 为「无数据」，Available 恒为 false。
type DayNode struct {
	Node      string `json:"node"`      // 小节编号 01..12
	Block     string `json:"block"`     // 所属大节，如 030405
	StateKey  string `json:"state_key"` // 字典状态键；无观测时为空
	Label     string `json:"label"`     // 字典展示名；无观测时为「无数据」
	Available bool   `json:"available"` // 是否可用
	HasData   bool   `json:"has_data"`  // 该节是否存在观测
}

// DayResult 是 /api/query?view=day 的 data 载荷。
// 房间字段沿用空教室的命名，避免两套契约；hint 与 availability 共用同一套三档语义。
type DayResult struct {
	RoomID       string            `json:"room_id"`        // 请求指定的房间身份
	Name         string            `json:"name"`           // 房间展示名；房间不可归属时为空
	BuildingID   string            `json:"building_id"`    // 归属教学楼编号；房间不可归属时为空
	BuildingName string            `json:"building_name"`  // 归属教学楼展示名；房间不可归属时为空
	Date         string            `json:"date"`           // 目标业务日期 YYYY-MM-DD
	Term         string            `json:"term"`           // 目标日期所属学期；不在学期周为空
	Week         int               `json:"week"`           // 教学周次；不在学期周为 0
	Weekday      int               `json:"weekday"`        // 星期 1..7；不在学期周为 0
	Nodes        []DayNode         `json:"nodes"`          // 按轴顺序的逐节状态；无版本时为空数组
	Hint         *AvailabilityHint `json:"hint,omitempty"` // 页顶提示，仅空态出现
}

// DayFilter 是查询层向存储层要全天状态时的筛选条件。
type DayFilter struct {
	Term     string // 学期
	Week     int    // 教学周次
	Weekday  int    // 星期 1..7
	RoomID   string // 房间身份 jsbh
	Building string // 教学楼编号；空串表示全部
}

// DayPage 是存储层返回的逐节状态与空态判定所需标记。
type DayPage struct {
	HasRelease         bool      // 该学期是否存在当前发布版本
	WeekHasObservation bool      // 该周是否有任何观测
	Room               *DayRoom  // nil 表示房间不可归属
	Nodes              []DayNode // 按轴顺序的逐节状态
}

// DayRoom 是存储层给出的房间与归属。
type DayRoom struct {
	RoomID       string // 房间身份
	Name         string // 展示名
	BuildingID   string // 归属教学楼编号
	BuildingName string // 归属教学楼展示名
}

// DayLookup 是存储层注入点；用函数类型让 query 不依赖 pgx 与具体仓储实现。
type DayLookup func(ctx context.Context, filter DayFilter) (DayPage, error)

// DayStatus 返回某间教室某一天 12 小节的逐节状态。
// 空态（无版本、学期外、该周无观测、房间不可归属）一律成功返回，由 hint 说明原因。
func (s *Service) DayStatus(ctx context.Context, now time.Time, params DayParams) (DayResult, error) {
	date := targetDate(now, params.DateOffset)
	result := DayResult{
		RoomID: params.RoomID,
		Date:   date.Format(time.DateOnly),
		Nodes:  make([]DayNode, 0),
	}

	tw, ok, err := s.weekLookup(ctx, date)
	if err != nil {
		return DayResult{}, err
	}
	// 不在教学周不是错误：按空态返回，由页顶提示说明原因。
	if !ok || !tw.InCalendar {
		result.Hint = &AvailabilityHint{Code: HintNoData, Message: "该日期没有可查询的数据"}
		return result, nil
	}

	result.Term = tw.Term
	result.Week = int(tw.Week)
	result.Weekday = weekdayOf(date, tw.Monday)

	if s.day == nil {
		return DayResult{}, fmt.Errorf("全天状态查询未装配")
	}

	page, err := s.day(ctx, DayFilter{
		Term:     tw.Term,
		Week:     int(tw.Week),
		Weekday:  result.Weekday,
		RoomID:   params.RoomID,
		Building: params.Building,
	})
	if err != nil {
		return DayResult{}, err
	}

	// 房间不可归属时保留请求房号，其余字段留空：不回填别的楼，也不假装房间存在。
	if page.Room != nil {
		result.Name = page.Room.Name
		result.BuildingID = page.Room.BuildingID
		result.BuildingName = page.Room.BuildingName
	}
	result.Nodes = dayNodes(page.Nodes)
	result.Hint = dayHint(page)
	return result, nil
}

// dayNodes 把存储层的逐节状态整形成对外形状：缺观测的小节标为无数据，不臆造为空闲。
func dayNodes(nodes []DayNode) []DayNode {
	out := make([]DayNode, 0, len(nodes))
	for _, node := range nodes {
		if !node.HasData {
			node.StateKey = ""
			node.Label = NodeLabelNoData
			node.Available = false
		}
		out = append(out, node)
	}
	return out
}

// dayHint 按空态原因选择页顶提示，沿用空教室的三档口径。
// 房间可归属且该周有观测时不给提示：个别小节缺观测由逐节「无数据」表达，不算整页空态。
func dayHint(page DayPage) *AvailabilityHint {
	switch {
	case !page.HasRelease:
		return &AvailabilityHint{Code: HintNoData, Message: "没有已发布的数据"}
	case !page.WeekHasObservation:
		return &AvailabilityHint{Code: HintWeekNoSchedule, Message: "该周疑似无排课"}
	case page.Room == nil:
		return &AvailabilityHint{Code: HintNoMatch, Message: "没有符合条件的教室"}
	default:
		return nil
	}
}
