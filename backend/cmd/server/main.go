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
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/auth"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collector"
	"github.com/w1ndys/easy-qfnu-kjs/internal/config"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/scheduler"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// 管理端数据来源必须满足 auth.AdminStore：消费侧接口定义在 internal/auth，
// 实现留在 internal/store（store 不反向 import auth）。断言放在装配层，
// 两边方法集一旦漂移就在编译期失败，而不是等到某个端点运行期才发现取不到数据。
var _ auth.AdminStore = store.AdminRepo{}

// 业务设置仓储同理：settings.Store 是消费侧接口，store.SettingsRepo 用同名方法
// 结构匹配，store 不 import settings（依赖方向见 backend/.golangci.yml）。这条断言
// 与上面的 auth.AdminStore 并列，把「settings 服务能不能直接用这个仓储」钉在编译期。
var _ settings.Store = store.SettingsRepo{}

// 版本管理仓储同理：api.ReleaseAdminReader 是消费侧接口（定义在 api/admin_releases.go），
// store.ReleaseAdminRepo 靠同名方法结构匹配，store 不 import api。两边方法集一旦漂移
// 就在编译期失败，而不是等到第一次打开版本列表或点回退才发现取不到数据。
var _ api.ReleaseAdminReader = store.ReleaseAdminRepo{}

