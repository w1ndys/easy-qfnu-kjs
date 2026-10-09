// 本文件属于 data 层：读取发布版本与采集运行状态，供查询与面板使用。

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrNoRelease 表示库里还没有对应的发布版本（例如一次都没采集成功过）。
var ErrNoRelease = errors.New("没有可用 release")

// CurrentRelease 读当前发布版本。
func (s *Store) CurrentRelease(ctx context.Context) (model.Release, error) {
	return s.releaseByCurrentFlag(ctx, true)
}

// PreviousRelease 读上一版发布；它用于版本对比与回退目标。
func (s *Store) PreviousRelease(ctx context.Context) (model.Release, error) {
	return s.releaseByCurrentFlag(ctx, false)
}

// releaseByCurrentFlag 按 is_current 标记取一条发布版本。
func (s *Store) releaseByCurrentFlag(ctx context.Context, current bool) (model.Release, error) {
	const sqlText = `
SELECT release_id, term, generated_at, dict_version, axis_version, is_current
  FROM release
 WHERE is_current = $1
 ORDER BY generated_at DESC
 LIMIT 1`

	var release model.Release
	err := s.pool.QueryRow(ctx, sqlText, current).
		Scan(&release.ID, &release.Term, &release.GeneratedAt, &release.DictVersion, &release.AxisVersion, &release.IsCurrent)
	// 没有行说明这一版还不存在，属于正常情况而不是故障
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Release{}, ErrNoRelease
	}
	if err != nil {
		return model.Release{}, fmt.Errorf("读取 release 失败: %w", err)
	}
	return release, nil
}

// WeekPublished 判断某个版本是否发布过某一周的观测。
func (s *Store) WeekPublished(ctx context.Context, releaseID, term string, week int) (bool, error) {
	const sqlText = `
SELECT EXISTS (
  SELECT 1 FROM observation
   WHERE release_id = $1 AND term = $2 AND week = $3
)`

	var exists bool
	if err := s.pool.QueryRow(ctx, sqlText, releaseID, term, week).Scan(&exists); err != nil {
		return false, fmt.Errorf("查询周发布状态失败: %w", err)
	}
	return exists, nil
}

// PublishedWeeks 返回某个版本里实际有观测数据的全部 (学期, 周次)。
func (s *Store) PublishedWeeks(ctx context.Context, releaseID string) ([]model.TermWeek, error) {
	const sqlText = `
SELECT DISTINCT term, week
  FROM observation
 WHERE release_id = $1
 ORDER BY term, week`

	rows, err := s.pool.Query(ctx, sqlText, releaseID)
	if err != nil {
		return nil, fmt.Errorf("查询已发布周次失败: %w", err)
	}
	defer rows.Close()

	published := make([]model.TermWeek, 0, 32)
	for rows.Next() {
		var week model.TermWeek
		if err := rows.Scan(&week.Term, &week.Week); err != nil {
			return nil, fmt.Errorf("扫描已发布周次失败: %w", err)
		}
		published = append(published, week)
	}
	// 遍历中途出错时不能当作读完，否则会漏掉周次
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历已发布周次失败: %w", err)
	}
	return published, nil
}

// LatestRunStatus 读最近一轮采集的运行状态；还没有任何轮次时 found 为 false。
func (s *Store) LatestRunStatus(ctx context.Context) (string, bool, error) {
	const sqlText = `SELECT status FROM collect_run ORDER BY started_at DESC LIMIT 1`

	var status string
	err := s.pool.QueryRow(ctx, sqlText).Scan(&status)
	// 还没有采集过就没有状态可读，交给调用方按“未知”处理
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取采集运行状态失败: %w", err)
	}
	return status, true, nil
}
