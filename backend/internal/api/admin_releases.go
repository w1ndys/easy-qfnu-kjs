package api

// 本文件是版本管理（admin-release）在 HTTP 层的装配骨架（tasklist 第 5 项）：
//
//	WithReleaseAdmin(reader, invalidate)  版本仓储 + 回退成功后的缓存失效回调
//
// 五个端点与参数门属于 tasklist 第 7 项，本项不注册路由；`Anomalies` 属于第 6 项，
// 本项也不提前实现。这里只放消费侧接口、Option 与 nil 守卫，让装配层能先把
// `store.NewReleaseAdminRepo(pool)` 与「回退后清公开查询缓存」的回调接上，
// 并由 main.go 的编译期断言钉住仓储与接口的方法集一致。
//
// 职责边界（design D1）：api 只做 HTTP/参数/形状，SQL 与回退事务全部在 store；
// api 通过消费侧接口依赖 store 的实现，store 不反向 import api。

import (
	"context"

	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// ReleaseAdminReader 是本包对版本管理仓储的消费侧接口，方法集是 store.ReleaseAdminRepo 的子集。
//
// 只列当前实现已经具备的四个方法：List（列表）、Detail（详情）、Diff（对比）、
// Rollback（回退事务）。design 的接口签名里还有 Anomalies（异常视图），它属于
// tasklist 第 6 项；在仓储实现该方法之前把它写进接口，会让 main.go 的编译期断言
// `var _ api.ReleaseAdminReader = store.ReleaseAdminRepo{}` 直接失败。第 6 项落地
// `Anomalies` 之后，应把它补进这里——接口只扩大、不缩小，补上时两端仍然结构匹配。
//
// 「未找到」由 Detail 的 bool 通道承载，不靠哨兵错误：版本不存在是 404 而不是 500，
// 写进 error 会让 handler 把它当查询失败处理（与 RunReader.ByID 同一约定）。
type ReleaseAdminReader interface {
	List(ctx context.Context) ([]store.ReleaseSummary, error)
	Detail(ctx context.Context, releaseID string) (store.ReleaseDetailView, bool, error)
	Diff(ctx context.Context, params store.ReleaseDiffParams) (store.ReleaseDiff, error)
	Rollback(ctx context.Context, releaseID string) (store.RollbackResult, error)
}

// WithReleaseAdmin 装配版本管理的数据来源与回退后的缓存失效回调。
//
//   - reader 为 nil 表示未装配：第 7 项注册端点后按 50000 返回，与 WithAdmin / WithMeta /
//     WithRunStatus 未装配同口径（Requirement 8、CP15），让 main.go 漏装配立刻暴露，
//     而不是被 404 掩盖成「版本不存在」。本项还没有这些端点，nil 语义此刻不可观测。
//   - invalidate 是回退提交成功后、写响应前要执行的失效动作，装配层通常传
//     `func(){ queries.Invalidate(); screenService.Invalidate() }`；nil 表示跳过失效，
//     回退照常成功（design D8、CP15）——测试或未开启查询缓存的进程都走这条。
//
// 回调不返回错误：缓存失效失败只影响「多久后重新查库」，不能把已经提交成功的回退
// 改写成失败响应（CP10）。
func WithReleaseAdmin(reader ReleaseAdminReader, invalidate func()) Option {
	return func(s *Server) {
		s.releaseAdmin = reader
		s.releaseInvalidate = invalidate
	}
}

// invalidateReleaseCaches 执行回退成功后的公开查询缓存失效，nil 回调时跳过。
//
// 生产调用点在第 7 项 handler：`Rollback` 成功后、写响应前调用一次（CP10）。
// 失败路径一次都不调——回退没提交就没有换版本，清了缓存也只是白让下一次查询回库。
// 把 nil 判断收在这一处，handler 不必每加一条成功分支就抄一遍 if。
// 本项不注册端点，所以该函数目前只有测试在调。
func (s *Server) invalidateReleaseCaches() {
	if s.releaseInvalidate == nil {
		return
	}
	s.releaseInvalidate()
}
