package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// 本文件的矩阵夹具全部手写脱敏，结构与迁移种子一致，不含真实页面样本
// （docs/pitfalls.md 第 10 节）。夹具只覆盖 collector 需要验证的形状：
// 表头 36 格、数据行 36 格、状态原文原样交给 clean。

// matrixFixtureBlocks 是表头 5 个大节的编码，与迁移 axis_node 的种子一致。
var matrixFixtureBlocks = []string{"0102", "030405", "0607", "0809", "101112"}

// matrixFixtureAxis 按迁移种子造 12 个小节：5 大节 → 12 小节。
func matrixFixtureAxis() []model.AxisNode {
	return []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
		{Node: "04", Ordinal: 4, Block: "030405", BlockOrdinal: 2},
		{Node: "05", Ordinal: 5, Block: "030405", BlockOrdinal: 2},
		{Node: "06", Ordinal: 6, Block: "0607", BlockOrdinal: 3},
		{Node: "07", Ordinal: 7, Block: "0607", BlockOrdinal: 3},
		{Node: "08", Ordinal: 8, Block: "0809", BlockOrdinal: 4},
		{Node: "09", Ordinal: 9, Block: "0809", BlockOrdinal: 4},
		{Node: "10", Ordinal: 10, Block: "101112", BlockOrdinal: 5},
		{Node: "11", Ordinal: 11, Block: "101112", BlockOrdinal: 5},
		{Node: "12", Ordinal: 12, Block: "101112", BlockOrdinal: 5},
	}
}

// matrixFixtureDict 是迁移种子里字典的浓缩版：符号表覆盖夹具用到的全部原文，
// 状态表给出 free 可用、其余占用键不可用。逐格可用性只由 clean.State 从这份字典
// 推出，测试不另立一套「哪些状态算空闲」的规则（CP4）。
func matrixFixtureDict() model.Dict {
	return model.Dict{
		Symbols: map[string]string{
			"◆":  "class",
			"Ｊ":  "borrowed",
			"Ｘ":  "locked",
			"Κ":  "exam",
			"空闲": "free",
			"Ｇ":  "fixed_reschedule",
			"Ｌ":  "temp_reschedule",
			"M":  "cross_mode",
		},
		States: map[string]model.DictState{
			"class":            {Label: "正常上课"},
			"borrowed":         {Label: "借用"},
			"locked":           {Label: "锁定"},
			"exam":             {Label: "考试"},
			"free":             {Label: "空闲", Available: true},
			"fixed_reschedule": {Label: "固定调课"},
			"temp_reschedule":  {Label: "临时调课"},
			"cross_mode":       {Label: "跨模式占用"},
			"composite":        {Label: "复合占用"},
			"unknown":          {Label: "未知"},
		},
	}
}

// matrixFixtureSnapshot 返回一份版本号与内容都齐备的快照，供抓取器使用。
func matrixFixtureSnapshot() store.CollectSnapshot {
	return store.CollectSnapshot{
		DictVersion: 1,
		Dict:        matrixFixtureDict(),
		AxisVersion: 1,
		Axis:        matrixFixtureAxis(),
	}
}

// matrixHeaderRow 拼表头行：36 个 td，其中 35 个带 tdvalue，第一个房名列不带。
// 状态格按「每天重复 5 大节」展开，与 parser 的 axisBlocks 口径一致。
func matrixHeaderRow() string {
	var b strings.Builder
	b.WriteString(`<tr height="60"><td></td>`)
	for day := 0; day < 7; day++ {
		for _, block := range matrixFixtureBlocks {
			b.WriteString(`<td tdvalue="` + block + `">` + block + `</td>`)
		}
	}
	b.WriteString(`</tr>`)
	return b.String()
}

// matrixDataRow 拼一个数据行：首列房名列 + 35 个状态格。
// states 是「状态格序号（从 0 开始）→ 原文」，没列出的格子写空文本（空白格即空闲）。
// 序号按「星期 × 大节」展开：序号 i 对应星期 i/5+1、大节 matrixFixtureBlocks[i%5]。
func matrixDataRow(roomID string, states map[int]string) string {
	var b strings.Builder
	b.WriteString(`<tr jsbh="` + roomID + `">`)
	b.WriteString(`<td><input type="checkbox" value="` + roomID + `">` + roomID + `</td>`)
	for i := 0; i < 7*len(matrixFixtureBlocks); i++ {
		b.WriteString("<td>" + states[i] + "</td>")
	}
	b.WriteString("</tr>")
	return b.String()
}

