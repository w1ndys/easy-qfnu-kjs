package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// parseManifest 反序列化 manifest JSON。
func parseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// loadLocalManifest 读取“上一成功版本”作为基线：
// 优先 git show HEAD:data/manifest.json，其次本地 data/manifest.json；均不可用返回 nil。
func loadLocalManifest(opts Options) *Manifest {
	if b, err := gitShowFile(opts, ManifestFileName); err == nil {
		if m, err := parseManifest(b); err == nil {
			return m
		}
	}
	if b, err := os.ReadFile(filepath.Join(opts.DataDir, ManifestFileName)); err == nil {
		if m, err := parseManifest(b); err == nil {
			return m
		}
	}
	return nil
}

// buildManifest 生成 manifest 草稿并写入候选目录。
// baseline 是上一成功版本（可为 nil）。学期切换或全新学期时周表从零构建；
// 同学期刷新则合并上一版未刷新的周条目与失败周条目（保留旧快照引用 + 记录错误码）。
func (c *Candidate) buildManifest(opts Options, baseline *Manifest, generated, compactTS string, now time.Time, cfg *Config, groupCounts map[string]int) error {
	weeks := map[string]*WeekEntry{}
	if !c.SwitchTerm && baseline != nil && baseline.Term == c.Term {
		for k, v := range baseline.Weeks {
			weeks[k] = cloneWeekEntry(v)
		}
	}

	// 成功周：新快照引用。
	for _, w := range c.SuccessWeeks {
		file := filepath.Join(c.Dir, dataRelPath(c.Term, w))
		sha, err := sha256FileHex(file)
		if err != nil {
			return newGlobal(CodeInternal, "计算第 %d 周快照哈希失败: %v", w, err)
		}
		weeks[weekKey(w)] = &WeekEntry{
			Week:          w,
			SnapshotID:    snapshotIDFor(compactTS, w),
			SHA256:        sha,
			GeneratedAt:   generated,
			LastSuccessAt: generated,
		}
	}

	// 失败周：仅同学期、且上一版有该周条目时，保留旧快照并记录错误码；
	// 同时把旧周文件复制进候选目录，保证候选整体自洽。
	for _, f := range c.Failures {
		if c.SwitchTerm {
			continue
		}
		prev := baselineWeeksGet(baseline, c.Term, f.Week)
		if prev == nil {
			continue
		}
		e := cloneWeekEntry(prev)
		code := f.Code
		e.LastErrorCode = &code
		at := f.AttemptAt
		e.LastAttemptAt = &at
		weeks[weekKey(f.Week)] = e
		if err := c.copyOldWeekFile(opts, c.Term, f.Week); err != nil {
			return err
		}
	}

	groups := make([]ManifestGroup, 0, len(cfg.EnabledGroups()))
	for _, g := range cfg.EnabledGroups() {
		groups = append(groups, ManifestGroup{
			ID:        g.ID,
			Name:      g.Name,
			Order:     g.Order,
			RoomCount: groupCounts[g.ID],
		})
	}

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		ReleaseID:     compactTS,
		Term:          c.Term,
		GeneratedAt:   generated,
		Anchor: Anchor{
			Date:               beijingDate(now),
			Week:               c.CurrentWeek,
			TotalWeeks:         c.TotalWeeks,
			Timezone:           DefaultTimezone,
			InTeachingCalendar: c.InCalendar,
		},
		Groups:   groups,
		Nodes:    manifestNodes(),
		Statuses: manifestStatuses(),
		Weeks:    weeks,
	}
	c.Manifest = m

	raw, err := marshalIndent(m)
	if err != nil {
		return newGlobal(CodeInternal, "序列化 manifest 失败: %v", err)
	}
	if err := writeFileAtomic(filepath.Join(c.Dir, ManifestFileName), raw, 0644); err != nil {
		return newGlobal(CodeInternal, "写入候选 manifest 失败: %v", err)
	}
	return nil
}

// copyOldWeekFile 把 data/（或 git HEAD）中的旧周文件复制进候选目录。
func (c *Candidate) copyOldWeekFile(opts Options, term string, week int) error {
	rel := dataRelPath(term, week)
	var data []byte
	if b, err := os.ReadFile(weekFilePath(opts.DataDir, term, week)); err == nil {
		data = b
	} else if b, err := gitShowFile(opts, rel); err == nil {
		data = b
	} else {
		return newGlobal(CodeInternal,
			"失败周 %d 的上一版文件在 data/ 与 git HEAD 中均不存在（%s）", week, rel)
	}
	path := filepath.Join(c.Dir, rel)
	if err := writeFileAtomic(path, data, 0644); err != nil {
		return newGlobal(CodeInternal, "复制失败周 %d 旧文件失败: %v", week, err)
	}
	return nil
}

// baselineWeeksGet 取基线 manifest 中同学期某周条目。
func baselineWeeksGet(baseline *Manifest, term string, week int) *WeekEntry {
	if baseline == nil || baseline.Term != term {
		return nil
	}
	return baseline.Weeks[weekKey(week)]
}

func cloneWeekEntry(e *WeekEntry) *WeekEntry {
	if e == nil {
		return nil
	}
	c := *e
	if e.LastErrorCode != nil {
		v := *e.LastErrorCode
		c.LastErrorCode = &v
	}
	if e.LastAttemptAt != nil {
		v := *e.LastAttemptAt
		c.LastAttemptAt = &v
	}
	return &c
}
