package clean

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testDict 手写镜像 backend/migrations/0001_baseline.sql 的 dict v1 种子：
// Symbols 是 dict_symbol，States 是 dict_state（含仅作展示、不进符号表的 fully_free）。
// 不读库是有意的：clean 的契约就是「调用方注入什么字典就按什么投影」，
// 测试必须能把可用性改成与种子相反，才能证明代码里没有第二套判断。
func testDict() model.Dict {
	return model.Dict{
		Symbols: map[string]string{
			"◆":  "class",
			"Ｊ":  "borrowed",
			"Ｘ":  "locked",
			"Κ":  "exam",
			"空闲": "free",
			"Ｇ":  "fixed_reschedule",
			"Ｌ":  "temp_reschedule",
			"M":  "cross_mode",
		},
		States: map[string]model.DictState{
			"class":            {Label: "正常上课", Available: false},
			"borrowed":         {Label: "借用", Available: false},
			"locked":           {Label: "锁定", Available: false},
			"exam":             {Label: "考试", Available: false},
			"free":             {Label: "空闲", Available: true},
			"fixed_reschedule": {Label: "固定调课", Available: false},
			"temp_reschedule":  {Label: "临时调课", Available: false},
			"fully_free":       {Label: "完全空闲", Available: true},
			"cross_mode":       {Label: "跨模式占用", Available: false},
			"composite":        {Label: "复合占用", Available: false},
			"unknown":          {Label: "未知", Available: false},
		},
	}
}

// dictWithAvailability 复制一份字典并改掉某个状态键的可用性，用来证明可用性只跟字典走。
func dictWithAvailability(key string, available bool) model.Dict {
	dict := testDict()
	state := dict.States[key]
	state.Available = available
	dict.States[key] = state

	return dict
}

// TestStateClassifiesRaw 用表驱动覆盖四个分类分支与字形边界
// （Requirement 4.1–4.3）：整词、空/纯空白、多符号复合、未收录。
func TestStateClassifiesRaw(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		wantKey       string
		wantAvailable bool
	}{
		{name: "空文本判空闲", raw: "", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "只有一个空格判空闲", raw: " ", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "制表符换行回车判空闲", raw: "\t\n\r", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "全角空格判空闲", raw: "　", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "多种空白混合判空闲", raw: " \t　\n ", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "整词正常上课", raw: "◆", wantKey: "class", wantAvailable: false},
		{name: "整词借用全角J", raw: "Ｊ", wantKey: "borrowed", wantAvailable: false},
		{name: "整词锁定全角X", raw: "Ｘ", wantKey: "locked", wantAvailable: false},
		{name: "整词考试希腊Kappa", raw: "Κ", wantKey: "exam", wantAvailable: false},
		{name: "整词固定调课全角G", raw: "Ｇ", wantKey: "fixed_reschedule", wantAvailable: false},
		{name: "整词临时调课全角L", raw: "Ｌ", wantKey: "temp_reschedule", wantAvailable: false},
		{name: "整词跨模式ASCII M", raw: "M", wantKey: "cross_mode", wantAvailable: false},
		{name: "整词空闲符号", raw: "空闲", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "空闲符号带首尾空白", raw: " 空闲 ", wantKey: model.StateKeyFree, wantAvailable: true},
		{name: "符号带首尾空白仍按整词命中", raw: " ◆ ", wantKey: "class", wantAvailable: false},
		{name: "两个已知符号组合判复合", raw: "◆Ｊ", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "同一符号重复判复合", raw: "◆◆", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "多字符号参与组合判复合", raw: "空闲◆", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "三个已知符号组合判复合", raw: "◆ＪＸ", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "半角J不在字典判未知", raw: "J", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "全角M不等于ASCII M判未知", raw: "Ｍ", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "ASCII K不等于希腊Kappa判未知", raw: "K", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "全角A判未知", raw: "Ａ", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "展示名不是符号判未知", raw: "正常上课", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "完全空闲只是标签不进符号表判未知", raw: "完全空闲", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "已知符号混未知字符判未知", raw: "◆？", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "两个已知符号加尾部未知字符判未知", raw: "◆Ｊ？", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "复合带首尾空白仍判复合", raw: " ◆Ｊ ", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "多字符号复合带首尾空白仍判复合", raw: " 空闲◆ ", wantKey: model.StateKeyComposite, wantAvailable: false},
		{name: "复合带首尾空白加尾部未知仍判未知", raw: " ◆Ｊ？ ", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "符号之间夹空白不认分隔符判未知", raw: "◆ Ｊ", wantKey: model.StateKeyUnknown, wantAvailable: false},
		{name: "纯文本未收录判未知", raw: "有课", wantKey: model.StateKeyUnknown, wantAvailable: false},
	}

	dict := testDict()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := State(tc.raw, dict)

			if obs.StateKey != tc.wantKey {
				t.Fatalf("语义键不符，期望 %q，实际 %q（原文 %q）", tc.wantKey, obs.StateKey, tc.raw)
			}
			if obs.Available != tc.wantAvailable {
				t.Fatalf("可用性不符，期望 %v，实际 %v（原文 %q）", tc.wantAvailable, obs.Available, tc.raw)
			}
			if obs.RawText != tc.raw {
				t.Fatalf("原文必须原样保留，期望 %q，实际 %q", tc.raw, obs.RawText)
			}
		})
	}
}

