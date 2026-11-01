package query

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// MetaRelease 是当前发布版本的对外形状：标识与生成时间，时间统一为 UTC。
type MetaRelease struct {
	ReleaseID   string    `json:"release_id"`
	GeneratedAt time.Time `json:"generated_at"`
}

// MetaReleaseFacts 是存储层给出的当前版本事实。
// Term 与 TotalWeeks 只用于向新鲜度服务要完整的学期区间，不进入响应。
type MetaReleaseFacts struct {
	ReleaseID   string
	GeneratedAt time.Time
	Term        string
	TotalWeeks  int
	DictVersion int
	AxisVersion int
}

// MetaDictStateFacts 是字典里一条状态的事实；状态键只在聚合成映射时使用。
type MetaDictStateFacts struct {
	StateKey  string
	Label     string
	Available bool
}

// MetaState 是字典里一条状态的对外形状。
type MetaState struct {
	Label     string `json:"label"`
	Available bool   `json:"available"`
}

// MetaDict 是字典版本与状态说明，states 以状态键为键。
type MetaDict struct {
	Version int                  `json:"version"`
	States  map[string]MetaState `json:"states"`
}

// MetaAxisNode 是节次轴上的一个节点；既是存储层事实，也是对外节点形状。
type MetaAxisNode struct {
	Node         string `json:"node"`          // 小节编号 '01'..'12'
	Ordinal      int    `json:"ordinal"`       // 小节顺序
	Block        string `json:"block"`         // 所属大节编码
	BlockOrdinal int    `json:"block_ordinal"` // 大节顺序
}

// MetaAxis 是轴版本、大节顺序与逐个小节节点。
type MetaAxis struct {
	Version int            `json:"version"`
	Blocks  []string       `json:"blocks"`
	Nodes   []MetaAxisNode `json:"nodes"`
}

// MetaBuilding 是白名单里的一栋教学楼；building_id 就是查询参数 building 的取值。
type MetaBuilding struct {
	BuildingID string `json:"building_id"`
	Name       string `json:"name"`
}

// MetaFacts 是存储层注入的事实快照；没有当前版本时 Release 为 nil。
type MetaFacts struct {
	Release    *MetaReleaseFacts
	DictStates []MetaDictStateFacts
	AxisNodes  []MetaAxisNode
	Buildings  []MetaBuilding
}

// MetaLookup 是存储层注入点；用函数类型让 query 不依赖 pgx 与具体仓储实现。
type MetaLookup func(ctx context.Context) (MetaFacts, error)

// MetaResult 是 /api/meta 的 data 载荷。
// 无当前版本时 release/dict/axis 为 null、weeks 为空数组、empty 为 true（design 空态约定）。
type MetaResult struct {
	Release   *MetaRelease    `json:"release"`
	Dict      *MetaDict       `json:"dict"`
	Axis      *MetaAxis       `json:"axis"`
	Buildings []MetaBuilding  `json:"buildings"`
	Weeks     []WeekFreshness `json:"weeks"`
	Empty     bool            `json:"empty"`
}

// MetaService 组装版本、字典、轴、白名单楼与逐周新鲜度。
// 新鲜度直接复用 FreshnessService，不在这里另写一套过期判定。
type MetaService struct {
	lookup    MetaLookup        // 版本与字典事实读取，由装配层注入
	freshness *FreshnessService // 逐周新鲜度，复用 9.6 的实现
}

// NewMeta 组装 meta 服务；freshness 由调用方用 NewFreshness 构造同一个服务。
func NewMeta(lookup MetaLookup, freshness *FreshnessService) *MetaService {
	return &MetaService{lookup: lookup, freshness: freshness}
}

// Meta 返回 meta 端点的完整载荷。
// 没有当前版本时按空态返回：不报错、不查新鲜度，白名单楼照常给出。
func (s *MetaService) Meta(ctx context.Context, now time.Time) (MetaResult, error) {
	if s.lookup == nil {
		return MetaResult{}, fmt.Errorf("meta 查询未装配")
	}

	facts, err := s.lookup(ctx)
	if err != nil {
		return MetaResult{}, err
	}

	result := MetaResult{
		Buildings: make([]MetaBuilding, 0, len(facts.Buildings)),
		Weeks:     make([]WeekFreshness, 0),
		Empty:     facts.Release == nil,
	}
	result.Buildings = append(result.Buildings, facts.Buildings...)

	// 无当前版本：设计约定返回 200 + release:null + empty:true，不返回 40401。
	if facts.Release == nil {
		return result, nil
	}

	// 生成时间统一 UTC：契约要求 RFC3339 UTC，存储层可能按会话时区回时刻。
	result.Release = &MetaRelease{
		ReleaseID:   facts.Release.ReleaseID,
		GeneratedAt: facts.Release.GeneratedAt.UTC(),
	}
	result.Dict = buildMetaDict(facts.Release.DictVersion, facts.DictStates)
	result.Axis = buildMetaAxis(facts.Release.AxisVersion, facts.AxisNodes)

	// 逐周状态覆盖当前版本所属学期的全部教学周：Requirement 17.2 只要求逐周发布状态，
	// 具体窗口由 9.8 的大屏按采集窗口自行取，这里不引入时钟依赖。
	if s.freshness == nil {
		return MetaResult{}, fmt.Errorf("新鲜度查询未装配")
	}
	weeks, err := s.freshness.Weeks(ctx, now, FreshnessParams{
		Term:     facts.Release.Term,
		FromWeek: 1,
		ToWeek:   facts.Release.TotalWeeks,
	})
	if err != nil {
		return MetaResult{}, err
	}
	result.Weeks = weeks

	return result, nil
}

// buildMetaDict 把字典状态列表折成「状态键 → 说明」的映射。
func buildMetaDict(version int, facts []MetaDictStateFacts) *MetaDict {
	states := make(map[string]MetaState, len(facts))
	for _, state := range facts {
		states[state.StateKey] = MetaState{Label: state.Label, Available: state.Available}
	}
	return &MetaDict{Version: version, States: states}
}

// buildMetaAxis 按小节顺序整形轴，并按首次出现顺序给出大节列表。
// 排序放在这里而不是只依赖 SQL 的 ORDER BY，存储层替身给出的乱序事实也能得到稳定形状。
func buildMetaAxis(version int, facts []MetaAxisNode) *MetaAxis {
	nodes := make([]MetaAxisNode, len(facts))
	copy(nodes, facts)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Ordinal < nodes[j].Ordinal })

	blocks := make([]string, 0, 5)
	seen := make(map[string]bool, 5)
	for _, node := range nodes {
		if seen[node.Block] {
			continue
		}
		seen[node.Block] = true
		blocks = append(blocks, node.Block)
	}

	return &MetaAxis{Version: version, Blocks: blocks, Nodes: nodes}
}
