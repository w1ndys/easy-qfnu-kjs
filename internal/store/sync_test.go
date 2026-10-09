package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。
// 覆盖 0004 迁移建出的结构、教学楼白名单读法、房间所属楼写入与占用明细的列范围。

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// insertTestTask 是插入同步任务的固定语句，形状相关的用例共用同一组列。
const insertTestTask = `
INSERT INTO sync_task (run_id, kind, building_id, week, room_id, weekday, block, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// TestSyncTaskRejectsWrongShape 断言两类任务的列形状与取值组合由库约束固定。
func TestSyncTaskRejectsWrongShape(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	runID := insertTestRun(t, s)
	roomID := createTestRoom(t, s, model.Room{ID: "it-shape-room", Name: "综合教学楼101", BuildingID: "jxlbh-1", BuildingName: "综合楼"})

	// 每种非法组合都必须被库拒绝：状态机靠这些约束才敢按形状取续传单元
	rejected := []struct {
		name string // 用例说明
		args []any  // 插入参数
	}{
		{"未知 kind", []any{runID, "aggregate", "jxlbh-1", 1, nil, nil, nil, "pending"}},
		{"未知 status", []any{runID, "week_matrix", "jxlbh-1", 1, nil, nil, nil, "running"}},
		{"周矩阵任务缺周次", []any{runID, "week_matrix", "jxlbh-1", nil, nil, nil, nil, "pending"}},
		{"周矩阵任务带明细列", []any{runID, "week_matrix", "jxlbh-1", 1, roomID, 1, "0102", "pending"}},
		{"明细任务带周次", []any{runID, "cell_detail", "jxlbh-1", 1, roomID, 1, "0102", "pending"}},
		{"明细任务缺大节", []any{runID, "cell_detail", "jxlbh-1", nil, roomID, 1, nil, "pending"}},
		{"星期越界", []any{runID, "cell_detail", "jxlbh-1", nil, roomID, 8, "0102", "pending"}},
	}

	for _, testCase := range rejected {
		// 房间外键有效且整行参数齐全，所以只有约束能让这些插入失败
		if _, err := s.pool.Exec(ctx, insertTestTask, testCase.args...); err == nil {
			t.Errorf("%s 竟然写入成功，约束未生效", testCase.name)
		}
	}
}

// TestSyncTaskAllowsBothKindsAndOnePerCell 断言两类正常任务能写入，且同一单元不会重复建任务。
func TestSyncTaskAllowsBothKindsAndOnePerCell(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	runID := insertTestRun(t, s)
	roomID := createTestRoom(t, s, model.Room{ID: "it-dup-room", Name: "综合教学楼102", BuildingID: "jxlbh-1", BuildingName: "综合楼"})

	weekTask := []any{runID, "week_matrix", "jxlbh-1", 1, nil, nil, nil, "pending"}
	cellTask := []any{runID, "cell_detail", "jxlbh-1", nil, roomID, 3, "0102", "pending"}

	// 两类正常任务各写一条，确认约束没有把状态机要用的形状一起拦下
	for _, args := range [][]any{weekTask, cellTask} {
		if _, err := s.pool.Exec(ctx, insertTestTask, args...); err != nil {
			t.Fatalf("正常任务写入失败: %v", err)
		}
	}

	// 同一轮同一栋楼的同一周只该有一个周矩阵任务，重复建任务要被唯一索引拦下
	if _, err := s.pool.Exec(ctx, insertTestTask, weekTask...); err == nil {
		t.Error("同一周矩阵任务写入两次成功，唯一索引未生效")
	}
	// 同一轮同一栋楼的同一个格子同样只该有一个明细任务
	if _, err := s.pool.Exec(ctx, insertTestTask, cellTask...); err == nil {
		t.Error("同一明细任务写入两次成功，唯一索引未生效")
	}
}

// TestSyncTaskColumns 断言同步任务表带齐状态机要用的列，缺一列就没法断点续传。
func TestSyncTaskColumns(t *testing.T) {
	s := openMigratedStore(t)

	expected := []string{
		"task_id", "run_id", "kind", "building_id", "week",
		"room_id", "weekday", "block", "status", "attempt_count", "error_message",
	}
	checkExactColumns(t, s, "sync_task", expected)
}

// TestSyncMigrationAddsRoomBuildingColumns 断言 0004 给 room 补上了所属楼两列。
func TestSyncMigrationAddsRoomBuildingColumns(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()

	columns := tableColumns(t, s, "room")
	for _, name := range []string{"building_id", "building_name"} {
		if !columns[name] {
			t.Errorf("room 缺少列 %s，0004 迁移未生效", name)
			continue
		}
		// 存量房间还没有楼，两列必须可空，否则下一轮写入直接失败
		if !columnIsNullable(t, s, "room", name) {
			t.Errorf("room.%s 不该是 NOT NULL", name)
		}
	}

	// 房间仍按 jsbh 唯一，补列不该改掉主键语义
	var roomID string
	if err := s.pool.QueryRow(ctx, `
