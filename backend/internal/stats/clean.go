package stats

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Deleter 删除边界之前的统计记录；通常由 store.DeleteStatsBefore 实现。
type Deleter interface {
	DeleteStatsBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Cleaner 按日清理过期统计（Requirement 34.10）。清理失败只记日志，不影响查询与采集。
type Cleaner struct {
	log      *slog.Logger
	deleter  Deleter
	clock    func() time.Time
	interval time.Duration

	done       chan struct{} // Run 退出时关闭，供 Wait 等待清理退出
	finishOnce sync.Once     // 保证 done 只关一次，Run 重复调用也不会 panic
}

// CleanerOption 调整清理任务的时钟与间隔；不传时按默认口径工作。
type CleanerOption func(*Cleaner)

// WithCleanerClock 注入时间来源，测试用假时钟验证 90 天边界。
func WithCleanerClock(clock func() time.Time) CleanerOption {
	return func(c *Cleaner) {
		if clock != nil {
			c.clock = clock
		}
	}
}

// WithCleanerInterval 设置两次清理之间的间隔；<=0 时保持默认的 24 小时。
func WithCleanerInterval(interval time.Duration) CleanerOption {
	return func(c *Cleaner) {
		if interval > 0 {
			c.interval = interval
		}
	}
}

// NewCleaner 组装清理任务；deleter 为 nil 时清理变成空操作。
func NewCleaner(log *slog.Logger, deleter Deleter, opts ...CleanerOption) *Cleaner {
	if log == nil {
		log = slog.Default()
	}
	c := &Cleaner{
		log:      log,
		deleter:  deleter,
		clock:    time.Now,
		interval: cleanupInterval,
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Run 启动时先清一次，之后按 interval 重复，直到 ctx 结束。
// 清理失败只记日志并等下一个周期：统计过期不影响对外服务，不值得重试打满日志。
func (c *Cleaner) Run(ctx context.Context) {
	if c == nil {
		return
	}
	// 没装配删除入口时也要让 Wait 拿到终态，避免关闭流程永远挂住。
	defer c.finish()
	if c.deleter == nil {
		return
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		if _, err := c.CleanupOnce(ctx, c.clock()); err != nil {
			c.log.Warn("访问统计清理失败", "err", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Wait 等待 Run 退出：返回时不再有正在执行的清理语句，调用方可以安全关池。
// 只能在 Run 启动之后调用；没有启动 Run 时没有可等的退出，会一直阻塞。
func (c *Cleaner) Wait() {
	if c == nil {
		return
	}
	<-c.done
}

// finish 关闭 done，唤醒 Wait。
func (c *Cleaner) finish() {
	c.finishOnce.Do(func() { close(c.done) })
}

// CleanupOnce 删除早于「今天减 RetentionDays」的记录，返回删除行数。
// 边界是「早于」而不是「早于等于」：恰好 90 天前的那一天仍然保留（Requirement 34.10）。
func (c *Cleaner) CleanupOnce(ctx context.Context, now time.Time) (int64, error) {
	if c == nil || c.deleter == nil {
		return 0, nil
	}

	cutoff := c.cutoff(now)
	deleted, err := c.deleter.DeleteStatsBefore(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if deleted > 0 {
		c.log.Info("访问统计已清理", "cutoff", cutoff.Format(time.DateOnly), "deleted", deleted)
	}
	return deleted, nil
}

// cutoff 是清理边界：今天（Asia/Shanghai）往前推 RetentionDays 天的当日 00:00。
func (c *Cleaner) cutoff(now time.Time) time.Time {
	return dayOf(now).AddDate(0, 0, -RetentionDays)
}
