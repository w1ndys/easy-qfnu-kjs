package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LogRotator implements io.Writer backed by a JSON-line log file under dir.
//
// Behaviour (collector logging contract):
//   - Active file is always <dir>/collector.log (append mode).
//   - When the calendar day (Asia/Shanghai) changes, the active file is
//     archived as <dir>/collector-YYYYMMDD.log and a fresh collector.log is
//     started. The archive date is the day the rotated content belongs to.
//   - Archives older than retentionDays are removed.
//   - The directory is created lazily on the first write, so merely importing
//     the package never touches the filesystem.
type LogRotator struct {
	mu      sync.Mutex
	dir     string
	maxSize int64 // bytes; 0 = unlimited
	file    *os.File
	size    int64
	day     string // Asia/Shanghai YYYYMMDD of the active collector.log content
}

const (
	activeName     = "collector.log"
	archivePrefix  = "collector-"
	archiveSuffix  = ".log"
	retentionDays  = 30
	dateLayout     = "20060102"
	maxArchiveKeep = 90 // safety net for cleanup iteration
)

// shanghai is the collector's canonical timezone. Falls back to a fixed +08:00
// offset when the system zoneinfo database is unavailable.
var shanghai = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}()

// NewLogRotator creates a LogRotator writing to dir. maxSizeMB limits the
// active file size before rotation (0 = no size limit); daily date rotation
// happens regardless.
func NewLogRotator(dir string, maxSizeMB int) *LogRotator {
	return &LogRotator{
		dir:     dir,
		maxSize: int64(maxSizeMB) * 1024 * 1024,
	}
}

func dayOf(t time.Time) string {
	return t.In(shanghai).Format(dateLayout)
}

func (r *LogRotator) openLocked() error {
	if err := os.MkdirAll(r.dir, 0755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	today := dayOf(time.Now())
	path := filepath.Join(r.dir, activeName)

	// An existing collector.log may hold content from a previous day (the
	// process that wrote it exited before midnight). Archive it by the mtime's
	// day before appending today's lines.
	if info, err := os.Stat(path); err == nil {
		if mday := dayOf(info.ModTime()); mday != today && info.Size() > 0 {
			if err := r.archiveLocked(mday); err != nil {
				return err
			}
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	r.file = f
	r.day = today
	r.size = 0
	if info, err := f.Stat(); err == nil {
		r.size = info.Size()
	}

	r.cleanupLocked(today)
	return nil
}

// archiveLocked moves collector.log to collector-YYYYMMDD.log where YYYYMMDD
// is the day of the content being archived. When the target archive already
// exists (e.g. a mid-day size rotation), a numeric suffix is appended so no
// earlier content is overwritten.
func (r *LogRotator) archiveLocked(day string) error {
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	src := filepath.Join(r.dir, activeName)
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dst := filepath.Join(r.dir, archivePrefix+day+archiveSuffix)
	for seq := 1; ; seq++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		if seq > maxArchiveKeep {
			return fmt.Errorf("归档日志文件失败: 同一天归档文件过多")
		}
		dst = filepath.Join(r.dir, fmt.Sprintf("%s%s-%d%s", archivePrefix, day, seq, archiveSuffix))
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("归档日志文件失败: %w", err)
	}
	return nil
}

// cleanupLocked removes archived collector-*.log files older than
// retentionDays (by the date encoded in their name).
func (r *LogRotator) cleanupLocked(today string) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	cutoff, err := time.ParseInLocation(dateLayout, today, shanghai)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, archivePrefix) || !strings.HasSuffix(name, archiveSuffix) {
			continue
		}
		datePart := strings.TrimSuffix(strings.TrimPrefix(name, archivePrefix), archiveSuffix)
		if idx := strings.IndexAny(datePart, "-_"); idx > 0 {
			datePart = datePart[:idx]
		}
		day, err := time.ParseInLocation(dateLayout, datePart, shanghai)
		if err != nil {
			continue
		}
		if cutoff.Sub(day) > retentionDays*24*time.Hour {
			names = append(names, filepath.Join(r.dir, name))
		}
	}
	sort.Strings(names)
	for _, p := range names {
		_ = os.Remove(p)
	}
}

func (r *LogRotator) Write(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}

	today := dayOf(time.Now())
	if today != r.day {
		if err := r.archiveLocked(r.day); err != nil {
			return 0, err
		}
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}

	if r.maxSize > 0 && r.size+int64(len(p)) > r.maxSize && r.size > 0 {
		// Size rotation mid-day: archive under today's name (appending to an
		// existing archive for the same day if one already exists).
		if err := r.archiveLocked(r.day); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(filepath.Join(r.dir, activeName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			return 0, fmt.Errorf("重新打开日志文件失败: %w", err)
		}
		r.file = f
		r.day = today
		r.size = 0
		r.cleanupLocked(today)
	}

	n, err = r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the active file if open.
func (r *LogRotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}
