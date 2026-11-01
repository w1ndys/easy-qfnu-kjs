package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// screenFixtureTerm 是大屏集成测试使用的学期。
const screenFixtureTerm = "2026-2027-1"

// seedScreenFixture 造一版当前发布版本、一版历史版本与两轮同步：
//   - 当前版本有 3 间房、2 栋楼；历史版本里还有第 4 间房与第 8 周的观测，都不该进大屏；
//   - 观测覆盖空闲、上课、未知、复合、锁定与字典里查不到的状态，用来验证占用口径（CP14）；
//   - 产出本轮（success，窗口 5..8）之后还有一轮 failed，验证 last_run 与 published 分开取。
func seedScreenFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", screenFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ('screen-release-cur', $1, $2, 1, 1, true, true),
       ('screen-release-old', $1, $2 - interval '7 days', 1, 1, false, false)`,
		screenFixtureTerm, freshnessAt(t, "2026-10-23T07:10:12Z"))

	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('s101', 'F101', 'F101', 'Q10', '综合教学楼'),
       ('s102', 'F102', 'F102', 'Q10', '综合教学楼'),
       ('s103', 'F201', 'F201', 'Q11', '外语楼'),
       ('s104', 'F999', 'F999', 'Q10', '综合教学楼')`)

	mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ('screen-release-cur', 's101', 'Q10', '综合教学楼'),
       ('screen-release-cur', 's102', 'Q10', '综合教学楼'),
       ('screen-release-cur', 's103', 'Q11', '外语楼'),
       ('screen-release-old', 's104', 'Q10', '综合教学楼')`)

	// 6 格 Q10 + 2 格 Q11：空闲 3 格不计占用，上课/未知/复合/锁定与字典外状态各计 1 格。
	mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ('screen-release-cur', 's101', $1, 5, 1, '01', 'free', true),
       ('screen-release-cur', 's101', $1, 5, 1, '02', 'class', false),
       ('screen-release-cur', 's101', $1, 5, 2, '03', 'unknown', false),
       ('screen-release-cur', 's101', $1, 6, 1, '01', 'composite', false),
       ('screen-release-cur', 's102', $1, 5, 1, '01', 'free', true),
       ('screen-release-cur', 's102', $1, 5, 3, '06', 'mystery', false),
       ('screen-release-cur', 's103', $1, 5, 4, '10', 'free', true),
       ('screen-release-cur', 's103', $1, 5, 5, '11', 'locked', false),
       ('screen-release-old', 's104', $1, 8, 1, '01', 'free', true)`, screenFixtureTerm)

	// 产出当前版本的那一轮：窗口 5..8，带异常计数与归属冲突。
	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, finished_at, status, term, window_start, window_end,
                         unknown_cells, composite_cells, attr_conflict_count, release_id)
VALUES ($1, $2, 'success', $3, 5, 8, 12, 51, 2, 'screen-release-cur')`,
		freshnessAt(t, "2026-10-23T06:40:00Z"), freshnessAt(t, "2026-10-23T07:10:30Z"), screenFixtureTerm)

	// 更晚的一轮失败：只影响 last_run，不该覆盖产出轮次的窗口与计数。
	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, finished_at, status, term, error_code, error_message)
VALUES ($1, $2, 'failed', $3, 'ocr_failed', '验证码识别失败：<html>secret</html>')`,
		freshnessAt(t, "2026-10-27T01:00:00Z"), freshnessAt(t, "2026-10-27T01:05:00Z"), screenFixtureTerm)
}

// screenCell 按 (楼, 星期, 大节) 找聚合格；找不到时返回 nil。
func screenCell(facts ScreenFacts, buildingID string, weekday, block int) *ScreenOccupancyCell {
	for i := range facts.Occupancy {
		cell := &facts.Occupancy[i]
		if cell.BuildingID == buildingID && cell.Weekday == weekday && cell.BlockOrdinal == block {
			return cell
		}
	}
	return nil
}

