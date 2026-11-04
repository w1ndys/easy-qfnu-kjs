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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/config"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
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

	// 启动时先收敛进程崩溃留下的过期轮次（Requirement 8.3）：它们占着
	// collect_run_single_running 唯一索引，不收拾掉，之后每轮都只能复用这条死轮次。
	// 失败只记日志、不阻止启动：收敛是维护动作，查询服务仍应能对外提供已有数据。
	// 连接池由 store.NewCollectRunRepo 适配成 pipeline.RunStore，pipeline 不碰 pgx。
	// 限时 5 秒：行锁或网络卡住时不能让启动无限等，超时同样按 Warn 继续。
	convergeCtx, cancelConverge := context.WithTimeout(ctx, 5*time.Second)
	defer cancelConverge()
	if staleRunIDs, err := pipeline.ConvergeStaleRuns(convergeCtx, store.NewCollectRunRepo(pool), nil); err != nil {
		log.Warn("收敛过期轮次失败，继续启动", "err", err)
	} else if len(staleRunIDs) > 0 {
		log.Info("已收敛过期轮次", "run_ids", staleRunIDs)
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

	// meta 端点：版本/字典/轴/白名单直接来自存储层，逐周新鲜度复用 9.6 的查询服务。
	metaLookup := func(ctx context.Context) (query.MetaFacts, error) {
		facts, err := store.Meta(ctx, pool)
		if err != nil {
			return query.MetaFacts{}, err
		}

		mapped := query.MetaFacts{
			DictStates: make([]query.MetaDictStateFacts, 0, len(facts.DictStates)),
			AxisNodes:  make([]query.MetaAxisNode, 0, len(facts.AxisNodes)),
			Buildings:  make([]query.MetaBuilding, 0, len(facts.Buildings)),
		}
		// 无当前版本时保持 nil，由查询层输出空态。
		if facts.Release != nil {
			mapped.Release = &query.MetaReleaseFacts{
				ReleaseID:   facts.Release.ID,
				GeneratedAt: facts.Release.GeneratedAt,
				Term:        facts.Release.Term,
				TotalWeeks:  facts.Release.TotalWeeks,
				DictVersion: facts.Release.DictVersion,
				AxisVersion: facts.Release.AxisVersion,
			}
		}
		for _, state := range facts.DictStates {
			mapped.DictStates = append(mapped.DictStates, query.MetaDictStateFacts{
				StateKey:  state.StateKey,
				Label:     state.Label,
				Available: state.Available,
			})
		}
		for _, node := range facts.AxisNodes {
			mapped.AxisNodes = append(mapped.AxisNodes, query.MetaAxisNode{
				Node:         node.Node,
				Ordinal:      node.Ordinal,
				Block:        node.Block,
				BlockOrdinal: node.BlockOrdinal,
			})
		}
		for _, building := range facts.Buildings {
			mapped.Buildings = append(mapped.Buildings, query.MetaBuilding{
				BuildingID: building.ID,
				Name:       building.Name,
			})
		}
		return mapped, nil
	}
	freshnessLookup := func(ctx context.Context, params query.FreshnessParams) (query.FreshnessFacts, error) {
		facts, err := store.Freshness(ctx, pool, store.FreshnessRange{
			Term:     params.Term,
			FromWeek: params.FromWeek,
			ToWeek:   params.ToWeek,
		})
		if err != nil {
			return query.FreshnessFacts{}, err
		}

		mapped := query.FreshnessFacts{
			Weeks:         make([]query.WeekFacts, 0, len(facts.Weeks)),
			LastRunStatus: facts.LastRunStatus,
		}
		for _, week := range facts.Weeks {
			mapped.Weeks = append(mapped.Weeks, query.WeekFacts{
				Week:          week.Week,
				Published:     week.Published,
				LastSuccessAt: week.LastSuccessAt,
			})
		}
		return mapped, nil
	}
	// 逐周新鲜度只有一份实现：meta 与 screen 共用同一个服务实例（9.6 的 Weeks）。
	freshnessService := query.NewFreshness(freshnessLookup)
	metaService := query.NewMeta(metaLookup, freshnessService)

	// 大屏端点：规模/版本/轮次/占用来自存储层，逐周新鲜度与访问统计分别复用 9.6 与 9.9 的实现。
	// 30 秒缓存由 query.ScreenService 自己持有，装配层不再传 TTL（Requirement 33.7、CP15）。
	statsReader := stats.NewReader(func(ctx context.Context, windows store.StatWindows, topN int) (store.StatFacts, error) {
		return store.ReadStatFacts(ctx, pool, windows, topN)
	})
	screenLookup := func(ctx context.Context) (query.ScreenFacts, error) {
		facts, err := store.Screen(ctx, pool)
		if err != nil {
			return query.ScreenFacts{}, err
		}

		mapped := query.ScreenFacts{
			Occupancy: make([]query.ScreenOccupancyCell, 0, len(facts.Occupancy)),
		}
		// 无当前版本时保持 nil，由查询层输出 release:null + empty:true。
		if facts.Release != nil {
			mapped.Release = &query.ScreenReleaseFacts{
				ReleaseID:   facts.Release.ID,
				GeneratedAt: facts.Release.GeneratedAt,
				Term:        facts.Release.Term,
				TotalWeeks:  facts.Release.TotalWeeks,
				Rooms:       facts.Release.Rooms,
				Buildings:   facts.Release.Buildings,
			}
		}
		mapped.Published = screenRunFacts(facts.Published)
		mapped.LastRun = screenRunFacts(facts.LastRun)
		for _, cell := range facts.Occupancy {
			mapped.Occupancy = append(mapped.Occupancy, query.ScreenOccupancyCell{
				BuildingID:   cell.BuildingID,
				BuildingName: cell.BuildingName,
				Weekday:      cell.Weekday,
				BlockOrdinal: cell.BlockOrdinal,
				Cells:        cell.Cells,
				Occupied:     cell.Occupied,
			})
		}
		return mapped, nil
	}
	screenService := query.NewScreen(screenLookup, freshnessService, statsReader, weekLookup)

	// 访问统计：记账在查询响应写出之后入队，后台批量落库，失败只记日志（Requirement 34.1、CP18）。
	// 清理任务与查询、采集共用连接池但互不阻塞，按日删除 90 天前的记录（Requirement 34.10）。
	// 部署级盐由 2.2 的秘密管理提供，本任务尚未落库，这里显式传 nil（只按天加盐）。
	//
	// 统计后台用自己的生命周期：进程信号先停 HTTP，等在途请求把记账投递完，再停记账与清理。
	// 直接复用信号的 ctx 会让记账在 HTTP 收尾期间就退出，收尾期间投递的记账会滞留在队列里。
	statsCtx, stopStats := context.WithCancel(context.Background())
	statsRecorder := stats.NewRecorder(log, statsBatchWriter{pool: pool}, stats.WithVisitorSalt(nil))
	statsCleaner := stats.NewCleaner(log, statsBatchDeleter{pool: pool})
	go statsRecorder.Run(statsCtx)
	go statsCleaner.Run(statsCtx)

	// HTTP 服务在独立 goroutine 里跑，主流程只负责等待终止信号。
	// 限流的来源判定与查询缓存都在装配层决定：来源是否可信取决于部署拓扑（design D2），
	// 缓存窗口见 Requirement 32.2。
	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: api.New(log, pool, queries, queries, queries,
			api.WithTrustedProxy(cfg.TrustProxyHeaders),
			api.WithMeta(metaService),
			api.WithScreen(screenService),
			api.WithStats(statsRecorder),
		).Handler(),
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
	// 收到终止信号不等同步任务，只给在途只读请求 5 秒收尾，超时直接断开；
	// 之后还要等统计后台把最后一批写完才返回。
	log.Info("收到终止信号，开始收尾")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 关闭未在宽限内完成", "err", err)
	}
	// HTTP 收尾之后才停记账：收尾期间投递的记账仍会被接收。
	// 记账退出前会用不受取消影响的上下文写完最后一批，所以必须等它退出再返回，
	// 连接池在 run 返回后才关，先等一等才不会丢掉最后一批计数。
	stopStats()
	statsRecorder.Wait()
	statsCleaner.Wait()
	return 0
}

