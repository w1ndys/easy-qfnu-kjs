// Package runner 是一轮采集的启动端口：先收敛过期轮次，再判「是否已有进行中轮次」，
// 白名单为空时直接建立一条 blocked 终态轮次，其余情况把「建立真正采集轮次」交给注入的
// Collector。
//
// 依赖方向见 specs/2026-11-08-admin-settings-sync/design.md：本包只 import 标准库、
// model 与 store 的类型，不 import api、pipeline、upstream 或 settings 的实现。三个依赖
// （RunStore / WhitelistSource / Collector）都是消费侧接口，签名与 store.RunStatusRepo、
// settings.Service、以及将来的 collect-runner 结构匹配；Converge 是函数，装配层用
// pipeline.ConvergeStaleRuns 包装后注入，避免「2 小时」与「started_at <」出现第二份实现
// （design D6、D8）。
//
// 本包不执行采集：登录、学期、窗口、发布都在后续的 collect-runner spec 里。Collector
// 为 nil 时只放行白名单为空的 blocked 路径；白名单非空的手动同步返回
// model.ErrCollectorUnavailable，且一行 collect_run 都不写（design D6、CP16）。
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 启动结果的状态取值，与 collect_run.status 的 CHECK 约束一致。
//
// 本包只会产出这两个：白名单为空时 blocked（建立即终态），其余情况由 Collector 返回
// running（本包不替它编状态）。success/partial/failed/skipped 是执行阶段的结果，
// 不属于「启动」这条路。
const (
	StatusRunning = "running" // 已交给采集启动器，轮次正在后台执行
	StatusBlocked = "blocked" // 白名单为空，本轮建立即结束、不发任何上游请求
)

// whitelistEmptyReason 是 blocked 轮次的固定原因：它同时写给 StartBlockedRun 的
// error_message 与 StartResult.Reason，让库里的记录、202 响应的 reason 与日志的
// reason 是同一句话（Requirement 8.4、design 的 sync 响应示例）。
//
// 写成常量而不是在调用点各写一份：面板与测试都按这句文案判读，散成字面量后改名
// 不会有编译错误，只会在响应或库里悄悄错开。
const whitelistEmptyReason = "教学楼白名单为空，未发起采集"

// RunStore 是判冲突与建立 blocked 轮次所需的最小仓储能力，由 store.RunStatusRepo 实现。
//
// 只声明这两个方法：Start 不读列表、不读详情，多要一个方法只会让替身更难写、
// 也让「启动」这条路径悄悄依赖上管理端读取。Running 返回完整摘要而不是只回编号，
// 是因为 409 文案要同时点名 run_id 与 phase，冲突时不必再查一次库。
type RunStore interface {
	// Running 返回当前进行中轮次；没有时 found=false。唯一索引保证至多一条。
	Running(ctx context.Context) (store.RunSummary, bool, error)
	// StartBlockedRun 直接插入一条 status=blocked、phase=finished 的终态轮次，
	// 返回编号与开始时刻。它不读学期、不占 running 唯一索引（design D5）。
	StartBlockedRun(ctx context.Context, reason string, now time.Time) (int64, time.Time, error)
}

// WhitelistSource 只提供「当前白名单」，用来判空。
//
// 不直接依赖 settings.Service：本包只需要一个切片，不需要设置读写、秘密或 cron。
// 空名单与 nil 切片都按「空」处理——两者都表示没有可采集的楼。
type WhitelistSource interface {
	Whitelist(ctx context.Context) ([]model.Building, error)
}

// Collector 是真正建立采集轮次的端口，由后续 collect-runner 实现；本包不 import 它。
//
// 契约（design「Components and Interfaces」）：
//   - Start 建立一轮采集并把执行放后台，返回时轮次已落库，HTTP 可以立刻响应；
//   - 已有一轮进行中必须返回包装 model.ErrRunConflict 的错误，不得复用该轮（D9：
//     手动同步不能静默接上别人的轮次）；
//   - 其它失败返回包装了具体哨兵的错误，由上层映射成 50000。
//
// 本包收到包装 model.ErrRunConflict 的错误时会重读一次 Running，见 mapCollectError。
type Collector interface {
	Start(ctx context.Context, source model.TriggerSource) (StartResult, error)
}

