package collector

import (
	"bytes"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// matrixRoomAt 造一条 parser 口径的房号行；states 是「状态格序号 → 原文」，
// 序号按「星期 × 大节」展开（与 parser.Rows 的输出一致）。
// 这里直接构造 model.MatrixRoom 而不是再过一遍 HTML：本文件测的是候选追加器，
// 页面到房号行的转换由 parser 与 fetch_test.go 覆盖。
func matrixRoomAt(roomID string, states map[int]string) model.MatrixRoom {
	room := model.MatrixRoom{RoomID: roomID}
	for day := 0; day < 7; day++ {
		for index, block := range matrixFixtureBlocks {
			cellIndex := day*len(matrixFixtureBlocks) + index
			room.Cells = append(room.Cells, model.MatrixCell{
				Weekday:  day + 1,
				Block:    block,
				StateRaw: states[cellIndex],
			})
		}
	}
	return room
}

// TestNewBankStartsEmpty 钉住零状态：两个 map 必须已分配，写入不能 panic。
func TestNewBankStartsEmpty(t *testing.T) {
	candidates := newBank()
	if len(candidates.pages) != 0 || len(candidates.unavailable) != 0 {
		t.Fatalf("新候选器应为空，实际 pages=%d unavailable=%d",
			len(candidates.pages), len(candidates.unavailable))
	}

	key := weekKey{BuildingID: "Q10", Week: 9}
	candidates.storeMatrix(key, []byte("page"), nil, matrixFixtureDict())
	if len(candidates.pages) != 1 {
		t.Fatalf("写入后应有一份候选，实际 %d 份", len(candidates.pages))
	}
}

// TestStoreMatrixReplacesWholePage 覆盖 Requirement 6.3 与 D4：同一 (楼,周) 写两次
// 整片覆盖，只留后一次；格子不追加、不残留旧页的房号。覆盖只作用于 pages，
// unavailable 仍是并集：旧页面报出的不可用格留着，新页面报出的追加进来。
//
// 覆盖的是「重放不产生第二批观测」（CP5）：publish.Build 以「重复观测」拒绝整轮，
// 追加式实现会让同一周在候选集里出现两份行。
//
// 第二份正文故意放一个不可用格，用来钉住「并集而非重算」：若覆盖时按新页面重算
// unavailable，旧的 R0001/周一/0102 会连同旧页一起消失。同键写两份不同正文在
// 生产路径里到不了（pages 命中即返回，见 CP10），这条用例绕过那层守卫，专门固化
// storeMatrix 的实现形状。
func TestStoreMatrixReplacesWholePage(t *testing.T) {
	candidates := newBank()
	key := weekKey{BuildingID: "Q10", Week: 9}

	first := candidates.storeMatrix(key, []byte("first"), []model.MatrixRoom{
		matrixRoomAt("R0001", map[int]string{0: "◆"}),
		matrixRoomAt("R0002", nil),
	}, matrixFixtureDict())
	if len(first.Cells) != 2*7*len(matrixFixtureBlocks) {
		t.Fatalf("第一页应摊平成 %d 个格，实际 %d 个", 2*7*len(matrixFixtureBlocks), len(first.Cells))
	}

	second := candidates.storeMatrix(key, []byte("second"), []model.MatrixRoom{
		matrixRoomAt("R0003", map[int]string{0: "◆"}),
	}, matrixFixtureDict())

	if len(candidates.pages) != 1 {
		t.Fatalf("同一 (楼,周) 只应留一份候选，实际 %d 份", len(candidates.pages))
	}
	stored, ok := candidates.pages[key]
	if !ok {
		t.Fatalf("候选里缺少 %+v", key)
	}
	if !bytes.Equal(stored.Page, []byte("second")) {
		t.Fatalf("页面正文应被后一次覆盖，实际 %q", stored.Page)
	}
	if len(stored.Cells) != 7*len(matrixFixtureBlocks) {
		t.Fatalf("后一页应只剩 %d 个格，实际 %d 个", 7*len(matrixFixtureBlocks), len(stored.Cells))
	}
	for _, cell := range stored.Cells {
		if cell.RoomID != "R0003" {
			t.Fatalf("旧页的房号不应残留，实际出现 %q", cell.RoomID)
		}
	}
	if len(second.Rooms) != 1 || second.Rooms[0].RoomID != "R0003" {
		t.Fatalf("返回值应描述后一页，实际 %+v", second.Rooms)
	}

	// 并集而非重算：旧的 R0001/周一/0102 仍在，新的 R0003/周一/0102 被追加。
	// 两页各只报一格不可用，所以总数仍是 2——顺带钉住「重算」不会多出来也不会少掉。
	wantUnavailable := []cellKey{
		{RoomID: "R0001", Weekday: 1, Block: "0102"},
		{RoomID: "R0003", Weekday: 1, Block: "0102"},
	}
	if len(candidates.unavailable) != len(wantUnavailable) {
		t.Fatalf("不可用格应是两页的并集（%d 个），实际 %d 个：%+v",
			len(wantUnavailable), len(candidates.unavailable), candidates.unavailable)
	}
	for _, cell := range wantUnavailable {
		if _, ok := candidates.unavailable[cell]; !ok {
			t.Fatalf("不可用格 %+v 应留在并集里（覆盖不重算并集）", cell)
		}
	}
}

// TestStoreMatrixDedupesUnavailable 覆盖 Requirement 6.2 的去重口径：
// 同一格跨周重复出现只记一条；可用格不记。
func TestStoreMatrixDedupesUnavailable(t *testing.T) {
	candidates := newBank()
	dict := matrixFixtureDict()

	// 两间房在周一第 1 大节都不可用：key 里没有楼，只按「房号 × 星期 × 大节」，
	// 所以这两条互不相同；同一间房的同一格在两个周都不可用，去重后仍是一条。
	candidates.storeMatrix(weekKey{BuildingID: "Q10", Week: 9}, []byte("page-9"),
		[]model.MatrixRoom{
			matrixRoomAt("R0001", map[int]string{0: "◆", 1: "空闲"}),
			matrixRoomAt("R0002", map[int]string{0: "◆"}),
		}, dict)
	candidates.storeMatrix(weekKey{BuildingID: "Q10", Week: 10}, []byte("page-10"),
		[]model.MatrixRoom{
			matrixRoomAt("R0001", map[int]string{0: "◆", 1: "空闲"}),
		}, dict)

	want := []cellKey{
		{RoomID: "R0001", Weekday: 1, Block: "0102"},
		{RoomID: "R0002", Weekday: 1, Block: "0102"},
	}
	if len(candidates.unavailable) != len(want) {
		t.Fatalf("不可用格去重后应有 %d 个，实际 %d 个：%+v",
			len(want), len(candidates.unavailable), candidates.unavailable)
	}
	for _, key := range want {
		if _, ok := candidates.unavailable[key]; !ok {
			t.Fatalf("不可用格 %+v 没有被记下", key)
		}
	}
	// 空闲格（周一第 2 大节）不记。
	if _, ok := candidates.unavailable[cellKey{RoomID: "R0001", Weekday: 1, Block: "030405"}]; ok {
		t.Fatal("空闲格不应进不可用集合")
	}
}

// TestProjectCellsKeepsRowAndHeaderOrder 钉住摊平顺序与逐格投影：顺序固定，
// 同一份上游页面恒得同一份格子序列（候选集与发布顺序可复现）。
func TestProjectCellsKeepsRowAndHeaderOrder(t *testing.T) {
	rooms := []model.MatrixRoom{
		matrixRoomAt("R0002", map[int]string{0: "◆"}),
		matrixRoomAt("R0001", nil),
	}
	cells := projectCells(rooms, matrixFixtureDict())

	if want := 2 * 7 * len(matrixFixtureBlocks); len(cells) != want {
		t.Fatalf("应摊平成 %d 个格，实际 %d 个", want, len(cells))
	}
	if cells[0].RoomID != "R0002" || cells[0].Weekday != 1 || cells[0].Block != "0102" {
		t.Fatalf("首格应是第一间房的周一第 1 大节，实际 %+v", cells[0])
	}
	if cells[0].State.StateKey != "class" || cells[0].State.Available {
		t.Fatalf("◆ 应投影成不可用的 class，实际 %+v", cells[0].State)
	}
	if cells[0].State.RawText != "◆" {
		t.Fatalf("原文应原样保留，实际 %q", cells[0].State.RawText)
	}
	// 第二间房的第一格在「第一间房 35 格」之后。
	next := cells[7*len(matrixFixtureBlocks)]
	if next.RoomID != "R0001" || next.Weekday != 1 || next.Block != "0102" {
		t.Fatalf("第二间房的首格位置不对，实际 %+v", next)
	}
	if next.State.StateKey != model.StateKeyFree || !next.State.Available {
		t.Fatalf("空白格应投影成可用的 free，实际 %+v", next.State)
	}
	// 状态格按「每天重复全部大节」展开：第 6 个格（下标 5）是周二第 1 大节，
	// 不是周一的第 6 个大节。
	if cells[len(matrixFixtureBlocks)].Weekday != 2 || cells[len(matrixFixtureBlocks)].Block != "0102" {
		t.Fatalf("第 6 个格应是周二第 1 大节，实际 %+v", cells[len(matrixFixtureBlocks)])
	}
}

// TestProjectCellsWithoutRooms 钉住空页：没有房号行时返回空切片（而不是 nil 恐慌），
// 调用方按「这周没有教室」处理。
func TestProjectCellsWithoutRooms(t *testing.T) {
	cells := projectCells(nil, matrixFixtureDict())
	if len(cells) != 0 {
		t.Fatalf("没有房号行时应返回空格子，实际 %d 个", len(cells))
	}
}
