package publish

import (
	"errors"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// validCandidate 是一份能通过全部校验的候选集：一条观测、一条说明、
// 归属快照覆盖该房号、两条必需任务都完成。
func validCandidate() Candidate {
	return Candidate{
		RunID:         1,
		Term:          "2026-2027-1",
		RequiredTotal: 2,
		RequiredDone:  2,
		Rooms: []RoomBuilding{
			{RoomID: "r101", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Observations: []Observation{
			{RoomID: "r101", Week: 5, Weekday: 1, Node: "01", StateKey: model.StateKeyFree, Available: true},
		},
		Details: []OccupancyDetail{
			{RoomID: "r101", Weekday: 1, Block: "0102", StateKey: "class", WeekRange: "5-8"},
		},
	}
}

// TestValidateAcceptsCompleteCandidate 覆盖通过路径：完整候选与不限制的阈值返回 nil。
func TestValidateAcceptsCompleteCandidate(t *testing.T) {
	if err := Validate(validCandidate(), Thresholds{}); err != nil {
		t.Fatalf("完整候选不应被拦截: %v", err)
	}
}

// TestValidateRejectsEmptyCandidate 覆盖 Requirement 7.5 的「候选集为空 → 拒绝」，
// 并检查拦截原因可取。
func TestValidateRejectsEmptyCandidate(t *testing.T) {
	candidate := validCandidate()
	candidate.Observations = nil

	err := Validate(candidate, Thresholds{})
	if !errors.Is(err, ErrEmptyCandidate) {
		t.Fatalf("空候选应返回 ErrEmptyCandidate，实际 err=%v", err)
	}
	if reason := ValidationReason(err); reason != ReasonEmptyCandidate {
		t.Fatalf("拦截原因不符: got %q want %q", reason, ReasonEmptyCandidate)
	}
}

// TestValidateRejectsIncompleteRequiredTasks 覆盖必需任务门槛：
// 未完成、失败、一条都没有、完成数超过总数都拒绝发布。
func TestValidateRejectsIncompleteRequiredTasks(t *testing.T) {
	cases := []struct {
		name   string
		total  int
		done   int
		failed int
	}{
		{"没有必需任务", 0, 0, 0},
		{"尚未完成", 2, 1, 0},
		{"有失败任务", 2, 1, 1},
		{"完成数超过总数", 2, 3, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validCandidate()
			candidate.RequiredTotal = tc.total
			candidate.RequiredDone = tc.done
			candidate.RequiredFailed = tc.failed

			err := Validate(candidate, Thresholds{})
			if !errors.Is(err, ErrRequiredTasksIncomplete) {
				t.Fatalf("必需任务不完整应返回 ErrRequiredTasksIncomplete，实际 err=%v", err)
			}
			if reason := ValidationReason(err); reason != ReasonRequiredIncomplete {
				t.Fatalf("拦截原因不符: got %q want %q", reason, ReasonRequiredIncomplete)
			}
		})
	}
}

// TestValidateRejectsIncompleteAttribution 覆盖归属快照完整性（CP2）：
// 观测或说明的房号缺归属、归属缺楼编号/楼名、房号重复都拒绝发布。
func TestValidateRejectsIncompleteAttribution(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate)
	}{
		{"观测房号缺归属", func(c *Candidate) {
			c.Observations[0].RoomID = "r999"
		}},
		{"说明房号缺归属", func(c *Candidate) {
			c.Details[0].RoomID = "r999"
		}},
		{"归属缺楼编号", func(c *Candidate) {
			c.Rooms[0].BuildingID = " "
		}},
		{"归属缺楼名", func(c *Candidate) {
			c.Rooms[0].BuildingName = ""
		}},
		{"房号重复", func(c *Candidate) {
			c.Rooms = append(c.Rooms, c.Rooms[0])
		}},
		{"空房号", func(c *Candidate) {
			c.Rooms = append(c.Rooms, RoomBuilding{BuildingID: "Q10", BuildingName: "综合教学楼"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validCandidate()
			tc.mutate(&candidate)

			err := Validate(candidate, Thresholds{})
			if !errors.Is(err, ErrAttributionIncomplete) {
				t.Fatalf("归属不完整应返回 ErrAttributionIncomplete，实际 err=%v", err)
			}
			if reason := ValidationReason(err); reason != ReasonAttributionIncomplete {
				t.Fatalf("拦截原因不符: got %q want %q", reason, ReasonAttributionIncomplete)
			}
		})
	}
}

// TestValidateEnforcesUnknownAndCompositeThresholds 覆盖未知/复合阈值：
// 只有显式配置正数阈值才拦截，等于上限放行，超过上限拒绝。
func TestValidateEnforcesUnknownAndCompositeThresholds(t *testing.T) {
	cases := []struct {
		name       string
		unknown    int
		composite  int
		thresholds Thresholds
		want       error
		reason     string
	}{
		{"未配置阈值不限制", 100, 100, Thresholds{}, nil, ""},
		{"未知等于上限", 3, 0, Thresholds{MaxUnknownCells: 3}, nil, ""},
		{"未知超过上限", 4, 0, Thresholds{MaxUnknownCells: 3}, ErrUnknownCellsExceeded, ReasonUnknownExceeded},
		{"复合等于上限", 0, 2, Thresholds{MaxCompositeCells: 2}, nil, ""},
		{"复合超过上限", 0, 3, Thresholds{MaxCompositeCells: 2}, ErrCompositeCellsExceeded, ReasonCompositeExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := validCandidate()
			candidate.UnknownCells = tc.unknown
			candidate.CompositeCells = tc.composite

			err := Validate(candidate, tc.thresholds)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("不应被拦截: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("阈值拦截错误不符: got %v want %v", err, tc.want)
			}
			if reason := ValidationReason(err); reason != tc.reason {
				t.Fatalf("拦截原因不符: got %q want %q", reason, tc.reason)
			}
		})
	}
}

// TestValidationReasonNonValidationError 覆盖非校验拦截：普通错误没有拦截原因。
func TestValidationReasonNonValidationError(t *testing.T) {
	if reason := ValidationReason(errors.New("普通错误")); reason != "" {
		t.Fatalf("普通错误不应有拦截原因，实际 %q", reason)
	}
	if reason := ValidationReason(nil); reason != "" {
		t.Fatalf("nil 不应有拦截原因，实际 %q", reason)
	}
}
