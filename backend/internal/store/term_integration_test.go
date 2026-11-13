package store

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// openTermTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 与其它集成测试一样会清空 public schema，只能指向专用测试库，
// 且整个包的集成用例必须 -p 1 串行。
func openTermTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	// 没有测试库就跳过，避免本地无 PostgreSQL 时整仓测试变红。
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过真实库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	// 从干净库开始，确保迁移与下面的种子都由本用例负责。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// TestTermWeekAtAgainstRealDB 只在提供 KJS_TEST_DSN 时运行，验证半开区间的边界。
func TestTermWeekAtAgainstRealDB(t *testing.T) {
	ctx, pool := openTermTestDB(t)

	// 造两周：第 5 周周一为 2026-10-19，第 6 周周一为 2026-10-26。
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", "2026-2027-1", 16)
	mustExec(ctx, t, pool, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)", "2026-2027-1", 5, mustDate(t, "2026-10-19"))
	mustExec(ctx, t, pool, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)", "2026-2027-1", 6, mustDate(t, "2026-10-26"))

	cases := []struct {
		name   string
		date   string
		wantOK bool
		wantWk int16
	}{
		{name: "周一当天", date: "2026-10-19", wantOK: true, wantWk: 5},
		{name: "周中", date: "2026-10-22", wantOK: true, wantWk: 5},
		{name: "周日是本周最后一天", date: "2026-10-25", wantOK: true, wantWk: 5},
		{name: "下周一进入下一周", date: "2026-10-26", wantOK: true, wantWk: 6},
		{name: "学期前无覆盖", date: "2026-10-12", wantOK: false},
		{name: "学期后无覆盖", date: "2026-12-31", wantOK: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tw, ok, err := TermWeekAt(ctx, pool, mustDate(t, c.date))
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if ok != c.wantOK {
				t.Fatalf("覆盖判定期望 %v，实际 %v", c.wantOK, ok)
			}
			if ok && tw.Week != c.wantWk {
				t.Fatalf("周次期望 %d，实际 %d", c.wantWk, tw.Week)
			}
		})
	}
}

// termCalendarEntry 是读回来的单周日历：周一日期只比日历日，in_calendar 原样保留。
type termCalendarEntry struct {
	Monday     string
	InCalendar bool
}

// loadTermCalendar 读回 termTestName 的总周数与逐周日历，week 为键，便于整体比较。
// 四个用例都只写这一个学期，学期参数固定，不额外多一个永远不变形参。
func loadTermCalendar(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (int, map[int16]termCalendarEntry) {
	const term = termTestName
	t.Helper()

	var totalWeeks int16
	if err := pool.QueryRow(ctx, "SELECT total_weeks FROM term WHERE term = $1", term).Scan(&totalWeeks); err != nil {
		t.Fatalf("读取学期 %s 的总周数失败: %v", term, err)
	}

	rows, err := pool.Query(ctx, "SELECT week, monday, in_calendar FROM term_week WHERE term = $1 ORDER BY week", term)
	if err != nil {
		t.Fatalf("读取学期 %s 的周历失败: %v", term, err)
	}
	defer rows.Close()

	calendar := make(map[int16]termCalendarEntry)
	for rows.Next() {
		var (
			week       int16
			monday     time.Time
			inCalendar bool
		)
		if err := rows.Scan(&week, &monday, &inCalendar); err != nil {
			t.Fatalf("扫描学期 %s 的周历失败: %v", term, err)
		}
		calendar[week] = termCalendarEntry{Monday: monday.Format(time.DateOnly), InCalendar: inCalendar}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历学期 %s 的周历失败: %v", term, err)
	}
	return int(totalWeeks), calendar
}

// TestReplaceTermFirstWriteCreatesWholeCalendar 覆盖 Requirement 3.1：
// 首次写入在单事务内落 term 与第 1..16 周，每周周一按锚点逐周加 7 天，in_calendar 为 true。
func TestReplaceTermFirstWriteCreatesWholeCalendar(t *testing.T) {
	ctx, pool := openTermTestDB(t)

	firstMonday := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	if err := NewTermRepo(pool).ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now()); err != nil {
		t.Fatalf("首次写入学期日历失败: %v", err)
	}

	totalWeeks, calendar := loadTermCalendar(ctx, t, pool)
	if totalWeeks != 16 {
		t.Fatalf("term.total_weeks 期望 16，实际 %d", totalWeeks)
	}
	if len(calendar) != 16 {
		t.Fatalf("term_week 期望 16 行，实际 %d 行", len(calendar))
	}
	for week := int16(1); week <= 16; week++ {
		entry, ok := calendar[week]
		if !ok {
			t.Fatalf("缺少第 %d 周", week)
		}
		want := firstMonday.AddDate(0, 0, 7*(int(week)-1)).Format(time.DateOnly)
		if entry.Monday != want {
			t.Fatalf("第 %d 周周一期望 %s，实际 %s", week, want, entry.Monday)
		}
		if !entry.InCalendar {
			t.Fatalf("第 %d 周应为教学周（in_calendar=true）", week)
		}
	}
}

