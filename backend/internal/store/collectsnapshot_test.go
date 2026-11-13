package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// snapshotRowStub 是 pgx.Row 的替身：回放一行版本号，或回放预设错误（如 pgx.ErrNoRows）。
type snapshotRowStub struct {
	values []any
	err    error
}

// Scan 把预设值按位置赋给 dest；err 非空时直接返回它。
func (r *snapshotRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("列数期望 %d，实际 %d", len(r.values), len(dest))
	}
	for i := range dest {
		if err := assignSnapshotValue(dest[i], r.values[i]); err != nil {
			return fmt.Errorf("第 %d 列: %w", i, err)
		}
	}
	return nil
}

// snapshotRowsStub 是 pgx.Rows 的替身：只覆盖快照读取用到的 Next/Scan/Err/Close，
// 其余方法嵌入接口、调用会 panic——替身只放行被测路径，测错方法立刻暴露。
//
// Scan 按位置真赋值并校验类型：只会回 nil 的替身能让「列顺序写反」「可空列扫进
// 值类型」照样通过，而那正是只在真实库上才炸的错误。
type snapshotRowsStub struct {
	pgx.Rows
	rows    [][]any // 依次回放的行，每行按 Scan 目标的列顺序给值
	current int     // 已被 Next 取出的行数，也是当前行在 rows 里的下标 +1

	scanErr   error // 第 scanErrAt 次 Scan 回放的扫描错误（有行，但这一行解不出来）
	scanErrAt int   // 第几次 Scan（从 1 起）才失败；0 表示不注入扫描错误
	scanCalls int   // Scan 已调用的次数

	err error // Err 回放的遍历错误（取到一半连接断了）

	closed bool // Close 是否被调用：漏关行集会占住连接，而返回值看起来一切正常
}

// Next 逐行推进：取完 rows 后返回 false，模拟行集遍历结束。
func (r *snapshotRowsStub) Next() bool {
	if r.current >= len(r.rows) {
		return false
	}
	r.current++
	return true
}

// Scan 把当前行的值按位置赋给 dest；列数或类型不符立即报错。
func (r *snapshotRowsStub) Scan(dest ...any) error {
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
		if err := assignSnapshotValue(dest[i], values[i]); err != nil {
			return fmt.Errorf("第 %d 列: %w", i, err)
		}
	}
	return nil
}

// Close 由仓储的 defer 调用，并记下真的被调过。
func (r *snapshotRowsStub) Close() { r.closed = true }

// Err 回放预设的遍历错误。
func (r *snapshotRowsStub) Err() error { return r.err }

// assignSnapshotValue 按 Scan 目标类型赋值；类型不符立即报错，避免替身掩盖扫错列。
func assignSnapshotValue(dest any, value any) error {
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
	case *bool:
		v, ok := value.(bool)
		if !ok {
			return fmt.Errorf("期望 bool，实际 %T", value)
		}
		*d = v
	default:
		return fmt.Errorf("不支持的扫描目标 %T", dest)
	}
	return nil
}

// snapshotQuerierStub 按 SQL 回放版本行与内容行，并记录调用参数。
// 按 SQL 而不是按调用顺序查找：用例想钉的是「哪条语句带哪个版本」，
// 顺带能在仓储查错语句时直接报「未预置查询」，而不是静默回放空行。
type snapshotQuerierStub struct {
	rowResults map[string]pgx.Row  // 版本查询的结果
	rows       map[string]pgx.Rows // 内容查询的结果
	queryErr   map[string]error    // 内容查询的注入错误

	rowSQL    []string         // QueryRow 的调用顺序与 SQL
	rowArgs   map[string][]any // 每条版本查询的参数（应恒为空）
	querySQL  []string         // Query 的调用顺序与 SQL
	queryArgs map[string][]any // 每条内容查询的参数
}

