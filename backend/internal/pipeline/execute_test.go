package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 编译期确认真实仓储与替身都满足执行循环的窄接口，签名漂移会直接让测试编译失败。
var (
	_ TaskStore  = store.SyncTaskRepo{}
	_ TaskStore  = (*taskStoreStub)(nil)
	_ RoomStore  = store.RoomRepo{}
	_ RoomStore  = (*roomStoreStub)(nil)
	_ PhaseStore = store.RunPhaseRepo{}
	_ PhaseStore = (*phaseStoreStub)(nil)
)

// phaseStoreStub 替身记录阶段推进、阶段读取、错误码写入与收口写库，
// 并按预置返回任务汇总。
//
// 默认当前阶段是 PhaseLogin（全新的轮次）；续跑用例显式设置 current。
// status 默认 running；已收口的用例只设 status（可配合任意 phase），因为
// 「这一轮是否结束」只看状态，不看阶段。
// 阶段写入默认不混进 taskStoreStub 的 events：两段任务的调用顺序断言不受影响；
// 需要断言「阶段推进与任务落库的相对顺序」的用例把 events 指到同一个切片上。
type phaseStoreStub struct {
	phases  []phaseCall
	codes   []errorCodeCall
	summary store.RunTaskSummary
	finish  *store.RunFinish
	current Phase
	status  string // collect_run.status；空串按 running 处理

	events *[]string // 与 taskStoreStub 共用的事件流；nil 时只记在 phases 里

	loadErr       error
	loadPhaseErr  error
	phaseErr      error
	recordCodeErr error
	finishErr     error
}

// phaseCall 是一次阶段推进的入参、可接受的当前阶段与时刻。
type phaseCall struct {
	runID       int64
	phase       string
	allowedFrom []string
	at          time.Time
}

// errorCodeCall 是一次轮次错误码写入的入参。
type errorCodeCall struct {
	runID int64
	code  string
	at    time.Time
}

// record 把事件追加到共用事件流（nil 时忽略）。
func (s *phaseStoreStub) record(event string) {
	if s.events == nil {
		return
	}
	*s.events = append(*s.events, event)
}

// SetRunPhase 实现 PhaseStore。
func (s *phaseStoreStub) SetRunPhase(_ context.Context, runID int64, phase string, allowedFrom []string, now time.Time) error {
	s.phases = append(s.phases, phaseCall{runID: runID, phase: phase, allowedFrom: allowedFrom, at: now})
	s.record("phase:" + phase)
	if s.phaseErr != nil {
		return s.phaseErr
	}
	s.current = Phase(phase)
	return nil
}

// LoadRunPhase 实现 PhaseStore。status 默认 running（全新或进行中的轮次），
// 「已收口」用例显式设置 status；phase 单独一个字段，便于验证「停在 publish
// 的成功轮次也算已收口」这类只有状态能表达的情况。
func (s *phaseStoreStub) LoadRunPhase(_ context.Context, _ int64) (string, string, error) {
	if s.loadPhaseErr != nil {
		return "", "", s.loadPhaseErr
	}
	status := s.status
	if status == "" {
		status = "running"
	}
	if s.current == "" {
		return string(PhaseLogin), status, nil
	}
	return string(s.current), status, nil
}

// RecordRunErrorCode 实现 PhaseStore。
func (s *phaseStoreStub) RecordRunErrorCode(_ context.Context, runID int64, code string, now time.Time) error {
	s.codes = append(s.codes, errorCodeCall{runID: runID, code: code, at: now})
	return s.recordCodeErr
}

// LoadRunTaskSummary 实现 PhaseStore。
func (s *phaseStoreStub) LoadRunTaskSummary(_ context.Context, _ int64) (store.RunTaskSummary, error) {
	return s.summary, s.loadErr
}

// FinishRun 实现 PhaseStore。
func (s *phaseStoreStub) FinishRun(_ context.Context, finish store.RunFinish) error {
	s.finish = &finish
	return s.finishErr
}

