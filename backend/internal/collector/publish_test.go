package collector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 本文件覆盖 tasklist 第 10 项「发布接线」的六条单测：success、partial、候选集为空、
// 校验拦截、发布事务失败、以及 Commit 成功后不再调用任何收口函数。
//
// 另有两条返修用例（复审 1 major + 1 minor）：
//   - TestExecuteFailsWhenBuildReadFails：Build 的仓储读取失败不编码，不能被记成
//     validation_blocked；
//   - TestPublishRunPublishesDroppedOptional：publishRun 收到的 DroppedOptional 必须
//     进草稿（partial + 丢弃说明），CP7 的回归。
//
// 全部用例走真实 pipeline / publish / parser / clean，只把上游、设置与存储换成
// stageFake：发布链路的判断（候选集组装、发布门槛、终态口径）都在真实实现里，
// 替身只负责提供本轮事实并按 store 的语义记账。

// successObservationPerWeek 是成功用例里窗口内每周的观测行数：
// 2 间房 × 7 天 × 12 小节（5 大节按轴展开）= 168。
const successObservationPerWeek = 168

// TestExecutePublishesSuccess 覆盖 Requirement 9.1：必需与可选任务全部完成时，
// 一轮按 success 发布，且发布顺序是 第二段判定 → AdvancePhase(validate) → Build →
// Validate → Commit。
//
// 断言分四组：
//  1. 顺序：第二次收口判定之后才推进 validate，Build 的读库（facts）早于 Commit；
//  2. RunInput 的组装：版本号与轴来自本轮快照，学期与窗口来自本轮落库的值，
//     候选集只含归属快照里的楼与房号，观测按「白名单楼 × 周次升序」摊平；
//  3. 终态由发布事务写：草稿状态 success、无错误原因，执行体一次收口函数都没调；
//  4. 明细原样进候选集：四项字段与状态键都对得上。
func TestExecutePublishesSuccess(t *testing.T) {
	f := newStageFake()
	// R0001 全空闲；R0002 周一第 1 大节不可用，触发一条明细任务。
	f.matrixBody = []byte(matrixFixturePage(
		matrixDataRow("R0001", nil),
		matrixDataRow("R0002", map[int]string{0: "◆"}),
	))
	// 明细页里的状态原文用字典里的 ◆（class，不可用），四项字段各一行。
	f.detailBody = detailFixturePage(
		detailFieldRow("教室状态：", "◆"),
		detailFieldRow("课程：", "高等数学"),
		detailFieldRow("周次：", "1-16"),
		detailFieldRow("时间标志：", "单双周"),
	)

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("发布成功时执行体不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("发布事务已写终态，不应再调用收口函数，实际 %+v", finishes)
	}
	if f.commitCalls != 1 || len(f.commits) != 1 {
		t.Fatalf("success 应恰好提交一次发布事务，实际 %d 次：%+v", f.commitCalls, f.commits)
	}

	// 顺序：EvaluateRun2 → AdvancePhase(validate) → Build → Commit。
	evaluate2 := nthEvent(t, f.events, "summary", 1)
	validatePhase := nthEvent(t, f.events, "phase:validate", 0)
	build := nthEvent(t, f.events, "facts", 0)
	commit := nthEvent(t, f.events, "commit", 0)
	if evaluate2 >= validatePhase || validatePhase >= build || build >= commit {
		t.Fatalf("发布顺序应为 收口判定 → validate → Build → Commit，实际序列 %v", f.events)
	}

	draft := f.commits[0]
	if draft.RunID != executeRunID {
		t.Fatalf("草稿的轮次期望 %d，实际 %d", executeRunID, draft.RunID)
	}
	if draft.Status != store.RunStatusSuccess {
		t.Fatalf("全部完成应发布 success，实际 %q（原因 %q）", draft.Status, draft.ErrorMessage)
	}
	if draft.ErrorMessage != "" {
		t.Fatalf("success 不应带错误原因，实际 %q", draft.ErrorMessage)
	}
	if draft.Term != "2026-2027-1" {
		t.Fatalf("学期应取本轮落库值 %q，实际 %q", "2026-2027-1", draft.Term)
	}
	if draft.WindowStart != 9 || draft.WindowEnd != 12 || draft.WeeksTotal != 4 {
		t.Fatalf("窗口应为 9..12（4 周），实际 %d..%d（%d 周）",
			draft.WindowStart, draft.WindowEnd, draft.WeeksTotal)
	}
	if draft.DictVersion != 1 || draft.AxisVersion != 1 {
		t.Fatalf("版本号应取本轮快照的 1/1，实际 %d/%d", draft.DictVersion, draft.AxisVersion)
	}
	if !draft.GeneratedAt.Equal(executeNow) {
		t.Fatalf("版本生成时间应取注入的时钟 %v，实际 %v", executeNow, draft.GeneratedAt)
	}
	if !draft.UpdatedAt.Equal(executeNow) {
		t.Fatalf("发布时刻应取注入的时钟 %v，实际 %v", executeNow, draft.UpdatedAt)
	}

	// 归属快照来自 collect_run_room：两间房都归 Q10，展示名取白名单。
	if len(draft.Rooms) != 2 {
		t.Fatalf("归属快照应有 2 行，实际 %d 行：%+v", len(draft.Rooms), draft.Rooms)
	}
	for _, room := range draft.Rooms {
		if room.BuildingID != "Q10" || room.BuildingName != "综合楼" {
			t.Fatalf("归属快照应带 Q10 / 综合楼，实际 %+v", room)
		}
	}

	// 2 间房 × 4 周 × 84（7 天 × 12 小节）= 672 行观测，逐周 168 行。
	if len(draft.Observations) != 672 {
		t.Fatalf("观测行数期望 672，实际 %d", len(draft.Observations))
	}
	perWeek := make(map[int]int, 4)
	for _, observation := range draft.Observations {
		perWeek[observation.Week]++
	}
	for week := 9; week <= 12; week++ {
		if perWeek[week] != successObservationPerWeek {
			t.Fatalf("第 %d 周应有 %d 行观测，实际 %d：%+v",
				week, successObservationPerWeek, perWeek[week], perWeek)
		}
	}
	// 摊平顺序：白名单楼顺序 → 周次升序 → 页内「房号 × 星期 × 大节」。第一条是
	// R0001 第 9 周周一第 1 大节的 01 小节，最后一条是 R0002 第 12 周周日第 5 大节的 12 小节。
	first := draft.Observations[0]
	if first.RoomID != "R0001" || first.Week != 9 || first.Weekday != 1 || first.Node != "01" {
		t.Fatalf("第一条观测应为 R0001 第 9 周周一 01 小节，实际 %+v", first)
	}
	last := draft.Observations[len(draft.Observations)-1]
	if last.RoomID != "R0002" || last.Week != 12 || last.Weekday != 7 || last.Node != "12" {
		t.Fatalf("最后一条观测应为 R0002 第 12 周周日 12 小节，实际 %+v", last)
	}

	// 明细按格摊平，四项原文字段与状态键原样保留；该格有矩阵行，所以没有派生。
	if len(draft.Details) != 1 {
		t.Fatalf("应发布 1 条占用说明，实际 %d 条：%+v", len(draft.Details), draft.Details)
	}
	detail := draft.Details[0]
	if detail.RoomID != "R0002" || detail.Weekday != 1 || detail.Block != "0102" {
		t.Fatalf("占用说明的格身份期望 R0002/周一/0102，实际 %+v", detail)
	}
	if detail.StateKey != "class" || detail.Course != "高等数学" ||
		detail.WeekRange != "1-16" || detail.TimeFlag != "单双周" {
		t.Fatalf("占用说明的四项字段不符，实际 %+v", detail)
	}
	if detail.Derived {
		t.Fatalf("该格有矩阵行，占用说明不应补写派生观测，实际 %+v", detail)
	}
}

