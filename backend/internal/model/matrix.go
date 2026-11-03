package model

// AxisNode 是节次轴上的一个节点及其大节归属；采集按它校验并解释周矩阵表头。
type AxisNode struct {
	Node         string // 小节编号，如 01
	Ordinal      int    // 小节在一天内的顺序，从 1 开始
	Block        string // 所属大节编码，如 0102（对应表头的 tdvalue）
	BlockOrdinal int    // 大节在一天内的顺序，从 1 开始
}

// MatrixCell 是周矩阵里的一格：某天某大节的状态原文，语义键由 clean 投影。
// StateRaw 与 OccupancyEntry.StateRaw 同名同义：都是 clean 投影前的原文，
// clean 按字典把它映射成 observation.state_key（与 occupancy_detail.state_key 同一套字典），
// 两处字段名保持一致，避免后续只改一边。
type MatrixCell struct {
	Weekday  int    // 星期，1=周一 … 7=周日
	Block    string // 大节编码，如 0102
	StateRaw string // 状态原文，未清洗；空白格为空串
}

// MatrixRoom 是周矩阵里的一行：一间教室及其逐格原文。
// 格子数由轴推导：7 天 × 大节数（当前种子 5 个大节 → 35 格），不要在这里写死格数。
type MatrixRoom struct {
	RoomID  string       // 教室编号，即上游 jsbh，房号的唯一身份
	NameRaw string       // 首列原始房名，含容量片段；规范化交给 clean.RoomName
	Cells   []MatrixCell // 按表头顺序展开的逐格原文
}
