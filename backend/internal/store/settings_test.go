package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// settingsRowsStub 是 pgx.Rows 的替身：只覆盖 LoadSettings 用到的 Next/Scan/Err/Close，
// 其余方法嵌入接口、调用会 panic——替身只放行被测路径，测错方法立刻暴露。
//
// Scan 照 adminRowStub 的写法真的往 Scan 目标里赋值并校验列数与类型：只会回 nil 的
// 替身能把「仓储按错顺序/错类型扫这四列」照样放过去，而那正是只在真实库上才炸的错误。
type settingsRowsStub struct {
	pgx.Rows
	rows    [][]any // 依次回放的行，每行按 Scan 目标的列顺序给值
	current int     // 已被 Next 取出的行数，也是当前行在 rows 里的下标 +1

	scanErr   error // 第 scanErrAt 次 Scan 回放的扫描错误（有行，但这一行解不出来）
	scanErrAt int   // 第几次 Scan（从 1 起）才失败；0 表示不注入扫描错误
	scanCalls int   // Scan 已调用的次数

	err error // Err 回放的遍历错误（取到一半连接断了）

	closed bool // Close 是否被调用：漏关行集时返回值看起来一切正常，只有看得见这个标志才会红
}

// Next 逐行推进：取完 rows 后返回 false，模拟行集遍历结束。
func (r *settingsRowsStub) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

// Scan 把当前行的值按位置赋给 dest；列数或类型不符立即报错，与真实库的扫描口径一致，
// 避免替身掩盖「仓储扫错列」。
//
// 扫描错误按 scanErrAt 定点注入而不是「一有 scanErr 就失败」：只有第 k 次（k>1）才失败，
// 才能模拟「前面几行已经扫出来了」的中途失败；每次都失败会让「不返回部分结果」这条
// 断言失去区分力（一条都没组装出来时它永远成立）。只设 scanErr 不设 scanErrAt 时
// 替身不会失败，用例会在「应返回错误」上红，不会静默放过。
func (r *settingsRowsStub) Scan(dest ...any) error {
	r.scanCalls++
	if r.scanErrAt > 0 && r.scanCalls == r.scanErrAt {
		return r.scanErr
	}
	if r.current == 0 {
		return errors.New("没有当前行时调用 Scan")
	}
	values := r.rows[r.current-1]
	if len(dest) != len(values) {
		return fmt.Errorf("列数期望 %d，实际 %d", len(values), len(dest))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			v, ok := values[i].(string)
			if !ok {
				return fmt.Errorf("第 %d 列期望 string，实际 %T", i, values[i])
			}
			*d = v
		case *bool:
			v, ok := values[i].(bool)
			if !ok {
				return fmt.Errorf("第 %d 列期望 bool，实际 %T", i, values[i])
			}
			*d = v
		case *time.Time:
			v, ok := values[i].(time.Time)
			if !ok {
				return fmt.Errorf("第 %d 列期望 time.Time，实际 %T", i, values[i])
			}
			*d = v
		default:
			return fmt.Errorf("第 %d 列类型 %T 不被替身支持", i, dest[i])
		}
	}
	return nil
}

// Close 由 LoadSettings 的 defer 调用，并记下真的被调过：漏掉 Close 的连接会一直被行集占着，
// 而调用方拿到的返回值完全正常——这条泄漏只能在替身上看出来。
func (r *settingsRowsStub) Close() { r.closed = true }

// Err 回放预设的遍历错误。
func (r *settingsRowsStub) Err() error { return r.err }

// settingsExecStub 记录一条事务内 Exec 的 SQL 与参数，供断言顺序、键与 now。
type settingsExecStub struct {
	sql  string
	args []any
}

// settingsTxStub 是 pgx.Tx 的替身：只实现 Exec/Commit/Rollback，
// 其余方法调用会 panic。用 recording 而不是逐条断言返回值，
// 是为了让「同一批同一个 now」「失败即停」这类顺序性质能被检查到。
type settingsTxStub struct {
	pgx.Tx
	execErrAt int // 第几条 Exec（从 1 起）返回错误；0 表示全部成功
	execs     []settingsExecStub
	// actions 按发生顺序记下事务内动作（exec/commit，rollback 不在其中）：
	// 「提交恰好一次且排在所有写入之后」只有看序列才能测出来，单看 Exec 条数时
	// 把 Commit 挪进循环（提交多次）或挪到 Exec 之前（提交早于写入）都照样能过。
	actions     []string
	commitCalls int // Commit 被调用的次数：挪进循环会变成多次，挪到 Exec 之前仍是 1 次但位置不对
	commitErr   error
	rolledBack  bool
}

