package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 本文件的明细夹具全部手写脱敏，不含真实页面样本（docs/pitfalls.md 第 10 节）。
// 结构与 docs/upstream-api.md 第 8 节一致：table#tab1 只是标题表，
// 字段行在下方表单的嵌套三列表里，标签带全角冒号。

// occupancyFieldRow 拼一行合法字段行：标签 / 空列 / 值 三列。
func occupancyFieldRow(label, value string) string {
	return `<tr><td width="173" align="right" bgcolor="#EFEFEF">` + label + `</td>` +
		`<td width="9"></td>` +
		`<td width="560">` + value + `</td></tr>`
}

// occupancyPage 拼一张明细页：标题表 + 表单里包两层的嵌套表。
// 外层的 `<table border="1"><tr><td>` 只有一列，用来验证行宽过滤会跳过它；
// fieldRows 是字段行原文，按页面顺序原样放入。
func occupancyPage(fieldRows ...string) string {
	return `<html><body>` +
		`<center id="alldiv"><table id="tab1"><tr><td class="toolstitle">教室占用情况</td></tr></table></center>` +
		`<form action="/jsxsd/jspyfa/selectJx02" name="Form1" id="Form1" method="post">` +
		`<table border="1"><tr><td><table border="0">` +
		strings.Join(fieldRows, "") +
		`</table></td></tr></table></form></body></html>`
}

// TestOccupancyAdoptsThreeColumnFieldRows 覆盖 Requirement 3.1：
// 只采纳三列且标签去空白、去末尾冒号后属于四项字段的行。
// 夹具混入申请人、任课教师、备注、教室、时间、节次、未知标签，
// 以及两列和四列的行，它们都不得进入结果。
func TestOccupancyAdoptsThreeColumnFieldRows(t *testing.T) {
	page := occupancyPage(
		// 四种标签分别用全角冒号、半角冒号、冒号前带空格、不带冒号，验证归一。
		occupancyFieldRow("教室状态：", " 正常上课 "),
		occupancyFieldRow("教室：", "1号教学楼101"),
		occupancyFieldRow("课程:", "概率论与数理统计"),
		occupancyFieldRow("周次 ：", "1-16"),
		occupancyFieldRow("时间标志", "单双周"),
		occupancyFieldRow("申请人：", "某申请人"),
		occupancyFieldRow("任课教师：", "某教师"),
		occupancyFieldRow("备注：", "某备注"),
		// 两列行与四列行都不是字段行；四列行即使标签命中也必须丢弃。
		`<tr><td>课程：</td><td>两列行</td></tr>`,
		`<tr><td>教室状态：</td><td></td><td>四列行</td><td>多余列</td></tr>`,
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应只采纳 1 条记录，实际 %d 条: %+v", len(entries), entries)
	}
	want := model.OccupancyEntry{
		StateRaw:  "正常上课",
		Course:    "概率论与数理统计",
		WeekRange: "1-16",
		TimeFlag:  "单双周",
	}
	if entries[0] != want {
		t.Fatalf("记录不符:\n got %+v\nwant %+v", entries[0], want)
	}
}

// TestOccupancyKeepsMultipleRecordsInOrder 覆盖 Requirement 3.2：
// 同一格多条记录以教室状态行分条，按页面行顺序保留，字段回填到各自记录。
func TestOccupancyKeepsMultipleRecordsInOrder(t *testing.T) {
	page := occupancyPage(
		occupancyFieldRow("教室状态：", "正常上课"),
		occupancyFieldRow("课程：", "课程甲"),
		occupancyFieldRow("周次：", "1-16"),
		occupancyFieldRow("时间标志：", "单双周"),
		occupancyFieldRow("教室状态：", "考试"),
		occupancyFieldRow("课程：", "课程乙"),
		occupancyFieldRow("周次：", "3-4"),
		occupancyFieldRow("时间标志：", "单周"),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应保留 2 条记录，实际 %d 条: %+v", len(entries), entries)
	}
	want := []model.OccupancyEntry{
		{StateRaw: "正常上课", Course: "课程甲", WeekRange: "1-16", TimeFlag: "单双周"},
		{StateRaw: "考试", Course: "课程乙", WeekRange: "3-4", TimeFlag: "单周"},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("第 %d 条记录不符:\n got %+v\nwant %+v", i+1, entries[i], want[i])
		}
	}
}

