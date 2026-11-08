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

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// runStatusRowsStub 是 pgx.Rows 的替身：只覆盖列表/周结果用到的 Next/Scan/Err/Close，
// 其余方法嵌入接口、调用会 panic——替身只放行被测路径，测错方法立刻暴露。
//
// Scan 按位置真赋值并校验类型：只会回 nil 的替身能让「列顺序写反」「可空列扫进
// 值类型」照样通过，而那正是只在真实库上才炸的错误。
type runStatusRowsStub struct {
	pgx.Rows
	rows    [][]any // 依次回放的行，每行按 Scan 目标的列顺序给值
	current int     // 已被 Next 取出的行数，也是当前行在 rows 里的下标 +1

	scanErr   error // 第 scanErrAt 次 Scan 回放的扫描错误（有行，但这一行解不出来）
	scanErrAt int   // 第几次 Scan（从 1 起）才失败；0 表示不注入扫描错误
	scanCalls int   // Scan 已调用的次数

	err error // Err 回放的遍历错误（取到一半连接断了）

	closed bool // Close 是否被调用：漏关行集会占住连接，而返回值看起来一切正常
}

// Next 逐行推进：取完 rows 后返回 false，模拟行集遍历结束。
func (r *runStatusRowsStub) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

// Scan 把当前行的值按位置赋给 dest；列数或类型不符立即报错。
func (r *runStatusRowsStub) Scan(dest ...any) error {
	r.scanCalls++
	if r.scanErrAt > 0 && r.scanCalls == r.scanErrAt {
		return r.scanErr
	}
	if r.current == 0 {
		return errors.New("没有当前行时调用 Scan")
	}
	return scanRunStatusValues(dest, r.rows[r.current-1])
}

// Close 由仓储的 defer 调用，并记下真的被调过。
func (r *runStatusRowsStub) Close() { r.closed = true }

// Err 回放预设的遍历错误。
func (r *runStatusRowsStub) Err() error { return r.err }

// runStatusRowStub 是 pgx.Row 的替身：按位置回放一行，或回放预设错误。
type runStatusRowStub struct {
	values []any
	err    error
}

// Scan 把预设值按位置赋给 dest；err 非空时直接返回它（用于 pgx.ErrNoRows 等）。
func (r *runStatusRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanRunStatusValues(dest, r.values)
}

// scanRunStatusValues 按位置赋值，dest 与 values 的列数必须一致。
// 支持 RunSummary、RunWeekResult、count(*) 与 LoadRunTaskSummary 用到的目标类型。
func scanRunStatusValues(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("列数期望 %d，实际 %d", len(values), len(dest))
	}
	for i := range dest {
		if err := assignRunStatusValue(dest[i], values[i]); err != nil {
			return fmt.Errorf("第 %d 列: %w", i, err)
		}
	}
	return nil
}

// assignRunStatusValue 是单列赋值：值类型直接写，可空列的 nil 写成 nil 指针。
// 不支持的组合返回错误而不是静默跳过，避免替身掩盖仓储扫错列。
func assignRunStatusValue(dest any, value any) error {
	switch d := dest.(type) {
	case *int64:
		v, ok := value.(int64)
		if !ok {
			return fmt.Errorf("期望 int64，实际 %T", value)
		}
		*d = v
	case *int:
		v, ok := value.(int)
		if !ok {
			return fmt.Errorf("期望 int，实际 %T", value)
		}
		*d = v
	case *string:
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("期望 string，实际 %T", value)
		}
		*d = v
	case *bool:
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("期望 bool，实际 %T", value)
		}
		*d = v
	case *time.Time:
		v, ok := value.(time.Time)
		if !ok {
			return fmt.Errorf("期望 time.Time，实际 %T", value)
		}
		*d = v
	case **string:
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("期望 string 或 nil，实际 %T", value)
		}
		*d = &v
	case **int:
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(int)
		if !ok {
			return fmt.Errorf("期望 int 或 nil，实际 %T", value)
		}
		*d = &v
	case **time.Time:
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(time.Time)
		if !ok {
			return fmt.Errorf("期望 time.Time 或 nil，实际 %T", value)
		}
		*d = &v
	default:
		return fmt.Errorf("目标类型 %T 不被替身支持", dest)
	}
	return nil
}

