package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// rowQuerier 只取单行查询能力，便于把「按日期定位周」与驱动的其余部分解耦。
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// termQuerier 是学期日历写入需要的最小能力：整体替换必须在单事务内完成，
// 所以这里只开事务，事务内的读写由 pgx.Tx 承担（*pgxpool.Pool 直接满足）。
type termQuerier interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrTermAnchorConflict 表示库里已有该学期的日历，且由已存周行反推出的第 1 周周一
// 与新算值不同。
//
// 锚点决定「日期 → 周次」的换算，而 observation.week 存的是周次：静默改锚点会让
// 已经发布的版本从此指向错误的日期。所以这里不改任何行、整事务回滚，由调用方把
// 本轮按 calendar_invalid 收口（Requirement 3.3、design D「锚点冲突不改历史」）。
//
// 两种情形都归到这里：一是开事务时读到的已存周行与传入锚点不一致；二是读完锚点之后、
// 写第 1 周之前被另一个事务抢先落了不同的周一（第 1 周 upsert 的影响行数为 0）。
var ErrTermAnchorConflict = errors.New("学期日历锚点与已存日历不一致")

// maxTermWeeks 是学期总周数的业务上限：一年最多 52 周，与 parser.maxSemesterWeeks 同一口径。
// parser 解析周历页时已经挡掉 Y > 52，这里再挡一次是仓储入口的自我防护：
// ReplaceTerm 是 store 的对外方法，不能假设调用方一定经过 parser。
const maxTermWeeks = 52

// termAnchorSQL 读已有日历里周次最小的那一行，返回 (week, monday)。
// 只读 week = 1 会把「term 行在、第 1 周行缺失」的半截日历误当成首次写入，
// 从而按新算值把已有日期整学期覆盖掉；取最小周次行后，完整日历与半截日历
// 都能反推出同一个锚点（week = 1 时就是该行日期本身）。
// week >= 1 这道下界只防脏数据：week 是 smallint，没有 CHECK 约束，
// 历史误写的 0 或负数会让 monday − 7×(week−1) 把锚点推到该行之后，
// 反而把一个明确冲突的日历判成「锚点一致」。合法行（1..52）全部保留。
// 没有这一行时按「还没有可用锚点」处理：首次写入，或 term 行在但一行周历都没有——
// 后者没有任何日期可被改错，刷新 total_weeks 即可。
const termAnchorSQL = `SELECT week, monday FROM term_week WHERE term = $1 AND week >= 1 ORDER BY week LIMIT 1`

// termZone 是学期日历的业务时区。与 pipeline / upstream / query 同一口径用固定
// +08:00 而不 LoadLocation：2026 年之后没有夏令时，也免去容器缺 tzdata 的分支。
var termZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// upsertTermSQL 刷新学期总周数。只改 total_weeks，不碰 timezone：
// 学期日历的时区口径固定在业务侧，不由某一次采集的执行结果决定。
const upsertTermSQL = `
INSERT INTO term (term, total_weeks)
VALUES ($1, $2)
ON CONFLICT (term) DO UPDATE
   SET total_weeks = EXCLUDED.total_weeks`

// termAnchorSQL（见上）之后是三条写入语句，顺序固定：term → 第 1 周 → 第 2..Y 周。

// upsertTermFirstWeekSQL 是第 1 周的 upsert，比其余周多一条 WHERE 守卫：
// 只有已存第 1 周的周一与本次要写的周一完全相同时才更新。另一事务若在
// 「读锚点」与「写第 1 周」之间抢先把不同的周一落库，这条语句影响 0 行，
// replaceTermTx 据此判 ErrTermAnchorConflict 并回滚（消掉读-写之间的 TOCTOU）。
// 影响行数为 1 同时覆盖两种正常情形：首次插入（INSERT 1 行）与锚点一致的重写（UPDATE 1 行）。
const upsertTermFirstWeekSQL = `
INSERT INTO term_week (term, week, monday, in_calendar)
VALUES ($1, $2, $3, true)
ON CONFLICT (term, week) DO UPDATE
   SET monday = EXCLUDED.monday,
       in_calendar = EXCLUDED.in_calendar
 WHERE term_week.monday = EXCLUDED.monday`

