package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// State 采集器本地状态（~/.local/state/easy-qfnu-kjs/collector-state.json）。
type State struct {
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastRoundSuccess    bool   `json:"last_round_success"`
	LastRoundAt         string `json:"last_round_at,omitempty"`
	LastCandidateDir    string `json:"last_candidate_dir,omitempty"`
	LastTerm            string `json:"last_term,omitempty"`
}

func loadState(opts Options) (*State, error) {
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(opts.StateDir, StateFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return &State{}, nil // 状态文件损坏时按空白状态继续（不阻塞采集）
	}
	return &s, nil
}

func (s *State) save(opts Options) error {
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(opts.StateDir, StateFileName), append(b, '\n'), 0600)
}

// AcquireLock 用 flock 获取采集锁：最多等待 wait 时长（默认策略 5 分钟），
// 超时返回错误（本轮放弃，绝不强制终止正在运行的任务）。
func AcquireLock(ctx context.Context, stateDir string, wait time.Duration) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件失败: %w", err)
	}

	deadline := time.Now().Add(wait)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, fmt.Errorf("获取锁失败: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, newGlobal(CodeLockTimeout, "等待采集锁超过 %s（另一采集任务可能仍在运行），本轮放弃", wait)
		}
		if !sleepCtx(ctx, 5*time.Second) {
			f.Close()
			return nil, newGlobal(CodeLockTimeout, "等待采集锁时上下文取消")
		}
	}
}
