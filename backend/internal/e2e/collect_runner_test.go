package e2e

// 本文件是 collect-runner 的端到端检查点（tasklist 第 11 项）：在真实 PostgreSQL 上
// 用生产装配 + 假上游，另注入假时钟，跑通 collector.Start → 202 → 轮次终态 → 公开查询可见。
//
// 三个检查点：
//  1. 正常轮：POST /api/admin/sync 得 202 → 后台一轮落成 success → 发布 → 公开查询可见；
//  2. 归属冲突轮：两栋楼报同一个房号 → collect_run_room 只含赢家的归属、冲突计数与版本
//     标记非零、明细任务一条都没被丢弃；
//  3. 续跑轮：本轮周矩阵任务已全部 done（上一轮执行完成）、内存为空 → hydrate 把全部
//     8 个「楼 × 周」补抓进内存后照常发布，观测覆盖窗口内全部 4 周；hydrate 只累计内存
//     候选，collect_run_week 与 sync_task 的既有行一行都不改写。
//
// 与 collect_pipeline_test.go 的区别：那一条从 pipeline.StartRun 起跑，窗口、候选累计与
// 不可用格都由测试 shim 代劳；本文件从 collector.Start 起跑，登录、学期、日历、窗口、
// 快照、两段 Plan/Run 与发布全部是生产代码，只有上游 HTTP 客户端是假的。
//
// 装配口径照抄 cmd/server/main.go：settings.New(store.NewSettingsRepo(pool), keys, nil)、
// store 的八个仓储 → collector.New → runner.New（Converge 用 pipeline.ConvergeStaleRuns
// 包装）→ api.WithAdmin / WithSync。密钥管理器指向 t.TempDir() 下的真实文件，由
// POST /api/admin/init 生成并装载。
//
// 与 main.go 的差别只在外部依赖：上游 HTTP 客户端是假的（collectRunnerUpstream），
// 时钟也不传 nil——这里注入一个从 2026-11-04 10:00（第 9 周周三）起每次调用 +1 秒的
// 假时钟，每一轮再前进 1 小时。这样学期日历锚点（2026-09-07）与窗口（9..12）可复现，
// 三轮的 started_at / release_id 也不会互相撞车（release_id 由 generated_at 派生）。
//
// 与 collect_pipeline_test.go / admin_settings_sync_test.go 一样只在设置 KJS_TEST_DSN
// 时运行，并且会重建 public schema；带 DSN 跑全量测试必须固定 `-p 1`（原因见
// collect_pipeline_test.go 的包注释）。

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/collector"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/query"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

const (
	// collectRunnerAdminPassword 是 init 设置的管理员口令（超过 12 码点）。
	collectRunnerAdminPassword = "e2e-collect-runner-admin-7a3f"

	// 教务账号、密码与 OCR 地址：密码是唯一能把 settings.Reveal → upstream.Login
	// 这段装配串起来的哨兵，假上游把它记下来，检查点按整串比对。三者都必须非空，
	// 否则执行器会在发第一个上游请求之前按 settings_incomplete 收口。
	collectRunnerJWUsername = "e2e-collect-runner-teacher"
	collectRunnerJWPassword = "e2e-collect-runner-password-91c4"
	collectRunnerOCRBaseURL = "http://ocr.e2e.test:8000"

	// collectRunnerKBJCMSID 是学期页下发的节次模式 id；parser 与假上游的最小提取
	// 必须给出同一个值，否则 readCalendar 会按 calendar_invalid 结束本轮。
	collectRunnerKBJCMSID = "1"

	// collectRunnerDetailBlock 是 A101 周一第 1 大节的大节代码（矩阵表头 tdvalue），
	// 也是明细夹具的键。与 collect_pipeline_test.go 的不可用格口径一致。
	collectRunnerDetailBlock = "0102"

	// collectRunnerUnknownBlock 是 B201 周一第 2 大节的大节代码：夹具把它写成半角 J，
	// 字典里没有这个符号 → clean 投影成 unknown（不可用），于是它也会规划一条明细任务。
	collectRunnerUnknownBlock = "030405"

	// collectRunnerDetailTasks 是窗口内不可用格去重后的条数：A101 周一第 1 大节（◆）
	// 与 B201 周一第 2 大节（J）各一条。Plan 按「房 × 天 × 大节」去重，跨周不重复。
	collectRunnerDetailTasks = 2
)

// collectRunnerUpstream 是假上游：实现 collector 包内 upstreamAPI 的六个方法。
//
// 登录、学期、周历、矩阵与明细都只返回夹具数据，页面正文由本文件的页面构造器生成，
// 解析、清洗、编排与发布全部走真实代码。所有可变状态都由 mu 保护：一轮执行在
// collector 的后台协程里，断言与阻塞登录的种子写入在主测试协程里。
//
// weekCalls / detailCalls 是请求计数，按「抓取轮次前后取差」使用：hydrate 的检查点
// 靠它证明「补抓了全部 8 页」而不是只补了一部分。
type collectRunnerUpstream struct {
	mu          sync.Mutex
	pages       map[string][]byte // 楼编号 → 周矩阵页
	details     map[string][]byte // detailKey(房号,星期,大节) → 占用说明页
	gate        *collectRunnerLoginGate
	login       upstream.LoginOptions
	weekCalls   int
	detailCalls int
}

// collectRunnerLoginGate 是「登录卡住」的闸门：续跑轮需要在 Start 返回之后、hydrate
// 之前往库里预置已完成的周矩阵任务。执行链里 Login 是 prepare 的第一个上游动作，
// 卡在这里既能保证 run_id 已经可读，也能保证 hydrate 一定还没开始。
type collectRunnerLoginGate struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

// signalEntered 只关一次 entered：后台协程进入 Login 时通知测试。
func (g *collectRunnerLoginGate) signalEntered() { g.enteredOnce.Do(func() { close(g.entered) }) }

// releaseLogin 只关一次 release：测试写完种子数据后放行后台协程。
func (g *collectRunnerLoginGate) releaseLogin() { g.releaseOnce.Do(func() { close(g.release) }) }

// setPages 换掉整批矩阵页：归属冲突轮与正常轮用不同的页面，执行器本身不变。
func (f *collectRunnerUpstream) setPages(pages map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages = pages
}

