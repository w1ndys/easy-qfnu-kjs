// 本文件属于 business 层：从响应 HTML 里取出 table#dataList 的表头、行与格。
// 用 goquery 按 CSS 选择器做定点匹配：真实页面里 dataListHeader 排在 dataList 之前，
// 正则很容易误匹配；HTML 解析库按 id 精确选择，也让属性顺序与嵌套结构的变化更可控。
// 只解析这张表，页面上的个人课表等其它内容一律不管，也不保留原始 HTML。

package clean

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// rawRow 是一行还没清洗的数据：身份、原始房名与 35 格原文。
type rawRow struct {
	jsbh    string   // 房间唯一身份
	nameRaw string   // 首列文本，含容量片段
	cells   []string // 35 格原文，按天分组、组内按大节顺序
}

// parseTable 解析 table#dataList：带 tdvalue 的第一行是表头，其余带格子的行是数据行。
// 表头块序与 expected 不一致返回 ErrAxisChanged（整轮停止），其余结构问题返回 ErrStructure。
func parseTable(body []byte, expected []string) ([]string, []rawRow, error) {
	table, err := findDataTable(body)
	if err != nil {
		return nil, nil, err
	}

	rows := table.Find("tr")
	headerIndex, err := findHeaderRow(rows)
	if err != nil {
		return nil, nil, err
	}
	header, err := headerCodes(rows.Eq(headerIndex), expected)
	if err != nil {
		return nil, nil, err
	}

	parsed := make([]rawRow, 0, 64)
	var rowErr error
	rows.EachWithBreak(func(index int, row *goquery.Selection) bool {
		// 表头行已经在上面单独处理，不参与数据行解析
		if index == headerIndex {
			return true
		}
		cells := row.Find("td")
		// 没有格子的 tr（分隔行、空行）不参与解析
		if cells.Length() == 0 {
			return true
		}
		// 数据行必须是一个房间列加 35 个状态格，个数不对说明表结构变了
		if cells.Length() != dataRowCells {
			rowErr = fmt.Errorf("%w: 数据行有 %d 个 td，期望 %d", ErrStructure, cells.Length(), dataRowCells)
			return false
		}
		rowData, err := parseRow(row, cells)
		// 单行结构失败就停止，避免返回半截数据
		if err != nil {
			rowErr = err
			return false
		}
		parsed = append(parsed, rowData)
		return true
	})
	if rowErr != nil {
		return nil, nil, rowErr
	}
	return header, parsed, nil
}

// findDataTable 取出 table#dataList 的选择结果；没有这张表说明响应不是状态页。
// 用 #id 选择器精确匹配，不会把 dataListHeader 当成目标表。
func findDataTable(body []byte) (*goquery.Selection, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	// 正文完全不是 HTML 时解析器会报错，按结构失败处理
	if err != nil {
		return nil, fmt.Errorf("%w: 解析响应 HTML 失败: %v", ErrStructure, err)
	}
	table := doc.Find("table#dataList").First()
	// 只解析契约指定的那张表，找不到就是结构失败
	if table.Length() == 0 {
		return nil, fmt.Errorf("%w: 响应里没有 table#dataList", ErrStructure)
	}
	return table, nil
}

// findHeaderRow 返回带 tdvalue 的表头行下标；没有表头行说明响应不是状态矩阵页。
func findHeaderRow(rows *goquery.Selection) (int, error) {
	index := -1
	rows.EachWithBreak(func(rowIndex int, row *goquery.Selection) bool {
		// 表头的判定是 td 带 tdvalue；数据行的 td 没有这个属性
		if row.Find("td[tdvalue]").Length() > 0 {
			index = rowIndex
			return false
		}
		return true
	})
	// 没有表头就校验不了块序，这一份响应不能用
	if index < 0 {
		return -1, fmt.Errorf("%w: 表里没有表头行", ErrStructure)
	}
	return index, nil
}

// headerCodes 取出表头各列的块编码并校验顺序。
// 不带 tdvalue 的房间列不参与块序；个数、集合或顺序任何一个不对都按表头变化处理。
func headerCodes(row *goquery.Selection, expected []string) ([]string, error) {
	// 期望的大节为空时无法判断表头是不是同一套，属于调用方的数据问题
	if len(expected) == 0 {
		return nil, fmt.Errorf("%w: 表头期望的大节为空", ErrStructure)
	}

	codes := make([]string, 0, blockCellCount)
	row.Find("td[tdvalue]").Each(func(_ int, cell *goquery.Selection) {
		codes = append(codes, strings.TrimSpace(cell.AttrOr("tdvalue", "")))
	})
	// 35 个块格是契约固定的形状，个数不对同样是表头变了
	if len(codes) != blockCellCount {
		return nil, fmt.Errorf("%w: 表头有 %d 个块格，期望 %d", ErrAxisChanged, len(codes), blockCellCount)
	}
	for index, code := range codes {
		// 按天重复：第 index 个块格应当是轴上第 index % 5 个大节
		if want := expected[index%len(expected)]; code != want {
			return nil, fmt.Errorf("%w: 第 %d 个块格是 %s，期望 %s", ErrAxisChanged, index+1, code, want)
		}
	}
	return codes, nil
}

// parseRow 解析一行数据：身份、首列原始房名与 35 个状态格原文。
func parseRow(row *goquery.Selection, cells *goquery.Selection) (rawRow, error) {
	jsbh, err := rowJsbh(row, cells.First())
	// 没有身份就没法把这一行落到某个房间上
	if err != nil {
		return rawRow{}, err
	}

	texts := make([]string, 0, blockCellCount)
	cells.Each(func(index int, cell *goquery.Selection) {
		// 第一列是房间信息，后面 35 格才是状态原文
		if index == 0 {
			return
		}
		texts = append(texts, textOf(cell))
	})
	return rawRow{jsbh: jsbh, nameRaw: textOf(cells.First()), cells: texts}, nil
}

// rowJsbh 按契约顺序取房间身份：tr 的 jsbh 属性、行内 checkbox 的 value、
// 首列第一个带 value 的 input。三者都取不到就没法把这一行落到某个房间上。
func rowJsbh(row, firstCell *goquery.Selection) (string, error) {
	// 优先用 tr 上直接写的 jsbh
	if value := strings.TrimSpace(row.AttrOr("jsbh", "")); value != "" {
		return value, nil
	}
	// 其次找行内的复选框：它通常就是这一行的房间选择框
	if value := strings.TrimSpace(row.Find(`input[type="checkbox"]`).First().AttrOr("value", "")); value != "" {
		return value, nil
	}
	// 最后退回首列第一个带 value 的 input，兼容没有复选框的表
	if value := strings.TrimSpace(firstCell.Find("input[value]").First().AttrOr("value", "")); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("%w: 行内取不到 jsbh", ErrStructure)
}

// textOf 取节点的可见文本并去掉首尾空白。
// 状态原文不做其它规范化：全角符号的码点必须原样保留，否则会认不出来。
func textOf(selection *goquery.Selection) string {
	return strings.TrimSpace(selection.Text())
}
