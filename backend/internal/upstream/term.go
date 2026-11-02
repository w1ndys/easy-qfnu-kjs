package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 学期页与周历页的端点，来自 docs/upstream-api.md 第 3、4 节。
const (
	termPagePath     = "/jsxsd/kbxx/jsjy_query"                 // 学期页：学期编号 + kbjcmsid
	weekCalendarPath = "/jsxsd/framework/main_index_loadkb.jsp" // 周历页：第 X 周 / Y 周
	kbjcmsidInputID  = "kbjcmsid"                               // 节次模式 id 的元素 id/name
)

// businessZone 是业务时区。固定 +08:00 与 query 包同一口径：2026 年之后没有夏令时，
// 也免去容器缺 tzdata 时 LoadLocation 失败的分支。
var businessZone = time.FixedZone("Asia/Shanghai", 8*60*60)

var (
	// termNumberPattern 匹配学期编号 YYYY-YYYY-N（docs/pitfalls.md 第 2 节）。
	termNumberPattern = regexp.MustCompile(`[0-9]{4}-[0-9]{4}-[0-9]+`)
	// weekTextPattern 匹配「第 X 周 / Y 周」；兼容“共”与任意空白（docs/upstream-api.md 第 4 节）。
	weekTextPattern = regexp.MustCompile(`第\s*([0-9]+)\s*周\s*/\s*共?\s*([0-9]+)\s*周`)
	// inputTagPattern 只切出开始标签，供 kbjcmsid 的属性提取使用。
	inputTagPattern = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	// attrPattern 逐个取「名=值」，双引号、单引号与不加引号都接受。
	attrPattern = regexp.MustCompile(`(?is)([a-z_][a-z0-9_.:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// 包内错误原因：只在包内用于错误分类与测试断言，对外统一由 model 哨兵暴露。
var (
	errTermNotFound     = errors.New("学期页缺少学期编号")
	errKBJCMSIDNotFound = errors.New("学期页缺少 kbjcmsid")
	errWeekTextNotFound = errors.New("周历页缺少周次文本")
	errWeekOutOfRange   = errors.New("周次不满足 1 ≤ X ≤ Y")
)

// TermSnapshot 是学期页的最小提取结果。
// Raw 保存原始正文，交给 parser（tasklist 4.3）做权威解析；本包只做取学期编号与
// kbjcmsid 所必需的最小提取，不解析页面结构。
type TermSnapshot struct {
	Term     string // 学期编号，如 2026-2027-1
	KBJCMSID string // 节次模式 id，明细请求必填，每次登录/学期可能不同
	Raw      string // 学期页原始正文，只给 parser，不落库、不写日志（CP9）
}

// WeekCalendar 是周历页的最小提取结果：X 与 Y 已按 1 ≤ X ≤ Y 校验。
type WeekCalendar struct {
	Week       int    // 当前教学周 X
	TotalWeeks int    // 学期总周数 Y
	Raw        string // 周历页原始正文，只给 parser，不落库、不写日志（CP9）
}

// FetchTerm 读取学期页并提取学期编号与 kbjcmsid（Requirement 3.1）。
//
// 请求走 doWithRelogin：会话失效只重登录一次，随后重跑本次请求；
// 取不到学期编号或 kbjcmsid 返回 model.ErrUpstreamCalendarInvalid，不猜学期，
// 由上层结束本轮并保持学期日历不变（Requirement 3.4）。
func (c *Client) FetchTerm(ctx context.Context) (*TermSnapshot, error) {
	resp, err := c.doWithRelogin(ctx, request{method: http.MethodGet, path: termPagePath})
	if err != nil {
		return nil, fmt.Errorf("读取学期页: %w", err)
	}
	term, kbjcmsid, err := parseTermPage(resp.Body)
	if err != nil {
		return nil, err
	}
	return &TermSnapshot{Term: term, KBJCMSID: kbjcmsid, Raw: string(resp.Body)}, nil
}

// FetchWeekCalendar 读取周历页并解析「第 X 周 / Y 周」（Requirement 3.3）。
//
// at 是任意时刻，本方法按 Asia/Shanghai 取它的日期作为 rq，调用方不必自己换算时区；
// 同一句周次还出现在 jsMain_new.jsp，采集固定用这里的 POST（docs/upstream-api.md 第 4 节）。
// 文本缺失或 X、Y 不满足 1 ≤ X ≤ Y 返回 model.ErrUpstreamCalendarInvalid，不猜周次。
func (c *Client) FetchWeekCalendar(ctx context.Context, at time.Time) (*WeekCalendar, error) {
	rq := at.In(businessZone).Format(time.DateOnly)
	resp, err := c.doWithRelogin(ctx, request{
		method: http.MethodPost,
		path:   weekCalendarPath,
		form:   url.Values{"rq": {rq}},
	})
	if err != nil {
		return nil, fmt.Errorf("读取周历页: %w", err)
	}
	week, total, err := parseWeekText(resp.Body)
	if err != nil {
		return nil, err
	}
	return &WeekCalendar{Week: week, TotalWeeks: total, Raw: string(resp.Body)}, nil
}

// parseTermPage 从学期页正文做最小提取：第一个形如 YYYY-YYYY-N 的学期编号，
// 以及 id/name 为 kbjcmsid 的隐藏输入的 value。
// 任一缺失都返回 model.ErrUpstreamCalendarInvalid；错误里只带响应字节数，不带页面片段。
func parseTermPage(body []byte) (string, string, error) {
	match := termNumberPattern.Find(body)
	if match == nil {
		return "", "", fmt.Errorf("%w: %w: 响应 %d 字节",
			model.ErrUpstreamCalendarInvalid, errTermNotFound, len(body))
	}
	kbjcmsid := inputValue(body, kbjcmsidInputID)
	if kbjcmsid == "" {
		return "", "", fmt.Errorf("%w: %w: 响应 %d 字节",
			model.ErrUpstreamCalendarInvalid, errKBJCMSIDNotFound, len(body))
	}
	return string(match), kbjcmsid, nil
}

// inputValue 找出 id 或 name 等于 want 的第一个 input，返回它的 value。
// 属性顺序、引号风格、标签大小写都不敏感；value 为空或没有 value 视为没取到。
func inputValue(body []byte, want string) string {
	for _, tag := range inputTagPattern.FindAll(body, -1) {
		attrs := parseInputAttributes(tag)
		// HTML 的 id/name 大小写不敏感，比较前统一转小写。
		id := strings.ToLower(attrs["id"])
		name := strings.ToLower(attrs["name"])
		if id != want && name != want {
			continue
		}
		if value := strings.TrimSpace(attrs["value"]); value != "" {
			return value
		}
	}
	return ""
}

// parseInputAttributes 把一个开始标签里的属性拆成小写名到值的映射；同名属性取第一个。
func parseInputAttributes(tag []byte) map[string]string {
	attrs := make(map[string]string, 4)
	for _, match := range attrPattern.FindAllSubmatch(tag, -1) {
		name := strings.ToLower(string(match[1]))
		if _, exists := attrs[name]; exists {
			continue
		}
		switch {
		case match[2] != nil:
			attrs[name] = string(match[2])
		case match[3] != nil:
			attrs[name] = string(match[3])
		default:
			attrs[name] = string(match[4])
		}
	}
	return attrs
}

// parseWeekText 从周历页正文提取 X 与 Y，并校验 1 ≤ X ≤ Y。
// 文本缺失与周次非法分别用 errWeekTextNotFound、errWeekOutOfRange 区分，两者都归为
// model.ErrUpstreamCalendarInvalid；错误里不带页面原文。
func parseWeekText(body []byte) (int, int, error) {
	match := weekTextPattern.FindSubmatch(body)
	if match == nil {
		return 0, 0, fmt.Errorf("%w: %w: 响应 %d 字节",
			model.ErrUpstreamCalendarInvalid, errWeekTextNotFound, len(body))
	}
	week, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w: 当前周 %q 超出整数范围",
			model.ErrUpstreamCalendarInvalid, errWeekOutOfRange, match[1])
	}
	total, err := strconv.Atoi(string(match[2]))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %w: 总周数 %q 超出整数范围",
			model.ErrUpstreamCalendarInvalid, errWeekOutOfRange, match[2])
	}
	if week < 1 || week > total {
		return 0, 0, fmt.Errorf("%w: %w: 第 %d 周 / %d 周",
			model.ErrUpstreamCalendarInvalid, errWeekOutOfRange, week, total)
	}
	return week, total, nil
}
