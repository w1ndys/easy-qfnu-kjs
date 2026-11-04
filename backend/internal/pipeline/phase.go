package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// Phase 是一轮采集的阶段，取值必须与 collect_run.phase 的 CHECK 约束一致
// （登录 → 学期周历 → 周矩阵 → 占用说明 → 校验 → 发布 → 结束）。
//
// 阶段只表示进度，不表示结果：轮次是否失败由 status 表达，阶段停在失败发生的
// 那一格直到收口写 finished。计划里的「planning/fetching/cleaning/publishing」
// 是口语描述，落库只认这七个值，否则会撞上表上的 CHECK。
type Phase string

// 一轮采集的全部合法阶段，顺序即推进顺序。
const (
	PhaseLogin      Phase = "login"       // 登录：建轮时由表默认值写入
	PhaseCalendar   Phase = "calendar"    // 学期与周历：取学期编号、周次与总周数
	PhaseWeekMatrix Phase = "week_matrix" // 周矩阵：逐楼逐周取矩阵
	PhaseCellDetail Phase = "cell_detail" // 占用说明：对不可用格下钻明细
	PhaseValidate   Phase = "validate"    // 校验：组装候选集并做发布前校验
	PhasePublish    Phase = "publish"     // 发布：单事务版本切换
	PhaseFinished   Phase = "finished"    // 结束：轮次已收口，只能由收口写入
)

// declaredPhases 是全部合法阶段，按推进顺序排列。
// 阶段顺序只在这里维护一份：valid、phaseOrder、phasePredecessors 都由它派生，
// 测试再拿它和 migrations/0001_baseline.sql 的 CHECK 列表逐项比对（minor 12）。
var declaredPhases = []Phase{
	PhaseLogin,
	PhaseCalendar,
	PhaseWeekMatrix,
	PhaseCellDetail,
	PhaseValidate,
	PhasePublish,
	PhaseFinished,
}

// 写进 collect_run.error_code 的业务错误码，取值来自本 spec 与旧 spec design 的
// 「Error Handling」错误分层表。轮次级只记第一个结构失败码，逐条原因留在
// sync_task.error_message 与 collect_run_week.error_message 里。
//
// 非结构失败（网络、超时、上游非 200）没有对应的业务错误码，不写这一列：
// 给它们编一个码只会让管理端误判成「页面结构变了」。
const (
	errorCodeCalendarInvalid        = "calendar_invalid"         // 学期/周历内容非法（FailCalendar 专用）
	errorCodeMatrixStructureInvalid = "matrix_structure_invalid" // 数据行结构异常、响应内重复房号
	errorCodeDetailStructureInvalid = "detail_structure_invalid" // 明细页连标题表都没有
	errorCodeAxisChanged            = "axis_changed"             // 表头状态格与节次轴不一致
)

// phaseOrder 返回阶段在推进顺序里的下标；不是合法阶段时返回 -1。
func phaseOrder(p Phase) int {
	for index, candidate := range declaredPhases {
		if candidate == p {
			return index
		}
	}
	return -1
}

// valid 报告阶段是否落在 collect_run.phase 的取值集合内。
// 表上的 CHECK 仍是最后一道；这里先挡一道是为了让调用方拿到「传错了阶段名」
// 而不是一条只有 SQL 细节的库错误。
func (p Phase) valid() bool {
	return phaseOrder(p) >= 0
}

// phaseBefore 报告 current 是否还没推进到 target（target 更靠后）。
// 执行循环用它判断续跑时哪些阶段已经过了：过了就不写，避免把阶段改回去。
func phaseBefore(current, target Phase) bool {
	currentOrder, targetOrder := phaseOrder(current), phaseOrder(target)
	return currentOrder >= 0 && targetOrder >= 0 && currentOrder < targetOrder
}

// phasePredecessors 返回「可以推进到 target」的全部当前阶段：target 及其之前的阶段。
//
// 它交给 store 作为 UPDATE 的 WHERE 条件（phase = ANY($4)），让「只允许前进」
// 在一条语句里完成：当前阶段在列表里才更新，否则一行都不动。同一阶段的重复推进
// 也在列表里，算幂等刷新计数，不算回退。
func phasePredecessors(target Phase) []string {
	order := phaseOrder(target)
	if order < 0 {
		return nil
	}
	allowed := make([]string, 0, order+1)
	for _, phase := range declaredPhases[:order+1] {
		allowed = append(allowed, string(phase))
	}
	return allowed
}

