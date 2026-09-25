package collector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/cas"
	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

// WeekFailure 单周失败信息（同学期已有上一版数据时可写入 manifest）。
type WeekFailure struct {
	Week        int
	Code        string
	AttemptAt   string
	HadPrevious bool // 该周在 data/ 或 git 中存在上一版文件
}

// RoomCore 白名单过滤并分组后的房间身份（引用对应解析行）。
type RoomCore struct {
	Jsbh    string
	Name    string
	GroupID string
	row     *ParsedRow
}

// Candidate 一轮采集的候选结果（仓库外临时目录 + 内存 manifest 草稿）。
type Candidate struct {
	Dir          string // 候选目录（结构同 data/）
	Term         string
	TotalWeeks   int
	CurrentWeek  int
	InCalendar   bool
	Config       *Config
	Manifest     *Manifest
	SuccessWeeks []int // 本轮新采集成功的周（升序）
	Failures     []WeekFailure
	SwitchTerm   bool   // 本轮是否切换学期（发布时删除旧学期目录）
	OldTerm      string // SwitchTerm 时被替换的旧学期
	GroupCounts  map[string]int
}

// Collect 执行整轮采集：登录（≤2 轮）→ 学期/教学周 → 按目标周逐周顺序请求 →
// 白名单过滤分组 → 候选周快照与 manifest 草稿写入仓库外临时目录。
// 不触碰 data/、不做 git 操作。
//
// 返回 InCalendar=false 表示当前不在教学周历内（本轮跳过采集发布，非失败）。
func Collect(ctx context.Context, cfg *Config, up *Upstream, opts Options) (*Candidate, error) {
	if opts.Username == "" || opts.Password == "" {
		return nil, newGlobal(CodeLoginFailed, "未配置 QFNU_USERNAME/QFNU_PASSWORD（从 .env 读取）")
	}
	if err := loginUpstream(ctx, up, opts.Username, opts.Password); err != nil {
		return nil, err
	}
	term, cal, err := fetchTermCalendar(ctx, up)
	if err != nil {
		return nil, err
	}
	// 不在教学周历内不是失败：调用方跳过发布，也不发告警。
	if !cal.InCalendar {
		logger.Info("当前不在教学周历内（学期=%s），本轮跳过采集发布", term)
		return &Candidate{Term: term, InCalendar: false}, nil
	}
	return collectInCalendar(ctx, cfg, up, opts, term, cal)
}

func fetchTermCalendar(ctx context.Context, up *Upstream) (string, CalendarInfo, error) {
	term, err := up.fetchTerm(ctx)
	if err != nil {
		return "", CalendarInfo{}, err
	}
	cal, err := up.fetchCalendar(ctx)
	if err != nil {
		return "", CalendarInfo{}, err
	}
	return term, cal, nil
}

type weekCollectState struct {
	groupCounts map[string]int
	haveCounts  bool
	generated   string
	compactTS   string
}

func collectInCalendar(ctx context.Context, cfg *Config, up *Upstream, opts Options, term string, cal CalendarInfo) (*Candidate, error) {
	baseline := loadLocalManifest(opts)
	switchTerm := baseline != nil && baseline.Term != "" && baseline.Term != term
	// 没有基线或学期变了，必须采全学期；否则只采当前周和未来 4 周。
	targetWeeks := planWeeks(cal, switchTerm || baseline == nil)
	if len(targetWeeks) == 0 {
		return nil, newGlobal(CodeCalendarParse, "目标周集合为空：当前周=%d 总周数=%d", cal.CurrentWeek, cal.TotalWeeks)
	}
	now := nowBJ()
	cand := &Candidate{
		Term:        term,
		TotalWeeks:  cal.TotalWeeks,
		CurrentWeek: cal.CurrentWeek,
		InCalendar:  true,
		Config:      cfg,
		SwitchTerm:  switchTerm,
		OldTerm:     oldTermOf(baseline),
		GroupCounts: map[string]int{},
	}
	state := &weekCollectState{
		groupCounts: map[string]int{},
		generated:   formatRFC3339BJ(now),
		compactTS:   formatCompactBJ(now),
	}
	if err := collectTargetWeeks(ctx, cfg, up, opts, cand, targetWeeks, state); err != nil {
		return nil, err
	}
	return finishCollect(opts, cand, baseline, cal, now, state)
}