// TestOccupancyFieldValueKeepsInnerText 覆盖值里含标签与全角空格的情况：
// 取第三个 td 的完整文本并去掉首尾空白，不把嵌套标签当结构。
func TestOccupancyFieldValueKeepsInnerText(t *testing.T) {
	page := occupancyPage(
		`<tr><td align="right" bgcolor="#EFEFEF">教室状态：</td><td></td>`+
			`<td><font color="red">正常上课</font></td></tr>`,
		`<tr><td align="right" bgcolor="#EFEFEF">课程：</td><td></td>`+
			`<td>　高级数学　</td></tr>`,
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应采纳 1 条记录，实际 %d 条", len(entries))
	}
	if entries[0].StateRaw != "正常上课" {
		t.Fatalf("状态原文应取嵌套标签内文本，实际 %q", entries[0].StateRaw)
	}
	if entries[0].Course != "高级数学" {
		t.Fatalf("课程应去掉首尾全角空格，实际 %q", entries[0].Course)
	}
}

// TestOccupancyTitleTableWithoutFieldRows 覆盖 Requirement 3.4 的前半段：
// 有标题表但没有字段行表示该格无说明，解析成功且返回空列表（非 nil）。
func TestOccupancyTitleTableWithoutFieldRows(t *testing.T) {
	page := occupancyPage()

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("有标题表无字段行应解析成功: %v", err)
	}
	if entries == nil {
		t.Fatal("空结果应是非 nil 空切片，便于上层序列化")
	}
	if len(entries) != 0 {
		t.Fatalf("应返回空列表，实际 %+v", entries)
	}
}

// TestOccupancyMissingTitleTable 覆盖 Requirement 3.4 的后半段：
// 连 table#tab1 都没有说明拿到的不是明细页，判结构失败且错误可区分。
func TestOccupancyMissingTitleTable(t *testing.T) {
	// 只有字段行、没有标题表：不能因为字段看起来对就当成成功。
	page := `<html><body><table border="0">` +
		occupancyFieldRow("教室状态：", "正常上课") +
		`</table></body></html>`

	entries, err := Occupancy([]byte(page))
	if err == nil {
		t.Fatalf("缺标题表应结构失败，实际返回 %+v", entries)
	}
	if !errors.Is(err, ErrDetailStructureInvalid) {
		t.Fatalf("错误应命中 ErrDetailStructureInvalid，实际 %v", err)
	}
	if errors.Is(err, ErrWeekTextInvalid) || errors.Is(err, ErrTermTextInvalid) {
		t.Fatalf("明细结构失败不应归为学期/周历文本错误: %v", err)
	}
}

// TestOccupancyIgnoresOrphanFieldRows 覆盖 docs/upstream-api.md 第 8 节
// 「以教室状态开头分条」：任何「教室状态」行之前的字段行都无归属，必须忽略。
// 旧实现会给它补一条 StateRaw 为空的记录，clean 随后把空原文投影成空闲，
// 把有课的格子说成空教室。
func TestOccupancyIgnoresOrphanFieldRows(t *testing.T) {
	page := occupancyPage(
		// 两条 orphan：出现在第一条教室状态行之前，不得新建记录。
		occupancyFieldRow("课程：", "领先课程"),
		occupancyFieldRow("周次：", "9-10"),
		occupancyFieldRow("教室状态：", "正常上课"),
		occupancyFieldRow("课程：", "课程甲"),
		occupancyFieldRow("时间标志：", "单双周"),
		occupancyFieldRow("教室状态：", "考试"),
		occupancyFieldRow("课程：", "课程乙"),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("orphan 字段行不应多出记录，期望 2 条，实际 %d 条: %+v", len(entries), entries)
	}
	want := []model.OccupancyEntry{
		{StateRaw: "正常上课", Course: "课程甲", TimeFlag: "单双周"},
		{StateRaw: "考试", Course: "课程乙"},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("第 %d 条记录不符:\n got %+v\nwant %+v", i+1, entries[i], want[i])
		}
	}
}

