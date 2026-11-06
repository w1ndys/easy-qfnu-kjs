package auth

// 本文件提供管理会话的凭证生成与判定：会话令牌、CSRF 令牌、绝对过期时刻与有效性判定。
// 全是纯函数，不碰 HTTP、SQL 与配置；时间一律由调用方传入，方便用假时钟测边界。

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// SessionTokenBytes 是会话令牌与 CSRF 令牌的随机原文长度：32 字节即 256 位熵，
	// 编码成无填充 base64url 后固定 43 个字符。暴力猜中的概率远低于硬件寿命内可完成的范围。
	SessionTokenBytes = 32
	// SessionTTL 只是 model.SessionTTL 的别名，12 小时唯一定义在 internal/model：
	// auth 不另写字面量，避免两处数值各自漂移（D6、CP6）。
	SessionTTL = model.SessionTTL
)

// newRandomToken 取 SessionTokenBytes 字节密码学随机数，编码成无填充 base64url。
// 编码而不是把随机字节直接当 Cookie 值：Cookie 值里不能出现分号、逗号、空白、引号等字符，
// base64url 只含 A-Za-z0-9-_，可以原样进 Cookie 与 JSON 响应。
func newRandomToken() (string, error) {
	buf := make([]byte, SessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewSessionToken 生成一条新的会话令牌，返回 Cookie 里那份原文与它的 SHA-256 摘要。
// 原文只下发到浏览器 Cookie，库里只存摘要，因此拿到库也还原不出可用的 Cookie（D5、CP5）。
// 摘要对「编码后的原文」计算而非对 32 字节随机数计算：查询时手里只有 Cookie 字符串，
// 两处必须同一口径才能按 Cookie 命中记录。
func NewSessionToken() (raw, tokenHash string, err error) {
	raw, err = newRandomToken()
	if err != nil {
		return "", "", fmt.Errorf("生成会话令牌: %w", err)
	}
	return raw, HashSessionToken(raw), nil
}

// HashSessionToken 返回会话令牌原文的 SHA-256，小写十六进制（64 字符）。
// 写入与查询会话都调用它，保证「登录时存进库的摘要」与「按 Cookie 查库的摘要」一致。
// 空原文不该被查询：调用方必须先确认 Cookie 存在，否则会拿一个可预测的摘要去查库。
func HashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewCSRFToken 生成与会话绑定的 CSRF 同步器令牌：32 字节随机数的无填充 base64url。
// 它与会话令牌同编码但互不派生，客户端经 X-CSRF-Token 头回传，跨站页面读不到（D7、Requirement 6）。
func NewCSRFToken() (string, error) {
	token, err := newRandomToken()
	if err != nil {
		return "", fmt.Errorf("生成 CSRF 令牌: %w", err)
	}
	return token, nil
}

// CompareCSRFToken 以常量时间比对会话记录里的 CSRF 令牌与请求头 X-CSRF-Token 的值。
// 长度不同会立刻返回 false（长度本身不是秘密）；等长时逐字节耗时与内容无关，
// 避免攻击者按响应时间逐字符试出令牌（Requirement 6.1、CP7）。
// 任一侧为空都返回 false：空串之间按字节相等，但「会话里没有令牌」不能等于「校验通过」。
func CompareCSRFToken(expected, got string) bool {
	if expected == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(got)) == 1
}

// SessionExpiry 返回会话的绝对过期时刻：恒为 now + model.SessionTTL（12 小时）。
// 用 Add 加时长而非改时钟字段，跨时区与夏令时都恰好 12 小时；不做滑动续期（D6、CP6）。
func SessionExpiry(now time.Time) time.Time {
	return now.Add(SessionTTL)
}

// SessionActive 判定会话在 now 是否仍然有效：当且仅当 now 严格早于 expires_at。
// 恰好等于 expires_at 即视为过期，所以边界是「过期前一秒仍有效、后一秒与整点都无效」（Requirement 5.2、CP6）。
// 零值 ExpiresAt 因此恒为无效，不需要额外分支；它不看 CreatedAt，过期判定只有这一次比较。
func SessionActive(s model.AdminSession, now time.Time) bool {
	return now.Before(s.ExpiresAt)
}
