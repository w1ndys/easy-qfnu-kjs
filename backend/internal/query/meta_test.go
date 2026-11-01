package query

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeMetaLookup 用预设事实造一个存储层替身，并记录被调用次数。
type fakeMetaLookup struct {
	facts MetaFacts
	err   error
	calls int
}

// lookup 实现 MetaLookup。
func (f *fakeMetaLookup) lookup(context.Context) (MetaFacts, error) {
	f.calls++
	return f.facts, f.err
}

// recordingFreshness 记录新鲜度查询的入参与次数，用来确认 meta 复用了 9.6 的服务。
type recordingFreshness struct {
	facts FreshnessFacts
	err   error
	got   FreshnessParams
	calls int
}

// lookup 实现 FreshnessLookup。
func (r *recordingFreshness) lookup(_ context.Context, params FreshnessParams) (FreshnessFacts, error) {
	r.calls++
	r.got = params
	return r.facts, r.err
}

// newMetaTestService 组装 meta 服务与新鲜度记录器。
func newMetaTestService(facts MetaFacts, fresh FreshnessFacts) (*MetaService, *recordingFreshness) {
	recorder := &recordingFreshness{facts: fresh}
	return NewMeta((&fakeMetaLookup{facts: facts}).lookup, NewFreshness(recorder.lookup)), recorder
}

// fullMetaFacts 造一份含当前版本、字典、乱序轴与白名单楼的事实。
func fullMetaFacts() MetaFacts {
	return MetaFacts{
		Release: &MetaReleaseFacts{
			ReleaseID:   "20261023T071012Z",
			GeneratedAt: time.Date(2026, 10, 23, 15, 10, 12, 0, shanghai),
			Term:        freshnessFixtureTerm,
			TotalWeeks:  3,
			DictVersion: 1,
			AxisVersion: 1,
		},
		DictStates: []MetaDictStateFacts{
			{StateKey: "free", Label: "空闲", Available: true},
			{StateKey: "class", Label: "正常上课", Available: false},
		},
		// 故意乱序：服务层要按 ordinal 整形，不依赖存储层替身是否排好。
		AxisNodes: []MetaAxisNode{
			{Node: "03", Ordinal: 3, Block: "030405", BlockOrdinal: 2},
			{Node: "01", Ordinal: 1, Block: "0102", BlockOrdinal: 1},
			{Node: "02", Ordinal: 2, Block: "0102", BlockOrdinal: 1},
		},
		Buildings: []MetaBuilding{{BuildingID: "Q10", Name: "综合教学楼"}},
	}
}

// TestMetaShape 覆盖 meta 的正常形状：版本、字典、轴、白名单楼与逐周状态。
func TestMetaShape(t *testing.T) {
	svc, recorder := newMetaTestService(fullMetaFacts(), FreshnessFacts{
		Weeks:         []WeekFacts{{Week: 2, Published: true, LastSuccessAt: freshAt(-time.Hour)}},
		LastRunStatus: "success",
	})

	result, err := svc.Meta(t.Context(), freshnessNow)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if result.Empty {
		t.Fatal("有当前版本时 empty 应为 false")
	}

	wantRelease := time.Date(2026, 10, 23, 7, 10, 12, 0, time.UTC)
	if result.Release == nil || result.Release.ReleaseID != "20261023T071012Z" {
		t.Fatalf("版本标识不符: %+v", result.Release)
	}
	if !result.Release.GeneratedAt.Equal(wantRelease) || result.Release.GeneratedAt.Location() != time.UTC {
		t.Fatalf("生成时间应统一为 UTC，实际 %v", result.Release.GeneratedAt)
	}

	if result.Dict == nil || result.Dict.Version != 1 {
		t.Fatalf("字典版本不符: %+v", result.Dict)
	}
	if got := result.Dict.States["free"]; got.Label != "空闲" || !got.Available {
		t.Fatalf("free 状态不符: %+v", got)
	}
	if got := result.Dict.States["class"]; got.Label != "正常上课" || got.Available {
		t.Fatalf("class 状态不符: %+v", got)
	}

	if result.Axis == nil || result.Axis.Version != 1 {
		t.Fatalf("轴版本不符: %+v", result.Axis)
	}
	// 大节按首次出现的顺序去重，节点按 ordinal 升序。
	if want := []string{"0102", "030405"}; !reflect.DeepEqual(result.Axis.Blocks, want) {
		t.Fatalf("大节顺序期望 %v，实际 %v", want, result.Axis.Blocks)
	}
	if len(result.Axis.Nodes) != 3 {
		t.Fatalf("节点数期望 3，实际 %d", len(result.Axis.Nodes))
	}
	for i, want := range []string{"01", "02", "03"} {
		if result.Axis.Nodes[i].Node != want {
			t.Fatalf("第 %d 个节点期望 %s，实际 %s", i, want, result.Axis.Nodes[i].Node)
		}
	}
	if node := result.Axis.Nodes[2]; node.Block != "030405" || node.BlockOrdinal != 2 {
		t.Fatalf("节点大节归属不符: %+v", node)
	}

	if len(result.Buildings) != 1 || result.Buildings[0].BuildingID != "Q10" ||
		result.Buildings[0].Name != "综合教学楼" {
		t.Fatalf("白名单楼不符: %+v", result.Buildings)
	}

	if len(result.Weeks) != 3 {
		t.Fatalf("逐周状态应覆盖 1..3 周，实际 %+v", result.Weeks)
	}
	if !result.Weeks[1].Published || result.Weeks[1].Stale {
		t.Fatalf("第 2 周应为已发布且新鲜: %+v", result.Weeks[1])
	}
	if result.Weeks[0].Published || result.Weeks[2].Published {
		t.Fatalf("未发布周应保持 published=false: %+v", result.Weeks)
	}

	// 区间取当前版本所属学期的全部周，且复用 9.6 的新鲜度服务。
	wantParams := FreshnessParams{Term: freshnessFixtureTerm, FromWeek: 1, ToWeek: 3}
	if recorder.got != wantParams {
		t.Fatalf("新鲜度入参期望 %+v，实际 %+v", wantParams, recorder.got)
	}
}

