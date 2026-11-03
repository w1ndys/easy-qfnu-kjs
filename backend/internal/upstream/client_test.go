package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// newTestClient 构造一个校验通过的测试客户端，避免每个用例重复处理 New 的错误。
func newTestClient(t *testing.T, baseURL string, opts ...Option) *Client {
	t.Helper()
	c, err := New(baseURL, opts...)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

// TestNewRejectsInvalidConfig 覆盖根地址与节奏参数的启动期校验：
// 任一项非法都返回 model.ErrConfigInvalid，由入口拒绝启动。
func TestNewRejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		opts    []Option
	}{
		{name: "根地址为空", baseURL: ""},
		{name: "缺协议头", baseURL: "zhjw.qfnu.edu.cn"},
		{name: "协议不支持", baseURL: "ftp://zhjw.qfnu.edu.cn"},
		{name: "缺主机", baseURL: "http://"},
		{name: "间隔为负", baseURL: "http://upstream.test", opts: []Option{WithMinInterval(-time.Millisecond)}},
		{name: "重试为负", baseURL: "http://upstream.test", opts: []Option{WithMaxRetries(-1)}},
		{name: "User-Agent 为空", baseURL: "http://upstream.test", opts: []Option{WithUserAgent("")}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(c.baseURL, c.opts...); !errors.Is(err, model.ErrConfigInvalid) {
				t.Fatalf("期望 ErrConfigInvalid，实际 err=%v", err)
			}
		})
	}
}

// TestNewDefaults 锁定默认口径：11.5 的相邻请求 500ms、首发之外最多 3 次重试与
// 单次尝试 15s 超时，upstream-api 请求头的统一 User-Agent，以及 2.6 的内存会话 CookieJar。
func TestNewDefaults(t *testing.T) {
	c := newTestClient(t, "http://upstream.test")

	if c.minInterval != 500*time.Millisecond {
		t.Fatalf("默认间隔期望 500ms，实际 %v", c.minInterval)
	}
	if c.maxRetries != 3 {
		t.Fatalf("默认重试次数期望 3，实际 %d", c.maxRetries)
	}
	if c.attemptTimeout != 15*time.Second {
		t.Fatalf("默认单次尝试超时期望 15s，实际 %v", c.attemptTimeout)
	}
	if c.userAgent != DefaultUserAgent {
		t.Fatalf("默认 User-Agent 期望 %q，实际 %q", DefaultUserAgent, c.userAgent)
	}
	if c.httpClient.Jar == nil {
		t.Fatal("默认客户端必须带内存 CookieJar")
	}
}

// TestDoPostFormSendsUnifiedHeaders 覆盖统一请求头与表单编码：
// Content-Type、User-Agent 固定，POST 正文是表单编码，根地址的路径前缀保留。
func TestDoPostFormSendsUnifiedHeaders(t *testing.T) {
	type captured struct {
		method      string
		path        string
		contentType string
		userAgent   string
		form        url.Values
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "表单解析失败", http.StatusBadRequest)
			return
		}
		got <- captured{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
			form:        r.PostForm,
		}
		_, _ = io.WriteString(w, "scode#sxh")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL+"/jw", WithMinInterval(0))
	form := url.Values{"userAccount": {""}, "RANDOMCODE": {"1234"}}
	resp, err := c.postForm(t.Context(), "/Logon.do", form)
	if err != nil {
		t.Fatalf("表单请求失败: %v", err)
	}
	if got := string(resp.Body); got != "scode#sxh" {
		t.Fatalf("响应正文期望 %q，实际 %q", "scode#sxh", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码期望 200，实际 %d", resp.StatusCode)
	}

	req := <-got
	if req.method != http.MethodPost {
		t.Fatalf("方法期望 POST，实际 %q", req.method)
	}
	if req.path != "/jw/Logon.do" {
		t.Fatalf("路径期望 /jw/Logon.do，实际 %q", req.path)
	}
	if req.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type 期望表单编码，实际 %q", req.contentType)
	}
	if req.userAgent != DefaultUserAgent {
		t.Fatalf("User-Agent 期望 %q，实际 %q", DefaultUserAgent, req.userAgent)
	}
	if req.form.Get("RANDOMCODE") != "1234" {
		t.Fatalf("表单 RANDOMCODE 期望 1234，实际 %q", req.form.Get("RANDOMCODE"))
	}
}

