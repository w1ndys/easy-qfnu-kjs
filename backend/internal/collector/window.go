package collector

import "time"

// shanghai 是业务时区。与 pipeline / upstream / query 一致用固定 +08:00 而不
// LoadLocation：2026 年之后没有夏令时，固定偏移与 Asia/Shanghai 等价，
// 也免去容器缺 tzdata 的分支。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// anchorMonday 按周历页自报的当前教学周 C 反推学期第 1 周周一：
//
//	第 1 周周一 = 采样日所在周周一 − 7×(C−1)
//
// 公式取自 docs/pitfalls.md 第 2 节，日期按 Asia/Shanghai（固定 +08:00）解释：
// 先把采样时刻换算到业务时区再取年月日，UTC 或本地时区传入的时刻在跨零点时
// 不会落到前一天而算错一周。
//
// 周一只认周一：time.Weekday 的周日为 0，用 (weekday+6)%7 折成「距本周周一几天」，
// 否则周日会被算成下一周的开始。currentWeek 由 parser.WeekText 保证 1 ≤ C ≤ Y，
// 本函数不重复校验；C < 1 会得到第 1 周之后的日期，不属于正常输入。
func anchorMonday(now time.Time, currentWeek int) time.Time {
	local := now.In(shanghai)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)
	daysSinceMonday := (int(day.Weekday()) + 6) % 7
	monday := day.AddDate(0, 0, -daysSinceMonday)
	return monday.AddDate(0, 0, -7*(currentWeek-1))
}
