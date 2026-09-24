package collector

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/cas"
	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

// Options 采集器运行参数（由 cmd/collector 组装）。
type Options struct {
	RepoDir     string // 仓库根（默认当前工作目录）
	ConfigPath  string
	DataDir     string
	SchemaDir   string
	StateDir    string
	DryRun      bool
	Username    string
	Password    string
	PublishBase string
	PublishRepo string
	WebhookURL  string
	WebhookSec  string
	HTTPTimeout time.Duration
}

// LockWaitTimeout 采集锁最长等待时间（Q160）。
const LockWaitTimeout = 5 * time.Minute

// CollectDeadline 整轮采集网络阶段总时限（Q57/§7）。
const CollectDeadline = FullTotalMinutes * time.Minute

// newUpstream 创建 CAS 客户端 + 上游访问器。
func newUpstream(timeout time.Duration) (*Upstream, error) {
	client, err := cas.NewClient(cas.WithTimeout(timeout))
	if err != nil {
		return nil, err
	}
	return NewUpstream(client), nil
}

// RunCommand run 子命令：collect + validate + （非 dry-run）publish + 发布验收。
// 返回错误时调用方以非零退出码退出；告警触发点集中在函数内。
func RunCommand(ctx context.Context, opts Options) error {
	notifier := NewNotifier(NewAlerter(opts.WebhookURL, opts.WebhookSec))
	release, err := AcquireLock(ctx, opts.StateDir, LockWaitTimeout)
	if err != nil {
		logger.Error("获取采集锁失败: %v", err)
		notifier.Notify(ctx, "lock_timeout", fmt.Sprintf("等待采集锁超时，本轮放弃（%v）", errText(err)))
		return err
	}
	defer release()
	logger.Info("采集锁已获取")

	state := loadOrEmptyState(opts)
	prev := *state
	round := &roundRun{ctx: ctx, opts: opts, notifier: notifier, state: state}
	cand, err := round.collect()
	if err != nil || cand == nil {
		return err
	}
	return round.publish(cand, prev)
}

type roundRun struct {
	ctx      context.Context
	opts     Options
	notifier *Notifier
	state    *State
}

func loadOrEmptyState(opts Options) *State {
	state, err := loadState(opts)
	if err != nil {
		logger.Warn("读取状态文件失败（按空白状态继续）: %v", err)
		return &State{}
	}
	return state
}

func (r *roundRun) fail(err error, category string) error {
	code := errorCodeOf(err)
	logger.Error("本轮失败（%s）: %v", category, err)
	r.notifier.Notify(r.ctx, category, fmt.Sprintf("%s（code=%s）", errText(err), code))
	r.state.ConsecutiveFailures++
	r.state.LastRoundSuccess = false
	r.state.LastRoundAt = formatRFC3339BJ(nowBJ())
	if serr := r.state.save(r.opts); serr != nil {
		logger.Error("保存状态失败: %v", serr)
	}
	return err
}

func (r *roundRun) collect() (*Candidate, error) {
	cfg, err := LoadConfig(r.opts.ConfigPath, r.opts.SchemaDir)
	if err != nil {
		return nil, r.fail(err, "collect_failed")
	}
	up, err := newUpstream(httpTimeout(r.opts))
	if err != nil {
		return nil, r.fail(newGlobal(CodeInternal, "创建 CAS 客户端失败: %v", err), "collect_failed")
	}
	collectCtx, cancel := context.WithTimeout(r.ctx, CollectDeadline)
	cand, err := Collect(collectCtx, cfg, up, r.opts)
	cancel()
	if err != nil {
		return nil, r.fail(err, "collect_failed")
	}
	if !cand.InCalendar {
		r.skipOutsideCalendar(cand)
		return nil, nil
	}
	logger.Info("采集完成：学期=%s 成功周=%v", cand.Term, cand.SuccessWeeks)
	r.rememberCandidate(cand)
	r.notifyWeekFailures(cand)
	return cand, nil
}

