// Package upstream 是教务系统的 HTTP 客户端骨架：只负责请求、内存会话、
// 统一请求头、请求节奏与重试，不做页面解析。
//
// Cookie 只保存在内存中的 http.CookieJar 里，不落盘、不写日志；同一 Client
// 的所有请求共用这一个会话。后续的登录与业务请求都在本包内基于 do 组装。
package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// DefaultUserAgent 是 docs/upstream-api.md 记录的实测可用 User-Agent。
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

	// DefaultMinInterval 是相邻上游请求的最小间隔：每秒至多 2 次（Requirement 11.5）。
	DefaultMinInterval = 500 * time.Millisecond

	// DefaultMaxRetries 是单次请求在首发之外允许的重试次数（Requirement 11.5）。
	DefaultMaxRetries = 3

	// DefaultAttemptTimeout 是单次尝试（含重试）的上游往返上限。
	// 超时与网络错误同类，计入重试；调用方 ctx 取消不重试。
	DefaultAttemptTimeout = 15 * time.Second

	// formContentType 是所有请求统一携带的 Content-Type；上游只接受表单编码。
	formContentType = "application/x-www-form-urlencoded"

	// sessionMarkerLogin 与 sessionMarkerForbidden 是正文里判定会话失效的标记。
	// 200 也可能是登录页，所以命中标记时必须报会话错误，不能交给解析器当数据。
	sessionMarkerLogin     = "用户登录"
	sessionMarkerForbidden = "非法访问"
)

// 内部错误原因：只在包内用于错误分类与测试断言，对外统一由 model 哨兵暴露。
var (
	errNon200         = errors.New("上游返回非 200")
	errReadBody       = errors.New("上游响应读取失败")
	errAttemptTimeout = errors.New("单次上游尝试超时")
)

// Response 是一次成功请求的结果：正文已完整读入内存，状态码必为 200。
type Response struct {
	StatusCode int    // HTTP 状态码，成功时恒为 200
	Body       []byte // 完整响应正文；登录页/图片等原样返回给上层判断
}

// Client 是单会话的上游客户端，一个实例对应一个教师账号的内存会话。
type Client struct {
	baseURL     *url.URL      // 上游根地址，可带路径前缀
	httpClient  *http.Client  // 底层客户端，CookieJar 永远指向内存 jar
	userAgent   string        // 统一 User-Agent
	minInterval time.Duration // 相邻请求最小间隔，0 表示不限速（仅测试使用）
	maxRetries  int           // 首发之外的重试次数
	// attemptTimeout 是单次尝试的上游往返上限，每次尝试单独派生超时 ctx。
	// 不用 http.Client.Timeout：那个值会把整个 Client 的生命周期与一次调用绑死。
	attemptTimeout time.Duration

	// paceGate 是容量 1 的节奏闸门：持令牌者才可进入等待与记账，
	// 排队者等令牌时可被 ctx 取消（不用 Mutex，避免锁不可取消）。
	paceGate chan struct{}
	// lastRequest 是上一次请求的发起时刻；零值表示本实例还没发过请求。
	lastRequest time.Time
	// clock 是时间来源，测试可注入假时钟。
	clock func() time.Time
	// sleep 是等待实现，测试可快进。
	sleep func(context.Context, time.Duration) error

	// ocrTimeout 是单次 OCR 识别往返的上限，与上游单次尝试超时分开配置。
	ocrTimeout time.Duration
	// ocrHTTPClient 只用于 OCR 服务：它没有 CookieJar，绝不把教务会话发给第三方服务。
	ocrHTTPClient *http.Client

	// loginMu 串行化登录：并发重登录会互相踢掉同一个教务会话（Requirement 2.6）。
	// credentials 也由它保护，只在内存里保存，不落盘、不写日志。
	loginMu     sync.Mutex
	credentials *LoginOptions
	// loginSeq 在每次登录成功后自增，用来判定「本次会话失效前是否已有别的调用完成过重登录」。
	loginSeq atomic.Int64
}

// Option 调整客户端参数；不传时按 Default* 常量工作。
type Option func(*Client)

