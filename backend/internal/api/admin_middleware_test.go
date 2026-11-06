package api

// 本文件覆盖管理端中间件的契约（tasklist 第 8 项、CP7、CP11）：
// 请求体 8 KiB 上限与 application/json 要求、会话校验与 40101、CSRF 校验与 40301、
// 管理端响应的 no-store，以及「会话/CSRF 失败时业务写方法调用为 0」。
// 业务处理器与路由注册由后续任务实现，这里用替身处理器模拟写端点。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// testAdminRawToken 是会话 Cookie 的原文；库里只该出现它的 SHA-256（CP5）。
	testAdminRawToken = "raw-admin-session-token"
	// testAdminCSRF 是替身会话绑定的 CSRF 令牌。
	testAdminCSRF = "csrf-token-for-admin-middleware"
)

// testAdminNow 是中间件用例的固定时钟，边界用例都以它为基准加减。
var testAdminNow = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

// adminStoreStub 是管理中间件的替身：回放会话查询结果，并统计写方法调用次数。
// 写方法虽不在中间件用到的接口里，但业务替身会真的调用它们，
// 用来断言「中间件失败时业务逻辑没跑、写库调用为 0」（CP7）。
type adminStoreStub struct {
	session    model.AdminSession // Session 的返回值
	found      bool               // 会话记录是否存在
	sessionErr error              // 非 nil 时 Session 返回该错误

	sessionCalls int    // Session 调用次数
	gotTokenHash string // 最后一次查询用的令牌摘要
	writeCalls   int    // 所有写方法的总调用次数
}

// Session 回放预设的会话查询结果，并记下入参摘要。
func (s *adminStoreStub) Session(_ context.Context, tokenHash string) (model.AdminSession, bool, error) {
	s.sessionCalls++
	s.gotTokenHash = tokenHash
	return s.session, s.found, s.sessionErr
}

// UpdatePassword 记一次写调用；真实业务在改密时调用。
func (s *adminStoreStub) UpdatePassword(context.Context, string, time.Time) error {
	s.writeCalls++
	return nil
}

// InsertSession 记一次写调用；真实业务在登录时调用。
func (s *adminStoreStub) InsertSession(context.Context, model.AdminSession) error {
	s.writeCalls++
	return nil
}

// DeleteSession 记一次写调用；真实业务在退出登录时调用。
func (s *adminStoreStub) DeleteSession(context.Context, string) error {
	s.writeCalls++
	return nil
}

// DeleteAllSessions 记一次写调用；真实业务在改密成功后调用。
func (s *adminStoreStub) DeleteAllSessions(context.Context) (int64, error) {
	s.writeCalls++
	return 0, nil
}

// adminBusinessStub 模拟管理端写端点的业务逻辑：先按真实 handler 的写法解析请求体，
// 解析通过后才走替身的写方法。两个计数分开记：
//   - entered 表示请求进到了业务处理器（会话与 CSRF 中间件已放行）；
//   - wrote 表示业务真的走到了写库调用（请求体也解析通过）。
//
// 体不合法时 entered=true 但 wrote=false，因为解析与业务写入都发生在 handler 里；
// 所以「写方法调用为 0」的断言看 wrote/writeCalls，而不是「handler 没跑」（CP7）。
type adminBusinessStub struct {
	store      *adminStoreStub
	entered    bool
	wrote      bool
	gotSession model.AdminSession
	hasSession bool
}

// handler 返回替身业务处理器：解析成功才调用写方法并给 204。
func (b *adminBusinessStub) handler(s *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b.entered = true
		b.gotSession, b.hasSession = adminSessionFrom(r.Context())
		var body struct {
			CurrentPassword string `json:"current_password"`
		}
		if !s.decodeAdminBody(w, r, &body) {
			return
		}
		b.wrote = true
		_ = b.store.UpdatePassword(r.Context(), "new-hash", testAdminNow)
		_ = b.store.InsertSession(r.Context(), model.AdminSession{})
		_ = b.store.DeleteSession(r.Context(), "hash")
		_, _ = b.store.DeleteAllSessions(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}
}

