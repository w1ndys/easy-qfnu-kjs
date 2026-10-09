// 本文件属于 business 层：从响应 HTML 里取出 table#dataList 的表头、行与格。
// 上游是一张结构固定的表格，所以按表格结构做定点匹配，不引入 HTML 解析依赖。
// 只解析这张表，页面上的个人课表等其它内容一律不管，也不保留原始 HTML。

package clean

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// 表格结构的匹配式：表、行、格、input 与各类属性。
var (
	// tablePattern 定位 table#dataList 并取出它的内部 HTML
	tablePattern = regexp.MustCompile(`(?is)<table[^>]*\bid\s*=\s*["']?dataList["']?[^>]*>(.*?)</table>`)
	// rowPattern 取出一行的属性与内部 HTML
	rowPattern = regexp.MustCompile(`(?is)<tr([^>]*)>(.*?)</tr>`)
	// cellPattern 取出一个 td 的属性与内部 HTML
	cellPattern = regexp.MustCompile(`(?is)<td([^>]*)>(.*?)</td>`)
	// inputPattern 取出一个 input 的属性
	inputPattern = regexp.MustCompile(`(?is)<input([^>]*)>`)
	// tagPattern 去掉格子里残留的标签，只留可见文本
	tagPattern = regexp.MustCompile(`(?s)<[^>]+>`)
	// jsbhPattern 取 tr 上的 jsbh 属性
	jsbhPattern = regexp.MustCompile(`(?i)\bjsbh\s*=\s*["']?([^"'\s>]+)`)
	// valuePattern 取属性里的 value 值
	valuePattern = regexp.MustCompile(`(?i)\bvalue\s*=\s*["']([^"']*)["']`)
	// tdValuePattern 取 td 的 tdvalue，表头用它标出大节编码
	tdValuePattern = regexp.MustCompile(`(?i)\btdvalue\s*=\s*["']?([^"'\s>]+)`)
	// checkboxPattern 判断一个 input 是不是行内的复选框
	checkboxPattern = regexp.MustCompile(`(?i)\btype\s*=\s*["']?checkbox`)
)

// rawRow 是一行还没清洗的数据：身份、原始房名与 35 格原文。
type rawRow struct {
	jsbh    string   // 房间唯一身份
	nameRaw string   // 首列文本，含容量片段
	cells   []string // 35 格原文，按天分组、组内按大节顺序
}

// rawCell 是一个未清洗的格子：td 的属性与内部 HTML。
type rawCell struct {
	attributes string // td 的属性，表头在这一层带 tdvalue
	inner      string // td 的内部 HTML，取文本要先去掉标签
}

// parseTable 解析 table#dataList：带 tdvalue 的第一行是表头，其余带格子的行是数据行。
// 表头块序与 expected 不一致返回 ErrAxisChanged（整轮停止），其余结构问题返回 ErrStructure。
func parseTable(body []byte, expected []string) ([]string, []rawRow, error) {
	table, err := findDataTable(body)
	if err != nil {
		return nil, nil, err
	}

	var header []string
	rows := make([]rawRow, 0, 64)
	for _, match := range rowPattern.FindAllStringSubmatch(table, -1) {
		attributes, inner := match[1], match[2]
		cells := splitCells(inner)
		// 没有格子的 tr（分隔行、空行）不参与解析
		if len(cells) == 0 {
			continue
		}

		// 带 tdvalue 的行是表头，只认第一处
		if header == nil && isHeaderRow(cells) {
			if err := checkHeader(cells, expected); err != nil {
				return nil, nil, err
			}
			header = tdValues(cells)
			continue
		}

		// 数据行必须是一个房间列加 35 个状态格，个数不对说明表结构变了
		if len(cells) != dataRowCells {
			return nil, nil, fmt.Errorf("%w: 数据行有 %d 个 td，期望 %d", ErrStructure, len(cells), dataRowCells)
		}
		row, err := parseRow(attributes, inner, cells)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, row)
	}

	// 没有表头就校验不了块序，这一份响应不能用
	if header == nil {
		return nil, nil, fmt.Errorf("%w: 表里没有表头行", ErrStructure)
	}
	return header, rows, nil
}

// findDataTable 取出 table#dataList 的内部 HTML；没有这张表说明响应不是状态页。
func findDataTable(body []byte) (string, error) {
	match := tablePattern.FindSubmatch(body)
	// 只解析契约指定的那张表，找不到就是结构失败
	if match == nil {
		return "", fmt.Errorf("%w: 响应里没有 table#dataList", ErrStructure)
	}
	return string(match[1]), nil
}

