package api

// 本文件实现管理端引导的两个端点（tasklist 第 9 项）：
//   GET  /api/admin/state  前端路由与取回 CSRF 令牌；未登录也返回 200，走公开 60 次/分钟限流
//   POST /api/admin/init   首次初始化：设密码并一次性生成加密密钥，走失败计数限流
//
// 两个端点都不要求会话：state 靠 Cookie 是否命中有效会话自行判定 authenticated，
// init 必须在登录之前就能调用。登录、退出与改密的处理器在 admin_session.go。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
)

// 管理端引导相关业务码（design 错误码表）。40001/40101/40301/42901/50000 已在
// admin_middleware.go、ratelimit.go 里定义，这里只补本项新出现的三个。
const (
	codeAdminExists     = 40902 // 管理员记录已存在，拒绝初始化
	codeKeyExists       = 40903 // 密钥文件已存在（含存在但不可用），拒绝初始化
	codePasswordInvalid = 42201 // 两次密码不一致或长度不合规
)

// keyAlgorithm 是初始化响应里声明的密钥算法。密钥本身固定 32 字节（AES-256），
// 信封格式见 internal/secret；这里只做展示性声明，不参与任何校验。
const keyAlgorithm = "AES-256-GCM"

// adminCompensateTimeout 是补偿删除的独立上限（D10）。
// 补偿发生在「管理员已写入、密钥文件写失败」之后，此时客户端可能早已断开；
// 补偿必须能在请求 ctx 之外独立跑完，但也不能无限等一个卡死的库，所以另给 5 秒。
const adminCompensateTimeout = 5 * time.Second

// WithAdmin 装配管理端的数据来源与密钥管理器；不传时两个管理端点仍注册，
// 按 50000 返回（与 WithMeta 未装配即内部错误的既有做法一致），让 main.go 漏装配立刻暴露。
//
// 设计里的签名是 WithAdmin(auth.AdminStore, *secret.Manager)，但 auth 包目前没有落地
// store.go；api 侧沿用 admin_middleware.go 的自有消费侧接口，store.AdminRepo 结构匹配。
func WithAdmin(store adminStore, keys *secret.Manager) Option {
	return func(s *Server) {
		s.admin = store
		s.keys = keys
	}
}

// adminStateResponse 是 GET /api/admin/state 的 data。
// 未登录时后三项为 null（用指针区分「没有值」与「空字符串」），
// 前端靠它决定进初始化页、登录页还是面板，因此未登录也返回 200 而不是 401。
type adminStateResponse struct {
	Initialized    bool    `json:"initialized"`
	Authenticated  bool    `json:"authenticated"`
	ExpiresAt      *string `json:"expires_at"`
	CSRFToken      *string `json:"csrf_token"`
	KeyFingerprint *string `json:"key_fingerprint"`
}

// adminInitRequest 是 POST /api/admin/init 的请求体。
// 字段用指针是为了把「没提交这个字段」（40001）与「提交了空串」（42201 长度不足）分开，
// 两者的业务含义不同，前端提示也不同。
type adminInitRequest struct {
	Password        *string `json:"password"`
	PasswordConfirm *string `json:"password_confirm"`
}

// adminInitResponse 是初始化成功响应。encryption_key 是明文密钥唯一一次出现的出口，
// 之后只能从 state 拿到指纹（Requirement 3.6）。
type adminInitResponse struct {
	EncryptionKey  string `json:"encryption_key"`
	KeyAlgorithm   string `json:"key_algorithm"`
	KeyFingerprint string `json:"key_fingerprint"`
}

// adminStateLimited 用公开查询的限流器给 state 计数：同一来源 60 次/分钟。
// 复用 s.limiter 而不是新建：设计要求 state「走公开 60 次/分钟限流」，
// 即管理端状态轮询与公开查询共享同一份来源配额。
// 不用 s.rateLimited 是为了让日志走 adminFail 的五字段白名单——那条中间件只记 path，
// 不满足 Requirement 9.1 对管理端日志的字段约束。
func (s *Server) adminStateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.limiter != nil && !s.limiter.allow(clientSource(r, s.trustProxy), s.currentTime()) {
			s.adminFail(w, r, http.StatusTooManyRequests, codeRateLimited, "管理端状态查询触发限流", "请求过于频繁，请稍后重试")
			return
		}
		next(w, r)
	}
}