// newAdminMiddlewareServer 组装只装了管理依赖的 Server：假时钟 + 替身会话来源。
func newAdminMiddlewareServer(store adminStore) *Server {
	srv := New(quietLogger(), nil, nil, nil, nil)
	srv.admin = store
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// activeAdminSession 返回一条以 testAdminRawToken 为 Cookie 原文的会话。
func activeAdminSession(expiresAt time.Time) model.AdminSession {
	return model.AdminSession{
		TokenHash: auth.HashSessionToken(testAdminRawToken),
		CSRFToken: testAdminCSRF,
		CreatedAt: testAdminNow.Add(-time.Hour),
		ExpiresAt: expiresAt,
	}
}

// activeAdminStore 返回「有会话且未过期」的替身存储。
func activeAdminStore() *adminStoreStub {
	return &adminStoreStub{found: true, session: activeAdminSession(testAdminNow.Add(time.Hour))}
}

// newAdminWriteRequest 构造管理端写请求：默认带 application/json 与合法 JSON 体。
// rawToken 为空表示不带 Cookie，csrf 为空表示不带 X-CSRF-Token。
func newAdminWriteRequest(t *testing.T, body, rawToken, csrf string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if rawToken != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: rawToken})
	}
	if csrf != "" {
		req.Header.Set(adminCSRFHeader, csrf)
	}
	return req
}

// runAdminWrite 用管理端写链路跑一个请求，返回响应与业务替身。
func runAdminWrite(t *testing.T, store *adminStoreStub, req *http.Request) (*httptest.ResponseRecorder, *adminBusinessStub) {
	t.Helper()
	business := &adminBusinessStub{store: store}
	srv := newAdminMiddlewareServer(store)
	rec := httptest.NewRecorder()
	srv.adminWrite(business.handler(srv))(rec, req)
	return rec, business
}

// adminFailure 是失败响应的最小形状。
type adminFailure struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// decodeAdminFailure 解析失败响应并断言 HTTP 状态、业务码与非空文案。
func decodeAdminFailure(t *testing.T, rec *httptest.ResponseRecorder, wantStatus, wantCode int) adminFailure {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("期望 HTTP %d，实际 %d（body=%s）", wantStatus, rec.Code, rec.Body.String())
	}
	var got adminFailure
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != wantCode {
		t.Fatalf("期望业务码 %d，实际 %d（body=%s）", wantCode, got.Code, rec.Body.String())
	}
	if got.Message == "" {
		t.Fatalf("失败响应缺少文案: %s", rec.Body.String())
	}
	return got
}

// assertNoStore 断言响应带 Cache-Control: no-store（Requirement 9.5、CP11）。
func assertNoStore(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("期望 Cache-Control: no-store，实际 %q", got)
	}
}

// assertNoSetCookie 断言响应没有下发任何 Cookie（例如 CSRF 失败时保留会话）。
func assertNoSetCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("不该下发 Set-Cookie，实际 %q", got)
	}
}

// assertClearedCookie 断言 401 响应带清除会话 Cookie 的 Set-Cookie：
// 同名、无值、Max-Age=0（Go 解析为 MaxAge<0），且 Path/HttpOnly/Secure/SameSite
// 与下发时一致——属性不一致时浏览器删不掉真正的死 Cookie。
// wantSecure 跟随装配：默认 true；本地 http 开发关掉后清除指令也必须不带 Secure。
func assertClearedCookie(t *testing.T, rec *httptest.ResponseRecorder, wantSecure bool) {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name != adminCookieName {
			continue
		}
		if c.Value != "" {
			t.Fatalf("清除 Cookie 不该带值，实际 %q", c.Value)
		}
		if c.MaxAge >= 0 {
			t.Fatalf("期望 Max-Age=0，实际 MaxAge=%d", c.MaxAge)
		}
		if c.Path != adminCookiePath {
			t.Fatalf("期望 Path=%s，实际 %q", adminCookiePath, c.Path)
		}
		if !c.HttpOnly {
			t.Fatal("清除 Cookie 应保留 HttpOnly")
		}
		if c.Secure != wantSecure {
			t.Fatalf("期望 Secure=%t，实际 %t", wantSecure, c.Secure)
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("期望 SameSite=Lax，实际 %v", c.SameSite)
		}
		return
	}
	t.Fatalf("缺少清除会话 Cookie 的 Set-Cookie，实际头: %v", rec.Header())
}

