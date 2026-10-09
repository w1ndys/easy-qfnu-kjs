// 本文件属于 business 层：把候选集展开成观测行与占用明细行，并统计符号计数。
// 展开规则来自 docs/contract/data-format.v2.md 的 observe 层与 derive 层：
// 一间房 × 一周 × 一天 × 一节恰一行；某周有周矩阵时该周只由矩阵决定，
// 没有周矩阵的周才用明细的周次范围派生状态（clean.DeriveStates 负责挑选）。
// 展开是纯计算：不写数据库、不发请求，失败时一行都不会落地。

package publish

import (
	"errors"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrInvalidCandidate 表示候选集与节次轴或周历对不上：展开位置会错位，不能发布。
var ErrInvalidCandidate = errors.New("候选集不可用")

// plan 是展开后的结果：校验与写入都用它，避免同一份数据算两遍。
type plan struct {
	observations   []model.Observation     // 展开后的观测事实
	details        []model.OccupancyDetail // 展开后的占用明细，含派生标记
	rooms          int                     // 本版房间总数（去重）
	unknownCells   int                     // 未知符号的格子数，只计数不阻断发布
	compositeCells int                     // 复合符号的格子数，只计数不阻断发布
}

// axisBlocks 是节次轴的展开视图：大节顺序、每个大节的小节、以及每个大节第一个小节的序号。
// 矩阵的一行按轴的小节顺序排（见 clean.expandDay），所以位置要按轴的下标定位。
type axisBlocks struct {
	order []string            // 大节编码，按轴上的展示顺序
	nodes map[string][]string // 大节 → 该大节的小节编号，按顺序
	first map[string]int      // 大节 → 该大节第一个小节在轴上的下标
}

// buildPlan 把候选集展开成观测与明细；展开失败说明候选集本身有问题，本轮不发布。
func buildPlan(candidate Candidate, releaseID string) (plan, error) {
	blocks, err := newAxisBlocks(candidate.Axis)
	if err != nil {
		return plan{}, err
	}

	// 有周矩阵的周由矩阵说了算，明细的范围不能覆盖它们（设计 Correctness Properties）
	matrixWeeks := matrixWeekNumbers(candidate.Weeks)

	observations := make([]model.Observation, 0, len(candidate.Weeks)*clean.DayCount*clean.NodeCount)
	unknown, composite := 0, 0
	for _, week := range candidate.Weeks {
		rows, weekUnknown, weekComposite, err := observationsOfWeek(candidate, blocks, releaseID, week)
		if err != nil {
			return plan{}, err
		}
		observations = append(observations, rows...)
		unknown += weekUnknown
		composite += weekComposite
	}

	derived, derivedUnknown, derivedComposite, err := observationsOfDetails(candidate, blocks, releaseID, matrixWeeks)
	if err != nil {
		return plan{}, err
	}
	observations = append(observations, derived...)

	return plan{
		observations:   observations,
		details:        buildDetails(candidate, matrixWeeks),
		rooms:          distinctRooms(observations),
		unknownCells:   unknown + derivedUnknown,
		compositeCells: composite + derivedComposite,
	}, nil
}

// observationsOfWeek 把一周的矩阵展开成观测行，并按大节格统计未知与复合。
func observationsOfWeek(candidate Candidate, blocks axisBlocks, releaseID string, week MatrixWeek) ([]model.Observation, int, int, error) {
	// 周次是观测主键的一部分，取不到合法周次就没有可以落行的周
	if week.Week < 1 {
		return nil, 0, 0, fmt.Errorf("%w: 周矩阵的周次是 %d", ErrInvalidCandidate, week.Week)
	}

	rows := make([]model.Observation, 0, len(week.Matrix.Rows)*clean.DayCount*clean.NodeCount)
	unknown, composite := 0, 0
	for _, room := range week.Matrix.Rows {
		for day := 1; day <= clean.DayCount; day++ {
			for _, block := range blocks.order {
				cell := room.Week[day-1][blocks.first[block]]
				// 同一天同一大节展开出的各小节状态相同，未知与复合按大节格各计一次
				if cell.StateKey == model.StateUnknown {
					unknown++
				}
				if cell.StateKey == model.StateComposite {
					composite++
				}
				for _, node := range blocks.nodes[block] {
					rows = append(rows, model.Observation{
						ReleaseID: releaseID,
						RoomID:    room.Jsbh,
						Term:      candidate.Term,
						Week:      week.Week,
						Weekday:   day,
						Node:      node,
						StateKey:  cell.StateKey,
						Available: cell.StateKey.IsUsable(),
						RawText:   cell.RawText,
					})
				}
			}
		}
	}
	return rows, unknown, composite, nil
}

// observationsOfDetails 生成明细带来的派生周观测：只填没有周矩阵的周。
// 派生状态同样落在 observation 里，它的「来自明细」语义由对应明细行的 derived 标记表达
// （observation 没有 derived 列，派生标记按契约只落在 occupancy_detail 上）。
func observationsOfDetails(candidate Candidate, blocks axisBlocks, releaseID string, matrixWeeks []int) ([]model.Observation, int, int, error) {
	rows := make([]model.Observation, 0, len(candidate.Details))
	unknown, composite := 0, 0
	for _, cell := range candidate.Details {
		nodes, ok := blocks.nodes[cell.Block]
		// 明细的大节不在轴上就定位不到小节，这一格展开位置会错，不能发布
		if !ok {
			return nil, 0, 0, fmt.Errorf("%w: 明细的大节 %s 不在节次轴上", ErrInvalidCandidate, cell.Block)
		}
		// 星期越界说明候选集里的格子本身不合法，库里也会被约束拒掉
		if cell.Weekday < 1 || cell.Weekday > clean.DayCount {
			return nil, 0, 0, fmt.Errorf("%w: 格子的星期是 %d", ErrInvalidCandidate, cell.Weekday)
		}

		for _, state := range clean.DeriveStates(cell.Details, candidate.TotalWeeks, matrixWeeks) {
			if state.StateKey == model.StateUnknown {
				unknown++
			}
			if state.StateKey == model.StateComposite {
				composite++
			}
			for _, node := range nodes {
				rows = append(rows, model.Observation{
					ReleaseID: releaseID,
					RoomID:    cell.RoomID,
					Term:      candidate.Term,
					Week:      state.Week,
					Weekday:   cell.Weekday,
					Node:      node,
					StateKey:  state.StateKey,
					Available: state.StateKey.IsUsable(),
				})
			}
		}
	}
	return rows, unknown, composite, nil
}

// buildDetails 把每格的说明展开成明细行。
// derived 标记表示这条说明给「没有周矩阵的周」补了状态，只有这类说明才是派生的来源。
func buildDetails(candidate Candidate, matrixWeeks []int) []model.OccupancyDetail {
	details := make([]model.OccupancyDetail, 0, len(candidate.Details))
	for _, cell := range candidate.Details {
		for _, detail := range cell.Details {
			details = append(details, model.OccupancyDetail{
				RoomID:    cell.RoomID,
				Weekday:   cell.Weekday,
				Block:     cell.Block,
				StateKey:  detail.StateKey,
				Course:    detail.Course,
				WeekRange: detail.WeekRange,
				TimeFlag:  detail.TimeFlag,
				Derived:   clean.DetailUsesDerivedWeeks(detail, candidate.TotalWeeks, matrixWeeks),
			})
		}
	}
	return details
}

// newAxisBlocks 校验节次轴并建立「大节 → 小节」的展开视图。
// 轴必须按展示顺序传入：矩阵的列位置就是轴的下标，顺序不对整份展开都会错位。
func newAxisBlocks(axis []model.AxisNode) (axisBlocks, error) {
	// 没有轴就无法把大节格展开成小节，观测行也就无从落起
	if len(axis) != clean.NodeCount {
		return axisBlocks{}, fmt.Errorf("%w: 节次轴有 %d 个小节，期望 %d", ErrInvalidCandidate, len(axis), clean.NodeCount)
	}

	blocks := axisBlocks{
		order: make([]string, 0, len(axis)),
		nodes: make(map[string][]string, len(axis)),
		first: make(map[string]int, len(axis)),
	}
	for index, node := range axis {
		// 轴按 ordinal 连续排列，乱序说明调用方没按展示顺序传
		if node.Ordinal != index+1 {
			return axisBlocks{}, fmt.Errorf("%w: 节次轴第 %d 个节点的 ordinal 是 %d", ErrInvalidCandidate, index+1, node.Ordinal)
		}
		// 空大节编码落不出块归属，展开时找不到它的小节
		if node.Block == "" {
			return axisBlocks{}, fmt.Errorf("%w: 节次轴的小节 %s 没有大节", ErrInvalidCandidate, node.Node)
		}
		// 同一大节的小节按轴上顺序连着出现，第一次见到时记下大节与它的首个下标
		if _, ok := blocks.first[node.Block]; !ok {
			blocks.first[node.Block] = index
			blocks.order = append(blocks.order, node.Block)
		}
		blocks.nodes[node.Block] = append(blocks.nodes[node.Block], node.Node)
	}
	return blocks, nil
}

// matrixWeekNumbers 返回本轮已经有周矩阵的周次，按出现顺序去重。
// 这些周的空闲判定只由矩阵决定，明细的范围不覆盖它们。
func matrixWeekNumbers(weeks []MatrixWeek) []int {
	seen := make(map[int]bool, len(weeks))
	numbers := make([]int, 0, len(weeks))
	for _, week := range weeks {
		// 同一个周次可能来自多栋楼，只记一次
		if seen[week.Week] {
			continue
		}
		seen[week.Week] = true
		numbers = append(numbers, week.Week)
	}
	return numbers
}

// distinctRooms 统计观测里出现过的房间数：它要与上一版按同一口径比较。
func distinctRooms(observations []model.Observation) int {
	seen := make(map[string]bool, len(observations))
	for _, observation := range observations {
		seen[observation.RoomID] = true
	}
	return len(seen)
}
