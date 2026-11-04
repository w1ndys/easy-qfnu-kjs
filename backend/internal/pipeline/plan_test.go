package pipeline

import (
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
)

// matrixKey 是周矩阵任务的唯一身份：楼编号 + 教学周。
type matrixKey struct {
	buildingID string
	week       int
}

// TestStatusAndTaskKindMatchDatabaseContract 固定状态与任务种类对
// collect_run.status 与 sync_task.kind 的取值契约：
// 这些字符串会原样写库，写错只会等到插入时才被 CHECK 约束拒绝。
func TestStatusAndTaskKindMatchDatabaseContract(t *testing.T) {
	statuses := []struct {
		name   string
		status RunStatus
		want   string
	}{
		{name: "进行中", status: StatusRunning, want: "running"},
		{name: "成功", status: StatusSuccess, want: "success"},
		{name: "部分完成", status: StatusPartial, want: "partial"},
		{name: "失败", status: StatusFailed, want: "failed"},
		{name: "阻止", status: StatusBlocked, want: "blocked"},
		{name: "跳过", status: StatusSkipped, want: "skipped"},
	}
	for _, testCase := range statuses {
		t.Run("状态 "+testCase.name, func(t *testing.T) {
			if string(testCase.status) != testCase.want {
				t.Fatalf("状态取值 = %q，期望 %q", testCase.status, testCase.want)
			}
		})
	}

	kinds := []struct {
		name string
		kind TaskKind
		want string
	}{
		{name: "周矩阵", kind: TaskWeekMatrix, want: "week_matrix"},
		{name: "占用说明", kind: TaskCellDetail, want: "cell_detail"},
	}
	for _, testCase := range kinds {
		t.Run("任务种类 "+testCase.name, func(t *testing.T) {
			if string(testCase.kind) != testCase.want {
				t.Fatalf("任务种类取值 = %q，期望 %q", testCase.kind, testCase.want)
			}
		})
	}
}

// TestPlanSkipsWhenWindowEmpty 覆盖 Requirement 1.2：窗口为空时该轮跳过，
// 即使白名单与不可用格都在，也不得生成任何任务（不发起矩阵请求）。
func TestPlanSkipsWhenWindowEmpty(t *testing.T) {
	cases := []struct {
		name   string
		window Window
	}{
		{name: "零值窗口", window: Window{}},
		{name: "当前周小于一的零值窗口", window: Window{Start: 0, End: 0}},
		{name: "起点大于终点的窗口", window: Window{Start: 5, End: 4}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Plan(testCase.window, []string{"Q10", "Q11"}, []Cell{
				{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
			})

			if result.Status != StatusSkipped {
				t.Fatalf("空窗状态 = %q，期望 %q", result.Status, StatusSkipped)
			}
			if len(result.Tasks) != 0 {
				t.Fatalf("空窗不应生成任务，实际 %+v", result.Tasks)
			}
		})
	}
}

// TestPlanBlocksWhenWhitelistEmpty 覆盖 Requirement 1.3：白名单为空时该轮阻止，
// 不生成任何任务；nil 与空切片同义。
func TestPlanBlocksWhenWhitelistEmpty(t *testing.T) {
	cases := []struct {
		name        string
		buildingIDs []string
	}{
		{name: "nil 白名单", buildingIDs: nil},
		{name: "空切片白名单", buildingIDs: []string{}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Plan(Window{Start: 5, End: 8}, testCase.buildingIDs, []Cell{
				{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
			})

			if result.Status != StatusBlocked {
				t.Fatalf("空白名单状态 = %q，期望 %q", result.Status, StatusBlocked)
			}
			if len(result.Tasks) != 0 {
				t.Fatalf("空白名单不应生成任务，实际 %+v", result.Tasks)
			}
		})
	}
}

// TestPlanPrefersSkippedOverBlockedWhenBothEmpty 固定空窗与空白名单同时成立时的
// 先后：窗口先判，所以结果是 skipped。Requirement 1 把窗口判定的 1.2 排在
// 白名单判定的 1.3 之前；两种状态都不发请求，区别只在轮次记录。
func TestPlanPrefersSkippedOverBlockedWhenBothEmpty(t *testing.T) {
	result := Plan(Window{}, nil, nil)

	if result.Status != StatusSkipped {
		t.Fatalf("空窗且空白名单状态 = %q，期望 %q", result.Status, StatusSkipped)
	}
}