// upsertTermWeekSQL 逐周覆盖周一日期。ON CONFLICT 必须改成新值而不是 DO NOTHING：
// 学期总周数不变时日历也要能被修正（上一次可能只写了半截或标记过非日历周），
// in_calendar 一并恢复成 true，因为第 1..Y 周本来就都是教学周。
// 第 2 周起刻意不带 monday 守卫：这些日期由第 1 周推出，第 1 周一致即整学期一致；
// 带上守卫反而会让「上次把第 N 周写歪了」永远修不回来（重复写入用例钉住这条修复路径）。
//
// 并发保证的边界：第 2..Y 周不带守卫，只保证「本实现的串行重复写入」与
// 「两个 ReplaceTerm 之间」的并发——两者都先比对锚点，且第 1 周 upsert 带 monday 守卫，
// 所以并发调用不会各自写出半截日历并都提交。外部/人工写库造成的中间态不在保证范围：
// 本语句只按本次锚点把第 1..Y 周拉正，既不识别也不回滚外部写入留下的其它行；
// 若外部只写了部分周行且反推出的锚点不同，由上面的锚点比对判冲突、整事务回滚。
const upsertTermWeekSQL = `
INSERT INTO term_week (term, week, monday, in_calendar)
VALUES ($1, $2, $3, true)
ON CONFLICT (term, week) DO UPDATE
   SET monday = EXCLUDED.monday,
       in_calendar = EXCLUDED.in_calendar`

// deleteTermWeeksAfterSQL 删掉学期变短后残留的尾部周。
// 放在逐周 upsert 之后：先保证 1..Y 齐全，再按同一个 Y 收口，顺序反过来会让
// 「先删后写」在事务中途失败时多一次无用写入，也让失败原因更难定位。
const deleteTermWeeksAfterSQL = `DELETE FROM term_week WHERE term = $1 AND week > $2`

// TermWeekAt 查出覆盖给定业务日期的学期周；没有覆盖时 ok=false 而不是报错。
func TermWeekAt(ctx context.Context, q rowQuerier, date time.Time) (model.TermWeek, bool, error) {
	// 一周覆盖 [monday, monday+7)，用半开区间避免边界日重复命中。
	const sql = `SELECT term, week, monday, in_calendar
	             FROM term_week
	             WHERE $1::date >= monday AND $1::date < monday + 7
	             ORDER BY term DESC
	             LIMIT 1`

	var tw model.TermWeek
	err := q.QueryRow(ctx, sql, date).Scan(&tw.Term, &tw.Week, &tw.Monday, &tw.InCalendar)
	// 查不到属于正常业务情形（寒暑假、学期外的日期），不算错误。
	if errors.Is(err, pgx.ErrNoRows) {
		return model.TermWeek{}, false, nil
	}
	if err != nil {
		return model.TermWeek{}, false, fmt.Errorf("%w: 按日期定位学期周: %w", model.ErrQueryFailed, err)
	}
	return tw, true, nil
}

// TermRepo 把查询器闭在内部，方法集与 collector.TermCalendar 结构匹配：
// 装配层用 var _ collector.TermCalendar = store.TermRepo{} 在编译期钉住。
type TermRepo struct {
	q termQuerier
}

// NewTermRepo 绑定一个能开事务的查询器，通常是 *pgxpool.Pool。
func NewTermRepo(q termQuerier) TermRepo {
	return TermRepo{q: q}
}

