package secret

// 本文件的用例都在 t.TempDir() 下操作真实文件：O_EXCL、文件权限与「44 字节」格式
// 只能落在文件系统上验证，用内存替身测不出来。

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// newManagerAt 返回指向临时目录下 secret.key 的 Manager 与文件路径。
func newManagerAt(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret.key")
	return NewManager(path), path
}

// writeRaw 用给定内容写入 path，用来模拟被截断或手改过的密钥文件。
func writeRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
}

// permOf 返回 path 的权限位。
func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取 %s 属性失败: %v", path, err)
	}
	return info.Mode().Perm()
}

// stubSeam 临时替换包内 seam 并在用例结束时恢复。
// seam 是包级变量，因此改写它的用例不得 t.Parallel；需要真实行为的替身可以先取出
// 原值再在替身里调用（例如 close 替身仍要真正关闭文件，避免泄漏描述符）。
func stubSeam[T any](t *testing.T, seam *T, stub T) {
	t.Helper()
	original := *seam
	*seam = stub
	t.Cleanup(func() { *seam = original })
}

// TestNewManagerDoesNoIO 确认构造 Manager 只记路径：不建目录、不读文件、不装载密钥。
func TestNewManagerDoesNoIO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "secret.key")
	m := NewManager(path)

	if got := m.Path(); got != path {
		t.Fatalf("Path() = %q，应为 %q", got, path)
	}
	if m.Present() {
		t.Fatal("新建的 Manager 不应报告已装载密钥")
	}
	if _, err := m.Current(); !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("未装载时 Current() 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("构造 Manager 不应建目录，实际 err=%v", err)
	}
}

// TestLoadMissingFileReturnsFalseNil 确认「未初始化」是正常状态：返回 (false, nil)，
// 而不是把缺失当成不可用错误。
func TestLoadMissingFileReturnsFalseNil(t *testing.T) {
	m, path := newManagerAt(t)

	loaded, err := m.Load()
	if err != nil {
		t.Fatalf("缺失文件应返回 nil 错误，实际 err=%v", err)
	}
	if loaded {
		t.Fatal("缺失文件时 Load 应返回 false")
	}
	if m.Present() {
		t.Fatal("缺失文件时 Present() 应为 false")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("Load 不应创建文件，实际 err=%v", statErr)
	}
}

