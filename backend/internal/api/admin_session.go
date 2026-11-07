package api

// 本文件实现管理端登录与退出（tasklist 第 10 项）：
//   POST   /api/admin/session  用固定账号 admin 的密码登录，建立 12 小时服务端会话并下发 Cookie
//   DELETE /api/admin/session  退出登录：删除会话记录并清除 Cookie
//
// 登录不挂会话/CSRF 中间件（此时还没有会话），只挂失败计数限流与请求体门；
// 退出走 adminWrite（no-store → 体门 → 会话 → CSRF），与后续的改密同一条写链路。
// 改密（POST /api/admin/password）属于 task 11，不在本文件。

import (
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
	// 这里不用 s.ok 而是自己写信封，是为了让清理失败的 WARN 与成功响应共享同一个
	// request_id：s.ok 内部现生成 id，外面拿不到，抄进日志就会和响应里那个对不上。
	// 信封形状与 s.ok 完全一致（code 0、message success）。
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
	writeJSON(w, http.StatusOK, envelope{
		Code:      0,
		Message:   "success",
		Data:      adminLoginResponse{ExpiresAt: expiresAt.UTC().Format(time.RFC3339), CSRFToken: csrfToken},
		RequestID: requestID,
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
