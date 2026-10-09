// 本文件属于 entry 层：三个端点返回的 JSON 结构。
// 字段名与 docs/contract/api.v2.md 的示例逐项对应。

package api

import "github.com/w1ndys/easy-qfnu-kjs/internal/query"

// contextBody 是 /api/context 的数据载荷。
type contextBody struct {
	Date       string `json:"date"`        // 目标日期，YYYY-MM-DD
	Term       string `json:"term"`        // 学期标识，不在教学周为空串
	Week       int    `json:"week"`        // 教学周次，不在教学周为 0
	Weekday    int    `json:"weekday"`     // 星期，1=周一 … 7=周日
	InCalendar bool   `json:"in_calendar"` // 是否落在教学周历内
}

// availabilityItem 是空教室结果里的一间房。
type availabilityItem struct {
	ID   string `json:"id"`   // 房间身份 jsbh
	Name string `json:"name"` // 规范化展示名
}

// dayItem 是全天状态结果里的一间房与它的 12 小节状态。
type dayItem struct {
	ID       string            `json:"id"`       // 房间身份 jsbh
	Name     string            `json:"name"`     // 规范化展示名
	Statuses map[string]string `json:"statuses"` // 节次 '01'..'12' → 状态语义键
}

// queryBody 是 /api/query 的数据载荷，items 随视图变化。
type queryBody struct {
	Total     int    `json:"total"`       // 命中的房间总数
	Items     any    `json:"items"`       // 结果列表，形状随视图变化
	Date      string `json:"date"`        // 目标日期，YYYY-MM-DD
	Week      int    `json:"week"`        // 教学周次，不在教学周为 0
	DayOfWeek int    `json:"day_of_week"` // 星期，1=周一 … 7=周日
}

// metaState 是字典里一个状态的说明。
type metaState struct {
	Label     string `json:"label"`     // 状态中文名
	Available bool   `json:"available"` // 是否算可用
}

// metaNode 是节次轴上的一个节点。
type metaNode struct {
	Code  string `json:"code"`  // 节次编号 '01'..'12'
	Block string `json:"block"` // 所属大节编码
}

// metaWeek 是逐周信息。
type metaWeek struct {
	Week          int     `json:"week"`            // 教学周次
	Monday        string  `json:"monday"`          // 该周周一，YYYY-MM-DD
	Published     bool    `json:"published"`       // 当前版本里是否有这一周的观测
	LastSuccessAt *string `json:"last_success_at"` // 最近成功发布时间，RFC3339；从未发布为 null
	Stale         bool    `json:"stale"`           // 是否过期
}

// metaTerm 是一个学期及其周次。
type metaTerm struct {
	Term       string     `json:"term"`        // 学期标识
	TotalWeeks int        `json:"total_weeks"` // 总周数
	Weeks      []metaWeek `json:"weeks"`       // 逐周信息
}

// metaAxis 是节次轴：大节顺序与 12 个节点。
type metaAxis struct {
	Blocks []string   `json:"blocks"` // 大节编码顺序
	Nodes  []metaNode `json:"nodes"`  // 12 个节次节点
}

// metaBody 是 /api/meta 的数据载荷。
type metaBody struct {
	ReleaseID   string               `json:"release_id"`   // 当前发布版本 id
	GeneratedAt string               `json:"generated_at"` // 发布时间，RFC3339
	DictVersion int                  `json:"dict_version"` // 字典版本
	AxisVersion int                  `json:"axis_version"` // 节次轴版本
	States      map[string]metaState `json:"states"`       // 状态键 → 状态说明
	Axis        metaAxis             `json:"axis"`         // 节次轴
	Terms       []metaTerm           `json:"terms"`        // 学期与逐周新鲜度
}

// metaBody 把业务层的 meta 结果转成响应结构。
func (s *Server) metaBody(result query.MetaResult) metaBody {
	states := make(map[string]metaState, len(result.States))
	for key, state := range result.States {
		states[key] = metaState{Label: state.Label, Available: state.Available}
	}

	nodes := make([]metaNode, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		nodes = append(nodes, metaNode{Code: node.Code, Block: node.Block})
	}

	terms := make([]metaTerm, 0, len(result.Terms))
	for _, term := range result.Terms {
		weeks := make([]metaWeek, 0, len(term.Weeks))
		for _, week := range term.Weeks {
			var lastSuccess *string
			// 有成功发布时间才输出时间串，没有就保持 null
			if week.LastSuccessAt != nil {
				formatted := s.formatTime(*week.LastSuccessAt)
				lastSuccess = &formatted
			}
			weeks = append(weeks, metaWeek{
				Week:          week.Week,
				Monday:        s.formatDate(week.Monday),
				Published:     week.Published,
				LastSuccessAt: lastSuccess,
				Stale:         week.Stale,
			})
		}
		terms = append(terms, metaTerm{Term: term.Term, TotalWeeks: term.TotalWeeks, Weeks: weeks})
	}

	return metaBody{
		ReleaseID:   result.ReleaseID,
		GeneratedAt: s.formatTime(result.GeneratedAt),
		DictVersion: result.DictVersion,
		AxisVersion: result.AxisVersion,
		States:      states,
		Axis:        metaAxis{Blocks: result.Blocks, Nodes: nodes},
		Terms:       terms,
	}
}
