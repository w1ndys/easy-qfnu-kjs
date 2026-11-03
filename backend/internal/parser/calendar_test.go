package parser

import (
	"errors"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// 本文件的学期页与周历页夹具全部手写脱敏（docs/pitfalls.md 第 10 节）。

// termPage 拼一张学期页：学期编号写在正文文本里，节次模式 id 在隐藏 input 里。
// 学期编号固定在正文，因为 TermText 只认正文可见文本——需要「正文没有学期编号」的
// 夹具时另手写页面，不要从这里塞一个空学期编号。
// inputTag 由调用方给出，用来覆盖属性顺序、引号风格与空 value 的情况。
func termPage(inputTag string) string {
	return `<html><body><div class="head">2026-2027 学年</div>` +
		`<div class="term">当前学期 2026-2027-1</div>` +
		`<form name="Form1">` + inputTag + `</form></body></html>`
}

// TestTermTextParsesTermAndKBJCMSID 覆盖学期编号与 kbjcmsid 的权威解析：
// 学期编号取第一个 YYYY-YYYY-N；kbjcmsid 大小写、属性顺序、引号风格都不敏感，
// 空的 value 跳过继续找。
func TestTermTextParsesTermAndKBJCMSID(t *testing.T) {
	page := termPage(
		`<input id="kbjcmsid" name="kbjcmsid" type="hidden" value="">` +
			`<input value='KMS0099' NAME='KBJCMSID' type='hidden'>`)

	term, kbjcmsid, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("学期编号不符，实际 %q", term)
	}
	if kbjcmsid != "KMS0099" {
		t.Fatalf("kbjcmsid 不符，实际 %q", kbjcmsid)
	}
}

// TestTermTextFirstTermNumberWins 覆盖同页出现多个学期编号时取第一个：
// 竞争项放在正文里、排在当前学期之后，取到「第一个」才算真的取第一个。
// 竞争项不能放进 select/option——那类节点已被 visibleText 整段跳过，测不出区分力。
func TestTermTextFirstTermNumberWins(t *testing.T) {
	page := termPage(`<input id="kbjcmsid" value="KMS0001">`) +
		`<div class="history">历史学期 2025-2026-2</div>`

	term, _, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("应取第一个学期编号，实际 %q", term)
	}
}

// TestTermTextTakesFirstTermNumberInBody 覆盖 m4：正文里同时出现 2025-2026-2 与
// 2026-2027-1 时取第一个，不按「看起来最新」猜。两个编号都在正文、都留在
// visibleText 里，才测得出「取第一个」这个口径。
func TestTermTextTakesFirstTermNumberInBody(t *testing.T) {
	page := `<html><body>` +
		`<div class="term">2025-2026-2</div>` +
		`<div class="next">2026-2027-1</div>` +
		`<form><input id="kbjcmsid" value="KMS0001"></form>` +
		`</body></html>`

	term, _, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2025-2026-2" {
		t.Fatalf("正文多个学期编号应取第一个，实际 %q", term)
	}
}

// TestTermTextPrefersVisibleCurrentTerm 覆盖 minor 4：脚本与样式里先出现的旧学期编号
// 不能算数，学期编号必须取自可见文本（正文里的当前学期），历史下拉项排在后面。
func TestTermTextPrefersVisibleCurrentTerm(t *testing.T) {
	page := `<html><head>` +
		`<script>var lastTerm = "2025-2026-2";</script>` +
		`<style>/* 2024-2025-1 */</style>` +
		`</head><body><div class="head">2026-2027 学年</div>` +
		`<div class="term">当前学期 2026-2027-1</div>` +
		`<form><input id="kbjcmsid" value="KMS0001"></form>` +
		`<select name="xnxqh"><option>2025-2026-2</option></select>` +
		`</body></html>`

	term, kbjcmsid, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("应取正文可见的当前学期，实际 %q", term)
	}
	if kbjcmsid != "KMS0001" {
		t.Fatalf("kbjcmsid 不符，实际 %q", kbjcmsid)
	}
}

