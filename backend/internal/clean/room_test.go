package clean

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"unicode"
)

// propertySeed 固定属性测试的随机种子：结果可复现，失败时能按同一种子重跑，
// 不让门禁的绿红取决于当次 time.Now() 抽到哪批样本。
const propertySeed = 20261103

// propertyConfig 返回带固定种子的 quick 配置，room 与 nodes 的属性测试共用。
func propertyConfig() *quick.Config {
	return &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(propertySeed))}
}

// TestRoomNameNormalizesWidthCapacityAndWhitespace 覆盖旧 spec Requirement 5.2
// 与当前 design.md 的三步口径：全半角归一 → 反复删容量片段 → 压缩空白。
// 容量片段放在开头、中间、结尾、连续出现、带内部空白、用全角数字写，都必须删干净。
func TestRoomNameNormalizesWidthCapacityAndWhitespace(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "半角容量括号", raw: "F126(90/0)", want: "F126"},
		{name: "全角字母数字与容量括号", raw: "Ｆ１２６（９０／０）", want: "F126"},
		{name: "容量片段在中间", raw: "综合教学楼(60/0)101", want: "综合教学楼101"},
		{name: "容量片段在开头", raw: "(60人)综合教学楼101", want: "综合教学楼101"},
		{name: "容量片段在结尾", raw: "综合教学楼101 60人", want: "综合教学楼101"},
		{name: "容量片段在结尾并留下空格", raw: "综合教学楼 101 (60/0)", want: "综合教学楼 101"},
		{name: "反复删除多个容量片段", raw: "F126(60人)(90/0) 60人", want: "F126"},
		{name: "中文容量带空格", raw: "阶梯教室 60 人", want: "阶梯教室"},
		{name: "括号内数字与人文带空格", raw: "阶梯教室（60 人）", want: "阶梯教室"},
		{name: "容量括号内留空白", raw: "阶梯教室( 90 / 0 )", want: "阶梯教室"},
		{name: "全角空格分隔容量", raw: "阶梯教室　（６０人）", want: "阶梯教室"},
		{name: "不换行空格分隔括号容量", raw: "阶梯教室\u00a0（60\u00a0人）", want: "阶梯教室"},
		{name: "不换行空格分隔裸容量", raw: "阶梯教室\u00a060\u00a0人", want: "阶梯教室"},
		{name: "压缩连续空白", raw: "  综合   教学楼\t101  ", want: "综合 教学楼 101"},
		{name: "全角字母转半角", raw: "Ａ１０１", want: "A101"},
		{name: "全角标点转半角", raw: "１０１～１０３", want: "101~103"},
		{name: "普通房间号不变", raw: "1号教学楼101", want: "1号教学楼101"},
		{name: "纯数字没有容量单位", raw: "101", want: "101"},
		// 边界：删掉 (60/0) 后，两侧的 6 与 人 拼出新片段，必须再删一轮。
		{name: "删片段后拼出新片段", raw: "F126(60/0)人", want: "F"},
		{name: "空文本", raw: "", want: ""},
		{name: "纯空白", raw: " \t　\n ", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RoomName(tc.raw)
			if got != tc.want {
				t.Fatalf("规范化结果不符，期望 %q，实际 %q（原文 %q）", tc.want, got, tc.raw)
			}
			// 每个分支顺手验一次幂等：第二次规范化不能继续变化。
			if second := RoomName(got); second != got {
				t.Fatalf("规范化必须幂等，第一次 %q，第二次 %q（原文 %q）", got, second, tc.raw)
			}
		})
	}
}

// TestRoomNameFallsBackToRawWhenWholeNameIsCapacity 锁住用户拍板的口径：
// 整名都是容量片段时返回原文而不是空串，别让展示层拿到空房名。
// 纯空白与空文本没有删过容量片段，不属于这一类，仍返回空串（上面的表已覆盖）。
func TestRoomNameFallsBackToRawWhenWholeNameIsCapacity(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "只有容量括号", raw: "(60/0)"},
		{name: "全角容量括号", raw: "（６０人）"},
		{name: "只剩裸容量", raw: "60人"},
		{name: "连续容量片段", raw: "(60人)(90/0)"},
		{name: "容量片段夹空白", raw: " (60/0) "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RoomName(tc.raw)
			if got != tc.raw {
				t.Fatalf("整名容量应回退原文，期望 %q，实际 %q", tc.raw, got)
			}
			if got == "" {
				t.Fatalf("整名容量不能返回空串（原文 %q）", tc.raw)
			}
			if second := RoomName(got); second != got {
				t.Fatalf("回退分支必须幂等，第一次 %q，第二次 %q", got, second)
			}
		})
	}
}

