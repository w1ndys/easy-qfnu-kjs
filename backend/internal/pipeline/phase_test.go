package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// collectRunPhaseCheckPath 是 collect_run.phase 的 CHECK 定义所在文件。
// go test 的工作目录是包目录（backend/internal/pipeline），所以按相对路径上溯到
// backend/migrations；不手抄一份期望值，migration 改了测试就跟着改。
const collectRunPhaseCheckPath = "../../migrations/0001_baseline.sql"

// collectRunPhaseCheckPattern 从 0001_baseline.sql 里取出 collect_run.phase 的取值列表。
var collectRunPhaseCheckPattern = regexp.MustCompile(
	`phase\s+text NOT NULL DEFAULT 'login' CHECK \(phase IN \(([^)]*)\)\)`)

// quotedPhasePattern 从 CHECK 的取值列表里取出每个带引号的值。
var quotedPhasePattern = regexp.MustCompile(`'([a-z_]+)'`)

// TestPhaseValuesMatchCollectRunCheck 直接读 migrations/0001_baseline.sql 的 CHECK 列表，
// 与 declaredPhases 逐项比对（minor 12）：阶段常量与表定义必须同值同序，
// 测试里不再手抄一份期望值。
func TestPhaseValuesMatchCollectRunCheck(t *testing.T) {
	body, err := os.ReadFile(collectRunPhaseCheckPath)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", collectRunPhaseCheckPath, err)
	}
	matches := collectRunPhaseCheckPattern.FindSubmatch(body)
	if matches == nil {
		t.Fatalf("%s 里找不到 collect_run.phase 的 CHECK 定义", collectRunPhaseCheckPath)
	}
	quoted := quotedPhasePattern.FindAllStringSubmatch(string(matches[1]), -1)
	if len(quoted) == 0 {
		t.Fatalf("collect_run.phase 的 CHECK 列表解析为空: %s", matches[1])
	}
	checked := make([]string, 0, len(quoted))
	for _, match := range quoted {
		checked = append(checked, match[1])
	}

	if len(checked) != len(declaredPhases) {
		t.Fatalf("阶段常量有 %d 个，0001 的 CHECK 有 %d 个: %v", len(declaredPhases), len(checked), checked)
	}
	for index, want := range checked {
		if got := string(declaredPhases[index]); got != want {
			t.Fatalf("第 %d 个阶段期望 %q（0001 的 CHECK），实际 %q", index+1, want, got)
		}
	}
	// 每个常量都必须是合法阶段，且被 constant 顺序覆盖到。
	for _, phase := range declaredPhases {
		if !phase.valid() {
			t.Fatalf("阶段 %q 必须是合法阶段", phase)
		}
	}
	if Phase("planning").valid() {
		t.Fatal("planning 不是 collect_run.phase 的取值，不能当成合法阶段")
	}
	if Phase("").valid() {
		t.Fatal("空阶段不能当成合法阶段")
	}
}

// TestAdvancePhaseWritesPhaseAndClock 覆盖阶段推进：按 runID 写入目标阶段与
// 「可接受的当前阶段」列表，时刻经时钟换算到业务时区，与其它落库时刻同一口径。
func TestAdvancePhaseWritesPhaseAndClock(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{}

	if err := AdvancePhase(context.Background(), phases, 7, PhaseCalendar, fixedClock(now)); err != nil {
		t.Fatalf("推进阶段不应报错，实际 %v", err)
	}
	if len(phases.phases) != 1 {
		t.Fatalf("应写 1 次阶段，实际 %+v", phases.phases)
	}
	call := phases.phases[0]
	if call.runID != 7 || call.phase != string(PhaseCalendar) {
		t.Fatalf("阶段推进入参不符: %+v", call)
	}
	// 只允许前进：推进到 calendar 时，当前阶段只能是 login 或 calendar 本身。
	if want := []string{"login", "calendar"}; fmt.Sprint(call.allowedFrom) != fmt.Sprint(want) {
		t.Fatalf("可接受阶段期望 %v，实际 %v", want, call.allowedFrom)
	}
	if !call.at.Equal(now) {
		t.Fatalf("阶段时刻期望 %s，实际 %s", now, call.at)
	}
	if _, offset := call.at.Zone(); offset != 8*60*60 {
		t.Fatalf("阶段时刻应换算到 +08:00，实际偏移 %d 秒", offset)
	}
}

// TestPhasePredecessorsCoverEveryPhase 覆盖 minor 6：每个可推进阶段的
// allowedFrom 都是「目标阶段及其之前的全部阶段」，表驱动补齐 week_matrix 与
// cell_detail，不再只测 calendar。
func TestPhasePredecessorsCoverEveryPhase(t *testing.T) {
	cases := []struct {
		target Phase
		want   []string
	}{
		{target: PhaseLogin, want: []string{"login"}},
		{target: PhaseCalendar, want: []string{"login", "calendar"}},
		{target: PhaseWeekMatrix, want: []string{"login", "calendar", "week_matrix"}},
		{target: PhaseCellDetail, want: []string{"login", "calendar", "week_matrix", "cell_detail"}},
		{target: PhaseValidate, want: []string{"login", "calendar", "week_matrix", "cell_detail", "validate"}},
		{target: PhasePublish, want: []string{"login", "calendar", "week_matrix", "cell_detail", "validate", "publish"}},
	}
	for _, c := range cases {
		t.Run(string(c.target), func(t *testing.T) {
			phases := &phaseStoreStub{}
			if err := AdvancePhase(context.Background(), phases, 7, c.target, fixedClock(shanghaiMorning())); err != nil {
				t.Fatalf("推进到 %s 不应报错，实际 %v", c.target, err)
			}
			if len(phases.phases) != 1 {
				t.Fatalf("应写 1 次阶段，实际 %+v", phases.phases)
			}
			if got := phases.phases[0].allowedFrom; fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("推进到 %s 的可接受阶段期望 %v，实际 %v", c.target, c.want, got)
			}
		})
	}

	// 未知阶段没有可接受的当前阶段，不能退化成「任何阶段都能写」。
	if got := phasePredecessors(Phase("planning")); got != nil {
		t.Fatalf("未知阶段不应有可接受阶段，实际 %v", got)
	}
}

