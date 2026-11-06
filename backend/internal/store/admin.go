package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// adminQuerier 是管理仓储需要的最小能力：Exec 执行不带回值的写，
// QueryRow 既用于读行，也用于接 CreateAdmin 的 RETURNING。
// 用窄接口而不是 *pgxpool.Pool，与 publishQuerier 等既有仓储同一写法：
// 单元测试可用替身验证错误口径，装配时交同一个连接池即可。
// 它不包含 Begin：管理端的写都是一条语句，没有需要跨语句原子性的场景
// （改密与清会话的顺序由 api 层决定，见 Requirement 8）。
type adminQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// adminUserSQL 读唯一的管理员行。id 恒为 1（0001 的 CHECK (id = 1)），
// 不写 LIMIT 1：多行在约束下不可能出现，若真出现说明约束被绕过，
// 静默取一行会掩盖问题。
const adminUserSQL = `
SELECT id, password_hash, password_updated_at, created_at
  FROM admin_user
 WHERE id = 1`

// createAdminSQL 创建管理员，冲突时不写并让 RETURNING 无行可回。
//
// ON CONFLICT (id) DO NOTHING 是并发初始化唯一的串行化点：两个请求同时到达时，
// 后到者在冲突行上等待先到者提交，随后什么也不写。因此 created 必须由
// 「RETURNING 是否有行」判定，不能先 Admin() 再 INSERT——先查后插在并发下会双写
// 或把两个请求都判成成功（CP3、Requirement 1.4）。
//
// created_at 与 password_updated_at 都写调用方给的 now：初始化设的密码就是
// 首次设置密码的时刻，两列同值，不依赖列默认 now()（默认值只兜底手工插入）。
const createAdminSQL = `
INSERT INTO admin_user (id, password_hash, password_updated_at, created_at)
VALUES (1, $1, $2, $2)
ON CONFLICT (id) DO NOTHING
RETURNING id, password_hash, password_updated_at, created_at`

// deleteAdminSQL 删唯一的管理员行，用于初始化写密钥文件失败后的补偿
// （design D10、Error Handling「初始化失败补偿」）。0 行不算错误：
// 补偿路径上记录可能已被并发清掉，重复删除应当幂等。
const deleteAdminSQL = `DELETE FROM admin_user WHERE id = 1`

// updatePasswordSQL 覆盖密码哈希并前移改密时刻。
// 不动 created_at：账号建立时刻是历史事实，改密不改变它。
const updatePasswordSQL = `
UPDATE admin_user
   SET password_hash = $1,
       password_updated_at = $2
 WHERE id = 1`

// insertSessionSQL 落一条服务端会话。
//
// 不写 ON CONFLICT DO NOTHING：token_hash 来自 32 字节随机值，撞主键意味着
// 熵源或调用方出了问题；此时若静默忽略，新登录会复用旧会话的 CSRF 令牌，
// 调用方还以为自己刚建了一条新会话。让主键冲突变成 ErrAdminStoreFailed 更诚实。
// expires_at > created_at 由 0004 的 CHECK 兜底，这里不重复校验。
const insertSessionSQL = `
INSERT INTO admin_session (token_hash, csrf_token, created_at, expires_at)
VALUES ($1, $2, $3, $4)`

// sessionSQL 按令牌摘要读会话。只查主键，认证路径上每次管理请求一条。
const sessionSQL = `
SELECT token_hash, csrf_token, created_at, expires_at
  FROM admin_session
 WHERE token_hash = $1`

// deleteSessionSQL 删一条会话（退出登录）。0 行不算错误：重复退出、
// Cookie 在库里已被清理，都应返回成功而不是 500。
const deleteSessionSQL = `DELETE FROM admin_session WHERE token_hash = $1`

// deleteAllSessionsSQL 清空全部会话（改密）。没有 WHERE：改密后旧登录一律失效（CP8）。
const deleteAllSessionsSQL = `DELETE FROM admin_session`

