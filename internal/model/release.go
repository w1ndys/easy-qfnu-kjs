// 本文件属于 entity 层：定义发布版本与观测事实实体（docs/contract/db.v2.sql 的 release 与 observation）。

package model

import "time"

// Release 是一次发布：只保留 current 与 previous 两份。
type Release struct {
	ID          string    // 可读短 id（时间戳串），无内容寻址
	Term        string    // 所属学期
	GeneratedAt time.Time // 发布时刻
	DictVersion int       // 该版数据用的字典版本
	AxisVersion int       // 该版数据用的节次轴版本
	IsCurrent   bool      // 是否为当前版本
}

// Observation 是一格观测事实：一间房 × 一周 × 一天 × 一个节次恰一行。
type Observation struct {
	ReleaseID string
	RoomID    string // jsbh
	Term      string // 冗余自 release，便于单表直查
	Week      int
	Weekday   int // 1=周一 … 7=周日
	Node      string
	StateKey  StateKey
	Available bool   // 字典 available 在发布期的投影，查询不必回连字典
	RawText   string // 仅 unknown / composite / 非标准写法保留原文
}
