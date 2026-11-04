package clean

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testAxis 是迁移 axis v1 种子（backend/migrations/0001_baseline.sql）的镜像：
// 5 个大节 → 12 个小节，节点顺序与 block_ordinal 一致。
func testAxis() []model.AxisNode {
	return []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
		{Node: "04", Ordinal: 4, Block: "030405", BlockOrdinal: 2},
		{Node: "05", Ordinal: 5, Block: "030405", BlockOrdinal: 2},
		{Node: "06", Ordinal: 6, Block: "0607", BlockOrdinal: 3},
		{Node: "07", Ordinal: 7, Block: "0607", BlockOrdinal: 3},
		{Node: "08", Ordinal: 8, Block: "0809", BlockOrdinal: 4},
		{Node: "09", Ordinal: 9, Block: "0809", BlockOrdinal: 4},
		{Node: "10", Ordinal: 10, Block: "101112", BlockOrdinal: 5},
		{Node: "11", Ordinal: 11, Block: "101112", BlockOrdinal: 5},
		{Node: "12", Ordinal: 12, Block: "101112", BlockOrdinal: 5},
	}
}

// orderedBlocks 按轴顺序返回去重后的大节编码，供测试按大节展开。
func orderedBlocks(axis []model.AxisNode) []string {
	blocks := make([]string, 0, len(axis))
	seen := make(map[string]bool, len(axis))

	for _, node := range axis {
		if seen[node.Block] {
			continue
		}
		seen[node.Block] = true
		blocks = append(blocks, node.Block)
	}

	return blocks
}

// TestExpandNodesMapsFiveBlocksToTwelve 锁住 Requirement 4.5 的主口径：
// 轴上 5 个大节依次展开后必须覆盖 12 个小节，且顺序与轴一致。
func TestExpandNodesMapsFiveBlocksToTwelve(t *testing.T) {
	blocks := []string{"0102", "030405", "0607", "0809", "101112"}
	want := []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12"}

	got := make([]string, 0, len(want))
	for _, block := range blocks {
		got = append(got, ExpandNodes(block)...)
	}

	if !slices.Equal(got, want) {
		t.Fatalf("5 大节应展开为 12 小节，期望 %v，实际 %v", want, got)
	}
}

// TestExpandNodesSplitsBlockByTwoDigitNodes 覆盖编码切分本身：
// 每个大节按两位一组给出小节编号，十位与个位不能被拆开；小节数由编码长度决定。
func TestExpandNodesSplitsBlockByTwoDigitNodes(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  []string
	}{
		{name: "两个小节", block: "0102", want: []string{"01", "02"}},
		{name: "三个小节", block: "030405", want: []string{"03", "04", "05"}},
		{name: "两位数的节点整体切出", block: "101112", want: []string{"10", "11", "12"}},
		{name: "六个小节", block: "010203040506", want: []string{"01", "02", "03", "04", "05", "06"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandNodes(tc.block); !slices.Equal(got, tc.want) {
				t.Fatalf("展开结果不符，期望 %v，实际 %v（大节编码 %q）", tc.want, got, tc.block)
			}
		})
	}
}

// TestExpandNodesRejectsMalformedBlock 锁住坏轴兜底：空编码、奇数长度、
// 非 ASCII 数字（包括全角数字与空格）都返回 nil，由调用方判轴不可用，不猜节点。
func TestExpandNodesRejectsMalformedBlock(t *testing.T) {
	cases := []struct {
		name  string
		block string
	}{
		{name: "空编码", block: ""},
		{name: "奇数长度", block: "010"},
		{name: "含字母", block: "01A2"},
		{name: "全角数字不猜", block: "０１０２"},
		{name: "含空格", block: "01 02"},
		{name: "含斜杠", block: "01/02"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandNodes(tc.block); got != nil {
				t.Fatalf("坏编码应返回 nil，实际 %v（大节编码 %q）", got, tc.block)
			}
		})
	}
}

// TestExpandNodesFollowsAxisNotFixedTwelve 锁住「按轴数据展开，不写死 12」：
// 换成 4 大节 8 小节的轴，展开结果必须随轴变成 8 个节点。
func TestExpandNodesFollowsAxisNotFixedTwelve(t *testing.T) {
	axis := []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "0304", BlockOrdinal: 2},
		{Node: "04", Ordinal: 4, Block: "0304", BlockOrdinal: 2},
		{Node: "05", Ordinal: 5, Block: "0506", BlockOrdinal: 3},
		{Node: "06", Ordinal: 6, Block: "0506", BlockOrdinal: 3},
		{Node: "07", Ordinal: 7, Block: "0708", BlockOrdinal: 4},
		{Node: "08", Ordinal: 8, Block: "0708", BlockOrdinal: 4},
	}

	want := []string{"01", "02", "03", "04", "05", "06", "07", "08"}
	got := make([]string, 0, len(want))
	for _, block := range orderedBlocks(axis) {
		got = append(got, ExpandNodes(block)...)
	}

	if !slices.Equal(got, want) {
		t.Fatalf("4 大节轴应展开为 8 小节，期望 %v，实际 %v", want, got)
	}
}

