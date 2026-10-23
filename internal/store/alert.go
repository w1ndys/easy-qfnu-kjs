// 本文件属于 data 层：告警去重（alert_log）。
// 表结构在 migrations/0002_run_log.sql；去重键由入口按「场合 + 轮次时刻」拼好传进来，
// 本层只保证同一个键里的「首次」判定准确：同一项只该被通知一次。

package store

import (
	"context"
	"fmt"
	"time"
)

// RecordAlert 记一条告警，并返回它是不是首次出现。
// 首次出现（send_count 为 1）时返回 true，调用方据此只发一次；重复的键只累加次数并刷新时间。
func (s *Store) RecordAlert(ctx context.Context, dedupeKey string, at time.Time) (bool, error) {
	const sqlText = `
INSERT INTO alert_log (dedupe_key, last_sent_at, send_count)
VALUES ($1, $2, 1)
ON CONFLICT (dedupe_key) DO UPDATE
   SET last_sent_at = EXCLUDED.last_sent_at,
       send_count   = alert_log.send_count + 1
RETURNING send_count`

	var sendCount int
	if err := s.pool.QueryRow(ctx, sqlText, dedupeKey, at).Scan(&sendCount); err != nil {
		return false, fmt.Errorf("记录告警 %s 失败: %w", dedupeKey, err)
	}
	// 计数为 1 说明这个键是第一次落地，也就是第一次通知
	return sendCount == 1, nil
}
