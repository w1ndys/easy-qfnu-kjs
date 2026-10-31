package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dayFixtureRelease 是全天状态夹具使用的发布版本。
const dayFixtureRelease = "day-release-1"

// seedDayFixture 造一套覆盖缺失节点、占用节点与归属快照的观测。
// 观察窗口固定在第 5 周周一（2026-10-19），第 6 周故意不放任何观测。
func seedDayFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", availabilityFixtureTerm, 20)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, now(), 1, 1, true)`, dayFixtureRelease, availabilityFixtureTerm)

	rooms := []struct {
		id           string
		name         string
		buildingID   string
		buildingName string
		snapshot     bool
	}{
		{id: "d101", name: "D101", buildingID: "Q10", buildingName: "综合教学楼", snapshot: true},
		// d102 只在教室目录里，没有进入发布版本的归属快照。
		{id: "d102", name: "D102", buildingID: "Q10", buildingName: "综合教学楼", snapshot: false},
		{id: "d103", name: "D103", buildingID: "Q11", buildingName: "实验楼", snapshot: true},
	}
	for _, room := range rooms {
		mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ($1, $2, $2, $3, $4)`, room.id, room.name, room.buildingID, room.buildingName)
		if room.snapshot {
			mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, $2, $3, $4)`, dayFixtureRelease, room.id, room.buildingID, room.buildingName)
		}
	}

	// d101：01 空闲、02 上课、06 未知，其余小节没有观测。
	seedDayObservation(ctx, t, pool, "d101", "01", "free", true)
	seedDayObservation(ctx, t, pool, "d101", "02", "class", false)
	seedDayObservation(ctx, t, pool, "d101", "06", "unknown", false)

	// d103：另一栋楼的一间，只有 01 节有观测。
	seedDayObservation(ctx, t, pool, "d103", "01", "free", true)
}

// seedDayObservation 写一格第 5 周周一的观测。
func seedDayObservation(ctx context.Context, t *testing.T, pool *pgxpool.Pool, roomID, node, stateKey string, available bool) {
	t.Helper()

	mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available, raw_text)