// WithHTTPClient 替换底层 HTTP 客户端；传入 nil 时忽略。
// CookieJar 会在装配时统一替换成内存 jar，避免会话被外部实现带出进程。
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// WithUserAgent 替换统一 User-Agent；空串在 New 校验阶段被拒绝。
func WithUserAgent(userAgent string) Option {
	return func(c *Client) {
		c.userAgent = userAgent
	}
}

// WithMinInterval 调整相邻请求最小间隔；负值在 New 校验阶段被拒绝。
func WithMinInterval(interval time.Duration) Option {
	return func(c *Client) {
		c.minInterval = interval
	}
}

// WithMaxRetries 调整首发之外的重试次数；负值在 New 校验阶段被拒绝。
func WithMaxRetries(retries int) Option {
	return func(c *Client) {
		c.maxRetries = retries
	}
}

// New 校验根地址并装配客户端。
// 根地址必须是 http/https 且有主机；非法配置返回包装 model.ErrConfigInvalid 的错误，
// 由入口拒绝启动。
func New(baseURL string, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	// 缺协议头时 url.Parse 仍然成功，所以在这里显式要求 scheme 与 host 都存在。
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: UPSTREAM_BASE_URL %q 必须是 http/https 且带主机", model.ErrConfigInvalid, baseURL)
	}

	c := &Client{
		baseURL:        parsed,
		httpClient:     &http.Client{},
		userAgent:      DefaultUserAgent,
		minInterval:    DefaultMinInterval,
		maxRetries:     DefaultMaxRetries,
		attemptTimeout: DefaultAttemptTimeout,
		ocrTimeout:     DefaultOCRTimeout,
		ocrHTTPClient:  &http.Client{},
		paceGate:       make(chan struct{}, 1),
		clock:          time.Now,
		sleep:          waitFor,
	}
	for _, opt := range opts {
		opt(c)
	}

	if c.userAgent == "" {
		return nil, fmt.Errorf("%w: User-Agent 不能为空", model.ErrConfigInvalid)
	}
	if c.minInterval < 0 {
		return nil, fmt.Errorf("%w: 请求间隔不能为负", model.ErrConfigInvalid)
	}
	if c.maxRetries < 0 {
		return nil, fmt.Errorf("%w: 重试次数不能为负", model.ErrConfigInvalid)
	}
	if c.ocrTimeout <= 0 {
		return nil, fmt.Errorf("%w: OCR 超时必须为正", model.ErrConfigInvalid)
	}

	// 复制调用方给的客户端再挂 jar：既不修改外部实例，也保证会话只在本 Client 内。
	httpClient := *c.httpClient
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化内存 Cookie 失败: %w", model.ErrConfigInvalid, err)
	}
	httpClient.Jar = jar
	c.httpClient = &httpClient

	// 初始令牌让首个请求直接通过；之后每个请求进出闸门各取放一个令牌。
	c.paceGate <- struct{}{}

	return c, nil
}

// request 描述一次上游调用，由本包的登录与业务方法组装。
type request struct {
	method string     // HTTP 方法
	path   string     // 相对根地址的路径，必须以 / 开头
	query  url.Values // GET 查询参数；nil 表示不带查询串
	form   url.Values // POST 表单；nil 表示不带请求体
	// loginFlow 为真时跳过会话失效标记检测：登录流程本来就会拿到登录页。
	loginFlow bool
}

// get 发起一次受节奏与重试约束的 GET。
func (c *Client) get(ctx context.Context, path string, query url.Values) (*Response, error) {
	return c.do(ctx, request{method: http.MethodGet, path: path, query: query})
}

// postForm 发起一次受节奏与重试约束的表单 POST。
func (c *Client) postForm(ctx context.Context, path string, form url.Values) (*Response, error) {
	return c.do(ctx, request{method: http.MethodPost, path: path, form: form})
}