// ReplaceTerm 在单事务内整体替换某学期的日历：UPSERT term → 逐周 UPSERT
// term_week(week, monday, in_calendar=true) → 删掉 week > Y 的残留行。
//
// 事务里第一件事是读已有日历里周次最小的那一行，并按 monday − 7×(week−1) 反推
// 第 1 周周一：与传入锚点不同即返回 ErrTermAnchorConflict 并回滚，term 与 term_week
// 一行都不动（Requirement 3.3）。取最小周次行而不是只读 week = 1，是为了让
// 「term 行在、第 1 周行缺失」的半截日历也按已有锚点判定，而不是当成首次写入覆盖。
// 比较按「日历日」而不是时刻：库里是 date，调用方传的是带时区的时刻，
// 先把两边都折成 Asia/Shanghai 的日期再比，跨时区调用不会误判成冲突。
// 读完锚点到写完第 1 周之间还有一次并发窗口，由第 1 周 upsert 的 monday 守卫兜住：
// 影响 0 行同样是锚点冲突（见 upsertTermFirstWeekSQL）。
//
// firstMonday 由调用方按「采样日所在周周一 − 7×(C−1)」算好；仓储只校验它是周一，
// 然后展开成第 1..Y 周并落库，不重算锚点（锚点纯函数在 collector，design 第 4 节）。
// 签名里的 now 是消费侧 TermCalendar 接口的统一参数；term / term_week 没有时间列，
// 所以这里不落库，用 _ 占位以免给出「写了时间戳」的错觉。
//
// 入参在开事务之前先校验：空学期、总周数不在 1..52、第 1 周周一为零值或不是周一，
// 一律返回 model.ErrTermInvalid，一行不写也不白开事务。
// model.ErrTermStoreFailed 只留给 Begin / Exec / Commit / 读锚点失败。
func (r TermRepo) ReplaceTerm(ctx context.Context, term string, totalWeeks int, firstMonday, _ time.Time) error {
	term = strings.TrimSpace(term)
	// 空学期与越界周数是调用方的编码错误：先于开事务拒绝，不留半套日历、也不白开事务。
	// 上界与 parser 的 52 对齐，不依赖 smallint 的 MaxInt16：53 周的学期在业务上不存在，
	// 放行只会被下游当成数据错。
	if term == "" {
		return fmt.Errorf("%w: 学期编号为空，无法写入学期日历", model.ErrTermInvalid)
	}
	if totalWeeks < 1 || totalWeeks > maxTermWeeks {
		return fmt.Errorf("%w: 学期 %s 的总周数 %d 超出 1..%d 的范围",
			model.ErrTermInvalid, term, totalWeeks, maxTermWeeks)
	}
	// 锚点必须是一个真实的周一：第 2..Y 周由它按 7 天等差展开，起点不是周一
	// 会让整张 term_week 的周一列全部错位，而锚点冲突守卫只比对已存行，挡不住首次写入。
	// 零值 time.Time{} 必须单独判：它的日历日是 0001-01-01，恰好是周一，Weekday 检查放不过它。
	// 这两条都是调用方的编码错误（collector.anchorMonday 保证输出周一），与总周数同口径。
	if firstMonday.IsZero() {
		return fmt.Errorf("%w: 学期 %s 的第 1 周周一为零值，无法展开周历", model.ErrTermInvalid, term)
	}
	if termDay(firstMonday).Weekday() != time.Monday {
		return fmt.Errorf("%w: 学期 %s 的第 1 周周一 %s 不是周一，无法展开周历",
			model.ErrTermInvalid, term, termDay(firstMonday).Format(time.DateOnly))
	}

	tx, err := r.q.Begin(ctx)
	if err != nil {
		return fmt.Errorf("%w: 学期日历事务开启失败: %w", model.ErrTermStoreFailed, err)
	}
	// 提交成功后再回滚是空操作；所有失败路径都靠这里收尾（与 ApplySettings / CommitRelease 同一写法）。
	defer func() { _ = tx.Rollback(ctx) }()

	if err := replaceTermTx(ctx, tx, term, totalWeeks, firstMonday); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: 学期日历事务提交失败: %w", model.ErrTermStoreFailed, err)
	}
	return nil
}

