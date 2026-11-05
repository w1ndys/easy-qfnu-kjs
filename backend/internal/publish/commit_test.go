package publish

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// committerStub 记录交给写侧的草稿，并按用例返回失败，用于验证 publish.Commit
// 只做「转草稿 + 生成版本标识 + 交给仓储」这三件事。
type committerStub struct {
	calls int
	draft store.ReleaseDraft
	err   error
}

// CommitRelease 实现 Committer。
func (c *committerStub) CommitRelease(_ context.Context, draft store.ReleaseDraft) error {
	c.calls++
	c.draft = draft
	return c.err
}

// commitFixtureTimes 返回固定的版本生成时刻（UTC）与发布时刻（Asia/Shanghai）。
// 生成时刻故意用 UTC：版本标识的格式是 `<UTC 秒级>Z`，必须先把入参换算到 UTC
// 再格式化（结果恰好与 UTC 入参相同），不能照抄入参所在时区、也不能用业务时区的
// 墙上时间；落库的 generated_at / updated_at 才换算到业务时区。
func commitFixtureTimes() (generatedAt, now time.Time) {
	generatedAt = time.Date(2026, 11, 4, 14, 2, 51, 0, time.UTC)
	now = time.Date(2026, 11, 4, 22, 5, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
	return generatedAt, now
}

// commitCandidateFixture 是一份可发布的候选集：一行观测、一行说明、一行归属。
func commitCandidateFixture() Candidate {
	generatedAt, _ := commitFixtureTimes()
	return Candidate{
		RunID:           42,
		Term:            " 2026-2027-1 ",
		GeneratedAt:     generatedAt,
		DictVersion:     1,
		AxisVersion:     1,
		WindowStart:     5,
		WindowEnd:       8,
		HasAttrConflict: true,
		RequiredTotal:   4,
		RequiredDone:    4,
		OptionalTotal:   3,
		OptionalDone:    3,
		RoomsTotal:      1,
		WeeksTotal:      4,
		UnknownCells:    2,
		CompositeCells:  1,
		Rooms: []RoomBuilding{
			{RoomID: "r101", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Observations: []Observation{
			{
				RoomID: "r101", Week: 5, Weekday: 1, Node: "01",
				StateKey: "free", Available: true,
			},
		},
		Details: []OccupancyDetail{
			{
				RoomID: "r101", Weekday: 1, Block: "0102", StateKey: "class",
				Course: "高等数学", WeekRange: "1-16", TimeFlag: "单双周", Derived: true,
			},
		},
	}
}

// TestCommitMapsCandidateToDraft 覆盖候选集到写侧草稿的完整映射：
// 版本标识按 UTC 生成、学期去空白、观测/说明/归属原样搬运、
// 未知/复合/房号数/窗口周数进轮次汇总、终态与两个时间字段各自换算。
func TestCommitMapsCandidateToDraft(t *testing.T) {
	generatedAt, now := commitFixtureTimes()
	committer := &committerStub{}

	result, err := Commit(context.Background(), committer, commitCandidateFixture(), now)
	if err != nil {
		t.Fatalf("发布不应报错，实际 %v", err)
	}
	if committer.calls != 1 {
		t.Fatalf("应提交 1 次，实际 %d 次", committer.calls)
	}

	// 生成时刻 14:02:51Z 换算到 UTC 仍是 14:02:51，版本标识必须用 UTC + Z。
	if want := "20261104T140251Z"; result.ReleaseID != want {
		t.Fatalf("版本标识不符: got %q want %q", result.ReleaseID, want)
	}

	draft := committer.draft
	if draft.ReleaseID != result.ReleaseID {
		t.Fatalf("草稿版本标识应与结果一致: %q vs %q", draft.ReleaseID, result.ReleaseID)
	}
	if draft.RunID != 42 || draft.Term != "2026-2027-1" {
		t.Fatalf("轮次/学期映射不符: %+v", draft)
	}
	if draft.DictVersion != 1 || draft.AxisVersion != 1 || !draft.HasAttrConflict {
		t.Fatalf("版本身份映射不符: %+v", draft)
	}
	if draft.UnknownCells != 2 || draft.CompositeCells != 1 {
		t.Fatalf("未知/复合计数映射不符: %+v", draft)
	}
	if draft.RoomsTotal != 1 || draft.WeeksTotal != 4 {
		t.Fatalf("房号数/窗口周数映射不符: %+v", draft)
	}
	// 窗口随草稿交给写层做交叉校验：WeeksTotal 必须等于窗口跨度、
	// RoomsTotal 必须等于归属快照行数，缺了窗口就核不了前一条。
	if draft.WindowStart != 5 || draft.WindowEnd != 8 {
		t.Fatalf("采集窗口映射不符: %+v", draft)
	}
	if draft.Status != store.RunStatusSuccess {
		t.Fatalf("可选任务全部完成应写 success，实际 %q", draft.Status)
	}
	if !draft.GeneratedAt.Equal(generatedAt) || !draft.UpdatedAt.Equal(now) {
		t.Fatalf("时间字段不应改变时刻: generated=%s updated=%s", draft.GeneratedAt, draft.UpdatedAt)
	}
	if draft.GeneratedAt.Location() != shanghai || draft.UpdatedAt.Location() != shanghai {
		t.Fatalf("时间字段应换算到 Asia/Shanghai: %v / %v",
			draft.GeneratedAt.Location(), draft.UpdatedAt.Location())
	}

	if len(draft.Rooms) != 1 || draft.Rooms[0] != (store.ReleaseRoom{
		RoomID: "r101", BuildingID: "Q10", BuildingName: "综合教学楼",
	}) {
		t.Fatalf("归属快照映射不符: %+v", draft.Rooms)
	}
	if len(draft.Observations) != 1 {
		t.Fatalf("观测行数不符: %+v", draft.Observations)
	}
	if got, want := draft.Observations[0], (store.ReleaseObservation{
		RoomID: "r101", Week: 5, Weekday: 1, Node: "01",
		StateKey: "free", Available: true,
	}); got != want {
		t.Fatalf("观测映射不符: got %+v want %+v", got, want)
	}
	if len(draft.Details) != 1 {
		t.Fatalf("说明行数不符: %+v", draft.Details)
	}
	if got, want := draft.Details[0], (store.ReleaseDetail{
		RoomID: "r101", Weekday: 1, Block: "0102", StateKey: "class",
		Course: "高等数学", WeekRange: "1-16", TimeFlag: "单双周", Derived: true,
	}); got != want {
		t.Fatalf("说明映射不符: got %+v want %+v", got, want)
	}
}

// TestCommitPartialWritesErrorMessage 覆盖用户拍板：partial 把失败原因写进
// collect_run.error_message，口径与 pipeline.CloseRun 的 partial 分支一致——
// 有可选失败写最早失败任务的原因，只有被归属过滤丢弃的可选明细时写一句丢弃计数；
// success 传空串，由写层清空。
func TestCommitPartialWritesErrorMessage(t *testing.T) {
	_, now := commitFixtureTimes()

	t.Run("可选失败写最早失败原因", func(t *testing.T) {
		candidate := commitCandidateFixture()
		candidate.OptionalDone = 2
		candidate.OptionalFailed = 1
		candidate.FirstFailureMessage = "明细页结构异常（task 12）"
		committer := &committerStub{}

		if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
			t.Fatalf("发布不应报错，实际 %v", err)
		}
		if committer.draft.Status != store.RunStatusPartial {
			t.Fatalf("有可选失败应写 partial，实际 %q", committer.draft.Status)
		}
		if committer.draft.ErrorMessage != candidate.FirstFailureMessage {
			t.Fatalf("partial 应写最早失败原因: got %q want %q",
				committer.draft.ErrorMessage, candidate.FirstFailureMessage)
		}
	})

	t.Run("只有丢弃写丢弃说明", func(t *testing.T) {
		candidate := commitCandidateFixture()
		candidate.DroppedOptional = 2
		committer := &committerStub{}

		if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
			t.Fatalf("丢弃可选明细不影响发布，实际 %v", err)
		}
		if committer.draft.Status != store.RunStatusPartial {
			t.Fatalf("丢弃可选明细应写 partial，实际 %q", committer.draft.Status)
		}
		// 库里没有 failed 行可引用，写一句丢弃计数；文案与 CloseRun.droppedMessage 一致。
		if want := "归属校验丢弃 2 条明细任务（必需 0 条、可选 2 条）"; committer.draft.ErrorMessage != want {
			t.Fatalf("丢弃原因文案不符: got %q want %q", committer.draft.ErrorMessage, want)
		}
	})

	t.Run("success 清空", func(t *testing.T) {
		candidate := commitCandidateFixture()
		// 人为塞一条原因，证明清空是按终态做的，不是「候选集恰好没带」。
		candidate.FirstFailureMessage = "上一状态残留的原因"
		committer := &committerStub{}

		if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
			t.Fatalf("发布不应报错，实际 %v", err)
		}
		if committer.draft.Status != store.RunStatusSuccess {
			t.Fatalf("可选任务全部完成应写 success，实际 %q", committer.draft.Status)
		}
		if committer.draft.ErrorMessage != "" {
			t.Fatalf("success 必须清空 error_message，实际 %q", committer.draft.ErrorMessage)
		}
	})
}

