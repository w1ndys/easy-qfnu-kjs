package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestSecretSettingCoversSevenKeys 用七个键的完整清单钉住「三真四假」：
// 回显判定只走 SecretSetting，少判一项就会把密文当明文回给管理员，
// 多判一项则会把可回显的配置变成不可读（Requirement 3.4、CP4）。
func TestSecretSettingCoversSevenKeys(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{SettingJWUsername, false},
		{SettingJWPassword, true},
		{SettingOCRBaseURL, false},
		{SettingFeishuWebhook, true},
		{SettingFeishuSignSecret, true},
		{SettingCronExpr, false},
		{SettingBuildingWhitelist, false},
	}

	for _, c := range cases {
		if got := SecretSetting(c.key); got != c.want {
			t.Fatalf("SecretSetting(%q) 应为 %v，实际 %v", c.key, c.want, got)
		}
	}

	// 键名之外的输入一律按非秘密处理：写入侧有键白名单挡着，
	// 但判定函数本身不能因为拼错键名就退回「秘密」这种更危险的一侧。
	for _, key := range []string{"", "jw_password ", "cron_express", "building_whitelist2"} {
		if SecretSetting(key) {
			t.Fatalf("未知键 %q 不应被判为秘密项", key)
		}
	}
}

// TestSettingKeyValuesMatchStorage 钉住七个键的字面值：它们既是 settings 表的主键值，
// 也是 PUT 请求体与 GET 响应的字段名（秘密项在 GET 里以 `*_set` 出现）。任一处改动都会
// 让既有行读不到、或让前端提交的字段落进不存在的键（Requirement 1.1、2.1）。
func TestSettingKeyValuesMatchStorage(t *testing.T) {
	got := []string{
		SettingJWUsername,
		SettingJWPassword,
		SettingOCRBaseURL,
		SettingFeishuWebhook,
		SettingFeishuSignSecret,
		SettingCronExpr,
		SettingBuildingWhitelist,
	}
	want := []string{
		"jw_username",
		"jw_password",
		"ocr_base_url",
		"feishu_webhook",
		"feishu_sign_secret",
		"cron_expr",
		"building_whitelist",
	}

	if len(got) != len(want) {
		t.Fatalf("键数量应为 %d，实际 %d", len(want), len(got))
	}
	seen := make(map[string]struct{}, len(got))
	for i, key := range got {
		if key != want[i] {
			t.Fatalf("第 %d 个键应为 %q，实际 %q", i+1, want[i], key)
		}
		if _, ok := seen[key]; ok {
			t.Fatalf("键 %q 重复，七个键必须互不相同", key)
		}
		seen[key] = struct{}{}
	}
}

// TestSettingKeysReturnsOrderedCopy 钉住 SettingKeys 的两件事：顺序与副本。
//
// 顺序：返回值要和常量声明顺序一致，快照组装与 LoadSettings 的 IN 取键都按它取键（Requirement 1.1）。
//
// 副本：调用方改元素之后，下一次调用必须还是原样的七个键。若返回包级切片本身，
// 改元素就能串改所有调用方看到的键集合（Requirement 1.1）。
func TestSettingKeysReturnsOrderedCopy(t *testing.T) {
	want := []string{
		"jw_username",
		"jw_password",
		"ocr_base_url",
		"feishu_webhook",
		"feishu_sign_secret",
		"cron_expr",
		"building_whitelist",
	}

	got := SettingKeys()
	if len(got) != len(want) {
		t.Fatalf("SettingKeys 应返回 %d 个键，实际 %d 个：%v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个键应为 %q，实际 %q", i+1, want[i], got[i])
		}
	}

	// 断言元素改写：改返回值的元素不能穿透到包级集合。
	got[0] = "hacked"

	again := SettingKeys()
	if len(again) != len(want) {
		t.Fatalf("改过返回值后 SettingKeys 应仍返回 %d 个键，实际 %d 个：%v", len(want), len(again), again)
	}
	for i := range want {
		if again[i] != want[i] {
			t.Fatalf("改过返回值后第 %d 个键应为 %q，实际 %q（返回值必须是副本）", i+1, want[i], again[i])
		}
	}

	// append 的断言要挑对形态：对**完整返回值** append（len==cap）必然重新分配，
	// 改不到任何人的底层数组，拿它断言「副本」等于空转。能测出「返回的是包级切片」
	// 的是子切片形态——got[:len-1] 的 cap 仍是 7，append 会原地写回第 7 个元素；
	// 若底层数组是包级那份，下一次调用就会看见它。
	tail := SettingKeys()[:len(want)-1]
	tail = append(tail, "hacked-last")
	if len(tail) != len(want) || tail[len(want)-1] != "hacked-last" {
		t.Fatalf("子切片 append 后应有 %d 个元素且末尾为 \"hacked-last\"，实际 %v", len(want), tail)
	}
	if last := SettingKeys()[len(want)-1]; last != want[len(want)-1] {
		t.Fatalf("子切片 append 后最后一个键应为 %q，实际 %q（返回值必须是副本）", want[len(want)-1], last)
	}
}

