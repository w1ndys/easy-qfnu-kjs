package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
//   - r210 第 9 周 0102：只在 from → 消失格。这间房两版都有归属快照，格只在一侧，
//     用来钉住「另一侧归属楼照常给值」——归属楼只看该房在该版有没有快照，与格是否
//     出现无关（to 侧归属楼由 to 版快照给出，不是 nil）；
//   - r201 第 11 周 0102：from 01 free + 02 class、to 01/02 都 class → 两侧折叠后的
//     state_key 都是 class（未变格），只有 from 侧块内不一致。这一格钉住
//     inconsistent 取自「from 或 to」两侧的并集 OR，而不是只看 to 侧。
//
// 于是并集 12 个格：added 1 / removed 3 / changed 4 / same 4，unknown 1→0，
// composite 0→1，inconsistent_blocks 2；cells.total 是前三类之和 8，不含 same。
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
		// 这间房两版都有归属快照，但格只在 from 侧：明细里 to_state 是 nil，
		// to_building 仍是 to 版快照里的楼（不是 nil）。
		{releaseDiffFromID, "r210", 9, 1, []string{"01", "02"}, "free"},
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
//   - from：r201 r202 r203 r204 r205 r206 r207 r209 r210（9 间）；
//   - to：  r201 r203 r204 r205 r207 r208 r209 r210（8 间；r204 从 Q10 换到 Q03、
//     r210 从 Q03 换到 Q10）。
//
// 于是 rooms = added 1（r208）/ removed 2（r202、r206）/ shared 7 / building_changed 2
// （r204、r210）。r210 同时是 cells 小节「房号两版都有快照、格只在一侧」的夹具。
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
			{"r210", "Q03", "老文史楼"}, // 换楼的第一侧
		},
		releaseDiffToID: {
			{"r201", "Q10", "综合教学楼"},
			{"r203", "Q10", "综合教学楼"},
			{"r204", "Q03", "老文史楼"}, // 归属变化
			{"r205", "Q10", "综合教学楼"},
			{"r207", "Q03", "老文史楼"},
			{"r208", "Q10", "综合教学楼"}, // 新增房号
			{"r209", "Q10", "综合教学楼"},
			{"r210", "Q10", "综合教学楼"}, // 归属变化，与 r204 方向相反
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
       ('r209', 'F209', 'F209', 'Q10', '综合教学楼'),
       ('r210', 'JA201', 'JA201', 'Q03', '老文史楼')`)

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
		Cells:              DiffCellsSummary{Added: 1, Removed: 3, Changed: 4, Total: 8},
		Rooms:              DiffRoomsSummary{Added: 1, Removed: 2, Shared: 7, BuildingChanged: 2},
		Unknown:            StateDelta{From: 1, To: 0, Delta: -1},
		Composite:          StateDelta{From: 0, To: 1, Delta: 1},
		InconsistentBlocks: 2,
		SameTerm:           true,
	}
	if diff.Summary != want {
		t.Fatalf("汇总不符:\n got %+v\nwant %+v", diff.Summary, want)
	}

	// 折叠 CTE 的逐格计数：并集 12 行，四种 kind 各数一遍。'same' 必须真的被数到
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
	}{12, 4, 1, 3, 4, 2}
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
		Cells:              DiffCellsSummary{Added: 3, Removed: 1, Changed: 4, Total: 8},
		Rooms:              DiffRoomsSummary{Added: 2, Removed: 1, Shared: 7, BuildingChanged: 2},
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

// releaseDiffCellsFixture 是 seedReleaseDiffFixture 的两版在 cells 小节的全部差异行，
// 按固定排序键 (room_id, week, weekday, block) 升序。
//
// 与 releaseDiffFixtureCells 的注释对应：12 个折叠格里 4 个 same 不进明细，剩下
// 8 行 = added 1（r208）/ removed 3（r202、r206、r210）/ changed 4（r203、r205、r207、r209）。
//
// 归属楼只看该房在该版有没有归属快照（release_id + room_id 左连接），与格是否出现
// 无关：格只在一侧时另一侧的格状态为 nil，但归属楼照常给值——最后一行 r210 就是
// 这条的钉子（to 侧没有格，to_building 仍是 to 版快照的 Q10）。
func releaseDiffCellsFixture() []DiffCellRow {
	return []DiffCellRow{
		{
			Kind: DiffKindRemoved, RoomID: "r202", RoomName: "F202",
			Week: 9, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("free"),
			FromBuilding: releaseAdminPtr("Q10"),
		},
		{
			Kind: DiffKindChanged, RoomID: "r203", RoomName: "F203",
			Week: 9, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("free"),
			ToState:      releaseAdminPtr("class"),
			FromBuilding: releaseAdminPtr("Q10"),
			ToBuilding:   releaseAdminPtr("Q10"),
		},
		{
			Kind: DiffKindChanged, RoomID: "r205", RoomName: "F205",
			Week: 9, Weekday: 1, Block: "0607",
			FromState:    releaseAdminPtr("free"),
			ToState:      releaseAdminPtr("class"),
			FromBuilding: releaseAdminPtr("Q10"),
			ToBuilding:   releaseAdminPtr("Q10"),
		},
		{
			Kind: DiffKindRemoved, RoomID: "r206", RoomName: "JA101",
			Week: 9, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("free"),
			FromBuilding: releaseAdminPtr("Q03"),
		},
		{
			Kind: DiffKindChanged, RoomID: "r207", RoomName: "JA102",
			Week: 9, Weekday: 1, Block: "0809",
			FromState:    releaseAdminPtr("unknown"),
			ToState:      releaseAdminPtr("free"),
			FromBuilding: releaseAdminPtr("Q03"),
			ToBuilding:   releaseAdminPtr("Q03"),
		},
		{
			Kind: DiffKindAdded, RoomID: "r208", RoomName: "F208",
			Week: 9, Weekday: 1, Block: "0809",
			ToState:    releaseAdminPtr("composite"),
			ToBuilding: releaseAdminPtr("Q10"),
		},
		{
			Kind: DiffKindChanged, RoomID: "r209", RoomName: "F209",
			Week: 10, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("class"),
			ToState:      releaseAdminPtr("free"),
			FromBuilding: releaseAdminPtr("Q10"),
			ToBuilding:   releaseAdminPtr("Q10"),
		},
		// 房号两版都有归属快照，格只在 from 侧：ToState 为 nil，但 ToBuilding 不是 nil，
		// 而且是 to 版快照里的 Q10（from 版是 Q03）——归属楼与格是否出现无关。
		{
			Kind: DiffKindRemoved, RoomID: "r210", RoomName: "JA201",
			Week: 9, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("free"),
			FromBuilding: releaseAdminPtr("Q03"),
			ToBuilding:   releaseAdminPtr("Q10"),
		},
	}
}

// releaseDiffBuildingsFixture 是 section=buildings 的全部行，按排序键 room_id 升序。
//
// 两间房都换了楼、方向相反：r204 从 Q10 到 Q03、r210 从 Q03 到 Q10。四个归属字段
// 全部非空，因为这一节只取两版快照的交集；相邻页断言靠它们的顺序。
func releaseDiffBuildingsFixture() []DiffBuildingRow {
	return []DiffBuildingRow{
		{
			RoomID: "r204", RoomName: "F204",
			FromBuilding: "Q10", FromBuildingName: "综合教学楼",
			ToBuilding: "Q03", ToBuildingName: "老文史楼",
		},
		{
			RoomID: "r210", RoomName: "JA201",
			FromBuilding: "Q03", FromBuildingName: "老文史楼",
			ToBuilding: "Q10", ToBuildingName: "综合教学楼",
		},
	}
}

// swapDiffCellRows 把 cells 夹具的两侧对调：added ↔ removed、changed 不变，
// 两侧格状态与两侧归属楼互换，行序不变（排序键与哪一侧无关）。
//
// 交换 from / to 后的期望页由它生成，于是断言落到行级 kind 与顺序，而不只是条数。
func swapDiffCellRows(rows []DiffCellRow) []DiffCellRow {
	swapped := make([]DiffCellRow, 0, len(rows))
	for _, row := range rows {
		switch row.Kind {
		case DiffKindAdded:
			row.Kind = DiffKindRemoved
		case DiffKindRemoved:
			row.Kind = DiffKindAdded
		}
		row.FromState, row.ToState = row.ToState, row.FromState
		row.FromBuilding, row.ToBuilding = row.ToBuilding, row.FromBuilding
		swapped = append(swapped, row)
	}
	return swapped
}

// TestReleaseAdminRepoDiffCellsPageAgainstRealDB 覆盖 section=cells 的真实库口径
// （Requirement 4.1、4.2、4.4）：逐行断言排序、kind、两侧状态与两侧归属楼（含 null
// 形状），并按类别过滤逐类断言条数；total 由同一快照的汇总推导，与未分页行数一致。
func TestReleaseAdminRepoDiffCellsPageAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	all := releaseDiffCellsFixture()
	diff, err := repo.Diff(ctx, ReleaseDiffParams{
		From: releaseDiffFromID, To: releaseDiffToID,
		Section: DiffSectionCells, Kind: DiffKindAll, Limit: 200, Offset: 0,
	})
	if err != nil {
		t.Fatalf("读 cells 分页失败: %v", err)
	}
	if !reflect.DeepEqual(diff.Cells, all) {
		t.Fatalf("cells 分页不符:\n got %+v\nwant %+v", diff.Cells, all)
	}
	// rooms / buildings 未选中：仍是非 nil 空切片。
	if diff.Rooms == nil || diff.Buildings == nil {
		t.Fatalf("未选中小节应为非 nil 空切片: rooms=%v buildings=%v",
			diff.Rooms, diff.Buildings)
	}
	if len(diff.Rooms) != 0 || len(diff.Buildings) != 0 {
		t.Fatalf("未选中小节不应有行: %+v / %+v", diff.Rooms, diff.Buildings)
	}

	// r210 的格只在 from 侧、房号两版都有归属快照：to_state 是 nil，但 to_building
	// 不是 nil，且取 to 版快照的 Q10（归属楼只看该房在该版有没有快照，与格是否出现
	// 无关）。单列一条断言，让这条语义在失败时一眼可读，而不只是藏在夹具对比里。
	var onlyFrom *DiffCellRow
	for index := range diff.Cells {
		if diff.Cells[index].RoomID == "r210" {
			onlyFrom = &diff.Cells[index]
			break
		}
	}
	if onlyFrom == nil {
		t.Fatal("cells 分页缺 r210 这一行")
	}
	if onlyFrom.ToState != nil {
		t.Fatalf("r210 只在 from 侧，to_state 应为 nil: %+v", *onlyFrom)
	}
	if onlyFrom.ToBuilding == nil || *onlyFrom.ToBuilding != "Q10" {
		t.Fatalf("r210 两版都有归属快照，to_building 应为 Q10 而不是 nil: %+v", *onlyFrom)
	}

	// total 从汇总推导：all 取 cells.total，且与未分页行数一致（不在分页语句里数行）。
	if got := DiffTotal(DiffSectionCells, DiffKindAll, diff.Summary); got != len(all) {
		t.Fatalf("cells/all 的 total 应为 %d（汇总推导），实际 %d", len(all), got)
	}

	// 逐类过滤：条数与汇总里的分类计数逐一对应。
	for _, testCase := range []struct {
		kind     DiffKind
		want     int
		wantRows []string // room_id 的期望顺序
	}{
		{DiffKindAdded, 1, []string{"r208"}},
		{DiffKindRemoved, 3, []string{"r202", "r206", "r210"}},
		{DiffKindChanged, 4, []string{"r203", "r205", "r207", "r209"}},
	} {
		filtered, filterErr := repo.Diff(ctx, ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionCells, Kind: testCase.kind, Limit: 200, Offset: 0,
		})
		if filterErr != nil {
			t.Fatalf("读 cells/%s 分页失败: %v", testCase.kind, filterErr)
		}
		if len(filtered.Cells) != testCase.want {
			t.Fatalf("cells/%s 应有 %d 行，实际 %d 行: %+v",
				testCase.kind, testCase.want, len(filtered.Cells), filtered.Cells)
		}
		for index, roomID := range testCase.wantRows {
			if filtered.Cells[index].RoomID != roomID {
				t.Fatalf("cells/%s 第 %d 行应为 %s，实际 %+v",
					testCase.kind, index, roomID, filtered.Cells[index])
			}
			if filtered.Cells[index].Kind != testCase.kind {
				t.Fatalf("cells/%s 第 %d 行的 kind 不符: %+v",
					testCase.kind, index, filtered.Cells[index])
			}
		}
		if got, wantTotal := DiffTotal(DiffSectionCells, testCase.kind, filtered.Summary), testCase.want; got != wantTotal {
			t.Fatalf("cells/%s 的 total 应由汇总推导为 %d，实际 %d", testCase.kind, wantTotal, got)
		}
	}

	// 省略 section/kind/limit 时走 cells/all/50：与显式请求同一页。
	defaulted, err := repo.Diff(ctx, ReleaseDiffParams{From: releaseDiffFromID, To: releaseDiffToID})
	if err != nil {
		t.Fatalf("缺省参数读对比失败: %v", err)
	}
	if !reflect.DeepEqual(defaulted.Cells, all) {
		t.Fatalf("缺省 section 应为 cells/all 且页大小 50（夹具 8 行）:\n got %+v\nwant %+v",
			defaulted.Cells, all)
	}
}

// TestReleaseAdminRepoDiffRoomsPageAgainstRealDB 覆盖 section=rooms 的真实库口径
// （Requirement 4.3、4.4）：added / removed 的条数与归属字段的 null 形状。
func TestReleaseAdminRepoDiffRoomsPageAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	wantAll := []DiffRoomRow{
		{
			Kind: DiffKindRemoved, RoomID: "r202", RoomName: "F202",
			FromBuilding:     releaseAdminPtr("Q10"),
			FromBuildingName: releaseAdminPtr("综合教学楼"),
		},
		{
			Kind: DiffKindRemoved, RoomID: "r206", RoomName: "JA101",
			FromBuilding:     releaseAdminPtr("Q03"),
			FromBuildingName: releaseAdminPtr("老文史楼"),
		},
		{
			Kind: DiffKindAdded, RoomID: "r208", RoomName: "F208",
			ToBuilding:     releaseAdminPtr("Q10"),
			ToBuildingName: releaseAdminPtr("综合教学楼"),
		},
	}

	diff, err := repo.Diff(ctx, ReleaseDiffParams{
		From: releaseDiffFromID, To: releaseDiffToID,
		Section: DiffSectionRooms, Kind: DiffKindAll, Limit: 200, Offset: 0,
	})
	if err != nil {
		t.Fatalf("读 rooms 分页失败: %v", err)
	}
	if !reflect.DeepEqual(diff.Rooms, wantAll) {
		t.Fatalf("rooms 分页不符:\n got %+v\nwant %+v", diff.Rooms, wantAll)
	}
	// rooms 的 total 不含 shared：all = added + removed。
	if got := DiffTotal(DiffSectionRooms, DiffKindAll, diff.Summary); got != len(wantAll) {
		t.Fatalf("rooms/all 的 total 应为 %d，实际 %d", len(wantAll), got)
	}

	for _, testCase := range []struct {
		kind DiffKind
		want int
	}{
		{DiffKindAdded, 1},
		{DiffKindRemoved, 2},
	} {
		filtered, filterErr := repo.Diff(ctx, ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionRooms, Kind: testCase.kind, Limit: 200, Offset: 0,
		})
		if filterErr != nil {
			t.Fatalf("读 rooms/%s 分页失败: %v", testCase.kind, filterErr)
		}
		if len(filtered.Rooms) != testCase.want {
			t.Fatalf("rooms/%s 应有 %d 行，实际 %d 行: %+v",
				testCase.kind, testCase.want, len(filtered.Rooms), filtered.Rooms)
		}
		for _, row := range filtered.Rooms {
			if row.Kind != testCase.kind {
				t.Fatalf("rooms/%s 出现别的 kind: %+v", testCase.kind, row)
			}
			// added 的 from_* 必须为 nil，removed 的 to_* 必须为 nil（null 形状）。
			if testCase.kind == DiffKindAdded &&
				(row.FromBuilding != nil || row.FromBuildingName != nil) {
				t.Fatalf("新增房不应有 from_*: %+v", row)
			}
			if testCase.kind == DiffKindRemoved &&
				(row.ToBuilding != nil || row.ToBuildingName != nil) {
				t.Fatalf("消失房不应有 to_*: %+v", row)
			}
		}
		if got := DiffTotal(DiffSectionRooms, testCase.kind, filtered.Summary); got != testCase.want {
			t.Fatalf("rooms/%s 的 total 应为 %d，实际 %d", testCase.kind, testCase.want, got)
		}
	}
}

// TestReleaseAdminRepoDiffBuildingsPageAgainstRealDB 覆盖 section=buildings 的真实库口径
// （Requirement 4.3、4.4）：只返回两侧都有且换了楼的房号，归属字段都非空；
// all 与 changed 是同一集合，total 取汇总的 building_changed。
func TestReleaseAdminRepoDiffBuildingsPageAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	want := releaseDiffBuildingsFixture()

	for _, kind := range []DiffKind{DiffKindAll, DiffKindChanged} {
		diff, err := repo.Diff(ctx, ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionBuildings, Kind: kind, Limit: 200, Offset: 0,
		})
		if err != nil {
			t.Fatalf("读 buildings/%s 分页失败: %v", kind, err)
		}
		if !reflect.DeepEqual(diff.Buildings, want) {
			t.Fatalf("buildings/%s 分页不符:\n got %+v\nwant %+v", kind, diff.Buildings, want)
		}
		if got := DiffTotal(DiffSectionBuildings, kind, diff.Summary); got != len(want) {
			t.Fatalf("buildings/%s 的 total 应为 %d，实际 %d", kind, len(want), got)
		}
	}
}

// TestReleaseAdminRepoDiffPaginationAgainstRealDB 覆盖分页的确定性与边界（CP6）：
// 三节各自相邻页无重复、无遗漏，offset 翻过末页返回空 items 且 total 不变，同一请求
// 重复执行返回同一页；buildings 的相邻页断言同时钉住两间换楼房号的排序。
func TestReleaseAdminRepoDiffPaginationAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseDiffFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	all := releaseDiffCellsFixture()
	params := func(limit, offset int) ReleaseDiffParams {
		return ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionCells, Kind: DiffKindAll, Limit: limit, Offset: offset,
		}
	}

	// 每页 2 条：2/2/2/2（8 行），拼起来必须与一次性取全量完全一致（无重复、无遗漏）。
	collected := make([]DiffCellRow, 0, len(all))
	for offset := 0; offset < len(all); offset += 2 {
		page, err := repo.Diff(ctx, params(2, offset))
		if err != nil {
			t.Fatalf("读第 %d 页失败: %v", offset/2, err)
		}
		wantLen := 2
		if remaining := len(all) - offset; remaining < 2 {
			wantLen = remaining
		}
		if len(page.Cells) != wantLen {
			t.Fatalf("offset=%d 的页应有 %d 行，实际 %d 行: %+v",
				offset, wantLen, len(page.Cells), page.Cells)
		}
		if got := DiffTotal(DiffSectionCells, DiffKindAll, page.Summary); got != len(all) {
			t.Fatalf("offset=%d 的 total 应恒为 %d，实际 %d", offset, len(all), got)
		}
		collected = append(collected, page.Cells...)
	}
	if !reflect.DeepEqual(collected, all) {
		t.Fatalf("相邻页拼接后应与全量一致（无重复无遗漏）:\n got %+v\nwant %+v", collected, all)
	}

	// offset 翻过末页：空 items，total 仍是真实值。
	beyond, err := repo.Diff(ctx, params(50, 100))
	if err != nil {
		t.Fatalf("读越界页失败: %v", err)
	}
	if beyond.Cells == nil {
		t.Fatal("越界页应返回空切片而不是 nil")
	}
	if len(beyond.Cells) != 0 {
		t.Fatalf("越界页不应有行: %+v", beyond.Cells)
	}
	if got := DiffTotal(DiffSectionCells, DiffKindAll, beyond.Summary); got != len(all) {
		t.Fatalf("越界页的 total 应仍为 %d，实际 %d", len(all), got)
	}

	// 同一请求重复执行：行与顺序完全一致。
	first, err := repo.Diff(ctx, params(2, 2))
	if err != nil {
		t.Fatalf("读固定页失败: %v", err)
	}
	second, err := repo.Diff(ctx, params(2, 2))
	if err != nil {
		t.Fatalf("重复读固定页失败: %v", err)
	}
	if !reflect.DeepEqual(first.Cells, second.Cells) {
		t.Fatalf("同一请求两次应返回同一页:\n first %+v\nsecond %+v", first.Cells, second.Cells)
	}
	if len(first.Cells) != 2 || first.Cells[0].RoomID != "r205" || first.Cells[1].RoomID != "r206" {
		t.Fatalf("offset=2 的页应为 r205、r206，实际 %+v", first.Cells)
	}

	// rooms 小节也逐页翻：每页 1 条，拼起来等于全量。
	roomAll := []DiffRoomRow{
		{Kind: DiffKindRemoved, RoomID: "r202", RoomName: "F202",
			FromBuilding: releaseAdminPtr("Q10"), FromBuildingName: releaseAdminPtr("综合教学楼")},
		{Kind: DiffKindRemoved, RoomID: "r206", RoomName: "JA101",
			FromBuilding: releaseAdminPtr("Q03"), FromBuildingName: releaseAdminPtr("老文史楼")},
		{Kind: DiffKindAdded, RoomID: "r208", RoomName: "F208",
			ToBuilding: releaseAdminPtr("Q10"), ToBuildingName: releaseAdminPtr("综合教学楼")},
	}
	roomCollected := make([]DiffRoomRow, 0, len(roomAll))
	for offset := 0; offset < len(roomAll); offset++ {
		page, pageErr := repo.Diff(ctx, ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionRooms, Kind: DiffKindAll, Limit: 1, Offset: offset,
		})
		if pageErr != nil {
			t.Fatalf("读 rooms 第 %d 页失败: %v", offset, pageErr)
		}
		if len(page.Rooms) != 1 {
			t.Fatalf("rooms offset=%d 应有 1 行，实际 %+v", offset, page.Rooms)
		}
		roomCollected = append(roomCollected, page.Rooms...)
	}
	if !reflect.DeepEqual(roomCollected, roomAll) {
		t.Fatalf("rooms 相邻页拼接不符:\n got %+v\nwant %+v", roomCollected, roomAll)
	}

	// rooms 越界页：空 items 但不是 nil，total 仍是 added + removed（翻页不改变 total）。
	roomBeyond, roomBeyondErr := repo.Diff(ctx, ReleaseDiffParams{
		From: releaseDiffFromID, To: releaseDiffToID,
		Section: DiffSectionRooms, Kind: DiffKindAll, Limit: 50, Offset: 100,
	})
	if roomBeyondErr != nil {
		t.Fatalf("读 rooms 越界页失败: %v", roomBeyondErr)
	}
	if roomBeyond.Rooms == nil {
		t.Fatal("rooms 越界页应返回空切片而不是 nil")
	}
	if len(roomBeyond.Rooms) != 0 {
		t.Fatalf("rooms 越界页不应有行: %+v", roomBeyond.Rooms)
	}
	if got := DiffTotal(DiffSectionRooms, DiffKindAll, roomBeyond.Summary); got != len(roomAll) {
		t.Fatalf("rooms 越界页的 total 应仍为 %d，实际 %d", len(roomAll), got)
	}

	// rooms 同一请求重复执行：行与顺序完全一致（offset=1 的页应是 r206）。
	roomPage := func(offset int) ReleaseDiffParams {
		return ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionRooms, Kind: DiffKindAll, Limit: 1, Offset: offset,
		}
	}
	roomFirst, roomFirstErr := repo.Diff(ctx, roomPage(1))
	if roomFirstErr != nil {
		t.Fatalf("读 rooms 固定页失败: %v", roomFirstErr)
	}
	roomSecond, roomSecondErr := repo.Diff(ctx, roomPage(1))
	if roomSecondErr != nil {
		t.Fatalf("重复读 rooms 固定页失败: %v", roomSecondErr)
	}
	if !reflect.DeepEqual(roomFirst.Rooms, roomSecond.Rooms) {
		t.Fatalf("rooms 同一请求两次应返回同一页:\n first %+v\nsecond %+v",
			roomFirst.Rooms, roomSecond.Rooms)
	}
	if len(roomFirst.Rooms) != 1 || roomFirst.Rooms[0].RoomID != "r206" {
		t.Fatalf("rooms offset=1 的页应为 r206，实际 %+v", roomFirst.Rooms)
	}

	// buildings 小节逐页翻：每页 1 条，相邻页各一间且顺序由 room_id 决定
	// （r204 Q10→Q03、r210 Q03→Q10），拼起来等于全量。
	buildingAll := releaseDiffBuildingsFixture()
	buildingParams := func(offset int) ReleaseDiffParams {
		return ReleaseDiffParams{
			From: releaseDiffFromID, To: releaseDiffToID,
			Section: DiffSectionBuildings, Kind: DiffKindAll, Limit: 1, Offset: offset,
		}
	}
	buildingCollected := make([]DiffBuildingRow, 0, len(buildingAll))
	for offset := 0; offset < len(buildingAll); offset++ {
		page, pageErr := repo.Diff(ctx, buildingParams(offset))
		if pageErr != nil {
			t.Fatalf("读 buildings 第 %d 页失败: %v", offset, pageErr)
		}
		if len(page.Buildings) != 1 {
			t.Fatalf("buildings offset=%d 应有 1 行，实际 %+v", offset, page.Buildings)
		}
		if got, want := page.Buildings[0].RoomID, buildingAll[offset].RoomID; got != want {
			t.Fatalf("buildings offset=%d 的相邻页应为 %s，实际 %s", offset, want, got)
		}
		if got := DiffTotal(DiffSectionBuildings, DiffKindAll, page.Summary); got != len(buildingAll) {
			t.Fatalf("buildings offset=%d 的 total 应恒为 %d，实际 %d",
				offset, len(buildingAll), got)
		}
		buildingCollected = append(buildingCollected, page.Buildings...)
	}
	if !reflect.DeepEqual(buildingCollected, buildingAll) {
		t.Fatalf("buildings 相邻页拼接不符:\n got %+v\nwant %+v", buildingCollected, buildingAll)
	}

	// buildings 越界页：空 items 但不是 nil，total 仍是 building_changed。
	buildingBeyond, buildingBeyondErr := repo.Diff(ctx, ReleaseDiffParams{
		From: releaseDiffFromID, To: releaseDiffToID,
		Section: DiffSectionBuildings, Kind: DiffKindAll, Limit: 50, Offset: 100,
	})
	if buildingBeyondErr != nil {
		t.Fatalf("读 buildings 越界页失败: %v", buildingBeyondErr)
	}
	if buildingBeyond.Buildings == nil {
		t.Fatal("buildings 越界页应返回空切片而不是 nil")
	}
	if len(buildingBeyond.Buildings) != 0 {
		t.Fatalf("buildings 越界页不应有行: %+v", buildingBeyond.Buildings)
	}
	if got := DiffTotal(DiffSectionBuildings, DiffKindAll, buildingBeyond.Summary); got != len(buildingAll) {
		t.Fatalf("buildings 越界页的 total 应仍为 %d，实际 %d", len(buildingAll), got)
	}

	// buildings 同一请求重复执行：行与顺序完全一致（offset=1 的页应是 r210）。
	buildingFirst, buildingFirstErr := repo.Diff(ctx, buildingParams(1))
	if buildingFirstErr != nil {
		t.Fatalf("读 buildings 固定页失败: %v", buildingFirstErr)
	}
	buildingSecond, buildingSecondErr := repo.Diff(ctx, buildingParams(1))
	if buildingSecondErr != nil {
		t.Fatalf("重复读 buildings 固定页失败: %v", buildingSecondErr)
	}
	if !reflect.DeepEqual(buildingFirst.Buildings, buildingSecond.Buildings) {
		t.Fatalf("buildings 同一请求两次应返回同一页:\n first %+v\nsecond %+v",
			buildingFirst.Buildings, buildingSecond.Buildings)
	}
	if len(buildingFirst.Buildings) != 1 || buildingFirst.Buildings[0].RoomID != "r210" {
		t.Fatalf("buildings offset=1 的页应为 r210，实际 %+v", buildingFirst.Buildings)
	}

	// 交换 from / to 后分页仍按同一排序键：行序不变，added 与 removed 互换、changed
	// 行不变，两侧格状态与两侧归属楼互换——逐行断言到 kind 与顺序，不只是条数。
	swapped, err := repo.Diff(ctx, ReleaseDiffParams{
		From: releaseDiffToID, To: releaseDiffFromID,
		Section: DiffSectionCells, Kind: DiffKindAll, Limit: 200, Offset: 0,
	})
	if err != nil {
		t.Fatalf("交换两侧后读 cells 分页失败: %v", err)
	}
	if wantSwapped := swapDiffCellRows(all); !reflect.DeepEqual(swapped.Cells, wantSwapped) {
		t.Fatalf("交换两侧后的 cells 页不符:\n got %+v\nwant %+v", swapped.Cells, wantSwapped)
	}
	if got := DiffTotal(DiffSectionCells, DiffKindAll, swapped.Summary); got != len(all) {
		t.Fatalf("交换两侧后 total 应不变: got %d want %d", got, len(all))
	}
}

// releaseAdminRollbackOtherTerm / releaseAdminRollbackForeignID 是跨学期回退用例的
// 另一学期与非 current 版本。只在这条用例里单独铺，不并进共享夹具：列表/详情用例
// 按三版夹具写死断言，多插一版会让它们一起偏。
const (
	releaseAdminRollbackOtherTerm = "2025-2026-2"
	releaseAdminRollbackForeignID = "20260901T010203Z"
)

// seedReleaseAdminForeignTermRelease 追加一个另一学期的非 current 版本。
func seedReleaseAdminForeignTermRelease(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20)",
		releaseAdminRollbackOtherTerm)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $2, $3, 1, 1, false, false)`,
		releaseAdminRollbackForeignID, releaseAdminRollbackOtherTerm,
		mustParseReleaseAdminTime(t, "2026-09-01T01:02:03Z"))
}

