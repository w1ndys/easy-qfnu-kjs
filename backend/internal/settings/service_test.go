package settings

// 本文件是 Service 的替身测试：Store 用内存版替身（就地生效，好让 Patch 的回读看到刚
// 写入的值，并记录「读了几次、写了几批、每批的 now」），Sealer 用一把确定的测试密钥。
//
// 断言的重点是顺序、次数与密文形态，而不是错误文案：
//   - 一次保存只调一次 ApplySettings，同一批同一个 now；
//   - 校验失败、非空秘密 + 密钥不可用时 ApplySettings 调用次数为 0（零写入）；
//   - 只把秘密设为空串（清除）时不碰 Current()，且照常删除；
//   - 秘密落库的是 v1: 信封，AAD 是 settings/<key>（用同一把密钥按正确 AAD 能解开、
//     按别的键的 AAD 解不开，等于从外部钉住 AAD）；
//   - Load 的 *_set 只由行存在性决定，Snapshot 里没有秘密明文字段。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
)

// fakeStore 是 Store 的内存替身：rows 模拟 settings 表，ApplySettings 就地生效。
// 记录 loadCalls/applyCalls 与每批的 now，因为「一次 ApplySettings」「同一批同一个 now」
// 「密钥不可用时零写入」这三条只能从调用侧观察。
type fakeStore struct {
	rows []model.SettingRow

	loadCalls  int
	applyCalls int
	batches    [][]model.SettingChange
	nows       []time.Time

	loadErr  error
	applyErr error
}

func (s *fakeStore) LoadSettings(context.Context) ([]model.SettingRow, error) {
	s.loadCalls++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return append([]model.SettingRow(nil), s.rows...), nil
}

func (s *fakeStore) ApplySettings(_ context.Context, changes []model.SettingChange, now time.Time) error {
	s.applyCalls++
	if s.applyErr != nil {
		return s.applyErr
	}
	s.batches = append(s.batches, append([]model.SettingChange(nil), changes...))
	s.nows = append(s.nows, now)

	for _, change := range changes {
		s.applyOne(change, now)
	}
	return nil
}

// applyOne 把一条变更按 Key 落到 rows 上：删除即摘掉行，否则覆盖/追加。
// 与真实仓储的差异只有「不判事务」——原子性由仓储集成测试覆盖，本文件只关心服务层
// 交给仓储的是「一批」还是「逐条」。
func (s *fakeStore) applyOne(change model.SettingChange, now time.Time) {
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
		// 删除一个不存在的键是幂等的，与仓储同口径。
		return
	}
	s.rows = append(s.rows, model.SettingRow{
		Key:       change.Key,
		Value:     change.Value,
		Secret:    change.Secret,
		UpdatedAt: now,
	})
}

// fakeSealer 是 Sealer 的替身：记录 Current() 被调了几次，可注入错误。
type fakeSealer struct {
	key   secret.Key
	err   error
	calls int
}

func (s *fakeSealer) Current() (secret.Key, error) {
	s.calls++
	if s.err != nil {
		return secret.Key{}, s.err
	}
	return s.key, nil
}

// serviceTestKey 返回一把确定性的测试密钥，内容与生产无关。
func serviceTestKey() secret.Key {
	var key secret.Key
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// fixedClock 返回一个恒定的时钟，用来钉住同一批写入的 updated_at。
func fixedClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

// newTestService 造一个「内存仓储 + 可用密钥 + 真实时钟」的服务，返回替身以便断言次数。
func newTestService(rows ...model.SettingRow) (*Service, *fakeStore, *fakeSealer) {
	store := &fakeStore{rows: rows}
	sealer := &fakeSealer{key: serviceTestKey()}
	return New(store, sealer, nil), store, sealer
}

// mustLoad 读一次快照，失败即终止用例。
func mustLoad(t *testing.T, svc *Service) Snapshot {
	t.Helper()
	snapshot, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load 应成功，实际 err=%v", err)
	}
	return snapshot
}

// changeKeys 取出变更的键序列，用来断言写入顺序固定。
func changeKeys(changes []model.SettingChange) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, change.Key)
	}
	return keys
}

// findChange 按设置键找一条变更，找不到就是用例前提被破坏。
func findChange(t *testing.T, changes []model.SettingChange, key string) model.SettingChange {
	t.Helper()
	for _, change := range changes {
		if change.Key == key {
			return change
		}
	}
	t.Fatalf("变更里应有 %s，实际只有 %v", key, changeKeys(changes))
	return model.SettingChange{}
}

// sealedRow 造一行「已加密的秘密设置」，AAD 与写入侧一致（settings/<key>）。
func sealedRow(t *testing.T, key secret.Key, setting, plaintext string) model.SettingRow {
	t.Helper()
	envelope, err := key.Seal(settingAAD(setting), []byte(plaintext))
	if err != nil {
		t.Fatalf("用例前提不成立：加封 %s 失败：%v", setting, err)
	}
	return model.SettingRow{Key: setting, Value: envelope, Secret: true}
}

