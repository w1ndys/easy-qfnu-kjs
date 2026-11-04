package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 编译期确认真实仓储与替身都满足执行循环的窄接口，签名漂移会直接让测试编译失败。
var (
	_ TaskStore = store.SyncTaskRepo{}
	_ TaskStore = (*taskStoreStub)(nil)
)

// taskStoreStub 替身记录执行循环对仓储的每一次调用，并按预置返回任务清单。
// 真实 SQL 行为由 store 的集成测试覆盖，这里只锁编排口径。
type taskStoreStub struct {
	persisted        []store.SyncTask // EnsureSyncTasks 返回的权威任务清单
	ensureErr        error
	recordErr        error
	recordFailureErr error
	completeErr      error
	failErr          error

	events       []string
	ensureRunID  int64
	ensureInputs []store.SyncTaskInput
	ensureNow    time.Time

	records      []store.RunWeekFetch
	weekFailures []store.RunWeekFailure
	completes    []completeCall
	fails        []failCall
}

type completeCall struct {
	runID  int64
	taskID int64
	at     time.Time
}

type failCall struct {
	runID   int64
	taskID  int64
	message string
	at      time.Time
}

// EnsureSyncTasks 实现 TaskStore。
func (s *taskStoreStub) EnsureSyncTasks(_ context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error) {
	s.events = append(s.events, "ensure")
	s.ensureRunID = runID
	s.ensureInputs = tasks
	s.ensureNow = now
	return s.persisted, s.ensureErr
}

// RecordWeekFetch 实现 TaskStore。
func (s *taskStoreStub) RecordWeekFetch(_ context.Context, fetch store.RunWeekFetch) error {
	s.events = append(s.events, fmt.Sprintf("record:%d", fetch.Week))
	s.records = append(s.records, fetch)
	return s.recordErr
}

// RecordWeekFailure 实现 TaskStore。
func (s *taskStoreStub) RecordWeekFailure(_ context.Context, failure store.RunWeekFailure) error {
	s.events = append(s.events, fmt.Sprintf("record-fail:%d", failure.Week))
	s.weekFailures = append(s.weekFailures, failure)
	return s.recordFailureErr
}

// CompleteSyncTask 实现 TaskStore。
func (s *taskStoreStub) CompleteSyncTask(_ context.Context, runID, taskID int64, completedAt time.Time) error {
	s.events = append(s.events, fmt.Sprintf("complete:%d", taskID))
	s.completes = append(s.completes, completeCall{runID: runID, taskID: taskID, at: completedAt})
	return s.completeErr
}

// FailSyncTask 实现 TaskStore。
func (s *taskStoreStub) FailSyncTask(_ context.Context, runID, taskID int64, message string, failedAt time.Time) error {
	s.events = append(s.events, fmt.Sprintf("fail:%d", taskID))
	s.fails = append(s.fails, failCall{runID: runID, taskID: taskID, message: message, at: failedAt})
	return s.failErr
}

// taskFetchStub 按任务身份回放页面正文或错误，并记录调用顺序。
type taskFetchStub struct {
	pages  map[string][]byte
	errs   map[string]error
	onCall func()
	calls  []Task
}

// fetch 实现 TaskFetcher。
func (f *taskFetchStub) fetch(_ context.Context, task Task) ([]byte, error) {
	f.calls = append(f.calls, task)
	if f.onCall != nil {
		f.onCall()
	}
	key := taskKey(task)
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	return f.pages[key], nil
}

// taskKey 用任务的全部请求参数拼身份，便于测试按任务预置页面与断言顺序。
func taskKey(task Task) string {
	return fmt.Sprintf("%s/%s/w%d/%s/d%d/%s", task.Kind, task.BuildingID, task.Week,
		task.RoomID, task.Weekday, task.Block)
}

// shanghaiMorning 返回业务时区 9 点的固定时刻，测试不读真实时间。
func shanghaiMorning() time.Time {
	return time.Date(2026, 11, 4, 9, 30, 0, 0, shanghai)
}

// weekTask 造一条周矩阵任务；taskID 用库里的编号。
func weekTask(taskID int64, buildingID string, week int, status string) store.SyncTask {
	return store.SyncTask{
		TaskID:     taskID,
		Kind:       string(TaskWeekMatrix),
		Required:   true,
		BuildingID: buildingID,
		Week:       week,
		Status:     status,
	}
}