// matrixFixturePage 拼一张完整矩阵页：目标表按 id 精确定位，页首另放一张同前缀的
// table#dataListHeader（与真实页面一致），实现若退化成前缀匹配会先解析到假表。
func matrixFixturePage(rows ...string) string {
	return `<html><body><center id="alldiv">` +
		`<table id="dataListHeader" name="kbDataTable" width="100%">` +
		matrixHeaderRow() +
		matrixDataRow("H0001", nil) +
		`</table>` +
		`<table id="dataList" name="kbDataTable" border="1">` +
		matrixHeaderRow() + strings.Join(rows, "") +
		`</table></center></body></html>`
}

// matrixUpstream 是只实现 FetchWeekMatrix 的假上游：按入参预置正文或错误，
// 并记下每次调用的入参，供「每 (楼,周) 至多一次请求」的断言使用。
//
// 内嵌 upstreamAPI 只为凑齐 Collector.upstream 的方法集：本项的抓取路径只调
// FetchWeekMatrix，其余方法一旦被调用会因内嵌接口是 nil 而 panic——这正是我们想要的
// 「本项不该碰别的上游能力」的报警。
type matrixUpstream struct {
	upstreamAPI
	bodies map[upstream.WeekMatrixParams][]byte
	errs   map[upstream.WeekMatrixParams]error
	calls  []upstream.WeekMatrixParams
}

// FetchWeekMatrix 返回预置的正文；未预置的参数按测试夹具缺口处理（直接报错），
// 免得「忘了预置」被当成一次普通的网络失败混过去。
func (u *matrixUpstream) FetchWeekMatrix(_ context.Context, params upstream.WeekMatrixParams) ([]byte, error) {
	u.calls = append(u.calls, params)
	if err := u.errs[params]; err != nil {
		return nil, err
	}
	body, ok := u.bodies[params]
	if !ok {
		return nil, fmt.Errorf("测试未预置周矩阵 %+v", params)
	}
	return body, nil
}

// newTaskFetcher 组装「只有上游」的执行器并返回统一的任务抓取闭包。
// 抓取器只读 c.upstream 与注入的快照/候选，其余依赖不参与，所以不必走 New。
// 用例按任务种类走分派，不直接调矩阵/明细两条私有路径——它们的入口就是这层分派。
func newTaskFetcher(t *testing.T, u upstreamAPI, snapshot store.CollectSnapshot, candidates *bank) pipeline.TaskFetcher {
	t.Helper()
	c := &Collector{upstream: u}
	return c.taskFetcher("2026-2027-1", "01", snapshot, candidates)
}

// matrixTask 造一条周矩阵任务。
func matrixTask(buildingID string, week int) pipeline.Task {
	return pipeline.Task{
		Kind:       pipeline.TaskWeekMatrix,
		Required:   true,
		BuildingID: buildingID,
		Week:       week,
	}
}

// wantCell 是一条期望的清洗结果：语义键与可用性。
type wantCell struct {
	StateKey  string
	Available bool
}

// assertCell 断言某格被逐格清洗成了期望的状态。
func assertCell(t *testing.T, candidates *bank, key cellKey, want wantCell) {
	t.Helper()
	for _, page := range candidates.pages {
		for _, cell := range page.Cells {
			got := cellKey{RoomID: cell.RoomID, Weekday: cell.Weekday, Block: cell.Block}
			if got != key {
				continue
			}
			if cell.State.StateKey != want.StateKey || cell.State.Available != want.Available {
				t.Fatalf("格 %+v 的清洗结果期望 %+v，实际 {StateKey:%q Available:%v RawText:%q}",
					key, want, cell.State.StateKey, cell.State.Available, cell.State.RawText)
			}
			return
		}
	}
	t.Fatalf("候选里没有格 %+v", key)
}

