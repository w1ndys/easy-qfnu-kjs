package collector

import (
	"context"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// weekMatrixTaskFetcher 返回一条周矩阵任务的任务抓取闭包（Requirement 6.1）。
//
// 每一步的位置都是固定的：
//  1. 先看 pages 里有没有 (楼,周)：命中直接返回缓存，把原始正文与房号行原样带回。
//     这一步保证的是「同一进程、同一 bank 内，成功页面重放每键至多一份」；第二段 /
//     hydrate 的请求数由任务状态与 hydrate 的缺页筛选保证，不由本缓存单独兜住；
//  2. 未命中才请求上游。上游内部自带 3 次重试，本层不再重试：重试只会把「每轮至多
//     一次」放大成多次；
//  3. parser.Rows 用本轮快照里的轴解表头与数据行；
//  4. 逐格 clean.State 投影成状态，连同原始正文整片写入内存候选。
//
// 失败原样返回，不包装也不改写：pipeline 的 errorCodeFor 按 parser 的三个哨兵映射
// matrix_structure_invalid / axis_changed，并按任务失败处理（Req 6.4）；网络错误
// 保持无业务码。结构失败时什么都不写进 pages——否则重放会把坏页面当成「已抓」，
// 后续任务全部拿到同一份坏结果。于是「结构失败不写缓存」与「同一键再次调用会重新
// 请求」是同一件事的两面：缓存只挡成功结果，挡不住坏页面。本层给出的只是「成功结果
// 每键至多一份」；一轮内至多一次请求还要靠 pipeline 不重试失败任务（failed 保持
// 原状，不回到 pending），否则重放同一键仍会再打一次上游。
func (c *Collector) weekMatrixTaskFetcher(
	term string,
	snapshot store.CollectSnapshot,
	candidates *bank,
) pipeline.TaskFetcher {
	return func(ctx context.Context, task pipeline.Task) (pipeline.TaskFetch, error) {
		if task.Kind != pipeline.TaskWeekMatrix {
			// 两段式执行里这个闭包只喂周矩阵段；混进别的种类说明编排接错了，
			// 不能默默按矩阵去请求（明细参数完全不同，会白打一次上游）。
			return pipeline.TaskFetch{}, fmt.Errorf(
				"collector: 周矩阵抓取器收到 %q 任务", task.Kind)
		}

		key := weekKey{BuildingID: task.BuildingID, Week: task.Week}
		if cached, ok := candidates.pages[key]; ok {
			// 房号行与正文一起返回：pipeline 用前者做归属裁决、用后者算 sha256，
			// 两者必须来自同一次抓取，不能一半缓存一半重抓。
			return pipeline.TaskFetch{Page: cached.Page, Rooms: cached.Rooms}, nil
		}

		body, err := c.upstream.FetchWeekMatrix(ctx, upstream.WeekMatrixParams{
			Term:       term,
			BuildingID: task.BuildingID,
			Week:       task.Week,
		})
		if err != nil {
			return pipeline.TaskFetch{}, err
		}

		rooms, err := parser.Rows(body, snapshot.Axis)
		if err != nil {
			return pipeline.TaskFetch{}, err
		}

		page := candidates.storeMatrix(key, body, rooms, snapshot.Dict)
		return pipeline.TaskFetch{Page: page.Page, Rooms: page.Rooms}, nil
	}
}
