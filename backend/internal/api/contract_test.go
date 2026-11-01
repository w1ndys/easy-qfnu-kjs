package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
)

// 本文件是公开 API 的契约测试（tasklist 9.11）：装配真实的 api.New 与 query.New，
// 只把存储层替换成函数替身，覆盖 HTTP → 参数校验 → query 判定 → JSON 的完整链路。
// 响应键名按 design 的公开 API 契约逐层断言，错误码按错误码表断言；
// 9.8 的大屏（/api/screen）尚未实现，暂不在本文件覆盖。

// contractStore 是契约测试的替身存储层：每个字段对应 query 包的一个注入点。
// 方法值转接既保留了函数类型的注入方式，又能记录调用次数与入参，
// 用来断言「非法参数根本没到存储层」与「请求里的节次区间原样下传」。
type contractStore struct {
	weekLookup         query.WeekLookup
	availabilityLookup query.AvailabilityLookup
	dayLookup          query.DayLookup
	metaLookup         query.MetaLookup
	freshnessLookup    query.FreshnessLookup

	weekCalls         int
	availabilityCalls int
	dayCalls          int
	metaCalls         int
	freshnessCalls    int

	gotAvailability query.AvailabilityFilter
	gotDay          query.DayFilter
	gotFreshness    query.FreshnessParams
}

// Week 实现 query.WeekLookup 的替身；未注入时按「不在任何学期周」返回。
func (s *contractStore) Week(ctx context.Context, date time.Time) (model.TermWeek, bool, error) {
	s.weekCalls++
	if s.weekLookup == nil {
		return model.TermWeek{}, false, nil
	}
	return s.weekLookup(ctx, date)
}

// Availability 实现 query.AvailabilityLookup 的替身，并记录查询层下传的筛选条件。
func (s *contractStore) Availability(ctx context.Context, filter query.AvailabilityFilter) (query.AvailabilityPage, error) {
	s.availabilityCalls++
	s.gotAvailability = filter
	if s.availabilityLookup == nil {
		return query.AvailabilityPage{}, nil
	}
	return s.availabilityLookup(ctx, filter)
}

// Day 实现 query.DayLookup 的替身，并记录查询层下传的筛选条件。
func (s *contractStore) Day(ctx context.Context, filter query.DayFilter) (query.DayPage, error) {
	s.dayCalls++
	s.gotDay = filter
	if s.dayLookup == nil {
		return query.DayPage{}, nil
	}
	return s.dayLookup(ctx, filter)
}

// Meta 实现 query.MetaLookup 的替身。
func (s *contractStore) Meta(ctx context.Context) (query.MetaFacts, error) {
	s.metaCalls++
	if s.metaLookup == nil {
		return query.MetaFacts{}, nil
	}
	return s.metaLookup(ctx)
}

// Weeks 实现 query.FreshnessLookup 的替身。
func (s *contractStore) Weeks(ctx context.Context, params query.FreshnessParams) (query.FreshnessFacts, error) {
	s.freshnessCalls++
	s.gotFreshness = params
	if s.freshnessLookup == nil {
		return query.FreshnessFacts{}, nil
	}
	return s.freshnessLookup(ctx, params)
}

// newContractStore 造一份全部成功的替身存储；单个用例可以只覆盖某个字段来制造空态或失败。
func newContractStore() *contractStore {
	return &contractStore{
		weekLookup: contractInCalendarWeek,
		availabilityLookup: func(context.Context, query.AvailabilityFilter) (query.AvailabilityPage, error) {
			return contractAvailabilityPage(), nil
		},
		dayLookup: func(context.Context, query.DayFilter) (query.DayPage, error) {
			return contractDayPage(), nil
		},
		metaLookup: func(context.Context) (query.MetaFacts, error) {
			return contractMetaFacts(), nil
		},
		freshnessLookup: func(context.Context, query.FreshnessParams) (query.FreshnessFacts, error) {
			return contractFreshnessFacts(), nil
		},
	}
}

// newContractServer 用真实的 query 服务装配 api.Server；时钟固定在 fixedQueryClock（周三）。
func newContractServer(store *contractStore) *Server {
	svc := query.New(store.Week, store.Availability, store.Day)
	metaSvc := query.NewMeta(store.Meta, query.NewFreshness(store.Weeks))
	srv := New(quietLogger(), okPinger{}, svc, svc, svc, WithMeta(metaSvc))
	srv.clock = fixedQueryClock
	return srv
}

