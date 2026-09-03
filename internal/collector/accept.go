package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

const (
	checkPollInterval = 15 * time.Second
	checkPollMax      = 40 // 10 分钟
	manifestPollInt   = 10 * time.Second
	manifestPollMax   = 30 // 5 分钟
)

// AcceptPublish 发布验收（Q156）：
//  1. 每 15s 查 GitHub commit check-runs 直到通过（上限 10 分钟）；
//  2. 每 10s GET 生产 /api/v1/manifest 直到 release_id 匹配（上限 5 分钟）；
//  3. 固定样例查询（第一启用分组 + 当前周 + 当天 + 01—12）要求 200。
//
// 任一环节超时/失败 → 返回错误（调用方告警，不自动回滚）。
func AcceptPublish(ctx context.Context, opts Options, res *PublishResult, httpClient *http.Client) error {
	if err := waitGitHubChecks(ctx, opts, res.CommitSHA); err != nil {
		return err
	}
	live, err := waitManifestRelease(ctx, opts, res.ReleaseID, httpClient)
	if err != nil {
		return err
	}
	if err := sampleQuery(ctx, opts, live, httpClient); err != nil {
		return err
	}
	logger.Info("发布验收通过：release=%s commit=%s", res.ReleaseID, res.CommitSHA)
	return nil
}

// ghAPIGet 调用 gh api（需 gh 已认证）。
func ghAPIGet(ctx context.Context, path string) ([]byte, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh CLI 不可用: %v", err)
	}
	cmd := exec.CommandContext(ctx, "gh", "api", path)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gh api %s 失败: %v (%s)", path, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

