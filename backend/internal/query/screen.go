package query

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
)

// ScreenCacheTTL 是大屏聚合结果的复用窗口：同一业务日期内 30 秒（Requirement 33.7、CP15）。
// 与查询缓存分开成两个常量：两条需求的窗口目前都是 30 秒，但调整其中一条不该牵动另一条。
const ScreenCacheTTL = 30 * time.Second

// ViewScreen 是大屏缓存键的视图名，与 /api/query 的空教室、全天状态视图分开。
const ViewScreen = "screen"

// runStatusRunning 是「仍在跑」的轮次状态：它的耗时按已过去的时长给出。
const runStatusRunning = "running"

// 大屏矩阵的固定维度：7 天（周一…周日）× 5 个大节（design 响应形状）。
const (
	screenWeekdays = 7
	screenBlocks   = 5
)

// ScreenScale 是站点规模：房间数、教学楼数、学期、当前周与采集窗口。
// Window 是采集窗口 [起始周, 结束周]；没有可用的窗口时为空数组，不编造 [0,0]。
type ScreenScale struct {
	Rooms     int    `json:"rooms"`
	Buildings int    `json:"buildings"`
	Term      string `json:"term"`
	Week      int    `json:"week"`
	Window    []int  `json:"window"`
}

// ScreenRelease 是当前发布版本的对外形状：标识与生成时间，时间统一为 UTC。
type ScreenRelease struct {
	ReleaseID   string    `json:"release_id"`
	GeneratedAt time.Time `json:"generated_at"`
}

// ScreenLastRun 是最近一轮同步的状态、开始时间、耗时与归属冲突标记。
// 只含状态与时间：不含 error_code、错误原文与账号（Requirement 33.6、CP13）。
type ScreenLastRun struct {
	Status       string    `json:"status"`      // 轮次状态五档之一
	StartedAt    time.Time `json:"started_at"`  // 开始时刻，UTC
	DurationMS   *int64    `json:"duration_ms"` // 耗时毫秒；没有结束时刻且不在跑时为 null
	AttrConflict bool      `json:"attr_conflict"`
}

// ScreenAnomalies 是未知、复合与归属冲突的计数。
type ScreenAnomalies struct {
	UnknownCells   int `json:"unknown_cells"`
	CompositeCells int `json:"composite_cells"`
	AttrConflicts  int `json:"attr_conflicts"`
}

// ScreenBuildingOccupancy 是一栋教学楼的占用情况；Rate 是占用量除以格数。
type ScreenBuildingOccupancy struct {
	BuildingID string  `json:"building_id"`
	Name       string  `json:"name"`
	Cells      int     `json:"cells"`
	Occupied   int     `json:"occupied"`
	Rate       float64 `json:"rate"`
}

// ScreenOccupancy 是占用分布的两条口径。
// ByWeekdayBlock 固定 7 行（周一…周日）× 5 列（大节 1..5），值为占用率；
// 没有观测的格为 0 而不是 NaN。
type ScreenOccupancy struct {
	ByBuilding     []ScreenBuildingOccupancy `json:"by_building"`
	ByWeekdayBlock [][]float64               `json:"by_weekday_block"`
}

// ScreenResult 是 /api/screen 的 data 载荷。
// 形状对应 design 的 /api/screen 示例：基础块之后是访问统计块（Requirement 34，复用 9.9 的聚合）。
// 无当前版本时 Release 为 null、Empty 为 true，其余块给空/零值（Requirement 33.8）。
type ScreenResult struct {
	Scale            ScreenScale        `json:"scale"`
	Release          *ScreenRelease     `json:"release"`
	Weeks            []WeekFreshness    `json:"weeks"`
	LastRun          *ScreenLastRun     `json:"last_run"`
	Anomalies        ScreenAnomalies    `json:"anomalies"`
	Occupancy        ScreenOccupancy    `json:"occupancy"`
	Traffic          stats.Traffic      `json:"traffic"`
	Leaderboard      stats.Leaderboard  `json:"leaderboard"`
	Peaks            stats.Peaks        `json:"peaks"`
	Trend            []stats.TrendPoint `json:"trend"`
	NodeDistribution []stats.NodeHit    `json:"node_distribution"`
	Empty            bool               `json:"empty"`
}

