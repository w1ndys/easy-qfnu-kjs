package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// releaseTxStub 是 releaseTx 的记录型替身：按 SQL 记下调用顺序与参数，
// 按用例预置各步骤的受影响行数与失败点。CopyFrom 会真的遍历行源，
// 因此能验证单批行数与切批边界，而不只是「调过一次 COPY」。
type releaseTxStub struct {
	calls       []string          // 调用顺序，形如 preflight / insert_release / copy:observation
	execArgs    map[string][]any  // 每条 SQL 最后一次的参数
	execErrSQL  string            // 命中的 SQL 返回 execErr
	execErr     error             // 预置的执行错误
	promoteRows int64             // 抬 current 的受影响行数
	summaryRows int64             // 回写轮次的受影响行数
	copyErrAt   int               // 第几次 COPY 失败（1 起，0 表示不失败）
	copyCalls   []releaseCopyCall // 每次 COPY 的表、列与全部行

	preflight     runReleasePreflight // 发布前置检查读回的状态
	preflightMiss bool                // 前置检查返回 no rows（轮次不存在）
	preflightErr  error               // 前置检查读库失败

	requiredUndone int   // 事务内复核必需任务时读到的未完成条数
	requiredErr    error // 事务内复核必需任务时读库失败

	rowRelease  string     // 回写 0 行后补读的 release_id
	rowReleased *time.Time // 回写 0 行后补读的 released_at
	rowStatus   string     // 回写 0 行后补读的 status
	rowNotFound bool       // 回写 0 行后补读返回 no rows
	rowErr      error      // 回写 0 行后补读失败
}

// releaseCopyCall 是一次 COPY 的行数据，测试用它断言列顺序、行内容与批大小。
type releaseCopyCall struct {
	table   string
	columns []string
	rows    [][]any
}

// newReleaseTxStub 返回默认全部成功的替身：前置检查可发布，两处 UPDATE 都命中 1 行。
func newReleaseTxStub() *releaseTxStub {
	return &releaseTxStub{
		execArgs:    make(map[string][]any),
		promoteRows: 1,
		summaryRows: 1,
		preflight:   runReleasePreflight{Status: "running"},
	}
}

// label 把 SQL 归一成稳定的调用标签；测试断言顺序只认标签，不比对整段 SQL。
func releaseSQLLabel(sql string) string {
	switch sql {
	case insertReleaseSQL:
		return "insert_release"
	case demoteCurrentSQL:
		return "demote"
	case promoteCurrentSQL:
		return "promote"
	case deleteOlderReleasesSQL:
		return "delete_older"
	case writeReleaseSummarySQL:
		return "run_summary"
	default:
		return "unknown"
	}
}

// Exec 实现 releaseTx：记下调用与参数，并按用例返回受影响行数。
func (s *releaseTxStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	label := releaseSQLLabel(sql)
	s.calls = append(s.calls, label)
	s.execArgs[label] = args
	if s.execErr != nil && s.execErrSQL == label {
		return pgconn.CommandTag{}, s.execErr
	}
	switch label {
	case "insert_release":
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case "demote":
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case "promote":
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", s.promoteRows)), nil
	case "delete_older":
		return pgconn.NewCommandTag("DELETE 1"), nil
	case "run_summary":
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", s.summaryRows)), nil
	default:
		return pgconn.NewCommandTag(""), nil
	}
}

// CopyFrom 实现 releaseTx：完整消费行源后留存一份，便于断言行内容与批大小。
func (s *releaseTxStub) CopyFrom(_ context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	call := releaseCopyCall{table: table[0], columns: append([]string(nil), columns...)}
	for source.Next() {
		values, err := source.Values()
		if err != nil {
			return 0, err
		}
		call.rows = append(call.rows, values)
	}
	if err := source.Err(); err != nil {
		return 0, err
	}
	s.copyCalls = append(s.copyCalls, call)
	s.calls = append(s.calls, "copy:"+table[0])
	if s.copyErrAt > 0 && len(s.copyCalls) == s.copyErrAt {
		return 0, errors.New("COPY 失败")
	}
	return int64(len(call.rows)), nil
}

// QueryRow 实现 releaseTx：服务发布前置检查与回写 0 行后的补读。
func (s *releaseTxStub) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if sql == preflightRunReleaseSQL {
		s.calls = append(s.calls, "preflight")
		return releasePreflightRow{
			value:    s.preflight,
			notFound: s.preflightMiss,
			err:      s.preflightErr,
		}
	}
	if sql == undoneRequiredTasksSQL {
		s.calls = append(s.calls, "required_tasks")
		return releaseCountRow{count: s.requiredUndone, err: s.requiredErr}
	}
	s.calls = append(s.calls, "writeback_probe")
	return releaseRowStub{
		value:    s.rowRelease,
		released: s.rowReleased,
		status:   s.rowStatus,
		notFound: s.rowNotFound,
		err:      s.rowErr,
	}
}