// TestSettingLimits 钉住三个常量：默认 cron 是 5 字段（与 cronexpr 的语法一致，
// 0005 把 0001 的 6 段旧种子改写成同一串，见 Requirement 9.5），
// 条数与字节上限的校验口径见 design.md「Data Models → 设置项」与 tasklist 任务 3。
func TestSettingLimits(t *testing.T) {
	if DefaultCronExpr != "10 4 * * 6" {
		t.Fatalf("DefaultCronExpr 应为 5 字段的 \"10 4 * * 6\"，实际 %q", DefaultCronExpr)
	}
	if MaxWhitelistEntries != 200 {
		t.Fatalf("MaxWhitelistEntries 应为 200，实际 %d", MaxWhitelistEntries)
	}
	if MaxSettingValueBytes != 4096 {
		t.Fatalf("MaxSettingValueBytes 应为 4096，实际 %d", MaxSettingValueBytes)
	}
}

// settingsSentinelCases 是本 spec 追加的五个哨兵，下面的三件套用例都从这张表取：
// 五个哨兵各自映射到 40901 / 40401 / 42201 / 50000 等分支（其中 ErrRunNotFound 是
// reserved，不返回；40401 仍由 handler 在 ByID 返回 false 时映射），文案或包装关系一变，
// 上层就可能把两类失败混成同一个业务码（design.md「哨兵的包装关系」）。
var settingsSentinelCases = []struct {
	name string
	err  error
}{
	{"ErrSettingInvalid", ErrSettingInvalid},
	{"ErrSettingStoreFailed", ErrSettingStoreFailed},
	{"ErrRunBusy", ErrRunBusy},
	{"ErrRunNotFound", ErrRunNotFound},
	{"ErrCollectorUnavailable", ErrCollectorUnavailable},
}

// TestSettingSentinelsAreDistinct 保证五个哨兵各有独立文案（design.md「Error Handling」的错误码表）：
// 文案重复时日志分不清是校验失败还是读库失败，调用方也无法只凭错误判定分支。
func TestSettingSentinelsAreDistinct(t *testing.T) {
	seen := make(map[string]string, len(settingsSentinelCases))
	for _, c := range settingsSentinelCases {
		if c.err == nil {
			t.Fatalf("%s 不应为 nil", c.name)
		}
		if prev, ok := seen[c.err.Error()]; ok {
			t.Fatalf("%s 与 %s 的文案相同（%q），调用方无法区分", c.name, prev, c.err.Error())
		}
		seen[c.err.Error()] = c.name
	}
}

// TestSettingSentinelsSurviveWrapping 只测通用的 `%w` 包装链：仓储用 fmt.Errorf("%w") 补上下文后，
// 两层包装仍要能被 errors.Is 命中原始哨兵。具体的 FieldError（任务 3）、
// SettingsRepo（任务 4）、BusyError（任务 7）包装留给各自的用例，这里不预设它们的类型。
func TestSettingSentinelsSurviveWrapping(t *testing.T) {
	for _, c := range settingsSentinelCases {
		// 两层包装，里外都用通用 %w：不引入任何具体错误类型，只测包装链本身。
		wrapped := fmt.Errorf("保存设置: %w", fmt.Errorf("写库: %w", c.err))
		if !errors.Is(wrapped, c.err) {
			t.Fatalf("%q 经两层 %%w 包装后应仍能被 errors.Is 命中", c.err)
		}
	}
}

// TestSettingSentinelsDoNotCrossMatch 反向也要成立：不同哨兵之间不能互相误判，
// 否则「设置值非法」会顶替「设置读写失败」返回 50000，或者 busy 被当成 not found 返回 404。
func TestSettingSentinelsDoNotCrossMatch(t *testing.T) {
	for i, a := range settingsSentinelCases {
		for j, b := range settingsSentinelCases {
			if i == j {
				continue
			}
			if errors.Is(a.err, b.err) {
				t.Fatalf("%s 不应命中 %s", a.name, b.name)
			}
		}
	}
}

