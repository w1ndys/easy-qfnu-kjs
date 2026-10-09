// 本文件是 entry 层：查询服务的 HTTP 入口。
// 用法：DATABASE_URL=postgres://... go run ./cmd/server -addr 127.0.0.1:8080

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	// 容器里可能没有 tzdata，把时区数据库编进二进制，Asia/Shanghai 才能加载
	_ "time/tzdata"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// main 装配存储、查询服务与 HTTP 服务，并在收到信号时优雅退出。
func main() {
	// 监听地址不是业务配置，用启动参数给，业务设置一律走数据库
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// 时区固定 Asia/Shanghai：日历换算与响应里的时间都按它输出
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		logger.Error("加载时区失败", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DATABASE_URL 是部署级变量，只从环境读取
	dataStore, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		logger.Error("打开数据库失败", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(query.NewService(dataStore, location, time.Now), location, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("查询服务启动", "addr", *addr)
		// 正常关闭会返回 ErrServerClosed，那不是故障
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务异常退出", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 给在途请求 10 秒收尾，超时就放弃
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("关闭服务失败", "error", err)
	}
}