// adminState 返回引导状态与（登录时）会话信息。
//
// 它不挂在 withSession 下：未登录必须得到 200 + authenticated=false，而不是 401。
// 因此这里自己读 Cookie 并可选地查一次会话，三种结果分别是：
//   - 没有管理员记录：initialized=false，后三项全 null；
//   - 有管理员但 Cookie 缺失/记录不存在/已过期：initialized=true、authenticated=false，后三项全 null；
//   - 会话有效：authenticated=true，回 expires_at、csrf_token 与密钥指纹。
//
// 会话查询失败按 50000 返回而不是降级成「未登录」：那是服务端故障，
// 假装未登录会把运维引向登录页反复试密码。
// 密钥缺失（CP14）只让 key_fingerprint 退化成 null，不影响其他字段。
func (s *Server) adminState(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
		return
	}

	ctx := r.Context()
	_, found, err := s.admin.Admin(ctx)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理员失败", "内部错误")
		return
	}

	resp := adminStateResponse{Initialized: found}
	if found {
		if raw, ok := adminSessionCookie(r); ok {
			session, sessionFound, err := s.admin.Session(ctx, auth.HashSessionToken(raw))
			if err != nil {
				s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理会话失败", "内部错误")
				return
			}
			// 过期判定与写链路同一个纯函数（now < expires_at），不在本端点另立口径。
			if sessionFound && auth.SessionActive(session, s.currentTime()) {
				expires := session.ExpiresAt.UTC().Format(time.RFC3339)
				csrf := session.CSRFToken
				resp.Authenticated = true
				resp.ExpiresAt = &expires
				resp.CSRFToken = &csrf
				resp.KeyFingerprint = s.adminKeyFingerprint()
			}
		}
	}

	s.ok(w, resp)
}

// adminKeyFingerprint 返回当前已装载密钥的指纹；没有可用密钥时返回 nil（CP14）。
// 只读内存，不碰文件系统：state 是高频端点，不能因为密钥路径配置错就让每个请求去 stat。
func (s *Server) adminKeyFingerprint() *string {
	if s.keys == nil || !s.keys.Present() {
		return nil
	}
	key, err := s.keys.Current()
	if err != nil {
		return nil
	}
	fingerprint := key.Fingerprint()
	return &fingerprint
}

// adminInitLimited 是初始化端点的失败计数门：同一来源 60 秒窗口内累计 5 次
// 密码校验失败后，下一次初始化直接 42901，不再查询管理员、也不计算哈希（Requirement 7.6、CP9）。
//
// 只有「密码校验失败（42201）」计入失败，这一点是有意为之：
// 40902/40903 是状态冲突而非可猜的凭据，且并发初始化用例要求 49 个 40902 全部原样返回；
// 40001 是坏请求、50000 是服务端故障，都不该把运维锁在门外。
// 该口径记在这里：init 的失败限流只针对可猜凭据，状态冲突与服务端故障不计数。
func (s *Server) adminInitLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.initLimiter != nil {
			blocked, retryAfter := s.initLimiter.Blocked(clientSource(r, s.trustProxy), s.currentTime())
			if blocked {
				// ceilToSecond 保证剩余时间落在 (0, 窗口] 且不小于 1 秒。
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
				s.adminFail(w, r, http.StatusTooManyRequests, codeRateLimited, "初始化触发失败限流", "初始化失败次数过多，请稍后重试")
				return
			}
		}
		next(w, r)
	}
}

