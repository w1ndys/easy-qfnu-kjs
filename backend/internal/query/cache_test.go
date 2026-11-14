package query

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// cacheAvailabilityParams 是空教室缓存用例的基准查询：每个字段都可单独改动，
// 用来断言缓存键确实覆盖了全部影响结果的请求参数。
func cacheAvailabilityParams() AvailabilityParams {
	return AvailabilityParams{
		DateOffset: 2,
		Building:   "Q10",
		Keyword:    "F126",
		StartNode:  "03",
		EndNode:    "05",
		NodeCount:  3,
		Limit:      10,
		Offset:     20,
	}
}

// TestAvailabilityCacheReusesResultWithinTTL 覆盖 Requirement 32.2：
// 相同查询在 30 秒内只查一次存储，两次响应的内容完全一致。
func TestAvailabilityCacheReusesResultWithinTTL(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total:              1,
		Rooms:              []AvailabilityRoom{{RoomID: "0899", Name: "F126", BuildingID: "Q10"}},
		HasRelease:         true,
		WeekHasObservation: true,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	first, err := svc.Availability(t.Context(), now, params)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	// 第 29 秒仍在 30 秒窗口内，必须命中缓存。
	second, err := svc.Availability(t.Context(), now.Add(29*time.Second), params)
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("相同查询应只查一次存储，实际 %d 次", fake.calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("缓存结果与首次结果不一致:\n首次 %+v\n缓存 %+v", first, second)
	}
}

// TestAvailabilityCacheExpiresAtTTL 覆盖 TTL 到期：恰好 30 秒即视为未命中并重新查存储。
func TestAvailabilityCacheExpiresAtTTL(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 0, HasRelease: true, WeekHasObservation: true}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	if _, err := svc.Availability(t.Context(), now, params); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := svc.Availability(t.Context(), now.Add(DefaultCacheTTL), params); err != nil {
		t.Fatalf("过期后查询失败: %v", err)
	}

	if fake.calls != 2 {
		t.Fatalf("TTL 到期后应重新查存储，实际调用 %d 次", fake.calls)
	}
}

// TestAvailabilityCacheSeparatesQueryParams 逐个改动影响结果的参数，断言每次都算新查询。
// 覆盖 date_offset、building、keyword、start_node、end_node、limit、offset。
func TestAvailabilityCacheSeparatesQueryParams(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1, HasRelease: true, WeekHasObservation: true}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	base := cacheAvailabilityParams()

	if _, err := svc.Availability(t.Context(), now, base); err != nil {
		t.Fatalf("基准查询失败: %v", err)
	}

	cases := []struct {
		name   string
		change func(AvailabilityParams) AvailabilityParams
	}{
		{name: "date_offset", change: func(p AvailabilityParams) AvailabilityParams { p.DateOffset = 3; return p }},
		{name: "building", change: func(p AvailabilityParams) AvailabilityParams { p.Building = "Q11"; return p }},
		{name: "keyword", change: func(p AvailabilityParams) AvailabilityParams { p.Keyword = "F127"; return p }},
		{name: "start_node", change: func(p AvailabilityParams) AvailabilityParams { p.StartNode = "02"; p.NodeCount = 4; return p }},
		{name: "end_node", change: func(p AvailabilityParams) AvailabilityParams { p.EndNode = "06"; p.NodeCount = 4; return p }},
		{name: "limit", change: func(p AvailabilityParams) AvailabilityParams { p.Limit = 20; return p }},
		{name: "offset", change: func(p AvailabilityParams) AvailabilityParams { p.Offset = 21; return p }},
	}
	for i, c := range cases {
		if _, err := svc.Availability(t.Context(), now, c.change(base)); err != nil {
			t.Fatalf("%s 变体查询失败: %v", c.name, err)
		}
		want := 2 + i
		if fake.calls != want {
			t.Fatalf("%s 变体应各查一次存储，期望 %d 次，实际 %d 次", c.name, want, fake.calls)
		}
	}

	// 关键词按归一化形态做键：全角与首尾空白不改变语义，不该各占一条缓存。
	if _, err := svc.Availability(t.Context(), now, func() AvailabilityParams {
		p := base
		p.Keyword = "  Ｆ１２６  "
		return p
	}()); err != nil {
		t.Fatalf("归一化关键词查询失败: %v", err)
	}
	if fake.calls != 1+len(cases) {
		t.Fatalf("归一化关键词应复用基准缓存，实际调用 %d 次", fake.calls)
	}
}

