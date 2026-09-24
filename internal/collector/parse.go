package collector

import (
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

const (
	numDays   = 7
	numBlocks = 5
	// 每行单元格数 = 1 个教室列 + 7 天 × 5 大节状态格。
	expectedRowCells = 1 + numDays*numBlocks
)

// ParsedRow 单间教室一行的解析结果。Blocks[day][block] 是状态 ID：
// day=0..6 对应星期一..星期日，block=0..4 对应 {0102,030405,0607,0809,101112}。
type ParsedRow struct {
	Jsbh   string
	Name   string // 规范化后的房间名
	Blocks [numDays][numBlocks]int
}

// ParseWeekHTML 解析 jsjy_query2 返回的整周表格 HTML。
//
// 结构契约（docs/upstream.md）：
//   - 存在 table#dataList；表头 td 携带 tdvalue，按天重复 5 大节；
//   - 数据行共 1+35 个 td：首列为教室（checkbox + 房名），随后按
//     天（xq=1..7）× 大节顺序排列状态格；
//   - 状态文本 → ID；空文本 = 5（空闲）；未知文本 → 结构失败（禁止发布）。
//
// 调用方必须先排除登录页/非法访问页特征；这里会再次防御性检测。
func ParseWeekHTML(body string) ([]ParsedRow, error) {
	if err := rejectBlockedPage(body); err != nil {
		return nil, err
	}
	table, err := findWeekTable(body)
	if err != nil {
		return nil, err
	}
	if err := readHeaderBlocks(table); err != nil {
		return nil, err
	}
	return parseTableRows(table)
}

func rejectBlockedPage(body string) error {
	if strings.Contains(body, MarkLoginPage) {
		return newGlobal(CodeLoginPage, "响应命中登录页特征，无法解析周表")
	}
	if strings.Contains(body, MarkIllegalAccess) {
		return newGlobal(CodeIllegalAccess, "响应命中非法访问特征，无法解析周表")
	}
	return nil
}

func findWeekTable(body string) (*goquery.Selection, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, newGlobal(CodeStructure, "解析 HTML 失败: %v", err)
	}
	table := doc.Find("table#dataList").First()
	if table.Length() == 0 {
		return nil, newGlobal(CodeStructure, "响应中缺少 table#dataList，无法解析周表")
	}
	return table, nil
}

func readHeaderBlocks(table *goquery.Selection) error {
	var blocks []string
	table.Find("td[tdvalue]").Each(func(_ int, s *goquery.Selection) {
		if v, ok := s.Attr("tdvalue"); ok {
			blocks = append(blocks, v)
		}
	})
	return validateHeaderBlocks(blocks)
}

func parseTableRows(table *goquery.Selection) ([]ParsedRow, error) {
	dataRows := weekDataRows(table)
	var rows []ParsedRow
	seenJsbh := map[string]bool{}
	var parseErr error
	dataRows.Each(func(_ int, tr *goquery.Selection) {
		if parseErr != nil {
			return
		}
		row, skip, err := parseTableRow(tr, seenJsbh)
		if err != nil {
			parseErr = err
			return
		}
		if !skip {
			rows = append(rows, row)
		}
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if len(rows) == 0 {
		return nil, newGlobal(CodeStructure, "dataList 中没有解析到任何教室数据行")
	}
	return rows, nil
}

func weekDataRows(table *goquery.Selection) *goquery.Selection {
	dataRows := table.Find("tbody > tr")
	if dataRows.Length() > 0 {
		return dataRows
	}
	// HTML5 解析器通常会补 tbody；没有时再排除 thead 里的行。
	return table.Find("tr").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.ParentsFiltered("thead").Length() == 0
	})
}

func parseTableRow(tr *goquery.Selection, seenJsbh map[string]bool) (ParsedRow, bool, error) {
	if tr.Find("th").Length() > 0 || tr.Find("td[tdvalue]").Length() > 0 {
		return ParsedRow{}, true, nil
	}
	tds := tr.Find("td")
	if tds.Length() == 0 {
		return ParsedRow{}, true, nil
	}
	if tds.Length() != expectedRowCells {
		return ParsedRow{}, false, newGlobal(CodeStructure,
			"数据行单元格数异常：期望 %d 实际 %d", expectedRowCells, tds.Length())
	}
	row, err := rowFromCells(tr, tds, seenJsbh)
	if err != nil {
		return ParsedRow{}, false, err
	}
	if row.Jsbh == "" {
		return ParsedRow{}, true, nil
	}
	return row, false, nil
}