// TestAdvancePhaseRejectsUnknownPhase 覆盖阶段白名单：未定义的阶段名不写库，
// 避免把坏值交给 collect_run.phase 的 CHECK 约束。
func TestAdvancePhaseRejectsUnknownPhase(t *testing.T) {
	phases := &phaseStoreStub{}

	err := AdvancePhase(context.Background(), phases, 7, Phase("planning"), fixedClock(shanghaiMorning()))
	if err == nil {
		t.Fatal("未知阶段应报错")
	}
	if !strings.Contains(err.Error(), "未知的采集阶段") {
		t.Fatalf("错误应说明阶段未知，实际 %v", err)
	}
	if len(phases.phases) != 0 {
		t.Fatalf("未知阶段不应写库，实际 %+v", phases.phases)
	}
}

// TestAdvancePhaseRejectsFinished 覆盖 minor 3：终态不能靠阶段推进写入，
// 结束只能由 CloseRun / FailCalendar / FailStaleRuns 收口（它们同时写 status 与 finished_at）。
func TestAdvancePhaseRejectsFinished(t *testing.T) {
	phases := &phaseStoreStub{}

	err := AdvancePhase(context.Background(), phases, 7, PhaseFinished, fixedClock(shanghaiMorning()))
	if err == nil {
		t.Fatal("推进到终态应报错")
	}
	if !strings.Contains(err.Error(), "只能由收口写入") {
		t.Fatalf("错误应说明终态只能由收口写入，实际 %v", err)
	}
	if len(phases.phases) != 0 {
		t.Fatalf("终态推进不应写库，实际 %+v", phases.phases)
	}
}

// TestAdvancePhaseRejectsBackwardPhase 覆盖 minor 2 / minor 3：当前阶段已经超过目标时
// 拒绝回退，不写库。
func TestAdvancePhaseRejectsBackwardPhase(t *testing.T) {
	phases := &phaseStoreStub{current: PhaseCellDetail}

	err := AdvancePhase(context.Background(), phases, 7, PhaseWeekMatrix, fixedClock(shanghaiMorning()))
	if err == nil {
		t.Fatal("回退阶段应报错")
	}
	if !strings.Contains(err.Error(), "只能前进") {
		t.Fatalf("错误应说明阶段只能前进，实际 %v", err)
	}
	if len(phases.phases) != 0 {
		t.Fatalf("回退阶段不应写库，实际 %+v", phases.phases)
	}
}

// TestAdvancePhaseAllowsSamePhase 覆盖幂等：重复推进同一阶段算刷新计数，不算回退，
// 续跑才能照常进入已经到达的阶段。
func TestAdvancePhaseAllowsSamePhase(t *testing.T) {
	phases := &phaseStoreStub{current: PhaseCellDetail}

	if err := AdvancePhase(context.Background(), phases, 7, PhaseCellDetail, fixedClock(shanghaiMorning())); err != nil {
		t.Fatalf("重复推进同一阶段不应报错，实际 %v", err)
	}
	if len(phases.phases) != 1 || phases.phases[0].phase != string(PhaseCellDetail) {
		t.Fatalf("同一阶段应照常落库，实际 %+v", phases.phases)
	}
}

// TestAdvancePhaseRejectsClosedRun 覆盖已收口的两种终态：失败收口停在 finished、
// 发布成功停在 publish。判定只看 status（status != running），不看 phase——
// 停在 publish 的发布成功轮次同样是已收口，迟到的推进必须拒绝且不写库。
func TestAdvancePhaseRejectsClosedRun(t *testing.T) {
	cases := []struct {
		name   string
		phase  Phase
		status string
	}{
		{name: "失败收口停在 finished", phase: PhaseFinished, status: "failed"},
		{name: "发布成功停在 publish", phase: PhasePublish, status: store.RunStatusSuccess},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phases := &phaseStoreStub{current: c.phase, status: c.status}

			err := AdvancePhase(context.Background(), phases, 7, PhasePublish, fixedClock(shanghaiMorning()))
			if err == nil {
				t.Fatal("已收口的轮次不应还能推进阶段")
			}
			if !strings.Contains(err.Error(), "已收口") {
				t.Fatalf("错误应说明轮次已收口，实际 %v", err)
			}
			if len(phases.phases) != 0 {
				t.Fatalf("已收口时不应写库，实际 %+v", phases.phases)
			}
		})
	}
}

// TestAdvancePhaseRequiresPhaseStore 覆盖装配错误：没有阶段仓储时立即报错。
func TestAdvancePhaseRequiresPhaseStore(t *testing.T) {
	if err := AdvancePhase(context.Background(), nil, 7, PhaseLogin, fixedClock(shanghaiMorning())); err == nil {
		t.Fatal("缺阶段仓储时应报错")
	}
}

// TestErrorCodeForMapsStructureFailures 覆盖用户拍板 1 的错误分层映射：
// 只认哨兵，结构失败各有具体码，非结构失败没有码。
func TestErrorCodeForMapsStructureFailures(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{name: "矩阵结构失败", cause: fmt.Errorf("解析失败: %w", parser.ErrMatrixStructureInvalid), want: "matrix_structure_invalid"},
		{name: "明细结构失败", cause: fmt.Errorf("解析失败: %w", parser.ErrDetailStructureInvalid), want: "detail_structure_invalid"},
		{name: "轴变化", cause: fmt.Errorf("轴不可用: %w", parser.ErrAxisInvalid), want: "axis_changed"},
		// minor 2：只有响应自身的行结构错误（空房号、响应内重复房号）才归矩阵结构失败。
		{name: "归属行结构错误", cause: fmt.Errorf("响应不可用: %w", errAttributionRows), want: "matrix_structure_invalid"},
		// 晚到楼与缺楼编号是调用顺序/编排问题，不是页面结构失败，不编业务码。
		{name: "归属晚到楼", cause: fmt.Errorf("响应不可用: %w", errAttributionLateArrival), want: ""},
		{name: "归属缺楼编号", cause: fmt.Errorf("响应不可用: %w", errAttributionNoBuilding), want: ""},
		// 父哨兵本身不映射：具体分类由三个子哨兵负责。
		{name: "归属父哨兵", cause: fmt.Errorf("响应不可用: %w", errAttributionData), want: ""},
		{name: "网络失败", cause: errors.New("请求超时"), want: ""},
		{name: "无错误", cause: nil, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := errorCodeFor(c.cause); got != c.want {
				t.Fatalf("错误码期望 %q，实际 %q", c.want, got)
			}
		})
	}
}

