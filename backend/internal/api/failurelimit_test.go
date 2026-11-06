package api

// 本文件覆盖登录与初始化失败计数限流（Requirement 7、CP9、CP10）。
// 全部用例用假时钟注入时刻，不读真实时间，也不写 t.Parallel——与同包其他用例的既有约定一致。

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// loginAttempt 走一次「先判是否被拦、未拦则记一次失败」的尝试，返回是否放行。
// 顺序与真实处理器一致：被拦的尝试不记失败，所以持续重试不会把窗口终点往后推。
func loginAttempt(l *failureLimiter, source string, now time.Time) bool {
	if blocked, _ := l.Blocked(source, now); blocked {
		return false
	}
	l.Fail(source, now)
	return true
}

// assertRetryAfter 断言拦截时给出的剩余时间落在 (0, window] 且不小于 1 秒。
func assertRetryAfter(t *testing.T, retryAfter, window time.Duration) {
	t.Helper()
	if retryAfter <= 0 || retryAfter > window {
		t.Fatalf("Retry-After 期望落在 (0, %s]，实际 %s", window, retryAfter)
	}
	if retryAfter < time.Second {
		t.Fatalf("Retry-After 不应小于 1 秒，否则客户端会立刻重试，实际 %s", retryAfter)
	}
}

// evictionSource 生成第 i 个互不相同的来源标识，供填满来源表的用例使用。
func evictionSource(i int) string {
	return fmt.Sprintf("198.51.%d.%d", i/256, i%256)
}

// TestFailureLimiterBlocksSixthAttempt 覆盖 Requirement 7.1 的主线：
// 同一来源连续 5 次失败都放行，第 6 次尝试被拦，Retry-After 落在 (0, 60s]。
func TestFailureLimiterBlocksSixthAttempt(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()
	source := "198.51.100.7"

	// 第 1–5 次尝试放行并各记一次失败；第 5 次之后恰好达到阈值。
	for i := 1; i <= failureLimitMaxFails; i++ {
		if !loginAttempt(limiter, source, now) {
			t.Fatalf("第 %d 次尝试应放行（失败计数未达阈值），实际被拦", i)
		}
	}

	blocked, retryAfter := limiter.Blocked(source, now)
	if !blocked {
		t.Fatalf("累计 %d 次失败后第 %d 次尝试应被拦", failureLimitMaxFails, failureLimitMaxFails+1)
	}
	assertRetryAfter(t, retryAfter, failureLimitWindow)
	// 5 次失败都发生在同一时刻：剩余时间就是整个窗口，向上取整后仍是 60 秒。
	if retryAfter != failureLimitWindow {
		t.Fatalf("同一时刻失败 %d 次后 Retry-After 期望 %s，实际 %s", failureLimitMaxFails, failureLimitWindow, retryAfter)
	}
	if loginAttempt(limiter, source, now) {
		t.Fatalf("第 %d 次尝试不应放行", failureLimitMaxFails+1)
	}
}

// TestFailureLimiterSeparatesSources 覆盖来源之间的计数互不相干：
// 一个来源被拦不影响另一个来源，后者的 Retry-After 仍为零值。
func TestFailureLimiterSeparatesSources(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()

	for i := 0; i < failureLimitMaxFails; i++ {
		loginAttempt(limiter, "198.51.100.7", now)
	}
	if blocked, _ := limiter.Blocked("198.51.100.7", now); !blocked {
		t.Fatalf("达到阈值的来源应被拦")
	}

	if !loginAttempt(limiter, "198.51.100.8", now) {
		t.Fatalf("另一来源的计数是独立的，应放行")
	}
	if blocked, retryAfter := limiter.Blocked("198.51.100.8", now); blocked || retryAfter != 0 {
		t.Fatalf("另一来源不应被拦，实际 blocked=%v retryAfter=%s", blocked, retryAfter)
	}
}

// TestFailureLimiterRetryAfterRoundsUpToOneSecond 覆盖 Requirement 7.4 的下界：
// 窗口只剩 0.5 秒时，剩余时间向上取整为 1 秒，而不是 0 秒。
func TestFailureLimiterRetryAfterRoundsUpToOneSecond(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	clock := newMovableClock(fixedQueryClock())
	source := "198.51.100.7"

	for i := 0; i < failureLimitMaxFails; i++ {
		loginAttempt(limiter, source, clock.now())
	}

	clock.advance(failureLimitWindow - 500*time.Millisecond)
	blocked, retryAfter := limiter.Blocked(source, clock.now())
	if !blocked {
		t.Fatalf("窗口到期前一刻仍应拦截")
	}
	if retryAfter != time.Second {
		t.Fatalf("剩余 0.5 秒应向上取整为 1 秒，实际 %s", retryAfter)
	}
	assertRetryAfter(t, retryAfter, failureLimitWindow)
}

