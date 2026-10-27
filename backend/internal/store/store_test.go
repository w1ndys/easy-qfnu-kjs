package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestOpenBadDSN 覆盖连接串语法错误：不进入重试循环，立即报错。
func TestOpenBadDSN(t *testing.T) {
	_, err := Open(context.Background(), "://not-a-dsn")
	if !errors.Is(err, model.ErrStoreUnavailable) {
		t.Fatalf("期望 ErrStoreUnavailable，实际 err=%v", err)
	}
}

// TestOpenCancelled 覆盖上游取消：不等满就绪窗口，立刻返回。
func TestOpenCancelled(t *testing.T) {
	// 指向本地关闭端口，连接必然失败，从而落到重试分支。
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Open(ctx, "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	elapsed := time.Since(start)

	if !errors.Is(err, model.ErrStoreUnavailable) {
		t.Fatalf("期望 ErrStoreUnavailable，实际 err=%v", err)
	}
	// 就绪窗口是 30 秒，被取消后应当远早于此返回。
	if elapsed > readyWait/2 {
		t.Fatalf("取消后应在就绪窗口之前返回，实际耗时 %s", elapsed)
	}
}