SELECT column_name
  FROM information_schema.key_column_usage
 WHERE table_name = 'room' AND constraint_name = 'room_pkey'`).Scan(&roomID); err != nil {
		t.Fatalf("读取 room 主键失败: %v", err)
	}
	if roomID != "room_id" {
		t.Errorf("room 主键 = %s，期望 room_id", roomID)
	}
}

// TestOccupancyDetailColumnsExcludeApplicant 断言明细表只有约定的这些列。
// 列集合是精确比较：申请人、任课教师与原始 HTML 都不进库，多一列就说明这些内容有地方可落。
func TestOccupancyDetailColumnsExcludeApplicant(t *testing.T) {
	s := openMigratedStore(t)

	expected := []string{
		"detail_id", "release_id", "room_id", "weekday", "block",
		"state_key", "course", "week_range", "time_flag", "derived",
	}
	checkExactColumns(t, s, "occupancy_detail", expected)
}

// TestBuildingWhitelist 断言白名单读法：键不存在、空数组与非法 JSON 都退回空名单，
// 缺 jxlbh 的条目被丢掉（空 jxlbh 会被上游当成「不限楼」）。
func TestBuildingWhitelist(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	// 白名单是全局唯一的一行配置，先记住原值，测完还原，避免污染开发库
	restoreWhitelist(t, s)

	cases := []struct {
		name   string   // 用例说明
		value  string   // 写入 settings 的原始值
		expect []string // 期望读出的 jxlbh
	}{
		{"空数组", `[]`, nil},
		{"非法 JSON", `[{"jxlbh":`, nil},
		{"不是数组", `{"jxlbh":"jxlbh-1"}`, nil},
		{"正常两条", `[{"jxlbh":"jxlbh-1","name":"综合楼"},{"jxlbh":"jxlbh-2","name":"实验楼"}]`, []string{"jxlbh-1", "jxlbh-2"}},
		{"缺 jxlbh 的条目被丢掉", `[{"name":"综合楼"},{"jxlbh":"jxlbh-3","name":"实验楼"}]`, []string{"jxlbh-3"}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			writeWhitelist(t, s, testCase.value)
			if got := whitelistCodes(t, s); !slices.Equal(got, testCase.expect) {
				t.Errorf("白名单 jxlbh = %v，期望 %v", got, testCase.expect)
			}
		})
	}

	// 管理员还没配过白名单时读不到行，也要当成空名单，而不是报错或放行全校请求
	t.Run("键不存在", func(t *testing.T) {
		if _, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key = 'building_whitelist'`); err != nil {
			t.Fatalf("删除白名单失败: %v", err)
		}
		if codes := whitelistCodes(t, s); len(codes) != 0 {
			t.Errorf("键不存在时读到 %v，期望空名单", codes)
		}
	})
}

