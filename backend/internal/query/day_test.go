package query

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeDay 记录查询入参并返回预设结果，替代真实存储。
type fakeDay struct {
	page  DayPage // 预设的存储结果
	err   error   // 非空时返回该错误
	calls int     // 调用次数，用来断言短路分支
	got   DayFilter
}

// lookup 实现 DayLookup 的方法值。
func (f *fakeDay) lookup(_ context.Context, filter DayFilter) (DayPage, error) {
	f.calls++
	f.got = filter
	return f.page, f.err
}

// dayFixtureNodes 造 01..12 共 12 节的存储层节点，缺观测的小节 HasData=false。
func dayFixtureNodes(missing map[string]bool) []DayNode {
	nodes := make([]DayNode, 0, 12)
	for i := 1; i <= 12; i++ {
		node := DayNode{
			Node:      nodeCode(i),
			Block:     "0102",
			StateKey:  "free",
			Label:     "空闲",
			Available: true,
			HasData:   true,
		}
		if missing[node.Node] {
			node.StateKey = ""
			node.Label = ""
			node.Available = false
			node.HasData = false
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// nodeCode 把 1..12 转成两位小节编号。
func nodeCode(number int) string {
	return fmt.Sprintf("%02d", number)
}

// dayFixtureRoom 是固定归属的房间。
func dayFixtureRoom() *DayRoom {
	return &DayRoom{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"}
}

// TestDayStatusReturnsTwelveNodes 覆盖 12 节完整返回与房间、日期上下文透出。
func TestDayStatusReturnsTwelveNodes(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(map[string]bool{"03": true, "07": true}),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if got.Date != "2026-10-21" || got.Term != "2026-2027-1" || got.Week != 5 || got.Weekday != 3 {
		t.Fatalf("日期上下文不符: %+v", got)
	}
	if got.RoomID != "0899" || got.Name != "F126" || got.BuildingID != "Q10" || got.BuildingName != "综合教学楼" {
		t.Fatalf("房间字段不符: %+v", got)
	}
	if len(got.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(got.Nodes))
	}
	for i, node := range got.Nodes {
		if node.Node != nodeCode(i+1) {
			t.Fatalf("第 %d 节编号期望 %s，实际 %s", i+1, nodeCode(i+1), node.Node)
		}
	}
	if got.Hint != nil {
		t.Fatalf("有数据时不应带页顶提示，实际 %+v", got.Hint)
	}
}

// TestDayStatusMarksMissingNodesAsNoData 覆盖 Requirement 16.2：
// 缺观测的小节标为无数据，不臆造为空闲，也不让整条请求变成错误。
func TestDayStatusMarksMissingNodesAsNoData(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(map[string]bool{"05": true, "12": true}),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if len(got.Nodes) != 12 {
		t.Fatalf("缺节点也要返回完整 12 节，实际 %d", len(got.Nodes))
	}
	for _, node := range got.Nodes {
		missing := node.Node == "05" || node.Node == "12"
		if !missing {
			if !node.HasData || node.Label != "空闲" || !node.Available {
				t.Fatalf("第 %s 节有观测，字段不符: %+v", node.Node, node)
			}
			continue
		}
		if node.HasData {
			t.Fatalf("第 %s 节缺观测，has_data 应为 false: %+v", node.Node, node)
		}
		if node.Label != NodeLabelNoData || node.StateKey != "" || node.Available {
			t.Fatalf("第 %s 节应标为无数据，实际 %+v", node.Node, node)
		}
	}
	if got.Hint != nil {
		t.Fatalf("个别小节缺观测不算整页空态，不应带提示，实际 %+v", got.Hint)
	}
}

// dayAllMissing 造出 01..12 全部缺观测的节点集合。
func dayAllMissing() []DayNode {
	missing := make(map[string]bool, 12)
	for i := 1; i <= 12; i++ {
		missing[nodeCode(i)] = true
	}
	return dayFixtureNodes(missing)
}

// TestDayStatusAllNodesMissingCarriesDayNoDataHint 覆盖 PRD 3.6 的「该日无数据」整页空态：
// 房间可归属、该周有观测，但这间教室这一天 12 节全无观测，要给出明确的第四档提示，
// 而不是让用户对着一屏「无数据」猜是没排课还是查询失败。
func TestDayStatusAllNodesMissingCarriesDayNoDataHint(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayAllMissing(),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("空态不应返回错误，实际 err=%v", err)
	}
	if got.Hint == nil || got.Hint.Code != HintDayNoData {
		t.Fatalf("期望提示 %q，实际 %+v", HintDayNoData, got.Hint)
	}
	if got.Hint.Message != "该日无数据" {
		t.Fatalf("提示文案不符: %q", got.Hint.Message)
	}
	// 提示归提示，轴仍是已知的：12 节照常返回，逐节仍标无数据。
	if len(got.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(got.Nodes))
	}
	for _, node := range got.Nodes {
		if node.HasData || node.StateKey != "" || node.Label != NodeLabelNoData || node.Available {
			t.Fatalf("整日无观测时每节都应无数据，实际 %+v", node)
		}
	}
	// 房间信息不能因为整日无观测就丢：前端要显示「哪间教室」「哪天」。
	if got.Name != "F126" || got.BuildingID != "Q10" || got.BuildingName != "综合教学楼" {
		t.Fatalf("房间字段不符: %+v", got)
	}
}

// TestDayStatusHintPriority 断言 day 视图四档提示的优先级与边界：
// 前三档不能被 day_no_data 抢走，部分小节有观测时也不能给整页空态提示。
func TestDayStatusHintPriority(t *testing.T) {
	cases := []struct {
		name    string
		page    DayPage
		want    string // 空串表示不给提示
		wantMsg string
	}{
		{
			name: "无版本优先于其余空态",
			page: DayPage{HasRelease: false, WeekHasObservation: true, Room: dayFixtureRoom()},
			want: HintNoData, wantMsg: "没有已发布的数据",
		},
		{
			name: "该周无观测仍走 week_no_schedule",
			page: DayPage{HasRelease: true, WeekHasObservation: false, Room: dayFixtureRoom(), Nodes: dayAllMissing()},
			want: HintWeekNoSchedule, wantMsg: "该周疑似无排课",
		},
		{
			name: "房间不可归属仍是 no_match",
			page: DayPage{HasRelease: true, WeekHasObservation: true, Room: nil},
			want: HintNoMatch, wantMsg: "没有符合条件的教室",
		},
		{
			name: "全部小节无观测才给 day_no_data",
			page: DayPage{HasRelease: true, WeekHasObservation: true, Room: dayFixtureRoom(), Nodes: dayAllMissing()},
			want: HintDayNoData, wantMsg: "该日无数据",
		},
		{
			name: "部分小节有观测不给提示",
			page: DayPage{
				HasRelease:         true,
				WeekHasObservation: true,
				Room:               dayFixtureRoom(),
				Nodes:              dayFixtureNodes(map[string]bool{"02": true, "09": true}),
			},
			want: "",
		},
		{
			name: "轴缺失不算该日无数据",
			page: DayPage{HasRelease: true, WeekHasObservation: true, Room: dayFixtureRoom()},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeDay{page: c.page}
			svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

			got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
			if err != nil {
				t.Fatalf("空态不应返回错误，实际 err=%v", err)
			}
			if c.want == "" {
				if got.Hint != nil {
					t.Fatalf("期望不给提示，实际 %+v", got.Hint)
				}
				return
			}
			if got.Hint == nil || got.Hint.Code != c.want {
				t.Fatalf("期望提示 %q，实际 %+v", c.want, got.Hint)
			}
			if got.Hint.Message != c.wantMsg {
				t.Fatalf("提示文案不符: 实际 %q，期望 %q", got.Hint.Message, c.wantMsg)
			}
		})
	}
}

