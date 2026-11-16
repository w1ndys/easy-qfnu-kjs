# 内网验收记录：deploy-ops（2026-11-16）

日期：2026-11-16
状态：**进行中** —— 本地脚本、镜像构建与 `task deploy:check` 门禁已就绪；内网六节演练待执行，各节「证据」栏均为待填。其中 §2 首次初始化、§3 业务版本回退、§6 并发演练依赖管理端页面，而**管理端页面尚未交付**（当前 `frontend/src` 只有公开页，`/admin` 落到 SPA 的 404 页）；这三节在页面交付前需按 §2 列出的管理 API 手动完成，不能当作已验证。

范围：单机、单副本四服务（`web` / `server` / `postgres` / `backup`）的内网部署、首次初始化、业务版本回退、镜像回退、数据库恢复与并发演练。
依据：[design.md](../specs/2026-11-16-deploy-ops/design.md) 的「部署、首次初始化与回退演练」A–E 段与 Test Strategy C；命令名与参数以当前 `Taskfile.yml` 为准。
纪律：本文件是待填骨架，不写未发生的记录。每节「证据」栏在内网执行后补日期、操作人、命令输出摘要与佐证文件路径；密钥、口令等敏感值一律只记路径或脱敏值。

## 命令速查

| target | 用途 |
|---|---|
| `task deploy:env` | 首次从 `.env.example` 生成仓库根 `.env`（已存在则只提示、不覆盖），随后填 `POSTGRES_PASSWORD` |
| `task deploy:check-config` | 快速校验（不构建镜像）：compose 配置合法 + `.env.example` 与 `config` 对齐 |
| `task deploy:check` | 部署门禁：配置 → 对齐 → `sh -n` → 构建两个镜像 → `nginx -t`（独立 target，不在 `gate` 里） |
| `task up` | 构建并启动 `web` + `server` + `postgres` (+ `backup`) |
| `task ps` | 查看四个服务状态（含已退出的 `Exited`） |
| `task logs service=server` | 跟踪日志；`service=web\|server\|postgres\|backup` 只看一个 |
| `task deploy:health` | 体检：容器状态 + 经 `web` 的 `/healthz`、`/readyz` + 最近备份时间（超 26 小时非零退出） |
| `task deploy:backup` | 手动备份一次，产物 `backup/kjs-<UTC>.dump` 并更新 `backup/.last_success` |
| `task deploy:restore file=… target=… CONFIRM=yes` | 恢复备份到显式目标库；`file=`/`target=` 必填，缺 `CONFIRM=yes` 被拒绝 |
| `task deploy:secret-backup` | 导出加密密钥到 `backup/secret/secret.key`（权限 `600`，内容不进 stdout/日志） |
| `task deploy:rollback tag=<tag>` | `web` 与 `server` 成对回退到同一 tag（`--no-build`，不重建镜像） |
| `task deploy:smoke` | opt-in 冒烟：临时 project、临时卷、宿主端口 18081，验完自行清理 |
| `task restart service=server` | 重启指定服务，默认 `server`（启动时会先跑一次清理） |
| `task gate` | 提交前 7 项门禁（后端 test/test-race/vet/lint + 前端 test/lint/build） |

## 验收环境（待内网执行后填写）

| 项 | 值 |
|---|---|
| 内网主机 / IP | 待填 |
| Docker / compose 版本 | 待填 |
| 仓库路径 | `/opt/easy-qfnu-kjs`（或实际路径，待填） |
| `BIND_ADDR:SERVER_PORT` | 待填（内网验收建议 `0.0.0.0:8080`） |
| `IMAGE_TAG` | 待填 |
| 操作人 | 待填 |

## 拓扑口径

