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
	"github.com/jackc/pgx/v5/pgconn"

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
	case **string:
		// 可空字符串列（分页明细的 from_state / to_state / 归属字段）：nil 落成 nil
		// 指针，非 nil 分配一个新指针，断言时用 reflect.DeepEqual 比较指向的值。
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(string)
		if !ok {
			return fmt.Errorf("期望 string 或 nil，实际 %T", value)
		}
		*d = &v
	case **int16:
		// 可空 smallint 列（异常视图的窗口周次）：nil 落成 nil 指针。
		if value == nil {
			*d = nil
			return nil
		}
		v, ok := value.(int16)
		if !ok {
			return fmt.Errorf("期望 int16 或 nil，实际 %T", value)
		}
		*d = &v
	case *[]int32:
		// int4[] 列（异常视图的 observed_weeks）：空数组落成空切片而不是 nil，
		// 与 pgx 扫空数组的行为一致。
		v, ok := value.([]int32)
		if !ok {
			return fmt.Errorf("期望 []int32，实际 %T", value)
		}
		*d = v
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

// releaseDiffQuerierStub 是 releaseDiffQuerier 的替身：按 SQL 常量分派「两侧版本头」、
// 「汇总」与「三类分页」读，并记下调用顺序与参数。
//
// heads 按 release_id 回放：键不存在时回 pgx.ErrNoRows，这样「缺一版」走的是与真实库
// 完全相同的那条分支，不需要在测试里伪造一个特例。
//
// page* 只保留最后一次分页调用：一个用例只会命中一个小节，区分「哪个 SQL 被调用」
// 比记录一串调用更有用（同时被调用两个小节就是 bug）。三类 rows 为 nil 时回放空行集，
// 这样只断言汇总的老用例不必为分页再铺一份数据。
type releaseDiffQuerierStub struct {
	heads   map[string]pgx.Row // 按 release_id 回放版本头；缺键即「这一版不存在」
	summary pgx.Row            // 汇总单行

	cells     pgx.Rows // section=cells 的一页
	cellsErr  error
	rooms     pgx.Rows // section=rooms 的一页
	roomsErr  error
	buildings pgx.Rows // section=buildings 的一页
	bldErr    error

	headIDs     []string // 依次收到的版本头参数，用来断言 from 先于 to
	summaryArgs []any    // 汇总语句收到的参数，顺序为 (from 轴版本, from 版, to 轴版本, to 版)
	summaryRead bool     // 汇总是否被读过：缺一版时它必须一次都不被读

	pageSQL  string // 最后一次分页调用的 SQL 常量值
	pageArgs []any  // 最后一次分页调用的参数
	pageRead bool   // 分页是否被读过：缺一版或汇总失败时它必须一次都不被读
}

// QueryRow 按 SQL 常量分派：两侧头与汇总都是单行读。
func (s *releaseDiffQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch sql {
	case releaseDiffHeadSQL:
		id, _ := args[0].(string)
		s.headIDs = append(s.headIDs, id)
		head, ok := s.heads[id]
		if !ok {
			return &releaseAdminRowStub{err: pgx.ErrNoRows}
		}
		return head
	case releaseDiffSummarySQL:
		s.summaryArgs = append([]any(nil), args...)
		s.summaryRead = true
		return s.summary
	default:
		panic("未知的单行读语句")
	}
}

// Query 按 SQL 常量分派三类分页；记录最后一次的 SQL 与参数。
func (s *releaseDiffQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.pageSQL = sql
	s.pageArgs = append([]any(nil), args...)
	s.pageRead = true
	switch sql {
	case releaseDiffCellsPageSQL:
		if s.cells == nil {
			return &releaseAdminRowsStub{}, s.cellsErr
		}
		return s.cells, s.cellsErr
	case releaseDiffRoomsPageSQL:
		if s.rooms == nil {
			return &releaseAdminRowsStub{}, s.roomsErr
		}
		return s.rooms, s.roomsErr
	case releaseDiffBuildingsPageSQL:
		if s.buildings == nil {
			return &releaseAdminRowsStub{}, s.bldErr
		}
		return s.buildings, s.bldErr
	default:
		panic("未知的多行读语句")
	}
}

// releaseDiffTxStub 是 pgx.Tx 的替身：事务体内的读转发给查询替身，Rollback 由
// Diff 的 defer 调用（只读事务也靠它结束快照并归还连接），其余方法嵌入接口、
// 调用即 panic，避免替身放行被测路径之外的行为。
type releaseDiffTxStub struct {
	pgx.Tx
	querier    *releaseDiffQuerierStub
	rolledBack bool // Rollback 是否被调用：漏掉收尾会让连接一直占着事务
}

// QueryRow 转发给查询替身，服务两侧头与汇总。
func (s *releaseDiffTxStub) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return s.querier.QueryRow(ctx, sql, args...)
}

// Query 转发给查询替身，服务三类分页。
func (s *releaseDiffTxStub) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.querier.Query(ctx, sql, args...)
}

// Rollback 记下收尾调用；只读事务不需要提交，回滚与提交等价。
func (s *releaseDiffTxStub) Rollback(context.Context) error {
	s.rolledBack = true
	return nil
}

// releaseAnomalyQuerierStub 是 releaseAnomalyQuerier 的替身：按 SQL 常量分派两条
// 多行读，记录调用顺序，用来断言「版本折叠先于冲突清单」与错误口径。
type releaseAnomalyQuerierStub struct {
	anomalies    pgx.Rows // 版本折叠的行集
	anomaliesErr error
	conflicts    pgx.Rows // 冲突清单的行集
	conflictsErr error

	calls []string // 调用顺序：anomalies / conflicts
}

// Query 按 SQL 常量分派；没预设行集时回放一个空行集，保持「成功但零行」。
func (s *releaseAnomalyQuerierStub) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch sql {
	case releaseAnomaliesSQL:
		s.calls = append(s.calls, "anomalies")
		if s.anomalies == nil {
			return &releaseAdminRowsStub{}, s.anomaliesErr
		}
		return s.anomalies, s.anomaliesErr
	case releaseAnomalyConflictRoomsSQL:
		s.calls = append(s.calls, "conflicts")
		if s.conflicts == nil {
			return &releaseAdminRowsStub{}, s.conflictsErr
		}
		return s.conflicts, s.conflictsErr
	default:
		panic("未知的异常视图语句")
	}
}

// releaseAnomalyTxStub 是 pgx.Tx 的替身：事务体内的读转发给查询替身，Rollback 由
// Anomalies 的 defer 调用（只读事务也靠它结束快照并归还连接），其余方法嵌入接口、
// 调用即 panic，避免替身放行被测路径之外的行为。
type releaseAnomalyTxStub struct {
	pgx.Tx
	querier    *releaseAnomalyQuerierStub
	rolledBack bool // Rollback 是否被调用：漏掉收尾会让连接一直占着事务
}

// Query 转发给查询替身，服务版本折叠与冲突清单两条语句。
func (s *releaseAnomalyTxStub) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.querier.Query(ctx, sql, args...)
}

// Rollback 记下收尾调用；只读事务不需要提交，回滚与提交等价。
func (s *releaseAnomalyTxStub) Rollback(context.Context) error {
	s.rolledBack = true
	return nil
}

// releaseAnomalyRow 是异常视图一行的值序列，列序与 releaseAnomaliesSQL 一致。
//
// windowStart / windowEnd 传 nil 表示该版没有关联轮次（两列都为 NULL）；传指针表示
// 窗口周次。term 固定成一个值：用例关心的是每行自己的列序与派生量，再让学期随行
// 变化只会让断言更难读。
func releaseAnomalyRow(
	releaseID string,
	generatedAt time.Time,
	isCurrent bool,
	hasAttrConflict bool,
	observationRows int64,
	blockCells int,
	unknownCells int,
	compositeCells int,
	observedWeeks []int32,
	windowStart *int16,
	windowEnd *int16,
) []any {
	var start, end any
	if windowStart != nil {
		start = *windowStart
	}
	if windowEnd != nil {
		end = *windowEnd
	}
	return []any{
		releaseID, "2026-2027-1", generatedAt, isCurrent, hasAttrConflict,
		observationRows, blockCells, unknownCells, compositeCells,
		observedWeeks,
		start, end,
	}
}

// releaseAnomalyConflictRow 是冲突清单一行：release_id、房号、采纳楼、冲突楼、序号。
func releaseAnomalyConflictRow(releaseID, roomID, buildingID, conflictBuildingID string, rank int64) []any {
	return []any{releaseID, roomID, buildingID, conflictBuildingID, rank}
}

// releaseDiffFromSide / releaseDiffToSide 是两侧版本头的夹具。
//
// 两侧的每个同类型字段都取不同值（学期、字典版本、轴版本、两个布尔、时刻）：
// 一旦列序或某一侧被写反，逐字段相等断言就会失败——取同值能让「两侧串了」
// 照样通过。
func releaseDiffFromSide() ReleaseSide {
	return ReleaseSide{
		ReleaseID:       "20261028T041500Z",
		Term:            "2026-2027-1",
		GeneratedAt:     releaseAdminTestNow().Add(-17 * 24 * time.Hour),
		DictVersion:     1,
		AxisVersion:     2,
		IsCurrent:       false,
		HasAttrConflict: true,
	}
}

// releaseDiffToSide 是 to 侧版本头夹具。
func releaseDiffToSide() ReleaseSide {
	return ReleaseSide{
		ReleaseID:       "20261114T030512Z",
		Term:            "2026-2027-1",
		GeneratedAt:     releaseAdminTestNow(),
		DictVersion:     3,
		AxisVersion:     4,
		IsCurrent:       true,
		HasAttrConflict: false,
	}
}

// releaseSideRow 把一侧版本头折成一行，列序与 releaseDiffHeadSQL 一致。
func releaseSideRow(side ReleaseSide) []any {
	return []any{
		side.ReleaseID,
		side.Term,
		side.GeneratedAt,
		side.DictVersion,
		side.AxisVersion,
		side.IsCurrent,
		side.HasAttrConflict,
	}
}

// releaseDiffSummaryRow 是汇总语句一行的值序列，列序与 releaseDiffSummarySQL 一致。
func releaseDiffSummaryRow(
	added int, removed int, changed int, inconsistent int,
	fromUnknown int, fromComposite int, toUnknown int, toComposite int,
	roomAdded int, roomRemoved int, roomShared int, roomBuildingChanged int,
) []any {
	return []any{
		added, removed, changed, inconsistent,
		fromUnknown, fromComposite, toUnknown, toComposite,
		roomAdded, roomRemoved, roomShared, roomBuildingChanged,
	}
}

