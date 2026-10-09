package clean

// 本文件是 clean 的单元测试：不访问网络、不写数据库、不涉及 Cookie。
// 正文是内嵌的最小表格片段，真实教务 HTML 不进 git；状态符号与节次轴取自契约种子
// （docs/contract/data-format.v2.md 的状态字典与 internal/store/migrations/0003_seed.sql）。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testBlockCodes 是契约种子里的 5 个大节编码，按天重复组成表头。
var testBlockCodes = []string{"0102", "030405", "0607", "0809", "101112"}

// TestParseClassifiesContractStates 按契约样例覆盖各状态：空闲、上课、借用、锁定、考试、
// 完全空闲、跨模式，以及要能区分且一律不可用的未知与复合。
func TestParseClassifiesContractStates(t *testing.T) {
	cells := freeCells()
	cells[0] = "◆"    // 周一 0102：正常上课
	cells[1] = "Ｊ"    // 周一 030405：借用
	cells[2] = "Ｘ"    // 周一 0607：锁定
	cells[3] = "Κ"    // 周一 0809：考试
	cells[4] = "完全空闲" // 周一 101112：完全空闲
	cells[5] = "张老师"  // 周二 0102：未收录文本
	cells[6] = "◆Ｊ"   // 周二 030405：两个已收录符号拼在一起
	cells[7] = "空闲"   // 周二 0607：空闲文字
	cells[8] = " "    // 周二 0809：空单元格
	cells[9] = "M"    // 周二 101112：跨模式占用

	body := buildBody(testRoom{jsbh: "r-1", nameRaw: "F126(90/0)", cells: cells})
	matrix, err := Parse(body, testAxis(), testSymbols())
	if err != nil {
		t.Fatalf("清洗失败: %v", err)
	}
	if len(matrix.Rows) != 1 {
		t.Fatalf("清洗出 %d 行，期望 1 行", len(matrix.Rows))
	}
	if len(matrix.Blocks) != blocksPerDay {
		t.Errorf("大节编码有 %d 个，期望 %d 个", len(matrix.Blocks), blocksPerDay)
	}

	row := matrix.Rows[0]
	// 周一：一个大节一格状态，块内 12 小节的各节点状态必须一致
	checkDay(t, row, 0, []model.StateKey{
		model.StateClass, model.StateClass,
		model.StateBorrowed, model.StateBorrowed, model.StateBorrowed,
		model.StateLocked, model.StateLocked,
		model.StateExam, model.StateExam,
		model.StateFullyFree, model.StateFullyFree, model.StateFullyFree,
	})
	// 周二：未知、复合、空闲、空单元格与跨模式
	checkDay(t, row, 1, []model.StateKey{
		model.StateUnknown, model.StateUnknown,
		model.StateComposite, model.StateComposite, model.StateComposite,
		model.StateFree, model.StateFree,
		model.StateFree, model.StateFree,
		model.StateCrossMode, model.StateCrossMode, model.StateCrossMode,
	})
	// 周三到周日没写过内容，整天空闲
	for day := 2; day < DayCount; day++ {
		checkDay(t, row, day, freeStates())
	}

	// 未知与复合要能区分：各自留原文，且都不可用
	checkCell(t, row.Week[1][0], model.StateUnknown, "张老师")
	checkCell(t, row.Week[1][2], model.StateComposite, "◆Ｊ")
	// 命中标准符号的格子不需要留原文
	checkCell(t, row.Week[0][0], model.StateClass, "")
	// 空单元格是空闲，算可用
	if !row.Week[3][0].StateKey.IsUsable() {
		t.Error("空单元格应当判为可用状态")
	}
}