// TestWeekMatrixFetcherCachesPagePerBuildingWeek 覆盖 Requirement 6.1、6.3 与 CP10：
// 同一 (楼,周) 抓两次只打一次上游、只留一份候选；返回的正文与房号行来自同一次抓取，
// 且每一格都已用本轮字典投影过。
func TestWeekMatrixFetcherCachesPagePerBuildingWeek(t *testing.T) {
	params := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 9}
	body := []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{
		0:  "◆",  // 周一第 1 大节：正常上课 → 不可用
		1:  "空闲", // 周一第 2 大节：空闲 → 可用
		5:  "J",  // 周二第 1 大节：半角 J 不在字典 → 未知 → 不可用
		34: "◆Ｊ", // 周日第 5 大节：复合 → 不可用
	})))
	u := &matrixUpstream{bodies: map[upstream.WeekMatrixParams][]byte{params: body}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	task := matrixTask("Q10", 9)
	first, err := fetch(context.Background(), task)
	if err != nil {
		t.Fatalf("第一次抓取应成功，实际 err=%v", err)
	}
	second, err := fetch(context.Background(), task)
	if err != nil {
		t.Fatalf("第二次抓取应命中缓存，实际 err=%v", err)
	}

	if len(u.calls) != 1 {
		t.Fatalf("同一 (楼,周) 每轮至多请求一次（CP10），实际 %d 次：%+v", len(u.calls), u.calls)
	}
	if !bytes.Equal(first.Page, body) || !bytes.Equal(second.Page, body) {
		t.Fatal("两次返回的页面正文都应原样带回（pipeline 要用它算 sha256）")
	}
	if len(first.Rooms) != 1 || len(second.Rooms) != 1 || first.Rooms[0].RoomID != "R0001" {
		t.Fatalf("两次返回的房号行应来自同一次抓取，实际 first=%+v second=%+v", first.Rooms, second.Rooms)
	}

	if len(candidates.pages) != 1 {
		t.Fatalf("同一 (楼,周) 只应留一份候选，实际 %d 份", len(candidates.pages))
	}
	key := weekKey{BuildingID: "Q10", Week: 9}
	page, ok := candidates.pages[key]
	if !ok {
		t.Fatalf("候选里缺少 %+v", key)
	}
	if want := 7 * len(matrixFixtureBlocks); len(page.Cells) != want {
		t.Fatalf("一页应摊平成 %d 个格，实际 %d 个", want, len(page.Cells))
	}

	// 逐格 clean.State：空文本判空闲、字典符号按状态表给可用性、未知与复合恒不可用。
	assertCell(t, candidates, cellKey{RoomID: "R0001", Weekday: 1, Block: "0102"},
		wantCell{StateKey: "class", Available: false})
	assertCell(t, candidates, cellKey{RoomID: "R0001", Weekday: 1, Block: "030405"},
		wantCell{StateKey: model.StateKeyFree, Available: true})
	assertCell(t, candidates, cellKey{RoomID: "R0001", Weekday: 2, Block: "0102"},
		wantCell{StateKey: model.StateKeyUnknown, Available: false})
	assertCell(t, candidates, cellKey{RoomID: "R0001", Weekday: 7, Block: "101112"},
		wantCell{StateKey: model.StateKeyComposite, Available: false})
	assertCell(t, candidates, cellKey{RoomID: "R0001", Weekday: 3, Block: "0607"},
		wantCell{StateKey: model.StateKeyFree, Available: true})

	// 不可用格只记不可用这一事实：◆、半角 J、◆Ｊ 三个，空闲格不进。
	wantUnavailable := []cellKey{
		{RoomID: "R0001", Weekday: 1, Block: "0102"},
		{RoomID: "R0001", Weekday: 2, Block: "0102"},
		{RoomID: "R0001", Weekday: 7, Block: "101112"},
	}
	if len(candidates.unavailable) != len(wantUnavailable) {
		t.Fatalf("不可用格应有 %d 个，实际 %d 个：%+v", len(wantUnavailable), len(candidates.unavailable), candidates.unavailable)
	}
	for _, key := range wantUnavailable {
		if _, ok := candidates.unavailable[key]; !ok {
			t.Fatalf("不可用格 %+v 没有被记下", key)
		}
	}
}

