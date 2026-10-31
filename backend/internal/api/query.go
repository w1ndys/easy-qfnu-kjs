package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// query 按 view 分发 /api/query：缺省与 availability 走空教室，day 走全天状态。
// view 枚举与日期窗口的完整校验留给 9.5。
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	switch view := strings.TrimSpace(r.URL.Query().Get("view")); view {
	case "", query.ViewAvailability:
		s.queryAvailability(w, r)
	case query.ViewDay:
		s.queryDay(w, r)
	default:
		s.fail(w, http.StatusBadRequest, 40001, "参数错误")
	}
}

// queryAvailability 处理 /api/query 的空教室分支。
func (s *Server) queryAvailability(w http.ResponseWriter, r *http.Request) {
	params, err := parseAvailabilityParams(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, 40001, err.Error())
		return
	}

	if s.availability == nil {
		s.log.Error("空教室查询未装配")
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}

	result, err := s.availability.Availability(r.Context(), s.currentTime(), params)
	// 客户端主动断开不是服务端故障：不写响应、不打 ERROR，和 /api/context 一致。
	if errors.Is(err, context.Canceled) {
		s.log.Info("客户端已断开，放弃本次响应")
		return
	}
	// 其余失败只给业务码与通用文案，不把数据库细节透给公开用户。
	if err != nil {
		s.log.Error("空教室查询失败", "err", err)
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}
	s.ok(w, result)
}

// queryDay 处理 /api/query 的全天状态分支：一天一间教室的 12 小节状态。
func (s *Server) queryDay(w http.ResponseWriter, r *http.Request) {
	params, err := parseDayParams(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, 40001, err.Error())
		return
	}

	if s.day == nil {
		s.log.Error("全天状态查询未装配")
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}

	result, err := s.day.DayStatus(r.Context(), s.currentTime(), params)
	// 与空教室一致：客户端断开不写响应、不打 ERROR。
	if errors.Is(err, context.Canceled) {
		s.log.Info("客户端已断开，放弃本次响应")
		return
	}
	if err != nil {
		s.log.Error("全天状态查询失败", "err", err)
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}
	s.ok(w, result)
}

// parseDayParams 只解析全天状态需要的参数；日期窗口与 view 枚举的完整校验由 9.5 接管。
func parseDayParams(r *http.Request) (query.DayParams, error) {
	values := r.URL.Query()

	dateOffset, err := parseOptionalInt(values.Get("date_offset"), 0)
	if err != nil {
		return query.DayParams{}, fmt.Errorf("date_offset 必须是整数")
	}

	roomID := strings.TrimSpace(values.Get("room_id"))
	// 全天状态必须指向一间确定的教室；缺房间标识时无法返回任何有意义的格子。
	if roomID == "" {
		return query.DayParams{}, fmt.Errorf("room_id 不能为空")
	}

	return query.DayParams{
		DateOffset: dateOffset,
		RoomID:     roomID,
		Building:   strings.TrimSpace(values.Get("building")),
	}, nil
}

// parseAvailabilityParams 做本任务需要的最小参数解析；越界规则由 9.5 统一接管。
func parseAvailabilityParams(r *http.Request) (query.AvailabilityParams, error) {
	values := r.URL.Query()

	dateOffset, err := parseOptionalInt(values.Get("date_offset"), 0)
	if err != nil {
		return query.AvailabilityParams{}, fmt.Errorf("date_offset 必须是整数")
	}

	startNode, endNode, nodeCount, err := query.ParseNodeRange(values.Get("start_node"), values.Get("end_node"))
	if err != nil {
		return query.AvailabilityParams{}, err
	}

	keyword := query.NormalizeKeyword(values.Get("keyword"))
	// 关键词过长不截断，避免把用户输入悄悄改掉；长度规则在 9.5 统一成错误码。
	if utf8.RuneCountInString(keyword) > query.MaxKeywordRunes {
		return query.AvailabilityParams{}, fmt.Errorf("keyword 最长 %d 个字符", query.MaxKeywordRunes)
	}

	limit, err := parseOptionalInt(values.Get("limit"), query.DefaultLimit)
	if err != nil || limit < 1 || limit > query.MaxLimit {
		return query.AvailabilityParams{}, fmt.Errorf("limit 取值 1..%d", query.MaxLimit)
	}

	offset, err := parseOptionalInt(values.Get("offset"), 0)
	if err != nil || offset < 0 {
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
