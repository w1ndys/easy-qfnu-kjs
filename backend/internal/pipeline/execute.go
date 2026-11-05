package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// TaskFetch 是一条任务抓取与解析的结果。
//
// Page 是本任务抓到的页面正文：执行循环只对周矩阵正文算 sha256（判断上游是否变化），
// 原始 HTML 不落库。Rooms 是周矩阵响应解析出的房号行（含逐格原文），执行循环
// 用它做跨楼归属裁决；占用说明没有房号行，Rooms 为空。
//
// 周矩阵的格子数据属于候选观测，必须只取 AttributionOffer.Accepted 里的行：
// 被判定为跨楼重复的房号整行丢弃，不能写进 room，也不能进入候选集。
type TaskFetch struct {
	Page  []byte             // 页面正文
	Rooms []model.MatrixRoom // 周矩阵的房号行；占用说明为空
}

// TaskFetcher 执行一条同步任务：请求上游、解析正文并清洗都由注入的具体实现完成。
// 周矩阵响应要把解析出的房号行放回 TaskFetch.Rooms，执行循环据此落归属；
// 占用说明的正文由 TaskFetcher 自行处理或丢弃，执行循环不回读、不落库。
// 返回错误表示该任务失败；ctx 被调用方取消时执行循环不会把它当任务失败。
type TaskFetcher func(ctx context.Context, task Task) (TaskFetch, error)

// TaskStore 是执行循环需要的最小仓储能力，由 store.SyncTaskRepo 实现。
// pipeline 只调这些方法，不接触 pgx、SQL 与 sync_task 的列细节
// （依赖方向见 backend/.golangci.yml）。
type TaskStore interface {
	// EnsureWeekMatrixTasks 写入周矩阵段并返回库里的权威任务清单（按执行顺序）。
	// 周矩阵任务不依赖 room，新库也能先落这一段。
	EnsureWeekMatrixTasks(ctx context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error)
	// EnsureCellDetailTasks 写入占用说明段并返回库里的权威任务清单。
	// 调用前必须先完成本轮归属落库：room_id 有外键指向 room。
	EnsureCellDetailTasks(ctx context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error)
	// RecordWeekFetch 写一条成功周矩阵抓取结果，含 fetched_at 与页面 sha256。
	RecordWeekFetch(ctx context.Context, fetch store.RunWeekFetch) error
	// RecordWeekFailure 写一条失败周矩阵结果（ok=false + error_message），
	// 与成功行共用唯一键，后写的一方覆盖前一方。
	RecordWeekFailure(ctx context.Context, failure store.RunWeekFailure) error
	// CompleteSyncTask 把一条 pending 任务置为 done 并回写轮次完成计数。
	CompleteSyncTask(ctx context.Context, runID, taskID int64, completedAt time.Time) error
	// FailSyncTask 把一条 pending 任务置为 failed；同一轮内不会再被重试。
	FailSyncTask(ctx context.Context, runID, taskID int64, message string, failedAt time.Time) error
}

// RoomStore 是执行循环落归属需要的最小仓储能力，由 store.RoomRepo 实现。
// pipeline 只调这四个方法，不接触 pgx、SQL 与 room 表的列细节
// （依赖方向见 backend/.golangci.yml）。
type RoomStore interface {
	// UpsertRoomAttribution 把一栋楼本轮归属的房号幂等写进 room 目录。
	UpsertRoomAttribution(ctx context.Context, attribution store.RoomAttribution) error
	// UpsertRunRooms 把本轮归属快照增量写进 collect_run_room（幂等）。
	// 只写裁决后的有效归属；被丢弃的楼不落行，冲突只打在有效归属那一行上。
	UpsertRunRooms(ctx context.Context, runID int64, rooms []store.RunRoom, now time.Time) error
	// LoadRunRooms 读回本轮已落库的归属，续跑时用它重建累计器。
	LoadRunRooms(ctx context.Context, runID int64) ([]store.RunRoom, error)
	// RecordAttrConflicts 把本轮归属冲突数回写轮次；取值只增不减，重复调用不累加。
	RecordAttrConflicts(ctx context.Context, runID int64, conflicts int, now time.Time) error
}

// RunDeps 是执行一轮采集所需的注入项：任务仓储、归属仓储、阶段仓储、
// 任务执行函数与时钟。
type RunDeps struct {
	Tasks  TaskStore   // 任务与周结果的仓储
	Rooms  RoomStore   // 教室归属与冲突计数的仓储
	Phases PhaseStore  // 阶段推进与轮次收口的仓储
	Fetch  TaskFetcher // 一条任务的执行函数
	Clock  Clock       // 完成时刻来源；nil 退回 time.Now
}

