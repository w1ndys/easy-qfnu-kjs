package auth

// 本文件覆盖会话令牌、CSRF 令牌、绝对过期与有效性判定。
// 全部是纯函数用例，不替换包级状态，也不写 t.Parallel——与同包其他用例的既有约定一致。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// tokenRawPattern 是令牌原文形态：32 字节密码学随机数的无填充 base64url，恒 43 字符。
var tokenRawPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// decodeTokenRaw 把令牌原文解码回随机字节；解码失败直接终止用例。
func decodeTokenRaw(t *testing.T, token string) []byte {
	t.Helper()
	buf, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("令牌原文应是合法的无填充 base64url：%q，err=%v", token, err)
	}
	return buf
}

// TestNewSessionToken 断言会话令牌的形态与两段结果的关系：原文是 43 字符无填充 base64url，
// 解码后回到 32 字节；摘要是对原文的 SHA-256 小写十六进制（64 字符）；
// 1,000 次调用内原文与摘要都不重复，且摘要永远不等于原文（Requirement 5、CP5）。
func TestNewSessionToken(t *testing.T) {
	const rounds = 1000
	seenRaw := make(map[string]struct{}, rounds)
	seenHash := make(map[string]struct{}, rounds)

	for i := range rounds {
		raw, tokenHash, err := NewSessionToken()
		if err != nil {
			t.Fatalf("第 %d 次 NewSessionToken 应成功，实际 err=%v", i, err)
		}
		if !tokenRawPattern.MatchString(raw) {
			t.Fatalf("第 %d 次令牌原文形态不符：%q", i, raw)
		}
		if buf := decodeTokenRaw(t, raw); len(buf) != SessionTokenBytes {
			t.Fatalf("第 %d 次令牌原文解码后期望 %d 字节，实际 %d", i, SessionTokenBytes, len(buf))
		}
		if strings.ContainsAny(raw, "=+/") {
			t.Fatalf("第 %d 次令牌原文不应含填充或标准 base64 字符：%q", i, raw)
		}

		if len(tokenHash) != 64 {
			t.Fatalf("第 %d 次摘要应是 64 位十六进制，实际 %q", i, tokenHash)
		}
		digest, err := hex.DecodeString(tokenHash)
		if err != nil {
			t.Fatalf("第 %d 次摘要不是合法十六进制：%q，err=%v", i, tokenHash, err)
		}
		if len(digest) != sha256.Size {
			t.Fatalf("第 %d 次摘要期望 %d 字节，实际 %d", i, sha256.Size, len(digest))
		}
		if tokenHash != strings.ToLower(tokenHash) {
			t.Fatalf("第 %d 次摘要应是小写十六进制：%q", i, tokenHash)
		}
		if tokenHash != HashSessionToken(raw) {
			t.Fatalf("第 %d 次 NewSessionToken 的摘要必须等于 HashSessionToken(原文)", i)
		}
		if tokenHash == raw {
			t.Fatalf("第 %d 次摘要不应与原文相同", i)
		}

		if _, ok := seenRaw[raw]; ok {
			t.Fatalf("第 %d 次生成的令牌原文与之前重复", i)
		}
		if _, ok := seenHash[tokenHash]; ok {
			t.Fatalf("第 %d 次生成的令牌摘要与之前重复", i)
		}
		seenRaw[raw] = struct{}{}
		seenHash[tokenHash] = struct{}{}
	}
}

// TestHashSessionToken 钉住摘要口径：对原文整体做 SHA-256 后写小写十六进制。
// 固定向量把输入锁在「Cookie 里那串编码后的原文」上，而不是解码出来的 32 字节，
// 这样查询时用 Cookie 值算出的摘要才能和入库摘要一致（CP5）。
func TestHashSessionToken(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"空原文", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"ASCII", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"base64url 形态原文", "Zm9vYmFyLWJhel9xdXV4", "1ee7b18bf6f7597811cb07fc9f5d067ad57a5f0c7a9415a715dc1e2d11f41b41"},
		{"非 ASCII", "会话令牌", "99b93dec6b61f6fcc431d7a235dda1fd888e36f39cece8dd5063b1663d32f977"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HashSessionToken(tt.raw); got != tt.want {
				t.Fatalf("HashSessionToken(%q) 期望 %q，实际 %q", tt.raw, tt.want, got)
			}
			if again := HashSessionToken(tt.raw); again != tt.want {
				t.Fatalf("同一原文必须得到同一摘要：期望 %q，实际 %q", tt.want, again)
			}
		})
	}

	if HashSessionToken("a") == HashSessionToken("b") {
		t.Fatalf("不同原文的摘要不应相同")
	}
	long := HashSessionToken(strings.Repeat("x", 4096))
	if len(long) != 64 || long != strings.ToLower(long) {
		t.Fatalf("超长原文的摘要仍应是 64 位小写十六进制，实际 %q", long)
	}
}