// TestDoGetSendsQueryAndUnifiedHeaders 覆盖 GET 查询串与 GET 也必须带统一请求头。
func TestDoGetSendsQueryAndUnifiedHeaders(t *testing.T) {
	type captured struct {
		method      string
		rawQuery    string
		contentType string
		userAgent   string
	}
	got := make(chan captured, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- captured{
			method:      r.Method,
			rawQuery:    r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
		}
		_, _ = io.WriteString(w, "<html>学期页</html>")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, WithMinInterval(0))
	query := url.Values{"xqid": {"1"}, "requestType": {"jxl"}}
	if _, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_processAjax", query); err != nil {
		t.Fatalf("GET 请求失败: %v", err)
	}

	req := <-got
	if req.method != http.MethodGet {
		t.Fatalf("方法期望 GET，实际 %q", req.method)
	}
	if req.rawQuery != "requestType=jxl&xqid=1" {
		t.Fatalf("查询串不符，实际 %q", req.rawQuery)
	}
	if req.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("GET 也必须带统一 Content-Type，实际 %q", req.contentType)
	}
	if req.userAgent != DefaultUserAgent {
		t.Fatalf("GET 也必须带统一 User-Agent，实际 %q", req.userAgent)
	}
}

// TestDoKeepsCookieInMemory 覆盖会话统一与隔离：
// 同一 Client 的后续请求自动带上前一次响应设置的 Cookie；新建 Client 不带旧会话。
func TestDoKeepsCookieInMemory(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := ""
		if cookie, err := r.Cookie("JSESSIONID"); err == nil {
			value = cookie.Value
		}
		mu.Lock()
		seen = append(seen, value)
		first := len(seen) == 1
		mu.Unlock()
		if first {
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session-1", Path: "/"})
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, WithMinInterval(0))
	for range 2 {
		if _, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query", nil); err != nil {
			t.Fatalf("请求失败: %v", err)
		}
	}

	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "" || got[1] != "session-1" {
		t.Fatalf("同一 Client 的 Cookie 传递期望 [\"\" \"session-1\"]，实际 %v", got)
	}

	// 另一个 Client 是另一个内存会话，不能复用前一个的 Cookie。
	other := newTestClient(t, srv.URL, WithMinInterval(0))
	if _, err := other.get(t.Context(), "/jsxsd/kbxx/jsjy_query", nil); err != nil {
		t.Fatalf("第二个 Client 请求失败: %v", err)
	}
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != "" {
		t.Fatalf("新 Client 不应带旧会话，实际 Cookie=%q", last)
	}
}

// TestDoPacesAdjacentRequests 覆盖 Requirement 11.5 的节奏下限：
// 同一 Client 连续三次请求，任意相邻两次的「发起」间隔都不小于配置的最小间隔。
// 时刻取自注入的假时钟、测量点在客户端发起请求处，不受调度与连接建立延迟影响。
func TestDoPacesAdjacentRequests(t *testing.T) {
	const interval = 40 * time.Millisecond

	clock := newFakeClock()
	var at []time.Time
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		at = append(at, clock.Now())
		return okResponse("ok"), nil
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(interval))
	c.clock = clock.Now
	c.sleep = clock.Sleep

	for range 3 {
		if _, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query", nil); err != nil {
			t.Fatalf("请求失败: %v", err)
		}
	}

	if len(at) != 3 {
		t.Fatalf("期望 3 次请求，实际 %d 次", len(at))
	}
	assertGapsAtLeast(t, at, interval)

	// 首次没有前序请求，后两次各等满一个间隔；等待时长必须落在 (0, interval]。
	slept := clock.sleeps()
	if len(slept) != 2 {
		t.Fatalf("期望 2 次节奏等待，实际 %d 次", len(slept))
	}
	for i, d := range slept {
		if d <= 0 || d > interval {
			t.Fatalf("第 %d 次等待 %v，应落在 (0, %v]", i+1, d, interval)
		}
	}
}

