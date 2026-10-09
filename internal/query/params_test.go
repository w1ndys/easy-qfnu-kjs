// 本文件是 business 层的单元测试：参数解析与校验，不依赖数据库。

package query

import (
	"strings"
	"testing"
)

// TestParseParamsDefaults 校验省略参数时的默认值。
func TestParseParamsDefaults(t *testing.T) {
	params, err := ParseParams(RawParams{View: ViewAvailability})
	if err != nil {
		t.Fatalf("默认参数应当通过：%v", err)
	}

	// 契约默认起止节次是 01 与 11，对应 11 个节次
	if params.StartNode != "01" || params.EndNode != "11" {
		t.Errorf("默认节次 = %s..%s，期望 01..11", params.StartNode, params.EndNode)
	}
	if params.NodeCount != 11 {
		t.Errorf("默认节次数 = %d，期望 11", params.NodeCount)
	}
	// 契约默认 date_offset 0、limit 50、offset 0
	if params.DateOffset != 0 || params.Limit != 50 || params.Offset != 0 {
		t.Errorf("默认 date_offset/limit/offset = %d/%d/%d，期望 0/50/0", params.DateOffset, params.Limit, params.Offset)
	}
}

// TestParseParamsKeyword 校验关键词的 NFKC、去空白与长度上限。
func TestParseParamsKeyword(t *testing.T) {
	params, err := ParseParams(RawParams{View: ViewDay, Keyword: "  Ｆ１２６  "})
	if err != nil {
		t.Fatalf("全角关键词应当通过：%v", err)
	}
	// NFKC 之后全角字母数字变成半角，首尾空白去掉
	if params.Keyword != "F126" {
		t.Errorf("关键词 = %q，期望 F126", params.Keyword)
	}
	// 全天状态不解析节次，节次数保持 0
	if params.NodeCount != 0 {
		t.Errorf("day 视图的节次数 = %d，期望 0", params.NodeCount)
	}

	// 超过 32 个字符要按参数错误挡下
	if _, err := ParseParams(RawParams{View: ViewDay, Keyword: strings.Repeat("教", MaxKeywordRunes+1)}); err == nil {
		t.Error("超长关键词应当报参数错误")
	}
}

// TestParseParamsRejectsInvalid 逐项校验非法参数。
func TestParseParamsRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string    // 用例名称，失败时打出来
		raw  RawParams // 该用例传入的原始参数
	}{
		{"视图缺失", RawParams{}},
		{"视图非法", RawParams{View: "week"}},
		{"起点晚于终点", RawParams{View: ViewAvailability, StartNode: "05", EndNode: "03"}},
		{"节次越界", RawParams{View: ViewAvailability, StartNode: "13"}},
		{"节次非数字", RawParams{View: ViewAvailability, EndNode: "AB"}},
		{"date_offset 越界", RawParams{View: ViewDay, DateOffset: "11"}},
		{"date_offset 非数字", RawParams{View: ViewDay, DateOffset: "明天"}},
		{"limit 为零", RawParams{View: ViewDay, Limit: "0"}},
		{"offset 为负", RawParams{View: ViewDay, Offset: "-1"}},
	}

	for _, testCase := range cases {
		// 每个非法组合都必须报参数错误，前端才能提示用户改
		if _, err := ParseParams(testCase.raw); err == nil {
			t.Errorf("%s：期望参数错误，实际通过", testCase.name)
		}
	}
}

// TestParseParamsAcceptsBounds 校验边界值落在允许范围内。
func TestParseParamsAcceptsBounds(t *testing.T) {
	params, err := ParseParams(RawParams{
		View:       ViewAvailability,
		DateOffset: "10",
		StartNode:  "12",
		EndNode:    "12",
		Limit:      "200",
		Offset:     "0",
	})
	if err != nil {
		t.Fatalf("边界值应当通过：%v", err)
	}
	// 单节区间只要求这一个节次可用
	if params.NodeCount != 1 {
		t.Errorf("单节区间的节次数 = %d，期望 1", params.NodeCount)
	}
	if params.DateOffset != 10 || params.Limit != 200 {
		t.Errorf("边界值 = %d/%d，期望 10/200", params.DateOffset, params.Limit)
	}
}