// cellTask 造一条 Q10 楼的占用说明任务：周一第 0102 大节、pending。
func cellTask(taskID int64, roomID string) store.SyncTask {
	return store.SyncTask{
		TaskID:     taskID,
		Kind:       string(TaskCellDetail),
		BuildingID: "Q10",
		RoomID:     roomID,
		Weekday:    1,
		Block:      "0102",
		Status:     store.SyncTaskPending,
	}
}

// sameTask 比较任务的全部请求参数，忽略 TaskID 与状态。
func sameTask(got, want Task) bool {
	return got == want
}

// TestRunResumesFromFirstPendingAndSkipsDone 覆盖 Requirement 11.1 / 8.4：
// 续跑从第一个 pending 继续，done 直接跳过，不重复取页面也不重复计数。
func TestRunResumesFromFirstPendingAndSkipsDone(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q10", 21, store.SyncTaskPending),
		weekTask(3, "Q10", 22, store.SyncTaskDone),
		weekTask(4, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 21}): []byte("page-21"),
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): []byte("page-20"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("续跑不应报错，实际 %v", err)
	}

	// 只执行两条 pending，done 一条都不取。
	if len(fetch.calls) != 2 {
		t.Fatalf("应只执行 2 条 pending，实际调用 %d 次: %+v", len(fetch.calls), fetch.calls)
	}
	wantOrder := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 20},
	}
	for index, want := range wantOrder {
		if !sameTask(fetch.calls[index], want) {
			t.Fatalf("第 %d 次执行任务不符: 期望 %+v，实际 %+v", index+1, want, fetch.calls[index])
		}
	}

	wantEvents := []string{"ensure", "record:21", "complete:2", "record:20", "complete:4"}
	if fmt.Sprint(storeStub.events) != fmt.Sprint(wantEvents) {
		t.Fatalf("调用顺序不符: 期望 %v，实际 %v", wantEvents, storeStub.events)
	}
	if progress.Completed != 2 || progress.SkippedDone != 2 || progress.Failed != 0 || progress.SkippedFailed != 0 {
		t.Fatalf("进度不符: %+v", progress)
	}
}

// TestRunSkipsDoneAndKeepsFailedWithoutRetry 覆盖 Requirement 11.2 / 8.4：
// failed 在续跑时保持原状、不再请求，也不产生新的 fail 写入。
func TestRunSkipsDoneAndKeepsFailedWithoutRetry(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskFailed),
		weekTask(2, "Q10", 21, store.SyncTaskDone),
		weekTask(3, "Q10", 22, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 22}): []byte("page-22"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("续跑不应报错，实际 %v", err)
	}
	if len(fetch.calls) != 1 || fetch.calls[0].Week != 22 {
		t.Fatalf("只应执行 pending 的第 22 周，实际 %+v", fetch.calls)
	}
	if len(storeStub.fails) != 0 {
		t.Fatalf("failed 任务不应在本轮被重试或改写，实际 fail 调用 %+v", storeStub.fails)
	}
	if progress.SkippedFailed != 1 || progress.SkippedDone != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}
}

// TestRunRecordsWeekFetchWithFetchedAtAndPageHash 覆盖任务原文：
// 周矩阵成功后写 collect_run_week（fetched_at + sha256），再置 done。
func TestRunRecordsWeekFetchWithFetchedAtAndPageHash(t *testing.T) {
	now := shanghaiMorning()
	page := []byte("<table>第 20 周</table>")
	persisted := []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20}): page,
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(now),
	})
	if err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if progress.Completed != 1 {
		t.Fatalf("应完成 1 条，实际 %+v", progress)
	}
	if len(storeStub.records) != 1 {
		t.Fatalf("应写 1 条周结果，实际 %d 条", len(storeStub.records))
	}
	record := storeStub.records[0]
	if record.RunID != 7 || record.Week != 20 || record.BuildingID != "Q10" {
		t.Fatalf("周结果身份不符: %+v", record)
	}
	if !record.FetchedAt.Equal(now) {
		t.Fatalf("fetched_at 期望 %s，实际 %s", now, record.FetchedAt)
	}
	if record.PageSHA256 != pageSHA256(page) {
		t.Fatalf("页面哈希期望 %s，实际 %s", pageSHA256(page), record.PageSHA256)
	}
	if len(storeStub.completes) != 1 || storeStub.completes[0].taskID != 1 || !storeStub.completes[0].at.Equal(now) {
		t.Fatalf("完成任务调用不符: %+v", storeStub.completes)
	}
}

