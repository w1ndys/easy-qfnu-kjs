// Package publish 组装并校验一轮采集的发布候选集。
//
// 它只做发布前的两件事：把本轮在内存里攒下的观测与说明组装成候选集（Build），
// 再按发布门槛校验（Validate）。版本切换的事务写入在 Commit（任务 11），
// 本包当前不写库。
//
// 数据来源分两半：
//   - 观测与说明尚未落库。observation 与 occupancy_detail 都以 release_id 作为
//     主键的一部分，发布前没有版本号可写，也没有「本轮暂存表」，所以它们由采集
//     编排在抓取、清洗时累计后以 RunInput 传入；publish 不重新解析页面。
//   - 学期、采集窗口、归属冲突与任务完成情况在库里：分别从 collect_run、
//     collect_run_room 与 sync_task 读，SQL 全在 store（依赖方向见
//     backend/.golangci.yml 的 publish 规则）。
//
// 候选集是「可以原样写进新版本」的形状：观测已按轴展开到小节，说明保留原文字段，
// 归属快照带楼展示名，未知/复合已按「房 × 周 × 天 × 大节」计过数。
package publish

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// Building 是白名单里的一栋教学楼：版本归属快照需要楼编号与展示名两列，
// 展示名不来自 room.building_name（那一列不由采集链路维护）。
type Building struct {
	ID   string // 教学楼编号，如 Q10
	Name string // 展示名，如 综合教学楼
}

// CellObservation 是候选观测在按轴展开成小节之前的一格：
// 房号 × 教学周 × 星期 × 大节。State 是 clean 已投影过的结果，
// publish 只读 StateKey 与 Available，不按原文重算可用性（CP4）。
type CellObservation struct {
	RoomID  string            // 房号 jsbh
	Week    int               // 教学周；必须在采集窗口内（CP1）
	Weekday int               // 星期，1=周一 … 7=周日
	Block   string            // 大节编码，如 0102
	State   model.Observation // 清洗后的状态
}

// CellDetail 是候选占用说明在落库前的一行：房号 × 星期 × 大节，
// 加上四项原文字段。WeekRange 用于与采集窗口求交：无交集或解析失败的说明
// 整条丢弃（Requirement 3.3、6.3；原 spec 6.4），不会出现在候选集里。
type CellDetail struct {
	RoomID    string            // 房号 jsbh
	Weekday   int               // 星期，1=周一 … 7=周日
	Block     string            // 大节编码，如 0102
	State     model.Observation // 清洗后的状态
	Course    string            // 课程或考试科目原文
	WeekRange string            // 周次范围原文，如 1-16
	TimeFlag  string            // 时间标志原文，如 单双周
}

// RunInput 是本轮采集在内存里的候选原料，由采集编排在抓取、清洗时累计后传入。
//
// 字典版本与轴版本放在这里而不是从库里读：collect_run 没有这两列，
// 而版本要如实记录「这一轮清洗时用的是哪一版字典与轴」。
// AxisNodes 是同一轴版本的全部节点：Build 用它把大节展开成小节，
// 展开口径与 clean.ExpandNodes 一致（大节编码本身就是小节编号的拼接，这里按轴查，
// 不在代码里写死 5 大节 → 12 小节）。
type RunInput struct {
	DictVersion int               // 本轮清洗使用的字典版本
	AxisVersion int               // 本轮清洗使用的轴版本
	GeneratedAt time.Time         // 版本生成时间；零值退回 time.Now
	Buildings   []Building        // 白名单教学楼，提供归属快照的展示名
	AxisNodes   []model.AxisNode  // 本轴版本的全部节点
	Cells       []CellObservation // 矩阵观测，按采集顺序
	Details     []CellDetail      // 占用说明，按采集顺序

	// DroppedRequired/DroppedOptional 是执行循环按归属过滤丢弃、从未写进
	// sync_task 的明细任务数，来自 pipeline.RunProgress.Dropped()（按必需性分开）。
	// 它们不在库里的任务汇总里，发布终态必须单独看这两个数：库里查不到这批任务，
	// 只按汇总判会把「明细全被丢弃」发布成 success。
	DroppedRequired int
	DroppedOptional int
}

// RoomBuilding 是版本归属快照的一行：房号 → 教学楼。
type RoomBuilding struct {
	RoomID       string // 房号 jsbh
	BuildingID   string // 归属教学楼编号
	BuildingName string // 归属教学楼展示名
}

