package api

// 本文件实现管理端的会话管理（tasklist 第 10、11 项）：
//   POST   /api/admin/session   用固定账号 admin 的密码登录，建立 12 小时服务端会话并下发 Cookie
//   DELETE /api/admin/session   退出登录：删除会话记录并清除 Cookie
//   POST   /api/admin/password  改密：校验当前密码 → 写新哈希 → 收回全部会话
//
// 登录不挂会话/CSRF 中间件（此时还没有会话），只挂失败计数限流与请求体门；
// 退出与改密走 adminWrite（no-store → 体门 → 会话 → CSRF），是同一条写链路。

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// adminCredentialsInvalidMessage 是 40101 的唯一文案（Requirement 4.3、5.3、错误表）。
// 登录时「未初始化 / 密码错误 / 哈希串损坏」三种原因共用它，中间件拒绝无效会话也用同一句：
// 对外不可区分，探测者无法从文案判断账号是否初始化、密码是否接近正确。
// 登录失败（本文件的 rejectAdminLogin）与会话失效（admin_middleware.go 的 rejectAdminSession）
// 两条出口引用同一个常量，文案只有这一处定义；admin_session_test.go 仍按两条链路的响应实际文案
// 互相比对，将来谁改成自己的字面量都会让用例失败。
const adminCredentialsInvalidMessage = "凭据无效或会话已过期"

// adminLoginRequest 是 POST /api/admin/session 的请求体。
// 用指针区分「没提交 password」（40001）与「提交了空串」（照常走校验 → 40101）：
// 空串是一个确定的错误密码，不该和缺字段混为一谈。
type adminLoginRequest struct {
	Password *string `json:"password"`
}

// adminLoginResponse 是登录成功响应。Cookie 原文只进 Set-Cookie，响应体里只有
// 过期时刻与 CSRF 同步器令牌（CP5、CP11）。
type adminLoginResponse struct {
	ExpiresAt string `json:"expires_at"`
	CSRFToken string `json:"csrf_token"`
}

// adminLogoutResponse 是退出成功响应；字段集合只有 logged_out（Requirement 9.2）。
type adminLogoutResponse struct {
	LoggedOut bool `json:"logged_out"`
}

// adminPasswordRequest 是 POST /api/admin/password 的请求体。
// 三个字段都用指针：缺字段是 40001（错误表「字段缺失」），提交空串则照常往下走——
// 当前密码空串是一次确定的错误密码（40101），新密码空串是长度不足（42201）。
// 两者混成一种错误会让前端没法区分「没填」与「填错了」，所以按指针区分。
type adminPasswordRequest struct {
	CurrentPassword    *string `json:"current_password"`
	NewPassword        *string `json:"new_password"`
	NewPasswordConfirm *string `json:"new_password_confirm"`
}

// adminPasswordResponse 是改密成功响应。改密会删掉全部会话，所以客户端必须重新登录；
// 字段集合只有 relogin_required（Requirement 8.4、9.2）。
type adminPasswordResponse struct {
	ReloginRequired bool `json:"relogin_required"`
}

// adminLoginLimited 是登录端点的失败计数门：同一来源 60 秒窗口内累计 5 次凭据失败后，
// 下一次登录直接 42901，不再查询管理员、也不计算哈希（Requirement 7.1、7.4、CP9）。
//
// 与 adminInitLimited 是同一套逻辑，但用独立的 loginLimiter：登录与初始化的失败
// 各算各的，一个端点的爆破不该把另一个端点锁住（Requirement 7「各自计数」）。
// 只有 40101 计入失败：40001 是坏请求、50000 是服务端故障，都不代表有人在猜密码。
// 该口径与 init 一致，记在这里；Reject 的记账点在 rejectAdminLogin。
func (s *Server) adminLoginLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.loginLimiter != nil {
			blocked, retryAfter := s.loginLimiter.Blocked(clientSource(r, s.trustProxy), s.currentTime())
			if blocked {
				// ceilToSecond 保证剩余时间落在 (0, 窗口] 且向上取整到整秒，取值不小于 1。
				w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
				s.adminFail(w, r, http.StatusTooManyRequests, codeRateLimited, "登录触发失败限流", "登录失败次数过多，请稍后重试")
				return
			}
		}
		next(w, r)
	}
}