// tamperedEnvelope 改动信封正中间的一个 base64url 字符：仍在同一分组内，解码不会因
// 尾部比特失败，因此必然落到 GCM tag 校验失败，用来钉住「篡改解不开」。
func tamperedEnvelope(t *testing.T, envelope string) string {
	t.Helper()
	const prefix = "v1:"
	if !strings.HasPrefix(envelope, prefix) {
		t.Fatalf("用例前提不成立：信封 %q 没有 v1: 前缀", envelope)
	}
	body := []byte(envelope[len(prefix):])
	mid := len(body) / 2
	if body[mid] == 'A' {
		body[mid] = 'B'
	} else {
		body[mid] = 'A'
	}
	return prefix + string(body)
}

// TestServiceLoad 覆盖快照组装口径（Requirement 1.1–1.3、3.4、9.5）：
// 缺行回落、*_set 只看行存在性、白名单顺序与坏 JSON、读失败上抛。
func TestServiceLoad(t *testing.T) {
	t.Run("空库回落", func(t *testing.T) {
		svc, store, _ := newTestService()

		snapshot := mustLoad(t, svc)

		if store.loadCalls != 1 {
			t.Fatalf("Load 应只读一次仓储，实际 %d 次", store.loadCalls)
		}
		if snapshot.JWUsername != "" || snapshot.OCRBaseURL != "" {
			t.Fatalf("缺行时字符串项应为空串，实际 %+v", snapshot)
		}
		if snapshot.JWPasswordSet || snapshot.FeishuWebhookSet || snapshot.FeishuSignSecretSet {
			t.Fatalf("缺行时三个 *_set 都应为 false，实际 %+v", snapshot)
		}
		if snapshot.CronExpr != model.DefaultCronExpr {
			t.Fatalf("cron_expr 缺行应回落默认值 %q，实际 %q", model.DefaultCronExpr, snapshot.CronExpr)
		}
		if snapshot.Whitelist == nil || len(snapshot.Whitelist) != 0 {
			t.Fatalf("白名单缺行应为非 nil 空切片，实际 %#v", snapshot.Whitelist)
		}
		if raw, err := json.Marshal(snapshot.Whitelist); err != nil || string(raw) != "[]" {
			t.Fatalf("空白名单应序列化为 []，实际 %s（err=%v）", raw, err)
		}
	})

	t.Run("set 标志只由行存在性决定", func(t *testing.T) {
		// jw_password 的值是空串、is_secret 列是 false：两者都不影响「已设置」
		// （Requirement 3.4、CP4）。另外插一行键外的值，读路径应忽略它。
		svc, _, _ := newTestService(
			model.SettingRow{Key: model.SettingJWPassword, Value: "", Secret: false},
			model.SettingRow{Key: model.SettingFeishuSignSecret, Value: "v1:whatever", Secret: false},
			model.SettingRow{Key: "cron_express", Value: "* * * * *"},
			model.SettingRow{Key: model.SettingJWUsername, Value: "teacher01"},
			model.SettingRow{Key: model.SettingOCRBaseURL, Value: "http://ocr:8000"},
			model.SettingRow{Key: model.SettingCronExpr, Value: "*/5 * * * *"},
		)

		snapshot := mustLoad(t, svc)

		if !snapshot.JWPasswordSet {
			t.Fatal("jw_password 行存在（值即使是空串）就应回显已设置")
		}
		if snapshot.FeishuWebhookSet {
			t.Fatal("feishu_webhook 没有行，应为未设置")
		}
		if !snapshot.FeishuSignSecretSet {
			t.Fatal("is_secret=false 的 feishu_sign_secret 也应按键名视为已设置")
		}
		if snapshot.JWUsername != "teacher01" || snapshot.OCRBaseURL != "http://ocr:8000" {
			t.Fatalf("非秘密项应原样回显，实际 %+v", snapshot)
		}
		if snapshot.CronExpr != "*/5 * * * *" {
			t.Fatalf("cron_expr 行存在时应原样回显，实际 %q", snapshot.CronExpr)
		}
	})

	t.Run("白名单保持库里的顺序", func(t *testing.T) {
		svc, _, _ := newTestService(model.SettingRow{
			Key:   model.SettingBuildingWhitelist,
			Value: `[{"jxlbh":"Q03","name":"老文史楼"},{"jxlbh":"Q10","name":"综合教学楼"}]`,
		})

		snapshot := mustLoad(t, svc)

		want := []model.Building{{ID: "Q03", Name: "老文史楼"}, {ID: "Q10", Name: "综合教学楼"}}
		if len(snapshot.Whitelist) != len(want) {
			t.Fatalf("白名单条数不符：got %+v want %+v", snapshot.Whitelist, want)
		}
		for i := range want {
			if snapshot.Whitelist[i] != want[i] {
				t.Fatalf("第 %d 条顺序或内容不符：got %+v want %+v", i+1, snapshot.Whitelist[i], want[i])
			}
		}
	})

	t.Run("白名单坏 JSON 回空数组而不是报错", func(t *testing.T) {
		svc, _, _ := newTestService(
			model.SettingRow{Key: model.SettingBuildingWhitelist, Value: "{不是数组"},
			model.SettingRow{Key: model.SettingCronExpr, Value: "10 4 * * 6"},
		)

		snapshot := mustLoad(t, svc)

		if snapshot.Whitelist == nil || len(snapshot.Whitelist) != 0 {
			t.Fatalf("坏 JSON 应回非 nil 空切片，实际 %#v", snapshot.Whitelist)
		}
		if snapshot.CronExpr != "10 4 * * 6" {
			t.Fatalf("一行坏数据不应影响其他设置项，实际 cron=%q", snapshot.CronExpr)
		}
	})

	t.Run("读失败原样上抛", func(t *testing.T) {
		loadErr := fmt.Errorf("%w: 注入的读取失败", model.ErrSettingStoreFailed)
		store := &fakeStore{loadErr: loadErr}
		svc := New(store, &fakeSealer{key: serviceTestKey()}, nil)

		if _, err := svc.Load(context.Background()); !errors.Is(err, model.ErrSettingStoreFailed) {
			t.Fatalf("应能 errors.Is 命中 model.ErrSettingStoreFailed，实际 err=%v", err)
		}
	})
}

