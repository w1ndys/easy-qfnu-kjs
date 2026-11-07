package api

// 本文件覆盖 tasklist 第 10 项：POST /api/admin/session（登录）与 DELETE /api/admin/session（退出）。
// 用例走真实路由（srv.Handler()），复用 admin_test.go 的 adminRuntimeStore 与固定时钟 testAdminNow：
// 失败限流、体门、Cookie 属性与 no-store 都由真实实现承担，替身只负责数据来源并记录会话读写。
// 哈希是否真的发生用 auth.HashCallCount 观察（CP9），这与 init 用例同一口径。
//
// 两条链路形状不同，用例的分组也跟着分：
//   - 登录时还没有会话可校验，链序是 adminLoginLimited → withJSONBody → adminLogin；
//   - 退出挂在 adminWrite 上（no-store → 体门 → 会话 → CSRF → 业务），因此即便没有请求体，
//     也要求 Content-Type: application/json，否则先得到 40001。

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// loginSource 是登录用例的固定来源。默认 trustProxy 为真，clientSource 取 X-Forwarded-For
// 最右一跳，于是失败计数落在一个确定的桶里；断言限流状态时也用同一个字符串复查。
const loginSource = "203.0.113.7"

// initializedAdminStore 返回「已初始化」的替身：admin_user 里有一条能校验通过的哈希。
// 它自己会消耗一次 argon2 计算，断言哈希次数的用例必须在取到它之后再读 HashCallCount。
func initializedAdminStore(t *testing.T) *adminRuntimeStore {
	t.Helper()
	hash, err := auth.HashPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("准备管理员密码哈希失败: %v", err)
	}
	store := newAdminRuntimeStore()
	store.userFound = true
	store.user = model.AdminUser{
		ID:                1,
		PasswordHash:      hash,
		CreatedAt:         testAdminNow,
		PasswordUpdatedAt: testAdminNow,
	}
	return store
}

// storeWithMalformedHash 返回「哈希串损坏」的替身：VerifyPassword 在解析阶段就失败，
// 不消耗 argon2 预算，所以这类凭据失败既能计入限流、又不会让 HashCallCount 增长。
func storeWithMalformedHash() *adminRuntimeStore {
	store := newAdminRuntimeStore()
	store.userFound = true
	store.user.PasswordHash = "not-a-phc-string"
	return store
}

// loginJSON 序列化登录请求体；password 为 nil 表示请求体里没有这个字段。
func loginJSON(t *testing.T, password *string) string {
	t.Helper()
	payload := map[string]*string{}
	if password != nil {
		payload["password"] = password
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造登录请求体失败: %v", err)
	}
	return string(raw)
}

// newLoginRequest 构造登录请求：Content-Type 合法，来源固定为 loginSource。
func newLoginRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/session", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", loginSource)
	return req
}

// loginSessionCookie 取出登录响应下发的管理会话 Cookie。
func loginSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == adminCookieName {
			return cookie
		}
	}
	t.Fatalf("缺少会话 Cookie 的 Set-Cookie，实际头: %v", rec.Header())
	return nil
}

// newInitRequestFromLoginSource 与 admin_test.go 的 newInitRequest 相同，但把来源固定成 loginSource。
// 交叉用例要看「同一来源在另一个端点上是否被连带锁住」：限流按来源分桶，
// 来源不同的话，就算两个端点真的共用同一个限流器也测不出来。
func newInitRequestFromLoginSource(body string) *http.Request {
	req := newInitRequest(body)
	req.Header.Set("X-Forwarded-For", loginSource)
	return req
}

// newLogoutRequest 构造退出请求。退出挂在 adminWrite 上，体门（withJSONBody）无条件要求
// application/json——即使这个端点没有请求体（Requirement 6.6）。这也是前端调用
// DELETE /api/admin/session 时必须带上的头，缺了会先得到 40001 而不是执行退出。
// cookie 为 nil 表示未登录，csrf 为空表示不带 X-CSRF-Token。
func newLogoutRequest(cookie *http.Cookie, csrf string) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/session", nil)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(adminCSRFHeader, csrf)
	}
	return req
}

