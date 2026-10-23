package main

// 本文件是采集器入口的单元测试：假上游 + 内存数据层，不连数据库、不访问网络。
// 覆盖任务 8.1 的三项：固定顺序、定时与手动跑同一套任务、告警文本不含秘密；
// 另外覆盖整轮停止不发布与空白名单不发教室状态请求。

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/sync"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// TestStartRunsFixedStageOrder 断言一轮全量的顺序固定：登录、读学期、采样周历、各周矩阵、
// 聚合矩阵、明细、校验与发布（需求 5.4、5.5 与设计文档 Architecture）。
func TestStartRunsFixedStageOrder(t *testing.T) {
	rig := newTestRig(t)

	if err := rig.collector.Start(context.Background()); err != nil {
		t.Fatalf("跑一轮失败: %v", err)
	}

	// 登录只做一次：整轮复用同一个会话
	if rig.session.logins != 1 {
		t.Errorf("登录次数 = %d，期望 1", rig.session.logins)
	}

	expected := []string{"term", "calendar", "week:1", "week:2", "aggregate", "detail"}
	if got := callLabels(rig.upstream.calls); !sameLabels(got, expected) {
		t.Errorf("请求顺序 = %v，期望 %v", got, expected)
	}

	// 周历按采样日期写成第 1、2 周的周一：2026-10-30 是星期五，第 1 周周一是 2026-10-26
	if len(rig.data.terms) != 2 {
		t.Fatalf("写入 %d 周周历，期望 2 周", len(rig.data.terms))
	}
	firstMonday := rig.data.terms[0].Monday.Format("2006-01-02")
	if firstMonday != "2026-10-26" {
		t.Errorf("第 1 周周一 = %s，期望 2026-10-26", firstMonday)
	}
	if !rig.data.terms[0].InCalendar {
		t.Error("采样生成的周历行没有标成教学周")
	}

	// 房间带上返回它的那次请求的楼
	if room := rig.data.rooms["r-1"]; room.BuildingID != "jxlbh-1" || room.Name != "F126" {
		t.Errorf("房间 = %+v，期望属于 jxlbh-1 且房名规范化成 F126", room)
	}

	// 发布一次：两周 × 一间房 × 7 天 × 12 小节，再加一格明细
	if len(rig.data.batches) != 1 {
		t.Fatalf("发布 %d 次，期望 1 次", len(rig.data.batches))
	}
	batch := rig.data.batches[0]
	if got := len(batch.Observations); got != 2*7*12 {
		t.Errorf("观测 %d 行，期望 %d 行", got, 2*7*12)
	}
	if got := len(batch.Details); got != 1 {
		t.Errorf("明细 %d 行，期望 1 行", got)
	}
	if batch.Release.Term != testTerm || batch.Release.DictVersion != 1 || batch.Release.AxisVersion != 1 {
		t.Errorf("版本 = %+v，期望学期 %s 与字典、轴版本 1", batch.Release, testTerm)
	}
	if rig.data.runStatus != store.RunStatusSuccess {
		t.Errorf("轮次状态 = %s，期望 %s", rig.data.runStatus, store.RunStatusSuccess)
	}
	// 正常一轮不发告警
	if len(rig.alerts.texts) != 0 {
		t.Errorf("正常一轮发了 %d 条告警，期望 0 条: %v", len(rig.alerts.texts), rig.alerts.texts)
	}
}

