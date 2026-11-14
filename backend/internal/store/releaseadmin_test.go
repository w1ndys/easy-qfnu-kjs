package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// releaseAdminRowsStub 是 pgx.Rows 的替身：只覆盖版本读取用到的 Next/Scan/Err/Close，
// 其余方法嵌入接口、调用会 panic——替身只放行被测路径，测错方法立刻暴露。
//
// Scan 按位置真赋值并校验类型：只会回 nil 的替身能让「列顺序写反」「可空列扫进
// 值类型」照样通过，而那正是只在真实库上才炸的错误。
type releaseAdminRowsStub struct {
	pgx.Rows
	rows    [][]any // 依次回放的行，每行按 Scan 目标的列顺序给值
	current int     // 已被 Next 取出的行数，也是当前行在 rows 里的下标 +1

	scanErr error // 下一次 Scan 回放的扫描错误（有行，但这一行解不出来）

	err    error // Err 回放的遍历错误（取到一半连接断了）
	closed bool  // Close 是否被调用：漏关行集会占住连接，而返回值看起来一切正常
}

// Next 逐行推进：取完 rows 后返回 false，模拟行集遍历结束。
func (r *releaseAdminRowsStub) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

// Scan 把当前行的值按位置赋给 dest；列数或类型不符立即报错。
func (r *releaseAdminRowsStub) Scan(dest ...any) error {
	if r.scanErr != nil {
		err := r.scanErr
		r.scanErr = nil
		return err
	}
	if r.current == 0 {
		return errors.New("没有当前行时调用 Scan")
	}
	return scanReleaseAdminValues(dest, r.rows[r.current-1])
}

// Close 由仓储的 defer 调用，并记下真的被调过。
func (r *releaseAdminRowsStub) Close() { r.closed = true }

// Err 回放预设的遍历错误。
func (r *releaseAdminRowsStub) Err() error { return r.err }

// releaseAdminRowStub 是 pgx.Row 的替身：按位置回放一行，或回放预设错误。
type releaseAdminRowStub struct {
	values []any
	err    error
}

// Scan 把预设值按位置赋给 dest；err 非空时直接返回它（用于 pgx.ErrNoRows 等）。
func (r *releaseAdminRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanReleaseAdminValues(dest, r.values)
}

// scanReleaseAdminValues 按位置赋值，dest 与 values 的列数必须一致。
func scanReleaseAdminValues(dest []any, values []any) error {
	if len(dest) != len(values) {
		return fmt.Errorf("列数期望 %d，实际 %d", len(values), len(dest))
	}
	for i := range dest {
		if err := assignReleaseAdminValue(dest[i], values[i]); err != nil {
			return fmt.Errorf("第 %d 列: %w", i, err)
		}
	}
	return nil
}

// assignReleaseAdminValue 是单列赋值：值类型直接写，可空列的 nil 写成 nil 指针。
// 不支持的组合返回错误而不是静默跳过，避免替身掩盖仓储扫错列。
func assignReleaseAdminValue(dest any, value any) error {
	switch d := dest.(type) {
	case *string:
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("期望 string，实际 %T", value)
		}
		*d = v
	case *int:
		v, ok := value.(int)
		if !ok {
			return fmt.Errorf("期望 int，实际 %T", value)
		}
		*d = v
	case *int16:
		v, ok := value.(int16)
		if !ok {
			return fmt.Errorf("期望 int16，实际 %T", value)
		}
		*d = v
	case *int64:
		v, ok := value.(int64)
		if !ok {
			return fmt.Errorf("期望 int64，实际 %T", value)
		}
		*d = v
	case *bool:
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("期望 bool，实际 %T", value)
		}
		*d = v
	case *time.Time:
		v, ok := value.(time.Time)
		if !ok {
			return fmt.Errorf("期望 time.Time，实际 %T", value)
		}
		*d = v
	case **time.Time:
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(time.Time)
		if !ok {
			return fmt.Errorf("期望 time.Time 或 nil，实际 %T", value)
		}
		*d = &v
	default:
		return fmt.Errorf("目标类型 %T 不被替身支持", dest)
	}
	return nil
}

// releaseAdminPoolStub 是 releaseAdminPool 的替身：List 走 Query，Detail 走 BeginTx。
// QueryRow 没有调用点（列表与详情的单行读都在事务体内），调用即 panic。
type releaseAdminPoolStub struct {
	rows     pgx.Rows
	queryErr error

	beginErr  error
	tx        pgx.Tx
	txOptions pgx.TxOptions // BeginTx 收到的隔离级别与读写模式，供 Detail 断言
}

