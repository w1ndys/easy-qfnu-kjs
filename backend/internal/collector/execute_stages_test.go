package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// stageFake 是两段式编排用例的替身：在 execFake（设置、学期页、快照、窗口）之上
// 补一份可用的内存版任务/归属/阶段仓储，并把上游矩阵与明细改成按预置返回。
//
// 它替掉 stub 里会 panic 的两类方法，其余仍走 stub：本组用例若意外碰到发布
// （LoadRunFacts / CommitRelease）或 hydrate（LoadWeekMatrixTasks），仍会炸在调用点，
// 而不是拿到零值后悄悄通过。
//
// 任务状态与归属都按 store 的语义实现到「够 pipeline.Run 跑完」为止：两段 Ensure
// 遇到同类任务已存在时不再写入、从第一个 pending 继续、done/failed 不重试、
// collect_run_room 以房号覆盖。真实 SQL 行为由 store 的测试覆盖，这里只锁编排口径。
type stageFake struct {
	*execFake

	currentPhase string                   // collect_run.phase，随 SetRunPhase 前进
	tasks        []store.SyncTask         // 两段任务，按写入顺序（即 task_id 升序）
	taskMessages map[int64]string         // 失败任务的原因，按 task_id
	errorCode    string                   // 轮次已记录的第一个错误码
	rooms        map[string]store.RunRoom // collect_run_room 快照

	loadRoomsErr      error  // LoadRunRooms 的错误
	loadRoomsErrAt    int    // 只让第 N 次（从 1 起）LoadRunRooms 报错；0 表示每次都报错
	loadRoomsCalls    int    // LoadRunRooms 的调用次数
	loadRoomsDropRoom string // 从 LoadRunRooms 的返回里剔除的房号；空串表示不过滤（造孤儿格）

	// weekInputs 按调用次序记录每次 EnsureWeekMatrixTasks 的入参：下标 0 是 Run1 收到
	// 的 Plan1 周矩阵段，下标 1 是 Run2 收到的 Plan2 周矩阵段。两段的周矩阵段本应
	// 相同（同窗口、同白名单），分开记录才能证明「两次 Run 各自收到的是自己那一段
	// 规划的输出」，而不是第二次复用了第一次的入参。
	weekInputs [][]store.SyncTaskInput
	cellInputs []store.SyncTaskInput // 最近一次非空 EnsureCellDetailTasks 的入参（Run2）

	matrixBody   []byte                           // FetchWeekMatrix 的默认返回正文
	matrixBodies map[string][]byte                // 按楼编号覆盖返回正文（多楼用例）
	matrixErr    error                            // FetchWeekMatrix 的返回错误
	matrixCalls  []upstream.WeekMatrixParams      // 每次矩阵请求的入参
	detailBody   []byte                           // FetchOccupancyDetail 的返回正文
	detailErr    error                            // FetchOccupancyDetail 的返回错误
	detailCalls  []upstream.OccupancyDetailParams // 每次明细请求的入参

	summaryCalls int // LoadRunTaskSummary 的调用次数：两段各一次 = 2
}

// newStageFake 返回一份「白名单只有 Q10、学期页与周历页可解析、快照齐备」的替身。
// 单楼白名单让任务编号与事件序列短到可以逐条断言；多楼用例（归属重写）在用例里
// 自行扩展白名单与 matrixBodies。
func newStageFake() *stageFake {
	f := newExecuteFake()
	f.snapshot.Whitelist = []model.Building{{ID: "Q10", Name: "综合楼"}}
	return &stageFake{
		execFake:     f,
		currentPhase: string(pipeline.PhaseLogin),
		taskMessages: make(map[int64]string),
		rooms:        make(map[string]store.RunRoom),
	}
}

// stageDeps 用替身填满 Deps；Clock 与 executeDeps 一样固定在 executeNow。
func stageDeps(f *stageFake) Deps {
	return Deps{
		Runs:      f,
		Terms:     f,
		Snapshots: f,
		Phases:    f,
		Tasks:     f,
		TaskRead:  f,
		Rooms:     f,
		Reader:    f,
		Committer: f,
		Settings:  f,
		Upstream:  f,
		RunCtx:    context.Background(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return executeNow },
	}
}

// newStageExecutor 组装执行器；Deps 漏填时 New 会报错，说明用例自己写漏了。
func newStageExecutor(t *testing.T, f *stageFake) *Collector {
	t.Helper()
	c, err := New(stageDeps(f))
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}
	return c
}

// newStageExecutorWithLog 组装带自定义日志的执行器：孤儿格用例要把 WARN 写进
// bytes.Buffer 再逐条断言，其余依赖与 newStageExecutor 完全相同。
func newStageExecutorWithLog(t *testing.T, f *stageFake, log *slog.Logger) *Collector {
	t.Helper()
	deps := stageDeps(f)
	deps.Log = log
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}
	return c
}

// SetRunPhase 推进阶段并记到达顺序。
func (s *stageFake) SetRunPhase(_ context.Context, _ int64, phase string, _ []string, _ time.Time) error {
	s.record("phase:" + phase)
	s.currentPhase = phase
	return nil
}

// LoadRunPhase 始终报告 running：本组用例不构造已收口的轮次。
func (s *stageFake) LoadRunPhase(_ context.Context, _ int64) (string, string, error) {
	return s.currentPhase, string(pipeline.StatusRunning), nil
}