// runStatusQuerierStub 是 runStatusQuerier 的替身：Query 回放行集，QueryRow 回放单行，
// 并记录 SQL 与参数供片段断言。Exec 没有调用点（blocked 直插走 RETURNING），调用即
// panic，避免「以为测了却走了空路径」。
type runStatusQuerierStub struct {
	rows      pgx.Rows
	queryErr  error
	querySQL  string
	queryArgs []any

	row      pgx.Row
	rowSQL   string
	rowArgs  []any
	rowCalls int
}

// Query 记录 SQL 与参数，并回放预设的行集或错误。
func (s *runStatusQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.querySQL = sql
	s.queryArgs = args
	return s.rows, s.queryErr
}

// QueryRow 记录 SQL 与参数，并回放预设的单行或错误。
func (s *runStatusQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.rowCalls++
	s.rowSQL = sql
	s.rowArgs = args
	return s.row
}

// Exec 实现接口但不被当前仓储调用。
func (s *runStatusQuerierStub) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("单元测试不应调用 Exec：RunStatusRepo 的写路径走 QueryRow 的 RETURNING")
}

// runStatusTestNow 是单元用例统一使用的固定时刻，避免 time.Now 让断言漂移。
func runStatusTestNow() time.Time {
	return time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
}

// runStatusSummaryFixture 是一轮「有学期、有窗口、已发布、已失败」的完整摘要：
// 所有可空列都有值，用来验证 22 列的扫描顺序与指针目标。
//
// 相邻的同类型列必须取不同的值：finished_at / updated_at / released_at 三个时刻、
// rooms_total 与 optional_total 两个计数，一旦取值相同，列序写反后扫出来的摘要仍与
// 期望逐字段相等，这条断言就再也测不出「22 列顺序」。
func runStatusSummaryFixture() RunSummary {
	startedAt := runStatusTestNow()
	finishedAt := startedAt.Add(12 * time.Minute)
	updatedAt := startedAt.Add(15 * time.Minute) // 收官之后仍有写入
	releasedAt := startedAt.Add(9 * time.Minute) // 发布发生在收官之前
	term := "2026-2027-1"
	windowStart, windowEnd := 10, 20
	releaseID := "rel-20261108"
	errorCode := "week_matrix_failed"
	errorMessage := "矩阵结构不符合预期"
	return RunSummary{
		RunID:             21,
		Status:            "failed",
		Phase:             "finished",
		StartedAt:         startedAt,
		FinishedAt:        &finishedAt,
		UpdatedAt:         updatedAt,
		Term:              &term,
		WindowStart:       &windowStart,
		WindowEnd:         &windowEnd,
		WeeksTotal:        11,
		RoomsTotal:        128,
		RequiredTotal:     44,
		RequiredDone:      43,
		OptionalTotal:     320,
		OptionalDone:      318,
		UnknownCells:      7,
		CompositeCells:    9,
		AttrConflictCount: 2,
		ReleaseID:         &releaseID,
		ReleasedAt:        &releasedAt,
		ErrorCode:         &errorCode,
		ErrorMessage:      &errorMessage,
	}
}

// runStatusSummaryRow 把摘要折成一行的值序列，顺序与 runSummaryColumns 一致；
// 指针列按 nil/值两种形态回放，供替身的按位置赋值使用。
func runStatusSummaryRow(summary RunSummary) []any {
	valueOrNil := func(pointer *string) any {
		if pointer == nil {
			return nil
		}
		return *pointer
	}
	intOrNil := func(pointer *int) any {
		if pointer == nil {
			return nil
		}
		return *pointer
	}
	timeOrNil := func(pointer *time.Time) any {
		if pointer == nil {
			return nil
		}
		return *pointer
	}
	return []any{
		summary.RunID,
		summary.Status,
		summary.Phase,
		summary.StartedAt,
		timeOrNil(summary.FinishedAt),
		summary.UpdatedAt,
		valueOrNil(summary.Term),
		intOrNil(summary.WindowStart),
		intOrNil(summary.WindowEnd),
		summary.WeeksTotal,
		summary.RoomsTotal,
		summary.RequiredTotal,
		summary.RequiredDone,
		summary.OptionalTotal,
		summary.OptionalDone,
		summary.UnknownCells,
		summary.CompositeCells,
		summary.AttrConflictCount,
		valueOrNil(summary.ReleaseID),
		timeOrNil(summary.ReleasedAt),
		valueOrNil(summary.ErrorCode),
		valueOrNil(summary.ErrorMessage),
	}
}

