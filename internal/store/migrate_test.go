package store

// 本文件是 data 层的集成测试：需要一个真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。

import (
	"context"
	"os"
	"testing"
	"time"
)

// 契约表清单：迁移跑完后这些表必须齐全（含本阶段新增的三张运行记录表）
var contractTables = []string{
	"dict", "dict_state", "dict_symbol",
	"axis", "axis_node",
	"term", "term_week",
	"room", "settings", "release", "observation",
	"collect_run", "collect_run_week", "alert_log",
}

// openTestStore 连接测试库；没给连接串就跳过，而不是让 go test 整体失败。
func openTestStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	// 集成测试必须显式指定测试库，避免误连开发库
	if dsn == "" {
		t.Skip("未设置 TEST_DATABASE_URL，跳过存储层集成测试")
	}

	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// TestMigrateEnablesContractTables 断言迁移后契约表与运行记录表全部存在。
func TestMigrateEnablesContractTables(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// 先跑迁移，确保测试不依赖外部先手工建库
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	for _, table := range contractTables {
		var found *string
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, "public."+table).Scan(&found); err != nil {
			t.Fatalf("查询表 %s 是否存在失败: %v", table, err)
		}
		// to_regclass 返回 NULL 说明这张表不存在，迁移漏了
		if found == nil {
			t.Errorf("表 %s 不存在，迁移未覆盖", table)
		}
	}
}

// TestMigrateIsIdempotent 断言迁移重复执行是幂等的：第二次不再执行任何版本。
func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("重复迁移失败: %v", err)
	}
	// 第二次应当无迁移可跑，否则说明版本记录没生效
	if len(applied) != 0 {
		t.Errorf("重复迁移仍然执行了迁移: %v", applied)
	}
}

// TestSeedDictAndAxis 校验种子字典与节次轴的内容符合契约。
func TestSeedDictAndAxis(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	// 契约种子表共 11 个状态
	var stateCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dict_state WHERE dict_version = 1`).Scan(&stateCount); err != nil {
		t.Fatalf("统计状态失败: %v", err)
	}
	if stateCount != 11 {
		t.Errorf("状态数 = %d，期望 11", stateCount)
	}

	// 可用状态必须只有 free 与 fully_free
	usable, err := s.usableStates(ctx)
	if err != nil {
		t.Fatalf("查询可用状态失败: %v", err)
	}
	if _, ok := usable["free"]; !ok {
		t.Error("free 应当是可用状态")
	}
	if _, ok := usable["fully_free"]; !ok {
		t.Error("fully_free 应当是可用状态")
	}
	if len(usable) != 2 {
		t.Errorf("可用状态 = %v，期望只有 free 与 fully_free", usable)
	}

	// 符号表里的 Ｘ 必须是全角码点 U+FF38，写成 ASCII 就匹配不到上游
	var lockedSymbol string
	if err := s.pool.QueryRow(ctx,
		`SELECT symbol FROM dict_symbol WHERE dict_version = 1 AND state_key = 'locked'`).Scan(&lockedSymbol); err != nil {
		t.Fatalf("查询锁定符号失败: %v", err)
	}
	if lockedSymbol != "\uFF38" {
		t.Errorf("锁定符号 = %q，期望全角 Ｘ（U+FF38）", lockedSymbol)
	}

	// 节次轴 1 版有 12 个节点、5 个大节
	var nodeCount, blockCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM axis_node WHERE axis_version = 1`).Scan(&nodeCount); err != nil {
		t.Fatalf("统计节点失败: %v", err)
	}
	if nodeCount != 12 {
		t.Errorf("节点数 = %d，期望 12", nodeCount)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT block) FROM axis_node WHERE axis_version = 1`).Scan(&blockCount); err != nil {
		t.Fatalf("统计大节失败: %v", err)
	}
	if blockCount != 5 {
		t.Errorf("大节数 = %d，期望 5", blockCount)
	}

	// 采集调度默认值必须存在，否则采集器没有可读的 cron
	var cronExpr string
	if err := s.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = 'cron_expr'`).Scan(&cronExpr); err != nil {
		t.Fatalf("查询 cron_expr 失败: %v", err)
	}
	if cronExpr == "" {
		t.Error("cron_expr 默认值为空")
	}
}

// TestReleaseOneCurrent 断言同一时刻只能有一个 current 版本。
func TestReleaseOneCurrent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("执行迁移失败: %v", err)
	}

	// 用唯一学期名隔离测试数据，避免和开发库里的真实数据冲突
	term := "test-term-" + time.Now().Format("20060102150405.000000")
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO term (term, total_weeks) VALUES ($1, 20)`, term); err != nil {
		t.Fatalf("插入测试学期失败: %v", err)
	}
	// 学期是 release 的外键，清理时必须先删 release
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM release WHERE term = $1`, term); err != nil {
			t.Logf("清理 release 失败: %v", err)
		}
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM term WHERE term = $1`, term); err != nil {
			t.Logf("清理 term 失败: %v", err)
		}
	})

	releaseIDs := []string{term + "#1", term + "#2"}
	// 第一个 current 应当成功
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
		 VALUES ($1, $2, now(), 1, 1, true)`, releaseIDs[0], term); err != nil {
		t.Fatalf("插入第一个 current 失败: %v", err)
	}

	// 第二个 current 必须被唯一索引拦下
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
		 VALUES ($1, $2, now(), 1, 1, true)`, releaseIDs[1], term); err == nil {
		t.Fatal("插入第二个 current 竟然成功，release_one_current 唯一索引未生效")
	}
}

// usableStates 返回字典 1 版里 available 为真的状态键集合。
func (s *Store) usableStates(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT state_key FROM dict_state WHERE dict_version = 1 AND available`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	usable := make(map[string]bool)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		usable[key] = true
	}
	// 遍历出错时不能当成读完，否则可用集合会被低估
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usable, nil
}