// TestCloseRunKeepsEarliestStructuralCode 覆盖用户拍板 1 / minor 6：
// 必需任务失败时，轮次错误码是任务失败当时记下的第一个结构失败码，
// 收口原样保留，不覆盖成别的码。
func TestCloseRunKeepsEarliestStructuralCode(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal:       4,
		RequiredDone:        3,
		RequiredFailed:      1,
		OptionalTotal:       2,
		OptionalDone:        2,
		FirstFailureMessage: "矩阵结构不符合预期",
		RecordedErrorCode:   "matrix_structure_invalid",
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("必需任务失败应收口为 failed，实际 %q", status)
	}
	finish := *phases.finish
	if finish.ErrorCode != "matrix_structure_invalid" {
		t.Fatalf("轮次错误码应保留第一个结构失败码，实际 %q", finish.ErrorCode)
	}
	if finish.ErrorMessage != "矩阵结构不符合预期" {
		t.Fatalf("失败原因应取最早失败任务，实际 %q", finish.ErrorMessage)
	}
}

// TestCloseRunFailsWhenRequiredTaskFailed 覆盖 Requirement 8.1：
// 任一必需任务失败 → 轮次 failed、不发布；收口从库里的任务汇总决策，
// 不是从本次运行的内存计数，续跑时上一轮留下的 failed 同样算数。
func TestCloseRunFailsWhenRequiredTaskFailed(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal:       4,
		RequiredDone:        3,
		RequiredFailed:      1,
		OptionalTotal:       2,
		OptionalDone:        2,
		FirstFailureMessage: "矩阵结构不符合预期",
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("必需任务失败应收口为 failed，实际 %q", status)
	}
	if phases.finish == nil {
		t.Fatal("收口必须写终态")
	}
	finish := *phases.finish
	if finish.RunID != 7 || finish.Status != string(StatusFailed) {
		t.Fatalf("终态入参不符: %+v", finish)
	}
	if finish.ErrorMessage != "矩阵结构不符合预期" {
		t.Fatalf("失败原因应取最早失败任务，实际 %q", finish.ErrorMessage)
	}
	// 没有记录过结构失败码（网络失败）时留空：非结构失败不编业务码。
	if finish.ErrorCode != "" {
		t.Fatalf("非结构失败不应写错误码，实际 %q", finish.ErrorCode)
	}
	if !finish.FinishedAt.Equal(now) {
		t.Fatalf("收口时刻期望 %s，实际 %s", now, finish.FinishedAt)
	}
}

// TestCloseRunMarksPartialForOptionalFailures 覆盖 Requirement 8.2 与用户拍板 2：
// 可选明细失败 → partial，可用性数据完整，仍可发布。失败来源有两种：写进
// sync_task 的可选任务失败，以及被归属过滤丢弃、从未落库的可选明细；两种来源
// 都必须让轮次 partial，只读库里的汇总会把后者误判成 success。
func TestCloseRunMarksPartialForOptionalFailures(t *testing.T) {
	now := shanghaiMorning()
	optionalFailed := store.RunTaskSummary{
		RequiredTotal:       4,
		RequiredDone:        4,
		OptionalTotal:       3,
		OptionalDone:        2,
		OptionalFailed:      1,
		FirstFailureMessage: "明细结构不符合预期",
		RecordedErrorCode:   "detail_structure_invalid",
	}
	cases := []struct {
		name        string
		summary     store.RunTaskSummary
		dropped     DroppedTasks
		wantCode    string
		wantMessage string
	}{
		{
			name:        "可选任务结构失败",
			summary:     optionalFailed,
			wantCode:    "detail_structure_invalid",
			wantMessage: "明细结构不符合预期",
		},
		{
			// 网络失败没有结构失败码：不硬塞 detail_structure_invalid，
			// 免得出现「码说结构失败、原因说超时」。
			name: "可选任务网络失败",
			summary: store.RunTaskSummary{
				RequiredTotal: 4, RequiredDone: 4,
				OptionalTotal: 3, OptionalDone: 2, OptionalFailed: 1,
				FirstFailureMessage: "请求超时",
			},
			wantCode:    "",
			wantMessage: "请求超时",
		},
		{
			name:        "可选明细全被丢弃",
			summary:     store.RunTaskSummary{RequiredTotal: 4, RequiredDone: 4},
			dropped:     DroppedTasks{Optional: 2},
			wantCode:    "",
			wantMessage: "归属校验丢弃 2 条明细任务（必需 0 条、可选 2 条）",
		},
		{
			// 有真实失败任务时原因取它；丢弃数不覆盖最早失败原因。
			name:        "可选失败与丢弃并存",
			summary:     optionalFailed,
			dropped:     DroppedTasks{Optional: 1},
			wantCode:    "detail_structure_invalid",
			wantMessage: "明细结构不符合预期",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phases := &phaseStoreStub{summary: c.summary}
			status, err := CloseRun(context.Background(), phases, 7, c.dropped, fixedClock(now))
			if err != nil {
				t.Fatalf("收口不应报错，实际 %v", err)
			}
			if status != StatusPartial {
				t.Fatalf("只有可选失败时应收口为 partial，实际 %q", status)
			}
			if phases.finish == nil || phases.finish.Status != string(StatusPartial) {
				t.Fatalf("终态入参不符: %+v", phases.finish)
			}
			if phases.finish.ErrorCode != c.wantCode {
				t.Fatalf("partial 的 error_code 期望 %q，实际 %q", c.wantCode, phases.finish.ErrorCode)
			}
			if phases.finish.ErrorMessage != c.wantMessage {
				t.Fatalf("partial 的 error_message 期望 %q，实际 %q", c.wantMessage, phases.finish.ErrorMessage)
			}
		})
	}
}

