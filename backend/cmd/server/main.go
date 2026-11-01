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
		"trust_proxy_headers", cfg.TrustProxyHeaders,
		"version", version,
	)

	// 查询服务只拿两项能力：按日期查学期周与按条件查空教室；连接池被闭包挡在里面。
	weekLookup := func(ctx context.Context, date time.Time) (model.TermWeek, bool, error) {
		return store.TermWeekAt(ctx, pool, date)
	}
	availabilityLookup := func(ctx context.Context, filter query.AvailabilityFilter) (query.AvailabilityPage, error) {
		page, err := store.Availability(ctx, pool, store.AvailabilityFilter{
			Term:      filter.Term,
			Week:      filter.Week,
			Weekday:   filter.Weekday,
			StartNode: filter.StartNode,
			EndNode:   filter.EndNode,
			NodeCount: filter.NodeCount,
			Building:  filter.Building,
			Keyword:   filter.Keyword,
			Limit:     filter.Limit,
			Offset:    filter.Offset,
		})
		if err != nil {
			return query.AvailabilityPage{}, err
		}

		rooms := make([]query.AvailabilityRoom, 0, len(page.Rooms))
		for _, room := range page.Rooms {
			rooms = append(rooms, query.AvailabilityRoom{
				RoomID:       room.ID,
				Name:         room.Name,
				BuildingID:   room.BuildingID,
				BuildingName: room.BuildingName,
			})
		}
		return query.AvailabilityPage{
			Total:              page.Total,
			Rooms:              rooms,
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
		}, nil
	}
	dayLookup := func(ctx context.Context, filter query.DayFilter) (query.DayPage, error) {
		page, err := store.DayStatus(ctx, pool, store.DayFilter{
			Term:     filter.Term,
			Week:     filter.Week,
			Weekday:  filter.Weekday,
			RoomID:   filter.RoomID,
			Building: filter.Building,
		})
		if err != nil {
			return query.DayPage{}, err
		}

		nodes := make([]query.DayNode, 0, len(page.Nodes))
		for _, node := range page.Nodes {
			nodes = append(nodes, query.DayNode{
				Node:      node.Node,
				Block:     node.Block,
				StateKey:  node.StateKey,
				Label:     node.Label,
				Available: node.Available,
				HasData:   node.HasData,
			})
		}

		result := query.DayPage{
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
			Nodes:              nodes,
		}
		// 房间不可归属时保持 nil，让查询层拿到明确的空态信号。
		if page.Room != nil {
			result.Room = &query.DayRoom{
				RoomID:       page.Room.ID,
				Name:         page.Room.Name,
				BuildingID:   page.Room.BuildingID,
				BuildingName: page.Room.BuildingName,
			}
		}
		return result, nil
	}
	queries := query.New(weekLookup, availabilityLookup, dayLookup, query.WithCache(query.DefaultCacheTTL))

	// HTTP 服务在独立 goroutine 里跑，主流程只负责等待终止信号。
	// 限流的来源判定与查询缓存都在装配层决定：来源是否可信取决于部署拓扑（design D2），
	// 缓存窗口见 Requirement 32.2。
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.New(log, pool, queries, queries, queries, api.WithTrustedProxy(cfg.TrustProxyHeaders)).Handler(),
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
