// 本文件属于 data 层：采集器需要的房间所属楼写入与教学楼白名单读取。
// sync_task 与 occupancy_detail 的表结构在 migrations/0004_sync.sql 里，
// 写入它们的任务状态机属于后续任务，不在本文件。

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrRoomBuildingConflict 表示同一 jsbh 已经归属另一栋教学楼：本轮失败，不挑一栋覆盖。
var ErrRoomBuildingConflict = errors.New("房间已归属另一栋教学楼")

// Building 是教学楼白名单里的一条，字段名与 settings 里 JSON 元素的键一致。
type Building struct {
	Jxlbh string `json:"jxlbh"` // 教学楼编号，按楼请求时作为 jxlbh 参数
	Name  string `json:"name"`  // 教学楼展示名，只用于展示
}

// UpsertRoom 写入一条房间目录记录：新房间插入，已有房间更新房名与所属楼。
// 同一 jsbh 已归属另一栋楼时返回 ErrRoomBuildingConflict，由调用方把本轮判成失败，
// 不使用后一栋楼的展示名覆盖前一栋；不带楼编号的写入不动已有归属。
func (s *Store) UpsertRoom(ctx context.Context, room model.Room) error {
	const sqlText = `
INSERT INTO room (room_id, name, name_raw, building_id, building_name, first_seen, last_seen)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (room_id) DO UPDATE
   SET name          = EXCLUDED.name,
       name_raw      = EXCLUDED.name_raw,
       -- 这次没带楼信息时不覆盖已记下的归属，避免一次缺失把楼道清空
       building_id   = COALESCE(EXCLUDED.building_id, room.building_id),
       building_name = COALESCE(EXCLUDED.building_name, room.building_name),
       first_seen    = COALESCE(room.first_seen, EXCLUDED.first_seen),
       last_seen     = EXCLUDED.last_seen
 WHERE room.building_id IS NULL
    OR EXCLUDED.building_id IS NULL
    OR room.building_id = EXCLUDED.building_id
RETURNING room_id`

	var written string
	err := s.pool.QueryRow(ctx, sqlText,
		room.ID, room.Name, room.NameRaw,
		nullableText(room.BuildingID), nullableText(room.BuildingName),
		room.FirstSeen, room.LastSeen).Scan(&written)
	// 冲突分支里 WHERE 不成立时不会有行返回，这正说明该房间已属于另一栋楼
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrRoomBuildingConflict, room.ID)
	}
	if err != nil {
		return fmt.Errorf("写入房间 %s 失败: %w", room.ID, err)
	}
	return nil
}

// BuildingWhitelist 读出 settings.building_whitelist 里的教学楼白名单。
// 键不存在、空数组与非法 JSON 都返回空名单：调用方据此把本轮标成 blocked，
// 而不是退回「jxlbh 留空拉全校」。
func (s *Store) BuildingWhitelist(ctx context.Context) ([]Building, error) {
	const sqlText = `SELECT value FROM settings WHERE key = 'building_whitelist'`

	var raw string
	err := s.pool.QueryRow(ctx, sqlText).Scan(&raw)
	// 键不存在说明管理员还没配过白名单，与空名单同义
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取教学楼白名单失败: %w", err)
	}

	var entries []Building
	// 配置被写坏时按空名单处理，不让采集器带着未知参数去请求上游
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, nil
	}

	buildings := make([]Building, 0, len(entries))
	for _, entry := range entries {
		// 空 jxlbh 会被上游当成「不限楼」，等于拉全校，这类条目直接丢掉
		if strings.TrimSpace(entry.Jxlbh) == "" {
			continue
		}
		buildings = append(buildings, entry)
	}
	return buildings, nil
}

// nullableText 把空白字符串写成 NULL：空串与「没有这个值」在库里必须是同一件事，
// 否则空楼编号会被当成一栋真实的楼参与归属比较。
func nullableText(value string) any {
	// 空白值没有业务含义，一律落 NULL
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
