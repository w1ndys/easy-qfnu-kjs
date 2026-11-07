package api

// 本文件覆盖 tasklist 第 9 项：GET /api/admin/state 与 POST /api/admin/init。
// 用例尽量走真实路由（srv.Handler()）：管理端链路、no-store、公开限流与失败限流都由真实实现承担，
// 替身只负责数据来源，密钥一律落在 t.TempDir() 下的真实文件上。
// 时间沿用同包既有的固定时钟 testAdminNow；哈希是否发生用 auth.HashCallCount 观察（CP9、CP3、CP4）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
)

// testAdminPassword 是能通过 auth.ValidatePassword 的固定密码（12 码点以上）。
const testAdminPassword = "correct horse battery staple"

// adminRuntimeStore 是 state/init 用例的替身存储：带锁，并发初始化用例会同时打进来。
// 它模拟 store.AdminRepo 的关键语义：CreateAdmin 只有第一次返回 created=true。
type adminRuntimeStore struct {
	mu sync.Mutex

	user      model.AdminUser
	userFound bool
	userErr   error
	createErr error
	deleteErr error

	// onCreate 在 CreateAdmin 成功写入管理员之后、仍持锁时调用；补偿用例用它模拟
	// 「Reload 时还没有密钥文件、Create 之前另一个进程抢先写好」的时序，让 Create 的
	// O_EXCL 必然撞 ErrKeyExists。钩子不得回调本替身的方法，否则会自锁。
	onCreate func()

	adminCalls  int
	createCalls int
	deleteCalls int

	// deleteCtxErr / deleteCtxDeadline / deleteCtxHasDeadline 记录 DeleteAdmin 被调用时
	// ctx 的状态：补偿用例用它断言「客户端断开后补偿仍有一个未取消、带截止时间的 ctx」。
	deleteCtxErr         error
	deleteCtxDeadline    time.Time
	deleteCtxHasDeadline bool

	sessions   map[string]model.AdminSession
	sessionErr error

	// 以下字段服务于 task 10 的登录/退出用例：会话写方法的错误注入与入参记录。
	// task 9 的用例不触碰它们，零值即「照常写入、不报错」。
	insertErr          error
	deleteSessionErr   error
	expireErr          error
	insertCalls        int
	deleteSessionCalls int
	expireCalls        int
	gotInsert          model.AdminSession
	gotDeleteHash      string
	gotExpireNow       time.Time
}

// newAdminRuntimeStore 返回空库替身：没有管理员、没有会话。
func newAdminRuntimeStore() *adminRuntimeStore {
	return &adminRuntimeStore{sessions: make(map[string]model.AdminSession)}
}

// Admin 回放管理员读取结果。
func (s *adminRuntimeStore) Admin(context.Context) (model.AdminUser, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminCalls++
	if s.userErr != nil {
		return model.AdminUser{}, false, s.userErr
	}
	return s.user, s.userFound, nil
}

// CreateAdmin 模拟 INSERT ... ON CONFLICT DO NOTHING：已存在时 created=false。
func (s *adminRuntimeStore) CreateAdmin(_ context.Context, passwordHash string, now time.Time) (model.AdminUser, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return model.AdminUser{}, false, s.createErr
	}
	if s.userFound {
		return model.AdminUser{}, false, nil
	}
	s.user = model.AdminUser{
		ID:                1,
		PasswordHash:      passwordHash,
		PasswordUpdatedAt: now,
		CreatedAt:         now,
	}
	s.userFound = true
	if s.onCreate != nil {
		s.onCreate()
	}
	return s.user, true, nil
}

// DeleteAdmin 模拟补偿删除；deleteErr 非 nil 时用于覆盖「补偿失败」分支。
// 同时记下 ctx 的取消状态与截止时间，让补偿用例能验证客户端取消没有传染进来、
// 且补偿仍受 adminCompensateTimeout 约束。
func (s *adminRuntimeStore) DeleteAdmin(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	s.deleteCtxErr = ctx.Err()
	s.deleteCtxDeadline, s.deleteCtxHasDeadline = ctx.Deadline()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.user = model.AdminUser{}
	s.userFound = false
	return nil
}

// Session 按令牌摘要回放会话。
func (s *adminRuntimeStore) Session(_ context.Context, tokenHash string) (model.AdminSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionErr != nil {
		return model.AdminSession{}, false, s.sessionErr
	}
	session, ok := s.sessions[tokenHash]
	return session, ok, nil
}

// InsertSession 落一条会话，并记下入参供登录用例断言。
func (s *adminRuntimeStore) InsertSession(_ context.Context, session model.AdminSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.insertCalls++
	if s.insertErr != nil {
		return s.insertErr
	}
	s.gotInsert = session
	s.sessions[session.TokenHash] = session
	return nil
}

// DeleteSession 按摘要删一条会话（退出登录），并记下目标摘要。
func (s *adminRuntimeStore) DeleteSession(_ context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteSessionCalls++
	s.gotDeleteHash = tokenHash
	if s.deleteSessionErr != nil {
		return s.deleteSessionErr
	}
	delete(s.sessions, tokenHash)
	return nil
}

// DeleteExpiredSessions 删除过期时刻不晚于 now 的会话，边界与真实仓储一致
// （now < expires_at 才有效，所以 expires_at = now 算过期）。
func (s *adminRuntimeStore) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireCalls++
	s.gotExpireNow = now
	if s.expireErr != nil {
		return 0, s.expireErr
	}
	var removed int64
	for hash, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, hash)
			removed++
		}
	}
	return removed, nil
}

// snapshotInsert 在锁内取 InsertSession 的调用次数与最后一次入参。
func (s *adminRuntimeStore) snapshotInsert() (calls int, session model.AdminSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insertCalls, s.gotInsert
}

// snapshotDeleteSession 在锁内取 DeleteSession 的调用次数与最后一次目标摘要。
func (s *adminRuntimeStore) snapshotDeleteSession() (calls int, tokenHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteSessionCalls, s.gotDeleteHash
}

// snapshotExpire 在锁内取 DeleteExpiredSessions 的调用次数与最后一次入参时钟。
func (s *adminRuntimeStore) snapshotExpire() (calls int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expireCalls, s.gotExpireNow
}

// sessionCount 在锁内取当前会话行数。
func (s *adminRuntimeStore) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// hasSession 在锁内判定某个摘要是否有会话行。
func (s *adminRuntimeStore) hasSession(tokenHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[tokenHash]
	return ok
}

