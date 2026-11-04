package publish

import (
	"regexp"
	"strconv"
	"strings"
)

// 周次范围原文只认两种写法：一个数字，或两个数字用连接号相连。
//
// 连接号族只有这五个字符（两侧允许空白）：
//   - '-' 半角连字符、'~' 半角波浪号；
//   - '～' 全角波浪号、'—' 全角破折号（em dash）、'–' 短破折号（en dash）。
//
// 前后缀同样收窄到固定几种，不再「任意非数字字符都能带」：
//   - 前缀只允许可选的「第」；
//   - 后缀只允许可选的「周」。
//
// 收窄的理由是「1-16节」这类节次写法和「1-16周」长得一样，放任任意后缀会把
// 节次范围错当成周次范围去派生状态。认不出来就判解析失败、不派生（Requirement 6.3），
// 比猜错安全；真实明细页的取值为「1-16」或「1-16周」（docs/upstream-api.md）。
//
// 用整串匹配而不是 FindAllString：逗号列表（「1,3,5」）不是区间，按区间展开
// 会把没课的那几周也补上状态，同样宁可判解析失败。
var (
	weekRangeSpan   = regexp.MustCompile(`^[[:space:]]*第?[[:space:]]*([0-9]+)[[:space:]]*[-~～—–][[:space:]]*([0-9]+)[[:space:]]*周?[[:space:]]*$`)
	weekRangeSingle = regexp.MustCompile(`^[[:space:]]*第?[[:space:]]*([0-9]+)[[:space:]]*周?[[:space:]]*$`)
)

// parseWeekRange 把占用说明的周次范围原文解析成闭区间 [from, to]。
//
// 解析失败是正常数据：说明不参与派生，也整条丢弃——没有可用的范围就定不了它是否
// 落在窗口内，与 Requirement 3.3「与采集窗口求交为空即丢弃」同一处理（原 spec 6.4）；
// Requirement 6.3 规定的是解析失败时跳过派生、保持该格无状态。丢弃由 addDetails
// 执行并计入 Candidate.DroppedDetails。
// 周次必须从 1 开始且起点不晚于终点；全角数字与其它没见过的写法都不猜。
func parseWeekRange(raw string) (from, to int, ok bool) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return 0, 0, false
	}

	if match := weekRangeSpan.FindStringSubmatch(text); match != nil {
		from, fromErr := strconv.Atoi(match[1])
		to, toErr := strconv.Atoi(match[2])
		if fromErr != nil || toErr != nil || from < 1 || to < from {
			return 0, 0, false
		}
		return from, to, true
	}

	if match := weekRangeSingle.FindStringSubmatch(text); match != nil {
		week, err := strconv.Atoi(match[1])
		if err != nil || week < 1 {
			return 0, 0, false
		}
		return week, week, true
	}

	return 0, 0, false
}
