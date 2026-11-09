package api

// 本文件是管理端 HTTP 的横切层：请求体大小与 Content-Type、会话、CSRF、no-store、错误码映射
// 与日志字段白名单。
// 管理端读端点的顺序固定为 no-store → 会话 → 业务（adminRead）；
// 写端点的顺序固定为 no-store → 体门 → 会话 → CSRF → 业务（adminWrite / adminWriteLimit），
// 没有请求体的写端点（sync）省掉体门，其余门序不变（adminWriteNoBody）：
// 任何一步失败都不进入下一步，因此「缺会话 / 缺 CSRF 时业务写方法调用为 0」由链路结构保证，
// 而不是靠每个 handler 自觉（CP7）。
// 体门的上限是参数：密码类端点 8 KiB、设置端点 64 KiB，两条链只有这一个差异。
// state/init 的处理器在 admin.go、路由在 api.go 注册；登录、退出与改密的处理器在
// admin_session.go，设置读写处理器在 admin_settings.go，轮次读取在 admin_runs.go，
// 手动同步在 admin_sync.go，路由同样在 api.go 注册。
// 这里只提供可组合的中间件与解析入口。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// adminBodyLimit 是管理端写请求体的上限：8 KiB。
	// 管理端提交的是密码与确认密码，8 KiB 足够容纳 256 码点上限且留了编码余量，
	// 同时挡住「用一个超大请求体把解析与哈希拖死」的尝试（design「约束」）。
	adminBodyLimit = 8 << 10
	// adminSettingsBodyLimit 是 PUT /api/admin/settings 的请求体上限：64 KiB。
	// 设置体里最重的是教学楼白名单：200 条、每条几十字节，几 KiB 就到头了，8 KiB
	// 看似够用；但上界要按「合法但很长」的条目留出余量（白名单整体没有字节上界、
	// 单条字段最多 4096 字节），且拼错字段时用户会整段贴进请求体，8 KiB 会把这些请求
	// 误判成体超限。
	// 单独给 64 KiB 而不是放大 adminBodyLimit，是为了不让密码端点跟着放宽（D10）。
	adminSettingsBodyLimit = 64 << 10
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
	codeBadRequest     = 40001 // 请求体不合法：非 JSON、字段缺失、超过请求体上限（8 KiB 或 64 KiB）、Content-Type 不符
	codeSessionInvalid = 40101 // 无会话、会话过期、凭据无效
	codeCSRFInvalid    = 40301 // CSRF 令牌缺失或与会话不一致
	codeInternal       = 50000 // 仓储失败、密钥写入失败、装配缺失
)

// adminStore 是 auth.AdminStore 在本包的别名，不是第二份接口定义。
// 消费侧接口的唯一定义在 internal/auth（design「依赖方向」）：auth 不 import store，
// store.AdminRepo 靠同名方法结构匹配，store 也不反向 import api/auth。
// 保留这个包内别名而不是把签名全改成 auth.AdminStore，是为了让 Server.admin 字段、
// 中间件与测试替身的函数签名保持一致的读法；WithAdmin 的对外签名仍写 auth.AdminStore，
// 两者是同一个类型，不会出现「接口加一个方法、别名没跟上」的漂移。
type adminStore = auth.AdminStore

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
// 它挂在管理端读/写链路的最外层，并且在内层动手之前就把头写好：401/403 是内层中间件直接写出的，
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

// withPragmaNoCache 给初始化端点的全部响应加 Pragma: no-cache。
// 初始化是唯一可能回一次性明文密钥的端点（Requirement 3.6），只靠 Cache-Control: no-store
// 挡不住不认它的老代理与 HTTP/1.0 缓存。这条头挂在路由最外层的包装上，而不是只写在成功路径里：
// 失败限流的 42901、Content-Type 体门的 40001 与 handler 内的 40902/40903/42201/50000
// 都发生在成功响应之前，写进 handler 就会漏掉。其它管理端点不带它：它们的响应里没有密钥。
func withPragmaNoCache(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Pragma", "no-cache")
		next(w, r)
	}
}