// Exec 记录调用，并按 execErrAt 回放失败。
func (s *settingsTxStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, settingsExecStub{sql: sql, args: append([]any(nil), args...)})
	s.actions = append(s.actions, "exec")
	if s.execErrAt > 0 && len(s.execs) == s.execErrAt {
		return pgconn.CommandTag{}, errors.New("模拟第 " + strconv.Itoa(s.execErrAt) + " 条写入失败")
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

// Commit 记录提交动作与次数，便于断言失败路径没有提交、成功路径只提交一次且排在最后。
func (s *settingsTxStub) Commit(context.Context) error {
	s.commitCalls++
	s.actions = append(s.actions, "commit")
	return s.commitErr
}

// Rollback 记录回滚动作：失败路径必须回滚，成功路径上 defer 的回滚是空操作。
func (s *settingsTxStub) Rollback(context.Context) error {
	s.rolledBack = true
	return nil
}

// settingsQuerierStub 是 settingsQuerier 的替身：Query 回放预设行或错误，Begin 回放
// 预置事务。QueryRow 目前没有调用点，调用即 panic，避免「以为测了却走了空路径」。
type settingsQuerierStub struct {
	rows      pgx.Rows
	queryErr  error
	querySQL  string
	queryArgs []any

	tx       pgx.Tx
	beginErr error
	begins   int
}

// Query 记录 SQL 与参数，并回放预设的行集或错误。
func (s *settingsQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.querySQL = sql
	s.queryArgs = args
	return s.rows, s.queryErr
}

// QueryRow 实现接口但不被当前仓储调用。
func (s *settingsQuerierStub) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("单元测试不应调用 QueryRow：LoadSettings 走多行 Query")
}

// Begin 记录开启次数，并回放预置事务或错误。
func (s *settingsQuerierStub) Begin(context.Context) (pgx.Tx, error) {
	s.begins++
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return s.tx, nil
}

// TestLoadSettingsRequestsAllSevenKeys 锁住「键集合只有一个来源」：查询参数就是
// model.SettingKeys() 的七项，漏掉一项会先在这里红，而不是在设置页上静默空着。
func TestLoadSettingsRequestsAllSevenKeys(t *testing.T) {
	rowsStub := &settingsRowsStub{}
	stub := &settingsQuerierStub{rows: rowsStub}

	rows, err := NewSettingsRepo(stub).LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("没有行时应返回空切片，实际 %+v", rows)
	}

	if len(stub.queryArgs) != 1 {
		t.Fatalf("应只传一个数组参数，实际 %d 个: %+v", len(stub.queryArgs), stub.queryArgs)
	}
	keys, ok := stub.queryArgs[0].([]string)
	if !ok {
		t.Fatalf("查询参数应为 []string，实际 %T", stub.queryArgs[0])
	}
	if want := model.SettingKeys(); !slices.Equal(keys, want) {
		t.Fatalf("查询键集合期望 %v，实际 %v", want, keys)
	}

	// SQL 片段断言（写法同 room_test 的 EXCLUDED 断言）：参数化键集合、四列清单与
	// 排序都是 Scan 与快照的结构前提——把 ANY($1) 换回字面量 IN 列表，键集合就多出
	// 一份来源；漏掉一列，Scan 会在真实库上才炸；去掉 ORDER BY key，集成测试里那条
	// 「顺序按 key 升序」的断言就没有 SQL 支撑，只剩巧合。
	for _, fragment := range []string{
		"SELECT key, value, is_secret, updated_at",
		"FROM settings",
		"WHERE key = ANY($1)",
		"ORDER BY key",
	} {
		if !strings.Contains(stub.querySQL, fragment) {
			t.Fatalf("查询语句应包含 %q，实际 %q", fragment, stub.querySQL)
		}
	}
	// 零行也要关行集：没有行可扫时最容易漏掉 defer，泄漏的连接要等连接池自己回收。
	if !rowsStub.closed {
		t.Fatal("查询结果必须关闭，避免占住连接")
	}
}

