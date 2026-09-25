package collector

import (
	"encoding/json"
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

// loadLocalManifest 读取上一成功版本。
// 先读本机快照库 current；还没有 current 时读仓库 data/。都不存在返回 nil。
func loadLocalManifest(opts Options) *Manifest {
	b, _, err := readPublishedFile(opts, ManifestFileName)
	if err != nil {
		return nil
	}
	m, err := parseManifest(b)
	if err != nil {
		return nil
	}
	return m
}

// buildManifest 生成 manifest 草稿并写入候选目录。
// baseline 是上一成功版本（可为 nil）。学期切换或全新学期时周表从零构建；
// 同学期刷新则合并上一版未刷新的周条目与失败周条目（保留旧快照引用 + 记录错误码）。
func (c *Candidate) buildManifest(opts Options, baseline *Manifest, generated, compactTS string, now time.Time, cfg *Config, groupCounts map[string]int) error {
	weeks, err := c.manifestWeeks(opts, baseline, generated, compactTS)
	if err != nil {
		return err
	}
	m := &Manifest{
		SchemaVersion: SchemaVersion,
		ReleaseID:     compactTS,
		Term:          c.Term,
		GeneratedAt:   generated,
		Anchor:        c.manifestAnchor(now),
		Groups:        manifestGroups(cfg, groupCounts),
		Nodes:         manifestNodes(),
		Statuses:      manifestStatuses(),
		Weeks:         weeks,
	}
	c.Manifest = m
	return writeCandidateManifest(c.Dir, m)
}

func (c *Candidate) manifestWeeks(opts Options, baseline *Manifest, generated, compactTS string) (map[string]*WeekEntry, error) {
	weeks := map[string]*WeekEntry{}
	// 同学期刷新才沿用旧周；换学期时旧周不能混进新 manifest。
	if !c.SwitchTerm && baseline != nil && baseline.Term == c.Term {
		for k, v := range baseline.Weeks {
			weeks[k] = cloneWeekEntry(v)
		}
	}
	if err := c.putSuccessWeeks(weeks, generated, compactTS); err != nil {
		return nil, err
	}
	if err := c.putFailedWeeks(opts, baseline, weeks); err != nil {
		return nil, err
	}
	return weeks, nil
}

func (c *Candidate) putSuccessWeeks(weeks map[string]*WeekEntry, generated, compactTS string) error {
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
	return nil
}

func (c *Candidate) putFailedWeeks(opts Options, baseline *Manifest, weeks map[string]*WeekEntry) error {
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
	return nil
}

func (c *Candidate) manifestAnchor(now time.Time) Anchor {
	return Anchor{
		Date:               beijingDate(now),
		Week:               c.CurrentWeek,
		TotalWeeks:         c.TotalWeeks,
		Timezone:           DefaultTimezone,
		InTeachingCalendar: c.InCalendar,
	}
}

func manifestGroups(cfg *Config, groupCounts map[string]int) []ManifestGroup {
	groups := make([]ManifestGroup, 0, len(cfg.EnabledGroups()))
	for _, g := range cfg.EnabledGroups() {
		groups = append(groups, ManifestGroup{
			ID:        g.ID,
			Name:      g.Name,
			Order:     g.Order,
			RoomCount: groupCounts[g.ID],
		})
	}
	return groups
}

func writeCandidateManifest(dir string, m *Manifest) error {
	raw, err := marshalIndent(m)
	if err != nil {
		return newGlobal(CodeInternal, "序列化 manifest 失败: %v", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, ManifestFileName), raw, 0644); err != nil {
		return newGlobal(CodeInternal, "写入候选 manifest 失败: %v", err)
	}
	return nil
}

// copyOldWeekFile 把上一版周文件复制进候选目录。
// 先找本机快照库，再找仓库 data/。两边都没有就失败，不能发布一份缺文件的候选。
func (c *Candidate) copyOldWeekFile(opts Options, term string, week int) error {
	rel := dataRelPath(term, week)
	data, _, err := readPublishedFile(opts, rel)
	if err != nil {
		return newGlobal(CodeInternal, "失败周 %d 的上一版文件不存在（%s）", week, rel)
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