// TestUpsertRoomBuildingOwner 断言同一 jsbh 不会被改写成另一栋楼，空楼编号也不清已有归属。
func TestUpsertRoomBuildingOwner(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	room := model.Room{ID: "it-owner-room", Name: "综合教学楼101", BuildingID: "jxlbh-1", BuildingName: "综合楼"}
	createTestRoom(t, s, room)

	// 同一个 jsbh 出现在另一栋楼：本轮失败，不允许挑一栋覆盖
	conflict := model.Room{ID: room.ID, Name: "实验楼201", BuildingID: "jxlbh-2", BuildingName: "实验楼"}
	if err := s.UpsertRoom(ctx, conflict); !errors.Is(err, ErrRoomBuildingConflict) {
		t.Fatalf("跨楼写入返回 %v，期望 ErrRoomBuildingConflict", err)
	}
	// 被拒绝的写入不能留下痕迹：库里仍是最早那栋楼与最早那次房名
	buildingID, name := readRoom(t, s, room.ID)
	if buildingID != room.BuildingID || name != room.Name {
		t.Errorf("冲突写入后 room = (%s, %s)，期望 (%s, %s)", buildingID, name, room.BuildingID, room.Name)
	}

	// 同一栋楼的重复写入是正常续传，房名更新成最近一次见到的
	again := model.Room{ID: room.ID, Name: "综合教学楼103", BuildingID: "jxlbh-1", BuildingName: "综合楼"}
	if err := s.UpsertRoom(ctx, again); err != nil {
		t.Fatalf("同楼重复写入失败: %v", err)
	}
	// 没带楼信息的写入只更新房名，不清掉已经记下的归属
	withoutBuilding := model.Room{ID: room.ID, Name: "综合教学楼104"}
	if err := s.UpsertRoom(ctx, withoutBuilding); err != nil {
		t.Fatalf("不带楼写入失败: %v", err)
	}
	buildingID, name = readRoom(t, s, room.ID)
	if buildingID != room.BuildingID || name != withoutBuilding.Name {
		t.Errorf("无楼写入后 room = (%s, %s)，期望 (%s, %s)", buildingID, name, room.BuildingID, withoutBuilding.Name)
	}
}

// openMigratedStore 打开测试库并把迁移跑到最新，是这些用例的共同起点。
func openMigratedStore(t *testing.T) *Store {
	t.Helper()
	s := openTestStore(t)
	// 用例不依赖先手工建库，每条都自己跑到最新版本
	if _, err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}
	return s
}

// insertTestRun 建一轮采集运行，供同步任务的外键使用。
func insertTestRun(t *testing.T, s *Store) int64 {
	t.Helper()

	var runID int64
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO collect_run (started_at, status) VALUES (now(), 'running') RETURNING run_id`).Scan(&runID); err != nil {
		t.Fatalf("插入测试轮次失败: %v", err)
	}
	t.Cleanup(func() {
		// 同步任务随轮次级联删除，删掉轮次就够了
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM collect_run WHERE run_id = $1`, runID); err != nil {
			t.Logf("清理 collect_run 失败: %v", err)
		}
	})
	return runID
}

