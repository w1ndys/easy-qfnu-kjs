package settings

// 本文件覆盖 Validate 的纯校验：白名单、cron、URL 形状、五项文本项与白名单两个字段
// 的 4096 字节上界，以及 Patch 指针三态（省略 / 空串 / 值）。所有用例都不碰密钥与数据库，
// 因此不需要替身；每条拒绝用例都断言 *FieldError.Field，确保 42201 能点名到具体设置项。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 合法取值只写一次，用例里复用；它们同时是「值」这一态的样本。
const (
	validOCRURL     = "https://ocr.example.com/api/v1/recognize"
	validWebhookURL = "https://open.feishu.cn/open-apis/bot/v2/hook/0f0f0f0f"
)

func ptr(value string) *string { return &value }

func buildingListPtr(list []model.Building) *[]model.Building { return &list }

// validWhitelist 是两条合法白名单：编号不重复、展示名非空。
func validWhitelist() []model.Building {
	return []model.Building{
		{ID: "Q10", Name: "综合教学楼"},
		{ID: "Q11", Name: "实验楼"},
	}
}

// whitelistOf 生成 count 条互不重复的合法白名单，用于条数边界。
func whitelistOf(count int) []model.Building {
	list := make([]model.Building, 0, count)
	for i := range count {
		list = append(list, model.Building{
			ID:   fmt.Sprintf("B%03d", i+1),
			Name: fmt.Sprintf("%d 号楼", i+1),
		})
	}
	return list
}

// httpURLOfBytes 返回恰好 n 字节、形状合法的 http(s) 绝对地址，用于量 4096 的边界。
func httpURLOfBytes(t *testing.T, n int) string {
	t.Helper()
	const prefix = "https://ocr.example.com/"
	if n < len(prefix) {
		t.Fatalf("用例前提不成立：n=%d 小于 URL 前缀长度 %d", n, len(prefix))
	}
	return prefix + strings.Repeat("a", n-len(prefix))
}

// validateCase 是一条 Validate 用例。wantField 为空表示期望通过；非空时断言
// FieldError.Field 与该字段名，并按需对文案做包含/不包含断言（空串表示不检查）。
// wantContainsAll 用于一条文案里必须同时出现的多个片段：wantContains 只能钉一段
// 连续文字，而「第 2 条的楼编号……超过上限 4096 字节」这类文案的几个要素是分开的。
type validateCase struct {
	name            string
	patch           Patch
	wantField       string
	wantContains    string
	wantContainsAll []string
	wantNotContains string
}

// runValidateCases 执行表驱动用例，统一断言「通过」或「点名字段的 FieldError」。
func runValidateCases(t *testing.T, cases []validateCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.patch)
			if tc.wantField == "" {
				assertValidateOK(t, err)
				return
			}

			assertFieldError(t, err, tc.wantField)
			if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("错误文案应包含 %q：%q", tc.wantContains, err.Error())
			}
			for _, want := range tc.wantContainsAll {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误文案应同时包含 %q：%q", want, err.Error())
				}
			}
			if tc.wantNotContains != "" && strings.Contains(err.Error(), tc.wantNotContains) {
				t.Fatalf("错误文案不应包含 %q：%q", tc.wantNotContains, err.Error())
			}
		})
	}
}

// assertValidateOK 断言没有任何校验错误。
func assertValidateOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("应通过校验，实际 err=%v", err)
	}
}

// assertFieldError 断言 err 是包装了 model.ErrSettingInvalid 的 *FieldError：
// Field 等于期望字段名，Message 非空，Error() 文案点名字段（Requirement 6.3）。
func assertFieldError(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %s 的校验错误，实际通过", wantField)
	}
	if !errors.Is(err, model.ErrSettingInvalid) {
		t.Fatalf("应能用 errors.Is 命中 model.ErrSettingInvalid，实际 err=%v", err)
	}

	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) {
		t.Fatalf("应能用 errors.As 取出 *FieldError，实际类型 %T", err)
	}
	if fieldErr.Field != wantField {
		t.Fatalf("FieldError.Field 不符：got %q want %q", fieldErr.Field, wantField)
	}
	if fieldErr.Message == "" {
		t.Fatalf("FieldError.Message 不应为空")
	}
	if !strings.Contains(fieldErr.Error(), wantField) {
		t.Fatalf("错误文案应点名字段 %q：%q", wantField, fieldErr.Error())
	}
}

