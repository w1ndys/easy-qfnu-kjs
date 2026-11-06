// Package auth 提供管理端鉴权的纯逻辑：密码规则、argon2id 哈希与校验。
// 它不依赖 HTTP、数据库或配置，便于脱离基础设施做单元测试。
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	// MinPasswordRunes 是密码长度下限，按 Unicode 码点计而非字节数（Requirement 2.2）。
	MinPasswordRunes = 12
	// MaxPasswordRunes 是密码长度上限，挡住超长输入（Requirement 2）。
	MaxPasswordRunes = 256

	// Argon2MemoryKiB 是 argon2id 的内存参数 m（KiB），OWASP 2024 最低推荐档（D4）。
	Argon2MemoryKiB = 19456
	// Argon2Time 是 argon2id 的迭代次数 t。
	Argon2Time = 2
	// Argon2Parallelism 是 argon2id 的并行度 p；固定 1 让一次计算只压一个核，便于给并发封顶。
	Argon2Parallelism = 1
	// Argon2SaltLen 是随机盐长度（字节）。
	Argon2SaltLen = 16
	// Argon2KeyLen 是输出摘要长度（字节）。
	Argon2KeyLen = 32
)

const (
	// argon2Version 是写进 PHC 串的 argon2 版本号，v=19 即 0x13。
	argon2Version = 19
	// hashConcurrency 是同一时刻允许进行的 argon2id 计算数：每次计算的内存量由串内 m 决定，
	// 解析上限为 64 MiB，容量 2 把最坏内存占用控在约 128 MiB（2×64 MiB，D4）；
	// 生产路径只写 19456（19 MiB），此时并发两份约 38 MiB，超出的调用在信号量上排队。
	hashConcurrency = 2
	// maxArgon2MemoryKiB 是接受的最大 m（KiB）：64 MiB。本系统只写 19456（19 MiB），
	// 上限留约 3 倍余量给调参，同时挡住损坏串里天文数字的 m，
	// 避免按串内参数重算时申请巨量内存。
	maxArgon2MemoryKiB = 64 << 10
	// maxArgon2Time 是接受的最大 t：本系统只写 2，上限 4 足够容纳调参，
	// 又不会让一条损坏串把一次校验拖成 CPU 黑洞。
	maxArgon2Time = 4
	// maxArgon2Parallelism 是接受的最大 p：本系统只写 1，上限 4 足够容纳调参；
	// 更大的 p 既不会有真实用途，又会让损坏串按大并行度重算，因此越界按损坏处理。
	maxArgon2Parallelism = 4
	// minSaltLen / maxSaltLen 是接受的盐长度区间：本系统只写 16B，
	// 越界即按损坏处理（argon2 本身允许更短或更长的盐）。
	minSaltLen = 8
	maxSaltLen = 64
	// minKeyLen / maxKeyLen 是接受的摘要长度区间：本系统只写 32B。过短的摘要
	// （尤其空摘要）会让常量时间比对失去意义；过长的摘要只是白算，同样按损坏处理。
	minKeyLen = 16
	maxKeyLen = 128
)

var (
	// ErrPasswordMismatch 表示两次输入的密码不一致（Requirement 2.1）。
	ErrPasswordMismatch = errors.New("两次密码不一致")
	// ErrPasswordTooShort 表示密码码点数少于 MinPasswordRunes（Requirement 2.2）。
	ErrPasswordTooShort = errors.New("密码长度不足")
	// ErrPasswordTooLong 表示密码码点数超过 MaxPasswordRunes。
	ErrPasswordTooLong = errors.New("密码过长")

	// ErrHashMalformed 表示存储的 PHC 串无法解析或参数越界。
	// 它总是以 %w 包装后返回，调用方可用 errors.Is 区分「哈希损坏」与「密码错误」：
	// 前者说明库里的串已不可用（响应同样是凭据无效 40101），后者只是这次输错了。
	// 错误信息里不放哈希串与密码，避免把凭据带进日志。
	ErrHashMalformed = errors.New("密码哈希格式非法")
)

