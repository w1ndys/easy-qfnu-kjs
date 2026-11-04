package publish

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// fakeReader 是发布组装的假仓储：每个方法都能单独注入返回值与错误，
// 不连库就能锁住「缺学期、窗口越界、仓储失败」这些分支。
type fakeReader struct {
	facts      store.RunFacts
	factsErr   error
	rooms      []store.RunRoom
	roomsErr   error
	summary    store.RunTaskSummary
	summaryErr error
}

func (f *fakeReader) LoadRunFacts(_ context.Context, _ int64) (store.RunFacts, error) {
	return f.facts, f.factsErr
}

func (f *fakeReader) LoadRunRooms(_ context.Context, _ int64) ([]store.RunRoom, error) {
	return f.rooms, f.roomsErr
}

func (f *fakeReader) LoadRunTaskSummary(_ context.Context, _ int64) (store.RunTaskSummary, error) {
	return f.summary, f.summaryErr
}

// newFakeReader 返回一份「一切正常」的假仓储：窗口 5..8、学期齐全、
// r101 归属 Q10、两条必需任务都已完成。
func newFakeReader() *fakeReader {
	return &fakeReader{
		facts: store.RunFacts{
			RunID:       1,
			Term:        "2026-2027-1",
			WindowStart: 5,
			WindowEnd:   8,
		},
		rooms:   []store.RunRoom{{RoomID: "r101", BuildingID: "Q10"}},
		summary: store.RunTaskSummary{RequiredTotal: 2, RequiredDone: 2},
	}
}

// testAxisNodes 是两小节 + 三小节两个大节的轴快照：
// 0102 → 01、02，030405 → 03、04、05，用来验证展开按轴数据走而不是写死 12 小节。
func testAxisNodes() []model.AxisNode {
	return []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
		{Node: "04", Ordinal: 4, Block: "030405", BlockOrdinal: 2},
		{Node: "05", Ordinal: 5, Block: "030405", BlockOrdinal: 2},
	}
}

// testGeneratedAt 是固定的版本生成时间，避免用例依赖当前时刻。
func testGeneratedAt() time.Time {
	return time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
}

// freeObservation 是空文本清洗后的结果：空闲且可用。
func freeObservation() model.Observation {
	return model.Observation{StateKey: model.StateKeyFree, Available: true}
}

// occupancyObservation 是一条已知占用状态，语义键用测试里的虚构值。
func occupancyObservation() model.Observation {
	return model.Observation{StateKey: "class", Available: false, RawText: "正常上课"}
}

// unknownObservation 是未收录状态：语义键 unknown，恒不可用，保留原文。
func unknownObservation() model.Observation {
	return model.Observation{StateKey: model.StateKeyUnknown, Available: false, RawText: "J"}
}

// compositeObservation 是多个符号组合：语义键 composite，恒不可用，保留原文。
func compositeObservation() model.Observation {
	return model.Observation{StateKey: model.StateKeyComposite, Available: false, RawText: "◆Ｊ"}
}

// baseInput 是一份最小可组装的候选原料：两周矩阵格共 7 个小节。
func baseInput() RunInput {
	return RunInput{
		DictVersion: 3,
		AxisVersion: 2,
		GeneratedAt: testGeneratedAt(),
		Buildings:   []Building{{ID: "Q10", Name: "综合教学楼"}},
		AxisNodes:   testAxisNodes(),
		Cells: []CellObservation{
			{RoomID: "r101", Week: 5, Weekday: 1, Block: "0102", State: freeObservation()},
			{RoomID: "r101", Week: 5, Weekday: 1, Block: "030405", State: unknownObservation()},
			{RoomID: "r101", Week: 5, Weekday: 2, Block: "0102", State: occupancyObservation()},
		},
	}
}