// ScreenReleaseFacts 是存储层给出的当前版本与规模事实。
type ScreenReleaseFacts struct {
	ReleaseID   string    // 发布版本标识
	GeneratedAt time.Time // 版本生成时间
	Term        string    // 该版本所属学期
	TotalWeeks  int       // 该学期总周数，采集窗口缺失时兜底
	Rooms       int       // 归属快照里的房间数
	Buildings   int       // 归属快照里的教学楼数
}

// ScreenRunFacts 是一轮同步的存储层事实；不含错误码与错误原文。
type ScreenRunFacts struct {
	Status            string     // 轮次状态五档之一
	StartedAt         time.Time  // 开始时刻
	FinishedAt        *time.Time // 结束时刻；仍在跑或异常终止时为 nil
	WindowStart       *int       // 采集窗口起始周
	WindowEnd         *int       // 采集窗口结束周
	UnknownCells      int        // 未知格计数
	CompositeCells    int        // 复合格计数
	AttrConflictCount int        // 归属冲突计数
}

// ScreenOccupancyCell 是「楼 × 星期 × 大节」的占用聚合格事实。
// Occupied 在存储层已按字典投影算好：未知与复合都计入占用（Requirement 33.5、CP14）。
type ScreenOccupancyCell struct {
	BuildingID   string // 教学楼编号
	BuildingName string // 教学楼展示名
	Weekday      int    // 星期 1..7
	BlockOrdinal int    // 大节顺序 1..5；轴里查不到归属时为 0
	Cells        int    // 观测格数
	Occupied     int    // 其中不可用的格数
}

// ScreenFacts 是存储层注入的大屏事实快照；没有当前版本时 Release 为 nil。
type ScreenFacts struct {
	Release   *ScreenReleaseFacts   // 当前版本与规模
	Published *ScreenRunFacts       // 产出当前版本的那一轮
	LastRun   *ScreenRunFacts       // 最近一轮同步
	Occupancy []ScreenOccupancyCell // 楼 × 星期 × 大节的占用格
}

// ScreenLookup 是存储层注入点；用函数类型让 query 不依赖 pgx 与具体仓储实现。
type ScreenLookup func(ctx context.Context) (ScreenFacts, error)

// ScreenTrafficReader 是访问统计读取的注入点；*stats.Reader 直接满足它。
// 声明成窄接口而不是直接持有 *stats.Reader：大屏测试不用为访问统计造数据库替身。
type ScreenTrafficReader interface {
	Snapshot(ctx context.Context, now time.Time, params stats.ReadParams) (stats.Snapshot, error)
}

// ScreenService 聚合大屏数据：规模、版本、逐周新鲜度、最近轮次、异常计数与占用分布。
// 逐周新鲜度复用 FreshnessService，访问统计复用 stats.Reader，不另写一套判定与聚合。
type ScreenService struct {
	lookup     ScreenLookup        // 大屏事实读取，由装配层注入
	freshness  *FreshnessService   // 逐周新鲜度，复用 9.6 的实现
	traffic    ScreenTrafficReader // 访问统计，复用 9.9 的读取侧
	weekLookup WeekLookup          // 按业务日期取学期周，用来标出「当前周」
	cache      *ttlCache[ScreenResult]
}

// NewScreen 组装大屏服务。结果缓存固定 30 秒打开：Requirement 33.7 与 CP15 是接口契约，
// 不是可选优化项，所以不像空教室那样用 Option 控制。
func NewScreen(lookup ScreenLookup, freshness *FreshnessService, traffic ScreenTrafficReader, weekLookup WeekLookup) *ScreenService {
	return &ScreenService{
		lookup:     lookup,
		freshness:  freshness,
		traffic:    traffic,
		weekLookup: weekLookup,
		cache:      newTTLCache[ScreenResult](ScreenCacheTTL, 0),
	}
}

