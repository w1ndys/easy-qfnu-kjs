package model

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestSessionTTLIsTwelveHours 钉住会话有效期：登录写入的 expires_at 恒为
// 创建时刻 + 12 小时，改动这个常量会直接改变已签发会话的寿命。
func TestSessionTTLIsTwelveHours(t *testing.T) {
	if SessionTTL != 12*time.Hour {
		t.Fatalf("SessionTTL 应为 12h，实际 %s", SessionTTL)
	}
}

// TestAdminSentinelsAreDistinct 保证四个哨兵各有独立文案：
// 文案重复时调用方无法只凭错误判定分支，日志里也分不清是冲突还是读写失败。
func TestAdminSentinelsAreDistinct(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrAdminExists", ErrAdminExists},
		{"ErrKeyExists", ErrKeyExists},
		{"ErrKeyUnavailable", ErrKeyUnavailable},
		{"ErrAdminStoreFailed", ErrAdminStoreFailed},
	}

	seen := make(map[string]string, len(sentinels))
	for _, s := range sentinels {
		if s.err == nil {
			t.Fatalf("%s 不应为 nil", s.name)
		}
		if prev, ok := seen[s.err.Error()]; ok {
			t.Fatalf("%s 与 %s 的文案相同（%q），调用方无法区分", s.name, prev, s.err.Error())
		}
		seen[s.err.Error()] = s.name
	}
}

// TestAdminSentinelsSurviveWrapping 覆盖上层的实际用法：仓储与密钥代码会用
// fmt.Errorf("...: %w") 补上下文，判定仍必须靠 errors.Is 命中原始哨兵，
// 这样 40902/40903 与 50000 才走不同分支。
func TestAdminSentinelsSurviveWrapping(t *testing.T) {
	sentinels := []error{
		ErrAdminExists,
		ErrKeyExists,
		ErrKeyUnavailable,
		ErrAdminStoreFailed,
	}

	for _, sentinel := range sentinels {
		wrapped := fmt.Errorf("读取管理会话: %w", sentinel)
		if !errors.Is(wrapped, sentinel) {
			t.Fatalf("%q 包装后应仍能用 errors.Is 命中", sentinel)
		}
	}

	// 反向也要成立：不同哨兵之间不能互相误判，否则「管理员已存在」
	// 会被当成「密钥已存在」返回 40903。
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("%q 不应命中 %q", a, b)
			}
		}
	}
}
