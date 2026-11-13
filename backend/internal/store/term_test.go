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

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// termTestName 是单测使用的学期编号。
const termTestName = "2026-2027-1"

// termMonday 造单测统一使用的第 1 周周一（2026-09-07，业务时区零点）。
// 只保留一个锚点，避免多出永远不变的参数而被 unparam 判成设计噪音。
func termMonday() time.Time {
	return time.Date(2026, time.September, 7, 0, 0, 0, 0, termZone)
}

// termRowFunc 是 pgx.Row 的函数式替身：测试只关心 Scan 的返回与写入值。
type termRowFunc func(dest ...any) error

// Scan 实现 pgx.Row。
func (f termRowFunc) Scan(dest ...any) error { return f(dest...) }

// termNoRowsRow 模拟「该学期还没有第 1 周行」：首次写入走这条路径。
func termNoRowsRow() pgx.Row {
	return termRowFunc(func(...any) error { return pgx.ErrNoRows })
}

// termAnchorRow 回一条已有周历行（最小周次 + 该周周一），由调用方决定周次。
// 库里是 date，pgx 扫出来是 UTC 零点的 time.Time；week 决定反推锚点时的偏移。
//
// 替身还会检查调用方传进来的 SQL 的形状：不含 `week = 1`（那等于退回「只读第 1 周」，
// 真实库里半截日历会查不到行、被当成首次写入覆盖掉），且带 `week >= 1` 下界
// （挡掉 week = 0 的脏行被 ORDER BY 排到最前）。QueryRow 是唯一能看到 SQL 的地方，
// 所以断言挂在替身上、由 termTxStub.QueryRow 转交（见 termAnchorStub.checkSQL）。
func termAnchorRow(t *testing.T, week int16, monday time.Time) *termAnchorStub {
	t.Helper()
	return &termAnchorStub{t: t, week: week, monday: monday}
}

// termAnchorStub 是「库里已有周历行」的替身，同时携带锚点 SQL 的形状断言。
type termAnchorStub struct {
	t      *testing.T
	week   int16
	monday time.Time
}

// checkSQL 钉住锚点查询的两个 SQL 形状约束：
//  1. 按最小周次取行而不是只读 week = 1——否则半截日历会被当成首次写入；
//  2. 带 week >= 1 下界——week 是 smallint 且没有 CHECK 约束，库里若有误写的
//     week = 0 脏行，它会被 ORDER BY week 排到最前，而 monday − 7×(week−1) 会
//     把锚点推到该行之后，反而把一份明确冲突的日历判成「锚点一致」。
//
// 这两条只能从调用方实际发出的 SQL 看出来，所以断言挂在替身上、由
// termTxStub.QueryRow 转交。用 Errorf 而不是 Fatalf：失败信息已经足够定位，
// 剩下的断言继续跑完还能顺带暴露别的问题。
func (s *termAnchorStub) checkSQL(sql string) {
	s.t.Helper()
	if strings.Contains(sql, "week = 1") || strings.Contains(sql, "week=1") {
		s.t.Errorf("锚点查询不能只读 week = 1，否则半截日历会被当成首次写入；实际 SQL: %s", sql)
	}
	if !strings.Contains(sql, "week >= 1") && !strings.Contains(sql, "week>=1") {
		s.t.Errorf("锚点查询必须带 week >= 1 下界，否则 week = 0 的脏行会把锚点反推到该行之后、把冲突判成一致；实际 SQL: %s", sql)
	}
}

// Scan 实现 pgx.Row：把预置的最小周次与该周周一写进两个扫描目标。
func (s *termAnchorStub) Scan(dest ...any) error {
	if len(dest) != 2 {
		return fmt.Errorf("期望 2 个扫描目标（week、monday），实际 %d 个", len(dest))
	}
	weekTarget, ok := dest[0].(*int16)
	if !ok {
		return fmt.Errorf("期望 *int16，实际 %T", dest[0])
	}
	mondayTarget, ok := dest[1].(*time.Time)
	if !ok {
		return fmt.Errorf("期望 *time.Time，实际 %T", dest[1])
	}
	*weekTarget = s.week
	*mondayTarget = s.monday
	return nil
}

