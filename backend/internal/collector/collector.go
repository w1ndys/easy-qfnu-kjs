package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// errPanic 是后台 panic 收口时写进 error_message 的固定原因（design「Error Handling」
// 的后台 panic 行：无错误码 +「内部错误」）。
//
// panic 的具体值只进日志、不落库：error_message 是给管理端看的一句话，把 panic 值写进去
// 可能带出页面片段、账号或密文（CP12）。日志里的 panic 值是排障信息，同样要收窄：
// 只记类型名与截断后的值（maxPanicLogRunes），绝不整段照抄可能来自页面解析的错误文本。
var errPanic = errors.New("内部错误")

// maxPanicLogRunes 是 panic 值写进日志的字符上限（按 rune 计，避免截断半个汉字）。
//
// 200 这个上限是「够认出错误类型与被解析的那一行」与「绝不把一整段页面文本带进日志」
// 之间的折中：panic 值常常是 parser 报错时拼进去的上下文，长度本身就可能是一页 HTML。
const maxPanicLogRunes = 200

// RunLifecycle 是建轮、补学期与写窗口所需的最小仓储能力，由 store.CollectRunRepo 实现。
//
// 三个方法都只动 collect_run 的当前轮次：建轮撞 collect_run_single_running 唯一索引时
// 返回包装 model.ErrRunConflict 的错误；补学期与写窗口带 status='running' 守卫，
// 已收口的轮次读不到更新（0 行即报错）。执行器不写 SQL，只调这些方法（D7）。
//
// 导出是因为装配层要用 var _ collector.RunLifecycle = store.CollectRunRepo{} 把方法集
// 钉在编译期（tasklist 第 11 项）；它与 TermCalendar、SnapshotReader 是本包仅有的三个
// 导出接口，其余窄接口（任务、上游、设置）只在本包内使用，保持不导出。
type RunLifecycle interface {
	// StartRunningRun 插入一条 status=running、term 为 NULL 的轮次；
	// 撞唯一索引返回包装 model.ErrRunConflict 的错误，不复用旧轮次。
	StartRunningRun(ctx context.Context, startedAt time.Time) (store.CollectRun, error)
	// SetRunTerm 在登录拿到学期后补写；守卫 status='running'。
	SetRunTerm(ctx context.Context, runID int64, term string, now time.Time) error
	// SetRunWindow 写 window_start / window_end / weeks_total；守卫 status='running'。
	SetRunWindow(ctx context.Context, runID int64, start, end, weeksTotal int, now time.Time) error
}

// TermCalendar 是学期日历整体替换的入口，由 store.TermRepo 实现。
//
// 写 term 与 term_week 在同一个事务里，锚点与库里已有日历不一致时返回
// store.ErrTermAnchorConflict 且不改任何行（design 第 4 节）。
type TermCalendar interface {
	ReplaceTerm(ctx context.Context, term string, totalWeeks int, firstMonday, now time.Time) error
}

// SnapshotReader 读字典与轴的当前快照，由 store.SnapshotRepo 实现。
//
// 没有版本行即报错：空字典会把所有格判成未知、空轴会让 parser.Rows 直接失败，
// 两者都该在发第一个矩阵请求之前就停下（design 第 5 节）。
type SnapshotReader interface {
	LoadCollectSnapshot(ctx context.Context) (store.CollectSnapshot, error)
}

// taskReader 是 hydrate 读周矩阵任务状态的最小入口，由 store.SyncTaskRepo 实现。
//
// 只读本轮全部周矩阵任务（含状态），hydrate 用它找出「已完成但内存缺页」的楼×周。
type taskReader interface {
	LoadWeekMatrixTasks(ctx context.Context, runID int64) ([]store.SyncTask, error)
}

// upstreamAPI 是执行器真正消费的上游能力，*upstream.Client 结构匹配。
//
// 只声明这六个方法：楼列表只用于 advisory 校验，明细只按格下一次钻；多声明一个方法
// 只会让测试替身更难写，也让执行器悄悄依赖上别处才用得上的上游能力。
type upstreamAPI interface {
	Login(ctx context.Context, opts upstream.LoginOptions) error
	FetchTerm(ctx context.Context) (*upstream.TermSnapshot, error)
	FetchWeekCalendar(ctx context.Context, at time.Time) (*upstream.WeekCalendar, error)
	FetchBuildings(ctx context.Context, campusID string) ([]upstream.Building, error)
	FetchWeekMatrix(ctx context.Context, params upstream.WeekMatrixParams) ([]byte, error)
	FetchOccupancyDetail(ctx context.Context, params upstream.OccupancyDetailParams) ([]byte, error)
}

