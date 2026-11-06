package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// adminRowStub 是 pgx.Row 的替身：按调用方给的 Scan 目标顺序回放预设值，
// 或直接返回预设错误（用来模拟 pgx.ErrNoRows 与读库失败）。
type adminRowStub struct {
	values []any // 依次写入 Scan 目标
	err    error // 非 nil 时 Scan 直接失败
}

// Scan 把 values 按位置赋给 dest；类型不匹配或列数不符立即报错，
// 避免替身把「仓储扫错列」也放过去。
func (r adminRowStub) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("列数期望 %d，实际 %d", len(r.values), len(dest))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *int16:
			v, ok := r.values[i].(int16)
			if !ok {
				return fmt.Errorf("第 %d 列期望 int16，实际 %T", i, r.values[i])
			}
			*d = v
		case *string:
			v, ok := r.values[i].(string)
			if !ok {
				return fmt.Errorf("第 %d 列期望 string，实际 %T", i, r.values[i])
			}
			*d = v
		case *time.Time:
			v, ok := r.values[i].(time.Time)
			if !ok {
				return fmt.Errorf("第 %d 列期望 time.Time，实际 %T", i, r.values[i])
			}
			*d = v
		default:
			return fmt.Errorf("第 %d 列类型 %T 不被替身支持", i, dest[i])
		}
	}
	return nil
}

// adminQueryStub 是 adminQuerier 的替身：回放预设结果并记下最后一次调用的
// SQL 与参数，用于断言「0 行折成 created=false」「影响行数原样返回」这类
// 不需要真实库的口径；真实 SQL 语义由 admin_integration_test.go 覆盖。
type adminQueryStub struct {
	row     pgx.Row           // QueryRow 返回的行；nil 表示没有行
	execTag pgconn.CommandTag // Exec 返回的命令标签
	execErr error             // 非 nil 时 Exec 失败

	lastSQL string // 最后一次调用的 SQL
	lastArg []any  // 最后一次调用的参数
}

// QueryRow 记录调用并按预设返回；未设置 row 时等价于「查不到」。
func (s *adminQueryStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	s.lastSQL = sql
	s.lastArg = args
	if s.row == nil {
		return adminRowStub{err: pgx.ErrNoRows}
	}
	return s.row
}

// Exec 记录调用并返回预设标签或错误。
func (s *adminQueryStub) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.lastSQL = sql
	s.lastArg = args
	if s.execErr != nil {
		return pgconn.CommandTag{}, s.execErr
	}
	return s.execTag, nil
}

// TestAdminRepoReads 覆盖读路径的空态与错误口径：
// 没管理员/没会话是 found=false 的正常结果，真正的读库失败才包 ErrAdminStoreFailed。
func TestAdminRepoReads(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	readErr := errors.New("连接被重置")

	t.Run("没有管理员时返回 false", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{})
		user, found, err := repo.Admin(ctx)
		if err != nil || found {
			t.Fatalf("空库期望 (零值, false, nil)，实际 (%+v, %v, %v)", user, found, err)
		}
	})

	t.Run("管理员读库失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{row: adminRowStub{err: readErr}})
		if _, _, err := repo.Admin(ctx); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("没有会话时返回 false", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{})
		s, found, err := repo.Session(ctx, "missing")
		if err != nil || found {
			t.Fatalf("查不到会话期望 (零值, false, nil)，实际 (%+v, %v, %v)", s, found, err)
		}
	})

	t.Run("会话读库失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{row: adminRowStub{err: readErr}})
		if _, _, err := repo.Session(ctx, "hash"); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("冲突时 CreateAdmin 返回 created=false", func(t *testing.T) {
		// ON CONFLICT DO NOTHING + RETURNING 在冲突时扫到 pgx.ErrNoRows。
		repo := NewAdminRepo(&adminQueryStub{})
		user, created, err := repo.CreateAdmin(ctx, "hash", now)
		if err != nil || created {
			t.Fatalf("冲突期望 (零值, false, nil)，实际 (%+v, %v, %v)", user, created, err)
		}
	})

	t.Run("创建成功返回库里的行", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{row: adminRowStub{values: []any{
			int16(1), "hash", now, now,
		}}})
		user, created, err := repo.CreateAdmin(ctx, "hash", now)
		if err != nil || !created {
			t.Fatalf("期望创建成功，实际 created=%v err=%v", created, err)
		}
		if user.ID != 1 || user.PasswordHash != "hash" ||
			!user.PasswordUpdatedAt.Equal(now) || !user.CreatedAt.Equal(now) {
			t.Fatalf("返回行不符: %+v", user)
		}
	})

	t.Run("创建失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{row: adminRowStub{err: readErr}})
		if _, _, err := repo.CreateAdmin(ctx, "hash", now); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})
}