// TestBuildingJSONShapeMatchesMetaStorage 锁住写入形状等于 meta 的读取形状：
// store 侧按 `{"jxlbh":…,"name":…}` 解析白名单，这里把序列化结果逐字节钉死，
// 任一侧改键名都会让本用例失败（CP7、D11）。
func TestBuildingJSONShapeMatchesMetaStorage(t *testing.T) {
	list := []Building{{ID: "Q10", Name: "综合教学楼"}}

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("序列化白名单失败: %v", err)
	}

	// 这段字面量与 store/meta.go 的解析形状一致；0001 种下的是同形状的空数组 []。
	const want = `[{"jxlbh":"Q10","name":"综合教学楼"}]`
	if string(raw) != want {
		t.Fatalf("白名单存储形状应为 %s，实际 %s", want, raw)
	}

	// 反向：已存在于库里的同形状 JSON 必须能读回 Building，且顺序不变。
	var back []Building
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatalf("反序列化白名单失败: %v", err)
	}
	if len(back) != 1 || back[0] != list[0] {
		t.Fatalf("往返后应为 %+v，实际 %+v", list, back)
	}
}

// TestBuildingEmptyListMarshalsToArray 锁住空白名单落成 `[]` 而不是 `null`：
// 0001 种的就是 `[]`，而 `null` 会让「已清空」与「从未写入」在库里看起来一样。
//
// 注意这里序列化的是**非 nil** 空切片 `[]Building{}`。nil 切片编出来是 `null`，
// 见 TestBuildingNilSliceMarshalsToNull——调用方清空白名单时必须构造非 nil 切片，
// 才能得到 `[]`。
func TestBuildingEmptyListMarshalsToArray(t *testing.T) {
	raw, err := json.Marshal([]Building{})
	if err != nil {
		t.Fatalf("序列化空白名单失败: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("空白名单应存成 []，实际 %s", raw)
	}
}

// TestBuildingNilSliceMarshalsToNull 钉住 nil 切片与空切片在 JSON 上的差别：
// `var b []Building` 编出来是 `null`，而 `[]Building{}` 才是 `[]`。这条差别不是理论问题：
// meta 侧按 JSON 数组解析，`null` 解不出数组元素，只能回落成空列表，
// 于是「管理员清了空」和「库里是脏数据」在库里长得一模一样；
// 而 Requirement 1.3 要求未写入时响应里就是空数组，`null` 也答不出那个形状。
func TestBuildingNilSliceMarshalsToNull(t *testing.T) {
	var list []Building // nil 切片，故意不初始化

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("序列化 nil 白名单失败: %v", err)
	}
	if string(raw) != "null" {
		t.Fatalf("nil 白名单应序列化成 null（这也是调用方必须避开它的原因），实际 %s", raw)
	}

	// 反过来：`null` 读回来仍是 nil，不会自动变成空切片——所以「先 marshal nil 再 unmarshal」
	// 这种写法救不回来，只能在构造时就写 `[]Building{}`。
	var back []Building
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("反序列化 null 白名单失败: %v", err)
	}
	if back != nil {
		t.Fatalf("null 反序列化后应为 nil，实际 %+v", back)
	}
}

// TestBusinessLocationIsFixedPlusEight 钉住业务时区的口径：+08:00 固定偏移，
// 与 pipeline/query/publish 既有实现一致，且不随宿主机时区变化。
func TestBusinessLocationIsFixedPlusEight(t *testing.T) {
	loc := BusinessLocation()

	if loc == time.Local {
		t.Fatal("业务时区不应等于宿主机本地时区，否则容器 TZ 一改就偏移")
	}

	// 冬季与夏季取同一个偏移：2026 年之后中国无夏令时，两个季节都得是 +08:00。
	for _, date := range []time.Time{
		time.Date(2026, 1, 14, 4, 10, 0, 0, time.UTC),
		time.Date(2026, 7, 14, 4, 10, 0, 0, time.UTC),
	} {
		_, offset := date.In(loc).Zone()
		if offset != 8*60*60 {
			t.Fatalf("%s 在业务时区的偏移应为 8h，实际 %ds", date.Format(time.DateOnly), offset)
		}
	}

	// 具体换算：周六 04:10（+08:00）就是前一天 20:10 UTC，这正是 cron_next_at 的写法。
	utc := time.Date(2026, 11, 13, 20, 10, 0, 0, time.UTC)
	business := utc.In(loc)
	if business.Hour() != 4 || business.Day() != 14 || business.Weekday() != time.Saturday {
		t.Fatalf("2026-11-13T20:10:00Z 在业务时区应为 11-14 周六 04:10，实际 %s", business.Format(time.RFC3339))
	}
}

// TestTriggerSourceValues 钉住两个来源的字面值：它们只出现在日志字段 trigger=，
// 值一变，既有日志检索口径就失配（D7、Requirement 9.8）。
func TestTriggerSourceValues(t *testing.T) {
	if TriggerManual != "manual" {
		t.Fatalf("TriggerManual 应为 \"manual\"，实际 %q", TriggerManual)
	}
	if TriggerCron != "cron" {
		t.Fatalf("TriggerCron 应为 \"cron\"，实际 %q", TriggerCron)
	}
}