// TestNewAxisIndexRejectsBadBlock 覆盖坏轴：每个节点编号必须恰为两位 ASCII 数字，
// 大节编码必须是小节编号的顺序拼接（偶数长度 ASCII 数字），否则展开结果与编码对不上，
// 坏轴在组装前就拒绝。节点位宽单独成条：只靠拼接校验挡不住 Node="0102" 这种
// 把大节编码写进 node 列的坏轴。
func TestNewAxisIndexRejectsBadBlock(t *testing.T) {
	cases := []struct {
		name  string
		nodes []model.AxisNode
	}{
		{"节点为大节编码", []model.AxisNode{{Node: "0102", Block: "0102", Ordinal: 1}}},
		{"节点只有一位", []model.AxisNode{{Node: "1", Block: "12", Ordinal: 1}}},
		{"节点为全角数字", []model.AxisNode{{Node: "０１", Block: "01", Ordinal: 1}}},
		{"大节为奇数长度", []model.AxisNode{{Node: "01", Block: "010", Ordinal: 1}}},
		{"大节含非 ASCII 数字", []model.AxisNode{{Node: "01", Block: "0A", Ordinal: 1}}},
		{"拼接不一致", []model.AxisNode{
			{Node: "01", Block: "0102", Ordinal: 1},
			{Node: "03", Block: "0102", Ordinal: 2},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := newAxisIndex(tc.nodes); !errors.Is(err, ErrAxisInvalid) {
				t.Fatalf("坏轴应返回 ErrAxisInvalid，实际 err=%v", err)
			}
		})
	}

	// 合法轴（01+02 → 0102、03+04+05 → 030405）不能误拦。
	if _, err := newAxisIndex(testAxisNodes()); err != nil {
		t.Fatalf("合法轴不应报错: %v", err)
	}
}

// TestBuildAssemblesExpandedCandidate 覆盖组装主路径：
// 观测按轴展开到小节、未知/复合按大节计一次、归属快照带楼展示名、版本字段来自入参。
func TestBuildAssemblesExpandedCandidate(t *testing.T) {
	candidate, err := Build(context.Background(), newFakeReader(), 1, baseInput())
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}

	// 两格大节展开成 2 + 3 + 2 = 7 个小节。
	if len(candidate.Observations) != 7 {
		t.Fatalf("观测应按轴展开成 7 行，实际 %d 行: %+v", len(candidate.Observations), candidate.Observations)
	}
	if candidate.UnknownCells != 1 {
		t.Fatalf("未知状态应按大节计 1 格（不是 3 个小节），实际 %d", candidate.UnknownCells)
	}
	if candidate.CompositeCells != 0 {
		t.Fatalf("不应有复合格，实际 %d", candidate.CompositeCells)
	}
	if candidate.DerivedCells != 0 {
		t.Fatalf("没有说明时不应派生，实际 %d 格", candidate.DerivedCells)
	}
	if candidate.DroppedDetails != 0 {
		t.Fatalf("没有说明时不应有丢弃计数，实际 %d", candidate.DroppedDetails)
	}

	// 同一大节内的每个小节都拿到同一份状态与可用性。
	wantNodes := map[string]bool{"01": false, "02": false, "03": false, "04": false, "05": false}
	for _, observation := range candidate.Observations {
		if observation.Week != 5 {
			t.Fatalf("观测周不符: %+v", observation)
		}
		wantNodes[observation.Node] = true
	}
	for node, seen := range wantNodes {
		if !seen {
			t.Fatalf("小节 %s 没有观测: %+v", node, candidate.Observations)
		}
	}

	if candidate.RunID != 1 || candidate.Term != "2026-2027-1" ||
		candidate.WindowStart != 5 || candidate.WindowEnd != 8 {
		t.Fatalf("版本事实不符: %+v", candidate)
	}
	if candidate.DictVersion != 3 || candidate.AxisVersion != 2 {
		t.Fatalf("字典/轴版本不符: %+v", candidate)
	}
	if !candidate.GeneratedAt.Equal(testGeneratedAt()) {
		t.Fatalf("生成时间不符: %v", candidate.GeneratedAt)
	}
	if candidate.HasAttrConflict {
		t.Fatalf("没有冲突时不应打标: %+v", candidate)
	}

	wantRooms := []RoomBuilding{{RoomID: "r101", BuildingID: "Q10", BuildingName: "综合教学楼"}}
	if len(candidate.Rooms) != 1 || candidate.Rooms[0] != wantRooms[0] {
		t.Fatalf("归属快照不符: got %+v want %+v", candidate.Rooms, wantRooms)
	}
}

