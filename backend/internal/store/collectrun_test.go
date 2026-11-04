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

// staleThreshold 是测试用的过期阈值。store 只接收 cutoff，2 小时策略在 pipeline，
// 这里放一份同值常量，避免 store 反向依赖 pipeline。
const staleThreshold = 2 * time.Hour

// runRowFunc 是 pgx.Row 的函数式替身：测试只关心 Scan 的返回与写入值。
type runRowFunc func(dest ...any) error

// Scan 实现 pgx.Row。
func (f runRowFunc) Scan(dest ...any) error {
	return f(dest...)
}

// errRunRow 返回固定错误，用来模拟插入失败与查不到行。
func errRunRow(err error) pgx.Row {
	return runRowFunc(func(...any) error {
		return err
	})
}

// runningRunRow 回一条 running 轮次：列顺序与 SELECT/RETURNING 一致。
func runningRunRow(t *testing.T, runID int64, startedAt time.Time) pgx.Row {
	t.Helper()
	return runRowFunc(func(dest ...any) error {
		if len(dest) != 3 {
			return fmt.Errorf("期望 3 个扫描目标，实际 %d 个", len(dest))
		}
		id, ok := dest[0].(*int64)
		if !ok {
			return fmt.Errorf("第 1 个目标期望 *int64，实际 %T", dest[0])
		}
		at, ok := dest[1].(*time.Time)
		if !ok {
			return fmt.Errorf("第 2 个目标期望 *time.Time，实际 %T", dest[1])
		}
		status, ok := dest[2].(*string)
		if !ok {
			return fmt.Errorf("第 3 个目标期望 *string，实际 %T", dest[2])
		}
		*id = runID
		*at = startedAt
		*status = "running"
		return nil
	})
}

// runQuerierStub 按调用顺序回放 QueryRow 的结果，并记录 Query 的参数。
type runQuerierStub struct {
	rowResults []pgx.Row
	queryRows  pgx.Rows
	queryErr   error

	rowCalls   int
	rowSQL     []string
	rowArgs    [][]any
	queryCalls int
	querySQL   string
	queryArgs  []any
}

// QueryRow 实现 collectRunQuerier：每次取一条预置结果。
func (s *runQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.rowCalls++
	s.rowSQL = append(s.rowSQL, sql)
	s.rowArgs = append(s.rowArgs, args)
	if len(s.rowResults) == 0 {
		return errRunRow(errors.New("替身没有更多 QueryRow 结果"))
	}
	row := s.rowResults[0]
	s.rowResults = s.rowResults[1:]
	return row
}

// Query 实现 collectRunQuerier：过期收敛只有一条 UPDATE ... RETURNING。
func (s *runQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.queryCalls++
	s.querySQL = sql
	s.queryArgs = args
	return s.queryRows, s.queryErr
}

// staleRunRowsStub 是 pgx.Rows 的最小替身，只实现调用方真的会用到的部分。
type staleRunRowsStub struct {
	runIDs  []int64
	index   int
	current int64

	scanErr error
	iterErr error
	closed  bool
}

// Close 实现 pgx.Rows。
func (s *staleRunRowsStub) Close() {
	s.closed = true
}

// Err 实现 pgx.Rows。
func (s *staleRunRowsStub) Err() error {
	return s.iterErr
}

// CommandTag 实现 pgx.Rows；测试不关心命令标签。
func (s *staleRunRowsStub) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

// FieldDescriptions 实现 pgx.Rows；测试不关心列描述。
func (s *staleRunRowsStub) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

// Next 实现 pgx.Rows：逐条推进预置的轮次编号。
func (s *staleRunRowsStub) Next() bool {
	if s.index >= len(s.runIDs) {
		return false
	}
	s.current = s.runIDs[s.index]
	s.index++
	return true
}

// Scan 实现 pgx.Rows：RETURNING 只有 run_id 一列。
func (s *staleRunRowsStub) Scan(dest ...any) error {
	if s.scanErr != nil {
		return s.scanErr
	}
	if len(dest) != 1 {
		return fmt.Errorf("期望 1 个扫描目标，实际 %d 个", len(dest))
	}
	id, ok := dest[0].(*int64)
	if !ok {
		return fmt.Errorf("期望 *int64，实际 %T", dest[0])
	}
	*id = s.current
	return nil
}

// Values 实现 pgx.Rows。
func (s *staleRunRowsStub) Values() ([]any, error) {
	return []any{s.current}, nil
}

// RawValues 实现 pgx.Rows。
func (s *staleRunRowsStub) RawValues() [][]byte {
	return nil
}

// Conn 实现 pgx.Rows；替身没有底层连接。
func (s *staleRunRowsStub) Conn() *pgx.Conn {
	return nil
}

// TypeMap 实现 pgx.Rows；替身没有类型映射。
func (s *staleRunRowsStub) TypeMap() *pgtype.Map {
	return nil
}

