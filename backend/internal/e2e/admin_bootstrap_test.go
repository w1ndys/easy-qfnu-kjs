package e2e

// 本文件是 admin-bootstrap 的端到端检查点（tasklist 第 12 项）：
// 干净库迁移 → 初始化 → state → 登录 → 带/不带 CSRF 的写请求 → 改密 → 旧 Cookie 401 →
// 登出 → 清管理员记录与密钥文件 → 重新初始化。
//
// 装配口径照抄 cmd/server/main.go：store.AdminRepo 经 api.WithAdmin 注入，密钥文件落在
// t.TempDir() 下的真实文件上，其余依赖（探活、公开查询）传 nil——本检查点只打管理端五个端点。
// Cookie 的 Secure 显式关掉（等价于本地 http 开发）：httptest 走 http，带 Secure 的 Cookie
// 不会被浏览器/客户端回传，留着它反而测不了「Cookie → 会话」这一段。
//
// 与 collect_pipeline_test.go 一样只在设置 KJS_TEST_DSN 时运行，并且会重建 public schema；
// 带 DSN 跑全量测试必须固定 `-p 1`（原因见该文件的包注释）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

const (
	// adminE2EPassword 是首次初始化设置的密码；adminE2ENewPassword 是改密后的新密码。
	// 两个都超过 12 码点且互不相同：改密要能区分「旧密码还能不能登录」。
	adminE2EPassword    = "e2e-admin-password-9f3c"
	adminE2ENewPassword = "e2e-admin-new-password-5b21"

	// adminE2ECookieName 是管理会话 Cookie 名，与 internal/api 的部署契约一致。
	adminE2ECookieName = "eqk_admin_session"
)

// openAdminE2ETestDB 连真实 PostgreSQL 并重建 schema，然后迁移到最新版本。
// 未设置 KJS_TEST_DSN 时跳过；库里除了迁移种子之外不种任何业务数据，管理员与会话都由本用例产生。
func openAdminE2ETestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过管理员引导端到端检查点")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	// 从干净库开始：0004 的 admin_session 必须由本次迁移建出来，而不是残留自上一次运行。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// adminE2EClient 把请求构造与路由执行收在一处，让检查点正文只表达业务步骤。
type adminE2EClient struct {
	t       *testing.T
	handler http.Handler
}

// do 执行一个管理端请求并返回响应记录。
// cookie 为 nil 表示未登录；csrf 为空表示不带 X-CSRF-Token。
// 非 GET 请求一律带 Content-Type: application/json：管理端写链路（含没有请求体的 DELETE）
// 会先过体门，缺这个头会直接 40001，测不到后面的会话与 CSRF 校验。
func (c *adminE2EClient) do(method, path string, payload any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	c.t.Helper()

	var body *bytes.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			c.t.Fatalf("构造 %s %s 请求体失败: %v", method, path, err)
		}
		body = bytes.NewReader(raw)
	} else {
		body = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, body)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

// decodeE2ESuccess 断言 200 + 业务码 0，并返回 data 对象。
func decodeE2ESuccess(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	body := decodeE2EEnvelope(t, rec)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 HTTP 200，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("期望业务码 0，实际 %d（body=%s）", body.Code, rec.Body.String())
	}
	data, ok := body.Data.(map[string]any)
	if !ok {
		t.Fatalf("成功响应的 data 不是对象: %#v", body.Data)
	}
	return data
}

// decodeE2EFailure 断言失败响应的 HTTP 状态与业务码，并确认 data 为 null。
func decodeE2EFailure(t *testing.T, rec *httptest.ResponseRecorder, wantStatus, wantCode int) {
	t.Helper()

	body := decodeE2EEnvelope(t, rec)
	if rec.Code != wantStatus {
		t.Fatalf("期望 HTTP %d，实际 %d（body=%s）", wantStatus, rec.Code, rec.Body.String())
	}
	if body.Code != wantCode {
		t.Fatalf("期望业务码 %d，实际 %d（body=%s）", wantCode, body.Code, rec.Body.String())
	}
	if body.Data != nil {
		t.Fatalf("失败响应 data 期望 null，实际 %#v", body.Data)
	}
}