// StartResult 是一次启动的结果：手动同步的 202 与 StartCron 的成功返回值都用它。
//
// Reason 只在 blocked 时有值；running 时为空。Status 取自上面两个常量之一，本包不校验
// Collector 返回的取值——契约由注入方保证，校验只会把装配错误推迟到运行期才暴露。
type StartResult struct {
	RunID     int64     // 轮次编号
	Status    string    // running 或 blocked
	StartedAt time.Time // 开始时刻，取自库里的 started_at
	Reason    string    // blocked 原因；其它状态为空
}

// BusyError 表示已有一轮采集进行中，本次启动被拒。
//
// 它带着冲突轮次的完整摘要，API 层用 errors.As 取出后直接拼 409 / 40901 的文案
// （点名 run_id 与 phase），不必再查一次库；只判类别时 errors.Is(err, model.ErrRunBusy)
// 即可。Unwrap 恒返回 model.ErrRunBusy，不随字段变化（design「哨兵的包装关系」）。
type BusyError struct {
	Run store.RunSummary // 冲突轮次；RunID 与 Phase 用于 409 文案
}

// Error 返回「已有一轮采集进行中（run_id=…, phase=…），请等待其结束」。
// 文案与 design 的 409 示例逐字一致，API 层可以直接透出，不必再拼一次。
func (e *BusyError) Error() string {
	return fmt.Sprintf("已有一轮采集进行中（run_id=%d, phase=%s），请等待其结束", e.Run.RunID, e.Run.Phase)
}

// Unwrap 恒返回 model.ErrRunBusy，供 errors.Is 判定类别 → 409 / 40901。
func (e *BusyError) Unwrap() error {
	return model.ErrRunBusy
}

// Converge 把超过 2 小时的 running 轮次收敛成 failed，返回被收敛的轮次编号。
//
// 装配层用 pipeline.ConvergeStaleRuns 实现并注入（design D8）：本包不 import pipeline，
// 也不自己算 cutoff，否则「2 小时」与「started_at <」会出现第二份实现，两处边界一旦
// 分叉，管理员就会看到「刚过 2 小时却永远 409」或「没到 2 小时就被收敛」。
type Converge func(ctx context.Context, now time.Time) ([]int64, error)

// Deps 是启动服务的全部注入点。除 Collect 外都必填，缺一即在新组装时拒绝。
//
// Collect 允许为 nil：采集执行链属于 collect-runner spec，本 spec 只交付「启动端口」
// 与空白名单的阻止路径（design D6）。
type Deps struct {
	Runs     RunStore         // 判冲突与 blocked 建轮，必填
	Settings WhitelistSource  // 判白名单是否为空，必填
	Collect  Collector        // 建立采集轮次；nil 时只放行 blocked 路径
	Converge Converge         // 启动前收敛过期轮次，必填
	Log      *slog.Logger     // 按来源记录 run_id/status/trigger/reason，必填
	Clock    func() time.Time // 当前时刻；nil 时退回 time.Now
}

// Service 是启动端口的实现。除内部的互斥锁外没有业务状态，可并发使用。
//
// mu 串行化「读 running → 建轮」这一小段（start / blocked 都算建轮）：单进程内两个
// 手动同步同时到达时，不会都读到「无 running」再一起去建轮，输家也不会把
// Collector.Start 的冲突错误当成自己的失败。多副本之间靠 collect_run_single_running
// 唯一索引兜底（design CP11）。采集执行在 Collector 的后台协程里，锁不覆盖它；启动
// 日志与冲突后的重读也在解锁之后做（见 Start 与 startLocked）。
type Service struct {
	runs     RunStore
	settings WhitelistSource
	collect  Collector
	converge Converge
	log      *slog.Logger
	clock    func() time.Time

	mu sync.Mutex
}

