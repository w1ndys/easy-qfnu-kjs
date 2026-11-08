// Package scheduler 是进程内的定时触发循环：装载表达式 → 算下一次 → 等待 → 到点触发 →
// 重算。它不执行采集，也不 import runner：触发器是注入的函数（design D1、D12）。
//
// 依赖方向见 specs/2026-11-08-admin-settings-sync/design.md：本包只 import 标准库、
// model 与 cronexpr。表达式来源是消费侧接口 ExprSource，settings.Service 的 CronExpr
// 用同名方法结构匹配，装配层直接把服务传进来即可，不必让本包依赖 settings。
//
// 单实例语义：compose 部署单副本，进程内一个循环就够；多副本时各实例都会到点触发，
// 由 collect_run 的唯一索引只让一轮启动（D12）。保存设置后的「立即重排」只发生在处理
// 该请求的实例上，其余实例要等重启——这是本 spec 明说的限制，不在本包里假装解决。
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/cronexpr"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// Trigger 是到点要做的事。装配层传 runner.Service.StartCron：它自己处理「已有轮次
// 进行中」的跳过，本包只负责到点调用一次并按错误记日志（Requirement 9.2 / 9.3 / 9.8）。
//
// 参数 ctx 已经带上 30 秒超时，派生自 Run 的 ctx：进程收到终止信号时，正在跑的那次
// 触发会跟着取消，不会在关停过程中继续建轮（Requirement 9.7）。
type Trigger func(ctx context.Context) error

// ExprSource 提供当前生效的 cron 表达式。settings.Service 实现同名方法：缺行时返回
// model.DefaultCronExpr（Requirement 9.5），读库失败时返回 error。
//
// 本包不缓存来源，只在 Run 启动时读一次；之后的变更走 Reload。每次到点都重新读库会
// 让「调度」依赖一条额外的 SQL，且读到的值可能和 Reload 交错，计划就说不清是哪一份。
type ExprSource interface {
	CronExpr(ctx context.Context) (string, error)
}

// triggerTimeout 是一次触发的上限。触发器只做「建轮并放后台」，30 秒足够走完收敛、
// 判冲突、建轮这几条 SQL；超时只取消这一次触发，循环照常在下一个计划时刻继续，
// 不重试、不排队（Requirement 9.3）。
const triggerTimeout = 30 * time.Second

// Scheduler 是调度循环。除 mu 与 running 外没有别的状态，可并发调用 Reload 与 Next。
//
// mu 保护「当前表达式、计划、下一次时刻、最近触发时刻、Reload 代数」这一组字段：
// Reload（HTTP 写请求的协程）与 Run（调度协程）会同时读写它们。clock 与 wait 是注入
// 的纯函数，不参与加锁。
//
// running 挡住 Run 的并发重入：装配层写了两次 go sched.Run，或者测试里误开两个循环，
// 第二个会立刻返回并记一条 WARN，而不是让两个循环各自到点、把一轮采集触发两次。
// 它只在 Run 的生命周期内为 true，Run 返回后重置，所以进程重启式的「先 Run 完再 Run」
// 仍然可以。
type Scheduler struct {
	log     *slog.Logger
	trigger Trigger
	source  ExprSource
	clock   func() time.Time
	wait    func(time.Duration) <-chan time.Time

	mu          sync.Mutex
	schedule    cronexpr.Schedule // 当前生效的解析结果
	expr        string            // 当前生效的原写法，只进日志
	hasSchedule bool              // false 表示连默认值都装不上，循环只等 Reload
	next        time.Time         // 下一次触发时刻（业务时区）
	hasNext     bool              // false 表示表达式在 5 年内没有触发时刻
	firedAt     time.Time         // 最近一次触发的实际时刻；零值表示本进程还没触发过
	reloadGen   uint64            // 每成功一次 Reload 自增，启动装载用它识别「期间有人重排过」

	reloadCh chan struct{} // Reload 唤醒循环；容量 1，重复唤醒合并
	running  atomic.Bool   // Run 是否正在跑，挡住并发重入
}

