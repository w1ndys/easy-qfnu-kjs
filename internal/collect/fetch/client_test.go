package fetch

// 本文件是 fetch 的单元测试：HTTP 交给假 RoundTripper，不建连接、不访问真实教务系统，
// 也不涉及真实账号与 Cookie（假会话里的 Cookie 是测试自造的字符串）。
// 正文都是手写的最小片段，真实教务 HTML 不进 git。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testBaseURL 是假上游的根地址：假传输不解析主机名，只用它拼路径。
const testBaseURL = "http://upstream.test"

// testTerm 是测试用的学期编号。
const testTerm = "2026-2027-1"

// testPageBody 是一份不含会话失效特征的最小正文。
const testPageBody = "<html><body>最小样本</body></html>"

// fakeSession 是假会话：Cookie 由测试控制，未登录时返回空串。
type fakeSession struct {
	cookie string // 当前 Cookie 头
}

// Cookie 实现 Session 接口。
func (s *fakeSession) Cookie() string { return s.cookie }

// fakeClock 是假时钟：等待只把时间往前推，测试不真等 500 毫秒。
type fakeClock struct {
	now time.Time // 当前时间
}

// Now 返回当前假时间。
func (c *fakeClock) Now() time.Time { return c.now }

// Sleep 把假时间往前推一段时间。
func (c *fakeClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// fakeResponse 是一次预设的上游响应。
type fakeResponse struct {
	status int    // HTTP 状态码
	body   string // 响应正文
}

// recordedRequest 是被假传输记下的一次请求。
type recordedRequest struct {
	at     time.Time  // 发出请求时的假时钟时刻
	method string     // HTTP 方法
	path   string     // 请求路径
	form   url.Values // 解析后的表单
	cookie string     // 请求头里的 Cookie
}

// fakeTransport 是假 RoundTripper：按调用顺序返回预设响应，并记下每次请求。
type fakeTransport struct {
	clock     *fakeClock        // 假时钟，用来记录请求时刻
	responses []fakeResponse    // 预设响应，用完后一直用最后一个
	requests  []recordedRequest // 已记录的请求
}

// RoundTrip 记录一次请求并返回对应的预设响应。
func (t *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	form, err := requestParams(req)
	if err != nil {
		return nil, err
	}
	t.requests = append(t.requests, recordedRequest{
		at:     t.clock.Now(),
		method: req.Method,
		path:   req.URL.Path,
		form:   form,
		cookie: req.Header.Get("Cookie"),
	})

	// 预设响应按调用顺序取，用完后一直用最后一个
	next := t.responses[len(t.responses)-1]
	if index := len(t.requests) - 1; index < len(t.responses) {
		next = t.responses[index]
	}
	return &http.Response{
		StatusCode: next.status,
		Status:     fmt.Sprintf("%d %s", next.status, http.StatusText(next.status)),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(next.body)),
	}, nil
}

// requestParams 取出一次请求的参数：GET 的参数在查询串里，POST 的参数在表单里。
func requestParams(req *http.Request) (url.Values, error) {
	// GET 没有请求体，参数只能从查询串读
	if req.Method == http.MethodGet {
		return url.ParseQuery(req.URL.RawQuery)
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(raw))
}