// New 组装启动服务。Runs/Settings/Converge/Log 缺一即返回包装 model.ErrConfigInvalid
// 的错误，由入口拒绝启动：这些依赖缺失时任何一次 Start 都只能 panic 或给出错误答案，
// 与其运行期才炸，不如装配期就失败（与 upstream.New 的启动期校验同一口径）。
//
// Collect 为 nil 不报错：这是本 spec 的过渡态，白名单非空的手动同步会得到
// ErrCollectorUnavailable，装配层另记一条 WARN（Requirement 10.5）。
func New(deps Deps) (*Service, error) {
	if deps.Runs == nil {
		return nil, fmt.Errorf("%w: runner 缺少 Runs 依赖", model.ErrConfigInvalid)
	}
	if deps.Settings == nil {
		return nil, fmt.Errorf("%w: runner 缺少 Settings 依赖", model.ErrConfigInvalid)
	}
	if deps.Converge == nil {
		return nil, fmt.Errorf("%w: runner 缺少 Converge 依赖", model.ErrConfigInvalid)
	}
	if deps.Log == nil {
		return nil, fmt.Errorf("%w: runner 缺少 Log 依赖", model.ErrConfigInvalid)
	}

	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		runs:     deps.Runs,
		settings: deps.Settings,
		collect:  deps.Collect,
		converge: deps.Converge,
		log:      deps.Log,
		clock:    clock,
	}, nil
}

// Start 启动一轮采集，source 只用于日志字段 trigger=manual|cron（D7：不落库）。
//
// 顺序是固定的，每一步的位置都有理由（Requirement 8、design D8/D9）：
//  1. Converge(now)：崩溃留下的 running 会永久占住唯一索引，不先收敛就会让之后每次
//     启动都只能 409。它放在锁外——是幂等 UPDATE，串行化它只会让并发的 409 一起排队；
//  2. Running()：命中就返回 *BusyError（409），**不调用 Collector**（CP14）；
//  3. Whitelist()：读失败原样上抛；空名单建立 blocked 轮次并返回 202 的 blocked 形状
//     （Requirement 8.4），这条路径在 Collector 未装配时也可用；
//  4. 白名单非空且 Collector 为 nil：返回 model.ErrCollectorUnavailable，零建轮（CP16）；
//  5. 其余情况交给 Collector.Start，返回它的结果；冲突映射见 mapCollectError。
//
// now 在一次 Start 里只取一次：Converge 的 cutoff 与 blocked 轮的三个时刻必须来自同一
// 个瞬间，分两次取会让「收敛」与「建轮」跨过秒边界，日志与库里的时刻对不上。
//
// 第 2～5 步在 startLocked 的锁里完成；本函数只在解锁之后才做冲突重读、才记日志。
func (s *Service) Start(ctx context.Context, source model.TriggerSource) (StartResult, error) {
	now := s.clock()

	// 先收敛再判冲突：这就是 Requirement 8.3「超过 2 小时先置 failed 再按无轮次处理」。
	// Converge 的返回值（被收敛的编号）在这里不消费，也不进日志字段白名单。
	if _, err := s.converge(ctx, now); err != nil {
		return StartResult{}, err
	}

	result, err := s.startLocked(ctx, source, now)
	if err != nil {
		// 走到这里锁已释放：mapCollectError 可能再读一次 running，那次读库不该占着
		// 启动的锁。非冲突错误它原样返回，所以不必先判类别。
		return StartResult{}, s.mapCollectError(ctx, err)
	}

	// 日志也在锁外：handler 可能同步写文件或网络，握着锁写会让并发的启动请求排在
	// 日志后面。
	s.logStarted(source, result)
	return result, nil
}

