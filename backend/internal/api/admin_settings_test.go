package api

// 本文件是设置端点的契约测试（tasklist 第 9 项）：
//
//	GET /api/admin/settings  键集合、*_set、cron_next_at、空名单形状、秘密不回显
//	PUT /api/admin/settings  局部更新、严格解码 40001、校验 42201、仓储/密钥 50000、
//	                         64 KiB 体边界、会话与 CSRF 门槛、no-store
//
// 服务层用真实的 settings.Service（替身 Store + 替身 Sealer），只有存储与密钥是假的：
// 这样「校验 → 加封 → 一次事务 → 回读」整条链真的跑过一遍，契约测试才能真正暴露
// 「handler 忘了映射某个错误」这类问题。请求一律经过 s.Handler()，中间件链与路由
// 注册都在测（而不是直接调 handler 绕过 adminRead/adminWriteLimit）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
)

// settingsStoreStub 是 settings.Store 的内存替身：rows 模拟 settings 表，
// ApplySettings 就地生效，Patch 的回读因此能看到刚写入的值。
// 记录调用次数与批次，用来断言「42201/40001/50000 时零写入」「一次保存只开一次事务」。
type settingsStoreStub struct {
	rows []model.SettingRow

	loadErr     error
	applyErr    error
	readBackErr error // 只在 ApplySettings 成功后生效：模拟「写已提交、回读失败」

	loadCalls  int
	applyCalls int
	batches    [][]model.SettingChange
}

// LoadSettings 返回行的副本，并记一次读调用。
//
// readBackErr 与 loadErr 分开：loadErr 让每一次读都失败（GET 与 Patch 的首次读），
// readBackErr 只在已经成功提交过一次 ApplySettings 之后才生效，用来复现
// 「事务已提交、回读那一次 Load 失败」——此时设置确实落库了，但服务层拿不到快照。
func (s *settingsStoreStub) LoadSettings(context.Context) ([]model.SettingRow, error) {
	s.loadCalls++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.readBackErr != nil && s.applyCalls > 0 {
		return nil, s.readBackErr
	}
	return append([]model.SettingRow(nil), s.rows...), nil
}

// ApplySettings 记录批次与调用次数；applyErr 非 nil 时按仓储失败返回、不落行。
func (s *settingsStoreStub) ApplySettings(_ context.Context, changes []model.SettingChange, now time.Time) error {
	s.applyCalls++
	if s.applyErr != nil {
		return s.applyErr
	}
	s.batches = append(s.batches, append([]model.SettingChange(nil), changes...))
	for _, change := range changes {
		s.applyOne(change, now)
	}
	return nil
}

// applyOne 把一条变更按 Key 落到 rows 上：删除即摘行，否则覆盖或追加。
// 与真实仓储的差异只有「不判事务」，原子性由 store 的集成测试覆盖。
func (s *settingsStoreStub) applyOne(change model.SettingChange, now time.Time) {
	for i := range s.rows {
		if s.rows[i].Key != change.Key {
			continue
		}
		if change.Delete {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return
		}
		s.rows[i].Value, s.rows[i].Secret, s.rows[i].UpdatedAt = change.Value, change.Secret, now
		return
	}
	if change.Delete {
		return
	}
	s.rows = append(s.rows, model.SettingRow{
		Key: change.Key, Value: change.Value, Secret: change.Secret, UpdatedAt: now,
	})
}

// row 按键取当前行，供断言「秘密是否真的以密文落库」「清除是否真的删行」。
func (s *settingsStoreStub) row(key string) (model.SettingRow, bool) {
	for _, row := range s.rows {
		if row.Key == key {
			return row, true
		}
	}
	return model.SettingRow{}, false
}

// settingsSealerStub 是 settings.Sealer 的替身：记录 Current() 调用次数，可注入错误。
type settingsSealerStub struct {
	key   secret.Key
	err   error
	calls int
}

// Current 返回测试密钥或预设错误。
func (s *settingsSealerStub) Current() (secret.Key, error) {
	s.calls++
	if s.err != nil {
		return secret.Key{}, s.err
	}
	return s.key, nil
}

// settingsTestKey 返回一把确定性的测试密钥，内容与生产无关。
func settingsTestKey() secret.Key {
	var key secret.Key
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// reloadRecorder 记录重排 cron 的调用；err 非 nil 时模拟调度器拒绝新表达式。
type reloadRecorder struct {
	exprs []string
	err   error
}

// reload 是传给 WithAdminSettings 的回调。
func (r *reloadRecorder) reload(expr string) error {
	r.exprs = append(r.exprs, expr)
	return r.err
}

// settingRow 造一行设置，secret 为 true 时表示 is_secret 列。
func settingRow(key, value string, isSecret bool) model.SettingRow {
	return model.SettingRow{Key: key, Value: value, Secret: isSecret, UpdatedAt: testAdminNow}
}

// sealedSettingRow 造一行「密文落库」的秘密设置；值本身不需要能解开，契约测试只断言不回显。
func sealedSettingRow(key, sentinel string) model.SettingRow {
	return settingRow(key, "v1:"+sentinel, true)
}

// newAdminSettingsServer 组装完整的 Server：可注入日志器、会话来源、设置存储、密钥与重排回调。
func newAdminSettingsServer(log *slog.Logger, admin adminStore,
	store *settingsStoreStub, sealer *settingsSealerStub, reload func(string) error) *Server {
	svc := settings.New(store, sealer, func() time.Time { return testAdminNow })
	srv := New(log, nil, nil, nil, nil,
		WithAdmin(admin, nil),
		WithAdminSettings(svc, reload),
	)
	srv.clock = func() time.Time { return testAdminNow }
	return srv
}

// newActiveAdminSettingsServer 是「有效会话 + 丢弃日志」的常用组合。
func newActiveAdminSettingsServer(store *settingsStoreStub,
	sealer *settingsSealerStub, reload func(string) error) *Server {
	return newAdminSettingsServer(quietLogger(), activeAdminStore(), store, sealer, reload)
}

// newActiveSettingsStore 造一个「密钥可用」的存储替身。
func newActiveSettingsStore(rows ...model.SettingRow) (*settingsStoreStub, *settingsSealerStub) {
	return &settingsStoreStub{rows: rows}, &settingsSealerStub{key: settingsTestKey()}
}

// lockedAdminStore 给 adminStore 替身套一层互斥，专供并发保存用例。
//
// 两个并发 PUT 会同时走到 adminRead/adminWriteLimit 的会话校验，而 adminStoreStub 会
// 累加调用次数、记下最后一次令牌摘要——那只是替身的统计，不在生产契约里，不加锁时
// `-race` 会先在替身上报 data race，把「settingsMu 是否真的串行」的断言淹掉。
// 生产代码的共享状态只有 settingsMu 保护的那一段。
type lockedAdminStore struct {
	adminStore
	mu sync.Mutex
}

// Session 串行回放会话查询，避免替身字段被两个请求同时读写。
func (s *lockedAdminStore) Session(ctx context.Context, tokenHash string) (model.AdminSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adminStore.Session(ctx, tokenHash)
}

// adminSettingsGetRequest 构造 GET 请求；rawToken 为空表示不带会话 Cookie。
func adminSettingsGetRequest(rawToken string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	if rawToken != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: rawToken})
	}
	return req
}

// adminSettingsPutRequest 构造 PUT 请求：默认带 application/json、会话 Cookie 与 CSRF 头，
// 三者都可以按用例置空。
func adminSettingsPutRequest(body, rawToken, csrf string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if rawToken != "" {
		req.AddCookie(&http.Cookie{Name: adminCookieName, Value: rawToken})
	}
	if csrf != "" {
		req.Header.Set(adminCSRFHeader, csrf)
	}
	return req
}

// serveAdminSettings 经完整的 Handler 跑一个请求（路由 + 中间件 + handler）。
func serveAdminSettings(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// settingsDataKeys 是设置响应的 8 个键（CP18）。
var settingsDataKeys = []string{
	"jw_username", "jw_password_set", "ocr_base_url",
	"feishu_webhook_set", "feishu_sign_secret_set",
	"cron_expr", "cron_next_at", "building_whitelist",
}

// assertSettingsDataKeys 断言 data 键集合恰好是 8 个。
func assertSettingsDataKeys(t *testing.T, data map[string]any) {
	t.Helper()
	assertContractKeys(t, "settings data", data, settingsDataKeys...)
}

// assertSecretNotEchoed 断言响应体不含任何秘密哨兵，也不含密文前缀。
func assertSecretNotEchoed(t *testing.T, rec *httptest.ResponseRecorder, sentinels ...string) {
	t.Helper()
	body := rec.Body.String()
	for _, sentinel := range sentinels {
		if strings.Contains(body, sentinel) {
			t.Fatalf("响应泄露了秘密 %q: %s", sentinel, body)
		}
	}
	if strings.Contains(body, "v1:") {
		t.Fatalf("响应出现了密文前缀 v1: %s", body)
	}
}

// TestAdminSettingsGetShape 覆盖 GET 的成功形状（Requirement 1.1–1.4、CP18）：
// 8 个键、四项明文、三项 *_set、cron_next_at 的 UTC 形状、白名单顺序；
// 同时断言秘密密文与哨兵都不出现在响应里（CP1）。
func TestAdminSettingsGetShape(t *testing.T) {
	const (
		passwordSentinel = "sentinel-password-4a1f"
		webhookSentinel  = "sentinel-webhook-7b2c"
		signSentinel     = "sentinel-sign-9d3e"
	)

	store, sealer := newActiveSettingsStore(
		settingRow(model.SettingJWUsername, "teacher01", false),
		sealedSettingRow(model.SettingJWPassword, passwordSentinel),
		settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
		sealedSettingRow(model.SettingFeishuWebhook, webhookSentinel),
		sealedSettingRow(model.SettingFeishuSignSecret, signSentinel),
		settingRow(model.SettingCronExpr, "10 4 * * 6", false),
		settingRow(model.SettingBuildingWhitelist,
			`[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q03","name":"老文史楼"}]`, false),
	)
	srv := newActiveAdminSettingsServer(store, sealer, nil)

	rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	assertNoStore(t, rec)

	if data["jw_username"] != "teacher01" {
		t.Fatalf("jw_username 不符：%#v", data["jw_username"])
	}
	if data["ocr_base_url"] != "http://ocr:8000" {
		t.Fatalf("ocr_base_url 不符：%#v", data["ocr_base_url"])
	}
	if data["cron_expr"] != "10 4 * * 6" {
		t.Fatalf("cron_expr 不符：%#v", data["cron_expr"])
	}
	// testAdminNow = 2026-11-06T10:00:00Z（周五 18:00 +08:00），下一命中是周六 04:10 +08:00。
	if data["cron_next_at"] != "2026-11-06T20:10:00Z" {
		t.Fatalf("cron_next_at 期望 UTC RFC3339，实际 %#v", data["cron_next_at"])
	}
	for _, key := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if data[key] != true {
			t.Fatalf("%s 期望 true（库里该键有行），实际 %#v", key, data[key])
		}
	}

	got := contractTypedData[adminSettingsResponse](t, rec)
	wantWhitelist := []model.Building{
		{ID: "Q10", Name: "综合教学楼"},
		{ID: "Q03", Name: "老文史楼"},
	}
	if !reflect.DeepEqual(got.BuildingWhitelist, wantWhitelist) {
		t.Fatalf("白名单顺序/取值不符：实际 %+v，期望 %+v", got.BuildingWhitelist, wantWhitelist)
	}

	assertSecretNotEchoed(t, rec, passwordSentinel, webhookSentinel, signSentinel)
}