// do 执行一次请求骨架：节奏 → 请求 → 分类 →（必要时）重试。
// 非 200、网络错误、读取正文失败与单次尝试超时都计入重试；重试耗尽后返回任务级错误
// model.ErrUpstreamFailed。会话失效不重试，返回 model.ErrUpstreamSessionExpired；
// 调用方 ctx 取消或到期也不重试，立即返回 context 错误。
func (c *Client) do(ctx context.Context, r request) (*Response, error) {
	target, err := c.buildURL(r)
	if err != nil {
		// 路径为空或拼接失败是编程错误，重试不会变好。
		return nil, err
	}

	var payload []byte
	if r.form != nil {
		payload = []byte(r.form.Encode())
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		// 每次尝试（含首发）都先过节奏闸门，所以重试不会紧跟前一次请求连发。
		if err := c.waitTurn(ctx); err != nil {
			return nil, err
		}

		// 超时只约束这一次上游往返：父 ctx 仍是调用方的生命周期，
		// 不设 http.Client.Timeout，免得把整个 Client 绑死在一次调用上。
		attemptCtx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
		req, err := c.newRequest(attemptCtx, target, r.method, payload)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("构造上游请求失败: %w", err)
		}
		resp, err := c.attempt(r, req)
		attemptTimedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return resp, nil
		}

		// 父 ctx 先结束说明是调用方取消或调用方自己的截止时间到了：立即返回，不重试。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, model.ErrUpstreamSessionExpired) {
			// 会话失效要靠重新登录解决，拿同一会话重发只会再拿到登录页。
			return nil, err
		}
		// 单次尝试超时与网络错误同类，计入重试，同时保留超时原因便于定位。
		if attemptTimedOut {
			lastErr = fmt.Errorf("%w: %w", errAttemptTimeout, err)
			continue
		}
		lastErr = err
	}

	return nil, fmt.Errorf("%w: %s %s 重试 %d 次后仍失败: %w",
		model.ErrUpstreamFailed, r.method, r.path, c.maxRetries, lastErr)
}

// attempt 发出单次请求并读取正文，失败时只做分类，不做重试。
func (c *Client) attempt(r request, req *http.Request) (*Response, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游网络错误: %w", err)
	}
	// bodyclose：无论下面走哪条分支，响应体都由这里负责关闭。
	defer func() { _ = resp.Body.Close() }()

	// 非 200 不读正文：错误消息里也不带页面内容，避免上游错误页混进日志。
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: 状态码 %d", errNon200, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReadBody, err)
	}
	// 登录流程的登录页本身含「用户登录」，只对业务页做会话失效判定。
	if !r.loginFlow && isSessionExpired(body) {
		return nil, fmt.Errorf("%w: 响应正文含会话失效标记", model.ErrUpstreamSessionExpired)
	}

	return &Response{StatusCode: resp.StatusCode, Body: body}, nil
}

// waitTurn 保证同一 Client 相邻两次请求的发起时刻至少相隔 minInterval。
// 闸门让并发调用排队成合规序列，排队与等待都能被 ctx 取消。
func (c *Client) waitTurn(ctx context.Context) error {
	// 先看 ctx：已取消的调用不应再排进闸门队列。
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.paceGate:
	}
	defer func() { c.paceGate <- struct{}{} }()

	if !c.lastRequest.IsZero() {
		wait := c.minInterval - c.clock().Sub(c.lastRequest)
		if wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		}
	}
	// 记发起时刻而不是响应时刻：间隔按请求起点计算，响应耗时只会让实际间隔更长。
	c.lastRequest = c.clock()
	return nil
}

// waitFor 是默认的等待实现：靠真实定时器等到 d，ctx 取消时提前返回。
func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// buildURL 把相对路径与查询串拼到根地址上，保留根地址自带的路径前缀。
func (c *Client) buildURL(r request) (*url.URL, error) {
	if r.path == "" {
		return nil, errors.New("上游请求路径为空")
	}
	target := *c.baseURL
	target.Path = strings.TrimSuffix(c.baseURL.Path, "/") + "/" + strings.TrimPrefix(r.path, "/")
	target.RawQuery = r.query.Encode()
	return &target, nil
}

// newRequest 每次尝试都新建请求，避免重试复用已被关闭的请求体。
func (c *Client) newRequest(ctx context.Context, target *url.URL, method string, payload []byte) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Content-Type", formContentType)
	return req, nil
}

// isSessionExpired 判断正文是否含会话失效标记。
func isSessionExpired(body []byte) bool {
	return bytes.Contains(body, []byte(sessionMarkerLogin)) ||
		bytes.Contains(body, []byte(sessionMarkerForbidden))
}