// snapshotUser 在锁内取一份管理员记录，供断言使用。
func (s *adminRuntimeStore) snapshotUser() (model.AdminUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.user, s.userFound
}

// snapshotCalls 在锁内取三类调用次数。
func (s *adminRuntimeStore) snapshotCalls() (adminCalls, createCalls, deleteCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminCalls, s.createCalls, s.deleteCalls
}

// snapshotDeleteCtx 在锁内取 DeleteAdmin 被调用时的 ctx 状态：
// 是否带截止时间、截止时间本身、是否已取消。
func (s *adminRuntimeStore) snapshotDeleteCtx() (hasDeadline bool, deadline time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCtxHasDeadline, s.deleteCtxDeadline, s.deleteCtxErr
}

// newAdminTestServer 装配带固定时钟的 Server；store 与 keys 都通过 WithAdmin 注入。
func newAdminTestServer(t *testing.T, store adminStore, keys *secret.Manager) *Server {
	t.Helper()
	srv := New(quietLogger(), nil, nil, nil, nil, WithAdmin(store, keys))
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// newTestKeyManager 返回指向临时目录内不存在文件的密钥管理器。
func newTestKeyManager(t *testing.T) *secret.Manager {
	t.Helper()
	return secret.NewManager(filepath.Join(t.TempDir(), "secret.key"))
}

// concurrentKeyFileContent 是一份合法的密钥文件内容：43 个 base64url 字符（全 'A'，
// 解码为 32 个零字节）加结尾换行。补偿用例用它模拟另一个进程抢先落盘的密钥，
// 让 Create 的 O_EXCL 必然撞 ErrKeyExists。
var concurrentKeyFileContent = strings.Repeat("A", 43) + "\n"

// writeConcurrentKey 在密钥路径上写入一份合法密钥文件，模拟「Reload 之后、Create 之前
// 另一个进程抢先初始化」的时序；由 adminRuntimeStore.onCreate 钩子调用。
func writeConcurrentKey(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(concurrentKeyFileContent), 0o600); err != nil {
		t.Fatalf("模拟并发写入密钥文件失败: %v", err)
	}
}

// serveAdmin 用真实路由执行一个请求。
func serveAdmin(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// newInitRequest 构造初始化请求；body 由调用方给出，Content-Type 固定为 application/json。
func newInitRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/init", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// initJSON 序列化初始化请求体；字段按指针语义允许缺省，所以这里用指针 map。
func initJSON(t *testing.T, password, confirm *string) string {
	t.Helper()
	payload := map[string]*string{}
	if password != nil {
		payload["password"] = password
	}
	if confirm != nil {
		payload["password_confirm"] = confirm
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造初始化请求体失败: %v", err)
	}
	return string(raw)
}

// ptr 返回字符串指针，便于构造 pointer 字段。
func ptr(value string) *string { return &value }

// newStateRequest 构造状态请求；cookie 为 nil 表示未登录。
func newStateRequest(cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/state", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

// stateCookie 返回指向 testAdminRawToken 的会话 Cookie。
func stateCookie() *http.Cookie {
	return &http.Cookie{Name: adminCookieName, Value: testAdminRawToken}
}

// assertNullFields 断言 data 里给定键都是 JSON null。
func assertNullFields(t *testing.T, data map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if data[key] != nil {
			t.Fatalf("%s 期望 null，实际 %#v", key, data[key])
		}
	}
}

// ---------------------------------------------------------------------------
// GET /api/admin/state
// ---------------------------------------------------------------------------

// TestAdminStateUninitialized 覆盖首次部署：没有管理员记录时 200 + initialized=false，
// 后三项为 null，且响应带 no-store（Requirement 1.5、9.5）。
func TestAdminStateUninitialized(t *testing.T) {
	store := newAdminRuntimeStore()
	srv := newAdminTestServer(t, store, newTestKeyManager(t))

	rec := serveAdmin(srv, newStateRequest(nil))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "state data", data,
		"initialized", "authenticated", "expires_at", "csrf_token", "key_fingerprint")
	if data["initialized"] != false {
		t.Fatalf("期望 initialized=false，实际 %#v", data["initialized"])
	}
	if data["authenticated"] != false {
		t.Fatalf("期望 authenticated=false，实际 %#v", data["authenticated"])
	}
	assertNullFields(t, data, "expires_at", "csrf_token", "key_fingerprint")
	assertNoStore(t, rec)
}