// TestStateKeyConstantsMatchSeed 锁住三个语义键常量的字面量：
// 它们直接写进 observation.state_key，必须与 dict_state 种子（migrations/0001_baseline.sql）一致。
func TestStateKeyConstantsMatchSeed(t *testing.T) {
	if model.StateKeyFree != "free" || model.StateKeyComposite != "composite" || model.StateKeyUnknown != "unknown" {
		t.Fatalf("语义键常量必须与种子一致，实际 %q/%q/%q",
			model.StateKeyFree, model.StateKeyComposite, model.StateKeyUnknown)
	}
}

// TestStateWholeWordBeatsComposite 锁住整词优先（Requirement 4.1）：
// 字典同时收录 Ａ、Ｂ 与整词 ＡＢ 时，State("ＡＢ") 必须返回整词的语义键，
// 不能因为「能拆成两个更短符号」就判复合；可用性也随整词取，不随复合恒不可用。
func TestStateWholeWordBeatsComposite(t *testing.T) {
	dict := model.Dict{
		Symbols: map[string]string{
			"Ａ":  "locked",
			"Ｂ":  "borrowed",
			"ＡＢ": "class",
		},
		States: map[string]model.DictState{
			"locked":                {Label: "锁定", Available: false},
			"borrowed":              {Label: "借用", Available: false},
			"class":                 {Label: "正常上课", Available: true},
			model.StateKeyComposite: {Label: "复合占用", Available: true},
			model.StateKeyUnknown:   {Label: "未知", Available: true},
		},
	}

	obs := State("ＡＢ", dict)
	if obs.StateKey != "class" {
		t.Fatalf("整词应优先于更短符号切分，期望 %q，实际 %q（原文 %q）", "class", obs.StateKey, obs.RawText)
	}
	if !obs.Available {
		t.Fatal("整词命中应按整词语义键取字典可用性")
	}

	// 反过来，字典里没有整词 ＡＢ 时，同样的原文才判复合且不可用。
	delete(dict.Symbols, "ＡＢ")
	if obs := State("ＡＢ", dict); obs.StateKey != model.StateKeyComposite || obs.Available {
		t.Fatalf("缺少整词时两个已知符号应判复合且不可用，实际 %q / %v", obs.StateKey, obs.Available)
	}
}

// TestStateSegmentsOverlappingSymbols 覆盖切分算法本身：符号之间存在前缀重叠时，
// 「最长匹配」贪心会先吃掉 ＡＢ、把剩下的 Ｃ 判成不能切分而漏判复合；
// 实际存在 Ａ + ＢＣ 的完整切分，应判复合。真实 v1 字典没有重叠符号，
// 这条是给字典演进留的护栏，防止有人把动态规划换成贪心。
func TestStateSegmentsOverlappingSymbols(t *testing.T) {
	dict := model.Dict{
		Symbols: map[string]string{
			"ＡＢ": "class",
			"Ａ":  "locked",
			"ＢＣ": "borrowed",
		},
		States: map[string]model.DictState{
			"class":    {Label: "正常上课", Available: false},
			"locked":   {Label: "锁定", Available: false},
			"borrowed": {Label: "借用", Available: false},
		},
	}

	obs := State("ＡＢＣ", dict)
	if obs.StateKey != model.StateKeyComposite {
		t.Fatalf("重叠符号应判复合，实际 %q（原文 %q）", obs.StateKey, obs.RawText)
	}
	if obs.Available {
		t.Fatal("复合必须恒不可用")
	}
}

