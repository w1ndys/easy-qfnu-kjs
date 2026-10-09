package store

// 本文件是 data 层的集成测试：需要真实 PostgreSQL（见 deploy/compose.yaml）。
// 未设置 TEST_DATABASE_URL 时整组测试跳过，保证 go test ./... 不依赖外部服务。
// 覆盖占用明细的写入：只落教室状态、课程、周次范围、时间标志与派生标记，空值落 NULL。

import (
	"context"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestWriteOccupancyDetails 断言明细按行写入并原样读回，没写值的列落 NULL。
func TestWriteOccupancyDetails(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	roomID := createTestRoom(t, s, model.Room{ID: "it-detail-room", Name: "综合教学楼301", BuildingID: "jxlbh-1"})
	releaseID := createTestRelease(t, s)

	details := []model.OccupancyDetail{
		{
			RoomID: roomID, Weekday: 1, Block: "0102", StateKey: model.StateClass,
			Course: "西方史学史", WeekRange: "1-16", TimeFlag: "单周", Derived: false,
		},
		{
			RoomID: roomID, Weekday: 1, Block: "030405", StateKey: model.StateBorrowed,
			WeekRange: "1-16", Derived: true,
		},
	}
	if err := s.WriteOccupancyDetails(ctx, releaseID, details); err != nil {
		t.Fatalf("写入占用明细失败: %v", err)
	}

	rows := readOccupancyDetails(t, s, releaseID, roomID)
	if len(rows) != len(details) {
		t.Fatalf("读回 %d 行，期望 %d 行", len(rows), len(details))
	}

	// 第一条：四项齐全，derived 为 false
	first := rows["0102"]
	if first.StateKey != model.StateClass || first.Course != "西方史学史" || first.WeekRange != "1-16" {
		t.Errorf("第一条明细 = %+v，期望正常上课的西方史学史 1-16 周", first)
	}
	if first.TimeFlag != "单周" {
		t.Errorf("时间标志 = %q，期望 单周", first.TimeFlag)
	}
	if first.Derived {
		t.Error("第一条明细的 derived = true，期望 false")
	}

	// 第二条：课程与时间标志没写，落 NULL 后读回空串；派生标记要保留
	second := rows["030405"]
	if second.StateKey != model.StateBorrowed || second.Course != "" || second.TimeFlag != "" {
		t.Errorf("第二条明细 = %+v，期望借用且课程与时间标志为空", second)
	}
	if !second.Derived {
		t.Error("第二条明细的 derived = false，期望 true")
	}
}

// TestWriteOccupancyDetailsWithoutRows 断言空列表是正常情况：不写行也不报错。
func TestWriteOccupancyDetailsWithoutRows(t *testing.T) {
	s := openMigratedStore(t)
	ctx := context.Background()
	releaseID := createTestRelease(t, s)

	if err := s.WriteOccupancyDetails(ctx, releaseID, nil); err != nil {
		t.Fatalf("写入空明细失败: %v", err)
	}

	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM occupancy_detail WHERE release_id = $1`, releaseID).Scan(&count); err != nil {
		t.Fatalf("统计明细失败: %v", err)
	}
	if count != 0 {
		t.Errorf("写入 %d 行，期望 0 行", count)
	}
}

// createTestRelease 建一个测试学期与发布版本，供占用明细的外键使用；结束时按外键顺序清理。
func createTestRelease(t *testing.T, s *Store) string {
	t.Helper()

	ctx := context.Background()
	stamp := time.Now().Format("20060102150405.000000")
	term := "it-detail-term-" + stamp
	releaseID := "it-detail-release-" + stamp

	if _, err := s.pool.Exec(ctx, `INSERT INTO term (term, total_weeks) VALUES ($1, 20)`, term); err != nil {
		t.Fatalf("插入测试学期失败: %v", err)
	}
	// 这一版不是 current：明细用例只验证写入，不参与发布切换
	if _, err := s.pool.Exec(ctx, `
INSERT INTO release (release_id, term, generated_at, dict_version, axis_version, is_current)
VALUES ($1, $2, now(), 1, 1, false)`, releaseID, term); err != nil {
		t.Fatalf("插入测试发布失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// 明细随 release 级联删除，删掉 release 与 term 就够
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM release WHERE release_id = $1`, releaseID); err != nil {
			t.Logf("清理 release 失败: %v", err)
		}
		if _, err := s.pool.Exec(cleanupCtx, `DELETE FROM term WHERE term = $1`, term); err != nil {
			t.Logf("清理 term 失败: %v", err)
		}
	})
	return releaseID
}

// readOccupancyDetails 读回某房间的明细，按大节编码索引。
func readOccupancyDetails(t *testing.T, s *Store, releaseID, roomID string) map[string]model.OccupancyDetail {
	t.Helper()

	rows, err := s.pool.Query(context.Background(), `
SELECT block, state_key, COALESCE(course, ''), COALESCE(week_range, ''), COALESCE(time_flag, ''), derived
  FROM occupancy_detail
 WHERE release_id = $1 AND room_id = $2`, releaseID, roomID)
	if err != nil {
		t.Fatalf("读取明细失败: %v", err)
	}
	defer rows.Close()

	details := make(map[string]model.OccupancyDetail)
	for rows.Next() {
		var detail model.OccupancyDetail
		var stateKey string
		if err := rows.Scan(&detail.Block, &stateKey, &detail.Course, &detail.WeekRange,
			&detail.TimeFlag, &detail.Derived); err != nil {
			t.Fatalf("扫描明细失败: %v", err)
		}
		detail.RoomID = roomID
		detail.StateKey = model.StateKey(stateKey)
		details[detail.Block] = detail
	}
	// 遍历中途出错时不能当作读完，否则会漏掉明细行
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历明细失败: %v", err)
	}
	return details
}