VALUES ($1, $2, $3, 5, 1, $4, $5, $6, NULL)`,
		dayFixtureRelease, roomID, availabilityFixtureTerm, node, stateKey, available)
}

// dayFilter 构造第 5 周周一的全天状态查询。
func dayFilter(roomID, building string) DayFilter {
	return DayFilter{
		Term:     availabilityFixtureTerm,
		Week:     5,
		Weekday:  1,
		RoomID:   roomID,
		Building: building,
	}
}

// TestDayStatusReturnsTwelveNodesWithMissingMarked 覆盖 Requirement 16.1–16.2：
// 房间该日返回轴上的 12 小节，缺观测的小节 HasData=false 而不是报错。
func TestDayStatusReturnsTwelveNodesWithMissingMarked(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedDayFixture(ctx, t, pool)

	page, err := DayStatus(ctx, pool, dayFilter("d101", ""))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !page.HasRelease || !page.WeekHasObservation {
		t.Fatalf("空态标记不符: %+v", page)
	}
	if page.Room == nil || page.Room.ID != "d101" || page.Room.BuildingID != "Q10" ||
		page.Room.BuildingName != "综合教学楼" || page.Room.Name != "D101" {
		t.Fatalf("房间与归属不符: %+v", page.Room)
	}
	if len(page.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(page.Nodes))
	}

	// 01：空闲；02：上课；03：没有观测，必须保留在结果里并标为无数据。
	if page.Nodes[0].Node != "01" || page.Nodes[0].StateKey != "free" ||
		page.Nodes[0].Label != "空闲" || !page.Nodes[0].Available || !page.Nodes[0].HasData {
		t.Fatalf("01 节字段不符: %+v", page.Nodes[0])
	}
	if page.Nodes[1].StateKey != "class" || page.Nodes[1].Label != "正常上课" ||
		page.Nodes[1].Available || !page.Nodes[1].HasData {
		t.Fatalf("02 节字段不符: %+v", page.Nodes[1])
	}
	if page.Nodes[2].Node != "03" || page.Nodes[2].HasData || page.Nodes[2].StateKey != "" ||
		page.Nodes[2].Available {
		t.Fatalf("03 节应为无数据: %+v", page.Nodes[2])
	}
	// 06：未知状态，不可用但仍算有观测。
	if page.Nodes[5].StateKey != "unknown" || page.Nodes[5].Available || !page.Nodes[5].HasData {
		t.Fatalf("06 节字段不符: %+v", page.Nodes[5])
	}

	// 轴顺序与所属大节必须来自 axis_node，不能按 5 大节折叠。
	for i, node := range page.Nodes {
		wantNode := fmt.Sprintf("%02d", i+1)
		if node.Node != wantNode {
			t.Fatalf("第 %d 个节点期望 %s，实际 %s", i, wantNode, node.Node)
		}
	}
	if page.Nodes[2].Block != "030405" || page.Nodes[9].Block != "101112" {
		t.Fatalf("大节归属不符: %+v %+v", page.Nodes[2], page.Nodes[9])
	}
}

// TestDayStatusBuildingFilterUsesSnapshot 覆盖 Requirement 16.3：
// 楼的筛选与空教室共用归属快照；不属于所选楼或没有快照时按空态处理，不返回别的楼的数据。
func TestDayStatusBuildingFilterUsesSnapshot(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedDayFixture(ctx, t, pool)

	cases := []struct {
		name      string
		roomID    string
		building  string
		wantFound bool
		wantBld   string
	}{
		{name: "全部命中带快照的房间", roomID: "d101", building: "", wantFound: true, wantBld: "Q10"},
		{name: "所选楼与归属一致", roomID: "d101", building: "Q10", wantFound: true, wantBld: "Q10"},
		{name: "所选楼与归属不一致", roomID: "d101", building: "Q11", wantFound: false},
		{name: "没有归属快照的房间", roomID: "d102", building: "", wantFound: false},
		{name: "没有快照且指定了楼", roomID: "d102", building: "Q10", wantFound: false},
		{name: "另一栋楼的房间被 Q10 排除", roomID: "d103", building: "Q10", wantFound: false},
		{name: "另一栋楼的房间被 Q11 命中", roomID: "d103", building: "Q11", wantFound: true, wantBld: "Q11"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			page, err := DayStatus(ctx, pool, dayFilter(c.roomID, c.building))
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if !c.wantFound {
				if page.Room != nil {
					t.Fatalf("应查不到房间，实际 %+v", page.Room)
				}
				return
			}
			if page.Room == nil || page.Room.BuildingID != c.wantBld {
				t.Fatalf("期望归属 %s，实际 %+v", c.wantBld, page.Room)
			}
		})
	}
}

// TestDayStatusNoRelease 覆盖没有当前版本：不分页、不报错，只留空态标记。
func TestDayStatusNoRelease(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedDayFixture(ctx, t, pool)
	mustExec(ctx, t, pool, "UPDATE release SET is_current = false WHERE release_id = $1", dayFixtureRelease)

	page, err := DayStatus(ctx, pool, dayFilter("d101", ""))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.HasRelease || page.WeekHasObservation || page.Room != nil || len(page.Nodes) != 0 {
		t.Fatalf("无版本时应是空态，实际 %+v", page)
	}
}

// TestDayStatusWeekWithoutObservation 覆盖该周没有任何观测：房间仍可归属，但每节都无数据。
func TestDayStatusWeekWithoutObservation(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedDayFixture(ctx, t, pool)

	filter := dayFilter("d101", "")
	filter.Week = 6
	page, err := DayStatus(ctx, pool, filter)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !page.HasRelease || page.WeekHasObservation {
		t.Fatalf("该周无观测时标记不符: %+v", page)
	}
	if page.Room == nil || page.Room.ID != "d101" {
		t.Fatalf("房间仍应可归属: %+v", page.Room)
	}
	if len(page.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(page.Nodes))
	}
	for _, node := range page.Nodes {
		if node.HasData || node.StateKey != "" || node.Available {
			t.Fatalf("该周无观测时每节都应无数据，实际 %+v", node)
		}
	}
}

// TestDayStatusUnknownRoom 覆盖房号不存在：不返回错误，Room 为 nil 交给调用方输出空态。
func TestDayStatusUnknownRoom(t *testing.T) {
	ctx, pool := openAvailabilityTestDB(t)
	seedDayFixture(ctx, t, pool)

	page, err := DayStatus(ctx, pool, dayFilter("not-exist", ""))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !page.HasRelease || !page.WeekHasObservation || page.Room != nil || len(page.Nodes) != 0 {
		t.Fatalf("未知房号应是空态，实际 %+v", page)
	}
}
