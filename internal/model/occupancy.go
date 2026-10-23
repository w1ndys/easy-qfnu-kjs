// 本文件属于 entity 层：定义占用明细实体（migrations/0004_sync.sql 的 occupancy_detail）。

package model

// OccupancyDetail 是一格占用明细的一行：只有契约允许入库的四项加派生标记。
// 申请人、任课教师与原始 HTML 不在这里，库里也没有可以落它们的列。
type OccupancyDetail struct {
	RoomID    string   // 房间身份 jsbh
	Weekday   int      // 1=周一 … 7=周日
	Block     string   // 大节编码，取值见 axis_node.block
	StateKey  StateKey // 教室状态语义键，含义由该次发布的 dict_version 决定
	Course    string   // 课程名；考试科目出现在课程字段时同样落在这里
	WeekRange string   // 周次范围原文，解析交给派生逻辑
	TimeFlag  string   // 时间标志原文，例如 单周 / 双周
	Derived   bool     // 该说明是否用于给没有周矩阵的周补派生状态
}
