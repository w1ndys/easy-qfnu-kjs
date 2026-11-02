package store

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openStatsTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
func openStatsTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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

// statsDay 返回统计夹具使用的业务日期（固定在 2026 年）：
// 仓储按 time.Time 的年月日编码成 date，因此用 UTC 零点做锚点即可得到确定的 date，不依赖进程时区。
func statsDay(month time.Month, day int) time.Time {
	return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
}

// statsWindow 造一段按日闭区间窗口。
func statsWindow(from, to time.Time) StatRange {
	return StatRange{From: from, To: to}
}

// countStatsRows 数一张统计表的行数，表名由用例给出（只接受本文件里的常量）。
func countStatsRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
		t.Fatalf("统计 %s 行数失败: %v", table, err)
	}
	return count
}

// TestWriteStatsAccumulatesPVAndDedupesVisitors 覆盖 PV 累加与 UV 去重（CP17）。
func TestWriteStatsAccumulatesPVAndDedupesVisitors(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	yesterday := statsDay(time.November, 2)

	// 同一批重复写入两次：PV 与维度命中累加，访客摘要只留一行。
	batch := StatsBatch{
		Hours:     []StatHourPV{{Day: yesterday, Hour: 10, Weekday: 1, PV: 1}},
		Visitors:  []StatVisitorKey{{Day: yesterday, Hour: 10, Hash: "hash-a"}},
		Buildings: []StatDayHits{{Day: yesterday, Key: "Q10", Hits: 1}},
		Rooms:     []StatDayHits{{Day: yesterday, Key: "0899", Hits: 1}},
		Keywords:  []StatKeywordHits{{Day: yesterday, Keyword: "f126", Hits: 1}},
		Nodes:     []StatNodeHits{{Day: yesterday, Node: "01", Hits: 1}},
	}
	for i := 0; i < 2; i++ {
		if err := WriteStats(ctx, pool, batch); err != nil {
			t.Fatalf("第 %d 次写入统计失败: %v", i+1, err)
		}
	}
	// 换日同一访客重新计一次；摘要本身已按天加盐，这里用不同摘要表达另一天。
	if err := WriteStats(ctx, pool, StatsBatch{
		Hours:    []StatHourPV{{Day: today, Hour: 9, Weekday: 2, PV: 1}},
		Visitors: []StatVisitorKey{{Day: today, Hour: 9, Hash: "hash-b"}},
	}); err != nil {
		t.Fatalf("写入次日统计失败: %v", err)
	}

	if got := countStatsRows(ctx, t, pool, "stat_visitor"); got != 2 {
		t.Fatalf("期望 2 条 UV 行，实际 %d 条", got)
	}

	facts, err := ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}

	// 昨天两次 + 今天一次；UV 昨天去重为 1、今天 1。
	if facts.Today != (StatPVUV{PV: 1, UV: 1}) {
		t.Fatalf("今日口径不符，实际 %+v", facts.Today)
	}
	if facts.D7 != (StatPVUV{PV: 3, UV: 2}) {
		t.Fatalf("近 7 日口径不符，实际 %+v", facts.D7)
	}
	if facts.D30 != (StatPVUV{PV: 3, UV: 2}) {
		t.Fatalf("近 30 日口径不符，实际 %+v", facts.D30)
	}
	if len(facts.Heatmap) != 2 {
		t.Fatalf("期望 2 个热力格，实际 %+v", facts.Heatmap)
	}
	for _, cell := range facts.Heatmap {
		switch cell.Weekday {
		case 1:
			if cell.Hour != 10 || cell.PV != 2 {
				t.Fatalf("周一 10 时期望 2 次，实际 %+v", cell)
			}
		case 2:
			if cell.Hour != 9 || cell.PV != 1 {
				t.Fatalf("周二 9 时期望 1 次，实际 %+v", cell)
			}
		default:
			t.Fatalf("出现意外的星期：%+v", cell)
		}
	}
}