// TestAdminSettingsGetEmpty 覆盖「从未写入任何设置」的回落（Requirement 1.3、9.5）：
// 字符串项空串、三项 *_set 为 false、cron 回默认值、白名单是 [] 而不是 null。
func TestAdminSettingsGetEmpty(t *testing.T) {
	store, sealer := newActiveSettingsStore()
	srv := newActiveAdminSettingsServer(store, sealer, nil)

	rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	assertNoStore(t, rec)

	if data["jw_username"] != "" || data["ocr_base_url"] != "" {
		t.Fatalf("缺行应回空串：%v", data)
	}
	for _, key := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if data[key] != false {
			t.Fatalf("%s 期望 false（库里没有该键的行），实际 %#v", key, data[key])
		}
	}
	if data["cron_expr"] != model.DefaultCronExpr {
		t.Fatalf("缺行应回落 %q，实际 %#v", model.DefaultCronExpr, data["cron_expr"])
	}
	if _, ok := data["building_whitelist"].([]any); !ok {
		t.Fatalf("building_whitelist 应是数组，实际 %#v", data["building_whitelist"])
	}
	if whitelist, ok := data["building_whitelist"].([]any); !ok || len(whitelist) != 0 {
		t.Fatalf("空名单应是 []，实际 %#v", data["building_whitelist"])
	}
	// 序列化后必须是 []，不能是 null——前端按数组遍历。
	if !strings.Contains(rec.Body.String(), `"building_whitelist":[]`) {
		t.Fatalf("空名单应序列化为 []，实际 body=%s", rec.Body.String())
	}
}

// TestAdminSettingsCronNextAt 覆盖 cron_next_at 的四种取值（Requirement 1.4、5.6）：
// 默认值、自定义表达式、非法存量表达式（含 0005 之前的 6 段写法）、五年内不可达。
func TestAdminSettingsCronNextAt(t *testing.T) {
	cases := []struct {
		name string
		rows []model.SettingRow
		want *string
	}{
		{
			name: "缺行回落默认值",
			want: stringPtr("2026-11-06T20:10:00Z"), // 周六 04:10 +08:00
		},
		{
			name: "工作日每两小时半点",
			rows: []model.SettingRow{settingRow(model.SettingCronExpr, "30 8-18/2 * * 1-5", false)},
			want: stringPtr("2026-11-06T10:30:00Z"), // 周五 18:30 +08:00
		},
		{
			name: "非法存量表达式（6 段旧种子）",
			rows: []model.SettingRow{settingRow(model.SettingCronExpr, "0 10 4 * * 6", false)},
		},
		{
			name: "空表达式",
			rows: []model.SettingRow{settingRow(model.SettingCronExpr, "", false)},
		},
		{
			name: "五年内不可达",
			rows: []model.SettingRow{settingRow(model.SettingCronExpr, "0 0 30 2 *", false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore(tc.rows...)
			srv := newActiveAdminSettingsServer(store, sealer, nil)

			rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

			data := contractSuccessData(t, rec)
			assertSettingsDataKeys(t, data)
			if tc.want == nil {
				if data["cron_next_at"] != nil {
					t.Fatalf("算不出下一次触发时应为 null，实际 %#v", data["cron_next_at"])
				}
				return
			}
			if data["cron_next_at"] != *tc.want {
				t.Fatalf("cron_next_at 期望 %q，实际 %#v", *tc.want, data["cron_next_at"])
			}
			if !strings.HasSuffix(data["cron_next_at"].(string), "Z") {
				t.Fatalf("cron_next_at 必须是 UTC（Z 结尾），实际 %#v", data["cron_next_at"])
			}
		})
	}
}

// stringPtr 返回字符串指针，用来表达「期望有值」的 cron_next_at。
func stringPtr(value string) *string { return &value }

// TestAdminSettingsPutContract 覆盖 PUT 的成功路径（Requirement 2.1–2.7、3.1）：
// 断言写库结果（trim、密文、is_secret、白名单顺序）、重排 cron 的入参、
// 响应与 GET 同形状，以及秘密不回显。
func TestAdminSettingsPutContract(t *testing.T) {
	const (
		passwordSentinel = "sentinel-password-5c1d"
		webhookSentinel  = "https://open.feishu.cn/open-apis/bot/v2/hook/sentinel-6d2e"
		signSentinel     = "sentinel-sign-7e3f"
	)

	store, sealer := newActiveSettingsStore()
	reload := &reloadRecorder{}
	srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

	body := `{
		"jw_username": "  teacher01  ",
		"jw_password": "` + passwordSentinel + `",
		"ocr_base_url": " http://ocr:8000 ",
		"feishu_webhook": "` + webhookSentinel + `",
		"feishu_sign_secret": "` + signSentinel + `",
		"cron_expr": " 30 8-18/2 * * 1-5 ",
		"building_whitelist": [
			{"jxlbh": " Q10 ", "name": " 综合教学楼 "},
			{"jxlbh": "Q03", "name": "老文史楼"}
		]
	}`
	rec := serveAdminSettings(srv, adminSettingsPutRequest(body, testAdminRawToken, testAdminCSRF))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	assertNoStore(t, rec)

	if data["jw_username"] != "teacher01" {
		t.Fatalf("jw_username 应 trim 后回显，实际 %#v", data["jw_username"])
	}
	if data["ocr_base_url"] != "http://ocr:8000" {
		t.Fatalf("ocr_base_url 应 trim 后回显，实际 %#v", data["ocr_base_url"])
	}
	if data["cron_expr"] != "30 8-18/2 * * 1-5" {
		t.Fatalf("cron_expr 应 trim 后回显，实际 %#v", data["cron_expr"])
	}
	if data["cron_next_at"] != "2026-11-06T10:30:00Z" {
		t.Fatalf("cron_next_at 应按新表达式重算，实际 %#v", data["cron_next_at"])
	}
	for _, key := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if data[key] != true {
			t.Fatalf("%s 期望 true，实际 %#v", key, data[key])
		}
	}

	// 白名单按提交顺序、trim 后回显。
	got := contractTypedData[adminSettingsResponse](t, rec)
	wantWhitelist := []model.Building{
		{ID: "Q10", Name: "综合教学楼"},
		{ID: "Q03", Name: "老文史楼"},
	}
	if !reflect.DeepEqual(got.BuildingWhitelist, wantWhitelist) {
		t.Fatalf("白名单不符：实际 %+v，期望 %+v", got.BuildingWhitelist, wantWhitelist)
	}

	// 落库形状：秘密是 v1: 信封且 is_secret=true，明文不得出现在库里。
	if store.applyCalls != 1 {
		t.Fatalf("一次保存应只开一次事务，实际 %d 次", store.applyCalls)
	}
	for _, key := range []string{model.SettingJWPassword, model.SettingFeishuWebhook, model.SettingFeishuSignSecret} {
		row, found := store.row(key)
		if !found {
			t.Fatalf("%s 应已落库", key)
		}
		if !strings.HasPrefix(row.Value, "v1:") {
			t.Fatalf("%s 落库值应是 v1: 信封，实际 %q", key, row.Value)
		}
		if !row.Secret {
			t.Fatalf("%s 的 is_secret 应为 true", key)
		}
	}
	for _, sentinel := range []string{passwordSentinel, webhookSentinel, signSentinel} {
		for _, row := range store.rows {
			if strings.Contains(row.Value, sentinel) {
				t.Fatalf("%s 的落库值含明文哨兵 %q", row.Key, sentinel)
			}
		}
	}

	// 重排用的是库里那串（trim 后），不是请求体原文。
	if want := []string{"30 8-18/2 * * 1-5"}; !reflect.DeepEqual(reload.exprs, want) {
		t.Fatalf("重排入参不符：实际 %v，期望 %v", reload.exprs, want)
	}

	assertSecretNotEchoed(t, rec, passwordSentinel, webhookSentinel, signSentinel)

	// 保存成功后 GET 必须给同一份快照（Requirement 2.7）。
	getRec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))
	getData := contractSuccessData(t, getRec)
	if !reflect.DeepEqual(getData, data) {
		t.Fatalf("PUT 与 GET 的 data 不一致：\nPUT=%#v\nGET=%#v", data, getData)
	}
}

