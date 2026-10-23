package sync

// 本文件是任务状态机的单元测试：存储用内存实现、会话与执行器用假实现，
// 不连数据库、不发上游请求，覆盖任务 5.1 的四项：完成任务不重打、会话失效后续跑、
// 第二轮被拒绝、空白名单不发请求，另加任务级失败隔离与整轮停止。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/fetch"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// testTerm 是测试用的学期编号：真实存储里它必须已经在 term 表里。
const testTerm = "2026-2027-1"

// fakeTask 是一条内存任务：任务本身加当前状态，状态对应 sync_task.status。
type fakeTask struct {
	task     store.SyncTask // 任务本身
	status   string         // pending / done / failed
	attempts int            // 上游请求次数
	message  string         // 失败原因
}

// fakeStore 是内存版任务表与轮次表：语义与 internal/store 的实现一致，测试不连数据库。
type fakeStore struct {
	run       *fakeRun         // 当前轮次，未开始时为空
	nextRunID int64            // 轮次自增，也是最近一轮的 id
	tasks     []fakeTask       // 本轮任务，按建立顺序
	nextID    int64            // 任务自增
	buildings []store.Building // 白名单
}

// fakeRun 是一轮的轮次记录：状态与最近错误。
type fakeRun struct {
	status    string // running / success / failed / blocked
	lastError string // 最近错误，与 collect_run.error_message 同义
}

// StartSyncRun 开一轮；已经有一轮 running 时与真实存储一样返回 ErrRunInProgress。
func (s *fakeStore) StartSyncRun(ctx context.Context, term string) (int64, error) {
	// 同一时刻至多一轮：已有 running 时拒绝，也不清掉已有任务
	if s.run != nil && s.run.status == store.RunStatusRunning {
		return 0, store.ErrRunInProgress
	}
	s.nextRunID++
	s.run = &fakeRun{status: store.RunStatusRunning}
	s.tasks = nil
	return s.nextRunID, nil
}

// BlockSyncRun 把本轮标成 blocked 并记下原因。
func (s *fakeStore) BlockSyncRun(ctx context.Context, runID int64, reason string) error {
	s.run.status = store.RunStatusBlocked
	s.run.lastError = reason
	return nil
}

// FinishSyncRun 结束本轮，写下状态与最近错误。
func (s *fakeStore) FinishSyncRun(ctx context.Context, runID int64, status string, lastError string) error {
	s.run.status = status
	s.run.lastError = lastError
	return nil
}

// BuildingWhitelist 返回测试配置的白名单。
func (s *fakeStore) BuildingWhitelist(ctx context.Context) ([]store.Building, error) {
	return s.buildings, nil
}

// EnqueueSyncTasks 写入任务：同一轮里同样的任务只留一条，与唯一索引的行为一致。
func (s *fakeStore) EnqueueSyncTasks(ctx context.Context, runID int64, tasks []store.SyncTask) error {
	for _, task := range tasks {
		// 续跑时会重新建一次清单，重复的单元不能再建一条
		if s.hasTask(task) {
			continue
		}
		s.nextID++
		task.ID = s.nextID
		s.tasks = append(s.tasks, fakeTask{task: task, status: "pending"})
	}
	return nil
}

// NextPendingSyncTask 取第一条待办任务：done 与 failed 的都不再取。
func (s *fakeStore) NextPendingSyncTask(ctx context.Context, runID int64) (store.SyncTask, bool, error) {
	for _, item := range s.tasks {
		if item.status == "pending" {
			return item.task, true, nil
		}
	}
	return store.SyncTask{}, false, nil
}

// MarkSyncTaskDone 把任务标成完成。
func (s *fakeStore) MarkSyncTaskDone(ctx context.Context, taskID int64, attempts int) error {
	item := s.find(taskID)
	item.status = "done"
	item.attempts = attempts
	return nil
}

// MarkSyncTaskFailed 把任务标成失败并记下请求次数与错误。
func (s *fakeStore) MarkSyncTaskFailed(ctx context.Context, taskID int64, attempts int, message string) error {
	item := s.find(taskID)
	item.status = "failed"
	item.attempts = attempts
	item.message = message
	return nil
}

// SyncProgress 统计完成数与总数。
func (s *fakeStore) SyncProgress(ctx context.Context, runID int64) (int, int, error) {
	done := 0
	for _, item := range s.tasks {
		if item.status == "done" {
			done++
		}
	}
	return done, len(s.tasks), nil
}