// TestDoPacesAdjacentRequestsInRealTime 守住默认真实等待：
// 默认 sleep 没有被替换时，两次相邻请求的总耗时不可能小于一个间隔（定时器不会提前触发）。
func TestDoPacesAdjacentRequestsInRealTime(t *testing.T) {
	const interval = 20 * time.Millisecond

	var calls atomic.Int64
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse("ok"), nil
	})
	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(interval))

	start := time.Now()
	for range 2 {
		if _, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query", nil); err != nil {
			t.Fatalf("请求失败: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed < interval {
		t.Fatalf("两次请求总耗时 %v，小于节奏下限 %v", elapsed, interval)
	}
	if calls.Load() != 2 {
		t.Fatalf("期望 2 次请求，实际 %d 次", calls.Load())
	}
}

// TestDoRetriesUntilSuccessAndPacesRetries 覆盖重试成功与「重试也要等间隔」：
// 前两次 503 计入重试，第三次 200；相邻尝试（含首发与重试）的发起间隔不小于下限。
func TestDoRetriesUntilSuccessAndPacesRetries(t *testing.T) {
	const interval = 10 * time.Millisecond

	clock := newFakeClock()
	var calls atomic.Int64
	var at []time.Time
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		at = append(at, clock.Now())
		if calls.Add(1) < 3 {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       http.NoBody,
			}, nil
		}
		return okResponse("矩阵页"), nil
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(interval))
	c.clock = clock.Now
	c.sleep = clock.Sleep

	resp, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if err != nil {
		t.Fatalf("第三次尝试应成功，实际 err=%v", err)
	}
	if string(resp.Body) != "矩阵页" {
		t.Fatalf("响应正文期望 矩阵页，实际 %q", resp.Body)
	}
	if calls.Load() != 3 {
		t.Fatalf("期望 3 次尝试，实际 %d 次", calls.Load())
	}

	if len(at) != 3 {
		t.Fatalf("期望记录 3 次发起，实际 %d 次", len(at))
	}
	assertGapsAtLeast(t, at, interval)
	if slept := clock.sleeps(); len(slept) != 2 {
		t.Fatalf("两次重试各等一个间隔，期望 2 次等待，实际 %d 次", len(slept))
	}
}

// TestDoQueuedWaitCancelable 覆盖并发排队时的取消：
// 第二个请求占着节奏闸门等待时，第三个请求在排队中被 ctx 取消，
// 必须立刻返回而不是一直卡到前者放行。
func TestDoQueuedWaitCancelable(t *testing.T) {
	const interval = 200 * time.Millisecond

	var calls atomic.Int64
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse("ok"), nil
	})
	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(interval))

	// 首个请求占住节奏窗口：之后 200ms 内的请求都要排队。
	if _, err := c.get(t.Context(), "/first", nil); err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}

	// 用可控等待把第二个请求稳定停在闸门内，此时闸门令牌一定不在。
	sleeping := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGate()
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		sleeping <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := c.get(t.Context(), "/second", nil)
		secondDone <- err
	}()
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("第二个请求未进入节奏等待")
	}

	// 第三个请求的 ctx 在排队时被取消：必须立刻返回 context.Canceled。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	thirdDone := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "/third", nil)
		thirdDone <- err
	}()
	// 留出第三个请求进入排队的时间，再取消它的 ctx。
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-thirdDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled，实际 err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("排队等待节奏的调用没有被 ctx 取消")
	}
	if calls.Load() != 1 {
		t.Fatalf("被取消的请求不应发出，实际已发 %d 次", calls.Load())
	}

	// 放行第二个请求并收尾，确认它仍能正常完成。
	releaseGate()
	if err := <-secondDone; err != nil {
		t.Fatalf("第二个请求失败: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("期望共 2 次请求，实际 %d 次", calls.Load())
	}
}

// TestDoExhaustsRetriesOnNon200 覆盖非 200 计入重试：
// 首发 + 3 次重试共 4 次后返回任务级 model.ErrUpstreamFailed。
func TestDoExhaustsRetriesOnNon200(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, WithMinInterval(0))
	_, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("期望任务级 ErrUpstreamFailed，实际 err=%v", err)
	}
	// 非 200 这一具体原因也要能被包内区分，便于上层映射错误码。
	if !errors.Is(err, errNon200) {
		t.Fatalf("期望错误可追踪到非 200，实际 err=%v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("默认 3 次重试应共 4 次尝试，实际 %d 次", calls.Load())
	}
}

