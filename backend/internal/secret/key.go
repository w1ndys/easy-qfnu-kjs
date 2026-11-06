// Package secret 管理业务秘密加密密钥的生命周期：密钥文件的创建、读取与内存持有；
// 本包同时提供基于该密钥的密文信封。
//
// 密钥落在文件而不是数据库，是为了让数据库备份与密钥备份分开保管：只有密钥不在库里，
// 「初始化时一次性导出的那份密钥」才真正具备灾备价值（design.md D2）。因此本包只认
// 调用方注入的路径（配置项 SECRET_KEY_PATH），不读环境变量、不碰数据库与 HTTP，
// 用临时目录就能把文件行为测干净。
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

const (
	// KeyLen 是密钥的字节数：AES-256 要求 32 字节。
	KeyLen = 32

	// encodedKeyLen 是 32 字节密钥按 base64url 无填充编码后的字符数：ceil(32*8/6) = 43。
	encodedKeyLen = 43

	// keyFileMode 是创建密钥文件时写进去的权限：只有属主可读写（CP13）。
	// Load 的判定口径比它宽一档——只要求「不开放属主之外访问」（perm&0o077 == 0，
	// 0400/0500/0600/0700 都算合规），运维自己把文件收紧成只读不会让启动失败。
	keyFileMode = 0o600
	// keyDirMode 是新建父目录的权限：只有属主可进入。
	keyDirMode = 0o700
)

// Key 是一把 32 字节对称密钥。它是值类型（定长数组），赋值与传参都发生拷贝，
// 调用方拿到的副本不会与 Manager 内存里的那份共享底层数组。
type Key [KeyLen]byte

// Manager 持有密钥文件路径与当前已装载的密钥。
// Present/Current/Path 走读锁，只有 Load/Create/Reload 会改动内存状态，因此装配期之后的
// 并发读不需要调用方额外加锁。
type Manager struct {
	path string

	mu     sync.RWMutex
	key    Key
	loaded bool
}

var (
	// syncFile、closeFile、writeFile、removeFile 与 readAll 是包内可替换的 seam：单测用它模拟
	// 落盘、关闭、写入、清理失败，以及「Stat 之后文件被追加」这类读到的字节数与 Size 不符的
	// 时序，这几种故障在本机文件系统上无法稳定制造。生产恒为 (*os.File).Sync、
	// (*os.File).Close、(*os.File).WriteString、os.Remove 与 io.ReadAll。
	// 改写这些变量的用例不得 t.Parallel（用 key_test.go 的 stubSeam 替换并在结束时恢复）。
	syncFile   = (*os.File).Sync
	closeFile  = (*os.File).Close
	writeFile  = (*os.File).WriteString
	removeFile = os.Remove
	readAll    = io.ReadAll
)

// NewManager 返回指向 path 的 Manager，不产生任何文件 IO。
// 路径由配置注入，包内既不拼默认路径也不读环境变量，便于测试与装配分离。
func NewManager(path string) *Manager {
	return &Manager{path: path}
}

// Load 读取并校验密钥文件，成功时把密钥装入内存并返回 (true, nil)。
//
// 文件不存在返回 (false, nil)：未初始化是正常状态，不是错误；文件不存在时也不清空
// 已装载的密钥（要「文件没了就清空」的语义用 Reload，两者不合并）。
// 文件不可读、不是普通文件、权限过宽、文件尺寸超过 44 字节或内容非法返回
// (false, ErrKeyUnavailable)，此时同样不改动已装载的密钥：启动期降级只影响依赖密钥的能力，
// 已经拿到手的密钥仍然可用（CP14）。
// 打开、检查与读取共用同一个 *os.File 并且都在写锁内完成：与 Create 互斥，否则可能出现
// 「Load 读到旧文件、Create 写入新密钥，最后 Load 把旧密钥装进内存」的错位；权限、文件类型
// 与随后读到的字节也来自同一个 inode，没有 Stat 与 ReadFile 之间被换文件（TOCTOU）的窗口。
// 本方法在装配阶段调用一次；重复调用是幂等的。
func (m *Manager) Load() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, found, err := m.loadFile()
	if err != nil || !found {
		return false, err
	}

	m.key, m.loaded = key, true
	return true, nil
}

