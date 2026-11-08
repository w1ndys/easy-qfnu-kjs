package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// mustExec 执行种子语句，失败直接终止用例。
//
// 放在包级的 helpers_test.go 而不是某个具体测试文件里：store 的真实库用例普遍用它
// 铺种子，挂在某一个文件的测试名下会让其他用例隐式依赖「那个文件恰好还在、还没改名」，
// 拆文件或重命名时才会以「undefined: mustExec」的形式暴露。同包测试文件共享它，
// 任何一个用例文件都能直接调。
func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("执行 %q 失败: %v", sql, err)
	}
}
