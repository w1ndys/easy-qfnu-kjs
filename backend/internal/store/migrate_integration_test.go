package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
	if applied != 5 {
		t.Fatalf("schema_migration 期望 5 行（0001 基线 + 0002 每轮归属 + 0003 发布标记 + 0004 管理会话 + 0005 默认 cron 改 5 字段），实际 %d", applied)
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

// TestMigrateAdminSessionAgainstRealDB 覆盖 0004：管理会话表从干净库建出、
// 重复迁移走「已应用则跳过」，且列、主键、CHECK 与索引真的生效——
// 约束名对不等于规则对，只查名字会漏掉「写反了但名字还在」。
func TestMigrateAdminSessionAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	// openCollectRunTestDB 已经迁移过一轮；再跑一次证明 0004 也只记一次账、
	// 不会因重复建表/建索引报错。
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("重复迁移应为幂等，实际失败: %v", err)
	}

	var recorded int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migration WHERE version = '0004_admin_session'`).Scan(&recorded); err != nil {
		t.Fatalf("读取 0004 记账失败: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("schema_migration 应恰有 1 条 0004_admin_session，实际 %d", recorded)
	}

	// 逐列断言形态：漏 NOT NULL、created_at 丢默认值都在这里暴露，
	// 而不是等仓储写入时报一个离现场很远的错。
	type column struct {
		dataType   string
		isNullable string
		hasDefault bool
	}
	wantColumns := map[string]column{
		"token_hash": {dataType: "text", isNullable: "NO", hasDefault: false},
		"csrf_token": {dataType: "text", isNullable: "NO", hasDefault: false},
		"created_at": {dataType: "timestamp with time zone", isNullable: "NO", hasDefault: true},
		"expires_at": {dataType: "timestamp with time zone", isNullable: "NO", hasDefault: false},
	}
	rows, err := pool.Query(ctx, `
SELECT column_name, data_type, is_nullable, column_default
  FROM information_schema.columns
 WHERE table_schema = 'public' AND table_name = 'admin_session'`)
	if err != nil {
		t.Fatalf("读取 admin_session 列失败: %v", err)
	}
	gotColumns := make(map[string]column, len(wantColumns))
	for rows.Next() {
		var name string
		var col column
		var def *string
		if err := rows.Scan(&name, &col.dataType, &col.isNullable, &def); err != nil {
			rows.Close()
			t.Fatalf("扫描 admin_session 列失败: %v", err)
		}
		col.hasDefault = def != nil
		gotColumns[name] = col
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 admin_session 列失败: %v", err)
	}
	if len(gotColumns) != len(wantColumns) {
		t.Fatalf("admin_session 应有 %d 列，实际 %d（%v）", len(wantColumns), len(gotColumns), gotColumns)
	}
	for name, want := range wantColumns {
		got, ok := gotColumns[name]
		if !ok {
			t.Fatalf("admin_session 缺少 %s 列", name)
		}
		if got != want {
			t.Fatalf("%s 列形态不符: got %+v want %+v", name, got, want)
		}
	}

	// 主键必须落在 token_hash：认证查询按它走，换了列就变成全表扫描。
	var pkDef string
	if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = 'admin_session' AND c.contype = 'p'`).Scan(&pkDef); err != nil {
		t.Fatalf("admin_session 缺少主键: %v", err)
	}
	if !strings.Contains(pkDef, "token_hash") {
		t.Fatalf("admin_session 主键应落在 token_hash 上，实际 %q", pkDef)
	}

	// 过期顺序约束必须存在，且规则文本就是 expires_at > created_at。
	var checkDef string
	if err := pool.QueryRow(ctx, `
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
 WHERE t.relname = 'admin_session' AND c.contype = 'c'`).Scan(&checkDef); err != nil {
		t.Fatalf("admin_session 缺少 expires_at > created_at 约束: %v", err)
	}
	if !strings.Contains(checkDef, "expires_at > created_at") {
		t.Fatalf("CHECK 应约束 expires_at > created_at，实际 %q", checkDef)
	}

	// 清理过期会话要按过期时刻筛，索引必须真的建在 expires_at 上。
	var indexDef string
	if err := pool.QueryRow(ctx, `
SELECT indexdef
  FROM pg_indexes
 WHERE schemaname = 'public' AND tablename = 'admin_session' AND indexname = 'admin_session_expires'`).Scan(&indexDef); err != nil {
		t.Fatalf("admin_session 缺少 admin_session_expires 索引: %v", err)
	}
	if !strings.Contains(indexDef, "(expires_at)") {
		t.Fatalf("admin_session_expires 应按 expires_at 建索引，实际 %q", indexDef)
	}

	var tableComment *string
	if err := pool.QueryRow(ctx,
		`SELECT obj_description('public.admin_session'::regclass, 'pg_class')`).Scan(&tableComment); err != nil {
		t.Fatalf("读取 admin_session 表注释失败: %v", err)
	}
	if tableComment == nil || !strings.Contains(*tableComment, "token_hash") {
		t.Fatalf("admin_session 应有说明 token_hash 口径的表注释，实际 %v", tableComment)
	}

	base := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

	// 合法行必须能落库：只测拒绝会漏掉「约束写反了、连合法行也拒绝」。
	if _, err := pool.Exec(ctx, `
INSERT INTO admin_session (token_hash, csrf_token, created_at, expires_at)
VALUES ('hash-valid', 'csrf-valid', $1, $2)`, base, base.Add(12*time.Hour)); err != nil {
		t.Fatalf("合法的管理会话行不应被约束拒绝: %v", err)
	}

	// 过期时刻不晚于创建时刻的行必须被 CHECK 拒绝，且断言 SQLSTATE 与约束名：
	// 只判 err != nil 的话，表名拼错、列不存在这类错误也会让用例变绿。
	rejected := []struct {
		name      string
		expiresAt time.Time
	}{
		{"过期时刻早于创建时刻", base.Add(-time.Second)},
		{"过期时刻等于创建时刻", base},
	}
	for i, c := range rejected {
		_, err := pool.Exec(ctx, `
INSERT INTO admin_session (token_hash, csrf_token, created_at, expires_at)
VALUES ($1, 'csrf-invalid', $2, $3)`, fmt.Sprintf("hash-rejected-%d", i), base, c.expiresAt)
		if err == nil {
			t.Fatalf("admin_session 应拒绝「%s」的行", c.name)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("「%s」应以 *pgconn.PgError 报错，实际 %T: %v", c.name, err, err)
		}
		if pgErr.Code != "23514" {
			t.Fatalf("「%s」应以 SQLSTATE 23514（check_violation）报错，实际 %s: %v",
				c.name, pgErr.Code, err)
		}
		if pgErr.ConstraintName != "admin_session_expiry_after_created" {
			t.Fatalf("「%s」应命中断言约束 admin_session_expiry_after_created，实际 %q",
				c.name, pgErr.ConstraintName)
		}
	}

	var rowCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_session`).Scan(&rowCount); err != nil {
		t.Fatalf("统计 admin_session 行数失败: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("被约束拒绝的写入不应留下任何行，admin_session 期望 1 行，实际 %d", rowCount)
	}
}

// TestMigrateDefaultCronFiveFieldsAgainstRealDB 覆盖 0005：0001 的种子把 cron_expr
// 写成 6 段的 `0 10 4 * * 6`，而 cronexpr 只认 5 字段，0005 在升级时把它就地改成
// `10 4 * * 6`。三条断言分别对应新库的最终值、旧库的升级改写、以及重复执行不改
// 已更新的值——幂等只靠 WHERE 的旧值条件，不依赖 schema_migration 的跳过。
func TestMigrateDefaultCronFiveFieldsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)

	// 新库：0001 种下旧值，0005 在同一次迁移里改写。
	if got := readCronExpr(ctx, t, pool); got != "10 4 * * 6" {
		t.Fatalf("全新迁移后 cron_expr 应为 \"10 4 * * 6\"，实际 %q", got)
	}

	// 升级路径：还原成 0001 的原值并抹掉 0005 记账，重跑迁移应就地改写。
	legacyUpdatedAt := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	mustExec(ctx, t, pool, `