// TestReleaseDiffSQLClauses 锁定折叠 CTE 与汇总语句的口径：
// 两侧各按自己的 axis_version 折叠、键是四元组、min/max 判块内一致、汇总一条语句、
// 未知/复合按格计数、归属变化只在两侧都存在时才算。
func TestReleaseDiffSQLClauses(t *testing.T) {
	foldClauses := []string{
		// 两侧各用自己版本的轴折叠：轴改版后同一个 node 可能落到不同 block。
		"JOIN axis_node an ON an.axis_version = $1 AND an.node = o.node",
		"JOIN axis_node an ON an.axis_version = $3 AND an.node = o.node",
		"WHERE o.release_id = $2",
		"WHERE o.release_id = $4",
		// 折叠键是 (room_id, week, weekday, block)，min 决定格状态、max 判块内一致。
		"min(o.state_key) AS state_key",
		"max(o.state_key) AS max_state",
		"GROUP BY o.room_id, o.week, o.weekday, an.block",
		"FULL JOIN t USING (room_id, week, weekday, block)",
		// 两侧任一出现块内多状态都算不一致（不是只看一侧）。
		"f.max_state <> f.state_key",
		"t.max_state <> t.state_key",
	}
	for _, clause := range foldClauses {
		if !strings.Contains(releaseDiffFoldCTE, clause) {
			t.Fatalf("折叠 CTE 缺少 %q:\n%s", clause, releaseDiffFoldCTE)
		}
	}
	if got := strings.Count(releaseDiffFoldCTE, "GROUP BY o.room_id, o.week, o.weekday, an.block"); got != 2 {
		t.Fatalf("两侧各折叠一次，GROUP BY 应出现 2 次，实际 %d 次:\n%s", got, releaseDiffFoldCTE)
	}

	// 汇总必须复用同一段折叠，否则汇总与明细会在两套口径上各算一遍。
	if !strings.HasPrefix(releaseDiffSummarySQL, releaseDiffFoldCTE) {
		t.Fatal("汇总语句必须以同一段折叠 CTE 开头")
	}
	summaryClauses := []string{
		"count(*) FILTER (WHERE kind = 'added')",
		"count(*) FILTER (WHERE kind = 'removed')",
		"count(*) FILTER (WHERE kind = 'changed')",
		"count(*) FILTER (WHERE inconsistent)",
		"state_key = 'unknown'",
		"state_key = 'composite'",
		"UNION ALL", // 两侧状态只扫一遍
		"FROM rf FULL JOIN rt USING (room_id)",
		"rf.building_id <> rt.building_id",
		"FROM cells CROSS JOIN states CROSS JOIN rooms",
	}
	for _, clause := range summaryClauses {
		if !strings.Contains(releaseDiffSummarySQL, clause) {
			t.Fatalf("汇总 SQL 缺少 %q:\n%s", clause, releaseDiffSummarySQL)
		}
	}
	// 两侧各只折叠一次：折叠是最重的一段，重复出现说明又扫了一遍 87 万行。
	if got := strings.Count(releaseDiffSummarySQL, "FROM observation o"); got != 2 {
		t.Fatalf("两侧各折叠一次，观测扫描应出现 2 次，实际 %d 次:\n%s", got, releaseDiffSummarySQL)
	}
	// 汇总是一条语句（供 QueryRow 单行读），不带分页（分页属第 3 项）。
	if strings.Contains(releaseDiffSummarySQL, ";") {
		t.Fatalf("汇总必须是单条语句，不能带分号:\n%s", releaseDiffSummarySQL)
	}
	if got := strings.Count(releaseDiffSummarySQL, "\nSELECT "); got != 1 {
		t.Fatalf("汇总只应有一个顶层 SELECT，实际 %d 个:\n%s", got, releaseDiffSummarySQL)
	}
	if strings.Contains(releaseDiffSummarySQL, "LIMIT") || strings.Contains(releaseDiffSummarySQL, "OFFSET") {
		t.Fatalf("汇总不分页，不应带 LIMIT/OFFSET:\n%s", releaseDiffSummarySQL)
	}
	// same_term 是版本头的事实，由 Go 侧比较两个 term，不在汇总 SQL 里判。
	if strings.Contains(releaseDiffSummarySQL, "same_term") {
		t.Fatalf("same_term 不应出现在汇总 SQL 里:\n%s", releaseDiffSummarySQL)
	}
}

// TestReleaseAdminRepoDiffOpensRepeatableReadReadOnlyTx 覆盖对比的事务外观：
// REPEATABLE READ 让两侧头、汇总与分页明细看同一快照（CP7），READ ONLY 让误写直接
// 失败（CP2）；顺序固定为「from 头 → to 头 → 汇总 → 分页」，汇总参数取自读回的头
// 而不是外部入参，分页也走同一个事务（而不是回到连接池另开一次读），结束后 Rollback 收尾。
func TestReleaseAdminRepoDiffOpensRepeatableReadReadOnlyTx(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffSummaryRow(
			1, 2, 3, 1, 4, 5, 6, 7, 8, 9, 10, 11)},
	}
	tx := &releaseDiffTxStub{querier: querier}
	pool := &releaseAdminPoolStub{tx: tx}
	repo := NewReleaseAdminRepo(pool)

	diff, err := repo.Diff(context.Background(), ReleaseDiffParams{
		From: from.ReleaseID,
		To:   to.ReleaseID,
	})
	if err != nil {
		t.Fatalf("对比两版失败: %v", err)
	}
	if !reflect.DeepEqual(diff.From, from) || !reflect.DeepEqual(diff.To, to) {
		t.Fatalf("两侧头扫描不符: got %+v / %+v want %+v / %+v", diff.From, diff.To, from, to)
	}
	if fmt.Sprint(querier.headIDs) != fmt.Sprint([]string{from.ReleaseID, to.ReleaseID}) {
		t.Fatalf("必须先读 from 头、再读 to 头，实际 %v", querier.headIDs)
	}

	wantArgs := []any{from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID}
	if fmt.Sprint(querier.summaryArgs) != fmt.Sprint(wantArgs) {
		t.Fatalf("汇总参数应取自两侧头（轴版本, 版本, 轴版本, 版本），got %v want %v",
			querier.summaryArgs, wantArgs)
	}
	// 分页明细也必须走同一个事务：回到连接池另开一次读就会换快照（CP7）。
	// 缺省参数按 cells/all/50/0 下推。
	if !querier.pageRead {
		t.Fatal("汇总之后必须读所选小节的分页")
	}
	if querier.pageSQL != releaseDiffCellsPageSQL {
		t.Fatalf("缺省 section 应读 cells 分页，实际 %q", querier.pageSQL)
	}
	wantPageArgs := []any{from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID, "all", 50, 0}
	if fmt.Sprint(querier.pageArgs) != fmt.Sprint(wantPageArgs) {
		t.Fatalf("分页参数不符: got %v want %v", querier.pageArgs, wantPageArgs)
	}

	if pool.txOptions.IsoLevel != pgx.RepeatableRead {
		t.Fatalf("对比事务隔离级别应为 REPEATABLE READ，实际 %v", pool.txOptions.IsoLevel)
	}
	if pool.txOptions.AccessMode != pgx.ReadOnly {
		t.Fatalf("对比事务应为 READ ONLY，实际 %v", pool.txOptions.AccessMode)
	}
	if !tx.rolledBack {
		t.Fatal("对比事务结束后必须 Rollback 收尾")
	}
}

// TestLoadReleaseDiffSummaryComposition 覆盖汇总的拼装：
// 12 列逐列落到对的位置、total 是三类之和、delta = to - from、same_term 由两个头比出。
func TestLoadReleaseDiffSummaryComposition(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffSummaryRow(
			1, 2, 3, 1, // cells: added / removed / changed / inconsistent_blocks
			4, 5, 6, 7, // unknown: from / to，composite: from / to
			8, 9, 10, 11, // rooms: added / removed / shared / building_changed
		)},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID,
		To:   to.ReleaseID,
	})
	if err != nil {
		t.Fatalf("对比失败: %v", err)
	}

	want := ReleaseDiffSummary{
		Cells:              DiffCellsSummary{Added: 1, Removed: 2, Changed: 3, Total: 6},
		Rooms:              DiffRoomsSummary{Added: 8, Removed: 9, Shared: 10, BuildingChanged: 11},
		Unknown:            StateDelta{From: 4, To: 6, Delta: 2},
		Composite:          StateDelta{From: 5, To: 7, Delta: 2},
		InconsistentBlocks: 1,
		SameTerm:           true,
	}
	if diff.Summary != want {
		t.Fatalf("汇总拼装不符:\n got %+v\nwant %+v", diff.Summary, want)
	}
	if !querier.summaryRead {
		t.Fatal("两侧头都读到后必须读汇总")
	}
}

// TestLoadReleaseDiffDifferentTermKeepsDiff 覆盖不同 term 的两版：same_term=false，
// 差异照常给出（Requirement 3.4），delta 由各侧计数算出。
func TestLoadReleaseDiffDifferentTermKeepsDiff(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	to.Term = "2025-2026-2" // 跨学期
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffSummaryRow(
			3, 0, 5, 0, 12, 51, 0, 48, 1, 2, 30, 4)},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID,
		To:   to.ReleaseID,
	})
	if err != nil {
		t.Fatalf("跨学期对比不应报错: %v", err)
	}
	if diff.Summary.SameTerm {
		t.Fatal("两侧 term 不同时 same_term 应为 false")
	}
	if diff.Summary.Cells != (DiffCellsSummary{Added: 3, Removed: 0, Changed: 5, Total: 8}) {
		t.Fatalf("跨学期也要照常给差异: %+v", diff.Summary.Cells)
	}
	if diff.Summary.Unknown != (StateDelta{From: 12, To: 0, Delta: -12}) {
		t.Fatalf("unknown 计数不符: %+v", diff.Summary.Unknown)
	}
	if diff.Summary.Composite != (StateDelta{From: 51, To: 48, Delta: -3}) {
		t.Fatalf("composite 计数不符: %+v", diff.Summary.Composite)
	}
}

