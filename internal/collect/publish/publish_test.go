package publish

// 本文件是发布层的单元测试：存储用内存实现替换，不连数据库、不发上游请求。
// 覆盖任务 7.1 的三项失败场景——重复 jsbh、表头块变化、发布事务失败——每项都断言 current 不变，
// 另加成功路径下 current / previous 的切换。真实事务的用例在 internal/store 里，未设
// TEST_DATABASE_URL 时跳过，所以这里用内存假存储把「失败不发布」钉住。

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// testTerm 是测试用的学期编号：真实存储里它必须已经在 term 表里。
const testTerm = "2026-2027-1"

// testOldRelease 是发布前已经在位的那一版。
const testOldRelease = "old-release"

// testReleaseID 是固定时钟算出的版本 id，与 newTestPublisher 的时钟配套。
const testReleaseID = "20261010041000.000000"

// testBlocks 是 5 个大节编码，与契约的表头顺序一致。
var testBlocks = []string{"0102", "030405", "0607", "0809", "101112"}

// fakeStore 是内存版发布存储：语义与 internal/store 一致——只有发布成功才换 current。
type fakeStore struct {
	done       int                  // 本轮已完成任务数
	total      int                  // 本轮任务总数
	current    string               // 当前版本 id
	previous   string               // 上一版 id
	publishErr error                // 注入的发布事务失败
	batches    []store.PublishBatch // 成功收到的发布批次
}

// SyncProgress 返回注入的任务进度。
func (s *fakeStore) SyncProgress(ctx context.Context, runID int64) (int, int, error) {
	return s.done, s.total, nil
}

// PublishRelease 记录发布批次并换 current：注入失败时什么都不改，current 保持原样。
func (s *fakeStore) PublishRelease(ctx context.Context, batch store.PublishBatch) error {
	// 事务失败时新版本没写成，current 与 previous 都不该动
	if s.publishErr != nil {
		return s.publishErr
	}
	s.previous = s.current
	s.current = batch.Release.ID
	s.batches = append(s.batches, batch)
	return nil
}

// newTestPublisher 建一个发布层，时钟固定，便于断言版本 id。
func newTestPublisher(fake *fakeStore) *Publisher {
	fixed := time.Date(2026, 10, 10, 4, 10, 0, 0, time.UTC)
	return NewPublisher(fake, func() time.Time { return fixed })
}

