package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 测试夹具里的三间房：a 房 01–02 空闲、03 起上课；b 房全天 12 节空闲；c 房 01 空闲、02 上课。
var (
	testRoomA = testRoom{id: "it-room-a", name: "综合教学楼101", nameRaw: "综合教学楼101(90/0)"}
	testRoomB = testRoom{id: "it-room-b", name: "综合教学楼102", nameRaw: "综合教学楼102(60/0)"}
	testRoomC = testRoom{id: "it-room-c", name: "F126", nameRaw: "F126(40/0)"}
)

// testRoom 是夹具里的一间房：jsbh、规范化名与含容量的原始名。
type testRoom struct {
	id      string // 房间身份 jsbh
	name    string // 规范化展示名
	nameRaw string // 含容量片段的原始名
}

// TestAvailability 校验空教室判定：区间内每一节都可用才返回。
func TestAvailability(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	term, releaseID := seedQueryFixture(t, s)

	full := AvailabilityFilter{
		ReleaseID: releaseID,
		Term:      term,
		Week:      1,
		Weekday:   1,
		StartNode: "01",
		EndNode:   "02",
		NodeCount: 2,
		Limit:     50,
	}

	// 01–02 两节：a 房与 b 房都可用
	total, rooms, err := s.Availability(ctx, full)
	if err != nil {
		t.Fatalf("查询空教室失败: %v", err)
	}
	if total != 2 || len(rooms) != 2 {
		t.Errorf("01–02 命中 %d 间（返回 %d 间），期望 2", total, len(rooms))
	}

	// 01–05 五节：只有 b 房全空闲，a 房从 03 起上课
	full.EndNode = "05"
	full.NodeCount = 5
	total, rooms, err = s.Availability(ctx, full)
	if err != nil {
		t.Fatalf("查询空教室失败: %v", err)
	}
	if total != 1 || len(rooms) != 1 || rooms[0].ID != testRoomB.id {
		t.Errorf("01–05 命中 %d 间 %+v，期望只有 b 房", total, rooms)
	}
}

// TestAvailabilityKeyword 校验关键词的子串匹配、ASCII 大小写与 LIKE 元字符转义。
func TestAvailabilityKeyword(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	term, releaseID := seedQueryFixture(t, s)

	base := AvailabilityFilter{
		ReleaseID: releaseID,
		Term:      term,
		Week:      1,
		Weekday:   1,
		StartNode: "01",
		EndNode:   "11",
		NodeCount: 11,
		Limit:     50,
	}

	// 中文子串匹配两间综合教学楼（a 与 b 里只有 b 全天空闲）
	base.Keyword = "综合教学楼"
	total, rooms, err := s.Availability(ctx, base)
	if err != nil {
		t.Fatalf("关键词查询失败: %v", err)
	}
	if total != 1 || len(rooms) != 1 || rooms[0].ID != testRoomB.id {
		t.Errorf("关键词“综合教学楼”命中 %d 间 %+v，期望只有 b 房", total, rooms)
	}

	// ASCII 不区分大小写：小写 f126 也要命中 c 房。
	// 这里把区间收成 01–01，因为 c 房 02 节在上课，全区间判定本来就不该命中它。
	base.Keyword = "f126"
	base.StartNode, base.EndNode, base.NodeCount = "01", "01", 1
	total, rooms, err = s.Availability(ctx, base)
	if err != nil {
		t.Fatalf("大小写查询失败: %v", err)
	}
	if total != 1 || len(rooms) != 1 || rooms[0].ID != testRoomC.id {
		t.Errorf("关键词“f126”命中 %d 间 %+v，期望 c 房", total, rooms)
	}

	// 关键词里的 % 只能当普通字符：转义失效时它会退化成通配符并命中 101/102
	base.Keyword = "10%"
	total, rooms, err = s.Availability(ctx, base)
	if err != nil {
		t.Fatalf("转义查询失败: %v", err)
	}
	if total != 0 || len(rooms) != 0 {
		t.Errorf("关键词“10%%”命中 %d 间，期望 0（%% 不应被当作通配符）", total)
	}
}