// TestBuildRejectsEmptyCandidate 覆盖 Requirement 7.5 的空候选：
// 没有矩阵观测、说明也派生不出任何观测时，返回可区分的 ErrEmptyCandidate，
// 并且用 ValidationReason 能拿到与 Validate 相同的稳定原因。
// 说明本身不是可发布数据——只有能被用来补写格子状态的说明才产出观测。
func TestBuildRejectsEmptyCandidate(t *testing.T) {
	input := baseInput()
	input.Cells = nil
	// 周次范围解析失败 → 说明被整条丢弃；候选集没有观测，也没有说明。
	input.Details = []CellDetail{{
		RoomID: "r101", Weekday: 1, Block: "0102",
		State: occupancyObservation(), WeekRange: "单周",
	}}

	_, err := Build(context.Background(), newFakeReader(), 1, input)
	if !errors.Is(err, ErrEmptyCandidate) {
		t.Fatalf("空候选应返回 ErrEmptyCandidate，实际 err=%v", err)
	}
	if reason := ValidationReason(err); reason != ReasonEmptyCandidate {
		t.Fatalf("空候选的拦截原因不符: got %q want %q", reason, ReasonEmptyCandidate)
	}
}

// TestBuildRejectsOutOfWindowCell 覆盖 CP1：观测周落在采集窗口之外直接拒绝组装。
func TestBuildRejectsOutOfWindowCell(t *testing.T) {
	input := baseInput()
	input.Cells[0].Week = 4

	_, err := Build(context.Background(), newFakeReader(), 1, input)
	if !errors.Is(err, ErrCandidateInputInvalid) {
		t.Fatalf("窗口外观测应返回 ErrCandidateInputInvalid，实际 err=%v", err)
	}
}

// TestBuildRejectsDuplicateObservation 覆盖同格重复：
// 同一房号同周同天同大节出现两次，展开后会撞 observation 主键，组装阶段就拒绝。
func TestBuildRejectsDuplicateObservation(t *testing.T) {
	input := baseInput()
	input.Cells = append(input.Cells, input.Cells[0])

	_, err := Build(context.Background(), newFakeReader(), 1, input)
	if !errors.Is(err, ErrCandidateInputInvalid) {
		t.Fatalf("重复观测应返回 ErrCandidateInputInvalid，实际 err=%v", err)
	}
}

// TestBuildRejectsUnknownBlock 覆盖轴里没有的大节：无法展开就拒绝，不猜小节。
func TestBuildRejectsUnknownBlock(t *testing.T) {
	input := baseInput()
	input.Cells[0].Block = "9999"

	_, err := Build(context.Background(), newFakeReader(), 1, input)
	if !errors.Is(err, ErrCandidateInputInvalid) {
		t.Fatalf("未知大节应返回 ErrCandidateInputInvalid，实际 err=%v", err)
	}
}

// TestBuildRejectsBadRunFacts 覆盖轮次事实不足：缺学期与没定窗口都不能组装。
func TestBuildRejectsBadRunFacts(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeReader)
	}{
		{"缺学期", func(r *fakeReader) { r.facts.Term = "  " }},
		{"没有窗口", func(r *fakeReader) { r.facts.WindowStart = 0; r.facts.WindowEnd = 0 }},
		{"窗口倒挂", func(r *fakeReader) { r.facts.WindowStart = 8; r.facts.WindowEnd = 5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeReader()
			tc.mutate(reader)
			_, err := Build(context.Background(), reader, 1, baseInput())
			if !errors.Is(err, ErrRunFactsInvalid) {
				t.Fatalf("轮次事实非法应返回 ErrRunFactsInvalid，实际 err=%v", err)
			}
		})
	}
}