// e2eEnvelope 是统一响应信封的最小形状。
type e2eEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// decodeE2EEnvelope 解析统一响应信封，并断言 message 非空。
func decodeE2EEnvelope(t *testing.T, rec *httptest.ResponseRecorder) e2eEnvelope {
	t.Helper()

	var body e2eEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%s）", err, rec.Body.String())
	}
	if body.Message == "" {
		t.Fatalf("响应缺少 message: %s", rec.Body.String())
	}
	return body
}

// e2eString 取 data 里的字符串字段，缺失或类型不符直接失败。
func e2eString(t *testing.T, data map[string]any, field string) string {
	t.Helper()

	value, ok := data[field].(string)
	if !ok || value == "" {
		t.Fatalf("data.%s 期望非空字符串，实际 %#v", field, data[field])
	}
	return value
}

// e2eSessionCookie 取出响应里下发的管理会话 Cookie。
func e2eSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != adminE2ECookieName {
			continue
		}
		if cookie.Value == "" {
			t.Fatalf("%s 不该是清除指令（Value 为空）", adminE2ECookieName)
		}
		return cookie
	}
	t.Fatalf("缺少会话 Cookie 的 Set-Cookie，实际头: %v", rec.Header())
	return nil
}

// e2eAssertClearedCookie 断言响应带清除会话 Cookie 的 Set-Cookie：
// 同名、无值、Max-Age=0（Go 解析为 MaxAge<0）。
func e2eAssertClearedCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name != adminE2ECookieName {
			continue
		}
		if cookie.Value != "" {
			t.Fatalf("清除 Cookie 不该带值，实际 %q", cookie.Value)
		}
		if cookie.MaxAge >= 0 {
			t.Fatalf("清除 Cookie 期望 Max-Age=0，实际 MaxAge=%d", cookie.MaxAge)
		}
		return
	}
	t.Fatalf("缺少清除会话 Cookie 的 Set-Cookie，实际头: %v", rec.Header())
}

// e2ePasswordHash 读唯一的密码哈希；没有管理员行时直接失败。
func e2ePasswordHash(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	var hash string
	if err := pool.QueryRow(ctx, "SELECT password_hash FROM admin_user WHERE id = 1").Scan(&hash); err != nil {
		t.Fatalf("读取管理员密码哈希失败: %v", err)
	}
	return hash
}

// e2eAssertPHC 断言哈希串是设计约定的 argon2id 参数串（Requirement 2.3）。
func e2eAssertPHC(t *testing.T, hash, when string) {
	t.Helper()

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("%s的密码哈希不是约定的 argon2id PHC 串: %q", when, hash)
	}
}

// e2eAssertKeyFile 断言密钥文件是 0600、内容是 init 响应里那把密钥的 base64url + 换行。
// 失败信息只报字节数与长度，不把密钥材料打进测试输出。
func e2eAssertKeyFile(t *testing.T, path, exported string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件不存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限期望 0600，实际 %#o", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if got, want := string(raw), exported+"\n"; got != want {
		t.Fatalf("密钥文件内容与 init 响应不一致（文件 %d 字节，导出串 %d 字符）", len(got), len(want))
	}
}