// QueryRow 实现 snapshotQuerier。
func (s *snapshotQuerierStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.rowSQL = append(s.rowSQL, sql)
	s.rowArgs[sql] = args
	if row, ok := s.rowResults[sql]; ok {
		return row
	}
	return &snapshotRowStub{err: fmt.Errorf("未预置版本查询: %s", sql)}
}

// Query 实现 snapshotQuerier。
func (s *snapshotQuerierStub) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.querySQL = append(s.querySQL, sql)
	s.queryArgs[sql] = args
	if err, ok := s.queryErr[sql]; ok {
		return nil, err
	}
	if rows, ok := s.rows[sql]; ok {
		return rows, nil
	}
	return nil, fmt.Errorf("未预置内容查询: %s", sql)
}

// normalizeSQL 把 SQL 的换行与连续空白折成单空格，便于按列清单与子句断言：
// 断言的是 SQL 的语义片段，不是格式化后的缩进。
func normalizeSQL(sql string) string {
	return strings.Join(strings.Fields(sql), " ")
}

// newSnapshotQuerier 预置两条版本查询与三张内容表的最小有效行集；用例按需替换，
// 包括替换成空行集（覆盖「版本行在、内容表为空」）与注入错误。
func newSnapshotQuerier(dictVersion, axisVersion int) *snapshotQuerierStub {
	return &snapshotQuerierStub{
		rowResults: map[string]pgx.Row{
			latestDictVersionSQL: &snapshotRowStub{values: []any{dictVersion}},
			latestAxisVersionSQL: &snapshotRowStub{values: []any{axisVersion}},
		},
		rows: map[string]pgx.Rows{
			snapshotDictStateSQL:  &snapshotRowsStub{rows: [][]any{{"free", "空闲", true}}},
			snapshotDictSymbolSQL: &snapshotRowsStub{rows: [][]any{{"√", "free"}}},
			snapshotAxisNodeSQL:   &snapshotRowsStub{rows: [][]any{{"01", 1, "0102", 1}}},
		},
		queryErr:  map[string]error{},
		rowArgs:   map[string][]any{},
		queryArgs: map[string][]any{},
	}
}

// TestLoadCollectSnapshotPicksLatestVersions 锁住版本选择口径：两条版本查询各自
// ORDER BY version DESC LIMIT 1 且不带参数；取到的字典版本只传给字典内容查询、
// 轴版本只传给轴内容查询，不把两者混用。
func TestLoadCollectSnapshotPicksLatestVersions(t *testing.T) {
	q := newSnapshotQuerier(7, 3)

	snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}

	if snap.DictVersion != 7 || snap.AxisVersion != 3 {
		t.Fatalf("版本期望 dict=7 axis=3，实际 dict=%d axis=%d", snap.DictVersion, snap.AxisVersion)
	}

	// 替身不执行 SQL，只能钉住「取最大版本」的子句与两张表的归属。
	if len(q.rowSQL) != 2 {
		t.Fatalf("应读两条版本，实际 %d 条: %+v", len(q.rowSQL), q.rowSQL)
	}
	if !strings.Contains(q.rowSQL[0], "FROM dict") ||
		!strings.Contains(q.rowSQL[0], "ORDER BY version DESC LIMIT 1") {
		t.Fatalf("字典版本查询应按版本倒序取一行，实际: %s", q.rowSQL[0])
	}
	if !strings.Contains(q.rowSQL[1], "FROM axis") ||
		!strings.Contains(q.rowSQL[1], "ORDER BY version DESC LIMIT 1") {
		t.Fatalf("轴版本查询应按版本倒序取一行，实际: %s", q.rowSQL[1])
	}
	for _, sql := range []string{latestDictVersionSQL, latestAxisVersionSQL} {
		if args := q.rowArgs[sql]; len(args) != 0 {
			t.Fatalf("版本查询不应带参数，%q 实际 %+v", sql, args)
		}
	}

	if got := q.queryArgs[snapshotDictStateSQL]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("字典状态应按版本 7 读取，实际参数 %+v", got)
	}
	if got := q.queryArgs[snapshotDictSymbolSQL]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("字典符号应按版本 7 读取，实际参数 %+v", got)
	}
	if got := q.queryArgs[snapshotAxisNodeSQL]; len(got) != 1 || got[0] != 3 {
		t.Fatalf("节次轴应按版本 3 读取，实际参数 %+v", got)
	}
}

