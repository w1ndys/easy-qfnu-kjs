package store

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件是 design「预算」段的性能实测夹具与用例（tasklist 第 9 项）。
//
// 它按真实发布数据的形状造两版：每版约 87 万条观测（小节粒度）、约 36 万格
// （房号 × 周 × 星期 × 大节），再用生产用的 SQL 量「汇总 + 首页」与
// `section=rooms` 的耗时。观测、归属快照与教室目录的批量写入走 COPY（CopyFrom），
// 与发布事务的写侧一致；逐行 INSERT 造 174 万行会慢到失去意义（`term` / `release`
// 各只有一两行，仍用 INSERT）。
//
// 用例是 opt-in 的：会重建 public schema 并写 174 万行观测，默认跳过，只有显式
// 设置 KJS_PERF=1 才跑。门禁里的 `go test -p 1 -count=1 ./...` 因此不会被它拖慢；
// 未设置 KJS_TEST_DSN 时仍按其它集成用例的惯例先跳过。

// perfOptInEnv 是性能实测的显式开关。
const perfOptInEnv = "KJS_PERF"

// perfOptInValue 是开关的生效值：只认 "1"，避免 KJS_PERF=0 之类的误判成「开」。
const perfOptInValue = "1"

// 性能夹具的规模与 shape。
//
// 取这些数字而不是随便凑一个 87 万：
//   - 房号数 2585 与真实全校房间数（约 2481 间）同一量级；
//   - 4 周 × 7 天与 design 里的「一个版本覆盖窗口内至多 4 周」一致；
//   - 12 个小节折叠成 5 个大节（0001 基线种子的轴），所以每个观测点（房号 × 周 ×
//     星期）写 12 条观测、折叠成 5 个格（大节），两版各约 36 万格。
//
// 两版的观测行数刻意不等（from 少 1 020 行、to 正好 87 万），差异来自 35 个消失
// 观测点与 120 个新增观测点；折算成「格」（大节）后三类差异是 600 / 175 / 10 240。
const (
	perfBaseRooms     = 2585 // 两版共有的房号数
	perfWeekCount     = 4    // 窗口内的教学周数
	perfWeekdayCount  = 7    // 每周天数：观测允许 1–7，包含周末
	perfNodeCount     = 12   // 一个观测点在轴上的小节数
	perfBlocksPerSite = 5    // 轴 v1 把 12 个小节折叠成 5 个大节
	perfChangedSites  = 2048 // 两侧都有、但状态不同的观测点（每个点的 5 个格一起变）
	perfAddedRooms    = 120  // 只在 to 版出现的房号，每间一个观测点
	perfRemovedRooms  = 35   // 只在 from 版出现的房号，每间一个观测点
	perfFirstWeek     = 9    // 窗口从第 9 周开始

	perfTerm      = "2026-2027-1"
	perfFromID    = "20261101T000000Z" // 旧版本，非 current
	perfToID      = "20261115T000000Z" // 新版本，current
	perfFromGenAt = "2026-11-01T00:00:00Z"
	perfToGenAt   = "2026-11-15T00:00:00Z"
)

// perfBaseSites 是两版共有网格的观测点数：房号 × 周 × 星期 = 72 380。
const perfBaseSites = perfBaseRooms * perfWeekCount * perfWeekdayCount

// perfNodes 是轴 v1 的 12 个小节编码，顺序与 axis_node.ordinal 一致。
var perfNodes = [perfNodeCount]string{
	"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11", "12",
}

// perfSite 是夹具里的一个观测点：房号 × 周 × 星期。
//
// 它不是 design 里按大节计的「格」：一个观测点写 12 条小节观测，折叠后得到 5 个格
// （轴 v1 的 5 个大节）。差异计数要先折算，见 seedReleaseAdminPerfFixture 的断言。
//
// changed 只对 to 版有意义：该点在 from 版是 class、在 to 版是 free，折叠后两侧
// state_key 不同，因此 5 个格都计入「状态变化」。点内的 12 个小节状态恒一致，这样
// 块内不一致计数为 0，和正常发布数据一致。
type perfSite struct {
	roomID  string
	week    int16
	weekday int16
	changed bool
}

// perfRoom 是教室目录与归属快照的一行。
type perfRoom struct {
	roomID       string
	buildingID   string
	buildingName string
}

