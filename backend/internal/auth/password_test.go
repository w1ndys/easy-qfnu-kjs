package auth

// 本文件所有用例都禁止使用 t.Parallel：测试会替换包级 argon2IDKey、占满或清空 hashSlots，
// 并行执行会让「替换实现」「占满槽位」与其他用例互相串扰。新增用例请保持串行。

import (
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// phcPattern 是 HashPassword 必须产出的 PHC 形态：算法、版本、参数固定，
// 盐 16B 编码成 22 个 base64 字符，摘要 32B 编码成 43 个，且无填充。
var phcPattern = regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$[A-Za-z0-9+/]{22}\$[A-Za-z0-9+/]{43}$`)

// mustHash 用真实 argon2 生成一个哈希串；生成失败直接终止用例。
func mustHash(t *testing.T, password string) string {
	t.Helper()
	phc, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword 应成功，实际 err=%v", err)
	}
	return phc
}

// stubArgon2Key 替换 argon2 计算实现，用于观察并发上限与计数；
// 测试结束时恢复，避免影响其他用例（本包测试不并行）。
func stubArgon2Key(t *testing.T, fn func(password, salt []byte, rounds, memory uint32, parallelism uint8, keyLen uint32) []byte) {
	t.Helper()
	previous := argon2IDKey
	argon2IDKey = fn
	t.Cleanup(func() { argon2IDKey = previous })
}

// drainHashSlots 非阻塞清空信号量，保证用例失败时不会把后续哈希卡死。
func drainHashSlots(t *testing.T) {
	t.Helper()
	for {
		select {
		case <-hashSlots:
		default:
			return
		}
	}
}

// TestValidatePassword 覆盖一致性、码点上下限与多字节字符：12/256 码点是闭区间边界，
// 全角与 emoji 按码点而不是 UTF-8 字节计（Requirement 2.1、2.2）。
func TestValidatePassword(t *testing.T) {
	ascii12 := strings.Repeat("a", 12)
	wide11 := strings.Repeat("密", 11) // 11 码点、33 字节
	wide12 := strings.Repeat("密", 12)
	emoji11 := strings.Repeat("😀", 11) // 11 码点、44 字节
	emoji12 := strings.Repeat("😀", 12)

	tests := []struct {
		name     string
		password string
		confirm  string
		want     error
	}{
		{"12 个 ASCII 码点通过", ascii12, ascii12, nil},
		{"11 个 ASCII 码点太短", strings.Repeat("a", 11), strings.Repeat("a", 11), ErrPasswordTooShort},
		{"13 个 ASCII 码点通过", strings.Repeat("a", 13), strings.Repeat("a", 13), nil},
		{"空密码太短", "", "", ErrPasswordTooShort},
		{"256 个 ASCII 码点通过（上限含）", strings.Repeat("a", 256), strings.Repeat("a", 256), nil},
		{"257 个 ASCII 码点过长", strings.Repeat("a", 257), strings.Repeat("a", 257), ErrPasswordTooLong},
		{"两次不一致", ascii12, strings.Repeat("b", 12), ErrPasswordMismatch},
		{"不一致优先于长度不足", "short", "other", ErrPasswordMismatch},
		{"12 个全角码点通过", wide12, wide12, nil},
		{"11 个全角码点按码点计太短（33 字节）", wide11, wide11, ErrPasswordTooShort},
		{"12 个 emoji 通过", emoji12, emoji12, nil},
		{"11 个 emoji 按码点计太短（44 字节）", emoji11, emoji11, ErrPasswordTooShort},
		{"256 个全角码点通过", strings.Repeat("密", 256), strings.Repeat("密", 256), nil},
		{"257 个全角码点过长", strings.Repeat("密", 257), strings.Repeat("密", 257), ErrPasswordTooLong},
		{"仅确认值过长也算不一致", ascii12, ascii12 + "x", ErrPasswordMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidatePassword(tt.password, tt.confirm); !errors.Is(err, tt.want) {
				t.Fatalf("ValidatePassword 期望 %v，实际 %v", tt.want, err)
			}
		})
	}
}

// TestValidatePasswordRuneCountProperty 用不同字节宽度的字符把「按码点计数」钉成性质：
// 0..260 个码点逐一验证，只有 [MinPasswordRunes, MaxPasswordRunes] 闭区间才通过。
func TestValidatePasswordRuneCountProperty(t *testing.T) {
	fills := []struct {
		name string
		r    rune
	}{
		{"ASCII", 'a'},
		{"全角", '密'},
		{"emoji", '😀'},
		{"零宽空格", '\u200b'},
	}
	for _, fill := range fills {
		for n := 0; n <= MaxPasswordRunes+4; n++ {
			password := strings.Repeat(string(fill.r), n)
			var want error
			switch {
			case n < MinPasswordRunes:
				want = ErrPasswordTooShort
			case n > MaxPasswordRunes:
				want = ErrPasswordTooLong
			}
			if err := ValidatePassword(password, password); !errors.Is(err, want) {
				t.Fatalf("%s × %d 码点期望 %v，实际 %v", fill.name, n, want, err)
			}
		}
	}
}

// TestHashPasswordPHCFormat 断言 PHC 串自带完整参数、盐与摘要长度正确、无填充，
// 且同一密码两次哈希因随机盐而不同（Requirement 2.3）。
func TestHashPasswordPHCFormat(t *testing.T) {
	const password = "correct horse battery staple"
	phc := mustHash(t, password)

	if !phcPattern.MatchString(phc) {
		t.Fatalf("PHC 串形态不符：%q", phc)
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 {
		t.Fatalf("PHC 段数期望 6，实际 %d：%q", len(parts), phc)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		t.Fatalf("盐应是合法的无填充 base64：%v", err)
	}
	if len(salt) != Argon2SaltLen {
		t.Fatalf("盐长度期望 %d 字节，实际 %d", Argon2SaltLen, len(salt))
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		t.Fatalf("摘要应是合法的无填充 base64：%v", err)
	}
	if len(key) != Argon2KeyLen {
		t.Fatalf("摘要长度期望 %d 字节，实际 %d", Argon2KeyLen, len(key))
	}
	if strings.Contains(parts[4], "=") || strings.Contains(parts[5], "=") {
		t.Fatalf("盐与摘要不应带 base64 填充：%q", phc)
	}
	if strings.Contains(phc, password) {
		t.Fatalf("PHC 串不应包含明文密码")
	}

	other := mustHash(t, password)
	if other == phc {
		t.Fatalf("同一密码两次哈希应因随机盐而不同：%q", phc)
	}
}

// TestVerifyPassword 覆盖正确密码、错误密码、空密码与「前缀正确的长密码」，
// 这些情况都应走到常量时间比对并给出确定结论。
func TestVerifyPassword(t *testing.T) {
	const password = "a-very-long-password"
	phc := mustHash(t, password)

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"正确密码", password, true},
		{"错误密码", "a-very-long-passworD", false},
		{"空密码", "", false},
		{"前缀相同但更长", password + "x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(phc, tt.password)
			if err != nil {
				t.Fatalf("合法 PHC 校验不应返回错误，实际 err=%v", err)
			}
			if ok != tt.want {
				t.Fatalf("VerifyPassword 期望 %v，实际 %v", tt.want, ok)
			}
		})
	}
}

// TestVerifyPasswordUsesParamsFromPHC 手工构造一份 m/t/p 与常量不同的合法 PHC，
// 校验证明确实按串内参数重算（若偷用编译期常量会得到不同摘要而失败）。
func TestVerifyPasswordUsesParamsFromPHC(t *testing.T) {
	const password = "another-long-password"
	salt := []byte{0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49} // 8B 盐，恰好是可接受下限
	key := argon2.IDKey([]byte(password), salt, 1, 8192, 1, 16)    // t=1, m=8192, p=1, 16B 摘要
	phc := "$argon2id$v=19$m=8192,t=1,p=1$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)

	ok, err := VerifyPassword(phc, password)
	if err != nil || !ok {
		t.Fatalf("按串内参数重算应通过，实际 ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(phc, password+"x")
	if err != nil || ok {
		t.Fatalf("错误密码应按串内参数判失败，实际 ok=%v err=%v", ok, err)
	}
}

// TestVerifyPasswordComputesWithMaxParallelismAndKeyLen 用真实 argon2 跑一次「p 与 keyLen 取到解析上限」：
// p=4、摘要 128B、t=2、m=19456（19 MiB，与生产同档）。解析层通过不等于计算不 panic——
// 只有真算一遍才能证明 p 与 keyLen 取到上限时 VerifyPassword 不崩，也不会因 argon2
// 静默改写参数而算出与串内摘要不符的结果（D4）。m=64 MiB 的上限由参数边界用例覆盖，不真算。
func TestVerifyPasswordComputesWithMaxParallelismAndKeyLen(t *testing.T) {
	const password = "another-long-password"
	salt := make([]byte, Argon2SaltLen) // 16B 全零盐：本用例只验计算路径，不验随机性
	// 两个解析上限一起取满：p=4、摘要 128B；m 取生产档 19456（远低于 64 MiB 上限）。
	key := argon2.IDKey([]byte(password), salt, 2, Argon2MemoryKiB, 4, 128)
	if len(key) != 128 {
		t.Fatalf("摘要长度期望 128 字节，实际 %d", len(key))
	}
	phc := "$argon2id$v=19$m=19456,t=2,p=4$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key)

	ok, err := VerifyPassword(phc, password)
	if err != nil {
		t.Fatalf("p=4、摘要 128B 的合法 PHC 应当算得出来，实际 err=%v", err)
	}
	if !ok {
		t.Fatal("正确密码在 p=4、摘要 128B 下应校验通过，实际 false")
	}

	ok, err = VerifyPassword(phc, password+"x")
	if err != nil {
		t.Fatalf("错误密码只该比对失败，不应报错，实际 err=%v", err)
	}
	if ok {
		t.Fatal("错误密码在 p=4、摘要 128B 下不应校验通过，实际 true")
	}
}

// TestVerifyPasswordRejectsMalformedHash 覆盖算法、版本、段数、参数、base64 与长度层面的损坏：
// 全部必须返回校验失败与非 nil 错误，且不得 panic（Requirement 2.5）。
func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	const password = "a-very-long-password"
	phc := mustHash(t, password)
	parts := strings.Split(phc, "$")
	salt, key := parts[4], parts[5]
	shortKey := base64.RawStdEncoding.EncodeToString(make([]byte, 8))
	// 盐 65B、摘要 129B 各越过上限一格：公共入口必须同样按损坏处理。
	// 字节数直接写死并与常量核对，常量一旦调整这里立刻失败，用例名不会悄悄说谎。
	if maxSaltLen != 64 || maxKeyLen != 128 {
		t.Fatalf("上限期望 salt=64 key=128，实际 salt=%d key=%d（改常量须同步本用例）",
			maxSaltLen, maxKeyLen)
	}
	longSalt := base64.RawStdEncoding.EncodeToString(make([]byte, 65))
	longKey := base64.RawStdEncoding.EncodeToString(make([]byte, 129))

	tests := []struct {
		name string
		phc  string
	}{
		{"空串", ""},
		{"非 PHC 文本", "argon2id"},
		{"算法不是 argon2id", "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
		{"算法大小写不符", "$Argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key},
		{"版本不是 19", "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key},
		{"缺少版本段", "$argon2id$m=19456,t=2,p=1$" + salt + "$" + key},
		{"只有五段", "$argon2id$v=19$m=19456,t=2,p=1$" + salt},
		{"多出一段", phc + "$extra"},
		{"缺少参数项", "$argon2id$v=19$m=19456,t=2$" + salt + "$" + key},
		{"参数项多余", "$argon2id$v=19$m=19456,t=2,p=1,q=1$" + salt + "$" + key},
		{"参数重复", "$argon2id$v=19$m=19456,m=19456,t=2,p=1$" + salt + "$" + key},
		{"参数不是整数", "$argon2id$v=19$m=abc,t=2,p=1$" + salt + "$" + key},
		{"参数为负数", "$argon2id$v=19$m=-19456,t=2,p=1$" + salt + "$" + key},
		{"m 为零", "$argon2id$v=19$m=0,t=2,p=1$" + salt + "$" + key},
		{"t 为零", "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key},
		{"p 为零", "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key},
		{"p 超出单字节", "$argon2id$v=19$m=19456,t=2,p=256$" + salt + "$" + key},
		{"m 超出上限", "$argon2id$v=19$m=65537,t=2,p=1$" + salt + "$" + key},
		{"t 超出上限", "$argon2id$v=19$m=19456,t=5,p=1$" + salt + "$" + key},
		{"m 小于 8×p", "$argon2id$v=19$m=8,t=2,p=2$" + salt + "$" + key},
		{"盐为空", "$argon2id$v=19$m=19456,t=2,p=1$$" + key},
		{"盐不是 base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!!$" + key},
		{"盐过短", "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString([]byte("1234")) + "$" + key},
		{"盐过长（65B）", "$argon2id$v=19$m=19456,t=2,p=1$" + longSalt + "$" + key},
		{"摘要为空", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$"},
		{"摘要不是 base64", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$!!!!"},
		{"摘要过短", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + shortKey},
		{"摘要过长（129B）", "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + longKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(tt.phc, password)
			if ok {
				t.Fatalf("损坏的 PHC 不应校验通过：%q", tt.phc)
			}
			if err == nil {
				t.Fatalf("损坏的 PHC 应返回错误：%q", tt.phc)
			}
			if !errors.Is(err, ErrHashMalformed) {
				t.Fatalf("损坏的 PHC 应返回 ErrHashMalformed，实际 %v", err)
			}
		})
	}
}

// TestVerifyPasswordTruncatedHash 把合法串逐字符截断：任何截断都不得通过校验、不得 panic，
// 且失败只允许两种形态——仍可解码时由常量时间比对判 (false, nil)，
// 段数或 base64 被破坏时返回 ErrHashMalformed，不会冒出第三种错误。
func TestVerifyPasswordTruncatedHash(t *testing.T) {
	const password = "a-very-long-password"
	phc := mustHash(t, password)

	for cut := 1; cut < len(phc); cut++ {
		truncated := phc[:len(phc)-cut]
		ok, err := VerifyPassword(truncated, password)
		if ok {
			t.Fatalf("截断 %d 个字符后仍校验通过：%q", cut, truncated)
		}
		if err != nil && !errors.Is(err, ErrHashMalformed) {
			t.Fatalf("截断 %d 个字符后的错误应为 ErrHashMalformed，实际 %v", cut, err)
		}
	}
}

// TestVerifyPasswordTruncationSemantics 显式钉住截断的两条路径：
// 摘要少 1 或 3 个 base64 字符仍可解码（31/30 字节，均 ≥ minKeyLen），
// 解析通过、比对失败，返回 (false, nil)——绝不能报「哈希损坏」；
// 截到只剩 5 段、或截成非法 base64 长度时，返回 ErrHashMalformed。
func TestVerifyPasswordTruncationSemantics(t *testing.T) {
	const password = "a-very-long-password"
	phc := mustHash(t, password)
	parts := strings.Split(phc, "$")
	// 5 段：丢掉「$<hash>」整段，只留下空串 + argon2id + v=19 + 参数 + 盐。
	noKeySegment := strings.Join(parts[:5], "$")
	// 41 个字符的 base64 长度非法（41 mod 4 == 1），解码直接失败。
	badBase64 := phc[:len(phc)-2]

	tests := []struct {
		name       string
		phc        string
		wantErr    bool
		wantSalt   int
		wantKeyLen int
	}{
		{"摘要少 1 个字符仍可解码", phc[:len(phc)-1], false, Argon2SaltLen, Argon2KeyLen - 1},
		{"摘要少 3 个字符仍可解码", phc[:len(phc)-3], false, Argon2SaltLen, Argon2KeyLen - 2},
		{"截到只剩 5 段", noKeySegment, true, 0, 0},
		{"截成非法 base64 长度", badBase64, true, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(tt.phc, password)
			if ok {
				t.Fatalf("截断串不应校验通过：%q", tt.phc)
			}
			if tt.wantErr {
				if !errors.Is(err, ErrHashMalformed) {
					t.Fatalf("应返回 ErrHashMalformed，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("可解码的截断串应返回 (false, nil)，实际 err=%v", err)
			}
			// 解析层同样要认定它「结构合法」，长度正是截断后的字节数。
			parsed, parseErr := parsePHC(tt.phc)
			if parseErr != nil {
				t.Fatalf("可解码的截断串应能被解析，实际 err=%v", parseErr)
			}
			if len(parsed.salt) != tt.wantSalt || len(parsed.key) != tt.wantKeyLen {
				t.Fatalf("截断后长度期望 salt=%d key=%d，实际 salt=%d key=%d",
					tt.wantSalt, tt.wantKeyLen, len(parsed.salt), len(parsed.key))
			}
		})
	}
}

// TestParsePHCParameterBoundaries 直接用 parsePHC 钉住解析的上下限（不进入 argon2 计算，
// 因此 64 MiB / t=4 这一档不会真的申请内存或耗时）：
// m ∈ [8×p, 64 MiB]、t ∈ [1, 4]、p ∈ [1, 4]、salt ∈ [8, 64] 字节、key ∈ [16, 128] 字节。
func TestParsePHCParameterBoundaries(t *testing.T) {
	salt16 := base64.RawStdEncoding.EncodeToString(make([]byte, Argon2SaltLen))
	key32 := base64.RawStdEncoding.EncodeToString(make([]byte, Argon2KeyLen))
	bytesSegment := func(n int) string {
		return base64.RawStdEncoding.EncodeToString(make([]byte, n))
	}
	build := func(params, salt, key string) string {
		return "$argon2id$v=19$" + params + "$" + salt + "$" + key
	}

	tests := []struct {
		name       string
		phc        string
		wantErr    bool
		wantSalt   int
		wantKeyLen int
	}{
		{"m 取上限 64 MiB", build("m=65536,t=2,p=1", salt16, key32), false, Argon2SaltLen, Argon2KeyLen},
		{"m 超上限一格", build("m=65537,t=2,p=1", salt16, key32), true, 0, 0},
		{"t 取上限 4", build("m=19456,t=4,p=1", salt16, key32), false, Argon2SaltLen, Argon2KeyLen},
		{"t 超上限一格", build("m=19456,t=5,p=1", salt16, key32), true, 0, 0},
		{"p 取上限 4", build("m=19456,t=2,p=4", salt16, key32), false, Argon2SaltLen, Argon2KeyLen},
		{"p 超上限一格", build("m=19456,t=2,p=5", salt16, key32), true, 0, 0},
		{"盐取下限 8B", build("m=19456,t=2,p=1", bytesSegment(8), key32), false, 8, Argon2KeyLen},
		{"盐低于下限一格", build("m=19456,t=2,p=1", bytesSegment(7), key32), true, 0, 0},
		{"盐取上限 64B", build("m=19456,t=2,p=1", bytesSegment(64), key32), false, 64, Argon2KeyLen},
		{"盐超上限一格", build("m=19456,t=2,p=1", bytesSegment(65), key32), true, 0, 0},
		{"摘要取下限 16B", build("m=19456,t=2,p=1", salt16, bytesSegment(16)), false, Argon2SaltLen, 16},
		{"摘要低于下限一格", build("m=19456,t=2,p=1", salt16, bytesSegment(15)), true, 0, 0},
		{"摘要取上限 128B", build("m=19456,t=2,p=1", salt16, bytesSegment(128)), false, Argon2SaltLen, 128},
		{"摘要超上限一格", build("m=19456,t=2,p=1", salt16, bytesSegment(129)), true, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parsePHC(tt.phc)
			if tt.wantErr {
				if !errors.Is(err, ErrHashMalformed) {
					t.Fatalf("越界参数应返回 ErrHashMalformed，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("边界内的参数应解析成功，实际 err=%v", err)
			}
			if len(parsed.salt) != tt.wantSalt || len(parsed.key) != tt.wantKeyLen {
				t.Fatalf("解析长度期望 salt=%d key=%d，实际 salt=%d key=%d",
					tt.wantSalt, tt.wantKeyLen, len(parsed.salt), len(parsed.key))
			}
		})
	}
}

// TestHashPasswordConcurrent 用真实 argon2 并发计算：并发上限不应造成死锁或串号，
// 每个结果都应可用原始密码校验通过。
func TestHashPasswordConcurrent(t *testing.T) {
	const (
		workers  = 4
		password = "a-very-long-password"
	)

	phcs := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			phcs[i], errs[i] = HashPassword(password)
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[string]bool, workers)
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发哈希失败：%v", i+1, errs[i])
		}
		if seen[phcs[i]] {
			t.Fatalf("并发哈希出现重复串：%q", phcs[i])
		}
		seen[phcs[i]] = true
		ok, err := VerifyPassword(phcs[i], password)
		if err != nil || !ok {
			t.Fatalf("第 %d 个并发哈希无法用原密码校验通过：ok=%v err=%v", i+1, ok, err)
		}
	}
}

// TestHashPasswordWaitsForFreeSlot 占满容量 2 的信号量，断言 HashPassword 在开始 argon2
// 计算之前就阻塞，释放一个槽位后才继续——这条用例不依赖真实计算耗时（D4）。
func TestHashPasswordWaitsForFreeSlot(t *testing.T) {
	if cap(hashSlots) != hashConcurrency {
		t.Fatalf("信号量容量期望 %d，实际 %d", hashConcurrency, cap(hashSlots))
	}
	if hashConcurrency != 2 {
		t.Fatalf("设计要求的并发上限是 2，实际 %d", hashConcurrency)
	}

	drainHashSlots(t)
	hashSlots <- struct{}{}
	hashSlots <- struct{}{}
	t.Cleanup(func() { drainHashSlots(t) })

	entered := make(chan struct{}, 1)
	stubArgon2Key(t, func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		select {
		case entered <- struct{}{}:
		default:
		}
		return make([]byte, keyLen)
	})

	done := make(chan error, 1)
	go func() {
		_, err := HashPassword("a-very-long-password")
		done <- err
	}()

	select {
	case <-entered:
		t.Fatal("两个槽位都被占用时仍进入了 argon2id 计算")
	case <-time.After(100 * time.Millisecond):
	}

	<-hashSlots // 释放一个槽位，允许排队的哈希继续
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("槽位释放后 2s 内仍未开始 argon2id 计算")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("槽位释放后哈希应成功，实际 err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HashPassword 在槽位释放后 2s 内未返回")
	}
}

// TestVerifyPasswordWaitsForFreeSlot 与 TestHashPasswordWaitsForFreeSlot 对称：
// 校验与哈希共用同一组槽位，两个槽位被占满时 VerifyPassword 也必须停在 argon2 计算之前（D4）。
func TestVerifyPasswordWaitsForFreeSlot(t *testing.T) {
	if cap(hashSlots) != hashConcurrency || hashConcurrency != 2 {
		t.Fatalf("信号量容量期望 2，实际 %d（常量 %d）", cap(hashSlots), hashConcurrency)
	}

	// 摘要取全零、桩函数也返回全零，因此只要计算被放行就必然是 (true, nil)：
	// 既能证明「卡在槽位上」，也能证明放行后真的走完了比对。
	salt := base64.RawStdEncoding.EncodeToString(make([]byte, Argon2SaltLen))
	zeroKey := base64.RawStdEncoding.EncodeToString(make([]byte, Argon2KeyLen))
	phc := "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + zeroKey

	drainHashSlots(t)
	hashSlots <- struct{}{}
	hashSlots <- struct{}{}
	t.Cleanup(func() { drainHashSlots(t) })

	entered := make(chan struct{}, 1)
	stubArgon2Key(t, func(_, _ []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		select {
		case entered <- struct{}{}:
		default:
		}
		return make([]byte, keyLen)
	})

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := VerifyPassword(phc, "a-very-long-password")
		done <- result{ok: ok, err: err}
	}()

	select {
	case <-entered:
		t.Fatal("两个槽位都被占用时校验仍进入了 argon2id 计算")
	case <-time.After(100 * time.Millisecond):
	}

	<-hashSlots // 释放一个槽位，允许排队的校验继续
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("槽位释放后 2s 内校验仍未开始 argon2id 计算")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("槽位释放后校验应成功，实际 err=%v", got.err)
		}
		if !got.ok {
			t.Fatal("摘要全零应比对通过，实际 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("VerifyPassword 在槽位释放后 2s 内未返回")
	}
}

// TestHashCallCount 断言计数只随真正执行的 argon2 计算增长（哈希与校验都算），
// 解析阶段就被拒的损坏串不增长——限流用例据此断言「被限流时不哈希」（CP9）。
func TestHashCallCount(t *testing.T) {
	const password = "a-very-long-password"
	start := HashCallCount()

	phc, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword 应成功，实际 err=%v", err)
	}
	if got := HashCallCount() - start; got != 1 {
		t.Fatalf("一次哈希后计数应 +1，实际 +%d", got)
	}

	ok, err := VerifyPassword(phc, password)
	if err != nil || !ok {
		t.Fatalf("正确密码应校验通过，实际 ok=%v err=%v", ok, err)
	}
	if got := HashCallCount() - start; got != 2 {
		t.Fatalf("一次校验后计数应再 +1，实际累计 +%d", got)
	}

	ok, err = VerifyPassword(phc, "a-very-long-passworD")
	if err != nil || ok {
		t.Fatalf("错误密码应校验失败，实际 ok=%v err=%v", ok, err)
	}
	if got := HashCallCount() - start; got != 3 {
		t.Fatalf("错误密码的校验同样要算一次，实际累计 +%d", got)
	}

	for _, malformed := range []string{"", "argon2id", "$argon2id$v=16$m=1,t=1,p=1$AAAA$AAAA"} {
		if ok, err := VerifyPassword(malformed, password); err == nil || ok {
			t.Fatalf("损坏串 %q 应解析失败，实际 ok=%v err=%v", malformed, ok, err)
		}
	}
	if got := HashCallCount() - start; got != 3 {
		t.Fatalf("解析失败的校验不应触发 argon2 计算，实际累计 +%d", got)
	}
}