// Screen 返回大屏载荷；同一业务日期内 30 秒复用同一份结果（Requirement 33.7、CP15）。
// 只缓存成功结果：失败不入缓存，一次数据库抖动不该让大屏在这个窗口里一直报错。
func (s *ScreenService) Screen(ctx context.Context, now time.Time) (ScreenResult, error) {
	if s.lookup == nil {
		return ScreenResult{}, fmt.Errorf("大屏查询未装配")
	}

	// 键含解析后的业务日期：跨零点后「今天」变了，统计窗口与当前周都跟着变；
	// 接口没有其它查询参数，所以日期就是全部维度（将来暴露参数必须补进键）。
	key := screenCacheKey(now)
	if s.cache != nil {
		if cached, ok := s.cache.get(key, now); ok {
			return cloneScreenResult(cached), nil
		}
	}

	result, err := s.build(ctx, now)
	if err != nil {
		return ScreenResult{}, err
	}
	if s.cache != nil {
		s.cache.put(key, cloneScreenResult(result), now)
	}
	return result, nil
}

// build 是未命中缓存时的实时聚合。
func (s *ScreenService) build(ctx context.Context, now time.Time) (ScreenResult, error) {
	facts, err := s.lookup(ctx)
	if err != nil {
		return ScreenResult{}, err
	}

	result := emptyScreenResult()
	// 无当前版本：200 + release:null + empty:true，其余块保持空/零值（Requirement 33.8）。
	if facts.Release == nil {
		result.Empty = true
		return result, nil
	}

	// 生成时间统一 UTC：契约要求 RFC3339 UTC，存储层可能按会话时区回时刻。
	result.Release = &ScreenRelease{
		ReleaseID:   facts.Release.ReleaseID,
		GeneratedAt: facts.Release.GeneratedAt.UTC(),
	}

	week, err := s.currentWeek(ctx, now, facts.Release.Term)
	if err != nil {
		return ScreenResult{}, err
	}
	result.Scale = ScreenScale{
		Rooms:     facts.Release.Rooms,
		Buildings: facts.Release.Buildings,
		Term:      facts.Release.Term,
		Week:      week,
		Window:    screenWindow(facts.Published),
	}

	weeks, err := s.screenWeeks(ctx, now, facts.Release, result.Scale.Window)
	if err != nil {
		return ScreenResult{}, err
	}
	result.Weeks = weeks

	result.LastRun = screenLastRun(facts.LastRun, now)
	result.Anomalies = screenAnomalies(facts.Published)
	result.Occupancy = screenOccupancy(facts.Occupancy)

	if err := s.applyTraffic(ctx, now, &result); err != nil {
		return ScreenResult{}, err
	}
	return result, nil
}

// currentWeek 用今天的业务日期解析当前周次，只认当前版本所属学期。
// 今天不在教学周、或落在另一个学期时给 0：把别的学期的周次贴在这份数据上会读成同一周。
func (s *ScreenService) currentWeek(ctx context.Context, now time.Time, term string) (int, error) {
	if s.weekLookup == nil {
		return 0, fmt.Errorf("学期周查询未装配")
	}
	tw, ok, err := s.weekLookup(ctx, targetDate(now, 0))
	if err != nil {
		return 0, err
	}
	if !ok || !tw.InCalendar || tw.Term != term {
		return 0, nil
	}
	return int(tw.Week), nil
}