func (r *roundRun) skipOutsideCalendar(cand *Candidate) {
	logger.Info("不在教学周历内：本轮跳过（学期=%s）；无告警", cand.Term)
	r.state.LastTerm = cand.Term
	r.state.LastRoundAt = formatRFC3339BJ(nowBJ())
	r.state.LastRoundSuccess = true
	_ = r.state.save(r.opts)
}

func (r *roundRun) rememberCandidate(cand *Candidate) {
	r.state.LastCandidateDir = cand.Dir
	r.state.LastTerm = cand.Term
	if err := r.state.save(r.opts); err != nil {
		logger.Warn("保存候选路径失败: %v", err)
	}
}

func (r *roundRun) notifyWeekFailures(cand *Candidate) {
	if len(cand.Failures) == 0 {
		return
	}
	var parts []string
	for _, f := range cand.Failures {
		parts = append(parts, fmt.Sprintf("第%d周(%s)", f.Week, f.Code))
	}
	r.notifier.Notify(r.ctx, "week_failed",
		fmt.Sprintf("以下周采集失败、保留上一版数据继续发布：%s", joinList(parts)))
}

func (r *roundRun) publish(cand *Candidate, prev State) error {
	if err := ValidateCandidate(r.opts, cand); err != nil {
		return r.fail(err, "validate_blocked")
	}
	if r.opts.DryRun {
		return r.finishDryRun(cand)
	}
	res, err := Publish(r.ctx, r.opts, cand)
	if err != nil {
		logger.Warn("候选目录保留供检查：%s", cand.Dir)
		return r.fail(err, "publish_failed")
	}
	if err := AcceptPublish(r.ctx, r.opts, res, &http.Client{Timeout: httpTimeout(r.opts)}); err != nil {
		logger.Warn("候选目录保留供检查：%s", cand.Dir)
		return r.fail(err, "publish_failed")
	}
	return r.finishSuccess(cand, prev)
}

func (r *roundRun) finishDryRun(cand *Candidate) error {
	logger.Info("dry-run：校验通过，跳过发布。候选目录（保留供检查）=%s", cand.Dir)
	logger.Info("dry-run：若正式发布将提交 data: %s（release=%s）", cand.Term, cand.Manifest.ReleaseID)
	r.state.ConsecutiveFailures = 0
	r.state.LastRoundSuccess = true
	r.state.LastRoundAt = formatRFC3339BJ(nowBJ())
	_ = r.state.save(r.opts)
	return nil
}

func (r *roundRun) finishSuccess(cand *Candidate, prev State) error {
	if cand.SwitchTerm {
		r.notifier.Notify(r.ctx, "term_switch",
			fmt.Sprintf("新学期 %s 已切换发布（删除旧学期 data/terms/%s）", cand.Term, cand.OldTerm))
	}
	if prev.ConsecutiveFailures > 0 || !prev.LastRoundSuccess {
		r.notifier.Notify(r.ctx, "recovered",
			fmt.Sprintf("连续 %d 轮失败后首次成功（release=%s）", prev.ConsecutiveFailures, cand.Manifest.ReleaseID))
	}
	r.markSuccess()
	_ = os.RemoveAll(cand.Dir)
	logger.Info("本轮 run 完成：release=%s term=%s weeks=%v", cand.Manifest.ReleaseID, cand.Term, cand.SuccessWeeks)
	return nil
}

func (r *roundRun) markSuccess() {
	r.state.ConsecutiveFailures = 0
	r.state.LastRoundSuccess = true
	r.state.LastRoundAt = formatRFC3339BJ(nowBJ())
	if err := r.state.save(r.opts); err != nil {
		logger.Warn("保存状态失败: %v", err)
	}
}

