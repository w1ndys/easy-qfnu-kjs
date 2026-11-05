package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// shanghai 是业务时区。与 pipeline/query/stats/upstream 一致用固定 +08:00 而不
// LoadLocation：2026 年之后没有夏令时，固定偏移与 Asia/Shanghai 等价，
// 也免去容器缺 tzdata 时的分支。发布落库的时间戳都换算到这里；
// 版本标识例外，按 UTC 生成（见 releaseIdentifier）。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// releaseIDLayout 是版本标识的时间部分：UTC 精确到秒。版本标识只由这段时间
// 加一个固定的 Z 后缀组成（见 releaseIdentifier），不含轮次编号等其他字段，
// 因此可以直接用 SQL 按前缀/区间筛版本，也能一眼看出是哪一刻生成的。
const releaseIDLayout = "20060102T150405Z"

// Committer 是发布写侧需要的唯一能力，由 store.ReleaseRepo 实现：
// 它在一个事务里写版本、切换 current、删旧版本并回写轮次。
// publish 只调这一个方法，不接触 pgx、SQL 与表结构（依赖方向见
// backend/.golangci.yml 的 publish 规则）。
type Committer interface {
	CommitRelease(ctx context.Context, draft store.ReleaseDraft) error
}

// CommitResult 是一次成功发布的结果：调用方用它记日志或回填管理端响应，
// 不必再回库查「刚发布的是哪个版本」。
type CommitResult struct {
	ReleaseID string // 新版本标识
}