// contractAt 返回固定日期 2026-10-21 的某个本地时刻，用于 CP10 的「不同请求时刻」。
func contractAt(hour, minute int) time.Time {
	return time.Date(2026, 10, 21, hour, minute, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
}

// contractGeneratedAt 是版本生成时间；放在 UTC 上以便断言 RFC3339 UTC 序列化。
func contractGeneratedAt() time.Time {
	return time.Date(2026, 10, 20, 7, 10, 12, 0, time.UTC)
}

// contractAvailabilityPage 是空教室存储层替身的成功页：一间 Q10 的 F126。
func contractAvailabilityPage() query.AvailabilityPage {
	return query.AvailabilityPage{
		Total: 1,
		Rooms: []query.AvailabilityRoom{
			{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		HasRelease:         true,
		WeekHasObservation: true,
	}
}

// contractDayPage 是全天状态存储层替身的成功页：12 节里第 02 节缺观测。
func contractDayPage() query.DayPage {
	return query.DayPage{
		HasRelease:         true,
		WeekHasObservation: true,
		Room:               &query.DayRoom{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
		Nodes:              dayStorageNodes("02"),
	}
}

// contractMetaFacts 造一份含当前版本、字典、轴与白名单楼的事实。
func contractMetaFacts() query.MetaFacts {
	return query.MetaFacts{
		Release: &query.MetaReleaseFacts{
			ReleaseID:   "20261020T071012Z",
			GeneratedAt: contractGeneratedAt(),
			Term:        "2026-2027-1",
			TotalWeeks:  5,
			DictVersion: 1,
			AxisVersion: 1,
		},
		DictStates: []query.MetaDictStateFacts{
			{StateKey: "free", Label: "空闲", Available: true},
			{StateKey: "class", Label: "正常上课", Available: false},
		},
		AxisNodes: contractAxisNodes(),
		Buildings: []query.MetaBuilding{
			{BuildingID: "Q10", Name: "综合教学楼"},
			{BuildingID: "Q11", Name: "实验楼"},
		},
	}
}

// contractAxisNodes 按迁移种子的「5 大节 → 12 小节」造轴节点。
func contractAxisNodes() []query.MetaAxisNode {
	blocks := []string{
		"0102", "0102",
		"030405", "030405", "030405",
		"0607", "0607",
		"0809", "0809",
		"101112", "101112", "101112",
	}
	blockOrdinals := []int{1, 1, 2, 2, 2, 3, 3, 4, 4, 5, 5, 5}

	nodes := make([]query.MetaAxisNode, 0, len(blocks))
	for i, block := range blocks {
		nodes = append(nodes, query.MetaAxisNode{
			Node:         fmt.Sprintf("%02d", i+1),
			Ordinal:      i + 1,
			Block:        block,
			BlockOrdinal: blockOrdinals[i],
		})
	}
	return nodes
}

// contractOldSuccessAt 是超过 8 天的成功抓取时间，用来断言逐周过期标记。
func contractOldSuccessAt() time.Time {
	return time.Date(2026, 9, 30, 7, 0, 0, 0, time.UTC)
}

// contractFreshnessFacts 造逐周事实：第 4 周已发布但过期，第 5 周新鲜，其余周未发布。
func contractFreshnessFacts() query.FreshnessFacts {
	old := contractOldSuccessAt()
	generated := contractGeneratedAt()
	return query.FreshnessFacts{
		Weeks: []query.WeekFacts{
			{Week: 4, Published: true, LastSuccessAt: &old},
			{Week: 5, Published: true, LastSuccessAt: &generated},
		},
		LastRunStatus: "success",
	}
}

// contractInCalendarWeek 是契约测试的固定教学周：2026-2027-1 第 5 周，周一 2026-10-19。
func contractInCalendarWeek(context.Context, time.Time) (model.TermWeek, bool, error) {
	return model.TermWeek{
		Term:       "2026-2027-1",
		Week:       5,
		Monday:     time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		InCalendar: true,
	}, true, nil
}

// contractOutOfTermWeek 模拟当天没有被任何学期周覆盖（寒暑假等）。
func contractOutOfTermWeek(context.Context, time.Time) (model.TermWeek, bool, error) {
	return model.TermWeek{}, false, nil
}

// contractNonTeachingWeek 模拟当天落在学期周里但该周被标为非教学周。
func contractNonTeachingWeek(context.Context, time.Time) (model.TermWeek, bool, error) {
	return model.TermWeek{
		Term:       "2026-2027-1",
		Week:       5,
		Monday:     time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		InCalendar: false,
	}, true, nil
}

// contractGet 打一次 GET；target 含路径与查询串。
func contractGet(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// contractEnvelope 是统一响应信封；Data 留原始 JSON 供各端点按自己的契约类型二次解析。
type contractEnvelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

// decodeContractBody 解析统一信封，并断言信封本身的契约（内容类型、键集合）。
func decodeContractBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if contentType := rec.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("响应 Content-Type 期望 application/json，实际 %q", contentType)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	assertContractKeys(t, "信封", body, "code", "message", "data", "request_id")
	return body
}

// contractSuccessData 断言 200 + code 0 + success + request_id 合规，并返回 data 对象。
// 业务码为 0 同时排除了 40401：空态与「无数据」都不能走错误码。
func contractSuccessData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 HTTP 200，实际 %d，body=%s", rec.Code, rec.Body.String())
	}
	body := decodeContractBody(t, rec)
	if code, ok := body["code"].(float64); !ok || code != 0 {
		t.Fatalf("期望业务码 0（不是 40401），实际 %v，body=%s", body["code"], rec.Body.String())
	}
	if body["message"] != "success" {
		t.Fatalf("期望 message=success，实际 %v", body["message"])
	}
	assertContractRequestID(t, body["request_id"])
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 不是对象: %#v", body["data"])
	}
	return data
}

// assertContractFailure 断言失败响应的公共形状：HTTP 状态、业务码、data 为 null、request_id 合规。
func assertContractFailure(t *testing.T, rec *httptest.ResponseRecorder, wantStatus, wantCode int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("期望 HTTP %d，实际 %d，body=%s", wantStatus, rec.Code, rec.Body.String())
	}
	body := decodeContractBody(t, rec)
	if code, ok := body["code"].(float64); !ok || int(code) != wantCode {
		t.Fatalf("期望业务码 %d，实际 %v", wantCode, body["code"])
	}
	if message, _ := body["message"].(string); message == "" {
		t.Fatalf("失败响应必须带可读 message，实际 %#v", body["message"])
	}
	if body["data"] != nil {
		t.Fatalf("失败响应 data 期望 null，实际 %#v", body["data"])
	}
	assertContractRequestID(t, body["request_id"])
	return body
}

// assertContractRequestID 断言 request_id 是 req_ + 32 位十六进制。
func assertContractRequestID(t *testing.T, value any) {
	t.Helper()
	id, ok := value.(string)
	if !ok {
		t.Fatalf("request_id 不是字符串: %#v", value)
	}
	if len(id) != len("req_")+32 || !strings.HasPrefix(id, "req_") {
		t.Fatalf("request_id 不符合 req_+32 位十六进制: %q", id)
	}
}

// assertContractKeys 断言对象键集合与契约完全一致：多一个或少一个都算契约漂移。
func assertContractKeys(t *testing.T, what string, got map[string]any, want ...string) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for key := range got {
		gotKeys = append(gotKeys, key)
	}
	sort.Strings(gotKeys)

	wantKeys := append([]string(nil), want...)
	sort.Strings(wantKeys)

	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("%s 键集合不符：实际 %v，期望 %v", what, gotKeys, wantKeys)
	}
}

// contractTypedData 按调用方给定的契约类型解析 data，便于断言字段取值。
func contractTypedData[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var env contractEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	var typed T
	if err := json.Unmarshal(env.Data, &typed); err != nil {
		t.Fatalf("data 不符合目标契约类型: %v, data=%s", err, env.Data)
	}
	return typed
}

// TestContractContextShape 覆盖 /api/context 的成功响应形状（Requirement 17.1）。
func TestContractContextShape(t *testing.T) {
	srv := newContractServer(newContractStore())
	rec := contractGet(t, srv, "/api/context")

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "context data", data, "date", "term", "week", "weekday", "in_calendar")
	if _, exists := data["hint"]; exists {
		t.Fatalf("context 契约不含 hint 键: %v", data)
	}

	got := contractTypedData[query.ContextInfo](t, rec)
	want := query.ContextInfo{Date: "2026-10-21", Term: "2026-2027-1", Week: 5, Weekday: 3, InCalendar: true}
	if got != want {
		t.Fatalf("context 取值不符：实际 %+v，期望 %+v", got, want)
	}
}

