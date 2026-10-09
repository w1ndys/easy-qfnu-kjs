// 本文件属于 business 层：按状态字典把格子原文归成语义键。
// 规则见 docs/contract/data-format.v2.md 的 classify 层与状态字典种子值。

package clean

import (
	"fmt"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// classifier 是这次清洗用的分类表：整词查表用符号表，判断复合用字符集合。
// 符号的码点原样使用，不得规范化成 ASCII：Ｊ/Ｘ/Ｇ/Ｌ 是全角拉丁字母，Κ 是希腊大写 Kappa。
type classifier struct {
	symbols map[string]model.StateKey // 符号原文 → 状态键
	runes   map[rune]bool             // 出现在符号表里的字符，用来判断复合
}

// newClassifier 用字典里的符号建分类表；符号表为空说明读到的字典版本不对。
func newClassifier(symbols []model.DictSymbol) (classifier, error) {
	// 没有符号表会把整页判成未知，属于我们自己的数据问题，按结构失败返回
	if len(symbols) == 0 {
		return classifier{}, fmt.Errorf("%w: 状态字典里没有符号", ErrStructure)
	}

	c := classifier{
		symbols: make(map[string]model.StateKey, len(symbols)),
		runes:   make(map[rune]bool, len(symbols)),
	}
	for _, symbol := range symbols {
		c.symbols[symbol.Symbol] = symbol.StateKey
		for _, r := range symbol.Symbol {
			c.runes[r] = true
		}
	}
	return c, nil
}

// classify 把一个格子的原文归成语义键。
// 空单元格是 free；整词命中字典就用它的状态键；整词未收录但每个字符都收录且不止一个字符、
// 或者由多个词组成，都算 composite；出现没收录的字符就是 unknown。
// unknown 与 composite 不可用，也都不阻断发布，原文留档供人工核对。
func (c classifier) classify(raw string) Cell {
	text := strings.TrimSpace(raw)
	// 空单元格是空闲，契约不对它登记符号
	if text == "" {
		return Cell{StateKey: model.StateFree}
	}
	// 整词命中（◆、Ｊ、空闲、完全空闲 …）直接用它的语义键
	if stateKey, ok := c.symbols[text]; ok {
		return Cell{StateKey: stateKey}
	}

	fields := strings.Fields(text)
	joined := strings.Join(fields, "")
	// 去掉空白后还剩不止一个字符，且每个字符都收录，才算若干符号的组合
	if c.allKnown(fields) && len([]rune(joined)) > 1 {
		return Cell{StateKey: model.StateComposite, RawText: text}
	}
	// 出现没收录的字符就是未知：不把未知当空闲，但要能看出原文是什么
	return Cell{StateKey: model.StateUnknown, RawText: text}
}

// allKnown 判断这些词里的每个字符都出现在符号表里。
func (c classifier) allKnown(fields []string) bool {
	// 一个词都没有说明原文只有空白，不该走到这里
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		for _, r := range field {
			// 有字符没收录，就无法断言它是若干已收录符号的组合
			if !c.runes[r] {
				return false
			}
		}
	}
	return true
}
