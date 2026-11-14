package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ReleaseSummary 是管理端版本列表的一行，对应 release 一行加上三个按版本聚合的计数。
//
// 顺序字段是 GeneratedAt 而不是「实际发布时刻」：release 表没有 released_at 列
// （0001 基线），版本先后的既有口径就是 generated_at（store/publish.go 的
// deleteOlderReleasesSQL 用它挑「上一版」）。ReleasedAt 从 collect_run 左连接补充，
// 只作展示，不参与排序。
//
// 三个计数都是小节/行粒度的快照值，不是详情里的「格」：
//   - WeekCount：该版有观测的周数，count(DISTINCT observation.week)。注意这与详情的
//     week_count 不是同一口径：详情的逐周语句内连接 axis_node，只有能折叠出大节的周
//     才成行，所以详情 week_count = 可折叠周数，可能小于这里的「有观测周数」；
//   - ObservationRows：该版观测行数（小节粒度），count(*)；
//   - Rooms：该版归属快照行数（release_room_building），不是全局教室目录的房号数。
type ReleaseSummary struct {
	ReleaseID       string     // 版本标识，release.release_id
	Term            string     // 学期，release.term
	GeneratedAt     time.Time  // 版本生成时间，也是列表排序键
	ReleasedAt      *time.Time // 发布时刻，collect_run.released_at；没有关联轮次时为 nil
	IsCurrent       bool       // 是否为公开查询读取的当前版本
	HasAttrConflict bool       // 该版是否有归属冲突
	WeekCount       int        // 列表口径：有观测的周数；详情口径是可折叠周数，见 ReleaseStats
	ObservationRows int64      // 观测行数（小节粒度）
	Rooms           int        // 归属快照房号数
	DictVersion     int        // 字典版本
	AxisVersion     int        // 轴版本
}

// ReleaseStats 是版本详情的总量，全部由详情的三条语句拼出，不再额外发聚合。
type ReleaseStats struct {
	ObservationRows int64 // 观测行数（小节粒度），逐周计数之和
	BlockCells      int   // 该版格数：房号 × 周 × 星期 × 大节，逐周计数之和
	WeekCount       int   // 可折叠周数，即 WeekCounts 的长度（逐周语句内连接 axis_node，见下）
	Rooms           int   // 归属快照房号数，取自头语句
	Buildings       int   // 归属快照覆盖的楼数，即 BuildingCounts 的长度
}

// ReleaseWeekCount 是某一周的覆盖面：观测行数与小节折叠后的大节格数。
type ReleaseWeekCount struct {
	Week            int   // 教学周次
	ObservationRows int64 // 该周观测行数（小节粒度）
	BlockCells      int   // 该周格数：房号 × 星期 × 大节
}

// ReleaseBuildingCount 是某一栋楼的归属快照房号数。
type ReleaseBuildingCount struct {
	BuildingID   string // 教学楼编号
	BuildingName string // 教学楼展示名，来自发布时的归属快照
	Rooms        int    // 该楼在本版的房号数
}

// ReleaseDetailView 是 GET /api/admin/releases/{release_id} 的数据：版本头、
// 总量、逐周覆盖与按楼分布。
//
// 名字不叫 ReleaseDetail：那个名字在本包已经被 publish.go 占用，指的是
// occupancy_detail 的一行（占用说明）。两者语义完全不同，这里不复用名字，
// 也不为了改名去动发布写侧的既有类型（见 design 的接口签名，实现时发现同名冲突）。
//
// WeekCounts 与 BuildingCounts 恒为非 nil 切片：没有观测或没有归属快照时是空切片，
// 序列化成 JSON 的 [] 而不是 null，前端不必区分两种「空」。
type ReleaseDetailView struct {
	Release        ReleaseSummary         // 版本头，WeekCount / ObservationRows 由逐周结果回填
	Stats          ReleaseStats           // 总量，由头语句与逐周/按楼结果拼出
	WeekCounts     []ReleaseWeekCount     // 按周次升序
	BuildingCounts []ReleaseBuildingCount // 按 building_id 升序
}

// ReleaseSide 是对比响应里一侧版本的元信息（版本头）。
//
// 只带「读一版头」就能定下来的东西：身份（ReleaseID / Term / GeneratedAt）、
// 折叠口径（AxisVersion，两侧可以不同）、展示口径（DictVersion）与两次发布之间的
// 状态标记（IsCurrent / HasAttrConflict）。不带 ReleasedAt 与各计数：那些属于版本
// 列表；对比只需要在同一快照里把两侧身份钉死，计数全部来自汇总语句。
type ReleaseSide struct {
	ReleaseID       string    // 版本标识，release.release_id
	Term            string    // 学期，release.term；两侧不同即 same_term=false
	GeneratedAt     time.Time // 版本生成时间，release.generated_at
	DictVersion     int       // 字典版本
	AxisVersion     int       // 轴版本，决定这一侧的小节 → 大节折叠口径
	IsCurrent       bool      // 是否为公开查询读取的当前版本
	HasAttrConflict bool      // 该版是否有归属冲突
}

// DiffCellsSummary 是三类格差异的计数，三者互斥且穷尽（CP4）。
//
// 格按「房号 × 周 × 星期 × 大节」计；同一格的大节内若干小节观测折叠成一行，
// 所以这里的数字是格数，不是 observation 行数。
type DiffCellsSummary struct {
	Added   int // 只在 to 出现的格
	Removed int // 只在 from 出现的格
	Changed int // 两侧都有但折叠后的 state_key 不同
	Total   int // Added + Removed + Changed，不是两版格数的并集大小
}

// DiffRoomsSummary 是归属快照的房号级差异，与格差异无关（房号可以没有任何观测）。
type DiffRoomsSummary struct {
	Added           int // 只在 to 的归属快照里
	Removed         int // 只在 from 的归属快照里
	Shared          int // 两侧都有
	BuildingChanged int // 两侧都有但 building_id 不同；这是 Shared 的子集，不另计一次
}

// StateDelta 是未知/复合格数在两侧的计数与变化量。
//
// From 与 To 各自是「该版折叠后 state_key 等于 unknown（或 composite）的格数」；
// Delta = To - From，交换对比方向时取反（CP5）。
type StateDelta struct {
	From  int // from 侧的格数
	To    int // to 侧的格数
	Delta int // To - From
}

// ReleaseDiffSummary 是一次对比的全部分类计数，一条汇总语句算完。
type ReleaseDiffSummary struct {
	Cells              DiffCellsSummary // 三类格差异
	Rooms              DiffRoomsSummary // 房号增删与归属变化
	Unknown            StateDelta       // 未知格两侧计数与变化量
	Composite          StateDelta       // 复合格两侧计数与变化量
	InconsistentBlocks int              // 块内状态不一致的格数（正常为 0，见 D4）
	SameTerm           bool             // 两侧 term 相同；不同也照常给差异（Requirement 3.4）
}

// DiffSection 是 GET /api/admin/releases/diff 的明细小节。
//
// 三个小节的数据源不同：cells 是两版观测折叠后的格差异（最重，一次两侧折叠），
// rooms / buildings 只读归属快照（release_room_building，几千行）。缺省是 cells
// （Requirement 4.1），由 api 层解析；仓储对零值同样按 cells 处理，直接调用不会
// 退化成「什么都不返回」。
type DiffSection string

// 三节小节的取值与请求参数一一对应；api 的参数门按这三个值判定合法性。
const (
	DiffSectionCells     DiffSection = "cells"     // 格差异：房号 × 周 × 星期 × 大节
	DiffSectionRooms     DiffSection = "rooms"     // 归属快照的房号增删
	DiffSectionBuildings DiffSection = "buildings" // 归属快照里换了楼的房号
)

// DiffKind 是明细小节的类别过滤，也是每一行的类别标记。
//
// 合法组合由 section 决定：cells 接受四个取值；rooms 没有 changed（房号要么存在
// 要么不存在）；buildings 只有 changed，但入参也接受 all（等价于不过滤，响应回显
// all）。组合校验在 api 层（Requirement 4.5），仓储只把 kind 原样交给 SQL 做等值
// 过滤，不在这里重写一遍组合矩阵。
type DiffKind string

// 四个类别取值；各小节允许的子集不同，见 DiffKind 的说明。
const (
	DiffKindAll     DiffKind = "all"     // 不做类别过滤
	DiffKindAdded   DiffKind = "added"   // 只在 to 侧
	DiffKindRemoved DiffKind = "removed" // 只在 from 侧
	DiffKindChanged DiffKind = "changed" // 两侧都有但值不同
)

// diffLimitDefault 是省略 limit 时的页大小。
//
// 1–200 的合法区间与 offset ≥ 0 由 api 参数门判定（Requirement 4.5，design
// 「管理 API 契约」的约束段）；仓储只处理「零值即缺省」这一种情况，不把越界值
// 悄悄改成合法值——那样会把上游参数 bug 藏起来。
const diffLimitDefault = 50

// ReleaseDiffParams 是一次对比请求的参数。
//
// From 与 To 的形状校验、两者是否相同、缺参，全部由 api 层在查库之前判定
// （Requirement 3.5，CP14），仓储只如实读两版；万一收到相同的两个版本，结果是一次
// 「全 same」的空差异（added/removed/changed 都是 0），不会报错，也不会把同一版
// 读两遍之外多做别的事。
//
// Section / Kind / Limit / Offset 决定明细小节的那一页：三个零值分别按 cells、all、
// 50 处理（见 normalized），offset 零值本来就是 0。合法的 section/kind 组合、
// limit 1–200 与 offset ≥ 0 由 api 在查库前挡掉（Requirement 4.5），仓储不重复校验。
type ReleaseDiffParams struct {
	From    string      // 对比基准（旧版本）的 release_id
	To      string      // 对比目标（新版本）的 release_id
	Section DiffSection // 明细小节，缺省 cells
	Kind    DiffKind    // 类别过滤，缺省 all
	Limit   int         // 页大小，缺省 50
	Offset  int         // 页偏移，从 0 起
}

// normalized 补齐三个缺省值：section → cells、kind → all、limit → 50。
//
// 只映射零值，不做区间校验也不纠正越界值：负数 limit、offset < 0、跨小节的 kind
// 组合都是 api 参数门要拒的输入（Requirement 4.5），仓储再拦一次只会出现两套
// 判定口径。
func (p ReleaseDiffParams) normalized() ReleaseDiffParams {
	if p.Section == "" {
		p.Section = DiffSectionCells
	}
	if p.Kind == "" {
		p.Kind = DiffKindAll
	}
	if p.Limit == 0 {
		p.Limit = diffLimitDefault
	}
	return p
}

// ReleaseDiff 是一次对比的结果：两侧版本头、汇总与所选小节的一页明细。
//
// 判断「有没有差异」看 Summary.Cells，不靠明细切片是否为空：明细为空既可能是真的
// 没有该类差异，也可能只是 offset 翻过了末页（CP6）。三个切片恒为非 nil：未选中的
// 小节省略成空切片，选中的小节没有行时也是空切片，序列化都输出 []。
//
// 这里不带 total / limit / offset / section / kind：total 由 DiffTotal 从 Summary
// 推导（同一快照，CP6），其余四项是请求参数的生效值，由 api 回显；仓储不重复存一份。
type ReleaseDiff struct {
	From      ReleaseSide        // from 侧版本头
	To        ReleaseSide        // to 侧版本头
	Summary   ReleaseDiffSummary // 分类汇总
	Cells     []DiffCellRow      // section=cells 时填充，否则为空切片
	Rooms     []DiffRoomRow      // section=rooms 时填充，否则为空切片
	Buildings []DiffBuildingRow  // section=buildings 时填充，否则为空切片
}

