package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