var (
	// hashSlots 是并发哈希的信号量，生产代码只在 deriveKey 一处获取与释放；
	// 仅测试可直接读写：用例会占满或清空它来观察排队，因此测试不得 t.Parallel。
	hashSlots = make(chan struct{}, hashConcurrency)
	// hashCallCount 记录本进程内已执行的 argon2id 计算次数，HashPassword 与 VerifyPassword 都计入。
	hashCallCount atomic.Int64
	// argon2IDKey 是 argon2.IDKey 的间接引用：生产恒为 argon2.IDKey；
	// 仅测试可替换，用来观察并发上限与计数，不必真的算 19 MiB，也不必依赖计算耗时。
	argon2IDKey = argon2.IDKey
)

// ValidatePassword 校验两次输入的密码：先比一致性，再按码点数检查上下限。
// 它与哈希彻底分开，被拒请求在业务层就能被挡下，不会进入 argon2（Requirement 2.1、2.2）。
// 顺序固定为「不一致 → 过短 → 过长」：两个字段都填了却对不上时，先报不一致更贴近用户操作。
func ValidatePassword(password, confirm string) error {
	if password != confirm {
		return ErrPasswordMismatch
	}
	runes := utf8.RuneCountInString(password)
	if runes < MinPasswordRunes {
		return ErrPasswordTooShort
	}
	if runes > MaxPasswordRunes {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword 生成自描述的 argon2id PHC 串：$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>。
// 参数写进串里，校验时无需读配置；每次调用都用 crypto/rand 重新取 16B 盐，
// 因此同一明文两次的串必不相同（Requirement 2.3）。
func HashPassword(password string) (string, error) {
	salt := make([]byte, Argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成 argon2id 盐: %w", err)
	}
	key := deriveKey(password, salt, Argon2MemoryKiB, Argon2Time, Argon2Parallelism, Argon2KeyLen)
	return formatPHC(salt, key), nil
}

// VerifyPassword 解析 PHC 串，按串内参数重算摘要并做常量时间比对。
// 摘要不一致返回 (false, nil)；串损坏或参数越界返回 (false, err)，且 err 一定
// 满足 errors.Is(err, ErrHashMalformed)，供 api 区分「库里串坏了」与「密码输错了」。
// 两条路径都不 panic（Requirement 2.4、2.5）。
func VerifyPassword(phc, password string) (bool, error) {
	parsed, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	key := deriveKey(password, parsed.salt, parsed.memory, parsed.rounds, parsed.parallelism, uint32(len(parsed.key)))
	return subtle.ConstantTimeCompare(key, parsed.key) == 1, nil
}

// HashCallCount 返回本进程内已执行的 argon2id 计算次数（HashPassword 与 VerifyPassword 都计入）。
// 供限流等用例断言「被拒的请求没有触发计算」；解析阶段就失败的损坏串不计入。
func HashCallCount() int64 {
	return hashCallCount.Load()
}

// deriveKey 在信号量内执行一次 argon2id 计算并计数：并发上限与计数只在这一处生效，
// 哈希与校验共用，避免多条登录路径同时各占一份 19 MiB（D4、CP9）。
func deriveKey(password string, salt []byte, memory, rounds uint32, parallelism uint8, keyLen uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	hashCallCount.Add(1)
	return argon2IDKey([]byte(password), salt, rounds, memory, parallelism, keyLen)
}

// formatPHC 把盐与摘要拼成 PHC 串，参数取编译期常量，保证写出的串格式稳定。
func formatPHC(salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, Argon2MemoryKiB, Argon2Time, Argon2Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// phcHash 是解析后的 PHC 字段：参数来自串本身，盐与摘要已解码。
type phcHash struct {
	memory      uint32 // m，KiB
	rounds      uint32 // t
	parallelism uint8  // p
	salt        []byte
	key         []byte
}

// parsePHC 严格解析 $argon2id$v=19$m=..,t=..,p=..$<salt>$<hash>：
// 段数、算法、版本、参数名与取值范围、base64 编码逐项校验，
// 任何一项不符都返回 ErrHashMalformed，把会导致 argon2 内部 panic 的参数挡在计算之前。
func parsePHC(phc string) (phcHash, error) {
	var parsed phcHash

	parts := strings.Split(phc, "$")
	// 期望恰好 6 段：空串 + argon2id + v=19 + 参数 + 盐 + 摘要。
	if len(parts) != 6 || parts[0] != "" {
		return parsed, fmt.Errorf("%w: 期望 6 段 PHC 结构", ErrHashMalformed)
	}
	if parts[1] != "argon2id" {
		return parsed, fmt.Errorf("%w: 算法不是 argon2id", ErrHashMalformed)
	}
	version, ok := strings.CutPrefix(parts[2], "v=")
	if !ok {
		return parsed, fmt.Errorf("%w: 缺少版本段", ErrHashMalformed)
	}
	if v, err := strconv.ParseUint(version, 10, 32); err != nil || v != argon2Version {
		return parsed, fmt.Errorf("%w: 版本不是 v=%d", ErrHashMalformed, argon2Version)
	}

	if err := parsePHCParams(parts[3], &parsed); err != nil {
		return parsed, err
	}
	if err := validatePHCParams(parsed); err != nil {
		return parsed, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return parsed, fmt.Errorf("%w: 盐不是合法 base64 或长度不在 %d..%d 字节",
			ErrHashMalformed, minSaltLen, maxSaltLen)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < minKeyLen || len(key) > maxKeyLen {
		return parsed, fmt.Errorf("%w: 摘要不是合法 base64 或长度不在 %d..%d 字节",
			ErrHashMalformed, minKeyLen, maxKeyLen)
	}
	parsed.salt, parsed.key = salt, key
	return parsed, nil
}

// parsePHCParams 解析 m/t/p 三个参数，不接受缺项、重复、未知名字与非十进制值。
func parsePHCParams(segment string, parsed *phcHash) error {
	params := strings.Split(segment, ",")
	if len(params) != 3 {
		return fmt.Errorf("%w: 参数应为 m、t、p 三项", ErrHashMalformed)
	}
	seen := make(map[string]bool, 3)
	for _, param := range params {
		name, value, ok := strings.Cut(param, "=")
		if !ok || seen[name] {
			return fmt.Errorf("%w: 参数格式非法或重复", ErrHashMalformed)
		}
		seen[name] = true
		switch name {
		case "m":
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return fmt.Errorf("%w: m 不是合法整数", ErrHashMalformed)
			}
			parsed.memory = uint32(n)
		case "t":
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return fmt.Errorf("%w: t 不是合法整数", ErrHashMalformed)
			}
			parsed.rounds = uint32(n)
		case "p":
			n, err := strconv.ParseUint(value, 10, 8)
			if err != nil {
				return fmt.Errorf("%w: p 不是合法整数", ErrHashMalformed)
			}
			parsed.parallelism = uint8(n)
		default:
			return fmt.Errorf("%w: 未知参数 %q", ErrHashMalformed, name)
		}
	}
	return nil
}

// validatePHCParams 检查会越过安全边界的取值：t ∈ [1, 4]、p ∈ [1, 4]、m ∈ [8×p, 64 MiB]。
// 越界后果各不相同：t<1 与 p<1 会让 Go 的 argon2 直接 panic；m 超过上限则按串内声明的
// 内存量申请巨量内存；m<8p 违反 RFC 9106，Go 实现会把内存静默抬到 8p，串里写的参数
// 与实际计算不一致。三类都按损坏处理，损坏串才既不会崩进程、也不会烧掉内存与 CPU。
func validatePHCParams(parsed phcHash) error {
	if parsed.rounds < 1 || parsed.rounds > maxArgon2Time {
		return fmt.Errorf("%w: t 超出支持范围", ErrHashMalformed)
	}
	if parsed.parallelism < 1 || parsed.parallelism > maxArgon2Parallelism {
		return fmt.Errorf("%w: p 超出支持范围", ErrHashMalformed)
	}
	if parsed.memory < 8*uint32(parsed.parallelism) || parsed.memory > maxArgon2MemoryKiB {
		return fmt.Errorf("%w: m 超出支持范围", ErrHashMalformed)
	}
	return nil
}