// TestAvailabilityCacheKeepsEmptyStateAndHint 覆盖空态：空结果与页顶提示原样缓存，
// 第二次请求仍然是「命中」而不是重新查询，提示档位与文案一字不差。
func TestAvailabilityCacheKeepsEmptyStateAndHint(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 0, HasRelease: false}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	first, err := svc.Availability(t.Context(), now, params)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	second, err := svc.Availability(t.Context(), now.Add(time.Second), params)
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("空态也应缓存，实际查存储 %d 次", fake.calls)
	}
	if first.Hint == nil || first.Hint.Code != HintNoData {
		t.Fatalf("首次空态提示不符: %+v", first.Hint)
	}
	if second.Hint == nil || *second.Hint != *first.Hint {
		t.Fatalf("缓存后的提示不一致:\n首次 %+v\n缓存 %+v", first.Hint, second.Hint)
	}
	if second.Items == nil || len(second.Items) != 0 {
		t.Fatalf("空态应返回空数组而不是 nil，实际 %+v", second.Items)
	}
}

// TestAvailabilityCacheSkippedWhenDisabled 覆盖不传 WithCache 的默认路径：
// 两次相同查询仍然各查一次存储，9.3 起的实时行为不变。
func TestAvailabilityCacheSkippedWhenDisabled(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1, HasRelease: true, WeekHasObservation: true}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil)
	now := availabilityNow()

	for i := 0; i < 2; i++ {
		if _, err := svc.Availability(t.Context(), now, cacheAvailabilityParams()); err != nil {
			t.Fatalf("第 %d 次查询失败: %v", i+1, err)
		}
	}
	if fake.calls != 2 {
		t.Fatalf("未开启缓存时两次都该查存储，实际 %d 次", fake.calls)
	}
}

// TestAvailabilityCacheSkipsErrorResult 覆盖失败不入缓存：一次存储错误不能让后续请求继续拿旧错误。
func TestAvailabilityCacheSkipsErrorResult(t *testing.T) {
	boom := errors.New("boom")
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1, HasRelease: true, WeekHasObservation: true}, err: boom}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	if _, err := svc.Availability(t.Context(), now, params); !errors.Is(err, boom) {
		t.Fatalf("首次应原样上抛存储错误，实际 %v", err)
	}
	fake.err = nil
	if _, err := svc.Availability(t.Context(), now, params); err != nil {
		t.Fatalf("错误不应被缓存，第二次应重新查询并成功: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("失败结果不入缓存，期望查存储 2 次，实际 %d 次", fake.calls)
	}
}

// TestAvailabilityCacheDoesNotCrossMidnight 覆盖跨零点：date_offset 相同但业务日期已变，
// 不能把昨天的结果当今天的返回，即使两次请求相距不到 TTL。
func TestAvailabilityCacheDoesNotCrossMidnight(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{Total: 1, HasRelease: true, WeekHasObservation: true}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	// 上海时区 2026-10-21 23:59:50 与 2026-10-22 00:00:10 相差 20 秒，但日期不同。
	beforeMidnight := time.Date(2026, 10, 21, 23, 59, 50, 0, shanghai)

	if _, err := svc.Availability(t.Context(), beforeMidnight, cacheAvailabilityParams()); err != nil {
		t.Fatalf("零点前查询失败: %v", err)
	}
	if _, err := svc.Availability(t.Context(), beforeMidnight.Add(20*time.Second), cacheAvailabilityParams()); err != nil {
		t.Fatalf("零点后查询失败: %v", err)
	}

	if fake.calls != 2 {
		t.Fatalf("跨零点应算两次查询，实际 %d 次", fake.calls)
	}
}

// TestAvailabilityCacheReturnsIndependentCopy 覆盖缓存结果被调用方改动：
// 命中缓存返回的是副本，改切片元素或提示文案都不该污染缓存里的共享数据。
func TestAvailabilityCacheReturnsIndependentCopy(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total:              1,
		Rooms:              []AvailabilityRoom{{RoomID: "0899", Name: "F126", BuildingID: "Q10"}},
		HasRelease:         true,
		WeekHasObservation: true,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	first, err := svc.Availability(t.Context(), now, params)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if len(first.Items) == 0 || first.Hint != nil {
		t.Fatalf("夹具应有命中且无提示，实际 %+v", first)
	}
	first.Items[0].Name = "被调用方改掉的名字"

	second, err := svc.Availability(t.Context(), now.Add(time.Second), params)
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if second.Items[0].Name != "F126" {
		t.Fatalf("缓存被调用方改动污染，实际 %q", second.Items[0].Name)
	}
}