// TestRunDoesNotRecordWeekForCellDetail 覆盖表结构口径：
// 占用说明任务没有「楼 × 周」行，只置 done，不写 collect_run_week。
func TestRunDoesNotRecordWeekForCellDetail(t *testing.T) {
	persisted := []store.SyncTask{cellTask(1, "r202")}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r202", Weekday: 1, Block: "0102"}): []byte("detail"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if len(storeStub.records) != 0 {
		t.Fatalf("占用说明不应写周结果，实际 %+v", storeStub.records)
	}
	if progress.Completed != 1 || len(storeStub.completes) != 1 || storeStub.completes[0].taskID != 1 {
		t.Fatalf("占用说明应置 done 并计数: progress=%+v completes=%+v", progress, storeStub.completes)
	}
}

// TestRunMarksFailedTaskAndContinues 覆盖任务失败后的同轮行为：
// 失败任务置 failed 并记录原因，循环不中断；后续可选任务照常完成。
func TestRunMarksFailedTaskAndContinues(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		cellTask(2, "r101"),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{
		pages: map[string][]byte{
			taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): []byte("detail"),
		},
		errs: map[string]error{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): errors.New("矩阵结构失败"),
		},
	}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(now),
	})
	if err != nil {
		t.Fatalf("任务失败不应让执行循环报错，实际 %v", err)
	}
	if len(storeStub.fails) != 1 || storeStub.fails[0].taskID != 1 {
		t.Fatalf("应把第 1 条任务置 failed，实际 %+v", storeStub.fails)
	}
	if storeStub.fails[0].message != "矩阵结构失败" || !storeStub.fails[0].at.Equal(now) {
		t.Fatalf("失败原因或时刻不符: %+v", storeStub.fails[0])
	}
	if len(storeStub.records) != 0 {
		t.Fatalf("失败任务不应写周结果，实际 %+v", storeStub.records)
	}
	if progress.Failed != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}
	if len(storeStub.completes) != 1 || storeStub.completes[0].taskID != 2 {
		t.Fatalf("后续任务应继续完成，实际 %+v", storeStub.completes)
	}
}

// TestRunDoesNotWriteWeekFailureForCellDetail 覆盖占用说明的失败路径：
// 占用说明不属于「楼 × 周」，失败只置 failed，不写 collect_run_week 的失败行；
// 循环照常继续，后续周矩阵任务写自己的成功周结果。
func TestRunDoesNotWriteWeekFailureForCellDetail(t *testing.T) {
	persisted := []store.SyncTask{
		cellTask(1, "r101"),
		weekTask(2, "Q10", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{
		pages: map[string][]byte{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): []byte("page-20"),
		},
		errs: map[string]error{
			taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): errors.New("明细结构失败"),
		},
	}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("任务失败不应让执行循环报错，实际 %v", err)
	}
	if len(storeStub.weekFailures) != 0 {
		t.Fatalf("占用说明任务失败不应写周失败结果，实际 %+v", storeStub.weekFailures)
	}
	if len(storeStub.fails) != 1 || storeStub.fails[0].taskID != 1 {
		t.Fatalf("应把占用说明任务置 failed，实际 %+v", storeStub.fails)
	}
	if len(storeStub.records) != 1 || storeStub.records[0].Week != 20 {
		t.Fatalf("后续周矩阵任务应照常写周结果，实际 %+v", storeStub.records)
	}
	if progress.Failed != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}
}

// TestRunWritesWeekFailureRowAndKeepsFailed 覆盖周矩阵失败：
// 先写同一唯一键的 ok=false 行（含原因），再置 failed，后续可选任务照常完成。
func TestRunWritesWeekFailureRowAndKeepsFailed(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		cellTask(2, "r101"),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{
		pages: map[string][]byte{
			taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): []byte("detail"),
		},
		errs: map[string]error{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): errors.New("矩阵结构失败"),
		},
	}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("任务失败不应让执行循环报错，实际 %v", err)
	}
	if len(storeStub.records) != 0 {
		t.Fatalf("失败任务不应写成功周结果，实际 %+v", storeStub.records)
	}
	if len(storeStub.weekFailures) != 1 {
		t.Fatalf("应写 1 条失败周结果，实际 %+v", storeStub.weekFailures)
	}
	failure := storeStub.weekFailures[0]
	if failure.RunID != 7 || failure.Week != 20 || failure.BuildingID != "Q10" || failure.ErrorMessage != "矩阵结构失败" {
		t.Fatalf("失败周结果身份或原因不符: %+v", failure)
	}
	if progress.Failed != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}
	wantEvents := []string{"ensure", "record-fail:20", "fail:1", "complete:2"}
	if fmt.Sprint(storeStub.events) != fmt.Sprint(wantEvents) {
		t.Fatalf("调用顺序不符: 期望 %v，实际 %v", wantEvents, storeStub.events)
	}
}