// TestLoadReleaseDiffMissingSideReturnsNotFound 覆盖缺一版：分别点名 from / to，
// 返回包着 ErrReleaseNotFound 的错误、零值结果，并且不再读汇总（省一次全量折叠）。
func TestLoadReleaseDiffMissingSideReturnsNotFound(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()

	cases := []struct {
		name     string
		heads    map[string]pgx.Row
		params   ReleaseDiffParams
		wantSide string
		wantID   string
	}{
		{
			name: "缺 from",
			heads: map[string]pgx.Row{
				to.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(to)},
			},
			params:   ReleaseDiffParams{From: from.ReleaseID, To: to.ReleaseID},
			wantSide: "from",
			wantID:   from.ReleaseID,
		},
		{
			name: "缺 to",
			heads: map[string]pgx.Row{
				from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			},
			params:   ReleaseDiffParams{From: from.ReleaseID, To: to.ReleaseID},
			wantSide: "to",
			wantID:   to.ReleaseID,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			querier := &releaseDiffQuerierStub{heads: testCase.heads}
			diff, err := loadReleaseDiff(context.Background(), querier, testCase.params)
			if !errors.Is(err, ErrReleaseNotFound) {
				t.Fatalf("期望 ErrReleaseNotFound，实际 err=%v", err)
			}
			if !strings.Contains(err.Error(), testCase.wantSide) ||
				!strings.Contains(err.Error(), testCase.wantID) {
				t.Fatalf("错误消息应点名 %s 侧与版本 %s，实际 %q",
					testCase.wantSide, testCase.wantID, err)
			}
			if !reflect.DeepEqual(diff, ReleaseDiff{}) {
				t.Fatalf("缺一版时应返回零值，实际 %+v", diff)
			}
			if querier.summaryRead {
				t.Fatal("缺一版时不应再读汇总")
			}
		})
	}
}

// TestReleaseAdminRepoDiffBeginTxError 覆盖事务开不起来：包 model.ErrQueryFailed。
func TestReleaseAdminRepoDiffBeginTxError(t *testing.T) {
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{beginErr: errors.New("连接池耗尽")})

	diff, err := repo.Diff(context.Background(), ReleaseDiffParams{
		From: "20261028T041500Z",
		To:   "20261114T030512Z",
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
}

// TestReleaseAdminRepoDiffHeadQueryErrorWraps 覆盖读版本头时的库错误：
// 包 model.ErrQueryFailed，且不是 ErrReleaseNotFound（缺版本与读库故障不能混）。
func TestReleaseAdminRepoDiffHeadQueryErrorWraps(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{err: errors.New("连接断了")},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID,
		To:   to.ReleaseID,
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("读库故障不能报成版本不存在: %v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
	if querier.summaryRead {
		t.Fatal("头都读不出来时不应再读汇总")
	}
}

// TestReleaseAdminRepoDiffSummaryScanErrorWraps 覆盖汇总扫描失败：
// 包 model.ErrQueryFailed 且不返回半份汇总。
func TestReleaseAdminRepoDiffSummaryScanErrorWraps(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{err: errors.New("列不可解")},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID,
		To:   to.ReleaseID,
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
}

// releaseAdminPtr 返回字符串指针，用来构造可空字段的期望值；同包的
// releaseadmin_integration_test.go 也用它拼两版夹具的期望行。
//
// 不能直接对字面量取址：每次取址都是不同指针，断言必须比较指向的值而不是地址，
// 因此期望值也走 reflect.DeepEqual（它会递归比较指针指向的内容）。
func releaseAdminPtr(value string) *string { return &value }

// releaseDiffCellValues 是 cells 分页一行的值序列，列序与 releaseDiffCellsPageSQL 一致。
func releaseDiffCellValues(row DiffCellRow) []any {
	var fromState, toState, fromBuilding, toBuilding any
	if row.FromState != nil {
		fromState = *row.FromState
	}
	if row.ToState != nil {
		toState = *row.ToState
	}
	if row.FromBuilding != nil {
		fromBuilding = *row.FromBuilding
	}
	if row.ToBuilding != nil {
		toBuilding = *row.ToBuilding
	}
	return []any{
		string(row.Kind), row.RoomID, int16(row.Week), int16(row.Weekday), row.Block,
		fromState, toState, row.RoomName, fromBuilding, toBuilding,
	}
}

// releaseDiffRoomValues 是 rooms 分页一行的值序列，列序与 releaseDiffRoomsPageSQL 一致。
func releaseDiffRoomValues(row DiffRoomRow) []any {
	var fromBuilding, fromName, toBuilding, toName any
	if row.FromBuilding != nil {
		fromBuilding = *row.FromBuilding
	}
	if row.FromBuildingName != nil {
		fromName = *row.FromBuildingName
	}
	if row.ToBuilding != nil {
		toBuilding = *row.ToBuilding
	}
	if row.ToBuildingName != nil {
		toName = *row.ToBuildingName
	}
	return []any{
		string(row.Kind), row.RoomID, row.RoomName,
		fromBuilding, fromName, toBuilding, toName,
	}
}

// releaseDiffBuildingValues 是 buildings 分页一行的值序列，列序与
// releaseDiffBuildingsPageSQL 的 `SELECT p.*, room_name` 一致。
func releaseDiffBuildingValues(row DiffBuildingRow) []any {
	return []any{
		row.RoomID,
		row.FromBuilding, row.FromBuildingName,
		row.ToBuilding, row.ToBuildingName,
		row.RoomName,
	}
}

// releaseDiffPageSummaryRow 是一条合法的汇总单行，默认值供分页用例铺底。
func releaseDiffPageSummaryRow() []any {
	return releaseDiffSummaryRow(1, 2, 4, 2, 1, 0, 0, 1, 1, 2, 6, 1)
}

// TestReleaseDiffPageSQLClauses 锁定三节分页语句的共同口径：
// 固定排序键、LIMIT/OFFSET、分页语句不数行（total 由汇总推导，CP6）。
func TestReleaseDiffPageSQLClauses(t *testing.T) {
	// cells 复用折叠 CTE（与汇总同一口径），且只取差异行、kind 走参数等值过滤。
	if !strings.HasPrefix(releaseDiffCellsPageSQL, releaseDiffFoldCTE) {
		t.Fatal("cells 分页必须以同一段折叠 CTE 开头")
	}
	cellsClauses := []string{
		"kind <> 'same'",                         // 未变格不进明细
		"$5 = 'all'",                             // kind=all 不过滤
		"kind = $5",                              // 其余取值等值过滤
		"ORDER BY room_id, week, weekday, block", // 页内固定排序键
		"ORDER BY p.room_id, p.week, p.weekday, p.block",   // JOIN 后重排，翻页才稳定
		"LIMIT $6 OFFSET $7",                               // 页大小与偏移
		"COALESCE(r.name, '') AS room_name",                // 教室目录缺行时给空串
		"ON fb.release_id = $2 AND fb.room_id = p.room_id", // from 侧归属楼
		"ON tb.release_id = $4 AND tb.room_id = p.room_id", // to 侧归属楼
	}
	for _, clause := range cellsClauses {
		if !strings.Contains(releaseDiffCellsPageSQL, clause) {
			t.Fatalf("cells 分页缺少 %q:\n%s", clause, releaseDiffCellsPageSQL)
		}
	}

	roomsClauses := []string{
		"FROM f FULL JOIN t USING (room_id)",
		"$3 = 'added'   AND f.room_id IS NULL",
		"$3 = 'removed' AND t.room_id IS NULL",
		"ORDER BY room_id",
		"ORDER BY p.room_id",
		"LIMIT $4 OFFSET $5",
	}
	for _, clause := range roomsClauses {
		if !strings.Contains(releaseDiffRoomsPageSQL, clause) {
			t.Fatalf("rooms 分页缺少 %q:\n%s", clause, releaseDiffRoomsPageSQL)
		}
	}

	buildingClauses := []string{
		"FROM f JOIN t USING (room_id)",
		"f.building_id <> t.building_id",
		"ORDER BY f.room_id",
		"ORDER BY p.room_id",
		"LIMIT $3 OFFSET $4",
	}
	for _, clause := range buildingClauses {
		if !strings.Contains(releaseDiffBuildingsPageSQL, clause) {
			t.Fatalf("buildings 分页缺少 %q:\n%s", clause, releaseDiffBuildingsPageSQL)
		}
	}

	// 三节都必须是单条语句，且都不带 count(*)：total 由 DiffTotal 从汇总推导。
	for name, sql := range map[string]string{
		"cells":     releaseDiffCellsPageSQL,
		"rooms":     releaseDiffRoomsPageSQL,
		"buildings": releaseDiffBuildingsPageSQL,
	} {
		if strings.Contains(sql, ";") {
			t.Fatalf("%s 分页必须是单条语句，不能带分号:\n%s", name, sql)
		}
		if strings.Contains(strings.ToLower(sql), "count(") {
			t.Fatalf("%s 分页不应再数行（total 从汇总推导）:\n%s", name, sql)
		}
		if got := strings.Count(sql, "\nSELECT "); got != 1 {
			t.Fatalf("%s 分页只应有一个顶层 SELECT，实际 %d 个:\n%s", name, got, sql)
		}
	}
}

// TestDiffTotalDerivation 覆盖 total 的推导矩阵：三节 × 各自的合法 kind，
// 以及零值按缺省（cells / all）与未识别组合返回 0。
func TestDiffTotalDerivation(t *testing.T) {
	summary := ReleaseDiffSummary{
		Cells: DiffCellsSummary{Added: 11, Removed: 22, Changed: 33, Total: 66},
		Rooms: DiffRoomsSummary{Added: 4, Removed: 5, Shared: 100, BuildingChanged: 6},
	}

	cases := []struct {
		name    string
		section DiffSection
		kind    DiffKind
		want    int
	}{
		{"缺省小节取 cells", "", "", 66}, // 零值即缺省
		{"cells/all", DiffSectionCells, DiffKindAll, 66},
		{"cells/added", DiffSectionCells, DiffKindAdded, 11},
		{"cells/removed", DiffSectionCells, DiffKindRemoved, 22},
		{"cells/changed", DiffSectionCells, DiffKindChanged, 33},
		{"rooms/all", DiffSectionRooms, DiffKindAll, 9}, // added + removed，不含 shared
		{"rooms/added", DiffSectionRooms, DiffKindAdded, 4},
		{"rooms/removed", DiffSectionRooms, DiffKindRemoved, 5},
		{"buildings/all", DiffSectionBuildings, DiffKindAll, 6},
		{"buildings/changed", DiffSectionBuildings, DiffKindChanged, 6}, // all 与 changed 同集合
		// 以下组合由 api 参数门挡掉（Requirement 4.5）；仓储返回 0 暴露上游 bug，
		// 不静默按 all 或 cells 解释。
		{"rooms/changed 非法", DiffSectionRooms, DiffKindChanged, 0},
		{"buildings/added 非法", DiffSectionBuildings, DiffKindAdded, 0},
		{"buildings/removed 非法", DiffSectionBuildings, DiffKindRemoved, 0},
		{"未知小节", DiffSection("blocks"), DiffKindAll, 0},
		{"未知类别", DiffSectionCells, DiffKind("same"), 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := DiffTotal(testCase.section, testCase.kind, summary); got != testCase.want {
				t.Fatalf("DiffTotal(%q, %q) = %d，期望 %d",
					testCase.section, testCase.kind, got, testCase.want)
			}
		})
	}
}

// TestLoadReleaseDiffSectionRouting 覆盖三节分派与参数编号：
// 每个 section 只发自己的那条分页语句，参数按该语句的编号拼接；
// 未选中的两个切片保持非 nil 空切片（序列化输出 []）。
func TestLoadReleaseDiffSectionRouting(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()

	cases := []struct {
		name     string
		section  DiffSection
		kind     DiffKind
		limit    int
		offset   int
		wantSQL  string
		wantArgs []any
	}{
		{
			name: "缺省参数落到 cells/all/50/0",
			// section、kind、limit 全零，offset 本就是 0
			wantSQL:  releaseDiffCellsPageSQL,
			wantArgs: []any{from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID, "all", 50, 0},
		},
		{
			name:    "cells 带类别与偏移",
			section: DiffSectionCells, kind: DiffKindChanged, limit: 7, offset: 3,
			wantSQL:  releaseDiffCellsPageSQL,
			wantArgs: []any{from.AxisVersion, from.ReleaseID, to.AxisVersion, to.ReleaseID, "changed", 7, 3},
		},
		{
			name:    "rooms 只读归属快照",
			section: DiffSectionRooms, kind: DiffKindRemoved, limit: 5, offset: 2,
			wantSQL:  releaseDiffRoomsPageSQL,
			wantArgs: []any{from.ReleaseID, to.ReleaseID, "removed", 5, 2},
		},
		{
			name:    "buildings 不带 kind",
			section: DiffSectionBuildings, kind: DiffKindChanged, limit: 9, offset: 1,
			wantSQL:  releaseDiffBuildingsPageSQL,
			wantArgs: []any{from.ReleaseID, to.ReleaseID, 9, 1},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cell := DiffCellRow{Kind: DiffKindAdded, RoomID: "r1", RoomName: "F1",
				Week: 9, Weekday: 1, Block: "0102",
				ToState: releaseAdminPtr("free"), ToBuilding: releaseAdminPtr("Q10")}
			room := DiffRoomRow{Kind: DiffKindAdded, RoomID: "r2", RoomName: "F2",
				ToBuilding: releaseAdminPtr("Q10"), ToBuildingName: releaseAdminPtr("综合教学楼")}
			building := DiffBuildingRow{RoomID: "r3", RoomName: "F3",
				FromBuilding: "Q10", FromBuildingName: "综合教学楼",
				ToBuilding: "Q03", ToBuildingName: "老文史楼"}
			querier := &releaseDiffQuerierStub{
				heads: map[string]pgx.Row{
					from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
					to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
				},
				summary:   &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
				cells:     &releaseAdminRowsStub{rows: [][]any{releaseDiffCellValues(cell)}},
				rooms:     &releaseAdminRowsStub{rows: [][]any{releaseDiffRoomValues(room)}},
				buildings: &releaseAdminRowsStub{rows: [][]any{releaseDiffBuildingValues(building)}},
			}

			diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
				From: from.ReleaseID, To: to.ReleaseID,
				Section: testCase.section, Kind: testCase.kind,
				Limit: testCase.limit, Offset: testCase.offset,
			})
			if err != nil {
				t.Fatalf("对比失败: %v", err)
			}
			if querier.pageSQL != testCase.wantSQL {
				t.Fatalf("分页应走 %q，实际 %q", testCase.wantSQL, querier.pageSQL)
			}
			if fmt.Sprint(querier.pageArgs) != fmt.Sprint(testCase.wantArgs) {
				t.Fatalf("分页参数不符:\n got %v\nwant %v", querier.pageArgs, testCase.wantArgs)
			}

			// 只有被选中的小节有行；另外两个是空切片而不是 nil。
			section := testCase.section
			if section == "" {
				section = DiffSectionCells // 零值缺省与 normalized 一致
			}
			wantCells, wantRooms, wantBuildings := 0, 0, 0
			switch section {
			case DiffSectionCells:
				wantCells = 1
			case DiffSectionRooms:
				wantRooms = 1
			case DiffSectionBuildings:
				wantBuildings = 1
			}
			if len(diff.Cells) != wantCells {
				t.Fatalf("cells 切片应有 %d 行，实际 %+v", wantCells, diff.Cells)
			}
			if len(diff.Rooms) != wantRooms {
				t.Fatalf("rooms 切片应有 %d 行，实际 %+v", wantRooms, diff.Rooms)
			}
			if len(diff.Buildings) != wantBuildings {
				t.Fatalf("buildings 切片应有 %d 行，实际 %+v", wantBuildings, diff.Buildings)
			}
			if diff.Cells == nil || diff.Rooms == nil || diff.Buildings == nil {
				t.Fatalf("三个切片必须非 nil: cells=%v rooms=%v buildings=%v",
					diff.Cells, diff.Rooms, diff.Buildings)
			}
		})
	}
}

