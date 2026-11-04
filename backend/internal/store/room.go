package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// AttributedRoom 是一间已裁决归属的教室，写 room 目录所需的最小字段：
// 房号是身份，展示名由 clean.RoomName 生成，原始名原样保留（原 spec Requirement 5.2）。
type AttributedRoom struct {
	RoomID  string // 房号 jsbh
	Name    string // 展示名（clean.RoomName）；为空时落库回退 NameRaw、再回退 RoomID 写 room.name
	NameRaw string // 原始房名，写 room.name_raw
}

// RoomAttribution 是一栋楼一份响应里归属该楼的房号集合。
// 调用方必须先按「白名单顺序 + 周次升序」完成跨楼裁决，再把它交进来：
// 这里只写 room 目录，不做冲突判断，也不接收被丢弃的房号。
type RoomAttribution struct {
	BuildingID string           // 归属的教学楼编号
	Rooms      []AttributedRoom // 已裁决归本楼的房号；同批重复房号按契约静默去重（保留首次）
}

// RunRoom 是 collect_run_room 的一行：一轮内某房号的有效归属与冲突标记。
// 它是续跑重建累计器的唯一依据，也是发布阶段组装版本归属快照的来源；
// 只记录裁决后的有效归属，被丢弃的楼不在这里留行。
type RunRoom struct {
	RoomID             string // 房号 jsbh
	BuildingID         string // 本轮有效归属的楼
	Conflict           bool   // 本轮是否有别的楼也报过这个房号
	ConflictBuildingID string // 第一次被丢弃的楼；没有冲突时为空
}

// roomQuerier 只取归属落库与续跑读回需要的两种能力，便于用替身测试。
type roomQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// upsertRoomAttributionSQL 把一栋楼的房号批量写进 room 目录。
//
// 幂等的关键是 ON CONFLICT (room_id) DO UPDATE：同一轮重放、续跑重做或
// 同一房号在另一周再次报出时，覆盖同名同归属而不是新增一行。
//
// building_id 允许被新轮覆盖（用户已拍板）：同一房号在不同轮里换楼是正常数据，
// 这一列跟着本轮归属走，DO UPDATE 里没有 WHERE 守卫，换楼不报错也不改判。
// 轮内「首个命中」不靠这一列：collect_run_room 的 ON CONFLICT 有意不改 building_id，
// 加上 pipeline.Attribution 的累计器，一轮里先归属的楼不会被后到的楼抢走。
//
// 整批是一条 INSERT ... SELECT：PostgreSQL 里单条语句自成一个隐式事务，任一行
// 失败整批都不落库。所以这里不存在「部分房号已写入、函数却报错」的中间态，
// 也不需要再包一层显式事务。
//
// building_name、first_seen、last_seen 不由本链路维护：版本内的楼名由发布阶段
// 写 release_room_building，查询侧读的也是那份；本链路只保证 room 目录的
// 房号身份、展示名与归属楼，不碰其余展示列。
const upsertRoomAttributionSQL = `
INSERT INTO room (room_id, name, name_raw, building_id)
SELECT t.room_id, t.name, t.name_raw, $1
  FROM unnest($2::text[], $3::text[], $4::text[]) AS t(room_id, name, name_raw)
ON CONFLICT (room_id) DO UPDATE
   SET name = EXCLUDED.name,
       name_raw = EXCLUDED.name_raw,
       building_id = EXCLUDED.building_id`

// recordAttrConflictsSQL 把本轮归属冲突数回写轮次。
// 用 GREATEST 而不是 +1/赋值：同一份响应重放、续跑重做都会拿同一个累计值再写一次，
// 只有数值变大时才变化，天然幂等；「一轮只记一次」由 pipeline 按房号去重保证，
// 这里只负责把已知的最大值落库。
// 轮次不存在时影响 0 行，调用方据此报可读错误。
const recordAttrConflictsSQL = `
UPDATE collect_run
   SET attr_conflict_count = GREATEST(attr_conflict_count, $2),
       updated_at = $3
 WHERE run_id = $1`