// Observation 是候选观测落库前的一行：一间房 × 一周 × 一天 × 一节恰一行
// （与 observation 表的主键同口径）。Node 已经按轴从大节展开，
// 同一大节内各小节的状态一致。
type Observation struct {
	RoomID    string // 房号 jsbh
	Week      int    // 教学周
	Weekday   int    // 星期，1=周一 … 7=周日
	Node      string // 小节编号，如 01
	StateKey  string // 语义键
	Available bool   // 按字典投影的可用性
	RawText   string // 状态原文；未知与复合靠它回溯上游
}

// OccupancyDetail 是候选占用说明的一行，对应 occupancy_detail 的列。
// Derived 表示这条说明被用来给缺矩阵行的周补过状态：observation 表没有派生列，
// 「由占用说明推导」只能标在说明这一侧（见 Requirement 6.2）。
type OccupancyDetail struct {
	RoomID    string // 房号 jsbh
	Weekday   int    // 星期，1=周一 … 7=周日
	Block     string // 大节编码
	StateKey  string // 语义键
	Course    string // 课程或考试科目原文
	WeekRange string // 周次范围原文
	TimeFlag  string // 时间标志原文
	Derived   bool   // 是否用这条说明补写过派生观测
}

// Candidate 是一轮采集组装完成、可以写入新版本的候选集（尚未落库）。
//
// 字段分成三组：版本身份（RunID、Term、GeneratedAt、DictVersion、AxisVersion、
// WindowStart/End、HasAttrConflict）、发布门槛所需的轮次事实（必需/可选任务计数
// 与归属过滤丢弃数）以及要写进版本的数据（归属快照、观测、说明与未知/复合/派生计数）。
// RoomsTotal 与 WeeksTotal 是发布时要回写 collect_run 的两个汇总：
// 前者是归属快照覆盖的房号数，后者是采集窗口 `window_end - window_start + 1`
// （窗口已校验为合法，周数至少 1）。
// DroppedDetails 是诊断计数，不写进版本，只用来解释「说明为什么比明细任务少」。
type Candidate struct {
	RunID           int64     // 轮次编号
	Term            string    // 学期
	GeneratedAt     time.Time // 版本生成时间
	DictVersion     int       // 字典版本
	AxisVersion     int       // 轴版本
	WindowStart     int       // 采集窗口起始周
	WindowEnd       int       // 采集窗口结束周
	HasAttrConflict bool      // 本轮是否有归属冲突（原 spec 12.1）

	RequiredTotal  int // 必需任务总数
	RequiredDone   int // 必需任务已完成数
	RequiredFailed int // 必需任务已失败数
	OptionalTotal  int // 可选任务总数
	OptionalDone   int // 可选任务已完成数
	OptionalFailed int // 可选任务已失败数

	// FirstFailureMessage 是当轮最早失败任务（按 task_id 升序的第一条 failed）的原因，
	// 来自 store.RunTaskSummary，原样带进候选集。Commit 在 partial 时把它写进
	// collect_run.error_message，与 pipeline.CloseRun 的 partial 口径一致：没有失败
	// 任务、只有被归属过滤丢弃的可选明细时改用丢弃计数写一句原因（见 Commit）。
	// success 不用这一列，发布事务会清空 error_message。
	FirstFailureMessage string

	// DroppedRequired/DroppedOptional 是归属过滤丢弃、从未落库的明细任务数，
	// 来自 pipeline.RunProgress.Dropped()，经 RunInput 原样带进候选集。
	// 它们和下面的 DroppedDetails 不是一回事：DroppedDetails 是 Build 期因周次
	// 范围解析失败或与窗口无交集而整条丢弃的「说明」数，只用于解释「说明为什么
	// 比明细任务少」，不写进版本、不参与发布终态；这两个字段是「已经规划出来、
	// 但因房号归属对不上而没执行」的明细任务，决定 partial 与拒绝发布。
	DroppedRequired int // 被归属过滤丢弃的必需明细任务数
	DroppedOptional int // 被归属过滤丢弃的可选明细任务数

	RoomsTotal int // 归属快照覆盖的房号数，发布时回写 collect_run.rooms_total
	WeeksTotal int // 采集窗口周数，发布时回写 collect_run.weeks_total

	Rooms          []RoomBuilding    // 归属快照
	Observations   []Observation     // 逐格观测，已展开到小节
	Details        []OccupancyDetail // 占用说明
	UnknownCells   int               // 未知状态格数（按大节计，不按展开后的小节计）
	CompositeCells int               // 复合状态格数（同上）
	DerivedCells   int               // 由占用说明补写状态的格数（按大节计）
	DroppedDetails int               // Build 期整条丢弃的说明数（周次范围解析失败或与窗口无交集），仅诊断，不参与发布终态
}

