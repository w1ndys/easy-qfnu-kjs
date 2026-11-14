package store

import (
	"context"
	"errors"
	"os"
	"strings"
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

// 对比汇总集成用例的标识与学期。版本 id 形状与 publish 的真实版本一致，但仓储把
// 它当不透明主键，不在这里解析。
const (
	releaseDiffTerm      = "2026-2027-1"
	releaseDiffOtherTerm = "2025-2026-2"
	releaseDiffFromID    = "20261001T000000Z" // 对比基准（旧版本）
	releaseDiffToID      = "20261015T030512Z" // 对比目标（新版本，current）
	releaseDiffMissingID = "20990101T000000Z" // 库里不存在的版本
)

// releaseDiffCellSeed 是一条观测种子：nodes 里的每个小节各写一行，共用同一状态。
//
// 同一个 (房号, 周, 星期, 大节) 可以有多条种子（见下面 r205 的 to 侧），它们在库里
// 折叠成一个格——那正是「块内不一致」夹具的造法，不必写一条专门的 SQL。
type releaseDiffCellSeed struct {
	releaseID string
	roomID    string
	week      int
	weekday   int
	nodes     []string
	stateKey  string
}

// releaseDiffRoomSeed 是归属快照的一行：房号与它在该版本的楼。
type releaseDiffRoomSeed struct {
	roomID       string
	buildingID   string
	buildingName string
}

// releaseDiffFixtureCells 是两版夹具的全部观测，覆盖五类差异与两侧各一个块内不一致的格。
//
// 手工折叠（按 (房号, 周, 星期, 大节)）：
//   - r201 第 9 周 0102：两侧 free → 未变格；
//   - r202 第 9 周 0102：只在 from → 消失格；
//   - r203 第 9 周 0102：from free、to class → 状态变化；
//   - r204 第 9 周 0102：两侧 free → 未变格（这间房换了楼，属归属变化，不是格变化）；
//   - r205 第 9 周 0607：from 06/07 都 free、to 06 free + 07 class → 状态变化，
//     且 to 侧块内出现两个 state_key（min 取 class）→ 计入 inconsistent_blocks；
//   - r206 第 9 周 0102：只在 from → 消失格；
//   - r207 第 9 周 0809：from unknown、to free → 状态变化，unknown 由 1 变 0；
//   - r208 第 9 周 0809：只在 to，状态 composite → 新增格，composite 由 0 变 1；
//   - r209 第 9 周 0102：两侧 free → 未变格；
//   - r209 第 10 周 0102：from class、to free → 状态变化（周在键里，第 9 周不受影响）；
//   - r201 第 11 周 0102：from 01 free + 02 class、to 01/02 都 class → 两侧折叠后的
//     state_key 都是 class（未变格），只有 from 侧块内不一致。这一格钉住
//     inconsistent 取自「from 或 to」两侧的并集 OR，而不是只看 to 侧。
//
// 于是并集 11 个格：added 1 / removed 2 / changed 4 / same 4，unknown 1→0，
// composite 0→1，inconsistent_blocks 2；cells.total 是前三类之和 7，不含 same。
func releaseDiffFixtureCells() []releaseDiffCellSeed {
	return []releaseDiffCellSeed{
		// from 侧
		{releaseDiffFromID, "r201", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r202", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r203", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r204", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r205", 9, 1, []string{"06", "07"}, "free"},
		{releaseDiffFromID, "r206", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r207", 9, 1, []string{"08", "09"}, "unknown"},
		{releaseDiffFromID, "r209", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffFromID, "r209", 10, 1, []string{"01", "02"}, "class"},
		// 第 11 周这一格的 from 侧两行状态不同：折叠后 state_key = min = class，
		// max = free ≠ min，于是这一格计入 inconsistent_blocks（to 侧折成同一个
		// class，kind 是 same——不一致与「状态是否变化」是两件事）。
		{releaseDiffFromID, "r201", 11, 1, []string{"01"}, "free"},
		{releaseDiffFromID, "r201", 11, 1, []string{"02"}, "class"},

		// to 侧
		{releaseDiffToID, "r201", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffToID, "r203", 9, 1, []string{"01", "02"}, "class"},
		{releaseDiffToID, "r204", 9, 1, []string{"01", "02"}, "free"},
		// 同一个格的两行状态不同：折叠后 state_key = min('class','free') = class，
		// max = free ≠ min，这一格计入 inconsistent_blocks。
		{releaseDiffToID, "r205", 9, 1, []string{"06"}, "free"},
		{releaseDiffToID, "r205", 9, 1, []string{"07"}, "class"},
		{releaseDiffToID, "r207", 9, 1, []string{"08", "09"}, "free"},
		{releaseDiffToID, "r208", 9, 1, []string{"08", "09"}, "composite"},
		{releaseDiffToID, "r209", 9, 1, []string{"01", "02"}, "free"},
		{releaseDiffToID, "r209", 10, 1, []string{"01", "02"}, "free"},
		// 与 from 侧 r201 第 11 周那一格折叠后同为 class，to 侧本身一致（min = max）。
		{releaseDiffToID, "r201", 11, 1, []string{"01", "02"}, "class"},
	}
}

// releaseDiffFixtureRooms 是两版归属快照，覆盖房号新增 / 消失 / 保留 / 换楼：
//   - from：r201 r202 r203 r204 r205 r206 r207 r209（8 间）；
//   - to：  r201 r203 r204 r205 r207 r208 r209（7 间，r204 从 Q10 换到 Q03）。
//
// 于是 rooms = added 1（r208）/ removed 2（r202、r206）/ shared 6 / building_changed 1。
func releaseDiffFixtureRooms() map[string][]releaseDiffRoomSeed {
	return map[string][]releaseDiffRoomSeed{
		releaseDiffFromID: {
			{"r201", "Q10", "综合教学楼"},
			{"r202", "Q10", "综合教学楼"},
			{"r203", "Q10", "综合教学楼"},
			{"r204", "Q10", "综合教学楼"},
			{"r205", "Q10", "综合教学楼"},
			{"r206", "Q03", "老文史楼"},
			{"r207", "Q03", "老文史楼"},
			{"r209", "Q10", "综合教学楼"},
		},
		releaseDiffToID: {
			{"r201", "Q10", "综合教学楼"},
			{"r203", "Q10", "综合教学楼"},
			{"r204", "Q03", "老文史楼"}, // 归属变化
			{"r205", "Q10", "综合教学楼"},
			{"r207", "Q03", "老文史楼"},
			{"r208", "Q10", "综合教学楼"}, // 新增房号
			{"r209", "Q10", "综合教学楼"},
		},
	}
}

// seedReleaseDiffFixture 铺对比夹具：学期、教室目录、两个版本（to 为 current）与
// 各自的归属快照和观测。字典 v1 与轴 v1 直接用基线种子。
func seedReleaseDiffFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, 20)", releaseDiffTerm)

	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('r201', 'F201', 'F201', 'Q10', '综合教学楼'),
       ('r202', 'F202', 'F202', 'Q10', '综合教学楼'),
       ('r203', 'F203', 'F203', 'Q10', '综合教学楼'),
       ('r204', 'F204', 'F204', 'Q10', '综合教学楼'),
       ('r205', 'F205', 'F205', 'Q10', '综合教学楼'),
       ('r206', 'JA101', 'JA101', 'Q03', '老文史楼'),
       ('r207', 'JA102', 'JA102', 'Q03', '老文史楼'),
       ('r208', 'F208', 'F208', 'Q10', '综合教学楼'),
       ('r209', 'F209', 'F209', 'Q10', '综合教学楼')`)

	// 两个版本：from 带归属冲突标记且非 current；to 为 current。
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $2, $3, 1, 1, true, false),
       ($4, $2, $5, 1, 1, false, true)`,
		releaseDiffFromID, releaseDiffTerm,
		mustParseReleaseAdminTime(t, "2026-10-01T00:00:00Z"),
		releaseDiffToID, mustParseReleaseAdminTime(t, "2026-10-15T03:05:12Z"))

	for releaseID, rooms := range releaseDiffFixtureRooms() {
		for _, room := range rooms {
			mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, $2, $3, $4)`,
				releaseID, room.roomID, room.buildingID, room.buildingName)
		}
	}

	for _, seed := range releaseDiffFixtureCells() {
		for _, node := range seed.nodes {
			mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				seed.releaseID, seed.roomID, releaseDiffTerm, seed.week, seed.weekday,
				node, seed.stateKey, seed.stateKey == "free")
		}
	}
}

// TestReleaseAdminRepoDiffAgainstRealDB 覆盖对比汇总的真实库口径（Requirement 3.1–3.4）：
// 五类差异逐项断言计数、折叠 CTE 的并集逐格计数（含 same）、两侧各一个块内不一致的
// 格都计一次且取 min、交换 from/to 断言 CP5、同一请求两次结果一致、
// 缺一版返回 ErrReleaseNotFound。
func TestReleaseAdminRepoDiffAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	diff, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffFromID, To: releaseDiffToID})
	if err != nil {
		t.Fatalf("对比两版失败: %v", err)
	}

	// 两侧版本头：身份、折叠口径与状态标记都来自 release 行。
	if diff.From.ReleaseID != releaseDiffFromID || diff.From.Term != releaseDiffTerm ||
		diff.From.DictVersion != 1 || diff.From.AxisVersion != 1 ||
		diff.From.IsCurrent || !diff.From.HasAttrConflict {
		t.Fatalf("from 侧版本头不符: %+v", diff.From)
	}
	if diff.To.ReleaseID != releaseDiffToID || diff.To.Term != releaseDiffTerm ||
		diff.To.DictVersion != 1 || diff.To.AxisVersion != 1 ||
		!diff.To.IsCurrent || diff.To.HasAttrConflict {
		t.Fatalf("to 侧版本头不符: %+v", diff.To)
	}

	want := ReleaseDiffSummary{
		Cells:              DiffCellsSummary{Added: 1, Removed: 2, Changed: 4, Total: 7},
		Rooms:              DiffRoomsSummary{Added: 1, Removed: 2, Shared: 6, BuildingChanged: 1},
		Unknown:            StateDelta{From: 1, To: 0, Delta: -1},
		Composite:          StateDelta{From: 0, To: 1, Delta: 1},
		InconsistentBlocks: 2,
		SameTerm:           true,
	}
	if diff.Summary != want {
		t.Fatalf("汇总不符:\n got %+v\nwant %+v", diff.Summary, want)
	}

	// 折叠 CTE 的逐格计数：并集 11 行，四种 kind 各数一遍。'same' 必须真的被数到
	// （4 个），否则「未变格」只写在注释里、没进断言；inconsistent 数 2，钉住它取
	// from / to 两侧的并集 OR，而不是只看 to 侧。
	var foldedCounts struct {
		total, same, added, removed, changed, inconsistent int
	}
	err = pool.QueryRow(ctx, releaseDiffFoldCTE+`
SELECT count(*),
       count(*) FILTER (WHERE kind = 'same'),
       count(*) FILTER (WHERE kind = 'added'),
       count(*) FILTER (WHERE kind = 'removed'),
       count(*) FILTER (WHERE kind = 'changed'),
       count(*) FILTER (WHERE inconsistent)
  FROM j`,
		1, releaseDiffFromID, 1, releaseDiffToID).
		Scan(&foldedCounts.total, &foldedCounts.same, &foldedCounts.added,
			&foldedCounts.removed, &foldedCounts.changed, &foldedCounts.inconsistent)
	if err != nil {
		t.Fatalf("数折叠后的格失败: %v", err)
	}
	wantFolded := struct {
		total, same, added, removed, changed, inconsistent int
	}{11, 4, 1, 2, 4, 2}
	if foldedCounts != wantFolded {
		t.Fatalf("折叠后的逐格计数不符:\n got %+v\nwant %+v", foldedCounts, wantFolded)
	}

	// to 侧块内不一致的格 r205/第 9 周/0607：折叠后取 min(state_key)=class，max 仍是
	// free —— 这条断言直接跑生产用的折叠 CTE，锁住「min 决定格状态、max 用来标记异常」
	// 的口径。
	var foldedTo struct {
		kind         string
		inconsistent bool
		stateKey     string
		maxState     string
	}
	err = pool.QueryRow(ctx, releaseDiffFoldCTE+`
SELECT j.kind, j.inconsistent, t.state_key, t.max_state
  FROM j
  JOIN t USING (room_id, week, weekday, block)
 WHERE j.room_id = $5 AND j.week = 9 AND j.weekday = 1 AND j.block = '0607'`,
		1, releaseDiffFromID, 1, releaseDiffToID, "r205").
		Scan(&foldedTo.kind, &foldedTo.inconsistent, &foldedTo.stateKey, &foldedTo.maxState)
	if err != nil {
		t.Fatalf("读折叠后的 to 侧格失败: %v", err)
	}
	if foldedTo.kind != "changed" || !foldedTo.inconsistent ||
		foldedTo.stateKey != "class" || foldedTo.maxState != "free" {
		t.Fatalf("to 侧块内不一致的格应为 changed、inconsistent、min=class、max=free，实际 %+v", foldedTo)
	}

	// from 侧块内不一致的格 r201/第 11 周/0102：to 侧一致，两侧折叠后同为 class，所以
	// kind 是 same 但 inconsistent 仍为 true——不一致只由 from 侧的 max ≠ min 触发，
	// 这就是并集 OR 的左半边。
	var foldedFrom struct {
		kind         string
		inconsistent bool
		stateKey     string
		maxState     string
	}
	err = pool.QueryRow(ctx, releaseDiffFoldCTE+`
SELECT j.kind, j.inconsistent, f.state_key, f.max_state
  FROM j
  JOIN f USING (room_id, week, weekday, block)
 WHERE j.room_id = $5 AND j.week = 11 AND j.weekday = 1 AND j.block = '0102'`,
		1, releaseDiffFromID, 1, releaseDiffToID, "r201").
		Scan(&foldedFrom.kind, &foldedFrom.inconsistent, &foldedFrom.stateKey, &foldedFrom.maxState)
	if err != nil {
		t.Fatalf("读折叠后的 from 侧格失败: %v", err)
	}
	if foldedFrom.kind != "same" || !foldedFrom.inconsistent ||
		foldedFrom.stateKey != "class" || foldedFrom.maxState != "free" {
		t.Fatalf("from 侧块内不一致的格应为 same、inconsistent、min=class、max=free，实际 %+v", foldedFrom)
	}

	// 同一请求再执行一次：计数完全一致（排序与聚合的确定性，CP6 的前置）。
	again, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffFromID, To: releaseDiffToID})
	if err != nil {
		t.Fatalf("第二次对比失败: %v", err)
	}
	if again.Summary != want {
		t.Fatalf("同一数据两次对比的汇总必须一致:\n got %+v\nwant %+v", again.Summary, want)
	}

	// 交换 from / to（CP5）：added 与 removed 互换、changed 不变、unknown/composite
	// 两侧互换且 delta 取反、rooms 的 added/removed 互换、shared 与 building_changed
	// 不变、inconsistent_blocks 与 same_term 不变。
	swapped, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffToID, To: releaseDiffFromID})
	if err != nil {
		t.Fatalf("交换两侧后对比失败: %v", err)
	}
	wantSwapped := ReleaseDiffSummary{
		Cells:              DiffCellsSummary{Added: 2, Removed: 1, Changed: 4, Total: 7},
		Rooms:              DiffRoomsSummary{Added: 2, Removed: 1, Shared: 6, BuildingChanged: 1},
		Unknown:            StateDelta{From: 0, To: 1, Delta: 1},
		Composite:          StateDelta{From: 1, To: 0, Delta: -1},
		InconsistentBlocks: 2,
		SameTerm:           true,
	}
	if swapped.Summary != wantSwapped {
		t.Fatalf("交换两侧后的汇总不符:\n got %+v\nwant %+v", swapped.Summary, wantSwapped)
	}
	if swapped.From.ReleaseID != releaseDiffToID || swapped.To.ReleaseID != releaseDiffFromID {
		t.Fatalf("交换两侧后版本头也应互换: %+v / %+v", swapped.From, swapped.To)
	}
}

// TestReleaseAdminRepoDiffCrossTermAndAxis 覆盖两件「照常比较」的事：
//   - 不同 term 的两版 same_term=false，差异照常给出（Requirement 3.4）；
//   - 两版 axis_version 不同时仍按 block 编码比较：轴 v2 把小节 05 单独成块 '0304'，
//     于是同一格在两侧落到不同 block（added 1 + removed 1），而不是被某一边的轴
//     版本折成同一个格。
func TestReleaseAdminRepoDiffCrossTermAndAxis(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)

	mustExec(ctx, t, pool, `
INSERT INTO term (term, total_weeks) VALUES ($1, 20), ($2, 20)`,
		releaseDiffTerm, releaseDiffOtherTerm)

	// 轴 v2 与 v1 的唯一差别：小节 05 单独成块 '0304'（v1 里它与 03/04 同块 030405）。
	mustExec(ctx, t, pool, "INSERT INTO axis (version, note) VALUES (2, '夹具轴：05 单独成块')")
	mustExec(ctx, t, pool, `
INSERT INTO axis_node (axis_version, node, ordinal, block, block_ordinal) VALUES
  (2, '01', 1,  '0102',    1),
  (2, '02', 2,  '0102',    1),
  (2, '03', 3,  '030405',  2),
  (2, '04', 4,  '030405',  2),
  (2, '05', 5,  '0304',    2),
  (2, '06', 6,  '0607',    3),
  (2, '07', 7,  '0607',    3),
  (2, '08', 8,  '0809',    4),
  (2, '09', 9,  '0809',    4),
  (2, '10', 10, '101112',  5),
  (2, '11', 11, '101112',  5),
  (2, '12', 12, '101112',  5)`)

	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('r210', 'F210', 'F210', 'Q10', '综合教学楼'),
       ('r211', 'F211', 'F211', 'Q10', '综合教学楼')`)

	// 三个版本：F 与 D 同学期不同轴；C 另一学期同轴。
	const (
		crossTermID = "20260920T000000Z"
		crossAxisID = "20261005T000000Z"
	)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $2, $3, 1, 1, false, true),
       ($4, $2, $5, 1, 2, false, false),
       ($6, $7, $8, 1, 1, false, false)`,
		releaseDiffFromID, releaseDiffTerm,
		mustParseReleaseAdminTime(t, "2026-10-01T00:00:00Z"),
		crossAxisID, mustParseReleaseAdminTime(t, "2026-10-05T00:00:00Z"),
		crossTermID, releaseDiffOtherTerm,
		mustParseReleaseAdminTime(t, "2026-09-20T00:00:00Z"))

	// 三版同样的两格：r210 的 01/02（v1 与 v2 都折成 0102）与 r211 的 05
	// （v1 → 030405，v2 → 0304）。C 只有 r210 变成 class。
	for _, releaseID := range []string{releaseDiffFromID, crossAxisID, crossTermID} {
		mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, 'r210', 'Q10', '综合教学楼'),
       ($1, 'r211', 'Q10', '综合教学楼')`, releaseID)
	}

	stateOf := func(releaseID string) map[string]string {
		if releaseID == crossTermID {
			return map[string]string{"r210": "class", "r211": "free"}
		}
		return map[string]string{"r210": "free", "r211": "free"}
	}
	for _, releaseID := range []string{releaseDiffFromID, crossAxisID, crossTermID} {
		term := releaseDiffTerm
		if releaseID == crossTermID {
			term = releaseDiffOtherTerm
		}
		states := stateOf(releaseID)
		for roomID, nodes := range map[string][]string{
			"r210": {"01", "02"},
			"r211": {"05"},
		} {
			for _, node := range nodes {
				mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, $2, $3, 9, 1, $4, $5, $6)`,
					releaseID, roomID, term, node, states[roomID], states[roomID] == "free")
			}
		}
	}

	repo := NewReleaseAdminRepo(pool)

	// 轴改版：同一小节 05 在两侧折成不同 block，所以是一增一删，不是「未变」。
	axisDiff, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffFromID, To: crossAxisID})
	if err != nil {
		t.Fatalf("跨轴版本对比失败: %v", err)
	}
	if axisDiff.From.AxisVersion != 1 || axisDiff.To.AxisVersion != 2 {
		t.Fatalf("两侧轴版本应如实回显: %+v / %+v", axisDiff.From, axisDiff.To)
	}
	wantAxis := ReleaseDiffSummary{
		Cells:    DiffCellsSummary{Added: 1, Removed: 1, Changed: 0, Total: 2},
		Rooms:    DiffRoomsSummary{Shared: 2},
		SameTerm: true,
	}
	if axisDiff.Summary != wantAxis {
		t.Fatalf("跨轴对比汇总不符:\n got %+v\nwant %+v", axisDiff.Summary, wantAxis)
	}

	// 跨学期：same_term=false，但差异照常给出（r210 的 0102 状态不同）。
	termDiff, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffFromID, To: crossTermID})
	if err != nil {
		t.Fatalf("跨学期版本对比失败: %v", err)
	}
	wantTerm := ReleaseDiffSummary{
		Cells:    DiffCellsSummary{Changed: 1, Total: 1},
		Rooms:    DiffRoomsSummary{Shared: 2},
		SameTerm: false,
	}
	if termDiff.Summary != wantTerm {
		t.Fatalf("跨学期对比汇总不符:\n got %+v\nwant %+v", termDiff.Summary, wantTerm)
	}
}

// TestReleaseAdminRepoDiffMissingRelease 覆盖缺一版：分别点名 from / to，
// 返回包着 ErrReleaseNotFound 的错误与零值结果。
func TestReleaseAdminRepoDiffMissingRelease(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	cases := []struct {
		name     string
		params   ReleaseDiffParams
		wantSide string
	}{
		{
			name:     "缺 to",
			params:   ReleaseDiffParams{From: releaseDiffFromID, To: releaseDiffMissingID},
			wantSide: "to",
		},
		{
			name:     "缺 from",
			params:   ReleaseDiffParams{From: releaseDiffMissingID, To: releaseDiffToID},
			wantSide: "from",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			diff, err := repo.Diff(ctx, testCase.params)
			if !errors.Is(err, ErrReleaseNotFound) {
				t.Fatalf("期望 ErrReleaseNotFound，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), testCase.wantSide) ||
				!strings.Contains(err.Error(), releaseDiffMissingID) {
				t.Fatalf("错误消息应点名 %s 侧与缺失的版本，实际 %q", testCase.wantSide, err)
			}
			if diff.From.ReleaseID != "" || diff.To.ReleaseID != "" ||
				diff.Summary != (ReleaseDiffSummary{}) {
				t.Fatalf("缺一版时应返回零值，实际 %+v", diff)
			}
		})
	}
}

// TestReleaseAdminRepoDiffEmptySides 覆盖两侧都没有观测与归属快照的版本：
// 汇总语句的每个 CTE 在空集上仍各出一行，CROSS JOIN 后是一行全零，而不是零行。
// same_term 仍由两个头比出，与有没有数据无关。
func TestReleaseAdminRepoDiffEmptySides(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)

	const (
		emptyFromID = "20260601T000000Z"
		emptyToID   = "20260608T000000Z"
	)
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, 20)", releaseDiffTerm)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, 1, 1, false),
       ($4, $2, $5, 1, 1, false)`,
		emptyFromID, releaseDiffTerm, mustParseReleaseAdminTime(t, "2026-06-01T00:00:00Z"),
		emptyToID, mustParseReleaseAdminTime(t, "2026-06-08T00:00:00Z"))

	repo := NewReleaseAdminRepo(pool)
	diff, err := repo.Diff(ctx, ReleaseDiffParams{From: emptyFromID, To: emptyToID})
	if err != nil {
		t.Fatalf("对比两个空版本失败: %v", err)
	}
	if diff.From.ReleaseID != emptyFromID || diff.To.ReleaseID != emptyToID {
		t.Fatalf("版本头不符: %+v / %+v", diff.From, diff.To)
	}
	want := ReleaseDiffSummary{SameTerm: true}
	if diff.Summary != want {
		t.Fatalf("空版本之间的汇总应全零:\n got %+v\nwant %+v", diff.Summary, want)
	}
}