// TestReplaceTermRepeatWriteKeepsRowsAndDates 覆盖 Requirement 3.4 与 CP4：
// 同一学期重复写入后行数与周一日期不变。写入前故意篡改第 5 周（周一 +7 天、
// in_calendar=false），用来证明 upsert 是覆盖写而不是 DO NOTHING。
func TestReplaceTermRepeatWriteKeepsRowsAndDates(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)
	firstMonday := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)

	if err := repo.ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now()); err != nil {
		t.Fatalf("首次写入学期日历失败: %v", err)
	}
	wantTotal, wantCalendar := loadTermCalendar(ctx, t, pool)

	mustExec(ctx, t, pool,
		"UPDATE term_week SET monday = monday + 7, in_calendar = false WHERE term = $1 AND week = 5",
		termTestName)

	if err := repo.ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now()); err != nil {
		t.Fatalf("重复写入学期日历失败: %v", err)
	}

	gotTotal, gotCalendar := loadTermCalendar(ctx, t, pool)
	if gotTotal != wantTotal {
		t.Fatalf("重复写入后 term.total_weeks 期望 %d，实际 %d", wantTotal, gotTotal)
	}
	if len(gotCalendar) != 16 {
		t.Fatalf("重复写入后 term_week 期望 16 行，实际 %d 行", len(gotCalendar))
	}
	if !reflect.DeepEqual(wantCalendar, gotCalendar) {
		t.Fatalf("重复写入后周历应逐周不变\n期望: %+v\n实际: %+v", wantCalendar, gotCalendar)
	}
}

// TestReplaceTermShrinkDeletesTailWeeks 覆盖 Requirement 3.1 的「删 week > Y 残留」：
// 学期从 16 周缩到 10 周后，后 6 行被删除，前 10 周的周一日期原样保留。
func TestReplaceTermShrinkDeletesTailWeeks(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)
	firstMonday := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)

	if err := repo.ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now()); err != nil {
		t.Fatalf("首次写入 16 周失败: %v", err)
	}
	_, before := loadTermCalendar(ctx, t, pool)

	if err := repo.ReplaceTerm(ctx, termTestName, 10, firstMonday, time.Now()); err != nil {
		t.Fatalf("缩短为 10 周失败: %v", err)
	}

	totalWeeks, after := loadTermCalendar(ctx, t, pool)
	if totalWeeks != 10 {
		t.Fatalf("term.total_weeks 期望 10，实际 %d", totalWeeks)
	}
	if len(after) != 10 {
		t.Fatalf("term_week 期望 10 行，实际 %d 行", len(after))
	}
	for week := int16(1); week <= 10; week++ {
		if after[week] != before[week] {
			t.Fatalf("第 %d 周不应因缩短而改变：期望 %+v，实际 %+v", week, before[week], after[week])
		}
	}
	for week := int16(11); week <= 16; week++ {
		if _, ok := after[week]; ok {
			t.Fatalf("第 %d 周应在学期缩短后被删除", week)
		}
	}
}

// TestReplaceTermAnchorConflictLeavesCalendarUnchanged 覆盖 Requirement 3.3 与 CP4：
// 已有日历的第 1 周周一与新算值不同时返回 ErrTermAnchorConflict，
// term 与 term_week 一行都不动（这里同时改总周数，确保 term 行也没被提前刷新）。
func TestReplaceTermAnchorConflictLeavesCalendarUnchanged(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)

	if err := repo.ReplaceTerm(ctx, termTestName, 16,
		time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC), time.Now()); err != nil {
		t.Fatalf("首次写入学期日历失败: %v", err)
	}
	wantTotal, wantCalendar := loadTermCalendar(ctx, t, pool)

	err := repo.ReplaceTerm(ctx, termTestName, 18,
		time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC), time.Now())
	if !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("期望 ErrTermAnchorConflict，实际 %v", err)
	}

	gotTotal, gotCalendar := loadTermCalendar(ctx, t, pool)
	if gotTotal != wantTotal {
		t.Fatalf("锚点冲突后 term.total_weeks 应保持 %d，实际 %d", wantTotal, gotTotal)
	}
	if !reflect.DeepEqual(wantCalendar, gotCalendar) {
		t.Fatalf("锚点冲突后周历应一行不变\n期望: %+v\n实际: %+v", wantCalendar, gotCalendar)
	}
}

