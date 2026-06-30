package cas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

func ocrEndpointFromEnv() (string, error) {
	base := strings.TrimSpace(os.Getenv("OCR_URL"))
	if base == "" {
		return "", errors.New("未配置 OCR_URL，无法自动识别验证码")
	}
	return strings.TrimRight(base, "/") + "/ocr", nil
}

type ocrAPIResponse struct {
	Code    any    `json:"code"`
	Data    string `json:"data"`
	Result  string `json:"result"`
	Message string `json:"message"`
}

func parseOCRResponse(body []byte) (string, error) {
	var result ocrAPIResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析 OCR 响应失败: %w", err)
	}

	if result.Code != nil {
		ok, err := isSuccessfulOCRCode(result.Code)
		if err != nil {
			return "", err
		}
		if !ok {
			msg := strings.TrimSpace(result.Message)
			if msg == "" {
				msg = fmt.Sprintf("OCR 返回失败 code=%v", result.Code)
			}
			return "", errors.New(msg)
		}
	}

	text := strings.TrimSpace(result.Data)
	if text == "" {
		text = strings.TrimSpace(result.Result)
	}
	if text == "" {
		return "", errors.New("OCR 识别结果为空")
	}
	return text, nil
}

func isSuccessfulOCRCode(code any) (bool, error) {
	switch v := code.(type) {
	case float64:
		return v == 200 || v == 0, nil
	case string:
		trimmed := strings.TrimSpace(v)
		return trimmed == "200" || trimmed == "0", nil
	default:
		return false, fmt.Errorf("OCR 响应 code 类型异常: %T", code)
	}
}

func solveCaptcha(ctx context.Context, client *http.Client, endpoint string, image []byte) (string, error) {
	if len(image) == 0 {
		return "", errors.New("验证码图片为空")
	}

	form := url.Values{}
	form.Set("image", base64.StdEncoding.EncodeToString(image))
	form.Set("probability", "false")
	form.Set("png_fix", "false")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("创建 OCR 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("调用 OCR 服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", fmt.Errorf("读取 OCR 响应失败: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("OCR 服务状态异常: %d", resp.StatusCode)
	}

	return parseOCRResponse(body)
}