// replaceTermTx 是 ReplaceTerm 的事务体：先读最小周次行反推锚点、再写 term、
// 写第 1 周（带并发守卫）、逐周写第 2..Y 周、最后删尾部。
// 拆出来是为了让「锚点不一致时一行都不写」「第 1 周被并发改写时回滚」能用记录型
// 事务替身单独验证，不必连库（真实 SQL 语义由集成测试覆盖）。
func replaceTermTx(ctx context.Context, tx pgx.Tx, term string, totalWeeks int, firstMonday time.Time) error {
	var (
		storedWeek   int16
		storedMonday time.Time
	)
	err := tx.QueryRow(ctx, termAnchorSQL, term).Scan(&storedWeek, &storedMonday)
	switch {
	case err == nil:
		// 用最小周次行反推第 1 周周一：week = 1 时就是该行本身；半截日历（缺第 1 周行）
		// 时按 monday − 7×(week−1) 折回，这样半截日历也会被比对，而不是被当成首次写入。
		storedAnchor := termAnchorFromStored(storedMonday, int(storedWeek))
		if !sameTermDay(storedAnchor, firstMonday) {
			return fmt.Errorf("%w: 学期 %s 已存第 %d 周周一为 %s（反推第 1 周周一为 %s），新算值为 %s",
				ErrTermAnchorConflict, term, storedWeek,
				storedMonday.Format(time.DateOnly), storedAnchor.Format(time.DateOnly),
				termDay(firstMonday).Format(time.DateOnly))
		}
	case errors.Is(err, pgx.ErrNoRows):
		// 还没有可用锚点：首次写入，或者上次只写了 term 行、一行周历都没有。
		// 后者没有任何日期可被改错，按新算值整学期写入即可。
	default:
		return fmt.Errorf("%w: 读取学期 %s 的日历锚点失败: %w", model.ErrTermStoreFailed, term, err)
	}

	// smallint 列显式传 int16：pgx 对 Go int 的默认编码宽度与 smallint 不保证匹配。
	if _, err := tx.Exec(ctx, upsertTermSQL, term, int16(totalWeeks)); err != nil {
		return fmt.Errorf("%w: 刷新学期 %s 的总周数失败: %w", model.ErrTermStoreFailed, term, err)
	}

	// 第 1 周单独写：它同时承担「读到的锚点没在这期间被并发换掉」的守卫。
	// 影响 0 行只可能是另一个事务在上面的读之后落了不同的周一，按锚点冲突收口。
	firstTag, err := tx.Exec(ctx, upsertTermFirstWeekSQL, term, int16(1), termWeekMonday(firstMonday, 1))
	if err != nil {
		return fmt.Errorf("%w: 写入学期 %s 第 1 周失败: %w", model.ErrTermStoreFailed, term, err)
	}
	if firstTag.RowsAffected() == 0 {
		return fmt.Errorf("%w: 学期 %s 第 1 周在读锚点之后被并发改写，第 1 周周一与新算值 %s 不一致",
			ErrTermAnchorConflict, term, termDay(firstMonday).Format(time.DateOnly))
	}

	for week := 2; week <= totalWeeks; week++ {
		if _, err := tx.Exec(ctx, upsertTermWeekSQL, term, int16(week), termWeekMonday(firstMonday, week)); err != nil {
			return fmt.Errorf("%w: 写入学期 %s 第 %d 周失败: %w", model.ErrTermStoreFailed, term, week, err)
		}
	}
	if _, err := tx.Exec(ctx, deleteTermWeeksAfterSQL, term, int16(totalWeeks)); err != nil {
		return fmt.Errorf("%w: 清理学期 %s 第 %d 周之后的残留失败: %w", model.ErrTermStoreFailed, term, totalWeeks+1, err)
	}
	return nil
}

// termAnchorFromStored 把库里的某条周历行反推成第 1 周周一：monday − 7×(week−1)。
// 与 termWeekMonday 互逆：前者折回锚点，后者由锚点展开某一周。week 只由
// termAnchorSQL 的 week >= 1 与 ORDER BY week LIMIT 1 给出，恒 ≥ 1。
func termAnchorFromStored(monday time.Time, week int) time.Time {
	return termDay(monday).AddDate(0, 0, -7*(week-1))
}

// termDay 把时刻折算成业务时区（Asia/Shanghai，固定 +08:00）的日历日零点。
// 先 In 再取年月日：上游与 pipeline 传进来的时刻可能带 UTC 或本地时区，
// 直接取对方的日期会在跨零点的那几个小时上差一天。
func termDay(at time.Time) time.Time {
	local := at.In(termZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, termZone)
}

// termWeekMonday 返回第 week 周的周一：第 1 周周一加 7×(week−1) 天。
// 业务时区是固定偏移、没有夏令时，AddDate 不会跳日或重复。
func termWeekMonday(firstMonday time.Time, week int) time.Time {
	return termDay(firstMonday).AddDate(0, 0, 7*(week-1))
}

// sameTermDay 只比日历日，不比时刻：库里存 date，调用方传的是任意时区的时刻，
// 两者折成 Asia/Shanghai 的日期后相等即视为同一个锚点。
func sameTermDay(stored, want time.Time) bool {
	day := termDay(want)
	year, month, date := stored.Date()
	return year == day.Year() && month == day.Month() && date == day.Day()
}