// TestReleaseIdentifierUsesUTCWithZuluSuffix 覆盖版本标识的格式（用户拍板）：
// 固定为 `<UTC 秒级>Z`，与入参时区无关、毫秒截断，且始终以 Z 结尾。
func TestReleaseIdentifierUsesUTCWithZuluSuffix(t *testing.T) {
	cases := []struct {
		name   string
		moment time.Time
		want   string
	}{
		{"UTC", time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC), "20261023T071012Z"},
		{
			"东八区同一时刻",
			time.Date(2026, 10, 23, 15, 10, 12, 0, time.FixedZone("Asia/Shanghai", 8*60*60)),
			"20261023T071012Z",
		},
		{
			"西五区同一时刻",
			time.Date(2026, 10, 23, 2, 10, 12, 0, time.FixedZone("EST", -5*60*60)),
			"20261023T071012Z",
		},
		{
			"毫秒被截断",
			time.Date(2026, 10, 23, 7, 10, 12, 999*int(time.Millisecond), time.UTC),
			"20261023T071012Z",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			candidate := commitCandidateFixture()
			candidate.GeneratedAt = c.moment
			if got := releaseIdentifier(candidate); got != c.want {
				t.Fatalf("版本标识不符: got %q want %q", got, c.want)
			}
		})
	}

	// 格式断言：8 位日期 + T + 6 位时间 + Z，不夹带轮次编号或时区偏移。
	pattern := regexp.MustCompile(`^\d{8}T\d{6}Z$`)
	if got := releaseIdentifier(commitCandidateFixture()); !pattern.MatchString(got) {
		t.Fatalf("版本标识格式不符（应匹配 %s）: %q", pattern, got)
	}
}

