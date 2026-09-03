package collector

import (
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// 容量片段：纯数字 (n/N) 或 （n/N）（NFKC 后全角括号/数字已半角化，
// 此处同时容忍残余全角形态以便直接作用于原始文本的子场景）。
var capacityPattern = regexp.MustCompile(`[（(][0-9０-９]+(?:[／/][0-9０-９]+)?[)）]`)

var spaceRunPattern = regexp.MustCompile(`\s+`)

// NormalizeRoomName 按决策 §6 规范化房间名称：
// NFKC → 反复删除纯数字容量片段 → 压缩清理首尾空格；其余括号（方位/校区）保留。
func NormalizeRoomName(raw string) string {
	s := norm.NFKC.String(raw)

	// 反复删除，直到不再变化（防御嵌套/重复形态）。
	for i := 0; i < 5; i++ {
		next := capacityPattern.ReplaceAllString(s, "")
		if next == s {
			break
		}
		s = next
	}

	s = spaceRunPattern.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