// CollectCommand collect 子命令：只生成候选（不做 schema 校验与发布）。
func CollectCommand(ctx context.Context, opts Options) error {
	release, err := AcquireLock(ctx, opts.StateDir, LockWaitTimeout)
	if err != nil {
		logger.Error("获取采集锁失败: %v", err)
		return err
	}
	defer release()

	cfg, err := LoadConfig(opts.ConfigPath, opts.SchemaDir)
	if err != nil {
		return err
	}
	up, err := newUpstream(httpTimeout(opts))
	if err != nil {
		return err
	}
	collectCtx, cancel := context.WithTimeout(ctx, CollectDeadline)
	cand, err := Collect(collectCtx, cfg, up, opts)
	cancel()
	if err != nil {
		return err
	}
	if !cand.InCalendar {
		logger.Info("不在教学周历内：本轮跳过（学期=%s）", cand.Term)
		return nil
	}
	state, err := loadState(opts)
	if err != nil {
		state = &State{}
	}
	state.LastCandidateDir = cand.Dir
	state.LastTerm = cand.Term
	if err := state.save(opts); err != nil {
		logger.Warn("保存候选路径失败: %v", err)
	}
	logger.Info("collect 完成：候选目录=%s 学期=%s 成功周=%v 失败周=%d（validate 子命令校验）",
		cand.Dir, cand.Term, cand.SuccessWeeks, len(cand.Failures))
	return nil
}

// ValidateCommand validate 子命令：校验最近一次 collect 的候选。
func ValidateCommand(ctx context.Context, opts Options) error {
	state, err := loadState(opts)
	if err != nil {
		return fmt.Errorf("读取状态失败: %w", err)
	}
	if state.LastCandidateDir == "" {
		return newGlobal(CodeNoCandidate, "没有候选数据：请先运行 collect")
	}
	cand := &Candidate{Dir: state.LastCandidateDir}
	return ValidateCandidate(opts, cand)
}

// PublishCommand publish 子命令：发布最近一次 collect 的候选（含验收）。
func PublishCommand(ctx context.Context, opts Options) error {
	release, err := AcquireLock(ctx, opts.StateDir, LockWaitTimeout)
	if err != nil {
		logger.Error("获取采集锁失败: %v", err)
		return err
	}
	defer release()

	state, err := loadState(opts)
	if err != nil {
		return fmt.Errorf("读取状态失败: %w", err)
	}
	if state.LastCandidateDir == "" {
		return newGlobal(CodeNoCandidate, "没有候选数据：请先运行 collect")
	}
	cand, err := candidateFromDir(state.LastCandidateDir)
	if err != nil {
		return err
	}
	if err := ValidateCandidate(opts, cand); err != nil {
		return newGlobal(CodeSchemaFailed, "候选校验未通过，拒绝发布: %v", err)
	}
	res, err := Publish(ctx, opts, cand)
	if err != nil {
		return err
	}
	if err := AcceptPublish(ctx, opts, res, &http.Client{Timeout: httpTimeout(opts)}); err != nil {
		return err
	}
	_ = os.RemoveAll(cand.Dir)
	state.LastCandidateDir = ""
	_ = state.save(opts)
	logger.Info("publish 完成：%s", res.Message)
	return nil
}

// candidateFromDir 从候选目录重建 Candidate（manifest 从文件读取）。
func candidateFromDir(dir string) (*Candidate, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return nil, newGlobal(CodeNoCandidate, "候选 manifest 缺失: %v", err)
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, newGlobal(CodeNoCandidate, "候选 manifest 解析失败: %v", err)
	}
	return &Candidate{Dir: dir, Manifest: m, Term: m.Term,
		TotalWeeks: m.Anchor.TotalWeeks, CurrentWeek: m.Anchor.Week, InCalendar: m.Anchor.InTeachingCalendar,
		GroupCounts: map[string]int{}}, nil
}

func httpTimeout(opts Options) time.Duration {
	if opts.HTTPTimeout > 0 {
		return opts.HTTPTimeout
	}
	return 120 * time.Second
}

func errText(err error) string {
	return err.Error()
}

func joinList(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "、"
		}
		out += s
	}
	return out
}