- **宿主端口只有 `web` 一个**：`${BIND_ADDR}:${SERVER_PORT}` → 容器 `8080`；`server` 只在 compose 内网监听 `18080`；`postgres`、`backup` 没有任何宿主端口。外部流量一律经 `web` 进入，`docker compose config` 里 `ports` 只应出现在 `web`。
- **`/api` 与探针都经 `web` 反代**：`/api/`、`/healthz`、`/readyz` 由 nginx 反代到 `server:18080`；宿主侧探针看到的是「nginx → server」全链路状态。`/api` 下未注册路径仍是 `server` 的 404，不会被兜成 `index.html`。
- **`TRUST_PROXY_HEADERS` 必须为 `true`**（`.env` 留空即 compose 默认 `true`）：`web` 会透传 `X-Forwarded-For` / `X-Real-IP`；置 `false` 会让限流与访问统计看到的客户端 IP 全是 `web` 容器地址。只有绕过 `web` 直连 `server` 排障时才临时置 `false`。
- **`ADMIN_COOKIE_SECURE`**：内网 HTTP 阶段用 `false`（否则浏览器丢弃管理会话 Cookie）；TLS 终止之后改 `true`。
- **`.env` 在仓库根**：compose 以 `deploy/` 为项目目录，默认只找 `deploy/.env`，所以 compose 命令一律显式 `-f deploy/compose.yaml --env-file .env`（Taskfile 的 `COMPOSE` 已统一带上；`task up/ps/down/logs` 在缺 `.env` 时直接拒绝执行）。
- **镜像 tag 成对**：`web` 与 `server` 共用同一个 `IMAGE_TAG`，回退时两个服务一起换，不出现「新前端 + 旧后端」。

## 门禁口径

- `task gate` 仍是 7 项，不因部署而变：后端 `test`、`test-race`、`vet`、`lint` + 前端 `test`、`lint`、`build`。
- `task deploy:check` 是**独立 target，不在 `gate` 的依赖里**（已定）。它跑「`docker compose config -q` → `.env.example` 与 `config` 对齐测试 → `deploy/**/*.sh` 逐个 `sh -n` → 构建 `web` 与 `server` 两个镜像 → 渲染后的 nginx 配置 `nginx -t`」，任一步失败整体非零退出。
- 触发条件：改编排（`deploy/compose.yaml`）、镜像（`deploy/Dockerfile.*`）、nginx 配置（`deploy/nginx/`）、部署脚本（`deploy/**/*.sh`）或 `.env.example` 时，**必须**追加跑 `task deploy:check`。
- 分工理由：`gate` 要能在没有 Docker 的机器与 CI 上跑完；`deploy:check` 含镜像构建、分钟级，所以保持手动或 CI 可选，不并入 `gate`。
- 不构建镜像的快速入口是 `task deploy:check-config`（前两步），适合改配置后的即时自检。

## 1. 内网部署

### 前置条件

- 单台内网主机：Docker 29 + compose v2；≥4 vCPU / 8 GB 内存；磁盘 ≥60 GB（数据库 + 每份 dump 数百 MB × 7 天 + 余量）；固定内网 IP；能出网访问强智教务与 OCR。
- 代码在 `/opt/easy-qfnu-kjs`（或实际路径），下面所有命令在**仓库根**执行。
- `.env` 由 `task deploy:env` 生成：填 `POSTGRES_PASSWORD`；内网验收设 `BIND_ADDR=0.0.0.0`、`SERVER_PORT=8080`、`ADMIN_COOKIE_SECURE=false`；`TRUST_PROXY_HEADERS` 留空（默认 `true`）。

### 步骤

```bash
task deploy:env
# 编辑 .env：POSTGRES_PASSWORD=<真实口令>；BIND_ADDR=0.0.0.0；SERVER_PORT=8080
#            ADMIN_COOKIE_SECURE=false；TRUST_PROXY_HEADERS 留空
task deploy:check
task up
task ps
task deploy:backup
task deploy:health

# 宿主侧复验（把 127.0.0.1:8080 换成实际 BIND_ADDR:SERVER_PORT）
curl -fsS http://127.0.0.1:8080/ | head -c 200
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/admin
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/api/not-registered
```

### 期望结果