// TestFieldErrorWrapsSettingInvalid 钉住 FieldError 的包装关系：Unwrap 恒返回
// model.ErrSettingInvalid，errors.Is 判类别、errors.As 取字段名，Error() 是
// 「字段名：原因」（design「哨兵的包装关系」）。
func TestFieldErrorWrapsSettingInvalid(t *testing.T) {
	original := &FieldError{Field: model.SettingCronExpr, Message: "测试原因"}

	var err error = original
	if !errors.Is(err, model.ErrSettingInvalid) {
		t.Fatalf("FieldError 应包装 model.ErrSettingInvalid，实际 err=%v", err)
	}
	if !errors.Is(original.Unwrap(), model.ErrSettingInvalid) {
		t.Fatalf("FieldError.Unwrap 应恒返回 model.ErrSettingInvalid，实际 %v", original.Unwrap())
	}

	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) {
		t.Fatalf("应能用 errors.As 取出 *FieldError，实际类型 %T", err)
	}
	if fieldErr != original {
		t.Fatalf("errors.As 应取回同一个 FieldError，got %p want %p", fieldErr, original)
	}
	if got, want := fieldErr.Error(), "cron_expr：测试原因"; got != want {
		t.Fatalf("Error() 文案不符：got %q want %q", got, want)
	}
}

// TestValidatePatchPointerTriState 覆盖 Patch 七个字段的指针三态：
// 省略（nil）保持原值、空串清除（cron_expr 例外，空串非法）、给值替换。
// 每一项各一行，拒绝用例断言 FieldError.Field（Requirement 2.1–2.6、CP6）。
func TestValidatePatchPointerTriState(t *testing.T) {
	cases := []validateCase{
		// 省略：全部 nil，不校验任何一项。
		{name: "三态/全部省略", patch: Patch{}},

		// 空串：五项文本项与白名单都是「清除」；cron_expr 的空串非法。
		{name: "三态/jw_username 空串（清除）", patch: Patch{JWUsername: ptr("")}},
		{name: "三态/jw_password 空串（清除）", patch: Patch{JWPassword: ptr("")}},
		{name: "三态/ocr_base_url 空串（清除）", patch: Patch{OCRBaseURL: ptr("")}},
		{name: "三态/feishu_webhook 空串（清除）", patch: Patch{FeishuWebhook: ptr("")}},
		{name: "三态/feishu_sign_secret 空串（清除）", patch: Patch{FeishuSignSecret: ptr("")}},
		{name: "三态/cron_expr 空串（非法）", patch: Patch{CronExpr: ptr("")},
			wantField: model.SettingCronExpr, wantContains: "表达式为空"},
		{name: "三态/building_whitelist 空数组（清空）", patch: Patch{Whitelist: buildingListPtr([]model.Building{})}},

		// 值：每项的合法取值都能通过。
		{name: "三态/jw_username 值", patch: Patch{JWUsername: ptr("admin")}},
		{name: "三态/jw_password 值", patch: Patch{JWPassword: ptr("Pa55 word")}},
		{name: "三态/ocr_base_url 值", patch: Patch{OCRBaseURL: ptr(validOCRURL)}},
		{name: "三态/feishu_webhook 值", patch: Patch{FeishuWebhook: ptr(validWebhookURL)}},
		{name: "三态/feishu_sign_secret 值", patch: Patch{FeishuSignSecret: ptr("sign-secret")}},
		{name: "三态/cron_expr 值", patch: Patch{CronExpr: ptr(model.DefaultCronExpr)}},
		{name: "三态/building_whitelist 值", patch: Patch{Whitelist: buildingListPtr(validWhitelist())}},
	}

	runValidateCases(t, cases)
}