// Query 回放预设的行集或错误。
func (s *releaseAdminPoolStub) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return s.rows, s.queryErr
}

// QueryRow 实现接口但不被版本读取仓储在池上调用。
func (s *releaseAdminPoolStub) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("单元测试不应在池上调用 QueryRow：单行读都在 Detail 的事务体内")
}

// BeginTx 记下事务选项，再回放预设的事务或错误。
func (s *releaseAdminPoolStub) BeginTx(_ context.Context, txOptions pgx.TxOptions) (pgx.Tx, error) {
	s.txOptions = txOptions
	return s.tx, s.beginErr
}

// releaseDetailQuerierStub 是 releaseDetailQuerier 的替身：按 SQL 常量分派三条读语句，
// 记录调用顺序与参数，用来断言「拼装 stats 不额外发聚合」。
type releaseDetailQuerierStub struct {
	head     pgx.Row
	week     pgx.Rows
	weekErr  error
	building pgx.Rows
	bldErr   error

	calls    []string // 调用顺序：head / week / building
	headArgs []any    // 头条参数
	weekArgs []any    // 逐周参数
	bldArgs  []any    // 按楼参数
}

// QueryRow 只服务头条。
func (s *releaseDetailQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if sql != releaseDetailHeadSQL {
		panic("未知的单行读语句")
	}
	s.calls = append(s.calls, "head")
	s.headArgs = args
	return s.head
}

// Query 服务逐周与按楼两条。
func (s *releaseDetailQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch sql {
	case releaseWeekCountsSQL:
		s.calls = append(s.calls, "week")
		s.weekArgs = args
		return s.week, s.weekErr
	case releaseBuildingCountsSQL:
		s.calls = append(s.calls, "building")
		s.bldArgs = args
		return s.building, s.bldErr
	default:
		panic("未知的多行读语句")
	}
}

// releaseAdminTxStub 是 pgx.Tx 的替身：事务体内的读转发给查询替身，Rollback 由
// Detail 的 defer 调用（只读事务也靠它结束快照并归还连接），其余方法嵌入接口、
// 调用即 panic，避免替身放行被测路径之外的行为。
type releaseAdminTxStub struct {
	pgx.Tx
	querier    *releaseDetailQuerierStub
	rolledBack bool // Rollback 是否被调用：漏掉收尾会让连接一直占着事务
}

// Query 转发给查询替身，按 SQL 常量分派逐周与按楼两条。
func (s *releaseAdminTxStub) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.querier.Query(ctx, sql, args...)
}

// QueryRow 转发给查询替身，服务头条。
func (s *releaseAdminTxStub) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.querier.QueryRow(ctx, sql, args...)
}

// Rollback 记下收尾调用；只读事务不需要提交，回滚与提交等价。
func (s *releaseAdminTxStub) Rollback(context.Context) error {
	s.rolledBack = true
	return nil
}

// releaseAdminTestNow 是单元用例统一使用的固定时刻，避免 time.Now 让断言漂移。
func releaseAdminTestNow() time.Time {
	return time.Date(2026, 11, 14, 3, 5, 12, 0, time.UTC)
}

// releaseSummaryFixture 是一个「所有列都有值」的版本摘要。
//
// 相邻的同类型列必须取不同的值：dict_version / axis_version、两个布尔、
// observation_rows / week_count / rooms 一旦取值相同，列序写反后扫出来的摘要仍与
// 期望逐字段相等，这条断言就再也测不出列顺序。
func releaseSummaryFixture() ReleaseSummary {
	releasedAt := releaseAdminTestNow().Add(88 * time.Second)
	return ReleaseSummary{
		ReleaseID:       "20261114T030512Z",
		Term:            "2026-2027-1",
		GeneratedAt:     releaseAdminTestNow(),
		ReleasedAt:      &releasedAt,
		IsCurrent:       true,
		HasAttrConflict: false,
		WeekCount:       4,
		ObservationRows: 871320,
		Rooms:           2481,
		DictVersion:     1,
		AxisVersion:     2,
	}
}

