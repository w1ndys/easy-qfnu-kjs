// 本文件属于 entity 层：定义状态语义键与状态字典实体。
// 语义键的含义由所属 dict_version 决定，取值见 docs/contract/data-format.v2.md。

package model

// StateKey 是状态语义键，例如 free、class、unknown。
// 用语义键而不是整数 ID，是为了让数据自描述，字典改版不静默污染旧数据。
type StateKey string

// 状态语义键取值，与契约的种子表逐条对应。
const (
	StateFree            StateKey = "free"             // 空闲：空单元格，或文字“空闲”
	StateFullyFree       StateKey = "fully_free"       // 完全空闲
	StateClass           StateKey = "class"            // 正常上课
	StateBorrowed        StateKey = "borrowed"         // 借用
	StateLocked          StateKey = "locked"           // 锁定
	StateExam            StateKey = "exam"             // 考试
	StateFixedReschedule StateKey = "fixed_reschedule" // 固定调课
	StateTempReschedule  StateKey = "temp_reschedule"  // 临时调课
	StateCrossMode       StateKey = "cross_mode"       // 跨模式占用
	StateComposite       StateKey = "composite"        // 复合占用：多个已收录符号
	StateUnknown         StateKey = "unknown"          // 未收录文本
)

// IsUsable 判断该状态在空间意义上是否可用。
// 只有 free 与 fully_free 可用；unknown 与 composite 一律不可用，绝不把未知当空闲。
func (k StateKey) IsUsable() bool {
	// 两个可用状态直接放行，其余全部按占用处理
	if k == StateFree || k == StateFullyFree {
		return true
	}
	return false
}

// DictState 是字典里的一个状态：语义键、中文名、是否可用。
type DictState struct {
	StateKey  StateKey
	Label     string
	Available bool
}

// DictSymbol 是字典里的一条符号映射：上游原文本 → 状态键。
// 符号码点不得规范化成 ASCII：Ｊ/Ｘ/Ｇ/Ｌ 是全角拉丁字母，Κ 是希腊大写 Kappa。
type DictSymbol struct {
	Symbol   string
	StateKey StateKey
}

// Dict 是一份完整且不可变的字典，Version 即 dict_version。
type Dict struct {
	Version int
	States  []DictState
	Symbols []DictSymbol
}
