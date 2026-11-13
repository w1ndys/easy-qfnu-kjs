package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// CollectSnapshot 是一轮采集开跑前要钉住的字典与轴快照：版本号原样带进发布
// （release.dict_version / axis_version），内容交给 clean 投影、parser 解表头。
// 四部分由同一次调用取回，避免调用方各读一次导致版本与内容错配。
type CollectSnapshot struct {
	DictVersion int              // dict 表的最大版本
	Dict        model.Dict       // 该版本的符号表与状态表
	AxisVersion int              // axis 表的最大版本
	Axis        []model.AxisNode // 该版本的全部节点，按 ordinal 升序
}

// snapshotQuerier 只取字典与轴快照需要的两种能力，便于用替身测试。
type snapshotQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// SnapshotRepo 是采集快照的只读仓储；*pgxpool.Pool 直接满足 snapshotQuerier。
type SnapshotRepo struct{ q snapshotQuerier }

// NewSnapshotRepo 把查询器包成快照仓储。
func NewSnapshotRepo(q snapshotQuerier) SnapshotRepo {
	return SnapshotRepo{q: q}
}

// 写入侧的不变量：版本行（dict / axis）必须与它这一版的内容行在同一个事务里提交。
// 读侧正是靠这条不变量才敢「先读版本号、再按版本号读内容」而不用开事务：提交要么
// 让整版一起可见，要么整版都还看不见。目前还没有运行时写入方——两套种子由 0001 写进
// 同一个迁移事务（store.Migrate 的全部脚本共用一个事务），所以读侧不加事务是正确的；
// 将来新增字典/轴的导入入口时必须守住「同事务写版本行与内容行」，否则读侧会读到
// 「版本行已提交、内容行未提交」，把一次进行中的写入误判成空表。那时要改的是写入方，
// 而不是靠给这里补一个可重复读事务来兜底。
//
// 版本表只追加：最大版本就是当前生效版本。用 ORDER BY version DESC LIMIT 1
// 而不是 max(version)，是为了让字典与轴共用同一种「取一行」的读取形状。
const (
	latestDictVersionSQL = `SELECT version FROM dict ORDER BY version DESC LIMIT 1`
	latestAxisVersionSQL = `SELECT version FROM axis ORDER BY version DESC LIMIT 1`
)

// snapshotDictStateSQL 读指定字典版本的状态定义。可用性只取 available，
// 采集侧不在代码里另立「哪些状态算空闲」的判断（Requirement 4.4、CP4）。
const snapshotDictStateSQL = `
SELECT state_key, label, available
  FROM dict_state
 WHERE dict_version = $1`

// snapshotDictSymbolSQL 读指定字典版本的符号表（符号原文 → 语义键）。
const snapshotDictSymbolSQL = `
SELECT symbol, state_key
  FROM dict_symbol
 WHERE dict_version = $1`

// snapshotAxisNodeSQL 读指定轴版本的节点。排序放在 SQL 里：parser 解表头时
// 直接按小节顺序消费，调用方不必再排一次。ordinal 没有唯一约束，并列时再按
// node 排，否则并列行的先后由数据库自己定，同一份数据两次读出的顺序可能不同。
const snapshotAxisNodeSQL = `
SELECT node, ordinal, block, block_ordinal
  FROM axis_node
 WHERE axis_version = $1
 ORDER BY ordinal, node`

// LoadCollectSnapshot 取 dict / axis 的最大版本与各自的内容。
// 任一张版本表没有版本行、或版本行在而它这一版的内容表为空，都返回包装
// model.ErrQueryFailed 的错误，而不是空快照：空字典会把每一格都判成 unknown，
// 空轴会让 parser.Rows 直接 ErrAxisInvalid，两者都该在发出第一个上游请求之前
// 失败（design「字典与轴快照的读取入口」）。
func (r SnapshotRepo) LoadCollectSnapshot(ctx context.Context) (CollectSnapshot, error) {
	dictVersion, err := latestSnapshotVersion(ctx, r.q, latestDictVersionSQL, "字典")
	if err != nil {
		return CollectSnapshot{}, err
	}
	axisVersion, err := latestSnapshotVersion(ctx, r.q, latestAxisVersionSQL, "轴")
	if err != nil {
		return CollectSnapshot{}, err
	}

	states, err := snapshotDictStates(ctx, r.q, dictVersion)
	if err != nil {
		return CollectSnapshot{}, err
	}
	symbols, err := snapshotDictSymbols(ctx, r.q, dictVersion)
	if err != nil {
		return CollectSnapshot{}, err
	}
	axis, err := snapshotAxisNodes(ctx, r.q, axisVersion)
	if err != nil {
		return CollectSnapshot{}, err
	}

	return CollectSnapshot{
		DictVersion: dictVersion,
		Dict:        model.Dict{Symbols: symbols, States: states},
		AxisVersion: axisVersion,
		Axis:        axis,
	}, nil
}

