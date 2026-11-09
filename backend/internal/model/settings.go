package model

import "time"

// 七个业务设置键，对应 settings 表的 key 列。键集合在代码里固定：
// 读取只取这七个，写入只接受这七个；键之外的行走不到管理端
// （Requirement 1.1 / 6.2 / Glossary）。
const (
	SettingJWUsername        = "jw_username"
	SettingJWPassword        = "jw_password"
	SettingOCRBaseURL        = "ocr_base_url"
	SettingFeishuWebhook     = "feishu_webhook"
	SettingFeishuSignSecret  = "feishu_sign_secret"
	SettingCronExpr          = "cron_expr"
	SettingBuildingWhitelist = "building_whitelist"
)

const (
	// DefaultCronExpr 是 cron_expr 缺行时的回落值：业务时区（+08:00）每周六
	// 04:10（Requirement 9.5）。
	//
	// 必须是 5 字段（分 时 日 月 周），与 cronexpr.Parse 的语法一致：多一段秒
	// 会让这个默认值自己过不了校验，回落值就成了「又一条非法表达式」。
	// 0001 的种子行早先写的是 6 段 `0 10 4 * * 6`，由 0005 迁移就地改写成这串；
	// 0001 本身已应用，一个字节都不改。
	DefaultCronExpr = "10 4 * * 6"

	// MaxWhitelistEntries 是一条白名单的条数上限。按 4 周窗口 × 200 楼 = 800 条周结果
	// 估算，详情响应仍然可控（Requirement 4.4）。
	MaxWhitelistEntries = 200

	// MaxSettingValueBytes 是文本项明文的上界，作用于五个文本设置项：jw_username、
	// jw_password、ocr_base_url、feishu_webhook、feishu_sign_secret（见 design.md
	// 「Data Models → 设置项」表），以及 building_whitelist 每条的两个字段（`jxlbh`
	// 与 `name` 各自 trim 后计，不是整个数组）；cron_expr 不套这个上界，只看 5 字段
	// 语法。
	//
	// 它限的是管理员**提交的明文**字节数，不是库里的值长度：校验发生在加封之前，
	// 明文超限在 Seal 之前就被拒。计数口径跟着存储口径走：非秘密文本项按 TrimSpace
	// 后计（trim 后存），秘密项按提交原文计（秘密明文不 trim）。
	//
	// feishu_webhook 虽然是 URL，也照样受这个上界：它是秘密项，落库是 v1: 信封。
	// 信封长度 ≤ 3 + 4·ceil((p+28)/3)：v1: 前缀在 base64 之外（单独 3 字节），p 是明文
	// 字节数，p+28 是 nonce 12 + 密文 + GCM tag 16 的载荷长度；无填充 base64url 每 3 字节
	// 编成 4 字符，末组不足 3 字节也按整组 4 字符计，所以这是上界（p=4096 时该式给 5503，
	// 实际 5502）。secret.Open 的 64 KiB 长度门要求
	// 3 + 4·ceil((p+28)/3) ≤ 65536，即 p ≤ 49121；明文超过 49121 字节就成了「存得进、
	// 读不出」。这段开销由信封格式定死，密文上限因此由明文上界推出，不另设常量。
	// 对 ocr_base_url 这类非秘密文本项，上界只是统一口径、挡住误贴的整段网页，
	// 不是凭据的强度规则。
	MaxSettingValueBytes = 4096
)

// SettingKeys 返回七个业务设置键的副本，顺序即常量声明顺序。它有两个计划调用点
// （任务 4 / 任务 5）：LoadSettings 的 IN 取键，以及快照组装。这两处都从这里取集合，
// 不要在调用点各写一份字面量——漏掉一项，那个设置项就会读不出，而调用点本身不会有
// 任何编译错误（Requirement 1.1 / 6.2、Glossary）。
//
// 返回的是副本，调用方排序、过滤或追加都不会改到包级键集合。若改成返回包级切片本身，
// 那么改元素，或者截取后仍有剩余容量再 append，都会串改到其他调用方看到的键集合。
// 代价是每次调用一次小分配。
func SettingKeys() []string {
	return []string{
		SettingJWUsername,
		SettingJWPassword,
		SettingOCRBaseURL,
		SettingFeishuWebhook,
		SettingFeishuSignSecret,
		SettingCronExpr,
		SettingBuildingWhitelist,
	}
}