// newTestClient 组装一个用假会话、假时钟与假传输的客户端。
func newTestClient(t *testing.T, responses ...fakeResponse) (*Client, *fakeTransport, *fakeClock, *fakeSession) {
	t.Helper()

	clock := &fakeClock{now: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	session := &fakeSession{cookie: "JSESSIONID=it"}
	transport := &fakeTransport{clock: clock, responses: responses}

	client, err := NewClient(ClientConfig{
		BaseURL:    testBaseURL,
		Session:    session,
		HTTPClient: &http.Client{Transport: transport},
		Now:        clock.Now,
		Sleep:      clock.Sleep,
	})
	if err != nil {
		t.Fatalf("组装客户端失败: %v", err)
	}
	return client, transport, clock, session
}

// TestWeekMatrixSendsBuildingAndEmptyKeyword 断言请求参数：楼编号进 jxlbh，
// 教室关键词留空，周次写在 zc 与 zc2，且不传 jc/jc2/jszt（需求 2.2）。
func TestWeekMatrixSendsBuildingAndEmptyKeyword(t *testing.T) {
	client, transport, clock, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})

	response, err := client.WeekMatrix(context.Background(), WeekMatrixParams{
		Term:       testTerm,
		BuildingID: "jxlbh-1",
		Week:       6,
	})
	if err != nil {
		t.Fatalf("请求周矩阵失败: %v", err)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("发出 %d 次请求，期望 1 次", len(transport.requests))
	}

	request := transport.requests[0]
	if request.method != http.MethodPost || request.path != WeekMatrixPath {
		t.Errorf("请求 %s %s，期望 POST %s", request.method, request.path, WeekMatrixPath)
	}
	if request.cookie != "JSESSIONID=it" {
		t.Errorf("请求头 Cookie = %q，期望带上会话里的 Cookie", request.cookie)
	}

	expected := map[string]string{
		"typewhere": "jszq",
		"xnxqh":     testTerm,
		"jxlbh":     "jxlbh-1",
		"jsmc_mh":   "", // 留空：用楼编号限定范围，不能把楼名写进关键词
		"zc":        "6",
		"zc2":       "6",
		"xq":        "1",
		"xq2":       "7",
	}
	for name, want := range expected {
		if got := request.form.Get(name); got != want {
			t.Errorf("表单 %s = %q，期望 %q", name, got, want)
		}
	}
	// 全天、全状态：这三个参数一律不传
	for _, name := range []string{"jc", "jc2", "jszt"} {
		if _, exists := request.form[name]; exists {
			t.Errorf("表单不该带 %s", name)
		}
	}

	if got := response.PageSHA256; got != sha256Hex([]byte(testPageBody)) {
		t.Errorf("正文摘要 = %s，期望 %s", got, sha256Hex([]byte(testPageBody)))
	}
	if !response.FetchedAt.Equal(clock.Now()) {
		t.Errorf("抓取时间 = %s，期望 %s", response.FetchedAt, clock.Now())
	}
	if response.Attempts != 1 {
		t.Errorf("请求次数 = %d，期望 1", response.Attempts)
	}
}

// TestWeekMatrixRejectsBadParamsBeforeRequest 断言空楼编号与非正周次在发出请求前就失败：
// 空 jxlbh 会被上游当成「不限楼」，等于拉全校。
func TestWeekMatrixRejectsBadParamsBeforeRequest(t *testing.T) {
	cases := []struct {
		name     string // 用例说明
		params   WeekMatrixParams
		expected error // 期望的错误
	}{
		{"楼编号为空", WeekMatrixParams{Term: testTerm, Week: 1}, ErrEmptyBuilding},
		{"楼编号只有空白", WeekMatrixParams{Term: testTerm, BuildingID: "  ", Week: 1}, ErrEmptyBuilding},
		{"周次为 0", WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1"}, ErrBadWeek},
		{"周次为负", WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: -3}, ErrBadWeek},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})
			if _, err := client.WeekMatrix(context.Background(), testCase.params); !errors.Is(err, testCase.expected) {
				t.Fatalf("返回 %v，期望 %v", err, testCase.expected)
			}
			// 参数不合法时一次请求都不该发出去
			if len(transport.requests) != 0 {
				t.Errorf("发出 %d 次请求，期望 0 次", len(transport.requests))
			}
		})
	}
}

// TestWeekMatrixRetriesExhausted 断言持续失败时最多重试 3 次（共 4 次请求），
// 并保留请求次数供任务表记录（需求 3.4）。
func TestWeekMatrixRetriesExhausted(t *testing.T) {
	client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusInternalServerError, body: "boom"})

	_, err := client.WeekMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 1})
	if err == nil {
		t.Fatal("持续失败时竟然返回成功")
	}
	var requestError RequestError
	if !errors.As(err, &requestError) {
		t.Fatalf("错误类型 = %T，期望 RequestError", err)
	}
	if requestError.Attempts != MaxRetries+1 {
		t.Errorf("请求次数 = %d，期望 %d", requestError.Attempts, MaxRetries+1)
	}
	if len(transport.requests) != MaxRetries+1 {
		t.Errorf("发出 %d 次请求，期望 %d 次", len(transport.requests), MaxRetries+1)
	}
	// 重试之间也要守间隔：不能一次失败后立刻再打
	checkInterval(t, transport)
}

// TestWeekMatrixSucceedsAfterRetry 断言中途恢复后返回成功，并记下真正的请求次数。
func TestWeekMatrixSucceedsAfterRetry(t *testing.T) {
	client, transport, _, _ := newTestClient(t,
		fakeResponse{status: http.StatusInternalServerError, body: "boom"},
		fakeResponse{status: http.StatusInternalServerError, body: "boom"},
		fakeResponse{status: http.StatusOK, body: testPageBody},
	)

	response, err := client.WeekMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 2})
	if err != nil {
		t.Fatalf("第三次成功时仍返回失败: %v", err)
	}
	if response.Attempts != 3 {
		t.Errorf("请求次数 = %d，期望 3", response.Attempts)
	}
	if len(transport.requests) != 3 {
		t.Errorf("发出 %d 次请求，期望 3 次", len(transport.requests))
	}
	if string(response.Body) != testPageBody {
		t.Errorf("正文 = %q，期望 %q", response.Body, testPageBody)
	}
	checkInterval(t, transport)
}