// RecordRunErrorCode 只保留第一个码，与 store 的 COALESCE 口径一致。
func (s *stageFake) RecordRunErrorCode(_ context.Context, _ int64, code string, _ time.Time) error {
	s.record("code:" + code)
	if s.errorCode == "" {
		s.errorCode = code
	}
	return nil
}

// LoadRunTaskSummary 按 tasks 现状聚合，与 store.LoadRunTaskSummary 的列口径一致。
func (s *stageFake) LoadRunTaskSummary(_ context.Context, _ int64) (store.RunTaskSummary, error) {
	s.record("summary")
	s.summaryCalls++

	summary := store.RunTaskSummary{RecordedErrorCode: s.errorCode}
	var firstFailure *store.SyncTask
	for index := range s.tasks {
		task := &s.tasks[index]
		if task.Required {
			summary.RequiredTotal++
			switch task.Status {
			case store.SyncTaskDone:
				summary.RequiredDone++
			case store.SyncTaskFailed:
				summary.RequiredFailed++
			}
		} else {
			summary.OptionalTotal++
			switch task.Status {
			case store.SyncTaskDone:
				summary.OptionalDone++
			case store.SyncTaskFailed:
				summary.OptionalFailed++
			}
		}
		if task.Status == store.SyncTaskFailed && firstFailure == nil {
			firstFailure = task
		}
	}
	if firstFailure != nil {
		summary.FirstFailureMessage = s.taskMessages[firstFailure.TaskID]
	}
	return summary, nil
}

// EnsureWeekMatrixTasks 按调用次序记录入参（Run1 的 Plan1、Run2 的 Plan2）并返回
// 库里该段任务。
func (s *stageFake) EnsureWeekMatrixTasks(_ context.Context, _ int64, tasks []store.SyncTaskInput, _ time.Time) ([]store.SyncTask, error) {
	s.record("ensure_week")
	s.weekInputs = append(s.weekInputs, append([]store.SyncTaskInput(nil), tasks...))
	return s.ensure(store.SyncTaskKindWeekMatrix, tasks), nil
}

// EnsureCellDetailTasks 记录最近一次非空入参（Run2 的规划）并返回库里该段任务。
func (s *stageFake) EnsureCellDetailTasks(_ context.Context, _ int64, tasks []store.SyncTaskInput, _ time.Time) ([]store.SyncTask, error) {
	s.record("ensure_cell")
	if len(tasks) > 0 {
		s.cellInputs = append([]store.SyncTaskInput(nil), tasks...)
	}
	return s.ensure(store.SyncTaskKindCellDetail, tasks), nil
}

// ensure 模拟 store 的两段写入：该 kind 已有任务时整批不写，否则按入参顺序追加为
// pending；返回该 kind 的全部任务，保持 task_id 升序。
func (s *stageFake) ensure(kind string, inputs []store.SyncTaskInput) []store.SyncTask {
	if len(inputs) > 0 && !s.hasKind(kind) {
		for _, input := range inputs {
			s.tasks = append(s.tasks, store.SyncTask{
				TaskID:     int64(len(s.tasks) + 1),
				Kind:       input.Kind,
				Required:   input.Required,
				BuildingID: input.BuildingID,
				Week:       input.Week,
				RoomID:     input.RoomID,
				Weekday:    input.Weekday,
				Block:      input.Block,
				Status:     store.SyncTaskPending,
			})
		}
	}
	return s.tasksOfKind(kind)
}

// hasKind 报告该段是否已经落过任务。
func (s *stageFake) hasKind(kind string) bool {
	for _, task := range s.tasks {
		if task.Kind == kind {
			return true
		}
	}
	return false
}

// tasksOfKind 按 kind 过滤，保持 task_id 升序。
func (s *stageFake) tasksOfKind(kind string) []store.SyncTask {
	filtered := make([]store.SyncTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		if task.Kind == kind {
			filtered = append(filtered, task)
		}
	}
	return filtered
}

// RecordWeekFetch 记周结果到达顺序。
func (s *stageFake) RecordWeekFetch(_ context.Context, fetch store.RunWeekFetch) error {
	s.record(fmt.Sprintf("record:%d", fetch.Week))
	return nil
}

// RecordWeekFailure 记周失败结果到达顺序。
func (s *stageFake) RecordWeekFailure(_ context.Context, failure store.RunWeekFailure) error {
	s.record(fmt.Sprintf("record_fail:%d", failure.Week))
	return nil
}

// CompleteSyncTask 置 done。
func (s *stageFake) CompleteSyncTask(_ context.Context, _ int64, taskID int64, _ time.Time) error {
	s.record(fmt.Sprintf("complete:%d", taskID))
	for index := range s.tasks {
		if s.tasks[index].TaskID == taskID {
			s.tasks[index].Status = store.SyncTaskDone
		}
	}
	return nil
}

// FailSyncTask 置 failed 并记原因。
func (s *stageFake) FailSyncTask(_ context.Context, _ int64, taskID int64, message string, _ time.Time) error {
	s.record(fmt.Sprintf("fail:%d", taskID))
	for index := range s.tasks {
		if s.tasks[index].TaskID == taskID {
			s.tasks[index].Status = store.SyncTaskFailed
			s.taskMessages[taskID] = message
		}
	}
	return nil
}

// UpsertRoomAttribution 只记到达顺序：归属细节由 collect_run_room 快照表达。
func (s *stageFake) UpsertRoomAttribution(_ context.Context, _ store.RoomAttribution) error {
	s.record("rooms")
	return nil
}