// TestReleaseIdentifierIsDeterministic 覆盖版本标识的稳定口径：
// 同一生成时刻恒定；标识只由生成时刻决定，同秒内不同轮次得到同一个标识
// （同 ID 由 store 的发布前置检查拒绝，不再靠轮次编号规避）。
func TestReleaseIdentifierIsDeterministic(t *testing.T) {
	candidate := commitCandidateFixture()
	first := releaseIdentifier(candidate)
	if second := releaseIdentifier(candidate); second != first {
		t.Fatalf("同一生成时刻的标识应稳定: %q vs %q", first, second)
	}

	other := candidate
	other.RunID = 43
	if releaseIdentifier(other) != first {
		t.Fatalf("标识只由生成时刻决定，同秒不同轮次应相同: got %q want %q",
			releaseIdentifier(other), first)
	}

	later := candidate
	later.GeneratedAt = later.GeneratedAt.Add(time.Second)
	if releaseIdentifier(later) == first {
		t.Fatalf("不同生成秒应有不同标识: %q", first)
	}
}

// TestCommitStatusFromDroppedDetails 覆盖归属过滤丢弃的明细任务对发布终态的影响
// （与 pipeline.decideOutcome 同口径）：丢弃可选明细 → partial；丢弃必需明细 →
// 拒绝发布。这批任务从未写进 sync_task，任务汇总里看不到，只能靠候选集上的
// DroppedRequired/DroppedOptional 带进来。
//
// 注意与 Candidate.DroppedDetails 区分：那是 Build 期因周次范围整条丢弃的「说明」
// 数，只用于解释说明条数，不参与发布终态——它非零时终态仍是 success。
func TestCommitStatusFromDroppedDetails(t *testing.T) {
	t.Run("归属过滤丢弃可选明细 → partial", func(t *testing.T) {
		candidate := commitCandidateFixture()
		candidate.DroppedOptional = 2
		committer := &committerStub{}
		_, now := commitFixtureTimes()

		if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
			t.Fatalf("丢弃可选明细不影响发布，实际 %v", err)
		}
		if committer.draft.Status != store.RunStatusPartial {
			t.Fatalf("丢弃可选明细应写 partial，实际 %q", committer.draft.Status)
		}
	})

	t.Run("归属过滤丢弃必需明细 → 拒绝发布", func(t *testing.T) {
		candidate := commitCandidateFixture()
		candidate.DroppedRequired = 1
		committer := &committerStub{}
		_, now := commitFixtureTimes()

		_, err := Commit(context.Background(), committer, candidate, now)
		if !errors.Is(err, ErrRequiredTasksIncomplete) {
			t.Fatalf("丢弃必需明细应返回 ErrRequiredTasksIncomplete，实际 %v", err)
		}
		if reason := ValidationReason(err); reason != ReasonRequiredIncomplete {
			t.Fatalf("拦截原因不符: got %q want %q", reason, ReasonRequiredIncomplete)
		}
		if committer.calls != 0 {
			t.Fatalf("丢弃必需明细不应提交，实际提交 %d 次", committer.calls)
		}
	})

	t.Run("Build 期丢弃说明不影响终态", func(t *testing.T) {
		candidate := commitCandidateFixture()
		candidate.DroppedDetails = 5
		committer := &committerStub{}
		_, now := commitFixtureTimes()

		if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
			t.Fatalf("丢弃说明不影响发布，实际 %v", err)
		}
		if committer.draft.Status != store.RunStatusSuccess {
			t.Fatalf("DroppedDetails 不参与终态，应写 success，实际 %q", committer.draft.Status)
		}
	})
}