// hasTask 判断本轮是否已经有同一个任务单元。
func (s *fakeStore) hasTask(task store.SyncTask) bool {
	for _, item := range s.tasks {
		if item.task.Kind == task.Kind && item.task.BuildingID == task.BuildingID &&
			item.task.Week == task.Week && item.task.RoomID == task.RoomID &&
			item.task.Weekday == task.Weekday && item.task.Block == task.Block {
			return true
		}
	}
	return false
}

// find 按任务 id 找任务；测试里 id 一定存在，找不到就直接报错。
func (s *fakeStore) find(taskID int64) *fakeTask {
	for index := range s.tasks {
		if s.tasks[index].task.ID == taskID {
			return &s.tasks[index]
		}
	}
	panic(fmt.Sprintf("测试任务 %d 不存在", taskID))
}

// statuses 按建立顺序返回任务状态，方便断言哪些任务被标记成失败。
func (s *fakeStore) statuses() []string {
	statuses := make([]string, 0, len(s.tasks))
	for _, item := range s.tasks {
		statuses = append(statuses, item.status)
	}
	return statuses
}

// fakeSession 是假会话：只记登录次数，真实 CAS 不在本任务里。
type fakeSession struct {
	logins int // 登录次数
}

// Login 记一次重新登录。
func (s *fakeSession) Login(ctx context.Context) error {
	s.logins++
	return nil
}

// fakeWorker 是假执行器：按调用序号给出结果，并记下被执行的顺序。
type fakeWorker struct {
	calls []store.SyncTask // 被执行的顺序：同一个任务被重跑会出现两次
	errs  []error          // 按调用序号给出的错误，缺省表示成功
}

// RunTask 记一次执行，并按序号返回预设结果。
func (w *fakeWorker) RunTask(ctx context.Context, task store.SyncTask) (int, error) {
	w.calls = append(w.calls, task)
	// 已经用完了预设结果就表示成功
	if index := len(w.calls) - 1; index < len(w.errs) && w.errs[index] != nil {
		return 1, w.errs[index]
	}
	return 1, nil
}

// weekMatrixTask 拼一个周矩阵任务，供断言期望的任务单元。
func weekMatrixTask(buildingID string, week int) store.SyncTask {
	return store.SyncTask{Kind: store.KindWeekMatrix, BuildingID: buildingID, Week: week}
}

// TestRunBuildsWeekMatrixTasksPerBuildingAndWeek 断言一轮的任务清单是「楼 × 周」，
// 执行顺序按建立顺序，跑完后进度是完成数等于总数（需求 3.1、5.1）。
func TestRunBuildsWeekMatrixTasksPerBuildingAndWeek(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}, {Jxlbh: "jxlbh-2"}}}
	worker := &fakeWorker{}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	if err := runner.Run(context.Background(), testTerm, []int{1, 2, 3}); err != nil {
		t.Fatalf("跑一轮失败: %v", err)
	}

	expected := []store.SyncTask{
		weekMatrixTask("jxlbh-1", 1), weekMatrixTask("jxlbh-1", 2), weekMatrixTask("jxlbh-1", 3),
		weekMatrixTask("jxlbh-2", 1), weekMatrixTask("jxlbh-2", 2), weekMatrixTask("jxlbh-2", 3),
	}
	if len(worker.calls) != len(expected) {
		t.Fatalf("执行了 %d 个任务，期望 %d 个", len(worker.calls), len(expected))
	}
	for index, want := range expected {
		if got := worker.calls[index]; got.Kind != want.Kind || got.BuildingID != want.BuildingID || got.Week != want.Week {
			t.Errorf("第 %d 个任务 = %+v，期望 %+v", index+1, got, want)
		}
	}

	if dataStore.run.status != store.RunStatusSuccess {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusSuccess)
	}
	progress := runner.Progress()
	if progress.Done != 6 || progress.Total != 6 {
		t.Errorf("进度 = %d/%d，期望 6/6", progress.Done, progress.Total)
	}
	if progress.Phase != PhaseFinished {
		t.Errorf("阶段 = %s，期望 %s", progress.Phase, PhaseFinished)
	}
	if progress.LastError != "" {
		t.Errorf("最近错误 = %q，期望为空", progress.LastError)
	}
}