// TestStartCollectRunCreatesRunningRound 覆盖首个轮次：插入成功，返回新编号与开始时刻。
func TestStartCollectRunCreatesRunningRound(t *testing.T) {
	startedAt := time.Date(2026, 11, 4, 11, 37, 31, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	querier := &runQuerierStub{rowResults: []pgx.Row{runningRunRow(t, 7, startedAt)}}

	run, reused, err := StartCollectRun(context.Background(), querier, startedAt)
	if err != nil {
		t.Fatalf("创建轮次不应报错，实际 %v", err)
	}
	if reused {
		t.Fatal("没有已有一轮时不应标记为复用")
	}
	if run.RunID != 7 || !run.StartedAt.Equal(startedAt) || run.Status != "running" {
		t.Fatalf("轮次不符: %+v", run)
	}
	if querier.rowCalls != 1 {
		t.Fatalf("创建成功只应查一次，实际 %d 次", querier.rowCalls)
	}
	if got := querier.rowArgs[0][0].(time.Time); !got.Equal(startedAt) {
		t.Fatalf("插入 started_at 期望 %s，实际 %s", startedAt, got)
	}
}

// TestStartCollectRunReusesRunningRoundOnUniqueViolation 覆盖 CP6 的冲突路径：
// 撞上 collect_run_single_running 时返回已有轮次，不报错也不重试。
func TestStartCollectRunReusesRunningRoundOnUniqueViolation(t *testing.T) {
	existingStartedAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	querier := &runQuerierStub{rowResults: []pgx.Row{
		errRunRow(&pgconn.PgError{Code: "23505", ConstraintName: collectRunSingleRunningIndex}),
		runningRunRow(t, 42, existingStartedAt),
	}}

	run, reused, err := StartCollectRun(context.Background(), querier, time.Now())
	if err != nil {
		t.Fatalf("冲突应返回已有轮次而不是报错，实际 %v", err)
	}
	if !reused {
		t.Fatal("撞上唯一索引时应标记为复用")
	}
	if run.RunID != 42 || !run.StartedAt.Equal(existingStartedAt) || run.Status != "running" {
		t.Fatalf("应原样返回已有轮次，实际 %+v", run)
	}
	if querier.rowCalls != 2 {
		t.Fatalf("冲突后只应补查一次已有轮次，实际 %d 次查询", querier.rowCalls)
	}
	if !strings.Contains(querier.rowSQL[1], "WHERE status = 'running'") {
		t.Fatalf("第二次查询应取进行中轮次，实际语句 %q", querier.rowSQL[1])
	}
}

// TestStartCollectRunReportsConflictWhenRunningRoundDisappeared 覆盖冲突与补查之间
// 那一轮刚好结束的竞态：没有可复用轮次时报 ErrRunConflict，由下一轮重试。
func TestStartCollectRunReportsConflictWhenRunningRoundDisappeared(t *testing.T) {
	querier := &runQuerierStub{rowResults: []pgx.Row{
		errRunRow(&pgconn.PgError{Code: "23505", ConstraintName: collectRunSingleRunningIndex}),
		errRunRow(pgx.ErrNoRows),
	}}

	_, _, err := StartCollectRun(context.Background(), querier, time.Now())
	if !errors.Is(err, model.ErrRunConflict) {
		t.Fatalf("期望 ErrRunConflict，实际 %v", err)
	}
	if errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("冲突不是数据库读写失败，不应包装 ErrRunStoreFailed，实际 %v", err)
	}
	if querier.rowCalls != 2 {
		t.Fatalf("应补查一次已有轮次，实际 %d 次查询", querier.rowCalls)
	}
}

