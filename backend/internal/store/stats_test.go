package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// statExecStub 是统计写入的语句级替身：记录执行过的语句，命中 failOn 时返回错误。
type statExecStub struct {
	failOn   string   // 命中这段 SQL 就失败；空串表示全部成功
	executed []string // 按顺序记录执行过的 SQL
}

// Exec 实现 statExecutor。
func (s *statExecStub) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	s.executed = append(s.executed, sql)
	if s.failOn != "" && strings.Contains(sql, s.failOn) {
		return pgconn.CommandTag{}, errors.New("模拟写入失败")
	}
	return pgconn.CommandTag{}, nil
}

// statsBatchAllSix 造一批六段齐全的统计增量，用来观察某一段失败时其余段的执行情况。
func statsBatchAllSix() StatsBatch {
	day := time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)
	return StatsBatch{
		Hours:     []StatHourPV{{Day: day, Hour: 10, Weekday: 1, PV: 1}},
		Visitors:  []StatVisitorKey{{Day: day, Hour: 10, Hash: "hash-a"}},
		Buildings: []StatDayHits{{Day: day, Key: "Q10", Hits: 1}},
		Rooms:     []StatDayHits{{Day: day, Key: "0899", Hits: 1}},
		Keywords:  []StatKeywordHits{{Day: day, Keyword: "f126", Hits: 1}},
		Nodes:     []StatNodeHits{{Day: day, Node: "01", Hits: 1}},
	}
}

// TestWriteStatsContinuesAfterOneStatementFails 覆盖六段逐条容错：
// 任意一段失败都不能吞掉同批后续维度，失败仍要合并成 ErrStatsFailed 返回（CP18）。
func TestWriteStatsContinuesAfterOneStatementFails(t *testing.T) {
	tables := []string{
		"stat_hour",
		"stat_visitor",
		"stat_building_day",
		"stat_room_day",
		"stat_keyword_day",
		"stat_node_day",
	}

	for _, failOn := range tables {
		t.Run(failOn, func(t *testing.T) {
			exec := &statExecStub{failOn: failOn}
			err := WriteStats(context.Background(), exec, statsBatchAllSix())

			if !errors.Is(err, model.ErrStatsFailed) {
				t.Fatalf("%s 失败时应返回统计写入错误，实际 %v", failOn, err)
			}
			// 六段都必须尝试过：一段失败不能提前返回。
			if len(exec.executed) != len(tables) {
				t.Fatalf("%s 失败后应继续写完其余段，期望执行 %d 段，实际 %d 段", failOn, len(tables), len(exec.executed))
			}
			joined := strings.Join(exec.executed, "\n")
			for _, table := range tables {
				if !strings.Contains(joined, table) {
					t.Fatalf("%s 失败后仍应执行 %s，实际执行语句 %s", failOn, table, joined)
				}
			}
		})
	}
}

// TestWriteStatsAllStatementsSucceed 覆盖全部成功时返回 nil，不把空错误集合当成失败。
func TestWriteStatsAllStatementsSucceed(t *testing.T) {
	exec := &statExecStub{}
	if err := WriteStats(context.Background(), exec, statsBatchAllSix()); err != nil {
		t.Fatalf("全部写入成功时不应返回错误，实际 %v", err)
	}
	if len(exec.executed) != 6 {
		t.Fatalf("期望执行 6 段写入，实际 %d 段", len(exec.executed))
	}
}

// TestWriteStatsSkipsEmptySections 覆盖空段落不落库：没有维度的批次不应发无意义的 SQL。
func TestWriteStatsSkipsEmptySections(t *testing.T) {
	exec := &statExecStub{}
	if err := WriteStats(context.Background(), exec, StatsBatch{}); err != nil {
		t.Fatalf("空批次不应返回错误，实际 %v", err)
	}
	if len(exec.executed) != 0 {
		t.Fatalf("空批次不应执行写入，实际 %d 段", len(exec.executed))
	}
}

// statRowStub 是最小 pgx.Row 替身：只回一份预设的白名单 JSON。
type statRowStub struct{ raw string }

// Scan 实现 pgx.Row：白名单查询只有一个 *string 目标。
func (s statRowStub) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("白名单查询期望 1 个扫描目标，实际 %d 个", len(dest))
	}
	target, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("白名单查询期望 *string，实际 %T", dest[0])
	}
	*target = s.raw
	return nil
}

// errStatStubQuery 让「不该发生的统计表查询」直接失败，而不是悄悄返回空结果。
var errStatStubQuery = errors.New("统计查询替身不该被调用")

// statQuerierStub 记录下发过的语句，并按需回白名单。
// Query 一律失败：本用例只关心「空白名单时根本没查统计表」，
// 真去查了就会以 ErrQueryFailed 暴露出来。
type statQuerierStub struct {
	whitelist string
	queries   []string
}

// QueryRow 实现 statQuerier：白名单是这里唯一允许的读。
func (s *statQuerierStub) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	s.queries = append(s.queries, sql)
	return statRowStub{raw: s.whitelist}
}

// Query 实现 statQuerier：用例断言它不该被调用。
func (s *statQuerierStub) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	s.queries = append(s.queries, sql)
	return nil, errStatStubQuery
}

// queriedStatBuilding 判断替身是否收到过读楼榜统计表的语句。
func (s *statQuerierStub) queriedStatBuilding() bool {
	for _, sql := range s.queries {
		if strings.Contains(sql, "stat_building_day") {
			return true
		}
	}
	return false
}

// TestStatBuildingTopSkipsQueryWhenWhitelistEmpty 覆盖楼榜白名单为空时的短路：
// 公开榜必然为空，不该再对 stat_building_day 发查询。
// 这条短路是「过滤先于 LIMIT」的一部分：没有它，空名单会退化成一次全表 Top N 白跑。
func TestStatBuildingTopSkipsQueryWhenWhitelistEmpty(t *testing.T) {
	window := StatRange{
		From: time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC),
	}

	querier := &statQuerierStub{whitelist: "[]"}
	hits, err := statBuildingTop(context.Background(), querier, window, 1)
	if err != nil {
		t.Fatalf("白名单为空不应报错，实际 %v", err)
	}
	if hits == nil {
		t.Fatal("空榜应是空数组而不是 nil")
	}
	if len(hits) != 0 {
		t.Fatalf("白名单为空时楼榜应为空，实际 %+v", hits)
	}
	if querier.queriedStatBuilding() {
		t.Fatalf("白名单为空时不该查 stat_building_day，实际语句 %v", querier.queries)
	}

	// 反向对照：白名单非空时确实会查统计表，避免上面的断言因为「什么都不查」而空转。
	querier = &statQuerierStub{whitelist: `[{"jxlbh":"Q10","name":"综合教学楼"}]`}
	if _, err := statBuildingTop(context.Background(), querier, window, 1); !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("替身拒答统计表时应上抛 ErrQueryFailed，实际 %v", err)
	}
	if !querier.queriedStatBuilding() {
		t.Fatalf("白名单非空时应查 stat_building_day，实际语句 %v", querier.queries)
	}
}
