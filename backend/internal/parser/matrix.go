package parser

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// matrixWeekdays 是矩阵固定的 7 天：每栋楼每周一次请求覆盖周一至周日。
	matrixWeekdays = 7
	// matrixTableID 是目标表的选择器。必须按 id 精确匹配：真实页面里
	// table#dataListHeader 排在目标表之前，前缀匹配会选错表（docs/pitfalls.md 第 3 节）。
	matrixTableID = "table#dataList"
	// statusCellAttr 是表头状态格上的大节编码属性，35 个状态格都带、房名列不带。
	statusCellAttr = "tdvalue"
	// roomIDAttr 是数据行上的房号属性，优先于行内复选框与首列输入框。
	roomIDAttr = "jsbh"
	// clippedRunes 是错误信息里页面派生值的截断长度（rune 计）。
	// 房号与 tdvalue 都直接来自页面属性，长度不受控，整段带进错误会顺着日志散出去。
	clippedRunes = 32
)

// clipPageValue 把页面派生值截断到 32 个 rune 再内联进错误信息：
// 保留开头的可辨识片段（足够定位是哪个值），尾部用省略号标明被截断。
// 按 rune 而不是字节切，避免把多字节字符切成非法 UTF-8。
// 行号是 tr 计数的整数，长度天然有界，不经过这里。
func clipPageValue(value string) string {
	runes := []rune(value)
	if len(runes) <= clippedRunes {
		return value
	}
	return string(runes[:clippedRunes]) + "…"
}

// Rows 解析某楼某周的矩阵响应，返回该响应里所有教室及其逐格状态原文。
//
// 目标表按 id 精确匹配，避开同名前缀的 table#dataListHeader（Requirement 2.1）；
// 表头必须是 1 个房名列加 7×大节数个状态格，且状态格的 tdvalue 集合与顺序与 axis 完全一致，
// 否则判结构失败（Requirement 2.2）；每个数据行必须与表头等宽（Requirement 2.3）；
// 房号按「行属性 → 行内复选框 → 首列输入框」取值（Requirement 2.5）；
// 同一响应内出现重复房号判结构失败（Requirement 2.4）。
//
// 返回的教室保持页面行顺序，格子按「每天重复 5 大节」的表头顺序展开为
// 星期 1..7 × 大节 1..N；只有表头没有数据行时返回空切片与 nil 错误。
func Rows(body []byte, axis []model.AxisNode) ([]model.MatrixRoom, error) {
	blocks, err := axisBlocks(axis)
	if err != nil {
		return nil, err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: 页面无法按 HTML 解析", ErrMatrixStructureInvalid)
	}

	table := doc.Find(matrixTableID).First()
	if table.Length() == 0 {
		return nil, fmt.Errorf("%w: 找不到 %s", ErrMatrixStructureInvalid, matrixTableID)
	}

	allRows := table.Find("tr")
	if allRows.Length() == 0 {
		return nil, fmt.Errorf("%w: 目标表没有任何 tr", ErrMatrixStructureInvalid)
	}

	if err := checkHeader(allRows.First(), blocks); err != nil {
		return nil, err
	}

	// 表头已校验，行宽与格子位置都可以由轴推出：1 个房名列 + 7×大节数个状态格。
	wantCells := 1 + matrixWeekdays*len(blocks)
	rooms := make([]model.MatrixRoom, 0, allRows.Length()-1)
	seen := make(map[string]struct{}, allRows.Length()-1)
	var rowErr error
	allRows.Slice(1, allRows.Length()).EachWithBreak(func(index int, tr *goquery.Selection) bool {
		// 行号从 1 开始，加 2 是因为第 1 行是表头；报错时按人读的行号指位置。
		rowNo := index + 2
		cells := tr.ChildrenFiltered("td")
		if cells.Length() != wantCells {
			rowErr = fmt.Errorf("%w: 第 %d 行应有 %d 个单元格（1 个房名列 + %d 个状态格），实际 %d",
				ErrMatrixStructureInvalid, rowNo, wantCells, wantCells-1, cells.Length())
			return false
		}
		roomID := roomIdentity(tr, cells)
		if roomID == "" {
			rowErr = fmt.Errorf("%w: 第 %d 行取不到房号（行属性、行内复选框、首列输入框都为空）",
				ErrMatrixStructureInvalid, rowNo)
			return false
		}
		if _, ok := seen[roomID]; ok {
			rowErr = fmt.Errorf("%w: 同一响应内房号 %q 重复", ErrMatrixStructureInvalid, clipPageValue(roomID))
			return false
		}
		seen[roomID] = struct{}{}

		room := model.MatrixRoom{
			RoomID:  roomID,
			NameRaw: strings.TrimSpace(cells.First().Text()),
			Cells:   make([]model.MatrixCell, 0, wantCells-1),
		}
		for i := 0; i < wantCells-1; i++ {
			room.Cells = append(room.Cells, model.MatrixCell{
				Weekday: i/len(blocks) + 1,
				Block:   blocks[i%len(blocks)],
				Raw:     strings.TrimSpace(cells.Eq(i + 1).Text()),
			})
		}
		rooms = append(rooms, room)
		return true
	})
	if rowErr != nil {
		return nil, rowErr
	}
	return rooms, nil
}