// Reader 是发布组装需要的只读仓储能力，由 store.PublishRepo 实现。
// publish 只调这三个方法，不接触 pgx、SQL 与表结构（依赖方向见
// backend/.golangci.yml 的 publish 规则）。
type Reader interface {
	// LoadRunFacts 读一轮的学期、采集窗口与归属冲突计数。
	LoadRunFacts(ctx context.Context, runID int64) (store.RunFacts, error)
	// LoadRunRooms 读当轮归属快照（collect_run_room），按房号排序。
	LoadRunRooms(ctx context.Context, runID int64) ([]store.RunRoom, error)
	// LoadRunTaskSummary 聚合必需/可选任务的总数、完成数与失败数。
	LoadRunTaskSummary(ctx context.Context, runID int64) (store.RunTaskSummary, error)
}

// Build 从一轮的轮次事实、任务状态与内存候选原料组装发布候选集
// （design 的 publish.Build，Requirement 6、7.5、CP1）。
//
// 组装口径：
//   - 学期与采集窗口取 collect_run，缺学期或窗口非法直接拒绝——窗口是 CP1 的判据，
//     没有窗口就无从判断观测是否越界；
//   - 观测的周必须落在窗口内、星期必须 1..7、大节必须在轴里，否则拒绝组装；
//     同一格（房号 × 周 × 星期 × 小节）出现两次也拒绝，不靠落库主键兜底；
//   - 归属快照取 collect_run_room，楼展示名按白名单入参补；
//     缺展示名的归属项保留空名，交给 Validate 按「归属快照不完整」拦截；
//   - 派生：先按周次范围与采集窗口求交，无交集或范围解析失败的说明整条丢弃、
//     不进候选集，并计入 Candidate.DroppedDetails（Requirement 3.3、6.3；
//     原 spec 6.4）；保留的说明只在窗口内、该房号该周没有矩阵行时补写状态，
//     并按时间标志的奇偶约束决定补哪几周（Requirement 6.1–6.3）；
//   - 未知/复合只计数、不在这里拦截：同一大节展开出的多个小节只算一格，
//     阈值判断在 Validate；
//   - 汇总计数随候选集一起产出：RoomsTotal 取归属快照的房号数，WeeksTotal 取
//     采集窗口的周数，发布时原样回写 collect_run（管理端进度条要用）；
//   - 归属过滤丢弃的明细任务数（RunInput.DroppedRequired/DroppedOptional）原样
//     带进候选集：这批任务没有写进 sync_task，任务汇总里根本看不到，只能由采集
//     编排从 pipeline.RunProgress.Dropped() 传进来，供 Commit 判终态；
//   - 组装完成后没有任何观测时返回带 ReasonEmptyCandidate 的校验拦截，
//     拒绝发布（Requirement 7.5）。
//
// 成功返回的 Candidate 在上述检查之外都是完整的；业务错误有三类来源，
// 调用方按哨兵区分，不要只看文案：
//   - 候选集为空：带 ReasonEmptyCandidate 的 ValidationError（ErrEmptyCandidate）；
//   - 轮次事实不足：缺学期、采集窗口没定或倒挂（ErrRunFactsInvalid）；
//   - 原料非法：版本号非正、白名单楼空编号或重复、观测越界/重复/大节不在轴里
//     （ErrCandidateInputInvalid），以及轴快照本身不可用（ErrAxisInvalid）；
//
// 另有装配错误（reader 为 nil）与仓储读取失败，它们不是业务错误、原样上抛，
// 调用方不应把这三类哨兵当成「所有错误」的完整分类。
func Build(ctx context.Context, reader Reader, runID int64, input RunInput) (Candidate, error) {
	if reader == nil {
		return Candidate{}, errors.New("publish: 缺少发布仓储")
	}
	if runID <= 0 {
		return Candidate{}, fmt.Errorf("%w: 轮次编号非法: %d", ErrCandidateInputInvalid, runID)
	}
	// 版本号要写进 release.dict_version / axis_version，两列都有外键指向版本表；
	// 零值或负数不可能是合法版本，在组装前挡住，不让它到发布事务才撞外键。
	if input.DictVersion <= 0 || input.AxisVersion <= 0 {
		return Candidate{}, fmt.Errorf("%w: 字典版本 %d / 轴版本 %d 非法",
			ErrCandidateInputInvalid, input.DictVersion, input.AxisVersion)
	}

	facts, err := reader.LoadRunFacts(ctx, runID)
	if err != nil {
		return Candidate{}, err
	}
	term := strings.TrimSpace(facts.Term)
	if term == "" {
		return Candidate{}, fmt.Errorf("%w: 轮次 %d 没有学期，无法组装候选集", ErrRunFactsInvalid, runID)
	}
	if facts.WindowStart < 1 || facts.WindowEnd < facts.WindowStart {
		return Candidate{}, fmt.Errorf("%w: 轮次 %d 的采集窗口非法: %d..%d",
			ErrRunFactsInvalid, runID, facts.WindowStart, facts.WindowEnd)
	}

	axis, err := newAxisIndex(input.AxisNodes)
	if err != nil {
		return Candidate{}, err
	}
	names, err := buildingNameIndex(input.Buildings)
	if err != nil {
		return Candidate{}, err
	}

	attribution, err := reader.LoadRunRooms(ctx, runID)
	if err != nil {
		return Candidate{}, err
	}
	summary, err := reader.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		return Candidate{}, err
	}

	candidate := Candidate{
		RunID:           runID,
		Term:            term,
		GeneratedAt:     input.GeneratedAt,
		DictVersion:     input.DictVersion,
		AxisVersion:     input.AxisVersion,
		WindowStart:     facts.WindowStart,
		WindowEnd:       facts.WindowEnd,
		HasAttrConflict: facts.AttrConflictCount > 0,
		RequiredTotal:   summary.RequiredTotal,
		RequiredDone:    summary.RequiredDone,
		RequiredFailed:  summary.RequiredFailed,
		OptionalTotal:   summary.OptionalTotal,
		OptionalDone:    summary.OptionalDone,
		OptionalFailed:  summary.OptionalFailed,

		FirstFailureMessage: summary.FirstFailureMessage,
		DroppedRequired:     input.DroppedRequired,
		DroppedOptional:     input.DroppedOptional,
		Rooms:               roomsOf(attribution, names),
	}
	candidate.RoomsTotal = len(candidate.Rooms)
	candidate.WeeksTotal = candidate.WindowEnd - candidate.WindowStart + 1
	if candidate.GeneratedAt.IsZero() {
		candidate.GeneratedAt = time.Now()
	}
	// 归属冲突是附加标记，来源有两处：轮次累计计数，以及快照里逐房号的 conflict 标记。
	// 两者任一为真即打标；下一轮没有冲突时自然不会带上（原 spec 12.3）。
	for _, room := range attribution {
		if room.Conflict {
			candidate.HasAttrConflict = true
			break
		}
	}

	assembler := newAssembler(axis, candidate)
	if err := assembler.addCells(input.Cells); err != nil {
		return Candidate{}, err
	}
	if err := assembler.addDetails(input.Details); err != nil {
		return Candidate{}, err
	}

	candidate = assembler.candidate
	if len(candidate.Observations) == 0 {
		// 用校验拦截包一层：调用方拿不到候选集时仍能从 ValidationReason(err)
		// 读出稳定原因，和 Validate 的空候选拦截同一个口径。
		return Candidate{}, newValidationError(
			ReasonEmptyCandidate,
			fmt.Errorf("%w: 轮次 %d 没有可发布的观测", ErrEmptyCandidate, runID))
	}
	return candidate, nil
}

