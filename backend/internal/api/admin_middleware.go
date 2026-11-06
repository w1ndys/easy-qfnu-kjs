package api

// 本文件是管理端 HTTP 的横切层：请求体大小与 Content-Type、会话、CSRF、no-store、错误码映射
// 与日志字段白名单。
// 管理端写端点的顺序固定为 no-store → 体门 → 会话 → CSRF → 业务：任何一步失败都不进入下一步，
// 因此「缺会话 / 缺 CSRF 时业务写方法调用为 0」由链路结构保证，而不是靠每个 handler 自觉（CP7）。
// state/init 已在 admin.go 注册；登录、退出、改密由后续任务注册。这里只提供可组合的中间件与解析入口。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// adminBodyLimit 是管理端写请求体的上限：8 KiB。
	// 管理端提交的是密码与确认密码，8 KiB 足够容纳 256 码点上限且留了编码余量，
	// 同时挡住「用一个超大请求体把解析与哈希拖死」的尝试（design「约束」）。
	adminBodyLimit = 8 << 10
	// adminCSRFHeader 是写请求回传 CSRF 同步器令牌的头名（D7、Requirement 6.1）。
	adminCSRFHeader = "X-CSRF-Token"
	// adminCookieName 是管理会话 Cookie 名；eqk_ 前缀与同域其它系统的 Cookie 区分。
	adminCookieName = "eqk_admin_session"
	// adminCookiePath 限定 Cookie 只在管理前缀下发送：公开查询请求不会顺带携带它（Requirement 5.5）。
	adminCookiePath = "/api/admin"
)

// 管理端错误码（design 错误码表）。HTTP 状态表达「类别」，业务码表达「具体原因」，
// 前端只认业务码，所以文案与状态都从这里的常量出发，避免各处写字面量漂移。
const (
	codeBadRequest     = 40001 // 请求体不合法：非 JSON、字段缺失、超过 8 KiB、Content-Type 不符
	codeSessionInvalid = 40101 // 无会话、会话过期、凭据无效
	codeCSRFInvalid    = 40301 // CSRF 令牌缺失或与会话不一致
	codeInternal       = 50000 // 仓储失败、密钥写入失败、装配缺失
)

// adminStore 是管理端在 api 层声明的消费侧接口，口径与设计中的 auth.AdminStore 一致
// （auth 包当前还没有落地 store.go）：
// 接口由使用方定义，store.AdminRepo 靠同名方法结构匹配，store 不反向 import api。
// 这里只列出已实现的端点用到的方法；登录、退出与改密（后续任务）再补 InsertSession 等方法，
// Server.admin 的赋值点不用改，store.AdminRepo 继续匹配。
type adminStore interface {
	Admin(ctx context.Context) (model.AdminUser, bool, error)
	CreateAdmin(ctx context.Context, passwordHash string, now time.Time) (model.AdminUser, bool, error)
	DeleteAdmin(ctx context.Context) error
	Session(ctx context.Context, tokenHash string) (model.AdminSession, bool, error)
}

// adminSessionKey 是请求上下文的键类型；空结构体保证不会与任何其它包的键相撞。
type adminSessionKey struct{}

// adminFail 是管理端「需要留痕的失败」的唯一出口：写失败响应，同时记一条白名单日志。
//
// 日志字段严格限于 Requirement 9.1 与 CP2 的五项：request_id、HTTP 方法、路径、状态码、业务码。
// 密码、Cookie 值、csrf_token、密钥以及 err 原文一律不进日志——err 可能带上输入片段或 SQL 细节，
// 所以「哪一类检查没过」只由 logMsg 这段固定文案表达，具体原因由业务码区分。
//
// 响应与日志共用同一个 request_id：response.go 的 s.fail 每次调用都现生成一个 id，
// 中间件拿不到，抄进日志的 id 就会和响应里那个对不上，排障时反而误导。本项不改 response.go 的 s.fail，
// 所以由管理端链路自己生成一次 id，再用同一个 writeJSON 把信封写出去（形状与 s.fail 完全一致）。
//
// 只用在会话、装配与密钥文件不可用这类需要留痕的失败上；请求体与 CSRF 令牌的客户端错误
// 仍走 s.fail、不记日志，免得每个走错的请求都刷一行日志。
func (s *Server) adminFail(w http.ResponseWriter, r *http.Request, status, code int, logMsg, message string) {
	id := newRequestID()
	// 字段集中构造一次，两个级别共用：将来加字段只改一处，也不会让 4xx 与 5xx 的字段集合漂移。
	args := []any{
		"request_id", id,
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"code", code,
	}
	// 5xx 是服务端故障记 ERROR，4xx 是客户端问题记 INFO：告警只看 ERROR 就不会被无效请求淹没。
	if status >= http.StatusInternalServerError {
		s.log.Error(logMsg, args...)
	} else {
		s.log.Info(logMsg, args...)
	}
	writeJSON(w, status, envelope{Code: code, Message: message, RequestID: id})
}

