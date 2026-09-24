package collector

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

// PublishResult 发布结果（用于验收与告警文案）。
type PublishResult struct {
	CommitSHA string
	ReleaseID string
	Term      string
	Switch    bool
	Message   string
}

// Publish 把候选数据发布到 main：
// fetch + merge --ff-only → 工作区洁净检查 → 复制候选进 data/（学期切换删旧目录）→
// 提交（data: 前缀）→ push。发布验收见 AcceptPublish。
//
// cand.Manifest 必须已加载（来自候选目录）。学期切换由候选 term 与基线 manifest term
// 比较得出，因此独立 publish 命令也安全。
func Publish(ctx context.Context, opts Options, cand *Candidate) (*PublishResult, error) {
	if cand == nil || cand.Manifest == nil {
		return nil, newGlobal(CodeNoCandidate, "没有可发布的候选数据")
	}
	term := cand.Manifest.Term
	oldTerm, switchTerm := publishSwitch(opts, term)
	configRel := repoRelative(opts, opts.ConfigPath)
	if err := preparePublishTree(opts, cand, term, oldTerm, configRel); err != nil {
		return nil, err
	}
	return commitAndPush(opts, cand, term, switchTerm)
}

func publishSwitch(opts Options, term string) (string, bool) {
	baseline := loadLocalManifest(opts)
	if baseline == nil || baseline.Term == "" || baseline.Term == term {
		return "", false
	}
	return baseline.Term, true
}

func preparePublishTree(opts Options, cand *Candidate, term, oldTerm, configRel string) error {
	if err := ensurePublishBase(opts); err != nil {
		return err
	}
	if err := rejectUnexpectedChanges(opts, configRel); err != nil {
		return err
	}
	if err := copyCandidateToData(opts, cand, term, oldTerm); err != nil {
		return err
	}
	paths, err := gitPorcelain(opts)
	if err != nil {
		return newGlobal(CodePublishState, "读取工作区状态失败: %v", err)
	}
	return assertPlannedChanges(paths, term, oldTerm, configRel)
}

func rejectUnexpectedChanges(opts Options, configRel string) error {
	paths, err := gitPorcelain(opts)
	if err != nil {
		return newGlobal(CodePublishState, "读取工作区状态失败: %v", err)
	}
	for _, p := range paths {
		if p == configRel {
			continue
		}
		return newGlobal(CodeRepoDirty, "工作区存在预期外改动 %q（发布前只允许 config/rooms.json 人工改动）", p)
	}
	return nil
}

func commitAndPush(opts Options, cand *Candidate, term string, switchTerm bool) (*PublishResult, error) {
	sha, msg, err := commitDataChange(opts, term, cand.Manifest.ReleaseID, switchTerm)
	if err != nil {
		return nil, err
	}
	if err := pushIfStillFastForward(opts); err != nil {
		return nil, err
	}
	logger.Info("发布推送成功：%s %s", msg, sha)
	return &PublishResult{
		CommitSHA: sha,
		ReleaseID: cand.Manifest.ReleaseID,
		Term:      term,
		Switch:    switchTerm,
		Message:   msg,
	}, nil
}

func commitDataChange(opts Options, term, releaseID string, switchTerm bool) (string, string, error) {
	if err := gitAddAllData(opts); err != nil {
		return "", "", newGlobal(CodePushFailed, "git add data/ 失败: %v", err)
	}
	hasStaged, err := gitHasStagedChanges(opts)
	if err != nil {
		return "", "", newGlobal(CodePushFailed, "检查暂存区失败: %v", err)
	}
	if !hasStaged {
		return "", "", newGlobal(CodeNoCandidate, "暂存区没有变化（候选与已发布数据一致）")
	}
	msg := commitMessage(term, releaseID, switchTerm)
	if err := gitCommit(opts, msg); err != nil {
		return "", "", newGlobal(CodePushFailed, "git commit 失败: %v", err)
	}
	sha, err := gitRevParse(opts, "HEAD")
	if err != nil {
		return "", "", newGlobal(CodePushFailed, "读取 HEAD 失败: %v", err)
	}
	return sha, msg, nil
}