// TestDoExhaustsRetriesOnNetworkError 覆盖网络错误计入重试与耗尽后的任务级失败。
func TestDoExhaustsRetriesOnNetworkError(t *testing.T) {
	networkErr := errors.New("模拟连接被拒绝")
	var calls atomic.Int64

	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, networkErr
	})}
	c, err := New("http://upstream.test", WithHTTPClient(httpClient), WithMinInterval(0))
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	_, err = c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("期望任务级 ErrUpstreamFailed，实际 err=%v", err)
	}
	if !errors.Is(err, networkErr) {
		t.Fatalf("期望错误可追踪到网络错误，实际 err=%v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("默认 3 次重试应共 4 次尝试，实际 %d 次", calls.Load())
	}
}

// TestDoExhaustsRetriesOnReadFailure 覆盖读取正文失败计入重试与耗尽后的任务级失败。
func TestDoExhaustsRetriesOnReadFailure(t *testing.T) {
	var calls atomic.Int64

	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(failingReader{}),
		}, nil
	})}
	c, err := New("http://upstream.test", WithHTTPClient(httpClient), WithMinInterval(0))
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	_, err = c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("期望任务级 ErrUpstreamFailed，实际 err=%v", err)
	}
	if !errors.Is(err, errReadBody) {
		t.Fatalf("期望错误可追踪到读取失败，实际 err=%v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("默认 3 次重试应共 4 次尝试，实际 %d 次", calls.Load())
	}
}

// TestDoRetriesOnAttemptTimeoutUntilSuccess 覆盖单次尝试超时计入重试：
// 前两次尝试卡住直到单次超时，第三次成功；超时只归入重试，不会把整个调用判成失败。
func TestDoRetriesOnAttemptTimeoutUntilSuccess(t *testing.T) {
	const attemptTimeout = 20 * time.Millisecond

	var calls atomic.Int64
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			// 一直不响应，直到本次尝试的 15s（测试缩短为 20ms）超时被截断。
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return okResponse("矩阵页"), nil
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(0))
	c.attemptTimeout = attemptTimeout

	resp, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if err != nil {
		t.Fatalf("超时两次后第三次应成功，实际 err=%v", err)
	}
	if string(resp.Body) != "矩阵页" {
		t.Fatalf("响应正文期望 矩阵页，实际 %q", resp.Body)
	}
	if calls.Load() != 3 {
		t.Fatalf("期望超时两次后成功，共 3 次尝试，实际 %d 次", calls.Load())
	}
}

// TestDoExhaustsRetriesOnAttemptTimeout 覆盖单次尝试超时耗尽重试：
// 4 次尝试全部超时后仍返回任务级 model.ErrUpstreamFailed，而不是 context 错误。
func TestDoExhaustsRetriesOnAttemptTimeout(t *testing.T) {
	const attemptTimeout = 10 * time.Millisecond

	var calls atomic.Int64
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(0))
	c.attemptTimeout = attemptTimeout

	_, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil)
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("期望任务级 ErrUpstreamFailed，实际 err=%v", err)
	}
	// 超时这一具体原因也要能被包内区分，便于上层定位是卡住而不是被拒绝。
	if !errors.Is(err, errAttemptTimeout) {
		t.Fatalf("期望错误可追踪到单次尝试超时，实际 err=%v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("默认 3 次重试应共 4 次尝试，实际 %d 次", calls.Load())
	}
}

// TestDoParentCancelIsNotRetried 覆盖调用方取消与单次尝试超时的区分：
// 父 ctx 在请求进行中被取消时立即返回 context.Canceled、不重试，也不会误报任务级失败。
func TestDoParentCancelIsNotRetried(t *testing.T) {
	started := make(chan struct{}, 1)
	var calls atomic.Int64
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		started <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(0))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, "/jsxsd/kbxx/jsjy_query2", nil)
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未开始")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled，实际 err=%v", err)
		}
		if errors.Is(err, model.ErrUpstreamFailed) {
			t.Fatalf("调用方取消不应混成任务级失败，实际 err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("父 ctx 取消后请求没有立即返回")
	}
	if calls.Load() != 1 {
		t.Fatalf("调用方取消不应重试，实际 %d 次尝试", calls.Load())
	}
}

