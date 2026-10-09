// 本文件是 entry 层：采集器的上游会话。
// 真实 CAS 登录与 ddddocr 验证码识别不在本次实现里，这里留一个明确的未实现入口；
// 实现时按 docs/upstream.md 第 1 节做（CAS 登录页取 salt/execution、AES/CBC 加密密码、
// 验证码图片走 {ocr_base_url}/ocr）。测试注入假会话，不访问网络。

package main

import (
	"context"
	"errors"
	"fmt"
)

// ErrLoginNotImplemented 表示真实 CAS 登录还没实现：拿不到会话时不要静默继续。
var ErrLoginNotImplemented = errors.New("CAS 登录未实现（见 docs/upstream.md 第 1 节）")

// casSession 是真实上游会话的占位实现：登录未实现，Cookie 始终为空。
type casSession struct {
	baseURL string // 教务系统根地址，只为错误信息留下出处
}

// newCASSession 组装真实会话的占位实现。
func newCASSession(baseURL string) *casSession {
	return &casSession{baseURL: baseURL}
}

// Login 明确报告未实现：空手请求只会一直拿到登录页，不如在这里停下。
func (s *casSession) Login(ctx context.Context) error {
	return fmt.Errorf("%w: %s", ErrLoginNotImplemented, s.baseURL)
}

// Cookie 在登录实现之前没有可用会话，返回空串。
func (s *casSession) Cookie() string { return "" }
