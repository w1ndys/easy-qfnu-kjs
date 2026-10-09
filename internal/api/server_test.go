// 本文件是 entry 层的单元测试：三个端点的响应码、四段式与 items 形状。
// 用假 Querier 替换业务层，不依赖数据库。

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// fakeQuerier 是查询服务的假实现。
type fakeQuerier struct {
	contextResult model.CalendarContext // Context 的预设返回
	contextErr    error                 // Context 的预设错误
	metaResult    query.MetaResult      // Meta 的预设返回
	metaErr       error                 // Meta 的预设错误
	queryResult   query.QueryResult     // Query 的预设返回
	queryErr      error                 // Query 的预设错误
	calls         int                   // Query 被调用的次数
	lastParams    query.Params          // 最近一次 Query 的入参
}

// Context 返回预设的时间位置。
func (f *fakeQuerier) Context(ctx context.Context) (model.CalendarContext, error) {
	return f.contextResult, f.contextErr
}

// Meta 返回预设的元信息。
func (f *fakeQuerier) Meta(ctx context.Context) (query.MetaResult, error) {
	return f.metaResult, f.metaErr
}

// Query 记录入参并返回预设结果，便于断言参数是否被正确解析。
func (f *fakeQuerier) Query(ctx context.Context, params query.Params) (query.QueryResult, error) {
	f.calls++
	f.lastParams = params
	return f.queryResult, f.queryErr
}

// testEnvelope 是断言用的响应结构，Data 保留原始 JSON 以便按需解析。
type testEnvelope struct {
	Code      int             `json:"code"`       // 业务响应码
	Message   string          `json:"message"`    // 说明文本
	Data      json.RawMessage `json:"data"`       // 原始 data，按用例自行解析
	RequestID string          `json:"request_id"` // 服务端追踪号
}

// doGet 用假服务跑一次 GET 并解析响应。
func doGet(t *testing.T, querier Querier, target string) (int, testEnvelope) {
	t.Helper()

	handler := NewServer(querier, time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, target, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	var body testEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v，原文 %s", err, recorder.Body.String())
	}
	return recorder.Code, body
}

// TestContextEnvelope 校验成功响应的四段式与时间位置字段。
func TestContextEnvelope(t *testing.T) {
	querier := &fakeQuerier{contextResult: model.CalendarContext{
		Date:       time.Date(2025, 9, 15, 0, 0, 0, 0, time.UTC),
		Term:       "2025-2026-1",
		Week:       2,
		Weekday:    1,
		InCalendar: true,
	}}

	status, body := doGet(t, querier, "/api/context")
	// 契约只定义响应体里的 code，因此 HTTP 状态固定 200
	if status != http.StatusOK {
		t.Errorf("HTTP 状态 = %d，期望 200", status)
	}
	if body.Code != CodeOK {
		t.Errorf("code = %d，期望 %d", body.Code, CodeOK)
	}
	// 追踪号由服务端生成，必须存在
	if !strings.HasPrefix(body.RequestID, "req_") {
		t.Errorf("request_id = %q，期望 req_ 前缀", body.RequestID)
	}

	var data contextBody
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if data.Date != "2025-09-15" || data.Term != "2025-2026-1" || data.Week != 2 || data.Weekday != 1 || !data.InCalendar {
		t.Errorf("context 数据不符：%+v", data)
	}
}

// TestQueryParamError 校验参数错误返回 40001 且不查库。
func TestQueryParamError(t *testing.T) {
	querier := &fakeQuerier{}

	_, body := doGet(t, querier, "/api/query")
	if body.Code != CodeBadRequest {
		t.Errorf("code = %d，期望 %d", body.Code, CodeBadRequest)
	}
	// 参数都没过校验，业务层不应被调用
	if querier.calls != 0 {
		t.Errorf("参数错误时仍调用了业务层 %d 次", querier.calls)
	}
	// 失败响应的 data 必须是 null
	if string(body.Data) != "null" {
		t.Errorf("data = %s，期望 null", string(body.Data))
	}
}

