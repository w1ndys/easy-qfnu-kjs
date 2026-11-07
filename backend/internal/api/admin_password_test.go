package api

// 本文件覆盖 tasklist 第 11 项：POST /api/admin/password 改密与全量会话失效
// （Requirement 8、CP8）。用例走真实路由（srv.Handler()），因此 adminWrite 的
// no-store → 体门 → 会话 → CSRF → 业务 这条链路、Cookie 属性与错误映射都由真实实现承担；
// 替身只负责数据来源，并记录改密两个写方法的入参与失败注入。
//
// 替身结构说明：adminStore 接口在本项补了 UpdatePassword 与 DeleteAllSessions，
// task 9/10 的 adminRuntimeStore 必须跟着满足接口，但它的字段只属于那两项用例。
// 所以这里给共享替身补两个语义正确的最小实现（不记录入参、不注入失败），
// 再由 passwordRuntimeStore 包一层做记录与注入——同一包不同文件给同一类型加方法是合法的，
// 这样就不必改动 task 9 的 admin_test.go。

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
	// testAdminNewPassword 是改密用例的新密码：能通过 auth.ValidatePassword，且与旧密码不同。
	testAdminNewPassword = "brand new correct horse battery staple"
	// otherDeviceRawToken 是「另一台设备」的会话 Cookie 原文：改密必须连它一起作废。
	otherDeviceRawToken = "another-device-session-token"
)

// UpdatePassword 让 task 9/10 的共享替身继续满足扩展后的 adminStore 接口。
// 语义与 store.AdminRepo 对齐：覆盖哈希、前移 password_updated_at，没有管理员记录时
// 返回 ErrAdminStoreFailed（与真实仓储的 0 行判错一致）。
// task 11 的用例不直接用它，而是用 passwordRuntimeStore 观察入参与注入失败。
func (s *adminRuntimeStore) UpdatePassword(_ context.Context, passwordHash string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.userFound {
		return model.ErrAdminStoreFailed
	}
	s.user.PasswordHash = passwordHash
	s.user.PasswordUpdatedAt = now
	return nil
}

// DeleteAllSessions 清空全部会话并返回行数，语义与 store.AdminRepo 一致（CP8）。
func (s *adminRuntimeStore) DeleteAllSessions(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := int64(len(s.sessions))
	clear(s.sessions)
	return removed, nil
}

// passwordRuntimeStore 在 adminRuntimeStore 之上记录改密两个写方法的入参，并可注入失败。
// 嵌入指针而不是复制字段：会话、管理员与锁仍由共享替身持有，行为只有这两个方法被覆盖。
type passwordRuntimeStore struct {
	*adminRuntimeStore

	updatePasswordErr error
	deleteAllErr      error

	updatePasswordCalls int
	gotUpdateHash       string
	gotUpdateNow        time.Time
	deleteAllCalls      int

	// deleteAllCtxErr / deleteAllCtxDeadline / deleteAllCtxHasDeadline 记录 DeleteAllSessions
	// 被调用时 ctx 的状态：客户端取消用例用它断言清理仍有一个未取消、带截止时间的 ctx
	// （与 admin_test.go 里 DeleteAdmin 的 deleteCtx* 同一形状）。
	deleteAllCtxErr         error
	deleteAllCtxDeadline    time.Time
	deleteAllCtxHasDeadline bool
}

// UpdatePassword 记录这次改密的哈希与时刻；updatePasswordErr 非 nil 时模拟仓储失败。
func (s *passwordRuntimeStore) UpdatePassword(_ context.Context, passwordHash string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updatePasswordCalls++
	s.gotUpdateHash = passwordHash
	s.gotUpdateNow = now
	if s.updatePasswordErr != nil {
		return s.updatePasswordErr
	}
	if !s.userFound {
		return model.ErrAdminStoreFailed
	}
	s.user.PasswordHash = passwordHash
	s.user.PasswordUpdatedAt = now
	return nil
}

// DeleteAllSessions 记录调用次数与 ctx 状态再清空会话；deleteAllErr 非 nil 时模拟仓储失败，
// 此时一条行都不删——真实仓储失败时也不会留下半删的状态。
// ctx 已取消时与真实仓储同口径：直接返回取消错误、一行不删。这样「取消信号被带进清理」
// 会真的表现为「会话没收回」，而不是被替身悄悄吞掉、让用例在错误的实现上也通过。
func (s *passwordRuntimeStore) DeleteAllSessions(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteAllCalls++
	s.deleteAllCtxErr = ctx.Err()
	s.deleteAllCtxDeadline, s.deleteAllCtxHasDeadline = ctx.Deadline()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.deleteAllErr != nil {
		return 0, s.deleteAllErr
	}
	removed := int64(len(s.sessions))
	clear(s.sessions)
	return removed, nil
}

// snapshotDeleteAllCtx 在锁内取 DeleteAllSessions 被调用时的 ctx 状态：
// 是否带截止时间、截止时间本身、是否已取消。
func (s *passwordRuntimeStore) snapshotDeleteAllCtx() (hasDeadline bool, deadline time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteAllCtxHasDeadline, s.deleteAllCtxDeadline, s.deleteAllCtxErr
}