// TestLoadSettingsWrapsQueryError 覆盖「查询直接失败」：包 ErrSettingStoreFailed，
// 且不返回半套结果（CP5）。
func TestLoadSettingsWrapsQueryError(t *testing.T) {
	stub := &settingsQuerierStub{queryErr: errors.New("连接中断")}

	rows, err := NewSettingsRepo(stub).LoadSettings(context.Background())
	if err == nil {
		t.Fatal("查询失败时应返回错误")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if rows != nil {
		t.Fatalf("失败时不应返回部分结果，实际 %+v", rows)
	}
}

// TestLoadSettingsWrapsRowsError 覆盖「取到一半连接断了」：两行都取出来之后 rows.Err
// 才报错，错误也必须包成 ErrSettingStoreFailed，不能把残行当完整快照交出去。
func TestLoadSettingsWrapsRowsError(t *testing.T) {
	rowsStub := &settingsRowsStub{
		rows: [][]any{
			{model.SettingCronExpr, model.DefaultCronExpr, false, time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)},
			{model.SettingJWUsername, "teacher01", false, time.Date(2026, 11, 8, 9, 1, 0, 0, time.UTC)},
		},
		err: errors.New("连接中断"),
	}
	stub := &settingsQuerierStub{rows: rowsStub}

	rows, err := NewSettingsRepo(stub).LoadSettings(context.Background())
	if err == nil {
		t.Fatal("遍历失败时应返回错误")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if rows != nil {
		t.Fatalf("失败时不应返回部分结果，实际 %+v", rows)
	}
	// 失败点必须在两行都取出来之后：先回放 N 行再报错，才存在「已组装出一部分」的残行；
	// 一行都没取就报错时，这条用例对部分结果没有区分力。
	if rowsStub.current != len(rowsStub.rows) {
		t.Fatalf("应在取完 %d 行后才报错，实际只取了 %d 行", len(rowsStub.rows), rowsStub.current)
	}
	if !rowsStub.closed {
		t.Fatal("失败路径也必须关闭结果集，否则连接会泄漏")
	}
}

// TestLoadSettingsWrapsScanError 覆盖「第一行扫得出来、第二行扫不出来」：扫描失败必须
// 包成 ErrSettingStoreFailed，且不能把已组装的那部分当快照交出去——Scan 是列形状的
// 最后一道关，这里放过错误，上面只会看到少了几列的空值行。
func TestLoadSettingsWrapsScanError(t *testing.T) {
	rowsStub := &settingsRowsStub{
		rows: [][]any{
			{model.SettingCronExpr, model.DefaultCronExpr, false, time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)},
			{model.SettingJWUsername, "teacher01", false, time.Date(2026, 11, 8, 9, 1, 0, 0, time.UTC)},
		},
		scanErrAt: 2,
		scanErr:   errors.New("列类型不匹配"),
	}
	stub := &settingsQuerierStub{rows: rowsStub}

	rows, err := NewSettingsRepo(stub).LoadSettings(context.Background())
	if err == nil {
		t.Fatal("扫描失败时应返回错误")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if rows != nil {
		t.Fatalf("扫描失败时不应返回部分结果，实际 %+v", rows)
	}
	// 失败点必须是第二行：第一行成功组装、第二行才报错，才覆盖「已有部分结果」的中途
	// 失败；若替身在第一行就失败，这条用例同样测不到部分结果被丢弃。
	if rowsStub.scanCalls != 2 {
		t.Fatalf("应在第 2 次 Scan 失败，实际 Scan 调用 %d 次", rowsStub.scanCalls)
	}
	// 扫描失败也要关行集：错误路径是提前 return 的，defer 之外的关行写法会在这里漏掉。
	if !rowsStub.closed {
		t.Fatal("即使扫描失败也必须关闭结果集")
	}
}

// TestLoadSettingsScansFourColumnsInOrder 锁住四列的扫描顺序与类型：SELECT 的列序是
// key, value, is_secret, updated_at，仓储按同一顺序取地址。替身按位置真赋值并校验类型，
// 所以「仓储把 is_secret 扫进字符串」会当场变成扫描错误，而把 key/value 两列换序会
// 让下面的期望值对不上。
func TestLoadSettingsScansFourColumnsInOrder(t *testing.T) {
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	rowsStub := &settingsRowsStub{rows: [][]any{
		{model.SettingJWPassword, "v1:cipher", true, now},
		{model.SettingCronExpr, model.DefaultCronExpr, false, now.Add(time.Minute)},
	}}
	stub := &settingsQuerierStub{rows: rowsStub}

	rows, err := NewSettingsRepo(stub).LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	want := []model.SettingRow{
		{Key: model.SettingJWPassword, Value: "v1:cipher", Secret: true, UpdatedAt: now},
		{Key: model.SettingCronExpr, Value: model.DefaultCronExpr, Secret: false, UpdatedAt: now.Add(time.Minute)},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("四列扫描结果期望 %+v，实际 %+v", want, rows)
	}
	if !rowsStub.closed {
		t.Fatal("查询结果必须关闭，避免占住连接")
	}
}

// TestApplySettingsSkipsTransactionForEmptyBatch 覆盖空批次：没有行要写就不开事务，
// 否则「零变更」会在连接池不可用时表现成一次失败。
func TestApplySettingsSkipsTransactionForEmptyBatch(t *testing.T) {
	stub := &settingsQuerierStub{tx: &settingsTxStub{}}

	if err := NewSettingsRepo(stub).ApplySettings(context.Background(), nil, time.Now()); err != nil {
		t.Fatalf("空批次不应报错: %v", err)
	}
	if stub.begins != 0 {
		t.Fatalf("空批次不应开事务，实际开了 %d 次", stub.begins)
	}
}

// TestApplySettingsUsesOneTransactionAndOneNow 覆盖成功路径：整批只开一个事务、
// 按提交顺序各一条语句、每条 upsert 的时间戳都是入参 now，最后提交一次。
func TestApplySettingsUsesOneTransactionAndOneNow(t *testing.T) {
	tx := &settingsTxStub{}
	stub := &settingsQuerierStub{tx: tx}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	changes := []model.SettingChange{
		{Key: model.SettingJWUsername, Value: "teacher01"},
		{Key: model.SettingJWPassword, Value: "v1:cipher", Secret: true},
	}

	if err := NewSettingsRepo(stub).ApplySettings(context.Background(), changes, now); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if stub.begins != 1 {
		t.Fatalf("整批应只开一个事务，实际 %d 个", stub.begins)
	}
	if len(tx.execs) != len(changes) {
		t.Fatalf("应有 %d 条语句，实际 %d 条", len(changes), len(tx.execs))
	}
	for index, exec := range tx.execs {
		// 四条片段钉住 upsert 的覆盖语义（写法同 room_test 的 EXCLUDED 断言）：
		// DO NOTHING 会让第二次保存静默不生效；漏掉任一 SET 列，那一列就还是旧值，
		// 而集成测试只覆盖了正常路径的「三列都变了」，换成 DO NOTHING 时它仍会红，
		// 但不指向具体哪一列——这里把三列逐列钉死。
		for _, fragment := range []string{
			"ON CONFLICT (key) DO UPDATE",
			"value = EXCLUDED.value",
			"is_secret = EXCLUDED.is_secret",
			"updated_at = EXCLUDED.updated_at",
		} {
			if !strings.Contains(exec.sql, fragment) {
				t.Fatalf("第 %d 条 upsert 应包含 %q，实际 SQL %q", index, fragment, exec.sql)
			}
		}
		if len(exec.args) != 4 {
			t.Fatalf("第 %d 条应有 4 个参数，实际 %d 个", index, len(exec.args))
		}
		gotNow, ok := exec.args[3].(time.Time)
		if !ok || !gotNow.Equal(now) {
			t.Fatalf("第 %d 条 updated_at 期望 %s，实际 %v", index, now, exec.args[3])
		}
		if exec.args[0] != changes[index].Key || exec.args[1] != changes[index].Value || exec.args[2] != changes[index].Secret {
			t.Fatalf("第 %d 条参数不符: %+v", index, exec.args)
		}
	}
	if tx.commitCalls != 1 {
		t.Fatalf("整批应只提交一次，实际 %d 次", tx.commitCalls)
	}
	// 动作序列钉住「提交恰好一次、且排在所有写入之后」：Commit 挪进循环会变成
	// exec/commit/exec/commit（两次提交），挪到 Exec 之前会变成 commit/exec/exec
	// （提交早于写入）。两种改法都不改 Exec 的条数，只有看序列才会红。
	wantActions := make([]string, 0, len(changes)+1)
	for range changes {
		wantActions = append(wantActions, "exec")
	}
	wantActions = append(wantActions, "commit")
	if !slices.Equal(tx.actions, wantActions) {
		t.Fatalf("事务动作序列期望 %v，实际 %v（Commit 应在所有 Exec 之后且只出现一次）", wantActions, tx.actions)
	}
}

// TestApplySettingsRejectsUnknownKey 覆盖键白名单：键外变更在开事务之前就被整批拒绝，
// 库里不会留下半批写入，也不会写进一行 LoadSettings 永远读不到的设置。upsert 与
// delete 两个分支都要经过同一道门，所以两者各来一次。
func TestApplySettingsRejectsUnknownKey(t *testing.T) {
	cases := []struct {
		name   string
		change model.SettingChange
	}{
		{name: "upsert 键外", change: model.SettingChange{Key: "legacy_unknown_key", Value: "should-not-be-written"}},
		{name: "delete 键外", change: model.SettingChange{Key: "legacy_unknown_key", Delete: true}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx := &settingsTxStub{}
			stub := &settingsQuerierStub{tx: tx}

			err := NewSettingsRepo(stub).ApplySettings(context.Background(), []model.SettingChange{
				{Key: model.SettingJWUsername, Value: "legit"},
				c.change,
			}, time.Now())
			if err == nil {
				t.Fatal("键外变更应被拒绝")
			}
			if !errors.Is(err, model.ErrSettingStoreFailed) {
				t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
			}
			// 白名单校验排在 Begin 之前：注定失败的批次不该多开一个事务。
			if stub.begins != 0 {
				t.Fatalf("键外变更不应开事务，实际开了 %d 次", stub.begins)
			}
			if len(tx.execs) != 0 {
				t.Fatalf("键外变更不应执行任何语句，实际 %d 条", len(tx.execs))
			}
			if !strings.Contains(err.Error(), "legacy_unknown_key") {
				t.Fatalf("错误消息应点名越界键，实际 %q", err.Error())
			}
			if strings.Contains(err.Error(), "should-not-be-written") || strings.Contains(err.Error(), "legit") {
				t.Fatalf("错误消息不应包含设置值，实际 %q", err.Error())
			}
		})
	}
}

