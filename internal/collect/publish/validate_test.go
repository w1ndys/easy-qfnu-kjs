package publish

// 本文件是发布前校验的单元测试：每一条阻断规则都要既报出对应错误、又不发起发布。
// 覆盖任务 7.1 的重复 jsbh 与表头块变化两项，另加空候选集与未知只计数。房间总数变化不阻断发布。
// 「未知符号只计数不阻断」。存储是内存实现，current 是否被换掉在断言里直接看得见。

import (
	"context"
	"errors"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestPublishBlocksDuplicateBuilding 断言同一 jsbh 归属两栋楼时本轮失败且不发布（需求 2.5）。
func TestPublishBlocksDuplicateBuilding(t *testing.T) {
	fake := &fakeStore{done: 1, total: 1, current: testOldRelease}
	publisher := newTestPublisher(fake)

	candidate := testCandidate(1)
	// 同一间房第二次出现在另一栋楼的结果里：房名一样，楼编号不同
	candidate.Rooms = append(candidate.Rooms, model.Room{
		ID: testRoomID(0), Name: testRoomID(0), BuildingID: "jxlbh-2", BuildingName: "教学楼二号",
	})

	_, err := publisher.Publish(context.Background(), 7, candidate)
	// 归属冲突属于整轮失败，不挑一栋覆盖
	if !errors.Is(err, ErrRoomBuildingConflict) {
		t.Fatalf("错误 = %v，期望 ErrRoomBuildingConflict", err)
	}
	if fake.current != testOldRelease {
		t.Errorf("current = %s，期望保持 %s", fake.current, testOldRelease)
	}
	if len(fake.batches) != 0 {
		t.Errorf("发起了 %d 次发布，期望一次都没有", len(fake.batches))
	}
}

// TestPublishBlocksAxisChange 断言调用方传入的轴变化错误阻止发布，且不被吞掉（需求 3.5）。
func TestPublishBlocksAxisChange(t *testing.T) {
	fake := &fakeStore{done: 1, total: 1, current: testOldRelease}
	publisher := newTestPublisher(fake)

	candidate := testCandidate(1)
	// 表头块集合变化在清洗层已经定为整轮停止，发布层必须原样把它带出去
	candidate.StopErr = clean.ErrAxisChanged

	_, err := publisher.Publish(context.Background(), 7, candidate)
	// 轴变化的原因要能从返回的错误里查出来，否则调用方无法区分该告警哪一类
	if !errors.Is(err, clean.ErrAxisChanged) {
		t.Fatalf("错误 = %v，期望保留 clean.ErrAxisChanged", err)
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("错误 = %v，期望同时是 ErrValidation", err)
	}
	if fake.current != testOldRelease {
		t.Errorf("current = %s，期望保持 %s", fake.current, testOldRelease)
	}
	if len(fake.batches) != 0 {
		t.Errorf("发起了 %d 次发布，期望一次都没有", len(fake.batches))
	}
}

// TestPublishAllowsFirstRelease 断言还没有 current 时第一次发布照常通过。
func TestPublishAllowsFirstRelease(t *testing.T) {
	fake := &fakeStore{done: 1, total: 1}
	publisher := newTestPublisher(fake)

	_, err := publisher.Publish(context.Background(), 7, testCandidate(1))
	if err != nil {
		t.Fatalf("首次发布失败: %v", err)
	}
	// 没有上一版时也不该留下 previous
	if fake.previous != "" {
		t.Errorf("首次发布 previous = %q，期望空", fake.previous)
	}
	if fake.current != testReleaseID {
		t.Errorf("current = %s，期望 %s", fake.current, testReleaseID)
	}
}

// TestPublishBlocksEmptyCandidate 断言没有观测的候选集不发布：它会让查询侧丢掉全部数据。
func TestPublishBlocksEmptyCandidate(t *testing.T) {
	fake := &fakeStore{done: 1, total: 1, current: testOldRelease}
	publisher := newTestPublisher(fake)

	candidate := testCandidate(0)
	// 一间房都没有，也没有明细可以派生：这一版没有任何可发布的事实
	candidate.Weeks = nil

	_, err := publisher.Publish(context.Background(), 7, candidate)
	if !errors.Is(err, ErrEmptyPlan) {
		t.Fatalf("错误 = %v，期望 ErrEmptyPlan", err)
	}
	if fake.current != testOldRelease {
		t.Errorf("current = %s，期望保持 %s", fake.current, testOldRelease)
	}
	if len(fake.batches) != 0 {
		t.Errorf("发起了 %d 次发布，期望一次都没有", len(fake.batches))
	}
}

// TestPublishCountsUnknownWithoutBlocking 断言未知与复合符号只计数，不阻断发布。
func TestPublishCountsUnknownWithoutBlocking(t *testing.T) {
	fake := &fakeStore{done: 1, total: 1}
	publisher := newTestPublisher(fake)

	candidate := testCandidate(1)
	// 周一第一大节判成未知、周二第一大节判成复合：都只是计数，不是失败
	// 同一大节里两个小节状态相同（清洗层的展开保证），所以两个下标一起改
	candidate.Weeks[0].Matrix.Rows[0].Week[0][0] = clean.Cell{StateKey: model.StateUnknown, RawText: "??"}
	candidate.Weeks[0].Matrix.Rows[0].Week[0][1] = clean.Cell{StateKey: model.StateUnknown, RawText: "??"}
	candidate.Weeks[0].Matrix.Rows[0].Week[1][0] = clean.Cell{StateKey: model.StateComposite, RawText: "◆Ｊ"}
	candidate.Weeks[0].Matrix.Rows[0].Week[1][1] = clean.Cell{StateKey: model.StateComposite, RawText: "◆Ｊ"}

	result, err := publisher.Publish(context.Background(), 7, candidate)
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	// 计数按大节格算：一个大节格展开成几个小节也只算一格
	if result.UnknownCells != 1 || result.CompositeCells != 1 {
		t.Errorf("计数 = 未知 %d、复合 %d，期望各 1", result.UnknownCells, result.CompositeCells)
	}
	// 未知格不可用：它不能被当成空闲
	if !containsUnavailable(fake, model.StateUnknown) {
		t.Error("未知格没有按不可用写入观测")
	}
}

// containsUnavailable 在收到的观测里找某个状态且不可用的行，用来确认可用性投影。
func containsUnavailable(fake *fakeStore, state model.StateKey) bool {
	for _, observation := range fake.batches[0].Observations {
		// 命中目标状态时要它同时标成不可用，两者不一致就说明投影写错了
		if observation.StateKey == state && !observation.Available {
			return true
		}
	}
	return false
}
