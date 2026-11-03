package model

// OccupancyEntry 是占用说明里的一条记录：某教室某状态格的一条占用或考试说明。
// 四个字段都保留上游原文，语义键、周次求交与派生判定交给 clean
// （本 spec specs/2026-11-03-collect-pipeline Requirement 3.1、4、6）。
// StateRaw 与 MatrixCell.StateRaw 同名同义：都是 clean 投影前的原文，
// clean 按字典把它映射成 occupancy_detail.state_key；两处保持一致，不要单独改名。
type OccupancyEntry struct {
	StateRaw  string // 教室状态原文（clean 前），如 正常上课；clean 后是 occupancy_detail.state_key
	Course    string // 课程或考试科目原文；上游把考试科目也写在这个字段
	WeekRange string // 周次范围原文，如 1-16；与采集窗口求交交给 clean
	TimeFlag  string // 时间标志原文，如 单双周
}