// loadFile 是 Load 与 Reload 共用的读取阶段：打开、检查并解析密钥文件，返回读到的密钥，
// 不接触内存状态。调用方必须持有 m.mu 写锁——它负责与 Create 互斥，也保证
// 「读到的字节」与「替换内存」之间不会被另一次写入插进来。
//
// 文件不存在返回 (Key{}, false, nil)：未初始化是正常状态，由调用方决定怎么处理内存。
// 其余失败（不可读、非普通文件、权限过宽、尺寸或内容非法）返回包裹
// model.ErrKeyUnavailable 的错误，同样不动内存。
func (m *Manager) loadFile() (Key, bool, error) {
	f, err := os.Open(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("打开密钥文件 %s: %w: %w", m.path, model.ErrKeyUnavailable, err)
	}
	// 只读句柄的关闭错误不改变结论（内容已经读完或没读成），显式丢弃。
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return Key{}, false, fmt.Errorf("检查密钥文件 %s: %w: %w", m.path, model.ErrKeyUnavailable, err)
	}

	// 先断文件类型再断权限：目录、FIFO、字符/块设备、套接字一律拒掉——对 FIFO 做 io.ReadAll
	// 会一直等写入方，读设备文件拿到的也不是密钥。类型判定放在权限之前，是因为路径指到目录上时
	// 报「权限过宽」会把运维引去 chmod，而真正该改的是路径。判定落在已打开的这个 fd 上，
	// 才不会与随后的读取看到两个不同文件。
	// 残余风险记在这里：open(2) 对没有写入方的 FIFO 本身就可能阻塞，本步挡住的是「打开之后
	// 去读」那一半；彻底堵住要引入平台相关的 O_NONBLOCK 或预先 stat，与这里的单句柄序列冲突，
	// 而密钥路径由运维配置，不值得为它分平台。阻塞一旦真的发生，它就在 loadFile 调用方
	// （Load/Reload）的写锁内：同一 Manager 上的 Present/Current 拿不到读锁，会一并等到
	// open(2) 返回；Load 在装配期只调用一次，最坏结果是启动卡住；Reload 由未认证的
	// init 触发，最坏结果是该请求与 state 的 Present/Current 一起等锁——这是已知接受项，
	// 而不是读到错的数据。
	if !info.Mode().IsRegular() {
		return Key{}, false, fmt.Errorf("密钥文件 %s 不是普通文件（%s）: %w",
			m.path, describeFileType(info.Mode()), model.ErrKeyUnavailable)
	}

	// 判定口径是「不得允许属主之外访问」：perm&0o077 == 0，0400/0500/0600/0700 都合规。
	// 0644 这类权限会让同机其他用户直接读走密钥明文，与 CP13 的 0600 约定不符，这里不静默
	// 接受，而是把实际权限写进错误，让运维一眼看出该 chmod。
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Key{}, false, fmt.Errorf("密钥文件 %s 权限 %#o 不得允许属主之外访问: %w",
			m.path, perm, model.ErrKeyUnavailable)
	}

	// 读取量必须封顶：路径由运维配置，误指到日志、镜像或数据库文件时，先前的 io.ReadAll 会把
	// 整份内容读进内存，再报一句「长度不符」。有效密钥文件恒为 44 字节，所以先用 Stat 拿到的
	// Size 拒掉超过 44 字节的文件——一个字节都不读，错误里也能写出真实大小，运维一眼看出指错了
	// 路径。这道门排在类型与权限之后：文件类型或权限不合规时已经先返回，走不到这里。
	if size := info.Size(); size > encodedKeyLen+1 {
		return Key{}, false, fmt.Errorf("密钥文件 %s 内容 %d 字节，超过 %d 字节: %w",
			m.path, size, encodedKeyLen+1, model.ErrKeyUnavailable)
	}

	// 读取再套一层 LimitReader 兜住「Stat 之后文件被追加」这种尺寸与内容不一致的情形：无论
	// 文件后来长到多大，这里最多读取 encodedKeyLen+2 字节（45 字节），多出来的字节交给
	// parseKey 按「长度不符」拒掉。
	raw, err := readAll(io.LimitReader(f, encodedKeyLen+2))
	if err != nil {
		return Key{}, false, fmt.Errorf("读取密钥文件 %s: %w: %w", m.path, model.ErrKeyUnavailable, err)
	}

	key, err := parseKey(raw)
	if err != nil {
		return Key{}, false, fmt.Errorf("解析密钥文件 %s: %w: %w", m.path, model.ErrKeyUnavailable, err)
	}

	return key, true, nil
}

