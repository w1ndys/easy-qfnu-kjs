package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// attrConflictAt 是冲突回写用例的固定时刻，避免读真实时间。
var attrConflictAt = time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))

// TestUpsertRoomAttributionAgainstRealDB 只在提供 KJS_TEST_DSN 时运行，
// 用真实 PostgreSQL 验 room 目录的幂等 upsert：同一份归属重放不新增行、不改值，
// 同批重复房号只留首次出现。
func TestUpsertRoomAttributionAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	first := RoomAttribution{
		BuildingID: "Q10",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "F101", NameRaw: "F101(60/0)"},
			{RoomID: "r102", Name: "F102", NameRaw: "F102"},
			{RoomID: "r101", Name: "重复行", NameRaw: "重复行"},
		},
	}
	if err := UpsertRoomAttribution(ctx, pool, first); err != nil {
		t.Fatalf("首次写入归属失败: %v", err)
	}

	rooms := readRooms(ctx, t, pool)
	if len(rooms) != 2 {
		t.Fatalf("应写入 2 间房，实际 %+v", rooms)
	}
	if got := rooms["r101"]; got != (roomRow{Name: "F101", NameRaw: "F101(60/0)", BuildingID: "Q10"}) {
		t.Fatalf("r101 首次归属不符: %+v", got)
	}
	if got := rooms["r102"]; got != (roomRow{Name: "F102", NameRaw: "F102", BuildingID: "Q10"}) {
		t.Fatalf("r102 首次归属不符: %+v", got)
	}

	// 同一份归属重放：幂等，不新增行、不改值。
	if err := UpsertRoomAttribution(ctx, pool, first); err != nil {
		t.Fatalf("重放归属失败: %v", err)
	}
	rooms = readRooms(ctx, t, pool)
	if len(rooms) != 2 {
		t.Fatalf("重放不应新增房间，实际 %+v", rooms)
	}
	if got := rooms["r101"]; got != (roomRow{Name: "F101", NameRaw: "F101(60/0)", BuildingID: "Q10"}) {
		t.Fatalf("重放不应改写归属，实际 %+v", got)
	}
}

// TestUpsertRoomAttributionOverwritesBuildingAcrossRoundsAgainstRealDB 只在提供
// KJS_TEST_DSN 时运行，覆盖用户拍板的「允许跨轮换楼覆盖」：同一房号在新一轮换到
// 另一栋楼时 room.building_id 直接被新楼覆盖、不报错，房名一起更新；
// 轮内的首个归属不靠这一列，仍由 collect_run_room 的当轮快照保住。
func TestUpsertRoomAttributionOverwritesBuildingAcrossRoundsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	mustExec(ctx, t, pool, `
INSERT INTO term (term, total_weeks) VALUES ('2026-2027-1', 20)`)
	runID := seedRunRoomRun(ctx, t, pool, "2026-2027-1")

	// 第一轮：r101、r102 归 Q10，并把本轮归属写进每轮快照。
	if err := UpsertRoomAttribution(ctx, pool, RoomAttribution{
		BuildingID: "Q10",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "F101", NameRaw: "F101(60/0)"},
			{RoomID: "r102", Name: "F102", NameRaw: "F102"},
		},
	}); err != nil {
		t.Fatalf("第一轮写入归属失败: %v", err)
	}
	if err := UpsertRunRooms(ctx, pool, runID, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q10"},
	}, attrConflictAt); err != nil {
		t.Fatalf("第一轮写入每轮归属失败: %v", err)
	}

	// 第二轮：同一房号改归 Q11。跨轮换楼是正常数据，覆盖成功、不报 ErrRunStoreFailed。
	if err := UpsertRoomAttribution(ctx, pool, RoomAttribution{
		BuildingID: "Q11",
		Rooms:      []AttributedRoom{{RoomID: "r101", Name: "F101-新", NameRaw: "F101(新)"}},
	}); err != nil {
		t.Fatalf("跨轮换楼应正常覆盖，实际 %v", err)
	}

	rooms := readRooms(ctx, t, pool)
	if len(rooms) != 2 {
		t.Fatalf("覆盖不应改行数，实际 %+v", rooms)
	}
	if got := rooms["r101"]; got != (roomRow{Name: "F101-新", NameRaw: "F101(新)", BuildingID: "Q11"}) {
		t.Fatalf("r101 应被新轮覆盖成 Q11 且房名一起更新，实际 %+v", got)
	}
	if got := rooms["r102"]; got != (roomRow{Name: "F102", NameRaw: "F102", BuildingID: "Q10"}) {
		t.Fatalf("本批没涉及的 r102 不应被改写: %+v", got)
	}

	// 轮内首归属仍以当轮快照为准：续跑重建累计器读到的还是 Q10。
	assertRunRooms(ctx, t, pool, runID, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q10"},
	})

	// 再换回 Q10 同样允许：覆盖没有方向限制，也不受上一轮归属牵制。
	if err := UpsertRoomAttribution(ctx, pool, RoomAttribution{
		BuildingID: "Q10",
		Rooms:      []AttributedRoom{{RoomID: "r101", Name: "F101", NameRaw: "F101(60/0)"}},
	}); err != nil {
		t.Fatalf("再换回原楼也应成功，实际 %v", err)
	}
	if got := readRooms(ctx, t, pool)["r101"]; got.BuildingID != "Q10" {
		t.Fatalf("r101 应换回 Q10，实际 %+v", got)
	}
}