// sameOptionalString 按值比较可空字符串，nil 与空串是两种不同的值。
func sameOptionalString(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// sameOptionalInt 按值比较可空整数。
func sameOptionalInt(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

// sameOptionalTime 按时刻比较可空时间，不被时区表示差异影响。
func sameOptionalTime(a, b *time.Time) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || a.Equal(*b)
}

// sameRunSummary 逐字段比较两份摘要。
//
// 不能直接用 ==：可空列是指针，== 比的是地址，替身每行都会新分配，值相同也会判不等。
// time.Time 用 Equal 而不是 ==，避免同一时刻因时区表示不同被当成两个值。
func sameRunSummary(a, b RunSummary) bool {
	return a.RunID == b.RunID &&
		a.Status == b.Status &&
		a.Phase == b.Phase &&
		a.StartedAt.Equal(b.StartedAt) &&
		sameOptionalTime(a.FinishedAt, b.FinishedAt) &&
		a.UpdatedAt.Equal(b.UpdatedAt) &&
		sameOptionalString(a.Term, b.Term) &&
		sameOptionalInt(a.WindowStart, b.WindowStart) &&
		sameOptionalInt(a.WindowEnd, b.WindowEnd) &&
		a.WeeksTotal == b.WeeksTotal &&
		a.RoomsTotal == b.RoomsTotal &&
		a.RequiredTotal == b.RequiredTotal &&
		a.RequiredDone == b.RequiredDone &&
		a.OptionalTotal == b.OptionalTotal &&
		a.OptionalDone == b.OptionalDone &&
		a.UnknownCells == b.UnknownCells &&
		a.CompositeCells == b.CompositeCells &&
		a.AttrConflictCount == b.AttrConflictCount &&
		sameOptionalString(a.ReleaseID, b.ReleaseID) &&
		sameOptionalTime(a.ReleasedAt, b.ReleasedAt) &&
		sameOptionalString(a.ErrorCode, b.ErrorCode) &&
		sameOptionalString(a.ErrorMessage, b.ErrorMessage)
}

// TestRecentListsRunsAndCountsTotal 覆盖列表口径：SQL 的排序、分页与列清单，
// 22 列按序扫描（含可空列），以及列表之后单独取一次总数。
func TestRecentListsRunsAndCountsTotal(t *testing.T) {
	first := runStatusSummaryFixture()

	// 第二条模拟还没收官的轮次：running 才是 finished_at 真正为 NULL 的状态——blocked
	// 轮次建轮即终态，三个时刻同刻落库，finished_at 反而不会为空（design D5）。
	// 其余可空列一并置空，覆盖 22 列里每个指针目标的 NULL 分支。
	second := first
	second.RunID = first.RunID + 1
	second.Status = "running"
	second.Phase = "week_matrix"
	second.FinishedAt = nil
	second.Term = nil
	second.WindowStart = nil
	second.WindowEnd = nil
	second.ReleaseID = nil
	second.ReleasedAt = nil
	second.ErrorCode = nil
	second.ErrorMessage = nil

	rowsStub := &runStatusRowsStub{rows: [][]any{
		runStatusSummaryRow(first),
		runStatusSummaryRow(second),
	}}
	querier := &runStatusQuerierStub{
		rows: rowsStub,
		row:  &runStatusRowStub{values: []any{7}},
	}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 2, 10)
	if err != nil {
		t.Fatalf("读取轮次列表失败: %v", err)
	}
	if total != 7 {
		t.Fatalf("总数应来自 count(*)，期望 7，实际 %d", total)
	}
	if len(summaries) != 2 {
		t.Fatalf("期望 2 条摘要，实际 %d 条", len(summaries))
	}
	if !sameRunSummary(summaries[0], first) {
		t.Fatalf("第一条摘要期望 %+v，实际 %+v", first, summaries[0])
	}
	if !sameRunSummary(summaries[1], second) {
		t.Fatalf("第二条摘要期望 %+v，实际 %+v", second, summaries[1])
	}
	if summaries[1].Term != nil || summaries[1].FinishedAt != nil || summaries[1].ErrorCode != nil {
		t.Fatalf("空列必须扫成 nil 而不是零值: %+v", summaries[1])
	}

	// 排序必须补 run_id 兜底：同刻多轮时只按 started_at 的排序不稳定，翻页会重复/漏条。
	for _, fragment := range []string{
		"FROM collect_run",
		"ORDER BY started_at DESC, run_id DESC",
		"LIMIT $1 OFFSET $2",
		"term",
		"window_start",
		"window_end",
		"release_id",
		"released_at",
		"error_code",
		"error_message",
	} {
		if !strings.Contains(querier.querySQL, fragment) {
			t.Fatalf("列表 SQL 应包含 %q，实际 %q", fragment, querier.querySQL)
		}
	}
	if len(querier.queryArgs) != 2 || querier.queryArgs[0] != 2 || querier.queryArgs[1] != 10 {
		t.Fatalf("LIMIT/OFFSET 参数期望 [2 10]，实际 %+v", querier.queryArgs)
	}
	if !strings.Contains(querier.rowSQL, "count(*)") {
		t.Fatalf("总数应走 count(*) 查询，实际 SQL %q", querier.rowSQL)
	}
	if !rowsStub.closed {
		t.Fatal("查询结果必须关闭，避免占住连接")
	}
}

