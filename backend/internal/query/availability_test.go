package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// fakeAvailability 记录查询入参并返回预设结果，替代真实存储。
type fakeAvailability struct {
	page  AvailabilityPage // 预设的存储结果
	err   error            // 非空时返回该错误
	calls int              // 调用次数，用来断言短路分支
	got   AvailabilityFilter
}

// lookup 实现 AvailabilityLookup 的方法值。
func (f *fakeAvailability) lookup(_ context.Context, filter AvailabilityFilter) (AvailabilityPage, error) {
	f.calls++
	f.got = filter
	return f.page, f.err
}

// availabilityWeekLookup 返回固定在第五周的学期周，供空教室用例复用。
func availabilityWeekLookup(gotDate *time.Time) WeekLookup {
	return func(_ context.Context, date time.Time) (model.TermWeek, bool, error) {
		if gotDate != nil {
			*gotDate = date
		}
		return model.TermWeek{
			Term:       "2026-2027-1",
			Week:       5,
			Monday:     time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
			InCalendar: true,
		}, true, nil
	}
}

// availabilityNow 固定在上海时区 2026-10-21 10:00，对应周三。
func availabilityNow() time.Time {
	return time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
}

// TestAvailabilityPassesRangeAndPagingToStore 断言区间、楼、关键词与分页原样下传。
func TestAvailabilityPassesRangeAndPagingToStore(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	_, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{
		DateOffset: 2,
		Building:   "Q10",
		Keyword:    "F126",
		StartNode:  "03",
		EndNode:    "05",
		NodeCount:  3,
		Limit:      10,
		Offset:     20,
	})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	want := AvailabilityFilter{
		Term:      "2026-2027-1",
		Week:      5,
		Weekday:   5,
		StartNode: "03",
		EndNode:   "05",
		NodeCount: 3,
		Building:  "Q10",
		Keyword:   "F126",
		Limit:     10,
		Offset:    20,
	}
	if fake.got != want {
		t.Fatalf("下传入参不符:\n实际 %+v\n期望 %+v", fake.got, want)
	}
}

// TestAvailabilityDateOffsetLandsOnTargetDate 断言日期偏移按上海时区加到业务日期。
func TestAvailabilityDateOffsetLandsOnTargetDate(t *testing.T) {
	var gotDate time.Time
	fake := &fakeAvailability{page: AvailabilityPage{Total: 0}}
	svc := New(availabilityWeekLookup(&gotDate), fake.lookup, nil)

	if _, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{DateOffset: 2}); err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	want := time.Date(2026, 10, 23, 0, 0, 0, 0, shanghai)
	if !gotDate.Equal(want) {
		t.Fatalf("目标日期期望 %s，实际 %s", want, gotDate)
	}
}

// TestAvailabilityMapsRoomAndTotal 断言房间字段、总数与日期上下文完整透出。
func TestAvailabilityMapsRoomAndTotal(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total: 7,
		Rooms: []AvailabilityRoom{
			{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
			{RoomID: "0900", Name: "F127", BuildingID: "Q11", BuildingName: "实验楼"},
		},
		HasRelease:         true,
		WeekHasObservation: true,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if got.Total != 7 {
		t.Fatalf("总数期望 7，实际 %d", got.Total)
	}
	if got.Date != "2026-10-21" || got.Term != "2026-2027-1" || got.Week != 5 || got.Weekday != 3 {
		t.Fatalf("日期上下文不符: %+v", got)
	}
	if len(got.Items) != 2 {
		t.Fatalf("房间数期望 2，实际 %d", len(got.Items))
	}
	if got.Items[0].RoomID != "0899" || got.Items[0].BuildingName != "综合教学楼" {
		t.Fatalf("房间字段不符: %+v", got.Items[0])
	}
	if got.Hint != nil {
		t.Fatalf("有命中时不应带提示，实际 %+v", got.Hint)
	}
}

// TestAvailabilityEmptyResultCarriesWeekNoScheduleHint 覆盖情形 B：
// 该周没有任何观测时仍返回成功，并带「该周疑似无排课」页顶提示。
func TestAvailabilityEmptyResultCarriesWeekNoScheduleHint(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total:              0,
		HasRelease:         true,
		WeekHasObservation: false,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3})
	if err != nil {
		t.Fatalf("空结果不应返回错误，实际 err=%v", err)
	}
	if got.Total != 0 || len(got.Items) != 0 {
		t.Fatalf("空结果不符: %+v", got)
	}
	if got.Items == nil {
		t.Fatal("items 必须是空数组而不是 nil")
	}
	if got.Hint == nil || got.Hint.Code != HintWeekNoSchedule {
		t.Fatalf("期望提示 %q，实际 %+v", HintWeekNoSchedule, got.Hint)
	}
	if got.Hint.Message != "该周疑似无排课" {
		t.Fatalf("提示文案不符: %q", got.Hint.Message)
	}
}

// TestAvailabilityEmptyResultCarriesNoMatchHint 覆盖情形 B 的筛选无命中分支：
// 该周有观测但没有房间满足区间/楼/关键词，同样返回成功与提示。
func TestAvailabilityEmptyResultCarriesNoMatchHint(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total:              0,
		HasRelease:         true,
		WeekHasObservation: true,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3})
	if err != nil {
		t.Fatalf("空结果不应返回错误，实际 err=%v", err)
	}
	if got.Hint == nil || got.Hint.Code != HintNoMatch {
		t.Fatalf("期望提示 %q，实际 %+v", HintNoMatch, got.Hint)
	}
}

