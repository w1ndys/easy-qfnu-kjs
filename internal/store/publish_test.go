package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。
// 覆盖发布事务：写观测与明细、新版本标 current、原 current 降为 previous、更早的版本删除，
// 以及事务失败时 current 保持原样（需求 6.1、6.2）。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// insertTestTerm 写入一个测试学期：release 与 observation 都引用它。
func insertTestTerm(t *testing.T, s *Store, term string) {
	t.Helper()

	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO term (term, total_weeks) VALUES ($1, 16)`, term); err != nil {
		t.Fatalf("插入测试学期失败: %v", err)
	}
}

// publishTestRelease 写入一个测试版本，is_current 由用例决定：用来布置发布前的库状态。
func publishTestRelease(t *testing.T, s *Store, term, releaseID string, current bool) {
	t.Helper()

	if _, err := s.pool.Exec(context.Background(), `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, now(), 1, 1, $3)`, releaseID, term, current); err != nil {
		t.Fatalf("插入测试版本 %s 失败: %v", releaseID, err)
	}
}

// cleanupTestReleases 在用例结束时按 id 删掉测试版本，观测与明细随外键级联删除。
func cleanupTestReleases(t *testing.T, s *Store, releaseIDs ...string) {
	t.Helper()

	t.Cleanup(func() {
		for _, releaseID := range releaseIDs {
			// 版本可能已经被发布事务删掉，删不到不算失败
			if _, err := s.pool.Exec(context.Background(),
				`DELETE FROM release WHERE release_id = $1`, releaseID); err != nil {
				t.Logf("清理版本 %s 失败: %v", releaseID, err)
			}
		}
	})
}

// currentReleaseIDs 读库里全部 current 版本 id，按 id 排序。
// release_one_current 唯一索引保证最多一个，返回多个就是发布把两个都标成了 current。
func currentReleaseIDs(t *testing.T, s *Store) []string {
	t.Helper()

	rows, err := s.pool.Query(context.Background(),
		`SELECT release_id FROM release WHERE is_current ORDER BY release_id`)
	if err != nil {
		t.Fatalf("读取 current 版本失败: %v", err)
	}
	defer rows.Close()

	ids := make([]string, 0, 1)
	for rows.Next() {
		var releaseID string
		if err := rows.Scan(&releaseID); err != nil {
			t.Fatalf("扫描 current 版本失败: %v", err)
		}
		ids = append(ids, releaseID)
	}
	// 遍历中途出错时不能当作读完，否则会漏掉 current
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 current 版本失败: %v", err)
	}
	return ids
}

// observationCount 统计某个版本里的观测行数。
func observationCount(t *testing.T, s *Store, releaseID string) int {
	t.Helper()

	var count int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM observation WHERE release_id = $1`, releaseID).Scan(&count); err != nil {
		t.Fatalf("统计版本 %s 的观测失败: %v", releaseID, err)
	}
	return count
}

