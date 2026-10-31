// Package api 提供公开与管理 HTTP 接口。
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// Pinger 是就绪检查唯一需要的能力；声明在这里，避免 API 层直接依赖数据库驱动。
type Pinger interface {
	Ping(ctx context.Context) error
}

// ContextProvider 提供页面上下文；由 query 服务实现，测试里可用替身。
type ContextProvider interface {
	Context(ctx context.Context, now time.Time) (query.ContextInfo, error)
}

// AvailabilityProvider 提供空教室查询；由 query 服务实现，测试里可用替身。
type AvailabilityProvider interface {
	Availability(ctx context.Context, now time.Time, params query.AvailabilityParams) (query.AvailabilityResult, error)
}

// Server 持有 API 需要的依赖，公开与管理路由都挂到它上面。
type Server struct {
	log          *slog.Logger         // 结构化日志器
	health       Pinger               // 依赖探活，通常是连接池
	ctxSvc       ContextProvider      // 页面上下文查询
	availability AvailabilityProvider // 空教室查询
	clock        func() time.Time     // 时间来源，测试可替换
}

// New 组装 API 依赖；路由在 Handler 里一次性挂好。
func New(log *slog.Logger, health Pinger, ctxSvc ContextProvider, availability AvailabilityProvider) *Server {
	return &Server{
		log:          log,
		health:       health,
		ctxSvc:       ctxSvc,
		availability: availability,
		clock:        time.Now,
	}
}

// Handler 返回挂好路由的 mux，测试可直接用 httptest 打它。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /api/context", s.contextInfo)
	mux.HandleFunc("GET /api/query", s.queryAvailability)
	return mux
}

// healthz 只表示进程活着，不查依赖：把库检查放这里会让库抖动连带重启进程。
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyz 表示能接流量：数据库可达才给 200，否则 503 让编排器摘掉这个实例。
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	// 探针自身也要有上限，否则库挂死时探针跟着挂死，编排器只看到超时看不到原因。
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.health.Ping(ctx); err != nil {
		s.log.Warn("就绪检查失败", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("unavailable"))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}

// contextInfo 返回页面上下文：业务日期、学期、周次、星期与是否教学周。
func (s *Server) contextInfo(w http.ResponseWriter, r *http.Request) {
	info, err := s.ctxSvc.Context(r.Context(), s.currentTime())
	// 客户端主动断开不是服务端故障：记一条 info 就返回，既不算 500 也不打 ERROR。
	if errors.Is(err, context.Canceled) {
		s.log.Info("客户端已断开，放弃本次响应")
		return
	}
	// 其余失败属于内部错误：只给业务码与通用文案，不把内部细节透给公开用户。
	if err != nil {
		s.log.Error("页面上下文查询失败", "err", err)
		s.fail(w, http.StatusInternalServerError, 50000, "内部错误")
		return
	}
	s.ok(w, info)
}
