// 本文件属于 business 层：解析占用明细弹窗（GET /jsxsd/kbxx/jsjy_jszyqk）里的说明。
// 真实弹窗里 table#tab1 只是标题表；字段行在页面下半部的嵌套表格里，
// 每行固定三列「标签 / 空列 / 值」，标签带全角冒号，同一格可能有多条记录。
// 这一层只取教室状态、课程、周次范围与时间标志；申请人、任课教师、备注与原始 HTML 一律不留档。
// 用 goquery 扫描整页的 tr，再按三列与标签过滤，不依赖字段行所在表格的 id。
// 依据 specs/collector-full-sync/requirements.md 的 4.2 与 docs/decisions/2026-10-09-saturday-full-sync.md。

package clean

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/PuerkitoBio/goquery"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

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
// 没有占用记录时页面没有字段行，这时返回空列表而不是错误；连标题表都不见了才按结构失败返回。
func ParseCellDetail(body []byte, states []model.DictState) ([]Detail, error) {
	// 没有字典就认不出教室状态，这是我们的数据问题，不能把状态全判成未知
	if len(states) == 0 {
		return nil, fmt.Errorf("%w: 状态字典为空", ErrStructure)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	// 正文完全不是 HTML 时解析器会报错，按结构失败处理
	if err != nil {
		return nil, fmt.Errorf("%w: 解析占用明细 HTML 失败: %v", ErrStructure, err)
	}
	// 标题表在真实页面和空弹窗里都存在；没有它说明响应不是占用明细页
	if doc.Find("table#tab1").Length() == 0 {
		return nil, fmt.Errorf("%w: 响应里没有 table#tab1", ErrStructure)
	}

	labels := stateKeysByLabel(states)
	details := make([]Detail, 0, 2)
	// 每条记录以「教室状态」开头：见到下一个「教室状态」就把上一条收下
	var current *Detail
	doc.Find("tr").Each(func(_ int, row *goquery.Selection) {
		cells := row.ChildrenFiltered("td")
		// 字段行固定三列：标签、空列、值；其它行（标题、嵌套布局）一律跳过
		if cells.Length() != 3 {
			return
		}
		label := normalizeLabel(textOf(cells.First()))
		// 不是要存的四个字段就整行跳过，申请人、任课教师、备注都不会进结果
		if !slices.Contains(detailLabels, label) {
			return
		}
		value := textOf(cells.Last())
		// 「教室状态」是每条记录的第一个字段，见到它就另起一条
		if label == detailLabelState {
			if current != nil {
				details = append(details, *current)
			}
			current = &Detail{StateKey: stateKeyOf(labels, value)}
			return
		}
		// 还没见到教室状态就先出现别的字段，说明这一行不属于任何一条记录
		if current == nil {
			return
		}
		applyDetailField(current, label, value)
	})
	// 循环结束后最后一条记录还没收下
	if current != nil {
		details = append(details, *current)
	}
	return details, nil
}

// normalizeLabel 去掉标签里的全部空白与末尾冒号：
// 真实页面写作「教室状态：」「申 请 人：」，去掉后才能与字段名等价比较。
func normalizeLabel(label string) string {
	compact := strings.Join(strings.Fields(label), "")
	// 全角冒号与半角冒号都去掉，避免同一字段因标点写法不同被漏掉
	compact = strings.TrimSuffix(compact, "：")
	return strings.TrimSuffix(compact, ":")
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