// TestPlanCreatesOneMatrixTaskPerBuildingPerWeek 覆盖 Requirement 1.4 与 CP5：
// 每栋白名单楼、窗口内每一周恰好一条必需矩阵任务，顺序为白名单顺序 × 周次升序。
func TestPlanCreatesOneMatrixTaskPerBuildingPerWeek(t *testing.T) {
	result := Plan(Window{Start: 5, End: 8}, []string{"Q10", "Q11"}, nil)

	want := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 5},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 6},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 7},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 8},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 5},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 6},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 7},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 8},
	}

	if result.Status != StatusRunning {
		t.Fatalf("状态 = %q，期望 %q", result.Status, StatusRunning)
	}
	if !reflect.DeepEqual(result.Tasks, want) {
		t.Fatalf("任务清单 =\n%+v\n期望\n%+v", result.Tasks, want)
	}
}

// TestPlanDeduplicatesWhitelistBuildings 覆盖「同一楼同一周至多一条」：
// 白名单里重复的楼编号只保留首次出现，不生成重复任务掩盖配置错误。
func TestPlanDeduplicatesWhitelistBuildings(t *testing.T) {
	result := Plan(Window{Start: 1, End: 2}, []string{"Q10", "Q10", "Q11"}, nil)

	want := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 1},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 2},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 1},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 2},
	}

	if !reflect.DeepEqual(result.Tasks, want) {
		t.Fatalf("重复白名单的任务清单 =\n%+v\n期望\n%+v", result.Tasks, want)
	}
}

// TestPlanCreatesOneDetailTaskPerUnavailableCell 覆盖原 spec Requirement 6.1 与 CP5：
// 每个不可用格一条可选占用说明任务；格子重复只留一条，房号/星期/大节任一不同
// 都算不同格子。矩阵任务恒在可选任务之前。
func TestPlanCreatesOneDetailTaskPerUnavailableCell(t *testing.T) {
	cells := []Cell{
		{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
		{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"}, // 完全重复
		{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "030405"},
		{BuildingID: "Q10", RoomID: "101", Weekday: 2, Block: "0102"},
		{BuildingID: "Q10", RoomID: "102", Weekday: 1, Block: "0102"},
	}

	result := Plan(Window{Start: 2, End: 2}, []string{"Q10"}, cells)

	want := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 2},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "030405"},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "101", Weekday: 2, Block: "0102"},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "102", Weekday: 1, Block: "0102"},
	}

	if !reflect.DeepEqual(result.Tasks, want) {
		t.Fatalf("占用说明任务 =\n%+v\n期望\n%+v", result.Tasks, want)
	}
}

// TestPlanBlocksWhenWindowExceedsWindowWeeks 固定 Plan 侧的窗口防御：
// 窗口本应来自 WindowFor，跨度超过 windowWeeks 的输入直接 blocked，
// 既不按 End-Start+1 规划上百周，也不对 int 上限做超大分配。
func TestPlanBlocksWhenWindowExceedsWindowWeeks(t *testing.T) {
	cases := []struct {
		name   string
		window Window
	}{
		{name: "刚好超过四周", window: Window{Start: 1, End: 5}},
		{name: "明显越界", window: Window{Start: 1, End: 100}},
		{name: "int 上限也不超大分配", window: Window{Start: 1, End: math.MaxInt}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Plan(testCase.window, []string{"Q10"}, nil)

			if result.Status != StatusBlocked {
				t.Fatalf("越界窗口状态 = %q，期望 %q", result.Status, StatusBlocked)
			}
			if len(result.Tasks) != 0 {
				t.Fatalf("越界窗口不应生成任务，实际 %+v", result.Tasks)
			}
		})
	}
}

