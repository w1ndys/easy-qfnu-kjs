package clean

import (
	"regexp"
	"strings"
)

// capacityFragment 是房名里的容量片段，覆盖上游实际出现的两种写法：
//   - 括号里的容量，如 (90/0)、（60人）、( 60 / 0 )；
//   - 不带括号的「60人」，可能出现在房名的任意位置。
//
// 全角括号、数字与斜杠会先被 foldWidth 折成 ASCII，所以这里只按半角写。
// 空白用 [\s\p{Z}]* 而不是 \s*：页面里的 &nbsp;（U+00A0）与其它 Unicode 空白
// 也会出现在片段内部，只认 ASCII 空白会漏删。
// 方位/校区括号（如（曲阜）、（日照））不含数字与「人」，不会被这条规则命中。
var capacityFragment = regexp.MustCompile(`\([\s\p{Z}]*(?:\d+[\s\p{Z}]*/[\s\p{Z}]*\d+|\d+[\s\p{Z}]*人)[\s\p{Z}]*\)|\d+[\s\p{Z}]*人`)

// RoomName 按 docs/prd.md 的房名规范化口径生成展示名（旧 spec Requirement 5.2、
// 当前 design.md 的 clean.RoomName）：
//  1. 全半角归一：U+FF01–U+FF5E 折成 ASCII，U+3000 折成普通空格；
//  2. 反复删除容量片段，直到没有可删的为止；
//  3. 压缩空白：连续空白压成一个空格并去掉首尾。
//
// 整名都是容量片段时（如 "(60/0)"、"（６０人）"、"60人"），删完就没有可展示的内容了，
// 这时回退返回原始名 raw 而不是空串：原文至少还带着编号信息，空串会让展示层与查询层
// 失去可辨识的房名。回退只认「确实删掉过容量片段且删完为空」，纯空白与空文本没有删过
// 片段，仍按第 3 步返回空串。
//
// 只删容量片段，不删其它括号：方位/校区括号（曲阜、日照）的内容原样保留，
// 只有括号本身随第 1 步变成半角。函数是纯函数且幂等：
// RoomName(RoomName(raw)) == RoomName(raw)。回退分支同样幂等：回退结果是原文，
// 再调用一次仍走同一条回退。
//
// 原始房名不在这里保留：调用方把 parser 的 MatrixRoom.NameRaw 原样存库
// （room.name_raw），本函数只产出展示名（room.name），不修改也不丢弃原文。
//
// 全半角归一按查询层 9.3 的同一做法手动折叠，不引入 golang.org/x/text/unicode/norm：
// NFKC 还会改写其它兼容字符，而房名只需要处理全角 ASCII 与表意空格；
// 这样 clean 的依赖方向仍然只有标准库 + model（见 backend/.golangci.yml）。
func RoomName(raw string) string {
	// 先按 unicode.IsSpace 归一空白，再删容量片段：正则的空白类与 strings.Fields
	// 对 \v、U+0085 的判定不同，先归一能避免「第二轮才删出片段」破坏幂等。
	name := strings.Join(strings.Fields(foldWidth(raw)), " ")
	removed := false

	// 删掉一个片段可能让两侧的字拼成新的片段（如 F12(60/0)6人 → F126人 → F），
	// 所以必须循环到不动点：一次 ReplaceAll 只能删掉当前文本里的片段，
	// 删完新露出来的那一段要下一轮才会被扫到。
	for {
		next := capacityFragment.ReplaceAllString(name, "")
		if next == name {
			break
		}
		name = next
		removed = true
	}

	name = strings.Join(strings.Fields(name), " ")

	// 整名都是容量片段：删完为空，按契约回退原文。removed 排除空文本与纯空白，
	// 它们没有删过容量片段，本来就该得到空串，不是「整名容量」的边界。
	if name == "" && removed {
		return raw
	}

	return name
}

// foldWidth 把全角 ASCII（U+FF01–U+FF5E）映射回 ASCII，并把表意空格 U+3000 映射为普通空格。
// 不做大小写转换，也不碰中文与希腊字母：状态原文里的 Κ 等符号由 State 原样匹配，
// 房名里的校区名也不能被改写。
func foldWidth(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))

	for _, r := range text {
		switch {
		case r == '\u3000':
			builder.WriteByte(' ')
		case r >= '\uFF01' && r <= '\uFF5E':
			builder.WriteRune(r - 0xFEE0)
		default:
			builder.WriteRune(r)
		}
	}

	return builder.String()
}
