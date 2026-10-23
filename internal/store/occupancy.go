// 本文件属于 data 层：写入占用明细。
// 表结构在 migrations/0004_sync.sql，解析与派生在 internal/collect/clean。
// 只写教室状态、课程、周次范围、时间标志与派生标记：申请人、任课教师与原始 HTML 没有可落的列。

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// occupancyDetailInsert 是占用明细的插入语句：发布事务与单独写入共用同一句，避免两处写法漂移。
const occupancyDetailInsert = `
INSERT INTO occupancy_detail (release_id, room_id, weekday, block, state_key, course, week_range, time_flag, derived)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// WriteOccupancyDetails 把一格的占用明细写进 occupancy_detail，全部行在同一个事务里。
// 空列表是正常情况（这一轮没有要下钻的格子），不必开事务。
func (s *Store) WriteOccupancyDetails(ctx context.Context, releaseID string, details []model.OccupancyDetail) error {
	// 没有明细时不写任何行，也不开一个什么都不写的事务
	if len(details) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启明细写入事务失败: %w", err)
	}
	// 任意一行失败都整体回滚，避免留下半截明细
	defer func() { _ = tx.Rollback(ctx) }()

	if err := insertOccupancyDetails(ctx, tx, releaseID, details); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交明细写入事务失败: %w", err)
	}
	return nil
}

// insertOccupancyDetails 在一个已有事务里逐行写入明细，供发布与单独写入共用。
func insertOccupancyDetails(ctx context.Context, tx pgx.Tx, releaseID string, details []model.OccupancyDetail) error {
	for _, detail := range details {
		// 课程、周次范围与时间标志没有值时落 NULL：空串与「没有这个值」在库里必须是同一件事
		if _, err := tx.Exec(ctx, occupancyDetailInsert, releaseID, detail.RoomID, detail.Weekday,
			detail.Block, string(detail.StateKey), nullableText(detail.Course),
			nullableText(detail.WeekRange), nullableText(detail.TimeFlag), detail.Derived); err != nil {
			return fmt.Errorf("写入房间 %s 的占用明细失败: %w", detail.RoomID, err)
		}
	}
	return nil
}
