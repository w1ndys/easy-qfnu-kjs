package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// openSettingsTestDB 连真实 PostgreSQL 并重建 schema；未设置 KJS_TEST_DSN 时跳过。
// 每次重建保证 settings 只有 0001 种下的两行种子（cron_expr、building_whitelist），
// 用例之间不共享残留设置。
func openSettingsTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
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

	// 从干净库开始，确保 0005 把种子 cron 改写成 5 字段这件事由本用例的迁移带上。
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

// settingsRowsByKey 把 LoadSettings 的结果折成按键索引，失败即终止用例。
func settingsRowsByKey(ctx context.Context, t *testing.T, repo SettingsRepo) map[string]model.SettingRow {
	t.Helper()
	rows, err := repo.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	out := make(map[string]model.SettingRow, len(rows))
	for _, row := range rows {
		out[row.Key] = row
	}
	return out
}

// settingsFixture 是七项设置的一次完整提交：值都是可辨识的哨兵，
// is_secret 与 model.SecretSetting 的判定一致（服务层就是这么填的）。
func settingsFixture(prefix string) []model.SettingChange {
	return []model.SettingChange{
		{Key: model.SettingJWUsername, Value: prefix + "-user"},
		{Key: model.SettingJWPassword, Value: prefix + "-cipher", Secret: true},
		{Key: model.SettingOCRBaseURL, Value: "http://ocr:8000/" + prefix},
		{Key: model.SettingFeishuWebhook, Value: prefix + "-hook-cipher", Secret: true},
		{Key: model.SettingFeishuSignSecret, Value: prefix + "-sign-cipher", Secret: true},
		{Key: model.SettingCronExpr, Value: model.DefaultCronExpr},
		{Key: model.SettingBuildingWhitelist, Value: `[{"jxlbh":"Q10","name":"综合教学楼"}]`},
	}
}

// settingsKeyRowCount 直查某个键在表里的行数。用例要证明的是「库里存在但读不到」，
// 只断言 LoadSettings 少一行是不够的：那行可能压根没插进去，断言就会假通过。
func settingsKeyRowCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, key string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM settings WHERE key = $1", key).Scan(&count); err != nil {
		t.Fatalf("统计设置键 %s 的行数失败: %v", key, err)
	}
	return count
}