// deleteExpiredSessionsSQL 只删已过期的行。
//
// 有效判定是 now < expires_at（CP6），所以过期一侧含等号：expires_at = now 的会话
// 在认证时已经无效，若这里漏掉等号，它会被永远留下——认证永远失败，清理永远删不掉，
// 每次登录都白扫一遍。expires_at 上有 admin_session_expires 索引，这里能走索引。
const deleteExpiredSessionsSQL = `DELETE FROM admin_session WHERE expires_at <= $1`

// AdminRepo 把管理端两张表（admin_user、admin_session）的 SQL 闭在内部。
//
// 它是 auth.AdminStore 的结构匹配实现：store 不 import auth（依赖方向见
// backend/.golangci.yml），auth 在自己的包里声明消费侧接口，方法名与签名
// 与本类型一一对应。对应关系最终由 api 装配处的编译期赋值确认，这里不复制接口，
// 以免维护两份契约。
type AdminRepo struct {
	q adminQuerier
}

// NewAdminRepo 绑定一个查询器，通常是 *pgxpool.Pool。
func NewAdminRepo(q adminQuerier) AdminRepo {
	return AdminRepo{q: q}
}

// Admin 读唯一的管理员记录；没有记录时返回 found=false 与零值，不报错。
//
// 「没有管理员」是首次部署的正常空态（Requirement 1.1、8.5），由调用方决定
// 进初始化还是登录；只有真正的读库失败才包 ErrAdminStoreFailed（映射 50000）。
func (r AdminRepo) Admin(ctx context.Context) (model.AdminUser, bool, error) {
	var user model.AdminUser
	err := r.q.QueryRow(ctx, adminUserSQL).Scan(
		&user.ID, &user.PasswordHash, &user.PasswordUpdatedAt, &user.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AdminUser{}, false, nil
	}
	if err != nil {
		return model.AdminUser{}, false, fmt.Errorf("%w: 读取管理员失败: %w", model.ErrAdminStoreFailed, err)
	}
	return user, true, nil
}

// CreateAdmin 创建 id=1 的管理员，created 表示本次是否真的写入了新行。
//
// 已有记录时 PostgreSQL 的 ON CONFLICT DO NOTHING 让 RETURNING 返回 0 行，
// 这里折成 created=false、无错误的正常结果：调用方据此映射 40902，
// 并把「记录已存在」与「库坏了」分开（CP3）。返回的 AdminUser 是库里写下的行，
// 不是用入参拼出来的副本，时间列也来自 RETURNING，避免应用与库的时钟口径漂移。
func (r AdminRepo) CreateAdmin(ctx context.Context, passwordHash string, now time.Time) (model.AdminUser, bool, error) {
	var user model.AdminUser
	err := r.q.QueryRow(ctx, createAdminSQL, passwordHash, now).Scan(
		&user.ID, &user.PasswordHash, &user.PasswordUpdatedAt, &user.CreatedAt)
	// 冲突时 RETURNING 无行可扫：管理员已存在，不是错误。
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AdminUser{}, false, nil
	}
	if err != nil {
		return model.AdminUser{}, false, fmt.Errorf("%w: 创建管理员失败: %w", model.ErrAdminStoreFailed, err)
	}
	return user, true, nil
}

// DeleteAdmin 删除唯一的管理员记录，用于初始化失败时的补偿回滚。
//
// 故意不把「0 行」判成错误：补偿要尽力而为，重复删除或被并发清掉时
// 结果一样（库里没有管理员），不该让补偿路径再抛一个错掩盖原始失败。
func (r AdminRepo) DeleteAdmin(ctx context.Context) error {
	if _, err := r.q.Exec(ctx, deleteAdminSQL); err != nil {
		return fmt.Errorf("%w: 删除管理员失败: %w", model.ErrAdminStoreFailed, err)
	}
	return nil
}

