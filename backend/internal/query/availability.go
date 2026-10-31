package query

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 节次区间的契约。请求侧的分页、关键词等边界统一在 api 层定义，
// 这里只保留查询自身需要的视图名与小节默认值。
const (
	// ViewAvailability 是 /api/query 的空教室视图。
	ViewAvailability = "availability"
	// DefaultStartNode 是省略 start_node 时的起始节次，沿用既有契约的 01。
	DefaultStartNode = "01"
	// DefaultEndNode 是省略 end_node 时的结束节次，沿用既有契约的 11。
	DefaultEndNode = "11"
)

// 页顶提示的稳定标记。前端按 code 映射文案，message 只作兜底与排障。
const (
	// HintNoData 表示没有当前版本或日期不在可查询的学期周。
	HintNoData = "no_data"
	// HintWeekNoSchedule 与 Requirement 18.3 共用：该周疑似无排课。
	HintWeekNoSchedule = "week_no_schedule"
	// HintNoMatch 表示该周有观测，但没有房间满足区间、楼与关键词筛选。
	HintNoMatch = "no_match"
)

// AvailabilityParams 是空教室查询的业务入参。
// 起止节次与 NodeCount 已由调用方校验并成组传入，避免查询层重复解析同一段区间。
type AvailabilityParams struct {
	DateOffset int    // 目标日期相对今天的天数，业务日期按 Asia/Shanghai 解释
	Building   string // 教学楼编号；空串表示全部
	Keyword    string // 房名关键词，查询层会做全半角归一
	StartNode  string // 区间起始小节，01..12
	EndNode    string // 区间结束小节，01..12
	NodeCount  int    // 区间内小节个数，用于判定每一节都有观测且可用
	Limit      int    // 每页条数
	Offset     int    // 分页偏移
}

// AvailabilityRoom 是空教室结果里的一间房。
type AvailabilityRoom struct {
	RoomID       string `json:"room_id"`       // 房间身份 jsbh
	Name         string `json:"name"`          // 规范化展示名
	BuildingID   string `json:"building_id"`   // 归属教学楼编号
	BuildingName string `json:"building_name"` // 归属教学楼展示名
}

// AvailabilityHint 是页顶提示标记；与 Requirement 18.3 的「该周疑似无排课」共用同一套语义。
type AvailabilityHint struct {
	Code    string `json:"code"`    // 稳定标记，前端据此选文案
	Message string `json:"message"` // 兜底文案
}

// AvailabilityResult 是 /api/query?view=availability 的 data 载荷。
type AvailabilityResult struct {
	Total   int                `json:"total"`   // 分页前的命中总数
	Items   []AvailabilityRoom `json:"items"`   // 当前页房间
	Date    string             `json:"date"`    // 目标业务日期 YYYY-MM-DD
	Term    string             `json:"term"`    // 目标日期所属学期；不在学期周为空
	Week    int                `json:"week"`    // 教学周次；不在学期周为 0
	Weekday int                `json:"weekday"` // 星期 1..7；不在学期周为 0
	Hint    *AvailabilityHint  `json:"hint,omitempty"`
}

// AvailabilityFilter 是查询层向存储层要空教室结果时的筛选条件。
type AvailabilityFilter struct {
	Term      string // 学期
	Week      int    // 教学周次
	Weekday   int    // 星期 1..7
	StartNode string // 区间起始小节
	EndNode   string // 区间结束小节
	NodeCount int    // 区间内小节个数
	Building  string // 教学楼编号；空串表示全部
	Keyword   string // 已归一化的房名关键词
	Limit     int    // 每页条数
	Offset    int    // 分页偏移
}

// AvailabilityPage 是存储层返回的一页空教室结果与空态判定所需标记。
type AvailabilityPage struct {
	Total              int                // 分页前命中总数
	Rooms              []AvailabilityRoom // 当前页房间
	HasRelease         bool               // 该学期是否存在当前发布版本
	WeekHasObservation bool               // 该周是否有任何观测，用于区分「疑似无排课」与「筛选无命中」
}

// AvailabilityLookup 是存储层注入点；用函数类型让 query 不依赖 pgx 与具体仓储实现。
type AvailabilityLookup func(ctx context.Context, filter AvailabilityFilter) (AvailabilityPage, error)

// NormalizeKeyword 做全半角归一与首尾空白裁剪。
// 只处理与房名相关的全角 ASCII（U+FF01..U+FF5E）与表意空格，避免在查询层引入新的外部依赖。
func NormalizeKeyword(raw string) string {
	return strings.TrimSpace(normalizeWidth(raw))
}

