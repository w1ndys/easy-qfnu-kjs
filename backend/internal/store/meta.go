package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// MetaRelease 是当前发布版本的元信息：标识、生成时间、所属学期与总周数，
// 以及该版本使用的字典/轴版本号。
type MetaRelease struct {
	ID          string    // 发布版本标识
	Term        string    // 该版本所属学期
	GeneratedAt time.Time // 版本生成时间
	DictVersion int       // 该版本使用的字典版本
	AxisVersion int       // 该版本使用的轴版本
	TotalWeeks  int       // 该学期总周数，供逐周新鲜度取完整区间
}

// MetaDictState 是字典里一条状态的定义。
type MetaDictState struct {
	StateKey  string // 状态键，如 free
	Label     string // 展示名
	Available bool   // 是否算可用
}

// MetaAxisNode 是节次轴上的一个节点及其大节归属。
type MetaAxisNode struct {
	Node         string // 小节编号 '01'..'12'
	Ordinal      int    // 小节顺序
	Block        string // 所属大节编码，如 '0102'
	BlockOrdinal int    // 大节顺序
}

// MetaBuilding 是白名单里的一栋教学楼。
type MetaBuilding struct {
	ID   string // 楼编号，即查询参数 building 的取值
	Name string // 展示名
}

// MetaFacts 是 /api/meta 需要的一组事实：当前版本、字典、轴与白名单楼。
// 没有当前版本时 Release 为 nil；白名单与版本无关，照常返回。
type MetaFacts struct {
	Release    *MetaRelease
	DictStates []MetaDictState
	AxisNodes  []MetaAxisNode
	Buildings  []MetaBuilding
}

// metaQuerier 只取 meta 查询需要的两种能力，便于与驱动其余部分解耦。
type metaQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// metaReleaseSQL 取唯一的当前发布版本，并带出该学期的总周数。
// 没有当前版本属于正常空态（Requirement 18.4），由调用方判 ErrNoRows。
const metaReleaseSQL = `
SELECT r.release_id, r.term, r.generated_at, r.dict_version, r.axis_version, t.total_weeks
  FROM release r
  JOIN term t ON t.term = r.term
 WHERE r.is_current
 LIMIT 1`

// metaDictSQL 取指定版本的字典状态，按状态键排序保证输出稳定。
const metaDictSQL = `
SELECT state_key, label, available
  FROM dict_state
 WHERE dict_version = $1
 ORDER BY state_key`

// metaAxisSQL 取指定版本的节次节点，按小节顺序排序。
const metaAxisSQL = `
SELECT node, ordinal, block, block_ordinal
  FROM axis_node
 WHERE axis_version = $1
 ORDER BY ordinal`

// metaWhitelistSQL 读教学楼白名单；键不存在与空名单同义（Requirement 17.5）。
const metaWhitelistSQL = `SELECT value FROM settings WHERE key = 'building_whitelist'`

// metaWhitelistEntry 是白名单 JSON 里的一个元素，键名沿用上游楼编号 jxlbh 与展示名 name。
type metaWhitelistEntry struct {
	Jxlbh string `json:"jxlbh"`
	Name  string `json:"name"`
}

// Meta 读取当前版本、字典、轴与白名单教学楼。
// 没有当前版本时不报错：Release 为 nil，字典与轴留空，白名单照常返回。
func Meta(ctx context.Context, q metaQuerier) (MetaFacts, error) {
	buildings, err := metaBuildingWhitelist(ctx, q)
	if err != nil {
		return MetaFacts{}, err
	}

	facts := MetaFacts{
		DictStates: make([]MetaDictState, 0),
		AxisNodes:  make([]MetaAxisNode, 0),
		Buildings:  buildings,
	}

	release, err := metaCurrentRelease(ctx, q)
	if err != nil {
		return MetaFacts{}, err
	}
	// 无当前版本是契约里的空态，不是错误：白名单仍然可用于前端下拉。
	if release == nil {
		return facts, nil
	}
	facts.Release = release

	states, err := metaDictStates(ctx, q, release.DictVersion)
	if err != nil {
		return MetaFacts{}, err
	}
	facts.DictStates = states

	nodes, err := metaAxisNodes(ctx, q, release.AxisVersion)
	if err != nil {
		return MetaFacts{}, err
	}
	facts.AxisNodes = nodes

	return facts, nil
}

