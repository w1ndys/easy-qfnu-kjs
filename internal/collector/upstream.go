package collector

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	termPattern   = regexp.MustCompile(`\d{4}-\d{4}-\d`)
	weekPattern   = regexp.MustCompile(`第\s*(\d+)\s*周`)
	totalPattern  = regexp.MustCompile(`[/(（共总]\s*(\d+)\s*周`)
	notCalPattern = regexp.MustCompile(MarkNotInCalendar)
)

// requester 抽象出 cas.Client.Do，便于测试。
type requester interface {
	Do(req *http.Request) (*http.Response, error)
}

// Upstream 封装采集器访问教务系统的三个上游动作。
type Upstream struct {
	client requester
}

// NewUpstream 创建上游访问器。
func NewUpstream(client requester) *Upstream {
	return &Upstream{client: client}
}

// CalendarInfo 学期 + 当前教学周上下文。
type CalendarInfo struct {
	Term        string
	CurrentWeek int // 0 表示不在教学周历内
	TotalWeeks  int
	InCalendar  bool
}

// fetchTerm 读取学期：GET /jsxsd/kbxx/jsjy_query。
func (u *Upstream) fetchTerm(ctx context.Context) (string, error) {
	body, err := u.request(ctx, UpstreamBase+URITermPage, nil, true)
	if err != nil {
		return "", err
	}
	m := termPattern.FindString(body)
	if m == "" {
		return "", newGlobal(CodeTermParse, "学期页面未解析到 YYYY-YYYY-N 学期串")
	}
	return m, nil
}

// fetchCalendar 读取当前周/总周数：GET /jsxsd/framework/jsMain_new.jsp?t1=1。
func (u *Upstream) fetchCalendar(ctx context.Context) (CalendarInfo, error) {
	body, err := u.request(ctx, UpstreamBase+URICalendar, nil, true)
	if err != nil {
		return CalendarInfo{}, err
	}

	notInCal := notCalPattern.MatchString(body)
	weekMatch := weekPattern.FindStringSubmatch(body)

	if notInCal || weekMatch == nil {
		if notInCal {
			total := 0
			if m := totalPattern.FindStringSubmatch(body); m != nil {
				total, _ = strconv.Atoi(m[1])
			}
			return CalendarInfo{CurrentWeek: 0, TotalWeeks: total, InCalendar: false}, nil
		}
		return CalendarInfo{}, newGlobal(CodeCalendarParse,
			"周次页面未解析到教学周信息，且无“不在教学周历内”提示")
	}

	cur, _ := strconv.Atoi(weekMatch[1])
	total := 0
	if m := totalPattern.FindStringSubmatch(body[weekMatch[0][0]:]); m != nil {
		total, _ = strconv.Atoi(m[1])
	}
	if cur <= 0 || total <= 0 || cur > total {
		return CalendarInfo{}, newGlobal(CodeCalendarParse,
			"周次页面解析异常：当前周=%d 总周数=%d", cur, total)
	}
	return CalendarInfo{CurrentWeek: cur, TotalWeeks: total, InCalendar: true}, nil
}

// queryWeek 按周 POST /jsxsd/kbxx/jsjy_query2 并解析。
// 参数契约：typewhere=jszq、xnxqh、jsmc_mh 空、zc=zc2=周、xq=1、xq2=7；
// 不传 jc/jc2、jszt。请求级失败（网络/HTTP）是周级失败（ScopeWeek，保留旧数据）；
// 登录页/非法访问/结构失败是全局失败。
func (u *Upstream) queryWeek(ctx context.Context, term string, week int) ([]ParsedRow, error) {
	form := url.Values{}
	form.Set("typewhere", "jszq")
	form.Set("xnxqh", term)
	form.Set("jsmc_mh", "")
	form.Set("zc", strconv.Itoa(week))
	form.Set("zc2", strconv.Itoa(week))
	form.Set("xq", "1")
	form.Set("xq2", "7")

	body, err := u.request(ctx, UpstreamBase+URIWeekQuery, form, false)
	if err != nil {
		return nil, err
	}
	return ParseWeekHTML(body)
}

// request 执行带 3 次重试的请求并返回 200 响应体。
// scopeGlobal=false 时网络失败被归类为周级（ScopeWeek）；页面特征失败恒为全局。
func (u *Upstream) request(ctx context.Context, endpoint string, form url.Values, scopeGlobal bool) (string, error) {
	scope := ScopeWeek
	if scopeGlobal {
		scope = ScopeGlobal
	}

	var lastErr error
	for attempt := range 3 {
		if err := ctx.Err(); err != nil {
			return "", &CollectorError{Code: CodeRequestFailed, Scope: scope,
				Msg: fmt.Sprintf("%s 请求取消: %v", endpoint, err)}
		}
		if attempt > 0 {
			// 请求级退避：1s、2s（周间 0.5–2s 随机间隔由调用方控制）。
			if !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
				return "", &CollectorError{Code: CodeRequestFailed, Scope: scope,
					Msg: fmt.Sprintf("%s 请求取消（退避中）", endpoint)}
			}
		}

		body, err := u.doOnce(ctx, endpoint, form)
		if err != nil {
			lastErr = err
			continue
		}
		if code, ok := detectPageFailure(body); ok {
			return "", newGlobal(code, "%s 返回异常页面特征 %q", endpoint, code)
		}
		return body, nil
	}
	return "", &CollectorError{Code: CodeRequestFailed, Scope: scope,
		Msg: fmt.Sprintf("%s 请求连续失败: %v", endpoint, lastErr)}
}

func (u *Upstream) doOnce(ctx context.Context, endpoint string, form url.Values) (string, error) {
	var req *http.Request
	var err error
	if form != nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	}
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP 状态 %d", resp.StatusCode)
	}
	return string(raw), nil
}

// detectPageFailure 返回命中的页面特征错误码（登录页/非法访问）。
func detectPageFailure(body string) (string, bool) {
	if strings.Contains(body, MarkIllegalAccess) {
		return CodeIllegalAccess, true
	}
	if strings.Contains(body, MarkLoginPage) {
		return CodeLoginPage, true
	}
	return "", false
}

// sleepCtx 可取消睡眠；返回 false 表示 context 已取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// weekJitterDelay 返回 0.5—2 秒的随机间隔。
func weekJitterDelay() time.Duration {
	return time.Duration(500+rand.IntN(1501)) * time.Millisecond
}
