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
	// detailRefreshedKBJCMSID 是重登录后学期页给出的新节次模式 id，必须与首次不同。
	detailRefreshedKBJCMSID = "20252"
)

// fixtureRefreshedTermPage 是重登录后的学期页夹具：学期编号不变，kbjcmsid 换成新值。
const fixtureRefreshedTermPage = `<html><body><div>2026-2027-1</div>` +
	`<input type="hidden" id="kbjcmsid" name="kbjcmsid" value="` + detailRefreshedKBJCMSID + `"></body></html>`

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

// TestFetchOccupancyDetailRefreshesTermAfterRelogin 锁定拍板口径：
// 明细页会话失效 → 重登录一次 → 重取学期页拿新 kbjcmsid → 用新值重建请求重跑。
// 断言第二次明细请求的 kbjcmsid 是新值且与第一次不同，重登录与学期页刷新各只一次，
// 且其余查询参数在重建时没有丢。
func TestFetchOccupancyDetailRefreshesTermAfterRelogin(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost, fixtureOccupancyDetail}
	f.businessBodies = []string{fixtureRefreshedTermPage}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	body, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if err != nil {
		t.Fatalf("重登录并刷新学期参数后明细页应成功，实际 err=%v", err)
	}
	if !bytes.Equal(body, []byte(fixtureOccupancyDetail)) {
		t.Fatalf("应返回第二次的明细正文，实际 %q", body)
	}

	reqs := f.upstreamSnapshot()
	// 首次登录 1 次 + 重登录 1 次，只允许重登录一次。
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
	}
	// 学期页只在重登录后取一次：取多了说明刷新逻辑跑偏。
	if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 1 {
		t.Fatalf("期望只重取一次学期页，实际 %d 次", got)
	}
	detailReqs := requestsFor(reqs, http.MethodGet, occupancyDetailPath)
	if len(detailReqs) != 2 {
		t.Fatalf("期望明细请求重跑一次共 2 次，实际 %d 次", len(detailReqs))
	}

	firstKBJCMSID := detailReqs[0].query.Get("kbjcmsid")
	if firstKBJCMSID != detailTestKBJCMSID {
		t.Fatalf("第一次明细请求应带调用方传入的 %q，实际 %q", detailTestKBJCMSID, firstKBJCMSID)
	}
	secondKBJCMSID := detailReqs[1].query.Get("kbjcmsid")
	if secondKBJCMSID != detailRefreshedKBJCMSID {
		t.Fatalf("重跑必须用学期页的新 kbjcmsid %q，实际 %q", detailRefreshedKBJCMSID, secondKBJCMSID)
	}
	if firstKBJCMSID == secondKBJCMSID {
		t.Fatalf("两次明细请求的 kbjcmsid 必须不同，实际都是 %q", firstKBJCMSID)
	}
	// 重建只换 kbjcmsid：学期编号、房号、星期与大节仍然来自调用方参数。
	second := detailReqs[1].query
	for name, want := range map[string]string{
		"xnxqh": detailTestTerm,
		"jsbh":  detailTestRoomID,
		"kcsj":  "10102",
		"xq":    "1",
		"type":  "add",
	} {
		if got := second.Get(name); got != want {
			t.Fatalf("重建后查询参数 %s 期望 %q，实际 %q", name, want, got)
		}
	}
	if got := len(second); got != len(detailReqs[0].query) {
		t.Fatalf("重建后查询参数个数应与首次一致，实际 %d 个", got)
	}
}

// TestFetchOccupancyDetailGivesUpAfterSecondExpiry 锁定「只重试一次」：
// 重登录并用新 kbjcmsid 重跑后仍返回登录页时，把会话错误交回上层，
// 不再重登录第二次，也不再多取一次学期页。
func TestFetchOccupancyDetailGivesUpAfterSecondExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost}
	f.businessBodies = []string{fixtureRefreshedTermPage}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	_, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if !errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("重跑仍失效时应返回会话错误，实际 err=%v", err)
	}

	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("只允许重登录一次，期望共 2 次提交，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 1 {
		t.Fatalf("期望只重取一次学期页，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, occupancyDetailPath)); got != 2 {
		t.Fatalf("期望明细请求只重跑一次共 2 次，实际 %d 次", got)
	}
}

// TestFetchOccupancyDetailReloginOnceOnSessionExpiry 覆盖 Requirement 2.5、6.1：
// 明细页会话失效时经 doWithReloginRefresh 重登录一次后重取学期参数并重跑当前下钻。
func TestFetchOccupancyDetailReloginOnceOnSessionExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost, fixtureOccupancyDetail}
	f.businessBodies = []string{fixtureTermPage}
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
	if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 1 {
		t.Fatalf("重登录后应重取一次学期页，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, occupancyDetailPath)); got != 2 {
		t.Fatalf("期望明细请求重跑一次，实际 %d 次", got)
	}
}