// TestBuildRejectsMissingVersions 覆盖版本号缺失：字典/轴版本要写进 release 并受外键约束，
// 零值在组装前就拒绝，不等到发布事务。
func TestBuildRejectsMissingVersions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RunInput)
	}{
		{"缺字典版本", func(input *RunInput) { input.DictVersion = 0 }},
		{"缺轴版本", func(input *RunInput) { input.AxisVersion = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := baseInput()
			tc.mutate(&input)
			_, err := Build(context.Background(), newFakeReader(), 1, input)
			if !errors.Is(err, ErrCandidateInputInvalid) {
				t.Fatalf("版本号非法应返回 ErrCandidateInputInvalid，实际 err=%v", err)
			}
		})
	}
}

// TestBuildPropagatesReaderErrors 覆盖仓储失败原样上抛：组装不做「读不到就空候选」的降级。
func TestBuildPropagatesReaderErrors(t *testing.T) {
	sentinel := errors.New("库挂了")
	cases := []struct {
		name   string
		mutate func(*fakeReader)
	}{
		{"轮次事实", func(r *fakeReader) { r.factsErr = sentinel }},
		{"归属快照", func(r *fakeReader) { r.roomsErr = sentinel }},
		{"任务汇总", func(r *fakeReader) { r.summaryErr = sentinel }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeReader()
			tc.mutate(reader)
			_, err := Build(context.Background(), reader, 1, baseInput())
			if !errors.Is(err, sentinel) {
				t.Fatalf("应原样上抛仓储错误，实际 err=%v", err)
			}
		})
	}
}

// TestBuildDerivesOnlyMissingMatrixWeeks 覆盖 Requirement 6：
// 只在窗口内、该房号该周没有矩阵行时派生；已有矩阵行的周以矩阵为准；
// 同一格被多条说明覆盖时只补一次；范围解析失败或与窗口无交集的说明整条丢弃
// （Requirement 3.3、6.3；原 spec 6.4），既不进候选集也不派生。
func TestBuildDerivesOnlyMissingMatrixWeeks(t *testing.T) {
	input := baseInput()
	// r101 在第 5、6 周都有矩阵行，第 7、8 周没有。
	input.Cells = append(input.Cells, CellObservation{
		RoomID: "r101", Week: 6, Weekday: 1, Block: "0102", State: freeObservation(),
	})
	input.Details = []CellDetail{
		// 5、6 周有矩阵行，跳过；7、8 周派生 3 个小节 × 2 周。
		{RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "5-8"},
		// 同一格再次覆盖：已经派生过，不再写第二份。
		{RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "7-8"},
		// 解析失败：整条丢弃，不派生。
		{RoomID: "r101", Weekday: 2, Block: "0102", State: occupancyObservation(), WeekRange: "单周"},
		// 与窗口 5..8 无交集：整条丢弃，不派生。
		{RoomID: "r101", Weekday: 3, Block: "0102", State: occupancyObservation(), WeekRange: "1-4"},
	}

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}

	// 矩阵：4 格（01/02 + 03/04/05 + 01/02 + 01/02）共 9 行；派生：7、8 周各 3 行共 6 行。
	if len(candidate.Observations) != 15 {
		t.Fatalf("观测行数不符: got %d want 15: %+v", len(candidate.Observations), candidate.Observations)
	}
	if candidate.DerivedCells != 2 {
		t.Fatalf("应派生 2 格（第 7、8 周各一格），实际 %d", candidate.DerivedCells)
	}

	derivedWeeks := map[int]int{}
	for _, observation := range candidate.Observations {
		if observation.Week >= 7 {
			derivedWeeks[observation.Week]++
		}
	}
	if derivedWeeks[7] != 3 || derivedWeeks[8] != 3 {
		t.Fatalf("派生应只落在第 7、8 周且每格 3 个小节，实际 %+v", derivedWeeks)
	}
	for _, observation := range candidate.Observations {
		if observation.Week < 7 {
			continue
		}
		if observation.Weekday != 1 || observation.StateKey != "class" || observation.Available {
			t.Fatalf("派生观测的身份或状态不符: %+v", observation)
		}
	}

	// 只有前两条说明留下；窗口外与解析失败的两条不在 Candidate.Details 里，
	// 但必须计入丢弃数，不能让它们静默消失。
	if candidate.DroppedDetails != 2 {
		t.Fatalf("解析失败与窗口外的说明应各计一次丢弃，实际 %d", candidate.DroppedDetails)
	}
	wantRanges := []string{"5-8", "7-8"}
	if len(candidate.Details) != len(wantRanges) {
		t.Fatalf("说明行数不符: got %d want %d: %+v",
			len(candidate.Details), len(wantRanges), candidate.Details)
	}
	for index, want := range wantRanges {
		if candidate.Details[index].WeekRange != want {
			t.Fatalf("第 %d 条说明的周次范围不符: got %q want %q",
				index+1, candidate.Details[index].WeekRange, want)
		}
	}

	wantDerived := []bool{true, false}
	for index, want := range wantDerived {
		if candidate.Details[index].Derived != want {
			t.Fatalf("第 %d 条说明的派生标记不符: got %v want %v",
				index+1, candidate.Details[index].Derived, want)
		}
	}
}

