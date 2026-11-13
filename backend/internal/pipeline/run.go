package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// StaleRunTTL 是进行中轮次的最长存活时间：进程崩溃后留下的 running 超过它即判定为
// 过期（原 spec Requirement 11.4、本 spec Requirement 8.3）。取值来自 PRD 4.3 的
// 「超过 2 小时」，不是可以随手调大的重试参数。
const StaleRunTTL = 2 * time.Hour

// shanghai 是业务时区。与 query/stats/upstream 一致用固定 +08:00 而不 LoadLocation：
// 2026 年之后没有夏令时，固定偏移与 Asia/Shanghai 等价，也免去容器缺 tzdata 的分支。
var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// Clock 返回当前时刻；生产传 time.Now，测试注入固定时刻即可，不必 sleep。
// 取到的时刻会在落库前统一换算到业务时区 Asia/Shanghai。
type Clock func() time.Time

// RunStore 是单轮互斥与过期收敛需要的最小仓储能力，由 store.CollectRunRepo 实现。
// pipeline 只调这两个方法，不接触 pgx 与 SQL（依赖方向见 backend/.golangci.yml）。
type RunStore interface {
	// StartCollectRun 建轮时一并写入学期；已有一轮在跑且学期一致才返回该轮次并置
	// reused=true，学期不一致（含旧轮次没写学期）返回 model.ErrRunConflict。
	StartCollectRun(ctx context.Context, term string, startedAt time.Time) (store.CollectRun, bool, error)
	// FailStaleRuns 把 started_at 早于 cutoff 的 running 置为 failed，返回被收敛的轮次编号。
	FailStaleRuns(ctx context.Context, now, cutoff time.Time) ([]int64, error)
}

// RunStart 是一次「开始一轮」的结果。
type RunStart struct {
	Run         store.CollectRun // 本轮在库里的身份；Reused 为 true 时是已有轮次
	Status      RunStatus        // 权威状态枚举（由 Run.Status 转换）；执行阶段只比较 Status，不要回读 Run.Status 的字符串
	Reused      bool             // true 表示已有进行中轮次，本次没有新建
	StaleRunIDs []int64          // 本次开始前被收敛掉的过期轮次
}

// ConvergeStaleRuns 把 started_at 早于 now-StaleRunTTL 的 running 轮次置为失败
// （error_code=stale_run）。服务启动与每轮开始都应先调用它，否则崩溃留下的 running
// 会永久占住唯一索引，之后每轮都只能复用这条死轮次。
//
// 阈值是绝对时长：started_at 是 timestamptz，存的是绝对时刻，now 的时区只影响呈现，
// 不影响「是否超过 2 小时」的判定；这里仍把 now 换算到业务时区，只为落库与日志可读。
func ConvergeStaleRuns(ctx context.Context, runs RunStore, clock Clock) ([]int64, error) {
	return convergeStaleRuns(ctx, runs, nowIn(clock))
}

// StartRun 开始一轮采集（Requirement 8.3、design CP6）：
//   - 先校验学期非空：term 只去两侧空白，缺失时返回包装了 model.ErrRunStoreFailed
//     的可读错误——错误契约与 store 层一致，调用方 errors.Is 一次即可，且不去建一条
//     collect_run.term 为空、后续周结果写不进去的轮次，也不先做无谓的过期收敛；
//   - 再收敛过期轮次，避免崩溃留下的 running 永久锁死；
//   - 最后尝试新建 running；唯一索引冲突时复用已有轮次（Reused=true），但已有轮次的
//     学期与本轮不一致时由 store 返回 model.ErrRunConflict，这里原样上抛。
//
// 学期随建轮一并写入 collect_run.term，后续 RecordWeekFetch 直接从该列取，
// 执行循环不需要再传一次。返回的 Run 状态恒为 running；后续执行、收尾与阶段推进
// 由任务 7/9 负责。
func StartRun(ctx context.Context, runs RunStore, term string, clock Clock) (RunStart, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return RunStart{}, fmt.Errorf("%w: 缺少学期，无法开始采集轮次", model.ErrRunStoreFailed)
	}

	now := nowIn(clock)
	staleRunIDs, err := convergeStaleRuns(ctx, runs, now)
	if err != nil {
		return RunStart{}, err
	}

	run, reused, err := runs.StartCollectRun(ctx, term, now)
	if err != nil {
		return RunStart{}, err
	}
	// 仓储的 Status 是裸 string（它不认识 pipeline 的类型）；在这里转换一次，
	// 让任务 7/9 能直接写 start.Status == StatusRunning，不必再各自转。
	return RunStart{
		Run:         run,
		Status:      RunStatus(run.Status),
		Reused:      reused,
		StaleRunIDs: staleRunIDs,
	}, nil
}

// staleCutoff 返回过期轮次的分界时刻：started_at 早于它才算过期。
// 边界是严格的「<」——恰好 now-StaleRunTTL 的轮次仍算存活，与 store 的
// failStaleRunsSQL 里 `started_at < $2` 是同一个口径。抽成纯函数是为了不连库
// 也能用假时钟锁住这条边界（真实库用例还要 KJS_TEST_DSN）。
func staleCutoff(now time.Time) time.Time {
	return now.Add(-StaleRunTTL)
}

// convergeStaleRuns 用同一个 now 求 cutoff，保证收敛与「开始新一轮」写的是同一时刻。
func convergeStaleRuns(ctx context.Context, runs RunStore, now time.Time) ([]int64, error) {
	return runs.FailStaleRuns(ctx, now, staleCutoff(now))
}

// nowIn 取一次当前时刻并换算到业务时区；clock 为 nil 时退回 time.Now，
// 方便装配层不必显式写一个 time.Now 闭包。
func nowIn(clock Clock) time.Time {
	if clock == nil {
		return time.Now().In(shanghai)
	}
	return clock().In(shanghai)
}

// TermSetter 给已建的 running 轮次补学期。登录前建轮时 term 为 NULL，拿到学期后才调用。
type TermSetter interface {
	SetRunTerm(ctx context.Context, runID int64, term string, now time.Time) error
}

// RecordTerm 只去两侧空白并拒绝空学期，然后落库。
// 空学期（含纯空白）不调用仓储：collect_run.term 有外键，空串会撞约束，
// 也和「登录前 term 留 NULL」的口径冲突——缺学期应保持 NULL，而不是写成空串。
func RecordTerm(ctx context.Context, terms TermSetter, runID int64, term string, clock Clock) error {
	term = strings.TrimSpace(term)
	if term == "" {
		return fmt.Errorf("%w: 缺少学期，无法绑定采集轮次", model.ErrRunStoreFailed)
	}
	return terms.SetRunTerm(ctx, runID, term, nowIn(clock))
}
