package store

// 本文件是采集器入口用到的 data 层接口的集成测试：设置读取、告警去重与字典版本读取。
// 需要真实 PostgreSQL（见 deploy/compose.yaml）；未设置 TEST_DATABASE_URL 时整组跳过。

import (
	"context"
	"testing"
	"time"
)

// TestSettingValueReadsCronExpr 断言业务设置按键读出，键不存在时 found 为 false。
func TestSettingValueReadsCronExpr(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()

	expr, found, err := s.SettingValue(ctx, SettingKeyCronExpr)
	if err != nil {
		t.Fatalf("读取 cron_expr 失败: %v", err)
	}
	// 种子数据里这条键必须有值，否则采集器没有可用的调度表达式
	if !found || expr == "" {
		t.Errorf("cron_expr = (%q, %v)，期望有值", expr, found)
	}

	// 没配过的键要能与「配成了空串」区分开
	if _, found, err := s.SettingValue(ctx, "不存在的键"); err != nil || found {
		t.Errorf("未知键返回 (found=%v, err=%v)，期望 (false, nil)", found, err)
	}
}

// TestRecordAlertDedupesKey 断言同一个去重键只在第一次返回 true，后续只累加次数。
func TestRecordAlertDedupesKey(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()

	// 键里带测试名与纳秒，避免与别的用例或历史数据互相干扰
	key := "it-alert:" + t.Name() + time.Now().Format("20060102150405.000000000")
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DELETE FROM alert_log WHERE dedupe_key = $1`, key); err != nil {
			t.Logf("清理 alert_log 失败: %v", err)
		}
	})

	first, err := s.RecordAlert(ctx, key, time.Now())
	if err != nil {
		t.Fatalf("第一次记录告警失败: %v", err)
	}
	if !first {
		t.Error("第一次记录告警返回 false，期望 true")
	}

	second, err := s.RecordAlert(ctx, key, time.Now())
	if err != nil {
		t.Fatalf("第二次记录告警失败: %v", err)
	}
	// 同一轮同类错误只通知一次：第二次不该再算首次
	if second {
		t.Error("第二次记录同一个键返回 true，期望 false")
	}

	var sendCount int
	if err := s.pool.QueryRow(ctx, `SELECT send_count FROM alert_log WHERE dedupe_key = $1`, key).Scan(&sendCount); err != nil {
		t.Fatalf("查询告警次数失败: %v", err)
	}
	if sendCount != 2 {
		t.Errorf("send_count = %d，期望 2", sendCount)
	}
}

// TestCatalogReadsLatestVersionsAndSymbols 断言字典与节次轴的最新版本可读，符号表按版本读出。
func TestCatalogReadsLatestVersionsAndSymbols(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()

	dictVersion, err := s.LatestDictVersion(ctx)
	if err != nil {
		t.Fatalf("读取最新字典版本失败: %v", err)
	}
	axisVersion, err := s.LatestAxisVersion(ctx)
	if err != nil {
		t.Fatalf("读取最新节次轴版本失败: %v", err)
	}
	// 种子数据里两版都存在，且版本号为正整数
	if dictVersion < 1 || axisVersion < 1 {
		t.Errorf("版本 = (%d, %d)，期望都大于 0", dictVersion, axisVersion)
	}

	symbols, err := s.DictSymbols(ctx, dictVersion)
	if err != nil {
		t.Fatalf("读取符号表失败: %v", err)
	}
	// 种子符号表里至少要有上下文用到的这几个符号，且码点原样
	found := map[string]bool{}
	for _, symbol := range symbols {
		found[symbol.Symbol] = true
	}
	for _, want := range []string{"◆", "空闲", "完全空闲"} {
		if !found[want] {
			t.Errorf("符号表里缺少 %q", want)
		}
	}

	// 没有这一版时返回空表而不是报错：调用方按空表判失败
	if empty, err := s.DictSymbols(ctx, dictVersion+1000); err != nil || len(empty) != 0 {
		t.Errorf("不存在的版本返回 (%d 个符号, %v)，期望 (0, nil)", len(empty), err)
	}
}