// ---------------------------------------------------------------------------
// POST /api/admin/session
// ---------------------------------------------------------------------------

// TestAdminLoginSuccess 覆盖登录成功：200 + expires_at/csrf_token、会话行按令牌摘要落库、
// Cookie 属性齐全（HttpOnly / SameSite=Lax / Path=/api/admin / Secure 随配置 / Max-Age=43200），
// 并顺手清理过期会话（Requirement 5.1、5.5、9.5、CP5、CP11）。
// 密钥管理器刻意不装配：登录不依赖密钥文件，缺键也要能登录（CP14）。
func TestAdminLoginSuccess(t *testing.T) {
	cases := []struct {
		name   string
		secure bool
	}{
		{"默认下发 Secure", true},
		{"本地 http 关闭 Secure", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := initializedAdminStore(t)
			srv := newAdminTestServer(t, store, nil)
			srv.adminCookieSecure = tc.secure

			rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))

			data := contractSuccessData(t, rec)
			assertContractKeys(t, "登录 data", data, "expires_at", "csrf_token")
			assertNoStore(t, rec)

			// expires_at 取注入时钟 + 12 小时，RFC3339 UTC，不做滑动续期（D6）。
			wantExpiresAt := auth.SessionExpiry(testAdminNow)
			wantExpires := wantExpiresAt.UTC().Format(time.RFC3339)
			if data["expires_at"] != wantExpires {
				t.Fatalf("expires_at 期望 %q，实际 %#v", wantExpires, data["expires_at"])
			}
			csrf, _ := data["csrf_token"].(string)
			if len(csrf) != 43 {
				t.Fatalf("csrf_token 期望 43 字符 base64url，实际 %q（%d 字符）", csrf, len(csrf))
			}

			cookie := loginSessionCookie(t, rec)
			if cookie.Value == "" {
				t.Fatal("登录成功必须下发非空的会话令牌")
			}
			if !cookie.HttpOnly {
				t.Fatal("会话 Cookie 必须 HttpOnly，否则脚本能读走令牌")
			}
			if cookie.Path != adminCookiePath {
				t.Fatalf("Path 期望 %s，实际 %q", adminCookiePath, cookie.Path)
			}
			if cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("SameSite 期望 Lax，实际 %v", cookie.SameSite)
			}
			if cookie.Secure != tc.secure {
				t.Fatalf("Secure 期望 %t，实际 %t", tc.secure, cookie.Secure)
			}
			// 43200 = 12 小时，与库里 expires_at 同源；这里刻意写死数值，钉住线上口径。
			if cookie.MaxAge != 43200 {
				t.Fatalf("Max-Age 期望 43200，实际 %d", cookie.MaxAge)
			}
			if header := rec.Header().Get("Set-Cookie"); !strings.Contains(header, "Max-Age=43200") {
				t.Fatalf("Set-Cookie 原文应带 Max-Age=43200，实际 %q", header)
			}

			// 会话行：主键是 Cookie 原文的 SHA-256，库里不出现原文（CP5）。
			calls, inserted := store.snapshotInsert()
			if calls != 1 {
				t.Fatalf("InsertSession 期望 1 次，实际 %d 次", calls)
			}
			wantHash := auth.HashSessionToken(cookie.Value)
			if inserted.TokenHash != wantHash {
				t.Fatalf("token_hash 期望 Cookie 原文的 SHA-256 %q，实际 %q", wantHash, inserted.TokenHash)
			}
			if inserted.TokenHash == cookie.Value {
				t.Fatal("库里不能存可直接使用的令牌原文（CP5）")
			}
			if !store.hasSession(wantHash) {
				t.Fatal("登录成功应写入 admin_session 行")
			}
			if inserted.CSRFToken != csrf {
				t.Fatalf("会话里的 csrf_token 应与响应一致：库里 %q，响应 %q", inserted.CSRFToken, csrf)
			}
			if !inserted.CreatedAt.Equal(testAdminNow) || !inserted.ExpiresAt.Equal(wantExpiresAt) {
				t.Fatalf("会话时间列期望 created=%s、expires=%s，实际 %+v",
					testAdminNow, wantExpiresAt, inserted)
			}
			if strings.Contains(rec.Body.String(), cookie.Value) {
				t.Fatal("响应体不能回显会话令牌原文（CP5）")
			}

			// 成功路径顺手清理过期行：恰好一次，时钟取注入时刻，失败也不影响本次登录。
			if expireCalls, now := store.snapshotExpire(); expireCalls != 1 || !now.Equal(testAdminNow) {
				t.Fatalf("DeleteExpiredSessions 期望 1 次且时钟 %s，实际 %d 次 / %s",
					testAdminNow, expireCalls, now)
			}
		})
	}
}