- `task deploy:check` 退出码 0：编排配置合法、`.env.example` 与 `config` 对齐、`deploy/**/*.sh` 全部 `sh -n` 通过、`web` 与 `server` 两个镜像构建成功、渲染后的 nginx 配置 `nginx -t` 通过。
- `task ps`：`web`、`server`、`postgres` 为 `healthy`，`backup` 为 `running`；`Exited` 的服务同样可见（`ps --all`）。
- `task deploy:health`：经 `web` 宿主端口探测 `/healthz`、`/readyz` 均为 2xx；最近成功备份距今 ≤ 26 小时（93600 秒）。首次部署尚无备份时，先跑 `task deploy:backup` 再判定。
- `GET /` 返回 200 且含 `<div id="root">`；`/admin` 返回 200 的 `index.html`（SPA 兜底，内容是 404 页）；`/api/not-registered` 返回 404 且正文为 `404 page not found`（反代不兜底）。
- 浏览器访问 `http://<内网IP>:8080/` 公开查询页可用；`/admin` 当前只落到 SPA 的 404 页（管理端页面尚未交付）。
- 连续两周：`task logs service=server` 无 ERROR 级启动失败；`task logs service=web` 无 4xx/5xx 风暴；同步记录（`GET /api/admin/runs`）显示每日同步成功；`ls backup/*.dump` 每天多一份、超过 7 天的被清掉。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（`task ps` 四服务状态、`task deploy:health` 探针状态码与备份距今秒数）：
- 佐证（截图或日志片段路径）：

## 2. 首次初始化

> **状态：待管理端页面交付后执行（或按下列管理 API 手动完成）。** 管理端页面尚未交付：当前 `frontend/src` 只有公开页（`/`、`/day/:roomId?`、`/screen`、`*`），打开 `http://<内网IP>:8080/admin` 会落到 SPA 的 404 页，不能按页面提示完成初始化与设置。

### 前置条件

- 第 1 节通过，`task ps` 中 `server` 为 `healthy`。
- 未初始化过：`/api/admin/state` 返回 `initialized: false`。
- 管理 API 可从宿主经 `web` 访问：`http://<内网IP>:8080/api/admin/*`。
- OCR 请求超时不是业务设置：用 `.env` 的 `OCR_TIMEOUT`（默认 `10s`，改后 `task up` 重启 `server` 生效）。

### 步骤

```bash
curl -s http://127.0.0.1:8080/api/admin/state      # 期望 "initialized": false
```

1. 初始化：`POST /api/admin/init`（请求体 `password`、`password_confirm`）替代页面引导；初始化响应里的 `encryption_key` 是明文密钥唯一一次出口（`backend/internal/api/admin.go` 的 `adminInitResponse`），按响应当场记录/保管，不写进本文件。
2. 初始化完成后立即导出加密密钥并离线保管：

```bash
task deploy:secret-backup
ls -l backup/secret/secret.key
```

3. 登录：`POST /api/admin/session`（固定账号 `admin` + 密码）建立会话，响应给 `csrf_token` 并下发 Cookie `eqk_admin_session`；之后写端点都要带 `X-CSRF-Token`（值也可从 `GET /api/admin/state` 取）与该 Cookie，见 `backend/internal/api/admin_middleware.go`。业务设置用 `GET/PUT /api/admin/settings` 提交 7 个键：`jw_username`、`jw_password`、`ocr_base_url`、`feishu_webhook`、`feishu_sign_secret`、`cron_expr`、`building_whitelist`（每条 = 楼编号 `jxlbh` + 展示名 `name`）。
4. 触发同步：`POST /api/admin/sync`（无请求体，写端点仍要 CSRF 与 Cookie），等轮次推进到发布成功；观察 `task logs service=server`。
5. 公开页做一次查询：

```bash
curl -s 'http://127.0.0.1:8080/api/query?date_offset=0' | head -c 300
```

### 期望结果

- 初始化前 `state` 返回 `initialized: false`；初始化成功后同一端点变为 `true`。
- `task deploy:secret-backup` 产出 `backup/secret/secret.key`，权限 `600`；stdout 只有路径/权限/字节数，不含密钥内容。产物随即移到离线介质，**不与数据库备份放在同一处**（密钥丢了，库里加密保存的教务密码与 OCR 密钥就解不开）。
- 设置保存成功；`GET /api/admin/settings` 回显与提交一致（页面交付后：刷新 `/admin` 仍为已登录状态、设置回显一致）。
- 手动同步轮次推进到发布成功；公开页查询返回成功信封（`"code": 0`）并能查到空教室。
- 运维记录里写下「密钥位置、备份位置、`.env` 关键取值（脱敏）」。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（`initialized` 前后取值、`ls -l` 的权限、首次同步轮次 ID、查询结果摘要）：
- 佐证（截图或日志片段路径；不要记录密钥内容）：

