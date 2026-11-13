package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
	"github.com/w1ndys/easy-qfnu-kjs/internal/pipeline"
	"github.com/w1ndys/easy-qfnu-kjs/internal/settings"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
	"github.com/w1ndys/easy-qfnu-kjs/internal/upstream"
)

// executeNow 是本组用例固定的执行时刻：2026-11-04 是周三，所在周周一为 2026-11-02，
// 周历页自报第 9 周，所以第 1 周周一 = 2026-09-07（design「Test Strategy」的锚点样例）。
var executeNow = time.Date(2026, 11, 4, 10, 0, 0, 0, shanghai)

// executeRunID 是本组用例的轮次编号。
const executeRunID = int64(77)

// settingsSentinel 埋进每一项设置值里，用来证明失败原因只点名键、不写值。
const settingsSentinel = "9f3a"

// 学期页与周历页的最小夹具：正文里学期编号、kbjcmsid 与「第 X 周 / Y 周」都与
// upstream 的最小提取一致，parser 才是权威口径（见 readCalendar）。
const (
	termPageFixture = `<html><body><div>2026-2027-1</div><input id="kbjcmsid" value="01"></body></html>`
	weekPageFixture = `<html><body><div>第 9 周 / 共 16 周</div></body></html>`
)

// termCalendar 是 fake 里的一份学期日历（term → 第 1 周周一与总周数）。
type termCalendar struct {
	TotalWeeks  int
	FirstMonday time.Time
}

// replacement 是一次成功的 ReplaceTerm 入参。
type replacement struct {
	Term        string
	TotalWeeks  int
	FirstMonday time.Time
}

// windowWrite 是一次 SetRunWindow 入参：窗口三列由 pipeline.RecordWindow 算好再落库。
type windowWrite struct {
	Start      int
	End        int
	WeeksTotal int
}

// execFake 在 stub 之上替换本项会调到的注入点，其余方法继续沿用 stub 的 panic：
// 将来若 execute 提前用到了第 7 项起才该用的依赖，用例会炸在调用点，而不是拿到零值
// 后悄悄通过。所有替换方法都同步记录到达顺序，供「执行顺序」断言使用。
type execFake struct {
	*stub

	events []string // 各注入点的到达顺序，元素是固定短名

	snapshot  settings.Snapshot // Settings.Load 的返回
	loadErr   error             // Settings.Load 的错误
	secrets   map[string]string // Reveal 能解出的明文，键是设置键
	revealErr map[string]error  // Reveal 的错误，键是设置键

	loginOpts upstream.LoginOptions // 最近一次 Login 的入参
	loginErr  error                 // Login 的返回错误

	termSnapshot *upstream.TermSnapshot // FetchTerm 的返回
	termFetchErr error                  // FetchTerm 的错误
	weekCalendar *upstream.WeekCalendar // FetchWeekCalendar 的返回
	weekFetchErr error                  // FetchWeekCalendar 的错误
	weekAt       time.Time              // FetchWeekCalendar 收到的采样时刻

	calendars    map[string]termCalendar // 已存的学期日历：只在 ReplaceTerm 成功时改写
	replaceErr   error                   // ReplaceTerm 的错误（优先于锚点比对）
	replacements []replacement           // 成功的 ReplaceTerm 入参
	replaceCalls int                     // ReplaceTerm 的调用次数（含失败）

	// replaceHook 可选：ReplaceTerm 一进来就调用，用来模拟「执行到锚点冲突这一步时
	// 调用方才取消」，覆盖 failCalendar 的 ctx 守卫（预取消只能覆盖 failRun 那条路径）。
	replaceHook func()

	termSets   []string // SetRunTerm 收到的学期
	termSetErr error    // SetRunTerm 的错误

	// windows 是本轮写进 collect_run 的窗口，按到达顺序；windowErr 覆盖「窗口写库
	// 失败」这条分支（空窗在到达写库之前就收口了，不会写窗口）。
	windows   []windowWrite
	windowErr error

	// collectSnapshot 是 LoadCollectSnapshot 的返回；collectSnapshotErr 覆盖
	// 「字典与轴没有版本行」这条执行器在发第一个矩阵请求之前的失败点。
	collectSnapshot    store.CollectSnapshot
	collectSnapshotErr error

	phase    string // 当前阶段，随 SetRunPhase 前进
	phaseErr error  // SetRunPhase 的错误
}

// newExecuteFake 返回一份「账号、密码、OCR 都齐备，学期页与周历页可解析」的替身；
// 设置值里都带 settingsSentinel，供「原因只点名键、不写值」的断言使用。
func newExecuteFake() *execFake {
	return &execFake{
		stub: &stub{},
		snapshot: settings.Snapshot{
			JWUsername:    "teacher-" + settingsSentinel,
			JWPasswordSet: true,
			OCRBaseURL:    "http://ocr-" + settingsSentinel + ".test",
			Whitelist: []model.Building{
				{ID: "Q10", Name: "综合楼"},
				{ID: "Q11", Name: "实验楼"},
			},
		},
		secrets: map[string]string{
			model.SettingJWPassword: "pw-" + settingsSentinel,
		},
		revealErr: map[string]error{},
		termSnapshot: &upstream.TermSnapshot{
			Term:     "2026-2027-1",
			KBJCMSID: "01",
			Raw:      termPageFixture,
		},
		weekCalendar: &upstream.WeekCalendar{
			Week:       9,
			TotalWeeks: 16,
			Raw:        weekPageFixture,
		},
		calendars:       map[string]termCalendar{},
		collectSnapshot: matrixFixtureSnapshot(),
		phase:           string(pipeline.PhaseLogin),
	}
}

func (f *execFake) SetRunWindow(_ context.Context, _ int64, start, end, weeksTotal int, _ time.Time) error {
	f.record("window")
	if f.windowErr != nil {
		return f.windowErr
	}
	f.windows = append(f.windows, windowWrite{Start: start, End: end, WeeksTotal: weeksTotal})
	return nil
}

func (f *execFake) LoadCollectSnapshot(_ context.Context) (store.CollectSnapshot, error) {
	f.record("snapshot")
	return f.collectSnapshot, f.collectSnapshotErr
}

func (f *execFake) record(event string) { f.events = append(f.events, event) }

// Load 返回设置快照；错误路径用来钉住「读设置失败不编 settings_incomplete」与
// 「ctx 取消不收口」。
func (f *execFake) Load(_ context.Context) (settings.Snapshot, error) {
	f.record("load")
	return f.snapshot, f.loadErr
}

