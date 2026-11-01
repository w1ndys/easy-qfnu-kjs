package api

import (
	"net/http"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// 访问统计的匿名标识入口（Requirement 34.4）。
// 前端把本地随机标识放在请求头里最省事；同时接受同名查询参数，
// 方便不想加头的调用方。两者都没有时按缺失处理，由记账侧退回 IP 摘要，不报错。
const (
	visitorIDHeader = "X-Visitor-Id"
	visitorIDParam  = "visitor_id"
)

// StatsRecorder 是访问统计的记账入口。
// 实现必须是「投递后立即返回」：它由查询 handler 在响应写出之后同步调用（Requirement 34.1、CP18）。
type StatsRecorder interface {
	Record(visit stats.Visit)
}

// WithStats 装配访问统计记账；不传时查询不记账，9.2–9.7 的响应完全不变。
func WithStats(recorder StatsRecorder) Option {
	return func(s *Server) { s.recorder = recorder }
}

// recordAvailability 记录一次空教室查询：维度取请求里的楼、关键词与节次区间。
// 记账发生在响应写出之后，任何统计问题都不改变响应与状态码（CP18）。
func (s *Server) recordAvailability(r *http.Request, params query.AvailabilityParams) {
	if s.recorder == nil {
		return
	}
	visit := s.baseVisit(r)
	visit.Building = params.Building
	visit.Keyword = params.Keyword
	visit.StartNode = params.StartNode
	visit.EndNode = params.EndNode
	s.recorder.Record(visit)
}

// recordDay 记录一次全天状态查询：维度取请求里的楼与房号。
func (s *Server) recordDay(r *http.Request, params query.DayParams) {
	if s.recorder == nil {
		return
	}
	visit := s.baseVisit(r)
	visit.Building = params.Building
	visit.RoomID = params.RoomID
	s.recorder.Record(visit)
}

// baseVisit 填上每次查询都有的匿名标识与来源 IP。
// IP 只用于按天加盐的摘要，既不落库也不进日志（Requirement 34.9、CP16）。
func (s *Server) baseVisit(r *http.Request) stats.Visit {
	return stats.Visit{
		VisitorID: visitorID(r),
		IP:        clientSource(r, s.trustProxy),
	}
}

// visitorID 取前端回传的随机匿名标识：请求头优先，查询参数兜底。
// 不做格式校验：标识由前端生成，任何稳定字符串都能用于当日去重，长度上限在记账侧统一夹取。
func visitorID(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get(visitorIDHeader))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get(visitorIDParam))
	}
	return raw
}