// settingsSource 是设置读取的两个方法，*settings.Service 结构匹配。
//
// 账号与 OCR 地址的明文就在 Load 返回的 Snapshot 里；密码只有 Reveal 一条路，
// 执行器拿不到密钥与密文（collector 不 import secret）。
type settingsSource interface {
	Load(ctx context.Context) (settings.Snapshot, error)
	Reveal(ctx context.Context, key string) (string, error)
}

// execFunc 是一轮执行体的签名。
//
// 本项（tasklist 第 5 项）只交付启动语义与调用壳，执行体由第 6–10 项按阶段顺序填充；
// 同包测试用 Deps.exec 注入替身来钉住「启动先返回」「panic 收口」「取消请求 ctx 不收口」。
type execFunc func(ctx context.Context, runID int64) error

// Deps 是执行器的全部注入点。RunCtx 与 Log 必填，其余仓储/上游/设置也必填：
// 缺任何一个都只能让某一轮在跑到一半时 panic 或给出错误答案，与其运行期才炸，
// 不如装配期就拒绝（与 runner.New、upstream.New 的启动期校验同一口径）。
//
// 生产用 store 的仓储、*settings.Service 与 *upstream.Client；测试用真实
// pipeline/publish/parser/clean + 假上游与内存仓储替身。Clock 是唯一可缺省的注入项：
// nil 时退回 time.Now。
type Deps struct {
	Runs      RunLifecycle        // 建轮、补学期、写窗口（store.CollectRunRepo）
	Terms     TermCalendar        // 学期日历整体替换（store.TermRepo）
	Snapshots SnapshotReader      // 字典与轴快照（store.SnapshotRepo）
	Phases    pipeline.PhaseStore // 阶段、计数、收口（store.RunPhaseRepo）
	Tasks     pipeline.TaskStore  // 任务与周结果（store.SyncTaskRepo）
	TaskRead  taskReader          // 读周矩阵任务状态（store.SyncTaskRepo 的增量）
	Rooms     pipeline.RoomStore  // 归属快照、冲突计数（store.RoomRepo）
	Reader    publish.Reader      // 发布组装（store.PublishRepo）
	Committer publish.Committer   // 发布事务（store.ReleaseRepo）
	Settings  settingsSource      // 白名单、账号、OCR、密码（*settings.Service）
	Upstream  upstreamAPI         // 教务 HTTP 客户端（*upstream.Client）
	RunCtx    context.Context     // 进程生命周期上下文，必填（D9）
	Log       *slog.Logger        // 必填
	Clock     pipeline.Clock      // 当前时刻；nil 退回 time.Now

	// exec 是执行体注入点，只有同包测试会设置：nil 时 New 用 execute。
	// 不导出是因为执行体不是装配层的选择——生产只有一条执行路径，多一个导出字段
	// 只会让「这里能不能换掉」变成需要回答的问题。
	exec execFunc
}

// Collector 是 runner.Collector 的生产实现：建轮后把执行放后台，进度与终态都落在库里。
//
// 除 runCtx 指向的进程上下文外没有可变状态；Start 可以被并发调用，竞争由
// collect_run_single_running 唯一索引裁决（多副本同样如此，design CP11）。
type Collector struct {
	runs      RunLifecycle
	terms     TermCalendar
	snapshots SnapshotReader
	phases    pipeline.PhaseStore
	tasks     pipeline.TaskStore
	taskRead  taskReader
	rooms     pipeline.RoomStore
	reader    publish.Reader
	committer publish.Committer
	settings  settingsSource
	upstream  upstreamAPI
	runCtx    context.Context
	log       *slog.Logger
	clock     pipeline.Clock
	exec      execFunc
}

// 编译期钉住 runner 的启动端口：Start 的签名或返回值一旦漂移，装配层立刻编译失败，
// 而不是等到 main.go 里那次结构匹配才发现（design「Components and Interfaces」）。
var _ runner.Collector = (*Collector)(nil)