// releaseCountRow 是「数未完成必需任务」的 pgx.Row 替身：只有一个 count 列。
type releaseCountRow struct {
	count int
	err   error
}

// Scan 实现 pgx.Row。
func (r releaseCountRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("releaseCountRow: 期望 1 个目标，实际 %d", len(dest))
	}
	count, ok := dest[0].(*int)
	if !ok {
		return fmt.Errorf("releaseCountRow: 第 1 个目标不是 *int: %T", dest[0])
	}
	*count = r.count
	return nil
}

// releasePreflightRow 是前置检查的 pgx.Row 替身，列顺序与 preflightRunReleaseSQL 一致。
type releasePreflightRow struct {
	value    runReleasePreflight
	notFound bool
	err      error
}

// Scan 实现 pgx.Row。
func (r releasePreflightRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.notFound {
		return pgx.ErrNoRows
	}
	if len(dest) != 4 {
		return fmt.Errorf("releasePreflightRow: 期望 4 个目标，实际 %d", len(dest))
	}
	releaseID, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("releasePreflightRow: 第 1 个目标不是 *string: %T", dest[0])
	}
	releasedAt, ok := dest[1].(**time.Time)
	if !ok {
		return fmt.Errorf("releasePreflightRow: 第 2 个目标不是 **time.Time: %T", dest[1])
	}
	status, ok := dest[2].(*string)
	if !ok {
		return fmt.Errorf("releasePreflightRow: 第 3 个目标不是 *string: %T", dest[2])
	}
	exists, ok := dest[3].(*bool)
	if !ok {
		return fmt.Errorf("releasePreflightRow: 第 4 个目标不是 *bool: %T", dest[3])
	}
	*releaseID = r.value.ReleaseID
	*releasedAt = r.value.ReleasedAt
	*status = r.value.Status
	*exists = r.value.ReleaseExists
	return nil
}

// releaseRowStub 是 pgx.Row 的最小替身，列顺序与 selectRunReleaseSQL 一致。
type releaseRowStub struct {
	value    string
	released *time.Time
	status   string
	notFound bool
	err      error
}

// Scan 实现 pgx.Row。
func (r releaseRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.notFound {
		return pgx.ErrNoRows
	}
	if len(dest) != 3 {
		return fmt.Errorf("releaseRowStub: 期望 3 个目标，实际 %d", len(dest))
	}
	value, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("releaseRowStub: 第 1 个目标不是 *string: %T", dest[0])
	}
	released, ok := dest[1].(**time.Time)
	if !ok {
		return fmt.Errorf("releaseRowStub: 第 2 个目标不是 **time.Time: %T", dest[1])
	}
	status, ok := dest[2].(*string)
	if !ok {
		return fmt.Errorf("releaseRowStub: 第 3 个目标不是 *string: %T", dest[2])
	}
	*value = r.value
	*released = r.released
	*status = r.status
	return nil
}

// releaseDraftFixture 是一份最小但完整的发布草稿：
// 两行观测、一行说明、一行归属，足够覆盖全部写入步骤。
func releaseDraftFixture() ReleaseDraft {
	generatedAt := time.Date(2026, 11, 4, 22, 2, 51, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	return ReleaseDraft{
		ReleaseID:       "20261104T140251Z",
		RunID:           7,
		Term:            "2026-2027-1",
		GeneratedAt:     generatedAt,
		UpdatedAt:       generatedAt.Add(2 * time.Minute),
		DictVersion:     1,
		AxisVersion:     1,
		WindowStart:     5,
		WindowEnd:       8,
		HasAttrConflict: true,
		UnknownCells:    3,
		CompositeCells:  4,
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
			{RoomID: "r101", Weekday: 1, Block: "0102", StateKey: "class", Course: "高等数学", Derived: true},
		},
	}
}