// TestNewCSRFToken 断言 CSRF 令牌与需求一致：32 字节随机数的无填充 base64url，
// 1,000 次内不重复，且同一条令牌与自身常量时间比对通过（Requirement 6、D7）。
func TestNewCSRFToken(t *testing.T) {
	const rounds = 1000
	seen := make(map[string]struct{}, rounds)

	for i := range rounds {
		token, err := NewCSRFToken()
		if err != nil {
			t.Fatalf("第 %d 次 NewCSRFToken 应成功，实际 err=%v", i, err)
		}
		if !tokenRawPattern.MatchString(token) {
			t.Fatalf("第 %d 次 CSRF 令牌形态不符：%q", i, token)
		}
		if buf := decodeTokenRaw(t, token); len(buf) != SessionTokenBytes {
			t.Fatalf("第 %d 次 CSRF 令牌解码后期望 %d 字节，实际 %d", i, SessionTokenBytes, len(buf))
		}
		if strings.Contains(token, "=") {
			t.Fatalf("第 %d 次 CSRF 令牌不应带 base64 填充：%q", i, token)
		}
		if !CompareCSRFToken(token, token) {
			t.Fatalf("第 %d 次 CSRF 令牌与自身比对应通过", i)
		}
		if _, ok := seen[token]; ok {
			t.Fatalf("第 %d 次生成的 CSRF 令牌与之前重复", i)
		}
		seen[token] = struct{}{}
	}
}

// TestCompareCSRFToken 覆盖同步器令牌比对的全部结论：等值通过；任一字符不同、
// 长度不同、单侧或双侧为空都失败。空值两侧必须被拒——空串按字节相等，
// 但「会话里没有令牌」不能等于「校验通过」（Requirement 6.1、CP7）。
func TestCompareCSRFToken(t *testing.T) {
	const valid = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq" // 43 字符，与真实令牌同长
	if len(valid) != 43 {
		t.Fatalf("用例令牌长度应为 43，实际 %d", len(valid))
	}

	tests := []struct {
		name     string
		expected string
		got      string
		want     bool
	}{
		{"完全相同", valid, valid, true},
		{"末字符不同", valid, valid[:42] + "R", false},
		{"首字符不同", valid, "B" + valid[1:], false},
		{"少一个字符", valid, valid[:42], false},
		{"多一个字符", valid, valid + "A", false},
		{"等长但全异", valid, strings.Repeat("Z", 43), false},
		{"前缀相同而后缀不同", valid, valid[:30] + "0123456789abc", false},
		{"两侧都为空", "", "", false},
		{"会话侧为空", "", valid, false},
		{"请求头侧为空", valid, "", false},
		{"单字符相等", "a", "a", true},
		{"单字符不同", "a", "b", false},
		{"非 ASCII 相等", "令牌-值", "令牌-值", true},
		{"非 ASCII 末字符不同", "令牌-值", "令牌-止", false},
		{"尾部空格视为不同", valid, valid + " ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompareCSRFToken(tt.expected, tt.got); got != tt.want {
				t.Fatalf("CompareCSRFToken(%q, %q) 期望 %v，实际 %v",
					tt.expected, tt.got, tt.want, got)
			}
		})
	}
}

// TestSessionTTLMatchesModel 断言 auth 只做别名：12 小时唯一定义在 model，
// auth.SessionTTL 必须与 model.SessionTTL 逐纳秒相等（D6、CP6）。
func TestSessionTTLMatchesModel(t *testing.T) {
	if SessionTTL != model.SessionTTL {
		t.Fatalf("auth.SessionTTL 应等于 model.SessionTTL，实际 %s 与 %s", SessionTTL, model.SessionTTL)
	}
	if SessionTTL != 12*time.Hour {
		t.Fatalf("会话有效期应为 12h，实际 %s", SessionTTL)
	}
}

