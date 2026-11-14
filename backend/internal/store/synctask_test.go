package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// syncTaskQuerierStub 记录 SQL 与参数，并按预置返回任务行。
// execTags 按调用顺序回放受影响行数；缺省返回「影响 1 行」，让正常路径不必逐条预置。
type syncTaskQuerierStub struct {
	execCalls int
	execTags  []pgconn.CommandTag
	execSQLs  []string
	execArgs  [][]any
	execErr   error

	queryCalls int
	querySQL   string
	queryArgs  []any
	queryRows  pgx.Rows
	queryErr   error
}

// Exec 实现 syncTaskQuerier。
func (s *syncTaskQuerierStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execCalls++
	s.execSQLs = append(s.execSQLs, sql)
	s.execArgs = append(s.execArgs, args)
	if s.execErr != nil {
		return pgconn.CommandTag{}, s.execErr
	}
	if s.execCalls <= len(s.execTags) {
		return s.execTags[s.execCalls-1], nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// Query 实现 syncTaskQuerier。
func (s *syncTaskQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.queryCalls++
	s.querySQL = sql
	s.queryArgs = args
	return s.queryRows, s.queryErr
}

// syncTaskRowsStub 是 pgx.Rows 的最小替身，列顺序与 selectSyncTasksSQL 一致。
type syncTaskRowsStub struct {
	tasks   []SyncTask
	index   int
	current SyncTask
	scanErr error
	iterErr error
	closed  bool
}

// Close 实现 pgx.Rows。
func (s *syncTaskRowsStub) Close() { s.closed = true }

// Err 实现 pgx.Rows。
func (s *syncTaskRowsStub) Err() error { return s.iterErr }

// CommandTag 实现 pgx.Rows。
func (s *syncTaskRowsStub) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

// FieldDescriptions 实现 pgx.Rows。
func (s *syncTaskRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }

// Next 实现 pgx.Rows。
func (s *syncTaskRowsStub) Next() bool {
	if s.index >= len(s.tasks) {
		return false
	}
	s.current = s.tasks[s.index]
	s.index++
	return true
}

// Scan 实现 pgx.Rows：NULL 的周次与占用说明字段要写成 nil 指针。
func (s *syncTaskRowsStub) Scan(dest ...any) error {
	if s.scanErr != nil {
		return s.scanErr
	}
	if len(dest) != 9 {
		return fmt.Errorf("期望 9 个扫描目标，实际 %d 个", len(dest))
	}
	taskID, ok := dest[0].(*int64)
	if !ok {
		return fmt.Errorf("第 1 个目标期望 *int64，实际 %T", dest[0])
	}
	kind, ok := dest[1].(*string)
	if !ok {
		return fmt.Errorf("第 2 个目标期望 *string，实际 %T", dest[1])
	}
	required, ok := dest[2].(*bool)
	if !ok {
		return fmt.Errorf("第 3 个目标期望 *bool，实际 %T", dest[2])
	}
	buildingID, ok := dest[3].(*string)
	if !ok {
		return fmt.Errorf("第 4 个目标期望 *string，实际 %T", dest[3])
	}
	week, ok := dest[4].(**int16)
	if !ok {
		return fmt.Errorf("第 5 个目标期望 **int16，实际 %T", dest[4])
	}
	roomID, ok := dest[5].(**string)
	if !ok {
		return fmt.Errorf("第 6 个目标期望 **string，实际 %T", dest[5])
	}
	weekday, ok := dest[6].(**int16)
	if !ok {
		return fmt.Errorf("第 7 个目标期望 **int16，实际 %T", dest[6])
	}
	block, ok := dest[7].(**string)
	if !ok {
		return fmt.Errorf("第 8 个目标期望 **string，实际 %T", dest[7])
	}
	status, ok := dest[8].(*string)
	if !ok {
		return fmt.Errorf("第 9 个目标期望 *string，实际 %T", dest[8])
	}

	*taskID = s.current.TaskID
	*kind = s.current.Kind
	*required = s.current.Required
	*buildingID = s.current.BuildingID
	*week = nil
	if s.current.Week != 0 {
		value := int16(s.current.Week)
		*week = &value
	}
	*roomID = nil
	if s.current.RoomID != "" {
		value := s.current.RoomID
		*roomID = &value
	}
	*weekday = nil
	if s.current.Weekday != 0 {
		value := int16(s.current.Weekday)
		*weekday = &value
	}
	*block = nil
	if s.current.Block != "" {
		value := s.current.Block
		*block = &value
	}
	*status = s.current.Status
	return nil
}

// Values 实现 pgx.Rows。
func (s *syncTaskRowsStub) Values() ([]any, error) { return nil, nil }

// RawValues 实现 pgx.Rows。
func (s *syncTaskRowsStub) RawValues() [][]byte { return nil }

// Conn 实现 pgx.Rows。
func (s *syncTaskRowsStub) Conn() *pgx.Conn { return nil }

// TypeMap 实现 pgx.Rows。
func (s *syncTaskRowsStub) TypeMap() *pgtype.Map { return nil }

// TestEnsureWeekMatrixTasksInsertsAndReloads 覆盖周矩阵段落库映射与读取折零：
// 规划任务写成并行数组并以 kind 参数限定本段，读回时 NULL 的占用说明字段折成零值。
func TestEnsureWeekMatrixTasksInsertsAndReloads(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	rows := &syncTaskRowsStub{tasks: []SyncTask{
		{TaskID: 1, Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20, Status: SyncTaskDone},
		{TaskID: 2, Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 21, Status: SyncTaskPending},
	}}
	querier := &syncTaskQuerierStub{queryRows: rows}
	inputs := []SyncTaskInput{
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 21},
	}

	tasks, err := EnsureWeekMatrixTasks(context.Background(), querier, 7, inputs, now)
	if err != nil {
		t.Fatalf("落任务不应报错，实际 %v", err)
	}
	if len(tasks) != 2 || tasks[0].Status != SyncTaskDone || tasks[1].Week != 21 {
		t.Fatalf("读回任务不符: %+v", tasks)
	}
	if tasks[0].RoomID != "" || tasks[0].Weekday != 0 || tasks[0].Block != "" {
		t.Fatalf("周矩阵任务的占用说明字段应折成零值: %+v", tasks[0])
	}
	if querier.execCalls != 2 {
		t.Fatalf("应依次执行插入与重算计数两条 SQL，实际 %d 条", querier.execCalls)
	}
	if querier.execSQLs[0] != insertSyncTasksByKindSQL || querier.execSQLs[1] != recountSyncTasksSQL {
		t.Fatalf("SQL 顺序不符: %v", querier.execSQLs)
	}
	args := querier.execArgs[0]
	if len(args) != 9 {
		t.Fatalf("插入参数应有 9 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 {
		t.Fatalf("run_id 期望 7，实际 %v", args[0])
	}
	if got := args[1].([]string); len(got) != 2 || got[0] != SyncTaskKindWeekMatrix || got[1] != SyncTaskKindWeekMatrix {
		t.Fatalf("kind 数组不符: %v", got)
	}
	if got := args[2].([]bool); len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("required 数组不符: %v", got)
	}
	if got := args[4].([]int16); len(got) != 2 || got[0] != 20 || got[1] != 21 {
		t.Fatalf("week 数组不符: %v", got)
	}
	if got := args[5].([]string); len(got) != 2 || got[0] != "" || got[1] != "" {
		t.Fatalf("room_id 数组不符: %v", got)
	}
	if args[8] != SyncTaskKindWeekMatrix {
		t.Fatalf("段种类参数期望 %q，实际 %v", SyncTaskKindWeekMatrix, args[8])
	}
	recountArgs := querier.execArgs[1]
	if len(recountArgs) != 2 || recountArgs[0].(int64) != 7 || !recountArgs[1].(time.Time).Equal(now) {
		t.Fatalf("重算计数参数不符: %v", recountArgs)
	}
	if querier.queryCalls != 1 || querier.queryArgs[0].(int64) != 7 || querier.queryArgs[1] != SyncTaskKindWeekMatrix {
		t.Fatalf("读任务参数不符: calls=%d args=%v", querier.queryCalls, querier.queryArgs)
	}
	if !rows.closed {
		t.Fatal("任务行应被关闭")
	}
}

// TestEnsureCellDetailTasksInsertsAndReloads 覆盖占用说明段落库与读取折零：
// room_id 等字段原样传库，读回时 NULL 的周次折成零值，kind 参数限定本段。
func TestEnsureCellDetailTasksInsertsAndReloads(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	rows := &syncTaskRowsStub{tasks: []SyncTask{
		{TaskID: 1, Kind: SyncTaskKindCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304", Status: SyncTaskPending},
	}}
	querier := &syncTaskQuerierStub{queryRows: rows}
	inputs := []SyncTaskInput{
		{Kind: SyncTaskKindCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 3, Block: "0304"},
	}

	tasks, err := EnsureCellDetailTasks(context.Background(), querier, 7, inputs, now)
	if err != nil {
		t.Fatalf("落任务不应报错，实际 %v", err)
	}
	if len(tasks) != 1 || tasks[0].RoomID != "r101" || tasks[0].Weekday != 3 {
		t.Fatalf("读回任务不符: %+v", tasks)
	}
	if tasks[0].Week != 0 {
		t.Fatalf("占用说明任务的周次应折成零值: %+v", tasks[0])
	}
	if querier.execCalls != 2 || querier.execSQLs[0] != insertSyncTasksByKindSQL {
		t.Fatalf("SQL 顺序不符: %v", querier.execSQLs)
	}
	args := querier.execArgs[0]
	if len(args) != 9 || args[8] != SyncTaskKindCellDetail {
		t.Fatalf("段种类参数不符: %v", args)
	}
	if got := args[5].([]string); len(got) != 1 || got[0] != "r101" {
		t.Fatalf("room_id 数组不符: %v", got)
	}
	if got := args[6].([]int16); len(got) != 1 || got[0] != 3 {
		t.Fatalf("weekday 数组不符: %v", got)
	}
	if querier.queryCalls != 1 || querier.queryArgs[1] != SyncTaskKindCellDetail {
		t.Fatalf("读任务参数不符: calls=%d args=%v", querier.queryCalls, querier.queryArgs)
	}
}

// TestLoadWeekMatrixTasksReadsWeekKind 覆盖 hydrate 的读入口：只按 kind 查 week_matrix、
// 按 task_id 升序返回含状态的任务，且只读——不执行任何写入 SQL。
func TestLoadWeekMatrixTasksReadsWeekKind(t *testing.T) {
	rows := &syncTaskRowsStub{tasks: []SyncTask{
		{TaskID: 1, Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20, Status: SyncTaskDone},
		{TaskID: 2, Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 21, Status: SyncTaskPending},
	}}
	querier := &syncTaskQuerierStub{queryRows: rows}

	tasks, err := NewSyncTaskRepo(querier).LoadWeekMatrixTasks(context.Background(), 7)
	if err != nil {
		t.Fatalf("读周矩阵任务不应报错，实际 %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("应读回 2 条任务，实际 %d 条：%+v", len(tasks), tasks)
	}
	if tasks[0].Status != SyncTaskDone || tasks[1].Week != 21 || tasks[1].Status != SyncTaskPending {
		t.Fatalf("任务状态与周次应按库里的值原样返回: %+v", tasks)
	}
	if querier.execCalls != 0 {
		t.Fatalf("读入口不应执行任何写入 SQL，实际 %d 条", querier.execCalls)
	}
	if querier.queryCalls != 1 || querier.querySQL != selectSyncTasksByKindSQL {
		t.Fatalf("应只执行按种类读任务的 SQL，实际 calls=%d sql=%q", querier.queryCalls, querier.querySQL)
	}
	if got := querier.queryArgs; len(got) != 2 || got[0].(int64) != 7 || got[1] != SyncTaskKindWeekMatrix {
		t.Fatalf("读任务参数不符: %v", got)
	}
	if !rows.closed {
		t.Fatal("任务行应被关闭")
	}
}

// TestEnsureSplitSyncTasksRejectsMixedKind 覆盖两段 Ensure 的段边界：
// 把占用说明任务交给周矩阵段（或反过来）直接报错，不执行任何 SQL。
func TestEnsureSplitSyncTasksRejectsMixedKind(t *testing.T) {
	cases := []struct {
		name  string
		kind  string
		input SyncTaskInput
	}{
		{name: "周矩阵段拒绝占用说明", kind: SyncTaskKindWeekMatrix,
			input: SyncTaskInput{Kind: SyncTaskKindCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}},
		{name: "占用说明段拒绝周矩阵", kind: SyncTaskKindCellDetail,
			input: SyncTaskInput{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			querier := &syncTaskQuerierStub{}
			var err error
			if c.kind == SyncTaskKindWeekMatrix {
				_, err = EnsureWeekMatrixTasks(context.Background(), querier, 7, []SyncTaskInput{c.input}, time.Now())
			} else {
				_, err = EnsureCellDetailTasks(context.Background(), querier, 7, []SyncTaskInput{c.input}, time.Now())
			}
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("同段混入别的种类应报 ErrRunStoreFailed，实际 %v", err)
			}
			if querier.execCalls != 0 || querier.queryCalls != 0 {
				t.Fatalf("段边界校验失败时不应触库: exec=%d query=%d", querier.execCalls, querier.queryCalls)
			}
		})
	}
}

// TestRecordWeekFetchPassesWeekFact 覆盖周结果写入的参数：term 由 SQL 从轮次取，
// Go 侧只传 run_id、周、楼、抓取时刻与页面哈希。
func TestRecordWeekFetchPassesWeekFact(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &syncTaskQuerierStub{}

	err := RecordWeekFetch(context.Background(), querier, RunWeekFetch{
		RunID: 7, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "abc123",
	})
	if err != nil {
		t.Fatalf("写周结果不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQLs[0] != recordWeekFetchSQL {
		t.Fatalf("应只执行周结果 SQL，实际 %v", querier.execSQLs)
	}
	args := querier.execArgs[0]
	if len(args) != 5 {
		t.Fatalf("周结果参数应有 5 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 || args[1].(int) != 20 || args[2].(string) != "Q10" ||
		!args[3].(time.Time).Equal(now) || args[4].(string) != "abc123" {
		t.Fatalf("周结果参数不符: %v", args)
	}
}

// TestCompleteSyncTaskPassesIdentityAndTime 覆盖置 done 的身份与时刻。
func TestCompleteSyncTaskPassesIdentityAndTime(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &syncTaskQuerierStub{}

	if err := CompleteSyncTask(context.Background(), querier, 7, 3, now); err != nil {
		t.Fatalf("置 done 不应报错，实际 %v", err)
	}
	if querier.execSQLs[0] != completeSyncTaskSQL {
		t.Fatalf("SQL 不符: %s", querier.execSQLs[0])
	}
	args := querier.execArgs[0]
	if len(args) != 3 || args[0].(int64) != 7 || args[1].(int64) != 3 || !args[2].(time.Time).Equal(now) {
		t.Fatalf("置 done 参数不符: %v", args)
	}
}

// TestFailSyncTaskPassesReasonAndTime 覆盖置 failed 的原因与时刻。
func TestFailSyncTaskPassesReasonAndTime(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &syncTaskQuerierStub{}

	if err := FailSyncTask(context.Background(), querier, 7, 3, "结构失败", now); err != nil {
		t.Fatalf("置 failed 不应报错，实际 %v", err)
	}
	if querier.execSQLs[0] != failSyncTaskSQL {
		t.Fatalf("SQL 不符: %s", querier.execSQLs[0])
	}
	args := querier.execArgs[0]
	if len(args) != 4 || args[0].(int64) != 7 || args[1].(int64) != 3 ||
		args[2].(string) != "结构失败" || !args[3].(time.Time).Equal(now) {
		t.Fatalf("置 failed 参数不符: %v", args)
	}
}

// TestRecordWeekFailurePassesWeekFact 覆盖失败周结果的参数：Go 侧只传轮次、周、
// 楼与原因；term 仍由 SQL 从 collect_run 取，ok 固定 false。
func TestRecordWeekFailurePassesWeekFact(t *testing.T) {
	querier := &syncTaskQuerierStub{}

	err := RecordWeekFailure(context.Background(), querier, RunWeekFailure{
		RunID: 7, Week: 20, BuildingID: "Q10", ErrorMessage: "矩阵结构失败",
	})
	if err != nil {
		t.Fatalf("写失败周结果不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQLs[0] != recordWeekFailureSQL {
		t.Fatalf("应只执行失败周结果 SQL，实际 %v", querier.execSQLs)
	}
	args := querier.execArgs[0]
	if len(args) != 4 {
		t.Fatalf("失败周结果参数应有 4 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 || args[1].(int) != 20 || args[2].(string) != "Q10" || args[3].(string) != "矩阵结构失败" {
		t.Fatalf("失败周结果参数不符: %v", args)
	}
}

// TestRecordWeekFetchRejectsMissingRunOrTerm 覆盖 0 行写入：轮次不存在或缺学期时
// SQL 不插入任何行，函数必须返回可读错误而不是静默成功。
func TestRecordWeekFetchRejectsMissingRunOrTerm(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 0")}}

	err := RecordWeekFetch(context.Background(), querier, RunWeekFetch{
		RunID: 404, Week: 20, BuildingID: "Q10", FetchedAt: now, PageSHA256: "abc",
	})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行写入应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在或未写学期") {
		t.Fatalf("错误应说明轮次不存在或缺学期，实际 %v", err)
	}
}

// TestRecordWeekFailureRejectsMissingRunOrTerm 覆盖失败周结果的 0 行写入。
func TestRecordWeekFailureRejectsMissingRunOrTerm(t *testing.T) {
	querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 0")}}

	err := RecordWeekFailure(context.Background(), querier, RunWeekFailure{
		RunID: 404, Week: 20, BuildingID: "Q10", ErrorMessage: "矩阵结构失败",
	})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行写入应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在或未写学期") {
		t.Fatalf("错误应说明轮次不存在或缺学期，实际 %v", err)
	}
}

// TestCompleteSyncTaskRejectsMissingRunOrTask 覆盖 0 行更新：轮次或任务不存在时
// 外层 UPDATE 不命中，函数必须报错，不能把落空的写入当成已完成。
func TestCompleteSyncTaskRejectsMissingRunOrTask(t *testing.T) {
	querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}}

	err := CompleteSyncTask(context.Background(), querier, 404, 99, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行更新应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次或任务不存在，实际 %v", err)
	}
}

// TestFailSyncTaskRejectsMissingRunOrTask 覆盖 0 行更新：轮次或任务不存在时必须报错。
func TestFailSyncTaskRejectsMissingRunOrTask(t *testing.T) {
	querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}}

	err := FailSyncTask(context.Background(), querier, 404, 99, "结构失败", time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行更新应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次或任务不存在，实际 %v", err)
	}
}

// TestRecountSyncTasksPassesRunAndTime 覆盖单独重算计数：一条 UPDATE，参数是轮次与时刻。
func TestRecountSyncTasksPassesRunAndTime(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &syncTaskQuerierStub{}

	if err := RecountSyncTasks(context.Background(), querier, 7, now); err != nil {
		t.Fatalf("重算计数不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQLs[0] != recountSyncTasksSQL {
		t.Fatalf("应只执行重算计数 SQL，实际 %v", querier.execSQLs)
	}
	args := querier.execArgs[0]
	if len(args) != 2 || args[0].(int64) != 7 || !args[1].(time.Time).Equal(now) {
		t.Fatalf("重算计数参数不符: %v", args)
	}
}

// TestRecountSyncTasksRejectsMissingRun 覆盖 0 行更新：轮次不存在时重算落空，
// 必须报错而不是当成「计数已重算」。
func TestRecountSyncTasksRejectsMissingRun(t *testing.T) {
	querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}}

	err := RecountSyncTasks(context.Background(), querier, 404, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行更新应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestEnsureSplitSyncTasksRejectsMissingRunOnRecount 覆盖空任务清单加轮次不存在：
// 两段 Ensure 都没有插入语句可发，唯一的重算语句影响 0 行，必须报错；也不该去读任务。
func TestEnsureSplitSyncTasksRejectsMissingRunOnRecount(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	cases := []struct {
		name   string
		ensure func(context.Context, syncTaskQuerier, int64, []SyncTaskInput, time.Time) ([]SyncTask, error)
	}{
		{name: "周矩阵段", ensure: EnsureWeekMatrixTasks},
		{name: "占用说明段", ensure: EnsureCellDetailTasks},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			querier := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}}
			_, err := c.ensure(context.Background(), querier, 404, nil, now)
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("轮次不存在时应返回 ErrRunStoreFailed，实际 %v", err)
			}
			if querier.execCalls != 1 || querier.execSQLs[0] != recountSyncTasksSQL {
				t.Fatalf("空任务清单只应执行重算计数 SQL，实际 %v", querier.execSQLs)
			}
			if querier.queryCalls != 0 {
				t.Fatalf("重算失败后不应继续读任务，实际查询 %d 次", querier.queryCalls)
			}
		})
	}
}

// TestEnsureWeekMatrixTasksWrapsStoreFailure 覆盖基础设施错误统一映射成 ErrRunStoreFailed。
func TestEnsureWeekMatrixTasksWrapsStoreFailure(t *testing.T) {
	querier := &syncTaskQuerierStub{execErr: errors.New("连接断开")}

	_, err := EnsureWeekMatrixTasks(context.Background(), querier, 7,
		[]SyncTaskInput{{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 20}},
		time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60)))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包装成 ErrRunStoreFailed，实际 %v", err)
	}
}