// TestPlanHandlesWindowEndingAtIntMax 回归「末周为 int 上限」的窗口：
// Window{MaxInt, MaxInt} 与 Window{MaxInt-2, MaxInt} 的跨度分别是 0 与 2，
// 都不超过 windowWeeks，应当正常规划出 1 周与 3 周的矩阵任务，而不是挂死在
// Weeks() 的周次自增里。若规划侧将来选择直接 blocked（同样不发请求），本用例
// 也接受，但必须返回且任务为空。
func TestPlanHandlesWindowEndingAtIntMax(t *testing.T) {
	cases := []struct {
		name   string
		window Window
		weeks  []int
	}{
		{name: "末周为 int 上限的单周窗口",
			window: Window{Start: math.MaxInt, End: math.MaxInt},
			weeks:  []int{math.MaxInt}},
		{name: "末周为 int 上限的三周窗口",
			window: Window{Start: math.MaxInt - 2, End: math.MaxInt},
			weeks:  []int{math.MaxInt - 2, math.MaxInt - 1, math.MaxInt}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Plan(testCase.window, []string{"Q10"}, nil)

			// 这两个窗口跨度 0/2，均未超 windowWeeks，正常路径必须是 running 且任务精确。
			if result.Status != StatusRunning {
				t.Fatalf("状态 = %q，期望 %q", result.Status, StatusRunning)
			}

			want := make([]Task, 0, len(testCase.weeks))
			for _, week := range testCase.weeks {
				want = append(want, Task{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: week})
			}
			if !reflect.DeepEqual(result.Tasks, want) {
				t.Fatalf("任务清单 =\n%+v\n期望\n%+v", result.Tasks, want)
			}
		})
	}
}

// TestPlanDeduplicatesCellAcrossBuildings 覆盖「同一格至多一条」的跨楼情形：
// Q10 与 Q11 都报同一格，输入顺序与白名单一致（Q10 在前），归属自然取 Q10；
// 后续楼重复报同一格时不再生成第二条下钻任务。
func TestPlanDeduplicatesCellAcrossBuildings(t *testing.T) {
	cells := []Cell{
		{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
		{BuildingID: "Q11", RoomID: "101", Weekday: 1, Block: "0102"},
	}

	result := Plan(Window{Start: 1, End: 1}, []string{"Q10", "Q11"}, cells)

	want := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 1},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 1},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
	}

	if !reflect.DeepEqual(result.Tasks, want) {
		t.Fatalf("跨楼重复格的任务 =\n%+v\n期望\n%+v", result.Tasks, want)
	}
}

// TestPlanResolvesCrossBuildingCellByWhitelistRank 固定「输入顺序与白名单顺序
// 相反」时的裁决：同一格先被 Q11 报出、后被 Q10 报出，归属仍取白名单靠前的
// Q10，输入顺序不能决定归属。
func TestPlanResolvesCrossBuildingCellByWhitelistRank(t *testing.T) {
	cells := []Cell{
		{BuildingID: "Q11", RoomID: "101", Weekday: 1, Block: "0102"},
		{BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
	}

	result := Plan(Window{Start: 1, End: 1}, []string{"Q10", "Q11"}, cells)

	want := []Task{
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 1},
		{Kind: TaskWeekMatrix, Required: true, BuildingID: "Q11", Week: 1},
		{Kind: TaskCellDetail, Required: false, BuildingID: "Q10", RoomID: "101", Weekday: 1, Block: "0102"},
	}

	if !reflect.DeepEqual(result.Tasks, want) {
		t.Fatalf("输入顺序与白名单相反的任务 =\n%+v\n期望\n%+v", result.Tasks, want)
	}
}

// planCase 是任务规划属性测试的输入生成器：窗口可空可不空、白名单可有重复、
// 不可用格同一格可重复且可跨楼，专门把去重与窗口边界逼出来。
type planCase struct {
	Window    Window
	Buildings []string
	Cells     []Cell
}

// Generate 实现 testing/quick.Generator。
func (planCase) Generate(rand *rand.Rand, size int) reflect.Value {
	window := Window{}
	// 留一部分样本是空窗，让 skipped 分支也进属性检查。
	if rand.Intn(5) != 0 {
		start := 1 + rand.Intn(8)
		// 4 是字面量而非生产常量 windowWeeks：常量被改时生成器不能跟着变，
		// 否则属性测试的输入范围会和被测口径一起漂移。
		window = Window{Start: start, End: start + rand.Intn(4)}
	}

	buildingIDs := []string{"Q10", "Q11", "Q12"}
	buildings := make([]string, 0, min(size, len(buildingIDs))+1)
	for range rand.Intn(min(size, len(buildingIDs)) + 1) {
		buildings = append(buildings, buildingIDs[rand.Intn(len(buildingIDs))])
	}

	cells := make([]Cell, 0, min(size, 5)+1)
	for range rand.Intn(min(size, 5) + 1) {
		cells = append(cells, Cell{
			BuildingID: buildingIDs[rand.Intn(len(buildingIDs))],
			RoomID:     []string{"101", "102"}[rand.Intn(2)],
			Weekday:    1 + rand.Intn(3),
			Block:      []string{"0102", "030405"}[rand.Intn(2)],
		})
	}

	return reflect.ValueOf(planCase{Window: window, Buildings: buildings, Cells: cells})
}