// TestAdminLoginRejectsInvalidCredentials 覆盖「未初始化 / 密码错误 / 哈希串损坏」三类凭据失败：
// 一律 401 + 40101 + 同一句文案、下发清除 Cookie、不写会话行，并且都计入来源失败限流
// （Requirement 4.3、4.4、5.3、7.1、CP9）。
// 哈希次数按原因区分：只有密码错误真的进了 argon2，未初始化与损坏串在计算之前就返回。
func TestAdminLoginRejectsInvalidCredentials(t *testing.T) {
	cases := []struct {
		name            string
		store           func(t *testing.T) *adminRuntimeStore
		wantHashPerFail int64
	}{
		{"未初始化", func(*testing.T) *adminRuntimeStore { return newAdminRuntimeStore() }, 0},
		{"密码错误", initializedAdminStore, 1},
		{"哈希串损坏", func(*testing.T) *adminRuntimeStore { return storeWithMalformedHash() }, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			srv := newAdminTestServer(t, store, nil)

			hashBefore := auth.HashCallCount()
			for i := 0; i < failureLimitMaxFails; i++ {
				rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))

				failure := decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
				if failure.Message != adminCredentialsInvalidMessage {
					t.Fatalf("第 %d 次失败文案期望 %q，实际 %q",
						i+1, adminCredentialsInvalidMessage, failure.Message)
				}
				assertNoStore(t, rec)
				// 401 同时清 Cookie：浏览器里残留的死令牌不该在失败后继续被发回来。
				assertClearedCookie(t, rec, true)
			}

			if delta := auth.HashCallCount() - hashBefore; delta != tc.wantHashPerFail*int64(failureLimitMaxFails) {
				t.Fatalf("argon2 计算期望 %d 次，实际 %d 次",
					tc.wantHashPerFail*int64(failureLimitMaxFails), delta)
			}
			if calls, _ := store.snapshotInsert(); calls != 0 {
				t.Fatalf("凭据失败不该写会话行，实际 InsertSession %d 次", calls)
			}

			// 5 次失败之后该来源已被拦到窗口结束（Requirement 7.1、7.4）。
			blocked, retryAfter := srv.loginLimiter.Blocked(loginSource, testAdminNow)
			if !blocked {
				t.Fatalf("%d 次凭据失败后来源应被限流", failureLimitMaxFails)
			}
			if retryAfter <= 0 || retryAfter > failureLimitWindow {
				t.Fatalf("窗口剩余时间期望落在 (0, %s]，实际 %s", failureLimitWindow, retryAfter)
			}
		})
	}
}

