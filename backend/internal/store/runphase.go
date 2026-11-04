package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// RunTaskSummary 是一轮采集的任务完成情况，供收口阶段决定终态。
//
// 只聚合 sync_task 的现状，不掺内存计数：续跑时上一轮尝试留下的 failed 任务
// 同样要算进来，否则「必需失败 → 轮次 failed」在续跑后会被漏判。
// FirstFailureMessage 是按 task_id 升序的第一条 failed 任务（最早失败任务）的原因，
// 写进轮次 error_message 供面板展示；没有失败任务时为空。
//
// RecordedErrorCode 是 collect_run.error_code 的现值：结构失败在任务失败当时
// 就已按「第一个结构失败码」落库（见 pipeline 的 failTask），收口把它原样带回，
// 不再覆盖成别的码；没有记录时为空串。
//
// FirstFailureMessage 与 RecordedErrorCode 可能出自不同的任务：前者按 task_id
// 取最早失败的那条，后者按失败发生的先后只保留第一个结构失败码。管理端不要把
// 两者当成本条任务的 code 与 message，一个描述「最早那条任务」，一个描述
// 「最先发生的结构失败层」。
type RunTaskSummary struct {
	RequiredTotal       int    // 必需任务总数（collect_run.required_total）
	RequiredDone        int    // 必需任务已完成数
	RequiredFailed      int    // 必需任务已失败数
	OptionalTotal       int    // 可选任务总数（collect_run.optional_total）
	OptionalDone        int    // 可选任务已完成数
	OptionalFailed      int    // 可选任务已失败数
	FirstFailureMessage string // 最早失败任务的原因；没有失败任务时为空
	RecordedErrorCode   string // 轮次已记录的错误码；没有记录时为空
}

// RunFinish 是轮次收口的写入参数：终态、错误信息与完成时刻。
// 计数不从 Go 侧传：FinishRun 在 SQL 里按 sync_task 现状重算，避免两个写入者
// 各持一份可能已经过期的数字。
type RunFinish struct {
	RunID        int64     // 轮次编号，collect_run.run_id
	Status       string    // 终态：success、partial、failed、blocked 或 skipped；不能是 running
	ErrorCode    string    // 失败错误码；空串写 NULL
	ErrorMessage string    // 失败原因；空串写 NULL
	FinishedAt   time.Time // 收口时刻，同时写 finished_at 与 updated_at
}

// runPhaseQuerier 只取阶段推进与轮次收口需要的两种能力，便于用替身测试。
type runPhaseQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// setRunPhaseSQL 推进阶段，并顺带按 sync_task 现状刷新四个计数。
//
// 阶段与计数同一条语句落库：任务状态在阶段内被改写，计数若只在收口时重算，
// 停在中间阶段的轮次会一直显示上一阶段的完成数。phase 的取值由表的 CHECK 约束
// 兜底（login、calendar、week_matrix、cell_detail、validate、publish、finished），
// 本函数不重复维护一份白名单。
//
// 两条守卫写在 WHERE 里而不是先读后写：
//   - status = 'running'：收口之后不得再改阶段，终态只由 FinishRun 与过期收敛写；
//   - phase = ANY($4)：$4 是调用方给的「可接受的当前阶段」列表（目标阶段及其之前），
//     当前阶段不在里面就一行都不更新。续跑因此不能把阶段写回去，并发推进也不会
//     让阶段倒退。
//
// 0 行更新时由 SetRunPhase 补读一次状态，把「轮次不存在 / 已收口 / 阶段回退」分开报出来。
const setRunPhaseSQL = `
UPDATE collect_run
   SET phase = $2,
       updated_at = $3,
       required_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required),
       required_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required AND status = 'done'),
       optional_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required),
       optional_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required AND status = 'done')
 WHERE run_id = $1
   AND status = 'running'
   AND phase = ANY($4::text[])`

// loadRunStateSQL 读轮次的状态与阶段。它只服务两个地方：0 行更新时判别失败原因，
// 以及调用方读取当前阶段（LoadRunPhase）。
const loadRunStateSQL = `
SELECT status, phase
  FROM collect_run
 WHERE run_id = $1`