// screenWeeks 取窗口内各周的发布状态与新鲜度；窗口缺失时退回该学期全部教学周。
func (s *ScreenService) screenWeeks(ctx context.Context, now time.Time, release *ScreenReleaseFacts, window []int) ([]WeekFreshness, error) {
	if s.freshness == nil {
		return nil, fmt.Errorf("新鲜度查询未装配")
	}
	from, to := screenWeekRange(window, release.TotalWeeks)
	return s.freshness.Weeks(ctx, now, FreshnessParams{Term: release.Term, FromWeek: from, ToWeek: to})
}

// applyTraffic 把 9.9 的访问统计快照拼进大屏。
// 大屏热力与峰值默认取近 7 日：design 的 /api/screen 没有查询参数，7/30 切换留待接口扩展。
func (s *ScreenService) applyTraffic(ctx context.Context, now time.Time, result *ScreenResult) error {
	if s.traffic == nil {
		return fmt.Errorf("访问统计读取未装配")
	}
	snapshot, err := s.traffic.Snapshot(ctx, now, stats.ReadParams{PeakDays: stats.DefaultPeakDays})
	if err != nil {
		return err
	}
	result.Traffic = snapshot.Traffic
	result.Leaderboard = snapshot.Leaderboard
	result.Peaks = snapshot.Peaks
	result.Trend = snapshot.Trend
	result.NodeDistribution = snapshot.NodeDistribution
	return nil
}

// screenCacheKey 组装大屏缓存键：视图名 + 解析后的业务日期。
func screenCacheKey(now time.Time) cacheKey {
	return cacheKey{view: ViewScreen, date: targetDate(now, 0).Format(time.DateOnly)}
}

// screenWindow 把采集窗口整成 [起始周, 结束周]；窗口不完整或非法时给空数组。
func screenWindow(run *ScreenRunFacts) []int {
	if run == nil || run.WindowStart == nil || run.WindowEnd == nil {
		return make([]int, 0)
	}
	start, end := *run.WindowStart, *run.WindowEnd
	if start < 1 || end < start {
		return make([]int, 0)
	}
	return []int{start, end}
}

// screenWeekRange 给出逐周新鲜度要覆盖的区间：优先采集窗口，没有窗口时退回整学期。
// 返回值交给 FreshnessService 做边界判定（起点小于 1 或终点早于起点时返回空列表）。
func screenWeekRange(window []int, totalWeeks int) (int, int) {
	if len(window) == 2 {
		return window[0], window[1]
	}
	if totalWeeks > 0 {
		return 1, totalWeeks
	}
	return 0, 0
}

// screenLastRun 整理最近一轮同步；一轮都没跑过时返回 nil。
// 耗时口径：已结束给「结束 − 开始」；还在跑给「现在 − 开始」；其余没有结束时刻的给 null。
func screenLastRun(run *ScreenRunFacts, now time.Time) *ScreenLastRun {
	if run == nil {
		return nil
	}
	return &ScreenLastRun{
		Status:       run.Status,
		StartedAt:    run.StartedAt.UTC(),
		DurationMS:   screenDuration(run, now),
		AttrConflict: run.AttrConflictCount > 0,
	}
}

// screenDuration 按轮次状态给出耗时毫秒。
// 时钟回拨等异常会让差值变成负数，夹到 0：大屏宁可显示 0 也不显示负耗时。
func screenDuration(run *ScreenRunFacts, now time.Time) *int64 {
	var end time.Time
	switch {
	case run.FinishedAt != nil:
		end = *run.FinishedAt
	case run.Status == runStatusRunning:
		end = now
	default:
		return nil
	}

	ms := end.Sub(run.StartedAt).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return &ms
}

// screenAnomalies 取产出当前版本那一轮的未知、复合与归属冲突计数。
func screenAnomalies(run *ScreenRunFacts) ScreenAnomalies {
	if run == nil {
		return ScreenAnomalies{}
	}
	return ScreenAnomalies{
		UnknownCells:   run.UnknownCells,
		CompositeCells: run.CompositeCells,
		AttrConflicts:  run.AttrConflictCount,
	}
}

// blockKey 是 7 × 5 矩阵里的一格。
type blockKey struct {
	weekday int
	block   int
}

