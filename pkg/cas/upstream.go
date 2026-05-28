package cas

import (
	"sync"
	"time"
)

// UpstreamStatus 描述上游 CAS / 教务系统的健康状态。
// 仅用于对外暴露给前端，便于在服务抖动时给出可见的提示。
type UpstreamStatus struct {
	Healthy   bool      `json:"healthy"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

var (
	upstreamMu     sync.RWMutex
	upstreamStatus = UpstreamStatus{}
)

// MarkUpstreamHealthy 在登录链路成功完成时调用，清除告警态。
func MarkUpstreamHealthy() {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	upstreamStatus = UpstreamStatus{Healthy: true, UpdatedAt: time.Now()}
}

// MarkUpstreamUnhealthy 在重试用尽仍无法访问上游时调用，置为告警态。
func MarkUpstreamUnhealthy(message string) {
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	upstreamStatus = UpstreamStatus{Healthy: false, Message: message, UpdatedAt: time.Now()}
}

// GetUpstreamStatus 返回最近一次记录的上游服务健康状态。
func GetUpstreamStatus() UpstreamStatus {
	upstreamMu.RLock()
	defer upstreamMu.RUnlock()
	return upstreamStatus
}
