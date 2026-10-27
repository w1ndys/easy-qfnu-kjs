package store

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/migrations"
)

// createMigrationTable 保证记账表先存在；基线脚本自己也会建它，所以用 IF NOT EXISTS。
const createMigrationTable = `CREATE TABLE IF NOT EXISTS schema_migration (
  version    text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
)`

// querier 只取执行期需要的查询能力，方便把「读已应用版本」单独测。
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Migrate 按文件名顺序执行未应用的脚本，整轮一个事务：任一脚本失败则全部回滚。
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	names, err := fs.Glob(migrations.Files, "*.sql")
	// 打包漏掉脚本属于发布缺陷，宁可启动失败也不要带着空库跑起来。
	if err != nil || len(names) == 0 {
		return fmt.Errorf("%w: 迁移脚本读取失败（err=%w, scripts=%d）", model.ErrMigrationFailed, err, len(names))
	}
	sort.Strings(names)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: 事务开启失败: %w", model.ErrMigrationFailed, err)
	}
	// 提交成功后再回滚是空操作，所有失败路径都靠这里收尾。
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, createMigrationTable); err != nil {
		return fmt.Errorf("%w: 记账表创建失败: %w", model.ErrMigrationFailed, err)
	}

	applied, err := appliedVersions(ctx, tx)
	if err != nil {
		return err
	}

	// 逐个脚本执行：已应用的跳过，保证重复启动幂等。
	for _, name := range names {
		version := strings.TrimSuffix(path.Base(name), ".sql")
		if applied[version] {
			continue
		}
		if err := applyOne(ctx, tx, name, version); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: 提交失败: %w", model.ErrMigrationFailed, err)
	}
	return nil
}

// applyOne 执行单个脚本并记账，把「读文件、执行、写记录」三步绑在一起。
func applyOne(ctx context.Context, tx pgx.Tx, name, version string) error {
	body, err := migrations.Files.ReadFile(name)
	if err != nil {
		return fmt.Errorf("%w: 读取 %s 失败: %w", model.ErrMigrationFailed, version, err)
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("%w: 执行 %s 失败: %w", model.ErrMigrationFailed, version, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migration (version) VALUES ($1)", version); err != nil {
		return fmt.Errorf("%w: 记录 %s 失败: %w", model.ErrMigrationFailed, version, err)
	}
	return nil
}

// appliedVersions 读出已应用的版本集合，供执行前跳过。
func appliedVersions(ctx context.Context, q querier) (map[string]bool, error) {
	rows, err := q.Query(ctx, "SELECT version FROM schema_migration")
	if err != nil {
		return nil, fmt.Errorf("%w: 读取已应用版本失败: %w", model.ErrMigrationFailed, err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("%w: 扫描版本失败: %w", model.ErrMigrationFailed, err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本失败: %w", model.ErrMigrationFailed, err)
	}
	return applied, nil
}