// TestLoadReleaseDiffCellsPageScan 覆盖 cells 分页的列序与可空列：
// 新增格没有 from_state / from_building，消失格没有 to_state / to_building，
// 变化格两侧都有；kind / week / weekday / block 与房名逐列断言。
func TestLoadReleaseDiffCellsPageScan(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	want := []DiffCellRow{
		{ // 变化格：两侧都出现
			Kind: DiffKindChanged, RoomID: "r203", RoomName: "F203",
			Week: 9, Weekday: 1, Block: "0102",
			FromState:    releaseAdminPtr("free"),
			ToState:      releaseAdminPtr("class"),
			FromBuilding: releaseAdminPtr("Q10"),
			ToBuilding:   releaseAdminPtr("Q10"),
		},
		{ // 新增格：只有 to 侧
			Kind: DiffKindAdded, RoomID: "r208", RoomName: "F208",
			Week: 9, Weekday: 2, Block: "0809",
			ToState:    releaseAdminPtr("composite"),
			ToBuilding: releaseAdminPtr("Q10"),
		},
		{ // 消失格：只有 from 侧
			Kind: DiffKindRemoved, RoomID: "r202", RoomName: "",
			Week: 10, Weekday: 3, Block: "101112",
			FromState:    releaseAdminPtr("unknown"),
			FromBuilding: releaseAdminPtr("Q03"),
		},
	}
	rows := make([][]any, 0, len(want))
	for _, row := range want {
		rows = append(rows, releaseDiffCellValues(row))
	}
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		cells:   &releaseAdminRowsStub{rows: rows},
	}
	cellRows := querier.cells.(*releaseAdminRowsStub)

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID,
		Section: DiffSectionCells, Kind: DiffKindAll, Limit: 3, Offset: 0,
	})
	if err != nil {
		t.Fatalf("对比失败: %v", err)
	}
	if !reflect.DeepEqual(diff.Cells, want) {
		t.Fatalf("cells 扫描不符:\n got %+v\nwant %+v", diff.Cells, want)
	}
	if !cellRows.closed {
		t.Fatal("cells 行集读完必须关闭")
	}
}

// TestLoadReleaseDiffRoomsPageScan 覆盖 rooms 分页的列序与四个可空归属字段：
// 新增房 from_* 为 nil，消失房 to_* 为 nil。
func TestLoadReleaseDiffRoomsPageScan(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	want := []DiffRoomRow{
		{
			Kind: DiffKindRemoved, RoomID: "r202", RoomName: "F202",
			FromBuilding: releaseAdminPtr("Q10"), FromBuildingName: releaseAdminPtr("综合教学楼"),
		},
		{
			Kind: DiffKindAdded, RoomID: "r208", RoomName: "F208",
			ToBuilding: releaseAdminPtr("Q10"), ToBuildingName: releaseAdminPtr("综合教学楼"),
		},
	}
	rows := make([][]any, 0, len(want))
	for _, row := range want {
		rows = append(rows, releaseDiffRoomValues(row))
	}
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		rooms:   &releaseAdminRowsStub{rows: rows},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID,
		Section: DiffSectionRooms, Kind: DiffKindAll, Limit: 2, Offset: 0,
	})
	if err != nil {
		t.Fatalf("对比失败: %v", err)
	}
	if !reflect.DeepEqual(diff.Rooms, want) {
		t.Fatalf("rooms 扫描不符:\n got %+v\nwant %+v", diff.Rooms, want)
	}
}

// TestLoadReleaseDiffBuildingsPageScan 覆盖 buildings 分页的列序：四个归属字段都非空，
// 房名在最后一列。
func TestLoadReleaseDiffBuildingsPageScan(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	want := []DiffBuildingRow{
		{RoomID: "r204", RoomName: "F204",
			FromBuilding: "Q10", FromBuildingName: "综合教学楼",
			ToBuilding: "Q03", ToBuildingName: "老文史楼"},
	}
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary:   &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		buildings: &releaseAdminRowsStub{rows: [][]any{releaseDiffBuildingValues(want[0])}},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID,
		Section: DiffSectionBuildings, Kind: DiffKindChanged, Limit: 1, Offset: 0,
	})
	if err != nil {
		t.Fatalf("对比失败: %v", err)
	}
	if !reflect.DeepEqual(diff.Buildings, want) {
		t.Fatalf("buildings 扫描不符:\n got %+v\nwant %+v", diff.Buildings, want)
	}
}