// TestRecentReturnsEmptySliceAndZeroTotal 覆盖空表：列表是空切片而不是 nil，
// 总数是 0，且仍然查询了一次 count(*)。
func TestRecentReturnsEmptySliceAndZeroTotal(t *testing.T) {
	rowsStub := &runStatusRowsStub{}
	querier := &runStatusQuerierStub{
		rows: rowsStub,
		row:  &runStatusRowStub{values: []any{0}},
	}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 20, 0)
	if err != nil {
		t.Fatalf("空表读取不应报错: %v", err)
	}
	if summaries == nil {
		t.Fatal("空表应返回空切片而不是 nil，避免调用方写两种判空")
	}
	if len(summaries) != 0 {
		t.Fatalf("空表应返回 0 条摘要，实际 %d 条", len(summaries))
	}
	if total != 0 {
		t.Fatalf("空表总数应为 0，实际 %d", total)
	}
	if querier.rowCalls != 1 {
		t.Fatalf("应查询一次总数，实际 %d 次", querier.rowCalls)
	}
	if !rowsStub.closed {
		t.Fatal("空结果也必须关闭行集")
	}
}

// TestRecentWrapsQueryError 覆盖列表查询直接失败：包 ErrRunStoreFailed，
// 且不返回半套结果。
func TestRecentWrapsQueryError(t *testing.T) {
	querier := &runStatusQuerierStub{queryErr: errors.New("连接中断")}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 20, 0)
	if err == nil {
		t.Fatal("查询失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if summaries != nil || total != 0 {
		t.Fatalf("失败时不应返回部分结果，实际 %+v / %d", summaries, total)
	}
}

// TestRecentWrapsScanError 覆盖「第一行扫得出来、第二行扫不出来」：扫描失败包哨兵，
// 且不把已组装的那条当结果交出去。
func TestRecentWrapsScanError(t *testing.T) {
	first := runStatusSummaryFixture()
	rowsStub := &runStatusRowsStub{
		rows: [][]any{
			runStatusSummaryRow(first),
			runStatusSummaryRow(first),
		},
		scanErrAt: 2,
		scanErr:   errors.New("列类型不匹配"),
	}
	querier := &runStatusQuerierStub{rows: rowsStub}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 20, 0)
	if err == nil {
		t.Fatal("扫描失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if summaries != nil || total != 0 {
		t.Fatalf("扫描失败时不应返回部分结果，实际 %+v / %d", summaries, total)
	}
	if rowsStub.scanCalls != 2 {
		t.Fatalf("应在第 2 次 Scan 失败，实际 Scan 调用 %d 次", rowsStub.scanCalls)
	}
	if !rowsStub.closed {
		t.Fatal("扫描失败也必须关闭结果集")
	}
}

// TestRecentWrapsRowsError 覆盖「取到一半连接断了」：rows.Err 非空时整体报错，
// 不能把残行当完整一页交出去。
func TestRecentWrapsRowsError(t *testing.T) {
	rowsStub := &runStatusRowsStub{
		rows: [][]any{runStatusSummaryRow(runStatusSummaryFixture())},
		err:  errors.New("连接中断"),
	}
	querier := &runStatusQuerierStub{rows: rowsStub}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 20, 0)
	if err == nil {
		t.Fatal("遍历失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if summaries != nil || total != 0 {
		t.Fatalf("遍历失败时不应返回部分结果，实际 %+v / %d", summaries, total)
	}
	if !rowsStub.closed {
		t.Fatal("失败路径也必须关闭结果集")
	}
}