// termErrRow 回固定错误，用来模拟锚点读取失败。
func termErrRow(err error) pgx.Row {
	return termRowFunc(func(...any) error { return err })
}

// termExecStub 记录一条事务内 Exec 的 SQL 与参数，供断言顺序与取值。
type termExecStub struct {
	sql  string
	args []any
}

// termTxStub 是 pgx.Tx 的替身：只实现 QueryRow/Exec/Commit/Rollback，
// 其余方法嵌入接口、调用会 panic。它记录每条语句的顺序与参数，
// 使「先读锚点、后写三条语句」「冲突时一行都不写」能被直接检查到。
type termTxStub struct {
	pgx.Tx
	row pgx.Row

	rowSQL  []string
	rowArgs [][]any
	execs   []termExecStub

	execErrAt int // 第几条 Exec（从 1 起）返回错误；0 表示全部成功
	noRowsAt  int // 第几条 Exec（从 1 起）返回 0 行受影响；0 表示全部影响 1 行
	commitErr error

	commitCalls   int
	rollbackCalls int
}

// QueryRow 记录锚点查询并回放预置行。锚点替身自带 SQL 断言时在这里转交：
// 只有 QueryRow 能看到调用方实际发出的 SQL，替身自己看不到。
func (s *termTxStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.rowSQL = append(s.rowSQL, sql)
	s.rowArgs = append(s.rowArgs, append([]any(nil), args...))
	if anchor, ok := s.row.(*termAnchorStub); ok {
		anchor.checkSQL(sql)
	}
	return s.row
}