// TestCommitStatusFromOptionalTasks 覆盖发布终态的映射：可选任务全部完成 → success，
// 有可选失败 → partial；还有 pending 时不写终态（与 pipeline 的 decideOutcome 同口径）。
func TestCommitStatusFromOptionalTasks(t *testing.T) {
	cases := []struct {
		name   string
		total  int
		done   int
		failed int
		want   string
	}{
		{name: "可选全部完成", total: 3, done: 3, failed: 0, want: store.RunStatusSuccess},
		{name: "有可选失败", total: 3, done: 2, failed: 1, want: store.RunStatusPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			candidate := commitCandidateFixture()
			candidate.OptionalTotal = c.total
			candidate.OptionalDone = c.done
			candidate.OptionalFailed = c.failed
			committer := &committerStub{}
			_, now := commitFixtureTimes()

			if _, err := Commit(context.Background(), committer, candidate, now); err != nil {
				t.Fatalf("发布不应报错，实际 %v", err)
			}
			if committer.draft.Status != c.want {
				t.Fatalf("发布终态不符: got %q want %q", committer.draft.Status, c.want)
			}
		})
	}

	// 还有可选任务没跑完：写 success/partial 都会把「还没做」记成「已完成」。
	candidate := commitCandidateFixture()
	candidate.OptionalTotal = 3
	candidate.OptionalDone = 1
	candidate.OptionalFailed = 1
	committer := &committerStub{}
	_, now := commitFixtureTimes()
	if _, err := Commit(context.Background(), committer, candidate, now); !errors.Is(err, ErrCandidateInputInvalid) {
		t.Fatalf("可选任务未跑完应返回 ErrCandidateInputInvalid，实际 %v", err)
	}
	if committer.calls != 0 {
		t.Fatalf("可选任务未跑完不应提交，实际提交 %d 次", committer.calls)
	}
}