// DiffCellRow 是 section=cells 的一行：一个差异格与两侧折叠后的状态。
//
// FromState / ToState 是可空的：只在 to 侧出现的格没有 from_state，只在 from 侧
// 出现的格没有 to_state。FromBuilding / ToBuilding 只由「该房在该版有没有归属快照」
// 决定：SQL 按 (release_id, room_id) 左连接 release_room_building，不看格是否出现，
// 所以房号两版都有快照、格只在一侧时，另一侧的归属楼仍然有值；该房在该版没有归属
// 快照时才是 nil，响应输出 null（CP13）。
type DiffCellRow struct {
	Kind         DiffKind // added / removed / changed；分页永不返回 same
	RoomID       string   // 房号，observation.room_id
	RoomName     string   // 教室目录里的展示名，room.name；目录缺行时为空串
	Week         int      // 教学周次
	Weekday      int      // 星期，1–7
	Block        string   // 大节编码，axis_node.block
	FromState    *string  // from 侧折叠后的格状态；只在 to 侧时 nil
	ToState      *string  // to 侧折叠后的格状态；只在 from 侧时 nil
	FromBuilding *string  // from 侧归属楼编号；该房在 from 版没有归属快照时 nil（release_id + room_id 左连接，与格是否出现无关）
	ToBuilding   *string  // to 侧归属楼编号；该房在 to 版没有归属快照时 nil（release_id + room_id 左连接，与格是否出现无关）
}

// DiffRoomRow 是 section=rooms 的一行：归属快照里新增或消失的房号。
//
// 只有 added 与 removed 两种 kind（rooms 小节不过滤 shared）；新增房的 from_*
// 为 nil、消失房的 to_* 为 nil，四个归属字段成对出现。
type DiffRoomRow struct {
	Kind             DiffKind // added / removed
	RoomID           string   // 房号
	RoomName         string   // 教室目录里的展示名；目录缺行时为空串
	FromBuilding     *string  // from 侧楼编号；新增房为 nil
	ToBuilding       *string  // to 侧楼编号；消失房为 nil
	FromBuildingName *string  // from 侧楼展示名；新增房为 nil
	ToBuildingName   *string  // to 侧楼展示名；消失房为 nil
}

// DiffBuildingRow 是 section=buildings 的一行：两侧都有、但换了楼的房号。
//
// 四个归属字段都非空：这一节只取两版归属快照的交集且 building_id 不同，所以没有
// 「一侧没有」的形状。Kind 不在这里存：buildings 的等价类别恒为 changed，由 api
// 在 DTO 里回显（Requirement 4.3），仓储不必为每一行重复同一个常量。
type DiffBuildingRow struct {
	RoomID           string // 房号
	RoomName         string // 教室目录里的展示名；目录缺行时为空串
	FromBuilding     string // from 侧楼编号
	FromBuildingName string // from 侧楼展示名
	ToBuilding       string // to 侧楼编号
	ToBuildingName   string // to 侧楼展示名
}

// DiffTotal 从汇总推导分页的 total，与本次实际生效的 section / kind 对应。
//
// 明细分页语句不返回 count(*)：分页只取一页，再数一遍全量差异等于把最重的两侧
// 折叠多跑一次；而汇总与明细在同一个只读事务里，汇总的分类计数就是同一快照里的
// total（CP6）。映射关系：
//   - cells：all → total（added+removed+changed 之和），否则取对应一类；
//   - rooms：all → added+removed（shared 不在明细里），否则取对应一类；
//   - buildings：all 与 changed 等价，都取 building_changed（Requirement 4.3、4.4）。
//
// 未识别的组合返回 0 而不是猜一个类别：section/kind 的合法组合由 api 在查库前
// 校验（Requirement 4.5），仓储侧不会收到；真收到说明上游有 bug，让它以 0 暴露，
// 不要静默按 cells 或 all 解释。零值 section/kind 仍按缺省 cells / all 处理，
// 与 normalized 一致（直接调用仓储的用例也不必先补一遍默认值）。
func DiffTotal(section DiffSection, kind DiffKind, summary ReleaseDiffSummary) int {
	if section == "" {
		section = DiffSectionCells
	}
	if kind == "" {
		kind = DiffKindAll
	}

	switch section {
	case DiffSectionCells:
		switch kind {
		case DiffKindAll:
			return summary.Cells.Total
		case DiffKindAdded:
			return summary.Cells.Added
		case DiffKindRemoved:
			return summary.Cells.Removed
		case DiffKindChanged:
			return summary.Cells.Changed
		}
	case DiffSectionRooms:
		switch kind {
		case DiffKindAll:
			return summary.Rooms.Added + summary.Rooms.Removed
		case DiffKindAdded:
			return summary.Rooms.Added
		case DiffKindRemoved:
			return summary.Rooms.Removed
		}
	case DiffSectionBuildings:
		// buildings 只接受 all 与 changed，两者取同一份计数：这一节的行恒为
		// building_changed（Requirement 4.3）。
		if kind == DiffKindAll || kind == DiffKindChanged {
			return summary.Rooms.BuildingChanged
		}
	}
	return 0
}

// StateRatio 是某一类格（未知/复合）在该版里的格数与占比。
//
// Cells 按「房号 × 周 × 星期 × 大节」计数，同一大节展开的 2–3 个小节只算一格
// （D12 / CP11），与发布侧走 collect_run.unknown_cells / composite_cells 的口径同源。
// Ratio 以该版格数为分母、四舍五入到 4 位小数（Requirement 7.2）：两个值一起给，
// 调用方不必自己再算一遍除法，也不会出现「比例与格数对不上」的两种算法。
type StateRatio struct {
	Cells int     // 该类的格数（大节粒度）
	Ratio float64 // cells / 该版格数，4 位小数；分母为 0 时为 0
}

// ReleaseWindow 是该版关联发布轮次的采集窗口（教学周次闭区间）。
//
// 窗口属于轮次而不是版本：observation 不存窗口，collect_run 才是「这一轮抓了哪几周」
// 的事实来源。取 run_id 最大的一轮：正常一版只由一轮发布，手工修数据造成重复关联时
// 取最新一轮，结果仍然确定（与冲突清单取同一轮）。
//
// 没有关联轮次时 ReleaseAnomaly.Window 为 nil，响应输出 null；这与列表的 released_at
// 可空是同一件事。
type ReleaseWindow struct {
	Start int // 窗口起始周，collect_run.window_start
	End   int // 窗口结束周，collect_run.window_end
}

// ConflictRoom 是发布轮次里的一个房号级归属冲突：同一个房号落在两栋楼。
//
// BuildingID 是该轮采纳的楼，ConflictBuildingID 是被丢弃的另一栋；collect_run_room
// 的 CHECK 保证标了 conflict 的行两列都非空，因此这里用值类型而不是指针。
// 清单按 room_id 升序，每版最多 conflictRoomsLimit 条（Requirement 7.4）。
type ConflictRoom struct {
	RoomID             string // 房号
	BuildingID         string // 该轮采纳的教学楼
	ConflictBuildingID string // 与该房号冲突、被丢弃的教学楼
}

// ReleaseAnomaly 是 GET /api/admin/anomalies 里一个版本的一行（Requirement 7）。
//
// 计数口径与发布侧同源（D12）：先按各版自己的 axis_version 把小节折叠成大节再数，
// 不是小节粒度，所以管理员在运行详情里看到的 unknown/composite 与这里一致。
//
// 三处「空」都恒为非 nil 切片：ObservedWeeks / WeekGaps / ConflictRooms 没有内容时
// 是空切片，序列化成 JSON 的 [] 而不是 null（响应键集合是封闭的，值形状也要稳定）。
//
// Window 为 nil 表示该版没有可关联的发布轮次。ConflictRoomsTruncated 只在冲突数超过
// 每版上限时为 true，且此时 ConflictRooms 是 room_id 最小的那批——截断也不能悄悄换成
// 另一批房号。
type ReleaseAnomaly struct {
	ReleaseID              string         // 版本标识，release.release_id
	Term                   string         // 学期
	GeneratedAt            time.Time      // 版本生成时间
	IsCurrent              bool           // 是否为公开查询读取的当前版本
	HasAttrConflict        bool           // 该版是否标记了归属冲突
	Unknown                StateRatio     // 未知格数与占比
	Composite              StateRatio     // 复合格数与占比
	ObservedWeeks          []int          // 有观测的周次，升序
	WeekGaps               []int          // [min,max] 内没有观测的周，升序
	Window                 *ReleaseWindow // 关联轮次的采集窗口；没有轮次时 nil
	ConflictRooms          []ConflictRoom // room_id 最小的那批冲突房号，至多 conflictRoomsLimit 条
	ConflictRoomsTruncated bool           // 冲突数超过每版上限时为 true
}

// conflictRoomsLimit 是每个版本最多返回的冲突房号数（Requirement 7.4）。
//
// 冲突清单是排障线索，不是可翻页的明细：201 条与 20100 条对管理员没有区别，
// 反而会让异常视图的响应随数据量膨胀。SQL 侧按本常量加一取数（当前 rn <= 201）
// 才能区分「正好 200 条」与「超过 200 条」，Go 侧按 rn 是否超过本常量置截断标记。
const conflictRoomsLimit = 200

// ratioOf 返回 cells 占 blockCells 的比例，保留 4 位小数（四舍五入）。
//
// 分母为 0 时返回 0：空版本在发布侧不可达（发布门槛要求候选非空），这里仍按 0
// 处理，不让除零变成 NaN/Inf 混进响应。cells 为负也按 0 兜底——格数不可能为负，
// 真出现说明上游计数错，返回 0 比返回一个负比例更不容易被误读。
//
// 舍入全程用整数：先把比例放大成 q = floor(cells*1e4/blockCells)，再看余数是否达到
// 分母的一半（2*余数 >= 分母）决定进位。中点向上是 design 的边界口径，而浮点算不出
// 这个边界：57/800 = 0.07125、3/20000 = 0.00015 在二进制里都略小于真值，乘 1e4 后
// 是 712.4999… / 1.4999…，math.Round 会把它们舍成 0.0712 / 0.0001。整数除法与取余
// 没有这个误差，57/800 得 0.0713、3/20000 得 0.0002。
func ratioOf(cells int, blockCells int) float64 {
	if cells <= 0 || blockCells <= 0 {
		return 0
	}
	scaled := int64(cells) * 10000
	q := scaled / int64(blockCells)
	if 2*(scaled%int64(blockCells)) >= int64(blockCells) {
		q++
	}
	return float64(q) / 1e4
}

