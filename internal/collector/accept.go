package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

const (
	checkPollInterval = 15 * time.Second
	checkPollMax      = 40 // 10 分钟
	manifestPollInt   = 10 * time.Second
	manifestPollMax   = 30 // 5 分钟
)

// AcceptPublish 确认本机快照库已经切到这次 release。
// 公网域名还指着旧服务时不做 HTTP 验收，避免把旧站的 404 当成发布失败。
func AcceptPublish(ctx context.Context, opts Options, res *PublishResult, httpClient *http.Client) error {
	if err := ctx.Err(); err != nil {
		return newGlobal(CodeVerifyFailed, "发布验收上下文取消: %v", err)
	}
	if err := acceptLocalRelease(opts, res.ReleaseID); err != nil {
		return err
	}
	if !localPublishBase(opts.PublishBase) {
		logger.Info("跳过公网验收：%s 仍不是本机服务", opts.PublishBase)
		return nil
	}
	live, err := waitManifestRelease(ctx, opts, res.ReleaseID, httpClient)
	if err != nil {
		return err
	}
	if err := sampleQuery(ctx, opts, live, httpClient); err != nil {
		return err
	}
	logger.Info("发布验收通过：release=%s", res.ReleaseID)
	return nil
}

func acceptLocalRelease(opts Options, releaseID string) error {
	m := loadLocalManifest(opts)
	if m == nil || m.ReleaseID != releaseID {
		return newGlobal(CodeVerifyFailed, "本机快照库没有切换到 release %s", releaseID)
	}
	return nil
}

func localPublishBase(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
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
	defer closeResponseBody(resp.Body)
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
