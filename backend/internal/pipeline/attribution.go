package pipeline

import (
	"errors"
	"fmt"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// errAttributionData 标记「这份响应不能用于归属」的数据类错误：坏响应（空房号、
// 响应内重复房号、缺楼编号）与晚到楼。执行循环把它当任务失败处理——写失败周结果
// 并把任务置 failed——而不是中断整轮；仓储写入失败等编排错误不包这个哨兵，
// 仍直接上抛（见 execute.go 的 Run）。
var errAttributionData = errors.New("pipeline: 归属响应不可用")

// RoomConflict 是一次被裁决掉的跨楼重复：同一房号先出现在 KeptBuildingID 的响应里，
// 后又出现在 DroppedBuildingID 的响应里，后者的整行格子按裁决丢弃。
// 同一房号在一轮里至多产生一条 RoomConflict，重复出现的周次不再重复记。
type RoomConflict struct {
	RoomID            string // 房号 jsbh
	KeptBuildingID    string // 先命中、保住归属的楼
	DroppedBuildingID string // 后出现、整行丢弃的楼
}

// AttributionOffer 是一次 Offer 的裁决结果：本次响应里哪些房号可以归属本楼、
// 哪些因为已被更靠前的楼占用而必须整行丢弃，以及本次新记了哪些冲突。
// Accepted 里的房号用 clean.RoomName 生成展示名后写 room 与 collect_run_room；
// Dropped 里的房号不写 room、不写 collect_run_room，也不能进入候选观测
// （原 spec Requirement 5.3、design CP3）。发布阶段（任务 10）以 collect_run_room
// 的房号集合作为有效归属白名单组装候选集，Dropped 行天然不在其中。
type AttributionOffer struct {
	Accepted  []model.MatrixRoom // 归本楼、可以落库的房号行
	Dropped   []model.MatrixRoom // 已归更靠前的楼、整行丢弃的房号行
	Conflicts []RoomConflict     // 本次新记的冲突；同一房号一轮至多一条
}

// Attribution 累计一轮内的房号归属（原 spec Requirement 5.1、5.3，design CP3）。
//
// 裁决规则是「白名单顺序 + 周次升序取首个命中」：调用方必须按
// Plan 生成的任务顺序（白名单顺序 × 周次升序）逐份响应调用 Offer；
// 房号第一次出现即归属该楼，之后同楼再出现仍归它，别的楼再出现就整行丢弃。
// 白名单名次只用来识别顺序被破坏的调用（更靠前的楼晚于更靠后的楼到达），
// 一旦发生就报数据错误而不是悄悄改归属：已落库的房号不能回退。
//
// 续跑时调用方必须先用 Seed 把库里已完成段的归属灌进来，再对 pending 任务调
// Offer；否则已完成楼的房号会被后跑的楼当成首见，把归属改判（这正是
// 「从 per-run 归属表重建累计器」要解决的 blocker）。
//
// 类型不是并发安全的：一轮执行循环是串行的，不要跨 goroutine 共享。
type Attribution struct {
	ranks        map[string]int      // 楼编号 → 白名单名次，越小越优先
	owner        map[string]string   // 房号 → 首个命中它的楼
	conflicts    []RoomConflict      // 本轮已记的冲突，按首次发生顺序
	conflictSeen map[string]struct{} // 已记过冲突的房号，保证一轮只记一次
}

// NewAttribution 按白名单顺序创建本轮归属累计器。
// 白名单里重复的楼编号只保留首次出现（与 Plan 的裁决顺序一致）；
// 不在白名单里的楼名次排在最后，不会挤掉白名单内的楼。
func NewAttribution(buildingIDs []string) *Attribution {
	return &Attribution{
		ranks:        buildingRanks(uniqueBuildingIDs(buildingIDs)),
		owner:        make(map[string]string),
		conflicts:    make([]RoomConflict, 0),
		conflictSeen: make(map[string]struct{}),
	}
}

// Seed 把一条已落库的归属并入累计器，用于续跑重建（collect_run_room 的读回）。
//
// 与 Offer 不同，Seed 处理的是「已经裁决过」的事实：直接登记 owner；
// conflict 为 true 时同时登记冲突（同一房号只记一次），conflictBuildingID 是
// 库里记下的第一次被丢弃的楼，不能为空——冲突标记却不知道被谁丢弃，
// 说明 collect_run_room 的冲突列已经自相矛盾；conflict 为 false 时该字段必须为空，
// 与写侧（store 的 normalizeRunRooms）保持同一口径，不允许「没冲突却记了被丢弃的楼」。
// 归属楼与被丢弃的楼都必须在本轮白名单里：归属与冲突都只可能来自白名单内的楼，
// 出现别的楼说明传进来的不是本轮快照。已存在同一房号时：同楼是幂等重放，直接吸收；
// 换楼说明库里的归属自相矛盾，返回错误而不是悄悄改写。
//
// 所有校验都在写入之前完成，失败时累计器保持原状。
// Seed 不写库、不依赖时钟；执行循环在跑任何 pending 任务之前调用它。
func (a *Attribution) Seed(roomID, buildingID string, conflict bool, conflictBuildingID string) error {
	roomID = strings.TrimSpace(roomID)
	buildingID = strings.TrimSpace(buildingID)
	conflictBuildingID = strings.TrimSpace(conflictBuildingID)
	if roomID == "" {
		return fmt.Errorf("pipeline: 续跑归属里有空房号")
	}
	if buildingID == "" {
		return fmt.Errorf("pipeline: 续跑归属里房号 %q 缺少教学楼编号", roomID)
	}
	if _, ok := a.ranks[buildingID]; !ok {
		return fmt.Errorf("pipeline: 续跑归属里房号 %q 的教学楼 %s 不在本轮白名单里", roomID, buildingID)
	}
	if conflict && conflictBuildingID == "" {
		return fmt.Errorf("pipeline: 续跑归属里房号 %q 标了冲突，但缺少被丢弃的教学楼编号", roomID)
	}
	if !conflict && conflictBuildingID != "" {
		return fmt.Errorf(
			"pipeline: 续跑归属里房号 %q 未标冲突，却带了被丢弃的教学楼编号 %s",
			roomID, conflictBuildingID)
	}
	if conflictBuildingID != "" {
		if _, ok := a.ranks[conflictBuildingID]; !ok {
			return fmt.Errorf(
				"pipeline: 续跑归属里房号 %q 被丢弃的教学楼 %s 不在本轮白名单里",
				roomID, conflictBuildingID)
		}
	}

	if owner, claimed := a.owner[roomID]; claimed && owner != buildingID {
		return fmt.Errorf("pipeline: 续跑归属里房号 %q 同时属于 %s 与 %s", roomID, owner, buildingID)
	}
	a.owner[roomID] = buildingID

	if !conflict {
		return nil
	}
	if _, recorded := a.conflictSeen[roomID]; recorded {
		return nil
	}
	a.conflictSeen[roomID] = struct{}{}
	a.conflicts = append(a.conflicts, RoomConflict{
		RoomID:            roomID,
		KeptBuildingID:    buildingID,
		DroppedBuildingID: conflictBuildingID,
	})
	return nil
}

// Offer 并入一栋楼一份周矩阵响应里的全部房号，返回本份响应的裁决结果。
//
// 对每个房号：
//   - 本轮还没出现过：归本楼，进入 Accepted；
//   - 已归本楼（同楼另一周重复报出）：仍算 Accepted，用于幂等重写 room；
//   - 已归别的楼：进入 Dropped，整行丢弃；若这是该房号本轮第一次冲突，
//     同时记一条 RoomConflict（同一房号一轮只记一次，不按周次重复计）。
//
// 同一份响应里房号为空或重复直接报 errAttributionData：parser 已把这两种情况判成
// 结构失败（当前 spec Requirement 2.4、2.5），这里再兜一层，避免用坏数据覆盖 room。
// 比已归属楼更靠前的楼晚到时报 errAttributionData，不静默改写归属：后到的响应
// 不能让已落库的房号换楼，否则「首个命中」就失去意义。
//
// Offer 对一份响应是原子的：先把整份响应校验完，再一次写入归属。
// 校验失败时累计器保持原状，不会留下半份归属；执行循环据此把该任务判失败。
func (a *Attribution) Offer(buildingID string, rooms []model.MatrixRoom) (AttributionOffer, error) {
	buildingID = strings.TrimSpace(buildingID)
	if buildingID == "" {
		return AttributionOffer{}, fmt.Errorf("%w: 归属缺少教学楼编号", errAttributionData)
	}

	// 先整体校验：坏房号、响应内重复、以及会破坏「首个命中」的晚到楼，
	// 都要在写入任何一条归属之前拒绝，避免半份响应污染本轮归属。
	seen := make(map[string]struct{}, len(rooms))
	for _, room := range rooms {
		if room.RoomID == "" {
			return AttributionOffer{}, fmt.Errorf("%w: 楼 %s 的响应里有空房号", errAttributionData, buildingID)
		}
		if _, ok := seen[room.RoomID]; ok {
			return AttributionOffer{}, fmt.Errorf("%w: 楼 %s 的响应里房号 %q 重复",
				errAttributionData, buildingID, room.RoomID)
		}
		seen[room.RoomID] = struct{}{}

		kept, claimed := a.owner[room.RoomID]
		if !claimed || kept == buildingID {
			continue
		}
		// 更靠前的楼晚到：正确顺序下它应该先被处理，现在无法撤回已落库的归属。
		if buildingRank(a.ranks, buildingID) < buildingRank(a.ranks, kept) {
			return AttributionOffer{}, fmt.Errorf(
				"%w: 楼 %s 的响应晚于白名单更靠后的楼 %s 到达，房号 %q 的归属无法回退",
				errAttributionData, buildingID, kept, room.RoomID)
		}
	}

	offer := AttributionOffer{}
	for _, room := range rooms {
		kept, claimed := a.owner[room.RoomID]
		switch {
		case !claimed:
			a.owner[room.RoomID] = buildingID
			offer.Accepted = append(offer.Accepted, room)
		case kept == buildingID:
			offer.Accepted = append(offer.Accepted, room)
		default:
			offer.Dropped = append(offer.Dropped, room)
			if _, recorded := a.conflictSeen[room.RoomID]; recorded {
				continue
			}
			a.conflictSeen[room.RoomID] = struct{}{}
			conflict := RoomConflict{
				RoomID:            room.RoomID,
				KeptBuildingID:    kept,
				DroppedBuildingID: buildingID,
			}
			a.conflicts = append(a.conflicts, conflict)
			offer.Conflicts = append(offer.Conflicts, conflict)
		}
	}
	return offer, nil
}

// BuildingOf 返回房号本轮已归属的楼；还没出现过时第二个返回值为 false。
func (a *Attribution) BuildingOf(roomID string) (string, bool) {
	buildingID, ok := a.owner[roomID]
	return buildingID, ok
}

// ConflictCount 返回本轮已记的冲突房号数（按房号去重），包含 Seed 进来的
// 续跑前已记冲突。它就是要写进 collect_run.attr_conflict_count 的值：
// 同一房号一轮只加一次，续跑不能把已有冲突数丢掉，否则后续新冲突会把
// GREATEST 已经抬高的计数又写回小值。
func (a *Attribution) ConflictCount() int {
	return len(a.conflictSeen)
}

// Conflicts 返回本轮冲突的副本，按首次发生顺序排列（Seed 进来的在前）；
// 修改返回值不影响累计器。
func (a *Attribution) Conflicts() []RoomConflict {
	return append([]RoomConflict(nil), a.conflicts...)
}

// Snapshot 返回本轮至今的归属集合（房号 → 楼编号）副本。
// 发布阶段可以据此写版本归属快照；修改返回值不影响累计器。
func (a *Attribution) Snapshot() map[string]string {
	snapshot := make(map[string]string, len(a.owner))
	for roomID, buildingID := range a.owner {
		snapshot[roomID] = buildingID
	}
	return snapshot
}