// TestStartCollectRunWrapsExistingLookupFailure 覆盖冲突后的补查失败：按数据库读写
// 失败上抛，而不是误报成 ErrRunConflict。
func TestStartCollectRunWrapsExistingLookupFailure(t *testing.T) {
	lookupErr := errors.New("连接断开")
	querier := &runQuerierStub{rowResults: []pgx.Row{
		errRunRow(&pgconn.PgError{Code: "23505", ConstraintName: collectRunSingleRunningIndex}),
		errRunRow(lookupErr),
	}}

	_, _, err := StartCollectRun(context.Background(), querier, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
	if !errors.Is(err, lookupErr) {
		t.Fatalf("应保留底层错误，实际 %v", err)
	}
	if errors.Is(err, model.ErrRunConflict) {
		t.Fatalf("补查失败不是轮次冲突，实际 %v", err)
	}
}

// TestStartCollectRunRejectsOtherInsertErrors 覆盖非唯一冲突的插入失败：必须上抛，
// 不能误当成「已有一轮」而吞掉连接或语法错误。
func TestStartCollectRunRejectsOtherInsertErrors(t *testing.T) {
	insertErr := errors.New("连接断开")
	querier := &runQuerierStub{rowResults: []pgx.Row{errRunRow(insertErr)}}

	_, _, err := StartCollectRun(context.Background(), querier, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
	if !errors.Is(err, insertErr) {
		t.Fatalf("应保留底层错误，实际 %v", err)
	}
	if querier.rowCalls != 1 {
		t.Fatalf("非唯一冲突不应再查已有轮次，实际 %d 次查询", querier.rowCalls)
	}
}

// TestStartCollectRunRejectsOtherUniqueViolations 覆盖「别的唯一约束冲突」：
// 只有 collect_run_single_running 才代表已有一轮，其他约束冲突必须报错。
func TestStartCollectRunRejectsOtherUniqueViolations(t *testing.T) {
	querier := &runQuerierStub{rowResults: []pgx.Row{
		errRunRow(&pgconn.PgError{Code: "23505", ConstraintName: "collect_run_pkey"}),
	}}

	_, _, err := StartCollectRun(context.Background(), querier, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
	if querier.rowCalls != 1 {
		t.Fatalf("别的唯一约束不应触发补查，实际 %d 次查询", querier.rowCalls)
	}
}

// TestIsSingleRunningViolation 固定冲突判定的两个条件：SQLSTATE 与索引名都要匹配，
// 且包装后的错误也能识别。
func TestIsSingleRunningViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "本索引的唯一冲突",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: collectRunSingleRunningIndex},
			want: true,
		},
		{
			name: "包装后的本索引冲突",
			err:  fmt.Errorf("插入失败: %w", &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: collectRunSingleRunningIndex}),
			want: true,
		},
		{
			name: "别的唯一约束",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "collect_run_pkey"},
			want: false,
		},
		{
			name: "别的 SQLSTATE",
			err:  &pgconn.PgError{Code: "23503", ConstraintName: collectRunSingleRunningIndex},
			want: false,
		},
		{name: "普通错误", err: errors.New("连接断开"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := isSingleRunningViolation(testCase.err); got != testCase.want {
				t.Fatalf("isSingleRunningViolation(%v) = %v，期望 %v", testCase.err, got, testCase.want)
			}
		})
	}
}

// TestFailStaleRunsReturnsConvergedRunIDs 覆盖过期收敛：一条 UPDATE 带回所有被置为
// failed 的轮次编号，参数是「现在」与「截止时刻」。
func TestFailStaleRunsReturnsConvergedRunIDs(t *testing.T) {
	now := time.Date(2026, 11, 4, 11, 37, 31, 0, time.UTC)
	cutoff := now.Add(-staleThreshold)
	rows := &staleRunRowsStub{runIDs: []int64{11, 12}}
	querier := &runQuerierStub{queryRows: rows}

	runIDs, err := FailStaleRuns(context.Background(), querier, now, cutoff)
	if err != nil {
		t.Fatalf("收敛过期轮次不应报错，实际 %v", err)
	}
	if len(runIDs) != 2 || runIDs[0] != 11 || runIDs[1] != 12 {
		t.Fatalf("被收敛的轮次期望 [11 12]，实际 %v", runIDs)
	}
	if !rows.closed {
		t.Fatal("查询结果必须关闭，避免占住连接")
	}
	if querier.queryCalls != 1 {
		t.Fatalf("过期收敛只应下发一条 UPDATE，实际 %d 条", querier.queryCalls)
	}
	if got := querier.queryArgs[0].(time.Time); !got.Equal(now) {
		t.Fatalf("finished_at 期望 %s，实际 %s", now, got)
	}
	if got := querier.queryArgs[1].(time.Time); !got.Equal(cutoff) {
		t.Fatalf("截止时刻期望 %s，实际 %s", cutoff, got)
	}
	if !strings.Contains(querier.querySQL, "stale_run") || !strings.Contains(querier.querySQL, "started_at < $2") {
		t.Fatalf("过期收敛语句应写 stale_run 且按 started_at 判定，实际 %q", querier.querySQL)
	}
}

// TestFailStaleRunsWrapsQueryFailure 覆盖 UPDATE 本身失败。
func TestFailStaleRunsWrapsQueryFailure(t *testing.T) {
	querier := &runQuerierStub{queryErr: errors.New("连接断开")}

	_, err := FailStaleRuns(context.Background(), querier, time.Now(), time.Now().Add(-staleThreshold))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestFailStaleRunsWrapsIterationFailure 覆盖 UPDATE 执行中读取结果失败。
func TestFailStaleRunsWrapsIterationFailure(t *testing.T) {
	rows := &staleRunRowsStub{runIDs: []int64{11}, iterErr: errors.New("读取中断")}
	querier := &runQuerierStub{queryRows: rows}

	_, err := FailStaleRuns(context.Background(), querier, time.Now(), time.Now().Add(-staleThreshold))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
	if !rows.closed {
		t.Fatal("即使遍历失败也必须关闭结果集")
	}
}

// TestFailStaleRunsWrapsScanFailure 覆盖单行扫描失败。
func TestFailStaleRunsWrapsScanFailure(t *testing.T) {
	rows := &staleRunRowsStub{runIDs: []int64{11}, scanErr: errors.New("类型不符")}
	querier := &runQuerierStub{queryRows: rows}

	_, err := FailStaleRuns(context.Background(), querier, time.Now(), time.Now().Add(-staleThreshold))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("期望 ErrRunStoreFailed，实际 %v", err)
	}
}
