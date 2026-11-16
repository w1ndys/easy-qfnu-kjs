package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// .env.example 两段的分隔标题，由 design「环境变量表」固定，本测试按它们切段。
// 标题只改了字面量也必须同步改这里；找不到标题就直接失败，避免文件被改坏后测试静默通过。
const (
	envAppSectionTitle  = "# --- 应用配置（与 backend/internal/config 逐字段对应，共 8 项）---"
	envOrchSectionTitle = "# --- 编排变量（仅 deploy/compose.yaml 与 nginx 模板使用，不进入应用进程）---"
)

// TestEnvExample 校验 .env.example 的应用段与 config.go 实际读取的键集合双向一致：
// 两个方向都不许有差集，且应用段键数等于 config.Config 的字段数（由反射取得，不写死 8）。
// 测试从包目录（go test 的工作目录）定位仓库根；改动 config.go 或样例文件都会在这里失败。
func TestEnvExample(t *testing.T) {
	configKeys := getenvKeysInConfig(t)
	appKeys := envExampleAppSectionKeys(t)

	t.Run("双向差集为空", func(t *testing.T) {
		// 先报 config.go 读了但样例里缺的键，再报样例里多出来的键：两个方向的差异都要能直接看出来。
		for _, key := range sortedKeys(configKeys) {
			if !appKeys[key] {
				t.Errorf("config.go 读取 %s，但 .env.example 应用段缺少这个键", key)
			}
		}
		for _, key := range sortedKeys(appKeys) {
			if !configKeys[key] {
				t.Errorf(".env.example 应用段有 %s，但 config.go 没有对应的 os.Getenv", key)
			}
		}
	})

	t.Run("键数等于 Config 字段数", func(t *testing.T) {
		wantFields := reflect.TypeOf(Config{}).NumField()
		if len(appKeys) != wantFields {
			t.Fatalf(".env.example 应用段有 %d 个键，config.Config 有 %d 个字段，必须一一对应", len(appKeys), wantFields)
		}
	})
}

// getenvKeysInConfig 用 go/ast 解析同包 config.go，收集 os.Getenv("...") 的字符串字面量键集合。
// 键写成变量或表达式时静态核对不可能成立，直接判失败而不是静默漏掉。
func getenvKeysInConfig(t *testing.T) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 config.go 失败：%v", err)
	}

	keys := make(map[string]bool)
	var nonLiteral bool
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "os" || selector.Sel.Name != "Getenv" {
			return true
		}
		if len(call.Args) != 1 {
			nonLiteral = true
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			nonLiteral = true
			return true
		}
		key, err := strconv.Unquote(lit.Value)
		if err != nil {
			nonLiteral = true
			return true
		}
		keys[key] = true
		return true
	})
	if nonLiteral {
		t.Fatalf("config.go 里的 os.Getenv 键必须是字符串字面量，否则无法与 .env.example 对齐")
	}
	if len(keys) == 0 {
		t.Fatalf("在 config.go 里没找到任何 os.Getenv 调用")
	}
	return keys
}

// envExampleAppSectionKeys 读取仓库根 .env.example，按两行分隔标题切出应用段，
// 收集其中的 KEY= 行（忽略注释与空行）。分隔标题缺失或顺序不对时直接失败。
func envExampleAppSectionKeys(t *testing.T) map[string]bool {
	t.Helper()

	path := filepath.Join("..", "..", "..", ".env.example")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}

	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	appStart, orchStart := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case envAppSectionTitle:
			if appStart < 0 {
				appStart = i
			}
		case envOrchSectionTitle:
			if orchStart < 0 {
				orchStart = i
			}
		}
	}
	// 两个标题都必须存在且应用段在前：切段依赖顺序，缺一个就可能把编排段当成应用段。
	if appStart < 0 {
		t.Fatalf(".env.example 缺少应用段分隔标题：%s", envAppSectionTitle)
	}
	if orchStart < 0 {
		t.Fatalf(".env.example 缺少编排段分隔标题：%s", envOrchSectionTitle)
	}
	if orchStart < appStart {
		t.Fatalf(".env.example 段顺序错误：%q 必须排在 %q 之前", envAppSectionTitle, envOrchSectionTitle)
	}

	keys := make(map[string]bool)
	for _, line := range lines[appStart+1 : orchStart] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, found := strings.Cut(trimmed, "=")
		if !found {
			t.Fatalf(".env.example 应用段出现无法解析的行：%q", trimmed)
		}
		keys[strings.TrimSpace(key)] = true
	}
	if len(keys) == 0 {
		t.Fatalf(".env.example 应用段没有任何 KEY= 行")
	}
	return keys
}

// sortedKeys 把键集合排成稳定顺序，测试失败时错误信息不会因 map 遍历顺序而抖动。
func sortedKeys(keys map[string]bool) []string {
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	return sorted
}
