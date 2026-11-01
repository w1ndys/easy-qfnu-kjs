package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// query 按 view 分发 /api/query：缺省与 availability 走空教室，day 走全天状态。
// 参数契约全部由 parseQueryRequest 一处判定，这里只做分发与错误码映射。
func (s *Server) query(w http.ResponseWriter, r *http.Request) {
	req, err := parseQueryRequest(r)
	if err != nil {
		s.fail(w, http.StatusBadRequest, 40001, err.Error())
		return
	}

	if req.view == viewDay {
		s.queryDay(w, r, req.day)
		return
	}
	s.queryAvailability(w, r, req.availability)
}

// queryAvailability 处理 /api/query 的空教室分支。
func (s *Server) queryAvailability(w http.ResponseWriter, r *http.Request, params query.AvailabilityParams) {
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
	// 记账在响应写出之后投递：查询先返回，统计失败只影响统计（Requirement 34.1、CP18）。
	s.recordAvailability(r, params)
}

// queryDay 处理 /api/query 的全天状态分支：一天一间教室的 12 小节状态。
func (s *Server) queryDay(w http.ResponseWriter, r *http.Request, params query.DayParams) {
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
	// 与空教室一致：响应先返回，记账失败不改状态码与响应体（CP18）。
	s.recordDay(r, params)
}