// TestMetaWeeksMatchFreshness 覆盖逐周新鲜度与 9.6 完全一致：
// 同一批事实、同一时刻，meta 的 weeks 与直接调用 FreshnessService 的结果逐字段相等。
func TestMetaWeeksMatchFreshness(t *testing.T) {
	fresh := FreshnessFacts{
		Weeks: []WeekFacts{
			{Week: 1, Published: true, LastSuccessAt: freshAt(-time.Hour)},
			{Week: 3, Published: true, LastSuccessAt: freshAt(-9 * 24 * time.Hour)},
		},
		LastRunStatus: "success",
	}
	svc, recorder := newMetaTestService(fullMetaFacts(), fresh)

	result, err := svc.Meta(t.Context(), freshnessNow)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	want, err := NewFreshness((&recordingFreshness{facts: fresh}).lookup).
		Weeks(t.Context(), freshnessNow, recorder.got)
	if err != nil {
		t.Fatalf("直接查询新鲜度失败: %v", err)
	}
	if !reflect.DeepEqual(result.Weeks, want) {
		t.Fatalf("逐周新鲜度应与 9.6 一致:\nmeta %+v\n直接 %+v", result.Weeks, want)
	}
	// 第 3 周超过 8 天应判过期，第 1 周新鲜。
	if !result.Weeks[2].Stale || result.Weeks[0].Stale {
		t.Fatalf("过期判定不符: %+v", result.Weeks)
	}
}

// TestMetaEmptyState 覆盖无当前版本：200 空态，不查新鲜度，白名单仍然返回。
func TestMetaEmptyState(t *testing.T) {
	facts := MetaFacts{
		Buildings: []MetaBuilding{{BuildingID: "Q10", Name: "综合教学楼"}},
	}
	svc, recorder := newMetaTestService(facts, FreshnessFacts{})

	result, err := svc.Meta(t.Context(), freshnessNow)
	if err != nil {
		t.Fatalf("无当前版本不应报错: %v", err)
	}
	if !result.Empty {
		t.Fatal("无当前版本时 empty 应为 true")
	}
	if result.Release != nil || result.Dict != nil || result.Axis != nil {
		t.Fatalf("空态不应带版本/字典/轴: %+v", result)
	}
	if result.Weeks == nil || len(result.Weeks) != 0 {
		t.Fatalf("空态 weeks 应为空数组，实际 %+v", result.Weeks)
	}
	if len(result.Buildings) != 1 || result.Buildings[0].BuildingID != "Q10" {
		t.Fatalf("白名单楼应照常返回: %+v", result.Buildings)
	}
	if recorder.calls != 0 {
		t.Fatalf("空态不应查新鲜度，实际 %d 次", recorder.calls)
	}

	// 空态的 JSON 形状是 design 的空态约定：release 为 null、empty 为 true。
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	want := `{"release":null,"dict":null,"axis":null,` +
		`"buildings":[{"building_id":"Q10","name":"综合教学楼"}],"weeks":[],"empty":true}`
	if string(encoded) != want {
		t.Fatalf("空态 JSON 不符:\n期望 %s\n实际 %s", want, encoded)
	}
}

