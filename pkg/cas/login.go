package cas

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

const (
	baseURL        = "http://zhjw.qfnu.edu.cn"
	captchaURL     = baseURL + "/verifycode.servlet"
	loginSessURL   = baseURL + "/Logon.do?method=logon&flag=sess"
	loginURL       = baseURL + "/Logon.do?method=logonLdap"
	URLMainPage    = baseURL + "/jsxsd/framework/jsMain.jsp"
	URLSuccessMark = "教学一体化服务平台"

	maxCaptchaLoginAttempts = 3
)

// Login 执行完整的强智教务系统直登流程。
func (c *Client) Login(ctx context.Context, username, password string) error {
	c.username = username
	c.password = password

	ocrEndpoint, err := ocrEndpointFromEnv()
	if err != nil {
		MarkUpstreamUnhealthy(err.Error())
		return err
	}

	if err := c.initQiangzhiSession(ctx); err != nil {
		MarkUpstreamUnhealthy(fmt.Sprintf("初始化教务系统会话失败：%v", err))
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= maxCaptchaLoginAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			MarkUpstreamUnhealthy(fmt.Sprintf("登录流程已取消：%v", err))
			return err
		}

		logger.Info("正在尝试强智直登（验证码第 %d/%d 次）...", attempt, maxCaptchaLoginAttempts)
		err := c.loginWithCaptcha(ctx, username, password, ocrEndpoint)
		if err == nil {
			if verifyErr := c.verifyLogin(ctx); verifyErr != nil {
				MarkUpstreamUnhealthy(fmt.Sprintf("登录验证失败：%v", verifyErr))
				return verifyErr
			}
			MarkUpstreamHealthy()
			return nil
		}

		if errors.Is(err, errInvalidCredentials) {
			MarkUpstreamUnhealthy("账号或密码错误")
			return err
		}

		lastErr = err
		logger.Warn("强智直登失败（第 %d/%d 次）：%v", attempt, maxCaptchaLoginAttempts, err)
	}

	if lastErr == nil {
		lastErr = errors.New("强智直登失败")
	}
	MarkUpstreamUnhealthy(fmt.Sprintf("强智直登失败：%v", lastErr))
	return lastErr
}

var (
	errInvalidCredentials = errors.New("账号或密码错误")
	errCaptchaInvalid     = errors.New("验证码错误")
)

func (c *Client) initQiangzhiSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return fmt.Errorf("创建初始化请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("访问教务系统首页失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("教务系统首页状态异常: %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) loginWithCaptcha(ctx context.Context, username, password, ocrEndpoint string) error {
	captchaImage, err := c.fetchCaptcha(ctx)
	if err != nil {
		return err
	}

	captchaCode, err := solveCaptcha(ctx, c.httpClient, ocrEndpoint, captchaImage)
	if err != nil {
		return fmt.Errorf("OCR 识别验证码失败: %w", err)
	}

	scode, sxh, err := c.fetchLoginSession(ctx)
	if err != nil {
		return err
	}

	encoded, err := encodeCredentials(username, password, scode, sxh)
	if err != nil {
		return fmt.Errorf("生成登录凭证失败: %w", err)
	}

	return c.submitQiangzhiLogin(ctx, captchaCode, encoded)
}

func (c *Client) fetchCaptcha(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, captchaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建验证码请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取验证码失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("验证码接口状态异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取验证码图片失败: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("验证码图片为空")
	}
	return body, nil
}

func (c *Client) fetchLoginSession(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginSessURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("创建登录参数请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("获取登录参数失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("登录参数接口状态异常: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("读取登录参数失败: %w", err)
	}

	scode, sxh, err := parseLoginSession(string(body))
	if err != nil {
		return "", "", err
	}
	return scode, sxh, nil
}

func (c *Client) submitQiangzhiLogin(ctx context.Context, captchaCode, encoded string) error {
	form := url.Values{}
	form.Set("userAccount", "")
	form.Set("userPassword", "")
	form.Set("RANDOMCODE", captchaCode)
	form.Set("encoded", encoded)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("创建登录提交请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("提交登录失败: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取登录响应失败: %w", err)
	}
	body := string(bodyBytes)

	switch classifyLoginSubmitResponse(body) {
	case loginSubmitSuccess:
		return nil
	case loginSubmitPasswordError:
		return errInvalidCredentials
	case loginSubmitCaptchaError:
		return errCaptchaInvalid
	default:
		message := compactPageText(body)
		if message == "" {
			message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("登录失败：%s", message)
	}
}

func (c *Client) verifyLogin(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, URLMainPage, nil)
	if err != nil {
		return fmt.Errorf("创建主页验证请求失败: %w", err)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("访问主页失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("主页状态异常: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取主页响应失败: %w", err)
	}
	if !strings.Contains(string(bodyBytes), URLSuccessMark) {
		return errors.New("登录流程结束，但未检测到登录成功标识")
	}

	logger.Info("检测到登录成功标识，强智直登流程完成。")
	return nil
}