// TestSessionExpiry 断言过期时刻恒为 now+12h：不取整、不截断、不改时区表示，
// 即便跨夏令时也恰好加 12 小时时长（D6、CP6）。
func TestSessionExpiry(t *testing.T) {
	cst := time.FixedZone("CST", 8*3600)
	tests := []struct {
		name string
		now  time.Time
	}{
		{"UTC 带纳秒", time.Date(2026, 11, 6, 11, 45, 37, 123456789, time.UTC)},
		{"东八区固定偏移", time.Date(2026, 11, 6, 19, 45, 37, 0, cst)},
		{"零值时刻", time.Time{}},
		{"纳秒上界", time.Date(2026, 3, 8, 6, 59, 59, 999999999, time.UTC)},
		{"闰日", time.Date(2028, 2, 29, 23, 59, 59, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SessionExpiry(tt.now)
			want := tt.now.Add(model.SessionTTL)
			if !got.Equal(want) {
				t.Fatalf("SessionExpiry(%s) 期望 %s，实际 %s", tt.now, want, got)
			}
			if got.Sub(tt.now) != model.SessionTTL {
				t.Fatalf("过期时刻与当前时刻之差应恒为 %s，实际 %s", model.SessionTTL, got.Sub(tt.now))
			}
			if got.Location().String() != tt.now.Location().String() {
				t.Fatalf("SessionExpiry 不应改变时区表示：期望 %s，实际 %s",
					tt.now.Location(), got.Location())
			}
			if !got.Equal(SessionExpiry(tt.now)) {
				t.Fatalf("同一 now 两次调用应得到同一过期时刻")
			}
		})
	}

	// 跨夏令时：2026-03-08 美东从 EST 跳到 EDT，00:30 EST 之后 12 小时是 13:30 EDT。
	// 用 Add 加时长才能恰好 12 小时；这里同时断言偏移确实变了，避免用例在无 DST 的时区里空转。
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("时区库不可用，跳过夏令时用例：%v", err)
	}
	before := time.Date(2026, 3, 8, 0, 30, 0, 0, loc)
	after := SessionExpiry(before)
	if after.Sub(before) != model.SessionTTL {
		t.Fatalf("跨夏令时后仍应恰好加 %s，实际 %s", model.SessionTTL, after.Sub(before))
	}
	_, beforeOffset := before.Zone()
	_, afterOffset := after.Zone()
	if beforeOffset == afterOffset {
		t.Fatalf("用例应跨过夏令时切换，偏移却未变化：%d", beforeOffset)
	}
}

// TestSessionActive 覆盖「now 严格早于 expires_at」的边界：过期前一秒与前一纳秒仍有效，
// 恰好等于过期时刻、后一纳秒与后一秒都无效；判定只看 expires_at，
// 因而零值会话恒无效，创建时刻之前也无法靠 CreatedAt 挽救（Requirement 5.2、CP6）。
func TestSessionActive(t *testing.T) {
	created := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	expires := SessionExpiry(created)
	session := model.AdminSession{
		TokenHash: HashSessionToken("raw-token"),
		CSRFToken: "csrf-token",
		CreatedAt: created,
		ExpiresAt: expires,
	}
	cst := time.FixedZone("CST", 8*3600)

	tests := []struct {
		name    string
		session model.AdminSession
		now     time.Time
		want    bool
	}{
		{"创建时刻", session, created, true},
		{"过期前一秒", session, expires.Add(-time.Second), true},
		{"过期前一纳秒", session, expires.Add(-time.Nanosecond), true},
		{"恰好过期时刻", session, expires, false},
		{"过期后一纳秒", session, expires.Add(time.Nanosecond), false},
		{"过期后一秒", session, expires.Add(time.Second), false},
		{"同一时刻的东八区表示", session, expires.In(cst), false},
		{"创建前一秒", session, created.Add(-time.Second), true},
		{"零值会话", model.AdminSession{}, created, false},
		{"过期时刻为零值", model.AdminSession{ExpiresAt: time.Time{}}, created, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionActive(tt.session, tt.now); got != tt.want {
				t.Fatalf("SessionActive(expires=%s, now=%s) 期望 %v，实际 %v",
					tt.session.ExpiresAt, tt.now, tt.want, got)
			}
		})
	}
}