// TestRunReturnsErrorWhenRecordWeekFetchFails 覆盖周结果写失败：
// 执行循环立即返回错误，不把任务置 done，也不计入完成。
func TestRunReturnsErrorWhenRecordWeekFetchFails(t *testing.T) {
	recordErr := errors.New("写周结果失败")
	storeStub := &taskStoreStub{
		persisted: []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)},
		recordErr: recordErr,
	}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): []byte("page-20"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, recordErr) {
		t.Fatalf("应上抛周结果写入错误，实际 %v", err)
	}
	if len(storeStub.completes) != 0 {
		t.Fatalf("周结果写失败时不应置 done，实际 %+v", storeStub.completes)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Fatalf("写失败不应计入完成或失败: %+v", progress)
	}
}

// TestRunReturnsErrorWhenRecordWeekFailureFails 覆盖失败周结果写失败：
// 立即返回错误，不把任务置 failed（保持 pending 供下次续跑）。
func TestRunReturnsErrorWhenRecordWeekFailureFails(t *testing.T) {
	recordFailureErr := errors.New("写失败周结果失败")
	storeStub := &taskStoreStub{
		persisted:        []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)},
		recordFailureErr: recordFailureErr,
	}
	fetch := &taskFetchStub{errs: map[string]error{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): errors.New("矩阵结构失败"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, recordFailureErr) {
		t.Fatalf("应上抛失败周结果写入错误，实际 %v", err)
	}
	if len(storeStub.fails) != 0 {
		t.Fatalf("失败周结果写失败时不应置 failed，实际 %+v", storeStub.fails)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Fatalf("写失败不应计入完成或失败: %+v", progress)
	}
}

// TestRunReturnsErrorWhenCompleteSyncTaskFails 覆盖置 done 失败：错误上抛，
// 任务保持 pending，完成计数不增加。
func TestRunReturnsErrorWhenCompleteSyncTaskFails(t *testing.T) {
	completeErr := errors.New("标记完成失败")
	storeStub := &taskStoreStub{
		persisted:   []store.SyncTask{cellTask(1, "r101")},
		completeErr: completeErr,
	}
	fetch := &taskFetchStub{pages: map[string][]byte{
		taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): []byte("detail"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, completeErr) {
		t.Fatalf("应上抛置 done 错误，实际 %v", err)
	}
	if len(storeStub.weekFailures) != 0 {
		t.Fatalf("占用说明任务成功但置 done 失败时不应写周失败结果，实际 %+v", storeStub.weekFailures)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Fatalf("置 done 失败不应计入完成或失败: %+v", progress)
	}
}

// TestRunReturnsErrorWhenFailSyncTaskFails 覆盖置 failed 失败：错误上抛，
// 失败计数不增加，任务仍是 pending。
func TestRunReturnsErrorWhenFailSyncTaskFails(t *testing.T) {
	failErr := errors.New("标记失败失败")
	storeStub := &taskStoreStub{
		persisted: []store.SyncTask{cellTask(1, "r101")},
		failErr:   failErr,
	}
	fetch := &taskFetchStub{errs: map[string]error{
		taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): errors.New("明细结构失败"),
	}}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, failErr) {
		t.Fatalf("应上抛置 failed 错误，实际 %v", err)
	}
	if len(storeStub.weekFailures) != 0 {
		t.Fatalf("占用说明任务失败不应写周失败结果，实际 %+v", storeStub.weekFailures)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Fatalf("置 failed 失败不应计入完成或失败: %+v", progress)
	}
}

// TestRunWithEmptyTaskList 覆盖空任务清单：只落一次任务（不新增），
// 不执行任何请求，进度全零。
func TestRunWithEmptyTaskList(t *testing.T) {
	storeStub := &taskStoreStub{}
	fetch := &taskFetchStub{}

	progress, err := Run(context.Background(), 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("空任务清单不应报错，实际 %v", err)
	}
	if len(fetch.calls) != 0 {
		t.Fatalf("空任务清单不应发起请求，实际 %+v", fetch.calls)
	}
	if len(storeStub.completes) != 0 || len(storeStub.fails) != 0 {
		t.Fatalf("空任务清单不应写任务状态: completes=%+v fails=%+v", storeStub.completes, storeStub.fails)
	}
	// 空清单仍然只落一次任务（续跑时不新增），且落库入参为空——不是 nil 之外的多余写入。
	if want := []string{"ensure"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("空任务清单的调用顺序不符: 期望 %v，实际 %v", want, storeStub.events)
	}
	if len(storeStub.ensureInputs) != 0 {
		t.Fatalf("空任务清单不应带任务入参，实际 %+v", storeStub.ensureInputs)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("空任务清单的进度应全零，实际 %+v", progress)
	}
}

