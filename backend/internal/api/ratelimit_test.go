package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// movableClock 是可推进的假时钟：限流窗口用例靠它跨分钟，不用真实等待。
type movableClock struct {
	mu sync.Mutex
	at time.Time
}

// now 返回当前假时刻。
func (c *movableClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance 推进假时刻。
func (c *movableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newMovableClock 用固定起点造一个假时钟。
func newMovableClock(at time.Time) *movableClock {
	return &movableClock{at: at}
}

// sourceRequest 构造一次 /api/query 请求，并指定直连地址与代理来源头。
func sourceRequest(remoteAddr, forwardedFor, realIP string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/query?view=availability", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	if realIP != "" {
		req.Header.Set("X-Real-IP", realIP)
	}
	return req
}

// serveSource 打一次请求并把响应交给断言。
func serveSource(t *testing.T, srv *Server, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// assertRateLimited 断言响应是 429 + 业务码 42901 + 提示稍后重试。
func assertRateLimited(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超额请求期望 HTTP 429，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != codeRateLimited {
		t.Fatalf("期望业务码 %d，实际 %d", codeRateLimited, got.Code)
	}
	if !strings.Contains(got.Message, "稍后重试") {
		t.Fatalf("提示文案应说明稍后重试，实际 %q", got.Message)
	}
}

// TestRateLimitAllowsSixtyThenRejects 覆盖 Requirement 32.1 的主线：
// 同一来源的 60 次请求全部放行，第 61 次返回 429 + 42901。
func TestRateLimitAllowsSixtyThenRejects(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d，body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("窗口内应放行 %d 次查询，实际 %d 次", rateLimitMaxPerWindow, fake.calls)
	}

	assertRateLimited(t, serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", "")))
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("被限流的请求不应再进 handler，实际查询 %d 次", fake.calls)
	}
}

// TestRateLimitIndependentPerSource 覆盖不同来源互不占用额度。
func TestRateLimitIndependentPerSource(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", ""))
	}
	assertRateLimited(t, serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", "")))

	// 换一个来源，额度是独立的。
	rec := serveSource(t, srv, sourceRequest("198.51.100.8:4321", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("另一来源应放行，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
}

// TestRateLimitWindowReset 覆盖窗口重置：未满一分钟仍拒绝，满一分钟后重新放行。
func TestRateLimitWindowReset(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", ""))
	}

	// 差 1 秒到期：仍在同一窗口内。
	clock.advance(rateLimitWindow - time.Second)
	assertRateLimited(t, serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", "")))

	// 恰好到期：重开窗口，本次放行。
	clock.advance(time.Second)
	rec := serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("窗口重置后应放行，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
}

// TestRateLimitSeparatesClientsBehindProxy 覆盖 design D2 的部署形态：
// 直连地址恒为反代，但反代给出的真实来源不同，应落进不同的限流桶。
func TestRateLimitSeparatesClientsBehindProxy(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		serveSource(t, srv, sourceRequest("172.18.0.3:5000", "203.0.113.5", ""))
	}
	assertRateLimited(t, serveSource(t, srv, sourceRequest("172.18.0.3:5000", "203.0.113.5", "")))

	// 同一个反代地址、另一个真实来源，不能被前一个用户挤爆。
	rec := serveSource(t, srv, sourceRequest("172.18.0.3:5000", "203.0.113.6", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("不同真实来源应各自计数，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
}

// TestRateLimitUnaffectedByForgedLeftmostHop 覆盖伪造最左 XFF：
// 客户端只能控制最左端值，最右一跳由反代追加，所以换个伪造值也该落进同一个桶。
func TestRateLimitUnaffectedByForgedLeftmostHop(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		// 每次伪造一个不同的最左值，最右端始终是反代追加的真实来源。
		forged := fmt.Sprintf("198.51.100.%d, 203.0.113.5", i%250+1)
		rec := serveSource(t, srv, sourceRequest("172.18.0.3:5000", forged, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d", i+1, rec.Code)
		}
	}

	assertRateLimited(t, serveSource(t, srv, sourceRequest("172.18.0.3:5000", "192.0.2.99, 203.0.113.5", "")))
}

// TestRateLimitIgnoresProxyHeadersWhenUntrusted 覆盖 server 直接暴露时的口径：
// 关闭可信反代后只认直连地址，客户端自填 XFF 不能让限流放宽。
func TestRateLimitIgnoresProxyHeadersWhenUntrusted(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{}}
	srv := New(quietLogger(), nil, nil, fake, nil, WithTrustedProxy(false))
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		forged := fmt.Sprintf("198.51.100.%d", i%250+1)
		rec := serveSource(t, srv, sourceRequest("198.51.100.7:4321", forged, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d", i+1, rec.Code)
		}
	}

	assertRateLimited(t, serveSource(t, srv, sourceRequest("198.51.100.7:4321", "203.0.113.99", "")))
}

// cacheProbeLookup 记录空教室查询真正落到存储的次数。
type cacheProbeLookup struct {
	calls int // 存储层被调用的次数
}

// lookup 实现 AvailabilityLookup：返回「该周有观测但无命中」的空态，便于观察缓存行为。
func (f *cacheProbeLookup) lookup(context.Context, query.AvailabilityFilter) (query.AvailabilityPage, error) {
	f.calls++
	return query.AvailabilityPage{Total: 0, HasRelease: true, WeekHasObservation: true}, nil
}

// inCalendarWeekLookup 返回固定教学周，让查询一路走到存储层。
func inCalendarWeekLookup(context.Context, time.Time) (model.TermWeek, bool, error) {
	return model.TermWeek{
		Term:       "2026-2027-1",
		Week:       5,
		Monday:     time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		InCalendar: true,
	}, true, nil
}

// TestRateLimitCountsCachedRequests 覆盖限流与缓存的先后：
// 60 次相同查询里只有第 1 次查存储（其余命中缓存），但第 61 次仍被限流拒绝。
// 若顺序反了（先查缓存再限流），第 61 次会照样返回 200。
func TestRateLimitCountsCachedRequests(t *testing.T) {
	probe := &cacheProbeLookup{}
	svc := query.New(inCalendarWeekLookup, probe.lookup, nil, query.WithCache(query.DefaultCacheTTL))
	srv := New(quietLogger(), nil, svc, svc, nil)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d，body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if probe.calls != 1 {
		t.Fatalf("相同查询应只查一次存储并复用缓存，实际 %d 次", probe.calls)
	}

	assertRateLimited(t, serveSource(t, srv, sourceRequest("198.51.100.7:4321", "", "")))
	if probe.calls != 1 {
		t.Fatalf("被限流的请求不应触发查询，实际存储调用 %d 次", probe.calls)
	}
}

// TestClientSource 覆盖来源判定的各种头组合。
func TestClientSource(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string // 直连对端
		forwarded  string // X-Forwarded-For
		realIP     string // X-Real-IP
		trust      bool   // 是否信任前置反代
		want       string // 期望的来源标识
	}{
		{name: "无代理头时用 RemoteAddr", remoteAddr: "198.51.100.7:4321", trust: true, want: "198.51.100.7"},
		{name: "RemoteAddr 没有端口", remoteAddr: "198.51.100.7", trust: true, want: "198.51.100.7"},
		{name: "单跳 XFF 直接用", remoteAddr: "172.18.0.3:5000", forwarded: "203.0.113.9", trust: true, want: "203.0.113.9"},
		{name: "多跳 XFF 取最右一跳", remoteAddr: "172.18.0.3:5000", forwarded: "1.1.1.1, 203.0.113.9", trust: true, want: "203.0.113.9"},
		{name: "最右一跳带空白也能解析", remoteAddr: "172.18.0.3:5000", forwarded: " 1.1.1.1 ,  203.0.113.9 ", trust: true, want: "203.0.113.9"},
		{name: "XFF 非法时回退 X-Real-IP", remoteAddr: "172.18.0.3:5000", forwarded: "not-an-ip", realIP: "192.0.2.8", trust: true, want: "192.0.2.8"},
		{name: "没有 XFF 时用 X-Real-IP", remoteAddr: "172.18.0.3:5000", realIP: "192.0.2.8", trust: true, want: "192.0.2.8"},
		{name: "XFF 优先于 X-Real-IP", remoteAddr: "172.18.0.3:5000", forwarded: "203.0.113.9", realIP: "192.0.2.8", trust: true, want: "203.0.113.9"},
		{name: "不信任反代时忽略代理头", remoteAddr: "198.51.100.7:4321", forwarded: "203.0.113.9", realIP: "192.0.2.8", trust: false, want: "198.51.100.7"},
		{name: "IPv4 映射地址归一", remoteAddr: "172.18.0.3:5000", forwarded: "::ffff:203.0.113.9", trust: true, want: "203.0.113.9"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := sourceRequest(c.remoteAddr, c.forwarded, c.realIP)
			if got := clientSource(req, c.trust); got != c.want {
				t.Fatalf("来源期望 %q，实际 %q", c.want, got)
			}
		})
	}
}

// TestClientSourceIgnoresForgedLeftmostHop 覆盖伪造最左 XFF 不改变来源判定：
// 反代只会在末尾追加，最左端怎么改都不影响结果。
func TestClientSourceIgnoresForgedLeftmostHop(t *testing.T) {
	want := "203.0.113.5"
	for i := 0; i < 5; i++ {
		forged := fmt.Sprintf("198.51.100.%d, 203.0.113.5", i+1)
		req := sourceRequest("172.18.0.3:5000", forged, "")
		if got := clientSource(req, true); got != want {
			t.Fatalf("伪造最左值 %q 后来源变了: %q，期望 %q", forged, got, want)
		}
	}
}

// TestClientSourceUsesRightmostRepeatedHeader 覆盖 X-Forwarded-For 被重复发送：
// 按出现顺序拼接后取最右，仍是最靠近本服务的那一跳。
func TestClientSourceUsesRightmostRepeatedHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/query", nil)
	req.RemoteAddr = "172.18.0.3:5000"
	req.Header.Add("X-Forwarded-For", "1.1.1.1")
	req.Header.Add("X-Forwarded-For", "203.0.113.5")

	if got := clientSource(req, true); got != "203.0.113.5" {
		t.Fatalf("重复头的来源期望 %q，实际 %q", "203.0.113.5", got)
	}
}
