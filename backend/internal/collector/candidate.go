package collector

import (
	"sort"

	"github.com/w1ndys/easy-qfnu-kjs/internal/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// weekKey 是周矩阵页的业务身份「楼 × 周」。
//
// 键用楼编号而不是任务编号：同一轮里同一楼同一周至多一条任务，但续跑、hydrate
// 补抓与人为重放都按业务身份命中同一份页面——任务编号会随库里的行变化，业务身份
// 不变。它同时是 pages 的键与「同一进程内成功页面每键至多一份」的判据（CP10 的
// 缓存侧；一轮内至多一次请求还要靠 pipeline 不重试失败任务）。
type weekKey struct {
	BuildingID string // 楼编号 jxlbh
	Week       int    // 教学周
}

// cellKey 是格子的业务身份「房号 × 星期 × 大节」。
//
// 不含周：同一个格子在不同周的状态合起来只决定「这一格要不要下钻一次明细」，
// 不可用格的去重跨周生效（Requirement 6.2）。也不含楼：房号的归属楼由本轮
// collect_run_room 快照决定，不由「哪个响应报了它」决定（design 未决项 2）。
type cellKey struct {
	RoomID  string // 房号 jsbh
	Weekday int    // 星期，1=周一 … 7=周日
	Block   string // 大节编码，如 0102
}

// cellRow 是矩阵页里已经清洗过的一格：房号 × 星期 × 大节 × 状态。
//
// 它和 model.MatrixCell 的区别是「已经按字典投影」：State 是 clean.State 的结果，
// 同时带着语义键、可用性与状态原文。候选集与不可用格都从它派生，不再二次读原文。
type cellRow struct {
	RoomID  string            // 房号 jsbh
	Weekday int               // 星期
	Block   string            // 大节编码
	State   model.Observation // 清洗后的状态
}

// cellDetailRow 是明细页里已经清洗过的一条占用说明：四项字段 + 清洗后的状态。
//
// 它与 publish.CellDetail 的区别是「不含房号 / 天 / 大节」：那三项是 bank 的 key，
// 同一条明细只属于一个格子，重复存在每个行里没有意义。也不含楼：明细请求本身
// 不带楼（upstream.OccupancyDetailParams 只有学期、节次模式、房号、星期与大节），
// 归属楼由 collect_run_room 决定。
//
// State 是 clean.State 对 StateRaw 的投影（Requirement 7.1），Course / WeekRange /
// TimeFlag 保留上游原文；申请人、任课教师、备注、教室、时间、节次在 parser 就已丢弃
// （Requirement 7.1）。
type cellDetailRow struct {
	State     model.Observation // 教室状态：语义键、可用性与原文
	Course    string            // 课程或考试科目原文
	WeekRange string            // 周次范围原文，如 1-16；与窗口求交由 publish 做
	TimeFlag  string            // 时间标志原文，如 单双周
}

// matrixPage 是一次周矩阵抓取的全部产物，三者同生同灭。
//
// Page 是原始正文，只用于给 pipeline.Run 算 collect_run_week.page_sha256；
// 按 CP12 不落库、不写日志。Rooms 是 parser 解析出的房号行，交给 pipeline 做跨楼
// 归属裁决。Cells 是已经投影好的格，供组装候选集与不可用格使用。
type matrixPage struct {
	Page  []byte             // 页面正文
	Rooms []model.MatrixRoom // 房号行；pipeline 只读，不修改
	Cells []cellRow          // 房号 × 星期 × 大节 × 状态
}

// bank 是一轮采集的内存候选，发布前没有表可落（observation 的主键含 release_id）。
//
// 三个 map 的写入语义不一样，这是刻意保留的不对称：pages 按 (楼,周) 写入即覆盖，
// unavailable 只做并集、从不回溯，details 按 (房,天,大节) 整片覆盖。unavailable
// 的并集语义来自 Requirement 6.2「窗口内任一周不可用即算一格」；另外两个的覆盖
// 语义来自 D4/CP5「重放不产生第二份候选行」。
//
// 并集不失真依赖一条不变式：同一 (楼,周) 每轮只可能拿到同一份正文。pages 命中即
// 返回、不再打上游（CP10），所以同一键的「覆盖」写的必然是上次那份结果，不会借
// 重放带进新的不可用格；真正改写并集的只有换楼或换周，而那正是并集要保留的。
//
// 类型不是并发安全的：一轮执行循环是串行的，hydrate 与两段 Run 也都在同一个
// 协程里，不要跨 goroutine 共享。
type bank struct {
	// pages 是 (楼,周) → 一次矩阵抓取的全部产物；写入即覆盖，同一键重放不产生第二批观测。
	pages map[weekKey]matrixPage
	// unavailable 是 (房,天,大节) → 该格在窗口内至少一周不可用；只做并集、不回溯，
	// 窗口内任一周不可用即算一格（Requirement 6.2）。
	unavailable map[cellKey]struct{}
	// details 是 (房,天,大节) → 一次明细响应的全部说明；写入即覆盖，
	// 同一格重放不追加第二份（Requirement 7.4、D4/CP5）。
	details map[cellKey][]cellDetailRow
	// orphanCells 是导出不可用格时因 collect_run_room 里没有该房号而丢弃的格数。
	// 只作诊断计数，不参与失败分层（Requirement 8.5）；每次导出重新计算，
	// 不累加，重复导出同一份候选不会被算两遍。
	orphanCells int
}

// newBank 创建一轮的空候选。
func newBank() *bank {
	return &bank{
		pages:       make(map[weekKey]matrixPage),
		unavailable: make(map[cellKey]struct{}),
		details:     make(map[cellKey][]cellDetailRow),
	}
}

// storeMatrix 把一次矩阵抓取的产物整片写进 (楼,周) 这一格，并合并它报出的不可用格。
//
// 「整片覆盖」是 D4/CP5 的要求：同一楼同一周被重放时用后一次结果替换前一次，
// 不追加第二份房源与格子——否则 publish.Build 会以「重复观测」拒绝整轮。
//
// unavailable 是并集而不是快照：它的语义是「窗口内任一周不可用」，
// 同一个 (楼,周) 在本轮由 pages 守卫保证至多请求一次，重放带的是同一份正文，
// 所以并集不会因为这次覆盖而失真；换周、换楼写入的不可用格必须留下。
func (b *bank) storeMatrix(key weekKey, page []byte, rooms []model.MatrixRoom, dict model.Dict) matrixPage {
	cells := projectCells(rooms, dict)
	stored := matrixPage{
		Page:  page,
		Rooms: rooms,
		Cells: cells,
	}
	b.pages[key] = stored

	for _, cell := range cells {
		// 可用性只按字典投影，已由 clean.State 判好（未知与复合恒为不可用）。
		// 这里不再按语义键另判一次，避免出现第二套可用性口径（CP4）。
		if cell.State.Available {
			continue
		}
		b.unavailable[cellKey{RoomID: cell.RoomID, Weekday: cell.Weekday, Block: cell.Block}] = struct{}{}
	}

	return stored
}

// storeDetail 把一次明细抓取的全部说明整片写进 (房,天,大节) 这一格。
//
// 「整片覆盖」是 Requirement 7.4 与 D4/CP5 的要求：同一格被重放时用后一次响应
// 替换前一次，不追加第二份——publish.Build 会以「重复说明」拒绝整轮。
//
// 空切片也要写：明细页有标题表但没有字段行表示「该格没有说明」（parser.Occupancy
// 返回空切片与 nil），这是有效结果，必须覆盖掉上一次可能留下的说明；不写会让旧说明
// 冒充新结果。
//
// 状态原文在这里经 clean.State 投影，与矩阵格同一口径（CP4）：可用性只看字典，
// 不在明细侧另立一套判定。
func (b *bank) storeDetail(key cellKey, entries []model.OccupancyEntry, dict model.Dict) []cellDetailRow {
	rows := make([]cellDetailRow, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, cellDetailRow{
			State:     clean.State(entry.StateRaw, dict),
			Course:    entry.Course,
			WeekRange: entry.WeekRange,
			TimeFlag:  entry.TimeFlag,
		})
	}
	b.details[key] = rows
	return rows
}

