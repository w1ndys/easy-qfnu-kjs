package upstream

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestFetchTermExtractsTermAndKBJCMSID 覆盖 Requirement 3.1：
// GET /jsxsd/kbxx/jsjy_query 取学期编号与 kbjcmsid，复用登录后的内存会话，
// 并原样返回正文供 parser（tasklist 4.3）做权威解析。
func TestFetchTermExtractsTermAndKBJCMSID(t *testing.T) {
	f := newLoginFixture(t)
	f.businessBodies = []string{fixtureTermPage}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	snapshot, err := c.FetchTerm(t.Context())
	if err != nil {
		t.Fatalf("读取学期页应成功，实际 err=%v", err)
	}
	if snapshot.Term != "2026-2027-1" {
		t.Fatalf("学期编号期望 2026-2027-1，实际 %q", snapshot.Term)
	}
	if snapshot.KBJCMSID != "20251" {
		t.Fatalf("kbjcmsid 期望 20251，实际 %q", snapshot.KBJCMSID)
	}
	if snapshot.Raw != fixtureTermPage {
		t.Fatalf("原始正文应原样返回，实际 %q", snapshot.Raw)
	}

	reqs := requestsFor(f.upstreamSnapshot(), http.MethodGet, termPagePath)
	if len(reqs) != 1 {
		t.Fatalf("期望 1 次学期页请求，实际 %d 次", len(reqs))
	}
	if got := reqs[0].cookie; got != "JSESSIONID=session-1" {
		t.Fatalf("学期页必须复用登录会话，实际 Cookie=%q", got)
	}
	if got := reqs[0].query.Encode(); got != "" {
		t.Fatalf("学期页不应带查询串，实际 %q", got)
	}
}

// TestFetchTermToleratesInputAttributeOrder 覆盖 kbjcmsid 提取的健壮性：
// 属性顺序、单双引号、大小写与是否加引号都不影响取值，也不会误取别的输入框。
func TestFetchTermToleratesInputAttributeOrder(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "文档顺序", input: `<input type="hidden" id="kbjcmsid" name="kbjcmsid" value="20251">`},
		{name: "value 在前", input: `<input value="20251" type="hidden" id="kbjcmsid">`},
		{name: "单引号", input: `<input type='hidden' name='kbjcmsid' value='20251'>`},
		{name: "大写标签与属性", input: `<INPUT TYPE="hidden" ID="KBJCMSID" VALUE="20251">`},
		{name: "属性不加引号", input: `<input type=hidden id=kbjcmsid value=20251>`},
		{name: "前一个输入框不是目标", input: `<input id="other" value="wrong"><input id="kbjcmsid" value="20251">`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.businessBodies = []string{"<html><div>2026-2027-1</div>" + tc.input + "</html>"}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			snapshot, err := c.FetchTerm(t.Context())
			if err != nil {
				t.Fatalf("读取学期页应成功，实际 err=%v", err)
			}
			if snapshot.KBJCMSID != "20251" {
				t.Fatalf("kbjcmsid 期望 20251，实际 %q", snapshot.KBJCMSID)
			}
		})
	}
}

// TestFetchTermRejectsMissingFields 覆盖 Requirement 3.1、3.4 的缺失分支：
// 学期编号或 kbjcmsid 取不到时返回可区分的 model.ErrUpstreamCalendarInvalid，
// 不猜学期，也不混成网络/状态码失败。
func TestFetchTermRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "缺学期编号", body: `<input id="kbjcmsid" value="20251">`, want: errTermNotFound},
		{name: "缺 kbjcmsid", body: `<div>2026-2027-1</div>`, want: errKBJCMSIDNotFound},
		{name: "kbjcmsid 为空", body: `<div>2026-2027-1</div><input id="kbjcmsid" value="">`, want: errKBJCMSIDNotFound},
		{name: "空正文", body: ``, want: errTermNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.businessBodies = []string{tc.body}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			_, err := c.FetchTerm(t.Context())
			if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
				t.Fatalf("期望学期/周历内容非法，实际 err=%v", err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望命中包内原因 %v，实际 err=%v", tc.want, err)
			}
			if errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("内容缺失不能混成上游请求失败，实际 err=%v", err)
			}
		})
	}
}

// TestBusinessPagesReloginOnceOnSessionExpiry 覆盖 Requirement 2.5、3.1、3.3：
// 学期页与周历页都会话失效时，经 doWithRelogin 重登录一次后重跑当前请求。
func TestBusinessPagesReloginOnceOnSessionExpiry(t *testing.T) {
	t.Run("学期页", func(t *testing.T) {
		f := newLoginFixture(t)
		f.businessBodies = []string{fixtureSessionLost, fixtureTermPage}
		c := newTestClient(t, f.srv.URL, WithMinInterval(0))
		f.mustLogin(t, c)

		if _, err := c.FetchTerm(t.Context()); err != nil {
			t.Fatalf("重登录后学期页应成功，实际 err=%v", err)
		}
		reqs := f.upstreamSnapshot()
		if got := len(logonLDAPRequests(reqs)); got != 2 {
			t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
		}
		if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 2 {
			t.Fatalf("期望学期页重跑一次，实际 %d 次", got)
		}
	})

	t.Run("周历页", func(t *testing.T) {
		f := newLoginFixture(t)
		f.weekCalendarBodies = []string{fixtureSessionLost, fixtureWeekCalendar}
		c := newTestClient(t, f.srv.URL, WithMinInterval(0))
		f.mustLogin(t, c)

		if _, err := c.FetchWeekCalendar(t.Context(), fixtureWeekDate()); err != nil {
			t.Fatalf("重登录后周历页应成功，实际 err=%v", err)
		}
		reqs := f.upstreamSnapshot()
		if got := len(logonLDAPRequests(reqs)); got != 2 {
			t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
		}
		if got := len(requestsFor(reqs, http.MethodPost, weekCalendarPath)); got != 2 {
			t.Fatalf("期望周历页重跑一次，实际 %d 次", got)
		}
	})
}