// TestCloseRunFailsWhenRequiredCellDetailDropped 覆盖「丢弃的是必需任务 → failed」：
// 必需明细被归属过滤丢弃时，库里没有 failed 行，只读汇总会判 success；丢弃数按
// 必需性计入后整轮 failed，原因用丢弃计数补一句，且不编页面结构错误码。
func TestCloseRunFailsWhenRequiredCellDetailDropped(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2,
		RequiredDone:  2,
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{Required: 1}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("必需明细被丢弃应收口为 failed，实际 %q", status)
	}
	if phases.finish == nil || phases.finish.Status != string(StatusFailed) {
		t.Fatalf("终态入参不符: %+v", phases.finish)
	}
	if phases.finish.ErrorCode != "" {
		t.Fatalf("归属过滤不是页面结构失败，不应编错误码，实际 %q", phases.finish.ErrorCode)
	}
	if want := "归属校验丢弃 1 条明细任务（必需 1 条、可选 0 条）"; phases.finish.ErrorMessage != want {
		t.Fatalf("失败原因期望 %q，实际 %q", want, phases.finish.ErrorMessage)
	}
}

// TestCloseRunKeepsCodeAndMessageAsRecorded 覆盖 minor 1 的场景：error_code 是任务
// 失败当时记下的第一个结构失败码，error_message 是最早失败任务（task_id 升序第一条）
// 的原因，两者可能出自不同的任务。收口原样保留、不做「对齐」，并用注释与管理端
// 约定：code 表示失败所在的层，message 表示最早那条任务的原文。
func TestCloseRunKeepsCodeAndMessageAsRecorded(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2, RequiredDone: 1, RequiredFailed: 1,
		// 最早的失败任务是网络超时（没有码），后发生的结构失败写下了
		// matrix_structure_invalid：两者刻意来自不同的任务。
		FirstFailureMessage: "请求超时",
		RecordedErrorCode:   "matrix_structure_invalid",
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("必需任务失败应收口为 failed，实际 %q", status)
	}
	if phases.finish == nil {
		t.Fatal("收口必须写终态")
	}
	if phases.finish.ErrorCode != "matrix_structure_invalid" {
		t.Fatalf("轮次错误码应保留第一个结构失败码，实际 %q", phases.finish.ErrorCode)
	}
	if phases.finish.ErrorMessage != "请求超时" {
		t.Fatalf("失败原因应取最早失败任务，实际 %q", phases.finish.ErrorMessage)
	}
}

// TestCloseRunMarksSuccessWhenAllTasksDone 覆盖全成功：必需与可选全部 done →
// success，且清空错误码与原因，不把上一状态残留的文案带进成功轮次。
func TestCloseRunMarksSuccessWhenAllTasksDone(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 4,
		RequiredDone:  4,
		OptionalTotal: 3,
		OptionalDone:  3,
		// 正常这里应为空；故意留一条文案与错误码，确认成功路径会清掉它们。
		FirstFailureMessage: "历史失败",
		RecordedErrorCode:   "matrix_structure_invalid",
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusSuccess {
		t.Fatalf("全部完成应收口为 success，实际 %q", status)
	}
	if phases.finish == nil || phases.finish.Status != string(StatusSuccess) {
		t.Fatalf("终态入参不符: %+v", phases.finish)
	}
	if phases.finish.ErrorCode != "" || phases.finish.ErrorMessage != "" {
		t.Fatalf("成功轮次不应带错误信息，实际 %+v", *phases.finish)
	}
}

// TestCloseRunRefusesPendingTasks 覆盖收口前置条件：还有 pending 任务时拒绝收口，
// 不把「还没做」写成 success/partial。必需与可选两种 pending 都要挡住。
func TestCloseRunRefusesPendingTasks(t *testing.T) {
	cases := []struct {
		name    string
		summary store.RunTaskSummary
		want    string
	}{
		{name: "必需任务未完成", want: "必需任务未完成", summary: store.RunTaskSummary{
			RequiredTotal: 4, RequiredDone: 3,
		}},
		{name: "可选任务未完成", want: "可选任务未完成", summary: store.RunTaskSummary{
			RequiredTotal: 4, RequiredDone: 4,
			OptionalTotal: 3, OptionalDone: 2,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phases := &phaseStoreStub{summary: c.summary}
			if _, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(shanghaiMorning())); err == nil {
				t.Fatal("还有未完成任务时不应收口")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应说明「%s」，实际 %v", c.want, err)
			}
			if phases.finish != nil {
				t.Fatalf("拒绝收口时不应写终态，实际 %+v", *phases.finish)
			}
		})
	}
}

// TestCloseRunPrefersRequiredFailure 覆盖两种失败同时存在：必需失败优先，
// 轮次仍是 failed；partial 只描述「可用性完整、明细不全」。
func TestCloseRunPrefersRequiredFailure(t *testing.T) {
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2, RequiredDone: 1, RequiredFailed: 1,
		OptionalTotal: 2, OptionalDone: 1, OptionalFailed: 1,
	}}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(shanghaiMorning()))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("必需失败应优先于可选失败，实际 %q", status)
	}
}

// TestCloseRunPropagatesStoreErrors 覆盖仓储错误：读汇总与写终态失败都上抛，
// 不吞成某一种终态。
func TestCloseRunPropagatesStoreErrors(t *testing.T) {
	loadErr := errors.New("读任务汇总失败")
	phases := &phaseStoreStub{loadErr: loadErr}
	if _, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(shanghaiMorning())); !errors.Is(err, loadErr) {
		t.Fatalf("应上抛读汇总错误，实际 %v", err)
	}
	if phases.finish != nil {
		t.Fatalf("读汇总失败时不应写终态，实际 %+v", *phases.finish)
	}

	finishErr := errors.New("写终态失败")
	phases = &phaseStoreStub{finishErr: finishErr}
	if _, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(shanghaiMorning())); !errors.Is(err, finishErr) {
		t.Fatalf("应上抛写终态错误，实际 %v", err)
	}
}

// TestCloseRunLoadsSummaryOnce 覆盖收口的数据来源：终态只从库里的任务汇总读，
// 每个决策只读一次，不靠调用方传入的内存进度。
func TestCloseRunLoadsSummaryOnce(t *testing.T) {
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2,
		RequiredDone:  2,
	}}
	if _, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(shanghaiMorning())); err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	// 汇总来自库里的替身：全部必需任务完成时按 success 收口。
	if phases.finish == nil || phases.finish.Status != string(StatusSuccess) {
		t.Fatalf("必需任务全部完成应按 success 收口，实际 %+v", phases.finish)
	}
}