// screenRunFacts 把存储层的一轮同步事实映射成查询层形状；nil 原样透传，表示没有轮次。
// 只映射状态、时间、窗口与计数：错误码与错误原文不在任何一侧的结构体里（Requirement 33.6、CP13）。
func screenRunFacts(run *store.ScreenRun) *query.ScreenRunFacts {
	if run == nil {
		return nil
	}
	return &query.ScreenRunFacts{
		Status:            run.Status,
		StartedAt:         run.StartedAt,
		FinishedAt:        run.FinishedAt,
		WindowStart:       run.WindowStart,
		WindowEnd:         run.WindowEnd,
		UnknownCells:      run.UnknownCells,
		CompositeCells:    run.CompositeCells,
		AttrConflictCount: run.AttrConflictCount,
	}
}

// statsBatchWriter 把连接池适配成统计写入入口，避免 internal/stats 直接持有驱动类型。
type statsBatchWriter struct {
	pool *pgxpool.Pool
}

// WriteStats 实现 stats.BatchWriter。
func (w statsBatchWriter) WriteStats(ctx context.Context, batch store.StatsBatch) error {
	return store.WriteStats(ctx, w.pool, batch)
}

// statsBatchDeleter 把连接池适配成统计清理入口。
type statsBatchDeleter struct {
	pool *pgxpool.Pool
}

// DeleteStatsBefore 实现 stats.Deleter。
func (d statsBatchDeleter) DeleteStatsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return store.DeleteStatsBefore(ctx, d.pool, cutoff)
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