// TestPlanProperties 是任务规划的属性测试，一次覆盖四条不变量：
// 空窗必 skipped、空白名单必 blocked、每楼每周恰好一条矩阵任务、
// 每个格子至多一条占用说明任务且归属按白名单 rank 裁决。
func TestPlanProperties(t *testing.T) {
	property := func(generated planCase) bool {
		result := Plan(generated.Window, generated.Buildings, generated.Cells)

		// 独立判断空窗，不借 Window.Empty()：否则生产判断改错时期望跟着错。
		if generated.Window.Start < 1 || generated.Window.End < generated.Window.Start {
			return result.Status == StatusSkipped && len(result.Tasks) == 0
		}

		buildings := distinctStrings(generated.Buildings)
		if len(buildings) == 0 {
			return result.Status == StatusBlocked && len(result.Tasks) == 0
		}
		if result.Status != StatusRunning {
			return false
		}

		// 独立逐周枚举并独立裁决归属，不复用 Window.Weeks() 与 Plan 的裁决结果。
		weeks := enumerateWeeks(generated.Window)
		matrixTotal := len(buildings) * len(weeks)
		expectedCells := expectedBuildingByCell(generated.Cells, buildings)
		if len(result.Tasks) != matrixTotal+len(expectedCells) {
			return false
		}

		seenMatrix := make(map[matrixKey]struct{}, matrixTotal)
		seenCell := make(map[cellKey]struct{}, len(expectedCells))
		for index, task := range result.Tasks {
			if index < matrixTotal {
				// 必需段：只带楼与周，其余字段必须留空。
				if task.Kind != TaskWeekMatrix || !task.Required {
					return false
				}
				if task.RoomID != "" || task.Weekday != 0 || task.Block != "" {
					return false
				}
				if task.Week < generated.Window.Start || task.Week > generated.Window.End {
					return false
				}
				key := matrixKey{buildingID: task.BuildingID, week: task.Week}
				if _, ok := seenMatrix[key]; ok {
					return false
				}
				seenMatrix[key] = struct{}{}

				continue
			}

			// 可选段：只带楼与格，周次必须留空。
			if task.Kind != TaskCellDetail || task.Required || task.Week != 0 {
				return false
			}
			key := cellKey{roomID: task.RoomID, weekday: task.Weekday, block: task.Block}
			if _, ok := seenCell[key]; ok {
				return false
			}
			seenCell[key] = struct{}{}
			if task.BuildingID != expectedCells[key] {
				return false
			}
		}

		// 每栋楼每一周都必须出现，不能少也不能多（上面已判唯一）。
		for _, buildingID := range buildings {
			for _, week := range weeks {
				if _, ok := seenMatrix[matrixKey{buildingID: buildingID, week: week}]; !ok {
					return false
				}
			}
		}

		return len(seenCell) == len(expectedCells)
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("任务规划必须满足窗口与唯一性不变量: %v", err)
	}
}

// distinctStrings 按首次出现顺序去重，作为属性测试里独立于实现的期望值。
func distinctStrings(ids []string) []string {
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

// enumerateWeeks 独立按闭区间逐周枚举，作为属性测试的期望值。
// 不复用 Window.Weeks()：否则生产方法改错时测试期望会跟着一起错。
func enumerateWeeks(window Window) []int {
	weeks := make([]int, 0, window.End-window.Start+1)
	for week := window.Start; week <= window.End; week++ {
		weeks = append(weeks, week)
	}

	return weeks
}

// expectedBuildingByCell 独立计算每个格子应归属的楼：白名单名次靠前者优先，
// 不在白名单内的楼排在最后、彼此按首次出现保留。
func expectedBuildingByCell(cells []Cell, buildings []string) map[cellKey]string {
	whitelistRank := make(map[string]int, len(buildings))
	for index, buildingID := range buildings {
		whitelistRank[buildingID] = index
	}
	rankOf := func(buildingID string) int {
		if rank, ok := whitelistRank[buildingID]; ok {
			return rank
		}

		return len(whitelistRank)
	}

	chosen := make(map[cellKey]string, len(cells))
	for _, cell := range cells {
		key := cellKey{roomID: cell.RoomID, weekday: cell.Weekday, block: cell.Block}
		if current, ok := chosen[key]; !ok || rankOf(cell.BuildingID) < rankOf(current) {
			chosen[key] = cell.BuildingID
		}
	}

	return chosen
}