// RunProgress 是一轮执行循环的计数，供失败分层与轮次收口使用。
//
// Completed 是本轮实际执行成功并置 done 的任务数；Failed 是本轮失败的任务数，
// 包含两类：执行失败并写进 sync_task 置 failed 的任务，以及被归属过滤丢弃、
// 从未落库的占用说明任务。后者没有真实执行，但同样是一条没拿到的明细，算进
// Failed 才能让「明细全部被丢弃」的一轮在收口时不再静默成功。
// SkippedDone/SkippedFailed 是续跑跳过的任务，不属于本轮结果；它们与
// Completed/Failed 不互斥：续跑一轮里既可能跳过已完成的，也可能新执行几条。
type RunProgress struct {
	Completed     int // 本轮执行成功并置 done 的任务数
	Failed        int // 本轮失败的任务数；含被归属过滤丢弃的明细（细分见 DroppedTasks）
	SkippedDone   int // 续跑时跳过的已完成任务数
	SkippedFailed int // 续跑时保留失败、不重试的任务数

	// dropped 是 Failed 里被归属过滤丢弃的那部分，按必需性分开。丢弃任务不写
	// sync_task，收口只读库里的 failed 数会漏掉它们，所以调用方要用 Dropped()
	// 取出来交给 CloseRun / EvaluateRun。字段不导出：它是 Failed 的细分，
	// 不是与 Failed 并列的第二个计数，避免被再次加成一份。
	dropped DroppedTasks
}

// DroppedTasks 是被归属过滤丢弃的占用说明任务数，按必需性分开。
// 它们没有真实执行、也不写 sync_task，但收口必须算进失败：可选明细丢弃 →
// partial，必需明细丢弃 → failed（口径见 phase.go 的 decideOutcome）。
type DroppedTasks struct {
	Required int // 被丢弃的必需明细任务数
	Optional int // 被丢弃的可选明细任务数
}

// Total 返回被丢弃的明细任务总数。
func (d DroppedTasks) Total() int {
	return d.Required + d.Optional
}

// Dropped 返回已计入 Failed 的归属过滤丢弃数，供收口在库里的 failed 行之外补上
// 这批从未落库的失败。
func (p RunProgress) Dropped() DroppedTasks {
	return p.dropped
}

// markDropped 记一条被归属过滤丢弃的明细任务：既计入 Failed（它同样是一条没拿到
// 的明细），也按必需性记进 dropped 供收口使用。
func (p *RunProgress) markDropped(required bool) {
	p.Failed++
	if required {
		p.dropped.Required++
		return
	}
	p.dropped.Optional++
}