// taskStoreStub 替身记录执行循环对仓储的每一次调用，并按预置返回任务清单。
// 两段 Ensure 都从 persisted 里按 kind 过滤，保持段内顺序；真实 SQL 行为由
// store 的集成测试覆盖，这里只锁编排口径。
type taskStoreStub struct {
	persisted        []store.SyncTask // 两段 Ensure 共用的权威任务清单
	ensureWeekErr    error
	ensureCellErr    error
	recordErr        error
	recordFailureErr error
	completeErr      error
	failErr          error

	events []string

	weekRunID  int64
	weekInputs []store.SyncTaskInput
	weekNow    time.Time
	cellRunID  int64
	cellInputs []store.SyncTaskInput
	cellNow    time.Time

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

// syncTasksOfKind 按 kind 过滤任务，保持原顺序。
func syncTasksOfKind(tasks []store.SyncTask, kind string) []store.SyncTask {
	filtered := make([]store.SyncTask, 0, len(tasks))
	for _, task := range tasks {
		if task.Kind == kind {
			filtered = append(filtered, task)
		}
	}
	return filtered
}

// EnsureWeekMatrixTasks 实现 TaskStore。
func (s *taskStoreStub) EnsureWeekMatrixTasks(_ context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error) {
	s.events = append(s.events, "ensure-week")
	s.weekRunID = runID
	s.weekInputs = tasks
	s.weekNow = now
	return syncTasksOfKind(s.persisted, store.SyncTaskKindWeekMatrix), s.ensureWeekErr
}

// EnsureCellDetailTasks 实现 TaskStore。
func (s *taskStoreStub) EnsureCellDetailTasks(_ context.Context, runID int64, tasks []store.SyncTaskInput, now time.Time) ([]store.SyncTask, error) {
	s.events = append(s.events, "ensure-cell")
	s.cellRunID = runID
	s.cellInputs = tasks
	s.cellNow = now
	return syncTasksOfKind(s.persisted, store.SyncTaskKindCellDetail), s.ensureCellErr
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

// taskFetchStub 按任务身份回放页面正文、房号行或错误，并记录调用顺序。
type taskFetchStub struct {
	pages  map[string][]byte
	rooms  map[string][]model.MatrixRoom
	errs   map[string]error
	onCall func()
	calls  []Task
}

// fetch 实现 TaskFetcher。
func (f *taskFetchStub) fetch(_ context.Context, task Task) (TaskFetch, error) {
	f.calls = append(f.calls, task)
	if f.onCall != nil {
		f.onCall()
	}
	key := taskKey(task)
	if err := f.errs[key]; err != nil {
		return TaskFetch{}, err
	}
	return TaskFetch{Page: f.pages[key], Rooms: f.rooms[key]}, nil
}

// roomStoreStub 替身记录执行循环对本轮归属的每一次落库与冲突回写。
// 真实 SQL 行为由 store 的集成测试覆盖，这里只锁编排口径。
type roomStoreStub struct {
	attributions []store.RoomAttribution
	runRooms     []runRoomCall
	conflicts    []attrConflictCall
	loaded       map[int64][]store.RunRoom
	loadCalls    []int64
	upsertErr    error
	runRoomErr   error
	loadErr      error
	conflictErr  error
}

type runRoomCall struct {
	runID int64
	rooms []store.RunRoom
	at    time.Time
}

type attrConflictCall struct {
	runID     int64
	conflicts int
	at        time.Time
}

// UpsertRoomAttribution 实现 RoomStore。
func (s *roomStoreStub) UpsertRoomAttribution(_ context.Context, attribution store.RoomAttribution) error {
	s.attributions = append(s.attributions, attribution)
	return s.upsertErr
}

// UpsertRunRooms 实现 RoomStore。
func (s *roomStoreStub) UpsertRunRooms(_ context.Context, runID int64, rooms []store.RunRoom, now time.Time) error {
	s.runRooms = append(s.runRooms, runRoomCall{runID: runID, rooms: rooms, at: now})
	return s.runRoomErr
}

// LoadRunRooms 实现 RoomStore。
func (s *roomStoreStub) LoadRunRooms(_ context.Context, runID int64) ([]store.RunRoom, error) {
	s.loadCalls = append(s.loadCalls, runID)
	return s.loaded[runID], s.loadErr
}

// RecordAttrConflicts 实现 RoomStore。
func (s *roomStoreStub) RecordAttrConflicts(_ context.Context, runID int64, conflicts int, now time.Time) error {
	s.conflicts = append(s.conflicts, attrConflictCall{runID: runID, conflicts: conflicts, at: now})
	return s.conflictErr
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
	return cellTaskIn(taskID, "Q10", roomID)
}

// cellTaskIn 造一条指定楼的占用说明任务，供跨楼归属校验的用例使用。
func cellTaskIn(taskID int64, buildingID, roomID string) store.SyncTask {
	return store.SyncTask{
		TaskID:     taskID,
		Kind:       string(TaskCellDetail),
		BuildingID: buildingID,
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

	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	wantEvents := []string{"ensure-week", "record:21", "complete:2", "record:20", "complete:4", "ensure-cell"}
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(now),
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

	progress, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(now),
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
		t.Fatalf("失败任务只应写 ok=false 的失败行，不应写成功周结果，实际 %+v", storeStub.records)
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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
	wantEvents := []string{"ensure-week", "record-fail:20", "fail:1", "ensure-cell", "complete:2"}
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

	progress, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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

// TestRunPropagatesPhaseLoadError 覆盖 minor 6：读当前阶段失败属于编排错误，
// 立即上抛，不落任务、不发请求——阶段判断不出来就不知道哪些推进要跳过。
func TestRunPropagatesPhaseLoadError(t *testing.T) {
	loadErr := errors.New("读阶段失败")
	phases := &phaseStoreStub{loadPhaseErr: loadErr}
	storeStub := &taskStoreStub{}
	fetch := &taskFetchStub{}

	_, err := Run(context.Background(), 7, []string{"Q10"}, []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
	}, RunDeps{
		Phases: phases,
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, loadErr) {
		t.Fatalf("应上抛读阶段错误，实际 %v", err)
	}
	if len(storeStub.events) != 0 || len(fetch.calls) != 0 {
		t.Fatalf("读阶段失败时不应落库或发请求: events=%v calls=%+v", storeStub.events, fetch.calls)
	}
}

// TestRunPropagatesRecordErrorCodeFailure 覆盖 minor 6：写轮次错误码失败属于编排错误，
// failTask 立即返回，失败周结果与 failed 状态都不写，进度也不记失败（任务保持 pending）。
func TestRunPropagatesRecordErrorCodeFailure(t *testing.T) {
	codeErr := errors.New("写轮次错误码失败")
	phases := &phaseStoreStub{recordCodeErr: codeErr}
	storeStub := &taskStoreStub{
		persisted: []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)},
	}
	fetch := &taskFetchStub{errs: map[string]error{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): fmt.Errorf(
			"解析周矩阵失败: %w", parser.ErrMatrixStructureInvalid),
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: phases,
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, codeErr) {
		t.Fatalf("应上抛写错误码错误，实际 %v", err)
	}
	if len(phases.codes) != 1 || phases.codes[0].code != "matrix_structure_invalid" {
		t.Fatalf("应尝试写一次矩阵结构失败码，实际 %+v", phases.codes)
	}
	if len(storeStub.weekFailures) != 0 || len(storeStub.fails) != 0 {
		t.Fatalf("写码失败后不应继续写周失败或置 failed: failures=%+v fails=%+v",
			storeStub.weekFailures, storeStub.fails)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("写码失败时进度应全零，实际 %+v", progress)
	}
}

// TestRunWithEmptyTaskList 覆盖空任务清单：只落一次任务（不新增），
// 不执行任何请求，进度全零。
func TestRunWithEmptyTaskList(t *testing.T) {
	storeStub := &taskStoreStub{}
	fetch := &taskFetchStub{}

	progress, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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
	// 两段各落一次（续跑时不新增），且两段落库入参都为空——不是 nil 之外的多余写入。
	if want := []string{"ensure-week", "ensure-cell"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("空任务清单的调用顺序不符: 期望 %v，实际 %v", want, storeStub.events)
	}
	if len(storeStub.weekInputs) != 0 || len(storeStub.cellInputs) != 0 {
		t.Fatalf("空任务清单不应带任务入参，实际 week=%+v cell=%+v",
			storeStub.weekInputs, storeStub.cellInputs)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("空任务清单的进度应全零，实际 %+v", progress)
	}
}

// TestRunReturnsErrorWhenEnsureWeekMatrixTasksFails 覆盖规划落库失败：
// 错误上抛，不发起任何请求，进度全零——任务清单都没落库就没有可续跑的任务。
func TestRunReturnsErrorWhenEnsureWeekMatrixTasksFails(t *testing.T) {
	ensureErr := errors.New("写任务清单失败")
	storeStub := &taskStoreStub{ensureWeekErr: ensureErr}
	phases := &phaseStoreStub{}
	fetch := &taskFetchStub{}

	progress, err := Run(context.Background(), 7, []string{"Q10"}, []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
	}, RunDeps{
		Phases: phases,
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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
	if want := []string{"ensure-week"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("周矩阵段落库失败后不应再调用仓储，期望 %v，实际 %v", want, storeStub.events)
	}
	// minor 3：任务还没落库，阶段也必须原地不动——不能出现「已到周矩阵却一条任务都没有」。
	if len(phases.phases) != 0 {
		t.Fatalf("周矩阵段落库失败时不应推进阶段，实际 %+v", phases.phases)
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

	progress, err := Run(ctx, 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  fetch.fetch,
		Rooms:  &roomStoreStub{},
		Clock:  fixedClock(shanghaiMorning()),
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
// store.SyncTaskInput，并以注入时钟作 updated_at。周矩阵段已完成、归属由
// collect_run_room 读回，占用说明任务因此能通过归属校验进入落库入参。
func TestRunSeedsPlannedTasks(t *testing.T) {
	now := shanghaiMorning()
	planned := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304"},
	}
	storeStub := &taskStoreStub{persisted: []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskDone)}}
	roomStub := &roomStoreStub{loaded: map[int64][]store.RunRoom{
		7: {{RoomID: "r101", BuildingID: "Q10"}},
	}}

	if _, err := Run(context.Background(), 7, []string{"Q10"}, planned, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Fetch:  (&taskFetchStub{}).fetch,
		Rooms:  roomStub,
		Clock:  fixedClock(now),
	}); err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if storeStub.weekRunID != 7 || !storeStub.weekNow.Equal(now) {
		t.Fatalf("周矩阵段 Ensure 入参不符: runID=%d now=%s", storeStub.weekRunID, storeStub.weekNow)
	}
	if storeStub.cellRunID != 7 || !storeStub.cellNow.Equal(now) {
		t.Fatalf("占用说明段 Ensure 入参不符: runID=%d now=%s", storeStub.cellRunID, storeStub.cellNow)
	}
	wantWeeks := []store.SyncTaskInput{
		{Kind: string(TaskWeekMatrix), Required: true, BuildingID: "Q10", Week: 20},
	}
	if fmt.Sprint(storeStub.weekInputs) != fmt.Sprint(wantWeeks) {
		t.Fatalf("周矩阵段落库映射不符: 期望 %+v，实际 %+v", wantWeeks, storeStub.weekInputs)
	}
	wantCells := []store.SyncTaskInput{
		{Kind: string(TaskCellDetail), Required: false, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304"},
	}
	if fmt.Sprint(storeStub.cellInputs) != fmt.Sprint(wantCells) {
		t.Fatalf("占用说明段落库映射不符: 期望 %+v，实际 %+v", wantCells, storeStub.cellInputs)
	}
	if want := []string{"ensure-week", "ensure-cell"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("两段落库顺序不符: 期望 %v，实际 %v", want, storeStub.events)
	}
}

// TestRunRequiresDependencies 覆盖装配错误：缺任务仓储、归属仓储、阶段仓储或
// 执行函数时立即报错。
func TestRunRequiresDependencies(t *testing.T) {
	if _, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
	}); err == nil {
		t.Fatal("缺任务仓储时应报错")
	}
	if _, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  &taskStoreStub{},
	}); err == nil {
		t.Fatal("缺归属仓储时应报错")
	}
	if _, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  &taskStoreStub{},
		Rooms:  &roomStoreStub{},
	}); err == nil {
		t.Fatal("缺任务执行函数时应报错")
	}
	if _, err := Run(context.Background(), 7, nil, nil, RunDeps{
		Tasks: &taskStoreStub{},
		Rooms: &roomStoreStub{},
		Fetch: (&taskFetchStub{}).fetch,
	}); err == nil {
		t.Fatal("缺阶段仓储时应报错")
	}
}