// releaseAdminCountRows 统计一张表的行数；表名走 pgx.Identifier 转义，调用方只传常量。
func releaseAdminCountRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
		t.Fatalf("统计 %s 行数失败: %v", table, err)
	}
	return count
}

// releaseAdminReleaseState 是 release 表一行的 (release_id, is_current)：拒绝分支要断言
// 「版本表零变更」，行集合与 current 标记都不能动。
type releaseAdminReleaseState struct {
	ReleaseID string
	IsCurrent bool
}

// releaseAdminReleaseStates 按 release_id 升序取 release 表的 (标识, current) 全集。
func releaseAdminReleaseStates(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []releaseAdminReleaseState {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT release_id, is_current FROM release ORDER BY release_id")
	if err != nil {
		t.Fatalf("读取 release 表状态失败: %v", err)
	}
	defer rows.Close()

	states := make([]releaseAdminReleaseState, 0)
	for rows.Next() {
		var state releaseAdminReleaseState
		if err := rows.Scan(&state.ReleaseID, &state.IsCurrent); err != nil {
			t.Fatalf("扫描 release 表状态失败: %v", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 release 表状态失败: %v", err)
	}
	return states
}

// releaseAdminRunState 是 collect_run 一行里回退不该改动的三列。
type releaseAdminRunState struct {
	RunID      int64
	ReleaseID  *string
	ReleasedAt *time.Time
}

// releaseAdminRunStates 按 run_id 升序取 collect_run 的 (轮次, 关联版本, 发布时刻)。
func releaseAdminRunStates(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []releaseAdminRunState {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT run_id, release_id, released_at FROM collect_run ORDER BY run_id")
	if err != nil {
		t.Fatalf("读取 collect_run 状态失败: %v", err)
	}
	defer rows.Close()

	states := make([]releaseAdminRunState, 0)
	for rows.Next() {
		var state releaseAdminRunState
		if err := rows.Scan(&state.RunID, &state.ReleaseID, &state.ReleasedAt); err != nil {
			t.Fatalf("扫描 collect_run 状态失败: %v", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 collect_run 状态失败: %v", err)
	}
	return states
}

// releaseAdminCurrentIDs 返回库中 is_current 的版本标识；多于一行说明唯一索引的
// 不变量已被破坏，用例直接失败。
func releaseAdminCurrentIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, "SELECT release_id FROM release WHERE is_current ORDER BY release_id")
	if err != nil {
		t.Fatalf("读取当前版本失败: %v", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描当前版本失败: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历当前版本失败: %v", err)
	}
	return ids
}

// TestReleaseAdminRepoRollbackAgainstRealDB 覆盖成功回退的真实库口径（Requirement 5.1、
// 6.2、6.3、6.4）：current 在目标与原当前之间交换；release / observation /
// release_room_building 行数不变；collect_run 的 release_id 与 released_at 不变。
func TestReleaseAdminRepoRollbackAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseAdminFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	releasesBefore := releaseAdminReleaseStates(ctx, t, pool)
	observationRowsBefore := releaseAdminCountRows(ctx, t, pool, "observation")
	roomBuildingRowsBefore := releaseAdminCountRows(ctx, t, pool, "release_room_building")
	runsBefore := releaseAdminRunStates(ctx, t, pool)

	result, err := repo.Rollback(ctx, releaseAdminPreviousID)
	if err != nil {
		t.Fatalf("回退到 %s 应成功，实际 %v", releaseAdminPreviousID, err)
	}
	wantResult := RollbackResult{
		RolledBackFrom:   releaseAdminCurrentID,
		CurrentReleaseID: releaseAdminPreviousID,
		Term:             releaseAdminFixtureTerm,
	}
	if result != wantResult {
		t.Fatalf("回退结果不符：期望 %+v，实际 %+v", wantResult, result)
	}

	// current 交换：恰有一个 current，且是回退目标。
	if ids := releaseAdminCurrentIDs(ctx, t, pool); !reflect.DeepEqual(ids, []string{releaseAdminPreviousID}) {
		t.Fatalf("回退后应恰有目标版本一个 current，实际 %v", ids)
	}

	// 版本集合不变，只是 current 标记移动：release 表的行数与标识都不动。
	releasesAfter := releaseAdminReleaseStates(ctx, t, pool)
	if len(releasesAfter) != len(releasesBefore) {
		t.Fatalf("回退不应改变版本行数：回退前 %d，回退后 %d", len(releasesBefore), len(releasesAfter))
	}
	for index, before := range releasesBefore {
		if after := releasesAfter[index]; after.ReleaseID != before.ReleaseID {
			t.Fatalf("回退不应改动版本集合：第 %d 行 %s → %s", index, before.ReleaseID, after.ReleaseID)
		}
		if want := before.ReleaseID == releaseAdminPreviousID; releasesAfter[index].IsCurrent != want {
			t.Fatalf("版本 %s 的 current 标记应为 %t，实际 %t",
				before.ReleaseID, want, releasesAfter[index].IsCurrent)
		}
	}

	// 回退不重建、不删除版本数据（CP9）。
	if got := releaseAdminCountRows(ctx, t, pool, "observation"); got != observationRowsBefore {
		t.Fatalf("回退不应改变观测行数：回退前 %d，回退后 %d", observationRowsBefore, got)
	}
	if got := releaseAdminCountRows(ctx, t, pool, "release_room_building"); got != roomBuildingRowsBefore {
		t.Fatalf("回退不应改变归属快照行数：回退前 %d，回退后 %d", roomBuildingRowsBefore, got)
	}

	// 回退不写 collect_run：轮次记录「这一轮发布了哪一版」，回退是管理动作（design）。
	if runsAfter := releaseAdminRunStates(ctx, t, pool); !reflect.DeepEqual(runsAfter, runsBefore) {
		t.Fatalf("回退不应改动 collect_run：回退前 %+v，回退后 %+v", runsBefore, runsAfter)
	}

	// 列表口径（Requirement 6.3）：is_current 只出现一次，且指向目标版本；排序不变。
	summaries, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("回退后读版本列表失败: %v", err)
	}
	currentCount := 0
	for _, summary := range summaries {
		if summary.IsCurrent {
			currentCount++
			if summary.ReleaseID != releaseAdminPreviousID {
				t.Fatalf("current 应指向回退目标 %s，实际 %s", releaseAdminPreviousID, summary.ReleaseID)
			}
		}
	}
	if currentCount != 1 {
		t.Fatalf("列表里应恰有一个 current，实际 %d 个", currentCount)
	}
	if len(summaries) != len(releasesBefore) {
		t.Fatalf("列表版本数应保持 %d，实际 %d", len(releasesBefore), len(summaries))
	}
}

// TestReleaseAdminRepoRollbackRejectsAgainstRealDB 覆盖三条拒绝分支（Requirement 5.3–5.5）：
// 缺目标 40401、已 current 40904、跨学期 40905；每条都断言 release 表零变更
// （行集合与 current 标记都不动）。
func TestReleaseAdminRepoRollbackRejectsAgainstRealDB(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		foreign bool // 是否追加另一学期的非 current 版本
		wantErr error
	}{
		{name: "目标不存在", target: "20200101T000000Z", wantErr: ErrReleaseNotFound},
		{name: "目标已是当前版本", target: releaseAdminCurrentID, wantErr: ErrReleaseNotRollbackable},
		{name: "目标跨学期", target: releaseAdminRollbackForeignID, foreign: true, wantErr: ErrReleaseTermMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, pool := openReleaseAdminTestDB(t)
			seedReleaseAdminFixture(ctx, t, pool)
			if tc.foreign {
				seedReleaseAdminForeignTermRelease(ctx, t, pool)
			}
			repo := NewReleaseAdminRepo(pool)

			before := releaseAdminReleaseStates(ctx, t, pool)
			result, err := repo.Rollback(ctx, tc.target)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
			if result != (RollbackResult{}) {
				t.Fatalf("拒绝路径必须返回零值结果，实际 %+v", result)
			}
			after := releaseAdminReleaseStates(ctx, t, pool)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("拒绝的回退必须让 release 表零变更：拒绝前 %+v，拒绝后 %+v", before, after)
			}
		})
	}
}