// perfFixture 是实测用例要回填给断言与日志的夹具事实。
type perfFixture struct {
	fromID, toID string

	fromRows, toRows   int64 // 观测行数（小节粒度）
	fromSites, toSites int   // 观测点数：房号 × 周 × 星期
	fromRooms, toRooms int   // 归属快照房号数
	fromBlockCells     int   // 格（大节）数：观测点数 × 5
	toBlockCells       int   // 同上
}

// perfRoomID 把房号序号（从 1 起）编成稳定的字符串。
//
// 用零填充的 P + 4 位数字：room_id 是 text，固定宽度让「按 room_id 排序」与
// 「按序号排序」一致，翻页的排序键不会因为字符串长度不同而和直觉分叉。
func perfRoomID(index int) string {
	return fmt.Sprintf("P%04d", index)
}

// perfBaseSite 还原基础网格里第 index 个观测点（0 起）：先填满一间房的 4 × 7 点，
// 再换下一间房，这样 changed 的挑选（index % 35 == 0）能均匀散落到全表。
func perfBaseSite(index int) perfSite {
	weekdayIndex := index % perfWeekdayCount
	weekIndex := index / perfWeekdayCount % perfWeekCount
	roomIndex := index / (perfWeekCount * perfWeekdayCount)
	return perfSite{
		roomID:  perfRoomID(roomIndex + 1),
		week:    int16(perfFirstWeek + weekIndex),
		weekday: int16(weekdayIndex + 1),
	}
}

// perfFromSites 造 from 版的观测点集合：全部基础网格 + 35 个「消失房」各一个点。
func perfFromSites() []perfSite {
	sites := make([]perfSite, 0, perfBaseSites+perfRemovedRooms)
	for index := 0; index < perfBaseSites; index++ {
		sites = append(sites, perfBaseSite(index))
	}
	for offset := 0; offset < perfRemovedRooms; offset++ {
		sites = append(sites, perfSite{
			roomID:  fmt.Sprintf("RX%03d", offset+1),
			week:    int16(perfFirstWeek),
			weekday: 1,
		})
	}
	return sites
}

// perfToSites 造 to 版的观测点集合：全部基础网格（其中 2048 个点改成 free）+ 120 个
// 「新增房」各一个点。两版的基础网格完全相同，差异只来自这三类，正是断言要的
// 120 / 35 / 2048 个观测点（折算成格是 600 / 175 / 10 240）。
func perfToSites() []perfSite {
	sites := make([]perfSite, 0, perfBaseSites+perfAddedRooms)
	for index := 0; index < perfBaseSites; index++ {
		site := perfBaseSite(index)
		// 每 35 个点取一个，取满 2048 个：35 与 72 380 的最大公约数是 35，
		// 步长 35 在 [0, 72 380) 内恰好走完 2068 个互不相同的点，2048 一定不重复。
		site.changed = index%35 == 0 && index/35 < perfChangedSites
		sites = append(sites, site)
	}
	for offset := 0; offset < perfAddedRooms; offset++ {
		sites = append(sites, perfSite{
			roomID:  fmt.Sprintf("RA%03d", offset+1),
			week:    int16(perfFirstWeek),
			weekday: 1,
		})
	}
	return sites
}

// perfRooms 造教室目录：2585 间基础房 + 120 间新增房 + 35 间消失房。
//
// 每 100 间分一栋楼，与真实数据「一栋楼几十到上百间」的比例接近；新增房与消失房
// 各归一个独立的楼，让 section=rooms 的归属字段两侧都有内容而不是全 nil。
func perfRooms() []perfRoom {
	rooms := make([]perfRoom, 0, perfBaseRooms+perfAddedRooms+perfRemovedRooms)
	for index := 1; index <= perfBaseRooms; index++ {
		buildingNumber := (index-1)/100 + 1
		rooms = append(rooms, perfRoom{
			roomID:       perfRoomID(index),
			buildingID:   fmt.Sprintf("QB%02d", buildingNumber),
			buildingName: fmt.Sprintf("测试楼%02d", buildingNumber),
		})
	}
	for offset := 0; offset < perfRemovedRooms; offset++ {
		rooms = append(rooms, perfRoom{
			roomID:       fmt.Sprintf("RX%03d", offset+1),
			buildingID:   "QR1",
			buildingName: "已撤楼",
		})
	}
	for offset := 0; offset < perfAddedRooms; offset++ {
		rooms = append(rooms, perfRoom{
			roomID:       fmt.Sprintf("RA%03d", offset+1),
			buildingID:   "QA1",
			buildingName: "新增楼",
		})
	}
	return rooms
}

