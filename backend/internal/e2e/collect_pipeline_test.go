// Package e2e 是 collect-pipeline 的端到端检查点（tasklist 第 12 项）：
// 在真实 PostgreSQL 上跑通「采集 → 发布 → 查询」一条链，串起
// pipeline.StartRun/Plan/Run → publish.Build/Validate/Commit → query 的公开查询。
//
// 上游不真连：网络抓取与采集侧累计由夹具替代——周矩阵与占用说明都是手写脱敏的假页面，
// 由注入的 TaskFetcher 按任务返回，解析与清洗走真实的 parser / clean；pipeline 编排
// （StartRun/Plan/Run）与 publish 链路都是真实执行的。这条用例验证的是
// parser/clean → pipeline 组件 → publish → query 这条真实代码路径，
// 假的不止网络一跳：窗口写入与采集侧累计观测也由夹具/shim 代劳（见正文注释）。
//
// 只在设置 KJS_TEST_DSN 时运行，且会重建 public schema，只能指向测试库。
//
// 带 KJS_TEST_DSN 跑后端全量测试时必须固定 `-p 1` 串行：本包与 store 包的集成
// 用例都会先 DROP SCHEMA public 再迁移，`go test ./...` 默认按包并行（-p 取
// GOMAXPROCS），两个包同时重建同一个测试库会互踩——一边刚建好的 schema 被另一边
// DROP，表现为随机的「relation does not exist」或迁移冲突。正确跑法是
// `go test -p 1 -count=1 ./...`（或仓库根 `task test-integration`）。不带 DSN
// 的默认门禁不受影响：集成用例全部跳过，可以正常并行。
package e2e

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/parser"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/publish"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 检查点固定的业务身份。学期、白名单与房间都由本用例自己种进空库。
const (
	testTerm        = "2026-2027-1" // 学期；term 必须先落库，collect_run.term 有外键
	testTotalWeeks  = 20            // 学期总周数
	testCurrentWeek = 9             // 当前教学周 C；窗口 C..min(Y, C+3)

	testDictVersion = 1 // 迁移种子里的字典版本
	testAxisVersion = 1 // 迁移种子里的轴版本

	buildingQ10     = "Q10"   // 白名单第一栋：出问题的楼，也提供可选明细任务
	buildingQ10Name = "综合教学楼" // 展示名只来自白名单入参，不来自 room.building_name
	buildingQ11     = "Q11"   // 白名单第二栋
	buildingQ11Name = "实验楼"

	roomA = "A101" // Q10 的教室：周一第 1 大节占用
	roomB = "B201" // Q11 的教室：周一第 2 大节未知
)

// matrixWeekdays 是周矩阵覆盖的 7 天。格数由「7 天 × 轴里的大节数」决定，不写死。
const matrixWeekdays = 7

// 业务时区与三个固定时刻。时钟注入而不是读真实时间：检查点要能锁住
// collect_run_week.fetched_at、release.generated_at 与发布时刻。
var (
	shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

	// weekOneMonday 是第 1 周的周一；第 9 周周一 = 2026-11-02，即查询用的业务日期。
	weekOneMonday = time.Date(2026, 9, 7, 0, 0, 0, 0, shanghai)
	// 三个时刻按真实编排的先后摆：开始一轮(10:00) < 生成版本(10:03) < 发布(10:05)。
	// 版本生成时间不早于轮次开始、也不晚于发布，是为了让 release.generated_at、
	// release_id 与 collect_run.released_at 三者的先后关系符合生产语义；这是有意
	// 选定的相对次序，不是随手取的边界值。
	runStartedAt = time.Date(2026, 11, 4, 10, 0, 0, 0, shanghai) // 本轮开始时刻
	generatedAt  = time.Date(2026, 11, 4, 10, 3, 0, 0, shanghai) // 版本生成时间
	publishedAt  = time.Date(2026, 11, 4, 10, 5, 0, 0, shanghai) // 发布时刻

	// queryDate 是第 9 周周一，查询按它取学期周与星期。
	queryDate = weekOneMonday.AddDate(0, 0, 7*(testCurrentWeek-1))
)

// whitelistJSON 是 settings.building_whitelist 的取值形状，键名沿用上游 jxlbh 与 name。
const whitelistJSON = `[{"jxlbh":"Q10","name":"综合教学楼"},{"jxlbh":"Q11","name":"实验楼"}]`

