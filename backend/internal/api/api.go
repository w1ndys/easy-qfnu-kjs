// Package api 提供公开与管理 HTTP 接口。
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
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

// DayProvider 提供全天状态查询；由 query 服务实现，测试里可用替身。
type DayProvider interface {
	DayStatus(ctx context.Context, now time.Time, params query.DayParams) (query.DayResult, error)
}

// MetaProvider 提供版本、字典、轴与逐周新鲜度；由 query 服务实现，测试里可用替身。
type MetaProvider interface {
	Meta(ctx context.Context, now time.Time) (query.MetaResult, error)
}

// ScreenProvider 提供数据大屏聚合；由 query 服务实现，测试里可用替身。
type ScreenProvider interface {
	Screen(ctx context.Context, now time.Time, params query.ScreenParams) (query.ScreenResult, error)
}

// Server 持有 API 需要的依赖，公开与管理路由都挂到它上面。
type Server struct {
	log          *slog.Logger         // 结构化日志器
	health       Pinger               // 依赖探活，通常是连接池
	ctxSvc       ContextProvider      // 页面上下文查询
	availability AvailabilityProvider // 空教室查询
	day          DayProvider          // 全天状态查询
	meta         MetaProvider         // 版本与字典等元信息
	screen       ScreenProvider       // 数据大屏聚合
	clock        func() time.Time     // 时间来源，测试可替换
	limiter      *rateLimiter         // 公开查询的按来源限流器
	trustProxy   bool                 // 是否信任前置反代给出的来源头
	recorder     StatsRecorder        // 访问统计记账入口；nil 表示不记账
	admin        adminStore           // 管理端数据来源；nil 表示未装配，管理端点按内部错误处理
	keys         *secret.Manager      // 密钥文件管理器；nil 表示未装配，初始化按内部错误、指纹按 null 处理
	settingsSvc  *settings.Service    // 业务设置读写；nil 表示未装配，设置端点按内部错误处理
	// settingsMu 把一次 PUT 的「Patch → reloadCron」串成一段临界区：两次并发保存若交错
	// 成「A 写库、B 写库、B 重排、A 重排」，进程内的计划会停在 A，而库里已经是 B，
	// 直到下次重启才对得上。GET 不加这把锁：只读快照不需要与保存串行。
	settingsMu sync.Mutex
	// reloadCron 是保存设置后重排调度计划的回调；nil 表示调度器未装配，保存后跳过重排。
	reloadCron func(expr string) error
	// initLimiter 按来源统计初始化密码校验失败；与登录限流分开计数。
	initLimiter *failureLimiter
	// loginLimiter 按来源统计登录凭据失败；与初始化限流分开计数，
	// 一个端点的爆破不会把另一个端点锁住（Requirement 7.1、7.6）。
	loginLimiter *failureLimiter
	// adminCookieSecure 决定管理会话 Cookie 是否带 Secure：默认 true，
	// 只有本地 http 开发才关掉（浏览器不会在 http 上保存 Secure Cookie）。
	// 下发票据与清除 Cookie 必须用同一个值，否则死 Cookie 删不掉。
	adminCookieSecure bool
}

// Option 是 API 层的可选装配项；不传时按默认口径工作：60 次/分钟、信任前置反代。
type Option func(*Server)

// WithTrustedProxy 声明 server 是否部署在可信反代之后。
// 只有为 true 时才读取 X-Forwarded-For / X-Real-IP 作为限流来源；
// 为 false 时只用直连对端地址，供 server 直接暴露给客户端的部署使用。
func WithTrustedProxy(enabled bool) Option {
	return func(s *Server) { s.trustProxy = enabled }
}

// WithMeta 装配 /api/meta 的数据来源；不传时该端点按内部错误处理。
func WithMeta(provider MetaProvider) Option {
	return func(s *Server) { s.meta = provider }
}

// WithScreen 装配 /api/screen 的数据来源；不传时该端点按内部错误处理。
func WithScreen(provider ScreenProvider) Option {
	return func(s *Server) { s.screen = provider }
}