// TestDoPacesRetriesAfterAttemptTimeout 守住「超时重试也走节奏」：
// 第一次尝试超时后，第二次尝试仍要先等满最小间隔才发起。
func TestDoPacesRetriesAfterAttemptTimeout(t *testing.T) {
	const (
		interval       = 30 * time.Millisecond
		attemptTimeout = 10 * time.Millisecond
	)

	clock := newFakeClock()
	var calls atomic.Int64
	var at []time.Time
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		at = append(at, clock.Now())
		if calls.Add(1) == 1 {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return okResponse("ok"), nil
	})

	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(interval))
	c.clock = clock.Now
	c.sleep = clock.Sleep
	c.attemptTimeout = attemptTimeout

	if _, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query2", nil); err != nil {
		t.Fatalf("超时后重试应成功，实际 err=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("期望 2 次尝试，实际 %d 次", calls.Load())
	}
	if len(at) != 2 {
		t.Fatalf("期望记录 2 次发起，实际 %d 次", len(at))
	}
	assertGapsAtLeast(t, at, interval)
	if slept := clock.sleeps(); len(slept) != 1 {
		t.Fatalf("超时后重试应等一个间隔，期望 1 次等待，实际 %d 次", len(slept))
	} else if slept[0] <= 0 || slept[0] > interval {
		t.Fatalf("等待 %v，应落在 (0, %v]", slept[0], interval)
	}
}

// TestDoSessionExpiredIsNotRetried 覆盖 Requirement 2.5 的会话失效处理：
// 200 正文含「用户登录」或「非法访问」时抛会话错误，且不重试、不混成数据错误。
func TestDoSessionExpiredIsNotRetried(t *testing.T) {
	for _, marker := range []string{sessionMarkerLogin, sessionMarkerForbidden} {
		t.Run(marker, func(t *testing.T) {
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, "<html><title>登录</title>"+marker+"</html>")
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL, WithMinInterval(0))
			resp, err := c.get(t.Context(), "/jsxsd/kbxx/jsjy_query", nil)
			if resp != nil {
				t.Fatalf("会话失效不应返回数据，实际 %+v", resp)
			}
			if !errors.Is(err, model.ErrUpstreamSessionExpired) {
				t.Fatalf("期望会话错误，实际 err=%v", err)
			}
			if errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("会话错误不应同时是任务级失败，实际 err=%v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("会话失效不应重试，实际 %d 次请求", calls.Load())
			}
		})
	}
}

// TestDoLoginFlowSkipsSessionMarker 覆盖登录流程例外：
// 登录页本身含「用户登录」，标记 loginFlow 的请求必须原样返回而不是报会话失效（供 3.2 登录使用）。
func TestDoLoginFlowSkipsSessionMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html><title>登录</title>用户登录</html>")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, WithMinInterval(0))
	resp, err := c.do(t.Context(), request{method: http.MethodGet, path: "/", loginFlow: true})
	if err != nil {
		t.Fatalf("登录流程不应被判成会话失效: %v", err)
	}
	if len(resp.Body) == 0 {
		t.Fatal("登录流程应返回登录页正文")
	}
}

// TestDoStopsPacingOnContextCancel 覆盖等待节奏时响应取消：
// 第二个请求在节奏闸门处被 context 取消，既不发请求也不吞掉 context 错误。
func TestDoStopsPacingOnContextCancel(t *testing.T) {
	const interval = 200 * time.Millisecond

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL, WithMinInterval(interval))
	if _, err := c.get(t.Context(), "/first", nil); err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	_, err := c.get(ctx, "/second", nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled，实际 err=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("取消后不应再发请求，实际 %d 次", calls.Load())
	}
	// 已取消的 context 必须立刻放弃等待，而不是先睡满一个间隔再失败。
	if elapsed >= interval/2 {
		t.Fatalf("取消后等待了 %v，应远小于间隔 %v", elapsed, interval)
	}
}