// matrixRoom 造一行周矩阵解析结果；归属只关心房号与原始名，格子留空。
func matrixRoom(roomID, nameRaw string) model.MatrixRoom {
	return model.MatrixRoom{RoomID: roomID, NameRaw: nameRaw}
}

// attributionRoomIDs 取一次归属落库里的房号，便于断言「谁被写、谁被丢弃」。
func attributionRoomIDs(attribution store.RoomAttribution) []string {
	roomIDs := make([]string, len(attribution.Rooms))
	for index, room := range attribution.Rooms {
		roomIDs[index] = room.RoomID
	}
	return roomIDs
}

// TestRunAttributesFirstHitAndDropsCrossBuildingConflict 覆盖 Requirement 5.1–5.3：
// 每楼响应内房号归属该楼并写 room 与 collect_run_room；后出现的楼再报同房号时
// 整行丢弃、只记一次冲突；同楼在另一周重复报出不算冲突。
// 任务顺序取 Plan 的「白名单顺序 × 周次升序」：Q10w20 → Q10w21 → Q11w20。
func TestRunAttributesFirstHitAndDropsCrossBuildingConflict(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q10", 21, store.SyncTaskPending),
		weekTask(3, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	fetch := &taskFetchStub{
		pages: map[string][]byte{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): []byte("page-q10-20"),
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 21}): []byte("page-q10-21"),
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): []byte("page-q11-20"),
		},
		rooms: map[string][]model.MatrixRoom{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {
				matrixRoom("r101", "F101(60/0)"),
				matrixRoom("r102", "F102"),
			},
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 21}): {
				matrixRoom("r101", "F101(60/0)"),
			},
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {
				matrixRoom("r101", "F101(60/0)"),
				matrixRoom("r201", "F201"),
			},
		},
	}

	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(now),
	})
	if err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if progress.Completed != 3 {
		t.Fatalf("应完成 3 条任务，实际 %+v", progress)
	}

	// 执行顺序必须是 Plan 顺序，而不是按 task_id 猜出来的别的白名单顺序。
	wantOrder := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 20},
	}
	for index, want := range wantOrder {
		if !sameTask(fetch.calls[index], want) {
			t.Fatalf("第 %d 次执行任务不符: 期望 %+v，实际 %+v", index+1, want, fetch.calls[index])
		}
	}

	// 三次落库：Q10 第 20 周首见、Q10 第 21 周复现 r101、Q11 第 20 周只剩 r201。
	if len(roomStub.attributions) != 3 {
		t.Fatalf("应写 3 次归属，实际 %d 次: %+v", len(roomStub.attributions), roomStub.attributions)
	}
	wantBuildings := []string{"Q10", "Q10", "Q11"}
	wantRooms := [][]string{{"r101", "r102"}, {"r101"}, {"r201"}}
	for index, attribution := range roomStub.attributions {
		if attribution.BuildingID != wantBuildings[index] {
			t.Fatalf("第 %d 次归属楼期望 %s，实际 %s", index+1, wantBuildings[index], attribution.BuildingID)
		}
		if fmt.Sprint(attributionRoomIDs(attribution)) != fmt.Sprint(wantRooms[index]) {
			t.Fatalf("第 %d 次归属房号期望 %v，实际 %v", index+1, wantRooms[index], attributionRoomIDs(attribution))
		}
	}

	// 展示名走 clean.RoomName，原始名保留。
	first := roomStub.attributions[0].Rooms[0]
	if first.Name != "F101" || first.NameRaw != "F101(60/0)" {
		t.Fatalf("房名规范化不符: %+v", first)
	}

	// 每轮归属快照同步落库，且只含 Accepted 与被保留归属的冲突标记：
	// Q11 的 r101 是 dropped，不能以 Q11 的身份进入候选集。
	if len(roomStub.runRooms) != 3 {
		t.Fatalf("应写 3 次每轮归属，实际 %d 次: %+v", len(roomStub.runRooms), roomStub.runRooms)
	}
	wantRunRooms := [][]store.RunRoom{
		{{RoomID: "r101", BuildingID: "Q10"}, {RoomID: "r102", BuildingID: "Q10"}},
		{{RoomID: "r101", BuildingID: "Q10"}},
		{
			{RoomID: "r201", BuildingID: "Q11"},
			{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		},
	}
	for index, call := range roomStub.runRooms {
		if call.runID != 7 || !call.at.Equal(now) {
			t.Fatalf("第 %d 次每轮归属身份不符: %+v", index+1, call)
		}
		if fmt.Sprint(call.rooms) != fmt.Sprint(wantRunRooms[index]) {
			t.Fatalf("第 %d 次每轮归属期望 %+v，实际 %+v", index+1, wantRunRooms[index], call.rooms)
		}
	}
	for _, call := range roomStub.runRooms {
		for _, room := range call.rooms {
			if room.RoomID == "r101" && room.BuildingID == "Q11" {
				t.Fatalf("被丢弃的 r101 不应以 Q11 的身份进入候选集: %+v", room)
			}
		}
	}

	// 跨楼冲突只在 Q11 第 20 周记一次，计数为 1，时刻取注入时钟。
	if len(roomStub.conflicts) != 1 {
		t.Fatalf("应只记 1 次冲突，实际 %+v", roomStub.conflicts)
	}
	if call := roomStub.conflicts[0]; call.runID != 7 || call.conflicts != 1 || !call.at.Equal(now) {
		t.Fatalf("冲突回写不符: %+v", call)
	}
}