// TestUpsertRoomAttributionFillsNullBuildingAgainstRealDB 只在提供 KJS_TEST_DSN 时运行：
// 覆盖「room.building_id IS NULL 补归属」这条路径。目录里已存在房号但还没归属
// （旧库数据，或别的链路先建了目录行）时，本轮归属必须能补上而不是被当成新行。
func TestUpsertRoomAttributionFillsNullBuildingAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	// 先插一条未归属的目录行：building_id 为 NULL。
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id) VALUES ('r101', '旧名', '旧名', NULL)`)

	if err := UpsertRoomAttribution(ctx, pool, RoomAttribution{
		BuildingID: "Q10",
		Rooms:      []AttributedRoom{{RoomID: "r101", Name: "F101", NameRaw: "F101(60/0)"}},
	}); err != nil {
		t.Fatalf("未归属的房号应允许补上归属，实际 %v", err)
	}

	rooms := readRooms(ctx, t, pool)
	if len(rooms) != 1 {
		t.Fatalf("补归属不应新增行，实际 %+v", rooms)
	}
	if got := rooms["r101"]; got != (roomRow{Name: "F101", NameRaw: "F101(60/0)", BuildingID: "Q10"}) {
		t.Fatalf("r101 补归属后应为 Q10 且房名更新，实际 %+v", got)
	}
}

// TestRecordAttrConflictsAgainstRealDB 只在提供 KJS_TEST_DSN 时运行，
// 验证冲突数只增不减、重复回写不累加，以及轮次不存在时报错。
func TestRecordAttrConflictsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	var runID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status)
VALUES (now(), 'running')
RETURNING run_id`).Scan(&runID); err != nil {
		t.Fatalf("创建轮次失败: %v", err)
	}

	// 同一个累计值重复回写（重放/续跑）不会累加。
	for attempt := 0; attempt < 2; attempt++ {
		if err := RecordAttrConflicts(ctx, pool, runID, 1, attrConflictAt); err != nil {
			t.Fatalf("回写冲突数失败: %v", err)
		}
	}
	if got := readAttrConflicts(ctx, t, pool, runID); got != 1 {
		t.Fatalf("重复回写后冲突数应为 1，实际 %d", got)
	}

	// 新冲突把计数抬到 2；再写一次更小的旧值不能把它改小。
	if err := RecordAttrConflicts(ctx, pool, runID, 2, attrConflictAt); err != nil {
		t.Fatalf("回写新冲突数失败: %v", err)
	}
	if err := RecordAttrConflicts(ctx, pool, runID, 1, attrConflictAt); err != nil {
		t.Fatalf("回写旧冲突数失败: %v", err)
	}
	if got := readAttrConflicts(ctx, t, pool, runID); got != 2 {
		t.Fatalf("冲突数应保持 2，实际 %d", got)
	}

	// 轮次不存在：影响 0 行，按 ErrRunStoreFailed 上抛。
	if err := RecordAttrConflicts(ctx, pool, runID+100000, 1, attrConflictAt); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在时应报 ErrRunStoreFailed，实际 %v", err)
	}
}

// roomRow 是真实库里一间房的归属三列，便于整行比较。
type roomRow struct {
	Name       string
	NameRaw    string
	BuildingID string
}

// readRooms 读 room 目录的全部房号；未归属的楼编号折成空串，避免 NULL 扫进 string 报错。
func readRooms(ctx context.Context, t *testing.T, pool *pgxpool.Pool) map[string]roomRow {
	t.Helper()

	rows, err := pool.Query(ctx, `
SELECT room_id, name, COALESCE(name_raw, ''), COALESCE(building_id, '')
  FROM room`)
	if err != nil {
		t.Fatalf("读取 room 失败: %v", err)
	}
	defer rows.Close()

	rooms := make(map[string]roomRow)
	for rows.Next() {
		var roomID string
		var room roomRow
		if err := rows.Scan(&roomID, &room.Name, &room.NameRaw, &room.BuildingID); err != nil {
			t.Fatalf("扫描 room 失败: %v", err)
		}
		rooms[roomID] = room
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 room 失败: %v", err)
	}
	return rooms
}

// readAttrConflicts 读轮次的归属冲突数。
func readAttrConflicts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) int {
	t.Helper()

	var conflicts int
	if err := pool.QueryRow(ctx, `SELECT attr_conflict_count FROM collect_run WHERE run_id = $1`, runID).Scan(&conflicts); err != nil {
		t.Fatalf("读取冲突数失败: %v", err)
	}
	return conflicts
}