// TestAdminRepoWrites 覆盖写路径的影响行数与错误口径：
// 0 行更新管理员必须报错，删会话/删管理员 0 行仍是成功，
// 清理方法把 RowsAffected 原样交给调用方。
func TestAdminRepoWrites(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	writeErr := errors.New("磁盘写失败")

	t.Run("改密 0 行时报错", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execTag: pgconn.NewCommandTag("UPDATE 0")})
		if err := repo.UpdatePassword(ctx, "hash", now); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("0 行期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("改密命中一行时成功", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execTag: pgconn.NewCommandTag("UPDATE 1")})
		if err := repo.UpdatePassword(ctx, "hash", now); err != nil {
			t.Fatalf("期望成功，实际 %v", err)
		}
	})

	t.Run("改密写库失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		if err := repo.UpdatePassword(ctx, "hash", now); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("删除管理员失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		if err := repo.DeleteAdmin(ctx); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("写入会话失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		s := model.AdminSession{TokenHash: "hash", CSRFToken: "csrf", CreatedAt: now, ExpiresAt: now.Add(model.SessionTTL)}
		if err := repo.InsertSession(ctx, s); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("删除单条会话失败包成仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		if err := repo.DeleteSession(ctx, "hash"); !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 ErrAdminStoreFailed，实际 %v", err)
		}
	})

	t.Run("清空会话返回影响行数", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execTag: pgconn.NewCommandTag("DELETE 7")})
		rows, err := repo.DeleteAllSessions(ctx)
		if err != nil || rows != 7 {
			t.Fatalf("期望 7 行，实际 rows=%d err=%v", rows, err)
		}
	})

	t.Run("清空会话失败返回 0 与仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		rows, err := repo.DeleteAllSessions(ctx)
		if rows != 0 || !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 (0, ErrAdminStoreFailed)，实际 (%d, %v)", rows, err)
		}
	})

	t.Run("清理过期会话带 now 并返回影响行数", func(t *testing.T) {
		stub := &adminQueryStub{execTag: pgconn.NewCommandTag("DELETE 3")}
		repo := NewAdminRepo(stub)
		rows, err := repo.DeleteExpiredSessions(ctx, now)
		if err != nil || rows != 3 {
			t.Fatalf("期望 3 行，实际 rows=%d err=%v", rows, err)
		}
		if len(stub.lastArg) != 1 {
			t.Fatalf("期望以 now 作为唯一参数，实际 %+v", stub.lastArg)
		}
		if got, ok := stub.lastArg[0].(time.Time); !ok || !got.Equal(now) {
			t.Fatalf("期望以 now 作为唯一参数，实际 %+v", stub.lastArg[0])
		}
	})

	t.Run("清理过期会失败返回 0 与仓储错误", func(t *testing.T) {
		repo := NewAdminRepo(&adminQueryStub{execErr: writeErr})
		rows, err := repo.DeleteExpiredSessions(ctx, now)
		if rows != 0 || !errors.Is(err, model.ErrAdminStoreFailed) {
			t.Fatalf("期望 (0, ErrAdminStoreFailed)，实际 (%d, %v)", rows, err)
		}
	})
}