// TestCreateWritesKeyFile0600 覆盖密钥文件的全部格式约定：0600 权限、
// 43 个 base64url 字符加一个换行（共 44 字节）、内容与 Export 一致、创建后即可读回。
func TestCreateWritesKeyFile0600(t *testing.T) {
	m, path := newManagerAt(t)

	key, err := m.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	if key == (Key{}) {
		t.Fatal("Create 不应返回全零密钥")
	}
	if got := permOf(t, path); got != 0o600 {
		t.Fatalf("密钥文件权限 = %#o，应为 0600", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if len(raw) != encodedKeyLen+1 {
		t.Fatalf("密钥文件 %d 字节，应为 %d（43 字符 + 换行）", len(raw), encodedKeyLen+1)
	}
	if raw[len(raw)-1] != '\n' {
		t.Fatalf("密钥文件应以换行结尾，实际末字节 %#x", raw[len(raw)-1])
	}
	if got := string(raw[:encodedKeyLen]); got != key.Export() {
		t.Fatalf("文件正文 %q 与 key.Export() %q 不一致", got, key.Export())
	}

	exported := key.Export()
	if len(exported) != encodedKeyLen {
		t.Fatalf("Export() 长度 = %d，应为 %d", len(exported), encodedKeyLen)
	}
	if strings.ContainsRune(exported, '=') {
		t.Fatalf("Export() 应无填充，实际 %q", exported)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(exported)
	if err != nil {
		t.Fatalf("Export() 不是合法 base64url: %v", err)
	}
	if !bytes.Equal(decoded, key[:]) {
		t.Fatal("Export() 解码后应与密钥字节一致")
	}

	if !m.Present() {
		t.Fatal("Create 之后 Present() 应为 true")
	}
	current, err := m.Current()
	if err != nil {
		t.Fatalf("Create 之后 Current() 应成功，实际 err=%v", err)
	}
	if current != key {
		t.Fatal("Current() 应返回 Create 生成的那把密钥")
	}
}

// TestCreateSecondCallReturnsErrKeyExists 覆盖二次 Create：返回 ErrKeyExists，
// 并断言文件内容与权限一概不变；换成另一个 Manager 也拒绝，说明判重靠 O_EXCL 而不是内存状态。
func TestCreateSecondCallReturnsErrKeyExists(t *testing.T) {
	m, path := newManagerAt(t)

	if _, err := m.Create(); err != nil {
		t.Fatalf("首次 Create 应成功，实际 err=%v", err)
	}
	rawBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	permBefore := permOf(t, path)

	if _, err := m.Create(); !errors.Is(err, model.ErrKeyExists) {
		t.Fatalf("同一 Manager 二次 Create 应返回 ErrKeyExists，实际 err=%v", err)
	}

	rawAfter, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Fatalf("二次 Create 不应覆盖内容：%q → %q", rawBefore, rawAfter)
	}
	if got := permOf(t, path); got != permBefore {
		t.Fatalf("二次 Create 不应改变权限：%#o → %#o", permBefore, got)
	}

	other := NewManager(path)
	if _, err := other.Create(); !errors.Is(err, model.ErrKeyExists) {
		t.Fatalf("新 Manager 对同一路径 Create 应返回 ErrKeyExists（O_EXCL），实际 err=%v", err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("目录中应只有密钥文件一个条目，实际 %d 个", len(entries))
	}
}

// TestCreateAfterLoadReturnsErrKeyExists 覆盖「内存里已有密钥」这条分支：
// 即使跳过文件检查也不该再生成第二把密钥。
func TestCreateAfterLoadReturnsErrKeyExists(t *testing.T) {
	first, path := newManagerAt(t)
	key, err := first.Create()
	if err != nil {
		t.Fatalf("首次 Create 应成功，实际 err=%v", err)
	}

	second := NewManager(path)
	loaded, err := second.Load()
	if err != nil || !loaded {
		t.Fatalf("Load 应返回 (true, nil)，实际 (%v, %v)", loaded, err)
	}
	if _, err := second.Create(); !errors.Is(err, model.ErrKeyExists) {
		t.Fatalf("已装载密钥时 Create 应返回 ErrKeyExists，实际 err=%v", err)
	}

	current, err := second.Current()
	if err != nil || current != key {
		t.Fatalf("文件内容应保持不变，实际 key=%x err=%v", current, err)
	}
}

// TestCreateMkdirAllWith0700 覆盖父目录补建：多级不存在时按 0700 建出来。
func TestCreateMkdirAllWith0700(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "var", "lib", "kjs")
	m := NewManager(filepath.Join(nested, "secret.key"))

	if _, err := m.Create(); err != nil {
		t.Fatalf("Create 应补建父目录并成功，实际 err=%v", err)
	}
	for _, dir := range []string{filepath.Join(root, "var"), filepath.Join(root, "var", "lib"), nested} {
		if got := permOf(t, dir); got != 0o700 {
			t.Fatalf("新建目录 %s 权限 = %#o，应为 0700", dir, got)
		}
	}
}

// TestCreateFailureReturnsErrKeyUnavailable 覆盖建目录失败：父路径被文件占用时
// 不该 panic，也不该冒出裸 os 错误。
func TestCreateFailureReturnsErrKeyUnavailable(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	writeRaw(t, blocker, []byte("not a directory"))

	m := NewManager(filepath.Join(blocker, "secret.key"))
	if _, err := m.Create(); !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("父路径不可用时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("Create 失败后不应装载密钥")
	}
}

// TestCreateOpenFileFailureReturnsErrKeyUnavailable 覆盖 OpenFile 的非 EEXIST 失败：
// 父目录只读（0500）时建不出文件，此时既不能报 ErrKeyExists 也不能漏出裸 os 错误。
// root 可无视目录权限，先探测一次并跳过。
func TestCreateOpenFileFailureReturnsErrKeyUnavailable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, keyDirMode); err != nil {
		t.Fatalf("建子目录失败: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("改目录权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, keyDirMode) })

	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probe)
		t.Skip("当前用户可无视 0500 目录权限（如 root），跳过建文件失败用例")
	}

	m := NewManager(filepath.Join(dir, "secret.key"))
	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("建文件失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if errors.Is(err, model.ErrKeyExists) {
		t.Fatalf("建文件失败不应被误判成 ErrKeyExists，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "创建密钥文件") {
		t.Fatalf("错误信息应指出创建密钥文件失败，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("建文件失败后不应装载密钥")
	}
}

// TestLoadReadsCreatedFile 确认 Create 写出的文件能被另一个 Manager 读回，且 Load 幂等。
func TestLoadReadsCreatedFile(t *testing.T) {
	creator, path := newManagerAt(t)
	key, err := creator.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	m := NewManager(path)
	for i := 0; i < 2; i++ {
		loaded, err := m.Load()
		if err != nil || !loaded {
			t.Fatalf("第 %d 次 Load 应返回 (true, nil)，实际 (%v, %v)", i+1, loaded, err)
		}
		current, err := m.Current()
		if err != nil {
			t.Fatalf("Load 之后 Current() 应成功，实际 err=%v", err)
		}
		if current != key {
			t.Fatalf("Load 读回的密钥与写入的不一致：%x != %x", current, key)
		}
	}
}

// TestLoadInvalidFileReturnsErrKeyUnavailable 覆盖内容非法的各条分支：
// 总长不符（空文件、截断、缺换行、31 字节密钥编码）、非法字符、非规范尾比特，
// 以及两条 45 字节的文件——它们由 Size 门在读取之前拒掉，作为「不读内容」的对照留在这里。
func TestLoadInvalidFileReturnsErrKeyUnavailable(t *testing.T) {
	creator, validPath := newManagerAt(t)
	if _, err := creator.Create(); err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	valid, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}

	// 规范编码最后一个字符只承载 4 个有效位，低 2 位必须为 0；
	// 把它的值加一就得到一个「解码后仍是 32 字节、但不是规范编码」的串。
	const urlAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	nonCanonical := append([]byte(nil), valid...)
	idx := strings.IndexByte(urlAlphabet, nonCanonical[encodedKeyLen-1])
	nonCanonical[encodedKeyLen-1] = urlAlphabet[idx+1]

	illegal := append([]byte(nil), valid...)
	illegal[0] = '!'

	cases := []struct {
		name string
		raw  []byte
		// wantErrs 为空表示不断言；非空时断言错误文案包含其中每一项。
		// notWantErr 为空表示不断言；非空时断言错误文案不包含它。
		wantErrs   []string
		notWantErr string
	}{
		{"空文件", nil, nil, ""},
		{"截断到 32 字节", valid[:32], nil, ""},
		{"截断到 43 字节（同时缺换行）", valid[:encodedKeyLen], nil, ""},
		{"少一个字符", valid[:encodedKeyLen-1], nil, ""},
		{"非法字符", illegal, nil, ""},
		{"非规范尾比特", nonCanonical, nil, ""},
		// 31 字节密钥编码后 42 字符 + 换行 = 43 字节，落在 parseKey 的「总长必须正好 44 字节」
		// 这道门上；32 字节以外的密钥编码后总长必然不是 44，所以 parseKey 里「解码得到 != 32
		// 字节」那条分支不可达，只作防御保留（见 parseKey 注释）。
		{"总长 43 字节（31 字节密钥编码）", []byte(base64.RawURLEncoding.EncodeToString(make([]byte, KeyLen-1)) + "\n"), nil, ""},
		// 下面两条都是 45 字节，由 Size 门（size > 44）在读取之前拒掉：错误里写「超过 44 字节」，
		// 且不能出现「解析密钥文件」——出现它就说明内容已经被读出来交给 parseKey 了。
		// 「33 字节密钥编码」测的是生成端出错，「44 字节后再多一个换行」测的是运维手改文件时
		// 最容易犯的错；两者字节数相同但内容不同，各留一条，免得后者被前者盖住。
		{"总长 45 字节（33 字节密钥编码）", []byte(base64.RawURLEncoding.EncodeToString(make([]byte, KeyLen+1)) + "\n"), []string{"超过 44 字节"}, "解析密钥文件"},
		{"44 字节之后多一个换行", append(append([]byte(nil), valid...), '\n'), []string{"超过 44 字节"}, "解析密钥文件"},
		// 44 字节、长度门已过、只是末字节不是换行：必须报「缺少结尾换行」，而不是回落到长度文案。
		// 错误里带「解析密钥文件」是另一层证明：它确实走到了 parseKey，而不是在别处被拒。
		{"44 字节但末字节不是换行", append(append([]byte(nil), valid[:encodedKeyLen]...), 'A'), []string{"解析密钥文件", "缺少结尾换行"}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.key")
			writeRaw(t, path, tc.raw)

			m := NewManager(path)
			loaded, err := m.Load()
			if loaded {
				t.Fatal("内容非法时 Load 应返回 false")
			}
			if !errors.Is(err, model.ErrKeyUnavailable) {
				t.Fatalf("内容非法时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误信息应包含 %q，实际 err=%v", want, err)
				}
			}
			if tc.notWantErr != "" && strings.Contains(err.Error(), tc.notWantErr) {
				t.Fatalf("错误信息不应包含 %q，实际 err=%v", tc.notWantErr, err)
			}
			if m.Present() {
				t.Fatal("内容非法时 Present() 应为 false")
			}
		})
	}
}