## 3. 业务版本回退

> **状态：待管理端页面交付后执行（或按管理 API 手动完成）。** 页面未交付前用 `GET /api/admin/releases` 取版本列表、`POST /api/admin/releases/{release_id}/rollback` 回退（写端点带 `X-CSRF-Token` 与 Cookie `eqk_admin_session`，同 §2）。

### 前置条件

- 第 2 节完成；已有 ≥2 个版本（至少两次成功同步/发布）。
- 能建管理会话：页面交付后用管理端登录；页面未交付前按 §2 用 `POST /api/admin/session` 登录。

### 步骤

1. `GET /api/admin/releases`（页面交付后为「版本列表」）记录**当前版本 ID**与发布时间，以及版本数量（应为 2）。
2. 页面交付后选上一版本 → 版本对比；页面未交付时核对两个版本的 ID 与发布时间，确认差异只涉及本轮发布内容。
3. `POST /api/admin/releases/{release_id}/rollback`（页面交付后为点「一键回退」），记录提交时间。
4. 立即在新标签打开公开页做一次查询（不要等缓存过期）。
5. 复验容器与探针未被回退动作影响：

```bash
task deploy:health
task ps
```

6. 回退耗时 = 首次查询命中回退版本的时间 − 提交回退的时间；记录操作人。

### 期望结果

- 回退提交后**立即生效**：三张 30 秒缓存被清空，公开页首次查询即命中回退版本。
- 版本列表仍只有两个版本；查询、全天状态、大屏三处数据一致。
- 回退只改库内数据、不重启容器：`task ps` 状态不变，`task deploy:health` 仍通过。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（回退前后版本 ID、版本数量、回退耗时、`deploy:health` 结果）：
- 佐证（截图或日志片段路径）：

## 4. 镜像回退

### 前置条件

- 第 1 节通过；`web` 与 `server` 共用同一个 `IMAGE_TAG`。
- 至少一次「带 tag 的发布」留下了旧镜像：`docker image ls easy-qfnu-kjs-web easy-qfnu-kjs-server` 能看到新旧 tag。
- 已知当前 `.env` 的 `IMAGE_TAG` 与上一个成功 tag。

### 步骤

```bash
NEW_TAG=20261116-1                                          # 新 tag（换成实际值）
BAD_TAG=20261116-2                                          # 故意起不来的构建 tag

grep '^IMAGE_TAG=' .env
docker image ls easy-qfnu-kjs-web easy-qfnu-kjs-server      # 记录新旧 tag 与 digest

IMAGE_TAG="$NEW_TAG" task up                                # 发布新 tag（构建两个镜像）
task deploy:health
curl -fsS http://127.0.0.1:8080/readyz

IMAGE_TAG="$BAD_TAG" task up                                # 模拟失败发布
task ps                                                     # 看 Restarting / Exited

task deploy:rollback tag="$NEW_TAG"                         # 回到上一个成功 tag，成对回退，不重建
task deploy:health
curl -fsS http://127.0.0.1:8080/readyz
curl -s 'http://127.0.0.1:8080/api/query?date_offset=0' | head -c 300
```

说明：`task up` 带 `--build`，所以 `$BAD_TAG` 必须是真的会起不来的构建（例如临时改坏配置或镜像），不能用「不存在的 tag」假装失败。

### 期望结果

