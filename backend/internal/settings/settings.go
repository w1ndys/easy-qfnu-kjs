// Package settings 管理业务设置的请求结构、局部更新语义与纯校验。
//
// 依赖方向见 specs/2026-11-08-admin-settings-sync/design.md：本包只 import 标准库、
// model、cronexpr 与 secret，不碰 store 与 api。本文件的校验因此可以脱离数据库与密钥
// 单独运行；秘密的加封与落库由 service.go 的 Service 通过注入的窄接口完成。
package settings

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/cronexpr"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// Patch 是 PUT /api/admin/settings 的局部更新请求。指针用来区分「省略」与「提交空串」：
// nil = 保持原值；指向 "" = 删除该设置项；指向非空值 = 覆盖。白名单同理，`[]` 是
// 「清空白名单」而不是「省略」（Requirement 2.1–2.6、CP6）。
//
// JSON 键名就是设置表里的键，与 handler 的严格解码共用同一份结构体：拼错的键
// （如 cron_express）在解码阶段就被 DisallowUnknownFields 挡成 40001（D10）。
type Patch struct {
	JWUsername       *string           `json:"jw_username"`
	JWPassword       *string           `json:"jw_password"`
	OCRBaseURL       *string           `json:"ocr_base_url"`
	FeishuWebhook    *string           `json:"feishu_webhook"`
	FeishuSignSecret *string           `json:"feishu_sign_secret"`
	CronExpr         *string           `json:"cron_expr"`
	Whitelist        *[]model.Building `json:"building_whitelist"`
}

// FieldError 是纯校验失败时的定位信息。Field 是设置键名（model 的七个键常量之一），
// Message 是面向管理员的原因文案。两者都不回显**秘密明文**：feishu_webhook、
// jw_password 都是秘密项，错误消息和响应体一样不能带明文（CP1）。非秘密项的值可以
// 出现在文案里——重复的楼编号要照实点名（Requirement 4.2），cron 的报错片段也来自
// 提交的表达式，它们本来就不是秘密。
//
// 它包装 model.ErrSettingInvalid（Unwrap 恒返回该哨兵），API 层因此既能用
// errors.As 取出字段名拼 42201 的逐字段消息，也能用 errors.Is 只判类别
// （Requirement 6.3、design「哨兵的包装关系」）。
type FieldError struct {
	Field   string // 设置键名，取值限于 model 的七个键常量
	Message string // 中文原因，点名字段位置或越界口径，不含秘密明文
}

// Error 返回「字段名：原因」。字段名放在最前面，是为了让直接透出的 42201 消息
// 第一眼就能看出该改哪一项；原因里不再重复字段名。
func (e *FieldError) Error() string {
	return fmt.Sprintf("%s：%s", e.Field, e.Message)
}

// Unwrap 恒返回 model.ErrSettingInvalid，供 errors.Is 判定类别 → 42201。
// 它是常量返回值，不随 Field/Message 变化：字段级信息由 errors.As 取。
func (e *FieldError) Unwrap() error {
	return model.ErrSettingInvalid
}

// Validate 是 PUT /api/admin/settings 的纯校验：只看 Patch 里出现的字段，不把
// trim 结果写回、不取密钥、不读库，所以能脱离 Service 单独做表驱动。
//
// 只校验「出现」的字段：nil 表示省略，一律放行；空串是否合法按项而定——五项文本项
// 的空串是「清除」，cron_expr 的空串与全空白非法（trim 后为空，设计表「设置项」），
// 白名单的空数组合法。
//
// 校验顺序固定在设置项表的键顺序：jw_username → jw_password → ocr_base_url →
// feishu_webhook → feishu_sign_secret → cron_expr → building_whitelist。遇到第一处
// 失败就返回，因为需求 6.3 只要求回一条指出字段与原因的消息，整次保存都不落库
// （D10、CP5）。单项内部的口径见各 validate* 函数。
func Validate(p Patch) error {
	if err := validateTrimmedText(model.SettingJWUsername, p.JWUsername, false); err != nil {
		return err
	}
	if err := validateSecretText(model.SettingJWPassword, p.JWPassword, false); err != nil {
		return err
	}
	if err := validateTrimmedText(model.SettingOCRBaseURL, p.OCRBaseURL, true); err != nil {
		return err
	}
	if err := validateSecretText(model.SettingFeishuWebhook, p.FeishuWebhook, true); err != nil {
		return err
	}
	if err := validateSecretText(model.SettingFeishuSignSecret, p.FeishuSignSecret, false); err != nil {
		return err
	}
	if err := validateCronExpr(p.CronExpr); err != nil {
		return err
	}
	return validateWhitelist(p.Whitelist)
}

// validateTrimmedText 校验非秘密文本项（jw_username、ocr_base_url）：先 TrimSpace，
// 空串或 trim 后为空表示清除、合法；需要 URL 形状时判 trim 后的值；最后按 trim 后的
// 字节数比上界。
//
// 顺序不能反过来：先量原文会让前后空白撑大 4096 的计数，先判形状会让带空白的值
// 被判非法，而这两个结果都和「存 trim 后的值」对不上（design「设置项」）。
func validateTrimmedText(field string, value *string, requireURL bool) error {
	if value == nil {
		return nil
	}

	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}

	if requireURL && !isHTTPAbsoluteURL(trimmed) {
		return invalid(field, "必须是 http/https 绝对地址")
	}
	if n := len(trimmed); n > model.MaxSettingValueBytes {
		return invalidf(field, "去除两侧空白后 %d 字节，超过上限 %d 字节", n, model.MaxSettingValueBytes)
	}

	return nil
}

