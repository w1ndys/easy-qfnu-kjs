package publish

// 本文件是候选集展开的单元测试：直接调 buildPlan，不经过校验与发布。
// 覆盖「已有周矩阵的周不被明细覆盖」「没有矩阵的周由明细范围派生」「范围读不出来的周保持未知」
// 与「候选集与节次轴对不上就展开失败」四类情形（需求 4.3、4.4、4.5）。

import (
	"errors"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// testDetail 造一条覆盖给定周次范围的说明，状态与课程由参数给出。
func testDetail(state model.StateKey, weekRange string) []CellDetail {
	return []CellDetail{{
		RoomID:  testRoomID(0),
		Weekday: 1,
		Block:   "0102",
		Details: []clean.Detail{{StateKey: state, Course: "西方史学史", WeekRange: weekRange}},
	}}
}

// observationAt 在展开结果里按位置找一行观测，用于断言某一格的状态。
func observationAt(built plan, roomID string, week, weekday int, node string) (model.Observation, bool) {
	for _, observation := range built.observations {
		// 四个定位字段都相等才算命中同一格
		if observation.RoomID == roomID && observation.Week == week &&
			observation.Weekday == weekday && observation.Node == node {
			return observation, true
		}
	}
	return model.Observation{}, false
}

// TestBuildPlanDerivesOnlyWeeksWithoutMatrix 断言只有没有周矩阵的周用明细范围派生状态。
func TestBuildPlanDerivesOnlyWeeksWithoutMatrix(t *testing.T) {
	candidate := testCandidate(1)
	candidate.Details = testDetail(model.StateClass, "1-3")

	built, err := buildPlan(candidate, testReleaseID)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}

	// 第 1 周有矩阵，闲忙只由矩阵决定：明细说上课也不改它
	first, ok := observationAt(built, testRoomID(0), 1, 1, "01")
	if !ok || first.StateKey != model.StateFree {
		t.Errorf("第 1 周第一大节 = %v，期望保持矩阵里的空闲", first.StateKey)
	}
	// 第 2 周与第 3 周没有矩阵，按范围派生上课；同一大节的两个小节状态一致
	for _, week := range []int{2, 3} {
		derived, found := observationAt(built, testRoomID(0), week, 1, "02")
		if !found || derived.StateKey != model.StateClass {
			t.Errorf("第 %d 周第一大节 = %v，期望派生成上课", week, derived.StateKey)
		}
	}
	// 范围之外的周没有观测：第 4 周不该被派生出来
	if _, found := observationAt(built, testRoomID(0), 4, 1, "01"); found {
		t.Error("第 4 周出现了观测，期望范围外的周没有数据")
	}
	// 派生只发生在周一：其它星期没有明细来源
	if _, found := observationAt(built, testRoomID(0), 2, 2, "01"); found {
		t.Error("周二第 2 周出现了观测，期望只有下钻过的格子有派生")
	}

	// 明细行标成派生，并保留课程与范围原文
	if len(built.details) != 1 {
		t.Fatalf("明细 %d 行，期望 1 行", len(built.details))
	}
	if !built.details[0].Derived || built.details[0].Course != "西方史学史" {
		t.Errorf("明细 = %+v，期望 derived = true 且带课程", built.details[0])
	}
}

// TestBuildPlanKeepsUnparsableRangeUnknown 断言周次范围读不出来的说明不派生任何周。
func TestBuildPlanKeepsUnparsableRangeUnknown(t *testing.T) {
	candidate := testCandidate(1)
	candidate.Details = testDetail(model.StateClass, "")

	built, err := buildPlan(candidate, testReleaseID)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}

	// 只有第 1 周矩阵的观测：范围读不出来时那些周保持未知，绝不按空闲处理
	if len(built.observations) != clean.DayCount*clean.NodeCount {
		t.Errorf("观测 %d 行，期望只有矩阵的 %d 行", len(built.observations), clean.DayCount*clean.NodeCount)
	}
	// 说明本身仍然入库，只是不参与派生
	if len(built.details) != 1 || built.details[0].Derived {
		t.Errorf("明细 = %+v，期望入库且 derived = false", built.details)
	}
}

// TestBuildPlanMarksDerivedOnlyForUncoveredWeeks 断言派生标记只在有未覆盖周时为真。
func TestBuildPlanMarksDerivedOnlyForUncoveredWeeks(t *testing.T) {
	// 范围完全落在已有矩阵的第 1 周里：这条说明不派生任何周
	covered := testCandidate(1)
	covered.Details = testDetail(model.StateClass, "1")
	built, err := buildPlan(covered, testReleaseID)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}
	if built.details[0].Derived {
		t.Error("明细的 derived = true，期望 false（范围没有覆盖任何没有矩阵的周）")
	}
}

// TestBuildPlanCountsDistinctRooms 断言房间总数按去重后的 jsbh 统计。
func TestBuildPlanCountsDistinctRooms(t *testing.T) {
	candidate := testCandidate(3)

	built, err := buildPlan(candidate, testReleaseID)
	if err != nil {
		t.Fatalf("展开失败: %v", err)
	}

	// 三间房各 7 天 × 12 小节，房间数按 jsbh 去重
	if built.rooms != 3 || len(built.observations) != 3*clean.DayCount*clean.NodeCount {
		t.Errorf("展开 = %d 间房、%d 行观测，期望 3 间、%d 行",
			built.rooms, len(built.observations), 3*clean.DayCount*clean.NodeCount)
	}
	// 空闲是可用状态，观测行要标成可用
	first := built.observations[0]
	if !first.Available || first.StateKey != model.StateFree {
		t.Errorf("观测首行 = (%s, %v)，期望空闲且可用", first.StateKey, first.Available)
	}
}

// TestBuildPlanRejectsBadCandidate 断言候选集与节次轴或周历对不上时展开直接失败。
func TestBuildPlanRejectsBadCandidate(t *testing.T) {
	rejected := []struct {
		name    string // 用例说明
		prepare func(candidate Candidate) Candidate
	}{
		{"节次轴小节数不对", func(candidate Candidate) Candidate {
			candidate.Axis = candidate.Axis[:clean.NodeCount-1]
			return candidate
		}},
		{"节次轴顺序不对", func(candidate Candidate) Candidate {
			candidate.Axis[0].Ordinal = 2
			return candidate
		}},
		{"周的周次不是正整数", func(candidate Candidate) Candidate {
			candidate.Weeks[0].Week = 0
			return candidate
		}},
		{"明细的大节不在轴上", func(candidate Candidate) Candidate {
			candidate.Details = testDetail(model.StateClass, "1-3")
			candidate.Details[0].Block = "0000"
			return candidate
		}},
		{"明细的星期越界", func(candidate Candidate) Candidate {
			candidate.Details = testDetail(model.StateClass, "1-3")
			candidate.Details[0].Weekday = clean.DayCount + 1
			return candidate
		}},
	}

	for _, testCase := range rejected {
		// 这些候选集都过不了展开，发布层必须在动数据库之前就拦下它们
		_, err := buildPlan(testCase.prepare(testCandidate(1)), testReleaseID)
		if !errors.Is(err, ErrInvalidCandidate) {
			t.Errorf("%s 的错误 = %v，期望 ErrInvalidCandidate", testCase.name, err)
		}
	}
}
