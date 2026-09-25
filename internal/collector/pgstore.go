package collector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type roomSlot struct {
	day     int
	jsbh    string
	groupID string
	name    string
	node    string
	status  int
}

func publishToDB(ctx context.Context, dsn string, cand *Candidate) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer closeDB(db)
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	if err := ensurePGSchema(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := writeRelease(ctx, tx, cand); err != nil {
		return rollbackTx(tx, err)
	}
	return tx.Commit()
}

func ensurePGSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, pgSchemaSQL); err != nil {
		return fmt.Errorf("初始化 PostgreSQL 表失败: %w", err)
	}
	return nil
}

func writeRelease(ctx context.Context, tx *sql.Tx, cand *Candidate) error {
	if err := insertRelease(ctx, tx, cand); err != nil {
		return err
	}
	if err := insertGroups(ctx, tx, cand.Manifest); err != nil {
		return err
	}
	if err := insertWeeks(ctx, tx, cand); err != nil {
		return err
	}
	return switchCurrentRelease(ctx, tx, cand.Manifest.ReleaseID)
}

func insertRelease(ctx context.Context, tx *sql.Tx, cand *Candidate) error {
	raw, err := os.ReadFile(filepath.Join(cand.Dir, ManifestFileName))
	if err != nil {
		return err
	}
	m := cand.Manifest
	generated, err := time.Parse(time.RFC3339, m.GeneratedAt)
	if err != nil {
		return fmt.Errorf("解析发布时间失败: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO kjs.release (
  release_id, term, generated_at, anchor_date, anchor_week, total_weeks,
  timezone, in_teaching_calendar, is_current, manifest_body
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,false,$9)`,
		m.ReleaseID, m.Term, generated, m.Anchor.Date, m.Anchor.Week, m.Anchor.TotalWeeks,
		m.Anchor.Timezone, m.Anchor.InTeachingCalendar, string(raw))
	return err
}

func insertGroups(ctx context.Context, tx *sql.Tx, m *Manifest) error {
	for _, g := range m.Groups {
		_, err := tx.ExecContext(ctx, `
INSERT INTO kjs.release_group (release_id, group_id, name, sort_order, room_count)
VALUES ($1,$2,$3,$4,$5)`, m.ReleaseID, g.ID, g.Name, g.Order, g.RoomCount)
		if err != nil {
			return err
		}
	}
	return nil
}

func insertWeeks(ctx context.Context, tx *sql.Tx, cand *Candidate) error {
	for _, entry := range cand.Manifest.Weeks {
		if err := insertOneWeek(ctx, tx, cand, entry); err != nil {
			return err
		}
	}
	return nil
}

func insertOneWeek(ctx context.Context, tx *sql.Tx, cand *Candidate, entry *WeekEntry) error {
	path := filepath.Join(cand.Dir, dataRelPath(cand.Term, entry.Week))
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取第 %d 周快照失败: %w", entry.Week, err)
	}
	var snap WeekSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("解析第 %d 周快照失败: %w", entry.Week, err)
	}
	if err := insertWeekRow(ctx, tx, cand.Manifest.ReleaseID, entry, string(raw)); err != nil {
		return err
	}
	return insertSlots(ctx, tx, cand.Manifest.ReleaseID, entry.Week, &snap)
}

func insertWeekRow(ctx context.Context, tx *sql.Tx, releaseID string, entry *WeekEntry, body string) error {
	generated, err := time.Parse(time.RFC3339, entry.GeneratedAt)
	if err != nil {
		return err
	}
	successAt, err := time.Parse(time.RFC3339, entry.LastSuccessAt)
	if err != nil {
		return err
	}
	attempt, err := optionalTime(entry.LastAttemptAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO kjs.release_week (
  release_id, week, snapshot_id, sha256, generated_at, last_success_at,
  last_error_code, last_attempt_at, body
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		releaseID, entry.Week, entry.SnapshotID, entry.SHA256, generated, successAt,
		entry.LastErrorCode, attempt, body)
	return err
}

func insertSlots(ctx context.Context, tx *sql.Tx, releaseID string, week int, snap *WeekSnapshot) error {
	slots := slotsFromSnapshot(snap)
	const page = 200
	for start := 0; start < len(slots); start += page {
		end := start + page
		if end > len(slots) {
			end = len(slots)
		}
		if err := insertSlotPage(ctx, tx, releaseID, week, slots[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func insertSlotPage(ctx context.Context, tx *sql.Tx, releaseID string, week int, slots []roomSlot) error {
	var b strings.Builder
	args := make([]any, 0, len(slots)*8)
	b.WriteString(`INSERT INTO kjs.room_slot (release_id, week, day, jsbh, group_id, name, node, status) VALUES `)
	for i, s := range slots {
		if i > 0 {
			b.WriteString(",")
		}
		base := i * 8
		fmt.Fprintf(&b, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
		args = append(args, releaseID, week, s.day, s.jsbh, s.groupID, s.name, s.node, s.status)
	}
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}

func slotsFromSnapshot(snap *WeekSnapshot) []roomSlot {
	var slots []roomSlot
	for dayKey, day := range snap.Days {
		dayNum, err := strconv.Atoi(dayKey)
		if err != nil || day == nil {
			continue
		}
		for _, room := range day.Rooms {
			for node, status := range room.Statuses {
				slots = append(slots, roomSlot{
					day: dayNum, jsbh: room.Jsbh, groupID: room.GroupID,
					name: room.Name, node: node, status: status,
				})
			}
		}
	}
	return slots
}

func switchCurrentRelease(ctx context.Context, tx *sql.Tx, releaseID string) error {
	previous, err := currentReleaseID(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kjs.release SET is_current = false WHERE is_current`); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE kjs.release SET is_current = true WHERE release_id = $1`, releaseID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("切换当前 release 失败")
	}
	return deleteOldReleases(ctx, tx, releaseID, previous)
}

func currentReleaseID(ctx context.Context, tx *sql.Tx) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT release_id FROM kjs.release WHERE is_current`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func deleteOldReleases(ctx context.Context, tx *sql.Tx, currentID, previousID string) error {
	_, err := tx.ExecContext(ctx, `
DELETE FROM kjs.release
WHERE release_id <> $1 AND release_id <> $2`, currentID, previousID)
	return err
}

func readPublishedFromDB(ctx context.Context, dsn, rel string) ([]byte, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer closeDB(db)
	if rel == ManifestFileName {
		return currentManifestBody(ctx, db)
	}
	term, week, ok := parseWeekRel(rel)
	if !ok {
		return nil, os.ErrNotExist
	}
	return currentWeekBody(ctx, db, term, week)
}

func currentManifestBody(ctx context.Context, db *sql.DB) ([]byte, error) {
	var body string
	err := db.QueryRowContext(ctx, `SELECT manifest_body FROM kjs.release WHERE is_current`).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return []byte(body), nil
}

func currentWeekBody(ctx context.Context, db *sql.DB, term string, week int) ([]byte, error) {
	var body string
	err := db.QueryRowContext(ctx, `
SELECT w.body
FROM kjs.release_week w
JOIN kjs.release r ON r.release_id = w.release_id
WHERE r.is_current AND r.term = $1 AND w.week = $2`, term, week).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	return []byte(body), nil
}

func parseWeekRel(rel string) (string, int, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) != 4 || parts[0] != TermsDirName || parts[2] != WeeksDirName {
		return "", 0, false
	}
	name := parts[3]
	if !strings.HasPrefix(name, "week-") || !strings.HasSuffix(name, ".json") {
		return "", 0, false
	}
	week, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "week-"), ".json"))
	if err != nil {
		return "", 0, false
	}
	return parts[1], week, true
}

func optionalTime(raw *string) (any, error) {
	if raw == nil || *raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func rollbackTx(tx *sql.Tx, cause error) error {
	if err := tx.Rollback(); err != nil {
		return fmt.Errorf("%w；回滚也失败: %v", cause, err)
	}
	return cause
}

func closeDB(db *sql.DB) {
	if err := db.Close(); err != nil {
		logger.Warn("关闭数据库连接失败: %v", err)
	}
}