// TestLoadRejectsOversizedFile 覆盖「路径误指大文件」：内容远超 44 字节时必须在读取前就拒掉，
// 不能把整份文件先读进内存再报长度不符。这里用 Truncate 造一个 8 MiB 的稀疏文件（内容全零、
// 不实际占盘），断言错误写的是文件真实大小，且错误里没有「解析密钥文件」——出现它就说明
// 内容已经被读出来交给 parseKey 了，而这条路径本该在 Stat 之后直接返回。
// 读取上限与「先看 Size」是同一道意图的两层：Size 挡住这里能构造的静态大文件，LimitReader
// 兜住 Stat 之后被追加的情形；后者要靠并发追加才可观测，不在这条用例里构造。
func TestLoadRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")

	const size = 8 << 20
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("创建超大测试文件失败: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("把测试文件撑到 %d 字节失败: %v", size, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭超大测试文件失败: %v", err)
	}

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("超大文件时 Load 应返回 false")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("超大文件时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("内容 %d 字节", size)) {
		t.Fatalf("错误信息应带上文件真实大小 %d 字节，实际 err=%v", size, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("超过 %d 字节", encodedKeyLen+1)) {
		t.Fatalf("错误信息应指出超过 %d 字节上限，实际 err=%v", encodedKeyLen+1, err)
	}
	if strings.Contains(err.Error(), "解析密钥文件") {
		t.Fatalf("超大文件应在读取前拒绝，不应读到内容（错误里出现「解析密钥文件」），实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("超大文件不应装载密钥")
	}
}

// TestLoadFileAppendedAfterStatRejectedByLengthGate 覆盖「Stat 之后文件被追加」：Size 门看到
// 44 字节放行，随后的读取却拿到更长内容。用 readAll seam 在真正读取之前把文件追加到 60 字节，
// 于是 LimitReader 的 45 字节上限成为实际读取量——解析层收到的既不是 Stat 时的 44 字节，
// 也不是追加后的 60 字节，而是 45 字节，由 parseKey 的长度门拒掉。断言里不含 Size 门的
// 「超过」是这条用例的关键：它证明 Size 门当时确实只看到 44 字节，拒掉内容的是读取之后的
// 长度门；若 LimitReader 缺席，这里读到的会是 60 字节，长度断言随之失败。
func TestLoadFileAppendedAfterStatRejectedByLengthGate(t *testing.T) {
	creator, path := newManagerAt(t)
	if _, err := creator.Create(); err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	originalReadAll := readAll
	stubSeam(t, &readAll, func(r io.Reader) ([]byte, error) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.WriteString(strings.Repeat("X", 16)); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
		return originalReadAll(r)
	})

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("Stat 之后被追加的文件不应装载成功")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if want := fmt.Sprintf("长度 %d 字节，应为 %d", encodedKeyLen+2, encodedKeyLen+1); !strings.Contains(err.Error(), want) {
		t.Fatalf("应由 parseKey 的长度门报 %q，实际 err=%v", want, err)
	}
	if strings.Contains(err.Error(), "超过") {
		t.Fatalf("Size 门看到的是 44 字节，错误里不应出现「超过」，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("应不装载密钥")
	}
}

// TestLoadReadAllFailureReturnsErrKeyUnavailable 覆盖读取本身报错：文件类型、权限与 Size 都合规，
// 真正调 readAll 时才失败（磁盘错误、设备掉线这类故障在本机文件系统上造不出来，只能走 seam）。
// 断言文案是「读取密钥文件」而不是「解析密钥文件」：后者会把运维引向检查文件内容，而问题出在
// 根本读不出来；底层原因要能出现在错误串里，ErrKeyUnavailable 与 Present 保持 false 也不能少。
func TestLoadReadAllFailureReturnsErrKeyUnavailable(t *testing.T) {
	creator, path := newManagerAt(t)
	if _, err := creator.Create(); err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	stubSeam(t, &readAll, func(io.Reader) ([]byte, error) {
		return nil, errors.New("模拟读取失败")
	})

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("读取失败时 Load 应返回 false")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("读取失败时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "读取密钥文件") {
		t.Fatalf("错误信息应指出读取密钥文件失败，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "模拟读取失败") {
		t.Fatalf("错误信息应保留底层原因，实际 err=%v", err)
	}
	if strings.Contains(err.Error(), "解析密钥文件") {
		t.Fatalf("读取就失败时不应走到解析步骤，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("读取失败后不应装载密钥")
	}
}

// TestLoadOversizedOpenPermRejectedByPermFirst 覆盖「文件又大权限又过宽」：判定顺序是
// 类型 → 权限 → Size，所以 0644 的大文件由权限分支先拒，错误里带的是实际权限位，而不是
// 真实字节数。这条用例把「路径误指日志/镜像时错误写真实大小」的适用边界钉住：那个文案只在
// 权限与类型都合规时才出现（日志、镜像多是 0644，实际先被权限拦下）。
func TestLoadOversizedOpenPermRejectedByPermFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")

	const size = 8 << 20
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("创建超大测试文件失败: %v", err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		t.Fatalf("把测试文件撑到 %d 字节失败: %v", size, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭超大测试文件失败: %v", err)
	}
	const perm = os.FileMode(0o644)
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("权限过宽的大文件时 Load 应返回 false")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("权限过宽的大文件时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "不得允许属主之外访问") {
		t.Fatalf("错误信息应指出权限过宽，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("权限 %#o", perm)) {
		t.Fatalf("错误信息应带上实际权限 %#o，实际 err=%v", perm, err)
	}
	// 权限分支在 Size 门之前返回，所以错误里不该出现真实大小，也不该出现读取之后的解析文案。
	if strings.Contains(err.Error(), fmt.Sprintf("%d 字节", size)) {
		t.Fatalf("权限先拒时不应写文件真实大小，实际 err=%v", err)
	}
	if strings.Contains(err.Error(), "超过") || strings.Contains(err.Error(), "解析密钥文件") {
		t.Fatalf("权限先拒时不应走到 Size 门或读取步骤，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("权限过宽时不应装载密钥")
	}
}

// TestLoadUnreadableFileReturnsErrKeyUnavailable 覆盖权限不可读：不是「缺失」，
// 因此不能返回 (false, nil)。
func TestLoadUnreadableFileReturnsErrKeyUnavailable(t *testing.T) {
	creator, path := newManagerAt(t)
	if _, err := creator.Create(); err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := os.ReadFile(path); err == nil {
		t.Skip("当前用户可无视 0000 权限（如 root），跳过不可读用例")
	}

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("不可读时 Load 应返回 false")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("不可读时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
}

// TestLoadOwnerUnreadableFileCauseIsErrPermission 确认 Load 的错误不只是「包了一层
// ErrKeyUnavailable」：底层 cause 仍能被 errors.Is 取出来。样本取 0070——属主自己没有读位、
// 组里却有，非 root 下 open(2) 先返回 EACCES，于是 errors.Is(err, fs.ErrPermission) 应命中；
// root 能无视权限位，open 反而成功并落到权限分支（那条分支没有 permission cause），
// 所以先探测一次、root 下跳过。表驱动用例里的 0070 只断言 ErrKeyUnavailable，把这条单独放，
// 是为了让「cause 保真」这一约定有名字、可单独定位，而不是夹在权限矩阵的 else 分支里。
func TestLoadOwnerUnreadableFileCauseIsErrPermission(t *testing.T) {
	creator, path := newManagerAt(t)
	if _, err := creator.Create(); err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	if err := os.Chmod(path, 0o070); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := os.ReadFile(path); err == nil {
		t.Skip("当前用户可无视 0070 权限（如 root），跳过权限 cause 用例")
	}

	m := NewManager(path)
	loaded, err := m.Load()
	if loaded {
		t.Fatal("属主不可读时 Load 应返回 false")
	}
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("属主不可读时 Load 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Load 应保留底层权限错误（errors.Is(err, fs.ErrPermission) 成立），实际 err=%v", err)
	}
}

// TestLoadPermissionMatrix 用表驱动覆盖权限判定：判定口径是「不得允许属主之外访问」
// （perm&0o077 == 0），所以 0400/0500/0600/0700 都算合规——运维自己把文件收紧成只读
// 不会让启动失败；而 0640/0604/0070/0644 一律硬失败。其中属主自己仍可读的三条由权限
// 分支拒绝，错误里带实际权限而不是固定 0600；0070 在非 root 下由 open(2) 的 EACCES 先拦，
// 只保证 ErrKeyUnavailable。
func TestLoadPermissionMatrix(t *testing.T) {
	cases := []struct {
		name string
		perm os.FileMode
		ok   bool
		// wantErr 为空表示只断言 ErrKeyUnavailable；非空时再断言错误文案包含它。
		// 0070 这种「属主自己都读不了」的权限，非 root 下 open(2) 先返回 EACCES，
		// 根本走不到权限分支，所以不能要求「不得允许属主之外访问」这句文案。
		wantErr string
	}{
		{"0400 属主只读", 0o400, true, ""},
		{"0500 属主读执行", 0o500, true, ""},
		{"0600 属主读写（Create 写出的权限）", 0o600, true, ""},
		{"0700 属主全权", 0o700, true, ""},
		{"0640 组可读", 0o640, false, "不得允许属主之外访问"},
		{"0604 其他用户可读", 0o604, false, "不得允许属主之外访问"},
		{"0070 组全权", 0o070, false, ""},
		{"0644 其他用户可读", 0o644, false, "不得允许属主之外访问"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creator, path := newManagerAt(t)
			key, err := creator.Create()
			if err != nil {
				t.Fatalf("Create 应成功，实际 err=%v", err)
			}
			if err := os.Chmod(path, tc.perm); err != nil {
				t.Fatalf("改权限失败: %v", err)
			}

			m := NewManager(path)
			loaded, err := m.Load()
			if tc.ok {
				if err != nil || !loaded {
					t.Fatalf("权限 %#o 应可装载，实际 (%v, %v)", tc.perm, loaded, err)
				}
				if current, err := m.Current(); err != nil || current != key {
					t.Fatalf("权限 %#o 装载的密钥应与写入的一致，实际 key=%x err=%v", tc.perm, current, err)
				}
				return
			}
			if loaded {
				t.Fatalf("权限 %#o 不该装载成功", tc.perm)
			}
			if !errors.Is(err, model.ErrKeyUnavailable) {
				t.Fatalf("权限 %#o 时 Load 应返回 ErrKeyUnavailable，实际 err=%v", tc.perm, err)
			}
			if m.Present() {
				t.Fatal("权限不合规时不应装载密钥")
			}
			if tc.wantErr == "" {
				// 0070 属主自己都没有读权限：非 root 下 open(2) 先返回 EACCES，走不到权限
				// 分支，错误串里不会有权限位；root 下则落到权限分支。两条路径都只保证
				// ErrKeyUnavailable，所以这里不再断言文案。
				return
			}
			// 剩下三条属主自己仍可读，open 能成功，必然由权限分支拒绝；断言完整片段
			// 「权限 0644」而不是裸数字，避免被 t.TempDir() 生成的目录名误命中。
			if !strings.Contains(err.Error(), fmt.Sprintf("权限 %#o", tc.perm)) {
				t.Fatalf("错误信息应带上实际权限 %#o，实际 err=%v", tc.perm, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误信息应包含 %q，实际 err=%v", tc.wantErr, err)
			}
			// 判定口径不是固定 0600；文案写死「应为 0600」会把运维引向错误的 chmod。
			if strings.Contains(err.Error(), "应为") {
				t.Fatalf("错误信息不应给出固定的「应为 XXX」权限，实际 err=%v", err)
			}
		})
	}
}

// TestLoadRejectsNonRegularFile 覆盖非普通文件：路径指向目录或字符设备时不能把它当密钥
// 文件读，错误文案还要写中文类型名（目录/字符设备）——只渲染 Mode().Type() 的 d/D 会让
// 运维不知道是路径指错了目录还是设备。这里用 /dev/null 当字符设备样本（不存在或不是字符
// 设备就跳过），设备文件读出来的也不是密钥；FIFO 不在用例里，因为 open(2) 对没有写入端的
// FIFO 本身就会阻塞，用 mkfifo 构造会把测试挂死——IsRegular 挡住的是「打开之后去读」的一半。
func TestLoadRejectsNonRegularFile(t *testing.T) {
	cases := []struct {
		name     string
		wantType string
		path     func(t *testing.T) string
	}{
		{"目录", "目录", func(t *testing.T) string {
			t.Helper()
			return t.TempDir()
		}},
		{"字符设备 /dev/null", "字符设备", func(t *testing.T) string {
			t.Helper()
			info, err := os.Stat("/dev/null")
			if err != nil {
				t.Skipf("/dev/null 不可用，跳过字符设备用例: %v", err)
			}
			if info.Mode()&os.ModeCharDevice == 0 {
				t.Skipf("/dev/null 不是字符设备（%s），跳过", info.Mode().Type())
			}
			return "/dev/null"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			m := NewManager(path)

			loaded, err := m.Load()
			if loaded {
				t.Fatalf("%s 时 Load 应返回 false", tc.name)
			}
			if !errors.Is(err, model.ErrKeyUnavailable) {
				t.Fatalf("%s 时 Load 应返回 ErrKeyUnavailable，实际 err=%v", tc.name, err)
			}
			if !strings.Contains(err.Error(), "不是普通文件") {
				t.Fatalf("错误信息应指出不是普通文件，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), tc.wantType) {
				t.Fatalf("错误信息应写中文类型名 %q，实际 err=%v", tc.wantType, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("错误信息应带上路径 %s，实际 err=%v", path, err)
			}
			if m.Present() {
				t.Fatal("非普通文件不应装载密钥")
			}
		})
	}
}

// TestDescribeFileType 覆盖非普通文件到中文类型名的映射：Load 的错误文案靠它，
// 而 FIFO、块设备、套接字在本机测试里不好构造（对没有写入端的 FIFO open(2) 会阻塞），
// 所以直接喂 FileMode 验证。字符设备同时带着 ModeDevice 位，这一条同时锁住判定顺序：
// 先判 ModeCharDevice 才不会把 /dev/null 写成「块设备」。
func TestDescribeFileType(t *testing.T) {
	cases := []struct {
		mode fs.FileMode
		want string
	}{
		{fs.ModeDir, "目录"},
		{fs.ModeSymlink, "符号链接"},
		{fs.ModeDevice | fs.ModeCharDevice, "字符设备"},
		{fs.ModeDevice, "块设备"},
		{fs.ModeNamedPipe, "命名管道"},
		{fs.ModeSocket, "套接字"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := describeFileType(tc.mode); got != tc.want {
				t.Fatalf("describeFileType(%s) = %q，应为 %q", tc.mode.Type(), got, tc.want)
			}
		})
	}
}

// TestCreateCleansUpAfterWriteStringFailure 覆盖写入失败：WriteString 就报错时（文件内容
// 根本没写全）Create 返回 ErrKeyUnavailable，并删掉刚创建的半成品文件，Present 保持 false。
func TestCreateCleansUpAfterWriteStringFailure(t *testing.T) {
	m, path := newManagerAt(t)

	stubSeam(t, &writeFile, func(*os.File, string) (int, error) {
		return 0, errors.New("模拟写入失败")
	})

	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("写入失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "写入密钥文件") {
		t.Fatalf("错误信息应指出写入密钥文件失败，实际 err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("写失败后应删掉半成品文件，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("写失败后不应装载密钥")
	}
}

// TestCreateCleansUpAfterFileSyncFailure 覆盖文件落盘失败：Sync 失败（写入路径最后一步，
// 内容是写进去了但持久化结果未知）时 Create 返回 ErrKeyUnavailable，并删掉半成品文件，
// Present 保持 false。
func TestCreateCleansUpAfterFileSyncFailure(t *testing.T) {
	m, path := newManagerAt(t)

	stubSeam(t, &syncFile, func(*os.File) error { return errors.New("模拟落盘失败") })

	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("落盘失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "同步密钥文件") {
		t.Fatalf("错误信息应指出同步密钥文件失败，实际 err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("写失败后应删掉半成品文件，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("写失败后不应装载密钥")
	}
}

// TestCreateCleansUpAfterDirSyncFailure 覆盖父目录 fsync 失败：文件内容已落盘但目录项
// 未必持久化，Create 仍算失败并清掉文件。Sync 替身让第一次（文件）走真实 Sync，
// 第 2 次（目录）与第 3 次（删除后的补刷）各自返回不同文本，因此同一条错误串里能分别
// 找到两条分支的文案与底层原因——证明 errors.Join 确实把「清理前的目录同步失败」与
// 「删除后的目录同步失败」都带了出来，而不是只留了其中一条。
func TestCreateCleansUpAfterDirSyncFailure(t *testing.T) {
	m, path := newManagerAt(t)

	originalSync := syncFile
	calls := 0
	stubSeam(t, &syncFile, func(f *os.File) error {
		calls++
		switch calls {
		case 1:
			return originalSync(f)
		case 2:
			return errors.New("模拟目录同步失败")
		default:
			return errors.New("模拟删除后目录同步失败")
		}
	})

	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("目录同步失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if calls != 3 {
		t.Fatalf("Sync 应调用 3 次（文件、目录、删除后补刷），实际 %d 次", calls)
	}
	for _, want := range []string{
		"同步密钥目录",
		"模拟目录同步失败",
		"删除后同步密钥目录",
		"模拟删除后目录同步失败",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际 err=%v", want, err)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("目录同步失败后应删掉密钥文件，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("目录同步失败后不应装载密钥")
	}
}

// TestCreateCleansUpAfterCloseFailure 覆盖关闭失败：内容已写入但关闭上报错误时同样算失败，
// 删掉文件并返回 ErrKeyUnavailable；替身仍调用真实 Close，避免测试泄漏文件描述符。
func TestCreateCleansUpAfterCloseFailure(t *testing.T) {
	m, path := newManagerAt(t)

	originalClose := closeFile
	stubSeam(t, &closeFile, func(f *os.File) error {
		return errors.Join(originalClose(f), errors.New("模拟关闭失败"))
	})

	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("关闭失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("关闭失败后应删掉密钥文件，实际 err=%v", err)
	}
	if m.Present() {
		t.Fatal("关闭失败后不应装载密钥")
	}
}

// TestCreateReportsCleanupFailure 覆盖清理本身失败：删除错误不能像旧实现那样被吞掉，
// 必须与触发清理的错误一起返回，上层才能记 ERROR 并提示运维手工清理残留的半成品文件。
func TestCreateReportsCleanupFailure(t *testing.T) {
	m, path := newManagerAt(t)

	stubSeam(t, &syncFile, func(*os.File) error { return errors.New("模拟落盘失败") })
	stubSeam(t, &removeFile, func(string) error { return errors.New("模拟删除失败") })

	_, err := m.Create()
	if !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("清理失败时 Create 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}
	for _, want := range []string{"同步密钥文件", "删除半成品密钥文件", "模拟删除失败"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际 err=%v", want, err)
		}
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("删除失败时半成品文件应残留，实际 err=%v", statErr)
	}
	if m.Present() {
		t.Fatal("清理失败后不应装载密钥")
	}
}

// TestLoadFailureKeepsLoadedKey 确认 Load 失败不清空已经装载的密钥：
// 启动期降级只影响尚未装载的情形（CP14）。
func TestLoadFailureKeepsLoadedKey(t *testing.T) {
	m, path := newManagerAt(t)
	key, err := m.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	writeRaw(t, path, []byte("被改坏的内容"))

	if loaded, err := m.Load(); loaded || !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("Load 应返回 (false, ErrKeyUnavailable)，实际 (%v, %v)", loaded, err)
	}
	current, err := m.Current()
	if err != nil {
		t.Fatalf("已装载的密钥应仍可读，实际 err=%v", err)
	}
	if current != key {
		t.Fatalf("已装载的密钥不应被改动：%x != %x", current, key)
	}
}

// TestReloadDropsKeyAfterFileRemoved 覆盖同进程重新初始化的前置条件：Create 之后删掉密钥文件，
// Reload 必须清掉内存副本并返回 (false, nil)，让 Present 与 Current 都回到未初始化状态。
// Load 不具备这个能力：它读不到文件时只返回 (false, nil)，内存里的旧密钥仍然可用。
func TestReloadDropsKeyAfterFileRemoved(t *testing.T) {
	m, path := newManagerAt(t)
	key, err := m.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("删除密钥文件失败: %v", err)
	}

	loaded, err := m.Reload()
	if err != nil {
		t.Fatalf("文件已删时 Reload 应返回 nil 错误，实际 err=%v", err)
	}
	if loaded {
		t.Fatal("文件已删时 Reload 应返回 false")
	}
	if m.Present() {
		t.Fatal("Reload 之后 Present() 应为 false，否则同进程无法重新初始化")
	}
	if _, err := m.Current(); !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("Reload 之后 Current() 应返回 ErrKeyUnavailable，实际 err=%v", err)
	}

	// 清空之后 Create 可以重新写文件并装载新密钥。
	again, err := m.Create()
	if err != nil {
		t.Fatalf("Reload 之后 Create 应成功，实际 err=%v", err)
	}
	if again == key {
		t.Fatal("重新 Create 应生成不同的密钥")
	}
}

// TestReloadLoadsExistingFile 确认 Reload 也能把磁盘上的密钥装进一个内存为空的管理器：
// 只有文件、没有内存副本时返回 (true, nil)，且 Current 与文件内容一致。
func TestReloadLoadsExistingFile(t *testing.T) {
	writer, path := newManagerAt(t)
	key, err := writer.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	m := NewManager(path)
	loaded, err := m.Reload()
	if err != nil || !loaded {
		t.Fatalf("文件存在时 Reload 应返回 (true, nil)，实际 (%v, %v)", loaded, err)
	}
	current, err := m.Current()
	if err != nil {
		t.Fatalf("Reload 之后 Current() 应成功，实际 err=%v", err)
	}
	if current != key {
		t.Fatalf("Reload 装回的密钥应与文件一致：%x != %x", current, key)
	}
	if got, want := current.Export(), key.Export(); got != want {
		t.Fatalf("Reload 装回的密钥导出串 %q 应为 %q", got, want)
	}
}

// TestReloadInvalidFileKeepsLoadedKey 钉住 Reload 的失败口径：文件被改坏时返回
// (false, ErrKeyUnavailable)，但保留内存里那把旧密钥（与 Load 的启动期降级同一口径，CP14）。
// 清空内存只发生在文件确实不存在时（见 TestReloadDropsKeyAfterFileRemoved）——磁盘这份
// 读不出来不代表进程里已经拿到的那把失效，清掉它只会让还能解密的进程当场失能。
func TestReloadInvalidFileKeepsLoadedKey(t *testing.T) {
	m, path := newManagerAt(t)
	key, err := m.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}

	writeRaw(t, path, []byte("被改坏的内容"))

	loaded, err := m.Reload()
	if loaded || !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("Reload 应返回 (false, ErrKeyUnavailable)，实际 (%v, %v)", loaded, err)
	}
	if !m.Present() {
		t.Fatal("Reload 失败后应保留旧密钥副本")
	}
	current, err := m.Current()
	if err != nil {
		t.Fatalf("Reload 失败后已装载的密钥应仍可读，实际 err=%v", err)
	}
	if current != key {
		t.Fatalf("Reload 失败不应改动内存里的密钥：%x != %x", current, key)
	}
}

// TestReloadUnreadableFileKeepsLoadedKey 覆盖「不可读」与「不存在」的分界：文件权限被收走后
// Reload 返回 ErrKeyUnavailable 且保留旧键，不能当成「文件已删」把内存清掉。
// 0000 权限对 root 无效，先探测一次、root 下跳过。
func TestReloadUnreadableFileKeepsLoadedKey(t *testing.T) {
	m, path := newManagerAt(t)
	key, err := m.Create()
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := os.ReadFile(path); err == nil {
		t.Skip("当前用户可无视 0000 权限（如 root），跳过不可读用例")
	}

	loaded, err := m.Reload()
	if loaded || !errors.Is(err, model.ErrKeyUnavailable) {
		t.Fatalf("不可读时 Reload 应返回 (false, ErrKeyUnavailable)，实际 (%v, %v)", loaded, err)
	}
	if !m.Present() {
		t.Fatal("Reload 失败后应保留旧密钥副本")
	}
	if current, err := m.Current(); err != nil || current != key {
		t.Fatalf("不可读时 Reload 不应改动内存密钥：key=%v err=%v", current, err)
	}
}

// TestLoadCreateConcurrentKeepsKeyConsistent 覆盖 Load 与 Create 的互斥，且不靠时序碰运气：
// 用 syncFile seam 让 Create 在持有写锁时停住（它已写好文件、正等落盘），此时启动 Load，
// 断言 Load 在 Create 释放锁之前不返回；放行后 Load 必须返回 (true, nil)，且内存密钥、
// Create 返回值与文件内容三者一致。-race 下同时验证没有数据竞争。
func TestLoadCreateConcurrentKeepsKeyConsistent(t *testing.T) {
	m, path := newManagerAt(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }

	originalSync := syncFile
	stubSeam(t, &syncFile, func(f *os.File) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return originalSync(f)
	})

	created := make(chan Key, 1)
	createErr := make(chan error, 1)
	createDone := make(chan struct{})

	// 清理顺序必须是「先放行 Create、等它的 goroutine 退出，再恢复 seam」。t.Cleanup 后进先出，
	// 这条在 stubSeam 的恢复之后注册，因此先执行：否则 seam 已经还原，而替身里的 goroutine
	// 还卡在 <-release 上，用例结束后它仍在跑，-race 下也不干净。
	t.Cleanup(func() {
		releaseAll()
		select {
		case <-createDone:
		case <-time.After(5 * time.Second):
		}
	})

	go func() {
		defer close(createDone)
		key, err := m.Create()
		created <- key
		createErr <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Create 未进入文件 Sync，无法构造互斥场景")
	}

	loaded := make(chan struct{})
	var loadOK bool
	var loadErr error
	go func() {
		loadOK, loadErr = m.Load()
		close(loaded)
	}()

	select {
	case <-loaded:
		t.Fatal("Create 仍持有写锁时 Load 不应返回（否则会读到写了一半的文件）")
	case <-time.After(50 * time.Millisecond):
	}

	releaseAll()

	key, err := <-created, <-createErr
	if err != nil {
		t.Fatalf("Create 应成功，实际 err=%v", err)
	}
	<-loaded
	if !loadOK || loadErr != nil {
		t.Fatalf("Create 释放锁后 Load 应返回 (true, nil)，实际 (%v, %v)", loadOK, loadErr)
	}
	current, err := m.Current()
	if err != nil {
		t.Fatalf("Create 之后 Current() 应成功，实际 err=%v", err)
	}
	if current != key {
		t.Fatalf("内存密钥应与 Create 返回值一致：%x != %x", current, key)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	if got, want := string(raw), key.Export()+"\n"; got != want {
		t.Fatalf("文件内容 %q 应与内存密钥 %q 一致", got, want)
	}
}

// TestKeyExport 确认导出串是 43 个字符、无填充、能解回原密钥字节。
func TestKeyExport(t *testing.T) {
	var key Key
	for i := range key {
		key[i] = byte(i + 1)
	}

	exported := key.Export()
	if len(exported) != encodedKeyLen {
		t.Fatalf("Export() 长度 = %d，应为 %d", len(exported), encodedKeyLen)
	}
	if strings.ContainsRune(exported, '=') {
		t.Fatalf("Export() 不应含填充符，实际 %q", exported)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(exported)
	if err != nil {
		t.Fatalf("Export() 不是合法 base64url: %v", err)
	}
	if !bytes.Equal(decoded, key[:]) {
		t.Fatalf("Export() 解码结果与密钥不一致：%x != %x", decoded, key[:])
	}
}

// TestKeyFingerprint 覆盖指纹的四个约定：16 位、小写十六进制、等于 sha256 前 8 字节、
// 随密钥变化。
func TestKeyFingerprint(t *testing.T) {
	var key Key
	for i := range key {
		key[i] = byte(i)
	}

	fingerprint := key.Fingerprint()
	if len(fingerprint) != 16 {
		t.Fatalf("Fingerprint() 长度 = %d，应为 16", len(fingerprint))
	}
	if strings.ToLower(fingerprint) != fingerprint {
		t.Fatalf("Fingerprint() 应全为小写十六进制，实际 %q", fingerprint)
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		t.Fatalf("Fingerprint() 不是合法十六进制: %v", err)
	}
	sum := sha256.Sum256(key[:])
	if want := hex.EncodeToString(sum[:8]); fingerprint != want {
		t.Fatalf("Fingerprint() = %q，应为 sha256 前 8 字节 %q", fingerprint, want)
	}
	if again := key.Fingerprint(); again != fingerprint {
		t.Fatalf("同一把密钥的指纹应恒定：%q != %q", again, fingerprint)
	}

	other := key
	other[KeyLen-1] ^= 0x01
	if other.Fingerprint() == fingerprint {
		t.Fatal("不同密钥应产出不同指纹")
	}
}