// releaseExists 判断某个版本是否还在库里。
func releaseExists(t *testing.T, s *Store, releaseID string) bool {
	t.Helper()

	var exists bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM release WHERE release_id = $1)`, releaseID).Scan(&exists); err != nil {
		t.Fatalf("查询版本 %s 是否存在失败: %v", releaseID, err)
	}
	return exists
}

// TestPublishReleaseRejectsEmptyBatch 断言没有观测的批次在开事务之前就被拒绝。
// 这条不需要数据库：空批次直接返回，连接池根本没被用到。
func TestPublishReleaseRejectsEmptyBatch(t *testing.T) {
	// 零值 Store 就够：拒绝发生在打开事务之前
	if err := (&Store{}).PublishRelease(context.Background(), PublishBatch{}); !errors.Is(err, ErrEmptyRelease) {
		t.Fatalf("错误 = %v，期望 ErrEmptyRelease", err)
	}
}

// TestPublishReleaseSwitchesCurrent 断言发布把新版本标 current、原 current 降为 previous、
// 更早的版本删掉，并且观测与占用明细都写进新版本。
func TestPublishReleaseSwitchesCurrent(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	term := createTestTerm(t, s, "it-publish-switch")
	insertTestTerm(t, s, term)
	roomID := createTestRoom(t, s, model.Room{ID: "it-publish-room", Name: "综合教学楼201", BuildingID: "jxlbh-1"})

	// 布置发布前的库状态：更早的一版、当前在位的版本，以及当前版本里已有的一周观测
	publishTestRelease(t, s, term, "it-oldest-release", false)
	publishTestRelease(t, s, term, "it-current-release", true)
	insertDay(t, s, "it-current-release", term, 1, 1, roomID, freeStates())
	cleanupTestReleases(t, s, "it-current-release", "it-oldest-release", "it-new-release")

	// 上一版房间数按 current 版本的观测算，用来与本次日志对比
	baseline, found, err := s.CurrentRoomCount(ctx)
	if err != nil {
		t.Fatalf("读取上一版房间数失败: %v", err)
	}
	if !found || baseline != 1 {
		t.Errorf("上一版房间数 = (%d, %v)，期望 (1, true)", baseline, found)
	}

	batch := PublishBatch{
		Release: model.Release{
			ID: "it-new-release", Term: term, GeneratedAt: time.Now(), DictVersion: 1, AxisVersion: 1,
		},
		Observations: []model.Observation{{
			ReleaseID: "it-new-release", RoomID: roomID, Term: term, Week: 1, Weekday: 2, Node: "03",
			StateKey: model.StateClass, Available: false, RawText: "◆",
		}},
		Details: []model.OccupancyDetail{{
			RoomID: roomID, Weekday: 2, Block: "030405", StateKey: model.StateClass,
			Course: "西方史学史", WeekRange: "1-16", TimeFlag: "单周", Derived: true,
		}},
	}
	if err := s.PublishRelease(ctx, batch); err != nil {
		t.Fatalf("发布失败: %v", err)
	}

	// 新版本独占 current；原 current 留着当 previous；更早的那版被删掉
	current := currentReleaseIDs(t, s)
	if len(current) != 1 || current[0] != "it-new-release" {
		t.Errorf("current = %v，期望只有 it-new-release", current)
	}
	if !releaseExists(t, s, "it-current-release") {
		t.Error("原 current 被删了，期望降为 previous 保留")
	}
	if releaseExists(t, s, "it-oldest-release") {
		t.Error("更早的版本还在，期望被删除")
	}

	// 新版本拿到这次写入的观测与明细，旧版本的观测不受影响
	if count := observationCount(t, s, "it-new-release"); count != 1 {
		t.Errorf("新版本观测 %d 行，期望 1 行", count)
	}
	if count := observationCount(t, s, "it-current-release"); count != 12 {
		t.Errorf("上一版观测 %d 行，期望保留原来的 12 行", count)
	}
	details := readOccupancyDetails(t, s, "it-new-release", roomID)
	detail, ok := details["030405"]
	// 明细按大节落库：课程、周次范围与派生标记原样读回
	if !ok || !detail.Derived || detail.Course != "西方史学史" || detail.WeekRange != "1-16" {
		t.Errorf("明细 = %+v（found=%v），期望派生且带课程与周次范围", detail, ok)
	}

	// 发布后 current 换成新版本，房间数仍是一间
	if rooms, found, err := s.CurrentRoomCount(ctx); err != nil || !found || rooms != 1 {
		t.Errorf("发布后房间数 = (%d, %v, %v)，期望 (1, true, nil)", rooms, found, err)
	}
}

// TestPublishReleaseKeepsCurrentOnFailure 断言发布事务失败时 current 保持原样（需求 6.2）。
func TestPublishReleaseKeepsCurrentOnFailure(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	term := createTestTerm(t, s, "it-publish-fail")
	insertTestTerm(t, s, term)
	roomID := createTestRoom(t, s, model.Room{ID: "it-publish-fail-room", Name: "综合教学楼202", BuildingID: "jxlbh-1"})

	publishTestRelease(t, s, term, "it-current-release", true)
	insertDay(t, s, "it-current-release", term, 1, 1, roomID, freeStates())
	cleanupTestReleases(t, s, "it-current-release", "it-failed-release")

	// 同一个主键写两行：COPY 写第二行时撞主键，整个事务必须回滚
	observation := model.Observation{
		ReleaseID: "it-failed-release", RoomID: roomID, Term: term, Week: 1, Weekday: 1, Node: "01",
		StateKey: model.StateFree, Available: true,
	}
	batch := PublishBatch{
		Release: model.Release{
			ID: "it-failed-release", Term: term, GeneratedAt: time.Now(), DictVersion: 1, AxisVersion: 1,
		},
		Observations: []model.Observation{observation, observation},
	}
	if err := s.PublishRelease(ctx, batch); err == nil {
		t.Fatal("重复主键竟然发布成功，事务没起作用")
	}

	// 失败后 current 仍是原来那一版：新版本一行都没留下，旧观测也还在
	current := currentReleaseIDs(t, s)
	if len(current) != 1 || current[0] != "it-current-release" {
		t.Errorf("current = %v，期望保持 it-current-release", current)
	}
	if releaseExists(t, s, "it-failed-release") {
		t.Error("失败的版本留下了记录，期望整个事务回滚")
	}
	if count := observationCount(t, s, "it-current-release"); count != 12 {
		t.Errorf("上一版观测 %d 行，期望保持原来的 12 行", count)
	}
}