// New 组装 API 依赖；路由在 Handler 里一次性挂好。
func New(log *slog.Logger, health Pinger, ctxSvc ContextProvider, availability AvailabilityProvider, day DayProvider, opts ...Option) *Server {
	s := &Server{
		log:          log,
		health:       health,
		ctxSvc:       ctxSvc,
		availability: availability,
		day:          day,
		clock:        time.Now,
		limiter:      newRateLimiter(rateLimitMaxPerWindow, rateLimitWindow),
		// design D2 的部署拓扑是 web 反代 /api 到 server，默认按可信反代取来源。
		trustProxy: true,
		// 初始化失败限流与登录失败限流各一个实例：两者的失败来源与窗口互不影响。
		initLimiter:  newFailureLimiter(failureLimitMaxFails, failureLimitWindow),
		loginLimiter: newFailureLimiter(failureLimitMaxFails, failureLimitWindow),
		// 安全默认：未显式配置时管理会话 Cookie 带 Secure（ADMIN_COOKIE_SECURE 默认 true）。
		adminCookieSecure: true,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler 返回挂好路由的 mux，测试可直接用 httptest 打它。
// 公开查询统一先过限流再进 handler：命中查询缓存也照样占一次配额。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /api/context", s.rateLimited(s.contextInfo))
	mux.Handle("GET /api/meta", s.rateLimited(s.metaInfo))
	mux.Handle("GET /api/query", s.rateLimited(s.query))
	mux.Handle("GET /api/screen", s.rateLimited(s.screenInfo))

	// 管理端引导：state 是公开读端点（未登录也 200），走公开限流；
	// init 必须在登录之前可用，因此只复用写链路的体门（withJSONBody），不挂会话与 CSRF。
	// init 另包一层 withPragmaNoCache：成功响应里有一次性明文密钥，错误响应（429/400/409/500）
	// 也要带上同一条头，所以这条头必须在 handler 之外、且罩住整条 init 链。
	// 登录（POST session）同样没有会话可校验，挂失败计数限流与体门；
	// 退出（DELETE session）与改密（POST password）走 adminWrite：
	// no-store → 体门 → 会话 → CSRF → 业务。
	// 这些响应都包在可组合的 withNoStore 里，保证连 429/500 的错误响应也带 no-store（CP11）；
	// 404/405 由 ServeMux 自己写出、不经过 handler，由返回前的 withAdminNoStore 按前缀兜住。
	mux.Handle("GET /api/admin/state", withNoStore(s.adminStateLimited(s.adminState)))
	mux.Handle("POST /api/admin/init", withPragmaNoCache(withNoStore(s.adminInitLimited(s.withJSONBody(s.adminInit)))))
	mux.Handle("POST /api/admin/session", withNoStore(s.adminLoginLimited(s.withJSONBody(s.adminLogin))))
	mux.Handle("DELETE /api/admin/session", s.adminWrite(s.adminLogout))
	mux.Handle("POST /api/admin/password", s.adminWrite(s.adminPassword))

	// 业务设置（tasklist 第 9 项）：
	// GET 是管理端读端点，只要会话（no-store → 会话）；
	// PUT 是写端点，链是 no-store → 64 KiB 体门 → 会话 → CSRF，体上限比密码端点的
	// 8 KiB 宽（白名单 200 条 + 展示名），但门序与 adminWrite 完全相同。
	mux.Handle("GET /api/admin/settings", s.adminRead(s.adminSettingsGet))
	mux.Handle("PUT /api/admin/settings", s.adminWriteLimit(adminSettingsBodyLimit, s.adminSettingsPut))
	return withAdminNoStore(mux)
}

// adminRoutePrefix 是管理端路由前缀，与 Cookie 的 adminCookiePath（/api/admin）同一作用域。
const adminRoutePrefix = "/api/admin"

// withAdminNoStore 给管理端前缀下的所有响应加 Cache-Control: no-store。
//
// 内层 withNoStore 只罩住已注册的管理端点，而 ServeMux 自己写出的 404（未注册路径）与
// 405（方法不匹配）根本不经过任何 handler，只在内层加头是覆盖不到的——管理端前缀下的
// 错误响应同样不许被缓存（CP11），所以按前缀在最外层再兜一次。
// 判定接受前缀本身与带斜杠的子树：/api/admin 自身也会 404，且与 Cookie 的 Path=/api/admin
// 一致，避免出现「Cookie 会发到但缓存头不生效」的缺口。
// 前缀之外（公开路径、/healthz）一个头都不加：no-store 只属于管理端。
func withAdminNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == adminRoutePrefix || strings.HasPrefix(r.URL.Path, adminRoutePrefix+"/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
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
