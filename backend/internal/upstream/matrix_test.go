package upstream

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// matrixTestTerm 是周矩阵夹具里的学期编号。
const matrixTestTerm = "2026-2027-1"

// TestFetchWeekMatrixSendsBuildingAndWeek 覆盖 Requirement 4.1：
// POST 矩阵端点，楼编号进 jxlbh、周次同时写 zc 与 zc2、教室关键词留空，
// jc/jc2/jszt 一律不传；正文原样返回给 parser，一次调用只发一次请求。
func TestFetchWeekMatrixSendsBuildingAndWeek(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	body, err := c.FetchWeekMatrix(t.Context(), WeekMatrixParams{
		Term:       matrixTestTerm,
		BuildingID: "B001",
		Week:       5,
	})
	if err != nil {
		t.Fatalf("请求周矩阵应成功，实际 err=%v", err)
	}
	if !bytes.Equal(body, []byte(fixtureWeekMatrix)) {
		t.Fatalf("正文应原样返回，实际 %q", body)
	}

	reqs := requestsFor(f.upstreamSnapshot(), http.MethodPost, weekMatrixPath)
	if len(reqs) != 1 {
		t.Fatalf("每楼每周只应发 1 次矩阵请求，实际 %d 次", len(reqs))
	}
	req := reqs[0]
	if got := req.cookie; got != "JSESSIONID=session-1" {
		t.Fatalf("矩阵页必须复用登录会话，实际 Cookie=%q", got)
	}

	wantForm := map[string]string{
		"typewhere": "jszq",
		"xnxqh":     matrixTestTerm,
		"jxlbh":     "B001",
		"jsmc_mh":   "",
		"zc":        "5",
		"zc2":       "5",
		"xq":        "1",
		"xq2":       "7",
	}
	for name, want := range wantForm {
		if got := req.form.Get(name); got != want {
			t.Fatalf("表单 %s 期望 %q，实际 %q", name, want, got)
		}
	}
	// 字段数必须与文档一致：jc/jc2/jszt 不传，多带会改变查询范围。
	if got := len(req.form); got != len(wantForm) {
		t.Fatalf("表单期望 %d 个字段，实际 %d 个: %v", len(wantForm), got, req.form)
	}
}

// TestFetchWeekMatrixSendsOneRequestPerCall 锁定节奏：本层不按周循环、不批量，
// 每个「楼 × 周」由调用方各调一次；两次调用只对应两次请求，周次不串。
func TestFetchWeekMatrixSendsOneRequestPerCall(t *testing.T) {
	f := newLoginFixture(t)
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	for _, week := range []int{5, 6} {
		if _, err := c.FetchWeekMatrix(t.Context(), WeekMatrixParams{
			Term:       matrixTestTerm,
			BuildingID: "B001",
			Week:       week,
		}); err != nil {
			t.Fatalf("请求第 %d 周矩阵应成功，实际 err=%v", week, err)
		}
	}

	reqs := requestsFor(f.upstreamSnapshot(), http.MethodPost, weekMatrixPath)
	if len(reqs) != 2 {
		t.Fatalf("两次调用期望 2 次请求，实际 %d 次", len(reqs))
	}
	for i, want := range []string{"5", "6"} {
		if got := reqs[i].form.Get("zc"); got != want {
			t.Fatalf("第 %d 次请求 zc 期望 %q，实际 %q", i+1, want, got)
		}
		if got := reqs[i].form.Get("zc2"); got != want {
			t.Fatalf("第 %d 次请求 zc2 期望 %q，实际 %q", i+1, want, got)
		}
	}
}

// TestFetchWeekMatrixRejectsInvalidParams 覆盖参数缺失：空学期编号、空楼编号与
// 非正周次都在发请求前失败，不打上游。
func TestFetchWeekMatrixRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name   string
		params WeekMatrixParams
		want   error
	}{
		{
			name:   "学期编号为空",
			params: WeekMatrixParams{Term: "", BuildingID: "B001", Week: 5},
			want:   errEmptyTerm,
		},
		{
			name:   "学期编号只有空白",
			params: WeekMatrixParams{Term: "  ", BuildingID: "B001", Week: 5},
			want:   errEmptyTerm,
		},
		{
			name:   "楼编号为空",
			params: WeekMatrixParams{Term: matrixTestTerm, BuildingID: "", Week: 5},
			want:   errEmptyBuilding,
		},
		{
			name:   "楼编号只有空白",
			params: WeekMatrixParams{Term: matrixTestTerm, BuildingID: "  ", Week: 5},
			want:   errEmptyBuilding,
		},
		{
			name:   "周次为 0",
			params: WeekMatrixParams{Term: matrixTestTerm, BuildingID: "B001", Week: 0},
			want:   errBadWeek,
		},
		{
			name:   "周次为负",
			params: WeekMatrixParams{Term: matrixTestTerm, BuildingID: "B001", Week: -3},
			want:   errBadWeek,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoginFixture(t)
			c := newTestClient(t, f.srv.URL, WithMinInterval(0))

			_, err := c.FetchWeekMatrix(t.Context(), tc.params)
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

// TestFetchWeekMatrixReloginOnceOnSessionExpiry 覆盖 Requirement 2.5、4.1：
// 矩阵页会话失效时经 doWithRelogin 重登录一次后重跑当前请求。
func TestFetchWeekMatrixReloginOnceOnSessionExpiry(t *testing.T) {
	f := newLoginFixture(t)
	f.weekMatrixBodies = []string{fixtureSessionLost, fixtureWeekMatrix}
	c := newTestClient(t, f.srv.URL, WithMinInterval(0))
	f.mustLogin(t, c)

	if _, err := c.FetchWeekMatrix(t.Context(), WeekMatrixParams{
		Term:       matrixTestTerm,
		BuildingID: "B001",
		Week:       5,
	}); err != nil {
		t.Fatalf("重登录后矩阵页应成功，实际 err=%v", err)
	}
	reqs := f.upstreamSnapshot()
	if got := len(logonLDAPRequests(reqs)); got != 2 {
		t.Fatalf("期望首次登录 + 一次重登录，实际提交 %d 次", got)
	}
	if got := len(requestsFor(reqs, http.MethodPost, weekMatrixPath)); got != 2 {
		t.Fatalf("期望矩阵页重跑一次，实际 %d 次", got)
	}
}