// TestRunDroppedRoomDoesNotEnterRunSnapshot 覆盖 minor 4：被跨楼冲突丢弃的房号
// 不写 room、不写 collect_run_room（候选集来源），即使它出现在后到楼的响应里。
func TestRunDroppedRoomDoesNotEnterRunSnapshot(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	if _, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}

	// Q11 的房号整行都被丢弃：不落归属；但它报的冲突会打在 Q10 的有效归属行上，
	// 所以 Q11 自己不会有行，快照里仍会出现一次 Q10 的冲突标记写入。
	if len(roomStub.attributions) != 1 || roomStub.attributions[0].BuildingID != "Q10" {
		t.Fatalf("只有 Q10 能落归属，实际 %+v", roomStub.attributions)
	}
	// 两次写入：Q10 的有效归属一次，Q11 报冲突时给 Q10 那一行补冲突标记一次；
	// 两次的楼都是 Q10，不存在「r101 → Q11」的行。
	if len(roomStub.runRooms) != 2 {
		t.Fatalf("候选集快照应有 2 次写入，实际 %+v", roomStub.runRooms)
	}
	for _, call := range roomStub.runRooms {
		for _, room := range call.rooms {
			if room.RoomID == "r101" && room.BuildingID == "Q11" {
				t.Fatalf("被丢弃的 r101 不应进入候选集: %+v", room)
			}
		}
	}
	last := roomStub.runRooms[1].rooms
	if len(last) != 1 || last[0] != (store.RunRoom{
		RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11",
	}) {
		t.Fatalf("冲突标记应打在 Q10 的有效归属行上，实际 %+v", last)
	}
	if len(roomStub.conflicts) != 1 || roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("丢弃应记 1 次冲突，实际 %+v", roomStub.conflicts)
	}
}

// TestRunDropsCellDetailForNonAttributedBuilding 覆盖明细任务的归属校验（major）：
// Q10 首见 r101（该格可用、不规划明细），Q11 也报 r101 且这一格不可用，
// Plan 于是给了一条带 Q11 的明细任务 —— 本轮 r101 归 Q10，这条明细不能落库、
// 不能执行，计入失败；同一份 Q11 响应里归 Q11 的 r201 明细照常落库执行。
// 顺带覆盖「本轮没有任何归属」的房号 r999：同样丢弃，不靠外键失败兜底。
func TestRunDropsCellDetailForNonAttributedBuilding(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
		cellTaskIn(3, "Q11", "r201"),
	}
	planned := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 20},
		{Kind: TaskCellDetail, BuildingID: "Q11", RoomID: "r101", Weekday: 1, Block: "0102"},
		{Kind: TaskCellDetail, BuildingID: "Q11", RoomID: "r201", Weekday: 1, Block: "0102"},
		{Kind: TaskCellDetail, BuildingID: "Q11", RoomID: "r999", Weekday: 2, Block: "0304"},
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {
			matrixRoom("r101", "F101"),
			matrixRoom("r201", "F201"),
		},
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, planned, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(now),
	})
	if err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}

	// 只有归属 Q11 的 r201 明细能落库；r101 归 Q10、r999 没有归属，两条都被丢弃。
	wantCells := []store.SyncTaskInput{
		{Kind: string(TaskCellDetail), BuildingID: "Q11", RoomID: "r201", Weekday: 1, Block: "0102"},
	}
	if fmt.Sprint(storeStub.cellInputs) != fmt.Sprint(wantCells) {
		t.Fatalf("占用说明段落库入参应只剩 r201: 期望 %+v，实际 %+v", wantCells, storeStub.cellInputs)
	}
	// 被丢弃的明细不执行：抓取调用里不能出现 r101 / r999。
	cellCalls := 0
	for _, call := range fetch.calls {
		if call.Kind != TaskCellDetail {
			continue
		}
		cellCalls++
		if call.RoomID != "r201" {
			t.Fatalf("被丢弃的明细不应执行，实际执行 %+v", call)
		}
	}
	if cellCalls != 1 {
		t.Fatalf("应只执行 1 条明细任务，实际 %d 条: %+v", cellCalls, fetch.calls)
	}
	// 丢弃的明细计入失败（它们同样是一条没拿到的明细），并按必需性记进
	// DroppedTasks 供收口使用：丢弃任务没有写 sync_task，单读库里的 failed 行会漏掉它们。
	// 落库执行的三条任务照常完成。
	if progress.Completed != 3 || progress.Failed != 2 {
		t.Fatalf("进度不符: %+v", progress)
	}
	if want := (DroppedTasks{Optional: 2}); progress.Dropped() != want {
		t.Fatalf("丢弃明细期望 %+v，实际 %+v", want, progress.Dropped())
	}
	if len(storeStub.completes) != 3 {
		t.Fatalf("三条有效任务都应置 done，实际 %+v", storeStub.completes)
	}

	// 归属仍是 Q10 的 r101：Q11 只落 r201，跨楼冲突记 1 次。
	if len(roomStub.attributions) != 2 ||
		roomStub.attributions[0].BuildingID != "Q10" ||
		roomStub.attributions[1].BuildingID != "Q11" {
		t.Fatalf("归属落库不符: %+v", roomStub.attributions)
	}
	if got := attributionRoomIDs(roomStub.attributions[1]); fmt.Sprint(got) != fmt.Sprint([]string{"r201"}) {
		t.Fatalf("Q11 只应落 r201，实际 %v", got)
	}
	if len(roomStub.conflicts) != 1 || roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("r101 的跨楼冲突应记 1 次，实际 %+v", roomStub.conflicts)
	}
}

// TestRunRecordsSameRoomConflictOncePerRound 覆盖「一轮只记一次」：
// 同一房号在三栋楼、多周里反复出现，冲突只记一次，后续响应只丢弃不计数。
func TestRunRecordsSameRoomConflictOncePerRound(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q10", 21, store.SyncTaskPending),
		weekTask(3, "Q11", 20, store.SyncTaskPending),
		weekTask(4, "Q11", 21, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	rooms := map[string][]model.MatrixRoom{}
	for _, task := range []Task{
		{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20},
		{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 21},
		{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20},
		{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 21},
	} {
		rooms[taskKey(task)] = []model.MatrixRoom{matrixRoom("r101", "F101")}
	}
	fetch := &taskFetchStub{rooms: rooms}

	if _, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}

	if len(roomStub.conflicts) != 1 {
		t.Fatalf("同一房号一轮只应记 1 次冲突，实际 %+v", roomStub.conflicts)
	}
	if roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("冲突数应为 1，实际 %d", roomStub.conflicts[0].conflicts)
	}
	// Q11 两周都只有被丢弃的 r101，不应产生任何 room 写入。
	if len(roomStub.attributions) != 2 {
		t.Fatalf("应只有 Q10 的两周写归属，实际 %+v", roomStub.attributions)
	}
	for _, attribution := range roomStub.attributions {
		if attribution.BuildingID != "Q10" {
			t.Fatalf("被丢弃的楼不应写归属: %+v", attribution)
		}
		if fmt.Sprint(attributionRoomIDs(attribution)) != fmt.Sprint([]string{"r101"}) {
			t.Fatalf("Q10 应保留 r101，实际 %v", attributionRoomIDs(attribution))
		}
	}
}

