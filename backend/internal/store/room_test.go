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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestUpsertRoomAttributionWritesRows 覆盖归属落库的入参映射：
// 一次 Exec 写入全部房号，楼编号与三列数组按原顺序对齐。
func TestUpsertRoomAttributionWritesRows(t *testing.T) {
	q := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 2")}}
	attribution := RoomAttribution{
		BuildingID: "Q10",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "F101", NameRaw: "F101(60/0)"},
			{RoomID: "r102", Name: "F102", NameRaw: "F102"},
		},
	}

	if err := UpsertRoomAttribution(context.Background(), q, attribution); err != nil {
		t.Fatalf("写入归属不应报错，实际 %v", err)
	}
	if q.execCalls != 1 {
		t.Fatalf("应只执行 1 条 SQL，实际 %d 条", q.execCalls)
	}
	if !strings.Contains(q.execSQLs[0], "INSERT INTO room") {
		t.Fatalf("SQL 应写 room 表，实际 %q", q.execSQLs[0])
	}
	if !strings.Contains(q.execSQLs[0], "ON CONFLICT (room_id) DO UPDATE") {
		t.Fatalf("SQL 应靠 ON CONFLICT 保持幂等，实际 %q", q.execSQLs[0])
	}
	if !strings.Contains(q.execSQLs[0], "building_id = EXCLUDED.building_id") {
		t.Fatalf("SQL 应在冲突时把归属楼覆盖成新楼，实际 %q", q.execSQLs[0])
	}

	args := q.execArgs[0]
	if len(args) != 4 {
		t.Fatalf("应有 4 个参数，实际 %d 个: %+v", len(args), args)
	}
	if args[0] != "Q10" {
		t.Fatalf("楼编号参数期望 Q10，实际 %v", args[0])
	}
	if got := args[1].([]string); strings.Join(got, ",") != "r101,r102" {
		t.Fatalf("房号数组不符: %v", got)
	}
	if got := args[2].([]string); strings.Join(got, ",") != "F101,F102" {
		t.Fatalf("展示名数组不符: %v", got)
	}
	if got := args[3].([]string); strings.Join(got, ",") != "F101(60/0),F102" {
		t.Fatalf("原始名数组不符: %v", got)
	}
}

// TestUpsertRoomAttributionDedupesRooms 覆盖同批重复房号：
// 只保留首次出现，避免 ON CONFLICT DO UPDATE 对同一行改两次而报错。
func TestUpsertRoomAttributionDedupesRooms(t *testing.T) {
	q := &syncTaskQuerierStub{}
	attribution := RoomAttribution{
		BuildingID: "Q10",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "F101", NameRaw: "F101"},
			{RoomID: "r101", Name: "后到的名字", NameRaw: "F101(60/0)"},
		},
	}

	if err := UpsertRoomAttribution(context.Background(), q, attribution); err != nil {
		t.Fatalf("重复房号不应报错，实际 %v", err)
	}
	if got := q.execArgs[0][1].([]string); len(got) != 1 || got[0] != "r101" {
		t.Fatalf("重复房号应只保留一条，实际 %v", got)
	}
	if got := q.execArgs[0][2].([]string); len(got) != 1 || got[0] != "F101" {
		t.Fatalf("应保留首次出现的展示名，实际 %v", got)
	}
}

// TestUpsertRoomAttributionEmptyRoomsIsNoop 覆盖空房号集合：
// 不执行 SQL，也不会因为空 unnest 写坏数据。
func TestUpsertRoomAttributionEmptyRoomsIsNoop(t *testing.T) {
	q := &syncTaskQuerierStub{}
	if err := UpsertRoomAttribution(context.Background(), q, RoomAttribution{BuildingID: "Q10"}); err != nil {
		t.Fatalf("空房号集合不应报错，实际 %v", err)
	}
	if q.execCalls != 0 {
		t.Fatalf("空房号集合不应执行 SQL，实际 %d 次", q.execCalls)
	}
}