// TestReadStatFactsWindowsAndLeaderboards 覆盖读取侧窗口边界、榜单展示名与节次分布。
func TestReadStatFactsWindowsAndLeaderboards(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	recent := statsDay(time.November, 2) // 热力窗口内（本用例取近 7 日），也是近 7 日内
	older := statsDay(time.October, 20)  // 热力窗口外、近 30 日内

	// 白名单与房间目录提供榜单展示名。
	mustExec(ctx, t, pool, "UPDATE settings SET value = $1 WHERE key = 'building_whitelist'",
		`[{"jxlbh":"Q10","name":"综合教学楼"}]`)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('0899', 'F126', 'F126', 'Q10', '综合教学楼')`)

	write := func(batch StatsBatch) {
		t.Helper()
		if err := WriteStats(ctx, pool, batch); err != nil {
			t.Fatalf("写入统计失败: %v", err)
		}
	}
	// 热力窗口内（本用例等于近 7 日）的命中：楼、房、关键词、节次各计一次。
	write(StatsBatch{
		Hours:     []StatHourPV{{Day: recent, Hour: 8, Weekday: 1, PV: 2}},
		Visitors:  []StatVisitorKey{{Day: recent, Hour: 8, Hash: "hash-recent"}},
		Buildings: []StatDayHits{{Day: recent, Key: "Q10", Hits: 2}},
		Rooms:     []StatDayHits{{Day: recent, Key: "0899", Hits: 2}},
		Keywords:  []StatKeywordHits{{Day: recent, Keyword: "f126", Hits: 2}},
		Nodes:     []StatNodeHits{{Day: recent, Node: "01", Hits: 2}},
	})
	// 热力窗口外的命中：只影响 30 日口径与趋势，不进热力、三张榜单与节次分布。
	write(StatsBatch{
		Hours:     []StatHourPV{{Day: older, Hour: 12, Weekday: 2, PV: 5}},
		Visitors:  []StatVisitorKey{{Day: older, Hour: 12, Hash: "hash-old"}},
		Buildings: []StatDayHits{{Day: older, Key: "Q11", Hits: 5}},
		Rooms:     []StatDayHits{{Day: older, Key: "0777", Hits: 5}},
		Keywords:  []StatKeywordHits{{Day: older, Keyword: "sz", Hits: 5}},
		Nodes:     []StatNodeHits{{Day: older, Node: "02", Hits: 5}},
	})

	facts, err := ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}

	if facts.Today != (StatPVUV{PV: 0, UV: 0}) {
		t.Fatalf("今日没有访问，实际 %+v", facts.Today)
	}
	if facts.D7 != (StatPVUV{PV: 2, UV: 1}) {
		t.Fatalf("近 7 日口径不符，实际 %+v", facts.D7)
	}
	if facts.D30 != (StatPVUV{PV: 7, UV: 2}) {
		t.Fatalf("近 30 日口径不符，实际 %+v", facts.D30)
	}

	if len(facts.Buildings) != 1 || facts.Buildings[0] != (StatBuildingHit{ID: "Q10", Name: "综合教学楼", Hits: 2}) {
		t.Fatalf("教学楼榜应只含热力窗口内的 Q10，实际 %+v", facts.Buildings)
	}
	if len(facts.Rooms) != 1 || facts.Rooms[0] != (StatRoomHit{ID: "0899", Name: "F126", Hits: 2}) {
		t.Fatalf("教室榜应只含热力窗口内的 0899 且带房名，实际 %+v", facts.Rooms)
	}
	if len(facts.Keywords) != 1 || facts.Keywords[0] != (StatKeywordHit{Text: "f126", Hits: 2}) {
		t.Fatalf("关键词榜应只含热力窗口内的 f126，实际 %+v", facts.Keywords)
	}
	if len(facts.Nodes) != 1 || facts.Nodes[0] != (StatNodeHit{Node: "01", Hits: 2}) {
		t.Fatalf("节次分布应只含近 7 日窗口内的 01，实际 %+v", facts.Nodes)
	}

	if len(facts.Days) != 2 {
		t.Fatalf("30 日窗口内应有 2 天有数据，实际 %+v", facts.Days)
	}
	if !facts.Days[0].Day.Equal(older) || facts.Days[0].PV != 5 {
		t.Fatalf("趋势首行应为 10 月 20 日的 5 次，实际 %+v", facts.Days[0])
	}
	if !facts.Days[1].Day.Equal(recent) || facts.Days[1].PV != 2 {
		t.Fatalf("趋势次行应为 11 月 2 日的 2 次，实际 %+v", facts.Days[1])
	}
}

// TestReadStatFactsBuildingLeaderboardOnlyWhitelisted 覆盖楼榜的读取侧白名单过滤：
// 白名单外的楼编号（公开请求可以伪造出来）不出现，也不占 Top N 名额。
// topN 取 1 而不是 10：只有「先过滤、后 LIMIT」时 Q10 才留得住，
// 若过滤被挪到 LIMIT 之后，Q99（99 次）会先占掉唯一的榜位，用例立刻失败。
func TestReadStatFactsBuildingLeaderboardOnlyWhitelisted(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	recent := statsDay(time.November, 2)

	// 白名单只有 Q10；Q99 是白名单外的楼编号，命中数远高于 Q10。
	mustExec(ctx, t, pool, "UPDATE settings SET value = $1 WHERE key = 'building_whitelist'",
		`[{"jxlbh":"Q10","name":"综合教学楼"}]`)
	for _, hit := range []StatDayHits{
		{Day: recent, Key: "Q10", Hits: 1},
		{Day: recent, Key: "Q99", Hits: 99},
	} {
		if err := WriteStats(ctx, pool, StatsBatch{Buildings: []StatDayHits{hit}}); err != nil {
			t.Fatalf("写入统计失败: %v", err)
		}
	}

	facts, err := ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 1)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}
	// 过滤先于 LIMIT：Q99 命中更多也不会把 Q10 挤出唯一的榜位。
	if len(facts.Buildings) != 1 || facts.Buildings[0] != (StatBuildingHit{ID: "Q10", Name: "综合教学楼", Hits: 1}) {
		t.Fatalf("topN=1 时楼榜应只含白名单内的 Q10，实际 %+v", facts.Buildings)
	}

	// 白名单为空时公开榜必然为空：统计行再多也不露出。
	mustExec(ctx, t, pool, "UPDATE settings SET value = '[]' WHERE key = 'building_whitelist'")
	facts, err = ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 1)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}
	if len(facts.Buildings) != 0 {
		t.Fatalf("白名单为空时楼榜应为空，实际 %+v", facts.Buildings)
	}
}

// TestReadStatFactsLeaderboardFollowsPeakWindow 覆盖三张 Top 10 的窗口跟随热力窗口切换：
// 同一批事实在近 7 日与近 30 日下给出不同榜单；窗口按闭区间，首日算在内、前一天不算。
func TestReadStatFactsLeaderboardFollowsPeakWindow(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	before30 := today.AddDate(0, 0, -30)  // 10 月 4 日：30 日窗口前一天
	first30 := today.AddDate(0, 0, -29)   // 10 月 5 日：30 日窗口首日
	tenDaysAgo := today.AddDate(0, 0, -9) // 10 月 25 日：只在 30 日窗口内
	before7 := today.AddDate(0, 0, -7)    // 10 月 27 日：7 日窗口前一天
	first7 := today.AddDate(0, 0, -6)     // 10 月 28 日：7 日窗口首日

	// 白名单与房间目录提供榜单展示名；Q13 也在白名单内，用来确认 30 日窗口不会多算一天。
	mustExec(ctx, t, pool, "UPDATE settings SET value = $1 WHERE key = 'building_whitelist'",
		`[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q11","name":"实验楼"},`+
			`{"jxlbh":"Q12","name":"文科楼"},{"jxlbh":"Q13","name":"理科楼"},{"jxlbh":"Q14","name":"信息楼"}]`)
	for _, room := range []struct{ id, name string }{
		{"0899", "F126"},
		{"0999", "F104"},
		{"0888", "F103"},
		{"0777", "F102"},
		{"0666", "F101"},
	} {
		mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ($1, $2, $2, 'Q10', '综合教学楼')`, room.id, room.name)
	}

	seeds := []struct {
		day      time.Time
		building string
		roomID   string
		keyword  string
		node     string
		hits     int
	}{
		{before30, "Q13", "0999", "old", "03", 9},  // 30 日窗口外：榜单与节次都不该出现
		{first30, "Q12", "0888", "wf", "02", 4},    // 30 日窗口首日：闭区间含
		{tenDaysAgo, "Q11", "0777", "sz", "02", 5}, // 7 日窗口外、30 日窗口内
		{before7, "Q14", "0666", "new", "02", 8},   // 7 日窗口外、30 日窗口内
		{first7, "Q10", "0899", "f126", "01", 2},   // 7 日窗口首日：闭区间含
	}
	for _, seed := range seeds {
		if err := WriteStats(ctx, pool, StatsBatch{
			Buildings: []StatDayHits{{Day: seed.day, Key: seed.building, Hits: seed.hits}},
			Rooms:     []StatDayHits{{Day: seed.day, Key: seed.roomID, Hits: seed.hits}},
			Keywords:  []StatKeywordHits{{Day: seed.day, Keyword: seed.keyword, Hits: seed.hits}},
			Nodes:     []StatNodeHits{{Day: seed.day, Node: seed.node, Hits: seed.hits}},
		}); err != nil {
			t.Fatalf("写入 %s 的统计失败: %v", seed.day.Format(time.DateOnly), err)
		}
	}

	read := func(peakDays int) StatFacts {
		t.Helper()
		facts, err := ReadStatFacts(ctx, pool, StatWindows{
			Today: statsWindow(today, today),
			D7:    statsWindow(today.AddDate(0, 0, -6), today),
			D30:   statsWindow(today.AddDate(0, 0, -29), today),
			Peak:  statsWindow(today.AddDate(0, 0, -(peakDays-1)), today),
		}, 10)
		if err != nil {
			t.Fatalf("读取 %d 日窗口统计失败: %v", peakDays, err)
		}
		return facts
	}

	week := read(7)
	month := read(30)

	// 近 7 日榜单只有 7 日窗口首日那条：before7 的 Q14（8 次）落在窗口外。
	if want := []StatBuildingHit{{ID: "Q10", Name: "综合教学楼", Hits: 2}}; !reflect.DeepEqual(week.Buildings, want) {
		t.Fatalf("近 7 日教学楼榜期望 %+v，实际 %+v", want, week.Buildings)
	}
	if want := []StatRoomHit{{ID: "0899", Name: "F126", Hits: 2}}; !reflect.DeepEqual(week.Rooms, want) {
		t.Fatalf("近 7 日教室榜期望 %+v，实际 %+v", want, week.Rooms)
	}
	if want := []StatKeywordHit{{Text: "f126", Hits: 2}}; !reflect.DeepEqual(week.Keywords, want) {
		t.Fatalf("近 7 日关键词榜期望 %+v，实际 %+v", want, week.Keywords)
	}

	// 近 30 日榜单收进 7 日窗口外的命中，但仍排除 30 日窗口外的 Q13。
	if want := []StatBuildingHit{
		{ID: "Q14", Name: "信息楼", Hits: 8},
		{ID: "Q11", Name: "实验楼", Hits: 5},
		{ID: "Q12", Name: "文科楼", Hits: 4},
		{ID: "Q10", Name: "综合教学楼", Hits: 2},
	}; !reflect.DeepEqual(month.Buildings, want) {
		t.Fatalf("近 30 日教学楼榜期望 %+v，实际 %+v", want, month.Buildings)
	}
	if want := []StatRoomHit{
		{ID: "0666", Name: "F101", Hits: 8},
		{ID: "0777", Name: "F102", Hits: 5},
		{ID: "0888", Name: "F103", Hits: 4},
		{ID: "0899", Name: "F126", Hits: 2},
	}; !reflect.DeepEqual(month.Rooms, want) {
		t.Fatalf("近 30 日教室榜期望 %+v，实际 %+v", want, month.Rooms)
	}
	if want := []StatKeywordHit{
		{Text: "new", Hits: 8},
		{Text: "sz", Hits: 5},
		{Text: "wf", Hits: 4},
		{Text: "f126", Hits: 2},
	}; !reflect.DeepEqual(month.Keywords, want) {
		t.Fatalf("近 30 日关键词榜期望 %+v，实际 %+v", want, month.Keywords)
	}

	// 节次分布不走热力窗口：两次读取都只含近 7 日窗口内的 01。
	wantNodes := []StatNodeHit{{Node: "01", Hits: 2}}
	if !reflect.DeepEqual(week.Nodes, wantNodes) {
		t.Fatalf("近 7 日节次分布期望 %+v，实际 %+v", wantNodes, week.Nodes)
	}
	if !reflect.DeepEqual(month.Nodes, wantNodes) {
		t.Fatalf("切到近 30 日热力窗口后节次分布仍应取近 7 日，期望 %+v，实际 %+v", wantNodes, month.Nodes)
	}
}

// TestReadStatFactsLeaderboardTieBreak 覆盖三张 Top 10 的同分排序（Requirement 34.5）：
// 命中数相同时，教学楼按楼编号、教室按展示名再按房号、关键词按词面升序，
// 结果不随写入顺序漂移。排序在 SQL 里做，整形层没有二次排序，所以必须在真实库上验证。
func TestReadStatFactsLeaderboardTieBreak(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	recent := statsDay(time.November, 2)

	// 白名单故意按 Q12/Q10/Q11 的顺序写，榜单最终要按楼编号升序。
	mustExec(ctx, t, pool, "UPDATE settings SET value = $1 WHERE key = 'building_whitelist'",
		`[{"jxlbh":"Q12","name":"文科楼"},{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q11","name":"实验楼"}]`)
	// 房名顺序与房号顺序相反：0900→F100、0100→F200，同分时应先 F100。
	for _, room := range []struct{ id, name string }{
		{"0900", "F100"},
		{"0100", "F200"},
	} {
		mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ($1, $2, $2, 'Q10', '综合教学楼')`, room.id, room.name)
	}

	// 三栋楼、三间房、三个关键词命中数完全相同，只能靠次序键决定榜位。
	// 房号 0123 不在 room 表里：LEFT JOIN 后展示名为空串，按 coalesce 口径排在最前。
	seeds := []struct {
		building string
		roomID   string
		keyword  string
	}{
		{"Q12", "0900", "ww"},
		{"Q11", "0123", "bb"},
		{"Q10", "0100", "aa"},
	}
	for _, seed := range seeds {
		if err := WriteStats(ctx, pool, StatsBatch{
			Buildings: []StatDayHits{{Day: recent, Key: seed.building, Hits: 3}},
			Rooms:     []StatDayHits{{Day: recent, Key: seed.roomID, Hits: 3}},
			Keywords:  []StatKeywordHits{{Day: recent, Keyword: seed.keyword, Hits: 3}},
		}); err != nil {
			t.Fatalf("写入同分统计失败: %v", err)
		}
	}

	facts, err := ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}

	wantBuildings := []StatBuildingHit{
		{ID: "Q10", Name: "综合教学楼", Hits: 3},
		{ID: "Q11", Name: "实验楼", Hits: 3},
		{ID: "Q12", Name: "文科楼", Hits: 3},
	}
	if !reflect.DeepEqual(facts.Buildings, wantBuildings) {
		t.Fatalf("同分教学楼榜应按编号升序，期望 %+v，实际 %+v", wantBuildings, facts.Buildings)
	}

	wantRooms := []StatRoomHit{
		{ID: "0123", Name: "", Hits: 3},
		{ID: "0900", Name: "F100", Hits: 3},
		{ID: "0100", Name: "F200", Hits: 3},
	}
	if !reflect.DeepEqual(facts.Rooms, wantRooms) {
		t.Fatalf("同分教室榜应先按房名、再按房号升序，期望 %+v，实际 %+v", wantRooms, facts.Rooms)
	}

	wantKeywords := []StatKeywordHit{
		{Text: "aa", Hits: 3},
		{Text: "bb", Hits: 3},
		{Text: "ww", Hits: 3},
	}
	if !reflect.DeepEqual(facts.Keywords, wantKeywords) {
		t.Fatalf("同分关键词榜应按词面升序，期望 %+v，实际 %+v", wantKeywords, facts.Keywords)
	}
}

// TestWriteStatsUsesShanghaiBusinessDate 覆盖上海零点写 date 不差一天（Requirement 34.2、CP17）：
// 记账侧交给仓储的是 Asia/Shanghai 的当日时刻，date 列必须落成同一天，
// 不能因为它的 UTC 表示在前一天就退一天。
func TestWriteStatsUsesShanghaiBusinessDate(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	day := time.Date(2026, time.November, 2, 0, 0, 0, 0, shanghai)
	midnight := day
	lateNight := time.Date(2026, time.November, 2, 23, 59, 0, 0, shanghai)

	// 夹具前提：上海零点的 UTC 表示落在前一天，按 UTC 取年月日就会写错。
	if got := midnight.UTC().Format(time.DateOnly); got != "2026-11-01" {
		t.Fatalf("夹具前提不成立：上海零点应表示为 UTC 11-01，实际 %s", got)
	}

	if err := WriteStats(ctx, pool, StatsBatch{
		Hours: []StatHourPV{
			{Day: midnight, Hour: 0, Weekday: 1, PV: 1},
			{Day: lateNight, Hour: 23, Weekday: 1, PV: 1},
		},
		Visitors: []StatVisitorKey{
			{Day: midnight, Hour: 0, Hash: "hash-midnight"},
			{Day: lateNight, Hour: 23, Hash: "hash-late"},
		},
		Keywords: []StatKeywordHits{{Day: midnight, Keyword: "f126", Hits: 1}},
	}); err != nil {
		t.Fatalf("写入上海时段统计失败: %v", err)
	}

	// 三张表都应整段落在 11 月 2 日：既没有行退到 11 月 1 日，也没有行跳到 11 月 3 日。
	for _, table := range []struct {
		name string
		rows int
	}{
		{"stat_hour", 2},
		{"stat_visitor", 2},
		{"stat_keyword_day", 1},
	} {
		var total, wrong int
		if err := pool.QueryRow(ctx,
			"SELECT count(*), count(*) FILTER (WHERE day <> $1) FROM "+table.name, day).
			Scan(&total, &wrong); err != nil {
			t.Fatalf("核对 %s 的业务日期失败: %v", table.name, err)
		}
		if total != table.rows {
			t.Fatalf("%s 应有 %d 行，实际 %d 行", table.name, table.rows, total)
		}
		if wrong != 0 {
			t.Fatalf("%s 有 %d 行的 day 不是 2026-11-02：上海零点被写成了别的日期", table.name, wrong)
		}
	}

	// 读侧用同一份上海业务日期取数：今日口径读得到，前一天口径读不到。
	windows := StatWindows{
		Today: statsWindow(day, day),
		D7:    statsWindow(day.AddDate(0, 0, -6), day),
		D30:   statsWindow(day.AddDate(0, 0, -29), day),
		Peak:  statsWindow(day.AddDate(0, 0, -6), day),
	}
	facts, err := ReadStatFacts(ctx, pool, windows, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}
	if facts.Today != (StatPVUV{PV: 2, UV: 2}) {
		t.Fatalf("今日口径应为 PV=2、UV=2，实际 %+v", facts.Today)
	}

	yesterday := day.AddDate(0, 0, -1)
	facts, err = ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(yesterday, yesterday),
		D7:    statsWindow(yesterday.AddDate(0, 0, -6), yesterday),
		D30:   statsWindow(yesterday.AddDate(0, 0, -29), yesterday),
		Peak:  statsWindow(yesterday.AddDate(0, 0, -6), yesterday),
	}, 10)
	if err != nil {
		t.Fatalf("读取前一天统计失败: %v", err)
	}
	if facts.Today != (StatPVUV{PV: 0, UV: 0}) {
		t.Fatalf("前一天口径不该读到 11 月 2 日的记录，实际 %+v", facts.Today)
	}
}

// TestDeleteStatsBeforeCutoff 覆盖清理边界：只删早于边界的记录（Requirement 34.10、CP19）。
func TestDeleteStatsBeforeCutoff(t *testing.T) {
	ctx, pool := openStatsTestDB(t)

	today := statsDay(time.November, 3)
	cutoff := today.AddDate(0, 0, -90)
	tooOld := cutoff.AddDate(0, 0, -1)

	for _, day := range []time.Time{tooOld, cutoff, cutoff.AddDate(0, 0, 1)} {
		if err := WriteStats(ctx, pool, StatsBatch{
			Hours:     []StatHourPV{{Day: day, Hour: 8, Weekday: 1, PV: 1}},
			Visitors:  []StatVisitorKey{{Day: day, Hour: 8, Hash: day.Format(time.DateOnly)}},
			Buildings: []StatDayHits{{Day: day, Key: "Q10", Hits: 1}},
			Rooms:     []StatDayHits{{Day: day, Key: "0899", Hits: 1}},
			Keywords:  []StatKeywordHits{{Day: day, Keyword: "f126", Hits: 1}},
			Nodes:     []StatNodeHits{{Day: day, Node: "01", Hits: 1}},
		}); err != nil {
			t.Fatalf("写入 %s 的统计失败: %v", day.Format(time.DateOnly), err)
		}
	}

	deleted, err := DeleteStatsBefore(ctx, pool, cutoff)
	if err != nil {
		t.Fatalf("清理过期统计失败: %v", err)
	}
	// 六张表各删一行。
	if deleted != 6 {
		t.Fatalf("期望删除 6 行，实际 %d 行", deleted)
	}

	for _, table := range []string{
		"stat_hour", "stat_visitor", "stat_building_day",
		"stat_room_day", "stat_keyword_day", "stat_node_day",
	} {
		if got := countStatsRows(ctx, t, pool, table); got != 2 {
			t.Fatalf("%s 应保留 2 行（边界当天与之后），实际 %d 行", table, got)
		}
	}

	// 恰好落在边界上的那一天必须留着：保留期按「超过 90 天」算。
	var kept int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM stat_hour WHERE day = $1", cutoff).Scan(&kept); err != nil {
		t.Fatalf("查询边界行失败: %v", err)
	}
	if kept != 1 {
		t.Fatalf("边界当天应保留 1 行，实际 %d 行", kept)
	}
}