// validateSecretText 校验秘密项的明文（jw_password、feishu_webhook、feishu_sign_secret）：
// 只有恰好是空串才表示清除，明文不 trim——密码可能以空格结尾，trim 会改掉凭据
// （design「设置项」）。需要 URL 形状时按提交原文判：原文带前后空白就是形状非法，
// 而不是被悄悄 trim 掉，否则存进库的与校验过的不是同一串字符。
//
// 上界按提交原文的字节数计，与落库明文同口径。
func validateSecretText(field string, value *string, requireURL bool) error {
	if value == nil {
		return nil
	}

	raw := *value
	if raw == "" {
		return nil
	}

	if requireURL && !isHTTPAbsoluteURL(raw) {
		return invalid(field, "必须是 http/https 绝对地址")
	}
	if n := len(raw); n > model.MaxSettingValueBytes {
		return invalidf(field, "明文 %d 字节，超过上限 %d 字节", n, model.MaxSettingValueBytes)
	}

	return nil
}

// validateCronExpr 先 TrimSpace，再把结果原样交给 cronexpr.Parse：校验与调度装载
// 共用同一个解析器，才不会有「存进去却排不了」或「能排的存不进去」的组合（CP8）。
// 写入侧同样存 trim 后的值，与其余非秘密文本项同口径（design「设置项」）。
//
// trim 只去掉首尾空白：管理员粘贴时带上的换行、空格不该让一次保存失败；全空白 trim
// 后是空串，仍按「表达式为空」拒绝（42201）。字段之间的重复空格不 trim——Parse 用
// 单个空格切分，切出的空字段由字段数或字段解析报错，那是另一种写法而不是粘贴噪声。
// 6 段（秒级）写法也由 Parse 拒绝，这里不补齐字段。
//
// 只判语法，不判可达性：Parse 通过但 Next 五年内无命中的表达式（如 `0 0 30 2 *`）
// 仍算合法，GET 快照的 cron_next_at 返回 null（Requirement 1.4）。也不量字节：
// 4096 上界只管五项文本项，cron 的口径是语法（design「设置项」）。
func validateCronExpr(value *string) error {
	if value == nil {
		return nil
	}

	if _, err := cronexpr.Parse(strings.TrimSpace(*value)); err != nil {
		return invalid(model.SettingCronExpr, err.Error())
	}

	return nil
}

// validateWhitelist 校验白名单：条数 ≤ MaxWhitelistEntries，每条楼编号 trim 后非空且
// 唯一、展示名 trim 后非空；空数组合法，指针为 nil 表示省略、不校验。
//
// 这里只判不写：不 trim 入参、不重排、不去重，归一化与落库留给写入侧（D11 要求顺序
// 即裁决顺序）。编号唯一性按 trim 后的原字符串精确比较：spec 的口径是「trim 后唯一」，
// 没有大小写折叠的要求，需要折叠时再补一条规则。
func validateWhitelist(list *[]model.Building) error {
	if list == nil {
		return nil
	}

	entries := *list
	if len(entries) > model.MaxWhitelistEntries {
		return invalidf(model.SettingBuildingWhitelist, "条数 %d 超过上限 %d 条",
			len(entries), model.MaxWhitelistEntries)
	}

	seen := make(map[string]struct{}, len(entries))
	for i, building := range entries {
		id := strings.TrimSpace(building.ID)
		if id == "" {
			return invalidf(model.SettingBuildingWhitelist, "第 %d 条的楼编号去除空白后为空", i+1)
		}
		if _, duplicated := seen[id]; duplicated {
			return invalidf(model.SettingBuildingWhitelist, "第 %d 条的楼编号 %q 与前面的条目重复", i+1, id)
		}
		seen[id] = struct{}{}

		if strings.TrimSpace(building.Name) == "" {
			return invalidf(model.SettingBuildingWhitelist, "第 %d 条的展示名去除空白后为空", i+1)
		}
	}

	return nil
}

// isHTTPAbsoluteURL 判定 raw 是否是 http/https 绝对地址：scheme 只认 http 与 https
// （大小写不敏感，RFC 3986 里 scheme 本就大小写无关），且必须带主机——`http://`
// 能被 url.Parse 解析成绝对 URL，但没有主机，算不得可请求的地址。
//
// 相对地址（`/api/ocr`）、协议相对地址（`//host/path`）、别的 scheme（`ftp://`、
// `javascript:`）都在这里被拒。不做「自动补 http://」这类宽容：补出来的值与管理员
// 提交的不一样，回显和排障都会对不上。
func isHTTPAbsoluteURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return false
	}

	return strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")
}

// invalid 构造一条点名字段的校验错误。所有拒绝路径都走它，避免有的分支漏掉
// Field 或退回裸的 model.ErrSettingInvalid（那样 42201 的消息就少了字段名）。
func invalid(field, message string) error {
	return &FieldError{Field: field, Message: message}
}

// invalidf 是 invalid 的格式化版本，用于把条数、字节数、条目序号写进原因。
func invalidf(field, format string, args ...any) error {
	return invalid(field, fmt.Sprintf(format, args...))
}