// TestLoadCollectSnapshotMapsDictAndAxisRows 覆盖两表逐行映射：dict_state 的
// state_key → DictState、dict_symbol 的 symbol → state_key 各归各位，
// axis_node 的四列按 model.AxisNode 的字段顺序扫入；行集必须关闭。
func TestLoadCollectSnapshotMapsDictAndAxisRows(t *testing.T) {
	q := newSnapshotQuerier(2, 1)

	stateRows := &snapshotRowsStub{rows: [][]any{
		{"free", "空闲", true},
		{"occupied", "占用", false},
		{"unknown", "未知", false},
	}}
	symbolRows := &snapshotRowsStub{rows: [][]any{
		{"√", "free"},
		{"×", "occupied"},
	}}
	axisRows := &snapshotRowsStub{rows: [][]any{
		{"01", 1, "0102", 1},
		{"02", 2, "0102", 1},
		{"03", 3, "0304", 2},
	}}
	q.rows[snapshotDictStateSQL] = stateRows
	q.rows[snapshotDictSymbolSQL] = symbolRows
	q.rows[snapshotAxisNodeSQL] = axisRows

	snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}

	wantStates := map[string]model.DictState{
		"free":     {Label: "空闲", Available: true},
		"occupied": {Label: "占用", Available: false},
		"unknown":  {Label: "未知", Available: false},
	}
	if !reflect.DeepEqual(snap.Dict.States, wantStates) {
		t.Fatalf("状态表期望 %+v，实际 %+v", wantStates, snap.Dict.States)
	}

	wantSymbols := map[string]string{"√": "free", "×": "occupied"}
	if !reflect.DeepEqual(snap.Dict.Symbols, wantSymbols) {
		t.Fatalf("符号表期望 %+v，实际 %+v", wantSymbols, snap.Dict.Symbols)
	}

	wantAxis := []model.AxisNode{
		{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{Node: "03", Ordinal: 3, Block: "0304", BlockOrdinal: 2},
	}
	if !reflect.DeepEqual(snap.Axis, wantAxis) {
		t.Fatalf("节次轴期望 %+v，实际 %+v", wantAxis, snap.Axis)
	}

	for name, stub := range map[string]*snapshotRowsStub{
		"字典状态": stateRows,
		"字典符号": symbolRows,
		"节次轴":  axisRows,
	} {
		if !stub.closed {
			t.Fatalf("%s的行集必须关闭，避免占住连接", name)
		}
	}
}

// TestLoadCollectSnapshotAxisSQLOrdersByOrdinal 覆盖 ordinal 排序：排序由轴的
// ORDER BY ordinal, node 承担（替身不执行 SQL，因此钉住排序子句）；ordinal 没有
// 唯一约束，并列时按 node 定序。仓储按行集顺序原样产出，不在代码里二次排序或打乱。
func TestLoadCollectSnapshotAxisSQLOrdersByOrdinal(t *testing.T) {
	q := newSnapshotQuerier(1, 1)
	axisRows := &snapshotRowsStub{rows: [][]any{
		{"01", 1, "0102", 1},
		{"02", 2, "0102", 1},
		{"03", 3, "0304", 2},
	}}
	q.rows[snapshotAxisNodeSQL] = axisRows

	snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}

	for _, fragment := range []string{
		"FROM axis_node",
		"WHERE axis_version = $1",
		"ORDER BY ordinal, node",
	} {
		if !strings.Contains(normalizeSQL(snapshotAxisNodeSQL), fragment) {
			t.Fatalf("轴查询应包含 %q，实际: %s", fragment, normalizeSQL(snapshotAxisNodeSQL))
		}
	}
	if len(snap.Axis) != 3 {
		t.Fatalf("期望 3 个轴节点，实际 %d 个: %+v", len(snap.Axis), snap.Axis)
	}
	for i, wantOrdinal := range []int{1, 2, 3} {
		if snap.Axis[i].Ordinal != wantOrdinal {
			t.Fatalf("第 %d 个节点的 ordinal 期望 %d，实际 %d", i, wantOrdinal, snap.Axis[i].Ordinal)
		}
	}
}