// TestRecentWrapsCountError 覆盖总数查询失败：列表已经取出来了也必须整体报错，
// 不能返回「有列表、无总数」的半套结果。
func TestRecentWrapsCountError(t *testing.T) {
	rowsStub := &runStatusRowsStub{
		rows: [][]any{runStatusSummaryRow(runStatusSummaryFixture())},
	}
	querier := &runStatusQuerierStub{
		rows: rowsStub,
		row:  &runStatusRowStub{err: errors.New("计数超时")},
	}

	summaries, total, err := NewRunStatusRepo(querier).Recent(context.Background(), 20, 0)
	if err == nil {
		t.Fatal("总数查询失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if summaries != nil || total != 0 {
		t.Fatalf("失败时不应返回部分结果，实际 %+v / %d", summaries, total)
	}
}

// TestByIDReturnsSummary 覆盖单轮读取：整行按 22 列扫描，查询按 run_id 定位。
func TestByIDReturnsSummary(t *testing.T) {
	want := runStatusSummaryFixture()
	querier := &runStatusQuerierStub{
		row: &runStatusRowStub{values: runStatusSummaryRow(want)},
	}

	got, found, err := NewRunStatusRepo(querier).ByID(context.Background(), want.RunID)
	if err != nil {
		t.Fatalf("读取单轮失败: %v", err)
	}
	if !found {
		t.Fatal("已存在的轮次应返回 found=true")
	}
	if !sameRunSummary(got, want) {
		t.Fatalf("摘要期望 %+v，实际 %+v", want, got)
	}
	if !strings.Contains(querier.rowSQL, "WHERE run_id = $1") {
		t.Fatalf("单轮查询应按 run_id 过滤，实际 SQL %q", querier.rowSQL)
	}
	if len(querier.rowArgs) != 1 || querier.rowArgs[0] != want.RunID {
		t.Fatalf("查询参数期望 [%d]，实际 %+v", want.RunID, querier.rowArgs)
	}
}

// TestByIDReportsMissingWithoutError 覆盖未知 id：found=false 且 error 为 nil。
// 「没有这一轮」交给 handler 映射 404，仓储不把它当读库失败，也不返回 ErrRunNotFound。
func TestByIDReportsMissingWithoutError(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: pgx.ErrNoRows}}

	summary, found, err := NewRunStatusRepo(querier).ByID(context.Background(), 999)
	if err != nil {
		t.Fatalf("未知 id 不应返回错误，实际 %v", err)
	}
	if found {
		t.Fatal("未知 id 应返回 found=false")
	}
	if summary != (RunSummary{}) {
		t.Fatalf("未找到时应返回零值摘要，实际 %+v", summary)
	}
}

// TestByIDWrapsReadError 覆盖真正的读库失败：包 ErrRunStoreFailed，且 found=false。
func TestByIDWrapsReadError(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: errors.New("连接中断")}}

	_, found, err := NewRunStatusRepo(querier).ByID(context.Background(), 5)
	if err == nil {
		t.Fatal("读库失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if found {
		t.Fatal("读库失败时不应报告 found=true")
	}
}

// TestRunningReturnsRunningSummary 覆盖冲突判定用的读取：只按 status='running' 过滤，
// 取首条（唯一索引保证至多一条）。
func TestRunningReturnsRunningSummary(t *testing.T) {
	want := runStatusSummaryFixture()
	want.Status = "running"
	want.Phase = "week_matrix"
	want.FinishedAt = nil
	querier := &runStatusQuerierStub{row: &runStatusRowStub{values: runStatusSummaryRow(want)}}

	got, found, err := NewRunStatusRepo(querier).Running(context.Background())
	if err != nil {
		t.Fatalf("读取进行中轮次失败: %v", err)
	}
	if !found {
		t.Fatal("进行中轮次应返回 found=true")
	}
	if !sameRunSummary(got, want) {
		t.Fatalf("摘要期望 %+v，实际 %+v", want, got)
	}
	for _, fragment := range []string{"WHERE status = 'running'", "ORDER BY run_id", "LIMIT 1"} {
		if !strings.Contains(querier.rowSQL, fragment) {
			t.Fatalf("进行中查询应包含 %q，实际 %q", fragment, querier.rowSQL)
		}
	}
	if len(querier.rowArgs) != 0 {
		t.Fatalf("进行中查询不应带参数，实际 %+v", querier.rowArgs)
	}
}

