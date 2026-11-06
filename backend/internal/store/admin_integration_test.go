package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// openAdminTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 每次重建保证 admin_user / admin_session 都从空表开始，用例之间不共享残留行。
func openAdminTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("KJS_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 KJS_TEST_DSN，跳过真实库集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(pool.Close)

	// 从干净库开始，确保 0004 的 admin_session 由本用例的迁移建出来。
	if _, err := pool.Exec(ctx, "DROP SCHEMA public CASCADE"); err != nil {
		t.Fatalf("清理 schema 失败: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA public"); err != nil {
		t.Fatalf("重建 schema 失败: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return ctx, pool
}

// TestAdminRepoAgainstRealDB 覆盖 admin_user 的真实 SQL 口径：
// 空库无管理员、首次创建写入 created=true、冲突不覆盖且 created=false、
// 改密覆盖哈希并前移时刻、删除后回到未初始化且更新 0 行报错。
//
// 子测试按声明顺序执行且有状态依赖（后一步基于前一步的数据），因此不并行。
func TestAdminRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openAdminTestDB(t)
	repo := NewAdminRepo(pool)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	t.Run("空库没有管理员", func(t *testing.T) {
		user, found, err := repo.Admin(ctx)
		if err != nil {
			t.Fatalf("空库查询不应报错: %v", err)
		}
		if found {
			t.Fatalf("空库期望 found=false，实际 %+v", user)
		}
	})

	t.Run("首次创建返回 created=true", func(t *testing.T) {
		user, created, err := repo.CreateAdmin(ctx, "hash-1", now)
		if err != nil {
			t.Fatalf("创建失败: %v", err)
		}
		if !created {
			t.Fatal("空库首次创建应返回 created=true")
		}
		if user.ID != 1 || user.PasswordHash != "hash-1" {
			t.Fatalf("返回的管理员不符: %+v", user)
		}
		// 两个时间列都由入参 now 写入，不依赖列默认值。
		if !user.PasswordUpdatedAt.Equal(now) || !user.CreatedAt.Equal(now) {
			t.Fatalf("时间列期望 %s，实际 updated=%s created=%s",
				now, user.PasswordUpdatedAt, user.CreatedAt)
		}

		read, found, err := repo.Admin(ctx)
		if err != nil || !found {
			t.Fatalf("创建后应能读回，实际 found=%v err=%v", found, err)
		}
		// time.Time 用 Equal 比较：库回读的时区表示与入参不一定相同。
		if read.ID != user.ID || read.PasswordHash != user.PasswordHash ||
			!read.PasswordUpdatedAt.Equal(user.PasswordUpdatedAt) ||
			!read.CreatedAt.Equal(user.CreatedAt) {
			t.Fatalf("读回的行与创建返回不一致: %+v vs %+v", read, user)
		}
	})

	t.Run("冲突返回 created=false 且不覆盖", func(t *testing.T) {
		later := now.Add(time.Hour)
		user, created, err := repo.CreateAdmin(ctx, "hash-2", later)
		if err != nil {
			t.Fatalf("冲突不应报错: %v", err)
		}
		if created {
			t.Fatal("已有管理员时 created 期望 false")
		}
		if user != (model.AdminUser{}) {
			t.Fatalf("冲突时期望零值管理员，实际 %+v", user)
		}

		// 原有行的哈希与时间必须保持原样：ON CONFLICT DO NOTHING 不写覆盖。
		read, found, err := repo.Admin(ctx)
		if err != nil || !found {
			t.Fatalf("读回失败: found=%v err=%v", found, err)
		}
		if read.PasswordHash != "hash-1" ||
			!read.PasswordUpdatedAt.Equal(now) || !read.CreatedAt.Equal(now) {
			t.Fatalf("冲突后原记录被改动: %+v", read)
		}
	})

	t.Run("改密覆盖哈希并前移时刻", func(t *testing.T) {
		later := now.Add(24 * time.Hour)
		if err := repo.UpdatePassword(ctx, "hash-3", later); err != nil {
			t.Fatalf("改密失败: %v", err)
		}

		read, found, err := repo.Admin(ctx)
		if err != nil || !found {
			t.Fatalf("读回失败: found=%v err=%v", found, err)
		}
		if read.PasswordHash != "hash-3" || !read.PasswordUpdatedAt.Equal(later) {
			t.Fatalf("改密后期望 hash-3 / %s，实际 %+v", later, read)
		}
		// created_at 是账号建立时刻，改密不动它。
		if !read.CreatedAt.Equal(now) {
			t.Fatalf("改密不应改动 created_at，实际 %s", read.CreatedAt)
		}
	})

	t.Run("删除后回到未初始化", func(t *testing.T) {
		if err := repo.DeleteAdmin(ctx); err != nil {
			t.Fatalf("删除失败: %v", err)
		}
		_, found, err := repo.Admin(ctx)
		if err != nil || found {
			t.Fatalf("删除后期望 found=false，实际 found=%v err=%v", found, err)
		}

		// 补偿路径可能重复删除，第二次也应成功。
		if err := repo.DeleteAdmin(ctx); err != nil {
			t.Fatalf("重复删除应幂等，实际 %v", err)
		}

		// 没有记录时改密必须报错，而不是假装写成功。
		if err := repo.UpdatePassword(ctx, "hash-4", now); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("无管理员改密期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})
}

// TestAdminSessionRepoAgainstRealDB 覆盖 admin_session 的真实 SQL 口径：
// 写入后可读回、主键冲突报错且不覆盖、过期清理只删到期的行（含 expires_at = now
// 的边界）、单条删除幂等、全量清理返回影响行数。
func TestAdminSessionRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openAdminTestDB(t)
	repo := NewAdminRepo(pool)
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	// 三条会话：有效、已过期、恰好到期（expires_at == now）。
	active := model.AdminSession{
		TokenHash: "hash-active", CSRFToken: "csrf-active",
		CreatedAt: now, ExpiresAt: now.Add(model.SessionTTL),
	}
	expired := model.AdminSession{
		TokenHash: "hash-expired", CSRFToken: "csrf-expired",
		CreatedAt: now.Add(-13 * time.Hour), ExpiresAt: now.Add(-time.Hour),
	}
	boundary := model.AdminSession{
		TokenHash: "hash-boundary", CSRFToken: "csrf-boundary",
		CreatedAt: now.Add(-model.SessionTTL), ExpiresAt: now,
	}

	t.Run("写入后可按摘要读回", func(t *testing.T) {
		if err := repo.InsertSession(ctx, active); err != nil {
			t.Fatalf("写入会话失败: %v", err)
		}

		got, found, err := repo.Session(ctx, active.TokenHash)
		if err != nil || !found {
			t.Fatalf("应能读到会话，实际 found=%v err=%v", found, err)
		}
		// time.Time 用 Equal 比较：库回读的时区表示与入参不一定相同。
		if got.TokenHash != active.TokenHash || got.CSRFToken != active.CSRFToken ||
			!got.CreatedAt.Equal(active.CreatedAt) || !got.ExpiresAt.Equal(active.ExpiresAt) {
			t.Fatalf("读回的会话不符: %+v vs %+v", got, active)
		}

		_, found, err = repo.Session(ctx, "hash-missing")
		if err != nil || found {
			t.Fatalf("不存在的摘要期望 found=false，实际 found=%v err=%v", found, err)
		}
	})

	t.Run("主键冲突报仓储错误且不覆盖", func(t *testing.T) {
		conflict := active
		conflict.CSRFToken = "csrf-overwrite"
		if err := repo.InsertSession(ctx, conflict); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("重复摘要期望 ErrAdminStoreFailed，实际 %v", err)
		}

		got, found, err := repo.Session(ctx, active.TokenHash)
		if err != nil || !found {
			t.Fatalf("读回失败: found=%v err=%v", found, err)
		}
		if got.CSRFToken != active.CSRFToken {
			t.Fatalf("冲突写入不应覆盖原会话，实际 %+v", got)
		}
	})

	t.Run("过期清理只删到期的行", func(t *testing.T) {
		if err := repo.InsertSession(ctx, expired); err != nil {
			t.Fatalf("写入过期会话失败: %v", err)
		}
		if err := repo.InsertSession(ctx, boundary); err != nil {
			t.Fatalf("写入边界会话失败: %v", err)
		}

		// 过期一侧含等号：expires_at = now 的会话按 CP6 已经无效，必须一起清掉。
		rows, err := repo.DeleteExpiredSessions(ctx, now)
		if err != nil {
			t.Fatalf("清理过期会话失败: %v", err)
		}
		if rows != 2 {
			t.Fatalf("期望清理 2 条（已过期 + 恰好到期），实际 %d", rows)
		}

		if _, found, err := repo.Session(ctx, expired.TokenHash); err != nil || found {
			t.Fatalf("过期会话应被删掉，实际 found=%v err=%v", found, err)
		}
		if _, found, err := repo.Session(ctx, boundary.TokenHash); err != nil || found {
			t.Fatalf("恰好到期的会话应被删掉，实际 found=%v err=%v", found, err)
		}
		// 仍有效的会话不能被顺手清掉：now < expires_at 才算有效。
		if _, found, err := repo.Session(ctx, active.TokenHash); err != nil || !found {
			t.Fatalf("有效会话不应被清理，实际 found=%v err=%v", found, err)
		}

		// 再清一次没有可删的行，影响行数为 0。
		rows, err = repo.DeleteExpiredSessions(ctx, now)
		if err != nil || rows != 0 {
			t.Fatalf("第二次清理期望 0 行，实际 rows=%d err=%v", rows, err)
		}
	})

	t.Run("删除单条会话幂等", func(t *testing.T) {
		if err := repo.DeleteSession(ctx, active.TokenHash); err != nil {
			t.Fatalf("删除会话失败: %v", err)
		}
		if _, found, err := repo.Session(ctx, active.TokenHash); err != nil || found {
			t.Fatalf("删除后应查不到，实际 found=%v err=%v", found, err)
		}
		// 重复删除（比如重复退出）不应报错。
		if err := repo.DeleteSession(ctx, active.TokenHash); err != nil {
			t.Fatalf("重复删除应幂等，实际 %v", err)
		}
	})

	t.Run("全量清理返回影响行数", func(t *testing.T) {
		for _, tokenHash := range []string{"hash-all-a", "hash-all-b", "hash-all-c"} {
			s := model.AdminSession{
				TokenHash: tokenHash,
				CSRFToken: "csrf-all",
				CreatedAt: now,
				ExpiresAt: now.Add(model.SessionTTL),
			}
			if err := repo.InsertSession(ctx, s); err != nil {
				t.Fatalf("写入会话 %s 失败: %v", tokenHash, err)
			}
		}

		rows, err := repo.DeleteAllSessions(ctx)
		if err != nil {
			t.Fatalf("全量清理失败: %v", err)
		}
		if rows != 3 {
			t.Fatalf("期望清理 3 条，实际 %d", rows)
		}

		rows, err = repo.DeleteAllSessions(ctx)
		if err != nil || rows != 0 {
			t.Fatalf("再次清理期望 0 行，实际 rows=%d err=%v", rows, err)
		}
	})
}