// Run 执行一轮已规划的任务（本 spec Requirement 8.4、5.1–5.3，原 spec Requirement
// 11.1–11.2）：
//
//   - whitelist 是本轮归属裁决的白名单顺序，必须由调用方显式传入：入口先
//     TrimSpace 并拒绝空编号，这一步先于任何任务落库；落库后再拿它与库里周矩阵
//     任务的楼顺序、楼内周次升序比对，不一致就报错，不再只从 task_id 反推白名单。
//     本轮没有周矩阵任务时允许为空；
//   - 规划里的周矩阵段在落库前先校验「楼块不交错 + 楼内周次严格升序」，并把
//     楼序列与规范化后的白名单逐位比对（planned 为空跳过）：坏顺序或错配一旦
//     写进 sync_task，续跑会读回同一份顺序并永久报错，所以失败时一条任务都不落库。
//     落库后的白名单比对仍然保留：库里可能已有上一轮留下的任务，顺序以库里的为准；
//   - 先把周矩阵段落库并返回库里的权威任务清单；本轮已有该段任务时以库里的
//     任务与状态为准，不重复写。周矩阵落库不依赖 room，新库也能先跑这一段；
//   - 从 collect_run_room 读回本轮已落库的归属灌进累计器（含崩溃窗口里「归属已写、
//     任务还没置 done」的那一条），再从第一个 pending 继续，done 直接跳过，
//     failed 保持原状不重试；
//   - 每条 pending 任务调用注入的 TaskFetcher 取页面正文与解析结果；只有周矩阵正文由
//     执行循环哈希，占用说明正文由 TaskFetcher 自行处理或丢弃，执行循环不解析、不回读；
//   - 周矩阵成功后先把本份响应的房号并入本轮归属（白名单顺序 + 周次升序取首个命中）：
//     首见的房号写 room 与 collect_run_room（room.building_id 即本轮归属集合的元素），
//     重复出现在后续楼里的房号整行丢弃并按房号记一次冲突，然后才写 collect_run_week
//     （fetched_at + sha256）、置任务 done 并回写轮次完成计数。周矩阵失败先写同一
//     唯一键的 ok=false 行与原因，再置 failed；
//   - 归属裁决拒绝一份响应（坏房号、响应内重复房号、晚到楼）属于数据类错误：
//     该任务按失败路径处理（RecordWeekFailure + FailSyncTask），循环继续跑后面的
//     任务；只有仓储写入失败这类编排错误才立即中止整轮；
//   - 周矩阵段跑完、归属已落库后，先按本轮归属过滤占用说明任务，再落这一段并执行：
//     明细任务的楼必须等于 Attribution.BuildingOf(房号) 的归属楼；房号归了别的楼、
//     或本轮根本没有归属时，整条任务丢弃——不落库、不执行，并计入 RunProgress.Failed
//     （细分记在 DroppedTasks 里）。丢弃不进 sync_task，所以收口单读库里的 failed 行会
//     漏掉它们；调用方要把 RunProgress.Dropped() 同时交给 CloseRun（失败收口）和
//     publish.Build 的 RunInput（发布终态判定），两处必须是同一份值。
//     Plan 只能按白名单 rank 给格子挑楼，不知道这个房号最终归谁；不挡就会把
//     「靠后的楼报了首见楼的格子」下钻到非归属楼上。sync_task.room_id 的外键也因此
//     不会在 room 尚不存在时把两段任务一起打挂；
//   - 每段落库成功之后、执行这一段任务之前推进 collect_run.phase（周矩阵 → 占用说明），
//     供管理端看到当前进度。阶段只前进：续跑时已经过了的阶段不再写回去，已经收口的
//     轮次直接拒绝执行（终态由失败收口或发布事务写）。success/partial 由
//     publish.Commit 写，失败由本包的 CloseRun / FailCalendar（phase.go）写；
//     本函数只负责执行任务、落库结果并返回计数；
//   - 任务失败置 failed 后继续下一条，是否让整轮失败由失败分层决定；
//   - 调用方取消或超时不把当前任务记成 failed，留给下一次续跑。
//
// 落库顺序是刻意的：先写归属与周结果、再改任务状态。两者之间崩溃时任务仍是 pending，
// 续跑会重做并覆盖，不会出现任务已完成却缺房号或缺周结果的状态。
//
// 当轮归属快照落在 collect_run_room，只含 Accepted 与被保留归属的冲突标记；
// Dropped 行只出现在 AttributionOffer.Dropped 里，不写 room、不写 collect_run_room，
// 也不会进入候选集。发布阶段（任务 10）用 store.LoadRunRooms 读这份快照组装
// 候选集与版本归属，本函数不再返回另一份内存快照。
func Run(
	ctx context.Context,
	runID int64,
	whitelist []string,
	planned []Task,
	deps RunDeps,
) (RunProgress, error) {
	if deps.Tasks == nil {
		return RunProgress{}, errors.New("pipeline: 缺少任务仓储")
	}
	if deps.Rooms == nil {
		return RunProgress{}, errors.New("pipeline: 缺少归属仓储")
	}
	if deps.Phases == nil {
		return RunProgress{}, errors.New("pipeline: 缺少阶段仓储")
	}
	if deps.Fetch == nil {
		return RunProgress{}, errors.New("pipeline: 缺少任务执行函数")
	}

	buildings, err := normalizeWhitelist(whitelist)
	if err != nil {
		return RunProgress{}, err
	}

	weekInputs, cellInputs, err := splitTaskInputs(planned)
	if err != nil {
		return RunProgress{}, err
	}
	// 白名单必须落库前就与规划对齐：排了周矩阵任务却没有白名单时直接报错，
	// 不让一批无法用于裁决归属的任务先进库。
	if len(weekInputs) > 0 && len(buildings) == 0 {
		return RunProgress{}, fmt.Errorf(
			"pipeline: 本轮有 %d 条周矩阵任务，但缺少白名单顺序", len(weekInputs))
	}
	// 周矩阵段的顺序必须在落库前校验：坏计划一旦写进 sync_task，续跑会读回
	// 同一份顺序并永久报错，后面再改调用参数也救不回来。
	plannedBuildings, err := checkWeekInputOrder(weekInputs)
	if err != nil {
		return RunProgress{}, err
	}
	// 楼顺序同样要在落库前对齐白名单：周次校验只看楼内相对顺序，挡不住
	// 「计划 Q10,Q11、白名单 Q11,Q10」这类错配。按错的顺序落库、再按白名单裁决，
	// 归属会被判给后执行的楼；直接拒绝，一条任务都不写。planned 为空时跳过：
	// 本轮没有周矩阵任务，白名单不参与裁决（续跑仍可能传入它）。
	if err := checkPlannedWhitelist(plannedBuildings, buildings); err != nil {
		return RunProgress{}, err
	}

	// 阶段只前进：先读一次当前阶段，据此决定哪些推进要跳过。已收口的轮次不能再执行——
	// 「已结束」按 collect_run.status 判（status != running），不按 phase：发布事务
	// 写 success/partial 时阶段停在 publish，不是 finished。失败收口（CloseRun /
	// FailCalendar / FailStaleRuns）与发布事务都会同时写 status 与 finished_at。
	current, err := currentPhase(ctx, deps.Phases, runID)
	if err != nil {
		return RunProgress{}, err
	}

	persistedWeeks, err := deps.Tasks.EnsureWeekMatrixTasks(ctx, runID, weekInputs, nowIn(deps.Clock))
	if err != nil {
		return RunProgress{}, err
	}
	// 推进放在 Ensure 成功之后：任务还没落库就把阶段写成周矩阵，管理端会看到
	// 「已到周矩阵」却一条任务都没有；Ensure 失败时阶段保持原样，续跑照常重来。
	// 续跑已经过了这一段时跳过，阶段不回退（minor 2）。
	if phaseBefore(current, PhaseWeekMatrix) {
		if err := AdvancePhase(ctx, deps.Phases, runID, PhaseWeekMatrix, deps.Clock); err != nil {
			return RunProgress{}, err
		}
		current = PhaseWeekMatrix
	}

	buildings, err = resolveWhitelist(buildings, persistedWeeks)
	if err != nil {
		return RunProgress{}, err
	}
	attribution := NewAttribution(buildings)
	if err := seedAttribution(ctx, deps.Rooms, runID, attribution); err != nil {
		return RunProgress{}, err
	}
	// 续跑可能落在「collect_run_room 已打冲突标记、collect_run.attr_conflict_count
	// 还没写」的崩溃窗口里。重放那份响应时 Offer 不会再把同一房号算成新冲突，
	// 只靠「有新冲突才写」会漏掉计数；这里先把库里的累计值补写一次。
	// GREATEST 保证重复调用不累加，正常续跑也不会改大。
	if attribution.ConflictCount() > 0 {
		if err := deps.Rooms.RecordAttrConflicts(ctx, runID, attribution.ConflictCount(), nowIn(deps.Clock)); err != nil {
			return RunProgress{}, err
		}
	}

	var progress RunProgress
	if err := executeTasks(ctx, deps, runID, persistedWeeks, attribution, &progress); err != nil {
		return progress, err
	}

	// 占用说明段必须等周矩阵段跑完：只有这时本轮归属才写进 room，
	// sync_task.room_id 的外键才不会因「新库还没有这间房」整批失败。
	// 落库前还要按归属过滤：Plan 给格子挑的楼不一定是房号本轮真正的归属楼
	// （靠后的楼报首见楼的格子时就是这种情况），不挡就会把明细算到错的楼上。
	cellInputs = attributedCellInputs(cellInputs, attribution, &progress)
	persistedCells, err := deps.Tasks.EnsureCellDetailTasks(ctx, runID, cellInputs, nowIn(deps.Clock))
	if err != nil {
		return progress, err
	}
	// 同样在 Ensure 成功之后推进：即使本段没有任务，阶段也如实反映
	// 「矩阵已结束、明细尚未完成」。续跑已经过了这一段时跳过。
	if phaseBefore(current, PhaseCellDetail) {
		if err := AdvancePhase(ctx, deps.Phases, runID, PhaseCellDetail, deps.Clock); err != nil {
			return progress, err
		}
	}
	if err := executeTasks(ctx, deps, runID, persistedCells, attribution, &progress); err != nil {
		return progress, err
	}

	return progress, nil
}