// UpdatePassword 覆盖管理员密码哈希并前移 password_updated_at。
//
// 0 行说明库里根本没有管理员记录（初始化被绕过或记录被删），改密不能假装成功：
// 调用方刚校验过当前密码，此时返回错误让请求以 50000 结束，比留下一个「密码没变
// 但返回成功」的响应安全。
func (r AdminRepo) UpdatePassword(ctx context.Context, passwordHash string, now time.Time) error {
	tag, err := r.q.Exec(ctx, updatePasswordSQL, passwordHash, now)
	if err != nil {
		return fmt.Errorf("%w: 更新管理员密码失败: %w", model.ErrAdminStoreFailed, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: 更新管理员密码命中 %d 行，管理员记录不存在",
			model.ErrAdminStoreFailed, tag.RowsAffected())
	}
	return nil
}

// InsertSession 写入一条服务端会话，time 列原样取入参。
//
// 主键冲突按 ErrAdminStoreFailed 返回（原因见 insertSessionSQL 的说明）；
// created_at 与 expires_at 的先后由 0004 的 CHECK 兜底，违反时同样是一类写库失败。
func (r AdminRepo) InsertSession(ctx context.Context, s model.AdminSession) error {
	if _, err := r.q.Exec(ctx, insertSessionSQL,
		s.TokenHash, s.CSRFToken, s.CreatedAt, s.ExpiresAt); err != nil {
		return fmt.Errorf("%w: 写入管理会话失败: %w", model.ErrAdminStoreFailed, err)
	}
	return nil
}

// Session 按令牌摘要读一条会话；没有记录时返回 found=false 与零值。
//
// 这里只负责「取」，不判过期：过期判定是 auth.SessionActive 的纯函数
// （now < expires_at，CP6），把时间比较留在仓储外才能用假时钟单测边界。
func (r AdminRepo) Session(ctx context.Context, tokenHash string) (model.AdminSession, bool, error) {
	var s model.AdminSession
	err := r.q.QueryRow(ctx, sessionSQL, tokenHash).Scan(
		&s.TokenHash, &s.CSRFToken, &s.CreatedAt, &s.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.AdminSession{}, false, nil
	}
	if err != nil {
		return model.AdminSession{}, false, fmt.Errorf("%w: 读取管理会话失败: %w", model.ErrAdminStoreFailed, err)
	}
	return s, true, nil
}

// DeleteSession 按令牌摘要删除一条会话（退出登录）。
// 0 行是正常结果：重复退出或记录已被清理时，结果都是「这个令牌不再有效」。
func (r AdminRepo) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := r.q.Exec(ctx, deleteSessionSQL, tokenHash); err != nil {
		return fmt.Errorf("%w: 删除管理会话失败: %w", model.ErrAdminStoreFailed, err)
	}
	return nil
}

// DeleteAllSessions 删除全部会话，返回受影响行数供日志与测试断言。
// 改密成功后调用，保证改密前签发的任何 Cookie 都立刻失效（CP8、Requirement 8.3）。
func (r AdminRepo) DeleteAllSessions(ctx context.Context) (int64, error) {
	tag, err := r.q.Exec(ctx, deleteAllSessionsSQL)
	if err != nil {
		return 0, fmt.Errorf("%w: 清理全部管理会话失败: %w", model.ErrAdminStoreFailed, err)
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredSessions 删除过期时刻不晚于 now 的会话，返回受影响行数。
//
// 边界与认证判定严格同源：now < expires_at 才有效，所以 expires_at = now 算过期。
// 登录成功时顺手调用它做清理（Requirement 5、design「登录成功顺手
// DeleteExpiredSessions」），过期行不依赖外部定时任务。
func (r AdminRepo) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	tag, err := r.q.Exec(ctx, deleteExpiredSessionsSQL, now)
	if err != nil {
		return 0, fmt.Errorf("%w: 清理过期管理会话失败: %w", model.ErrAdminStoreFailed, err)
	}
	return tag.RowsAffected(), nil
}