// seedRunRoomRun 造一条带学期的非 running 轮次，供每轮归属用例使用。
// 用 failed 而不是 running，避免同库多轮时撞 collect_run_single_running。
func seedRunRoomRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, term string) int64 {
	t.Helper()

	var runID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term)
VALUES (now(), 'failed', $1)
RETURNING run_id`, term).Scan(&runID); err != nil {
		t.Fatalf("创建轮次失败: %v", err)
	}
	return runID
}

// TestUpsertAndLoadRunRoomsAgainstRealDB 只在提供 KJS_TEST_DSN 时运行：
// 验证每轮归属快照的幂等写入、冲突标记保留、轮内不换楼与跨轮隔离。
func TestUpsertAndLoadRunRoomsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	mustExec(ctx, t, pool, `
INSERT INTO term (term, total_weeks) VALUES ('2026-2027-1', 20) ON CONFLICT (term) DO NOTHING`)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id) VALUES
  ('r101', 'F101', 'F101', 'Q10'),
  ('r102', 'F102', 'F102', 'Q10'),
  ('r201', 'F201', 'F201', 'Q11')
ON CONFLICT (room_id) DO NOTHING`)

	firstRun := seedRunRoomRun(ctx, t, pool, "2026-2027-1")
	secondRun := seedRunRoomRun(ctx, t, pool, "2026-2027-1")

	if err := UpsertRunRooms(ctx, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q10"},
	}, attrConflictAt); err != nil {
		t.Fatalf("首次写每轮归属失败: %v", err)
	}
	assertRunRooms(ctx, t, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q10"},
	})

	// 冲突只打在有效归属那一行上：楼不变，标记与第一次被丢弃的楼落库。
	if err := UpsertRunRooms(ctx, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
	}, attrConflictAt); err != nil {
		t.Fatalf("写冲突标记失败: %v", err)
	}
	// 重放 Accept 不能让冲突标记退回 false。
	if err := UpsertRunRooms(ctx, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
	}, attrConflictAt); err != nil {
		t.Fatalf("重放归属失败: %v", err)
	}
	assertRunRooms(ctx, t, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		{RoomID: "r102", BuildingID: "Q10"},
	})

	// 轮内已定归属不被改写：即便调用方再报另一栋楼，SQL 也只保留首次归属。
	if err := UpsertRunRooms(ctx, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q11"},
	}, attrConflictAt); err != nil {
		t.Fatalf("写冲突标记失败: %v", err)
	}
	assertRunRooms(ctx, t, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		{RoomID: "r102", BuildingID: "Q10"},
	})

	// 跨轮隔离：下一轮同一房号可以换楼，第一轮的快照不受影响。
	if err := UpsertRunRooms(ctx, pool, secondRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q11"},
	}, attrConflictAt); err != nil {
		t.Fatalf("下一轮写归属失败: %v", err)
	}
	assertRunRooms(ctx, t, pool, firstRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		{RoomID: "r102", BuildingID: "Q10"},
	})
	assertRunRooms(ctx, t, pool, secondRun, []RunRoom{
		{RoomID: "r101", BuildingID: "Q11"},
	})

	// 轮次不存在：影响 0 行，按 ErrRunStoreFailed 上抛。
	if err := UpsertRunRooms(ctx, pool, firstRun+100000, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
	}, attrConflictAt); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在时应报 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestUpsertRunRoomsRejectsRunWithoutTermAgainstRealDB 只在提供 KJS_TEST_DSN 时运行：
// 轮次缺学期时 SQL 影响 0 行并返回可读错误，不撞 collect_run_room.term 的 NOT NULL。
func TestUpsertRunRoomsRejectsRunWithoutTermAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id) VALUES ('r101', 'F101', 'F101', 'Q10')`)
	runID := insertRunningCollectRun(ctx, t, pool, attrConflictAt)

	err := UpsertRunRooms(ctx, pool, runID, []RunRoom{{RoomID: "r101", BuildingID: "Q10"}}, attrConflictAt)
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("缺学期时应报 ErrRunStoreFailed，实际 %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM collect_run_room WHERE run_id = $1", runID).Scan(&rows); err != nil {
		t.Fatalf("统计每轮归属失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("缺学期不应留下每轮归属行，实际 %d 行", rows)
	}
}

// assertRunRooms 读回一轮归属并整表比较；LoadRunRooms 按 room_id 排序。
func assertRunRooms(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64, want []RunRoom) {
	t.Helper()

	rooms, err := LoadRunRooms(ctx, pool, runID)
	if err != nil {
		t.Fatalf("读取每轮归属失败: %v", err)
	}
	if len(rooms) != len(want) {
		t.Fatalf("轮次 %d 的归属期望 %+v，实际 %+v", runID, want, rooms)
	}
	for index, room := range rooms {
		if room != want[index] {
			t.Fatalf("轮次 %d 第 %d 条归属期望 %+v，实际 %+v", runID, index+1, want[index], room)
		}
	}
}