// Reload 按磁盘上的现状对账内存状态：先读到局部变量，读成功才替换；只有文件确实不存在时
// 才清空内存。
//
// 它服务于两种场景：
//   - 同进程重新初始化：运维删掉管理员记录与密钥文件后直接重新 init，若内存里那份旧密钥
//     还在，Present 恒为真、Create 恒撞 ErrKeyExists，init 会被永久挡在 40903。文件已删时
//     必须清空内存副本，流程才能继续往下走。
//   - 内存为空、文件其实存在：进程刚重启还没 Load，或上次启动时装载失败。只信 Present 会让
//     初始化一路走到 CreateAdmin，再由 Create 的 O_EXCL 挡下并触发补偿，白建又白删一条记录。
//     这里按文件对账，文件可用时装回内存，让请求在 CreateAdmin 之前按 40903 被拒。
//
// 返回值与 Load 的区别只在「失败与缺失时怎么处理内存」：
//   - 文件不存在：(false, nil)，清空内存——只有 fs.ErrNotExist 会走到这一支；
//   - 文件存在但不可用（不可读、非普通文件、权限过宽、尺寸或内容非法）：(false, ErrKeyUnavailable)，
//     保留内存里那把旧密钥。这与 Load 的启动期降级同一口径（CP14）：磁盘这份现在读不出来，
//     不代表进程里已经拿到的那把失效，清掉它只会让本来还能解密的进程当场失能。调用方据此
//     拒绝初始化（40903），而不是覆盖文件（CP4）；
//   - 文件存在且可用：(true, nil)，替换内存。
//
// 读取、判定与替换都在同一把写锁内完成：与 Create/Load 互斥，不会出现
// 「读到一半被 Create 换掉、最后把旧密钥写回内存」的错位。
// 本方法不碰管理员记录：是否真的允许重新初始化，仍由调用方的 Admin() 判定。
func (m *Manager) Reload() (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key, found, err := m.loadFile()
	if err != nil {
		// 损坏或不可读：保留旧键，只报告「磁盘上这份不可用」。
		return false, err
	}
	if !found {
		m.key, m.loaded = Key{}, false
		return false, nil
	}

	m.key, m.loaded = key, true
	return true, nil
}

// describeFileType 把非普通文件的类型翻成中文名，供「不是普通文件（…）」的文案使用。
//
// Mode().Type() 的字符串形态是 os 那套单字母（d、D、p、S、L），运维看到「不是普通文件（d）」
// 还得查文档才知道是目录；类型判定要能直接指向「该改路径还是该换文件」。
// 判定顺序有讲究：字符设备同时置着 ModeDevice 位，所以 ModeCharDevice 必须先于 ModeDevice；
// 符号链接排在前面只是完备性——Load 用的是 os.Open 的句柄 Stat，符号链接在打开时已被跟随，
// 实际只会看到目标文件的类型。落到 default 的只有 ModeIrregular 这类极端情形。
func describeFileType(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "目录"
	case mode&fs.ModeSymlink != 0:
		return "符号链接"
	case mode&fs.ModeCharDevice != 0:
		return "字符设备"
	case mode&fs.ModeDevice != 0:
		return "块设备"
	case mode&fs.ModeNamedPipe != 0:
		return "命名管道"
	case mode&fs.ModeSocket != 0:
		return "套接字"
	default:
		return mode.Type().String()
	}
}

