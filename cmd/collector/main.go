// easy-qfnu-kjs 本地采集器（Issue #29 新架构）。
//
// 子命令：
//
//	collect   采集到仓库外临时候选目录（不发布）
//	validate  校验最近一次 collect 的候选（schema/哈希/分组/回归）
//	publish   发布最近一次 collect 的候选并做发布验收
//	run       完整流程 collect+validate+publish；--dry-run 只做前两步
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/W1ndys/easy-qfnu-kjs/internal/collector"
)

const usageText = `easy-qfnu-kjs 采集器

用法:
  collector <命令> [选项]

命令:
  collect     采集到仓库外临时候选目录（不发布）
  validate    校验最近一次 collect 的候选数据
  publish     发布最近一次 collect 的候选并做发布验收
  run         完整流程 collect + validate + publish

选项:
  --config <path>    白名单配置（默认 config/rooms.json）
  --data-dir <path>  数据目录（默认 data/）
  --schema-dir <path> JSON Schema 目录（默认 schemas/）
  --repo-dir <path>  仓库根目录（默认当前工作目录）
  --dry-run          run 时只执行 collect + validate，不发布
`

type cliArgs struct {
	configPath string
	dataDir    string
	schemaDir  string
	repoDir    string
	dryRun     bool
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	loadDotEnv()
	cmd, code := commandName(argv)
	if cmd == "" {
		return code
	}
	args, code := parseCollectorArgs(cmd, argv[1:])
	if code != 0 {
		return code
	}
	opts, code := buildCollectorOptions(args)
	if code != 0 {
		return code
	}
	return dispatchCollector(cmd, opts)
}

func commandName(argv []string) (string, int) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return "", 2
	}
	cmd := argv[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usageText)
		return "", 0
	}
	return cmd, 0
}

func parseCollectorArgs(cmd string, argv []string) (cliArgs, int) {
	var a cliArgs
	fs := flag.NewFlagSet("collector "+cmd, flag.ExitOnError)
	fs.StringVar(&a.configPath, "config", "config/rooms.json", "白名单配置路径")
	fs.StringVar(&a.dataDir, "data-dir", "data", "数据目录")
	fs.StringVar(&a.schemaDir, "schema-dir", "schemas", "JSON Schema 目录")
	fs.StringVar(&a.repoDir, "repo-dir", "", "仓库根目录（默认当前工作目录）")
	fs.BoolVar(&a.dryRun, "dry-run", false, "run 只执行 collect+validate")
	if err := fs.Parse(argv); err != nil {
		fmt.Fprintf(os.Stderr, "参数解析失败: %v\n", err)
		return a, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "多余的参数: %v\n", fs.Args())
		return a, 2
	}
	return a, 0
}

func buildCollectorOptions(a cliArgs) (collector.Options, int) {
	repoDir, code := absRepoDir(a.repoDir)
	if code != 0 {
		return collector.Options{}, code
	}
	stateDir, code := collectorStateDir()
	if code != 0 {
		return collector.Options{}, code
	}
	return collector.Options{
		RepoDir:     repoDir,
		ConfigPath:  joinUnderRepo(repoDir, a.configPath),
		DataDir:     joinUnderRepo(repoDir, a.dataDir),
		SchemaDir:   joinUnderRepo(repoDir, a.schemaDir),
		StateDir:    stateDir,
		DryRun:      a.dryRun,
		Username:    os.Getenv("QFNU_USERNAME"),
		Password:    os.Getenv("QFNU_PASSWORD"),
		PublishBase: envOr("PUBLISH_BASE_URL", collector.DefaultPublishBase),
		PublishRepo: os.Getenv("PUBLISH_REPO"),
		WebhookURL:  os.Getenv("FEISHU_WEBHOOK_URL"),
		WebhookSec:  os.Getenv("FEISHU_WEBHOOK_SECRET"),
		HTTPTimeout: 120 * time.Second,
	}, 0
}

func absRepoDir(repoDir string) (string, int) {
	if repoDir == "" {
		abs, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取工作目录失败: %v\n", err)
			return "", 1
		}
		return abs, 0
	}
	abs, err := filepath.Abs(repoDir)
	if err != nil {
		return repoDir, 0
	}
	return abs, 0
}

func joinUnderRepo(repoDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repoDir, path)
}

func collectorStateDir() (string, int) {
	stateDir := os.Getenv("COLLECTOR_STATE_DIR")
	if stateDir != "" {
		return stateDir, 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取主目录失败: %v\n", err)
		return "", 1
	}
	return filepath.Join(home, collector.StateDirDefaultRel), 0
}

func dispatchCollector(cmd string, opts collector.Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runCollectorCommand(ctx, cmd, opts)
	if err == errUnknownCommand {
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n%s", cmd, usageText)
		return 2
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "collector %s 失败: %v\n", cmd, err)
		return 1
	}
	return 0
}

var errUnknownCommand = fmt.Errorf("未知命令")

func runCollectorCommand(ctx context.Context, cmd string, opts collector.Options) error {
	switch cmd {
	case "collect":
		return collector.CollectCommand(ctx, opts)
	case "validate":
		return collector.ValidateCommand(ctx, opts)
	case "publish":
		return collector.PublishCommand(ctx, opts)
	case "run":
		return collector.RunCommand(ctx, opts)
	default:
		return errUnknownCommand
	}
}

// envOr 返回环境变量值；为空时返回默认值。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// loadDotEnv 从当前工作目录读取 .env（KEY=VALUE；忽略注释与空行，支持引号与 export 前缀）。
func loadDotEnv() {
	data, err := os.ReadFile(".env")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists && os.Getenv(key) != "" {
			continue // 显式环境变量优先
		}
		_ = os.Setenv(key, val)
	}
}