// UpsertRunRooms 按房号覆盖本轮归属快照。
func (s *stageFake) UpsertRunRooms(_ context.Context, _ int64, rooms []store.RunRoom, _ time.Time) error {
	s.record("run_rooms")
	for _, room := range rooms {
		s.rooms[room.RoomID] = room
	}
	return nil
}

// LoadRunRooms 返回按房号升序的快照，保证导出顺序可复现。错误只在第
// loadRoomsErrAt 次调用上出现（0 表示每次都报错）：Run1 的 seedAttribution 与
// 第二段规划前的读取是两次独立调用，用例要能只打挂后者。
//
// loadRoomsDropRoom 非空时从返回里剔除该房号：本轮归属明明写过它，快照读回来却
// 没有，于是该房号的不可用格在导出时找不到归属楼，变成孤儿格（Requirement 8.5）。
// 过滤作用于每次调用；新建轮次时第 1 次（seedAttribution）本来就是空快照，不受影响。
func (s *stageFake) LoadRunRooms(_ context.Context, _ int64) ([]store.RunRoom, error) {
	s.record("load_rooms")
	s.loadRoomsCalls++
	if s.loadRoomsErr != nil && (s.loadRoomsErrAt == 0 || s.loadRoomsCalls == s.loadRoomsErrAt) {
		return nil, s.loadRoomsErr
	}
	rooms := make([]store.RunRoom, 0, len(s.rooms))
	for _, room := range s.rooms {
		if s.loadRoomsDropRoom != "" && room.RoomID == s.loadRoomsDropRoom {
			continue
		}
		rooms = append(rooms, room)
	}
	slices.SortFunc(rooms, func(a, b store.RunRoom) int {
		return strings.Compare(a.RoomID, b.RoomID)
	})
	return rooms, nil
}

// RecordAttrConflicts 不是本组用例的重点，只做到不 panic。
func (s *stageFake) RecordAttrConflicts(_ context.Context, _ int64, _ int, _ time.Time) error {
	return nil
}

// FetchWeekMatrix 返回预置正文并记入参。
func (s *stageFake) FetchWeekMatrix(_ context.Context, params upstream.WeekMatrixParams) ([]byte, error) {
	s.record("fetch_matrix")
	s.matrixCalls = append(s.matrixCalls, params)
	if s.matrixErr != nil {
		return nil, s.matrixErr
	}
	if body, ok := s.matrixBodies[params.BuildingID]; ok {
		return body, nil
	}
	return s.matrixBody, nil
}

// FetchOccupancyDetail 返回预置正文并记入参。
func (s *stageFake) FetchOccupancyDetail(_ context.Context, params upstream.OccupancyDetailParams) ([]byte, error) {
	s.record("fetch_detail")
	s.detailCalls = append(s.detailCalls, params)
	if s.detailErr != nil {
		return nil, s.detailErr
	}
	return s.detailBody, nil
}

// stageDetailPage 是一份含四项字段的明细页：课程、周次、时间标志各一行。
// 页面结构由 fetch_test.go 的 detailFixturePage 统一拼装，本组用例只挑字段。
func stageDetailPage() []byte {
	return detailFixturePage(
		detailFieldRow("教室状态：", "正常上课"),
		detailFieldRow("课程：", "高等数学"),
		detailFieldRow("周次：", "1-16"),
		detailFieldRow("时间标志：", "单双周"),
	)
}

// syncTaskInputsOf 把规划结果摊成落库入参，用来与 pipeline.Run 实际交给 Ensure 的
// 入参逐项比对：等价于「Run 收到的就是 Plan 的输出」。
func syncTaskInputsOf(tasks []pipeline.Task) []store.SyncTaskInput {
	inputs := make([]store.SyncTaskInput, 0, len(tasks))
	for _, task := range tasks {
		inputs = append(inputs, store.SyncTaskInput{
			Kind:       string(task.Kind),
			Required:   task.Required,
			BuildingID: task.BuildingID,
			Week:       task.Week,
			RoomID:     task.RoomID,
			Weekday:    task.Weekday,
			Block:      task.Block,
		})
	}
	return inputs
}

// cellInputsOf 只取规划里的明细段：Plan 恒返回「周矩阵段 + 明细段」，而第二段的
// EnsureCellDetailTasks 只收明细那一段，比较时必须过滤掉周矩阵任务。
func cellInputsOf(tasks []pipeline.Task) []store.SyncTaskInput {
	inputs := make([]store.SyncTaskInput, 0, len(tasks))
	for _, input := range syncTaskInputsOf(tasks) {
		if input.Kind == store.SyncTaskKindCellDetail {
			inputs = append(inputs, input)
		}
	}
	return inputs
}

// weekMatrixInputsOf 只取规划里的周矩阵段：Plan 的第二段输出同时含周矩阵与明细，
// EnsureWeekMatrixTasks 只收前一段，比较时同样要按种类过滤。
func weekMatrixInputsOf(tasks []pipeline.Task) []store.SyncTaskInput {
	inputs := make([]store.SyncTaskInput, 0, len(tasks))
	for _, input := range syncTaskInputsOf(tasks) {
		if input.Kind == store.SyncTaskKindWeekMatrix {
			inputs = append(inputs, input)
		}
	}
	return inputs
}