// TestRunReturnsErrorWhenRoomAttributionFails 覆盖归属落库失败：
// 错误上抛，周结果与任务状态都不写，任务保持 pending 供续跑重做。
func TestRunReturnsErrorWhenRoomAttributionFails(t *testing.T) {
	upsertErr := errors.New("写归属失败")
	storeStub := &taskStoreStub{persisted: []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)}}
	roomStub := &roomStoreStub{upsertErr: upsertErr}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, upsertErr) {
		t.Fatalf("应上抛归属写入错误，实际 %v", err)
	}
	if len(storeStub.records) != 0 || len(storeStub.completes) != 0 {
		t.Fatalf("归属写失败时不应写周结果或置 done: records=%+v completes=%+v",
			storeStub.records, storeStub.completes)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("归属写失败时进度应全零，实际 %+v", progress)
	}
}

// TestRunReturnsErrorWhenRecordAttrConflictsFails 覆盖冲突回写失败：
// 错误上抛；前一条已完成任务保持 done，当前任务保持 pending。
func TestRunReturnsErrorWhenRecordAttrConflictsFails(t *testing.T) {
	conflictErr := errors.New("写冲突数失败")
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{conflictErr: conflictErr}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, conflictErr) {
		t.Fatalf("应上抛冲突回写错误，实际 %v", err)
	}
	if progress.Completed != 1 || len(storeStub.completes) != 1 || storeStub.completes[0].taskID != 1 {
		t.Fatalf("第 1 条任务应已完成，第 2 条保持 pending: progress=%+v completes=%+v",
			progress, storeStub.completes)
	}
}

// TestRunResumeKeepsCompletedAttribution 覆盖 blocker：A1 done → B1 pending，
// 两楼都报 r101。续跑必须从 collect_run_room 重建 A 的归属，B 的 r101 只能整行
// 丢弃并记一次冲突，不能把 r101 改判给 B，也不能漏掉冲突。
func TestRunResumeKeepsCompletedAttribution(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{loaded: map[int64][]store.RunRoom{
		7: {{RoomID: "r101", BuildingID: "Q10"}},
	}}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {
			matrixRoom("r101", "F101"),
			matrixRoom("r201", "F201"),
		},
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(now),
	})
	if err != nil {
		t.Fatalf("续跑不应报错，实际 %v", err)
	}
	if len(fetch.calls) != 1 || fetch.calls[0].BuildingID != "Q11" {
		t.Fatalf("续跑只应执行 Q11 的 pending 任务，实际 %+v", fetch.calls)
	}
	if progress.SkippedDone != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}
	if fmt.Sprint(roomStub.loadCalls) != fmt.Sprint([]int64{7}) {
		t.Fatalf("续跑应按 run_id 读回归属快照，实际 %v", roomStub.loadCalls)
	}

	// r101 仍归 Q10：Q11 那次落库只能有 r201，不能把 r101 写成 Q11。
	if len(roomStub.attributions) != 1 {
		t.Fatalf("Q10 已完成不应重放，实际落库 %+v", roomStub.attributions)
	}
	attribution := roomStub.attributions[0]
	if attribution.BuildingID != "Q11" || fmt.Sprint(attributionRoomIDs(attribution)) != fmt.Sprint([]string{"r201"}) {
		t.Fatalf("Q11 只能落 r201，实际 %+v", attribution)
	}

	// 每轮归属快照：r201 归 Q11，r101 保持 Q10 并打上冲突标记。
	wantRunRooms := []store.RunRoom{
		{RoomID: "r201", BuildingID: "Q11"},
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
	}
	if len(roomStub.runRooms) != 1 || fmt.Sprint(roomStub.runRooms[0].rooms) != fmt.Sprint(wantRunRooms) {
		t.Fatalf("续跑每轮归属不符: %+v", roomStub.runRooms)
	}

	// 冲突不能因为 Q10 已完成而漏记。
	if len(roomStub.conflicts) != 1 || roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("续跑应补记 1 次冲突，实际 %+v", roomStub.conflicts)
	}
}

// TestRunResumeKeepsConflictCount 覆盖续跑冲突计数：库里的旧冲突先被 Seed 带上，
// 新冲突之后写入的是「旧 + 新」的累计值，不会把 GREATEST 已抬高的计数写小。
func TestRunResumeKeepsConflictCount(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{loaded: map[int64][]store.RunRoom{
		7: {
			{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
			{RoomID: "r102", BuildingID: "Q10"},
		},
	}}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {
			matrixRoom("r101", "F101"),
			matrixRoom("r102", "F102"),
		},
	}}

	if _, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("续跑不应报错，实际 %v", err)
	}

	// 先用 Seed 补写旧计数 1，再把旧 1 + 新 1 = 2 写一次；不能只写 1。
	if len(roomStub.conflicts) != 2 ||
		roomStub.conflicts[0].conflicts != 1 ||
		roomStub.conflicts[1].conflicts != 2 {
		t.Fatalf("续跑冲突计数应为 1 → 2，实际 %+v", roomStub.conflicts)
	}
	if len(roomStub.attributions) != 0 {
		t.Fatalf("两个房号都已被 Q10 占用，不应再写归属，实际 %+v", roomStub.attributions)
	}
}

// TestRunResumeRewritesSeededConflictCount 覆盖崩溃窗口：collect_run_room 已打
// 冲突标记、collect_run.attr_conflict_count 还没写就崩了。续跑重放同一份响应时
// Offer 不会再把它算成新冲突，必须靠 Seed 后的补写把计数补上。
func TestRunResumeRewritesSeededConflictCount(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{loaded: map[int64][]store.RunRoom{
		7: {{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"}},
	}}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	if _, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("续跑不应报错，实际 %v", err)
	}

	if len(roomStub.conflicts) != 1 || roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("续跑应补写已落库的冲突计数 1，实际 %+v", roomStub.conflicts)
	}
	if len(roomStub.attributions) != 0 {
		t.Fatalf("续跑重放只能丢弃 r101，实际 %+v", roomStub.attributions)
	}
	// 冲突标记已在库里，重放这份响应没有新的归属变化，不需要再写一次快照。
	if len(roomStub.runRooms) != 0 {
		t.Fatalf("没有新增归属时不应重写快照，实际 %+v", roomStub.runRooms)
	}
}

