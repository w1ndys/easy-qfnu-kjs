// 本文件属于 business 层：一轮全量的任务状态机。
// 任务粒度是「楼 + 周矩阵」与「楼 + 房间 + 格子明细」，落在 sync_task；
// 已完成的任务不重打，会话失效先重新登录再从第一个未完成任务继续。
// 依据 specs/collector-full-sync/design.md 的 Components、Error Handling 与
// requirements.md 的 5.1、5.2、5.3、5.6，以及 docs/decisions/2026-10-09-saturday-full-sync.md。
// 请求、清洗与入库由入口串好之后以 Worker 注入，本层不发 HTTP、不解析正文。

package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// Phase 是本轮同步的阶段名，取值与任务种类一致，面板据此显示当前在做哪一步。
type Phase string

const (
	// PhaseWeekMatrix 是按楼逐周请求周矩阵的阶段
	PhaseWeekMatrix Phase = "week_matrix"
	// PhaseCellDetail 是下钻占用明细的阶段
	PhaseCellDetail Phase = "cell_detail"
	// PhaseFinished 是本轮任务已经跑完
	PhaseFinished Phase = "finished"
)

// Progress 是一轮同步的进度：阶段、完成数、总数、最近错误与更新时间。
// 完成数与总数来自 sync_task 的统计；阶段与更新时间记在本层，落库还需要一个迁移。
type Progress struct {
	Phase     Phase     // 当前阶段
	Done      int       // 已完成任务数
	Total     int       // 本轮任务总数，明细任务补进来后会变大
	LastError string    // 最近一个任务的错误，还没有失败时为空
	UpdatedAt time.Time // 最近一次进度更新的时间
}

// ErrRunInProgress 表示已经有一轮 running：同一时刻至多一轮，拒绝开始第二轮。
var ErrRunInProgress = store.ErrRunInProgress

// ErrEmptyWhitelist 表示白名单为空：本轮标 blocked，不请求教室状态，也不退回全校请求。
var ErrEmptyWhitelist = errors.New("教学楼白名单为空")

// ErrNoWeeks 表示周历为空：本轮标 failed，不建任务也不请求上游。
var ErrNoWeeks = errors.New("周历为空，无法确定要采的周次")

// ErrTasksFailed 表示本轮有任务在重试耗尽后失败：这些任务记为 failed，其余任务照跑。
var ErrTasksFailed = errors.New("本轮有任务失败")

// StopError 包装「整轮停止」类失败：表头块集合变化、同一 jsbh 出现在两栋楼等。
// 这类失败不再继续本轮，由执行器用 StopRun 包起来返回，入口据此告警并且不发布。
type StopError struct {
	Err error // 触发整轮停止的原因
}

// Error 实现 error 接口。
func (e StopError) Error() string { return "整轮停止: " + e.Err.Error() }

// Unwrap 让调用方仍能用 errors.Is 判断具体原因。
func (e StopError) Unwrap() error { return e.Err }

// StopRun 把整轮停止类失败包起来，供执行器返回给状态机。
func StopRun(err error) error { return StopError{Err: err} }

// TaskStore 是本层需要的存储能力，实现在 internal/store；测试用内存实现替换。
type TaskStore interface {
	// StartSyncRun 开启一轮采集，已经有一轮 running 时返回 ErrRunInProgress
	StartSyncRun(ctx context.Context, term string) (int64, error)
	// BlockSyncRun 把一轮标成 blocked 并写下原因
	BlockSyncRun(ctx context.Context, runID int64, reason string) error
	// FinishSyncRun 结束一轮，写下最终状态与最近错误
	FinishSyncRun(ctx context.Context, runID int64, status string, lastError string) error
	// BuildingWhitelist 读教学楼白名单，空名单要拒绝开始本轮
	BuildingWhitelist(ctx context.Context) ([]store.Building, error)
	// EnqueueSyncTasks 写入本轮任务，已存在的任务跳过
	EnqueueSyncTasks(ctx context.Context, runID int64, tasks []store.SyncTask) error
	// NextPendingSyncTask 取第一个待办任务，没有时 found 为 false
	NextPendingSyncTask(ctx context.Context, runID int64) (store.SyncTask, bool, error)
	// MarkSyncTaskDone 把一个任务标成完成
	MarkSyncTaskDone(ctx context.Context, taskID int64, attempts int) error
	// MarkSyncTaskFailed 把一个任务标成失败，并记下请求次数与错误
	MarkSyncTaskFailed(ctx context.Context, taskID int64, attempts int, message string) error
	// SyncProgress 统计本轮的完成数与总数
	SyncProgress(ctx context.Context, runID int64) (int, int, error)
}