// TestWeekMatrixFetcherDedupesUnavailableAcrossWeeks 覆盖 Requirement 6.2：
// 不可用格按「房号 × 星期 × 大节」去重、跨周取并集——同一格两周都不可用只记一条；
// 前一周不可用、后一周可用仍然算一格；前一周可用、后一周不可用也要记上。
func TestWeekMatrixFetcherDedupesUnavailableAcrossWeeks(t *testing.T) {
	week9 := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 9}
	week10 := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 10}
	u := &matrixUpstream{bodies: map[upstream.WeekMatrixParams][]byte{
		// 第 9 周：周一 3 格都不可用，周二第 1 大节可用。
		week9: []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆", 2: "◆", 3: "◆", 5: "空闲"}))),
		// 第 10 周：周一第 3 格变为空闲，周二第 1 大节变为不可用。
		week10: []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆", 2: "空闲", 3: "", 5: "◆"}))),
	}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	for _, task := range []pipeline.Task{matrixTask("Q10", 9), matrixTask("Q10", 10)} {
		if _, err := fetch(context.Background(), task); err != nil {
			t.Fatalf("抓取第 %d 周应成功，实际 err=%v", task.Week, err)
		}
	}
	if len(candidates.pages) != 2 {
		t.Fatalf("两周应各留一份候选，实际 %d 份", len(candidates.pages))
	}

	// 周一第 1 大节两周都不可用：去重后只一条。
	// 周一第 3 大节只在第 9 周不可用：并集保留。
	// 周一第 4 大节只在第 9 周不可用（第 10 周空白=空闲）：并集保留。
	// 周二第 1 大节只在第 10 周不可用：并集追加。
	want := []cellKey{
		{RoomID: "R0001", Weekday: 1, Block: "0102"},
		{RoomID: "R0001", Weekday: 1, Block: "0607"},
		{RoomID: "R0001", Weekday: 1, Block: "0809"},
		{RoomID: "R0001", Weekday: 2, Block: "0102"},
	}
	if len(candidates.unavailable) != len(want) {
		t.Fatalf("跨周去重后应有 %d 个不可用格，实际 %d 个：%+v", len(want), len(candidates.unavailable), candidates.unavailable)
	}
	for _, key := range want {
		if _, ok := candidates.unavailable[key]; !ok {
			t.Fatalf("不可用格 %+v 没有被记下", key)
		}
	}
}

// TestWeekMatrixFetcherReturnsStructureError 覆盖 Requirement 6.4：结构失败原样返回给
// pipeline（由它按 parser 哨兵映射 matrix_structure_invalid / axis_changed 并把任务置
// failed）；坏页面不写进度候选，下一次调用仍会重新请求，而不是把坏结果当成「已抓」。
func TestWeekMatrixFetcherReturnsStructureError(t *testing.T) {
	params := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 9}
	cases := []struct {
		name     string
		body     string
		snapshot store.CollectSnapshot
		want     error
	}{
		{
			name:     "页面没有目标表",
			body:     `<html><body>不是矩阵页</body></html>`,
			snapshot: matrixFixtureSnapshot(),
			want:     parser.ErrMatrixStructureInvalid,
		},
		{
			name:     "数据行宽度与表头不一致",
			body:     `<html><body><table id="dataList">` + matrixHeaderRow() + `<tr jsbh="R0001"><td>R0001</td></tr></table></body></html>`,
			snapshot: matrixFixtureSnapshot(),
			want:     parser.ErrMatrixStructureInvalid,
		},
		{
			name: "快照里没有节次轴",
			body: matrixFixturePage(matrixDataRow("R0001", nil)),
			snapshot: store.CollectSnapshot{
				DictVersion: 1,
				Dict:        matrixFixtureDict(),
				AxisVersion: 1,
			},
			want: parser.ErrAxisInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &matrixUpstream{bodies: map[upstream.WeekMatrixParams][]byte{params: []byte(tc.body)}}
			candidates := newBank()
			fetch := newTaskFetcher(t, u, tc.snapshot, candidates)

			_, err := fetch(context.Background(), matrixTask("Q10", 9))
			if !errors.Is(err, tc.want) {
				t.Fatalf("结构失败应原样返回 %v，实际 %v", tc.want, err)
			}
			if len(candidates.pages) != 0 {
				t.Fatalf("结构失败时不应写入候选，实际 %d 份", len(candidates.pages))
			}
			if len(candidates.unavailable) != 0 {
				t.Fatalf("结构失败时不应记不可用格，实际 %+v", candidates.unavailable)
			}

			// 坏页面不进缓存：再抓一次仍会请求上游，而不是拿同一份坏结果糊过去。
			if _, err := fetch(context.Background(), matrixTask("Q10", 9)); err == nil {
				t.Fatal("结构失败后重复抓取仍应失败")
			}
			if len(u.calls) != 2 {
				t.Fatalf("坏页面不应被缓存，期望重试仍请求上游（2 次），实际 %d 次", len(u.calls))
			}
		})
	}
}