// TestWeekMatrixSessionLostIsNotRetried 断言登录页与非法访问提示按会话失效返回，
// 且不重试：重试只会再拿到一次登录页，应该交给调用方重新登录。
func TestWeekMatrixSessionLostIsNotRetried(t *testing.T) {
	bodies := []struct {
		name string // 用例说明
		body string
	}{
		{"登录页", "<html>用户登录</html>"},
		{"非法访问", "<html>非法访问，请重新登录</html>"},
	}

	for _, testCase := range bodies {
		t.Run(testCase.name, func(t *testing.T) {
			client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testCase.body})

			_, err := client.WeekMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 1})
			if !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("返回 %v，期望 ErrSessionExpired", err)
			}
			// 会话失效不重试：只发一次请求
			if len(transport.requests) != 1 {
				t.Errorf("发出 %d 次请求，期望 1 次", len(transport.requests))
			}
		})
	}
}

// TestWeekMatrixWithoutSessionDoesNotRequest 断言没有会话时直接按会话失效返回，不空手请求。
func TestWeekMatrixWithoutSessionDoesNotRequest(t *testing.T) {
	client, transport, _, session := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})
	session.cookie = ""

	_, err := client.WeekMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 1})
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("返回 %v，期望 ErrSessionExpired", err)
	}
	if len(transport.requests) != 0 {
		t.Errorf("发出 %d 次请求，期望 0 次", len(transport.requests))
	}

	// 重新登录拿到 Cookie 后同一个请求就能发出去
	session.cookie = "JSESSIONID=renewed"
	if _, err := client.WeekMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 1}); err != nil {
		t.Fatalf("重新登录后请求仍失败: %v", err)
	}
	if len(transport.requests) != 1 {
		t.Errorf("重新登录后发出 %d 次请求，期望 1 次", len(transport.requests))
	}
}

// TestWeekMatrixKeepsRequestInterval 断言相邻请求之间至少隔 500 毫秒（需求 5.7）。
func TestWeekMatrixKeepsRequestInterval(t *testing.T) {
	client, transport, clock, _ := newTestClient(t,
		fakeResponse{status: http.StatusOK, body: testPageBody},
		fakeResponse{status: http.StatusOK, body: testPageBody},
	)

	params := WeekMatrixParams{Term: testTerm, BuildingID: "jxlbh-1", Week: 1}
	// 第一次请求不用等，第二次开始必须等满一个间隔
	for week := 1; week <= 2; week++ {
		params.Week = week
		if _, err := client.WeekMatrix(context.Background(), params); err != nil {
			t.Fatalf("第 %d 次请求失败: %v", week, err)
		}
	}

	checkInterval(t, transport)
	if first := transport.requests[0].at; !first.Equal(clock.Now().Add(-RequestInterval)) {
		t.Errorf("第一次请求在 %s 发出，第二次在 %s，第一次不该等待", first, clock.Now())
	}
}

// checkInterval 断言相邻请求之间的间隔都不小于 RequestInterval。
func checkInterval(t *testing.T, transport *fakeTransport) {
	t.Helper()

	for index := 1; index < len(transport.requests); index++ {
		gap := transport.requests[index].at.Sub(transport.requests[index-1].at)
		// 间隔不足说明上游在单位时间内被打了不止两次
		if gap < RequestInterval {
			t.Errorf("第 %d 次与第 %d 次请求相隔 %s，期望不少于 %s", index, index+1, gap, RequestInterval)
		}
	}
}

// TestSha256Hex 断言正文摘要按十六进制小写给出，与任务 2 的字段口径一致。
func TestSha256Hex(t *testing.T) {
	sum := sha256.Sum256([]byte("abc"))
	if got := sha256Hex([]byte("abc")); got != hex.EncodeToString(sum[:]) {
		t.Errorf("摘要 = %s，期望 %s", got, hex.EncodeToString(sum[:]))
	}
}