// TestContractMetaShape 覆盖 /api/meta 的成功响应形状（Requirement 17.2–17.5）：
// release、dict、axis、buildings、weeks、empty 六块逐层断言键名与取值。
func TestContractMetaShape(t *testing.T) {
	store := newContractStore()
	srv := newContractServer(store)
	rec := contractGet(t, srv, "/api/meta")

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "meta data", data, "release", "dict", "axis", "buildings", "weeks", "empty")

	release, ok := data["release"].(map[string]any)
	if !ok {
		t.Fatalf("release 不是对象: %#v", data["release"])
	}
	assertContractKeys(t, "meta release", release, "release_id", "generated_at")
	// 时间必须是 RFC3339 UTC。
	if release["generated_at"] != "2026-10-20T07:10:12Z" {
		t.Fatalf("generated_at 期望 RFC3339 UTC，实际 %v", release["generated_at"])
	}

	dict, ok := data["dict"].(map[string]any)
	if !ok {
		t.Fatalf("dict 不是对象: %#v", data["dict"])
	}
	assertContractKeys(t, "meta dict", dict, "version", "states")
	states, ok := dict["states"].(map[string]any)
	if !ok {
		t.Fatalf("dict.states 不是对象: %#v", dict["states"])
	}
	assertContractKeys(t, "meta dict states", states, "free", "class")
	free, ok := states["free"].(map[string]any)
	if !ok {
		t.Fatalf("dict.states.free 不是对象: %#v", states["free"])
	}
	assertContractKeys(t, "meta dict state", free, "label", "available")

	axis, ok := data["axis"].(map[string]any)
	if !ok {
		t.Fatalf("axis 不是对象: %#v", data["axis"])
	}
	assertContractKeys(t, "meta axis", axis, "version", "blocks", "nodes")
	axisNodes, ok := axis["nodes"].([]any)
	if !ok || len(axisNodes) != 12 {
		t.Fatalf("axis.nodes 期望 12 个节点，实际 %#v", axis["nodes"])
	}
	firstNode, ok := axisNodes[0].(map[string]any)
	if !ok {
		t.Fatalf("axis.nodes[0] 不是对象: %#v", axisNodes[0])
	}
	assertContractKeys(t, "meta axis node", firstNode, "node", "ordinal", "block", "block_ordinal")

	buildings, ok := data["buildings"].([]any)
	if !ok || len(buildings) != 2 {
		t.Fatalf("buildings 期望 2 栋，实际 %#v", data["buildings"])
	}
	building, ok := buildings[0].(map[string]any)
	if !ok {
		t.Fatalf("buildings[0] 不是对象: %#v", buildings[0])
	}
	assertContractKeys(t, "meta building", building, "building_id", "name")

	weeks, ok := data["weeks"].([]any)
	if !ok || len(weeks) != 5 {
		t.Fatalf("weeks 期望覆盖 5 个教学周，实际 %#v", data["weeks"])
	}
	week, ok := weeks[0].(map[string]any)
	if !ok {
		t.Fatalf("weeks[0] 不是对象: %#v", weeks[0])
	}
	assertContractKeys(t, "meta week", week, "week", "published", "last_success_at", "stale")
	if data["empty"] != false {
		t.Fatalf("有当前版本时 empty 期望 false，实际 %v", data["empty"])
	}

	got := contractTypedData[query.MetaResult](t, rec)
	if got.Empty {
		t.Fatal("有当前版本时 empty 应为 false")
	}
	if got.Release == nil || got.Release.ReleaseID != "20261020T071012Z" {
		t.Fatalf("release 取值不符: %+v", got.Release)
	}
	if !got.Release.GeneratedAt.Equal(contractGeneratedAt()) || got.Release.GeneratedAt.Location() != time.UTC {
		t.Fatalf("generated_at 应统一为 UTC，实际 %v", got.Release.GeneratedAt)
	}
	if got.Dict == nil || got.Dict.Version != 1 {
		t.Fatalf("dict 取值不符: %+v", got.Dict)
	}
	if state := got.Dict.States["free"]; state.Label != "空闲" || !state.Available {
		t.Fatalf("dict.states.free 不符: %+v", state)
	}
	if state := got.Dict.States["class"]; state.Label != "正常上课" || state.Available {
		t.Fatalf("dict.states.class 不符: %+v", state)
	}
	if got.Axis == nil || got.Axis.Version != 1 {
		t.Fatalf("axis 取值不符: %+v", got.Axis)
	}
	wantBlocks := []string{"0102", "030405", "0607", "0809", "101112"}
	if !reflect.DeepEqual(got.Axis.Blocks, wantBlocks) {
		t.Fatalf("axis.blocks 期望 %v，实际 %v", wantBlocks, got.Axis.Blocks)
	}
	if len(got.Axis.Nodes) != 12 || got.Axis.Nodes[0].Node != "01" || got.Axis.Nodes[11].Node != "12" {
		t.Fatalf("axis.nodes 不符: %+v", got.Axis.Nodes)
	}
	if len(got.Buildings) != 2 || got.Buildings[0].BuildingID != "Q10" || got.Buildings[1].Name != "实验楼" {
		t.Fatalf("buildings 不符: %+v", got.Buildings)
	}

	if len(got.Weeks) != 5 {
		t.Fatalf("weeks 期望 5 项，实际 %d", len(got.Weeks))
	}
	// 未发布周：不算过期，last_success_at 为 null。
	if first := got.Weeks[0]; first.Week != 1 || first.Published || first.Stale || first.LastSuccessAt != nil {
		t.Fatalf("第 1 周新鲜度不符: %+v", first)
	}
	// 已发布且距最近成功超过 8 天：过期（Requirement 17.3）。
	if fourth := got.Weeks[3]; fourth.Week != 4 || !fourth.Published || !fourth.Stale || fourth.LastSuccessAt == nil {
		t.Fatalf("第 4 周新鲜度不符: %+v", fourth)
	}
	// 已发布且一天前抓取：不过期。
	if fifth := got.Weeks[4]; fifth.Week != 5 || !fifth.Published || fifth.Stale || fifth.LastSuccessAt == nil {
		t.Fatalf("第 5 周新鲜度不符: %+v", fifth)
	}
	if weeks[4].(map[string]any)["last_success_at"] != "2026-10-20T07:10:12Z" {
		t.Fatalf("last_success_at 期望 RFC3339 UTC，实际 %v", weeks[4].(map[string]any)["last_success_at"])
	}
	if weeks[0].(map[string]any)["last_success_at"] != nil {
		t.Fatalf("未发布周 last_success_at 期望 null，实际 %v", weeks[0].(map[string]any)["last_success_at"])
	}

	// 逐周新鲜度按当前版本的整个学期区间查询，不引入采集窗口猜测。
	wantFreshness := query.FreshnessParams{Term: "2026-2027-1", FromWeek: 1, ToWeek: 5}
	if store.gotFreshness != wantFreshness {
		t.Fatalf("逐周新鲜度查询区间不符：\n实际 %+v\n期望 %+v", store.gotFreshness, wantFreshness)
	}
}