// roomsOf 把当轮归属快照映射成版本归属快照：楼展示名来自白名单入参，
// 查不到时保留空串，由 Validate 拦截，不在这里猜一个名字。
func roomsOf(runRooms []store.RunRoom, names map[string]string) []RoomBuilding {
	if len(runRooms) == 0 {
		return nil
	}
	rooms := make([]RoomBuilding, len(runRooms))
	for index, room := range runRooms {
		buildingID := strings.TrimSpace(room.BuildingID)
		rooms[index] = RoomBuilding{
			RoomID:       strings.TrimSpace(room.RoomID),
			BuildingID:   buildingID,
			BuildingName: names[buildingID],
		}
	}
	return rooms
}

// buildingNameIndex 建「楼编号 → 展示名」索引，并拒绝空编号与重复编号：
// 重复编号会让归属快照的楼名取决于入参顺序，属于编排错误。
func buildingNameIndex(buildings []Building) (map[string]string, error) {
	names := make(map[string]string, len(buildings))
	for _, building := range buildings {
		id := strings.TrimSpace(building.ID)
		if id == "" {
			return nil, fmt.Errorf("%w: 白名单里出现空的教学楼编号", ErrCandidateInputInvalid)
		}
		if _, exists := names[id]; exists {
			return nil, fmt.Errorf("%w: 白名单里教学楼 %s 重复", ErrCandidateInputInvalid, id)
		}
		names[id] = strings.TrimSpace(building.Name)
	}
	return names, nil
}

