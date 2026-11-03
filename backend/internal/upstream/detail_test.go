package upstream

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 占用说明夹具的固定取值：学期编号与 kbjcmsid 与学期页夹具同一口径。
const (
	detailTestTerm     = "2026-2027-1"
	detailTestKBJCMSID = "20251"
	detailTestRoomID   = "R0001"
)

// detailParams 返回一份合法参数，用例只改自己关心的字段。
func detailParams() OccupancyDetailParams {
	return OccupancyDetailParams{
		Term:     detailTestTerm,
		KBJCMSID: detailTestKBJCMSID,
		RoomID:   detailTestRoomID,
		Weekday:  1,
		BigSlot:  "0102",
	}
}

// TestFetchOccupancyDetailSendsQueryAndReturnsBody 覆盖 Requirement 6.1：
// GET 明细端点，带齐文档第 8 节的 16 个查询参数（空值显式留空）、复用登录 Cookie，
// 正文原样返回给 parser，一次调用只下一次钻。
func TestFetchOccupancyDetailSendsQueryAndReturnsBody(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	body, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if err != nil {
		t.Fatalf("请求占用说明应成功，实际 err=%v", err)
	}
	if !bytes.Equal(body, []byte(fixtureOccupancyDetail)) {
		t.Fatalf("正文应原样返回，实际 %q", body)
	}

	reqs := requestsFor(f.upstreamSnapshot(), http.MethodGet, occupancyDetailPath)
	if len(reqs) != 1 {
		t.Fatalf("每次下钻只应发 1 次明细请求，实际 %d 次", len(reqs))
	}
	req := reqs[0]
	if got := req.cookie; got != "JSESSIONID=session-1" {
		t.Fatalf("明细页必须复用登录会话，实际 Cookie=%q", got)
	}

	wantQuery := map[string]string{
		"xnxqh":     detailTestTerm,
		"jsbh":      detailTestRoomID,
		"kcsj":      "10102",
		"xq":        "1",
		"typewhere": "jszq",
		"startZc":   "",
		"endZc":     "",
		"startJc":   "",
		"endJc":     "",
		"startXq":   "1",
		"endXq":     "7",
		"kssj":      "",
		"jssj":      "",
		"jszt":      "",
		"type":      "add",
		"kbjcmsid":  detailTestKBJCMSID,
	}
	for name, want := range wantQuery {
		if got := req.query.Get(name); got != want {
			t.Fatalf("查询参数 %s 期望 %q，实际 %q", name, want, got)
		}
	}
	// 字段数必须与文档一致：空参数也要显式带上，多带或少带都会改变上游的查询范围。
	if got := len(req.query); got != len(wantQuery) {
		t.Fatalf("查询参数期望 %d 个，实际 %d 个: %v", len(wantQuery), got, req.query)
	}
}

// TestOccupancyDetailQueryBuildsKcsjFromWeekdayAndBigSlot 覆盖 Requirement 6.1 的参数组装：
// kcsj 是「星期 + 大节代码」的拼接，五位或七位随大节代码长度变化；
// xq 与 kcsj 的第一位一致（docs/upstream-api.md 第 8 节）。
func TestOccupancyDetailQueryBuildsKcsjFromWeekdayAndBigSlot(t *testing.T) {
	cases := []struct {
		name        string
		weekday     int
		bigSlot     string
		wantKCSJ    string
		wantWeekday string
	}{
		{name: "周一第一大节", weekday: 1, bigSlot: "0102", wantKCSJ: "10102", wantWeekday: "1"},
		{name: "周三第二大节", weekday: 3, bigSlot: "030405", wantKCSJ: "3030405", wantWeekday: "3"},
		{name: "周五第三大节", weekday: 5, bigSlot: "0607", wantKCSJ: "50607", wantWeekday: "5"},
		{name: "周六第四大节", weekday: 6, bigSlot: "0809", wantKCSJ: "60809", wantWeekday: "6"},
		{name: "周日第五大节", weekday: 7, bigSlot: "101112", wantKCSJ: "7101112", wantWeekday: "7"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := detailParams()
			params.Weekday = tc.weekday
			params.BigSlot = tc.bigSlot
			query := occupancyDetailQuery(params)

			if got := query.Get("kcsj"); got != tc.wantKCSJ {
				t.Fatalf("kcsj 期望 %q，实际 %q", tc.wantKCSJ, got)
			}
			if got := query.Get("xq"); got != tc.wantWeekday {
				t.Fatalf("xq 期望 %q，实际 %q", tc.wantWeekday, got)
			}
			// 文档要求 xq 与 kcsj 第一位一致；拼接顺序写反时这里会先炸。
			if got := query.Get("kcsj")[:1]; got != query.Get("xq") {
				t.Fatalf("kcsj 第一位 %q 必须与 xq %q 一致", got, query.Get("xq"))
			}
		})
	}
}