// Session 是上游会话：状态机只在会话失效时重新登录，再从第一个待办任务继续。
// 真实 CAS 登录不在本层，测试注入假会话。
type Session interface {
	// Login 登录并刷新会话
	Login(ctx context.Context) error
}

// Worker 执行单个同步任务：请求、清洗与入库由入口串好之后注入。
type Worker interface {
	// RunTask 执行一个任务，返回本次上游请求的次数（含重试）；
	// 会话失效返回 fetch.ErrSessionExpired，整轮停止类失败用 StopRun 包起来
	RunTask(ctx context.Context, task store.SyncTask) (int, error)
}

// Runner 是一轮全量的任务状态机。
type Runner struct {
	store    TaskStore        // 任务表与轮次状态
	session  Session          // 上游会话，会话失效时重新登录
	worker   Worker           // 单个任务的执行者
	now      func() time.Time // 时钟，测试可注入固定时间
	progress Progress         // 当前进度，Run 与 Resume 期间更新
}

// NewRunner 组装状态机；now 可注入，便于测试固定更新时间。
func NewRunner(taskStore TaskStore, session Session, worker Worker, now func() time.Time) *Runner {
	// 调用方没给时钟就用系统时间，生产路径走这里
	if now == nil {
		now = time.Now
	}
	return &Runner{store: taskStore, session: session, worker: worker, now: now}
}

// Progress 返回当前进度的副本。
// 进度在 Run 所在 goroutine 里更新，读方在别的 goroutine 展示时自行同步。
func (r *Runner) Progress() Progress { return r.progress }

// Run 跑一轮全量：开轮次、读白名单、按周历建周矩阵任务，再把待办任务逐个跑完。
// term 必须是已经写入 term 表的学期（周历采样先落库），weeks 是第 1 周到总周数的周次。
// 已经有一轮 running 时返回 ErrRunInProgress；白名单为空时标 blocked 并返回 ErrEmptyWhitelist。
func (r *Runner) Run(ctx context.Context, term string, weeks []int) error {
	runID, err := r.store.StartSyncRun(ctx, term)
	// 已经有一轮在跑：拒绝开始第二轮，不建任务也不发上游请求
	if err != nil {
		return err
	}
	r.progress = Progress{Phase: PhaseWeekMatrix, UpdatedAt: r.now()}

	// 周历为空说明采样没成功，本轮不建任务也不请求上游（需求 1.4）
	if len(weeks) == 0 {
		return r.failRun(ctx, runID, ErrNoWeeks)
	}

	buildings, err := r.store.BuildingWhitelist(ctx)
	if err != nil {
		return r.failRun(ctx, runID, err)
	}
	// 白名单为空：标 blocked，不请求教室状态，也不退回全校请求（需求 2.3）
	if len(buildings) == 0 {
		return r.blockRun(ctx, runID, ErrEmptyWhitelist)
	}

	// 任务清单是「楼 × 周」：每栋白名单楼的每一周各一个周矩阵任务（需求 3.1）
	if err := r.store.EnqueueSyncTasks(ctx, runID, weekMatrixTasks(buildings, weeks)); err != nil {
		return r.failRun(ctx, runID, err)
	}
	return r.Resume(ctx, runID)
}

// Resume 从第一个待办任务继续跑完本轮：任务清单已经在库里，重复建会被跳过。
// 明细任务是执行期间才写进来的，所以循环每轮都重新取待办，不预先取清单。
func (r *Runner) Resume(ctx context.Context, runID int64) error {
	if err := r.runPending(ctx, runID); err != nil {
		// 整轮停止或存储故障：本轮标 failed，已完成的任务继续留在库里
		return r.failRun(ctx, runID, err)
	}
	return r.finishRun(ctx, runID)
}

// runPending 按建立顺序执行待办任务，直到本轮再没有待办任务。
func (r *Runner) runPending(ctx context.Context, runID int64) error {
	for {
		task, found, err := r.store.NextPendingSyncTask(ctx, runID)
		if err != nil {
			return err
		}
		// 没有待办任务说明本轮已经跑完
		if !found {
			return nil
		}
		if err := r.runTask(ctx, runID, task); err != nil {
			return err
		}
	}
}

