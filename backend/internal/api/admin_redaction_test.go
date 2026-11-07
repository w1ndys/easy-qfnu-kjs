package api

// 本文件覆盖 tasklist 第 12 项的日志脱敏检查点（Requirement 9.1、CP2）：
// 用互不相同的哨兵密码走完 init → 登录 → 改密 → 再登录 → 退出，并夹带登录失败、
// 无 Cookie 写、错误 CSRF、旧 Cookie 写等会真正写日志的失败路径；最后断言整段 slog 输出
// 不含任何一次提交的密码、Cookie 原文、csrf_token 与 encryption_key。
//
// 为什么要把「写过的日志非空」也一起断言：管理端成功路径不记日志，如果整个流程一条日志都没写，
// 脱敏断言就成了空集上的恒真命题，漏记日志反而不会被发现。这里用几条确定的失败路径
// （40101 都走 adminFail）保证日志里确实有内容，再在这个前提下检查不泄露。

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	// 三个哨兵密码互不相同、长度都超过 12 码点。任一段被写进日志，都会整串出现在 JSON 行里。
	redactionInitPassword  = "sentinel-init-pw-3a7c9f"
	redactionNewPassword   = "sentinel-new-pw-b4e2d8"
	redactionWrongPassword = "sentinel-wrong-pw-6c1a5d"

	// redactionWrongCSRF 是客户端提交的错误令牌：403 走 s.fail、不记日志，这里一并断言它没有漏进去。
	redactionWrongCSRF = "sentinel-csrf-8d4b"
)

