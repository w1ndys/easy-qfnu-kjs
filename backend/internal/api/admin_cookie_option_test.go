package api

// 本文件覆盖 tasklist 第 12 项新增的装配项 WithAdminCookieSecure（配置项 ADMIN_COOKIE_SECURE）：
// 该开关只在 main.go 接线一次，默认 true；显式关闭后，下发与清除两条出口都必须不带 Secure——
// 两处属性不一致时浏览器删不掉真正的死 Cookie，本地 http 开发也会表现为「登录成功但立刻掉登录」。

import (
	"testing"
	"time"
)

// TestWithAdminCookieSecure 断言默认值来自生产口径，关闭后 adminCookie 两个方向同步生效。
func TestWithAdminCookieSecure(t *testing.T) {
	// 默认装配：与 internal/api.New 的安全默认一致，Cookie 带 Secure。
	if cookie := New(quietLogger(), nil, nil, nil, nil).adminCookie("token", int(12*time.Hour/time.Second)); !cookie.Secure {
		t.Fatal("默认装配的管理会话 Cookie 应带 Secure")
	}

	local := New(quietLogger(), nil, nil, nil, nil, WithAdminCookieSecure(false))
	if cookie := local.adminCookie("token", int(12*time.Hour/time.Second)); cookie.Secure {
		t.Fatal("ADMIN_COOKIE_SECURE=false 时下发的 Cookie 不该带 Secure")
	}
	// 清除指令必须与下发时属性一致，否则删不掉同名的死 Cookie。
	if cookie := local.adminCookie("", -1); cookie.Secure {
		t.Fatal("ADMIN_COOKIE_SECURE=false 时清除 Cookie 也不该带 Secure")
	}
}