// TestScreenFacts 覆盖大屏事实读取：规模只数当前版本的归属快照、
// 窗口与异常计数取产出轮次、last_run 取最近一轮、占用按字典投影计数。
func TestScreenFacts(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedScreenFixture(ctx, t, pool)

	facts, err := Screen(ctx, pool)
	if err != nil {
		t.Fatalf("读取大屏事实失败: %v", err)
	}

	if facts.Release == nil {
		t.Fatal("有当前版本时 release 不该为 nil")
	}
	if facts.Release.ID != "screen-release-cur" || facts.Release.Term != screenFixtureTerm {
		t.Fatalf("版本不符: %+v", facts.Release)
	}
	if facts.Release.TotalWeeks != 20 {
		t.Fatalf("学期总周数不符: %d", facts.Release.TotalWeeks)
	}
	// 历史版本里的 s104 不该进当前版本的规模。
	if facts.Release.Rooms != 3 || facts.Release.Buildings != 2 {
		t.Fatalf("规模应只数当前版本快照（3 房 2 楼），实际 %+v", facts.Release)
	}

	if facts.Published == nil {
		t.Fatal("应能取到产出当前版本的那一轮")
	}
	if facts.Published.Status != "success" || facts.Published.UnknownCells != 12 ||
		facts.Published.CompositeCells != 51 || facts.Published.AttrConflictCount != 2 {
		t.Fatalf("产出轮次计数不符: %+v", facts.Published)
	}
	if facts.Published.WindowStart == nil || *facts.Published.WindowStart != 5 ||
		facts.Published.WindowEnd == nil || *facts.Published.WindowEnd != 8 {
		t.Fatalf("采集窗口不符: %+v", facts.Published)
	}

	if facts.LastRun == nil {
		t.Fatal("应能取到最近一轮同步")
	}
	if facts.LastRun.Status != "failed" {
		t.Fatalf("最近轮次应是最晚的 failed，实际 %q", facts.LastRun.Status)
	}
	// last_run 与 published 必须是不同的两轮：失败轮次不该顶掉产出轮次的窗口。
	if facts.LastRun.WindowStart != nil {
		t.Fatalf("失败轮次没有窗口，last_run 不该带窗口: %+v", facts.LastRun)
	}

	// 当前版本共 8 格观测（历史版本的第 9 格不计）。
	total := 0
	for _, cell := range facts.Occupancy {
		total += cell.Cells
	}
	if total != 8 {
		t.Fatalf("占用聚合应只数当前版本，期望 8 格，实际 %d: %+v", total, facts.Occupancy)
	}

	q10 := screenCell(facts, "Q10", 1, 1)
	if q10 == nil || q10.Cells != 4 || q10.Occupied != 2 {
		t.Fatalf("Q10 周一第 1 大节应为 4 格占用 2（上课与复合），实际 %+v", q10)
	}
	if q10.BuildingName != "综合教学楼" {
		t.Fatalf("楼名应取归属快照: %+v", q10)
	}
	// 未知格计入占用（CP14）。
	if cell := screenCell(facts, "Q10", 2, 2); cell == nil || cell.Cells != 1 || cell.Occupied != 1 {
		t.Fatalf("未知格应计入占用，实际 %+v", cell)
	}
	// 字典里没有的状态也计入占用：LEFT JOIN 让缺字典行按不可用处理。
	if cell := screenCell(facts, "Q10", 3, 3); cell == nil || cell.Cells != 1 || cell.Occupied != 1 {
		t.Fatalf("字典外状态应计入占用，实际 %+v", cell)
	}
	if cell := screenCell(facts, "Q11", 4, 5); cell == nil || cell.Cells != 1 || cell.Occupied != 0 {
		t.Fatalf("空闲格不该计入占用，实际 %+v", cell)
	}
	if cell := screenCell(facts, "Q11", 5, 5); cell == nil || cell.Cells != 1 || cell.Occupied != 1 {
		t.Fatalf("锁定格应计入占用，实际 %+v", cell)
	}
}

// TestScreenWithoutRelease 覆盖没有当前版本：空事实、不报错。
// 轮次记录存在也不返回：空态由查询层统一表达成 release:null + empty:true（Requirement 33.8）。
func TestScreenWithoutRelease(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", screenFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, status, term) VALUES ($1, 'failed', $2)`,
		freshnessAt(t, "2026-10-27T01:00:00Z"), screenFixtureTerm)

	facts, err := Screen(ctx, pool)
	if err != nil {
		t.Fatalf("没有当前版本不该报错: %v", err)
	}
	if facts.Release != nil || facts.Published != nil || facts.LastRun != nil {
		t.Fatalf("无当前版本时应给空事实: %+v", facts)
	}
	if facts.Occupancy == nil || len(facts.Occupancy) != 0 {
		t.Fatalf("无当前版本时占用应为空数组，实际 %v", facts.Occupancy)
	}
}

// TestScreenWithoutPublishingRun 覆盖「版本在、产出轮次查不到」：
// 发布轮次与窗口留空，不把没有窗口的数据编成窗口。
func TestScreenWithoutPublishingRun(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", screenFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ('screen-release-cur', $1, now(), 1, 1, true)`, screenFixtureTerm)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name) VALUES ('s101', 'F101')`)
	mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ('screen-release-cur', 's101', 'Q10', '综合教学楼')`)

	facts, err := Screen(ctx, pool)
	if err != nil {
		t.Fatalf("读取大屏事实失败: %v", err)
	}
	if facts.Release == nil || facts.Release.Rooms != 1 {
		t.Fatalf("版本与规模不符: %+v", facts.Release)
	}
	if facts.Published != nil {
		t.Fatalf("查不到产出轮次时应为 nil，实际 %+v", facts.Published)
	}
	if facts.LastRun != nil {
		t.Fatalf("没有轮次时 last_run 应为 nil，实际 %+v", facts.LastRun)
	}
}
