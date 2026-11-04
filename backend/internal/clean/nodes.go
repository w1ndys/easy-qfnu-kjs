package clean

// ExpandNodes 把一个大节编码展开成它包含的小节编号（Requirement 4.5）：
// 0102 → [01 02]，030405 → [03 04 05]，101112 → [10 11 12]。
//
// 大节编码来自轴数据（axis_node.block），本身就是小节编号按顺序的拼接，
// 所以按两位一组切开就是按轴展开；代码里不维护「5 大节 → 12 小节」的固定映射，
// 轴换成 4 大节、某个大节含 3 个小节，或将来出现别的组合时同样成立。
//
// 同一大节内各小节状态一致由调用方保证：每个大节只清洗一次状态原文，
// 再把同一份 Observation 写到 ExpandNodes 返回的每个小节上。
//
// 编码为空、长度为奇数或含非 ASCII 数字时返回 nil：说明轴数据坏了，
// 由调用方判轴不可用，不在这里猜节点，也不把 01..12 的范围写死。
func ExpandNodes(block string) []string {
	if block == "" || len(block)%2 != 0 {
		return nil
	}

	nodes := make([]string, 0, len(block)/2)
	for i := 0; i < len(block); i += 2 {
		node := block[i : i+2]
		if !isTwoDigitNode(node) {
			return nil
		}
		nodes = append(nodes, node)
	}

	return nodes
}

// isTwoDigitNode 判断两位编码是否都是 ASCII 数字。
// 只校验字形，不校验 01..12 的范围：小节范围由轴表约束，clean 不写死 12。
func isTwoDigitNode(node string) bool {
	return node[0] >= '0' && node[0] <= '9' && node[1] >= '0' && node[1] <= '9'
}
