package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/collector"
	"github.com/W1ndys/easy-qfnu-kjs/pkg/cas"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", name, err)
	}
	return string(b)
}

func TestNormalizeRoomName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"F126(90/0)", "F126"},
		{"篮球馆（东）(25/0)", "篮球馆(东)"}, // NFKC 先转半角括号，再删除容量
		{"射艺场地（西田径场以西）(40/40)", "射艺场地(西田径场以西)"},
		{" 老文史楼101 (75/30) ", "老文史楼101"},
		{"JA101（１０／２）", "JA101"},            // 全角数字容量
		{"房间（东北角）(90/0)(10/10)", "房间(东北角)"}, // 反复删除容量
		{"A楼(60)", "A楼"},     // 单个数字容量
		{"B楼（东区）", "B楼(东区)"}, // 方位括号保留
		{"C楼", "C楼"},
		{"D-1(88/0)（2号院）", "D-1(2号院)"},
	}
	for _, c := range cases {
		if got := collector.NormalizeRoomName(c.in); got != c.want {
			t.Errorf("NormalizeRoomName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fixtureStatus 复刻 fixture 生成公式：与生成脚本一致。
func fixtureStatus(d, b int) int {
	if (d == 3 && b == 2) || (d == 5 && b == 4) {
		return 5 // 空单元格 = 空闲
	}
	return ((d-1)+b*3)%9 + 1
}

func TestParseWeekNormal(t *testing.T) {
	rows, err := collector.ParseWeekHTML(readFixture(t, "week_normal.html"))
	if err != nil {
		t.Fatalf("解析正常周表失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 = %d, want 2", len(rows))
	}
	r0, r1 := rows[0], rows[1]
	if r0.Jsbh != "0899" || r0.Name != "综合教学楼101" {
		t.Errorf("row0 = %q/%q, want 0899/综合教学楼101", r0.Jsbh, r0.Name)
	}
	if r1.Jsbh != "1306" || r1.Name != "篮球馆(东)" {
		t.Errorf("row1 = %q/%q, want 1306/篮球馆(东)（checkbox value 回退）", r1.Jsbh, r1.Name)
	}
	for d := 1; d <= 7; d++ {
		for b := 0; b < 5; b++ {
			want := fixtureStatus(d, b)
			if r0.Blocks[d-1][b] != want {
				t.Errorf("row0 day=%d block=%d = %d, want %d", d, b, r0.Blocks[d-1][b], want)
			}
			if r1.Blocks[d-1][b] != want {
				t.Errorf("row1 day=%d block=%d = %d, want %d", d, b, r1.Blocks[d-1][b], want)
			}
		}
	}
	if r0.Blocks[2][2] != 5 {
		t.Errorf("空单元格应映射为 5（空闲），got %d", r0.Blocks[2][2])
	}
	// 全 9 种状态符号均出现在 fixture 中（含 Κ、完全空闲、M、Ｇ、Ｌ）。
	seen := map[int]bool{}
	for d := range 7 {
		for b := range 5 {
			seen[r0.Blocks[d][b]] = true
		}
	}
	for id := 1; id <= 9; id++ {
		if !seen[id] {
			t.Errorf("fixture 未覆盖状态 %d", id)
		}
	}
}

func TestStatusesForDayExpansion(t *testing.T) {
	// 每天 5 大块 → 12 小节；块内小节一致。
	var row collector.ParsedRow
	for b := range 5 {
		row.Blocks[0][b] = b + 1
	}
	st := row.StatusesForDay(0)
	want := map[string]int{
		"01": 1, "02": 1,
		"03": 2, "04": 2, "05": 2,
		"06": 3, "07": 3,
		"08": 4, "09": 4,
		"10": 5, "11": 5, "12": 5,
	}
	if len(st) != 12 {
		t.Fatalf("展开后小节数 = %d, want 12", len(st))
	}
	for code, id := range want {
		if st[code] != id {
			t.Errorf("statuses[%s] = %d, want %d", code, st[code], id)
		}
	}
}

func TestParseWeekUnknownStatus(t *testing.T) {
	_, err := collector.ParseWeekHTML(readFixture(t, "week_unknown_status.html"))
	var ce *collector.CollectorError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 CollectorError，got %v", err)
	}
	if ce.Code != collector.CodeStructure {
		t.Errorf("错误码 = %s, want %s", ce.Code, collector.CodeStructure)
	}
	if !strings.Contains(ce.Error(), "未知状态") {
		t.Errorf("错误信息应含「未知状态」: %v", ce)
	}
}

func TestParseWeekExtraTdValue(t *testing.T) {
	_, err := collector.ParseWeekHTML(readFixture(t, "week_extra_tdvalue.html"))
	var ce *collector.CollectorError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 CollectorError，got %v", err)
	}
	if ce.Code != collector.CodeStructure {
		t.Errorf("错误码 = %s, want %s", ce.Code, collector.CodeStructure)
	}
}

func TestParsePageFeatures(t *testing.T) {
	_, err := collector.ParseWeekHTML(readFixture(t, "page_login.html"))
	var ce *collector.CollectorError
	if !errors.As(err, &ce) || ce.Code != collector.CodeLoginPage {
		t.Errorf("登录页特征应报 %s，got %v", collector.CodeLoginPage, err)
	}
	_, err = collector.ParseWeekHTML(readFixture(t, "page_illegal.html"))
	if !errors.As(err, &ce) || ce.Code != collector.CodeIllegalAccess {
		t.Errorf("非法访问特征应报 %s，got %v", collector.CodeIllegalAccess, err)
	}
}

func testConfig() *collector.Config {
	return &collector.Config{Version: 1, Groups: []collector.Group{
		{ID: "zh", Name: "综合教学楼", Enabled: true, Order: 1, IncludePrefixes: []string{"综合教学楼"}},
		{ID: "ws", Name: "文史楼", Enabled: true, Order: 2, IncludePrefixes: []string{"文史楼"}},
		{ID: "lws", Name: "老文史楼", Enabled: true, Order: 3, IncludePrefixes: []string{"老文史楼"}},
		{ID: "ja", Name: "JA", Enabled: true, Order: 4, IncludePrefixes: []string{"JA"}},
		{ID: "test", Name: "测试楼", Enabled: true, Order: 5,
			IncludePrefixes: []string{"测试楼"},
			ExcludePrefixes: []string{"测试楼-"}, ExcludeExactNames: []string{"测试楼禁"}},
		{ID: "other", Name: "其它", Enabled: true, Order: 6,
			IncludePrefixes: []string{"其它"}, IncludeExactNames: []string{"特批室"}},
		{ID: "disabled", Name: "停用", Enabled: false, Order: 7, IncludePrefixes: []string{"停用楼"}},
	}}
}

func TestGroupRules(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		name string
		want string
	}{
		{"综合教学楼101", "zh"},
		{"文史楼201", "ws"},
		{"老文史楼301", "lws"}, // 前缀匹配不把 老文史楼 混入 文史楼
		{"JA101", "ja"},
		{"测试楼101", "test"},
		{"测试楼-101", ""}, // 排除前缀优先
		{"测试楼禁", ""},    // 精确排除
		{"其它实验楼", "other"},
		{"特批室", "other"}, // 精确包含
		{"停用楼1", ""},     // 停用分组不参与
		{"体育馆B1", ""},    // 未命中 → 不发布
	}
	for _, c := range cases {
		got, err := cfg.ResolveRoomGroup(c.name)
		if err != nil {
			t.Errorf("ResolveRoomGroup(%q) 意外错误: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("ResolveRoomGroup(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestGroupAmbiguousFails(t *testing.T) {
	cfg := testConfig()
	// 房间同时命中 ja（前缀 JA）与 other（精确 JA101）→ 禁止静默裁决。
	cfg.Groups = append(cfg.Groups, collector.Group{ID: "dup", Name: "重复", Enabled: true, Order: 8,
		IncludeExactNames: []string{"JA101"}})
	if _, err := cfg.ResolveRoomGroup("JA101"); err == nil {
		t.Fatal("多分组命中应报错")
	} else if ce := new(collector.CollectorError); !errors.As(err, &ce) || ce.Code != collector.CodeGroupAmbiguous {
		t.Errorf("期望 group_ambiguous，got %v", err)
	}

	// 经 ResolveSnapshotRooms 同样失败。
	rows := []collector.ParsedRow{{Jsbh: "0001", Name: "JA101"}}
	if _, _, err := collector.ResolveSnapshotRooms(cfg, rows); err == nil {
		t.Fatal("多分组命中应报错（ResolveSnapshotRooms）")
	}
}

func TestGroupZeroEnabledFails(t *testing.T) {
	cfg := &collector.Config{Version: 1, Groups: []collector.Group{
		{ID: "kong", Name: "空楼", Enabled: true, Order: 1, IncludePrefixes: []string{"空楼"}},
	}}
	rows := []collector.ParsedRow{{Jsbh: "0001", Name: "综合楼1"}}
	_, _, err := collector.ResolveSnapshotRooms(cfg, rows)
	var ce *collector.CollectorError
	if !errors.As(err, &ce) || ce.Code != collector.CodeGroupZero {
		t.Errorf("启用分组归零应报 group_zero，got %v", err)
	}
}

func TestNaturalLess(t *testing.T) {
	if !collector.NaturalLess("F9", "F126") {
		t.Error("F9 应排在 F126 之前（自然排序）")
	}
	if collector.NaturalLess("F126", "F9") {
		t.Error("F126 不应排在 F9 之前")
	}
	if !collector.NaturalLess("老文史楼2", "老文史楼10") {
		t.Error("老文史楼2 应排在 老文史楼10 之前")
	}
	if !collector.NaturalLess("A1", "A2") {
		t.Error("A1 < A2")
	}
}

func TestOCRLocalCommandOK(t *testing.T) {
	t.Setenv("OCR_CMD", "testdata/ocr/fakeocr.sh")
	got, err := cas.RecognizeCaptcha(context.Background(), []byte("fake-image-bytes"))
	if err != nil {
		t.Fatalf("本地 OCR 识别失败: %v", err)
	}
	if got != "k3x9" {
		t.Errorf("识别文本 = %q, want k3x9", got)
	}
}

func TestOCRLocalCommandFailures(t *testing.T) {
	t.Run("命令不存在", func(t *testing.T) {
		t.Setenv("OCR_CMD", "testdata/ocr/no-such-ocr.sh")
		if _, err := cas.RecognizeCaptcha(context.Background(), []byte("x")); err == nil ||
			!strings.Contains(err.Error(), "不存在") {
			t.Errorf("期望命令不存在错误，got %v", err)
		}
	})
	t.Run("执行失败", func(t *testing.T) {
		t.Setenv("OCR_CMD", "testdata/ocr/fakefail.sh")
		if _, err := cas.RecognizeCaptcha(context.Background(), []byte("x")); err == nil ||
			!strings.Contains(err.Error(), "执行失败") {
			t.Errorf("期望执行失败错误，got %v", err)
		}
	})
	t.Run("输出为空", func(t *testing.T) {
		t.Setenv("OCR_CMD", "testdata/ocr/fakeempty.sh")
		if _, err := cas.RecognizeCaptcha(context.Background(), []byte("x")); err == nil ||
			!strings.Contains(err.Error(), "输出为空") {
			t.Errorf("期望空输出错误，got %v", err)
		}
	})
}

func TestOCRLocalCommandTimeout(t *testing.T) {
	t.Setenv("OCR_CMD", "testdata/ocr/fakeslow.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := cas.RecognizeCaptcha(ctx, []byte("x")); err == nil {
		t.Fatal("超时应报错")
	}
}

func TestParseWeekCompositeStatus(t *testing.T) {
	// 同一格出现 ◆＋Ｊ 两个符号（上游双 <font>）→ 归并为复合占用 10，不阻断解析。
	rows, err := collector.ParseWeekHTML(readFixture(t, "week_composite.html"))
	if err != nil {
		t.Fatalf("复合状态不应导致解析失败: %v", err)
	}
	var found bool
	for _, row := range rows {
		for d := range 7 {
			for b := range 5 {
				if row.Blocks[d][b] == 10 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("fixture 应包含复合占用 10 的状态格")
	}
	if len(rows) == 0 {
		t.Fatal("应解析出至少一行")
	}
	if rows[0].Blocks[0][0] != 10 {
		t.Errorf("0899 首格应为复合占用 10, got %d", rows[0].Blocks[0][0])
	}
}
