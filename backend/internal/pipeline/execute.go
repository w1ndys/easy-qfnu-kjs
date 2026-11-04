package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// TaskFetcher 执行一条同步任务：请求上游、解析正文、清洗并落明细都由注入的
// 具体实现完成。返回值只是本任务抓到的页面正文；执行循环只对周矩阵正文算 sha256
// （判断上游是否变化），占用说明任务的正文由 TaskFetcher 自行处理或丢弃——
// 执行循环不解析它、不回读它，也不把它落库。
// 返回错误表示该任务失败；ctx 被调用方取消时执行循环不会把它当任务失败。
type TaskFetcher func(ctx context.Context, task Task) ([]byte, error)

// TaskStore 是执行循环需要的最小仓储能力，由 store.SyncTaskRepo 实现。
// pipeline 只调这五个方法，不接触 pgx、SQL 与 sync_task 的列细节
// （依赖方向见 backend/.golangci.yml）。
type TaskStore interface {
	// EnsureSyncTasks 写入本轮规划任务并返回库里的权威任务清单（按执行顺序）。
	EnsureSyncTasks(ctx context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error)
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

// RunDeps 是执行一轮采集所需的注入项：任务仓储、任务执行函数与时钟。
type RunDeps struct {
	Tasks TaskStore   // 任务与周结果的仓储
	Fetch TaskFetcher // 一条任务的执行函数
	Clock Clock       // 完成时刻来源；nil 退回 time.Now
}

// RunProgress 是一轮执行循环的计数，供后续失败分层与阶段推进使用。
// 各字段互斥：SkippedDone/SkippedFailed 是续跑跳过，Completed/Failed 是本轮实际执行结果。
type RunProgress struct {
	Completed     int // 本轮执行成功并置 done 的任务数
	Failed        int // 本轮执行失败并置 failed 的任务数
	SkippedDone   int // 续跑时跳过的已完成任务数
	SkippedFailed int // 续跑时保留失败、不重试的任务数
}

// Run 执行一轮已规划的任务（本 spec Requirement 8.4、原 spec Requirement 11.1–11.2）：
//
//   - 先把规划任务落库；本轮已有任务时以库里的任务与状态为准，不重复写；
//   - 按 task_id 顺序从第一个 pending 继续，done 直接跳过，failed 保持原状不重试；
//   - 每条 pending 任务调用注入的 TaskFetcher 取页面正文；只有周矩阵正文由执行循环
//     哈希，占用说明正文由 TaskFetcher 自行处理或丢弃，执行循环不解析、不回读；
//   - 周矩阵成功后先写 collect_run_week（fetched_at + sha256），再置任务 done
//     并回写轮次完成计数；周矩阵失败先写同一唯一键的 ok=false 行与原因，再置 failed；
//     占用说明任务只置 done 与计数；
//   - 任务失败置 failed 后继续下一条，是否让整轮失败由任务 9 的失败分层决定；
//   - 调用方取消或超时不把当前任务记成 failed，留给下一次续跑。
//
// 落库顺序故意是「先写周结果、再改任务状态」：两者之间崩溃时任务仍是 pending，
// 续跑会重做并覆盖周结果，不会出现任务已完成却缺周结果、或已失败却留着成功行的状态。
func Run(ctx context.Context, runID int64, planned []Task, deps RunDeps) (RunProgress, error) {
	if deps.Tasks == nil {
		return RunProgress{}, errors.New("pipeline: 缺少任务仓储")
	}
	if deps.Fetch == nil {
		return RunProgress{}, errors.New("pipeline: 缺少任务执行函数")
	}

	persisted, err := deps.Tasks.EnsureSyncTasks(ctx, runID, taskInputs(planned), nowIn(deps.Clock))
	if err != nil {
		return RunProgress{}, err
	}

	var progress RunProgress
	for _, task := range persisted {
		// 每条任务开始前都看一次取消信号：被中断时保持当前 pending，续跑从它继续。
		if err := ctx.Err(); err != nil {
			return progress, err
		}

		switch task.Status {
		case store.SyncTaskDone:
			progress.SkippedDone++
			continue
		case store.SyncTaskFailed:
			progress.SkippedFailed++
			continue
		}

		page, fetchErr := deps.Fetch(ctx, taskFromStore(task))
		// 调用方取消/超时不是任务自身失败：不写 failed，避免续跑丢掉一条本可重做的任务。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return progress, ctxErr
		}

		now := nowIn(deps.Clock)
		if fetchErr != nil {
			// 周矩阵失败也留一行 ok=false：与成功行同一唯一键，续跑重做成功后覆盖。
			// 先写周结果再置 failed，崩溃时任务仍是 pending，重做不会丢状态。
			if task.Kind == string(TaskWeekMatrix) {
				failure := store.RunWeekFailure{
					RunID:        runID,
					Week:         task.Week,
					BuildingID:   task.BuildingID,
					ErrorMessage: fetchErr.Error(),
				}
				if err := deps.Tasks.RecordWeekFailure(ctx, failure); err != nil {
					return progress, err
				}
			}
			if err := deps.Tasks.FailSyncTask(ctx, runID, task.TaskID, fetchErr.Error(), now); err != nil {
				return progress, err
			}
			progress.Failed++
			continue
		}

		// collect_run_week 只收周矩阵结果，记抓取时刻与页面哈希：只有周矩阵正文由
		// 执行循环哈希。正文的解析、清洗与明细落库都在 TaskFetcher 内完成，占用说明
		// 的正文由 TaskFetcher 自行处理或丢弃，这里不回读、不落库。
		// 占用说明没有「楼 × 周」行，只置 done。
		if task.Kind == string(TaskWeekMatrix) {
			fetch := store.RunWeekFetch{
				RunID:      runID,
				Week:       task.Week,
				BuildingID: task.BuildingID,
				FetchedAt:  now,
				PageSHA256: pageSHA256(page),
			}
			if err := deps.Tasks.RecordWeekFetch(ctx, fetch); err != nil {
				return progress, err
			}
		}

		if err := deps.Tasks.CompleteSyncTask(ctx, runID, task.TaskID, now); err != nil {
			return progress, err
		}
		progress.Completed++
	}

	return progress, nil
}

// pageSHA256 返回页面正文的 sha256 十六进制摘要。空正文也照算，
// 不用空串冒充哈希：抓没抓到由 TaskFetcher 的 error 表达。
func pageSHA256(page []byte) string {
	sum := sha256.Sum256(page)
	return hex.EncodeToString(sum[:])
}

// taskInputs 把 pipeline 的规划结果映射成 store 的落库入参。
func taskInputs(tasks []Task) []store.SyncTaskInput {
	if len(tasks) == 0 {
		return nil
	}
	inputs := make([]store.SyncTaskInput, len(tasks))
	for index, task := range tasks {
		inputs[index] = store.SyncTaskInput{
			Kind:       string(task.Kind),
			Required:   task.Required,
			BuildingID: task.BuildingID,
			Week:       task.Week,
			RoomID:     task.RoomID,
			Weekday:    task.Weekday,
			Block:      task.Block,
		}
	}
	return inputs
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