// screenOccupancy 把「楼 × 星期 × 大节」的聚合格折成两条口径。
// 楼口径按楼号升序输出，方便前端稳定排序；矩阵口径固定 7 × 5，缺格补 0。
func screenOccupancy(cells []ScreenOccupancyCell) ScreenOccupancy {
	byBuilding := make([]ScreenBuildingOccupancy, 0, len(cells))
	positions := make(map[string]int, len(cells))
	blockSums := make(map[blockKey]blockSum, screenWeekdays*screenBlocks)

	for _, cell := range cells {
		// 星期与大节越界只影响矩阵：楼口径的总数仍然要算进这栋楼。
		if cell.Weekday >= 1 && cell.Weekday <= screenWeekdays &&
			cell.BlockOrdinal >= 1 && cell.BlockOrdinal <= screenBlocks {
			key := blockKey{weekday: cell.Weekday, block: cell.BlockOrdinal}
			sum := blockSums[key]
			sum.cells += cell.Cells
			sum.occupied += cell.Occupied
			blockSums[key] = sum
		}

		// 归属缺失的格不进楼口径：没有楼号的一行无法在「按教学楼」里表达。
		if cell.BuildingID == "" {
			continue
		}
		pos, exists := positions[cell.BuildingID]
		if !exists {
			pos = len(byBuilding)
			positions[cell.BuildingID] = pos
			byBuilding = append(byBuilding, ScreenBuildingOccupancy{
				BuildingID: cell.BuildingID,
				Name:       cell.BuildingName,
			})
		}
		byBuilding[pos].Cells += cell.Cells
		byBuilding[pos].Occupied += cell.Occupied
	}

	// 不依赖存储层的 ORDER BY：替身给出的乱序事实也要得到同一份形状。
	sort.SliceStable(byBuilding, func(i, j int) bool {
		return byBuilding[i].BuildingID < byBuilding[j].BuildingID
	})
	for i := range byBuilding {
		byBuilding[i].Rate = occupancyRate(byBuilding[i].Occupied, byBuilding[i].Cells)
	}

	return ScreenOccupancy{
		ByBuilding:     byBuilding,
		ByWeekdayBlock: occupancyMatrix(blockSums),
	}
}

// blockSum 是一格的格数与占用量累计。
type blockSum struct {
	cells    int
	occupied int
}

// occupancyMatrix 把累计值铺成 7 × 5 的占用率矩阵；没有观测的格为 0。
func occupancyMatrix(blockSums map[blockKey]blockSum) [][]float64 {
	matrix := make([][]float64, screenWeekdays)
	for weekday := range matrix {
		matrix[weekday] = make([]float64, screenBlocks)
		for block := range matrix[weekday] {
			sum := blockSums[blockKey{weekday: weekday + 1, block: block + 1}]
			matrix[weekday][block] = occupancyRate(sum.occupied, sum.cells)
		}
	}
	return matrix
}

// occupancyRate 计算占用率；分母为 0 时给 0 而不是 NaN（Requirement 33.5、CP14）。
func occupancyRate(occupied, cells int) float64 {
	if cells <= 0 {
		return 0
	}
	return float64(occupied) / float64(cells)
}

// emptyScreenResult 是无当前版本时的载荷：所有需要版本的块都是空/零值。
// 切片给空数组而不是 nil，避免 JSON 里出现 null 让前端多做一次判空。
func emptyScreenResult() ScreenResult {
	return ScreenResult{
		Scale: ScreenScale{Window: make([]int, 0)},
		Weeks: make([]WeekFreshness, 0),
		Occupancy: ScreenOccupancy{
			ByBuilding:     make([]ScreenBuildingOccupancy, 0),
			ByWeekdayBlock: occupancyMatrix(nil),
		},
		Traffic: stats.Traffic{},
		Leaderboard: stats.Leaderboard{
			Buildings: make([]stats.BuildingHit, 0),
			Rooms:     make([]stats.RoomHit, 0),
			Keywords:  make([]stats.KeywordHit, 0),
		},
		Peaks: stats.Peaks{
			Heatmap: emptyHeatmap(),
			Top:     make([]stats.Peak, 0),
		},
		Trend:            make([]stats.TrendPoint, 0),
		NodeDistribution: make([]stats.NodeHit, 0),
	}
}

