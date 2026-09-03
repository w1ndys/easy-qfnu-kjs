package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

const (
	DefaultTimeout = 30 * time.Second
	// SessionExpiredMark 是检测 Session 失效的关键字
	SessionExpiredMark = "用户登录"
)

// Client 封装了 CAS 登录和后续请求的 HTTP 客户端
// 采用 Facade 模式隐藏复杂的登录细节
type Client struct {
	httpClient *http.Client
	options    *clientOptions

	// 互斥锁，用于并发重登录时的线程安全
	mu sync.Mutex
	// 保存账号密码，用于自动重登录
	username string
	password string
}

type clientOptions struct {
	timeout time.Duration
}

// ClientOption 定义配置选项函数类型 (Functional Options Pattern)
type ClientOption func(*clientOptions)

// WithTimeout 设置请求超时时间
func WithTimeout(d time.Duration) ClientOption {
	return func(o *clientOptions) {
		o.timeout = d
	}
}

// NewClient 创建一个新的 CAS 客户端
func NewClient(opts ...ClientOption) (*Client, error) {
	// 默认配置
	options := &clientOptions{
		timeout: DefaultTimeout,
	}

	for _, opt := range opts {
		opt(options)
	}

	// 初始化 CookieJar
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	// 配置 HTTP Transport
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}

	httpClient := &http.Client{
		Jar:       jar,
		Timeout:   options.timeout,
		Transport: transport,
	}

	return &Client{
		httpClient: httpClient,
		options:    options,
	}, nil
}

// GetClient 返回底层的 http.Client，用于已登录后的业务请求
func (c *Client) GetClient() *http.Client {
	return c.httpClient
}

// Do 发送 HTTP 请求 (代理方法)，增加了 Session 失效自动重试机制
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// 1. 如果请求有 Body，我们需要先缓存它，因为 Body 是 io.ReadCloser，读完就没了
	// 如果需要重试，我们必须能够重新读取 Body
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("读取请求体失败: %w", err)
		}
		// 恢复原请求的 Body，以便第一次发送
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// 2. 执行原始请求
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// 3. 检查响应是否包含 Session 失效的标识
	// 读取响应 Body
	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("读取响应体失败: %w", err)
	}
	resp.Body.Close() // 先关闭，后面我们会重新赋值一个新的 Body

	// 检查是否包含 "用户登录"
	// 注意：这里简单地检查字符串。如果页面结构复杂，可能需要更严谨的检查，但通常足够了
	respBodyStr := string(respBodyBytes)
	if strings.Contains(respBodyStr, SessionExpiredMark) {
		logger.Warn("检测到 Session 可能已失效（响应包含 '%s'），尝试自动重登录...", SessionExpiredMark)

		// 4. 尝试自动重登录
		if loginErr := c.retryWithReLogin(req.Context()); loginErr != nil {
			logger.Error("自动重登录失败: %v", loginErr)
			// 重登录失败，把刚才读取的 Body 恢复回去，返回原始的（失效的）响应给调用者
			// 这样调用者至少能看到是为什么失败（比如验证码拦截等）
			resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
			return resp, nil // 或者 return nil, loginErr ? 这里选择返回原响应更符合 HTTP 语义
		}

		logger.Info("自动重登录成功，正在重试请求...")

		// 5. 重登录成功，克隆并重试请求
		retryReq, err := c.cloneRequest(req, bodyBytes)
		if err != nil {
			return nil, fmt.Errorf("创建重试请求失败: %w", err)
		}

		// 执行重试请求
		// 注意：这里直接调用 c.httpClient.Do，避免递归调用 c.Do 导致死循环（虽然 Session 应该已经有效了）
		retryResp, err := c.httpClient.Do(retryReq)
		if err != nil {
			return nil, fmt.Errorf("重试请求失败: %w", err)
		}
		return retryResp, nil
	}

	// 6. 如果不需要重登录，恢复响应 Body 并返回
	resp.Body = io.NopCloser(bytes.NewReader(respBodyBytes))
	return resp, nil
}

// retryWithReLogin 尝试使用保存的凭据重新登录
// 这个方法是线程安全的
func (c *Client) retryWithReLogin(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 检查是否有凭据
	if c.username == "" || c.password == "" {
		return errors.New("无凭据，无法自动重登录")
	}

	// 调用 Login 进行重登录
	// 注意：Login 方法内部会更新 CookieJar
	// 我们传入 context，但要注意如果原请求的 context 快超时了，这里可能也会失败
	// 可以考虑创建一个新的 context 或者继承原 context
	if err := c.Login(ctx, c.username, c.password); err != nil {
		return err
	}

	return nil
}

// cloneRequest 克隆一个 HTTP 请求，用于重试
func (c *Client) cloneRequest(req *http.Request, bodyBytes []byte) (*http.Request, error) {
	// 创建新的 Request
	// Context 使用原请求的 Context
	var newBody io.Reader
	if bodyBytes != nil {
		newBody = bytes.NewReader(bodyBytes)
	}

	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), newBody)
	if err != nil {
		return nil, err
	}

	// 复制 Header
	for key, values := range req.Header {
		for _, value := range values {
			newReq.Header.Add(key, value)
		}
	}

	return newReq, nil
}