// TestWriteReleaseWritesInFixedOrder 覆盖事务体的步骤顺序（Requirement 7.1–7.4）：
// 先锁住轮次做发布前置检查、按 sync_task 复核必需任务完成度，再以非 current
// 写版本与数据，然后降原 current、抬新 current、删更早版本，最后回写轮次。
// 顺序写错会让「新版本未写完就对外可见」「已发布过的轮次再写一次」或
// 「必需任务没做完就写终态」变成可能。
func TestWriteReleaseWritesInFixedOrder(t *testing.T) {
	tx := newReleaseTxStub()
	draft := releaseDraftFixture()

	if err := writeRelease(context.Background(), tx, draft); err != nil {
		t.Fatalf("发布事务体不应报错，实际 %v", err)
	}

	wantOrder := []string{
		"preflight",
		"required_tasks",
		"insert_release",
		"copy:observation",
		"copy:occupancy_detail",
		"copy:release_room_building",
		"demote",
		"promote",
		"delete_older",
		"run_summary",
	}
	if !reflect.DeepEqual(tx.calls, wantOrder) {
		t.Fatalf("写入顺序不符:\n got %v\nwant %v", tx.calls, wantOrder)
	}

	// 版本行：非 current 由 SQL 字面量保证，参数按列对齐。
	if got, want := tx.execArgs["insert_release"],
		[]any{draft.ReleaseID, draft.Term, draft.GeneratedAt, draft.DictVersion, draft.AxisVersion, draft.HasAttrConflict}; !reflect.DeepEqual(got, want) {
		t.Fatalf("写版本的参数不符: got %#v want %#v", got, want)
	}
	if got, want := tx.execArgs["promote"], []any{draft.ReleaseID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("抬 current 的参数不符: got %#v want %#v", got, want)
	}
	if got, want := tx.execArgs["run_summary"],
		[]any{
			draft.RunID, draft.UnknownCells, draft.CompositeCells,
			draft.RoomsTotal, draft.WeeksTotal,
			draft.ReleaseID, draft.UpdatedAt, draft.Status, RunPhasePublish,
			draft.ErrorMessage,
		}; !reflect.DeepEqual(got, want) {
		t.Fatalf("回写轮次的参数不符: got %#v want %#v", got, want)
	}
	if !strings.Contains(demoteCurrentSQL, "WHERE is_current") {
		t.Fatalf("降 current 必须只针对现为 current 的行: %q", demoteCurrentSQL)
	}
	// 两道守卫都要在 SQL 里：独立发布标记与 running 状态。
	if !strings.Contains(writeReleaseSummarySQL, "released_at IS NULL") ||
		!strings.Contains(writeReleaseSummarySQL, "status = 'running'") {
		t.Fatalf("回写轮次必须带发布标记与 running 守卫: %q", writeReleaseSummarySQL)
	}
	// 失败原因同一条 UPDATE 落库，空串折 NULL。
	if !strings.Contains(writeReleaseSummarySQL, "error_message = NULLIF($10, '')") {
		t.Fatalf("回写轮次必须写 error_message（空串折 NULL）: %q", writeReleaseSummarySQL)
	}
}

// TestWriteReleaseErrorMessageForPartialAndSuccess 覆盖用户拍板：partial 把原因
// 写进 collect_run.error_message；success 清空，即使调用方误传了文案也不落库，
// 不把失败原因留在成功轮次上。
func TestWriteReleaseErrorMessageForPartialAndSuccess(t *testing.T) {
	t.Run("partial 写原因", func(t *testing.T) {
		draft := releaseDraftFixture()
		draft.Status = RunStatusPartial
		draft.ErrorMessage = "明细页结构异常（task 12）"
		tx := newReleaseTxStub()

		if err := writeRelease(context.Background(), tx, draft); err != nil {
			t.Fatalf("发布事务体不应报错，实际 %v", err)
		}
		args := tx.execArgs["run_summary"]
		// 参数顺序：$8 终态、$9 阶段、$10 失败原因。
		if got, want := args[7], any(RunStatusPartial); got != want {
			t.Fatalf("回写终态不符: got %v want %v", got, want)
		}
		if got, want := args[8], any(RunPhasePublish); got != want {
			t.Fatalf("回写阶段不符: got %v want %v", got, want)
		}
		if got, want := args[9], any(draft.ErrorMessage); got != want {
			t.Fatalf("partial 应写失败原因: got %#v want %#v", got, want)
		}
	})

	t.Run("success 清空", func(t *testing.T) {
		draft := releaseDraftFixture()
		draft.Status = RunStatusSuccess
		draft.ErrorMessage = "上一状态残留的原因"
		tx := newReleaseTxStub()

		if err := writeRelease(context.Background(), tx, draft); err != nil {
			t.Fatalf("发布事务体不应报错，实际 %v", err)
		}
		args := tx.execArgs["run_summary"]
		if got, want := args[7], any(RunStatusSuccess); got != want {
			t.Fatalf("回写终态不符: got %v want %v", got, want)
		}
		// 空串经 SQL 的 NULLIF 折成 NULL，也就是把这一列清掉。
		if got, want := args[9], any(""); got != want {
			t.Fatalf("success 必须清空 error_message: got %#v want %#v", got, want)
		}
	})
}

