package collector

import (
	"context"
	"fmt"

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

// Publish 把候选目录写入 PostgreSQL，并在同一事务里切换当前 release。
//
// 不再提交或推送 Git。周文件原文会存进数据库，失败周才能按原哈希继续合并。
// 教室小节同时拆成 kjs.room_slot，供查询服务按节次过滤。
func Publish(ctx context.Context, opts Options, cand *Candidate) (*PublishResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, newGlobal(CodePushFailed, "发布前上下文取消: %v", err)
	}
	if cand == nil || cand.Manifest == nil {
		return nil, newGlobal(CodeNoCandidate, "没有可发布的候选数据")
	}
	if opts.DatabaseURL == "" {
		return nil, newGlobal(CodePublishState, "未配置 DATABASE_URL")
	}
	term := cand.Manifest.Term
	_, switchTerm := publishSwitch(opts, term)
	releaseID := cand.Manifest.ReleaseID
	if err := publishToDB(ctx, opts.DatabaseURL, cand); err != nil {
		return nil, newGlobal(CodePushFailed, "写入 PostgreSQL 失败: %v", err)
	}
	msg := fmt.Sprintf("postgres %s", releaseID)
	logger.Info("当前 release 已切换：%s term=%s", msg, term)
	return &PublishResult{
		ReleaseID: releaseID,
		Term:      term,
		Switch:    switchTerm,
		Message:   msg,
	}, nil
}

func publishSwitch(opts Options, term string) (string, bool) {
	baseline := loadLocalManifest(opts)
	if baseline == nil || baseline.Term == "" || baseline.Term == term {
		return "", false
	}
	return baseline.Term, true
}