// TestApplySettingsDeletesWithKeyOnly 覆盖 Delete 分支：走 DELETE 语句、只传键，
// 不把 value/secret/now 拼进去。
func TestApplySettingsDeletesWithKeyOnly(t *testing.T) {
	tx := &settingsTxStub{}
	stub := &settingsQuerierStub{tx: tx}

	err := NewSettingsRepo(stub).ApplySettings(context.Background(),
		[]model.SettingChange{{Key: model.SettingCronExpr, Delete: true}}, time.Now())
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if len(tx.execs) != 1 {
		t.Fatalf("应只有一条语句，实际 %d 条", len(tx.execs))
	}
	exec := tx.execs[0]
	if !strings.HasPrefix(strings.TrimSpace(exec.sql), "DELETE FROM settings") {
		t.Fatalf("Delete 分支应走 DELETE，实际 SQL %q", exec.sql)
	}
	if len(exec.args) != 1 || exec.args[0] != model.SettingCronExpr {
		t.Fatalf("DELETE 应只传键，实际参数 %+v", exec.args)
	}
}

// TestApplySettingsRollsBackWhenSecondChangeFails 覆盖 CP5 的事务面：第二条失败时
// 立即停止、不提交、整体回滚，且错误消息只带键名不带值。
func TestApplySettingsRollsBackWhenSecondChangeFails(t *testing.T) {
	tx := &settingsTxStub{execErrAt: 2}
	stub := &settingsQuerierStub{tx: tx}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

	err := NewSettingsRepo(stub).ApplySettings(context.Background(), []model.SettingChange{
		{Key: model.SettingJWUsername, Value: "sentinel-first"},
		{Key: model.SettingFeishuWebhook, Value: "sentinel-second", Secret: true},
	}, now)
	if err == nil {
		t.Fatal("第二条写入失败时整批应失败")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if len(tx.execs) != 2 {
		t.Fatalf("失败后应停止，期望 2 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatal("失败批次不应提交")
	}
	if !tx.rolledBack {
		t.Fatal("失败批次应回滚整个事务")
	}
	if !strings.Contains(err.Error(), model.SettingFeishuWebhook) {
		t.Fatalf("错误消息应点名失败键，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("错误消息不应包含设置值，实际 %q", err.Error())
	}
}

// TestApplySettingsRollsBackWhenDeleteFails 覆盖 Delete 分支的单条失败：删除执行失败时
// 立即停止、不提交、整体回滚，错误包哨兵并点名键名。
//
// upsert 分支的失败已由 TestApplySettingsRollsBackWhenSecondChangeFails 覆盖，但两个分支
// 各自拼一套错误文案：删除路径漏包 ErrSettingStoreFailed、或把整个变更拼进消息时，
// 只有「单条 Delete 失败」这条用例会红。用单条变更时 Exec 条数 1 只说明「没有多余语句」；
// 「失败即停」由 TestApplySettingsRollsBackWhenSecondChangeFails 的第二条变更证明。
func TestApplySettingsRollsBackWhenDeleteFails(t *testing.T) {
	tx := &settingsTxStub{execErrAt: 1}
	stub := &settingsQuerierStub{tx: tx}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

	err := NewSettingsRepo(stub).ApplySettings(context.Background(), []model.SettingChange{
		// Delete 为 true 时 Value 被忽略；这里仍放一个哨兵，用来验证错误消息不把请求里的
		// 值带出去——删除路径的参数只有键，补上下文时最容易顺手把整条 change 拼进消息。
		{Key: model.SettingFeishuWebhook, Value: "sentinel-delete", Delete: true},
	}, now)
	if err == nil {
		t.Fatal("删除失败时整批应失败")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if len(tx.execs) != 1 {
		t.Fatalf("单条删除失败后应停止，期望 1 条语句，实际 %d 条", len(tx.execs))
	}
	if tx.commitCalls != 0 {
		t.Fatal("删除失败的批次不应提交")
	}
	if !tx.rolledBack {
		t.Fatal("删除失败的批次应回滚整个事务")
	}
	if !strings.Contains(err.Error(), model.SettingFeishuWebhook) {
		t.Fatalf("错误消息应点名失败键，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("错误消息不应包含设置值，实际 %q", err.Error())
	}
}

// TestApplySettingsWrapsBeginError 覆盖事务开不起来：包 ErrSettingStoreFailed，
// 且没有执行任何语句。
func TestApplySettingsWrapsBeginError(t *testing.T) {
	stub := &settingsQuerierStub{beginErr: errors.New("连接池耗尽")}

	err := NewSettingsRepo(stub).ApplySettings(context.Background(),
		[]model.SettingChange{{Key: model.SettingCronExpr, Value: model.DefaultCronExpr}}, time.Now())
	if err == nil {
		t.Fatal("开启事务失败时应返回错误")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if stub.begins != 1 {
		t.Fatalf("应尝试开启事务 1 次，实际 %d 次", stub.begins)
	}
}

// TestApplySettingsWrapsCommitError 覆盖提交失败：包 ErrSettingStoreFailed，
// 且 defer 的回滚仍然执行（事务未生效）。
func TestApplySettingsWrapsCommitError(t *testing.T) {
	tx := &settingsTxStub{commitErr: errors.New("提交超时")}
	stub := &settingsQuerierStub{tx: tx}

	err := NewSettingsRepo(stub).ApplySettings(context.Background(),
		[]model.SettingChange{{Key: model.SettingCronExpr, Value: model.DefaultCronExpr}}, time.Now())
	if err == nil {
		t.Fatal("提交失败时应返回错误")
	}
	if !errors.Is(err, model.ErrSettingStoreFailed) {
		t.Fatalf("应包 ErrSettingStoreFailed，实际 %v", err)
	}
	if tx.commitCalls != 1 {
		t.Fatal("应尝试提交一次")
	}
	if !tx.rolledBack {
		t.Fatal("提交失败后 defer 仍应回滚")
	}
}