// TestAdminStateInitializedUnauthenticated 覆盖已初始化但未登录：200 + initialized=true、
// authenticated=false，后三项仍为 null；未登录不返回 401（design「约束」）。
func TestAdminStateInitializedUnauthenticated(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	srv := newAdminTestServer(t, store, newTestKeyManager(t))

	rec := serveAdmin(srv, newStateRequest(nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("未登录也要 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	data := contractSuccessData(t, rec)
	if data["initialized"] != true || data["authenticated"] != false {
		t.Fatalf("期望 initialized=true, authenticated=false，实际 %#v", data)
	}
	assertNullFields(t, data, "expires_at", "csrf_token", "key_fingerprint")
	assertNoStore(t, rec)
}

// TestAdminStateAuthenticated 覆盖有效会话：回 expires_at（RFC3339 UTC）、csrf_token 与密钥指纹。
// 指纹必须与密钥文件里的内容一致，便于管理员核对备份（Requirement 3.5、6.4）。
func TestAdminStateAuthenticated(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	store.sessions[auth.HashSessionToken(testAdminRawToken)] = activeAdminSession(testAdminNow.Add(time.Hour))

	keys := newTestKeyManager(t)
	key, err := keys.Create()
	if err != nil {
		t.Fatalf("准备密钥失败: %v", err)
	}
	srv := newAdminTestServer(t, store, keys)

	rec := serveAdmin(srv, newStateRequest(stateCookie()))

	data := contractSuccessData(t, rec)
	if data["initialized"] != true || data["authenticated"] != true {
		t.Fatalf("期望 initialized=true, authenticated=true，实际 %#v", data)
	}
	wantExpires := testAdminNow.Add(time.Hour).UTC().Format(time.RFC3339)
	if data["expires_at"] != wantExpires {
		t.Fatalf("expires_at 期望 %q，实际 %#v", wantExpires, data["expires_at"])
	}
	if data["csrf_token"] != testAdminCSRF {
		t.Fatalf("csrf_token 期望 %q，实际 %#v", testAdminCSRF, data["csrf_token"])
	}
	if data["key_fingerprint"] != key.Fingerprint() {
		t.Fatalf("key_fingerprint 期望 %q，实际 %#v", key.Fingerprint(), data["key_fingerprint"])
	}
	assertNoStore(t, rec)
}

// TestAdminStateSessionExpiryBoundary 钉住「now < expires_at」的边界：
// 到期前一秒仍算登录，恰好到期与已过期都按未登录处理（CP6）。
func TestAdminStateSessionExpiryBoundary(t *testing.T) {
	cases := []struct {
		name       string
		expiresAt  time.Time
		wantAuthed bool
	}{
		{"到期前一秒", testAdminNow.Add(time.Second), true},
		{"恰在到期时刻", testAdminNow, false},
		{"已过期一小时", testAdminNow.Add(-time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newAdminRuntimeStore()
			store.userFound = true
			store.sessions[auth.HashSessionToken(testAdminRawToken)] = activeAdminSession(tc.expiresAt)
			srv := newAdminTestServer(t, store, newTestKeyManager(t))

			rec := serveAdmin(srv, newStateRequest(stateCookie()))

			data := contractSuccessData(t, rec)
			if data["authenticated"] != tc.wantAuthed {
				t.Fatalf("期望 authenticated=%t，实际 %#v", tc.wantAuthed, data["authenticated"])
			}
			if !tc.wantAuthed {
				assertNullFields(t, data, "expires_at", "csrf_token", "key_fingerprint")
			}
		})
	}
}

// TestAdminStateKeyFingerprintMissing 覆盖 CP14：管理员已初始化但密钥文件缺失时，
// 进程与 state 照常工作，只有 key_fingerprint 为 null，其余会话字段仍然可用。
func TestAdminStateKeyFingerprintMissing(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	store.sessions[auth.HashSessionToken(testAdminRawToken)] = activeAdminSession(testAdminNow.Add(time.Hour))
	keys := newTestKeyManager(t) // 不 Create，也不 Load：内存里没有密钥
	srv := newAdminTestServer(t, store, keys)

	rec := serveAdmin(srv, newStateRequest(stateCookie()))

	data := contractSuccessData(t, rec)
	if data["authenticated"] != true {
		t.Fatalf("密钥缺失不该影响登录态，实际 %#v", data)
	}
	if data["csrf_token"] != testAdminCSRF || data["expires_at"] == nil {
		t.Fatalf("密钥缺失不该影响会话字段，实际 %#v", data)
	}
	assertNullFields(t, data, "key_fingerprint")
	assertNoStore(t, rec)
}

// TestAdminStateStoreFailure 覆盖仓储失败：500 + 50000，不泄露内部细节，且仍带 no-store。
func TestAdminStateStoreFailure(t *testing.T) {
	const sentinel = "state-db-down-4a1c"
	cases := []struct {
		name    string
		prepare func(*adminRuntimeStore)
		cookie  *http.Cookie
	}{
		{"读取管理员失败", func(s *adminRuntimeStore) {
			s.userErr = errors.New("db down: " + sentinel)
		}, nil},
		{"读取会话失败", func(s *adminRuntimeStore) {
			s.userFound = true
			s.sessionErr = errors.New("db down: " + sentinel)
		}, stateCookie()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newAdminRuntimeStore()
			tc.prepare(store)
			srv := newAdminTestServer(t, store, newTestKeyManager(t))

			rec := serveAdmin(srv, newStateRequest(tc.cookie))

			decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
			if strings.Contains(rec.Body.String(), sentinel) || strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
			}
		})
	}
}

// TestAdminStateWithoutAdmin 覆盖未装配数据来源：路由仍注册，按 50000 返回而不是 404。
func TestAdminStateWithoutAdmin(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	srv.clock = func() time.Time { return testAdminNow }

	rec := serveAdmin(srv, newStateRequest(nil))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
}

// TestAdminStatePublicRateLimit 覆盖 state 走公开 60 次/分钟限流：
// 窗口内恰好 60 次放行，第 61 次 429 + 42901 且带 no-store（Requirement 32.1、CP11）。
func TestAdminStatePublicRateLimit(t *testing.T) {
	store := newAdminRuntimeStore()
	srv := newAdminTestServer(t, store, newTestKeyManager(t))

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveAdmin(srv, newStateRequest(nil))
		contractSuccessData(t, rec)
	}
	adminCallsBefore, _, _ := store.snapshotCalls()

	rec := serveAdmin(srv, newStateRequest(nil))

	decodeAdminFailure(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assertNoStore(t, rec)
	adminCallsAfter, _, _ := store.snapshotCalls()
	if adminCallsAfter != adminCallsBefore {
		t.Fatalf("被限流的请求不该查库：之前 %d 次，之后 %d 次", adminCallsBefore, adminCallsAfter)
	}
}

// TestAdminRouteNoStoreOnUnmatched 覆盖不经过业务处理器的响应：管理端前缀下的 405（方法不匹配）
// 与 404（未注册路径）也必须带 no-store，它们由 ServeMux 直接写出，内层 withNoStore 覆盖不到，
// 靠 Handler 返回前的按前缀包装兜住（CP11、Requirement 9.5）。
// 同时反向断言公开路径的 404 不带头：no-store 只属于管理端，包装不能变成全局的。
func TestAdminRouteNoStoreOnUnmatched(t *testing.T) {
	srv := newAdminTestServer(t, newAdminRuntimeStore(), newTestKeyManager(t))

	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"state 用了写方法", http.MethodPost, "/api/admin/state", http.StatusMethodNotAllowed},
		{"init 用了读方法", http.MethodGet, "/api/admin/init", http.StatusMethodNotAllowed},
		{"session 用了读方法", http.MethodGet, "/api/admin/session", http.StatusMethodNotAllowed},
		{"未注册的管理端路径", http.MethodGet, "/api/admin/unknown", http.StatusNotFound},
		{"管理前缀本身", http.MethodGet, "/api/admin", http.StatusNotFound},
		{"管理端路径多一个斜杠", http.MethodGet, "/api/admin/state/", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdmin(srv, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantStatus {
				t.Fatalf("期望 HTTP %d，实际 %d（body=%s）", tc.wantStatus, rec.Code, rec.Body.String())
			}
			assertNoStore(t, rec)
		})
	}

	t.Run("公开路径的 404 不带 no-store", func(t *testing.T) {
		rec := serveAdmin(srv, httptest.NewRequest(http.MethodGet, "/api/nope", nil))

		if rec.Code != http.StatusNotFound {
			t.Fatalf("期望 HTTP 404，实际 %d（body=%s）", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "" {
			t.Fatalf("公开路径的 404 不该带 Cache-Control，实际 %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// POST /api/admin/init
// ---------------------------------------------------------------------------

// TestAdminInitSuccess 覆盖初始化成功路径：恰好算一次哈希、写入管理员记录、以 O_EXCL 0600
// 建出密钥文件，响应只回一次性明文密钥、算法与指纹，并带 no-store + Pragma（Requirement 1.2、3.1–3.3）。
func TestAdminInitSuccess(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	srv := newAdminTestServer(t, store, keys)

	hashBefore := auth.HashCallCount()
	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	hashAfter := auth.HashCallCount()

	if hashAfter-hashBefore != 1 {
		t.Fatalf("初始化成功应恰好哈希 1 次，实际 %d 次", hashAfter-hashBefore)
	}

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "init data", data, "encryption_key", "key_algorithm", "key_fingerprint")
	assertNoStore(t, rec)
	if got := rec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("初始化响应应带 Pragma: no-cache，实际 %q", got)
	}
	if data["key_algorithm"] != keyAlgorithm {
		t.Fatalf("key_algorithm 期望 %q，实际 %#v", keyAlgorithm, data["key_algorithm"])
	}

	key, err := keys.Current()
	if err != nil {
		t.Fatalf("初始化后密钥应已装载: %v", err)
	}
	if data["encryption_key"] != key.Export() {
		t.Fatalf("encryption_key 与内存密钥不一致：%#v", data["encryption_key"])
	}
	if data["key_fingerprint"] != key.Fingerprint() {
		t.Fatalf("key_fingerprint 与内存密钥不一致：%#v", data["key_fingerprint"])
	}

	// 文件内容就是 43 字符 base64url + 换行，权限 0600（Requirement 3.1）。
	raw, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if string(raw) != key.Export()+"\n" {
		t.Fatalf("密钥文件内容不符: %q", string(raw))
	}
	info, err := os.Stat(keys.Path())
	if err != nil {
		t.Fatalf("stat 密钥文件失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限期望 0600，实际 %#o", perm)
	}

	// 管理员记录：PHC 串可校验回原密码，时间取注入的时钟。
	user, found := store.snapshotUser()
	if !found {
		t.Fatal("初始化成功后应存在管理员记录")
	}
	if !strings.HasPrefix(user.PasswordHash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("密码哈希不是约定的 PHC 串: %q", user.PasswordHash)
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, testAdminPassword)
	if err != nil || !ok {
		t.Fatalf("密码哈希校验失败: ok=%t err=%v", ok, err)
	}
	if !user.CreatedAt.Equal(testAdminNow) || !user.PasswordUpdatedAt.Equal(testAdminNow) {
		t.Fatalf("管理员时间列应取注入时钟 %s，实际 %+v", testAdminNow, user)
	}
	_, createCalls, _ := store.snapshotCalls()
	if createCalls != 1 {
		t.Fatalf("CreateAdmin 期望调用 1 次，实际 %d 次", createCalls)
	}
}

// TestAdminInitRejectsExistingAdmin 覆盖固定顺序的第一道门：管理员已存在时立即 40902，
// 不校验密码、不计算哈希、不读写密钥文件（CP3）。第二个子用例用非法 JSON 证明
// 「管理员存在」的判定排在请求体解析之前——这里的请求体解析指 handler 里的 decode，
// 不包含路由上的 Content-Type 体门（体门在前，见 TestAdminInitContentTypeGatePrecedesAdminCheck）。
func TestAdminInitRejectsExistingAdmin(t *testing.T) {
	cases := []struct{ name, body string }{
		{"密码不合法也先判管理员", initJSON(t, ptr("short"), ptr("short"))},
		{"请求体非法也先判管理员", `{"password":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newAdminRuntimeStore()
			store.userFound = true
			keys := newTestKeyManager(t)
			srv := newAdminTestServer(t, store, keys)

			hashBefore := auth.HashCallCount()
			rec := serveAdmin(srv, newInitRequest(tc.body))
			hashAfter := auth.HashCallCount()

			decodeAdminFailure(t, rec, http.StatusConflict, codeAdminExists)
			assertNoStore(t, rec)
			if hashAfter != hashBefore {
				t.Fatalf("被拒绝的初始化不应计算哈希，实际增加 %d 次", hashAfter-hashBefore)
			}
			if _, createCalls, _ := store.snapshotCalls(); createCalls != 0 {
				t.Fatalf("被拒绝的初始化不应写管理员，实际 %d 次", createCalls)
			}
			if _, err := os.Stat(keys.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("被拒绝的初始化不应创建密钥文件，stat err=%v", err)
			}
		})
	}
}

// TestAdminInitContentTypeGatePrecedesAdminCheck 钉住体门的位置：已存在管理员 + text/plain → 40001。
// Content-Type 体门（withJSONBody）挂在 Handler 的路由上、位于 adminInit 之外，先于「管理员已存在」
// 的判定执行（CP7 的链序）；所以「管理员存在时连请求体都不解析」只在通过体门之后成立，
// 准确表述要限定为「除 Content-Type 体外」。这里同时断言体门在查库之前就返回。
func TestAdminInitContentTypeGatePrecedesAdminCheck(t *testing.T) {
	store := newAdminRuntimeStore()
	store.userFound = true
	keys := newTestKeyManager(t)
	srv := newAdminTestServer(t, store, keys)

	req := newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword)))
	req.Header.Set("Content-Type", "text/plain")
	rec := serveAdmin(srv, req)

	decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
	assertNoStore(t, rec)
	if adminCalls, createCalls, _ := store.snapshotCalls(); adminCalls != 0 || createCalls != 0 {
		t.Fatalf("体门应在查库之前拦下请求，实际 Admin %d 次、CreateAdmin %d 次", adminCalls, createCalls)
	}
	if _, err := os.Stat(keys.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("体门拒绝的请求不应创建密钥文件，stat err=%v", err)
	}
}

// TestAdminInitPasswordValidation 覆盖第二道门：密码规则不符 42201、请求体问题 40001；
// 两类拒绝都不哈希、不写管理员、不碰密钥文件（Requirement 2.1、2.2、错误表）。
func TestAdminInitPasswordValidation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
	}{
		{"两次不一致", initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword+"x")),
			http.StatusUnprocessableEntity, codePasswordInvalid},
		{"少于 12 码点", initJSON(t, ptr("short"), ptr("short")),
			http.StatusUnprocessableEntity, codePasswordInvalid},
		{"超过 256 码点", initJSON(t, ptr(strings.Repeat("a", 257)), ptr(strings.Repeat("a", 257))),
			http.StatusUnprocessableEntity, codePasswordInvalid},
		{"缺少确认字段", initJSON(t, ptr(testAdminPassword), nil),
			http.StatusBadRequest, codeBadRequest},
		{"缺少密码字段", initJSON(t, nil, ptr(testAdminPassword)),
			http.StatusBadRequest, codeBadRequest},
		{"不是合法 JSON", `{"password":`,
			http.StatusBadRequest, codeBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newAdminRuntimeStore()
			keys := newTestKeyManager(t)
			srv := newAdminTestServer(t, store, keys)

			hashBefore := auth.HashCallCount()
			rec := serveAdmin(srv, newInitRequest(tc.body))
			hashAfter := auth.HashCallCount()

			decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)
			assertNoStore(t, rec)
			if hashAfter != hashBefore {
				t.Fatalf("密码校验失败不应哈希，实际增加 %d 次", hashAfter-hashBefore)
			}
			if _, createCalls, _ := store.snapshotCalls(); createCalls != 0 {
				t.Fatalf("密码校验失败不应写管理员，实际 %d 次", createCalls)
			}
			if _, err := os.Stat(keys.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("密码校验失败不应创建密钥文件，stat err=%v", err)
			}
		})
	}
}

// TestAdminInitRejectsExistingKey 覆盖第三道门：密钥已存在时 40903，保持既有密钥内容、
// 不计算哈希、不写管理员记录（CP4、Requirement 3.4）。
func TestAdminInitRejectsExistingKey(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	key, err := keys.Create()
	if err != nil {
		t.Fatalf("准备密钥失败: %v", err)
	}
	before, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	srv := newAdminTestServer(t, store, keys)

	hashBefore := auth.HashCallCount()
	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	hashAfter := auth.HashCallCount()

	decodeAdminFailure(t, rec, http.StatusConflict, codeKeyExists)
	assertNoStore(t, rec)
	if hashAfter != hashBefore {
		t.Fatalf("密钥已存在时不应哈希，实际增加 %d 次", hashAfter-hashBefore)
	}
	if _, createCalls, _ := store.snapshotCalls(); createCalls != 0 {
		t.Fatalf("密钥已存在时不应写管理员，实际 %d 次", createCalls)
	}
	after, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("密钥已存在时不应改动文件内容")
	}
	if current, err := keys.Current(); err != nil || current != key {
		t.Fatalf("内存里的密钥不该被替换: key=%v err=%v", current, err)
	}
}

// TestAdminInitRejectsKeyFileNotLoaded 覆盖 minor 1：内存为空、密钥文件已在磁盘上。
// Present() 只看内存；若 Present() 为假时不对账文件，流程会走到 CreateAdmin，
// 再由 Create 的 O_EXCL 挡下并触发补偿，白建又白删一条管理员记录（CP4）。
// 用「文件写好后新建一个 Manager」构造进程重启后的典型状态：文件在、内存空。
func TestAdminInitRejectsKeyFileNotLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	writer := secret.NewManager(path)
	key, err := writer.Create()
	if err != nil {
		t.Fatalf("准备密钥失败: %v", err)
	}
	keys := secret.NewManager(path)
	if keys.Present() {
		t.Fatal("前置条件：新 Manager 的内存应为空")
	}
	store := newAdminRuntimeStore()
	srv := newAdminTestServer(t, store, keys)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}

	hashBefore := auth.HashCallCount()
	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	hashAfter := auth.HashCallCount()

	decodeAdminFailure(t, rec, http.StatusConflict, codeKeyExists)
	assertNoStore(t, rec)
	if hashAfter != hashBefore {
		t.Fatalf("文件已有密钥时不应哈希，实际增加 %d 次", hashAfter-hashBefore)
	}
	adminCalls, createCalls, _ := store.snapshotCalls()
	if adminCalls != 2 {
		t.Fatalf("该分支应先查一次管理员、再为排除并发赢家查第二次，实际 %d 次", adminCalls)
	}
	if createCalls != 0 {
		t.Fatalf("文件已有密钥时不应写管理员，实际 %d 次", createCalls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("文件已有密钥时不应改动内容")
	}
	// Reload 把磁盘上那把装回内存：state 的指纹从此可用，后续 init 仍按 40903 拒绝。
	current, err := keys.Current()
	if err != nil || current != key {
		t.Fatalf("Reload 应把磁盘上的密钥装回内存: key=%v err=%v", current, err)
	}
}

// TestAdminInitUnavailableKeyFileRejectsAndKeepsMemory 覆盖 minor 2 的 api 层口径：
// 文件损坏时 Reload 返回 ErrKeyUnavailable 且保留旧键，init 按 40903 拒绝——不建管理员、
// 不补偿、不覆盖文件；日志只带白名单字段，不含错误原文（可能带路径与解析细节）。
func TestAdminInitUnavailableKeyFileRejectsAndKeepsMemory(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := newTestKeyManager(t)
	key, err := keys.Create()
	if err != nil {
		t.Fatalf("准备密钥失败: %v", err)
	}
	const corrupted = "被改坏的密钥内容-91be"
	if err := os.WriteFile(keys.Path(), []byte(corrupted), 0o600); err != nil {
		t.Fatalf("改坏密钥文件失败: %v", err)
	}
	store := newAdminRuntimeStore()
	srv := New(logger, nil, nil, nil, nil, WithAdmin(store, keys))
	srv.clock = func() time.Time { return testAdminNow }

	hashBefore := auth.HashCallCount()
	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	hashAfter := auth.HashCallCount()

	failure := decodeAdminFailure(t, rec, http.StatusConflict, codeKeyExists)
	assertNoStore(t, rec)
	if hashAfter != hashBefore {
		t.Fatalf("密钥文件不可用时不应哈希，实际增加 %d 次", hashAfter-hashBefore)
	}
	if _, createCalls, deleteCalls := store.snapshotCalls(); createCalls != 0 || deleteCalls != 0 {
		t.Fatalf("密钥文件不可用不应建管理员、也不补偿，实际 CreateAdmin %d 次、DeleteAdmin %d 次",
			createCalls, deleteCalls)
	}
	// 内存保留：文件坏了不代表进程里已经拿到的那把失效（CP14）。
	current, err := keys.Current()
	if err != nil || current != key {
		t.Fatalf("Reload 失败不应清空内存密钥: key=%v err=%v", current, err)
	}
	// 文件不被覆盖（CP4）。
	raw, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if string(raw) != corrupted {
		t.Fatalf("密钥文件不应被改动: %q", string(raw))
	}

	// 日志：一条 INFO、白名单字段、与响应同一 request_id，不出现路径或文件内容。
	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("期望 1 条日志（密钥文件不可用），实际 %d 条：%s", len(lines), logBuf.String())
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
	if entry.Level != "INFO" || entry.Status != http.StatusConflict || entry.Code != codeKeyExists {
		t.Fatalf("日志期望 INFO/409/%d，实际 %s/%d/%d", codeKeyExists, entry.Level, entry.Status, entry.Code)
	}
	if entry.RequestID != failure.RequestID {
		t.Fatalf("日志 request_id %q 与响应 %q 不一致", entry.RequestID, failure.RequestID)
	}
	for _, forbidden := range []string{keys.Path(), corrupted, testAdminPassword} {
		if strings.Contains(logBuf.String(), forbidden) {
			t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
		}
	}
}

// TestAdminInitReinitializesAfterManualReset 覆盖同进程重新初始化：运维按 50000 补偿失败时的
// 提示「删 admin_user + 删密钥文件」手工重置后，用同一个 Server 重新 init 必须成功。
// 内存里仍留着旧密钥，若密钥对账不先按文件 Reload，就会被永久挡在 40903。
func TestAdminInitReinitializesAfterManualReset(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	srv := newAdminTestServer(t, store, keys)

	first := contractSuccessData(t, serveAdmin(srv,
		newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword)))))

	// 手工重置：删管理员记录 + 删密钥文件（进程不重启，内存里仍有旧密钥）。
	if err := store.DeleteAdmin(context.Background()); err != nil {
		t.Fatalf("删除管理员记录失败: %v", err)
	}
	if err := os.Remove(keys.Path()); err != nil {
		t.Fatalf("删除密钥文件失败: %v", err)
	}

	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

	second := contractSuccessData(t, rec)
	assertNoStore(t, rec)
	if second["encryption_key"] == first["encryption_key"] {
		t.Fatal("重新初始化应生成新密钥，不能复用内存里的旧副本")
	}
	if _, found := store.snapshotUser(); !found {
		t.Fatal("重新初始化后应重新写入管理员记录")
	}
	if _, _, deleteCalls := store.snapshotCalls(); deleteCalls != 1 {
		t.Fatalf("重新初始化是成功路径，不该触发补偿；期望 DeleteAdmin 1 次（手工重置），实际 %d 次", deleteCalls)
	}

	// 密钥文件内容、内存密钥与响应三者一致，且是重新初始化后的这一把。
	raw, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	wantKey, _ := second["encryption_key"].(string)
	if wantKey == "" || string(raw) != wantKey+"\n" {
		t.Fatalf("密钥文件内容 %q 应为 %q", string(raw), wantKey+"\n")
	}
	current, err := keys.Current()
	if err != nil || current.Export() != wantKey {
		t.Fatalf("内存密钥应与重新初始化的响应一致：key=%v err=%v", current, err)
	}
}

// adminRaceStore 让第一次 Admin() 返回「没有管理员」，之后返回替身里的真实结果。
// 它确定性复现并发初始化里最窄的窗口：输家查完管理员之后、查密钥之前被调度挂起，
// 等它恢复时赢家已经写完了记录与密钥。
type adminRaceStore struct {
	*adminRuntimeStore
	adminCalls int
}

// Admin 首次返回空库，之后透传给内嵌替身。
func (s *adminRaceStore) Admin(ctx context.Context) (model.AdminUser, bool, error) {
	s.adminCalls++
	if s.adminCalls == 1 {
		return model.AdminUser{}, false, nil
	}
	return s.adminRuntimeStore.Admin(ctx)
}

// TestAdminInitKeySeenAfterConcurrentWinner 覆盖上述窗口：虽然 Present() 为真，
// 但管理员记录也已出现，必须按 40902 返回，不能变成 40903——否则「50 个并发恰好 1 个 200、
// 其余 40902」的契约会出现例外。此时仍未哈希、未写管理员。
func TestAdminInitKeySeenAfterConcurrentWinner(t *testing.T) {
	base := newAdminRuntimeStore()
	base.userFound = true // 赢家已插入记录
	store := &adminRaceStore{adminRuntimeStore: base}

	keys := newTestKeyManager(t)
	if _, err := keys.Create(); err != nil {
		t.Fatalf("准备赢家密钥失败: %v", err)
	}
	srv := newAdminTestServer(t, store, keys)

	hashBefore := auth.HashCallCount()
	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	hashAfter := auth.HashCallCount()

	decodeAdminFailure(t, rec, http.StatusConflict, codeAdminExists)
	assertNoStore(t, rec)
	if store.adminCalls != 2 {
		t.Fatalf("期望在密钥分支再查一次管理员，实际 Admin 调用 %d 次", store.adminCalls)
	}
	if hashAfter != hashBefore {
		t.Fatalf("该分支不应哈希，实际增加 %d 次", hashAfter-hashBefore)
	}
	if _, createCalls, _ := base.snapshotCalls(); createCalls != 0 {
		t.Fatalf("该分支不应写管理员，实际 %d 次", createCalls)
	}
}

// TestAdminInitStoreFailure 覆盖仓储失败：读取管理员与创建管理员失败都返回 50000，
// 不创建密钥文件，且不把底层错误写进响应。
func TestAdminInitStoreFailure(t *testing.T) {
	const sentinel = "init-store-down-91be"
	cases := []struct {
		name    string
		prepare func(*adminRuntimeStore)
	}{
		{"读取管理员失败", func(s *adminRuntimeStore) { s.userErr = errors.New("db down: " + sentinel) }},
		{"创建管理员失败", func(s *adminRuntimeStore) { s.createErr = errors.New("db down: " + sentinel) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newAdminRuntimeStore()
			tc.prepare(store)
			keys := newTestKeyManager(t)
			srv := newAdminTestServer(t, store, keys)

			rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

			decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
			if strings.Contains(rec.Body.String(), sentinel) || strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
			}
			if _, err := os.Stat(keys.Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("仓储失败不应创建密钥文件，stat err=%v", err)
			}
		})
	}
}

// TestAdminInitCompensatesKeyCreateFailure 覆盖补偿：CreateAdmin 成功后 Create 失败时，
// 调 DeleteAdmin 回滚并返回 50000，不留「有管理员、无密钥」的中间态（D10、错误处理）。
// 失败用「另一个进程在 Reload 之后、Create 之前抢先落盘」确定性制造：Create 的 O_EXCL
// 撞上 ErrKeyExists，正是它要挡的跨进程竞争，不依赖权限位或平台差异。
func TestAdminInitCompensatesKeyCreateFailure(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	store.onCreate = func() { writeConcurrentKey(t, keys.Path()) }
	srv := newAdminTestServer(t, store, keys)

	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

	assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	if _, createCalls, deleteCalls := store.snapshotCalls(); createCalls != 1 || deleteCalls != 1 {
		t.Fatalf("期望 CreateAdmin 1 次、DeleteAdmin 补偿 1 次，实际 %d / %d", createCalls, deleteCalls)
	}
	if _, found := store.snapshotUser(); found {
		t.Fatal("补偿后不应留下管理员记录")
	}
	// 补偿只删管理员记录，不能删掉或覆盖别人已经写好的密钥文件（CP4）。
	raw, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取并发写好的密钥文件失败: %v", err)
	}
	if string(raw) != concurrentKeyFileContent {
		t.Fatalf("补偿不应改动既有密钥文件内容: %q", string(raw))
	}
	if keys.Present() {
		t.Fatal("初始化失败后不应把别人的密钥装进内存")
	}
}

// TestAdminInitCompensationSurvivesClientCancel 覆盖补偿与客户端取消解耦：
// 请求 ctx 已取消时（客户端超时断开），补偿仍然要走到 DeleteAdmin，且传进去的 ctx 未被取消、
// 仍带 adminCompensateTimeout 截止时间（context.WithoutCancel + context.WithTimeout）。
// 否则补偿必然失败，库里留下「有管理员、无密钥」的中间态（D10）。
func TestAdminInitCompensationSurvivesClientCancel(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	store.onCreate = func() { writeConcurrentKey(t, keys.Path()) }
	srv := newAdminTestServer(t, store, keys)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))).WithContext(ctx)

	rec := serveAdmin(srv, req)

	assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if _, createCalls, deleteCalls := store.snapshotCalls(); createCalls != 1 || deleteCalls != 1 {
		t.Fatalf("期望 CreateAdmin 1 次、DeleteAdmin 补偿 1 次，实际 %d / %d", createCalls, deleteCalls)
	}
	hasDeadline, deadline, ctxErr := store.snapshotDeleteCtx()
	if ctxErr != nil {
		t.Fatalf("补偿的 ctx 不该继承客户端的取消信号，实际 err=%v", ctxErr)
	}
	if !hasDeadline {
		t.Fatal("补偿的 ctx 必须带截止时间，避免卡死的库把补偿无限拖住")
	}
	// 请求已经返回，剩余时间应接近 adminCompensateTimeout；+1s 余量只容忍调度与测量误差。
	if remaining := time.Until(deadline); remaining <= 0 || remaining > adminCompensateTimeout+time.Second {
		t.Fatalf("补偿 ctx 剩余时间应落在 (0, %s]，实际 %s", adminCompensateTimeout+time.Second, remaining)
	}
	if _, found := store.snapshotUser(); found {
		t.Fatal("补偿后不应留下管理员记录")
	}
}

// TestAdminInitCompensationFailureLogged 覆盖补偿失败：日志出现 init_compensation_failed，
// 且两条 ERROR 都只带白名单字段、与响应共享同一个 request_id，不含底层错误原文。
func TestAdminInitCompensationFailureLogged(t *testing.T) {
	const sentinel = "compensation-delete-failed-6f2d"
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	store := newAdminRuntimeStore()
	store.deleteErr = errors.New(sentinel)
	keys := newTestKeyManager(t)
	store.onCreate = func() { writeConcurrentKey(t, keys.Path()) }
	srv := New(logger, nil, nil, nil, nil, WithAdmin(store, keys))
	srv.clock = func() time.Time { return testAdminNow }

	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	failure := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	wantRequestID, _ := failure["request_id"].(string)

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("期望 2 条 ERROR（补偿失败 + 密钥写入失败），实际 %d 条：%s", len(lines), logBuf.String())
	}

	allowed := map[string]bool{
		"level": true, "msg": true, "time": true,
		"request_id": true, "method": true, "path": true, "status": true, "code": true,
	}
	sawCompensation := false
	for _, line := range lines {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("日志不是合法 JSON: %v, %s", err, line)
		}
		for key := range raw {
			if !allowed[key] {
				t.Fatalf("日志出现白名单外字段 %q：%s", key, line)
			}
		}
		var entry adminLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日志不是合法 JSON: %v, %s", err, line)
		}
		if entry.RequestID != wantRequestID {
			t.Fatalf("日志 request_id %q 与响应 %q 不一致", entry.RequestID, wantRequestID)
		}
		if entry.Status != http.StatusInternalServerError || entry.Code != codeInternal {
			t.Fatalf("日志状态码/业务码期望 500/50000，实际 %d/%d", entry.Status, entry.Code)
		}
		if entry.Level != "ERROR" {
			t.Fatalf("期望 ERROR 级别，实际 %s", entry.Level)
		}
		if entry.Msg == "init_compensation_failed" {
			sawCompensation = true
		}
	}
	if !sawCompensation {
		t.Fatalf("缺少 init_compensation_failed 日志：%s", logBuf.String())
	}
	for _, forbidden := range []string{sentinel, "delete failed", testAdminPassword, adminCookieName} {
		if strings.Contains(logBuf.String(), forbidden) {
			t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
		}
	}
}

// TestAdminInitFailureLimit 覆盖初始化失败限流：5 次密码校验失败后第 6 次 429 + Retry-After，
// 被限流的请求既不查库也不哈希（Requirement 7.4、7.6、CP9）。
func TestAdminInitFailureLimit(t *testing.T) {
	store := newAdminRuntimeStore()
	srv := newAdminTestServer(t, store, newTestKeyManager(t))

	hashBefore := auth.HashCallCount()
	for i := 0; i < failureLimitMaxFails; i++ {
		rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword+"-wrong"))))
		decodeAdminFailure(t, rec, http.StatusUnprocessableEntity, codePasswordInvalid)
		assertNoStore(t, rec)
	}
	if auth.HashCallCount() != hashBefore {
		t.Fatalf("密码校验失败不应哈希，实际增加 %d 次", auth.HashCallCount()-hashBefore)
	}
	adminCallsBefore, _, _ := store.snapshotCalls()

	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

	decodeAdminFailure(t, rec, http.StatusTooManyRequests, codeRateLimited)
	assertNoStore(t, rec)
	retryAfter := rec.Header().Get("Retry-After")
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds < 1 || seconds > int(failureLimitWindow/time.Second) {
		t.Fatalf("Retry-After 期望 1..%d 秒，实际 %q", int(failureLimitWindow/time.Second), retryAfter)
	}
	adminCallsAfter, _, _ := store.snapshotCalls()
	if adminCallsAfter != adminCallsBefore {
		t.Fatalf("被限流的请求不该查库：之前 %d 次，之后 %d 次", adminCallsBefore, adminCallsAfter)
	}
	if auth.HashCallCount() != hashBefore {
		t.Fatalf("被限流的请求不该哈希，实际增加 %d 次", auth.HashCallCount()-hashBefore)
	}
}

// TestAdminInitBadRequestDoesNotCountTowardFailureLimit 锁住「只有 42201 计入失败限流」的一面：
// 连续 5 次非法 JSON（40001）之后，第 6 次合法初始化仍必须成功，而不是被折成 42901。
func TestAdminInitBadRequestDoesNotCountTowardFailureLimit(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	srv := newAdminTestServer(t, store, keys)

	for i := 0; i < failureLimitMaxFails; i++ {
		rec := serveAdmin(srv, newInitRequest(`{"password":`))
		decodeAdminFailure(t, rec, http.StatusBadRequest, codeBadRequest)
	}

	rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

	data := contractSuccessData(t, rec)
	if data["encryption_key"] == "" {
		t.Fatalf("合法初始化应返回一次性密钥，实际 %#v", data["encryption_key"])
	}
	if _, found := store.snapshotUser(); !found {
		t.Fatal("40001 不该计入失败限流，第 6 次应能成功写入管理员")
	}
}

// TestAdminInitKeyExistsDoesNotCountTowardFailureLimit 锁住同一口径的另一面：
// 连续 5 次 40903 之后，第 6 次仍是 40903（不是 42901），也不应出现 Retry-After。
// 40903 是状态冲突而非可猜的凭据，把它计入失败限流会让合法的运维重试被锁在门外。
func TestAdminInitKeyExistsDoesNotCountTowardFailureLimit(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	if _, err := keys.Create(); err != nil {
		t.Fatalf("准备密钥失败: %v", err)
	}
	srv := newAdminTestServer(t, store, keys)

	// 阈值是 5：若 40903 被计入，第 6 次一定变成 429，所以这里多打一次。
	for i := 0; i < failureLimitMaxFails+1; i++ {
		rec := serveAdmin(srv, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))

		decodeAdminFailure(t, rec, http.StatusConflict, codeKeyExists)
		assertNoStore(t, rec)
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Fatalf("第 %d 次密钥冲突不该触发失败限流，Retry-After=%q", i+1, got)
		}
	}
	if _, createCalls, _ := store.snapshotCalls(); createCalls != 0 {
		t.Fatalf("密钥冲突不该写管理员，实际 CreateAdmin %d 次", createCalls)
	}
}

// TestAdminInitConcurrentSingleWinner 覆盖并发初始化：50 个请求同时打进来，
// 恰好 1 个 200、其余全部 40902（不能出现 429——40902 不计入失败限流），
// 密钥文件只出现一次且内容等于成功响应的 encryption_key（CP3、CP4、Requirement 1.4）。
func TestAdminInitConcurrentSingleWinner(t *testing.T) {
	store := newAdminRuntimeStore()
	keys := newTestKeyManager(t)
	srv := newAdminTestServer(t, store, keys)
	handler := srv.Handler()

	const concurrency = 50
	body := initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))

	type result struct {
		status int
		body   []byte
	}
	results := make([]result, concurrency)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/api/admin/init", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			results[i] = result{status: rec.Code, body: rec.Body.Bytes()}
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	conflicts := 0
	var winnerData map[string]any
	for i, got := range results {
		var envelope struct {
			Code int            `json:"code"`
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(got.body, &envelope); err != nil {
			t.Fatalf("第 %d 个响应不是合法 JSON: %v, body=%s", i, err, got.body)
		}
		switch got.status {
		case http.StatusOK:
			successes++
			if envelope.Code != 0 {
				t.Fatalf("成功响应业务码期望 0，实际 %d", envelope.Code)
			}
			winnerData = envelope.Data
		case http.StatusConflict:
			conflicts++
			if envelope.Code != codeAdminExists {
				t.Fatalf("冲突响应业务码期望 %d，实际 %d", codeAdminExists, envelope.Code)
			}
		default:
			t.Fatalf("第 %d 个响应期望 200 或 409，实际 %d（body=%s）", i, got.status, got.body)
		}
	}

	if successes != 1 {
		t.Fatalf("并发初始化应恰好 1 个成功，实际 %d 个", successes)
	}
	if conflicts != concurrency-1 {
		t.Fatalf("其余 %d 个应全部 40902，实际 %d 个", concurrency-1, conflicts)
	}

	key, err := keys.Current()
	if err != nil {
		t.Fatalf("成功后密钥应已装载: %v", err)
	}
	if winnerData["encryption_key"] != key.Export() {
		t.Fatalf("成功响应里的密钥与内存密钥不一致: %#v", winnerData["encryption_key"])
	}
	raw, err := os.ReadFile(keys.Path())
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if string(raw) != key.Export()+"\n" {
		t.Fatalf("密钥文件内容不符: %q", string(raw))
	}
}

// TestAdminInitWithoutDependencies 覆盖未装配：缺数据来源或缺密钥管理器都按 50000 返回，
// 让 main.go 漏装配立刻暴露（design「未装配 WithAdmin」约束）。
func TestAdminInitWithoutDependencies(t *testing.T) {
	srvWithoutAdmin := New(quietLogger(), nil, nil, nil, nil)
	srvWithoutAdmin.clock = func() time.Time { return testAdminNow }

	rec := serveAdmin(srvWithoutAdmin, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)

	store := newAdminRuntimeStore()
	srvWithoutKeys := newAdminTestServer(t, store, nil)
	rec = serveAdmin(srvWithoutKeys, newInitRequest(initJSON(t, ptr(testAdminPassword), ptr(testAdminPassword))))
	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	if _, createCalls, _ := store.snapshotCalls(); createCalls != 0 {
		t.Fatalf("缺密钥管理器时不该写管理员，实际 %d 次", createCalls)
	}
}
