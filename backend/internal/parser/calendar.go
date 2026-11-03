package parser

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// termNumberPattern 匹配学期编号 YYYY-YYYY-N（docs/pitfalls.md 第 2 节）。
// 正则只用于纯文本，不用于抠 HTML 结构（docs/pitfalls.md 第 10 节）。
var termNumberPattern = regexp.MustCompile(`[0-9]{4}-[0-9]{4}-[0-9]+`)

// weekTextPattern 匹配「第 X 周 / Y 周」，兼容「共」与任意空白。
// 输入是 visibleText 或 fullText 归一后的文本：&nbsp;（U+00A0）与全角空格（U+3000）
// 都已被换成半角空格，所以用 \s 即可（Go 的 \s 只认 ASCII 空白）。
var weekTextPattern = regexp.MustCompile(`第\s*([0-9]+)\s*周\s*/\s*共?\s*([0-9]+)\s*周`)

// kbjcmsidInput 是学期页里节次模式 id 的元素 id/name，明细请求必填且不能写死
// （docs/upstream-api.md 第 3 节）。
const kbjcmsidInput = "kbjcmsid"

// maxSemesterWeeks 是本包的业务护栏：一年最多 52 周，页面给出的 Y 超过它只能是
// 页面换了或抓错了，按解析失败处理，不让一个大到离谱的 Y 流进窗口计算。
// 这个上限不来自上游文档或需求，是用户已拍板为 52 的本地判断。
const maxSemesterWeeks = 52

// TermText 从学期页正文解析学期编号与 kbjcmsid。
//
// 学期编号只取正文可见文本里第一个形如 YYYY-YYYY-N 的片段：head 里的标题、脚本/样式
// 里的旧学期编号不计入，历史学期下拉（select/option）无论排在正文当前学期之前还是
// 之后都整段跳过，所以第一个命中就是正文当前学期（docs/upstream-api.md 第 3 节）。
// 正文里一个学期编号都没有就直接失败，不去翻下拉的 selected option 或隐藏字段——
// 表单当前值可能就是旧学期，取不到就失败、不猜学期（docs/pitfalls.md 第 2 节）。
// kbjcmsid 取 id 或 name 等于 kbjcmsid 的 input value（属性顺序、引号风格、大小写
// 都不敏感）：空 value 跳过，剩下的非空取值必须完全一致；同页出现两个不同取值时
// 同样失败，不静默取首个，见 uniqueInputValue。
// 两者任一取不到或冲突返回 ErrTermTextInvalid，不猜学期的同时也不猜节次模式；
// 错误信息只描述缺了哪一项或哪一项冲突，不带页面正文（docs/pitfalls.md 第 8 节，
// 旧 spec specs/2026-10-27-classroom-availability-rewrite Requirement 3.1）。
func TermText(body []byte) (string, string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("%w: 页面无法按 HTML 解析", ErrTermTextInvalid)
	}

	term := termNumberPattern.FindString(visibleText(doc))
	if term == "" {
		return "", "", fmt.Errorf("%w: 缺少学期编号", ErrTermTextInvalid)
	}

	kbjcmsid, found, conflict := uniqueInputValue(doc, kbjcmsidInput)
	if conflict {
		return "", "", fmt.Errorf("%w: 同页 %s 存在多个不同取值", ErrTermTextInvalid, kbjcmsidInput)
	}
	if !found {
		return "", "", fmt.Errorf("%w: 缺少 %s", ErrTermTextInvalid, kbjcmsidInput)
	}

	return term, kbjcmsid, nil
}

// WeekText 从周历页正文解析当前教学周 X 与学期总周数 Y。
//
// 兼容「第5周 /16周」与「第 5 周 / 共 16 周」，以及 HTML 的 &nbsp;、全角与半角空白：
// 匹配的是归一后的文本，不是原始 HTML。
// 先按 visibleText 匹配可见正文；upstream 文档记录周次句在响应正文里
// （docs/upstream-api.md 第 4 节、docs/pitfalls.md 第 2 节），另一次样本只在脚本里
// （docs/decisions/2026-10-09-term-week-from-one-sample.md）；两条都覆盖，所以可见
// 正文未命中时再对含 script/style 的整页文本匹配一次。
// 文本缺失、Y 超过业务上限 52（本包护栏，一年最多 52 周）或 X、Y 不满足 1 ≤ X ≤ Y
// 返回 ErrWeekTextInvalid，不猜周次（docs/upstream-api.md 第 4 节）。
// 上限只约束总周数：X ≤ Y ≤ 52 时 X 自然被一并约束。
// 同页出现多次时取第一次命中，与页面主展示顺序一致。
func WeekText(body []byte) (int, int, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: 页面无法按 HTML 解析", ErrWeekTextInvalid)
	}

	match := weekTextPattern.FindStringSubmatch(visibleText(doc))
	if match == nil {
		match = weekTextPattern.FindStringSubmatch(fullText(doc))
	}
	if match == nil {
		return 0, 0, fmt.Errorf("%w: 缺少「第 X 周 / Y 周」文本", ErrWeekTextInvalid)
	}

	week, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: 当前周 %q 超出整数范围", ErrWeekTextInvalid, clipPageValue(match[1]))
	}
	total, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, 0, fmt.Errorf("%w: 总周数 %q 超出整数范围", ErrWeekTextInvalid, clipPageValue(match[2]))
	}
	if total > maxSemesterWeeks {
		// 只报上限，不带页面给出的 Y：Y 是页面派生值，长度与内容都不受控
		// （docs/pitfalls.md 第 8 节）。
		return 0, 0, fmt.Errorf("%w: 总周数超过业务上限 %d 周", ErrWeekTextInvalid, maxSemesterWeeks)
	}
	if week < 1 || week > total {
		return 0, 0, fmt.Errorf("%w: 第 %d 周 / %d 周不满足 1 ≤ X ≤ Y", ErrWeekTextInvalid, week, total)
	}

	return week, total, nil
}