// executeTasks 按库里的顺序执行一段任务，累加计数。
// 周矩阵任务额外做归属裁决与周结果落库；占用说明任务只置 done。
func executeTasks(
	ctx context.Context,
	deps RunDeps,
	runID int64,
	tasks []store.SyncTask,
	attribution *Attribution,
	progress *RunProgress,
) error {
	for _, task := range tasks {
		// 每条任务开始前都看一次取消信号：被中断时保持当前 pending，续跑从它继续。
		if err := ctx.Err(); err != nil {
			return err
		}

		switch task.Status {
		case store.SyncTaskDone:
			progress.SkippedDone++
			continue
		case store.SyncTaskFailed:
			progress.SkippedFailed++
			continue
		}

		fetched, fetchErr := deps.Fetch(ctx, taskFromStore(task))
		// 调用方取消/超时不是任务自身失败：不写 failed，避免续跑丢掉一条本可重做的任务。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		now := nowIn(deps.Clock)
		if fetchErr != nil {
			if err := failTask(ctx, deps, runID, task, fetchErr, now); err != nil {
				return err
			}
			progress.Failed++
			continue
		}

		// collect_run_week 只收周矩阵结果，记抓取时刻与页面哈希：只有周矩阵正文由
		// 执行循环哈希。正文的解析、清洗与明细落库都在 TaskFetcher 内完成，占用说明
		// 的正文由 TaskFetcher 自行处理或丢弃，这里不回读、不落库。
		// 占用说明没有「楼 × 周」行，不写周结果也不参与归属。
		if TaskKind(task.Kind) == TaskWeekMatrix {
			if err := attributeWeekResponse(ctx, deps, runID, task, fetched, attribution, now); err != nil {
				if !errors.Is(err, errAttributionData) {
					return err
				}
				// 坏响应只废掉这一份响应：按任务失败处理，别把整轮打挂。
				if failErr := failTask(ctx, deps, runID, task, err, now); failErr != nil {
					return failErr
				}
				progress.Failed++
				continue
			}

			fetch := store.RunWeekFetch{
				RunID:      runID,
				Week:       task.Week,
				BuildingID: task.BuildingID,
				FetchedAt:  now,
				PageSHA256: pageSHA256(fetched.Page),
			}
			if err := deps.Tasks.RecordWeekFetch(ctx, fetch); err != nil {
				return err
			}
		}

		if err := deps.Tasks.CompleteSyncTask(ctx, runID, task.TaskID, now); err != nil {
			return err
		}
		progress.Completed++
	}
	return nil
}