// TestAdminSettingsPutConcurrentCronSerialized 锁定并发保存下「库值」与「最后一次重排」的
// 一致性：两个 PUT 带不同 cron_expr 同时打进来，最终 settings 行里的表达式必须等于
// reload.exprs 的最后一项——即「库里最后一次写入」与「进程内最后一次重排」是同一个请求。
//
// 没有 settingsMu 时，这两个动作可以被交错成「A 写库、B 写库、B 重排、A 重排」：库是 B、
// 进程内计划是 A，GET 显示 B 而调度器按 A 跑，直到下次重启才对齐。加锁后「Patch → 重排」
// 成为一段临界区，交错只剩「整段 A → 整段 B」或反之，不变式恒成立。
//
// 重复多轮是为了在缺锁时提高撞上交错窗口的概率（单轮里那个窗口只有几条指令宽）；
// 断言只看最终一致性、不用 sleep 或超时阈值，所以有锁时它是确定通过的。
func TestAdminSettingsPutConcurrentCronSerialized(t *testing.T) {
	exprs := []string{"10 4 * * 6", "30 8-18/2 * * 1-5"}
	const rounds = 32

	for round := range rounds {
		store, sealer := newActiveSettingsStore(
			settingRow(model.SettingCronExpr, model.DefaultCronExpr, false),
		)
		reload := &reloadRecorder{}
		srv := newAdminSettingsServer(quietLogger(), &lockedAdminStore{adminStore: activeAdminStore()},
			store, sealer, reload.reload)
		handler := srv.Handler()

		// 同一时刻起跑，让两次保存真的落在彼此的临界区旁边。
		start := make(chan struct{})
		results := make([]*httptest.ResponseRecorder, len(exprs))
		var wg sync.WaitGroup
		wg.Add(len(exprs))
		for i, expr := range exprs {
			go func() {
				defer wg.Done()
				body := fmt.Sprintf(`{"cron_expr":%q}`, expr)
				req := adminSettingsPutRequest(body, testAdminRawToken, testAdminCSRF)
				rec := httptest.NewRecorder()
				<-start
				handler.ServeHTTP(rec, req)
				results[i] = rec
			}()
		}
		close(start)
		wg.Wait()

		for i, rec := range results {
			if rec.Code != http.StatusOK {
				t.Fatalf("第 %d 轮：第 %d 个并发保存应 200，实际 %d（body=%s）",
					round, i, rec.Code, rec.Body.String())
			}
		}

		// 两个非空 cron_expr 各触发一次重排：两次都得发生，顺序不定。
		if len(reload.exprs) != len(exprs) {
			t.Fatalf("第 %d 轮：两次保存应各重排一次，实际 %v", round, reload.exprs)
		}
		row, found := store.row(model.SettingCronExpr)
		if !found {
			t.Fatalf("第 %d 轮：cron_expr 应已落库", round)
		}
		if last := reload.exprs[len(reload.exprs)-1]; row.Value != last {
			t.Fatalf("第 %d 轮：最后一次重排必须就是库里的值：库=%q，最后一次重排=%q（全部重排=%v）",
				round, row.Value, last, reload.exprs)
		}
	}
}

