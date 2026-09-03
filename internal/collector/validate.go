package collector

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/pkg/logger"
)

// ValidateCandidate 校验候选目录（schema、sha256、引用完整性、分组归零、回归）。
// 全部通过才允许复制进 data/ 并发布。
func ValidateCandidate(opts Options, cand *Candidate) error {
	cfg, err := LoadConfig(opts.ConfigPath, opts.SchemaDir)
	if err != nil {
		return err
	}
	if cand.Config != nil && !sameGroupStructure(cfg.EnabledGroups(), cand.Config.EnabledGroups()) {
		return newGlobal(CodeConfigInvalid, "白名单配置在采集后被修改（候选与当前配置分组不一致）")
	}

	manifestPath := filepath.Join(cand.Dir, ManifestFileName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return newGlobal(CodeNoCandidate, "候选 manifest 缺失: %v", err)
	}
	if err := ValidateSchemaFile(opts.SchemaDir, ManifestSchemaFile, raw); err != nil {
		return newGlobal(CodeSchemaFailed, "候选 manifest 未通过 schema 校验: %v", err)
	}
	m, err := parseManifest(raw)
	if err != nil {
		return newGlobal(CodeSchemaFailed, "候选 manifest 解析失败: %v", err)
	}
	curGroups := currentGroups(cfg)
	if len(m.Groups) != len(curGroups) {
		return newGlobal(CodeConfigInvalid, "候选 manifest 分组数与当前启用分组不一致")
	}
	for i := range m.Groups {
		if m.Groups[i].ID != curGroups[i].ID || m.Groups[i].Order != curGroups[i].Order {
			return newGlobal(CodeConfigInvalid,
				"候选 manifest 分组 %q 与当前启用分组不一致", m.Groups[i].ID)
		}
	}

	// 逐周快照 schema 校验（候选目录中存在的周文件）。
	weekDir := filepath.Join(cand.Dir, TermsDirName, m.Term, WeeksDirName)
	if entries, err := os.ReadDir(weekDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			p := filepath.Join(weekDir, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				return newGlobal(CodeSchemaFailed, "读取候选周文件失败: %v", err)
			}
			if err := ValidateSchemaFile(opts.SchemaDir, SnapshotSchemaFile, b); err != nil {
				return newGlobal(CodeSchemaFailed, "候选周文件 %s 未通过 schema 校验: %v", e.Name(), err)
			}
		}
	}

	// manifest 引用完整性：每个周条目都能找到文件且 sha256 一致。
	for key, entry := range m.Weeks {
		sha, src, err := resolveWeekFileSHA(opts, cand.Dir, m.Term, entry.Week)
		if err != nil {
			return err
		}
		if sha != entry.SHA256 {
			return newGlobal(CodeSchemaFailed,
				"manifest 第 %s 周 sha256 与文件不一致：manifest=%s 文件(%s)=%s",
				key, entry.SHA256, src, sha)
		}
		if entry.Week <= 0 {
			return newGlobal(CodeSchemaFailed, "manifest 第 %s 周 week 值非法", key)
		}
	}

	// 分组归零 → 失败。
	for _, g := range m.Groups {
		if g.RoomCount == 0 {
			return newGlobal(CodeGroupZero, "启用分组 %q(%s) room_count 归零，禁止发布", g.Name, g.ID)
		}
	}

	// 与上一成功版本比较分组房间数（Q97）：同分组结构下变化 >20% → 失败。
	baseline := loadLocalManifest(opts)
	if baseline != nil && baseline.Term == m.Term && sameManifestGroups(baseline.Groups, m.Groups) {
		oldByID := map[string]int{}
		for _, g := range baseline.Groups {
			oldByID[g.ID] = g.RoomCount
		}
		for _, g := range m.Groups {
			old, ok := oldByID[g.ID]
			if !ok {
				continue
			}
			if old == 0 {
				continue
			}
			ratio := math.Abs(float64(g.RoomCount-old)) / float64(old)
			if ratio > 0.20 {
				return newGlobal(CodeRegression,
					"启用分组 %q(%s) 房间数从 %d 变为 %d（变化 %.1f%% > 20%%）；需要人工检查",
					g.Name, g.ID, old, g.RoomCount, ratio*100)
			}
		}
	} else if baseline != nil && baseline.Term == m.Term {
		logger.Info("分组结构相对上一版有变化（白名单配置版本变化），重建数量基线")
	}

	logger.Info("候选校验通过：release=%s term=%s weeks=%d groups=%d",
		m.ReleaseID, m.Term, len(m.Weeks), len(m.Groups))
	return nil
}

// resolveWeekFileSHA 按 候选目录 → data/ → git HEAD 顺序查找周文件并计算哈希。
func resolveWeekFileSHA(opts Options, candDir, term string, week int) (sha, src string, err error) {
	rel := dataRelPath(term, week)
	candidates := []string{filepath.Join(candDir, rel), weekFilePath(opts.DataDir, term, week)}
	for _, p := range candidates {
		if b, rerr := os.ReadFile(p); rerr == nil {
			return sha256Hex(b), p, nil
		}
	}
	if b, gerr := gitShowFile(opts, rel); gerr == nil {
		return sha256Hex(b), "git HEAD:" + rel, nil
	}
	return "", "", newGlobal(CodeSchemaFailed,
		"manifest 引用的第 %d 周文件在候选目录/data/HEAD 中均不存在（%s）", week, rel)
}

// sameGroupStructure 比较两次加载的启用分组（id+order）是否一致。
func sameGroupStructure(a, b []Group) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Order != b[i].Order {
			return false
		}
	}
	return true
}

func sameManifestGroups(a, b []ManifestGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Order != b[i].Order {
			return false
		}
	}
	return true
}

func currentGroups(cfg *Config) []Group { return cfg.EnabledGroups() }