// testAxis 是契约种子节次轴：5 大节 → 12 小节，必须按展示顺序传入。
func testAxis() []model.AxisNode {
	return []model.AxisNode{
		{AxisVersion: 1, Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
		{AxisVersion: 1, Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		{AxisVersion: 1, Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
		{AxisVersion: 1, Node: "04", Ordinal: 4, Block: "030405", BlockOrdinal: 2},
		{AxisVersion: 1, Node: "05", Ordinal: 5, Block: "030405", BlockOrdinal: 2},
		{AxisVersion: 1, Node: "06", Ordinal: 6, Block: "0607", BlockOrdinal: 3},
		{AxisVersion: 1, Node: "07", Ordinal: 7, Block: "0607", BlockOrdinal: 3},
		{AxisVersion: 1, Node: "08", Ordinal: 8, Block: "0809", BlockOrdinal: 4},
		{AxisVersion: 1, Node: "09", Ordinal: 9, Block: "0809", BlockOrdinal: 4},
		{AxisVersion: 1, Node: "10", Ordinal: 10, Block: "101112", BlockOrdinal: 5},
		{AxisVersion: 1, Node: "11", Ordinal: 11, Block: "101112", BlockOrdinal: 5},
		{AxisVersion: 1, Node: "12", Ordinal: 12, Block: "101112", BlockOrdinal: 5},
	}
}

// freeRow 造一间房的全天空闲行：每个小节都判成空闲。
func freeRow(jsbh string) clean.Row {
	row := clean.Row{Jsbh: jsbh, Name: jsbh, NameRaw: jsbh + "(90/0)"}
	for day := 0; day < clean.DayCount; day++ {
		for node := 0; node < clean.NodeCount; node++ {
			row.Week[day][node] = clean.Cell{StateKey: model.StateFree}
		}
	}
	return row
}

// testRoomID 生成第 index 间房的 jsbh，测试里房间数可变。
func testRoomID(index int) string {
	return fmt.Sprintf("test-room-%03d", index+1)
}

// testCandidate 返回一份能通过校验的候选集：rooms 间房、只有第 1 周的矩阵、没有明细。
func testCandidate(rooms int) Candidate {
	rows := make([]clean.Row, 0, rooms)
	roomList := make([]model.Room, 0, rooms)
	for index := 0; index < rooms; index++ {
		roomID := testRoomID(index)
		rows = append(rows, freeRow(roomID))
		roomList = append(roomList, model.Room{ID: roomID, Name: roomID, BuildingID: "jxlbh-1", BuildingName: "综合楼"})
	}

	return Candidate{
		Term:        testTerm,
		DictVersion: 1,
		AxisVersion: 1,
		TotalWeeks:  16,
		Axis:        testAxis(),
		Rooms:       roomList,
		Weeks:       []MatrixWeek{{Week: 1, Matrix: clean.Matrix{Blocks: testBlocks, Rows: rows}}},
	}
}

// TestPublishSwitchesCurrentOnSuccess 断言发布成功后新版本成为 current、原 current 降为 previous。
func TestPublishSwitchesCurrentOnSuccess(t *testing.T) {
	fake := &fakeStore{done: 2, total: 2, current: testOldRelease}
	publisher := newTestPublisher(fake)

	result, err := publisher.Publish(context.Background(), 7, testCandidate(1))
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// 版本 id 由固定时钟算出，新版本顶到 current，原 current 留在 previous
	if result.ReleaseID != testReleaseID {
		t.Errorf("版本 id = %s，期望 %s", result.ReleaseID, testReleaseID)
	}
	if fake.current != testReleaseID || fake.previous != testOldRelease {
		t.Errorf("发布后 current = %s、previous = %s，期望 %s 与 %s",
			fake.current, fake.previous, testReleaseID, testOldRelease)
	}

	// 一间房 × 7 天 × 12 小节；观测行都带上这次发布的版本与学期
	if result.Observations != clean.DayCount*clean.NodeCount || result.Rooms != 1 {
		t.Errorf("观测 %d 行、房间 %d 间，期望 %d 行、1 间",
			result.Observations, result.Rooms, clean.DayCount*clean.NodeCount)
	}
	if len(fake.batches) != 1 {
		t.Fatalf("收到 %d 个发布批次，期望 1 个", len(fake.batches))
	}
	first := fake.batches[0].Observations[0]
	if first.ReleaseID != testReleaseID || first.Term != testTerm {
		t.Errorf("观测首行 = (%s, %s)，期望 (%s, %s)", first.ReleaseID, first.Term, testReleaseID, testTerm)
	}
	if fake.batches[0].Release.IsCurrent {
		t.Error("批次里的 IsCurrent = true，期望由发布事务自己切换")
	}
}

// TestPublishRequiresAllTasksDone 断言还有任务没完成时不发布，current 不变（需求 6.1）。
func TestPublishRequiresAllTasksDone(t *testing.T) {
	fake := &fakeStore{done: 3, total: 4, current: testOldRelease}
	publisher := newTestPublisher(fake)

	_, err := publisher.Publish(context.Background(), 7, testCandidate(1))
	// 完成数没追平总数，本轮不能发布
	if !errors.Is(err, ErrTasksIncomplete) {
		t.Fatalf("错误 = %v，期望 ErrTasksIncomplete", err)
	}
	if fake.current != testOldRelease {
		t.Errorf("current = %s，期望保持 %s", fake.current, testOldRelease)
	}
	if len(fake.batches) != 0 {
		t.Errorf("发起了 %d 次发布，期望一次都没有", len(fake.batches))
	}
}

// TestPublishKeepsCurrentOnStoreFailure 断言发布事务失败时 current 保持原样（需求 6.2）。
func TestPublishKeepsCurrentOnStoreFailure(t *testing.T) {
	fake := &fakeStore{
		done: 1, total: 1, current: testOldRelease,
		publishErr: errors.New("连接中断"),
	}
	publisher := newTestPublisher(fake)

	_, err := publisher.Publish(context.Background(), 7, testCandidate(1))
	// 事务失败的错误原样返回，调用方据此告警
	if err == nil || !errors.Is(err, fake.publishErr) {
		t.Fatalf("错误 = %v，期望带出发布事务失败的原因", err)
	}
	if fake.current != testOldRelease {
		t.Errorf("current = %s，期望保持 %s", fake.current, testOldRelease)
	}
	if fake.previous != "" {
		t.Errorf("previous = %s，期望为空（原 current 不该被降下）", fake.previous)
	}
}