// TestCloseRunFailsWithoutRequiredTasks 覆盖用户拍板 2：零必需任务不能收成 success。
// 空计划（或没按规划结果执行）的轮次没有任何窗口采集，收口写 failed 并给一句可读原因；
// 不编业务错误码（不是页面结构失败）。
//
// 这里刻意不覆盖 skipped / blocked：Requirement 1.2 的空窗与 Requirement 1.3 的
// 空白名单由规划阶段直接 FinishRun 写终态，不进入 CloseRun（接线约定见 decideOutcome
// 的注释，由任务 12 的端到端检查点落实）。因此 CloseRun 里只应出现「规划过却一条
// 必需任务都没有」这种自相矛盾的轮次。
func TestCloseRunFailsWithoutRequiredTasks(t *testing.T) {
	now := shanghaiMorning()
	phases := &phaseStoreStub{}

	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusFailed {
		t.Fatalf("零必需任务应收口为 failed，实际 %q", status)
	}
	if phases.finish == nil || phases.finish.Status != string(StatusFailed) {
		t.Fatalf("终态入参不符: %+v", phases.finish)
	}
	if phases.finish.ErrorCode != "" {
		t.Fatalf("零必需任务不是结构失败，不应编错误码，实际 %q", phases.finish.ErrorCode)
	}
	if phases.finish.ErrorMessage == "" {
		t.Fatalf("失败轮次必须写一句原因，实际 %+v", *phases.finish)
	}
}

// TestFailCalendarWritesFailedRunWithCalendarInvalid 覆盖任务原文第一句：
// 整轮只抓学期、周历就失败 → failed + error_code=calendar_invalid。
func TestFailCalendarWritesFailedRunWithCalendarInvalid(t *testing.T) {
	now := shanghaiMorning()
	cause := errors.New("学期页缺少学期编号")
	phases := &phaseStoreStub{}

	if err := FailCalendar(context.Background(), phases, 7, cause, fixedClock(now)); err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if phases.finish == nil {
		t.Fatal("学期/周历失败必须写终态")
	}
	finish := *phases.finish
	if finish.RunID != 7 || finish.Status != string(StatusFailed) {
		t.Fatalf("终态入参不符: %+v", finish)
	}
	if finish.ErrorCode != "calendar_invalid" {
		t.Fatalf("error_code 期望 calendar_invalid，实际 %q", finish.ErrorCode)
	}
	if finish.ErrorMessage != cause.Error() {
		t.Fatalf("失败原因应原样落库，期望 %q，实际 %q", cause.Error(), finish.ErrorMessage)
	}
	if !finish.FinishedAt.Equal(now) {
		t.Fatalf("收口时刻期望 %s，实际 %s", now, finish.FinishedAt)
	}
}

// TestFailCalendarWithoutCause 覆盖空原因：仍写一句可读说明，不把空串落库。
func TestFailCalendarWithoutCause(t *testing.T) {
	phases := &phaseStoreStub{}
	if err := FailCalendar(context.Background(), phases, 7, nil, fixedClock(shanghaiMorning())); err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if phases.finish == nil || strings.TrimSpace(phases.finish.ErrorMessage) == "" {
		t.Fatalf("空原因也应写通用说明，实际 %+v", phases.finish)
	}
}

// TestFailCalendarRequiresPhaseStore 覆盖装配错误：没有阶段仓储时立即报错。
func TestFailCalendarRequiresPhaseStore(t *testing.T) {
	if err := FailCalendar(context.Background(), nil, 7, errors.New("坏页面"), fixedClock(shanghaiMorning())); err == nil {
		t.Fatal("缺阶段仓储时应报错")
	}
}

// TestFailRunWritesFailedTerminalState 覆盖任务原文第一句的「正常写」：终态 failed、
// 轮次编号、错误码、cause 原文与收口时刻都交给仓储；phase=finished 与空码折 NULL
// 由 store.FinishRun 的 SQL 负责，不在 Go 侧重写一遍。
func TestFailRunWritesFailedTerminalState(t *testing.T) {
	now := shanghaiMorning()
	cause := errors.New("账号或密码未配置")
	phases := &phaseStoreStub{}

	if err := FailRun(context.Background(), phases, 9, "settings_incomplete", cause, fixedClock(now)); err != nil {
		t.Fatalf("失败收口不应报错，实际 %v", err)
	}
	if phases.finish == nil {
		t.Fatal("失败收口必须写终态")
	}
	finish := *phases.finish
	if finish.RunID != 9 || finish.Status != string(StatusFailed) {
		t.Fatalf("终态入参不符: %+v", finish)
	}
	if finish.ErrorCode != "settings_incomplete" {
		t.Fatalf("error_code 期望 settings_incomplete，实际 %q", finish.ErrorCode)
	}
	if finish.ErrorMessage != cause.Error() {
		t.Fatalf("失败原因应原样落库，期望 %q，实际 %q", cause.Error(), finish.ErrorMessage)
	}
	if !finish.FinishedAt.Equal(now) {
		t.Fatalf("收口时刻期望 %s，实际 %s", now, finish.FinishedAt)
	}
}

// TestFailRunWritesEmptyCodeAsNull 覆盖任务原文的「空串折 NULL」：空串与纯空白都
// 只交给仓储一个空码（store.FinishRun 的 NULLIF 把它写成 NULL），不编一个码，
// 也不让空白字符串混进 collect_run.error_code。
func TestFailRunWritesEmptyCodeAsNull(t *testing.T) {
	cases := []struct {
		name string
		code string
	}{
		{name: "空串", code: ""},
		{name: "纯空白", code: "  \t"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			phases := &phaseStoreStub{}
			err := FailRun(context.Background(), phases, 9, testCase.code, errors.New("发布事务失败"), fixedClock(shanghaiMorning()))
			if err != nil {
				t.Fatalf("无码失败收口不应报错，实际 %v", err)
			}
			if phases.finish == nil {
				t.Fatal("无码失败也必须写终态")
			}
			if phases.finish.ErrorCode != "" {
				t.Fatalf("码 %q 应收成空串交给 NULLIF，实际 %q", testCase.code, phases.finish.ErrorCode)
			}
		})
	}
}

