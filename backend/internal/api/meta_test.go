package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// fakeMetaProvider 记录入参并返回预设结果。
type fakeMetaProvider struct {
	result query.MetaResult
	err    error
	calls  int
	gotNow time.Time
}

// Meta 实现 MetaProvider。
func (f *fakeMetaProvider) Meta(_ context.Context, now time.Time) (query.MetaResult, error) {
	f.calls++
	f.gotNow = now
	return f.result, f.err
}

// serveMeta 打一次 /api/meta。
func serveMeta(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	return rec
}

// metaEnvelope 是 /api/meta 成功响应的数据形状。
type metaEnvelope struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    query.MetaResult `json:"data"`
	ReqID   string           `json:"request_id"`
}

// TestMetaOK 覆盖 /api/meta 的成功响应形状与时钟透传。
func TestMetaOK(t *testing.T) {
	generated := time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC)
	fake := &fakeMetaProvider{result: query.MetaResult{
		Release: &query.MetaRelease{ReleaseID: "20261023T071012Z", GeneratedAt: generated},
		Dict: &query.MetaDict{Version: 1, States: map[string]query.MetaState{
			"free": {Label: "空闲", Available: true},
		}},
		Axis: &query.MetaAxis{
			Version: 1,
			Blocks:  []string{"0102"},
			Nodes:   []query.MetaAxisNode{{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1}},
		},
		Buildings: []query.MetaBuilding{{BuildingID: "Q10", Name: "综合教学楼"}},
		Weeks: []query.WeekFreshness{
			{Week: 1, Published: true, LastSuccessAt: &generated},
		},
	}}

	srv := New(quietLogger(), nil, nil, nil, nil, WithMeta(fake))
	now := fixedQueryClock()
	srv.clock = func() time.Time { return now }

	rec := serveMeta(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got metaEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != 0 || got.Message != "success" {
		t.Fatalf("信封不符: %+v", got)
	}
	if len(got.ReqID) != len("req_")+32 || got.ReqID[:4] != "req_" {
		t.Fatalf("request_id 不符合 req_+32 位十六进制: %q", got.ReqID)
	}
	if got.Data.Release == nil || got.Data.Release.ReleaseID != "20261023T071012Z" {
		t.Fatalf("版本不符: %+v", got.Data.Release)
	}
	if got.Data.Dict == nil || got.Data.Dict.States["free"].Label != "空闲" {
		t.Fatalf("字典不符: %+v", got.Data.Dict)
	}
	if got.Data.Axis == nil || len(got.Data.Axis.Nodes) != 1 || got.Data.Axis.Nodes[0].Block != "0102" {
		t.Fatalf("轴不符: %+v", got.Data.Axis)
	}
	if len(got.Data.Buildings) != 1 || got.Data.Buildings[0].BuildingID != "Q10" {
		t.Fatalf("白名单楼不符: %+v", got.Data.Buildings)
	}
	if len(got.Data.Weeks) != 1 || got.Data.Weeks[0].Week != 1 {
		t.Fatalf("逐周状态不符: %+v", got.Data.Weeks)
	}
	if got.Data.Empty {
		t.Fatal("有当前版本时 empty 应为 false")
	}
	if !fake.gotNow.Equal(now) {
		t.Fatalf("时钟透传不符: %s", fake.gotNow)
	}
}

// TestMetaJSONKeys 按契约键名断言 data 各层，防止漏写 json tag 时字段名变成驼峰。
func TestMetaJSONKeys(t *testing.T) {
	fake := &fakeMetaProvider{result: query.MetaResult{
		Release: &query.MetaRelease{ReleaseID: "rel-1", GeneratedAt: fixedQueryClock()},
		Dict:    &query.MetaDict{Version: 1, States: map[string]query.MetaState{"free": {Label: "空闲", Available: true}}},
		Axis: &query.MetaAxis{Version: 1, Blocks: []string{"0102"},
			Nodes: []query.MetaAxisNode{{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1}}},
		Buildings: []query.MetaBuilding{{BuildingID: "Q10", Name: "综合教学楼"}},
		Weeks:     []query.WeekFreshness{{Week: 1, Published: true}},
	}}
	srv := New(quietLogger(), nil, nil, nil, nil, WithMeta(fake))

	var got map[string]any
	if err := json.Unmarshal(serveMeta(t, srv).Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %v", got["data"])
	}
	for _, key := range []string{"release", "dict", "axis", "buildings", "weeks", "empty"} {
		if _, exists := data[key]; !exists {
			t.Fatalf("缺少契约键 %q，实际键: %v", key, keysOf(data))
		}
	}
	release := data["release"].(map[string]any)
	for _, key := range []string{"release_id", "generated_at"} {
		if _, exists := release[key]; !exists {
			t.Fatalf("release 缺少契约键 %q，实际键: %v", key, keysOf(release))
		}
	}
	dict := data["dict"].(map[string]any)
	for _, key := range []string{"version", "states"} {
		if _, exists := dict[key]; !exists {
			t.Fatalf("dict 缺少契约键 %q，实际键: %v", key, keysOf(dict))
		}
	}
	axis := data["axis"].(map[string]any)
	for _, key := range []string{"version", "blocks", "nodes"} {
		if _, exists := axis[key]; !exists {
			t.Fatalf("axis 缺少契约键 %q，实际键: %v", key, keysOf(axis))
		}
	}
}