// TestScheduledRunMatchesManualRun 断言到达 cron_expr 时跑的任务与手动开始相同（需求 5.4、5.5）。
func TestScheduledRunMatchesManualRun(t *testing.T) {
	manual := newTestRig(t)
	if err := manual.collector.Start(context.Background()); err != nil {
		t.Fatalf("手动开始失败: %v", err)
	}

	scheduled := newTestRig(t)
	fired := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 每秒触发一次：用例只等第一次触发，不真等几分钟
	schedule, err := newScheduler(ctx, "* * * * * *", time.UTC, func(runCtx context.Context) {
		select {
		case fired <- scheduled.collector.Start(runCtx):
		default:
			// 已经有结果了：后面的触发不再挤进通道
		}
	})
	if err != nil {
		t.Fatalf("装配定时器失败: %v", err)
	}
	schedule.Start()

	select {
	case err := <-fired:
		// 定时触发与手动开始调用同一个入口，结果也必须一样
		if err != nil {
			t.Fatalf("定时触发失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待定时触发超时")
	}
	<-schedule.Stop().Done()

	manualLabels := callLabels(manual.upstream.calls)
	scheduledLabels := callLabels(scheduled.upstream.calls)
	if !sameLabels(manualLabels, scheduledLabels) {
		t.Errorf("定时序列 = %v，期望与手动序列 %v 相同", scheduledLabels, manualLabels)
	}
	if scheduled.data.runStatus != store.RunStatusSuccess {
		t.Errorf("定时一轮的轮次状态 = %s，期望 %s", scheduled.data.runStatus, store.RunStatusSuccess)
	}
}

// TestSchedulerRejectsBadExpression 断言非法 cron 表达式在启动时就报错，不静默退化成不采。
func TestSchedulerRejectsBadExpression(t *testing.T) {
	schedule, err := newScheduler(context.Background(), "不是表达式", time.UTC, func(context.Context) {})
	// 表达式写坏时宁可拒绝启动，也不要假装采集器在工作
	if err == nil {
		schedule.Stop()
		t.Fatal("非法表达式返回了成功")
	}
}

// TestAlertTextCarriesNoSecrets 断言告警正文不含账号、Cookie、验证码与上游原始页面（任务 8.1）。
func TestAlertTextCarriesNoSecrets(t *testing.T) {
	rig := newTestRig(t)
	// 学期页给出带敏感字样的正文，且没有学期编号：本轮在登录之后立刻失败
	rig.upstream.respond = func(call upstreamCall) (int, string) {
		if call.path == fetch.TermPath {
			return http.StatusOK, `<html>帐号 teacher@example.com 验证码 4321</html>`
		}
		return http.StatusOK, ""
	}

	if err := rig.collector.Start(context.Background()); err == nil {
		t.Fatal("读不到学期编号时返回了成功")
	}

	if len(rig.alerts.texts) != 1 {
		t.Fatalf("发出 %d 条告警，期望 1 条: %v", len(rig.alerts.texts), rig.alerts.texts)
	}
	text := rig.alerts.texts[0]
	if !strings.Contains(text, alarmFinalFailure) {
		t.Errorf("告警正文 = %q，期望带上场合 %s", text, alarmFinalFailure)
	}
	// 账号、验证码、Cookie 与原始页面都不得出现在告警里
	for _, secret := range []string{"<", "帐号", "验证码", "example.com", "4321", testSecretCookie} {
		if strings.Contains(text, secret) {
			t.Errorf("告警正文 %q 里出现了不该有的内容 %q", text, secret)
		}
	}

	// 同一轮同一场合只通知一次：时间没变时第二次只累加计数
	if err := rig.collector.Start(context.Background()); err == nil {
		t.Fatal("第二次仍然读不到学期编号，却返回了成功")
	}
	if len(rig.alerts.texts) != 1 {
		t.Errorf("同一轮去重后发了 %d 条告警，期望仍是 1 条", len(rig.alerts.texts))
	}
	if len(rig.data.alerts) != 1 {
		t.Errorf("alert_log 里有 %d 个键，期望 1 个", len(rig.data.alerts))
	}
}

// TestSanitizeReasonDropsPageContent 断言原因清洗：丢掉原始页面、只留第一行、按字符截断。
func TestSanitizeReasonDropsPageContent(t *testing.T) {
	cases := []struct {
		name   string // 用例说明
		reason string // 原始原因
		want   string // 清洗后的原因
	}{
		{"丢掉正文", "解析失败 <html><body>整页</body></html>", "解析失败"},
		{"只留第一行", "上游返回 500\n第二行", "上游返回 500"},
		{"去掉首尾空白", "  失败  ", "失败"},
		{"超长截断", strings.Repeat("长", reasonLimit+50), strings.Repeat("长", reasonLimit)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := sanitizeReason(testCase.reason)
			if got != testCase.want {
				t.Errorf("清洗结果 = %q，期望 %q", got, testCase.want)
			}
		})
	}
}

