// Package pipeline 编排一轮采集：先由当前周与总周数求窗口并规划任务，
// 再由后续实现按任务执行、续跑、处理归属冲突与失败分层。
//
// 本文件只做纯规划：不读库、不发请求、不取时钟。窗口、白名单与不可用格
// 全部由调用方传入，所以同一份输入恒得同一份任务清单，可以直接做表驱动与
// 属性测试（依赖方向见 specs/2026-11-03-collect-pipeline/design.md 与
// backend/.golangci.yml）。
package pipeline

// RunStatus 是一轮采集的状态，取值必须与 collect_run.status 的 CHECK 约束一致
// （design：running/success/partial/failed/blocked/skipped）。
// 规划阶段只产出其中三种：有任务时 running、窗口为空时 skipped、白名单为空
// 或窗口跨度非法时 blocked；success/partial/failed 由执行与校验阶段推进。
type RunStatus string

// 一轮采集的全部合法状态。
const (
	StatusRunning RunStatus = "running" // 进行中：本轮已规划出任务，可以开始执行
	StatusSuccess RunStatus = "success" // 成功：必需与可选任务全部完成
	StatusPartial RunStatus = "partial" // 部分完成：仅可选任务失败，仍可发布
	StatusFailed  RunStatus = "failed"  // 失败：必需任务失败或校验拦截，不发布
	StatusBlocked RunStatus = "blocked" // 阻止：白名单为空或窗口非法，不发请求
	StatusSkipped RunStatus = "skipped" // 跳过：采集窗口为空，不发请求
)

// TaskKind 是同步任务的种类，取值必须与 sync_task.kind 的 CHECK 约束一致。
type TaskKind string

// 同步任务的两种种类：周矩阵是必需任务，占用说明是可选任务。
const (
	TaskWeekMatrix TaskKind = "week_matrix" // 每栋白名单楼每一周一条
	TaskCellDetail TaskKind = "cell_detail" // 每个不可用格一条
)

// Task 是一条规划出的采集任务，字段一一对应 sync_task 的列。
// 但结构体零值不等于库里的值：落库时按 Kind 映射 NULL——cell_detail 的
// week 写 NULL（room_id、weekday、block 非空），week_matrix 的 room_id、
// weekday、block 写 NULL（week 非空）；具体映射由任务 7 的执行落库实现，
// 本结构体不承担这一转换。
// 任务只描述「请求什么」，不带上下文与错误：执行阶段据此重建上游参数。
type Task struct {
	Kind       TaskKind // 任务种类
	Required   bool     // 周矩阵必需、占用说明可选；决定失败后轮次走向
	BuildingID string   // 楼编号 jxlbh，两类任务都需要
	Week       int      // 教学周，仅周矩阵任务使用；占用说明任务为 0
	RoomID     string   // 房号 jsbh，仅占用说明任务使用；周矩阵任务为空
	Weekday    int      // 星期 1..7，仅占用说明任务使用；周矩阵任务为 0
	Block      string   // 大节编码，如 0102，仅占用说明任务使用；周矩阵任务为空
}

// Cell 是一个需要下钻占用说明的格子：房号 × 星期 × 大节。
// BuildingID 是该格的候选归属楼；同一格由多栋楼报出时，白名单内的楼按 rank
// 裁决，不在白名单的楼排最后，彼此同 rank 时保留首次出现。
// RoomID 是房号 jsbh。
type Cell struct {
	BuildingID string // 该格归属的教学楼
	RoomID     string // 房号 jsbh
	Weekday    int    // 星期，1=周一 … 7=周日
	Block      string // 大节编码，如 0102
}

// PlanResult 是一次规划的结果：本轮状态与任务清单。
type PlanResult struct {
	Status RunStatus // running 表示可以执行；skipped/blocked 表示本轮就此结束
	Tasks  []Task    // 状态为 skipped 或 blocked 时恒为空
}