// weekGaps 返回 [min(observedWeeks), max(observedWeeks)] 内没有观测的周，按周次升序。
//
// 入参是某版有观测的周次集合（SQL 侧 array_agg(DISTINCT ... ORDER BY ...) 已去重升序）。
// 空切片返回空切片而不是 nil：响应里 week_gaps 是 []，调用方不必区分两种「空」。
//
// 不假设入参有序：纯函数不该依赖调用方的隐含约定，先扫一遍定 min/max，再用标记数组
// 记下已覆盖的周，最后按周次升序补出缺口。规模是 min..max 的跨度（教学周，≤ 学期
// 总周数），线性且只分配一次，比逐周回查切片或 SQL 的 generate_series 都好测。
func weekGaps(observedWeeks []int) []int {
	gaps := make([]int, 0)
	if len(observedWeeks) == 0 {
		return gaps
	}

	minimum, maximum := observedWeeks[0], observedWeeks[0]
	for _, week := range observedWeeks[1:] {
		if week < minimum {
			minimum = week
		}
		if week > maximum {
			maximum = week
		}
	}

	// 下标是 week - minimum：区间跨度由观测决定，不按学期总周数分配，也不依赖 week
	// 从 1 开始。跨度来自 smallint 周次，实际数据不超过几十。
	covered := make([]bool, maximum-minimum+1)
	for _, week := range observedWeeks {
		covered[week-minimum] = true
	}
	for week := minimum; week <= maximum; week++ {
		if !covered[week-minimum] {
			gaps = append(gaps, week)
		}
	}
	return gaps
}

// ErrReleaseNotFound 表示要读的发布版本不在库里。
//
// 对比时两侧任一缺失都返回它（消息里点名 from / to 与版本标识），api 映射
// 404 / 40401。它与 model.ErrQueryFailed 分开：缺版本是调用方传错或版本已被
// 发布事务删掉的正常失败，不是数据库故障，不能当 50000。
var ErrReleaseNotFound = errors.New("发布版本不存在")

// RollbackResult 是一次成功回退的结果：谁退位、谁上位、上位版本属于哪个学期。
//
// 只有提交成功才会走到返回：任一失败路径都返回零值 + 错误，调用方据此区分
// 「回退成功」与「什么都没发生」（Requirement 5.2 要求失败时原 current 不动）。
//
// CurrentReleaseID 是从行锁里读回的版本标识而不是直接回显入参——入参与库中主键
// 相等由 $1 命中保证，回显读回的值让「返回的版本确实是库里那一行」不依赖推断。
type RollbackResult struct {
	RolledBackFrom   string // 操作前的当前版本；走到成功路径时必然存在
	CurrentReleaseID string // 回退后的当前版本，即回退目标
	Term             string // 目标版本的学期，api 原样放进响应
}

// 回退的失败原因由哨兵区分，api 按 errors.Is 映射 40401 / 40904 / 40905 / 40906。
//
//   - ErrReleaseNotFound：目标版本不在库里（40401）。读侧对比复用同一个哨兵。
//   - ErrReleaseNotRollbackable：目标已是 current、库中没有 current，或目标在锁与
//     更新之间不再满足「非 current」（40904）。这三种都表示「这次回退没有意义」，
//     不是数据故障，所以不能压成 50000。
//   - ErrReleaseTermMismatch：目标与当前版本的 term 不同（40905）。current 是一版
//     完整数据集，跨学期切换等于让访客看到另一学期的课表，不是「回退」。
//   - ErrReleaseConflict：数据库层的并发冲突（SQLSTATE 23505 / 40P01 / 40001，
//     40906）。它表示「另一条版本变更语句与你撞上了」，重试即可；与「目标不可回退」
//     分开，调用方才能给出不同的提示（design D9）。
var (
	ErrReleaseNotRollbackable = errors.New("版本不可回退")
	ErrReleaseTermMismatch    = errors.New("回退目标与当前版本学期不同")
	ErrReleaseConflict        = errors.New("回退并发冲突")
)

// releaseAdminPool 是版本读取仓储需要的最小能力：多行读、单行读与只读事务。
//
// 用窄接口而不是 *pgxpool.Pool，与 publishQuerier / runStatusQuerier 同一写法：
// 单元测试可用替身验证错误口径，装配时交同一个连接池。BeginTx 是详情必需的：
// 头条、逐周计数与按楼计数要走同一个快照（发布事务会删更早版本，多语句读若跨
// 快照就会给出「头还在、明细已经没了」的半份结果）。
//
// QueryRow 是 design 为本类型声明的方法，当前四个读方法都用不到（列表只用 Query，
// 详情、对比与异常的读都在事务体内）。保留它是为了与 design 的接口形状一致：将来
// 若需要「不进事务的单行读」，不必再回来扩接口；回退的直接写入走 interface 之外。
type releaseAdminPool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseDetailQuerier 是详情事务体内需要的两种读能力；pgx.Tx 天然满足。
// 把事务体与开事务拆开，让「三条语句怎么拼出 stats」能用一个替身单独验证，
// 不必连库（与 publish.go 把 writeRelease 从 CommitRelease 拆出来同一理由）。
type releaseDetailQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseDiffQuerier 是对比事务体内需要的最小能力：单行读与多行读。
//
// 两侧版本头各一次 QueryRow，汇总也是单行聚合一次 QueryRow；明细小节的那一页用
// Query（一次至多 200 行）。所有读都走同一个 REPEATABLE READ 只读事务，汇总与
// 明细因此来自同一快照（CP7）。
type releaseDiffQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// releaseAnomalyQuerier 是异常视图事务体内需要的能力：两条多行读。
//
// 异常视图没有单行读：版本行由第一条语句整批给出，冲突清单由第二条语句整批给出，
// 两条都在同一个 REPEATABLE READ 只读事务里，版本集合与冲突清单因此来自同一快照
// （CP2 / CP7）。单独的窄接口让事务体可以用替身单独验证。
type releaseAnomalyQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// releaseRollbackTx 是回退事务体需要的最小能力：两条加锁单行读与两条 UPDATE。
//
// 与 releaseDetailQuerier / releaseDiffQuerier 同一写法：把事务体与开事务拆开，
// 让锁序、判定顺序与「先降后抬」能用一个记录型替身单独验证，不必连库。*pgx.Tx
// 天然满足；Commit 与 Rollback 留在外层 Rollback 方法里，事务体只负责读写。
type releaseRollbackTx interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// releaseListSQL 列出库中全部版本，按 generated_at 倒序、同刻按版本标识倒序。
//
// 排序键与发布事务的 deleteOlderReleasesSQL 完全一致：列表首条是生成时间最新的版本，
// 而「上一版」正是它后面那条。用 released_at 排序会让列表顺序与保留口径分叉
// （版本表没有那一列，也不打算加）。
//
// 三个计数各用一个 LEFT JOIN LATERAL 聚合，而不是平铺 LEFT JOIN 三张表：
// 平铺时 observation 与 release_room_building 的行会互相放大，count(*) 直接失真；
// LATERAL 让每个聚合各自压成一行，再按 release_id 贴回来。聚合在空集上仍返回一行
// （count 为 0、max 为 NULL），所以 LEFT JOIN 的外连接语义只体现在 NULL 的
// released_at 上，COALESCE 只是把「理论上不会 NULL」的三个计数写成确定值。
//
// released_at 取 max：正常一个版本只由一轮发布，手工修数据造成重复关联时 max 让它
// 仍是一行、而不是把版本行复制成多行；没有关联轮次时为 NULL（Requirement 1.3）。
//
// 不分页：release 表上限两行（current + previous），分页没有意义；total 由调用方
// 取 len。这里也不加 LIMIT，列表语义是「全部在库版本」。
const releaseListSQL = `
SELECT r.release_id, r.term, r.generated_at, r.dict_version, r.axis_version,
       r.has_attr_conflict, r.is_current,
       run.released_at,
       COALESCE(obs.observation_rows, 0), COALESCE(obs.week_count, 0),
       COALESCE(rooms.rooms, 0)
  FROM release r
  LEFT JOIN LATERAL (
        SELECT max(cr.released_at) AS released_at
          FROM collect_run cr
         WHERE cr.release_id = r.release_id
       ) run ON true
  LEFT JOIN LATERAL (
        SELECT count(*) AS observation_rows, count(DISTINCT o.week) AS week_count
          FROM observation o
         WHERE o.release_id = r.release_id
       ) obs ON true
  LEFT JOIN LATERAL (
        SELECT count(*) AS rooms
          FROM release_room_building b
         WHERE b.release_id = r.release_id
       ) rooms ON true
 ORDER BY r.generated_at DESC, r.release_id DESC`

// scanReleaseSummary 按 releaseListSQL 的列序把一行扫进 ReleaseSummary。
//
// 形参是「扫描函数」而不是 pgx.Row：Rows.Scan 与 Row.Scan 签名一致，将来详情或其他
// 读取路径复用同一列序时不会出现「一处加了列、另一处忘了加」的偏差（与
// scanRunSummary 同一写法）。
func scanReleaseSummary(scan func(dest ...any) error) (ReleaseSummary, error) {
	var summary ReleaseSummary
	err := scan(
		&summary.ReleaseID,
		&summary.Term,
		&summary.GeneratedAt,
		&summary.DictVersion,
		&summary.AxisVersion,
		&summary.HasAttrConflict,
		&summary.IsCurrent,
		&summary.ReleasedAt,
		&summary.ObservationRows,
		&summary.WeekCount,
		&summary.Rooms,
	)
	return summary, err
}

// releaseDetailHeadSQL 读版本头：release 列 + released_at + 归属快照房号数。
//
// 有意不带观测聚合：observation 是全库最大的一张表（一版几十万到上百万行），
// week_count / observation_rows / block_cells 全部由下面那条逐周计数算出，
// 头条再扫一遍同一个版本等于白付一次全表前缀扫。
//
// 归属快照只有几千行，用标量子查询即可，不必再开一个 LATERAL。
const releaseDetailHeadSQL = `
SELECT r.release_id, r.term, r.generated_at, r.dict_version, r.axis_version,
       r.has_attr_conflict, r.is_current, run.released_at,
       (SELECT count(*) FROM release_room_building b WHERE b.release_id = r.release_id) AS rooms
  FROM release r
  LEFT JOIN LATERAL (
        SELECT max(cr.released_at) AS released_at
          FROM collect_run cr
         WHERE cr.release_id = r.release_id
       ) run ON true
 WHERE r.release_id = $1`

// releaseWeekCountsSQL 按周扫一次观测，同时给出观测行数与大节格数。
//
// $2 是该版自己的 axis_version（从头语句读回）：小节 → 大节的映射随轴改版变化，
// 不能用「当前」轴版本去折叠历史版本。
//
// count(DISTINCT (o.room_id, o.weekday, an.block)) 是 PostgreSQL 的行构造去重：
// 同一格内的 2–3 个小节只算一个格（CP3）。周已经在 GROUP BY 里，所以去重键不必
// 再带 week。
//
// 内连接 axis_node 决定「详情 week_count = 可折叠周数」：节点的 block 映射按该版
// 轴版本给出，查不到映射的观测行会被整行滤掉——不只格数与观测行数可能小于列表的
// count(DISTINCT observation.week) 与 count(*)，连周都可能不成行。因此详情这里报的
// 周数是「能折叠出大节的周数」，不是「有观测的周数」；两种口径各自服务列表与详情，
// 不互相替代。
//
// 排序固定按周次升序：面板按周分组展示，行序不能跟着索引或计划漂移。
const releaseWeekCountsSQL = `
SELECT o.week,
       count(*) AS observation_rows,
       count(DISTINCT (o.room_id, o.weekday, an.block)) AS block_cells
  FROM observation o
  JOIN axis_node an ON an.axis_version = $2 AND an.node = o.node
 WHERE o.release_id = $1
 GROUP BY o.week
 ORDER BY o.week`