// passwordSession 按 Cookie 原文生成一条会话：TokenHash 由 rawToken 现算，
// 调用方再用同一个摘要做 map 的 key。不能拿 activeAdminSession 充数——它固定按
// testAdminRawToken 算摘要，挂到「另一台设备」的 key 上会造出「key 与行内 TokenHash
// 不是同一个摘要」的假数据，真实仓储不会出现这种行，用例也就测不出全量失效是否真的按行删。
func passwordSession(rawToken string, expiresAt time.Time) model.AdminSession {
	return model.AdminSession{
		TokenHash: auth.HashSessionToken(rawToken),
		CSRFToken: testAdminCSRF,
		CreatedAt: testAdminNow.Add(-time.Hour),
		ExpiresAt: expiresAt,
	}
}

// newPasswordTestStore 返回「已初始化 + 两条有效会话」的替身：
// 当前请求用 testAdminRawToken，另一条模拟另一台设备上的登录（own rawToken、own 摘要）。
// password_updated_at 刻意设成一小时前：成功路径必须把它前移到注入时钟，
// 否则「前移」与「原样不动」在断言里分不开。
func newPasswordTestStore(t *testing.T) *passwordRuntimeStore {
	t.Helper()
	base := initializedAdminStore(t)
	base.user.PasswordUpdatedAt = testAdminNow.Add(-time.Hour)
	for rawToken, expiresAt := range map[string]time.Time{
		testAdminRawToken:   testAdminNow.Add(time.Hour),
		otherDeviceRawToken: testAdminNow.Add(2 * time.Hour),
	} {
		base.sessions[auth.HashSessionToken(rawToken)] = passwordSession(rawToken, expiresAt)
	}
	return &passwordRuntimeStore{adminRuntimeStore: base}
}

// passwordJSON 序列化改密请求体；传 nil 表示请求体里缺这个字段。
func passwordJSON(t *testing.T, current, newPassword, confirm *string) string {
	t.Helper()
	payload := map[string]*string{}
	if current != nil {
		payload["current_password"] = current
	}
	if newPassword != nil {
		payload["new_password"] = newPassword
	}
	if confirm != nil {
		payload["new_password_confirm"] = confirm
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("构造改密请求体失败: %v", err)
	}
	return string(raw)
}

// newPasswordRequestForDevice 用指定的 Cookie 原文构造「已登录」的改密请求：
// Content-Type 合法，带上该设备的会话 Cookie 与 CSRF 令牌。传 otherDeviceRawToken
// 就是另一台设备上的旧登录，用来验证全量失效不止删当前这一条。
func newPasswordRequestForDevice(t *testing.T, rawToken, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: rawToken})
	req.Header.Set(adminCSRFHeader, testAdminCSRF)
	return req
}

// newPasswordRequest 构造「当前设备已登录」的改密请求：Cookie 指向 testAdminRawToken。
// 要覆盖缺会话 / 缺 CSRF / 坏 Content-Type 时，由用例在这条请求上删改对应的头。
func newPasswordRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	return newPasswordRequestForDevice(t, testAdminRawToken, body)
}

// validPasswordRequest 构造一次字段齐全、当前密码正确的改密请求。
func validPasswordRequest(t *testing.T) *http.Request {
	t.Helper()
	return newPasswordRequest(t,
		passwordJSON(t, ptr(testAdminPassword), ptr(testAdminNewPassword), ptr(testAdminNewPassword)))
}

// ---------------------------------------------------------------------------
// 成功路径
// ---------------------------------------------------------------------------