// adminRead 组装管理端读端点的中间件链：
//
//	no-store → 会话 → 业务
//
// 读端点不要求 CSRF（没有写入可伪造），但同样必须带 no-store：设置快照里有教务账号、
// OCR 地址与白名单，被缓存下来等于把这些配置留在代理与浏览器里（Requirement 10.2、CP18）。
// 与 adminWrite 共用 withNoStore / withSession，错误响应（40101、50000）也带 no-store。
func (s *Server) adminRead(next http.HandlerFunc) http.HandlerFunc {
	return withNoStore(s.withSession(next))
}

// adminWrite 组装管理端写端点的中间件链：
//
//	no-store → 请求体大小与 Content-Type → 会话 → CSRF → 业务
//
// 顺序写在一处而不是各端点自己拼：CP7 要求「有效会话 → CSRF 一致都在业务体解析之前」，
// CP11 要求每个响应都带 no-store，这两条不该靠每个 handler 各自记得。
// 体门放在会话之前是最便宜的检查：坏 Content-Type 不必查库，也就不占用数据库连接。
// 上限固定 8 KiB，就是 adminWriteLimit(adminBodyLimit, …)：链的定义只留在 adminWriteLimit
// 一处，将来调整门序不必在两个函数里各改一遍。
func (s *Server) adminWrite(next http.HandlerFunc) http.HandlerFunc {
	return s.adminWriteLimit(adminBodyLimit, next)
}

// adminWriteLimit 是带请求体上限的管理端写链路：
//
//	no-store → 体门(limit) → 会话 → CSRF → 业务
//
// 签名与 adminWrite 只差一个 limit：密码端点用 8 KiB，设置端点用 64 KiB，其余门序与
// 语义完全相同（Requirement 6.1、10.1）。limit 只在这里装配，具体哪个体由调用点决定，
// 本函数不猜端点。
func (s *Server) adminWriteLimit(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return withNoStore(s.withJSONBodyLimit(limit, s.withSession(s.withCSRF(next))))
}

// adminWriteNoBody 是「没有请求体」的管理端写链路：
//
//	no-store → 会话 → CSRF → 业务
//
// POST /api/admin/sync 不读请求体、也不要求 Content-Type（Requirement 8.7）：
// 套上 withJSONBodyLimit 会把 `curl -X POST` 这种空体、无 Content-Type 的请求按 40001
// 拒掉，而同步不需要任何输入。除体门之外门序与 adminWrite 完全相同——同步会建轮次，
// 属于写操作，CSRF 不能省；未通过会话的请求连 CSRF 比对都不做（与 adminWrite 一致）。
func (s *Server) adminWriteNoBody(next http.HandlerFunc) http.HandlerFunc {
	return withNoStore(s.withSession(s.withCSRF(next)))
}

// withJSONBody 是写请求体的门：8 KiB 上限 + Content-Type 必须是 application/json。
// 它保留无参形式是因为两条既有链都用 8 KiB：init/session 直接用它，password/logout
// 走 adminWrite → adminWriteLimit(adminBodyLimit)；实现直接转给 withJSONBodyLimit，
// 8 KiB 只是调用点传入的值，不再是写死在本函数里的常量。
func (s *Server) withJSONBody(next http.HandlerFunc) http.HandlerFunc {
	return s.withJSONBodyLimit(adminBodyLimit, next)
}