// TestDayStatuses 校验全天状态按房间分页并返回 12 小节状态。
func TestDayStatuses(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	term, releaseID := seedQueryFixture(t, s)

	// 周一：三间房都有数据
	total, cells, err := s.DayStatuses(ctx, DayFilter{
		ReleaseID: releaseID,
		Term:      term,
		Week:      1,
		Weekday:   1,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("查询全天状态失败: %v", err)
	}
	if total != 3 {
		t.Errorf("周一房间数 = %d，期望 3", total)
	}
	if len(cells) != 36 {
		t.Fatalf("周一状态格数 = %d，期望 36（3 间 × 12 节）", len(cells))
	}

	// a 房 01 空闲、03 上课，逐格状态要能对上
	aStates := map[string]string{}
	for _, cell := range cells {
		if cell.RoomID == testRoomA.id {
			aStates[cell.Node] = cell.StateKey
		}
	}
	if aStates["01"] != string(model.StateFree) || aStates["03"] != string(model.StateClass) {
		t.Errorf("a 房状态不符：%+v", aStates)
	}

	// 周二只给 a 房写过数据，星期过滤必须生效
	total, cells, err = s.DayStatuses(ctx, DayFilter{
		ReleaseID: releaseID,
		Term:      term,
		Week:      1,
		Weekday:   2,
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("查询周二状态失败: %v", err)
	}
	if total != 1 || len(cells) != 12 {
		t.Errorf("周二房间数/格数 = %d/%d，期望 1/12", total, len(cells))
	}
}

// TestPublishedWeeks 校验逐周已发布判定与周集合。
func TestPublishedWeeks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	term, releaseID := seedQueryFixture(t, s)

	published, err := s.WeekPublished(ctx, releaseID, term, 1)
	if err != nil {
		t.Fatalf("查询周发布状态失败: %v", err)
	}
	if !published {
		t.Error("第 1 周应当已发布")
	}

	// 第 3 周没有数据行，必须判为未发布
	published, err = s.WeekPublished(ctx, releaseID, term, 3)
	if err != nil {
		t.Fatalf("查询周发布状态失败: %v", err)
	}
	if published {
		t.Error("第 3 周没有数据，不应判为已发布")
	}

	weeks, err := s.PublishedWeeks(ctx, releaseID)
	if err != nil {
		t.Fatalf("查询已发布周次失败: %v", err)
	}
	if len(weeks) != 1 || weeks[0].Term != term || weeks[0].Week != 1 {
		t.Errorf("已发布周次 = %+v，期望只有 %s 第 1 周", weeks, term)
	}
}

// TestCurrentReleaseAndRunStatus 校验 current 版本与运行状态的读法。
func TestCurrentReleaseAndRunStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	term, releaseID := seedQueryFixture(t, s)

	release, err := s.CurrentRelease(ctx)
	if err != nil {
		t.Fatalf("读取 current release 失败: %v", err)
	}
	if release.ID != releaseID && release.Term != term {
		t.Errorf("current release = %+v，期望包含测试版本 %s", release, releaseID)
	}
	if release.DictVersion != 1 || release.AxisVersion != 1 {
		t.Errorf("current release 版本号 = %d/%d，期望 1/1", release.DictVersion, release.AxisVersion)
	}

	// 第一次发布时还没有上一版，应当返回 ErrNoRelease
	if _, err := s.PreviousRelease(ctx); err != nil && err != ErrNoRelease {
		t.Errorf("读取上一版返回了意外错误: %v", err)
	}

	// 没有任何采集运行时 found 为 false，不能当成失败
	status, found, err := s.LatestRunStatus(ctx)
	if err != nil {
		t.Fatalf("读取运行状态失败: %v", err)
	}
	if found && status == "" {
		t.Error("found 为 true 时状态不应为空")
	}
}