// TestFailureLimiterWindowRecovery 覆盖 Requirement 7.2 的到期恢复：
// 差 1 秒到期仍拒绝，满 60 秒后恢复，并且恢复后的失败从新窗口重新计数。
func TestFailureLimiterWindowRecovery(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	clock := newMovableClock(fixedQueryClock())
	source := "198.51.100.7"

	for i := 0; i < failureLimitMaxFails; i++ {
		loginAttempt(limiter, source, clock.now())
	}
	if loginAttempt(limiter, source, clock.now()) {
		t.Fatalf("窗口内第 %d 次尝试应被拦", failureLimitMaxFails+1)
	}

	clock.advance(failureLimitWindow - time.Second)
	if blocked, _ := limiter.Blocked(source, clock.now()); !blocked {
		t.Fatalf("差 1 秒到期时仍应拦截")
	}

	clock.advance(time.Second)
	if blocked, retryAfter := limiter.Blocked(source, clock.now()); blocked || retryAfter != 0 {
		t.Fatalf("窗口恰好到期应恢复，实际 blocked=%v retryAfter=%s", blocked, retryAfter)
	}
	if !loginAttempt(limiter, source, clock.now()) {
		t.Fatalf("窗口到期后的尝试应放行")
	}

	// 恢复后的失败从新窗口重新计数：上一次恢复尝试已经是新窗口的第 1 次失败，
	// 再失败 3 次才累计 4 次，仍未达阈值。
	for i := 0; i < failureLimitMaxFails-2; i++ {
		if !loginAttempt(limiter, source, clock.now()) {
			t.Fatalf("新窗口内第 %d 次失败应放行", i+2)
		}
	}
	if blocked, _ := limiter.Blocked(source, clock.now()); blocked {
		t.Fatalf("新窗口内失败未达阈值时不应拦截")
	}
	if !loginAttempt(limiter, source, clock.now()) {
		t.Fatalf("新窗口内第 %d 次失败应放行（恰好达到阈值）", failureLimitMaxFails)
	}
	if loginAttempt(limiter, source, clock.now()) {
		t.Fatalf("新窗口内累计 %d 次失败后第 %d 次尝试应被拦", failureLimitMaxFails, failureLimitMaxFails+1)
	}
}

// TestFailureLimiterAnchorsWindowAtFirstFailure 覆盖 Requirement 7.2 的起算点：
// 窗口锚定在本轮首次失败时刻，后续失败不会把终点往后推，到点必恢复。
func TestFailureLimiterAnchorsWindowAtFirstFailure(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	clock := newMovableClock(fixedQueryClock())
	source := "198.51.100.7"

	if !loginAttempt(limiter, source, clock.now()) {
		t.Fatalf("首次失败的尝试应放行")
	}
	// 其余 4 次失败落在窗口末秒：若窗口从最后一次失败起算，剩余时间会是 60 秒。
	clock.advance(failureLimitWindow - time.Second)
	for i := 0; i < failureLimitMaxFails-1; i++ {
		loginAttempt(limiter, source, clock.now())
	}

	blocked, retryAfter := limiter.Blocked(source, clock.now())
	if !blocked {
		t.Fatalf("累计 %d 次失败后应被拦", failureLimitMaxFails)
	}
	if retryAfter != time.Second {
		t.Fatalf("窗口应从首次失败起算，剩余 1 秒，实际 %s", retryAfter)
	}

	clock.advance(time.Second)
	if blocked, _ := limiter.Blocked(source, clock.now()); blocked {
		t.Fatalf("首次失败起算满 %s 应恢复，即使期间一直在失败", failureLimitWindow)
	}
}