// logRecord 是一条 JSON 日志里断言用到的字段：级别、消息与两个固定字段。
type logRecord struct {
	Level       string `json:"level"`
	Msg         string `json:"msg"`
	RunID       int64  `json:"run_id"`
	OrphanCells int    `json:"orphan_cells"`
}

// jsonLogRecords 把 JSONHandler 写进缓冲区的每行日志解出来；没有输出时返回空切片。
func jsonLogRecords(t *testing.T, buf *bytes.Buffer) []logRecord {
	t.Helper()
	records := make([]logRecord, 0, bytes.Count(buf.Bytes(), []byte("\n")))
	decoder := json.NewDecoder(buf)
	for {
		var record logRecord
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			return records
		}
		if err != nil {
			t.Fatalf("JSON 日志解析失败：%v（原始输出 %q）", err, buf.String())
		}
		records = append(records, record)
	}
}

// warnsOf 只留 WARN 级别的日志记录，其余级别不参与「孤儿格只记一条 WARN」的断言。
func warnsOf(records []logRecord) []logRecord {
	warns := make([]logRecord, 0, len(records))
	for _, record := range records {
		if record.Level == "WARN" {
			warns = append(warns, record)
		}
	}
	return warns
}

// nthEvent 返回事件第 n 次（从 0 起）出现的位置；没出现时让用例直接失败。
func nthEvent(t *testing.T, events []string, event string, n int) int {
	t.Helper()
	seen := 0
	for index, got := range events {
		if got != event {
			continue
		}
		if seen == n {
			return index
		}
		seen++
	}
	t.Fatalf("事件 %q 第 %d 次没有出现，实际序列 %v", event, n+1, events)
	return -1
}

// TestExecuteTwoStagesInOrder 覆盖 tasklist 第 8 项的第一条单测：
// Plan1 → Run1 → EvaluateRun → Plan2 → Run2 的调用顺序。
//
// 顺序的观测点：Run1 的第一条仓储写入（ensure_week）、第一段收口判定（summary）、
// 第二段规划前读归属快照（load_rooms）、Run2 的第一条写入（第二次 ensure_week）、
// 明细请求（fetch_detail）、第二段收口判定（第二次 summary）。Plan 是纯函数，
// 它的输出由「Run 实际收到的入参 == 用同样输入重算的 Plan 输出」来证明；weekInputs
// 按调用次序记录两次 Ensure，分别对应 Plan1 与 Plan2 的周矩阵段。
func TestExecuteTwoStagesInOrder(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = stageDetailPage()

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("两段式执行不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("success/partial 的收口属于发布事务（第 10 项），本项不应提前收口，实际 %+v", finishes)
	}

	// 顺序：Run1 → 第一段判定 → 读归属 → Run2 → 明细 → 第二段判定。
	run1 := nthEvent(t, f.events, "ensure_week", 0)
	firstMatrix := nthEvent(t, f.events, "fetch_matrix", 0)
	evaluate1 := nthEvent(t, f.events, "summary", 0)
	plan2Read := nthEvent(t, f.events, "load_rooms", 1) // 第 0 次是 Run1 的归属重灌
	run2 := nthEvent(t, f.events, "ensure_week", 1)
	detail := nthEvent(t, f.events, "fetch_detail", 0)
	evaluate2 := nthEvent(t, f.events, "summary", 1)
	if run1 >= firstMatrix || firstMatrix >= evaluate1 ||
		evaluate1 >= plan2Read || plan2Read >= run2 || run2 >= detail || detail >= evaluate2 {
		t.Fatalf("两段式顺序应为 Plan1 → Run1 → EvaluateRun → Plan2 → Run2 → EvaluateRun，实际序列 %v", f.events)
	}
	if f.summaryCalls != 2 {
		t.Fatalf("两段各应做一次收口判定，实际 %d 次", f.summaryCalls)
	}

	// 第一段只规划周矩阵：Q10 × 窗口 9..12 共 4 条，Run1 收到的就是这份清单。
	window := pipeline.WindowFor(9, 16)
	buildings := []string{"Q10"}
	plan1 := pipeline.Plan(window, buildings, nil)

	// 第二段：不可用格只有一个（R0001 × 周一 × 0102），Run2 收到的就是 Plan2 的明细段。
	wantCells := []pipeline.Cell{{
		BuildingID: "Q10", RoomID: "R0001", Weekday: 1, Block: "0102",
	}}
	plan2 := pipeline.Plan(window, buildings, wantCells)

	// weekInputs 按调用次序记录：第 1 次是 Run1 的 Plan1 周矩阵段，第 2 次是 Run2 的
	// Plan2 周矩阵段。Plan2 的输出同时含明细段，EnsureWeekMatrixTasks 只收周矩阵那一段，
	// 所以两次都要先按种类过滤再比较。
	if len(f.weekInputs) != 2 {
		t.Fatalf("两段各应写一次周矩阵任务，期望 2 次 Ensure，实际 %d 次：%+v",
			len(f.weekInputs), f.weekInputs)
	}
	if !slices.Equal(f.weekInputs[0], weekMatrixInputsOf(plan1.Tasks)) {
		t.Fatalf("第 1 次 Ensure 的入参应为 Plan1 的周矩阵段 %+v，实际 %+v",
			weekMatrixInputsOf(plan1.Tasks), f.weekInputs[0])
	}
	if !slices.Equal(f.weekInputs[1], weekMatrixInputsOf(plan2.Tasks)) {
		t.Fatalf("第 2 次 Ensure 的入参应为 Plan2 的周矩阵段 %+v，实际 %+v",
			weekMatrixInputsOf(plan2.Tasks), f.weekInputs[1])
	}
	if len(f.weekInputs[0]) != 4 || len(f.weekInputs[1]) != 4 {
		t.Fatalf("窗口 9..12 两段都应含 4 条周矩阵任务，实际 %d / %d 条：%+v",
			len(f.weekInputs[0]), len(f.weekInputs[1]), f.weekInputs)
	}
	if len(f.matrixCalls) != len(plan1.Tasks) {
		t.Fatalf("每 (楼,周) 应请求一次矩阵，期望 %d 次，实际 %d 次：%+v",
			len(plan1.Tasks), len(f.matrixCalls), f.matrixCalls)
	}

	wantCellInputs := cellInputsOf(plan2.Tasks)
	if !slices.Equal(f.cellInputs, wantCellInputs) {
		t.Fatalf("Run2 的入参应为 Plan2 的明细段 %+v，实际 %+v", wantCellInputs, f.cellInputs)
	}
	if len(f.cellInputs) != 1 {
		t.Fatalf("应恰好规划一条明细任务，实际 %d 条：%+v", len(f.cellInputs), f.cellInputs)
	}

	// 明细请求的楼不在参数里（楼只用于归属过滤），学期与节次模式来自同一次学期页解析。
	wantDetail := upstream.OccupancyDetailParams{
		Term: "2026-2027-1", KBJCMSID: "01",
		RoomID: "R0001", Weekday: 1, BigSlot: "0102",
	}
	if f.detailCalls[0] != wantDetail {
		t.Fatalf("明细入参期望 %+v，实际 %+v", wantDetail, f.detailCalls[0])
	}

	// 明细任务已置 done，第二段收口判定看到的是「可选任务 1/1 完成」。
	for _, task := range f.tasks {
		if task.Kind == store.SyncTaskKindCellDetail && task.Status != store.SyncTaskDone {
			t.Fatalf("明细任务应已完成，实际 %+v", task)
		}
	}
}

