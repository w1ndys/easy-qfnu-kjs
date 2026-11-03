package upstream

import (
	"bytes"
	"errors"
	"net/http"
	"net/url"
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
	// detailRefreshedTerm 是重登录后学期页上出现的另一个学期编号：
	// 它必须与调用方 Term 不同，否则锁不住「xnxqh 仍来自调用方」。
	detailRefreshedTerm = "2026-2027-2"
)

// fixtureRefreshedTermPage 是重登录后的学期页夹具：学期编号故意换成 detailRefreshedTerm，
// kbjcmsid 换成新值；重建请求的 xnxqh 必须仍取调用方 Term，不能跟着这一页走。
const fixtureRefreshedTermPage = `<html><body><div>` + detailRefreshedTerm + `</div>` +
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

// detailQueryWant 是明细请求 16 个查询参数的期望表（docs/upstream-api.md 第 8 节）：
// 起止周次/节次/时间与 jszt 必须显式留空。kbjcmsid 由调用方给出，
// 因为重登录后的重建请求会换成学期页下发的新值，其余 15 项首发与重建必须一致。
// 这里照文档写死取值，不复用生产常量，避免常量写错时测试跟着一起错。
func detailQueryWant(kbjcmsid string) map[string]string {
	return map[string]string{
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
		"kbjcmsid":  kbjcmsid,
	}
}