// TestStateEmptyStateKeyFailsClosed 锁住坏字典的兜底：dict.Symbols 把符号映射到空语义键、
// 或 dict.States 里存在空键且配成可用时，State 不能产出 StateKey="" 且 Available=true。
// 空语义键无法落库（observation.state_key 必须能在字典里查回），必须按未收录处理。
func TestStateEmptyStateKeyFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		dict model.Dict
		raw  string
	}{
		{
			name: "整词映射到空语义键且空键可用",
			dict: model.Dict{
				Symbols: map[string]string{"◆": ""},
				States:  map[string]model.DictState{"": {Label: "坏数据", Available: true}},
			},
			raw: "◆",
		},
		{
			name: "整词映射到空语义键但无空键定义",
			dict: model.Dict{
				Symbols: map[string]string{"◆": ""},
				States:  map[string]model.DictState{},
			},
			raw: "◆",
		},
		{
			name: "空键可用也不要影响未收录",
			dict: model.Dict{
				Symbols: map[string]string{},
				States:  map[string]model.DictState{"": {Label: "坏数据", Available: true}},
			},
			raw: "◆",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := State(tc.raw, tc.dict)

			if obs.StateKey == "" {
				t.Fatalf("绝不能产出空语义键（原文 %q）", tc.raw)
			}
			if obs.StateKey != model.StateKeyUnknown {
				t.Fatalf("空语义键应按未收录处理，期望 %q，实际 %q", model.StateKeyUnknown, obs.StateKey)
			}
			if obs.Available {
				t.Fatalf("未收录必须不可用（原文 %q，语义键 %q）", tc.raw, obs.StateKey)
			}
			if obs.RawText != tc.raw {
				t.Fatalf("原文必须原样保留，期望 %q，实际 %q", tc.raw, obs.RawText)
			}
		})
	}
}

// TestStateProjectsAvailabilityFromDict 覆盖 Requirement 4.4 与 CP4：
// 已知状态取字典字段；未知与复合恒不可用；字典缺行或零值字典一律判不可用。
func TestStateProjectsAvailabilityFromDict(t *testing.T) {
	missingClass := testDict()
	delete(missingClass.States, "class")

	missingFree := testDict()
	delete(missingFree.States, model.StateKeyFree)

	cases := []struct {
		name          string
		dict          model.Dict
		raw           string
		wantKey       string
		wantAvailable bool
	}{
		{
			name:          "整词可用性取字典的可用值",
			dict:          dictWithAvailability("class", true),
			raw:           "◆",
			wantKey:       "class",
			wantAvailable: true,
		},
		{
			name:          "整词可用性取字典的不可用值",
			dict:          testDict(),
			raw:           "Ｊ",
			wantKey:       "borrowed",
			wantAvailable: false,
		},
		{
			name:          "空文本可用性跟 free 字典行走而不是写死为可用",
			dict:          dictWithAvailability(model.StateKeyFree, false),
			raw:           "",
			wantKey:       model.StateKeyFree,
			wantAvailable: false,
		},
		{
			name:          "字典缺 free 定义时判不可用",
			dict:          missingFree,
			raw:           "  ",
			wantKey:       model.StateKeyFree,
			wantAvailable: false,
		},
		{
			name:          "字典缺语义键时保留符号键但判不可用",
			dict:          missingClass,
			raw:           "◆",
			wantKey:       "class",
			wantAvailable: false,
		},
		{
			name:          "复合即使字典配成可用也恒不可用",
			dict:          dictWithAvailability(model.StateKeyComposite, true),
			raw:           "◆Ｊ",
			wantKey:       model.StateKeyComposite,
			wantAvailable: false,
		},
		{
			name:          "未知即使字典配成可用也恒不可用",
			dict:          dictWithAvailability(model.StateKeyUnknown, true),
			raw:           "有课",
			wantKey:       model.StateKeyUnknown,
			wantAvailable: false,
		},
		{
			name: "符号直接指向复合键时仍恒不可用",
			dict: model.Dict{
				Symbols: map[string]string{"◆": model.StateKeyComposite},
				States:  dictWithAvailability(model.StateKeyComposite, true).States,
			},
			raw:           "◆",
			wantKey:       model.StateKeyComposite,
			wantAvailable: false,
		},
		{
			name:          "零值字典下非空原文判未知且不可用",
			dict:          model.Dict{},
			raw:           "◆",
			wantKey:       model.StateKeyUnknown,
			wantAvailable: false,
		},
		{
			name:          "零值字典下空白原文判空闲但不可用",
			dict:          model.Dict{},
			raw:           " ",
			wantKey:       model.StateKeyFree,
			wantAvailable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obs := State(tc.raw, tc.dict)

			if obs.StateKey != tc.wantKey {
				t.Fatalf("语义键不符，期望 %q，实际 %q（原文 %q）", tc.wantKey, obs.StateKey, tc.raw)
			}
			if obs.Available != tc.wantAvailable {
				t.Fatalf("可用性不符，期望 %v，实际 %v（原文 %q，语义键 %q）",
					tc.wantAvailable, obs.Available, tc.raw, obs.StateKey)
			}
		})
	}
}