// TestCellDetailBuildsWeekdayBlockCode 断言占用明细请求的写法：GET 到明细端点，
// kcsj 是「星期 1 位数字 + 表头 tdvalue」，xq 用同一个星期数字（需求 4.1）。
// 四个例子取自上游探测记录第 5.1 节。
func TestCellDetailBuildsWeekdayBlockCode(t *testing.T) {
	cases := []struct {
		weekday int    // 星期
		block   string // 表头 tdvalue
		kcsj    string // 期望的 kcsj
	}{
		{1, "0102", "10102"},
		{2, "0102", "20102"},
		{1, "030405", "1030405"},
		{1, "101112", "1101112"},
	}

	for _, testCase := range cases {
		client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})

		response, err := client.CellDetail(context.Background(), CellDetailParams{
			Term:       testTerm,
			RoomID:     "1088",
			Weekday:    testCase.weekday,
			Block:      testCase.block,
			TimeModeID: "94786EE0",
		})
		if err != nil {
			t.Fatalf("请求占用明细失败: %v", err)
		}
		// 一次请求、且是 GET：参数要落在查询串里，不能当表单发
		if len(transport.requests) != 1 {
			t.Fatalf("发出 %d 次请求，期望 1 次", len(transport.requests))
		}
		request := transport.requests[0]
		if request.method != http.MethodGet || request.path != CellDetailPath {
			t.Errorf("请求 = %s %s，期望 GET %s", request.method, request.path, CellDetailPath)
		}
		if got := request.form.Get("kcsj"); got != testCase.kcsj {
			t.Errorf("kcsj = %q，期望 %q", got, testCase.kcsj)
		}
		if got := request.form.Get("xq"); got != fmt.Sprint(testCase.weekday) {
			t.Errorf("xq = %q，期望 %d", got, testCase.weekday)
		}
		if request.form.Get("xnxqh") != testTerm || request.form.Get("jsbh") != "1088" {
			t.Errorf("学期或房间没带上: %v", request.form)
		}
		// 时间模式随请求带上，读不到时为空串
		if request.form.Get("kbjcmsid") != "94786EE0" {
			t.Errorf("kbjcmsid = %q，期望 94786EE0", request.form.Get("kbjcmsid"))
		}
		if request.form.Get("typewhere") != typewhereValue {
			t.Errorf("typewhere = %q，期望 %q", request.form.Get("typewhere"), typewhereValue)
		}
		if response.PageSHA256 != sha256Hex([]byte(testPageBody)) {
			t.Errorf("正文摘要 = %s，期望按正文算出", response.PageSHA256)
		}
	}
}

// TestCellDetailRejectsBadParamsBeforeRequest 断言参数不合法时一次请求都不发：
// 空房间、越界星期与非数字大节都定位不到格子。
func TestCellDetailRejectsBadParamsBeforeRequest(t *testing.T) {
	cases := []struct {
		name     string           // 用例说明
		params   CellDetailParams // 请求参数
		expected error            // 期望的错误
	}{
		{"房间为空", CellDetailParams{Term: testTerm, Weekday: 1, Block: "0102"}, ErrEmptyRoom},
		{"房间只有空白", CellDetailParams{Term: testTerm, RoomID: "  ", Weekday: 1, Block: "0102"}, ErrEmptyRoom},
		{"星期为 0", CellDetailParams{Term: testTerm, RoomID: "1088", Block: "0102"}, ErrBadWeekday},
		{"星期为 8", CellDetailParams{Term: testTerm, RoomID: "1088", Weekday: 8, Block: "0102"}, ErrBadWeekday},
		{"大节为空", CellDetailParams{Term: testTerm, RoomID: "1088", Weekday: 1}, ErrBadBlock},
		{"大节不是数字", CellDetailParams{Term: testTerm, RoomID: "1088", Weekday: 1, Block: "0102x"}, ErrBadBlock},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})
			if _, err := client.CellDetail(context.Background(), testCase.params); !errors.Is(err, testCase.expected) {
				t.Fatalf("返回 %v，期望 %v", err, testCase.expected)
			}
			if len(transport.requests) != 0 {
				t.Errorf("发出 %d 次请求，期望 0 次", len(transport.requests))
			}
		})
	}
}

// TestCellDetailWithoutSessionDoesNotRequest 断言没有会话就不发请求：
// 空手请求只会拿到登录页，调用方应当先重新登录（需求 5.3）。
func TestCellDetailWithoutSessionDoesNotRequest(t *testing.T) {
	client, transport, _, session := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})
	session.cookie = ""

	_, err := client.CellDetail(context.Background(), CellDetailParams{
		Term: testTerm, RoomID: "1088", Weekday: 1, Block: "0102",
	})
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("返回 %v，期望 ErrSessionExpired", err)
	}
	if len(transport.requests) != 0 {
		t.Errorf("发出 %d 次请求，期望 0 次", len(transport.requests))
	}
}