// TestParseNormalizesName 覆盖房名规范化：NFKC、容量片段删除、方位括号保留。
func TestParseNormalizesName(t *testing.T) {
	cases := []struct {
		nameRaw string // 上游原始房名
		want    string // 期望的规范化展示名
	}{
		{"F126(90/0)", "F126"},
		{"Ｆ１２６（９０／０）", "F126"},     // 全角字母与数字先被 NFKC 归一
		{"篮球馆（东）(25/0)", "篮球馆(东)"}, // 方位括号保留，只被 NFKC 归一宽度
		{"综合教学楼 101 (60/0)", "综合教学楼 101"},
		{"F126", "F126"},
	}

	for _, testCase := range cases {
		body := buildBody(testRoom{jsbh: "r-name", nameRaw: testCase.nameRaw, cells: freeCells()})
		matrix, err := Parse(body, testAxis(), testSymbols())
		if err != nil {
			t.Fatalf("清洗 %s 失败: %v", testCase.nameRaw, err)
		}
		// 规范化只动展示名，原始名照样留着
		if got := matrix.Rows[0].Name; got != testCase.want {
			t.Errorf("%s 规范化为 %s，期望 %s", testCase.nameRaw, got, testCase.want)
		}
		if matrix.Rows[0].NameRaw != testCase.nameRaw {
			t.Errorf("原始房名 = %q，期望 %q", matrix.Rows[0].NameRaw, testCase.nameRaw)
		}
	}
}

// TestParseRoomIdentityOrder 覆盖 jsbh 的取值顺序：tr 属性、行内 checkbox、首列带 value 的 input。
func TestParseRoomIdentityOrder(t *testing.T) {
	checkbox := `<input type="checkbox" value="r-check"><input name="jsbh" value="r-input">`
	cases := []struct {
		name string // 用例说明
		row  string // 行 HTML
		want string // 期望的 jsbh
	}{
		{"tr 属性优先", roomRowHTML(`jsbh="r-attr"`, checkbox+"F126", freeCells()), "r-attr"},
		{"tr 没有 jsbh 时用行内 checkbox", roomRowHTML(`class="room"`, checkbox+"F127", freeCells()), "r-check"},
		{"没有 checkbox 时用首列 input", roomRowHTML(`jsbh=""`, `<input name="jsbh" value="r-input">F128`, freeCells()), "r-input"},
	}

	for _, testCase := range cases {
		body := buildBodyWithBlocks(testBlockCodes, DayCount, []string{testCase.row})
		matrix, err := Parse(body, testAxis(), testSymbols())
		if err != nil {
			t.Fatalf("%s 清洗失败: %v", testCase.name, err)
		}
		if got := matrix.Rows[0].Jsbh; got != testCase.want {
			t.Errorf("%s 取到 jsbh %s，期望 %s", testCase.name, got, testCase.want)
		}
	}
}

// TestParseRejectsDuplicateJsbh 覆盖同一响应内重复 jsbh：不能挑一行留下。
func TestParseRejectsDuplicateJsbh(t *testing.T) {
	first := testRoom{jsbh: "r-dup", nameRaw: "F126(90/0)", cells: freeCells()}
	third := testRoom{jsbh: "r-other", nameRaw: "F127(90/0)", cells: freeCells()}

	matrix, err := Parse(buildBody(first, third, first), testAxis(), testSymbols())
	if !errors.Is(err, ErrStructure) {
		t.Fatalf("返回 %v，期望 ErrStructure", err)
	}
	// 结构失败时不返回任何行，调用方拿不到半截数据
	if len(matrix.Rows) != 0 {
		t.Errorf("重复 jsbh 时仍然返回了 %d 行", len(matrix.Rows))
	}
}

// TestParseRejectsChangedHeader 覆盖表头块集合变化与顺序错乱：整轮停止，不许自动适配，
// 也不许降级成单格 unknown。
func TestParseRejectsChangedHeader(t *testing.T) {
	row := roomRowHTML(`jsbh="r-1"`, "F126(90/0)", freeCells())
	cases := []struct {
		name   string   // 用例说明
		blocks []string // 表头按天重复的块编码
		days   int      // 表头重复几天，用来造块格个数不对
	}{
		{"块集合变化", []string{"0102", "030405", "0607", "0809", "1011"}, DayCount},
		{"块顺序错乱", []string{"030405", "0102", "0607", "0809", "101112"}, DayCount},
		{"块格个数不足", testBlockCodes, DayCount - 1},
	}

	for _, testCase := range cases {
		body := buildBodyWithBlocks(testCase.blocks, testCase.days, []string{row})
		matrix, err := Parse(body, testAxis(), testSymbols())
		// 表头变化是整轮停止错误，必须与结构失败、单格未知分开
		if !errors.Is(err, ErrAxisChanged) {
			t.Errorf("%s 返回 %v，期望 ErrAxisChanged", testCase.name, err)
		}
		if errors.Is(err, ErrStructure) {
			t.Errorf("%s 同时被判成 ErrStructure，两类错误混淆了", testCase.name)
		}
		if len(matrix.Rows) != 0 {
			t.Errorf("%s 仍然返回了 %d 行", testCase.name, len(matrix.Rows))
		}
	}
}