// TestRoomNameKeepsOrientationBrackets 锁住「保留方位/校区括号」（Requirement 5.2）：
// 只删容量片段，括号里的 曲阜/日照/东 等内容不能被当成容量删掉；
// 括号本身按第 1 步全半角归一变成半角，这是宽度归一的结果，不是删除。
func TestRoomNameKeepsOrientationBrackets(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "方位括号在容量前", raw: "篮球馆（东）(25/0)", want: "篮球馆(东)"},
		{name: "校区括号单独存在", raw: "综合教学楼（曲阜）", want: "综合教学楼(曲阜)"},
		{name: "校区括号后跟容量", raw: "综合教学楼（日照）（60人）", want: "综合教学楼(日照)"},
		{name: "两个校区括号夹容量", raw: "教学楼（曲阜） 60人 （日照）", want: "教学楼(曲阜) (日照)"},
		{name: "两个校区括号紧夹容量", raw: "教学楼（曲阜）60人（日照）", want: "教学楼(曲阜)(日照)"},
		{name: "方位括号打头", raw: "（曲阜）东区101(90/0)", want: "(曲阜)东区101"},
		{name: "容量打头后接校区括号", raw: "60人（日照）教学楼101", want: "(日照)教学楼101"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoomName(tc.raw); got != tc.want {
				t.Fatalf("方位/校区括号必须保留，期望 %q，实际 %q（原文 %q）", tc.want, got, tc.raw)
			}
		})
	}
}

// 原始名保留（MatrixRoom.NameRaw → room.name_raw）是 pipeline 落库的契约，
// 不由 RoomName 这个纯函数承担：这里删掉了原先只验证「传值不修改局部变量」的恒真用例，
// 等 pipeline 落地后再在那一层覆盖，避免留下虚假的覆盖记录。

// TestRoomNameIsIdempotent 是旧 spec Requirement 5.2 的属性测试：由房名片段拼出的输入
// （含全角、空白、容量片段混排）规范化一次与两次的结果必须相同。
func TestRoomNameIsIdempotent(t *testing.T) {
	property := func(raw string) bool {
		once := RoomName(raw)
		return RoomName(once) == once
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("房名规范化必须幂等: %v", err)
	}
}

// TestRoomNameLeavesNoFullWidthOrUncollapsedWhitespace 是第 1、3 步的属性测试：
// 非回退分支的输出里不能再有全角 ASCII 或表意空格，也不能有首尾空白与连续空白。
func TestRoomNameLeavesNoFullWidthOrUncollapsedWhitespace(t *testing.T) {
	property := func(name roomName) bool {
		out := RoomName(string(name))

		for _, r := range out {
			if r == '\u3000' || (r >= '\uFF01' && r <= '\uFF5E') {
				return false
			}
		}

		return out == strings.Join(strings.Fields(out), " ")
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("规范化后不能残留全角字符或未压缩的空白: %v", err)
	}
}

// TestRoomNameLeavesNoCapacityFragment 是第 2 步的属性测试：
// 任意由房名片段拼出的输入，规范化后都不能再扫出容量片段。
//
// 生成器会固定塞一段非容量字面，保证输入不是「整名容量」、不会走回退分支：
// 回退按契约返回原文，原文里的容量片段本来就该留着，由
// TestRoomNameFallsBackToRawWhenWholeNameIsCapacity 的确定性用例覆盖。
func TestRoomNameLeavesNoCapacityFragment(t *testing.T) {
	property := func(name roomName) bool {
		return !hasCapacityLike(RoomName(string(name)))
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("规范化后不能残留容量片段: %v", err)
	}
}

// TestFoldWidth 用确定性用例锁住宽度归一的边界：全角 ASCII 范围 U+FF01–U+FF5E
// 折成 ASCII、U+3000 折成空格，范围外的字符（希腊 Κ、中文、U+FF5F、不换行空格）
// 原样保留，不能被顺手改写。
func TestFoldWidth(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "全角感叹号到撇号", raw: "！＂＃＄％＆＇", want: `!"#$%&'`},
		{name: "全角字母数字", raw: "Ｆ１２６", want: "F126"},
		{name: "全角括号与斜杠", raw: "（９０／０）", want: "(90/0)"},
		{name: "范围下界", raw: "！", want: "!"},
		{name: "范围上界", raw: "～", want: "~"},
		{name: "表意空格", raw: "阶梯　教室", want: "阶梯 教室"},
		{name: "希腊字母 Κ 不折", raw: "Κ", want: "Κ"},
		{name: "中文不折", raw: "综合教学楼中文", want: "综合教学楼中文"},
		{name: "U+FF5F 不在范围内", raw: "\uFF5F", want: "\uFF5F"},
		{name: "不换行空格不折", raw: "\u00a0", want: "\u00a0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldWidth(tc.raw); got != tc.want {
				t.Fatalf("宽度归一结果不符，期望 %q，实际 %q（原文 %q）", tc.want, got, tc.raw)
			}
		})
	}
}