// TestFailRunWritesFallbackMessageWithoutCause 覆盖 cause 为 nil 的固定文案：
// 不把空原因落库；错误码仍照传（只判出失败层、没有原始错误可引用时走这条）。
func TestFailRunWritesFallbackMessageWithoutCause(t *testing.T) {
	phases := &phaseStoreStub{}

	if err := FailRun(context.Background(), phases, 9, "validation_blocked", nil, fixedClock(shanghaiMorning())); err != nil {
		t.Fatalf("失败收口不应报错，实际 %v", err)
	}
	if phases.finish == nil {
		t.Fatal("失败收口必须写终态")
	}
	finish := *phases.finish
	if finish.ErrorMessage != failRunDefaultMessage {
		t.Fatalf("cause 为 nil 应写固定文案 %q，实际 %q", failRunDefaultMessage, finish.ErrorMessage)
	}
	if finish.ErrorCode != "validation_blocked" {
		t.Fatalf("错误码应照传，实际 %q", finish.ErrorCode)
	}
}

// TestFailRunReportsAlreadyFinalizedRun 覆盖任务原文的「已收口轮次报错」：
// 守卫在 store.FinishRun（WHERE status='running'），替身把它返回的错误原样上抛；
// FailRun 必须让调用方看到这个错误，不吞成 nil，也不在失败后补写第二次终态。
func TestFailRunReportsAlreadyFinalizedRun(t *testing.T) {
	closed := fmt.Errorf("%w: 轮次 9 已收口（status=success），不能重复收口", model.ErrRunStoreFailed)
	phases := &phaseStoreStub{finishErr: closed}

	err := FailRun(context.Background(), phases, 9, "auth_failed", errors.New("密码错误"), fixedClock(shanghaiMorning()))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("已收口应返回包装 ErrRunStoreFailed 的错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已收口") {
		t.Fatalf("错误应说明轮次已收口，实际 %v", err)
	}
	if phases.finishCalls != 1 {
		t.Fatalf("收口尝试应只有一次、失败后不重试，实际 %d 次", phases.finishCalls)
	}
}

// TestFailRunRequiresPhaseStore 覆盖装配错误：没有阶段仓储时立即返回错误。
// 守卫必须在调用 FinishRun 之前生效：nil 接口上调用方法会直接 panic，
// 用例能跑到断言且拿到非 nil 错误，就证明仓储一次都没被调用、也没有 panic。
func TestFailRunRequiresPhaseStore(t *testing.T) {
	err := FailRun(context.Background(), nil, 9, "auth_failed", errors.New("密码错误"), fixedClock(shanghaiMorning()))
	if err == nil {
		t.Fatal("缺阶段仓储时应返回错误，且不 panic")
	}
}

// TestRunAdvancesPhasesInOrder 覆盖任务原文第二句：执行循环把阶段从周矩阵推进到
// 占用说明，时刻取注入时钟；阶段与任务落库共用一条事件流（minor 8），
// 断言 week_matrix 在首条矩阵任务之前、cell_detail 在首条明细之前。
func TestRunAdvancesPhasesInOrder(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskPending),
		cellTask(2, "r101"),
	}
	storeStub := &taskStoreStub{persisted: persisted}
	phases := &phaseStoreStub{events: &storeStub.events}
	fetch := &taskFetchStub{
		pages: map[string][]byte{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}):                                  []byte("page-20"),
			taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): []byte("detail"),
		},
		rooms: map[string][]model.MatrixRoom{
			taskKey(Task{Kind: TaskWeekMatrix, BuildingID: "Q10", Week: 20}): {matrixRoom("r101", "F101")},
		},
	}

	if _, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Phases: phases,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(now),
	}); err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}

	want := []string{string(PhaseWeekMatrix), string(PhaseCellDetail)}
	if len(phases.phases) != len(want) {
		t.Fatalf("阶段推进期望 %v，实际 %+v", want, phases.phases)
	}
	for index, phase := range phases.phases {
		if phase.runID != 7 || phase.phase != want[index] {
			t.Fatalf("第 %d 次阶段推进期望 %s，实际 %+v", index+1, want[index], phase)
		}
		if !phase.at.Equal(now) {
			t.Fatalf("第 %d 次阶段时刻期望 %s，实际 %s", index+1, now, phase.at)
		}
	}

	// 阶段与任务落库共用事件流：推进在 Ensure 成功之后、首条任务之前。
	assertEventBefore(t, storeStub.events, "ensure-week", "phase:week_matrix")
	assertEventBefore(t, storeStub.events, "phase:week_matrix", "record:20")
	assertEventBefore(t, storeStub.events, "phase:week_matrix", "complete:1")
	// minor 4：占用说明段同样是「先落库、再推进阶段、然后才执行任务」。
	assertEventBefore(t, storeStub.events, "ensure-cell", "phase:cell_detail")
	assertEventBefore(t, storeStub.events, "phase:cell_detail", "complete:2")
}

// assertEventBefore 断言 phaseEvent 在事件流里出现在 taskEvent 之前（两者都必须存在）。
func assertEventBefore(t *testing.T, events []string, phaseEvent, taskEvent string) {
	t.Helper()

	indexOf := func(event string) int {
		for index, candidate := range events {
			if candidate == event {
				return index
			}
		}
		return -1
	}
	phaseIndex, taskIndex := indexOf(phaseEvent), indexOf(taskEvent)
	if phaseIndex < 0 {
		t.Fatalf("事件流里没有 %q: %v", phaseEvent, events)
	}
	if taskIndex < 0 {
		t.Fatalf("事件流里没有 %q: %v", taskEvent, events)
	}
	if phaseIndex > taskIndex {
		t.Fatalf("%q 应早于 %q，实际事件流 %v", phaseEvent, taskEvent, events)
	}
}

