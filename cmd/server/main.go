// 本文件是 entry 层：唯一常驻进程入口。
// 用法：DATABASE_URL=postgres://... UPSTREAM_BASE_URL=http://... go run ./cmd/server -addr 127.0.0.1:8080
// 它同时听公开查询，并按 settings.cron_expr 在本进程里跑采集。
// 公开查询处理函数不登录教务、不读教师账号。cmd/migrate 仍是部署时单独跑一次的命令。
// 手动同步以后走管理接口，由本进程执行；这里不提供 -once。

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

// main 装配查询与采集，并在收到信号时先停 HTTP，再等正在跑的那一轮采集收尾。
func main() {
	// 监听地址不是业务配置，用启动参数给；业务设置一律走数据库
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	location, err := loadShanghai(logger)
	if err != nil {
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dataStore, err := store.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		logger.Error("打开数据库失败", "error", err)
		os.Exit(1)
	}
	defer dataStore.Close()

	collector, err := newCollector(collectorConfig{
		store:    dataStore,
		baseURL:  os.Getenv("UPSTREAM_BASE_URL"),
		logger:   logger,
		location: location,
		now:      time.Now,
	})
	if err != nil {
		logger.Error("装配采集器失败", "error", err)
		os.Exit(1)
	}

	// 定时表达式非法时先退出：不要先把查询端口打开，再发现采集根本起不来
	schedule, err := startSchedule(ctx, collector, dataStore, location, logger)
	if err != nil {
		logger.Error("启动定时采集失败", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(query.NewService(dataStore, location, time.Now), location, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go serve(server, logger, *addr, stop)

	<-ctx.Done()
	shutdownServer(server, logger)
	// 等正在跑的那一轮收尾，不要把它腰斩在写库中间
	<-schedule.Stop().Done()
}

// loadShanghai 加载固定时区：日历换算、告警去重键与响应时间都按它算。
func loadShanghai(logger *slog.Logger) (*time.Location, error) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		logger.Error("加载时区失败", "error", err)
		return nil, err
	}
	return location, nil
}

// startSchedule 读 cron_expr 并启动内置定时器。到点调用的是采集器的 Start，与以后的手动同步同一套全量。
func startSchedule(ctx context.Context, collector *Collector, dataStore *store.Store, location *time.Location, logger *slog.Logger) (*scheduler, error) {
	expr, found, err := dataStore.SettingValue(ctx, store.SettingKeyCronExpr)
	if err != nil {
		return nil, err
	}
	// 没有调度表达式就不该按固定时间采集：宁可拒绝启动，也不猜一个时间去采
	if !found {
		return nil, errors.New("settings 里没有 cron_expr，先执行迁移或让管理员在面板配置")
	}

	schedule, err := newScheduler(ctx, expr, location, func(runCtx context.Context) {
		// 退出信号只拦住下一轮。这一轮用不会被取消的上下文跑完，避免写库写到一半
		if err := collector.Start(context.WithoutCancel(runCtx)); err != nil {
			logger.Error("定时全量失败", "error", err)
		}
	})
	if err != nil {
		return nil, err
	}
	schedule.Start()
	logger.Info("定时采集已启动", "cron", expr)
	return schedule, nil
}

// serve 监听公开查询。正常关闭返回 ErrServerClosed，那不是故障。
func serve(server *http.Server, logger *slog.Logger, addr string, stop context.CancelFunc) {
	logger.Info("服务启动", "addr", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP 服务异常退出", "error", err)
		stop()
	}
}

// shutdownServer 给在途请求 10 秒收尾，超时就放弃。
func shutdownServer(server *http.Server, logger *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("关闭服务失败", "error", err)
	}
}
