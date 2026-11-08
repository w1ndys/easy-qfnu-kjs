package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestTermWeekAtAgainstRealDB 只在提供 KJS_TEST_DSN 时运行，验证半开区间的边界。
func TestTermWeekAtAgainstRealDB(t *testing.T) {
	dsn := os.Getenv("KJS_TEST_DSN")
	// 没有测试库就跳过，避免本地无 PostgreSQL 时整仓测试变红。
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过真实库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer pool.Close()

	// 从干净库开始，确保迁移与下面的种子都由本用例负责。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 造两周：第 5 周周一为 2026-10-19，第 6 周周一为 2026-10-26。
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", "2026-2027-1", 16)
	mustExec(ctx, t, pool, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)", "2026-2027-1", 5, mustDate(t, "2026-10-19"))
	mustExec(ctx, t, pool, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)", "2026-2027-1", 6, mustDate(t, "2026-10-26"))

	cases := []struct {
		name   string
		date   string
		wantOK bool
		wantWk int16
	}{
		{name: "周一当天", date: "2026-10-19", wantOK: true, wantWk: 5},
		{name: "周中", date: "2026-10-22", wantOK: true, wantWk: 5},
		{name: "周日是本周最后一天", date: "2026-10-25", wantOK: true, wantWk: 5},
		{name: "下周一进入下一周", date: "2026-10-26", wantOK: true, wantWk: 6},
		{name: "学期前无覆盖", date: "2026-10-12", wantOK: false},
		{name: "学期后无覆盖", date: "2026-12-31", wantOK: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tw, ok, err := TermWeekAt(ctx, pool, mustDate(t, c.date))
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if ok != c.wantOK {
				t.Fatalf("覆盖判定期望 %v，实际 %v", c.wantOK, ok)
			}
			if ok && tw.Week != c.wantWk {
				t.Fatalf("周次期望 %d，实际 %d", c.wantWk, tw.Week)
			}
		})
	}
}

// mustDate 解析 YYYY-MM-DD，失败直接终止用例。
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("解析日期 %s 失败: %v", s, err)
	}
	return d
}
