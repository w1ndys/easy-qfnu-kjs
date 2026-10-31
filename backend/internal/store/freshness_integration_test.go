package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freshnessFixtureTerm 是新鲜度夹具使用的学期。
const freshnessFixtureTerm = "2026-2027-1"

// freshnessAt 解析 RFC3339 时刻，失败直接终止用例。
func freshnessAt(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("解析时刻 %s 失败: %v", s, err)
	}
	return at
}

// seedFreshnessData 造两个发布版本、两轮同步与逐楼逐周的抓取结果：
//   - 当前版本只在第 5、7 周有观测；历史版本里还有第 8 周，用来确认 published 只认 is_current；
//   - 第 5 周两轮都成功，取最近一次；
//   - 第 6 周在最近一轮有一栋楼失败，时间不前移；
//   - 第 7 周在最近一轮失败，时间同样不前移；
//   - 第 8 周抓取成功但从未发布。
func seedFreshnessData(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", freshnessFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ('fresh-release-cur', $1, now(), 1, 1, true),
       ('fresh-release-old', $1, now() - interval '7 days', 1, 1, false)`, freshnessFixtureTerm)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('fr101', 'F101', 'F101', 'Q10', '综合教学楼')`)

	// 一格观测就足以让该周算「已发布」，不必铺满 35 个格子。
	mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ('fresh-release-cur', 'fr101', $1, 5, 1, '01', 'free', true),
       ('fresh-release-cur', 'fr101', $1, 7, 1, '01', 'class', false),
       ('fresh-release-old', 'fr101', $1, 8, 1, '01', 'free', true)`, freshnessFixtureTerm)

	// 早的一轮成功，晚的一轮部分完成；两轮的 started_at 决定「最近一轮」。
	var olderRun, newerRun int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term) VALUES ($1, 'success', $2) RETURNING run_id`,
		freshnessAt(t, "2026-10-20T01:00:00Z"), freshnessFixtureTerm).Scan(&olderRun); err != nil {
		t.Fatalf("造早轮次失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term) VALUES ($1, 'partial', $2) RETURNING run_id`,
		freshnessAt(t, "2026-10-27T01:00:00Z"), freshnessFixtureTerm).Scan(&newerRun); err != nil {
		t.Fatalf("造晚轮次失败: %v", err)
	}

	fetches := []struct {
		runID     int64
		week      int
		building  string
		ok        bool
		fetchedAt any // nil 表示该楼失败、没有抓到页面
	}{
		{runID: olderRun, week: 5, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-20T02:00:00Z")},
		{runID: olderRun, week: 6, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-20T02:05:00Z")},
		{runID: olderRun, week: 7, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-20T02:10:00Z")},
		{runID: olderRun, week: 8, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-20T02:15:00Z")},
		{runID: newerRun, week: 5, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-27T02:00:00Z")},
		{runID: newerRun, week: 5, building: "Q11", ok: true, fetchedAt: freshnessAt(t, "2026-10-27T02:01:00Z")},
		{runID: newerRun, week: 6, building: "Q10", ok: true, fetchedAt: freshnessAt(t, "2026-10-27T02:05:00Z")},
		{runID: newerRun, week: 6, building: "Q11", ok: false, fetchedAt: nil},
		{runID: newerRun, week: 7, building: "Q10", ok: false, fetchedAt: nil},
	}
	for _, f := range fetches {
		mustExec(ctx, t, pool, `
INSERT INTO collect_run_week (run_id, term, week, building_id, ok, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6)`, f.runID, freshnessFixtureTerm, f.week, f.building, f.ok, f.fetchedAt)
	}
}

// TestFreshnessWeekFacts 覆盖逐周事实：发布状态只认当前版本，抓取时间只认「全部楼都成功」的最近一次。
func TestFreshnessWeekFacts(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedFreshnessData(ctx, t, pool)

	facts, err := Freshness(ctx, pool, FreshnessRange{Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 8})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	// 只回有事实的周，且按周次升序。
	wantWeeks := []int{5, 6, 7, 8}
	if len(facts.Weeks) != len(wantWeeks) {
		t.Fatalf("期望 %d 周，实际 %d: %+v", len(wantWeeks), len(facts.Weeks), facts.Weeks)
	}
	for i, want := range wantWeeks {
		if facts.Weeks[i].Week != want {
			t.Fatalf("第 %d 个周次期望 %d，实际 %d", i, want, facts.Weeks[i].Week)
		}
	}

	cases := []struct {
		name      string
		published bool
		lastAt    string // 空串表示从未成功抓取
	}{
		{name: "第 5 周两轮都成功取最近一次", published: true, lastAt: "2026-10-27T02:01:00Z"},
		{name: "第 6 周最近一轮有一栋楼失败不前移", published: false, lastAt: "2026-10-20T02:05:00Z"},
		{name: "第 7 周最近一轮失败不前移", published: true, lastAt: "2026-10-20T02:10:00Z"},
		{name: "第 8 周只在历史版本里且抓取成功", published: false, lastAt: "2026-10-20T02:15:00Z"},
	}
	for i, c := range cases {
		week := facts.Weeks[i]
		if week.Published != c.published {
			t.Fatalf("%s：published 期望 %v，实际 %v", c.name, c.published, week.Published)
		}
		if c.lastAt == "" {
			if week.LastSuccessAt != nil {
				t.Fatalf("%s：期望从未成功，实际 %v", c.name, week.LastSuccessAt)
			}
			continue
		}
		if week.LastSuccessAt == nil {
			t.Fatalf("%s：期望抓取时间 %s，实际 nil", c.name, c.lastAt)
		}
		if !week.LastSuccessAt.Equal(freshnessAt(t, c.lastAt)) {
			t.Fatalf("%s：抓取时间期望 %s，实际 %s", c.name, c.lastAt, week.LastSuccessAt.UTC().Format(time.RFC3339))
		}
	}

	// 最近一轮是 partial：原始状态回给查询层，由它判断「不算失败」。
	if facts.LastRunStatus != "partial" {
		t.Fatalf("最近一轮状态期望 partial，实际 %q", facts.LastRunStatus)
	}
}

// TestFreshnessLatestRunFailed 覆盖最近一轮失败：状态按 started_at 取最新，不按插入顺序。
func TestFreshnessLatestRunFailed(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedFreshnessData(ctx, t, pool)

	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, status, term) VALUES ($1, 'failed', $2)`,
		freshnessAt(t, "2026-10-31T01:00:00Z"), freshnessFixtureTerm)

	facts, err := Freshness(ctx, pool, FreshnessRange{Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 5})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if facts.LastRunStatus != "failed" {
		t.Fatalf("最近一轮状态期望 failed，实际 %q", facts.LastRunStatus)
	}
}

// TestFreshnessNoRunsAndEmptyRange 覆盖没有轮次与非法区间：两者都不算错误。
func TestFreshnessNoRunsAndEmptyRange(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)

	facts, err := Freshness(ctx, pool, FreshnessRange{Term: freshnessFixtureTerm, FromWeek: 5, ToWeek: 8})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if facts.Weeks == nil {
		t.Fatal("空结果应是空数组而不是 nil")
	}
	if len(facts.Weeks) != 0 {
		t.Fatalf("空库应没有周事实，实际 %+v", facts.Weeks)
	}
	if facts.LastRunStatus != "" {
		t.Fatalf("没有轮次时状态期望空串，实际 %q", facts.LastRunStatus)
	}

	// 区间非法时直接返回空事实，不把 9..8 交给 SQL。
	facts, err = Freshness(ctx, pool, FreshnessRange{Term: freshnessFixtureTerm, FromWeek: 9, ToWeek: 8})
	if err != nil {
		t.Fatalf("非法区间不应报错，实际 %v", err)
	}
	if len(facts.Weeks) != 0 {
		t.Fatalf("非法区间应没有周事实，实际 %+v", facts.Weeks)
	}
}