// Plan 按采集窗口规划一轮任务（本 spec Requirement 1、原 spec Requirement
// 10.3–10.4、design CP5）：
//
//   - 窗口为空 → skipped，不生成任何任务；
//   - 窗口跨度超过 windowWeeks → blocked，不生成任何任务：窗口本应来自
//     WindowFor，跨度失控说明调用方传了没走校验的窗口，这里挡住 100 周甚至
//     int 上限这类输入，避免超大任务表与内存分配；
//   - 白名单为空 → blocked，不生成任何任务；
//   - 其余情况 → running，任务分两段，必需在前、可选在后：
//     每栋白名单楼每一周一条周矩阵任务（同一楼同一周至多一条），
//     每个不可用格一条占用说明任务（同一格至多一条）。
//
// 白名单顺序即冲突裁决顺序，所以周矩阵任务严格按「白名单顺序 × 周次升序」
// 生成；重复的楼编号只保留首次出现，保证同一楼同一周不会规划出两条任务。
// 不可用格按房号 + 星期 + 大节去重：同一格由多栋楼报出时按白名单 rank 裁决，
// 白名单里靠前的楼优先（不在白名单内的楼排在最后，同 rank 保留首次出现）；
// 任务位置仍按格子首次出现的顺序，方便复现。
func Plan(window Window, buildingIDs []string, unavailable []Cell) PlanResult {
	// 空窗优先于空白名单：两者都不发请求，但空窗是本轮根本没有可查范围，
	// 与配置无关；本 spec Requirement 1 也先判窗口再判白名单。
	if window.Empty() {
		return PlanResult{Status: StatusSkipped}
	}

	// 窗口只应由 WindowFor 产生，跨度恒不超过 windowWeeks。上面已排除
	// Start<1 与 End<Start，所以这里的减法不会溢出，能安全挡住
	// Window{1, 100} 这类越界输入，也避免 Weeks() 按跨度做超大分配。
	if window.End-window.Start >= windowWeeks {
		return PlanResult{Status: StatusBlocked}
	}

	buildings := uniqueBuildingIDs(buildingIDs)
	if len(buildings) == 0 {
		return PlanResult{Status: StatusBlocked}
	}

	// 白名单名次：越小越优先，跨楼同格按它裁决归属。
	ranks := buildingRanks(buildings)
	weeks := window.Weeks()
	tasks := make([]Task, 0, len(buildings)*len(weeks)+len(unavailable))

	// 必需段：每栋楼每周一条，顺序固定，保证断点续跑与冲突裁决可复现。
	for _, buildingID := range buildings {
		for _, week := range weeks {
			tasks = append(tasks, Task{
				Kind:       TaskWeekMatrix,
				Required:   true,
				BuildingID: buildingID,
				Week:       week,
			})
		}
	}

	// 可选段（原 spec Requirement 6.1）：同一格至多一条。格子先按首次出现占位，
	// 后续楼报同一格时只可能用它更靠前的白名单名次替换归属楼，不会再生成第二条任务。
	detailIndex := make(map[cellKey]int, len(unavailable))
	for _, cell := range unavailable {
		key := cellKey{roomID: cell.RoomID, weekday: cell.Weekday, block: cell.Block}
		if index, ok := detailIndex[key]; ok {
			if buildingRank(ranks, cell.BuildingID) < buildingRank(ranks, tasks[index].BuildingID) {
				tasks[index].BuildingID = cell.BuildingID
			}
			continue
		}
		detailIndex[key] = len(tasks)

		tasks = append(tasks, Task{
			Kind:       TaskCellDetail,
			Required:   false,
			BuildingID: cell.BuildingID,
			RoomID:     cell.RoomID,
			Weekday:    cell.Weekday,
			Block:      cell.Block,
		})
	}

	return PlanResult{Status: StatusRunning, Tasks: tasks}
}

// cellKey 是占用说明任务去重用的格子身份：房号 + 星期 + 大节。
// 不含楼：同一个房号格子只下一次钻，归属楼由 Plan 按白名单 rank 裁决。
type cellKey struct {
	roomID  string
	weekday int
	block   string
}

// buildingRanks 把白名单顺序折成名次：名次越小，归属冲突时越优先。
func buildingRanks(buildings []string) map[string]int {
	ranks := make(map[string]int, len(buildings))
	for index, buildingID := range buildings {
		ranks[buildingID] = index
	}

	return ranks
}

// buildingRank 返回楼在白名单中的名次；不在白名单内的楼统一排在最后，
// 它们之间仍按首次出现保留，不会挤掉白名单内的楼。
func buildingRank(ranks map[string]int, buildingID string) int {
	if rank, ok := ranks[buildingID]; ok {
		return rank
	}

	return len(ranks)
}

// uniqueBuildingIDs 按白名单顺序去掉重复楼编号，保留首次出现的项。
// 白名单保存时已校验编号非空且唯一（原 spec Requirement 22.1），这里再兜一层，
// 是为了让「同一楼同一周至多一条」在输入异常时也成立，而不是把重复任务写进库。
func uniqueBuildingIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	return unique
}