// TestDeleteOlderReleasesSQLKeepsNewestGenerated 覆盖 CP8 的选版口径在默认门禁里
// 也有断言：连库用例需要 DSN、平时会被跳过，SQL 片段回归只能靠这条测试挡住。
// 「上一版」按 generated_at 倒序取非当前版本的第一条，同一生成时刻用 release_id
// 倒序做稳定 tie-break；不能改成按发布顺序（没有这一列）或只用 release_id 排序。
func TestDeleteOlderReleasesSQLKeepsNewestGenerated(t *testing.T) {
	for _, fragment := range []string{
		"release_id <> $1",
		"ORDER BY generated_at DESC",
		"LIMIT 1",
	} {
		if !strings.Contains(deleteOlderReleasesSQL, fragment) {
			t.Fatalf("删除旧版本 SQL 缺少 %q: %q", fragment, deleteOlderReleasesSQL)
		}
	}
	// 片段可能分散在子查询的不同位置仍各自命中，这里按排序子句整体再断言一次，
	// 并容忍 SQL 字面量里为了对齐写的换行与多空格。
	ordered := regexp.MustCompile(`(?s)ORDER BY\s+generated_at\s+DESC,\s*release_id\s+DESC\s+LIMIT\s+1`)
	if !ordered.MatchString(deleteOlderReleasesSQL) {
		t.Fatalf("删除旧版本 SQL 的排序子句不符（应为 generated_at DESC, release_id DESC 后接 LIMIT 1）: %q",
			deleteOlderReleasesSQL)
	}
}

// TestWriteReleaseCopiesRowsWithColumnOrder 覆盖 COPY 的列顺序与可空列折 NULL：
// 列顺序与 schema 对齐、week/weekday 用 smallint 能接受的整数、空原文写 NULL。
func TestWriteReleaseCopiesRowsWithColumnOrder(t *testing.T) {
	tx := newReleaseTxStub()
	draft := releaseDraftFixture()

	if err := writeRelease(context.Background(), tx, draft); err != nil {
		t.Fatalf("发布事务体不应报错，实际 %v", err)
	}
	if len(tx.copyCalls) != 3 {
		t.Fatalf("应有三张表各一次 COPY，实际 %d 次", len(tx.copyCalls))
	}

	observations := tx.copyCalls[0]
	if !reflect.DeepEqual(observations.columns, observationColumns) {
		t.Fatalf("观测列顺序不符: got %v want %v", observations.columns, observationColumns)
	}
	wantFirst := []any{draft.ReleaseID, "r101", draft.Term, int16(5), int16(1), "01", "free", true, nil}
	if !reflect.DeepEqual(observations.rows[0], wantFirst) {
		t.Fatalf("首行观测不符:\n got %#v\nwant %#v", observations.rows[0], wantFirst)
	}
	if got := observations.rows[1][8]; got != "有课" {
		t.Fatalf("非空原文应原样写入，实际 %#v", got)
	}

	details := tx.copyCalls[1]
	if !reflect.DeepEqual(details.columns, detailColumns) {
		t.Fatalf("说明列顺序不符: got %v want %v", details.columns, detailColumns)
	}
	wantDetail := []any{draft.ReleaseID, "r101", int16(1), "0102", "class", "高等数学", nil, nil, true}
	if !reflect.DeepEqual(details.rows[0], wantDetail) {
		t.Fatalf("说明行不符:\n got %#v\nwant %#v", details.rows[0], wantDetail)
	}

	rooms := tx.copyCalls[2]
	if !reflect.DeepEqual(rooms.columns, roomBuildingColumns) {
		t.Fatalf("归属快照列顺序不符: got %v want %v", rooms.columns, roomBuildingColumns)
	}
	wantRoom := []any{draft.ReleaseID, "r101", "Q10", "综合教学楼"}
	if !reflect.DeepEqual(rooms.rows[0], wantRoom) {
		t.Fatalf("归属快照行不符:\n got %#v\nwant %#v", rooms.rows[0], wantRoom)
	}
}

// TestWriteReleaseSkipsEmptyCollections 覆盖空说明：没有行就不发 COPY，
// 不给事务加无意义的往返（空归属会先被 validateReleaseDraft 拦下）。
func TestWriteReleaseSkipsEmptyCollections(t *testing.T) {
	tx := newReleaseTxStub()
	draft := releaseDraftFixture()
	draft.Details = nil

	if err := writeRelease(context.Background(), tx, draft); err != nil {
		t.Fatalf("发布事务体不应报错，实际 %v", err)
	}
	wantOrder := []string{
		"preflight",
		"required_tasks",
		"insert_release",
		"copy:observation",
		"copy:release_room_building",
		"demote",
		"promote",
		"delete_older",
		"run_summary",
	}
	if !reflect.DeepEqual(tx.calls, wantOrder) {
		t.Fatalf("空说明不应发 COPY，归属快照仍要写:\n got %v\nwant %v", tx.calls, wantOrder)
	}
}