// TestExecuteDetailTasksNotDroppedByAttribution 覆盖 tasklist 第 8 项的第二条单测：
// 明细任务一条都没被 pipeline.Run 的归属过滤丢弃（Dropped()==0）。
//
// 构造一个只有错误实现才会失败的场景：Q10 与 Q11 都报房号 R0001，白名单顺序让 R0001
// 归 Q10；不可用格却只出现在 Q11 的矩阵页面上。第二段必须按 collect_run_room 把楼
// 重写成 Q10——用「响应所在的楼」会得到 Q11，任务会被 attributedCellInputs 丢弃。
//
// 「Dropped()==0」的等价断言：pipeline.Run 在 EnsureCellDetailTasks **之前**按归属
// 过滤，被丢弃的任务根本不会进 Ensure 入参；所以「Run2 落库的明细任务数 == Plan2
// 的明细段条数」且每条都被下钻，就是 Dropped()==0。
func TestExecuteDetailTasksNotDroppedByAttribution(t *testing.T) {
	f := newStageFake()
	f.snapshot.Whitelist = []model.Building{
		{ID: "Q10", Name: "综合楼"},
		{ID: "Q11", Name: "实验楼"},
	}
	// Q10 全空闲；Q11 的 R0001 在周一第 1 大节不可用（同一房号被跨楼重复报出）。
	f.matrixBodies = map[string][]byte{
		"Q10": []byte(matrixFixturePage(matrixDataRow("R0001", nil))),
		"Q11": []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"}))),
	}
	f.detailBody = stageDetailPage()

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("两段式执行不应返回错误，实际 %v", err)
	}

	// Run1 的归属裁决把 R0001 判给白名单靠前的 Q10（Q11 的同一房号只打冲突标记）。
	if owner := f.rooms["R0001"].BuildingID; owner != "Q10" {
		t.Fatalf("R0001 的归属楼应为 Q10，实际 %q", owner)
	}

	window := pipeline.WindowFor(9, 16)
	buildings := []string{"Q10", "Q11"}
	wantCells := []pipeline.Cell{{
		BuildingID: "Q10", RoomID: "R0001", Weekday: 1, Block: "0102",
	}}
	wantCellInputs := cellInputsOf(pipeline.Plan(window, buildings, wantCells).Tasks)
	if !slices.Equal(f.cellInputs, wantCellInputs) {
		t.Fatalf("明细任务应带归属楼 Q10（不是响应所在的 Q11），期望 %+v，实际 %+v",
			wantCellInputs, f.cellInputs)
	}
	if len(f.cellInputs) != 1 || len(f.detailCalls) != 1 {
		t.Fatalf("明细任务不应被归属过滤丢弃：Plan2 1 条、落库 %d 条、下钻 %d 次",
			len(f.cellInputs), len(f.detailCalls))
	}
	if got := f.detailCalls[0]; got.RoomID != "R0001" || got.Weekday != 1 || got.BigSlot != "0102" {
		t.Fatalf("明细请求参数不符：%+v", got)
	}
}

