package auth

// 本文件声明管理端数据来源的消费侧接口（design「依赖方向」）。
//
// 接口定义在消费方 auth 里，实现留在 store：`store.AdminRepo` 用同名方法结构匹配，
// store 不 import auth，因此不存在反向依赖；这与 pipeline.RunStore 与
// store.CollectRunRepo 的既有做法一致。api 层直接用这个类型作 `WithAdmin` 的入参，
// 两边是否还对得上由装配层的编译期断言兜住：`var _ auth.AdminStore = store.AdminRepo{}`
// （放在 cmd/server，store 包里不放，免得实现方发起对 auth 的引用）。

import (
	"context"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// AdminStore 是管理端处理器需要的最小存储能力：唯一管理员记录的读写，
// 以及服务端会话的落库、查询与清理。方法集合与 store.AdminRepo 一一对应，
// 只列出五个端点实际用到的方法，不为将来的设置项预留。
//
// 时间一律由调用方传入（now），仓储只负责写库：过期口径是纯函数 auth.SessionActive
// 与 model.SessionTTL 的事，这里不再定义第二份。
type AdminStore interface {
	// Admin 读唯一的管理员记录；没有记录时返回 found=false 与零值，不报错。
	Admin(ctx context.Context) (user model.AdminUser, found bool, err error)
	// CreateAdmin 写入唯一管理员记录；并发下已有行时返回 created=false（不覆盖）。
	CreateAdmin(ctx context.Context, passwordHash string, now time.Time) (user model.AdminUser, created bool, err error)
	// DeleteAdmin 删除唯一管理员记录，用于初始化写密钥失败后的补偿；幂等。
	DeleteAdmin(ctx context.Context) error
	// UpdatePassword 覆盖密码哈希并前移改密时刻，不动 created_at。
	UpdatePassword(ctx context.Context, passwordHash string, now time.Time) error
	// InsertSession 落一条服务端会话；token_hash 撞主键视为存储故障。
	InsertSession(ctx context.Context, session model.AdminSession) error
	// Session 按令牌摘要读会话；查不到时返回 found=false 与零值。
	Session(ctx context.Context, tokenHash string) (session model.AdminSession, found bool, err error)
	// DeleteSession 删一条会话（退出登录）；0 行不算错误。
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteAllSessions 清空全部会话（改密），返回删除行数。
	DeleteAllSessions(ctx context.Context) (removed int64, err error)
	// DeleteExpiredSessions 只删 expires_at <= now 的行，返回删除行数。
	DeleteExpiredSessions(ctx context.Context, now time.Time) (removed int64, err error)
}