// TestBuildDropsDetailsOutsideWindow 覆盖 Requirement 3.3（原 spec 6.4）：
// 周次范围与采集窗口求交为空、或范围解析失败的说明整条丢弃——不进 Candidate.Details，
// 也不派生，并计入 Candidate.DroppedDetails。
func TestBuildDropsDetailsOutsideWindow(t *testing.T) {
	input := baseInput()
	input.Cells = input.Cells[:1] // 只有第 5 周有矩阵行
	input.Details = []CellDetail{
		{RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "1-4"},
		{RoomID: "r101", Weekday: 2, Block: "030405", State: occupancyObservation(), WeekRange: "单周"},
		{RoomID: "r101", Weekday: 3, Block: "030405", State: occupancyObservation(), WeekRange: "6-8"},
	}

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}

	if len(candidate.Details) != 1 || candidate.Details[0].WeekRange != "6-8" {
		t.Fatalf("只有窗口内的说明应留下: %+v", candidate.Details)
	}
	if candidate.DroppedDetails != 2 {
		t.Fatalf("窗口外与解析失败的说明应各计一次丢弃，实际 %d", candidate.DroppedDetails)
	}
	if !candidate.Details[0].Derived {
		t.Fatalf("留下的说明应派生过状态: %+v", candidate.Details[0])
	}
	// 6、7、8 周 × 3 个小节 = 9 行派生；第 5 周有矩阵行，不派生。
	if candidate.DerivedCells != 3 {
		t.Fatalf("派生格数不符: got %d want 3", candidate.DerivedCells)
	}
	for _, observation := range candidate.Observations {
		if observation.Week < 5 || observation.Week > 8 {
			t.Fatalf("不应产生窗口外的观测: %+v", observation)
		}
	}
}

// TestBuildCountsDroppedDetails 覆盖丢弃计数：说明因周次范围解析失败、或与采集窗口
// 求交为空而整条丢弃时，Candidate.DroppedDetails 逐条加一；留下的说明不算丢弃。
// 输入数与「留下的条数 + 丢弃数」必须相等，保证没有别的静默丢法。
func TestBuildCountsDroppedDetails(t *testing.T) {
	cases := []struct {
		name        string
		details     []CellDetail
		wantDropped int
		wantKept    int
	}{
		{
			name: "解析失败与窗口外各计一次",
			details: []CellDetail{
				{RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "5-8"},
				{RoomID: "r101", Weekday: 2, Block: "0102", State: occupancyObservation(), WeekRange: "单周"},
				{RoomID: "r101", Weekday: 3, Block: "0102", State: occupancyObservation(), WeekRange: "1-4"},
			},
			wantDropped: 2,
			wantKept:    1,
		},
		{
			name: "与窗口部分重叠不算丢弃",
			details: []CellDetail{
				{RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "1-20"},
			},
			wantDropped: 0,
			wantKept:    1,
		},
		{
			name:        "没有说明不计数",
			details:     nil,
			wantDropped: 0,
			wantKept:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := baseInput()
			input.Details = tc.details

			candidate, err := Build(context.Background(), newFakeReader(), 1, input)
			if err != nil {
				t.Fatalf("组装候选集失败: %v", err)
			}
			if candidate.DroppedDetails != tc.wantDropped {
				t.Fatalf("丢弃计数不符: got %d want %d", candidate.DroppedDetails, tc.wantDropped)
			}
			if len(candidate.Details) != tc.wantKept {
				t.Fatalf("留下的说明条数不符: got %d want %d: %+v",
					len(candidate.Details), tc.wantKept, candidate.Details)
			}
			if len(candidate.Details)+candidate.DroppedDetails != len(input.Details) {
				t.Fatalf("留下的 %d 条 + 丢弃的 %d 条应等于输入的 %d 条",
					len(candidate.Details), candidate.DroppedDetails, len(input.Details))
			}
		})
	}
}

