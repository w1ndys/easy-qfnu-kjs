package collector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// doneWeekMatrixTasks 返回窗口 9..12 的 4 条周矩阵任务，全部已是 done：这是
// 「同一轮被再次执行」的库里现场——pipeline.Run 会跳过它们，观测只能由 hydrate 补回。
func doneWeekMatrixTasks() []store.SyncTask {
	tasks := make([]store.SyncTask, 0, 4)
	for week := 9; week <= 12; week++ {
		tasks = append(tasks, store.SyncTask{
			TaskID:     int64(week - 8),
			Kind:       store.SyncTaskKindWeekMatrix,
			Required:   true,
			BuildingID: "Q10",
			Week:       week,
			Status:     store.SyncTaskDone,
		})
	}
	return tasks
}

// TestExecuteHydratesDoneWeekMatrixPages 覆盖 tasklist 第 9 项的第一条单测：
// 预置 running 轮次 + done 的周矩阵任务 + 空内存 → 补抓数与缺失键数一致。
//
// 「预置 running 轮次」由 stageFake 的 LoadRunPhase 恒报 running 表达（pipeline.Run
// 对已收口轮次会直接拒绝，能跑完两段本身就证明轮次是 running）。
//
// 断言五件事：
//  1. 4 条 done 任务、空内存 → 恰好 4 次矩阵请求，且楼与周逐一对应缺失键；
//  2. 补抓全部发生在第一段（ensure_week）之前——测试能观测到的口径是「早于 Run1」。
//     Plan1 是纯函数，没有可供注入的副作用，所以「hydrate 在 Plan1 之前」只能由
//     runStages 的调用顺序保证，不能由本用例证明；
//  3. 补抓只进内存候选：没有 collect_run_week 写入（record/record_fail），
//     sync_task 的 done 状态一条都没被改写；
//  4. 补回来的页真的参与了第二段规划：R0001 的不可用格按上一轮 collect_run_room
//     的归属下钻一次明细（否则 hydrate 只是空转）；
//  5. success/partial 的发布与收口留给第 10 项，本项不写终态。
func TestExecuteHydratesDoneWeekMatrixPages(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.detailBody = stageDetailPage()
	f.tasks = doneWeekMatrixTasks()
	// collect_run_room 是上一轮落库的归属（D5）：hydrate 只重建观测，不重建归属。
	f.rooms["R0001"] = store.RunRoom{RoomID: "R0001", BuildingID: "Q10"}

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err != nil {
		t.Fatalf("续跑补抓成功时执行体不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("success/partial 的收口属于发布事务（第 10 项），本项不应提前收口，实际 %+v", finishes)
	}

	if f.loadWeekMatrixCalls != 1 {
		t.Fatalf("hydrate 应恰好读一次周矩阵任务清单，实际 %d 次", f.loadWeekMatrixCalls)
	}
	if len(f.matrixCalls) != 4 {
		t.Fatalf("4 个缺失键应各补抓一次，实际 %d 次请求：%+v", len(f.matrixCalls), f.matrixCalls)
	}
	for index, call := range f.matrixCalls {
		wantWeek := 9 + index
		if call.BuildingID != "Q10" || call.Week != wantWeek {
			t.Fatalf("第 %d 次补抓应为 Q10 第 %d 周，实际 %+v", index+1, wantWeek, call)
		}
	}

	// 顺序：读清单 → 逐页补抓 → 才进入 Run1（ensure_week）。
	//
	// 这里钉的口径只是「早于 Run1」：Plan1 不落任务、不请求上游，没有可观测的事件点；
	// 「hydrate 在 Plan1 之前」属于 runStages 的结构，不靠这条顺序断言证明。
	loadAt := nthEvent(t, f.events, "load_week_tasks", 0)
	run1At := nthEvent(t, f.events, "ensure_week", 0)
	if loadAt > nthEvent(t, f.events, "fetch_matrix", 0) {
		t.Fatalf("应先读任务清单再补抓，实际序列 %v", f.events)
	}
	for index := 0; index < len(f.matrixCalls); index++ {
		if at := nthEvent(t, f.events, "fetch_matrix", index); at > run1At {
			t.Fatalf("补抓必须早于 Run1（第一段），第 %d 次却在 ensure_week 之后：%v", index+1, f.events)
		}
	}

	// 只累计内存候选：不写 collect_run_week、不改 sync_task。
	for _, event := range f.events {
		if strings.HasPrefix(event, "record:") || strings.HasPrefix(event, "record_fail:") {
			t.Fatalf("hydrate 不应写 collect_run_week，实际出现事件 %q：%v", event, f.events)
		}
	}
	for _, task := range f.tasks {
		if task.Kind == store.SyncTaskKindWeekMatrix && task.Status != store.SyncTaskDone {
			t.Fatalf("hydrate 不应改 sync_task 的状态，实际 %+v", task)
		}
	}

	// 补回来的不可用格参与了 Plan2：按上一轮归属 Q10 下钻一次明细。
	if len(f.cellInputs) != 1 {
		t.Fatalf("hydrate 的候选应规划出一条明细任务，实际 %d 条：%+v", len(f.cellInputs), f.cellInputs)
	}
	if len(f.detailCalls) != 1 {
		t.Fatalf("hydrate 的候选应下钻一次明细，实际 %d 次：%+v", len(f.detailCalls), f.detailCalls)
	}
	if got := f.detailCalls[0]; got.RoomID != "R0001" || got.Weekday != 1 || got.BigSlot != "0102" {
		t.Fatalf("明细请求参数不符：%+v", got)
	}
}

// TestHydrateSkipsPagesAlreadyCached 覆盖 tasklist 第 9 项的第二条单测（CP10）：
// 内存里已经命中全部 (楼,周) 时，hydrate 的补抓数为 0。
//
// 先把 4 页按第一段的方式抓进内存（等价于「本轮刚抓过、页面缓存已命中」），再清掉
// 请求记录；hydrate 面对同一批 done 任务时应当一次上游都不打，候选仍是原来那 4 页。
func TestHydrateSkipsPagesAlreadyCached(t *testing.T) {
	f := newStageFake()
	f.matrixBody = []byte(matrixFixturePage(matrixDataRow("R0001", map[int]string{0: "◆"})))
	f.tasks = doneWeekMatrixTasks()

	c := newStageExecutor(t, f)
	candidates := newBank()
	fetch := c.taskFetcher("2026-2027-1", "01", f.collectSnapshot, candidates)
	for week := 9; week <= 12; week++ {
		if _, err := fetch(context.Background(), pipeline.Task{
			Kind:       pipeline.TaskWeekMatrix,
			BuildingID: "Q10",
			Week:       week,
		}); err != nil {
			t.Fatalf("预抓 Q10 第 %d 周不应报错，实际 %v", week, err)
		}
	}
	f.matrixCalls = nil
	f.events = nil

	if err := c.hydrate(context.Background(), executeRunID, fetch, candidates); err != nil {
		t.Fatalf("全部命中时 hydrate 不应报错，实际 %v", err)
	}
	if len(f.matrixCalls) != 0 {
		t.Fatalf("全部命中时补抓数应为 0，实际 %d 次：%+v", len(f.matrixCalls), f.matrixCalls)
	}
	for _, event := range f.events {
		if event == "fetch_matrix" {
			t.Fatalf("全部命中时不应再请求上游，实际序列 %v", f.events)
		}
	}
	if len(candidates.pages) != 4 {
		t.Fatalf("内存候选应保持 4 页，实际 %d 页", len(candidates.pages))
	}
	if f.loadWeekMatrixCalls != 1 {
		t.Fatalf("hydrate 应读一次任务清单，实际 %d 次", f.loadWeekMatrixCalls)
	}
}

// TestHydrateFailureFailsRunAndKeepsReleaseRows 覆盖 tasklist 第 9 项的第三条单测：
// 补抓失败 → 轮次 failed、原因点名楼与周、不进入发布（本项做到「发布事务一次都不调」）。
//
// 失败点定在补抓本身（上游错误），且库里只有一条 done 的周矩阵任务：
// 执行体应停在 hydrate，一条任务都不落、一条明细都不下钻、发布事务一次都不调。
//
// 「release 行数不变」这条不变量要等第 10 项把 Committer 接进 execute 之后才有区分力：
// 本项的 execute 根本不调用 Committer，因此下面的 commitCalls == 0 今天只能证明
// 「没有提前接线」。用例仍显式打开 allowCommit 并计数，是为了第 10 项接线后这段断言
// 自动升级成真正的回归——不打开的话，误接线只会炸在替身的 panic 上，看不出一共
// 调了几次、也就无从判断 release 行数有没有变。
func TestHydrateFailureFailsRunAndKeepsReleaseRows(t *testing.T) {
	f := newStageFake()
	f.matrixErr = errors.New("上游超时")
	// 打开替身的发布计数：默认是 panic（第 9 项不发布），这里要的是「0 次」这个
	// 可读的计数证据，而不是一个炸在调用点、说不出次数的 panic。
	f.allowCommit = true
	f.tasks = []store.SyncTask{{
		TaskID:     1,
		Kind:       store.SyncTaskKindWeekMatrix,
		Required:   true,
		BuildingID: "Q10",
		Week:       9,
		Status:     store.SyncTaskDone,
	}}

	c := newStageExecutor(t, f)
	if err := c.execute(context.Background(), executeRunID); err == nil {
		t.Fatal("补抓失败时执行体应返回错误供 run() 记 WARN")
	}

	finish := singleFinish(t, f.execFake)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "第 9 周") {
		t.Fatalf("原因应点名缺失的周，实际 %q", finish.ErrorMessage)
	}
	if !strings.Contains(finish.ErrorMessage, "Q10") {
		t.Fatalf("原因应点名缺失的楼，实际 %q", finish.ErrorMessage)
	}
	if !strings.Contains(finish.ErrorMessage, "上游超时") {
		t.Fatalf("原因应保留上游文案供排障，实际 %q", finish.ErrorMessage)
	}

	if len(f.matrixCalls) != 1 {
		t.Fatalf("第一条缺失键补抓失败就应收口，实际请求 %d 次：%+v", len(f.matrixCalls), f.matrixCalls)
	}
	// 第 10 项接线后这条断言才有区分力（今天只能证明「没有提前接线」）：打开
	// allowCommit 之后，误接线会留下 commitCalls == 1 而不是一个说不清次数的 panic。
	if f.commitCalls != 0 {
		t.Fatalf("hydrate 失败不应进入发布，release 行数应不变，实际提交 %d 次", f.commitCalls)
	}
	if len(f.detailCalls) != 0 || len(f.cellInputs) != 0 {
		t.Fatalf("hydrate 失败不应规划或下钻明细，实际规划 %d 条、下钻 %d 次",
			len(f.cellInputs), len(f.detailCalls))
	}
	for _, event := range f.events {
		if event == "ensure_week" || event == "ensure_cell" {
			t.Fatalf("hydrate 早于 Run1，失败时不应进入两段执行，实际序列 %v", f.events)
		}
		if strings.HasPrefix(event, "record:") || strings.HasPrefix(event, "record_fail:") {
			t.Fatalf("补抓失败不应写 collect_run_week，实际出现事件 %q：%v", event, f.events)
		}
	}
}