// axisIndex 是「大节编码 → 小节编号」的展开表，按轴的 ordinal 排序。
// 它按轴数据展开，不写死「5 大节 → 12 小节」。
type axisIndex struct {
	nodesByBlock map[string][]string
}

// newAxisIndex 建展开表并校验轴快照：节点不能为空、每个节点编号必须恰为两位
// ASCII 数字（节点编号是全库通行的「两位一小节」编码，位宽不齐时大节拼接与
// observation.node 都会写进别的形状）、编号不能重复、大节不能为空，
// 且大节必须是小节编号的顺序拼接。轴快照来自本轮清洗使用的版本，坏轴会让所有观测
// 无法展开，因此直接拒绝组装。
func newAxisIndex(nodes []model.AxisNode) (axisIndex, error) {
	if len(nodes) == 0 {
		return axisIndex{}, fmt.Errorf("%w: 没有节次节点", ErrAxisInvalid)
	}

	ordered := make([]model.AxisNode, len(nodes))
	copy(ordered, nodes)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Ordinal < ordered[j].Ordinal
	})

	index := axisIndex{nodesByBlock: make(map[string][]string, len(ordered))}
	seenNodes := make(map[string]struct{}, len(ordered))
	for _, node := range ordered {
		nodeID := strings.TrimSpace(node.Node)
		block := strings.TrimSpace(node.Block)
		if nodeID == "" {
			return axisIndex{}, fmt.Errorf("%w: 节次节点编号为空", ErrAxisInvalid)
		}
		// 节点位宽单独校验：只靠「大节 = 节点顺序拼接」挡不住 Node="0102" 这种
		// 把大节编码整个写进 node 列的坏轴，它会原样落进 observation.node。
		// 先 TrimSpace 再校验：DB 的 char(2) 来源不会带空白，这里容忍内存快照的
		// 首尾空白，但归一化后的值仍必须恰为两位 ASCII 数字。
		if !isTwoASCIIDigits(nodeID) {
			return axisIndex{}, fmt.Errorf("%w: 节次节点 %s 不是两位 ASCII 数字", ErrAxisInvalid, nodeID)
		}
		if block == "" {
			return axisIndex{}, fmt.Errorf("%w: 节次节点 %s 缺少大节编码", ErrAxisInvalid, nodeID)
		}
		if _, exists := seenNodes[nodeID]; exists {
			return axisIndex{}, fmt.Errorf("%w: 节次节点 %s 重复", ErrAxisInvalid, nodeID)
		}
		seenNodes[nodeID] = struct{}{}
		index.nodesByBlock[block] = append(index.nodesByBlock[block], nodeID)
	}

	// 大节编码按约定是小节编号的顺序拼接（01 + 02 → 0102），展开与落库的
	// observation.node 都依赖这个等式。不成立时按大节展开出的节点与编码对不上，
	// 只能拒绝；坏轴不能靠猜分组继续跑。逐个大节校验，先排好块名让错误文案稳定。
	blocks := make([]string, 0, len(index.nodesByBlock))
	for block := range index.nodesByBlock {
		blocks = append(blocks, block)
	}
	sort.Strings(blocks)
	for _, block := range blocks {
		blockNodes := index.nodesByBlock[block]
		if joined := strings.Join(blockNodes, ""); !isEvenASCIIDigits(block) || joined != block {
			return axisIndex{}, fmt.Errorf(
				"%w: 大节 %s 与小节编号 %s 的顺序拼接不一致（大节须为偶数长度 ASCII 数字）",
				ErrAxisInvalid, block, joined)
		}
	}
	return index, nil
}

// isTwoASCIIDigits 报告 text 是否恰为两位 ASCII 数字：节点编号按「两位一小节」编码，
// 一位、四位或含全角数字的编号都不是合法节点。
func isTwoASCIIDigits(text string) bool {
	return len(text) == 2 && isASCIIDigits(text)
}

