---
name: easy-qfnu-kjs-manual-data-refresh
description: 开发者明确要求手动刷新或发布 easy-qfnu-kjs 教室快照数据时，按安全采集、校验、发布和代理验收流程执行。
---

# easy-qfnu-kjs 手动数据源更新

## 触发条件

仅在开发者明确要求“更新数据源”“刷新教室快照”“重新采集并发布”等手动数据更新时使用。普通代码修改、数据结构讨论或定时任务说明不触发本流程。

命令语义：

- “更新数据源 / 刷新数据”默认执行完整 `collect → validate → publish → 生产验收`。
- “只采集 / 试跑 / dry-run / 不发布”只执行 `collect + validate`。
- “采集后等确认”不得调用 `publish`。

## 不可违反的边界

- 数据源只能由 Go 采集器生成；禁止手工编辑 `data/manifest.json` 或 `data/terms/**/weeks/*.json`。
- 禁止打印、复制或提交 `QFNU_USERNAME`、`QFNU_PASSWORD`、Cookie、Token、OCR 原图、完整上游 HTML、Webhook 密钥。
- 采集失败或校验失败时不得覆盖当前已发布数据。
- 禁止 `git reset --hard`、`git clean -fd`、强制推送、自动回滚或删除旧快照。
- 发布前保留并尊重开发者已有工作区改动；不要把无关改动混入数据提交。
- 代理是本机运行条件，不写入仓库配置、不写入快照。

## 执行前检查

在仓库根目录执行：

```bash
cd /home/w1ndys/github/w1ndys/easy-qfnu-kjs
git status --short --branch
```

确认当前分支为 `main`，且没有未授权的 tracked/untracked 改动。工作区不干净时，报告具体路径并停止发布；不要替开发者清理或暂存文件。

只检查 `.env` 是否存在，不读取或回显其内容：

```bash
test -f .env
```

采集器会从当前目录的 `.env` 加载 `QFNU_USERNAME`、`QFNU_PASSWORD` 和 `OCR_CMD`。缺少账号或 OCR 依赖时，让采集器返回明确错误；不要在命令行中补写密码。

构建到仓库外，避免生成根目录 `collector` 文件：

```bash
go build -o /tmp/easy-qfnu-kjs-collector ./cmd/collector
```

## 代理与生产地址

本机访问 Vercel 默认域名、GitHub API 和生产验收时使用 HTTP 代理 `127.0.0.1:7890`：

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
export NODE_USE_ENV_PROXY=1
```

对 `curl` 显式使用 `--proxy http://127.0.0.1:7890` 和 `--max-time 20`。Vercel CLI 必须带 `NODE_USE_ENV_PROXY=1`。不要把这些变量提交到仓库。上游教务系统是否需要代理按现有采集器行为处理，不为了本次运行临时改网络代码。

当前 Vercel 默认地址为 `https://easy-qfnu-kjs.vercel.app`。如果本次要验收自定义域名，则以 `.env` 中明确配置的 `PUBLISH_BASE_URL` 为准；未配置时遵循 `internal/collector` 的默认值。不要擅自改域名，也不要把 URL 配置提交进代码。

## 采集命令

### 仅演练，不发布

```bash
/tmp/easy-qfnu-kjs-collector run --dry-run
```

要求输出显示登录、学期/教学周解析、目标周采集、白名单分组、Schema/哈希/数量校验均成功。候选目录留在仓库外；不得手工复制候选文件进 `data/`。

### 完整手动更新

开发者明确要求更新并发布时：

```bash
HTTP_PROXY=http://127.0.0.1:7890 \
HTTPS_PROXY=http://127.0.0.1:7890 \
NODE_USE_ENV_PROXY=1 \
/tmp/easy-qfnu-kjs-collector run
```

`run` 负责锁、重试预算、候选目录、校验、Git 基线同步、数据提交推送和发布验收。不要绕过采集器直接 `git add data`。

只有开发者明确要求分步操作时，才使用：

```bash
/tmp/easy-qfnu-kjs-collector collect
/tmp/easy-qfnu-kjs-collector validate
/tmp/easy-qfnu-kjs-collector publish
```

`publish` 只能发布最近一次已校验候选；候选路径和错误信息可以记录，但必须脱敏。

## 发布后验收

采集器内置验收包括 GitHub check-runs、生产 `manifest.release_id` 轮询和固定样例查询。完成后仍需用代理确认公开接口没有超时：

```bash
BASE_URL="${PUBLISH_BASE_URL:-https://easy-qfnu-kjs.vercel.app}"
curl --proxy http://127.0.0.1:7890 --fail-with-body --silent --show-error --max-time 20 \
  "$BASE_URL/api/v1/manifest" | jq -e '{term,release_id,weeks:(.weeks|length)}'
curl --proxy http://127.0.0.1:7890 --fail-with-body --silent --show-error --max-time 20 \
  "$BASE_URL/api/v1/context" | jq -e '{term,date,weekday,week}'
```

如果 `.env` 中配置了 `PUBLISH_BASE_URL`，将 `BASE_URL` 设置为同一个值后再验收。再用 manifest 中第一启用分组、有效周和星期执行一次 `empty-classrooms` 样例查询；必要时执行 `full-day-status`。所有请求都必须有超时限制，不能把账号或原始响应写入日志。

最后检查：

```bash
git status --short --branch
git log -1 --oneline
```

## 失败处理

- `collect`/`validate` 失败：确认旧 `data/` 未被改动，保留仓库外候选和 `logs/collector.log`，报告采集器错误码。
- `publish` 推送成功但 GitHub/Vercel 验收失败：不自动回滚；记录 commit SHA、release ID、失败阶段和生产地址，等待人工处理。
- 生产接口返回 5xx/超时：先区分代理、部署状态和数据校验问题；不要重复发布制造多个数据提交。
- 任何报告只写 term、release_id、周次、房间数量、错误码和 URL，不写凭据、Cookie、Token 或原始页面。

## 完成报告

报告以下可验证事实：

1. 采集学期、成功周、分组/房间数量；
2. Schema、哈希和回归校验结果；
3. 数据提交 commit SHA 与 release ID；
4. GitHub/Vercel 验收结果；
5. 代理访问的 manifest、context 和样例查询状态。

没有实际执行的检查不得声称通过。
