// 本文件属于 business 层：房名规范化。
// 规则见 docs/contract/data-format.v2.md 的 normalize 层：NFKC → 反复删除容量片段 → 压缩空白。

package clean

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// capacityPattern 是房名里的容量片段：半角或全角括号里包着 n/N。
var capacityPattern = regexp.MustCompile(`[（(]\s*\d+\s*/\s*\d+\s*[)）]`)

// normalizeName 按契约规范化房名：NFKC → 反复删除容量片段 → 压缩空白并去首尾。
// 方位、校区等其它括号保留，同名不同 jsbh 不合并。
func normalizeName(raw string) string {
	name := norm.NFKC.String(raw)

	// 容量片段可能连着写好几个，删一次不够
	for capacityPattern.MatchString(name) {
		name = capacityPattern.ReplaceAllString(name, "")
	}

	// 删掉片段后可能留下多余空白，Fields 顺手把首尾空白也去掉
	return strings.Join(strings.Fields(name), " ")
}