// TestQueryAvailability 校验空教室视图的 items 形状与参数透传。
func TestQueryAvailability(t *testing.T) {
	querier := &fakeQuerier{queryResult: query.QueryResult{
		Total:        1,
		Availability: []query.AvailabilityItem{{ID: "0899", Name: "F126"}},
		Date:         time.Date(2025, 9, 15, 0, 0, 0, 0, time.UTC),
		Week:         2,
		Weekday:      1,
	}}

	_, body := doGet(t, querier, "/api/query?view=availability&keyword=F126&start_node=03&end_node=05")
	if body.Code != CodeOK {
		t.Fatalf("code = %d，期望 0", body.Code)
	}
	// 参数解析结果要原样交给业务层
	if querier.lastParams.StartNode != "03" || querier.lastParams.EndNode != "05" || querier.lastParams.NodeCount != 3 {
		t.Errorf("透传参数不符：%+v", querier.lastParams)
	}

	var data struct {
		Total     int                `json:"total"`       // 命中总数
		Items     []availabilityItem `json:"items"`       // 空教室列表
		Date      string             `json:"date"`        // 目标日期
		Week      int                `json:"week"`        // 教学周次
		DayOfWeek int                `json:"day_of_week"` // 星期
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if data.Total != 1 || len(data.Items) != 1 || data.Items[0].ID != "0899" {
		t.Errorf("空教室数据不符：%+v", data)
	}
	if data.Date != "2025-09-15" || data.Week != 2 || data.DayOfWeek != 1 {
		t.Errorf("日期字段不符：%+v", data)
	}
}

// TestQueryDay 校验全天状态视图输出 12 小节的状态键。
func TestQueryDay(t *testing.T) {
	querier := &fakeQuerier{queryResult: query.QueryResult{
		Total: 1,
		Day: []query.DayItem{{
			ID:   "0899",
			Name: "F126",
			Statuses: map[string]model.StateKey{
				"01": model.StateFree,
				"02": model.StateClass,
			},
		}},
		Date:    time.Date(2025, 9, 15, 0, 0, 0, 0, time.UTC),
		Week:    2,
		Weekday: 1,
	}}

	_, body := doGet(t, querier, "/api/query?view=day")
	if body.Code != CodeOK {
		t.Fatalf("code = %d，期望 0", body.Code)
	}

	var data struct {
		Items []dayItem `json:"items"` // 全天状态列表
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if len(data.Items) != 1 {
		t.Fatalf("items 长度 = %d，期望 1", len(data.Items))
	}
	// 状态以语义键输出，不是整数 ID
	if data.Items[0].Statuses["01"] != "free" || data.Items[0].Statuses["02"] != "class" {
		t.Errorf("状态键不符：%+v", data.Items[0].Statuses)
	}
}

// TestErrorMapping 校验无数据与内部错误的响应码映射。
func TestErrorMapping(t *testing.T) {
	noData := &fakeQuerier{queryErr: query.NoDataError{Message: "该日期没有已发布数据"}}
	if _, body := doGet(t, noData, "/api/query?view=day"); body.Code != CodeNoData {
		t.Errorf("无数据时 code = %d，期望 %d", body.Code, CodeNoData)
	}

	internal := &fakeQuerier{metaErr: errors.New("数据库炸了")}
	_, body := doGet(t, internal, "/api/meta")
	if body.Code != CodeInternal {
		t.Errorf("内部错误时 code = %d，期望 %d", body.Code, CodeInternal)
	}
	// 内部细节只进日志，不回给客户端
	if strings.Contains(body.Message, "数据库炸了") {
		t.Errorf("响应里泄露了内部错误细节：%q", body.Message)
	}
}

// TestMetaBody 校验 meta 数据载荷的关键字段。
func TestMetaBody(t *testing.T) {
	lastSuccess := time.Date(2025, 9, 15, 4, 10, 12, 0, time.UTC)
	querier := &fakeQuerier{metaResult: query.MetaResult{
		ReleaseID:   "20261003T041012+0800",
		GeneratedAt: lastSuccess,
		DictVersion: 1,
		AxisVersion: 1,
		States:      map[string]query.MetaState{"free": {Label: "空闲", Available: true}},
		Blocks:      []string{"0102", "030405"},
		Nodes:       []query.MetaNode{{Code: "01", Block: "0102"}},
		Terms: []query.MetaTerm{{
			Term:       "2025-2026-1",
			TotalWeeks: 20,
			Weeks: []query.MetaWeek{{
				Week:          2,
				Monday:        time.Date(2025, 9, 15, 0, 0, 0, 0, time.UTC),
				Published:     true,
				LastSuccessAt: &lastSuccess,
				Stale:         false,
			}},
		}},
	}}

	_, body := doGet(t, querier, "/api/meta")
	if body.Code != CodeOK {
		t.Fatalf("code = %d，期望 0", body.Code)
	}

	var data metaBody
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("解析 data 失败: %v", err)
	}
	if data.ReleaseID != "20261003T041012+0800" || data.DictVersion != 1 || data.AxisVersion != 1 {
		t.Errorf("meta 版本字段不符：%+v", data)
	}
	if data.States["free"].Label != "空闲" || !data.States["free"].Available {
		t.Errorf("states 不符：%+v", data.States)
	}
	if len(data.Axis.Nodes) != 1 || data.Axis.Nodes[0].Code != "01" {
		t.Errorf("axis.nodes 不符：%+v", data.Axis.Nodes)
	}
	if len(data.Terms) != 1 || len(data.Terms[0].Weeks) != 1 {
		t.Fatalf("terms 不符：%+v", data.Terms)
	}
	week := data.Terms[0].Weeks[0]
	if week.Monday != "2025-09-15" || !week.Published || week.LastSuccessAt == nil || week.Stale {
		t.Errorf("逐周字段不符：%+v", week)
	}
}
