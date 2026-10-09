package clean

// 本文件是周次范围与派生状态的单元测试：纯函数，不访问网络、不写数据库。
// 覆盖任务 6.1 要求的四项：「1-16」、单双周、范围解析失败、已有矩阵不被覆盖。

import (
	"errors"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testTotalWeeks 是测试用的学期总周数，与设计文档的采样样例（共 16 周）一致。
const testTotalWeeks = 16

// TestWeekRangeWeeksParsesContractForm 断言契约写法「1-16」解析成第 1 到第 16 周。
func TestWeekRangeWeeksParsesContractForm(t *testing.T) {
	weeks, err := WeekRangeWeeks("1-16", "", testTotalWeeks)
	if err != nil {
		t.Fatalf("解析周次范围失败: %v", err)
	}
	if len(weeks) != testTotalWeeks {
		t.Fatalf("解析出 %d 周，期望 %d 周", len(weeks), testTotalWeeks)
	}
	for index, week := range weeks {
		// 闭区间从第 1 周开始，逐个加一
		if week != index+1 {
			t.Errorf("第 %d 个周次 = %d，期望 %d", index+1, week, index+1)
		}
	}
}

// TestWeekRangeWeeksDistinguishesParity 断言单双周能从时间标志区分出来。
func TestWeekRangeWeeksDistinguishesParity(t *testing.T) {
	cases := []struct {
		name     string // 用例说明
		timeFlag string // 时间标志原文
		want     []int  // 期望的周次
	}{
		{"没有时间标志", "", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		{"单双周都有", "单双周", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}},
		{"只上单周", "单周", []int{1, 3, 5, 7, 9, 11, 13, 15}},
		{"只上双周", "双周", []int{2, 4, 6, 8, 10, 12, 14, 16}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			weeks, err := WeekRangeWeeks("1-16", testCase.timeFlag, testTotalWeeks)
			if err != nil {
				t.Fatalf("解析周次范围失败: %v", err)
			}
			if len(weeks) != len(testCase.want) {
				t.Fatalf("解析出 %v，期望 %v", weeks, testCase.want)
			}
			for index, week := range weeks {
				if week != testCase.want[index] {
					t.Fatalf("解析出 %v，期望 %v", weeks, testCase.want)
				}
			}
		})
	}
}

// TestWeekRangeWeeksRejectsUnparsable 断言读不出周次时返回 ErrWeekRange：
// 调用方据此让那些周保持未知，绝不按空闲处理（需求 4.5）。
func TestWeekRangeWeeksRejectsUnparsable(t *testing.T) {
	cases := []struct {
		name       string // 用例说明
		weekRange  string // 周次范围原文
		timeFlag   string // 时间标志原文
		totalWeeks int    // 学期总周数
	}{
		{"周次范围为空白", "  ", "", testTotalWeeks},
		{"周次范围不是数字", "第1-16周", "", testTotalWeeks},
		{"起点晚于终点", "16-1", "", testTotalWeeks},
		{"起点为 0", "0-8", "", testTotalWeeks},
		{"超出一个学期的周次", "1-30", "", testTotalWeeks},
		{"时间标志读不出奇偶", "1-16", "每周", testTotalWeeks},
		{"没有总周数", "1-16", "", 0},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			weeks, err := WeekRangeWeeks(testCase.weekRange, testCase.timeFlag, testCase.totalWeeks)
			if !errors.Is(err, ErrWeekRange) {
				t.Fatalf("返回 %v, %v，期望 ErrWeekRange", weeks, err)
			}
			if len(weeks) != 0 {
				t.Errorf("解析失败时仍返回 %v，期望不给任何周", weeks)
			}
		})
	}
}

// TestDeriveStatesKeepsMatrixWeeks 断言已有周矩阵的周保持矩阵判定：
// 明细范围只给没有矩阵的那一周补派生状态，且标记 derived=true（需求 4.3、4.4）。
func TestDeriveStatesKeepsMatrixWeeks(t *testing.T) {
	details := []Detail{{StateKey: model.StateClass, WeekRange: "1-16"}}
	matrixWeeks := weekNumbers(1, 15)

	states := DeriveStates(details, testTotalWeeks, matrixWeeks)
	if len(states) != 1 {
		t.Fatalf("派生 %d 周，期望 1 周", len(states))
	}
	want := CellState{Week: 16, StateKey: model.StateClass, Derived: true}
	if states[0] != want {
		t.Errorf("派生状态 = %+v，期望 %+v", states[0], want)
	}
	// 明细覆盖到没有矩阵的周，写库时这一条要带派生标记
	if !DetailUsesDerivedWeeks(details[0], testTotalWeeks, matrixWeeks) {
		t.Error("明细覆盖了没有矩阵的第 16 周，derived 标记应为 true")
	}
}

// TestDeriveStatesDoesNotOverwriteFullMatrix 断言全部周都有周矩阵时明细只作说明：
// 一条派生状态都不产出，derived 标记保持 false（需求 4.3）。
func TestDeriveStatesDoesNotOverwriteFullMatrix(t *testing.T) {
	details := []Detail{{StateKey: model.StateFree, WeekRange: "1-16"}}
	matrixWeeks := weekNumbers(1, testTotalWeeks)

	if states := DeriveStates(details, testTotalWeeks, matrixWeeks); len(states) != 0 {
		t.Errorf("派生 %v，期望不产出任何周：已有矩阵的周不被覆盖", states)
	}
	if DetailUsesDerivedWeeks(details[0], testTotalWeeks, matrixWeeks) {
		t.Error("每周都有矩阵时 derived 标记应为 false")
	}
}