// adminLogin 处理登录：解析体 → 读管理员 → VerifyPassword → 写会话 → 下发 Cookie。
//
// 顺序说明：先解析请求体再查管理员。缺失 password 是 40001（错误表「字段缺失」），
// 与账号是否已初始化无关；而「未初始化」是凭据失败的 40101（Requirement 4.4），
// 只有请求体合法时才走到那一步。与 init 相反（init 先判 40902），因为登录没有
// 「拒绝先于解析」的需求，反而必须读到密码才能校验。
//
// 未初始化与密码错误共用 rejectAdminLogin：文案、清 Cookie、失败计数三项都一致，
// 响应无法区分「库里没有管理员」与「密码不对」（Requirement 4.3、4.4）。
// 哈希串损坏（auth.VerifyPassword 返回 ErrHashMalformed）对外同样是 40101，
// 只把「哪一类检查没过」写进固定的日志文案，错误原文可能带哈希片段，不进日志（CP2）。
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
		return
	}

	var body adminLoginRequest
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Password == nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest, "缺少 password")
		return
	}

	ctx := r.Context()
	user, found, err := s.admin.Admin(ctx)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理员失败", "内部错误")
		return
	}
	if !found {
		// 未初始化：不存在的账号没有可校验的哈希，直接按凭据失败处理，并计入来源限流。
		s.rejectAdminLogin(w, r, "尚未初始化")
		return
	}

	ok, err := auth.VerifyPassword(user.PasswordHash, *body.Password)
	if err != nil {
		// 解析失败发生在 argon2 之前，不消耗哈希预算；响应与普通密码错误完全相同。
		s.rejectAdminLogin(w, r, "密码哈希损坏")
		return
	}
	if !ok {
		s.rejectAdminLogin(w, r, "登录密码错误")
		return
	}

	// 凭据通过之后才生成令牌。原文只下发到 Cookie，库里只留摘要（D5、CP5）。
	now := s.currentTime()
	expiresAt := auth.SessionExpiry(now)
	rawToken, tokenHash, err := auth.NewSessionToken()
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "生成会话令牌失败", "内部错误")
		return
	}
	csrfToken, err := auth.NewCSRFToken()
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "生成 CSRF 令牌失败", "内部错误")
		return
	}

	session := model.AdminSession{
		TokenHash: tokenHash,
		CSRFToken: csrfToken,
		CreatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := s.admin.InsertSession(ctx, session); err != nil {
		// 写库失败不下发 Cookie：否则浏览器会拿着一个服务端不存在的令牌反复 401。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "写入管理会话失败", "内部错误")
		return
	}

	// 登录成功顺手清理过期行（best effort）：不依赖外部定时任务，也不会因为清理失败
	// 让本次已经成功的登录变成 500。日志字段仍限于 Requirement 9.1 的白名单，
	// 不含 err 原文（可能带 SQL 细节）与任何令牌。
	//
	// 先显式取一次 request_id，再交给 writeSuccess：让清理失败的 WARN 与成功响应
	// 共享同一个 id（s.ok 内部现生成 id，外面拿不到，抄进日志就会和响应里那个对不上）。
	// writeSuccess 是成功信封的唯一构造点，s.ok 也走它，所以这里的应答形状与其它
	// 成功响应同源，而不是另抄一份「code 0 + message success」。
	requestID := newRequestID()
	if _, err := s.admin.DeleteExpiredSessions(ctx, now); err != nil {
		s.log.Warn("清理过期管理会话失败",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", http.StatusOK,
			"code", 0,
		)
	}

	// 成功登录清掉该来源的失败计数（Requirement 7.3）：几次输错不该把合法登录挡在门外。
	if s.loginLimiter != nil {
		s.loginLimiter.Reset(clientSource(r, s.trustProxy))
	}

	// Max-Age 取会话总时长（43200 秒），与库里 expires_at 同源，不做滑动续期（D6）。
	http.SetCookie(w, s.adminCookie(rawToken, int(auth.SessionTTL/time.Second)))
	writeSuccess(w, requestID, adminLoginResponse{
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
		CSRFToken: csrfToken,
	})
}

// rejectAdminLogin 是登录失败 40101 的统一出口：计入来源失败限流、清 Cookie、写固定文案。
// 清 Cookie 与错误表的「401 响应同时下发清除 Cookie」一致：浏览器里残留的死令牌
// 不会在这次失败之后继续被发回来。原因类别只进日志文案（固定短句），不含密码与令牌（CP2）。
func (s *Server) rejectAdminLogin(w http.ResponseWriter, r *http.Request, reason string) {
	if s.loginLimiter != nil {
		s.loginLimiter.Fail(clientSource(r, s.trustProxy), s.currentTime())
	}
	http.SetCookie(w, s.adminCookie("", -1))
	s.adminFail(w, r, http.StatusUnauthorized, codeSessionInvalid, "登录失败："+reason, adminCredentialsInvalidMessage)
}