// TestStopErrorHaltsBeforeAggregateAndPublish 断言表头块集合变化时整轮停止：
// 不再取聚合矩阵与明细，也不发布，原 current 保持不动（设计文档 Error Handling）。
func TestStopErrorHaltsBeforeAggregateAndPublish(t *testing.T) {
	rig := newTestRig(t)
	// 发布过一版，用来确认整轮停止不会把它换掉
	rig.data.release = releaseOf("20261030090000.000000", "2025-2026-3")
	rig.data.hasRelease = true
	// 表头只有 4 个大节：解析前提没了
	rig.upstream.respond = func(call upstreamCall) (int, string) {
		switch call.path {
		case fetch.TermPath:
			return http.StatusOK, testTermBody
		case fetch.CalendarPath:
			return http.StatusOK, testCalendarBody
		case fetch.WeekMatrixPath:
			return http.StatusOK, buildMatrixBodyWithBlocks(testBlocks[:4])
		}
		return http.StatusOK, ""
	}

	err := rig.collector.Start(context.Background())
	if err == nil {
		t.Fatal("表头块变化时返回了成功")
	}

	if got := callLabels(rig.upstream.calls); !sameLabels(got, []string{"term", "calendar", "week:1"}) {
		t.Errorf("请求序列 = %v，期望只到第一个周矩阵为止", got)
	}
	if len(rig.data.batches) != 0 {
		t.Errorf("整轮停止后发布了 %d 次，期望 0 次", len(rig.data.batches))
	}
	if rig.data.release.Term != "2025-2026-3" {
		t.Errorf("current 学期 = %s，期望保持 2025-2026-3", rig.data.release.Term)
	}
	if rig.data.runStatus != store.RunStatusFailed {
		t.Errorf("轮次状态 = %s，期望 %s", rig.data.runStatus, store.RunStatusFailed)
	}
	if len(rig.alerts.texts) != 1 || !strings.Contains(rig.alerts.texts[0], alarmFinalFailure) {
		t.Errorf("告警 = %v，期望一条最终失败告警", rig.alerts.texts)
	}
}

// TestEmptyWhitelistSendsNoClassroomRequest 断言白名单为空时标 blocked、
// 不发教室状态请求，也不退回全校请求（需求 2.3）。
func TestEmptyWhitelistSendsNoClassroomRequest(t *testing.T) {
	rig := newTestRig(t)
	rig.data.buildings = nil

	err := rig.collector.Start(context.Background())
	if !errors.Is(err, sync.ErrEmptyWhitelist) {
		t.Fatalf("返回 %v，期望 ErrEmptyWhitelist", err)
	}

	if got := callLabels(rig.upstream.calls); !sameLabels(got, []string{"term", "calendar"}) {
		t.Errorf("请求序列 = %v，期望只有读学期与采样周历", got)
	}
	if rig.data.runStatus != store.RunStatusBlocked {
		t.Errorf("轮次状态 = %s，期望 %s", rig.data.runStatus, store.RunStatusBlocked)
	}
	// 空白名单是配置问题，不是需要重复通知的失败
	if len(rig.alerts.texts) != 0 {
		t.Errorf("空白名单发了 %d 条告警，期望 0 条", len(rig.alerts.texts))
	}
}

// releaseOf 造一个用于断言的已发布版本。
func releaseOf(releaseID, term string) model.Release {
	return model.Release{ID: releaseID, Term: term, IsCurrent: true}
}
