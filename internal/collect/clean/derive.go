// 本文件属于 business 层：从聚合矩阵挑出要下钻的格子，并决定哪些周用明细范围派生状态。
// 规则：某周有周矩阵时该周空闲判定只由矩阵决定，明细只作说明；
// 没有矩阵的周才用明细的周次范围派生状态，范围读不出来的周保持未知。
// 依据 specs/collector-full-sync/requirements.md 的 4.1、4.3、4.4、4.5 与设计文档 Correctness Properties。

package clean

import (
	"fmt"
	"sort"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// CellRef 指向一个格子：房间、星期与大节，请求占用明细时用它。
type CellRef struct {
	RoomID  string // 房间身份 jsbh
	Weekday int    // 1=周一 … 7=周日
	Block   string // 大节编码，同时是 kcsj 的后半段
}

// CellState 是某一周某一格的状态：来自周矩阵，或由明细的周次范围派生。
type CellState struct {
	Week     int            // 教学周次
	StateKey model.StateKey // 该周该格的状态键
	Derived  bool           // 是否由明细范围派生，也就是该周没有周矩阵
}

// OccupiedCells 从聚合矩阵里挑出要下钻的格子：只挑判成占用、未知或复合的格子。
// 空闲格没有可说明的占用，下钻只会拿到一张没有数据行的弹窗；同一格只出现一次，不按周重复。
// axis 是当前节次轴（store.AxisNodes），用来把一行里第几个大节对准它的小节。
func OccupiedCells(matrix Matrix, axis []model.AxisNode) ([]CellRef, error) {
	nodeIndex, err := firstNodeByBlock(axis, matrix.Blocks)
	if err != nil {
		return nil, err
	}

	cells := make([]CellRef, 0, len(matrix.Rows))
	for _, row := range matrix.Rows {
		for dayIndex := 0; dayIndex < DayCount; dayIndex++ {
			for _, block := range matrix.Blocks {
				// 同一大节内各小节状态相同，取该大节第一个小节就代表这一格
				if !row.Week[dayIndex][nodeIndex[block]].StateKey.IsUsable() {
					cells = append(cells, CellRef{RoomID: row.Jsbh, Weekday: dayIndex + 1, Block: block})
				}
			}
		}
	}
	return cells, nil
}

// DeriveStates 决定一格在哪些周需要派生状态，按周从小到大返回。
// matrixWeeks 是本轮已经有周矩阵的周次：这些周不出现在结果里，空闲判定仍由矩阵决定。
// 每条说明各按自己的周次范围与时间标志展开；范围读不出来的说明不产出任何周，那些周保持未知。
func DeriveStates(details []Detail, totalWeeks int, matrixWeeks []int) []CellState {
	covered := weekSet(matrixWeeks)

	// 先定下「周 → 状态」：同一周先到的那条说明为准，后面的不覆盖它
	byWeek := make(map[int]model.StateKey, totalWeeks)
	weeks := make([]int, 0, totalWeeks)
	for _, detail := range details {
		rangeWeeks, err := WeekRangeWeeks(detail.WeekRange, detail.TimeFlag, totalWeeks)
		// 范围读不出来时这条说明不派生任何周：那些周保持未知，绝不按空闲处理
		if err != nil {
			continue
		}
		for _, week := range rangeWeeks {
			// 已有周矩阵的周保持矩阵结果，明细不覆盖它
			if covered[week] {
				continue
			}
			// 同一周已经有说明就不再收第二条，避免把一格记成两个状态
			if _, ok := byWeek[week]; ok {
				continue
			}
			byWeek[week] = detail.StateKey
			weeks = append(weeks, week)
		}
	}

	sort.Ints(weeks)
	states := make([]CellState, 0, len(weeks))
	for _, week := range weeks {
		states = append(states, CellState{Week: week, StateKey: byWeek[week], Derived: true})
	}
	return states
}

// DetailUsesDerivedWeeks 判断一条说明是否给「没有周矩阵的周」补派生状态。
// 写 occupancy_detail 时用它决定 derived 标记：只有这类说明才是派生状态的来源。
func DetailUsesDerivedWeeks(detail Detail, totalWeeks int, matrixWeeks []int) bool {
	covered := weekSet(matrixWeeks)
	weeks, err := WeekRangeWeeks(detail.WeekRange, detail.TimeFlag, totalWeeks)
	// 范围读不出来说明这条说明不派生任何周，标记保持 false
	if err != nil {
		return false
	}
	for _, week := range weeks {
		// 找到一个没有周矩阵的周，这条说明就参与了派生
		if !covered[week] {
			return true
		}
	}
	return false
}

// firstNodeByBlock 建立「大节编码 → 该大节第一个小节的序号」的映射。
// 展开后的小节按轴上顺序排，所以同一大节里第一个就是它的代表。
func firstNodeByBlock(axis []model.AxisNode, blocks []string) (map[string]int, error) {
	index := make(map[string]int, len(blocks))
	for nodeIndex, node := range axis {
		// 同一大节只记第一次出现的序号
		if _, ok := index[node.Block]; !ok {
			index[node.Block] = nodeIndex
		}
	}
	// 表头里的大节在轴上找不到，说明这份矩阵与当前节次轴不是一套
	for _, block := range blocks {
		if _, ok := index[block]; !ok {
			return nil, fmt.Errorf("%w: 大节 %s 不在节次轴上", ErrStructure, block)
		}
	}
	return index, nil
}

// weekSet 把周次列表变成集合，判断「这一周有没有矩阵」时用。
func weekSet(weeks []int) map[int]bool {
	set := make(map[int]bool, len(weeks))
	for _, week := range weeks {
		set[week] = true
	}
	return set
}