// Exec 记录写入语句，并按 execErrAt / noRowsAt 回放失败或「影响 0 行」。
// 影响 0 行对应 PostgreSQL 的 `INSERT 0 0`：第 1 周的 ON CONFLICT ... WHERE 没命中，
// 即另一个事务在读完锚点之后抢先落了不同的周一。
func (s *termTxStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, termExecStub{sql: sql, args: append([]any(nil), args...)})
	if s.execErrAt > 0 && len(s.execs) == s.execErrAt {
		return pgconn.CommandTag{}, errors.New("模拟第 " + fmt.Sprint(s.execErrAt) + " 条写入失败")
	}
	if s.noRowsAt > 0 && len(s.execs) == s.noRowsAt {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// Commit 记录提交次数：成功路径必须恰好一次。
func (s *termTxStub) Commit(context.Context) error {
	s.commitCalls++
	return s.commitErr
}

// Rollback 记录回滚：失败路径必须回滚，成功路径上 defer 的回滚是空操作。
func (s *termTxStub) Rollback(context.Context) error {
	s.rollbackCalls++
	return nil
}

// termQuerierStub 是 termQuerier 的替身：只开事务。
type termQuerierStub struct {
	tx       pgx.Tx
	beginErr error
	begins   int
}

// Begin 记录开启次数，并回放预置事务或错误。
func (s *termQuerierStub) Begin(context.Context) (pgx.Tx, error) {
	s.begins++
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return s.tx, nil
}

// TestReplaceTermWritesThreeStepsInOrder 钉住事务体里唯一允许的写法：
// 先读最小周次行反推锚点，再 UPSERT term、写第 1 周（带 monday 守卫）、
// 逐周 UPSERT 第 2..Y 周、最后删 week > Y 的残留。
// 单测只验语句顺序、参数与提交次数；SQL 的真实语义由真实库集成用例覆盖。
func TestReplaceTermWritesThreeStepsInOrder(t *testing.T) {
	tx := &termTxStub{row: termNoRowsRow()}
	q := &termQuerierStub{tx: tx}
	firstMonday := termMonday()

	if err := NewTermRepo(q).ReplaceTerm(context.Background(), "  "+termTestName+"  ", 16, firstMonday, time.Time{}); err != nil {
		t.Fatalf("首次写入不应报错: %v", err)
	}

	if q.begins != 1 {
		t.Fatalf("应开一个事务，实际 %d 次", q.begins)
	}
	if len(tx.rowSQL) != 1 {
		t.Fatalf("事务里应只读一次锚点，实际 %d 次", len(tx.rowSQL))
	}
	// 锚点查询必须按最小周次取一行：只读 week = 1 会把半截日历当成首次写入。
	if !strings.Contains(tx.rowSQL[0], "ORDER BY week") || !strings.Contains(tx.rowSQL[0], "LIMIT 1") {
		t.Fatalf("锚点查询应按最小周次取一行，实际 SQL: %s", tx.rowSQL[0])
	}
	if len(tx.rowArgs[0]) != 1 || tx.rowArgs[0][0] != termTestName {
		t.Fatalf("锚点查询应按去空白后的学期读，实际参数 %+v", tx.rowArgs[0])
	}

	// 1 条 UPSERT term + 16 条 UPSERT term_week + 1 条 DELETE 残留。
	if want := 1 + 16 + 1; len(tx.execs) != want {
		t.Fatalf("应写 %d 条语句，实际 %d 条", want, len(tx.execs))
	}

	termWrite := tx.execs[0]
	if !strings.Contains(termWrite.sql, "INSERT INTO term ") || !strings.Contains(termWrite.sql, "total_weeks") {
		t.Fatalf("第一条应是 term 的 upsert，实际: %s", termWrite.sql)
	}
	if len(termWrite.args) != 2 || termWrite.args[0] != termTestName || termWrite.args[1] != int16(16) {
		t.Fatalf("term 的写入参数应为学期与总周数，实际 %+v", termWrite.args)
	}

	for week := 1; week <= 16; week++ {
		write := tx.execs[week] // 下标 0 已被 term 占用
		if !strings.Contains(write.sql, "INSERT INTO term_week") || !strings.Contains(write.sql, "in_calendar") {
			t.Fatalf("第 %d 周的语句不是 term_week 的 upsert: %s", week, write.sql)
		}
		// 只有第 1 周带 monday 守卫：它负责消掉「读锚点 → 写第 1 周」之间的并发窗口；
		// 第 2 周起不带守卫，否则「上次把第 N 周写歪了」永远修不回来。
		guarded := strings.Contains(write.sql, "term_week.monday = EXCLUDED.monday")
		if week == 1 && !guarded {
			t.Fatalf("第 1 周的 upsert 应带 monday 守卫，实际: %s", write.sql)
		}
		if week > 1 && guarded {
			t.Fatalf("第 %d 周的 upsert 不应带 monday 守卫，实际: %s", week, write.sql)
		}
		if len(write.args) != 3 {
			t.Fatalf("第 %d 周应有 3 个参数，实际 %+v", week, write.args)
		}
		if write.args[0] != termTestName || write.args[1] != int16(week) {
			t.Fatalf("第 %d 周的学期/周次参数不对: %+v", week, write.args)
		}
		want := firstMonday.AddDate(0, 0, 7*(week-1))
		if monday, ok := write.args[2].(time.Time); !ok || !monday.Equal(want) {
			t.Fatalf("第 %d 周周一期望 %s，实际 %+v", week, want.Format(time.DateOnly), write.args[2])
		}
	}

	deleteWrite := tx.execs[len(tx.execs)-1]
	if !strings.Contains(deleteWrite.sql, "DELETE FROM term_week") || !strings.Contains(deleteWrite.sql, "week >") {
		t.Fatalf("最后一条应删尾部残留: %s", deleteWrite.sql)
	}
	if len(deleteWrite.args) != 2 || deleteWrite.args[0] != termTestName || deleteWrite.args[1] != int16(16) {
		t.Fatalf("删除残留的参数应为学期与总周数，实际 %+v", deleteWrite.args)
	}

	if tx.commitCalls != 1 {
		t.Fatalf("应提交恰好一次，实际 %d 次", tx.commitCalls)
	}
}

// TestReplaceTermAcceptsConsistentAnchor 钉住「已有日历且锚点一致」继续整体替换：
// 第 1 周周一按日历日比较，库里是 UTC 零点的 date、调用方传业务时区的时刻，不判冲突。
func TestReplaceTermAcceptsConsistentAnchor(t *testing.T) {
	stored := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	tx := &termTxStub{row: termAnchorRow(t, 1, stored)}
	q := &termQuerierStub{tx: tx}

	if err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{}); err != nil {
		t.Fatalf("锚点一致时不应报错: %v", err)
	}
	if len(tx.execs) != 18 {
		t.Fatalf("锚点一致时应照常写 18 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 1 {
		t.Fatalf("应提交恰好一次，实际 %d 次", tx.commitCalls)
	}
}

// TestReplaceTermRejectsAnchorConflict 钉住锚点守卫：不一致时整事务回滚，
// 一行都不写，返回 store.ErrTermAnchorConflict（调用方据此映射 calendar_invalid）。
func TestReplaceTermRejectsAnchorConflict(t *testing.T) {
	stored := time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC)
	tx := &termTxStub{row: termAnchorRow(t, 1, stored)}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 18,
		termMonday(), time.Time{})
	if !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("期望 ErrTermAnchorConflict，实际 %v", err)
	}
	if len(tx.execs) != 0 {
		t.Fatalf("锚点冲突时不应写任何行，实际写了 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatalf("锚点冲突时不能提交，实际提交 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("锚点冲突时应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermRejectsHalfCalendarAnchorConflict 钉住「已有日历」不只看第 1 周行：
// 库里只有第 3 周行（term 行在、week = 1 缺失）时，按最小周次行反推
// 第 1 周周一（09-21 − 7×2 = 09-07），再与新算值 08-31 比较，判锚点冲突且一行不写。
// 若实现只读 week = 1，这里会被当成首次写入而把半截日历覆盖掉，所以必须报错。
func TestReplaceTermRejectsHalfCalendarAnchorConflict(t *testing.T) {
	halfWeekMonday := time.Date(2026, time.September, 21, 0, 0, 0, 0, time.UTC)
	tx := &termTxStub{row: termAnchorRow(t, 3, halfWeekMonday)}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		time.Date(2026, time.August, 31, 0, 0, 0, 0, termZone), time.Time{})
	if !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("期望 ErrTermAnchorConflict，实际 %v", err)
	}
	if errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("锚点冲突不应算成仓储故障，实际 %v", err)
	}
	if len(tx.execs) != 0 {
		t.Fatalf("半截日历锚点冲突时不应写任何行，实际写了 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatalf("半截日历锚点冲突时不能提交，实际提交 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("半截日历锚点冲突时应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermAcceptsHalfCalendarMatchingAnchor 钉住反推的另一个方向：
// 只有第 3 周行、但反推出的锚点与新算值一致时不算冲突，照常补齐第 1..Y 周。
func TestReplaceTermAcceptsHalfCalendarMatchingAnchor(t *testing.T) {
	tx := &termTxStub{row: termAnchorRow(t, 3, termMonday().AddDate(0, 0, 14))}
	q := &termQuerierStub{tx: tx}

	if err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{}); err != nil {
		t.Fatalf("半截日历锚点一致时不应报错: %v", err)
	}
	// 1 条 term + 16 条 term_week + 1 条 DELETE：缺的第 1、2 周被补上。
	if len(tx.execs) != 18 {
		t.Fatalf("半截日历锚点一致时应照常写 18 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 1 {
		t.Fatalf("应提交恰好一次，实际 %d 次", tx.commitCalls)
	}
}

// TestReplaceTermRejectsConcurrentAnchorChange 钉住「读锚点 → 写第 1 周」之间的 TOCTOU：
// 第 1 周 upsert 影响 0 行（另一事务抢先落了不同的周一）即按锚点冲突回滚，
// 不继续写第 2..Y 周、不提交。
func TestReplaceTermRejectsConcurrentAnchorChange(t *testing.T) {
	// 第 2 条 Exec 是第 1 周的 upsert（第 1 条是 term 的 upsert）。
	tx := &termTxStub{row: termNoRowsRow(), noRowsAt: 2}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("期望 ErrTermAnchorConflict，实际 %v", err)
	}
	if errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("并发锚点冲突不应算成仓储故障，实际 %v", err)
	}
	if len(tx.execs) != 2 {
		t.Fatalf("第 1 周守卫失败后应立即停下，实际写了 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatalf("第 1 周守卫失败时不能提交，实际提交 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("第 1 周守卫失败时应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermRejectsInvalidWeeks 钉住开事务前的护栏：空学期、越界总周数、
// 零值或非周一的第 1 周周一都不开事务、不写行，返回 model.ErrTermInvalid（入参错误），
// 而不是仓储故障。零值 time.Time{} 的日历日恰好是周一，所以它必须被 IsZero 单独挡下。
func TestReplaceTermRejectsInvalidWeeks(t *testing.T) {
	cases := []struct {
		name        string
		term        string
		totalWeeks  int
		firstMonday time.Time
	}{
		{name: "空学期", term: "   ", totalWeeks: 16, firstMonday: termMonday()},
		{name: "零周", term: termTestName, totalWeeks: 0, firstMonday: termMonday()},
		{name: "负数周", term: termTestName, totalWeeks: -1, firstMonday: termMonday()},
		{name: "53 周超业务上限", term: termTestName, totalWeeks: 53, firstMonday: termMonday()},
		{name: "锚点零值时间", term: termTestName, totalWeeks: 16},
		{name: "锚点不是周一", term: termTestName, totalWeeks: 16, firstMonday: termMonday().AddDate(0, 0, 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx := &termTxStub{row: termNoRowsRow()}
			q := &termQuerierStub{tx: tx}

			err := NewTermRepo(q).ReplaceTerm(context.Background(), c.term, c.totalWeeks,
				c.firstMonday, time.Time{})
			if !errors.Is(err, model.ErrTermInvalid) {
				t.Fatalf("期望包装 ErrTermInvalid，实际 %v", err)
			}
			if errors.Is(err, model.ErrTermStoreFailed) {
				t.Fatalf("入参错误不应算成仓储故障，实际 %v", err)
			}
			if q.begins != 0 {
				t.Fatalf("非法入参不应开事务，实际开 %d 次", q.begins)
			}
			if len(tx.rowSQL) != 0 || len(tx.execs) != 0 {
				t.Fatalf("非法入参不应发任何语句，实际读 %d 条、写 %d 条", len(tx.rowSQL), len(tx.execs))
			}
		})
	}
}

// TestReplaceTermAcceptsMaxWeeks 钉住上界本身合法：52 周（一年最多 52 周）能正常写入，
// 53 才拒绝；两条一起看才说明边界是闭区间 1..52。
func TestReplaceTermAcceptsMaxWeeks(t *testing.T) {
	tx := &termTxStub{row: termNoRowsRow()}
	q := &termQuerierStub{tx: tx}

	if err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 52,
		termMonday(), time.Time{}); err != nil {
		t.Fatalf("52 周应合法: %v", err)
	}
	if q.begins != 1 {
		t.Fatalf("52 周应开一个事务，实际 %d 次", q.begins)
	}
	// 1 条 term + 52 条 term_week + 1 条 DELETE。
	if len(tx.execs) != 54 {
		t.Fatalf("52 周应写 54 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 1 {
		t.Fatalf("应提交恰好一次，实际 %d 次", tx.commitCalls)
	}
}

// TestReplaceTermRollsBackOnWriteFailure 钉住中途失败的整体回滚：
// 任何一条写入失败都不提交，错误包装 model.ErrTermStoreFailed，由调用方按无码失败收口。
func TestReplaceTermRollsBackOnWriteFailure(t *testing.T) {
	tx := &termTxStub{row: termNoRowsRow(), execErrAt: 3}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if !errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("期望包装 ErrTermStoreFailed，实际 %v", err)
	}
	if tx.commitCalls != 0 {
		t.Fatalf("写入失败时不能提交，实际提交 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("写入失败时应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermRollsBackWhenTermUpsertFails 钉住第一条写入（term 的 upsert）失败：
// 立即回滚、零提交、不写任何周行，错误包装 model.ErrTermStoreFailed 而不是锚点冲突。
func TestReplaceTermRollsBackWhenTermUpsertFails(t *testing.T) {
	tx := &termTxStub{row: termNoRowsRow(), execErrAt: 1}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if !errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("期望包装 ErrTermStoreFailed，实际 %v", err)
	}
	if errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("写入失败不应算成锚点冲突，实际 %v", err)
	}
	if len(tx.execs) != 1 {
		t.Fatalf("term upsert 失败后应立即停下，实际写了 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatalf("term upsert 失败时不能提交，实际提交 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("term upsert 失败时应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermFailsWhenCommitFails 钉住提交失败不可宣称成功：
// Commit 报错时返回包装 model.ErrTermStoreFailed 的非 nil 错误（不能因为
// 「写都写完了」就返回 nil），并且仍走 defer 回滚收尾。
func TestReplaceTermFailsWhenCommitFails(t *testing.T) {
	tx := &termTxStub{row: termNoRowsRow(), commitErr: errors.New("模拟提交失败")}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if err == nil {
		t.Fatal("提交失败必须返回错误，不能宣称写入成功")
	}
	if !errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("期望包装 ErrTermStoreFailed，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "提交失败") {
		t.Fatalf("错误应指出提交阶段失败，实际 %v", err)
	}
	if len(tx.execs) != 18 {
		t.Fatalf("提交前应已写满 18 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 1 {
		t.Fatalf("应尝试提交一次，实际 %d 次", tx.commitCalls)
	}
	if tx.rollbackCalls != 1 {
		t.Fatalf("提交失败后应回滚一次，实际 %d 次", tx.rollbackCalls)
	}
}

// TestReplaceTermFailsWhenAnchorReadFails 钉住「读锚点失败不降级成首次写入」：
// 读库错误必须上抛，不能当成「还没有日历」继续覆盖。
func TestReplaceTermFailsWhenAnchorReadFails(t *testing.T) {
	tx := &termTxStub{row: termErrRow(errors.New("模拟读锚点失败"))}
	q := &termQuerierStub{tx: tx}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if !errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("期望包装 ErrTermStoreFailed，实际 %v", err)
	}
	if len(tx.execs) != 0 {
		t.Fatalf("读锚点失败时不应写任何行，实际写了 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatalf("读锚点失败时不能提交，实际提交 %d 次", tx.commitCalls)
	}
}

// TestReplaceTermFailsWhenBeginFails 钉住开事务失败的错误口径。
func TestReplaceTermFailsWhenBeginFails(t *testing.T) {
	q := &termQuerierStub{beginErr: errors.New("模拟连接失败")}

	err := NewTermRepo(q).ReplaceTerm(context.Background(), termTestName, 16,
		termMonday(), time.Time{})
	if !errors.Is(err, model.ErrTermStoreFailed) {
		t.Fatalf("期望包装 ErrTermStoreFailed，实际 %v", err)
	}
}
