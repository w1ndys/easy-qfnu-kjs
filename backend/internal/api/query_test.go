package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// fakeAvailabilityProvider 记录入参并返回预设结果。
type fakeAvailabilityProvider struct {
	result    query.AvailabilityResult
	err       error
	calls     int
	gotNow    time.Time
	gotParams query.AvailabilityParams
}

// Availability 实现 AvailabilityProvider。
func (f *fakeAvailabilityProvider) Availability(_ context.Context, now time.Time, params query.AvailabilityParams) (query.AvailabilityResult, error) {
	f.calls++
	f.gotNow = now
	f.gotParams = params
	return f.result, f.err
}

// fixedQueryClock 返回固定时刻，避免用例依赖真实时间。
func fixedQueryClock() time.Time {
	return time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
}

// serveQuery 用给定查询串打一次 /api/query。
func serveQuery(t *testing.T, srv *Server, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/query?"+values.Encode(), nil))
	return rec
}

// TestQueryAvailabilityOK 覆盖 /api/query?view=availability 的成功响应形状与参数透传。
func TestQueryAvailabilityOK(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{
		Total: 2,
		Items: []query.AvailabilityRoom{
			{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Date: "2026-10-23", Term: "2026-2027-1", Week: 5, Weekday: 5,
	}}
	srv := New(quietLogger(), nil, nil, fake)
	now := fixedQueryClock()
	srv.clock = func() time.Time { return now }

	values := url.Values{}
	values.Set("view", "availability")
	values.Set("date_offset", "2")
	values.Set("start_node", "03")
	values.Set("end_node", "05")
	values.Set("building", " Q10 ")
	values.Set("keyword", "Ｆ１２６")
	values.Set("limit", "10")
	values.Set("offset", "20")

	rec := serveQuery(t, srv, values)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Code      int                      `json:"code"`
		Message   string                   `json:"message"`
		Data      query.AvailabilityResult `json:"data"`
		RequestID string                   `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != 0 || got.Data.Total != 2 || len(got.Data.Items) != 1 {
		t.Fatalf("响应内容不符: %+v", got)
	}
	if got.Data.Items[0].RoomID != "0899" || got.Data.Items[0].BuildingName != "综合教学楼" {
		t.Fatalf("房间字段不符: %+v", got.Data.Items[0])
	}
	if got.Data.Date != "2026-10-23" || got.Data.Term != "2026-2027-1" || got.Data.Week != 5 || got.Data.Weekday != 5 {
		t.Fatalf("日期上下文不符: %+v", got.Data)
	}
	if len(got.RequestID) != len("req_")+32 || got.RequestID[:4] != "req_" {
		t.Fatalf("request_id 不符合 req_+32 位十六进制: %q", got.RequestID)
	}

	if !fake.gotNow.Equal(now) {
		t.Fatalf("时钟透传不符: %s", fake.gotNow)
	}
	wantParams := query.AvailabilityParams{
		DateOffset: 2,
		Building:   "Q10",
		Keyword:    "F126",
		StartNode:  "03",
		EndNode:    "05",
		NodeCount:  3,
		Limit:      10,
		Offset:     20,
	}
	if fake.gotParams != wantParams {
		t.Fatalf("参数透传不符:\n实际 %+v\n期望 %+v", fake.gotParams, wantParams)
	}
}

// TestQueryAvailabilityEmptyIsOKWithHint 覆盖情形 B：
// 空结果返回 HTTP 200、code 0、空数组与页顶提示，而不是 40401。
func TestQueryAvailabilityEmptyIsOKWithHint(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{
		Total: 0,
		Items: []query.AvailabilityRoom{},
		Date:  "2026-10-21", Term: "2026-2027-1", Week: 5, Weekday: 3,
		Hint: &query.AvailabilityHint{Code: query.HintWeekNoSchedule, Message: "该周疑似无排课"},
	}}
	srv := New(quietLogger(), nil, nil, fake)
	srv.clock = fixedQueryClock

	rec := serveQuery(t, srv, url.Values{"view": {"availability"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("空结果期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Code int `json:"code"`
		Data struct {
			Total int                     `json:"total"`
			Items []json.RawMessage       `json:"items"`
			Hint  *query.AvailabilityHint `json:"hint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != 0 {
		t.Fatalf("空结果业务码期望 0，实际 %d", got.Code)
	}
	if got.Data.Total != 0 {
		t.Fatalf("总数期望 0，实际 %d", got.Data.Total)
	}
	if got.Data.Items == nil || len(got.Data.Items) != 0 {
		t.Fatalf("items 必须是空数组，实际 %#v", got.Data.Items)
	}
	if got.Data.Hint == nil || got.Data.Hint.Code != query.HintWeekNoSchedule {
		t.Fatalf("期望提示 %q，实际 %+v", query.HintWeekNoSchedule, got.Data.Hint)
	}
	if got.Data.Hint.Message != "该周疑似无排课" {
		t.Fatalf("提示文案不符: %q", got.Data.Hint.Message)
	}
}

