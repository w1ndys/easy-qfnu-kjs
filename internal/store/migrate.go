// 本文件属于 data 层：把内嵌的 SQL 迁移按版本顺序应用到数据库。
// 迁移文件是数据库结构的唯一产出路径，结构本身以 docs/contract/db.v2.sql 为准。

package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrationFiles 把 migrations 目录编进二进制，部署时不需要额外带 SQL 文件。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration 是一份迁移：版本名（文件名去掉扩展名）与 SQL 正文。
type migration struct {
	version string
	sql     string
}

// Migrate 建立迁移记录表，再按版本名顺序执行尚未应用的迁移。
// 已应用的版本会跳过，因此重复执行是幂等的；每个迁移各自一个事务，失败即整份回滚。
// 返回值是本次真正执行的版本名列表。
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	// 迁移记录表必须先存在，否则无从判断哪些版本已经应用
	if err := s.ensureMigrationTable(ctx); err != nil {
		return nil, err
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}
	files, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	ran := make([]string, 0, len(files))
	for _, m := range files {
		// 已应用过的版本不再执行，保证迁移可以反复跑
		if applied[m.version] {
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			return ran, fmt.Errorf("迁移 %s 失败: %w", m.version, err)
		}
		ran = append(ran, m.version)
	}
	return ran, nil
}

// ensureMigrationTable 建立迁移记录表，记录每个已应用版本与时间。
func (s *Store) ensureMigrationTable(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_migration (
  version    text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
)`
	// 这条 DDL 自身不能依赖任何迁移，否则陷入先有鸡还是先有蛋
	if _, err := s.pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("建立迁移记录表失败: %w", err)
	}
	return nil
}

// appliedVersions 读出已经应用过的版本名集合。
func (s *Store) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migration`)
	if err != nil {
		return nil, fmt.Errorf("读取迁移记录失败: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("扫描迁移记录失败: %w", err)
		}
		applied[version] = true
	}
	// 遍历中途出错时不能当作读完，否则会漏掉迁移记录
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历迁移记录失败: %w", err)
	}
	return applied, nil
}

// applyMigration 在单个事务里执行一份迁移，并写入版本记录。
func (s *Store) applyMigration(ctx context.Context, m migration) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	// 任意一步失败都要整体回滚，避免留下半套表结构
	defer func() { _ = tx.Rollback(ctx) }()

	// 迁移文件里是多条语句，pgx 在无参数时走简单查询协议，可以整份执行
	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return fmt.Errorf("执行迁移 SQL 失败: %w", err)
	}
	// 版本记录与结构变更必须同事务落库，否则重跑会重复建表
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migration (version) VALUES ($1)`, m.version); err != nil {
		return fmt.Errorf("写入迁移记录失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// loadMigrations 读出全部内嵌迁移，按版本名升序返回。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("读取内嵌迁移目录失败: %w", err)
	}

	loaded := make([]migration, 0, len(entries))
	for _, entry := range entries {
		// 只认 .sql 文件，其他内容（子目录、编辑器临时文件）不参与迁移
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("读取迁移 %s 失败: %w", entry.Name(), err)
		}
		loaded = append(loaded, migration{
			version: strings.TrimSuffix(entry.Name(), ".sql"),
			sql:     string(body),
		})
	}
	// 文件名的字典序就是执行顺序，版本名按 0001、0002 … 编号保证顺序稳定
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].version < loaded[j].version })
	return loaded, nil
}