// releaseBuildingCountsSQL 按楼统计归属快照，排序按 building_id 升序。
//
// 分组键带 building_name：同一楼编号在快照里应当只有一个展示名，万一出现两个，
// 这里如实分成两行而不是用 min() 掩盖——那是数据异常，管理端应当看见。
const releaseBuildingCountsSQL = `
SELECT b.building_id, b.building_name, count(*) AS rooms
  FROM release_room_building b
 WHERE b.release_id = $1
 GROUP BY b.building_id, b.building_name
 ORDER BY b.building_id`

// releaseDiffHeadSQL 读一侧版本头：对比只需要身份与折叠口径，不碰观测与归属。
//
// 两次调用（from / to）而不是一条 `IN ($1, $2)`：缺哪一侧要能分别说清，错误消息
// 才带得出 from / to（design「错误处理」要求点名缺失的一侧）。两次读都在同一个
// REPEATABLE READ 只读事务里，所以两侧头来自同一快照——发布事务删掉较旧版本时，
// 不会出现「一侧读到、另一侧刚好被删」的半份对比（CP7）。
//
// 不带 released_at 与任何计数：对比响应不返回它们（两侧只需要 DictVersion /
// AxisVersion / IsCurrent / HasAttrConflict），多读一列就要多扫一次 collect_run。
const releaseDiffHeadSQL = `
SELECT release_id, term, generated_at, dict_version, axis_version,
       is_current, has_attr_conflict
  FROM release
 WHERE release_id = $1`

// releaseDiffFoldCTE 是两侧各自的「小节观测 → 大节格」折叠，参数固定为
// $1 = from 的 axis_version、$2 = from 的 release_id、$3 = to 的 axis_version、
// $4 = to 的 release_id；汇总与（第 3 项的）分页明细共用这一段的编号。
//
// 折叠键是 (room_id, week, weekday, block)，键在单个版本内唯一：observation 的
// 主键含 release_id 与 node，一个 node 只属于一个 block，term 在一个版本内是常量
// （CP3）。加周是 D3 的决定：状态逐周存储，只用房号 × 星期 × 大节压不成一个标量。
//
// 大节状态取 min(state_key)，同时取 max 用来判「块内不一致」：正常数据一个块内的
// 小节状态一致（发布侧展开保证），此时 min = max；出现不一致时仍有确定状态（min），
// 且该格被标记出来计入 inconsistent_blocks（D4），不静默吞掉。
//
// 两侧各用自己版本的 axis_version 折叠：轴改版后同一个 node 可能落到不同 block，
// 用「当前」轴去折历史版本会把旧版折成另一套格。两侧 axis_version 不同时仍按
// block 编码比较，语义变化由管理员从响应里的轴版本看出。
//
// j 的投影同时服务汇总与明细分页：汇总只数 kind / inconsistent，分页还要两侧折叠后
// 的 from_state / to_state 原样给管理员看。两处共用同一段 j，折叠口径只有一份。
const releaseDiffFoldCTE = `
WITH f AS (
  SELECT o.room_id, o.week, o.weekday, an.block,
         min(o.state_key) AS state_key,
         max(o.state_key) AS max_state
    FROM observation o
    JOIN axis_node an ON an.axis_version = $1 AND an.node = o.node
   WHERE o.release_id = $2
   GROUP BY o.room_id, o.week, o.weekday, an.block
), t AS (
  SELECT o.room_id, o.week, o.weekday, an.block,
         min(o.state_key) AS state_key,
         max(o.state_key) AS max_state
    FROM observation o
    JOIN axis_node an ON an.axis_version = $3 AND an.node = o.node
   WHERE o.release_id = $4
   GROUP BY o.room_id, o.week, o.weekday, an.block
), j AS (
  SELECT COALESCE(f.room_id, t.room_id) AS room_id,
         COALESCE(f.week, t.week)       AS week,
         COALESCE(f.weekday, t.weekday) AS weekday,
         COALESCE(f.block, t.block)     AS block,
         f.state_key AS from_state,
         t.state_key AS to_state,
         CASE WHEN f.room_id IS NULL THEN 'added'
              WHEN t.room_id IS NULL THEN 'removed'
              WHEN f.state_key <> t.state_key THEN 'changed'
              ELSE 'same' END AS kind,
         (f.max_state IS NOT NULL AND f.max_state <> f.state_key)
           OR (t.max_state IS NOT NULL AND t.max_state <> t.state_key) AS inconsistent
    FROM f
    FULL JOIN t USING (room_id, week, weekday, block)
)`

// releaseDiffSummarySQL 用一条语句算出全部汇总：三类格计数、不一致块、未知/复合
// 两侧计数、房号增删与归属变化。f / t 各只折叠一次（非递归 CTE 只物化一次），
// 汇总与（第 3 项的）分页明细共享同一段折叠，不重复扫两版各约几十万行观测。
//
// 每个 CTE 都压成恰好一行，最后 CROSS JOIN 拼成一行：cells 的四个 FILTER 在空集上
// 仍是 0，states 对空 UNION ALL 聚合仍出一行，rooms 对空 FULL JOIN 仍出一行。
//
// 未知/复合按「折叠后的格」计数，与异常视图、发布侧的大节口径同源（D12）：直接用
// f / t 的 state_key，而不是数 observation 行，否则同一格内的 2–3 个小节会重复计数。
//
// 归属变化只数「两侧都存在且 building_id 不同」的房号，它是 shared 的子集：既算进
// shared 也单独计数，因为「两版都有这间房」与「这间房换了楼」是两件要同时知道的事。
const releaseDiffSummarySQL = releaseDiffFoldCTE + `,
     rf AS (SELECT room_id, building_id, building_name
              FROM release_room_building WHERE release_id = $2),
     rt AS (SELECT room_id, building_id, building_name
              FROM release_room_building WHERE release_id = $4),
     cells AS (
       SELECT count(*) FILTER (WHERE kind = 'added')   AS added,
              count(*) FILTER (WHERE kind = 'removed') AS removed,
              count(*) FILTER (WHERE kind = 'changed') AS changed,
              count(*) FILTER (WHERE inconsistent)     AS inconsistent_blocks
         FROM j),
     states AS (
       SELECT count(*) FILTER (WHERE state_key = 'unknown'   AND src = 'f') AS f_unknown,
              count(*) FILTER (WHERE state_key = 'composite' AND src = 'f') AS f_composite,
              count(*) FILTER (WHERE state_key = 'unknown'   AND src = 't') AS t_unknown,
              count(*) FILTER (WHERE state_key = 'composite' AND src = 't') AS t_composite
         FROM (SELECT 'f' AS src, state_key FROM f
               UNION ALL
               SELECT 't', state_key FROM t) u),
     rooms AS (
       SELECT count(*) FILTER (WHERE rf.room_id IS NULL) AS added,
              count(*) FILTER (WHERE rt.room_id IS NULL) AS removed,
              count(*) FILTER (WHERE rf.room_id IS NOT NULL AND rt.room_id IS NOT NULL) AS shared,
              count(*) FILTER (WHERE rf.room_id IS NOT NULL AND rt.room_id IS NOT NULL
                                 AND rf.building_id <> rt.building_id) AS building_changed
         FROM rf FULL JOIN rt USING (room_id))
SELECT cells.added, cells.removed, cells.changed, cells.inconsistent_blocks,
       states.f_unknown, states.f_composite, states.t_unknown, states.t_composite,
       rooms.added, rooms.removed, rooms.shared, rooms.building_changed
  FROM cells CROSS JOIN states CROSS JOIN rooms`

// releaseDiffCellsPageSQL 是 section=cells 的一页差异格，沿用折叠 CTE 的 $1–$4。
//
// 参数：$1 from 轴版本、$2 from 版本、$3 to 轴版本、$4 to 版本、$5 kind
// （'all' 表示不过滤）、$6 limit、$7 offset。
//
// 分页 CTE 先按固定键排序再 LIMIT/OFFSET，外层 JOIN 出房名与两版归属楼后**再排一次**：
// JOIN 不保证保持子查询的行序，只在外层 ORDER BY 才让「同一请求同一页」成立（CP6）。
// 排序键 (room_id, week, weekday, block) 在差异集合内唯一（CP3），所以翻页不会出现
// 同一格出现两次或漏掉一格。
//
// kind 用 SQL 参数比较、不拼字符串：$5 = 'all' 时不过滤，否则 kind = $5。kind <>
// 'same' 恒成立——'same' 不是合法的请求取值，但折叠结果里确实有这一行，必须在这里
// 先剔掉，否则 kind=all 会把未变格也发给管理员。
//
// 房名取 room.name、目录缺行时由 COALESCE 折成空串：教室目录可能因数据对齐删过房号，
// 房名缺失不该让整行消失（room 是 LEFT JOIN），空串由 api 原样输出。归属楼用两版各自的归属快照 LEFT
// JOIN：只在 to 侧出现的格没有 from_building，反之亦然，扫描成 nil 后输出 null。
//
// 本语句不返回行数统计：total 由 DiffTotal 从同一事务里的汇总推导（CP6），在分页
// 语句里再 count(*) 等于把两侧折叠多跑一遍。
const releaseDiffCellsPageSQL = releaseDiffFoldCTE + `,
     page AS (
       SELECT kind, room_id, week, weekday, block, from_state, to_state
         FROM j
        WHERE kind <> 'same'
          AND ($5 = 'all' OR kind = $5)
        ORDER BY room_id, week, weekday, block
        LIMIT $6 OFFSET $7
     )
SELECT p.kind, p.room_id, p.week, p.weekday, p.block, p.from_state, p.to_state,
       COALESCE(r.name, '') AS room_name,
       fb.building_id AS from_building,
       tb.building_id AS to_building
  FROM page p
  LEFT JOIN room r  ON r.room_id = p.room_id
  LEFT JOIN release_room_building fb ON fb.release_id = $2 AND fb.room_id = p.room_id
  LEFT JOIN release_room_building tb ON tb.release_id = $4 AND tb.room_id = p.room_id
 ORDER BY p.room_id, p.week, p.weekday, p.block`

// releaseDiffRoomsPageSQL 是 section=rooms 的一页房号增删。
//
// 参数：$1 from 版本、$2 to 版本、$3 kind（'all' | 'added' | 'removed'）、
// $4 limit、$5 offset。
//
// 只读两版归属快照（各几千行），不碰 observation。FULL JOIN 的并集减去交集就是
// 增删集合；kind 的三个取值在这里落成三个 IS NULL 判定，与折叠 CTE 无关。
//
// 归属字段成对可空：新增房没有 from_*，消失房没有 to_*，扫描成 nil 由 api 输出 null。
// 排序键 room_id 在「只在一侧」的集合内唯一（归属快照主键是 (release_id, room_id)），
// 外层再排一次的意义同 cells 页。
const releaseDiffRoomsPageSQL = `
WITH f AS (SELECT room_id, building_id, building_name
             FROM release_room_building WHERE release_id = $1),
     t AS (SELECT room_id, building_id, building_name
             FROM release_room_building WHERE release_id = $2),
     page AS (
       SELECT CASE WHEN f.room_id IS NULL THEN 'added' ELSE 'removed' END AS kind,
              COALESCE(f.room_id, t.room_id) AS room_id,
              f.building_id AS from_building, f.building_name AS from_building_name,
              t.building_id AS to_building,   t.building_name AS to_building_name
         FROM f FULL JOIN t USING (room_id)
        WHERE (f.room_id IS NULL OR t.room_id IS NULL)
          AND ($3 = 'all'
               OR ($3 = 'added'   AND f.room_id IS NULL)
               OR ($3 = 'removed' AND t.room_id IS NULL))
        ORDER BY room_id
        LIMIT $4 OFFSET $5
     )
SELECT p.kind, p.room_id, COALESCE(r.name, '') AS room_name,
       p.from_building, p.from_building_name, p.to_building, p.to_building_name
  FROM page p LEFT JOIN room r ON r.room_id = p.room_id
 ORDER BY p.room_id`

