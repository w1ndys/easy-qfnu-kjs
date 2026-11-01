package store

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openMetaTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
func openMetaTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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

// TestMetaAgainstRealDB 覆盖 meta 的 SQL 口径：只读当前版本、只读该版本的字典/轴、
// 只读当前白名单，并在无当前版本时返回空事实。
func TestMetaAgainstRealDB(t *testing.T) {
	ctx, pool := openMetaTestDB(t)

	// 两个版本：只有 is_current 的那个参与 meta。
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", "2026-2027-1", 16)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ('meta-cur', $1, $2, 1, 1, true),
       ('meta-old', $1, $2 - interval '7 days', 1, 1, false)`,
		"2026-2027-1", time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC))

	// 白名单：一条正常、一条带空白、一条空编号（应被丢弃）。
	mustExec(ctx, t, pool, `
INSERT INTO settings (key, value) VALUES ('building_whitelist', $1)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		`[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":" Q11 ","name":"实验楼"},{"jxlbh":"","name":"空编号"}]`)

	t.Run("完整数据", func(t *testing.T) {
		facts, err := Meta(ctx, pool)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}

		if facts.Release == nil {
			t.Fatal("应读到当前版本")
		}
		if facts.Release.ID != "meta-cur" || facts.Release.Term != "2026-2027-1" ||
			facts.Release.TotalWeeks != 16 || facts.Release.DictVersion != 1 ||
			facts.Release.AxisVersion != 1 {
			t.Fatalf("版本事实不符: %+v", facts.Release)
		}
		wantGenerated := time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC)
		if !facts.Release.GeneratedAt.Equal(wantGenerated) {
			t.Fatalf("生成时间期望 %s，实际 %s", wantGenerated, facts.Release.GeneratedAt)
		}

		// 字典种子 11 条，按状态键排序，free 可用、unknown 不可用。
		if len(facts.DictStates) != 11 {
			t.Fatalf("字典状态期望 11 条，实际 %d", len(facts.DictStates))
		}
		keys := make([]string, 0, len(facts.DictStates))
		states := make(map[string]MetaDictState, len(facts.DictStates))
		for _, state := range facts.DictStates {
			keys = append(keys, state.StateKey)
			states[state.StateKey] = state
		}
		if !sort.StringsAreSorted(keys) {
			t.Fatalf("字典状态应按状态键排序，实际 %v", keys)
		}
		if free := states["free"]; !free.Available || free.Label != "空闲" {
			t.Fatalf("free 状态不符: %+v", free)
		}
		if unknown := states["unknown"]; unknown.Available {
			t.Fatalf("unknown 状态不应可用: %+v", unknown)
		}

		// 轴种子 12 个小节，按 ordinal 排序。
		if len(facts.AxisNodes) != 12 {
			t.Fatalf("轴节点期望 12 条，实际 %d", len(facts.AxisNodes))
		}
		first, last := facts.AxisNodes[0], facts.AxisNodes[len(facts.AxisNodes)-1]
		if first.Node != "01" || first.Ordinal != 1 || first.Block != "0102" || first.BlockOrdinal != 1 {
			t.Fatalf("首节点不符: %+v", first)
		}
		if last.Node != "12" || last.Ordinal != 12 || last.Block != "101112" || last.BlockOrdinal != 5 {
			t.Fatalf("末节点不符: %+v", last)
		}

		wantBuildings := []MetaBuilding{
			{ID: "Q10", Name: "综合教学楼"},
			{ID: "Q11", Name: "实验楼"},
		}
		if len(facts.Buildings) != len(wantBuildings) {
			t.Fatalf("白名单楼期望 %+v，实际 %+v", wantBuildings, facts.Buildings)
		}
		for i, want := range wantBuildings {
			if facts.Buildings[i] != want {
				t.Fatalf("第 %d 栋楼期望 %+v，实际 %+v", i, want, facts.Buildings[i])
			}
		}
	})

	t.Run("无当前版本", func(t *testing.T) {
		mustExec(ctx, t, pool, "UPDATE release SET is_current = false")

		facts, err := Meta(ctx, pool)
		if err != nil {
			t.Fatalf("无当前版本不应报错: %v", err)
		}
		if facts.Release != nil {
			t.Fatalf("无当前版本时 Release 期望 nil，实际 %+v", facts.Release)
		}
		if len(facts.DictStates) != 0 || len(facts.AxisNodes) != 0 {
			t.Fatalf("无当前版本时字典与轴应为空: %+v", facts)
		}
		if len(facts.Buildings) != 2 {
			t.Fatalf("无当前版本仍应返回白名单楼，实际 %+v", facts.Buildings)
		}
	})

	t.Run("白名单键缺失", func(t *testing.T) {
		mustExec(ctx, t, pool, "DELETE FROM settings WHERE key = 'building_whitelist'")

		facts, err := Meta(ctx, pool)
		if err != nil {
			t.Fatalf("白名单键缺失不应报错: %v", err)
		}
		if facts.Buildings == nil || len(facts.Buildings) != 0 {
			t.Fatalf("白名单键缺失期望空列表，实际 %+v", facts.Buildings)
		}
	})
}
