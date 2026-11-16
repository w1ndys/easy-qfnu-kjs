# Requirements Document

Feature Name: deploy-ops
Spec Directory: 2026-11-16-deploy-ops
Updated: 2026-11-16

## Introduction

本 spec 只交付「部署与运维」：把已经完成的七份 spec（公开查询 API、采集 pipeline、管理员初始化、设置与同步、采集执行器、版本对比与回退、公开前端）装进一套单机可重复执行的容器编排，并补齐备份、清理口径、可观测性、门禁与验收步骤。

不新增业务功能。本 spec 不改后端代码（已定）：前端产物由独立 `web` 容器托管并反代 `/api`、`/healthz`、`/readyz` 到 `server`（design 的 D1、D3），后端配置与路由保持原样（design 的 D9，已定：不新增 `StaticDir`）。

需求来源：`docs/prd.md` 的 1.2（用户与权限）、7.1（时间）、7.2（规模与性能，含统计保留 90 天）、7.3（可靠性）、7.4（安全与隐私）、7.5（可观测性与运维）、8.6（交付）、9（交付顺序），以及已完成的七份 spec 的 design 里关于运维的段落（尤其 `specs/2026-10-27-classroom-availability-rewrite/design.md` 的 D1/D2 与部署拓扑段）。

**本版修订（2026-11-16，两处拍板均已定）**：①前端改由独立 `web` 容器托管并反代（不等于后端静态托管），R1、R2、R3、R6、R7、R8 的相应条目已同步改写；②`task deploy:check` 保持独立 target、不并入 `task gate`（R7.2）。改动处均标注「（已定）」。后端代码零改动，因此不再有「唯一后端改动」这类表述。

## Glossary

| 术语 | 定义 |
|---|---|
| 部署单元 | 本 spec 交付的一整套部署能力：`deploy/compose.yaml`、`deploy/` 下的两份 Dockerfile（`Dockerfile.backend`、`Dockerfile.web`）与 nginx 配置、`deploy/` 下的脚本、根 `.env.example`、根 `Taskfile.yml` 里的 `deploy:*` target |
| 编排器 | Docker Compose v2（`docker compose`），单机、单副本 |
| `web` 服务 | 部署拓扑的边缘容器：nginx 托管前端构建产物、兜 SPA 路由，并把 `/api/*`、`/healthz`、`/readyz` 反代到 `server`（已定） |
| 应用运行进程 | `server` 服务里的 `easy-qfnu-kjs` 二进制：公开 API、管理 API、进程内 cron、访问统计记账与清理都在这一个进程内；它不提供任何静态文件 |
| 应用段变量 | `.env.example` 中被 `backend/internal/config` 读取的变量，与 `config.Config` 字段一一对应 |
| 编排段变量 | `.env.example` 中只被 `deploy/compose.yaml` 或 nginx 模板使用、不进入应用进程的变量 |
| 备份集 | 一次成功 `pg_dump` 产出的单个文件，文件名含 UTC 时间戳 |
| 保留窗口 | 备份保留 7 天；访问统计保留 90 天 |
| 验收环境 | 校内网单台主机上的单副本部署，尚不启用 HTTPS（内网验收阶段） |
| 门禁 | 提交前必须通过的自动化检查集合：现有 `task gate` 的 7 项，外加本 spec 的 `task deploy:check`。两者相互独立，`deploy:check` 不并入 `gate`（已定） |

## Requirements

### Requirement 1

**User Story:** AS 运维，I want 用一份编排描述整套服务及其启动顺序，so that 换一台内网主机也能重复部署同一套环境。

#### Acceptance Criteria

1. WHEN 运维执行 `POSTGRES_PASSWORD=ci-placeholder docker compose -f deploy/compose.yaml --env-file .env.example config -q`，编排器 SHALL 通过 `deploy/compose.yaml` 的语法与必填变量校验（占位口令由 shell 环境注入，不写进 `.env.example`）。
2. IF `POSTGRES_PASSWORD` 未设置（含完全不传 `--env-file` 的情形），编排器 SHALL 以非零退出码结束并在错误信息中指出该变量名。
3. `server` 服务 SHALL 只在 `postgres` 的健康检查通过之后启动；`web` 服务 SHALL 只在 `server` 的健康检查通过之后启动（已定）。
4. `postgres` SHALL 只通过 compose 内部网络被 `server` 访问，宿主端口映射 SHALL 只存在于 `web` 服务（已定：`server` 不再映射宿主端口）。
5. `postgres` 的数据目录与加密密钥目录 SHALL 分别落在两个命名卷上，编排文件 SHALL 为两个卷固定卷名。
6. WHEN `server` 进程以非零退出码结束，编排器 SHALL 按 `restart` 策略重启，连续 5 次失败后 SHALL 停止重试并保留可查询的退出状态。
7. `web`、`server`、`postgres`、`backup` 四个服务 SHALL 各自声明健康检查或成功标记，供 `docker compose ps` 与验收脚本读取（已定）。
8. `web` 服务 SHALL 由 nginx 托管前端构建产物，并把 `/api/*`、`/healthz`、`/readyz` 反代到 `server`，同一宿主端口上 SHALL 既是页面入口也是 API 入口（已定）。

