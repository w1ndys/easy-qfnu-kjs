// Package store 负责数据库连接池与仓储实现。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// maxConns 是连接池上限：用户查询与采集批量写入共用这 25 条连接。
	// 取 25 的依据是 PG 默认 max_connections=100 与单副本部署：留足备份、迁移与排障的余量。
	maxConns = 25
	// readyWait 是启动时等待数据库就绪的总时长，超过即放弃启动。
	readyWait = 30 * time.Second
	// readyInterval 是两次探活之间的间隔，避免 30 秒内把日志打满。
	readyInterval = time.Second
	// pingTimeout 是单次探活的超时。
	pingTimeout = 5 * time.Second
)

// Open 建立连接池并在就绪窗口内等待数据库可用；任一步失败都返回错误供入口退出。
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	// 连接串语法错误与连不上是两回事，但都需要入口拒绝启动，统一包成同一类错误。
	if err != nil {
		return nil, fmt.Errorf("%w: 连接串解析失败: %w", model.ErrStoreUnavailable, err)
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: 连接池创建失败: %w", model.ErrStoreUnavailable, err)
	}

	deadline := time.Now().Add(readyWait)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
		pingErr := pool.Ping(pingCtx)
		cancel()
		// 库还不接受连接是启动期常态，先当作可重试，直到窗口耗尽。
		if pingErr == nil {
			return pool, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("%w: %s 内未能连上数据库: %w", model.ErrStoreUnavailable, readyWait, pingErr)
		}
		// 等待期间若上游 ctx 被取消（比如进程收到终止信号），立刻放弃并释放连接池。
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("%w: %w", model.ErrStoreUnavailable, errors.Join(ctx.Err(), pingErr))
		case <-time.After(readyInterval):
		}
	}
}
