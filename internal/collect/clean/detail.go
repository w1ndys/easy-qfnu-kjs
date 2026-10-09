// 本文件属于 business 层：解析占用明细弹窗（GET /jsxsd/kbxx/jsjy_jszyqk）里的说明。
// 弹窗正文是一张按「标签格 + 空格 + 值格」三列重复的表格，同一格可能有多条记录，按行顺序输出。
// 这一层只取教室状态、课程、周次范围与时间标志；申请人、任课教师、备注与原始 HTML 一律不留档。
// 依据 specs/collector-full-sync/requirements.md 的 4.2 与 docs/decisions/2026-10-09-saturday-full-sync.md。
// 仓库里没有真实的 jsjy_jszyqk 弹窗样本。下面按探测记录假定 table#tab1、标签格后隔一格是值、以「教室状态」拆多条记录。
// 真实页面若多一层嵌套或值不在最后一格，ParseCellDetail 会返回 ErrStructure。修解析时改本文件，不要把缺字段当成空闲。

package clean

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// detailTablePattern 定位占用明细弹窗里的表格并取出它的内部 HTML。
var detailTablePattern = regexp.MustCompile(`(?is)<table[^>]*\bid\s*=\s*["']?tab1["']?[^>]*>(.*?)</table>`)

// 弹窗里的字段名：只有这四个字段进库。
const (
	detailLabelState     = "教室状态" // 教室状态：与格子符号对应的中文状态名
	detailLabelCourse    = "课程"   // 课程：课程名；考试科目出现在课程字段时同样落在这里
	detailLabelWeekRange = "周次"   // 周次：该占用覆盖的周次范围，例如 1-16
	detailLabelTimeFlag  = "时间标志" // 时间标志：单双周标志
)

// detailLabels 是占用明细里允许入库的字段名：其余字段（教室、时间、节次、备注、申请人、任课教师）在解析时丢掉。
var detailLabels = []string{detailLabelState, detailLabelCourse, detailLabelWeekRange, detailLabelTimeFlag}

// Detail 是一条占用说明：只保留契约允许入库的四项。
type Detail struct {
	StateKey  model.StateKey // 教室状态语义键，由字典的标签映射得到
	Course    string         // 课程字段原文；考试科目出现在课程字段时同样落在这里
	WeekRange string         // 周次范围原文，例如 1-16；解析交给派生逻辑
	TimeFlag  string         // 时间标志原文，例如 单周 / 双周
}

// ParseCellDetail 解析一格占用明细的弹窗页，按行顺序返回它的全部说明。
// states 是当前版本的字典（store.DictStates 读到的 dict_state），用它把中文状态名换成语义键。
// 没有占用记录时页面没有数据行，这时返回空列表而不是错误；连表都不见了才按结构失败返回。
func ParseCellDetail(body []byte, states []model.DictState) ([]Detail, error) {
	// 没有字典就认不出教室状态，这是我们的数据问题，不能把状态全判成未知
	if len(states) == 0 {
		return nil, fmt.Errorf("%w: 状态字典为空", ErrStructure)
	}
	table, err := findDetailTable(body)
	if err != nil {
		return nil, err
	}

	labels := stateKeysByLabel(states)
	details := make([]Detail, 0, 2)
	// 每条记录以「教室状态」开头：见到下一个「教室状态」就把上一条收下
	var current *Detail
	for _, texts := range detailRows(table) {
		label, value := rowField(texts)
		// 不是要存的四个字段就整行跳过，申请人、任课教师、备注都不会进结果
		if !slices.Contains(detailLabels, label) {
			continue
		}
		// 「教室状态」是每条记录的第一个字段，见到它就另起一条
		if label == detailLabelState {
			if current != nil {
				details = append(details, *current)
			}
			current = &Detail{StateKey: stateKeyOf(labels, value)}
			continue
		}
		// 还没见到教室状态就先出现别的字段，说明这一行不属于任何一条记录
		if current == nil {
			continue
		}
		applyDetailField(current, label, value)
	}
	// 循环结束后最后一条记录还没收下
	if current != nil {
		details = append(details, *current)
	}
	return details, nil
}

// findDetailTable 取出弹窗表格 table#tab1 的内部 HTML；没有这张表说明响应不是占用明细页。
func findDetailTable(body []byte) (string, error) {
	match := detailTablePattern.FindSubmatch(body)
	// 弹窗页的表 id 固定，找不到就是页面结构变了，不能当成「这一格没有占用」
	if match == nil {
		return "", fmt.Errorf("%w: 响应里没有 table#tab1", ErrStructure)
	}
	return string(match[1]), nil
}

// detailRows 把表格拆成数据行，每行是若干格的可见文本。
func detailRows(table string) [][]string {
	rows := make([][]string, 0, 16)
	for _, match := range rowPattern.FindAllStringSubmatch(table, -1) {
		cells := splitCells(match[2])
		// 没有格子的行（空行、分隔行）不参与解析
		if len(cells) == 0 {
			continue
		}
		texts := make([]string, 0, len(cells))
		for _, cell := range cells {
			texts = append(texts, cellText(cell.inner))
		}
		rows = append(rows, texts)
	}
	return rows
}

// rowField 取一行的字段名与字段值：字段名在第一格，值在最后一格（三列写法里值在第三列）。
func rowField(texts []string) (string, string) {
	// 只有一格的行走不到字段匹配，直接当作没有值
	if len(texts) == 1 {
		return normalizeLabel(texts[0]), ""
	}
	return normalizeLabel(texts[0]), texts[len(texts)-1]
}

// normalizeLabel 去掉字段名里的全部空白：「申 请 人」这类写法必须与「申请人」等价比较。
func normalizeLabel(label string) string {
	return strings.Join(strings.Fields(label), "")
}

// stateKeysByLabel 建立「字典标签 → 状态键」的映射：弹窗给的是中文状态名，库里存的是语义键。
func stateKeysByLabel(states []model.DictState) map[string]model.StateKey {
	keys := make(map[string]model.StateKey, len(states))
	for _, state := range states {
		keys[normalizeLabel(state.Label)] = state.StateKey
	}
	return keys
}

// stateKeyOf 按标签取状态键：标签没收录时按未知处理，未知不可用，也不阻断发布。
func stateKeyOf(keys map[string]model.StateKey, label string) model.StateKey {
	key, ok := keys[normalizeLabel(label)]
	// 字典里没有这个状态名，只能记成未知，不猜它属于哪一种占用
	if !ok {
		return model.StateUnknown
	}
	return key
}

// applyDetailField 把一行的值填进当前说明：这里只认课程、周次与时间标志三项。
func applyDetailField(detail *Detail, label, value string) {
	switch label {
	// 课程字段同时承载考试科目：出现在这个字段里就照样存下来
	case detailLabelCourse:
		detail.Course = value
	// 周次范围存原文，能不能解析成周次由派生逻辑判断
	case detailLabelWeekRange:
		detail.WeekRange = value
	// 时间标志存原文，单双周由派生逻辑判断
	case detailLabelTimeFlag:
		detail.TimeFlag = value
	}
}