// TestDayStatusBuildingMismatchReturnsEmpty 覆盖 Requirement 16.3：
// 房间不属于所选楼时按空态处理，不返回别的楼的数据。
func TestDayStatusBuildingMismatchReturnsEmpty(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               nil,
		Nodes:              make([]DayNode, 0),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899", Building: "Q11"})
	if err != nil {
		t.Fatalf("空态不应返回错误，实际 err=%v", err)
	}
	if got.BuildingID != "" || got.BuildingName != "" || got.Name != "" {
		t.Fatalf("不属于所选楼时不应回填房间信息: %+v", got)
	}
	if got.RoomID != "0899" {
		t.Fatalf("应原样回显请求房号，实际 %q", got.RoomID)
	}
	if got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("空态节点应为空数组，实际 %#v", got.Nodes)
	}
	if got.Hint == nil || got.Hint.Code != HintNoMatch {
		t.Fatalf("期望提示 %q，实际 %+v", HintNoMatch, got.Hint)
	}
}

// TestDayStatusWeekNoObservationCarriesHint 覆盖 Requirement 18.4 的「该周无观测」分支。
func TestDayStatusWeekNoObservationCarriesHint(t *testing.T) {
	// 该周无观测意味着这一天 12 节都没有观测行，存储层会原样给出 12 个 HasData=false 的节点。
	missingAll := make(map[string]bool, 12)
	for i := 1; i <= 12; i++ {
		missingAll[nodeCode(i)] = true
	}
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: false,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(missingAll),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("空态不应返回错误，实际 err=%v", err)
	}
	if got.Hint == nil || got.Hint.Code != HintWeekNoSchedule {
		t.Fatalf("期望提示 %q，实际 %+v", HintWeekNoSchedule, got.Hint)
	}
	if got.Hint.Message != "该周疑似无排课" {
		t.Fatalf("提示文案不符: %q", got.Hint.Message)
	}
	// 该周无观测时轴仍是已知的：12 节照样返回，每节都标无数据。
	if len(got.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(got.Nodes))
	}
	for _, node := range got.Nodes {
		if node.HasData || node.Label != NodeLabelNoData || node.Available {
			t.Fatalf("该周无观测时每节都应无数据，实际 %+v", node)
		}
	}
}