// TestDeriveStatesKeepsUnknownOnBadRange 断言范围读不出来时该周保持未知：
// 不产出派生状态，也绝不补出一个空闲周。
func TestDeriveStatesKeepsUnknownOnBadRange(t *testing.T) {
	details := []Detail{{StateKey: model.StateFree, WeekRange: "第1-16周"}}

	if states := DeriveStates(details, testTotalWeeks, nil); len(states) != 0 {
		t.Errorf("派生 %v，期望不产出任何周：范围读不出来时保持未知", states)
	}
	if DetailUsesDerivedWeeks(details[0], testTotalWeeks, nil) {
		t.Error("范围读不出来时 derived 标记应为 false")
	}
}

// TestDeriveStatesFollowsTimeFlag 断言派生周次同样按单双周筛。
func TestDeriveStatesFollowsTimeFlag(t *testing.T) {
	details := []Detail{{StateKey: model.StateBorrowed, WeekRange: "1-16", TimeFlag: "单周"}}

	states := DeriveStates(details, testTotalWeeks, nil)
	want := []int{1, 3, 5, 7, 9, 11, 13, 15}
	if len(states) != len(want) {
		t.Fatalf("派生 %d 周，期望 %d 周", len(states), len(want))
	}
	for index, state := range states {
		if state.Week != want[index] || state.StateKey != model.StateBorrowed || !state.Derived {
			t.Errorf("第 %d 个派生状态 = %+v，期望第 %d 周的借用", index+1, state, want[index])
		}
	}
}

// TestDeriveStatesKeepsFirstDetailPerWeek 断言同一周有多条说明时先到的那条为准，
// 结果稳定且每周只出现一次。
func TestDeriveStatesKeepsFirstDetailPerWeek(t *testing.T) {
	details := []Detail{
		{StateKey: model.StateClass, WeekRange: "1-4"},
		{StateKey: model.StateBorrowed, WeekRange: "2-6"},
	}

	states := DeriveStates(details, testTotalWeeks, nil)
	want := []CellState{
		{Week: 1, StateKey: model.StateClass, Derived: true},
		{Week: 2, StateKey: model.StateClass, Derived: true},
		{Week: 3, StateKey: model.StateClass, Derived: true},
		{Week: 4, StateKey: model.StateClass, Derived: true},
		{Week: 5, StateKey: model.StateBorrowed, Derived: true},
		{Week: 6, StateKey: model.StateBorrowed, Derived: true},
	}
	if len(states) != len(want) {
		t.Fatalf("派生 %d 周，期望 %d 周", len(states), len(want))
	}
	for index, state := range states {
		if state != want[index] {
			t.Errorf("第 %d 个派生状态 = %+v，期望 %+v", index+1, state, want[index])
		}
	}
}

// TestOccupiedCellsPicksNonEmptyCells 断言只挑有内容的格子下钻：
// 上课、借用与未知的格子各一条，空闲格与空格子不下钻，同一格只出现一次（需求 4.1）。
func TestOccupiedCellsPicksNonEmptyCells(t *testing.T) {
	cells := freeCells()
	cells[0] = "◆"   // 周一 0102：正常上课
	cells[1] = "Ｊ"   // 周一 030405：借用
	cells[2] = "张老师" // 周一 0607：未知
	cells[3] = "空闲"  // 周一 0809：空闲文字，不下钻
	cells[4] = " "   // 周一 101112：空单元格，不下钻
	matrix, err := Parse(buildBody(testRoom{jsbh: "r-1", nameRaw: "F126(90/0)", cells: cells}), testAxis(), testSymbols())
	if err != nil {
		t.Fatalf("清洗失败: %v", err)
	}

	refs, err := OccupiedCells(matrix, testAxis())
	if err != nil {
		t.Fatalf("挑格子失败: %v", err)
	}

	want := []CellRef{
		{RoomID: "r-1", Weekday: 1, Block: "0102"},
		{RoomID: "r-1", Weekday: 1, Block: "030405"},
		{RoomID: "r-1", Weekday: 1, Block: "0607"},
	}
	if len(refs) != len(want) {
		t.Fatalf("挑出 %d 个格子，期望 %d 个：%+v", len(refs), len(want), refs)
	}
	for index, ref := range refs {
		if ref != want[index] {
			t.Errorf("第 %d 个格子 = %+v，期望 %+v", index+1, ref, want[index])
		}
	}
}

// TestOccupiedCellsRejectsForeignBlock 断言矩阵里的大节不在当前节次轴上时按结构失败返回：
// 这时挑不出格子，不能拿错的节次去请求明细。
func TestOccupiedCellsRejectsForeignBlock(t *testing.T) {
	matrix := Matrix{Blocks: []string{"9999"}, Rows: []Row{{Jsbh: "r-1"}}}

	if _, err := OccupiedCells(matrix, testAxis()); !errors.Is(err, ErrStructure) {
		t.Fatalf("返回 %v，期望 ErrStructure", err)
	}
}

// weekNumbers 造一段连续的周次，供矩阵周次的用例使用。
func weekNumbers(from, to int) []int {
	weeks := make([]int, 0, to-from+1)
	for week := from; week <= to; week++ {
		weeks = append(weeks, week)
	}
	return weeks
}
