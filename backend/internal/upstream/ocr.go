package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// DefaultOCRTimeout 是单次 OCR 识别往返的上限。
	// OCR 服务是外部依赖，卡住时必须让本轮失败，而不是把采集协程拖死。
	DefaultOCRTimeout = 10 * time.Second

	// ocrPath 是 ddddocr-fastapi 的识别端点，base URL 由调用方传入。
	ocrPath = "/ocr"

	// ocrSuccessCode 是 OCR 服务的业务成功码：HTTP 200 也可能是错误，必须再看 code。
	ocrSuccessCode = 200
)

// errOCR 是 OCR 失败的包内原因：请求失败、HTTP 非 200、响应不是约定 JSON、
// 业务 code 非 200，或识别文本为空。登录流程按累计尝试次数把它归并为
// model.ErrUpstreamOCRFailed（Requirement 2.3）。
var errOCR = errors.New("OCR 识别失败")

// WithOCRTimeout 调整单次 OCR 识别超时；非正值在 New 校验阶段被拒绝。
func WithOCRTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		c.ocrTimeout = timeout
	}
}

// ocrResult 是 ddddocr-fastapi 的响应体。
// Message 只用于错误消息；Data 是识别文本，不写日志、不进错误消息。
type ocrResult struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

// buildOCRURL 校验 OCR base URL 并拼出识别端点；保留 base 自带的路径前缀。
// 配置缺失或非法返回 model.ErrConfigInvalid，由调用方拒绝这次登录。
func buildOCRURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("%w: OCR 服务地址 %q 必须是 http/https 且带主机", model.ErrConfigInvalid, baseURL)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + ocrPath
	return parsed.String(), nil
}

// recognizeCaptcha 把验证码 JPEG 的 base64 交给 OCR 服务，返回识别文本。
// 请求按 ddddocr-fastapi 的约定带 image、probability=false、png_fix=false；
// 只有 HTTP 200 且业务 code=200 且识别文本非空才算成功（Requirement 2.2）。
// base64 不是图像解码：验证码字节原样编码，所以不需要 image/jpeg 的解码器。
func (c *Client) recognizeCaptcha(ctx context.Context, ocrURL string, image []byte) (string, error) {
	form := url.Values{
		"image":       {base64.StdEncoding.EncodeToString(image)},
		"probability": {"false"},
		"png_fix":     {"false"},
	}

	// 超时只约束这一次 OCR 往返；父 ctx 仍是调用方的生命周期。
	ocrCtx, cancel := context.WithTimeout(ctx, c.ocrTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ocrCtx, http.MethodPost, ocrURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: 构造请求失败: %w", errOCR, err)
	}
	req.Header.Set("Content-Type", formContentType)

	// 用不带 CookieJar 的独立客户端：教务会话绝不能发给 OCR 服务。
	resp, err := c.ocrHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: 请求失败: %w", errOCR, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP 状态码 %d", errOCR, resp.StatusCode)
	}

	var result ocrResult
	// OCR 服务可能返回很大的错误页；限制读取量，避免一条坏响应吃掉内存。
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("%w: 响应不是约定的 JSON: %w", errOCR, err)
	}
	if result.Code != ocrSuccessCode {
		return "", fmt.Errorf("%w: 业务 code=%d message=%q", errOCR, result.Code, result.Message)
	}
	text := strings.TrimSpace(result.Data)
	if text == "" {
		return "", fmt.Errorf("%w: 识别文本为空", errOCR)
	}
	return text, nil
}
