package e2e

// 本文件是 admin-release 的端到端检查点（tasklist 第 8 项）：
// init → login → 种两版（含归属冲突与缺周）→ 列表与详情 → 对比三类差异与分页 →
// 预热三处 30 秒缓存（空教室、全天状态、大屏）→ 回退 → 同一进程内用同参数再查
// （不睡 30 秒）→ 大屏版本标识同步切换 → 列表仍两行且 current 恰好一条指向回退目标 →
// collect_run 未变 → 每个管理端响应都带 Cache-Control: no-store。
//
// 装配口径照抄 cmd/server/main.go：store.NewReleaseAdminRepo(pool) 交给
// api.WithReleaseAdmin，第二个参数是真实的 `func(){ queries.Invalidate();
// screenService.Invalidate() }` 回调；公开查询服务按 main.go 的映射逐段接上
// （空教室、全天状态、逐周新鲜度、访问统计、大屏）。本检查点不接 meta 端点：
// 它不在第 8 项的验收范围里，接上只会多一份与 main.go 重复的映射。
//
// 与 collect_pipeline_test.go 的 publicQueries 有一处**刻意的**差别：这里打开
// 30 秒缓存（query.WithCache(query.DefaultCacheTTL)）。本检查点要验证的正是
// 「回退提交后缓存先于响应清空」（design D8、CP10）：不打开缓存时同参数查询必然
// 回库，断言永远成立却什么都没证明；打开后若装配漏了 Invalidate，回退后的同参数
// 查询会在剩余 TTL 内继续返回旧版本的结果，用例当场红。
//
// 夹具的口径：
//   - 学期日历锚在「运行检查点的这一周」，今天固定是第 1 周、星期取今天的真实星期。
//     公开查询只按业务日期（今天）反查周次，写死日期的夹具会在某些日子里跑出
//     「不在教学周」的空态，变成一条随日期飘的用例；
//   - 两版在同一个 (房号, 周, 星期, 大节) 上刻意做出三类差异各至少一条；
//   - 旧版有归属冲突（release.has_attr_conflict + collect_run_room.conflict）与缺周
//     （有观测的周是 1、2、4，缺第 3 周），新版的观测周没有缺口；
//   - 新版 current、旧版非 current，回退方向是「新版 → 旧版」。
//
// 与其它带库的检查点一样只在设置 KJS_TEST_DSN 时运行，并且会重建 public schema；
// 带 DSN 跑全量测试必须固定 `-p 1`（原因见 collect_pipeline_test.go 的包注释）。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/stats"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// 夹具的固定身份。版本 id 必须是 `YYYYMMDDTHHMMSSZ` 形状：管理端参数门按发布侧的
// 布局校验路径参数（design D13），形状不对的 id 连仓储都进不去。
const (
	adminReleaseE2EPreviousID = "20261028T041500Z" // 旧版：非 current，带归属冲突与缺周
	adminReleaseE2ECurrentID  = "20261111T100300Z" // 新版：current

	adminReleaseE2ECurrentGenAt       = "2026-11-11T10:03:00Z" // 新版生成时间
	adminReleaseE2EPreviousGenAt      = "2026-10-28T04:15:00Z" // 旧版生成时间
	adminReleaseE2ECurrentStartedAt   = "2026-11-11T10:00:00Z"
	adminReleaseE2ECurrentFinishedAt  = "2026-11-11T10:04:00Z"
	adminReleaseE2EPreviousStartedAt  = "2026-10-28T04:12:00Z"
	adminReleaseE2EPreviousFinishedAt = "2026-10-28T04:16:00Z"

	adminReleaseE2ECurrentReleasedAt  = "2026-11-11T10:05:00Z" // 新版关联轮次的 released_at
	adminReleaseE2EPreviousReleasedAt = "2026-10-28T04:16:22Z" // 旧版关联轮次的 released_at

	adminReleaseE2ERoomA = "A101" // 归属换楼 + 两版可见性互换的房间
	adminReleaseE2ERoomB = "B201" // 两版共有、格状态互换
	adminReleaseE2ERoomC = "C103" // 只旧版有（rooms.removed）
	adminReleaseE2ERoomD = "D104" // 只新版有（rooms.added）

	adminReleaseE2EBuildingQ10     = "Q10"
	adminReleaseE2EBuildingQ10Name = "综合教学楼"
	adminReleaseE2EBuildingQ03     = "Q03"
	adminReleaseE2EBuildingQ03Name = "老文史楼"
	adminReleaseE2EBuildingQ11     = "Q11"
	adminReleaseE2EBuildingQ11Name = "实验楼"

	// 学期总周数：只影响 term_week 的行数与 meta/新鲜度的兜底区间，取与既有检查点一致的 20。
	adminReleaseE2ETotalWeeks = 20
)

// 两版夹具的期望计数，全部由 adminReleaseE2EObservationSeeds 手工折叠得出
// （折叠键：房号 × 周 × 星期 × 大节；同一块内多个小节只算一格）。
//
// 新版：第 1 周 A101 的 01..11 与 B201 的 01..11 → 22 行观测、10 个格；
// 第 2 周 A101 与 B201 各 01..02 → 4 行观测、2 个格；合计 26 行、12 格、2 个周。
// 旧版：第 1 周同样 22 行、10 个格；第 2 周 B201 的 01..02 → 2 行、1 格；
// 第 4 周 C103 的 06 → 1 行、1 格；合计 25 行、12 格、3 个周（第 3 周缺）。
const (
	adminReleaseE2ECurrentObservationRows  = 26
	adminReleaseE2ECurrentBlockCells       = 12
	adminReleaseE2EPreviousObservationRows = 25
	adminReleaseE2EPreviousBlockCells      = 12

	// 对比（from=旧版、to=新版）的三类格差异：
	//   added   A101 第 2 周星期一的 0102 只在新版有；
	//   removed C103 第 4 周星期二的 0607 只在旧版有；
	//   changed A101 与 B201 第 1 周的 030405 两版状态互换（class ↔ free），折叠取 min 后不同。
	adminReleaseE2ECellsAdded   = 1
	adminReleaseE2ECellsRemoved = 1
	adminReleaseE2ECellsChanged = 2

	// 归属快照的房号级差异：added D104、removed C103、shared A101+B201、
	// building_changed 只有 A101（Q03 → Q10）。
	adminReleaseE2ERoomsAdded   = 1
	adminReleaseE2ERoomsRemoved = 1
	adminReleaseE2ERoomsShared  = 2
	adminReleaseE2ERoomsMoved   = 1
)

// adminReleaseE2EFixture 是本检查点的学期身份：学期名、本周的星期与第 1 周周一。
type adminReleaseE2EFixture struct {
	Term          string    // 学期标识，两版与观测都用它
	Weekday       int       // 今天在这一周里的星期，1=周一 … 7=周日
	WeekOneMonday time.Time // 第 1 周周一（业务时区的 00:00），term_week 的首行
}

// newAdminReleaseE2EFixture 以运行时刻推出夹具的学期日历锚点。
//
// 今天必须落在第 1 周：公开查询的目标日期只有「今天 + date_offset」，offset 0 是最稳的；
// 把日历锚到本周周一之后，service 反查出来的 (term, week, weekday) 一定是
// (fx.Term, 1, fx.Weekday)，与观测种子的写入位置逐项对上。
//
// 学期名只作夹具标签，但按「9 月/2 月开学」的常见口径生成：3 月跑出来一个
// 「…-1」的学期名不会影响任何断言，只会让看日志的人先花时间确认它不是 bug。
func newAdminReleaseE2EFixture(now time.Time) adminReleaseE2EFixture {
	local := now.In(shanghai)

	weekday := int(local.Weekday())
	if weekday == 0 {
		weekday = 7 // Go 的周日是 0，业务口径是 7
	}

	date := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)

	startYear := local.Year()
	if local.Month() <= time.July {
		startYear--
	}
	semester := 1
	if local.Month() >= time.February && local.Month() <= time.July {
		semester = 2
	}

	return adminReleaseE2EFixture{
		Term:          fmt.Sprintf("%d-%d-%d", startYear, startYear+1, semester),
		Weekday:       weekday,
		WeekOneMonday: date.AddDate(0, 0, -(weekday - 1)),
	}
}

// adminReleaseE2ENodeState 是一个格内某一小节的状态；available 由状态推出：
// 契约种子字典里只有 free 与 fully_free 可用，class 等占用状态一律不可用。
type adminReleaseE2ENodeState struct {
	Node  string
	State string
}

// adminReleaseE2EObsSeed 是一组观测种子：同一间房、同一周、同一天的一批小节。
type adminReleaseE2EObsSeed struct {
	ReleaseID string
	RoomID    string
	Week      int
	Weekday   int
	Nodes     []adminReleaseE2ENodeState
}

// adminReleaseE2EQueryNodes 是 /api/query 默认区间 01..11 覆盖的小节。
// 空教室查询要求区间内每一节都有观测且可用（HAVING count(*) = NodeCount），
// 所以「一间房整天可用」在这条用例里的含义就是这 11 个小节都写 free。
var adminReleaseE2EQueryNodes = []string{"01", "02", "03", "04", "05", "06", "07", "08", "09", "10", "11"}

