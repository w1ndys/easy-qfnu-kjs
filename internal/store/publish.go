// 本文件属于 data 层：把一版候选数据在一个事务里发布出去。
// 发布后只保留两份版本：新版本标 current，原 current 降为 previous，更早的版本连同它的观测
// 与明细一起删掉（观测与明细都靠外键级联）。校验由 internal/collect/publish 做，本层只负责
// 「一次事务要么全成、要么一行都不动」，失败时 current 保持原样。
// 依据 specs/collector-full-sync/requirements.md 的 6.1、6.2 与设计文档 Error Handling。

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrEmptyRelease 表示要发布的候选集没有观测行：空版本会让查询侧一无所有，直接拒绝。
var ErrEmptyRelease = errors.New("候选集没有观测，拒绝发布")

// observationColumns 是观测表的列顺序，必须与 insertObservations 给出的值一一对应。
var observationColumns = []string{
	"release_id", "room_id", "term", "week", "weekday", "node", "state_key", "available", "raw_text",
}

// PublishBatch 是一次发布要写进去的全部内容。
// Release 的 IsCurrent 由发布事务自己切换，调用方不用设置。
type PublishBatch struct {
	Release      model.Release           // 待发布版本：学期、发布时刻与两个版本号
	Observations []model.Observation     // 本轮全部观测事实
	Details      []model.OccupancyDetail // 本轮占用明细，含派生标记
}

// PublishRelease 在一个事务里换 current：写新版本、写观测、写占用明细，原 current 降为
// previous，更早的版本删除。任意一步失败整体回滚，current 保持原样（需求 6.1、6.2）。
func (s *Store) PublishRelease(ctx context.Context, batch PublishBatch) error {
	// 没有观测的版本会顶掉查询侧手里全部数据，这类发布不能放行
	if len(batch.Observations) == 0 {
		return ErrEmptyRelease
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启发布事务失败: %w", err)
	}
	// 任意一步失败都整体回滚，绝不留下半发布状态
	defer func() { _ = tx.Rollback(ctx) }()

	previousID, err := writeRelease(ctx, tx, batch.Release)
	if err != nil {
		return err
	}
	if err := insertObservations(ctx, tx, batch.Observations); err != nil {
		return err
	}
	// 明细的插入语句与单独写入共用同一句，避免两处写法漂移
	if err := insertOccupancyDetails(ctx, tx, batch.Release.ID, batch.Details); err != nil {
		return err
	}
	if err := deleteOlderReleases(ctx, tx, batch.Release.ID, previousID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交发布事务失败: %w", err)
	}
	return nil
}

// CurrentRoomCount 统计当前 current 版本里的房间总数，供发布后的核对使用。
// 还没有 current 时 found 为 false，这不是故障。
func (s *Store) CurrentRoomCount(ctx context.Context) (int, bool, error) {
	release, err := s.CurrentRelease(ctx)
	// 没有 current 就没有已发布房间，交给调用方按未发布处理
	if errors.Is(err, ErrNoRelease) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}

	var rooms int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(DISTINCT room_id) FROM observation WHERE release_id = $1`, release.ID).Scan(&rooms); err != nil {
		return 0, false, fmt.Errorf("统计版本 %s 的房间数失败: %w", release.ID, err)
	}
	return rooms, true, nil
}

// writeRelease 写入新版本并切换 current，返回被降下的版本 id（也就是新的 previous）。
// 顺序不能反：release_one_current 唯一索引不允许同一时刻出现两个 current，
// 所以先写入非 current 的新版本，再降原 current，最后抬起新版本。
func writeRelease(ctx context.Context, tx pgx.Tx, release model.Release) (string, error) {
	if _, err := tx.Exec(ctx, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, $4, $5, false)`,
		release.ID, release.Term, release.GeneratedAt, release.DictVersion, release.AxisVersion); err != nil {
		return "", fmt.Errorf("写入版本 %s 失败: %w", release.ID, err)
	}

	var previousID string
	err := tx.QueryRow(ctx,
		`UPDATE release SET is_current = false WHERE is_current RETURNING release_id`).Scan(&previousID)
	// 第一次发布时没有 current 可降，这不是错误
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("降下原 current 版本失败: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE release SET is_current = true WHERE release_id = $1`, release.ID); err != nil {
		return "", fmt.Errorf("标记版本 %s 为 current 失败: %w", release.ID, err)
	}
	return previousID, nil
}

// insertObservations 批量写入观测行。
// 一轮观测可达数百万行（一间房 × 一周 × 一天 × 一节一行），逐行 Exec 的开销不可接受，
// 所以走 COPY；取值逐行生成，不额外攒一份全量切片。
func insertObservations(ctx context.Context, tx pgx.Tx, observations []model.Observation) error {
	row := func(index int) ([]any, error) {
		observation := observations[index]
		return []any{observation.ReleaseID, observation.RoomID, observation.Term, observation.Week,
			observation.Weekday, observation.Node, string(observation.StateKey),
			observation.Available, nullableText(observation.RawText)}, nil
	}

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"observation"}, observationColumns,
		pgx.CopyFromSlice(len(observations), row)); err != nil {
		return fmt.Errorf("写入观测失败: %w", err)
	}
	return nil
}

// deleteOlderReleases 删掉 current 与 previous 之外的版本：观测与明细随外键级联删除。
// previousID 为空表示这次是第一版，此时除了新版本没有别的版本该留下。
func deleteOlderReleases(ctx context.Context, tx pgx.Tx, currentID, previousID string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM release WHERE release_id <> $1 AND release_id <> $2`, currentID, previousID); err != nil {
		return fmt.Errorf("清理更早的发布版本失败: %w", err)
	}
	return nil
}