// TestLoadReleaseDiffEmptyPageIsEmptySlice 覆盖 offset 翻过末页：分页行集为空，
// 选中切片仍是非 nil 空切片，total 由汇总推导不随页为空而变化。
func TestLoadReleaseDiffEmptyPageIsEmptySlice(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		cells:   &releaseAdminRowsStub{}, // 空页
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID,
		Section: DiffSectionCells, Kind: DiffKindAll, Limit: 50, Offset: 100,
	})
	if err != nil {
		t.Fatalf("对比失败: %v", err)
	}
	if diff.Cells == nil {
		t.Fatal("空页应返回空切片而不是 nil")
	}
	if len(diff.Cells) != 0 {
		t.Fatalf("翻过末页不应有行: %+v", diff.Cells)
	}
	if got := DiffTotal(DiffSectionCells, DiffKindAll, diff.Summary); got != diff.Summary.Cells.Total {
		t.Fatalf("total 应来自汇总、不随页为空变化: got %d want %d",
			got, diff.Summary.Cells.Total)
	}
}

// TestLoadReleaseDiffUnknownSectionFails 覆盖未知 section：不发分页语句，返回包着
// model.ErrQueryFailed 的错误与零值（合法取值由 api 参数门保证）。
func TestLoadReleaseDiffUnknownSectionFails(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID,
		Section: DiffSection("blocks"),
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
	if querier.pageRead {
		t.Fatal("未知小节不应发任何分页语句")
	}
}

// TestLoadReleaseDiffPageQueryErrorWraps 覆盖分页读库失败：包 model.ErrQueryFailed，
// 不返回半份结果。
func TestLoadReleaseDiffPageQueryErrorWraps(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary:  &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		cellsErr: errors.New("连接断了"),
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID, Section: DiffSectionCells,
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
}

// TestLoadReleaseDiffPageScanErrorWraps 覆盖分页扫描失败：包 model.ErrQueryFailed，
// 行集照常关闭，不返回半页。
func TestLoadReleaseDiffPageScanErrorWraps(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	rows := &releaseAdminRowsStub{
		rows:    [][]any{releaseDiffRoomValues(DiffRoomRow{Kind: DiffKindAdded, RoomID: "r208"})},
		scanErr: errors.New("列不可解"),
	}
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary: &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		rooms:   rows,
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID, Section: DiffSectionRooms,
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
	if !rows.closed {
		t.Fatal("扫描失败也要关闭行集")
	}
}

// TestLoadReleaseDiffPageRowsErrorWraps 覆盖分页遍历到一半失败：已取到的行是残的，
// 整体报错，不交半页。
func TestLoadReleaseDiffPageRowsErrorWraps(t *testing.T) {
	from, to := releaseDiffFromSide(), releaseDiffToSide()
	rows := &releaseAdminRowsStub{
		rows: [][]any{releaseDiffBuildingValues(DiffBuildingRow{
			RoomID: "r204", FromBuilding: "Q10", FromBuildingName: "综合教学楼",
			ToBuilding: "Q03", ToBuildingName: "老文史楼", RoomName: "F204",
		})},
		err: errors.New("遍历中连接断了"),
	}
	querier := &releaseDiffQuerierStub{
		heads: map[string]pgx.Row{
			from.ReleaseID: &releaseAdminRowStub{values: releaseSideRow(from)},
			to.ReleaseID:   &releaseAdminRowStub{values: releaseSideRow(to)},
		},
		summary:   &releaseAdminRowStub{values: releaseDiffPageSummaryRow()},
		buildings: rows,
	}

	diff, err := loadReleaseDiff(context.Background(), querier, ReleaseDiffParams{
		From: from.ReleaseID, To: to.ReleaseID, Section: DiffSectionBuildings,
	})
	if !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
	}
	if !reflect.DeepEqual(diff, ReleaseDiff{}) {
		t.Fatalf("失败时应返回零值，实际 %+v", diff)
	}
	if !rows.closed {
		t.Fatal("遍历失败也要关闭行集")
	}
}

// releaseRollbackTxStub 是 releaseRollbackTx 的替身：按 SQL 常量回放两条加锁单行读、
// 两条 UPDATE 与提交/回滚，并把每次调用记进 calls。
//
// 回退的正确性一半在「做了什么」，一半在「按什么顺序做」：锁序写反、先抬后降，
// 单看最终状态或返回值都可能照样通过，只有顺序断言能拦住。所以这里的记录粒度是
// 每条语句（current / target / demote / promote / commit），不是「有没有调用」。
type releaseRollbackTxStub struct {
	pgx.Tx // 只覆盖被测路径用到的方法，其余调用即 panic

	currentRow pgx.Row // 锁当前行（releaseLockCurrentSQL）的回放结果
	targetRow  pgx.Row // 锁目标行（releaseLockTargetSQL）的回放结果

	demoteErr   error // 降原 current 的错误（非空时不发命令标签）
	demoteRows  int64 // 降原 current 的影响行数
	promoteErr  error // 抬目标的错误
	promoteRows int64 // 抬目标的影响行数

	commitErr error // 提交错误，由外层 Rollback 映射

	calls       []string // current / target / demote / promote / commit
	targetArgs  []any    // 锁目标行收到的参数
	promoteArgs []any    // 抬目标收到的参数

	committed  bool // Commit 是否成功
	rolledBack bool // Rollback 是否被调用：失败路径必须靠它收尾
}

// QueryRow 按 SQL 常量分派两条加锁读，并记下顺序。
func (s *releaseRollbackTxStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch sql {
	case releaseLockCurrentSQL:
		s.calls = append(s.calls, "current")
		return s.currentRow
	case releaseLockTargetSQL:
		s.calls = append(s.calls, "target")
		s.targetArgs = append([]any(nil), args...)
		return s.targetRow
	default:
		panic("未知的单行读语句")
	}
}

// Exec 按 SQL 常量分派两条 UPDATE，按预设的行数造命令标签。
func (s *releaseRollbackTxStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch sql {
	case releaseDemoteCurrentSQL:
		s.calls = append(s.calls, "demote")
		if s.demoteErr != nil {
			return pgconn.CommandTag{}, s.demoteErr
		}
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", s.demoteRows)), nil
	case releasePromoteTargetSQL:
		s.calls = append(s.calls, "promote")
		s.promoteArgs = append([]any(nil), args...)
		if s.promoteErr != nil {
			return pgconn.CommandTag{}, s.promoteErr
		}
		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", s.promoteRows)), nil
	default:
		panic("未知的写入语句")
	}
}

// Commit 记下提交，并回放预设错误。
func (s *releaseRollbackTxStub) Commit(context.Context) error {
	s.calls = append(s.calls, "commit")
	if s.commitErr != nil {
		return s.commitErr
	}
	s.committed = true
	return nil
}

// Rollback 记录收尾调用。成功提交后 defer 也会调它，所以它不是失败标志。
func (s *releaseRollbackTxStub) Rollback(context.Context) error {
	s.rolledBack = true
	return nil
}

// releaseRollbackDefaultCurrentRow 造一行「当前版本」回放数据，列序与
// releaseLockCurrentSQL 一致。当前行在回退用例里只有这一个身份：能成功回退的夹具与
// 各条拒绝分支比的都是「当前版本是 20261114T030512Z、学期 2026-2027-1」。
func releaseRollbackDefaultCurrentRow() pgx.Row {
	return &releaseAdminRowStub{values: []any{"20261114T030512Z", "2026-2027-1"}}
}

// releaseRollbackTargetRow 造一行「回退目标」回放数据，列序与 releaseLockTargetSQL 一致。
func releaseRollbackTargetRow(id, term string, isCurrent bool) pgx.Row {
	return &releaseAdminRowStub{values: []any{id, term, isCurrent}}
}

// releaseRollbackTxFixture 是一套「可以成功回退」的默认回放：当前版本较新、目标是
// 同学期的非 current 版本、两条 UPDATE 各命中 1 行。
func releaseRollbackTxFixture() *releaseRollbackTxStub {
	return &releaseRollbackTxStub{
		currentRow:  releaseRollbackDefaultCurrentRow(),
		targetRow:   releaseRollbackTargetRow("20261028T041500Z", "2026-2027-1", false),
		demoteRows:  1,
		promoteRows: 1,
	}
}

// TestReleaseRollbackSQLClauses 锁定四条语句的关键子句：两条加锁读都带 FOR UPDATE，
// 降 current 不带版本标识，抬目标带 NOT is_current 守卫。
//
// 这些子句是锁序（CP8）与「目标行数必须为 1」的载体：改成普通 SELECT 或去掉守卫后，
// 单元用例仍然能过，只有真实库的并发与拒绝分支才会偶尔暴露。
func TestReleaseRollbackSQLClauses(t *testing.T) {
	cases := []struct {
		name     string
		sql      string
		contains []string
	}{
		{"锁当前行", releaseLockCurrentSQL, []string{"FROM release", "WHERE is_current", "FOR UPDATE"}},
		{"锁目标行", releaseLockTargetSQL, []string{"WHERE release_id = $1", "FOR UPDATE"}},
		{"降原 current", releaseDemoteCurrentSQL, []string{"SET is_current = false", "WHERE is_current"}},
		{"抬目标", releasePromoteTargetSQL, []string{"SET is_current = true", "WHERE release_id = $1", "AND NOT is_current"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.contains {
				if !strings.Contains(tc.sql, want) {
					t.Fatalf("%s 缺少子句 %q：\n%s", tc.name, want, tc.sql)
				}
			}
		})
	}

	// 降 current 必须不按 release_id 过滤：current 至多一行，带上标识只会让「锁的
	// 那一行」与「更新的一行」多出一个需要证明相等的地方。
	if strings.Contains(releaseDemoteCurrentSQL, "release_id") {
		t.Fatalf("降原 current 不应带版本标识：\n%s", releaseDemoteCurrentSQL)
	}
}

// TestRollbackReleaseSwapsCurrent 断言成功路径的语句顺序与参数：先锁当前、再锁目标、
// 先降后抬，结果字段来自读回的两行。
func TestRollbackReleaseSwapsCurrent(t *testing.T) {
	tx := releaseRollbackTxFixture()

	result, err := rollbackRelease(context.Background(), tx, "20261028T041500Z")
	if err != nil {
		t.Fatalf("回退应成功，实际 %v", err)
	}
	want := RollbackResult{
		RolledBackFrom:   "20261114T030512Z",
		CurrentReleaseID: "20261028T041500Z",
		Term:             "2026-2027-1",
	}
	if result != want {
		t.Fatalf("回退结果不符：期望 %+v，实际 %+v", want, result)
	}

	wantCalls := []string{"current", "target", "demote", "promote"}
	if !reflect.DeepEqual(tx.calls, wantCalls) {
		t.Fatalf("语句顺序不符：期望 %v，实际 %v", wantCalls, tx.calls)
	}
	if !reflect.DeepEqual(tx.targetArgs, []any{"20261028T041500Z"}) {
		t.Fatalf("锁目标行的参数不符：%v", tx.targetArgs)
	}
	if !reflect.DeepEqual(tx.promoteArgs, []any{"20261028T041500Z"}) {
		t.Fatalf("抬目标的参数不符：%v", tx.promoteArgs)
	}
	if tx.committed {
		t.Fatal("事务体不应自己提交：提交属于外层 Rollback")
	}
}