// TestRunFailsWeekTaskWhenAttributionResponseInvalid 覆盖数据类错误分层：
// 响应内重复房号只让该任务走失败路径（写失败周结果 + 置 failed），
// 后面的楼照常执行；只有仓储写入失败才中止整轮。
func TestRunFailsWeekTaskWhenAttributionResponseInvalid(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {
			matrixRoom("r101", "F101"),
			matrixRoom("r101", "F101 重复"),
		},
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {
			matrixRoom("r201", "F201"),
		},
	}}

	phases := &phaseStoreStub{}
	progress, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: phases,
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("数据类错误不应中断整轮，实际 %v", err)
	}
	if progress.Failed != 1 || progress.Completed != 1 {
		t.Fatalf("进度不符: %+v", progress)
	}

	// 坏响应走周失败 + 任务失败，原因保留。
	if len(storeStub.weekFailures) != 1 {
		t.Fatalf("应为 Q10 写 1 条失败周结果，实际 %+v", storeStub.weekFailures)
	}
	failure := storeStub.weekFailures[0]
	if failure.BuildingID != "Q10" || failure.Week != 20 || !strings.Contains(failure.ErrorMessage, "重复") {
		t.Fatalf("失败周结果不符: %+v", failure)
	}
	// 响应内重复房号属于结构类数据错误，按矩阵结构失败写轮次错误码。
	if len(phases.codes) != 1 || phases.codes[0].code != "matrix_structure_invalid" {
		t.Fatalf("坏响应应写 matrix_structure_invalid，实际 %+v", phases.codes)
	}
	if len(storeStub.fails) != 1 || storeStub.fails[0].taskID != 1 {
		t.Fatalf("应把 Q10 任务置 failed，实际 %+v", storeStub.fails)
	}

	// 坏响应不落任何归属；Q11 照常成功。
	if len(roomStub.attributions) != 1 || roomStub.attributions[0].BuildingID != "Q11" {
		t.Fatalf("坏响应不应落归属，Q11 应照常落库，实际 %+v", roomStub.attributions)
	}
	if len(roomStub.runRooms) != 1 || roomStub.runRooms[0].rooms[0].BuildingID != "Q11" {
		t.Fatalf("每轮归属只应含 Q11，实际 %+v", roomStub.runRooms)
	}
	if len(storeStub.records) != 1 || storeStub.records[0].BuildingID != "Q11" {
		t.Fatalf("成功周结果只应有 Q11，实际 %+v", storeStub.records)
	}
}

// TestRunRequiresExplicitWhitelistOrder 覆盖 minor 7：归属白名单顺序必须显式传入，
// 且要与库里的周矩阵任务楼顺序一致；缺白名单或顺序不符时不发起请求。
func TestRunRequiresExplicitWhitelistOrder(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	cases := []struct {
		name      string
		whitelist []string
	}{
		{name: "缺少白名单", whitelist: nil},
		{name: "顺序颠倒", whitelist: []string{"Q11", "Q10"}},
		{name: "多出一栋", whitelist: []string{"Q10", "Q11", "Q12"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{persisted: persisted}
			fetch := &taskFetchStub{}
			if _, err := Run(context.Background(), 7, c.whitelist, nil, RunDeps{
				Phases: &phaseStoreStub{},
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			}); err == nil {
				t.Fatal("白名单顺序不合法时应报错")
			}
			if len(fetch.calls) != 0 {
				t.Fatalf("白名单校验失败时不应发起请求，实际 %+v", fetch.calls)
			}
			if want := []string{"ensure-week"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
				t.Fatalf("校验失败后不应再落占用说明段: 期望 %v，实际 %v", want, storeStub.events)
			}
		})
	}
}

// TestRunRejectsIllegalWhitelistBeforePersistingTasks 覆盖 minor 2：
// 白名单先规范化再落任务。空编号、纯空白编号、以及「排了周矩阵任务却没有白名单」
// 都直接报错，一条任务都不落库，避免非法白名单先写出无法用于裁决归属的任务。
func TestRunRejectsIllegalWhitelistBeforePersistingTasks(t *testing.T) {
	planned := []Task{{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20}}
	cases := []struct {
		name      string
		whitelist []string
	}{
		{name: "空编号", whitelist: []string{"Q10", ""}},
		{name: "只有空白", whitelist: []string{"   "}},
		{name: "缺白名单", whitelist: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{}
			fetch := &taskFetchStub{}
			if _, err := Run(context.Background(), 7, c.whitelist, planned, RunDeps{
				Phases: &phaseStoreStub{},
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			}); err == nil {
				t.Fatal("非法白名单应报错")
			}
			if len(storeStub.events) != 0 {
				t.Fatalf("非法白名单不应落任何任务，实际 %v", storeStub.events)
			}
			if len(fetch.calls) != 0 {
				t.Fatalf("非法白名单不应发起请求，实际 %+v", fetch.calls)
			}
		})
	}
}

// TestRunTrimsWhitelistEntries 覆盖 minor 1：白名单项两侧空白先 TrimSpace 再比对，
// 去空白后与库里周矩阵任务的楼顺序一致即可执行，归属仍取去空白后的楼编号。
func TestRunTrimsWhitelistEntries(t *testing.T) {
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	roomStub := &roomStoreStub{}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	progress, err := Run(context.Background(), 7, []string{" Q10 ", "\tQ11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("带空白但去空白后合法的白名单不应报错，实际 %v", err)
	}
	if progress.Completed != 2 {
		t.Fatalf("应完成 2 条任务，实际 %+v", progress)
	}
	// 裁决顺序仍按白名单：Q10 首见 r101，Q11 再报只能丢弃。
	if len(roomStub.attributions) != 1 || roomStub.attributions[0].BuildingID != "Q10" {
		t.Fatalf("r101 应归 Q10，实际 %+v", roomStub.attributions)
	}
	if len(roomStub.conflicts) != 1 || roomStub.conflicts[0].conflicts != 1 {
		t.Fatalf("Q11 报 r101 应记 1 次冲突，实际 %+v", roomStub.conflicts)
	}
}

// TestRunRequiresAscendingWeeksWithinBuilding 覆盖落库后复检：续跑时库里可能已有
// 上一轮留下的任务，顺序只认库里的权威清单——楼内周次颠倒、周次重复、
// 以及楼块交错（A1,B1,A2）都直接报错，不发起请求。
func TestRunRequiresAscendingWeeksWithinBuilding(t *testing.T) {
	cases := []struct {
		name      string
		whitelist []string
		persist   []store.SyncTask
		want      string
	}{
		{name: "周次颠倒", whitelist: []string{"Q10"}, want: "周次", persist: []store.SyncTask{
			weekTask(1, "Q10", 21, store.SyncTaskPending),
			weekTask(2, "Q10", 20, store.SyncTaskPending),
		}},
		{name: "周次重复", whitelist: []string{"Q10"}, want: "周次", persist: []store.SyncTask{
			weekTask(1, "Q10", 20, store.SyncTaskPending),
			weekTask(2, "Q10", 20, store.SyncTaskPending),
		}},
		{name: "楼块交错", whitelist: []string{"Q10", "Q11"}, want: "交错", persist: []store.SyncTask{
			weekTask(1, "Q10", 20, store.SyncTaskPending),
			weekTask(2, "Q11", 20, store.SyncTaskPending),
			weekTask(3, "Q10", 21, store.SyncTaskPending),
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{persisted: c.persist}
			fetch := &taskFetchStub{}
			_, err := Run(context.Background(), 7, c.whitelist, nil, RunDeps{
				Phases: &phaseStoreStub{},
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			})
			if err == nil {
				t.Fatal("周矩阵任务顺序不合法时应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应说明「%s」问题，实际 %v", c.want, err)
			}
			if len(fetch.calls) != 0 {
				t.Fatalf("顺序校验失败时不应发起请求，实际 %+v", fetch.calls)
			}
		})
	}
}

// TestRunRejectsBadWeekOrderBeforePersistingTasks 覆盖 minor 1：周矩阵段的
// 「楼内周次严格升序 + 楼块不交错」必须在 EnsureWeekMatrixTasks 之前校验。
// 坏计划一旦落库，续跑会读回同一份顺序并永久报错，改调用参数也救不回来；
// 这里要求一条任务都不写、一个请求都不发。
func TestRunRejectsBadWeekOrderBeforePersistingTasks(t *testing.T) {
	cases := []struct {
		name      string
		whitelist []string
		planned   []Task
		want      string
	}{
		{name: "楼内周次颠倒", whitelist: []string{"Q10"}, want: "周次", planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		}},
		{name: "楼内周次重复", whitelist: []string{"Q10"}, want: "周次", planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		}},
		{name: "楼块交错", whitelist: []string{"Q10", "Q11"}, want: "交错", planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 20},
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{}
			roomStub := &roomStoreStub{}
			fetch := &taskFetchStub{}
			_, err := Run(context.Background(), 7, c.whitelist, c.planned, RunDeps{
				Phases: &phaseStoreStub{},
				Tasks:  storeStub,
				Rooms:  roomStub,
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			})
			if err == nil {
				t.Fatal("规划里的周矩阵顺序不合法时应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误信息应说明「%s」问题，实际 %v", c.want, err)
			}
			if len(storeStub.events) != 0 {
				t.Fatalf("坏计划不应落库任何任务，实际 %v", storeStub.events)
			}
			if len(roomStub.loadCalls) != 0 || len(roomStub.attributions) != 0 || len(roomStub.runRooms) != 0 {
				t.Fatalf("坏计划不应读写归属，实际 %+v", roomStub)
			}
			if len(fetch.calls) != 0 {
				t.Fatalf("坏计划不应发起请求，实际 %+v", fetch.calls)
			}
		})
	}
}