// seedTermWeeks 先铺 term 行，再按「周次 → YYYY-MM-DD」铺若干 term_week 行。
// 用来造「半截日历」：真实库里只会由历史半途写入留下（term 行在、第 1 周缺失），
// 用整学期写入是造不出这种状态的。
func seedTermWeeks(ctx context.Context, t *testing.T, pool *pgxpool.Pool, totalWeeks int16, mondays map[int16]string) {
	t.Helper()
	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)", termTestName, totalWeeks)
	for week, monday := range mondays {
		mustExec(ctx, t, pool, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)",
			termTestName, week, mustDate(t, monday))
	}
}

// TestReplaceTermHalfCalendarConflicts 钉住「已有日历」不只看第 1 周行（minor 1）：
// term 行在、week = 1 缺失时，按最小周次行反推锚点（09-21 − 7×2 = 09-07），
// 与新算值 08-31 不一致即判冲突，term 与 term_week 一行都不动。
// 若实现只读 week = 1，这里会把半截日历当成首次写入，用新锚点把 16 周整学期覆盖掉。
func TestReplaceTermHalfCalendarConflicts(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)

	seedTermWeeks(ctx, t, pool, 16, map[int16]string{3: "2026-09-21", 4: "2026-09-28"})
	wantTotal, wantCalendar := loadTermCalendar(ctx, t, pool)

	err := repo.ReplaceTerm(ctx, termTestName, 10,
		time.Date(2026, time.August, 31, 0, 0, 0, 0, time.UTC), time.Now())
	if !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("半截日历锚点不一致时期望 ErrTermAnchorConflict，实际 %v", err)
	}

	gotTotal, gotCalendar := loadTermCalendar(ctx, t, pool)
	if gotTotal != wantTotal {
		t.Fatalf("冲突后 term.total_weeks 应保持 %d，实际 %d", wantTotal, gotTotal)
	}
	if !reflect.DeepEqual(wantCalendar, gotCalendar) {
		t.Fatalf("冲突后半截日历应一行不变\n期望: %+v\n实际: %+v", wantCalendar, gotCalendar)
	}
}

// TestReplaceTermHalfCalendarMatchingAnchorFillsMissingWeeks 钉住反推的通过方向（minor 1）：
// 半截日历反推出的锚点与新算值一致时不算冲突，照常写入并把缺的第 1、2 周补齐，
// 已有第 3、4 周日期不变。
func TestReplaceTermHalfCalendarMatchingAnchorFillsMissingWeeks(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)

	seedTermWeeks(ctx, t, pool, 4, map[int16]string{3: "2026-09-21", 4: "2026-09-28"})

	firstMonday := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	if err := repo.ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now()); err != nil {
		t.Fatalf("半截日历锚点一致时应整体替换成功: %v", err)
	}

	totalWeeks, calendar := loadTermCalendar(ctx, t, pool)
	if totalWeeks != 16 {
		t.Fatalf("term.total_weeks 期望 16，实际 %d", totalWeeks)
	}
	if len(calendar) != 16 {
		t.Fatalf("term_week 期望补成 16 行，实际 %d 行", len(calendar))
	}
	for week := int16(1); week <= 16; week++ {
		want := firstMonday.AddDate(0, 0, 7*(int(week)-1)).Format(time.DateOnly)
		if calendar[week].Monday != want {
			t.Fatalf("第 %d 周周一期望 %s，实际 %s", week, want, calendar[week].Monday)
		}
		if !calendar[week].InCalendar {
			t.Fatalf("第 %d 周应为教学周（in_calendar=true）", week)
		}
	}
}

