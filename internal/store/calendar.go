// 本文件属于 data 层：写入学期周历。term_week 是查询侧解析周次的唯一来源，
// 所以这里保证「一次替换要么全成、要么一行都不动」：整个替换在一个事务里完成。

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrEmptyCalendar 表示要写入的周历是空的：调用方应当先确认周历采样解析成功。
var ErrEmptyCalendar = errors.New("周历为空，拒绝写入")

// ReplaceTermWeeks 把某学期的周历整体换成 weeks，并把该学期的总周数更新成行数。
// 学期以参数 term 为准，weeks 里只取周次、周一日期与教学周标记；整个过程在一个事务里，
// 中途失败会整体回滚，不会留下半截周历。
func (s *Store) ReplaceTermWeeks(ctx context.Context, term string, weeks []model.TermWeek) error {
	// 学期编号是 term_week 的外键，空值会造出一个没有名字的学期
	if strings.TrimSpace(term) == "" {
		return errors.New("学期编号为空，无法写入周历")
	}
	// 空周历说明采样或解析出了问题，此时替换等于把已有的好周历清空
	if len(weeks) == 0 {
		return ErrEmptyCalendar
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启周历写入事务失败: %w", err)
	}
	// 任意一步失败都整体回滚，避免留下半截周历
	defer func() { _ = tx.Rollback(ctx) }()

	// 总周数就是周历行数：采样得到「共 Y 周」后按 Y 行生成
	if _, err := tx.Exec(ctx, `
INSERT INTO term (term, total_weeks) VALUES ($1, $2)
ON CONFLICT (term) DO UPDATE SET total_weeks = EXCLUDED.total_weeks`,
		term, len(weeks)); err != nil {
		return fmt.Errorf("写入学期 %s 总周数失败: %w", term, err)
	}

	// 先清旧行再写新行：两次采样的周数可能不同，留着旧行会多出不在周历里的周
	if _, err := tx.Exec(ctx, `DELETE FROM term_week WHERE term = $1`, term); err != nil {
		return fmt.Errorf("清理学期 %s 旧周历失败: %w", term, err)
	}

	for _, week := range weeks {
		// 一周一行，周一日期与教学周标记由调用方算好
		if _, err := tx.Exec(ctx, `
INSERT INTO term_week (term, week, monday, in_calendar) VALUES ($1, $2, $3, $4)`,
			term, week.Week, week.Monday, week.InCalendar); err != nil {
			return fmt.Errorf("写入学期 %s 第 %d 周失败: %w", term, week.Week, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交周历写入事务失败: %w", err)
	}
	return nil
}