- `IMAGE_TAG="$NEW_TAG" task up` 后 `task deploy:health` 通过（两条探针 2xx、备份 ≤26 小时）。
- `$BAD_TAG` 后：`/readyz` 非 2xx，或 `task ps` 看到 `web`/`server` 处于 `Restarting`/`Exited`（`on-failure:5` 失败 5 次后停在可见状态）。
- `task deploy:rollback tag="$NEW_TAG"` 只执行 `up -d --no-build web server`，两个服务一起回到同一个 tag（不重建镜像）；`/readyz` 回到 200，首页与一次空教室查询正常；回退后 `web` 的 `resolver 127.0.0.11 valid=10s` 会在 10 秒内重新解析 `server` 的新地址，不应出现持续 502。
- `.env` 里的 `IMAGE_TAG` 不会跟着回退（target 会提示）；下一次 `task up` 前手动改成同一个 tag，否则会按 `.env` 的旧值 `--build` 覆盖回退结果。
- 镜像回退**不回数据库**：跨迁移回退前先 `task deploy:backup`，并确认旧镜像内嵌的迁移集不小于库内已执行的最大版本（本仓库迁移只增，`0001`–`0005`）。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（新旧 tag/digest、`task ps` 失败态、rollback 输出、`/readyz` 前后状态码）：
- 佐证（截图或日志片段路径）：

## 5. 数据库恢复

### 前置条件

- 第 1 节通过，`postgres` 为 `healthy`；`backup/` 至少一份 `kjs-*.dump`（可先 `task deploy:backup` 现做一份）。
- 演练**只用临时库** `easy_qfnu_kjs_restore`，不碰生产库 `easy_qfnu_kjs`。
- 宿主有 `curl`；本地有 `easy-qfnu-kjs-server:<IMAGE_TAG>` 镜像（方式二用）。

### 步骤

1. 备份并记下文件名：

```bash
task deploy:backup
ls -lt backup/kjs-*.dump | head -1
```

2. 建临时库（用户从容器内 `POSTGRES_USER` 取；宿主 shell 里没有这个变量）：

```bash
docker compose -f deploy/compose.yaml --env-file .env \
  exec postgres sh -c 'createdb -U "$POSTGRES_USER" easy_qfnu_kjs_restore'
```

3. 恢复到临时库（`file=`、`target=` 必填，`CONFIRM=yes` 才执行）：

```bash
DUMP=$(ls -t backup/kjs-*.dump | head -1)   # 取最新一份，如 kjs-20261116T082255Z.dump
task deploy:restore file="$DUMP" target=easy_qfnu_kjs_restore CONFIRM=yes
```

4. 方式一：直接查临时库的房间数与最近发布时间：

```bash
docker compose -f deploy/compose.yaml --env-file .env exec postgres \
  sh -c 'psql -U "$POSTGRES_USER" -d easy_qfnu_kjs_restore -Atc \
  "SELECT (SELECT count(*) FROM room), (SELECT max(generated_at) FROM release)"'
```

5. 方式二（优先用方式一 psql 查询）：起一个一次性 `server`，直接探 `/readyz` 与 `/api/query`（不经过 `web`，避免临时实例混进正式流量）。起之前先在临时库清空 `cron_expr`（或确认不会触发），探完立即停。先在一个空闲终端启动：

```bash
docker compose -f deploy/compose.yaml --env-file .env run --rm \
  --publish 127.0.0.1:18082:18080 \
  -e DATABASE_URL='postgres://<POSTGRES_USER>:<POSTGRES_PASSWORD>@postgres:5432/easy_qfnu_kjs_restore?sslmode=disable' \
  server
```

把 `<POSTGRES_USER>` / `<POSTGRES_PASSWORD>` 换成 `.env` 里的真实值（口令含 `@` `:` `/` 时要 URL 编码）。再在另一个终端探：

```bash
curl -fsS http://127.0.0.1:18082/readyz
curl -s 'http://127.0.0.1:18082/api/query?date_offset=0' | head -c 300
```

探完 Ctrl-C 停掉临时 `server`。

6. 清理临时库：

```bash
docker compose -f deploy/compose.yaml --env-file .env \
  exec postgres sh -c 'dropdb -U "$POSTGRES_USER" easy_qfnu_kjs_restore'
```

### 期望结果