// loadRunTaskSummarySQL 一次聚合出本轮必需/可选任务的总数、完成数、失败数、
// 最早失败原因与轮次已记录的错误码。
//
// 用 count(*) FILTER 而不是多次查询，读到的才是同一时刻的快照。
// FROM sync_task 在没有任何任务时仍返回一行（聚合查询的特性），所以不需要
// 额外补一次 count；轮次是否存在由末尾的 EXISTS 单独判断，避免把「轮次不存在」
// 静默当成「一轮零任务、全部成功」。
//
// error_message 保留最早失败任务：按 task_id 升序（任务创建顺序）取第一条 failed 任务，
// 续跑时上一轮留下的失败任务同样排在本轮新失败之前，不会被后发生的失败覆盖。
const loadRunTaskSummarySQL = `
SELECT count(*) FILTER (WHERE required),
       count(*) FILTER (WHERE required AND status = 'done'),
       count(*) FILTER (WHERE required AND status = 'failed'),
       count(*) FILTER (WHERE NOT required),
       count(*) FILTER (WHERE NOT required AND status = 'done'),
       count(*) FILTER (WHERE NOT required AND status = 'failed'),
       COALESCE((SELECT error_message
                   FROM sync_task
                  WHERE run_id = $1
                    AND status = 'failed'
                  ORDER BY task_id
                  LIMIT 1), ''),
       COALESCE((SELECT error_code FROM collect_run WHERE run_id = $1), ''),
       EXISTS (SELECT 1 FROM collect_run WHERE run_id = $1)
  FROM sync_task
 WHERE run_id = $1`

// finishCollectRunSQL 写轮次终态：status、phase=finished、finished_at、
// error_code/error_message，并按 sync_task 现状重算四个计数。
//
// 计数与状态放在同一条 UPDATE 里，收口后不会出现「failed 却还显示旧完成数」的
// 中间态。error_code/error_message 用 NULLIF 把空串折成 NULL，与 stale_run
// 收敛写入的列语义一致：成功轮次留空而不是留一个空字符串。
// phase 固定写 finished：收口之后没有下一个阶段，即便终态是 failed 也停在结束。
//
// WHERE 带 status = 'running'：一条轮次只能收口一次，重复收口（迟到的重试、
// 并发调用）一行都不更新，由 FinishRun 报错，不会把第一次的终态与 finished_at 盖掉。
const finishCollectRunSQL = `
UPDATE collect_run
   SET status = $2,
       phase = 'finished',
       finished_at = $3,
       updated_at = $3,
       error_code = NULLIF($4, ''),
       error_message = NULLIF($5, ''),
       required_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required),
       required_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND required AND status = 'done'),
       optional_total = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required),
       optional_done  = (SELECT count(*) FROM sync_task WHERE run_id = $1 AND NOT required AND status = 'done')
 WHERE run_id = $1
   AND status = 'running'`

// recordRunErrorCodeSQL 写轮次的错误码，只保留第一个。
//
// COALESCE 让后到的结构失败不覆盖先到的：一轮里可能有多条任务各自撞上结构失败，
// 轮次级只记「第一个结构失败码」，逐条原因仍留在 sync_task.error_message 与
// collect_run_week.error_message 里。空码不写（COALESCE 会保留 NULL，也不会被空串占位）：
// 网络、超时这类非结构失败没有对应的业务错误码，不编一个。
// status = 'running' 保证收口之后不再往已完成的行上补码。
const recordRunErrorCodeSQL = `
UPDATE collect_run
   SET error_code = COALESCE(error_code, NULLIF($2, '')),
       updated_at = $3
 WHERE run_id = $1
   AND status = 'running'`

// runPhaseState 是 0 行更新后补读的最小轮次状态：状态 + 阶段。
type runPhaseState struct {
	Status string // collect_run.status
	Phase  string // collect_run.phase
}