// TestCopyInBatchesSplitsAtLimit 覆盖批大小：单批不超过 5 万行（原 spec 设计
// 「写入策略」、Requirement 28.3），并且切批按全局行号连续、不丢行不重行。
func TestCopyInBatchesSplitsAtLimit(t *testing.T) {
	tx := newReleaseTxStub()
	total := 2*maxCopyRows + 1

	err := copyInBatches(context.Background(), tx, pgx.Identifier{"observation"}, observationColumns, total,
		func(index int) ([]any, error) { return []any{index}, nil })
	if err != nil {
		t.Fatalf("分批判量写入不应报错，实际 %v", err)
	}
	if len(tx.copyCalls) != 3 {
		t.Fatalf("总行数 %d 应切 3 批，实际 %d 批", total, len(tx.copyCalls))
	}
	wantSizes := []int{maxCopyRows, maxCopyRows, 1}
	for index, call := range tx.copyCalls {
		if len(call.rows) > maxCopyRows {
			t.Fatalf("第 %d 批 %d 行超过上限 %d", index+1, len(call.rows), maxCopyRows)
		}
		if len(call.rows) != wantSizes[index] {
			t.Fatalf("第 %d 批行数不符: got %d want %d", index+1, len(call.rows), wantSizes[index])
		}
	}
	// 行号必须连续：第二批从 maxCopyRows 开始，最后一批是 total-1。
	if got := tx.copyCalls[1].rows[0][0]; got != maxCopyRows {
		t.Fatalf("第二批首行行号不符: %v", got)
	}
	if got := tx.copyCalls[2].rows[0][0]; got != total-1 {
		t.Fatalf("最后一批首行行号不符: %v", got)
	}
}

// TestCopyInBatchesExactLimit 覆盖恰好等于上限与超出上限一行的两种边界：
// 等于上限只发一批，超一行拆成两批。
func TestCopyInBatchesExactLimit(t *testing.T) {
	for _, total := range []int{maxCopyRows, maxCopyRows + 1} {
		tx := newReleaseTxStub()
		err := copyInBatches(context.Background(), tx, pgx.Identifier{"observation"}, observationColumns, total,
			func(index int) ([]any, error) { return []any{index}, nil })
		if err != nil {
			t.Fatalf("批大小 %d 时不应报错，实际 %v", total, err)
		}
		want := 1
		if total > maxCopyRows {
			want = 2
		}
		if len(tx.copyCalls) != want {
			t.Fatalf("批大小 %d 应切 %d 批，实际 %d 批", total, want, len(tx.copyCalls))
		}
	}
}

