package store

import (
	"context"
	"os"
	"testing"
	"time"
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
	if applied != 1 {
		t.Fatalf("schema_migration 期望 1 行，实际 %d", applied)
	}
}
