package model

// 语义键常量是字典契约里不随版本变化的三个特殊键，对应 dict_state 的种子行：
// 空文本投影成 free，多符号组合投影成 composite，未收录投影成 unknown。
// 其余语义键一律来自 dict_symbol，不在代码里另立状态表
// （Requirement 4.1–4.4、CP4）。
const (
	StateKeyFree      = "free"
	StateKeyComposite = "composite"
	StateKeyUnknown   = "unknown"
)

// DictState 是字典里一条状态的定义，对应 dict_state 的一行。
// 可用性只取 Available；clean 不在代码里维护第二套「哪些状态算空闲」的判断
// （Requirement 4.4、CP4）。
type DictState struct {
	Label     string // 展示名，如 正常上课
	Available bool   // 是否算可用，是 observation.available 的唯一来源
}

// Dict 是一次清洗使用的字典快照，由调用方注入，clean 不读库：
// Symbols 对应 dict_symbol（符号原文 → 语义键），States 对应 dict_state（语义键 → 定义）。
// 两个映射都允许缺项：缺项按「未收录」处理——宁可判未知或不可用，
// 也不把不确定的格子说成空闲（与查询层「字典缺行一律算已占用」同口径）。
type Dict struct {
	Symbols map[string]string    // 符号原文 → 语义键
	States  map[string]DictState // 语义键 → 状态定义
}

// Observation 是清洗后的观测，对应 observation 表里的 state_key / available / raw_text 三列。
// RawText 恒为调用方传入的原文，不做 trim、不做全半角归一：
// 未知与复合时靠它回溯上游到底写了什么（Requirement 4.3）。
type Observation struct {
	StateKey  string // 语义键，写入 observation.state_key
	Available bool   // 可用性，写入 observation.available
	RawText   string // 状态原文，写入 observation.raw_text
}