// withNoStore 给管理端响应加 Cache-Control: no-store（Requirement 9.5、CP11）。
// 它挂在管理端写链路的最外层，并且在内层动手之前就把头写好：401/403 是内层中间件直接写出的，
// 若 no-store 在内层，这些错误响应就会漏掉缓存控制，而登录态与密钥恰恰最不能被缓存。
// 用 Set 而不是 Add：进到这里时已有的 Cache-Control（外层或上游留下的）会被替换成单个 no-store，
// 而不是叠成 `max-age=3600, no-store` 这种自相矛盾的组合。
// 它管不到「下游再改这个头」：本链的内层与业务 handler 都不写缓存头，但管理路由的统一包装
// （GET state、404/405 也要带 no-store）属于 task 9，这里只保证本链成功响应与全部错误响应都带 no-store。
func withNoStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// adminWrite 组装管理端写端点的中间件链：
//
//	no-store → 请求体大小与 Content-Type → 会话 → CSRF → 业务
//
// 顺序写在一处而不是各端点自己拼：CP7 要求「有效会话 → CSRF 一致都在业务体解析之前」，
// CP11 要求每个响应都带 no-store，这两条不该靠每个 handler 各自记得。
// 体门放在会话之前是最便宜的检查：坏 Content-Type 不必查库，也就不占用数据库连接。
func (s *Server) adminWrite(next http.HandlerFunc) http.HandlerFunc {
	return withNoStore(s.withJSONBody(s.withSession(s.withCSRF(next))))
}

// withJSONBody 是写请求体的门：先装 http.MaxBytesReader（8 KiB），再要求 Content-Type
// 是 application/json（design「约束」、Requirement 6.6）。
// 先装读取上限再判头：MaxBytesReader 只约束之后的读取，装在前面才能保证接下来的任何一次
// 读（包括业务 handler 里的解析）都受 8 KiB 约束。Content-Type 不符直接 40001，体一个字节都不读，
// 这同时是挡住「表单型 CSRF」的兜底层。
func (s *Server) withJSONBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, adminBodyLimit)
		if !isJSONMediaType(r.Header.Get("Content-Type")) {
			// 文案固定、不回显提交值：Content-Type 完全由客户端控制，不回写进响应。
			s.fail(w, http.StatusBadRequest, codeBadRequest, "请求体必须是 application/json")
			return
		}
		next(w, r)
	}
}

// isJSONMediaType 判定 Content-Type 是否为 application/json。
// 用 mime.ParseMediaType 而不是字符串前缀比较：`application/json; charset=utf-8`
// 能通过，而 `application/merge-patch+json` 这类近似类型不会因为前缀相同被误放行。
// 缺头、空值、解析失败（例如 `application/json; charset=`）一律按不符合处理。
func isJSONMediaType(raw string) bool {
	if raw == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// withSession 校验管理会话：读 Cookie → 按摘要查 admin_session → 判 SessionActive（Requirement 5.2、CP7）。
// Cookie 缺失或为空时根本不查库：空串的 SHA-256 是个固定值，拿它查库既没有意义，
// 又会让每个无 Cookie 的请求多打一次库。
// 「记录不存在」与「已过期」走同一出口：统一文案让探测者分不清是哪种，并清 Cookie，
// 避免前端拿着死 Cookie 反复重试（design「401 响应同时下发清除 Cookie」）。
// 仓储失败映射 50000 且不清 Cookie：那是服务端故障，不代表这个会话无效。
// 通过后把整条会话放进请求上下文，供 withCSRF 与业务处理器（state 回 csrf_token）使用。
func (s *Server) withSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.admin == nil {
			// 与 WithMeta 未装配同口径：部署缺陷按内部错误暴露，不假装 401 或 404。
			s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
			return
		}
		raw, ok := adminSessionCookie(r)
		if !ok {
			s.rejectAdminSession(w, r, "缺少管理会话 Cookie")
			return
		}
		session, found, err := s.admin.Session(r.Context(), auth.HashSessionToken(raw))
		if err != nil {
			// 只记错误类别（logMsg），不记 err 原文：err 里可能有 SQL 细节（CP2 白名单）。
			s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理会话失败", "内部错误")
			return
		}
		if !found || !auth.SessionActive(session, s.currentTime()) {
			s.rejectAdminSession(w, r, "管理会话不存在或已过期")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminSessionKey{}, session)))
	}
}

