package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestLoadRunFactsAgainstRealDB 覆盖发布组装读轮次事实的真实库口径：
// 学期、窗口与归属冲突计数如实读回；NULL 的学期与窗口折成空串与 0；
// 轮次不存在时返回 ErrRunStoreFailed，不把零值当成合法轮次。
func TestLoadRunFactsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedSyncTaskRun(ctx, t, pool)

	mustExec(ctx, t, pool, `
UPDATE collect_run
   SET window_start = 5, window_end = 8, attr_conflict_count = 3
 WHERE run_id = $1`, runID)

	facts, err := LoadRunFacts(ctx, pool, runID)
	if err != nil {
		t.Fatalf("读轮次发布事实失败: %v", err)
	}
	want := RunFacts{
		RunID:             runID,
		Term:              syncTaskFixtureTerm,
		WindowStart:       5,
		WindowEnd:         8,
		AttrConflictCount: 3,
	}
	if facts != want {
		t.Fatalf("轮次发布事实不符: got %+v want %+v", facts, want)
	}

	// 没写学期、没定窗口的轮次照样能读：NULL 折成空串与 0，由调用方判非法。
	var legacyRunID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status)
VALUES (now(), 'failed')
RETURNING run_id`).Scan(&legacyRunID); err != nil {
		t.Fatalf("创建无学期轮次失败: %v", err)
	}
	legacy, err := LoadRunFacts(ctx, pool, legacyRunID)
	if err != nil {
		t.Fatalf("读无学期轮次失败: %v", err)
	}
	if legacy.Term != "" || legacy.WindowStart != 0 || legacy.WindowEnd != 0 {
		t.Fatalf("NULL 学期/窗口应折成零值: %+v", legacy)
	}

	if _, err := LoadRunFacts(ctx, pool, runID+100000); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在应返回 ErrRunStoreFailed，实际 err=%v", err)
	}
}

// TestPublishRepoAgainstRealDB 覆盖发布仓储的三个读方法委托：
// 归属快照读 collect_run_room、任务汇总读 sync_task 现状、轮次事实读 collect_run。
func TestPublishRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedSyncTaskRun(ctx, t, pool)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	mustExec(ctx, t, pool, `
UPDATE collect_run
   SET window_start = 5, window_end = 8, attr_conflict_count = 1
 WHERE run_id = $1`, runID)

	// 归属快照：一条有效归属，一条带冲突标记。
	if err := UpsertRunRooms(ctx, pool, runID, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
	}, now); err != nil {
		t.Fatalf("写归属快照失败: %v", err)
	}
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id)
VALUES ('r102', 'F102', 'F102', 'Q11')
ON CONFLICT (room_id) DO NOTHING`)
	if err := UpsertRunRooms(ctx, pool, runID, []RunRoom{
		{RoomID: "r102", BuildingID: "Q11", Conflict: true, ConflictBuildingID: "Q12"},
	}, now); err != nil {
		t.Fatalf("写冲突归属失败: %v", err)
	}

	// 两条必需任务：一条完成、一条失败。
	tasks, err := EnsureWeekMatrixTasks(ctx, pool, runID, []SyncTaskInput{
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 5},
		{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 6},
	}, now)
	if err != nil {
		t.Fatalf("写周矩阵任务失败: %v", err)
	}
	if err := CompleteSyncTask(ctx, pool, runID, tasks[0].TaskID, now); err != nil {
		t.Fatalf("置任务完成失败: %v", err)
	}
	if err := FailSyncTask(ctx, pool, runID, tasks[1].TaskID, "矩阵结构不符合预期", now); err != nil {
		t.Fatalf("置任务失败失败: %v", err)
	}

	repo := NewPublishRepo(pool)

	facts, err := repo.LoadRunFacts(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读发布事实失败: %v", err)
	}
	if facts.RunID != runID || facts.WindowStart != 5 || facts.WindowEnd != 8 || facts.AttrConflictCount != 1 {
		t.Fatalf("仓储读发布事实不符: %+v", facts)
	}
	if facts.Term != syncTaskFixtureTerm {
		t.Fatalf("仓储读学期不符: %q", facts.Term)
	}

	rooms, err := repo.LoadRunRooms(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读归属快照失败: %v", err)
	}
	wantRooms := []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q11", Conflict: true, ConflictBuildingID: "Q12"},
	}
	if len(rooms) != len(wantRooms) {
		t.Fatalf("归属快照行数不符: got %+v want %+v", rooms, wantRooms)
	}
	for index := range wantRooms {
		if rooms[index] != wantRooms[index] {
			t.Fatalf("第 %d 行归属不符: got %+v want %+v", index+1, rooms[index], wantRooms[index])
		}
	}

	summary, err := repo.LoadRunTaskSummary(ctx, runID)
	if err != nil {
		t.Fatalf("仓储读任务汇总失败: %v", err)
	}
	wantSummary := RunTaskSummary{
		RequiredTotal:       2,
		RequiredDone:        1,
		RequiredFailed:      1,
		FirstFailureMessage: "矩阵结构不符合预期",
	}
	if summary != wantSummary {
		t.Fatalf("任务汇总不符: got %+v want %+v", summary, wantSummary)
	}
}