// withJSONBodyLimit 是参数化后的体门：先装 http.MaxBytesReader（limit），再要求
// Content-Type 是 application/json（design「约束」、Requirement 6.6）。
// 先装读取上限再判头：MaxBytesReader 只约束之后的读取，装在前面才能保证接下来的任何一次
// 读（包括业务 handler 里的解析）都受 limit 约束。Content-Type 不符直接 40001，体一个字节都不读，
// 这同时是挡住「表单型 CSRF」的兜底层。
//
// 超限错误只会在实际读取时浮现，所以文案不能写死在这里的常量上：由
// rejectAdminBody 从 *http.MaxBytesError.Limit 取出真实上限再生成，8 KiB 与 64 KiB
// 两条链各自报各自的数，调用点不必再抄一份上限。
func (s *Server) withJSONBodyLimit(limit int64, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
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
// 文案复用 admin_session.go 的 adminCredentialsInvalidMessage，不在这里写第二份字面量：
// 登录失败与会话失效两条出口只要有一处漂移，探测者就能从文案分辨是哪种情况。
// 文案对所有失败原因一致（Requirement 4.3、5.3），不区分「没登录」与「过期」，
// 以免把会话状态当成可探测的信号。
// 原因类别（reason）只进日志文案，且都是本文件写死的短句：不含 Cookie 值，
// 日志里出现令牌等于把它备份到日志系统（CP2）。
func (s *Server) rejectAdminSession(w http.ResponseWriter, r *http.Request, reason string) {
	http.SetCookie(w, s.adminCookie("", -1))
	s.adminFail(w, r, http.StatusUnauthorized, codeSessionInvalid, "管理会话不可用："+reason, adminCredentialsInvalidMessage)
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

// decodeAdminBody 解析管理端写请求体（8 KiB 链，非严格模式），成功返回 true；
// 失败时已写好 40001 响应。体积上限由体门装的 MaxBytesReader 生效，
// 但超限错误只在读取时浮现，所以只能在这里折成业务码
// （design 错误表把「非法 JSON / 体超限」都映射成 40001）。
// 只允许一个 JSON 值：`{}` 后跟另一个对象或任意垃圾都按非法处理，避免「前半段合法」的
// 请求被接受；尾随空白仍是合法的 JSON 文档，按 EOF 处理。
// 边界：恰好用满上限的体不会触发 MaxBytesError。MaxBytesReader 内部会多读一个字节来判定是否
// 超限；恰好用满上限时这次探测得到 EOF（不会把多余字节交给调用方），超限时返回 *MaxBytesError，
// 所以「正好用满上限」与「超限」本就不会混淆，
// 不需要也不该再按 Content-Length 特判——那样只会留下一条分块传输时判错、平时从不执行的死分支。
func (s *Server) decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	return s.decodeAdminJSON(w, r, dst, false)
}

// decodeAdminBodyStrict 是设置端点的严格解码：除 decodeAdminBody 的全部检查之外，
// 还打开 DisallowUnknownFields（D10）。拼错的键名（`cron_express`）若被静默忽略，
// 管理员会以为保存成功而设置根本没变，这是最难排的一类故障（Requirement 6.2）。
//
// 只给 PUT /api/admin/settings 用，不替换 decodeAdminBody：严格模式会拒绝既有密码端点
// 请求体里将来可能多出来的字段，那是另一份已定的契约，不在本项改动范围内。
// 上限由体门（adminSettingsBodyLimit 装的 MaxBytesReader）决定，超限文案因此报 64 KiB 而不是 8 KiB。
//
// 未定义字段的字段名是从错误文案里切出来的：encoding/json 不为这类错误提供具体类型，
// 只能依赖标准库「json: unknown field 」这个前缀（见 unknownJSONField）。前缀变了最坏
// 退化成通用「请求体不是合法 JSON」文案，业务码仍是 40001，不会升成 500。
// 嵌套对象里的未定义字段只报字段名、没有父级路径（标准库错误本身不带路径），所以
// `{"building_whitelist":[{"name":"x","foo":1}]}` 的文案只说「包含未定义的字段 foo」，
// 不指出它在 building_whitelist 里。这是已知取舍：补齐路径要自己重写解析，不值得。
func (s *Server) decodeAdminBodyStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	return s.decodeAdminJSON(w, r, dst, true)
}