// TestRunningReportsNoRunningRun 覆盖没有进行中轮次：found=false 且无错误。
func TestRunningReportsNoRunningRun(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: pgx.ErrNoRows}}

	summary, found, err := NewRunStatusRepo(querier).Running(context.Background())
	if err != nil {
		t.Fatalf("没有进行中轮次不应报错，实际 %v", err)
	}
	if found {
		t.Fatal("没有进行中轮次时应返回 found=false")
	}
	if summary != (RunSummary{}) {
		t.Fatalf("没有进行中轮次时应返回零值摘要，实际 %+v", summary)
	}
}

// TestRunningWrapsReadError 覆盖进行中轮次的真正读库失败：包 ErrRunStoreFailed，
// 且 found=false。失败不能被当成「没有进行中轮次」——那会让冲突判定直接放行第二轮。
func TestRunningWrapsReadError(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: errors.New("连接中断")}}

	summary, found, err := NewRunStatusRepo(querier).Running(context.Background())
	if err == nil {
		t.Fatal("读库失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if found {
		t.Fatal("读库失败时不应报告 found=true")
	}
	if summary != (RunSummary{}) {
		t.Fatalf("读库失败时应返回零值摘要，实际 %+v", summary)
	}
}

// TestWeeksScansResultsInQueryOrder 覆盖周结果：五列按序扫描（含两个可空列），
// SQL 固定按 (week, building_id) 排序。
func TestWeeksScansResultsInQueryOrder(t *testing.T) {
	fetchedAt := runStatusTestNow()
	errorMessage := "页面结构不符合预期"
	rowsStub := &runStatusRowsStub{rows: [][]any{
		{1, "Q10", true, fetchedAt, nil},
		{2, "Q10", false, nil, errorMessage},
	}}
	querier := &runStatusQuerierStub{rows: rowsStub}

	weeks, err := NewRunStatusRepo(querier).Weeks(context.Background(), 21)
	if err != nil {
		t.Fatalf("读取周结果失败: %v", err)
	}
	want := []RunWeekResult{
		{Week: 1, BuildingID: "Q10", OK: true, FetchedAt: &fetchedAt},
		{Week: 2, BuildingID: "Q10", OK: false, ErrorMessage: &errorMessage},
	}
	if len(weeks) != len(want) {
		t.Fatalf("期望 %d 条周结果，实际 %d 条: %+v", len(want), len(weeks), weeks)
	}
	for i := range want {
		if weeks[i].Week != want[i].Week || weeks[i].BuildingID != want[i].BuildingID || weeks[i].OK != want[i].OK {
			t.Fatalf("第 %d 条周结果期望 %+v，实际 %+v", i, want[i], weeks[i])
		}
		if (weeks[i].FetchedAt == nil) != (want[i].FetchedAt == nil) ||
			(weeks[i].FetchedAt != nil && !weeks[i].FetchedAt.Equal(*want[i].FetchedAt)) {
			t.Fatalf("第 %d 条 fetched_at 不符: %+v", i, weeks[i].FetchedAt)
		}
		if (weeks[i].ErrorMessage == nil) != (want[i].ErrorMessage == nil) ||
			(weeks[i].ErrorMessage != nil && *weeks[i].ErrorMessage != *want[i].ErrorMessage) {
			t.Fatalf("第 %d 条 error_message 不符: %+v", i, weeks[i].ErrorMessage)
		}
	}
	for _, fragment := range []string{"FROM collect_run_week", "WHERE run_id = $1", "ORDER BY week, building_id"} {
		if !strings.Contains(querier.querySQL, fragment) {
			t.Fatalf("周结果 SQL 应包含 %q，实际 %q", fragment, querier.querySQL)
		}
	}
	if len(querier.queryArgs) != 1 || querier.queryArgs[0] != int64(21) {
		t.Fatalf("周结果查询参数期望 [21]，实际 %+v", querier.queryArgs)
	}
	if !rowsStub.closed {
		t.Fatal("查询结果必须关闭，避免占住连接")
	}
}

// TestWeeksReturnsEmptySlice 覆盖没有周结果（含未知 run_id）：空切片而不是 nil。
func TestWeeksReturnsEmptySlice(t *testing.T) {
	rowsStub := &runStatusRowsStub{}
	querier := &runStatusQuerierStub{rows: rowsStub}

	weeks, err := NewRunStatusRepo(querier).Weeks(context.Background(), 404)
	if err != nil {
		t.Fatalf("没有周结果不应报错: %v", err)
	}
	if weeks == nil {
		t.Fatal("没有周结果时应返回空切片而不是 nil")
	}
	if len(weeks) != 0 {
		t.Fatalf("没有周结果时应返回 0 条，实际 %d 条", len(weeks))
	}
	if !rowsStub.closed {
		t.Fatal("空结果也必须关闭行集")
	}
}