func collectTargetWeeks(ctx context.Context, cfg *Config, up *Upstream, opts Options, cand *Candidate, weeks []int, state *weekCollectState) error {
	for i, week := range weeks {
		if i > 0 && !sleepCtx(ctx, weekJitterDelay()) {
			return newGlobal(CodeRequestFailed, "上下文取消（周间等待）")
		}
		if err := collectOneWeek(ctx, cfg, up, opts, cand, week, state); err != nil {
			return err
		}
	}
	return nil
}

func collectOneWeek(ctx context.Context, cfg *Config, up *Upstream, opts Options, cand *Candidate, week int, state *weekCollectState) error {
	rows, qerr := up.queryWeek(ctx, cand.Term, week)
	if qerr != nil {
		return recordWeekFailure(opts, cand, week, qerr, state.generated)
	}
	return writeCollectedWeek(cfg, cand, week, rows, state)
}

func recordWeekFailure(opts Options, cand *Candidate, week int, qerr error, generated string) error {
	// 周级失败保留旧快照并继续；登录页、非法访问、结构失败会让整轮停止。
	if !isWeekScope(qerr) {
		return qerr
	}
	logger.Warn("第 %d 周请求失败（周级，保留旧数据）: %v", week, qerr)
	cand.Failures = append(cand.Failures, WeekFailure{
		Week: week, Code: errorCodeOf(qerr), AttemptAt: generated,
		HadPrevious: hasPreviousWeekFile(opts, cand.Term, week),
	})
	return nil
}

func writeCollectedWeek(cfg *Config, cand *Candidate, week int, rows []ParsedRow, state *weekCollectState) error {
	cores, counts, err := ResolveSnapshotRooms(cfg, rows)
	if err != nil {
		return err
	}
	if !state.haveCounts {
		state.groupCounts = counts
		state.haveCounts = true
	}
	snap := buildWeekSnapshot(cand.Term, week, state.generated, state.compactTS, rows, cores)
	if err := validateWeekSnapshotLocally(cfg, snap); err != nil {
		return err
	}
	jsonBytes, err := marshalIndent(snap)
	if err != nil {
		return newGlobal(CodeInternal, "序列化第 %d 周快照失败: %v", week, err)
	}
	if err := ensureCandidateDir(cand); err != nil {
		return err
	}
	if err := cand.writeWeekFile(cand.Term, week, jsonBytes); err != nil {
		return err
	}
	cand.SuccessWeeks = append(cand.SuccessWeeks, week)
	return nil
}

func ensureCandidateDir(cand *Candidate) error {
	if cand.Dir != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "easy-qfnu-kjs-collect-*")
	if err != nil {
		return newGlobal(CodeInternal, "创建候选目录失败: %v", err)
	}
	cand.Dir = dir
	return nil
}

func finishCollect(opts Options, cand *Candidate, baseline *Manifest, cal CalendarInfo, now time.Time, state *weekCollectState) (*Candidate, error) {
	if len(cand.SuccessWeeks) == 0 {
		return nil, newGlobal(CodeRequestFailed, "目标周次全部失败，无可发布数据")
	}
	// 学期切换时当前周失败就不能换学期，否则会发布一套缺当前周的新数据。
	if cand.SwitchTerm && !containsWeek(cand.SuccessWeeks, cal.CurrentWeek) {
		return nil, newGlobal(CodeTermSwitchFailed,
			"新学期 %s 切换要求当前周 %d 必须采集成功（本轮未成功），禁止切换", cand.Term, cal.CurrentWeek)
	}
	cand.GroupCounts = state.groupCounts
	if err := cand.buildManifest(opts, baseline, state.generated, state.compactTS, now, cand.Config, state.groupCounts); err != nil {
		return nil, err
	}
	logger.Info("候选生成完毕：目录=%s 学期=%s 成功周=%v 失败周=%d",
		cand.Dir, cand.Term, cand.SuccessWeeks, len(cand.Failures))
	return cand, nil
}

