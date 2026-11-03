package parser

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 本文件的矩阵夹具全部手写脱敏，不含真实页面样本（docs/pitfalls.md 第 10 节）。

// matrixBlocks 是表头 5 个大节的编码，与迁移 axis_node 的种子一致。
var matrixBlocks = []string{"0102", "030405", "0607", "0809", "101112"}

// axisFixture 按迁移种子造 12 个小节：5 大节 → 12 小节。
func axisFixture() []model.AxisNode {
	return []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
		{Node: "04", Ordinal: 4, Block: "030405", BlockOrdinal: 2},
		{Node: "05", Ordinal: 5, Block: "030405", BlockOrdinal: 2},
		{Node: "06", Ordinal: 6, Block: "0607", BlockOrdinal: 3},
		{Node: "07", Ordinal: 7, Block: "0607", BlockOrdinal: 3},
		{Node: "08", Ordinal: 8, Block: "0809", BlockOrdinal: 4},
		{Node: "09", Ordinal: 9, Block: "0809", BlockOrdinal: 4},
		{Node: "10", Ordinal: 10, Block: "101112", BlockOrdinal: 5},
		{Node: "11", Ordinal: 11, Block: "101112", BlockOrdinal: 5},
		{Node: "12", Ordinal: 12, Block: "101112", BlockOrdinal: 5},
	}
}

// axisFromBlocks 按给定的大节编码序列造一条最小轴：一个大节一个节点。
// axisBlocks 只看 Block 与 BlockOrdinal，所以够用；用来造「大节数 != 5」的轴。
func axisFromBlocks(blocks []string) []model.AxisNode {
	axis := make([]model.AxisNode, 0, len(blocks))
	for i, block := range blocks {
		axis = append(axis, model.AxisNode{
			Node:         fmt.Sprintf("%02d", i+1),
			Ordinal:      i + 1,
			Block:        block,
			BlockOrdinal: i + 1,
		})
	}
	return axis
}

// matrixHeaderRow 拼表头行：36 个 td，其中 35 个带 tdvalue，
// 状态格按「每天重复 5 大节」展开，第一个房名列不带 tdvalue。
func matrixHeaderRow() string {
	var b strings.Builder
	b.WriteString(`<tr height="60"><td></td>`)
	for day := 0; day < matrixWeekdays; day++ {
		for _, block := range matrixBlocks {
			b.WriteString(`<td tdvalue="` + block + `">` + block + `</td>`)
		}
	}
	b.WriteString(`</tr>`)
	return b.String()
}

// matrixRow 拼一个数据行：首列房名列 + 35 个状态格。
// states 是「状态格序号（从 0 开始）→ 原文」，没列出的格子写空文本（空白格即空闲）。
func matrixRow(trAttrs, roomCell string, states map[int]string) string {
	var b strings.Builder
	b.WriteString("<tr" + trAttrs + ">")
	b.WriteString("<td>" + roomCell + "</td>")
	for i := 0; i < matrixWeekdays*len(matrixBlocks); i++ {
		b.WriteString("<td>" + states[i] + "</td>")
	}
	b.WriteString("</tr>")
	return b.String()
}

// matrixPage 拼一张完整矩阵页：同名前缀的 table#dataListHeader 排在目标表之前，
// 而且它自身就是一张结构完整的假矩阵（表头 + 一行 H0001），
// 一旦实现退化成前缀匹配就会把 H0001 解析出来（docs/pitfalls.md 第 3 节）。
func matrixPage(headerRow string, rows ...string) string {
	return `<html><body><center id="alldiv">` +
		`<table id="dataListHeader" name="kbDataTable" width="100%">` +
		matrixHeaderRow() +
		matrixRow(` jsbh="H0001"`, `<input type="checkbox" value="H0001">表头表的假教室`, nil) +
		`</table>` +
		`<table id="dataList" name="kbDataTable" border="1">` +
		headerRow + strings.Join(rows, "") +
		`</table></center></body></html>`
}