// TestServicePatchWritesOnceAndRereads 是一个完整保存：七个字段全给，断言变更的键顺序、
// 落库值形态（秘密是信封、非秘密 trim、白名单 JSON）、只有一次 ApplySettings、一次回读，
// 以及响应快照就是刚写入的值。
func TestServicePatchWritesOnceAndRereads(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	sealer := &fakeSealer{key: serviceTestKey()}
	svc := New(store, sealer, fixedClock(now))

	snapshot, err := svc.Patch(context.Background(), Patch{
		JWUsername:       ptr("  teacher01  "),
		JWPassword:       ptr(" s3cret "),
		OCRBaseURL:       ptr("  " + validOCRURL + "  "),
		FeishuWebhook:    ptr(validWebhookURL),
		FeishuSignSecret: ptr("sign-secret"),
		CronExpr:         ptr("  " + model.DefaultCronExpr + "  "),
		Whitelist:        buildingListPtr(validWhitelist()),
	})
	if err != nil {
		t.Fatalf("合法保存应成功，实际 err=%v", err)
	}

	if store.applyCalls != 1 {
		t.Fatalf("一次保存应只调一次 ApplySettings，实际 %d 次", store.applyCalls)
	}
	if store.loadCalls != 1 {
		t.Fatalf("保存后应回读一次，实际 %d 次", store.loadCalls)
	}
	if !store.nows[0].Equal(now) {
		t.Fatalf("同一批写入应使用注入的时刻，got %v want %v", store.nows[0], now)
	}

	changes := store.batches[0]
	wantKeys := []string{
		model.SettingJWUsername, model.SettingJWPassword, model.SettingOCRBaseURL,
		model.SettingFeishuWebhook, model.SettingFeishuSignSecret, model.SettingCronExpr,
		model.SettingBuildingWhitelist,
	}
	gotKeys := changeKeys(changes)
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("变更条数不符：got %v want %v", gotKeys, wantKeys)
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Fatalf("变更顺序不符：got %v want %v", gotKeys, wantKeys)
		}
	}

	if got := findChange(t, changes, model.SettingJWUsername); got.Value != "teacher01" || got.Secret || got.Delete {
		t.Fatalf("jw_username 应是 trim 后的非秘密值，实际 %+v", got)
	}
	if got := findChange(t, changes, model.SettingOCRBaseURL); got.Value != validOCRURL {
		t.Fatalf("ocr_base_url 应是 trim 后的值，实际 %+v", got)
	}
	if got := findChange(t, changes, model.SettingCronExpr); got.Value != model.DefaultCronExpr {
		t.Fatalf("cron_expr 应是 trim 后的表达式，实际 %+v", got)
	}
	for _, key := range []string{model.SettingJWPassword, model.SettingFeishuWebhook, model.SettingFeishuSignSecret} {
		got := findChange(t, changes, key)
		if !got.Secret {
			t.Fatalf("%s 的 is_secret 应记 true，实际 %+v", key, got)
		}
		if got.Delete || !strings.HasPrefix(got.Value, "v1:") {
			t.Fatalf("%s 应落 v1: 信封，实际 %+v", key, got)
		}
	}
	wantWhitelist := `[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q11","name":"实验楼"}]`
	if got := findChange(t, changes, model.SettingBuildingWhitelist); got.Value != wantWhitelist {
		t.Fatalf("白名单落库形态不符：got %q want %q", got.Value, wantWhitelist)
	}

	if snapshot.JWUsername != "teacher01" || snapshot.OCRBaseURL != validOCRURL {
		t.Fatalf("回读快照应是刚写入的值，实际 %+v", snapshot)
	}
	if !snapshot.JWPasswordSet || !snapshot.FeishuWebhookSet || !snapshot.FeishuSignSecretSet {
		t.Fatalf("三个秘密写入后都应回显已设置，实际 %+v", snapshot)
	}
	if snapshot.CronExpr != model.DefaultCronExpr {
		t.Fatalf("回读的 cron_expr 不符：%q", snapshot.CronExpr)
	}
	if len(snapshot.Whitelist) != 2 || snapshot.Whitelist[0].ID != "Q10" || snapshot.Whitelist[1].ID != "Q11" {
		t.Fatalf("回读的白名单不符：%+v", snapshot.Whitelist)
	}

	// CP1：快照里没有秘密明文字段，序列化后不含明文。
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("快照应可序列化：%v", err)
	}
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "sign-secret") {
		t.Fatalf("快照序列化后不应出现秘密明文：%s", raw)
	}
}