// seedReleaseAdminPerfFixture 铺两版 87 万行夹具并带回两版的规模事实。
//
// 写入顺序：term → release → room / release_room_building → observation。
// 观测必须最后写：observation 的 room_id 外键指向 room，先写会被外键挡住。
// 所有批量写入都走 COPY，与发布事务的写侧同一条路。
func seedReleaseAdminPerfFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) perfFixture {
	t.Helper()

	fromSites := perfFromSites()
	toSites := perfToSites()
	rooms := perfRooms()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, 20)", perfTerm)
	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $3, $4, 1, 1, false, false),
       ($2, $3, $5, 1, 1, false, true)`,
		perfFromID, perfToID, perfTerm,
		mustParseReleaseAdminTime(t, perfFromGenAt),
		mustParseReleaseAdminTime(t, perfToGenAt))

	if _, err := pool.CopyFrom(ctx, pgx.Identifier{"room"},
		[]string{"room_id", "name", "name_raw", "building_id", "building_name"},
		pgx.CopyFromSlice(len(rooms), func(index int) ([]any, error) {
			room := rooms[index]
			return []any{room.roomID, room.roomID, room.roomID, room.buildingID, room.buildingName}, nil
		})); err != nil {
		t.Fatalf("COPY 教室目录失败: %v", err)
	}

	// 两版的归属快照：from 是基础房 + 消失房，to 是基础房 + 新增房。
	// 快照行数各 2 620 / 2 705，与真实数据的几千行同一量级。
	fromRooms := append(slices.Clone(rooms[:perfBaseRooms]),
		rooms[perfBaseRooms:perfBaseRooms+perfRemovedRooms]...)
	toRooms := append(slices.Clone(rooms[:perfBaseRooms]),
		rooms[perfBaseRooms+perfRemovedRooms:]...)
	snapshots := []struct {
		releaseID string
		rooms     []perfRoom
	}{
		{perfFromID, fromRooms},
		{perfToID, toRooms},
	}
	for _, snapshot := range snapshots {
		if _, err := pool.CopyFrom(ctx, pgx.Identifier{"release_room_building"},
			roomBuildingColumns,
			pgx.CopyFromSlice(len(snapshot.rooms), func(index int) ([]any, error) {
				room := snapshot.rooms[index]
				return []any{snapshot.releaseID, room.roomID, room.buildingID, room.buildingName}, nil
			})); err != nil {
			t.Fatalf("COPY 版本 %s 的归属快照失败: %v", snapshot.releaseID, err)
		}
	}

	// 观测：每个观测点 12 行（12 个小节）。COPY 一次性流式写入，不把 87 万行物化在内存里。
	start := time.Now()
	copyPerfObservations(ctx, t, pool, perfFromID, fromSites)
	copyPerfObservations(ctx, t, pool, perfToID, toSites)
	t.Logf("夹具写入耗时 %s：from %d 行 / to %d 行",
		time.Since(start), len(fromSites)*perfNodeCount, len(toSites)*perfNodeCount)

	// 大批量写入后必须 ANALYZE：没有统计信息时规划器会按默认选择率猜，量出来的
	// 耗时可能来自一个生产上不会出现的计划（比如把观察表的顺序扫换成索引扫）。
	mustExec(ctx, t, pool, "ANALYZE release")
	mustExec(ctx, t, pool, "ANALYZE room")
	mustExec(ctx, t, pool, "ANALYZE release_room_building")
	mustExec(ctx, t, pool, "ANALYZE observation")

	fixture := perfFixture{
		fromID:         perfFromID,
		toID:           perfToID,
		fromRows:       int64(len(fromSites) * perfNodeCount),
		toRows:         int64(len(toSites) * perfNodeCount),
		fromSites:      len(fromSites),
		toSites:        len(toSites),
		fromRooms:      len(fromRooms),
		toRooms:        len(toRooms),
		fromBlockCells: len(fromSites) * perfBlocksPerSite,
		toBlockCells:   len(toSites) * perfBlocksPerSite,
	}
	assertPerfObservationRows(ctx, t, pool, fixture)
	return fixture
}

// copyPerfObservations 用 COPY 写一版全部观测。
//
// 行源是 CopyFromSlice 的惰性回调：第 index 行 = 第 index/12 个观测点的第 index%12
// 个小节。这样不预先造 87 万个 []any，内存只跟观测点集合（7 万多条）走。
//
// 状态：changed 点在 to 版是 free（available=true），其余是 class（available=false）。
// 落在 observation_free 部分索引里的只有 2048 × 12 行，与真实数据「大部分格被占」
// 的索引体积接近。
func copyPerfObservations(ctx context.Context, t *testing.T, pool *pgxpool.Pool, releaseID string, sites []perfSite) {
	t.Helper()

	total := len(sites) * perfNodeCount
	rows, err := pool.CopyFrom(ctx, pgx.Identifier{"observation"}, observationColumns,
		pgx.CopyFromSlice(total, func(index int) ([]any, error) {
			site := sites[index/perfNodeCount]
			node := perfNodes[index%perfNodeCount]
			state, available := "class", false
			if site.changed {
				state, available = "free", true
			}
			return []any{releaseID, site.roomID, perfTerm, site.week, site.weekday, node, state, available, nil}, nil
		}))
	if err != nil {
		t.Fatalf("COPY 版本 %s 的观测失败: %v", releaseID, err)
	}
	if rows != int64(total) {
		t.Fatalf("COPY 版本 %s 的观测写入 %d 行，期望 %d 行", releaseID, rows, total)
	}
}

// assertPerfObservationRows 回查两版的观测行数，确认 COPY 真的落库。
//
// 用例的耗时断言只在夹具规模正确时才有意义；行数不符时先停在这里，而不是拿一份
// 缩水的数据量出一份「很快」的结论。
func assertPerfObservationRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, fixture perfFixture) {
	t.Helper()

	for _, want := range []struct {
		releaseID string
		rows      int64
	}{{fixture.fromID, fixture.fromRows}, {fixture.toID, fixture.toRows}} {
		var got int64
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM observation WHERE release_id = $1", want.releaseID).Scan(&got); err != nil {
			t.Fatalf("回查版本 %s 的观测行数失败: %v", want.releaseID, err)
		}
		if got != want.rows {
			t.Fatalf("版本 %s 的观测行数为 %d，期望 %d", want.releaseID, got, want.rows)
		}
	}

	// 顺带记一下物理体积：耗时结论要和「表多大」一起看，否则换一台机器复现时
	// 无法判断差异是不是数据量导致的。
	var sizeBytes int64
	if err := pool.QueryRow(ctx, "SELECT pg_total_relation_size('observation')").Scan(&sizeBytes); err != nil {
		t.Fatalf("读取 observation 体积失败: %v", err)
	}
	t.Logf("observation 总大小 %.1f MiB（两版共 %d 行）",
		float64(sizeBytes)/(1<<20), fixture.fromRows+fixture.toRows)
}

// perfStats 是一次实测的耗时分布，单位是 time.Duration。
type perfStats struct {
	min     time.Duration
	median  time.Duration
	max     time.Duration
	samples []time.Duration
}

// perfRuns 是每组耗时的采样次数。
//
// 5 次足以看出抖动幅度，又不会让整个用例跑太久；每组都先预热一次，所以样本反映的
// 是稳定状态的耗时；夹具刚建完的首次调用单独在 TestReleaseAdminDiffPerformance 里
// 记一次，它不代表进程刚起、shared_buffers 未热的状态。
const perfRuns = 5

// measureRuns 量 action 的耗时：先预热一次（连接、计划、页缓存），再连续量 perfRuns 次。
//
// 首次调用单独在调用处记：预热后的数字才是「管理面板反复点对比」的稳定口径；首次
// 调用量的是夹具刚建完、刚 ANALYZE 过的状态，不代表进程刚起、shared_buffers 未热。
// 中位数取排序后的上中位数（perfRuns 为奇数时就是正中间那次）；预算按最大值判定，
// 见 TestReleaseAdminDiffPerformance。
func measureRuns(t *testing.T, name string, action func() error) perfStats {
	t.Helper()

	if err := action(); err != nil {
		t.Fatalf("%s 预热失败: %v", name, err)
	}

	samples := make([]time.Duration, 0, perfRuns)
	for range perfRuns {
		start := time.Now()
		if err := action(); err != nil {
			t.Fatalf("%s 第 %d 次执行失败: %v", name, len(samples)+1, err)
		}
		samples = append(samples, time.Since(start))
	}

	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	stats := perfStats{
		min:     sorted[0],
		median:  sorted[len(sorted)/2],
		max:     sorted[len(sorted)-1],
		samples: samples,
	}
	t.Logf("%s：中位 %.1f ms（最小 %.1f ms / 最大 %.1f ms，样本 %v）",
		name,
		perfMillis(stats.median), perfMillis(stats.min), perfMillis(stats.max),
		perfSampleMillis(stats.samples))
	return stats
}

// perfMillis 把耗时折成毫秒浮点，日志里只保留一位小数。
func perfMillis(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

// perfSampleMillis 把样本切片折成毫秒浮点（一位小数），便于日志里一行看完。
func perfSampleMillis(samples []time.Duration) []float64 {
	millis := make([]float64, len(samples))
	for index, sample := range samples {
		millis[index] = math.Round(perfMillis(sample)*10) / 10
	}
	return millis
}

// TestReleaseAdminDiffPerformance 是 design「预算」段的实测用例。
//
// 量五组数字：
//  1. 「汇总 + 首页」= Diff(section=cells, limit=50) 整次调用（含两侧头、汇总、首页），
//     对应契约里的默认对比请求，目标 ≤ 2 秒；
//  2. 汇总语句单独执行（两侧各折叠一次）；
//  3. cells 首页语句单独执行；
//  4. section=rooms 的首页语句单独执行，目标 ≤ 50 毫秒；
//  5. Diff(section=rooms) 整次调用（它仍要先算汇总，所以比第 4 条重）。
//
// 夹具同时断言三类格差异（600 / 175 / 10 240，由 120 / 35 / 2048 个观测点折算）
// 与房号差异（120 / 35 / 2585），保证量的是真实存在的差异而不是一份空结果。
//
// 预算按 5 次样本的最大值判定：任一次超预算即失败，中位数与最小值只记日志。
func TestReleaseAdminDiffPerformance(t *testing.T) {
	if os.Getenv(perfOptInEnv) != perfOptInValue {
		t.Skipf("未设置 %s=1，跳过 87 万行性能实测（用例会重建 schema 并写 174 万行观测）", perfOptInEnv)
	}

	// 60 秒的默认 context 对这套夹具太紧：实测整条用例已用掉 50 秒左右，
	// 只剩 10 秒余量。单独放宽到 10 分钟，避免把「用例没跑完」误报成「查询变慢」。
	ctx, pool := openReleaseAdminTestDBWithTimeout(t, 10*time.Minute)
	fixture := seedReleaseAdminPerfFixture(ctx, t, pool)

	t.Logf("夹具：from %d 行 / %d 观测点 / %d 格(大节) / %d 房；to %d 行 / %d 观测点 / %d 格(大节) / %d 房",
		fixture.fromRows, fixture.fromSites, fixture.fromBlockCells, fixture.fromRooms,
		fixture.toRows, fixture.toSites, fixture.toBlockCells, fixture.toRooms)

	repo := NewReleaseAdminRepo(pool)
	cellsParams := ReleaseDiffParams{
		From:    fixture.fromID,
		To:      fixture.toID,
		Section: DiffSectionCells,
		Kind:    DiffKindAll,
		Limit:   diffLimitDefault,
	}
	roomsParams := cellsParams
	roomsParams.Section = DiffSectionRooms

	// 夹具刚建完的首次调用：此时已 ANALYZE、页缓存里还是刚 COPY 进去的数据，
	// 量的是第一次走这条代码路径的固定开销，不代表进程刚起、shared_buffers 未热。
	start := time.Now()
	diff, err := repo.Diff(ctx, cellsParams)
	if err != nil {
		t.Fatalf("首次对比失败: %v", err)
	}
	t.Logf("汇总 + 首页（夹具刚建完的首次调用）：%.1f ms", perfMillis(time.Since(start)))

	// 三类格差异与房号差异：先确认夹具真的造出了差异，再谈耗时。
	//
	// 期望值要把观测点数折算成格（大节）：一个观测点的 12 个小节折叠成 5 个格，
	// 所以 120 / 35 / 2048 个点分别对应 600 / 175 / 10 240 个格。房号差异按快照
	// 计，不需要折算。
	wantAddedCells := perfAddedRooms * perfBlocksPerSite
	wantRemovedCells := perfRemovedRooms * perfBlocksPerSite
	wantChangedCells := perfChangedSites * perfBlocksPerSite
	if got := diff.Summary.Cells; got.Added != wantAddedCells || got.Removed != wantRemovedCells ||
		got.Changed != wantChangedCells || got.Total != wantAddedCells+wantRemovedCells+wantChangedCells {
		t.Fatalf("格差异计数不符：%+v（期望 added=%d removed=%d changed=%d）",
			got, wantAddedCells, wantRemovedCells, wantChangedCells)
	}
	if got := diff.Summary.Rooms; got.Added != perfAddedRooms || got.Removed != perfRemovedRooms ||
		got.Shared != perfBaseRooms || got.BuildingChanged != 0 {
		t.Fatalf("房号差异计数不符：%+v", got)
	}
	if diff.Summary.InconsistentBlocks != 0 {
		t.Fatalf("夹具的块内状态应一致，实际不一致块 %d", diff.Summary.InconsistentBlocks)
	}
	if len(diff.Cells) != diffLimitDefault {
		t.Fatalf("cells 首页应有 %d 行，实际 %d 行", diffLimitDefault, len(diff.Cells))
	}

	// 明细分页语句直接复用生产函数，不复制扫描逻辑：量的就是 Diff 里跑的那几条 SQL。
	fromSide, err := loadReleaseDiffSide(ctx, pool, "from", fixture.fromID)
	if err != nil {
		t.Fatalf("读 from 侧版本头失败: %v", err)
	}
	toSide, err := loadReleaseDiffSide(ctx, pool, "to", fixture.toID)
	if err != nil {
		t.Fatalf("读 to 侧版本头失败: %v", err)
	}

	cellsRun := measureRuns(t, "Diff section=cells（汇总 + 首页）", func() error {
		_, err := repo.Diff(ctx, cellsParams)
		return err
	})
	summaryRun := measureRuns(t, "汇总语句", func() error {
		_, err := loadReleaseDiffSummary(ctx, pool, fromSide, toSide)
		return err
	})
	cellsPageRun := measureRuns(t, "cells 首页语句（limit=50）", func() error {
		_, err := loadDiffCells(ctx, pool, cellsParams, fromSide, toSide)
		return err
	})
	roomsPageRun := measureRuns(t, "rooms 首页语句（limit=50）", func() error {
		_, err := loadDiffRooms(ctx, pool, roomsParams, fromSide, toSide)
		return err
	})
	roomsRun := measureRuns(t, "Diff section=rooms（汇总 + rooms 首页）", func() error {
		_, err := repo.Diff(ctx, roomsParams)
		return err
	})

	// 预算来自 design「预算」段：汇总 + 第一页 ≤ 2 秒；rooms 分页语句 ≤ 50 毫秒。
	// 按 5 次样本的最大值判定——任一次超预算就算不达标，不能让中位数把一次超预算的
	// 样本盖过去，否则「管理面板偶尔卡 2 秒以上」会被判成通过。
	if budget := 2 * time.Second; cellsRun.max > budget {
		t.Fatalf("汇总 + 首页最大 %.1f ms 超过预算 %s；按 design 的备选方案应把汇总与明细分页合并为单条语句",
			perfMillis(cellsRun.max), budget)
	}
	if budget := 50 * time.Millisecond; roomsPageRun.max > budget {
		t.Fatalf("rooms 首页最大 %.1f ms 超过预算 %s", perfMillis(roomsPageRun.max), budget)
	}

	// 汇总语句与首页语句加起来应当接近整次调用：差额就是两次版本头读与开事务的固定开销。
	t.Logf("拆解：汇总 %.1f ms + cells 首页 %.1f ms = %.1f ms；整次调用 %.1f ms",
		perfMillis(summaryRun.median), perfMillis(cellsPageRun.median),
		perfMillis(summaryRun.median+cellsPageRun.median), perfMillis(cellsRun.median))
	t.Logf("rooms 小节：分页语句 %.1f ms；整次调用 %.1f ms（含两端都有的汇总）",
		perfMillis(roomsPageRun.median), perfMillis(roomsRun.median))
}