// TestExpandNodesMatchesAxisBlockMembership 用轴数据交叉验证：
// 每个大节编码展开出的小节，必须恰好是轴里归属该大节的节点，顺序也一致。
func TestExpandNodesMatchesAxisBlockMembership(t *testing.T) {
	axis := testAxis()
	wantByBlock := make(map[string][]string, len(axis))
	for _, node := range axis {
		wantByBlock[node.Block] = append(wantByBlock[node.Block], node.Node)
	}

	for block, want := range wantByBlock {
		if got := ExpandNodes(block); !slices.Equal(got, want) {
			t.Fatalf("大节 %q 展开结果与轴不符，期望 %v，实际 %v", block, want, got)
		}
	}
}

// TestExpandNodesCoversEveryAxisNode 锁住 Requirement 4.5 的覆盖口径：
// 5 大节展开后正好覆盖轴上的 12 个小节，不重不漏，也不冒出轴外节点。
//
// 这里不断言「同一大节内各小节状态一致」：ExpandNodes 只做编码切分，看不到状态；
// 状态一致是 pipeline 的职责——每个大节只清洗一次状态原文，再把同一份 Observation
// 写到展开出的每个小节上。那层投影函数落地后由 pipeline 测试覆盖。
func TestExpandNodesCoversEveryAxisNode(t *testing.T) {
	axis := testAxis()
	want := make(map[string]bool, len(axis))
	for _, node := range axis {
		want[node.Node] = true
	}

	covered := make(map[string]bool, len(axis))
	for _, block := range orderedBlocks(axis) {
		for _, node := range ExpandNodes(block) {
			if covered[node] {
				t.Fatalf("小节 %q 被多个大节重复展开（大节 %q）", node, block)
			}
			covered[node] = true
		}
	}

	if len(covered) != len(axis) {
		t.Fatalf("展开后应覆盖 %d 个小节，实际 %d 个", len(axis), len(covered))
	}

	for node := range want {
		if !covered[node] {
			t.Fatalf("小节 %q 没有被任何大节展开覆盖", node)
		}
	}
	for node := range covered {
		if !want[node] {
			t.Fatalf("展开出轴外节点 %q", node)
		}
	}
}

// TestExpandNodesSplitsEveryEvenASCIIBlock 是 ExpandNodes 的属性测试：
// 偶数长度且全为 ASCII 数字的编码，切成 len(block)/2 个两位小节且拼回原串；
// 奇数长度或含非 ASCII 数字时返回 nil，由调用方判轴不可用，不猜节点。
func TestExpandNodesSplitsEveryEvenASCIIBlock(t *testing.T) {
	property := func(code blockCode) bool {
		block := string(code)
		got := ExpandNodes(block)

		if !isEvenASCIIDigits(block) {
			return got == nil
		}
		if len(got) != len(block)/2 || strings.Join(got, "") != block {
			return false
		}
		for _, node := range got {
			if len(node) != 2 {
				return false
			}
		}

		return true
	}

	if err := quick.Check(property, propertyConfig()); err != nil {
		t.Fatalf("ExpandNodes 必须按两位一组切分: %v", err)
	}
}

// isEvenASCIIDigits 是属性测试侧的判据：空串、奇数长度、含非 ASCII 数字都不算可展开。
func isEvenASCIIDigits(block string) bool {
	if block == "" || len(block)%2 != 0 {
		return false
	}
	for i := 0; i < len(block); i++ {
		if block[i] < '0' || block[i] > '9' {
			return false
		}
	}

	return true
}

// blockCode 生成数字串，并在约三分之一样本里把一位换成非 ASCII 数字
// （字母、全角数字、空格、斜杠、负号、人），覆盖「坏编码返回 nil」的分支；
// 其余样本保持全数字，偶数与奇数长度都由长度随机覆盖。
type blockCode string

// Generate 实现 testing/quick.Generator；size 由 quick 传入，这里只用来限制串长。
func (blockCode) Generate(rand *rand.Rand, size int) reflect.Value {
	length := rand.Intn(min(size, 8) + 1)

	var builder strings.Builder
	for i := 0; i < length; i++ {
		builder.WriteByte(byte('0' + rand.Intn(10)))
	}

	block := builder.String()
	if length > 0 && rand.Intn(3) == 0 {
		junk := []string{"A", "０", " ", "/", "-", "人"}
		pos := rand.Intn(length)
		block = block[:pos] + junk[rand.Intn(len(junk))] + block[pos+1:]
	}

	return reflect.ValueOf(blockCode(block))
}