// TestRunDoesNotAdvancePhaseOnBadPlan 覆盖坏计划的前置校验：白名单与计划对不上时
// 直接报错，阶段一次都不推进，避免留下「阶段已到周矩阵却没有任务」的半成品。
func TestRunDoesNotAdvancePhaseOnBadPlan(t *testing.T) {
	phases := &phaseStoreStub{}
	_, err := Run(context.Background(), 7, []string{"Q11"}, []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
	}, RunDeps{
		Tasks:  &taskStoreStub{},
		Rooms:  &roomStoreStub{},
		Phases: phases,
		Fetch:  (&taskFetchStub{}).fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if err == nil {
		t.Fatal("规划楼顺序与白名单不一致时应报错")
	}
	if len(phases.phases) != 0 {
		t.Fatalf("坏计划不应推进阶段，实际 %+v", phases.phases)
	}
}

// TestRunAllCellDetailsDroppedClosesRunPartial 覆盖用户拍板的收口语义：
// 明细任务全部被归属过滤丢弃时，轮次不能静默 success——丢弃计入 RunProgress.Failed，
// 并由调用方把 RunProgress.Dropped() 交给 CloseRun，收口判 partial（丢弃的是可选明细）。
// 丢弃任务没有写 sync_task，所以库里的汇总里看不到它们，这正是必须显式传数的原因。
func TestRunAllCellDetailsDroppedClosesRunPartial(t *testing.T) {
	now := shanghaiMorning()
	persisted := []store.SyncTask{
		weekTask(1, "Q10", 20, store.SyncTaskDone),
		weekTask(2, "Q10", 21, store.SyncTaskDone),
	}
	planned := []Task{
		{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"},
		{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r102", Weekday: 2, Block: "0304"},
	}
	storeStub := &taskStoreStub{persisted: persisted}
	fetch := &taskFetchStub{}

	progress, err := Run(context.Background(), 7, []string{"Q10"}, planned, RunDeps{
		Phases: &phaseStoreStub{},
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{}, // 本轮没有任何归属，两条明细都被丢弃
		Fetch:  fetch.fetch,
		Clock:  fixedClock(now),
	})
	if err != nil {
		t.Fatalf("执行不应报错，实际 %v", err)
	}
	if progress.Failed != 2 || progress.Dropped() != (DroppedTasks{Optional: 2}) {
		t.Fatalf("两条明细都应计入失败并记进 Dropped，实际 %+v", progress)
	}
	if len(fetch.calls) != 0 {
		t.Fatalf("被丢弃的明细不应执行，实际 %+v", fetch.calls)
	}
	if len(storeStub.cellInputs) != 0 {
		t.Fatalf("被丢弃的明细不应落库，实际 %+v", storeStub.cellInputs)
	}

	// 丢弃任务不在 sync_task 里：库里的汇总只有两条必需任务，且都已完成。
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2,
		RequiredDone:  2,
	}}
	status, err := CloseRun(context.Background(), phases, 7, progress.Dropped(), fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusPartial {
		t.Fatalf("全部明细被丢弃应收口为 partial，实际 %q", status)
	}
	if phases.finish == nil || phases.finish.Status != string(StatusPartial) {
		t.Fatalf("终态入参不符: %+v", phases.finish)
	}
	if phases.finish.ErrorCode != "" {
		t.Fatalf("归属过滤不是页面结构失败，不应编错误码，实际 %q", phases.finish.ErrorCode)
	}
	if want := "归属校验丢弃 2 条明细任务（必需 0 条、可选 2 条）"; phases.finish.ErrorMessage != want {
		t.Fatalf("失败原因期望 %q，实际 %q", want, phases.finish.ErrorMessage)
	}
}

// TestPhaseAdvanceStopsRunWhenStoreFails 覆盖阶段写入失败：属于编排错误，立即上抛，
// 不再执行任务。推进在 Ensure 之后，所以任务清单已经落库（续跑会读回），
// 但一条都不执行。
func TestPhaseAdvanceStopsRunWhenStoreFails(t *testing.T) {
	phaseErr := errors.New("推进阶段失败")
	phases := &phaseStoreStub{phaseErr: phaseErr}
	storeStub := &taskStoreStub{}
	fetch := &taskFetchStub{}
	_, err := Run(context.Background(), 7, []string{"Q10"}, []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
	}, RunDeps{
		Tasks:  storeStub,
		Rooms:  &roomStoreStub{},
		Phases: phases,
		Fetch:  fetch.fetch,
		Clock:  fixedClock(shanghaiMorning()),
	})
	if !errors.Is(err, phaseErr) {
		t.Fatalf("应上抛阶段写入错误，实际 %v", err)
	}
	if want := []string{"ensure-week"}; fmt.Sprint(storeStub.events) != fmt.Sprint(want) {
		t.Fatalf("阶段写入失败后不应再执行任务，期望 %v，实际 %v", want, storeStub.events)
	}
	if len(fetch.calls) != 0 {
		t.Fatalf("阶段写入失败后不应发起请求，实际 %+v", fetch.calls)
	}
}

// TestRunResumeDoesNotRegressPhase 覆盖 minor 2：续跑时轮次已经过了某一段，
// 执行循环不再把阶段写回去；只有还没到的阶段才推进。
func TestRunResumeDoesNotRegressPhase(t *testing.T) {
	cases := []struct {
		name       string
		current    Phase
		wantPhases []string
	}{
		// 已到占用说明：两段都不能回退，一次也不写。
		{name: "已到占用说明", current: PhaseCellDetail, wantPhases: nil},
		// 停在周矩阵：跳过周矩阵，只往前推进到占用说明。
		{name: "停在周矩阵", current: PhaseWeekMatrix, wantPhases: []string{string(PhaseCellDetail)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			persisted := []store.SyncTask{
				weekTask(1, "Q10", 20, store.SyncTaskDone),
				cellTask(2, "r101"),
			}
			storeStub := &taskStoreStub{persisted: persisted}
			phases := &phaseStoreStub{current: c.current}
			fetch := &taskFetchStub{pages: map[string][]byte{
				taskKey(Task{Kind: TaskCellDetail, BuildingID: "Q10", RoomID: "r101", Weekday: 1, Block: "0102"}): []byte("detail"),
			}}

			if _, err := Run(context.Background(), 7, []string{"Q10"}, nil, RunDeps{
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Phases: phases,
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			}); err != nil {
				t.Fatalf("续跑不应报错，实际 %v", err)
			}

			got := make([]string, 0, len(phases.phases))
			for _, call := range phases.phases {
				got = append(got, call.phase)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.wantPhases) {
				t.Fatalf("续跑推进期望 %v，实际 %v", c.wantPhases, got)
			}
		})
	}
}

// TestRunRejectsClosedRun 覆盖已收口判定：终态由收口或发布事务写，执行循环读到
// status != running 直接报错，不落任务、不发请求。阶段不一定是 finished——
// 发布成功/部分成功的轮次停在 publish，只有状态能说明这一轮已经结束。
func TestRunRejectsClosedRun(t *testing.T) {
	cases := []struct {
		name   string
		phase  Phase
		status string
	}{
		{name: "失败收口停在 finished", phase: PhaseFinished, status: "failed"},
		{name: "发布成功停在 publish", phase: PhasePublish, status: store.RunStatusSuccess},
		{name: "发布部分成功停在 publish", phase: PhasePublish, status: store.RunStatusPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storeStub := &taskStoreStub{}
			phases := &phaseStoreStub{current: c.phase, status: c.status}
			fetch := &taskFetchStub{}

			_, err := Run(context.Background(), 7, []string{"Q10"}, []Task{
				{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 20},
			}, RunDeps{
				Tasks:  storeStub,
				Rooms:  &roomStoreStub{},
				Phases: phases,
				Fetch:  fetch.fetch,
				Clock:  fixedClock(shanghaiMorning()),
			})
			if err == nil {
				t.Fatal("已收口的轮次不应还能执行")
			}
			if !strings.Contains(err.Error(), "已收口") {
				t.Fatalf("错误应说明轮次已收口，实际 %v", err)
			}
			if len(storeStub.events) != 0 || len(fetch.calls) != 0 {
				t.Fatalf("已收口时不应落库或发请求: events=%v calls=%v", storeStub.events, fetch.calls)
			}
		})
	}
}