// nonVisibleTags 是不算可见正文的标签黑名单：head 里的标题，脚本与样式，模板、
// 内嵌框架，以及下拉表单元素（历史学期的 option 会排在正文当前学期前后）。
var nonVisibleTags = map[string]struct{}{
	"head":     {},
	"title":    {},
	"script":   {},
	"style":    {},
	"noscript": {},
	"template": {},
	"iframe":   {},
	"select":   {},
	"option":   {},
}

// visibleText 取按标签黑名单近似出的可见文本：按文档顺序拼接文本节点，并把所有
// unicode.IsSpace 字符归一成一个半角空格。这是按标签黑名单近似的「看得见的正文」，
// 不做 CSS 可见性判定（display:none、visibility:hidden 之类一概不看）。
// 跳过黑名单的原因：goquery 的 Text 会带上非正文文本——head 里的 <title>、脚本与
// 样式，以及历史学期下拉（select/option）——它们可能先出现旧学期编号或旧周次，
// 不跳过就会被当成当前值。
// 每个文本节点之间留一个空格再归一：HTML 实体 &nbsp; 解析出来是 U+00A0，全角空格
// 是 U+3000，而 Go 正则的 \s 只认 ASCII 空白；同时相邻元素的文本直接拼起来会把
// 「2026-2027-1」和后面的「2025」黏成形如 YYYY-YYYY-N 的假编号
// （docs/pitfalls.md 第 2 节、第 10 节）。
func visibleText(doc *goquery.Document) string {
	return pageText(doc, nonVisibleTags)
}

// fullText 取整页文本（含 head、script/style 与下拉），供可见正文未命中时兜底匹配。
func fullText(doc *goquery.Document) string {
	return pageText(doc, nil)
}

// pageText 按文档顺序收集文本节点，每个节点后补一个空格当分隔；skip 里有的标签
// 整棵子树跳过，skip 为 nil 时不过滤。
func pageText(doc *goquery.Document, skip map[string]struct{}) string {
	var b strings.Builder
	collectText(&b, doc.Selection, skip)
	return normalizeSpaces(b.String())
}

// collectText 递归进入子节点收集文本；黑名单标签整棵子树跳过，其它元素继续深入。
func collectText(b *strings.Builder, s *goquery.Selection, skip map[string]struct{}) {
	s.Contents().Each(func(_ int, node *goquery.Selection) {
		switch name := goquery.NodeName(node); name {
		case "#text":
			b.WriteString(node.Text())
			b.WriteByte(' ')
		default:
			if _, skipped := skip[name]; skipped {
				return
			}
			collectText(b, node, skip)
		}
	})
}

// normalizeSpaces 把连续空白压成一个半角空格，并去掉首尾空白。
// 按 unicode.IsSpace 判定，所以 &nbsp;（U+00A0）、全角空格（U+3000）与换行都算空白。
func normalizeSpaces(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// uniqueInputValue 汇总 id 或 name 等于 want 的 input 的 value。
// HTML 的 id/name 大小写不敏感，比较前统一转小写；value 去掉首尾空白，空值跳过。
// 与 upstream 的最小提取同一口径：空 value 不算取到。
// 剩下的非空取值必须完全一致——同页出现两个不同取值时返回 conflict，由调用方判失败，
// 不按文档顺序静默取首个：页面自己都没统一时，不替它裁决哪个生效
// （与学期文本同一口径，docs/pitfalls.md 第 2 节「取不到就失败，不猜」）。
func uniqueInputValue(doc *goquery.Document, want string) (value string, found, conflict bool) {
	doc.Find("input").Each(func(_ int, input *goquery.Selection) {
		id := strings.ToLower(input.AttrOr("id", ""))
		name := strings.ToLower(input.AttrOr("name", ""))
		if id != want && name != want {
			return
		}
		trimmed := strings.TrimSpace(input.AttrOr("value", ""))
		if trimmed == "" {
			return
		}
		if !found {
			value, found = trimmed, true
			return
		}
		if trimmed != value {
			conflict = true
		}
	})
	return value, found, conflict
}
