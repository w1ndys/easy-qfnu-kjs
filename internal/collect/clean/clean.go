// 本文件属于 business 层：把一份教室状态表格清洗成可入库的语义事实。
// 分层沿用 docs/contract/data-format.v2.md：解析 → 房名规范化 → 状态分类 → 12 小节展开。
// 这一层不访问网络、不写数据库，也不保留原始 HTML。

package clean

import (
	"errors"
	"fmt"
	"slices"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// DayCount 是一周的天数，与契约的 7 天一致。
const DayCount = 7

// NodeCount 是一个教学日的小节数，与契约的 12 个小节一致。
const NodeCount = 12

// blocksPerDay 是一个教学日的大节数：表头按它把 35 格切成 7 组。
const blocksPerDay = 5

// blockCellCount 是一行数据里状态格的个数：7 天 × 5 大节。
const blockCellCount = DayCount * blocksPerDay

// dataRowCells 是一行数据应有的 td 个数：一个房间列加 35 个状态格。
const dataRowCells = blockCellCount + 1

// ErrStructure 表示响应结构与契约不符：缺表、行内 td 个数不对、jsbh 取不到或同一响应内重复、
// 展开时轴有大节覆盖不上等。这类问题不能按单格降级，调用方不得把这一份响应当成可用数据。
var ErrStructure = errors.New("教室状态矩阵结构失败")

// ErrAxisChanged 表示表头大节集合或顺序与当前节次轴不一致：整轮停止并告警，不发布，
// 也不允许自动适配。它与单格 unknown 是两回事：unknown 只影响一个格子，不阻断发布。
var ErrAxisChanged = errors.New("表头大节与当前节次轴不一致")

// Cell 是一个格子的清洗结果。
type Cell struct {
	StateKey model.StateKey // 语义状态键，含义由该次发布的 dict_version 决定
	RawText  string         // 上游原文：只有 unknown 与 composite 留档，其余为空
}

// Row 是一间房在一份矩阵里的状态：身份、房名与展开后的 7×12 小节。
type Row struct {
	Jsbh    string                    // 房间唯一身份，来自 tr 属性、行内 checkbox 或首列 input
	Name    string                    // 规范化展示名，例如 F126
	NameRaw string                    // 上游原始房名，含容量片段，例如 F126(90/0)
	Week    [DayCount][NodeCount]Cell // 周一到周日 × 12 小节，第二维按节次轴节点顺序
}

// Matrix 是一份清洗后的教室状态矩阵。
type Matrix struct {
	Blocks []string // 一天内的 5 个大节编码，7 天相同，顺序与节次轴一致
	Rows   []Row    // 数据行，一行一间房
}

// Parse 解析并清洗一份周矩阵或聚合矩阵。
// axis 是当前节次轴（store.AxisNodes 按展示顺序返回），symbols 是当前字典的符号表
// （dict_symbol）：分类规则本身是数据，所以由调用方按版本读出而不是写死在这里。
// 表头与轴不一致返回 ErrAxisChanged，其余结构问题返回 ErrStructure。
func Parse(body []byte, axis []model.AxisNode, symbols []model.DictSymbol) (Matrix, error) {
	blocks, err := axisBlockCodes(axis)
	if err != nil {
		return Matrix{}, err
	}
	classifier, err := newClassifier(symbols)
	if err != nil {
		return Matrix{}, err
	}

	header, rows, err := parseTable(body, blocks)
	if err != nil {
		return Matrix{}, err
	}

	matrix := Matrix{Blocks: blocks, Rows: make([]Row, 0, len(rows))}
	seen := make(map[string]bool, len(rows))
	for _, raw := range rows {
		// 同一响应里同一个 jsbh 出现两次，拿不到稳定的房间身份，不能挑一行留下
		if seen[raw.jsbh] {
			return Matrix{}, fmt.Errorf("%w: 同一响应内重复 jsbh %s", ErrStructure, raw.jsbh)
		}
		seen[raw.jsbh] = true

		row, err := buildRow(raw, header, axis, classifier)
		if err != nil {
			return Matrix{}, err
		}
		matrix.Rows = append(matrix.Rows, row)
	}
	return matrix, nil
}

// axisBlockCodes 校验节次轴并返回它的大节编码，顺序即表头应有的顺序。
func axisBlockCodes(axis []model.AxisNode) ([]string, error) {
	// 12 个小节是契约固定的展开宽度，轴对不上就展开不出 7×12
	if len(axis) != NodeCount {
		return nil, fmt.Errorf("%w: 节次轴有 %d 个小节，期望 %d", ErrStructure, len(axis), NodeCount)
	}

	codes := make([]string, 0, blocksPerDay)
	for index, node := range axis {
		// 轴必须按展示顺序传入，乱序会让块序校验得出错误结论
		if node.Ordinal != index+1 {
			return nil, fmt.Errorf("%w: 节次轴第 %d 个节点的 ordinal 是 %d", ErrStructure, index+1, node.Ordinal)
		}
		// 同一个大节的小节连续出现时只记一次编码
		if len(codes) > 0 && codes[len(codes)-1] == node.Block {
			continue
		}
		codes = append(codes, node.Block)
	}
	// 一天切成几列由大节个数决定，5 个之外说明轴版本与这份契约不一致
	if len(codes) != blocksPerDay {
		return nil, fmt.Errorf("%w: 节次轴有 %d 个大节，期望 %d", ErrStructure, len(codes), blocksPerDay)
	}
	return codes, nil
}

// buildRow 把一行的 35 个状态格按天清洗并展开成 7×12 小节。
func buildRow(raw rawRow, header []string, axis []model.AxisNode, classifier classifier) (Row, error) {
	row := Row{Jsbh: raw.jsbh, Name: normalizeName(raw.nameRaw), NameRaw: raw.nameRaw}

	for dayIndex := 0; dayIndex < DayCount; dayIndex++ {
		dayHeader := header[dayIndex*blocksPerDay : (dayIndex+1)*blocksPerDay]
		cells := make([]Cell, len(dayHeader))
		for blockIndex := range dayHeader {
			// 35 格按天分组，组内顺序与表头一致，位置就是列号
			cells[blockIndex] = classifier.classify(raw.cells[dayIndex*blocksPerDay+blockIndex])
		}

		day, err := expandDay(dayHeader, cells, axis)
		if err != nil {
			return Row{}, err
		}
		row.Week[dayIndex] = day
	}
	return row, nil
}

// expandDay 把一天的 5 个大节格展开成 12 个小节：轴上每个小节按自己的大节编码认领状态。
// 同一个大节的各节点因此拿到同一格状态，「同一大节内各节点状态相同」由构造保证；
// 轴上某个小节的大节在表头里找不到，说明轴与这份响应不是一套，按结构失败返回。
func expandDay(dayHeader []string, cells []Cell, axis []model.AxisNode) ([NodeCount]Cell, error) {
	var day [NodeCount]Cell

	for nodeIndex, node := range axis {
		blockIndex := slices.Index(dayHeader, node.Block)
		// 表头里没有这个小节的大节，这一列的状态没有来源
		if blockIndex < 0 {
			return day, fmt.Errorf("%w: 小节 %s 的大节 %s 不在表头里", ErrStructure, node.Node, node.Block)
		}
		day[nodeIndex] = cells[blockIndex]
	}
	return day, nil
}
