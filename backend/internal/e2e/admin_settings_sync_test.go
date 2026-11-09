package e2e

// 本文件是 admin-settings-sync 的端到端检查点（tasklist 第 12 项）：
// init → login → GET 默认设置 → PUT（白名单 + cron + 三个秘密）→ GET（*_set、无明文、
// cron_next_at）→ 新调度器启动装载从库读到新表达式 → 清空白名单后 sync 得 blocked →
// 配置名单后 sync 得 running → 再 sync 得 409 → runs 列表与详情 → 非法 cron 42201
// 且设置不变 → 库里密文 v1: → 日志脱敏。
//
// 采集执行器用假实现：真实执行链属于后续 collect-runner（D6），本检查点只要求它按
// 契约「真实建轮」——用 store.StartCollectRun 往 collect_run 插一条 running 行，
// 让轮次读取端点看到的是真数据，而不是内存里的假摘要。
//
// 装配口径照抄 cmd/server/main.go：settings.New(store.NewSettingsRepo(pool), keys, nil)、
// store.NewRunStatusRepo(pool)、runner.New（Converge 用 pipeline.ConvergeStaleRuns 包装）、
// scheduler.New 并 go Run(ctx)、三个 api.With*；scheduler 真的跑起来，PUT 提交 cron 时
// 走的是真实的 sched.Reload，而不是把回调换成记录用的假函数。
//
// 与 main.go 唯一的差别是 scheduler 的 clock/wait：生产传 nil（真实时钟 + time.After），
// 这里注入一个冻结时钟与一个永不触发的 wait。真实时钟会在当前时刻正撞上计划里的
// 周六 04:10 / 周一 03:30 时真的到点建轮，把「只读检查点」变成一条会建轮次的用例；
// 冻结时钟让循环永远等不到到点时刻。Reload 不受影响——它照样解析、算 next、换计划、
// 唤醒循环，所以「保存后立即重排」仍然是被真实代码验证的。
//
// 与 collect_pipeline_test.go / admin_bootstrap_test.go 一样只在设置 KJS_TEST_DSN 时运行，
// 并且会重建 public schema；带 DSN 跑全量测试必须固定 `-p 1`（原因见 collect_pipeline_test.go
// 的包注释）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/api"
	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/runner"
	"github.com/w1ndys/easy-qfnu-kjs/internal/scheduler"
	"github.com/w1ndys/easy-qfnu-kjs/internal/secret"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

const (
	// 管理端口令与三个秘密都带唯一哨兵：日志脱敏用例按这些整串搜索，
	// 任何一个出现在日志或响应里都说明有一处把明文带出去了。
	// webhook 的哨兵保持合法 http(s) URL 形状，否则会被 URL 校验先拒掉，测不到加封。
	e2eAdminSentinelPassword = "e2e-admin-password-sentinel-7c1f"
	e2eSentinelJWPassword    = "e2e-jw-password-sentinel-6f2a1c"
	e2eSentinelWebhook       = "https://open.feishu.cn/open-apis/bot/v2/hook/e2e-webhook-sentinel-9d4b"
	e2eSentinelSignSecret    = "e2e-sign-secret-sentinel-3d8e"

	// e2eSyncCron 与默认值不同，且字段位置可判别（周一 03:30），用来证明
	// PUT 后的 cron_expr 与 cron_next_at 真的换成了新表达式。
	e2eSyncCron = "30 3 * * 1"

	// e2eSyncTerm 是假采集器建轮时写入的学期；collect_run.term 有外键，
	// 用例开始前先往 term 表种一行。
	e2eSyncTerm = "2026-2027-1"

	// 白名单两栋楼的编号与展示名；顺序即提交顺序，GET 必须原样保持。
	e2eBuildingA     = "Q10"
	e2eBuildingAName = "综合教学楼"
	e2eBuildingB     = "Q03"
	e2eBuildingBName = "老文史楼"
)

// e2eSyncCollector 是 runner.Collector 的假实现：按契约真实建一条 running 轮次。
//
// 它只做「建轮」这一件事：登录、学期、窗口、发布都属于 collect-runner（D6）。
// 撞上唯一索引时返回包装 model.ErrRunConflict 的错误，与真实启动器的契约一致——
// runner 收到它会重读 Running，走 409 的分支。
type e2eSyncCollector struct {
	pool *pgxpool.Pool
	term string

	mu    sync.Mutex
	calls int
}

// Start 建立一条 running 轮次并返回启动结果。source 只进日志、不落库（D7）；
// 本假实现不区分 manual / cron —— 本检查点只从 HTTP 手动触发。

func (c *e2eSyncCollector) Start(ctx context.Context, _ model.TriggerSource) (runner.StartResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()

	run, reused, err := store.StartCollectRun(ctx, c.pool, c.term, time.Now())
	if err != nil {
		return runner.StartResult{}, err
	}
	if reused {
		return runner.StartResult{}, fmt.Errorf("假采集器：已有进行中轮次 %w", model.ErrRunConflict)
	}
	return runner.StartResult{
		RunID:     run.RunID,
		Status:    runner.StatusRunning,
		StartedAt: run.StartedAt,
	}, nil
}