// upsertRunRoomsSQL 把一轮的归属快照增量写进 collect_run_room。
//
// 幂等靠 ON CONFLICT (run_id, room_id)：重放、续跑重做都只更新冲突标记，不改已定归属。
// building_id 有意不在 DO UPDATE 里改写：一轮内房号一旦归属某楼就不该换楼，
// 续跑时从这张表重建累计器也依赖这一点。
// term 直接从 collect_run 取，与 collect_run_week 的口径一致；轮次不存在或缺学期时
// 影响 0 行，调用方据此报可读错误。
// conflict 用 OR 保留历史标记；conflict_building_id 用 COALESCE 保留第一次被丢弃的楼。
const upsertRunRoomsSQL = `
INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id, created_at)
SELECT $1, r.term, t.room_id, t.building_id, t.conflict, NULLIF(t.conflict_building_id, ''), $6
  FROM collect_run r
 CROSS JOIN unnest($2::text[], $3::text[], $4::boolean[], $5::text[])
       AS t(room_id, building_id, conflict, conflict_building_id)
 WHERE r.run_id = $1
   AND r.term IS NOT NULL
ON CONFLICT (run_id, room_id) DO UPDATE
   SET conflict = collect_run_room.conflict OR EXCLUDED.conflict,
       conflict_building_id = COALESCE(collect_run_room.conflict_building_id, EXCLUDED.conflict_building_id)`

// selectRunRoomsSQL 读回一轮的归属快照。按 room_id 排序保证测试与发布阶段拿到稳定顺序。
const selectRunRoomsSQL = `
SELECT room_id, building_id, conflict, COALESCE(conflict_building_id, '')
  FROM collect_run_room
 WHERE run_id = $1
 ORDER BY room_id`

// UpsertRoomAttribution 写入一栋楼本轮归属的房号（幂等 upsert）。
//
// 房号与展示名的映射由调用方做好：房号为空、楼编号为空都直接报错，
// 不把坏数据写进 room；展示名为空时按契约回退 NameRaw、再回退 RoomID，不写空串。
// 同批内重复房号按契约静默去重、只保留首次出现：parser 已把响应内重复房号判成
// 结构失败，pipeline 也会把这种响应当任务失败；这里是防 ON CONFLICT DO UPDATE
// 对着同一行改两次的最后一道兜底。
//
// 库里已有的房号换到新楼时按新楼覆盖，不报 ErrRunStoreFailed：跨轮换楼是正常数据，
// 本轮归属由 collect_run_room 单独记录，room 目录只保留全局最新归属。
// 覆盖只改归属楼，不动 room.building_name：那一列不由本链路维护，读路径也不读它
// （查询侧读的是 release_room_building.building_name，由发布阶段写）。
func UpsertRoomAttribution(ctx context.Context, q roomQuerier, attribution RoomAttribution) error {
	buildingID := strings.TrimSpace(attribution.BuildingID)
	if buildingID == "" {
		return fmt.Errorf("%w: 归属落库缺少教学楼编号", model.ErrRunStoreFailed)
	}

	rooms, err := uniqueAttributedRooms(attribution.Rooms)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		return nil
	}

	roomIDs := make([]string, len(rooms))
	names := make([]string, len(rooms))
	nameRaws := make([]string, len(rooms))
	for index, room := range rooms {
		roomIDs[index] = room.RoomID
		names[index] = displayName(room)
		nameRaws[index] = room.NameRaw
	}

	// 单条 INSERT ... SELECT：整批要么全写、要么一条不写，失败时不留半批数据。
	if _, err := q.Exec(ctx, upsertRoomAttributionSQL, buildingID, roomIDs, names, nameRaws); err != nil {
		return fmt.Errorf("%w: 写入教室归属失败: %w", model.ErrRunStoreFailed, err)
	}
	return nil
}

// displayName 返回写进 room.name 的展示名：调用方给的规范化结果优先，
// 为空时回退原始名，原始名也为空时回退房号，保证 name 列永远不是空串。
func displayName(room AttributedRoom) string {
	if name := strings.TrimSpace(room.Name); name != "" {
		return name
	}
	if raw := strings.TrimSpace(room.NameRaw); raw != "" {
		return raw
	}
	return room.RoomID
}