// isEvenASCIIDigits 报告 text 是否由偶数个 ASCII 数字组成。
// 大节编码是两两一组的节点编号拼接，长度必为偶数；全角数字与字母都不是合法编码。
func isEvenASCIIDigits(text string) bool {
	return text != "" && len(text)%2 == 0 && isASCIIDigits(text)
}

// isASCIIDigits 报告 text 是否每个字节都是 ASCII 数字；空串按成立返回，
// 长度约束由调用方的 isTwoASCIIDigits / isEvenASCIIDigits 负责。
func isASCIIDigits(text string) bool {
	for position := 0; position < len(text); position++ {
		if text[position] < '0' || text[position] > '9' {
			return false
		}
	}
	return true
}

// nodes 返回一个大节包含的小节编号；轴里没有这个大节时返回 false。
func (a axisIndex) nodes(block string) ([]string, bool) {
	nodes, ok := a.nodesByBlock[block]
	return nodes, ok
}

// observationKey 是观测落库主键在内存里的同口径身份：
// 一间房 × 一周 × 一天 × 一节。
type observationKey struct {
	RoomID  string
	Week    int
	Weekday int
	Node    string
}

// cellKey 是「房 × 周 × 天 × 大节」的格身份：未知/复合按它计数，
// 派生也按它去重，同一大节展开出的多个小节不重复计数（PRD 4.5）。
type cellKey struct {
	RoomID  string
	Week    int
	Weekday int
	Block   string
}

// assembler 是组装过程中的累计器：轴展开表、已写入的观测、矩阵周集合与派生态。
type assembler struct {
	axis        axisIndex
	candidate   Candidate
	seenObs     map[observationKey]struct{} // 已写入的观测，用于拒绝重复格
	matrixWeeks map[roomWeek]struct{}       // 有矩阵行的「房号 × 周」，派生的判据
	derivedCell map[cellKey]struct{}        // 已派生过的格，保证多条说明只补一次
}

// roomWeek 是「房号 × 教学周」：该房号在该周是否有矩阵行的判据。
type roomWeek struct {
	RoomID string
	Week   int
}

// newAssembler 用轴展开表与已填好版本字段的候选集初始化累计器。
func newAssembler(axis axisIndex, base Candidate) *assembler {
	return &assembler{
		axis:        axis,
		candidate:   base,
		seenObs:     make(map[observationKey]struct{}),
		matrixWeeks: make(map[roomWeek]struct{}),
		derivedCell: make(map[cellKey]struct{}),
	}
}

// addCells 把矩阵观测按轴展开成逐小节观测，并累计未知/复合格数。
//
// 校验口径：周必须在窗口内（CP1）、星期 1..7、大节在轴里、语义键非空；
// 同一格（房号 × 周 × 天 × 小节）重复出现直接拒绝，不把重复行留给落库主键去撞。
func (a *assembler) addCells(cells []CellObservation) error {
	for _, cell := range cells {
		roomID := strings.TrimSpace(cell.RoomID)
		block := strings.TrimSpace(cell.Block)
		if cell.Week < a.candidate.WindowStart || cell.Week > a.candidate.WindowEnd {
			return fmt.Errorf("%w: 房号 %s 的观测周 %d 落在采集窗口 %d..%d 之外",
				ErrCandidateInputInvalid, roomID, cell.Week, a.candidate.WindowStart, a.candidate.WindowEnd)
		}
		if err := a.checkCell(roomID, cell.Weekday, block, cell.State); err != nil {
			return err
		}
		nodes, ok := a.axis.nodes(block)
		if !ok {
			return fmt.Errorf("%w: 大节 %s 不在当前轴快照里", ErrCandidateInputInvalid, block)
		}

		// 矩阵行是否存在按「房号 × 周」记，不按格记：Requirement 6 说该房号该周
		// 缺矩阵行才派生，一行里只要有任意一格就说明这一周有矩阵结果。
		a.matrixWeeks[roomWeek{RoomID: roomID, Week: cell.Week}] = struct{}{}

		// 未知/复合按大节计一次：展开出的每个小节共享同一状态，不重复计数。
		a.countAnomaly(cell.State.StateKey)
		base := observationKey{RoomID: roomID, Week: cell.Week, Weekday: cell.Weekday}
		if err := a.appendObservation(base, nodes, cell.State); err != nil {
			return err
		}
	}
	return nil
}