// TestValidateWhitelist 覆盖 Requirement 4 的全部拒绝口径与接受口径：
// 条数上限、编号 trim 后非空且唯一、展示名 trim 后非空；空数组合法。
func TestValidateWhitelist(t *testing.T) {
	cases := []validateCase{
		{name: "空数组合法", patch: Patch{Whitelist: buildingListPtr([]model.Building{})}},
		{name: "单条合法", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: "Q10", Name: "综合楼"}})}},
		{name: "两条合法", patch: Patch{Whitelist: buildingListPtr(validWhitelist())}},
		{name: "条数恰好 200 合法", patch: Patch{Whitelist: buildingListPtr(whitelistOf(model.MaxWhitelistEntries))}},
		{name: "条数 201 拒绝", patch: Patch{Whitelist: buildingListPtr(whitelistOf(model.MaxWhitelistEntries + 1))},
			wantField: model.SettingBuildingWhitelist, wantContains: "200"},

		{name: "编号为空拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: "", Name: "综合楼"}})},
			wantField: model.SettingBuildingWhitelist, wantContains: "第 1 条"},
		{name: "编号只有空白拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: " \t ", Name: "综合楼"}})},
			wantField: model.SettingBuildingWhitelist, wantContains: "第 1 条"},
		{name: "编号 trim 后重复拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: "  Q10  ", Name: "实验楼"},
		})}, wantField: model.SettingBuildingWhitelist, wantContains: "Q10"},
		{name: "编号大小写不同不算重复", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: "q10", Name: "实验楼"},
		})}},
		{name: "编号带空白但非空通过", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: " Q10 ", Name: "综合楼"}})}},

		{name: "展示名为空拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: "Q10", Name: ""}})},
			wantField: model.SettingBuildingWhitelist, wantContains: "第 1 条"},
		{name: "展示名只有空白拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: "Q10", Name: "  \t "}})},
			wantField: model.SettingBuildingWhitelist, wantContains: "第 1 条"},
		{name: "展示名带空白但非空通过", patch: Patch{Whitelist: buildingListPtr([]model.Building{{ID: "Q10", Name: " 综合楼 "}})}},
		{name: "第二条重复也拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: "Q11", Name: "实验楼"},
			{ID: "Q11", Name: "实验楼二"},
		})}, wantField: model.SettingBuildingWhitelist, wantContains: "第 3 条"},
	}

	runValidateCases(t, cases)
}

// TestValidateCronExpr 覆盖 Requirement 5 的语法口径：5 字段、取值范围、步进、
// 列表；首尾空白由 Validate 先 trim，trim 后的表达式交给 cronexpr.Parse，非法写法
// 由 Parse 拒绝，Validate 只透传原因并点名字段。同时钉住「只判语法、不量字节、
// 不判可达性」：超长合法表达式与 `0 0 30 2 *` 都通过。
func TestValidateCronExpr(t *testing.T) {
	// 4107 字节、语法合法的表达式：分钟字段是 strings.Repeat("0,", 2049) 拼上末尾
	// 一个 `0`，共 2050 个 `0` 的列表。它超过 4096 仍须通过，因为 4096 上界只管
	// 五项文本项与白名单的两个字段（design「设置项」）。
	longValidCron := strings.Repeat("0,", 2049) + "0 * * * *"
	if len(longValidCron) <= model.MaxSettingValueBytes {
		t.Fatalf("用例前提不成立：表达式只有 %d 字节，未超过上界 %d", len(longValidCron), model.MaxSettingValueBytes)
	}

	// 恰好 4097 字节的合法表达式（分钟字段 2045 个 `0` 的列表）：越过上界一格，
	// 用来钉住 cron_expr 根本不量字节——拒绝与否只看 cronexpr.Parse。
	cronAt4097Bytes := strings.Repeat("0,", 2044) + "0 * * * *"
	if len(cronAt4097Bytes) != model.MaxSettingValueBytes+1 {
		t.Fatalf("用例前提不成立：表达式应为 %d 字节，实际 %d 字节",
			model.MaxSettingValueBytes+1, len(cronAt4097Bytes))
	}

	cases := []validateCase{
		{name: "默认表达式合法", patch: Patch{CronExpr: ptr(model.DefaultCronExpr)}},
		{name: "全星号合法", patch: Patch{CronExpr: ptr("* * * * *")}},
		{name: "区间与步进合法", patch: Patch{CronExpr: ptr("30 8-18/2 * * 1-5")}},
		{name: "列表合法", patch: Patch{CronExpr: ptr("0,30 * * * *")}},
		{name: "周 7 归一为周日", patch: Patch{CronExpr: ptr("10 4 * * 7")}},
		{name: "语法合法但不可达仍通过", patch: Patch{CronExpr: ptr("0 0 30 2 *")}},
		{name: "超过 4096 字节仍按语法判定通过", patch: Patch{CronExpr: ptr(longValidCron)}},
		{name: "恰好 4097 字节仍合法", patch: Patch{CronExpr: ptr(cronAt4097Bytes)}},

		{name: "空串非法", patch: Patch{CronExpr: ptr("")},
			wantField: model.SettingCronExpr, wantContains: "表达式为空"},
		{name: "全空白非法（trim 后为空）", patch: Patch{CronExpr: ptr("   ")},
			wantField: model.SettingCronExpr, wantContains: "表达式为空"},
		{name: "前后空白通过（trim 后解析）", patch: Patch{CronExpr: ptr("  " + model.DefaultCronExpr + "\t\n")}},
		{name: "六字段非法", patch: Patch{CronExpr: ptr("0 10 4 * * 6")},
			wantField: model.SettingCronExpr, wantContains: "5 个字段"},
		{name: "四字段非法", patch: Patch{CronExpr: ptr("10 4 * *")},
			wantField: model.SettingCronExpr, wantContains: "5 个字段"},
		{name: "分钟越界非法", patch: Patch{CronExpr: ptr("60 * * * *")},
			wantField: model.SettingCronExpr, wantContains: "第 1 个字段"},
		{name: "日越界非法", patch: Patch{CronExpr: ptr("0 0 32 * *")},
			wantField: model.SettingCronExpr, wantContains: "第 3 个字段"},
		{name: "步长为零非法", patch: Patch{CronExpr: ptr("*/0 * * * *")},
			wantField: model.SettingCronExpr},
		{name: "倒序区间非法", patch: Patch{CronExpr: ptr("0 5-1 * * *")},
			wantField: model.SettingCronExpr},
		{name: "别名非法", patch: Patch{CronExpr: ptr("@every 1h")},
			wantField: model.SettingCronExpr},
		{name: "字母名称非法", patch: Patch{CronExpr: ptr("0 0 * * MON")},
			wantField: model.SettingCronExpr},
	}

	runValidateCases(t, cases)
}

