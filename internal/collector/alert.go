package collector

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

// Alerter 飞书自定义机器人（加签模式）。WebhookURL 为空时禁用。
type Alerter struct {
	WebhookURL string
	Secret     string
	Client     *http.Client
}

func NewAlerter(webhookURL, secret string) *Alerter {
	return &Alerter{
		WebhookURL: webhookURL,
		Secret:     secret,
		Client:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (a *Alerter) enabled() bool { return a.WebhookURL != "" }

// feishuSign 按飞书加签规则：HMAC-SHA256(timestamp+"\n"+secret)，base64 URL-safe 编码。
func feishuSign(secret, ts string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	return base64.URLEncoding.EncodeToString(mac.Sum(nil))
}

type feishuPayload struct {
	Timestamp string `json:"timestamp"`
	Sign      string `json:"sign"`
	MsgType   string `json:"msg_type"`
	Content   struct {
		Text string `json:"text"`
	} `json:"content"`
}

type feishuResp struct {
	Code json.RawMessage `json:"code"`
	Msg  string          `json:"msg"`
}

// send 发送一条告警；最多尝试 3 次。
func (a *Alerter) send(ctx context.Context, text string) error {
	if !a.enabled() {
		return nil
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := feishuPayload{Timestamp: ts, Sign: feishuSign(a.Secret, ts), MsgType: "text"}
	payload.Content.Text = text
	body, err := json.Marshal(&payload)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			if !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
				return ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.WebhookURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.Client.Do(req)
		if err != nil {
			lastErr = err
			logger.Warn("飞书告警发送失败（第 %d/3 次）: %v", attempt, err)
			continue
		}
		var fr feishuResp
		decodeErr := json.NewDecoder(resp.Body).Decode(&fr)
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Warn("关闭飞书响应失败: %v", cerr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("飞书返回 HTTP %d", resp.StatusCode)
		} else if failed, label := feishuCodeFailed(fr.Code); decodeErr == nil && failed {
			lastErr = fmt.Errorf("飞书返回错误码 %s: %s", label, fr.Msg)
		} else {
			return nil
		}
		logger.Warn("飞书告警发送失败（第 %d/3 次）: %v", attempt, lastErr)
	}
	return fmt.Errorf("飞书告警 3 次尝试均失败: %v", lastErr)
}

func feishuCodeFailed(raw json.RawMessage) (bool, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, ""
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		if n == 0 {
			return false, ""
		}
		return true, strconv.Itoa(n)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" && s != "0" {
		return true, s
	}
	return false, ""
}

// Notifier 单轮告警去重（同类错误每轮只通知一次）。
type Notifier struct {
	Alerter *Alerter
	sent    map[string]bool
}

func NewNotifier(a *Alerter) *Notifier {
	return &Notifier{Alerter: a, sent: map[string]bool{}}
}

// Notify 发送（若配置了 Webhook）。同一 category 每轮只发送一次；内容不含敏感信息。
func (n *Notifier) Notify(ctx context.Context, category, text string) {
	if n == nil || n.Alerter == nil || !n.Alerter.enabled() {
		return
	}
	if n.sent[category] {
		return
	}
	n.sent[category] = true
	msg := "easy-qfnu-kjs 采集告警 [" + category + "] " + text
	if err := n.Alerter.send(ctx, msg); err != nil {
		logger.Error("飞书告警发送最终失败: %v", err)
	}
}