// TestWriteReleaseRejectsUndoneRequiredTasks 覆盖 minor 2 的事务内复核：
// 只要还有 pending/failed 的必需任务，就在写建立版本之前拒绝，既不能把
// 「没做完」写成 success，也不能留下半成品版本。
func TestWriteReleaseRejectsUndoneRequiredTasks(t *testing.T) {
	tx := newReleaseTxStub()
	tx.requiredUndone = 2

	err := writeRelease(context.Background(), tx, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("必需任务未完成应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "必需任务未完成") {
		t.Fatalf("错误应说明必需任务未完成，实际 %q", err.Error())
	}
	if want := []string{"preflight", "required_tasks"}; !reflect.DeepEqual(tx.calls, want) {
		t.Fatalf("必需任务未完成时不应继续写:\n got %v\nwant %v", tx.calls, want)
	}
	// 复核 SQL 必须真的按 sync_task 的必需性与状态判，不能只数 done 行。
	for _, fragment := range []string{"sync_task", "required", "status <> 'done'"} {
		if !strings.Contains(undoneRequiredTasksSQL, fragment) {
			t.Fatalf("必需任务复核 SQL 缺少 %q: %q", fragment, undoneRequiredTasksSQL)
		}
	}
}

// TestWriteReleaseRejectsUnreadableRequiredTasks 覆盖复核读库失败：不能把
// 「读不到」当成「没有未完成任务」，必须整体失败。
func TestWriteReleaseRejectsUnreadableRequiredTasks(t *testing.T) {
	tx := newReleaseTxStub()
	tx.requiredErr = errors.New("连接中断")

	err := writeRelease(context.Background(), tx, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("复核读库失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if _, written := tx.execArgs["insert_release"]; written {
		t.Fatal("复核失败后不应写版本")
	}
}

// TestWriteReleaseStopsWhenCopyFails 覆盖中途失败即停：COPY 失败后不得继续
// 降 current、删旧版本或回写轮次，否则事务内的中间状态会掩盖真正的失败点。
func TestWriteReleaseStopsWhenCopyFails(t *testing.T) {
	tx := newReleaseTxStub()
	tx.copyErrAt = 1

	err := writeRelease(context.Background(), tx, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("COPY 失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if want := []string{"preflight", "required_tasks", "insert_release", "copy:observation"}; !reflect.DeepEqual(tx.calls, want) {
		t.Fatalf("COPY 失败后不应继续写:\n got %v\nwant %v", tx.calls, want)
	}
}

// TestWriteReleaseStopsWhenPromoteMisses 覆盖「抬新 current 命中 0 行」：
// 说明新版本没写进去，必须失败，不能留下没有 current 的库（CP7）。
func TestWriteReleaseStopsWhenPromoteMisses(t *testing.T) {
	tx := newReleaseTxStub()
	tx.promoteRows = 0

	err := writeRelease(context.Background(), tx, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("抬 current 未命中应返回 ErrRunStoreFailed，实际 %v", err)
	}
	for _, call := range tx.calls {
		if call == "delete_older" || call == "run_summary" {
			t.Fatalf("抬 current 未命中后不应继续写，实际调用 %v", tx.calls)
		}
	}
	if _, called := tx.execArgs["delete_older"]; called {
		t.Fatal("抬 current 未命中后不应删除旧版本")
	}
}

// TestWriteReleaseStopsWhenExecFails 覆盖 Exec 步骤失败：降 current 失败即刻返回，
// 后续的抬 current 与删版本都不执行。
func TestWriteReleaseStopsWhenExecFails(t *testing.T) {
	tx := newReleaseTxStub()
	tx.execErrSQL = "demote"
	tx.execErr = errors.New("锁等待超时")

	err := writeRelease(context.Background(), tx, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("降 current 失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	for _, call := range tx.calls {
		if call == "promote" || call == "delete_older" || call == "run_summary" {
			t.Fatalf("降 current 失败后不应继续写，实际调用 %v", tx.calls)
		}
	}
}

// TestWriteReleaseRunWritebackReasons 覆盖回写轮次 0 行更新的几种原因：
// 已发布过版本（独立标记或旧 release_id）、轮次已收口、轮次不存在，必须分开报。
func TestWriteReleaseRunWritebackReasons(t *testing.T) {
	t.Run("已发布过版本（独立标记）", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		releasedAt := time.Date(2026, 11, 4, 21, 0, 0, 0, time.UTC)
		tx.rowReleased = &releasedAt
		tx.rowRelease = "rel-old"
		tx.rowStatus = "running"
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "已发布过版本") || !strings.Contains(err.Error(), "rel-old") {
			t.Fatalf("错误应说明已发布过版本，实际 %q", err.Error())
		}
	})

	t.Run("已发布过版本（旧 release_id）", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		tx.rowRelease = "rel-old"
		tx.rowStatus = "running"
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "已发布过版本") || !strings.Contains(err.Error(), "rel-old") {
			t.Fatalf("错误应说明已发布过版本，实际 %q", err.Error())
		}
	})

	t.Run("已发布过（版本已被清理）", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		releasedAt := time.Date(2026, 11, 4, 21, 0, 0, 0, time.UTC)
		tx.rowReleased = &releasedAt
		tx.rowStatus = "running"
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		// release_id 已被 CP8 的删旧版本置空：文案要说明版本已被清理，
		// 并带上 released_at，不能报一个空版本号。
		if !strings.Contains(err.Error(), "版本已被清理") || !strings.Contains(err.Error(), "released_at=") {
			t.Fatalf("错误应说明版本已被清理并带 released_at，实际 %q", err.Error())
		}
	})

	t.Run("轮次已收口", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		tx.rowStatus = "failed"
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "已收口") {
			t.Fatalf("错误应说明轮次已收口，实际 %q", err.Error())
		}
	})

	t.Run("轮次不存在", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		tx.rowNotFound = true
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "不存在") {
			t.Fatalf("错误应说明轮次不存在，实际 %q", err.Error())
		}
	})

	t.Run("补读失败", func(t *testing.T) {
		tx := newReleaseTxStub()
		tx.summaryRows = 0
		tx.rowErr = errors.New("连接中断")
		err := writeRelease(context.Background(), tx, releaseDraftFixture())
		if !errors.Is(err, model.ErrRunStoreFailed) {
			t.Fatalf("应返回 ErrRunStoreFailed，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "无法确认原因") {
			t.Fatalf("错误应说明无法确认原因，实际 %q", err.Error())
		}
	})
}

// fakePgxTx 是 pgx.Tx 的替身：嵌入接口只为补齐未使用的方法，
// 测试只走 Exec/CopyFrom/QueryRow/Commit/Rollback，其余方法调用会 panic。
type fakePgxTx struct {
	pgx.Tx
	stub       *releaseTxStub
	committed  bool
	rolledBack bool
	commitErr  error
}

