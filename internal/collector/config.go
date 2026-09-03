package collector

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

// Config 对应 config/rooms.json（config.schema.v1.json）。
type Config struct {
	Version int     `json:"version"`
	Groups  []Group `json:"groups"`
}

// Group 单个查询分组（白名单规则，不支持正则）。
type Group struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Order             int      `json:"order"`
	IncludePrefixes   []string `json:"include_prefixes"`
	IncludeExactNames []string `json:"include_exact_names"`
	ExcludePrefixes   []string `json:"exclude_prefixes"`
	ExcludeExactNames []string `json:"exclude_exact_names"`
}

// LoadConfig 读取并做 schema 校验 + 语义校验。
// schemaDir 为 schemas/ 目录，schema 文件名见 ConfigSchemaFile。
func LoadConfig(path, schemaDir string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, newGlobal(CodeConfigInvalid, "读取白名单配置失败: %v", err)
	}
	if err := ValidateSchemaFile(schemaDir, ConfigSchemaFile, raw); err != nil {
		return nil, newGlobal(CodeConfigInvalid, "白名单配置未通过 schema 校验: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, newGlobal(CodeConfigInvalid, "白名单配置解析失败: %v", err)
	}
	if cfg.Version != SchemaVersion {
		return nil, newGlobal(CodeConfigInvalid, "白名单配置版本不受支持: %d", cfg.Version)
	}
	seen := map[string]bool{}
	for _, g := range cfg.Groups {
		if seen[g.ID] {
			return nil, newGlobal(CodeConfigInvalid, "分组 id 重复: %s", g.ID)
		}
		seen[g.ID] = true
		if !g.Enabled {
			continue
		}
		if len(g.IncludePrefixes) == 0 && len(g.IncludeExactNames) == 0 {
			return nil, newGlobal(CodeConfigInvalid,
				"启用分组 %q(%s) 未配置任何 include 规则，将永远匹配不到房间", g.ID, g.Name)
		}
	}
	return &cfg, nil
}

// EnabledGroups 返回启用分组，按 (order 升序, 名称自然排序) 稳定排序。
func (c *Config) EnabledGroups() []Group {
	var out []Group
	for _, g := range c.Groups {
		if g.Enabled {
			out = append(out, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return naturalLess(out[i].Name, out[j].Name)
	})
	return out
}

// ResolveRoomGroup 按决策 §5 把（已规范化）房名分到唯一启用分组。
// 返回 groupID；未命中任何分组返回空字符串；命中多个分组返回错误（禁止静默裁决）。
// 匹配顺序固定：排除规则先于包含规则。
func (c *Config) ResolveRoomGroup(normalizedName string) (string, error) {
	var matched []string
	for _, g := range c.Groups {
		if !g.Enabled {
			continue
		}
		if excludedBy(g, normalizedName) {
			continue
		}
		if includedBy(g, normalizedName) {
			matched = append(matched, g.ID)
		}
	}
	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		return matched[0], nil
	default:
		return "", newGlobal(CodeGroupAmbiguous,
			"房间 %s 同时命中多个启用分组 %s；禁止按配置顺序静默裁决",
			quoteShort(normalizedName, 40), strings.Join(matched, ", "))
	}
}

func includedBy(g Group, name string) bool {
	for _, p := range g.IncludePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, e := range g.IncludeExactNames {
		if name == e {
			return true
		}
	}
	return false
}

func excludedBy(g Group, name string) bool {
	for _, p := range g.ExcludePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, e := range g.ExcludeExactNames {
		if name == e {
			return true
		}
	}
	return false
}