// TestExecutePublishesPartial 覆盖 Requirement 9.2：只有可选任务（占用说明）失败时
// 照常发布，轮次终态由发布事务写成 partial，原因取最早失败任务的原因。
//
// 失败源是明细页结构非法（parser.ErrDetailStructureInvalid）：必需矩阵全部完成，
// 所以发布不被拦截，但候选集里没有这条说明，终态必须如实写成 partial 而不是 success。
func TestExecutePublishesPartial(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = []byte(`<html><body>这不是明细页</body></html>`)

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("可选任务失败不应让执行体返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("partial 的终态应由发布事务写，不应调用收口函数，实际 %+v", finishes)
	}
	if f.commitCalls != 1 || len(f.commits) != 1 {
		t.Fatalf("partial 应恰好提交一次发布事务，实际 %d 次：%+v", f.commitCalls, f.commits)
	}

	draft := f.commits[0]
	if draft.Status != store.RunStatusPartial {
		t.Fatalf("明细结构失败应发布 partial，实际 %q", draft.Status)
	}
	if draft.ErrorMessage == "" {
		t.Fatal("partial 应带最早失败任务的原因，实际为空")
	}
	if !strings.Contains(draft.ErrorMessage, "占用说明") {
		t.Fatalf("原因应来自明细页结构失败，实际 %q", draft.ErrorMessage)
	}
	// 明细没拿到，但可用性数据（必需矩阵）完整：观测仍在，说明为空。
	if len(draft.Observations) != 4*84 {
		t.Fatalf("1 间房 × 4 周应有 336 行观测，实际 %d", len(draft.Observations))
	}
	if len(draft.Details) != 0 {
		t.Fatalf("明细结构失败时不应有说明，实际 %d 条", len(draft.Details))
	}
}

