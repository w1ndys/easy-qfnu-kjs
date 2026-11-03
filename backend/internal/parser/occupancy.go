package parser

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// detailTitleTableID 是占用说明页的标题表选择器：它只含「教室占用情况」一行，
	// 字段行在页面下方的嵌套表格里，不能按它找字段（docs/pitfalls.md 第 4 节）。
	// 它的存在是「这份响应确实是明细页」的唯一结构证据。
	detailTitleTableID = "table#tab1"
	// detailFieldColumns 是字段行的固定列数：标签 / 空列 / 值。
	detailFieldColumns = 3

	// 只保留这四项字段；申请人、任课教师、备注、教室、时间、节次等一律丢弃
	// （Requirement 3.1，docs/prd.md 第 120 行）。
	detailFieldState     = "教室状态"
	detailFieldCourse    = "课程"
	detailFieldWeekRange = "周次"
	detailFieldTimeFlag  = "时间标志"
)

// Occupancy 解析某教室某状态格的占用说明页，返回该格的全部说明记录。
//
// 扫描整页 tr，只采纳恰好三列且标签去空白、去末尾冒号后属于四项字段的行
// （Requirement 3.1）；同一格多条记录以「教室状态」行开始分条，按页面行顺序保留
// （Requirement 3.2，docs/upstream-api.md 第 8 节「以教室状态开头分条」）。
// 有 table#tab1 但没有字段行表示该格无说明，返回空切片与 nil；
// 连 table#tab1 都没有才判结构失败（Requirement 3.4）。
//
// 返回的字段值都是原文，周次范围与采集窗口求交、状态语义键都由 clean 负责。
func Occupancy(body []byte) ([]model.OccupancyEntry, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 页面无法按 HTML 解析", ErrDetailStructureInvalid)
	}

	if doc.Find(detailTitleTableID).Length() == 0 {
		return nil, fmt.Errorf("%w: 找不到 %s", ErrDetailStructureInvalid, detailTitleTableID)
	}

	// 一条记录从「教室状态」行开始，其余字段按行顺序回填到当前记录
	// （docs/upstream-api.md 第 8 节：以教室状态开始分条）。
	// 出现在任何「教室状态」行之前的字段行是 orphan：直接忽略，既不新建记录也不动已有记录。
	// 不能给它补一条空记录——StateRaw 为空会被 clean 投影成空闲，把有课的格子说成空教室。
	entries := make([]model.OccupancyEntry, 0, 1)
	current := -1
	doc.Find("tr").Each(func(_ int, tr *goquery.Selection) {
		cells := tr.ChildrenFiltered("td")
		if cells.Length() != detailFieldColumns {
			return
		}
		field, ok := occupancyField(cells.Eq(0).Text())
		if !ok {
			return
		}
		value := strings.TrimSpace(cells.Eq(2).Text())

		if field == detailFieldState {
			if value == "" {
				// 「教室状态：」值为空：这条记录不成立，按 orphan 字段行同一口径丢弃，
				// 不新建 Entry（StateRaw 为空会被 clean 投影成空闲）。
				// current 一并复位，让紧跟其后的课程/周次/时间标志也随这条记录丢掉，
				// 不能回填到上一条记录上，把两条记录的数据混在一起。
				current = -1
				return
			}
			entries = append(entries, model.OccupancyEntry{StateRaw: value})
			current = len(entries) - 1
			return
		}
		if current < 0 {
			// 没有归属的记录：孤儿字段行，或前面那条「教室状态」行为空已被丢弃。丢掉这一行。
			return
		}
		// 同一记录内同名字段重复时以最后一行为准；正常页面每条记录各字段只出现一次。
		switch field {
		case detailFieldCourse:
			entries[current].Course = value
		case detailFieldWeekRange:
			entries[current].WeekRange = value
		case detailFieldTimeFlag:
			entries[current].TimeFlag = value
		}
	})

	return entries, nil
}

// occupancyField 把字段行的标签归一后与四项字段比对。
// 标签带全角冒号（`教室状态：`），也可能夹空白或半角冒号；
// 归一后再比较，避免因格式差异漏掉整条记录（docs/upstream-api.md 第 9 节）。
func occupancyField(labelText string) (string, bool) {
	label := normalizeDetailLabel(labelText)
	switch label {
	case detailFieldState, detailFieldCourse, detailFieldWeekRange, detailFieldTimeFlag:
		return label, true
	default:
		return "", false
	}
}

// normalizeDetailLabel 去掉标签里的全部空白（含全角空格与 &nbsp;）与末尾冒号，
// 保留其它字符原样，所以「教室状态」「教室状态：」「教室状态 :」都会归一为同一个标签。
func normalizeDetailLabel(labelText string) string {
	var b strings.Builder
	b.Grow(len(labelText))
	for _, r := range labelText {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "：:")
}