// TestRunRejectsPlannedWhitelistMismatchBeforePersistingTasks 覆盖 minor 2：
// 落库前除了周次顺序，还要比对「规划里的周矩阵楼序列」与规范化后的白名单。
// 只校验周次挡不住楼错配（计划 Q10,Q11 配白名单 Q11,Q10），按错的顺序落库再按
// 白名单裁决会把归属判给后执行的楼；这里要求直接报错，一条任务都不写、不发请求。
func TestRunRejectsPlannedWhitelistMismatchBeforePersistingTasks(t *testing.T) {
	cases := []struct {
		name      string
		whitelist []string
		planned   []Task
	}{
		{name: "楼顺序颠倒", whitelist: []string{"Q11", "Q10"}, planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 20},
		}},
		{name: "白名单多一栋", whitelist: []string{"Q10", "Q11"}, planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		}},
		{name: "楼不同", whitelist: []string{"Q11"}, planned: []Task{
			{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{}
			roomStub := &roomStoreStub{}
			fetch := &taskFetchStub{}
			_, err := Run(context.Background(), 7, c.whitelist, c.planned, RunDeps{
				Phases: &phaseStoreStub{},
				Tasks:  storeStub,
				Rooms:  roomStub,
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			})
			if err == nil {
				t.Fatal("规划楼顺序与白名单不一致时应报错")
			}
			if !strings.Contains(err.Error(), "不一致") {
				t.Fatalf("错误信息应说明楼顺序不一致，实际 %v", err)
			}
			if len(storeStub.events) != 0 {
				t.Fatalf("白名单错配不应落库任何任务，实际 %v", storeStub.events)
			}
			if len(roomStub.loadCalls) != 0 || len(roomStub.attributions) != 0 || len(roomStub.runRooms) != 0 {
				t.Fatalf("白名单错配不应读写归属，实际 %+v", roomStub)
			}
			if len(fetch.calls) != 0 {
				t.Fatalf("白名单错配不应发起请求，实际 %+v", fetch.calls)
			}
		})
	}
}

// TestRunSkipsPlannedWhitelistCheckWhenNoWeekTasks 覆盖 minor 2 的跳过分支：
// 本轮没有周矩阵任务时，白名单不参与裁决，允许调用方传入（比如续跑只跑明细段），
// 不因「计划楼序列为空」被误判成错配。
func TestRunSkipsPlannedWhitelistCheckWhenNoWeekTasks(t *testing.T) {
	storeStub := &taskStoreStub{}
	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Fetch:  (&taskFetchStub{}).fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if err != nil {
		t.Fatalf("没有周矩阵任务时白名单应被跳过，实际 %v", err)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("空计划不应产生进度，实际 %+v", progress)
	}
	if want := []string{"ensure-week", "ensure-cell"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("调用顺序不符: 期望 %v，实际 %v", want, storeStub.events)
	}
}

// TestRunReturnsErrorWhenEnsureCellDetailTasksFails 覆盖占用说明段落库失败：
// 周矩阵段已完成的任务保持 done，错误上抛，不再发起请求。
func TestRunReturnsErrorWhenEnsureCellDetailTasksFails(t *testing.T) {
	ensureErr := errors.New("写占用说明任务失败")
	storeStub := &taskStoreStub{
		persisted:     []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskDone)},
		ensureCellErr: ensureErr,
	}
	phases := &phaseStoreStub{}
	fetch := &taskFetchStub{}

	_, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: phases,
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, ensureErr) {
		t.Fatalf("应上抛占用说明段落库错误，实际 %v", err)
	}
	if len(fetch.calls) != 0 {
		t.Fatalf("占用说明段落库失败时不应发起请求，实际 %+v", fetch.calls)
	}
	if want := []string{"ensure-week", "ensure-cell"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("调用顺序不符: 期望 %v，实际 %v", want, storeStub.events)
	}
	// minor 3：周矩阵段已落库、阶段已到 week_matrix；占用说明段没落库，
	// 阶段不能推进到 cell_detail。
	wantPhases := []string{string(PhaseWeekMatrix)}
	if got := phaseNames(phases.phases); fmt.Sprint(got) != fmt.Sprint(wantPhases) {
		t.Fatalf("占用说明段落库失败时阶段应停在 %v，实际 %v", wantPhases, got)
	}
}

// phaseNames 取阶段推进的入参阶段名，便于断言阶段序列。
func phaseNames(calls []phaseCall) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.phase)
	}
	return names
}

// TestRunReturnsErrorWhenUpsertRunRoomsFails 覆盖每轮归属写入失败：
// 属于编排错误，立即上抛；周结果与任务状态都不写，任务保持 pending 供续跑重做。
func TestRunReturnsErrorWhenUpsertRunRoomsFails(t *testing.T) {
	runRoomErr := errors.New("写每轮归属失败")
	storeStub := &taskStoreStub{persisted: []store.SyncTask{weekTask(1, "Q10", 20, store.SyncTaskPending)}}
	roomStub := &roomStoreStub{runRoomErr: runRoomErr}
	fetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
	}}

	progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  roomStub,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, runRoomErr) {
		t.Fatalf("应上抛每轮归属写入错误，实际 %v", err)
	}
	if len(storeStub.records) != 0 || len(storeStub.completes) != 0 {
		t.Fatalf("归属写失败时不应写周结果或置 done: records=%+v completes=%+v",
			storeStub.records, storeStub.completes)
	}
	if progress != (RunProgress{}) {
		t.Fatalf("归属写失败时进度应全零，实际 %+v", progress)
	}
}