// TestServicePatchSecretAAD 钉住秘密信封的 AAD 是 settings/<key>：
// 三个秘密共用一次 Current()，但各自用自己键名的 AAD 加封——按本键 AAD 能解回原明文，
// 按别的键的 AAD 解不开（CP2、Requirement 3.2）。
func TestServicePatchSecretAAD(t *testing.T) {
	store := &fakeStore{}
	sealer := &fakeSealer{key: serviceTestKey()}
	svc := New(store, sealer, nil)

	const (
		password = "plain-password"
		sign     = "sign-secret"
	)
	_, err := svc.Patch(context.Background(), Patch{
		JWPassword:       ptr(password),
		FeishuWebhook:    ptr(validWebhookURL),
		FeishuSignSecret: ptr(sign),
	})
	if err != nil {
		t.Fatalf("合法保存应成功，实际 err=%v", err)
	}
	if sealer.calls != 1 {
		t.Fatalf("同一批的三个秘密应只取一次密钥，实际 %d 次", sealer.calls)
	}

	changes := store.batches[0]
	key := serviceTestKey()
	cases := []struct {
		key       string
		plaintext string
	}{
		{model.SettingJWPassword, password},
		{model.SettingFeishuWebhook, validWebhookURL},
		{model.SettingFeishuSignSecret, sign},
	}
	for _, tc := range cases {
		envelope := findChange(t, changes, tc.key).Value

		plaintext, err := key.Open(settingAAD(tc.key), envelope)
		if err != nil {
			t.Fatalf("%s 应能用 settings/%s 的 AAD 解封，实际 err=%v", tc.key, tc.key, err)
		}
		if string(plaintext) != tc.plaintext {
			t.Fatalf("%s 解封出的明文不符：got %q want %q", tc.key, plaintext, tc.plaintext)
		}
	}

	// 换键名解不开：jw_password 的密文拿去按 feishu_webhook 的 AAD 解。
	passwordEnvelope := findChange(t, changes, model.SettingJWPassword).Value
	if _, err := key.Open(settingAAD(model.SettingFeishuWebhook), passwordEnvelope); !errors.Is(err, secret.ErrEnvelopeInvalid) {
		t.Fatalf("换键名的密文应解不开（ErrEnvelopeInvalid），实际 err=%v", err)
	}
}

// TestServicePatchSecretPlaintextNotTrimmed 钉住秘密明文不 trim：带前后空格的密码
// 落库后再解封，必须逐字节等于提交原文（trim 会改掉凭据）。
func TestServicePatchSecretPlaintextNotTrimmed(t *testing.T) {
	store := &fakeStore{}
	sealer := &fakeSealer{key: serviceTestKey()}
	svc := New(store, sealer, nil)

	const password = "  pass word  "
	if _, err := svc.Patch(context.Background(), Patch{JWPassword: ptr(password)}); err != nil {
		t.Fatalf("带空格的密码应能保存，实际 err=%v", err)
	}

	envelope := findChange(t, store.batches[0], model.SettingJWPassword).Value
	plaintext, err := serviceTestKey().Open(settingAAD(model.SettingJWPassword), envelope)
	if err != nil {
		t.Fatalf("应能解封，实际 err=%v", err)
	}
	if string(plaintext) != password {
		t.Fatalf("秘密明文不应被 trim：got %q want %q", plaintext, password)
	}
}