// TestAdminSettingsPutPartialUpdate 覆盖局部更新语义（Requirement 2.1–2.4、3.6）：
// 省略即保持、空串即清除，且只清除秘密时不需要加密密钥。
func TestAdminSettingsPutPartialUpdate(t *testing.T) {
	t.Run("省略的字段保持原值", func(t *testing.T) {
		store, sealer := newActiveSettingsStore(
			settingRow(model.SettingJWUsername, "old-user", false),
			sealedSettingRow(model.SettingJWPassword, "old-cipher"),
			settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
			settingRow(model.SettingCronExpr, "10 4 * * 6", false),
			settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
		)
		reload := &reloadRecorder{}
		srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"jw_username":"new-user"}`, testAdminRawToken, testAdminCSRF))

		data := contractSuccessData(t, rec)
		assertSettingsDataKeys(t, data)
		if data["jw_username"] != "new-user" {
			t.Fatalf("jw_username 应更新，实际 %#v", data["jw_username"])
		}
		if data["ocr_base_url"] != "http://ocr:8000" {
			t.Fatalf("省略的 ocr_base_url 不该被清空，实际 %#v", data["ocr_base_url"])
		}
		if data["cron_expr"] != "10 4 * * 6" {
			t.Fatalf("省略的 cron_expr 不该变化，实际 %#v", data["cron_expr"])
		}
		if data["jw_password_set"] != true {
			t.Fatalf("省略的秘密项不该被删除，实际 jw_password_set=%#v", data["jw_password_set"])
		}
		if len(store.batches) != 1 || len(store.batches[0]) != 1 {
			t.Fatalf("本次只该有 jw_username 一条变更，实际 %+v", store.batches)
		}
		if len(reload.exprs) != 0 {
			t.Fatalf("未提交 cron_expr 不该重排（改用户名不影响调度计划），实际 %v", reload.exprs)
		}
	})

	t.Run("只改白名单不重排", func(t *testing.T) {
		// 与「省略的字段保持原值」正交：这次确实提交了字段、开了事务、落库成功，
		// 只是提交的是白名单而不是 cron_expr，调度计划不该被顺手重排一遍
		// （design「管理 API 契约」：仅提交了 cron_expr 时才 Reload）。
		store, sealer := newActiveSettingsStore(
			settingRow(model.SettingCronExpr, "10 4 * * 6", false),
			settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
		)
		reload := &reloadRecorder{}
		srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"building_whitelist":[{"jxlbh":"Q03","name":"老文史楼"}]}`, testAdminRawToken, testAdminCSRF))

		data := contractSuccessData(t, rec)
		assertSettingsDataKeys(t, data)
		if data["cron_expr"] != "10 4 * * 6" {
			t.Fatalf("没提交的 cron_expr 不该变化，实际 %#v", data["cron_expr"])
		}
		got := contractTypedData[adminSettingsResponse](t, rec)
		want := []model.Building{{ID: "Q03", Name: "老文史楼"}}
		if !reflect.DeepEqual(got.BuildingWhitelist, want) {
			t.Fatalf("白名单应按提交值落库：实际 %+v，期望 %+v", got.BuildingWhitelist, want)
		}
		if store.applyCalls != 1 {
			t.Fatalf("改白名单应落库一次，实际 ApplySettings %d 次", store.applyCalls)
		}
		if len(reload.exprs) != 0 {
			t.Fatalf("只改白名单不该重排 cron，实际 %v", reload.exprs)
		}
	})

	t.Run("空串清除非秘密项", func(t *testing.T) {
		store, sealer := newActiveSettingsStore(
			settingRow(model.SettingJWUsername, "teacher01", false),
			settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
		)
		srv := newActiveAdminSettingsServer(store, sealer, nil)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"ocr_base_url":""}`, testAdminRawToken, testAdminCSRF))

		data := contractSuccessData(t, rec)
		if data["ocr_base_url"] != "" {
			t.Fatalf("空串应清空 ocr_base_url，实际 %#v", data["ocr_base_url"])
		}
		if _, found := store.row(model.SettingOCRBaseURL); found {
			t.Fatal("空串应删除该键的行")
		}
	})

	t.Run("空数组清空白名单", func(t *testing.T) {
		// 与 TestAdminSettingsPutNullFieldOmitted 的「白名单为 null」对偶：
		// null 是「没提交」、保持原值；[] 是「提交了空名单」、把值写成 "[]"。
		store, sealer := newActiveSettingsStore(
			settingRow(model.SettingJWUsername, "teacher01", false),
			settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
		)
		srv := newActiveAdminSettingsServer(store, sealer, nil)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"building_whitelist":[]}`, testAdminRawToken, testAdminCSRF))

		data := contractSuccessData(t, rec)
		assertSettingsDataKeys(t, data)
		// 清空不是删除该键：行还在、值是 "[]"（Requirement 4.5、CP7）。
		row, found := store.row(model.SettingBuildingWhitelist)
		if !found {
			t.Fatal("[] 应把白名单写成 []，而不是删除该键")
		}
		if row.Value != "[]" {
			t.Fatalf("白名单落库值应为 []，实际 %q", row.Value)
		}
		whitelist, ok := data["building_whitelist"].([]any)
		if !ok || len(whitelist) != 0 {
			t.Fatalf("空名单应回显 []，实际 %#v", data["building_whitelist"])
		}
		if !strings.Contains(rec.Body.String(), `"building_whitelist":[]`) {
			t.Fatalf("空名单应序列化为 []，实际 body=%s", rec.Body.String())
		}
	})

	t.Run("空串清除秘密项不取密钥", func(t *testing.T) {
		store, sealer := newActiveSettingsStore(
			sealedSettingRow(model.SettingJWPassword, "sentinel-cipher-8f4a"),
		)
		sealer.err = model.ErrKeyUnavailable
		srv := newActiveAdminSettingsServer(store, sealer, nil)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"jw_password":""}`, testAdminRawToken, testAdminCSRF))

		data := contractSuccessData(t, rec)
		if data["jw_password_set"] != false {
			t.Fatalf("清除秘密项后应为 false，实际 %#v", data["jw_password_set"])
		}
		if _, found := store.row(model.SettingJWPassword); found {
			t.Fatal("空串应删除该秘密项的行")
		}
		if sealer.calls != 0 {
			t.Fatalf("只清除秘密项时不该取密钥，实际 Current() 调用 %d 次", sealer.calls)
		}
		assertNoStore(t, rec)
	})
}

// TestAdminSettingsPutNullBodyNoop 锁定「请求体是 JSON null」的行为：
// 200 + 当前快照，一项设置都不改、不开事务。
//
// 这不是本端点特判出来的，而是 encoding/json 的标准行为：对非指针的结构体目标解码
// JSON null 是 no-op 且不报错（标准库文档：「Unmarshaling JSON null into other Go types
// has no effect on the value and produces no error」），所以 settings.Patch 保持全 nil，
// 等价于「所有字段都省略」。契约选择把 null 定为「全部省略」：局部更新的语义是
// 「提交了什么就改什么」，null 没有提交任何字段。若将来要改成拒绝 null，得先改 design
// 的契约与前端调用点，而不是在 handler 里悄悄多一条分支。
func TestAdminSettingsPutNullBodyNoop(t *testing.T) {
	store, sealer := newActiveSettingsStore(
		settingRow(model.SettingJWUsername, "teacher01", false),
		sealedSettingRow(model.SettingJWPassword, "sentinel-cipher-0a1b"),
		settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
		settingRow(model.SettingCronExpr, "10 4 * * 6", false),
		settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
	)
	reload := &reloadRecorder{}
	srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

	before := append([]model.SettingRow(nil), store.rows...)
	rec := serveAdminSettings(srv, adminSettingsPutRequest(`null`, testAdminRawToken, testAdminCSRF))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	assertNoStore(t, rec)

	// 零变更：行集合原样、没有开过事务、没有触发任何秘密加封。
	if !reflect.DeepEqual(store.rows, before) {
		t.Fatalf("null 体不该改任何设置：\n之前=%+v\n之后=%+v", before, store.rows)
	}
	if store.applyCalls != 0 {
		t.Fatalf("null 体不该开事务，实际 ApplySettings %d 次", store.applyCalls)
	}
	if sealer.calls != 0 {
		t.Fatalf("null 体不该取密钥，实际 Current() 调用 %d 次", sealer.calls)
	}
	// 没有提交 cron_expr，即便 reload 已装配也不该动进程内的调度计划。
	if len(reload.exprs) != 0 {
		t.Fatalf("null 体不该重排 cron，实际 %v", reload.exprs)
	}
	if data["jw_username"] != "teacher01" || data["ocr_base_url"] != "http://ocr:8000" {
		t.Fatalf("null 体应返回当前快照，实际 %#v", data)
	}
	if data["cron_expr"] != "10 4 * * 6" {
		t.Fatalf("null 体不该改 cron_expr，实际 %#v", data["cron_expr"])
	}
	if data["jw_password_set"] != true {
		t.Fatalf("null 体不该清除秘密项，实际 jw_password_set=%#v", data["jw_password_set"])
	}

	// 与 GET 同一份当前快照（Requirement 2.7 的「保存后不必再 GET」在这里退化成「没保存」）。
	getData := contractSuccessData(t, serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken)))
	if !reflect.DeepEqual(getData, data) {
		t.Fatalf("null 体返回的快照与 GET 不一致：\nPUT=%#v\nGET=%#v", data, getData)
	}
}

// TestAdminSettingsPutNullFieldOmitted 锁定字段级 null 的契约（design「管理 API 契约」）：
// `{"jw_password":null}`、`{"jw_username":null,"ocr_base_url":null}`、
// `{"cron_expr":null}` 与 `{"building_whitelist":null}` 都等同「没提交该字段」——
// 200 + 当前快照，原值保持、零写入、不取密钥、不重排。与整体 null 同源：encoding/json 把 null
// 解码进指针字段是 no-op，Patch 里该字段仍为 nil，service 因此连一条变更都不生成。
// 若将来把 null 改成「清除」或「拒绝」，这里与 design 要一起改，而不是在 handler 悄悄特判。
func TestAdminSettingsPutNullFieldOmitted(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(*testing.T, *httptest.ResponseRecorder, map[string]any)
	}{
		{
			name: "秘密字段为 null",
			body: `{"jw_password":null}`,
			check: func(t *testing.T, _ *httptest.ResponseRecorder, data map[string]any) {
				t.Helper()
				if data["jw_password_set"] != true {
					t.Fatalf("null 不该清除秘密项，实际 jw_password_set=%#v", data["jw_password_set"])
				}
			},
		},
		{
			name: "非秘密文本字段为 null",
			body: `{"jw_username":null,"ocr_base_url":null}`,
			check: func(t *testing.T, _ *httptest.ResponseRecorder, data map[string]any) {
				t.Helper()
				if data["jw_username"] != "teacher01" {
					t.Fatalf("null 不该清空 jw_username，实际 %#v", data["jw_username"])
				}
				if data["ocr_base_url"] != "http://ocr:8000" {
					t.Fatalf("null 不该清空 ocr_base_url，实际 %#v", data["ocr_base_url"])
				}
			},
		},
		{
			// cron_expr 的 null 单独列：它是唯一会触发重排的字段，若 handler 把 null
			// 当成「提交了」，就会拿库里的旧表达式重排一次（reload.exprs 非空）。
			name: "cron 表达式为 null",
			body: `{"cron_expr":null}`,
			check: func(t *testing.T, _ *httptest.ResponseRecorder, data map[string]any) {
				t.Helper()
				if data["cron_expr"] != "10 4 * * 6" {
					t.Fatalf("null 不该改 cron_expr，实际 %#v", data["cron_expr"])
				}
				// cron_next_at 仍按原表达式算出，说明快照读的是库里的值而不是请求体。
				if data["cron_next_at"] != "2026-11-06T20:10:00Z" {
					t.Fatalf("null 不该影响 cron_next_at，实际 %#v", data["cron_next_at"])
				}
			},
		},
		{
			name: "白名单为 null",
			body: `{"building_whitelist":null}`,
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ map[string]any) {
				t.Helper()
				got := contractTypedData[adminSettingsResponse](t, rec)
				want := []model.Building{{ID: "Q10", Name: "综合教学楼"}}
				if !reflect.DeepEqual(got.BuildingWhitelist, want) {
					t.Fatalf("null 不该清空白名单：实际 %+v，期望 %+v", got.BuildingWhitelist, want)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore(
				settingRow(model.SettingJWUsername, "teacher01", false),
				settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
				sealedSettingRow(model.SettingJWPassword, "sentinel-cipher-1c2d"),
				// cron 行必须存在：否则「cron_expr:null 保持原值」退化成「本来就没有值」，
				// 断言分不清「没改」与「默认值恰好相同」。
				settingRow(model.SettingCronExpr, "10 4 * * 6", false),
				settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
			)
			reload := &reloadRecorder{}
			srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

			before := append([]model.SettingRow(nil), store.rows...)
			rec := serveAdminSettings(srv, adminSettingsPutRequest(tc.body, testAdminRawToken, testAdminCSRF))

			data := contractSuccessData(t, rec)
			assertSettingsDataKeys(t, data)
			assertNoStore(t, rec)
			tc.check(t, rec, data)

			// 与原值逐字段一致，且没有开事务、没有取密钥。
			if !reflect.DeepEqual(store.rows, before) {
				t.Fatalf("字段级 null 不该改任何设置：\n之前=%+v\n之后=%+v", before, store.rows)
			}
			if store.applyCalls != 0 {
				t.Fatalf("字段级 null 不该开事务，实际 ApplySettings %d 次", store.applyCalls)
			}
			if sealer.calls != 0 {
				t.Fatalf("字段级 null 不该取密钥，实际 Current() 调用 %d 次", sealer.calls)
			}
			if len(reload.exprs) != 0 {
				t.Fatalf("字段级 null 不该重排 cron，实际 %v", reload.exprs)
			}
		})
	}
}

// TestAdminSettingsPutRejectsUnknownField 覆盖严格解码（Requirement 6.2、D10）：
// 未定义字段 → 400/40001，消息点名字段，且零写入、不重排。
func TestAdminSettingsPutRejectsUnknownField(t *testing.T) {
	cases := []struct{ name, body, wantField string }{
		{"拼错的键名", `{"cron_express":"10 4 * * 6"}`, "cron_express"},
		{"合法字段旁的未定义字段", `{"jw_username":"teacher01","unknown_key":1}`, "unknown_key"},
		// 嵌套对象的严格解码与顶层同一套规则：白名单条目里多出的键也要 40001，
		// 不能因为「外层键合法」就把它静默丢掉（Req 6.2、D10）。
		{
			"嵌套对象里的未定义字段",
			`{"building_whitelist":[{"jxlbh":"Q10","name":"综合教学楼","foo":1}]}`,
			"foo",
		},
		// 键名内部含引号：标准库回显为 cron_\"express，还原后应是原字符 cron_"express。
		{"键名含转义引号", `{"cron_\"express":1}`, `cron_"express`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore()
			reload := &reloadRecorder{}
			srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

			rec := serveAdminSettings(srv, adminSettingsPutRequest(tc.body, testAdminRawToken, testAdminCSRF))

			body := assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			message, _ := body["message"].(string)
			if !strings.Contains(message, tc.wantField) {
				t.Fatalf("40001 文案应点名字段 %q，实际 %q", tc.wantField, message)
			}
			// 标准库用 %q 包字段名：外层引号与 JSON 转义都该被还原。键名内部本来就
			// 含引号的用例，回显的应是原字符，而不是 \" 这种转义形式。
			if strings.Contains(message, `\"`) {
				t.Fatalf("40001 文案应还原键名里的转义引号，实际 %q", message)
			}
			if !strings.Contains(tc.wantField, `"`) && strings.Contains(message, `"`) {
				t.Fatalf("40001 文案不该带英文双引号，实际 %q", message)
			}
			assertNoStore(t, rec)
			if store.applyCalls != 0 {
				t.Fatalf("未定义字段不该写库，实际 ApplySettings %d 次", store.applyCalls)
			}
			if len(reload.exprs) != 0 {
				t.Fatalf("未定义字段不该重排 cron，实际 %v", reload.exprs)
			}
		})
	}
}