// normalizeWidth 把全角 ASCII 映射回 ASCII，并把表意空格映射为普通空格。
func normalizeWidth(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\u3000':
			b.WriteByte(' ')
		case r >= '\uFF01' && r <= '\uFF5E':
			b.WriteRune(r - 0xFEE0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ParseNodeRange 解析并校验小节区间，省略时用默认的 01..11。
// 这是 01..12 与起点不晚于终点的单点实现；返回的错误由 api 层统一映射成 40001。
func ParseNodeRange(startRaw, endRaw string) (string, string, int, error) {
	start := strings.TrimSpace(startRaw)
	// 省略起点时沿用既有契约默认的 01。
	if start == "" {
		start = DefaultStartNode
	}
	end := strings.TrimSpace(endRaw)
	// 省略终点时沿用既有契约默认的 11。
	if end == "" {
		end = DefaultEndNode
	}

	startNumber, ok := nodeNumber(start)
	if !ok {
		return "", "", 0, fmt.Errorf("start_node 必须是 01..12")
	}
	endNumber, ok := nodeNumber(end)
	if !ok {
		return "", "", 0, fmt.Errorf("end_node 必须是 01..12")
	}
	if startNumber > endNumber {
		return "", "", 0, fmt.Errorf("start_node 不能大于 end_node")
	}
	return start, end, endNumber - startNumber + 1, nil
}

// nodeNumber 把两位小节编号转成序号；00、13 与任何非数字都判非法。
func nodeNumber(node string) (int, bool) {
	if len(node) != 2 {
		return 0, false
	}
	if node[0] < '0' || node[0] > '9' || node[1] < '0' || node[1] > '9' {
		return 0, false
	}
	number := int(node[0]-'0')*10 + int(node[1]-'0')
	if number < 1 || number > 12 {
		return 0, false
	}
	return number, true
}

// Availability 返回某天某区间内每一节都可用、且落在筛选范围内的教室。
func (s *Service) Availability(ctx context.Context, now time.Time, params AvailabilityParams) (AvailabilityResult, error) {
	date := targetDate(now, params.DateOffset)
	result := AvailabilityResult{Date: date.Format(time.DateOnly), Items: make([]AvailabilityRoom, 0)}

	tw, ok, err := s.weekLookup(ctx, date)
	if err != nil {
		return AvailabilityResult{}, err
	}
	// 不在教学周不是错误：按空态返回，由页顶提示说明原因。
	if !ok || !tw.InCalendar {
		result.Hint = &AvailabilityHint{Code: HintNoData, Message: "该日期没有可查询的数据"}
		return result, nil
	}

	result.Term = tw.Term
	result.Week = int(tw.Week)
	result.Weekday = weekdayOf(date, tw.Monday)

	if s.availability == nil {
		return AvailabilityResult{}, fmt.Errorf("空教室查询未装配")
	}

	page, err := s.availability(ctx, AvailabilityFilter{
		Term:      tw.Term,
		Week:      int(tw.Week),
		Weekday:   result.Weekday,
		StartNode: params.StartNode,
		EndNode:   params.EndNode,
		NodeCount: params.NodeCount,
		Building:  params.Building,
		Keyword:   NormalizeKeyword(params.Keyword),
		Limit:     params.Limit,
		Offset:    params.Offset,
	})
	if err != nil {
		return AvailabilityResult{}, err
	}

	result.Total = page.Total
	for _, room := range page.Rooms {
		result.Items = append(result.Items, AvailabilityRoom{
			RoomID:       room.RoomID,
			Name:         room.Name,
			BuildingID:   room.BuildingID,
			BuildingName: room.BuildingName,
		})
	}
	// 命中总数为 0 才是空态：分页越界时 total 仍大于 0，不应提示「无排课」。
	if page.Total == 0 {
		result.Hint = availabilityHint(page)
	}
	return result, nil
}

// targetDate 把业务日期归一到上海时区的 00:00，再加日期偏移。
func targetDate(now time.Time, offset int) time.Time {
	local := now.In(shanghai)
	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)
	return date.AddDate(0, 0, offset)
}

// availabilityHint 按空态原因选择页顶提示；三者共用同一套 hint 结构。
func availabilityHint(page AvailabilityPage) *AvailabilityHint {
	switch {
	case !page.HasRelease:
		return &AvailabilityHint{Code: HintNoData, Message: "没有已发布的数据"}
	case !page.WeekHasObservation:
		return &AvailabilityHint{Code: HintWeekNoSchedule, Message: "该周疑似无排课"}
	default:
		return &AvailabilityHint{Code: HintNoMatch, Message: "没有符合条件的教室"}
	}
}