// dispatchUpstream 同时实现矩阵与明细两条上游路径，供「按种类分派」的用例观测
// 每个任务实际走了哪条路径。内嵌 upstreamAPI 只为凑齐方法集：分派之外的任何
// 上游能力一旦被碰会因 nil 而 panic。
type dispatchUpstream struct {
	upstreamAPI
	matrixBody  []byte
	detailBody  []byte
	matrixCalls []upstream.WeekMatrixParams
	detailCalls []upstream.OccupancyDetailParams
}

// FetchWeekMatrix 返回预置的矩阵正文并记入参。
func (u *dispatchUpstream) FetchWeekMatrix(_ context.Context, params upstream.WeekMatrixParams) ([]byte, error) {
	u.matrixCalls = append(u.matrixCalls, params)
	return u.matrixBody, nil
}

// FetchOccupancyDetail 返回预置的明细正文并记入参。
func (u *dispatchUpstream) FetchOccupancyDetail(_ context.Context, params upstream.OccupancyDetailParams) ([]byte, error) {
	u.detailCalls = append(u.detailCalls, params)
	return u.detailBody, nil
}

// TestTaskFetcherDispatchesByKind 钉住统一抓取器的分派：周矩阵任务走矩阵路径、
// 占用说明任务走明细路径、未知种类明确报错且不碰任何上游。
//
// 这条用例替代了「两个闭包各自拒绝对方的任务」——按段各给一个闭包的旧形状会让
// 第一段拿到库里遗留的 pending 明细任务时把它拒成永久 failed（Requirement 8.1、
// 10.4），分派后同一个闭包在任何一段都能把两种任务各走各的路。
func TestTaskFetcherDispatchesByKind(t *testing.T) {
	matrixBody := []byte(matrixFixturePage(matrixDataRow("R0001", nil)))
	detailBody := detailFixturePage(detailFieldRow("教室状态：", "空闲"))
	u := &dispatchUpstream{matrixBody: matrixBody, detailBody: detailBody}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	fetched, err := fetch(context.Background(), matrixTask("Q10", 9))
	if err != nil {
		t.Fatalf("周矩阵任务应走矩阵路径，实际 err=%v", err)
	}
	wantMatrix := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 9}
	if len(u.matrixCalls) != 1 || u.matrixCalls[0] != wantMatrix {
		t.Fatalf("矩阵入参期望 %+v，实际 %+v", wantMatrix, u.matrixCalls)
	}
	if len(u.detailCalls) != 0 {
		t.Fatalf("矩阵任务不应碰明细上游，实际 %+v", u.detailCalls)
	}
	if !bytes.Equal(fetched.Page, matrixBody) {
		t.Fatal("矩阵正文应原样带回（pipeline 要用它算 sha256）")
	}

	if _, err := fetch(context.Background(), detailTask("R0001", 1, "0102")); err != nil {
		t.Fatalf("明细任务应走明细路径，实际 err=%v", err)
	}
	wantDetail := upstream.OccupancyDetailParams{
		Term: "2026-2027-1", KBJCMSID: "01",
		RoomID: "R0001", Weekday: 1, BigSlot: "0102",
	}
	if len(u.detailCalls) != 1 || u.detailCalls[0] != wantDetail {
		t.Fatalf("明细入参期望 %+v，实际 %+v", wantDetail, u.detailCalls)
	}
	if len(u.matrixCalls) != 1 {
		t.Fatalf("明细任务不应碰矩阵上游，实际 %+v", u.matrixCalls)
	}
	if len(candidates.pages) != 1 || len(candidates.details) != 1 {
		t.Fatalf("两条路径应各留一份候选，实际 pages=%d details=%d",
			len(candidates.pages), len(candidates.details))
	}

	// 未知种类：明确报错，且两种上游都不请求。
	if _, err := fetch(context.Background(), pipeline.Task{Kind: pipeline.TaskKind("unknown")}); err == nil {
		t.Fatal("未知任务种类应报错")
	}
	if len(u.matrixCalls) != 1 || len(u.detailCalls) != 1 {
		t.Fatalf("未知种类不应请求上游，实际矩阵 %d 次、明细 %d 次",
			len(u.matrixCalls), len(u.detailCalls))
	}
}