// decodeAdminJSON 是两个解码入口的共同实现：解析一个 JSON 值、拒绝尾随内容，
// 失败一律折成 40001。读取上限由体门的 MaxBytesReader 装好，本函数不接收 limit：
// 超限文案由 rejectAdminBody 从错误本身取上限，调用点因此没有第二份上限可抄。
func (s *Server) decodeAdminJSON(w http.ResponseWriter, r *http.Request, dst any, strict bool) bool {
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
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

// rejectAdminBody 把请求体问题折成 40001：超限、未定义字段与非法 JSON 用不同文案便于
// 前端提示，但都不含提交内容（可能是密码），也不含解析器原文（可能回显输入片段）。
//
// 未定义字段先按 Go 字面量还原（strconv.Unquote）再拼文案：标准库用 %q 包装键名，
// 只裁首尾引号会把 JSON 转义留下的反斜杠一起回显（键名 cron_"express 会显示成 cron_\"express）；
// Unquote 一次完成「去外层引号」与「还原转义」两件事，键名内部本就有引号时也照实回显。
// 还原失败（理论上不可达：%q 的输出总能被 Unquote 解析）回退到原串，业务码仍是 40001。
//
// 超限文案里的数字来自 *http.MaxBytesError.Limit，也就是装进 MaxBytesReader 的那个值：
// 8 KiB 链与 64 KiB 链各自报自己的上限，管理员才知道该删多少，且文案不会与真实门限漂移。
func (s *Server) rejectAdminBody(w http.ResponseWriter, err error) {
	if limit, ok := maxBytesLimit(err); ok {
		s.fail(w, http.StatusBadRequest, codeBadRequest, bodyLimitMessage(limit))
		return
	}
	if field, ok := unknownJSONField(err); ok {
		// 只回字段名，不回提交值：字段名是请求结构里的键，不会是密码内容。
		s.fail(w, http.StatusBadRequest, codeBadRequest, "请求体包含未定义的字段 "+unquoteJSONField(field))
		return
	}
	s.fail(w, http.StatusBadRequest, codeBadRequest, "请求体不是合法 JSON")
}

// bodyLimitMessage 生成请求体超限的固定文案。
//
// 两条管理链的上限都是 KiB 整数（8 KiB、64 KiB），整 KiB 时按 KiB 报数最易读；
// 但这里不假设 limit 一定是整 KiB：整数除法会把 1025 报成「1 KiB」这种错数，
// 所以非整 KiB 的 limit 直接按字节报出，文案里的数字始终等于实际门限。
func bodyLimitMessage(limit int64) string {
	if limit >= 1<<10 && limit%(1<<10) == 0 {
		return fmt.Sprintf("请求体超过 %d KiB", limit/(1<<10))
	}
	return fmt.Sprintf("请求体超过 %d 字节", limit)
}

// unknownJSONField 从 encoding/json 的「未定义字段」错误里取出字段名。
//
// 标准库没有为这类错误提供具体类型（Decode 返回的是 fmt.Errorf("json: unknown field %q", …)），
// 要拿到字段名只能按固定的前缀切；前缀来自标准库的错误文案，Go 大版本升级时可能变，
// 变了也只会退化成「请求体不是合法 JSON」，业务码仍是 40001，不会变成 500。
// 返回值保留标准库 %q 包上的英文双引号与转义，由调用方（unquoteJSONField）还原。
func unknownJSONField(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	const prefix = "json: unknown field "
	message := err.Error()
	if !strings.HasPrefix(message, prefix) {
		return "", false
	}
	return strings.TrimPrefix(message, prefix), true
}

// unquoteJSONField 把标准库 %q 包出来的键名还原成原始字符（去外层引号并还原 \" 等转义）。
//
// 只做字面量还原，不做其它清洗：键名来自请求，对外的 JSON 编码由 writeJSON 负责，
// 多做的任何替换都只会让「提示里的键名」与「请求里的键名」对不上。
// 还原失败回退原串：宁可带一对引号，也不把键名吞成空串。
func unquoteJSONField(field string) string {
	unquoted, err := strconv.Unquote(field)
	if err != nil {
		return field
	}
	return unquoted
}

// maxBytesLimit 取出读取撞上的上限值：用 errors.As 取 *http.MaxBytesError 的 Limit 字段，
// 而不是比较文案（文案随 Go 版本可能变），也不再由调用点手工传一份上限。
// 这样「体门实际装的 limit」只有一处来源：MaxBytesReader 报出来的那个数。
func maxBytesLimit(err error) (int64, bool) {
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		return 0, false
	}
	return maxErr.Limit, true
}