// TestContractAvailabilityShape 覆盖 /api/query?view=availability 的成功响应形状
// 与查询层下传的筛选条件（Requirement 15.1–15.5、18.1）。
func TestContractAvailabilityShape(t *testing.T) {
	store := newContractStore()
	srv := newContractServer(store)

	values := url.Values{
		"view":        {"availability"},
		"date_offset": {"2"},
		"start_node":  {"03"},
		"end_node":    {"05"},
		"building":    {" Q10 "},
		"keyword":     {"Ｆ１２６"},
		"limit":       {"10"},
		"offset":      {"0"},
	}
	rec := serveQuery(t, srv, values)

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "availability data", data, "total", "items", "date", "term", "week", "weekday")
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items 期望 1 项数组，实际 %#v", data["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] 不是对象: %#v", items[0])
	}
	assertContractKeys(t, "availability item", item, "room_id", "name", "building_id", "building_name")

	got := contractTypedData[query.AvailabilityResult](t, rec)
	want := query.AvailabilityResult{
		Total: 1,
		Items: []query.AvailabilityRoom{
			{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
		},
		Date: "2026-10-23", Term: "2026-2027-1", Week: 5, Weekday: 5,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("availability 取值不符：\n实际 %+v\n期望 %+v", got, want)
	}

	// 节次区间、楼、关键词（全角转半角）与分页都原样进入查询层。
	wantFilter := query.AvailabilityFilter{
		Term: "2026-2027-1", Week: 5, Weekday: 5,
		StartNode: "03", EndNode: "05", NodeCount: 3,
		Building: "Q10", Keyword: "F126", Limit: 10, Offset: 0,
	}
	if store.gotAvailability != wantFilter {
		t.Fatalf("availability 筛选条件不符：\n实际 %+v\n期望 %+v", store.gotAvailability, wantFilter)
	}
}

// TestContractDayShape 覆盖 /api/query?view=day 的成功响应形状（Requirement 16.1–16.3）：
// 12 小节逐节下发，缺观测的小节标为无数据而不是空。
func TestContractDayShape(t *testing.T) {
	store := newContractStore()
	srv := newContractServer(store)

	values := url.Values{
		"view":        {"day"},
		"room_id":     {" 0899 "},
		"date_offset": {"2"},
		"building":    {" Q10 "},
	}
	rec := serveQuery(t, srv, values)

	data := contractSuccessData(t, rec)
	assertContractKeys(t, "day data", data,
		"room_id", "name", "building_id", "building_name",
		"date", "term", "week", "weekday", "nodes")
	nodes, ok := data["nodes"].([]any)
	if !ok || len(nodes) != 12 {
		t.Fatalf("nodes 期望 12 项数组，实际 %#v", data["nodes"])
	}
	node, ok := nodes[0].(map[string]any)
	if !ok {
		t.Fatalf("nodes[0] 不是对象: %#v", nodes[0])
	}
	assertContractKeys(t, "day node", node, "node", "block", "state_key", "label", "available", "has_data")

	got := contractTypedData[query.DayResult](t, rec)
	if got.RoomID != "0899" || got.Name != "F126" || got.BuildingID != "Q10" || got.BuildingName != "综合教学楼" {
		t.Fatalf("房间字段不符: %+v", got)
	}
	if got.Date != "2026-10-23" || got.Term != "2026-2027-1" || got.Week != 5 || got.Weekday != 5 {
		t.Fatalf("日期上下文不符: %+v", got)
	}
	if got.Hint != nil {
		t.Fatalf("有数据时不应带 hint: %+v", got.Hint)
	}
	if len(got.Nodes) != 12 {
		t.Fatalf("节点数期望 12，实际 %d", len(got.Nodes))
	}
	if first := got.Nodes[0]; first.Node != "01" || first.Block != "0102" || !first.HasData || first.Label != "空闲" {
		t.Fatalf("第 1 节不符: %+v", first)
	}
	if second := got.Nodes[1]; second.Node != "02" || second.HasData || second.Available ||
		second.StateKey != "" || second.Label != query.NodeLabelNoData {
		t.Fatalf("第 2 节应无数据: %+v", second)
	}

	wantFilter := query.DayFilter{Term: "2026-2027-1", Week: 5, Weekday: 5, RoomID: "0899", Building: "Q10"}
	if store.gotDay != wantFilter {
		t.Fatalf("day 筛选条件不符：\n实际 %+v\n期望 %+v", store.gotDay, wantFilter)
	}
}

// TestContractParamErrorCode 覆盖参数错误：一律 HTTP 400 + 业务码 40001，
// 且非法请求不会进入查询层（Requirement 18.1–18.2）。
func TestContractParamErrorCode(t *testing.T) {
	tooLongKeyword := strings.Repeat("字", 33)

	cases := []struct {
		name   string
		values url.Values
	}{
		// 日期偏移：0..10（Requirement 18.1）。
		{name: "日期偏移为负", values: url.Values{"date_offset": {"-1"}}},
		{name: "日期偏移越过上界", values: url.Values{"date_offset": {"11"}}},
		{name: "日期偏移非整数", values: url.Values{"date_offset": {"abc"}}},
		// 节次：01..12 且起点不晚于终点（Requirement 18.2）。
		{name: "起始节次 00", values: url.Values{"start_node": {"00"}}},
		{name: "起始节次 13", values: url.Values{"start_node": {"13"}}},
		{name: "起始节次非两位", values: url.Values{"start_node": {"1"}}},
		{name: "结束节次 00", values: url.Values{"end_node": {"00"}}},
		{name: "结束节次 13", values: url.Values{"end_node": {"13"}}},
		{name: "结束节次非数字", values: url.Values{"end_node": {"ab"}}},
		{name: "起点晚于终点", values: url.Values{"start_node": {"07"}, "end_node": {"06"}}},
		// 分页与关键词。
		{name: "limit 为 0", values: url.Values{"limit": {"0"}}},
		{name: "limit 越过上界", values: url.Values{"limit": {"201"}}},
		{name: "limit 非整数", values: url.Values{"limit": {"abc"}}},
		{name: "offset 为负", values: url.Values{"offset": {"-1"}}},
		{name: "offset 非整数", values: url.Values{"offset": {"abc"}}},
		{name: "关键词 33 字符", values: url.Values{"keyword": {tooLongKeyword}}},
		// view 枚举。
		{name: "未知视图", values: url.Values{"view": {"unknown"}}},
		{name: "视图大小写不符", values: url.Values{"view": {"AVAILABILITY"}}},
		// day 的 room_id 必填。
		{name: "day 缺 room_id", values: url.Values{"view": {"day"}}},
		{name: "day room_id 全空白", values: url.Values{"view": {"day"}, "room_id": {"   "}}},
		{name: "day room_id 超长", values: url.Values{"view": {"day"}, "room_id": {strings.Repeat("9", 33)}}},
		{name: "day 日期偏移为负", values: url.Values{"view": {"day"}, "room_id": {"0899"}, "date_offset": {"-1"}}},
		{name: "day 日期偏移越界", values: url.Values{"view": {"day"}, "room_id": {"0899"}, "date_offset": {"11"}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newContractStore()
			srv := newContractServer(store)
			rec := serveQuery(t, srv, c.values)

			assertContractFailure(t, rec, http.StatusBadRequest, 40001)
			if store.weekCalls+store.availabilityCalls+store.dayCalls+store.metaCalls != 0 {
				t.Fatalf("非法参数不应进入查询层，实际调用 week=%d availability=%d day=%d meta=%d",
					store.weekCalls, store.availabilityCalls, store.dayCalls, store.metaCalls)
			}
		})
	}
}