// failTask 把一条任务走失败路径：先按错误分层写轮次错误码，再写 ok=false 的
// 周结果（仅周矩阵），最后置 failed。占用说明没有「楼 × 周」行，只写错误码与失败状态。
//
// 错误码只写结构失败（errorCodeFor 按哨兵映射）：网络、超时这类非结构失败没有
// 对应业务码，不写。轮次只保留第一个结构失败码——可能有多条任务各自撞上结构失败，
// 轮次级记最先发生的那一个，逐条原因仍留在 sync_task 与 collect_run_week。
// 写码放在置 failed 之前：两者之间崩溃时续跑会重做这条任务并重新写码，COALESCE
// 保证不会把先到的码覆盖掉。
func failTask(ctx context.Context, deps RunDeps, runID int64, task store.SyncTask, cause error, now time.Time) error {
	message := cause.Error()
	if code := errorCodeFor(cause); code != "" {
		if err := deps.Phases.RecordRunErrorCode(ctx, runID, code, now); err != nil {
			return err
		}
	}
	if TaskKind(task.Kind) == TaskWeekMatrix {
		failure := store.RunWeekFailure{
			RunID:        runID,
			Week:         task.Week,
			BuildingID:   task.BuildingID,
			ErrorMessage: message,
		}
		if err := deps.Tasks.RecordWeekFailure(ctx, failure); err != nil {
			return err
		}
	}
	return deps.Tasks.FailSyncTask(ctx, runID, task.TaskID, message, now)
}

// seedAttribution 从 collect_run_room 读回本轮已落库的归属，重建累计器。
// 续跑时已完成楼的响应不会再被重放，不先灌入这些归属，后跑的楼就会把
// 已归属的房号当成首见并改判（blocker 场景）。读库失败是编排错误，直接上抛。
func seedAttribution(ctx context.Context, rooms RoomStore, runID int64, attribution *Attribution) error {
	existing, err := rooms.LoadRunRooms(ctx, runID)
	if err != nil {
		return err
	}
	for _, room := range existing {
		if err := attribution.Seed(room.RoomID, room.BuildingID, room.Conflict, room.ConflictBuildingID); err != nil {
			return err
		}
	}
	return nil
}