// expectedAvailable 是字典投影口径的测试侧独立表述（CP4）：
// 复合与未知恒 false，其余取字典，字典缺行也算 false。
// 单独写一份而不是复用 projectAvailable，失败时才能说明是「实现偏了」而不是「测试跟着实现写」。
func expectedAvailable(key string, dict model.Dict) bool {
	if key == model.StateKeyComposite || key == model.StateKeyUnknown {
		return false
	}

	state, ok := dict.States[key]
	return ok && state.Available
}

// TestStateAvailabilityAlwaysMatchesDict 是 CP4 的属性测试：
// 任意状态原文，State 投影出的 available 必须与字典字段一致，
// 且语义键只能来自「三个特殊键 + 字典符号指向的键」，不能凭空造键。
// 字典取可用性全反转的一份，确保实现没有把种子里的 true/false 写死在代码里。
func TestStateAvailabilityAlwaysMatchesDict(t *testing.T) {
	dict := testDict()
	for key, state := range dict.States {
		state.Available = !state.Available
		dict.States[key] = state
	}

	allowedKeys := map[string]bool{
		model.StateKeyFree:      true,
		model.StateKeyComposite: true,
		model.StateKeyUnknown:   true,
	}
	for _, key := range dict.Symbols {
		allowedKeys[key] = true
	}

	property := func(raw rawText) bool {
		obs := State(string(raw), dict)

		if obs.StateKey == "" || !allowedKeys[obs.StateKey] {
			return false
		}
		if obs.Available != expectedAvailable(obs.StateKey, dict) {
			return false
		}
		if strings.TrimSpace(string(raw)) == "" && obs.StateKey != model.StateKeyFree {
			return false
		}

		return true
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatalf("任意原文的可用性投影必须与字典一致: %v", err)
	}
}

// TestStateAlwaysKeepsRawText 是 Requirement 4.3/4.4 的属性测试：
// 四个分类分支都必须原样保留原文，空文本与纯空白也不例外。
func TestStateAlwaysKeepsRawText(t *testing.T) {
	dict := testDict()

	property := func(raw rawText) bool {
		return State(string(raw), dict).RawText == string(raw)
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatalf("任意状态原文都必须原样保留: %v", err)
	}
}

// TestStateCompositeAndUnknownNeverAvailable 是 CP4 后半句的属性测试：
// 输入是任意原文，只要落进 composite 或 unknown，available 必须是 false，
// 即使字典把这两个键配成可用（字典已在上一个测试里全反转，这里再显式配一次）。
func TestStateCompositeAndUnknownNeverAvailable(t *testing.T) {
	dict := dictWithAvailability(model.StateKeyComposite, true)
	state := dict.States[model.StateKeyUnknown]
	state.Available = true
	dict.States[model.StateKeyUnknown] = state

	property := func(raw rawText) bool {
		obs := State(string(raw), dict)
		if obs.StateKey != model.StateKeyComposite && obs.StateKey != model.StateKeyUnknown {
			return true
		}

		return !obs.Available
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatalf("复合与未知必须恒不可用: %v", err)
	}
}

