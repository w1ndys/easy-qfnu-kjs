// 本文件属于 business 层：逐周新鲜度判定。
// 规则见 docs/contract/api.v2.md：任一周距最近成功超过 8 天为过期；最近一次采集失败也算过期。

package query

import "time"

const (
	// staleAfter 是任一周距最近成功的过期阈值，对应每周一次的全量同步。
	staleAfter = 8 * 24 * time.Hour
)

// WeekStale 判断某一周的数据是否过期。
// 没有任何成功时间时：当前版本有观测却无法证明新鲜，算过期；否则是未收录，不算过期。
// 有最近成功时间后，超过 8 天或最近一次采集失败都算过期，与该周远近无关。
func WeekStale(now time.Time, published bool, lastSuccess *time.Time, runFailed bool) bool {
	// 从未成功过就没有可比较的发布时间
	if lastSuccess == nil {
		return published
	}
	// 最近一次采集失败时，已有成功数据不再可信
	if runFailed {
		return true
	}
	// 超过 8 天才算过期，刚好 8 天仍算新鲜
	return now.Sub(*lastSuccess) > staleAfter
}