// TestAdminLifecycleLogRedaction 是日志脱敏检查点：真实路由、真实密钥文件、替身存储，
// 时间固定在 testAdminNow，整段 slog 输出收集到内存 buffer 后逐项断言。
func TestAdminLifecycleLogRedaction(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := newAdminRuntimeStore()
	srv := New(logger, nil, nil, nil, nil, WithAdmin(store, newTestKeyManager(t)))
	srv.clock = func() time.Time { return testAdminNow }

	// 1. 初始化：encryption_key 的明文只在这一次响应里出现，之后任何日志都不许有它。
	initData := contractSuccessData(t, serveAdmin(srv, newInitRequest(
		initJSON(t, ptr(redactionInitPassword), ptr(redactionInitPassword)))))
	encryptionKey, _ := initData["encryption_key"].(string)
	if encryptionKey == "" {
		t.Fatalf("初始化响应缺少 encryption_key: %#v", initData)
	}

	// 2. 登录失败一次：40101 走 adminFail，是真正会写日志的失败路径；提交的哨兵密码不能被记下。
	decodeAdminFailure(t,
		serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(redactionWrongPassword)))),
		http.StatusUnauthorized, codeSessionInvalid)

	// 3. 正常登录：Cookie 原文只出现在 Set-Cookie，csrf_token 只出现在响应体。
	loginRec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(redactionInitPassword))))
	loginData := contractSuccessData(t, loginRec)
	csrfToken, _ := loginData["csrf_token"].(string)
	if csrfToken == "" {
		t.Fatalf("登录响应缺少 csrf_token: %#v", loginData)
	}
	cookie := loginSessionCookie(t, loginRec)

	passwordBody := passwordJSON(t,
		ptr(redactionInitPassword), ptr(redactionNewPassword), ptr(redactionNewPassword))

	// 4. 无 Cookie 的写请求：40101 并清 Cookie，同样写一条日志。
	decodeAdminFailure(t, serveAdmin(srv, newAdminWriteRequest(t, passwordBody, "", "")),
		http.StatusUnauthorized, codeSessionInvalid)

	// 5. 有 Cookie、CSRF 错误的写请求：40301 走 s.fail（不记日志），但令牌本身也不许出现在任何行里。
	decodeAdminFailure(t,
		serveAdmin(srv, newAdminWriteRequest(t, passwordBody, cookie.Value, redactionWrongCSRF)),
		http.StatusForbidden, codeCSRFInvalid)

	// 6. 改密成功：新旧两个哨兵密码都经过这条链路，响应只回 relogin_required。
	passwordRec := serveAdmin(srv, newAdminWriteRequest(t, passwordBody, cookie.Value, csrfToken))
	passwordData := contractSuccessData(t, passwordRec)
	if passwordData["relogin_required"] != true {
		t.Fatalf("改密成功期望 relogin_required=true，实际 %#v", passwordData)
	}
	assertClearedCookie(t, passwordRec, true)

	// 7. 改密前签发的 Cookie 再打一次写端点：40101 并写日志，验证「旧票失效」这条路径也不泄露。
	decodeAdminFailure(t,
		serveAdmin(srv, newAdminWriteRequest(t, passwordBody, cookie.Value, csrfToken)),
		http.StatusUnauthorized, codeSessionInvalid)

	// 8. 用新密码重新登录，再退出：把第二条会话的 Cookie 与 csrf_token 也纳入脱敏范围。
	newLoginRec := serveAdmin(srv, newLoginRequest(t, loginJSON(t, ptr(redactionNewPassword))))
	newLoginData := contractSuccessData(t, newLoginRec)
	newCSRFToken, _ := newLoginData["csrf_token"].(string)
	if newCSRFToken == "" {
		t.Fatalf("新密码登录响应缺少 csrf_token: %#v", newLoginData)
	}
	newCookie := loginSessionCookie(t, newLoginRec)

	logoutData := contractSuccessData(t, serveAdmin(srv, newLogoutRequest(newCookie, newCSRFToken)))
	if logoutData["logged_out"] != true {
		t.Fatalf("退出响应期望 logged_out=true，实际 %#v", logoutData)
	}

	// ---------- 断言：日志非空、字段白名单、且不含任何凭据 ----------
	rawLog := strings.TrimSpace(logBuf.String())
	if rawLog == "" {
		t.Fatal("整段流程一条日志都没有，脱敏断言会在空集上恒真，先保证失败路径确实留痕")
	}
	lines := strings.Split(rawLog, "\n")

	// 每条日志只能是白名单字段：level/msg/time 是 slog 自带，其余五项是 Requirement 9.1 允许的业务字段。
	allowed := map[string]bool{
		"level": true, "msg": true, "time": true,
		"request_id": true, "method": true, "path": true, "status": true, "code": true,
	}
	for i, line := range lines {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("第 %d 条日志不是合法 JSON: %v", i+1, err)
		}
		for field := range fields {
			if !allowed[field] {
				t.Fatalf("第 %d 条日志出现白名单外字段 %q", i+1, field)
			}
		}
		var entry adminLogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("第 %d 条日志解析失败: %v", i+1, err)
		}
		// 失败路径的日志必须带齐五项业务字段，不能靠「只写了 msg」蒙混过关。
		if entry.RequestID == "" || entry.Method == "" || entry.Path == "" || entry.Status == 0 || entry.Code == 0 {
			t.Fatalf("第 %d 条日志缺少白名单业务字段: %+v", i+1, entry)
		}
	}

	// 断言不泄露时只报标签与行号，不打印日志内容：失败输出本身也不该把凭据带出去。
	forbidden := []struct {
		label string
		value string
	}{
		{"初始化密码", redactionInitPassword},
		{"新密码", redactionNewPassword},
		{"错误密码", redactionWrongPassword},
		{"错误 CSRF 令牌", redactionWrongCSRF},
		{"第一条会话 Cookie", cookie.Value},
		{"第一条会话 csrf_token", csrfToken},
		{"第二条会话 Cookie", newCookie.Value},
		{"第二条会话 csrf_token", newCSRFToken},
		{"encryption_key", encryptionKey},
	}
	for _, secret := range forbidden {
		if secret.value == "" {
			t.Fatalf("%s 的哨兵为空，断言会失去意义", secret.label)
		}
		for i, line := range lines {
			if strings.Contains(line, secret.value) {
				t.Fatalf("第 %d 条日志泄露了%s", i+1, secret.label)
			}
		}
	}
}