// TestAdminSettingsPutRejectsBadBody 覆盖体门与解码失败 → 400/40001（Requirement 6.1、6.6、
// design 错误表）：非 JSON 体、Content-Type 不符、缺 Content-Type 都在业务之前被拒，
// 零读写设置、不重排 cron，响应带 no-store 与可读文案。
func TestAdminSettingsPutRejectsBadBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string // 空串表示不带 Content-Type 头
		wantMessage string
	}{
		{"非 JSON 体", `not-json`, "application/json", "请求体不是合法 JSON"},
		{"空体", ``, "application/json", "请求体不是合法 JSON"},
		{"合法 JSON 是数组", `[]`, "application/json", "请求体不是合法 JSON"},
		{"合法 JSON 是字符串", `"x"`, "application/json", "请求体不是合法 JSON"},
		{"合法 JSON 是数字", `123`, "application/json", "请求体不是合法 JSON"},
		{"合法 JSON 是布尔 true", `true`, "application/json", "请求体不是合法 JSON"},
		{"合法 JSON 是布尔 false", `false`, "application/json", "请求体不是合法 JSON"},
		{"Content-Type 不符", `{"jw_username":"teacher01"}`, "application/x-www-form-urlencoded", "请求体必须是 application/json"},
		{"缺 Content-Type", `{"jw_username":"teacher01"}`, "", "请求体必须是 application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore()
			reload := &reloadRecorder{}
			srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

			req := adminSettingsPutRequest(tc.body, testAdminRawToken, testAdminCSRF)
			if tc.contentType == "" {
				req.Header.Del("Content-Type")
			} else {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := serveAdminSettings(srv, req)

			body := assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
			if message, _ := body["message"].(string); message != tc.wantMessage {
				t.Fatalf("40001 文案期望 %q，实际 %q", tc.wantMessage, message)
			}
			assertNoStore(t, rec)
			if store.loadCalls != 0 {
				t.Fatalf("坏请求体不该读设置，实际 LoadSettings %d 次", store.loadCalls)
			}
			if store.applyCalls != 0 {
				t.Fatalf("坏请求体不该写设置，实际 ApplySettings %d 次", store.applyCalls)
			}
			if len(reload.exprs) != 0 {
				t.Fatalf("坏请求体不该重排 cron，实际 %v", reload.exprs)
			}
		})
	}
}

// TestAdminSettingsPutValidation 覆盖字段校验失败 → 422/42201（Requirement 4、5、6.3）：
// 消息点名字段、零写入、不重排；秘密字段的错误消息不回显提交值。
func TestAdminSettingsPutValidation(t *testing.T) {
	const webhookSentinel = "sentinel-not-a-url-9a1b"

	tooMany := make([]model.Building, 0, model.MaxWhitelistEntries+1)
	for i := 0; i <= model.MaxWhitelistEntries; i++ {
		tooMany = append(tooMany, model.Building{ID: fmt.Sprintf("B%03d", i), Name: "教学楼"})
	}
	tooManyBody, err := json.Marshal(map[string]any{"building_whitelist": tooMany})
	if err != nil {
		t.Fatalf("用例构造失败：%v", err)
	}

	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"cron 字段数不是 5", `{"cron_expr":"0 10 4 * * 6"}`, model.SettingCronExpr},
		{"cron 全空白", `{"cron_expr":"   "}`, model.SettingCronExpr},
		{"cron 取值越界", `{"cron_expr":"60 * * * *"}`, model.SettingCronExpr},
		{"白名单编号为空", `{"building_whitelist":[{"jxlbh":"  ","name":"综合教学楼"}]}`, model.SettingBuildingWhitelist},
		{"白名单展示名为空", `{"building_whitelist":[{"jxlbh":"Q10","name":"  "}]}`, model.SettingBuildingWhitelist},
		{"白名单编号 trim 后重复", `{"building_whitelist":[{"jxlbh":"Q10","name":"甲"},{"jxlbh":" Q10 ","name":"乙"}]}`, model.SettingBuildingWhitelist},
		{"白名单超过 200 条", string(tooManyBody), model.SettingBuildingWhitelist},
		{"ocr 地址不是 http(s)", `{"ocr_base_url":"ftp://ocr:8000"}`, model.SettingOCRBaseURL},
		{"秘密地址不是 http(s)", `{"feishu_webhook":"` + webhookSentinel + `"}`, model.SettingFeishuWebhook},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore()
			reload := &reloadRecorder{}
			srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

			rec := serveAdminSettings(srv, adminSettingsPutRequest(tc.body, testAdminRawToken, testAdminCSRF))

			body := assertContractFailure(t, rec, http.StatusUnprocessableEntity, codeSettingInvalid)
			if message, _ := body["message"].(string); !strings.Contains(message, tc.wantField) {
				t.Fatalf("42201 文案应点名字段 %q，实际 %q", tc.wantField, message)
			}
			assertNoStore(t, rec)
			if store.applyCalls != 0 {
				t.Fatalf("校验失败不该写库，实际 ApplySettings %d 次", store.applyCalls)
			}
			if len(reload.exprs) != 0 {
				t.Fatalf("校验失败不该重排 cron，实际 %v", reload.exprs)
			}
			// 秘密字段的形状错误只报字段与原因，不回显提交值（CP1）。
			if strings.Contains(rec.Body.String(), webhookSentinel) {
				t.Fatalf("422 响应泄露了秘密提交值: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "v1:") {
				t.Fatalf("422 响应不该出现密文前缀: %s", rec.Body.String())
			}
		})
	}
}