// TestDayCacheReusesResultWithinTTL 覆盖全天状态的缓存命中与 TTL 到期。
func TestDayCacheReusesResultWithinTTL(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(map[string]bool{"03": true}),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := DayParams{DateOffset: 2, RoomID: "0899", Building: "Q10"}

	first, err := svc.DayStatus(t.Context(), now, params)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	second, err := svc.DayStatus(t.Context(), now.Add(29*time.Second), params)
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("相同查询应只查一次存储，实际 %d 次", fake.calls)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("缓存结果与首次结果不一致:\n首次 %+v\n缓存 %+v", first, second)
	}

	// 恰好 30 秒即未命中，重新查存储。
	if _, err := svc.DayStatus(t.Context(), now.Add(DefaultCacheTTL), params); err != nil {
		t.Fatalf("过期后查询失败: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("TTL 到期后应重新查存储，实际调用 %d 次", fake.calls)
	}
}

// TestDayCacheSeparatesRoomAndBuilding 覆盖全天状态缓存键：
// 房号、楼筛选与日期偏移都影响结果，任何一个不同都不能复用同一份缓存。
func TestDayCacheSeparatesRoomAndBuilding(t *testing.T) {
	fake := &fakeDay{page: DayPage{HasRelease: true, WeekHasObservation: true, Room: dayFixtureRoom(), Nodes: dayFixtureNodes(nil)}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	base := DayParams{RoomID: "0899"}

	if _, err := svc.DayStatus(t.Context(), now, base); err != nil {
		t.Fatalf("基准查询失败: %v", err)
	}

	cases := []struct {
		name   string
		change func(DayParams) DayParams
	}{
		{name: "room_id", change: func(p DayParams) DayParams { p.RoomID = "0900"; return p }},
		{name: "building", change: func(p DayParams) DayParams { p.Building = "Q10"; return p }},
		{name: "date_offset", change: func(p DayParams) DayParams { p.DateOffset = 1; return p }},
	}
	for i, c := range cases {
		if _, err := svc.DayStatus(t.Context(), now, c.change(base)); err != nil {
			t.Fatalf("%s 变体查询失败: %v", c.name, err)
		}
		want := 2 + i
		if fake.calls != want {
			t.Fatalf("%s 变体应各查一次存储，期望 %d 次，实际 %d 次", c.name, want, fake.calls)
		}
	}
}

// TestDayCacheKeepsNoDataHint 覆盖 day 视图的「该日无数据」空态：
// 逐节状态与第四档提示一起缓存，命中缓存不改变提示档位。
func TestDayCacheKeepsNoDataHint(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(map[string]bool{"01": true, "02": true, "03": true, "04": true, "05": true, "06": true, "07": true, "08": true, "09": true, "10": true, "11": true, "12": true}),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := DayParams{RoomID: "0899"}

	first, err := svc.DayStatus(t.Context(), now, params)
	if err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	second, err := svc.DayStatus(t.Context(), now.Add(time.Second), params)
	if err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("空态也应缓存，实际查存储 %d 次", fake.calls)
	}
	if first.Hint == nil || first.Hint.Code != HintDayNoData {
		t.Fatalf("首次空态提示不符: %+v", first.Hint)
	}
	if second.Hint == nil || *second.Hint != *first.Hint {
		t.Fatalf("缓存后的提示不一致:\n首次 %+v\n缓存 %+v", first.Hint, second.Hint)
	}
	if len(second.Nodes) != 12 {
		t.Fatalf("缓存应保留 12 节状态，实际 %d 节", len(second.Nodes))
	}
}

// TestTTLCacheDoesNotGrowBeyondMax 覆盖缓存表上限：表满后新键宁可不缓存，也不无界增长。
func TestTTLCacheDoesNotGrowBeyondMax(t *testing.T) {
	cache := newTTLCache[int](DefaultCacheTTL, 2)
	now := availabilityNow()

	cache.put(cacheKey{view: "a"}, 1, now)
	cache.put(cacheKey{view: "b"}, 2, now)
	cache.put(cacheKey{view: "c"}, 3, now)

	if len(cache.entries) != 2 {
		t.Fatalf("缓存表不应超过上限 2，实际 %d 条", len(cache.entries))
	}
	if _, ok := cache.get(cacheKey{view: "c"}, now); ok {
		t.Fatal("超出上限的新键不应被缓存")
	}
}