// PhaseStore 是阶段推进、阶段读取、错误码写入与轮次收口需要的最小仓储能力，
// 由 store.RunPhaseRepo 实现。pipeline 只调这五个方法，不接触 pgx、SQL 与
// collect_run 的列细节（依赖方向见 backend/.golangci.yml）。
type PhaseStore interface {
	// SetRunPhase 推进阶段，并按 sync_task 现状刷新轮次计数。
	// allowedFrom 是可接受的当前阶段列表，当前阶段不在其中时一行都不更新。
	SetRunPhase(ctx context.Context, runID int64, phase string, allowedFrom []string, now time.Time) error
	// LoadRunPhase 读当前阶段，供续跑判断「这一格已经过了」与「这一轮已收口」。
	LoadRunPhase(ctx context.Context, runID int64) (string, error)
	// RecordRunErrorCode 写轮次错误码，只保留第一个（后到的结构失败不覆盖先到的）。
	RecordRunErrorCode(ctx context.Context, runID int64, code string, now time.Time) error
	// LoadRunTaskSummary 聚合必需/可选任务的总数、完成数、失败数与已记录的错误码。
	LoadRunTaskSummary(ctx context.Context, runID int64) (store.RunTaskSummary, error)
	// FinishRun 写轮次终态（status、phase=finished、finished_at、错误信息与计数）。
	FinishRun(ctx context.Context, finish store.RunFinish) error
}

// AdvancePhase 把一轮的阶段推进到 next 并落库。
//
// 推进只允许前进：
//   - next 必须是 collect_run.phase 认识的值，未知阶段直接拒绝，不写库；
//   - 终态 PhaseFinished 不能用推进写入：一轮结束只能由 CloseRun / FailCalendar /
//     FailStaleRuns 收口，它们同时写 status 与 finished_at，推进只改进度；
//   - 当前阶段已经超过 next 时拒绝（回退），报错而不是把进度改回去；
//   - 当前阶段恰好等于 next 时按幂等处理，照常落库以刷新计数。
//
// 时钟经 nowIn 换算到业务时区，与其它落库时刻同一口径。
func AdvancePhase(ctx context.Context, phases PhaseStore, runID int64, next Phase, clock Clock) error {
	if phases == nil {
		return errors.New("pipeline: 缺少阶段仓储")
	}
	if next == PhaseFinished {
		return fmt.Errorf("pipeline: 终态 %q 只能由收口写入，不能用阶段推进", next)
	}
	if !next.valid() {
		return fmt.Errorf("pipeline: 未知的采集阶段 %q", next)
	}

	current, err := currentPhase(ctx, phases, runID)
	if err != nil {
		return err
	}
	if phaseOrder(next) < phaseOrder(current) {
		return fmt.Errorf("pipeline: 阶段只能前进，轮次 %d 不能从 %s 回退到 %s", runID, current, next)
	}
	return phases.SetRunPhase(ctx, runID, string(next), phasePredecessors(next), nowIn(clock))
}

// currentPhase 读本轮当前阶段并校验取值：不是合法阶段或已经是终态时直接拒绝。
//
// 终态拒绝放在这里，是为了让「已收口的轮次不能再推进 / 再执行」只有一个口径：
// 收口写的是 status 加 phase=finished，读到 finished 说明这一轮已经结束，
// 任何后续的推进都会把管理端的进度改回中间态。
func currentPhase(ctx context.Context, phases PhaseStore, runID int64) (Phase, error) {
	raw, err := phases.LoadRunPhase(ctx, runID)
	if err != nil {
		return "", err
	}
	current := Phase(strings.TrimSpace(raw))
	if !current.valid() {
		return "", fmt.Errorf("pipeline: 轮次 %d 的阶段 %q 不在 collect_run.phase 的取值集合内", runID, raw)
	}
	if current == PhaseFinished {
		return "", fmt.Errorf("pipeline: 轮次 %d 已收口，不能再推进或执行", runID)
	}
	return current, nil
}

// errorCodeFor 把一条任务失败的错误映射成 collect_run.error_code；没有对应业务码时返回空串。
//
// 映射只认哨兵，不匹配错误文案（errorlint）：parser 的三类结构失败在解析时就分开了，
// 网络/超时/会话失效不属于结构失败，不写具体码。
// 归属裁决的哨兵里只有 errAttributionRows（空房号、响应内重复房号）是响应自身的
// 行结构异常，与 design 里「数据行结构异常、重复房号」同一类，归
// matrix_structure_invalid；晚到楼与缺楼编号是调用顺序/编排问题，不是页面结构失败，
// 不编业务码——编了会让管理端误判成「页面结构变了」。
func errorCodeFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, parser.ErrMatrixStructureInvalid):
		return errorCodeMatrixStructureInvalid
	case errors.Is(err, parser.ErrDetailStructureInvalid):
		return errorCodeDetailStructureInvalid
	case errors.Is(err, parser.ErrAxisInvalid):
		return errorCodeAxisChanged
	case errors.Is(err, errAttributionRows):
		return errorCodeMatrixStructureInvalid
	default:
		return ""
	}
}