// UpsertRunRooms 把一轮的归属快照增量写进 collect_run_room（幂等）。
//
// 调用方在每份周矩阵响应裁决后调用：Accepted 的房号写本楼，新记的冲突同一批
// 把对应房号的 conflict 打成 true。同一房号重复出现时只合并冲突标记、不改已定归属；
// 空房号、空楼号、同批房号换楼、以及冲突标记与被丢弃的楼不匹配（标了冲突却缺楼、
// 没冲突却带着被丢弃的楼）都直接报错，不把坏归属写进去。
// 轮次不存在或缺学期时 SQL 影响 0 行，这里返回可读错误，不静默丢归属。
func UpsertRunRooms(ctx context.Context, q roomQuerier, runID int64, rooms []RunRoom, now time.Time) error {
	normalized, err := normalizeRunRooms(rooms)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}

	roomIDs := make([]string, len(normalized))
	buildingIDs := make([]string, len(normalized))
	conflicts := make([]bool, len(normalized))
	conflictBuildingIDs := make([]string, len(normalized))
	for index, room := range normalized {
		roomIDs[index] = room.RoomID
		buildingIDs[index] = room.BuildingID
		conflicts[index] = room.Conflict
		conflictBuildingIDs[index] = room.ConflictBuildingID
	}

	tag, err := q.Exec(ctx, upsertRunRoomsSQL,
		runID, roomIDs, buildingIDs, conflicts, conflictBuildingIDs, now)
	if err != nil {
		return fmt.Errorf("%w: 写入每轮归属失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 不存在或未写学期，无法写入每轮归属", model.ErrRunStoreFailed, runID)
	}
	return nil
}