// latestSnapshotVersion 读一张版本表的最大版本。查不到行说明该表是空的，
// 按读取失败上报而不是回一个 0：0 不是合法版本，放过去只会在更晚的位置炸。
func latestSnapshotVersion(ctx context.Context, q snapshotQuerier, sql, what string) (int, error) {
	var version int
	err := q.QueryRow(ctx, sql).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s表没有版本行，无法采集", model.ErrQueryFailed, what)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: 读取%s最大版本失败: %w", model.ErrQueryFailed, what, err)
	}
	return version, nil
}

// snapshotDictStates 读指定版本的字典状态表。版本行存在但状态表整表为空说明这一版
// 是坏的：空状态表会让 clean 把每一格都投影成 unknown。这与「单条语义键缺行按未
// 收录处理」是两回事——整表为空是数据缺陷，该在发请求之前失败。
func snapshotDictStates(
	ctx context.Context, q snapshotQuerier, version int,
) (map[string]model.DictState, error) {
	rows, err := q.Query(ctx, snapshotDictStateSQL, version)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询字典状态失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	states := make(map[string]model.DictState)
	for rows.Next() {
		var key string
		var state model.DictState
		if err := rows.Scan(&key, &state.Label, &state.Available); err != nil {
			return nil, fmt.Errorf("%w: 扫描字典状态失败: %w", model.ErrQueryFailed, err)
		}
		states[key] = state
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历字典状态失败: %w", model.ErrQueryFailed, err)
	}
	if len(states) == 0 {
		return nil, fmt.Errorf(
			"%w: dict_state表在字典版本 %d 下没有内容行，无法采集", model.ErrQueryFailed, version)
	}
	return states, nil
}

// snapshotDictSymbols 读指定版本的符号表。同一版本的符号是主键，重复即库故障，
// 这里按后写覆盖处理，不额外维护一份去重表。整表为空同样按读取失败上报：
// 空符号表会让每一格都判成未知，和没有字典一样不可用于采集。
func snapshotDictSymbols(
	ctx context.Context, q snapshotQuerier, version int,
) (map[string]string, error) {
	rows, err := q.Query(ctx, snapshotDictSymbolSQL, version)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询字典符号失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	symbols := make(map[string]string)
	for rows.Next() {
		var symbol, stateKey string
		if err := rows.Scan(&symbol, &stateKey); err != nil {
			return nil, fmt.Errorf("%w: 扫描字典符号失败: %w", model.ErrQueryFailed, err)
		}
		symbols[symbol] = stateKey
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历字典符号失败: %w", model.ErrQueryFailed, err)
	}
	if len(symbols) == 0 {
		return nil, fmt.Errorf(
			"%w: dict_symbol表在字典版本 %d 下没有内容行，无法采集", model.ErrQueryFailed, version)
	}
	return symbols, nil
}

// snapshotAxisNodes 读指定版本的节次轴。行序由 SQL 的 ORDER BY ordinal, node 决定，
// 这里不再二次排序：排序若在代码里再做一遍，只会掩盖 SQL 漏写排序的问题。
// 整表为空按读取失败上报：parser.Rows 拿到空轴会直接 ErrAxisInvalid，
// 早报错能少走一次上游请求。
func snapshotAxisNodes(
	ctx context.Context, q snapshotQuerier, version int,
) ([]model.AxisNode, error) {
	rows, err := q.Query(ctx, snapshotAxisNodeSQL, version)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询节次轴失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	nodes := make([]model.AxisNode, 0)
	for rows.Next() {
		var node model.AxisNode
		if err := rows.Scan(&node.Node, &node.Ordinal, &node.Block, &node.BlockOrdinal); err != nil {
			return nil, fmt.Errorf("%w: 扫描节次轴失败: %w", model.ErrQueryFailed, err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历节次轴失败: %w", model.ErrQueryFailed, err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf(
			"%w: axis_node表在轴版本 %d 下没有内容行，无法采集", model.ErrQueryFailed, version)
	}
	return nodes, nil
}