// checkHeader 校验表头与轴一致：行宽为 1+35，且 35 个状态格的 tdvalue 集合与顺序
// 都必须等于「每天重复 5 大节」的展开结果。位置逐个比对同时覆盖集合与顺序：
// 少一个、多一个、漏属性、顺序调换都会在第一个不符的位置失败（Requirement 2.2）。
func checkHeader(header *goquery.Selection, blocks []string) error {
	cells := header.ChildrenFiltered("td")
	wantStatus := matrixWeekdays * len(blocks)
	if cells.Length() != wantStatus+1 {
		return fmt.Errorf("%w: 表头应有 %d 个 td（1 个房名列 + %d 个状态格），实际 %d",
			ErrMatrixStructureInvalid, wantStatus+1, wantStatus, cells.Length())
	}
	// 房名列必须不带 tdvalue：带了说明第一个状态格被挤到别处，35 个状态格已错位。
	if _, ok := cells.First().Attr(statusCellAttr); ok {
		return fmt.Errorf("%w: 表头房名列不应带 %s", ErrMatrixStructureInvalid, statusCellAttr)
	}
	for i := 0; i < wantStatus; i++ {
		want := blocks[i%len(blocks)]
		got, ok := cells.Eq(i + 1).Attr(statusCellAttr)
		if !ok {
			return fmt.Errorf("%w: 表头第 %d 个状态格缺少 %s，期望 %q",
				ErrMatrixStructureInvalid, i+1, statusCellAttr, want)
		}
		if got != want {
			return fmt.Errorf("%w: 表头第 %d 个状态格期望 %q，实际 %q",
				ErrMatrixStructureInvalid, i+1, want, clipPageValue(got))
		}
	}
	return nil
}

// roomIdentity 按固定顺序取房号：行属性 jsbh → 行内复选框的 value → 首列第一个 input 的 value。
// 顺序不能颠倒：行属性是权威身份，复选框与首列输入框只是页面给它补的入口（Requirement 2.5）；
// 三处都取不到时返回空串，由调用方判结构失败，不要退回用房名猜。
func roomIdentity(tr *goquery.Selection, cells *goquery.Selection) string {
	if id := strings.TrimSpace(tr.AttrOr(roomIDAttr, "")); id != "" {
		return id
	}
	if id := strings.TrimSpace(tr.Find(`input[type="checkbox"]`).First().AttrOr("value", "")); id != "" {
		return id
	}
	return strings.TrimSpace(cells.First().Find("input").First().AttrOr("value", ""))
}

// axisBlocks 把 12 个小节折叠成按大节顺序排列的编码序列（当前种子是 5 个大节）。
// 顺序取自 block_ordinal，不依赖传入切片顺序；同一序号对应多个编码、
// 或序号序列出现空洞都判轴不可用，因为表头无法再按「每天重复大节」解释（Requirement 2.2）。
// 空洞按位置逐个查：先按最大序号建数组，再从头找第一个空位，报「缺少第 N 个大节」。
// 不能先按 len(axis) 建数组判断「序号超出」——{1,3} 会被误报成越界，而真实问题是缺第 2 个大节。
func axisBlocks(axis []model.AxisNode) ([]string, error) {
	if len(axis) == 0 {
		return nil, fmt.Errorf("%w: 节次轴没有任何节点", ErrAxisInvalid)
	}
	codeByOrdinal := make(map[int]string, len(axis))
	maxOrdinal := 0
	for _, node := range axis {
		if node.Block == "" || node.BlockOrdinal < 1 {
			return nil, fmt.Errorf("%w: 节点 %q 缺少大节编码或大节序号", ErrAxisInvalid, node.Node)
		}
		if code, ok := codeByOrdinal[node.BlockOrdinal]; ok && code != node.Block {
			return nil, fmt.Errorf("%w: 大节序号 %d 同时对应 %q 与 %q",
				ErrAxisInvalid, node.BlockOrdinal, code, node.Block)
		}
		codeByOrdinal[node.BlockOrdinal] = node.Block
		if node.BlockOrdinal > maxOrdinal {
			maxOrdinal = node.BlockOrdinal
		}
	}
	// 连续 1..N 时最大序号不超过节点数（每个序号至少一个节点）；超出说明前面必有空洞，
	// 用节点数封顶即可定位到第一个空洞，也避免坏轴塞一个巨大序号把数组撑爆。
	size := maxOrdinal
	if size > len(axis) {
		size = len(axis)
	}
	blocks := make([]string, size)
	for ordinal, code := range codeByOrdinal {
		if ordinal <= size {
			blocks[ordinal-1] = code
		}
	}
	for i, code := range blocks {
		if code == "" {
			return nil, fmt.Errorf("%w: 大节序号不连续，缺少第 %d 个大节",
				ErrAxisInvalid, i+1)
		}
	}
	return blocks, nil
}