// releaseDiffBuildingsPageSQL 是 section=buildings 的一页归属变化房号。
//
// 参数：$1 from 版本、$2 to 版本、$3 limit、$4 offset。
//
// kind 不参与 SQL：这一节的行恒为「两侧都存在且 building_id 不同」，对应汇总里的
// building_changed；入参 all 与 changed 在这里是同一个集合，api 只负责回显生效值。
// 用内连接而不是 FULL JOIN：只在一侧出现的房号属于 rooms 小节的 added/removed，
// 不是「归属变化」。
//
// 四个归属字段都非空（归属快照两列均 NOT NULL），所以扫描进值类型而不是指针；
// 房名仍用 COALESCE 兜教室目录缺行。排序键 room_id 在交集内唯一，外层再排一次。
const releaseDiffBuildingsPageSQL = `
WITH f AS (SELECT room_id, building_id, building_name
             FROM release_room_building WHERE release_id = $1),
     t AS (SELECT room_id, building_id, building_name
             FROM release_room_building WHERE release_id = $2),
     page AS (
       SELECT f.room_id,
              f.building_id AS from_building, f.building_name AS from_building_name,
              t.building_id AS to_building,   t.building_name AS to_building_name
         FROM f JOIN t USING (room_id)
        WHERE f.building_id <> t.building_id
        ORDER BY f.room_id
        LIMIT $3 OFFSET $4
     )
SELECT p.*, COALESCE(r.name, '') AS room_name
  FROM page p LEFT JOIN room r ON r.room_id = p.room_id
 ORDER BY p.room_id`

// releaseAnomaliesSQL 按版本折叠出异常视图的折叠口径，窗口从发布轮次取。
//
// 每个版本一个 LEFT JOIN LATERAL：内层先用该版自己的 axis_version 把小节观测折成
// 「房号 × 周 × 星期 × 大节」的格（与详情、对比的折叠同口径，D4 / D12），再在折叠
// 结果上数格数、未知、复合与观测行数。必须折完再数：同一大节展开的 2–3 个小节只算
// 一格，直接数 observation 行会把未知/复合放大 2–3 倍，与发布侧 collect_run 的
// 计数（publish/build.go 的大节口径）对不上（CP11）。
//
// 折叠用 r.axis_version 而不是「当前」轴版本：轴改版后同一个 node 可能落到不同 block，
// 用当前轴折历史版本会把旧版折成另一套格。
//
// LATERAL 在空集上仍返回一行：sum 为 NULL、count 为 0、array_agg 为 NULL，所以
// COALESCE 把观测行数/周次折成 0 / 空数组；COALESCE 的其它列只是把「count 理论上
// 不会 NULL」写成确定值，与列表 SQL 同一写法。
//
// observed_weeks 在 SQL 里就排好序（array_agg 带 ORDER BY），Go 侧只做 int32 → int
// 的搬运与 weekGaps 推导；week 先转 int 再聚合，扫描侧就不会撞上 int2[] 的类型
// 歧义（design「异常聚合」的说明）。
//
// 窗口取 run_id 最大的一轮：正常一版只由一轮发布，重复关联时取最新一轮并保持确定；
// 没有关联轮次时两列为 NULL，Go 侧输出 null（Requirement 7.4）。
const releaseAnomaliesSQL = `
SELECT r.release_id, r.term, r.generated_at, r.is_current, r.has_attr_conflict,
       COALESCE(s.observation_rows, 0), COALESCE(s.block_cells, 0),
       COALESCE(s.unknown_cells, 0),    COALESCE(s.composite_cells, 0),
       COALESCE(s.observed_weeks, '{}'::int[]),
       w.window_start, w.window_end
  FROM release r
  LEFT JOIN LATERAL (
        SELECT sum(blk.node_rows)::bigint AS observation_rows,
               count(*)                   AS block_cells,
               count(*) FILTER (WHERE blk.state_key = 'unknown')   AS unknown_cells,
               count(*) FILTER (WHERE blk.state_key = 'composite') AS composite_cells,
               array_agg(DISTINCT blk.week::int ORDER BY blk.week::int) AS observed_weeks
          FROM (
                SELECT o.room_id, o.week, o.weekday, an.block,
                       min(o.state_key) AS state_key,
                       count(*) AS node_rows
                  FROM observation o
                  JOIN axis_node an ON an.axis_version = r.axis_version AND an.node = o.node
                 WHERE o.release_id = r.release_id
                 GROUP BY o.room_id, o.week, o.weekday, an.block
               ) blk
       ) s ON true
  LEFT JOIN LATERAL (
        SELECT cr.window_start, cr.window_end
          FROM collect_run cr
         WHERE cr.release_id = r.release_id
         ORDER BY cr.run_id DESC
         LIMIT 1
       ) w ON true
 ORDER BY r.generated_at DESC, r.release_id DESC`

// releaseAnomalyConflictRoomsSQL 取每个异常版本最新发布轮次的房号级冲突清单。
//
// 取数上限是 conflictRoomsLimit + 1（201）：多取一条才能区分「正好 200 条」与
// 「超过 200 条」，Go 侧据此置 conflict_rooms_truncated（Requirement 7.4）。
// 上限由 fmt.Sprintf 从 conflictRoomsLimit 算出，不在这里另写一个数字：Go 侧的截断
// 判定（rank > conflictRoomsLimit）与 SQL 的过滤条件是同一个来源，改常量时两边一起动。
//
// 冲突只对 has_attr_conflict 的版本查：没标冲突的版本即使某轮 collect_run_room 里
// 有 conflict=true 的行也不属于「这个版本的归属冲突」（版本标记是发布时定下的口径）。
//
// 轮次同样用 LATERAL 取 run_id 最大的一轮：冲突快照按轮次存，回退不改轮次，所以
// 「这一版最新一轮」就是发布这一版的那一轮（重复关联时是最新一次）。
//
// row_number() 按 room_id 升序编号，截断保留的就是 room_id 最小的一批——结果确定，
// 不会随执行计划换一批房号（Requirement 7.4 的「room_id 最小的 200 条」）。
//
// conflict_building_id 在表里可空，但 CHECK 保证 conflict=true 的行两列非空，
// 所以 WHERE crr.conflict 之后可以扫进值类型。
var releaseAnomalyConflictRoomsSQL = fmt.Sprintf(`
WITH ranked AS (
  SELECT r.release_id, crr.room_id, crr.building_id, crr.conflict_building_id,
         row_number() OVER (PARTITION BY r.release_id ORDER BY crr.room_id) AS rn
    FROM release r
    JOIN LATERAL (
          SELECT c.run_id FROM collect_run c
           WHERE c.release_id = r.release_id
           ORDER BY c.run_id DESC LIMIT 1
         ) pub ON true
    JOIN collect_run_room crr ON crr.run_id = pub.run_id AND crr.conflict
   WHERE r.has_attr_conflict
)
SELECT release_id, room_id, building_id, conflict_building_id, rn
  FROM ranked
 WHERE rn <= %d
 ORDER BY release_id, rn`, conflictRoomsLimit+1)

// 回退要识别的两个「可重试」SQLSTATE。唯一冲突复用 collectrun.go 的
// pgUniqueViolation（23505），不在这里重复声明。
const (
	pgDeadlockDetected     = "40P01" // deadlock_detected：两个事务交叉持锁
	pgSerializationFailure = "40001" // serialization_failure：可串行化事务被中止
)

// releaseLockCurrentSQL 是回退事务的第一条语句：锁住当前版本行并读出它的身份与学期。
//
// 先锁当前行、再锁目标行的锁序与发布事务对齐（store/publish.go 的
// demoteCurrentSQL 先降原 current）：两边都从当前行开始持锁，就不会出现「回退持目标
// 等当前、发布持当前等目标」的环（design D7、CP8）。并发下仍可能撞上数据库层冲突，
// 那时交给 40906 让调用方重试。
//
// 用 SELECT ... FOR UPDATE 而不是「先读后改」：判定与后续 UPDATE 都基于本事务持有
// 的行锁，并发回退或发布改不动这两行，判定结论不会在锁与更新之间失效。
const releaseLockCurrentSQL = `
SELECT release_id, term
  FROM release
 WHERE is_current
 FOR UPDATE`

// releaseLockTargetSQL 是回退事务的第二条语句：锁住目标版本行并读出它是否已是 current。
//
// 必须在 releaseLockCurrentSQL 之后执行（锁序固定）。目标缺行由调用方映射
// ErrReleaseNotFound；目标与当前是同一行时（目标已是 current）第二次加锁是同一事务内
// 的重入，不会自锁。
const releaseLockTargetSQL = `
SELECT release_id, term, is_current
  FROM release
 WHERE release_id = $1
 FOR UPDATE`

// releaseDemoteCurrentSQL 降原 current。条件不带版本标识：release_one_current 部分
// 唯一索引保证同时至多一行 current，带 id 只会让「锁的那一行与更新的一行是否同一行」
// 变成一个新的不确定点。
const releaseDemoteCurrentSQL = `
UPDATE release
   SET is_current = false
 WHERE is_current`

// releasePromoteTargetSQL 抬目标为 current，AND NOT is_current 让「目标行数必须为 1」
// 成为可断言的事实。
//
// 0 行说明目标在锁与更新之间不再是非 current（并发回退抢先把它抬成 current，或版本
// 被发布事务删掉）。两条路都靠行锁挡住，理论上不可达；真出现就整个事务回滚，按
// ErrReleaseNotRollbackable 收口，不留下「0 个 current」的库。
const releasePromoteTargetSQL = `
UPDATE release
   SET is_current = true
 WHERE release_id = $1
   AND NOT is_current`

// ReleaseAdminRepo 把管理端版本的 SQL 与事务闭在内部。
//
// 它是 api.ReleaseAdminReader 的结构匹配实现：store 不 import api（依赖方向见
// backend/.golangci.yml 的 store 规则），接口定义在消费侧，方法集在这里一一对应。
// 本文件有列表、详情、对比（汇总 + 小节分页）、异常四个读方法与回退一个写方法
// （tasklist 第 1–6 项）。
type ReleaseAdminRepo struct {
	pool releaseAdminPool // 通常是 *pgxpool.Pool
}

// NewReleaseAdminRepo 绑定一个连接池。
func NewReleaseAdminRepo(pool releaseAdminPool) ReleaseAdminRepo {
	return ReleaseAdminRepo{pool: pool}
}