// TestRunSkipsDoneTasksOnResume 覆盖任务 5.1 的「完成的任务不重打」：
// 第一轮被整轮停止打断（第 2 个任务还没定论），续跑时只跑没完成的那个。
func TestRunSkipsDoneTasksOnResume(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	// 第 1 个任务成功，第 2 个任务触发整轮停止，第 3 个任务还没开始
	first := &fakeWorker{errs: []error{nil, StopRun(errors.New("表头大节与当前节次轴不一致"))}}
	runner := NewRunner(dataStore, &fakeSession{}, first, fixedClock())

	if err := runner.Run(context.Background(), testTerm, []int{1, 2, 3}); err == nil {
		t.Fatal("整轮停止时返回了成功")
	}
	if got := dataStore.statuses(); got[0] != "done" || got[1] != "pending" || got[2] != "pending" {
		t.Fatalf("任务状态 = %v，期望 [done pending pending]", got)
	}
	if dataStore.run.status != store.RunStatusFailed {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusFailed)
	}

	// 续跑：第 2、3 个任务该被跑，第 1 个已完成的任务不能再打
	second := &fakeWorker{}
	resumed := NewRunner(dataStore, &fakeSession{}, second, fixedClock())
	if err := resumed.Resume(context.Background(), dataStore.nextRunID); err != nil {
		t.Fatalf("续跑失败: %v", err)
	}
	if len(second.calls) != 2 {
		t.Fatalf("续跑执行了 %d 个任务，期望 2 个", len(second.calls))
	}
	for index, week := range []int{2, 3} {
		if got := second.calls[index]; got.Week != week {
			t.Errorf("续跑第 %d 个任务是第 %d 周，期望第 %d 周", index+1, got.Week, week)
		}
	}
	if dataStore.run.status != store.RunStatusSuccess {
		t.Errorf("续跑后轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusSuccess)
	}
	if progress := resumed.Progress(); progress.Done != 3 || progress.Total != 3 {
		t.Errorf("续跑后进度 = %d/%d，期望 3/3", progress.Done, progress.Total)
	}
}

// TestRunDoesNotRetryFailedTaskInSameRun 断言重试耗尽的任务在本轮不再被取到：
// 它已经有结论，本轮继续跑别的任务，不再打同一个上游请求。
func TestRunDoesNotRetryFailedTaskInSameRun(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	worker := &fakeWorker{errs: []error{errors.New("上游请求发出 4 次后仍失败"), nil}}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	if err := runner.Run(context.Background(), testTerm, []int{1, 2}); !errors.Is(err, ErrTasksFailed) {
		t.Fatalf("返回 %v，期望 ErrTasksFailed", err)
	}
	if len(worker.calls) != 2 {
		t.Fatalf("执行了 %d 个任务，期望 2 个（失败的任务不重跑）", len(worker.calls))
	}
	if got := dataStore.statuses(); got[0] != "failed" || got[1] != "done" {
		t.Errorf("任务状态 = %v，期望 [failed done]", got)
	}
}

// TestRunReloginsAndContinuesAfterSessionExpired 覆盖任务 5.1 的「会话失效后续跑」：
// 重新登录后重跑同一个任务，已完成的其它任务不受影响（需求 5.3）。
func TestRunReloginsAndContinuesAfterSessionExpired(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	// 第 1 次执行说会话失效，重新登录后第 2 次成功，第 3 次是第二个任务
	worker := &fakeWorker{errs: []error{fetch.ErrSessionExpired, nil, nil}}
	session := &fakeSession{}
	runner := NewRunner(dataStore, session, worker, fixedClock())

	if err := runner.Run(context.Background(), testTerm, []int{1, 2}); err != nil {
		t.Fatalf("会话失效后应当续跑完成，实际返回: %v", err)
	}
	if session.logins != 1 {
		t.Errorf("登录次数 = %d，期望 1", session.logins)
	}
	if len(worker.calls) != 3 {
		t.Fatalf("执行了 %d 次任务，期望 3 次（失败的任务重跑一次）", len(worker.calls))
	}
	// 第一次与第二次必须是同一个任务：会话失效不影响已完成的任务，也不跳过断点
	if worker.calls[0] != worker.calls[1] {
		t.Errorf("重跑的任务 = %+v，期望与失败的任务相同 %+v", worker.calls[1], worker.calls[0])
	}
	if worker.calls[2].Week != 2 {
		t.Errorf("第三个任务 = %+v，期望第二个周矩阵任务", worker.calls[2])
	}
	if got := dataStore.statuses(); got[0] != "done" || got[1] != "done" {
		t.Errorf("任务状态 = %v，期望 [done done]", got)
	}
	if dataStore.run.status != store.RunStatusSuccess {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusSuccess)
	}
}