// TestUpsertRoomAttributionRejectsBadInput 覆盖坏入参：
// 缺楼编号、空房号都报 ErrRunStoreFailed，且不执行 SQL。
func TestUpsertRoomAttributionRejectsBadInput(t *testing.T) {
	cases := []struct {
		name        string
		attribution RoomAttribution
	}{
		{name: "缺楼编号", attribution: RoomAttribution{Rooms: []AttributedRoom{{RoomID: "r101"}}}},
		{name: "楼编号只有空白", attribution: RoomAttribution{BuildingID: "  ", Rooms: []AttributedRoom{{RoomID: "r101"}}}},
		{name: "空房号", attribution: RoomAttribution{BuildingID: "Q10", Rooms: []AttributedRoom{{Name: "F101"}}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &syncTaskQuerierStub{}
			err := UpsertRoomAttribution(context.Background(), q, c.attribution)
			if !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("应报 ErrRunStoreFailed，实际 %v", err)
			}
			if q.execCalls != 0 {
				t.Fatalf("坏入参不应执行 SQL，实际 %d 次", q.execCalls)
			}
		})
	}
}

// TestUpsertRoomAttributionWrapsExecError 覆盖驱动错误：
// 包装成 ErrRunStoreFailed 上抛，不吞成写成成功。
func TestUpsertRoomAttributionWrapsExecError(t *testing.T) {
	execErr := errors.New("连接断开")
	q := &syncTaskQuerierStub{execErr: execErr}
	err := UpsertRoomAttribution(context.Background(), q, RoomAttribution{
		BuildingID: "Q10",
		Rooms:      []AttributedRoom{{RoomID: "r101", Name: "F101"}},
	})
	if !errors.Is(err, execErr) || !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应同时保留底层错误与 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestUpsertRoomAttributionOverwritesBuilding 覆盖用户拍板的「跨轮换楼覆盖」：
// DO UPDATE 里没有 building_id 守卫，一条 INSERT 直接把归属改写成新楼，
// 函数不再因换楼报 ErrRunStoreFailed；轮内首归属由 collect_run_room 的快照负责。
func TestUpsertRoomAttributionOverwritesBuilding(t *testing.T) {
	q := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 2")}}
	err := UpsertRoomAttribution(context.Background(), q, RoomAttribution{
		BuildingID: "Q11",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "F101"},
			{RoomID: "r201", Name: "F201"},
		},
	})
	if err != nil {
		t.Fatalf("跨轮换楼应正常覆盖，实际 %v", err)
	}
	// 整批一条 INSERT ... SELECT：PostgreSQL 单条语句自成一个隐式事务，
	// 任何一行失败都不会留下半批数据。
	if q.execCalls != 1 {
		t.Fatalf("整批应只执行 1 条 SQL，实际 %d 条", q.execCalls)
	}
	sql := q.execSQLs[0]
	if !strings.Contains(sql, "building_id = EXCLUDED.building_id") {
		t.Fatalf("换楼应覆盖 room.building_id，实际 %q", sql)
	}
	if strings.Contains(sql, "WHERE room.building_id") {
		t.Fatalf("DO UPDATE 不应保留换楼守卫，实际 %q", sql)
	}
}

// TestRecordAttrConflictsWritesGreatest 覆盖冲突回写：
// 一条 SQL 把冲突数按 GREATEST 落库，重复调用不累加；参数带轮次与时刻。
func TestRecordAttrConflictsWritesGreatest(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	q := &syncTaskQuerierStub{}

	if err := RecordAttrConflicts(context.Background(), q, 7, 3, now); err != nil {
		t.Fatalf("回写冲突数不应报错，实际 %v", err)
	}
	if q.execCalls != 1 {
		t.Fatalf("应只执行 1 条 SQL，实际 %d 条", q.execCalls)
	}
	if !strings.Contains(q.execSQLs[0], "GREATEST(attr_conflict_count, $2)") {
		t.Fatalf("SQL 应用 GREATEST 保持幂等，实际 %q", q.execSQLs[0])
	}
	args := q.execArgs[0]
	if len(args) != 3 || args[0] != int64(7) || args[1] != 3 || args[2] != now {
		t.Fatalf("冲突回写参数不符: %+v", args)
	}
}