// EvaluateRun 读取轮次的任务汇总并给出终态，但不写库。
//
// 它是收口前的决策点：调用方在发布之前用它判断「必需任务是否已失败」——
// 必需失败就跳过发布、直接 CloseRun 收口；没有失败才进入发布。
// 不在这里写库，是因为发布尚未发生：过早写 finished_at 会把「任务跑完」误记成
// 「一轮结束」。收口本身仍由 CloseRun 一次性写状态、阶段、计数与完成时刻。
//
// dropped 是执行循环丢弃、从未落库的明细任务数（RunProgress.Dropped()），
// 与交给 CloseRun 的必须是同一个值：库里没有这批任务的 failed 行，只看汇总会把
// 「明细全部被丢弃」判成 success，进而放出发布。
func EvaluateRun(
	ctx context.Context,
	phases PhaseStore,
	runID int64,
	dropped DroppedTasks,
) (RunStatus, store.RunTaskSummary, error) {
	if phases == nil {
		return "", store.RunTaskSummary{}, errors.New("pipeline: 缺少阶段仓储")
	}
	summary, err := phases.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		return "", store.RunTaskSummary{}, err
	}
	status, err := decideOutcome(summary, dropped)
	if err != nil {
		return "", store.RunTaskSummary{}, err
	}
	return status, summary, nil
}

// CloseRun 收口一轮采集并返回写进库里的终态（本 spec Requirement 8.1–8.2）。
//
// dropped 是执行循环里被归属过滤丢弃、从未写进 sync_task 的明细任务数
// （RunProgress.Dropped()）。它们没有真实执行，但同样是一条没拿到的明细，必须
// 参与收口：可选明细全部被丢弃时轮次是 partial，不能静默收成 success；必需明细
// 被丢弃时是 failed。只读库里的 failed 行会漏掉整批失败，所以这个数必须传进来。
//
// 收口时机由调用方决定：success 与 partial 在发布事务提交之后调用（先发布再收口），
// failed 在发布之前调用（必需任务失败或校验拦截时跳过发布，直接收口保留线上版本）。
// 两种时机都不能提前：任务还没跑完就收口会把「还没做」记成「已完成」。
//
// 语义如下：
//   - 仍有 pending 任务（必需或可选）时拒绝收口：执行循环没跑完就写终态会把
//     「还没做」记成「已完成」，续跑也补不回来；
//   - 有 failed 必需任务，或被丢弃的必需明细任务 → failed：整轮失败、不发布，
//     保留线上版本；
//   - 只有 failed 可选任务或被丢弃的可选明细任务 → partial：可用性数据完整，
//     发布仍可继续；
//   - 都没有 → success。
//
// 终态从库里的 sync_task 现状读出，而不是本次运行的内存计数：续跑时上一次尝试
// 留下的 failed 任务不会再被重试，只看本轮进度会把 failed 轮次收口成 success。
// 失败原因取最早失败任务（task_id 升序第一条）的 error_message；成功时清空错误码与原因。
//
// error_code 与 error_message 可能来自不同的任务，这里不强行对齐：error_code 是
// 任务失败当时记下的第一个结构失败码（failTask 按失败发生的先后写入，只保留第一个），
// error_message 是 task_id 升序最早的那条 failed 任务的原因。两者都如实反映轮次，
// 管理端应把 code 理解为「失败所在的层」、把 message 理解为「最早那条任务的原文」，
// 不要假设它们必然出自同一条任务。
// 只由丢弃产生的失败没有任何 failed 行可引用，改用丢弃计数写一句原因；这类失败
// 不编错误码——归属裁决不是页面结构失败。
//
// error_code 按失败分层写：
//   - failed：原样保留任务失败当时记下的第一个结构失败码（matrix_structure_invalid /
//     axis_changed 等）；非结构失败没有码，这一列留 NULL；
//   - partial：同样保留第一个结构失败码（可选明细任务的结构失败在 failTask 时就写了
//     detail_structure_invalid）；网络、超时这类非结构失败没有码，留 NULL，
//     不硬塞一个 detail_structure_invalid 造成「码说结构失败、原因说超时」的错配；
//   - success：清空。
//
// 学期/周历阶段的失败不走本函数，走 FailCalendar：那一类失败有固定的
// error_code=calendar_invalid，且常常发生在任务表还没写出来的时候。
func CloseRun(
	ctx context.Context,
	phases PhaseStore,
	runID int64,
	dropped DroppedTasks,
	clock Clock,
) (RunStatus, error) {
	status, summary, err := EvaluateRun(ctx, phases, runID, dropped)
	if err != nil {
		return "", err
	}

	finish := store.RunFinish{
		RunID:        runID,
		Status:       string(status),
		ErrorCode:    summary.RecordedErrorCode,
		ErrorMessage: summary.FirstFailureMessage,
		FinishedAt:   nowIn(clock),
	}
	switch status {
	case StatusSuccess:
		// 成功轮次不留错误原因：error_code / error_message 只在失败轮次有值，
		// 一轮全部成功就不该带着上一状态残留的文案。
		finish.ErrorCode = ""
		finish.ErrorMessage = ""
	case StatusPartial:
		// 没有真实失败任务、只有被丢弃的可选明细时，库里没有原因可引用，
		// 用丢弃计数写一句；错误码留空（见函数注释）。
		if summary.OptionalFailed == 0 {
			finish.ErrorMessage = droppedMessage(dropped)
		}
	case StatusFailed:
		// 失败全部来自被丢弃的必需明细、库里没有任何 failed 行时才用丢弃说明；
		// 一旦库里有失败任务（哪怕只是可选明细），保留它记录的码与最早原因，
		// 不要用丢弃说明覆盖，否则 code 与 message 会来自两套口径。
		if summary.RequiredFailed == 0 && summary.OptionalFailed == 0 {
			finish.ErrorMessage = droppedMessage(dropped)
		}
	}
	if err := phases.FinishRun(ctx, finish); err != nil {
		return "", err
	}
	return status, nil
}