// hasCapacityLike 是测试侧按文法独立写的容量扫描，不复用实现里的正则：
//   - 括号片段：配对的括号内整段只能是「数字 / 数字」或「数字 人」，两端空白不计；
//   - 裸片段：数字串后跟可选空白与「人」。
//
// 只认整段匹配。旧版「从 ( 扫到第一个 )，里面有斜杠和数字就算残留」过宽：
// 生成器会拼出括号不配对的字面（如 "F126(/ 101(东)"），旧判法拿外层 ( 去配后面
// 某个无关的 )，把中间的斜杠与数字误报成容量残留；实现按文法并不会删这种文本。
// 括号不配对（缺右括号）不算片段：实现的正则同样删不掉它。
func hasCapacityLike(text string) bool {
	runes := []rune(text)

	for i, r := range runes {
		if r != '(' {
			continue
		}
		if end := matchingParen(runes, i); end >= 0 && isCapacityInside(runes[i+1:end]) {
			return true
		}
	}

	for i := 0; i < len(runes); {
		if !isASCIIDigit(runes[i]) {
			i++
			continue
		}

		j := i
		for j < len(runes) && isASCIIDigit(runes[j]) {
			j++
		}

		k := j
		for k < len(runes) && unicode.IsSpace(runes[k]) {
			k++
		}
		if k < len(runes) && runes[k] == '人' {
			return true
		}

		i = j
	}

	return false
}

// matchingParen 返回与 start 处左括号配对的右括号下标，括号不配对时返回 -1。
func matchingParen(runes []rune, start int) int {
	depth := 0
	for i := start; i < len(runes); i++ {
		switch runes[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// isCapacityInside 判断括号内整段是否就是容量文法（数字/数字 或 数字人），两端空白不计。
func isCapacityInside(inside []rune) bool {
	inside = trimCapacitySpace(inside)
	if len(inside) == 0 {
		return false
	}

	i := 0
	for i < len(inside) && isASCIIDigit(inside[i]) {
		i++
	}
	if i == 0 {
		return false
	}

	// 数字与单位之间的空白两种写法都允许。
	for i < len(inside) && unicode.IsSpace(inside[i]) {
		i++
	}

	switch {
	case i < len(inside) && inside[i] == '/':
		i++
		for i < len(inside) && unicode.IsSpace(inside[i]) {
			i++
		}
		start := i
		for i < len(inside) && isASCIIDigit(inside[i]) {
			i++
		}
		if i == start {
			return false
		}
	case i < len(inside) && inside[i] == '人':
		i++
	default:
		return false
	}

	for i < len(inside) && unicode.IsSpace(inside[i]) {
		i++
	}

	return i == len(inside)
}

// trimCapacitySpace 去掉两端空白，测试侧按 unicode.IsSpace 判；
// 与实现正则空白类的差别（\v、U+0085）由 RoomName 先做 Fields 归一吸收。
func trimCapacitySpace(runes []rune) []rune {
	start, end := 0, len(runes)
	for start < end && unicode.IsSpace(runes[start]) {
		start++
	}
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}

	return runes[start:end]
}

// isASCIIDigit 只认 ASCII 数字：全角数字要先过 foldWidth，没折掉就是坏数据。
func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// roomName 是房名生成器：把真实房名里会出现的字面、容量片段、方位括号、
// 全角写法与各类空白随机拼 0..6 段，让样本落在宽度归一、反复删除与空白压缩的边界上。
//
// 生成器固定塞入一段非容量字面 "F126"：整名都是容量片段时 RoomName 按契约回退原文，
// 上面两条属性只覆盖「删干净」的分支，回退分支由确定性用例负责。
type roomName string

// Generate 实现 testing/quick.Generator；size 由 quick 传入，这里只用来限制拼接段数。
func (roomName) Generate(rand *rand.Rand, size int) reflect.Value {
	pieces := []string{
		"F126", "101", "综合教学楼", "阶梯教室", "中文",
		"Ａ１０１", "１２３",
		"！＂＃＄％＆＇", "Κ",
		"（东）", "(曲阜)", "（日照）",
		"(90/0)", "（６０人）", "(60/0)",
		"60人", " 60 人 ",
		"　", " ", "\t", "\n", "\u00a0",
		"～", "(", ")", "（", "）", "/", "人",
	}

	const anchor = "F126"
	count := rand.Intn(min(size, 6) + 1)
	at := rand.Intn(count + 1)

	var builder strings.Builder
	for i := 0; i <= count; i++ {
		if i == at {
			builder.WriteString(anchor)
		}
		if i < count {
			builder.WriteString(pieces[rand.Intn(len(pieces))])
		}
	}

	return reflect.ValueOf(roomName(builder.String()))
}