// TestParseRejectsBadStructure 覆盖结构失败：缺表、行内 td 个数不对、没有表头行、
// 行内取不到 jsbh、符号表为空。
func TestParseRejectsBadStructure(t *testing.T) {
	shortCells := freeCells()
	shortCells = shortCells[:len(shortCells)-1]
	cases := []struct {
		name    string             // 用例说明
		body    []byte             // 响应正文
		symbols []model.DictSymbol // 符号表
	}{
		{"响应里没有 table#dataList", []byte(`<html><body>用户登录</body></html>`), testSymbols()},
		{"数据行 td 个数不对",
			buildBodyWithBlocks(testBlockCodes, DayCount,
				[]string{roomRowHTML(`jsbh="r-2"`, "F127(90/0)", shortCells)}), testSymbols()},
		{"表里没有表头行",
			buildBodyWithBlocks(nil, 0, []string{roomRowHTML(`jsbh="r-3"`, "F128(90/0)", freeCells())}), testSymbols()},
		{"行内取不到 jsbh",
			buildBodyWithBlocks(testBlockCodes, DayCount,
				[]string{roomRowHTML(`class="room"`, "F129(90/0)", freeCells())}), testSymbols()},
		{"符号表为空",
			buildBody(testRoom{jsbh: "r-4", nameRaw: "F130(90/0)", cells: freeCells()}), nil},
	}

	for _, testCase := range cases {
		matrix, err := Parse(testCase.body, testAxis(), testCase.symbols)
		if !errors.Is(err, ErrStructure) {
			t.Errorf("%s 返回 %v，期望 ErrStructure", testCase.name, err)
		}
		// 结构失败时不返回任何行
		if len(matrix.Rows) != 0 {
			t.Errorf("%s 仍然返回了 %d 行", testCase.name, len(matrix.Rows))
		}
	}
}

// TestExpandDayRejectsForeignHeader 覆盖轴与表头不是一套的情况：找不到大节就结构失败。
func TestExpandDayRejectsForeignHeader(t *testing.T) {
	// 表头只认识前 4 个大节，轴上第 5 个大节的 3 个小节没有状态来源
	day, err := expandDay(testBlockCodes[:4], freeCellsInState(), testAxis())
	if !errors.Is(err, ErrStructure) {
		t.Errorf("返回 %v，期望 ErrStructure", err)
	}
	if day[NodeCount-1].StateKey != "" {
		t.Errorf("第 %d 个小节不该有状态", NodeCount)
	}

	// 表头与轴一致时每个小节都能认领到状态
	day, err = expandDay(testBlockCodes, freeCellsInState(), testAxis())
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	for nodeIndex := range day {
		if day[nodeIndex].StateKey != model.StateFree {
			t.Errorf("第 %d 个小节 = %s，期望 free", nodeIndex+1, day[nodeIndex].StateKey)
		}
	}
}

// testAxis 按契约种子造节次轴：12 个节点，5 个大节，按展示顺序排列。
func testAxis() []model.AxisNode {
	blocks := []struct {
		code  string // 大节编码
		nodes int    // 该大节覆盖几个小节
	}{
		{"0102", 2},
		{"030405", 3},
		{"0607", 2},
		{"0809", 2},
		{"101112", 3},
	}

	nodes := make([]model.AxisNode, 0, NodeCount)
	ordinal := 0
	for blockOrdinal, block := range blocks {
		for node := 0; node < block.nodes; node++ {
			ordinal++
			nodes = append(nodes, model.AxisNode{
				AxisVersion:  1,
				Node:         fmt.Sprintf("%02d", ordinal),
				Ordinal:      ordinal,
				Block:        block.code,
				BlockOrdinal: blockOrdinal + 1,
			})
		}
	}
	return nodes
}

// testSymbols 造契约种子里 dict_version 1 的符号表，码点必须原样。
func testSymbols() []model.DictSymbol {
	return []model.DictSymbol{
		{Symbol: "◆", StateKey: model.StateClass},
		{Symbol: "Ｊ", StateKey: model.StateBorrowed},
		{Symbol: "Ｘ", StateKey: model.StateLocked},
		{Symbol: "Κ", StateKey: model.StateExam},
		{Symbol: "空闲", StateKey: model.StateFree},
		{Symbol: "Ｇ", StateKey: model.StateFixedReschedule},
		{Symbol: "Ｌ", StateKey: model.StateTempReschedule},
		{Symbol: "完全空闲", StateKey: model.StateFullyFree},
		{Symbol: "M", StateKey: model.StateCrossMode},
	}
}