// New 组装调度器。clock 为 nil 退回 time.Now，wait 为 nil 退回 time.After——这两项
// 只有测试需要注入（design 的 scheduler 接口约定）。log 为 nil 退回 slog.Default，
// trigger 为 nil 时到点只会记一条 WARN，不再 panic。
//
// 构造时先把 model.DefaultCronExpr 装上：这样 Run 还没装载、或者来源读不出来时，
// Next() 也有确定答案；默认值本身若被改坏，启动日志里立刻能看到，而不是等到第一个
// 到点时刻才发现调度从未启动过。
func New(
	log *slog.Logger,
	trigger Trigger,
	src ExprSource,
	clock func() time.Time,
	wait func(time.Duration) <-chan time.Time,
) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	if wait == nil {
		wait = time.After
	}
	if trigger == nil {
		log.Error("scheduler 未配置触发器，到点将只记日志")
		trigger = func(context.Context) error {
			return errors.New("scheduler 未配置触发器")
		}
	}

	s := &Scheduler{
		log:      log,
		trigger:  trigger,
		source:   src,
		clock:    clock,
		wait:     wait,
		reloadCh: make(chan struct{}, 1),
	}

	// DefaultCronExpr 与 cronexpr 在同一个仓里维护，解析失败说明默认值被改成了这个
	// 解析器不接受的形状（例如退回 6 段）；记 ERROR 后不装计划，Run 会只等 Reload。
	if schedule, err := cronexpr.Parse(model.DefaultCronExpr); err != nil {
		log.Error("默认 cron 表达式无法解析，调度暂停",
			slog.String("default", model.DefaultCronExpr),
			slog.Any("err", err),
		)
	} else {
		s.setSchedule(schedule, model.DefaultCronExpr)
	}

	return s
}

// Run 跑调度循环，直到 ctx 被取消。它是阻塞调用，装配层用 go sched.Run(ctx) 启动。
//
// 一轮循环固定是：取下一次时刻 → wait 到点或等 Reload/取消 → 到点触发（带 30 秒
// 超时）→ 按当前时刻重算。到点触发失败只记一条 WARN，下一圈照常按计划等待
// （Requirement 9.3）；表达式在 5 年内没有触发时刻（如 2 月 30 日）时不再空转，
// 只等 Reload 或取消，避免 wait(0) 立刻返回把循环变成忙等。
//
// 并发重入：第二个 Run 会立刻返回并记 WARN，不共享 reloadCh 的消费权，也不会让
// 同一个计划触发两次。
func (s *Scheduler) Run(ctx context.Context) {
	if !s.running.CompareAndSwap(false, true) {
		s.log.Warn("scheduler 已在运行，忽略重复启动")
		return
	}
	defer s.running.Store(false)

	s.loadInitial(ctx)
	s.logStarted()

	for {
		next, ok := s.Next()
		if !ok {
			// 没有下一次：等 Reload 给新表达式，或者取消。不能退回 wait(0)，
			// 那会变成「每个调度周期都空转一次」的忙等。
			s.log.Error("cron 表达式没有下一次触发时刻，等待重新装载",
				slog.String("cron", s.currentExpr()),
			)
			select {
			case <-ctx.Done():
				return
			case <-s.reloadCh:
				continue
			}
		}

		// 用注入时钟算剩余时长：测试里时钟不动，wait 就永远等同一个时长；真实运行
		// 时与 time.Until 等价。时钟被拨到计划时刻之后（机器休眠、容器暂停）时按 0
		// 处理，补触发一次后立刻重算，不做逐分钟追赶。
		remaining := next.Sub(s.clock())
		if remaining < 0 {
			remaining = 0
		}

		timer := s.wait(remaining)

		select {
		case <-ctx.Done():
			return
		case <-s.reloadCh:
			// Reload 已经把新计划与下一次时刻算好，下一圈直接用新的 next 重新计时。
			continue
		case <-timer:
			if ctx.Err() != nil {
				return
			}
			s.fire(ctx)
			s.recompute()
		}
	}
}

// Reload 用新表达式重排下一次触发，并唤醒等待中的循环（Requirement 9.4）。
//
// 只有解析成功才替换计划：解析失败时返回包装 cronexpr.ErrInvalid 的错误，当前计划与
// 下一次时刻一个字节都不动，循环继续按旧计划等待。这样「保存了非法表达式」不会悄悄
// 把定时任务停掉——校验层已先拒绝，这里只是最后一道兜底（design D10）。
//
// 唤醒用非阻塞发送：循环正在触发或正在处理上一次 Reload 时，重复的唤醒信号合并成
// 一次即可；Reload 本身不该因为循环忙而阻塞 HTTP handler。
//
// 解析放在锁外（纯函数），取时钟、算下一次、替换计划与自增代数放在锁内一次完成：
// 在锁外先算 next 再写回会与 recompute 交错，把 next 写回一个已经被触发过的时刻，
// 同一个计划分钟就会被排第二次（CP9）；代数与计划同临界区更新，启动装载的比对才没有缝。
func (s *Scheduler) Reload(expr string) error {
	schedule, err := cronexpr.Parse(expr)
	if err != nil {
		s.log.Warn("cron 表达式重新装载失败，保留旧计划",
			slog.String("cron", expr),
			slog.Any("err", err),
		)
		return err
	}

	s.mu.Lock()
	next, ok := s.installLocked(schedule, expr)
	s.reloadGen++
	s.mu.Unlock()

	select {
	case s.reloadCh <- struct{}{}:
	default:
	}

	if ok {
		s.log.Info("cron 表达式已重新装载",
			slog.String("cron", expr),
			slog.Time("next_at", next),
		)
	} else {
		// 解析通过但 5 年内没有命中：保留计划，让 Run 走「只等 Reload」的分支，
		// 而不是在这里返回错误——表达式本身合法，只是不会触发。
		s.log.Warn("cron 表达式没有下一次触发时刻",
			slog.String("cron", expr),
		)
	}

	return nil
}