// TestEvaluateRunDecidesWithoutWriting 覆盖发布前的决策点：给出终态与任务汇总，
// 但不写阶段、不写终态，调用方据此决定「必需失败就跳过发布」。
func TestEvaluateRunDecidesWithoutWriting(t *testing.T) {
	cases := []struct {
		name    string
		summary store.RunTaskSummary
		dropped DroppedTasks
		want    RunStatus
	}{
		{name: "零必需任务", want: StatusFailed, summary: store.RunTaskSummary{}},
		{name: "必需失败", want: StatusFailed, summary: store.RunTaskSummary{
			RequiredTotal: 2, RequiredDone: 1, RequiredFailed: 1,
			FirstFailureMessage: "矩阵结构不符合预期",
		}},
		{name: "只有可选失败", want: StatusPartial, summary: store.RunTaskSummary{
			RequiredTotal: 2, RequiredDone: 2,
			OptionalTotal: 1, OptionalFailed: 1,
		}},
		{name: "只有可选明细被丢弃", want: StatusPartial, dropped: DroppedTasks{Optional: 1},
			summary: store.RunTaskSummary{RequiredTotal: 2, RequiredDone: 2}},
		{name: "必需明细被丢弃", want: StatusFailed, dropped: DroppedTasks{Required: 1},
			summary: store.RunTaskSummary{RequiredTotal: 2, RequiredDone: 2}},
		{name: "全部完成", want: StatusSuccess, summary: store.RunTaskSummary{
			RequiredTotal: 2, RequiredDone: 2,
			OptionalTotal: 1, OptionalDone: 1,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			phases := &phaseStoreStub{summary: c.summary}
			status, summary, err := EvaluateRun(context.Background(), phases, 7, c.dropped)
			if err != nil {
				t.Fatalf("判定终态不应报错，实际 %v", err)
			}
			if status != c.want {
				t.Fatalf("终态期望 %q，实际 %q", c.want, status)
			}
			if summary != c.summary {
				t.Fatalf("任务汇总期望 %+v，实际 %+v", c.summary, summary)
			}
			// 决策点只读不写：阶段与终态都不应被动过。
			if len(phases.phases) != 0 || phases.finish != nil {
				t.Fatalf("判定终态不应写库: phases=%+v finish=%+v", phases.phases, phases.finish)
			}
		})
	}

	if _, _, err := EvaluateRun(context.Background(), nil, 7, DroppedTasks{}); err == nil {
		t.Fatal("缺阶段仓储时应报错")
	}
}

// TestAdvancePhaseCoversFullLifecycle 覆盖任务原文的阶段推进顺序：
// 登录 → 学期周历 → 周矩阵 → 占用说明 → 校验 → 发布；结束不在推进范围内，
// 只能由收口写（minor 3）。
func TestAdvancePhaseCoversFullLifecycle(t *testing.T) {
	now := shanghaiMorning()
	// 收口要求有且全部完成必需任务：这里给两条已完成任务，只验证阶段推进。
	phases := &phaseStoreStub{summary: store.RunTaskSummary{
		RequiredTotal: 2,
		RequiredDone:  2,
	}}
	steps := []Phase{
		PhaseLogin,
		PhaseCalendar,
		PhaseWeekMatrix,
		PhaseCellDetail,
		PhaseValidate,
		PhasePublish,
	}
	for _, step := range steps {
		if err := AdvancePhase(context.Background(), phases, 7, step, fixedClock(now)); err != nil {
			t.Fatalf("推进到 %s 不应报错，实际 %v", step, err)
		}
	}

	if len(phases.phases) != len(steps) {
		t.Fatalf("应推进 %d 次，实际 %+v", len(steps), phases.phases)
	}
	for index, call := range phases.phases {
		if call.runID != 7 || call.phase != string(steps[index]) {
			t.Fatalf("第 %d 次推进期望 %s，实际 %+v", index+1, steps[index], call)
		}
	}

	// 结束阶段只能由收口写：推进到 finished 被拒，CloseRun 才把它写进终态。
	if err := AdvancePhase(context.Background(), phases, 7, PhaseFinished, fixedClock(now)); err == nil {
		t.Fatal("推进到终态应被拒绝")
	}
	status, err := CloseRun(context.Background(), phases, 7, DroppedTasks{}, fixedClock(now))
	if err != nil {
		t.Fatalf("收口不应报错，实际 %v", err)
	}
	if status != StatusSuccess || phases.finish == nil {
		t.Fatalf("收口应写终态: status=%q finish=%+v", status, phases.finish)
	}
}