// TestTermTextSkipsHistoryDropdownBeforeCurrentTerm 覆盖 minor（可见文本）：
// 历史学期下拉排在正文当前学期之前时，下拉项与 head 标题里的旧学期编号都不能算数，
// 学期仍取正文的当前学期。
func TestTermTextSkipsHistoryDropdownBeforeCurrentTerm(t *testing.T) {
	page := `<html><head><title>2025-2026-2 学期</title></head><body>` +
		`<form><select name="xnxqh">` +
		`<option value="2025-2026-2">2025-2026-2</option>` +
		`<option value="2025-2026-1">2025-2026-1</option>` +
		`</select></form>` +
		`<div class="term">当前学期 2026-2027-1</div>` +
		`<form><input id="kbjcmsid" value="KMS0001"></form>` +
		`</body></html>`

	term, kbjcmsid, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("应跳过下拉取正文当前学期，实际 %q", term)
	}
	if kbjcmsid != "KMS0001" {
		t.Fatalf("kbjcmsid 不符，实际 %q", kbjcmsid)
	}
}

// TestTermTextVisibleBodyWinsOverFormTerm 覆盖「正文有学期即成功」：正文里写着当前
// 学期时，即使下拉的 selected option 与隐藏字段都还是旧学期，也直接取正文的学期编号，
// 既不失败也不被表单值覆盖。表单字段整段不参与取值，所以旧学期也不会让解析失败。
func TestTermTextVisibleBodyWinsOverFormTerm(t *testing.T) {
	page := `<html><body>` +
		`<div class="term">当前学期 2026-2027-1</div>` +
		`<form name="Form1"><select name="xnxqh">` +
		`<option value="2025-2026-2">2025-2026-2</option>` +
		`<option value="2025-2026-1" selected>2025-2026-1</option>` +
		`</select><input type="hidden" name="xnxqh" value="2025-2026-2">` +
		`<input id="kbjcmsid" value="KMS0001"></form>` +
		`</body></html>`

	term, kbjcmsid, err := TermText([]byte(page))
	if err != nil {
		t.Fatalf("正文有学期编号时应成功: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("应取正文当前学期，实际 %q", term)
	}
	if kbjcmsid != "KMS0001" {
		t.Fatalf("kbjcmsid 不符，实际 %q", kbjcmsid)
	}
}

// TestTermTextKBJCMSIDConflict 覆盖 kbjcmsid 的冲突分支：同页两个非空 kbjcmsid 取值
// 不同时，页面自己没统一，返回 ErrTermTextInvalid，不静默取首个。
// 空 value 与带空白的重复值不算冲突（现网页面会有空的同名 input）。
func TestTermTextKBJCMSIDConflict(t *testing.T) {
	page := termPage(
		`<input id="kbjcmsid" type="hidden" value="KMS0001">` +
			`<input name="KBJCMSID" type="hidden" value="KMS0002">`)

	term, kbjcmsid, err := TermText([]byte(page))
	if err == nil {
		t.Fatalf("kbjcmsid 取值冲突应失败，实际返回 %q / %q", term, kbjcmsid)
	}
	if !errors.Is(err, ErrTermTextInvalid) {
		t.Fatalf("错误应命中 ErrTermTextInvalid，实际 %v", err)
	}
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
	}
	for _, leak := range []string{"KMS0001", "KMS0002", "2026-2027-1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误信息不应含页面值 %q: %v", leak, err)
		}
	}

	// 两个取值只差首尾空白时是同一个值，不算冲突。
	page = termPage(
		`<input id="kbjcmsid" type="hidden" value="KMS0001">` +
			`<input name="kbjcmsid" type="hidden" value=" KMS0001 ">`)
	term, kbjcmsid, err = TermText([]byte(page))
	if err != nil {
		t.Fatalf("取值相同不应失败: %v", err)
	}
	if term != "2026-2027-1" || kbjcmsid != "KMS0001" {
		t.Fatalf("取值不符: got %q / %q", term, kbjcmsid)
	}
}