// TestAdminBootstrapAgainstRealDB 是管理员引导的端到端检查点。
// 每一步都按 tasklist 第 12 项的顺序推进，任一步失败都说明链路有一段没接上。
func TestAdminBootstrapAgainstRealDB(t *testing.T) {
	ctx, pool := openAdminE2ETestDB(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 密钥管理器指向临时目录里的真实文件：检查点不碰宿主机的 /var/lib，清理时随 TempDir 回收。
	keys := secret.NewManager(filepath.Join(t.TempDir(), "secret.key"))
	loaded, err := keys.Load()
	if err != nil {
		t.Fatalf("干净目录里 Load 不该报错: %v", err)
	}
	if loaded {
		t.Fatal("干净目录里不该装载出密钥")
	}

	// 与 cmd/server/main.go 同口径装配：AdminRepo 是 store 的值类型，结构匹配 api 的消费侧接口。
	repo := store.NewAdminRepo(pool)
	handler := api.New(logger, nil, nil, nil, nil,
		api.WithAdmin(repo, keys),
		api.WithAdminCookieSecure(false),
	).Handler()
	client := &adminE2EClient{t: t, handler: handler}

	// ---------- 0. 干净库已迁移到 0004 ----------
	var migrated bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migration WHERE version = '0004_admin_session')").Scan(&migrated); err != nil {
		t.Fatalf("查询迁移记录失败: %v", err)
	}
	if !migrated {
		t.Fatal("schema_migration 里没有 0004_admin_session，干净库迁移没跑完")
	}
	assertCount(ctx, t, pool, 0, "干净库的管理员行数", "SELECT count(*) FROM admin_user")

	// ---------- 1. 未初始化：state 报 initialized=false ----------
	state := decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/state", nil, nil, ""))
	if state["initialized"] != false || state["authenticated"] != false {
		t.Fatalf("未初始化时 state 期望 initialized=false / authenticated=false，实际 %#v", state)
	}
	for _, field := range []string{"expires_at", "csrf_token", "key_fingerprint"} {
		if state[field] != nil {
			t.Fatalf("未登录时 %s 期望 null，实际 %#v", field, state[field])
		}
	}

	// ---------- 2. 初始化：拿一次性明文密钥与指纹 ----------
	initRec := client.do(http.MethodPost, "/api/admin/init", map[string]string{
		"password":         adminE2EPassword,
		"password_confirm": adminE2EPassword,
	}, nil, "")
	initData := decodeE2ESuccess(t, initRec)
	encryptionKey := e2eString(t, initData, "encryption_key")
	// export 是 43 字符的 base64url 无填充文本，解码后必须正好是 32 字节密钥。
	rawKey, err := base64.RawURLEncoding.DecodeString(encryptionKey)
	if err != nil || len(rawKey) != secret.KeyLen {
		t.Fatalf("encryption_key 不是 32 字节 base64url（解码 %d 字节，err=%v）", len(rawKey), err)
	}
	if initData["key_algorithm"] != "AES-256-GCM" {
		t.Fatalf("key_algorithm 期望 AES-256-GCM，实际 %#v", initData["key_algorithm"])
	}
	fingerprint := e2eString(t, initData, "key_fingerprint")
	if len(fingerprint) != 16 {
		t.Fatalf("key_fingerprint 期望 16 位十六进制，实际 %q", fingerprint)
	}
	e2eAssertKeyFile(t, keys.Path(), encryptionKey)
	// 一次性导出的响应绝不能进缓存。
	if got := initRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("init 响应期望 Cache-Control: no-store，实际 %q", got)
	}
	if got := initRec.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("init 响应期望 Pragma: no-cache，实际 %q", got)
	}

	// ---------- 3. 初始化后 state：initialized=true、仍未登录 ----------
	state = decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/state", nil, nil, ""))
	if state["initialized"] != true || state["authenticated"] != false {
		t.Fatalf("初始化后 state 期望 initialized=true / authenticated=false，实际 %#v", state)
	}

	// ---------- 4. 登录：服务端会话可用，Cookie 属性齐全 ----------
	loginRec := client.do(http.MethodPost, "/api/admin/session", map[string]string{
		"password": adminE2EPassword,
	}, nil, "")
	loginData := decodeE2ESuccess(t, loginRec)
	csrfToken := e2eString(t, loginData, "csrf_token")
	expiresAt := e2eString(t, loginData, "expires_at")
	cookie := e2eSessionCookie(t, loginRec)

	if !cookie.HttpOnly || cookie.Secure {
		t.Fatalf("会话 Cookie 期望 HttpOnly=true / Secure=false（检查点关了 Secure），实际 %+v", cookie)
	}
	if cookie.Path != "/api/admin" {
		t.Fatalf("会话 Cookie 的 Path 期望 /api/admin，实际 %q", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("会话 Cookie 的 SameSite 期望 Lax，实际 %v", cookie.SameSite)
	}
	if cookie.MaxAge != int(model.SessionTTL/time.Second) {
		t.Fatalf("会话 Cookie 的 Max-Age 期望 %d，实际 %d",
			int(model.SessionTTL/time.Second), cookie.MaxAge)
	}

	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("expires_at 不是 RFC3339: %q（err=%v）", expiresAt, err)
	}
	// 时钟取进程真实时间，这里给足分钟级余量，只钉「12 小时绝对过期」这个量级。
	if remaining := time.Until(expiry); remaining <= 11*time.Hour || remaining > model.SessionTTL+time.Minute {
		t.Fatalf("expires_at 距当前时刻期望约 12 小时，实际 %s", remaining)
	}

	assertCount(ctx, t, pool, 1, "登录后的管理会话行数", "SELECT count(*) FROM admin_session")
	oldHash := e2ePasswordHash(ctx, t, pool)
	e2eAssertPHC(t, oldHash, "初始化")

	// ---------- 5. 带 Cookie 的 state：登录态、CSRF 与密钥指纹 ----------
	authState := decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/state", nil, cookie, ""))
	if authState["initialized"] != true || authState["authenticated"] != true {
		t.Fatalf("登录后 state 期望 initialized=true / authenticated=true，实际 %#v", authState)
	}
	if authState["csrf_token"] != csrfToken {
		t.Fatalf("state 回传的 csrf_token 与登录响应不一致: %#v vs %q", authState["csrf_token"], csrfToken)
	}
	if authState["expires_at"] != expiresAt {
		t.Fatalf("state 回传的 expires_at 与登录响应不一致: %#v vs %q", authState["expires_at"], expiresAt)
	}
	if authState["key_fingerprint"] != fingerprint {
		t.Fatalf("state 回传的 key_fingerprint 与 init 响应不一致: %#v vs %q",
			authState["key_fingerprint"], fingerprint)
	}

	// ---------- 6. 不带 CSRF（以及带错 CSRF）的写请求必须被挡在业务之前 ----------
	changeBody := map[string]string{
		"current_password":     adminE2EPassword,
		"new_password":         adminE2ENewPassword,
		"new_password_confirm": adminE2ENewPassword,
	}
	decodeE2EFailure(t, client.do(http.MethodPost, "/api/admin/password", changeBody, cookie, ""),
		http.StatusForbidden, 40301)
	decodeE2EFailure(t, client.do(http.MethodPost, "/api/admin/password", changeBody, cookie, "wrong-csrf-token"),
		http.StatusForbidden, 40301)
	if got := e2ePasswordHash(ctx, t, pool); got != oldHash {
		t.Fatal("CSRF 校验失败时不该改动密码哈希")
	}
	assertCount(ctx, t, pool, 1, "CSRF 失败后的管理会话行数", "SELECT count(*) FROM admin_session")

	// ---------- 7. 带 CSRF 改密：哈希变化、会话全部收回、Cookie 被清 ----------
	changeRec := client.do(http.MethodPost, "/api/admin/password", changeBody, cookie, csrfToken)
	changeData := decodeE2ESuccess(t, changeRec)
	if changeData["relogin_required"] != true {
		t.Fatalf("改密成功期望 relogin_required=true，实际 %#v", changeData)
	}
	e2eAssertClearedCookie(t, changeRec)
	assertCount(ctx, t, pool, 0, "改密后的管理会话行数", "SELECT count(*) FROM admin_session")

	newHash := e2ePasswordHash(ctx, t, pool)
	if newHash == oldHash {
		t.Fatal("改密后密码哈希必须变化")
	}
	e2eAssertPHC(t, newHash, "改密")

	// ---------- 8. 改密前的 Cookie 与旧密码都不能再用 ----------
	decodeE2EFailure(t, client.do(http.MethodPost, "/api/admin/password", changeBody, cookie, csrfToken),
		http.StatusUnauthorized, 40101)
	decodeE2EFailure(t, client.do(http.MethodPost, "/api/admin/session",
		map[string]string{"password": adminE2EPassword}, nil, ""),
		http.StatusUnauthorized, 40101)

	// ---------- 9. 新密码登录 ----------
	newLoginRec := client.do(http.MethodPost, "/api/admin/session", map[string]string{
		"password": adminE2ENewPassword,
	}, nil, "")
	newLoginData := decodeE2ESuccess(t, newLoginRec)
	newCSRFToken := e2eString(t, newLoginData, "csrf_token")
	newCookie := e2eSessionCookie(t, newLoginRec)
	assertCount(ctx, t, pool, 1, "新密码登录后的管理会话行数", "SELECT count(*) FROM admin_session")

	// ---------- 10. 登出：删记录、清 Cookie，旧 Cookie 立刻 401 ----------
	logoutRec := client.do(http.MethodDelete, "/api/admin/session", nil, newCookie, newCSRFToken)
	logoutData := decodeE2ESuccess(t, logoutRec)
	if logoutData["logged_out"] != true {
		t.Fatalf("退出响应期望 logged_out=true，实际 %#v", logoutData)
	}
	e2eAssertClearedCookie(t, logoutRec)
	assertCount(ctx, t, pool, 0, "退出后的管理会话行数", "SELECT count(*) FROM admin_session")
	decodeE2EFailure(t, client.do(http.MethodDelete, "/api/admin/session", nil, newCookie, newCSRFToken),
		http.StatusUnauthorized, 40101)

	// ---------- 11. 清管理员记录与密钥文件：回到未初始化 ----------
	if err := repo.DeleteAdmin(ctx); err != nil {
		t.Fatalf("删除管理员记录失败: %v", err)
	}
	if err := os.Remove(keys.Path()); err != nil {
		t.Fatalf("删除密钥文件失败: %v", err)
	}
	// 这一步专门钉住「文件已删、内存里还留着旧密钥」：重新初始化必须靠 Reload 清空内存副本，
	// 否则 Create 恒撞 ErrKeyExists，部署会被永久卡在 40903。
	if !keys.Present() {
		t.Fatal("删除文件不该清空内存里已装载的密钥（加载期语义），这里应仍为 true")
	}
	state = decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/state", nil, nil, ""))
	if state["initialized"] != false {
		t.Fatalf("删除管理员记录后 state 期望 initialized=false，实际 %#v", state)
	}

	// ---------- 12. 重新初始化：接受请求并生成一把全新的密钥 ----------
	reInitRec := client.do(http.MethodPost, "/api/admin/init", map[string]string{
		"password":         adminE2ENewPassword,
		"password_confirm": adminE2ENewPassword,
	}, nil, "")
	reInitData := decodeE2ESuccess(t, reInitRec)
	newEncryptionKey := e2eString(t, reInitData, "encryption_key")
	if newEncryptionKey == encryptionKey {
		t.Fatal("重新初始化必须生成新密钥，不能复用内存里的旧副本")
	}
	e2eAssertKeyFile(t, keys.Path(), newEncryptionKey)

	state = decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/state", nil, nil, ""))
	if state["initialized"] != true || state["authenticated"] != false {
		t.Fatalf("重新初始化后 state 期望 initialized=true / authenticated=false，实际 %#v", state)
	}

	// ---------- 13. 端到端再兜一次脱敏：失败路径的日志不含任何凭据 ----------
	rawLog := logBuf.String()
	for _, secretValue := range []struct {
		label string
		value string
	}{
		{"初始密码", adminE2EPassword},
		{"新密码", adminE2ENewPassword},
		{"第一条会话 Cookie", cookie.Value},
		{"第一条会话 csrf_token", csrfToken},
		{"第二条会话 Cookie", newCookie.Value},
		{"第二条会话 csrf_token", newCSRFToken},
		{"首次加密密钥", encryptionKey},
		{"重新初始化的加密密钥", newEncryptionKey},
	} {
		if strings.Contains(rawLog, secretValue.value) {
			t.Fatalf("端到端日志泄露了%s", secretValue.label)
		}
	}
}
