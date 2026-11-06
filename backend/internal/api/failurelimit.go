package api

import (
	"sync"
	"time"
)

// 登录与初始化失败限流的口径（Requirement 7.1、7.6）：
// 同一来源在 60 秒窗口内累计 5 次失败后，下一次请求被拦；窗口从该来源
// 本轮首次失败的时刻起算，到点即恢复，不做滑动续期。
const (
	failureLimitMaxFails = 5
	failureLimitWindow   = time.Minute
)

// failureBucket 是一个来源在当前窗口内的失败计数。
type failureBucket struct {
	windowStart time.Time // 本轮窗口的起点，即该来源本轮首次失败的时刻
	fails       int       // 窗口内已记录的失败次数
}

// failureLimiter 按来源统计「失败次数」，把超过阈值的来源拦到窗口结束。
// 它不数请求：公开查询的 rateLimiter 数请求，登录与初始化必须数失败，
// 两者语义不同所以分开实现，只共用来源判定 clientSource。
// 表上限与淘汰策略与 rateLimiter 一致：来源标识来自客户端，不设上限等于把
// 内存交给调用方；满表时先清过期窗口、再淘汰窗口最旧的一条。
type failureLimiter struct {
	mu       sync.Mutex
	limit    int                       // 窗口内允许的失败次数，达到即拦截
	window   time.Duration             // 窗口长度
	max      int                       // 表内来源上限
	failures map[string]*failureBucket // 来源 → 当前窗口失败计数
}

// newFailureLimiter 组装限流器；limit 与 window 必须为正，调用方只传常量。
func newFailureLimiter(limit int, window time.Duration) *failureLimiter {
	return &failureLimiter{
		limit:    limit,
		window:   window,
		max:      rateLimitMaxSources,
		failures: make(map[string]*failureBucket),
	}
}

// Blocked 判定来源此刻是否应被拦截，now 由调用方提供以便测试注入时钟。
// 未达阈值、或窗口已到期都返回 (false, 0)；被拦时返回窗口剩余时间，
// 向上取整到整秒后落在 (0, window]，调用方除以 time.Second 即得 Retry-After 秒数。
// 向上取整是为了满足「取值不小于 1」：剩余不足 1 秒时不能报 0 秒让客户端立刻重试。
func (l *failureLimiter) Blocked(source string, now time.Time) (blocked bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.failures[source]
	if !exists {
		return false, 0
	}
	// 窗口到期（now ≥ 起点 + 窗口长度）按「不再拦截」处理，边界与 rateLimiter 一致：
	// 窗口重置后的下一次尝试必然放行，不会因为差一纳秒继续拦。
	remaining := bucket.windowStart.Add(l.window).Sub(now)
	if remaining <= 0 || bucket.fails < l.limit {
		return false, 0
	}
	return true, ceilToSecond(remaining)
}

// Fail 记录一次失败；新来源或窗口已到期时从此刻起算新窗口。
// 窗口内继续失败只增加计数、不推迟窗口终点：窗口锚定在本轮首次失败，
// 所以持续尝试不会把封锁无限延长，到点一定恢复。
func (l *failureLimiter) Fail(source string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, exists := l.failures[source]
	if exists {
		if now.Before(bucket.windowStart.Add(l.window)) {
			bucket.fails++
			return
		}
		// 旧窗口已到期：原地重开，不额外占用表位置。
		l.failures[source] = &failureBucket{windowStart: now, fails: 1}
		return
	}

	if len(l.failures) >= l.max {
		l.evictLocked(now)
	}
	l.failures[source] = &failureBucket{windowStart: now, fails: 1}
}

// Reset 清空一个来源的失败计数，登录成功时调用（Requirement 7.3）。
// 删掉整条记录而不是把计数置零：没在失败中的来源不该继续占用表位置。
func (l *failureLimiter) Reset(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, source)
}

// evictLocked 在表满时腾一个位置：先删窗口已过期的来源，仍满则淘汰窗口最旧的一条。
// 淘汰只让那个来源提前重开窗口（等于短暂放宽），不会改动其他来源的计数；
// 调用方必须已持有锁。
func (l *failureLimiter) evictLocked(now time.Time) {
	for source, bucket := range l.failures {
		if !now.Before(bucket.windowStart.Add(l.window)) {
			delete(l.failures, source)
		}
	}
	if len(l.failures) < l.max {
		return
	}
	var oldestSource string
	var oldestStart time.Time
	for source, bucket := range l.failures {
		if oldestSource == "" || bucket.windowStart.Before(oldestStart) {
			oldestSource, oldestStart = source, bucket.windowStart
		}
	}
	delete(l.failures, oldestSource)
}

// ceilToSecond 把剩余时间向上取整到整秒，保证调用方拿到的值恒为正且不超过窗口。
func ceilToSecond(d time.Duration) time.Duration {
	return (d + time.Second - 1) / time.Second * time.Second
}
