package collector

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/W1ndys/easy-qfnu-kjs/internal/logger"
)

// ValidateCandidate 校验候选目录（schema、sha256、引用完整性、分组归零、回归）。
// 全部通过才允许复制进 data/ 并发布。
func ValidateCandidate(opts Options, cand *Candidate) error {
	cfg, err := LoadConfig(opts.ConfigPath, opts.SchemaDir)
	if err != nil {
		return err
	}
	// 采集后改白名单会让候选分组和当前配置对不上，不能发布。
	if cand.Config != nil && !sameGroupStructure(cfg.EnabledGroups(), cand.Config.EnabledGroups()) {
		return newGlobal(CodeConfigInvalid, "白名单配置在采集后被修改（候选与当前配置分组不一致）")
	}
	m, err := loadCandidateManifest(opts, cand, cfg)
	if err != nil {
		return err
	}
	if err := validateCandidateWeekFiles(opts, cand, m); err != nil {
		return err
	}
	if err := validateManifestWeekRefs(opts, cand, m); err != nil {
		return err
	}
	if err := rejectZeroGroups(m); err != nil {
		return err
	}
	if err := rejectRoomCountRegression(opts, m); err != nil {
		return err
	}
	logger.Info("候选校验通过：release=%s term=%s weeks=%d groups=%d",
		m.ReleaseID, m.Term, len(m.Weeks), len(m.Groups))
	return nil
}

func loadCandidateManifest(opts Options, cand *Candidate, cfg *Config) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(cand.Dir, ManifestFileName))
	if err != nil {
		return nil, newGlobal(CodeNoCandidate, "候选 manifest 缺失: %v", err)
	}
	if err := ValidateSchemaFile(opts.SchemaDir, ManifestSchemaFile, raw); err != nil {
		return nil, newGlobal(CodeSchemaFailed, "候选 manifest 未通过 schema 校验: %v", err)
	}
	m, err := parseManifest(raw)
	if err != nil {
		return nil, newGlobal(CodeSchemaFailed, "候选 manifest 解析失败: %v", err)
	}
	if err := sameEnabledGroups(m, cfg); err != nil {
		return nil, err
	}
	return m, nil
}

func sameEnabledGroups(m *Manifest, cfg *Config) error {
	curGroups := currentGroups(cfg)
	if len(m.Groups) != len(curGroups) {
		return newGlobal(CodeConfigInvalid, "候选 manifest 分组数与当前启用分组不一致")
	}
	for i := range m.Groups {
		if m.Groups[i].ID != curGroups[i].ID || m.Groups[i].Order != curGroups[i].Order {
			return newGlobal(CodeConfigInvalid, "候选 manifest 分组 %q 与当前启用分组不一致", m.Groups[i].ID)
		}
	}
	return nil
}

func validateCandidateWeekFiles(opts Options, cand *Candidate, m *Manifest) error {
	weekDir := filepath.Join(cand.Dir, TermsDirName, m.Term, WeeksDirName)
	entries, err := os.ReadDir(weekDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(weekDir, e.Name()))
		if err != nil {
			return newGlobal(CodeSchemaFailed, "读取候选周文件失败: %v", err)
		}
		if err := ValidateSchemaFile(opts.SchemaDir, SnapshotSchemaFile, b); err != nil {
			return newGlobal(CodeSchemaFailed, "候选周文件 %s 未通过 schema 校验: %v", e.Name(), err)
		}
	}
	return nil
}

func validateManifestWeekRefs(opts Options, cand *Candidate, m *Manifest) error {
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
	return nil
}

func rejectZeroGroups(m *Manifest) error {
	for _, g := range m.Groups {
		if g.RoomCount == 0 {
			return newGlobal(CodeGroupZero, "启用分组 %q(%s) room_count 归零，禁止发布", g.Name, g.ID)
		}
	}
	return nil
}

func rejectRoomCountRegression(opts Options, m *Manifest) error {
	baseline := loadLocalManifest(opts)
	if baseline == nil || baseline.Term != m.Term {
		return nil
	}
	if !sameManifestGroups(baseline.Groups, m.Groups) {
		logger.Info("分组结构相对上一版有变化（白名单配置版本变化），重建数量基线")
		return nil
	}
	oldByID := map[string]int{}
	for _, g := range baseline.Groups {
		oldByID[g.ID] = g.RoomCount
	}
	for _, g := range m.Groups {
		if err := roomCountChangedTooMuch(g, oldByID[g.ID]); err != nil {
			return err
		}
	}
	return nil
}

func roomCountChangedTooMuch(g ManifestGroup, old int) error {
	if old == 0 {
		return nil
	}
	ratio := math.Abs(float64(g.RoomCount-old)) / float64(old)
	// 同结构下房间数变化超过 20% 多半是页面或白名单异常，停止发布等人看。
	if ratio > 0.20 {
		return newGlobal(CodeRegression,
			"启用分组 %q(%s) 房间数从 %d 变为 %d（变化 %.1f%% > 20%%）；需要人工检查",
			g.Name, g.ID, old, g.RoomCount, ratio*100)
	}
	return nil
}

// resolveWeekFileSHA 按候选目录、本机快照库、仓库 data/ 的顺序查找周文件并计算哈希。
func resolveWeekFileSHA(opts Options, candDir, term string, week int) (sha, src string, err error) {
	rel := dataRelPath(term, week)
	candPath := filepath.Join(candDir, rel)
	if b, rerr := os.ReadFile(candPath); rerr == nil {
		return sha256Hex(b), candPath, nil
	}
	b, src, rerr := readPublishedFile(opts, rel)
	if rerr != nil {
		return "", "", newGlobal(CodeSchemaFailed, "manifest 引用的第 %d 周文件不存在（%s）", week, rel)
	}
	return sha256Hex(b), src, nil
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