// normalizeWhitelist 规范化调用方传入的白名单：去掉楼编号两侧空白、拒绝空编号，
// 并按首次出现去重。它必须先于任何任务落库执行：非法白名单一旦先落了任务，
// 库里就留下一批永远无法用来裁决归属的周矩阵任务。
func normalizeWhitelist(whitelist []string) ([]string, error) {
	if len(whitelist) == 0 {
		return nil, nil
	}

	trimmed := make([]string, 0, len(whitelist))
	for _, buildingID := range whitelist {
		buildingID = strings.TrimSpace(buildingID)
		if buildingID == "" {
			return nil, fmt.Errorf("pipeline: 白名单里出现空的教学楼编号: %v", whitelist)
		}
		trimmed = append(trimmed, buildingID)
	}

	return uniqueBuildingIDs(trimmed), nil
}

// resolveWhitelist 用规范化后的白名单校验库里周矩阵任务的执行顺序，返回本轮
// 归属用的白名单顺序。本轮没有周矩阵任务时允许白名单为空（归属不会被使用）；
// 有任务却缺白名单、楼顺序不一致、楼块交错、或同一栋楼的周次不是升序，都是
// 编排不变量被破坏，直接报错，不退回用 task_id 猜顺序。
func resolveWhitelist(buildings []string, weekTasks []store.SyncTask) ([]string, error) {
	taskOrder, err := weekMatrixBuildingOrder(weekTasks)
	if err != nil {
		return nil, err
	}
	if len(taskOrder) == 0 {
		return buildings, nil
	}
	if len(buildings) == 0 {
		return nil, fmt.Errorf(
			"pipeline: 本轮有 %d 条周矩阵任务，但缺少白名单顺序", len(weekTasks))
	}
	if len(buildings) != len(taskOrder) {
		return nil, fmt.Errorf(
			"pipeline: 白名单顺序 %v 与库里的周矩阵任务楼顺序 %v 不一致", buildings, taskOrder)
	}
	for index := range buildings {
		if buildings[index] != taskOrder[index] {
			return nil, fmt.Errorf(
				"pipeline: 白名单顺序 %v 与库里的周矩阵任务楼顺序 %v 不一致", buildings, taskOrder)
		}
	}
	return buildings, nil
}

// weekMatrixKey 是周矩阵任务在裁决顺序上的最小身份：楼 + 周。
// 落库前（规划入参）与落库后（库里任务）都用它按同一套口径校验顺序。
type weekMatrixKey struct {
	BuildingID string
	Week       int
}

// weekMatrixBuildingOrder 按任务顺序取周矩阵任务的楼编号（去重），
// 得到库里实际会执行的裁决顺序。
// 它只用于校验，不参与归属计算。
func weekMatrixBuildingOrder(tasks []store.SyncTask) ([]string, error) {
	keys := make([]weekMatrixKey, 0, len(tasks))
	for _, task := range tasks {
		if TaskKind(task.Kind) != TaskWeekMatrix {
			continue
		}
		keys = append(keys, weekMatrixKey{BuildingID: task.BuildingID, Week: task.Week})
	}
	return checkWeekMatrixOrder(keys)
}

// checkWeekInputOrder 在落库前用与库里任务同一套口径校验规划里的周矩阵段：
// 楼块不交错、楼内周次严格升序，并返回去重后的楼顺序供白名单比对。
// 楼顺序的权威来源仍是落库后的任务清单，这里返回的结果只用于前置比对。
func checkWeekInputOrder(tasks []store.SyncTaskInput) ([]string, error) {
	keys := make([]weekMatrixKey, 0, len(tasks))
	for _, task := range tasks {
		if TaskKind(task.Kind) != TaskWeekMatrix {
			continue
		}
		keys = append(keys, weekMatrixKey{BuildingID: task.BuildingID, Week: task.Week})
	}
	return checkWeekMatrixOrder(keys)
}

// checkPlannedWhitelist 校验规划里的周矩阵楼序列与规范化后的白名单完全一致。
// planned 为空时跳过：本轮没有周矩阵任务，白名单不参与归属裁决（续跑仍可能传入）。
// 不一致直接报错：规划里的楼顺序就是落库后的执行顺序，与白名单错配会让「首个命中」
// 落在错的楼上，归属与冲突计数都会跟着错。
func checkPlannedWhitelist(plannedBuildings, buildings []string) error {
	if len(plannedBuildings) == 0 {
		return nil
	}
	mismatch := len(plannedBuildings) != len(buildings)
	for index := 0; !mismatch && index < len(plannedBuildings); index++ {
		mismatch = plannedBuildings[index] != buildings[index]
	}
	if mismatch {
		return fmt.Errorf(
			"pipeline: 规划里的周矩阵楼顺序 %v 与白名单顺序 %v 不一致", plannedBuildings, buildings)
	}
	return nil
}