// releaseSummaryRow 把摘要折成一行的值序列，顺序与 releaseListSQL 的列序一致。
func releaseSummaryRow(summary ReleaseSummary) []any {
	var releasedAt any
	if summary.ReleasedAt != nil {
		releasedAt = *summary.ReleasedAt
	}
	return []any{
		summary.ReleaseID,
		summary.Term,
		summary.GeneratedAt,
		summary.DictVersion,
		summary.AxisVersion,
		summary.HasAttrConflict,
		summary.IsCurrent,
		releasedAt,
		summary.ObservationRows,
		summary.WeekCount,
		summary.Rooms,
	}
}

// TestReleaseListSQLClauses 锁定列表 SQL 的口径：排序键、released_at 的左连接、
// 三个 LATERAL 聚合，以及「列表不分页」。
func TestReleaseListSQLClauses(t *testing.T) {
	clauses := []string{
		"ORDER BY r.generated_at DESC, r.release_id DESC", // Requirement 1.1
		"max(cr.released_at)",                             // 一版至多一行发布时刻
		"count(DISTINCT o.week)",                          // week_count 是「有观测的周数」
		"COALESCE(obs.observation_rows, 0)",
		"COALESCE(rooms.rooms, 0)",
	}
	for _, clause := range clauses {
		if !strings.Contains(releaseListSQL, clause) {
			t.Fatalf("列表 SQL 缺少 %q:\n%s", clause, releaseListSQL)
		}
	}
	for _, table := range []string{
		"FROM collect_run cr",
		"FROM observation o",
		"FROM release_room_building b",
	} {
		if !strings.Contains(releaseListSQL, table) {
			t.Fatalf("列表 SQL 缺少 %q:\n%s", table, releaseListSQL)
		}
	}
	if got := strings.Count(releaseListSQL, "LEFT JOIN LATERAL"); got != 3 {
		t.Fatalf("released_at 与两组计数共三个 LATERAL 聚合，实际 %d 个:\n%s", got, releaseListSQL)
	}
	// 列表是「全部在库版本」：release 上限两行，加 LIMIT/OFFSET 只会让 total 与
	// 真实行数脱节。
	if strings.Contains(releaseListSQL, "LIMIT") || strings.Contains(releaseListSQL, "OFFSET") {
		t.Fatalf("列表不分页，不应带 LIMIT/OFFSET:\n%s", releaseListSQL)
	}
}

// TestReleaseDetailSQLClauses 锁定详情三条语句的口径：头条不碰 observation、
// 逐周计数用行构造去重、两条明细各自固定升序。
func TestReleaseDetailSQLClauses(t *testing.T) {
	if strings.Contains(releaseDetailHeadSQL, "observation") {
		t.Fatalf("头条不应带观测聚合（逐周计数已经算过）:\n%s", releaseDetailHeadSQL)
	}
	weekClauses := []string{
		"count(DISTINCT (o.room_id, o.weekday, an.block))", // 同一格的小节只算一个格
		"an.axis_version = $2",                             // 用该版自己的轴版本折叠
		"GROUP BY o.week",
		"ORDER BY o.week", // Requirement 2.3：按周次升序
	}
	for _, clause := range weekClauses {
		if !strings.Contains(releaseWeekCountsSQL, clause) {
			t.Fatalf("逐周 SQL 缺少 %q:\n%s", clause, releaseWeekCountsSQL)
		}
	}
	buildingClauses := []string{
		"GROUP BY b.building_id, b.building_name",
		"ORDER BY b.building_id", // Requirement 2.3：按楼编号升序
	}
	for _, clause := range buildingClauses {
		if !strings.Contains(releaseBuildingCountsSQL, clause) {
			t.Fatalf("按楼 SQL 缺少 %q:\n%s", clause, releaseBuildingCountsSQL)
		}
	}
	// 三条语句各管一段，不能是同一条语句的别名（曾出现过常量贴错的低级错误）。
	if releaseDetailHeadSQL == releaseWeekCountsSQL ||
		releaseWeekCountsSQL == releaseBuildingCountsSQL {
		t.Fatal("详情三条语句不能复用同一段 SQL")
	}
}

