package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// RunFacts 是一轮采集在 collect_run 里供发布组装读取的事实：
// 学期、采集窗口与归属冲突计数。
//
// 字典版本与轴版本不在这里：collect_run 没有这两列，它们由采集编排在清洗时
// 使用的字典/轴快照带进发布组装的入参（见 publish.RunInput）。
// 窗口允许为 NULL（还没定窗口的轮次），读回时折成 0，由 publish.Build 判非法。
type RunFacts struct {
	RunID             int64  // 轮次编号
	Term              string // 学期；轮次未写学期时为空串
	WindowStart       int    // 采集窗口起始周；NULL 读回 0
	WindowEnd         int    // 采集窗口结束周；NULL 读回 0
	AttrConflictCount int    // 本轮归属冲突数，决定版本的 has_attr_conflict 标记
}

// publishQuerier 是发布阶段只读仓储需要的最小能力：它由两个既有窄接口组合而来，
// 归属快照从 room.go 读、任务汇总从 runphase.go 读，本文件只新增 RunFacts 一条查询。
// 组合而不是重写方法声明，是为了让 PublishRepo 能直接把同一个查询器交给既有函数，
// 不复制它们的 SQL 与错误口径。
type publishQuerier interface {
	roomQuerier
	runPhaseQuerier
}

// loadRunFactsSQL 读一轮的学期、窗口与归属冲突计数。
// term/window 允许 NULL：这里折成空串与 0，由调用方判「轮次缺学期 / 没定窗口」，
// 不让扫描 NULL 失败掩盖真正的数据缺口。
const loadRunFactsSQL = `
SELECT run_id,
       COALESCE(term, ''),
       COALESCE(window_start, 0),
       COALESCE(window_end, 0),
       attr_conflict_count
  FROM collect_run
 WHERE run_id = $1`

// LoadRunFacts 读一轮的发布相关事实。轮次不存在时返回包装 ErrRunStoreFailed 的
// 可读错误，不把零值当成「窗口为 0 的合法轮次」。
func LoadRunFacts(ctx context.Context, q publishQuerier, runID int64) (RunFacts, error) {
	var facts RunFacts
	err := q.QueryRow(ctx, loadRunFactsSQL, runID).Scan(
		&facts.RunID, &facts.Term, &facts.WindowStart, &facts.WindowEnd, &facts.AttrConflictCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunFacts{}, fmt.Errorf("%w: 轮次 %d 不存在，无法读取发布事实", model.ErrRunStoreFailed, runID)
	}
	if err != nil {
		return RunFacts{}, fmt.Errorf("%w: 读取轮次 %d 发布事实失败: %w", model.ErrRunStoreFailed, runID, err)
	}
	return facts, nil
}

// PublishRepo 把查询器闭在内部，供 publish 以窄接口消费：
// publish 只看到「轮次事实、归属快照、任务汇总」三个读方法，不接触 pgx、SQL
// 与 collect_run / collect_run_room / sync_task 的列细节（依赖方向见
// backend/.golangci.yml 的 publish 规则）。
type PublishRepo struct {
	q publishQuerier
}

// NewPublishRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewPublishRepo(q publishQuerier) PublishRepo {
	return PublishRepo{q: q}
}

// LoadRunFacts 见同名函数。
func (r PublishRepo) LoadRunFacts(ctx context.Context, runID int64) (RunFacts, error) {
	return LoadRunFacts(ctx, r.q, runID)
}

// LoadRunRooms 读当轮归属快照，见同名函数。
func (r PublishRepo) LoadRunRooms(ctx context.Context, runID int64) ([]RunRoom, error) {
	return LoadRunRooms(ctx, r.q, runID)
}

// LoadRunTaskSummary 聚合当轮必需/可选任务完成情况，见同名函数。
func (r PublishRepo) LoadRunTaskSummary(ctx context.Context, runID int64) (RunTaskSummary, error) {
	return LoadRunTaskSummary(ctx, r.q, runID)
}
