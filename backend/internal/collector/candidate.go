package collector

import (
	"github.com/w1ndys/easy-qfnu-kjs/internal/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
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
// 两个 map 的写入语义不同，这是刻意保留的不对称：pages 按 (楼,周) 写入即覆盖，
// unavailable 只做并集、从不回溯——某格一旦在窗口内任一周被记为不可用，后面同格
// 可用也不会把它撤掉（Requirement 6.2）。
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
}

// newBank 创建一轮的空候选。
func newBank() *bank {
	return &bank{
		pages:       make(map[weekKey]matrixPage),
		unavailable: make(map[cellKey]struct{}),
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