// TestAdminPasswordChangeSuccess 覆盖改密成功（Requirement 8.1、8.3、8.4、CP8）：
//   - 200 + {relogin_required:true}、no-store、清除 Cookie；
//   - UpdatePassword 一次（argon2id PHC、时刻取注入时钟）、DeleteAllSessions 一次；
//   - admin_session 行数清零（当前设备与另一台设备都不留）；
//   - password_updated_at 前移，新哈希只认新密码、旧密码校验不过；
//   - 改密前签发的 Cookie 后续请求 40101 + 清 Cookie。
func TestAdminPasswordChangeSuccess(t *testing.T) {
	store := newPasswordTestStore(t)
	srv := newAdminTestServer(t, store, nil)
	oldHash := store.user.PasswordHash

	rec := serveAdmin(srv, validPasswordRequest(t))

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "改密 data", data, "relogin_required")
	if data["relogin_required"] != true {
		t.Fatalf("relogin_required 期望 true，实际 %#v", data["relogin_required"])
	}
	assertNoStore(t, rec)
	assertClearedCookie(t, rec, true)

	// 写库顺序与入参：任务规定 UpdatePassword → DeleteAllSessions，各一次。
	if store.updatePasswordCalls != 1 {
		t.Fatalf("UpdatePassword 期望 1 次，实际 %d 次", store.updatePasswordCalls)
	}
	if store.deleteAllCalls != 1 {
		t.Fatalf("DeleteAllSessions 期望 1 次，实际 %d 次", store.deleteAllCalls)
	}
	if !store.gotUpdateNow.Equal(testAdminNow) {
		t.Fatalf("password_updated_at 期望取注入时钟 %s，实际 %s", testAdminNow, store.gotUpdateNow)
	}
	if store.gotUpdateHash == oldHash {
		t.Fatal("改密必须写入新的哈希串，不能沿用旧串")
	}
	// PHC 前缀钉住口径（Requirement 2.3）：改密与初始化是同一套 argon2id 参数。
	if !strings.HasPrefix(store.gotUpdateHash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("新哈希不是约定的 PHC 串: %q", store.gotUpdateHash)
	}

	// 新哈希只认新密码；旧密码必须校验不过。
	if ok, err := auth.VerifyPassword(store.gotUpdateHash, testAdminNewPassword); err != nil || !ok {
		t.Fatalf("新哈希应能校验新密码，实际 ok=%t err=%v", ok, err)
	}
	if ok, err := auth.VerifyPassword(store.gotUpdateHash, testAdminPassword); err != nil || ok {
		t.Fatalf("新哈希不该再认旧密码，实际 ok=%t err=%v", ok, err)
	}

	// 库里（替身）的状态：哈希换新、password_updated_at 前移，created_at 不动。
	user, found := store.snapshotUser()
	if !found {
		t.Fatal("改密不该删除管理员记录")
	}
	if user.PasswordHash != store.gotUpdateHash {
		t.Fatalf("库里的哈希期望与写入一致 %q，实际 %q", store.gotUpdateHash, user.PasswordHash)
	}
	if !user.PasswordUpdatedAt.After(testAdminNow.Add(-time.Hour)) {
		t.Fatalf("password_updated_at 应前移，实际停在 %s", user.PasswordUpdatedAt)
	}
	if !user.PasswordUpdatedAt.Equal(testAdminNow) {
		t.Fatalf("password_updated_at 期望 %s，实际 %s", testAdminNow, user.PasswordUpdatedAt)
	}
	if !user.CreatedAt.Equal(testAdminNow) {
		t.Fatalf("改密不该改动 created_at，实际 %s", user.CreatedAt)
	}

	// admin_session 行数为 0（CP8）：当前会话与另一台设备的会话都不能留。
	if store.sessionCount() != 0 {
		t.Fatalf("改密后 admin_session 行数期望 0，实际 %d 行", store.sessionCount())
	}

	// 改密前签发的 Cookie 后续请求 40101：写链路先查会话，查不到就不进业务。
	replay := serveAdmin(srv, validPasswordRequest(t))
	failure := decodeAdminFailure(t, replay, http.StatusUnauthorized, codeSessionInvalid)
	if failure.Message != adminCredentialsInvalidMessage {
		t.Fatalf("旧 Cookie 的文案期望 %q，实际 %q", adminCredentialsInvalidMessage, failure.Message)
	}
	assertClearedCookie(t, replay, true)
	assertNoStore(t, replay)
	if store.updatePasswordCalls != 1 {
		t.Fatalf("旧 Cookie 不该再次改密，UpdatePassword 实际 %d 次", store.updatePasswordCalls)
	}

	// 另一台设备上的旧 Cookie 同样立即失效：收回的是全部会话，不只是当前这条（CP8）。
	// 这条断言与上一条查的是两个不同摘要，map 里各有一行，删一行不会连带让另一行「看起来失效」。
	otherReplay := serveAdmin(srv, newPasswordRequestForDevice(t, otherDeviceRawToken,
		passwordJSON(t, ptr(testAdminNewPassword), ptr(testAdminNewPassword), ptr(testAdminNewPassword))))
	otherFailure := decodeAdminFailure(t, otherReplay, http.StatusUnauthorized, codeSessionInvalid)
	if otherFailure.Message != adminCredentialsInvalidMessage {
		t.Fatalf("另一台设备的文案期望 %q，实际 %q", adminCredentialsInvalidMessage, otherFailure.Message)
	}
	assertClearedCookie(t, otherReplay, true)
	assertNoStore(t, otherReplay)
	if store.updatePasswordCalls != 1 || store.deleteAllCalls != 1 {
		t.Fatalf("另一台设备的旧 Cookie 不该再触发改密，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
}

// TestAdminPasswordSessionRevokeSurvivesClientCancel 钉住「收回会话不跟客户端走」：
// 请求 ctx 预先取消（客户端在响应返回前就断开）时，改密仍必须走到 DeleteAllSessions，
// 且传进去的 ctx 未被取消、仍带 adminCompensateTimeout 截止时间
// （context.WithoutCancel + context.WithTimeout，与 admin.go 的补偿同一做法）。
// 否则会出现「密码已经改了、会话却一条都没收回」的半成品：旧 Cookie 继续可用，
// 而 CP8 要求改密成功后 admin_session 行数为 0。
//
// 替身在 ctx 已取消时按真实仓储的行为返回取消错误（一行不删），所以这条用例在
// 退回「直接传 r.Context()」的实现上会变成 50000 且会话剩 2 条，不会假装通过。
func TestAdminPasswordSessionRevokeSurvivesClientCancel(t *testing.T) {
	store := newPasswordTestStore(t)
	srv := newAdminTestServer(t, store, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := validPasswordRequest(t).WithContext(ctx)

	rec := serveAdmin(srv, req)

	data := contractSuccessData(t, rec)
	if data["relogin_required"] != true {
		t.Fatalf("relogin_required 期望 true，实际 %#v", data["relogin_required"])
	}
	assertNoStore(t, rec)
	assertClearedCookie(t, rec, true)

	if store.updatePasswordCalls != 1 || store.deleteAllCalls != 1 {
		t.Fatalf("客户端断开也应按顺序各调用一次，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	hasDeadline, deadline, ctxErr := store.snapshotDeleteAllCtx()
	if ctxErr != nil {
		t.Fatalf("DeleteAllSessions 的 ctx 不该继承客户端的取消信号，实际 err=%v", ctxErr)
	}
	if !hasDeadline {
		t.Fatal("DeleteAllSessions 的 ctx 必须带截止时间，避免卡死的库把会话清理无限拖住")
	}
	// 请求已经返回，剩余时间应接近 adminCompensateTimeout；+1s 余量只容忍调度与测量误差。
	if remaining := time.Until(deadline); remaining <= 0 || remaining > adminCompensateTimeout+time.Second {
		t.Fatalf("清理 ctx 剩余时间应落在 (0, %s]，实际 %s", adminCompensateTimeout+time.Second, remaining)
	}
	if store.sessionCount() != 0 {
		t.Fatalf("客户端断开也必须收回全部会话，实际还剩 %d 条", store.sessionCount())
	}
}

// TestAdminPasswordAcceptsLegacyShortCurrentPassword 锁住「只对新密码套 ValidatePassword」这条拍板：
// 库里可能躺着规则收紧之前写入的短密码（本例 8 码点），它必须仍能作为当前密码校验通过。
// 若有人把 ValidatePassword 也套到当前密码上，这条会从 200 变成 42201，改密对老用户直接失效。
//
// 替身直接用 auth.HashPassword 写这个短串，绕过规则校验：真实库里的旧哈希就是这样落进去的，
// 不因为后来的规则变化而失效。
func TestAdminPasswordAcceptsLegacyShortCurrentPassword(t *testing.T) {
	const legacyShortPassword = "old-pass" // 8 码点，当前规则会以「长度不足」拒绝

	store := newPasswordTestStore(t)
	legacyHash, err := auth.HashPassword(legacyShortPassword)
	if err != nil {
		t.Fatalf("准备旧密码哈希失败: %v", err)
	}
	store.user.PasswordHash = legacyHash
	srv := newAdminTestServer(t, store, nil)

	rec := serveAdmin(srv, newPasswordRequest(t,
		passwordJSON(t, ptr(legacyShortPassword), ptr(testAdminNewPassword), ptr(testAdminNewPassword))))

	data := contractSuccessData(t, rec)
	if data["relogin_required"] != true {
		t.Fatalf("relogin_required 期望 true，实际 %#v", data["relogin_required"])
	}
	assertNoStore(t, rec)
	assertClearedCookie(t, rec, true)

	if store.updatePasswordCalls != 1 || store.deleteAllCalls != 1 {
		t.Fatalf("改密应各调用一次，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	if store.sessionCount() != 0 {
		t.Fatalf("改密成功后会话应清空，实际 %d 条", store.sessionCount())
	}
	// 真正的判定点：新哈希认新密码，不认那个 8 码点旧密码。
	if ok, err := auth.VerifyPassword(store.gotUpdateHash, testAdminNewPassword); err != nil || !ok {
		t.Fatalf("新哈希应能校验新密码，实际 ok=%t err=%v", ok, err)
	}
	if ok, err := auth.VerifyPassword(store.gotUpdateHash, legacyShortPassword); err != nil || ok {
		t.Fatalf("新哈希不该再认旧密码，实际 ok=%t err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// 当前密码错误
// ---------------------------------------------------------------------------

// TestAdminPasswordWrongCurrentPassword 覆盖当前密码错误（Requirement 8.2、错误表
// 「当前密码错误」一行）：401 + 40101、与登录失败同一段文案、不清 Cookie、
// 不改哈希、不动会话；会话仍然有效，所以随后 state 仍报 authenticated=true。
func TestAdminPasswordWrongCurrentPassword(t *testing.T) {
	store := newPasswordTestStore(t)
	srv := newAdminTestServer(t, store, nil)
	hashBefore := store.user.PasswordHash
	updatedBefore := store.user.PasswordUpdatedAt
	hashCallsBefore := auth.HashCallCount()

	rec := serveAdmin(srv, newPasswordRequest(t,
		passwordJSON(t, ptr(testAdminPassword+"-wrong"), ptr(testAdminNewPassword), ptr(testAdminNewPassword))))

	failure := decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	if failure.Message != adminCredentialsInvalidMessage {
		t.Fatalf("当前密码错误文案期望 %q，实际 %q", adminCredentialsInvalidMessage, failure.Message)
	}
	assertNoStore(t, rec)
	// 不清 Cookie：Requirement 8.2 要求保持会话不变，一次输错不该把人踢下线。
	assertNoSetCookie(t, rec)

	// 真的走了一次 VerifyPassword（相对计数，因为准备替身时已算过一次哈希）。
	if got := auth.HashCallCount() - hashCallsBefore; got != 1 {
		t.Fatalf("当前密码错误应恰好校验一次，实际 argon2 次数增加 %d", got)
	}
	if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
		t.Fatalf("当前密码错误不该写库，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	if store.user.PasswordHash != hashBefore {
		t.Fatal("当前密码错误时哈希必须保持不变")
	}
	if !store.user.PasswordUpdatedAt.Equal(updatedBefore) {
		t.Fatalf("当前密码错误时 password_updated_at 不该动，实际 %s", store.user.PasswordUpdatedAt)
	}
	if store.sessionCount() != 2 {
		t.Fatalf("当前密码错误时对话应保持 2 条，实际 %d 条", store.sessionCount())
	}

	// 会话没有被作废：同一个 Cookie 仍然能查到有效会话。
	stateRec := serveAdmin(srv, newStateRequest(stateCookie()))
	if data := contractSuccessData(t, stateRec); data["authenticated"] != true {
		t.Fatalf("当前密码错误不该影响会话有效性，实际 authenticated=%#v", data["authenticated"])
	}
}

// TestAdminPasswordCurrentPasswordCheckedFirst 钉住顺序（tasklist 第 11 项第一句）：
// 当前密码错误与新密码不合规同时出现时返回 40101，而不是 42201。
// 否则不知道当前密码的人也能靠 401/422 的差异判断「新密码规则是否满足」，前端提示也会串味。
func TestAdminPasswordCurrentPasswordCheckedFirst(t *testing.T) {
	store := newPasswordTestStore(t)
	srv := newAdminTestServer(t, store, nil)

	rec := serveAdmin(srv, newPasswordRequest(t,
		passwordJSON(t, ptr(testAdminPassword+"-wrong"),
			ptr(testAdminNewPassword), ptr(testAdminNewPassword+"-mismatch"))))

	decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
	if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
		t.Fatalf("当前密码错误不该写库，实际 %d / %d",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	if store.sessionCount() != 2 {
		t.Fatalf("会话应保持 2 条，实际 %d 条", store.sessionCount())
	}
}

// TestAdminPasswordHashMalformed 覆盖库里哈希串损坏（Requirement 2.5）：
// 没有可用的当前密码可比对，对外仍是 40101，且不写哈希、不删会话。
func TestAdminPasswordHashMalformed(t *testing.T) {
	store := newPasswordTestStore(t)
	store.user.PasswordHash = "not-a-phc-string"
	srv := newAdminTestServer(t, store, nil)

	rec := serveAdmin(srv, validPasswordRequest(t))

	decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
	if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
		t.Fatalf("哈希损坏不该写库，实际 %d / %d", store.updatePasswordCalls, store.deleteAllCalls)
	}
	if store.user.PasswordHash != "not-a-phc-string" {
		t.Fatalf("哈希损坏时应保持原串不动，实际 %q", store.user.PasswordHash)
	}
	if store.sessionCount() != 2 {
		t.Fatalf("会话应保持 2 条，实际 %d 条", store.sessionCount())
	}
}

// TestAdminPasswordWithoutAdminRecord 覆盖「有效会话却查不到管理员记录」：
// 没有哈希可校验，按凭据失败 40101 处理（与登录的「尚未初始化」同一业务码与文案、
// 却是不同出口：这里不清 Cookie、不计入登录限流），不写库、不删会话。
func TestAdminPasswordWithoutAdminRecord(t *testing.T) {
	base := newAdminRuntimeStore()
	base.sessions[auth.HashSessionToken(testAdminRawToken)] = passwordSession(testAdminRawToken, testAdminNow.Add(time.Hour))
	store := &passwordRuntimeStore{adminRuntimeStore: base}
	srv := newAdminTestServer(t, store, nil)
	sessionsBefore := store.sessionCount()

	rec := serveAdmin(srv, validPasswordRequest(t))

	failure := decodeAdminFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
	if failure.Message != adminCredentialsInvalidMessage {
		t.Fatalf("文案期望 %q，实际 %q", adminCredentialsInvalidMessage, failure.Message)
	}
	assertNoStore(t, rec)
	// 与登录的「尚未初始化」共用文案，但不共用出口：登录失败会清 Cookie，改密不清
	// （Requirement 8.2 要求保持会话不变）。
	assertNoSetCookie(t, rec)
	if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
		t.Fatalf("缺少管理员记录时不该写库，实际 %d / %d",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	// 会话条数不变：这条路径连 DeleteAllSessions 都不该走到（走了就会清零）。
	if store.sessionCount() != sessionsBefore {
		t.Fatalf("会话条数应保持 %d 条不变，实际 %d 条", sessionsBefore, store.sessionCount())
	}
}

// ---------------------------------------------------------------------------
// 新密码规则
// ---------------------------------------------------------------------------

// TestAdminPasswordRuleViolations 覆盖新密码规则（Requirement 2.1、2.2、错误表 42201）：
// 两次不一致、少于 12 码点、超过 256 码点都返回 422 + 42201，缺字段返回 400 + 40001。
// 每一种都必须「不写哈希、不删会话、不清 Cookie」——校验发生在任何写操作之前。
//
// 42201 的用例都带 wantMessage：init 与改密共用 passwordRuleMessage，文案本身就是契约
// （前端靠它提示用户改哪一栏），只断言业务码会放过「两种规则说同一句话」这类回归。
//
// 汉字那条断言按码点计：11 个汉字是 33 字节但只有 11 码点，按字节算会被误放行。
func TestAdminPasswordRuleViolations(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantStatus  int
		wantCode    int
		wantMessage string
	}{
		{
			"两次新密码不一致",
			passwordJSON(t, ptr(testAdminPassword), ptr(testAdminNewPassword), ptr(testAdminNewPassword+"-x")),
			http.StatusUnprocessableEntity, codePasswordInvalid, "两次密码不一致",
		},
		{
			"新密码 11 个码点",
			passwordJSON(t, ptr(testAdminPassword), ptr("12345678901"), ptr("12345678901")),
			http.StatusUnprocessableEntity, codePasswordInvalid, "密码长度不足 12 个字符",
		},
		{
			"新密码 11 个汉字（33 字节但只有 11 码点）",
			passwordJSON(t, ptr(testAdminPassword),
				ptr(strings.Repeat("密", 11)), ptr(strings.Repeat("密", 11))),
			http.StatusUnprocessableEntity, codePasswordInvalid, "密码长度不足 12 个字符",
		},
		{
			"新密码超过 256 码点",
			passwordJSON(t, ptr(testAdminPassword),
				ptr(strings.Repeat("a", 257)), ptr(strings.Repeat("a", 257))),
			http.StatusUnprocessableEntity, codePasswordInvalid, "密码过长",
		},
		{
			"缺少 new_password",
			passwordJSON(t, ptr(testAdminPassword), nil, ptr(testAdminNewPassword)),
			http.StatusBadRequest, codeBadRequest, "",
		},
		{
			"缺少 new_password_confirm",
			passwordJSON(t, ptr(testAdminPassword), ptr(testAdminNewPassword), nil),
			http.StatusBadRequest, codeBadRequest, "",
		},
		{
			"缺少 current_password",
			passwordJSON(t, nil, ptr(testAdminNewPassword), ptr(testAdminNewPassword)),
			http.StatusBadRequest, codeBadRequest, "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPasswordTestStore(t)
			srv := newAdminTestServer(t, store, nil)
			hashBefore := store.user.PasswordHash

			rec := serveAdmin(srv, newPasswordRequest(t, tc.body))

			failure := decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)
			// 42201 必须给出并核对文案；40001 的文案不在这里锁（由 decodeAdminFailure 保证非空）。
			if tc.wantCode == codePasswordInvalid {
				if tc.wantMessage == "" {
					t.Fatal("42201 用例必须给出 wantMessage")
				}
				if failure.Message != tc.wantMessage {
					t.Fatalf("文案期望 %q，实际 %q", tc.wantMessage, failure.Message)
				}
			}
			assertNoStore(t, rec)
			assertNoSetCookie(t, rec)
			if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
				t.Fatalf("规则不符时不该写库，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
					store.updatePasswordCalls, store.deleteAllCalls)
			}
			if store.user.PasswordHash != hashBefore {
				t.Fatal("规则不符时哈希必须保持不变")
			}
			if store.sessionCount() != 2 {
				t.Fatalf("规则不符时对话应保持 2 条，实际 %d 条", store.sessionCount())
			}
		})
	}
}

// TestAdminPasswordEmptyStrings 覆盖「字段存在但是空串」这类边界（Requirement 8.2、2.2）：
//   - 当前密码空串：不是缺字段（缺字段是 40001），而是一次确定的错误密码 → 401 + 40101，
//     且不清 Cookie、不写库、不动会话；
//   - 新密码空串：两次都是空串，先过一致性再去比长度 → 422 + 42201「密码长度不足 12 个字符」。
//
// 请求体用指针字段构造：passwordJSON 只在传 nil 时省略键，ptr("") 会真的写进一个空串。
// 两者都不会让新密码进入 argon2：最多只做一次「校验当前密码」，HashPassword 一次都不发生。
func TestAdminPasswordEmptyStrings(t *testing.T) {
	cases := []struct {
		name        string
		current     *string
		newPassword *string
		confirm     *string
		wantStatus  int
		wantCode    int
		wantMessage string
	}{
		{
			"当前密码空串",
			ptr(""), ptr(testAdminNewPassword), ptr(testAdminNewPassword),
			http.StatusUnauthorized, codeSessionInvalid, adminCredentialsInvalidMessage,
		},
		{
			"新密码空串",
			ptr(testAdminPassword), ptr(""), ptr(""),
			http.StatusUnprocessableEntity, codePasswordInvalid, "密码长度不足 12 个字符",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPasswordTestStore(t)
			srv := newAdminTestServer(t, store, nil)
			hashBefore := store.user.PasswordHash
			hashCallsBefore := auth.HashCallCount()

			rec := serveAdmin(srv, newPasswordRequest(t, passwordJSON(t, tc.current, tc.newPassword, tc.confirm)))

			failure := decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)
			if failure.Message != tc.wantMessage {
				t.Fatalf("文案期望 %q，实际 %q", tc.wantMessage, failure.Message)
			}
			assertNoStore(t, rec)
			// 两条空串分支都不清 Cookie：40101 那侧会话仍有效，42201 那侧压根不是凭据问题。
			assertNoSetCookie(t, rec)
			if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
				t.Fatalf("空串不该写库，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
					store.updatePasswordCalls, store.deleteAllCalls)
			}
			if store.user.PasswordHash != hashBefore {
				t.Fatal("空串不该改写哈希")
			}
			if store.sessionCount() != 2 {
				t.Fatalf("空串不该删除会话，期望 2 条，实际 %d 条", store.sessionCount())
			}
			// 两条分支的 argon2 都只来自「校验当前密码」这一次（改密顺序把当前密码排在新规则之前）：
			// 当前密码空串时这次校验不过，新密码空串时它通过、随后规则被挡。
			// 关键断言是「新密码没有被哈希」——HashPassword 一次都没发生。
			if got := auth.HashCallCount() - hashCallsBefore; got != 1 {
				t.Fatalf("argon2 次数期望增加 1（只校验当前密码），实际增加 %d", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 仓储失败与中间件
// ---------------------------------------------------------------------------

// TestAdminPasswordStoreFailures 覆盖改密链路上的仓储故障：
//   - UpdatePassword 失败 → 50000，不清 Cookie；哈希与会话都没变，可以原样重试；
//   - DeleteAllSessions 失败 → 50000，不清 Cookie。此时密码已经改了、会话却没收回，
//     CP8 要求「成功后行数为 0」，所以不能假装成功；响应同样不透出底层错误。
//
// 两条分支都要断言日志：既不泄露仓储错误原文（sentinel 只可能在 err 里），
// 又必须留下「哪一步失败」的固定类别——只断言「不泄露」会放过一种回归：
// 5xx 悄悄记成一句泛泛的「内部错误」，排障时看不出是写哈希还是清理会话出的问题。
// 因此日志侧沿用 admin 测试的另两处做法：独立的 JSON 日志缓冲 + 按固定短句匹配。
func TestAdminPasswordStoreFailures(t *testing.T) {
	const sentinel = "password-store-down-9c4b"

	cases := []struct {
		name          string
		prepare       func(*passwordRuntimeStore)
		wantUpdated   int
		wantDeleteAll int
		wantHashMoved bool
		// wantLog 是 adminFail 的固定日志文案，也是这次失败的唯一类别标记。
		wantLog string
	}{
		{"更新密码失败", func(s *passwordRuntimeStore) {
			s.updatePasswordErr = errors.New(sentinel)
		}, 1, 0, false, "更新管理员密码失败"},
		{"清理会话失败", func(s *passwordRuntimeStore) {
			s.deleteAllErr = errors.New(sentinel)
		}, 1, 1, true, "清理全部管理会话失败"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 这条路径要断言日志，不能沿用 newAdminTestServer 的 quietLogger。
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

			store := newPasswordTestStore(t)
			tc.prepare(store)
			srv := New(logger, nil, nil, nil, nil, WithAdmin(store, nil))
			srv.clock = func() time.Time { return testAdminNow }
			hashBefore := store.user.PasswordHash

			rec := serveAdmin(srv, validPasswordRequest(t))

			decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
			assertNoStore(t, rec)
			// 失败不清 Cookie：会话可能还有效（更新失败时）或密码已改但会话未收回
			// （清理失败时）；两种情况都不该让客户端以为自己已经登出。
			assertNoSetCookie(t, rec)
			if strings.Contains(rec.Body.String(), sentinel) {
				t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
			}
			// 日志侧：原文绝不出现，固定类别必须出现（且是这次失败对应的那一条）。
			if strings.Contains(logBuf.String(), sentinel) {
				t.Fatalf("日志泄露内部错误: %s", logBuf.String())
			}
			if !strings.Contains(logBuf.String(), tc.wantLog) {
				t.Fatalf("日志缺少固定类别 %q，实际: %s", tc.wantLog, logBuf.String())
			}

			if store.updatePasswordCalls != tc.wantUpdated {
				t.Fatalf("UpdatePassword 期望 %d 次，实际 %d 次", tc.wantUpdated, store.updatePasswordCalls)
			}
			if store.deleteAllCalls != tc.wantDeleteAll {
				t.Fatalf("DeleteAllSessions 期望 %d 次，实际 %d 次", tc.wantDeleteAll, store.deleteAllCalls)
			}
			if moved := store.user.PasswordHash != hashBefore; moved != tc.wantHashMoved {
				t.Fatalf("哈希是否已改写期望 %t，实际 %t", tc.wantHashMoved, moved)
			}
			// 两条失败分支都不删会话：失败绝不留下「密码换了、会话没了」的半成品。
			if store.sessionCount() != 2 {
				t.Fatalf("失败路径不该删除会话，实际剩 %d 条", store.sessionCount())
			}
		})
	}
}

// TestAdminPasswordAdminLookupFailure 覆盖改密链路读管理员失败：50000、不写库、不清 Cookie、
// 不把仓储错误原文透给客户端或日志。失败发生在校验当前密码之前，所以 argon2 也不该被消耗。
// 读管理员失败不是凭据问题：会话本身仍然有效，清 Cookie 等于因为一次服务端故障把人踢下线。
func TestAdminPasswordAdminLookupFailure(t *testing.T) {
	const sentinel = "admin-lookup-down-2b7e"

	// 这条路径要断言日志，不能沿用 newAdminTestServer 的 quietLogger。
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	store := newPasswordTestStore(t)
	store.userErr = errors.New(sentinel)
	srv := New(logger, nil, nil, nil, nil, WithAdmin(store, nil))
	srv.clock = func() time.Time { return testAdminNow }
	hashBefore := store.user.PasswordHash
	hashCallsBefore := auth.HashCallCount()

	rec := serveAdmin(srv, validPasswordRequest(t))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
	assertNoSetCookie(t, rec)
	if strings.Contains(rec.Body.String(), sentinel) {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
	if strings.Contains(logBuf.String(), sentinel) {
		t.Fatalf("日志泄露内部错误: %s", logBuf.String())
	}

	if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
		t.Fatalf("读管理员失败不该写库，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
			store.updatePasswordCalls, store.deleteAllCalls)
	}
	if store.user.PasswordHash != hashBefore {
		t.Fatal("读管理员失败时哈希必须保持不变")
	}
	if store.sessionCount() != 2 {
		t.Fatalf("读管理员失败不该删除会话，实际 %d 条", store.sessionCount())
	}
	if got := auth.HashCallCount() - hashCallsBefore; got != 0 {
		t.Fatalf("读管理员失败应在密码校验之前返回，argon2 实际增加 %d 次", got)
	}
}

// TestAdminPasswordWriteChainGates 覆盖真实路由上的三道门（Requirement 6.2、6.3、6.6、CP7）：
// 缺会话 → 40101 并清 Cookie；缺 CSRF → 40301 且保留会话；Content-Type 不符 → 40001。
// 三种情况都必须让改密的写方法调用次数为 0，也不该走到密码哈希。
func TestAdminPasswordWriteChainGates(t *testing.T) {
	cases := []struct {
		name       string
		build      func(t *testing.T) *http.Request
		wantStatus int
		wantCode   int
	}{
		{
			"缺少会话 Cookie",
			func(t *testing.T) *http.Request {
				t.Helper()
				req := validPasswordRequest(t)
				req.Header.Del("Cookie")
				return req
			},
			http.StatusUnauthorized, codeSessionInvalid,
		},
		{
			"缺少 CSRF 令牌",
			func(t *testing.T) *http.Request {
				t.Helper()
				req := validPasswordRequest(t)
				req.Header.Del(adminCSRFHeader)
				return req
			},
			http.StatusForbidden, codeCSRFInvalid,
		},
		{
			"Content-Type 不符",
			func(t *testing.T) *http.Request {
				t.Helper()
				req := validPasswordRequest(t)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				return req
			},
			http.StatusBadRequest, codeBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPasswordTestStore(t)
			srv := newAdminTestServer(t, store, nil)
			hashCallsBefore := auth.HashCallCount()

			rec := serveAdmin(srv, tc.build(t))

			decodeAdminFailure(t, rec, tc.wantStatus, tc.wantCode)
			assertNoStore(t, rec)
			if store.updatePasswordCalls != 0 || store.deleteAllCalls != 0 {
				t.Fatalf("门内失败不该写库，实际 UpdatePassword %d 次 / DeleteAllSessions %d 次",
					store.updatePasswordCalls, store.deleteAllCalls)
			}
			// 三门都在业务之前：不查管理员、不校验当前密码，因此不消耗 argon2。
			if got := auth.HashCallCount() - hashCallsBefore; got != 0 {
				t.Fatalf("门内失败不该计算哈希，实际 argon2 次数增加 %d", got)
			}
			if store.sessionCount() != 2 {
				t.Fatalf("会话应保持 2 条，实际 %d 条", store.sessionCount())
			}
		})
	}

	// CSRF 失败与体门失败都要保留会话：清除指令只属于「会话无效」那类 401。
	t.Run("CSRF 失败不清 Cookie", func(t *testing.T) {
		store := newPasswordTestStore(t)
		srv := newAdminTestServer(t, store, nil)
		req := validPasswordRequest(t)
		req.Header.Del(adminCSRFHeader)

		rec := serveAdmin(srv, req)
		decodeAdminFailure(t, rec, http.StatusForbidden, codeCSRFInvalid)
		assertNoSetCookie(t, rec)
	})
}

// TestAdminPasswordWithoutStore 覆盖未装配数据来源：路由仍注册，按 50000 返回而不是 404，
// 让 main.go 漏装配立刻暴露（与 state/init/login 同口径）。
func TestAdminPasswordWithoutStore(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	srv.clock = func() time.Time { return testAdminNow }

	rec := serveAdmin(srv, validPasswordRequest(t))

	decodeAdminFailure(t, rec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, rec)
}
