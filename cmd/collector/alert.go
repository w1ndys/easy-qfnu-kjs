// 本文件是 entry 层：告警去重与告警正文。
// 同一轮同一类错误只通知一次，靠 alert_log 的键去重；正文只由非敏感字段拼成，
// 账号、Cookie、验证码与上游原始页面都不进入告警（设计文档 Error Handling）。

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// 告警场合：每个场合在一轮里各自只通知一次。
const (
	// alarmFinalFailure 是重试耗尽与整轮停止这类最终失败；键前缀沿用 alert_log 注释里的写法
	alarmFinalFailure = "retry_exhausted"
	// alarmValidation 是发布被拦下：校验不过或任务没跑完，原 current 不动
	alarmValidation = "validation_blocked"
	// alarmTermChanged 是学期切换：整张周历与观测都换到新学期
	alarmTermChanged = "term_changed"
	// alarmRecovered 是连续失败后的首次恢复
	alarmRecovered = "recovered"
)

// reasonLimit 是告警正文里原因的字符上限：只留能排查的一句话，不搬运上游正文。
const reasonLimit = 200

// alertKeyLayout 是去重键里的时刻格式，与 alert_log 注释里的例子一致（20261003T041012+0800）。
const alertKeyLayout = "20060102T150405-0700"

// AlertStore 是告警去重需要的数据能力，实现在 internal/store；测试注入内存实现。
type AlertStore interface {
	// RecordAlert 落一条告警并返回它是否首次出现
	RecordAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error)
}

// Notifier 是真正发出告警的一方。
// 本任务只落 alert_log 去重并写进程日志；飞书发送（settings 的 feishu_webhook_url 与
// feishu_secret）不在本次实现里，等面板阶段接上。
type Notifier interface {
	// Send 发出一条告警正文
	Send(ctx context.Context, text string) error
}

// Alerter 是告警层：先落库去重，只有首次出现才交给发送方。
type Alerter struct {
	store    AlertStore     // 落 alert_log 并判定首次
	notifier Notifier       // 发送方
	location *time.Location // 去重键里的时刻按它输出
}

// newAlerter 装配告警层。
func newAlerter(alertStore AlertStore, notifier Notifier, location *time.Location) *Alerter {
	return &Alerter{store: alertStore, notifier: notifier, location: location}
}

// Notify 发一条告警：键首次出现时通知一次，重复的只累加次数。
func (a *Alerter) Notify(ctx context.Context, occasion, term, reason string, at time.Time) error {
	key := dedupeKey(occasion, at, a.location)
	first, err := a.store.RecordAlert(ctx, key, at)
	if err != nil {
		return err
	}
	// 这个键已经通知过了：只留计数，不再发
	if !first {
		return nil
	}
	return a.notifier.Send(ctx, alarmText(occasion, term, reason, at.In(a.location)))
}

// dedupeKey 拼去重键：场合 + 轮次时刻，与 alert_log 注释里的例子同形。
func dedupeKey(occasion string, at time.Time, location *time.Location) string {
	return occasion + ":" + at.In(location).Format(alertKeyLayout)
}

// alarmText 拼告警正文：只放场合、时刻、学期与清洗过的原因。
func alarmText(occasion, term, reason string, at time.Time) string {
	text := fmt.Sprintf("[采集告警] %s %s", occasion, at.Format("2006-01-02 15:04:05"))
	// 学期只有读到时才拼进来：读学期之前就失败时没有学期可说
	if term != "" {
		text += " 学期 " + term
	}
	// 原因已经去掉正文与换行，这里只把它接在最后
	if cleaned := sanitizeReason(reason); cleaned != "" {
		text += " 原因: " + cleaned
	}
	return text
}

// sanitizeReason 把失败原因收成一句话：丢掉上游正文、只留第一行、按字符截断。
// 这样即使某个错误里裹了响应片段，告警里也带不出原始页面。
func sanitizeReason(reason string) string {
	// 第一个 '<' 之后可能是整段原始页面，一律不带走
	if index := strings.Index(reason, "<"); index >= 0 {
		reason = reason[:index]
	}
	// 换行会让日志与卡片排版乱掉，只取第一行
	if index := strings.IndexAny(reason, "\r\n"); index >= 0 {
		reason = reason[:index]
	}
	reason = strings.TrimSpace(reason)

	runes := []rune(reason)
	// 过长的原因没有排查价值，按字符截断，避免切坏中文
	if len(runes) > reasonLimit {
		return string(runes[:reasonLimit])
	}
	return reason
}

// logNotifier 把告警写进进程日志：面板与飞书落地前先保证告警可查。
type logNotifier struct {
	logger *slog.Logger // 进程日志
}

// Send 记一条 warn 日志。
func (n logNotifier) Send(ctx context.Context, text string) error {
	n.logger.WarnContext(ctx, "采集告警", "text", text)
	return nil
}
