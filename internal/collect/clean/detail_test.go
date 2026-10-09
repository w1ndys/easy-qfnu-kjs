package clean

// 本文件是占用明细解析的单元测试：不访问网络、不写数据库、不涉及 Cookie。
// 正文按上游探测记录的第 5 节结构手写最小片段（弹窗表格按「标签格 + 空格 + 值格」三列重复），
// 真实教务 HTML 不进 git；状态名与字典种子（internal/store/migrations/0003_seed.sql）一致。

import (
	"errors"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testDictStates 造字典种子里的状态表：状态键与中文标签。
func testDictStates() []model.DictState {
	return []model.DictState{
		{StateKey: model.StateClass, Label: "正常上课"},
		{StateKey: model.StateBorrowed, Label: "借用"},
		{StateKey: model.StateLocked, Label: "锁定"},
		{StateKey: model.StateExam, Label: "考试"},
		{StateKey: model.StateFree, Label: "空闲", Available: true},
		{StateKey: model.StateFullyFree, Label: "完全空闲", Available: true},
	}
}

// buildDetailBody 拼一份最小的占用明细弹窗：每条记录是若干「标签 + 值」字段。
func buildDetailBody(records ...[]detailField) []byte {
	var body strings.Builder
	body.WriteString(`<html><body><center id="alldiv"><table id="tab1">`)
	for _, record := range records {
		for _, field := range record {
			body.WriteString(`<tr><td>` + field.label + `</td><td></td><td>` + field.value + `</td></tr>`)
		}
	}
	body.WriteString(`</table></center></body></html>`)
	return []byte(body.String())
}

// detailField 是弹窗里的一行字段：标签与值。
type detailField struct {
	label string // 字段名，例如 教室状态
	value string // 字段值，例如 正常上课
}

// TestParseCellDetailReadsStoredFields 断言只取教室状态、课程、周次范围与时间标志，
// 申请人、任课教师与备注虽然出现在弹窗里，但不会进解析结果（需求 4.2）。
func TestParseCellDetailReadsStoredFields(t *testing.T) {
	body := buildDetailBody([]detailField{
		{label: "教室状态", value: "正常上课"},
		{label: "教室", value: "格物楼B101"},
		{label: "时间", value: ""},
		{label: "节次", value: "10102"},
		{label: "周次", value: "1-16"},
		{label: "时间标志", value: "单周"},
		{label: "备注", value: "历史文化学院"},
		{label: "申 请 人", value: "夏金金"},
		{label: "课程", value: "西方史学史"},
		{label: "任课教师", value: "王静"},
	})

	details, err := ParseCellDetail(body, testDictStates())
	if err != nil {
		t.Fatalf("解析占用明细失败: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("解析出 %d 条说明，期望 1 条", len(details))
	}

	want := Detail{StateKey: model.StateClass, Course: "西方史学史", WeekRange: "1-16", TimeFlag: "单周"}
	if details[0] != want {
		t.Errorf("说明 = %+v，期望 %+v", details[0], want)
	}
}

// TestParseCellDetailKeepsRecordOrder 断言同一格的多条记录按行顺序输出：
// 借用记录只有周次范围，上课记录带课程与任课教师，两条都要能分辨（设计 Data Models）。
func TestParseCellDetailKeepsRecordOrder(t *testing.T) {
	body := buildDetailBody(
		[]detailField{
			{label: "教室状态", value: "借用"},
			{label: "周次", value: "1-16"},
			{label: "时间标志", value: "双周"},
			{label: "备注", value: "研究生排课用"},
			{label: "申请人", value: "祝丽丽"},
		},
		[]detailField{
			{label: "教室状态", value: "正常上课"},
			{label: "周次", value: "1-8"},
			{label: "课程", value: "中国古代史"},
			{label: "任课教师", value: "吴延民"},
		},
	)

	details, err := ParseCellDetail(body, testDictStates())
	if err != nil {
		t.Fatalf("解析占用明细失败: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("解析出 %d 条说明，期望 2 条", len(details))
	}

	want := []Detail{
		{StateKey: model.StateBorrowed, WeekRange: "1-16", TimeFlag: "双周"},
		{StateKey: model.StateClass, Course: "中国古代史", WeekRange: "1-8"},
	}
	for index, detail := range details {
		if detail != want[index] {
			t.Errorf("第 %d 条说明 = %+v，期望 %+v", index+1, detail, want[index])
		}
	}
}

// TestParseCellDetailTreatsUnknownStateAsUnknown 断言没收录的状态名按未知处理：
// 未知不可用，但不阻断发布（docs/contract/data-format.v2.md 的状态字典一节）。
func TestParseCellDetailTreatsUnknownStateAsUnknown(t *testing.T) {
	body := buildDetailBody([]detailField{
		{label: "教室状态", value: "院系自用"},
		{label: "周次", value: "3"},
	})

	details, err := ParseCellDetail(body, testDictStates())
	if err != nil {
		t.Fatalf("解析占用明细失败: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("解析出 %d 条说明，期望 1 条", len(details))
	}
	if details[0].StateKey != model.StateUnknown {
		t.Errorf("状态 = %s，期望 unknown", details[0].StateKey)
	}
	// 未知绝不当成空闲
	if details[0].StateKey.IsUsable() {
		t.Error("未知状态被判为可用")
	}
	if details[0].WeekRange != "3" {
		t.Errorf("周次范围 = %q，期望 3", details[0].WeekRange)
	}
}

// TestParseCellDetailWithoutRecordsIsEmpty 断言没有占用记录时返回空列表而不是错误：
// 弹窗里没有数据行是正常情况（该格子没有说明），只有表本身不见了才算结构失败。
func TestParseCellDetailWithoutRecordsIsEmpty(t *testing.T) {
	body := []byte(`<html><body><center id="alldiv"><table id="tab1"></table></center></body></html>`)

	details, err := ParseCellDetail(body, testDictStates())
	if err != nil {
		t.Fatalf("解析空弹窗失败: %v", err)
	}
	if len(details) != 0 {
		t.Errorf("解析出 %d 条说明，期望 0 条", len(details))
	}
}

// TestParseCellDetailRejectsBrokenStructure 断言表不见了或字典为空时按结构失败返回，
// 不能把「解析不了」当成「这一格没有占用」。
func TestParseCellDetailRejectsBrokenStructure(t *testing.T) {
	cases := []struct {
		name   string            // 用例说明
		body   []byte            // 响应正文
		states []model.DictState // 字典状态
	}{
		{"弹窗里没有 table#tab1", []byte(`<html><body>非法访问</body></html>`), testDictStates()},
		{"状态字典为空", buildDetailBody([]detailField{{label: "教室状态", value: "正常上课"}}), nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseCellDetail(testCase.body, testCase.states); !errors.Is(err, ErrStructure) {
				t.Fatalf("返回 %v，期望 ErrStructure", err)
			}
		})
	}
}