// testRoom 是夹具里的一间房：身份、原始房名与 35 格原文。
type testRoom struct {
	jsbh    string   // 房间身份
	nameRaw string   // 房名，含容量片段
	cells   []string // 35 格原文，按天分组、组内按大节顺序
}

// buildBody 拼一份最小响应：表头用契约的 5 个大节重复 7 天，每个房间一行。
func buildBody(rooms ...testRoom) []byte {
	rows := make([]string, 0, len(rooms))
	for _, room := range rooms {
		rows = append(rows, roomRowHTML(`jsbh="`+room.jsbh+`"`, room.nameRaw, room.cells))
	}
	return buildBodyWithBlocks(testBlockCodes, DayCount, rows)
}

// buildBodyWithBlocks 拼一份响应：表头按给定块重复 days 天，后面接给定的数据行。
func buildBodyWithBlocks(blocks []string, days int, rows []string) []byte {
	var body strings.Builder
	body.WriteString(`<html><body><table id="dataList"><tr>`)
	for day := 0; day < days; day++ {
		for _, code := range blocks {
			body.WriteString(`<td tdvalue="` + code + `"></td>`)
		}
	}
	body.WriteString("</tr>")
	for _, row := range rows {
		body.WriteString(row)
	}
	body.WriteString("</table></body></html>")
	return []byte(body.String())
}

// roomRowHTML 拼一行数据：一个房间列加 35 个状态格。
func roomRowHTML(rowAttributes, firstCell string, cells []string) string {
	var row strings.Builder
	row.WriteString("<tr " + rowAttributes + "><td>" + firstCell + "</td>")
	for _, cell := range cells {
		row.WriteString("<td>" + cell + "</td>")
	}
	row.WriteString("</tr>")
	return row.String()
}

// freeCells 造 35 个空格原文，代表整周全空闲。
func freeCells() []string {
	cells := make([]string, 0, blockCellCount)
	for index := 0; index < blockCellCount; index++ {
		cells = append(cells, "")
	}
	return cells
}

// freeCellsInState 造 5 个已清洗的空闲格，供展开函数直接用。
func freeCellsInState() []Cell {
	cells := make([]Cell, 0, blocksPerDay)
	for index := 0; index < blocksPerDay; index++ {
		cells = append(cells, Cell{StateKey: model.StateFree})
	}
	return cells
}

// freeStates 造一天 12 小节的空闲状态序列。
func freeStates() []model.StateKey {
	states := make([]model.StateKey, 0, NodeCount)
	for index := 0; index < NodeCount; index++ {
		states = append(states, model.StateFree)
	}
	return states
}

// checkDay 断言某个教学日 12 个小节的状态序列。
func checkDay(t *testing.T, row Row, day int, want []model.StateKey) {
	t.Helper()

	// 契约里一天恰 12 个小节，长度不对说明夹具写错了
	if len(want) != NodeCount {
		t.Fatalf("用例给了一天 %d 个小节，期望 %d 个", len(want), NodeCount)
	}
	for nodeIndex, stateKey := range want {
		if got := row.Week[day][nodeIndex].StateKey; got != stateKey {
			t.Errorf("第 %d 天第 %d 节 = %s，期望 %s", day+1, nodeIndex+1, got, stateKey)
		}
	}
}

// checkCell 断言一个格子的语义键与留档原文。
func checkCell(t *testing.T, cell Cell, wantKey model.StateKey, wantRaw string) {
	t.Helper()

	if cell.StateKey != wantKey {
		t.Errorf("状态 = %s，期望 %s", cell.StateKey, wantKey)
	}
	if cell.RawText != wantRaw {
		t.Errorf("留档原文 = %q，期望 %q", cell.RawText, wantRaw)
	}
	// 未知与复合一律不可用：绝不把未知当空闲
	if wantKey == model.StateUnknown || wantKey == model.StateComposite {
		if cell.StateKey.IsUsable() {
			t.Errorf("%s 不该判为可用", wantKey)
		}
	}
}