// adminLogout 处理退出：会话与 CSRF 已由 adminWrite 校验，这里只需删记录、清 Cookie。
//
// 删除用上下文里那条会话的摘要，不重新解析 Cookie：withSession 已经把「Cookie → 摘要 →
// 未过期记录」走完，重算一次只会多一次哈希，还可能在校验与删除之间被换掉的 Cookie 骗到。
// 删除失败返回 50000 且不清 Cookie：库里的会话仍然有效，清掉客户端 Cookie 只会让
// 用户以为自己退出了，实际令牌还能用；留给重试或服务恢复。
// 删除成功才下发清除 Cookie（Path/Secure/SameSite 与下发时完全一致，否则删不掉）。
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		// 链内不可达（withSession 先拦），留分支让直接调用时显式暴露成 50000。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
		return
	}
	session, ok := adminSessionFrom(r.Context())
	if !ok {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "退出登录缺少会话上下文", "内部错误")
		return
	}
	if err := s.admin.DeleteSession(r.Context(), session.TokenHash); err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "删除管理会话失败", "内部错误")
		return
	}
	http.SetCookie(w, s.adminCookie("", -1))
	s.ok(w, adminLogoutResponse{LoggedOut: true})
}

// adminPassword 处理改密与全量会话失效（tasklist 第 11 项、Requirement 8）。顺序固定：
//
//	解析体/缺字段 → 40001
//	读管理员 → 校验当前密码 → 40101（不写哈希、不删会话、不清 Cookie）
//	新密码两次一致且 ≥12 码点 → 42201（不写哈希、不删会话、不清 Cookie）
//	哈希 → UpdatePassword → DeleteAllSessions → 清 Cookie + {relogin_required:true}
//
// 当前密码排在前面是有意的：它决定「这次请求有没有资格改密」。当前密码错了就只回 40101，
// 不让「新密码格式不合规」的 42201 盖住真正的问题——否则前端会提示用户去改新密码，
// 而实际要改的是当前密码那一栏。代价是当前密码正确、新密码不合规时要白算一次 argon2；
// 请求已通过会话与 CSRF 双校验，这点开销换「错误原因不串味」是值得的。
//
// 失败口径逐条写清：
//   - 当前密码错误：40101 统一文案，保持哈希与会话不变，也不清 Cookie——Requirement 8.2
//     要求「保持密码哈希与会话不变」，而会话本身确实仍然有效，清 Cookie 等于因为一次
//     手误就把人踢回登录页；这点与 rejectAdminLogin（登录失败清 Cookie）不同。
//   - 哈希串损坏：没有可用的当前密码可比对，对外同样 40101，原因只进日志（Requirement 2.5）。
//   - 新密码不合规：42201，文案只说规则、不回显提交值（错误表「不含提交值」）。
//   - UpdatePassword 失败：50000，哈希没变、会话还在，客户端可以原样重试。
//   - DeleteAllSessions 失败：50000 且不清 Cookie。密码此时已经改了、会话却没收回去，
//     CP8 要求「改密成功后行数为 0」，所以不能假装成功；当前会话仍然有效，用户拿新密码
//     重试一次即可。这里不回滚密码——旧哈希已经不可取回，回滚没有依据。
//
// 成功后旧 Cookie 为什么立即失效：DeleteAllSessions 把 admin_session 清空，任何改密前
// 签发的 Cookie 在下一次 withSession 里都查不到记录，直接 40101（CP8）。
func (s *Server) adminPassword(w http.ResponseWriter, r *http.Request) {
	if s.admin == nil {
		// 链内不可达（withSession 先拦），留分支让直接调用时显式暴露成 50000。
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "管理端数据来源未装配", "内部错误")
		return
	}

	var body adminPasswordRequest
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.CurrentPassword == nil || body.NewPassword == nil || body.NewPasswordConfirm == nil {
		s.fail(w, http.StatusBadRequest, codeBadRequest,
			"缺少 current_password、new_password 或 new_password_confirm")
		return
	}

	ctx := r.Context()
	user, found, err := s.admin.Admin(ctx)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "读取管理员失败", "内部错误")
		return
	}
	if !found {
		// 有效会话却查不到管理员记录（记录被删、初始化被绕过）：没有哈希可校验，按凭据失败处理。
		// 与登录的「尚未初始化」是同一业务码与文案、不同出口：登录走 rejectAdminLogin，会
		// loginLimiter.Fail 并清 Cookie；改密走 rejectAdminCurrentPassword，两者都不做。
		// 文案一致才让外部无法区分这两种情况（Requirement 4.4、8.2）。
		s.rejectAdminCurrentPassword(w, r, "尚未初始化")
		return
	}

	ok, err := auth.VerifyPassword(user.PasswordHash, *body.CurrentPassword)
	if err != nil {
		// 解析失败发生在 argon2 之前；响应与普通密码错误完全相同，原因只进日志。
		s.rejectAdminCurrentPassword(w, r, "密码哈希损坏")
		return
	}
	if !ok {
		s.rejectAdminCurrentPassword(w, r, "当前密码错误")
		return
	}

	// 当前密码通过之后才校验新密码：不会对新密码调用 HashPassword；
	// 因当前密码先校验，仍会消耗一次 VerifyPassword。
	// 规则与初始化完全同一份 auth.ValidatePassword，文案也同一份 passwordRuleMessage，
	// 改密不允许出现更宽的密码，也不允许两处对同一条规则给出不同措辞。
	if err := auth.ValidatePassword(*body.NewPassword, *body.NewPasswordConfirm); err != nil {
		s.fail(w, http.StatusUnprocessableEntity, codePasswordInvalid, passwordRuleMessage(err))
		return
	}

	hash, err := auth.HashPassword(*body.NewPassword)
	if err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "生成密码哈希失败", "内部错误")
		return
	}

	// 时间取注入时钟，与登录/会话同一来源：password_updated_at 必须前移到这次改密的时刻。
	now := s.currentTime()
	if err := s.admin.UpdatePassword(ctx, hash, now); err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "更新管理员密码失败", "内部错误")
		return
	}

	// 新哈希落库之后才收回全部会话：顺序反过来会在「密码没写成、会话却没了」时
	// 把用户锁在门外（还得靠运维重置）。删全部而不是删当前这条，是因为改密后
	// 其它设备上的旧登录同样不该继续有效（Requirement 8.3、CP8）。
	//
	// 这一步不跟客户端的 ctx 走：密码此刻已经写进库，若客户端在响应前断开、r.Context()
	// 被取消，清理就会失败，会话永远收不回去，旧 Cookie 继续可用（CP8 要求改密成功后
	// 行数为 0）。所以和 admin.go 的补偿同一做法——context.WithoutCancel 摘掉取消与
	// 截止时间、保留 ctx 里的值，再配 adminCompensateTimeout 兜住真正卡死的库。
	// 上面几步（读管理员、写新哈希）仍用请求 ctx：它们没做成时本就该随客户端一起放弃。
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), adminCompensateTimeout)
	defer cancel()
	if _, err := s.admin.DeleteAllSessions(revokeCtx); err != nil {
		s.adminFail(w, r, http.StatusInternalServerError, codeInternal, "清理全部管理会话失败", "内部错误")
		return
	}

	// 全部会话已删，客户端手里的 Cookie 已经是一张死票；这里主动清掉，
	// 免得前端拿它反复重试（design「401 响应同时下发清除 Cookie」同一思路）。
	http.SetCookie(w, s.adminCookie("", -1))
	s.ok(w, adminPasswordResponse{ReloginRequired: true})
}

// rejectAdminCurrentPassword 是「当前密码校验失败」的出口：40101 统一文案，且不清 Cookie。
// 与 rejectAdminLogin 的两处差异都是有意的：
//   - 不计入 loginLimiter：这个请求已经通过会话与 CSRF 双校验，不是登录端点；
//     登录限流的口径只覆盖「猜登录密码」，在这里计数只会让合法用户在几次手误后
//     连改密都做不了（Requirement 7 只约束登录与初始化）。
//   - 不清 Cookie：Requirement 8.2 要求「保持密码哈希与会话不变」，会话仍然有效，
//     清掉客户端 Cookie 会因为一次输错就把用户踢回登录页。
//
// 文案复用 adminCredentialsInvalidMessage，不新增第二份字面量；原因类别只进日志文案，
// 且都是本文件写死的短句，不含密码与令牌（CP2）。
func (s *Server) rejectAdminCurrentPassword(w http.ResponseWriter, r *http.Request, reason string) {
	s.adminFail(w, r, http.StatusUnauthorized, codeSessionInvalid, "改密失败："+reason,
		adminCredentialsInvalidMessage)
}
