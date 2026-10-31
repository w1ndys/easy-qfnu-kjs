package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// FreshnessRange 是逐周新鲜度查询的入参：学期 + 周次闭区间。
type FreshnessRange struct {
	Term     string // 学期
	FromWeek int    // 起始周（含）
	ToWeek   int    // 结束周（含）
}

// WeekFetch 是某一周的抓取与发布事实。
type WeekFetch struct {
	Week          int        // 教学周次
	Published     bool       // 该周在当前发布版本里是否有观测
	LastSuccessAt *time.Time // 该周矩阵最近一次成功抓取时间；从未成功为 nil
}

// FreshnessFacts 是一段周区间的抓取事实与最近一轮同步状态。
type FreshnessFacts struct {
	Weeks         []WeekFetch // 按周次升序；区间内没有事实的周不出现
	LastRunStatus string      // 最近一轮同步的原始状态；一条轮次都没有时为空串
}

// freshnessQuerier 只取新鲜度查询需要的两种能力，便于与驱动其余部分解耦。
type freshnessQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// freshnessPublishedSQL 取当前发布版本里已有观测的周；历史版本即使留着该周也不算已发布。
const freshnessPublishedSQL = `
SELECT DISTINCT o.week
  FROM observation o
  JOIN release rel
    ON rel.release_id = o.release_id
   AND rel.is_current
   AND rel.term = $1
 WHERE o.term = $1
   AND o.week BETWEEN $2 AND $3
 ORDER BY o.week`

// freshnessLastSuccessSQL 先按 (轮次, 周) 汇总该周各楼的结果，只有全部楼都 ok 才算「成功抓取该周矩阵」，
// 再对每个周取最近一次。任一楼失败的周不前移时间，避免用半份矩阵冒充成功。
// 这里不按轮次状态过滤：Requirement 17.3 说的是「抓取」，最近一轮失败另有 last_run 状态单独判过期。
const freshnessLastSuccessSQL = `
WITH week_run AS (
  SELECT run_id, week, bool_and(ok) AS all_ok, max(fetched_at) AS fetched_at
    FROM collect_run_week
   WHERE term = $1
     AND week BETWEEN $2 AND $3
   GROUP BY run_id, week
)
SELECT week, max(fetched_at)
  FROM week_run
 WHERE all_ok
   AND fetched_at IS NOT NULL
 GROUP BY week
 ORDER BY week`

// freshnessLastRunSQL 取最近一轮同步的原始状态；轮次可能因登录失败而没有 term，所以不按学期过滤。
const freshnessLastRunSQL = `
SELECT status
  FROM collect_run
 ORDER BY started_at DESC, run_id DESC
 LIMIT 1`

// Freshness 读取窗口内各周的发布与抓取事实，以及最近一轮同步的状态。
// 区间内没有任何事实的周不会出现在结果里，由查询层补成「未发布、从未成功」。
func Freshness(ctx context.Context, q freshnessQuerier, r FreshnessRange) (FreshnessFacts, error) {
	facts := FreshnessFacts{Weeks: make([]WeekFetch, 0)}
	// 非法区间没有可查的周：直接返回空事实，不把 1..0 这类区间交给 SQL。
	if r.FromWeek < 1 || r.ToWeek < r.FromWeek {
		return facts, nil
	}

	published, err := freshnessPublishedWeeks(ctx, q, r)
	if err != nil {
		return FreshnessFacts{}, err
	}
	lastSuccess, err := freshnessLastSuccessWeeks(ctx, q, r)
	if err != nil {
		return FreshnessFacts{}, err
	}

	// 两个来源按周次合并：只在一个来源出现的周也要出现在结果里。
	byWeek := make(map[int]*WeekFetch, len(published)+len(lastSuccess))
	touch := func(week int) *WeekFetch {
		fetch, ok := byWeek[week]
		if !ok {
			fetch = &WeekFetch{Week: week}
			byWeek[week] = fetch
		}
		return fetch
	}
	for _, week := range published {
		touch(week).Published = true
	}
	for week, at := range lastSuccess {
		fetchedAt := at
		touch(week).LastSuccessAt = &fetchedAt
	}

	// 顺序稳定：按周次升序输出，调用方不必再排一次。
	weeks := make([]int, 0, len(byWeek))
	for week := range byWeek {
		weeks = append(weeks, week)
	}
	sort.Ints(weeks)
	for _, week := range weeks {
		facts.Weeks = append(facts.Weeks, *byWeek[week])
	}

	status, err := freshnessLastRunStatus(ctx, q)
	if err != nil {
		return FreshnessFacts{}, err
	}
	facts.LastRunStatus = status
	return facts, nil
}

// freshnessPublishedWeeks 读当前发布版本里已有观测的周。
func freshnessPublishedWeeks(ctx context.Context, q freshnessQuerier, r FreshnessRange) ([]int, error) {
	rows, err := q.Query(ctx, freshnessPublishedSQL, r.Term, r.FromWeek, r.ToWeek)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询已发布周次失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	weeks := make([]int, 0)
	for rows.Next() {
		var week int16
		if err := rows.Scan(&week); err != nil {
			return nil, fmt.Errorf("%w: 扫描已发布周次失败: %w", model.ErrQueryFailed, err)
		}
		weeks = append(weeks, int(week))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历已发布周次失败: %w", model.ErrQueryFailed, err)
	}
	return weeks, nil
}

// freshnessLastSuccessWeeks 读每个周最近一次「全部楼都成功」的抓取时间。
func freshnessLastSuccessWeeks(ctx context.Context, q freshnessQuerier, r FreshnessRange) (map[int]time.Time, error) {
	rows, err := q.Query(ctx, freshnessLastSuccessSQL, r.Term, r.FromWeek, r.ToWeek)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询周矩阵抓取时间失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	lastSuccess := make(map[int]time.Time)
	for rows.Next() {
		var week int16
		var at time.Time
		if err := rows.Scan(&week, &at); err != nil {
			return nil, fmt.Errorf("%w: 扫描周矩阵抓取时间失败: %w", model.ErrQueryFailed, err)
		}
		lastSuccess[int(week)] = at
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历周矩阵抓取时间失败: %w", model.ErrQueryFailed, err)
	}
	return lastSuccess, nil
}

// freshnessLastRunStatus 读最近一轮同步的原始状态；还没跑过任何一轮时返回空串。
func freshnessLastRunStatus(ctx context.Context, q freshnessQuerier) (string, error) {
	var status string
	err := q.QueryRow(ctx, freshnessLastRunSQL).Scan(&status)
	// 一条轮次都没有属于正常情形：空库或刚清过数据，不算错误。
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: 查询最近一轮同步状态失败: %w", model.ErrQueryFailed, err)
	}
	return status, nil
}