// checkWeekMatrixOrder 校验「白名单顺序 × 周次升序」的编排不变量，返回楼顺序（去重）。
//
// 两条规则：
//   - 同一栋楼的周次必须严格升序，因为「首个命中」是按周逐份响应推进的；
//   - 同一栋楼的周次必须连成一块（楼块不交错）。A1,B1,A2 这种顺序意味着 A 的
//     第二周排在 B 之后执行，B 会抢走本该由更靠前的 A 首见的房号，与
//     「白名单顺序优先」的口径冲突。
func checkWeekMatrixOrder(keys []weekMatrixKey) ([]string, error) {
	buildings := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	lastWeek := make(map[string]int, len(keys))
	lastBuilding := ""
	for _, key := range keys {
		if previous, ok := lastWeek[key.BuildingID]; ok {
			if key.Week <= previous {
				return nil, fmt.Errorf(
					"pipeline: 楼 %s 的周矩阵任务周次不是升序（第 %d 周之后出现第 %d 周）",
					key.BuildingID, previous, key.Week)
			}
			if key.BuildingID != lastBuilding {
				return nil, fmt.Errorf(
					"pipeline: 楼 %s 的周矩阵任务与楼 %s 交错（楼 %s 的第 %d 周出现在楼 %s 之后）",
					key.BuildingID, lastBuilding, key.BuildingID, key.Week, lastBuilding)
			}
		}
		lastWeek[key.BuildingID] = key.Week
		lastBuilding = key.BuildingID
		if _, ok := seen[key.BuildingID]; ok {
			continue
		}
		seen[key.BuildingID] = struct{}{}
		buildings = append(buildings, key.BuildingID)
	}
	return buildings, nil
}

// attributedCellInputs 只保留本轮归属能验证的占用说明任务：任务里的楼必须等于
// Attribution.BuildingOf(房号) 的归属楼。房号已归别的楼、或本轮还没有归属时，
// 整条任务丢弃——不落库、不执行，并计入 progress.Failed（细分见 DroppedTasks）。
//
// 挡的是 Plan 的盲区：规划只看得到「哪个格不可用」，不知道这个房号最终归谁；
// 靠后的楼报了首见楼的格子时，规划出来的明细会带着非归属楼，真去下钻就会把
// 明细算到错的楼上（Q10 首见 r101、Q11 也报 r101 且该格不可用就是这种）。
// 口径：丢弃任务不写 sync_task、不写 collect_run_week，也不给
// collect_run.attr_conflict_count 加数（那是房号级冲突，两者不能混算）；但它同样
// 是一条没拿到的明细，必须计入 Failed 并随 RunProgress.Dropped() 参与收口，
// 否则明细全被丢弃的一轮会静默收口成 success。
func attributedCellInputs(cellInputs []store.SyncTaskInput, attribution *Attribution, progress *RunProgress) []store.SyncTaskInput {
	if len(cellInputs) == 0 {
		return nil
	}

	kept := make([]store.SyncTaskInput, 0, len(cellInputs))
	for _, input := range cellInputs {
		owner, ok := attribution.BuildingOf(strings.TrimSpace(input.RoomID))
		if !ok || owner != strings.TrimSpace(input.BuildingID) {
			progress.markDropped(input.Required)
			continue
		}
		kept = append(kept, input)
	}
	return kept
}