// TestMetaJSONContract 锁定对外键名，防止漏写 json tag 时字段名变成驼峰。
func TestMetaJSONContract(t *testing.T) {
	svc, _ := newMetaTestService(fullMetaFacts(), FreshnessFacts{
		Weeks:         []WeekFacts{{Week: 2, Published: true, LastSuccessAt: freshAt(-time.Hour)}},
		LastRunStatus: "success",
	})
	result, err := svc.Meta(t.Context(), freshnessNow)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(encoded, &top); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}

	assertKeys(t, top, "release", "dict", "axis", "buildings", "weeks", "empty")
	assertKeys(t, top["release"].(map[string]any), "release_id", "generated_at")
	dict := top["dict"].(map[string]any)
	assertKeys(t, dict, "version", "states")
	assertKeys(t, dict["states"].(map[string]any)["free"].(map[string]any), "label", "available")
	axis := top["axis"].(map[string]any)
	assertKeys(t, axis, "version", "blocks", "nodes")
	assertKeys(t, axis["nodes"].([]any)[0].(map[string]any),
		"node", "ordinal", "block", "block_ordinal")
	assertKeys(t, top["buildings"].([]any)[0].(map[string]any), "building_id", "name")
	assertKeys(t, top["weeks"].([]any)[0].(map[string]any),
		"week", "published", "last_success_at", "stale")
}

// assertKeys 断言对象包含全部契约键。
func assertKeys(t *testing.T, object map[string]any, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			t.Fatalf("缺少契约键 %q，实际键: %v", key, metaKeysOf(object))
		}
	}
}

// metaKeysOf 返回 map 的键列表，仅用于失败信息。
func metaKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestMetaLookupError 覆盖存储层失败：错误原样上抛。
func TestMetaLookupError(t *testing.T) {
	wantErr := errors.New("db down")
	svc := NewMeta((&fakeMetaLookup{err: wantErr}).lookup, NewFreshness((&recordingFreshness{}).lookup))
	if _, err := svc.Meta(t.Context(), freshnessNow); !errors.Is(err, wantErr) {
		t.Fatalf("期望透传存储层错误，实际 %v", err)
	}
}

// TestMetaFreshnessError 覆盖新鲜度失败：错误原样上抛。
func TestMetaFreshnessError(t *testing.T) {
	wantErr := errors.New("freshness down")
	recorder := &recordingFreshness{err: wantErr}
	svc := NewMeta((&fakeMetaLookup{facts: fullMetaFacts()}).lookup, NewFreshness(recorder.lookup))
	if _, err := svc.Meta(t.Context(), freshnessNow); !errors.Is(err, wantErr) {
		t.Fatalf("期望透传新鲜度错误，实际 %v", err)
	}
}

// TestMetaNotWired 覆盖未装配存储层与新鲜度：明确报错而不是给出空结果。
func TestMetaNotWired(t *testing.T) {
	if _, err := NewMeta(nil, NewFreshness((&recordingFreshness{}).lookup)).
		Meta(t.Context(), freshnessNow); err == nil {
		t.Fatal("未装配存储层时应报错")
	}

	svc := NewMeta((&fakeMetaLookup{facts: fullMetaFacts()}).lookup, nil)
	if _, err := svc.Meta(t.Context(), freshnessNow); err == nil {
		t.Fatal("有当前版本但未装配新鲜度时应报错")
	}
}

// TestMetaEmptyStateWithoutFreshness 覆盖空态不依赖新鲜度：没装配也能返回空态。
func TestMetaEmptyStateWithoutFreshness(t *testing.T) {
	svc := NewMeta((&fakeMetaLookup{facts: MetaFacts{}}).lookup, nil)
	result, err := svc.Meta(t.Context(), freshnessNow)
	if err != nil {
		t.Fatalf("空态不应报错: %v", err)
	}
	if !result.Empty {
		t.Fatal("期望空态")
	}
}