// TestRunRejectedWhileRunning 覆盖任务 5.1 的「running 时第二轮被拒绝」：
// 拒绝时既不建任务，也不发上游请求（需求 5.6）。
func TestRunRejectedWhileRunning(t *testing.T) {
	// 已经有一轮在跑：存储里留着这条 running 记录
	dataStore := &fakeStore{
		run:       &fakeRun{status: store.RunStatusRunning},
		buildings: []store.Building{{Jxlbh: "jxlbh-1"}},
	}
	worker := &fakeWorker{}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	err := runner.Run(context.Background(), testTerm, []int{1, 2})
	if !errors.Is(err, ErrRunInProgress) {
		t.Fatalf("返回 %v，期望 ErrRunInProgress", err)
	}
	if len(worker.calls) != 0 {
		t.Errorf("被拒绝后仍执行了 %d 个任务", len(worker.calls))
	}
	if len(dataStore.tasks) != 0 {
		t.Errorf("被拒绝后仍建了 %d 个任务", len(dataStore.tasks))
	}
	if dataStore.run.status != store.RunStatusRunning {
		t.Errorf("轮次状态 = %s，期望仍是 %s", dataStore.run.status, store.RunStatusRunning)
	}
}

// TestRunBlockedOnEmptyWhitelist 覆盖任务 5.1 的「空白名单不发请求」：
// 状态标 blocked，不建任务、不发教室状态请求，也不退回全校请求（需求 2.3）。
func TestRunBlockedOnEmptyWhitelist(t *testing.T) {
	dataStore := &fakeStore{}
	worker := &fakeWorker{}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	err := runner.Run(context.Background(), testTerm, []int{1, 2})
	if !errors.Is(err, ErrEmptyWhitelist) {
		t.Fatalf("返回 %v，期望 ErrEmptyWhitelist", err)
	}
	if dataStore.run.status != store.RunStatusBlocked {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusBlocked)
	}
	// 空白名单时一个教室状态请求都不该发出去
	if len(worker.calls) != 0 {
		t.Errorf("空白名单时执行了 %d 个任务", len(worker.calls))
	}
	if len(dataStore.tasks) != 0 {
		t.Errorf("空白名单时建了 %d 个任务", len(dataStore.tasks))
	}
	if dataStore.run.lastError == "" {
		t.Error("blocked 轮次没有记下阻断原因")
	}
}

// TestRunMarksFailedTaskOnlyAndContinues 断言单个任务重试耗尽只影响它自己，
// 其余任务照跑，本轮整体标 failed（需求 3.4 与设计 Error Handling）。
func TestRunMarksFailedTaskOnlyAndContinues(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	// 第 2 个任务失败，第 3 个任务仍要被执行
	worker := &fakeWorker{errs: []error{nil, errors.New("上游返回 500 Internal Server Error"), nil}}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	err := runner.Run(context.Background(), testTerm, []int{1, 2, 3})
	if !errors.Is(err, ErrTasksFailed) {
		t.Fatalf("返回 %v，期望 ErrTasksFailed", err)
	}
	if got := dataStore.statuses(); got[0] != "done" || got[1] != "failed" || got[2] != "done" {
		t.Errorf("任务状态 = %v，期望 [done failed done]", got)
	}
	if dataStore.run.status != store.RunStatusFailed {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusFailed)
	}

	progress := runner.Progress()
	if progress.Done != 2 || progress.Total != 3 {
		t.Errorf("进度 = %d/%d，期望 2/3", progress.Done, progress.Total)
	}
	// 最近错误要留下失败原因，后续成功不该把它清掉
	if progress.LastError == "" {
		t.Error("失败后最近错误为空")
	}
}

// TestRunStopsOnStopError 断言整轮停止类失败不再继续剩余任务，
// 该任务也不标记成失败（它还没有定论），本轮标 failed（设计 Error Handling）。
func TestRunStopsOnStopError(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	axisErr := errors.New("表头大节与当前节次轴不一致")
	worker := &fakeWorker{errs: []error{nil, StopRun(axisErr)}}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	err := runner.Run(context.Background(), testTerm, []int{1, 2, 3})
	var stop StopError
	if !errors.As(err, &stop) {
		t.Fatalf("返回 %v，期望 StopError", err)
	}
	// 包装后仍要能用 errors.Is 认出原始原因
	if !errors.Is(err, axisErr) {
		t.Errorf("返回 %v，期望能判出 %v", err, axisErr)
	}
	if len(worker.calls) != 2 {
		t.Errorf("执行了 %d 个任务，期望 2 个（第三个不再执行）", len(worker.calls))
	}
	// 触发整轮停止的任务不标失败：它还没有定论，续跑时会重来
	if got := dataStore.statuses(); got[0] != "done" || got[1] != "pending" || got[2] != "pending" {
		t.Errorf("任务状态 = %v，期望 [done pending pending]", got)
	}
	if dataStore.run.status != store.RunStatusFailed {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusFailed)
	}
}