// TestContractInternalErrorCode 覆盖内部错误：一律 HTTP 500 + 业务码 50000，
// 响应里不出现内部错误原文。
func TestContractInternalErrorCode(t *testing.T) {
	dbDown := errors.New("db down")

	cases := []struct {
		name   string
		target string
		mutate func(*contractStore)
	}{
		{
			name:   "context 读取失败",
			target: "/api/context",
			mutate: func(s *contractStore) {
				s.weekLookup = func(context.Context, time.Time) (model.TermWeek, bool, error) {
					return model.TermWeek{}, false, dbDown
				}
			},
		},
		{
			name:   "availability 读取失败",
			target: "/api/query?view=availability",
			mutate: func(s *contractStore) {
				s.availabilityLookup = func(context.Context, query.AvailabilityFilter) (query.AvailabilityPage, error) {
					return query.AvailabilityPage{}, dbDown
				}
			},
		},
		{
			name:   "day 读取失败",
			target: "/api/query?view=day&room_id=0899",
			mutate: func(s *contractStore) {
				s.dayLookup = func(context.Context, query.DayFilter) (query.DayPage, error) {
					return query.DayPage{}, dbDown
				}
			},
		},
		{
			name:   "meta 读取失败",
			target: "/api/meta",
			mutate: func(s *contractStore) {
				s.metaLookup = func(context.Context) (query.MetaFacts, error) {
					return query.MetaFacts{}, dbDown
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newContractStore()
			c.mutate(store)
			srv := newContractServer(store)
			rec := contractGet(t, srv, c.target)

			body := assertContractFailure(t, rec, http.StatusInternalServerError, 50000)
			if body["message"] != "内部错误" {
				t.Fatalf("内部错误文案期望「内部错误」，实际 %v", body["message"])
			}
			if strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("响应泄露内部错误: %s", rec.Body.String())
			}
		})
	}
}

// TestContractRateLimitCode 覆盖限流：同来源第 61 次返回 HTTP 429 + 业务码 42901（Requirement 32.1）。
func TestContractRateLimitCode(t *testing.T) {
	store := newContractStore()
	srv := newContractServer(store)
	clock := newMovableClock(fixedQueryClock())
	srv.clock = clock.now

	values := url.Values{"view": {"availability"}}
	for i := 0; i < rateLimitMaxPerWindow; i++ {
		rec := serveQuery(t, srv, values)
		contractSuccessData(t, rec)
	}
	if store.availabilityCalls != rateLimitMaxPerWindow {
		t.Fatalf("窗口内应查询 %d 次，实际 %d 次", rateLimitMaxPerWindow, store.availabilityCalls)
	}

	rec := serveQuery(t, srv, values)
	body := assertContractFailure(t, rec, http.StatusTooManyRequests, 42901)
	if message, _ := body["message"].(string); !strings.Contains(message, "稍后重试") {
		t.Fatalf("限流文案应提示稍后重试，实际 %q", message)
	}
	if store.availabilityCalls != rateLimitMaxPerWindow {
		t.Fatalf("被限流的请求不应再查存储，实际存储调用 %d 次", store.availabilityCalls)
	}
}