// createTestRoom 建一间测试房并登记所属楼，测试结束按外键顺序清理。
func createTestRoom(t *testing.T, s *Store, room model.Room) string {
	t.Helper()
	if err := s.UpsertRoom(context.Background(), room); err != nil {
		t.Fatalf("插入测试房间失败: %v", err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		// 明细与任务都引用房间，先清掉它们才能删掉房间
		if _, err := s.pool.Exec(ctx, `DELETE FROM occupancy_detail WHERE room_id = $1`, room.ID); err != nil {
			t.Logf("清理 occupancy_detail 失败: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM sync_task WHERE room_id = $1`, room.ID); err != nil {
			t.Logf("清理 sync_task 失败: %v", err)
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM room WHERE room_id = $1`, room.ID); err != nil {
			t.Logf("清理 room 失败: %v", err)
		}
	})
	return room.ID
}

// readRoom 回读一个房间的所属楼与展示名。
func readRoom(t *testing.T, s *Store, roomID string) (string, string) {
	t.Helper()

	var buildingID, name string
	// 楼可能还没写过，取空串让断言能直接比较
	if err := s.pool.QueryRow(context.Background(),
		`SELECT COALESCE(building_id, ''), name FROM room WHERE room_id = $1`, roomID).Scan(&buildingID, &name); err != nil {
		t.Fatalf("回读房间失败: %v", err)
	}
	return buildingID, name
}

// restoreWhitelist 记住白名单键的原值，测试结束还原，避免集成测试污染开发库。
func restoreWhitelist(t *testing.T, s *Store) {
	t.Helper()

	var previous string
	err := s.pool.QueryRow(context.Background(),
		`SELECT value FROM settings WHERE key = 'building_whitelist'`).Scan(&previous)
	// 键没配过时读出 ErrNoRows，说明结束时要把这一行删掉而不是写回空串
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("读取白名单原值失败: %v", err)
	}
	existed := err == nil

	t.Cleanup(func() {
		ctx := context.Background()
		if !existed {
			if _, err := s.pool.Exec(ctx, `DELETE FROM settings WHERE key = 'building_whitelist'`); err != nil {
				t.Logf("清理白名单失败: %v", err)
			}
			return
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE settings SET value = $1, updated_at = now() WHERE key = 'building_whitelist'`, previous); err != nil {
			t.Logf("还原白名单失败: %v", err)
		}
	})
}

// writeWhitelist 覆盖白名单配置值。
func writeWhitelist(t *testing.T, s *Store, value string) {
	t.Helper()

	// 键可能还不存在，所以用 upsert 而不是 UPDATE
	if _, err := s.pool.Exec(context.Background(), `
INSERT INTO settings (key, value) VALUES ('building_whitelist', $1)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, value); err != nil {
		t.Fatalf("写入白名单失败: %v", err)
	}
}

// whitelistCodes 读出白名单，只留 jxlbh 方便按序列断言。
func whitelistCodes(t *testing.T, s *Store) []string {
	t.Helper()

	buildings, err := s.BuildingWhitelist(context.Background())
	if err != nil {
		t.Fatalf("读取白名单失败: %v", err)
	}
	codes := make([]string, 0, len(buildings))
	for _, building := range buildings {
		codes = append(codes, building.Jxlbh)
	}
	return codes
}

// checkExactColumns 断言表里的列恰好等于期望集合：多一列就可能多存了不该存的东西。
func checkExactColumns(t *testing.T, s *Store, table string, expected []string) {
	t.Helper()
	columns := tableColumns(t, s, table)

	for _, name := range expected {
		if !columns[name] {
			t.Errorf("%s 缺少列 %s", table, name)
		}
	}
	for name := range columns {
		// 多出来的列不在约定范围内，明细表出现申请人这类列会在这里暴露
		if !slices.Contains(expected, name) {
			t.Errorf("%s 多出列 %s，期望列集合 %v", table, name, expected)
		}
	}
}

// tableColumns 读出表的列名集合。
func tableColumns(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()

	rows, err := s.pool.Query(context.Background(), `
SELECT column_name
  FROM information_schema.columns
 WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("读取 %s 的列失败: %v", table, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("扫描列名失败: %v", err)
		}
		columns[name] = true
	}
	// 遍历中途出错时当成读完，会把缺列判成通过
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历列名失败: %v", err)
	}
	return columns
}

// columnIsNullable 判断某列是否允许为空。
func columnIsNullable(t *testing.T, s *Store, table, column string) bool {
	t.Helper()

	var nullable string
	if err := s.pool.QueryRow(context.Background(), `
SELECT is_nullable
  FROM information_schema.columns
 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`, table, column).Scan(&nullable); err != nil {
		t.Fatalf("读取 %s.%s 的可空属性失败: %v", table, column, err)
	}
	return nullable == "YES"
}