// TestRunFailsWithoutWeeks 断言周历为空时不建任务也不请求上游，本轮标 failed（需求 1.4）。
func TestRunFailsWithoutWeeks(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	worker := &fakeWorker{}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	if err := runner.Run(context.Background(), testTerm, nil); !errors.Is(err, ErrNoWeeks) {
		t.Fatalf("返回 %v，期望 ErrNoWeeks", err)
	}
	if dataStore.run.status != store.RunStatusFailed {
		t.Errorf("轮次状态 = %s，期望 %s", dataStore.run.status, store.RunStatusFailed)
	}
	if len(worker.calls) != 0 || len(dataStore.tasks) != 0 {
		t.Errorf("周历为空时执行了 %d 个任务、建了 %d 个任务", len(worker.calls), len(dataStore.tasks))
	}
}

// TestResumePicksUpTasksEnqueuedDuringRun 断言执行期间补进来的任务会在同一轮被跑到：
// 明细任务要等聚合矩阵清洗完才知道，所以循环每轮都要重新取待办。
func TestResumePicksUpTasksEnqueuedDuringRun(t *testing.T) {
	dataStore := &fakeStore{buildings: []store.Building{{Jxlbh: "jxlbh-1"}}}
	// 执行周矩阵任务时补一个明细任务进来，它必须也被跑到
	worker := &enqueueingWorker{dataStore: dataStore}
	runner := NewRunner(dataStore, &fakeSession{}, worker, fixedClock())

	if err := runner.Run(context.Background(), testTerm, []int{1}); err != nil {
		t.Fatalf("跑一轮失败: %v", err)
	}

	detail := store.SyncTask{Kind: store.KindCellDetail, BuildingID: "jxlbh-1", RoomID: "r-1", Weekday: 3, Block: "0102"}
	if len(worker.calls) != 2 {
		t.Fatalf("执行了 %d 个任务，期望 2 个（周矩阵与补进来的明细）", len(worker.calls))
	}
	// 任务 id 由存储分配，这里只比任务单元本身
	if got := worker.calls[1]; !sameTaskUnit(got, detail) {
		t.Errorf("第二个任务 = %+v，期望 %+v", got, detail)
	}
	if progress := runner.Progress(); progress.Done != 2 || progress.Total != 2 {
		t.Errorf("进度 = %d/%d，期望 2/2", progress.Done, progress.Total)
	}
}

// enqueueingWorker 在执行周矩阵任务时补一个明细任务，模拟聚合矩阵清洗后的下钻清单。
type enqueueingWorker struct {
	dataStore *fakeStore       // 内存任务表
	calls     []store.SyncTask // 被执行的顺序，用来断言补进来的任务也被跑到
}

// RunTask 记一次执行；执行周矩阵任务时补一个明细任务，其余任务按成功返回。
func (w *enqueueingWorker) RunTask(ctx context.Context, task store.SyncTask) (int, error) {
	w.calls = append(w.calls, task)
	// 只补一次：明细任务自己也走这里
	if task.Kind == store.KindWeekMatrix {
		detail := store.SyncTask{
			Kind:       store.KindCellDetail,
			BuildingID: task.BuildingID,
			RoomID:     "r-1",
			Weekday:    3,
			Block:      "0102",
		}
		if err := w.dataStore.EnqueueSyncTasks(ctx, w.dataStore.nextRunID, []store.SyncTask{detail}); err != nil {
			return 0, err
		}
	}
	return 1, nil
}

// fixedClock 返回固定时间的时钟，避免进度里的更新时间随测试运行漂移。
func fixedClock() func() time.Time {
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return at }
}

// sameTaskUnit 比较两个任务单元是否同一件事：任务 id 由存储分配，不参与比较。
func sameTaskUnit(left, right store.SyncTask) bool {
	return left.Kind == right.Kind && left.BuildingID == right.BuildingID &&
		left.Week == right.Week && left.RoomID == right.RoomID &&
		left.Weekday == right.Weekday && left.Block == right.Block
}