// TestWeeksWrapsErrors 覆盖周结果的查询失败、扫描失败与遍历失败：都包 ErrRunStoreFailed。
func TestWeeksWrapsErrors(t *testing.T) {
	t.Run("查询失败", func(t *testing.T) {
		querier := &runStatusQuerierStub{queryErr: errors.New("连接中断")}
		weeks, err := NewRunStatusRepo(querier).Weeks(context.Background(), 1)
		if err == nil || !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
		}
		if weeks != nil {
			t.Fatalf("失败时不应返回部分结果，实际 %+v", weeks)
		}
	})

	t.Run("扫描失败", func(t *testing.T) {
		rowsStub := &runStatusRowsStub{
			rows:      [][]any{{1, "Q10", true, nil, nil}},
			scanErrAt: 1,
			scanErr:   errors.New("列类型不匹配"),
		}
		querier := &runStatusQuerierStub{rows: rowsStub}
		weeks, err := NewRunStatusRepo(querier).Weeks(context.Background(), 1)
		if err == nil || !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
		}
		if weeks != nil {
			t.Fatalf("扫描失败时不应返回部分结果，实际 %+v", weeks)
		}
		if !rowsStub.closed {
			t.Fatal("扫描失败也必须关闭结果集")
		}
	})

	t.Run("遍历失败", func(t *testing.T) {
		rowsStub := &runStatusRowsStub{
			rows: [][]any{{1, "Q10", true, nil, nil}},
			err:  errors.New("连接中断"),
		}
		querier := &runStatusQuerierStub{rows: rowsStub}
		weeks, err := NewRunStatusRepo(querier).Weeks(context.Background(), 1)
		if err == nil || !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
		}
		if weeks != nil {
			t.Fatalf("遍历失败时不应返回部分结果，实际 %+v", weeks)
		}
	})
}

// TestTaskSummaryDelegatesToLoadRunTaskSummary 锁住「只有一份聚合实现」：
// TaskSummary 必须走包内既有的 LoadRunTaskSummary（同一段 SQL、同一个 runID 参数），
// 而不是在这里再写一条 count 查询。
func TestTaskSummaryDelegatesToLoadRunTaskSummary(t *testing.T) {
	want := RunTaskSummary{
		RequiredTotal:       44,
		RequiredDone:        43,
		RequiredFailed:      1,
		OptionalTotal:       320,
		OptionalDone:        318,
		OptionalFailed:      2,
		FirstFailureMessage: "矩阵结构不符合预期",
		RecordedErrorCode:   "week_matrix_failed",
	}
	querier := &runStatusQuerierStub{row: &runStatusRowStub{values: []any{
		want.RequiredTotal,
		want.RequiredDone,
		want.RequiredFailed,
		want.OptionalTotal,
		want.OptionalDone,
		want.OptionalFailed,
		want.FirstFailureMessage,
		want.RecordedErrorCode,
		true,
	}}}

	got, err := NewRunStatusRepo(querier).TaskSummary(context.Background(), 21)
	if err != nil {
		t.Fatalf("读取任务汇总失败: %v", err)
	}
	if got != want {
		t.Fatalf("任务汇总期望 %+v，实际 %+v", want, got)
	}
	if querier.rowSQL != loadRunTaskSummarySQL {
		t.Fatalf("应复用 LoadRunTaskSummary 的 SQL，实际 %q", querier.rowSQL)
	}
	if len(querier.rowArgs) != 1 || querier.rowArgs[0] != int64(21) {
		t.Fatalf("查询参数期望 [21]，实际 %+v", querier.rowArgs)
	}
}

// TestTaskSummaryReportsMissingRun 覆盖未知 run_id：沿用 LoadRunTaskSummary 的行为，
// 返回包 ErrRunStoreFailed 的错误，而不是「一轮零任务、全部成功」的全零汇总。
func TestTaskSummaryReportsMissingRun(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{values: []any{
		0, 0, 0, 0, 0, 0, "", "", false,
	}}}

	summary, err := NewRunStatusRepo(querier).TaskSummary(context.Background(), 404)
	if err == nil {
		t.Fatal("轮次不存在时任务汇总应报错")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if summary != (RunTaskSummary{}) {
		t.Fatalf("失败时不应返回汇总，实际 %+v", summary)
	}
}