// blockLogin 装上登录闸门，返回给测试等待「已进入 Login」与放行用。
func (f *collectRunnerUpstream) blockLogin() *collectRunnerLoginGate {
	gate := &collectRunnerLoginGate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = gate
	return gate
}

// calls 返回当前的上游请求计数（周矩阵 / 明细）。测试按前后差断言，不受轮次之间
// 累计值影响；调用本身是并发安全的。
func (f *collectRunnerUpstream) calls() (week, detail int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.weekCalls, f.detailCalls
}

// loginOptions 返回最近一次登录的入参，用来证明设置里的账号与解封后的密码真的流到了
// 上游客户端，而不是执行器自己编了一份。
func (f *collectRunnerUpstream) loginOptions() upstream.LoginOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.login
}

// Login 记录入参；装了闸门时先通知测试、再等放行（或被 ctx 取消）。
func (f *collectRunnerUpstream) Login(ctx context.Context, opts upstream.LoginOptions) error {
	f.mu.Lock()
	f.login = opts
	gate := f.gate
	f.mu.Unlock()

	if gate == nil {
		return nil
	}
	gate.signalEntered()
	select {
	case <-gate.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// FetchTerm 返回学期页快照：Term / KBJCMSID 是上游侧最小提取，Raw 交给 parser 复核。
func (f *collectRunnerUpstream) FetchTerm(context.Context) (*upstream.TermSnapshot, error) {
	return &upstream.TermSnapshot{
		Term:     testTerm,
		KBJCMSID: collectRunnerKBJCMSID,
		Raw:      collectRunnerTermPage(),
	}, nil
}

// FetchWeekCalendar 返回固定周历：第 9 周 / 共 16 周，与假时钟所在的周一致。
func (f *collectRunnerUpstream) FetchWeekCalendar(context.Context, time.Time) (*upstream.WeekCalendar, error) {
	return &upstream.WeekCalendar{
		Week:       testCurrentWeek,
		TotalWeeks: testTotalWeeks,
		Raw:        collectRunnerWeekPage(),
	}, nil
}

// FetchBuildings 返回白名单里的两栋楼。执行器当前不消费楼列表（advisory），
// 这里仍按契约给出合法数据，避免「未实现」被误报成上游缺能力。
func (f *collectRunnerUpstream) FetchBuildings(context.Context, string) ([]upstream.Building, error) {
	return []upstream.Building{
		{ID: buildingQ10, Name: buildingQ10Name},
		{ID: buildingQ11, Name: buildingQ11Name},
	}, nil
}

// FetchWeekMatrix 按楼编号返回夹具页并记一次请求。
func (f *collectRunnerUpstream) FetchWeekMatrix(_ context.Context, params upstream.WeekMatrixParams) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.weekCalls++
	page, ok := f.pages[params.BuildingID]
	if !ok {
		return nil, fmt.Errorf("假上游没有楼 %s 第 %d 周的矩阵页", params.BuildingID, params.Week)
	}
	return page, nil
}

// FetchOccupancyDetail 按「房号 × 星期 × 大节」返回夹具页并记一次请求。
func (f *collectRunnerUpstream) FetchOccupancyDetail(_ context.Context, params upstream.OccupancyDetailParams) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detailCalls++
	page, ok := f.details[detailKey(params.RoomID, params.Weekday, params.BigSlot)]
	if !ok {
		return nil, fmt.Errorf("假上游没有 %s 星期 %d 大节 %s 的明细页",
			params.RoomID, params.Weekday, params.BigSlot)
	}
	return page, nil
}

// collectRunnerClock 是每调用一次前进一秒的假时钟。
//
// 前进而不是冻结：release_id 由 publish 的 GeneratedAt（时钟）派生，三轮若共用同一时刻
// 会撞 release 主键，Commit 失败会让检查点变成「发布事务失败」而不是「发布成功」。
// 每轮开始前再 Advance 一小时，保证三轮的 started_at 与 generated_at 落在不同小时。
type collectRunnerClock struct {
	mu  sync.Mutex
	now time.Time
}

// newCollectRunnerClock 以 base 为起点建时钟。
func newCollectRunnerClock(base time.Time) *collectRunnerClock {
	return &collectRunnerClock{now: base}
}