// weekParity 是时间标志对派生周的约束：区间内只有满足奇偶条件的周参与派生。
//
// 取值与教务明细页「时间标志」原文对应（docs/upstream-api.md 的实测值为「单双周」）：
// 单双周与空标记表示每周都上；单周只上奇数周；双周只上偶数周；
// 其它标记不认识，一律不派生——说明照常保留，不按猜测的周次补状态。
type weekParity uint8

const (
	parityEveryWeek weekParity = iota // 单双周 / 空标记：区间内每周都派生
	parityOddWeek                     // 单周：只派生奇数周
	parityEvenWeek                    // 双周：只派生偶数周
	parityUnknown                     // 不认识的标记：不派生
)

// parseTimeFlag 把时间标志原文归一到派生口径。
// 只认「单双周」「单周」「双周」与空标记；其余（含全角、带空格以外的修饰）都算不认识。
func parseTimeFlag(raw string) weekParity {
	switch strings.TrimSpace(raw) {
	case "", "单双周":
		return parityEveryWeek
	case "单周":
		return parityOddWeek
	case "双周":
		return parityEvenWeek
	default:
		return parityUnknown
	}
}

// allows 报告某一周是否落在该时间标志的派生范围内；不认识的标记不允许任何周。
func (p weekParity) allows(week int) bool {
	switch p {
	case parityEveryWeek:
		return true
	case parityOddWeek:
		return week%2 == 1
	case parityEvenWeek:
		return week%2 == 0
	default:
		return false
	}
}

// addDetails 丢弃与窗口无交集的说明，再对保留的说明做派生。
//
// 丢弃口径来自 Requirement 3.3（原 spec 6.4）：周次范围与采集窗口求交后为空的
// 说明整条丢弃，既不进候选集、也不派生。解析失败的说明同样丢弃——没有可用的范围
// 就无法判断它是否落在窗口内，保留一条定不了位的说明没有意义（Requirement 6.3
// 要求解析失败时跳过派生）。两种丢弃都计入 Candidate.DroppedDetails，不让「说明
// 比任务少」变成只能靠翻日志猜的静默行为。丢弃先于身份校验：一条窗口外说明的
// 房号/星期写错时只丢它自己，不该把整轮组装打成错误。
//
// 派生只在保留的说明上做（Requirement 6.1–6.3）：该房号该周没有矩阵行时，才用
// 说明补写观测；时间标志决定补哪几周（见 weekParity），不认识的标记不派生、
// 只保留说明。同一格被多条说明覆盖时以输入里靠前的一条为准（见 addDerivedCell）。
func (a *assembler) addDetails(details []CellDetail) error {
	for _, detail := range details {
		from, to, ok := parseWeekRange(detail.WeekRange)
		if !ok {
			a.candidate.DroppedDetails++
			continue
		}
		from, to = a.clampToWindow(from, to)
		if from > to {
			a.candidate.DroppedDetails++
			continue
		}

		roomID := strings.TrimSpace(detail.RoomID)
		block := strings.TrimSpace(detail.Block)
		if err := a.checkCell(roomID, detail.Weekday, block, detail.State); err != nil {
			return err
		}
		if _, inAxis := a.axis.nodes(block); !inAxis {
			return fmt.Errorf("%w: 大节 %s 不在当前轴快照里", ErrCandidateInputInvalid, block)
		}

		parity := parseTimeFlag(detail.TimeFlag)
		derived := false
		if parity != parityUnknown {
			for week := from; week <= to; week++ {
				if !parity.allows(week) {
					continue
				}
				if _, hasMatrix := a.matrixWeeks[roomWeek{RoomID: roomID, Week: week}]; hasMatrix {
					continue
				}
				added, err := a.addDerivedCell(roomID, week, detail.Weekday, block, detail.State)
				if err != nil {
					return err
				}
				if added {
					derived = true
				}
			}
		}

		a.candidate.Details = append(a.candidate.Details, OccupancyDetail{
			RoomID:    roomID,
			Weekday:   detail.Weekday,
			Block:     block,
			StateKey:  detail.State.StateKey,
			Course:    detail.Course,
			WeekRange: detail.WeekRange,
			TimeFlag:  detail.TimeFlag,
			Derived:   derived,
		})
	}
	return nil
}