// TestReleaseAdminRepoListScansAllColumns 覆盖列表的列序与可空列：
// released_at 为 nil 时扫成空指针，有值时扫成该时刻。
func TestReleaseAdminRepoListScansAllColumns(t *testing.T) {
	first := releaseSummaryFixture()
	second := releaseSummaryFixture()
	second.ReleaseID = "20261028T041500Z"
	second.GeneratedAt = releaseAdminTestNow().Add(-17 * 24 * time.Hour)
	second.ReleasedAt = nil // 没有关联轮次的版本
	second.IsCurrent = false
	second.HasAttrConflict = true

	rows := &releaseAdminRowsStub{
		rows: [][]any{
			releaseSummaryRow(first),
			releaseSummaryRow(second),
		},
	}
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{rows: rows})

	summaries, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("读版本列表失败: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("期望 2 个版本，实际 %d 个: %+v", len(summaries), summaries)
	}
	if !reflect.DeepEqual(summaries[0], first) {
		t.Fatalf("首个版本扫描不符: got %+v want %+v", summaries[0], first)
	}
	if summaries[1].ReleasedAt != nil {
		t.Fatalf("没有关联轮次的版本 released_at 应为 nil，实际 %v", summaries[1].ReleasedAt)
	}
	if summaries[1].HasAttrConflict != true || summaries[1].IsCurrent != false {
		t.Fatalf("布尔列扫描不符: %+v", summaries[1])
	}
	if !rows.closed {
		t.Fatal("列表读完必须关闭行集")
	}
}

// TestReleaseAdminRepoListEmptyIsEmptySlice 覆盖空库：返回非 nil 的空切片，
// 调用方据此给 total=0（Requirement 1.4）。
func TestReleaseAdminRepoListEmptyIsEmptySlice(t *testing.T) {
	rows := &releaseAdminRowsStub{}
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{rows: rows})

	summaries, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("空库读版本列表失败: %v", err)
	}
	if summaries == nil {
		t.Fatal("空库应返回空切片而不是 nil")
	}
	if len(summaries) != 0 {
		t.Fatalf("空库不应有版本: %+v", summaries)
	}
	if !rows.closed {
		t.Fatal("空列表也要关闭行集")
	}
}

// TestReleaseAdminRepoListQueryError 覆盖读库失败：包 model.ErrQueryFailed 且不返回半份列表。
func TestReleaseAdminRepoListQueryError(t *testing.T) {
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{queryErr: errors.New("连接断了")})

	summaries, err := repo.List(context.Background())
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if summaries != nil {
		t.Fatalf("失败时不应返回列表: %+v", summaries)
	}
}

// TestReleaseAdminRepoListRowsError 覆盖遍历到一半失败：已取到的行是残的，整体报错。
func TestReleaseAdminRepoListRowsError(t *testing.T) {
	rows := &releaseAdminRowsStub{
		rows: [][]any{releaseSummaryRow(releaseSummaryFixture())},
		err:  errors.New("遍历中连接断了"),
	}
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{rows: rows})

	summaries, err := repo.List(context.Background())
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if summaries != nil {
		t.Fatalf("失败时不应返回半份列表: %+v", summaries)
	}
	if !rows.closed {
		t.Fatal("失败路径也要关闭行集")
	}
}

// TestReleaseAdminRepoDetailBeginTxError 覆盖事务开不起来：包 model.ErrQueryFailed。
func TestReleaseAdminRepoDetailBeginTxError(t *testing.T) {
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{beginErr: errors.New("连接池耗尽")})

	detail, found, err := repo.Detail(context.Background(), "20261114T030512Z")
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if found {
		t.Fatal("事务开不起来时不应报告找到版本")
	}
	if !reflect.DeepEqual(detail, ReleaseDetailView{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", detail)
	}
}