type ghChecks struct {
	TotalCount int `json:"total_count"`
	CheckRuns  []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type ghStatus struct {
	State      string `json:"state"`
	TotalCount int    `json:"total_count"`
}

// waitGitHubChecks 轮询 GitHub 提交状态与 check-runs。
func waitGitHubChecks(ctx context.Context, opts Options, commitSHA string) error {
	repo, err := remoteOwnerRepo(opts)
	if err != nil {
		return newGlobal(CodeVerifyFailed, "推断 GitHub 仓库失败: %v", err)
	}

	badConclusions := map[string]bool{
		"failure": true, "cancelled": true, "timed_out": true, "action_required": true,
	}
	base := fmt.Sprintf("repos/%s/commits/%s", repo, commitSHA)

	deadline := time.Now().Add(checkPollMax * checkPollInterval)
	emptyRounds := 0
	for round := 1; round <= checkPollMax; round++ {
		if ctx.Err() != nil {
			return newGlobal(CodeVerifyFailed, "check-runs 验收上下文取消")
		}
		checksJSON, cerr := ghAPIGet(ctx, base+"/check-runs")
		statusJSON, serr := ghAPIGet(ctx, base+"/status")
		if cerr != nil || serr != nil {
			logger.Warn("查询 GitHub 状态失败（第 %d 次）: %v / %v", round, cerr, serr)
			if !sleepCtx(ctx, checkPollInterval) {
				return newGlobal(CodeVerifyFailed, "check-runs 验收上下文取消")
			}
			continue
		}

		var checks ghChecks
		_ = json.Unmarshal(checksJSON, &checks)
		var st ghStatus
		_ = json.Unmarshal(statusJSON, &st)

		total := checks.TotalCount + st.TotalCount
		if total == 0 {
			emptyRounds++
			if emptyRounds >= 3 {
				logger.Info("仓库未配置 check-runs/status（3 轮仍为空），视为无 CI 门禁通过")
				return nil
			}
			if time.Now().After(deadline) {
				return newGlobal(CodeVerifyFailed, "GitHub 状态查询超时（无任何状态上报）")
			}
			if !sleepCtx(ctx, checkPollInterval) {
				return newGlobal(CodeVerifyFailed, "check-runs 验收上下文取消")
			}
			continue
		}

		failed := false
		pending := false
		for _, cr := range checks.CheckRuns {
			switch {
			case cr.Status != "completed":
				pending = true
			case badConclusions[cr.Conclusion]:
				failed = true
			case cr.Conclusion != "success" && cr.Conclusion != "neutral" && cr.Conclusion != "skipped":
				pending = true
			}
		}
		if st.State == "failure" || st.State == "error" {
			failed = true
		} else if st.State == "pending" {
			pending = true
		}

		if failed {
			return newGlobal(CodeVerifyFailed, "GitHub 提交 %s 的 check-runs/status 出现失败结论", commitSHA)
		}
		if !pending {
			logger.Info("GitHub 提交检查通过（checks=%d status=%s）", checks.TotalCount, st.State)
			return nil
		}
		if time.Now().After(deadline) {
			return newGlobal(CodeVerifyFailed, "等待 GitHub check-runs 通过超时（10 分钟）")
		}
		if !sleepCtx(ctx, checkPollInterval) {
			return newGlobal(CodeVerifyFailed, "check-runs 验收上下文取消")
		}
	}
	return newGlobal(CodeVerifyFailed, "等待 GitHub check-runs 通过超时（10 分钟）")
}

// liveManifest 生产环境返回的 manifest 关键字段。
type liveManifest struct {
	ReleaseID string          `json:"release_id"`
	Term      string          `json:"term"`
	Anchor    Anchor          `json:"anchor"`
	Groups    []ManifestGroup `json:"groups"`
}

// waitManifestRelease 轮询生产 manifest 直到 release_id 匹配。
func waitManifestRelease(ctx context.Context, opts Options, releaseID string, httpClient *http.Client) (*liveManifest, error) {
	base := strings.TrimRight(opts.PublishBase, "/")
	url := base + "/api/v1/manifest"
	var lastErr error
	for round := 1; round <= manifestPollMax; round++ {
		if ctx.Err() != nil {
			return nil, newGlobal(CodeVerifyFailed, "生产 manifest 验收上下文取消")
		}
		body, err := httpGet(ctx, httpClient, url)
		if err != nil {
			lastErr = err
		} else {
			var lm liveManifest
			if uerr := json.Unmarshal(body, &lm); uerr != nil {
				lastErr = fmt.Errorf("解析生产 manifest 失败: %v", uerr)
			} else if lm.ReleaseID == releaseID {
				logger.Info("生产 manifest 已切换到 release=%s（第 %d 次轮询）", releaseID, round)
				return &lm, nil
			} else {
				lastErr = fmt.Errorf("生产 release_id=%q 尚不匹配（期望 %q）", lm.ReleaseID, releaseID)
			}
		}
		logger.Info("生产 manifest 轮询第 %d/%d 次: %v", round, manifestPollMax, lastErr)
		if !sleepCtx(ctx, manifestPollInt) {
			return nil, newGlobal(CodeVerifyFailed, "生产 manifest 验收上下文取消")
		}
	}
	return nil, newGlobal(CodeVerifyFailed, "生产 manifest 未在 5 分钟内切换到 release_id=%s", releaseID)
}

// sampleQuery 固定样例查询：第一启用分组 + 当前周 + 当天 + 01—12。
func sampleQuery(ctx context.Context, opts Options, lm *liveManifest, httpClient *http.Client) error {
	if len(lm.Groups) == 0 {
		return newGlobal(CodeVerifyFailed, "生产 manifest 无启用分组，无法执行样例查询")
	}
	groupID := lm.Groups[0].ID
	week := lm.Anchor.Week
	day := weekdayOfDate(lm.Anchor.Date)
	if week <= 0 || day <= 0 {
		return newGlobal(CodeVerifyFailed, "生产 manifest 锚点无效（week=%d day=%d），无法执行样例查询", week, day)
	}
	base := strings.TrimRight(opts.PublishBase, "/")
	u := fmt.Sprintf("%s/api/v1/empty-classrooms?group_id=%s&week=%d&day=%d&start_node=01&end_node=12",
		base, groupID, week, day)
	body, err := httpGet(ctx, httpClient, u)
	if err != nil {
		return newGlobal(CodeVerifyFailed, "样例查询失败: %v", err)
	}
	var resp struct {
		Rooms []json.RawMessage `json:"rooms"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return newGlobal(CodeVerifyFailed, "样例查询响应不是合法 JSON: %v", err)
	}
	if resp.Rooms == nil {
		return newGlobal(CodeVerifyFailed, "样例查询响应缺少 rooms 数组")
	}
	logger.Info("样例查询通过：group=%s week=%d day=%d rooms=%d",
		groupID, week, day, len(resp.Rooms))
	return nil
}

// httpGet 带状态码校验的 GET。
func httpGet(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "easy-qfnu-kjs-collector")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d（%s）", resp.StatusCode, url)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// weekdayOfDate 解析 YYYY-MM-DD 的星期（1=周一 … 7=周日）。
func weekdayOfDate(date string) int {
	t, err := time.ParseInLocation("2006-01-02", date, shanghaiLoc)
	if err != nil {
		return 0
	}
	wd := t.Weekday()
	if wd == time.Sunday {
		return 7
	}
	return int(wd)
}