// TestContractParamBoundaries 覆盖每个越界参数的通过侧边界：
// date_offset 0/10、节次 01/12 且 start≤end、limit 1/200、offset 0、keyword 32 字符、
// view 的省略与两种取值都必须放行并把参数交给查询层。
func TestContractParamBoundaries(t *testing.T) {
	t.Run("空教室", func(t *testing.T) {
		cases := []struct {
			name     string
			values   url.Values
			wantDate string
			check    func(*testing.T, query.AvailabilityFilter)
		}{
			{name: "日期偏移下界 0", values: url.Values{"date_offset": {"0"}}, wantDate: "2026-10-21"},
			{name: "日期偏移上界 10", values: url.Values{"date_offset": {"10"}}, wantDate: "2026-10-31"},
			{
				name: "节次下界 01..01", values: url.Values{"start_node": {"01"}, "end_node": {"01"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.StartNode != "01" || f.EndNode != "01" || f.NodeCount != 1 {
						t.Fatalf("节次区间不符: %+v", f)
					}
				},
			},
			{
				name: "节次上界 01..12", values: url.Values{"start_node": {"01"}, "end_node": {"12"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.StartNode != "01" || f.EndNode != "12" || f.NodeCount != 12 {
						t.Fatalf("节次区间不符: %+v", f)
					}
				},
			},
			{
				name: "节次起止相等 06..06", values: url.Values{"start_node": {"06"}, "end_node": {"06"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.StartNode != "06" || f.EndNode != "06" || f.NodeCount != 1 {
						t.Fatalf("节次区间不符: %+v", f)
					}
				},
			},
			{
				name: "limit 下界 1", values: url.Values{"limit": {"1"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Limit != 1 {
						t.Fatalf("limit 期望 1，实际 %d", f.Limit)
					}
				},
			},
			{
				name: "limit 上界 200", values: url.Values{"limit": {"200"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Limit != 200 {
						t.Fatalf("limit 期望 200，实际 %d", f.Limit)
					}
				},
			},
			{
				name: "offset 下界 0", values: url.Values{"offset": {"0"}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Offset != 0 {
						t.Fatalf("offset 期望 0，实际 %d", f.Offset)
					}
				},
			},
			{
				name: "keyword 32 个汉字", values: url.Values{"keyword": {strings.Repeat("字", 32)}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Keyword != strings.Repeat("字", 32) {
						t.Fatalf("keyword 期望 32 个汉字原样下传，实际 %q", f.Keyword)
					}
				},
			},
			{
				name: "keyword 32 个 ASCII", values: url.Values{"keyword": {strings.Repeat("a", 32)}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Keyword != strings.Repeat("a", 32) {
						t.Fatalf("keyword 期望 32 个字符原样下传，实际 %q", f.Keyword)
					}
				},
			},
			{
				name: "省略节次用默认区间", values: url.Values{},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.StartNode != "01" || f.EndNode != "11" || f.NodeCount != 11 {
						t.Fatalf("默认节次区间不符: %+v", f)
					}
					if f.Limit != 50 || f.Offset != 0 {
						t.Fatalf("默认分页不符: %+v", f)
					}
				},
			},
			{
				name: "节次带空白", values: url.Values{"start_node": {" 03 "}, "end_node": {" 05 "}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.StartNode != "03" || f.EndNode != "05" || f.NodeCount != 3 {
						t.Fatalf("节次去空白不符: %+v", f)
					}
				},
			},
			{
				name: "教学楼去空白", values: url.Values{"building": {" Q10 "}},
				check: func(t *testing.T, f query.AvailabilityFilter) {
					if f.Building != "Q10" {
						t.Fatalf("building 期望去空白后 Q10，实际 %q", f.Building)
					}
				},
			},
			{name: "view 显式 availability", values: url.Values{"view": {"availability"}}},
			{name: "view 带空白", values: url.Values{"view": {" availability "}}},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				store := newContractStore()
				srv := newContractServer(store)
				rec := serveQuery(t, srv, c.values)

				contractSuccessData(t, rec)
				if store.availabilityCalls != 1 {
					t.Fatalf("边界值应进入查询层一次，实际 %d 次", store.availabilityCalls)
				}
				if c.check != nil {
					c.check(t, store.gotAvailability)
				}

				wantDate := c.wantDate
				if wantDate == "" {
					wantDate = "2026-10-21"
				}
				got := contractTypedData[query.AvailabilityResult](t, rec)
				if got.Date != wantDate {
					t.Fatalf("业务日期期望 %s，实际 %s", wantDate, got.Date)
				}
			})
		}
	})

	t.Run("全天状态", func(t *testing.T) {
		cases := []struct {
			name     string
			values   url.Values
			wantDate string
			check    func(*testing.T, query.DayFilter)
		}{
			{
				name:     "日期偏移下界 0",
				values:   url.Values{"view": {"day"}, "room_id": {"0899"}, "date_offset": {"0"}},
				wantDate: "2026-10-21",
				check: func(t *testing.T, f query.DayFilter) {
					if f.RoomID != "0899" {
						t.Fatalf("room_id 期望 0899，实际 %q", f.RoomID)
					}
				},
			},
			{
				name:     "日期偏移上界 10",
				values:   url.Values{"view": {"day"}, "room_id": {"0899"}, "date_offset": {"10"}},
				wantDate: "2026-10-31",
				check: func(t *testing.T, f query.DayFilter) {
					if f.RoomID != "0899" {
						t.Fatalf("room_id 期望 0899，实际 %q", f.RoomID)
					}
				},
			},
			{
				name:     "房号 32 字符",
				values:   url.Values{"view": {"day"}, "room_id": {strings.Repeat("9", 32)}},
				wantDate: "2026-10-21",
				check: func(t *testing.T, f query.DayFilter) {
					if f.RoomID != strings.Repeat("9", 32) {
						t.Fatalf("room_id 期望 32 字符原样下传，实际 %q", f.RoomID)
					}
				},
			},
			{
				name:     "房号与教学楼去空白",
				values:   url.Values{"view": {"day"}, "room_id": {" 0899 "}, "building": {" Q10 "}},
				wantDate: "2026-10-21",
				check: func(t *testing.T, f query.DayFilter) {
					if f.RoomID != "0899" || f.Building != "Q10" {
						t.Fatalf("房号或教学楼去空白不符: %+v", f)
					}
				},
			},
		}

		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				store := newContractStore()
				srv := newContractServer(store)
				rec := serveQuery(t, srv, c.values)

				contractSuccessData(t, rec)
				if store.dayCalls != 1 {
					t.Fatalf("边界值应进入查询层一次，实际 %d 次", store.dayCalls)
				}
				if c.check != nil {
					c.check(t, store.gotDay)
				}
				got := contractTypedData[query.DayResult](t, rec)
				if got.Date != c.wantDate {
					t.Fatalf("业务日期期望 %s，实际 %s", c.wantDate, got.Date)
				}
			})
		}
	})
}

// TestContractCP10NodeRangeNotClock 验证 CP10 与 Requirement 19.5：
// 同一请求在不同时刻调用，结果只由请求里的节次区间决定；
// 服务端不因为「当前是第几节」改变结果，左右结果的只有 start_node/end_node。
func TestContractCP10NodeRangeNotClock(t *testing.T) {
	store := newContractStore()
	// 存储层只认查询层下传的节次区间：06..09 有教室，其余区间没有。
	store.availabilityLookup = func(_ context.Context, filter query.AvailabilityFilter) (query.AvailabilityPage, error) {
		if filter.StartNode == "06" && filter.EndNode == "09" && filter.NodeCount == 4 {
			return contractAvailabilityPage(), nil
		}
		return query.AvailabilityPage{HasRelease: true, WeekHasObservation: true}, nil
	}
	srv := newContractServer(store)

	values := url.Values{"start_node": {"06"}, "end_node": {"09"}}
	moments := []time.Time{
		contractAt(0, 1),
		contractAt(8, 0),
		contractAt(12, 30),
		contractAt(19, 0),
		contractAt(23, 59),
	}

	bodies := make([]map[string]any, 0, len(moments))
	filters := make([]query.AvailabilityFilter, 0, len(moments))
	for _, moment := range moments {
		srv.clock = func() time.Time { return moment }
		rec := serveQuery(t, srv, values)
		data := contractSuccessData(t, rec)

		got := contractTypedData[query.AvailabilityResult](t, rec)
		if got.Total != 1 {
			t.Fatalf("%s 请求 06..09 期望 1 间教室，实际 %d", moment.Format(time.TimeOnly), got.Total)
		}
		if got.Date != "2026-10-21" {
			t.Fatalf("%s 请求的业务日期期望 2026-10-21，实际 %q", moment.Format(time.TimeOnly), got.Date)
		}

		bodies = append(bodies, data)
		filters = append(filters, store.gotAvailability)
	}

	// 不同时刻的同一请求必须返回同形的 data（request_id 在 data 之外）。
	for i := 1; i < len(bodies); i++ {
		if !reflect.DeepEqual(bodies[0], bodies[i]) {
			t.Fatalf("第 %d 个时刻的结果与首个时刻不同：\n实际 %+v\n期望 %+v", i+1, bodies[i], bodies[0])
		}
		if filters[i] != filters[0] {
			t.Fatalf("第 %d 个时刻下传的筛选条件不同：%+v vs %+v", i+1, filters[i], filters[0])
		}
	}
	wantFilter := query.AvailabilityFilter{
		Term: "2026-2027-1", Week: 5, Weekday: 3,
		StartNode: "06", EndNode: "09", NodeCount: 4,
		Limit: 50,
	}
	if filters[0] != wantFilter {
		t.Fatalf("下传的节次区间不符：\n实际 %+v\n期望 %+v", filters[0], wantFilter)
	}

	// 同一时刻换一个节次区间，结果随之改变：左右结果的只有请求里的区间。
	srv.clock = func() time.Time { return contractAt(12, 30) }
	otherRange := url.Values{"start_node": {"01"}, "end_node": {"05"}}
	rec := serveQuery(t, srv, otherRange)
	data := contractSuccessData(t, rec)
	got := contractTypedData[query.AvailabilityResult](t, rec)
	if got.Total != 0 || len(got.Items) != 0 {
		t.Fatalf("01..05 期望 0 间教室，实际 %+v", got)
	}
	hint, ok := data["hint"].(map[string]any)
	if !ok {
		t.Fatalf("空结果应带 hint，实际 %#v", data["hint"])
	}
	assertContractKeys(t, "availability hint", hint, "code", "message")
	if hint["code"] != query.HintNoMatch {
		t.Fatalf("空结果提示期望 %q，实际 %v", query.HintNoMatch, hint["code"])
	}
	wantOtherFilter := query.AvailabilityFilter{
		Term: "2026-2027-1", Week: 5, Weekday: 3,
		StartNode: "01", EndNode: "05", NodeCount: 5,
		Limit: 50,
	}
	if store.gotAvailability != wantOtherFilter {
		t.Fatalf("换区间后的筛选条件不符：\n实际 %+v\n期望 %+v", store.gotAvailability, wantOtherFilter)
	}
}