UPDATE settings SET value = '0 10 4 * * 6', updated_at = $1 WHERE key = 'cron_expr'`, legacyUpdatedAt)
	mustExec(ctx, t, pool, `DELETE FROM schema_migration WHERE version = '0005_default_cron_5_fields'`)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("从旧种子升级到 0005 失败: %v", err)
	}
	if got := readCronExpr(ctx, t, pool); got != "10 4 * * 6" {
		t.Fatalf("升级后 cron_expr 应为 \"10 4 * * 6\"，实际 %q", got)
	}

	updatedAt := readCronUpdatedAt(ctx, t, pool)
	if !updatedAt.After(legacyUpdatedAt) {
		t.Fatalf("改写应刷新 updated_at，实际 %s 未晚于 %s", updatedAt, legacyUpdatedAt)
	}

	// 幂等：新值已就位时再执行一次 0005，值与时标都不应变化。
	mustExec(ctx, t, pool, `DELETE FROM schema_migration WHERE version = '0005_default_cron_5_fields'`)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("重复执行 0005 应为幂等，实际失败: %v", err)
	}
	if got := readCronExpr(ctx, t, pool); got != "10 4 * * 6" {
		t.Fatalf("重复执行后 cron_expr 应保持 \"10 4 * * 6\"，实际 %q", got)
	}
	if repeatedAt := readCronUpdatedAt(ctx, t, pool); !repeatedAt.Equal(updatedAt) {
		t.Fatalf("重复执行不应改动 updated_at：之前 %s，之后 %s", updatedAt, repeatedAt)
	}

	// 管理员改成别的表达式后，0005 的定位条件不再命中，重跑也不能覆盖。
	mustExec(ctx, t, pool, `UPDATE settings SET value = '30 6 * * 1' WHERE key = 'cron_expr'`)
	mustExec(ctx, t, pool, `DELETE FROM schema_migration WHERE version = '0005_default_cron_5_fields'`)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("管理员自定义 cron 后重跑 0005 失败: %v", err)
	}
	if got := readCronExpr(ctx, t, pool); got != "30 6 * * 1" {
		t.Fatalf("0005 不应覆盖管理员自定义的 cron_expr，实际 %q", got)
	}
}

// readCronExpr 读 settings 里 cron_expr 的当前值；行缺失时直接判失败，
// 免得「读不到」被当成空串蒙过断言。
func readCronExpr(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	var value string
	if err := pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE key = 'cron_expr'`).Scan(&value); err != nil {
		t.Fatalf("读取 cron_expr 失败: %v", err)
	}

	return value
}

// readCronUpdatedAt 读 cron_expr 行的 updated_at，用来断言「改了」与「没改」。
func readCronUpdatedAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()

	var updatedAt time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM settings WHERE key = 'cron_expr'`).Scan(&updatedAt); err != nil {
		t.Fatalf("读取 cron_expr 的 updated_at 失败: %v", err)
	}

	return updatedAt
}