// seedQueryFixture 建一套最小可查询数据，返回学期与版本 id。
func seedQueryFixture(t *testing.T, s *Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().Format("20060102150405.000000")
	term := "it-term-" + stamp
	releaseID := "it-release-" + stamp

	// 学期与两周周历：第 1 周从 2025-09-15（周一）开始
	if _, err := s.pool.Exec(ctx, `INSERT INTO term (term, total_weeks) VALUES ($1, 20)`, term); err != nil {
		t.Fatalf("插入测试学期失败: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO term_week (term, week, monday) VALUES
  ($1, 1, DATE '2025-09-15'),
  ($1, 2, DATE '2025-09-22')`, term); err != nil {
		t.Fatalf("插入测试周历失败: %v", err)
	}

	rooms := []testRoom{testRoomA, testRoomB, testRoomC}
	for _, room := range rooms {
		if _, err := s.pool.Exec(ctx, `
INSERT INTO room (room_id, name, name_raw, first_seen, last_seen)
VALUES ($1, $2, $3, DATE '2025-09-15', DATE '2025-09-15')`, room.id, room.name, room.nameRaw); err != nil {
			t.Fatalf("插入测试房间失败: %v", err)
		}
	}

	if _, err := s.pool.Exec(ctx, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, now(), 1, 1, true)`, releaseID, term); err != nil {
		t.Fatalf("插入测试发布失败: %v", err)
	}

	// a 房：01–02 空闲，03 起上课；b 房：全天空闲；c 房：01 空闲、02 上课
	statesA := freeStates()
	markClassFrom(statesA, 3)
	insertDay(t, s, releaseID, term, 1, 1, testRoomA.id, statesA)
	insertDay(t, s, releaseID, term, 1, 1, testRoomB.id, freeStates())
	statesC := freeStates()
	statesC[1] = model.StateClass
	insertDay(t, s, releaseID, term, 1, 1, testRoomC.id, statesC)
	// 周二只给 a 房写数据，用来验证星期过滤
	insertDay(t, s, releaseID, term, 1, 2, testRoomA.id, freeStates())

	// 观测随 release 级联删除，房间与学期随后才能删掉
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM release WHERE release_id = $1`, releaseID); err != nil {
			t.Logf("清理 release 失败: %v", err)
		}
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM room WHERE room_id = ANY($1)`,
			[]string{testRoomA.id, testRoomB.id, testRoomC.id}); err != nil {
			t.Logf("清理 room 失败: %v", err)
		}
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM term WHERE term = $1`, term); err != nil {
			t.Logf("清理 term 失败: %v", err)
		}
	})

	return term, releaseID
}

// freeStates 返回 12 小节全空闲的状态序列。
func freeStates() []model.StateKey {
	states := make([]model.StateKey, 0, 12)
	for index := 0; index < 12; index++ {
		states = append(states, model.StateFree)
	}
	return states
}

// markClassFrom 把从 startNode 节开始到第 12 节都标成正常上课。
func markClassFrom(states []model.StateKey, startNode int) {
	for index := startNode - 1; index < len(states); index++ {
		states[index] = model.StateClass
	}
}

// insertDay 写入某房间某天的 12 小节状态。
func insertDay(t *testing.T, s *Store, releaseID, term string, week, weekday int, roomID string, states []model.StateKey) {
	t.Helper()
	ctx := context.Background()

	// 契约要求一间房一天恰 12 个节次，长度不对说明夹具写错了
	if len(states) != 12 {
		t.Fatalf("夹具需要 12 个节次状态，实际 %d", len(states))
	}

	for index, stateKey := range states {
		node := fmt.Sprintf("%02d", index+1)
		if _, err := s.pool.Exec(ctx, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			releaseID, roomID, term, week, weekday, node, string(stateKey), stateKey.IsUsable()); err != nil {
			t.Fatalf("插入观测失败: %v", err)
		}
	}
}