// unavailableCells 把不可用格导出成第二次 Plan 的入参，归属楼按本轮 collect_run_room
// 重写（design 未决项 2、Requirement 5.3）。
//
// 楼必须取快照里的归属楼，而不是「哪个响应报了它」：明细请求本身不带楼编号，
// 楼只用于 pipeline.Run 的归属过滤（attributedCellInputs 要求任务楼 ==
// Attribution.BuildingOf(房号)）。用快照的楼填进去，明细任务就一条都不会被丢弃，
// Dropped() 恒为 0；用响应所在的楼则会把「靠后的楼报了首见楼的格子」下钻到错的楼上。
//
// 没有归属的房号（collect_run_room 里查不到）不规划、不下钻，只把它的格计入
// orphanCells（Requirement 8.5）；这个计数不影响终态。
//
// 导出顺序固定为 (房号, 星期, 大节) 升序：同一份上游返回同一份候选集才可复现，
// 任务落库顺序与断言都依赖它（design「内存候选的形状」最后一条）。
func (b *bank) unavailableCells(rooms []store.RunRoom) []pipeline.Cell {
	owners := make(map[string]string, len(rooms))
	for _, room := range rooms {
		owners[room.RoomID] = room.BuildingID
	}

	keys := make([]cellKey, 0, len(b.unavailable))
	for key := range b.unavailable {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].RoomID != keys[j].RoomID {
			return keys[i].RoomID < keys[j].RoomID
		}
		if keys[i].Weekday != keys[j].Weekday {
			return keys[i].Weekday < keys[j].Weekday
		}
		return keys[i].Block < keys[j].Block
	})

	cells := make([]pipeline.Cell, 0, len(keys))
	b.orphanCells = 0
	for _, key := range keys {
		buildingID, ok := owners[key.RoomID]
		if !ok {
			b.orphanCells++
			continue
		}
		cells = append(cells, pipeline.Cell{
			BuildingID: buildingID,
			RoomID:     key.RoomID,
			Weekday:    key.Weekday,
			Block:      key.Block,
		})
	}
	return cells
}

// projectCells 把 parser 的房号行摊平成格子并逐格清洗。
//
// 顺序固定为「房号页面顺序 × 星期 × 大节表头顺序」：同一份上游页面恒得同一份格子
// 序列，候选集的顺序才可复现。房号取 room.RoomID 而不是房名：jsbh 是唯一身份，
// 房名只是展示（docs/pitfalls.md 第 3 节）。
func projectCells(rooms []model.MatrixRoom, dict model.Dict) []cellRow {
	total := 0
	for _, room := range rooms {
		total += len(room.Cells)
	}

	cells := make([]cellRow, 0, total)
	for _, room := range rooms {
		for _, cell := range room.Cells {
			cells = append(cells, cellRow{
				RoomID:  room.RoomID,
				Weekday: cell.Weekday,
				Block:   cell.Block,
				State:   clean.State(cell.StateRaw, dict),
			})
		}
	}
	return cells
}