// loginUpstream 最多执行 2 轮 CAS 登录。
func loginUpstream(ctx context.Context, up *Upstream, username, password string) error {
	var lastErr error
	for round := 1; round <= 2; round++ {
		if err := ctx.Err(); err != nil {
			return newGlobal(CodeLoginFailed, "登录取消: %v", err)
		}
		client, ok := up.client.(*cas.Client)
		if !ok {
			return newGlobal(CodeInternal, "上游客户端类型异常")
		}
		logger.Info("CAS 登录第 %d/2 轮", round)
		if err := client.Login(ctx, username, password); err == nil {
			logger.Info("CAS 登录成功")
			return nil
		} else {
			lastErr = err
		}
		if round == 1 && !sleepCtx(ctx, 3*time.Second) {
			return newGlobal(CodeLoginFailed, "登录取消（轮间等待）")
		}
	}
	return newGlobal(CodeLoginFailed, "CAS 登录 2 轮均失败: %v", lastErr)
}

// planWeeks 计算目标周集合（Q93/§7）：
//   - 学期首次/切换：全学期逐周（切换场景当前周必须成功）；
//   - 北京时间周日：全学期；其余日期：当前周 + 未来 4 周。
func planWeeks(cal CalendarInfo, fullTerm bool) []int {
	var weeks []int
	if fullTerm || isBeijingSunday(time.Now()) {
		for w := 1; w <= cal.TotalWeeks; w++ {
			weeks = append(weeks, w)
		}
		return weeks
	}
	end := min(cal.CurrentWeek+4, cal.TotalWeeks)
	for w := cal.CurrentWeek; w <= end; w++ {
		weeks = append(weeks, w)
	}
	return weeks
}

// ResolveSnapshotRooms 把行集合过滤为白名单房间核心：
// 分组归属必须唯一（>1 → 全局失败）；每个启用分组必须命中 ≥1 房间（归零 → 全局失败）。
func ResolveSnapshotRooms(cfg *Config, rows []ParsedRow) ([]RoomCore, map[string]int, error) {
	byJsbh := map[string]*ParsedRow{}
	counts := map[string]int{}
	cores := make([]RoomCore, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		byJsbh[r.Jsbh] = r
	}
	for jsbh, r := range byJsbh {
		groupID, err := cfg.ResolveRoomGroup(r.Name)
		if err != nil {
			return nil, nil, err
		}
		if groupID == "" {
			continue // 未命中白名单的房间不发布
		}
		counts[groupID]++
		cores = append(cores, RoomCore{Jsbh: jsbh, Name: r.Name, GroupID: groupID, row: r})
	}

	enabled := cfg.EnabledGroups()
	if len(enabled) == 0 {
		return nil, nil, newGlobal(CodeGroupZero, "配置中没有任何启用分组")
	}
	for _, g := range enabled {
		if counts[g.ID] == 0 {
			return nil, nil, newGlobal(CodeGroupZero,
				"启用分组 %q(%s) 匹配到 0 个房间，禁止发布", g.Name, g.ID)
		}
	}
	sort.SliceStable(cores, func(i, j int) bool {
		if cores[i].Name != cores[j].Name {
			return naturalLess(cores[i].Name, cores[j].Name)
		}
		if cores[i].Jsbh != cores[j].Jsbh {
			return naturalLess(cores[i].Jsbh, cores[j].Jsbh)
		}
		return cores[i].GroupID < cores[j].GroupID
	})
	return cores, counts, nil
}