// List 返回库中全部版本摘要，按生成时间倒序、同刻按版本标识倒序。
//
// 没有版本时返回空切片而不是 nil：调用方与 JSON 序列化都不必区分两种「空」，
// total 由调用方取 len（空库即 0，Requirement 1.4）。
//
// 读库失败一律包 model.ErrQueryFailed（api 映射 50000）；不返回「取到一半」的
// 部分列表——rows.Err() 覆盖「遍历到一半连接断了」，此时已取到的行是残的。
func (r ReleaseAdminRepo) List(ctx context.Context) ([]ReleaseSummary, error) {
	rows, err := r.pool.Query(ctx, releaseListSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本列表失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	summaries := make([]ReleaseSummary, 0)
	for rows.Next() {
		summary, scanErr := scanReleaseSummary(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描版本列表失败: %w", model.ErrQueryFailed, scanErr)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本列表失败: %w", model.ErrQueryFailed, err)
	}
	return summaries, nil
}

// Detail 返回一个版本的详情；版本不存在时返回 (零值, false, nil)。
//
// 三条语句包在一个 REPEATABLE READ READ ONLY 事务里：
//   - REPEATABLE READ：发布事务会删更早版本（deleteOlderReleasesSQL）。多语句读若
//     各看各的快照，就会出现「头条还读得到这一版、逐周计数已经空了」的半份结果（CP7）。
//   - READ ONLY：本路径只有 SELECT，让误写在这里直接失败，而不是靠人工审查（CP2）。
//
// 「没有这个版本」是正常查询结果，不是仓储失败：handler 据此映射 404 / 40401，
// 因此不把 pgx.ErrNoRows 写进 error（与 RunStatusRepo.ByID 同口径）。
func (r ReleaseAdminRepo) Detail(ctx context.Context, releaseID string) (ReleaseDetailView, bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return ReleaseDetailView{}, false, fmt.Errorf("%w: 开启版本详情只读事务失败: %w", model.ErrQueryFailed, err)
	}
	// 只读事务不需要提交：回滚与提交一样只是结束快照并归还连接，不会产生任何写入。
	// 所有失败路径也靠这里收尾（与 CommitRelease 的 defer Rollback 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	return loadReleaseDetail(ctx, tx, releaseID)
}

// loadReleaseDetail 是 Detail 的事务体：只负责三条读语句与拼装，不负责开事务。
//
// stats 与版本头里的 week_count / observation_rows 全部由逐周结果拼出，不额外发
// 聚合语句（design「版本详情」的约束）：
//   - stats.observation_rows = 逐周 observation_rows 之和；
//   - stats.block_cells      = 逐周 block_cells 之和；
//   - stats.week_count       = 逐周结果的行数，也就是可折叠周数（见 releaseWeekCountsSQL）；
//   - release.week_count / release.observation_rows 同上；
//   - stats.rooms            = 头语句的归属快照行数；
//   - stats.buildings        = 按楼结果的行数。
//
// 头条缺失时立刻返回 (零值, false, nil)，不再发后两条语句。
func loadReleaseDetail(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
) (ReleaseDetailView, bool, error) {
	var detail ReleaseDetailView
	err := q.QueryRow(ctx, releaseDetailHeadSQL, releaseID).Scan(
		&detail.Release.ReleaseID,
		&detail.Release.Term,
		&detail.Release.GeneratedAt,
		&detail.Release.DictVersion,
		&detail.Release.AxisVersion,
		&detail.Release.HasAttrConflict,
		&detail.Release.IsCurrent,
		&detail.Release.ReleasedAt,
		&detail.Release.Rooms,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseDetailView{}, false, nil
	}
	if err != nil {
		return ReleaseDetailView{}, false, fmt.Errorf("%w: 读取版本 %s 失败: %w", model.ErrQueryFailed, releaseID, err)
	}

	weekCounts, err := loadReleaseWeekCounts(ctx, q, releaseID, detail.Release.AxisVersion)
	if err != nil {
		return ReleaseDetailView{}, false, err
	}
	buildingCounts, err := loadReleaseBuildingCounts(ctx, q, releaseID)
	if err != nil {
		return ReleaseDetailView{}, false, err
	}

	detail.WeekCounts = weekCounts
	detail.BuildingCounts = buildingCounts
	detail.Stats = ReleaseStats{
		Rooms:     detail.Release.Rooms,
		WeekCount: len(weekCounts),
		Buildings: len(buildingCounts),
	}
	for _, week := range weekCounts {
		detail.Stats.ObservationRows += week.ObservationRows
		detail.Stats.BlockCells += week.BlockCells
	}
	detail.Release.ObservationRows = detail.Stats.ObservationRows
	detail.Release.WeekCount = detail.Stats.WeekCount
	return detail, true, nil
}