// adminSessionCookie 读会话 Cookie；缺失与空值都返回 ok=false。
// 空值是客户端明确发来的「没有令牌」，不能当原文去哈希查库。
func adminSessionCookie(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(adminCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// rejectAdminSession 是 40101 的统一出口：清 Cookie（Max-Age=0）再写固定文案。
// 文案对所有失败原因一致（Requirement 4.3、5.3），不区分「没登录」与「过期」，
// 以免把会话状态当成可探测的信号。
// 原因类别（reason）只进日志文案，且都是本文件写死的短句：不含 Cookie 值，
// 日志里出现令牌等于把它备份到日志系统（CP2）。
func (s *Server) rejectAdminSession(w http.ResponseWriter, r *http.Request, reason string) {
	http.SetCookie(w, s.adminCookie("", -1))
	s.adminFail(w, r, http.StatusUnauthorized, codeSessionInvalid, "管理会话不可用："+reason, "凭据无效或会话已过期")
}

// adminCookie 构造管理会话 Cookie；下发与清除共用同一组属性。
// 清除时 Path / Secure / SameSite 必须与原 Cookie 完全一致，否则浏览器会把删除指令
// 当成另一个 Cookie，真正的死 Cookie 仍会继续被发送。
// HttpOnly 挡住脚本读取，SameSite=Lax 挡住跨站写请求，Secure 由部署配置决定
// （本地 http 开发必须关掉，否则浏览器直接丢弃这个 Cookie）。
// maxAge 为负表示立即删除，Go 会把它渲染成 Set-Cookie 里的 Max-Age=0。
func (s *Server) adminCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     adminCookieName,
		Value:    value,
		Path:     adminCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.adminCookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
}

// withCSRF 比对 X-CSRF-Token 与会话记录里的同步器令牌（Requirement 6.1、6.3、CP7）。
// 它必须挂在 withSession 内层：没有有效会话就无从比对。取不到上下文里的会话说明
// 链路装配错了，按 50000 暴露而不是当成 CSRF 失败——后者会误导前端去重取令牌。
// 失败时保留会话与 Cookie：令牌错是客户端状态问题，会话本身仍然有效（design 错误表）。
// 比对走 auth.CompareCSRFToken 的常量时间实现；期望值不写进响应与日志。
func (s *Server) withCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := adminSessionFrom(r.Context())
		if !ok {
			// 链内不可达（withSession 先放行才进到这里），直接调用 withCSRF 才走得到；
			// 留这条分支是为了让「装配错了」显式暴露成 50000，而不是被 40301 盖住。
			s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "CSRF 校验缺少会话上下文", "内部错误")
			return
		}
		if !auth.CompareCSRFToken(session.CSRFToken, r.Header.Get(adminCSRFHeader)) {
			s.fail(w, http.StatusForbidden, codeCSRFInvalid, "CSRF 令牌缺失或无效")
			return
		}
		next(w, r)
	}
}

// adminSessionFrom 取出 withSession 校验过的会话。
// 业务处理器用它取 csrf_token 与 expires_at（state 端点）；没有会话时返回 false。
func adminSessionFrom(ctx context.Context) (model.AdminSession, bool) {
	session, ok := ctx.Value(adminSessionKey{}).(model.AdminSession)
	return session, ok
}

// decodeAdminBody 解析管理端写请求体，成功返回 true；失败时已写好 40001 响应。
// 体积上限由 withJSONBody 装的 MaxBytesReader 生效，但超限错误只在读取时浮现，
// 所以只能在这里折成业务码（design 错误表把「非法 JSON / 超 8 KiB」都映射成 40001）。
// 只允许一个 JSON 值：`{}` 后跟另一个对象或任意垃圾都按非法处理，避免「前半段合法」的
// 请求被接受；尾随空白仍是合法的 JSON 文档，按 EOF 处理。
// 边界：恰好 8 KiB 的体不会触发 MaxBytesError。MaxBytesReader 内部会多读一个字节来判定是否
// 超限；恰好用满上限时这次探测得到 EOF（不会把多余字节交给调用方），超限时返回 *MaxBytesError，
// 所以「正好用满上限」与「超限」本就不会混淆，
// 不需要也不该再按 Content-Length 特判——那样只会留下一条分块传输时判错、平时从不执行的死分支。
func (s *Server) decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		s.rejectAdminBody(w, err)
		return false
	}
	var extra json.RawMessage
	// err == nil 表示后面还有第二个 JSON 值；只有 EOF 才算「体已经到底，没有多余内容」。
	if err := dec.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		s.rejectAdminBody(w, err)
		return false
	}
	return true
}

// rejectAdminBody 把请求体问题折成 40001：超 8 KiB 与非法 JSON 用不同文案便于前端提示，
// 但都不含提交内容（可能是密码），也不含解析器原文（可能回显输入片段）。
func (s *Server) rejectAdminBody(w http.ResponseWriter, err error) {
	if isMaxBytesError(err) {
		s.fail(w, http.StatusBadRequest, codeBadRequest, "请求体超过 8 KiB")
		return
	}
	s.fail(w, http.StatusBadRequest, codeBadRequest, "请求体不是合法 JSON")
}

// isMaxBytesError 判定读取是否撞上 http.MaxBytesReader 的上限。
// 用 errors.As 而不是比较文案：上限错误带 Limit 字段，文案随 Go 版本可能变。
func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}
