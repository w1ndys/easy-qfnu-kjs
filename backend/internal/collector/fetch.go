package collector

import (
	"context"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// taskFetcher 返回一条同步任务的任务抓取闭包，按 task.Kind 分派到矩阵或明细路径。
//
// 一个抓取器而不是两个，是 pipeline.Run 的形状决定的：它每次调用都会落库并执行
// 库里该段已有的全部同类任务，两段式编排只是各调它一次。第一段虽然只规划周矩阵，
// 但 sync_task 里可能留着上一轮续跑写入、尚未完成的明细任务（EnsureCellDetailTasks
// 会把库里该 kind 的任务原样读回），这些任务本来就该在这一轮正常下钻；若第一段交给
// 一个只认周矩阵的闭包，它们会被判成抓取失败并永久置 failed，续跑再也做不回来
// （Requirement 8.1、10.4）。按 Kind 分派让「哪一段拿到哪种任务」都不再是错误前提：
// 每种已知任务都走它自己的上游与解析路径，只有真正未知的种类才报错。
//
// 两条路径共用同一份快照与候选：快照由 prepare 一次读出（第 10 项的发布还要用两个
// 版本号），候选是本轮唯一的观测内存，两段看到的必须是同一份。
func (c *Collector) taskFetcher(
	term string,
	kbjcmsid string,
	snapshot store.CollectSnapshot,
	candidates *bank,
) pipeline.TaskFetcher {
	return func(ctx context.Context, task pipeline.Task) (pipeline.TaskFetch, error) {
		switch task.Kind {
		case pipeline.TaskWeekMatrix:
			return c.fetchWeekMatrix(ctx, term, snapshot, candidates, task)
		case pipeline.TaskCellDetail:
			return c.fetchCellDetail(ctx, term, kbjcmsid, snapshot, candidates, task)
		default:
			// 未知种类不猜参数：拿矩阵或明细参数去打一次上游只会换来更难排查的报错。
			return pipeline.TaskFetch{}, fmt.Errorf(
				"collector: 未知任务种类 %q", task.Kind)
		}
	}
}

// fetchWeekMatrix 执行一条周矩阵任务（Requirement 6.1）。
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
func (c *Collector) fetchWeekMatrix(
	ctx context.Context,
	term string,
	snapshot store.CollectSnapshot,
	candidates *bank,
	task pipeline.Task,
) (pipeline.TaskFetch, error) {
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

// fetchCellDetail 执行一条占用说明任务（Requirement 7.1–7.4）。
//
// 与周矩阵路径的区别：
//   - 明细没有「楼 × 周」缓存：同一格每轮只规划一条任务（Plan 按 (房,天,大节)
//     去重），pipeline 也不重试失败任务，所以不需要第二层请求守卫；重放由
//     details 的整片覆盖兜住（CP5）；
//   - 明细请求不带楼，但必须带学期与节次模式 kbjcmsid：kbjcmsid 是学期页下发的
//     节次模式 id，每次登录/学期都可能变（upstream/detail.go 的注释），所以它和
//     学期一起由 prepare 从 readCalendar 的同一次学期页解析里持有并传入；
//   - 正文只在本方法内解析、清洗并写进 details，不回传给 pipeline：明细没有周结果、
//     没有页面哈希，原始页面按 CP12 不落库。
//
// 结构失败（parser.ErrDetailStructureInvalid）原样返回，由 pipeline 的 errorCodeFor
// 映射成 detail_structure_invalid 并把任务置 failed；坏页面不写 details，重放时
// 会重新请求，而不是把坏结果当成「已抓」。
//
// 失败原样返回，不包装也不改写：网络/会话失败保持无业务码（非结构失败不编码）。
func (c *Collector) fetchCellDetail(
	ctx context.Context,
	term string,
	kbjcmsid string,
	snapshot store.CollectSnapshot,
	candidates *bank,
	task pipeline.Task,
) (pipeline.TaskFetch, error) {
	body, err := c.upstream.FetchOccupancyDetail(ctx, upstream.OccupancyDetailParams{
		Term:     term,
		KBJCMSID: kbjcmsid,
		RoomID:   task.RoomID,
		Weekday:  task.Weekday,
		BigSlot:  task.Block,
	})
	if err != nil {
		return pipeline.TaskFetch{}, err
	}

	entries, err := parser.Occupancy(body)
	if err != nil {
		return pipeline.TaskFetch{}, err
	}

	candidates.storeDetail(cellKey{
		RoomID:  task.RoomID,
		Weekday: task.Weekday,
		Block:   task.Block,
	}, entries, snapshot.Dict)

	// 明细正文没有页哈希、没有房号行要交给执行循环，两者都留空。
	return pipeline.TaskFetch{}, nil
}