// TestExecuteRunsPreExistingDetailTask 是「统一抓取器」的回归用例：库里留着一条
// 上一轮/续跑写入的 pending 明细任务、内存候选为空时，本轮必须把它正常下钻，
// 而不是让第一段那个「只认周矩阵」的旧闭包把它拒成永久 failed。
//
// 为什么它会出现在第一段：pipeline.Run 每调用一次都会落库并执行库里该段已有的
// 全部同类任务（EnsureCellDetailTasks 原样读回该 kind 的任务），而第一段只规划
// 周矩阵，于是这条遗留明细在 Run1 里就被执行。旧实现下它会被 c.weekMatrixTaskFetcher
// 以「周矩阵抓取器收到 cell_detail 任务」判失败、FailSyncTask 置 failed——续跑再也
// 做不回来，req 8.1/10.4 被破坏。
//
// 断言三件事：明细请求真的发生（且发生在第一段）、该任务最终是 done、没有 fail 事件。
func TestExecuteRunsPreExistingDetailTask(t *testing.T) {
	f := newStageFake()
	// 空内存：本轮的矩阵全空闲，Plan2 不会规划任何明细任务，唯一的明细任务就是
	// 库里这条遗留的 pending。房号不进 collect_run_room 也不影响它：第一段对
	// 库里已有的明细任务不做归属过滤（归属过滤只针对本轮 Plan2 的入参）。
	f.tasks = []store.SyncTask{{
		TaskID:  1,
		Kind:    store.SyncTaskKindCellDetail,
		RoomID:  "R0001",
		Weekday: 1,
		Block:   "0102",
		Status:  store.SyncTaskPending,
	}}
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", nil)))
	f.detailBody = stageDetailPage()

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("遗留明细任务应被正常执行，执行体不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("遗留明细执行成功后本轮应进入发布，不应提前收口，实际 %+v", finishes)
	}

	if len(f.detailCalls) != 1 {
		t.Fatalf("遗留的明细任务应触发一次明细请求，实际 %d 次：%+v", len(f.detailCalls), f.detailCalls)
	}
	if got := f.detailCalls[0]; got.RoomID != "R0001" || got.Weekday != 1 || got.BigSlot != "0102" {
		t.Fatalf("明细请求参数应取任务本身，实际 %+v", got)
	}
	// 第一段就执行：早于第一次收口判定。
	if detail := nthEvent(t, f.events, "fetch_detail", 0); detail >= nthEvent(t, f.events, "summary", 0) {
		t.Fatalf("遗留明细应在第一段执行，实际事件序列 %v", f.events)
	}
	if f.tasks[0].Status != store.SyncTaskDone {
		t.Fatalf("遗留明细任务应被置 done，实际 %+v（原因 %q）",
			f.tasks[0], f.taskMessages[f.tasks[0].TaskID])
	}
	if slices.Contains(f.events, "fail:1") {
		t.Fatalf("遗留明细任务不应被拒成 failed，实际事件序列 %v", f.events)
	}
}

// TestExecuteWarnsOnOrphanCells 覆盖 Requirement 8.5 的诊断计数：不可用格在
// collect_run_room 里查不到归属楼时，不规划、不下钻，只记一条 WARN。
//
// 造法：Q10 的矩阵页同时报 R0001（全空闲）与 R0002（两格不可用），但 LoadRunRooms
// 少返回 R0002——归属在 Run1 明明写过它，第二段规划前读回来的快照却没有，于是 R0002
// 的两格都成了孤儿格。断言「恰好一条 WARN + 计数等于孤儿格数」，而不是「恰好一格」：
// 把两格算成一格、或把有归属的格也算进计数，这两种错误都必须能测出来。
//
// 孤儿格只影响诊断，不影响终态：明细任务 0 条、不提前收口，执行体也不返回错误。
func TestExecuteWarnsOnOrphanCells(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(
		matrixDataRow("R0001", nil),
		matrixDataRow("R0002", map[int]string{0: "◆", 6: "◆"}),
	))
	f.loadRoomsDropRoom = "R0002"

	var logs bytes.Buffer
	c := newStageExecutorWithLog(t, f, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("孤儿格不阻断本轮，执行体不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("孤儿格不影响终态，不应提前收口，实际 %+v", finishes)
	}

	warns := warnsOf(jsonLogRecords(t, &logs))
	if len(warns) != 1 {
		t.Fatalf("孤儿格应恰好记一条 WARN，实际 %d 条：%+v", len(warns), warns)
	}
	if warns[0].Msg != "collect_run_orphan_cells" {
		t.Fatalf("WARN 消息期望 collect_run_orphan_cells，实际 %q", warns[0].Msg)
	}
	if warns[0].RunID != executeRunID {
		t.Fatalf("WARN 的 run_id 期望 %d，实际 %d", executeRunID, warns[0].RunID)
	}
	// R0002 × 周一 0102、R0002 × 周二 030405：窗口内任一周不可用即算一格，共两格。
	if warns[0].OrphanCells != 2 {
		t.Fatalf("orphan_cells 应等于孤儿格数 2，实际 %d", warns[0].OrphanCells)
	}

	// 孤儿格不规划、不下钻：Plan2 的明细段为空。
	if len(f.cellInputs) != 0 || len(f.detailCalls) != 0 {
		t.Fatalf("孤儿格不应规划或下钻明细，实际落库 %+v、请求 %+v", f.cellInputs, f.detailCalls)
	}
}