// TestRunRejectsUnknownTaskKind 覆盖规划口径：未知任务种类不静默丢弃，直接报错。
func TestRunRejectsUnknownTaskKind(t *testing.T) {
	storeStub := &taskStoreStub{}
	if _, err := Run(context.Background(), 7, nil, []Task{{Kind: TaskKind("unknown")}}, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Fetch:  (&taskFetchStub{}).fetch,
	}); err == nil {
		t.Fatal("未知任务种类应报错")
	}
	if len(storeStub.events) != 0 {
		t.Fatalf("规划校验失败时不应落库，实际 %v", storeStub.events)
	}
}

// TestRunDoesNotReuseAttributionAcrossRuns 覆盖跨轮隔离：归属累计器按 Run 调用
// 重建，collect_run_room 的读回只按传入的 run_id 生效，新轮不从上一轮继承。
//
// 注意这里只测内存累计器：替身不模拟 room 目录，所以第 8 轮把 r101 判给 Q11 只是
// 累计器的结论。真实仓储里跨轮换楼是允许的——room.building_id 被新轮直接覆盖，
// 不报错也不改判（口径见 store 的
// TestUpsertRoomAttributionOverwritesBuildingAcrossRoundsAgainstRealDB），
// 轮内的首个归属由 collect_run_room 的当轮快照保住。
func TestRunDoesNotReuseAttributionAcrossRuns(t *testing.T) {
	// 第 7 轮续跑：Q10 已完成，Q11 再报 r101 应被丢弃。
	firstRun := &taskStoreStub{persisted: []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q11", 20, store.SyncTaskPending),
	}}
	firstRooms := &roomStoreStub{loaded: map[int64][]store.RunRoom{
		7: {{RoomID: "r101", BuildingID: "Q10"}},
	}}
	firstFetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}
	if _, err := Run(context.Background(), 7, []string{"Q10", "Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  firstRun,
		Rooms:  firstRooms,
		Fetch:  firstFetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("第 7 轮续跑不应报错，实际 %v", err)
	}
	if len(firstRooms.attributions) != 0 {
		t.Fatalf("第 7 轮 r101 应被丢弃，实际 %+v", firstRooms.attributions)
	}

	// 第 8 轮是全新一轮：累计器里没有 r101 的既有归属，Q11 首见即判给它。
	// 真实仓储同样允许这次写入：room.building_id 被新轮覆盖，上一轮的快照不动（见上）。
	secondRun := &taskStoreStub{persisted: []store.SyncTask{
		weekTask(1, "Q11", 20, store.SyncTaskPending),
	}}
	secondRooms := &roomStoreStub{}
	secondFetch := &taskFetchStub{rooms: map[string][]model.MatrixRoom{
		taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q11", Week: 20}): {matrixRoom("r101", "F101")},
	}}
	if _, err := Run(context.Background(), 8, []string{"Q11"}, nil, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  secondRun,
		Rooms:  secondRooms,
		Fetch:  secondFetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	}); err != nil {
		t.Fatalf("第 8 轮不应报错，实际 %v", err)
	}
	if len(secondRooms.attributions) != 1 || secondRooms.attributions[0].BuildingID != "Q11" {
		t.Fatalf("新轮不应继承上一轮归属，实际 %+v", secondRooms.attributions)
	}
	if len(secondRooms.conflicts) != 0 {
		t.Fatalf("新轮不应记上一轮的冲突，实际 %+v", secondRooms.conflicts)
	}
}

// TestAttributedRoomsFallsBackDisplayName 覆盖 minor 6：展示名为空时回退原始名，
// 原始名也为空时回退房号，不把空串写进 room.name。
func TestAttributedRoomsFallsBackDisplayName(t *testing.T) {
	rooms := attributedRooms([]model.MatrixRoom{
		{RoomID: "r101", NameRaw: "F101(60/0)"},
		{RoomID: "r102", NameRaw: ""},
		{RoomID: "r103", NameRaw: "   "},
	})
	wantNames := []string{"F101", "r102", "r103"}
	for index, name := range wantNames {
		if rooms[index].Name != name {
			t.Fatalf("第 %d 个展示名期望 %q，实际 %q", index+1, name, rooms[index].Name)
		}
		if strings.TrimSpace(rooms[index].Name) == "" {
			t.Fatalf("第 %d 个展示名不应为空串", index+1)
		}
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

// TestRunWritesStructuralErrorCodeOnTaskFailure 覆盖用户拍板 1：任务失败时按
// errors.Is 哨兵映射出具体 error_code 写进轮次；网络、超时这类非结构失败不写码。
// 任务失败本身仍照常置 failed，原因原样落库。
func TestRunWritesStructuralErrorCodeOnTaskFailure(t *testing.T) {
	cases := []struct {
		name     string
		cause    error
		wantCode string
	}{
		{
			name:     "矩阵结构失败",
			cause:    fmt.Errorf("解析周矩阵失败: %w", parser.ErrMatrixStructureInvalid),
			wantCode: "matrix_structure_invalid",
		},
		{
			name:     "明细结构失败",
			cause:    fmt.Errorf("解析占用说明失败: %w", parser.ErrDetailStructureInvalid),
			wantCode: "detail_structure_invalid",
		},
		{
			name:     "轴变化",
			cause:    fmt.Errorf("节次轴不可用: %w", parser.ErrAxisInvalid),
			wantCode: "axis_changed",
		},
		{
			name:     "网络失败",
			cause:    fmt.Errorf("请求失败: %w", model.ErrUpstreamFailed),
			wantCode: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := shanghaiMorning()
			storeStub := &taskStoreStub{persisted: []store.SyncTask{
				weekTask(1, "Q10", 20, store.SyncTaskPending),
			}}
			phases := &phaseStoreStub{}
			fetch := &taskFetchStub{errs: map[string]error{
				taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): c.cause,
			}}

			progress, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
				Phases: phases,
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Fetch:  fetch.fetch,
				Clock:  fixedClock(now),
			})
			if err != nil {
				t.Fatalf("任务失败不应让执行循环报错，实际 %v", err)
			}
			if progress.Failed != 1 {
				t.Fatalf("任务应记 1 次失败，实际 %+v", progress)
			}
			if len(storeStub.fails) != 1 || storeStub.fails[0].message != c.cause.Error() {
				t.Fatalf("失败原因应原样落库，实际 %+v", storeStub.fails)
			}

			if c.wantCode == "" {
				if len(phases.codes) != 0 {
					t.Fatalf("非结构失败不应写错误码，实际 %+v", phases.codes)
				}
				return
			}
			if len(phases.codes) != 1 {
				t.Fatalf("结构失败应写 1 个错误码，实际 %+v", phases.codes)
			}
			call := phases.codes[0]
			if call.runID != 7 || call.code != c.wantCode || !call.at.Equal(now) {
				t.Fatalf("错误码入参不符: %+v（期望 %s）", call, c.wantCode)
			}
		})
	}
}