// seedPublishRun 建一条带学期的轮次，并保证学期与测试房号已落库。
// 发布要求轮次仍在 running：发布事务会写终态（success/partial），
// 已收口的轮次由前置检查拒绝。
func seedPublishRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string) int64 {
	t.Helper()

	mustExec(ctx, t, pool,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		syncTaskFixtureTerm)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('r101', 'F101', 'F101', 'Q10', '综合教学楼'),
       ('r102', 'F102', 'F102', 'Q11', '理科楼')
ON CONFLICT (room_id) DO NOTHING`)

	var runID int64
	err := pool.QueryRow(ctx, `
INSERT INTO collect_run (started_at, status, term)
VALUES (now(), $1, $2)
RETURNING run_id`, status, syncTaskFixtureTerm).Scan(&runID)
	if err != nil {
		t.Fatalf("创建发布轮次失败: %v", err)
	}
	return runID
}

// seedCurrentRelease 造一个已是 current 的旧版本，并带一行观测，
// 用来验证发布失败后旧版本仍然可用（CP7）。
func seedCurrentRelease(ctx context.Context, t *testing.T, pool *pgxpool.Pool, releaseID string, generatedAt time.Time) {
	t.Helper()

	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, $3, 1, 1, true)`, releaseID, syncTaskFixtureTerm, generatedAt)
	mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, 'r101', $2, 5, 1, '01', 'free', true)`, releaseID, syncTaskFixtureTerm)
}

// publishDraftFixture 是一份可直接提交的发布草稿：一行归属、两行观测、一行说明。
func publishDraftFixture(runID int64, releaseID string, generatedAt time.Time) ReleaseDraft {
	return ReleaseDraft{
		ReleaseID:       releaseID,
		RunID:           runID,
		Term:            syncTaskFixtureTerm,
		GeneratedAt:     generatedAt,
		UpdatedAt:       generatedAt.Add(time.Minute),
		DictVersion:     1,
		AxisVersion:     1,
		WindowStart:     5,
		WindowEnd:       8,
		HasAttrConflict: true,
		UnknownCells:    2,
		CompositeCells:  1,
		RoomsTotal:      1,
		WeeksTotal:      4,
		Status:          RunStatusSuccess,
		Rooms: []ReleaseRoom{
			{RoomID: "r101", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Observations: []ReleaseObservation{
			{RoomID: "r101", Week: 5, Weekday: 1, Node: "01", StateKey: "free", Available: true},
			{RoomID: "r101", Week: 5, Weekday: 1, Node: "02", StateKey: "class", Available: false, RawText: "有课"},
		},
		Details: []ReleaseDetail{
			{
				RoomID: "r101", Weekday: 1, Block: "0102", StateKey: "class",
				Course: "高等数学", WeekRange: "1-16", TimeFlag: "单双周", Derived: true,
			},
		},
	}
}

// releaseCurrentState 读一个版本是否存在以及是否为 current。
func releaseCurrentState(ctx context.Context, t *testing.T, pool *pgxpool.Pool, releaseID string) (isCurrent, found bool) {
	t.Helper()

	err := pool.QueryRow(ctx, "SELECT is_current FROM release WHERE release_id = $1", releaseID).Scan(&isCurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatalf("读取版本 %s 失败: %v", releaseID, err)
	}
	return isCurrent, true
}

// countReleases 数版本总行数与 current 行数（CP8 与「current 恒为一个」）。
func countReleases(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (total, current int) {
	t.Helper()

	err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE is_current)
  FROM release`).Scan(&total, &current)
	if err != nil {
		t.Fatalf("统计版本失败: %v", err)
	}
	return total, current
}