// splitCells 把一行里的 td 拆成属性与内部 HTML。
func splitCells(inner string) []rawCell {
	matches := cellPattern.FindAllStringSubmatch(inner, -1)
	cells := make([]rawCell, 0, len(matches))
	for _, match := range matches {
		cells = append(cells, rawCell{attributes: match[1], inner: match[2]})
	}
	return cells
}

// isHeaderRow 判断这一行是不是表头：表头的 td 带 tdvalue。
func isHeaderRow(cells []rawCell) bool {
	for _, cell := range cells {
		// 只要有一个 td 带 tdvalue 就按表头处理，避免把半截表头当成数据行
		if tdValuePattern.MatchString(cell.attributes) {
			return true
		}
	}
	return false
}

// tdValues 按列顺序取出表头各格的块编码，不带 tdvalue 的格（房间列）不参与块序。
func tdValues(cells []rawCell) []string {
	codes := make([]string, 0, len(cells))
	for _, cell := range cells {
		match := tdValuePattern.FindStringSubmatch(cell.attributes)
		// 房间列没有 tdvalue，跳过它
		if match == nil {
			continue
		}
		codes = append(codes, match[1])
	}
	return codes
}

// checkHeader 校验表头的块集合与顺序：35 个块格必须按天重复节次轴上的 5 个大节。
// 个数、集合或顺序任何一个不对，都按表头变化处理，返回整轮停止错误。
func checkHeader(cells []rawCell, expected []string) error {
	codes := tdValues(cells)
	// 35 个块格是契约固定的形状，个数不对同样是表头变了
	if len(codes) != blockCellCount {
		return fmt.Errorf("%w: 表头有 %d 个块格，期望 %d", ErrAxisChanged, len(codes), blockCellCount)
	}
	for index, code := range codes {
		// 按天重复：第 index 个块格应当是轴上第 index % 5 个大节
		if want := expected[index%len(expected)]; code != want {
			return fmt.Errorf("%w: 第 %d 个块格是 %s，期望 %s", ErrAxisChanged, index+1, code, want)
		}
	}
	return nil
}

// parseRow 解析一行数据：身份、首列原始房名与 35 个状态格原文。
func parseRow(attributes, inner string, cells []rawCell) (rawRow, error) {
	jsbh, err := rowJsbh(attributes, inner, cells[0].inner)
	if err != nil {
		return rawRow{}, err
	}

	texts := make([]string, 0, blockCellCount)
	for _, cell := range cells[1:] {
		// 第一列是房间信息，后面 35 格才是状态原文
		texts = append(texts, cellText(cell.inner))
	}
	return rawRow{jsbh: jsbh, nameRaw: cellText(cells[0].inner), cells: texts}, nil
}

// rowJsbh 按契约顺序取房间身份：tr 的 jsbh 属性、行内 checkbox 的 value、
// 首列第一个带 value 的 input。三者都取不到就没法把这一行落到某个房间上。
func rowJsbh(attributes, inner, firstCell string) (string, error) {
	// 优先用 tr 上直接写的 jsbh
	if match := jsbhPattern.FindStringSubmatch(attributes); match != nil {
		return html.UnescapeString(match[1]), nil
	}
	// 其次找行内的复选框：它通常就是这一行的房间选择框
	for _, input := range inputPattern.FindAllStringSubmatch(inner, -1) {
		// 只要 checkbox，不要同一行里的其它 input
		if !checkboxPattern.MatchString(input[1]) {
			continue
		}
		if value := attrValue(input[1]); value != "" {
			return value, nil
		}
	}
	// 最后退回首列第一个带 value 的 input，兼容没有复选框的表
	for _, input := range inputPattern.FindAllStringSubmatch(firstCell, -1) {
		if value := attrValue(input[1]); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("%w: 行内取不到 jsbh", ErrStructure)
}

// attrValue 取属性里的 value 值，属性可能用双引号或单引号。
func attrValue(attributes string) string {
	match := valuePattern.FindStringSubmatch(attributes)
	// 没有 value 属性就当没取到
	if match == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(match[1]))
}

// cellText 取格子的可见文本：去掉内部标签、反转义实体并去掉首尾空白。
// 这里不做规范化：状态原文的码点必须原样保留，否则全角符号会认不出来。
func cellText(inner string) string {
	text := tagPattern.ReplaceAllString(inner, "")
	return strings.TrimSpace(html.UnescapeString(text))
}
