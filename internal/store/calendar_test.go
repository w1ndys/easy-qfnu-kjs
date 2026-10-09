package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。
// 覆盖学期周历的整体替换：写入、按学期读回，以及失败与空输入时不留半截周。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestReplaceTermWeeksRoundTrip 断言周历写入后能按学期读回，周一日期原样落库。
func TestReplaceTermWeeksRoundTrip(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	term := createTestTerm(t, s, "it-calendar-roundtrip")

	weeks := testWeeks(term, 3)
	if err := s.ReplaceTermWeeks(ctx, term, weeks); err != nil {
		t.Fatalf("写入周历失败: %v", err)
	}

	got := readWeeks(t, s, term)
	if len(got) != len(weeks) {
		t.Fatalf("读回 %d 行，期望 %d 行", len(got), len(weeks))
	}
	for index, week := range got {
		// 周次按序读回，落库的 DATE 不应因为时区换算挪走一天
		if week.Week != weeks[index].Week {
			t.Errorf("第 %d 行的周次 = %d，期望 %d", index+1, week.Week, weeks[index].Week)
		}
		if week.Monday.Format("2006-01-02") != weeks[index].Monday.Format("2006-01-02") {
			t.Errorf("第 %d 周周一落库为 %s，期望 %s", week.Week,
				week.Monday.Format("2006-01-02"), weeks[index].Monday.Format("2006-01-02"))
		}
		// 采样生成的行都是教学周
		if !week.InCalendar {
			t.Errorf("第 %d 周 in_calendar = false，期望 true", week.Week)
		}
	}

	// 总周数跟着行数更新，查询侧据此知道学期长度
	if total := readTotalWeeks(t, s, term); total != len(weeks) {
		t.Errorf("term.total_weeks = %d，期望 %d", total, len(weeks))
	}
}

// TestReplaceTermWeeksKeepsOldRowsOnFailure 断言替换中途失败时旧周历原样保留。
func TestReplaceTermWeeksKeepsOldRowsOnFailure(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	term := createTestTerm(t, s, "it-calendar-failure")

	if err := s.ReplaceTermWeeks(ctx, term, testWeeks(term, 3)); err != nil {
		t.Fatalf("准备旧周历失败: %v", err)
	}

	// 同一个周次写两遍会撞主键，事务必须整体回滚
	duplicated := testWeeks(term, 2)
	duplicated = append(duplicated, duplicated[1])
	if err := s.ReplaceTermWeeks(ctx, term, duplicated); err == nil {
		t.Fatal("重复周次竟然写入成功，事务没起作用")
	}

	// 回滚之后必须还是原来那三行，旧的周历不能被删掉一半
	if got := readWeeks(t, s, term); len(got) != 3 {
		t.Errorf("失败后读回 %d 行，期望保留原来的 3 行", len(got))
	}
	// 总周数也要跟着回滚，否则查询侧会以为学期有别的长度
	if total := readTotalWeeks(t, s, term); total != 3 {
		t.Errorf("失败后 term.total_weeks = %d，期望 3", total)
	}
}

// TestReplaceTermWeeksRejectsEmpty 断言空周历与空学期编号被拒绝，且不动已有周历。
func TestReplaceTermWeeksRejectsEmpty(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	term := createTestTerm(t, s, "it-calendar-empty")

	if err := s.ReplaceTermWeeks(ctx, term, testWeeks(term, 2)); err != nil {
		t.Fatalf("准备旧周历失败: %v", err)
	}

	// 空周历既可能是采样失败，也可能是生成出错，两种情况都不许清表
	if err := s.ReplaceTermWeeks(ctx, term, nil); !errors.Is(err, ErrEmptyCalendar) {
		t.Errorf("空切片返回 %v，期望 ErrEmptyCalendar", err)
	}
	if err := s.ReplaceTermWeeks(ctx, term, []model.TermWeek{}); !errors.Is(err, ErrEmptyCalendar) {
		t.Errorf("空切片返回 %v，期望 ErrEmptyCalendar", err)
	}
	// 空学期编号也要挡住，否则会造出一个没有名字的学期行
	if err := s.ReplaceTermWeeks(ctx, "  ", testWeeks(term, 2)); err == nil {
		t.Error("空学期编号竟然写入成功")
	}

	if got := readWeeks(t, s, term); len(got) != 2 {
		t.Errorf("拒绝写入后读回 %d 行，期望原来那 2 行", len(got))
	}
}

// testWeeks 造 count 周的周历行，第 1 周周一固定为 2026-09-21。
func testWeeks(term string, count int) []model.TermWeek {
	firstMonday := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	weeks := make([]model.TermWeek, 0, count)
	for week := 1; week <= count; week++ {
		weeks = append(weeks, model.TermWeek{
			Term:       term,
			Week:       week,
			Monday:     firstMonday.AddDate(0, 0, 7*(week-1)),
			InCalendar: true,
		})
	}
	return weeks
}

// createTestTerm 用唯一名字隔离测试学期，测试结束删掉它（周历行随学期级联删除）。
func createTestTerm(t *testing.T, s *Store, prefix string) string {
	t.Helper()

	term := prefix + "-" + time.Now().Format("20060102150405.000000")
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM term WHERE term = $1`, term); err != nil {
			t.Logf("清理 term 失败: %v", err)
		}
	})
	return term
}

// readWeeks 按学期读出周历行，走查询侧同一个读法。
func readWeeks(t *testing.T, s *Store, term string) []model.TermWeek {
	t.Helper()

	all, err := s.ListTermWeeks(context.Background())
	if err != nil {
		t.Fatalf("读取周历失败: %v", err)
	}

	weeks := make([]model.TermWeek, 0, len(all))
	for _, week := range all {
		// 只挑本次测试的学期，开发库里的其它学期不参与断言
		if week.Term == term {
			weeks = append(weeks, week)
		}
	}
	return weeks
}

// readTotalWeeks 读某学期登记的总周数。
func readTotalWeeks(t *testing.T, s *Store, term string) int {
	t.Helper()

	var total int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT total_weeks FROM term WHERE term = $1`, term).Scan(&total); err != nil {
		t.Fatalf("读取学期总周数失败: %v", err)
	}
	return total
}