// adminReleaseE2ENodesWithClass 返回 nodes 的逐节状态：classNodes 里的写 class，
// 其余写 free；不传 classNodes 表示整批 free。
//
// 占用必须是**整块**同状态：对比折叠后要检查块内一致性（min(state_key) 与
// max(state_key) 相等），同一大节里一半 free 一半 class 会被算进 inconsistent_blocks。
// 发布侧的明文展开也保证同一格内各小节同状态，夹具不该造出线上不会出现的数据。
func adminReleaseE2ENodesWithClass(nodes []string, classNodes ...string) []adminReleaseE2ENodeState {
	occupied := make(map[string]struct{}, len(classNodes))
	for _, node := range classNodes {
		occupied[node] = struct{}{}
	}

	states := make([]adminReleaseE2ENodeState, 0, len(nodes))
	for _, node := range nodes {
		state := "free"
		if _, ok := occupied[node]; ok {
			state = "class"
		}
		states = append(states, adminReleaseE2ENodeState{Node: node, State: state})
	}
	return states
}

// adminReleaseE2EObservationSeeds 是两版的全部观测。
//
// 三类差异与公开查询的可见性切换是同一份数据的两面：
//   - A101 在新版整天可用、在旧版第 2 大节（03..05 三个小节）被占 → 预热缓存时查得到
//     A101，回退后查不到（空教室要求区间内每一节都有观测且可用）；
//   - B201 恰好相反 → 回退后同一个请求只查得到 B201；
//   - A101 第 2 周、C103 第 4 周各自只出现在一版里，构成 added / removed。
//
// 两个「changed」格都落在 030405 这一块上：旧版 A101 整块 class、新版整块 free，
// B201 反过来。整块同状态，块内一致性成立（inconsistent_blocks=0）。
func adminReleaseE2EObservationSeeds(fx adminReleaseE2EFixture) []adminReleaseE2EObsSeed {
	return []adminReleaseE2EObsSeed{
		// 新版第 1 周：A101 整天可用；B201 第 2 大节上课（可用性口径下不出现）。
		{adminReleaseE2ECurrentID, adminReleaseE2ERoomA, 1, fx.Weekday,
			adminReleaseE2ENodesWithClass(adminReleaseE2EQueryNodes)},
		{adminReleaseE2ECurrentID, adminReleaseE2ERoomB, 1, fx.Weekday,
			adminReleaseE2ENodesWithClass(adminReleaseE2EQueryNodes, "03", "04", "05")},
		// 新版第 2 周：两间房只写 0102 一个格；A101 的这一格只在新版出现（added）。
		{adminReleaseE2ECurrentID, adminReleaseE2ERoomA, 2, 1,
			adminReleaseE2ENodesWithClass([]string{"01", "02"})},
		{adminReleaseE2ECurrentID, adminReleaseE2ERoomB, 2, 1,
			adminReleaseE2ENodesWithClass([]string{"01", "02"})},

		// 旧版第 1 周：与新版互换——A101 第 2 大节上课、B201 整天可用。
		{adminReleaseE2EPreviousID, adminReleaseE2ERoomA, 1, fx.Weekday,
			adminReleaseE2ENodesWithClass(adminReleaseE2EQueryNodes, "03", "04", "05")},
		{adminReleaseE2EPreviousID, adminReleaseE2ERoomB, 1, fx.Weekday,
			adminReleaseE2ENodesWithClass(adminReleaseE2EQueryNodes)},
		// 旧版第 2 周：与新版逐格相同（差异里是 same，不计数）。
		{adminReleaseE2EPreviousID, adminReleaseE2ERoomB, 2, 1,
			adminReleaseE2ENodesWithClass([]string{"01", "02"})},
		// 旧版第 4 周：只有 C103 一个格，只旧版有（removed）；第 3 周故意留空（缺周）。
		{adminReleaseE2EPreviousID, adminReleaseE2ERoomC, 4, 2,
			adminReleaseE2ENodesWithClass([]string{"06"}, "06")},
	}
}

// adminReleaseE2EMustTime 解析夹具里的 UTC 时刻，写错格式直接终止用例。
func adminReleaseE2EMustTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("解析夹具时刻 %q 失败: %v", value, err)
	}
	return parsed
}