// TestRecordAttrConflictsRejectsMissingRun 覆盖轮次不存在：
// 影响 0 行时报 ErrRunStoreFailed，不把落空的写入当成成功。
func TestRecordAttrConflictsRejectsMissingRun(t *testing.T) {
	q := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 0")}}
	err := RecordAttrConflicts(context.Background(), q, 7, 1, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("轮次不存在时应报 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestRecordAttrConflictsRejectsNegativeCount 覆盖非法冲突数：直接报错，不执行 SQL。
func TestRecordAttrConflictsRejectsNegativeCount(t *testing.T) {
	q := &syncTaskQuerierStub{}
	if err := RecordAttrConflicts(context.Background(), q, 7, -1, time.Now()); !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("负数冲突数应报 ErrRunStoreFailed，实际 %v", err)
	}
	if q.execCalls != 0 {
		t.Fatalf("非法冲突数不应执行 SQL，实际 %d 次", q.execCalls)
	}
}

// TestRecordAttrConflictsWrapsExecError 覆盖驱动错误：保留底层错误并包装哨兵。
func TestRecordAttrConflictsWrapsExecError(t *testing.T) {
	execErr := errors.New("连接断开")
	q := &syncTaskQuerierStub{execErr: execErr}
	err := RecordAttrConflicts(context.Background(), q, 7, 1, time.Now())
	if !errors.Is(err, execErr) || !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应同时保留底层错误与 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestUpsertRoomAttributionFallsBackDisplayName 覆盖展示名回退：
// 规范化结果为空时用原始名，原始名也为空时用房号，room.name 不写空串。
func TestUpsertRoomAttributionFallsBackDisplayName(t *testing.T) {
	q := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("UPDATE 2")}}
	attribution := RoomAttribution{
		BuildingID: "Q10",
		Rooms: []AttributedRoom{
			{RoomID: "r101", Name: "", NameRaw: "F101(60/0)"},
			{RoomID: "r102", Name: "  ", NameRaw: "  "},
		},
	}

	if err := UpsertRoomAttribution(context.Background(), q, attribution); err != nil {
		t.Fatalf("写入归属不应报错，实际 %v", err)
	}
	names := q.execArgs[0][2].([]string)
	if len(names) != 2 || names[0] != "F101(60/0)" || names[1] != "r102" {
		t.Fatalf("展示名回退不符: %v", names)
	}
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("展示名不应为空串: %v", names)
		}
	}
}

// TestUpsertRunRoomsWritesRows 覆盖每轮归属落库：
// 一次 Exec 写全部归属，重复房号合并冲突标记，参数按列对齐。
func TestUpsertRunRoomsWritesRows(t *testing.T) {
	now := time.Date(2026, 11, 4, 9, 30, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	q := &syncTaskQuerierStub{}
	rooms := []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
		{RoomID: "r102", BuildingID: "Q10"},
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
	}

	if err := UpsertRunRooms(context.Background(), q, 7, rooms, now); err != nil {
		t.Fatalf("写入每轮归属不应报错，实际 %v", err)
	}
	if q.execCalls != 1 {
		t.Fatalf("应只执行 1 条 SQL，实际 %d 条", q.execCalls)
	}
	if !strings.Contains(q.execSQLs[0], "INSERT INTO collect_run_room") {
		t.Fatalf("SQL 应写 collect_run_room，实际 %q", q.execSQLs[0])
	}
	if !strings.Contains(q.execSQLs[0], "ON CONFLICT (run_id, room_id) DO UPDATE") {
		t.Fatalf("SQL 应靠 ON CONFLICT 保持幂等，实际 %q", q.execSQLs[0])
	}
	if strings.Contains(q.execSQLs[0], "building_id = EXCLUDED.building_id") {
		t.Fatalf("每轮归属不应在冲突时改写楼编号，实际 %q", q.execSQLs[0])
	}
	args := q.execArgs[0]
	if len(args) != 6 || args[0].(int64) != 7 || !args[5].(time.Time).Equal(now) {
		t.Fatalf("每轮归属参数不符: %v", args)
	}
	if got := args[1].([]string); strings.Join(got, ",") != "r101,r102" {
		t.Fatalf("房号数组不符: %v", got)
	}
	if got := args[2].([]string); strings.Join(got, ",") != "Q10,Q10" {
		t.Fatalf("楼数组不符: %v", got)
	}
	if got := args[3].([]bool); len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("冲突标记数组不符: %v", got)
	}
	if got := args[4].([]string); got[0] != "Q11" || got[1] != "" {
		t.Fatalf("冲突楼数组不符: %v", got)
	}
}