### Requirement 2

**User Story:** AS 运维，I want 从两份 Dockerfile 分别构建出可独立运行的镜像，so that 内网主机不需要 Go、Node 或 pnpm 环境。

#### Acceptance Criteria

1. 构建 `server` 镜像时，构建器阶段 SHALL 使用 `golang:1.26.10-alpine` 编译后端，运行阶段 SHALL 基于 `alpine` 且只包含二进制、CA 证书与必要运行库；`server` 镜像 SHALL 不含任何前端产物（已定）。
2. WHEN 构建 `web` 镜像，构建器阶段 SHALL 基于 `node:24-alpine` 用 `pnpm build` 从源码产出 `frontend/dist`，运行阶段 SHALL 基于 nginx 镜像且只包含该产物与 nginx 配置；仓库中被版本控制忽略的 `dist` 目录不参与构建（已定）。
3. 编译后的二进制 SHALL 在 `CGO_ENABLED=0` 下静态链接，`server` 运行阶段的进程 SHALL 以非 root 用户身份运行。
4. `server` 运行阶段 SHALL 在镜像内预建密钥目录并划归运行用户，使命名卷首次挂载后可直接写入；`web` 镜像 SHALL 不需要任何可写卷（已定）。
5. WHEN 运维在干净构建机上执行 `docker compose build`，构建 SHALL 构建出 `web` 与 `server` 两个镜像并以退出码 0 结束，且镜像内不含源码、测试文件与 `.git`。

### Requirement 3

**User Story:** AS 运维，I want 一份列全所有变量的样例文件并知道每一项对应哪段代码，so that 配置写错时能在启动前发现。

#### Acceptance Criteria

1. `.env.example` 的应用段键集合 SHALL 与 `config.Config` 的字段一一对应：每个字段恰有一个键，每个键恰对应一个字段。
2. WHEN 某个应用段变量取值为空串，应用运行进程 SHALL 回落到 `config.Load` 定义的默认值（与未设置同义）。
3. IF `.env.example` 的应用段出现 `config` 未读取的键、或缺少 `config` 读取的键，`task deploy:check` SHALL 以非零退出码结束并列出差异。
4. 编排段变量 SHALL 只出现在 `deploy/compose.yaml` 的插值或 nginx 模板里；应用运行进程的环境 SHALL 只含应用段变量与编排层派生值（`DATABASE_URL`），SHALL 不含 `STATIC_DIR` 一类静态托管配置项（已定）。
5. `.env` SHALL 被版本控制忽略，`.env.example` SHALL 不含任何真实口令、密钥或内网地址。
6. WHEN 运维执行 `task deploy:env`，任务 SHALL 在 `.env` 不存在时从 `.env.example` 复制一份，在 `.env` 已存在时保留原文件并提示未覆盖。

### Requirement 4

**User Story:** AS 运维，I want 每天自动全量备份数据库并只保留 7 天，so that 数据丢失时可回到最近一周内任一天。

#### Acceptance Criteria

1. WHEN 每日 03:15（`Asia/Shanghai`）到达，`backup` 服务 SHALL 对 `postgres` 执行一次全量 `pg_dump`，把单个文件写入宿主 `./backup`。
2. 备份集 SHALL 以 UTC 时间戳命名（`kjs-YYYYmmddTHHMMSSZ.dump`），一次执行 SHALL 只产出一个文件。
3. WHEN 一次备份成功，任务 SHALL 写入成功标记文件（含 UTC 时间与文件名），随后删除保留窗口（7 天）之前的备份集。
4. IF `pg_dump` 失败或产出文件为空，任务 SHALL 保留既有备份集、以非零退出码结束本次执行、不更新成功标记，并在下个周期重试。
5. 运维 SHALL 能用与定时任务同一份脚本手动触发一次备份（`task deploy:backup`），手动触发 SHALL 同样受保留策略约束。
6. 备份集 SHALL 可通过 `task deploy:restore file=<路径>` 恢复到一个显式指定的数据库，恢复操作 SHALL 在目标库未显式声明时拒绝执行。

### Requirement 5

**User Story:** AS 运维，I want 过期统计按日自动清理且不打扰采集与备份，so that 数据量随时间增长而查询与采集不被拖慢。

#### Acceptance Criteria

1. WHEN 应用运行进程启动，访问统计清理 SHALL 立即执行一次，此后每 24 小时执行一次，删除早于「当天（`Asia/Shanghai`）0 点减 90 天」的记录。
2. WHILE 一轮采集正在执行，清理任务 SHALL 按自己的周期继续运行，两者之间不存在共享互斥。
3. WHILE `backup` 服务正在执行 `pg_dump`，公开查询与清理 SHALL 保持可用；备份 SHALL 使用独立的数据库会话读快照。
4. IF 单次清理失败，应用运行进程 SHALL 记录一条 WARN 并在下个周期重试，对外查询不受影响。
5. WHEN 单次清理删除的记录数大于 0，应用运行进程 SHALL 记录一条携带 `cutoff` 与 `deleted` 的 INFO 日志。