// TestAdminCredentialFailureMessageMatchesMiddleware 钉住两条链路的文案一致：
// admin_session.go 的 rejectAdminLogin 与 admin_middleware.go 的 rejectAdminSession
// 现在共用 adminCredentialsInvalidMessage 常量，但这里仍按两条链路的响应实际文案互相比对，
// 而不是直接比较常量——将来谁在中间件里改回自己的字面量，用例照样失败。
// Requirement 4.3/5.3 要求「登录凭据无效」与「会话无效」对外不可区分，
// 否则探测者能从文案分辨账号是否初始化或会话是否过期。
func TestAdminCredentialFailureMessageMatchesMiddleware(t *testing.T) {
	store := initializedAdminStore(t)
	srv := newAdminTestServer(t, store, nil)

	loginRec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
	loginFailure := decodeAdminFailure(t, loginRec, http.StatusUnauthorized, codeSessionInvalid)

	// 无 Cookie 的退出请求：withSession 先拒绝，走 rejectAdminSession 那条出口。
	sessionRec := serveAdmin(srv, newLogoutRequest(nil, ""))
	sessionFailure := decodeAdminFailure(t, sessionRec, http.StatusUnauthorized, codeSessionInvalid)
	assertClearedCookie(t, sessionRec, true)

	if loginFailure.Message != sessionFailure.Message {
		t.Fatalf("登录与中间件的 401 文案不一致：登录 %q，中间件 %q",
			loginFailure.Message, sessionFailure.Message)
	}
	if loginFailure.Message != adminCredentialsInvalidMessage {
		t.Fatalf("文案期望 %q，实际 %q", adminCredentialsInvalidMessage, loginFailure.Message)
	}
}