// TestBuildAllowsDerivedOnlyCandidate 覆盖用户拍板 3：观测全部来自派生
// （本轮没有矩阵行）也是合法候选集，Build 不额外拒绝；必需任务门槛仍由 Validate 把关。
func TestBuildAllowsDerivedOnlyCandidate(t *testing.T) {
	input := baseInput()
	input.Cells = nil
	input.Details = []CellDetail{{
		RoomID: "r101", Weekday: 1, Block: "0102",
		State: occupancyObservation(), WeekRange: "5-6", TimeFlag: "单双周",
	}}

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("派生-only 候选不应被 Build 拒绝: %v", err)
	}
	if len(candidate.Observations) != 4 {
		t.Fatalf("两周各 2 个小节，应派生 4 行，实际 %d: %+v",
			len(candidate.Observations), candidate.Observations)
	}
	if candidate.DerivedCells != 2 {
		t.Fatalf("派生格数不符: got %d want 2", candidate.DerivedCells)
	}
	if len(candidate.Details) != 1 || !candidate.Details[0].Derived {
		t.Fatalf("说明应保留并标记派生: %+v", candidate.Details)
	}
}

// TestBuildDerivesByTimeFlag 覆盖用户拍板 1：时间标志参与派生周的选择。
// 单双周与空标记每周都派生；单周只派生奇数周；双周只派生偶数周；
// 不认识的标记不派生，但说明本身照常保留。
func TestBuildDerivesByTimeFlag(t *testing.T) {
	cases := []struct {
		name        string
		flag        string
		wantWeeks   []int
		wantDerived bool
	}{
		{name: "单双周每周都派生", flag: "单双周", wantWeeks: []int{5, 6, 7, 8}, wantDerived: true},
		{name: "空标记按周次范围全派生", flag: "", wantWeeks: []int{5, 6, 7, 8}, wantDerived: true},
		{name: "单周只派生奇数周", flag: "单周", wantWeeks: []int{5, 7}, wantDerived: true},
		{name: "双周只派生偶数周", flag: "双周", wantWeeks: []int{6, 8}, wantDerived: true},
		{name: "不认识的标记不派生", flag: "隔周", wantWeeks: nil, wantDerived: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := baseInput()
			// 换一个没有矩阵行的房号，避免第 5 周的矩阵行干扰奇偶判定。
			input.Details = []CellDetail{{
				RoomID: "r102", Weekday: 1, Block: "0102",
				State: occupancyObservation(), WeekRange: "5-8", TimeFlag: tc.flag,
			}}

			candidate, err := Build(context.Background(), newFakeReader(), 1, input)
			if err != nil {
				t.Fatalf("组装候选集失败: %v", err)
			}

			derivedWeeks := map[int]int{}
			for _, observation := range candidate.Observations {
				if observation.RoomID == "r102" {
					derivedWeeks[observation.Week]++
				}
			}
			if len(derivedWeeks) != len(tc.wantWeeks) {
				t.Fatalf("派生周期望 %v，实际 %+v", tc.wantWeeks, derivedWeeks)
			}
			for _, week := range tc.wantWeeks {
				if derivedWeeks[week] != 2 {
					t.Fatalf("第 %d 周应派生 2 个小节，实际 %+v", week, derivedWeeks)
				}
			}
			if candidate.DerivedCells != len(tc.wantWeeks) {
				t.Fatalf("派生格数不符: got %d want %d", candidate.DerivedCells, len(tc.wantWeeks))
			}
			if len(candidate.Details) != 1 || candidate.Details[0].Derived != tc.wantDerived {
				t.Fatalf("说明的派生标记不符: %+v", candidate.Details)
			}
			if candidate.Details[0].TimeFlag != tc.flag {
				t.Fatalf("时间标志应原样保留: %+v", candidate.Details[0])
			}
		})
	}
}