// Present 报告内存里是否已有一把可用密钥。
// 它不查文件系统，所以「内存为空、磁盘上已有文件」时仍返回 false；要按磁盘对账用 Reload。
// 文件内容非法时 Load/Reload 都会返回 ErrKeyUnavailable（Reload 还保留旧键），由调用方
// 据此拒绝初始化，而不是靠 Create 的 O_EXCL 兜底。
func (m *Manager) Present() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.loaded
}

// Create 生成一把新密钥、以 O_EXCL 加 0600 写入密钥文件，并装入内存返回。
//
// 已装载密钥或文件已存在时返回 ErrKeyExists 且不覆盖既有内容（CP4、CP13）：
// 内存判断挡住同一进程的二次初始化，O_EXCL 挡住其他进程与残留文件。
// 父目录不存在时按 0700 一次性补建（MkdirAll 只对新建层级生效）。
// 生成、建目录、写入、落盘（文件与父目录）、关闭任一步失败都返回 ErrKeyUnavailable，
// 并删掉写了一半的文件——否则下次初始化只会撞 ErrKeyExists，把部署卡死；删除成功后再对父目录
// fsync 一次，让「目录项已删除」也持久化（只删不刷的话掉电后目录项可能被捡回来）；
// 删除或这次补刷失败时，错误与触发清理的错误一起返回，交给上层记 ERROR。
func (m *Manager) Create() (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.loaded {
		return Key{}, model.ErrKeyExists
	}

	var key Key
	if _, err := rand.Read(key[:]); err != nil {
		return Key{}, fmt.Errorf("生成密钥: %w", model.ErrKeyUnavailable)
	}

	if dir := filepath.Dir(m.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, keyDirMode); err != nil {
			return Key{}, fmt.Errorf("创建密钥目录: %w", model.ErrKeyUnavailable)
		}
	}

	f, err := os.OpenFile(m.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if errors.Is(err, fs.ErrExist) {
		return Key{}, model.ErrKeyExists
	}
	if err != nil {
		return Key{}, fmt.Errorf("创建密钥文件: %w", model.ErrKeyUnavailable)
	}

	writeErr := writeKeyFile(f, key)
	closeErr := closeFile(f)
	if closeErr != nil {
		closeErr = fmt.Errorf("关闭密钥文件: %w: %w", model.ErrKeyUnavailable, closeErr)
	}
	var dirErr error
	if writeErr == nil && closeErr == nil {
		dirErr = syncKeyDir(filepath.Dir(m.path))
	}
	if writeErr != nil || closeErr != nil || dirErr != nil {
		removeErr := removeKeyFile(m.path)
		// 删掉文件后再刷一次父目录：目录项的新增与删除是两次独立变更，只删不刷时掉电后可能
		// 把刚删掉的目录项又捡回来，下次初始化照样撞 ErrKeyExists。删除失败说明文件还在，
		// 没有可持久化的删除，不必再刷。
		var cleanupDirErr error
		if removeErr == nil {
			if err := syncKeyDir(filepath.Dir(m.path)); err != nil {
				cleanupDirErr = fmt.Errorf("删除后同步密钥目录: %w", err)
			}
		}
		return Key{}, errors.Join(writeErr, closeErr, dirErr, removeErr, cleanupDirErr)
	}

	m.key, m.loaded = key, true
	return key, nil
}

// Current 返回当前已装载的密钥；没有可用密钥时返回 ErrKeyUnavailable。
func (m *Manager) Current() (Key, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.loaded {
		return Key{}, model.ErrKeyUnavailable
	}
	return m.key, nil
}

// Path 返回密钥文件路径，供日志与运维提示使用；日志里只出现路径，不出现密钥内容。
func (m *Manager) Path() string {
	return m.path
}

// Export 把密钥编码成 43 字符的 base64url 无填充文本，与密钥文件内容逐字符一致
// （文件内容只多一个换行），运维 cat 文件得到的就是这个串。
func (k Key) Export() string {
	return base64.RawURLEncoding.EncodeToString(k[:])
}

// Fingerprint 返回密钥指纹：SHA-256 的前 8 字节，写成 16 位小写十六进制。
// 它够短便于人眼核对备份副本，又不包含足够信息还原密钥（Requirement 3.5）。
func (k Key) Fingerprint() string {
	sum := sha256.Sum256(k[:])
	return hex.EncodeToString(sum[:8])
}