// TestFailureLimiterResetClearsCount 覆盖 Requirement 7.3：
// 登录成功清零该来源的计数，清零后要重新失败 5 次才会再被拦。
func TestFailureLimiterResetClearsCount(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()
	source := "198.51.100.7"

	for i := 0; i < failureLimitMaxFails; i++ {
		loginAttempt(limiter, source, now)
	}
	if blocked, _ := limiter.Blocked(source, now); !blocked {
		t.Fatalf("清零前应处于拦截状态")
	}

	limiter.Reset(source)
	if blocked, retryAfter := limiter.Blocked(source, now); blocked || retryAfter != 0 {
		t.Fatalf("清零后不应再拦截，实际 blocked=%v retryAfter=%s", blocked, retryAfter)
	}

	// 计数若没真正归零，这里的第 1 次失败就会立刻超限：4 次失败后仍未达阈值。
	for i := 0; i < failureLimitMaxFails-1; i++ {
		if !loginAttempt(limiter, source, now) {
			t.Fatalf("清零后第 %d 次失败应放行", i+1)
		}
	}
	if blocked, _ := limiter.Blocked(source, now); blocked {
		t.Fatalf("清零后失败未达阈值时不应拦截")
	}
	if !loginAttempt(limiter, source, now) {
		t.Fatalf("清零后第 %d 次失败应放行（恰好达到阈值）", failureLimitMaxFails)
	}
	if loginAttempt(limiter, source, now) {
		t.Fatalf("清零后重新累计 %d 次失败，第 %d 次尝试应被拦", failureLimitMaxFails, failureLimitMaxFails+1)
	}
}

// TestFailureLimiterEvictsExpiredBeforeOldest 覆盖「满时先清过期」：
// 表满且所有窗口都过期时，新增来源把过期记录全部清掉，而不是只淘汰最旧一条。
func TestFailureLimiterEvictsExpiredBeforeOldest(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	clock := newMovableClock(fixedQueryClock())

	for i := 0; i < rateLimitMaxSources; i++ {
		limiter.Fail(evictionSource(i), clock.now())
	}
	if len(limiter.failures) != rateLimitMaxSources {
		t.Fatalf("来源表应恰好填满 %d 条，实际 %d", rateLimitMaxSources, len(limiter.failures))
	}

	clock.advance(failureLimitWindow + time.Second)
	limiter.Fail("203.0.113.9", clock.now())
	if len(limiter.failures) != 1 {
		t.Fatalf("过期记录应全部清掉只留新来源，实际 %d 条", len(limiter.failures))
	}
	if _, ok := limiter.failures["203.0.113.9"]; !ok {
		t.Fatalf("新来源应写入来源表")
	}
	if blocked, _ := limiter.Blocked("203.0.113.9", clock.now()); blocked {
		t.Fatalf("新来源只有 1 次失败，不应被拦")
	}
}

// TestFailureLimiterEvictsOldestWhenNothingExpired 覆盖「无过期时淘汰最旧」：
// 只淘汰窗口最旧的一条，其他来源的记录与失败计数都原样保留。
func TestFailureLimiterEvictsOldestWhenNothingExpired(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	clock := newMovableClock(fixedQueryClock())

	// 4095 个来源的窗口起点逐毫秒递增，最旧的是 evictionSource(0)。
	for i := 0; i < rateLimitMaxSources-1; i++ {
		limiter.Fail(evictionSource(i), clock.now())
		clock.advance(time.Millisecond)
	}
	// 再补一个已满 5 次失败的来源占满表；它比上面都新，不会先被淘汰。
	for i := 0; i < failureLimitMaxFails; i++ {
		limiter.Fail("203.0.113.7", clock.now())
	}
	if len(limiter.failures) != rateLimitMaxSources {
		t.Fatalf("来源表应恰好填满 %d 条，实际 %d", rateLimitMaxSources, len(limiter.failures))
	}

	clock.advance(time.Second)
	limiter.Fail("203.0.113.9", clock.now())

	if len(limiter.failures) != rateLimitMaxSources {
		t.Fatalf("淘汰一条再插入一条后仍应是 %d 条，实际 %d", rateLimitMaxSources, len(limiter.failures))
	}
	if _, ok := limiter.failures[evictionSource(0)]; ok {
		t.Fatalf("窗口最旧的来源应被淘汰")
	}
	if _, ok := limiter.failures[evictionSource(1)]; !ok {
		t.Fatalf("淘汰只应影响最旧一条，次旧来源不该消失")
	}

	blocked, retryAfter := limiter.Blocked("203.0.113.7", clock.now())
	if !blocked {
		t.Fatalf("淘汰其他来源不应改变本来源的拦截状态")
	}
	assertRetryAfter(t, retryAfter, failureLimitWindow)
}

