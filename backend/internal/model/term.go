package model

import "time"

// TermWeek 是学期里的一周：属于哪个学期、第几周、以及该周周一的业务日期。
type TermWeek struct {
	Term       string    // 学期标识，如 2026-2027-1
	Week       int16     // 教学周序号，从 1 开始
	Monday     time.Time // 该周周一，纯日期，按 Asia/Shanghai 解释
	InCalendar bool      // 该周是否在教学周内
}
