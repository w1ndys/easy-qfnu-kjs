package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// captureRecorder 记录查询 handler 投递的记账，用来断言接线位置与入参。
type captureRecorder struct {
	mu     sync.Mutex
	visits []stats.Visit
}

// Record 实现 StatsRecorder。
func (r *captureRecorder) Record(visit stats.Visit) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visits = append(r.visits, visit)
}

// snapshot 返回已记录的记账副本。
func (r *captureRecorder) snapshot() []stats.Visit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]stats.Visit{}, r.visits...)
}

// serveQueryWith 打一次 /api/query，并允许附加请求头。
func serveQueryWith(t *testing.T, srv *Server, values url.Values, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/query?"+values.Encode(), nil)
	for key, list := range header {
		for _, value := range list {
			req.Header.Add(key, value)
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestQueryRecordsAvailabilityVisit 覆盖空教室查询完成后投递一次记账，维度取自请求（Requirement 34.1、34.5、34.8）。
func TestQueryRecordsAvailabilityVisit(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{Date: "2026-10-23"}}
	recorder := &captureRecorder{}
	srv := New(quietLogger(), nil, nil, fake, nil, WithStats(recorder))
	now := fixedQueryClock()
	srv.clock = func() time.Time { return now }

	values := url.Values{}
	values.Set("start_node", "02")
	values.Set("end_node", "04")
	values.Set("building", " Q10 ")
	values.Set("keyword", "Ｆ１２６")

	rec := serveQueryWith(t, srv, values, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	visits := recorder.snapshot()
	if len(visits) != 1 {
		t.Fatalf("一次成功查询应记一次账，实际 %d 次", len(visits))
	}
	visit := visits[0]
	if visit.Building != "Q10" {
		t.Fatalf("楼维度应为 Q10，实际 %q", visit.Building)
	}
	if visit.Keyword != "F126" {
		t.Fatalf("关键词应按查询口径折成全半角，实际 %q", visit.Keyword)
	}
	if visit.StartNode != "02" || visit.EndNode != "04" {
		t.Fatalf("节次区间应为 02–04，实际 %q–%q", visit.StartNode, visit.EndNode)
	}
	if visit.RoomID != "" {
		t.Fatalf("空教室查询没有单房维度，实际 %q", visit.RoomID)
	}
	if visit.VisitorID != "" {
		t.Fatalf("未回传标识时应为空，实际 %q", visit.VisitorID)
	}
	if visit.IP != "192.0.2.1" {
		t.Fatalf("来源 IP 应取直连对端，实际 %q", visit.IP)
	}
}

// TestQueryRecordsDayVisit 覆盖全天状态查询记录房号维度（Requirement 34.5）。
func TestQueryRecordsDayVisit(t *testing.T) {
	fake := &fakeDayProvider{result: dayResultFixture()}
	recorder := &captureRecorder{}
	srv := New(quietLogger(), nil, nil, nil, fake, WithStats(recorder))
	now := fixedQueryClock()
	srv.clock = func() time.Time { return now }

	values := url.Values{}
	values.Set("view", "day")
	values.Set("room_id", "0899")
	values.Set("building", "Q10")

	rec := serveQueryWith(t, srv, values, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	visits := recorder.snapshot()
	if len(visits) != 1 {
		t.Fatalf("一次成功查询应记一次账，实际 %d 次", len(visits))
	}
	visit := visits[0]
	if visit.RoomID != "0899" || visit.Building != "Q10" {
		t.Fatalf("期望房号 0899、楼 Q10，实际 %+v", visit)
	}
	if visit.Keyword != "" || visit.StartNode != "" || visit.EndNode != "" {
		t.Fatalf("全天状态查询没有关键词与节次区间，实际 %+v", visit)
	}
}

// TestQueryRecordsVisitorIDFromHeaderOrParam 覆盖匿名标识的两条来源与优先级（Requirement 34.4）。
func TestQueryRecordsVisitorIDFromHeaderOrParam(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		values url.Values
		want   string
	}{
		{
			name:   "请求头",
			header: http.Header{visitorIDHeader: {"from-header"}},
			values: url.Values{},
			want:   "from-header",
		},
		{
			name:   "查询参数",
			values: url.Values{visitorIDParam: {"from-param"}},
			want:   "from-param",
		},
		{
			name:   "请求头优先",
			header: http.Header{visitorIDHeader: {"from-header"}},
			values: url.Values{visitorIDParam: {"from-param"}},
			want:   "from-header",
		},
		{
			name:   "都没有",
			values: url.Values{},
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &captureRecorder{}
			srv := New(quietLogger(), nil, nil,
				&fakeAvailabilityProvider{result: query.AvailabilityResult{Date: "2026-10-23"}}, nil,
				WithStats(recorder))
			srv.clock = fixedQueryClock

			rec := serveQueryWith(t, srv, tc.values, tc.header)
			if rec.Code != http.StatusOK {
				t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
			}

			visits := recorder.snapshot()
			if len(visits) != 1 {
				t.Fatalf("期望记一次账，实际 %d 次", len(visits))
			}
			if visits[0].VisitorID != tc.want {
				t.Fatalf("期望匿名标识 %q，实际 %q", tc.want, visits[0].VisitorID)
			}
		})
	}
}

// TestQueryDoesNotRecordRejectedQuery 覆盖只有成功完成的查询才记账：参数错误不入账。
func TestQueryDoesNotRecordRejectedQuery(t *testing.T) {
	recorder := &captureRecorder{}
	srv := New(quietLogger(), nil, nil, nil, &fakeDayProvider{}, WithStats(recorder))
	srv.clock = fixedQueryClock

	values := url.Values{}
	values.Set("view", "day") // 缺 room_id
	rec := serveQueryWith(t, srv, values, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("参数错误不应记账，实际记了 %d 次", got)
	}
}

// TestNonQueryEndpointsDoNotRecord 覆盖只有 /api/query 记账：context 与 meta 不计入 PV。
func TestNonQueryEndpointsDoNotRecord(t *testing.T) {
	store := newContractStore()
	recorder := &captureRecorder{}
	svc := query.New(store.Week, store.Availability, store.Day)
	metaSvc := query.NewMeta(store.Meta, query.NewFreshness(store.Weeks))
	srv := New(quietLogger(), okPinger{}, svc, svc, svc, WithMeta(metaSvc), WithStats(recorder))
	srv.clock = fixedQueryClock

	for _, path := range []string{"/api/context", "/api/meta"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 期望 200，实际 %d", path, rec.Code)
		}
	}
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("context 与 meta 不应记账，实际记了 %d 次", got)
	}
}