// loadReleaseWeekCounts 读逐周覆盖；没有观测时返回空切片而不是 nil。
func loadReleaseWeekCounts(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
	axisVersion int,
) ([]ReleaseWeekCount, error) {
	rows, err := q.Query(ctx, releaseWeekCountsSQL, releaseID, axisVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	defer rows.Close()

	weekCounts := make([]ReleaseWeekCount, 0)
	for rows.Next() {
		var (
			week   int16
			counts ReleaseWeekCount
		)
		if err := rows.Scan(&week, &counts.ObservationRows, &counts.BlockCells); err != nil {
			return nil, fmt.Errorf("%w: 扫描版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
		}
		counts.Week = int(week)
		weekCounts = append(weekCounts, counts)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本 %s 的逐周计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	return weekCounts, nil
}

// loadReleaseBuildingCounts 读按楼分布；没有归属快照时返回空切片而不是 nil。
func loadReleaseBuildingCounts(
	ctx context.Context,
	q releaseDetailQuerier,
	releaseID string,
) ([]ReleaseBuildingCount, error) {
	rows, err := q.Query(ctx, releaseBuildingCountsSQL, releaseID)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	defer rows.Close()

	buildingCounts := make([]ReleaseBuildingCount, 0)
	for rows.Next() {
		var count ReleaseBuildingCount
		if err := rows.Scan(&count.BuildingID, &count.BuildingName, &count.Rooms); err != nil {
			return nil, fmt.Errorf("%w: 扫描版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
		}
		buildingCounts = append(buildingCounts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历版本 %s 的按楼计数失败: %w", model.ErrQueryFailed, releaseID, err)
	}
	return buildingCounts, nil
}

// Diff 返回两版的分类差异汇总与所选小节的一页明细；两侧任一版本不存在时返回包着
// ErrReleaseNotFound 的错误。
//
// 整个对比包在一个 REPEATABLE READ READ ONLY 事务里（与 Detail 同一形状）：
//   - REPEATABLE READ：发布事务会删更早版本。两侧头、汇总、明细若各看各的快照，
//     就可能出现「头的轴版本还在、观测已经随版本被删」的拼凑结果（CP7）。
//   - READ ONLY：本路径只有 SELECT，误写直接失败而不是靠人工审查（CP2）。
//
// 顺序固定为「先两侧头、再汇总、最后明细页」：头解析失败（缺一版）时立刻返回，不再
// 花一次全量折叠去算一份注定要丢掉的汇总；汇总失败时也不必再发分页语句。汇总与
// 明细同事务、同快照，total 由 DiffTotal 从汇总推导必然与明细一致（CP6）。
//
// 失败口径：缺版本 → ErrReleaseNotFound（api 映射 404 / 40401）；读库失败 →
// model.ErrQueryFailed（api 映射 50000）。参数形状、两侧相同、缺参、section/kind
// 组合与 limit/offset 区间由 api 层在查库前挡掉，仓储不重复校验，也不返回半份结果。
func (r ReleaseAdminRepo) Diff(ctx context.Context, params ReleaseDiffParams) (ReleaseDiff, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return ReleaseDiff{}, fmt.Errorf("%w: 开启版本对比只读事务失败: %w", model.ErrQueryFailed, err)
	}
	// 只读事务不需要提交：回滚与提交一样只是结束快照并归还连接，不会产生任何写入。
	defer func() { _ = tx.Rollback(ctx) }()

	return loadReleaseDiff(ctx, tx, params)
}

// loadReleaseDiff 是 Diff 的事务体：解析两侧头、算汇总、取所选小节的一页、
// 拼 same_term，不负责开事务。
//
// 两版头的读法相同，用 loadReleaseDiffSide 的 side 参数带一个「哪一侧」的标签：
// 缺版本时消息要点名 from / to，排障时才知道是基准版被删还是目标版传错了。
//
// 汇总语句的参数顺序固定为 (from 轴版本, from 版本, to 轴版本, to 版本)，与
// releaseDiffFoldCTE 的 $1–$4 一一对应；参数取自刚读回的头，而不是调用方的入参，
// 这样轴版本一定与该版 release 行一致。
//
// 分页参数在入口先补齐缺省（section → cells、kind → all、limit → 50，见
// normalized）：三个字段的零值与 api 的缺省语义一致，直接调用仓储的用例不必先
// 自己补一遍。明细只取所选小节：三个切片初始化为空切片，未选中的保持空，序列化
// 输出 [] 而不是 null。
//
// 未知 section 返回包着 model.ErrQueryFailed 的错误而不是退回 cells：合法取值由 api
// 参数门保证（Requirement 4.5），走到这里说明装配或解析有 bug，按内部错误暴露比
// 静默给一份 cells 明细诚实。
func loadReleaseDiff(
	ctx context.Context,
	q releaseDiffQuerier,
	params ReleaseDiffParams,
) (ReleaseDiff, error) {
	params = params.normalized()

	from, err := loadReleaseDiffSide(ctx, q, "from", params.From)
	if err != nil {
		return ReleaseDiff{}, err
	}
	to, err := loadReleaseDiffSide(ctx, q, "to", params.To)
	if err != nil {
		return ReleaseDiff{}, err
	}

	summary, err := loadReleaseDiffSummary(ctx, q, from, to)
	if err != nil {
		return ReleaseDiff{}, err
	}

	diff := ReleaseDiff{
		From:      from,
		To:        to,
		Summary:   summary,
		Cells:     make([]DiffCellRow, 0),
		Rooms:     make([]DiffRoomRow, 0),
		Buildings: make([]DiffBuildingRow, 0),
	}

	switch params.Section {
	case DiffSectionCells:
		diff.Cells, err = loadDiffCells(ctx, q, params, from, to)
	case DiffSectionRooms:
		diff.Rooms, err = loadDiffRooms(ctx, q, params, from, to)
	case DiffSectionBuildings:
		diff.Buildings, err = loadDiffBuildings(ctx, q, params, from, to)
	default:
		return ReleaseDiff{}, fmt.Errorf("%w: 未知的对比小节 %q", model.ErrQueryFailed, params.Section)
	}
	if err != nil {
		return ReleaseDiff{}, err
	}

	// same_term 只由两个 term 决定：不同学期照常给差异，只是在汇总里如实标出来
	// （Requirement 3.4），不做拒绝也不清空计数。
	diff.Summary.SameTerm = from.Term == to.Term
	return diff, nil
}

// scanDiffCellRow 按 releaseDiffCellsPageSQL 的列序扫一行格差异。
//
// kind 先扫进 string 再转 DiffKind：只有那么几个取值，直接扫进命名类型也行，但
// week / weekday 是 smallint，用 int16 中转再转 int 与逐周计数同一写法，避免依赖
// pgx 对 int2 → int 的隐式转换。可空列（from_state / to_state / 两侧归属楼）扫进
// 指针：NULL 落成 nil，响应据此输出 null。
func scanDiffCellRow(scan func(dest ...any) error) (DiffCellRow, error) {
	var (
		row           DiffCellRow
		kind          string
		week, weekday int16
	)
	err := scan(
		&kind,
		&row.RoomID,
		&week,
		&weekday,
		&row.Block,
		&row.FromState,
		&row.ToState,
		&row.RoomName,
		&row.FromBuilding,
		&row.ToBuilding,
	)
	if err != nil {
		return DiffCellRow{}, err
	}
	row.Kind = DiffKind(kind)
	row.Week = int(week)
	row.Weekday = int(weekday)
	return row, nil
}

// scanDiffRoomRow 按 releaseDiffRoomsPageSQL 的列序扫一行房号增删。
//
// 四个归属字段都是指针：新增房缺 from_*、消失房缺 to_*，扫描后由 api 输出 null。
func scanDiffRoomRow(scan func(dest ...any) error) (DiffRoomRow, error) {
	var (
		row  DiffRoomRow
		kind string
	)
	err := scan(
		&kind,
		&row.RoomID,
		&row.RoomName,
		&row.FromBuilding,
		&row.FromBuildingName,
		&row.ToBuilding,
		&row.ToBuildingName,
	)
	if err != nil {
		return DiffRoomRow{}, err
	}
	row.Kind = DiffKind(kind)
	return row, nil
}

// scanDiffBuildingRow 按 releaseDiffBuildingsPageSQL 的列序扫一行归属变化。
//
// 四个归属字段是值类型（两版归属快照的列都 NOT NULL，且这一节只取交集），房名在
// 最后一列，顺序与 SQL 的 `SELECT p.*, room_name` 一致。
func scanDiffBuildingRow(scan func(dest ...any) error) (DiffBuildingRow, error) {
	var row DiffBuildingRow
	err := scan(
		&row.RoomID,
		&row.FromBuilding,
		&row.FromBuildingName,
		&row.ToBuilding,
		&row.ToBuildingName,
		&row.RoomName,
	)
	if err != nil {
		return DiffBuildingRow{}, err
	}
	return row, nil
}

// loadDiffCells 取 section=cells 的一页；没有差异或 offset 翻过末页时返回空切片。
//
// 参数编号与折叠 CTE 对齐：$1/$2 是 from 的轴版本与版本，$3/$4 是 to 的，$5 kind、
// $6 limit、$7 offset。轴版本取自两侧头而不是入参，保证与 release 行一致。
func loadDiffCells(
	ctx context.Context,
	q releaseDiffQuerier,
	params ReleaseDiffParams,
	from ReleaseSide,
	to ReleaseSide,
) ([]DiffCellRow, error) {
	rows, err := q.Query(ctx, releaseDiffCellsPageSQL,
		from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID,
		string(params.Kind), params.Limit, params.Offset)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 与 %s 的格差异分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	defer rows.Close()

	cells := make([]DiffCellRow, 0)
	for rows.Next() {
		cell, scanErr := scanDiffCellRow(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描 %s 与 %s 的格差异分页失败: %w",
				model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, scanErr)
		}
		cells = append(cells, cell)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历 %s 与 %s 的格差异分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	return cells, nil
}

// loadDiffRooms 取 section=rooms 的一页；参数是 ($1 from, $2 to, $3 kind, $4 limit, $5 offset)。
func loadDiffRooms(
	ctx context.Context,
	q releaseDiffQuerier,
	params ReleaseDiffParams,
	from ReleaseSide,
	to ReleaseSide,
) ([]DiffRoomRow, error) {
	rows, err := q.Query(ctx, releaseDiffRoomsPageSQL,
		from.ReleaseID, to.ReleaseID, string(params.Kind), params.Limit, params.Offset)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 与 %s 的房号差异分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	defer rows.Close()

	rooms := make([]DiffRoomRow, 0)
	for rows.Next() {
		room, scanErr := scanDiffRoomRow(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描 %s 与 %s 的房号差异分页失败: %w",
				model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, scanErr)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历 %s 与 %s 的房号差异分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	return rooms, nil
}

// loadDiffBuildings 取 section=buildings 的一页；参数是 ($1 from, $2 to, $3 limit, $4 offset)。
//
// kind 不进 SQL：这一节的行恒为 building_changed，all 与 changed 是同一集合。
func loadDiffBuildings(
	ctx context.Context,
	q releaseDiffQuerier,
	params ReleaseDiffParams,
	from ReleaseSide,
	to ReleaseSide,
) ([]DiffBuildingRow, error) {
	rows, err := q.Query(ctx, releaseDiffBuildingsPageSQL,
		from.ReleaseID, to.ReleaseID, params.Limit, params.Offset)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 与 %s 的归属变化分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	defer rows.Close()

	buildings := make([]DiffBuildingRow, 0)
	for rows.Next() {
		building, scanErr := scanDiffBuildingRow(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描 %s 与 %s 的归属变化分页失败: %w",
				model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, scanErr)
		}
		buildings = append(buildings, building)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历 %s 与 %s 的归属变化分页失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}
	return buildings, nil
}

// loadReleaseDiffSide 读一侧版本头。
//
// 「没有这一版」是正常查询结果而不是仓储故障：包 ErrReleaseNotFound 由 handler 映射
// 404 / 40401，消息里点名 from / to 与版本标识（标识不是秘密，排障必需）；其余读库
// 错误包 model.ErrQueryFailed。两种都不返回零值之外的东西，调用方不会拿到半份头。
func loadReleaseDiffSide(
	ctx context.Context,
	q releaseDiffQuerier,
	side string,
	releaseID string,
) (ReleaseSide, error) {
	var head ReleaseSide
	err := q.QueryRow(ctx, releaseDiffHeadSQL, releaseID).Scan(
		&head.ReleaseID,
		&head.Term,
		&head.GeneratedAt,
		&head.DictVersion,
		&head.AxisVersion,
		&head.IsCurrent,
		&head.HasAttrConflict,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseSide{}, fmt.Errorf("%w: %s 侧版本 %s 不存在", ErrReleaseNotFound, side, releaseID)
	}
	if err != nil {
		return ReleaseSide{}, fmt.Errorf("%w: 读取 %s 侧版本 %s 失败: %w",
			model.ErrQueryFailed, side, releaseID, err)
	}
	return head, nil
}

// loadReleaseDiffSummary 执行汇总语句并把一行 12 列拼成 ReleaseDiffSummary。
//
// 该语句每个 CTE 都恰好一行，所以这里用 QueryRow 而不是 Query：多行说明 SQL 写错了
// （比如漏了某个 CROSS JOIN 的聚合），此时取的仍是第一行，宁可让集成用例断言数值
// 不符，也不在热路径上多开一个行集。
//
// 派生量全部在 Go 侧算，SQL 只给原始计数：cells.total = added + removed + changed
// （类型划分，不在 SQL 里再数一遍 same/non-same）；delta = to - from（交换方向时
// 自然取反，CP5）。这样 SQL 的列序变化不会被「total 也写进 SQL」掩盖。
func loadReleaseDiffSummary(
	ctx context.Context,
	q releaseDiffQuerier,
	from ReleaseSide,
	to ReleaseSide,
) (ReleaseDiffSummary, error) {
	var (
		summary                    ReleaseDiffSummary
		fromUnknown, fromComposite int
		toUnknown, toComposite     int
	)
	err := q.QueryRow(ctx, releaseDiffSummarySQL,
		from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID,
	).Scan(
		&summary.Cells.Added,
		&summary.Cells.Removed,
		&summary.Cells.Changed,
		&summary.InconsistentBlocks,
		&fromUnknown,
		&fromComposite,
		&toUnknown,
		&toComposite,
		&summary.Rooms.Added,
		&summary.Rooms.Removed,
		&summary.Rooms.Shared,
		&summary.Rooms.BuildingChanged,
	)
	if err != nil {
		return ReleaseDiffSummary{}, fmt.Errorf("%w: 统计 %s 与 %s 的差异失败: %w",
			model.ErrQueryFailed, from.ReleaseID, to.ReleaseID, err)
	}

	summary.Cells.Total = summary.Cells.Added + summary.Cells.Removed + summary.Cells.Changed
	summary.Unknown = StateDelta{From: fromUnknown, To: toUnknown, Delta: toUnknown - fromUnknown}
	summary.Composite = StateDelta{From: fromComposite, To: toComposite, Delta: toComposite - fromComposite}
	return summary, nil
}

// Anomalies 返回库中每个版本的异常聚合：未知/复合格数与占比、观测周与缺周、
// 关联轮次的采集窗口、归属冲突清单（Requirement 7）。
//
// 两条语句（版本折叠 + 冲突清单）包在一个 REPEATABLE READ READ ONLY 事务里：
//   - REPEATABLE READ：发布事务会删更早版本。版本行与冲突清单若各看各的快照，
//     就会出现「版本还在、冲突清单已经跟着轮次或版本被删」的拼凑结果（CP7）；
//   - READ ONLY：本路径只有 SELECT，误写直接失败而不是靠人工审查（CP2）。
//
// 没有版本时返回空切片而不是 nil：响应里 releases 是 []（Requirement 7.5）。
// 读库失败一律包 model.ErrQueryFailed（api 映射 50000），不返回半份视图。
//
// 冲突清单在版本行之后单独读：它要 join collect_run 找最新一轮再扫 collect_run_room，
// 与观测折叠是两棵互不相干的查询；放进同一条语句会让折叠结果按冲突行复制。
func (r ReleaseAdminRepo) Anomalies(ctx context.Context) ([]ReleaseAnomaly, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: 开启异常视图只读事务失败: %w", model.ErrQueryFailed, err)
	}
	// 只读事务不需要提交：回滚与提交一样只是结束快照并归还连接，不会产生任何写入。
	defer func() { _ = tx.Rollback(ctx) }()

	return loadReleaseAnomalies(ctx, tx)
}

// loadReleaseAnomalies 是 Anomalies 的事务体：先读全部版本的折叠结果，再读冲突清单
// 并按版本贴回去，不负责开事务。
//
// 冲突清单必须等版本行全部读完之后再读：版本行要按 release_id 建下标，才能把
// 「冲突行属于哪个版本」在 O(1) 里定位；两条语句在同一个只读事务里，先后不影响快照。
// 每个版本的 ConflictRooms 在扫描阶段就初始化成空切片，所以「没有冲突」的版本与
// 「冲突行还没贴上来」的版本在序列化上是同一个稳定的 []。
func loadReleaseAnomalies(ctx context.Context, q releaseAnomalyQuerier) ([]ReleaseAnomaly, error) {
	rows, err := q.Query(ctx, releaseAnomaliesSQL)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取异常视图失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	anomalies := make([]ReleaseAnomaly, 0)
	positions := make(map[string]int) // release_id → 在 anomalies 里的下标
	for rows.Next() {
		anomaly, scanErr := scanReleaseAnomaly(rows.Scan)
		if scanErr != nil {
			return nil, fmt.Errorf("%w: 扫描异常视图失败: %w", model.ErrQueryFailed, scanErr)
		}
		positions[anomaly.ReleaseID] = len(anomalies)
		anomalies = append(anomalies, anomaly)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: 遍历异常视图失败: %w", model.ErrQueryFailed, err)
	}

	if err := loadReleaseAnomalyConflictRooms(ctx, q, anomalies, positions); err != nil {
		return nil, err
	}
	return anomalies, nil
}

// loadReleaseAnomalyConflictRooms 读冲突清单并按下标贴回 anomalies；截断按版本标记。
//
// 空切片来自扫描阶段的初始化，这里只 append 命中上限以内的行，所以没有冲突的版本
// 仍是 []。rn 由 SQL 的 row_number() 给出、不是 Go 侧数出来的：截断边界必须与
// 「room_id 最小的 N 条」是同一次排序的结果，否则两条路径各排一次就可能换一批房号。
func loadReleaseAnomalyConflictRooms(
	ctx context.Context,
	q releaseAnomalyQuerier,
	anomalies []ReleaseAnomaly,
	positions map[string]int,
) error {
	rows, err := q.Query(ctx, releaseAnomalyConflictRoomsSQL)
	if err != nil {
		return fmt.Errorf("%w: 读取异常视图的归属冲突清单失败: %w", model.ErrQueryFailed, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			releaseID          string
			roomID             string
			buildingID         string
			conflictBuildingID string
			rank               int64
		)
		if err := rows.Scan(&releaseID, &roomID, &buildingID, &conflictBuildingID, &rank); err != nil {
			return fmt.Errorf("%w: 扫描异常视图的归属冲突清单失败: %w", model.ErrQueryFailed, err)
		}

		position, ok := positions[releaseID]
		if !ok {
			// 同一只读事务的两条语句看到同一份 release 集合，正常不可能走到这里。
			// 真出现说明快照被外力切开（或 SQL 改了过滤条件），跳过这一行而不是 panic，
			// 也不为它伪造一个版本条目。
			continue
		}
		if rank > conflictRoomsLimit {
			// SQL 只多取一条（201）：这一行唯一的用途就是告诉 Go「后面还有」。
			anomalies[position].ConflictRoomsTruncated = true
			continue
		}
		anomalies[position].ConflictRooms = append(anomalies[position].ConflictRooms, ConflictRoom{
			RoomID:             roomID,
			BuildingID:         buildingID,
			ConflictBuildingID: conflictBuildingID,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: 遍历异常视图的归属冲突清单失败: %w", model.ErrQueryFailed, err)
	}
	return nil
}

// scanReleaseAnomaly 按 releaseAnomaliesSQL 的列序扫一行，并就地补出派生量：
// 未知/复合的 ratio、observed_weeks 的 int 转换、week_gaps、window 与空冲突清单。
//
// 列序：版本身份 5 列、折叠 5 列（观测行数、格数、未知、复合、观测周）、窗口 2 列。
// 窗口两列是 smallint 且可空，扫成 *int16 再转 int：NULL 表示没有关联轮次，Window 为
// nil。observed_weeks 扫成 []int32 再逐项转 int，避开 int2[] 的扫描歧义（design）。
//
// observation_rows 不在 ReleaseAnomaly 的契约里（响应没有这个键），仍随折叠选出：
// 它与 block_cells 同出一次扫描，是「观测行数 / 格数」这对口径的完整一行。本视图
// 只把 block_cells 用作 ratio 分母；行数扫进局部变量后即不再参与拼装，后续条目若要
// 在视图里展示观测行数，不必回来把同一个版本再折一遍。
func scanReleaseAnomaly(scan func(dest ...any) error) (ReleaseAnomaly, error) {
	var (
		anomaly         ReleaseAnomaly
		observationRows int64
		blockCells      int
		unknownCells    int
		compositeCells  int
		observedWeeks   []int32
		windowStart     *int16
		windowEnd       *int16
	)
	err := scan(
		&anomaly.ReleaseID,
		&anomaly.Term,
		&anomaly.GeneratedAt,
		&anomaly.IsCurrent,
		&anomaly.HasAttrConflict,
		&observationRows,
		&blockCells,
		&unknownCells,
		&compositeCells,
		&observedWeeks,
		&windowStart,
		&windowEnd,
	)
	if err != nil {
		return ReleaseAnomaly{}, err
	}

	anomaly.Unknown = StateRatio{Cells: unknownCells, Ratio: ratioOf(unknownCells, blockCells)}
	anomaly.Composite = StateRatio{Cells: compositeCells, Ratio: ratioOf(compositeCells, blockCells)}
	anomaly.ObservedWeeks = make([]int, len(observedWeeks))
	for index, week := range observedWeeks {
		anomaly.ObservedWeeks[index] = int(week)
	}
	anomaly.WeekGaps = weekGaps(anomaly.ObservedWeeks)
	anomaly.ConflictRooms = make([]ConflictRoom, 0)
	// 两列要么同时有值、要么同时为 NULL（同一个 LATERAL 选同一行）：只认成对的窗口，
	// 只有一半时按「没有窗口」处理，不拼一个半开区间出来。
	if windowStart != nil && windowEnd != nil {
		anomaly.Window = &ReleaseWindow{Start: int(*windowStart), End: int(*windowEnd)}
	}
	return anomaly, nil
}

// Rollback 把目标版本置为当前版本，并在同一个事务里把原当前版本置为非当前版本。
//
// 事务顺序固定（design「回退事务」、CP8）：
//  1. 锁当前版本行（releaseLockCurrentSQL）；
//  2. 锁目标版本行（releaseLockTargetSQL）；
//  3. Go 侧判定：目标存在 → 目标非 current → 有当前 → 同 term；
//  4. 先降后抬：demoteCurrent → promoteTarget（目标必须命中 1 行）；
//  5. 提交。
//
// 两条 UPDATE 在同一事务内、提交前对外不可见；release_one_current 是只约束
// is_current = true 的部分唯一索引，所以「先降后抬」之间零个 current 是允许的。
// 任一步失败都回滚整个事务，原 current 保持原状（Requirement 5.2、CP8）。
//
// 隔离级别用 READ COMMITTED（design 的 BEGIN ISOLATION LEVEL READ COMMITTED）：
// 回退不靠「同一快照的多语句读」，靠的是两条 FOR UPDATE 行锁；提高隔离级别只会把
// 并发回退从「40904 目标已 current」变成「40001 → 40906」，并不会更安全。
//
// 成功返回后的公开查询缓存失效不在这里做：它由装配层（api.WithReleaseAdmin 的
// invalidate 回调）在提交成功后、写响应前调用；仓储只保证提交成功（CP10）。
func (r ReleaseAdminRepo) Rollback(ctx context.Context, targetID string) (RollbackResult, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return RollbackResult{}, fmt.Errorf("%w: 开启回退事务失败: %w", model.ErrQueryFailed, err)
	}
	// 提交成功后回滚是空操作；所有失败路径都靠这里收尾（与 CommitRelease 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := rollbackRelease(ctx, tx, targetID)
	if err != nil {
		return RollbackResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RollbackResult{}, releaseRollbackError(err, "提交回退事务失败")
	}
	return result, nil
}

// rollbackRelease 是 Rollback 的事务体：锁两行、判定、先降后抬、拼结果，不负责
// 开事务与提交。
//
// 判定顺序在两条 SELECT 都执行完之后，且「目标缺行」优先于其它原因：只有目标存在，
// 后面那些比较才有意义。目标已 current 先于「没有当前」判定——目标已 current 时
// 库里通常也有一行 current（就是它自己），先报 40904 能让消息说清「你点的是当前版本」，
// 而不是「库里没有当前版本」。
//
// 判定一律返回哨兵错误，不做任何写入：拒绝路径的 release 表零变更是调用方可以直接
// 断言的（tasklist 第 4 项的集成要求）。
func rollbackRelease(ctx context.Context, tx releaseRollbackTx, targetID string) (RollbackResult, error) {
	var (
		currentID, currentTerm string
		hasCurrent             bool
	)
	err := tx.QueryRow(ctx, releaseLockCurrentSQL).Scan(&currentID, &currentTerm)
	switch {
	case err == nil:
		hasCurrent = true
	case errors.Is(err, pgx.ErrNoRows):
		// 没有 current 不是读失败：继续锁目标，按判定顺序给 40904（无 current）。
		// 先不返回，是为了让「目标缺行」在无 current 的库里仍然报 404。
	default:
		return RollbackResult{}, releaseRollbackError(err, "锁定当前版本失败")
	}

	var (
		targetIDRead string
		targetTerm   string
		targetNow    bool
	)
	err = tx.QueryRow(ctx, releaseLockTargetSQL, targetID).Scan(&targetIDRead, &targetTerm, &targetNow)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return RollbackResult{}, fmt.Errorf("%w: 回退目标版本 %s 不存在", ErrReleaseNotFound, targetID)
	case err != nil:
		return RollbackResult{}, releaseRollbackError(err, fmt.Sprintf("锁定回退目标 %s 失败", targetID))
	}

	if targetNow {
		return RollbackResult{}, fmt.Errorf("%w: 目标版本 %s 已是当前版本，不能回退",
			ErrReleaseNotRollbackable, targetIDRead)
	}
	if !hasCurrent {
		return RollbackResult{}, fmt.Errorf("%w: 库中没有当前版本，版本 %s 不能回退",
			ErrReleaseNotRollbackable, targetIDRead)
	}
	if currentTerm != targetTerm {
		return RollbackResult{}, fmt.Errorf("%w: 目标版本属于学期 %s，当前版本属于学期 %s，不能跨学期回退",
			ErrReleaseTermMismatch, targetTerm, currentTerm)
	}

	tag, err := tx.Exec(ctx, releaseDemoteCurrentSQL)
	if err != nil {
		return RollbackResult{}, releaseRollbackError(err, "降原当前版本失败")
	}
	if tag.RowsAffected() != 1 {
		// 刚才在同一个事务里锁住并读到了 current，别的会话改不动这一行；命不中 1 行
		// 说明库的「恰有一个 current」不变量已被破坏。按内部错误暴露，不装成 40904。
		return RollbackResult{}, fmt.Errorf("%w: 降原当前版本命中 %d 行，期望 1 行",
			model.ErrQueryFailed, tag.RowsAffected())
	}

	tag, err = tx.Exec(ctx, releasePromoteTargetSQL, targetIDRead)
	if err != nil {
		return RollbackResult{}, releaseRollbackError(err, "抬目标版本为当前失败")
	}
	if tag.RowsAffected() != 1 {
		return RollbackResult{}, fmt.Errorf("%w: 目标版本 %s 在锁与更新之间不再是可回退状态，回退中止",
			ErrReleaseNotRollbackable, targetIDRead)
	}

	return RollbackResult{
		RolledBackFrom:   currentID,
		CurrentReleaseID: targetIDRead,
		Term:             targetTerm,
	}, nil
}

// releaseRollbackError 把回退事务里的数据库错误折成两类哨兵。
//
// 可重试的并发冲突（唯一索引冲突 23505、死锁 40P01、序列化失败 40001）包
// ErrReleaseConflict（409 / 40906）：它们表示「另一条版本变更语句和你撞上了」，
// 重试就能成功，不是数据或装配问题。其余一律包 model.ErrQueryFailed（50000）。
//
// 注意 40001 是 SQLSTATE，与业务码 40001（参数非法）同名但域不同：一个来自
// PostgreSQL，一个来自 api；这里只认 *pgconn.PgError 里的那个。
//
// 必须用 errors.As 取 *pgconn.PgError：pgx 在事务失败时会把原始错误再包一层，
// 直接类型断言会漏掉。
func releaseRollbackError(err error, action string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgUniqueViolation, pgDeadlockDetected, pgSerializationFailure:
			return fmt.Errorf("%w: %s: %w", ErrReleaseConflict, action, err)
		}
	}
	return fmt.Errorf("%w: %s: %w", model.ErrQueryFailed, action, err)
}