// TestValidateURLFields 覆盖 ocr_base_url 与 feishu_webhook 的形状口径：
// 非空时必须是 http/https 绝对地址（带主机）。ocr_base_url 是非秘密项，判 trim 后的值；
// feishu_webhook 是秘密项，判提交原文。
func TestValidateURLFields(t *testing.T) {
	cases := []validateCase{
		{name: "ocr/http 带端口合法", patch: Patch{OCRBaseURL: ptr("http://ocr.internal:8080/recognize")}},
		{name: "ocr/https 合法", patch: Patch{OCRBaseURL: ptr(validOCRURL)}},
		{name: "ocr/大写 scheme 合法", patch: Patch{OCRBaseURL: ptr("HTTPS://OCR.EXAMPLE.COM/recognize")}},
		{name: "ocr/空串合法", patch: Patch{OCRBaseURL: ptr("")}},
		{name: "ocr/只有空白合法（清除）", patch: Patch{OCRBaseURL: ptr(" \t ")}},
		{name: "ocr/前后空白包着的合法 URL 通过", patch: Patch{OCRBaseURL: ptr("  " + validOCRURL + "  ")}},
		{name: "ocr/相对路径拒绝", patch: Patch{OCRBaseURL: ptr("/api/ocr")},
			wantField: model.SettingOCRBaseURL, wantContains: "绝对地址"},
		{name: "ocr/缺 scheme 拒绝", patch: Patch{OCRBaseURL: ptr("ocr.example.com/recognize")},
			wantField: model.SettingOCRBaseURL, wantContains: "绝对地址"},
		{name: "ocr/协议相对地址拒绝", patch: Patch{OCRBaseURL: ptr("//ocr.example.com/recognize")},
			wantField: model.SettingOCRBaseURL, wantContains: "绝对地址"},
		{name: "ocr/缺主机拒绝", patch: Patch{OCRBaseURL: ptr("http://")},
			wantField: model.SettingOCRBaseURL, wantContains: "绝对地址"},
		{name: "ocr/非 http(s) scheme 拒绝", patch: Patch{OCRBaseURL: ptr("ftp://ocr.example.com/")},
			wantField: model.SettingOCRBaseURL, wantContains: "绝对地址"},

		{name: "webhook/https 合法", patch: Patch{FeishuWebhook: ptr(validWebhookURL)}},
		{name: "webhook/http 合法", patch: Patch{FeishuWebhook: ptr("http://127.0.0.1:9000/hook")}},
		{name: "webhook/空串合法", patch: Patch{FeishuWebhook: ptr("")}},
		{name: "webhook/相对地址拒绝", patch: Patch{FeishuWebhook: ptr("open-apis/bot/v2/hook/x")},
			wantField: model.SettingFeishuWebhook, wantContains: "绝对地址"},
		{name: "webhook/非 http(s) scheme 拒绝", patch: Patch{FeishuWebhook: ptr("ftp://open.feishu.cn/hook")},
			wantField: model.SettingFeishuWebhook, wantContains: "绝对地址"},
		{name: "webhook/缺主机拒绝", patch: Patch{FeishuWebhook: ptr("https://")},
			wantField: model.SettingFeishuWebhook, wantContains: "绝对地址"},
		{name: "webhook/只有空白拒绝（秘密原文不 trim）", patch: Patch{FeishuWebhook: ptr("   ")},
			wantField: model.SettingFeishuWebhook, wantContains: "绝对地址"},
	}

	runValidateCases(t, cases)
}

