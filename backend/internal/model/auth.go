package model

import (
	"errors"
	"time"
)

// SessionTTL 是管理会话的绝对有效期：登录时写入 now+SessionTTL，之后不再滑动续期，
// 过期时刻可预测，判定只剩「当前时刻早于 expires_at」一次比较（Requirement 5、CP6）。
const SessionTTL = 12 * time.Hour

// AdminUser 是唯一的管理员账号，对应 admin_user 表的一行（至多一行，CP1）。
// 密码只以 PHC 串形态出现在这里，明文密码既不落库也不进日志（CP2）。
type AdminUser struct {
	ID                int16     // 固定为 1：主键 CHECK (id = 1) 只允许这一行
	PasswordHash      string    // argon2id PHC 串，校验时按串内参数重算
	PasswordUpdatedAt time.Time // 最近一次改密时刻，改密成功后前移
	CreatedAt         time.Time // 账号建立时刻
}

// AdminSession 是一条服务端会话，对应 admin_session 表的一行。
// 认证只凭 TokenHash：明文令牌只存在于浏览器 Cookie，库里没有可直接使用的凭据（CP5）。
type AdminSession struct {
	TokenHash string    // 会话令牌的 SHA-256（hex），主键
	CSRFToken string    // 与会话绑定的同步器令牌，写操作须经 X-CSRF-Token 回传
	CreatedAt time.Time // 会话建立时刻
	ExpiresAt time.Time // 绝对过期时刻，恒为 CreatedAt + SessionTTL
}

var (
	// ErrAdminExists 表示 admin_user 已有记录：初始化撞上既有账号，对外映射 40902。
	ErrAdminExists = errors.New("管理员已存在")
	// ErrKeyExists 表示密钥文件已存在：初始化按 40903 拒绝，且不覆盖既有密钥（CP4）。
	ErrKeyExists = errors.New("加密密钥已存在")
	// ErrKeyUnavailable 表示密钥未装载，或文件不可读、内容非法。
	// 它只影响依赖密钥的能力，进程仍应启动（CP14）。
	ErrKeyUnavailable = errors.New("加密密钥不可用")
	// ErrAdminStoreFailed 表示管理数据的仓储读写失败，对外映射 50000。
	ErrAdminStoreFailed = errors.New("管理数据读写失败")
)