// TestRowsParsesMatrixRows 覆盖 Requirement 2.2、2.3、2.5：
// 表头 36 格里 35 个状态格按轴展开，数据行 36 格，逐格原文与星期/大节按位置对应，
// 空白格保持空原文（语义键由 clean 决定，parser 不猜）。
func TestRowsParsesMatrixRows(t *testing.T) {
	body := matrixPage(matrixHeaderRow(),
		matrixRow(` jsbh="R0001" onclick="pick(this)"`,
			`<input type="checkbox" value="R0001">1号教学楼101(90/0)`,
			map[int]string{0: "◆", 1: "Ｊ", 5: "空闲", 34: "◆Ｊ"}),
		matrixRow(``,
			`<input type="checkbox" value="R0002">2号教学楼202(0/60)`,
			map[int]string{6: "Ｘ"}),
		matrixRow(``,
			`<input type="text" value="R0003">3号教学楼303（15/30）`, nil),
	)

	rooms, err := Rows([]byte(body), axisFixture())
	if err != nil {
		t.Fatalf("解析正常矩阵页应成功，实际 err=%v", err)
	}
	if len(rooms) != 3 {
		t.Fatalf("期望 3 间教室，实际 %d", len(rooms))
	}

	wantIDs := []string{"R0001", "R0002", "R0003"}
	wantNames := []string{
		"1号教学楼101(90/0)",
		"2号教学楼202(0/60)",
		"3号教学楼303（15/30）",
	}
	for i, room := range rooms {
		if room.RoomID != wantIDs[i] {
			t.Fatalf("第 %d 行房号期望 %q，实际 %q", i+1, wantIDs[i], room.RoomID)
		}
		if room.NameRaw != wantNames[i] {
			t.Fatalf("第 %d 行房名期望 %q，实际 %q", i+1, wantNames[i], room.NameRaw)
		}
		if len(room.Cells) != 35 {
			t.Fatalf("第 %d 行期望 35 个状态格，实际 %d", i+1, len(room.Cells))
		}
	}

	// 位置 → 星期/大节由表头顺序唯一决定：前 5 格是周一，第 6 格进周二。
	wantCells := map[int]model.MatrixCell{
		0:  {Weekday: 1, Block: "0102", Raw: "◆"},
		1:  {Weekday: 1, Block: "030405", Raw: "Ｊ"},
		5:  {Weekday: 2, Block: "0102", Raw: "空闲"},
		34: {Weekday: 7, Block: "101112", Raw: "◆Ｊ"},
	}
	for index, want := range wantCells {
		if got := rooms[0].Cells[index]; got != want {
			t.Fatalf("第 1 行第 %d 格期望 %+v，实际 %+v", index+1, want, got)
		}
	}
	if want := (model.MatrixCell{Weekday: 2, Block: "030405", Raw: "Ｘ"}); rooms[1].Cells[6] != want {
		t.Fatalf("第 2 行第 7 格期望 %+v，实际 %+v", want, rooms[1].Cells[6])
	}
	if want := (model.MatrixCell{Weekday: 1, Block: "0607"}); rooms[2].Cells[2] != want {
		t.Fatalf("空白格应保持空原文，期望 %+v，实际 %+v", want, rooms[2].Cells[2])
	}
}

// TestRowsIgnoresPrefixedTableBefore 覆盖 Requirement 2.1：
// 前缀表 table#dataListHeader 在目标表之前且自身是完整假矩阵，
// 只解析目标表意味着结果里绝不会出现 H0001。
func TestRowsIgnoresPrefixedTableBefore(t *testing.T) {
	body := matrixPage(matrixHeaderRow(),
		matrixRow(` jsbh="R0001"`, `1号教学楼101(90/0)`, nil))

	rooms, err := Rows([]byte(body), axisFixture())
	if err != nil {
		t.Fatalf("避开前缀表后应解析成功，实际 err=%v", err)
	}
	if len(rooms) != 1 {
		t.Fatalf("目标表只有 1 行数据，期望 1 间教室，实际 %d", len(rooms))
	}
	if rooms[0].RoomID != "R0001" {
		t.Fatalf("应取目标表的 R0001，实际 %q", rooms[0].RoomID)
	}
	for _, room := range rooms {
		if room.RoomID == "H0001" {
			t.Fatal("不能解析前缀表 table#dataListHeader 里的教室")
		}
	}
}

