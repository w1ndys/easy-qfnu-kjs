package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openReleaseAdminTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 与其它集成测试一样会清空 public schema，只能指向测试库。
func openReleaseAdminTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过真实库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	// 从干净库开始，确保下面的种子都由本用例负责。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// 版本读取集成用例的固定标识与学期。版本 id 的形状与 publish.releaseIdentifier
// 一致（YYYYMMDDTHHMMSSZ），但仓储把它当不透明主键，不在这里再解析。
const (
	releaseAdminFixtureTerm        = "2026-2027-1"
	releaseAdminCurrentID          = "20261114T030512Z" // 新版本、current
	releaseAdminTieBreakID         = "20261114T030500Z" // 与新版本同 generated_at、release_id 更小，验证次键
	releaseAdminPreviousID         = "20261028T041500Z" // 旧版本、无关联轮次
	releaseAdminEmptyID            = "20260101T000000Z" // 没有任何观测与归属快照的版本
	releaseAdminCurrentGenAt       = "2026-11-14T03:05:12Z"
	releaseAdminPreviousGenAt      = "2026-10-28T04:15:00Z"
	releaseAdminCurrentPublish     = "2026-11-14T03:06:40Z" // 关联轮次的 max(released_at)
	releaseAdminDuplicatePublishAt = "2026-11-14T03:06:00Z" // 同一版本的更早一轮，验证 max
)

// releaseAdminObsSeed 是一条观测种子：nodes 里的每个小节都写一行。
// 同一 block 下的多个小节折叠成一个格，正是 week_block_cells 要断言的东西。
type releaseAdminObsSeed struct {
	releaseID string
	roomID    string
	week      int
	weekday   int
	nodes     []string
}

// releaseAdminFixtureObs 是两版夹具的全部观测。
//
// 手工折叠（分周）：
//   - current 第 9 周：r101 的 01..05 五行落进 0102 与 030405 两个格，
//     r102 的 01..02 两行落进 0102 一个格 → 观测 7 行、格 3 个；
//   - current 第 10 周：r101 的 01..02 两行 → 观测 2 行、格 1 个；
//   - previous 第 5 周：r101 的 01..05 五行 → 观测 5 行、格 2 个；
//   - previous 第 6 周：r103 的 06 一行 → 观测 1 行、格 1 个。
func releaseAdminFixtureObs() []releaseAdminObsSeed {
	return []releaseAdminObsSeed{
		{releaseAdminCurrentID, "r101", 9, 1, []string{"01", "02", "03", "04", "05"}},
		{releaseAdminCurrentID, "r102", 9, 1, []string{"01", "02"}},
		{releaseAdminCurrentID, "r101", 10, 2, []string{"01", "02"}},
		{releaseAdminPreviousID, "r101", 5, 1, []string{"01", "02", "03", "04", "05"}},
		{releaseAdminPreviousID, "r103", 6, 3, []string{"06"}},
	}
}

// seedReleaseAdminFixture 铺一套三版夹具：一个新版本（current，带两轮发布记录）、
// 一个与新版本同 generated_at 但 release_id 更小的并列版本（没有观测与归属快照，
// 也没有关联轮次）、一个旧版本（带归属冲突，没有关联轮次）。另外铺学期、三间房，
// 并给新版本与旧版本相同的归属快照和各自的观测。字典 v1 与轴 v1 直接用基线种子。
func seedReleaseAdminFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20)",
		releaseAdminFixtureTerm)
	// 字典 v1 与轴 v1 不在这里铺：0001 基线已经种了 dict v1 与「12 小节 → 5 大节」
	// 的 axis_node（01/02 → 0102，03–05 → 030405，06/07 → 0607，08/09 → 0809，
	// 10–12 → 101112），夹具直接复用，再插一遍会撞主键。

	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('r101', 'F101', 'F101', 'Q10', '综合教学楼'),
       ('r102', 'F102', 'F102', 'Q10', '综合教学楼'),
       ('r103', 'JA101', 'JA101', 'Q03', '老文史楼')`)

	// 三版：新版本 current；同 generated_at 但 id 更小的并列版本（无观测、无归属快照，
	// 放在最前面插入，让「只按 generated_at 排」与期望顺序不一致，次键才真的被测到）；
	// 旧版本非 current 且带归属冲突标记。
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $2, $3, 1, 1, false, false),
       ($4, $2, $3, 1, 1, false, true),
       ($5, $2, $6, 1, 1, true, false)`,
		releaseAdminTieBreakID, releaseAdminFixtureTerm,
		mustParseReleaseAdminTime(t, releaseAdminCurrentGenAt),
		releaseAdminCurrentID, releaseAdminPreviousID,
		mustParseReleaseAdminTime(t, releaseAdminPreviousGenAt))

	// 两版的归属快照相同：Q03 一楼、Q10 两楼，共 3 间房。
	for _, releaseID := range []string{releaseAdminCurrentID, releaseAdminPreviousID} {
		mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, 'r101', 'Q10', '综合教学楼'),
       ($1, 'r102', 'Q10', '综合教学楼'),
       ($1, 'r103', 'Q03', '老文史楼')`, releaseID)
	}

	for _, seed := range releaseAdminFixtureObs() {
		for _, node := range seed.nodes {
			mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, $2, $3, $4, $5, $6, 'free', true)`,
				seed.releaseID, seed.roomID, releaseAdminFixtureTerm, seed.week, seed.weekday, node)
		}
	}

	// 新版本关联两轮发布：max(released_at) 取较晚的那一轮，列表仍只有一行版本。
	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, status, term, release_id, released_at, window_start, window_end)