// TestReleaseAdminRepoDetailOpensRepeatableReadReadOnlyTx 覆盖 Detail 开事务时的选项：
// REPEATABLE READ 让三条语句看同一快照（CP7），READ ONLY 让误写直接失败（CP2）。
// 三条语句必须都走这个事务，且结束后 Rollback 收尾。
func TestReleaseAdminRepoDetailOpensRepeatableReadReadOnlyTx(t *testing.T) {
	releasedAt := releaseAdminTestNow().Add(88 * time.Second)
	querier := &releaseDetailQuerierStub{
		head: &releaseAdminRowStub{values: []any{
			"20261114T030512Z", "2026-2027-1", releaseAdminTestNow(),
			1, 2, false, true, releasedAt, 3,
		}},
		week:     &releaseAdminRowsStub{rows: [][]any{{int16(9), int64(7), 3}}},
		building: &releaseAdminRowsStub{rows: [][]any{{"Q10", "综合教学楼", 3}}},
	}
	tx := &releaseAdminTxStub{querier: querier}
	pool := &releaseAdminPoolStub{tx: tx}
	repo := NewReleaseAdminRepo(pool)

	detail, found, err := repo.Detail(context.Background(), "20261114T030512Z")
	if err != nil {
		t.Fatalf("读版本详情失败: %v", err)
	}
	if !found || detail.Release.ReleaseID != "20261114T030512Z" {
		t.Fatalf("详情应经事务体返回: found=%v detail=%+v", found, detail)
	}
	if fmt.Sprint(querier.calls) != fmt.Sprint([]string{"head", "week", "building"}) {
		t.Fatalf("三条语句都应走同一事务，实际调用 %v", querier.calls)
	}
	if pool.txOptions.IsoLevel != pgx.RepeatableRead {
		t.Fatalf("详情事务隔离级别应为 REPEATABLE READ，实际 %v", pool.txOptions.IsoLevel)
	}
	if pool.txOptions.AccessMode != pgx.ReadOnly {
		t.Fatalf("详情事务应为 READ ONLY，实际 %v", pool.txOptions.AccessMode)
	}
	if !tx.rolledBack {
		t.Fatal("详情事务结束后必须 Rollback 收尾")
	}
}

// TestLoadReleaseDetailComposesStatsFromWeekCounts 覆盖详情的事务体：
// 三条语句（头条 → 逐周 → 按楼）各发一次，stats 与版本头的计数全部由逐周结果拼出，
// 逐周语句用的是头条读回的 axis_version。
func TestLoadReleaseDetailComposesStatsFromWeekCounts(t *testing.T) {
	releasedAt := releaseAdminTestNow().Add(88 * time.Second)
	querier := &releaseDetailQuerierStub{
		head: &releaseAdminRowStub{values: []any{
			"20261114T030512Z", "2026-2027-1", releaseAdminTestNow(),
			1, 2, false, true, releasedAt, 3,
		}},
		week: &releaseAdminRowsStub{rows: [][]any{
			{int16(9), int64(7), 3},
			{int16(10), int64(2), 1},
		}},
		building: &releaseAdminRowsStub{rows: [][]any{
			{"Q03", "老文史楼", 1},
			{"Q10", "综合教学楼", 2},
		}},
	}

	detail, found, err := loadReleaseDetail(context.Background(), querier, "20261114T030512Z")
	if err != nil {
		t.Fatalf("读版本详情失败: %v", err)
	}
	if !found {
		t.Fatal("夹具版本应当找得到")
	}

	// 三条语句、顺序固定；没有第四条聚合语句。
	wantCalls := []string{"head", "week", "building"}
	if fmt.Sprint(querier.calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("查询顺序应为 %v，实际 %v", wantCalls, querier.calls)
	}
	if len(querier.headArgs) != 1 || querier.headArgs[0] != "20261114T030512Z" {
		t.Fatalf("头条参数不符: %v", querier.headArgs)
	}
	if len(querier.weekArgs) != 2 ||
		querier.weekArgs[0] != "20261114T030512Z" || querier.weekArgs[1] != 2 {
		t.Fatalf("逐周语句必须带版本与该版自己的 axis_version: %v", querier.weekArgs)
	}

	wantStats := ReleaseStats{
		ObservationRows: 9, // 7 + 2
		BlockCells:      4, // 3 + 1
		WeekCount:       2,
		Rooms:           3,
		Buildings:       2,
	}
	if detail.Stats != wantStats {
		t.Fatalf("stats 应由逐周结果拼出: got %+v want %+v", detail.Stats, wantStats)
	}
	// 版本头里的两个计数也来自逐周结果，不是再查一次库。
	if detail.Release.ObservationRows != wantStats.ObservationRows ||
		detail.Release.WeekCount != wantStats.WeekCount {
		t.Fatalf("版本头计数未回填: %+v", detail.Release)
	}
	if detail.Release.Rooms != 3 || detail.Release.AxisVersion != 2 {
		t.Fatalf("版本头字段不符: %+v", detail.Release)
	}
	if detail.Release.ReleasedAt == nil || !detail.Release.ReleasedAt.Equal(releasedAt) {
		t.Fatalf("released_at 扫描不符: %v", detail.Release.ReleasedAt)
	}

	wantWeeks := []ReleaseWeekCount{
		{Week: 9, ObservationRows: 7, BlockCells: 3},
		{Week: 10, ObservationRows: 2, BlockCells: 1},
	}
	if fmt.Sprint(detail.WeekCounts) != fmt.Sprint(wantWeeks) {
		t.Fatalf("逐周计数不符: got %+v want %+v", detail.WeekCounts, wantWeeks)
	}
	wantBuildings := []ReleaseBuildingCount{
		{BuildingID: "Q03", BuildingName: "老文史楼", Rooms: 1},
		{BuildingID: "Q10", BuildingName: "综合教学楼", Rooms: 2},
	}
	if fmt.Sprint(detail.BuildingCounts) != fmt.Sprint(wantBuildings) {
		t.Fatalf("按楼计数不符: got %+v want %+v", detail.BuildingCounts, wantBuildings)
	}
}