// TestExecuteNoOrphanWarnWhenAllRoomsAttributed 是上一条的对照：不可用格都能在
// collect_run_room 里找到归属楼时（正常轮次），执行体一条 WARN 都不该记——孤儿格
// WARN 只对「查不到归属」的格生效，不能退化成每轮都报。
func TestExecuteNoOrphanWarnWhenAllRoomsAttributed(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = stageDetailPage()

	var logs bytes.Buffer
	c := newStageExecutorWithLog(t, f, slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("正常轮次不应返回错误，实际 %v", err)
	}
	if warns := warnsOf(jsonLogRecords(t, &logs)); len(warns) != 0 {
		t.Fatalf("无孤儿格时不应记 WARN，实际 %d 条：%+v", len(warns), warns)
	}
	// 对照：这一轮确实有一格不可用且被正常下钻，说明「零 WARN」不是因为本轮没有候选。
	if len(f.cellInputs) != 1 || len(f.detailCalls) != 1 {
		t.Fatalf("应有 1 条明细任务并下钻 1 次，实际落库 %+v、请求 %+v", f.cellInputs, f.detailCalls)
	}
}

// TestExecuteDetailStructureFailureYieldsPartial 覆盖 tasklist 第 8 项的第三条单测：
// 明细结构失败（parser.ErrDetailStructureInvalid）只让可选任务失败，轮次的判定结果
// 是 partial，不是 failed。
//
// 本项还没有发布（第 10 项），所以 partial 不会被 CloseRun 收口，轮次保持 running：
// 用例在 execute 返回后用同一条库状态再跑一次 EvaluateRun，断言它给出的正是 partial。
// 这同时钉住「明细失败不升级成 failed」与「partial 不提前收口」两条口径。
func TestExecuteDetailStructureFailureYieldsPartial(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = []byte(`<html><body>这不是明细页</body></html>`)

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("明细结构失败属于任务失败，执行体不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("partial 应由发布事务收口，本项不应提前收口，实际 %+v", finishes)
	}
	if f.summaryCalls != 2 {
		t.Fatalf("两段各应做一次收口判定，实际 %d 次", f.summaryCalls)
	}

	// 结构失败码在 failTask 时写进轮次，任务置 failed。
	if f.errorCode != "detail_structure_invalid" {
		t.Fatalf("轮次错误码期望 detail_structure_invalid，实际 %q", f.errorCode)
	}
	var detailTask *store.SyncTask
	for index := range f.tasks {
		if f.tasks[index].Kind == store.SyncTaskKindCellDetail {
			detailTask = &f.tasks[index]
		}
	}
	if detailTask == nil {
		t.Fatal("应落一条明细任务")
	}
	if detailTask.Status != store.SyncTaskFailed {
		t.Fatalf("明细结构失败应把任务置 failed，实际 %+v", *detailTask)
	}

	status, summary, err := pipeline.EvaluateRun(context.Background(), f, executeRunID, pipeline.DroppedTasks{})
	if err != nil {
		t.Fatalf("EvaluateRun 不应报错，实际 %v", err)
	}
	if status != pipeline.StatusPartial {
		t.Fatalf("明细结构失败应判定 partial，实际 %q（汇总 %+v）", status, summary)
	}
	if summary.RecordedErrorCode != "detail_structure_invalid" {
		t.Fatalf("汇总里的错误码期望 detail_structure_invalid，实际 %q", summary.RecordedErrorCode)
	}
	if summary.RequiredFailed != 0 || summary.OptionalFailed != 1 {
		t.Fatalf("失败应只落在可选任务上，实际必需失败 %d、可选失败 %d",
			summary.RequiredFailed, summary.OptionalFailed)
	}
}

// TestExecuteSkipsDetailsWhenMatrixStageFails 覆盖 Requirement 10.7：第一段跑完若
// 必需周矩阵已经失败，立即 CloseRun 收口 failed 并跳过全部占用说明任务。
//
// 断言三件事：终态 failed + 结构失败码；明细请求 0 次（不再浪费时间）；收口只做一次
// （CloseRun 内部会再算一次 EvaluateRun，所以 summary 是 2 次而不是无限重试）。
func TestExecuteSkipsDetailsWhenMatrixStageFails(t *testing.T) {
	f := newStageFake()
	f.matrixErr = fmt.Errorf("%w: 矩阵页没有目标表", parser.ErrMatrixStructureInvalid)

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("首段失败已收口，执行体不应返回错误，实际 %v", err)
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, "matrix_structure_invalid")
	if len(f.matrixCalls) != 4 {
		t.Fatalf("窗口 9..12 的 4 条必需任务都应被尝试，实际 %d 次矩阵请求", len(f.matrixCalls))
	}
	if len(f.detailCalls) != 0 {
		t.Fatalf("必需矩阵已失败时不应下钻明细，实际 %d 次：%+v", len(f.detailCalls), f.detailCalls)
	}
	if len(f.cellInputs) != 0 {
		t.Fatalf("必需矩阵已失败时不应规划明细任务，实际 %+v", f.cellInputs)
	}
	if f.summaryCalls != 2 {
		t.Fatalf("首段判定一次 + CloseRun 内部一次，期望 2 次汇总读取，实际 %d 次", f.summaryCalls)
	}

	// 首段失败的轮次不会进入第二段：没有 Run2 的 ensure_week。
	ensureWeeks := 0
	for _, event := range f.events {
		if event == "ensure_week" {
			ensureWeeks++
		}
	}
	if ensureWeeks != 1 {
		t.Fatalf("首段失败后不应再跑第二段，ensure_week 期望 1 次，实际 %d 次：%v",
			ensureWeeks, f.events)
	}
}