// loadRunState 读轮次的状态与阶段；轮次不存在时返回 found=false 而不是报错，
// 由调用方决定文案（「不存在」与「已收口」是两种不同的失败）。
// 读库本身失败仍按仓储失败上抛。
func loadRunState(ctx context.Context, q runPhaseQuerier, runID int64) (runPhaseState, bool, error) {
	var state runPhaseState
	err := q.QueryRow(ctx, loadRunStateSQL, runID).Scan(&state.Status, &state.Phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return runPhaseState{}, false, nil
	}
	if err != nil {
		return runPhaseState{}, false, fmt.Errorf("%w: 读取轮次 %d 状态失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	return state, true, nil
}

// SetRunPhase 把一轮的阶段推进到 phase，并按任务现状刷新四个计数。
//
// phase 两侧空白先去掉；空阶段直接拒绝，不把坏值交给 CHECK 约束撞出一个
// 只有 SQL 细节的库错误。allowedFrom 是「可接受的当前阶段」：目标阶段及其之前的
// 全部阶段，由 pipeline 按阶段顺序算好传入，store 不重复维护一份顺序。
//
// 0 行更新时补读一次状态，把三种原因分开报：轮次不存在、已收口（status 不再是
// running）、当前阶段已经超过目标（回退）。不静默当成「阶段已推进」。
func SetRunPhase(
	ctx context.Context,
	q runPhaseQuerier,
	runID int64,
	phase string,
	allowedFrom []string,
	now time.Time,
) error {
	phase = strings.TrimSpace(phase)
	if phase == "" {
		return fmt.Errorf("%w: 轮次 %d 的阶段为空", model.ErrRunStoreFailed, runID)
	}
	if len(allowedFrom) == 0 {
		return fmt.Errorf("%w: 轮次 %d 推进到 %s 时缺少可接受的当前阶段", model.ErrRunStoreFailed, runID, phase)
	}

	tag, err := q.Exec(ctx, setRunPhaseSQL, runID, phase, now, allowedFrom)
	if err != nil {
		return fmt.Errorf("%w: 推进轮次阶段失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	state, found, stateErr := loadRunState(ctx, q, runID)
	if stateErr != nil {
		return stateErr
	}
	if !found {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法推进阶段", model.ErrRunStoreFailed, runID)
	}
	if state.Status != "running" {
		return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能再推进阶段",
			model.ErrRunStoreFailed, runID, state.Status)
	}
	return fmt.Errorf("%w: 轮次 %d 的阶段已经是 %s，不能回退到 %s",
		model.ErrRunStoreFailed, runID, state.Phase, phase)
}

// LoadRunPhase 读一轮当前的阶段。终态轮次的 phase 恒为 finished（收口与过期收敛
// 都写这一列），调用方据此判断「这一轮还能不能继续执行」。
// 轮次不存在时返回可读错误，不把空串当成一个阶段。
func LoadRunPhase(ctx context.Context, q runPhaseQuerier, runID int64) (string, error) {
	state, found, err := loadRunState(ctx, q, runID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: 轮次 %d 不存在，无法读取阶段", model.ErrRunStoreFailed, runID)
	}
	return state.Phase, nil
}

// RecordRunErrorCode 把一条结构失败的 error_code 写进轮次，只保留第一个。
//
// 传空码时 SQL 不会改动 error_code（NULLIF 折成 NULL 后 COALESCE 保留原值）：
// 非结构失败没有业务码。已收口的轮次返回错误：收口之后不该再改错误码。
func RecordRunErrorCode(ctx context.Context, q runPhaseQuerier, runID int64, code string, now time.Time) error {
	tag, err := q.Exec(ctx, recordRunErrorCodeSQL, runID, strings.TrimSpace(code), now)
	if err != nil {
		return fmt.Errorf("%w: 写轮次错误码失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	state, found, stateErr := loadRunState(ctx, q, runID)
	if stateErr != nil {
		return stateErr
	}
	if !found {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法写错误码", model.ErrRunStoreFailed, runID)
	}
	return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能再写错误码",
		model.ErrRunStoreFailed, runID, state.Status)
}

// LoadRunTaskSummary 聚合一轮的必需/可选任务完成情况，供收口决定终态。
// 轮次不存在时返回可读错误：汇总全零与「轮次不存在」必须分开，否则一个
// 写错的 run_id 会被收口成「全部成功」。
func LoadRunTaskSummary(ctx context.Context, q runPhaseQuerier, runID int64) (RunTaskSummary, error) {
	var summary RunTaskSummary
	var runExists bool
	err := q.QueryRow(ctx, loadRunTaskSummarySQL, runID).Scan(
		&summary.RequiredTotal,
		&summary.RequiredDone,
		&summary.RequiredFailed,
		&summary.OptionalTotal,
		&summary.OptionalDone,
		&summary.OptionalFailed,
		&summary.FirstFailureMessage,
		&summary.RecordedErrorCode,
		&runExists,
	)
	if err != nil {
		return RunTaskSummary{}, fmt.Errorf("%w: 读取轮次任务汇总失败: %w", model.ErrRunStoreFailed, err)
	}
	if !runExists {
		return RunTaskSummary{}, fmt.Errorf("%w: 轮次 %d 不存在，无法读取任务汇总", model.ErrRunStoreFailed, runID)
	}
	return summary, nil
}

// FinishRun 写一轮的终态（status、phase=finished、finished_at、错误信息与计数）。
//
// 只挡空状态与 running：running 是收口的反义，空状态落库只会撞 CHECK 约束并
// 留下一条没有状态的轮次。其余合法值（success、partial、failed、blocked、
// skipped）原样落库，由调用方按自己的语义选择；本函数不重复维护一份状态白名单。
//
// WHERE 带 status = 'running'：一轮只能收口一次。重复收口（迟到的重试、并发调用）
// 一行都不更新，这里补读状态后报「已收口」，不覆盖第一次写下的终态与 finished_at。
// 轮次不存在时同样补读后报「不存在」。
func FinishRun(ctx context.Context, q runPhaseQuerier, finish RunFinish) error {
	status := strings.TrimSpace(finish.Status)
	if status == "" || status == "running" {
		return fmt.Errorf("%w: 轮次 %d 的收口状态 %q 非法", model.ErrRunStoreFailed, finish.RunID, finish.Status)
	}
	if finish.FinishedAt.IsZero() {
		return fmt.Errorf("%w: 轮次 %d 缺少收口时刻", model.ErrRunStoreFailed, finish.RunID)
	}

	tag, err := q.Exec(ctx, finishCollectRunSQL,
		finish.RunID, status, finish.FinishedAt, finish.ErrorCode, finish.ErrorMessage)
	if err != nil {
		return fmt.Errorf("%w: 轮次收口失败: %w", model.ErrRunStoreFailed, err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	state, found, stateErr := loadRunState(ctx, q, finish.RunID)
	if stateErr != nil {
		return stateErr
	}
	if !found {
		return fmt.Errorf("%w: 轮次 %d 不存在，无法收口", model.ErrRunStoreFailed, finish.RunID)
	}
	return fmt.Errorf("%w: 轮次 %d 已收口（status=%s），不能重复收口",
		model.ErrRunStoreFailed, finish.RunID, state.Status)
}

// RunPhaseRepo 把查询器闭在内部，供 pipeline 以窄接口消费：
// pipeline 只看到阶段推进、阶段读取、错误码写入与收口的方法，不接触 pgx、SQL
// 与 collect_run 的列细节。
type RunPhaseRepo struct {
	q runPhaseQuerier
}

// NewRunPhaseRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewRunPhaseRepo(q runPhaseQuerier) RunPhaseRepo {
	return RunPhaseRepo{q: q}
}

// SetRunPhase 见同名函数。
func (r RunPhaseRepo) SetRunPhase(ctx context.Context, runID int64, phase string, allowedFrom []string, now time.Time) error {
	return SetRunPhase(ctx, r.q, runID, phase, allowedFrom, now)
}

// LoadRunPhase 见同名函数。
func (r RunPhaseRepo) LoadRunPhase(ctx context.Context, runID int64) (string, error) {
	return LoadRunPhase(ctx, r.q, runID)
}

// RecordRunErrorCode 见同名函数。
func (r RunPhaseRepo) RecordRunErrorCode(ctx context.Context, runID int64, code string, now time.Time) error {
	return RecordRunErrorCode(ctx, r.q, runID, code, now)
}

// LoadRunTaskSummary 见同名函数。
func (r RunPhaseRepo) LoadRunTaskSummary(ctx context.Context, runID int64) (RunTaskSummary, error) {
	return LoadRunTaskSummary(ctx, r.q, runID)
}

// FinishRun 见同名函数。
func (r RunPhaseRepo) FinishRun(ctx context.Context, finish RunFinish) error {
	return FinishRun(ctx, r.q, finish)
}