// addDerivedCell 给一个「房 × 周 × 天 × 大节」补写派生观测，返回是否真的补写了。
// 写观测失败（同格重复）按组装错误上抛：那说明轴或矩阵数据已经自相矛盾，
// 不能当成「这条说明没派生」静默吞掉。
//
// 同一格可能被同一批说明里的多条记录覆盖（多条课程的周次范围重叠），但 observation
// 的主键只允许一行，所以以输入里第一条覆盖到它的说明为准；后来的说明不补观测、
// 其 Derived 也保持 false——它没有贡献任何派生数据。这个取舍在 design 里没有写死，
// 属于发布组装的实现口径。
func (a *assembler) addDerivedCell(roomID string, week, weekday int, block string, state model.Observation) (bool, error) {
	cell := cellKey{RoomID: roomID, Week: week, Weekday: weekday, Block: block}
	if _, exists := a.derivedCell[cell]; exists {
		return false, nil
	}
	nodes, ok := a.axis.nodes(block)
	if !ok {
		return false, nil
	}
	if err := a.appendObservation(observationKey{RoomID: roomID, Week: week, Weekday: weekday}, nodes, state); err != nil {
		return false, err
	}
	a.derivedCell[cell] = struct{}{}
	a.candidate.DerivedCells++
	a.countAnomaly(state.StateKey)
	return true, nil
}

// checkCell 校验一格的身份与状态（房号、星期、大节、语义键）。
// 周次不在这里校验：矩阵观测的周在 addCells 里单独判窗口，说明本身没有周。
func (a *assembler) checkCell(roomID string, weekday int, block string, state model.Observation) error {
	if roomID == "" {
		return fmt.Errorf("%w: 候选格缺少房号", ErrCandidateInputInvalid)
	}
	if weekday < 1 || weekday > 7 {
		return fmt.Errorf("%w: 房号 %s 的星期 %d 不在 1..7 内",
			ErrCandidateInputInvalid, roomID, weekday)
	}
	if block == "" {
		return fmt.Errorf("%w: 房号 %s 的候选格缺少大节编码", ErrCandidateInputInvalid, roomID)
	}
	if strings.TrimSpace(state.StateKey) == "" {
		return fmt.Errorf("%w: 房号 %s 的候选格缺少语义键", ErrCandidateInputInvalid, roomID)
	}
	return nil
}

// appendObservation 把一个状态写到该大节的每个小节上；同一小节重复写入时返回错误。
// 同一大节内各小节状态一致由这里保证：一份状态原样写到展开出的全部节点。
//
// 先整组查重再写入：重复时一个节点都不追加，不留下「前半组已写、后半组报错」
// 的半格数据（调用方拿到错误后整轮也不会发布）。
func (a *assembler) appendObservation(base observationKey, nodes []string, state model.Observation) error {
	keys := make([]observationKey, len(nodes))
	for index, node := range nodes {
		key := observationKey{RoomID: base.RoomID, Week: base.Week, Weekday: base.Weekday, Node: node}
		if _, exists := a.seenObs[key]; exists {
			return fmt.Errorf("%w: 房号 %s 第 %d 周星期 %d 小节 %s 出现重复观测",
				ErrCandidateInputInvalid, key.RoomID, key.Week, key.Weekday, key.Node)
		}
		keys[index] = key
	}

	for _, key := range keys {
		a.seenObs[key] = struct{}{}
		a.candidate.Observations = append(a.candidate.Observations, Observation{
			RoomID:    key.RoomID,
			Week:      key.Week,
			Weekday:   key.Weekday,
			Node:      key.Node,
			StateKey:  state.StateKey,
			Available: state.Available,
			RawText:   state.RawText,
		})
	}
	return nil
}

// countAnomaly 按大节累计未知与复合格数；可用性不在这里重算（CP4）。
func (a *assembler) countAnomaly(stateKey string) {
	switch stateKey {
	case model.StateKeyUnknown:
		a.candidate.UnknownCells++
	case model.StateKeyComposite:
		a.candidate.CompositeCells++
	}
}

// clampToWindow 把周次范围裁到采集窗口内；没有交集时返回的区间起点会大于终点，
// 调用方据此整条丢弃这条说明并计入 DroppedDetails（Requirement 3.3；原 spec 6.4）。
func (a *assembler) clampToWindow(from, to int) (int, int) {
	if from < a.candidate.WindowStart {
		from = a.candidate.WindowStart
	}
	if to > a.candidate.WindowEnd {
		to = a.candidate.WindowEnd
	}
	return from, to
}