// waitForLockWait 轮询到「另一个后端在等锁」为止，说明 ReplaceTerm 已经读完（看不到的）
// 锚点并卡在抢先事务的未提交行上。不用固定 sleep：固定等待在慢机器上会假通过
// （还没跑到写入就提交了抢先行，用例压根没经过竞争窗口）。
//
// 等待上限取 15s 与用例 ctx 剩余时间的较小值。原来的 5s 硬上限会在 CI 冷启动、
// 连接池首次建连排队或机器负载高时假失败（被测实现没病，只是还没走到写入）；
// 15s 已经远超本地一次 SQL 往返，而真正的死锁或实现卡住最终由 ctx 超时收口，
// 放宽不会把挂死藏起来。ctx 剩余不足 15s 时跟随 ctx，避免用例在 ctx 过期后
// 还在空转轮询、把失败原因拖成一句没信息量的查询错误。
//
// 过滤收窄到目标库与目标语句：只数当前库（current_database()，也就是 DSN 指向的测试库）
// 里正在执行 `INSERT INTO term...` 的活动后端。同一测试库上其它会话的空闲事务、
// autovacuum 或无关表（含读锚点的 SELECT）的锁等待都不该被算成本用例的竞争窗口，
// 否则前提会假成立。用 ltrim 去掉前导空白再匹配：upsertTermSQL / upsertTermFirstWeekSQL
// 常量以换行开头，pg_stat_activity.query 原样保留前导空白，直接
// `query LIKE 'INSERT INTO term%'` 永远匹配不上；注意 btrim/ltrim 单参数默认只去空格、
// 不去换行，所以这里必须显式给出空白字符集。前缀仍只认 INSERT INTO term / term_week。
func waitForLockWait(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	const sql = `SELECT count(*) FROM pg_stat_activity
	             WHERE datname = current_database()
	               AND pid <> pg_backend_pid()
	               AND backend_type = 'client backend'
	               AND state = 'active'
	               AND wait_event_type = 'Lock'
	               AND ltrim(query, E' \t\r\n') LIKE 'INSERT INTO term%'`
	// 本地一次 SQL 往返是毫秒级，15s 只用于容忍 CI 冷启动与连接池建连排队。
	const maxWait = 15 * time.Second
	deadline := time.Now().Add(maxWait)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	budget := time.Until(deadline).Round(time.Millisecond)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, sql).Scan(&waiting); err != nil {
			t.Fatalf("查询锁等待失败: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s（%s 与 ctx 剩余时间取小）仍未观察到 ReplaceTerm 阻塞在未提交的 term 行上，并发用例前提不成立",
				budget, maxWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReplaceTermConcurrentAnchorInsertConflicts 钉住「读锚点 → 写第 1 周」之间的 TOCTOU（minor 2）：
// 另一个事务先插入 term 行与一个周一不同的第 1 周行并保持未提交，ReplaceTerm 的锚点读取
// 看不到它，随后的 term upsert 阻塞；抢先事务提交后，第 1 周 upsert 的 monday 守卫影响 0 行，
// 整事务回滚并返回 ErrTermAnchorConflict——库里只剩抢先事务那一行，本次一行都没留下。
func TestReplaceTermConcurrentAnchorInsertConflicts(t *testing.T) {
	ctx, pool := openTermTestDB(t)
	repo := NewTermRepo(pool)

	firstMonday := time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)
	racingMonday := firstMonday.AddDate(0, 0, 7)

	racer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启抢先事务失败: %v", err)
	}
	// 无论用例怎么结束都释放锁，避免阻塞的 goroutine 把 pool.Close 拖住。
	defer func() { _ = racer.Rollback(ctx) }()
	if _, err := racer.Exec(ctx, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)",
		termTestName, int16(18)); err != nil {
		t.Fatalf("抢先写入 term 行失败: %v", err)
	}
	if _, err := racer.Exec(ctx, "INSERT INTO term_week (term, week, monday) VALUES ($1, $2, $3)",
		termTestName, int16(1), racingMonday); err != nil {
		t.Fatalf("抢先写入第 1 周失败: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- repo.ReplaceTerm(ctx, termTestName, 16, firstMonday, time.Now())
	}()

	waitForLockWait(ctx, t, pool)
	if err := racer.Commit(ctx); err != nil {
		t.Fatalf("提交抢先事务失败: %v", err)
	}

	if err := <-done; !errors.Is(err, ErrTermAnchorConflict) {
		t.Fatalf("并发抢锚点后期望 ErrTermAnchorConflict，实际 %v", err)
	}

	// 冲突必须整体回滚：只剩抢先事务写的第 1 周，没有本次展开的 16 周。
	var rows int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM term_week WHERE term = $1", termTestName).Scan(&rows); err != nil {
		t.Fatalf("统计 term_week 失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("冲突后 term_week 应只剩抢先事务的 1 行，实际 %d 行", rows)
	}
	var storedMonday time.Time
	if err := pool.QueryRow(ctx, "SELECT monday FROM term_week WHERE term = $1 AND week = 1", termTestName).
		Scan(&storedMonday); err != nil {
		t.Fatalf("读取第 1 周失败: %v", err)
	}
	if got := storedMonday.Format(time.DateOnly); got != racingMonday.Format(time.DateOnly) {
		t.Fatalf("第 1 周应保持抢先事务的 %s，实际 %s", racingMonday.Format(time.DateOnly), got)
	}
	var totalWeeks int16
	if err := pool.QueryRow(ctx, "SELECT total_weeks FROM term WHERE term = $1", termTestName).
		Scan(&totalWeeks); err != nil {
		t.Fatalf("读取 term.total_weeks 失败: %v", err)
	}
	if totalWeeks != 18 {
		t.Fatalf("回滚后 term.total_weeks 应保持抢先事务的 18，实际 %d", totalWeeks)
	}
}

// mustDate 解析 YYYY-MM-DD，失败直接终止用例。
func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatalf("解析日期 %s 失败: %v", s, err)
	}
	return d
}