// TestSettingsRepoAgainstRealDB 覆盖 SettingsRepo 的真实 SQL 口径：
// 初始库只返回种子行、键外行不被返回、upsert 覆盖值与 is_secret、updated_at 由入参
// 决定且同一批一致、第二条失败整批回滚、delete 删除并幂等、空表返回空切片。
//
// 子测试按声明顺序执行且有状态依赖（后一步基于前一步的设置行），因此不并行。
func TestSettingsRepoAgainstRealDB(t *testing.T) {
	ctx, pool := openSettingsTestDB(t)
	repo := NewSettingsRepo(pool)

	t.Run("初始库只返回两行种子", func(t *testing.T) {
		rows, err := repo.LoadSettings(ctx)
		if err != nil {
			t.Fatalf("读取种子失败: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("迁移后应只有两条种子行，实际 %d 行: %+v", len(rows), rows)
		}
		// ORDER BY key：building_whitelist 在前、cron_expr 在后。
		if rows[0].Key != model.SettingBuildingWhitelist || rows[1].Key != model.SettingCronExpr {
			t.Fatalf("行顺序应按键升序，实际 %q, %q", rows[0].Key, rows[1].Key)
		}
		if rows[1].Value != model.DefaultCronExpr {
			t.Fatalf("0005 应把种子 cron 改写成 %q，实际 %q", model.DefaultCronExpr, rows[1].Value)
		}
		if rows[0].Value != `[]` {
			t.Fatalf("种子白名单应为 []，实际 %q", rows[0].Value)
		}
		if rows[0].Secret || rows[1].Secret {
			t.Fatalf("两项种子都不是秘密项，实际 secret=%v/%v", rows[0].Secret, rows[1].Secret)
		}

		// updated_at 原样来自库：与直接查到的列逐值相等，不被仓储改写或补零。
		var stored time.Time
		if err := pool.QueryRow(ctx,
			"SELECT updated_at FROM settings WHERE key = $1", model.SettingCronExpr).Scan(&stored); err != nil {
			t.Fatalf("读取种子 updated_at 失败: %v", err)
		}
		if !rows[1].UpdatedAt.Equal(stored) {
			t.Fatalf("updated_at 应原样返回 %s，实际 %s", stored, rows[1].UpdatedAt)
		}
		if rows[1].UpdatedAt.IsZero() {
			t.Fatal("种子行的 updated_at 不应为零值")
		}
	})

	// 键外行（历史遗留或手工插入）不出现在结果里：读取只认七个键。
	mustExec(ctx, t, pool, `
INSERT INTO settings (key, value) VALUES ('legacy_unknown_key', 'should-not-be-visible')`)

	t.Run("键外行不被返回", func(t *testing.T) {
		rows, err := repo.LoadSettings(ctx)
		if err != nil {
			t.Fatalf("读取设置失败: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("键外行不应被返回，期望 2 行，实际 %d 行: %+v", len(rows), rows)
		}
		for _, row := range rows {
			if row.Key == "legacy_unknown_key" {
				t.Fatalf("键外行 %q 被 LoadSettings 返回了", row.Key)
			}
		}
		// 直查确认那行确实还在库里：本用例要证明的是「存在但读不到」，
		// 若上面少了一行是因为插入失败，这条断言会先红。
		if got := settingsKeyRowCount(ctx, t, pool, "legacy_unknown_key"); got != 1 {
			t.Fatalf("键外行应仍在库里（count=1），实际 count=%d", got)
		}
	})

	now1 := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

	t.Run("upsert 写入七个键", func(t *testing.T) {
		if err := repo.ApplySettings(ctx, settingsFixture("first"), now1); err != nil {
			t.Fatalf("写入七项设置失败: %v", err)
		}
		rows, err := repo.LoadSettings(ctx)
		if err != nil {
			t.Fatalf("读回设置失败: %v", err)
		}
		// 仍是七行：键外那行既没被读出来，也没被误删或误写。
		if len(rows) != len(model.SettingKeys()) {
			t.Fatalf("期望 %d 行，实际 %d 行: %+v", len(model.SettingKeys()), len(rows), rows)
		}
		// 直查确认整批 upsert 没有顺手动到键外行：写路径的 ON CONFLICT 只按传入键定位。
		if got := settingsKeyRowCount(ctx, t, pool, "legacy_unknown_key"); got != 1 {
			t.Fatalf("upsert 不应删掉键外行（count=1），实际 count=%d", got)
		}
		changes := make(map[string]model.SettingChange, len(model.SettingKeys()))
		for _, change := range settingsFixture("first") {
			changes[change.Key] = change
		}
		for index, row := range rows {
			// ORDER BY key：顺序必须严格升序，不能按写入顺序返回。
			if index > 0 && rows[index-1].Key >= row.Key {
				t.Fatalf("行顺序应按键升序，实际 %q 在 %q 之后", row.Key, rows[index-1].Key)
			}
			change, ok := changes[row.Key]
			if !ok {
				t.Fatalf("读出了七键之外的行 %q", row.Key)
			}
			if row.Value != change.Value {
				t.Fatalf("%s 的值期望 %q，实际 %q", row.Key, change.Value, row.Value)
			}
			if row.Secret != change.Secret {
				t.Fatalf("%s 的 is_secret 期望 %v，实际 %v", row.Key, change.Secret, row.Secret)
			}
			if !row.UpdatedAt.Equal(now1) {
				t.Fatalf("%s 的 updated_at 期望入参 %s，实际 %s", row.Key, now1, row.UpdatedAt)
			}
		}
	})

	t.Run("updated_at 前进且同一批同一个 now", func(t *testing.T) {
		now2 := now1.Add(2 * time.Hour)
		if err := repo.ApplySettings(ctx, settingsFixture("second"), now2); err != nil {
			t.Fatalf("第二次写入失败: %v", err)
		}
		for key, row := range settingsRowsByKey(ctx, t, repo) {
			if !row.UpdatedAt.Equal(now2) {
				t.Fatalf("%s 的 updated_at 期望 %s，实际 %s", key, now2, row.UpdatedAt)
			}
			if !row.UpdatedAt.After(now1) {
				t.Fatalf("%s 的 updated_at 应前进到 %s 之后，实际 %s", key, now1, row.UpdatedAt)
			}
		}
	})

	t.Run("第二条失败整批回滚", func(t *testing.T) {
		before := settingsRowsByKey(ctx, t, repo)

		// settings.value 是 text，上限约 1 GiB，没有 4096 这类业务上界；超长值会被
		// TOAST 照常写入，所以「注入一条超长值」在真实库不会失败。要让第二条 Exec
		// 真的失败，用一个库必然拒绝的值：NUL 字节在 text/UTF8 下非法（SQLSTATE 22021）。
		// 触发的是同一条路径——第二条语句报错，整批回滚，第一条也不生效。
		const badValue = "ocr\x00probe"
		changes := []model.SettingChange{
			{Key: model.SettingJWUsername, Value: "rolled-back-user"},
			{Key: model.SettingOCRBaseURL, Value: badValue},
		}
		err := repo.ApplySettings(ctx, changes, now1.Add(3*time.Hour))
		if err == nil {
			t.Fatal("第二条写入非法时整批应失败")
		}
		if !errors.Is(err, model.ErrSettingStoreFailed) {
			t.Fatalf("仓储失败应包 ErrSettingStoreFailed，实际 %v", err)
		}

		after := settingsRowsByKey(ctx, t, repo)
		if len(after) != len(before) {
			t.Fatalf("回滚后行数应不变，期望 %d，实际 %d", len(before), len(after))
		}
		// 第一条 upsert 若提交了，这里会读到 rolled-back-user 与新时间戳。
		if got := after[model.SettingJWUsername]; got.Value != before[model.SettingJWUsername].Value ||
			!got.UpdatedAt.Equal(before[model.SettingJWUsername].UpdatedAt) {
			t.Fatalf("第一条 upsert 应被回滚，实际 %q / %s", got.Value, got.UpdatedAt)
		}
		if got := after[model.SettingOCRBaseURL]; got.Value != before[model.SettingOCRBaseURL].Value {
			t.Fatalf("失败项的值应保持原样，实际 %q", got.Value)
		}
		// 错误消息带键名便于排障，但不得携带值（秘密项的值是密文）。
		if !strings.Contains(err.Error(), model.SettingOCRBaseURL) {
			t.Fatalf("错误消息应点名失败键，实际 %q", err.Error())
		}
		if strings.Contains(err.Error(), "rolled-back-user") {
			t.Fatalf("错误消息不应包含设置值，实际 %q", err.Error())
		}
	})

	t.Run("delete 删除该键且幂等", func(t *testing.T) {
		if err := repo.ApplySettings(ctx,
			[]model.SettingChange{{Key: model.SettingCronExpr, Delete: true}},
			now1.Add(4*time.Hour)); err != nil {
			t.Fatalf("删除设置失败: %v", err)
		}
		rows := settingsRowsByKey(ctx, t, repo)
		if _, ok := rows[model.SettingCronExpr]; ok {
			t.Fatal("已删除的键不应再被 LoadSettings 返回")
		}
		if len(rows) != len(model.SettingKeys())-1 {
			t.Fatalf("删除后应剩 %d 行，实际 %d 行", len(model.SettingKeys())-1, len(rows))
		}

		// 重复删除 0 行不算错误：清除语义是幂等的。
		if err := repo.ApplySettings(ctx,
			[]model.SettingChange{{Key: model.SettingCronExpr, Delete: true}},
			now1.Add(5*time.Hour)); err != nil {
			t.Fatalf("重复删除应幂等，实际 %v", err)
		}
	})

	t.Run("同一批混写与删除", func(t *testing.T) {
		now := now1.Add(6 * time.Hour)
		changes := []model.SettingChange{
			{Key: model.SettingCronExpr, Value: model.DefaultCronExpr},
			{Key: model.SettingJWPassword, Delete: true},
		}
		if err := repo.ApplySettings(ctx, changes, now); err != nil {
			t.Fatalf("混写批次失败: %v", err)
		}
		rows := settingsRowsByKey(ctx, t, repo)
		if got := rows[model.SettingCronExpr]; got.Value != model.DefaultCronExpr || !got.UpdatedAt.Equal(now) {
			t.Fatalf("恢复的 cron 期望 %q / %s，实际 %q / %s", model.DefaultCronExpr, now, got.Value, got.UpdatedAt)
		}
		if _, ok := rows[model.SettingJWPassword]; ok {
			t.Fatal("同批里的删除应生效")
		}
	})

	t.Run("空表返回空切片而非 nil", func(t *testing.T) {
		mustExec(ctx, t, pool, "DELETE FROM settings")
		rows, err := repo.LoadSettings(ctx)
		if err != nil {
			t.Fatalf("空表读取不应报错: %v", err)
		}
		if rows == nil {
			t.Fatal("空表应返回空切片而不是 nil，避免调用方写两种判空")
		}
		if len(rows) != 0 {
			t.Fatalf("空表应返回 0 行，实际 %d 行", len(rows))
		}
	})
}
