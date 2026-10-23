// 本文件属于 entity 层：定义房间、学期日历与节次轴实体，与 docs/contract/db.v2.sql 的表逐张对应。

package model

import "time"

// Room 是房间目录里的一条：身份是 jsbh（ID），展示名允许重复，同名房间不合并。
type Room struct {
	ID           string     // jsbh，房间唯一身份
	Name         string     // 规范化展示名（最近一次）
	NameRaw      string     // 最近一次见到的原始名，含容量片段
	BuildingID   string     // 返回该房间的 jxlbh；状态行本身不带楼，不能从房名反推
	BuildingName string     // 该次采集时的教学楼展示名
	FirstSeen    *time.Time // 首次出现的日期；还没有数据时为 nil
	LastSeen     *time.Time // 最近出现的日期；还没有数据时为 nil
}

// Term 是一个学期。
type Term struct {
	Term       string // 例如 2025-2026-3
	TotalWeeks int    // 总周数
	Timezone   string // 固定 Asia/Shanghai
}

// TermWeek 是学期里的一周：周次与那一周的周一绝对日期。
type TermWeek struct {
	Term       string
	Week       int
	Monday     time.Time // 该周的周一（日期），消费方不再自己算周次
	InCalendar bool      // 是否在教学周历内
}

// AxisNode 是节次轴上的一个节点，Block 是它所属的大节编码。
type AxisNode struct {
	AxisVersion  int
	Node         string // '01'..'12'
	Ordinal      int    // 展示顺序
	Block        string // '0102' / '030405' / '0607' / '0809' / '101112'
	BlockOrdinal int    // 大节的展示顺序
}
