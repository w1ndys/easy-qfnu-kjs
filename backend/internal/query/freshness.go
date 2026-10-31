package query

import (
	"context"
	"fmt"
	"time"
)

// StaleAfter 是周矩阵的新鲜度阈值：距该周最近一次成功抓取超过该时长即判为过期。
// 取值来自 Requirement 17.3 的「8 天」。边界按「超过」处理：恰好 8 天不算过期，多 1 秒才算。
const StaleAfter = 8 * 24 * time.Hour

// runStatusFailed 是会让窗口内各周一起过期的轮次状态（Requirement 17.3）。
// 「部分完成」按 Requirement 17.4 明确不算失败，所以这里只认字面量 failed；
// 其余状态（running/success/blocked/skipped）也都不是失败，存储层只负责回原始状态。
const runStatusFailed = "failed"

// WeekFreshness 是一周的发布与新鲜度，形状直接对应 /api/meta 与 /api/screen 的 weeks 元素。
type WeekFreshness struct {
	Week          int        `json:"week"`            // 教学周次
	Published     bool       `json:"published"`       // 该周是否已发布：当前版本里有该周的观测
	LastSuccessAt *time.Time `json:"last_success_at"` // 该周矩阵最近一次成功抓取时间（UTC）；从未成功为 null
	Stale         bool       `json:"stale"`           // 是否过期（Requirement 17.3–17.4）
}

// FreshnessParams 是逐周新鲜度查询的业务入参。
// 周区间由调用方按采集窗口给出：Requirement 33.2 只要求覆盖窗口内各周，服务层不自己猜窗口。
type FreshnessParams struct {
	Term     string // 学期
	FromWeek int    // 窗口起始周（含）
	ToWeek   int    // 窗口结束周（含）
}

// WeekFacts 是存储层给出的某一周抓取事实。
type WeekFacts struct {
	Week          int        // 教学周次
	Published     bool       // 该周是否已有当前版本的观测
	LastSuccessAt *time.Time // 该周矩阵最近一次成功抓取时间；从未成功为 nil
}

// FreshnessFacts 是一段周区间的抓取事实与最近一轮同步状态。
// Weeks 只含查到的周：区间内没有事实的周由查询层补成「未发布、从未成功」。
type FreshnessFacts struct {
	Weeks         []WeekFacts // 按周次升序；可以缺周
	LastRunStatus string      // 最近一轮同步的原始状态；从未跑过为空串
}

// FreshnessLookup 是存储层注入点；用函数类型让 query 不依赖 pgx 与具体仓储实现。
type FreshnessLookup func(ctx context.Context, params FreshnessParams) (FreshnessFacts, error)

// FreshnessService 只做逐周新鲜度：不改动空教室与全天状态的既有判定，也不负责装路由。
type FreshnessService struct {
	lookup FreshnessLookup // 周抓取事实读取，由装配层注入
}

// NewFreshness 组装新鲜度服务。
func NewFreshness(lookup FreshnessLookup) *FreshnessService {
	return &FreshnessService{lookup: lookup}
}

// Weeks 返回窗口内每一周的发布状态、最近成功时间与是否过期，按周次升序。
// 无当前版本、该周无观测、从未成功抓取都按事实返回，不在这里报错。
func (s *FreshnessService) Weeks(ctx context.Context, now time.Time, params FreshnessParams) ([]WeekFreshness, error) {
	weeks := make([]WeekFreshness, 0)
	// 空窗口与非法区间没有可表达的周：返回空数组而不是错误，窗口本身由 pipeline 保证 1 ≤ from ≤ to。
	if params.FromWeek < 1 || params.ToWeek < params.FromWeek {
		return weeks, nil
	}
	if s.lookup == nil {
		return nil, fmt.Errorf("新鲜度查询未装配")
	}

	facts, err := s.lookup(ctx, params)
	if err != nil {
		return nil, err
	}

	// 存储层可以只回有事实的周，这里按区间补齐，调用方拿到的就是完整窗口。
	byWeek := make(map[int]WeekFacts, len(facts.Weeks))
	for _, fact := range facts.Weeks {
		byWeek[fact.Week] = fact
	}

	// 最近一轮失败对窗口内每一周都成立，与那一周自己的抓取时间无关（Requirement 17.3）。
	lastRunFailed := facts.LastRunStatus == runStatusFailed
	for week := params.FromWeek; week <= params.ToWeek; week++ {
		fact := byWeek[week]
		weeks = append(weeks, WeekFreshness{
			Week:          week,
			Published:     fact.Published,
			LastSuccessAt: utcOrNil(fact.LastSuccessAt),
			Stale:         weekStale(fact, lastRunFailed, now),
		})
	}
	return weeks, nil
}

// weekStale 按优先级判定一周是否过期：
//  1. 该周从未发布（当前版本没有它的观测）：不算过期，「没有数据」与「数据过期」是两件事，
//     前者只由 published=false 表达，标成过期会让前端把无数据误读成旧数据；
//  2. 最近一轮同步失败：过期，这是 Requirement 17.3 的第二个触发条件，「部分完成」不算失败；
//  3. 距最近一次成功抓取超过 StaleAfter：过期；恰好 8 天不算。
//
// 已发布却查不到任何成功抓取记录时按过期处理：没有任何证据能证明这份数据是新鲜的。
func weekStale(fact WeekFacts, lastRunFailed bool, now time.Time) bool {
	if !fact.Published {
		return false
	}
	if lastRunFailed {
		return true
	}
	if fact.LastSuccessAt == nil {
		return true
	}
	return now.Sub(*fact.LastSuccessAt) > StaleAfter
}

// utcOrNil 把时间统一成 UTC 再输出：存储层可能按会话时区返回时刻，契约要求 RFC3339 UTC（Requirement 27.1）。
func utcOrNil(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	utc := at.UTC()
	return &utc
}
