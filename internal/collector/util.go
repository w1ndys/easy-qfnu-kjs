package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// shanghaiLoc 是采集与时间戳使用的 Asia/Shanghai 时区；zoneinfo 缺失时回退 +08:00。
var shanghaiLoc = func() *time.Location {
	if loc, err := time.LoadLocation(DefaultTimezone); err == nil {
		return loc
	}
	return time.FixedZone(DefaultTimezone, 8*60*60)
}()

// nowBJ 返回当前 Asia/Shanghai 时间。
func nowBJ() time.Time { return time.Now().In(shanghaiLoc) }

// formatRFC3339BJ 输出带 +08:00 偏移的 RFC3339。
func formatRFC3339BJ(t time.Time) string { return t.In(shanghaiLoc).Format(time.RFC3339) }

// formatCompactBJ 输出快照/发布 ID 用的紧凑时间戳，如 20260903T041000+0800。
func formatCompactBJ(t time.Time) string { return t.In(shanghaiLoc).Format(SnapshotIDLayout) }

// beijingDate 返回 YYYY-MM-DD。
func beijingDate(t time.Time) string { return t.In(shanghaiLoc).Format("2006-01-02") }

// beijingWeekday 返回 1（周一）— 7（周日）。
func beijingWeekday(t time.Time) int {
	wd := t.In(shanghaiLoc).Weekday()
	if wd == time.Sunday {
		return 7
	}
	return int(wd)
}

// isBeijingSunday 是否为北京时间的周日（全量刷新日）。
func isBeijingSunday(t time.Time) bool { return beijingWeekday(t) == 7 }

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// sha256FileHex 计算文件内容哈希。
func sha256FileHex(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// writeFileAtomic 原子写文件（临时文件 + rename），并设置权限。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// marshalIndent 统一输出：两空格缩进 + 末尾换行，字段顺序由结构体保证。
func marshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// NaturalLess 自然排序（导出供测试与排序使用）：数字段按数值比较，
// 其余按忽略大小写比较，仍相同按原始字节。
func NaturalLess(a, b string) bool { return naturalLess(a, b) }

// naturalLess 见 NaturalLess。
func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		ca, cb := a[ia], b[ib]
		if isASCIIDigit(ca) && isASCIIDigit(cb) {
			na, nb := ia, ib
			for na < len(a) && isASCIIDigit(a[na]) {
				na++
			}
			for nb < len(b) && isASCIIDigit(b[nb]) {
				nb++
			}
			da, db := stripLeadingZeros(a[ia:na]), stripLeadingZeros(b[ib:nb])
			if len(da) != len(db) {
				return len(da) < len(db)
			}
			if da != db {
				return da < db
			}
			if na-ia != nb-ib {
				return na-ia < nb-ib // 前导零更少者优先
			}
			ia, ib = na, nb
			continue
		}
		if isASCIIDigit(ca) != isASCIIDigit(cb) {
			return isASCIIDigit(ca) // 数字优先于字母
		}
		la, lb := lowerByte(ca), lowerByte(cb)
		if la != lb {
			return la < lb
		}
		if ca != cb {
			return ca < cb
		}
		ia++
		ib++
	}
	return len(a)-ia < len(b)-ib
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func stripLeadingZeros(s string) string {
	i := 0
	for i < len(s)-1 && s[i] == '0' {
		i++
	}
	return s[i:]
}

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// weekKey 是 manifest.weeks 的键（无前导零）。
func weekKey(week int) string { return strconv.Itoa(week) }

// weekFileName 如 week-02.json。
func weekFileName(week int) string { return fmt.Sprintf(WeekFilePattern, week) }

// quote 截断并加引号，用于在错误信息里安全展示短文本片段。
func quoteShort(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return `""`
	}
	if len(r) > max {
		r = r[:max]
		return `"` + string(r) + `…"`
	}
	return `"` + string(r) + `"`
}