// TestRollbackReleaseRejects 覆盖四条拒绝分支：每条都必须在任何 UPDATE 之前返回，
// 且返回零值结果。
func TestRollbackReleaseRejects(t *testing.T) {
	cases := []struct {
		name      string
		current   pgx.Row
		target    pgx.Row
		targetID  string
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "目标不存在",
			current:   releaseRollbackDefaultCurrentRow(),
			target:    &releaseAdminRowStub{err: pgx.ErrNoRows},
			targetID:  "20200101T000000Z",
			wantErr:   ErrReleaseNotFound,
			wantCalls: []string{"current", "target"},
		},
		{
			name:      "目标已是当前版本",
			current:   releaseRollbackDefaultCurrentRow(),
			target:    releaseRollbackTargetRow("20261114T030512Z", "2026-2027-1", true),
			targetID:  "20261114T030512Z",
			wantErr:   ErrReleaseNotRollbackable,
			wantCalls: []string{"current", "target"},
		},
		{
			name:      "库中没有当前版本",
			current:   &releaseAdminRowStub{err: pgx.ErrNoRows},
			target:    releaseRollbackTargetRow("20261028T041500Z", "2026-2027-1", false),
			targetID:  "20261028T041500Z",
			wantErr:   ErrReleaseNotRollbackable,
			wantCalls: []string{"current", "target"},
		},
		{
			name:      "跨学期",
			current:   releaseRollbackDefaultCurrentRow(),
			target:    releaseRollbackTargetRow("20260901T010203Z", "2025-2026-2", false),
			targetID:  "20260901T010203Z",
			wantErr:   ErrReleaseTermMismatch,
			wantCalls: []string{"current", "target"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := &releaseRollbackTxStub{
				currentRow:  tc.current,
				targetRow:   tc.target,
				demoteRows:  1,
				promoteRows: 1,
			}
			result, err := rollbackRelease(context.Background(), tx, tc.targetID)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
			if result != (RollbackResult{}) {
				t.Fatalf("拒绝路径必须返回零值结果，实际 %+v", result)
			}
			if !reflect.DeepEqual(tx.calls, tc.wantCalls) {
				t.Fatalf("拒绝路径不应发 UPDATE：期望调用 %v，实际 %v", tc.wantCalls, tx.calls)
			}
		})
	}
}

// TestRollbackReleaseRowCountGuards 覆盖两条 UPDATE 的行数守卫：抬目标 0 行按
// 「不可回退」收口；降 current 0 行说明库的 current 不变量已被破坏，按内部错误暴露。
func TestRollbackReleaseRowCountGuards(t *testing.T) {
	t.Run("抬目标命中0行", func(t *testing.T) {
		tx := releaseRollbackTxFixture()
		tx.promoteRows = 0
		_, err := rollbackRelease(context.Background(), tx, "20261028T041500Z")
		if !errors.Is(err, ErrReleaseNotRollbackable) {
			t.Fatalf("期望 ErrReleaseNotRollbackable，实际 %v", err)
		}
	})

	t.Run("降current命中0行", func(t *testing.T) {
		tx := releaseRollbackTxFixture()
		tx.demoteRows = 0
		_, err := rollbackRelease(context.Background(), tx, "20261028T041500Z")
		if !errors.Is(err, model.ErrQueryFailed) {
			t.Fatalf("期望 model.ErrQueryFailed，实际 %v", err)
		}
		if containsString(tx.calls, "promote") {
			t.Fatalf("降 current 失败后不应继续抬目标：%v", tx.calls)
		}
	})
}

// TestRollbackReleaseMapsRetryableSQLStates 断言三个可重试 SQLSTATE 在各条写入上
// 都映射成 ErrReleaseConflict，而不是 50000。
func TestRollbackReleaseMapsRetryableSQLStates(t *testing.T) {
	for _, code := range []string{"23505", "40P01", "40001"} {
		for _, stage := range []string{"demote", "promote"} {
			t.Run(code+"-"+stage, func(t *testing.T) {
				tx := releaseRollbackTxFixture()
				pgErr := &pgconn.PgError{Code: code}
				if stage == "demote" {
					tx.demoteErr = pgErr
				} else {
					tx.promoteErr = pgErr
				}
				_, err := rollbackRelease(context.Background(), tx, "20261028T041500Z")
				if !errors.Is(err, ErrReleaseConflict) {
					t.Fatalf("SQLSTATE %s 应映射 ErrReleaseConflict，实际 %v", code, err)
				}
				if errors.Is(err, model.ErrQueryFailed) {
					t.Fatalf("并发冲突不应同时被当成读失败：%v", err)
				}
			})
		}
	}
}

// TestRollbackReleaseWrapsOtherErrors 覆盖非并发冲突的错误口径：一律包
// model.ErrQueryFailed，且不误报成 ErrReleaseConflict。
func TestRollbackReleaseWrapsOtherErrors(t *testing.T) {
	cases := []struct {
		name string
		tx   func() *releaseRollbackTxStub
	}{
		{
			name: "锁当前行失败",
			tx: func() *releaseRollbackTxStub {
				tx := releaseRollbackTxFixture()
				tx.currentRow = &releaseAdminRowStub{err: errors.New("连接中断")}
				return tx
			},
		},
		{
			name: "锁目标行失败",
			tx: func() *releaseRollbackTxStub {
				tx := releaseRollbackTxFixture()
				tx.targetRow = &releaseAdminRowStub{err: errors.New("连接中断")}
				return tx
			},
		},
		{
			name: "降current失败（非并发）",
			tx: func() *releaseRollbackTxStub {
				tx := releaseRollbackTxFixture()
				tx.demoteErr = &pgconn.PgError{Code: "22021"}
				return tx
			},
		},
		{
			name: "抬目标失败（非并发）",
			tx: func() *releaseRollbackTxStub {
				tx := releaseRollbackTxFixture()
				tx.promoteErr = &pgconn.PgError{Code: "22021"}
				return tx
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rollbackRelease(context.Background(), tc.tx(), "20261028T041500Z")
			if !errors.Is(err, model.ErrQueryFailed) {
				t.Fatalf("期望 model.ErrQueryFailed，实际 %v", err)
			}
			if errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("非并发冲突不应映射 ErrReleaseConflict: %v", err)
			}
		})
	}
}

// TestReleaseAdminRepoRollbackOpensReadCommittedTx 断言仓储用 READ COMMITTED 开事务、
// 成功后提交，并把事务体结果原样返回。
func TestReleaseAdminRepoRollbackOpensReadCommittedTx(t *testing.T) {
	tx := releaseRollbackTxFixture()
	pool := &releaseAdminPoolStub{tx: tx}
	repo := NewReleaseAdminRepo(pool)

	result, err := repo.Rollback(context.Background(), "20261028T041500Z")
	if err != nil {
		t.Fatalf("回退应成功，实际 %v", err)
	}
	if pool.txOptions.IsoLevel != pgx.ReadCommitted {
		t.Fatalf("隔离级别应为 READ COMMITTED，实际 %v", pool.txOptions.IsoLevel)
	}
	// 访问模式留零值（PostgreSQL 默认读写）；只读事务会让两条 UPDATE 在库上直接失败。
	if pool.txOptions.AccessMode == pgx.ReadOnly {
		t.Fatalf("回退必须是可写事务，实际访问模式 %v", pool.txOptions.AccessMode)
	}
	if !tx.committed {
		t.Fatal("成功路径必须提交事务")
	}
	if result.CurrentReleaseID != "20261028T041500Z" {
		t.Fatalf("返回的当前版本不符：%+v", result)
	}
}

// TestReleaseAdminRepoRollbackBeginTxError 断言开事务失败包 model.ErrQueryFailed。
func TestReleaseAdminRepoRollbackBeginTxError(t *testing.T) {
	pool := &releaseAdminPoolStub{beginErr: errors.New("连接池耗尽")}
	repo := NewReleaseAdminRepo(pool)

	if _, err := repo.Rollback(context.Background(), "20261028T041500Z"); !errors.Is(err, model.ErrQueryFailed) {
		t.Fatalf("期望 model.ErrQueryFailed，实际 %v", err)
	}
}

// TestReleaseAdminRepoRollbackCommitError 断言提交失败的两类口径：可重试 SQLSTATE
// 映射 ErrReleaseConflict，其余映射 model.ErrQueryFailed；失败都不返回结果。
func TestReleaseAdminRepoRollbackCommitError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantErr error
	}{
		{"提交撞唯一索引", &pgconn.PgError{Code: "23505"}, ErrReleaseConflict},
		{"提交死锁", &pgconn.PgError{Code: "40P01"}, ErrReleaseConflict},
		{"提交普通失败", errors.New("连接断开"), model.ErrQueryFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := releaseRollbackTxFixture()
			tx.commitErr = tc.err
			pool := &releaseAdminPoolStub{tx: tx}
			repo := NewReleaseAdminRepo(pool)

			result, err := repo.Rollback(context.Background(), "20261028T041500Z")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望 %v，实际 %v", tc.wantErr, err)
			}
			if result != (RollbackResult{}) {
				t.Fatalf("提交失败必须返回零值结果，实际 %+v", result)
			}
			if tx.committed {
				t.Fatal("提交失败不应记成已提交")
			}
			if !tx.rolledBack {
				t.Fatal("失败路径必须回滚收尾")
			}
		})
	}
}

