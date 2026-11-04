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

// runPhaseQuerierStub 记录阶段推进与收口的 SQL 与参数，并按预置返回受影响行数、
// 任务汇总行或轮次状态行。默认返回「影响 1 行」，让正常路径不必逐条预置。
type runPhaseQuerierStub struct {
	execCalls int
	execSQL   string
	execArgs  []any
	execTag   pgconn.CommandTag
	execErr   error

	queryRowCalls int
	queryRowSQL   string
	queryRowArgs  []any
	row           pgx.Row // 任务汇总行
	stateRow      pgx.Row // 0 行更新后的轮次状态行
}

// Exec 实现 runPhaseQuerier。
func (s *runPhaseQuerierStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execCalls++
	s.execSQL = sql
	s.execArgs = args
	if s.execErr != nil {
		return pgconn.CommandTag{}, s.execErr
	}
	if s.execTag.String() != "" {
		return s.execTag, nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// QueryRow 实现 runPhaseQuerier：状态查询返回 stateRow，其余（任务汇总）返回 row。
func (s *runPhaseQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.queryRowCalls++
	s.queryRowSQL = sql
	s.queryRowArgs = args
	if sql == loadRunStateSQL {
		return s.stateRow
	}
	return s.row
}

// runSummaryRowStub 是 pgx.Row 的最小替身，列顺序与 loadRunTaskSummarySQL 一致。
type runSummaryRowStub struct {
	summary RunTaskSummary
	exists  bool
	scanErr error
}

// Scan 实现 pgx.Row。
func (r *runSummaryRowStub) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if len(dest) != 9 {
		return fmt.Errorf("期望 9 个扫描目标，实际 %d 个", len(dest))
	}
	ints := make([]*int, 6)
	for index := range ints {
		value, ok := dest[index].(*int)
		if !ok {
			return fmt.Errorf("第 %d 个目标期望 *int，实际 %T", index+1, dest[index])
		}
		ints[index] = value
	}
	message, ok := dest[6].(*string)
	if !ok {
		return fmt.Errorf("第 7 个目标期望 *string，实际 %T", dest[6])
	}
	errorCode, ok := dest[7].(*string)
	if !ok {
		return fmt.Errorf("第 8 个目标期望 *string，实际 %T", dest[7])
	}
	exists, ok := dest[8].(*bool)
	if !ok {
		return fmt.Errorf("第 9 个目标期望 *bool，实际 %T", dest[8])
	}

	*ints[0] = r.summary.RequiredTotal
	*ints[1] = r.summary.RequiredDone
	*ints[2] = r.summary.RequiredFailed
	*ints[3] = r.summary.OptionalTotal
	*ints[4] = r.summary.OptionalDone
	*ints[5] = r.summary.OptionalFailed
	*message = r.summary.FirstFailureMessage
	*errorCode = r.summary.RecordedErrorCode
	*exists = r.exists
	return nil
}

// runStateRowStub 是 pgx.Row 的最小替身，列顺序与 loadRunStateSQL 一致：
// found=false 时返回 pgx.ErrNoRows，模拟轮次不存在。
type runStateRowStub struct {
	status  string
	phase   string
	found   bool
	scanErr error
}

// Scan 实现 pgx.Row。
func (r *runStateRowStub) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if !r.found {
		return pgx.ErrNoRows
	}
	if len(dest) != 2 {
		return fmt.Errorf("期望 2 个扫描目标，实际 %d 个", len(dest))
	}
	status, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("第 1 个目标期望 *string，实际 %T", dest[0])
	}
	phase, ok := dest[1].(*string)
	if !ok {
		return fmt.Errorf("第 2 个目标期望 *string，实际 %T", dest[1])
	}
	*status = r.status
	*phase = r.phase
	return nil
}

// runPhaseTestNow 是阶段与收口测试使用的固定时刻，测试不读真实时间。
func runPhaseTestNow() time.Time {
	return time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
}