// adminInit 处理首次初始化，顺序固定（D10）：
//
//	Admin() 已存在 → 40902
//	密码校验 → 40001/42201（管理员判定之后才解析请求体，仍不哈希）
//	Reload 密钥文件 → 文件已有可用密钥 40903 / 文件存在但不可用 40903
//	哈希 → CreateAdmin → Create() 独占创建密钥文件
//
// 路由上的门序是 adminInitLimited → withJSONBody → 本处理器：未命中失败限流时，
// 坏 Content-Type 才会走到体门并一律 40001；一旦该来源已被 5 次密码失败锁住，限流门在前，
// 坏 Content-Type 也先返回 42901，不会到达体门。所以「管理员存在时连请求体都不解析」
// 只在「未命中限流且通过体门」之后成立，准确的说法是「除 Content-Type 体与限流门外」。
//
// 顺序的意义是「拒绝先于哈希与写入」，但各道门的代价并不相同：40902 的代价只有一次主键查询，
// 并发初始化时只有赢家会走完哈希与文件写入，输家在最便宜的那道门上就被 40902 挡掉。
// 40903 是例外，代价分两种：文件不可用时是一次 Admin() 查询加一次文件读取（Reload 失败即
// 返回）；文件可用时是两次 Admin() 查询加一次文件读取（第二次用来排除并发赢家，见第 3 步）。
// CreateAdmin 的 created 标志是并发下唯一的裁决点，不能只靠前面的 Admin() 查询。
func (s *Server) adminInit(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
		return
	}
	if s.keys == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "加密密钥管理器未装配", "内部错误")
		return
	}

	ctx := r.Context()

	// 1. 管理员已存在：连请求体都不解析，直接 40902。前端据此切到登录页。
	// 本处理器之外还有两道门，顺序是失败限流 → Content-Type：未命中限流时坏 Content-Type
	// 先以 40001 返回，命中限流则先 42901。这里的「不解析请求体」是在通过这两道门之后而言。
	if _, found, err := s.admin.Admin(ctx); err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理员失败", "内部错误")
		return
	} else if found {
		s.fail(w, http.StatusConflict, codeAdminExists, "管理员已存在，初始化已完成")
		return
	}

	// 2. 密码校验：解析失败与字段缺失映射 40001，规则不符映射 42201。
	var body adminInitRequest
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Password == nil || body.PasswordConfirm == nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, "缺少 password 或 password_confirm")
		return
	}
	if err := auth.ValidatePassword(*body.Password, *body.PasswordConfirm); err != nil {
		s.rejectInitPassword(w, r, err)
		return
	}

	// 3. 密钥对账：磁盘上已有密钥就拒绝且不覆盖（CP4）。这里无条件 Reload 一次，而不是只在
	// Present() 为真时才做——Present 只看内存，两种情况都要求按文件对账：
	//   - 内存里有旧键：运维按「删 admin_user + 删密钥文件」手工重置后重新 init 时，
	//     文件已删 → Reload 清空内存，流程继续；文件还在 → 按原逻辑 40903。
	//   - 内存里没有键：文件可能已经存在（进程刚重启、启动时 Load 未成功）。只信 Present
	//     会让流程一路走到 CreateAdmin，再由 Create 的 O_EXCL 挡下并触发补偿，白建又白删
	//     一条管理员记录。文件可用时装回内存，按 40903 在 CreateAdmin 之前拦下。
	//
	// Reload 只在文件确实不存在时清空内存；文件损坏/不可读时保留旧键并返回 ErrKeyUnavailable，
	// 这里按 40903 拒绝：不建管理员、不补偿，交运维修文件（CP4、CP14）。
	// 首次部署（文件不存在）的代价是这里一次 os.Open 的 ENOENT，之后才允许消耗 CPU 与写文件。
	present, reloadErr := s.keys.Reload()
	if reloadErr != nil {
		// 走 adminFail 而不是 s.fail：坏密钥文件是部署问题，需要留痕；日志只有五字段白名单，
		// 错误原文可能带路径与解析细节，不进日志。
		s.adminFail(w, r, http.StatusConflict, codeKeyExists, "加密密钥文件不可用",
			"加密密钥文件已存在或不可用，请检查密钥文件后再重新初始化")
		return
	}
	if present {
		// 这里要再查一次管理员：并发初始化时，输家可能在上一步查到「没有管理员」之后、
		// 查到「有密钥」之前被调度挂起，等它恢复时赢家已经把记录与密钥都写完了。
		// 此时直接返回 40903 会让 concurrency 契约（恰好一个 200、其余 40902）出现例外；
		// 再查一次主键，确认这次冲突到底是「别人刚初始化完」（40902）还是「只有密钥残留」（40903）。
		if _, found, adminErr := s.admin.Admin(ctx); adminErr != nil {
			s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理员失败", "内部错误")
			return
		} else if found {
			s.fail(w, http.StatusConflict, codeAdminExists, "管理员已存在，初始化已完成")
			return
		}
		s.fail(w, http.StatusConflict, codeKeyExists, "加密密钥已存在，请先移除密钥文件再重新初始化")
		return
	}

	// 4. 到这里才允许消耗 CPU 与写文件。
	hash, err := auth.HashPassword(*body.Password)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "生成密码哈希失败", "内部错误")
		return
	}
	now := s.currentTime()
	if _, created, err := s.admin.CreateAdmin(ctx, hash, now); err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "创建管理员失败", "内部错误")
		return
	} else if !created {
		// 并发下输家走到这里：管理员刚被赢家插进去，按 40902 返回，不触碰密钥文件。
		s.fail(w, http.StatusConflict, codeAdminExists, "管理员已存在，初始化已完成")
		return
	}

	key, err := s.keys.Create()
	if err != nil {
		s.adminInitCompensate(w, r)
		return
	}

	// 成功路径：清掉该来源的失败计数，避免几次输错密码之后一直被挡。
	if s.initLimiter != nil {
		s.initLimiter.Reset(clientSource(r, s.trustProxy))
	}
	// 明文密钥只在这个响应里出现一次：除 no-store 外再加 Pragma，兜住不看 Cache-Control 的老代理。
	w.Header().Set("Pragma", "no-cache")
	s.ok(w, adminInitResponse{
		EncryptionKey:  key.Export(),
		KeyAlgorithm:   keyAlgorithm,
		KeyFingerprint: key.Fingerprint(),
	})
}