// fixtureWeekDate 返回一个固定的业务时刻：UTC 晚上对应上海已经是次日。
func fixtureWeekDate() time.Time {
	return time.Date(2026, 11, 2, 20, 30, 0, 0, time.UTC)
}

// TestFetchWeekCalendarPostsBusinessDate 覆盖 Requirement 3.3：
// POST 周历页且表单只带 rq；rq 按 Asia/Shanghai 取日期；正文「第5周 /16周」解析为 5/16，
// 原始正文一并返回，Cookie 仍复用登录会话。
func TestFetchWeekCalendarPostsBusinessDate(t *testing.T) {
	f := newLoginFixture(t)
	f.weekCalendarBodies = []string{fixtureWeekCalendar}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	calendar, err := c.FetchWeekCalendar(t.Context(), fixtureWeekDate())
	if err != nil {
		t.Fatalf("读取周历页应成功，实际 err=%v", err)
	}
	if calendar.Week != 5 || calendar.TotalWeeks != 16 {
		t.Fatalf("周次期望 5/16，实际 %d/%d", calendar.Week, calendar.TotalWeeks)
	}
	if calendar.Raw != fixtureWeekCalendar {
		t.Fatalf("原始正文应原样返回，实际 %q", calendar.Raw)
	}

	reqs := requestsFor(f.upstreamSnapshot(), http.MethodPost, weekCalendarPath)
	if len(reqs) != 1 {
		t.Fatalf("期望 1 次周历页请求，实际 %d 次", len(reqs))
	}
	// 2026-11-02T20:30Z 在上海是 2026-11-03，rq 必须按业务时区取日期。
	if got := reqs[0].form.Encode(); got != "rq=2026-11-03" {
		t.Fatalf("表单期望 rq=2026-11-03，实际 %q", got)
	}
	if got := reqs[0].cookie; got != "JSESSIONID=session-1" {
		t.Fatalf("周历页必须复用登录会话，实际 Cookie=%q", got)
	}
}

// TestParseWeekText 覆盖 Requirement 3.3 的文本解析与 1 ≤ X ≤ Y 校验：
// 兼容“共”与空白，文本缺失、周次为 0、超过总周数、总周数为 0 都算非法。
func TestParseWeekText(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		week    int
		total   int
		wantErr bool
	}{
		{name: "紧凑写法", body: "第5周 /16周", week: 5, total: 16},
		{name: "空白与共", body: "第 5 周 / 共 16 周", week: 5, total: 16},
		{name: "首周等于总周数", body: "第1周 / 共1周", week: 1, total: 1},
		{name: "缺总周数", body: "第5周", wantErr: true},
		{name: "没有周次文本", body: "<html>暂无</html>", wantErr: true},
		{name: "周次为 0", body: "第0周 /16周", wantErr: true},
		{name: "超过总周数", body: "第17周 /16周", wantErr: true},
		{name: "总周数为 0", body: "第1周 / 共0周", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			week, total, err := parseWeekText([]byte(tc.body))
			if tc.wantErr {
				if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
					t.Fatalf("期望学期/周历内容非法，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析周历文本失败: %v", err)
			}
			if week != tc.week || total != tc.total {
				t.Fatalf("周次期望 %d/%d，实际 %d/%d", tc.week, tc.total, week, total)
			}
		})
	}
}

// TestFetchWeekCalendarRejectsIllegalWeekText 覆盖 Requirement 3.4：
// 周历页取不到文本或周次非法时返回可区分的 model.ErrUpstreamCalendarInvalid，
// 由上层结束本轮，不猜周次。
func TestFetchWeekCalendarRejectsIllegalWeekText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "缺周次文本", body: "<html><body>系统维护</body></html>", want: errWeekTextNotFound},
		{name: "周次超过总周数", body: "<html><div>第17周 /16周</div></html>", want: errWeekOutOfRange},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.weekCalendarBodies = []string{tc.body}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			_, err := c.FetchWeekCalendar(t.Context(), fixtureWeekDate())
			if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
				t.Fatalf("期望学期/周历内容非法，实际 err=%v", err)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望命中包内原因 %v，实际 err=%v", tc.want, err)
			}
			if errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("内容非法不能混成上游请求失败，实际 err=%v", err)
			}
		})
	}
}