// 采集执行器的三条仓储窄接口同理：collector.RunLifecycle / TermCalendar / SnapshotReader
// 是消费侧接口，实现留在 internal/store（store 不 import collector），方法集一旦漂移就
// 在编译期失败，而不是等到第一次手动同步才在后台轮次里报「缺依赖」。
var (
	_ collector.RunLifecycle   = store.CollectRunRepo{}
	_ collector.TermCalendar   = store.TermRepo{}
	_ collector.SnapshotReader = store.SnapshotRepo{}
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

	// 管理端装配的前两步（design「依赖方向」的装配顺序）：先装载密钥，再建仓储。
	// 密钥只有一把，放在文件里让数据库备份与密钥备份分开保管（D2）；Load 只读文件，
	// 不做任何创建，文件缺失是首次部署的正常状态，真正的生成发生在 POST /api/admin/init。
	// Load 失败按启动期降级处理：记 ERROR 但继续启动，公开查询、state 与登录都不依赖这把密钥（CP14）。
	keys := secret.NewManager(cfg.SecretKeyPath)
	keyLoaded, keyErr := keys.Load()
	if keyErr != nil {
		log.Error("加密密钥不可用，管理端密钥相关能力降级", "path", keys.Path(), "err", keyErr)
	}
	adminRepo := store.NewAdminRepo(pool)
	// 管理员已初始化、密钥文件却缺失，是「备份没恢复」的部署事故：进程照常启动，但必须留一条
	// 可检索的 WARN（design Error Handling 的 secret_key_missing），否则 init 的 40902 会让运维
	// 以为系统一切正常。只有文件确实不存在时才会走到这里；文件存在但不可用已在上面记过 ERROR。
	// 这次查询与下面的收敛过期轮次同一口径：限时 5 秒，库卡住时不能让启动无限等，超时同样按 WARN 继续。
	// 失败只记固定类别、不记底层错误原文：启动日志没有理由把 SQL 或连接串片段带出去
	// （Requirement 9.1 只约束管理端请求日志，启动日志的字段单列在 design Error Handling）。
	if keyErr == nil && !keyLoaded {
		adminCtx, cancelAdmin := context.WithTimeout(ctx, 5*time.Second)
		_, adminFound, adminErr := adminRepo.Admin(adminCtx)
		cancelAdmin()
		if adminErr != nil {
			// msg 是固定类别而不是 err 原文：字段只有路径，排障靠类别 + 日志级别即可定位。
			log.Warn("admin_query_failed", "path", keys.Path())
		} else if adminFound {
			log.Warn("secret_key_missing", "path", keys.Path())
		}
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

	// 业务设置的装配顺序（design「Components and Interfaces」）：仓储 → 服务。
	// 服务层持有仓储、密钥与时钟；这里 clock 传 nil，生产用 time.Now。keys 可能尚未
	// 装载（首次部署），服务层把「密钥不可用」当成保存秘密的失败，读取与清除秘密不受
	// 影响（Requirement 3.6、10.6）。密钥的生成仍只发生在 POST /api/admin/init。
	settingsSvc := settings.New(store.NewSettingsRepo(pool), keys, nil)

	// 运行状态的读侧与 blocked 建轮共用同一个仓储：列表、详情、冲突判定与阻止轮次
	// 都读同一张 collect_run，不另建第二份查询实现。
	runStatusRepo := store.NewRunStatusRepo(pool)

	// 版本管理仓储：列表、详情、对比与回退事务都走它（设计 D1 的 store 侧）；
	// 异常视图 `Anomalies` 属于 tasklist 第 6 项，第 6 项实现 `Anomalies` 后走该仓储。
	// 这里只构造，端点注册在 tasklist 第 7 项；回退提交成功后的缓存失效由下方
	// api.WithReleaseAdmin 的第二个参数接上，仓储本身不碰 query 包。
	releaseAdminRepo := store.NewReleaseAdminRepo(pool)

	// 采集链路的装配顺序固定（collect-runner design「依赖方向」的装配段）：
	// 上游客户端 → 采集执行器 → runner 的 Collect 端口。三步都必须在 runner.New 之前完成：
	// runner 允许 Collect 为 nil（过渡态），但本装配成功后不再存在 nil 状态；装配顺序反过来
	// 就只能先塞一个 nil 再回头补，等于把「执行链没接上」变成运行期才暴露的问题。
	//
	// 上游根地址来自配置（UPSTREAM_BASE_URL，缺省 http://zhjw.qfnu.edu.cn），
	// 形状校验只有 upstream.New 一处；OCR 超时用 config 里那个值，与启动日志打的是
	// 同一个配置项，避免「日志写 10s、客户端用默认值」这种两处口径。
	upstreamClient, err := upstream.New(cfg.UpstreamBaseURL, upstream.WithOCRTimeout(cfg.OCRTimeout))
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}

	// 任务仓储同时承担两个接口：Tasks 是执行循环的读写面，TaskRead 只给 hydrate 读
	// 周矩阵任务状态。同一个值注入两次，避免两个仓储实例对同一批行各持一份状态。
	syncTasks := store.NewSyncTaskRepo(pool)
	collectorPort, err := newRunCollector(collector.Deps{
		Runs:      store.NewCollectRunRepo(pool),
		Terms:     store.NewTermRepo(pool),
		Snapshots: store.NewSnapshotRepo(pool),
		Phases:    store.NewRunPhaseRepo(pool),
		Tasks:     syncTasks,
		TaskRead:  syncTasks,
		Rooms:     store.NewRoomRepo(pool),
		Reader:    store.NewPublishRepo(pool),
		Committer: store.NewReleaseRepo(pool),
		Settings:  settingsSvc,
		Upstream:  upstreamClient,
		// RunCtx 是进程生命周期上下文（信号 ctx），不是任何一次 HTTP 请求的 ctx：
		// 一轮采集要跑几十分钟，请求 ctx 在 202 写出后立刻取消（D9、CP11）。
		RunCtx: ctx,
		Log:    log,
	})
	// 依赖缺失只可能是装配被改坏；与 upstream.New 的启动期校验同口径直接拒绝启动，
	// 不把「第一次手动同步 50000」留给运维。
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}

	// 端口变量保持接口类型而不是 *collector.Collector：runner.Deps.Collect 是消费侧
	// 接口，装配层在这里决定「接上谁」；接口为 nil 仍是 runner 能识别的未装配状态。
	collectorWired := collectorPort != nil

	// Converge 注入 pipeline.ConvergeStaleRuns，而不是在 runner 里重算 cutoff：
	// 「2 小时」与 started_at < 的边界只允许有一份实现（D8）。now 由 runner 传入，
	// 这里只把它转成 pipeline.Clock，不取自己的时钟——否则两者会各算一个时刻。
	converge := func(ctx context.Context, now time.Time) ([]int64, error) {
		return pipeline.ConvergeStaleRuns(ctx, store.NewCollectRunRepo(pool), func() time.Time { return now })
	}
	syncSvc, err := runner.New(runner.Deps{
		Runs:     runStatusRepo,
		Settings: settingsSvc,
		Collect:  collectorPort,
		Converge: converge,
		Log:      log,
	})
	// 四个必填依赖都是本函数里刚建好的具体值，缺失只可能是代码被改坏；与 upstream.New
	// 的启动期校验同口径，直接拒绝启动，不把问题拖到第一次手动同步。
	if err != nil {
		log.Error("启动失败", "err", err)
		return 1
	}

	// 进程内调度：触发器接 runner.StartCron（busy 只记 INFO），表达式来源接
	// settings.Service（缺行回落 DefaultCronExpr）。clock/wait 传 nil，生产用真实
	// 时钟与 time.After；保存设置后的重排由 api.WithAdminSettings 第二参数接 sched.Reload。
	sched := scheduler.New(log, syncSvc.StartCron, settingsSvc, nil, nil)

	// 启动日志里的 cron 打的是**生效值**，不是库里的原样值：存量表达式非法（0005 之前的
	// 6 段写法、被手工改坏）时，调度器的启动装载会回落 DefaultCronExpr 并记 ERROR
	// （Requirement 9.6），进程实际按默认值调度；日志要是照抄库值，就会编一个读得到却
	// 不会生效的表达式。表达式本身不是秘密，但「只打表达式、不打任何设置值」这条口径
	// （design「Components and Interfaces」）不变——设置值里可能有凭据。
	//
	// 回落规则不在这里另写一份：scheduler.ResolveStartupCron 与调度器的启动装载调的是
	// 同一个 cronexpr.ResolveStartup，两处走同一套回落规则（缺失、非法都回落默认值）。
	// 这只是「规则同源」，不等于「值一定相同」：这里是独立的一次读库，调度器在 Run 里
	// 还会再读一次，两次之间管理员保存了新表达式、这次读库瞬时失败或多副本并发写，都会
	// 让 settings_cron 与进程真正装载的计划不同。这个字段是装配层启动时按同一套规则解出的
	// 快照；库里的原样值不进任何启动日志（非法/缺失时它与回落值相同），进程当前生效的
	// 计划以 scheduler_started 为准。
	// 读库限时 5 秒：与上面的启动查询同一口径，库卡住时不能让启动无限等。
	cronCtx, cancelCron := context.WithTimeout(ctx, 5*time.Second)
	startupCron, cronErr := scheduler.ResolveStartupCron(cronCtx, settingsSvc)
	cancelCron()
	if cronErr != nil {
		// 读失败只记固定类别、不打 err 原文：启动日志没有理由把 SQL 或连接串片段带出去
		// （与上面 admin_query_failed 同一口径）。细节由调度器启动装载那条 ERROR 负责。
		log.Warn("settings_cron_read_failed", "default", model.DefaultCronExpr)
	}
	settingsCron := startupCron.Expr

	// 启动日志只打非敏感项：连接串拆成主机与库名，密码与查询参数不出现；
	// 密钥只打路径（运维要确认挂载点），密钥内容与指纹都不进启动日志。
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
		"secret_key_path", keys.Path(),
		"admin_cookie_secure", cfg.AdminCookieSecure,
		"collector_wired", collectorWired,
		"settings_cron", settingsCron,
		"version", version,
	)
	// collector.New 失败即拒绝启动，所以本装配成功后 collectorPort 必非 nil：下方 WARN
	// 分支为将来装配回 nil 预留、当前不可达。保留它是为了「装配被改回 nil」时仍能从启动
	// 日志一眼看出来，而不是等管理员点了「立即同步」只拿到 50000。
	if !collectorWired {
		log.Warn("采集启动器未装配，白名单非空的手动同步与定时触发不会建立采集轮次")
	}

	// 调度协程随信号 ctx 退出：进程收到终止信号后不再发起新的轮次（Requirement 9.7）。
	// 启动放在日志之后，让「服务启动」那条带 collector_wired / settings_cron 的日志
	// 先落盘；调度器自己会在装载完成后另记一条 scheduler_started。
	//
	// 用 WaitGroup 接管协程的退出：收尾时 join 它，正在跑的触发不会和 server.Shutdown、
	// 连接池关闭交错（触发自己的 ctx 也派生自这个信号 ctx，取消后不会等满 30 秒）。
	var schedWG sync.WaitGroup
	schedWG.Add(1)
	go func() {
		defer schedWG.Done()
		sched.Run(ctx)
	}()

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
	// 管理端两项：数据来源用 store.AdminRepo（结构匹配 api 的消费侧接口，store 不 import api），
	// 密钥用上面已装载的 Manager；Cookie 的 Secure 随 ADMIN_COOKIE_SECURE，默认 true。
	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: api.New(log, pool, queries, queries, queries,
			api.WithTrustedProxy(cfg.TrustProxyHeaders),
			api.WithMeta(metaService),
			api.WithScreen(screenService),
			api.WithStats(statsRecorder),
			api.WithAdmin(adminRepo, keys),
			api.WithAdminCookieSecure(cfg.AdminCookieSecure),
			// 业务设置：svc 负责读写与加封，reloadCron 接调度器的重排入口（保存 cron 后
			// 立即生效，无需重启）。RunStatus 与 Sync 分别接轮次只读视图与启动端口；
			// 三件依赖缺一，对应端点按 50000 返回，不以 404 掩盖装配缺失（Requirement 10.7）。
			api.WithAdminSettings(settingsSvc, sched.Reload),
			api.WithRunStatus(runStatusRepo),
			api.WithSync(syncSvc),
			// 版本管理：数据来源是上面的 releaseAdminRepo；第二个参数是回退成功后的
			// 公开查询缓存失效（design D8）——回退切换 current 版本后，空教室、全天状态
			// 与大屏三张 30 秒缓存必须立即清空，否则访客最多要等 30 秒才看到新版本。
			// 回调只在回退提交成功后、写响应前被调用（CP10），由 handler 保证时机。
			api.WithReleaseAdmin(releaseAdminRepo, func() {
				queries.Invalidate()
				screenService.Invalidate()
			}),
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

	// 先等调度协程退出：它派生自同一个信号 ctx，取消后正在跑的触发会立刻返回。
	// 等它返回再关 HTTP 与连接池，收尾期间就不会再有调度侧在建轮或占用连接。
	schedWG.Wait()

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

// newRunCollector 组装采集执行器，并以消费侧接口返回。
//
// 返回 runner.Collector 而不是 *collector.Collector，是为了让装配层保留「执行链接上
// 了没有」这一状态：runner.Deps.Collect 用 nil 表示未装配（admin-settings-sync 的
// 过渡态），直接返回具体类型会把「成功即非 nil」写死成常量，启动日志里的
// collector_wired 与未装配 WARN 就失去意义。这与上面的 Converge 闭包同一做法：
// 装配层负责适配，runner 与 collector 互不 import。
func newRunCollector(deps collector.Deps) (runner.Collector, error) {
	runCollector, err := collector.New(deps)
	if err != nil {
		return nil, err
	}
	return runCollector, nil
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