// TestTermPageRequestsTermPage 断言读学期是一次不带表单的 GET：学期编号从这一页的正文里取。
func TestTermPageRequestsTermPage(t *testing.T) {
	client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})

	response, err := client.TermPage(context.Background())
	if err != nil {
		t.Fatalf("请求学期页失败: %v", err)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("发出 %d 次请求，期望 1 次", len(transport.requests))
	}

	request := transport.requests[0]
	// 学期页是默认查询页，不带任何表单参数
	if request.method != http.MethodGet || request.path != TermPath {
		t.Errorf("请求 %s %s，期望 GET %s", request.method, request.path, TermPath)
	}
	if len(request.form) != 0 {
		t.Errorf("查询串有 %d 个参数，期望一个都不带", len(request.form))
	}
	if response.Attempts != 1 {
		t.Errorf("请求次数 = %d，期望 1", response.Attempts)
	}
}

// TestCalendarPageSendsSampleDate 断言周历采样只带 rq：日期按 YYYY-MM-DD 写成 Asia/Shanghai 的当天。
func TestCalendarPageSendsSampleDate(t *testing.T) {
	client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})

	sampleDate := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	if _, err := client.CalendarPage(context.Background(), sampleDate); err != nil {
		t.Fatalf("请求周历页失败: %v", err)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("发出 %d 次请求，期望 1 次", len(transport.requests))
	}

	request := transport.requests[0]
	if request.method != http.MethodPost || request.path != CalendarPath {
		t.Errorf("请求 %s %s，期望 POST %s", request.method, request.path, CalendarPath)
	}
	// 表单字段只有 rq，多带参数会被上游当成另一次查询
	if got := request.form.Get("rq"); got != "2026-10-30" {
		t.Errorf("表单 rq = %q，期望 2026-10-30", got)
	}
	if len(request.form) != 1 {
		t.Errorf("表单有 %d 个字段，期望只有 rq", len(request.form))
	}
}

// TestAggregateMatrixLeavesWeekEmpty 断言聚合矩阵把 zc 与 zc2 留空：周次填上就变成逐周矩阵。
func TestAggregateMatrixLeavesWeekEmpty(t *testing.T) {
	client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})

	if _, err := client.AggregateMatrix(context.Background(), WeekMatrixParams{
		Term:       testTerm,
		BuildingID: "jxlbh-1",
	}); err != nil {
		t.Fatalf("请求聚合矩阵失败: %v", err)
	}
	if len(transport.requests) != 1 {
		t.Fatalf("发出 %d 次请求，期望 1 次", len(transport.requests))
	}

	request := transport.requests[0]
	if request.method != http.MethodPost || request.path != WeekMatrixPath {
		t.Errorf("请求 %s %s，期望 POST %s", request.method, request.path, WeekMatrixPath)
	}
	// 两个字段都在，值留空：与页面上的全学期查询一致
	for _, name := range []string{"zc", "zc2"} {
		value, exists := request.form[name]
		if !exists {
			t.Errorf("表单缺少 %s", name)
			continue
		}
		if len(value) != 1 || value[0] != "" {
			t.Errorf("表单 %s = %v，期望留空", name, value)
		}
	}
	if got := request.form.Get("jxlbh"); got != "jxlbh-1" {
		t.Errorf("表单 jxlbh = %q，期望 jxlbh-1", got)
	}
	if got := request.form.Get("jsmc_mh"); got != "" {
		t.Errorf("表单 jsmc_mh = %q，期望留空", got)
	}
}

// TestAggregateMatrixRejectsEmptyBuilding 断言聚合矩阵同样拒绝空楼编号，且不发请求。
func TestAggregateMatrixRejectsEmptyBuilding(t *testing.T) {
	cases := []struct {
		name     string // 用例说明
		building string // 传入的楼编号
	}{
		{"楼编号为空", ""},
		{"楼编号只有空白", "  "},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, transport, _, _ := newTestClient(t, fakeResponse{status: http.StatusOK, body: testPageBody})
			_, err := client.AggregateMatrix(context.Background(), WeekMatrixParams{Term: testTerm, BuildingID: testCase.building})
			// 空 jxlbh 等于拉全校，必须在本地挡住
			if !errors.Is(err, ErrEmptyBuilding) {
				t.Fatalf("返回 %v，期望 ErrEmptyBuilding", err)
			}
			if len(transport.requests) != 0 {
				t.Errorf("发出 %d 次请求，期望 0 次", len(transport.requests))
			}
		})
	}
}