// Exec 见 releaseTxStub.Exec。
func (f *fakePgxTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.stub.Exec(ctx, sql, args...)
}

// CopyFrom 见 releaseTxStub.CopyFrom。
func (f *fakePgxTx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	return f.stub.CopyFrom(ctx, table, columns, source)
}

// QueryRow 见 releaseTxStub.QueryRow。
func (f *fakePgxTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.stub.QueryRow(ctx, sql, args...)
}

// Commit 记录提交动作，便于断言失败路径没有提交。
func (f *fakePgxTx) Commit(context.Context) error {
	f.committed = true
	return f.commitErr
}

// Rollback 记录回滚动作：失败路径必须回滚，成功路径上 defer 的回滚是空操作。
func (f *fakePgxTx) Rollback(context.Context) error {
	f.rolledBack = true
	return nil
}

// releaseBeginnerStub 是 releaseTxBeginner 的替身，返回预置的事务。
type releaseBeginnerStub struct {
	tx     pgx.Tx
	err    error
	begins int
}

// Begin 实现 releaseTxBeginner。
func (b *releaseBeginnerStub) Begin(context.Context) (pgx.Tx, error) {
	b.begins++
	return b.tx, b.err
}

// TestCommitReleaseCommitsOnSuccess 覆盖提交路径：写完后提交一次，defer 的回滚
// 不再影响已提交的事务。
func TestCommitReleaseCommitsOnSuccess(t *testing.T) {
	tx := &fakePgxTx{stub: newReleaseTxStub()}
	beginner := &releaseBeginnerStub{tx: tx}

	if err := CommitRelease(context.Background(), beginner, releaseDraftFixture()); err != nil {
		t.Fatalf("提交发布不应报错，实际 %v", err)
	}
	if beginner.begins != 1 {
		t.Fatalf("应只开启 1 个事务，实际 %d 个", beginner.begins)
	}
	if !tx.committed {
		t.Fatal("成功路径应提交事务")
	}
}