// TestRowsRoomIDSourcesAndPriority 覆盖 Requirement 2.5：
// 三处来源都要能取到房号，且顺序固定为行属性 → 行内复选框 → 首列输入框。
func TestRowsRoomIDSourcesAndPriority(t *testing.T) {
	cases := []struct {
		name     string
		trAttrs  string
		roomCell string
		want     string
	}{
		{
			name:     "行属性优先于复选框与首列输入框",
			trAttrs:  ` jsbh="R0001"`,
			roomCell: `<input type="checkbox" value="R9999"><input type="text" value="R8888">1号教学楼101`,
			want:     "R0001",
		},
		{
			name:     "复选框优先于首列输入框",
			trAttrs:  ``,
			roomCell: `<input type="checkbox" value="R0002"><input type="text" value="R8888">2号教学楼202`,
			want:     "R0002",
		},
		{
			name:     "只剩首列输入框",
			trAttrs:  ``,
			roomCell: `<input type="text" value="R0003">3号教学楼303`,
			want:     "R0003",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := matrixPage(matrixHeaderRow(), matrixRow(tc.trAttrs, tc.roomCell, nil))
			rooms, err := Rows([]byte(body), axisFixture())
			if err != nil {
				t.Fatalf("解析应成功，实际 err=%v", err)
			}
			if len(rooms) != 1 {
				t.Fatalf("期望 1 间教室，实际 %d", len(rooms))
			}
			if rooms[0].RoomID != tc.want {
				t.Fatalf("房号期望 %q，实际 %q", tc.want, rooms[0].RoomID)
			}
		})
	}
}

// TestRowsRejectsHeaderStructure 覆盖 Requirement 2.2：
// 表头行宽、35 个状态格的集合与顺序、房名列不带 tdvalue，任何一项不符都判结构失败。
func TestRowsRejectsHeaderStructure(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{
			name:   "少一个状态格",
			header: strings.Replace(matrixHeaderRow(), `<td tdvalue="101112">101112</td>`, ``, 1),
		},
		{
			name: "状态格顺序调换",
			header: strings.Replace(matrixHeaderRow(),
				`<td tdvalue="0102">0102</td><td tdvalue="030405">030405</td>`,
				`<td tdvalue="030405">030405</td><td tdvalue="0102">0102</td>`, 1),
		},
		{
			name: "状态格缺 tdvalue",
			header: strings.Replace(matrixHeaderRow(),
				`<td tdvalue="0607">0607</td>`, `<td>0607</td>`, 1),
		},
		{
			name: "状态格编码写错",
			header: strings.Replace(matrixHeaderRow(),
				`<td tdvalue="0809">0809</td>`, `<td tdvalue="0801">0801</td>`, 1),
		},
		{
			name: "房名列混进状态格",
			header: strings.Replace(matrixHeaderRow(),
				`<tr height="60"><td></td>`, `<tr height="60"><td tdvalue="0102"></td>`, 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := matrixPage(tc.header, matrixRow(` jsbh="R0001"`, `1号教学楼101`, nil))
			_, err := Rows([]byte(body), axisFixture())
			if !errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("表头不符应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
			}
			if strings.Contains(err.Error(), "<td") {
				t.Fatalf("错误信息不应带页面标记，实际 %q", err.Error())
			}
		})
	}
}