// TestKeywordLimitMatchesStatsTruncation 覆盖关键词入口上限与统计截断的联动（Requirement 34.5）：
// /api/query 的 keyword 上限按字符计，长度与 stats.MaxKeywordRunes 相同；
// 恰好到上限的合法输入进统计后不被截断，超一个字符的输入在入口就被拒绝、根本不记账。
func TestKeywordLimitMatchesStatsTruncation(t *testing.T) {
	if maxKeywordRunes != stats.MaxKeywordRunes {
		t.Fatalf("入口上限 %d 与统计侧截断 %d 必须一致，否则要么白截、要么放过超长键",
			maxKeywordRunes, stats.MaxKeywordRunes)
	}

	// 恰好 32 个字符（中文按字符计，不是字节）。
	atLimit := strings.Repeat("教", maxKeywordRunes)
	recorder := &captureRecorder{}
	srv := New(quietLogger(), nil, nil,
		&fakeAvailabilityProvider{result: query.AvailabilityResult{Date: "2026-10-23"}}, nil,
		WithStats(recorder))
	srv.clock = fixedQueryClock

	values := url.Values{}
	values.Set("keyword", atLimit)
	rec := serveQueryWith(t, srv, values, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("上限内的关键词期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	visits := recorder.snapshot()
	if len(visits) != 1 {
		t.Fatalf("一次成功查询应记一次账，实际 %d 次", len(visits))
	}
	if visits[0].Keyword != atLimit {
		t.Fatalf("入口放行的关键词应原样记账，实际 %q（%d 字符）",
			visits[0].Keyword, utf8.RuneCountInString(visits[0].Keyword))
	}
	// 统计侧对同一输入不做任何截断：两边上限一致时，入口允许的最长值正好落满。
	if got := stats.NormalizeKeyword(visits[0].Keyword); got != atLimit {
		t.Fatalf("统计归一化不该截断入口允许的关键词，实际 %q", got)
	}

	// 多一个字符：入口直接 400，不记账；统计侧的截断只兜别的调用方。
	overLimit := strings.Repeat("教", maxKeywordRunes+1)
	if got := utf8.RuneCountInString(stats.NormalizeKeyword(overLimit)); got != stats.MaxKeywordRunes {
		t.Fatalf("统计侧应把超长关键词截到 %d 个字符，实际 %d 个", stats.MaxKeywordRunes, got)
	}

	recorder = &captureRecorder{}
	srv = New(quietLogger(), nil, nil,
		&fakeAvailabilityProvider{result: query.AvailabilityResult{Date: "2026-10-23"}}, nil,
		WithStats(recorder))
	srv.clock = fixedQueryClock
	values.Set("keyword", overLimit)
	rec = serveQueryWith(t, srv, values, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长关键词期望 400，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("被入口拒绝的查询不应记账，实际记了 %d 次", got)
	}
}

// TestStatsRecorderDoesNotChangeQueryResponse 覆盖装配记账前后响应字段一致（Requirement 34.1、CP18）。
func TestStatsRecorderDoesNotChangeQueryResponse(t *testing.T) {
	values := url.Values{}
	values.Set("building", "Q10")
	values.Set("keyword", "F126")

	// responseData 只取会影响契约的三段：业务码、文案与 data。
	type responseData struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	// 两次请求都走同一份 query 服务替身，唯一差别是是否装配记账。
	run := func(t *testing.T, recorder StatsRecorder) responseData {
		t.Helper()
		opts := []Option{}
		if recorder != nil {
			opts = append(opts, WithStats(recorder))
		}
		srv := New(quietLogger(), nil, nil,
			&fakeAvailabilityProvider{result: query.AvailabilityResult{
				Total: 1,
				Items: []query.AvailabilityRoom{{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"}},
				Date:  "2026-10-23",
			}}, nil,
			opts...)
		srv.clock = fixedQueryClock

		rec := serveQueryWith(t, srv, values, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
		}
		var got responseData
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("解析响应失败: %v", err)
		}
		return got
	}

	without := run(t, nil)
	with := run(t, &captureRecorder{})
	if without.Code != with.Code || without.Message != with.Message || string(without.Data) != string(with.Data) {
		t.Fatalf("装配记账改变了响应：无记账 %+v，有记账 %+v", without, with)
	}
}
