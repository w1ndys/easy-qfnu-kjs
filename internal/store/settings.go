// 本文件属于 data 层：读取 WebUI 管理面板写进 settings 的业务设置。
// 采集器只读它需要的键（调度表达式），不在这里写设置，也不读秘密再转手。

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SettingKeyCronExpr 是采集调度表达式的键：六段式含秒的 cron，种子值与面板写法一致。
const SettingKeyCronExpr = "cron_expr"

// SettingValue 读一条业务设置。
// 键不存在时 found 为 false：调用方据此拒绝启动或退回默认值，不把空串当成配置。
func (s *Store) SettingValue(ctx context.Context, key string) (string, bool, error) {
	const sqlText = `SELECT value FROM settings WHERE key = $1`

	var value string
	err := s.pool.QueryRow(ctx, sqlText, key).Scan(&value)
	// 键不存在说明管理员还没配过这条设置，与「配成了空串」不是一回事
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取设置 %s 失败: %w", key, err)
	}
	return value, true, nil
}