// writeKeyFile 把密钥写进刚以 O_EXCL 打开的文件并落盘。
//
// 写入走 writeFile seam：写失败与 Sync 失败是两条不同的分支，前者不落盘、后者落盘结果未知，
// 但两者都要让调用方清理半成品文件，单测用 seam 分别覆盖。
// 这里显式 Sync 是有理由的：明文密钥只在初始化响应里出现一次，若响应先于数据落盘发出，
// 进程或机器一崩就会留下一把「谁也不知道」的密钥，秘密再也解不开。
func writeKeyFile(f *os.File, key Key) error {
	if _, err := writeFile(f, key.Export()+"\n"); err != nil {
		return fmt.Errorf("写入密钥文件: %w: %w", model.ErrKeyUnavailable, err)
	}
	if err := syncFile(f); err != nil {
		return fmt.Errorf("同步密钥文件: %w: %w", model.ErrKeyUnavailable, err)
	}
	return nil
}

// syncKeyDir 对密钥文件所在目录执行一次 fsync。
//
// 文件的 O_CREAT 只保证文件内容落盘，目录项本身可能还在缓存里：掉电后会出现
// 「文件内容在、目录项没了」的密钥丢失。对父目录 Sync 能把新建的目录项刷下去
// （ext4 等文件系统的常规做法），代价只有初始化时一次系统调用。
func syncKeyDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("打开密钥目录 %s: %w: %w", dir, model.ErrKeyUnavailable, err)
	}
	syncErr := syncFile(d)
	closeErr := closeFile(d)
	if syncErr != nil {
		return fmt.Errorf("同步密钥目录 %s: %w: %w", dir, model.ErrKeyUnavailable, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭密钥目录 %s: %w: %w", dir, model.ErrKeyUnavailable, closeErr)
	}
	return nil
}

// removeKeyFile 删掉 Create 写了一半（内容可能不完整）的密钥文件。
// 删除失败时返回包裹 ErrKeyUnavailable 的错误，且保留底层原因，与触发清理的错误一起
// 交给上层记 ERROR：残留文件会让下次初始化永远撞 ErrKeyExists，必须让运维看见并手工清理。
func removeKeyFile(path string) error {
	if err := removeFile(path); err != nil {
		return fmt.Errorf("删除半成品密钥文件 %s: %w: %w", path, model.ErrKeyUnavailable, err)
	}
	return nil
}

// parseKey 把文件内容解析成密钥，只接受「43 个 base64url 字符 + 一个换行」这唯一形态。
//
// 校验分三层：总长度必须正好 44 字节、末字节必须是换行（两条分开报，运维才知道该截断文件
// 还是该补换行）；正文必须是规范的 base64url（Strict 拒绝非零尾比特，保证 Export 能逐字符
// 还原文件）；解码结果必须正好 32 字节。任一层不符都算内容非法，由调用方转成 ErrKeyUnavailable。
// 最后一层是防御性分支：43 个 base64url 字符解出来必然是 32 字节（43×6=258 位，
// 末尾 2 位被 Strict 拒绝），通过总长门后它不可达；保留它是为了将来放宽长度门或
// 换编码方式时不至于静默接受短密钥。
func parseKey(raw []byte) (Key, error) {
	var key Key

	if len(raw) != encodedKeyLen+1 {
		return key, fmt.Errorf("长度 %d 字节，应为 %d", len(raw), encodedKeyLen+1)
	}
	if raw[len(raw)-1] != '\n' {
		return key, errors.New("缺少结尾换行")
	}

	decoded, err := base64.RawURLEncoding.Strict().DecodeString(string(raw[:encodedKeyLen]))
	if err != nil {
		return key, fmt.Errorf("解码 base64url: %w", err)
	}
	if len(decoded) != KeyLen {
		return key, fmt.Errorf("解码得到 %d 字节，应为 %d", len(decoded), KeyLen)
	}

	copy(key[:], decoded)
	return key, nil
}