// TestQueryAvailabilityDefaults 覆盖省略参数时的最小默认值。
func TestQueryAvailabilityDefaults(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{Items: []query.AvailabilityRoom{}}}
	srv := New(quietLogger(), nil, nil, fake)
	srv.clock = fixedQueryClock

	rec := serveQuery(t, srv, url.Values{})
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	want := query.AvailabilityParams{
		DateOffset: 0,
		StartNode:  query.DefaultStartNode,
		EndNode:    query.DefaultEndNode,
		NodeCount:  11,
		Limit:      query.DefaultLimit,
		Offset:     0,
	}
	if fake.gotParams != want {
		t.Fatalf("默认参数不符:\n实际 %+v\n期望 %+v", fake.gotParams, want)
	}
}

// TestQueryAvailabilityRejectsInvalidParams 覆盖缺失/非法参数的安全处理：
// 一律 40001，绝不 panic，也不进入查询服务。
func TestQueryAvailabilityRejectsInvalidParams(t *testing.T) {
	cases := []struct {
		name   string
		values url.Values
	}{
		{name: "day 视图尚未实现", values: url.Values{"view": {"day"}}},
		{name: "未知视图", values: url.Values{"view": {"unknown"}}},
		{name: "日期偏移非整数", values: url.Values{"date_offset": {"abc"}}},
		{name: "起始节次 00", values: url.Values{"start_node": {"00"}}},
		{name: "起始晚于结束", values: url.Values{"start_node": {"05"}, "end_node": {"03"}}},
		{name: "limit 非整数", values: url.Values{"limit": {"abc"}}},
		{name: "limit 为 0", values: url.Values{"limit": {"0"}}},
		{name: "limit 超上界", values: url.Values{"limit": {"201"}}},
		{name: "offset 为负", values: url.Values{"offset": {"-1"}}},
		{name: "关键词超长", values: url.Values{"keyword": {strings.Repeat("a", query.MaxKeywordRunes+1)}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{Items: []query.AvailabilityRoom{}}}
			srv := New(quietLogger(), nil, nil, fake)
			srv.clock = fixedQueryClock

			rec := serveQuery(t, srv, c.values)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，实际 %d，body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if got.Code != 40001 || got.Message == "" {
				t.Fatalf("期望业务码 40001 与非空描述，实际 %+v", got)
			}
			if fake.calls != 0 {
				t.Fatalf("非法参数不应进入查询服务，实际调用 %d 次", fake.calls)
			}
		})
	}
}

// TestQueryAvailabilityFailure 覆盖查询失败：500 + 50000，且不泄露内部错误。
func TestQueryAvailabilityFailure(t *testing.T) {
	fake := &fakeAvailabilityProvider{err: errors.New("db down")}
	srv := New(quietLogger(), nil, nil, fake)
	srv.clock = fixedQueryClock

	rec := serveQuery(t, srv, url.Values{"view": {"availability"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	var got struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got.Code != 50000 {
		t.Fatalf("期望业务码 50000，实际 %d", got.Code)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
}

// TestQueryAvailabilityCanceled 覆盖客户端断开：不写响应、不打 ERROR。
func TestQueryAvailabilityCanceled(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(logger, nil, nil, &fakeAvailabilityProvider{err: context.Canceled})
	srv.clock = fixedQueryClock

	rec := serveQuery(t, srv, url.Values{"view": {"availability"}})
	if rec.Body.Len() != 0 {
		t.Fatalf("取消场景不该写响应体，实际 %q", rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("取消场景不该返回错误码，实际 %d", rec.Code)
	}
	if strings.Contains(logBuf.String(), `"level":"ERROR"`) {
		t.Fatalf("取消场景不该记 ERROR，实际日志 %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "客户端已断开") {
		t.Fatalf("取消场景应留一条 info，实际日志 %s", logBuf.String())
	}
}

// TestQueryAvailabilityJSONKeys 按契约键名断言，防止 json tag 漏写。
func TestQueryAvailabilityJSONKeys(t *testing.T) {
	fake := &fakeAvailabilityProvider{result: query.AvailabilityResult{
		Total: 1,
		Items: []query.AvailabilityRoom{
			{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Date: "2026-10-21", Term: "2026-2027-1", Week: 5, Weekday: 3,
	}}
	srv := New(quietLogger(), nil, nil, fake)
	srv.clock = fixedQueryClock

	rec := serveQuery(t, srv, url.Values{"view": {"availability"}})
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %v", got["data"])
	}
	for _, key := range []string{"total", "items", "date", "term", "week", "weekday"} {
		if _, exists := data[key]; !exists {
			t.Fatalf("data 缺少契约键 %q，实际键: %v", key, keysOf(data))
		}
	}
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 形状不符: %#v", data["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item 不是对象: %#v", items[0])
	}
	for _, key := range []string{"room_id", "name", "building_id", "building_name"} {
		if _, exists := item[key]; !exists {
			t.Fatalf("item 缺少契约键 %q，实际键: %v", key, keysOf(item))
		}
	}
}