// TestDayStatusNoReleaseCarriesNoDataHint 覆盖没有当前版本：不返回 40401，而是空态与提示。
func TestDayStatusNoReleaseCarriesNoDataHint(t *testing.T) {
	fake := &fakeDay{page: DayPage{HasRelease: false, Nodes: make([]DayNode, 0)}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("无版本不应返回错误，实际 err=%v", err)
	}
	if got.Hint == nil || got.Hint.Code != HintNoData {
		t.Fatalf("期望提示 %q，实际 %+v", HintNoData, got.Hint)
	}
	if got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("无版本时节点应为空数组，实际 %#v", got.Nodes)
	}
}

// TestDayStatusOutsideCalendarReturnsEmptyHint 覆盖日期不在学期周：不查存储，直接空态。
func TestDayStatusOutsideCalendarReturnsEmptyHint(t *testing.T) {
	fake := &fakeDay{page: DayPage{HasRelease: true}}
	svc := New(missingLookup(), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("学期外不应返回错误，实际 err=%v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("学期外不应查存储，实际调用 %d 次", fake.calls)
	}
	if got.Hint == nil || got.Hint.Code != HintNoData {
		t.Fatalf("期望提示 %q，实际 %+v", HintNoData, got.Hint)
	}
	if got.RoomID != "0899" || got.Date != "2026-10-21" || got.Week != 0 || got.Weekday != 0 {
		t.Fatalf("日期上下文不符: %+v", got)
	}
	if got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("学期外节点应为空数组，实际 %#v", got.Nodes)
	}
}

// TestDayStatusPassesFilterToStore 断言学期、周次、星期、房号与楼筛选原样下传。
func TestDayStatusPassesFilterToStore(t *testing.T) {
	fake := &fakeDay{page: DayPage{HasRelease: true}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	_, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{
		DateOffset: 2,
		RoomID:     "0899",
		Building:   "Q10",
	})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	want := DayFilter{Term: "2026-2027-1", Week: 5, Weekday: 5, RoomID: "0899", Building: "Q10"}
	if fake.got != want {
		t.Fatalf("下传入参不符:\n实际 %+v\n期望 %+v", fake.got, want)
	}
}

// TestDayStatusErrorPropagation 覆盖学期周查询、存储查询与未装配三种失败。
func TestDayStatusErrorPropagation(t *testing.T) {
	boom := errors.New("boom")

	if _, err := New(brokenLookup(), nil, (&fakeDay{}).lookup).DayStatus(
		t.Context(), availabilityNow(), DayParams{RoomID: "0899"}); err == nil {
		t.Fatal("学期周错误应上抛，实际 nil")
	}

	fake := &fakeDay{err: boom}
	if _, err := New(availabilityWeekLookup(nil), nil, fake.lookup).DayStatus(
		t.Context(), availabilityNow(), DayParams{RoomID: "0899"}); !errors.Is(err, boom) {
		t.Fatalf("存储错误应上抛，实际 %v", err)
	}

	if _, err := New(availabilityWeekLookup(nil), nil, nil).DayStatus(
		t.Context(), availabilityNow(), DayParams{RoomID: "0899"}); err == nil {
		t.Fatal("未装配时不应静默成功，实际 nil")
	}
}

// TestDayStatusDateOffset 断言日期偏移按上海时区落到目标日期。
func TestDayStatusDateOffset(t *testing.T) {
	var gotDate time.Time
	fake := &fakeDay{page: DayPage{HasRelease: true}}
	svc := New(availabilityWeekLookup(&gotDate), nil, fake.lookup)

	if _, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{DateOffset: 2, RoomID: "0899"}); err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	want := time.Date(2026, 10, 23, 0, 0, 0, 0, shanghai)
	if !gotDate.Equal(want) {
		t.Fatalf("目标日期期望 %s，实际 %s", want, gotDate)
	}
}

// TestDayStatusAlwaysReturnsEmptyNodesSlice 断言 nodes 在空态下也是空数组而不是 null，
// 避免前端在 JSON 里拿到 null 后额外兜底。
func TestDayStatusAlwaysReturnsEmptyNodesSlice(t *testing.T) {
	fake := &fakeDay{page: DayPage{HasRelease: true, WeekHasObservation: true, Room: dayFixtureRoom()}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup)

	got, err := svc.DayStatus(t.Context(), availabilityNow(), DayParams{RoomID: "0899"})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if got.Nodes == nil || len(got.Nodes) != 0 {
		t.Fatalf("无节点时也应为空数组，实际 %#v", got.Nodes)
	}
}