VALUES ($1, 'success', $2, $3, $4, 9, 12),
       ($1, 'success', $2, $3, $5, 9, 12)`,
		mustParseReleaseAdminTime(t, "2026-11-14T03:06:00Z"), releaseAdminFixtureTerm,
		releaseAdminCurrentID,
		mustParseReleaseAdminTime(t, releaseAdminCurrentPublish),
		mustParseReleaseAdminTime(t, releaseAdminDuplicatePublishAt))
}

// mustParseReleaseAdminTime 解析夹具里的 UTC 时刻，写错格式直接终止用例。
func mustParseReleaseAdminTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("解析夹具时刻 %q 失败: %v", value, err)
	}
	return parsed
}

// TestReleaseAdminRepoListAgainstRealDB 覆盖版本列表的真实库口径（Requirement 1.1–1.4）：
// 生成时间倒序、同刻按 release_id 倒序、released_at 取关联轮次的 max、三个计数与夹具
// 一致、空库为空切片。
func TestReleaseAdminRepoListAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseAdminFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	summaries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("读版本列表失败: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("夹具应有三个版本，实际 %d 个: %+v", len(summaries), summaries)
	}

	// 排序：generated_at 倒序；新版本与并列版本同刻，先后只能由 release_id DESC 次键
	// 决定（并列版本 id 更小，所以排在新版本之后），旧版本生成时间更早排最后。
	// 夹具把并列版本插在最前面，只按 generated_at 排时物理顺序与期望相反。
	wantOrder := []string{releaseAdminCurrentID, releaseAdminTieBreakID, releaseAdminPreviousID}
	for index, wantID := range wantOrder {
		if summaries[index].ReleaseID != wantID {
			t.Fatalf("第 %d 个版本应为 %s，实际 %s（完整列表 %+v）",
				index, wantID, summaries[index].ReleaseID, summaries)
		}
	}
	current, tie, previous := summaries[0], summaries[1], summaries[2]
	if !tie.GeneratedAt.Equal(current.GeneratedAt) {
		t.Fatalf("并列版本应与新版本同 generated_at: %s vs %s", tie.GeneratedAt, current.GeneratedAt)
	}

	if !current.GeneratedAt.Equal(mustParseReleaseAdminTime(t, releaseAdminCurrentGenAt)) {
		t.Fatalf("新版本 generated_at 不符: %s", current.GeneratedAt)
	}
	if current.Term != releaseAdminFixtureTerm || current.DictVersion != 1 || current.AxisVersion != 1 {
		t.Fatalf("新版本身份字段不符: %+v", current)
	}
	if !current.IsCurrent || current.HasAttrConflict {
		t.Fatalf("新版本应为 current 且无归属冲突: %+v", current)
	}
	if tie.IsCurrent || tie.HasAttrConflict {
		t.Fatalf("并列版本应为非 current 且无归属冲突: %+v", tie)
	}
	if previous.IsCurrent || !previous.HasAttrConflict {
		t.Fatalf("旧版本应为非 current 且有归属冲突: %+v", previous)
	}

	// released_at：新版本取关联两轮里的较晚时刻；另外两版没有关联轮次 → nil。
	if current.ReleasedAt == nil {
		t.Fatal("新版本 released_at 不应为 nil")
	}
	if want := mustParseReleaseAdminTime(t, releaseAdminCurrentPublish); !current.ReleasedAt.Equal(want) {
		t.Fatalf("released_at 应取 max：got %s want %s", current.ReleasedAt, want)
	}
	if tie.ReleasedAt != nil {
		t.Fatalf("没有关联轮次的并列版本 released_at 应为 nil，实际 %v", tie.ReleasedAt)
	}
	if previous.ReleasedAt != nil {
		t.Fatalf("没有关联轮次的版本 released_at 应为 nil，实际 %v", previous.ReleasedAt)
	}

	// 三个计数与夹具一致：新版本 2 周 / 9 行 / 3 房，旧版本 2 周 / 6 行 / 3 房；
	// 并列版本没有观测与归属快照，计数走 COALESCE 的 0。
	if current.WeekCount != 2 || current.ObservationRows != 9 || current.Rooms != 3 {
		t.Fatalf("新版本计数不符（含手工折叠）: %+v", current)
	}
	if previous.WeekCount != 2 || previous.ObservationRows != 6 || previous.Rooms != 3 {
		t.Fatalf("旧版本计数不符（含手工折叠）: %+v", previous)
	}
	if tie.WeekCount != 0 || tie.ObservationRows != 0 || tie.Rooms != 0 {
		t.Fatalf("并列版本三个计数都应为 0: %+v", tie)
	}

	// 同一数据再查一次，顺序与内容完全一致（CP1）；同刻两版的相对顺序尤其不能漂。
	again, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("第二次读版本列表失败: %v", err)
	}
	if len(again) != len(summaries) {
		t.Fatalf("两次列表长度不一致: %d vs %d", len(again), len(summaries))
	}
	for index := range summaries {
		if again[index].ReleaseID != summaries[index].ReleaseID {
			t.Fatalf("同一数据的版本顺序不应变化: %s vs %s",
				again[index].ReleaseID, summaries[index].ReleaseID)
		}
	}

	// 删掉全部版本后列表为空：空切片而不是 nil（Requirement 1.4，total 由 len 得 0）。
	mustExec(ctx, t, pool, "DELETE FROM release")
	empty, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("空库读版本列表失败: %v", err)
	}
	if empty == nil {
		t.Fatal("空库应返回空切片而不是 nil")
	}
	if len(empty) != 0 {
		t.Fatalf("删掉全部版本后列表应为空，实际 %+v", empty)
	}
}

// TestReleaseAdminRepoDetailAgainstRealDB 覆盖版本详情的真实库口径（Requirement 2.1–2.4）：
// stats 由逐周结果拼出、week_counts 与手工折叠一致且升序、building_counts 升序、
// 不存在返回 (零值, false, nil)。
func TestReleaseAdminRepoDetailAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseAdminFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	detail, found, err := repo.Detail(ctx, releaseAdminCurrentID)
	if err != nil {
		t.Fatalf("读新版本详情失败: %v", err)
	}
	if !found {
		t.Fatal("夹具中的新版本应当找得到")
	}

	// 版本头的身份与 released_at 与列表同源。
	if detail.Release.ReleaseID != releaseAdminCurrentID ||
		detail.Release.Term != releaseAdminFixtureTerm ||
		detail.Release.DictVersion != 1 || detail.Release.AxisVersion != 1 ||
		!detail.Release.IsCurrent || detail.Release.HasAttrConflict {
		t.Fatalf("详情版本头不符: %+v", detail.Release)
	}
	if detail.Release.ReleasedAt == nil ||
		!detail.Release.ReleasedAt.Equal(mustParseReleaseAdminTime(t, releaseAdminCurrentPublish)) {
		t.Fatalf("详情 released_at 不符: %v", detail.Release.ReleasedAt)
	}

	// stats：观测 9 行、格 4 个、2 周、3 房、2 楼；全是拼出来的，不额外查库。
	wantStats := ReleaseStats{ObservationRows: 9, BlockCells: 4, WeekCount: 2, Rooms: 3, Buildings: 2}
	if detail.Stats != wantStats {
		t.Fatalf("stats 不符: got %+v want %+v", detail.Stats, wantStats)
	}
	// 版本头的两个计数由逐周结果回填，与 stats 同值。
	if detail.Release.ObservationRows != 9 || detail.Release.WeekCount != 2 {
		t.Fatalf("版本头计数未由逐周结果拼出: %+v", detail.Release)
	}

	// 逐周计数：升序，且与夹具的手工折叠一致（第 9 周 7 行/3 格，第 10 周 2 行/1 格）。
	wantWeeks := []ReleaseWeekCount{
		{Week: 9, ObservationRows: 7, BlockCells: 3},
		{Week: 10, ObservationRows: 2, BlockCells: 1},
	}
	if len(detail.WeekCounts) != len(wantWeeks) {
		t.Fatalf("逐周计数条数不符: %+v", detail.WeekCounts)
	}
	for index, want := range wantWeeks {
		if detail.WeekCounts[index] != want {
			t.Fatalf("第 %d 个逐周计数不符: got %+v want %+v", index, detail.WeekCounts[index], want)
		}
	}

	// 按楼计数：building_id 升序（Q03 → Q10）。
	wantBuildings := []ReleaseBuildingCount{
		{BuildingID: "Q03", BuildingName: "老文史楼", Rooms: 1},
		{BuildingID: "Q10", BuildingName: "综合教学楼", Rooms: 2},
	}
	if len(detail.BuildingCounts) != len(wantBuildings) {
		t.Fatalf("按楼计数条数不符: %+v", detail.BuildingCounts)
	}
	for index, want := range wantBuildings {
		if detail.BuildingCounts[index] != want {
			t.Fatalf("第 %d 个按楼计数不符: got %+v want %+v",
				index, detail.BuildingCounts[index], want)
		}
	}

	// 旧版本：内容不同（第 5 周 5 行/2 格，第 6 周 1 行/1 格），且没有关联轮次。
	previous, found, err := repo.Detail(ctx, releaseAdminPreviousID)
	if err != nil {
		t.Fatalf("读旧版本详情失败: %v", err)
	}
	if !found {
		t.Fatal("夹具中的旧版本应当找得到")
	}
	if previous.Release.ReleasedAt != nil {
		t.Fatalf("旧版本没有关联轮次，released_at 应为 nil: %v", previous.Release.ReleasedAt)
	}
	wantPreviousStats := ReleaseStats{ObservationRows: 6, BlockCells: 3, WeekCount: 2, Rooms: 3, Buildings: 2}
	if previous.Stats != wantPreviousStats {
		t.Fatalf("旧版本 stats 不符: got %+v want %+v", previous.Stats, wantPreviousStats)
	}
	wantPreviousWeeks := []ReleaseWeekCount{
		{Week: 5, ObservationRows: 5, BlockCells: 2},
		{Week: 6, ObservationRows: 1, BlockCells: 1},
	}
	for index, want := range wantPreviousWeeks {
		if previous.WeekCounts[index] != want {
			t.Fatalf("旧版本第 %d 个逐周计数不符: got %+v want %+v",
				index, previous.WeekCounts[index], want)
		}
	}

	// 不存在的版本：(零值, false, nil)，不是错误（handler 据此映射 404 / 40401）。
	missing, found, err := repo.Detail(ctx, "20250101T000000Z")
	if err != nil {
		t.Fatalf("版本不存在不是错误，实际 err=%v", err)
	}
	if found {
		t.Fatal("不存在的版本 found 应为 false")
	}
	if missing.Release.ReleaseID != "" || missing.Stats != (ReleaseStats{}) {
		t.Fatalf("版本不存在应返回零值: %+v", missing)
	}

	// 空版本（有 release 行、没有观测与归属快照）：切片恒非 nil，stats 全零。
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, 1, 1, false)`,
		releaseAdminEmptyID, releaseAdminFixtureTerm,
		mustParseReleaseAdminTime(t, "2026-01-01T00:00:00Z"))
	empty, found, err := repo.Detail(ctx, releaseAdminEmptyID)
	if err != nil {
		t.Fatalf("读空版本详情失败: %v", err)
	}
	if !found {
		t.Fatal("空版本仍然存在，found 应为 true")
	}
	if empty.WeekCounts == nil || empty.BuildingCounts == nil {
		t.Fatalf("两个切片必须非 nil（JSON 输出 []）: weeks=%v buildings=%v",
			empty.WeekCounts, empty.BuildingCounts)
	}
	if len(empty.WeekCounts) != 0 || len(empty.BuildingCounts) != 0 {
		t.Fatalf("空版本不应有计数: %+v", empty)
	}
	if empty.Stats != (ReleaseStats{}) || empty.Release.ObservationRows != 0 || empty.Release.WeekCount != 0 {
		t.Fatalf("空版本 stats 应全零: %+v", empty)
	}
}
