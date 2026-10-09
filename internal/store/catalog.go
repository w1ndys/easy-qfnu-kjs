// 本文件属于 data 层：读取学期日历、状态字典与节次轴这些目录数据。

package store

import (
	"context"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ListTerms 读出全部学期。
func (s *Store) ListTerms(ctx context.Context) ([]model.Term, error) {
	const sqlText = `SELECT term, total_weeks, timezone FROM term ORDER BY term`

	rows, err := s.pool.Query(ctx, sqlText)
	if err != nil {
		return nil, fmt.Errorf("查询学期失败: %w", err)
	}
	defer rows.Close()

	terms := make([]model.Term, 0, 4)
	for rows.Next() {
		var term model.Term
		if err := rows.Scan(&term.Term, &term.TotalWeeks, &term.Timezone); err != nil {
			return nil, fmt.Errorf("扫描学期失败: %w", err)
		}
		terms = append(terms, term)
	}
	// 遍历中途出错时不能当作读完，否则学期列表会不完整
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历学期失败: %w", err)
	}
	return terms, nil
}

// ListTermWeeks 读出全部学期周历；查询侧据此把日期换算成周次，不自己算日历。
func (s *Store) ListTermWeeks(ctx context.Context) ([]model.TermWeek, error) {
	const sqlText = `
SELECT term, week, monday, in_calendar
  FROM term_week
 ORDER BY term, week`

	rows, err := s.pool.Query(ctx, sqlText)
	if err != nil {
		return nil, fmt.Errorf("查询学期周历失败: %w", err)
	}
	defer rows.Close()

	weeks := make([]model.TermWeek, 0, 64)
	for rows.Next() {
		var week model.TermWeek
		if err := rows.Scan(&week.Term, &week.Week, &week.Monday, &week.InCalendar); err != nil {
			return nil, fmt.Errorf("扫描学期周历失败: %w", err)
		}
		weeks = append(weeks, week)
	}
	// 遍历中途出错时不能当作读完，否则日历会缺周
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历学期周历失败: %w", err)
	}
	return weeks, nil
}

// DictStates 读出某个字典版本的全部状态。
func (s *Store) DictStates(ctx context.Context, dictVersion int) ([]model.DictState, error) {
	const sqlText = `
SELECT state_key, label, available
  FROM dict_state
 WHERE dict_version = $1
 ORDER BY state_key`

	rows, err := s.pool.Query(ctx, sqlText, dictVersion)
	if err != nil {
		return nil, fmt.Errorf("查询状态字典失败: %w", err)
	}
	defer rows.Close()

	states := make([]model.DictState, 0, 16)
	for rows.Next() {
		var state model.DictState
		if err := rows.Scan(&state.StateKey, &state.Label, &state.Available); err != nil {
			return nil, fmt.Errorf("扫描状态字典失败: %w", err)
		}
		states = append(states, state)
	}
	// 遍历中途出错时不能当作读完，否则面板会少状态
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历状态字典失败: %w", err)
	}
	return states, nil
}

// AxisNodes 读出某个轴版本的 12 个节次节点，按展示顺序返回。
func (s *Store) AxisNodes(ctx context.Context, axisVersion int) ([]model.AxisNode, error) {
	const sqlText = `
SELECT axis_version, node, ordinal, block, block_ordinal
  FROM axis_node
 WHERE axis_version = $1
 ORDER BY ordinal`

	rows, err := s.pool.Query(ctx, sqlText, axisVersion)
	if err != nil {
		return nil, fmt.Errorf("查询节次轴失败: %w", err)
	}
	defer rows.Close()

	nodes := make([]model.AxisNode, 0, 12)
	for rows.Next() {
		var node model.AxisNode
		if err := rows.Scan(&node.AxisVersion, &node.Node, &node.Ordinal, &node.Block, &node.BlockOrdinal); err != nil {
			return nil, fmt.Errorf("扫描节次轴失败: %w", err)
		}
		nodes = append(nodes, node)
	}
	// 遍历中途出错时不能当作读完，否则前端拿不到完整节次
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节次轴失败: %w", err)
	}
	return nodes, nil
}
