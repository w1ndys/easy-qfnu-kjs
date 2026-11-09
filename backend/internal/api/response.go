package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// envelope 是所有接口的统一响应体；失败时 data 省略。
type envelope struct {
	Code      int    `json:"code"`       // 业务码：0 成功，其余见错误码表
	Message   string `json:"message"`    // 人类可读描述
	Data      any    `json:"data"`       // 业务数据；失败时为 null
	RequestID string `json:"request_id"` // 请求标识，用于日志定位
}

// now 是当前时刻来源；声明成字段便于测试固定时间。
func (s *Server) currentTime() time.Time {
	return s.clock()
}

// newRequestID 生成 req_ 前缀的 16 字节随机标识。
func newRequestID() string {
	buf := make([]byte, 16)
	// 随机源失败属于系统级故障，宁可返回空也不编一个假标识。
	if _, err := rand.Read(buf); err != nil {
		return "req_"
	}
	return "req_" + hex.EncodeToString(buf)
}

// writeJSON 序列化并写回；序列化失败时降级为纯文本，避免响应体为空。
func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// ok 输出成功响应，业务码固定为 0。
func (s *Server) ok(w http.ResponseWriter, data any) {
	writeSuccess(w, newRequestID(), data)
}

// writeSuccess 按成功信封写回，成功响应的形状只在这里定义一次。
//
// 它比 s.ok 多一个 request_id 参数：有些调用点（设置保存）要先拿到 id 去写一条与本次
// 响应同 id 的日志，再写响应。让这些调用点也走这里，而不是自己拼 envelope，成功信封就
// 不会出现第二份「code=0 + message=success」的副本——GET 与 PUT 的同形状因此是构造同源，
// 不是两处各自对齐。
func writeSuccess(w http.ResponseWriter, requestID string, data any) {
	writeJSON(w, http.StatusOK, envelope{Code: 0, Message: "success", Data: data, RequestID: requestID})
}

// fail 输出失败响应，HTTP 状态与业务码分开表达。
func (s *Server) fail(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, envelope{Code: code, Message: message, RequestID: newRequestID()})
}