// TestAdminLoginBadRequest 覆盖请求体问题：缺 password 与非法 JSON 都是 400 + 40001
// （错误表「字段缺失 / 非法 JSON」），与账号是否初始化无关。不算哈希、不写会话、
// 不下发任何 Cookie，也不计入失败限流——坏请求不代表有人在猜密码，口径与 init 一致。
func TestAdminLoginBadRequest(t *testing.T) {
	cases := []struct{ name, body string }{
		{"空对象", `{}`},
		{"password 为 null", `{"password":null}`},
		{"只有别的字段", `{"passwd":"hunter2"}`},
		{"不是合法 JSON", `{"password":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := initializedAdminStore(t)
			srv := newAdminTestServer(t, store, nil)
			hashBefore := auth.HashCallCount()

			rec := serveAdmin(srv, newLoginRequest(t, tc.body))

			decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			assertNoSetCookie(t, rec)
			if got := auth.HashCallCount(); got != hashBefore {
				t.Fatalf("缺字段不该计算哈希，实际增加 %d 次", got-hashBefore)
			}
			if calls, _ := store.snapshotInsert(); calls != 0 {
				t.Fatalf("缺字段不该写会话行，实际 InsertSession %d 次", calls)
			}
			if blocked, _ := srv.loginLimiter.Blocked(loginSource, testAdminNow); blocked {
				t.Fatal("40001 是坏请求，不该计入失败限流")
			}
		})
	}
}

// TestAdminLoginFailureLimitBlocksBeforeVerify 覆盖失败限流：5 次凭据失败后第 6 次
// 即使提交正确密码也直接 429 + 42901 + Retry-After，既不查管理员也不调用 VerifyPassword
// （Requirement 7.1、7.4、CP9）。用 HashCallCount 与 Admin 调用次数两处共同证明。
func TestAdminLoginFailureLimitBlocksBeforeVerify(t *testing.T) {
	store := initializedAdminStore(t)
	srv := newAdminTestServer(t, store, nil)

	for i := 0; i < failureLimitMaxFails; i++ {
		rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
		decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	}

	hashBefore := auth.HashCallCount()
	adminCallsBefore, _, _ := store.snapshotCalls()
	insertCallsBefore, _ := store.snapshotInsert()

	rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))

	decodeAdminFailure(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assertNoStore(t, rec)
	// 429 是「先别试」，不是凭据判定，因此不该清掉既有的会话 Cookie。
	assertNoSetCookie(t, rec)

	if got := auth.HashCallCount(); got != hashBefore {
		t.Fatalf("被限流的登录不该调用 VerifyPassword：argon2 次数从 %d 变成 %d", hashBefore, got)
	}
	adminCallsAfter, _, _ := store.snapshotCalls()
	if adminCallsAfter != adminCallsBefore {
		t.Fatalf("被限流的登录不该查管理员：之前 %d 次，之后 %d 次", adminCallsBefore, adminCallsAfter)
	}
	if insertCallsAfter, _ := store.snapshotInsert(); insertCallsAfter != insertCallsBefore {
		t.Fatalf("被限流的登录不该写会话：之前 %d 次，之后 %d 次", insertCallsBefore, insertCallsAfter)
	}

	retryAfter := rec.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 || seconds > int(failureLimitWindow/time.Second) {
		t.Fatalf("Retry-After 期望 1..%d 秒，实际 %q", int(failureLimitWindow/time.Second), retryAfter)
	}
}

// TestAdminLoginSuccessResetsFailureCount 覆盖成功登录清零该来源的失败计数（Requirement 7.3）：
// 先输错 4 次（差一次到阈值）再登录成功，然后再输错 4 次。若计数没被清零，累计失败会在
// 第一轮之后继续累加，第二轮的请求会变成 429 而不是 401。
func TestAdminLoginSuccessResetsFailureCount(t *testing.T) {
	store := initializedAdminStore(t)
	srv := newAdminTestServer(t, store, nil)

	for i := 0; i < failureLimitMaxFails-1; i++ {
		rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
		decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	}

	rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))
	contractSuccessData(t, rec)
	if calls, _ := store.snapshotInsert(); calls != 1 {
		t.Fatalf("成功登录应写 1 条会话，实际 %d 条", calls)
	}

	if blocked, _ := srv.loginLimiter.Blocked(loginSource, testAdminNow); blocked {
		t.Fatal("登录成功应清零该来源的失败计数")
	}

	for i := 0; i < failureLimitMaxFails-1; i++ {
		rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
		decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	}
}

// TestAdminLoginStoreFailures 覆盖登录路径上的仓储故障，各自的口径不同：
//   - 读管理员失败 → 500，不继续校验密码、不下发 Cookie；
//   - 写会话失败 → 500 且不下发 Cookie——否则浏览器会拿着一个服务端不存在的令牌反复 401；
//   - 清理过期会话失败只是 best effort，本次登录仍然 200 并正常下发 Cookie，
//     但要留一条 WARN（字段白名单与 request_id 由该子用例用独立的 buffer logger 断言）。
//
// 三种响应都不该把底层错误原文透给客户端（CP2）。
func TestAdminLoginStoreFailures(t *testing.T) {
	const sentinel = "login-store-down-5b2e"

	cases := []struct {
		name       string
		prepare    func(t *testing.T) *adminRuntimeStore
		wantVerify bool // 是否真的走到了 VerifyPassword（读管理员失败时走不到）
	}{
		{"读取管理员失败", func(*testing.T) *adminRuntimeStore {
			store := newAdminRuntimeStore()
			store.userErr = errors.New(sentinel)
			return store
		}, false},
		{"写会话失败", func(t *testing.T) *adminRuntimeStore {
			store := initializedAdminStore(t)
			store.insertErr = errors.New(sentinel)
			return store
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.prepare(t)
			srv := newAdminTestServer(t, store, nil)
			hashBefore := auth.HashCallCount()

			rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))

			decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
			assertNoSetCookie(t, rec)
			if strings.Contains(rec.Body.String(), sentinel) {
				t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
			}
			if verified := auth.HashCallCount() != hashBefore; verified != tc.wantVerify {
				t.Fatalf("期望走了 VerifyPassword=%t，实际 argon2 次数从 %d 变成 %d",
					tc.wantVerify, hashBefore, auth.HashCallCount())
			}
			if store.sessionCount() != 0 {
				t.Fatalf("失败路径不该留下会话行，实际 %d 行", store.sessionCount())
			}
		})
	}

	t.Run("清理过期会话失败不影响登录", func(t *testing.T) {
		// 这条路径是唯一「响应成功但仍留痕」的登录分支，所以日志要单独捕获来断言，
		// 不能沿用 newAdminTestServer 的 quietLogger。
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

		store := initializedAdminStore(t)
		store.expireErr = errors.New(sentinel)
		srv := New(logger, nil, nil, nil, nil, WithAdmin(store, nil))
		srv.clock = func() time.Time { return testAdminNow }

		rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))

		data := contractSuccessData(t, rec)
		assertNoStore(t, rec)
		if strings.Contains(rec.Body.String(), sentinel) {
			t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
		}
		cookie := loginSessionCookie(t, rec)
		if !store.hasSession(auth.HashSessionToken(cookie.Value)) {
			t.Fatal("清理失败不该影响本次会话的写入")
		}
		if data["csrf_token"] == "" {
			t.Fatalf("清理失败不该影响登录响应，实际 %#v", data)
		}

		// 清理失败必须留痕：恰好一条 WARN，字段限于白名单五项，request_id 与本次 200 响应里
		// 那个一致——清理发生在成功响应之前，两者必须指向同一次请求才好排障（Requirement 9.1、CP2）。
		wantRequestID, _ := decodeContractBody(t, rec)["request_id"].(string)
		if wantRequestID == "" {
			t.Fatalf("成功响应缺少 request_id: %s", rec.Body.String())
		}
		lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("期望 1 条 WARN（清理过期会话失败），实际 %d 条：%s", len(lines), logBuf.String())
		}
		var rawLine map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &rawLine); err != nil {
			t.Fatalf("日志不是合法 JSON: %v, %s", err, lines[0])
		}
		allowed := map[string]bool{
			"level": true, "msg": true, "time": true,
			"request_id": true, "method": true, "path": true, "status": true, "code": true,
		}
		for field := range rawLine {
			if !allowed[field] {
				t.Fatalf("日志出现白名单外字段 %q：%s", field, lines[0])
			}
		}
		var entry adminLogEntry
		if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
			t.Fatalf("日志不是合法 JSON: %v, %s", err, lines[0])
		}
		if entry.Level != "WARN" {
			t.Fatalf("清理失败应记 WARN 而不是影响响应，实际级别 %s", entry.Level)
		}
		if entry.RequestID != wantRequestID {
			t.Fatalf("日志 request_id %q 与成功响应 %q 不一致，拿日志定位不到这次登录", entry.RequestID, wantRequestID)
		}
		// 响应本身是 200/0，日志也必须照同一个口径记，不能记成 500。
		if entry.Status != http.StatusOK || entry.Code != 0 {
			t.Fatalf("日志状态码/业务码期望 200/0，实际 %d/%d", entry.Status, entry.Code)
		}
		if entry.Method != http.MethodPost || entry.Path != "/api/admin/session" {
			t.Fatalf("日志方法/路径期望 POST /api/admin/session，实际 %s %s", entry.Method, entry.Path)
		}
		for _, forbidden := range []string{sentinel, testAdminPassword, cookie.Value} {
			if strings.Contains(logBuf.String(), forbidden) {
				t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
			}
		}
	})
}

// TestAdminLoginWithoutStore 覆盖未装配数据来源：路由仍注册，按 50000 返回而不是 404，
// 让 main.go 漏装配立刻暴露（与 state/init 同口径）。
func TestAdminLoginWithoutStore(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	srv.clock = func() time.Time { return testAdminNow }

	rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword))))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
}

// TestAdminLoginAndInitFailureLimitersIndependent 钉住「两个端点各自计数」（Requirement 7）：
// loginLimiter 与 initLimiter 是同一个 Server 上的两个实例，一个端点的爆破不该把另一个端点
// 对同一来源锁住，否则攻击者只要往登录端点灌 5 次错密码，就能顺手把初始化和运维挡在门外。
//
// 两个方向都测：先用 5 次登录失败锁住 loginLimiter，确认 init 仍照常 40001/200/40902；
// 再用 5 次初始化失败锁住 initLimiter，确认登录仍照常 40101。
// 所有请求都用同一个来源（loginSource）：限流按来源分桶，换来源就算两边真的共用实例也测不出来。
// 断言落在响应码与 Retry-After 上，不只看 limiter 的内部状态——真正要保证的是对外行为。
func TestAdminLoginAndInitFailureLimitersIndependent(t *testing.T) {
	t.Run("登录被锁不影响初始化", func(t *testing.T) {
		// 空库：这里 5 次登录失败走的是「尚未初始化」那条 40101，同样计入 loginLimiter。
		store := newAdminRuntimeStore()
		srv := newAdminTestServer(t, store, newTestKeyManager(t))

		for i := 0; i < failureLimitMaxFails; i++ {
			rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
			decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
		}
		if blocked, _ := srv.loginLimiter.Blocked(loginSource, testAdminNow); !blocked {
			t.Fatalf("%d 次登录失败后该来源应被 loginLimiter 锁住", failureLimitMaxFails)
		}

		// 反向确认 initLimiter 自己没被登录失败计数污染。必须在任何 init 请求之前查：
		// init 成功会 Reset 该来源，之后再看就恒为未锁，断言会变成恒真。
		if blocked, _ := srv.initLimiter.Blocked(loginSource, testAdminNow); blocked {
			t.Fatal("登录失败不该计入 initLimiter")
		}

		// 40001：非法请求体仍按请求体问题拒绝，说明 initLimiter 的门没被登录失败触发。
		badRequest := serveAdmin(srv, newInitRequestFromLoginSource(`{"password":`))
		decodeAdminFailure(t, badRequest, http.StatusBadRequest, codeBadRequest)
		if got := badRequest.Header().Get("Retry-After"); got != "" {
			t.Fatalf("登录被锁不该给初始化下发 Retry-After，实际 %q", got)
		}

		// 200：初始化照常建出管理员与密钥——这一次成功本身就证明该来源没被锁。
		success := serveAdmin(srv, newInitRequestFromLoginSource(
			initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
		contractSuccessData(t, success)
		assertNoStore(t, success)

		// 40902：管理员已在，第二次初始化按状态冲突拒绝，仍然不是 42901。
		conflict := serveAdmin(srv, newInitRequestFromLoginSource(
			initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
		decodeAdminFailure(t, conflict, http.StatusConflict, codeAdminExists)
		if got := conflict.Header().Get("Retry-After"); got != "" {
			t.Fatalf("登录被锁不该给初始化下发 Retry-After，实际 %q", got)
		}
	})

	t.Run("初始化被锁不影响登录", func(t *testing.T) {
		// 空库：初始化会先过「管理员不存在」，再因两次密码不一致得到 42201（唯一计入 initLimiter 的失败）。
		store := newAdminRuntimeStore()
		srv := newAdminTestServer(t, store, newTestKeyManager(t))

		for i := 0; i < failureLimitMaxFails; i++ {
			rec := serveAdmin(srv, newInitRequestFromLoginSource(
				initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword+"-x"))))
			decodeAdminFailure(t, rec, http.StatusUnprocessableEntity, codePasswordInvalid)
		}
		if blocked, _ := srv.initLimiter.Blocked(loginSource, testAdminNow); !blocked {
			t.Fatalf("%d 次初始化失败后该来源应被 initLimiter 锁住", failureLimitMaxFails)
		}

		// 未初始化时登录的 40101 走 rejectAdminLogin，仍是登录链路自己的失败计数。
		rec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(testAdminPassword+"-wrong"))))
		failure := decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
		if failure.Message != adminCredentialsInvalidMessage {
			t.Fatalf("登录文案期望 %q，实际 %q", adminCredentialsInvalidMessage, failure.Message)
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Fatalf("初始化被锁不该给登录下发 Retry-After，实际 %q", got)
		}

		// 登录只累计了这一次，远未到阈值——初始化那 5 次没有算进来。
		if blocked, _ := srv.loginLimiter.Blocked(loginSource, testAdminNow); blocked {
			t.Fatal("初始化失败不该计入 loginLimiter")
		}
	})
}

// ---------------------------------------------------------------------------
// DELETE /api/admin/session
// ---------------------------------------------------------------------------

// TestAdminLogoutSuccess 覆盖退出成功：删除上下文里那条会话（按摘要，不重新解析 Cookie）、
// 下发清除 Cookie、返回 {logged_out:true} 并带 no-store（Requirement 5.4、9.2、CP11）。
func TestAdminLogoutSuccess(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	wantHash := auth.HashSessionToken(testAdminRawToken)
	store.sessions[wantHash] = activeAdminSession(testAdminNow.Add(time.Hour))
	srv := newAdminTestServer(t, store, nil)

	rec := serveAdmin(srv, newLogoutRequest(stateCookie(), testAdminCSRF))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "退出 data", data, "logged_out")
	if data["logged_out"] != true {
		t.Fatalf("logged_out 期望 true，实际 %#v", data["logged_out"])
	}
	assertNoStore(t, rec)
	assertClearedCookie(t, rec, true)

	calls, gotHash := store.snapshotDeleteSession()
	if calls != 1 || gotHash != wantHash {
		t.Fatalf("DeleteSession 期望 1 次且目标摘要 %q，实际 %d 次 / %q", wantHash, calls, gotHash)
	}
	if store.sessionCount() != 0 {
		t.Fatalf("退出后不该留下会话行，实际 %d 行", store.sessionCount())
	}
}

// TestAdminLogoutDeleteFailureKeepsCookie 覆盖删除会话报错：500 + 50000、不清 Cookie，
// 且响应不泄露底层错误。库里的行还在，会话仍然有效——此时清掉客户端 Cookie 只会让用户
// 以为自己退出了（错误表「仓储失败」一行）。
func TestAdminLogoutDeleteFailureKeepsCookie(t *testing.T) {
	const sentinel = "logout-delete-failed-3d9a"

	store := newAdminRuntimeStore()
	store.userFound = true
	wantHash := auth.HashSessionToken(testAdminRawToken)
	store.sessions[wantHash] = activeAdminSession(testAdminNow.Add(time.Hour))
	store.deleteSessionErr = errors.New(sentinel)
	srv := newAdminTestServer(t, store, nil)

	rec := serveAdmin(srv, newLogoutRequest(stateCookie(), testAdminCSRF))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
	if strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
	if !store.hasSession(wantHash) {
		t.Fatal("删除失败时会话行应保留")
	}

	// 故障恢复后同一个 Cookie 仍然可用：state 报 authenticated=true，证明会话真的没被作废。
	store.deleteSessionErr = nil
	stateRec := serveAdmin(srv, newStateRequest(stateCookie()))
	if data := contractSuccessData(t, stateRec); data["authenticated"] != true {
		t.Fatalf("删除失败不该影响会话有效性，实际 authenticated=%#v", data["authenticated"])
	}
}

// TestAdminLogoutWithoutSessionContext 覆盖「缺会话上下文」分支：链内不可达（withSession 先拦），
// 直接调用处理器时必须显式暴露成 500 + 50000，而不是当成已退出或 CSRF 失败；
// 此时不删任何会话、也不清 Cookie。
func TestAdminLogoutWithoutSessionContext(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	wantHash := auth.HashSessionToken(testAdminRawToken)
	store.sessions[wantHash] = activeAdminSession(testAdminNow.Add(time.Hour))
	srv := newAdminTestServer(t, store, nil)

	rec := httptest.NewRecorder()
	srv.adminLogout(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/session", nil))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoSetCookie(t, rec)
	if calls, _ := store.snapshotDeleteSession(); calls != 0 {
		t.Fatalf("缺会话上下文时不该删会话，实际 DeleteSession %d 次", calls)
	}
	if store.sessionCount() != 1 {
		t.Fatalf("会话行应原样保留，实际 %d 行", store.sessionCount())
	}
}

// TestAdminLogoutWithoutStore 覆盖未装配数据来源：链内不可达（withSession 先按同一口径 50000 拦下），
// 直接调用处理器时必须显式返回 500 + 50000，而不是假装已退出。
func TestAdminLogoutWithoutStore(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	srv.clock = func() time.Time { return testAdminNow }

	rec := httptest.NewRecorder()
	srv.adminLogout(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/session", nil))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoSetCookie(t, rec)
}