// TestFailureLimiterReusesClientSourceRightmostHop 覆盖 Requirement 7.5：
// 可信反代下来源取 XFF 最右可解析跳，客户端伪造最左值既不换桶也不放宽限制。
func TestFailureLimiterReusesClientSourceRightmostHop(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()

	for i := 1; i <= failureLimitMaxFails; i++ {
		req := sourceRequest("172.18.0.3:5000", fmt.Sprintf("198.51.100.%d, 203.0.113.5", i), "")
		if source := clientSource(req, true); source != "203.0.113.5" {
			t.Fatalf("第 %d 次请求的来源期望 %q，实际 %q", i, "203.0.113.5", source)
		}
		if !loginAttempt(limiter, clientSource(req, true), now) {
			t.Fatalf("第 %d 次尝试应放行", i)
		}
	}

	forged := sourceRequest("172.18.0.3:5000", "192.0.2.99, 203.0.113.5", "")
	if loginAttempt(limiter, clientSource(forged, true), now) {
		t.Fatalf("伪造最左值不能让第 %d 次尝试绕过限流", failureLimitMaxFails+1)
	}

	other := sourceRequest("172.18.0.3:5000", "192.0.2.99, 203.0.113.6", "")
	if !loginAttempt(limiter, clientSource(other, true), now) {
		t.Fatalf("同一个反代地址、另一个真实来源应各自计数")
	}
}

// TestFailureLimiterIgnoresForwardedForWhenProxyUntrusted 覆盖 server 直接暴露时的口径：
// 不信任反代就没有 XFF 可采信，客户端自填哪个值都落进直连对端的桶。
func TestFailureLimiterIgnoresForwardedForWhenProxyUntrusted(t *testing.T) {
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()

	for i := 1; i <= failureLimitMaxFails; i++ {
		req := sourceRequest("198.51.100.7:4321", fmt.Sprintf("203.0.113.%d", i), "")
		if !loginAttempt(limiter, clientSource(req, false), now) {
			t.Fatalf("第 %d 次尝试应放行", i)
		}
	}

	req := sourceRequest("198.51.100.7:4321", "192.0.2.99", "")
	if loginAttempt(limiter, clientSource(req, false), now) {
		t.Fatalf("不信任反代时自填 XFF 不能绕过限流")
	}
}

// TestFailureLimiterUsesConfiguredLimitAndWindow 覆盖阈值与窗口来自构造参数：
// 这里按 2 次 / 3 秒工作，证明限流器没有写死设计里的 5 次 / 60 秒。
func TestFailureLimiterUsesConfiguredLimitAndWindow(t *testing.T) {
	const (
		limit  = 2
		window = 3 * time.Second
	)
	limiter := newFailureLimiter(limit, window)
	clock := newMovableClock(fixedQueryClock())
	source := "198.51.100.7"

	for i := 0; i < limit; i++ {
		if !loginAttempt(limiter, source, clock.now()) {
			t.Fatalf("第 %d 次尝试应放行", i+1)
		}
	}
	blocked, retryAfter := limiter.Blocked(source, clock.now())
	if !blocked {
		t.Fatalf("按 %d 次阈值，第 %d 次尝试应被拦", limit, limit+1)
	}
	assertRetryAfter(t, retryAfter, window)

	clock.advance(window)
	if blocked, _ := limiter.Blocked(source, clock.now()); blocked {
		t.Fatalf("%s 窗口到期应恢复", window)
	}
}

// TestFailureLimiterRecordsConcurrentFailures 覆盖并发记失败不丢计数：
// 同一来源的失败次数在锁内累加，窗口起点仍是首次失败。
func TestFailureLimiterRecordsConcurrentFailures(t *testing.T) {
	const (
		goroutines   = 8
		perGoroutine = 50
	)
	limiter := newFailureLimiter(failureLimitMaxFails, failureLimitWindow)
	now := fixedQueryClock()

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				limiter.Fail("198.51.100.7", now)
			}
		}()
	}
	wg.Wait()

	limiter.mu.Lock()
	got := limiter.failures["198.51.100.7"].fails
	limiter.mu.Unlock()
	if got != goroutines*perGoroutine {
		t.Fatalf("并发失败应累计 %d 次，实际 %d 次", goroutines*perGoroutine, got)
	}
	if blocked, _ := limiter.Blocked("198.51.100.7", now); !blocked {
		t.Fatalf("远超阈值的来源应被拦")
	}
}