// TestExecuteFailsWhenCandidateEmpty 覆盖 Requirement 9.3 的「候选集为空」分支：
// 矩阵页只有表头、没有任何房号行时，必需任务照样全部完成，但候选集没有任何观测，
// 发布组装拒绝整轮，轮次收口 failed + validation_blocked，原 current 不动。
//
// 这条用例刻意让收口判定给出 success（必需 4/4、无可选任务）：拦截必须来自发布
// 组装本身，而不是任务失败分层——否则「零观测」会被当成一次成功发布。
func TestExecuteFailsWhenCandidateEmpty(t *testing.T) {
	f := newStageFake()
	// 只有表头，没有任何数据行：解析出 0 间房，任务全部成功但没有观测。
	f.matrixBody = []byte(matrixFixturePage())

	c := newStageExecutor(t, f)
	// failRun 把原始失败返回给 run() 记 WARN：拦截本身已经收口，返回的是原因而非「没处理」。
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("发布组装拒绝时应把原因返回给 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, codeValidationBlocked)
	if !strings.Contains(finish.ErrorMessage, "候选集为空") {
		t.Fatalf("原因应点明候选集为空，实际 %q", finish.ErrorMessage)
	}
	if f.factsCalls != 1 {
		t.Fatalf("应进入发布组装并读一次轮次事实，实际 %d 次", f.factsCalls)
	}
	if f.commitCalls != 0 {
		t.Fatalf("候选集为空时不应提交发布事务，实际 %d 次", f.commitCalls)
	}
	if f.finishesAfterCommit != 0 {
		t.Fatalf("发布未成功，收口应正常执行一次且不需要第二次，实际发布后收口 %d 次",
			f.finishesAfterCommit)
	}
}

// TestExecuteFailsOnValidateRejection 覆盖 Requirement 9.3 的第二条：候选集非空但
// 发布前校验拦截时，收口 failed + validation_blocked，且一次发布事务都不提交。
//
// 造法是让白名单里 Q10 的展示名为空：publish.Build 能组装出候选集（观测与归属都
// 有），但 publish.Validate 的「归属快照不完整」会拦下整轮。这与上一个用例不同——
// 上一个在组装阶段就被拒，这个必须真的走到 Validate 才拦得住。
func TestExecuteFailsOnValidateRejection(t *testing.T) {
	f := newStageFake()
	f.snapshot.Whitelist = []model.Building{{ID: "Q10"}} // 展示名为空
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", nil)))

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("校验拦截时应把原因返回给 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, codeValidationBlocked)
	if !strings.Contains(finish.ErrorMessage, "归属快照不完整") {
		t.Fatalf("原因应为归属快照不完整，实际 %q", finish.ErrorMessage)
	}
	if !strings.Contains(finish.ErrorMessage, "展示名") {
		t.Fatalf("原因应点明缺的是展示名，实际 %q", finish.ErrorMessage)
	}
	if f.commitCalls != 0 {
		t.Fatalf("校验拦截时不应提交发布事务，实际 %d 次", f.commitCalls)
	}
}

// TestExecuteFailsWhenCommitFails 覆盖 Requirement 9.4：发布事务失败时，事务已整体
// 回滚、轮次还在 running，执行器必须收口 failed（无错误码，原因取仓储文案）。
//
// 不收口会让 collect_run_single_running 一直被这一轮占着，下一次同步只能拿到 409；
// 收口失败又会把「发布失败」变成一条沉默的 WARN，所以这条路径必须有一条用例。
func TestExecuteFailsWhenCommitFails(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", nil)))
	f.commitErr = errors.New("发布事务失败")

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("发布事务失败时应把原因返回给 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "发布事务失败") {
		t.Fatalf("原因应保留仓储文案供排障，实际 %q", finish.ErrorMessage)
	}
	if f.commitCalls != 1 {
		t.Fatalf("发布事务应被尝试一次，实际 %d 次", f.commitCalls)
	}
	if f.finishesAfterCommit != 0 {
		t.Fatalf("发布事务失败后收口是合法路径，不应计入「发布后再收口」，实际 %d 次",
			f.finishesAfterCommit)
	}
}

// TestExecuteDoesNotCloseRunAfterCommit 覆盖 Requirement 9.1、10.5 与 CP8 的最后一条：
// Commit 成功之后不再调用任何收口函数。
//
// 替身的 FinishRun 在发布成功之后会立刻报错并计数（见 stageFake.FinishRun），所以
// 只要执行体在发布后再收一次口，这条用例就会以「执行体返回错误」或「finishesAfterCommit
// 不为 0」失败，而不是靠一个永远为空的 finishCalls 蒙混过去。
//
// 这里用 partial 路径：它是「最容易顺手再收一次口」的场景——任务汇总里有一条 failed，
// 照 CloseRun 的旧习惯会再写一次终态，而发布事务已经写死了 partial。
func TestExecuteDoesNotCloseRunAfterCommit(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = []byte(`<html><body>这不是明细页</body></html>`)

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("Commit 成功后不应再收口（替身会因此报错），实际 %v", err)
	}
	if !f.committed {
		t.Fatal("本轮应已成功提交发布事务，实际没有")
	}
	if f.commitCalls != 1 {
		t.Fatalf("一轮只发布一次，实际提交 %d 次", f.commitCalls)
	}
	if f.finishesAfterCommit != 0 {
		t.Fatalf("Commit 成功后不应再调用 FinishRun，实际 %d 次", f.finishesAfterCommit)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("Commit 成功后不应出现任何收口，实际 %+v", finishes)
	}
	if status := f.commits[0].Status; status != store.RunStatusPartial {
		t.Fatalf("终态应由发布事务写成 partial，实际 %q", status)
	}
}

// TestPublishCellsAndDetailsFilterBySnapshotAndWindow 钉住 RunInput 摊平的两条过滤：
// 窗口外的页、以及快照里没有归属的房号都不进候选集。
//
// 编排用例只能造出「窗口内、已归属」的候选（Plan 只排窗口任务、明细只按快照规划），
// 所以这两条防线只能在这一层直接喂：窗口外的页由续跑（旧窗口）产生，无归属的房号由
// 已废弃的归属产生，两者若漏进候选集，publish.Build 会分别以「观测周越界」与
// 「归属快照不完整」拒绝整轮——拒绝是对的，但这一轮本可以照常发布。
func TestPublishCellsAndDetailsFilterBySnapshotAndWindow(t *testing.T) {
	state := model.Observation{StateKey: "free", Available: true, RawText: "空闲"}
	b := newBank()
	// 第 9 周两间房都在快照里；R0002 这一页会被归属过滤（快照只给 R0001 归属）。
	b.pages[weekKey{BuildingID: "Q10", Week: 9}] = matrixPage{Cells: []cellRow{
		{RoomID: "R0001", Weekday: 1, Block: "0102", State: state},
		{RoomID: "R0002", Weekday: 1, Block: "0102", State: state},
	}}
	// 第 11 周在窗口 9..10 之外：续跑留下的旧页，不能进候选集。
	b.pages[weekKey{BuildingID: "Q10", Week: 11}] = matrixPage{Cells: []cellRow{
		{RoomID: "R0001", Weekday: 1, Block: "0102", State: state},
	}}
	b.details[cellKey{RoomID: "R0001", Weekday: 1, Block: "0102"}] = []cellDetailRow{{
		State: state, Course: "高等数学", WeekRange: "9-10", TimeFlag: "单双周",
	}}
	b.details[cellKey{RoomID: "R0002", Weekday: 1, Block: "0102"}] = []cellDetailRow{{
		State: state, Course: "大学物理", WeekRange: "9-10", TimeFlag: "单双周",
	}}

	rooms := []store.RunRoom{{RoomID: "R0001", BuildingID: "Q10"}}
	cells := b.publishCells([]string{"Q10"}, rooms, pipeline.Window{Start: 9, End: 10})
	if len(cells) != 1 {
		t.Fatalf("窗口过滤 + 归属过滤后应只剩 1 格，实际 %d 格：%+v", len(cells), cells)
	}
	if cells[0].RoomID != "R0001" || cells[0].Week != 9 || cells[0].Weekday != 1 || cells[0].Block != "0102" {
		t.Fatalf("候选格应为 R0001 第 9 周周一 0102，实际 %+v", cells[0])
	}

	details := b.publishDetails(rooms)
	if len(details) != 1 {
		t.Fatalf("明细只应导出快照里有归属的房号，实际 %d 条：%+v", len(details), details)
	}
	if details[0].RoomID != "R0001" || details[0].Course != "高等数学" {
		t.Fatalf("导出的明细应为 R0001 的高等数学，实际 %+v", details[0])
	}
}

// TestExecuteFailsWhenBuildReadFails 钉住 publish.Build 的错误分流：仓储读取失败
// 是基础设施失败，收口 failed 且 error_code 为空；不能因为「错误来自 Build」就记成
// validation_blocked。
//
// 失败打在第 4 次 LoadRunRooms 上——正是发布组装那一次：
//  1. Run1 内部 seedAttribution 重建本轮归属；
//  2. 第二段规划前 runStages 读归属快照；
//  3. Run2 内部 seedAttribution 再重建一次；
//  4. 发布组装（publish.Build）读归属快照装配候选集。
//
// 让错误落在第 2 次上就只是 runStages 的读失败（已有用例覆盖），落在第 4 次才真的
// 踩到「Build 里的仓储错误」。旧实现把 Build 的所有错误一律记成 validation_blocked，
// 这条用例正好把那个缺陷钉死：读库失败被写成「数据不合格」，排障方向直接跑偏。
func TestExecuteFailsWhenBuildReadFails(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", nil)))
	f.loadRoomsErr = errors.New("读取每轮归属失败")
	f.loadRoomsErrAt = 4

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("发布组装读归属失败时应把原因返回给 run() 记 WARN")
	}
	if f.loadRoomsCalls != 4 {
		t.Fatalf("失败应发生在发布组装那次读取（第 4 次），实际调用 %d 次", f.loadRoomsCalls)
	}

	finish := singleFinish(t, f.execFake)
	// 空错误码就是本用例的核心断言：仓储读取失败不属于任何业务失败层。
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "读取每轮归属失败") {
		t.Fatalf("原因应取仓储文案供排障，实际 %q", finish.ErrorMessage)
	}
	if f.factsCalls != 1 {
		t.Fatalf("应已进入发布组装并读过轮次事实，实际 %d 次", f.factsCalls)
	}
	if f.commitCalls != 0 {
		t.Fatalf("读归属失败时不应提交发布事务，实际 %d 次", f.commitCalls)
	}
}