// countReleaseRows 数某个版本在某张表里的行数；table 只由本文件的用例传入固定值。
func countReleaseRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table, releaseID string) int {
	t.Helper()

	var count int
	err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE release_id = $1", releaseID).Scan(&count)
	if err != nil {
		t.Fatalf("统计 %s 行数失败: %v", table, err)
	}
	return count
}

// runPublishRow 是发布后 collect_run 的发布相关列。
type runPublishRow struct {
	ReleaseID  string
	Unknown    int
	Composite  int
	RoomsTotal int
	WeeksTotal int
	Status     string
	Phase      string
	ReleasedAt *time.Time
	FinishedAt *time.Time
	UpdatedAt  time.Time
}

// loadRunPublishRow 读轮次的发布汇总、终态与两个时刻。
func loadRunPublishRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) runPublishRow {
	t.Helper()

	var row runPublishRow
	err := pool.QueryRow(ctx, `
SELECT COALESCE(release_id, ''), unknown_cells, composite_cells, rooms_total, weeks_total,
       status, phase, released_at, finished_at, updated_at
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(
		&row.ReleaseID, &row.Unknown, &row.Composite, &row.RoomsTotal, &row.WeeksTotal,
		&row.Status, &row.Phase, &row.ReleasedAt, &row.FinishedAt, &row.UpdatedAt)
	if err != nil {
		t.Fatalf("读取轮次 %d 发布汇总失败: %v", runID, err)
	}
	return row
}

// TestCommitReleaseAgainstRealDB 覆盖真实库的成功发布（Requirement 7.1–7.4）：
// 新版本以非 current 写入后抬为 current、原 current 降级、观测/说明/归属快照
// 行数与列值正确、轮次汇总与 release_id 在同一事务里回写。
func TestCommitReleaseAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedPublishRun(ctx, t, pool, "running")

	oldGenerated := time.Date(2026, 11, 4, 21, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	seedCurrentRelease(ctx, t, pool, "rel-old", oldGenerated)

	draft := publishDraftFixture(runID, "rel-new", oldGenerated.Add(time.Hour))
	if err := NewReleaseRepo(pool).CommitRelease(ctx, draft); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-new"); !found || !isCurrent {
		t.Fatalf("新版本应是 current: found=%v current=%v", found, isCurrent)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-old"); !found || isCurrent {
		t.Fatalf("原版本应保留但不再是 current: found=%v current=%v", found, isCurrent)
	}
	if total, current := countReleases(ctx, t, pool); total != 2 || current != 1 {
		t.Fatalf("版本行数应为 2、current 应为 1: total=%d current=%d", total, current)
	}

	if got := countReleaseRows(ctx, t, pool, "observation", "rel-new"); got != len(draft.Observations) {
		t.Fatalf("观测行数不符: got %d want %d", got, len(draft.Observations))
	}
	if got := countReleaseRows(ctx, t, pool, "occupancy_detail", "rel-new"); got != len(draft.Details) {
		t.Fatalf("说明行数不符: got %d want %d", got, len(draft.Details))
	}
	if got := countReleaseRows(ctx, t, pool, "release_room_building", "rel-new"); got != len(draft.Rooms) {
		t.Fatalf("归属快照行数不符: got %d want %d", got, len(draft.Rooms))
	}
	// 旧版本的数据还在：发布只切换 current，不重建旧版本。
	if got := countReleaseRows(ctx, t, pool, "observation", "rel-old"); got != 1 {
		t.Fatalf("旧版本观测应保留 1 行，实际 %d 行", got)
	}

	// 可空列按 NULL 落库：空原文不应写成空串占位。
	var rawText string
	if err := pool.QueryRow(ctx, `
SELECT COALESCE(raw_text, '<null>')
  FROM observation
 WHERE release_id = 'rel-new' AND node = '01'`).Scan(&rawText); err != nil {
		t.Fatalf("读取观测原文失败: %v", err)
	}
	if rawText != "<null>" {
		t.Fatalf("空原文应写 NULL，实际 %q", rawText)
	}
	var derived bool
	if err := pool.QueryRow(ctx, `
SELECT derived FROM occupancy_detail WHERE release_id = 'rel-new'`).Scan(&derived); err != nil {
		t.Fatalf("读取说明派生标记失败: %v", err)
	}
	if !derived {
		t.Fatal("说明的派生标记应原样写入")
	}
	var hasAttrConflict bool
	if err := pool.QueryRow(ctx, `
SELECT has_attr_conflict FROM release WHERE release_id = 'rel-new'`).Scan(&hasAttrConflict); err != nil {
		t.Fatalf("读取版本归属冲突标记失败: %v", err)
	}
	if !hasAttrConflict {
		t.Fatal("版本的归属冲突标记应原样写入")
	}

	state := loadRunPublishRow(ctx, t, pool, runID)
	if state.ReleaseID != "rel-new" || state.Unknown != draft.UnknownCells || state.Composite != draft.CompositeCells {
		t.Fatalf("轮次汇总回写不符: release=%q unknown=%d composite=%d", state.ReleaseID, state.Unknown, state.Composite)
	}
	// 房号数与窗口周数按候选集回写（用户拍板）。
	if state.RoomsTotal != draft.RoomsTotal || state.WeeksTotal != draft.WeeksTotal {
		t.Fatalf("房号数/窗口周数回写不符: got %d/%d want %d/%d",
			state.RoomsTotal, state.WeeksTotal, draft.RoomsTotal, draft.WeeksTotal)
	}
	// 发布在同一事务里写终态与阶段：轮次不再 running，phase 停在 publish。
	if state.Status != RunStatusSuccess || state.Phase != RunPhasePublish {
		t.Fatalf("发布终态/阶段不符: status=%q phase=%q", state.Status, state.Phase)
	}
	if state.ReleasedAt == nil || !state.ReleasedAt.Equal(draft.UpdatedAt) {
		t.Fatalf("独立发布标记不符: got %v want %s", state.ReleasedAt, draft.UpdatedAt)
	}
	if state.FinishedAt == nil || !state.FinishedAt.Equal(draft.UpdatedAt) {
		t.Fatalf("finished_at 不符: got %v want %s", state.FinishedAt, draft.UpdatedAt)
	}
	if !state.UpdatedAt.Equal(draft.UpdatedAt) {
		t.Fatalf("轮次 updated_at 不符: got %s want %s", state.UpdatedAt, draft.UpdatedAt)
	}
	// 终态写入后 running 槽立即释放，下一轮可以开始。
	if running := countRunningCollectRuns(ctx, t, pool); running != 0 {
		t.Fatalf("发布成功后不应还有 running 轮次，实际 %d", running)
	}
}

// TestCommitReleaseRollbackKeepsCurrentAgainstRealDB 覆盖 CP7：发布事务在写完
// 版本行之后中途失败（观测引用不存在的房号），整个事务回滚——新版本不存在、
// 原 current 仍可用、原数据未被改动、轮次也没有被回写。
func TestCommitReleaseRollbackKeepsCurrentAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedPublishRun(ctx, t, pool, "running")

	oldGenerated := time.Date(2026, 11, 4, 21, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	seedCurrentRelease(ctx, t, pool, "rel-old", oldGenerated)

	draft := publishDraftFixture(runID, "rel-bad", oldGenerated.Add(time.Hour))
	draft.Observations[0].RoomID = "missing-room"

	err := NewReleaseRepo(pool).CommitRelease(ctx, draft)
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("写入失败应返回 ErrRunStoreFailed，实际 %v", err)
	}

	if _, found := releaseCurrentState(ctx, t, pool, "rel-bad"); found {
		t.Fatal("回滚后不应留下新版本行")
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-old"); !found || !isCurrent {
		t.Fatalf("回滚后原版本仍应是 current: found=%v current=%v", found, isCurrent)
	}
	if total, current := countReleases(ctx, t, pool); total != 1 || current != 1 {
		t.Fatalf("回滚后应只剩原版本且 current 为一个: total=%d current=%d", total, current)
	}
	if got := countReleaseRows(ctx, t, pool, "observation", "rel-old"); got != 1 {
		t.Fatalf("回滚后原版本观测应不变，实际 %d 行", got)
	}

	state := loadRunPublishRow(ctx, t, pool, runID)
	if state.ReleaseID != "" || state.Unknown != 0 || state.Composite != 0 ||
		state.RoomsTotal != 0 || state.WeeksTotal != 0 {
		t.Fatalf("回滚后轮次不应被回写: %+v", state)
	}
	if state.ReleasedAt != nil || state.FinishedAt != nil || state.Status != "running" {
		t.Fatalf("回滚后轮次应保持 running 且未标记发布: %+v", state)
	}
}

// TestCommitReleaseKeepsTwoVersionsAgainstRealDB 覆盖 CP8：连续发布三次后
// release 只保留当前与上一版，最旧版本的观测随级联删除清掉，指向它的轮次
// release_id 被置空（ON DELETE SET NULL），任何时刻 current 都恰好一个。
func TestCommitReleaseKeepsTwoVersionsAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewReleaseRepo(pool)

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	runIDs := make([]int64, 3)
	releaseIDs := []string{"rel-1", "rel-2", "rel-3"}
	for index := range runIDs {
		runIDs[index] = seedPublishRun(ctx, t, pool, "running")
		draft := publishDraftFixture(runIDs[index], releaseIDs[index], base.Add(time.Duration(index)*time.Hour))
		if err := repo.CommitRelease(ctx, draft); err != nil {
			t.Fatalf("第 %d 次发布失败: %v", index+1, err)
		}

		total, current := countReleases(ctx, t, pool)
		if current != 1 {
			t.Fatalf("第 %d 次发布后 current 应为 1，实际 %d", index+1, current)
		}
		if index == 0 && total != 1 {
			t.Fatalf("首次发布后应只有 1 个版本，实际 %d", total)
		}
		if index > 0 && total != 2 {
			t.Fatalf("第 %d 次发布后应保留 2 个版本，实际 %d", index+1, total)
		}
	}

	if total, current := countReleases(ctx, t, pool); total != 2 || current != 1 {
		t.Fatalf("三次发布后应保留 2 个版本、1 个 current: total=%d current=%d", total, current)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-3"); !found || !isCurrent {
		t.Fatalf("最新版本应是 current: found=%v current=%v", found, isCurrent)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-2"); !found || isCurrent {
		t.Fatalf("上一版应保留且不是 current: found=%v current=%v", found, isCurrent)
	}
	if _, found := releaseCurrentState(ctx, t, pool, "rel-1"); found {
		t.Fatal("最旧版本应被删除")
	}
	if got := countReleaseRows(ctx, t, pool, "observation", "rel-1"); got != 0 {
		t.Fatalf("最旧版本的观测应级联删除，实际还剩 %d 行", got)
	}

	// 指向被删版本的轮次 release_id 应被置空，而不是让删除被外键挡住；
	// 独立发布标记 released_at 不随外键动作重置，那一轮仍然不能再发布。
	cleaned := loadRunPublishRow(ctx, t, pool, runIDs[0])
	if cleaned.ReleaseID != "" {
		t.Fatalf("最旧轮次的 release_id 应被置空，实际 %q", cleaned.ReleaseID)
	}
	if cleaned.ReleasedAt == nil {
		t.Fatal("最旧轮次的 released_at 不应被删旧版本重置")
	}
	if state := loadRunPublishRow(ctx, t, pool, runIDs[1]); state.ReleaseID != "rel-2" {
		t.Fatalf("上一版轮次应仍指向 rel-2，实际 %q", state.ReleaseID)
	}
	if state := loadRunPublishRow(ctx, t, pool, runIDs[2]); state.ReleaseID != "rel-3" {
		t.Fatalf("当前轮次应指向 rel-3，实际 %q", state.ReleaseID)
	}
}

// TestCommitReleaseKeepsNewestGeneratedAtAsPreviousVersionAgainstRealDB 锁定 CP8
// 的「上一版」口径（用户拍板）：上一版是生成时间次新的版本，不是「上一次发布
// 的那一版」。按 10:00 → 09:00 → 08:00 补发一批生成时间更早的数据，第三次之后
// 当前版本是 08:00，上一版必须保留生成时间次新的 10:00，中间的 09:00 被删掉。
func TestCommitReleaseKeepsNewestGeneratedAtAsPreviousVersionAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewReleaseRepo(pool)

	base := time.Date(2026, 11, 4, 10, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	// 发布顺序与生成时间顺序相反：每次都补发更早生成的数据。
	steps := []struct {
		releaseID string
		generated time.Time
	}{
		{"rel-gen-10", base},
		{"rel-gen-09", base.Add(-time.Hour)},
		{"rel-gen-08", base.Add(-2 * time.Hour)},
	}
	for _, step := range steps {
		runID := seedPublishRun(ctx, t, pool, "running")
		if err := repo.CommitRelease(ctx, publishDraftFixture(runID, step.releaseID, step.generated)); err != nil {
			t.Fatalf("发布 %s 失败: %v", step.releaseID, err)
		}
	}

	if total, current := countReleases(ctx, t, pool); total != 2 || current != 1 {
		t.Fatalf("三次发布后应保留 2 个版本、1 个 current: total=%d current=%d", total, current)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-gen-08"); !found || !isCurrent {
		t.Fatalf("最后发布（生成时间最早）的版本应是 current: found=%v current=%v", found, isCurrent)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-gen-10"); !found || isCurrent {
		t.Fatalf("生成时间次新的 10:00 版本应作为上一版保留: found=%v current=%v", found, isCurrent)
	}
	if _, found := releaseCurrentState(ctx, t, pool, "rel-gen-09"); found {
		t.Fatal("生成时间 09:00 的中间版本应被删除")
	}
	if got := countReleaseRows(ctx, t, pool, "observation", "rel-gen-09"); got != 0 {
		t.Fatalf("被删版本的观测应级联删除，实际还剩 %d 行", got)
	}
}

// TestCommitReleaseRejectsRepublishAgainstRealDB 覆盖「一轮只能发布一次」：
// 同一轮第二次发布会整体回滚，既不产生新版本，也不改 already 写好的关联。
func TestCommitReleaseRejectsRepublishAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedPublishRun(ctx, t, pool, "running")
	repo := NewReleaseRepo(pool)

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	if err := repo.CommitRelease(ctx, publishDraftFixture(runID, "rel-first", base)); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}

	err := repo.CommitRelease(ctx, publishDraftFixture(runID, "rel-second", base.Add(time.Hour)))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("重复发布应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已发布过版本") {
		t.Fatalf("错误应说明已发布过版本，实际 %q", err.Error())
	}

	if _, found := releaseCurrentState(ctx, t, pool, "rel-second"); found {
		t.Fatal("重复发布回滚后不应留下第二个版本")
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-first"); !found || !isCurrent {
		t.Fatalf("首次发布的版本应仍是 current: found=%v current=%v", found, isCurrent)
	}
	if state := loadRunPublishRow(ctx, t, pool, runID); state.ReleaseID != "rel-first" {
		t.Fatalf("轮次应仍指向首次发布的版本，实际 %q", state.ReleaseID)
	}
}

// TestCommitReleaseRejectsRepublishAfterOldReleaseDeletedAgainstRealDB 覆盖 minor 2
// 的闭环：CP8 删掉最旧版本时 collect_run.release_id 被 ON DELETE SET NULL 置空，
// 独立标记 released_at 仍在，因此那一轮仍然不能再发布；此时错误文案要说明
// 「版本已被清理」并带上 released_at，而不是报一个空版本号。
func TestCommitReleaseRejectsRepublishAfterOldReleaseDeletedAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewReleaseRepo(pool)

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	runIDs := make([]int64, 3)
	releaseIDs := []string{"rel-1", "rel-2", "rel-3"}
	for index := range runIDs {
		runIDs[index] = seedPublishRun(ctx, t, pool, "running")
		if err := repo.CommitRelease(ctx, publishDraftFixture(runIDs[index], releaseIDs[index], base.Add(time.Duration(index)*time.Hour))); err != nil {
			t.Fatalf("第 %d 次发布失败: %v", index+1, err)
		}
	}

	cleaned := loadRunPublishRow(ctx, t, pool, runIDs[0])
	if cleaned.ReleaseID != "" {
		t.Fatalf("最旧轮次的 release_id 应被置空，实际 %q", cleaned.ReleaseID)
	}
	if cleaned.ReleasedAt == nil {
		t.Fatal("最旧轮次的 released_at 不应被删旧版本重置")
	}

	err := repo.CommitRelease(ctx, publishDraftFixture(runIDs[0], "rel-4", base.Add(4*time.Hour)))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("已发布过的轮次再发布应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "版本已被清理") {
		t.Fatalf("错误应说明版本已被清理，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "released_at=") {
		t.Fatalf("错误应带上 released_at，实际 %q", err.Error())
	}
	if _, found := releaseCurrentState(ctx, t, pool, "rel-4"); found {
		t.Fatal("release_id 被置空后重发不应产生新版本")
	}
	if total, current := countReleases(ctx, t, pool); total != 2 || current != 1 {
		t.Fatalf("重发被拒后应仍是 2 个版本、1 个 current: total=%d current=%d", total, current)
	}
}

// TestCommitReleaseRejectsDuplicateReleaseIDAgainstRealDB 覆盖 minor 3：另一个
// 轮次使用已存在的版本标识时，返回「版本标识已存在」而不是撞 release_pkey。
func TestCommitReleaseRejectsDuplicateReleaseIDAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	repo := NewReleaseRepo(pool)

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	firstRun := seedPublishRun(ctx, t, pool, "running")
	if err := repo.CommitRelease(ctx, publishDraftFixture(firstRun, "rel-same", base)); err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}

	secondRun := seedPublishRun(ctx, t, pool, "running")
	err := repo.CommitRelease(ctx, publishDraftFixture(secondRun, "rel-same", base.Add(time.Hour)))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("重复版本标识应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("错误应说明版本标识已存在，实际 %q", err.Error())
	}
	// 不能把 release_pkey 的库错误原样暴露给调用方。
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		t.Fatalf("应返回可读拒绝而不是主键冲突: %v", err)
	}

	state := loadRunPublishRow(ctx, t, pool, secondRun)
	if state.ReleaseID != "" || state.ReleasedAt != nil || state.Status != "running" {
		t.Fatalf("被拒的轮次不应被回写: %+v", state)
	}
}

// TestCommitReleaseRejectsClosedRunAgainstRealDB 覆盖已收口轮次不能发布：
// 终态只能由收口写一次，发布不会把 failed/partial 的轮次重新拉回发布链路。
func TestCommitReleaseRejectsClosedRunAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedPublishRun(ctx, t, pool, "failed")

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	err := NewReleaseRepo(pool).CommitRelease(ctx, publishDraftFixture(runID, "rel-closed", base))
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("已收口轮次发布应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已收口") {
		t.Fatalf("错误应说明轮次已收口，实际 %q", err.Error())
	}
	if _, found := releaseCurrentState(ctx, t, pool, "rel-closed"); found {
		t.Fatal("已收口轮次不应产生版本")
	}
}

// TestCommitReleaseRejectsUndoneRequiredTasksAgainstRealDB 覆盖 minor 2 的
// 事务内复核（真实库口径）：只要 sync_task 里还有 pending 或 failed 的必需任务，
// 发布就整体拒绝——不写版本、不复位 current、不改轮次，绝不能让「没做完」落成 success。
func TestCommitReleaseRejectsUndoneRequiredTasksAgainstRealDB(t *testing.T) {
	cases := []struct {
		name string
		// undone 决定第二条必需任务保持 pending 还是置 failed。
		undone string
	}{
		{name: "必需任务仍 pending", undone: "pending"},
		{name: "必需任务已 failed", undone: "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, pool := openCollectRunTestDB(t)
			runID := seedPublishRun(ctx, t, pool, "running")
			repo := NewReleaseRepo(pool)

			now := time.Date(2026, 11, 4, 21, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			tasks, err := EnsureWeekMatrixTasks(ctx, pool, runID, []SyncTaskInput{
				{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 5},
				{Kind: SyncTaskKindWeekMatrix, Required: true, BuildingID: "Q10", Week: 6},
			}, now)
			if err != nil {
				t.Fatalf("写周矩阵任务失败: %v", err)
			}
			if err := CompleteSyncTask(ctx, pool, runID, tasks[0].TaskID, now); err != nil {
				t.Fatalf("置任务完成失败: %v", err)
			}
			if c.undone == "failed" {
				if err := FailSyncTask(ctx, pool, runID, tasks[1].TaskID, "矩阵结构不符合预期", now); err != nil {
					t.Fatalf("置任务失败失败: %v", err)
				}
			}

			base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
			publishErr := repo.CommitRelease(ctx, publishDraftFixture(runID, "rel-undone", base))
			if !errors.Is(publishErr, model.ErrRunStoreFailed) {
				t.Fatalf("必需任务未完成应返回 ErrRunStoreFailed，实际 %v", publishErr)
			}
			if !strings.Contains(publishErr.Error(), "必需任务未完成") {
				t.Fatalf("错误应说明必需任务未完成，实际 %q", publishErr.Error())
			}
			if _, found := releaseCurrentState(ctx, t, pool, "rel-undone"); found {
				t.Fatal("必需任务未完成不应写出任何版本")
			}
			state := loadRunPublishRow(ctx, t, pool, runID)
			if state.ReleaseID != "" || state.ReleasedAt != nil || state.Status != "running" {
				t.Fatalf("被拒的轮次不应被回写: %+v", state)
			}
		})
	}
}

// TestCommitReleaseCopiesLargeBatchAgainstRealDB 覆盖真实库跨批写入：
// 观测行数超过单批上限（5 万）时按批 COPY 全部落库，版本与 current 正常切换。
func TestCommitReleaseCopiesLargeBatchAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunTestDB(t)
	runID := seedPublishRun(ctx, t, pool, "running")

	const (
		roomCount = 150
		weekCount = 4
		nodeCount = 12
	)
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
SELECT 'B' || lpad(g::text, 4, '0'), '房' || g, '房' || g, 'Q10', '综合教学楼'
  FROM generate_series(1, $1) AS g`, roomCount)

	rooms := make([]ReleaseRoom, 0, roomCount)
	observations := make([]ReleaseObservation, 0, roomCount*weekCount*7*nodeCount)
	for roomIndex := 0; roomIndex < roomCount; roomIndex++ {
		roomID := fmt.Sprintf("B%04d", roomIndex+1)
		rooms = append(rooms, ReleaseRoom{RoomID: roomID, BuildingID: "Q10", BuildingName: "综合教学楼"})
		for week := 5; week < 5+weekCount; week++ {
			for weekday := 1; weekday <= 7; weekday++ {
				for node := 1; node <= nodeCount; node++ {
					observations = append(observations, ReleaseObservation{
						RoomID: roomID, Week: week, Weekday: weekday,
						Node: fmt.Sprintf("%02d", node), StateKey: "free", Available: true,
					})
				}
			}
		}
	}
	if len(observations) <= maxCopyRows {
		t.Fatalf("用例数据应超过单批上限 %d，实际 %d 行", maxCopyRows, len(observations))
	}

	base := time.Date(2026, 11, 4, 20, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	draft := ReleaseDraft{
		ReleaseID:   "rel-large",
		RunID:       runID,
		Term:        syncTaskFixtureTerm,
		GeneratedAt: base,
		UpdatedAt:   base.Add(time.Minute),
		DictVersion: 1,
		AxisVersion: 1,
		WindowStart: 5,
		WindowEnd:   5 + weekCount - 1,
		RoomsTotal:  roomCount,
		WeeksTotal:  weekCount,
		Status:      RunStatusSuccess,
		Rooms:       rooms,
	}
	draft.Observations = observations

	repo := NewReleaseRepo(pool)
	if err := repo.CommitRelease(ctx, draft); err != nil {
		t.Fatalf("跨批发布失败: %v", err)
	}
	if got := countReleaseRows(ctx, t, pool, "observation", "rel-large"); got != len(observations) {
		t.Fatalf("跨批观测行数不符: got %d want %d", got, len(observations))
	}
	if got := countReleaseRows(ctx, t, pool, "release_room_building", "rel-large"); got != roomCount {
		t.Fatalf("归属快照行数不符: got %d want %d", got, roomCount)
	}
	if isCurrent, found := releaseCurrentState(ctx, t, pool, "rel-large"); !found || !isCurrent {
		t.Fatalf("跨批发布的版本应是 current: found=%v current=%v", found, isCurrent)
	}
}