// TestContractWeekNoScheduleHint 覆盖 Requirement 18.3：
// 该周全部状态格为空时，两个 view 都返回 200 + 结果 + week_no_schedule 提示，而不是错误。
func TestContractWeekNoScheduleHint(t *testing.T) {
	store := newContractStore()
	store.availabilityLookup = func(context.Context, query.AvailabilityFilter) (query.AvailabilityPage, error) {
		return query.AvailabilityPage{HasRelease: true, WeekHasObservation: false}, nil
	}
	store.dayLookup = func(context.Context, query.DayFilter) (query.DayPage, error) {
		return query.DayPage{
			HasRelease:         true,
			WeekHasObservation: false,
			Room:               &query.DayRoom{RoomID: "0899", Name: "F126", BuildingID: "Q10", BuildingName: "综合教学楼"},
			Nodes:              dayStorageNodesAllMissing(),
		}, nil
	}
	srv := newContractServer(store)

	t.Run("空教室", func(t *testing.T) {
		values := url.Values{"start_node": {"06"}, "end_node": {"09"}}
		rec := serveQuery(t, srv, values)
		// 200 + code 0：空周不是 40401。
		data := contractSuccessData(t, rec)
		assertContractKeys(t, "availability data", data, "total", "items", "date", "term", "week", "weekday", "hint")

		got := contractTypedData[query.AvailabilityResult](t, rec)
		if got.Total != 0 {
			t.Fatalf("全空周 total 期望 0，实际 %d", got.Total)
		}
		if items, ok := data["items"].([]any); !ok || len(items) != 0 {
			t.Fatalf("全空周 items 期望空数组，实际 %#v", data["items"])
		}
		// 结果照常返回：日期上下文仍在，不是错误。
		if got.Date != "2026-10-21" || got.Term != "2026-2027-1" || got.Week != 5 || got.Weekday != 3 {
			t.Fatalf("全空周仍应返回日期上下文: %+v", got)
		}
		if got.Hint == nil || got.Hint.Code != query.HintWeekNoSchedule {
			t.Fatalf("提示期望 %q，实际 %+v", query.HintWeekNoSchedule, got.Hint)
		}
		if got.Hint.Message != "该周疑似无排课" {
			t.Fatalf("提示文案不符: %q", got.Hint.Message)
		}
	})

	t.Run("全天状态", func(t *testing.T) {
		values := url.Values{"view": {"day"}, "room_id": {"0899"}}
		rec := serveQuery(t, srv, values)
		data := contractSuccessData(t, rec)
		assertContractKeys(t, "day data", data,
			"room_id", "name", "building_id", "building_name",
			"date", "term", "week", "weekday", "nodes", "hint")

		got := contractTypedData[query.DayResult](t, rec)
		if got.RoomID != "0899" || got.Name != "F126" {
			t.Fatalf("全空周仍应返回房间信息: %+v", got)
		}
		if nodes, ok := data["nodes"].([]any); !ok || len(nodes) != 12 {
			t.Fatalf("全空周 nodes 期望 12 项数组，实际 %#v", data["nodes"])
		}
		if got.Hint == nil || got.Hint.Code != query.HintWeekNoSchedule {
			t.Fatalf("提示期望 %q，实际 %+v", query.HintWeekNoSchedule, got.Hint)
		}
		if got.Hint.Message != "该周疑似无排课" {
			t.Fatalf("提示文案不符: %q", got.Hint.Message)
		}
	})
}