// TestSetRunPhasePassesPhaseTimeAndAllowedFrom 覆盖阶段推进写库：一条 UPDATE，参数是
// 轮次、阶段、时刻与「可接受的当前阶段」；计数由 SQL 按任务现状重算，Go 侧不传。
func TestSetRunPhasePassesPhaseTimeAndAllowedFrom(t *testing.T) {
	now := runPhaseTestNow()
	querier := &runPhaseQuerierStub{}
	allowedFrom := []string{"login", "calendar", "week_matrix"}

	if err := SetRunPhase(context.Background(), querier, 7, " week_matrix ", allowedFrom, now); err != nil {
		t.Fatalf("推进阶段不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQL != setRunPhaseSQL {
		t.Fatalf("应只执行阶段 SQL，实际 %v", querier.execSQL)
	}
	args := querier.execArgs
	if len(args) != 4 {
		t.Fatalf("阶段参数应有 4 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 || args[1].(string) != "week_matrix" || !args[2].(time.Time).Equal(now) {
		t.Fatalf("阶段参数不符: %v", args)
	}
	got, ok := args[3].([]string)
	if !ok || fmt.Sprint(got) != fmt.Sprint(allowedFrom) {
		t.Fatalf("可接受阶段期望 %v，实际 %v", allowedFrom, args[3])
	}
}

// TestSetRunPhaseRejectsEmptyPhase 覆盖空阶段：直接拒绝，不触库。
func TestSetRunPhaseRejectsEmptyPhase(t *testing.T) {
	querier := &runPhaseQuerierStub{}

	err := SetRunPhase(context.Background(), querier, 7, "   ", []string{"login"}, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("空阶段应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if querier.execCalls != 0 {
		t.Fatalf("空阶段不应触库，实际执行 %d 次", querier.execCalls)
	}
}

// TestSetRunPhaseRejectsEmptyAllowedFrom 覆盖缺少可接受阶段：调用方没算阶段顺序时
// 不能放行，否则 UPDATE 的 WHERE 会退化成「任何阶段都能写」。
func TestSetRunPhaseRejectsEmptyAllowedFrom(t *testing.T) {
	querier := &runPhaseQuerierStub{}

	err := SetRunPhase(context.Background(), querier, 7, "week_matrix", nil, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("缺可接受阶段应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if querier.execCalls != 0 {
		t.Fatalf("缺可接受阶段不应触库，实际执行 %d 次", querier.execCalls)
	}
}

// TestSetRunPhaseRejectsMissingRun 覆盖 0 行更新且轮次不存在：报「不存在」，
// 不把落空的写入当成阶段已推进。
func TestSetRunPhaseRejectsMissingRun(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{found: false},
	}

	err := SetRunPhase(context.Background(), querier, 404, "calendar", []string{"login", "calendar"}, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行更新应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestSetRunPhaseRejectsClosedRun 覆盖 minor 5：已收口的轮次不能再推进阶段，
// 0 行更新后补读状态，报「已收口」而不是含混的「不存在」。
func TestSetRunPhaseRejectsClosedRun(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{status: "success", phase: "finished", found: true},
	}

	err := SetRunPhase(context.Background(), querier, 7, "week_matrix", []string{"login", "week_matrix"}, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("已收口应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已收口") {
		t.Fatalf("错误应说明轮次已收口，实际 %v", err)
	}
}

// TestSetRunPhaseRejectsBackwardPhase 覆盖 minor 2：SQL 的 phase = ANY($4) 挡住回退时
// 一行都不更新，补读状态后报「不能回退」，不把阶段改回去。
func TestSetRunPhaseRejectsBackwardPhase(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{status: "running", phase: "cell_detail", found: true},
	}

	err := SetRunPhase(context.Background(), querier, 7, "week_matrix", []string{"login", "week_matrix"}, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("回退应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不能回退") {
		t.Fatalf("错误应说明不能回退，实际 %v", err)
	}
}

// TestSetRunPhaseWrapsStoreFailure 覆盖基础设施错误统一映射成 ErrRunStoreFailed。
func TestSetRunPhaseWrapsStoreFailure(t *testing.T) {
	querier := &runPhaseQuerierStub{execErr: errors.New("连接断开")}

	err := SetRunPhase(context.Background(), querier, 7, "calendar", []string{"login", "calendar"}, runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包装成 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestLoadRunPhaseScansCurrentPhase 覆盖阶段读取：续跑与 0 行更新的判别都靠这一列。
func TestLoadRunPhaseScansCurrentPhase(t *testing.T) {
	querier := &runPhaseQuerierStub{
		stateRow: &runStateRowStub{status: "running", phase: "cell_detail", found: true},
	}

	phase, err := LoadRunPhase(context.Background(), querier, 7)
	if err != nil {
		t.Fatalf("读阶段不应报错，实际 %v", err)
	}
	if phase != "cell_detail" {
		t.Fatalf("阶段期望 cell_detail，实际 %q", phase)
	}
	if querier.queryRowCalls != 1 || querier.queryRowSQL != loadRunStateSQL {
		t.Fatalf("应只执行状态 SQL，实际 %v", querier.queryRowSQL)
	}
	if len(querier.queryRowArgs) != 1 || querier.queryRowArgs[0].(int64) != 7 {
		t.Fatalf("状态参数不符: %v", querier.queryRowArgs)
	}
}

// TestLoadRunPhaseRejectsMissingRun 覆盖轮次不存在：不能把空串当成一个阶段。
func TestLoadRunPhaseRejectsMissingRun(t *testing.T) {
	querier := &runPhaseQuerierStub{stateRow: &runStateRowStub{found: false}}

	_, err := LoadRunPhase(context.Background(), querier, 404)
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestRecordRunErrorCodePassesCodeAndTime 覆盖结构失败写码：一条 UPDATE，参数是轮次、
// 错误码与时刻；「只保留第一个」由 SQL 的 COALESCE 完成，Go 侧不做读改写。
func TestRecordRunErrorCodePassesCodeAndTime(t *testing.T) {
	now := runPhaseTestNow()
	querier := &runPhaseQuerierStub{}

	if err := RecordRunErrorCode(context.Background(), querier, 7, " matrix_structure_invalid ", now); err != nil {
		t.Fatalf("写错误码不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQL != recordRunErrorCodeSQL {
		t.Fatalf("应只执行写码 SQL，实际 %v", querier.execSQL)
	}
	args := querier.execArgs
	if len(args) != 3 {
		t.Fatalf("写码参数应有 3 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 || args[1].(string) != "matrix_structure_invalid" || !args[2].(time.Time).Equal(now) {
		t.Fatalf("写码参数不符: %v", args)
	}
}

// TestRecordRunErrorCodeRejectsClosedRun 覆盖收口后不再补码：0 行更新后补读状态，
// 报「已收口」，不往已结束的轮次上改错误码。
func TestRecordRunErrorCodeRejectsClosedRun(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{status: "failed", phase: "finished", found: true},
	}

	err := RecordRunErrorCode(context.Background(), querier, 7, "matrix_structure_invalid", runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("已收口应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已收口") {
		t.Fatalf("错误应说明轮次已收口，实际 %v", err)
	}
}

// TestRecordRunErrorCodeRejectsMissingRun 覆盖轮次不存在：补读后报「不存在」。
func TestRecordRunErrorCodeRejectsMissingRun(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{found: false},
	}

	err := RecordRunErrorCode(context.Background(), querier, 404, "axis_changed", runPhaseTestNow())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestLoadRunTaskSummaryScansRow 覆盖任务汇总读取：九列按预定顺序扫描，
// 带出必需/可选的总数、完成数、失败数、最早失败原因与已记录的错误码。
func TestLoadRunTaskSummaryScansRow(t *testing.T) {
	want := RunTaskSummary{
		RequiredTotal:       4,
		RequiredDone:        3,
		RequiredFailed:      1,
		OptionalTotal:       2,
		OptionalDone:        1,
		OptionalFailed:      1,
		FirstFailureMessage: "矩阵结构不符合预期",
		RecordedErrorCode:   "matrix_structure_invalid",
	}
	querier := &runPhaseQuerierStub{row: &runSummaryRowStub{summary: want, exists: true}}

	got, err := LoadRunTaskSummary(context.Background(), querier, 7)
	if err != nil {
		t.Fatalf("读任务汇总不应报错，实际 %v", err)
	}
	if got != want {
		t.Fatalf("任务汇总期望 %+v，实际 %+v", want, got)
	}
	if querier.queryRowCalls != 1 || querier.queryRowSQL != loadRunTaskSummarySQL {
		t.Fatalf("应只执行汇总 SQL，实际 %v", querier.queryRowSQL)
	}
	if len(querier.queryRowArgs) != 1 || querier.queryRowArgs[0].(int64) != 7 {
		t.Fatalf("汇总参数不符: %v", querier.queryRowArgs)
	}
}

// TestLoadRunTaskSummaryRejectsMissingRun 覆盖轮次不存在：EXISTS 为 false 时报错，
// 不能把「一轮零任务」收口成全部成功。
func TestLoadRunTaskSummaryRejectsMissingRun(t *testing.T) {
	querier := &runPhaseQuerierStub{row: &runSummaryRowStub{exists: false}}

	_, err := LoadRunTaskSummary(context.Background(), querier, 404)
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestLoadRunTaskSummaryWrapsScanFailure 覆盖扫描失败：错误上抛并归类为仓储失败。
func TestLoadRunTaskSummaryWrapsScanFailure(t *testing.T) {
	querier := &runPhaseQuerierStub{row: &runSummaryRowStub{scanErr: errors.New("列类型不符")}}

	if _, err := LoadRunTaskSummary(context.Background(), querier, 7); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("扫描失败应包装成 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestFinishRunPassesTerminalState 覆盖收口写库：一条 UPDATE，参数是轮次、终态、
// 时刻与错误信息；phase=finished 与计数重算写在 SQL 里，不由 Go 传。
func TestFinishRunPassesTerminalState(t *testing.T) {
	now := runPhaseTestNow()
	querier := &runPhaseQuerierStub{}

	err := FinishRun(context.Background(), querier, RunFinish{
		RunID:        7,
		Status:       "failed",
		ErrorCode:    "calendar_invalid",
		ErrorMessage: "学期页缺少学期编号",
		FinishedAt:   now,
	})
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if querier.execCalls != 1 || querier.execSQL != finishCollectRunSQL {
		t.Fatalf("应只执行收口 SQL，实际 %v", querier.execSQL)
	}
	args := querier.execArgs
	if len(args) != 5 {
		t.Fatalf("收口参数应有 5 个，实际 %d 个", len(args))
	}
	if args[0].(int64) != 7 || args[1].(string) != "failed" || !args[2].(time.Time).Equal(now) {
		t.Fatalf("收口身份或时刻不符: %v", args)
	}
	if args[3].(string) != "calendar_invalid" || args[4].(string) != "学期页缺少学期编号" {
		t.Fatalf("错误码或原因不符: %v", args)
	}
}

// TestFinishRunRejectsIllegalStatus 覆盖终态白名单：空状态与 running 都拒绝，
// 不写一条「已结束却仍是进行中」的轮次。
func TestFinishRunRejectsIllegalStatus(t *testing.T) {
	for _, status := range []string{"", "   ", "running"} {
		t.Run(status, func(t *testing.T) {
			querier := &runPhaseQuerierStub{}
			err := FinishRun(context.Background(), querier, RunFinish{
				RunID: 7, Status: status, FinishedAt: runPhaseTestNow(),
			})
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("终态 %q 应返回 ErrRunStoreFailed，实际 %v", status, err)
			}
			if querier.execCalls != 0 {
				t.Fatalf("非法终态不应触库，实际执行 %d 次", querier.execCalls)
			}
		})
	}
}

// TestFinishRunRejectsMissingFinishTime 覆盖缺少收口时刻：finished_at 是 NOT NULL
// 语义上的收口事实，零值直接拒绝而不是写一个 0001 年。
func TestFinishRunRejectsMissingFinishTime(t *testing.T) {
	querier := &runPhaseQuerierStub{}
	err := FinishRun(context.Background(), querier, RunFinish{RunID: 7, Status: "success"})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("缺收口时刻应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if querier.execCalls != 0 {
		t.Fatalf("缺收口时刻不应触库，实际执行 %d 次", querier.execCalls)
	}
}

// TestFinishRunRejectsMissingRun 覆盖 0 行更新且轮次不存在：报「不存在」。
func TestFinishRunRejectsMissingRun(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{found: false},
	}

	err := FinishRun(context.Background(), querier, RunFinish{
		RunID: 404, Status: "success", FinishedAt: runPhaseTestNow(),
	})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行更新应包装成 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误应说明轮次不存在，实际 %v", err)
	}
}

// TestFinishRunRejectsDuplicateFinish 覆盖 minor 5：同一轮重复收口（迟到重试、
// 并发调用）一行都不更新，报「已收口」，不覆盖第一次的终态与 finished_at。
func TestFinishRunRejectsDuplicateFinish(t *testing.T) {
	querier := &runPhaseQuerierStub{
		execTag:  pgconn.NewCommandTag("UPDATE 0"),
		stateRow: &runStateRowStub{status: "success", phase: "finished", found: true},
	}

	err := FinishRun(context.Background(), querier, RunFinish{
		RunID: 7, Status: "failed", FinishedAt: runPhaseTestNow(),
	})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("重复收口应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不能重复收口") {
		t.Fatalf("错误应说明不能重复收口，实际 %v", err)
	}
}

// TestFinishRunWrapsStoreFailure 覆盖基础设施错误统一映射成 ErrRunStoreFailed。
func TestFinishRunWrapsStoreFailure(t *testing.T) {
	querier := &runPhaseQuerierStub{execErr: errors.New("连接断开")}

	err := FinishRun(context.Background(), querier, RunFinish{
		RunID: 7, Status: "partial", FinishedAt: runPhaseTestNow(),
	})
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应包装成 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestRunPhaseSQLKeepsTerminalInvariants 用字面量锁住四条 SQL 的关键列，
// 防止后续改动把「阶段与计数一起写」「收口固定 finished」「只收 running 的轮次」
// 「错误码只保留第一个」「汇总区分必需/可选」这些口径悄悄改掉。
func TestRunPhaseSQLKeepsTerminalInvariants(t *testing.T) {
	phaseClauses := []string{
		"SET phase = $2",
		"required_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required AND status = 'done')",
		"optional_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required AND status = 'done')",
		// minor 2 / minor 5：只允许前进，且只改还在 running 的轮次。
		"AND status = 'running'",
		"AND phase = ANY($4::text[])",
	}
	for _, clause := range phaseClauses {
		if !strings.Contains(setRunPhaseSQL, clause) {
			t.Fatalf("阶段 SQL 缺少关键列 %q:\n%s", clause, setRunPhaseSQL)
		}
	}
	// 阶段推进不该改 status：状态迁移只发生在收口与过期收敛。
	if strings.Contains(setRunPhaseSQL, "SET status") {
		t.Fatalf("阶段 SQL 不应改写 status:\n%s", setRunPhaseSQL)
	}

	finishClauses := []string{
		"SET status = $2",
		"phase = 'finished'",
		"finished_at = $3",
		"error_code = NULLIF($4, '')",
		"error_message = NULLIF($5, '')",
		// minor 5：一条轮次只能收口一次。
		"AND status = 'running'",
	}
	for _, clause := range finishClauses {
		if !strings.Contains(finishCollectRunSQL, clause) {
			t.Fatalf("收口 SQL 缺少关键列 %q:\n%s", clause, finishCollectRunSQL)
		}
	}

	// 用户拍板 1：结构失败码只在 running 时写，且只保留第一个。
	recordClauses := []string{
		"error_code = COALESCE(error_code, NULLIF($2, ''))",
		"AND status = 'running'",
	}
	for _, clause := range recordClauses {
		if !strings.Contains(recordRunErrorCodeSQL, clause) {
			t.Fatalf("写码 SQL 缺少关键列 %q:\n%s", clause, recordRunErrorCodeSQL)
		}
	}

	summaryClauses := []string{
		"count(*) FILTER (WHERE required AND status = 'failed')",
		"count(*) FILTER (WHERE NOT required AND status = 'failed')",
		// 最早失败任务：按 task_id 升序取第一条。
		"ORDER BY task_id",
		"LIMIT 1",
		"COALESCE((SELECT error_code FROM collect_run WHERE run_id = $1), '')",
		"EXISTS (SELECT 1 FROM collect_run WHERE run_id = $1)",
	}
	for _, clause := range summaryClauses {
		if !strings.Contains(loadRunTaskSummarySQL, clause) {
			t.Fatalf("汇总 SQL 缺少关键列 %q:\n%s", clause, loadRunTaskSummarySQL)
		}
	}
}