// TestAvailabilityNoReleaseReturnsEmptyDataHint 覆盖没有任何已发布版本：
// 不返回 40401，而是空列表 + 无数据提示，交给 9.5 再做统一空态。
func TestAvailabilityNoReleaseReturnsEmptyDataHint(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 0, HasRelease: false}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3})
	if err != nil {
		t.Fatalf("无版本不应返回错误，实际 err=%v", err)
	}
	if got.Hint == nil || got.Hint.Code != HintNoData {
		t.Fatalf("期望提示 %q，实际 %+v", HintNoData, got.Hint)
	}
}

// TestAvailabilityOutsideCalendarReturnsEmptyHint 覆盖日期不在学期周：不查存储，直接空态。
func TestAvailabilityOutsideCalendarReturnsEmptyHint(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1}}
	svc := New(missingLookup(), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3})
	if err != nil {
		t.Fatalf("学期外不应返回错误，实际 err=%v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("学期外不应查存储，实际调用 %d 次", fake.calls)
	}
	if got.Total != 0 || got.Hint == nil || got.Hint.Code != HintNoData {
		t.Fatalf("学期外空态不符: %+v", got)
	}
	if got.Date != "2026-10-21" || got.Week != 0 || got.Weekday != 0 {
		t.Fatalf("日期上下文不符: %+v", got)
	}
}

// TestAvailabilityKeepsTotalWhenPageEmpty 覆盖分页越界：总数仍按分页前返回，且不误判为空态。
func TestAvailabilityKeepsTotalWhenPageEmpty(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 3, Rooms: nil, HasRelease: true, WeekHasObservation: true}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)

	got, err := svc.Availability(t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3, Limit: 50, Offset: 99})
	if err != nil {
		t.Fatalf("期望成功，实际 err=%v", err)
	}
	if got.Total != 3 {
		t.Fatalf("总数期望 3，实际 %d", got.Total)
	}
	if len(got.Items) != 0 {
		t.Fatalf("越界页应为空，实际 %d 条", len(got.Items))
	}
	if got.Hint != nil {
		t.Fatalf("有总数时不应带空态提示，实际 %+v", got.Hint)
	}
}

// TestAvailabilityErrorPropagation 覆盖学期周查询与存储查询失败都原样上抛。
func TestAvailabilityErrorPropagation(t *testing.T) {
	boom := errors.New("boom")

	if _, err := New(brokenLookup(), (&fakeAvailability{}).lookup, nil).Availability(
		t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3}); err == nil {
		t.Fatal("学期周错误应上抛，实际 nil")
	}

	fake := &fakeAvailability{err: boom}
	if _, err := New(availabilityWeekLookup(nil), fake.lookup, nil).Availability(
		t.Context(), availabilityNow(), AvailabilityParams{StartNode: "01", EndNode: "03", NodeCount: 3}); !errors.Is(err, boom) {
		t.Fatalf("存储错误应上抛，实际 %v", err)
	}
}

// TestNormalizeKeyword 覆盖全半角归一与首尾空白。
func TestNormalizeKeyword(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "全角字母数字", raw: "  Ｆ１２６  ", want: "F126"},
		{name: "全角括号与加号", raw: "Ｆ１２６（东）＋１", want: "F126(东)+1"},
		{name: "表意空格归一后去空白", raw: "综合楼　　101", want: "综合楼  101"},
		{name: "半角原样保留", raw: "F126", want: "F126"},
		{name: "空字符串", raw: "   ", want: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NormalizeKeyword(c.raw); got != c.want {
				t.Fatalf("NormalizeKeyword(%q) = %q，期望 %q", c.raw, got, c.want)
			}
		})
	}
}

// TestParseNodeRange 覆盖默认区间、合法区间与非法区间。
func TestParseNodeRange(t *testing.T) {
	cases := []struct {
		name      string
		start     string
		end       string
		wantStart string
		wantEnd   string
		wantCount int
		wantErr   bool
	}{
		{name: "省略取默认 01 到 11", wantStart: "01", wantEnd: "11", wantCount: 11},
		{name: "合法单节", start: "05", end: "05", wantStart: "05", wantEnd: "05", wantCount: 1},
		{name: "合法跨大节", start: "02", end: "06", wantStart: "02", wantEnd: "06", wantCount: 5},
		{name: "起点 00 非法", start: "00", end: "03", wantErr: true},
		{name: "终点 13 非法", start: "01", end: "13", wantErr: true},
		{name: "非数字非法", start: "0A", end: "03", wantErr: true},
		{name: "起点晚于终点", start: "06", end: "05", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, count, err := ParseNodeRange(c.start, c.end)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望参数错误，实际返回 %q..%q 共 %d 节", start, end, count)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际 err=%v", err)
			}
			if start != c.wantStart || end != c.wantEnd || count != c.wantCount {
				t.Fatalf("得到 %q..%q 共 %d 节，期望 %q..%q 共 %d 节",
					start, end, count, c.wantStart, c.wantEnd, c.wantCount)
			}
		})
	}
}