// TestTTLCacheClearDropsUnexpiredEntries 覆盖 clear 的语义：未过期的条目也一起丢，
// 与只清过期项的 purgeExpiredLocked 区分开。回退切换当前版本后必须走这条路径，
// 否则剩余 TTL 内访客仍会读到旧版本的结果（design D8）。
func TestTTLCacheClearDropsUnexpiredEntries(t *testing.T) {
	cache := newTTLCache[int](DefaultCacheTTL, 0)
	now := availabilityNow()
	key := cacheKey{view: "a"}

	cache.put(key, 1, now)
	if _, ok := cache.get(key, now); !ok {
		t.Fatal("预热后应命中缓存")
	}

	cache.clear()

	if _, ok := cache.get(key, now); ok {
		t.Fatal("clear 后未过期的条目也必须失效")
	}
	if len(cache.entries) != 0 {
		t.Fatalf("clear 后表应为空，实际 %d 条", len(cache.entries))
	}
}

// TestAvailabilityCacheClearedByInvalidate 覆盖 Service.Invalidate 对空教室缓存的作用：
// 同一键先预热（第二次命中缓存、替身只被调用一次），Invalidate 后 TTL 远未到期，
// 同键必须重新查库（替身被第二次调用）。
func TestAvailabilityCacheClearedByInvalidate(t *testing.T) {
	fake := &fakeAvailability{page: AvailabilityPage{
		Total:              1,
		Rooms:              []AvailabilityRoom{{RoomID: "0899", Name: "F126", BuildingID: "Q10"}},
		HasRelease:         true,
		WeekHasObservation: true,
	}}
	svc := New(availabilityWeekLookup(nil), fake.lookup, nil, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := cacheAvailabilityParams()

	// 预热：同键查两次，第二次命中缓存。
	if _, err := svc.Availability(t.Context(), now, params); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := svc.Availability(t.Context(), now.Add(time.Second), params); err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("预热后相同查询应只查一次存储，实际 %d 次", fake.calls)
	}

	svc.Invalidate()

	// 只过了 2 秒，TTL 远未到期；缓存被清后同键必须重新查库。
	if _, err := svc.Availability(t.Context(), now.Add(2*time.Second), params); err != nil {
		t.Fatalf("失效后查询失败: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("Invalidate 后同键应重新查存储，期望 2 次，实际 %d 次", fake.calls)
	}
}

// TestDayCacheClearedByInvalidate 覆盖 Service.Invalidate 对全天状态缓存的作用，
// 断言方式与空教室一致：预热 → 失效 → 同键重新查库。
func TestDayCacheClearedByInvalidate(t *testing.T) {
	fake := &fakeDay{page: DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               dayFixtureRoom(),
		Nodes:              dayFixtureNodes(map[string]bool{"03": true}),
	}}
	svc := New(availabilityWeekLookup(nil), nil, fake.lookup, WithCache(DefaultCacheTTL))
	now := availabilityNow()
	params := DayParams{DateOffset: 2, RoomID: "0899", Building: "Q10"}

	if _, err := svc.DayStatus(t.Context(), now, params); err != nil {
		t.Fatalf("首次查询失败: %v", err)
	}
	if _, err := svc.DayStatus(t.Context(), now.Add(time.Second), params); err != nil {
		t.Fatalf("缓存命中不应失败: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("预热后相同查询应只查一次存储，实际 %d 次", fake.calls)
	}

	svc.Invalidate()

	if _, err := svc.DayStatus(t.Context(), now.Add(2*time.Second), params); err != nil {
		t.Fatalf("失效后查询失败: %v", err)
	}
	if fake.calls != 2 {
		t.Fatalf("Invalidate 后同键应重新查存储，期望 2 次，实际 %d 次", fake.calls)
	}
}

// TestServiceInvalidateWithoutCacheIsNoop 覆盖未传 WithCache 的进程：
// 两张缓存表都是 nil，Invalidate 直接返回，装配层的失效回调可以无条件调用。
func TestServiceInvalidateWithoutCacheIsNoop(t *testing.T) {
	svc := New(availabilityWeekLookup(nil), nil, nil)
	if svc.availabilityCache != nil || svc.dayCache != nil {
		t.Fatalf("未传 WithCache 时两张缓存表应为 nil，实际 %p / %p",
			svc.availabilityCache, svc.dayCache)
	}

	svc.Invalidate()
}
