package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// rowQuerier 只取单行查询能力，便于把「按日期定位周」与驱动的其余部分解耦。
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TermWeekAt 查出覆盖给定业务日期的学期周；没有覆盖时 ok=false 而不是报错。
func TermWeekAt(ctx context.Context, q rowQuerier, date time.Time) (model.TermWeek, bool, error) {
	// 一周覆盖 [monday, monday+7)，用半开区间避免边界日重复命中。
	const sql = `SELECT term, week, monday, in_calendar
	             FROM term_week
	             WHERE $1::date >= monday AND $1::date < monday + 7
	             ORDER BY term DESC
	             LIMIT 1`

	var tw model.TermWeek
	err := q.QueryRow(ctx, sql, date).Scan(&tw.Term, &tw.Week, &tw.Monday, &tw.InCalendar)
	// 查不到属于正常业务情形（寒暑假、学期外的日期），不算错误。
	if errors.Is(err, pgx.ErrNoRows) {
		return model.TermWeek{}, false, nil
	}
	if err != nil {
		return model.TermWeek{}, false, fmt.Errorf("%w: 按日期定位学期周: %w", model.ErrQueryFailed, err)
	}
	return tw, true, nil
}