// TestTextSkipsTitleInBody 覆盖 m3：body 里的 <title> 也按标签黑名单跳过。
// 假学期编号与假周次都放在 title 里、真值放在正文里，两个解析器分别校验：
// 只要 title 没被跳过，任一个断言就会拿到假值。
func TestTextSkipsTitleInBody(t *testing.T) {
	body := `<html><body>` +
		`<title>2025-2026-2 第9周 /16周</title>` +
		`<div class="term">当前学期 2026-2027-1</div>` +
		`<div>第5周 /16周</div>` +
		`<form><input id="kbjcmsid" value="KMS0001"></form>` +
		`</body></html>`

	term, _, err := TermText([]byte(body))
	if err != nil {
		t.Fatalf("学期页解析不应失败: %v", err)
	}
	if term != "2026-2027-1" {
		t.Fatalf("body 内 title 应被跳过，实际 %q", term)
	}

	week, total, err := WeekText([]byte(body))
	if err != nil {
		t.Fatalf("周历解析不应失败: %v", err)
	}
	if week != 5 || total != 16 {
		t.Fatalf("body 内 title 的周次应被跳过: got %d/%d", week, total)
	}
}

// TestTermTextMissingTermNumber 覆盖「正文取不到就失败」：正文里一个学期编号都没有时
// 返回 ErrTermTextInvalid，同时归属 model.ErrUpstreamCalendarInvalid，且错误信息不带
// 页面正文。下拉 selected option 与隐藏字段里写着合法学期编号也不算数——表单当前值
// 可能就是旧学期，这条口径要求硬失败而不是猜（docs/pitfalls.md 第 2 节）。
func TestTermTextMissingTermNumber(t *testing.T) {
	page := `<html><body><div>请选择学期</div>` +
		`<select name="xnxqh"><option value="2026-2027-1" selected>2026-2027-1</option></select>` +
		`<input type="hidden" name="xnxqh" value="2026-2027-1">` +
		`<input id="kbjcmsid" value="KMS0001"></body></html>`

	term, kbjcmsid, err := TermText([]byte(page))
	if err == nil {
		t.Fatalf("正文缺学期编号应失败，实际返回 %q / %q", term, kbjcmsid)
	}
	if !errors.Is(err, ErrTermTextInvalid) {
		t.Fatalf("错误应命中 ErrTermTextInvalid，实际 %v", err)
	}
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
	}
	if errors.Is(err, ErrWeekTextInvalid) || errors.Is(err, ErrDetailStructureInvalid) {
		t.Fatalf("学期错误不应归为周历或明细错误: %v", err)
	}
	for _, leak := range []string{"KMS0001", "2026-2027-1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误信息不应含页面值 %q: %v", leak, err)
		}
	}
}

// TestTermTextMissingKBJCMSID 覆盖缺 kbjcmsid：学期编号已经取到也不能猜，
// 返回 ErrTermTextInvalid，并且错误信息不回显学期编号。
func TestTermTextMissingKBJCMSID(t *testing.T) {
	page := termPage(`<input id="other" value="x">`)

	term, kbjcmsid, err := TermText([]byte(page))
	if err == nil {
		t.Fatalf("缺 kbjcmsid 应失败，实际返回 %q / %q", term, kbjcmsid)
	}
	if !errors.Is(err, ErrTermTextInvalid) {
		t.Fatalf("错误应命中 ErrTermTextInvalid，实际 %v", err)
	}
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
	}
	if strings.Contains(err.Error(), "2026-2027-1") {
		t.Fatalf("错误信息不应含学期编号: %v", err)
	}
}

// TestTermTextEmptyBody 覆盖空正文：没有学期编号，返回 ErrTermTextInvalid。
func TestTermTextEmptyBody(t *testing.T) {
	if _, _, err := TermText(nil); !errors.Is(err, ErrTermTextInvalid) {
		t.Fatalf("空正文应命中 ErrTermTextInvalid，实际 %v", err)
	}
}