// containsString 判断切片里是否出现过 want，用于「某条语句一次都不该出现」的断言。
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestWeekGaps 覆盖周覆盖缺口的纯函数口径（Requirement 7.3、design 的边界清单）：
// 空输入、单周、连续区间、缺中间周、多周缺口、无序输入与非 1 起始的区间。
func TestWeekGaps(t *testing.T) {
	cases := []struct {
		name     string
		observed []int
		want     []int
	}{
		{name: "空输入", observed: nil, want: []int{}},
		{name: "空切片", observed: []int{}, want: []int{}},
		{name: "单周没有缺口", observed: []int{9}, want: []int{}},
		{name: "连续四周没有缺口", observed: []int{9, 10, 11, 12}, want: []int{}},
		{name: "缺中间一周", observed: []int{9, 10, 12}, want: []int{11}},
		{name: "两头之间缺多周", observed: []int{5, 9}, want: []int{6, 7, 8}},
		{name: "无序且重复仍按升序给出", observed: []int{12, 9, 12, 10}, want: []int{11}},
		{name: "区间不从 1 开始", observed: []int{16, 14}, want: []int{15}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := weekGaps(tc.observed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("weekGaps(%v) = %v，期望 %v", tc.observed, got, tc.want)
			}
			if got == nil {
				t.Fatal("weekGaps 必须返回非 nil 切片，JSON 才会输出 []")
			}
		})
	}
}

// TestRatioOf 覆盖占比的纯函数口径（Requirement 7.2）：4 位小数、四舍五入、
// 分母为 0 记 0，以及中点向上这个边界。中点用例按浮点不精确的方向挑：
// 57/800 = 0.07125 与 3/20000 = 0.00015 在 float64 里略小于真值，旧实现会舍成
// 0.0712 / 0.0001，整数运算才得到 0.0713 / 0.0002。
func TestRatioOf(t *testing.T) {
	cases := []struct {
		name       string
		cells      int
		blockCells int
		want       float64
	}{
		{name: "分母为 0 且分子为 0", cells: 0, blockCells: 0, want: 0},
		{name: "分母为 0 但有分子", cells: 12, blockCells: 0, want: 0},
		{name: "分子为 0", cells: 0, blockCells: 363050, want: 0},
		{name: "48/363050 进到第 4 位", cells: 48, blockCells: 363050, want: 0.0001},
		{name: "12/363050 舍成 0", cells: 12, blockCells: 363050, want: 0},
		{name: "中点 0.00005 向上", cells: 5, blockCells: 100000, want: 0.0001},
		{name: "边界下方 0.00004 舍去", cells: 4, blockCells: 100000, want: 0},
		{name: "中点 57/800 向上到 0.0713", cells: 57, blockCells: 800, want: 0.0713},
		{name: "中点 3/20000 向上到 0.0002", cells: 3, blockCells: 20000, want: 0.0002},
		{name: "1/3 保留四位", cells: 1, blockCells: 3, want: 0.3333},
		{name: "2/3 保留四位", cells: 2, blockCells: 3, want: 0.6667},
		{name: "整比例", cells: 1, blockCells: 1, want: 1},
		{name: "负数分子兜底 0", cells: -1, blockCells: 10, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratioOf(tc.cells, tc.blockCells); got != tc.want {
				t.Fatalf("ratioOf(%d, %d) = %v，期望 %v", tc.cells, tc.blockCells, got, tc.want)
			}
		})
	}
}

// TestReleaseAnomaliesSQLClauses 锁定异常视图两条语句的口径：
// 折叠按各版自己的轴版本、未知/复合数折叠后的格（不是小节行）、周次去重升序、
// 窗口取 run_id 最大的一轮、版本按生成时间倒序；冲突清单取最新一轮、按房号编号、
// 多取一条识别截断。
func TestReleaseAnomaliesSQLClauses(t *testing.T) {
	clauses := []string{
		"JOIN axis_node an ON an.axis_version = r.axis_version AND an.node = o.node", // 按该版自己的轴折叠
		"GROUP BY o.room_id, o.week, o.weekday, an.block",                            // 折叠键：房号 × 周 × 星期 × 大节
		"min(o.state_key) AS state_key",                                              // 块状态取 min，与对比/发布同口径
		"count(*) FILTER (WHERE blk.state_key = 'unknown')",                          // 数折叠后的格，不是小节
		"count(*) FILTER (WHERE blk.state_key = 'composite')",
		"array_agg(DISTINCT blk.week::int ORDER BY blk.week::int)", // 观测周去重升序
		"sum(blk.node_rows)::bigint",                               // 观测行数按大节格求和，仍是小节粒度
		"COALESCE(s.observed_weeks, '{}'::int[])",
		"ORDER BY cr.run_id DESC", // 窗口取最新一轮
		"LIMIT 1",
		"ORDER BY r.generated_at DESC, r.release_id DESC", // 与列表同一排序
	}
	for _, clause := range clauses {
		if !strings.Contains(releaseAnomaliesSQL, clause) {
			t.Fatalf("异常视图 SQL 缺少 %q:\n%s", clause, releaseAnomaliesSQL)
		}
	}
	// 折叠前的 observation.state_key 不能直接拿来数：同一大节展开的 2–3 个小节
	// 会重复计数（D12 / CP11）。
	if strings.Contains(releaseAnomaliesSQL, "WHERE o.state_key") {
		t.Fatalf("未知/复合必须数折叠后的格，不能直接数 observation 行:\n%s", releaseAnomaliesSQL)
	}
	// 窗口取最新一轮；写成 ASC 会让回退后窗口停在旧轮次。
	if strings.Contains(releaseAnomaliesSQL, "run_id ASC") {
		t.Fatalf("窗口应取 run_id 最大的一轮:\n%s", releaseAnomaliesSQL)
	}

	// 上限从 conflictRoomsLimit 派生：Go 侧按 rank > conflictRoomsLimit 置截断，
	// SQL 侧按 rn <= conflictRoomsLimit+1 取数，两边必须是同一个数字，不能各写一份。
	conflictFetchLimit := conflictRoomsLimit + 1
	conflictClauses := []string{
		"row_number() OVER (PARTITION BY r.release_id ORDER BY crr.room_id)", // 截断保留 room_id 最小的那批
		"WHERE r.has_attr_conflict",      // 只查标了冲突的版本
		"ORDER BY c.run_id DESC LIMIT 1", // 取该版最新一轮
		"JOIN collect_run_room crr ON crr.run_id = pub.run_id AND crr.conflict",
		fmt.Sprintf("WHERE rn <= %d", conflictFetchLimit), // 多取一条（200 + 1）才能识别截断
		"ORDER BY release_id, rn",
	}
	for _, clause := range conflictClauses {
		if !strings.Contains(releaseAnomalyConflictRoomsSQL, clause) {
			t.Fatalf("冲突清单 SQL 缺少 %q:\n%s", clause, releaseAnomalyConflictRoomsSQL)
		}
	}
	if !strings.Contains(releaseAnomalyConflictRoomsSQL, fmt.Sprintf("%d", conflictFetchLimit)) {
		t.Fatalf("冲突清单必须多取一条以识别截断:\n%s", releaseAnomalyConflictRoomsSQL)
	}
}

// TestReleaseAdminRepoAnomaliesOpensRepeatableReadReadOnlyTx 覆盖异常视图的事务外观：
// REPEATABLE READ 让版本行与冲突清单看同一快照（CP7），READ ONLY 让误写直接失败（CP2）；
// 顺序固定为「版本折叠 → 冲突清单」，结束后 Rollback 收尾。
func TestReleaseAdminRepoAnomaliesOpensRepeatableReadReadOnlyTx(t *testing.T) {
	windowStart, windowEnd := int16(9), int16(12)
	querier := &releaseAnomalyQuerierStub{
		anomalies: &releaseAdminRowsStub{rows: [][]any{
			releaseAnomalyRow("20261114T030512Z", releaseAdminTestNow(), true, false,
				10, 7, 2, 1, []int32{9, 11}, &windowStart, &windowEnd),
		}},
	}
	tx := &releaseAnomalyTxStub{querier: querier}
	pool := &releaseAdminPoolStub{tx: tx}
	repo := NewReleaseAdminRepo(pool)

	anomalies, err := repo.Anomalies(context.Background())
	if err != nil {
		t.Fatalf("读异常视图失败: %v", err)
	}
	if len(anomalies) != 1 || anomalies[0].ReleaseID != "20261114T030512Z" {
		t.Fatalf("异常视图应经事务体返回: %+v", anomalies)
	}
	if fmt.Sprint(querier.calls) != fmt.Sprint([]string{"anomalies", "conflicts"}) {
		t.Fatalf("应先读版本折叠、再读冲突清单，实际调用 %v", querier.calls)
	}
	if pool.txOptions.IsoLevel != pgx.RepeatableRead {
		t.Fatalf("异常视图事务隔离级别应为 REPEATABLE READ，实际 %v", pool.txOptions.IsoLevel)
	}
	if pool.txOptions.AccessMode != pgx.ReadOnly {
		t.Fatalf("异常视图事务应为 READ ONLY，实际 %v", pool.txOptions.AccessMode)
	}
	if !tx.rolledBack {
		t.Fatal("异常视图事务结束后必须 Rollback 收尾")
	}
}