// New 组装执行器。除 Clock 外的依赖缺一即返回包装 model.ErrConfigInvalid 的错误，
// 由入口拒绝启动：这些依赖缺失时任何一次 Start 都只能 panic 或给出错误答案。
//
// 校验顺序与 Deps 的字段顺序一致，错误文案点名缺的是哪个依赖，装配层不必猜。
// Clock 为 nil 不报错：它只影响落库时刻，退回 time.Now 即可，没有「配错时钟就不能跑」
// 这种业务结论。
func New(deps Deps) (*Collector, error) {
	if deps.Runs == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Runs 依赖", model.ErrConfigInvalid)
	}
	if deps.Terms == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Terms 依赖", model.ErrConfigInvalid)
	}
	if deps.Snapshots == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Snapshots 依赖", model.ErrConfigInvalid)
	}
	if deps.Phases == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Phases 依赖", model.ErrConfigInvalid)
	}
	if deps.Tasks == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Tasks 依赖", model.ErrConfigInvalid)
	}
	if deps.TaskRead == nil {
		return nil, fmt.Errorf("%w: collector 缺少 TaskRead 依赖", model.ErrConfigInvalid)
	}
	if deps.Rooms == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Rooms 依赖", model.ErrConfigInvalid)
	}
	if deps.Reader == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Reader 依赖", model.ErrConfigInvalid)
	}
	if deps.Committer == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Committer 依赖", model.ErrConfigInvalid)
	}
	if deps.Settings == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Settings 依赖", model.ErrConfigInvalid)
	}
	if deps.Upstream == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Upstream 依赖", model.ErrConfigInvalid)
	}
	if deps.RunCtx == nil {
		return nil, fmt.Errorf("%w: collector 缺少 RunCtx 依赖", model.ErrConfigInvalid)
	}
	if deps.Log == nil {
		return nil, fmt.Errorf("%w: collector 缺少 Log 依赖", model.ErrConfigInvalid)
	}

	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	c := &Collector{
		runs:      deps.Runs,
		terms:     deps.Terms,
		snapshots: deps.Snapshots,
		phases:    deps.Phases,
		tasks:     deps.Tasks,
		taskRead:  deps.TaskRead,
		rooms:     deps.Rooms,
		reader:    deps.Reader,
		committer: deps.Committer,
		settings:  deps.Settings,
		upstream:  deps.Upstream,
		runCtx:    deps.RunCtx,
		log:       deps.Log,
		clock:     clock,
		exec:      deps.exec,
	}
	if c.exec == nil {
		c.exec = c.execute
	}
	return c, nil
}

// Start 实现 runner.Collector：先建一条 running 轮次，再把执行交给后台协程。
//
// 顺序固定（design D2、D9、Requirement 1）：
//  1. StartRunningRun 用请求 ctx 写一次库。这是请求 ctx 的唯一用途——它随 HTTP 响应
//     结束而取消，拿它跑 70 分钟的采集会让每一轮在第一次上游请求后就中断（CP11）；
//  2. 写库失败原样返回：撞唯一索引时错误已包装 model.ErrRunConflict，runner 据此
//     重读 Running 折成 409，本层不再包装也不复用旧轮次（D9）；
//  3. 建轮成功后立刻启动后台协程并返回 running，不等待任何上游往返。runner 是在持有
//     s.mu 的情况下调用本方法的，这里多停一秒，并发的手动同步与同刻的 cron 就一起排队；
//  4. 后台协程只使用进程生命周期上下文 Deps.RunCtx（run 里取），请求 ctx 到第 3 步为止。
//
// source 不参与建轮也不落库（D7）：它只用于 runner 的启动日志 trigger=manual|cron，
// 执行器这一层不消费它，所以形参写成 _——保留 model.TriggerSource 类型是为了对上
// runner.Collector 的方法集（参数名不参与方法签名）。返回值里的 StartedAt 取自库里的
// started_at，与管理员在轮次列表里看到的时刻是同一个值。
func (c *Collector) Start(ctx context.Context, _ model.TriggerSource) (runner.StartResult, error) {
	run, err := c.runs.StartRunningRun(ctx, c.clock())
	if err != nil {
		return runner.StartResult{}, err
	}

	go c.run(run.RunID)

	return runner.StartResult{
		RunID:     run.RunID,
		Status:    runner.StatusRunning,
		StartedAt: run.StartedAt,
	}, nil
}

// run 是后台协程的入口：用进程生命周期上下文执行一轮，并在 panic 时把轮次收口为 failed。
//
// 执行体返回错误的处理与 panic 不同：错误由执行体自己在失败点收口（settings_incomplete、
// auth_failed、发布事务失败……见 design「Error Handling」），本函数只记一条 WARN 便于排障，
// 不替它写终态——两层都收口会撞 store.FinishRun 的 status='running' 守卫。
// 取消与进程退出同理：这里不调用任何收口函数，轮次保持 running，未完成任务留给下一次
// 执行（CP8、Requirement 10.4）。
//
// defer 里 recover：panic 不能穿过 goroutine 边界，否则整个进程退出。收口顺序是
// 先 FailRun（无码 +「内部错误」）再记 ERROR：日志永远会打，但只有 FailRun 成功时，
// 「日志出现」才等价于「终态已写」；收口失败时日志带 err 字段，那条等价关系不成立，
// 排障要按 err 判断，而不是看到 ERROR 就假定库里已经是 failed。
func (c *Collector) run(runID int64) {
	defer func() {
		if cause := recover(); cause != nil {
			c.failOnPanic(runID, cause)
		}
	}()

	if err := c.exec(c.runCtx, runID); err != nil {
		c.log.Warn("collect_run_exec_failed",
			slog.Int64("run_id", runID),
			slog.Any("err", err),
		)
	}
}