// TestOccupancyOnlyOrphanFieldRowsReturnsEmpty 覆盖 orphan 字段行的极端情况：
// 整页只有非「教室状态」的字段行，说明该格没有可归条的状态，解析成功且返回空列表，
// 不能返回一条 StateRaw 为空的记录（会被 clean 当成空闲）。
func TestOccupancyOnlyOrphanFieldRowsReturnsEmpty(t *testing.T) {
	page := occupancyPage(
		occupancyFieldRow("课程：", "没有状态行"),
		occupancyFieldRow("周次：", "1-16"),
		occupancyFieldRow("时间标志：", "单双周"),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if entries == nil {
		t.Fatal("空结果应是非 nil 空切片，便于上层序列化")
	}
	if len(entries) != 0 {
		t.Fatalf("orphan 字段行不应产生记录，实际 %+v", entries)
	}
}

// TestOccupancyDropsEmptyStateValue 覆盖「教室状态字段值为空」：空状态行不新建记录
// （StateRaw 为空会被 clean 投影成空闲，把有课的格子说成空教室），与被丢弃的 orphan
// 字段行同口径；紧跟它的课程/周次/时间标志也随这条记录一起丢掉，不能回填到上一条
// 记录上把两条数据混在一起。
func TestOccupancyDropsEmptyStateValue(t *testing.T) {
	page := occupancyPage(
		occupancyFieldRow("教室状态：", "正常上课"),
		occupancyFieldRow("课程：", "课程甲"),
		// 空状态：这一条不成立，它下面的字段行没有归属。
		occupancyFieldRow("教室状态：", "  "),
		occupancyFieldRow("课程：", "该丢弃的课程"),
		occupancyFieldRow("周次：", "9-10"),
		// 下一条记录必须完整保留，且不能被上面那两条污染。
		occupancyFieldRow("教室状态：", "考试"),
		occupancyFieldRow("课程：", "课程乙"),
		occupancyFieldRow("时间标志：", "单周"),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("空状态行不应产生记录，期望 2 条，实际 %d 条: %+v", len(entries), entries)
	}
	want := []model.OccupancyEntry{
		{StateRaw: "正常上课", Course: "课程甲"},
		{StateRaw: "考试", Course: "课程乙", TimeFlag: "单周"},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("第 %d 条记录不符:\n got %+v\nwant %+v", i+1, entries[i], want[i])
		}
	}
}

// TestOccupancyOnlyEmptyStateRowsReturnsEmpty 覆盖整页只有空状态行：解析成功且返回空
// 列表，不能返回一条 StateRaw 为空的记录。
func TestOccupancyOnlyEmptyStateRowsReturnsEmpty(t *testing.T) {
	page := occupancyPage(
		occupancyFieldRow("教室状态：", ""),
		occupancyFieldRow("课程：", "该丢弃的课程"),
		occupancyFieldRow("教室状态：", ""),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("明细解析不应失败: %v", err)
	}
	if entries == nil {
		t.Fatal("空结果应是非 nil 空切片，便于上层序列化")
	}
	if len(entries) != 0 {
		t.Fatalf("空状态行不应产生记录，实际 %+v", entries)
	}
}

// TestOccupancyTitleTableWithOnlyNonFieldRows 覆盖 minor 6：
// 有标题表、也有三列行，但没有任何一项四字段时，仍然表示该格无说明，
// 解析成功并返回空列表——不能因为「有行」就编一条记录。
func TestOccupancyTitleTableWithOnlyNonFieldRows(t *testing.T) {
	page := occupancyPage(
		occupancyFieldRow("申请人：", "某申请人"),
		occupancyFieldRow("任课教师：", "某教师"),
		occupancyFieldRow("备注：", "某备注"),
		occupancyFieldRow("教室：", "1号教学楼101"),
		occupancyFieldRow("时间：", "周一 01-02"),
		occupancyFieldRow("节次：", "01-02"),
	)

	entries, err := Occupancy([]byte(page))
	if err != nil {
		t.Fatalf("有标题表无非字段行应解析成功: %v", err)
	}
	if entries == nil {
		t.Fatal("空结果应是非 nil 空切片，便于上层序列化")
	}
	if len(entries) != 0 {
		t.Fatalf("非字段三列行不应产生记录，实际 %+v", entries)
	}
}

// TestOccupancyErrorOmitsPageBody 覆盖 minor 6：明细结构失败的错误信息
// 只能描述缺了哪一项结构，不能回显页面正文（docs/pitfalls.md 第 8 节）。
func TestOccupancyErrorOmitsPageBody(t *testing.T) {
	const (
		pageMarker = "页面正文标记-PAGE-BODY-MARKER-0123456789"
		valueMark  = "某课程-LEAK-CHECK-0123456789"
	)
	// 故意留下像明细页的字段行，但没有标题表：错误里不能出现这些值。
	page := `<html><body><div id="alldiv">` + pageMarker + `</div>` +
		`<table border="0">` +
		occupancyFieldRow("教室状态：", "正常上课") +
		occupancyFieldRow("课程：", valueMark) +
		`</table></body></html>`

	entries, err := Occupancy([]byte(page))
	if err == nil {
		t.Fatalf("缺标题表应结构失败，实际返回 %+v", entries)
	}
	if !errors.Is(err, ErrDetailStructureInvalid) {
		t.Fatalf("错误应命中 ErrDetailStructureInvalid，实际 %v", err)
	}
	for _, leak := range []string{pageMarker, valueMark, "正常上课"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误信息不应含页面正文 %q: %v", leak, err)
		}
	}
}

// TestOccupancyEmptyBody 覆盖空正文：没有标题表，同样判结构失败而不是空列表。
func TestOccupancyEmptyBody(t *testing.T) {
	if _, err := Occupancy(nil); !errors.Is(err, ErrDetailStructureInvalid) {
		t.Fatalf("空正文应命中 ErrDetailStructureInvalid，实际 %v", err)
	}
}