// TestCommitReleaseRollsBackWhenWriteFails 覆盖 CP7 的实现面：事务体中途失败
// 时不提交，并回滚整个事务，旧 current 不被降级。
func TestCommitReleaseRollsBackWhenWriteFails(t *testing.T) {
	stub := newReleaseTxStub()
	stub.copyErrAt = 1
	tx := &fakePgxTx{stub: stub}
	beginner := &releaseBeginnerStub{tx: tx}

	err := CommitRelease(context.Background(), beginner, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("写入失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if tx.committed {
		t.Fatal("写入失败不应提交事务")
	}
	if !tx.rolledBack {
		t.Fatal("写入失败必须回滚事务")
	}
}

// TestCommitReleaseRollsBackWhenCommitFails 覆盖提交本身失败：返回可读错误，
// 事务不再可用，线上仍是原 current。
func TestCommitReleaseRollsBackWhenCommitFails(t *testing.T) {
	tx := &fakePgxTx{stub: newReleaseTxStub(), commitErr: errors.New("序列化冲突")}
	beginner := &releaseBeginnerStub{tx: tx}

	err := CommitRelease(context.Background(), beginner, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("提交失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "提交失败") {
		t.Fatalf("错误应说明提交失败，实际 %q", err.Error())
	}
}

// TestCommitReleaseBeginFailure 覆盖事务开启失败：不进入事务体，也不 panic。
func TestCommitReleaseBeginFailure(t *testing.T) {
	beginner := &releaseBeginnerStub{err: errors.New("连接池耗尽")}

	err := CommitRelease(context.Background(), beginner, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("开启事务失败应返回 ErrRunStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "事务开启失败") {
		t.Fatalf("错误应说明事务开启失败，实际 %q", err.Error())
	}
}

// TestCommitReleaseRejectsInvalidDraft 覆盖草稿的身份字段校验：
// 坏草稿在开事务之前就被拒绝，不浪费一次连接，也不靠 SQL 约束兜底。
func TestCommitReleaseRejectsInvalidDraft(t *testing.T) {
	cases := map[string]func(*ReleaseDraft){
		"缺版本标识":     func(d *ReleaseDraft) { d.ReleaseID = "" },
		"缺轮次编号":     func(d *ReleaseDraft) { d.RunID = 0 },
		"缺学期":       func(d *ReleaseDraft) { d.Term = "" },
		"缺生成时间":     func(d *ReleaseDraft) { d.GeneratedAt = time.Time{} },
		"缺发布时刻":     func(d *ReleaseDraft) { d.UpdatedAt = time.Time{} },
		"字典版本非法":    func(d *ReleaseDraft) { d.DictVersion = 0 },
		"轴版本非法":     func(d *ReleaseDraft) { d.AxisVersion = -1 },
		"未知计数为负":    func(d *ReleaseDraft) { d.UnknownCells = -1 },
		"复合计数为负":    func(d *ReleaseDraft) { d.CompositeCells = -1 },
		"房号数非法":     func(d *ReleaseDraft) { d.RoomsTotal = 0 },
		"窗口周数非法":    func(d *ReleaseDraft) { d.WeeksTotal = 0 },
		"窗口非法":      func(d *ReleaseDraft) { d.WindowStart = 0 },
		"窗口倒挂":      func(d *ReleaseDraft) { d.WindowEnd = d.WindowStart - 1 },
		"房号数与快照不符":  func(d *ReleaseDraft) { d.RoomsTotal = 2 },
		"窗口周数与窗口不符": func(d *ReleaseDraft) { d.WeeksTotal = 5 },
		"终态非法":      func(d *ReleaseDraft) { d.Status = "running" },
		"终态失败":      func(d *ReleaseDraft) { d.Status = RunStatusPartial + "-x" },
		"没有观测":      func(d *ReleaseDraft) { d.Observations = nil },
		"没有归属":      func(d *ReleaseDraft) { d.Rooms = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			draft := releaseDraftFixture()
			mutate(&draft)
			beginner := &releaseBeginnerStub{tx: &fakePgxTx{stub: newReleaseTxStub()}}

			err := CommitRelease(context.Background(), beginner, draft)
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("坏草稿应返回 ErrRunStoreFailed，实际 %v", err)
			}
			if beginner.begins != 0 {
				t.Fatalf("坏草稿不应开启事务，实际开启 %d 次", beginner.begins)
			}
		})
	}
}

// TestCommitReleasePreflightRejects 覆盖发布前置检查：轮次不存在、已发布过
// （独立标记或旧 release_id）、已收口、目标版本标识已被占用、读库失败，
// 都在写 release 之前拒绝并回滚，返回可读原因而不是撞主键/外键。
func TestCommitReleasePreflightRejects(t *testing.T) {
	releasedAt := time.Date(2026, 11, 4, 21, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		setup         func(*releaseTxStub)
		wantSubstring string
	}{
		{
			name:          "轮次不存在",
			setup:         func(s *releaseTxStub) { s.preflightMiss = true },
			wantSubstring: "不存在",
		},
		{
			name: "已发布过版本（独立标记）",
			setup: func(s *releaseTxStub) {
				s.preflight.ReleaseID = "rel-old"
				s.preflight.ReleasedAt = &releasedAt
			},
			wantSubstring: "已发布过版本",
		},
		{
			name: "已发布过版本（旧 release_id）",
			setup: func(s *releaseTxStub) {
				s.preflight.ReleaseID = "rel-old"
			},
			wantSubstring: "已发布过版本",
		},
		{
			name: "已发布过（版本已被清理）",
			setup: func(s *releaseTxStub) {
				s.preflight.ReleasedAt = &releasedAt
			},
			wantSubstring: "版本已被清理",
		},
		{
			name:          "轮次已收口",
			setup:         func(s *releaseTxStub) { s.preflight.Status = "failed" },
			wantSubstring: "已收口",
		},
		{
			name:          "版本标识已被占用",
			setup:         func(s *releaseTxStub) { s.preflight.ReleaseExists = true },
			wantSubstring: "已存在",
		},
		{
			name:          "读库失败",
			setup:         func(s *releaseTxStub) { s.preflightErr = errors.New("连接中断") },
			wantSubstring: "读取轮次",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newReleaseTxStub()
			c.setup(stub)
			tx := &fakePgxTx{stub: stub}
			beginner := &releaseBeginnerStub{tx: tx}

			err := CommitRelease(context.Background(), beginner, releaseDraftFixture())
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("前置检查失败应返回 ErrRunStoreFailed，实际 %v", err)
			}
			if !strings.Contains(err.Error(), c.wantSubstring) {
				t.Fatalf("错误应包含 %q，实际 %q", c.wantSubstring, err.Error())
			}
			if tx.committed {
				t.Fatal("前置检查失败不应提交事务")
			}
			if !tx.rolledBack {
				t.Fatal("前置检查失败必须回滚事务")
			}
			for _, call := range stub.calls {
				if call == "insert_release" {
					t.Fatalf("前置检查失败后不应写版本，实际调用 %v", stub.calls)
				}
			}
		})
	}
}

// TestCommitReleaseRejectsMissingPool 覆盖装配错误：没有连接池时直接拒绝。
func TestCommitReleaseRejectsMissingPool(t *testing.T) {
	err := CommitRelease(context.Background(), nil, releaseDraftFixture())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("缺少连接池应返回 ErrRunStoreFailed，实际 %v", err)
	}
}