// TestAdminSettingsPutStoreFailure 覆盖仓储失败 → 500/50000（Requirement 6.4、design 错误表）：
// 固定文案、不透 SQL 细节、不重排；GET 的读失败同口径。
func TestAdminSettingsPutStoreFailure(t *testing.T) {
	t.Run("写入失败", func(t *testing.T) {
		const sqlDetail = "insert settings: sentinel-sql-2b3c"
		store, sealer := newActiveSettingsStore()
		store.applyErr = fmt.Errorf("%s: %w", sqlDetail, model.ErrSettingStoreFailed)
		reload := &reloadRecorder{}
		srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(
			`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))

		body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
		if message, _ := body["message"].(string); message != "内部错误" {
			t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
		}
		if strings.Contains(rec.Body.String(), "sentinel-sql-2b3c") {
			t.Fatalf("响应泄露了存储层细节: %s", rec.Body.String())
		}
		assertNoStore(t, rec)
		if len(reload.exprs) != 0 {
			t.Fatalf("写库失败不该重排 cron，实际 %v", reload.exprs)
		}
	})

	t.Run("读取失败", func(t *testing.T) {
		store, sealer := newActiveSettingsStore()
		store.loadErr = fmt.Errorf("select settings: %w", model.ErrSettingStoreFailed)
		srv := newActiveAdminSettingsServer(store, sealer, nil)

		rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

		assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
		assertNoStore(t, rec)
		if strings.Contains(rec.Body.String(), "select settings") {
			t.Fatalf("响应泄露了 SQL 细节: %s", rec.Body.String())
		}
	})
}

// TestAdminSettingsPutReadBackFailure 覆盖「事务已提交、回读失败」→ 500/50000
// （design 错误表「ApplySettings 已成功但回读失败」那一行）：
//
// 请求体提交了 cron_expr，正常路径会触发重排；但 Patch 在回读时失败，服务层拿不到
// 新快照，handler 也就没有可交给调度器的表达式——所以「已提交 cron_expr」并不等于
// 「一定重排」。同时断言写只开了一次事务、值确实落库（不回滚已提交的事务），
// 以及 SQL 细节不进响应（50000 固定文案）。
func TestAdminSettingsPutReadBackFailure(t *testing.T) {
	const sqlDetail = "sentinel-sql-7d8e"
	store, sealer := newActiveSettingsStore()
	store.readBackErr = fmt.Errorf("select settings: %s: %w", sqlDetail, model.ErrSettingStoreFailed)
	reload := &reloadRecorder{}
	srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

	rec := serveAdminSettings(srv, adminSettingsPutRequest(
		`{"cron_expr":"10 4 * * 6","jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))

	body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if message, _ := body["message"].(string); message != "内部错误" {
		t.Fatalf("500 文案应固定为「内部错误」，实际 %q", message)
	}
	if strings.Contains(rec.Body.String(), sqlDetail) {
		t.Fatalf("响应泄露了回读失败的 SQL 细节: %s", rec.Body.String())
	}
	assertNoStore(t, rec)

	// 写已提交：一次事务，新值在库里。
	if store.applyCalls != 1 {
		t.Fatalf("一次保存应只开一次事务，实际 ApplySettings %d 次", store.applyCalls)
	}
	if row, found := store.row(model.SettingCronExpr); !found || row.Value != "10 4 * * 6" {
		t.Fatalf("回读失败不回滚已提交的事务，cron_expr 应已落库，实际 %+v（found=%v）", row, found)
	}

	// 没有回读到快照，调度器不该收到任何表达式。
	if len(reload.exprs) != 0 {
		t.Fatalf("回读失败不该重排 cron，实际 %v", reload.exprs)
	}
}

// adminSettingsFailureLogs 是设置保存失败的三类服务端日志文案（CP5），三者必须互不相同：
//
//	写入失败（事务已回滚）       → 「保存设置失败」
//	回读失败（写入已提交）       → 「回读设置失败（写入已提交）」
//	空补丁回读失败（没有写过库） → 「设置读写失败」
//
// 三类失败对客户端完全一样：500 / 50000 / 「内部错误」，唯一的区别在服务端日志。
// 文案撞车等于把「这次到底写没写、写进去了没有」这个唯一线索弄丢。
const (
	adminSettingsWriteFailureLog = "保存设置失败"
	adminSettingsReadBackLog     = "回读设置失败（写入已提交）"
	adminSettingsEmptyPatchLog   = "设置读写失败"
)

// TestAdminSettingsPutFailureLogDistinguishesReadBack 覆盖 CP5 两种「真的写过库」的失败
// 在服务端日志上的区分：
//
//   - 写入失败（事务已回滚）→ 「保存设置失败」；
//   - 回读失败（写入已提交）→ 「回读设置失败（写入已提交）」。
//
// 两者对客户端完全一样：500 / 50000 / 固定文案「内部错误」。正因如此，「设置到底存进去
// 没有」只能由日志回答；文案混成同一句会让运维误以为设置丢了，去做多余的重放甚至回滚。
// 本用例直接断言两条路径各自的 msg，也就断言了它们不相等。
// 第三类「空补丁（没写过库）的回读失败」没有事务可讲，见 TestAdminSettingsPutEmptyPatchFailureLog。
func TestAdminSettingsPutFailureLogDistinguishesReadBack(t *testing.T) {
	cases := []struct {
		name   string
		want   string
		inject func(*settingsStoreStub)
	}{
		{
			name: "写入失败记保存失败",
			want: adminSettingsWriteFailureLog,
			inject: func(store *settingsStoreStub) {
				store.applyErr = fmt.Errorf("insert settings: %w", model.ErrSettingStoreFailed)
			},
		},
		{
			name: "回读失败记写入已提交",
			want: adminSettingsReadBackLog,
			inject: func(store *settingsStoreStub) {
				store.readBackErr = fmt.Errorf("select settings: %w", model.ErrSettingStoreFailed)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
			store, sealer := newActiveSettingsStore()
			tc.inject(store)
			srv := newAdminSettingsServer(logger, activeAdminStore(), store, sealer, nil)

			rec := serveAdminSettings(srv, adminSettingsPutRequest(
				`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))

			body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			if message, _ := body["message"].(string); message != "内部错误" {
				t.Fatalf("两条 500 路径的响应文案都应是固定的「内部错误」，实际 %q", message)
			}

			entry := singleAdminLogEntry(t, &logBuf)
			if entry.Level != "ERROR" {
				t.Fatalf("500 失败应记 ERROR，实际 %s", entry.Level)
			}
			if entry.Status != http.StatusInternalServerError || entry.Code != codeInternal {
				t.Fatalf("日志的 status/code 应是本次响应的 500/50000，实际 %d/%d", entry.Status, entry.Code)
			}
			if entry.Msg != tc.want {
				t.Fatalf("日志文案应为 %q，实际 %q", tc.want, entry.Msg)
			}
			if entry.RequestID != contractRequestID(t, rec) {
				t.Fatalf("日志 request_id %q 与响应不一致", entry.RequestID)
			}
			for _, detail := range []string{"insert settings", "select settings"} {
				if strings.Contains(logBuf.String(), detail) {
					t.Fatalf("日志泄露了存储层细节 %q：%s", detail, logBuf.String())
				}
			}
		})
	}
}

// TestAdminSettingsPutEmptyPatchFailureLog 覆盖「空补丁的回读失败」的日志文案（CP5）：
//
// PUT `{}` / `null` 一个字段都没提交，Service.Patch 因此不调 ApplySettings、只做回读。
// 这次回读失败既不是「写入失败」（没有事务可回滚），也没有「写入已提交」可讲——
// 一个键都没写过。它必须记中性文案「设置读写失败」，且与另外两类失败的日志文案都不同：
// 记成「保存设置失败」会让运维去追一次根本没发生的写入，记成「写入已提交」又会让人
// 以为设置已经落库、不必重放。三者对客户端都是 500 / 50000 / 「内部错误」，
// 所以这条断言是唯一能锁住「读失败没被说成写失败」的地方。
//
// 第三例用**非哨兵**读错误：分派只看补丁是否为空（emptySettingsPatch），不与
// model.ErrSettingStoreFailed 绑定。两个条件若写在一起，这种错误会掉进默认分支、
// 又被记成「保存设置失败」——那正是本用例要挡住的那条回退。
func TestAdminSettingsPutEmptyPatchFailureLog(t *testing.T) {
	const sqlDetail = "sentinel-sql-3c4d"

	cases := []struct {
		name string
		body string
		// sentinel 标记 loadErr 是否包了 ErrSettingStoreFailed；分派不该看这个布尔，
		// 它只用来自检「非哨兵」那一例真的没包哨兵，别让用例名与事实脱节。
		sentinel bool
		loadErr  error
	}{
		{
			name:     "请求体 {} + 哨兵读错误",
			body:     `{}`,
			sentinel: true,
			loadErr:  fmt.Errorf("select settings: %s: %w", sqlDetail, model.ErrSettingStoreFailed),
		},
		{
			name:     "请求体 null + 哨兵读错误",
			body:     `null`,
			sentinel: true,
			loadErr:  fmt.Errorf("select settings: %s: %w", sqlDetail, model.ErrSettingStoreFailed),
		},
		{
			name:     "请求体 {} + 非哨兵读错误",
			body:     `{}`,
			sentinel: false,
			loadErr:  fmt.Errorf("select settings: %s: connection reset", sqlDetail),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.loadErr, model.ErrSettingStoreFailed); got != tc.sentinel {
				t.Fatalf("用例自检失败：loadErr 的哨兵状态是 %v，期望 %v", got, tc.sentinel)
			}

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
			store, sealer := newActiveSettingsStore()
			// loadErr 让每一次 Load 都失败：空补丁不写库，Patch 的首次读就是那次回读。
			store.loadErr = tc.loadErr
			reload := &reloadRecorder{}
			srv := newAdminSettingsServer(logger, activeAdminStore(), store, sealer, reload.reload)

			rec := serveAdminSettings(srv, adminSettingsPutRequest(tc.body, testAdminRawToken, testAdminCSRF))

			failure := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
			if message, _ := failure["message"].(string); message != "内部错误" {
				t.Fatalf("空补丁失败的响应文案也应是固定的「内部错误」，实际 %q", message)
			}
			assertNoStore(t, rec)

			// 没有写过库：零次 ApplySettings、没有重排，SQL 细节也不进响应。
			if store.applyCalls != 0 {
				t.Fatalf("空补丁不该开事务，实际 ApplySettings %d 次", store.applyCalls)
			}
			if len(reload.exprs) != 0 {
				t.Fatalf("空补丁失败不该重排 cron，实际 %v", reload.exprs)
			}
			if strings.Contains(rec.Body.String(), sqlDetail) {
				t.Fatalf("响应泄露了回读失败的 SQL 细节: %s", rec.Body.String())
			}

			entry := singleAdminLogEntry(t, &logBuf)
			if entry.Level != "ERROR" {
				t.Fatalf("500 失败应记 ERROR，实际 %s", entry.Level)
			}
			if entry.Status != http.StatusInternalServerError || entry.Code != codeInternal {
				t.Fatalf("日志的 status/code 应是本次响应的 500/50000，实际 %d/%d", entry.Status, entry.Code)
			}
			if entry.Msg != adminSettingsEmptyPatchLog {
				t.Fatalf("空补丁的回读失败应记中性文案 %q，实际 %q", adminSettingsEmptyPatchLog, entry.Msg)
			}
			// 与另外两类失败（写入失败、回读失败（写入已提交））的日志文案都不同。
			if entry.Msg == adminSettingsWriteFailureLog || entry.Msg == adminSettingsReadBackLog {
				t.Fatalf("空补丁失败的日志文案 %q 与另一类失败撞车了", entry.Msg)
			}
			if entry.RequestID != contractRequestID(t, rec) {
				t.Fatalf("日志 request_id %q 与响应不一致", entry.RequestID)
			}
			if strings.Contains(logBuf.String(), sqlDetail) || strings.Contains(logBuf.String(), "select settings") {
				t.Fatalf("日志泄露了存储层细节：%s", logBuf.String())
			}
		})
	}
}

// TestEmptySettingsPatch 逐字段钉住「空补丁」判据（CP5）：七个字段各提交一项都必须
// 判为非空，只有七个指针全 nil（`{}` / `null`）才算空。漏判任何一个字段，该字段的
// 单独保存失败时就会被记成「设置读写失败」，可它其实开过写事务——日志会把运维引向
// 一次并不存在的「纯读失败」。新增设置项时这张表也要跟着长一行。
func TestEmptySettingsPatch(t *testing.T) {
	whitelist := []model.Building{{ID: "Q10", Name: "综合楼"}}

	cases := []struct {
		name  string
		patch settings.Patch
	}{
		{name: "jw_username", patch: settings.Patch{JWUsername: stringPtr("admin")}},
		{name: "jw_password", patch: settings.Patch{JWPassword: stringPtr("Pa55word")}},
		{name: "ocr_base_url", patch: settings.Patch{OCRBaseURL: stringPtr("https://ocr.example.com/api")}},
		{name: "feishu_webhook", patch: settings.Patch{FeishuWebhook: stringPtr("https://open.feishu.cn/open-apis/bot/v2/hook/x")}},
		{name: "feishu_sign_secret", patch: settings.Patch{FeishuSignSecret: stringPtr("sign-secret")}},
		{name: "cron_expr", patch: settings.Patch{CronExpr: stringPtr(model.DefaultCronExpr)}},
		{name: "building_whitelist", patch: settings.Patch{Whitelist: &whitelist}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if emptySettingsPatch(tc.patch) {
				t.Fatalf("只提交 %s 不应算空补丁", tc.name)
			}
		})
	}

	if !emptySettingsPatch(settings.Patch{}) {
		t.Fatalf("七个字段全 nil 的补丁才应算空补丁")
	}
}

// TestAdminSettingsPutKeyUnavailable 覆盖密钥不可用 + 非空秘密 → 500/50000 且零写入（Requirement 3.3、CP3）。
func TestAdminSettingsPutKeyUnavailable(t *testing.T) {
	store, sealer := newActiveSettingsStore()
	sealer.err = model.ErrKeyUnavailable
	reload := &reloadRecorder{}
	srv := newActiveAdminSettingsServer(store, sealer, reload.reload)

	rec := serveAdminSettings(srv, adminSettingsPutRequest(
		`{"jw_password":"sentinel-password-c4d5"}`, testAdminRawToken, testAdminCSRF))

	body := assertContractFailure(t, rec, http.StatusInternalServerError, codeInternal)
	if message, _ := body["message"].(string); !strings.Contains(message, "密钥") {
		t.Fatalf("500 文案应点名密钥，实际 %q", message)
	}
	assertNoStore(t, rec)
	if store.applyCalls != 0 {
		t.Fatalf("密钥不可用时应零写入，实际 ApplySettings %d 次", store.applyCalls)
	}
	if len(reload.exprs) != 0 {
		t.Fatalf("失败请求不该重排 cron，实际 %v", reload.exprs)
	}
	if strings.Contains(rec.Body.String(), "sentinel-password-c4d5") {
		t.Fatalf("响应泄露了秘密明文: %s", rec.Body.String())
	}
}

// TestAdminSettingsGetKeyUnavailable 覆盖「密钥不可用仍可读」（Requirement 10.6、CP3 的读侧）：
// GET 只读库、不解封，所以密钥坏了也照常 200 并给全 8 个键；*_set 仍由行存在性决定、原样返回，
// 且全程不取密钥——否则一个坏掉的密钥文件会把设置页整页打成 50000，管理员连改它都做不到。
func TestAdminSettingsGetKeyUnavailable(t *testing.T) {
	store, sealer := newActiveSettingsStore(
		settingRow(model.SettingJWUsername, "teacher01", false),
		sealedSettingRow(model.SettingJWPassword, "sentinel-cipher-2e3f"),
		settingRow(model.SettingOCRBaseURL, "http://ocr:8000", false),
		sealedSettingRow(model.SettingFeishuWebhook, "sentinel-webhook-4a5b"),
		settingRow(model.SettingCronExpr, "10 4 * * 6", false),
		settingRow(model.SettingBuildingWhitelist, `[{"jxlbh":"Q10","name":"综合教学楼"}]`, false),
	)
	sealer.err = model.ErrKeyUnavailable
	srv := newActiveAdminSettingsServer(store, sealer, nil)

	rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	assertNoStore(t, rec)
	if data["jw_username"] != "teacher01" || data["ocr_base_url"] != "http://ocr:8000" {
		t.Fatalf("密钥不可用不该影响明文项，实际 %#v", data)
	}
	// *_set 只由行存在性决定：设置了仍是 true，没设置过仍是 false。
	if data["jw_password_set"] != true {
		t.Fatalf("密钥不可用时 jw_password_set 应仍为 true，实际 %#v", data["jw_password_set"])
	}
	if data["feishu_webhook_set"] != true {
		t.Fatalf("密钥不可用时 feishu_webhook_set 应仍为 true，实际 %#v", data["feishu_webhook_set"])
	}
	if data["feishu_sign_secret_set"] != false {
		t.Fatalf("没设置过的秘密项应仍为 false，实际 %#v", data["feishu_sign_secret_set"])
	}
	if sealer.calls != 0 {
		t.Fatalf("GET 不该取密钥，实际 Current() 调用 %d 次", sealer.calls)
	}
	assertSecretNotEchoed(t, rec, "sentinel-cipher-2e3f", "sentinel-webhook-4a5b")
}

// TestAdminSettingsBodyLimit64KiB 覆盖 64 KiB 体边界（Requirement 6.1）：
// 恰好 64 KiB（含分块传输）通过并落库，多 1 字节 → 400/40001 且零写入。
//
// 体内容用多条合法白名单条目拼满：白名单的 `jxlbh`/`name` 各有 4096 字节上界，
// 单条 65 KiB 的 name 会被 42201 挡下，那样测到的是字段校验而不是体门。
func TestAdminSettingsBodyLimit64KiB(t *testing.T) {
	const prefix = `{"building_whitelist":[`
	const suffix = `]}`
	const entryCount = 16

	buildBody := func(extra int) string {
		ids := make([]string, entryCount)
		fixed := len(prefix) + len(suffix)
		for i := range ids {
			ids[i] = fmt.Sprintf("B%04d", i)
			fixed += len(`{"jxlbh":"` + ids[i] + `","name":""}`)
			if i > 0 {
				fixed++ // 条目之间的逗号
			}
		}

		names := adminSettingsBodyLimit + extra - fixed
		if names <= 0 || names > entryCount*model.MaxSettingValueBytes {
			t.Fatalf("用例构造错误：name 总长度 %d 不在 %d 条的合法范围内", names, entryCount)
		}

		entries := make([]string, 0, entryCount)
		remaining := names
		for i, id := range ids {
			length := min(remaining, model.MaxSettingValueBytes)
			if i == entryCount-1 {
				length = remaining // 最后一条吃掉余量
			}
			entries = append(entries, `{"jxlbh":"`+id+`","name":"`+strings.Repeat("a", length)+`"}`)
			remaining -= length
		}

		return prefix + strings.Join(entries, ",") + suffix
	}

	t.Run("恰好 64 KiB", func(t *testing.T) {
		store, sealer := newActiveSettingsStore()
		srv := newActiveAdminSettingsServer(store, sealer, nil)
		body := buildBody(0)
		if len(body) != adminSettingsBodyLimit {
			t.Fatalf("用例构造错误：期望体长 %d，实际 %d", adminSettingsBodyLimit, len(body))
		}

		rec := serveAdminSettings(srv, adminSettingsPutRequest(body, testAdminRawToken, testAdminCSRF))

		if rec.Code != http.StatusOK {
			t.Fatalf("恰好 64 KiB 应放行，实际 %d（body=%s）", rec.Code, rec.Body.String())
		}
		contractSuccessData(t, rec)
		if store.applyCalls != 1 {
			t.Fatalf("恰好 64 KiB 应落库一次，实际 ApplySettings %d 次", store.applyCalls)
		}
	})

	t.Run("恰好 64 KiB（分块传输）", func(t *testing.T) {
		store, sealer := newActiveSettingsStore()
		srv := newActiveAdminSettingsServer(store, sealer, nil)
		req := adminSettingsPutRequest(buildBody(0), testAdminRawToken, testAdminCSRF)
		req.ContentLength = -1 // 模拟分块传输：没有可依据的 Content-Length

		rec := serveAdminSettings(srv, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("恰好 64 KiB（分块）应放行，实际 %d（body=%s）", rec.Code, rec.Body.String())
		}
		if store.applyCalls != 1 {
			t.Fatalf("恰好 64 KiB（分块）应落库一次，实际 %d 次", store.applyCalls)
		}
	})

	t.Run("多 1 字节", func(t *testing.T) {
		store, sealer := newActiveSettingsStore()
		srv := newActiveAdminSettingsServer(store, sealer, nil)
		body := buildBody(1)

		rec := serveAdminSettings(srv, adminSettingsPutRequest(body, testAdminRawToken, testAdminCSRF))

		failure := assertContractFailure(t, rec, http.StatusBadRequest, codeBadRequest)
		if message, _ := failure["message"].(string); !strings.Contains(message, "64 KiB") {
			t.Fatalf("超限文案应点明 64 KiB，实际 %q", message)
		}
		assertNoStore(t, rec)
		if store.applyCalls != 0 {
			t.Fatalf("体超限时不该写库，实际 ApplySettings %d 次", store.applyCalls)
		}
	})
}

// TestAdminSettingsRequiresSession 覆盖 GET/PUT 的会话门槛（Requirement 1.5、10.1）：
// 缺 Cookie、空 Cookie、过期会话都 401/40101 并清 Cookie，且不读设置、不写库。
func TestAdminSettingsRequiresSession(t *testing.T) {
	expiredStore := func() *adminStoreStub {
		store := activeAdminStore()
		store.session = activeAdminSession(testAdminNow)
		return store
	}

	cases := []struct {
		name     string
		method   string
		admin    func() *adminStoreStub
		rawToken string
		csrf     string
	}{
		{"GET 没有 Cookie", http.MethodGet, activeAdminStore, "", ""},
		{"GET 会话过期", http.MethodGet, expiredStore, testAdminRawToken, ""},
		{"PUT 没有 Cookie", http.MethodPut, activeAdminStore, "", testAdminCSRF},
		{"PUT 会话过期", http.MethodPut, expiredStore, testAdminRawToken, testAdminCSRF},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore()
			srv := newAdminSettingsServer(quietLogger(), tc.admin(), store, sealer, nil)

			var rec *httptest.ResponseRecorder
			if tc.method == http.MethodGet {
				rec = serveAdminSettings(srv, adminSettingsGetRequest(tc.rawToken))
			} else {
				rec = serveAdminSettings(srv, adminSettingsPutRequest(
					`{"jw_username":"teacher01"}`, tc.rawToken, tc.csrf))
			}

			assertContractFailure(t, rec, http.StatusUnauthorized, codeSessionInvalid)
			assertNoStore(t, rec)
			assertClearedCookie(t, rec, true)
			if store.loadCalls != 0 {
				t.Fatalf("会话无效时不该读设置，实际 LoadSettings %d 次", store.loadCalls)
			}
			if store.applyCalls != 0 {
				t.Fatalf("会话无效时不该写设置，实际 ApplySettings %d 次", store.applyCalls)
			}
		})
	}
}

// TestAdminSettingsRequiresCSRF 覆盖 PUT 的 CSRF 门槛（Requirement 10.1）：
// 缺失或不一致 → 403/40301，保留会话、零读写；GET 是读端点，不需要 CSRF。
func TestAdminSettingsRequiresCSRF(t *testing.T) {
	for _, csrf := range []string{"", "wrong-csrf-token"} {
		name := "缺失令牌"
		if csrf != "" {
			name = "令牌不一致"
		}
		t.Run(name, func(t *testing.T) {
			store, sealer := newActiveSettingsStore()
			srv := newActiveAdminSettingsServer(store, sealer, nil)

			rec := serveAdminSettings(srv, adminSettingsPutRequest(
				`{"jw_username":"teacher01"}`, testAdminRawToken, csrf))

			assertContractFailure(t, rec, http.StatusForbidden, codeCSRFInvalid)
			assertNoStore(t, rec)
			assertNoSetCookie(t, rec)
			if store.loadCalls != 0 {
				t.Fatalf("CSRF 失败时不该读设置，实际 %d 次", store.loadCalls)
			}
			if store.applyCalls != 0 {
				t.Fatalf("CSRF 失败时不该写设置，实际 %d 次", store.applyCalls)
			}
			if strings.Contains(rec.Body.String(), testAdminCSRF) {
				t.Fatalf("响应泄露了会话的 CSRF 令牌: %s", rec.Body.String())
			}
		})
	}

	t.Run("GET 不需要 CSRF", func(t *testing.T) {
		store, sealer := newActiveSettingsStore()
		srv := newActiveAdminSettingsServer(store, sealer, nil)

		rec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))

		contractSuccessData(t, rec)
		assertNoStore(t, rec)
	})
}

// TestAdminSettingsNoStore 覆盖设置端点全部响应的缓存控制（Requirement 10.2、CP18）：
// 成功与每一种错误（400/401/403/422/500）都必须带 Cache-Control: no-store。
func TestAdminSettingsNoStore(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(t *testing.T) *httptest.ResponseRecorder
	}{
		{"GET 成功", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsGetRequest(testAdminRawToken))
		}},
		{"GET 缺会话", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsGetRequest(""))
		}},
		{"GET 读库失败", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			store.loadErr = model.ErrSettingStoreFailed
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsGetRequest(testAdminRawToken))
		}},
		{"PUT 成功", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))
		}},
		{"PUT 请求体非法", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{"cron_express":"x"}`, testAdminRawToken, testAdminCSRF))
		}},
		{"PUT 缺会话", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{}`, "", testAdminCSRF))
		}},
		{"PUT CSRF 不符", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{}`, testAdminRawToken, "wrong"))
		}},
		{"PUT 校验失败", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{"cron_expr":"0 10 4 * * 6"}`, testAdminRawToken, testAdminCSRF))
		}},
		{"PUT 写库失败", func(*testing.T) *httptest.ResponseRecorder {
			store, sealer := newActiveSettingsStore()
			store.applyErr = model.ErrSettingStoreFailed
			return serveAdminSettings(newActiveAdminSettingsServer(store, sealer, nil),
				adminSettingsPutRequest(`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))
		}},
		{"PUT 未装配", func(*testing.T) *httptest.ResponseRecorder {
			srv := New(quietLogger(), nil, nil, nil, nil, WithAdmin(activeAdminStore(), nil))
			srv.clock = func() time.Time { return testAdminNow }
			return serveAdminSettings(srv, adminSettingsPutRequest(
				`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))
		}},
	}
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			assertNoStore(t, tc.run(t))
		})
	}
}

// TestAdminSettingsWithoutService 覆盖未装配 WithAdminSettings（Requirement 10.7）：
// 两个端点仍注册，按 500/50000 返回，不以 404 掩盖装配缺失。
// 装配缺失优先于请求体解析：未装配 + `not-json` 体仍是 50000；但体门与会话/CSRF
// 在 handler 外层，Content-Type 不符仍先 40001（design 错误表的对应行）。
func TestAdminSettingsWithoutService(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil, WithAdmin(activeAdminStore(), nil))
	srv.clock = func() time.Time { return testAdminNow }

	getRec := serveAdminSettings(srv, adminSettingsGetRequest(testAdminRawToken))
	assertContractFailure(t, getRec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, getRec)

	putRec := serveAdminSettings(srv, adminSettingsPutRequest(
		`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF))
	assertContractFailure(t, putRec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, putRec)

	// 非法请求体也轮不到 40001：handler 的 nil 检查在严格解码之前。
	putBadBodyRec := serveAdminSettings(srv, adminSettingsPutRequest(
		`not-json`, testAdminRawToken, testAdminCSRF))
	assertContractFailure(t, putBadBodyRec, http.StatusInternalServerError, codeInternal)
	assertNoStore(t, putBadBodyRec)

	// 体门在 handler 之前：Content-Type 不符仍按 40001 返回，不被装配缺失盖住。
	badTypeReq := adminSettingsPutRequest(
		`{"jw_username":"teacher01"}`, testAdminRawToken, testAdminCSRF)
	badTypeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badTypeRec := serveAdminSettings(srv, badTypeReq)
	assertContractFailure(t, badTypeRec, http.StatusBadRequest, codeBadRequest)
	assertNoStore(t, badTypeRec)
}

// TestAdminSettingsReloadCronFailure 覆盖「保存成功但重排失败」（design「管理 API 契约」）：
// 响应仍是 200 + 新快照，只记一条 ERROR；日志字段仍在白名单内，且与响应共用 request_id。
// 日志里的 status=200 与 code=0 也被锁住：这条日志属于「保存成功」的那次响应。
func TestAdminSettingsReloadCronFailure(t *testing.T) {
	const passwordSentinel = "sentinel-password-e5f6"

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	store, sealer := newActiveSettingsStore()
	reload := &reloadRecorder{err: errors.New("调度器拒绝表达式")}
	srv := newAdminSettingsServer(logger, activeAdminStore(), store, sealer, reload.reload)

	rec := serveAdminSettings(srv, adminSettingsPutRequest(
		`{"cron_expr":"10 4 * * 6","jw_password":"`+passwordSentinel+`"}`,
		testAdminRawToken, testAdminCSRF))

	data := contractSuccessData(t, rec)
	assertSettingsDataKeys(t, data)
	if data["cron_expr"] != "10 4 * * 6" {
		t.Fatalf("重排失败不回滚设置，cron_expr 应为新值，实际 %#v", data["cron_expr"])
	}
	// 设置已经完整落库，重排失败只影响本次进程内的计划。
	if _, found := store.row(model.SettingJWPassword); !found {
		t.Fatal("重排失败不该回滚已保存的秘密项")
	}

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("重排失败应只记 1 条日志，实际 %d 条：%s", len(lines), logBuf.String())
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
	if entry.Level != "ERROR" {
		t.Fatalf("重排失败应记 ERROR，实际 %s", entry.Level)
	}
	// status/code 也锁死：这条日志描述的是「保存已经成功、只是内存里的计划没换」，
	// 所以它记的是本次响应的 200 与业务码 0，而不是 50000。改这两个字段就等于改契约
	// （排障时会误以为保存失败），必须同步改这里与 design。
	if entry.Status != http.StatusOK {
		t.Fatalf("重排失败日志的 status 应是响应实际值 200，实际 %d", entry.Status)
	}
	if entry.Code != 0 {
		t.Fatalf("重排失败日志的 code 应是保存成功的 0，实际 %d", entry.Code)
	}
	if entry.RequestID != contractRequestID(t, rec) {
		t.Fatalf("日志 request_id %q 与响应不一致", entry.RequestID)
	}
	if entry.Path != "/api/admin/settings" || entry.Method != http.MethodPut {
		t.Fatalf("日志方法/路径不符：%s %s", entry.Method, entry.Path)
	}
	for _, forbidden := range []string{passwordSentinel, "调度器拒绝表达式", "v1:"} {
		if strings.Contains(logBuf.String(), forbidden) {
			t.Fatalf("日志泄露了 %q：%s", forbidden, logBuf.String())
		}
	}
}

// contractRequestID 取出成功响应信封里的 request_id。
func contractRequestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env contractEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	return env.RequestID
}