// TestServicePatchClearsWithDelete 覆盖清除语义（Requirement 2.3 / 2.4）：非秘密文本
// 的空串与全空白、秘密的空串都翻成 Delete，回读后对应项为空串/false，行被摘掉。
func TestServicePatchClearsWithDelete(t *testing.T) {
	key := serviceTestKey()
	svc, store, _ := newTestService(
		model.SettingRow{Key: model.SettingJWUsername, Value: "old", UpdatedAt: time.Unix(1, 0).UTC()},
		model.SettingRow{Key: model.SettingOCRBaseURL, Value: "http://old", UpdatedAt: time.Unix(1, 0).UTC()},
		sealedRow(t, key, model.SettingJWPassword, "old-password"),
	)

	snapshot, err := svc.Patch(context.Background(), Patch{
		JWUsername: ptr("   "),
		OCRBaseURL: ptr(""),
		JWPassword: ptr(""),
	})
	if err != nil {
		t.Fatalf("清除应成功，实际 err=%v", err)
	}

	if store.applyCalls != 1 {
		t.Fatalf("一次清除应只调一次 ApplySettings，实际 %d 次", store.applyCalls)
	}
	if got := changeKeys(store.batches[0]); len(got) != 3 {
		t.Fatalf("应有三条删除变更，实际 %v", got)
	}
	for _, key := range []string{model.SettingJWUsername, model.SettingOCRBaseURL, model.SettingJWPassword} {
		change := findChange(t, store.batches[0], key)
		if !change.Delete {
			t.Fatalf("%s 的清除应翻成 Delete，实际 %+v", key, change)
		}
		if change.Value != "" {
			t.Fatalf("%s 的 Delete 变更不应带值，实际 %q", key, change.Value)
		}
	}

	if snapshot.JWUsername != "" || snapshot.OCRBaseURL != "" {
		t.Fatalf("清除后非秘密项应回显空串，实际 %+v", snapshot)
	}
	if snapshot.JWPasswordSet {
		t.Fatal("清除后 jw_password 应回显未设置")
	}
}

// TestServicePatchOnlyClearSecretDoesNotTakeKey 覆盖 Requirement 3.6：密钥不可用时，
// 只把秘密设为空串（清除）的保存照常成功，且根本不调用 Current()。
func TestServicePatchOnlyClearSecretDoesNotTakeKey(t *testing.T) {
	key := serviceTestKey()
	store := &fakeStore{rows: []model.SettingRow{
		sealedRow(t, key, model.SettingJWPassword, "old-password"),
		sealedRow(t, key, model.SettingFeishuSignSecret, "old-sign"),
	}}
	// 密钥不可用：只要这里被调用，请求就会失败。
	sealer := &fakeSealer{err: model.ErrKeyUnavailable}
	svc := New(store, sealer, nil)

	snapshot, err := svc.Patch(context.Background(), Patch{
		JWPassword:       ptr(""),
		FeishuSignSecret: ptr(""),
		JWUsername:       ptr("teacher01"),
	})
	if err != nil {
		t.Fatalf("只清除秘密时不应要求密钥可用，实际 err=%v", err)
	}
	if sealer.calls != 0 {
		t.Fatalf("只清除秘密时不应调用 Current()，实际 %d 次", sealer.calls)
	}
	if store.applyCalls != 1 {
		t.Fatalf("应照常写入，实际 ApplySettings %d 次", store.applyCalls)
	}

	if snapshot.JWPasswordSet || snapshot.FeishuSignSecretSet {
		t.Fatalf("清除后两个秘密都应回显未设置，实际 %+v", snapshot)
	}
	if snapshot.JWUsername != "teacher01" {
		t.Fatalf("同批的非秘密项应写入，实际 %q", snapshot.JWUsername)
	}
}

// TestServicePatchKeyUnavailableWritesNothing 覆盖 CP3 / Requirement 3.3：
// 请求里含非空秘密字段而密钥取不到时，整请求失败、ApplySettings 调用次数为 0。
// 两条分支：Current() 报错，以及 Sealer 为 nil（装配缺失，按同一口径处理）。
func TestServicePatchKeyUnavailableWritesNothing(t *testing.T) {
	patch := Patch{
		JWUsername: ptr("teacher01"),
		JWPassword: ptr("plain-password"),
	}

	t.Run("Current 报错", func(t *testing.T) {
		store := &fakeStore{}
		svc := New(store, &fakeSealer{err: model.ErrKeyUnavailable}, nil)

		_, err := svc.Patch(context.Background(), patch)
		if !errors.Is(err, model.ErrKeyUnavailable) {
			t.Fatalf("应能 errors.Is 命中 model.ErrKeyUnavailable，实际 err=%v", err)
		}
		if store.applyCalls != 0 {
			t.Fatalf("密钥不可用时必须零写入，实际 ApplySettings %d 次", store.applyCalls)
		}
		if store.loadCalls != 0 {
			t.Fatalf("失败时不应回读，实际 LoadSettings %d 次", store.loadCalls)
		}
	})

	t.Run("Sealer 为 nil", func(t *testing.T) {
		store := &fakeStore{}
		svc := New(store, nil, nil)

		_, err := svc.Patch(context.Background(), patch)
		if !errors.Is(err, model.ErrKeyUnavailable) {
			t.Fatalf("Sealer 为 nil 应按密钥不可用处理，实际 err=%v", err)
		}
		if store.applyCalls != 0 {
			t.Fatalf("装配缺失时必须零写入，实际 ApplySettings %d 次", store.applyCalls)
		}
	})
}