// TestWaitForStopsOnContextCancel 覆盖默认等待实现的 ctx 分支（3.1 遗留）：
// 已取消的 ctx 立即返回 context.Canceled；等待中取消则提前返回，不必等满时长。
func TestWaitForStopsOnContextCancel(t *testing.T) {
	const wait = 10 * time.Second

	t.Run("已取消的 ctx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		start := time.Now()
		err := waitFor(ctx, wait)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("期望 context.Canceled，实际 err=%v", err)
		}
		if elapsed := time.Since(start); elapsed >= wait/2 {
			t.Fatalf("已取消的 ctx 应立即返回，实际等了 %v", elapsed)
		}
	})

	t.Run("等待中取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- waitFor(ctx, wait) }()
		// 留出进入定时器等待的时间再取消，命中的是 ctx.Done() 而不是入口检查。
		time.Sleep(20 * time.Millisecond)
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("期望 context.Canceled，实际 err=%v", err)
			}
			if elapsed := time.Since(start); elapsed >= wait/2 {
				t.Fatalf("等待中取消应提前返回，实际等了 %v", elapsed)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("等待中取消后 waitFor 没有返回")
		}
	})
}

// TestWaitTurnPropagatesSleepError 覆盖 waitTurn 的 sleep 返回错误分支（3.1 遗留）：
// 节奏等待失败时原样交回该错误、不吞成上游失败、也不再发出请求；
// 同时确认失败路径会把节奏闸门令牌还回去，后续请求仍能继续。
func TestWaitTurnPropagatesSleepError(t *testing.T) {
	sleepErr := errors.New("模拟节奏等待被打断")
	var calls atomic.Int64
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse("ok"), nil
	})

	// 间隔取大值：第二次请求一定需要等待，不受调度延迟影响。
	c := newTestClient(t, "http://upstream.test",
		WithHTTPClient(&http.Client{Transport: transport}),
		WithMinInterval(time.Hour))

	if _, err := c.get(t.Context(), "/first", nil); err != nil {
		t.Fatalf("首个请求失败: %v", err)
	}

	c.sleep = func(context.Context, time.Duration) error { return sleepErr }
	_, err := c.get(t.Context(), "/second", nil)
	if !errors.Is(err, sleepErr) {
		t.Fatalf("等待失败应原样返回该错误，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("节奏等待失败不能混成任务级失败，实际 err=%v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("等待失败时不应发出第二个请求，实际已发 %d 次", calls.Load())
	}

	// 闸门令牌必须在失败路径上归还，否则后续请求会永远排队。
	// 给这一步加 deadline：令牌没归还时以超时暴露，而不是挂住整个测试进程。
	c.sleep = func(context.Context, time.Duration) error { return nil }
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := c.get(ctx, "/third", nil); err != nil {
		t.Fatalf("失败后应还能继续请求，实际 err=%v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("期望共 2 次请求（首个 + 恢复后的第三次），实际 %d 次", calls.Load())
	}
}

// roundTripFunc 让测试用函数替身替代真实网络传输。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// fakeClock 是测试用假时钟：Now 返回逻辑时刻，Sleep 直接把逻辑时刻推进 d 而不真实等待。
// 这样节奏断言只看客户端的发起序列，完全不受真实调度与网络握手延迟影响。
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.slept = append(f.slept, d)
	return nil
}

// sleeps 返回已请求过的等待时长副本。
func (f *fakeClock) sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

// okResponse 构造自定义 Transport 返回的 200 响应；正文由客户端读取后关闭。
func okResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// assertGapsAtLeast 断言相邻发起时刻的间隔不小于下限；零容差，不加补偿。
func assertGapsAtLeast(t *testing.T, at []time.Time, interval time.Duration) {
	t.Helper()
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < interval {
			t.Fatalf("第 %d 与第 %d 次发起间隔 %v，小于下限 %v", i, i+1, gap, interval)
		}
	}
}

// failingReader 模拟读取响应正文时出错。
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("模拟读取失败") }

func (failingReader) Close() error { return nil }