// TestMetaEmptyState 覆盖无当前版本：HTTP 200、code 0、release:null 与 empty:true，
// 不返回 40401，也不把 weeks 序列化成 null。
func TestMetaEmptyState(t *testing.T) {
	fake := &fakeMetaProvider{result: query.MetaResult{
		Buildings: []query.MetaBuilding{{BuildingID: "Q10", Name: "综合教学楼"}},
		Weeks:     []query.WeekFreshness{},
		Empty:     true,
	}}
	srv := New(quietLogger(), nil, nil, nil, nil, WithMeta(fake))

	rec := serveMeta(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("空态期望 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got["code"].(float64) != 0 {
		t.Fatalf("空态业务码期望 0，实际 %v", got["code"])
	}
	data := got["data"].(map[string]any)
	if data["release"] != nil {
		t.Fatalf("空态 release 期望 null，实际 %v", data["release"])
	}
	if data["empty"] != true {
		t.Fatalf("空态 empty 期望 true，实际 %v", data["empty"])
	}
	if weeks, ok := data["weeks"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("空态 weeks 期望空数组，实际 %v", data["weeks"])
	}
	if buildings, ok := data["buildings"].([]any); !ok || len(buildings) != 1 {
		t.Fatalf("空态仍应返回白名单楼，实际 %v", data["buildings"])
	}
}

// TestMetaFailure 覆盖查询失败：500 与业务码 50000，且不泄露内部细节。
func TestMetaFailure(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil, WithMeta(&fakeMetaProvider{err: errors.New("db down")}))

	rec := serveMeta(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	var got struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
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

// TestMetaNotWired 覆盖未装配 meta 服务：按内部错误处理，不 panic。
func TestMetaNotWired(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	rec := serveMeta(t, srv)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未装配时期望 500，实际 %d", rec.Code)
	}
}

// TestMetaRateLimited 覆盖公开端点的限流分支：同来源第 61 次返回 429 + 42901，
// 且被拒绝的请求不再进 handler。
func TestMetaRateLimited(t *testing.T) {
	fake := &fakeMetaProvider{result: query.MetaResult{Weeks: []query.WeekFreshness{}}}
	srv := New(quietLogger(), nil, nil, nil, nil, WithMeta(fake))
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveMeta(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应放行，实际 %d，body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("窗口内应调用 %d 次，实际 %d 次", rateLimitMaxPerWindow, fake.calls)
	}

	assertRateLimited(t, serveMeta(t, srv))
	if fake.calls != rateLimitMaxPerWindow {
		t.Fatalf("被限流的请求不应再进 handler，实际调用 %d 次", fake.calls)
	}
}

// TestMetaSharesRateLimitBucketWithContext 覆盖 meta 与 context 共用同一个限流桶：
// context 用满额度后，meta 直接 429，而不是重新获得 60 次。
func TestMetaSharesRateLimitBucketWithContext(t *testing.T) {
	fake := &fakeMetaProvider{result: query.MetaResult{Weeks: []query.WeekFreshness{}}}
	srv := New(quietLogger(), nil, fakeCtx{info: query.ContextInfo{Date: "2026-10-28"}}, nil, nil, WithMeta(fake))
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 context 请求应放行，实际 %d", i+1, rec.Code)
		}
	}

	assertRateLimited(t, serveMeta(t, srv))
	if fake.calls != 0 {
		t.Fatalf("同桶耗尽后 meta 不应进 handler，实际调用 %d 次", fake.calls)
	}
}