// TestCollectPublishQueryAgainstRealDB 是本 spec 的端到端检查点：
// 空库 → 种学期日历与白名单 → 开一轮 → 规划窗口任务 → 用假页面跑完周矩阵与
// 占用说明 → 发布前查询确认不可见 → 组装候选集、校验、单事务发布 →
// 用公开查询（availability / day / meta）确认新版本可见。
//
// 断言分四组：轮次与任务落库、候选集组装结果、发布事务的版本与轮次终态、
// 公开查询的结果形状。任何一组失败都说明链条上有一段没接上，而不是单点实现问题。
//
// 运行时必须 `-p 1` 串行：本用例会 DROP SCHEMA public，store 包的集成用例也在同一
// 测试库上做同样的事，按包并行会互踩（原因与正确跑法见包注释）。
func TestCollectPublishQueryAgainstRealDB(t *testing.T) {
	ctx, pool := openTestDB(t)

	if !runStartedAt.Before(generatedAt) || !generatedAt.Before(publishedAt) {
		t.Fatalf("三个时刻必须满足 开始 < 生成 < 发布，实际 %s / %s / %s",
			runStartedAt, generatedAt, publishedAt)
	}

	if queryDate.Weekday() != time.Monday {
		t.Fatalf("第 %d 周周一应为星期一，实际 %s", testCurrentWeek, queryDate.Weekday())
	}

	seedCalendar(ctx, t, pool)
	dict := loadDict(ctx, t, pool)
	axis := loadAxis(ctx, t, pool)

	// 公开查询按 cmd/server/main.go 的方式装配，发布前先建好，用来验证「采集完
	// 但没发布」时查询读不到任何东西。
	queries, metaService := publicQueries(pool)

	// ---------- 1. 开一轮 ----------
	runStart, err := pipeline.StartRun(ctx, store.NewCollectRunRepo(pool), testTerm, func() time.Time {
		return runStartedAt
	})
	if err != nil {
		t.Fatalf("开始一轮失败: %v", err)
	}
	if runStart.Reused || runStart.Status != pipeline.StatusRunning {
		t.Fatalf("空库应新建 running 轮次，实际 reused=%v status=%q", runStart.Reused, runStart.Status)
	}
	runID := runStart.Run.RunID
	t.Logf("轮次已建立: run_id=%d term=%s", runID, testTerm)

	// ---------- 2. 规划采集窗口与任务 ----------
	window := pipeline.WindowFor(testCurrentWeek, testTotalWeeks)
	// Requirement 1.1：窗口必须是 C..min(Y, C+3)，即 9..12。
	if window != (pipeline.Window{Start: 9, End: 12}) {
		t.Fatalf("采集窗口期望 9..12，实际 %d..%d", window.Start, window.End)
	}

	// 已知断点：生产还没有 window 写入入口。pipeline 与 store 目前都只读
	// collect_run.window_start / window_end，没有任何写入函数；publish.Build 又要求
	// 窗口非空，所以这里直接用一句 UPDATE 写窗口——这是测试 shim，模拟尚未接线的
	// 编排层，不是生产写法。接生产装配时要补一个 store 写入函数，并把这句换成真实
	// 调用；补齐前「窗口由谁写」就是本轮检查点之外的缺口（已列入任务报告）。
	mustExec(ctx, t, pool,
		"UPDATE collect_run SET window_start = $2, window_end = $3 WHERE run_id = $1",
		runID, window.Start, window.End)

	// 可选明细任务只规划一个不可用格：A101 周一第 1 大节（◆ class → 不可用）。
	// 注意：两份 spec 都没有写明编排从哪里取这份「不可用格」列表（见报告），
	// 检查点直接把这一格写死，目的只是让明细段也被执行到。
	plan := pipeline.Plan(window, []string{buildingQ10, buildingQ11}, []pipeline.Cell{
		{BuildingID: buildingQ10, RoomID: roomA, Weekday: 1, Block: "0102"},
	})
	if plan.Status != pipeline.StatusRunning {
		t.Fatalf("规划状态期望 running，实际 %q", plan.Status)
	}
	if len(plan.Tasks) != 9 {
		t.Fatalf("期望 8 条周矩阵任务 + 1 条明细任务，实际 %d 条", len(plan.Tasks))
	}

	// ---------- 3. 用假上游页面跑完一轮 ----------
	fetcher := newFixtureFetcher(dict, axis, planBlocks(axis))
	progress, err := pipeline.Run(ctx, runID, []string{buildingQ10, buildingQ11}, plan.Tasks, pipeline.RunDeps{
		Tasks:  store.NewSyncTaskRepo(pool),
		Rooms:  store.NewRoomRepo(pool),
		Phases: store.NewRunPhaseRepo(pool),
		Fetch:  fetcher.Fetch,
		Clock:  func() time.Time { return runStartedAt },
	})
	if err != nil {
		t.Fatalf("执行一轮失败: %v", err)
	}
	if progress.Completed != len(plan.Tasks) || progress.Failed != 0 {
		t.Fatalf("任务执行计数不符: completed=%d failed=%d，期望 %d/0",
			progress.Completed, progress.Failed, len(plan.Tasks))
	}
	if dropped := progress.Dropped(); dropped.Total() != 0 {
		t.Fatalf("本轮不应有被归属过滤丢弃的明细，实际 %d 条", dropped.Total())
	}
	if fetcher.weekFetches != 8 || fetcher.detailFetches != 1 {
		t.Fatalf("假上游调用次数不符: week=%d detail=%d，期望 8/1",
			fetcher.weekFetches, fetcher.detailFetches)
	}
	t.Logf("一轮执行完成: completed=%d failed=%d", progress.Completed, progress.Failed)

	// 周矩阵与任务落库：2 栋楼 × 窗口 4 周 = 8 条周矩阵结果，逐条 ok、带 page_sha256、
	// fetched_at 等于注入时钟；1 条明细 done、9 条任务 done、归属快照 2 行。
	assertWeekFetches(ctx, t, pool, runID, []string{buildingQ10, buildingQ11}, window, runStartedAt)
	assertCount(ctx, t, pool, 0, "缺页面哈希的周结果",
		"SELECT count(*) FROM collect_run_week WHERE run_id = $1 AND page_sha256 IS NULL", runID)
	assertCount(ctx, t, pool, 9, "done 任务数",
		"SELECT count(*) FROM sync_task WHERE run_id = $1 AND status = 'done'", runID)
	assertCount(ctx, t, pool, 2, "本轮归属快照房号数",
		"SELECT count(*) FROM collect_run_room WHERE run_id = $1", runID)

	// ---------- 4. 发布前：公开查询读不到 ----------
	// 这一条证明可见性只由发布事务产生，而不是采集落库的副作用。
	beforeAvailability, err := queries.Availability(ctx, queryDate, query.AvailabilityParams{
		StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("发布前查询空教室失败: %v", err)
	}
	if beforeAvailability.Total != 0 || beforeAvailability.Hint == nil ||
		beforeAvailability.Hint.Code != query.HintNoData {
		t.Fatalf("发布前应无可见数据，实际 total=%d hint=%+v",
			beforeAvailability.Total, beforeAvailability.Hint)
	}
	beforeMeta, err := metaService.Meta(ctx, runStartedAt)
	if err != nil {
		t.Fatalf("发布前查询 meta 失败: %v", err)
	}
	if !beforeMeta.Empty || beforeMeta.Release != nil {
		t.Fatalf("发布前 meta 应为空态，实际 empty=%v release=%+v", beforeMeta.Empty, beforeMeta.Release)
	}
	t.Log("发布前公开查询为空态，符合预期")

	// ---------- 5. 组装候选集并校验 ----------
	dropped := progress.Dropped()
	candidate, err := publish.Build(ctx, store.NewPublishRepo(pool), runID, publish.RunInput{
		DictVersion: testDictVersion,
		AxisVersion: testAxisVersion,
		GeneratedAt: generatedAt,
		Buildings: []publish.Building{
			{ID: buildingQ10, Name: buildingQ10Name},
			{ID: buildingQ11, Name: buildingQ11Name},
		},
		AxisNodes:       axis,
		Cells:           fetcher.cells,
		Details:         fetcher.cellDetails,
		DroppedRequired: dropped.Required,
		DroppedOptional: dropped.Optional,
	})
	if err != nil {
		t.Fatalf("组装候选集失败: %v", err)
	}

	// 8 条必需任务全部完成、1 条可选任务完成 → 本轮应发布成 success。
	if candidate.RequiredTotal != 8 || candidate.RequiredDone != 8 || candidate.RequiredFailed != 0 {
		t.Fatalf("必需任务计数不符: total=%d done=%d failed=%d",
			candidate.RequiredTotal, candidate.RequiredDone, candidate.RequiredFailed)
	}
	if candidate.OptionalTotal != 1 || candidate.OptionalDone != 1 || candidate.OptionalFailed != 0 {
		t.Fatalf("可选任务计数不符: total=%d done=%d failed=%d",
			candidate.OptionalTotal, candidate.OptionalDone, candidate.OptionalFailed)
	}
	if candidate.RoomsTotal != 2 || candidate.WeeksTotal != 4 {
		t.Fatalf("汇总期望 2 间房 / 4 周，实际 %d/%d", candidate.RoomsTotal, candidate.WeeksTotal)
	}
	if len(candidate.Rooms) != 2 {
		t.Fatalf("归属快照期望 2 行，实际 %d 行", len(candidate.Rooms))
	}
	// 2 间房 × 4 周 × 7 天 × 12 小节（5 大节展开）= 672 行观测。
	if len(candidate.Observations) != 672 {
		t.Fatalf("观测行数期望 672，实际 %d", len(candidate.Observations))
	}
	// B201 周一第 2 大节的半角 J 不在字典里 → 每周 1 格未知，4 周共 4 格。
	if candidate.UnknownCells != 4 || candidate.CompositeCells != 0 {
		t.Fatalf("未知/复合期望 4/0，实际 %d/%d", candidate.UnknownCells, candidate.CompositeCells)
	}
	if candidate.HasAttrConflict {
		t.Fatal("本轮没有跨楼重复房号，不应带归属冲突标记")
	}
	if len(candidate.Details) != 1 || candidate.DroppedDetails != 0 {
		t.Fatalf("占用说明期望 1 条、丢弃 0 条，实际 %d/%d",
			len(candidate.Details), candidate.DroppedDetails)
	}

	if err := publish.Validate(candidate, publish.Thresholds{}); err != nil {
		t.Fatalf("候选集应通过发布前校验: %v", err)
	}

	// ---------- 6. 单事务发布 ----------
	commit, err := publish.Commit(ctx, store.NewReleaseRepo(pool), candidate, publishedAt)
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	wantReleaseID := generatedAt.UTC().Format("20060102T150405Z")
	if commit.ReleaseID != wantReleaseID {
		t.Fatalf("版本标识期望 %q，实际 %q", wantReleaseID, commit.ReleaseID)
	}
	t.Logf("已发布版本: %s", commit.ReleaseID)

	// 轮次在发布事务里写终态：success、phase=publish、released_at 等于发布时刻、
	// error_message 清成 NULL，汇总与版本号一起回写。
	var (
		status, phase, releaseID    string
		releasedAt                  *time.Time
		errorMessage                *string
		roomsTotal, weeksTotal      int
		unknownCells, composite     int
		attrConflicts               int
		requiredTotal, requiredDone int
		optionalTotal, optionalDone int
	)
	err = pool.QueryRow(ctx, `
SELECT status, phase, COALESCE(release_id, ''), released_at, error_message,
       rooms_total, weeks_total, unknown_cells, composite_cells, attr_conflict_count,
       required_total, required_done, optional_total, optional_done
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(
		&status, &phase, &releaseID, &releasedAt, &errorMessage,
		&roomsTotal, &weeksTotal, &unknownCells, &composite,
		&attrConflicts, &requiredTotal, &requiredDone, &optionalTotal, &optionalDone)
	if err != nil {
		t.Fatalf("读取轮次终态失败: %v", err)
	}
	if status != store.RunStatusSuccess || phase != store.RunPhasePublish {
		t.Fatalf("轮次终态期望 success/publish，实际 %s/%s", status, phase)
	}
	if releaseID != commit.ReleaseID || releasedAt == nil {
		t.Fatalf("轮次应关联版本 %s 并写 released_at，实际 release_id=%q released_at=%v",
			commit.ReleaseID, releaseID, releasedAt)
	}
	// released_at 是发布时刻，不是版本生成时间：本用例的固定时钟里两者差 2 分钟，
	// 写成 generatedAt 也能「非空」，这里锁住具体值。
	if !releasedAt.Equal(publishedAt) {
		t.Fatalf("released_at 期望发布时刻 %s，实际 %s",
			publishedAt.Format(time.RFC3339), releasedAt.Format(time.RFC3339))
	}
	// success 轮次不保留失败原因：error_message 必须是 NULL 而不是空串或旧文案。
	if errorMessage != nil {
		t.Fatalf("success 轮次的 error_message 应为 NULL，实际 %q", *errorMessage)
	}
	if roomsTotal != 2 || weeksTotal != 4 {
		t.Fatalf("轮次汇总期望 2 间房 / 4 周，实际 %d/%d", roomsTotal, weeksTotal)
	}
	if unknownCells != 4 || composite != 0 || attrConflicts != 0 {
		t.Fatalf("轮次计数不符: unknown=%d composite=%d attr_conflict=%d",
			unknownCells, composite, attrConflicts)
	}
	if requiredTotal != 8 || requiredDone != 8 || optionalTotal != 1 || optionalDone != 1 {
		t.Fatalf("轮次任务计数不符: required=%d/%d optional=%d/%d",
			requiredDone, requiredTotal, optionalDone, optionalTotal)
	}

	// 版本内容：只有这一次发布，它是唯一且是 current。
	assertCount(ctx, t, pool, 1, "版本总数", "SELECT count(*) FROM release")
	assertCount(ctx, t, pool, 1, "current 版本数",
		"SELECT count(*) FROM release WHERE is_current AND release_id = $1", commit.ReleaseID)
	// release.generated_at 写的是版本生成时间，不是发布时刻；版本标识也由它派生。
	// 这条与 released_at 的断言一起，把「生成」和「发布」两个时刻钉成不同值。
	var releaseGeneratedAt time.Time
	if err := pool.QueryRow(ctx,
		"SELECT generated_at FROM release WHERE release_id = $1",
		commit.ReleaseID).Scan(&releaseGeneratedAt); err != nil {
		t.Fatalf("读取版本生成时间失败: %v", err)
	}
	if !releaseGeneratedAt.Equal(generatedAt) {
		t.Fatalf("release.generated_at 期望 %s，实际 %s",
			generatedAt.Format(time.RFC3339), releaseGeneratedAt.Format(time.RFC3339))
	}
	assertCount(ctx, t, pool, 672, "版本观测行数",
		"SELECT count(*) FROM observation WHERE release_id = $1", commit.ReleaseID)
	assertCount(ctx, t, pool, 1, "版本占用说明行数",
		"SELECT count(*) FROM occupancy_detail WHERE release_id = $1", commit.ReleaseID)
	assertCount(ctx, t, pool, 2, "版本归属快照行数",
		"SELECT count(*) FROM release_room_building WHERE release_id = $1", commit.ReleaseID)
	// 精确到格：A101 周一第 1 小节的 ◆ 必须落成不可用。
	assertCount(ctx, t, pool, 1, "A101 第 9 周周一第 1 小节不可用行数",
		`SELECT count(*) FROM observation
		  WHERE release_id = $1 AND room_id = $2 AND week = 9
		    AND weekday = 1 AND node = '01' AND NOT available`, commit.ReleaseID, roomA)
	// 明细的状态键由 clean 投影：正常上课 不在字典符号里 → unknown，且不派生。
	var (
		detailStateKey string
		detailDerived  bool
	)
	err = pool.QueryRow(ctx, `
SELECT state_key, derived
  FROM occupancy_detail
 WHERE release_id = $1`, commit.ReleaseID).Scan(&detailStateKey, &detailDerived)
	if err != nil {
		t.Fatalf("读取占用说明失败: %v", err)
	}
	if detailStateKey != model.StateKeyUnknown || detailDerived {
		t.Fatalf("占用说明期望 unknown/非派生，实际 %q/derived=%v", detailStateKey, detailDerived)
	}

	// ---------- 7. 公开查询可见 ----------
	// 空教室：周一第 1-2 小节。A101 这段被占用、B201 空闲，只应查到 B201。
	availability, err := queries.Availability(ctx, queryDate, query.AvailabilityParams{
		StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("查询空教室失败: %v", err)
	}
	if availability.Term != testTerm || availability.Week != testCurrentWeek || availability.Weekday != 1 {
		t.Fatalf("查询上下文不符: term=%q week=%d weekday=%d",
			availability.Term, availability.Week, availability.Weekday)
	}
	if availability.Total != 1 || len(availability.Items) != 1 {
		t.Fatalf("空教室期望命中 1 间，实际 total=%d items=%d", availability.Total, len(availability.Items))
	}
	if item := availability.Items[0]; item.RoomID != roomB ||
		item.BuildingID != buildingQ11 || item.BuildingName != buildingQ11Name {
		t.Fatalf("空教室应只命中 %s（%s），实际 %+v", roomB, buildingQ11, item)
	}
	t.Logf("空教室查询命中 %s / %s", availability.Items[0].RoomID, availability.Items[0].BuildingName)

	// 按楼过滤：Q10 这段有课，命中 0 且提示「筛选无命中」而不是「无数据」。
	byBuilding, err := queries.Availability(ctx, queryDate, query.AvailabilityParams{
		Building: buildingQ10, StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("按楼查询空教室失败: %v", err)
	}
	if byBuilding.Total != 0 || byBuilding.Hint == nil || byBuilding.Hint.Code != query.HintNoMatch {
		t.Fatalf("Q10 该段应筛选无命中，实际 total=%d hint=%+v", byBuilding.Total, byBuilding.Hint)
	}

	// 全天状态：B201 周一第 1-2 小节空闲、第 3-5 小节未知（J 不在字典里）。
	dayB, err := queries.DayStatus(ctx, queryDate, query.DayParams{RoomID: roomB})
	if err != nil {
		t.Fatalf("查询 %s 全天状态失败: %v", roomB, err)
	}
	if dayB.RoomID != roomB || dayB.BuildingID != buildingQ11 || len(dayB.Nodes) != 12 {
		t.Fatalf("%s 全天状态不完整: room=%q building=%q nodes=%d",
			roomB, dayB.RoomID, dayB.BuildingID, len(dayB.Nodes))
	}
	assertDayNode(t, dayB.Nodes, "01", model.StateKeyFree, "空闲", true)
	assertDayNode(t, dayB.Nodes, "03", model.StateKeyUnknown, "未知", false)

	// 全天状态：A101 周一第 1 小节是 ◆（class），第 3 小节空闲。
	dayA, err := queries.DayStatus(ctx, queryDate, query.DayParams{RoomID: roomA})
	if err != nil {
		t.Fatalf("查询 %s 全天状态失败: %v", roomA, err)
	}
	if dayA.RoomID != roomA || dayA.BuildingID != buildingQ10 {
		t.Fatalf("%s 全天状态归属不符: room=%q building=%q", roomA, dayA.RoomID, dayA.BuildingID)
	}
	assertDayNode(t, dayA.Nodes, "01", "class", "正常上课", false)
	assertDayNode(t, dayA.Nodes, "03", model.StateKeyFree, "空闲", true)

	// meta：当前版本、字典/轴版本、白名单与逐周新鲜度都应认得这次发布。
	meta, err := metaService.Meta(ctx, publishedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("查询 meta 失败: %v", err)
	}
	if meta.Empty || meta.Release == nil || meta.Release.ReleaseID != commit.ReleaseID {
		t.Fatalf("meta 应指向新版本 %s，实际 empty=%v release=%+v",
			commit.ReleaseID, meta.Empty, meta.Release)
	}
	if meta.Dict == nil || meta.Dict.Version != testDictVersion {
		t.Fatalf("meta 字典版本期望 %d，实际 %+v", testDictVersion, meta.Dict)
	}
	if meta.Axis == nil || len(meta.Axis.Nodes) != 12 {
		t.Fatalf("meta 轴应有 12 个小节，实际 %+v", meta.Axis)
	}
	if len(meta.Buildings) != 2 {
		t.Fatalf("meta 白名单应有 2 栋楼，实际 %d 栋", len(meta.Buildings))
	}
	publishedWeeks := 0
	for _, week := range meta.Weeks {
		if week.Week < 9 || week.Week > 12 {
			continue
		}
		if !week.Published || week.LastSuccessAt == nil {
			t.Fatalf("第 %d 周应已发布且有成功抓取时间，实际 %+v", week.Week, week)
		}
		publishedWeeks++
	}
	if publishedWeeks != 4 {
		t.Fatalf("窗口内应有 4 周已发布，实际 %d 周", publishedWeeks)
	}
	t.Log("发布后公开查询可见：空教室 1 间、全天状态 12 小节、meta 版本与 4 周新鲜度均正确")
}

// openTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 与 store 包的集成测试同一口径：会清空 public schema，只能指向专用测试库。
func openTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	return openE2ETestDB(t, "采集 → 发布 → 查询端到端检查点")
}

// openE2ETestDB 是各条端到端检查点共用的连接与重建逻辑；checkpoint 只用在跳过提示里，
// 让「哪一条检查点没跑」一眼可辨，不影响行为。
func openE2ETestDB(t *testing.T, checkpoint string) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过" + checkpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	pool, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// assertWeekFetches 按 (building_id, week) 逐格断言本轮周矩阵结果：窗口内每个组合
// 恰一行、ok=true、fetched_at 非空且等于注入时钟（runStartedAt）。
//
// 总数与逐格都要看：只数总数会漏掉「某栋楼少一周、另一栋楼多一周」这种互相抵消的
// 缺失；按组合逐条查才能报出具体是哪栋楼的哪一周没写进去。
func assertWeekFetches(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	runID int64,
	buildings []string,
	window pipeline.Window,
	clock time.Time,
) {
	t.Helper()

	assertCount(ctx, t, pool, len(buildings)*(window.End-window.Start+1), "窗口内周矩阵结果行数", `
SELECT count(*)
  FROM collect_run_week
 WHERE run_id = $1
   AND week BETWEEN $2 AND $3`, runID, window.Start, window.End)

	for _, building := range buildings {
		for week := window.Start; week <= window.End; week++ {
			var (
				ok      bool
				fetched *time.Time
			)
			err := pool.QueryRow(ctx, `
SELECT ok, fetched_at
  FROM collect_run_week
 WHERE run_id = $1
   AND building_id = $2
   AND week = $3`, runID, building, week).Scan(&ok, &fetched)
			if err != nil {
				t.Fatalf("%s 第 %d 周的周矩阵结果缺失: %v", building, week, err)
			}
			if !ok || fetched == nil {
				t.Fatalf("%s 第 %d 周应成功抓取且有 fetched_at，实际 ok=%v fetched_at=%v",
					building, week, ok, fetched)
			}
			if !fetched.Equal(clock) {
				t.Fatalf("%s 第 %d 周 fetched_at 期望注入时钟 %s，实际 %s",
					building, week, clock.Format(time.RFC3339), fetched.Format(time.RFC3339))
			}
		}
	}
}

// seedCalendar 种学期、逐周日历与教学楼白名单。
// 日历要覆盖整个学期而不是只覆盖采集窗口：公开查询按业务日期反查学期周，
// 而 meta 的逐周新鲜度会覆盖 1..总周数。
func seedCalendar(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, $2)",
		testTerm, testTotalWeeks)
	for week := 1; week <= testTotalWeeks; week++ {
		monday := weekOneMonday.AddDate(0, 0, 7*(week-1))
		mustExec(ctx, t, pool,
			"INSERT INTO term_week (term, week, monday, in_calendar) VALUES ($1, $2, $3, true)",
			testTerm, week, monday)
	}
	mustExec(ctx, t, pool,
		"UPDATE settings SET value = $1 WHERE key = 'building_whitelist'",
		whitelistJSON)
}

// loadDict 从迁移种子里读回本轮清洗用的字典快照。
// 字典由数据库而不是测试常量给出：检查点要验证「种子字典 + clean 投影」这条真实链路。
func loadDict(ctx context.Context, t *testing.T, pool *pgxpool.Pool) model.Dict {
	t.Helper()

	dict := model.Dict{
		Symbols: make(map[string]string),
		States:  make(map[string]model.DictState),
	}

	stateRows, err := pool.Query(ctx,
		"SELECT state_key, label, available FROM dict_state WHERE dict_version = $1", testDictVersion)
	if err != nil {
		t.Fatalf("读取字典状态失败: %v", err)
	}
	defer stateRows.Close()
	for stateRows.Next() {
		var (
			key       string
			label     string
			available bool
		)
		if err := stateRows.Scan(&key, &label, &available); err != nil {
			t.Fatalf("扫描字典状态失败: %v", err)
		}
		dict.States[key] = model.DictState{Label: label, Available: available}
	}
	if err := stateRows.Err(); err != nil {
		t.Fatalf("遍历字典状态失败: %v", err)
	}

	symbolRows, err := pool.Query(ctx,
		"SELECT symbol, state_key FROM dict_symbol WHERE dict_version = $1", testDictVersion)
	if err != nil {
		t.Fatalf("读取字典符号失败: %v", err)
	}
	defer symbolRows.Close()
	for symbolRows.Next() {
		var symbol, stateKey string
		if err := symbolRows.Scan(&symbol, &stateKey); err != nil {
			t.Fatalf("扫描字典符号失败: %v", err)
		}
		dict.Symbols[symbol] = stateKey
	}
	if err := symbolRows.Err(); err != nil {
		t.Fatalf("遍历字典符号失败: %v", err)
	}

	// 夹具依赖这三个符号，种子被改动时要在最前面报出来，而不是等到断言阶段才奇怪地失败。
	for _, symbol := range []string{"◆", "Ｊ", "空闲"} {
		if _, ok := dict.Symbols[symbol]; !ok {
			t.Fatalf("字典种子缺少符号 %q", symbol)
		}
	}
	return dict
}

// loadAxis 从迁移种子里读回轴快照，按小节顺序排列。
func loadAxis(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []model.AxisNode {
	t.Helper()

	rows, err := pool.Query(ctx, `
SELECT node, ordinal, block, block_ordinal
  FROM axis_node
 WHERE axis_version = $1
 ORDER BY ordinal`, testAxisVersion)
	if err != nil {
		t.Fatalf("读取轴失败: %v", err)
	}
	defer rows.Close()

	nodes := make([]model.AxisNode, 0, 12)
	for rows.Next() {
		var node model.AxisNode
		if err := rows.Scan(&node.Node, &node.Ordinal, &node.Block, &node.BlockOrdinal); err != nil {
			t.Fatalf("扫描轴节点失败: %v", err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历轴节点失败: %v", err)
	}
	if len(nodes) != 12 {
		t.Fatalf("轴应有 12 个小节，实际 %d 个", len(nodes))
	}
	return nodes
}

// planBlocks 按轴里首次出现的顺序取大节编码，供夹具拼表头。
func planBlocks(axis []model.AxisNode) []string {
	blocks := make([]string, 0, len(axis))
	seen := make(map[string]struct{}, len(axis))
	for _, node := range axis {
		if _, ok := seen[node.Block]; ok {
			continue
		}
		seen[node.Block] = struct{}{}
		blocks = append(blocks, node.Block)
	}
	return blocks
}

// fixtureFetcher 是假上游：按任务返回手写的脱敏页面，并把解析、清洗后的候选
// 原料攒在内存里。
//
// 它同时承担真实编排里「采集侧累计观测与说明」的职责：pipeline 只返回运行计数，
// 观测与说明在发布前没有表可落（observation 以 release_id 为主键），所以必须由
// 编排侧（这里就是夹具）攒着，再交给 publish.Build。
type fixtureFetcher struct {
	dict    model.Dict
	axis    []model.AxisNode
	matrix  map[string][]byte // 楼编号 → 周矩阵页
	details map[string][]byte // 房号|星期|大节 → 占用说明页

	weekFetches   int
	detailFetches int
	cells         []publish.CellObservation // 矩阵观测（房 × 周 × 天 × 大节）
	cellDetails   []publish.CellDetail      // 占用说明
}

// newFixtureFetcher 按楼与检查点需要的格子造夹具页面。
func newFixtureFetcher(dict model.Dict, axis []model.AxisNode, blocks []string) *fixtureFetcher {
	// 状态格序号 = 星期下标 × 大节数 + 大节下标；只给需要的格子写原文，其余留空（空闲）。
	mondayBlockOne := 0
	mondayBlockTwo := 1

	return &fixtureFetcher{
		dict: dict,
		axis: axis,
		matrix: map[string][]byte{
			// Q10：A101 周一第 1 大节 ◆（正常上课），房号来自行属性 jsbh。
			buildingQ10: []byte(matrixFixture(blocks,
				matrixRow(blocks, ` jsbh="A101"`,
					`<input type="checkbox" value="A101">综合教学楼101(90/0)`,
					map[int]string{mondayBlockOne: "◆"}),
			)),
			// Q11：B201 周一第 2 大节半角 J（不在字典里 → 未知），房号来自首列输入框。
			buildingQ11: []byte(matrixFixture(blocks,
				matrixRow(blocks, ``,
					`<input type="text" value="B201">实验楼201（0/60）`,
					map[int]string{mondayBlockTwo: "J"}),
			)),
		},
		details: map[string][]byte{
			detailKey(roomA, 1, "0102"): []byte(occupancyFixture(
				occupancyFieldRow("教室状态：", "正常上课"),
				occupancyFieldRow("课程：", "高等数学"),
				occupancyFieldRow("周次：", "1-16"),
				occupancyFieldRow("时间标志：", "单双周"),
			)),
		},
	}
}

// Fetch 实现 pipeline.TaskFetcher：周矩阵返回页面与解析结果，明细只返回页面。
func (f *fixtureFetcher) Fetch(ctx context.Context, task pipeline.Task) (pipeline.TaskFetch, error) {
	// 执行循环每条任务前都会看取消信号；夹具也看一眼，保持与真实抓取同样的语义。
	if err := ctx.Err(); err != nil {
		return pipeline.TaskFetch{}, err
	}

	if task.Kind == pipeline.TaskWeekMatrix {
		page, ok := f.matrix[task.BuildingID]
		if !ok {
			return pipeline.TaskFetch{}, fmt.Errorf("假上游没有楼 %s 的周矩阵页", task.BuildingID)
		}
		rooms, err := parser.Rows(page, f.axis)
		if err != nil {
			return pipeline.TaskFetch{}, fmt.Errorf("解析楼 %s 第 %d 周矩阵失败: %w",
				task.BuildingID, task.Week, err)
		}
		f.weekFetches++

		// 真实编排在这里把矩阵原文清洗成候选观测；跨楼重复房号由 pipeline 裁决，
		// 编排拿不到 Accepted 集合，所以夹具的页面要保证同一房号只出现在一栋楼里
		// （本检查点没有跨楼重复，因此不会把被丢弃的行混进候选集）。
		for _, room := range rooms {
			for _, cell := range room.Cells {
				f.cells = append(f.cells, publish.CellObservation{
					RoomID:  room.RoomID,
					Week:    task.Week,
					Weekday: cell.Weekday,
					Block:   cell.Block,
					State:   clean.State(cell.StateRaw, f.dict),
				})
			}
		}
		return pipeline.TaskFetch{Page: page, Rooms: rooms}, nil
	}

	page, ok := f.details[detailKey(task.RoomID, task.Weekday, task.Block)]
	if !ok {
		return pipeline.TaskFetch{}, fmt.Errorf("假上游没有 %s 星期 %d 大节 %s 的明细页",
			task.RoomID, task.Weekday, task.Block)
	}
	entries, err := parser.Occupancy(page)
	if err != nil {
		return pipeline.TaskFetch{}, fmt.Errorf("解析 %s 的占用说明失败: %w", task.RoomID, err)
	}
	f.detailFetches++
	for _, entry := range entries {
		f.cellDetails = append(f.cellDetails, publish.CellDetail{
			RoomID:    task.RoomID,
			Weekday:   task.Weekday,
			Block:     task.Block,
			State:     clean.State(entry.StateRaw, f.dict),
			Course:    entry.Course,
			WeekRange: entry.WeekRange,
			TimeFlag:  entry.TimeFlag,
		})
	}
	return pipeline.TaskFetch{Page: page}, nil
}

// detailKey 是明细夹具的索引：房号 + 星期 + 大节。
func detailKey(roomID string, weekday int, block string) string {
	return fmt.Sprintf("%s|%d|%s", roomID, weekday, block)
}

// matrixHeaderRow 拼表头行：首列房名列 + 每天按轴顺序重复一遍大节。
func matrixHeaderRow(blocks []string) string {
	var b strings.Builder
	b.WriteString("<tr><td></td>")
	for day := 0; day < matrixWeekdays; day++ {
		for _, block := range blocks {
			b.WriteString(`<td tdvalue="` + block + `">` + block + `</td>`)
		}
	}
	b.WriteString("</tr>")
	return b.String()
}

// matrixRow 拼一个数据行：首列房名列 + 「7 天 × 大节数」个状态格。
// states 是「状态格序号 → 原文」，没列出的格子写空文本（空白格即空闲）。
func matrixRow(blocks []string, trAttrs, roomCell string, states map[int]string) string {
	var b strings.Builder
	b.WriteString("<tr" + trAttrs + ">")
	b.WriteString("<td>" + roomCell + "</td>")
	for index := 0; index < matrixWeekdays*len(blocks); index++ {
		b.WriteString("<td>" + states[index] + "</td>")
	}
	b.WriteString("</tr>")
	return b.String()
}

// matrixFixture 拼一张完整矩阵页。同名前缀的 dataListHeader 表有意排在目标表之前：
// 解析必须按 dataList 精确定位，不能按前缀匹配（docs/pitfalls.md 第 3 节）。
func matrixFixture(blocks []string, rows ...string) string {
	return `<html><body><center id="alldiv">` +
		`<table id="dataListHeader" name="kbDataTable" width="100%">` + matrixHeaderRow(blocks) + `</table>` +
		`<table id="dataList" name="kbDataTable" border="1">` +
		matrixHeaderRow(blocks) + strings.Join(rows, "") +
		`</table></center></body></html>`
}

// occupancyFieldRow 拼一行合法字段行：标签 / 空列 / 值 三列。
func occupancyFieldRow(label, value string) string {
	return `<tr><td width="173" align="right" bgcolor="#EFEFEF">` + label + `</td>` +
		`<td width="9"></td>` +
		`<td width="560">` + value + `</td></tr>`
}

// occupancyFixture 拼一张明细页：table#tab1 只作标题表，字段行在下方嵌套的三列表里。
func occupancyFixture(fieldRows ...string) string {
	return `<html><body>` +
		`<center id="alldiv"><table id="tab1"><tr><td class="toolstitle">教室占用情况</td></tr></table></center>` +
		`<form action="/jsxsd/jspyfa/selectJx02" name="Form1" id="Form1" method="post">` +
		`<table border="1"><tr><td><table border="0">` +
		strings.Join(fieldRows, "") +
		`</table></td></tr></table></form></body></html>`
}

// publicQueries 按 cmd/server/main.go 的装配方式把存储层函数接成公开查询服务。
//
// main.go 里的这些闭包都是函数内的局部变量，没有可复用的装配函数，检查点只能
// 原样复制一份映射（报告里作为「装配层不可复用」列出）。缓存不开启：
// 检查点要读实时结果，避免用 TTL 掩盖可见性问题。
func publicQueries(pool *pgxpool.Pool) (*query.Service, *query.MetaService) {
	weekLookup := func(ctx context.Context, date time.Time) (model.TermWeek, bool, error) {
		return store.TermWeekAt(ctx, pool, date)
	}
	availabilityLookup := func(ctx context.Context, filter query.AvailabilityFilter) (query.AvailabilityPage, error) {
		page, err := store.Availability(ctx, pool, store.AvailabilityFilter{
			Term:      filter.Term,
			Week:      filter.Week,
			Weekday:   filter.Weekday,
			StartNode: filter.StartNode,
			EndNode:   filter.EndNode,
			NodeCount: filter.NodeCount,
			Building:  filter.Building,
			Keyword:   filter.Keyword,
			Limit:     filter.Limit,
			Offset:    filter.Offset,
		})
		if err != nil {
			return query.AvailabilityPage{}, err
		}

		rooms := make([]query.AvailabilityRoom, 0, len(page.Rooms))
		for _, room := range page.Rooms {
			rooms = append(rooms, query.AvailabilityRoom{
				RoomID:       room.ID,
				Name:         room.Name,
				BuildingID:   room.BuildingID,
				BuildingName: room.BuildingName,
			})
		}
		return query.AvailabilityPage{
			Total:              page.Total,
			Rooms:              rooms,
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
		}, nil
	}
	dayLookup := func(ctx context.Context, filter query.DayFilter) (query.DayPage, error) {
		page, err := store.DayStatus(ctx, pool, store.DayFilter{
			Term:     filter.Term,
			Week:     filter.Week,
			Weekday:  filter.Weekday,
			RoomID:   filter.RoomID,
			Building: filter.Building,
		})
		if err != nil {
			return query.DayPage{}, err
		}

		nodes := make([]query.DayNode, 0, len(page.Nodes))
		for _, node := range page.Nodes {
			nodes = append(nodes, query.DayNode{
				Node:      node.Node,
				Block:     node.Block,
				StateKey:  node.StateKey,
				Label:     node.Label,
				Available: node.Available,
				HasData:   node.HasData,
			})
		}

		result := query.DayPage{
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
			Nodes:              nodes,
		}
		if page.Room != nil {
			result.Room = &query.DayRoom{
				RoomID:       page.Room.ID,
				Name:         page.Room.Name,
				BuildingID:   page.Room.BuildingID,
				BuildingName: page.Room.BuildingName,
			}
		}
		return result, nil
	}
	queries := query.New(weekLookup, availabilityLookup, dayLookup)

	metaLookup := func(ctx context.Context) (query.MetaFacts, error) {
		facts, err := store.Meta(ctx, pool)
		if err != nil {
			return query.MetaFacts{}, err
		}

		mapped := query.MetaFacts{
			DictStates: make([]query.MetaDictStateFacts, 0, len(facts.DictStates)),
			AxisNodes:  make([]query.MetaAxisNode, 0, len(facts.AxisNodes)),
			Buildings:  make([]query.MetaBuilding, 0, len(facts.Buildings)),
		}
		if facts.Release != nil {
			mapped.Release = &query.MetaReleaseFacts{
				ReleaseID:   facts.Release.ID,
				GeneratedAt: facts.Release.GeneratedAt,
				Term:        facts.Release.Term,
				TotalWeeks:  facts.Release.TotalWeeks,
				DictVersion: facts.Release.DictVersion,
				AxisVersion: facts.Release.AxisVersion,
			}
		}
		for _, state := range facts.DictStates {
			mapped.DictStates = append(mapped.DictStates, query.MetaDictStateFacts{
				StateKey:  state.StateKey,
				Label:     state.Label,
				Available: state.Available,
			})
		}
		for _, node := range facts.AxisNodes {
			mapped.AxisNodes = append(mapped.AxisNodes, query.MetaAxisNode{
				Node:         node.Node,
				Ordinal:      node.Ordinal,
				Block:        node.Block,
				BlockOrdinal: node.BlockOrdinal,
			})
		}
		for _, building := range facts.Buildings {
			mapped.Buildings = append(mapped.Buildings, query.MetaBuilding{
				BuildingID: building.ID,
				Name:       building.Name,
			})
		}
		return mapped, nil
	}
	freshnessLookup := func(ctx context.Context, params query.FreshnessParams) (query.FreshnessFacts, error) {
		facts, err := store.Freshness(ctx, pool, store.FreshnessRange{
			Term:     params.Term,
			FromWeek: params.FromWeek,
			ToWeek:   params.ToWeek,
		})
		if err != nil {
			return query.FreshnessFacts{}, err
		}

		mapped := query.FreshnessFacts{
			Weeks:         make([]query.WeekFacts, 0, len(facts.Weeks)),
			LastRunStatus: facts.LastRunStatus,
		}
		for _, week := range facts.Weeks {
			mapped.Weeks = append(mapped.Weeks, query.WeekFacts{
				Week:          week.Week,
				Published:     week.Published,
				LastSuccessAt: week.LastSuccessAt,
			})
		}
		return mapped, nil
	}
	freshness := query.NewFreshness(freshnessLookup)
	return queries, query.NewMeta(metaLookup, freshness)
}

// assertDayNode 在小节列表里按编号找一个节点并断言它的四个字段。
func assertDayNode(t *testing.T, nodes []query.DayNode, node, stateKey, label string, available bool) {
	t.Helper()

	for _, item := range nodes {
		if item.Node != node {
			continue
		}
		if !item.HasData {
			t.Fatalf("小节 %s 应有观测", node)
		}
		if item.StateKey != stateKey || item.Label != label || item.Available != available {
			t.Fatalf("小节 %s 期望 %s/%s/available=%v，实际 %s/%s/available=%v",
				node, stateKey, label, available, item.StateKey, item.Label, item.Available)
		}
		return
	}
	t.Fatalf("小节列表里找不到 %s", node)
}

// mustExec 执行一条必须成功的写语句。
func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("执行写语句失败（%s）: %v", sql, err)
	}
}

// assertCount 断言一条 count 查询的结果。
func assertCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, want int, what, sql string, args ...any) {
	t.Helper()

	var got int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&got); err != nil {
		t.Fatalf("统计%s失败: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s期望 %d，实际 %d", what, want, got)
	}
}
