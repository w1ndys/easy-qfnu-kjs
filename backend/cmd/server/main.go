// Package main 是服务进程入口。
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/config"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// main 只把 run 的返回值转成退出码：0 正常，1 非正常终止。
func main() {
	os.Exit(run())
}

// run 读配置、建日志、连数据库、打印启动信息、等终止信号，返回进程退出码。
func run() int {
	cfg, err := config.Load()
	// 配置非法直接返回 1：这时还没有配置可信的日志级别，用 info 保证这条错误一定落盘。
	if err != nil {
		newLogger(os.Stdout, config.LogInfo).Error("启动失败", "err", err)
		return 1
	}

	log := newLogger(os.Stdout, cfg.LogLevel)

	// 信号接成 ctx：建池等待与将来的采集任务都靠它感知终止。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	// 连不上库同样属于启动失败，退出码与配置非法一样是 1。
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}
	// 退出时先记一条再关池，让日志能反映关闭前的池状态。
	defer func() {
		pool.Close()
		log.Info("连接池已关闭")
	}()

	// 结构变更在对外服务之前完成：失败即拒绝启动，避免带着半截库对外提供数据。
	if err := store.Migrate(ctx, pool); err != nil {
		log.Error("结构变更失败", "err", err)
		return 1
	}

	// 启动日志只打非敏感项：连接串拆成主机与库名，密码与查询参数不出现。
	dsn, _ := url.Parse(cfg.DatabaseURL)
	version := "devel"
	// 编译期版本来自构建信息；开发态没有 tag 时保留 devel。
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	log.Info("服务启动",
		"listen", cfg.ListenAddr,
		"log_level", string(cfg.LogLevel),
		"ocr_timeout", cfg.OCRTimeout.String(),
		"db_host", dsn.Host,
		"db_name", strings.TrimPrefix(dsn.Path, "/"),
		"version", version,
	)

	// 查询服务只拿到「按日期查学期周」这一项能力，连接池被闭包挡在里面。
	queries := query.New(func(ctx context.Context, date time.Time) (model.TermWeek, bool, error) {
		return store.TermWeekAt(ctx, pool, date)
	})

	// HTTP 服务在独立 goroutine 里跑，主流程只负责等待终止信号。
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(log, pool, queries).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		// 正常关闭时 ListenAndServe 返回 ErrServerClosed，不算异常。
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", "err", err)
		}
	}()
	log.Info("HTTP 监听", "addr", cfg.ListenAddr)

	<-ctx.Done()
	// 收到终止信号不等同步任务，只给在途只读请求 5 秒收尾，超时直接断开。
	log.Info("收到终止信号，立即退出")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 关闭未在宽限内完成", "err", err)
	}
	return 0
}

// newLogger 按配置级别构造结构化日志器；w 由调用方给出，便于测试注入。
func newLogger(w io.Writer, level config.LogLevel) *slog.Logger {
	// 配置里是字符串级别，slog 要数值级别，这里集中映射一次。
	var slogLevel slog.Level
	switch level {
	case config.LogDebug:
		slogLevel = slog.LevelDebug
	case config.LogWarn:
		slogLevel = slog.LevelWarn
	case config.LogError:
		slogLevel = slog.LevelError
	default:
		// 未知级别已在 config 处拒绝启动，这里兜底按 info，避免日志整体静音。
		slogLevel = slog.LevelInfo
	}

	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slogLevel}))
}