// seedAdminReleaseE2EFixture 铺一套两版夹具：学期与逐周日历、四间房、两版版本行与
// 各自的归属快照、全部观测、两轮发布记录，以及旧版那一轮的一条房号级归属冲突。
//
// 字典 v1 与轴 v1 直接用 0001 基线的种子（dict_state 的 free/class、axis_node 的
// 12 小节 → 5 大节），不再插一遍——重复插入会撞主键。
func seedAdminReleaseE2EFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, fx adminReleaseE2EFixture) {
	t.Helper()

	mustExec(ctx, t, pool, "INSERT INTO term (term, total_weeks) VALUES ($1, $2)",
		fx.Term, adminReleaseE2ETotalWeeks)
	for week := 1; week <= adminReleaseE2ETotalWeeks; week++ {
		mustExec(ctx, t, pool, `
INSERT INTO term_week (term, week, monday, in_calendar)
VALUES ($1, $2, $3, true)`,
			fx.Term, week, fx.WeekOneMonday.AddDate(0, 0, 7*(week-1)))
	}

	// room 是全局最新归属，只给空教室/全天状态的 JOIN 提供展示名；
	// 真正参与「归属变化」比较的是下面两版的 release_room_building 快照。
	mustExec(ctx, t, pool, `
INSERT INTO room (room_id, name, name_raw, building_id, building_name)
VALUES ('A101', 'F101',  'F101',  'Q10', '综合教学楼'),
       ('B201', 'F201',  'F201',  'Q10', '综合教学楼'),
       ('C103', 'JA101', 'JA101', 'Q03', '老文史楼'),
       ('D104', 'SY104', 'SY104', 'Q11', '实验楼')`)

	mustExec(ctx, t, pool, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, has_attr_conflict, is_current)
VALUES ($1, $3, $4, 1, 1, false, true),
       ($2, $3, $5, 1, 1, true, false)`,
		adminReleaseE2ECurrentID, adminReleaseE2EPreviousID, fx.Term,
		adminReleaseE2EMustTime(t, adminReleaseE2ECurrentGenAt),
		adminReleaseE2EMustTime(t, adminReleaseE2EPreviousGenAt))

	mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, 'A101', 'Q10', '综合教学楼'),
       ($1, 'B201', 'Q10', '综合教学楼'),
       ($1, 'D104', 'Q11', '实验楼')`, adminReleaseE2ECurrentID)
	mustExec(ctx, t, pool, `
INSERT INTO release_room_building (release_id, room_id, building_id, building_name)
VALUES ($1, 'A101', 'Q03', '老文史楼'),
       ($1, 'B201', 'Q10', '综合教学楼'),
       ($1, 'C103', 'Q03', '老文史楼')`, adminReleaseE2EPreviousID)

	for _, seed := range adminReleaseE2EObservationSeeds(fx) {
		for _, node := range seed.Nodes {
			mustExec(ctx, t, pool, `
INSERT INTO observation (release_id, room_id, term, week, weekday, node, state_key, available)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				seed.ReleaseID, seed.RoomID, fx.Term, seed.Week, seed.Weekday,
				node.Node, node.State, node.State == "free")
		}
	}

	// 两轮发布：新版窗口 1..2、旧版窗口 1..4。旧版那一轮带一条归属冲突快照
	// （A101 在旧版归属 Q03，被 Q10 也报过一次），异常视图的冲突清单从这里来。
	mustExec(ctx, t, pool, `
INSERT INTO collect_run (started_at, finished_at, status, phase, term,
                         window_start, window_end, released_at, release_id,
                         unknown_cells, composite_cells, attr_conflict_count)
VALUES ($1, $2, 'success', 'finished', $3, 1, 2, $4, $5, 0, 0, 0),
       ($6, $7, 'success', 'finished', $3, 1, 4, $8, $9, 0, 0, 1)`,
		adminReleaseE2EMustTime(t, adminReleaseE2ECurrentStartedAt),
		adminReleaseE2EMustTime(t, adminReleaseE2ECurrentFinishedAt),
		fx.Term,
		adminReleaseE2EMustTime(t, adminReleaseE2ECurrentReleasedAt),
		adminReleaseE2ECurrentID,
		adminReleaseE2EMustTime(t, adminReleaseE2EPreviousStartedAt),
		adminReleaseE2EMustTime(t, adminReleaseE2EPreviousFinishedAt),
		adminReleaseE2EMustTime(t, adminReleaseE2EPreviousReleasedAt),
		adminReleaseE2EPreviousID)

	var previousRunID int64
	if err := pool.QueryRow(ctx,
		"SELECT run_id FROM collect_run WHERE release_id = $1",
		adminReleaseE2EPreviousID).Scan(&previousRunID); err != nil {
		t.Fatalf("读取旧版发布轮次失败: %v", err)
	}
	mustExec(ctx, t, pool, `
INSERT INTO collect_run_room (run_id, term, room_id, building_id, conflict, conflict_building_id)
VALUES ($1, $2, 'A101', 'Q03', true, 'Q10')`, previousRunID, fx.Term)
}

// adminReleaseE2ERunRow 是 collect_run 一行的快照，用来断言回退不动轮次历史。
// 可空列折成零值/空串再比较：直接比较 *time.Time 会受扫描时的 Location 影响，
// 同一库值在两次查询里得到的时间指针不保证 DeepEqual。
type adminReleaseE2ERunRow struct {
	RunID       int64
	Status      string
	ReleaseID   string
	ReleasedAt  string
	WindowStart int
	WindowEnd   int
	StartedAt   string
}

// loadAdminReleaseE2ERuns 读出 collect_run 全表快照，按 run_id 升序。
func loadAdminReleaseE2ERuns(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []adminReleaseE2ERunRow {
	t.Helper()

	rows, err := pool.Query(ctx, `
SELECT run_id, status, release_id, released_at, window_start, window_end, started_at
  FROM collect_run
 ORDER BY run_id`)
	if err != nil {
		t.Fatalf("读取轮次快照失败: %v", err)
	}
	defer rows.Close()

	snapshot := make([]adminReleaseE2ERunRow, 0)
	for rows.Next() {
		var (
			row         adminReleaseE2ERunRow
			releaseID   *string
			releasedAt  *time.Time
			windowStart *int
			windowEnd   *int
			startedAt   time.Time
		)
		if err := rows.Scan(&row.RunID, &row.Status, &releaseID, &releasedAt,
			&windowStart, &windowEnd, &startedAt); err != nil {
			t.Fatalf("扫描轮次快照失败: %v", err)
		}
		if releaseID != nil {
			row.ReleaseID = *releaseID
		}
		if releasedAt != nil {
			row.ReleasedAt = releasedAt.UTC().Format(time.RFC3339Nano)
		}
		row.WindowStart = -1
		if windowStart != nil {
			row.WindowStart = *windowStart
		}
		row.WindowEnd = -1
		if windowEnd != nil {
			row.WindowEnd = *windowEnd
		}
		row.StartedAt = startedAt.UTC().Format(time.RFC3339Nano)
		snapshot = append(snapshot, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历轮次快照失败: %v", err)
	}
	return snapshot
}

// adminReleaseE2EPublicServices 按 cmd/server/main.go 的映射接出公开查询服务：
// 空教室与全天状态**开着** 30 秒缓存，大屏服务自带固定 30 秒缓存。
//
// 闭包是照抄 main.go 的逐字段映射（store 的类型与 query 的消费侧类型形状不同），
// main.go 里这些闭包是函数内局部变量，没有可复用的装配函数（报告里作为
// 「装配层不可复用」列出，与 collect_pipeline_test.go 的 publicQueries 同一口径）。
func adminReleaseE2EPublicServices(pool *pgxpool.Pool) (*query.Service, *query.ScreenService) {
	weekLookup := func(ctx context.Context, date time.Time) (model.TermWeek, bool, error) {
		return store.TermWeekAt(ctx, pool, date)
	}

	availabilityLookup := func(ctx context.Context, filter query.AvailabilityFilter) (query.AvailabilityPage, error) {
		page, err := store.Availability(ctx, pool, store.AvailabilityFilter{
			Term:      filter.Term,
			Week:      filter.Week,
			Weekday:   filter.Weekday,
			StartNode: filter.StartNode,
			EndNode:   filter.EndNode,
			NodeCount: filter.NodeCount,
			Building:  filter.Building,
			Keyword:   filter.Keyword,
			Limit:     filter.Limit,
			Offset:    filter.Offset,
		})
		if err != nil {
			return query.AvailabilityPage{}, err
		}

		rooms := make([]query.AvailabilityRoom, 0, len(page.Rooms))
		for _, room := range page.Rooms {
			rooms = append(rooms, query.AvailabilityRoom{
				RoomID:       room.ID,
				Name:         room.Name,
				BuildingID:   room.BuildingID,
				BuildingName: room.BuildingName,
			})
		}
		return query.AvailabilityPage{
			Total:              page.Total,
			Rooms:              rooms,
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
		}, nil
	}

	dayLookup := func(ctx context.Context, filter query.DayFilter) (query.DayPage, error) {
		page, err := store.DayStatus(ctx, pool, store.DayFilter{
			Term:     filter.Term,
			Week:     filter.Week,
			Weekday:  filter.Weekday,
			RoomID:   filter.RoomID,
			Building: filter.Building,
		})
		if err != nil {
			return query.DayPage{}, err
		}

		nodes := make([]query.DayNode, 0, len(page.Nodes))
		for _, node := range page.Nodes {
			nodes = append(nodes, query.DayNode{
				Node:      node.Node,
				Block:     node.Block,
				StateKey:  node.StateKey,
				Label:     node.Label,
				Available: node.Available,
				HasData:   node.HasData,
			})
		}

		result := query.DayPage{
			HasRelease:         page.HasRelease,
			WeekHasObservation: page.WeekHasObservation,
			Nodes:              nodes,
		}
		if page.Room != nil {
			result.Room = &query.DayRoom{
				RoomID:       page.Room.ID,
				Name:         page.Room.Name,
				BuildingID:   page.Room.BuildingID,
				BuildingName: page.Room.BuildingName,
			}
		}
		return result, nil
	}

	queries := query.New(weekLookup, availabilityLookup, dayLookup,
		query.WithCache(query.DefaultCacheTTL))

	freshnessLookup := func(ctx context.Context, params query.FreshnessParams) (query.FreshnessFacts, error) {
		facts, err := store.Freshness(ctx, pool, store.FreshnessRange{
			Term:     params.Term,
			FromWeek: params.FromWeek,
			ToWeek:   params.ToWeek,
		})
		if err != nil {
			return query.FreshnessFacts{}, err
		}

		mapped := query.FreshnessFacts{
			Weeks:         make([]query.WeekFacts, 0, len(facts.Weeks)),
			LastRunStatus: facts.LastRunStatus,
		}
		for _, week := range facts.Weeks {
			mapped.Weeks = append(mapped.Weeks, query.WeekFacts{
				Week:          week.Week,
				Published:     week.Published,
				LastSuccessAt: week.LastSuccessAt,
			})
		}
		return mapped, nil
	}
	freshnessService := query.NewFreshness(freshnessLookup)

	statsReader := stats.NewReader(func(ctx context.Context, windows store.StatWindows, topN int) (store.StatFacts, error) {
		return store.ReadStatFacts(ctx, pool, windows, topN)
	})

	screenLookup := func(ctx context.Context) (query.ScreenFacts, error) {
		facts, err := store.Screen(ctx, pool)
		if err != nil {
			return query.ScreenFacts{}, err
		}

		mapped := query.ScreenFacts{
			Occupancy: make([]query.ScreenOccupancyCell, 0, len(facts.Occupancy)),
		}
		if facts.Release != nil {
			mapped.Release = &query.ScreenReleaseFacts{
				ReleaseID:   facts.Release.ID,
				GeneratedAt: facts.Release.GeneratedAt,
				Term:        facts.Release.Term,
				TotalWeeks:  facts.Release.TotalWeeks,
				Rooms:       facts.Release.Rooms,
				Buildings:   facts.Release.Buildings,
			}
		}
		mapped.Published = adminReleaseE2EScreenRun(facts.Published)
		mapped.LastRun = adminReleaseE2EScreenRun(facts.LastRun)
		for _, cell := range facts.Occupancy {
			mapped.Occupancy = append(mapped.Occupancy, query.ScreenOccupancyCell{
				BuildingID:   cell.BuildingID,
				BuildingName: cell.BuildingName,
				Weekday:      cell.Weekday,
				BlockOrdinal: cell.BlockOrdinal,
				Cells:        cell.Cells,
				Occupied:     cell.Occupied,
			})
		}
		return mapped, nil
	}

	return queries, query.NewScreen(screenLookup, freshnessService, statsReader, weekLookup)
}

// adminReleaseE2EScreenRun 映射一轮同步事实；nil 原样透传表示没有轮次。
// 与 main.go 的 screenRunFacts 同一份映射，错误码与错误原文两侧结构体里都没有。
func adminReleaseE2EScreenRun(run *store.ScreenRun) *query.ScreenRunFacts {
	if run == nil {
		return nil
	}
	return &query.ScreenRunFacts{
		Status:            run.Status,
		StartedAt:         run.StartedAt,
		FinishedAt:        run.FinishedAt,
		WindowStart:       run.WindowStart,
		WindowEnd:         run.WindowEnd,
		UnknownCells:      run.UnknownCells,
		CompositeCells:    run.CompositeCells,
		AttrConflictCount: run.AttrConflictCount,
	}
}

// e2eAssertNoStore 断言管理端响应带 Cache-Control: no-store（Requirement 8.5、CP11）。
func e2eAssertNoStore(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("%s 期望 Cache-Control: no-store，实际 %q", what, got)
	}
}

// e2eDataObject 取 data 里的对象字段。
func e2eDataObject(t *testing.T, data map[string]any, field string) map[string]any {
	t.Helper()

	value, ok := data[field].(map[string]any)
	if !ok {
		t.Fatalf("data.%s 期望对象，实际 %#v", field, data[field])
	}
	return value
}

// e2eDataArray 取 data 里的数组字段。
func e2eDataArray(t *testing.T, data map[string]any, field string) []any {
	t.Helper()

	value, ok := data[field].([]any)
	if !ok {
		t.Fatalf("data.%s 期望数组，实际 %#v", field, data[field])
	}
	return value
}

// e2eDataIntSlice 把 data 里的整数数组取成 []int（JSON 数字解码后是 float64）。
func e2eDataIntSlice(t *testing.T, data map[string]any, field string) []int {
	t.Helper()

	values := e2eDataArray(t, data, field)
	converted := make([]int, 0, len(values))
	for _, value := range values {
		number, ok := value.(float64)
		if !ok || number != float64(int(number)) {
			t.Fatalf("data.%s 里的元素期望整数，实际 %#v", field, value)
		}
		converted = append(converted, int(number))
	}
	return converted
}

// e2eAssertNullField 断言字段存在且为 null。
func e2eAssertNullField(t *testing.T, what, field string, data map[string]any) {
	t.Helper()

	value, ok := data[field]
	if !ok {
		t.Fatalf("%s 缺少键 %s", what, field)
	}
	if value != nil {
		t.Fatalf("%s 的 %s 期望 null，实际 %#v", what, field, value)
	}
}

// e2eAssertStringField 断言字符串字段等于期望值。
func e2eAssertStringField(t *testing.T, what, field, want string, data map[string]any) {
	t.Helper()

	if got := e2eString(t, data, field); got != want {
		t.Fatalf("%s 的 %s 期望 %q，实际 %q", what, field, want, got)
	}
}

// adminReleaseE2EAssertInt 断言整数字段等于期望值。
func adminReleaseE2EAssertInt(t *testing.T, what, field string, want int, data map[string]any) {
	t.Helper()

	if got := e2eDataInt(t, data, field); got != int64(want) {
		t.Fatalf("%s 的 %s 期望 %d，实际 %d", what, field, want, got)
	}
}

// adminReleaseE2EFindRelease 在版本列表里按 release_id 找一行；找不到直接失败。
func adminReleaseE2EFindRelease(t *testing.T, releases []any, releaseID string) map[string]any {
	t.Helper()

	for _, item := range releases {
		release, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("releases 元素不是对象: %#v", item)
		}
		if release["release_id"] == releaseID {
			return release
		}
	}
	t.Fatalf("版本列表里没有 release_id=%s", releaseID)
	return nil
}

// adminReleaseE2ECurrentIDs 返回列表里 is_current=true 的版本标识。
// CP1 要求它至多一条：返回切片是为了让「零条」和「两条」都能在断言里被看见。
func adminReleaseE2ECurrentIDs(t *testing.T, releases []any) []string {
	t.Helper()

	current := make([]string, 0, 1)
	for _, item := range releases {
		release, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("releases 元素不是对象: %#v", item)
		}
		if e2eDataBool(t, release, "is_current") {
			current = append(current, e2eString(t, release, "release_id"))
		}
	}
	return current
}

// adminReleaseE2EAssertAvailability 断言一次空教室查询只命中一间房，返回响应里的业务日期。
//
// 返回日期是为了跨步骤比对：缓存键含解析后的业务日期，回退前后两次查询必须是同一天，
// 「同一个键」才成立；跨零点跑用例时日期会变，这时断言失败的原因是检查点的前提被破坏，
// 而不是缓存没失效。
func adminReleaseE2EAssertAvailability(
	t *testing.T,
	what string,
	data map[string]any,
	fx adminReleaseE2EFixture,
	wantRoomID, wantName, wantBuildingID, wantBuildingName string,
) string {
	t.Helper()

	adminReleaseE2EAssertInt(t, what, "total", 1, data)
	adminReleaseE2EAssertInt(t, what, "week", 1, data)
	adminReleaseE2EAssertInt(t, what, "weekday", fx.Weekday, data)
	e2eAssertStringField(t, what, "term", fx.Term, data)
	date := e2eString(t, data, "date")
	// 命中房间时没有页顶提示。hint 是带 omitempty 的指针，nil 时整个键都不出现，
	// 所以这里判「没有非空值」而不是判「键存在且为 null」。
	if hint := data["hint"]; hint != nil {
		t.Fatalf("%s 命中房间时不该有 hint，实际 %#v", what, hint)
	}

	items := e2eDataArray(t, data, "items")
	if len(items) != 1 {
		t.Fatalf("%s 期望恰好 1 间房，实际 %d 间: %#v", what, len(items), items)
	}
	room, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("%s 的房间元素不是对象: %#v", what, items[0])
	}
	e2eAssertStringField(t, what, "room_id", wantRoomID, room)
	e2eAssertStringField(t, what, "name", wantName, room)
	e2eAssertStringField(t, what, "building_id", wantBuildingID, room)
	e2eAssertStringField(t, what, "building_name", wantBuildingName, room)
	return date
}

// adminReleaseE2EAssertDayNode 断言全天状态某一小节的状态与可用性。
func adminReleaseE2EAssertDayNode(t *testing.T, what, node, wantState string, wantAvailable bool, data map[string]any) {
	t.Helper()

	for _, item := range e2eDataArray(t, data, "nodes") {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s 的节点元素不是对象: %#v", what, item)
		}
		if entry["node"] != node {
			continue
		}
		e2eAssertStringField(t, what+" 小节 "+node, "state_key", wantState, entry)
		if got := e2eDataBool(t, entry, "available"); got != wantAvailable {
			t.Fatalf("%s 小节 %s 的 available 期望 %v，实际 %v", what, node, wantAvailable, got)
		}
		if !e2eDataBool(t, entry, "has_data") {
			t.Fatalf("%s 小节 %s 应有观测", what, node)
		}
		return
	}
	t.Fatalf("%s 的节点列表里找不到小节 %s", what, node)
}

// adminReleaseE2EAssertScreen 断言大屏载荷里的当前版本、采集窗口与异常计数。
//
// 三件都取自当前版本：只断 release_id 会被「缓存没清但 id 恰好变了」蒙混过去；
// 窗口与归属冲突计数来自该版的发布轮次，回退后它们一起换，才说明整份大屏是重新
// 从库里建的。
func adminReleaseE2EAssertScreen(
	t *testing.T,
	what string,
	data map[string]any,
	fx adminReleaseE2EFixture,
	wantReleaseID string,
	wantGeneratedAt string,
	wantWindow []int,
	wantAttrConflicts int,
) {
	t.Helper()

	release := e2eDataObject(t, data, "release")
	e2eAssertStringField(t, what, "release_id", wantReleaseID, release)
	e2eAssertStringField(t, what, "generated_at", wantGeneratedAt, release)

	scale := e2eDataObject(t, data, "scale")
	e2eAssertStringField(t, what, "term", fx.Term, scale)
	adminReleaseE2EAssertInt(t, what, "week", 1, scale)
	if got := e2eDataIntSlice(t, scale, "window"); !reflect.DeepEqual(got, wantWindow) {
		t.Fatalf("%s 的 scale.window 期望 %v，实际 %v", what, wantWindow, got)
	}

	anomalies := e2eDataObject(t, data, "anomalies")
	adminReleaseE2EAssertInt(t, what, "attr_conflicts", wantAttrConflicts, anomalies)
}

// TestAdminReleaseAgainstRealDB 是版本管理与回退的端到端检查点（tasklist 第 8 项）。
// 步骤顺序即 tasklist 的验收顺序，任一步失败都说明装配或链路有一段没接上。
func TestAdminReleaseAgainstRealDB(t *testing.T) {
	// 先按本检查点自己的口径判跳过，再复用 e2e 的干净库准备（重建 public schema + 迁移）。
	if os.Getenv("KJS_TEST_DSN") == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过版本管理与回退端到端检查点")
	}
	ctx, pool := openE2ETestDB(t, "版本管理与回退端到端检查点")
	fixture := newAdminReleaseE2EFixture(time.Now())

	// 日志只用于失败后排障：50000 的原文只进日志、不进响应，用例红的时候没有这份日志
	// 就只能靠复现去找是哪一条映射没接上。
	var logBuf e2eLogBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("进程日志（排障用）:\n%s", logBuf.String())
		}
	})

	// 装配照抄 cmd/server/main.go：公开查询服务（缓存打开）+ 大屏服务 + 版本仓储，
	// 回退后的缓存失效接真实的 queries.Invalidate / screenService.Invalidate。
	queries, screenService := adminReleaseE2EPublicServices(pool)
	releaseRepo := store.NewReleaseAdminRepo(pool)

	keys := secret.NewManager(filepath.Join(t.TempDir(), "secret.key"))
	if _, err := keys.Load(); err != nil {
		t.Fatalf("干净目录里 Load 密钥不该报错: %v", err)
	}

	handler := api.New(logger, pool, queries, queries, queries,
		api.WithAdmin(store.NewAdminRepo(pool), keys),
		api.WithAdminCookieSecure(false),
		api.WithScreen(screenService),
		api.WithReleaseAdmin(releaseRepo, func() {
			queries.Invalidate()
			screenService.Invalidate()
		}),
	).Handler()
	client := &adminE2EClient{t: t, handler: handler}

	// ---------- 1. init + login ----------
	initRec := client.do(http.MethodPost, "/api/admin/init", map[string]string{
		"password":         e2eAdminSentinelPassword,
		"password_confirm": e2eAdminSentinelPassword,
	}, nil, "")
	decodeE2ESuccess(t, initRec)
	e2eAssertNoStore(t, "POST /api/admin/init", initRec)

	loginRec := client.do(http.MethodPost, "/api/admin/session", map[string]string{
		"password": e2eAdminSentinelPassword,
	}, nil, "")
	loginData := decodeE2ESuccess(t, loginRec)
	e2eAssertNoStore(t, "POST /api/admin/session", loginRec)
	csrfToken := e2eString(t, loginData, "csrf_token")
	cookie := e2eSessionCookie(t, loginRec)

	// ---------- 2. 种两版（归属冲突 + 缺周） ----------
	seedAdminReleaseE2EFixture(ctx, t, pool, fixture)

	// ---------- 3. 列表：两行、生成时间倒序、current 标在新版 ----------
	listRec := client.do(http.MethodGet, "/api/admin/releases", nil, cookie, "")
	listData := decodeE2ESuccess(t, listRec)
	e2eAssertNoStore(t, "GET /api/admin/releases", listRec)

	adminReleaseE2EAssertInt(t, "版本列表", "total", 2, listData)
	releases := e2eDataArray(t, listData, "releases")
	if len(releases) != 2 {
		t.Fatalf("版本列表期望两行，实际 %d 行: %#v", len(releases), releases)
	}
	currentRow := adminReleaseE2EFindRelease(t, releases, adminReleaseE2ECurrentID)
	previousRow := adminReleaseE2EFindRelease(t, releases, adminReleaseE2EPreviousID)
	if releases[0].(map[string]any)["release_id"] != adminReleaseE2ECurrentID {
		t.Fatalf("列表首条期望生成时间更新的 %s，实际 %#v", adminReleaseE2ECurrentID, releases[0])
	}

	adminReleaseE2EAssertInt(t, "新版列表行", "week_count", 2, currentRow)
	adminReleaseE2EAssertInt(t, "新版列表行", "observation_rows", adminReleaseE2ECurrentObservationRows, currentRow)
	adminReleaseE2EAssertInt(t, "新版列表行", "rooms", 3, currentRow)
	adminReleaseE2EAssertInt(t, "新版列表行", "dict_version", 1, currentRow)
	adminReleaseE2EAssertInt(t, "新版列表行", "axis_version", 1, currentRow)
	e2eAssertStringField(t, "新版列表行", "term", fixture.Term, currentRow)
	e2eAssertStringField(t, "新版列表行", "generated_at", adminReleaseE2ECurrentGenAt, currentRow)
	e2eAssertStringField(t, "新版列表行", "released_at", adminReleaseE2ECurrentReleasedAt, currentRow)
	if !e2eDataBool(t, currentRow, "is_current") {
		t.Fatal("新版在回退前应是 is_current=true")
	}
	if e2eDataBool(t, currentRow, "has_attr_conflict") {
		t.Fatal("新版不该带归属冲突")
	}

	adminReleaseE2EAssertInt(t, "旧版列表行", "week_count", 3, previousRow)
	adminReleaseE2EAssertInt(t, "旧版列表行", "observation_rows", adminReleaseE2EPreviousObservationRows, previousRow)
	adminReleaseE2EAssertInt(t, "旧版列表行", "rooms", 3, previousRow)
	e2eAssertStringField(t, "旧版列表行", "generated_at", adminReleaseE2EPreviousGenAt, previousRow)
	e2eAssertStringField(t, "旧版列表行", "released_at", adminReleaseE2EPreviousReleasedAt, previousRow)
	if e2eDataBool(t, previousRow, "is_current") {
		t.Fatal("旧版在回退前应是 is_current=false")
	}
	if !e2eDataBool(t, previousRow, "has_attr_conflict") {
		t.Fatal("旧版应带归属冲突（夹具 has_attr_conflict=true）")
	}

	if current := adminReleaseE2ECurrentIDs(t, releases); !reflect.DeepEqual(current, []string{adminReleaseE2ECurrentID}) {
		t.Fatalf("回退前 current 期望恰好一条 %s，实际 %v", adminReleaseE2ECurrentID, current)
	}

	// ---------- 4. 详情：逐周计数与按楼分布与手工折叠一致 ----------
	newDetailRec := client.do(http.MethodGet, "/api/admin/releases/"+adminReleaseE2ECurrentID, nil, cookie, "")
	newDetail := decodeE2ESuccess(t, newDetailRec)
	e2eAssertNoStore(t, "GET /api/admin/releases/{new}", newDetailRec)

	newRelease := e2eDataObject(t, newDetail, "release")
	adminReleaseE2EAssertInt(t, "新版详情", "week_count", 2, newRelease)
	adminReleaseE2EAssertInt(t, "新版详情", "observation_rows", adminReleaseE2ECurrentObservationRows, newRelease)

	newStats := e2eDataObject(t, newDetail, "stats")
	adminReleaseE2EAssertInt(t, "新版详情", "observation_rows", adminReleaseE2ECurrentObservationRows, newStats)
	adminReleaseE2EAssertInt(t, "新版详情", "block_cells", adminReleaseE2ECurrentBlockCells, newStats)
	adminReleaseE2EAssertInt(t, "新版详情", "week_count", 2, newStats)
	adminReleaseE2EAssertInt(t, "新版详情", "rooms", 3, newStats)
	adminReleaseE2EAssertInt(t, "新版详情", "buildings", 2, newStats)

	newWeeks := e2eDataArray(t, newDetail, "week_counts")
	if len(newWeeks) != 2 {
		t.Fatalf("新版详情期望 2 个逐周行，实际 %d: %#v", len(newWeeks), newWeeks)
	}
	// 第 1 周：两间房各 11 小节 → 22 行、10 格；第 2 周：两间房各 2 小节 → 4 行、2 格。
	for index, want := range []struct {
		week  int
		rows  int
		cells int
	}{{1, 22, 10}, {2, 4, 2}} {
		entry, ok := newWeeks[index].(map[string]any)
		if !ok {
			t.Fatalf("新版 week_counts[%d] 不是对象: %#v", index, newWeeks[index])
		}
		adminReleaseE2EAssertInt(t, "新版逐周", "week", want.week, entry)
		adminReleaseE2EAssertInt(t, "新版逐周", "observation_rows", want.rows, entry)
		adminReleaseE2EAssertInt(t, "新版逐周", "block_cells", want.cells, entry)
	}

	newBuildings := e2eDataArray(t, newDetail, "building_counts")
	if len(newBuildings) != 2 {
		t.Fatalf("新版详情期望 2 个按楼行，实际 %d: %#v", len(newBuildings), newBuildings)
	}
	// 按 building_id 升序：Q10 两间（A101、B201），Q11 一间（D104）。
	for index, want := range []struct {
		buildingID   string
		buildingName string
		rooms        int
	}{
		{adminReleaseE2EBuildingQ10, adminReleaseE2EBuildingQ10Name, 2},
		{adminReleaseE2EBuildingQ11, adminReleaseE2EBuildingQ11Name, 1},
	} {
		entry, ok := newBuildings[index].(map[string]any)
		if !ok {
			t.Fatalf("新版 building_counts[%d] 不是对象: %#v", index, newBuildings[index])
		}
		e2eAssertStringField(t, "新版按楼", "building_id", want.buildingID, entry)
		e2eAssertStringField(t, "新版按楼", "building_name", want.buildingName, entry)
		adminReleaseE2EAssertInt(t, "新版按楼", "rooms", want.rooms, entry)
	}

	oldDetailRec := client.do(http.MethodGet, "/api/admin/releases/"+adminReleaseE2EPreviousID, nil, cookie, "")
	oldDetail := decodeE2ESuccess(t, oldDetailRec)
	e2eAssertNoStore(t, "GET /api/admin/releases/{old}", oldDetailRec)

	oldRelease := e2eDataObject(t, oldDetail, "release")
	adminReleaseE2EAssertInt(t, "旧版详情", "week_count", 3, oldRelease)
	adminReleaseE2EAssertInt(t, "旧版详情", "observation_rows", adminReleaseE2EPreviousObservationRows, oldRelease)
	if e2eDataBool(t, oldRelease, "is_current") {
		t.Fatal("旧版详情在回退前应是 is_current=false")
	}

	oldStats := e2eDataObject(t, oldDetail, "stats")
	adminReleaseE2EAssertInt(t, "旧版详情", "observation_rows", adminReleaseE2EPreviousObservationRows, oldStats)
	adminReleaseE2EAssertInt(t, "旧版详情", "block_cells", adminReleaseE2EPreviousBlockCells, oldStats)
	adminReleaseE2EAssertInt(t, "旧版详情", "week_count", 3, oldStats)
	adminReleaseE2EAssertInt(t, "旧版详情", "rooms", 3, oldStats)
	adminReleaseE2EAssertInt(t, "旧版详情", "buildings", 2, oldStats)

	oldWeeks := e2eDataArray(t, oldDetail, "week_counts")
	if len(oldWeeks) != 3 {
		t.Fatalf("旧版详情期望 3 个逐周行，实际 %d: %#v", len(oldWeeks), oldWeeks)
	}
	// 第 1 周 22 行/10 格、第 2 周 2 行/1 格、第 4 周 1 行/1 格（第 3 周缺）。
	for index, want := range []struct {
		week  int
		rows  int
		cells int
	}{{1, 22, 10}, {2, 2, 1}, {4, 1, 1}} {
		entry, ok := oldWeeks[index].(map[string]any)
		if !ok {
			t.Fatalf("旧版 week_counts[%d] 不是对象: %#v", index, oldWeeks[index])
		}
		adminReleaseE2EAssertInt(t, "旧版逐周", "week", want.week, entry)
		adminReleaseE2EAssertInt(t, "旧版逐周", "observation_rows", want.rows, entry)
		adminReleaseE2EAssertInt(t, "旧版逐周", "block_cells", want.cells, entry)
	}

	oldBuildings := e2eDataArray(t, oldDetail, "building_counts")
	// 按 building_id 升序：Q03 两间（A101、C103），Q10 一间（B201）。
	for index, want := range []struct {
		buildingID   string
		buildingName string
		rooms        int
	}{
		{adminReleaseE2EBuildingQ03, adminReleaseE2EBuildingQ03Name, 2},
		{adminReleaseE2EBuildingQ10, adminReleaseE2EBuildingQ10Name, 1},
	} {
		entry, ok := oldBuildings[index].(map[string]any)
		if !ok {
			t.Fatalf("旧版 building_counts[%d] 不是对象: %#v", index, oldBuildings[index])
		}
		e2eAssertStringField(t, "旧版按楼", "building_id", want.buildingID, entry)
		adminReleaseE2EAssertInt(t, "旧版按楼", "rooms", want.rooms, entry)
	}

	// 不存在的版本：404 + 40401，响应同样带 no-store（错误响应也不许被缓存）。
	missingRec := client.do(http.MethodGet, "/api/admin/releases/20200101T000000Z", nil, cookie, "")
	decodeE2EFailure(t, missingRec, http.StatusNotFound, 40401)
	e2eAssertNoStore(t, "GET /api/admin/releases/{missing}", missingRec)

	// ---------- 5. 对比：三类差异 + 归属两小节 ----------
	diffBase := fmt.Sprintf("/api/admin/releases/diff?from=%s&to=%s",
		adminReleaseE2EPreviousID, adminReleaseE2ECurrentID)

	diffRec := client.do(http.MethodGet, diffBase+"&limit=2&offset=0", nil, cookie, "")
	diffData := decodeE2ESuccess(t, diffRec)
	e2eAssertNoStore(t, "GET /api/admin/releases/diff", diffRec)

	summary := e2eDataObject(t, diffData, "summary")
	cells := e2eDataObject(t, summary, "cells")
	adminReleaseE2EAssertInt(t, "对比汇总", "added", adminReleaseE2ECellsAdded, cells)
	adminReleaseE2EAssertInt(t, "对比汇总", "removed", adminReleaseE2ECellsRemoved, cells)
	adminReleaseE2EAssertInt(t, "对比汇总", "changed", adminReleaseE2ECellsChanged, cells)
	adminReleaseE2EAssertInt(t, "对比汇总", "total",
		adminReleaseE2ECellsAdded+adminReleaseE2ECellsRemoved+adminReleaseE2ECellsChanged, cells)

	rooms := e2eDataObject(t, summary, "rooms")
	adminReleaseE2EAssertInt(t, "对比汇总", "added", adminReleaseE2ERoomsAdded, rooms)
	adminReleaseE2EAssertInt(t, "对比汇总", "removed", adminReleaseE2ERoomsRemoved, rooms)
	adminReleaseE2EAssertInt(t, "对比汇总", "shared", adminReleaseE2ERoomsShared, rooms)
	adminReleaseE2EAssertInt(t, "对比汇总", "building_changed", adminReleaseE2ERoomsMoved, rooms)

	// 未知/复合两侧都是 0：夹具里没有 unknown / composite 格。
	for _, field := range []string{"unknown", "composite"} {
		delta := e2eDataObject(t, summary, field)
		adminReleaseE2EAssertInt(t, "对比汇总 "+field, "from", 0, delta)
		adminReleaseE2EAssertInt(t, "对比汇总 "+field, "to", 0, delta)
		adminReleaseE2EAssertInt(t, "对比汇总 "+field, "delta", 0, delta)
	}
	adminReleaseE2EAssertInt(t, "对比汇总", "inconsistent_blocks", 0, summary)
	if !e2eDataBool(t, summary, "same_term") {
		t.Fatal("两版同学期，same_term 应为 true")
	}

	fromSide := e2eDataObject(t, diffData, "from")
	toSide := e2eDataObject(t, diffData, "to")
	e2eAssertStringField(t, "对比 from", "release_id", adminReleaseE2EPreviousID, fromSide)
	e2eAssertStringField(t, "对比 to", "release_id", adminReleaseE2ECurrentID, toSide)
	if e2eDataBool(t, fromSide, "is_current") || !e2eDataBool(t, toSide, "is_current") {
		t.Fatalf("回退前对比两侧的 is_current 期望 from=false / to=true，实际 from=%v / to=%v",
			fromSide["is_current"], toSide["is_current"])
	}

	// 分页回显与 total 同源：total 来自汇总，不是本页条数（CP6）。
	adminReleaseE2EAssertInt(t, "对比分页", "total", 4, diffData)
	adminReleaseE2EAssertInt(t, "对比分页", "limit", 2, diffData)
	adminReleaseE2EAssertInt(t, "对比分页", "offset", 0, diffData)
	e2eAssertStringField(t, "对比分页", "section", "cells", diffData)
	e2eAssertStringField(t, "对比分页", "kind", "all", diffData)

	// 第一页按 (room_id, week, weekday, block) 升序：A101 第 1 周的 changed 在前，
	// A101 第 2 周的 added 在后。
	page1 := e2eDataArray(t, diffData, "items")
	if len(page1) != 2 {
		t.Fatalf("对比第一页期望 2 行，实际 %d 行: %#v", len(page1), page1)
	}
	firstRow := page1[0].(map[string]any)
	e2eAssertStringField(t, "对比首页", "kind", "changed", firstRow)
	e2eAssertStringField(t, "对比首页", "room_id", adminReleaseE2ERoomA, firstRow)
	e2eAssertStringField(t, "对比首页", "room_name", "F101", firstRow)
	adminReleaseE2EAssertInt(t, "对比首页", "week", 1, firstRow)
	adminReleaseE2EAssertInt(t, "对比首页", "weekday", fixture.Weekday, firstRow)
	e2eAssertStringField(t, "对比首页", "block", "030405", firstRow)
	e2eAssertStringField(t, "对比首页", "from_state", "class", firstRow)
	e2eAssertStringField(t, "对比首页", "to_state", "free", firstRow)
	e2eAssertStringField(t, "对比首页", "from_building", adminReleaseE2EBuildingQ03, firstRow)
	e2eAssertStringField(t, "对比首页", "to_building", adminReleaseE2EBuildingQ10, firstRow)

	secondRow := page1[1].(map[string]any)
	e2eAssertStringField(t, "对比首页", "kind", "added", secondRow)
	e2eAssertStringField(t, "对比首页", "room_id", adminReleaseE2ERoomA, secondRow)
	adminReleaseE2EAssertInt(t, "对比首页", "week", 2, secondRow)
	adminReleaseE2EAssertInt(t, "对比首页", "weekday", 1, secondRow)
	e2eAssertStringField(t, "对比首页", "block", "0102", secondRow)
	e2eAssertNullField(t, "只在新版出现的格", "from_state", secondRow)
	e2eAssertStringField(t, "只在新版出现的格", "to_state", "free", secondRow)

	page2Rec := client.do(http.MethodGet, diffBase+"&limit=2&offset=2", nil, cookie, "")
	page2Data := decodeE2ESuccess(t, page2Rec)
	e2eAssertNoStore(t, "GET /api/admin/releases/diff (offset=2)", page2Rec)
	adminReleaseE2EAssertInt(t, "对比第二页", "total", 4, page2Data)
	page2 := e2eDataArray(t, page2Data, "items")
	if len(page2) != 2 {
		t.Fatalf("对比第二页期望 2 行，实际 %d 行: %#v", len(page2), page2)
	}
	// 第二页：B201 第 1 周的 changed，C103 第 4 周的 removed。
	thirdRow := page2[0].(map[string]any)
	e2eAssertStringField(t, "对比第二页", "kind", "changed", thirdRow)
	e2eAssertStringField(t, "对比第二页", "room_id", adminReleaseE2ERoomB, thirdRow)
	e2eAssertStringField(t, "对比第二页", "from_state", "free", thirdRow)
	e2eAssertStringField(t, "对比第二页", "to_state", "class", thirdRow)

	fourthRow := page2[1].(map[string]any)
	e2eAssertStringField(t, "对比第二页", "kind", "removed", fourthRow)
	e2eAssertStringField(t, "对比第二页", "room_id", adminReleaseE2ERoomC, fourthRow)
	e2eAssertStringField(t, "对比第二页", "room_name", "JA101", fourthRow)
	adminReleaseE2EAssertInt(t, "对比第二页", "week", 4, fourthRow)
	adminReleaseE2EAssertInt(t, "对比第二页", "weekday", 2, fourthRow)
	e2eAssertStringField(t, "对比第二页", "from_state", "class", fourthRow)
	e2eAssertNullField(t, "只在旧版出现的格", "to_state", fourthRow)
	e2eAssertNullField(t, "只在旧版出现的格", "to_building", fourthRow)

	// 同一请求重复执行返回同一页（CP6）：排序键在差异集合内唯一。
	repeatData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&limit=2&offset=2", nil, cookie, ""))
	if !reflect.DeepEqual(decodePageIDs(t, page2Data), decodePageIDs(t, repeatData)) {
		t.Fatalf("同一分页请求两次返回了不同的行: %v vs %v",
			decodePageIDs(t, page2Data), decodePageIDs(t, repeatData))
	}

	// kind 过滤：added 只有 1 条且 from_state 为 null；changed 有 2 条。
	addedData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&kind=added", nil, cookie, ""))
	e2eAssertStringField(t, "kind=added", "kind", "added", addedData)
	adminReleaseE2EAssertInt(t, "kind=added", "total", adminReleaseE2ECellsAdded, addedData)
	addedItems := e2eDataArray(t, addedData, "items")
	if len(addedItems) != adminReleaseE2ECellsAdded {
		t.Fatalf("kind=added 期望 %d 行，实际 %d 行", adminReleaseE2ECellsAdded, len(addedItems))
	}

	changedData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&kind=changed", nil, cookie, ""))
	adminReleaseE2EAssertInt(t, "kind=changed", "total", adminReleaseE2ECellsChanged, changedData)

	// offset 翻过末页：items 为 []、total 不变（CP6）。
	beyondData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&offset=99", nil, cookie, ""))
	adminReleaseE2EAssertInt(t, "offset 越界", "total", 4, beyondData)
	if items := e2eDataArray(t, beyondData, "items"); len(items) != 0 {
		t.Fatalf("offset 越界时期望空 items，实际 %#v", items)
	}

	// section=rooms：房号级增删（按 room_id 升序：C103 removed、D104 added）。
	roomsData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&section=rooms", nil, cookie, ""))
	e2eAssertStringField(t, "section=rooms", "section", "rooms", roomsData)
	adminReleaseE2EAssertInt(t, "section=rooms", "total", adminReleaseE2ERoomsAdded+adminReleaseE2ERoomsRemoved, roomsData)
	roomsItems := e2eDataArray(t, roomsData, "items")
	if len(roomsItems) != 2 {
		t.Fatalf("section=rooms 期望 2 行，实际 %d 行: %#v", len(roomsItems), roomsItems)
	}
	removedRoom := roomsItems[0].(map[string]any)
	e2eAssertStringField(t, "section=rooms", "kind", "removed", removedRoom)
	e2eAssertStringField(t, "section=rooms", "room_id", adminReleaseE2ERoomC, removedRoom)
	e2eAssertStringField(t, "section=rooms", "from_building", adminReleaseE2EBuildingQ03, removedRoom)
	e2eAssertStringField(t, "section=rooms", "from_building_name", adminReleaseE2EBuildingQ03Name, removedRoom)
	e2eAssertNullField(t, "消失的房间", "to_building", removedRoom)
	e2eAssertNullField(t, "消失的房间", "to_building_name", removedRoom)

	addedRoom := roomsItems[1].(map[string]any)
	e2eAssertStringField(t, "section=rooms", "kind", "added", addedRoom)
	e2eAssertStringField(t, "section=rooms", "room_id", adminReleaseE2ERoomD, addedRoom)
	e2eAssertNullField(t, "新增的房间", "from_building", addedRoom)
	e2eAssertStringField(t, "section=rooms", "to_building", adminReleaseE2EBuildingQ11, addedRoom)
	e2eAssertStringField(t, "section=rooms", "to_building_name", adminReleaseE2EBuildingQ11Name, addedRoom)

	// section=buildings：归属变化（A101 从 Q03 老文史楼换到 Q10 综合教学楼）。
	buildingsData := decodeE2ESuccess(t, client.do(http.MethodGet, diffBase+"&section=buildings", nil, cookie, ""))
	e2eAssertStringField(t, "section=buildings", "section", "buildings", buildingsData)
	adminReleaseE2EAssertInt(t, "section=buildings", "total", adminReleaseE2ERoomsMoved, buildingsData)
	buildingsItems := e2eDataArray(t, buildingsData, "items")
	if len(buildingsItems) != adminReleaseE2ERoomsMoved {
		t.Fatalf("section=buildings 期望 %d 行，实际 %d 行", adminReleaseE2ERoomsMoved, len(buildingsItems))
	}
	movedRoom := buildingsItems[0].(map[string]any)
	e2eAssertStringField(t, "section=buildings", "kind", "changed", movedRoom)
	e2eAssertStringField(t, "section=buildings", "room_id", adminReleaseE2ERoomA, movedRoom)
	e2eAssertStringField(t, "section=buildings", "from_building", adminReleaseE2EBuildingQ03, movedRoom)
	e2eAssertStringField(t, "section=buildings", "from_building_name", adminReleaseE2EBuildingQ03Name, movedRoom)
	e2eAssertStringField(t, "section=buildings", "to_building", adminReleaseE2EBuildingQ10, movedRoom)
	e2eAssertStringField(t, "section=buildings", "to_building_name", adminReleaseE2EBuildingQ10Name, movedRoom)

	// ---------- 6. 异常视图：归属冲突与缺周（种子的另一半用途） ----------
	anomaliesRec := client.do(http.MethodGet, "/api/admin/anomalies", nil, cookie, "")
	anomaliesData := decodeE2ESuccess(t, anomaliesRec)
	e2eAssertNoStore(t, "GET /api/admin/anomalies", anomaliesRec)
	anomalies := e2eDataArray(t, anomaliesData, "releases")
	if len(anomalies) != 2 {
		t.Fatalf("异常视图期望两行，实际 %d 行: %#v", len(anomalies), anomalies)
	}
	newAnomaly := anomalies[0].(map[string]any)
	oldAnomaly := anomalies[1].(map[string]any)
	e2eAssertStringField(t, "异常视图", "release_id", adminReleaseE2ECurrentID, newAnomaly)
	e2eAssertStringField(t, "异常视图", "release_id", adminReleaseE2EPreviousID, oldAnomaly)

	// 新版：观测周 1、2 连续，没有缺口；窗口取自它那一轮（1..2）。
	if weeks := e2eDataIntSlice(t, newAnomaly, "observed_weeks"); !reflect.DeepEqual(weeks, []int{1, 2}) {
		t.Fatalf("新版 observed_weeks 期望 [1 2]，实际 %v", weeks)
	}
	if gaps := e2eDataIntSlice(t, newAnomaly, "week_gaps"); len(gaps) != 0 {
		t.Fatalf("新版 week_gaps 期望空，实际 %v", gaps)
	}
	newWindow := e2eDataObject(t, newAnomaly, "window")
	adminReleaseE2EAssertInt(t, "新版窗口", "start", 1, newWindow)
	adminReleaseE2EAssertInt(t, "新版窗口", "end", 2, newWindow)

	// 旧版：观测周 1、2、4，缺第 3 周；冲突清单来自它那一轮的 collect_run_room。
	if weeks := e2eDataIntSlice(t, oldAnomaly, "observed_weeks"); !reflect.DeepEqual(weeks, []int{1, 2, 4}) {
		t.Fatalf("旧版 observed_weeks 期望 [1 2 4]，实际 %v", weeks)
	}
	if gaps := e2eDataIntSlice(t, oldAnomaly, "week_gaps"); !reflect.DeepEqual(gaps, []int{3}) {
		t.Fatalf("旧版 week_gaps 期望 [3]（缺第 3 周），实际 %v", gaps)
	}
	if !e2eDataBool(t, oldAnomaly, "has_attr_conflict") {
		t.Fatal("旧版异常行应带 has_attr_conflict=true")
	}
	oldWindow := e2eDataObject(t, oldAnomaly, "window")
	adminReleaseE2EAssertInt(t, "旧版窗口", "start", 1, oldWindow)
	adminReleaseE2EAssertInt(t, "旧版窗口", "end", 4, oldWindow)
	conflicts := e2eDataArray(t, oldAnomaly, "conflict_rooms")
	if len(conflicts) != 1 {
		t.Fatalf("旧版冲突清单期望 1 条，实际 %#v", conflicts)
	}
	conflict := conflicts[0].(map[string]any)
	e2eAssertStringField(t, "旧版冲突清单", "room_id", adminReleaseE2ERoomA, conflict)
	e2eAssertStringField(t, "旧版冲突清单", "building_id", adminReleaseE2EBuildingQ03, conflict)
	e2eAssertStringField(t, "旧版冲突清单", "conflict_building_id", adminReleaseE2EBuildingQ10, conflict)
	if e2eDataBool(t, oldAnomaly, "conflict_rooms_truncated") {
		t.Fatal("一条冲突不该标成截断")
	}

	// ---------- 7. 预热公开查询缓存（回退前的 current = 新版） ----------
	//
	// 同参数连查两次：第一次回库并写缓存，第二次走命中路径。两次都要对——命中路径
	// 返回的是缓存里的副本，形状与实时结果一致才算「缓存可用」；后面回退后的第三次
	// 查询与这两次同键，缓存没被清就会继续读到这一份。
	availabilityPath := "/api/query?view=availability&date_offset=0&start_node=01&end_node=11&limit=50"
	warmRec := client.do(http.MethodGet, availabilityPath, nil, nil, "")
	warmData := decodeE2ESuccess(t, warmRec)
	warmDate := adminReleaseE2EAssertAvailability(t, "预热查询（新版）", warmData, fixture,
		adminReleaseE2ERoomA, "F101", adminReleaseE2EBuildingQ10, adminReleaseE2EBuildingQ10Name)

	hitData := decodeE2ESuccess(t, client.do(http.MethodGet, availabilityPath, nil, nil, ""))
	if hitDate := adminReleaseE2EAssertAvailability(t, "缓存命中路径", hitData, fixture,
		adminReleaseE2ERoomA, "F101", adminReleaseE2EBuildingQ10, adminReleaseE2EBuildingQ10Name); hitDate != warmDate {
		t.Fatalf("同一参数两次查询的业务日期不一致: %q vs %q", warmDate, hitDate)
	}

	// 全天状态也预热一张表：Service.Invalidate 清的是空教室与全天状态两张表，
	// 只验一张时另一张漏清不会被发现。新版里 A101 第 2 大节整块是 free（05 在内）。
	dayPath := "/api/query?view=day&date_offset=0&room_id=A101"
	dayWarmData := decodeE2ESuccess(t, client.do(http.MethodGet, dayPath, nil, nil, ""))
	e2eAssertStringField(t, "预热全天状态（新版）", "building_id", adminReleaseE2EBuildingQ10, dayWarmData)
	adminReleaseE2EAssertDayNode(t, "预热全天状态（新版）", "05", "free", true, dayWarmData)

	// 大屏同样预热：30 秒缓存固定打开（NewScreen 自带），不清它就要等 TTL。
	screenPath := "/api/screen?peak_days=7"
	screenWarmData := decodeE2ESuccess(t, client.do(http.MethodGet, screenPath, nil, nil, ""))
	adminReleaseE2EAssertScreen(t, "预热大屏（新版）", screenWarmData, fixture,
		adminReleaseE2ECurrentID, adminReleaseE2ECurrentGenAt, []int{1, 2}, 0)

	// 回退前的库快照：回退只动 release.is_current 的两个布尔，其余行数一行不变（CP9）。
	assertCount(ctx, t, pool, 2, "回退前的版本行数", "SELECT count(*) FROM release")
	assertCount(ctx, t, pool, 51, "回退前的观测行数", "SELECT count(*) FROM observation")
	runsBefore := loadAdminReleaseE2ERuns(ctx, t, pool)
	if len(runsBefore) != 2 {
		t.Fatalf("collect_run 期望两行（一版一轮），实际 %d 行: %+v", len(runsBefore), runsBefore)
	}

	// ---------- 8. 回退：新版 → 旧版 ----------
	rollbackRec := client.do(http.MethodPost,
		"/api/admin/releases/"+adminReleaseE2EPreviousID+"/rollback", nil, cookie, csrfToken)
	rollbackData := decodeE2ESuccess(t, rollbackRec)
	e2eAssertNoStore(t, "POST /api/admin/releases/{old}/rollback", rollbackRec)
	e2eAssertStringField(t, "回退响应", "rolled_back_from", adminReleaseE2ECurrentID, rollbackData)
	e2eAssertStringField(t, "回退响应", "current_release_id", adminReleaseE2EPreviousID, rollbackData)
	e2eAssertStringField(t, "回退响应", "term", fixture.Term, rollbackData)

	// ---------- 9. 回退后立即查：同一进程、同参数、不等 30 秒 ----------
	//
	// 这里不 sleep、不推进时钟：回退响应返回后立刻用同一组参数再查一次。命中旧缓存
	// 时读到的会是第 7 步的 A101/Q10；读到 B201/Q03 才说明 invalidate 真的在写响应
	// 之前清掉了缓存（CP10）。elapsed 只作日志；把它与 TTL 比较一次是为了让「没等」
	// 这件事在断言里也是可见的。
	queryStartedAt := time.Now()
	afterRollbackData := decodeE2ESuccess(t, client.do(http.MethodGet, availabilityPath, nil, nil, ""))
	queryElapsed := time.Since(queryStartedAt)
	afterRollbackDate := adminReleaseE2EAssertAvailability(t, "回退后的同参数查询", afterRollbackData, fixture,
		adminReleaseE2ERoomB, "F201", adminReleaseE2EBuildingQ10, adminReleaseE2EBuildingQ10Name)
	if afterRollbackDate != warmDate {
		t.Fatalf("回退前后两次查询的业务日期不一致（%q vs %q）：缓存键不同，本段无法证明缓存失效",
			warmDate, afterRollbackDate)
	}
	if queryElapsed >= query.DefaultCacheTTL {
		t.Fatalf("回退后的查询耗时 %s 已超过缓存 TTL %s，本段不再能证明缓存被主动清空",
			queryElapsed, query.DefaultCacheTTL)
	}
	t.Logf("回退后同参数查询立即读到新 current：耗时 %s（未等待 %s TTL）", queryElapsed, query.DefaultCacheTTL)

	// 全天状态的另一张表：同一间房，第 5 节从 free 变成 class，归属从 Q10 换到旧版的 Q03。
	dayAfterData := decodeE2ESuccess(t, client.do(http.MethodGet, dayPath, nil, nil, ""))
	e2eAssertStringField(t, "回退后全天状态", "building_id", adminReleaseE2EBuildingQ03, dayAfterData)
	e2eAssertStringField(t, "回退后全天状态", "building_name", adminReleaseE2EBuildingQ03Name, dayAfterData)
	adminReleaseE2EAssertDayNode(t, "回退后全天状态", "05", "class", false, dayAfterData)

	// 大屏：版本标识、生成时间、采集窗口与归属冲突计数一起换成旧版的。
	screenAfterData := decodeE2ESuccess(t, client.do(http.MethodGet, screenPath, nil, nil, ""))
	adminReleaseE2EAssertScreen(t, "回退后大屏", screenAfterData, fixture,
		adminReleaseE2EPreviousID, adminReleaseE2EPreviousGenAt, []int{1, 4}, 1)

	// ---------- 10. 列表：仍两行，current 恰好一条且指向回退目标 ----------
	afterListRec := client.do(http.MethodGet, "/api/admin/releases", nil, cookie, "")
	afterList := decodeE2ESuccess(t, afterListRec)
	e2eAssertNoStore(t, "回退后的版本列表", afterListRec)

	adminReleaseE2EAssertInt(t, "回退后的列表", "total", 2, afterList)
	afterReleases := e2eDataArray(t, afterList, "releases")
	if len(afterReleases) != 2 {
		t.Fatalf("回退不该增删版本：期望仍两行，实际 %d 行", len(afterReleases))
	}
	// 排序不变：首条仍是生成时间更新的新版；变的只有 is_current 落在哪一行（CP1）。
	if afterReleases[0].(map[string]any)["release_id"] != adminReleaseE2ECurrentID {
		t.Fatalf("回退不该改变排序：首条仍应是 %s，实际 %#v",
			adminReleaseE2ECurrentID, afterReleases[0])
	}
	if current := adminReleaseE2ECurrentIDs(t, afterReleases); !reflect.DeepEqual(current, []string{adminReleaseE2EPreviousID}) {
		t.Fatalf("回退后 current 期望恰好一条 %s，实际 %v", adminReleaseE2EPreviousID, current)
	}

	// ---------- 11. 回退不重建、不删数据：行数与 collect_run 都不变 ----------
	assertCount(ctx, t, pool, 2, "回退后的版本行数", "SELECT count(*) FROM release")
	assertCount(ctx, t, pool, 51, "回退后的观测行数", "SELECT count(*) FROM observation")
	assertCount(ctx, t, pool, 6, "回退后的归属快照行数", "SELECT count(*) FROM release_room_building")
	assertCount(ctx, t, pool, 2, "回退后的轮次行数", "SELECT count(*) FROM collect_run")

	runsAfter := loadAdminReleaseE2ERuns(ctx, t, pool)
	if !reflect.DeepEqual(runsBefore, runsAfter) {
		t.Fatalf("回退不该改 collect_run：前 %+v，后 %+v", runsBefore, runsAfter)
	}
}

// decodePageIDs 把对比分页的 items 折成「类别|房号|周|星期|大节」序列，用于比较两次请求
// 是否返回同一页。换成稳定字符串而不是直接 DeepEqual 两份 items：失败信息里能直接
// 看出是哪一行不同，而不是一坨 map。
func decodePageIDs(t *testing.T, data map[string]any) []string {
	t.Helper()

	items := e2eDataArray(t, data, "items")
	ids := make([]string, 0, len(items))
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("对比 items 元素不是对象: %#v", item)
		}
		ids = append(ids, fmt.Sprintf("%v|%v|%v|%v|%v",
			row["kind"], row["room_id"], row["week"], row["weekday"], row["block"]))
	}
	return ids
}