// emptyHeatmap 是 7 × 24 的全零热力矩阵；空态也保持固定形状，前端不必切换分支。
func emptyHeatmap() [][]int {
	matrix := make([][]int, screenWeekdays)
	for weekday := range matrix {
		matrix[weekday] = make([]int, 24)
	}
	return matrix
}

// cloneScreenResult 复制结果里的切片与指针。
// 缓存命中时返回副本，避免调用方改动响应结构时把缓存里的共享数据一起改掉。
func cloneScreenResult(result ScreenResult) ScreenResult {
	clone := result

	if result.Release != nil {
		release := *result.Release
		clone.Release = &release
	}
	if result.LastRun != nil {
		lastRun := *result.LastRun
		clone.LastRun = &lastRun
	}
	clone.Scale.Window = cloneInts(result.Scale.Window)
	clone.Weeks = cloneWeeks(result.Weeks)
	clone.Occupancy.ByBuilding = cloneBuildings(result.Occupancy.ByBuilding)
	clone.Occupancy.ByWeekdayBlock = cloneFloatMatrix(result.Occupancy.ByWeekdayBlock)
	clone.Leaderboard.Buildings = cloneSlice(result.Leaderboard.Buildings)
	clone.Leaderboard.Rooms = cloneSlice(result.Leaderboard.Rooms)
	clone.Leaderboard.Keywords = cloneSlice(result.Leaderboard.Keywords)
	clone.Peaks.Heatmap = cloneIntMatrix(result.Peaks.Heatmap)
	clone.Peaks.Top = cloneSlice(result.Peaks.Top)
	clone.Trend = cloneSlice(result.Trend)
	clone.NodeDistribution = cloneSlice(result.NodeDistribution)

	return clone
}

// cloneSlice 复制一个切片；nil 保持 nil，空的非 nil 切片保持为空数组。
func cloneSlice[T any](items []T) []T {
	if items == nil {
		return nil
	}
	clone := make([]T, len(items))
	copy(clone, items)
	return clone
}

// cloneInts 复制整数切片，用途与 cloneSlice 相同。
func cloneInts(items []int) []int { return cloneSlice(items) }

// cloneWeeks 复制逐周新鲜度切片，元素含指针，指针不共享。
func cloneWeeks(items []WeekFreshness) []WeekFreshness {
	if items == nil {
		return nil
	}
	clone := make([]WeekFreshness, len(items))
	for i, week := range items {
		clone[i] = week
		if week.LastSuccessAt != nil {
			at := *week.LastSuccessAt
			clone[i].LastSuccessAt = &at
		}
	}
	return clone
}

// cloneBuildings 复制按楼占用切片。
func cloneBuildings(items []ScreenBuildingOccupancy) []ScreenBuildingOccupancy {
	return cloneSlice(items)
}

// cloneIntMatrix 复制二维整数矩阵并逐行复制。
func cloneIntMatrix(matrix [][]int) [][]int {
	if matrix == nil {
		return nil
	}
	clone := make([][]int, len(matrix))
	for i, row := range matrix {
		clone[i] = cloneInts(row)
	}
	return clone
}

// cloneFloatMatrix 复制二维浮点矩阵并逐行复制。
func cloneFloatMatrix(matrix [][]float64) [][]float64 {
	if matrix == nil {
		return nil
	}
	clone := make([][]float64, len(matrix))
	for i, row := range matrix {
		if row == nil {
			continue
		}
		clone[i] = make([]float64, len(row))
		copy(clone[i], row)
	}
	return clone
}
