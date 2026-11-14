package api

// 本文件是版本管理装配骨架的单测（tasklist 第 5 项）。
//
// 五个 HTTP 端点与它们的契约测试属于第 7 项，回退成功/失败路径上的调用次数也在那里
// 断言；本项先锁住 WithReleaseAdmin 的装配语义：reader 传 nil 时字段保持 nil（端点
// 将来按 50000 返回），invalidate 传 nil 时失效动作被跳过且不 panic，传了回调时
// 每次调用恰好执行一次。

import "testing"

// TestWithReleaseAdminSkipsNilInvalidate 覆盖 CP15 的「invalidate 为 nil」分支：
// reader 与回调都不传时装配不失败，失效辅助方法直接跳过且不 panic（回退本身照常成功，
// 该路径由第 7 项的端点契约测试断言）。
func TestWithReleaseAdminSkipsNilInvalidate(t *testing.T) {
	s := New(nil, nil, nil, nil, nil, WithReleaseAdmin(nil, nil))

	if s.releaseAdmin != nil {
		t.Fatalf("reader 传 nil 时 releaseAdmin 应保持 nil，实际 %T", s.releaseAdmin)
	}
	if s.releaseInvalidate != nil {
		t.Fatal("invalidate 传 nil 时回调应保持 nil，实际非 nil")
	}

	// 不 panic 就是通过：nil 回调表示「未装配查询缓存」，handler 不必先判空。
	s.invalidateReleaseCaches()
}

// TestWithReleaseAdminCallsInvalidateOnce 覆盖「非 nil 回调恰好调用一次」：
// handler 在回退提交成功后只走一次失效辅助方法，清一遍三张缓存即可。
func TestWithReleaseAdminCallsInvalidateOnce(t *testing.T) {
	calls := 0
	s := New(nil, nil, nil, nil, nil, WithReleaseAdmin(nil, func() { calls++ }))

	s.invalidateReleaseCaches()

	if calls != 1 {
		t.Fatalf("非 nil 回调应恰好调用一次，实际 %d 次", calls)
	}
}