// TestBuildClampsDerivationToWindow 覆盖「派生只在窗口内」：
// 说明覆盖 1-20 周，窗口 5..8，只在 5..8 里补状态。
func TestBuildClampsDerivationToWindow(t *testing.T) {
	input := baseInput()
	input.Cells = input.Cells[:1] // 只有第 5 周有矩阵行
	input.Details = []CellDetail{{
		RoomID: "r101", Weekday: 1, Block: "030405", State: occupancyObservation(), WeekRange: "1-20",
	}}

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}

	// 第 5 周有矩阵行 → 不派生；6、7、8 周各 3 个小节。
	if candidate.DerivedCells != 3 {
		t.Fatalf("派生格数不符: got %d want 3", candidate.DerivedCells)
	}
	for _, observation := range candidate.Observations {
		if observation.Week < 5 || observation.Week > 8 {
			t.Fatalf("派生不应落在窗口外: %+v", observation)
		}
	}
}

// TestBuildCountsCompositePerBlock 覆盖复合计数口径：
// 030405 展开成 3 个小节，但复合只计 1 格。
func TestBuildCountsCompositePerBlock(t *testing.T) {
	input := baseInput()
	input.Cells = []CellObservation{{
		RoomID: "r101", Week: 5, Weekday: 1, Block: "030405", State: compositeObservation(),
	}}

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}
	if len(candidate.Observations) != 3 {
		t.Fatalf("应展开成 3 个小节，实际 %d", len(candidate.Observations))
	}
	if candidate.CompositeCells != 1 || candidate.UnknownCells != 0 {
		t.Fatalf("复合应按大节计 1 格，实际 composite=%d unknown=%d",
			candidate.CompositeCells, candidate.UnknownCells)
	}
}

// TestBuildSetsAttrConflict 覆盖原 spec 12.1：轮次冲突计数或逐房号冲突标记
// 任一为真都给候选集打「有归属冲突」标记。
func TestBuildSetsAttrConflict(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeReader)
	}{
		{"轮次冲突计数", func(r *fakeReader) { r.facts.AttrConflictCount = 2 }},
		{"房号冲突标记", func(r *fakeReader) {
			r.rooms = []store.RunRoom{{
				RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11",
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := newFakeReader()
			tc.mutate(reader)
			candidate, err := Build(context.Background(), reader, 1, baseInput())
			if err != nil {
				t.Fatalf("组装候选集失败: %v", err)
			}
			if !candidate.HasAttrConflict {
				t.Fatalf("应打上有归属冲突的标记: %+v", candidate)
			}
		})
	}
}

// TestBuildKeepsMissingBuildingNameForValidate 覆盖分工：组装不猜楼名，
// 白名单里没有的楼保留空名，交给 Validate 按「归属快照不完整」拦截。
func TestBuildKeepsMissingBuildingNameForValidate(t *testing.T) {
	input := baseInput()
	input.Buildings = nil

	candidate, err := Build(context.Background(), newFakeReader(), 1, input)
	if err != nil {
		t.Fatalf("组装候选集不应因缺楼名失败: %v", err)
	}
	if len(candidate.Rooms) != 1 || candidate.Rooms[0].BuildingName != "" {
		t.Fatalf("缺楼名时应保留空名: %+v", candidate.Rooms)
	}
	if err := Validate(candidate, Thresholds{}); !errors.Is(err, ErrAttributionIncomplete) {
		t.Fatalf("缺楼名应被 Validate 拦截，实际 err=%v", err)
	}
}
