package upstream

import (
	"errors"
	"net/http"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestFetchBuildingsRequestsCampusAndParsesDM 覆盖 tasklist 3.4：
// GET 楼列表端点带 xqid 与 requestType=jxl，复用登录后的内存会话，
// 并把 dm/dmmc 原样映射成楼编号与展示名。
func TestFetchBuildingsRequestsCampusAndParsesDM(t *testing.T) {
	cases := []struct {
		name   string
		campus string
	}{
		{name: "曲阜", campus: CampusQufu},
		{name: "日照", campus: CampusRizhao},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))
			f.mustLogin(t, c)

			buildings, err := c.FetchBuildings(t.Context(), tc.campus)
			if err != nil {
				t.Fatalf("读取楼列表应成功，实际 err=%v", err)
			}
			want := []Building{
				{ID: "B001", Name: "1号教学楼"},
				{ID: "B002", Name: "2号教学楼"},
			}
			if len(buildings) != len(want) {
				t.Fatalf("楼列表期望 %d 项，实际 %+v", len(want), buildings)
			}
			for i := range want {
				if buildings[i] != want[i] {
					t.Fatalf("第 %d 项期望 %+v，实际 %+v", i+1, want[i], buildings[i])
				}
			}

			reqs := requestsFor(f.upstreamSnapshot(), http.MethodGet, buildingListPath)
			if len(reqs) != 1 {
				t.Fatalf("期望 1 次楼列表请求，实际 %d 次", len(reqs))
			}
			if got := reqs[0].query.Get("xqid"); got != tc.campus {
				t.Fatalf("xqid 期望 %q，实际 %q", tc.campus, got)
			}
			if got := reqs[0].query.Get("requestType"); got != "jxl" {
				t.Fatalf("requestType 期望 jxl，实际 %q", got)
			}
			// 只带 xqid 与 requestType：多带参数会被上游当成别的下拉。
			if got := len(reqs[0].query); got != 2 {
				t.Fatalf("查询参数期望 2 个，实际 %d 个: %v", got, reqs[0].query)
			}
			if got := reqs[0].cookie; got != "JSESSIONID=session-1" {
				t.Fatalf("楼列表必须复用登录会话，实际 Cookie=%q", got)
			}
		})
	}
}

// TestFetchBuildingsPassesThroughEmptyFields 锁定空编号/空名称的接受口径：
// docs/upstream-api.md 与旧实现都没有规定要过滤空 dm 或空 dmmc，本任务原样返回，
// 由白名单校验（要求楼编号非空）兜底；若产品要求丢弃空项，属于新的业务规则。
func TestFetchBuildingsPassesThroughEmptyFields(t *testing.T) {
	f := newLoginFixture(t)
	f.buildingListBodies = []string{
		`[{"dm":"","dmmc":"缺编号"},{"dm":"B002","dmmc":""},{"dm":"B003","dmmc":"3号教学楼"}]`,
	}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))

	buildings, err := c.FetchBuildings(t.Context(), CampusQufu)
	if err != nil {
		t.Fatalf("含空字段的楼列表应原样接受，实际 err=%v", err)
	}
	want := []Building{
		{ID: "", Name: "缺编号"},
		{ID: "B002", Name: ""},
		{ID: "B003", Name: "3号教学楼"},
	}
	if len(buildings) != len(want) {
		t.Fatalf("楼列表期望 %d 项，实际 %+v", len(want), buildings)
	}
	for i := range want {
		if buildings[i] != want[i] {
			t.Fatalf("第 %d 项期望 %+v，实际 %+v", i+1, want[i], buildings[i])
		}
	}
}

// TestFetchBuildingsAcceptsEmptyList 覆盖空数据不是错误：
// 空数组与 JSON null 都表示该校区没有楼，返回非 nil 空切片，方便上层直接序列化。
func TestFetchBuildingsAcceptsEmptyList(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "空数组", body: `[]`},
		{name: "JSON null", body: `null`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.buildingListBodies = []string{tc.body}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			buildings, err := c.FetchBuildings(t.Context(), CampusQufu)
			if err != nil {
				t.Fatalf("空楼列表不应报错，实际 err=%v", err)
			}
			if buildings == nil {
				t.Fatalf("期望非 nil 空切片，实际 nil")
			}
			if len(buildings) != 0 {
				t.Fatalf("期望空列表，实际 %+v", buildings)
			}
		})
	}
}

// TestFetchBuildingsRejectsEmptyCampus 覆盖参数缺失：校区 id 为空或只有空白时不发请求。
func TestFetchBuildingsRejectsEmptyCampus(t *testing.T) {
	cases := []struct {
		name   string
		campus string
	}{
		{name: "空串", campus: ""},
		{name: "只有空格", campus: "  "},
		{name: "空白字符", campus: "\t\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			_, err := c.FetchBuildings(t.Context(), tc.campus)
			if !errors.Is(err, errEmptyCampus) {
				t.Fatalf("期望命中空校区原因，实际 err=%v", err)
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

// TestFetchBuildingsRejectsMalformedBody 覆盖数据级失败：
// 空正文、HTML 错误页与 JSON 对象都不是楼列表数组，返回可区分的 errBuildingListInvalid，
// 不重试成 model.ErrUpstreamFailed。
func TestFetchBuildingsRejectsMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "空正文", body: ""},
		{name: "HTML 错误页", body: "<html><body>系统维护</body></html>"},
		{name: "对象而非数组", body: `{"dm":"B001","dmmc":"1号教学楼"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			f.buildingListBodies = []string{tc.body}
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			_, err := c.FetchBuildings(t.Context(), CampusQufu)
			if !errors.Is(err, errBuildingListInvalid) {
				t.Fatalf("期望命中楼列表内容非法，实际 err=%v", err)
			}
			if errors.Is(err, model.ErrUpstreamFailed) {
				t.Fatalf("内容非法不能混成上游请求失败，实际 err=%v", err)
			}
		})
	}
}

// TestFetchBuildingsReloginOnceOnSessionExpiry 覆盖 Requirement 2.5：
// 楼列表会话失效时经 doWithRelogin 重登录一次后重跑，不出现登录循环。
func TestFetchBuildingsReloginOnceOnSessionExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.buildingListBodies = []string{fixtureSessionLost, fixtureBuildingList}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	if _, err := c.FetchBuildings(t.Context(), CampusQufu); err != nil {
		t.Fatalf("重登录后楼列表应成功，实际 err=%v", err)
	}
	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodGet, buildingListPath)); got != 2 {
		t.Fatalf("期望楼列表重跑一次，实际 %d 次", got)
	}
}