// TestExecuteFinishesBlockedWhenWhitelistEmpty 覆盖 Plan 的防御路径（design Error
// Handling 表的 whitelist_empty 行）：runner 判过白名单非空之后设置被清空，执行器
// 不再落任务、不发请求，直接以 blocked 收口。
func TestExecuteFinishesBlockedWhenWhitelistEmpty(t *testing.T) {
	f := newStageFake()
	f.snapshot.Whitelist = nil

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("空白名单已收口，执行体不应返回错误，实际 %v", err)
	}

	finish := singleFinish(t, f.execFake)
	if finish.Status != string(pipeline.StatusBlocked) {
		t.Fatalf("空白名单应收口 blocked，实际 %q", finish.Status)
	}
	if finish.ErrorCode != codeWhitelistEmpty {
		t.Fatalf("错误码期望 %q，实际 %q", codeWhitelistEmpty, finish.ErrorCode)
	}
	if finish.ErrorMessage != whitelistEmptyReason {
		t.Fatalf("原因期望 %q，实际 %q", whitelistEmptyReason, finish.ErrorMessage)
	}
	if len(f.matrixCalls) != 0 || len(f.detailCalls) != 0 {
		t.Fatalf("空白名单不应发任何上游请求，实际矩阵 %d 次、明细 %d 次",
			len(f.matrixCalls), len(f.detailCalls))
	}
	if slices.Contains(f.events, "ensure_week") || slices.Contains(f.events, "ensure_cell") {
		t.Fatalf("空白名单不应落任务，实际序列 %v", f.events)
	}
}

// TestRunStagesFailsOnInvalidWindowSpan 覆盖 Plan 的第二条 blocked 来源：窗口跨度
// 失控（调用方传了没走 WindowFor 的窗口）。白名单非空时不能借用白名单那条 blocked
// 收口——它会把「窗口非法」显示成「白名单为空，未发起采集」，管理端看到的码与原因
// 都是错的。这里按非结构失败收口：failed + 空错误码 + 一条点明窗口的原因。
func TestRunStagesFailsOnInvalidWindowSpan(t *testing.T) {
	f := newStageFake()
	c := newStageExecutor(t, f)

	// runStages 是执行体的第二层，这里直接喂一个跨度失控的窗口；execute 正常路径
	// 的窗口只来自 WindowFor，永远走不到这条分支。
	err := c.runStages(context.Background(), executeRunID, roundSetup{
		window:   pipeline.Window{Start: 1, End: 100},
		settings: roundSettings{Buildings: []model.Building{{ID: "Q10", Name: "综合楼"}}},
	})
	if err == nil {
		t.Fatal("窗口跨度非法时应返回错误供 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "窗口") {
		t.Fatalf("原因应点明窗口非法，实际 %q", finish.ErrorMessage)
	}
	if strings.Contains(finish.ErrorMessage, whitelistEmptyReason) {
		t.Fatalf("窗口非法不应借用白名单为空的文案，实际 %q", finish.ErrorMessage)
	}
	if len(f.matrixCalls) != 0 || len(f.detailCalls) != 0 {
		t.Fatalf("非法窗口不应发任何上游请求，实际矩阵 %d 次、明细 %d 次",
			len(f.matrixCalls), len(f.detailCalls))
	}
	if slices.Contains(f.events, "ensure_week") || slices.Contains(f.events, "ensure_cell") {
		t.Fatalf("非法窗口不应落任务，实际序列 %v", f.events)
	}
}

// TestRunStagesReportsLoadRunRoomsError 覆盖第二段规划前读归属快照失败：
// 这是基础设施失败，收口 failed、不写业务码，且不进入 Run2（没有归属就无法重写楼）。
//
// 错误只打挂第 2 次 LoadRunRooms：第 1 次是 Run1 内部 seedAttribution 的重建，
// 它必须成功，第一段才真的跑完 4 条周矩阵。让 fake 一直报错会让用例在 Run1 就
// 失败，看起来是绿的，实际没验证到「第二段规划前」这条路径。
func TestRunStagesReportsLoadRunRoomsError(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.loadRoomsErr = errors.New("读取每轮归属失败")
	f.loadRoomsErrAt = 2

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("读归属失败时执行体应返回错误供 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "读取每轮归属失败") {
		t.Fatalf("原因应取仓储文案，实际 %q", finish.ErrorMessage)
	}
	if f.loadRoomsCalls != 2 {
		t.Fatalf("失败应发生在第二段规划前那次读取（第 2 次），实际调用 %d 次", f.loadRoomsCalls)
	}
	if len(f.matrixCalls) != 4 {
		t.Fatalf("第一段应先完整跑完 4 条周矩阵，再在第二段规划前失败，实际 %d 次矩阵请求",
			len(f.matrixCalls))
	}
	if len(f.detailCalls) != 0 {
		t.Fatalf("没有归属快照就不该下钻明细，实际 %d 次", len(f.detailCalls))
	}
	// 只跑过 Run1，没有进入 Run2。
	ensureWeeks := 0
	for _, event := range f.events {
		if event == "ensure_week" {
			ensureWeeks++
		}
	}
	if ensureWeeks != 1 {
		t.Fatalf("读归属失败后不应进入 Run2，ensure_week 期望 1 次，实际 %d 次", ensureWeeks)
	}
}