// TestUpsertRunRoomsEmptyIsNoop 覆盖空归属：不执行 SQL。
func TestUpsertRunRoomsEmptyIsNoop(t *testing.T) {
	q := &syncTaskQuerierStub{}
	if err := UpsertRunRooms(context.Background(), q, 7, nil, time.Now()); err != nil {
		t.Fatalf("空归属不应报错，实际 %v", err)
	}
	if q.execCalls != 0 {
		t.Fatalf("空归属不应执行 SQL，实际 %d 次", q.execCalls)
	}
}

// TestUpsertRunRoomsRejectsBadInput 覆盖坏入参：空房号、空楼号、同批换楼，
// 以及冲突标记与被丢弃的楼不匹配（标了冲突却缺楼、没冲突却带着被丢弃的楼）
// 都报错且不执行 SQL——后者会让续跑 Seed 必报错，写侧不能落这种快照。
func TestUpsertRunRoomsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name  string
		rooms []RunRoom
	}{
		{name: "空房号", rooms: []RunRoom{{BuildingID: "Q10"}}},
		{name: "空楼号", rooms: []RunRoom{{RoomID: "r101"}}},
		{name: "同批换楼", rooms: []RunRoom{
			{RoomID: "r101", BuildingID: "Q10"},
			{RoomID: "r101", BuildingID: "Q11"},
		}},
		{name: "冲突缺被丢弃楼", rooms: []RunRoom{
			{RoomID: "r101", BuildingID: "Q10", Conflict: true},
		}},
		{name: "未冲突却带被丢弃楼", rooms: []RunRoom{
			{RoomID: "r101", BuildingID: "Q10", ConflictBuildingID: "Q11"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &syncTaskQuerierStub{}
			if err := UpsertRunRooms(context.Background(), q, 7, c.rooms, time.Now()); !errors.Is(err, model.ErrRunStoreFailed) {
				t.Fatalf("坏入参应报 ErrRunStoreFailed，实际 %v", err)
			}
			if q.execCalls != 0 {
				t.Fatalf("坏入参不应执行 SQL，实际 %d 次", q.execCalls)
			}
		})
	}
}

// TestUpsertRunRoomsRejectsMissingRun 覆盖轮次不存在或缺学期：
// SQL 影响 0 行时报 ErrRunStoreFailed，不把落空的写入当成成功。
func TestUpsertRunRoomsRejectsMissingRun(t *testing.T) {
	q := &syncTaskQuerierStub{execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 0")}}
	err := UpsertRunRooms(context.Background(), q, 7, []RunRoom{{RoomID: "r101", BuildingID: "Q10"}}, time.Now())
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("0 行写入应报 ErrRunStoreFailed，实际 %v", err)
	}
}

// TestLoadRunRoomsReadsRows 覆盖续跑读回：按行扫描冲突标记与冲突楼。
func TestLoadRunRoomsReadsRows(t *testing.T) {
	rows := &runRoomRowsStub{rooms: []RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		{RoomID: "r201", BuildingID: "Q11"},
	}}
	q := &syncTaskQuerierStub{queryRows: rows}

	rooms, err := LoadRunRooms(context.Background(), q, 7)
	if err != nil {
		t.Fatalf("读每轮归属不应报错，实际 %v", err)
	}
	if len(rooms) != 2 || fmt.Sprint(rooms) != fmt.Sprint([]RunRoom{
		{RoomID: "r101", BuildingID: "Q10", Conflict: true, ConflictBuildingID: "Q11"},
		{RoomID: "r201", BuildingID: "Q11"},
	}) {
		t.Fatalf("读回归属不符: %+v", rooms)
	}
	if q.queryCalls != 1 || q.queryArgs[0].(int64) != 7 {
		t.Fatalf("读归属参数不符: calls=%d args=%v", q.queryCalls, q.queryArgs)
	}
	if !rows.closed {
		t.Fatal("归属行应被关闭")
	}
}