// TestTaskSummaryWrapsReadError 覆盖任务汇总的读库失败：包 ErrRunStoreFailed。
func TestTaskSummaryWrapsReadError(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: errors.New("连接中断")}}

	_, err := NewRunStatusRepo(querier).TaskSummary(context.Background(), 21)
	if err == nil {
		t.Fatal("读库失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestStartBlockedRunInsertsTerminalRun 覆盖 blocked 直插：三个时刻共用一个 now、
// 状态与阶段以终态落库、error_code 取固定常量、reason 原样进 error_message，
// 并通过 RETURNING 取回 run_id 与 started_at。
func TestStartBlockedRunInsertsTerminalRun(t *testing.T) {
	now := runStatusTestNow()
	querier := &runStatusQuerierStub{row: &runStatusRowStub{values: []any{int64(31), now}}}

	runID, startedAt, err := NewRunStatusRepo(querier).StartBlockedRun(context.Background(), "白名单为空", now)
	if err != nil {
		t.Fatalf("创建阻止轮次失败: %v", err)
	}
	if runID != 31 {
		t.Fatalf("run_id 期望 31，实际 %d", runID)
	}
	if !startedAt.Equal(now) {
		t.Fatalf("started_at 期望 %s，实际 %s", now, startedAt)
	}

	for _, fragment := range []string{
		"INSERT INTO collect_run",
		// 错误码用常量拼出期望片段：改名时这条断言跟着常量走，不靠两份字面量对齐。
		"VALUES ($1, $1, $1, 'blocked', 'finished', '" + whitelistEmptyErrorCode + "', $2)",
		"RETURNING run_id, started_at",
	} {
		if !strings.Contains(querier.rowSQL, fragment) {
			t.Fatalf("blocked 建轮 SQL 应包含 %q，实际 %q", fragment, querier.rowSQL)
		}
	}
	if len(querier.rowArgs) != 2 {
		t.Fatalf("blocked 建轮应有 2 个参数，实际 %d 个", len(querier.rowArgs))
	}
	gotNow, ok := querier.rowArgs[0].(time.Time)
	if !ok || !gotNow.Equal(now) {
		t.Fatalf("第一个参数应是 now=%s，实际 %v", now, querier.rowArgs[0])
	}
	if querier.rowArgs[1] != "白名单为空" {
		t.Fatalf("第二个参数应是原因原文，实际 %v", querier.rowArgs[1])
	}
}

// TestStartBlockedRunKeepsEmptyReasonVerbatim 钉住空串口径：仓储不 trim、不 NULLIF，
// 调用方传 "" 就原样落 ""（空串不是 NULL）。非空原因由调用方（task 7 的 runner）保证，
// 仓储替它兜底的话，「忘了传」与「真没有原因」在库里就分不开了。
func TestStartBlockedRunKeepsEmptyReasonVerbatim(t *testing.T) {
	now := runStatusTestNow()
	querier := &runStatusQuerierStub{row: &runStatusRowStub{values: []any{int64(32), now}}}

	if _, _, err := NewRunStatusRepo(querier).StartBlockedRun(context.Background(), "", now); err != nil {
		t.Fatalf("创建阻止轮次失败: %v", err)
	}
	if len(querier.rowArgs) != 2 || querier.rowArgs[1] != "" {
		t.Fatalf("空串原因应原样传给 SQL，实际参数 %+v", querier.rowArgs)
	}
	if strings.Contains(querier.rowSQL, "NULLIF") {
		t.Fatalf("blocked 建轮不应把空串折成 NULL，实际 SQL %q", querier.rowSQL)
	}
}

// TestStartBlockedRunWrapsError 覆盖 blocked 建轮失败：包 ErrRunStoreFailed，
// 且不返回半套编号与时刻。
func TestStartBlockedRunWrapsError(t *testing.T) {
	querier := &runStatusQuerierStub{row: &runStatusRowStub{err: errors.New("连接中断")}}

	runID, startedAt, err := NewRunStatusRepo(querier).StartBlockedRun(
		context.Background(), "白名单为空", runStatusTestNow())
	if err == nil {
		t.Fatal("建轮失败时应返回错误")
	}
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包 ErrRunStoreFailed，实际 %v", err)
	}
	if runID != 0 || !startedAt.IsZero() {
		t.Fatalf("失败时不应返回编号与时刻，实际 %d / %s", runID, startedAt)
	}
}
