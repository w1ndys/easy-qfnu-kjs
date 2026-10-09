// 本文件是 entry 层：采集器内置的定时器。
// 到达 cron_expr 指定的时间就调用传进来的 run；run 与手动开始是同一个入口，
// 因此定时与手动跑的是同一套全量（需求 5.4、5.5 与设计文档 Architecture）。

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduler 是内置定时器：一轮全量在它自己的 goroutine 里跑，进程不为此阻塞。
type scheduler struct {
	engine *cron.Cron // 六段式含秒的 cron 引擎
}

// newScheduler 按 cron_expr 建定时器；表达式非法时返回错误，不静默退化成不采。
func newScheduler(ctx context.Context, expr string, location *time.Location, run func(context.Context)) (*scheduler, error) {
	// 六段式含秒，与 settings 种子和面板里的写法一致；时间判定按传入的时区
	engine := cron.New(cron.WithSeconds(), cron.WithLocation(location))
	if _, err := engine.AddFunc(expr, func() {
		// 进程正在退出时不再起新一轮：那只会立刻被取消
		if ctx.Err() != nil {
			return
		}
		run(ctx)
	}); err != nil {
		return nil, fmt.Errorf("cron 表达式 %q 不可用: %w", expr, err)
	}
	return &scheduler{engine: engine}, nil
}

// Start 启动定时器并立即返回。
func (s *scheduler) Start() {
	s.engine.Start()
}

// Stop 停止定时器；返回的 context 在正在跑的那一轮结束后关闭。
func (s *scheduler) Stop() context.Context {
	return s.engine.Stop()
}