- `task deploy:restore` 先打印目标库与备份文件再执行，结束打印「恢复完成」；退出码 0。缺 `file=`/`target=` 由 Task 的 `requires`（`Taskfile.yml:102-103`）在宿主侧拒绝，进不到容器；只有缺 `CONFIRM=yes` 才由 `deploy/backup/restore.sh:39-43` 拒绝（脚本已保证，本次不用重复跑负向用例）。
- 临时库 `room` 行数 > 0，`release` 的最大 `generated_at` 非空。
- 一次性 `server`：`/readyz` 200；`/api/query` 200 且正文 `"code": 0`（空结果也算通过）。
- `dropdb` 后 `psql -l` 里不再有 `easy_qfnu_kjs_restore`；生产库 `easy_qfnu_kjs` 未被触碰。
- 临时 `server` 会装载恢复自生产 dump 的设置（教务账号、OCR、cron、白名单），调度器照常装配；演练窗口撞上 cron 会真的发请求。优先用方式一 psql 查询；确要用方式二时，先在临时库把 `cron_expr` 清空（或确认不会触发）再起，探完立即停。
- 生产库恢复只在确认数据损坏时做：停 `web` → 停 `server` → 备份当前坏库 → 恢复 → 起 `server` → 起 `web` → 复验；整段写进事故记录，不写进本文件。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（dump 文件名、恢复耗时、临时库查询结果、`/readyz` 与 `/api/query` 状态码）：
- 清理确认（`dropdb` 输出、库列表复查）：
- 佐证（截图或日志片段路径）：

## 6. 并发演练

> **状态：待管理端页面交付后执行（或按管理 API 手动完成）。** 页面未交付时用 `POST /api/admin/sync` 触发同步，轮次进度用 `GET /api/admin/runs` 查（写端点带 `X-CSRF-Token` 与 Cookie `eqk_admin_session`，同 §2）。

### 前置条件

- 第 1、2 节完成；能触发手动同步（页面交付前用 `POST /api/admin/sync`）；`task ps` 四个服务正常；`backup/.last_success` 是最近一次成功备份。
- 清理在 `server` 启动时先跑一次、之后每 24 小时一次（保留 90 天）。要让清理与同步、备份同窗出现，可在 `task restart service=server` 之后立刻开始本节步骤。

### 步骤

1. 终端 A 跟踪 `server` 日志：

```bash
task logs service=server
```

2. 管理端点「立即同步」（页面交付前用 `POST /api/admin/sync`），等轮次状态进入「进行中」。
3. 同步进行中，在终端 B 跑一次备份：

```bash
task deploy:backup
```

4. 回到终端 A，看同一时间窗的日志：轮次推进（阶段/楼栋/周次）与清理记录（有删除时 INFO 带 `cutoff`、`deleted`）交错出现；轮次状态不倒退（`GET /api/admin/runs` 或页面），最终到发布成功。
5. 备份结束后复验：

```bash
task deploy:health
task ps
```

6. 重复一次「`task restart service=server` 后立刻触发同步 + 备份」，把三者在同一分钟内的输出抓成证据。

### 期望结果

- 三者互不阻塞：`task deploy:backup` 在同步进行中正常跑完并更新 `backup/.last_success`；同步轮次照常推进到发布成功，不卡在「进行中」、不倒退；清理记录照常出现，不等备份或同步结束。
- `task deploy:health` 在备份完成后仍通过；`task ps` 全程没有 `Restarting`/`Exited`。
- 清理 INFO 当前只在 `deleted>0` 时记；D8b「`deleted=0` 也记一条」待拍板（见 `design.md:31/561`）。本轮 `deleted=0` 时没有该行属正常，用「轮次不倒退 + 备份成功 + `deploy:health` 通过」作为不阻塞证据，不伪造日志。
- 三件事分别在独立上下文里跑：备份在 `backup` sidecar、清理在 `server` 内独立 goroutine + 独立 ctx、采集走轮次行锁，没有共享互斥。

### 证据（待内网执行后填写）

- 日期：
- 操作人：
- 命令输出摘要（同步轮次 ID 与起止时间、`deploy:backup` 输出与耗时、清理日志行、`deploy:health` 结果）：
- 佐证（截图或日志片段路径）：

## 归档约定

- 六节按顺序执行；每节执行完立即补「证据」栏，不留到最后凭记忆回填。
- 全部填完后把文首「状态」改为「通过」并注明完成日期；连续两周运行记录（每日同步、`backup/` 恒 7 份、启动无 ERROR）作为附录追加到本文末尾。
- 本文件只记录事实与命令输出摘要，不记录口令、密钥内容或完整连接串。
