// Package clean 把 parser 解析出的原文做纯语义清洗：状态原文投影成语义键与可用性，
// 房名做全半角归一、容量片段删除与空白压缩，大节编码按轴展开成小节编号。
//
// 本包不读库、不发请求、不取时钟：字典由调用方注入（采集链路按 dict_state 与
// dict_symbol 读出后传进来），所以同一份原文在同一份字典下的结果恒定，
// 可以直接做表驱动与属性测试。这里只做投影，不拼 SQL、不写库
// （依赖方向见 specs/2026-11-03-collect-pipeline/design.md 与 backend/.golangci.yml）。
package clean

import (
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// State 把一格状态原文投影成语义键与可用性，并原样保留原文
// （Requirement 4.1–4.4）。判定顺序就是口径，不要调换：
//  1. 去掉首尾空白后为空 → free：空白格就是空闲，上游不会写「空闲」两个字；
//  2. 去掉首尾空白后整词等于某个字典符号 → 该符号的语义键；
//  3. 原文能被完整切成两个及以上已知符号 → composite，例如 ◆Ｊ；
//  4. 其余 → unknown，例如半角 J、全角 Ｍ 这类字形相近但不在字典里的写法。
//
// 可用性只按字典投影：composite 与 unknown 恒为 false（Requirement 4.4），
// 其余语义键取 dict.States 的 Available；字典缺该键时取 false，
// 不把缺数据当空闲。首尾空白只影响匹配，不影响 RawText。
//
// 匹配是逐字精确匹配，不做全半角或大小写归一：字典里写 Ｊ 就只认 Ｊ，
// 半角 J 属于未收录（写法差异由字典版本决定，不由代码兜底）。
func State(raw string, dict model.Dict) model.Observation {
	text := strings.TrimSpace(raw)
	key := classify(text, dict.Symbols)

	return model.Observation{
		StateKey:  key,
		Available: projectAvailable(key, dict.States),
		RawText:   raw,
	}
}

// classify 只负责原文到语义键的映射，不碰可用性，两条判断链互不复用，避免一处改动牵连另一处。
func classify(text string, symbols map[string]string) string {
	if text == "" {
		return model.StateKeyFree
	}

	if key, ok := symbols[text]; ok {
		// 空语义键是坏字典：整词命中也不能把空键带出去，
		// 否则会产出 StateKey="" 的观测，落库即非法值。按未收录处理。
		if key == "" {
			return model.StateKeyUnknown
		}

		return key
	}

	if knownSymbolCount(text, symbols) >= 2 {
		return model.StateKeyComposite
	}

	return model.StateKeyUnknown
}

// projectAvailable 是本包唯一的可用性判定入口：有且只有这里决定 available，
// 上层只读 Observation.Available，不再按语义键另行判断（CP4）。
//
// composite 与 unknown 在这里被强制置为不可用，即使字典把这两个键配成可用也一样：
// 这是 Requirement 4.4 的明文口径（未知与复合恒不可用），不是第二套状态表。
// 其余语义键一律取字典的 Available；字典缺该键时取 false，宁可算占用。
func projectAvailable(key string, states map[string]model.DictState) bool {
	if key == model.StateKeyComposite || key == model.StateKeyUnknown {
		return false
	}

	state, ok := states[key]
	return ok && state.Available
}

// knownSymbolCount 返回原文能被字典符号完整切分时最多切出几个符号，切不完整返回 0。
//
// 用动态规划而不是贪心：符号长度不一致（「空闲」是两个字符，Ｊ 是一个字符），
// 贪心按最长匹配在「短符号是长符号前缀」的组合上会先吃掉长符号，
// 把本可切完的原文切成不完整而漏判复合。这里只关心能不能切完与段数，
// 不关心具体切法，所以每个位置只保留最大段数。
func knownSymbolCount(text string, symbols map[string]string) int {
	counts := make([]int, len(text)+1)
	for i := range counts {
		counts[i] = -1
	}
	counts[0] = 0

	for start := 0; start <= len(text); start++ {
		if counts[start] < 0 {
			continue
		}

		for symbol := range symbols {
			// 空符号是坏字典，跳过，否则会原地无限自增段数。
			if symbol == "" || !strings.HasPrefix(text[start:], symbol) {
				continue
			}

			if end := start + len(symbol); counts[end] < counts[start]+1 {
				counts[end] = counts[start] + 1
			}
		}
	}

	if counts[len(text)] < 0 {
		return 0
	}

	return counts[len(text)]
}
