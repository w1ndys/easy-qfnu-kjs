// 本文件是 business 层的单元测试：逐周新鲜度判定。

package query

import (
	"testing"
	"time"
)

// TestWeekStale 覆盖没有数据、没有成功时间、统一 8 天阈值和采集失败。
func TestWeekStale(t *testing.T) {
	// 固定“现在”，阈值不再区分当前周与远端周
	now := time.Date(2025, 9, 17, 10, 0, 0, 0, time.UTC)
	hoursAgo := func(hours int) *time.Time { at := now.Add(-time.Duration(hours) * time.Hour); return &at }
	daysAgo := func(days int) *time.Time { at := now.AddDate(0, 0, -days); return &at }

	cases := []struct {
		name        string     // 用例名称，失败时打出来
		published   bool       // 当前版本是否有这一周的观测
		lastSuccess *time.Time // 该周所在成功 release 的生成时间，nil 表示从未成功
		runFailed   bool       // 最近一轮采集是否失败
		want        bool       // 期望的过期判定结果
	}{
		{"没有数据不算过期", false, nil, false, false},
		{"有数据没有成功时间算过期", true, nil, false, true},
		{"采集失败且已有成功时间算过期", true, daysAgo(1), true, true},
		{"未收录但上一版成功后采集失败也算过期", false, daysAgo(1), true, true},
		{"40 小时仍新鲜", true, hoursAgo(40), false, false},
		{"刚好 8 天仍新鲜", true, daysAgo(8), false, false},
		{"超过 8 天算过期", true, daysAgo(9), false, true},
	}

	for _, testCase := range cases {
		got := WeekStale(now, testCase.published, testCase.lastSuccess, testCase.runFailed)
		if got != testCase.want {
			t.Errorf("%s：stale = %v，期望 %v", testCase.name, got, testCase.want)
		}
	}
}

// TestPublishedLookup 覆盖 published 只认 current，last_success_at 取该周所在成功 release。
func TestPublishedLookup(t *testing.T) {
	currentAt := time.Date(2025, 9, 15, 4, 10, 12, 0, time.UTC)
	previousAt := time.Date(2025, 9, 8, 4, 10, 12, 0, time.UTC)
	index := publishedIndex{
		current:    map[weekKey]bool{{term: "2025-2026-1", week: 2}: true},
		previous:   map[weekKey]bool{{term: "2025-2026-1", week: 1}: true, {term: "2025-2026-1", week: 2}: true},
		currentAt:  currentAt,
		previousAt: previousAt,
	}

	// 当前版本有观测才算已发布，时间用当前版本生成时间
	published, at := index.lookup("2025-2026-1", 2)
	if !published || at == nil || !at.Equal(currentAt) {
		t.Errorf("current 周 published=%v at=%v，期望 true 与 current 生成时间", published, at)
	}
	// 上一版有、当前没有：未发布，最近成功时间仍取上一版
	published, at = index.lookup("2025-2026-1", 1)
	if published || at == nil || !at.Equal(previousAt) {
		t.Errorf("previous-only 周 published=%v at=%v，期望 false 与 previous 生成时间", published, at)
	}
	// 两版都没有就是未收录，没有成功时间
	published, at = index.lookup("2025-2026-1", 3)
	if published || at != nil {
		t.Errorf("两版都没有 published=%v at=%v，期望 false 与 nil", published, at)
	}
}