func rowFromCells(tr *goquery.Selection, tds *goquery.Selection, seenJsbh map[string]bool) (ParsedRow, error) {
	jsbh := extractJsbh(tr, tds)
	if jsbh == "" {
		return ParsedRow{}, newGlobal(CodeStructure, "数据行缺少 jsbh（行属性与 checkbox value 均为空）")
	}
	if seenJsbh[jsbh] {
		return ParsedRow{}, newGlobal(CodeStructure, "同一响应中 jsbh 重复: %s", jsbh)
	}
	seenJsbh[jsbh] = true
	name := NormalizeRoomName(strings.TrimSpace(tds.First().Text()))
	if name == "" {
		return ParsedRow{}, newGlobal(CodeStructure, "jsbh=%s 的房间名称为空", jsbh)
	}
	row := ParsedRow{Jsbh: jsbh, Name: name}
	if err := parseRowCells(tds, &row); err != nil {
		return ParsedRow{}, err
	}
	return row, nil
}

// validateHeaderBlocks 校验表头 tdvalue 集合恰为 7×5 且按天按序等于 5 大节。
func validateHeaderBlocks(blocks []string) error {
	if len(blocks) != numDays*numBlocks {
		return newGlobal(CodeStructure,
			"表头 tdvalue 块数量异常：期望 %d 实际 %d（块=%s）",
			numDays*numBlocks, len(blocks), summarizeBlocks(blocks))
	}
	for d := range numDays {
		group := blocks[d*numBlocks : (d+1)*numBlocks]
		for b, want := range canonicalBlocks {
			if group[b] != want {
				return newGlobal(CodeStructure,
					"表头节次块集合变化：第 %d 天第 %d 块 %q ≠ 期望 %q（完整块=%s）；作息调整需人工确认",
					d+1, b+1, group[b], want, summarizeBlocks(blocks))
			}
		}
	}
	return nil
}

func summarizeBlocks(blocks []string) string {
	if len(blocks) == 0 {
		return "(空)"
	}
	seen := make([]string, 0, len(blocks))
	seenMap := map[string]bool{}
	for _, b := range blocks {
		if !seenMap[b] {
			seenMap[b] = true
			seen = append(seen, b)
		}
	}
	return strings.Join(seen, ",")
}

// extractJsbh 优先取 <tr jsbh=...>，否则取行内 checkbox 的 value。
func extractJsbh(tr *goquery.Selection, tds *goquery.Selection) string {
	if v, ok := tr.Attr("jsbh"); ok {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	if v, ok := tr.Find("input[type=checkbox]").First().Attr("value"); ok {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	// 兜底：首列内任意带 value 的 input（checkbox 命名可能变化）。
	if v, ok := tds.First().Find("input[value]").First().Attr("value"); ok {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// parseRowCells 把第 1 列之后的 35 个状态格填入 row.Blocks。
func parseRowCells(tds *goquery.Selection, row *ParsedRow) error {
	idx := 1
	for d := range numDays {
		for b := range numBlocks {
			if idx >= tds.Length() {
				return newGlobal(CodeStructure, "数据行状态格不足 35 个")
			}
			text := strings.TrimSpace(tds.Eq(idx).Text())
			id, err := mapStatusText(text)
			if err != nil {
				return newGlobal(CodeStructure, "jsbh=%s 出现未知状态文本 %s: %v",
					row.Jsbh, quoteShort(text, 16), err)
			}
			row.Blocks[d][b] = id
			idx++
		}
	}
	return nil
}

// mapStatusText 状态文本 → ID；空文本视为空闲。
// 同一格可能出现多个已知符号（如 "◆Ｊ"），代表多重占用 → 归并为复合占用；
// 任一符号未知时返回错误，禁止发布。
// 优先整词匹配（"空闲""完全空闲"是多字符号），无空白分隔的多符号文本按字符合并。
func mapStatusText(text string) (int, error) {
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return statusEmptyID, nil
	}
	ids := make([]int, 0, len(tokens))
	for _, tok := range tokens {
		if id, ok := statusGlyphToID[tok]; ok {
			ids = append(ids, id)
			continue
		}
		// 无空白分隔的复合符号：整词未收录时按字符拆分判断。
		runes := []rune(tok)
		allKnown := len(runes) > 1
		for _, r := range runes {
			if _, ok := statusGlyphToID[string(r)]; !ok {
				allKnown = false
				break
			}
		}
		if allKnown {
			ids = append(ids, statusCompositeID)
			continue
		}
		return 0, fmt.Errorf("未收录的状态符号 %q", truncateRunes(tok, 4))
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return statusCompositeID, nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// StatusesForDay 把单行某天（day=0..6）的 5 大节状态展开为 01—12 小节映射
// （块内小节状态一致，业务不变量）。
func (row *ParsedRow) StatusesForDay(day int) map[string]int {
	out := map[string]int{}
	for b, block := range canonicalBlocks {
		for _, code := range blockChildCodes[block] {
			out[code] = row.Blocks[day][b]
		}
	}
	return out
}
