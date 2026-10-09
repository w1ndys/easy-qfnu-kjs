// 本文件属于 entity 层：定义查询侧“当前落在哪个学期、哪一周、星期几”的解析结果。

package model

import "time"

// CalendarContext 是服务端按时钟与日历表解析出的时间位置。
// InCalendar 为 false 表示不在教学周历内，这是正常返回而不是错误。
type CalendarContext struct {
	Date       time.Time // 目标日期（Asia/Shanghai）
	Term       string    // 目标日期所属学期
	Week       int       // 教学周次
	Weekday    int       // 1=周一 … 7=周日
	InCalendar bool      // 是否落在教学周历内
}
