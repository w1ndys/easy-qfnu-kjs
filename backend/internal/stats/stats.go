// Package stats 提供访问统计：异步记账、PV/UV 聚合、维度日表与过期清理。
//
// 记账在查询响应写出之后投递（Requirement 34.1）：查询路径只做一次非阻塞入队，
// 真正的落库由后台 goroutine 批量执行，失败只记日志（CP18）。
// 统计表只写计数与匿名摘要，不含 IP、User-Agent、来源页与查询原文（Requirement 34.9、CP16）。
//
// 本包依赖 store 的仓储入口（design「依赖方向」stats → store），不直接持有数据库连接。
package stats

import "time"

// shanghai 是业务时区。与 query 一致用固定 +08:00 而不 LoadLocation：
// 2026 年之后没有夏令时，固定偏移与 Asia/Shanghai 等价，也免去容器缺 tzdata 的分支。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

const (
	// TopN 是排行榜条数：教学楼、教室、关键词各取前 10（Requirement 34.5）。
	TopN = 10
	// PeakTopN 是峰值时段个数：访问量最高的前三个小时段（Requirement 34.6）。
	PeakTopN = 3
	// LeaderboardDays 是排行榜的统计窗口。
	// Requirement 34.5 只要求「Top 10」而没有给窗口；这里取近 7 日，与公开页默认口径一致。
	LeaderboardDays = 7
	// TrendDays 是逐日趋势的窗口：近 30 日（Requirement 34.7）。
	// 近 7 日口径由这份序列的最后 LeaderboardDays 天表达，不必再返回第二份数据。
	TrendDays = 30
	// PeakDays7 是热力与峰值窗口的近 7 日口径（PRD「高峰时段分析」的切换之一）。
	PeakDays7 = 7
	// PeakDays30 是热力与峰值窗口的近 30 日口径。
	PeakDays30 = 30
	// DefaultPeakDays 是未指定或非法峰值窗口时的默认值。
	DefaultPeakDays = PeakDays7
	// RetentionDays 是统计保留天数：早于「今天减 90 天」的记录按日清理（Requirement 34.10）。
	RetentionDays = 90
	// MaxKeywordRunes 是关键词入库的字符上限。
	// 与公开查询的 keyword 上限一致；查询侧已拒绝超长输入，这里再截一次，
	// 保证从别的调用方进来的关键词也不会把日表撑长（Requirement 34.5）。
	MaxKeywordRunes = 32
	// MaxDimensionRunes 是楼编号与房号入库的字符上限，与 keyword、room_id 的上限一致。
	// 两者同样来自公开请求，长度不受信任：超长键会撞上统计日表的 btree 索引条目上限，
	// 让同批写入的整段维度失败，所以在记账侧统一夹取（Requirement 34.5）。
	MaxDimensionRunes = 32
)

// 记账队列与后台批量的默认参数。
// 队列满时丢弃并记日志：宁可少记几次访问，也不让统计反压查询（CP18）。
const (
	defaultQueueSize     = 4096
	defaultBatchSize     = 256
	defaultFlushInterval = 2 * time.Second
	// writeTimeout 是单批写入的上限，避免库抖动时后台 goroutine 长期挂住。
	writeTimeout = 5 * time.Second
	// cleanupInterval 是清理任务的执行间隔：按日执行（Requirement 34.10）。
	cleanupInterval = 24 * time.Hour
	// hashVersion 是摘要算法的版本前缀，将来换算法时可与新值区分。
	hashVersion = "v1"
)

// dayOf 把时刻归一到 Asia/Shanghai 的当日 00:00。
func dayOf(at time.Time) time.Time {
	local := at.In(shanghai)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)
}

// isoWeekday 返回 1=周一 … 7=周日，与统计表与响应口径一致。
func isoWeekday(at time.Time) int {
	weekday := int(at.In(shanghai).Weekday())
	// time.Weekday 里周日是 0，统计口径里周日是 7。
	if weekday == 0 {
		return 7
	}
	return weekday
}

// dateKey 是只按年月日比较的业务日期键。
// 用 time.Time 直接做 map 键会把单调时钟与时区表示带进比较，换一种构造方式就不相等。
type dateKey struct {
	year  int
	month time.Month
	day   int
}

// keyOfDay 把业务日期折成 dateKey。
func keyOfDay(day time.Time) dateKey {
	return dateKey{year: day.Year(), month: day.Month(), day: day.Day()}
}

// time 把 dateKey 还原成上海时区当日 00:00 的 time.Time；仓储按这个日期编码成 date。
func (k dateKey) time() time.Time {
	return time.Date(k.year, k.month, k.day, 0, 0, 0, 0, shanghai)
}