// metaCurrentRelease 读当前发布版本；没有时返回 nil 而不是错误。
func metaCurrentRelease(ctx context.Context, q metaQuerier) (*MetaRelease, error) {
	var release MetaRelease
	err := q.QueryRow(ctx, metaReleaseSQL).Scan(
		&release.ID, &release.Term, &release.GeneratedAt,
		&release.DictVersion, &release.AxisVersion, &release.TotalWeeks)
	// 空库或从未发布过时没有当前版本，交给调用方按空态处理。
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 查询当前发布版本失败: %w", model.ErrQueryFailed, err)
	}
	return &release, nil
}

// metaDictStates 读指定版本的字典状态。
func metaDictStates(ctx context.Context, q metaQuerier, version int) ([]MetaDictState, error) {
	rows, err := q.Query(ctx, metaDictSQL, version)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询字典状态失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	states := make([]MetaDictState, 0)
	for rows.Next() {
		var state MetaDictState
		if err := rows.Scan(&state.StateKey, &state.Label, &state.Available); err != nil {
			return nil, fmt.Errorf("%w: 扫描字典状态失败: %w", model.ErrQueryFailed, err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历字典状态失败: %w", model.ErrQueryFailed, err)
	}
	return states, nil
}

// metaAxisNodes 读指定版本的节次节点。
func metaAxisNodes(ctx context.Context, q metaQuerier, version int) ([]MetaAxisNode, error) {
	rows, err := q.Query(ctx, metaAxisSQL, version)
	if err != nil {
		return nil, fmt.Errorf("%w: 查询节次轴失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	nodes := make([]MetaAxisNode, 0)
	for rows.Next() {
		var node MetaAxisNode
		if err := rows.Scan(&node.Node, &node.Ordinal, &node.Block, &node.BlockOrdinal); err != nil {
			return nil, fmt.Errorf("%w: 扫描节次轴失败: %w", model.ErrQueryFailed, err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历节次轴失败: %w", model.ErrQueryFailed, err)
	}
	return nodes, nil
}

// metaBuildingWhitelist 读教学楼白名单：键不存在按空名单处理，不算错误。
func metaBuildingWhitelist(ctx context.Context, q metaQuerier) ([]MetaBuilding, error) {
	var raw string
	err := q.QueryRow(ctx, metaWhitelistSQL).Scan(&raw)
	// 管理员还没配过白名单时没有这一行，与空名单同义。
	if errors.Is(err, pgx.ErrNoRows) {
		return make([]MetaBuilding, 0), nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: 读取教学楼白名单失败: %w", model.ErrQueryFailed, err)
	}
	return parseBuildingWhitelist(raw), nil
}

// parseBuildingWhitelist 把 settings 里的 JSON 数组解析成白名单楼。
// 配置被写坏时退回空名单：meta 是公开只读端点，不能因为一条设置坏掉就整体 500；
// 空名单本身已有明确语义（采集受阻、下拉无选项）。
// 空编号条目直接丢弃：上游会把空 jxlbh 当成「不限楼」，等于拉全校。
func parseBuildingWhitelist(raw string) []MetaBuilding {
	var entries []metaWhitelistEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return make([]MetaBuilding, 0)
	}

	buildings := make([]MetaBuilding, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.Jxlbh)
		if id == "" {
			continue
		}
		buildings = append(buildings, MetaBuilding{ID: id, Name: entry.Name})
	}
	return buildings
}
