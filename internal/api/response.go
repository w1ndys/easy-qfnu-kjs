// 本文件属于 entry 层：统一响应四段式与请求追踪号。
// 形状见 docs/contract/api.v2.md：{code, message, data, request_id}。

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// 响应码取值，与契约一致。
const (
	// CodeOK 表示成功
	CodeOK = 0
	// CodeBadRequest 表示参数错误，前端据此提示用户改输入
	CodeBadRequest = 40001
	// CodeNoData 表示没有可用数据：没有任何 release，或该日期未收录
	CodeNoData = 40401
	// CodeInternal 表示内部错误，细节只进日志不回给客户端
	CodeInternal = 50000
)

// envelope 是统一响应体，四段式缺一不可。
type envelope struct {
	Code      int    `json:"code"`       // 业务响应码，0 为成功
	Message   string `json:"message"`    // 人类可读说明
	Data      any    `json:"data"`       // 业务载荷，失败时为 null
	RequestID string `json:"request_id"` // 服务端生成的追踪号
}

// newRequestID 生成一次请求的追踪号，写日志与回显用它对齐。
func newRequestID() string {
	buffer := make([]byte, 8)
	// 随机源失败时退化成时间戳，宁可追踪号不完美也不要中断请求
	if _, err := rand.Read(buffer); err != nil {
		return "req_" + time.Now().Format("20060102150405")
	}
	return "req_" + hex.EncodeToString(buffer)
}

// writeEnvelope 写出统一响应；契约只定义响应体里的 code，因此一律用 HTTP 200。
func (s *Server) writeEnvelope(w http.ResponseWriter, body envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// 序列化失败时响应已经写出，只能记日志，无法再改状态码
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.Error("写出响应失败", "request_id", body.RequestID, "error", err)
	}
}

// writeData 写出成功响应。
func (s *Server) writeData(w http.ResponseWriter, requestID string, data any) {
	s.writeEnvelope(w, envelope{Code: CodeOK, Message: "success", Data: data, RequestID: requestID})
}

// writeCode 写出失败响应，data 固定为 null。
func (s *Server) writeCode(w http.ResponseWriter, requestID string, code int, message string) {
	s.writeEnvelope(w, envelope{Code: code, Message: message, Data: nil, RequestID: requestID})
}

// logDone 记录一次请求的结果与耗时。
func (s *Server) logDone(requestID, path string, code int, started time.Time) {
	s.logger.Info("请求完成",
		"request_id", requestID,
		"path", path,
		"code", code,
		"elapsed_ms", time.Since(started).Milliseconds(),
	)
}

// logServed 是各 handler 结束时统一打的日志，code 为业务码。
func (s *Server) logServed(requestID, path string, code int, started time.Time, level slog.Level) {
	// 只有内部错误才用 error 级别，其余按 info 记，避免日志噪音
	if level == slog.LevelError {
		s.logger.Error("请求失败",
			"request_id", requestID,
			"path", path,
			"code", code,
			"elapsed_ms", time.Since(started).Milliseconds(),
		)
		return
	}
	s.logDone(requestID, path, code, started)
}