func (f *execFake) Reveal(_ context.Context, key string) (string, error) {
	f.record("reveal:" + key)
	if err := f.revealErr[key]; err != nil {
		return "", err
	}
	value, ok := f.secrets[key]
	if !ok {
		return "", fmt.Errorf("设置项 %s 未设置", key)
	}
	return value, nil
}

func (f *execFake) Login(_ context.Context, opts upstream.LoginOptions) error {
	f.record("login")
	f.loginOpts = opts
	return f.loginErr
}

func (f *execFake) FetchTerm(_ context.Context) (*upstream.TermSnapshot, error) {
	f.record("fetch_term")
	if f.termFetchErr != nil {
		return nil, f.termFetchErr
	}
	return f.termSnapshot, nil
}

func (f *execFake) FetchWeekCalendar(_ context.Context, at time.Time) (*upstream.WeekCalendar, error) {
	f.record("fetch_week")
	f.weekAt = at
	if f.weekFetchErr != nil {
		return nil, f.weekFetchErr
	}
	return f.weekCalendar, nil
}

// ReplaceTerm 模拟 store 的单事务语义：只有成功才改写日历，锚点与已存日历不一致时
// 返回 store.ErrTermAnchorConflict 且一行都不动。
func (f *execFake) ReplaceTerm(_ context.Context, term string, totalWeeks int, firstMonday, _ time.Time) error {
	f.record("replace_term")
	f.replaceCalls++
	if f.replaceHook != nil {
		f.replaceHook()
	}
	if f.replaceErr != nil {
		return f.replaceErr
	}
	if existing, ok := f.calendars[term]; ok && !existing.FirstMonday.Equal(firstMonday) {
		return store.ErrTermAnchorConflict
	}
	f.calendars[term] = termCalendar{TotalWeeks: totalWeeks, FirstMonday: firstMonday}
	f.replacements = append(f.replacements, replacement{Term: term, TotalWeeks: totalWeeks, FirstMonday: firstMonday})
	return nil
}

func (f *execFake) SetRunTerm(_ context.Context, _ int64, term string, _ time.Time) error {
	f.record("bind_term")
	f.termSets = append(f.termSets, term)
	return f.termSetErr
}

func (f *execFake) LoadRunPhase(_ context.Context, _ int64) (string, string, error) {
	return f.phase, string(pipeline.StatusRunning), nil
}

func (f *execFake) SetRunPhase(_ context.Context, _ int64, phase string, _ []string, _ time.Time) error {
	f.record("advance:" + phase)
	if f.phaseErr != nil {
		return f.phaseErr
	}
	f.phase = phase
	return nil
}

// executeDeps 用替身填满 Deps；Clock 固定在 executeNow，让锚点与落库时刻可断言。
func executeDeps(f *execFake) Deps {
	return Deps{
		Runs:      f,
		Terms:     f,
		Snapshots: f,
		Phases:    f,
		Tasks:     f,
		TaskRead:  f,
		Rooms:     f,
		Reader:    f,
		Committer: f,
		Settings:  f,
		Upstream:  f,
		RunCtx:    context.Background(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     func() time.Time { return executeNow },
	}
}

// newExecutor 用替身组装一个执行器；New 的必填依赖已由第 5 项逐项覆盖，
// 这里失败说明本组用例的 Deps 自己漏填了。
func newExecutor(t *testing.T, f *execFake) *Collector {
	t.Helper()
	c, err := New(executeDeps(f))
	if err != nil {
		t.Fatalf("New 不应报错，实际 %v", err)
	}
	return c
}

// runExecute 同步跑一次执行体（不走 Start 的后台协程：启动语义已由第 5 项覆盖，
// 本组用例只关心阶段与失败映射）。
func runExecute(t *testing.T, f *execFake) error {
	t.Helper()
	return newExecutor(t, f).execute(context.Background(), executeRunID)
}

// singleFinish 断言执行体恰好收口一次，并返回那条终态。
func singleFinish(t *testing.T, f *execFake) store.RunFinish {
	t.Helper()
	finishes := f.finishCalls()
	if len(finishes) != 1 {
		t.Fatalf("执行体应恰好收口一次，实际 %d 次：%+v", len(finishes), finishes)
	}
	return finishes[0]
}

// assertFailedFinish 断言终态是 failed、错误码与收口时刻正确。
func assertFailedFinish(t *testing.T, finish store.RunFinish, wantCode string) {
	t.Helper()
	if finish.RunID != executeRunID {
		t.Fatalf("收口的轮次应为 %d，实际 %d", executeRunID, finish.RunID)
	}
	if finish.Status != string(pipeline.StatusFailed) {
		t.Fatalf("终态应为 failed，实际 %q", finish.Status)
	}
	if finish.ErrorCode != wantCode {
		t.Fatalf("error_code 期望 %q，实际 %q（原因 %q）", wantCode, finish.ErrorCode, finish.ErrorMessage)
	}
	if finish.FinishedAt.IsZero() {
		t.Fatal("失败收口应写 finished_at，实际是零值")
	}
}

// assertCalendarUntouched 断言学期日历与学期绑定都没有被改动：fake 只在 ReplaceTerm
// 成功时改写 calendars，所以「没有绑定」与「日历逐项不变」两条一起看，才覆盖
// Requirement 2.4 的「保持库里已有的学期日历不变」。
func assertCalendarUntouched(t *testing.T, f *execFake, want map[string]termCalendar) {
	t.Helper()
	if len(f.calendars) != len(want) {
		t.Fatalf("学期日历的学期数应与进入执行体时相同（%d），实际 %d：%+v", len(want), len(f.calendars), f.calendars)
	}
	for term, expected := range want {
		got, ok := f.calendars[term]
		if !ok {
			t.Fatalf("学期 %q 的日历不应被删除", term)
		}
		if got.TotalWeeks != expected.TotalWeeks || !got.FirstMonday.Equal(expected.FirstMonday) {
			t.Fatalf("学期 %q 的日历不应被改写：期望 %+v，实际 %+v", term, expected, got)
		}
	}
	if len(f.termSets) != 0 {
		t.Fatalf("失败轮次不应绑定学期，实际写入 %v", f.termSets)
	}
}

// TestExecuteFailsWithSettingsIncomplete 覆盖 Requirement 4.3：账号、密码或 OCR 地址
// 缺失/解封失败 → failed + settings_incomplete，原因只点名设置键、不写任何值。
func TestExecuteFailsWithSettingsIncomplete(t *testing.T) {
	cases := []struct {
		name        string // 子用例名
		mutate      func(*execFake)
		wantMissing []string // 期望全部出现在原因里的设置键
		wantAbsent  []string // 期望不出现在原因里的设置键
	}{
		{
			// 密码是解得开的正常项：原因里点名它说明实现把「没缺的键」也报了，
			// 管理员会去修一个本来好好的设置项，所以三个密码正常的子用例都反向断言一次。
			name:        "账号为空",
			mutate:      func(f *execFake) { f.snapshot.JWUsername = "  " },
			wantMissing: []string{model.SettingJWUsername},
			wantAbsent:  []string{model.SettingJWPassword},
		},
		{
			name: "密码解封失败",
			mutate: func(f *execFake) {
				f.revealErr[model.SettingJWPassword] = errors.New("密钥不可用")
			},
			wantMissing: []string{model.SettingJWPassword},
		},
		{
			name:        "OCR 地址为空",
			mutate:      func(f *execFake) { f.snapshot.OCRBaseURL = "" },
			wantMissing: []string{model.SettingOCRBaseURL},
			wantAbsent:  []string{model.SettingJWPassword},
		},
		{
			// 多键一次点名：管理员「修一个、跑一次」是最费时间的路径，loadRoundSettings
			// 必须在同一条原因里把缺的键都写出来，而不是遇到第一个就返回。
			name: "账号与 OCR 同时为空",
			mutate: func(f *execFake) {
				f.snapshot.JWUsername = "  "
				f.snapshot.OCRBaseURL = " "
			},
			wantMissing: []string{model.SettingJWUsername, model.SettingOCRBaseURL},
			wantAbsent:  []string{model.SettingJWPassword},
		},
		{
			// 账号与密码同时缺失：Reveal 失败不短路账号那一支，两个键该一起点名；
			// OCR 地址正常，所以它也不该被报进来。
			name: "账号与密码同时缺失",
			mutate: func(f *execFake) {
				f.snapshot.JWUsername = " "
				f.revealErr[model.SettingJWPassword] = errors.New("密钥不可用")
			},
			wantMissing: []string{model.SettingJWUsername, model.SettingJWPassword},
			wantAbsent:  []string{model.SettingOCRBaseURL},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecuteFake()
			tc.mutate(f)

			err := runExecute(t, f)
			if err == nil {
				t.Fatal("设置不完整时执行体应返回错误供 run() 记 WARN")
			}

			finish := singleFinish(t, f)
			assertFailedFinish(t, finish, codeSettingsIncomplete)
			for _, key := range tc.wantMissing {
				if !strings.Contains(finish.ErrorMessage, key) {
					t.Fatalf("原因应点名缺失的设置键 %q，实际 %q", key, finish.ErrorMessage)
				}
			}
			for _, key := range tc.wantAbsent {
				if strings.Contains(finish.ErrorMessage, key) {
					t.Fatalf("原因不应点名没缺的设置键 %q，实际 %q", key, finish.ErrorMessage)
				}
			}
			if strings.Contains(finish.ErrorMessage, settingsSentinel) {
				t.Fatalf("原因不应写入任何设置值（含哨兵 %q），实际 %q", settingsSentinel, finish.ErrorMessage)
			}

			if slices.Contains(f.events, "login") {
				t.Fatalf("设置不完整时不应尝试登录，实际调用序列 %v", f.events)
			}
			assertCalendarUntouched(t, f, map[string]termCalendar{})
		})
	}
}