// TestWeekTextVariants 覆盖旧 spec specs/2026-10-27-classroom-availability-rewrite Requirement 3.3：兼容「共」可有可无、全角与半角空白，
// 以及加不加空格都成立。
func TestWeekTextVariants(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		week  int
		total int
	}{
		{
			name:  "实测格式带空格无共",
			body:  `<div>第5周 /16周</div>`,
			week:  5,
			total: 16,
		},
		{
			name:  "带共且有空格",
			body:  `<div>第 5 周 / 共 16 周</div>`,
			week:  5,
			total: 16,
		},
		{
			name:  "无空格",
			body:  `<div>第5周/共16周</div>`,
			week:  5,
			total: 16,
		},
		{
			name:  "全角空格",
			body:  `<div>第　5　周　/　共　16　周</div>`,
			week:  5,
			total: 16,
		},
		{
			name:  "换行与制表符",
			body:  "<div>第\n5\t周 / 共\n16 周</div>",
			week:  5,
			total: 16,
		},
		{
			name:  "共与数字间有空白",
			body:  `<div>第5周 / 共　16周</div>`,
			week:  5,
			total: 16,
		},
		{
			// minor 3：&nbsp; 解析出来是 U+00A0，Go 正则的 \s 不匹配它，
			// 所以必须先取可见文本并按 unicode.IsSpace 归一。
			name:  "nbsp 实体",
			body:  `<div>第&nbsp;5&nbsp;周 / 共&nbsp;16&nbsp;周</div>`,
			week:  5,
			total: 16,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			week, total, err := WeekText([]byte(tc.body))
			if err != nil {
				t.Fatalf("周历解析不应失败: %v", err)
			}
			if week != tc.week || total != tc.total {
				t.Fatalf("周次不符: got %d/%d, want %d/%d", week, total, tc.week, tc.total)
			}
		})
	}
}

// TestWeekTextScriptFallback 覆盖 major：实测周次只出现在一句脚本里
// （docs/decisions/2026-10-09-term-week-from-one-sample.md），可见正文没有命中时
// 必须回退到含 script/style 的整页文本再匹配一次。
func TestWeekTextScriptFallback(t *testing.T) {
	body := `<html><body><div>个人课表</div>` +
		`<script>var info="第6周/16周";</script>` +
		`</body></html>`

	week, total, err := WeekText([]byte(body))
	if err != nil {
		t.Fatalf("脚本里的周次也应能解析: %v", err)
	}
	if week != 6 || total != 16 {
		t.Fatalf("周次不符: got %d/%d, want 6/16", week, total)
	}
}

// TestWeekTextVisibleTextWins 覆盖 major 的另一半：可见正文与脚本里都有周次时，
// 可见正文优先，不能被脚本里的值覆盖页面主展示。
// 假脚本排在可见文本之前（放进 head），整页首次命中是脚本里的 6/16，只有先取
// 可见文本才能得到 5/16——脚本若排在可见文本之后，两个口径都命中 5/16，
// 这个断言就证明不了「可见优先」。
func TestWeekTextVisibleTextWins(t *testing.T) {
	body := `<html><head><script>var info="第6周/16周";</script></head>` +
		`<body><div>第5周 /16周</div></body></html>`

	if strings.Index(body, "第6周") > strings.Index(body, "第5周") {
		t.Fatalf("夹具失效：假脚本应排在可见文本之前，否则整页首次命中仍是 5/16")
	}

	week, total, err := WeekText([]byte(body))
	if err != nil {
		t.Fatalf("周历解析不应失败: %v", err)
	}
	if week != 5 || total != 16 {
		t.Fatalf("可见正文应优先: got %d/%d, want 5/16", week, total)
	}
}

// TestWeekTextFirstMatchWins 覆盖同页多处出现周次文本时取第一个。
func TestWeekTextFirstMatchWins(t *testing.T) {
	body := `<div>第5周 /16周</div><div>第6周 /16周</div>`

	week, total, err := WeekText([]byte(body))
	if err != nil {
		t.Fatalf("周历解析不应失败: %v", err)
	}
	if week != 5 || total != 16 {
		t.Fatalf("应取第一次命中: got %d/%d", week, total)
	}
}

// TestWeekTextMissing 覆盖非教学周或文本缺失：返回 ErrWeekTextInvalid，
// 同时归属 model.ErrUpstreamCalendarInvalid，不猜周次。
func TestWeekTextMissing(t *testing.T) {
	body := `<html><body><div>当前不是教学周</div></body></html>`

	week, total, err := WeekText([]byte(body))
	if err == nil {
		t.Fatalf("缺周次文本应失败，实际返回 %d/%d", week, total)
	}
	if !errors.Is(err, ErrWeekTextInvalid) {
		t.Fatalf("错误应命中 ErrWeekTextInvalid，实际 %v", err)
	}
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
	}
	if errors.Is(err, ErrTermTextInvalid) || errors.Is(err, ErrDetailStructureInvalid) {
		t.Fatalf("周历错误不应归为学期或明细错误: %v", err)
	}
	if strings.Contains(err.Error(), "当前不是教学周") {
		t.Fatalf("错误信息不应含页面正文: %v", err)
	}
}

