package store

import (
	"reflect"
	"testing"
)

// TestParseBuildingWhitelist 覆盖白名单 JSON 的解析口径：
// 键不存在/空数组/非法 JSON 都退回空名单，空编号条目被丢弃，顺序保持不变。
func TestParseBuildingWhitelist(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []MetaBuilding
	}{
		{name: "空串", raw: "", want: []MetaBuilding{}},
		{name: "空数组", raw: "[]", want: []MetaBuilding{}},
		{name: "null", raw: "null", want: []MetaBuilding{}},
		{name: "非法 JSON", raw: "not-json", want: []MetaBuilding{}},
		{name: "不是数组", raw: `{"jxlbh":"Q10"}`, want: []MetaBuilding{}},
		{
			name: "正常条目保持顺序",
			raw:  `[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q11","name":"实验楼"}]`,
			want: []MetaBuilding{
				{ID: "Q10", Name: "综合教学楼"},
				{ID: "Q11", Name: "实验楼"},
			},
		},
		{
			name: "编号去空白且空编号丢弃",
			raw:  `[{"jxlbh":" Q10 ","name":"综合教学楼"},{"jxlbh":"   ","name":"空编号"},{"jxlbh":"Q11","name":"实验楼"}]`,
			want: []MetaBuilding{
				{ID: "Q10", Name: "综合教学楼"},
				{ID: "Q11", Name: "实验楼"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseBuildingWhitelist(c.raw); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("解析 %q 期望 %+v，实际 %+v", c.raw, c.want, got)
			}
		})
	}
}