// TestExecuteFailsWithoutCodeWhenSettingsLoadFails 覆盖「设置读不出来」与「配置没配齐」
// 的分流（design 错误码表：非结构失败不编码）：健康 ctx 下 Settings.Load 的基础设施
// 失败收口为 failed、error_code 为空，原因取仓储文案，学期日历一行不动。
//
// 与 TestExecuteKeepsRunRunningWhenContextCanceled 的区别在于 ctx：那条故意用预取消的
// ctx，failRun 先返回、根本走不到 FinishRun。所以「无码收口」必须用健康 ctx 单独钉一次，
// 否则这条路径只有「不收口」的用例、没有「收口」的用例。
func TestExecuteFailsWithoutCodeWhenSettingsLoadFails(t *testing.T) {
	f := newExecuteFake()
	f.loadErr = errors.New("数据库连接已断开")
	existing := termCalendar{
		TotalWeeks:  16,
		FirstMonday: time.Date(2026, 9, 7, 0, 0, 0, 0, shanghai),
	}
	f.calendars["2026-2027-1"] = existing

	if err := runExecute(t, f); err == nil {
		t.Fatal("设置读取失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "数据库连接已断开") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}
	if slices.Contains(f.events, "login") {
		t.Fatalf("设置读取失败时不应尝试登录，实际调用序列 %v", f.events)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{"2026-2027-1": existing})
}

// TestExecuteReportsFailRunErrorWhenFinishFails 覆盖 failRun 的「收口本身失败」分支：
// 设置未配齐的同时 FinishRun 也失败，执行体应把两个原因一起返回（errors.Is 仍能追到
// errSettingsIncomplete），只为终态尝试一次收口，日历一行不动。
//
// 与 failCalendar 的同类用例靠文案区分：这里必须带 failRun 的「收口失败」而不带
// 「日历失败」，才钉住 settings_incomplete 走的是可空码的 failRun，而不是固定码的
// failCalendar。
func TestExecuteReportsFailRunErrorWhenFinishFails(t *testing.T) {
	f := newExecuteFake()
	f.snapshot.JWUsername = "  " // 只留账号缺失：收口码是 settings_incomplete
	f.finishErr = errors.New("写终态时数据库连接已断开")

	err := runExecute(t, f)
	if err == nil {
		t.Fatal("收口失败时执行体应返回错误供 run() 记 WARN")
	}
	if !errors.Is(err, errSettingsIncomplete) {
		t.Fatalf("返回的错误应仍能追到 errSettingsIncomplete，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "收口失败") {
		t.Fatalf("返回的错误应说明收口本身失败，实际 %q", err.Error())
	}
	if strings.Contains(err.Error(), "日历失败") {
		t.Fatalf("settings_incomplete 应走 failRun 而不是 failCalendar，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "写终态时数据库连接已断开") {
		t.Fatalf("返回的错误应保留收口失败的原因，实际 %q", err.Error())
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, codeSettingsIncomplete)
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsOnAuthFailed 覆盖 Requirement 10.2：上游判定密码错误 →
// failed + auth_failed，不抓学期页，日历不变。
func TestExecuteFailsOnAuthFailed(t *testing.T) {
	f := newExecuteFake()
	f.loginErr = fmt.Errorf("%w: 上游返回密码错误标记", model.ErrUpstreamAuthFailed)

	if err := runExecute(t, f); err == nil {
		t.Fatal("登录失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, codeAuthFailed)
	if !strings.Contains(finish.ErrorMessage, "密码错误") {
		t.Fatalf("原因应保留上游的失败文案，实际 %q", finish.ErrorMessage)
	}
	if slices.Contains(f.events, "fetch_term") {
		t.Fatalf("登录失败后不应读取学期页，实际调用序列 %v", f.events)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsOnOCRFailed 覆盖 Requirement 10.3：验证码识别耗尽 →
// failed + ocr_failed。
func TestExecuteFailsOnOCRFailed(t *testing.T) {
	f := newExecuteFake()
	f.loginErr = fmt.Errorf("%w: 累计 3 次验证码尝试后仍失败", model.ErrUpstreamOCRFailed)

	if err := runExecute(t, f); err == nil {
		t.Fatal("验证码失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, codeOCRFailed)
	if slices.Contains(f.events, "fetch_term") {
		t.Fatalf("登录失败后不应读取学期页，实际调用序列 %v", f.events)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsWithoutCodeOnLoginNetworkError 钉住「非结构失败不编码」：
// 网络/超时导致的登录失败收口为 failed、error_code 为空，原因取错误文案。
func TestExecuteFailsWithoutCodeOnLoginNetworkError(t *testing.T) {
	f := newExecuteFake()
	f.loginErr = fmt.Errorf("%w: 连接超时", model.ErrUpstreamFailed)

	if err := runExecute(t, f); err == nil {
		t.Fatal("登录失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "连接超时") {
		t.Fatalf("原因应取错误文案，实际 %q", finish.ErrorMessage)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsWithoutCodeWhenPhaseAdvanceFails 覆盖 AdvancePhase 失败分支：登录成功
// 后推进阶段写库失败 → failed、error_code 为空（写库失败不编码），原因取仓储文案；
// 学期页一次都不读（阶段没推进就不进入下一阶段的内容），日历不动。
func TestExecuteFailsWithoutCodeWhenPhaseAdvanceFails(t *testing.T) {
	f := newExecuteFake()
	f.phaseErr = errors.New("阶段更新撞上了 collect_run 的约束")

	if err := runExecute(t, f); err == nil {
		t.Fatal("推进阶段失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "阶段更新撞上了 collect_run 的约束") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}
	if slices.Contains(f.events, "fetch_term") {
		t.Fatalf("推进阶段失败后不应读取学期页，实际调用序列 %v", f.events)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsOnCalendarTextInvalid 覆盖 Requirement 2.4 的第一种情形：
// parser 文本非法或双口径不一致 → failed + calendar_invalid，学期日历不被改动。
func TestExecuteFailsOnCalendarTextInvalid(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*execFake)
		wantReason string // 可选：原因里应出现的片段，用来区分同一错误码下的不同分支
	}{
		{
			name: "学期页缺少学期编号",
			mutate: func(f *execFake) {
				f.termSnapshot.Raw = `<html><body><input id="kbjcmsid" value="01"></body></html>`
			},
		},
		{
			name: "学期编号双口径不一致",
			mutate: func(f *execFake) {
				f.termSnapshot.Raw = `<html><body><div>2026-2027-2</div><input id="kbjcmsid" value="01"></body></html>`
			},
		},
		{
			name: "kbjcmsid 双口径不一致",
			mutate: func(f *execFake) {
				// 学期编号两条口径一致，只有 kbjcmsid 变成 02（最小提取仍是 01）：
				// 钉住 readCalendar 里那一条独立比较，而不是被学期编号分支顺手覆盖。
				f.termSnapshot.Raw = `<html><body><div>2026-2027-1</div><input id="kbjcmsid" value="02"></body></html>`
			},
			wantReason: "kbjcmsid",
		},
		{
			name: "周历双口径不一致",
			mutate: func(f *execFake) {
				f.weekCalendar.Raw = `<html><body><div>第 8 周 / 共 16 周</div></body></html>`
			},
		},
		{
			name: "周历缺少周次文本",
			mutate: func(f *execFake) {
				f.weekCalendar.Raw = `<html><body><div>没有周次</div></body></html>`
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecuteFake()
			tc.mutate(f)

			if err := runExecute(t, f); err == nil {
				t.Fatal("学期/周历非法时执行体应返回错误")
			}

			finish := singleFinish(t, f)
			assertFailedFinish(t, finish, "calendar_invalid")
			if tc.wantReason != "" && !strings.Contains(finish.ErrorMessage, tc.wantReason) {
				t.Fatalf("原因应点名 %q 这条分支，实际 %q", tc.wantReason, finish.ErrorMessage)
			}
			if f.replaceCalls != 0 {
				t.Fatalf("文本非法时不应写学期日历，实际调用 ReplaceTerm %d 次", f.replaceCalls)
			}
			assertCalendarUntouched(t, f, map[string]termCalendar{})
		})
	}
}

// TestExecuteFailsOnCalendarInvalidFromUpstream 覆盖「上游最小提取自己就判非法」这一条：
// FetchTerm 返回 model.ErrUpstreamCalendarInvalid（缺学期编号、kbjcmsid 或周次越界）时，
// readCalendar 原样带回，执行体走 FailCalendar（calendar_invalid），不再请求周历页。
func TestExecuteFailsOnCalendarInvalidFromUpstream(t *testing.T) {
	f := newExecuteFake()
	f.termFetchErr = fmt.Errorf("%w: 学期页缺少学期编号", model.ErrUpstreamCalendarInvalid)

	if err := runExecute(t, f); err == nil {
		t.Fatal("学期页非法时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "calendar_invalid")
	if !strings.Contains(finish.ErrorMessage, "缺少学期编号") {
		t.Fatalf("原因应保留上游文案，实际 %q", finish.ErrorMessage)
	}
	if slices.Contains(f.events, "fetch_week") {
		t.Fatalf("学期页已判非法时不应再读周历页，实际调用序列 %v", f.events)
	}
	if f.replaceCalls != 0 {
		t.Fatalf("学期页非法时不应写学期日历，实际调用 ReplaceTerm %d 次", f.replaceCalls)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsWithoutCodeOnCalendarFetchNetworkError 钉住学期/周历请求的网络失败
// 没有业务码（design「非结构失败不编码」）：FetchTerm 与 FetchWeekCalendar 各一条，
// 收口 failed、error_code 为空，原因取错误文案，学期日历不动。
func TestExecuteFailsWithoutCodeOnCalendarFetchNetworkError(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*execFake)
	}{
		{
			name: "学期页网络错误",
			mutate: func(f *execFake) {
				f.termFetchErr = fmt.Errorf("%w: 读取学期页超时", model.ErrUpstreamFailed)
			},
		},
		{
			name: "周历页网络错误",
			mutate: func(f *execFake) {
				f.weekFetchErr = fmt.Errorf("%w: 读取周历页超时", model.ErrUpstreamFailed)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecuteFake()
			tc.mutate(f)

			if err := runExecute(t, f); err == nil {
				t.Fatal("网络失败时执行体应返回错误")
			}

			finish := singleFinish(t, f)
			assertFailedFinish(t, finish, "")
			if !strings.Contains(finish.ErrorMessage, "超时") {
				t.Fatalf("原因应取错误文案，实际 %q", finish.ErrorMessage)
			}
			if f.replaceCalls != 0 {
				t.Fatalf("网络失败时不应写学期日历，实际调用 ReplaceTerm %d 次", f.replaceCalls)
			}
			assertCalendarUntouched(t, f, map[string]termCalendar{})
		})
	}
}

// TestExecuteFailsOnTermAnchorConflict 覆盖 Requirement 2.4 的第二种情形与 design 第 4 节：
// 已存日历的第 1 周周一与新算值不同 → failed + calendar_invalid，原日历一行不动，
// 学期也不绑定。
func TestExecuteFailsOnTermAnchorConflict(t *testing.T) {
	f := newExecuteFake()
	existing := termCalendar{
		TotalWeeks:  16,
		FirstMonday: time.Date(2026, 8, 31, 0, 0, 0, 0, shanghai), // 比新算的 2026-09-07 早一周
	}
	f.calendars["2026-2027-1"] = existing

	if err := runExecute(t, f); err == nil {
		t.Fatal("锚点冲突时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "calendar_invalid")
	if f.replaceCalls != 1 {
		t.Fatalf("锚点冲突前应恰好尝试写一次日历，实际 %d 次", f.replaceCalls)
	}
	if len(f.replacements) != 0 {
		t.Fatalf("锚点冲突时不应写入任何周行，实际写入 %+v", f.replacements)
	}
	if slices.Contains(f.events, "bind_term") {
		t.Fatalf("锚点冲突后不应绑定学期，实际调用序列 %v", f.events)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{"2026-2027-1": existing})
}

// TestExecuteReportsFailCalendarErrorWhenFinishFails 覆盖 failCalendar 的「收口本身失败」
// 分支：锚点冲突的同时 FinishRun 也失败，执行体应把两个原因一起返回（errors.Is 仍能
// 追到 ErrTermAnchorConflict），只为终态尝试一次收口，原日历一行不动。
//
// 「只收口一次」是这条用例的重点：收口失败不重试，重试只会再撞一次 FinishRun 的
// status='running' 守卫，也不会把第一次写下的终态改写成别的错。
func TestExecuteReportsFailCalendarErrorWhenFinishFails(t *testing.T) {
	f := newExecuteFake()
	existing := termCalendar{
		TotalWeeks:  16,
		FirstMonday: time.Date(2026, 8, 31, 0, 0, 0, 0, shanghai), // 比新算的 2026-09-07 早一周
	}
	f.calendars["2026-2027-1"] = existing
	f.finishErr = errors.New("写终态时数据库连接已断开")

	err := runExecute(t, f)
	if err == nil {
		t.Fatal("收口失败时执行体应返回错误供 run() 记 WARN")
	}
	if !errors.Is(err, store.ErrTermAnchorConflict) {
		t.Fatalf("返回的错误应仍能追到锚点冲突，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "日历失败收口失败") {
		t.Fatalf("返回的错误应说明日历收口本身失败，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "写终态时数据库连接已断开") {
		t.Fatalf("返回的错误应保留收口失败的原因，实际 %q", err.Error())
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "calendar_invalid")
	assertCalendarUntouched(t, f, map[string]termCalendar{"2026-2027-1": existing})
}

// TestExecuteKeepsRunRunningWhenCanceledAtAnchorConflict 覆盖 failCalendar 的 ctx 守卫：
// 取消发生在锚点冲突这一刻（ReplaceTerm 返回 ErrTermAnchorConflict，同时调用方取消），
// 执行体应返回 context.Canceled、0 次收口，原日历一行不动。
//
// 预取消的 ctx 会在 Load 处就短路（见 TestExecuteKeepsRunRunningWhenContextCanceled），
// 根本到不了 FailCalendar；这里用 replaceHook 在到达冲突分支时才取消，才真正钉住
// 「锚点冲突 + ctx 已取消 → 不收口」这条组合。
func TestExecuteKeepsRunRunningWhenCanceledAtAnchorConflict(t *testing.T) {
	f := newExecuteFake()
	existing := termCalendar{
		TotalWeeks:  16,
		FirstMonday: time.Date(2026, 8, 31, 0, 0, 0, 0, shanghai), // 比新算的 2026-09-07 早一周
	}
	f.calendars["2026-2027-1"] = existing

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.replaceHook = cancel

	c := newExecutor(t, f)
	err := c.execute(ctx, executeRunID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后执行体应返回 context.Canceled，实际 %v", err)
	}
	if f.replaceCalls != 1 {
		t.Fatalf("取消前应恰好尝试写一次日历，实际 %d 次", f.replaceCalls)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("取消后不应收口（轮次应保持 running），实际收口 %+v", finishes)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{"2026-2027-1": existing})
}

// TestExecuteFailsWithoutCodeWhenCalendarWriteFails 覆盖 design 错误表「学期日历写库失败」：
// 事务已回滚，收口 failed 但不编码。
func TestExecuteFailsWithoutCodeWhenCalendarWriteFails(t *testing.T) {
	f := newExecuteFake()
	f.replaceErr = errors.New("数据库连接已断开")

	if err := runExecute(t, f); err == nil {
		t.Fatal("写日历失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "数据库连接已断开") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}
	assertCalendarUntouched(t, f, map[string]termCalendar{})
}

// TestExecuteFailsWithoutCodeWhenTermBindFails 覆盖 RecordTerm 失败分支：学期日历已整体
// 替换成功（事务已提交），绑定学期写库失败 → failed、error_code 为空，原因取仓储文案；
// 终态只收口一次，日历的写入保留（ReplaceTerm 先于 RecordTerm，设计顺序见 execute）。
func TestExecuteFailsWithoutCodeWhenTermBindFails(t *testing.T) {
	f := newExecuteFake()
	f.termSetErr = errors.New("绑定学期时数据库连接已断开")

	if err := runExecute(t, f); err == nil {
		t.Fatal("绑定学期失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "绑定学期时数据库连接已断开") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}
	if len(f.replacements) != 1 {
		t.Fatalf("绑定学期失败前应已成功写一次学期日历，实际 %d 次：%+v", len(f.replacements), f.replacements)
	}
	if !slices.Equal(f.termSets, []string{"2026-2027-1"}) {
		t.Fatalf("学期绑定应只尝试一次，实际 %v", f.termSets)
	}
}

// TestExecuteBindsTermAfterWritingCalendar 覆盖 happy path 的阶段顺序与取值：
// 读设置 → 解封密码 → 登录 → 推进阶段 → 学期页 → 周历页 → 写日历 → 绑定学期；
// 锚点由 parser 的 C 与同一个执行时刻推出，学期与总周数原样落库，成功轮次不收口。
func TestExecuteBindsTermAfterWritingCalendar(t *testing.T) {
	f := newExecuteFake()
	// 账号与 OCR 地址故意带前后空白：loadRoundSettings 会再 trim 一次，登录入参必须是
	// trim 后的值；密码是秘密项、按原样传入，不受这次 trim 影响——所以也让密码带上
	// 前后空白，用同一串进参钉住「密码不被 trim」。
	f.snapshot.JWUsername = "  teacher-" + settingsSentinel + "  "
	f.snapshot.OCRBaseURL = "  http://ocr-" + settingsSentinel + ".test  "
	f.secrets[model.SettingJWPassword] = "  pw-" + settingsSentinel + "  "

	if err := runExecute(t, f); err != nil {
		t.Fatalf("正常路径 execute 不应返回错误，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("学期日历写成功后本项不应收口（后续阶段还要继续），实际收口 %+v", finishes)
	}

	wantEvents := []string{
		"load",
		"reveal:" + model.SettingJWPassword,
		"login",
		"advance:" + string(pipeline.PhaseCalendar),
		"fetch_term",
		"fetch_week",
		"replace_term",
		"bind_term",
		"window",
		"snapshot",
	}
	if !slices.Equal(f.events, wantEvents) {
		t.Fatalf("阶段顺序应为 %v，实际 %v", wantEvents, f.events)
	}

	if !f.weekAt.Equal(executeNow) {
		t.Fatalf("周历页应拿到执行时刻 %v，实际 %v", executeNow, f.weekAt)
	}

	if len(f.replacements) != 1 {
		t.Fatalf("应恰好写一次学期日历，实际 %d 次", len(f.replacements))
	}
	wantFirstMonday := time.Date(2026, 9, 7, 0, 0, 0, 0, shanghai)
	got := f.replacements[0]
	if got.Term != "2026-2027-1" || got.TotalWeeks != 16 || !got.FirstMonday.Equal(wantFirstMonday) {
		t.Fatalf("学期日历入参期望 {2026-2027-1, 16, %v}，实际 %+v", wantFirstMonday, got)
	}

	if !slices.Equal(f.termSets, []string{"2026-2027-1"}) {
		t.Fatalf("学期应绑定 parser 口径的学期编号，实际 %v", f.termSets)
	}
	if f.phase != string(pipeline.PhaseCalendar) {
		t.Fatalf("阶段应推进到 calendar，实际 %q", f.phase)
	}

	// 窗口由周历口径 C=9 / Y=16 求得 9..12，落库的 weeks_total 是闭区间跨度 4。
	if want := []windowWrite{{Start: 9, End: 12, WeeksTotal: 4}}; !slices.Equal(f.windows, want) {
		t.Fatalf("窗口入参期望 %+v，实际 %+v", want, f.windows)
	}

	// 登录入参是 trim 后的值：上面故意给账号与 OCR 地址加了前后空白；密码原样传入，
	// 连前后空白一起进 Login，所以 wantOpts.Password 就是解出来的那一串。
	wantOpts := upstream.LoginOptions{
		Username:   "teacher-9f3a",
		Password:   "  pw-9f3a  ",
		OCRBaseURL: "http://ocr-9f3a.test",
	}
	if f.loginOpts != wantOpts {
		t.Fatalf("登录入参期望 %+v，实际 %+v", wantOpts, f.loginOpts)
	}
}

// TestExecuteKeepsRunRunningWhenContextCanceled 覆盖 Requirement 10.4 与 CP8：
// 调用方取消后执行体不写终态，轮次保持 running，未完成的任务留给下一次执行。
//
// 取消发生在设置读取这一步：这是本项最早的一个失败点，也是 FailRun 之前唯一必须
// 先判 ctx 的地方；后续失败点走同一个 failRun / failCalendar 守卫。
func TestExecuteKeepsRunRunningWhenContextCanceled(t *testing.T) {
	f := newExecuteFake()
	f.loadErr = context.Canceled

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newExecutor(t, f)
	err := c.execute(ctx, executeRunID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后执行体应返回 context.Canceled，实际 %v", err)
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("取消后不应收口（轮次应保持 running），实际收口 %+v", finishes)
	}
}

// TestWriteWindowSkipsRunOnEmptyWindow 覆盖 Requirement 2.3 与 CP1 的空窗分支：
// 窗口为空时本轮直接收口 skipped，不写窗口三列、不发任何请求、不收错误码。
//
// 空窗在完整 execute 里不可达（parser.WeekText 保证 1 ≤ C ≤ Y，WindowFor 此时恒非空），
// 所以这里按「写窗口」这一步单测：窗口由 pipeline.WindowFor(9, 8) 现算，保证被测的是
// WindowFor 与 writeWindow 的组合，而不是手写的零值窗口。
func TestWriteWindowSkipsRunOnEmptyWindow(t *testing.T) {
	f := newExecuteFake()
	c := newExecutor(t, f)

	window := pipeline.WindowFor(9, 8)
	if !window.Empty() {
		t.Fatalf("WindowFor(9, 8) 应是空窗，实际 %+v", window)
	}

	done, err := c.writeWindow(context.Background(), executeRunID, window)
	if err != nil {
		t.Fatalf("空窗收口不应返回错误，实际 %v", err)
	}
	if !done {
		t.Fatal("空窗应结束本轮（done=true）")
	}

	finish := singleFinish(t, f)
	if finish.RunID != executeRunID {
		t.Fatalf("收口的轮次应为 %d，实际 %d", executeRunID, finish.RunID)
	}
	if finish.Status != string(pipeline.StatusSkipped) {
		t.Fatalf("空窗终态应为 skipped，实际 %q", finish.Status)
	}
	if finish.ErrorCode != "" || finish.ErrorMessage != "" {
		t.Fatalf("skipped 不是失败，不应写错误码与原因，实际 code=%q message=%q",
			finish.ErrorCode, finish.ErrorMessage)
	}
	if !finish.FinishedAt.Equal(executeNow) {
		t.Fatalf("收口时刻应取注入的时钟 %v，实际 %v", executeNow, finish.FinishedAt)
	}

	if len(f.windows) != 0 {
		t.Fatalf("空窗不应写窗口三列，实际 %+v", f.windows)
	}
	if slices.Contains(f.events, "snapshot") {
		t.Fatalf("空窗不应继续读字典与轴，实际调用序列 %v", f.events)
	}
}

// TestWriteWindowLeavesRunRunningWhenCanceled 覆盖空窗收口的 ctx 守卫（Requirement 10.4、
// CP8）：调用方取消或进程退出时轮次保持 running，不写终态，留给下一次执行。
//
// writeWindow 的 done 只在终态真的写成功后才为 true；取消时返回 false + context.Canceled，
// 调用方据此停止后续阶段，而库里这一轮还在 running。
func TestWriteWindowLeavesRunRunningWhenCanceled(t *testing.T) {
	f := newExecuteFake()
	c := newExecutor(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done, err := c.writeWindow(ctx, executeRunID, pipeline.WindowFor(9, 8))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("取消后应返回 context.Canceled，实际 %v", err)
	}
	if done {
		t.Fatal("终态没写成时 done 必须为 false")
	}
	if finishes := f.finishCalls(); len(finishes) != 0 {
		t.Fatalf("取消后不应收口（轮次应保持 running），实际收口 %+v", finishes)
	}
}

// TestWriteWindowReportsErrorWhenFinishFails 覆盖空窗收口的「收口本身失败」分支：
// FinishRun 写终态失败时 writeWindow 必须把原因返回、done 保持 false，且只尝试收口
// 一次——重试只会再撞一次 FinishRun 的 status='running' 守卫，也不会改写已经写下的
// 终态。调用方拿到 error 后停止后续阶段，库里这一轮仍是 running。
func TestWriteWindowReportsErrorWhenFinishFails(t *testing.T) {
	f := newExecuteFake()
	f.finishErr = errors.New("写终态时数据库连接已断开")
	c := newExecutor(t, f)

	done, err := c.writeWindow(context.Background(), executeRunID, pipeline.WindowFor(9, 8))
	if err == nil {
		t.Fatal("空窗收口失败时 writeWindow 应返回错误，供 run() 记 WARN")
	}
	if done {
		t.Fatal("终态没写成时 done 必须为 false（这一轮并没有真正结束）")
	}
	if !strings.Contains(err.Error(), "空窗收口失败") {
		t.Fatalf("错误应说明空窗收口本身失败，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), "写终态时数据库连接已断开") {
		t.Fatalf("错误应保留收口失败的原因，实际 %q", err.Error())
	}

	// 只尝试一次：stub 在返回错误之前就记下这次调用，所以「条数」就是尝试次数。
	if finishes := f.finishCalls(); len(finishes) != 1 {
		t.Fatalf("收口失败不重试，应只尝试一次 FinishRun，实际 %d 次：%+v", len(finishes), finishes)
	}
	if len(f.windows) != 0 {
		t.Fatalf("空窗不应写窗口三列，实际 %+v", f.windows)
	}
}

// TestWriteWindowFailsRunWhenWriteFails 覆盖窗口落库失败：failed、不写业务码，
// 原因取仓储文案。窗口写库失败与快照失败同属「基础设施失败不编码」。
func TestWriteWindowFailsRunWhenWriteFails(t *testing.T) {
	f := newExecuteFake()
	f.windowErr = errors.New("写窗口时数据库连接已断开")
	c := newExecutor(t, f)

	done, err := c.writeWindow(context.Background(), executeRunID, pipeline.WindowFor(9, 16))
	if err == nil {
		t.Fatal("窗口写库失败时 writeWindow 应返回错误")
	}
	if done {
		t.Fatal("窗口写库失败不表示本轮已按 skipped 结束")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "写窗口时数据库连接已断开") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}
}

// TestExecuteFailsWithoutCodeWhenSnapshotReadFails 覆盖 design 错误码表的「快照」行：
// dict / axis 没有版本行时 failed、error_code 为空、原因取仓储文案，且队列里不会出现
// 任何矩阵请求——窗口已经写好，失败发生在发出第一个上游请求之前。
func TestExecuteFailsWithoutCodeWhenSnapshotReadFails(t *testing.T) {
	f := newExecuteFake()
	f.collectSnapshotErr = fmt.Errorf("%w: 字典表没有版本行，无法采集", model.ErrQueryFailed)

	if err := runExecute(t, f); err == nil {
		t.Fatal("快照读取失败时执行体应返回错误")
	}

	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if !strings.Contains(finish.ErrorMessage, "字典表没有版本行") {
		t.Fatalf("原因应取仓储错误文案，实际 %q", finish.ErrorMessage)
	}

	// 窗口已落在快照之前：快照失败不该把已钉住的窗口撤掉，也不该再往前发请求。
	if want := []windowWrite{{Start: 9, End: 12, WeeksTotal: 4}}; !slices.Equal(f.windows, want) {
		t.Fatalf("快照失败前应已写窗口 %+v，实际 %+v", want, f.windows)
	}
	if last := f.events[len(f.events)-1]; last != "snapshot" {
		t.Fatalf("快照失败应是本项最后一步，实际调用序列 %v", f.events)
	}
}

// TestLoadRoundSettingsNormalizesWhitelist 覆盖 Requirement 4.1–4.2：白名单与登录入参
// 来自同一次 Settings.Load；楼编号去两侧空白、按首次出现去重、保持提交顺序，
// 且 ids() 给出的规划/执行序列与规范化后的条目同序。
func TestLoadRoundSettingsNormalizesWhitelist(t *testing.T) {
	f := newExecuteFake()
	f.snapshot.Whitelist = []model.Building{
		{ID: "  Q10  ", Name: "综合楼"},
		{ID: "Q11", Name: "实验楼"},
		{ID: " Q10 ", Name: "重复条目"},
		{ID: "Q03", Name: "文史楼"},
	}
	c := newExecutor(t, f)

	rs, err := c.loadRoundSettings(context.Background())
	if err != nil {
		t.Fatalf("设置齐备时不应报错，实际 %v", err)
	}

	wantIDs := []string{"Q10", "Q11", "Q03"}
	if got := rs.ids(); !slices.Equal(got, wantIDs) {
		t.Fatalf("规范化后的楼编号期望 %v，实际 %v", wantIDs, got)
	}
	if len(rs.Buildings) != len(wantIDs) {
		t.Fatalf("白名单应有 %d 条，实际 %d 条：%+v", len(wantIDs), len(rs.Buildings), rs.Buildings)
	}
	if rs.Buildings[0].Name != "综合楼" {
		t.Fatalf("首次出现的展示名应保留，实际 %q", rs.Buildings[0].Name)
	}

	// 一次调用只读一次设置：登录入参与白名单来自同一份快照，不能各读一次各拿一份。
	loads := 0
	for _, event := range f.events {
		if event == "load" {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("读一轮设置应只 Load 一次，实际 %d 次（%v）", loads, f.events)
	}
}

// TestNormalizeBuildings 表驱动钉住白名单规范化的三条规则：trim、按首次出现去重、
// 保持提交顺序；空列表与空编号两种边界也在这里固定下来。
func TestNormalizeBuildings(t *testing.T) {
	cases := []struct {
		name      string
		entries   []model.Building
		wantIDs   []string
		wantNames []string
	}{
		{
			name: "去空白、去重、保序",
			entries: []model.Building{
				{ID: " Q10 ", Name: "综合楼"},
				{ID: "Q11", Name: "实验楼"},
				{ID: "Q10", Name: "重复条目"},
				{ID: "\tQ03\n", Name: "文史楼"},
			},
			wantIDs:   []string{"Q10", "Q11", "Q03"},
			wantNames: []string{"综合楼", "实验楼", "文史楼"},
		},
		{
			name:    "空名单",
			entries: nil,
			wantIDs: nil,
		},
		{
			// 空编号保留：设置保存时已经拒绝空编号，库里出现它只可能是手工写库，
			// 交给 pipeline.Run 的 normalizeWhitelist 拒绝并让该轮失败。
			name:      "trim 后为空的编号保留给下游拒绝",
			entries:   []model.Building{{ID: "   ", Name: "坏条目"}},
			wantIDs:   []string{""},
			wantNames: []string{"坏条目"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBuildings(tc.entries)
			ids := make([]string, 0, len(got))
			names := make([]string, 0, len(got))
			for _, building := range got {
				ids = append(ids, building.ID)
				names = append(names, building.Name)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Fatalf("楼编号期望 %v，实际 %v", tc.wantIDs, ids)
			}
			if !slices.Equal(names, tc.wantNames) {
				t.Fatalf("展示名期望 %v，实际 %v", tc.wantNames, names)
			}
		})
	}
}

// TestEmptyBuildingIDFailsRun 钉住空编号的去向，只覆盖三件事：保留、下游拒绝、收口口径。
// normalizeBuildings 不替下游静默丢弃 trim 后为空的编号，而是原样保留；直接调用
// pipeline.Run 时，入口的 normalizeWhitelist 拒绝它；执行器则按无码失败收口整轮，
// 原因点名「空的教学楼编号」。
//
// 这三段是本函数手工接起来的，不是执行体真的跑到了第 8 项：execute 目前到快照为止就
// 返回，「白名单 → pipeline.Run」的接续属于第 8 项。所以这里钉的是三段各自的形状，
// 端到端等第 8 项——空编号在真实执行体里应当让整轮 failed，但那条路径现在还没有入口。
//
// 空编号只可能来自手工写库（设置保存端已拒绝），所以显式失败比悄悄少采一栋楼更容易
// 被发现；这条用例把「保留」与「下游拒绝并收口」两段接起来，避免任何一段被改掉时
// 另一方仍默默通过。
func TestEmptyBuildingIDFailsRun(t *testing.T) {
	f := newExecuteFake()
	f.snapshot.Whitelist = []model.Building{
		{ID: "Q10", Name: "综合楼"},
		{ID: "   ", Name: "手工写入的空编号楼"},
	}
	c := newExecutor(t, f)

	rs, err := c.loadRoundSettings(context.Background())
	if err != nil {
		t.Fatalf("设置齐备时不应报错，实际 %v", err)
	}
	wantIDs := []string{"Q10", ""}
	if ids := rs.ids(); !slices.Equal(ids, wantIDs) {
		t.Fatalf("空编号应原样保留给下游拒绝，期望 %v，实际 %v", wantIDs, ids)
	}

	// 第 8 项的调用形状：规范化后的白名单直接交给 pipeline.Run。替身若被落任务或
	// 推进阶段会 panic，所以「一条任务都没写」由下面的到达序列一起证明。
	planned := []pipeline.Task{{Kind: pipeline.TaskWeekMatrix, Required: true, BuildingID: "Q10", Week: 9}}
	_, runErr := pipeline.Run(context.Background(), executeRunID, rs.ids(), planned, pipeline.RunDeps{
		Phases: f,
		Tasks:  f,
		Rooms:  f,
		Fetch: func(context.Context, pipeline.Task) (pipeline.TaskFetch, error) {
			return pipeline.TaskFetch{}, nil
		},
		Clock: c.clock,
	})
	if runErr == nil {
		t.Fatal("空编号应让 pipeline.Run 拒绝整轮")
	}
	if !strings.Contains(runErr.Error(), "空的教学楼编号") {
		t.Fatalf("拒绝原因应点名空编号，实际 %q", runErr.Error())
	}
	if want := []string{"load", "reveal:" + model.SettingJWPassword}; !slices.Equal(f.events, want) {
		t.Fatalf("拒绝应发生在任何落库/推进之前，实际到达序列 %v", f.events)
	}

	// 执行器对 pipeline 编排错误的收口口径：failed + 无业务码，原因原样落库。
	if err := c.failRun(context.Background(), executeRunID, "", runErr); err == nil {
		t.Fatal("失败收口应把原始错误返回给 run() 记 WARN")
	}
	finish := singleFinish(t, f)
	assertFailedFinish(t, finish, "")
	if finish.ErrorMessage != runErr.Error() {
		t.Fatalf("失败原因应原样落库，期望 %q，实际 %q", runErr.Error(), finish.ErrorMessage)
	}
	if !strings.Contains(finish.ErrorMessage, "空的教学楼编号") {
		t.Fatalf("落库原因应点名空编号，实际 %q", finish.ErrorMessage)
	}
}

// 编译期确认替身覆盖本项会用到的注入点：少一个方法时这里先报错。
var (
	_ settingsSource      = (*execFake)(nil)
	_ upstreamAPI         = (*execFake)(nil)
	_ TermCalendar        = (*execFake)(nil)
	_ RunLifecycle        = (*execFake)(nil)
	_ SnapshotReader      = (*execFake)(nil)
	_ pipeline.PhaseStore = (*execFake)(nil)
)
