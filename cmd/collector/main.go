// 本文件是 entry 层：采集器进程入口。
// 用法：DATABASE_URL=postgres://... UPSTREAM_BASE_URL=http://... go run ./cmd/collector [-once]
// 采集器是唯一访问教务系统的进程；手动开始只在进程内，不挂到公开查询端口（设计文档 Architecture）。

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	// 容器里可能没有 tzdata，把时区数据库编进二进制，Asia/Shanghai 才能加载
	_ "time/tzdata"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// main 装配采集器：读部署级变量，从库读调度表达式，启动内置定时器或跑一轮手动全量。
func main() {
	// -once 是进程内手动开始：跑一轮全量就退出，不启动定时器
	once := flag.Bool("once", false, "只跑一轮全量后退出（手动开始）")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// 时区固定 Asia/Shanghai：周历采样、告警去重键与房间首见时间都按它算
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		logger.Error("加载时区失败", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DATABASE_URL 与上游地址都是部署级变量，只从环境读取；业务设置一律走数据库
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

	// 手动开始：与定时触发调用同一个 Start，跑完这一轮就退出
	if *once {
		if err := collector.Start(ctx); err != nil {
			logger.Error("全量同步失败", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := runScheduled(ctx, collector, dataStore, location, logger); err != nil {
		logger.Error("采集器退出", "error", err)
		os.Exit(1)
	}
}

// runScheduled 读 cron_expr 并常驻：到点跑一轮全量，收到信号后等正在跑的那一轮收尾。
func runScheduled(ctx context.Context, collector *Collector, dataStore *store.Store, location *time.Location, logger *slog.Logger) error {
	expr, found, err := dataStore.SettingValue(ctx, store.SettingKeyCronExpr)
	if err != nil {
		return err
	}
	// 没有调度表达式就不该按固定时间采集：宁可拒绝启动，也不猜一个时间去采
	if !found {
		return errors.New("settings 里没有 cron_expr，先执行迁移或让管理员在面板配置")
	}

	schedule, err := newScheduler(ctx, expr, location, func(runCtx context.Context) {
		// 定时与手动是同一个入口，失败只记日志：下一轮照常按表达式来
		if err := collector.Start(runCtx); err != nil {
			logger.Error("定时全量失败", "error", err)
		}
	})
	if err != nil {
		return err
	}
	schedule.Start()
	logger.Info("采集器启动", "cron", expr)

	<-ctx.Done()
	// 等正在跑的那一轮收尾，不要把它腰斩在写库中间
	<-schedule.Stop().Done()
	return nil
}
