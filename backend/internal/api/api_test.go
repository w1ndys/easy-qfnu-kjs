package api

import (
	"bytes"
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

	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// failingPinger 模拟依赖不可用。
type failingPinger struct{}

// Ping 恒返回错误，用来触发就绪探针的失败分支。
func (failingPinger) Ping(context.Context) error { return errors.New("db down") }

// okPinger 模拟依赖可用。
type okPinger struct{}

// Ping 恒成功。
func (okPinger) Ping(context.Context) error { return nil }

// quietLogger 返回丢弃输出的日志器，避免测试输出被探针日志淹没。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// TestHealthz 覆盖存活探针：不查依赖，恒返回 200。
func TestHealthz(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("期望 body=ok，实际 %q", rec.Body.String())
	}
}

// TestReadyzUnavailable 覆盖就绪探针的失败路径：连不上库时应返回 503。
func TestReadyzUnavailable(t *testing.T) {
	srv := New(quietLogger(), failingPinger{}, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实际 %d", rec.Code)
	}
	if rec.Body.String() != "unavailable" {
		t.Fatalf("期望 body=unavailable，实际 %q", rec.Body.String())
	}
}

// TestReadyzReady 覆盖就绪探针的成功路径。
func TestReadyzReady(t *testing.T) {
	srv := New(quietLogger(), okPinger{}, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	if rec.Body.String() != "ready" {
		t.Fatalf("期望 body=ready，实际 %q", rec.Body.String())
	}
}

// TestProbeMethodRestricted 覆盖路径方法约束：POST 不应被当成探针接受。
func TestProbeMethodRestricted(t *testing.T) {
	srv := New(quietLogger(), nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，实际 %d", rec.Code)
	}
}

// fakeCtx 实现 ContextProvider，用来固定返回内容。
type fakeCtx struct {
	info query.ContextInfo // 固定返回的上下文
	err  error             // 非空时返回该错误
}

// Context 返回预设结果，忽略入参。
func (f fakeCtx) Context(context.Context, time.Time) (query.ContextInfo, error) {
	return f.info, f.err
}

// TestContextInfoOK 覆盖 /api/context 的成功响应形状。
func TestContextInfoOK(t *testing.T) {
	want := query.ContextInfo{Date: "2026-10-28", Term: "2026-2027-1", Week: 6, Weekday: 3, InCalendar: true}
	srv := New(quietLogger(), nil, fakeCtx{info: want}, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", rec.Code)
	}
	var got struct {
		Code    int               `json:"code"`
		Message string            `json:"message"`
		Data    query.ContextInfo `json:"data"`
		ReqID   string            `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	if got.Code != 0 || got.Data != want {
		t.Fatalf("响应内容不符: %+v", got)
	}
	if len(got.ReqID) != len("req_")+32 || got.ReqID[:4] != "req_" {
		t.Fatalf("request_id 不符合 req_+32 位十六进制: %q", got.ReqID)
	}
}

// TestContextInfoFailure 覆盖查询失败：500 与业务码 50000，且不泄露内部细节。
func TestContextInfoFailure(t *testing.T) {
	srv := New(quietLogger(), nil, fakeCtx{err: errors.New("db down")}, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))

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
	// 内部错误原文不能出现在公开响应里。
	if strings.Contains(rec.Body.String(), "db down") {
		t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
	}
}

// TestContextInfoCanceled 覆盖客户端断开：不写响应、不打 ERROR。
func TestContextInfoCanceled(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(logger, nil, fakeCtx{err: context.Canceled}, nil, nil)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))

	// 客户端已走，写响应没有意义：body 应为空，且不出现 500。
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

// TestContextInfoJSONKeys 按契约键名断言，防止漏写 json tag 时字段名变成驼峰。
func TestContextInfoJSONKeys(t *testing.T) {
	srv := New(quietLogger(), nil, fakeCtx{info: query.ContextInfo{Date: "2026-10-28", Week: 6, Weekday: 3}}, nil, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/context", nil))

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	data, ok := got["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %v", got["data"])
	}
	for _, key := range []string{"date", "term", "week", "weekday", "in_calendar"} {
		if _, exists := data[key]; !exists {
			t.Fatalf("缺少契约键 %q，实际键: %v", key, keysOf(data))
		}
	}
}

// keysOf 返回 map 的键列表，仅用于失败信息。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