// TestRowsRejectsRowWidthMismatch 覆盖 Requirement 2.3：
// 数据行单元格数量必须与表头一致，少一格、多一格都判结构失败。
// 多一格的用例专门盯着实现别把 `!=` 放宽成 `>=`：多一格同样会让后续格子错位。
func TestRowsRejectsRowWidthMismatch(t *testing.T) {
	cases := []struct {
		name string
		row  string
	}{
		{
			name: "少一格",
			row: `<tr jsbh="R0001"><td>1号教学楼101</td>` +
				strings.Repeat(`<td></td>`, 34) + `</tr>`,
		},
		{
			name: "多一格",
			row: `<tr jsbh="R0001"><td>1号教学楼101</td>` +
				strings.Repeat(`<td></td>`, 36) + `</tr>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := matrixPage(matrixHeaderRow(), tc.row)
			_, err := Rows([]byte(body), axisFixture())
			if !errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("行宽不符应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), "第 2 行") {
				t.Fatalf("错误信息应指出出错行号，实际 %q", err.Error())
			}
		})
	}
}

// TestRowsRejectsAxisBlockCountMismatch 覆盖 Requirement 2.2 的轴与页面格数对照：
// 4 大节轴能正常折叠成 4 个编码，但页面仍是 35 格（5 大节），
// 表头行宽与「7 × 大节数」不符，必须落页面结构失败而不是轴不可用。
func TestRowsRejectsAxisBlockCountMismatch(t *testing.T) {
	cases := []struct {
		name string
		axis []model.AxisNode
	}{
		{name: "轴只有 4 大节", axis: axisFromBlocks(matrixBlocks[:4])},
		{name: "轴有 6 大节", axis: axisFromBlocks(append(append([]string{}, matrixBlocks...), "131415"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rows([]byte(matrixPage(matrixHeaderRow())), tc.axis)
			if !errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("大节数与页面格数不符应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
			}
			if errors.Is(err, ErrAxisInvalid) {
				t.Fatalf("轴本身可折叠，不应被当成轴不可用，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), "表头") {
				t.Fatalf("错误信息应指出表头行宽不符，实际 %q", err.Error())
			}
		})
	}
}

// TestRowsRejectsDataListWithoutRows 是 Requirement 2.1 的边界：
// 找到 table#dataList 但里面一个 tr 都没有时判结构失败，不能当成「楼里没有教室」返回空集。
func TestRowsRejectsDataListWithoutRows(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "空表",
			body: `<html><body><table id="dataList"></table></body></html>`,
		},
		{
			name: "只有文本没有 tr",
			body: `<html><body><table id="dataList"><caption>教室占用情况</caption>` +
				`<tbody>没有行</tbody></table></body></html>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rows([]byte(tc.body), axisFixture())
			if !errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("dataList 没有 tr 应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), "tr") {
				t.Fatalf("错误信息应指出缺少 tr，实际 %q", err.Error())
			}
		})
	}
}