// TestValidateTextValueByteLimit 覆盖五项文本项的 4096 字节上界：
// 恰好 4096 通过、4097 拒绝；jw_username 与 ocr_base_url 按 TrimSpace 后计；
// 4097 字节的合法 URL 形状 feishu_webhook 仍被拒，且拒绝理由不是 URL 形状。
func TestValidateTextValueByteLimit(t *testing.T) {
	const limit = model.MaxSettingValueBytes

	cases := []validateCase{
		{name: "jw_username 恰好 4096 字节通过", patch: Patch{JWUsername: ptr(strings.Repeat("u", limit))}},
		{name: "jw_username 4097 字节拒绝", patch: Patch{JWUsername: ptr(strings.Repeat("u", limit+1))},
			wantField: model.SettingJWUsername, wantContains: "4096"},
		{name: "jw_username 前后空白不计入上界", patch: Patch{JWUsername: ptr("  " + strings.Repeat("u", limit) + "  ")}},
		{name: "jw_username trim 后 4097 字节拒绝", patch: Patch{JWUsername: ptr("  " + strings.Repeat("u", limit+1) + "  ")},
			wantField: model.SettingJWUsername, wantContains: "4097"},

		{name: "jw_password 恰好 4096 字节通过", patch: Patch{JWPassword: ptr(strings.Repeat("p", limit))}},
		{name: "jw_password 4097 字节拒绝", patch: Patch{JWPassword: ptr(strings.Repeat("p", limit+1))},
			wantField: model.SettingJWPassword, wantContains: "4096"},

		{name: "ocr_base_url 恰好 4096 字节通过", patch: Patch{OCRBaseURL: ptr(httpURLOfBytes(t, limit))}},
		{name: "ocr_base_url 4097 字节拒绝", patch: Patch{OCRBaseURL: ptr(httpURLOfBytes(t, limit+1))},
			wantField: model.SettingOCRBaseURL, wantContains: "4096"},
		{name: "ocr_base_url 前后空白不计入上界", patch: Patch{OCRBaseURL: ptr("  " + httpURLOfBytes(t, limit) + "  ")}},
		{name: "ocr_base_url trim 后 4097 字节拒绝", patch: Patch{OCRBaseURL: ptr(" " + httpURLOfBytes(t, limit+1) + " ")},
			wantField: model.SettingOCRBaseURL, wantContains: "4097"},

		{name: "feishu_webhook 恰好 4096 字节通过", patch: Patch{FeishuWebhook: ptr(httpURLOfBytes(t, limit))}},
		{name: "feishu_webhook 4097 字节拒绝来自字节上界", patch: Patch{FeishuWebhook: ptr(httpURLOfBytes(t, limit+1))},
			wantField: model.SettingFeishuWebhook, wantContains: "4096", wantNotContains: "绝对地址"},

		{name: "feishu_sign_secret 恰好 4096 字节通过", patch: Patch{FeishuSignSecret: ptr(strings.Repeat("s", limit))}},
		{name: "feishu_sign_secret 4097 字节拒绝", patch: Patch{FeishuSignSecret: ptr(strings.Repeat("s", limit+1))},
			wantField: model.SettingFeishuSignSecret, wantContains: "4096"},
	}

	runValidateCases(t, cases)
}