// Commit 把已通过 Validate 的候选集提交为一个新版本并切换 current
// （design 的 publish.Commit、Requirement 7.1–7.4）。now 是发布时刻，注入而不是
// 在函数里读时钟：测试可以用固定时刻锁住 collect_run.updated_at，生产由编排传
// 同一个时钟；候选集里的 GeneratedAt 是版本生成时间，两者语义不同，不要混用。
//
// 本函数只做「转成写侧草稿 + 生成版本标识 + 交给仓储」，事务步骤、批大小与回滚
// 全在 store.CommitRelease：publish 不 import pgx，也就能在不连库的情况下测完
// 这一步的全部判断。
//
// 草稿带发布终态，口径与 pipeline 的 decideOutcome 一致：可选任务全部 done、
// 且没有被丢弃的可选明细时写 success；有可选失败、或有被归属过滤丢弃的可选明细
// 时写 partial。partial 同时把失败原因写进 collect_run.error_message，口径与
// pipeline.CloseRun 的 partial 分支一致：有可选失败写最早失败任务的原因，只有
// 被丢弃的可选明细时写一句丢弃计数；success 传空串，由写层折成 NULL 清空。
// 轮次会在同一个发布事务里离开 running 并写 phase=publish。因此调用方在 Commit
// 成功后不要再对同一轮调用 FinishRun/CloseRun——终态已经由发布事务写死，再收口
// 会撞上「已收口」的守卫。
//
// 它不重复业务校验（必需任务是否完成、归属快照是否完整、未知/复合阈值）——
// 那是 Validate 的职责，调用方必须先 Validate 再 Commit。这里只挡会让线上数据
// 变空或写坏的硬错误：候选集没有观测（Requirement 7.1，带 ReasonEmptyCandidate
// 的校验拦截）、版本身份字段缺失（ErrCandidateInputInvalid）、可选任务还有
// pending（写终态会把「还没做」记成「已完成」）。
//
// 另有两条终态口径只能在这里判，因为库里看不到对应任务：
//   - 被归属过滤丢弃的必需明细（DroppedRequired > 0）：这些任务从未写进
//     sync_task，任务汇总与 Validate 都看不见它们，按库里的数字会判成「全部完成」。
//     与 decideOutcome 的 failed 同口径，这里直接拒绝发布（ErrRequiredTasksIncomplete），
//     不写任何版本；
//   - 被丢弃的可选明细（DroppedOptional > 0）：数据可用性完整、只是明细不全，
//     与可选失败同口径写 partial。
//
// 成功返回新版本标识；仓储失败原样包装上抛（errors.Is 可命中仓库层哨兵），
// 调用方不应在失败后再去查询「新版本」，事务已经整体回滚，线上仍是原 current。
func Commit(ctx context.Context, committer Committer, candidate Candidate, now time.Time) (CommitResult, error) {
	if committer == nil {
		return CommitResult{}, errors.New("publish: 缺少发布仓储")
	}
	term := strings.TrimSpace(candidate.Term)
	if candidate.RunID <= 0 {
		return CommitResult{}, fmt.Errorf("%w: 轮次编号非法: %d", ErrCandidateInputInvalid, candidate.RunID)
	}
	if term == "" {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 没有学期，无法发布", ErrCandidateInputInvalid, candidate.RunID)
	}
	if candidate.GeneratedAt.IsZero() {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 没有版本生成时间，无法发布",
			ErrCandidateInputInvalid, candidate.RunID)
	}
	if now.IsZero() {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 缺少发布时刻，无法发布",
			ErrCandidateInputInvalid, candidate.RunID)
	}
	if candidate.DictVersion <= 0 || candidate.AxisVersion <= 0 {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 的字典版本 %d / 轴版本 %d 非法",
			ErrCandidateInputInvalid, candidate.RunID, candidate.DictVersion, candidate.AxisVersion)
	}
	// 两个汇总要回写 collect_run：0 说明候选集没算计数，写进去会让管理端把
	// 这一轮显示成「零间房、零周」。窗口合法时周数至少 1，归属非空时房号数至少 1。
	if candidate.RoomsTotal <= 0 || candidate.WeeksTotal <= 0 {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 的房号数 %d / 窗口周数 %d 非法",
			ErrCandidateInputInvalid, candidate.RunID, candidate.RoomsTotal, candidate.WeeksTotal)
	}
	// 窗口随草稿一起交给写层做交叉校验（WeeksTotal 必须等于窗口周数，房号数必须
	// 等于归属快照行数）。窗口非法时这两条都对不上，先在这里挡住。
	if candidate.WindowStart < 1 || candidate.WindowEnd < candidate.WindowStart {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 的采集窗口非法: %d..%d",
			ErrCandidateInputInvalid, candidate.RunID, candidate.WindowStart, candidate.WindowEnd)
	}
	// 发布写的是终态：可选任务必须全部有结果（done 或 failed）。还有 pending 说明
	// 执行循环没跑完，写 success/partial 都会把「还没做」记成「已完成」；口径与
	// pipeline 的 decideOutcome 一致。必需的 pending 已由 Validate 拦截。
	if pending := candidate.OptionalTotal - candidate.OptionalDone - candidate.OptionalFailed; pending > 0 {
		return CommitResult{}, fmt.Errorf("%w: 轮次 %d 还有 %d 条可选任务未完成，不能发布",
			ErrCandidateInputInvalid, candidate.RunID, pending)
	}
	// 被归属过滤丢弃的必需明细从未写进 sync_task，任务汇总与 Validate 都看不到
	// 它们；不在这里挡，就会把「明细一条都没拿到」发成 success。与 decideOutcome
	// 的 failed 同口径：拒绝发布，保留线上版本。用「必需任务未完成」同一哨兵与
	// 原因，调用方不必为丢弃单独分支。
	if candidate.DroppedRequired > 0 {
		return CommitResult{}, newValidationError(
			ReasonRequiredIncomplete,
			fmt.Errorf("%w: 轮次 %d 有 %d 条必需明细任务被归属过滤丢弃，没有可发布的数据",
				ErrRequiredTasksIncomplete, candidate.RunID, candidate.DroppedRequired))
	}
	// 终态与 pipeline 的 decideOutcome 同口径：可选任务全部完成且没有被丢弃的
	// 可选明细 → success；有可选失败、或有被丢弃的可选明细 → partial（可用性数据
	// 完整，发布仍继续）。必需失败在 Validate 阶段就拦下了，不会走到这里。
	status := store.RunStatusSuccess
	// partial 的 error_message 与 pipeline.CloseRun 同口径（见 partialMessage）：
	// 有可选失败写最早失败任务的原因，只有被丢弃的可选明细时写一句丢弃计数。
	// success 留空串，让发布事务把这一列清成 NULL——一轮全部成功不该带着上一状态的文案。
	message := ""
	if candidate.OptionalFailed > 0 || candidate.DroppedOptional > 0 {
		status = store.RunStatusPartial
		message = partialMessage(candidate)
	}
	// 空版本会把公开查询整体变成空态，不能当成一次成功的发布；
	// 与 Validate 的拦截同口径（同一个 Reason 与哨兵）。
	if len(candidate.Observations) == 0 {
		return CommitResult{}, newValidationError(
			ReasonEmptyCandidate,
			fmt.Errorf("%w: 轮次 %d 没有可发布的观测", ErrEmptyCandidate, candidate.RunID))
	}

	releaseID := releaseIdentifier(candidate)
	draft := store.ReleaseDraft{
		ReleaseID:       releaseID,
		RunID:           candidate.RunID,
		Term:            term,
		GeneratedAt:     candidate.GeneratedAt.In(shanghai),
		UpdatedAt:       now.In(shanghai),
		DictVersion:     candidate.DictVersion,
		AxisVersion:     candidate.AxisVersion,
		WindowStart:     candidate.WindowStart,
		WindowEnd:       candidate.WindowEnd,
		HasAttrConflict: candidate.HasAttrConflict,
		UnknownCells:    candidate.UnknownCells,
		CompositeCells:  candidate.CompositeCells,
		RoomsTotal:      candidate.RoomsTotal,
		WeeksTotal:      candidate.WeeksTotal,
		Status:          status,
		ErrorMessage:    message,
		Rooms:           releaseRooms(candidate.Rooms),
		Observations:    releaseObservations(candidate.Observations),
		Details:         releaseDetails(candidate.Details),
	}
	if err := committer.CommitRelease(ctx, draft); err != nil {
		return CommitResult{}, fmt.Errorf("publish: 发布轮次 %d 的版本失败: %w", candidate.RunID, err)
	}
	return CommitResult{ReleaseID: releaseID}, nil
}