// TestRunReturnsErrorWhenEnsureSyncTasksFails 覆盖规划落库失败：
// 错误上抛，不发起任何请求，进度全零——任务清单都没落库就没有可续跑的任务。
func TestRunReturnsErrorWhenEnsureSyncTasksFails(t *testing.T) {
	ensureErr := errors.New("写任务清单失败")
	storeStub := &taskStoreStub{ensureErr: ensureErr}
	fetch := &taskFetchStub{}

	progress, err := Run(context.Background(), 7, []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
	}, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, ensureErr) {
		t.Fatalf("应上抛任务落库错误，实际 %v", err)
	}
	if len(fetch.calls) != 0 {
		t.Fatalf("任务落库失败时不应发起请求，实际 %+v", fetch.calls)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("任务落库失败时进度应全零，实际 %+v", progress)
	}
	if want := []string{"ensure"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("落库失败后不应再调用仓储，期望 %v，实际 %v", want, storeStub.events)
	}
}

// TestRunDoesNotFailTaskWhenContextCanceled 覆盖中断语义：
// 调用方取消时保持任务 pending，不写 failed，下一次续跑还能做。
func TestRunDoesNotFailTaskWhenContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	persisted := []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{
		errs: map[string]error{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): context.Canceled,
		},
		onCall: cancel,
	}

	progress, err := Run(ctx, 7, nil, RunDeps{
		Tasks: storeStub,
		Fetch: fetch.fetch,
		Clock: fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消时应返回 context.Canceled，实际 %v", err)
	}
	if len(storeStub.fails) != 0 {
		t.Fatalf("取消不是任务失败，不应写 failed: %+v", storeStub.fails)
	}
	if progress.Completed != 0 || progress.Failed != 0 {
		t.Fatalf("取消时不应计入已完成或失败: %+v", progress)
	}
}

// TestRunSeedsPlannedTasks 覆盖规划落库：把 pipeline.Task 的真实字段映射成
// store.SyncTaskInput，并以注入时钟作 updated_at。
func TestRunSeedsPlannedTasks(t *testing.T) {
	now := shanghaiMorning()
	planned := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304"},
	}
	storeStub := &taskStoreStub{}

	if _, err := Run(context.Background(), 7, planned, RunDeps{
		Tasks: storeStub,
		Fetch: (&taskFetchStub{}).fetch,
		Clock: fixedClock(now),
	}); err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if storeStub.ensureRunID != 7 || !storeStub.ensureNow.Equal(now) {
		t.Fatalf("Ensure 入参不符: runID=%d now=%s", storeStub.ensureRunID, storeStub.ensureNow)
	}
	want := []store.SyncTaskInput{
		{Kind: string(TaskWeekMatrix), Required: true, BuildingID: "Q10", Week: 20},
		{Kind: string(TaskCellDetail), Required: false, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304"},
	}
	if fmt.Sprint(storeStub.ensureInputs) != fmt.Sprint(want) {
		t.Fatalf("落库任务映射不符: 期望 %+v，实际 %+v", want, storeStub.ensureInputs)
	}
}

// TestRunRequiresDependencies 覆盖装配错误：缺仓储或执行函数时立即报错。
func TestRunRequiresDependencies(t *testing.T) {
	if _, err := Run(context.Background(), 7, nil, RunDeps{}); err == nil {
		t.Fatal("缺任务仓储时应报错")
	}
	if _, err := Run(context.Background(), 7, nil, RunDeps{Tasks: &taskStoreStub{}}); err == nil {
		t.Fatal("缺任务执行函数时应报错")
	}
}

// TestPageSHA256MatchesKnownDigest 用公开测试向量锁住哈希算法与十六进制编码。
func TestPageSHA256MatchesKnownDigest(t *testing.T) {
	cases := []struct {
		name string
		page []byte
		want string
	}{
		{name: "空正文", page: nil, want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{name: "abc", page: []byte("abc"), want: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageSHA256(tc.page); got != tc.want {
				t.Fatalf("sha256 期望 %s，实际 %s", tc.want, got)
			}
		})
	}
}