// TestValidateWhitelistFieldByteLimit 覆盖白名单每条字段的 4096 字节上界：
// `jxlbh` 与 `name` 各自按 trim 后的字节数计，恰好 4096 通过、4097 拒绝；
// 上界是逐字段的，不是整个数组的（数组另有条数上限，见下一个用例）。
//
// 拒绝用例把违规条目放在第二条，并同时断言文案里的「第 2 条」「楼编号/展示名」
// 与「4096」三个要素（Requirement 4.7 / AC 7）：只断言字节数字会漏掉「哪一条、
// 哪个字段」，管理员拿到 42201 时仍不知道该改哪里。
func TestValidateWhitelistFieldByteLimit(t *testing.T) {
	const limit = model.MaxSettingValueBytes

	cases := []validateCase{
		{name: "楼编号恰好 4096 字节通过", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: strings.Repeat("B", limit), Name: "综合楼"},
		})}},
		{name: "楼编号 4097 字节拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: strings.Repeat("B", limit+1), Name: "实验楼"},
		})}, wantField: model.SettingBuildingWhitelist,
			wantContainsAll: []string{"第 2 条", "楼编号", "4096"}},
		{name: "楼编号前后空白不计入上界", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "  " + strings.Repeat("B", limit) + "  ", Name: "综合楼"},
		})}},

		{name: "展示名恰好 4096 字节通过", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: strings.Repeat("n", limit)},
		})}},
		{name: "展示名 4097 字节拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: "Q11", Name: strings.Repeat("n", limit+1)},
		})}, wantField: model.SettingBuildingWhitelist,
			wantContainsAll: []string{"第 2 条", "展示名", "4096"}},
		{name: "展示名 trim 后 4097 字节拒绝", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "综合楼"},
			{ID: "Q11", Name: "  " + strings.Repeat("n", limit+1) + "  "},
		})}, wantField: model.SettingBuildingWhitelist,
			wantContainsAll: []string{"第 2 条", "展示名", "4097", "4096"}},
		{name: "展示名前后空白不计入上界", patch: Patch{Whitelist: buildingListPtr([]model.Building{
			{ID: "Q10", Name: "  " + strings.Repeat("n", limit) + "  "},
		})}},
	}

	runValidateCases(t, cases)
}

// TestValidateWhitelistIsNotSubjectToByteLimit 是反向用例：200 条白名单序列化后
// 超过 4096 字节仍然合法——4096 逐字段作用在 `jxlbh`/`name` 上，整个数组没有字节
// 上界，条数由 MaxWhitelistEntries 管。
func TestValidateWhitelistIsNotSubjectToByteLimit(t *testing.T) {
	list := make([]model.Building, 0, model.MaxWhitelistEntries)
	for i := range model.MaxWhitelistEntries {
		list = append(list, model.Building{
			ID:   fmt.Sprintf("B%03d", i+1),
			Name: strings.Repeat("教", 40), // 每条约 120 字节，200 条约 24 KiB
		})
	}

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("用例前提不成立：白名单序列化失败：%v", err)
	}
	if len(raw) <= model.MaxSettingValueBytes {
		t.Fatalf("用例前提不成立：序列化后只有 %d 字节，未超过上界 %d", len(raw), model.MaxSettingValueBytes)
	}

	if err := Validate(Patch{Whitelist: buildingListPtr(list)}); err != nil {
		t.Fatalf("200 条白名单即使超过 4096 字节也应合法，实际 err=%v", err)
	}
}

// TestValidateSecretPlaintextNotTrimmed 钉住「秘密明文不 trim」：只有恰好空串才是清除，
// 带空格的秘密照常作为值校验（密码可能以空格结尾）；feishu_webhook 因同时要求 URL
// 形状，空白原文按形状非法拒绝，而不是被悄悄 trim 成空。
func TestValidateSecretPlaintextNotTrimmed(t *testing.T) {
	cases := []validateCase{
		{name: "密码只有空格按明文接受", patch: Patch{JWPassword: ptr("   ")}},
		{name: "密码带前后空格按明文接受", patch: Patch{JWPassword: ptr("  secret  ")}},
		{name: "签名密钥带前后空格按明文接受", patch: Patch{FeishuSignSecret: ptr("  sign  ")}},
		{name: "webhook 只有空格拒绝", patch: Patch{FeishuWebhook: ptr("   ")},
			wantField: model.SettingFeishuWebhook, wantContains: "绝对地址"},
	}

	runValidateCases(t, cases)
}

// TestValidateDoesNotMutateWhitelist 钉住 Validate 的纯函数性质：它只判不写，
// 不 trim 入参、不重排。归一化与落库（Requirement 4.6 的「按提交顺序返回去除空白后的
// 条目」）属于写入侧，不能靠校验顺手改调用方的数据。
func TestValidateDoesNotMutateWhitelist(t *testing.T) {
	list := []model.Building{{ID: "  Q10  ", Name: " 综合楼 "}}

	if err := Validate(Patch{Whitelist: &list}); err != nil {
		t.Fatalf("带空白的合法白名单应通过校验，实际 err=%v", err)
	}
	if list[0].ID != "  Q10  " || list[0].Name != " 综合楼 " {
		t.Fatalf("Validate 不应修改入参：got %+v", list[0])
	}
}