// partialMessage 给出 partial 轮次要写进 collect_run.error_message 的原因，
// 与 pipeline.CloseRun 的 partial 分支同口径：
//   - 有可选任务失败：写最早失败任务的原因（Candidate.FirstFailureMessage，按
//     task_id 升序取第一条 failed），与收口写的是同一条；
//   - 只有被归属过滤丢弃的可选明细：库里没有 failed 行可引用，用丢弃计数写一句。
//
// publish 不能 import pipeline（依赖方向见 backend/.golangci.yml），文案只能在
// partialDroppedMessage 里维护一份副本；两处必须一起改，否则同一种 partial 会
// 因为走发布事务还是走 CloseRun 而显示不同的原因。
func partialMessage(candidate Candidate) string {
	if candidate.OptionalFailed > 0 {
		if candidate.FirstFailureMessage != "" {
			return candidate.FirstFailureMessage
		}
		// 失败任务没带原因时也要留一句，避免 partial 轮次 error_message 为空。
		return "本轮有可选任务失败"
	}
	return partialDroppedMessage(candidate.DroppedRequired, candidate.DroppedOptional)
}

// partialDroppedMessage 把归属过滤丢弃的明细任务写成一句可读原因，字面文案与
// pipeline.droppedMessage 保持一致（两处都只用于 partial 轮次的 error_message）。
func partialDroppedMessage(required, optional int) string {
	total := required + optional
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("归属校验丢弃 %d 条明细任务（必需 %d 条、可选 %d 条）", total, required, optional)
}

// releaseIdentifier 生成版本标识：生成时刻换算到 UTC 后加 Z 后缀，
// 例如 UTC 2026-10-23 07:10:12 → 20261023T071012Z。
//
// 时间取 GeneratedAt 而不是发布时刻：标识描述「这批数据是哪一刻生成的」，
// 与 release.generated_at 同源。格式固定为 `<UTC 秒级>Z`，不含轮次编号，
// 因此同一秒内生成的两个轮次会产生同一个标识——由 store 的发布前置检查
// 提前拒绝（返回「版本标识已存在」而不是撞 release_pkey）。毫秒被截掉：
// 标识只描述到秒。
func releaseIdentifier(candidate Candidate) string {
	return candidate.GeneratedAt.UTC().Format(releaseIDLayout)
}

// releaseRooms 把版本归属快照映射成写侧草稿。
func releaseRooms(rooms []RoomBuilding) []store.ReleaseRoom {
	if len(rooms) == 0 {
		return nil
	}
	mapped := make([]store.ReleaseRoom, len(rooms))
	for index, room := range rooms {
		mapped[index] = store.ReleaseRoom{
			RoomID:       room.RoomID,
			BuildingID:   room.BuildingID,
			BuildingName: room.BuildingName,
		}
	}
	return mapped
}

// releaseObservations 把逐格观测映射成写侧草稿。状态与可用性原样搬运，
// 不在这里重算（CP4：可用性只按字典投影，publish 不写第二套判断）。
func releaseObservations(observations []Observation) []store.ReleaseObservation {
	if len(observations) == 0 {
		return nil
	}
	mapped := make([]store.ReleaseObservation, len(observations))
	for index, observation := range observations {
		mapped[index] = store.ReleaseObservation{
			RoomID:    observation.RoomID,
			Week:      observation.Week,
			Weekday:   observation.Weekday,
			Node:      observation.Node,
			StateKey:  observation.StateKey,
			Available: observation.Available,
			RawText:   observation.RawText,
		}
	}
	return mapped
}

// releaseDetails 把占用说明映射成写侧草稿，Derived 原样保留：
// 它标记这条说明是否补写过派生观测，是版本内可回溯的信息。
func releaseDetails(details []OccupancyDetail) []store.ReleaseDetail {
	if len(details) == 0 {
		return nil
	}
	mapped := make([]store.ReleaseDetail, len(details))
	for index, detail := range details {
		mapped[index] = store.ReleaseDetail{
			RoomID:    detail.RoomID,
			Weekday:   detail.Weekday,
			Block:     detail.Block,
			StateKey:  detail.StateKey,
			Course:    detail.Course,
			WeekRange: detail.WeekRange,
			TimeFlag:  detail.TimeFlag,
			Derived:   detail.Derived,
		}
	}
	return mapped
}
