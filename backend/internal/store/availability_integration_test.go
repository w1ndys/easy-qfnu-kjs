package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openAvailabilityTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
func openAvailabilityTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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

// availabilityFixtureTerm 是空教室夹具使用的学期。
const availabilityFixtureTerm = "2026-2027-1"

// seedAvailabilityFixture 造一套覆盖缺失节次、不可用节次、归属快照与关键词的观测。
func seedAvailabilityFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", availabilityFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ('avail-release-1', $1, now(), 1, 1, true)`, availabilityFixtureTerm)

	rooms := []struct {
		id           string
		name         string
		buildingID   string
		buildingName string
		snapshot     bool
	}{
		{id: "a101", name: "F126", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		{id: "a102", name: "F127", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		{id: "a103", name: "F128", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		{id: "b201", name: "F126", buildingID: "Q11", buildingName: "实验楼", snapshot: true},
		// c301 有观测但不在归属快照里，任何筛选都不应返回它。
		{id: "c301", name: "F126", buildingID: "Q12", buildingName: "未在白名单的楼", snapshot: false},
		{id: "d401", name: "综合楼10%教室", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		{id: "e501", name: "综合楼10_教室", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		// f601 有归属快照但该周完全没有观测行，任何区间都不应命中。
		{id: "f601", name: "F129", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
	}

	for _, room := range rooms {
		mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ($1, $2, $2, $3, $4)`, room.id, room.name, room.buildingID, room.buildingName)
		if room.snapshot {
			mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ('avail-release-1', $1, $2, $3)`, room.id, room.buildingID, room.buildingName)
		}
	}

	// a101：01–03 全可用。
	seedAvailabilityObservation(ctx, t, pool, "a101", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "a101", "02", true)
	seedAvailabilityObservation(ctx, t, pool, "a101", "03", true)

	// a102：缺 03 的观测，区间 01–03 应整行排除（情形 A）。
	seedAvailabilityObservation(ctx, t, pool, "a102", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "a102", "02", true)

	// a103：03 有观测但不可用，同样整行排除。
	seedAvailabilityObservation(ctx, t, pool, "a103", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "a103", "02", true)
	seedAvailabilityObservation(ctx, t, pool, "a103", "03", false)

	// b201：另一栋楼，01–03 全可用。
	seedAvailabilityObservation(ctx, t, pool, "b201", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "b201", "02", true)
	seedAvailabilityObservation(ctx, t, pool, "b201", "03", true)

	// c301：数据完整但没有归属快照。
	seedAvailabilityObservation(ctx, t, pool, "c301", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "c301", "02", true)
	seedAvailabilityObservation(ctx, t, pool, "c301", "03", true)

	// d401/e501：用于验证关键词里的 % 与 _ 按字面处理。
	seedAvailabilityObservation(ctx, t, pool, "d401", "01", true)
	seedAvailabilityObservation(ctx, t, pool, "e501", "01", true)
}

// seedAvailabilityObservation 写一格第 5 周周一的观测。
func seedAvailabilityObservation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, roomID, node string, available bool) {
	t.Helper()

	stateKey := "class"
	if available {
		stateKey = "free"
	}
	mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available, raw_text)
VALUES ('avail-release-1', $1, $2, 5, 1, $3, $4, $5, NULL)`,
		roomID, availabilityFixtureTerm, node, stateKey, available)
}

// availabilityFilter 构造第 5 周周一的区间查询。
func availabilityFilter(startNode, endNode string, nodeCount int) AvailabilityFilter {
	return AvailabilityFilter{
		Term:      availabilityFixtureTerm,
		Week:      5,
		Weekday:   1,
		StartNode: startNode,
		EndNode:   endNode,
		NodeCount: nodeCount,
		Limit:     50,
	}
}

// TestAvailabilityEveryNodeMustBeObserved 覆盖情形 A：
// 区间内任一节缺观测或不可用，整间房都不命中。
func TestAvailabilityEveryNodeMustBeObserved(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedAvailabilityFixture(ctx, t, pool)

	page, err := Availability(ctx, pool, availabilityFilter("01", "03", 3))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("01–03 命中总数期望 2，实际 %d：%+v", page.Total, page.Rooms)
	}
	want := []string{"a101", "b201"}
	if len(page.Rooms) != len(want) {
		t.Fatalf("返回房间数期望 %d，实际 %d", len(want), len(page.Rooms))
	}
	for i, room := range page.Rooms {
		if room.ID != want[i] {
			t.Fatalf("第 %d 间期望 %s，实际 %s", i, want[i], room.ID)
		}
		if room.ID == "f601" {
			t.Fatalf("该周没有任何观测的 f601 不应出现: %+v", page.Rooms)
		}
	}

	// 把区间收成 01–02：a102、a103 的缺失/不可用节次不在区间内，应重新命中。
	page, err = Availability(ctx, pool, availabilityFilter("01", "02", 2))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 4 {
		t.Fatalf("01–02 命中总数期望 4，实际 %d：%+v", page.Total, page.Rooms)
	}
}

// TestAvailabilityBuildingFilterUsesSnapshot 覆盖按楼筛选走归属快照，且没有快照的房间不出现。
func TestAvailabilityBuildingFilterUsesSnapshot(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedAvailabilityFixture(ctx, t, pool)

	q10 := availabilityFilter("01", "03", 3)
	q10.Building = "Q10"
	page, err := Availability(ctx, pool, q10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 || len(page.Rooms) != 1 || page.Rooms[0].ID != "a101" {
		t.Fatalf("Q10 期望只命中 a101，实际 %+v", page.Rooms)
	}

	q11 := availabilityFilter("01", "03", 3)
	q11.Building = "Q11"
	page, err = Availability(ctx, pool, q11)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 || page.Rooms[0].ID != "b201" {
		t.Fatalf("Q11 期望只命中 b201，实际 %+v", page.Rooms)
	}

	// c301 在 room 表里有归属，但没有进入发布版本的快照，因此「全部」也应排除它。
	all := availabilityFilter("01", "03", 3)
	page, err = Availability(ctx, pool, all)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	for _, room := range page.Rooms {
		if room.ID == "c301" {
			t.Fatalf("没有归属快照的 c301 不应出现: %+v", page.Rooms)
		}
	}
}

// TestAvailabilityKeywordNormalizationAndEscaping 覆盖全半角、ASCII 大小写与通配符转义。
func TestAvailabilityKeywordNormalizationAndEscaping(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedAvailabilityFixture(ctx, t, pool)

	cases := []struct {
		name      string
		keyword   string
		start     string
		end       string
		nodeCount int
		wantIDs   []string
	}{
		{name: "ASCII 大小写不敏感", keyword: "f126", start: "01", end: "03", nodeCount: 3, wantIDs: []string{"a101", "b201"}},
		// 全半角归一发生在 query.NormalizeKeyword；这里传入归一后的形态，锁定存储层行为。
		{name: "归一后的子串匹配", keyword: "F126", start: "01", end: "03", nodeCount: 3, wantIDs: []string{"a101", "b201"}},
		{name: "百分号按字面匹配", keyword: "10%", start: "01", end: "01", nodeCount: 1, wantIDs: []string{"d401"}},
		{name: "下划线按字面匹配", keyword: "10_", start: "01", end: "01", nodeCount: 1, wantIDs: []string{"e501"}},
		{name: "无匹配关键词", keyword: "不存在的房名", start: "01", end: "03", nodeCount: 3, wantIDs: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			filter := availabilityFilter(c.start, c.end, c.nodeCount)
			filter.Keyword = c.keyword
			page, err := Availability(ctx, pool, filter)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if len(page.Rooms) != len(c.wantIDs) {
				t.Fatalf("命中 %d 间，期望 %d 间：%+v", len(page.Rooms), len(c.wantIDs), page.Rooms)
			}
			for i, want := range c.wantIDs {
				if page.Rooms[i].ID != want {
					t.Fatalf("第 %d 间期望 %s，实际 %s", i, want, page.Rooms[i].ID)
				}
			}
		})
	}
}

// TestAvailabilityOrderingPaginationAndTotal 覆盖房名升序、同房名按房号升序与分页前总数。
func TestAvailabilityOrderingPaginationAndTotal(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedAvailabilityFixture(ctx, t, pool)

	first := availabilityFilter("01", "02", 2)
	first.Limit = 2
	first.Offset = 0
	page, err := Availability(ctx, pool, first)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 4 {
		t.Fatalf("总数期望 4，实际 %d", page.Total)
	}
	wantFirst := []string{"a101", "b201"}
	for i, want := range wantFirst {
		if page.Rooms[i].ID != want {
			t.Fatalf("第一页第 %d 间期望 %s，实际 %s", i, want, page.Rooms[i].ID)
		}
	}

	second := availabilityFilter("01", "02", 2)
	second.Limit = 2
	second.Offset = 2
	page, err = Availability(ctx, pool, second)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	wantSecond := []string{"a102", "a103"}
	for i, want := range wantSecond {
		if page.Rooms[i].ID != want {
			t.Fatalf("第二页第 %d 间期望 %s，实际 %s", i, want, page.Rooms[i].ID)
		}
	}

	// 偏移越界时 total 仍是 4，不能因为当前页为空把总数算成 0。
	beyond := availabilityFilter("01", "02", 2)
	beyond.Offset = 99
	page, err = Availability(ctx, pool, beyond)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 4 || len(page.Rooms) != 0 {
		t.Fatalf("越界页期望 total=4 且 0 间，实际 total=%d rooms=%d", page.Total, len(page.Rooms))
	}
}

// TestAvailabilityEmptyResultFlags 覆盖空态标记：区分无版本、该周无观测与筛选无命中。
func TestAvailabilityEmptyResultFlags(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedAvailabilityFixture(ctx, t, pool)

	// 该周有观测，但 10–12 节没有任何数据：total=0，周仍有观测。
	noNodes := availabilityFilter("10", "12", 3)
	page, err := Availability(ctx, pool, noNodes)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 0 || !page.HasRelease || !page.WeekHasObservation {
		t.Fatalf("区间无数据时标记不符: %+v", page)
	}

	// 第 6 周没有任何观测：该周疑似无排课。
	noWeek := availabilityFilter("01", "03", 3)
	noWeek.Week = 6
	page, err = Availability(ctx, pool, noWeek)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 0 || !page.HasRelease || page.WeekHasObservation {
		t.Fatalf("该周无观测时标记不符: %+v", page)
	}

	// 没有 current 版本：既没有版本也没有观测。
	mustExec(ctx, t, pool, "UPDATE release SET is_current = false WHERE release_id = 'avail-release-1'")
	page, err = Availability(ctx, pool, availabilityFilter("01", "03", 3))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 0 || page.HasRelease || page.WeekHasObservation {
		t.Fatalf("无版本时标记不符: %+v", page)
	}
}