// publishRunFixture 造一份「一次就能通过 Build + Validate」的替身状态，供直接调用
// publishRun 的用例使用：学期与窗口已在库里、4 条必需周矩阵任务全部 done、
// R0001 归 Q10。不跑整条执行链，本组用例只钉 publishRun 这一段。
func publishRunFixture() (*stageFake, roundSetup, *bank, []store.RunRoom) {
	f := newStageFake()
	// LoadRunFacts 读的是「本轮已落库」的学期与窗口，直接喂替身的那两个字段。
	f.termSets = []string{"2026-2027-1"}
	f.windows = []windowWrite{{Start: 9, End: 12, WeeksTotal: 4}}
	for week := 9; week <= 12; week++ {
		f.tasks = append(f.tasks, store.SyncTask{
			TaskID:     int64(week - 8),
			Kind:       store.SyncTaskKindWeekMatrix,
			Required:   true,
			BuildingID: "Q10",
			Week:       week,
			Status:     store.SyncTaskDone,
		})
	}
	f.rooms["R0001"] = store.RunRoom{RoomID: "R0001", BuildingID: "Q10"}

	setup := roundSetup{
		window:   pipeline.Window{Start: 9, End: 12},
		settings: roundSettings{Buildings: []model.Building{{ID: "Q10", Name: "综合楼"}}},
		snapshot: matrixFixtureSnapshot(),
	}

	state := model.Observation{StateKey: "free", Available: true, RawText: "空闲"}
	b := newBank()
	b.pages[weekKey{BuildingID: "Q10", Week: 9}] = matrixPage{Cells: []cellRow{
		{RoomID: "R0001", Weekday: 1, Block: "0102", State: state},
	}}

	return f, setup, b, []store.RunRoom{{RoomID: "R0001", BuildingID: "Q10"}}
}

