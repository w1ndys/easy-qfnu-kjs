package store

import (
	"context"
	"os"
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
	recent := statsDay(time.November, 2) // 近 7 日内
	older := statsDay(time.October, 20)  // 近 7 日外、近 30 日内

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
	// 近 7 日内的命中：楼、房、关键词、节次各计一次。
	write(StatsBatch{
		Hours:     []StatHourPV{{Day: recent, Hour: 8, Weekday: 1, PV: 2}},
		Visitors:  []StatVisitorKey{{Day: recent, Hour: 8, Hash: "hash-recent"}},
		Buildings: []StatDayHits{{Day: recent, Key: "Q10", Hits: 2}},
		Rooms:     []StatDayHits{{Day: recent, Key: "0899", Hits: 2}},
		Keywords:  []StatKeywordHits{{Day: recent, Keyword: "f126", Hits: 2}},
		Nodes:     []StatNodeHits{{Day: recent, Node: "01", Hits: 2}},
	})
	// 近 7 日外的命中：只影响 30 日口径与趋势，不进排行榜。
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
		t.Fatalf("教学楼榜应只含近 7 日的 Q10，实际 %+v", facts.Buildings)
	}
	if len(facts.Rooms) != 1 || facts.Rooms[0] != (StatRoomHit{ID: "0899", Name: "F126", Hits: 2}) {
		t.Fatalf("教室榜应只含近 7 日的 0899 且带房名，实际 %+v", facts.Rooms)
	}
	if len(facts.Keywords) != 1 || facts.Keywords[0] != (StatKeywordHit{Text: "f126", Hits: 2}) {
		t.Fatalf("关键词榜应只含近 7 日的 f126，实际 %+v", facts.Keywords)
	}
	if len(facts.Nodes) != 1 || facts.Nodes[0] != (StatNodeHit{Node: "01", Hits: 2}) {
		t.Fatalf("节次分布应只含近 7 日的 01，实际 %+v", facts.Nodes)
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
	}, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}
	// 过滤先于 LIMIT：Q99 命中更多也不会把 Q10 挤出榜单。
	if len(facts.Buildings) != 1 || facts.Buildings[0] != (StatBuildingHit{ID: "Q10", Name: "综合教学楼", Hits: 1}) {
		t.Fatalf("楼榜应只含白名单内的 Q10，实际 %+v", facts.Buildings)
	}

	// 白名单为空时公开榜必然为空：统计行再多也不露出。
	mustExec(ctx, t, pool, "UPDATE settings SET value = '[]' WHERE key = 'building_whitelist'")
	facts, err = ReadStatFacts(ctx, pool, StatWindows{
		Today: statsWindow(today, today),
		D7:    statsWindow(today.AddDate(0, 0, -6), today),
		D30:   statsWindow(today.AddDate(0, 0, -29), today),
		Peak:  statsWindow(today.AddDate(0, 0, -6), today),
	}, 10)
	if err != nil {
		t.Fatalf("读取统计失败: %v", err)
	}
	if len(facts.Buildings) != 0 {
		t.Fatalf("白名单为空时楼榜应为空，实际 %+v", facts.Buildings)
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