func pushIfStillFastForward(opts Options) error {
	if err := gitFetch(opts); err != nil {
		return newGlobal(CodePushFailed, "推送前 fetch 失败: %v", err)
	}
	parentSHA, err := gitRevParse(opts, "HEAD~1")
	if err != nil {
		return newGlobal(CodePushFailed, "读取提交父节点失败: %v", err)
	}
	originSHA, err := gitRevParse(opts, "origin/main")
	if err != nil {
		return newGlobal(CodePushFailed, "读取 origin/main 失败: %v", err)
	}
	// 父提交不是 origin/main 说明远端有新提交；禁止强推，本轮停下等下一轮。
	if parentSHA != originSHA {
		return newGlobal(CodePushFailed,
			"推送前远端出现新提交（origin/main=%s，本地父=%s）；本轮终止等待下一轮", originSHA, parentSHA)
	}
	if err := gitPush(opts); err != nil {
		return newGlobal(CodePushFailed, "git push 失败: %v", err)
	}
	return nil
}

// ensurePublishBase 校验并同步发布基线（Q139）。
func ensurePublishBase(opts Options) error {
	if !gitHasHEAD(opts) {
		return newGlobal(CodePublishState, "仓库没有 HEAD 提交；请先完成 main 初始提交后再使用 publish")
	}
	branch, err := gitCurrentBranch(opts)
	if err != nil {
		return newGlobal(CodePublishState, "读取当前分支失败: %v", err)
	}
	if branch != "main" {
		return newGlobal(CodePublishState, "当前分支为 %q，发布只允许在 main 分支执行", branch)
	}
	if err := gitFetch(opts); err != nil {
		return newGlobal(CodePublishState, "git fetch origin 失败: %v", err)
	}
	if _, err := gitRevParse(opts, "origin/main"); err != nil {
		return newGlobal(CodePublishState, "远端没有 origin/main；请先推送 main 初始提交")
	}
	headSHA, err := gitRevParse(opts, "HEAD")
	if err != nil {
		return newGlobal(CodePublishState, "读取 HEAD 失败: %v", err)
	}
	originSHA, _ := gitRevParse(opts, "origin/main")
	if headSHA != originSHA {
		logger.Info("本地 main 落后于 origin/main，执行 merge --ff-only")
		if err := gitMergeFFOnly(opts); err != nil {
			return newGlobal(CodeRepoDirty,
				"pull --ff-only 失败（工作区改动或无法快进）: %v；请勿 stash/丢弃用户改动", err)
		}
	}
	return nil
}

// copyCandidateToData 把候选目录内容复制进 data/；学期切换时删除旧学期目录。
func copyCandidateToData(opts Options, cand *Candidate, term, oldTerm string) error {
	if err := copyTree(cand.Dir, opts.DataDir); err != nil {
		return newGlobal(CodePushFailed, "复制候选到 data/ 失败: %v", err)
	}
	if oldTerm != "" && oldTerm != term {
		oldDir := filepath.Join(opts.DataDir, TermsDirName, oldTerm)
		if err := os.RemoveAll(oldDir); err != nil {
			return newGlobal(CodePushFailed, "删除旧学期目录 data/terms/%s 失败: %v", oldTerm, err)
		}
		logger.Info("已删除旧学期目录 data/terms/%s", oldTerm)
	}
	return nil
}

// copyTree 递归复制目录树（字节一致；文件 0644，目录 0755）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeFileAtomic(target, data, 0644)
	})
}

// assertPlannedChanges 复核工作区变更集合是否全部在计划内。
func assertPlannedChanges(paths []string, term, oldTerm, configRel string) error {
	for _, p := range paths {
		if p == configRel {
			continue // 人工改过的 config 允许存在（不随自动提交入库）
		}
		if !strings.HasPrefix(p, "data/") {
			return newGlobal(CodeRepoDirty, "发布前发现计划外改动 %q；本轮终止并保留工作区", p)
		}
		if p == "data/manifest.json" {
			continue
		}
		if strings.HasPrefix(p, "data/terms/"+term+"/weeks/week-") && strings.HasSuffix(p, ".json") {
			continue
		}
		if oldTerm != "" && oldTerm != term && strings.HasPrefix(p, "data/terms/"+oldTerm+"/") {
			continue // 学期切换：旧学期目录整体删除
		}
		return newGlobal(CodeRepoDirty, "发布前发现计划外 data/ 改动 %q；本轮终止", p)
	}
	return nil
}

func commitMessage(term, releaseID string, switchTerm bool) string {
	if switchTerm {
		return fmt.Sprintf("data: term switch %s", term)
	}
	return fmt.Sprintf("data: snapshot %s release %s", term, releaseID)
}

// repoRelative 把路径转成相对仓库根的斜杠路径（用于 git 比较）。
func repoRelative(opts Options, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	repoAbs, err := filepath.Abs(opts.RepoDir)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rel, err := filepath.Rel(repoAbs, abs)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