// TestSnapshotSQLsSelectColumnsInScanOrder 钉住三条内容查询的 SELECT 列清单与列序。
// 行替身只能拦住类型不符（例如把 bool 的 available 扫进 string），拦不住两个同类型
// 文本列写反——symbol 与 state_key 互换后映射会静默错位；列序与 Scan 不一致时
// 真实库也不会报错。所以这里按 SQL 原文断言「SELECT 的列序就是 Scan 的顺序」。
func TestSnapshotSQLsSelectColumnsInScanOrder(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "字典状态按 state_key, label, available 扫码",
			sql:  snapshotDictStateSQL,
			want: "SELECT state_key, label, available FROM dict_state",
		},
		{
			name: "字典符号按 symbol, state_key 扫码",
			sql:  snapshotDictSymbolSQL,
			want: "SELECT symbol, state_key FROM dict_symbol",
		},
		{
			name: "节次轴按 node, ordinal, block, block_ordinal 扫码",
			sql:  snapshotAxisNodeSQL,
			want: "SELECT node, ordinal, block, block_ordinal FROM axis_node",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSQL(c.sql)
			if !strings.HasPrefix(got, c.want) {
				t.Fatalf("列清单应为 %q（顺序须与 Scan 一致），实际: %s", c.want, got)
			}
		})
	}
}

// TestLoadCollectSnapshotFailsWithoutVersionRows 覆盖空表：dict 或 axis 没有版本行时
// 返回包装 model.ErrQueryFailed 的错误、不返回空快照，且不再继续查内容表。
func TestLoadCollectSnapshotFailsWithoutVersionRows(t *testing.T) {
	cases := []struct {
		name       string
		missingSQL string
		table      string
	}{
		{name: "字典没有版本行", missingSQL: latestDictVersionSQL, table: "字典"},
		{name: "轴没有版本行", missingSQL: latestAxisVersionSQL, table: "轴"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newSnapshotQuerier(5, 2)
			q.rowResults[c.missingSQL] = &snapshotRowStub{err: pgx.ErrNoRows}

			snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
			if err == nil {
				t.Fatalf("%s时应报错，实际返回快照 %+v", c.name, snap)
			}
			if !errors.Is(err, model.ErrQueryFailed) {
				t.Fatalf("错误应包装 model.ErrQueryFailed，实际: %v", err)
			}
			if want := c.table + "表没有版本行"; !strings.Contains(err.Error(), want) {
				t.Fatalf("错误应点名 %q，实际: %v", want, err)
			}
			if snap.DictVersion != 0 || snap.AxisVersion != 0 ||
				snap.Dict.Symbols != nil || snap.Dict.States != nil || snap.Axis != nil {
				t.Fatalf("空表不应返回快照: %+v", snap)
			}
			if len(q.querySQL) != 0 {
				t.Fatalf("版本缺失后不应继续查内容表，实际: %+v", q.querySQL)
			}
		})
	}
}