// TestContractEmptyStateIsNot404 覆盖 Requirement 18.4 与 design 的空态约定：
// 无当前版本、日期不属于任何学期周都不返回 40401，而是 200 + 空数据 + hint。
func TestContractEmptyStateIsNot404(t *testing.T) {
	contextKeys := []string{"date", "term", "week", "weekday", "in_calendar"}
	availabilityKeys := []string{"total", "items", "date", "term", "week", "weekday", "hint"}
	dayKeys := []string{
		"room_id", "name", "building_id", "building_name",
		"date", "term", "week", "weekday", "nodes", "hint",
	}

	t.Run("meta 无当前版本", func(t *testing.T) {
		store := newContractStore()
		store.metaLookup = func(context.Context) (query.MetaFacts, error) {
			return query.MetaFacts{Buildings: contractMetaFacts().Buildings}, nil
		}
		srv := newContractServer(store)
		rec := contractGet(t, srv, "/api/meta")

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "meta data", data, "release", "dict", "axis", "buildings", "weeks", "empty")
		if data["release"] != nil {
			t.Fatalf("空态 release 期望 null，实际 %#v", data["release"])
		}
		if data["dict"] != nil {
			t.Fatalf("空态 dict 期望 null，实际 %#v", data["dict"])
		}
		if data["axis"] != nil {
			t.Fatalf("空态 axis 期望 null，实际 %#v", data["axis"])
		}
		if data["empty"] != true {
			t.Fatalf("空态 empty 期望 true，实际 %#v", data["empty"])
		}
		if weeks, ok := data["weeks"].([]any); !ok || len(weeks) != 0 {
			t.Fatalf("空态 weeks 期望空数组，实际 %#v", data["weeks"])
		}
		if store.freshnessCalls != 0 {
			t.Fatalf("无当前版本不应查新鲜度，实际 %d 次", store.freshnessCalls)
		}
	})

	t.Run("context 日期不属于任何学期周", func(t *testing.T) {
		store := newContractStore()
		store.weekLookup = contractOutOfTermWeek
		srv := newContractServer(store)
		rec := contractGet(t, srv, "/api/context")

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "context data", data, contextKeys...)
		got := contractTypedData[query.ContextInfo](t, rec)
		want := query.ContextInfo{Date: "2026-10-21"}
		if got != want {
			t.Fatalf("非学期周上下文不符：实际 %+v，期望 %+v", got, want)
		}
	})

	t.Run("context 学期周被标为非教学周", func(t *testing.T) {
		store := newContractStore()
		store.weekLookup = contractNonTeachingWeek
		srv := newContractServer(store)
		rec := contractGet(t, srv, "/api/context")

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "context data", data, contextKeys...)
		got := contractTypedData[query.ContextInfo](t, rec)
		// 非教学周与「不在学期周」同口径：term/week/weekday 置零，in_calendar 为 false。
		want := query.ContextInfo{Date: "2026-10-21"}
		if got != want {
			t.Fatalf("非教学周上下文不符：实际 %+v，期望 %+v", got, want)
		}
	})

	t.Run("availability 无当前版本", func(t *testing.T) {
		store := newContractStore()
		store.availabilityLookup = func(context.Context, query.AvailabilityFilter) (query.AvailabilityPage, error) {
			return query.AvailabilityPage{HasRelease: false}, nil
		}
		srv := newContractServer(store)
		rec := serveQuery(t, srv, url.Values{"start_node": {"06"}, "end_node": {"09"}})

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "availability data", data, availabilityKeys...)
		got := contractTypedData[query.AvailabilityResult](t, rec)
		if got.Total != 0 {
			t.Fatalf("空态 total 期望 0，实际 %d", got.Total)
		}
		if items, ok := data["items"].([]any); !ok || len(items) != 0 {
			t.Fatalf("空态 items 期望空数组，实际 %#v", data["items"])
		}
		if got.Hint == nil || got.Hint.Code != query.HintNoData || got.Hint.Message != "没有已发布的数据" {
			t.Fatalf("空态提示不符: %+v", got.Hint)
		}
	})

	t.Run("availability 日期不属于任何学期周", func(t *testing.T) {
		store := newContractStore()
		store.weekLookup = contractOutOfTermWeek
		srv := newContractServer(store)
		rec := serveQuery(t, srv, url.Values{"start_node": {"06"}, "end_node": {"09"}})

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "availability data", data, availabilityKeys...)
		got := contractTypedData[query.AvailabilityResult](t, rec)
		if got.Total != 0 || len(got.Items) != 0 {
			t.Fatalf("非学期周应为空结果: %+v", got)
		}
		if got.Date != "2026-10-21" || got.Term != "" || got.Week != 0 || got.Weekday != 0 {
			t.Fatalf("非学期周的日期上下文不符: %+v", got)
		}
		if got.Hint == nil || got.Hint.Code != query.HintNoData || got.Hint.Message != "该日期没有可查询的数据" {
			t.Fatalf("非学期周提示不符: %+v", got.Hint)
		}
	})

	t.Run("availability 学期周被标为非教学周", func(t *testing.T) {
		store := newContractStore()
		store.weekLookup = contractNonTeachingWeek
		srv := newContractServer(store)
		rec := serveQuery(t, srv, url.Values{"start_node": {"06"}, "end_node": {"09"}})

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "availability data", data, availabilityKeys...)
		got := contractTypedData[query.AvailabilityResult](t, rec)
		// 非教学周按空态处理：term/week/weekday 保持零值，与「不在学期周」同口径。
		if got.Term != "" || got.Week != 0 || got.Weekday != 0 {
			t.Fatalf("非教学周的行上下文期望为零值: %+v", got)
		}
		if got.Hint == nil || got.Hint.Code != query.HintNoData || got.Hint.Message != "该日期没有可查询的数据" {
			t.Fatalf("非教学周提示不符: %+v", got.Hint)
		}
	})

	t.Run("day 无当前版本", func(t *testing.T) {
		store := newContractStore()
		store.dayLookup = func(context.Context, query.DayFilter) (query.DayPage, error) {
			return query.DayPage{HasRelease: false}, nil
		}
		srv := newContractServer(store)
		rec := serveQuery(t, srv, url.Values{"view": {"day"}, "room_id": {"0899"}})

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "day data", data, dayKeys...)
		got := contractTypedData[query.DayResult](t, rec)
		if got.RoomID != "0899" {
			t.Fatalf("空态应回显请求房号，实际 %q", got.RoomID)
		}
		if nodes, ok := data["nodes"].([]any); !ok || len(nodes) != 0 {
			t.Fatalf("空态 nodes 期望空数组，实际 %#v", data["nodes"])
		}
		if got.Hint == nil || got.Hint.Code != query.HintNoData || got.Hint.Message != "没有已发布的数据" {
			t.Fatalf("空态提示不符: %+v", got.Hint)
		}
	})

	t.Run("day 日期不属于任何学期周", func(t *testing.T) {
		store := newContractStore()
		store.weekLookup = contractOutOfTermWeek
		srv := newContractServer(store)
		rec := serveQuery(t, srv, url.Values{"view": {"day"}, "room_id": {"0899"}})

		data := contractSuccessData(t, rec)
		assertContractKeys(t, "day data", data, dayKeys...)
		got := contractTypedData[query.DayResult](t, rec)
		if got.RoomID != "0899" {
			t.Fatalf("空态应回显请求房号，实际 %q", got.RoomID)
		}
		if nodes, ok := data["nodes"].([]any); !ok || len(nodes) != 0 {
			t.Fatalf("空态 nodes 期望空数组，实际 %#v", data["nodes"])
		}
		if got.Hint == nil || got.Hint.Code != query.HintNoData || got.Hint.Message != "该日期没有可查询的数据" {
			t.Fatalf("非学期周提示不符: %+v", got.Hint)
		}
	})
}