// TestPublishRunPublishesDroppedOptional 是 CP7「同一个 Dropped()」的回归用例：
// publishRun 收到的 DroppedOptional 必须原样进 RunInput，草稿因此是 partial，
// 原因写明丢弃条数——而不是被当成一轮干净的 success 发出去。
//
// 编排侧把 progress2.Dropped() 交给 EvaluateRun2 与 publishRun 是同一份值（execute.go
// 的 dropped 变量），这一层无法在整链用例里造出「明细任务被归属过滤丢弃」的场景
// （Plan2 按归属快照规划，正常轮次一条都不会被丢），所以直接喂 publishRun；对照组
// 用 Dropped 零值跑同一份状态，证明 partial 确实是那条丢弃记录带来的，而不是别的
// 原因。
func TestPublishRunPublishesDroppedOptional(t *testing.T) {
	tests := []struct {
		name        string
		dropped     pipeline.DroppedTasks
		wantStatus  string
		wantMessage string // 空串表示草稿不该带原因
	}{
		{
			name:       "没有丢弃时发布 success 且不带原因",
			dropped:    pipeline.DroppedTasks{},
			wantStatus: store.RunStatusSuccess,
		},
		{
			name:        "可选明细被归属过滤丢弃时发布 partial",
			dropped:     pipeline.DroppedTasks{Optional: 1},
			wantStatus:  store.RunStatusPartial,
			wantMessage: "归属校验丢弃 1 条明细任务",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, setup, b, rooms := publishRunFixture()
			c := newStageExecutor(t, f)

			if err := c.publishRun(context.Background(), executeRunID, setup, b, rooms, tc.dropped); err != nil {
				t.Fatalf("丢弃可选明细不阻断发布，publishRun 不应返回错误，实际 %v", err)
			}
			if f.commitCalls != 1 || len(f.commits) != 1 {
				t.Fatalf("应恰好提交一次发布事务，实际 %d 次：%+v", f.commitCalls, f.commits)
			}

			draft := f.commits[0]
			if draft.Status != tc.wantStatus {
				t.Fatalf("草稿状态期望 %q，实际 %q（原因 %q）",
					tc.wantStatus, draft.Status, draft.ErrorMessage)
			}
			if tc.wantMessage == "" {
				if draft.ErrorMessage != "" {
					t.Fatalf("success 不应带错误原因，实际 %q", draft.ErrorMessage)
				}
				return
			}
			if !strings.Contains(draft.ErrorMessage, tc.wantMessage) {
				t.Fatalf("原因应点明被丢弃的明细条数，实际 %q", draft.ErrorMessage)
			}
			if !strings.Contains(draft.ErrorMessage, "可选 1 条") {
				t.Fatalf("原因应区分必需与可选，实际 %q", draft.ErrorMessage)
			}
		})
	}
}