// Building 是白名单里的一条教学楼：楼编号沿用上游 jxlbh，展示名沿用 name。
// JSON 键名与 store 侧 meta 解析的存储形状一致——写入形状即读取形状，存进去的白名单
// 不改写就能被 meta 与采集复用（CP7）。顺序即归属裁决顺序，不要在持久化层重排（D11）。
type Building struct {
	ID   string `json:"jxlbh"`
	Name string `json:"name"`
}

// SettingRow 是 settings 表的一行原样读出（含 updated_at 列）。
// 秘密项在这里仍然是库里的密文：解密只有 settings.Service.Reveal 一个出口，
// 这个类型里没有能承载明文回显的字段（CP1）。
type SettingRow struct {
	Key       string    // 设置键，取值限于七个常量之一
	Value     string    // 原始值；秘密项是 v1: 信封密文
	Secret    bool      // 库里的 is_secret 列，只作记录，不决定是否回显（CP4）
	UpdatedAt time.Time // 该键最近一次写入时刻，原样取自库
}

// SettingChange 是一次事务里的单条变更。Delete 为 true 表示删除该键（清除语义），
// 此时 Value 与 Secret 被忽略；否则按键 upsert。
// 变更以切片成批提交，才能让「任一条失败整批回滚」有意义（Requirement 6.4）。
type SettingChange struct {
	Key    string // 设置键，取值限于七个常量之一
	Value  string // 待写入的最终存储值：秘密项是密文，白名单是 JSON 数组文本
	Secret bool   // 写入 is_secret 列的取值
	Delete bool   // true 表示删除该键，不做 upsert
}

// TriggerSource 是一轮采集的触发来源。它只进日志字段 trigger=，不落库（D7）：
// 验收没有按来源筛选的要求，加一列就要一版迁移。新增来源时同步改这个类型，
// 日志检索口径才不会悄悄多出一个值。
type TriggerSource string

const (
	// TriggerManual 表示管理员从 POST /api/admin/sync 手动发起。
	TriggerManual TriggerSource = "manual"
	// TriggerCron 表示进程内调度到点发起。
	TriggerCron TriggerSource = "cron"
)

// SecretSetting 报告某键是否为秘密设置项（只写不读）。三项秘密固定为
// jw_password、feishu_webhook、feishu_sign_secret。
//
// 判定只认键名，不看 settings.is_secret 列：手工插入一行 is_secret=false 的
// jw_password 也不会被快照回显（Requirement 3.4、CP4）。键名写错时一律按非秘密
// 处理，由写入侧的键白名单先挡下，不在这里兜底。
func SecretSetting(key string) bool {
	switch key {
	case SettingJWPassword, SettingFeishuWebhook, SettingFeishuSignSecret:
		return true
	default:
		return false
	}
}

// businessLocation 是业务时区 +08:00。用固定偏移而不是 time.LoadLocation：
// 2026 年之后中国没有夏令时，固定偏移与 Asia/Shanghai 完全等价，也不依赖容器里的
// tzdata（与 pipeline/query/publish 的既有口径一致）。
var businessLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// BusinessLocation 返回业务时区 +08:00。cron 求值、业务日期与落库时刻的换算都用它，
// 不要在调用点另建一个 time.FixedZone，否则同一个时刻会出现两种「本地时间」。
// 返回值是共享的只读定位器，调用方只应传阅，不要修改。
func BusinessLocation() *time.Location {
	return businessLocation
}