// attributeWeekResponse 把一份周矩阵响应的房号并入本轮归属并落库：
// 首见或同楼复现的房号写 room 与 collect_run_room，跨楼重复的房号整行丢弃、
// 只给有效归属那一行打冲突标记；只有出现新房号冲突时才回写一次轮次冲突数。
// 写在周结果与任务状态之前，失败时任务保持 pending，续跑重做会用同一份响应
// 再算一次（Upsert 与 GREATEST 都是幂等的）。
//
// 返回的错误里，坏响应/晚到楼包着 errAttributionData，调用方按任务失败处理；
// 仓储写入失败不包它，直接中止整轮。
func attributeWeekResponse(
	ctx context.Context,
	deps RunDeps,
	runID int64,
	task store.SyncTask,
	fetched TaskFetch,
	attribution *Attribution,
	now time.Time,
) error {
	offer, err := attribution.Offer(task.BuildingID, fetched.Rooms)
	if err != nil {
		return err
	}

	if len(offer.Accepted) > 0 {
		rooms := store.RoomAttribution{
			BuildingID: task.BuildingID,
			Rooms:      attributedRooms(offer.Accepted),
		}
		if err := deps.Rooms.UpsertRoomAttribution(ctx, rooms); err != nil {
			return err
		}
	}

	// 一次写入本份响应的全部归属变化：Accepted 写有效归属，Conflicts 给已有的
	// 有效归属行打冲突标记（同一房号一轮只记一次）。两批房号不会重叠。
	runRooms := make([]store.RunRoom, 0, len(offer.Accepted)+len(offer.Conflicts))
	for _, room := range offer.Accepted {
		runRooms = append(runRooms, store.RunRoom{
			RoomID:     room.RoomID,
			BuildingID: task.BuildingID,
		})
	}
	for _, conflict := range offer.Conflicts {
		runRooms = append(runRooms, store.RunRoom{
			RoomID:             conflict.RoomID,
			BuildingID:         conflict.KeptBuildingID,
			Conflict:           true,
			ConflictBuildingID: conflict.DroppedBuildingID,
		})
	}
	if len(runRooms) > 0 {
		if err := deps.Rooms.UpsertRunRooms(ctx, runID, runRooms, now); err != nil {
			return err
		}
	}

	// 同一房号一轮只记一次：ConflictCount 已按房号去重，这里只在出现新冲突时写。
	// 续跑时 Seed 会把已有冲突数带上，GREATEST 不会把计数写小。
	if len(offer.Conflicts) == 0 {
		return nil
	}
	return deps.Rooms.RecordAttrConflicts(ctx, runID, attribution.ConflictCount(), now)
}

// attributedRooms 把已裁决归属的房号行映射成 store 的落库入参：
// 展示名走 clean.RoomName，原始名原样保留（原 spec Requirement 5.2）。
// 展示名为空时回退原始名、再回退房号，保证 room.name 不写空串。
func attributedRooms(rooms []model.MatrixRoom) []store.AttributedRoom {
	if len(rooms) == 0 {
		return nil
	}
	attributed := make([]store.AttributedRoom, len(rooms))
	for index, room := range rooms {
		name := clean.RoomName(room.NameRaw)
		if name == "" {
			name = strings.TrimSpace(room.NameRaw)
		}
		if name == "" {
			name = room.RoomID
		}
		attributed[index] = store.AttributedRoom{
			RoomID:  room.RoomID,
			Name:    name,
			NameRaw: room.NameRaw,
		}
	}
	return attributed
}

// pageSHA256 返回页面正文的 sha256 十六进制摘要。空正文也照算，
// 不用空串冒充哈希：抓没抓到由 TaskFetcher 的 error 表达。
func pageSHA256(page []byte) string {
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:])
}

// splitTaskInputs 把规划结果按两段落库拆开，保持各自段内的原始顺序。
// 未知任务种类直接报错：两段 Ensure 只认自己的 kind，混进别的种类说明
// 规划与执行口径不一致，不能静默丢任务。
func splitTaskInputs(tasks []Task) (week, cell []store.SyncTaskInput, err error) {
	for _, task := range tasks {
		input := store.SyncTaskInput{
			Kind:       string(task.Kind),
			Required:   task.Required,
			BuildingID: task.BuildingID,
			Week:       task.Week,
			RoomID:     task.RoomID,
			Weekday:    task.Weekday,
			Block:      task.Block,
		}
		switch task.Kind {
		case TaskWeekMatrix:
			week = append(week, input)
		case TaskCellDetail:
			cell = append(cell, input)
		default:
			return nil, nil, fmt.Errorf("pipeline: 未知任务种类 %q", task.Kind)
		}
	}
	return week, cell, nil
}

// taskFromStore 把库里的任务还原成执行器入参；以库里的种类与必需性为准，
// 避免重算的规划与已落库任务不一致时执行错语义。
func taskFromStore(task store.SyncTask) Task {
	return Task{
		Kind:       TaskKind(task.Kind),
		Required:   task.Required,
		BuildingID: task.BuildingID,
		Week:       task.Week,
		RoomID:     task.RoomID,
		Weekday:    task.Weekday,
		Block:      task.Block,
	}
}