// TestLoadCollectSnapshotFailsWithoutContentRows 覆盖「版本行在、内容表为空」：
// dict_state / dict_symbol / axis_node 任一整表为空都算这一版字典或轴坏了，
// 必须返回包装 model.ErrQueryFailed 的错误并点名是哪张表为空，而不是交回
// 空映射或空轴让 clean / parser 在拿到上游数据之后才炸。
func TestLoadCollectSnapshotFailsWithoutContentRows(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		column string // 错误里应点名的空表
		scoped string // 错误里应带上的版本口径
	}{
		{
			name:   "状态表为空",
			sql:    snapshotDictStateSQL,
			column: "dict_state表",
			scoped: "字典版本 5",
		},
		{
			name:   "符号表为空",
			sql:    snapshotDictSymbolSQL,
			column: "dict_symbol表",
			scoped: "字典版本 5",
		},
		{
			name:   "节次轴为空",
			sql:    snapshotAxisNodeSQL,
			column: "axis_node表",
			scoped: "轴版本 2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newSnapshotQuerier(5, 2)
			q.rows[c.sql] = &snapshotRowsStub{}

			snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
			if err == nil {
				t.Fatalf("%s时应报错，实际返回快照 %+v", c.name, snap)
			}
			if !errors.Is(err, model.ErrQueryFailed) {
				t.Fatalf("错误应包装 model.ErrQueryFailed，实际: %v", err)
			}
			for _, want := range []string{c.column, "没有内容行", c.scoped} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("错误应包含 %q，实际: %v", want, err)
				}
			}
			if snap.DictVersion != 0 || snap.AxisVersion != 0 ||
				snap.Dict.Symbols != nil || snap.Dict.States != nil || snap.Axis != nil {
				t.Fatalf("内容表为空不应返回快照: %+v", snap)
			}
		})
	}
}

// TestLoadCollectSnapshotWrapsReadFailures 覆盖内容查询的各类失败：查询、扫描、
// 遍历都必须包装 model.ErrQueryFailed 并带上出错环节，且不返回半截快照。
func TestLoadCollectSnapshotWrapsReadFailures(t *testing.T) {
	boom := errors.New("连接断开")

	cases := []struct {
		name  string
		setup func(q *snapshotQuerierStub)
		want  string
	}{
		{
			name: "版本查询失败",
			setup: func(q *snapshotQuerierStub) {
				q.rowResults[latestDictVersionSQL] = &snapshotRowStub{err: boom}
			},
			want: "读取字典最大版本失败",
		},
		{
			name: "状态查询失败",
			setup: func(q *snapshotQuerierStub) {
				q.queryErr[snapshotDictStateSQL] = boom
			},
			want: "查询字典状态失败",
		},
		{
			name: "符号扫描失败",
			setup: func(q *snapshotQuerierStub) {
				q.rows[snapshotDictSymbolSQL] = &snapshotRowsStub{
					rows:      [][]any{{"√", "free"}},
					scanErr:   boom,
					scanErrAt: 1,
				}
			},
			want: "扫描字典符号失败",
		},
		{
			name: "轴查询失败",
			setup: func(q *snapshotQuerierStub) {
				q.queryErr[snapshotAxisNodeSQL] = boom
			},
			want: "查询节次轴失败",
		},
		{
			name: "轴遍历失败",
			setup: func(q *snapshotQuerierStub) {
				q.rows[snapshotAxisNodeSQL] = &snapshotRowsStub{err: boom}
			},
			want: "遍历节次轴失败",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newSnapshotQuerier(4, 6)
			c.setup(q)

			snap, err := NewSnapshotRepo(q).LoadCollectSnapshot(context.Background())
			if err == nil {
				t.Fatalf("%s时应报错，实际返回快照 %+v", c.name, snap)
			}
			if !errors.Is(err, model.ErrQueryFailed) {
				t.Fatalf("错误应包装 model.ErrQueryFailed，实际: %v", err)
			}
			if !errors.Is(err, boom) {
				t.Fatalf("错误应保留底层原因，实际: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应点名 %q，实际: %v", c.want, err)
			}
			if snap.DictVersion != 0 || snap.AxisVersion != 0 ||
				snap.Dict.Symbols != nil || snap.Dict.States != nil || snap.Axis != nil {
				t.Fatalf("失败时不应返回半截快照: %+v", snap)
			}
		})
	}
}