// TestStateClassificationOfSymbolRuns 是分类口径的属性测试，期望值由构造方式给出，
// 不经过实现里的切分逻辑：单个已知符号 → 整词命中；两个及以上 → 复合；零个 → 空闲。
func TestStateClassificationOfSymbolRuns(t *testing.T) {
	dict := testDict()

	property := func(run symbolRun) bool {
		obs := State(strings.Join(run, ""), dict)

		switch len(run) {
		case 0:
			return obs.StateKey == model.StateKeyFree
		case 1:
			return obs.StateKey == dict.Symbols[run[0]]
		default:
			return obs.StateKey == model.StateKeyComposite && !obs.Available
		}
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatalf("已知符号拼接的分类必须按符号个数确定: %v", err)
	}
}

// TestStateWhitespaceOnlyIsFree 是空文本分支的属性测试：
// 任意由 Unicode 空白拼成的原文都必须判空闲，且可用性跟字典的 free 行走。
func TestStateWhitespaceOnlyIsFree(t *testing.T) {
	dict := testDict()

	property := func(blank blankText) bool {
		obs := State(string(blank), dict)

		return obs.StateKey == model.StateKeyFree &&
			obs.Available == dict.States[model.StateKeyFree].Available
	}

	if err := quick.Check(property, &quick.Config{MaxCount: 300}); err != nil {
		t.Fatalf("任意纯空白原文都必须判空闲: %v", err)
	}
}

// rawText 是属性测试的状态原文生成器：从字典符号、字形相近但不在字典里的写法、
// 未知文本与空白里随机拼 0..4 段，让样本落在整词、复合、未知与空白四个分支附近。
type rawText string

// Generate 实现 testing/quick.Generator；size 由 quick 传入，这里只用来限制拼接段数。
func (rawText) Generate(rand *rand.Rand, size int) reflect.Value {
	pieces := []string{
		"◆", "Ｊ", "Ｘ", "Κ", "空闲", "Ｇ", "Ｌ", "M",
		"J", "X", "K", "L", "G", "Ｍ", "Ａ", "？",
		"正常上课", "完全空闲", "有课",
		" ", "\t", "\n", "\r", "　",
	}

	var builder strings.Builder
	for i := 0; i < rand.Intn(min(size, 4)+1); i++ {
		builder.WriteString(pieces[rand.Intn(len(pieces))])
	}

	return reflect.ValueOf(rawText(builder.String()))
}

// symbolRun 是已知符号序列生成器：每个元素都取自字典符号表，
// 拼起来就是「整词 / 复合」两个分支的输入。
type symbolRun []string

// Generate 实现 testing/quick.Generator；元素个数 0..4，0 个表示空原文。
func (symbolRun) Generate(rand *rand.Rand, size int) reflect.Value {
	symbols := []string{"◆", "Ｊ", "Ｘ", "Κ", "空闲", "Ｇ", "Ｌ", "M"}

	run := make(symbolRun, rand.Intn(min(size, 4)+1))
	for i := range run {
		run[i] = symbols[rand.Intn(len(symbols))]
	}

	return reflect.ValueOf(run)
}

// blankText 是纯空白生成器：覆盖空格、制表符、换行、回车、垂直制表、换页、
// 全角空格与不换行空格，全部是 unicode.IsSpace 认可的空白。
type blankText string

// Generate 实现 testing/quick.Generator；0 个空白表示空原文。
func (blankText) Generate(rand *rand.Rand, size int) reflect.Value {
	spaces := []string{" ", "\t", "\n", "\r", "\v", "\f", "　", "\u00a0"}

	var builder strings.Builder
	for i := 0; i < rand.Intn(min(size, 6)+1); i++ {
		builder.WriteString(spaces[rand.Intn(len(spaces))])
	}

	return reflect.ValueOf(blankText(builder.String()))
}
