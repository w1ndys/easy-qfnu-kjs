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
	if applied != 3 {
		t.Fatalf("schema_migration 期望 3 行（0001 基线 + 0002 每轮归属 + 0003 发布标记），实际 %d", applied)
	}

	// 0003 只新增列、不改 0001/0002：发布标记必须存在且是可空 timestamptz，
	// 这样「一轮只能发布一次」才不依赖会被 ON DELETE SET NULL 重置的 release_id。
	var releasedAtType string
	if err := pool.QueryRow(ctx, `
SELECT data_type
  FROM information_schema.columns
 WHERE table_name = 'collect_run'
   AND column_name = 'released_at'`).Scan(&releasedAtType); err != nil {
		t.Fatalf("collect_run 缺少 released_at 列: %v", err)
	}
	if releasedAtType != "timestamp with time zone" {
		t.Fatalf("released_at 应是 timestamptz，实际 %q", releasedAtType)
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

// TestMigrateBackfillsReleasedAtAgainstRealDB 覆盖 0003 的升级路径：迁移前已经
// 关联过版本的轮次在建列后要回填 released_at，否则升级后它们还能再发布一次。
// 通过「删列 + 清记账 + 重跑迁移」模拟从 0002 升级到 0003。
func TestMigrateBackfillsReleasedAtAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	if _, err := pool.Exec(ctx, "ALTER TABLE collect_run DROP COLUMN released_at"); err != nil {
		t.Fatalf("删除 released_at 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM schema_migration WHERE version = '0003_collect_run_released'"); err != nil {
		t.Fatalf("清理 0003 记账失败: %v", err)
	}

	publishedAt := time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC)
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ('2026-2027-1', 20)")
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ('rel-legacy', '2026-2027-1', $1, 1, 1, false)`, publishedAt)

	var linkedRunID, unlinkedRunID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term, release_id, updated_at)
VALUES ($1, 'success', '2026-2027-1', 'rel-legacy', $2)
RETURNING run_id`, publishedAt.Add(-time.Hour), publishedAt).Scan(&linkedRunID); err != nil {
		t.Fatalf("创建已关联版本的轮次失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term, updated_at)
VALUES ($1, 'failed', '2026-2027-1', $1)
RETURNING run_id`, publishedAt).Scan(&unlinkedRunID); err != nil {
		t.Fatalf("创建未发布轮次失败: %v", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("从 0002 升级到 0003 失败: %v", err)
	}

	var releasedAt time.Time
	if err := pool.QueryRow(ctx,
		"SELECT released_at FROM collect_run WHERE run_id = $1", linkedRunID).Scan(&releasedAt); err != nil {
		t.Fatalf("读取回填后的 released_at 失败: %v", err)
	}
	if !releasedAt.Equal(publishedAt) {
		t.Fatalf("升级回填不符: got %s want %s", releasedAt, publishedAt)
	}

	var unlinked *time.Time
	if err := pool.QueryRow(ctx,
		"SELECT released_at FROM collect_run WHERE run_id = $1", unlinkedRunID).Scan(&unlinked); err != nil {
		t.Fatalf("读取未发布轮次的 released_at 失败: %v", err)
	}
	if unlinked != nil {
		t.Fatalf("未发布轮次不应被回填，实际 %s", unlinked)
	}
}