// TestLoadRunRoomsWrapsErrors 覆盖查询与迭代错误统一包装成 ErrRunStoreFailed。
func TestLoadRunRoomsWrapsErrors(t *testing.T) {
	queryErr := errors.New("连接断开")
	if _, err := LoadRunRooms(context.Background(), &syncTaskQuerierStub{queryErr: queryErr}, 7); !errors.Is(err, queryErr) || !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("查询错误应包装成 ErrRunStoreFailed 并保留底层错误，实际 %v", err)
	}

	iterErr := errors.New("遍历断开")
	rows := &runRoomRowsStub{rooms: []RunRoom{{RoomID: "r101", BuildingID: "Q10"}}, iterErr: iterErr}
	if _, err := LoadRunRooms(context.Background(), &syncTaskQuerierStub{queryRows: rows}, 7); !errors.Is(err, iterErr) || !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("遍历错误应包装成 ErrRunStoreFailed 并保留底层错误，实际 %v", err)
	}
}

// TestRoomRepoDelegates 覆盖仓储封装：方法与同名函数行为一致，pipeline 只需窄接口。
func TestRoomRepoDelegates(t *testing.T) {
	q := &syncTaskQuerierStub{queryRows: &runRoomRowsStub{}}
	repo := NewRoomRepo(q)

	if err := repo.UpsertRoomAttribution(context.Background(), RoomAttribution{
		BuildingID: "Q10",
		Rooms:      []AttributedRoom{{RoomID: "r101", Name: "F101"}},
	}); err != nil {
		t.Fatalf("仓储写归属不应报错，实际 %v", err)
	}
	if err := repo.UpsertRunRooms(context.Background(), 7, []RunRoom{
		{RoomID: "r101", BuildingID: "Q10"},
	}, time.Now()); err != nil {
		t.Fatalf("仓储写每轮归属不应报错，实际 %v", err)
	}
	if _, err := repo.LoadRunRooms(context.Background(), 7); err != nil {
		t.Fatalf("仓储读每轮归属不应报错，实际 %v", err)
	}
	if err := repo.RecordAttrConflicts(context.Background(), 7, 1, time.Now()); err != nil {
		t.Fatalf("仓储写冲突数不应报错，实际 %v", err)
	}
	if q.execCalls != 3 || q.queryCalls != 1 {
		t.Fatalf("仓储方法调用不符: exec=%d query=%d", q.execCalls, q.queryCalls)
	}
}

// runRoomRowsStub 是 pgx.Rows 的最小替身，列顺序与 selectRunRoomsSQL 一致。
type runRoomRowsStub struct {
	rooms   []RunRoom
	index   int
	current RunRoom
	iterErr error
	closed  bool
}

// Close 实现 pgx.Rows。
func (s *runRoomRowsStub) Close() { s.closed = true }

// Err 实现 pgx.Rows。
func (s *runRoomRowsStub) Err() error { return s.iterErr }

// CommandTag 实现 pgx.Rows。
func (s *runRoomRowsStub) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

// FieldDescriptions 实现 pgx.Rows。
func (s *runRoomRowsStub) FieldDescriptions() []pgconn.FieldDescription { return nil }

// Next 实现 pgx.Rows。
func (s *runRoomRowsStub) Next() bool {
	if s.index >= len(s.rooms) {
		return false
	}
	s.current = s.rooms[s.index]
	s.index++
	return true
}

// Scan 实现 pgx.Rows：把当前行写进四个目标。
func (s *runRoomRowsStub) Scan(dest ...any) error {
	if len(dest) != 4 {
		return fmt.Errorf("期望 4 个扫描目标，实际 %d 个", len(dest))
	}
	*(dest[0].(*string)) = s.current.RoomID
	*(dest[1].(*string)) = s.current.BuildingID
	*(dest[2].(*bool)) = s.current.Conflict
	*(dest[3].(*string)) = s.current.ConflictBuildingID
	return nil
}

// Values 实现 pgx.Rows。
func (s *runRoomRowsStub) Values() ([]any, error) { return nil, nil }

// RawValues 实现 pgx.Rows。
func (s *runRoomRowsStub) RawValues() [][]byte { return nil }

// Conn 实现 pgx.Rows。
func (s *runRoomRowsStub) Conn() *pgx.Conn { return nil }

// TypeMap 实现 pgx.Rows。
func (s *runRoomRowsStub) TypeMap() *pgtype.Map { return nil }