### Requirement 6

**User Story:** AS 运维，I want 从日志与探针判断服务是否正常，so that 排障不用登进容器翻文件。

#### Acceptance Criteria

1. 应用运行进程 SHALL 把结构化 JSON 日志写到标准输出，容器日志 SHALL 按大小轮转（单文件 10 MiB、最多 5 个文件）。
2. `GET /healthz` SHALL 恒返回 200 表示进程存活，且不访问数据库。
3. `GET /readyz` SHALL 在数据库可达时返回 200、不可达时返回 503，单次探活时长不超过 2 秒。
4. WHEN 应用运行进程启动，数据库结构变更 SHALL 在开始监听之前完成；IF 结构变更失败，进程 SHALL 以退出码 1 结束并且不开始监听。
5. `server` 容器健康检查 SHALL 以 15 秒间隔探测 `/healthz`，启动宽限期 60 秒，连续 3 次失败判为不健康；`web` 容器健康检查 SHALL 探测其静态入口（`GET /`）而不探测 `/healthz`，以免 `server` 抖动连带重启反代（已定）。
6. `task deploy:health` SHALL 依次输出 `docker compose ps`、经 `web` 的宿主端口探测的 `/healthz` 与 `/readyz`、以及最近一次备份成功时间，任一探针失败 SHALL 以非零退出码结束（已定）。

### Requirement 7

**User Story:** AS 开发者，I want 部署改动也有可执行的门禁与验收清单，so that 编排、脚本与配置不会只在手工步骤里被验证。

#### Acceptance Criteria

1. `task deploy:check` SHALL 依次校验编排文件合法、`.env.example` 与 `config.Config` 对齐、`deploy/` 下脚本语法、`web` 与 `server` 镜像可构建、`web` 镜像内渲染后的 nginx 配置语法通过；任一步失败 SHALL 以非零退出码结束（已定）。
2. 现有 `task gate` 的 7 项覆盖（后端 test、test-race、vet、lint，前端 test、lint、build）SHALL 保持不变；`task deploy:check` SHALL 保持独立 target，SHALL NOT 并入 `task gate`（已定）。
3. `.env.example` 与 `config.Config` 的对齐校验 SHALL 作为后端单元测试运行，从而由 `task gate` 的后端 test 项覆盖。
4. `deploy/` 下的每个 shell 脚本 SHALL 通过 `sh -n` 语法检查，该检查 SHALL 属于 `task deploy:check`。
5. 验收清单 SHALL 覆盖内网部署、首次初始化、业务版本回退、镜像回退、数据库恢复演练与密钥备份，每条 SHALL 给出可执行命令与期望结果。
6. WHEN 从备份集恢复到一个临时库，恢复后的库 SHALL 支撑一次空教室查询并返回成功信封（结果为空也计入通过）。

### Requirement 8

**User Story:** AS 安全负责人，I want 部署默认值不把数据库与密钥暴露出去，so that 内网部署不会顺手扩大攻击面。

#### Acceptance Criteria

1. `postgres` SHALL 不映射任何宿主端口；`web` 的宿主端口 SHALL 默认绑定 `127.0.0.1`，`server` SHALL 不映射宿主端口（已定）。
2. 部署在 HTTP 下时 `ADMIN_COOKIE_SECURE` SHALL 取 `false`，在 TLS 终止之后 SHALL 取 `true`，`.env.example` SHALL 给出两个取值的适用场景。
3. `web` 的静态与 SPA 兜底响应 SHALL 携带 `X-Content-Type-Options: nosniff` 与 `Referrer-Policy: no-referrer`，HTML 响应 SHALL 携带 `X-Frame-Options: SAMEORIGIN`（已定：由 nginx 注入，不再由后端注入）。
4. `/assets/*`（文件名含内容哈希）SHALL 携带一年期 `immutable` 缓存头，`index.html` SHALL 携带 `no-cache`，`/admin` 前缀的 HTML SHALL 携带 `no-store`（已定：由 nginx 注入）。
5. 加密密钥文件 SHALL 只存在于命名卷 `/var/lib/easy-qfnu-kjs` 内，镜像、仓库与 `.env.example` SHALL 不含密钥内容或真实口令。
6. WHEN 运维执行 `task deploy:secret-backup`，任务 SHALL 把密钥文件导出到 `./backup/secret/` 并限制文件权限为 `600`。
7. `web` 的反代 SHALL NOT 把 `/api` 下的未注册路径兜成 `index.html`：这些请求 SHALL 仍返回 `server` 的 404（正文 `404 page not found`，与直连 `server` 逐字一致）（已定）。