// failOnPanic 把 panic 中断的一轮收口为 failed，并记一条 ERROR。
//
// 用 FailRun 而不是 CloseRun：panic 不是任务失败，库里可能一条 failed 任务都没有，
// CloseRun 的失败分层会把它读成「没有失败原因」。错误码传空串——「内部错误」不属于
// design 错误码表里的任何业务层，编一个码会让管理端误判成结构失败。
//
// 收口本身失败只能记日志（通常意味着 RunCtx 已取消或库连不上），不再尝试第二次收口：
// 终态只会被写一次，重试只会撞 FinishRun 的守卫。
//
// 日志里的 panic 字段不带原始值，而是 panicLogValue 折出来的「类型名: 截断值」：
// panic 的载荷可能是 parser 拼进来的整页 HTML，原样照抄等于把页面片段写进日志（CP12
// 的唯一例外是 panic/err 这类排障字段，例外也只到类型名与简短值为止）。
func (c *Collector) failOnPanic(runID int64, cause any) {
	finishErr := pipeline.FailRun(c.runCtx, c.phases, runID, "", errPanic, c.clock)

	attrs := []any{
		slog.Int64("run_id", runID),
		slog.String("panic", panicLogValue(cause)),
	}
	if finishErr != nil {
		attrs = append(attrs, slog.Any("err", finishErr))
	}
	c.log.Error("collect_run_panic", attrs...)
}

// failRun 把一轮按失败收口（status=failed、phase=finished、finished_at、错误码与原因），
// 并把原因返回给 run() 记一条 WARN。
//
// code 为空串表示「不属于任何业务失败层」：网络、超时、写库失败都不编码，由
// pipeline.FailRun 交给 store.FinishRun 的 NULLIF 折成 NULL（design「非结构失败不编码」）。
//
// ctx 已经结束时不收口：调用方取消或进程退出要求轮次保持 running，未完成任务留给
// 下一次执行（Requirement 10.4、CP8）。先判一次 ctx 还有第二个理由——同一个 ctx 下的
// 收口写库也必然失败，不判就会把「被取消」记成一条误导性的收口失败。
//
// 收口本身失败时把两个原因都带上：终态没写成是排障的一等事实，原始失败也不能丢。
// 这里不重试第二次收口——终态只会被写一次，重试只会再撞一次 FinishRun 的
// status='running' 守卫。
func (c *Collector) failRun(ctx context.Context, runID int64, code string, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := pipeline.FailRun(ctx, c.phases, runID, code, cause, c.clock); err != nil {
		return fmt.Errorf("轮次 %d 收口失败（原始失败：%w）: %w", runID, cause, err)
	}
	return cause
}

// failCalendar 是学期/周历失败唯一的收口入口：failed + error_code=calendar_invalid。
//
// 与 failRun 分开是因为 design 把这条码固定给 pipeline.FailCalendar：同一个码两条写入
// 路径（FailRun 的覆盖写入与 FailCalendar 的固定写入）会让文案与默认消息迟早分叉。
// 取消/退出时同样不收口，理由见 failRun。
func (c *Collector) failCalendar(ctx context.Context, runID int64, cause error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := pipeline.FailCalendar(ctx, c.phases, runID, cause, c.clock); err != nil {
		return fmt.Errorf("轮次 %d 日历失败收口失败（原始失败：%w）: %w", runID, cause, err)
	}
	return cause
}

// panicLogValue 把 recover 到的值折成一行可安全落日志的文本：类型名 + 值的字符串形式，
// 值超过 maxPanicLogRunes 个字符时按 rune 截断。
//
// 类型名放在最前是因为它决定了「这是什么错」——运行时错误（runtime.Error）与执行体自己
// panic 的字符串在排障时含义完全不同；值只是补充上下文，超过上限就不再往下写。
func panicLogValue(cause any) string {
	return fmt.Sprintf("%T: %s", cause, truncateRunes(fmt.Sprintf("%v", cause), maxPanicLogRunes))
}

// truncateRunes 按字符（rune）截断，避免按字节切出半个汉字；len 以内原样返回。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