// buildWeekSnapshot 由行集合生成单周快照（7 天 × 12 小节展开）。
func buildWeekSnapshot(term string, week int, generated, compactTS string, rows []ParsedRow, cores []RoomCore) *WeekSnapshot {
	byJsbh := map[string]*ParsedRow{}
	for i := range rows {
		byJsbh[rows[i].Jsbh] = &rows[i]
	}
	days := map[string]*Day{}
	for d := 1; d <= 7; d++ {
		day := &Day{}
		for _, c := range cores {
			row := byJsbh[c.Jsbh]
			day.Rooms = append(day.Rooms, Room{
				Jsbh:     c.Jsbh,
				GroupID:  c.GroupID,
				Name:     c.Name,
				Statuses: row.StatusesForDay(d - 1),
			})
		}
		sortRooms(day.Rooms)
		days[strconv.Itoa(d)] = day
	}
	return &WeekSnapshot{
		SchemaVersion: SchemaVersion,
		Term:          term,
		Week:          week,
		SnapshotID:    snapshotIDFor(compactTS, week),
		GeneratedAt:   generated,
		LastSuccessAt: generated,
		Days:          days,
	}
}

// validateWeekSnapshotLocally 结构自检（schema 校验在 validate 阶段）。
func validateWeekSnapshotLocally(cfg *Config, snap *WeekSnapshot) error {
	if snap.SchemaVersion != SchemaVersion {
		return newGlobal(CodeInternal, "快照 schema_version 异常")
	}
	if len(snap.Days) != 7 {
		return newGlobal(CodeStructure, "快照天数为 %d，期望 7", len(snap.Days))
	}
	expected := map[string]bool{}
	for _, g := range cfg.EnabledGroups() {
		expected[g.ID] = true
	}
	var first []Room
	for d := 1; d <= 7; d++ {
		day := snap.Days[strconv.Itoa(d)]
		if day == nil {
			return newGlobal(CodeInternal, "快照缺少第 %d 天", d)
		}
		if len(first) == 0 {
			first = day.Rooms
		}
		if len(day.Rooms) != len(first) {
			return newGlobal(CodeStructure, "第 %d 天房间数与第 1 天不一致", d)
		}
		for i, r := range day.Rooms {
			if i >= len(first) || r.Jsbh != first[i].Jsbh {
				return newGlobal(CodeStructure, "第 %d 天房间集合与第 1 天不一致", d)
			}
			if !expected[r.GroupID] {
				return newGlobal(CodeStructure, "快照房间 %s 的 group_id 不在启用分组内", r.Jsbh)
			}
		}
	}
	return nil
}

// writeWeekFile 写入候选目录中的周快照文件。
func (c *Candidate) writeWeekFile(term string, week int, data []byte) error {
	rel := filepath.Join(TermsDirName, term, WeeksDirName, weekFileName(week))
	if err := writeFileAtomic(filepath.Join(c.Dir, rel), data, 0644); err != nil {
		return newGlobal(CodeInternal, "写入候选周文件失败: %v", err)
	}
	return nil
}

func oldTermOf(baseline *Manifest) string {
	if baseline == nil || baseline.Term == "" {
		return ""
	}
	return baseline.Term
}

func containsWeek(weeks []int, w int) bool {
	for _, x := range weeks {
		if x == w {
			return true
		}
	}
	return false
}

func isWeekScope(err error) bool {
	var ce *CollectorError
	if !errors.As(err, &ce) {
		return false
	}
	return ce.Scope == ScopeWeek
}

func errorCodeOf(err error) string {
	var ce *CollectorError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return CodeInternal
}

// hasPreviousWeekFile 判断本机快照库或仓库 data/ 是否有该周旧文件。
func hasPreviousWeekFile(opts Options, term string, week int) bool {
	_, _, err := readPublishedFile(opts, dataRelPath(term, week))
	return err == nil
}

func dataRelPath(term string, week int) string {
	return filepath.Join(TermsDirName, term, WeeksDirName, weekFileName(week))
}
