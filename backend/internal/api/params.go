package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// /api/query 的请求契约边界。所有参数解析与越界判定都集中在本文件，
// handler 只拿 parseQueryRequest 的结果分发，不自己再写半套校验（Requirement 18.1–18.2）。
const (
	// viewAvailability、viewDay 是 view 仅有的两个取值；
	// 空串按 availability 处理，保持 9.3 起前端不传 view 也能查询的既有契约。
	viewAvailability = query.ViewAvailability
	viewDay          = query.ViewDay

	// 日期窗口：今天到第 10 天（含第 10 天），越界即参数错误（Requirement 18.1）。
	minDateOffset = 0
	maxDateOffset = 10

	// 分页：每页 1..200 条，默认 50；偏移从 0 起，不能为负。
	minLimit     = 1
	maxLimit     = 200
	defaultLimit = 50
	minOffset    = 0

	// keyword 按字符而不是字节计长；超长直接报错，不静默截断用户输入。
	maxKeywordRunes = 32

	// room_id 是全天状态视图的必填项，trim 后不能为空；
	// 上界只用来挡住明显异常的超长串，避免把无意义的长串打到存储层。
	maxRoomIDRunes = 32
)

// queryRequest 是一次 /api/query 解析后的结果：view 决定走哪个分支，
// 两个分支的参数各自独立，未被选中的那一组保持零值。
type queryRequest struct {
	view         string                   // 规范化后的视图名，空串已归一为 availability
	availability query.AvailabilityParams // 空教室分支入参
	day          query.DayParams          // 全天状态分支入参
}

// parseQueryRequest 是 /api/query 唯一的参数入口：先定 view 与公共的日期偏移，
// 再交给对应分支补齐其余参数。返回的错误只描述用户可见的参数问题，handler 直接映射成 40001。
func parseQueryRequest(r *http.Request) (queryRequest, error) {
	values := r.URL.Query()

	view := strings.TrimSpace(values.Get("view"))
	switch view {
	case "":
		view = viewAvailability
	case viewAvailability, viewDay:
	default:
		return queryRequest{}, fmt.Errorf("view 只接受 %s 或 %s", viewAvailability, viewDay)
	}

	dateOffset, err := parseDateOffset(values.Get("date_offset"))
	if err != nil {
		return queryRequest{}, err
	}

	if view == viewDay {
		params, err := parseDayParams(values, dateOffset)
		if err != nil {
			return queryRequest{}, err
		}
		return queryRequest{view: view, day: params}, nil
	}

	params, err := parseAvailabilityParams(values, dateOffset)
	if err != nil {
		return queryRequest{}, err
	}
	return queryRequest{view: view, availability: params}, nil
}

// parseDateOffset 校验日期偏移落在今天到第 10 天之间（Requirement 18.1）。
// 服务端不感知「今天是第几节」以外的时钟，只认这个偏移窗口。
func parseDateOffset(raw string) (int, error) {
	offset, err := parseOptionalInt(raw, minDateOffset)
	if err != nil {
		return 0, fmt.Errorf("date_offset 必须是整数")
	}
	if offset < minDateOffset || offset > maxDateOffset {
		return 0, fmt.Errorf("date_offset 取值 %d..%d", minDateOffset, maxDateOffset)
	}
	return offset, nil
}

// parseAvailabilityParams 解析空教室分支的节次区间、关键词、分页与楼筛选。
// 节次区间的默认值（01..11）与小节语义由 query.ParseNodeRange 单点实现，
// 这里只负责把它的错误原样上抛，避免两处各写一套 01..12 判定。
func parseAvailabilityParams(values url.Values, dateOffset int) (query.AvailabilityParams, error) {
	startNode, endNode, nodeCount, err := query.ParseNodeRange(values.Get("start_node"), values.Get("end_node"))
	if err != nil {
		return query.AvailabilityParams{}, err
	}

	keyword := query.NormalizeKeyword(values.Get("keyword"))
	if utf8.RuneCountInString(keyword) > maxKeywordRunes {
		return query.AvailabilityParams{}, fmt.Errorf("keyword 最长 %d 个字符", maxKeywordRunes)
	}

	limit, err := parseOptionalInt(values.Get("limit"), defaultLimit)
	if err != nil || limit < minLimit || limit > maxLimit {
		return query.AvailabilityParams{}, fmt.Errorf("limit 取值 %d..%d", minLimit, maxLimit)
	}

	offset, err := parseOptionalInt(values.Get("offset"), minOffset)
	if err != nil || offset < minOffset {
		return query.AvailabilityParams{}, fmt.Errorf("offset 不能为负")
	}

	return query.AvailabilityParams{
		DateOffset: dateOffset,
		Building:   strings.TrimSpace(values.Get("building")),
		Keyword:    keyword,
		StartNode:  startNode,
		EndNode:    endNode,
		NodeCount:  nodeCount,
		Limit:      limit,
		Offset:     offset,
	}, nil
}

// parseDayParams 解析全天状态分支的必填房号与可选楼筛选。
// 房号 trim 后再判空：只输入空格的请求等价于没填，不能拿它去猜教室。
func parseDayParams(values url.Values, dateOffset int) (query.DayParams, error) {
	roomID := strings.TrimSpace(values.Get("room_id"))
	if roomID == "" {
		return query.DayParams{}, fmt.Errorf("room_id 不能为空")
	}
	if utf8.RuneCountInString(roomID) > maxRoomIDRunes {
		return query.DayParams{}, fmt.Errorf("room_id 最长 %d 个字符", maxRoomIDRunes)
	}

	return query.DayParams{
		DateOffset: dateOffset,
		RoomID:     roomID,
		Building:   strings.TrimSpace(values.Get("building")),
	}, nil
}

// parseOptionalInt 解析可省略的整数参数：空串取默认值，非数字返回错误。
func parseOptionalInt(raw string, defaultValue int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("必须是整数: %w", err)
	}
	return value, nil
}