// LoadRunRooms 读回一轮的归属快照，供续跑重建累计器与发布阶段组装归属快照。
// 没有归属时返回空切片而不是错误：空轮次和从未开始采的轮次都是合法的。
func LoadRunRooms(ctx context.Context, q roomQuerier, runID int64) ([]RunRoom, error) {
	rows, err := q.Query(ctx, selectRunRoomsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取每轮归属失败: %w", model.ErrRunStoreFailed, err)
	}
	defer rows.Close()

	rooms := make([]RunRoom, 0)
	for rows.Next() {
		var room RunRoom
		if err := rows.Scan(&room.RoomID, &room.BuildingID, &room.Conflict, &room.ConflictBuildingID); err != nil {
			return nil, fmt.Errorf("%w: 扫描每轮归属失败: %w", model.ErrRunStoreFailed, err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历每轮归属失败: %w", model.ErrRunStoreFailed, err)
	}
	return rooms, nil
}

// normalizeRunRooms 校验并合并同批归属：房号、楼号不能为空；冲突标记与
// 被丢弃的楼必须同进同出（conflict=true 要有 conflict_building_id，
// conflict=false 必须留空）；同一房号只能属于一栋楼（换楼是编排错误），
// 重复行只合并 conflict 与 conflict_building_id。
//
// 冲突列自相矛盾的行直接拒绝写库：续跑 Seed 对同样的组合也报错，写侧不挡就会
// 落下一条「续跑永远读不回来」的快照。同一房号在同一批里重复出现本不该发生
// （一份响应内重复房号已判数据错误），这里合并是为了让重放与补偿写入幂等，
// 而不是替调用方掩盖换楼。
func normalizeRunRooms(rooms []RunRoom) ([]RunRoom, error) {
	unique := make([]RunRoom, 0, len(rooms))
	indexByRoom := make(map[string]int, len(rooms))
	for _, room := range rooms {
		room.RoomID = strings.TrimSpace(room.RoomID)
		room.BuildingID = strings.TrimSpace(room.BuildingID)
		room.ConflictBuildingID = strings.TrimSpace(room.ConflictBuildingID)
		if room.RoomID == "" {
			return nil, fmt.Errorf("%w: 每轮归属的房号为空", model.ErrRunStoreFailed)
		}
		if room.BuildingID == "" {
			return nil, fmt.Errorf("%w: 房号 %s 的每轮归属缺少教学楼编号", model.ErrRunStoreFailed, room.RoomID)
		}
		if room.Conflict && room.ConflictBuildingID == "" {
			return nil, fmt.Errorf(
				"%w: 房号 %s 的每轮归属标了冲突，但缺少被丢弃的教学楼编号",
				model.ErrRunStoreFailed, room.RoomID)
		}
		if !room.Conflict && room.ConflictBuildingID != "" {
			return nil, fmt.Errorf(
				"%w: 房号 %s 的每轮归属未标冲突，却带了被丢弃的教学楼编号 %s",
				model.ErrRunStoreFailed, room.RoomID, room.ConflictBuildingID)
		}

		index, seen := indexByRoom[room.RoomID]
		if !seen {
			indexByRoom[room.RoomID] = len(unique)
			unique = append(unique, room)
			continue
		}
		existing := &unique[index]
		if existing.BuildingID != room.BuildingID {
			return nil, fmt.Errorf("%w: 房号 %s 在同一批归属里换楼（%s → %s）",
				model.ErrRunStoreFailed, room.RoomID, existing.BuildingID, room.BuildingID)
		}
		existing.Conflict = existing.Conflict || room.Conflict
		if existing.ConflictBuildingID == "" {
			existing.ConflictBuildingID = room.ConflictBuildingID
		}
	}
	return unique, nil
}

// RecordAttrConflicts 把本轮归属冲突数回写到 collect_run.attr_conflict_count。
// 取值只增不减（GREATEST），重复调用同一份结果不会累加，也不会把已有更大的
// 计数改小；轮次不存在时返回可读错误，不把落空的写入当成成功。
func RecordAttrConflicts(ctx context.Context, q roomQuerier, runID int64, conflicts int, now time.Time) error {
	if conflicts < 0 {
		return fmt.Errorf("%w: 归属冲突数不能为负: %d", model.ErrRunStoreFailed, conflicts)
	}

	tag, err := q.Exec(ctx, recordAttrConflictsSQL, runID, conflicts, now)
	if err != nil {
		return fmt.Errorf("%w: 写入归属冲突数失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法记录归属冲突", model.ErrRunStoreFailed, runID)
	}
	return nil
}

// uniqueAttributedRooms 按首次出现去掉重复房号，并校验房号非空。
// 房号两侧空白在 parser 里已经去掉，这里再 Trim 一次只为对齐身份。
func uniqueAttributedRooms(rooms []AttributedRoom) ([]AttributedRoom, error) {
	unique := make([]AttributedRoom, 0, len(rooms))
	seen := make(map[string]struct{}, len(rooms))
	for _, room := range rooms {
		roomID := strings.TrimSpace(room.RoomID)
		if roomID == "" {
			return nil, fmt.Errorf("%w: 归属落库时房号为空", model.ErrRunStoreFailed)
		}
		if _, ok := seen[roomID]; ok {
			continue
		}
		seen[roomID] = struct{}{}
		room.RoomID = roomID
		unique = append(unique, room)
	}
	return unique, nil
}

// RoomRepo 把查询器闭在内部，供 pipeline 以窄接口消费：
// pipeline 只看到这四个方法，不接触 SQL 与 room 表的列细节。
type RoomRepo struct {
	q roomQuerier
}

// NewRoomRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewRoomRepo(q roomQuerier) RoomRepo {
	return RoomRepo{q: q}
}

// UpsertRoomAttribution 见同名函数。
func (r RoomRepo) UpsertRoomAttribution(ctx context.Context, attribution RoomAttribution) error {
	return UpsertRoomAttribution(ctx, r.q, attribution)
}

// UpsertRunRooms 见同名函数。
func (r RoomRepo) UpsertRunRooms(ctx context.Context, runID int64, rooms []RunRoom, now time.Time) error {
	return UpsertRunRooms(ctx, r.q, runID, rooms, now)
}

// LoadRunRooms 见同名函数。
func (r RoomRepo) LoadRunRooms(ctx context.Context, runID int64) ([]RunRoom, error) {
	return LoadRunRooms(ctx, r.q, runID)
}

// RecordAttrConflicts 见同名函数。
func (r RoomRepo) RecordAttrConflicts(ctx context.Context, runID int64, conflicts int, now time.Time) error {
	return RecordAttrConflicts(ctx, r.q, runID, conflicts, now)
}