// TestServicePatchValidateFailureWritesNothing 覆盖 Requirement 6.3 / CP5：
// 校验失败时既不写库也不取密钥——非法请求不该碰密钥文件。
func TestServicePatchValidateFailureWritesNothing(t *testing.T) {
	store := &fakeStore{}
	sealer := &fakeSealer{key: serviceTestKey()}
	svc := New(store, sealer, nil)

	_, err := svc.Patch(context.Background(), Patch{
		JWPassword: ptr("plain-password"),
		CronExpr:   ptr("0 10 4 * * 6"), // 6 段，非法
	})
	if err == nil {
		t.Fatal("非法 cron_expr 应被拒")
	}
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) {
		t.Fatalf("应能用 errors.As 取出 *FieldError，实际类型 %T", err)
	}
	if fieldErr.Field != model.SettingCronExpr {
		t.Fatalf("FieldError.Field 不符：got %q want %q", fieldErr.Field, model.SettingCronExpr)
	}
	if store.applyCalls != 0 || store.loadCalls != 0 {
		t.Fatalf("校验失败应零写入零回读，实际 apply=%d load=%d", store.applyCalls, store.loadCalls)
	}
	if sealer.calls != 0 {
		t.Fatalf("校验失败不应取密钥，实际 Current() %d 次", sealer.calls)
	}
}

// TestServicePatchApplyFailurePropagates 覆盖 Requirement 6.4：仓储写入失败原样上抛，
// 服务层不改写错误类别也不回读（库里可能已经被回滚，回读到的不是本次结果）。
func TestServicePatchApplyFailurePropagates(t *testing.T) {
	store := &fakeStore{applyErr: fmt.Errorf("%w: 注入的写入失败", model.ErrSettingStoreFailed)}
	svc := New(store, &fakeSealer{key: serviceTestKey()}, nil)

	_, err := svc.Patch(context.Background(), Patch{JWUsername: ptr("teacher01")})
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应能 errors.Is 命中 model.ErrSettingStoreFailed，实际 err=%v", err)
	}
	if store.applyCalls != 1 {
		t.Fatalf("应调用一次 ApplySettings，实际 %d 次", store.applyCalls)
	}
	if store.loadCalls != 0 {
		t.Fatalf("写入失败后不应回读，实际 %d 次", store.loadCalls)
	}
}

// TestServicePatchEmptyKeepsEverything 覆盖 Requirement 2.2：省略全部字段时既不改值
// 也不写库，只回读一次当前快照。
func TestServicePatchEmptyKeepsEverything(t *testing.T) {
	svc, store, _ := newTestService(
		model.SettingRow{Key: model.SettingJWUsername, Value: "teacher01"},
		model.SettingRow{Key: model.SettingCronExpr, Value: "*/5 * * * *"},
	)

	snapshot, err := svc.Patch(context.Background(), Patch{})
	if err != nil {
		t.Fatalf("空 patch 应成功，实际 err=%v", err)
	}
	if store.applyCalls != 0 {
		t.Fatalf("没有变更时不应调用 ApplySettings，实际 %d 次", store.applyCalls)
	}
	if store.loadCalls != 1 {
		t.Fatalf("应回读一次，实际 %d 次", store.loadCalls)
	}
	if snapshot.JWUsername != "teacher01" || snapshot.CronExpr != "*/5 * * * *" {
		t.Fatalf("空 patch 不应改动任何值，实际 %+v", snapshot)
	}
}

// TestServicePatchWhitelistKeepsSubmittedOrder 覆盖 D11 / Requirement 4.6：
// 白名单按提交顺序落库，每条的两个字段 trim 后存，不排序、不去重后重排。
func TestServicePatchWhitelistKeepsSubmittedOrder(t *testing.T) {
	store := &fakeStore{}
	svc := New(store, &fakeSealer{key: serviceTestKey()}, nil)

	snapshot, err := svc.Patch(context.Background(), Patch{
		Whitelist: buildingListPtr([]model.Building{
			{ID: " Q03 ", Name: " 老文史楼 "},
			{ID: "Q10", Name: "综合教学楼"},
		}),
	})
	if err != nil {
		t.Fatalf("合法白名单应能保存，实际 err=%v", err)
	}

	want := `[{"jxlbh":"Q03","name":"老文史楼"},{"jxlbh":"Q10","name":"综合教学楼"}]`
	if got := findChange(t, store.batches[0], model.SettingBuildingWhitelist).Value; got != want {
		t.Fatalf("白名单落库应按提交顺序且 trim：got %q want %q", got, want)
	}
	if len(snapshot.Whitelist) != 2 || snapshot.Whitelist[0].ID != "Q03" || snapshot.Whitelist[1].ID != "Q10" {
		t.Fatalf("回读顺序不符：%+v", snapshot.Whitelist)
	}
}