// TestReleaseAdminRepoAnomaliesScansAllColumns 覆盖异常视图的列序与派生量：
// 未知/复合的 ratio 由折叠格数当分母、周次数组转 int 后给出缺周、窗口两列可空、
// 冲突清单按 rn 贴回版本并置截断标记，没有冲突的版本是空切片。
func TestReleaseAdminRepoAnomaliesScansAllColumns(t *testing.T) {
	windowStart, windowEnd := int16(9), int16(12)
	anomalyRows := &releaseAdminRowsStub{rows: [][]any{
		releaseAnomalyRow("20261114T030512Z", releaseAdminTestNow(), true, false,
			10, 7, 2, 1, []int32{9, 11}, &windowStart, &windowEnd),
		releaseAnomalyRow("20261028T041500Z", releaseAdminTestNow().Add(-17*24*time.Hour), false, true,
			2, 1, 0, 1, []int32{5}, nil, nil),
	}}
	conflictRows := &releaseAdminRowsStub{rows: [][]any{
		releaseAnomalyConflictRow("20261028T041500Z", "z001", "Q10", "Q03", 1),
		releaseAnomalyConflictRow("20261028T041500Z", "z200", "Q10", "Q03", 200),
		releaseAnomalyConflictRow("20261028T041500Z", "z201", "Q10", "Q03", 201), // 第 201 条：只置截断
		releaseAnomalyConflictRow("20200101T000000Z", "ghost", "Q10", "Q03", 1),  // 不在版本集合里：跳过
	}}
	querier := &releaseAnomalyQuerierStub{anomalies: anomalyRows, conflicts: conflictRows}
	tx := &releaseAnomalyTxStub{querier: querier}
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{tx: tx})

	anomalies, err := repo.Anomalies(context.Background())
	if err != nil {
		t.Fatalf("读异常视图失败: %v", err)
	}
	if len(anomalies) != 2 {
		t.Fatalf("夹具应有两个版本，实际 %d 个: %+v", len(anomalies), anomalies)
	}

	current := anomalies[0]
	if !current.IsCurrent || current.HasAttrConflict {
		t.Fatalf("版本标记扫描不符: %+v", current)
	}
	if !current.GeneratedAt.Equal(releaseAdminTestNow()) {
		t.Fatalf("generated_at 扫描不符: %s", current.GeneratedAt)
	}
	if want := (StateRatio{Cells: 2, Ratio: 0.2857}); current.Unknown != want {
		t.Fatalf("unknown 应为 %+v，实际 %+v", want, current.Unknown)
	}
	if want := (StateRatio{Cells: 1, Ratio: 0.1429}); current.Composite != want {
		t.Fatalf("composite 应为 %+v，实际 %+v", want, current.Composite)
	}
	if !reflect.DeepEqual(current.ObservedWeeks, []int{9, 11}) {
		t.Fatalf("observed_weeks 应为 [9 11]，实际 %v", current.ObservedWeeks)
	}
	if !reflect.DeepEqual(current.WeekGaps, []int{10}) {
		t.Fatalf("week_gaps 应命中缺周 10，实际 %v", current.WeekGaps)
	}
	if current.Window == nil || *current.Window != (ReleaseWindow{Start: 9, End: 12}) {
		t.Fatalf("window 应来自关联轮次 9–12，实际 %+v", current.Window)
	}
	if current.ConflictRooms == nil || len(current.ConflictRooms) != 0 {
		t.Fatalf("没有冲突的版本应给出空切片，实际 %+v", current.ConflictRooms)
	}
	if current.ConflictRoomsTruncated {
		t.Fatal("没有冲突的版本不应标记截断")
	}

	previous := anomalies[1]
	if previous.Unknown != (StateRatio{Cells: 0, Ratio: 0}) {
		t.Fatalf("unknown 为 0 时占比也应为 0，实际 %+v", previous.Unknown)
	}
	if previous.Composite != (StateRatio{Cells: 1, Ratio: 1}) {
		t.Fatalf("composite 1/1 应为 1，实际 %+v", previous.Composite)
	}
	if previous.Window != nil {
		t.Fatalf("没有关联轮次的版本 window 应为 nil，实际 %+v", previous.Window)
	}
	if previous.WeekGaps == nil || len(previous.WeekGaps) != 0 {
		t.Fatalf("单周版本应给出空缺口切片，实际 %+v", previous.WeekGaps)
	}
	wantConflicts := []ConflictRoom{
		{RoomID: "z001", BuildingID: "Q10", ConflictBuildingID: "Q03"},
		{RoomID: "z200", BuildingID: "Q10", ConflictBuildingID: "Q03"},
	}
	if !reflect.DeepEqual(previous.ConflictRooms, wantConflicts) {
		t.Fatalf("冲突清单只保留 rn ≤ 200 的房号，got %+v want %+v",
			previous.ConflictRooms, wantConflicts)
	}
	if !previous.ConflictRoomsTruncated {
		t.Fatal("出现第 201 条冲突时应标记截断")
	}

	if !anomalyRows.closed || !conflictRows.closed {
		t.Fatal("两条语句都必须关闭行集")
	}
}

// TestReleaseAdminRepoAnomaliesConflictRoomsExactlyLimit 覆盖截断边界：冲突恰好
// conflictRoomsLimit 条时，SQL 取回的 rn 最大就是 conflictRoomsLimit，不存在第
// conflictRoomsLimit+1 行，所以清单条数等于上限且 conflict_rooms_truncated 为 false
// （Requirement 7.4）。这条与「201 条才截断」互补，防止把 >= 写成 >（或反过来）。
func TestReleaseAdminRepoAnomaliesConflictRoomsExactlyLimit(t *testing.T) {
	anomalyRows := &releaseAdminRowsStub{rows: [][]any{
		releaseAnomalyRow("20261114T030512Z", releaseAdminTestNow(), true, true,
			10, 7, 2, 1, []int32{9}, nil, nil),
	}}
	conflictRows := make([][]any, 0, conflictRoomsLimit)
	for rank := 1; rank <= conflictRoomsLimit; rank++ {
		conflictRows = append(conflictRows, releaseAnomalyConflictRow(
			"20261114T030512Z", fmt.Sprintf("z%03d", rank), "Q10", "Q03", int64(rank)))
	}
	querier := &releaseAnomalyQuerierStub{
		anomalies: anomalyRows,
		conflicts: &releaseAdminRowsStub{rows: conflictRows},
	}
	repo := newAnomaliesRepo(querier)

	anomalies, err := repo.Anomalies(context.Background())
	if err != nil {
		t.Fatalf("读异常视图失败: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("夹具应有一个版本，实际 %d 个: %+v", len(anomalies), anomalies)
	}
	if got := len(anomalies[0].ConflictRooms); got != conflictRoomsLimit {
		t.Fatalf("恰好 %d 条冲突时应全部返回，实际 %d 条", conflictRoomsLimit, got)
	}
	if anomalies[0].ConflictRoomsTruncated {
		t.Fatalf("恰好 %d 条冲突不是截断，不应标记 conflict_rooms_truncated", conflictRoomsLimit)
	}
	if last := anomalies[0].ConflictRooms[conflictRoomsLimit-1].RoomID; last != "z200" {
		t.Fatalf("清单末条应为第 %d 条 z200，实际 %q", conflictRoomsLimit, last)
	}
}

// TestReleaseAdminRepoAnomaliesEmptyIsEmptySlice 覆盖空库：返回非 nil 的空切片，
// 响应里 releases 是 []（Requirement 7.5）。
func TestReleaseAdminRepoAnomaliesEmptyIsEmptySlice(t *testing.T) {
	anomalyRows := &releaseAdminRowsStub{}
	conflictRows := &releaseAdminRowsStub{}
	querier := &releaseAnomalyQuerierStub{anomalies: anomalyRows, conflicts: conflictRows}
	tx := &releaseAnomalyTxStub{querier: querier}
	repo := NewReleaseAdminRepo(&releaseAdminPoolStub{tx: tx})

	anomalies, err := repo.Anomalies(context.Background())
	if err != nil {
		t.Fatalf("空库读异常视图失败: %v", err)
	}
	if anomalies == nil {
		t.Fatal("空库应返回空切片而不是 nil")
	}
	if len(anomalies) != 0 {
		t.Fatalf("空库不应有版本: %+v", anomalies)
	}
	if !anomalyRows.closed || !conflictRows.closed {
		t.Fatal("空结果也要关闭行集")
	}
}

// TestReleaseAdminRepoAnomaliesErrors 覆盖异常视图的失败口径：开事务失败、两条语句的
// 读库失败、扫描失败、遍历到一半失败，全部包 model.ErrQueryFailed（api 映射 50000），
// 且不返回半份视图。
func TestReleaseAdminRepoAnomaliesErrors(t *testing.T) {
	cases := []struct {
		name string
		// build 返回仓储、替身与「冲突清单是否应被读过」的期望。
		build func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool)
	}{
		{
			name: "事务开不起来",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{}
				repo := NewReleaseAdminRepo(&releaseAdminPoolStub{
					beginErr: errors.New("连接池耗尽"),
					tx:       &releaseAnomalyTxStub{querier: querier},
				})
				return repo, querier, false
			},
		},
		{
			name: "版本折叠查询失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{anomaliesErr: errors.New("连接断了")}
				repo := newAnomaliesRepo(querier)
				return repo, querier, false
			},
		},
		{
			name: "版本折叠扫描失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{anomalies: &releaseAdminRowsStub{
					rows:    [][]any{releaseAnomalyRow("20261114T030512Z", releaseAdminTestNow(), true, false, 0, 0, 0, 0, []int32{}, nil, nil)},
					scanErr: errors.New("列类型不符"),
				}}
				return newAnomaliesRepo(querier), querier, false
			},
		},
		{
			name: "版本折叠遍历中途失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{anomalies: &releaseAdminRowsStub{
					rows: [][]any{releaseAnomalyRow("20261114T030512Z", releaseAdminTestNow(), true, false, 0, 0, 0, 0, []int32{}, nil, nil)},
					err:  errors.New("遍历中连接断了"),
				}}
				return newAnomaliesRepo(querier), querier, false
			},
		},
		{
			name: "冲突清单查询失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{
					anomalies:    &releaseAdminRowsStub{},
					conflictsErr: errors.New("连接断了"),
				}
				return newAnomaliesRepo(querier), querier, true
			},
		},
		{
			name: "冲突清单扫描失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{
					anomalies: &releaseAdminRowsStub{},
					conflicts: &releaseAdminRowsStub{
						rows:    [][]any{releaseAnomalyConflictRow("20261028T041500Z", "z001", "Q10", "Q03", 1)},
						scanErr: errors.New("列类型不符"),
					},
				}
				return newAnomaliesRepo(querier), querier, true
			},
		},
		{
			name: "冲突清单遍历中途失败",
			build: func() (ReleaseAdminRepo, *releaseAnomalyQuerierStub, bool) {
				querier := &releaseAnomalyQuerierStub{
					anomalies: &releaseAdminRowsStub{},
					conflicts: &releaseAdminRowsStub{
						rows: [][]any{releaseAnomalyConflictRow("20261028T041500Z", "z001", "Q10", "Q03", 1)},
						err:  errors.New("遍历中连接断了"),
					},
				}
				return newAnomaliesRepo(querier), querier, true
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, querier, wantConflictRead := tc.build()

			anomalies, err := repo.Anomalies(context.Background())
			if !errors.Is(err, model.ErrQueryFailed) {
				t.Fatalf("期望包装 model.ErrQueryFailed，实际 err=%v", err)
			}
			if anomalies != nil {
				t.Fatalf("失败时不应返回半份视图: %+v", anomalies)
			}
			if got := containsString(querier.calls, "conflicts"); got != wantConflictRead {
				t.Fatalf("冲突清单是否被读应为 %t，实际调用 %v", wantConflictRead, querier.calls)
			}
		})
	}
}

// newAnomaliesRepo 用替身拼一个走事务体的仓储；cases 只关心查询替身的内容。
func newAnomaliesRepo(querier *releaseAnomalyQuerierStub) ReleaseAdminRepo {
	tx := &releaseAnomalyTxStub{querier: querier}
	return NewReleaseAdminRepo(&releaseAdminPoolStub{tx: tx})
}
