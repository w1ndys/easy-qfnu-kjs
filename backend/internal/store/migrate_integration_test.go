package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigrateAgainstRealDB 只在提供 KJS_TEST_DSN 时运行；会先清空 public schema，只指向测试库。
func TestMigrateAgainstRealDB(t *testing.T) {
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

	// 每次从干净状态开始，确保基线脚本真的从零建表。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}
	// 再跑一次验证幂等：已应用版本应被跳过而不是重复建表报错。
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("重复迁移应为幂等，实际失败: %v", err)
	}

	var seeds int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM dict_state").Scan(&seeds); err != nil {
		t.Fatalf("字典种子缺失: %v", err)
	}
	if seeds != 11 {
		t.Fatalf("dict_state 期望 11 行，实际 %d", seeds)
	}

	var nodes int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM axis_node").Scan(&nodes); err != nil {
		t.Fatalf("轴种子缺失: %v", err)
	}
	if nodes != 12 {
		t.Fatalf("axis_node 期望 12 行，实际 %d", nodes)
	}

	var applied int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migration").Scan(&applied); err != nil {
		t.Fatalf("迁移记录缺失: %v", err)
	}
	if applied != 2 {
		t.Fatalf("schema_migration 期望 2 行（0001 基线 + 0002 每轮归属），实际 %d", applied)
	}

	// 0002 的 conflict 不变量落在 DB 里，不只是一段注释：约束必须存在，
	// 且真的拒绝「标了冲突却没有被丢弃的楼」的行。只断言名字不够，
	// 一条 INSERT 才能证明它生效。
	var conflictCheck string
	if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = 'collect_run_room'
   AND c.conname = 'collect_run_room_conflict_consistent'`).Scan(&conflictCheck); err != nil {
		t.Fatalf("collect_run_room 缺少 conflict 不变量约束: %v", err)
	}
	if !strings.Contains(conflictCheck, "conflict_building_id") {
		t.Fatalf("conflict 不变量约束应引用 conflict_building_id，实际 %q", conflictCheck)
	}

	for _, statement := range []string{
		"INSERT INTO term (term, total_weeks) VALUES ('2026-2027-1', 20)",
		"INSERT INTO room (room_id, name, building_id) VALUES ('r101', 'F101', 'Q10'), ('r102', 'F102', 'Q10'), ('r103', 'F103', 'Q10')",
		"INSERT INTO collect_run (started_at, status, term) VALUES (now(), 'failed', '2026-2027-1')",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("准备约束用例数据失败（%s）: %v", statement, err)
		}
	}

	// 合法行必须能落库：冲突标记与被丢弃的楼同进同出，两个方向各一条。
	// 只测拒绝会漏掉「约束写反了、连合法行也拒绝」这种情况。
	for _, statement := range []string{
		`INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
SELECT run_id, term, 'r101', 'Q10', true, 'Q11' FROM collect_run`,
		`INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
SELECT run_id, term, 'r102', 'Q10', false, NULL FROM collect_run`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("合法的每轮归属行不应被约束拒绝（%s）: %v", statement, err)
		}
	}

	// 非法行必须被 CHECK 拒绝，而不是随便什么错：断言 SQLSTATE 与约束名，
	// 否则「表名拼错」「外键不存在」这类错误也能让只判 err != nil 的断言通过。
	// r103 只用来撞约束：两条都被拒，说明失败的行一条都没留下。
	rejected := []struct {
		name      string
		statement string
	}{
		{
			name: "标了冲突却没有被丢弃的楼",
			statement: `INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
SELECT run_id, term, 'r103', 'Q10', true, NULL FROM collect_run`,
		},
		{
			name: "没标冲突却带着被丢弃的楼",
			statement: `INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
SELECT run_id, term, 'r103', 'Q10', false, 'Q11' FROM collect_run`,
		},
		{
			name: "标了冲突但被丢弃的楼是空串",
			statement: `INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
SELECT run_id, term, 'r103', 'Q10', true, '   ' FROM collect_run`,
		},
	}
	for _, c := range rejected {
		_, err := pool.Exec(ctx, c.statement)
		if err == nil {
			t.Fatalf("collect_run_room 应拒绝「%s」的行", c.name)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("「%s」应以 *pgconn.PgError 报错，实际 %T: %v", c.name, err, err)
		}
		if pgErr.Code != "23514" {
			t.Fatalf("「%s」应以 SQLSTATE 23514（check_violation）报错，实际 %s: %v",
				c.name, pgErr.Code, err)
		}
		if pgErr.ConstraintName != "collect_run_room_conflict_consistent" {
			t.Fatalf("「%s」应命中断言约束 collect_run_room_conflict_consistent，实际 %q",
				c.name, pgErr.ConstraintName)
		}
	}

	var rejectedRows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM collect_run_room WHERE room_id = 'r103'").Scan(&rejectedRows); err != nil {
		t.Fatalf("统计被拒绝的行失败: %v", err)
	}
	if rejectedRows != 0 {
		t.Fatalf("被约束拒绝的写入不应留下任何行，实际 %d 行", rejectedRows)
	}
}