// TestServicePatchWhitelistEmptyArrayStoredAsEmpty 覆盖 Requirement 4.5：
// 白名单空数组是「清空」而不是「删除该键」，落库值是 []。
func TestServicePatchWhitelistEmptyArrayStoredAsEmpty(t *testing.T) {
	svc, store, _ := newTestService(model.SettingRow{
		Key:   model.SettingBuildingWhitelist,
		Value: `[{"jxlbh":"Q10","name":"综合教学楼"}]`,
	})

	snapshot, err := svc.Patch(context.Background(), Patch{
		Whitelist: buildingListPtr([]model.Building{}),
	})
	if err != nil {
		t.Fatalf("空白名单应能保存，实际 err=%v", err)
	}

	change := findChange(t, store.batches[0], model.SettingBuildingWhitelist)
	if change.Delete {
		t.Fatal("空白名单应存成 [] 而不是删除该键")
	}
	if change.Value != "[]" {
		t.Fatalf("空白名单落库值应为 []，实际 %q", change.Value)
	}
	if len(snapshot.Whitelist) != 0 {
		t.Fatalf("回读应为空白名单，实际 %+v", snapshot.Whitelist)
	}
}

// TestServicePatchUsesClock 钉住「时间一律取自 s.clock()」：clock 为 nil 时用 time.Now，
// 且同一批只有一条 now 记录（每条变更共用同一个时刻）。
func TestServicePatchUsesClock(t *testing.T) {
	store := &fakeStore{}
	svc := New(store, &fakeSealer{key: serviceTestKey()}, nil)

	before := time.Now()
	if _, err := svc.Patch(context.Background(), Patch{JWUsername: ptr("teacher01")}); err != nil {
		t.Fatalf("保存应成功，实际 err=%v", err)
	}
	after := time.Now()

	if len(store.nows) != 1 {
		t.Fatalf("一批写入应只有一个时刻，实际 %v", store.nows)
	}
	if store.nows[0].Before(before) || store.nows[0].After(after) {
		t.Fatalf("未注入 clock 时应取当前时刻，got %v（区间 %v ~ %v）", store.nows[0], before, after)
	}
}

// TestServiceReveal 覆盖唯一明文出口的四类失败与成功路径（Requirement 3.5、CP2）：
// 非秘密键、未设置、换键名（AAD 不符）、密文被篡改、密钥不可用都返回错误而不返回空值。
func TestServiceReveal(t *testing.T) {
	key := serviceTestKey()

	t.Run("取回明文", func(t *testing.T) {
		svc, _, sealer := newTestService(sealedRow(t, key, model.SettingJWPassword, "plain-password"))

		plaintext, err := svc.Reveal(context.Background(), model.SettingJWPassword)
		if err != nil {
			t.Fatalf("Reveal 应成功，实际 err=%v", err)
		}
		if plaintext != "plain-password" {
			t.Fatalf("明文不符：got %q", plaintext)
		}
		if sealer.calls != 1 {
			t.Fatalf("应取一次密钥，实际 %d 次", sealer.calls)
		}
	})

	t.Run("非秘密键报错", func(t *testing.T) {
		svc, _, sealer := newTestService(model.SettingRow{Key: model.SettingCronExpr, Value: "10 4 * * 6"})

		if _, err := svc.Reveal(context.Background(), model.SettingCronExpr); err == nil {
			t.Fatal("非秘密项不应能解封")
		}
		if sealer.calls != 0 {
			t.Fatalf("非秘密项不应取密钥，实际 %d 次", sealer.calls)
		}
	})

	t.Run("未设置报错", func(t *testing.T) {
		svc, _, _ := newTestService()

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); err == nil {
			t.Fatal("未设置的秘密项应报错，而不是返回空串")
		}
	})

	t.Run("换键名解不开", func(t *testing.T) {
		// 把 jw_password 的明文按 feishu_webhook 的 AAD 加封，再冒充 jw_password 的行。
		envelope, err := key.Seal(settingAAD(model.SettingFeishuWebhook), []byte("plain-password"))
		if err != nil {
			t.Fatalf("用例前提不成立：%v", err)
		}
		svc, _, _ := newTestService(model.SettingRow{Key: model.SettingJWPassword, Value: envelope, Secret: true})

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); !errors.Is(err, secret.ErrEnvelopeInvalid) {
			t.Fatalf("AAD 不符应返回 ErrEnvelopeInvalid，实际 err=%v", err)
		}
	})

	t.Run("密文被篡改解不开", func(t *testing.T) {
		row := sealedRow(t, key, model.SettingJWPassword, "plain-password")
		row.Value = tamperedEnvelope(t, row.Value)
		svc, _, _ := newTestService(row)

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); !errors.Is(err, secret.ErrEnvelopeInvalid) {
			t.Fatalf("篡改应返回 ErrEnvelopeInvalid，实际 err=%v", err)
		}
	})

	t.Run("密钥不可用报错", func(t *testing.T) {
		store := &fakeStore{rows: []model.SettingRow{sealedRow(t, key, model.SettingJWPassword, "plain-password")}}
		svc := New(store, &fakeSealer{err: model.ErrKeyUnavailable}, nil)

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); !errors.Is(err, model.ErrKeyUnavailable) {
			t.Fatalf("应返回 model.ErrKeyUnavailable，实际 err=%v", err)
		}
	})

	t.Run("Sealer 为 nil 报密钥不可用", func(t *testing.T) {
		store := &fakeStore{rows: []model.SettingRow{sealedRow(t, key, model.SettingJWPassword, "plain-password")}}
		svc := New(store, nil, nil)

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); !errors.Is(err, model.ErrKeyUnavailable) {
			t.Fatalf("Sealer 为 nil 应返回 model.ErrKeyUnavailable，实际 err=%v", err)
		}
	})

	t.Run("读失败原样上抛", func(t *testing.T) {
		store := &fakeStore{loadErr: fmt.Errorf("%w: 注入的读取失败", model.ErrSettingStoreFailed)}
		svc := New(store, &fakeSealer{key: key}, nil)

		if _, err := svc.Reveal(context.Background(), model.SettingJWPassword); !errors.Is(err, model.ErrSettingStoreFailed) {
			t.Fatalf("应能 errors.Is 命中 model.ErrSettingStoreFailed，实际 err=%v", err)
		}
	})
}