// startLocked 持锁完成「读 running → 建轮」这一小段：读进行中轮次、读白名单、建 blocked
// 轮次，或把建轮交给 Collector.Start。
//
// 锁从「读 running」开始，到「建轮返回」结束：Collector.Start 返回时轮次已落库，之后
// 到达的请求一定能在 Running 里看到它。不能在 Collector.Start 之前解锁，否则两个请求会
// 同时读到「无 running」，输家只能等 Collector 报冲突。多副本之间靠
// collect_run_single_running 唯一索引兜底（design CP11）；采集执行在 Collector 的后台
// 协程里，锁不覆盖它。
//
// defer 解锁：读库、白名单、建轮、Collector 每一条失败路径都先释放锁再返回，调用方拿到
// 的错误不会带着一把还握着的锁。
func (s *Service) startLocked(ctx context.Context, source model.TriggerSource, now time.Time) (StartResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	running, found, err := s.runs.Running(ctx)
	if err != nil {
		return StartResult{}, err
	}
	if found {
		return StartResult{}, &BusyError{Run: running}
	}

	whitelist, err := s.settings.Whitelist(ctx)
	if err != nil {
		return StartResult{}, err
	}

	if len(whitelist) == 0 {
		runID, startedAt, err := s.runs.StartBlockedRun(ctx, whitelistEmptyReason, now)
		if err != nil {
			return StartResult{}, err
		}
		return StartResult{
			RunID:     runID,
			Status:    StatusBlocked,
			StartedAt: startedAt,
			Reason:    whitelistEmptyReason,
		}, nil
	}

	if s.collect == nil {
		return StartResult{}, model.ErrCollectorUnavailable
	}

	return s.collect.Start(ctx, source)
}

// StartCron 是调度器到点触发的入口：与手动共用同一段启动逻辑，只以来源 cron 区分日志
// （Requirement 9.8）。
//
// 撞上进行中轮次时记一条 INFO 并返回 nil：定时触发不排队、不重试，下一个计划时刻照常
// 评估（Requirement 9.2、CP10）。其余错误原样返回，由调度器记 WARN——本包替它吞掉的话，
// 「到点失败」在日志里就只剩一条 INFO，运维会以为触发器没工作。
func (s *Service) StartCron(ctx context.Context) error {
	_, err := s.Start(ctx, model.TriggerCron)
	if err == nil {
		return nil
	}

	var busy *BusyError
	if errors.As(err, &busy) {
		// 字段只用 run_id/status/trigger：busy 的 phase 已经能通过 run_id 查详情，
		// 日志字段保持与启动成功那几条一致（D7、CP1）。
		s.log.Info("cron_skipped_busy",
			slog.String("trigger", string(model.TriggerCron)),
			slog.Int64("run_id", busy.Run.RunID),
			slog.String("status", busy.Run.Status),
		)
		return nil
	}
	return err
}

// mapCollectError 把 Collector.Start 的错误翻译成本包的两种结果。
//
// 只有包装 model.ErrRunConflict 的错误需要重读：Collector 报冲突说明唯一索引被占，
// 但那一轮可能在「冲突」与「这次重读」之间刚好结束。读得到：冲突真实存在，映射成
// *BusyError（409），不猜轮次编号；读不到：冲突窗口已过，返回包装了原错误的错误，
// 让上层按 50000 让管理员重试（design「Components and Interfaces」的 Collector 契约）。
// 重读本身失败时不改写类别：读库失败就是基础设施失败，它的错误比原冲突更有用。
func (s *Service) mapCollectError(ctx context.Context, err error) error {
	if !errors.Is(err, model.ErrRunConflict) {
		return err
	}

	running, found, readErr := s.runs.Running(ctx)
	if readErr != nil {
		return readErr
	}
	if found {
		return &BusyError{Run: running}
	}
	return fmt.Errorf("采集启动器报告轮次冲突，但已无进行中轮次，请重试: %w", err)
}

// logStarted 按来源记录一条启动结果日志。
//
// 字段固定为 run_id/status/trigger/reason 四项（reason 只在非空时出现）：启动日志
// 不写设置值、不写秘密，也不写白名单内容（Requirement 10.3、CP1）。trigger 取
// manual|cron，让「谁发起」在日志里可检索——它不落库（D7）。
func (s *Service) logStarted(source model.TriggerSource, result StartResult) {
	attrs := []any{
		slog.String("trigger", string(source)),
		slog.Int64("run_id", result.RunID),
		slog.String("status", result.Status),
	}
	if result.Reason != "" {
		attrs = append(attrs, slog.String("reason", result.Reason))
	}

	if result.Status == StatusBlocked {
		s.log.Info("collect_run_blocked", attrs...)
		return
	}
	s.log.Info("collect_run_started", attrs...)
}