// TestWeekTextOutOfRange 覆盖 X、Y 不满足 1 ≤ X ≤ Y：同样返回 ErrWeekTextInvalid。
func TestWeekTextOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "当前周大于总周数", body: `<div>第17周 / 16周</div>`},
		{name: "当前周为零", body: `<div>第0周 / 16周</div>`},
		{name: "总周数为零", body: `<div>第1周 / 0周</div>`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			week, total, err := WeekText([]byte(tc.body))
			if err == nil {
				t.Fatalf("非法周次应失败，实际返回 %d/%d", week, total)
			}
			if !errors.Is(err, ErrWeekTextInvalid) {
				t.Fatalf("错误应命中 ErrWeekTextInvalid，实际 %v", err)
			}
			if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
				t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
			}
		})
	}
}

// TestWeekTextTotalWeeksBusinessLimit 覆盖总周数的业务上限：一年最多 52 周，
// Y=52 仍然合法，Y=53 返回 ErrWeekTextInvalid，且错误信息不回显页面给出的 Y。
func TestWeekTextTotalWeeksBusinessLimit(t *testing.T) {
	week, total, err := WeekText([]byte(`<div>第52周 / 52周</div>`))
	if err != nil {
		t.Fatalf("52 周应仍合法: %v", err)
	}
	if week != 52 || total != 52 {
		t.Fatalf("上限内周次不符: got %d/%d", week, total)
	}

	week, total, err = WeekText([]byte(`<div>第5周 / 53周</div>`))
	if err == nil {
		t.Fatalf("53 周应失败，实际返回 %d/%d", week, total)
	}
	if !errors.Is(err, ErrWeekTextInvalid) {
		t.Fatalf("错误应命中 ErrWeekTextInvalid，实际 %v", err)
	}
	if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
		t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
	}
	if strings.Contains(err.Error(), "53") {
		t.Fatalf("错误信息不应含完整的页面总周数: %v", err)
	}
}

// TestWeekTextOverflowIsClipped 覆盖 minor 2：页面捕获数字超出 int 范围时，
// 错误信息按 clipPageValue 截断到 32 rune，不能把整段长数字带进日志。
func TestWeekTextOverflowIsClipped(t *testing.T) {
	longDigits := strings.Repeat("9", 40)
	cases := []struct {
		name string
		body string
	}{
		{name: "当前周溢出", body: `<div>第` + longDigits + `周 / 共 16 周</div>`},
		{name: "总周数溢出", body: `<div>第5周 / 共` + longDigits + `周</div>`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			week, total, err := WeekText([]byte(tc.body))
			if err == nil {
				t.Fatalf("溢出应失败，实际返回 %d/%d", week, total)
			}
			if !errors.Is(err, ErrWeekTextInvalid) {
				t.Fatalf("错误应命中 ErrWeekTextInvalid，实际 %v", err)
			}
			if !errors.Is(err, model.ErrUpstreamCalendarInvalid) {
				t.Fatalf("错误应同时归属 ErrUpstreamCalendarInvalid，实际 %v", err)
			}
			if strings.Contains(err.Error(), longDigits) {
				t.Fatalf("错误信息不应含完整长数字: %v", err)
			}
			if !strings.Contains(err.Error(), "…") {
				t.Fatalf("错误里的页面值应被截断并标省略号: %v", err)
			}
		})
	}
}

// TestWeekTextEmptyBody 覆盖空正文：没有周次文本，返回 ErrWeekTextInvalid。
func TestWeekTextEmptyBody(t *testing.T) {
	if _, _, err := WeekText(nil); !errors.Is(err, ErrWeekTextInvalid) {
		t.Fatalf("空正文应命中 ErrWeekTextInvalid，实际 %v", err)
	}
}