// TestFetchOccupancyDetailRejectsInvalidParams 覆盖参数拒绝：
// 学期编号、kbjcmsid、房号缺失，以及星期越界、大节代码不在五块之内，
// 都必须返回可区分的包内原因且在发请求前失败，不混成上游请求失败。
func TestFetchOccupancyDetailRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*OccupancyDetailParams)
		want   error
	}{
		{name: "学期编号为空", mutate: func(p *OccupancyDetailParams) { p.Term = "" }, want: errEmptyTerm},
		{name: "学期编号只有空白", mutate: func(p *OccupancyDetailParams) { p.Term = "  " }, want: errEmptyTerm},
		{name: "节次模式 id 为空", mutate: func(p *OccupancyDetailParams) { p.KBJCMSID = "" }, want: errKBJCMSIDEmpty},
		{name: "节次模式 id 只有空白", mutate: func(p *OccupancyDetailParams) { p.KBJCMSID = "  " }, want: errKBJCMSIDEmpty},
		{name: "房号为空", mutate: func(p *OccupancyDetailParams) { p.RoomID = "" }, want: errRoomIDEmpty},
		{name: "房号只有空白", mutate: func(p *OccupancyDetailParams) { p.RoomID = "  " }, want: errRoomIDEmpty},
		{name: "星期为 0", mutate: func(p *OccupancyDetailParams) { p.Weekday = 0 }, want: errBadWeekday},
		{name: "星期为 8", mutate: func(p *OccupancyDetailParams) { p.Weekday = 8 }, want: errBadWeekday},
		{name: "大节代码为空", mutate: func(p *OccupancyDetailParams) { p.BigSlot = "" }, want: errBadBigSlot},
		{name: "大节代码是单小节", mutate: func(p *OccupancyDetailParams) { p.BigSlot = "01" }, want: errBadBigSlot},
		{name: "大节代码不在五块之内", mutate: func(p *OccupancyDetailParams) { p.BigSlot = "010203" }, want: errBadBigSlot},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			params := detailParams()
			tc.mutate(&params)
			_, err := c.FetchOccupancyDetail(t.Context(), params)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望命中包内原因 %v，实际 err=%v", tc.want, err)
			}
			if errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("参数缺失不能混成上游请求失败，实际 err=%v", err)
			}
			if got := len(f.upstreamSnapshot()); got != 0 {
				t.Fatalf("参数缺失时不应发请求，实际 %d 次", got)
			}
		})
	}
}

// TestFetchOccupancyDetailReloginOnceOnSessionExpiry 覆盖 Requirement 2.5、6.1：
// 明细页会话失效时经 doWithRelogin 重登录一次后重跑当前下钻。
func TestFetchOccupancyDetailReloginOnceOnSessionExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost, fixtureOccupancyDetail}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	body, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if err != nil {
		t.Fatalf("重登录后明细页应成功，实际 err=%v", err)
	}
	if !bytes.Equal(body, []byte(fixtureOccupancyDetail)) {
		t.Fatalf("重登录后应返回第二次的明细正文，实际 %q", body)
	}

	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, occupancyDetailPath)); got != 2 {
		t.Fatalf("期望明细请求重跑一次，实际 %d 次", got)
	}
}
