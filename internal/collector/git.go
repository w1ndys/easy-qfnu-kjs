package collector

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// gitRun 在仓库目录执行 git 命令，返回 stdout。
func gitRun(opts Options, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", opts.RepoDir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s 失败: %v (%s)",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// gitShowFile 读取 git HEAD 中的文件（不存在/无 HEAD 时返回错误）。
func gitShowFile(opts Options, rel string) ([]byte, error) {
	out, err := gitRun(opts, "show", "HEAD:"+rel)
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

func gitHasHEAD(opts Options) bool {
	_, err := gitRun(opts, "rev-parse", "--verify", "HEAD")
	return err == nil
}

func gitCurrentBranch(opts Options) (string, error) {
	return gitRun(opts, "rev-parse", "--abbrev-ref", "HEAD")
}

func gitRevParse(opts Options, ref string) (string, error) {
	return gitRun(opts, "rev-parse", "--verify", ref)
}

func gitFetch(opts Options) error {
	_, err := gitRun(opts, "fetch", "origin")
	return err
}

func gitMergeFFOnly(opts Options) error {
	_, err := gitRun(opts, "merge", "--ff-only", "origin/main")
	return err
}

// gitPorcelain 返回工作区相对 HEAD 的全部改动（含未跟踪），解析为路径列表。
func gitPorcelain(opts Options) ([]string, error) {
	out, err := gitRun(opts, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) < 4 {
			continue
		}
		// 兼容普通与重命名（R old -> new）状态。
		fields := strings.Fields(line)
		if len(fields) >= 2 && (strings.HasPrefix(line, "R") || strings.HasPrefix(line, "C")) {
			paths = append(paths, fields[len(fields)-1])
			continue
		}
		paths = append(paths, fields[len(fields)-1])
	}
	return paths, nil
}

// gitAddAllData 暂存 data/ 全部变更（新增/修改/删除）。
func gitAddAllData(opts Options) error {
	_, err := gitRun(opts, "add", "-A", "--", "data/")
	return err
}

func gitCommit(opts Options, message string) error {
	_, err := gitRun(opts, "commit", "-m", message)
	return err
}

func gitPush(opts Options) error {
	_, err := gitRun(opts, "push", "origin", "main")
	return err
}

// gitHasStagedChanges 检查暂存区是否有改动。
func gitHasStagedChanges(opts Options) (bool, error) {
	out, err := gitRun(opts, "diff", "--cached", "--name-only")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// remoteOwnerRepo 从 git remote 推断 owner/repo（github.com 形态），可被 opts.PublishRepo 覆盖。
func remoteOwnerRepo(opts Options) (string, error) {
	if opts.PublishRepo != "" {
		return opts.PublishRepo, nil
	}
	url, err := gitRun(opts, "remote", "get-url", "origin")
	if err != nil {
		return "", err
	}
	url = strings.TrimSuffix(url, ".git")
	url = strings.TrimPrefix(url, "git@github.com:")
	url = strings.TrimPrefix(url, "https://github.com/")
	url = strings.TrimPrefix(url, "ssh://git@github.com/")
	if i := strings.Index(url, "/"); i <= 0 || i == len(url)-1 {
		return "", fmt.Errorf("无法从 remote 推断 owner/repo: %s", url)
	}
	return url, nil
}