// TestAdminWriteRejectsNonJSONContentType 覆盖 Content-Type 门：
// 缺失、表单类型、近似 JSON 类型都返回 400 + 40001，且不进入业务逻辑（Requirement 6.6）。
func TestAdminWriteRejectsNonJSONContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
	}{
		{"缺少 Content-Type", ""},
		{"text/plain", "text/plain"},
		{"表单类型", "application/x-www-form-urlencoded"},
		{"multipart 表单", "multipart/form-data; boundary=abc"},
		{"近似 JSON 类型", "application/merge-patch+json"},
		{"无法解析的参数", "application/json; charset="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			req := newAdminWriteRequest(t, `{"current_password":"x"}`, testAdminRawToken, testAdminCSRF)
			req.Header.Set("Content-Type", tc.contentType)

			rec, business := runAdminWrite(t, store, req)

			decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			if business.entered {
				t.Fatal("Content-Type 不符时不该进入业务逻辑")
			}
			if store.writeCalls != 0 {
				t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
			}
			// Content-Type 门在会话之前：坏请求连库都不查。
			if store.sessionCalls != 0 {
				t.Fatalf("期望会话查询 0 次，实际 %d", store.sessionCalls)
			}
		})
	}
}

// TestAdminWriteAcceptsJSONContentType 覆盖允许的 Content-Type 写法：
// 带 charset、大小写不同的 application/json 都应放行并进入业务逻辑。
func TestAdminWriteAcceptsJSONContentType(t *testing.T) {
	cases := []struct{ name, contentType string }{
		{"标准写法", "application/json"},
		{"带 charset", "application/json; charset=utf-8"},
		{"大写", "Application/JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			req := newAdminWriteRequest(t, `{"current_password":"x"}`, testAdminRawToken, testAdminCSRF)
			req.Header.Set("Content-Type", tc.contentType)

			rec, business := runAdminWrite(t, store, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("期望 204，实际 %d（body=%s）", rec.Code, rec.Body.String())
			}
			if !business.wrote {
				t.Fatal("Content-Type 合法时应进入业务逻辑")
			}
			if !business.hasSession {
				t.Fatal("业务处理器应从上下文取到会话（state 需要它回 csrf_token）")
			}
			if business.gotSession.CSRFToken != testAdminCSRF {
				t.Fatalf("上下文里的会话不符: %+v", business.gotSession)
			}
			assertNoStore(t, rec)
			assertNoSetCookie(t, rec)
		})
	}
}

// TestAdminWriteBodyLimit8KiB 覆盖 8 KiB 体上限：超限返回 40001 且不进业务，
// 恰好 8 KiB 的合法请求放行（上限是「不超过 8 KiB」）。
func TestAdminWriteBodyLimit8KiB(t *testing.T) {
	t.Run("超过 8 KiB", func(t *testing.T) {
		store := activeAdminStore()
		body := `{"pad":"` + strings.Repeat("a", 9000) + `"}`
		req := newAdminWriteRequest(t, body, testAdminRawToken, testAdminCSRF)

		rec, business := runAdminWrite(t, store, req)

		got := decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
		if !strings.Contains(got.Message, "8 KiB") {
			t.Fatalf("超限文案应点明 8 KiB，实际 %q", got.Message)
		}
		assertNoStore(t, rec)
		if business.wrote {
			t.Fatal("体超限时不该进入业务逻辑")
		}
		if store.writeCalls != 0 {
			t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
		}
	})

	t.Run("恰好 8 KiB", func(t *testing.T) {
		store := activeAdminStore()
		pad := adminBodyLimit - len(`{"pad":""}`)
		body := `{"pad":"` + strings.Repeat("a", pad) + `"}`
		if len(body) != adminBodyLimit {
			t.Fatalf("用例构造错误：期望体长 %d，实际 %d", adminBodyLimit, len(body))
		}
		req := newAdminWriteRequest(t, body, testAdminRawToken, testAdminCSRF)

		rec, business := runAdminWrite(t, store, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("恰好 8 KiB 应放行，实际 %d（body=%s）", rec.Code, rec.Body.String())
		}
		if !business.wrote {
			t.Fatal("恰好 8 KiB 应进入业务逻辑")
		}
		assertNoStore(t, rec)
	})

	// 同一件事在分块传输下再确认一次：没有 Content-Length 时读取方不知道还剩多少字节，
	// 恰好用满上限的体同样不能触发 MaxBytesError——这正是删掉「按 Content-Length 特判」那条死分支的依据。
	t.Run("恰好 8 KiB（分块传输）", func(t *testing.T) {
		store := activeAdminStore()
		pad := adminBodyLimit - len(`{"pad":""}`)
		body := `{"pad":"` + strings.Repeat("a", pad) + `"}`
		req := newAdminWriteRequest(t, body, testAdminRawToken, testAdminCSRF)
		req.ContentLength = -1 // 模拟分块传输：没有可依据的 Content-Length

		rec, business := runAdminWrite(t, store, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("恰好 8 KiB（分块）应放行，实际 %d（body=%s）", rec.Code, rec.Body.String())
		}
		if !business.wrote {
			t.Fatal("恰好 8 KiB（分块）应进入业务逻辑")
		}
		assertNoStore(t, rec)
	})

	t.Run("合法前缀后跟超限空白", func(t *testing.T) {
		store := activeAdminStore()
		body := `{"pad":"x"}` + strings.Repeat(" ", 8989)
		if len(body) <= adminBodyLimit {
			t.Fatalf("用例构造错误：体长应超过 %d，实际 %d", adminBodyLimit, len(body))
		}
		req := newAdminWriteRequest(t, body, testAdminRawToken, testAdminCSRF)

		rec, business := runAdminWrite(t, store, req)

		decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
		assertNoStore(t, rec)
		if business.wrote {
			t.Fatal("体超限时不该进入业务逻辑")
		}
	})
}