// assertDetailQuery 逐项断言明细请求的全部查询参数，并要求字段个数与文档完全一致。
// 首发与重登录后的重建请求共用这一份期望表：只抽查几个字段会漏掉重建时丢参数。
func assertDetailQuery(t *testing.T, query url.Values, want map[string]string) {
	t.Helper()
	for name, wantValue := range want {
		if got := query.Get(name); got != wantValue {
			t.Fatalf("查询参数 %s 期望 %q，实际 %q", name, wantValue, got)
		}
	}
	// 空参数也要显式带上，多带或少带都会改变上游的查询范围。
	if got := len(query); got != len(want) {
		t.Fatalf("查询参数期望 %d 个，实际 %d 个: %v", len(want), got, query)
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

	assertDetailQuery(t, req.query, detailQueryWant(detailTestKBJCMSID))
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
// 学期页夹具的学期编号故意与调用方不同，所以还能锁住 xnxqh 仍来自调用方 Term；
// 首发与重建的 16 个查询参数逐项断言，重登录与学期页刷新各只一次。
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

	// 首发用调用方传入的 kbjcmsid，16 个查询参数逐项对齐文档。
	assertDetailQuery(t, detailReqs[0].query, detailQueryWant(detailTestKBJCMSID))
	// 重建只换 kbjcmsid，其余 15 个参数必须原样保留。
	assertDetailQuery(t, detailReqs[1].query, detailQueryWant(detailRefreshedKBJCMSID))
	if first, second := detailReqs[0].query.Get("kbjcmsid"), detailReqs[1].query.Get("kbjcmsid"); first == second {
		t.Fatalf("两次明细请求的 kbjcmsid 必须不同，实际都是 %q", first)
	}

	// 学期页夹具必须给另一个学期编号，否则这条断言锁不住「xnxqh 不跟着刷新页走」。
	if detailRefreshedTerm == detailTestTerm {
		t.Fatal("刷新后的学期页夹具必须用不同学期编号，否则锁不住 xnxqh 的来源")
	}
	if got := detailReqs[1].query.Get("kbjcmsid"); got != detailRefreshedKBJCMSID {
		t.Fatalf("重跑必须用学期页的新 kbjcmsid %q，实际 %q", detailRefreshedKBJCMSID, got)
	}
	if got, want := detailReqs[1].query.Get("xnxqh"), detailParams().Term; got != want {
		t.Fatalf("重建后的 xnxqh 期望调用方 Term %q，实际 %q", want, got)
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

// TestFetchOccupancyDetailRefreshFailsWhenTermPageExpired 锁定刷新分支的第一条错误路径：
// 重登录后学期页仍是登录页时，把会话错误交回上层，不再重跑明细、也不再重登录第二次。
func TestFetchOccupancyDetailRefreshFailsWhenTermPageExpired(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost}
	f.businessBodies = []string{fixtureSessionLost}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	_, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if !errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("学期页仍失效时应返回会话错误，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("登录页不是学期内容非法，实际 err=%v", err)
	}

	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("只允许重登录一次，期望共 2 次提交，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 1 {
		t.Fatalf("学期页只应重取一次，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, occupancyDetailPath)); got != 1 {
		t.Fatalf("学期页失效时不应重跑明细，期望 1 次，实际 %d 次", got)
	}
}

// TestFetchOccupancyDetailRefreshRejectsMissingKBJCMSID 锁定刷新分支的第二条错误路径：
// 重登录后学期页缺 kbjcmsid 时返回学期内容非法，不二次登录、也不重跑明细。
func TestFetchOccupancyDetailRefreshRejectsMissingKBJCMSID(t *testing.T) {
	// 有学期编号、但没有 id/name 为 kbjcmsid 的隐藏输入。
	const termPageWithoutKBJCMSID = `<html><body><div>` + detailRefreshedTerm + `</div></body></html>`

	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost}
	f.businessBodies = []string{termPageWithoutKBJCMSID}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	_, err := c.FetchOccupancyDetail(t.Context(), detailParams())
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("学期页缺 kbjcmsid 应返回学期内容非法，实际 err=%v", err)
	}
	if !errors.Is(err, errKBJCMSIDNotFound) {
		t.Fatalf("错误应能追踪到缺 kbjcmsid，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrUpstreamSessionExpired) {
		t.Fatalf("内容缺失不是会话失效，实际 err=%v", err)
	}

	reqs := f.upstreamSnapshot()
	// 首次登录 + 一次重登录；内容非法不能再触发第二次重登录。
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("不应二次登录，期望共 2 次提交，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, termPagePath)); got != 1 {
		t.Fatalf("学期页只应重取一次，实际 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, occupancyDetailPath)); got != 1 {
		t.Fatalf("学期参数重建失败时不应重跑明细，期望 1 次，实际 %d 次", got)
	}
}

// TestFetchOccupancyDetailRefreshRequestOrder 锁定刷新分支的请求序列：
// 明细首发 → 会话失效 → 完整重登录 → 学期页刷新 → 明细重跑。
// 期望序列按夹具记录的上游请求逐条比对，顺序错位（例如先刷新学期页再登录）会在这里炸。
func TestFetchOccupancyDetailRefreshRequestOrder(t *testing.T) {
	f := newLoginFixture(t)
	f.occupancyDetailBodies = []string{fixtureSessionLost, fixtureOccupancyDetail}
	f.businessBodies = []string{fixtureRefreshedTermPage}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	if _, err := c.FetchOccupancyDetail(t.Context(), detailParams()); err != nil {
		t.Fatalf("刷新后明细页应成功，实际 err=%v", err)
	}

	want := append([]wantRequest{}, loginFlowOrder...)
	want = append(want, wantRequest{http.MethodGet, occupancyDetailPath})
	want = append(want, loginFlowOrder...)
	want = append(want,
		wantRequest{http.MethodGet, termPagePath},
		wantRequest{http.MethodGet, occupancyDetailPath},
	)

	reqs := f.upstreamSnapshot()
	if len(reqs) != len(want) {
		t.Fatalf("刷新分支应发出 %d 次上游请求，实际 %d 次: %+v", len(want), len(reqs), reqs)
	}
	for i, wantReq := range want {
		if reqs[i].method != wantReq.method || reqs[i].path != wantReq.path {
			t.Fatalf("第 %d 次请求期望 %s %s，实际 %s %s",
				i+1, wantReq.method, wantReq.path, reqs[i].method, reqs[i].path)
		}
	}
}