// TestRowsRejectsFullyFreeRoomListShape 覆盖 docs/upstream-api.md 第 274 行的反向夹具：
// jszt=8 的「完全空闲」响应是房间列表，table#dataList 每行只有 2 列（房间、状态）、没有 35 格，
// 与周矩阵同表名，所以必须判结构失败，不能把 2 列当矩阵读。
func TestRowsRejectsFullyFreeRoomListShape(t *testing.T) {
	body := `<html><body>` +
		`<table id="dataListHeader"><tr><td>教室占用情况</td></tr></table>` +
		`<table id="dataList" border="1">` +
		`<tr><td>房间</td><td>状态</td></tr>` +
		`<tr jsbh="R0001"><td>1号教学楼101</td><td>完全空闲</td></tr>` +
		`<tr jsbh="R0002"><td>1号教学楼102</td><td>完全空闲</td></tr>` +
		`</table></body></html>`

	_, err := Rows([]byte(body), axisFixture())
	if !errors.Is(err, ErrMatrixStructureInvalid) {
		t.Fatalf("两列的完全空闲列表应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
	}
	if errors.Is(err, ErrAxisInvalid) {
		t.Fatalf("页面形态问题不应被当成轴不可用，实际 err=%v", err)
	}
	if strings.Contains(err.Error(), "完全空闲") {
		t.Fatalf("错误信息不应带页面文本，实际 %q", err.Error())
	}
}

// TestRowsRejectsDuplicateRoomID 覆盖 Requirement 2.4：
// 同一响应内重复房号整页判结构失败，不挑一行留下。
func TestRowsRejectsDuplicateRoomID(t *testing.T) {
	body := matrixPage(matrixHeaderRow(),
		matrixRow(` jsbh="R0001"`, `1号教学楼101`, nil),
		matrixRow(` jsbh="R0001"`, `1号教学楼102`, nil),
	)

	_, err := Rows([]byte(body), axisFixture())
	if !errors.Is(err, ErrMatrixStructureInvalid) {
		t.Fatalf("重复房号应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "R0001") {
		t.Fatalf("错误信息应指出重复的房号，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "<tr") {
		t.Fatalf("错误信息不应带页面正文，实际 %q", err.Error())
	}
}

// TestRowsClipsPageDerivedValues 覆盖审核 minor 6（安全）：
// 房号与 tdvalue 都是页面属性原文，长度不受控；错误信息只允许内联 32 rune 的片段，
// 不能让畸形属性把整段页面文本顺着错误带进日志，也不能按字节切出非法 UTF-8。
func TestRowsClipsPageDerivedValues(t *testing.T) {
	// 混入多字节字符：按字节截断会切出半个「号」，utf8.ValidString 会失败。
	longRoomID := "R" + strings.Repeat("号", 60) // 61 rune
	wantClip := "R" + strings.Repeat("号", 31)   // 32 rune

	t.Run("重复房号", func(t *testing.T) {
		body := matrixPage(matrixHeaderRow(),
			matrixRow(` jsbh="`+longRoomID+`"`, `1号教学楼101`, nil),
			matrixRow(` jsbh="`+longRoomID+`"`, `1号教学楼102`, nil),
		)
		_, err := Rows([]byte(body), axisFixture())
		if !errors.Is(err, ErrMatrixStructureInvalid) {
			t.Fatalf("重复房号应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, wantClip+"…") {
			t.Fatalf("错误信息应保留 32 rune 的房号片段 %q，实际 %q", wantClip+"…", msg)
		}
		if strings.Contains(msg, longRoomID) {
			t.Fatalf("错误信息不应内联完整的畸形房号，实际 %q", msg)
		}
		if !utf8.ValidString(msg) {
			t.Fatalf("截断不能切出非法 UTF-8，实际 %q", msg)
		}
	})

	t.Run("表头 tdvalue", func(t *testing.T) {
		longValue := strings.Repeat("Ｊ", 60) // 60 rune
		header := strings.Replace(matrixHeaderRow(),
			`<td tdvalue="0607">0607</td>`, `<td tdvalue="`+longValue+`">x</td>`, 1)
		body := matrixPage(header, matrixRow(` jsbh="R0001"`, `1号教学楼101`, nil))

		_, err := Rows([]byte(body), axisFixture())
		if !errors.Is(err, ErrMatrixStructureInvalid) {
			t.Fatalf("状态格编码不符应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, strings.Repeat("Ｊ", 32)+"…") {
			t.Fatalf("错误信息应保留 32 rune 的 tdvalue 片段，实际 %q", msg)
		}
		if strings.Contains(msg, longValue) {
			t.Fatalf("错误信息不应内联完整的畸形 tdvalue，实际 %q", msg)
		}
		if !utf8.ValidString(msg) {
			t.Fatalf("截断不能切出非法 UTF-8，实际 %q", msg)
		}
	})
}

// TestRowsRejectsRowWithoutRoomID 覆盖 Requirement 2.5 的兜底：
// 行属性、行内复选框、首列输入框都没有值时判结构失败，不能退回用房名猜身份。
func TestRowsRejectsRowWithoutRoomID(t *testing.T) {
	noIDRow := `<tr><td>没有房号的教室</td>` +
		strings.Repeat(`<td></td>`, 35) + `</tr>`
	body := matrixPage(matrixHeaderRow(), noIDRow)

	_, err := Rows([]byte(body), axisFixture())
	if !errors.Is(err, ErrMatrixStructureInvalid) {
		t.Fatalf("取不到房号应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
	}
}

// TestRowsRejectsMissingDataList 覆盖 Requirement 2.1 的反面：
// 只有前缀表 dataListHeader、或正文根本不是矩阵页（登录页）时判结构失败。
func TestRowsRejectsMissingDataList(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "只有前缀表",
			body: `<html><body><table id="dataListHeader"><tr><td>教室占用情况</td></tr>` +
				matrixRow(` jsbh="H0001"`, `表头表的假教室`, nil) + `</table></body></html>`,
		},
		{
			name: "登录页正文",
			body: `<html><body><form>用户登录</form>非法访问</body></html>`,
		},
		{
			name: "空正文",
			body: ``,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rows([]byte(tc.body), axisFixture())
			if !errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("找不到目标表应返回 ErrMatrixStructureInvalid，实际 err=%v", err)
			}
		})
	}
}

// TestRowsAcceptsHeaderOnlyTable 是边界：表头正常但没有数据行时返回空切片与 nil 错误，
// 不能当成结构失败（楼里没有教室与页面变了要分开）。
func TestRowsAcceptsHeaderOnlyTable(t *testing.T) {
	rooms, err := Rows([]byte(matrixPage(matrixHeaderRow())), axisFixture())
	if err != nil {
		t.Fatalf("只有表头时应解析成功，实际 err=%v", err)
	}
	if rooms == nil {
		t.Fatal("没有数据行时应返回空切片而不是 nil")
	}
	if len(rooms) != 0 {
		t.Fatalf("没有数据行时应返回 0 间教室，实际 %d", len(rooms))
	}
}

// TestRowsAcceptsUnorderedAxis 保证大节顺序取自 block_ordinal：
// 传入的轴切片顺序被打乱也必须得到同一结果。
func TestRowsAcceptsUnorderedAxis(t *testing.T) {
	axis := axisFixture()
	for i, j := 0, len(axis)-1; i < j; i, j = i+1, j-1 {
		axis[i], axis[j] = axis[j], axis[i]
	}
	body := matrixPage(matrixHeaderRow(), matrixRow(` jsbh="R0001"`, `1号教学楼101`, nil))

	rooms, err := Rows([]byte(body), axis)
	if err != nil {
		t.Fatalf("轴顺序打乱不应影响解析，实际 err=%v", err)
	}
	if len(rooms) != 1 || len(rooms[0].Cells) != 35 {
		t.Fatalf("期望 1 间教室 35 格，实际 %d 间", len(rooms))
	}
	if want := (model.MatrixCell{Weekday: 1, Block: "0102"}); rooms[0].Cells[0] != want {
		t.Fatalf("第 1 格期望 %+v，实际 %+v", want, rooms[0].Cells[0])
	}
}

// TestRowsRequiresUsableAxis 保证轴本身坏掉时报 ErrAxisInvalid 而不是页面结构失败：
// 上层要能区分「页面变了」与「轴数据坏了」。
// 大节序号有空洞时报的是「缺少第 N 个大节」，不是「序号超出」——{1,3} 的真实问题是缺第 2 个，
// 按节点数当上限去查越界会把这类轴误报成越界（审核 minor 3）。
func TestRowsRequiresUsableAxis(t *testing.T) {
	cases := []struct {
		name    string
		axis    []model.AxisNode
		wantMsg string
	}{
		{name: "空轴", axis: nil, wantMsg: "没有任何节点"},
		{
			name:    "缺大节编码",
			axis:    []model.AxisNode{{Node: "01", Ordinal: 1, BlockOrdinal: 1}},
			wantMsg: "缺少大节编码",
		},
		{
			name: "同一大节序号对应多个编码",
			axis: []model.AxisNode{
				{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
				{Node: "02", Ordinal: 2, Block: "030405", BlockOrdinal: 1},
			},
			wantMsg: "同时对应",
		},
		{
			name: "大节序号不连续",
			axis: []model.AxisNode{
				{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
				{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 3},
			},
			wantMsg: "缺少第 2 个大节",
		},
		{
			name: "缺少第一个大节",
			axis: []model.AxisNode{
				{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 2},
				{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 3},
			},
			wantMsg: "缺少第 1 个大节",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rows([]byte(matrixPage(matrixHeaderRow())), tc.axis)
			if !errors.Is(err, ErrAxisInvalid) {
				t.Fatalf("轴不可用应返回 ErrAxisInvalid，实际 err=%v", err)
			}
			if errors.Is(err, ErrMatrixStructureInvalid) {
				t.Fatalf("轴问题不应被当成页面结构失败，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("错误信息应包含 %q，实际 %q", tc.wantMsg, err.Error())
			}
			if strings.Contains(err.Error(), "超出") {
				t.Fatalf("序号空洞不应被报成越界，实际 %q", err.Error())
			}
		})
	}
}