// TestAdminWriteRejectsInvalidJSONBody 覆盖体解析：空体、非 JSON、截断、
// 多个 JSON 值都映射 40001，且不进入业务逻辑（design 错误表）。
func TestAdminWriteRejectsInvalidJSONBody(t *testing.T) {
	cases := []struct{ name, body string }{
		{"空体", ""},
		{"不是 JSON", "not-json"},
		{"截断对象", `{"current_password":`},
		{"两个 JSON 值", `{"a":1}{"b":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			req := newAdminWriteRequest(t, tc.body, testAdminRawToken, testAdminCSRF)

			rec, business := runAdminWrite(t, store, req)

			decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			assertNoStore(t, rec)
			if business.wrote {
				t.Fatal("体不合法时不该进入业务逻辑")
			}
			if store.writeCalls != 0 {
				t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
			}
		})
	}
}

// TestAdminWriteRequiresSession 覆盖 withSession：缺 Cookie、空 Cookie、记录不存在、
// 恰好到期、已过期都返回 401 + 40101 并清 Cookie；到期前一秒仍放行（Requirement 5.2、5.3）。
func TestAdminWriteRequiresSession(t *testing.T) {
	cases := []struct {
		name        string
		prepare     func(*adminStoreStub)
		rawToken    string
		emptyCookie bool
		wantStatus  int
		wantCode    int
		wantCalls   int
	}{
		{
			name: "没有 Cookie", rawToken: "",
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantCalls: 0,
		},
		{
			name: "Cookie 值为空", rawToken: "", emptyCookie: true,
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantCalls: 0,
		},
		{
			name:       "记录不存在",
			prepare:    func(s *adminStoreStub) { s.found = false },
			rawToken:   testAdminRawToken,
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantCalls: 1,
		},
		{
			name: "会话恰在过期时刻",
			prepare: func(s *adminStoreStub) {
				s.session = activeAdminSession(testAdminNow)
			},
			rawToken:   testAdminRawToken,
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantCalls: 1,
		},
		{
			name: "会话过期一小时",
			prepare: func(s *adminStoreStub) {
				s.session = activeAdminSession(testAdminNow.Add(-time.Hour))
			},
			rawToken:   testAdminRawToken,
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantCalls: 1,
		},
		{
			name: "会话还差一秒到期",
			prepare: func(s *adminStoreStub) {
				s.session = activeAdminSession(testAdminNow.Add(time.Second))
			},
			rawToken:   testAdminRawToken,
			wantStatus: http.StatusNoContent, wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			if tc.prepare != nil {
				tc.prepare(store)
			}
			req := newAdminWriteRequest(t, `{"current_password":"x"}`, tc.rawToken, testAdminCSRF)
			if tc.emptyCookie {
				req.Header.Set("Cookie", adminCookieName+"=")
			}

			rec, business := runAdminWrite(t, store, req)

			if store.sessionCalls != tc.wantCalls {
				t.Fatalf("期望会话查询 %d 次，实际 %d", tc.wantCalls, store.sessionCalls)
			}
			assertNoStore(t, rec)
			if tc.wantStatus == http.StatusNoContent {
				if rec.Code != http.StatusNoContent {
					t.Fatalf("期望 204，实际 %d（body=%s）", rec.Code, rec.Body.String())
				}
				if !business.wrote {
					t.Fatal("有效会话应进入业务逻辑")
				}
				assertNoSetCookie(t, rec)
				return
			}
			got := decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)
			if got.Message != "凭据无效或会话已过期" {
				t.Fatalf("401 文案应统一，实际 %q", got.Message)
			}
			assertClearedCookie(t, rec, true)
			if business.entered {
				t.Fatal("会话无效时不该进入业务逻辑")
			}
			if store.writeCalls != 0 {
				t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
			}
		})
	}
}

// TestAdminClearCookieFollowsSecureConfig 覆盖本地 http 开发（ADMIN_COOKIE_SECURE=false）：
// 清除 Cookie 也要跟着去掉 Secure，否则浏览器会忽略删除指令，死 Cookie 永远删不掉。
func TestAdminClearCookieFollowsSecureConfig(t *testing.T) {
	store := activeAdminStore()
	srv := newAdminMiddlewareServer(store)
	srv.adminCookieSecure = false
	business := &adminBusinessStub{store: store}

	rec := httptest.NewRecorder()
	srv.adminWrite(business.handler(srv))(rec, newAdminWriteRequest(t, `{}`, "", testAdminCSRF))

	decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	assertClearedCookie(t, rec, false)
}

// TestAdminSessionLookupUsesHashedToken 覆盖令牌口径：查库用的是 Cookie 原文的 SHA-256，
// 库里不该出现可直接使用的原文（CP5）。
func TestAdminSessionLookupUsesHashedToken(t *testing.T) {
	store := activeAdminStore()
	req := newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)

	rec, business := runAdminWrite(t, store, req)

	if rec.Code != http.StatusNoContent || !business.wrote {
		t.Fatalf("期望进入业务逻辑，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	want := auth.HashSessionToken(testAdminRawToken)
	if store.gotTokenHash != want {
		t.Fatalf("期望按摘要查询 %q，实际 %q", want, store.gotTokenHash)
	}
	if store.gotTokenHash == testAdminRawToken {
		t.Fatal("查库不该使用 Cookie 原文")
	}
	if len(store.gotTokenHash) != 64 {
		t.Fatalf("期望 64 位十六进制摘要，实际 %d 位", len(store.gotTokenHash))
	}
}

// TestAdminWriteStoreFailure 覆盖仓储失败：500 + 50000，不泄露内部细节，不清 Cookie。
func TestAdminWriteStoreFailure(t *testing.T) {
	store := activeAdminStore()
	store.sessionErr = errors.New("db down")
	req := newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)

	rec, business := runAdminWrite(t, store, req)

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
	if strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
	if business.entered {
		t.Fatal("仓储失败时不该进入业务逻辑")
	}
	if store.writeCalls != 0 {
		t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
	}
}

// TestAdminWriteRequiresCSRF 覆盖 withCSRF：缺失或不一致都返回 403 + 40301，
// 保留会话 Cookie、不进入业务逻辑；令牌正确则放行（Requirement 6.3、CP7）。
func TestAdminWriteRequiresCSRF(t *testing.T) {
	cases := []struct {
		name       string
		csrf       string
		wantStatus int
	}{
		{"缺失令牌", "", http.StatusForbidden},
		{"令牌不一致", "wrong-csrf-token", http.StatusForbidden},
		{"令牌正确", testAdminCSRF, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			req := newAdminWriteRequest(t, `{}`, testAdminRawToken, tc.csrf)

			rec, business := runAdminWrite(t, store, req)

			assertNoStore(t, rec)
			if tc.wantStatus == http.StatusNoContent {
				if rec.Code != http.StatusNoContent {
					t.Fatalf("期望 204，实际 %d（body=%s）", rec.Code, rec.Body.String())
				}
				if !business.wrote {
					t.Fatal("CSRF 正确应进入业务逻辑")
				}
				return
			}
			// 403 保留会话：客户端拿到新令牌后可以直接重试，不用重新登录。
			decodeAdminFailure(t, rec, http.StatusForbidden, codeCSRFInvalid)
			if strings.Contains(rec.Body.String(), testAdminCSRF) {
				t.Fatalf("响应泄露了会话的 CSRF 令牌: %s", rec.Body.String())
			}
			assertNoSetCookie(t, rec)
			if business.entered {
				t.Fatal("CSRF 失败时不该进入业务逻辑")
			}
			if store.writeCalls != 0 {
				t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
			}
			if store.sessionCalls != 1 {
				t.Fatalf("CSRF 校验前应先查一次会话，实际 %d 次", store.sessionCalls)
			}
		})
	}
}

// TestAdminWriteSessionBeforeCSRF 钉住顺序：会话与 CSRF 同时不满足时返回 40101 而不是 40301，
// 且根本不查库（没有 Cookie 就没有可比对的会话）。
func TestAdminWriteSessionBeforeCSRF(t *testing.T) {
	store := activeAdminStore()
	req := newAdminWriteRequest(t, `{}`, "", "wrong-csrf-token")

	rec, business := runAdminWrite(t, store, req)

	decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	if store.sessionCalls != 0 {
		t.Fatalf("没有 Cookie 时不该查库，实际 %d 次", store.sessionCalls)
	}
	if business.entered {
		t.Fatal("会话失败时不该进入业务逻辑")
	}
	if store.writeCalls != 0 {
		t.Fatalf("期望写方法调用 0 次，实际 %d", store.writeCalls)
	}
}

// TestAdminWriteGuardsSkipStoreWrites 集中断言写端点的三道门（体门、会话、CSRF）都在业务写入之前：
// 任一失败时替身的写方法调用次数为 0（CP7）。最后一例是对照组：全部通过时必须真的发生写调用，
// 否则计数器本身失效，前面的 0 次断言就没有意义。
func TestAdminWriteGuardsSkipStoreWrites(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(store *adminStoreStub) *http.Request
		wantStatus int
	}{
		{"Content-Type 不符", func(*adminStoreStub) *http.Request {
			req := newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)
			req.Header.Set("Content-Type", "text/plain")
			return req
		}, http.StatusBadRequest},
		{"请求体非法", func(*adminStoreStub) *http.Request {
			return newAdminWriteRequest(t, "not-json", testAdminRawToken, testAdminCSRF)
		}, http.StatusBadRequest},
		{"没有会话", func(*adminStoreStub) *http.Request {
			return newAdminWriteRequest(t, `{}`, "", testAdminCSRF)
		}, http.StatusUnauthorized},
		{"会话已过期", func(store *adminStoreStub) *http.Request {
			store.session = activeAdminSession(testAdminNow)
			return newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)
		}, http.StatusUnauthorized},
		{"CSRF 缺失", func(*adminStoreStub) *http.Request {
			return newAdminWriteRequest(t, `{}`, testAdminRawToken, "")
		}, http.StatusForbidden},
		{"对照组：全部通过", func(*adminStoreStub) *http.Request {
			return newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)
		}, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := activeAdminStore()
			rec, business := runAdminWrite(t, store, tc.prepare(store))

			if rec.Code != tc.wantStatus {
				t.Fatalf("期望 HTTP %d，实际 %d（body=%s）", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if tc.wantStatus == http.StatusNoContent {
				if store.writeCalls == 0 || !business.wrote {
					t.Fatal("对照组应发生写调用，否则计数器的 0 次断言无效")
				}
				return
			}
			if business.wrote {
				t.Fatal("被门拦下的请求不该走到业务写入")
			}
			if store.writeCalls != 0 {
				t.Fatalf("被门拦下的请求不该触发写方法，实际 %d 次", store.writeCalls)
			}
		})
	}
}

// TestAdminWriteWithoutStore 覆盖未装配数据来源：与 WithMeta 未装配同口径，按 50000 返回。
func TestAdminWriteWithoutStore(t *testing.T) {
	business := &adminBusinessStub{store: activeAdminStore()}
	srv := newAdminMiddlewareServer(nil)
	req := newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)

	rec := httptest.NewRecorder()
	srv.adminWrite(business.handler(srv))(rec, req)

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	if business.entered {
		t.Fatal("未装配时不该进入业务逻辑")
	}
}

// TestAdminResponsesNoStore 逐条覆盖管理端响应的缓存控制：
// 成功与每一种错误（400/401/403/500）都必须带 Cache-Control: no-store（Requirement 9.5、CP11）。
func TestAdminResponsesNoStore(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"成功", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			rec, business := runAdminWrite(t, store, newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF))
			if !business.wrote || rec.Code != http.StatusNoContent {
				t.Fatalf("成功场景未放行: %d", rec.Code)
			}
			return rec
		}},
		{"Content-Type 不符", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			req := newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)
			req.Header.Set("Content-Type", "text/plain")
			rec, _ := runAdminWrite(t, store, req)
			return rec
		}},
		{"体超过 8 KiB", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			body := `{"pad":"` + strings.Repeat("a", 9000) + `"}`
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, body, testAdminRawToken, testAdminCSRF))
			return rec
		}},
		{"非法 JSON", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, "not-json", testAdminRawToken, testAdminCSRF))
			return rec
		}},
		{"缺会话", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, `{}`, "", testAdminCSRF))
			return rec
		}},
		{"会话过期", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			store.session = activeAdminSession(testAdminNow)
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF))
			return rec
		}},
		{"CSRF 不符", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, `{}`, testAdminRawToken, "wrong"))
			return rec
		}},
		{"仓储失败", func(t *testing.T) *httptest.ResponseRecorder {
			store := activeAdminStore()
			store.sessionErr = errors.New("db down")
			rec, _ := runAdminWrite(t, store, newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF))
			return rec
		}},
		{"未装配", func(t *testing.T) *httptest.ResponseRecorder {
			business := &adminBusinessStub{store: activeAdminStore()}
			srv := newAdminMiddlewareServer(nil)
			rec := httptest.NewRecorder()
			srv.adminWrite(business.handler(srv))(rec, newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF))
			return rec
		}},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			assertNoStore(t, tc.run(t))
		})
	}
}

// TestAdminCSRFWithoutSessionContext 直接调用 withCSRF 覆盖「上下文无会话」分支：
// 该分支在 adminWrite 链内不可达（withSession 放行后必然有会话），只能绕过链路单独测。
// 它必须按 50000 暴露而不是 40301——后者会把「中间件装配错了」误导成「前端该去重取令牌」。
func TestAdminCSRFWithoutSessionContext(t *testing.T) {
	srv := newAdminMiddlewareServer(activeAdminStore())
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }

	rec := httptest.NewRecorder()
	srv.withCSRF(next)(rec, newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if called {
		t.Fatal("缺少会话上下文时不该进入业务逻辑")
	}
}

// TestWithNoStoreUsesSet 覆盖 no-store 的写法：进到 withNoStore 时已存在的 Cache-Control
// 会被 Set 替换成单个 no-store，而不是用 Add 叠成 `max-age=3600, no-store` 这种自相矛盾的组合。
// 它只钉住「替换已有值」这一半语义；下游再改这个头的情形不在本中间件的责任里，
// 管理路由（GET state、404/405）的统一包装由 task 9 负责。
func TestWithNoStoreUsesSet(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "max-age=3600")

	withNoStore(func(http.ResponseWriter, *http.Request) {})(rec, httptest.NewRequest(http.MethodGet, "/api/admin/state", nil))

	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
		t.Fatalf("期望单个 Cache-Control: no-store，实际 %v", got)
	}
}

// adminLogEntry 是一条管理端日志行的形状：字段名与 Requirement 9.1 的白名单一一对应。
type adminLogEntry struct {
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Code      int    `json:"code"`
}

// TestAdminFailureLogWhitelist 覆盖管理端日志的字段白名单（Requirement 9.1、CP2）：
// 一条失败日志只带 request_id / 方法 / 路径 / 状态码 / 业务码，且 request_id 与响应里那个一致
// （不一致就没法拿日志定位到具体那一次响应）；err 原文、Cookie 值都不进日志。
func TestAdminFailureLogWhitelist(t *testing.T) {
	// sentinelErr 是塞进仓储错误里的哨兵：它若被写进日志，下面的断言会直接抓到。
	const sentinelErr = "sentinel-err-7c1f"

	cases := []struct {
		name       string
		prepare    func(*adminStoreStub) *http.Request
		wantStatus int
		wantCode   int
		wantLevel  string
	}{
		{
			name: "缺会话：INFO + 401/40101",
			prepare: func(*adminStoreStub) *http.Request {
				return newAdminWriteRequest(t, `{}`, "", testAdminCSRF)
			},
			wantStatus: http.StatusUnauthorized, wantCode: codeSessionInvalid, wantLevel: "INFO",
		},
		{
			name: "仓储失败：ERROR + 500/50000",
			prepare: func(store *adminStoreStub) *http.Request {
				store.sessionErr = errors.New("db down: " + sentinelErr)
				return newAdminWriteRequest(t, `{}`, testAdminRawToken, testAdminCSRF)
			},
			wantStatus: http.StatusInternalServerError, wantCode: codeInternal, wantLevel: "ERROR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
			store := activeAdminStore()
			req := tc.prepare(store)
			business := &adminBusinessStub{store: store}
			srv := New(logger, nil, nil, nil, nil)
			srv.admin = store
			srv.clock = func() time.Time { return testAdminNow }

			rec := httptest.NewRecorder()
			srv.adminWrite(business.handler(srv))(rec, req)

			got := decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)

			// 一次失败只记一条日志，且字段集合必须正好是白名单五项（外加 slog 自带的 level/msg/time）。
			lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("期望 1 条日志，实际 %d 条：%s", len(lines), logBuf.String())
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &raw); err != nil {
				t.Fatalf("日志不是合法 JSON: %v, %s", err, lines[0])
			}
			allowed := map[string]bool{
				"level": true, "msg": true, "time": true,
				"request_id": true, "method": true, "path": true, "status": true, "code": true,
			}
			for key := range raw {
				if !allowed[key] {
					t.Fatalf("日志出现白名单外字段 %q：%s", key, lines[0])
				}
			}

			var entry adminLogEntry
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatalf("日志不是合法 JSON: %v, %s", err, lines[0])
			}
			if entry.RequestID != got.RequestID {
				t.Fatalf("日志 request_id %q 与响应 %q 不一致，拿日志定位不到这次响应", entry.RequestID, got.RequestID)
			}
			if !strings.HasPrefix(entry.RequestID, "req_") {
				t.Fatalf("request_id 形状不合契约: %q", entry.RequestID)
			}
			if entry.Method != http.MethodPost {
				t.Fatalf("期望记录方法 POST，实际 %q", entry.Method)
			}
			if entry.Path != "/api/admin/password" {
				t.Fatalf("期望记录路径 /api/admin/password，实际 %q", entry.Path)
			}
			if entry.Status != tc.wantStatus || entry.Code != tc.wantCode {
				t.Fatalf("期望日志状态码/业务码 %d/%d，实际 %d/%d", tc.wantStatus, tc.wantCode, entry.Status, entry.Code)
			}
			if entry.Level != tc.wantLevel {
				t.Fatalf("期望日志级别 %s，实际 %s", tc.wantLevel, entry.Level)
			}
			if entry.Msg == "" {
				t.Fatal("日志缺少「哪一类检查没过」的文案")
			}

			// 敏感内容一律不进日志：err 原文、Cookie 值都不能出现。
			for _, forbidden := range []string{sentinelErr, "db down", testAdminRawToken, adminCookieName} {
				if strings.Contains(logBuf.String(), forbidden) {
					t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
				}
			}
		})
	}
}

// TestAdminSessionFromMissingContext 覆盖上下文取会话的失败分支：没走 withSession 时返回 false。
func TestAdminSessionFromMissingContext(t *testing.T) {
	if _, ok := adminSessionFrom(context.Background()); ok {
		t.Fatal("空上下文不该有会话")
	}
}