// TestServiceWhitelistAndCronExpr 覆盖 runner 与 scheduler 的两个读取入口：
// 白名单缺行回空数组；cron 缺行回默认值，行存在时原样返回（非法值留给调度器回落）。
func TestServiceWhitelistAndCronExpr(t *testing.T) {
	t.Run("白名单缺行回空数组", func(t *testing.T) {
		svc, _, _ := newTestService()

		list, err := svc.Whitelist(context.Background())
		if err != nil {
			t.Fatalf("Whitelist 应成功，实际 err=%v", err)
		}
		if list == nil || len(list) != 0 {
			t.Fatalf("缺行应回非 nil 空切片，实际 %#v", list)
		}
	})

	t.Run("白名单按库里的顺序返回", func(t *testing.T) {
		svc, _, _ := newTestService(model.SettingRow{
			Key:   model.SettingBuildingWhitelist,
			Value: `[{"jxlbh":"Q03","name":"老文史楼"},{"jxlbh":"Q10","name":"综合教学楼"}]`,
		})

		list, err := svc.Whitelist(context.Background())
		if err != nil {
			t.Fatalf("Whitelist 应成功，实际 err=%v", err)
		}
		if len(list) != 2 || list[0].ID != "Q03" || list[1].ID != "Q10" {
			t.Fatalf("顺序不符：%+v", list)
		}
	})

	t.Run("cron 缺行回默认值", func(t *testing.T) {
		svc, _, _ := newTestService()

		expr, err := svc.CronExpr(context.Background())
		if err != nil {
			t.Fatalf("CronExpr 应成功，实际 err=%v", err)
		}
		if expr != model.DefaultCronExpr {
			t.Fatalf("缺行应回默认值 %q，实际 %q", model.DefaultCronExpr, expr)
		}
	})

	t.Run("cron 行存在时原样返回", func(t *testing.T) {
		svc, _, _ := newTestService(model.SettingRow{Key: model.SettingCronExpr, Value: "*/5 * * * *"})

		expr, err := svc.CronExpr(context.Background())
		if err != nil {
			t.Fatalf("CronExpr 应成功，实际 err=%v", err)
		}
		if expr != "*/5 * * * *" {
			t.Fatalf("应原样返回库里的表达式，实际 %q", expr)
		}
	})

	t.Run("cron 行存在但非法时不回落", func(t *testing.T) {
		// Requirement 9.6 的回落发生在调度器装载时；服务层照实返回，管理员才看得到
		// 库里到底存了什么。
		svc, _, _ := newTestService(model.SettingRow{Key: model.SettingCronExpr, Value: "0 10 4 * * 6"})

		expr, err := svc.CronExpr(context.Background())
		if err != nil {
			t.Fatalf("CronExpr 应成功，实际 err=%v", err)
		}
		if expr != "0 10 4 * * 6" {
			t.Fatalf("服务层不应回落非法值，实际 %q", expr)
		}
	})
}