// runTask 执行一个任务：会话失效先重新登录再重跑，重试耗尽只标记该任务失败并继续。
// 只有整轮停止类失败会作为错误返回，交给调用方结束本轮。
func (r *Runner) runTask(ctx context.Context, runID int64, task store.SyncTask) error {
	attempts, err := r.executeTask(ctx, task)
	if err == nil {
		if err := r.store.MarkSyncTaskDone(ctx, task.ID, attempts); err != nil {
			return err
		}
		return r.updateProgress(ctx, runID, task, nil)
	}

	var stop StopError
	// 表头块集合变化这类失败说明这一轮的解析前提没了，剩余任务不再继续（设计 Error Handling）
	if errors.As(err, &stop) {
		return err
	}

	// 单个任务重试耗尽：只标记它失败，其余任务继续跑（需求 3.4）
	if err := r.store.MarkSyncTaskFailed(ctx, task.ID, attempts, err.Error()); err != nil {
		return err
	}
	return r.updateProgress(ctx, runID, task, err)
}

// executeTask 执行任务，并在会话失效时先重新登录再跑同一个任务（需求 5.3）。
// 重新登录后仍失败就按普通失败返回，由调用方只标记该任务。
func (r *Runner) executeTask(ctx context.Context, task store.SyncTask) (int, error) {
	attempts, err := r.worker.RunTask(ctx, task)
	// 会话失效与任务本身无关：登录后重跑，已完成的其它任务不受影响
	if errors.Is(err, fetch.ErrSessionExpired) {
		if loginErr := r.session.Login(ctx); loginErr != nil {
			return attempts, fmt.Errorf("重新登录失败: %w", loginErr)
		}
		return r.worker.RunTask(ctx, task)
	}
	return attempts, err
}

// updateProgress 刷新进度：完成数与总数按本轮任务统计，最近错误保留最近一次失败。
func (r *Runner) updateProgress(ctx context.Context, runID int64, task store.SyncTask, taskErr error) error {
	done, total, err := r.store.SyncProgress(ctx, runID)
	if err != nil {
		return err
	}
	r.progress.Done = done
	r.progress.Total = total
	// 阶段与任务种类同名，跟着刚跑完的任务走
	r.progress.Phase = Phase(task.Kind)
	// 后面的成功不该清掉上一条错误：面板要看的是最近一次失败
	if taskErr != nil {
		r.progress.LastError = taskErr.Error()
	}
	r.progress.UpdatedAt = r.now()
	return nil
}

// finishRun 结束本轮：还有未完成任务时标 failed，全部完成才标 success。
func (r *Runner) finishRun(ctx context.Context, runID int64) error {
	done, total, err := r.store.SyncProgress(ctx, runID)
	if err != nil {
		return err
	}
	r.progress.Done = done
	r.progress.Total = total
	r.progress.Phase = PhaseFinished
	r.progress.UpdatedAt = r.now()

	// 有任务失败或没跑完就不算成功：发布要等全部必需任务完成（见设计 Error Handling）
	if done < total {
		if err := r.store.FinishSyncRun(ctx, runID, store.RunStatusFailed, r.progress.LastError); err != nil {
			return err
		}
		return fmt.Errorf("%w: 已完成 %d/%d 个任务", ErrTasksFailed, done, total)
	}
	if err := r.store.FinishSyncRun(ctx, runID, store.RunStatusSuccess, r.progress.LastError); err != nil {
		return err
	}
	return nil
}

// failRun 把一轮标成 failed，原因进最近错误；返回原因本身供入口处置。
func (r *Runner) failRun(ctx context.Context, runID int64, reason error) error {
	if err := r.store.FinishSyncRun(ctx, runID, store.RunStatusFailed, reason.Error()); err != nil {
		return err
	}
	r.progress.Phase = PhaseFinished
	r.progress.LastError = reason.Error()
	r.progress.UpdatedAt = r.now()
	return reason
}

// blockRun 把一轮标成 blocked：白名单为空时不发业务请求，只留这条记录（需求 2.3）。
func (r *Runner) blockRun(ctx context.Context, runID int64, reason error) error {
	if err := r.store.BlockSyncRun(ctx, runID, reason.Error()); err != nil {
		return err
	}
	r.progress.Phase = PhaseFinished
	r.progress.LastError = reason.Error()
	r.progress.UpdatedAt = r.now()
	return reason
}

// weekMatrixTasks 生成本轮的周矩阵任务：每栋白名单楼 × 每个周次各一个。
func weekMatrixTasks(buildings []store.Building, weeks []int) []store.SyncTask {
	tasks := make([]store.SyncTask, 0, len(buildings)*len(weeks))
	for _, building := range buildings {
		for _, week := range weeks {
			tasks = append(tasks, store.SyncTask{
				Kind:       store.KindWeekMatrix,
				BuildingID: building.Jxlbh,
				Week:       week,
			})
		}
	}
	return tasks
}