// singleAdminLogEntry 解析日志缓冲里唯一一条 JSON 日志行。
// 多条或零条都直接失败：500 的失败路径应当只留一条 ERROR（adminFail 的一个出口）。
func singleAdminLogEntry(t *testing.T, logBuf *bytes.Buffer) adminLogEntry {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("应只记 1 条日志，实际 %d 条：%s", len(lines), logBuf.String())
	}
	var entry adminLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v, %s", err, lines[0])
	}
	return entry
}

// TestAdminSettingsReloadCronNil 覆盖 reloadCron 为 nil（调度器未装配）：
// 保存照常成功，不 panic、不报错（design「管理 API 契约」最后一条）。
func TestAdminSettingsReloadCronNil(t *testing.T) {
	store, sealer := newActiveSettingsStore()
	srv := newActiveAdminSettingsServer(store, sealer, nil)

	rec := serveAdminSettings(srv, adminSettingsPutRequest(
		`{"cron_expr":"10 4 * * 6"}`, testAdminRawToken, testAdminCSRF))

	data := contractSuccessData(t, rec)
	if data["cron_expr"] != "10 4 * * 6" {
		t.Fatalf("cron_expr 不符：%#v", data["cron_expr"])
	}
	if store.applyCalls != 1 {
		t.Fatalf("保存应落库一次，实际 %d 次", store.applyCalls)
	}
}