// TestReleaseAdminRepoRollbackNoCurrentAgainstRealDB 覆盖「库中没有当前版本」的 40904：
// design 把它与「目标已是 current」归为同一个错误码。这里手工清掉 current 标记后回退，
// 同样断言 release 表零变更。
func TestReleaseAdminRepoRollbackNoCurrentAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseAdminFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	mustExec(ctx, t, pool, "UPDATE release SET is_current = false WHERE is_current")
	before := releaseAdminReleaseStates(ctx, t, pool)

	result, err := repo.Rollback(ctx, releaseAdminPreviousID)
	if !errors.Is(err, ErrReleaseNotRollbackable) {
		t.Fatalf("没有 current 时应返回 ErrReleaseNotRollbackable，实际 %v", err)
	}
	if result != (RollbackResult{}) {
		t.Fatalf("拒绝路径必须返回零值结果，实际 %+v", result)
	}
	if after := releaseAdminReleaseStates(ctx, t, pool); !reflect.DeepEqual(after, before) {
		t.Fatalf("没有 current 时的回退必须零变更：拒绝前 %+v，拒绝后 %+v", before, after)
	}
}

// TestReleaseAdminRepoRollbackConcurrentAgainstRealDB 让两个 goroutine 同时回退同一目标：
// 恰好一个成功，另一个拿到 40904（目标已被抬成 current 或锁后看不到 current），
// 或极少数情况下撞上数据库层冲突的 40906；结束后恰有一个 current，且是目标版本。
//
// 两个事务的锁序相同（先当前行、再目标行），所以正常情况下不产生死锁：后到者要么在
// 第一条 SELECT 上等锁、EPQ 重检后发现原 current 已不再是 current，要么直接看到目标
// 已是 current。两者都落在 ErrReleaseNotRollbackable。
func TestReleaseAdminRepoRollbackConcurrentAgainstRealDB(t *testing.T) {
	ctx, pool := openReleaseAdminTestDB(t)
	seedReleaseAdminFixture(ctx, t, pool)
	repo := NewReleaseAdminRepo(pool)

	releasesBefore := releaseAdminReleaseStates(ctx, t, pool)

	type outcome struct {
		result RollbackResult
		err    error
	}
	const racers = 2
	start := make(chan struct{})
	outcomes := make(chan outcome, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := repo.Rollback(ctx, releaseAdminPreviousID)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)

	succeeded := 0
	for got := range outcomes {
		if got.err == nil {
			succeeded++
			if got.result.CurrentReleaseID != releaseAdminPreviousID {
				t.Fatalf("成功的回退应把 %s 置为 current，实际 %+v", releaseAdminPreviousID, got.result)
			}
			continue
		}
		if !errors.Is(got.err, ErrReleaseNotRollbackable) && !errors.Is(got.err, ErrReleaseConflict) {
			t.Fatalf("后到的回退只允许 40904 或 40906，实际 %v", got.err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("两个并发回退应恰好成功一个，实际成功 %d 个", succeeded)
	}

	if ids := releaseAdminCurrentIDs(ctx, t, pool); !reflect.DeepEqual(ids, []string{releaseAdminPreviousID}) {
		t.Fatalf("并发回退后应恰有目标版本一个 current，实际 %v", ids)
	}
	if after := releaseAdminReleaseStates(ctx, t, pool); len(after) != len(releasesBefore) {
		t.Fatalf("并发回退不应改变版本行数：并发前 %d，并发后 %d", len(releasesBefore), len(after))
	}
}
