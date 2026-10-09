// 本文件属于 business 层：查询参数的解析与校验。
// 契约见 docs/contract/api.v2.md 的参数段；不合法一律返回 ParamError。

package query

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// 查询视图取值：空教室与全天状态。
const (
	// ViewAvailability 是空教室视图：要求区间内每一节都可用
	ViewAvailability = "availability"
	// ViewDay 是全天状态视图：返回 12 小节的逐格状态
	ViewDay = "day"
)

// 契约规定的默认值与取值范围，取自 docs/contract/api.v2.md 的参数段。
const (
	// DefaultStartNode 是省略 start_node 时的起始节次
	DefaultStartNode = "01"
	// DefaultEndNode 是省略 end_node 时的结束节次，契约默认到第 11 节
	DefaultEndNode = "11"
	// DefaultLimit 是省略 limit 时的每页条数
	DefaultLimit = 50
	// MaxKeywordRunes 是 keyword 允许的最大字符数（按字符计，不是字节）
	MaxKeywordRunes = 32
	// MinDateOffset 是最早可查询的日期偏移，0 表示今天
	MinDateOffset = 0
	// MaxDateOffset 是最远可查询的日期偏移，10 表示今天之后第 10 天
	MaxDateOffset = 10
	// MinLimit 是 limit 的下界，0 条没有意义
	MinLimit = 1
	// MaxLimit 是 limit 的上界，防止一次把全库拉走
	MaxLimit = 200
)

// RawParams 是 HTTP 层原样取到的查询串参数，全部为字符串。
type RawParams struct {
	View       string // 视图：availability 或 day
	Keyword    string // 房名关键词，可省略
	DateOffset string // 日期偏移 0..10，省略取 0
	StartNode  string // 起始节次 01..12，仅 availability 使用
	EndNode    string // 结束节次 01..12，仅 availability 使用
	Limit      string // 每页条数，省略取 50
	Offset     string // 分页偏移，省略取 0
}

// Params 是校验之后的查询参数。
type Params struct {
	View       string // 校验后的视图，只可能是 availability 或 day
	Keyword    string // NFKC 并去首尾空白后的关键词
	DateOffset int    // 目标日期相对今天的天数，0..10
	StartNode  string // 区间起始节次，仅 availability 有值
	EndNode    string // 区间结束节次，仅 availability 有值
	NodeCount  int    // 区间内节次个数，空教室判定用；day 视图为 0
	Limit      int    // 每页条数，1..200
	Offset     int    // 分页偏移，从 0 开始
}

// ParamError 表示参数不合法，HTTP 层映射成 40001。
type ParamError struct {
	Message string // 给用户看的原因，直接进响应的 message
}

// Error 实现 error 接口。
func (e ParamError) Error() string {
	return e.Message
}

// ParseParams 校验并补默认值，任何非法取值都直接返回 ParamError。
func ParseParams(raw RawParams) (Params, error) {
	params := Params{
		Keyword:    normalizeKeyword(raw.Keyword),
		DateOffset: MinDateOffset,
		Limit:      DefaultLimit,
		Offset:     0,
	}

	// 视图只有两个合法取值，缺省或写错都算参数错误
	switch raw.View {
	case ViewAvailability, ViewDay:
		params.View = raw.View
	default:
		return Params{}, ParamError{Message: "view 必须是 availability 或 day"}
	}

	// 关键词按字符数限长，前端也按同样口径校验
	if utf8.RuneCountInString(params.Keyword) > MaxKeywordRunes {
		return Params{}, ParamError{Message: fmt.Sprintf("keyword 最长 %d 个字符", MaxKeywordRunes)}
	}

	dateOffset, err := parseInt(raw.DateOffset, MinDateOffset, MaxDateOffset, MinDateOffset)
	if err != nil {
		return Params{}, ParamError{Message: fmt.Sprintf("date_offset 取值 %d..%d", MinDateOffset, MaxDateOffset)}
	}
	params.DateOffset = dateOffset

	// 起止节次只对空教室有意义，全天状态固定返回 12 小节
	if params.View == ViewAvailability {
		start, end, nodeCount, err := parseNodes(raw.StartNode, raw.EndNode)
		if err != nil {
			return Params{}, err
		}
		params.StartNode, params.EndNode, params.NodeCount = start, end, nodeCount
	}

	limit, err := parseInt(raw.Limit, MinLimit, MaxLimit, DefaultLimit)
	if err != nil {
		return Params{}, ParamError{Message: fmt.Sprintf("limit 取值 %d..%d", MinLimit, MaxLimit)}
	}
	offset, err := parseInt(raw.Offset, 0, 1<<30, 0)
	if err != nil {
		return Params{}, ParamError{Message: "offset 不能为负"}
	}
	params.Limit, params.Offset = limit, offset

	return params, nil
}

// normalizeKeyword 按契约做 NFKC 与去首尾空白；ASCII 大小写交给 SQL 的 ILIKE。
func normalizeKeyword(keyword string) string {
	return strings.TrimSpace(norm.NFKC.String(keyword))
}

// parseInt 解析可省略的整数参数：空串取默认值，越界或非数字报错。
func parseInt(text string, minValue, maxValue, defaultValue int) (int, error) {
	// 参数没传时用默认值，保持契约里“省略即默认”的语义
	if strings.TrimSpace(text) == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(text))
	// 非数字或超出范围都视为参数错误
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("取值 %d..%d", minValue, maxValue)
	}
	return value, nil
}

// parseNodes 校验两位节次并算出区间内节次个数。
func parseNodes(rawStart, rawEnd string) (string, string, int, error) {
	start := strings.TrimSpace(rawStart)
	// 省略起止节次时用契约默认的 01 到 11
	if start == "" {
		start = DefaultStartNode
	}
	end := strings.TrimSpace(rawEnd)
	// 只给起点不给终点时，终点仍按默认值处理
	if end == "" {
		end = DefaultEndNode
	}

	// 两端都必须是 01–12 的两位节次
	if !isNode(start) || !isNode(end) {
		return "", "", 0, ParamError{Message: "start_node 与 end_node 必须是 01..12"}
	}
	// 起点晚于终点没有可查询的区间，属于参数错误
	if start > end {
		return "", "", 0, ParamError{Message: "start_node 不能大于 end_node"}
	}

	// 节次按契约是 01–12 连续编号，所以个数可由序号差直接算出
	startNumber, err := strconv.Atoi(start)
	if err != nil {
		return "", "", 0, ParamError{Message: "start_node 必须是 01..12"}
	}
	endNumber, err := strconv.Atoi(end)
	if err != nil {
		return "", "", 0, ParamError{Message: "end_node 必须是 01..12"}
	}
	return start, end, endNumber - startNumber + 1, nil
}

// isNode 判断是否为契约要求的两位节次 01–12。
func isNode(node string) bool {
	// 长度不是两位就不可能是节次，先挡掉
	if len(node) != 2 {
		return false
	}
	// 只接受 01 到 12 这两个数字组合，避免出现 00 或 13
	if node < "01" || node > "12" {
		return false
	}
	return node[0] >= '0' && node[0] <= '9' && node[1] >= '0' && node[1] <= '9'
}
