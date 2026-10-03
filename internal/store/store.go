// 本文件属于 data 层：负责 PostgreSQL 连接池的建立与释放。
// 采集器、查询服务与面板后端共用同一个连接串（DATABASE_URL）。

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 是数据层入口，内部持有一个 PostgreSQL 连接池。
type Store struct {
	pool *pgxpool.Pool
}

// Open 按连接串建立连接池，并主动探活一次。
// 传入空连接串会直接报错，避免误连本机默认库。
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	// 连接串是部署级配置，缺失时没有任何兜底值可用
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("DATABASE_URL 为空，无法连接数据库")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("解析 DATABASE_URL 失败: %w", err)
	}
	// 本机单实例部署，连接数保持小规模即可，避免打满 PostgreSQL
	cfg.MaxConns = 8
	cfg.MaxConnLifetime = time.Hour
	// 连接名落到 pg_stat_activity，排查慢查询时能看出是谁在连
	cfg.ConnConfig.RuntimeParams["application_name"] = "easy-qfnu-kjs"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("建立连接池失败: %w", err)
	}
	// 建池是懒连接，这里 Ping 一次，让连接串写错在启动时就暴露
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 释放连接池，服务退出时调用。
func (s *Store) Close() {
	// 池对象为空说明构造失败过，这里不重复释放
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}