// Now 返回值并前进一步；签名匹配 pipeline.Clock 与 runner.Deps.Clock。
func (c *collectRunnerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

// Advance 把时钟向前拨 d：每轮开始前用，拉开三轮的时间窗口。
func (c *collectRunnerClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// collectRunnerTermPage 是学期页正文：可见文本给出学期编号，隐藏输入给出 kbjcmsid。
// parser.TermText 只认这两样，假上游的 TermSnapshot 必须与它逐字一致（readCalendar 双口径）。
func collectRunnerTermPage() string {
	return `<html><body><div>` + testTerm + ` 学期</div>` +
		`<input type="hidden" id="kbjcmsid" value="` + collectRunnerKBJCMSID + `"></body></html>`
}

// collectRunnerWeekPage 是周历页正文：「第 X 周 / 共 Y 周」。
func collectRunnerWeekPage() string {
	return fmt.Sprintf(`<html><body><div>第 %d 周 / 共 %d 周</div></body></html>`,
		testCurrentWeek, testTotalWeeks)
}

// collectRunnerDetailPage 造一张占用说明页：教室状态「正常上课」不在字典符号里 →
// clean 投影成 unknown，与 collect_pipeline_test.go 的夹具同一份。课程区分两个格子，
// 便于失败时看出是哪一条明细任务出的问题。
func collectRunnerDetailPage(course string) []byte {
	return []byte(occupancyFixture(
		occupancyFieldRow("教室状态：", "正常上课"),
		occupancyFieldRow("课程：", course),
		occupancyFieldRow("周次：", "1-16"),
		occupancyFieldRow("时间标志：", "单双周"),
	))
}

// collectRunnerPages 造两栋楼的周矩阵页（窗口内每周共用同一份正文）。
//
// conflict=false：Q10 只报 A101（周一第 1 大节 ◆ 正常上课），Q11 只报 B201
// （周一第 2 大节半角 J → 不在字典 → 未知），两栋楼没有重叠房号。
// conflict=true：Q11 也报出 Q10 的 A101，状态写成半角 J（未知）。白名单顺序 Q10 在前，
// 所以 A101 归 Q10，Q11 的那一行被归属裁决丢弃并记一次冲突——如果赢家判错，
// 公开查询里 A101 周一第 1 小节会变成 unknown 而不是 class，检查点据此判红。
func collectRunnerPages(blocks []string, conflict bool) map[string][]byte {
	// 状态格序号 = 星期下标 × 大节数 + 大节下标；周一第 1、2 个大节分别是 0、1。
	const mondayBlockOne, mondayBlockTwo = 0, 1

	q10 := matrixFixture(blocks,
		matrixRow(blocks, ` jsbh="`+roomA+`"`,
			`<input type="checkbox" value="`+roomA+`">综合教学楼101(90/0)`,
			map[int]string{mondayBlockOne: "◆"}),
	)

	q11Rows := []string{
		matrixRow(blocks, ``,
			`<input type="text" value="`+roomB+`">实验楼201（0/60）`,
			map[int]string{mondayBlockTwo: "J"}),
	}
	if conflict {
		q11Rows = append([]string{
			matrixRow(blocks, ` jsbh="`+roomA+`"`,
				`<input type="checkbox" value="`+roomA+`">实验楼101(0/60)`,
				map[int]string{mondayBlockOne: "J"}),
		}, q11Rows...)
	}

	return map[string][]byte{
		buildingQ10: []byte(q10),
		buildingQ11: []byte(matrixFixture(blocks, q11Rows...)),
	}
}

// openCollectRunnerTestDB 连真实 PostgreSQL 并重建 schema，然后迁移到最新版本。
// 连接与重建复用 openE2ETestDB，只把跳过提示换成这一条检查点的名字；
// 未设置 KJS_TEST_DSN 时跳过，迁移种子提供字典与轴（版本 1），业务数据全部由本用例种入。
func openCollectRunnerTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	return openE2ETestDB(t, "采集执行器端到端检查点")
}

// collectRunState 是轮次在某一时刻的终态/阶段与错误信息。
type collectRunState struct {
	Status       string
	Phase        string
	ErrorCode    string
	ErrorMessage string
}

// loadCollectRunState 读一轮的状态列；空错误码与空原因折成空串，便于断言「无码」。
func loadCollectRunState(ctx context.Context, pool *pgxpool.Pool, runID int64) (collectRunState, error) {
	var state collectRunState
	err := pool.QueryRow(ctx, `
SELECT status, phase, COALESCE(error_code, ''), COALESCE(error_message, '')
  FROM collect_run
 WHERE run_id = $1`, runID).Scan(&state.Status, &state.Phase, &state.ErrorCode, &state.ErrorMessage)
	return state, err
}

// waitCollectRunTerminal 等到轮次离开 running，返回终态；超时直接判红。
//
// 后台一轮包含登录、日历、8 次矩阵抓取、明细与发布，正常应是毫秒级；60 秒是给
// 慢机器与连接池留的余量，超时说明执行链某处卡住了（不是「结果慢」）。
func waitCollectRunTerminal(ctx context.Context, t *testing.T, pool *pgxpool.Pool, runID int64) collectRunState {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for {
		state, err := loadCollectRunState(ctx, pool, runID)
		if err != nil {
			t.Fatalf("读取轮次 %d 状态失败: %v", runID, err)
		}
		if state.Status != string(pipeline.StatusRunning) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("轮次 %d 在 60s 内没有离开 running（phase=%s）", runID, state.Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startCollectRun 发一次 POST /api/admin/sync，断言 202 + running，返回 run_id。
//
// 除了 HTTP 形状，还核对响应里的 started_at 与库里 collect_run.started_at 是同一个
// 时刻：202 的 started_at 由 runner 透传 Collector.Start 的返回值，两处一旦不同源，
// 管理员看到的开始时间就会与轮次列表/详情对不上。
func startCollectRun(
	ctx context.Context,
	t *testing.T,
	client *adminE2EClient,
	pool *pgxpool.Pool,
	cookie *http.Cookie,
	csrf string,
) int64 {
	t.Helper()

	rec := client.do(http.MethodPost, "/api/admin/sync", nil, cookie, csrf)
	data := decodeE2EAccepted(t, rec)
	if got := data["status"]; got != runner.StatusRunning {
		t.Fatalf("期望 202 data.status=running，实际 %#v（body=%s）", got, rec.Body.String())
	}
	runID := e2eDataInt(t, data, "run_id")
	if runID <= 0 {
		t.Fatalf("202 的 run_id 期望正数，实际 %d", runID)
	}
	responseStartedAt, err := time.Parse(time.RFC3339, e2eString(t, data, "started_at"))
	if err != nil {
		t.Fatalf("202 的 started_at 不是 RFC3339: %v", err)
	}

	var dbStartedAt time.Time
	if err := pool.QueryRow(ctx,
		"SELECT started_at FROM collect_run WHERE run_id = $1", runID).Scan(&dbStartedAt); err != nil {
		t.Fatalf("读取轮次 %d 的 started_at 失败: %v", runID, err)
	}
	if !responseStartedAt.Equal(dbStartedAt) {
		t.Fatalf("202 的 started_at（%s）与库里的 started_at（%s）不是同一个时刻",
			responseStartedAt.Format(time.RFC3339Nano), dbStartedAt.Format(time.RFC3339Nano))
	}
	if state, err := loadCollectRunState(ctx, pool, runID); err != nil {
		t.Fatalf("读取新轮次状态失败: %v", err)
	} else if state.Status != string(pipeline.StatusRunning) {
		t.Fatalf("Start 返回时轮次 %d 应已是 running，实际 %s", runID, state.Status)
	}
	t.Logf("轮次已建立: run_id=%d", runID)
	return runID
}

// seedCollectRunnerContinuation 给「续跑轮」预置库里的既有事实，模拟同一轮被执行过一半：
//   - 全部 8 个「楼 × 周」的周矩阵任务已是 done（上一轮矩阵段跑完）；
//   - 本轮归属快照 collect_run_room 已有两个房号（裁决结果落在库里，内存里没有）；
//   - collect_run_week 已有 8 行（页面哈希用可识别的 seed- 前缀，用来证明 hydrate 不写它）；
//   - term 行先种好：collect_run_room.term 是外键，任务先于 ReplaceTerm 写入时必须能引用；
//     term_week 刻意不种，让执行器按锚点公式首次展开整学期日历。
//
// hydrate 的输入只有 sync_task 的状态；观测在发布前没有表可落（CP12 原始页面不落库），
// 所以「内存为空」正是这一轮要重建的东西。
func seedCollectRunnerContinuation(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	runID int64,
	seedAt time.Time,
) {
	t.Helper()

	if _, err := pool.Exec(ctx, `
INSERT INTO term (term, total_weeks) VALUES ($1, $2)
ON CONFLICT (term) DO UPDATE SET total_weeks = EXCLUDED.total_weeks`, testTerm, testTotalWeeks); err != nil {
		t.Fatalf("种学期失败: %v", err)
	}

	for _, room := range []struct {
		roomID   string
		name     string
		building string
	}{
		{roomID: roomA, name: "综合教学楼101", building: buildingQ10},
		{roomID: roomB, name: "实验楼201", building: buildingQ11},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO room (room_id, name, building_id) VALUES ($1, $2, $3)
			 ON CONFLICT (room_id) DO NOTHING`,
			room.roomID, room.name, room.building); err != nil {
			t.Fatalf("种教室 %s 失败: %v", room.roomID, err)
		}
	}

	// 归属快照：与正常轮一致，A101 → Q10、B201 → Q11。
	if _, err := pool.Exec(ctx, `
INSERT INTO collect_run_room (run_id, term, room_id, building_id)
VALUES ($1, $2, $3, $4), ($1, $2, $5, $6)
ON CONFLICT DO NOTHING`,
		runID, testTerm, roomA, buildingQ10, roomB, buildingQ11); err != nil {
		t.Fatalf("种归属快照失败: %v", err)
	}

	for _, buildingID := range []string{buildingQ10, buildingQ11} {
		for week := testCurrentWeek; week < testCurrentWeek+4; week++ {
			if _, err := pool.Exec(ctx, `
INSERT INTO sync_task (run_id, kind, required, building_id, week, status)
VALUES ($1, 'week_matrix', true, $2, $3, 'done')`,
				runID, buildingID, week); err != nil {
				t.Fatalf("种已完成周矩阵任务 %s 第 %d 周失败: %v", buildingID, week, err)
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO collect_run_week (run_id, term, week, building_id, ok, fetched_at, page_sha256)
VALUES ($1, $2, $3, $4, true, $5, $6)`,
				runID, testTerm, week, buildingID, seedAt,
				fmt.Sprintf("seed-%s-%d", buildingID, week)); err != nil {
				t.Fatalf("种周结果 %s 第 %d 周失败: %v", buildingID, week, err)
			}
		}
	}

	// 前置事实：这 8 条任务必须真的是 done，否则 hydrate 不会补抓，检查点会静默退化
	// 成「正常轮」。
	assertCount(ctx, t, pool, 8, "预置的已完成周矩阵任务数", `
SELECT count(*) FROM sync_task
 WHERE run_id = $1 AND kind = 'week_matrix' AND status = 'done'`, runID)
}

// collectRunWeekTaskState 是一条周矩阵任务在库里的完成事实。sync_task 没有 completed_at
// 列，完成与否就记在 status 上，另两个可观测的副作用列是 attempt_count 与 error_message；
// 三者一起拍下来，才能证明 hydrate 对 sync_task 一行都没写（Requirement 8.2）。
type collectRunWeekTaskState struct {
	BuildingID   string
	Week         int
	Status       string
	AttemptCount int
	ErrorMessage string
}

// loadWeekMatrixTaskStates 读出本轮全部周矩阵任务，按 (building_id, week) 排序，
// 供放行前后逐个比对：顺序稳定，比较结果不会因为行返回顺序而抖动。
func loadWeekMatrixTaskStates(
	ctx context.Context,
	t *testing.T,
	pool *pgxpool.Pool,
	runID int64,
) []collectRunWeekTaskState {
	t.Helper()

	rows, err := pool.Query(ctx, `
SELECT building_id, week, status, attempt_count, COALESCE(error_message, '')
  FROM sync_task
 WHERE run_id = $1 AND kind = 'week_matrix'
 ORDER BY building_id, week`, runID)
	if err != nil {
		t.Fatalf("读取周矩阵任务失败: %v", err)
	}
	defer rows.Close()

	var states []collectRunWeekTaskState
	for rows.Next() {
		var state collectRunWeekTaskState
		if err := rows.Scan(&state.BuildingID, &state.Week, &state.Status,
			&state.AttemptCount, &state.ErrorMessage); err != nil {
			t.Fatalf("读取周矩阵任务行失败: %v", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历周矩阵任务失败: %v", err)
	}
	return states
}

// TestCollectRunnerAgainstRealDB 是 collect-runner 的端到端检查点。
//
// 一个库、三轮、同一套装配：第一轮验证「从 Start 到公开查询」整条链，第二轮把归属
// 冲突打进来，第三轮把「上一轮跑完、内存为空」的续跑场景打进来。三轮共用同一个
// collector 实例与假上游，只换页面与时钟起点——装配一旦被改坏，三轮里至少有一轮会红。
func TestCollectRunnerAgainstRealDB(t *testing.T) {
	ctx, pool := openCollectRunnerTestDB(t)

	var logBuf e2eLogBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 密钥管理器指向临时目录里的真实文件：真正的密钥由 POST /api/admin/init 生成，
	// 生成后同一个 Manager 就装载了它，settings.Service 的加封/解封才有密钥可用。
	keys := secret.NewManager(filepath.Join(t.TempDir(), "secret.key"))

	// 假时钟从第 9 周周三 10:00 起步：锚点 2026-09-07、窗口 9..12 都由它推出。
	clock := newCollectRunnerClock(time.Date(2026, 11, 4, 10, 0, 0, 0, shanghai))

	settingsSvc := settings.New(store.NewSettingsRepo(pool), keys, nil)
	runStatusRepo := store.NewRunStatusRepo(pool)

	fake := &collectRunnerUpstream{
		details: map[string][]byte{
			detailKey(roomA, 1, collectRunnerDetailBlock):  collectRunnerDetailPage("高等数学"),
			detailKey(roomB, 1, collectRunnerUnknownBlock): collectRunnerDetailPage("大学物理"),
		},
	}

	// 与 cmd/server/main.go 同口径的装配顺序：仓储 → 上游 → 执行器 → 启动端口。
	// 任务仓储同时实现 TaskStore 与 taskReader（hydrate 只读那一面），注入同一个值。
	syncTasks := store.NewSyncTaskRepo(pool)
	collectorInstance, err := collector.New(collector.Deps{
		Runs:      store.NewCollectRunRepo(pool),
		Terms:     store.NewTermRepo(pool),
		Snapshots: store.NewSnapshotRepo(pool),
		Phases:    store.NewRunPhaseRepo(pool),
		Tasks:     syncTasks,
		TaskRead:  syncTasks,
		Rooms:     store.NewRoomRepo(pool),
		Reader:    store.NewPublishRepo(pool),
		Committer: store.NewReleaseRepo(pool),
		Settings:  settingsSvc,
		Upstream:  fake,
		RunCtx:    ctx,
		Log:       logger,
		Clock:     clock.Now,
	})
	if err != nil {
		t.Fatalf("组装采集执行器失败: %v", err)
	}

	converge := func(c context.Context, now time.Time) ([]int64, error) {
		return pipeline.ConvergeStaleRuns(c, store.NewCollectRunRepo(pool), func() time.Time { return now })
	}
	syncSvc, err := runner.New(runner.Deps{
		Runs:     runStatusRepo,
		Settings: settingsSvc,
		Collect:  collectorInstance,
		Converge: converge,
		Log:      logger,
		Clock:    clock.Now,
	})
	if err != nil {
		t.Fatalf("组装启动端口失败: %v", err)
	}

	handler := api.New(logger, nil, nil, nil, nil,
		api.WithAdmin(store.NewAdminRepo(pool), keys),
		api.WithAdminCookieSecure(false),
		// 设置端点：本轮用它写入白名单、账号、密码与 OCR 地址（真加密、真落库）。
		// 调度器不参与本检查点，reloadCron 传 nil（保存照常成功、跳过重排）。
		api.WithAdminSettings(settingsSvc, nil),
		api.WithSync(syncSvc),
	).Handler()
	client := &adminE2EClient{t: t, handler: handler}

	// ---------- 0. 初始化管理员、登录、写入本轮需要的设置 ----------
	decodeE2ESuccess(t, client.do(http.MethodPost, "/api/admin/init", map[string]string{
		"password":         collectRunnerAdminPassword,
		"password_confirm": collectRunnerAdminPassword,
	}, nil, ""))
	loginRec := client.do(http.MethodPost, "/api/admin/session", map[string]string{
		"password": collectRunnerAdminPassword,
	}, nil, "")
	loginData := decodeE2ESuccess(t, loginRec)
	csrf := e2eString(t, loginData, "csrf_token")
	cookie := e2eSessionCookie(t, loginRec)

	whitelist := []model.Building{
		{ID: buildingQ10, Name: buildingQ10Name},
		{ID: buildingQ11, Name: buildingQ11Name},
	}
	decodeE2ESuccess(t, client.do(http.MethodPut, "/api/admin/settings", map[string]any{
		"jw_username":        collectRunnerJWUsername,
		"jw_password":        collectRunnerJWPassword,
		"ocr_base_url":       collectRunnerOCRBaseURL,
		"building_whitelist": whitelist,
	}, cookie, csrf))

	// 设置写入走真实加封：Reveal 出来的明文必须与原串逐字相同，执行器才有密码可登。
	revealed, err := settingsSvc.Reveal(ctx, model.SettingJWPassword)
	if err != nil {
		t.Fatalf("解封 jw_password 失败: %v", err)
	}
	if revealed != collectRunnerJWPassword {
		t.Fatal("解封出的 jw_password 与写入的哨兵不一致")
	}

	// 公开查询按 main.go 的方式装配，用来验证发布后可见。
	queries, metaService := publicQueries(pool)
	axis := loadAxis(ctx, t, pool)
	blocks := planBlocks(axis)

	// ---------- 检查点 1：Start → 202 → success → 公开查询可见 ----------
	fake.setPages(collectRunnerPages(blocks, false))
	weekBefore, detailBefore := fake.calls()

	run1 := startCollectRun(ctx, t, client, pool, cookie, csrf)
	state1 := waitCollectRunTerminal(ctx, t, pool, run1)
	if state1.Status != store.RunStatusSuccess || state1.Phase != store.RunPhasePublish {
		t.Fatalf("第一轮终态期望 success/publish，实际 %s/%s（error_code=%q message=%q）",
			state1.Status, state1.Phase, state1.ErrorCode, state1.ErrorMessage)
	}
	if state1.ErrorCode != "" || state1.ErrorMessage != "" {
		t.Fatalf("success 轮次不应带错误码或原因，实际 code=%q message=%q",
			state1.ErrorCode, state1.ErrorMessage)
	}
	if week, detail := fake.calls(); week-weekBefore != 8 || detail-detailBefore != collectRunnerDetailTasks {
		t.Fatalf("第一轮上游请求数期望 8 次矩阵 / %d 次明细，实际 %d / %d",
			collectRunnerDetailTasks, week-weekBefore, detail-detailBefore)
	}
	// 登录入参来自设置：账号是明文设置项，密码是 Reveal 出来的明文（不是密文）。
	if got := fake.loginOptions(); got.Username != collectRunnerJWUsername ||
		got.Password != collectRunnerJWPassword || got.OCRBaseURL != collectRunnerOCRBaseURL {
		t.Fatalf("登录入参与设置不一致: username=%q password_set=%v ocr=%q",
			got.Username, got.Password != "", got.OCRBaseURL)
	}

	// 轮次汇总：窗口 9..12、学期、计数与发布信息都要与设计口径一致。
	var (
		run1Term, run1ReleaseID        string
		run1ReleasedAt                 *time.Time
		run1WindowStart, run1WindowEnd int
		run1WeeksTotal, run1RoomsTotal int
		run1ReqTotal, run1ReqDone      int
		run1OptTotal, run1OptDone      int
		run1Unknown, run1Composite     int
		run1AttrConflicts              int
	)
	if err := pool.QueryRow(ctx, `
SELECT term, window_start, window_end, weeks_total, rooms_total,
       required_total, required_done, optional_total, optional_done,
       unknown_cells, composite_cells, attr_conflict_count,
       release_id, released_at
  FROM collect_run
 WHERE run_id = $1`, run1).Scan(
		&run1Term, &run1WindowStart, &run1WindowEnd, &run1WeeksTotal, &run1RoomsTotal,
		&run1ReqTotal, &run1ReqDone, &run1OptTotal, &run1OptDone,
		&run1Unknown, &run1Composite, &run1AttrConflicts,
		&run1ReleaseID, &run1ReleasedAt); err != nil {
		t.Fatalf("读取第一轮汇总失败: %v", err)
	}
	if run1Term != testTerm {
		t.Fatalf("轮次学期期望 %q，实际 %q", testTerm, run1Term)
	}
	if run1WindowStart != testCurrentWeek || run1WindowEnd != testCurrentWeek+3 {
		t.Fatalf("采集窗口期望 %d..%d，实际 %d..%d",
			testCurrentWeek, testCurrentWeek+3, run1WindowStart, run1WindowEnd)
	}
	if run1WeeksTotal != 4 || run1RoomsTotal != 2 {
		t.Fatalf("汇总期望 4 周 / 2 间房，实际 %d/%d", run1WeeksTotal, run1RoomsTotal)
	}
	if run1ReqTotal != 8 || run1ReqDone != 8 ||
		run1OptTotal != collectRunnerDetailTasks || run1OptDone != collectRunnerDetailTasks {
		t.Fatalf("任务计数期望 required=8/8 optional=%d/%d，实际 %d/%d %d/%d",
			collectRunnerDetailTasks, collectRunnerDetailTasks,
			run1ReqTotal, run1ReqDone, run1OptTotal, run1OptDone)
	}
	// B201 周一第 2 大节的半角 J 不在字典里 → 每周 1 格未知，4 周共 4 格。
	if run1Unknown != 4 || run1Composite != 0 || run1AttrConflicts != 0 {
		t.Fatalf("未知/复合/冲突期望 4/0/0，实际 %d/%d/%d",
			run1Unknown, run1Composite, run1AttrConflicts)
	}
	if run1ReleaseID == "" || run1ReleasedAt == nil {
		t.Fatalf("success 轮次应关联版本并写 released_at，实际 release_id=%q released_at=%v",
			run1ReleaseID, run1ReleasedAt)
	}

	// 学期日历：总数与第 1 周周一由执行器的锚点公式写出（采样日 2026-11-04、第 9 周）。
	assertCount(ctx, t, pool, testTotalWeeks, "学期日历周数",
		"SELECT count(*) FROM term_week WHERE term = $1", testTerm)
	var storedAnchor time.Time
	if err := pool.QueryRow(ctx,
		"SELECT monday FROM term_week WHERE term = $1 AND week = 1", testTerm).Scan(&storedAnchor); err != nil {
		t.Fatalf("读取第 1 周周一失败: %v", err)
	}
	if got, want := storedAnchor.Format(time.DateOnly), weekOneMonday.Format(time.DateOnly); got != want {
		t.Fatalf("第 1 周周一期望 %s，实际 %s", want, got)
	}

	// 版本内容：一次发布、唯一 current、2 间房 × 4 周 × 7 天 × 12 小节 = 672 行观测。
	var run1HasConflict, run1Current bool
	if err := pool.QueryRow(ctx,
		"SELECT has_attr_conflict, is_current FROM release WHERE release_id = $1",
		run1ReleaseID).Scan(&run1HasConflict, &run1Current); err != nil {
		t.Fatalf("读取第一轮版本失败: %v", err)
	}
	if run1HasConflict || !run1Current {
		t.Fatalf("第一轮版本期望 无归属冲突 / 是 current，实际 conflict=%v current=%v",
			run1HasConflict, run1Current)
	}
	assertCount(ctx, t, pool, 1, "版本总数", "SELECT count(*) FROM release")
	assertCount(ctx, t, pool, 672, "版本观测行数",
		"SELECT count(*) FROM observation WHERE release_id = $1", run1ReleaseID)
	assertCount(ctx, t, pool, collectRunnerDetailTasks, "版本占用说明行数",
		"SELECT count(*) FROM occupancy_detail WHERE release_id = $1", run1ReleaseID)
	assertCount(ctx, t, pool, 2, "版本归属快照行数",
		"SELECT count(*) FROM release_room_building WHERE release_id = $1", run1ReleaseID)

	// 公开查询可见：周一第 1-2 小节 A101 被占用、B201 空闲，只应查到 B201。
	availability, err := queries.Availability(ctx, queryDate, query.AvailabilityParams{
		StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("查询空教室失败: %v", err)
	}
	if availability.Term != testTerm || availability.Week != testCurrentWeek || availability.Weekday != 1 {
		t.Fatalf("查询上下文不符: term=%q week=%d weekday=%d",
			availability.Term, availability.Week, availability.Weekday)
	}
	if availability.Total != 1 || len(availability.Items) != 1 ||
		availability.Items[0].RoomID != roomB || availability.Items[0].BuildingID != buildingQ11 {
		t.Fatalf("空教室期望只命中 %s/%s，实际 total=%d items=%+v",
			roomB, buildingQ11, availability.Total, availability.Items)
	}
	dayA, err := queries.DayStatus(ctx, queryDate, query.DayParams{RoomID: roomA})
	if err != nil {
		t.Fatalf("查询 %s 全天状态失败: %v", roomA, err)
	}
	assertDayNode(t, dayA.Nodes, "01", "class", "正常上课", false)
	meta, err := metaService.Meta(ctx, clock.Now())
	if err != nil {
		t.Fatalf("查询 meta 失败: %v", err)
	}
	if meta.Empty || meta.Release == nil || meta.Release.ReleaseID != run1ReleaseID {
		t.Fatalf("meta 应指向第一轮版本 %s，实际 empty=%v release=%+v",
			run1ReleaseID, meta.Empty, meta.Release)
	}
	t.Logf("检查点 1 通过: run_id=%d release_id=%s", run1, run1ReleaseID)

	// ---------- 检查点 2：归属冲突轮只含赢家房号 ----------
	// Q11 也报出 A101（状态写成半角 J）；白名单顺序 Q10 在前，A101 归 Q10。
	// 界面上必须看到 Q10 那行（class），而不是 Q11 的 unknown。
	clock.Advance(time.Hour)
	fake.setPages(collectRunnerPages(blocks, true))
	weekBefore, detailBefore = fake.calls()

	run2 := startCollectRun(ctx, t, client, pool, cookie, csrf)
	state2 := waitCollectRunTerminal(ctx, t, pool, run2)
	if state2.Status != store.RunStatusSuccess || state2.Phase != store.RunPhasePublish {
		t.Fatalf("冲突轮终态期望 success/publish，实际 %s/%s（error_code=%q message=%q）",
			state2.Status, state2.Phase, state2.ErrorCode, state2.ErrorMessage)
	}
	if week, detail := fake.calls(); week-weekBefore != 8 || detail-detailBefore != collectRunnerDetailTasks {
		t.Fatalf("冲突轮上游请求数期望 8 次矩阵 / %d 次明细，实际 %d / %d",
			collectRunnerDetailTasks, week-weekBefore, detail-detailBefore)
	}

	// 归属快照：A101 归 Q10 且带冲突标记（被丢弃的是 Q11），B201 归 Q11 无冲突。
	var (
		a101Building, a101ConflictBuilding string
		a101Conflict                       bool
	)
	if err := pool.QueryRow(ctx, `
SELECT building_id, conflict, COALESCE(conflict_building_id, '')
  FROM collect_run_room
 WHERE run_id = $1 AND room_id = $2`, run2, roomA).Scan(
		&a101Building, &a101Conflict, &a101ConflictBuilding); err != nil {
		t.Fatalf("读取冲突轮 %s 的归属失败: %v", roomA, err)
	}
	if a101Building != buildingQ10 || !a101Conflict || a101ConflictBuilding != buildingQ11 {
		t.Fatalf("%s 归属期望 %s（冲突来自 %s），实际 %s（conflict=%v from %q）",
			roomA, buildingQ10, buildingQ11, a101Building, a101Conflict, a101ConflictBuilding)
	}
	assertCount(ctx, t, pool, 2, "冲突轮归属快照房号数",
		"SELECT count(*) FROM collect_run_room WHERE run_id = $1", run2)

	var (
		run2ReleaseID     string
		run2AttrConflicts int
		run2OptTotal      int
		run2OptDone       int
	)
	if err := pool.QueryRow(ctx, `
SELECT release_id, attr_conflict_count, optional_total, optional_done
  FROM collect_run
 WHERE run_id = $1`, run2).Scan(
		&run2ReleaseID, &run2AttrConflicts, &run2OptTotal, &run2OptDone); err != nil {
		t.Fatalf("读取冲突轮汇总失败: %v", err)
	}
	if run2AttrConflicts != 1 {
		t.Fatalf("冲突轮 attr_conflict_count 期望 1，实际 %d", run2AttrConflicts)
	}
	// 明细任务「一条都没被丢弃」：丢弃的明细不进 sync_task，optional_total 会直接变小；
	// 这里要求仍规划了全部不可用格对应的明细任务，且全部完成。
	if run2OptTotal != collectRunnerDetailTasks || run2OptDone != collectRunnerDetailTasks {
		t.Fatalf("冲突轮可选任务期望 %d/%d（无丢弃），实际 %d/%d",
			collectRunnerDetailTasks, collectRunnerDetailTasks, run2OptTotal, run2OptDone)
	}

	var run2HasConflict bool
	if err := pool.QueryRow(ctx,
		"SELECT has_attr_conflict FROM release WHERE release_id = $1",
		run2ReleaseID).Scan(&run2HasConflict); err != nil {
		t.Fatalf("读取冲突轮版本失败: %v", err)
	}
	if !run2HasConflict {
		t.Fatal("冲突轮版本应带 has_attr_conflict=true")
	}
	// 候选只含赢家的房号：A101 在版本归属里仍属 Q10，观测里的状态也来自 Q10 那行。
	var releaseBuilding string
	if err := pool.QueryRow(ctx, `
SELECT building_id FROM release_room_building
 WHERE release_id = $1 AND room_id = $2`, run2ReleaseID, roomA).Scan(&releaseBuilding); err != nil {
		t.Fatalf("读取冲突轮版本归属失败: %v", err)
	}
	if releaseBuilding != buildingQ10 {
		t.Fatalf("版本里 %s 的归属期望 %s，实际 %s", roomA, buildingQ10, releaseBuilding)
	}
	assertCount(ctx, t, pool, 672, "冲突轮观测行数",
		"SELECT count(*) FROM observation WHERE release_id = $1", run2ReleaseID)
	assertCount(ctx, t, pool, 336, "冲突轮 A101 观测行数",
		"SELECT count(*) FROM observation WHERE release_id = $1 AND room_id = $2", run2ReleaseID, roomA)
	// 赢家的状态：A101 周一第 1 小节是 Q10 的 ◆（class），不是 Q11 丢掉那行的 J（unknown）。
	assertCount(ctx, t, pool, 1, "冲突轮 A101 第 9 周周一第 1 小节上课行数", `
SELECT count(*) FROM observation
 WHERE release_id = $1 AND room_id = $2 AND week = $3
   AND weekday = 1 AND node = '01' AND state_key = 'class' AND NOT available`,
		run2ReleaseID, roomA, testCurrentWeek)
	assertCount(ctx, t, pool, 0, "冲突轮 A101 的未知观测行数", `
SELECT count(*) FROM observation
 WHERE release_id = $1 AND room_id = $2 AND state_key = 'unknown'`,
		run2ReleaseID, roomA)
	// 只保留一个 current：第一轮版本已被切下去。
	assertCount(ctx, t, pool, 1, "current 版本数", "SELECT count(*) FROM release WHERE is_current")
	assertCount(ctx, t, pool, 1, "第二轮版本仍是 current",
		"SELECT count(*) FROM release WHERE is_current AND release_id = $1", run2ReleaseID)

	availability, err = queries.Availability(ctx, queryDate, query.AvailabilityParams{
		StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("冲突轮后查询空教室失败: %v", err)
	}
	if availability.Total != 1 || len(availability.Items) != 1 ||
		availability.Items[0].RoomID != roomB {
		t.Fatalf("冲突轮后空教室仍应只命中 %s，实际 total=%d items=%+v",
			roomB, availability.Total, availability.Items)
	}
	t.Logf("检查点 2 通过: run_id=%d 归属冲突只含赢家 %s/%s", run2, roomA, buildingQ10)

	// ---------- 检查点 3：续跑轮 hydrate 后发布完整 ----------
	// 做法：Start 之后卡住 Login（prepare 的第一个上游动作），趁后台还没规划，把本轮
	// 8 条周矩阵任务预置成 done、归属快照与周结果写进库，再放行。执行器随后在
	// hydrate 里把 8 页全部补抓进内存（内存没有页面），pipeline.Run 因任务已 done
	// 全部跳过，最后仍发布出覆盖 4 周的完整版本。
	clock.Advance(time.Hour)
	fake.setPages(collectRunnerPages(blocks, false))
	weekBefore, detailBefore = fake.calls()

	gate := fake.blockLogin()
	run3 := startCollectRun(ctx, t, client, pool, cookie, csrf)
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("10s 内没等到执行器进入登录阶段，续跑轮无法预置数据")
	}
	seedAt := clock.Now()
	seedCollectRunnerContinuation(ctx, t, pool, run3, seedAt)
	// 预置值拍一张快照，放行后再读同一批行：hydrate 的输入只有 sync_task 的状态，
	// 它对这张表只读不写，两个快照必须逐字段相同。
	tasksBefore := loadWeekMatrixTaskStates(ctx, t, pool, run3)
	gate.releaseLogin()

	state3 := waitCollectRunTerminal(ctx, t, pool, run3)
	if state3.Status != store.RunStatusSuccess || state3.Phase != store.RunPhasePublish {
		t.Fatalf("续跑轮终态期望 success/publish，实际 %s/%s（error_code=%q message=%q）",
			state3.Status, state3.Phase, state3.ErrorCode, state3.ErrorMessage)
	}
	// hydrate 补抓全部 8 页；pipeline.Run 跳过全部 done 任务，不再请求矩阵。
	if week, detail := fake.calls(); week-weekBefore != 8 || detail-detailBefore != collectRunnerDetailTasks {
		t.Fatalf("续跑轮上游请求数期望 8 次矩阵（全为 hydrate）/ %d 次明细，实际 %d / %d",
			collectRunnerDetailTasks, week-weekBefore, detail-detailBefore)
	}
	// hydrate 只累计内存候选：预置的 collect_run_week 行（seed- 前缀哈希）一行都不改写，
	// 抓取时间 fetched_at 也仍是预置时刻。
	assertCount(ctx, t, pool, 8, "hydrate 未改写的周结果行数", `
SELECT count(*) FROM collect_run_week
 WHERE run_id = $1 AND page_sha256 LIKE 'seed-%' AND fetched_at = $2`, run3, seedAt)
	// hydrate 同样只读 sync_task：8 条周矩阵任务仍是放行前的 status / attempt_count /
	// error_message，没有被重新记完成、重置或补写失败原因。
	tasksAfter := loadWeekMatrixTaskStates(ctx, t, pool, run3)
	if !slices.Equal(tasksBefore, tasksAfter) {
		t.Fatalf("hydrate 不应改写 sync_task：放行前 %+v，放行后 %+v", tasksBefore, tasksAfter)
	}

	var (
		run3ReleaseID string
		run3ReqTotal  int
		run3ReqDone   int
		run3OptTotal  int
		run3OptDone   int
	)
	if err := pool.QueryRow(ctx, `
SELECT release_id, required_total, required_done, optional_total, optional_done
  FROM collect_run
 WHERE run_id = $1`, run3).Scan(
		&run3ReleaseID, &run3ReqTotal, &run3ReqDone, &run3OptTotal, &run3OptDone); err != nil {
		t.Fatalf("读取续跑轮汇总失败: %v", err)
	}
	if run3ReqTotal != 8 || run3ReqDone != 8 ||
		run3OptTotal != collectRunnerDetailTasks || run3OptDone != collectRunnerDetailTasks {
		t.Fatalf("续跑轮任务计数期望 required=8/8 optional=%d/%d，实际 %d/%d %d/%d",
			collectRunnerDetailTasks, collectRunnerDetailTasks,
			run3ReqTotal, run3ReqDone, run3OptTotal, run3OptDone)
	}

	// 发布完整：观测覆盖窗口内全部 4 周，每周 2 间房 × 7 天 × 12 小节 = 168 行。
	assertCount(ctx, t, pool, 672, "续跑轮观测行数",
		"SELECT count(*) FROM observation WHERE release_id = $1", run3ReleaseID)
	assertCount(ctx, t, pool, 4, "续跑轮版本覆盖的周数",
		"SELECT count(DISTINCT week) FROM observation WHERE release_id = $1", run3ReleaseID)
	for week := testCurrentWeek; week < testCurrentWeek+4; week++ {
		assertCount(ctx, t, pool, 168, fmt.Sprintf("续跑轮第 %d 周观测行数", week),
			"SELECT count(*) FROM observation WHERE release_id = $1 AND week = $2", run3ReleaseID, week)
	}
	// 被 hydrate 重建的两周（任务在预置里就是 done、矩阵页只在 hydrate 里抓过）必须真的
	// 进了发布：抽查 Q10 在第 9 周周一第 1 小节的 ◆ 仍投影成 class。
	assertCount(ctx, t, pool, 1, "续跑轮 A101 第 9 周周一第 1 小节上课行数", `
SELECT count(*) FROM observation
 WHERE release_id = $1 AND room_id = $2 AND week = $3
   AND weekday = 1 AND node = '01' AND state_key = 'class' AND NOT available`,
		run3ReleaseID, roomA, testCurrentWeek)

	availability, err = queries.Availability(ctx, queryDate, query.AvailabilityParams{
		StartNode: "01", EndNode: "02", NodeCount: 2, Limit: 20,
	})
	if err != nil {
		t.Fatalf("续跑轮后查询空教室失败: %v", err)
	}
	if availability.Total != 1 || len(availability.Items) != 1 ||
		availability.Items[0].RoomID != roomB {
		t.Fatalf("续跑轮后空教室仍应只命中 %s，实际 total=%d items=%+v",
			roomB, availability.Total, availability.Items)
	}
	t.Logf("检查点 3 通过: run_id=%d 由 hydrate 补抓 8 页后发布完整（4 周 / 672 行观测）",
		run3)
}