// Next 返回最近一次装载/重算得到的下一次触发时刻快照；没有可用计划时为零值与 false。
//
// 它是快照而不是现算：要回答「按现在的表达式、现在的时钟，下一次是哪一刻」，得在锁内
// 用当前 schedule 与时钟重算一遍，读到的值可能与循环实际等待的那个不一致。它供测试
// 断言与启动日志使用；返回值是快照拷贝，之后的 Reload 不会改写已返回的值，下一次调用才
// 反映新计划。
func (s *Scheduler) Next() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next, s.hasNext
}

// loadInitial 读启动期的表达式。来源错误与缺行都回落 DefaultCronExpr 并继续跑：
// 设置读不出来不应该让整个进程起不来（Requirement 9.5 / 9.6）。
//
// 「缺行」由 settings.Service 在快照层就填成默认值；这里再判一次空串，是给别的
// ExprSource 实现（例如测试替身或将来的配置来源）兜底，口径不靠调用方自觉。
func (s *Scheduler) loadInitial(ctx context.Context) {
	// 读库之前先记下 Reload 代数：这次读库可能耗时（一次 SQL 往返），期间管理员保存
	// 设置触发的 Reload 是更新的意图，装计划前要用代数把它识别出来（minor 2）。
	gen := s.reloadGenValue()

	if s.source == nil {
		s.log.Warn("scheduler 未配置表达式来源，使用默认表达式",
			slog.String("default", model.DefaultCronExpr),
		)
		s.applyStartup(model.DefaultCronExpr, gen)
		return
	}

	expr, err := s.source.CronExpr(ctx)
	if err != nil {
		s.log.Error("读取 cron 表达式失败，使用默认表达式",
			slog.String("default", model.DefaultCronExpr),
			slog.Any("err", err),
		)
		s.applyStartup(model.DefaultCronExpr, gen)
		return
	}

	if strings.TrimSpace(expr) == "" {
		s.log.Info("cron 表达式缺失，使用默认表达式",
			slog.String("default", model.DefaultCronExpr),
		)
		s.applyStartup(model.DefaultCronExpr, gen)
		return
	}

	s.applyStartup(expr, gen)
}

// applyStartup 把启动期表达式装进计划：非法时记一条 ERROR 并回落到 DefaultCronExpr
// （Requirement 9.6）。回落值本身装不上只可能是代码被改坏，记 ERROR 后不装计划，
// 让循环停在「等 Reload」而不是带着一个错误的计划继续跑。
//
// gen 是开始读库时记下的 Reload 代数：安装与代数比对在同一个锁里完成，读库期间到达
// 的 Reload 更晚、更权威，启动期这份旧值不覆盖它（minor 2）。
func (s *Scheduler) applyStartup(expr string, gen uint64) {
	schedule, err := cronexpr.Parse(expr)
	if err != nil {
		s.log.Error("cron 表达式非法，回落到默认表达式",
			slog.String("cron", expr),
			slog.String("default", model.DefaultCronExpr),
			slog.Any("err", err),
		)

		schedule, err = cronexpr.Parse(model.DefaultCronExpr)
		if err != nil {
			s.log.Error("默认 cron 表达式无法解析，调度暂停",
				slog.String("default", model.DefaultCronExpr),
				slog.Any("err", err),
			)
			if !s.installStartup(cronexpr.Schedule{}, "", gen) {
				s.logStartupSuperseded()
			}
			return
		}
		expr = model.DefaultCronExpr
	}

	if !s.installStartup(schedule, expr, gen) {
		s.logStartupSuperseded()
	}
}

// installStartup 装启动期的结论，前提是「读库期间没有发生 Reload」：代数与 gen 不等时
// 说明管理员已经重排过，启动期这份值更旧，直接放弃安装。返回是否安装。
//
// expr 为空串表示连 DefaultCronExpr 都解析不了（代码被改坏），此时清空计划，
// 让循环只等 Reload，而不是带着一个错误的计划继续跑。
func (s *Scheduler) installStartup(schedule cronexpr.Schedule, expr string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reloadGen != gen {
		return false
	}
	if expr == "" {
		s.schedule = cronexpr.Schedule{}
		s.expr = ""
		s.hasSchedule = false
		s.next = time.Time{}
		s.hasNext = false
		return true
	}
	s.installLocked(schedule, expr)
	return true
}