// TestLoadReleaseDetailEmptySlicesAreNonNil 覆盖空版本：没有观测与归属快照时，
// WeekCounts / BuildingCounts 恒为非 nil 空切片（JSON 输出 []）。
func TestLoadReleaseDetailEmptySlicesAreNonNil(t *testing.T) {
	querier := &releaseDetailQuerierStub{
		head: &releaseAdminRowStub{values: []any{
			"20260101T000000Z", "2026-2027-1", releaseAdminTestNow(),
			1, 1, false, false, nil, 0,
		}},
		week:     &releaseAdminRowsStub{},
		building: &releaseAdminRowsStub{},
	}

	detail, found, err := loadReleaseDetail(context.Background(), querier, "20260101T000000Z")
	if err != nil {
		t.Fatalf("读空版本详情失败: %v", err)
	}
	if !found {
		t.Fatal("空版本仍应报告找到")
	}
	if detail.WeekCounts == nil || detail.BuildingCounts == nil {
		t.Fatalf("两个切片必须非 nil: weeks=%v buildings=%v", detail.WeekCounts, detail.BuildingCounts)
	}
	if len(detail.WeekCounts) != 0 || len(detail.BuildingCounts) != 0 {
		t.Fatalf("空版本不应有计数: %+v", detail)
	}
	wantStats := ReleaseStats{}
	if detail.Stats != wantStats {
		t.Fatalf("空版本 stats 应全零: %+v", detail.Stats)
	}
	if detail.Release.ReleasedAt != nil {
		t.Fatalf("没有关联轮次时 released_at 应为 nil: %v", detail.Release.ReleasedAt)
	}
}

// TestLoadReleaseDetailNotFound 覆盖版本不存在：返回 (零值, false, nil)，
// 且不再发逐周/按楼两条语句。
func TestLoadReleaseDetailNotFound(t *testing.T) {
	querier := &releaseDetailQuerierStub{
		head: &releaseAdminRowStub{err: pgx.ErrNoRows},
	}

	detail, found, err := loadReleaseDetail(context.Background(), querier, "20260101T000000Z")
	if err != nil {
		t.Fatalf("版本不存在不是错误，实际 err=%v", err)
	}
	if found {
		t.Fatal("版本不存在时 found 应为 false")
	}
	if !reflect.DeepEqual(detail, ReleaseDetailView{}) {
		t.Fatalf("版本不存在时返回零值，实际 %+v", detail)
	}
	if fmt.Sprint(querier.calls) != fmt.Sprint([]string{"head"}) {
		t.Fatalf("头条缺失后不应再发其它语句，实际调用 %v", querier.calls)
	}
}

// TestLoadReleaseDetailScanErrorWraps 覆盖逐周扫描失败：包装 model.ErrQueryFailed
// 且不返回半份详情。
func TestLoadReleaseDetailScanErrorWraps(t *testing.T) {
	weekRows := &releaseAdminRowsStub{
		rows:    [][]any{{int16(9), int64(7), 3}},
		scanErr: errors.New("列不可解"),
	}
	querier := &releaseDetailQuerierStub{
		head: &releaseAdminRowStub{values: []any{
			"20261114T030512Z", "2026-2027-1", releaseAdminTestNow(),
			1, 1, false, true, nil, 3,
		}},
		week:     weekRows,
		building: &releaseAdminRowsStub{},
	}

	detail, found, err := loadReleaseDetail(context.Background(), querier, "20261114T030512Z")
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if found {
		t.Fatal("失败时不应报告找到版本")
	}
	if !reflect.DeepEqual(detail, ReleaseDetailView{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", detail)
	}
	if !weekRows.closed {
		t.Fatal("失败路径也要关闭行集")
	}
}