// rejectInitPassword 把 auth.ValidatePassword 的哨兵错误映射成 42201 文案，
// 并把这次失败计入来源限流（这是唯一计入失败的初始化拒绝，理由见 adminInitLimited）。
// 文案本身由 passwordRuleMessage 统一给出，与改密共用同一份，避免两处措辞漂移。
func (s *Server) rejectInitPassword(w http.ResponseWriter, r *http.Request, err error) {
	if s.initLimiter != nil {
		s.initLimiter.Fail(clientSource(r, s.trustProxy), s.currentTime())
	}
	s.fail(w, http.StatusUnprocessableEntity, codePasswordInvalid, passwordRuleMessage(err))
}

// passwordRuleMessage 把 auth.ValidatePassword 的哨兵错误翻成 42201 的对外文案。
// 初始化（rejectInitPassword）与改密（adminPassword）共用这一份：两条链路对「哪条规则没过」
// 的措辞必须一致，前端只按文案提示用户改哪一栏。
//
// 这里只做纯映射，副作用留在两个调用点：init 要顺手计入 initLimiter，改密不计数
// （请求已过会话与 CSRF 双校验，见 rejectAdminCurrentPassword 的注释）。为了去重把限流
// 塞进公共函数，会让改密莫名其妙地往初始化限流器里记账。
//
// 文案只描述规则、不回显提交值（可能是密码本身），也不含解析器细节；
// 未知哨兵退化成通用句，不把 err.Error() 透出去。
func passwordRuleMessage(err error) string {
	switch {
	case errors.Is(err, auth.ErrPasswordMismatch):
		return "两次密码不一致"
	case errors.Is(err, auth.ErrPasswordTooShort):
		return "密码长度不足 12 个字符"
	case errors.Is(err, auth.ErrPasswordTooLong):
		return "密码过长"
	default:
		return "密码不符合要求"
	}
}

// adminInitCompensate 处理「管理员记录已写入、密钥文件创建失败」的补偿（D10）：
// 调 DeleteAdmin 把刚建的记录删掉，让运维修好密钥路径后可以重新初始化；响应固定 50000。
//
// 补偿不能跟着客户端走：密钥文件写失败时客户端常常已经超时断开，r.Context() 随即取消，
// 若把这个取消信号带进 DeleteAdmin，补偿必然失败，库里就留下一条没有密钥的管理员记录。
// 所以用 context.WithoutCancel 保留 ctx 里的值、只摘掉取消与截止时间，再配 adminCompensateTimeout
// 兜住真正卡死的库——补偿即使被拖住，也有确定的上限。
//
// 补偿本身失败时记 init_compensation_failed：库里会留下一条没有密钥的管理员记录，
// 运维要按「删 admin_user + 删密钥文件」手工重置。两行日志与响应共用同一个 request_id，
// 方便把「为什么这次初始化 500」串起来；字段严格限于 Requirement 9.1 的五项白名单，
// 底层错误（可能含路径或 SQL 细节）不写进日志。
func (s *Server) adminInitCompensate(w http.ResponseWriter, r *http.Request) {
	id := newRequestID()
	fields := []any{
		"request_id", id,
		"method", r.Method,
		"path", r.URL.Path,
		"status", http.StatusInternalServerError,
		"code", codeInternal,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), adminCompensateTimeout)
	defer cancel()
	if err := s.admin.DeleteAdmin(ctx); err != nil {
		s.log.Error("init_compensation_failed", fields...)
	}
	s.log.Error("初始化写入密钥失败", fields...)
	writeJSON(w, http.StatusInternalServerError, envelope{Code: codeInternal, Message: "内部错误", RequestID: id})
}