// droppedMessage 把归属过滤丢弃的明细任务写成一句可读原因，供收口在没有
// sync_task 失败行可引用时写进 error_message（明细全被丢弃的轮次，库里一条
// failed 任务都没有）。
func droppedMessage(dropped DroppedTasks) string {
	if dropped.Total() == 0 {
		return ""
	}
	return fmt.Sprintf("归属校验丢弃 %d 条明细任务（必需 %d 条、可选 %d 条）",
		dropped.Total(), dropped.Required, dropped.Optional)
}

// decideOutcome 由任务汇总与丢弃数决定终态（本 spec Requirement 8.1–8.2）。
//
// 必需失败优先于可选失败：两者同时存在时轮次仍是 failed，因为缺了必需周矩阵
// 就没有完整可用性，partial 只描述「可用性完整、明细不全」这一种情况。
// 没有必需任务的轮次按成功处理：计划里每栋楼每周必有一条必需任务，出现零必需
// 任务只可能是空计划，候选集是否为空由发布校验（任务 10）另外拒绝。
//
// dropped 在这里当作失败计入：被归属过滤丢弃的明细没有写 sync_task，汇总里看不到
// 它们，不补这一笔就会把「明细全部被丢弃」收口成 success。Plan 目前只把占用说明
// 规划成可选任务，丢弃因此通常落到 optional；仍按必需性分开，是因为调用方可以
// 传入必需明细任务，丢弃必需明细必须让整轮 failed。
func decideOutcome(summary store.RunTaskSummary, dropped DroppedTasks) (RunStatus, error) {
	if pending := summary.RequiredTotal - summary.RequiredDone - summary.RequiredFailed; pending > 0 {
		return "", fmt.Errorf("pipeline: 轮次还有 %d 条必需任务未完成，不能收口", pending)
	}
	if pending := summary.OptionalTotal - summary.OptionalDone - summary.OptionalFailed; pending > 0 {
		return "", fmt.Errorf("pipeline: 轮次还有 %d 条可选任务未完成，不能收口", pending)
	}

	requiredFailed := summary.RequiredFailed + dropped.Required
	optionalFailed := summary.OptionalFailed + dropped.Optional
	switch {
	case requiredFailed > 0:
		return StatusFailed, nil
	case optionalFailed > 0:
		return StatusPartial, nil
	default:
		return StatusSuccess, nil
	}
}

// FailCalendar 把一轮按「学期或周历非法」收口：failed + error_code=calendar_invalid。
//
// 学期/周历是本轮的第一个上游请求，失败时通常还没有任务表可跑，所以它不走
// CloseRun 的任务汇总；直接写终态，保留线上版本（Requirement 8.1、原 spec
// Requirement 3.4）。cause 的原因原样写进 error_message 供排查；传 nil 时
// 写一句通用说明，不把空原因落库。
func FailCalendar(ctx context.Context, phases PhaseStore, runID int64, cause error, clock Clock) error {
	if phases == nil {
		return errors.New("pipeline: 缺少阶段仓储")
	}

	message := "学期或周历内容非法"
	if cause != nil {
		message = cause.Error()
	}
	return phases.FinishRun(ctx, store.RunFinish{
		RunID:        runID,
		Status:       string(StatusFailed),
		ErrorCode:    errorCodeCalendarInvalid,
		ErrorMessage: message,
		FinishedAt:   nowIn(clock),
	})
}
