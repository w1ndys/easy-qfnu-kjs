package store

import "testing"

// TestLikePattern 覆盖空关键词与 LIKE 元字符转义。
func TestLikePattern(t *testing.T) {
	cases := []struct {
		name    string
		keyword string
		want    string
	}{
		{name: "空关键词匹配全部", keyword: "", want: "%"},
		{name: "普通子串", keyword: "F126", want: "%F126%"},
		{name: "百分号按字面处理", keyword: "10%", want: `%10\%%`},
		{name: "下划线按字面处理", keyword: "10_", want: `%10\_%`},
		{name: "反斜杠按字面处理", keyword: `10\`, want: `%10\\%`},
		{name: "混合元字符", keyword: `10%_\`, want: `%10\%\_\\%`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := likePattern(c.keyword); got != c.want {
				t.Fatalf("likePattern(%q) = %q，期望 %q", c.keyword, got, c.want)
			}
		})
	}
}