// logStartupSuperseded 记一条「启动装载让位于更晚的 Reload」，只打表达式不打设置值。
func (s *Scheduler) logStartupSuperseded() {
	s.log.Info("启动装载期间已有更晚的 Reload，保留重排后的计划",
		slog.String("cron", s.currentExpr()),
	)
}

// setSchedule 装上一份解析结果并算下一次，返回刚写下的快照（New 只取副作用）。
func (s *Scheduler) setSchedule(schedule cronexpr.Schedule, expr string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installLocked(schedule, expr)
}

// installLocked 在调用方持锁时按当前时钟算下一次并替换计划，返回刚写下的快照。
//
// 取时钟、算 Next、换计划三件事必须在同一个临界区里：在锁外先算 next 再写回会与
// recompute 交错——recompute 用更晚的时钟算出更晚的 next 之后，手里那份更早的推算
// 把 next 写回去，同一个计划分钟就会被等第二次、触发第二次（CP9）。
func (s *Scheduler) installLocked(schedule cronexpr.Schedule, expr string) (time.Time, bool) {
	next, ok := s.nextLocked(schedule, s.clock())

	s.schedule = schedule
	s.expr = expr
	s.hasSchedule = true
	s.next = next
	s.hasNext = ok
	return next, ok
}

// nextLocked 从 after 起算下一次触发，但绝不把 next 排回最近一次触发所在的分钟。
//
// cronexpr.Schedule.Next 保证结果严格晚于传入的 after，所以只要 after 不早于
// firedAt，结果就一定严格晚于上一次触发。Reload 手里可能是触发之前的时钟快照
// （锁外读时钟的旧实现），机器时钟也可能被 NTP 回拨；两种情况都会让已经触发过的
// 分钟被排第二次（CP9：同一表达式与同一 after 恒得同一结果）。
func (s *Scheduler) nextLocked(schedule cronexpr.Schedule, after time.Time) (time.Time, bool) {
	if !s.firedAt.IsZero() && after.Before(s.firedAt) {
		after = s.firedAt
	}
	return schedule.Next(after)
}

// markFired 记下这一次触发的实际时刻，作为 nextLocked 的守卫基准。同一分钟里的多次
// 记录取较晚者，时钟回拨不会把基准拉回去。触发失败也算这个分钟已经用过：不重试、
// 不排队（Requirement 9.3）。
func (s *Scheduler) markFired(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.firedAt) {
		s.firedAt = at
	}
}

// reloadGenValue 读当前 Reload 代数；启动装载在开始读库之前调用它。
func (s *Scheduler) reloadGenValue() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadGen
}

// recompute 在触发之后按当前时刻重算下一次。用当前时刻而不是原计划时刻做起点：
// 一次触发跑过了下一个计划点时，这些错过的时刻直接跳过，不补跑、不排队
// （Requirement 9.3 的「不排队、不重试」）。
func (s *Scheduler) recompute() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.hasSchedule {
		s.next = time.Time{}
		s.hasNext = false
		return
	}
	s.next, s.hasNext = s.nextLocked(s.schedule, s.clock())
}

// currentExpr 返回当前生效的原写法，供日志与测试读；不加锁地读会与 Reload 竞争。
func (s *Scheduler) currentExpr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expr
}

// logStarted 在装载完成后记一条启动日志。只打表达式与下一次时刻，不打任何设置值：
// cron 表达式不是秘密，设置值可能包含凭据（Requirement 10.3、CP1）。
func (s *Scheduler) logStarted() {
	next, ok := s.Next()

	attrs := []any{slog.String("cron", s.currentExpr())}
	if ok {
		attrs = append(attrs, slog.Time("next_at", next))
	}
	s.log.Info("scheduler_started", attrs...)
}

// fire 到点调用一次触发器。30 秒超时派生自 Run 的 ctx：关停时正在跑的触发跟着取消；
// 超时或失败都只记一条 WARN，由 Run 继续下一个计划时刻（Requirement 9.3）。
//
// 进入前再查一次 ctx：关停与到点恰好同时就绪时，select 可能选中 timer 分支，
// 这一次触发不该再建轮（Requirement 9.7）。
func (s *Scheduler) fire(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	// 这个分钟已经用掉了：记下实际触发时刻，之后任何重算都不许再把 next 排回来。
	s.markFired(s.clock())

	triggerCtx, cancel := context.WithTimeout(ctx, triggerTimeout)
	defer cancel()

	if err := s.trigger(triggerCtx); err != nil {
		s.log.Warn("定时触发失败，等待下一个计划时刻",
			slog.String("trigger", string(model.TriggerCron)),
			slog.Any("err", err),
		)
	}
}