// callCount 返回 Start 被调用的次数。
func (c *e2eSyncCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// e2eLogBuffer 是并发安全的日志收集器。
//
// 调度协程与 HTTP handler 会同时写同一个 slog handler，bytes.Buffer 不是并发安全的；
// 直接用 bytes.Buffer 会在 -race 下报数据竞争，日志脱敏断言也会读到撕裂的文本。
type e2eLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 把一行日志写进缓冲。
func (b *e2eLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 返回已收集的全部日志。
func (b *e2eLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// decodeE2EAccepted 断言 202 + 业务码 0，并返回 data 对象（手动同步的成功响应）。
func decodeE2EAccepted(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	body := decodeE2EEnvelope(t, rec)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("期望 HTTP 202，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if body.Code != 0 {
		t.Fatalf("期望业务码 0，实际 %d（body=%s）", body.Code, rec.Body.String())
	}
	data, ok := body.Data.(map[string]any)
	if !ok {
		t.Fatalf("成功响应的 data 不是对象: %#v", body.Data)
	}
	return data
}

// e2eDataInt 取 data 里的整数字段；JSON 数字解码后是 float64，这里按整数值取回。
func e2eDataInt(t *testing.T, data map[string]any, field string) int64 {
	t.Helper()

	value, ok := data[field].(float64)
	if !ok {
		t.Fatalf("data.%s 期望数字，实际 %#v", field, data[field])
	}
	if value != float64(int64(value)) {
		t.Fatalf("data.%s 期望整数，实际 %v", field, value)
	}
	return int64(value)
}

// e2eDataBool 取 data 里的布尔字段。
func e2eDataBool(t *testing.T, data map[string]any, field string) bool {
	t.Helper()

	value, ok := data[field].(bool)
	if !ok {
		t.Fatalf("data.%s 期望布尔，实际 %#v", field, data[field])
	}
	return value
}

// e2eWantWhitelist 返回本用例提交的两条白名单，顺序即断言口径。
func e2eWantWhitelist() []model.Building {
	return []model.Building{
		{ID: e2eBuildingA, Name: e2eBuildingAName},
		{ID: e2eBuildingB, Name: e2eBuildingBName},
	}
}

// e2eAssertWhitelist 断言响应里的 building_whitelist 与提交的数组逐条相等（含顺序）。
func e2eAssertWhitelist(t *testing.T, data map[string]any, want []model.Building) {
	t.Helper()

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("序列化期望白名单失败: %v", err)
	}
	var wantShape any
	if err := json.Unmarshal(raw, &wantShape); err != nil {
		t.Fatalf("反序列化期望白名单失败: %v", err)
	}
	if got := data["building_whitelist"]; !reflect.DeepEqual(got, wantShape) {
		t.Fatalf("building_whitelist 期望 %#v，实际 %#v", wantShape, got)
	}
}

// e2eAssertCronNextAt 断言 cron_next_at 是 UTC RFC3339、严格晚于请求时刻，
// 且换算到 +08:00 后落在期望的星期与时:分上。时间字段的「UTC 写法」与
// 「业务时区求值」是两条独立契约（Requirement 5.5 / 5.6），这里分开断言。
func e2eAssertCronNextAt(
	t *testing.T,
	data map[string]any,
	wantWeekday time.Weekday,
	wantHour, wantMinute int,
	after time.Time,
) {
	t.Helper()

	raw, ok := data["cron_next_at"].(string)
	if !ok {
		t.Fatalf("cron_next_at 期望字符串，实际 %#v", data["cron_next_at"])
	}
	if !strings.HasSuffix(raw, "Z") {
		t.Fatalf("cron_next_at 期望 UTC（以 Z 结尾）的 RFC3339，实际 %q", raw)
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("cron_next_at 不是 RFC3339: %q（err=%v）", raw, err)
	}
	if !at.After(after) {
		t.Fatalf("cron_next_at 必须严格晚于请求时刻：next=%s，请求时刻=%s", at, after)
	}

	business := at.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	if business.Weekday() != wantWeekday || business.Hour() != wantHour || business.Minute() != wantMinute {
		t.Fatalf("cron_next_at 在业务时区期望 %s %02d:%02d，实际 %s",
			wantWeekday, wantHour, wantMinute, business.Format("2006-01-02 15:04:05 MST"))
	}
}

// e2eSettingsRow 是一行设置的完整快照，用来做「非法 cron 后设置逐字节不变」的比对。
// updated_at 折成 UTC RFC3339Nano 字符串再比：同一个库值在不同查询里应得到同一时刻，
// 但 time.Time 的 Location 指针不保证相同，直接 reflect.DeepEqual 可能误报。
type e2eSettingsRow struct {
	Key       string
	Value     string
	Secret    bool
	UpdatedAt string
}

// e2eLoadSettingsRows 读出 settings 全表并按 key 排序。
func e2eLoadSettingsRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []e2eSettingsRow {
	t.Helper()

	rows, err := pool.Query(ctx, "SELECT key, value, is_secret, updated_at FROM settings ORDER BY key")
	if err != nil {
		t.Fatalf("读取设置行失败: %v", err)
	}
	defer rows.Close()

	found := make([]e2eSettingsRow, 0)
	for rows.Next() {
		var (
			row       e2eSettingsRow
			updatedAt time.Time
		)
		if err := rows.Scan(&row.Key, &row.Value, &row.Secret, &updatedAt); err != nil {
			t.Fatalf("扫描设置行失败: %v", err)
		}
		row.UpdatedAt = updatedAt.UTC().Format(time.RFC3339Nano)
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历设置行失败: %v", err)
	}
	return found
}

// e2eFindRun 在一页摘要里按 run_id 找一条；找不到直接失败。
func e2eFindRun(t *testing.T, runs []any, runID int64) map[string]any {
	t.Helper()

	for _, item := range runs {
		summary, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("runs 元素不是对象: %#v", item)
		}
		if e2eDataInt(t, summary, "run_id") == runID {
			return summary
		}
	}
	t.Fatalf("轮次列表里没有 run_id=%d", runID)
	return nil
}

// e2eWaitForLog 等日志里出现 needle，最多等 waitFor。
//
// 调度协程是 go sched.Run(ctx) 异步启动的，扫日志前直接 Contains 会和协程抢时序；
// 有界等待把「日志采集口径对不对」变成确定结论：要么等到，要么明确报错。
func e2eWaitForLog(t *testing.T, buf *e2eLogBuffer, needle string, waitFor time.Duration) {
	t.Helper()

	deadline := time.Now().Add(waitFor)
	for {
		if strings.Contains(buf.String(), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 内没等到日志行 %q，日志采集口径可疑（当前日志：%s）", waitFor, needle, buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// e2eSchedNext 取调度器进程内的下一次触发时刻；没有可用计划直接失败。
func e2eSchedNext(t *testing.T, sched *scheduler.Scheduler) time.Time {
	t.Helper()

	next, ok := sched.Next()
	if !ok {
		t.Fatal("调度器没有下一次触发时刻：计划没装上")
	}
	return next
}

// e2eAssertSchedBusinessTime 断言调度器当前计划在业务时区落在期望的星期与时:分。
// 只判时:分与星期、不判哪一周：同一表达式在不同日期求值，命中的是同一类时刻。
func e2eAssertSchedBusinessTime(
	t *testing.T,
	sched *scheduler.Scheduler,
	wantWeekday time.Weekday,
	wantHour, wantMinute int,
) {
	t.Helper()

	next := e2eSchedNext(t, sched)
	business := next.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	if business.Weekday() != wantWeekday || business.Hour() != wantHour || business.Minute() != wantMinute {
		t.Fatalf("sched.Next() 在业务时区期望 %s %02d:%02d，实际 %s",
			wantWeekday, wantHour, wantMinute, business.Format("2006-01-02 15:04:05 MST"))
	}
}

// e2eAssertSchedNextMatches 断言「进程内计划」与「响应里的 cron_next_at」是同一个计划时刻，
// 并且在业务时区落在期望的星期与时:分上。
//
// 这是「保存后立即重排」唯一的外部证据：只看库里的 cron_expr，无法区分「写库成功」与
// 「重排真的生效」——装配层把 sched.Reload 漏掉时，库照样变、响应照样对，只有进程内
// 计划还停在旧表达式。两边的时钟不同源（响应由 handler 按真实时钟算，sched.Next() 由
// 本用例冻结的时钟算），所以先各自截到分钟再比：同一分钟内的秒差不是计划不同，而
// 「±1 分钟」的差值容差会把相邻两个计划分钟（04:09 与 04:10）也一并放过。
//
// 跨分钟窗口单独处理：两次求值之间恰好跨过计划分钟时，冻结时钟算到本周的那个时刻、
// 真实时钟已经算到下一个周期，两边差一个完整周期。本检查点的两个表达式都是每周一次
// （默认周六 04:10、提交的周一 03:30），周期是 7 天；只有这种差值才按「跨窗口」放行，
// 其余不同分钟一律按装配失败处理。
func e2eAssertSchedNextMatches(
	t *testing.T,
	sched *scheduler.Scheduler,
	data map[string]any,
	wantWeekday time.Weekday,
	wantHour, wantMinute int,
) {
	t.Helper()

	raw, ok := data["cron_next_at"].(string)
	if !ok {
		t.Fatalf("cron_next_at 期望字符串，实际 %#v", data["cron_next_at"])
	}
	respAt, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("cron_next_at 不是 RFC3339: %q（err=%v）", raw, err)
	}
	next := e2eSchedNext(t, sched)

	nextMinute := next.Truncate(time.Minute)
	respMinute := respAt.Truncate(time.Minute)
	if !nextMinute.Equal(respMinute) {
		if respAt.Sub(next) != 7*24*time.Hour {
			t.Fatalf("sched.Next() 与响应 cron_next_at 不在同一分钟，且不是跨过计划分钟造成的相邻周期：sched=%s，响应=%s",
				next.Format(time.RFC3339), respAt.Format(time.RFC3339))
		}
		t.Logf("两次求值跨过了计划分钟（sched=%s，响应=%s），跳过「同一计划时刻」比对；星期与时:分仍逐项断言",
			next.Format(time.RFC3339), respAt.Format(time.RFC3339))
	}

	business := next.In(time.FixedZone("Asia/Shanghai", 8*60*60))
	if business.Weekday() != wantWeekday || business.Hour() != wantHour || business.Minute() != wantMinute {
		t.Fatalf("sched.Next() 在业务时区期望 %s %02d:%02d，实际 %s",
			wantWeekday, wantHour, wantMinute, business.Format("2006-01-02 15:04:05 MST"))
	}
}

// TestAdminSettingsSyncAgainstRealDB 是设置、同步与运行状态的端到端检查点。
// 步骤顺序与 tasklist 第 12 项一致，任一步失败都说明装配或链路有一段没接上。
func TestAdminSettingsSyncAgainstRealDB(t *testing.T) {
	// 先按本检查点自己的口径判跳过，再复用 admin-bootstrap 的干净库准备逻辑
	// （重建 public schema + 迁移到最新版本）：DSN 未设置时报告的是本条用例的名字。
	if os.Getenv("KJS_TEST_DSN") == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过设置/同步端到端检查点")
	}
	ctx, pool := openAdminE2ETestDB(t)

	var logBuf e2eLogBuffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 假采集器要写 collect_run.term，先种一行学期（term 表在干净库里没有种子数据）。
	if _, err := pool.Exec(ctx,
		"INSERT INTO term (term, total_weeks) VALUES ($1, 20) ON CONFLICT (term) DO NOTHING",
		e2eSyncTerm); err != nil {
		t.Fatalf("种学期失败: %v", err)
	}

	// 密钥管理器指向临时目录里的真实文件：与 admin-bootstrap 的检查点同一口径。
	keys := secret.NewManager(filepath.Join(t.TempDir(), "secret.key"))
	loaded, err := keys.Load()
	if err != nil {
		t.Fatalf("干净目录里 Load 不该报错: %v", err)
	}
	if loaded {
		t.Fatal("干净目录里不该装载出密钥")
	}

	// 与 cmd/server/main.go 同口径装配：设置服务持有仓储与密钥，运行状态仓储分给
	// 读取端点与启动端口，Converge 用 pipeline.ConvergeStaleRuns 包装（D8）。
	settingsSvc := settings.New(store.NewSettingsRepo(pool), keys, nil)
	runStatusRepo := store.NewRunStatusRepo(pool)
	collector := &e2eSyncCollector{pool: pool, term: e2eSyncTerm}
	converge := func(c context.Context, now time.Time) ([]int64, error) {
		return pipeline.ConvergeStaleRuns(c, store.NewCollectRunRepo(pool), func() time.Time { return now })
	}
	syncSvc, err := runner.New(runner.Deps{
		Runs:     runStatusRepo,
		Settings: settingsSvc,
		Collect:  collector,
		Converge: converge,
		Log:      logger,
	})
	if err != nil {
		t.Fatalf("组装启动端口失败: %v", err)
	}

	// 调度器真的跑起来：PUT 提交 cron 时走真实的 sched.Reload。时钟与等待由本用例接管：
	// clock 冻结在用例开始的一个时刻，wait 恒返回一个永不就绪的通道，所以即使真实时刻
	// 正撞上计划里的周六 04:10 / 周一 03:30，循环也不会到点触发（不会建轮、不会让
	// collector 的调用计数在断言之间偷偷加一）。冻结时刻与「现在」只差用例自身的耗时，
	// 第 2、3 步拿 sched.Next() 与响应 cron_next_at 比对时截到分钟后比较；两次求值
	// 之间恰好跨过计划分钟的情形由 e2eAssertSchedNextMatches 显式处理。
	schedNow := time.Now()
	schedWaitCh := make(chan time.Time)
	sched := scheduler.New(logger, syncSvc.StartCron, settingsSvc,
		func() time.Time { return schedNow },
		func(time.Duration) <-chan time.Time { return schedWaitCh },
	)
	schedCtx, stopSched := context.WithCancel(ctx)
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		sched.Run(schedCtx)
	}()
	t.Cleanup(func() {
		stopSched()
		<-schedDone
	})

	handler := api.New(logger, nil, nil, nil, nil,
		api.WithAdmin(store.NewAdminRepo(pool), keys),
		api.WithAdminCookieSecure(false),
		api.WithAdminSettings(settingsSvc, sched.Reload),
		api.WithRunStatus(runStatusRepo),
		api.WithSync(syncSvc),
	).Handler()
	client := &adminE2EClient{t: t, handler: handler}

	// ---------- 0. 迁移到 0005，干净库里只有种子设置 ----------
	var migrated bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM schema_migration WHERE version = '0005_default_cron_5_fields')").Scan(&migrated); err != nil {
		t.Fatalf("查询迁移记录失败: %v", err)
	}
	if !migrated {
		t.Fatal("schema_migration 里没有 0005_default_cron_5_fields，干净库迁移没跑完")
	}

	// ---------- 1. init：创建密钥，随后登录拿会话与 CSRF ----------
	decodeE2ESuccess(t, client.do(http.MethodPost, "/api/admin/init", map[string]string{
		"password":         e2eAdminSentinelPassword,
		"password_confirm": e2eAdminSentinelPassword,
	}, nil, ""))

	loginRec := client.do(http.MethodPost, "/api/admin/session", map[string]string{
		"password": e2eAdminSentinelPassword,
	}, nil, "")
	loginData := decodeE2ESuccess(t, loginRec)
	csrfToken := e2eString(t, loginData, "csrf_token")
	cookie := e2eSessionCookie(t, loginRec)

	// ---------- 2. GET 默认设置：默认 cron、空白名单、秘密全部未设置 ----------
	defaultsAt := time.Now()
	defaultsRec := client.do(http.MethodGet, "/api/admin/settings", nil, cookie, "")
	defaults := decodeE2ESuccess(t, defaultsRec)
	if got := defaultsRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET settings 期望 Cache-Control: no-store，实际 %q", got)
	}
	if got := defaults["cron_expr"]; got != model.DefaultCronExpr {
		t.Fatalf("默认 cron_expr 期望 %q，实际 %#v", model.DefaultCronExpr, got)
	}
	for _, field := range []string{"jw_username", "ocr_base_url"} {
		if got := defaults[field]; got != "" {
			t.Fatalf("默认 %s 期望空串，实际 %#v", field, got)
		}
	}
	for _, field := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if e2eDataBool(t, defaults, field) {
			t.Fatalf("默认 %s 期望 false", field)
		}
	}
	if got, ok := defaults["building_whitelist"].([]any); !ok || len(got) != 0 {
		t.Fatalf("默认 building_whitelist 期望空数组，实际 %#v", defaults["building_whitelist"])
	}
	e2eAssertCronNextAt(t, defaults, time.Saturday, 4, 10, defaultsAt)
	// 基线：进程内计划此刻也是默认表达式（scheduler.New 构造时就装了默认值，Run 的启动
	// 装载从库里读到的同样是默认值），与响应里的 cron_next_at 落在同一分钟。有了这条，
	// 第 3 步「变成周一 03:30」才是从默认值真的换了计划，而不是两边都没装。
	e2eAssertSchedNextMatches(t, sched, defaults, time.Saturday, 4, 10)

	// ---------- 3. PUT 白名单 + cron + 三个秘密 ----------
	putAt := time.Now()
	putRec := client.do(http.MethodPut, "/api/admin/settings", map[string]any{
		"building_whitelist": e2eWantWhitelist(),
		"cron_expr":          e2eSyncCron,
		"jw_password":        e2eSentinelJWPassword,
		"feishu_webhook":     e2eSentinelWebhook,
		"feishu_sign_secret": e2eSentinelSignSecret,
	}, cookie, csrfToken)
	putData := decodeE2ESuccess(t, putRec)
	if got := putData["cron_expr"]; got != e2eSyncCron {
		t.Fatalf("PUT 回显 cron_expr 期望 %q，实际 %#v", e2eSyncCron, got)
	}
	for _, field := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if !e2eDataBool(t, putData, field) {
			t.Fatalf("PUT 后 %s 期望 true", field)
		}
	}
	e2eAssertWhitelist(t, putData, e2eWantWhitelist())
	e2eAssertCronNextAt(t, putData, time.Monday, 3, 30, putAt)
	// 保存后立即重排（CP19）：PUT 提交了 cron_expr，handler 在回读之后、写响应之前调用
	// 真实的 sched.Reload，进程内计划必须已经从默认的「周六 04:10」换成「周一 03:30」，
	// 且与响应里的 cron_next_at 是同一计划时刻。
	e2eAssertSchedNextMatches(t, sched, putData, time.Monday, 3, 30)
	e2eAssertNoSecretsInBody(t, "PUT /api/admin/settings 响应", putRec.Body.String())

	// ---------- 4. GET：*_set、无明文、cron_next_at 按新表达式 ----------
	getAt := time.Now()
	getRec := client.do(http.MethodGet, "/api/admin/settings", nil, cookie, "")
	getData := decodeE2ESuccess(t, getRec)
	for _, field := range []string{"jw_password_set", "feishu_webhook_set", "feishu_sign_secret_set"} {
		if !e2eDataBool(t, getData, field) {
			t.Fatalf("保存后 GET 的 %s 期望 true", field)
		}
	}
	if got := getData["jw_username"]; got != "" {
		t.Fatalf("未提交 jw_username 时应保持空串，实际 %#v", got)
	}
	e2eAssertWhitelist(t, getData, e2eWantWhitelist())
	e2eAssertCronNextAt(t, getData, time.Monday, 3, 30, getAt)
	e2eAssertNoSecretsInBody(t, "GET /api/admin/settings 响应", getRec.Body.String())

	// ---------- 补充检查点：调度器启动时从 settings 源装载库里已保存的表达式 ----------
	//
	// 上面第 3 步验证的是「保存后立即重排」（sched.Reload）；这里验证另一条路径：
	// 进程重启后调度器的启动装载（Run → loadInitial）从 settings 源读到库里那条新表达式
	// 并装上计划。两处共用 cronexpr.ResolveStartup，但读库与安装是各自独立的一次，
	// 所以「重排成功」不能替代「启动装载能读到库值」。装配层把来源接错（例如 nil 或错的
	// 服务）只有这条端到端断言会红；「启动装载只装默认值」已由 scheduler 单测
	// TestNextMatchesCronexpr 覆盖，这里只补一层真实库 + 真实装配的串联证据。
	//
	// 新起一个调度器而不是复用上面的 sched：那个已经被 Reload 过，它的 Next() 无法区分
	// 「从库装载」与「重排留下来」。新实例构造时只装了 DefaultCronExpr（周六 04:10），
	// 启动装载完成后才变成库里的周一 03:30，前后两个快照的差别就是装载真的发生了的证据。
	//
	// 时钟仍冻结在同一个时刻，wait 恒不就绪（与上面那个调度器同口径）：装载完成后不会
	// 到点触发，所以 trigger 直接传 nil——这条只读检查点不该碰 collector 的调用计数。
	startupBuf := &e2eLogBuffer{}
	startupLogger := slog.New(slog.NewJSONHandler(startupBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	frozenWait := func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	restartedSched := scheduler.New(startupLogger, nil, settingsSvc,
		func() time.Time { return schedNow },
		frozenWait,
	)
	// 装载之前：构造时装的默认表达式，周六 04:10。
	e2eAssertSchedBusinessTime(t, restartedSched, time.Saturday, 4, 10)

	restartCtx, stopRestart := context.WithCancel(ctx)
	restartDone := make(chan struct{})
	go func() {
		defer close(restartDone)
		restartedSched.Run(restartCtx)
	}()
	t.Cleanup(func() {
		stopRestart()
		<-restartDone
	})

	// scheduler_started 在 loadInitial 之后才落盘，等到它就等于等到了装载结束，
	// 之后读 Next() 不会有「装载还没跑完」的时序竞争。
	e2eWaitForLog(t, startupBuf, "scheduler_started", 5*time.Second)
	// 装载之后：库里的表达式（第 3 步 PUT 的 30 3 * * 1），周一 03:30。
	e2eAssertSchedBusinessTime(t, restartedSched, time.Monday, 3, 30)
	if logs := startupBuf.String(); strings.Contains(logs, "未配置表达式来源") {
		t.Fatalf("启动装载应已接上 settings 源，日志里不该出现「未配置表达式来源」：%s", logs)
	}

	// ---------- 5. 先清空名单，再手动 sync：202 blocked ----------
	//
	// 对 tasklist 第 12 项的「空白名单 sync 得 blocked」：那一步不能直接照抄成「GET 到
	// 空白名单」，因为第 3 步刚把白名单存成了两栋楼——库里的名单此刻是非空的。所以这一步
	// 就是 tasklist 次序里的那次「空白名单」准备动作：先 PUT 空数组把名单清掉，再 sync。
	// 这次 PUT 没有提交 cron_expr，不触发重排，进程内计划仍是第 3 步装上的周一 03:30。
	clearRec := client.do(http.MethodPut, "/api/admin/settings", map[string]any{
		"building_whitelist": []model.Building{},
	}, cookie, csrfToken)
	clearData := decodeE2ESuccess(t, clearRec)
	if got, ok := clearData["building_whitelist"].([]any); !ok || len(got) != 0 {
		t.Fatalf("清空白名单后 GET 形状期望空数组，实际 %#v", clearData["building_whitelist"])
	}

	blockedRec := client.do(http.MethodPost, "/api/admin/sync", nil, cookie, csrfToken)
	blockedData := decodeE2EAccepted(t, blockedRec)
	if got := blockedData["status"]; got != "blocked" {
		t.Fatalf("空白名单 sync 期望 status=blocked，实际 %#v", got)
	}
	blockedRunID := e2eDataInt(t, blockedData, "run_id")
	if blockedRunID <= 0 {
		t.Fatalf("blocked 轮次 run_id 期望正数，实际 %d", blockedRunID)
	}
	blockedReason := e2eString(t, blockedData, "reason")
	if _, err := time.Parse(time.RFC3339, e2eString(t, blockedData, "started_at")); err != nil {
		t.Fatalf("blocked started_at 不是 RFC3339: %v", err)
	}
	// 库里这一轮必须是建立即终态，且原因与响应逐字一致。
	var (
		blockedStatus    string
		blockedPhase     string
		blockedErrorCode string
		blockedMessage   string
		blockedTerm      *string
	)
	if err := pool.QueryRow(ctx,
		"SELECT status, phase, error_code, error_message, term FROM collect_run WHERE run_id = $1",
		blockedRunID).Scan(&blockedStatus, &blockedPhase, &blockedErrorCode, &blockedMessage, &blockedTerm); err != nil {
		t.Fatalf("读取 blocked 轮次失败: %v", err)
	}
	if blockedStatus != "blocked" || blockedPhase != "finished" || blockedErrorCode != "whitelist_empty" {
		t.Fatalf("blocked 轮次列期望 blocked/finished/whitelist_empty，实际 %s/%s/%s",
			blockedStatus, blockedPhase, blockedErrorCode)
	}
	if blockedMessage != blockedReason {
		t.Fatalf("blocked error_message 与响应 reason 不一致: %q vs %q", blockedMessage, blockedReason)
	}
	if blockedTerm != nil {
		t.Fatalf("blocked 轮次 term 期望 null，实际 %q", *blockedTerm)
	}

	// ---------- 6. 配置名单后手动 sync：202 running，真实建轮 ----------
	decodeE2ESuccess(t, client.do(http.MethodPut, "/api/admin/settings", map[string]any{
		"building_whitelist": e2eWantWhitelist(),
	}, cookie, csrfToken))

	runningAt := time.Now()
	runningRec := client.do(http.MethodPost, "/api/admin/sync", nil, cookie, csrfToken)
	runningData := decodeE2EAccepted(t, runningRec)
	if got := runningData["status"]; got != "running" {
		t.Fatalf("配好名单后 sync 期望 status=running，实际 %#v", got)
	}
	runningRunID := e2eDataInt(t, runningData, "run_id")
	if runningRunID == blockedRunID {
		t.Fatalf("running 轮次不该复用 blocked 轮次（run_id=%d）", runningRunID)
	}
	runningStartedAt, err := time.Parse(time.RFC3339, e2eString(t, runningData, "started_at"))
	if err != nil {
		t.Fatalf("running started_at 不是 RFC3339: %v", err)
	}
	if runningStartedAt.Before(runningAt.Add(-time.Minute)) {
		t.Fatalf("running started_at 落在请求之前太远: %s", runningStartedAt)
	}
	if got := collector.callCount(); got != 1 {
		t.Fatalf("采集启动器期望被调用 1 次，实际 %d", got)
	}
	var phase string
	if err := pool.QueryRow(ctx, "SELECT phase FROM collect_run WHERE run_id = $1", runningRunID).Scan(&phase); err != nil {
		t.Fatalf("读取 running 轮次阶段失败: %v", err)
	}

	// ---------- 7. 再 sync：409 / 40901，文案点名 run_id 与 phase，不再建轮 ----------
	busyRec := client.do(http.MethodPost, "/api/admin/sync", nil, cookie, csrfToken)
	decodeE2EFailure(t, busyRec, http.StatusConflict, 40901)
	busyMessage := decodeE2EEnvelope(t, busyRec).Message
	if !strings.Contains(busyMessage, fmt.Sprintf("run_id=%d", runningRunID)) {
		t.Fatalf("409 消息期望点名 run_id=%d，实际 %q", runningRunID, busyMessage)
	}
	if !strings.Contains(busyMessage, "phase="+phase) {
		t.Fatalf("409 消息期望点名 phase=%s，实际 %q", phase, busyMessage)
	}
	if got := collector.callCount(); got != 1 {
		t.Fatalf("冲突请求不该再调用采集启动器，实际调用 %d 次", got)
	}
	assertCount(ctx, t, pool, 1, "进行中轮次行数", "SELECT count(*) FROM collect_run WHERE status = 'running'")

	// ---------- 8. runs 列表与详情 ----------
	listRec := client.do(http.MethodGet, "/api/admin/runs?limit=10", nil, cookie, "")
	listData := decodeE2ESuccess(t, listRec)
	listRuns, ok := listData["runs"].([]any)
	if !ok {
		t.Fatalf("runs 期望数组，实际 %#v", listData["runs"])
	}
	if total := e2eDataInt(t, listData, "total"); total < 2 {
		t.Fatalf("轮次总数期望至少 2（blocked + running），实际 %d", total)
	}
	if limit := e2eDataInt(t, listData, "limit"); limit != 10 {
		t.Fatalf("limit 回显期望 10，实际 %d", limit)
	}
	if offset := e2eDataInt(t, listData, "offset"); offset != 0 {
		t.Fatalf("offset 回显期望 0，实际 %d", offset)
	}
	// 刚建的一轮必须是最新一条（Requirement 8.6），且摘要状态正确。
	// 元素类型逐个 checked 断言：类型不符时给 t.Fatalf 的可读报错，而不是 on-the-fly
	// 断言失败带来的 panic（panic 的栈指向运行库，看不出是哪一步的形状不对）。
	if len(listRuns) == 0 {
		t.Fatal("runs 为空数组，无法断言最新一条")
	}
	first, ok := listRuns[0].(map[string]any)
	if !ok {
		t.Fatalf("runs 首条不是对象: %#v", listRuns[0])
	}
	if e2eDataInt(t, first, "run_id") != runningRunID {
		t.Fatalf("runs 首条期望刚建的 run_id=%d，实际 %#v", runningRunID, first)
	}
	if first["status"] != "running" {
		t.Fatalf("runs 首条状态期望 running，实际 %#v", first["status"])
	}
	blockedSummary := e2eFindRun(t, listRuns, blockedRunID)
	if blockedSummary["status"] != "blocked" || blockedSummary["error_code"] != "whitelist_empty" {
		t.Fatalf("blocked 摘要期望 status=blocked / error_code=whitelist_empty，实际 %#v", blockedSummary)
	}
	if blockedSummary["term"] != nil {
		t.Fatalf("blocked 摘要 term 期望 null，实际 %#v", blockedSummary["term"])
	}

	runningDetail := decodeE2ESuccess(t, client.do(
		http.MethodGet, fmt.Sprintf("/api/admin/runs/%d", runningRunID), nil, cookie, ""))
	runningRun, ok := runningDetail["run"].(map[string]any)
	if !ok {
		t.Fatalf("详情 run 期望对象，实际 %#v", runningDetail["run"])
	}
	if e2eDataInt(t, runningRun, "run_id") != runningRunID || runningRun["status"] != "running" {
		t.Fatalf("详情 run 期望 run_id=%d / status=running，实际 %#v", runningRunID, runningRun)
	}
	tasks, ok := runningDetail["tasks"].(map[string]any)
	if !ok || len(tasks) != 7 {
		t.Fatalf("详情 tasks 期望 7 个键，实际 %#v", runningDetail["tasks"])
	}
	if tasks["required_total"] != float64(0) || tasks["optional_total"] != float64(0) ||
		tasks["first_failure_message"] != "" {
		t.Fatalf("未建任务的轮次 tasks 期望全零与空首错，实际 %#v", tasks)
	}
	if weeks, ok := runningDetail["weeks"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("未抓取的轮次 weeks 期望 []，实际 %#v", runningDetail["weeks"])
	}

	blockedDetail := decodeE2ESuccess(t, client.do(
		http.MethodGet, fmt.Sprintf("/api/admin/runs/%d", blockedRunID), nil, cookie, ""))
	blockedRun, ok := blockedDetail["run"].(map[string]any)
	if !ok {
		t.Fatalf("blocked 详情 run 期望对象，实际 %#v", blockedDetail["run"])
	}
	if blockedRun["status"] != "blocked" || blockedRun["error_code"] != "whitelist_empty" ||
		blockedRun["finished_at"] == nil {
		t.Fatalf("blocked 详情期望 status=blocked / error_code / finished_at 非空，实际 %#v", blockedRun)
	}
	if weeks, ok := blockedDetail["weeks"].([]any); !ok || len(weeks) != 0 {
		t.Fatalf("blocked 轮次 weeks 期望 []，实际 %#v", blockedDetail["weeks"])
	}

	// ---------- 9. 非法 cron：42201，且设置逐字节不变 ----------
	rowsBefore := e2eLoadSettingsRows(ctx, t, pool)
	invalidRec := client.do(http.MethodPut, "/api/admin/settings", map[string]any{
		"cron_expr": "0 10 4 * * 6", // 6 字段：本 spec 只接受 5 字段
	}, cookie, csrfToken)
	decodeE2EFailure(t, invalidRec, http.StatusUnprocessableEntity, 42201)
	if msg := decodeE2EEnvelope(t, invalidRec).Message; !strings.Contains(msg, "cron_expr") {
		t.Fatalf("42201 消息期望点名 cron_expr，实际 %q", msg)
	}
	rowsAfter := e2eLoadSettingsRows(ctx, t, pool)
	if !reflect.DeepEqual(rowsBefore, rowsAfter) {
		t.Fatalf("非法 cron 不该改动任何设置行：前 %#v，后 %#v", rowsBefore, rowsAfter)
	}
	unchanged := decodeE2ESuccess(t, client.do(http.MethodGet, "/api/admin/settings", nil, cookie, ""))
	if got := unchanged["cron_expr"]; got != e2eSyncCron {
		t.Fatalf("非法 cron 被拒后 cron_expr 期望仍为 %q，实际 %#v", e2eSyncCron, got)
	}

	// ---------- 10. 库里三项秘密都是 v1: 信封密文，且不等于明文 ----------
	ciphertexts := make([]string, 0, 3)
	for key, plaintext := range map[string]string{
		model.SettingJWPassword:       e2eSentinelJWPassword,
		model.SettingFeishuWebhook:    e2eSentinelWebhook,
		model.SettingFeishuSignSecret: e2eSentinelSignSecret,
	} {
		var (
			value      string
			secretFlag bool
		)
		if err := pool.QueryRow(ctx,
			"SELECT value, is_secret FROM settings WHERE key = $1", key).Scan(&value, &secretFlag); err != nil {
			t.Fatalf("读取设置 %s 失败: %v", key, err)
		}
		if !secretFlag {
			t.Fatalf("设置 %s 的 is_secret 期望 true", key)
		}
		if !strings.HasPrefix(value, "v1:") {
			t.Fatalf("设置 %s 的库值期望 v1: 信封密文，实际 %q", key, value)
		}
		if value == plaintext || strings.Contains(value, plaintext) {
			t.Fatalf("设置 %s 的库值仍是明文", key)
		}
		ciphertexts = append(ciphertexts, value)
	}

	// ---------- 11. 日志脱敏：哨兵、密文、Cookie 与 CSRF 都不进日志 ----------
	//
	// 先证明「采集到的确实有日志、而且就是这条链路的日志」，再扫哨兵：缓冲为空时
	// strings.Contains 恒为 false，整段脱敏断言会变成永远通过的空转（slog handler
	// 接错、日志级别被调到 ERROR 以上都会这样）。scheduler_started 是调度协程装载
	// 完成后必记的一条非敏感行，用它做锚点；协程是异步启动的，所以用有界等待。
	e2eWaitForLog(t, &logBuf, "scheduler_started", 5*time.Second)
	logs := logBuf.String()
	for name, sentinel := range map[string]string{
		"管理员口令":              e2eAdminSentinelPassword,
		"jw_password":        e2eSentinelJWPassword,
		"feishu_webhook":     e2eSentinelWebhook,
		"feishu_sign_secret": e2eSentinelSignSecret,
		"会话 Cookie":          cookie.Value,
		"CSRF 令牌":            csrfToken,
	} {
		if strings.Contains(logs, sentinel) {
			t.Fatalf("%s 出现在日志里，脱敏失败", name)
		}
	}
	for _, ciphertext := range ciphertexts {
		if strings.Contains(logs, ciphertext) {
			t.Fatal("设置密文出现在日志里，脱敏失败")
		}
	}
}

// e2eAssertNoSecretsInBody 断言响应体里没有任何秘密哨兵。
func e2eAssertNoSecretsInBody(t *testing.T, what, body string) {
	t.Helper()

	for name, sentinel := range map[string]string{
		"jw_password":        e2eSentinelJWPassword,
		"feishu_webhook":     e2eSentinelWebhook,
		"feishu_sign_secret": e2eSentinelSignSecret,
	} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("%s 的明文出现在 %s 里", name, what)
		}
	}
}