// TestWeekMatrixFetcherAcceptsHeaderOnlyPage 钉住「只有表头没有数据行」的分支：
// 上游对一栋没有教室的楼就是这样返回的，parser 把它当合法空页，抓取器应缓存并返回
// 空房号行，不能按结构失败处理（那样会把正常的空楼判成页面变更）。
func TestWeekMatrixFetcherAcceptsHeaderOnlyPage(t *testing.T) {
	params := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q99", Week: 9}
	body := []byte(matrixFixturePage())
	u := &matrixUpstream{bodies: map[upstream.WeekMatrixParams][]byte{params: body}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	fetched, err := fetch(context.Background(), matrixTask("Q99", 9))
	if err != nil {
		t.Fatalf("只有表头的矩阵页是合法空页，实际 err=%v", err)
	}
	if len(fetched.Rooms) != 0 {
		t.Fatalf("只有表头时不应解析出房号，实际 %+v", fetched.Rooms)
	}
	if !bytes.Equal(fetched.Page, body) {
		t.Fatal("空页的正文也要原样带回（pipeline 仍要为它写 sha256）")
	}
	page, ok := candidates.pages[weekKey{BuildingID: "Q99", Week: 9}]
	if !ok || len(page.Cells) != 0 {
		t.Fatalf("空页应留一份没有格子的候选，实际 ok=%v page=%+v", ok, page)
	}
	if len(candidates.unavailable) != 0 {
		t.Fatalf("空页不应产生不可用格，实际 %+v", candidates.unavailable)
	}
}

// TestWeekMatrixFetcherPropagatesUpstreamError 钉住网络/会话失败原样上抛：
// 抓取器不重试、不包装，失败也不写候选，留给 pipeline 的失败分层处理
// （非结构失败不写业务码）。
func TestWeekMatrixFetcherPropagatesUpstreamError(t *testing.T) {
	params := upstream.WeekMatrixParams{Term: "2026-2027-1", BuildingID: "Q10", Week: 9}
	upstreamErr := fmt.Errorf("%w: 读取第 9 周矩阵超时", model.ErrUpstreamFailed)
	u := &matrixUpstream{errs: map[upstream.WeekMatrixParams]error{params: upstreamErr}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	_, err := fetch(context.Background(), matrixTask("Q10", 9))
	if !errors.Is(err, model.ErrUpstreamFailed) {
		t.Fatalf("上游失败应原样返回，实际 %v", err)
	}
	if len(candidates.pages) != 0 {
		t.Fatalf("上游失败时不应写入候选，实际 %d 份", len(candidates.pages))
	}
}

// TestMatrixFixtureCoversSeedOrder 是夹具自检：两个顺序敏感的地方一旦漂移，
// 上面所有「按序号写状态」的断言都会指向错的格子。
//   - 表头状态格的 tdvalue 顺序必须是「每天重复 5 大节」；
//   - 数据行的状态格数必须是 7 × 5。
func TestMatrixFixtureCoversSeedOrder(t *testing.T) {
	seen := make(map[string]struct{}, len(matrixFixtureBlocks))
	for _, block := range matrixFixtureBlocks {
		if _, ok := seen[block]; ok {
			t.Fatalf("夹具的大节编码应互不相同，实际 %v", matrixFixtureBlocks)
		}
		seen[block] = struct{}{}
	}
	header := matrixHeaderRow()
	if got, want := strings.Count(header, `tdvalue="`), 7*len(matrixFixtureBlocks); got != want {
		t.Fatalf("表头应带 %d 个 tdvalue，实际 %d 个", want, got)
	}
	row := matrixDataRow("R0001", nil)
	if got, want := strings.Count(row, "<td"), 1+7*len(matrixFixtureBlocks); got != want {
		t.Fatalf("数据行应有 %d 个 td，实际 %d 个", want, got)
	}
}

// detailUpstream 是只实现 FetchOccupancyDetail 的假上游：按调用次序回放预置正文或
// 错误，并记下每次入参。内嵌 upstreamAPI 只为凑齐方法集——明细路径一旦碰别的上游
// 能力就会因 nil 而 panic。
type detailUpstream struct {
	upstreamAPI
	bodies [][]byte
	errs   []error
	calls  []upstream.OccupancyDetailParams
}

// FetchOccupancyDetail 返回第 index 次预置结果；没预置时按夹具缺口报错，
// 免得「忘了预置」被当成一次普通网络失败混过去。
func (u *detailUpstream) FetchOccupancyDetail(_ context.Context, params upstream.OccupancyDetailParams) ([]byte, error) {
	index := len(u.calls)
	u.calls = append(u.calls, params)
	if index < len(u.errs) && u.errs[index] != nil {
		return nil, u.errs[index]
	}
	if index < len(u.bodies) {
		return u.bodies[index], nil
	}
	return nil, fmt.Errorf("测试未预置第 %d 次明细响应（%+v）", index+1, params)
}

// detailTask 造一条占用说明任务。BuildingID 只是规划产物，明细请求本身不用它。
func detailTask(roomID string, weekday int, block string) pipeline.Task {
	return pipeline.Task{
		Kind:       pipeline.TaskCellDetail,
		Required:   false,
		BuildingID: "Q10",
		RoomID:     roomID,
		Weekday:    weekday,
		Block:      block,
	}
}

// detailFixturePage 拼一张明细页（标题表 + 表单里的三列字段行），结构与 parser 的夹具一致。
// 这是本包唯一一份明细页夹具：抓取器用例与两段式编排用例都复用它，避免两份结构各改一半。
func detailFixturePage(fieldRows ...string) []byte {
	var b strings.Builder
	b.WriteString(`<html><body>`)
	b.WriteString(`<table id="tab1"><tr><td class="toolstitle">教室占用情况</td></tr></table>`)
	b.WriteString(`<form action="/jsxsd/jspyfa/selectJx02"><table border="1"><tr><td><table border="0">`)
	for _, row := range fieldRows {
		b.WriteString(row)
	}
	b.WriteString(`</table></td></tr></table></form></body></html>`)
	return []byte(b.String())
}

// detailFieldRow 拼一行合法字段：标签 / 空列 / 值。
func detailFieldRow(label, value string) string {
	return `<tr><td width="173" align="right">` + label + `</td>` +
		`<td width="9"></td><td width="560">` + value + `</td></tr>`
}

// TestCellDetailFetcherStoresFourFieldsAndOverwrites 覆盖 Requirement 7.1–7.4：
// 明细页经 parser.Occupancy → clean.State 后，四项字段（状态、课程、周次范围、
// 时间标志）写进 details[(房,天,大节)]；同一格重放整片覆盖，不追加第二份。
func TestCellDetailFetcherStoresFourFieldsAndOverwrites(t *testing.T) {
	firstPage := detailFixturePage(
		detailFieldRow("教室状态：", "◆"),
		detailFieldRow("课程：", "高等数学"),
		detailFieldRow("周次：", "1-16"),
		detailFieldRow("时间标志：", "单双周"),
		detailFieldRow("教室状态：", "空闲"),
		detailFieldRow("课程：", "自习"),
		detailFieldRow("周次：", "17"),
		detailFieldRow("时间标志：", ""),
	)
	secondPage := detailFixturePage(
		detailFieldRow("教室状态：", "空闲"),
		detailFieldRow("课程：", "临时调整"),
		detailFieldRow("周次：", "9-10"),
		detailFieldRow("时间标志：", "双周"),
	)

	params := upstream.OccupancyDetailParams{
		Term:     "2026-2027-1",
		KBJCMSID: "01",
		RoomID:   "R0001",
		Weekday:  1,
		BigSlot:  "0102",
	}
	u := &detailUpstream{bodies: [][]byte{firstPage, secondPage}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	fetched, err := fetch(context.Background(), detailTask("R0001", 1, "0102"))
	if err != nil {
		t.Fatalf("第一次明细抓取应成功，实际 err=%v", err)
	}
	if len(fetched.Page) != 0 || len(fetched.Rooms) != 0 {
		t.Fatalf("明细抓取不应把正文或房号行交给执行循环，实际 %+v", fetched)
	}
	if len(u.calls) != 1 || u.calls[0] != params {
		t.Fatalf("明细入参期望 %+v，实际 %+v", params, u.calls)
	}

	key := cellKey{RoomID: "R0001", Weekday: 1, Block: "0102"}
	rows := candidates.details[key]
	if len(rows) != 2 {
		t.Fatalf("一格的两条说明都应保留（Requirement 7.2），实际 %d 条：%+v", len(rows), rows)
	}
	if rows[0].State.StateKey != "class" || rows[0].State.Available {
		t.Fatalf("◆ 应投影成不可用的 class，实际 %+v", rows[0].State)
	}
	if rows[0].State.RawText != "◆" {
		t.Fatalf("状态原文应原样保留，实际 %q", rows[0].State.RawText)
	}
	if rows[0].Course != "高等数学" || rows[0].WeekRange != "1-16" || rows[0].TimeFlag != "单双周" {
		t.Fatalf("四项字段应原样写入，实际 %+v", rows[0])
	}
	if rows[1].State.StateKey != model.StateKeyFree || !rows[1].State.Available {
		t.Fatalf("空闲应投影成可用的 free，实际 %+v", rows[1].State)
	}

	// 同一格第二次抓取：整片覆盖，不追加。
	if _, err := fetch(context.Background(), detailTask("R0001", 1, "0102")); err != nil {
		t.Fatalf("第二次明细抓取应成功，实际 err=%v", err)
	}
	rows = candidates.details[key]
	if len(rows) != 1 {
		t.Fatalf("同一格重放应整片覆盖，实际留下 %d 条：%+v", len(rows), rows)
	}
	if rows[0].Course != "临时调整" || rows[0].WeekRange != "9-10" || rows[0].TimeFlag != "双周" {
		t.Fatalf("重放应只留后一次响应，实际 %+v", rows[0])
	}
}

// TestCellDetailFetcherReturnsStructureError 覆盖 Requirement 7.3：明细页连标题表都
// 没有时原样返回 parser 的结构失败哨兵，且什么都不写进 details（坏结果不能冒充已抓）。
func TestCellDetailFetcherReturnsStructureError(t *testing.T) {
	u := &detailUpstream{bodies: [][]byte{[]byte(`<html><body>不是明细页</body></html>`)}}
	candidates := newBank()
	fetch := newTaskFetcher(t, u, matrixFixtureSnapshot(), candidates)

	_, err := fetch(context.Background(), detailTask("R0002", 2, "030405"))
	if !errors.Is(err, parser.ErrDetailStructureInvalid) {
		t.Fatalf("结构失败应原样返回 ErrDetailStructureInvalid，实际 %v", err)
	}
	if len(candidates.details) != 0 {
		t.Fatalf("结构失败时不应写入明细候选，实际 %+v", candidates.details)
	}

	// 坏页面不进缓存：再抓一次仍会请求上游。
	if _, err := fetch(context.Background(), detailTask("R0002", 2, "030405")); err == nil {
		t.Fatal("结构失败后重复抓取仍应失败")
	}
	if len(u.calls) != 2 {
		t.Fatalf("坏页面不应被当成已抓，期望重试仍请求上游（2 次），实际 %d 次", len(u.calls))
	}
}