// TestCommitRejectsEmptyObservations 覆盖没有观测的候选集：
// 与 Validate 同口径返回 ReasonEmptyCandidate，且不交给仓储。
func TestCommitRejectsEmptyObservations(t *testing.T) {
	candidate := commitCandidateFixture()
	candidate.Observations = nil
	committer := &committerStub{}
	_, now := commitFixtureTimes()

	_, err := Commit(context.Background(), committer, candidate, now)
	if !errors.Is(err, ErrEmptyCandidate) {
		t.Fatalf("空候选集应返回 ErrEmptyCandidate，实际 %v", err)
	}
	if reason := ValidationReason(err); reason != ReasonEmptyCandidate {
		t.Fatalf("拦截原因不符: got %q want %q", reason, ReasonEmptyCandidate)
	}
	if committer.calls != 0 {
		t.Fatalf("被拦截的候选集不应提交，实际提交 %d 次", committer.calls)
	}
}

// TestCommitRejectsInvalidIdentity 覆盖版本身份字段缺失：开事务之前就拒绝，
// 不把坏数据交给仓储去撞约束。
func TestCommitRejectsInvalidIdentity(t *testing.T) {
	_, now := commitFixtureTimes()
	cases := map[string]func(*Candidate, *time.Time){
		"轮次编号非法": func(c *Candidate, _ *time.Time) { c.RunID = 0 },
		"缺学期":    func(c *Candidate, _ *time.Time) { c.Term = "   " },
		"缺生成时间":  func(c *Candidate, _ *time.Time) { c.GeneratedAt = time.Time{} },
		"缺发布时刻":  func(_ *Candidate, now *time.Time) { *now = time.Time{} },
		"字典版本非法": func(c *Candidate, _ *time.Time) { c.DictVersion = 0 },
		"轴版本非法":  func(c *Candidate, _ *time.Time) { c.AxisVersion = -1 },
		"房号数非法":  func(c *Candidate, _ *time.Time) { c.RoomsTotal = 0 },
		"窗口周数非法": func(c *Candidate, _ *time.Time) { c.WeeksTotal = 0 },
		"窗口非法":   func(c *Candidate, _ *time.Time) { c.WindowStart = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := commitCandidateFixture()
			publishAt := now
			mutate(&candidate, &publishAt)
			committer := &committerStub{}

			_, err := Commit(context.Background(), committer, candidate, publishAt)
			if !errors.Is(err, ErrCandidateInputInvalid) {
				t.Fatalf("坏候选集应返回 ErrCandidateInputInvalid，实际 %v", err)
			}
			if committer.calls != 0 {
				t.Fatalf("坏候选集不应提交，实际提交 %d 次", committer.calls)
			}
		})
	}
}

// TestCommitPropagatesCommitterError 覆盖仓储失败：错误带发布上下文上抛，
// errors.Is 仍能命中仓储层哨兵，失败时不返回版本标识。
func TestCommitPropagatesCommitterError(t *testing.T) {
	candidate := commitCandidateFixture()
	_, now := commitFixtureTimes()
	committer := &committerStub{err: model.ErrRunStoreFailed}

	result, err := Commit(context.Background(), committer, candidate, now)
	if !errors.Is(err, model.ErrRunStoreFailed) {
		t.Fatalf("应保留仓储层哨兵，实际 %v", err)
	}
	if result.ReleaseID != "" {
		t.Fatalf("失败时不应返回版本标识，实际 %q", result.ReleaseID)
	}
}

// TestCommitRejectsMissingCommitter 覆盖装配错误：没有仓储时直接拒绝。
func TestCommitRejectsMissingCommitter(t *testing.T) {
	_, now := commitFixtureTimes()
	if _, err := Commit(context.Background(), nil, commitCandidateFixture(), now); err == nil {
		t.Fatal("缺少发布仓储应报错")
	}
}
