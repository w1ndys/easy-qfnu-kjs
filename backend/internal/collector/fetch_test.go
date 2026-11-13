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

// newMatrixFetcher 组装「只有上游」的执行器并返回周矩阵抓取闭包。
// 抓取器只读 c.upstream 与注入的快照/候选，其余依赖不参与，所以不必走 New。
func newMatrixFetcher(t *testing.T, u *matrixUpstream, snapshot store.CollectSnapshot, candidates *bank) pipeline.TaskFetcher {
	t.Helper()
	c := &Collector{upstream: u}
	return c.weekMatrixTaskFetcher("2026-2027-1", snapshot, candidates)
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
	fetch := newMatrixFetcher(t, u, matrixFixtureSnapshot(), candidates)

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
	fetch := newMatrixFetcher(t, u, matrixFixtureSnapshot(), candidates)

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
			fetch := newMatrixFetcher(t, u, tc.snapshot, candidates)

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

// TestWeekMatrixFetcherRejectsOtherTaskKinds 钉住抓取闭包的边界：它只服务周矩阵段，
// 混进明细任务说明两段式编排接错了，应该直接报错而不是拿矩阵参数去打一次上游。
func TestWeekMatrixFetcherRejectsOtherTaskKinds(t *testing.T) {
	u := &matrixUpstream{}
	fetch := newMatrixFetcher(t, u, matrixFixtureSnapshot(), newBank())

	_, err := fetch(context.Background(), pipeline.Task{
		Kind:       pipeline.TaskCellDetail,
		BuildingID: "Q10",
		RoomID:     "R0001",
		Weekday:    1,
		Block:      "0102",
	})
	if err == nil {
		t.Fatal("明细任务不应交给周矩阵抓取器")
	}
	if len(u.calls) != 0 {
		t.Fatalf("拒绝的任务不应请求上游，实际 %d 次", len(u.calls))
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
	fetch := newMatrixFetcher(t, u, matrixFixtureSnapshot(), candidates)

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
	fetch := newMatrixFetcher(t, u, matrixFixtureSnapshot(), candidates)

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
