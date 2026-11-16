# deploy-ops：容器编排、备份、清理与运维门禁

Feature Name: deploy-ops
Spec Directory: 2026-11-16-deploy-ops
Updated: 2026-11-16

## Description

把七份已完成的 spec 装进一套单机、单副本的容器部署：一个根 `docker-compose.yml`、`deploy/` 下的两份 Dockerfile（`Dockerfile.backend`、`Dockerfile.web`）与 nginx 配置、一份根 `.env.example`、`deploy/` 下的备份与恢复脚本，以及根 `Taskfile.yml` 里的 `deploy:*` 门禁与运维入口。

拓扑是四服务（已定）：`web`（nginx 托管前端产物，反代 `/api`、`/healthz`、`/readyz` 到 `server`）+ `server` + `postgres` + 备份 sidecar（`backup`）。

范围之外的：多副本、Kubernetes、公网 TLS 证书自动化、CI 里跑 compose 全栈 e2e、远程推送部署（`task deploy HOST=...` 那一套）。业务代码零改动（已定）：静态托管路由与 `StaticDir` 配置项已从本 spec 移除，`backend/` 下不改任何文件（见「`web` 容器与 nginx 反代」段与 CP13）。

需求条目见同目录 [requirements.md](requirements.md)；产品口径以 [docs/prd.md](../../docs/prd.md) 为唯一基线。

**本版修订（2026-11-16，两处拍板）**：①D1 从「后端静态托管」改为「独立 `web` 容器（nginx）+ 反代」，连带 D3（两份 Dockerfile）、D9（撤销 `StaticDir`，改 `BACKEND_UPSTREAM` 与 `TRUST_PROXY_HEADERS=true`）、compose 拓扑（四服务、端口落在 `web`）与原「静态托管路由」段（整段删除）；②`task deploy:check` 保持独立 target、不并入 `task gate`（D10）。改动处均标注「（已定）」，待拍板剩 6 条见文末。

## 设计决策

| 编号 | 决策 | 状态 | 取舍 |
|---|---|---|---|
| D1 | **前端由独立 `web` 容器托管并反代（已定）**：`web`（nginx 或等价）托管 `frontend/dist` 的静态文件与 SPA 兜底，并把 `/api/*`、`/healthz`、`/readyz` 反代到 `server:18080`；`server` 只提供 JSON 与探针 | 已定 | 前端可单独发版、静态缓存头与安全头在 nginx 一处收敛；与 API 同源（同一个宿主端口），管理端 Cookie/CSRF 不受跨站影响。代价是多一个容器与一份反代配置，`TRUST_PROXY_HEADERS` 必须置 `true`（否则限流与统计看到的客户端 IP 全变成 `web` 容器地址） |
| D2 | **编排文件放仓库根、名 `docker-compose.yml`**（`deploy/` 只放两份 Dockerfile、nginx 配置与脚本） | 待拍板 | 与根 `Taskfile.yml`、`.gitignore` 的 `backup/`、README 的入口一致，运维一条 `task up` 就够；偏离 ai-kit 惯例的 `deploy/compose.yaml`，且 README 里历史写的 `compose.yaml` / `Dockerfile.backend` / `Dockerfile.frontend` 要同步改成 `docker-compose.yml` / `deploy/Dockerfile.backend` / `deploy/Dockerfile.web` |
| D3 | **两份 Dockerfile（已定）**：`deploy/Dockerfile.backend`（`golang:1.26.10-alpine` 构建 → `alpine:3.22` 运行，非 root 用户）与 `deploy/Dockerfile.web`（`node:24-alpine` + pnpm 构建 → `nginx:1.29-alpine` 运行），两者构建上下文都是仓库根，`.dockerignore` 只维护一份 | 已定 | 前端产物只进 web 镜像、后端镜像只装二进制、CA 证书与 tzdata，改前端不必重建后端镜像；代价是两份文件与两条 `build:` 声明，`docker compose build` 变成两次构建 |
| D4 | **备份用 compose 的 sidecar 服务**（`postgres:17-alpine` 镜像 + 自带 busybox `crond`），每日 03:15 `Asia/Shanghai` | 已定 | 备份与业务进程生命周期解耦，`server` 崩了备份照样跑；镜像自带 `pg_dump` 17.11，与 `postgres:17` 同主版本，不用给运行镜像装客户端。代价是多一个常驻容器 |
| D5 | **备份产物落宿主 `./backup`（bind mount）** | 待拍板 | 备份要能被运维直接拷走做异地副本，放命名卷反而要多一条 `docker run -v` 才能取出；`.gitignore` 已忽略 `backup/`。这是对「数据一律命名卷」的刻意例外，数据库数据卷与密钥卷仍用命名卷 |
| D6 | `server` 与 `web` 的 `restart: on-failure:5`；`postgres`、`backup` 用 `restart: unless-stopped` | 已定 | 配置写错或迁移失败时容器停在 `Exited`，运维一眼看到，不会无限重启刷库；`web` 跟着 `server` 一起停在可见状态，不会留下一个只回 502 的空壳。代价是运行期偶发崩溃累计到第 5 次后也要人工 `task up` |
| D7 | **日志只走标准输出**，轮转交给 compose 的 `json-file` 驱动（10 MiB × 5） | 已定 | 满足「日志按大小轮转、不写无限增长文件」，不用给 alpine 装 logrotate；进程本身已经是 `slog.NewJSONHandler(os.Stdout)`，零改动。代价是日志只在本机、要长期留存得另行收集 |
| D8 | **清理沿用进程内 `stats.Cleaner`**（90 天、启动先清一次、之后每 24 小时、独立 ctx），不新增 cron 容器 | 已定 | 与「采集、备份互不阻塞」的既有实现一致（独立 goroutine、无共享互斥）；备份是另一个容器，更不可能被清理阻塞 |
| D8b | 让清理**每次**（含 `deleted=0`）记一条 INFO | 待拍板 | 现状只在 `deleted > 0` 时记，运维看不到「今天清理跑过了」；每日一条不会刷日志，但要改 `clean.go` 一行并补测试 |
| D9 | **不新增任何后端配置字段（已定）**：`config.Config` 保持 8 个字段、`.env.example` 应用段保持 8 项；`web` 需要的上游地址走编排段变量 `BACKEND_UPSTREAM`（只被 nginx 模板读取，不进应用进程），应用进程的 `TRUST_PROXY_HEADERS` 固定 `true`（反代透传 `X-Forwarded-For` / `X-Real-IP`） | 已定 | 后端零改动：不需要 `StaticDir`、不需要静态路由、不需要为它新增单测。代价是上游地址只能靠模板渲染注入，改 `BACKEND_UPSTREAM` 要重启 `web` 才生效 |
| D10 | `task deploy:check` **不并入** `task gate`（已定） | 已定 | `gate` 要能在没有 Docker 的机器与 CI 上跑完；`deploy:check` 含镜像构建，分钟级。两段门禁的分工写进运维文档：`gate` 的 7 项覆盖保持不变，改编排、镜像、nginx 配置或脚本时手动追加 `deploy:check` |
| D11 | 两个镜像都用同一个 `IMAGE_TAG`：`easy-qfnu-kjs-web:${IMAGE_TAG:-local}` 与 `easy-qfnu-kjs-server:${IMAGE_TAG:-local}` | 待拍板 | 前后端成对回退（新前端配旧后端最容易出兼容问题），镜像回退靠回填同一个 `IMAGE_TAG` 实现；与「镜像名写死」的惯例只差 tag 可变这一处。代价是前端小改也要走一次 `IMAGE_TAG` |
| D12 | 迁移仍在 `server` 进程内执行，**不引入独立迁移容器** | 已定 | 单副本 + 迁移前置于监听是既有实现（`main.go` 里 `store.Migrate` 在构造 server 之前），等于天然满足「迁移在服务启动前执行，失败不启动」；多副本时进程内迁移会竞争，那是多副本设计要解决的，不在本 spec |

## Architecture

```mermaid
flowchart TB
    user["运维 / 校内访问者"] -->|"http://BIND_ADDR:SERVER_PORT"| web
    subgraph compose["compose 项目 easy-qfnu-kjs（单机、单副本）"]
        web["web<br/>nginx + dist<br/>静态产物 SPA 兜底 反代 /api /healthz /readyz"]
        server["server<br/>alpine + 二进制<br/>API 采集 cron 统计清理"]
        pg["postgres<br/>postgres:17-alpine<br/>无宿主端口"]
        backup["backup<br/>busybox crond 每日 03:15<br/>pg_dump + 保留 7 天"]
        pgvol[("命名卷 easy-qfnu-kjs-pgdata")]
        keyvol[("命名卷 easy-qfnu-kjs-secret")]
        out[("./backup 宿主机目录")]
    end
    web -->|"proxy_pass BACKEND_UPSTREAM"| server
    server -->|"DATABASE_URL"| pg
    server --- keyvol
    backup -->|"pg_dump 独立会话"| pg
    backup --> out
    server -.->|"HTTPS 采集与 OCR"| ext["强智教务 + OCR（外部）"]

    style pgvol fill:#eef
    style keyvol fill:#fee
    style out fill:#efe
```

图渲染不出来时看这段同义结构：

```text
浏览器 ──> web:8080（nginx，宿主 BIND_ADDR:SERVER_PORT）──> server:18080（compose 内网，宿主不映射）
              │                                                  │
              └── dist 静态文件 + SPA 兜底 + 安全头                └── 命名卷 /var/lib/easy-qfnu-kjs（secret.key）
server ──> postgres:5432（compose 内网，宿主不映射）
backup sidecar ──> postgres（pg_dump 只读快照）──> 宿主 ./backup（保留 7 天）
server 出网 ──> 强智教务 / OCR（http/https，见 UPSTREAM_BASE_URL 与设置项）
```

### 启动顺序（失败即退出，不监听）

```text
docker compose up -d
  └─ postgres 起 → pg_isready 通过（healthcheck）→ healthy
       ├─ server 起（depends_on: condition: service_healthy）
       │    ├─ config.Load：非法即退出码 1
       │    ├─ store.Open：30s 就绪窗口（store.go），失败即退出码 1
       │    ├─ store.Migrate：失败即退出码 1，此时还没有监听端口
       │    ├─ 密钥装载：失败/缺失只记 WARN，降级继续（首次部署的正常状态）
       │    ├─ 收敛过期轮次：失败只记 WARN
       │    ├─ 装配 runner / scheduler / query / api
       │    ├─ 启动 scheduler 协程、统计记账协程、统计清理协程
       │    └─ ListenAndServe(HTTP_ADDR=:18080) → /healthz 返回 200 → 容器 healthy
       └─ web 起（depends_on: server: service_healthy）
            ├─ entrypoint 渲染 nginx 模板：BACKEND_UPSTREAM → proxy_pass（缺值即启动失败）
            ├─ nginx -g 'daemon off'
            └─ GET / 返回 index.html → 容器 healthy
```

`backup` 独立于上面这条链：`depends_on: postgres(service_healthy)`，与 `server`、`web` 无启动依赖。

### 每日时序

```text
03:15 Asia/Shanghai   backup sidecar → pg_dump -Fc → ./backup/kjs-<UTC>.dump
                      → 校验非空 → 原子更新 .last_success → 删除 7 天前的备份集
每 24h（进程启动先跑一次） stats.Cleaner → DELETE 早于「今天-90 天」0 点 → 删除数 >0 记 INFO
业务设置里的 cron      进程内 scheduler → 采集轮次（与上面两条无任何共享互斥）
```

## Components and Interfaces

### 目录结构

```text
docker-compose.yml              根编排：web / server / postgres / backup，命名卷、网络、健康检查、日志轮转
.dockerignore                   唯一一份：排除 .git、node_modules、frontend/dist、backend/bin、backup、specs
.env.example                    两段：应用段（≡ config.Config，8 项）+ 编排段（仅 compose 与 nginx 模板）
Taskfile.yml                    新增 deploy:*，修正 up/down/logs/build 描述、db:dump/db:restore、LOCAL_DSN 注释
README.md                       「部署」段：内网部署、首次初始化、回退与恢复，链接验收记录
deploy/
  Dockerfile.backend            server 镜像：go-build（golang:1.26.10-alpine）→ runtime（alpine:3.22，非 root）
  Dockerfile.web                web 镜像：web-build（node:24-alpine + pnpm）→ runtime（nginx:1.29-alpine）
  nginx/
    default.conf.template       站点模板：静态 + SPA 兜底 + 反代 /api /healthz /readyz（由 entrypoint 渲染）
  backup/
    crontab                     15 3 * * * /scripts/backup.sh
    entrypoint.sh               校验 crontab 与脚本存在后 exec crond -f -l 8 -c /etc/crontabs
    backup.sh                   pg_dump -Fc → 非空校验 → .last_success → 保留策略
    restore.sh                  pg_restore 到显式目标库（缺 target 即拒绝）
  scripts/
    smoke.sh                    可选冒烟：临时 project + 临时卷 + 端口 18081
    secret-backup.sh            docker compose cp secret.key → ./backup/secret/，chmod 600
docs/acceptance-2026-11-16-internal.md   内网验收记录（M5 归档产物，由验收任务产出）
```

前端与后端目录保持不动（已定）：`frontend/`、`backend/` 里都不放 Dockerfile，两份 Dockerfile 都在 `deploy/` 下；构建上下文统一是仓库根，前端源码只在 `Dockerfile.web` 的 `web-build` 阶段被读取，`backend/` 只在 `Dockerfile.backend` 的 `go-build` 阶段被读取。

### compose 服务表

| 服务 | 镜像 / 构建 | 宿主端口 | 卷 | 健康检查 | 依赖 | 重启 | 日志 |
|---|---|---|---|---|---|---|---|
| `web` | `build: {context: ., dockerfile: deploy/Dockerfile.web, target: runtime}`，`image: easy-qfnu-kjs-web:${IMAGE_TAG:-local}` | `${BIND_ADDR:-127.0.0.1}:${SERVER_PORT:-8080}:8080` | 无（静态产物在镜像里） | `wget -qO- http://127.0.0.1:8080/`（只验 nginx 能出静态产物，不依赖 `server`），15s/3 次/3s | `server: service_healthy` | `on-failure:5` | json-file 10m×5 |
| `server` | `build: {context: ., dockerfile: deploy/Dockerfile.backend, target: runtime}`，`image: easy-qfnu-kjs-server:${IMAGE_TAG:-local}` | 无（只经 compose 内网被 `web` 访问） | `secret:/var/lib/easy-qfnu-kjs` | `wget -qO- http://127.0.0.1:18080/healthz`，15s/3 次/3s，`start_period: 60s` | `postgres: service_healthy` | `on-failure:5` | json-file 10m×5 |
| `postgres` | `postgres:17-alpine` | 无 | `pgdata:/var/lib/postgresql/data` | `pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}`，5s/10 次 | — | `unless-stopped` | json-file 10m×5 |
| `backup` | `postgres:17-alpine`，`entrypoint: /scripts/entrypoint.sh` 起 crond | 无 | `./backup:/backup`、`./deploy/backup:/scripts:ro` | 无进程级探针，靠 `/backup/.last_success` 的新鲜度由 `deploy:health` 断言 | `postgres: service_healthy` | `unless-stopped` | json-file 10m×5 |

顶层声明：`name: easy-qfnu-kjs`；`volumes:` 两个卷都写死 `name:`（`easy-qfnu-kjs-pgdata`、`easy-qfnu-kjs-secret`）；服务各自 `container_name: easy-qfnu-kjs-<svc>`；自定义网络 `easy-qfnu-kjs-net`（四个服务都加入，宿主只经 `web` 的映射端口进）。

清理不在第四个服务里：它仍是 `server` 进程内的 `stats.Cleaner`（D8），第四个服务是 `backup` sidecar。宿主端口映射只出现在 `web` 一处（CP2）。

`backup` 用 `postgres:17-alpine` 而不是另做一个镜像，理由：镜像自带 `pg_dump` 17.11 与 busybox `/usr/sbin/crond`、`/usr/bin/find`（已在 `postgres:17-alpine` 上核对过），一个 sidecar 不需要自建镜像。

### compose 骨架

```yaml
name: easy-qfnu-kjs

services:
  postgres:
    image: postgres:17-alpine
    container_name: easy-qfnu-kjs-postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: ${POSTGRES_USER:-kjs}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?需要设置 POSTGRES_PASSWORD}
      POSTGRES_DB: ${POSTGRES_DB:-easy_qfnu_kjs}
      TZ: Asia/Shanghai
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
      interval: 5s
      timeout: 5s
      retries: 10
    logging: &logging
      driver: json-file
      options: { max-size: "10m", max-file: "5" }

  server:
    build: { context: ., dockerfile: deploy/Dockerfile.backend, target: runtime }
    image: easy-qfnu-kjs-server:${IMAGE_TAG:-local}
    container_name: easy-qfnu-kjs-server
    restart: on-failure:5
    depends_on:
      postgres: { condition: service_healthy }
    environment:
      DATABASE_URL: postgres://${POSTGRES_USER:-kjs}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB:-easy_qfnu_kjs}?sslmode=disable
      HTTP_ADDR: ":18080"
      LOG_LEVEL: ${LOG_LEVEL:-}
      OCR_TIMEOUT: ${OCR_TIMEOUT:-}
      TRUST_PROXY_HEADERS: ${TRUST_PROXY_HEADERS:-true}
      SECRET_KEY_PATH: /var/lib/easy-qfnu-kjs/secret.key
      ADMIN_COOKIE_SECURE: ${ADMIN_COOKIE_SECURE:-false}
      UPSTREAM_BASE_URL: ${UPSTREAM_BASE_URL:-}
      TZ: Asia/Shanghai   # 依赖 runtime 阶段的 tzdata 才生效：日志时间与宿主机一致；业务时间仍用固定 +08:00，不依赖 tzdata
    volumes:
      - secret:/var/lib/easy-qfnu-kjs
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:18080/healthz || exit 1"]
      interval: 15s
      timeout: 3s
      retries: 3
      start_period: 60s
    stop_grace_period: 20s
    logging: *logging

  web:
    build: { context: ., dockerfile: deploy/Dockerfile.web, target: runtime }
    image: easy-qfnu-kjs-web:${IMAGE_TAG:-local}
    container_name: easy-qfnu-kjs-web
    restart: on-failure:5
    depends_on:
      server: { condition: service_healthy }
    environment:
      BACKEND_UPSTREAM: ${BACKEND_UPSTREAM:-server:18080}
      TZ: Asia/Shanghai
    ports:
      - "${BIND_ADDR:-127.0.0.1}:${SERVER_PORT:-8080}:8080"
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:8080/ >/dev/null || exit 1"]
      interval: 15s
      timeout: 3s
      retries: 3
    logging: *logging

volumes:
  pgdata: { name: easy-qfnu-kjs-pgdata }
  secret: { name: easy-qfnu-kjs-secret }

networks:
  default: { name: easy-qfnu-kjs-net }
```

（`backup` 服务在 tasklist 第 5 项补齐；上面只钉住接口与语义。）

要点：

- 四个服务共用同一个 `logging` 锚点，规格只写一处，避免某个服务漏配把磁盘写满。
- `server` 的 `environment` 里**没有** `POSTGRES_*`，也**没有** `STATIC_DIR`（已定）：编排段变量不进应用进程（Requirement 3.4），静态托管已经整段交给 `web`。
- 可选应用段变量一律 `${VAR:-}` 形式：值为空串时由 `config.Load` 回落默认值，compose 不复制一份默认值，避免两处口径。唯一例外是 `TRUST_PROXY_HEADERS`：默认值由拓扑决定而不是代码默认——请求全部来自 `web`，必须信任反代透传，所以写成 `${TRUST_PROXY_HEADERS:-true}`；只有绕过 `web` 直连 `server` 排障时才在 `.env` 里显式置 `false`。
- `ADMIN_COOKIE_SECURE` 在 compose 层的默认值**低于**代码默认（代码是 `true`）：内网直连 HTTP 是本次交付的默认场景。这个默认值的适用条件写进 `.env.example` 注释与部署步骤。
- `BACKEND_UPSTREAM` 是 `web` 唯一的业务变量，格式 `host:port`（不带协议），默认 `server:18080`；它只被 nginx 模板读取，不进应用进程。
- `POSTGRES_PASSWORD` 限定为字母、数字与 `-` `_`：`DATABASE_URL` 由 compose 直接插值拼出，口令里带 `@` `:` `/` 这类字符会把连接串拆坏。需要强随机符号口令时，改为在 `.env` 里显式写一条完整的 `DATABASE_URL` 并让 compose 直接引用它。

### 两份 Dockerfile（已定）

两个镜像的构建上下文都是仓库根（`context: .`），文件在 `deploy/` 下，`docker build -f deploy/Dockerfile.<kind> .` 可单独构建。

**`deploy/Dockerfile.backend`**

| 阶段 | 基础镜像 | 职责 | 关键点 |
|---|---|---|---|
| `go-build` | `golang:1.26.10-alpine` | 拷 `backend/go.mod`、`backend/go.sum` 先 `go mod download`，再拷 `backend/` 源码，`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server` | 迁移脚本已由 `//go:embed *.sql` 打进二进制，运行阶段不需要 `migrations/` 目录 |
| `runtime` | `alpine:3.22` | 只拷二进制，装 CA 证书与 tzdata；预建 `/var/lib/easy-qfnu-kjs` 并 chown 给非 root 用户；`USER app` | `apk add --no-cache ca-certificates tzdata`。`ca-certificates`：采集走 https 需要根证书；`tzdata`：alpine 无 zoneinfo，缺它时 `TZ=Asia/Shanghai` 是空操作、日志时间戳停在 UTC，装上后 `TZ` 生效、日志时间与宿主机一致。业务时间（到期判定等）仍用固定 `+08:00` 计算，不依赖 tzdata。**不再**有 `/app/web` 与静态目录 |

**`deploy/Dockerfile.web`**

| 阶段 | 基础镜像 | 职责 | 关键点 |
|---|---|---|---|
| `web-build` | `node:24-alpine` | `corepack enable pnpm` → 只拷 `frontend/package.json`、`frontend/pnpm-lock.yaml`、`frontend/pnpm-workspace.yaml` → `pnpm install --frozen-lockfile` → 拷 `frontend/` 源码 → `pnpm build` | `pnpm-workspace.yaml` 里的 `allowBuilds` / `minimumReleaseAgeExclude` 影响安装结果，必须先于 install 存在；产物在 `/app/frontend/dist`（`vite build` 的 `outDir: dist`） |
| `runtime` | `nginx:1.29-alpine` | `COPY --from=web-build /app/frontend/dist /usr/share/nginx/html` 与 `deploy/nginx/default.conf.template` → `/etc/nginx/templates/default.conf.template`；`EXPOSE 8080` | 官方镜像的 entrypoint 会用 envsubst 把 `/etc/nginx/templates/*.template` 渲染到 `/etc/nginx/conf.d/`，`BACKEND_UPSTREAM` 就是这样注入的；不装 curl、不装 Node |

运行阶段不装 `curl`：`server` 的健康检查用 alpine 自带的 busybox `wget`；`web` 的健康检查用 nginx 镜像内含的 busybox `wget`（两者都已在对应基础镜像上核对）。`nginx` master 以 root 起（渲染模板与绑定端口需要），worker 进程默认降为 `nginx` 用户——`web` 镜像里没有其他可执行内容，这个 root master 不扩大实际攻击面。

### `web` 容器与 nginx 反代（已定）

静态文件与反代整段由 `deploy/nginx/default.conf.template` 承担，后端**零改动**（`api.Handler()` 仍只注册 `/healthz`、`/readyz`、`/api/*`，[api.go](../../backend/internal/api/api.go)）。原「静态托管路由表」已随 D1 拍板删除（已定）。

1. **模板渲染**：文件落 `/etc/nginx/templates/default.conf.template`，官方镜像 entrypoint 调 `envsubst` 输出到 `/etc/nginx/conf.d/default.conf`，只替换环境里存在的变量名（`${BACKEND_UPSTREAM}`），nginx 自身的 `$uri`、`$host`、`$remote_addr` 不会被误替换。`BACKEND_UPSTREAM` 为空时渲染出空上游，`nginx -t` 直接失败，容器停在可见的 `Exited`。
2. **静态与 SPA 兜底**：`root /usr/share/nginx/html`；`location / { try_files $uri $uri/ /index.html; }`——`/admin`、`/screen` 这类前端路由由 nginx 兜 `index.html`，不再需要后端兜底。`/assets/` 用 `^~` 前缀单独一段 `try_files $uri =404`：缺失的哈希资源要返回 404 而不是被兜成 `index.html`（否则浏览器会把 HTML 当 JS 解析）。`/api`、`/healthz`、`/readyz` 也用 `^~` 前缀，保证带点的路径（如 `/api/export.json`）不会被兜底规则截走（CP8）。
3. **反代路径**：`location ^~ /api/`、`location = /healthz`、`location = /readyz` 三条反代到 `http://$backend`（`$backend` 由 `BACKEND_UPSTREAM` 渲染），其余路径一律静态。`/readyz` 也走反代（已定）：`task deploy:health` 要从宿主侧复验就绪探针，缺这条它只能进容器里查。
4. **动态解析（关键）**：`resolver 127.0.0.11 valid=10s ipv6=off;` + 变量形式的 `proxy_pass http://$backend;`。若把上游写成字面量在启动时解析一次，`task deploy:rollback` 重建 `server` 换 IP 后 `web` 会一直回 502 直到被手工重启；用 Docker 内嵌 DNS 按请求解析可以避开这个坑。
5. **透传头**：`proxy_set_header Host $host;`、`X-Real-IP $remote_addr;`、`X-Forwarded-For $proxy_add_x_forwarded_for;`、`X-Forwarded-Proto $scheme;`。后端的取源逻辑读 `X-Forwarded-For` 的**最右一跳**（紧邻反代追加，客户端伪造无效，[ratelimit.go](../../backend/internal/api/ratelimit.go)），`$proxy_add_x_forwarded_for` 正好把真实客户端地址追加在最后，与 `TRUST_PROXY_HEADERS=true` 配对成立。
6. **缓存头**：`/assets/`（文件名含内容哈希）`Cache-Control: public, max-age=31536000, immutable`；`index.html` 与 SPA 兜底响应 `Cache-Control: no-cache`；`/admin` 前缀额外 `no-store`（管理端 HTML 不能被缓存）。后端的 `withAdminNoStore` 仍兜 API 响应，两层口径一致。
7. **安全头**：`X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`、HTML 额外 `X-Frame-Options: SAMEORIGIN`（用 `add_header ... always` 挂到静态与兜底响应上）。CSP 不在本 spec：antd 的内联样式与 `CSP style-src` 的兼容要先单独核，硬加会把页面样式打断。
8. **压缩**：开 `gzip on` + `gzip_types`（`text/css`、`application/javascript`、`application/json`、`image/svg+xml`）。纯加法、不影响正确性，静态产物体积明显下降。
9. **日志**：`access_log` / `error_log` 保持官方镜像的默认（软链到 `/dev/stdout`、`/dev/stderr`），由 compose 的 `json-file` 驱动轮转，不写容器内文件。
10. **本体健康**：容器健康检查探 `GET /`，只看 nginx 能否吐出静态产物，不使用 `/healthz`——否则 `server` 一抖动就把 `web` 判成不健康，`restart: on-failure:5` 会把一个没问题的反代反复重启。

### 环境变量表

应用段（与 `config.Config` 逐字段对齐，由单元测试保证双向一致）：

| 变量 | Config 字段 | 类型 / 进程默认 | compose 取值 | 说明 |
|---|---|---|---|---|
| `DATABASE_URL` | `DatabaseURL` | string，必填 | `postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable` | 必须以 `postgres://` 或 `postgresql://` 开头，`config.Load` 强校验 |
| `HTTP_ADDR` | `ListenAddr` | string，`:18080` | `:18080` | 容器内固定端口；`server` 不做宿主端口映射，宿主侧端口由 `web` 的 `SERVER_PORT` 决定 |
| `LOG_LEVEL` | `LogLevel` | `debug`/`info`/`warn`/`error`，`info` | `${LOG_LEVEL:-}` | 拼错即启动失败（不静默回落） |
| `OCR_TIMEOUT` | `OCRTimeout` | duration，`10s` | `${OCR_TIMEOUT:-}` | 必须为正时长 |
| `TRUST_PROXY_HEADERS` | `TrustProxyHeaders` | bool，`true` | `${TRUST_PROXY_HEADERS:-true}` | 有前置反代时必须 `true`，否则限流与统计看到的客户端 IP 全是 `web` 容器地址；只有绕过 `web` 直连 `server` 排障时才置 `false`（此时来源可伪造） |
| `SECRET_KEY_PATH` | `SecretKeyPath` | string，`/var/lib/easy-qfnu-kjs/secret.key` | 同默认 | 只给路径；文件由管理端初始化写出 |
| `ADMIN_COOKIE_SECURE` | `AdminCookieSecure` | bool，`true` | `${ADMIN_COOKIE_SECURE:-false}` | 内网 HTTP 阶段必须 `false`，否则浏览器丢弃管理会话 Cookie；TLS 之后置 `true` |
| `UPSTREAM_BASE_URL` | `UpstreamBaseURL` | string，`http://zhjw.qfnu.edu.cn` | `${UPSTREAM_BASE_URL:-}` | 只做非空透传，形状校验在 `upstream.New` |

（已定：应用段共 8 项，`STATIC_DIR` 与 `config.StaticDir` 已随 D9 撤销，`config.Config` 回到原样、无新增字段。`web` 用的 `BACKEND_UPSTREAM` 不属应用段，见下表与「`web` 容器与 nginx 反代」段。）

编排段（只被 `docker-compose.yml` 与 `deploy/nginx/default.conf.template` 读取，不进应用进程）：

| 变量 | 默认 | 用途 |
|---|---|---|
| `POSTGRES_USER` | `kjs` | 数据库用户，同时进 `DATABASE_URL` |
| `POSTGRES_PASSWORD` | 必填（`:?`，未设置与空值同等对待） | 数据库口令；`.env.example` 里留空占位，真实口令只写在 `.env`。`task deploy:check-config` 用 shell 环境变量注入占位值后再校验（shell 环境优先于 `--env-file`），因此干净检出也能过 |
| `POSTGRES_DB` | `easy_qfnu_kjs` | 数据库名 |
| `BIND_ADDR` | `127.0.0.1` | `web` 的宿主端口绑定地址；内网验收改 `0.0.0.0` |
| `SERVER_PORT` | `8080` | 宿主侧端口（映射到 `web` 的 `8080`）；`server` 只在 compose 内网监听 `18080` |
| `BACKEND_UPSTREAM` | `server:18080` | `web` 的 nginx 模板渲染出的上游地址，格式 `host:port`（不带协议）；只被 `deploy/nginx/default.conf.template` 读取 |
| `IMAGE_TAG` | `local` | `web` 与 `server` 两个镜像共用的 tag，镜像回退时回填上一个值 |
| `BACKUP_RETAIN_DAYS` | `7` | 备份保留天数，传给 `backup.sh` |

`.env.example` 分两段，分隔标题固定为两行注释（对齐测试按这两行切段）：

```text
# --- 应用配置（与 backend/internal/config 逐字段对应，共 8 项）---
DATABASE_URL=
HTTP_ADDR=
LOG_LEVEL=
OCR_TIMEOUT=
TRUST_PROXY_HEADERS=
SECRET_KEY_PATH=
ADMIN_COOKIE_SECURE=
UPSTREAM_BASE_URL=
# --- 编排变量（仅 docker-compose.yml 与 nginx 模板使用，不进入应用进程）---
POSTGRES_USER=
POSTGRES_PASSWORD=
POSTGRES_DB=
BIND_ADDR=
SERVER_PORT=
BACKEND_UPSTREAM=
IMAGE_TAG=
BACKUP_RETAIN_DAYS=
```

两段各自带逐行注释说明默认值与适用场景；口令、密钥这类真实值只写在 `.env`（被 `.gitignore` 忽略），样例文件里一律留空。`POSTGRES_PASSWORD` 留空时 compose 的 `${POSTGRES_PASSWORD:?}` 会在部署阶段直接拦下，避免空口令上线。

### 备份子链路

- `backup` 服务的环境变量只有连接与保留参数，不注入应用段变量：`PGHOST=postgres`、`PGUSER=${POSTGRES_USER}`、`PGPASSWORD=${POSTGRES_PASSWORD}`、`PGDATABASE=${POSTGRES_DB}`、`BACKUP_RETAIN_DAYS=${BACKUP_RETAIN_DAYS:-7}`、`TZ=Asia/Shanghai`。
- `deploy/backup/crontab`：`15 3 * * * /scripts/backup.sh`（容器 `TZ=Asia/Shanghai`，即每日 03:15 本地时间；备份文件名的时间戳仍是 UTC）。该文件由 entrypoint 以 `600` 装到 `/etc/crontabs/root`。
- `deploy/backup/entrypoint.sh`：`set -eu`；`install -m 600 /scripts/crontab /etc/crontabs/root`（不走 bind mount 单文件，规避宿主侧权限差异导致 crond 拒读）；确认 `/scripts/backup.sh` 存在且可执行；记一条 `backup schedule loaded`；`exec crond -f -l 8 -c /etc/crontabs`。
- `deploy/backup/backup.sh`：
  1. `set -eu`，`umask 077`，`trap 'rm -f "${TMP:-}"' EXIT`（失败时不留半截文件，`/backup` 里只会有 `kjs-*.dump` 与标记文件）。
  2. 时间戳 `TS=$(date -u +%Y%m%dT%H%M%SZ)`，文件名 `kjs-$TS.dump`，先写 `$TMP=$(mktemp /backup/.kjs-XXXXXX)`。
  3. `pg_dump -Fc -f "$TMP"`（`-Fc` 自带压缩，恢复用 `pg_restore`，支持并行）。
  4. 非空校验：文件大小 > 0，否则删临时文件、退出码 1、不更新标记、不执行保留策略。
  5. `mv "$TMP" "/backup/$FILE"` 原子落位。
  6. 原子更新 `/backup/.last_success`：写 `.last_success.tmp` 后 `mv`，内容 `UTC时间 文件名`。
  7. 保留策略：`find /backup -maxdepth 1 -name 'kjs-*.dump' -mtime +$((BACKUP_RETAIN_DAYS-1)) -delete`，并且**排除最新一份**（删除前把最新文件从候选里剔掉），保证任何情况下至少留下最近一次成功备份。
- 手动入口 `task deploy:backup` = `docker compose run --rm --entrypoint /scripts/backup.sh backup`（必须覆盖 entrypoint，否则会用默认入口再起一个 crond），与定时任务同一份实现。
- 备份期间与业务的关系：`pg_dump` 只持 `ACCESS SHARE` 锁，不阻塞 DML；`server` 的采集写入与查询照常进行。连接数是 1 条，`postgres` 默认 `max_connections=100`、`server` 连接池上限 25，余量足够。

### 恢复与密钥备份

- `deploy/backup/restore.sh`：`set -eu`；要求 `TARGET_DB` 与 `BACKUP_FILE` 两个环境变量都显式给出，`TARGET_DB` 为空直接退出码 1（防止误恢复生产库）；目标库必须已存在，不存在时报错并打印 `createdb` 命令，脚本不代建库（避免顺手连到维护库）；`pg_restore --clean --if-exists --no-owner -d "$TARGET_DB" "$BACKUP_FILE"`；恢复前打印目标库名并要求 `CONFIRM=yes`。执行方式与备份一致：`docker compose run --rm --entrypoint /scripts/restore.sh -e TARGET_DB=... -e BACKUP_FILE=... -e CONFIRM=yes backup`。
- `task deploy:restore file=backup/kjs-....dump target=easy_qfnu_kjs_restore`：目标库由运维显式给出；演练一律用临时库，不动生产库。
- `task deploy:secret-backup`：`docker compose cp server:/var/lib/easy-qfnu-kjs/secret.key ./backup/secret/secret.key`，随后 `chmod 600`；只导出文件，不导出内容到日志。密钥与数据库备份分开保管（密钥丢了，库里加密保存的教务密码与 OCR 密钥就解不开）。

### 清理调度与不阻塞保证

清理不是本 spec 新写的东西，既有实现在 `internal/stats`（`RetentionDays = 90`、`cleanupInterval = 24h`、`Run` 启动先清一次、`cutoff` 用 `Asia/Shanghai` 的当天 0 点减 90 天）。本 spec 只负责把它写进运维口径并给验收证据：

- 生命周期独立：`statsCleaner.Run(statsCtx)` 跑在自己的 goroutine，`statsCtx` 与采集用的进程信号 ctx 分开（`main.go` 里有意为之：HTTP 收尾期间记账仍要能投递）；收尾时 `stopStats()` → `statsRecorder.Wait()` → `statsCleaner.Wait()`。
- 无共享互斥：清理只调 `store.DeleteStatsBefore`，采集走 `pipeline` 的轮次行锁（`collect_run_single_running`），两把锁作用对象不同；备份在另一个容器里，连进程都不共享。
- 失败只降级：清理失败记 WARN、下个周期重试，不改查询路径。
- 验收证据：`task logs service=server` 里能看到清理 INFO（`cutoff` + `deleted`）；D8b 若采纳，`deleted=0` 也能看到当日那条。

### 健康检查与启动顺序

| 探针 | 语义 | 实现现状 | 谁用 |
|---|---|---|---|
| `GET /healthz` | 进程存活，不查库 | 恒 200（api.go） | `server` 容器健康检查（容器内 `wget` 直连 `:18080`）；库抖动不该连带重启进程 |
| `GET /readyz` | 能接流量，查库 `Ping`，2 秒上限 | 200 / 503（api.go） | `task deploy:health`（经 `web` 的宿主端口）、切公网前后的复验 |
| `GET /`（`web`） | nginx 能吐出静态产物 | 静态 200 | `web` 容器健康检查；**不**探 `/healthz`，避免 `server` 抖动把反代判死并反复重启 |

反代把 `/healthz`、`/readyz` 也暴露在同一个宿主端口上，所以宿主侧看到的是全链路状态（nginx → server），容器健康检查看到的仍是单容器状态。两者不混用。

compose 的 `depends_on: postgres: service_healthy` 只保证「库能接受连接」，**不替代**进程内迁移：迁移仍在 `store.Open` 之后、`ListenAndServe` 之前执行，失败即退出码 1。因此「迁移失败不得启动」由进程逻辑保证，不靠编排顺序拼出来；`web` 的 `depends_on: server: service_healthy` 则保证「`server` 没起来就不对外提供半个站点」。

### 日志与可观测性

- 应用侧：`slog.NewJSONHandler(os.Stdout)`，不写文件；启动日志只打非敏感项（连接串拆成 `db_host`/`db_name`，密钥只打路径），这条既有口径不变。
- 反代侧：nginx 的 `access_log` / `error_log` 走官方镜像默认的 `/dev/stdout`、`/dev/stderr` 软链，不写容器内文件；`server` 的 `request_id` 不在 nginx 生成，保持「API 层产生、日志里可见」的现状。
- 容器侧：四个服务都挂 `json-file` + `max-size: 10m` + `max-file: "5"`，单服务日志上限 50 MiB，满足「按大小轮转」。
- 排障入口：`task logs`（`docker compose logs -f`）、`task logs service=web|server|postgres|backup`、`task deploy:health`。
- 关联性：`request_id` 由 API 层既有中间件透传；采集日志带轮次/阶段/楼栋/周次字段（既有实现），部署侧不做额外改造。
- 明确不做：集中式日志收集（Loki/ELK）、指标上报（Prometheus exporter）。本 spec 只保证日志有界、可查。

### `task deploy:*` target

| target | 命令要点 | 用途 |
|---|---|---|
| `deploy:env` | `.env` 不存在时从 `.env.example` 复制，存在则提示不覆盖 | 首次部署 |
| `deploy:check-config` | `POSTGRES_PASSWORD=${POSTGRES_PASSWORD:-ci-placeholder} docker compose --env-file .env.example config -q` → `.env.example` 对齐测试 → 全部脚本 `sh -n` | 无 Docker 构建的快速校验；占位口令只用于插值校验，不落盘 |
| `deploy:check` | `deploy:check-config` + `POSTGRES_PASSWORD=${POSTGRES_PASSWORD:-ci-placeholder} docker compose --env-file .env.example build` + `docker run --rm -e BACKEND_UPSTREAM=server:18080 easy-qfnu-kjs-web:local nginx -t`（模板渲染后的配置语法） | 全量部署门禁（`build` 也会解析整份编排，占位口令同样要注入）；**独立 target，不并入 `task gate`（已定）** |
| `deploy:health` | `docker compose ps` + 经 `web` 的宿主端口探 `/healthz`、`/readyz` + 最近备份时间（读 `backup/.last_success`，超过 26 小时判失败） | 体检与验收 |
| `deploy:backup` | `docker compose run --rm --entrypoint /scripts/backup.sh backup` | 手动备份 |
| `deploy:restore` | `file=` 与 `target=` 必填，`docker compose run --rm --entrypoint /scripts/restore.sh -e TARGET_DB=... -e BACKUP_FILE=... -e CONFIRM=yes backup` | 恢复演练 |
| `deploy:secret-backup` | 调 `deploy/scripts/secret-backup.sh` | 密钥备份 |
| `deploy:rollback` | `tag=` 必填：`IMAGE_TAG={{.tag}} docker compose up -d --no-build web server`（两个镜像一起换回同一个 tag，不重建镜像） | 镜像回退（前后端成对回退） |
| `deploy:smoke` | `deploy/scripts/smoke.sh`（opt-in，临时 project/卷/端口 18081） | 冒烟 |

既有 target 的同步修正（同属交付物，不是新增功能）：

- `up` / `down` / `ps` / `build` 的描述里 `web + server + postgres` 改成 `web + server + postgres (+ backup)`；`build` 的「两个镜像」保持（现在是真两个：`web` 与 `server`）；`logs` 的 `service` 取值补 `backup`，即 `web|server|postgres|backup`。
- `db:dump` / `db:restore` 改为转调 `deploy/backup/backup.sh` 与 `restore.sh`，避免仓库里存在两套 `pg_dump` 实现。
- `db:psql` 的用户与库名改用 `${POSTGRES_USER}` / `${POSTGRES_DB}`（默认不再是 `postgres`）。
- `LOCAL_DSN` 注释与 README 的 `postgres:postgres@127.0.0.1:5432` 示例同步改成 compose 编排段的实际变量。

## Data Models

部署单元本身不引入业务实体，它的「数据」是四类可被校验的产物：

**备份集**

| 字段 | 规则 |
|---|---|
| 文件名 | `kjs-YYYYmmddTHHMMSSZ.dump`（UTC，`date -u`） |
| 格式 | `pg_dump -Fc` 自定义格式，`pg_restore` 恢复 |
| 位置 | 宿主 `./backup`，容器内 `/backup` |
| 生命周期 | 每日一份；超过保留窗口（默认 7 天）删除；恒保留最近一份 |
| 成功标记 | `./backup/.last_success`，单行 `2026-11-16T19:15:03Z kjs-20261116T191500Z.dump`，只有非空 dump 落位后才更新 |

**凭据文件**

| 字段 | 规则 |
|---|---|
| 路径 | 命名卷内 `/var/lib/easy-qfnu-kjs/secret.key` |
| 属主 | 运行用户（镜像内预建目录并 chown） |
| 导出 | `./backup/secret/secret.key`，权限 `600` |
| 约束 | 不进镜像层、不进仓库、不进日志；丢失后已加密的设置项不可解 |

**`.env.example` 两段**

| 段 | 键 | 校验方式 |
|---|---|---|
| 应用段 | 8 项，与 `config.Config` 字段一一对应（已定：不再有 `STATIC_DIR`） | Go 单元测试：用 `go/ast` 解析 `config.go` 里的 `os.Getenv("...")` 键集合，与文件应用段的 `KEY=` 行做双向差集 |
| 编排段 | 8 项（`POSTGRES_*`、`BIND_ADDR`、`SERVER_PORT`、`BACKEND_UPSTREAM`、`IMAGE_TAG`、`BACKUP_RETAIN_DAYS`） | `POSTGRES_PASSWORD=ci-placeholder docker compose --env-file .env.example config -q` 能通过；键名与 compose / nginx 模板里的 `${...}` 引用对人审可见 |

**compose 名字与卷**

| 项 | 值 |
|---|---|
| 项目名 | `easy-qfnu-kjs` |
| 容器名 | `easy-qfnu-kjs-web` / `-server` / `-postgres` / `-backup` |
| 镜像 | `easy-qfnu-kjs-web:${IMAGE_TAG:-local}`、`easy-qfnu-kjs-server:${IMAGE_TAG:-local}` |
| 卷 | `easy-qfnu-kjs-pgdata`、`easy-qfnu-kjs-secret` |
| 网络 | `easy-qfnu-kjs-net` |
| 宿主端口 | `${BIND_ADDR}:${SERVER_PORT}` → `web` 的 `8080/tcp`；`server` 的 `18080/tcp` 只在 compose 内网 |

## 部署、首次初始化与回退演练

### A. 内网部署（M5 验收阶段）

前置：单台内网主机，Docker 29 + compose v2；≥4 vCPU / 8 GB 内存；磁盘 ≥ 60 GB（数据库 + 每份 dump 约数百 MB × 7 天 + 余量）；固定内网 IP；能出网访问强智教务与 OCR 服务。

1. 拉代码到 `/opt/easy-qfnu-kjs`，`task deploy:env` 生成 `.env`，填 `POSTGRES_PASSWORD`；内网验收阶段设 `BIND_ADDR=0.0.0.0`、`SERVER_PORT=8080`、`ADMIN_COOKIE_SECURE=false`，`TRUST_PROXY_HEADERS` 保持默认 `true`（请求经 `web` 反代进来，置 `false` 会让限流与统计拿不到真实客户端 IP）。
2. `task deploy:check` 通过（compose 配置、配置对齐、脚本语法、两个镜像构建、nginx 配置语法）。
3. `task up`；`task ps` 看到 `web`、`server` 与 `postgres` 为 `healthy`，`backup` 为 `running`。
4. `task deploy:health`：经 `web` 的宿主端口探 `/healthz` 与 `/readyz` 均为 200；首次部署时备份时间可能为空，允许在一次 `task deploy:backup` 后再判定。
5. 浏览器访问 `http://<内网IP>:8080/`：公开查询页可用（由 `web` 直接给出静态产物）；`/admin` 返回 `index.html` 并进入初始化引导而非 500；`/api/admin/state` 能返回 JSON（反代生效）。
6. 连跑两周：`task logs service=server` 无 ERROR 级启动失败；`task logs service=web` 无 4xx/5xx 风暴；管理端运行状态显示每日同步成功；`ls backup/*.dump` 每天多一份、超过 7 天的被清掉。

### B. 首次初始化

1. `curl -s http://127.0.0.1:8080/api/admin/state` → `initialized=false`（未初始化时不存在可登录账号）。
2. 打开 `/admin` 设置管理员密码 → 成功后**立即导出加密密钥**：`task deploy:secret-backup`，把 `backup/secret/secret.key` 移到离线介质，不要和数据库备份放同一处。
3. 登录 → 业务设置：教务账号与密码、OCR 地址与超时、教学楼白名单、同步 cron、展示名。
4. 点「立即同步」→ 管理端看到轮次推进到发布成功 → 打开公开页能查到空教室。
5. 把「密钥位置、备份位置、`.env` 关键取值（脱敏）」写进运维记录；`docs/acceptance-2026-11-16-internal.md` 开工。

### C. 业务版本回退演练（管理端）

1. 记录当前版本 ID 与发布时间（管理端版本列表）。
2. 在「版本对比」选上一版本 → 一键回退 → 公开页首次查询即命中新版本（回退提交后会清空三张 30 秒缓存）。
3. 版本列表仍只有两个版本；查询、全天状态、大屏三处数据一致。
4. 记录回退耗时与操作人。

### D. 镜像回退演练

1. 记录当前 `IMAGE_TAG`（`.env` 里的值）与两个镜像的 digest（`docker image ls easy-qfnu-kjs-web easy-qfnu-kjs-server`）。
2. 部署新 tag：`IMAGE_TAG=<new> task up`（重建两个镜像）→ `task deploy:health` 通过。
3. 模拟失败：`IMAGE_TAG=<bad> task up`，`/readyz` 不为 200 或容器进入 `Restarting`/`Exited`。
4. 回退：`task deploy:rollback tag=<上一个成功 tag>`（`web` 与 `server` 一起回填）→ 复验 `/readyz`、首页、一次空教室查询。回退后确认 `web` 已重新解析到新 `server` 地址（无 502 残留）。
5. 注意：镜像回退只回二进制与前端静态产物，不回数据库。回退前确认旧镜像内嵌的迁移集不小于库内已执行的最大版本（本仓库迁移只增，`0001`–`0005`）；跨越新增迁移的回退要按「先备份、再回退」的顺序做。

### E. 数据库恢复演练（每季度）

1. `task deploy:backup` 产出一份今天的 dump。
2. 建临时库：`docker compose exec postgres createdb -U "$POSTGRES_USER" easy_qfnu_kjs_restore`。
3. `task deploy:restore file=backup/kjs-<ts>.dump target=easy_qfnu_kjs_restore CONFIRM=yes`。
4. 用一次性 `psql` 查询临时库的房间数与最近发布时间，确认非空；或用 `docker compose run --rm --publish 127.0.0.1:18082:18080 -e DATABASE_URL=<临时库连接串> server` 起一个临时 `server`，直接打 `http://127.0.0.1:18082/readyz` 与 `/api/query`（不经过 `web`，避免临时实例被反代混进正式流量）。
5. 删除临时库；把演练时间、dump 文件名、验证结果写进验收记录。
6. 生产库恢复只在确认数据损坏时做：停 `web` → 停 `server` → 备份当前坏库 → 恢复 → 起 `server` → 起 `web` → 复验；整段动作写进事故记录。

## Correctness Properties

- **CP1 迁移先于监听**：任一路径下，`ListenAndServe` 只在 `store.Migrate` 成功之后被调用；迁移失败时进程退出码为 1 且没有任何端口监听。
- **CP2 单入口**：`docker compose config` 输出的 `ports` 只出现在 `web` 服务；`server`、`postgres` 与 `backup` 没有任何宿主端口映射。宿主侧所有流量只会经过 `web`。
- **CP3 配置双射**：`.env.example` 应用段的键集合 = `config.go` 中 `os.Getenv` 读取的键集合（双向差集为空），且都是 8 项。
- **CP4 默认值单一来源**：compose 对可选应用段变量只传空值占位（`${VAR:-}`），不复制 `config.Load` 的默认值；改变默认值只需改一处。唯一例外是 `TRUST_PROXY_HEADERS`（默认 `true`），它的取值由「请求必经 `web` 反代」这个拓扑事实决定，不由代码默认值决定（已定）。
- **CP5 密钥不外泄**：镜像层、仓库跟踪文件、`.env.example`、任何日志行都不含 `secret.key` 内容；密钥只经命名卷，`web` 镜像与 `web` 容器都不接触密钥。
- **CP6 备份原子性**：备份集只在 `pg_dump` 成功且文件非空后以 `mv` 落位；`.last_success` 与文件落位同源（同一文件名），不会出现「标记说成功但文件半截」。
- **CP7 保留不下穿**：保留策略执行后 `./backup` 至少仍有最近一份成功备份；失败的那次执行不删任何文件。
- **CP8 反代边界**：`/api`、`/healthz`、`/readyz` 由 `web` 反代到 `server`，状态码与正文与直连 `server` 逐字一致；`/api` 下未注册的路径仍是 `server` 的 404（不是 `index.html`）；`/assets/` 下未命中的文件返回 404 而不兜 `index.html`；`/admin`、`/screen` 等其余未命中路径兜 `index.html`。带点的非 `/api` 路径不会被反代截走，带点的 `/api` 路径也不会被静态兜底截走。`/healthz` 走反代，所以 `web` 挂掉时宿主侧探针必然失败（不会出现「反代死了但健康检查说好的」）。
- **CP9 三方不互斥**：清理（进程内独立 goroutine + 独立 ctx）、采集（轮次行锁）、备份（独立容器 + `ACCESS SHARE`）之间没有共享互斥量；任一在进行时其余不被阻塞。
- **CP10 日志有界**：每个服务的容器日志上限 50 MiB（10 MiB × 5），应用进程与 nginx 都不创建容器内日志文件（都写 stdout/stderr）。
- **CP11 门禁有效性**：`task deploy:check` 的每个步骤用非零退出码传播失败；任一步失败时整体退出码非零。
- **CP12 回退可行**：`IMAGE_TAG` 可在不重建的条件下同时回填 `web` 与 `server` 并重启；回退后 `web` 因 `resolver 127.0.0.11` 重新解析 `server` 的新地址，不出现长期 502；回退动作不触碰 `easy-qfnu-kjs-pgdata` 与 `easy-qfnu-kjs-secret`。
- **CP13 后端零改动**：本 spec 交付完成后 `git diff --stat -- backend/` 为空（已定）；镜像、编排、反代、脚本、文档都在 `backend/` 之外。

## Error Handling

| 场景 | 现象 | 处理 |
|---|---|---|
| 缺 `POSTGRES_PASSWORD` | `docker compose config` 直接报变量缺失 | `task deploy:env` 后填 `.env` |
| 库未就绪 | `server` 容器等待 `depends_on`；进程内还有 30 秒就绪窗口 | 看 `task logs service=postgres`；不用手工 sleep |
| 迁移失败（SQL 错、权限不足） | 进程退出码 1、容器按 `on-failure:5` 重启 5 次后停在 `Exited` | `docker compose logs server` 看迁移错误；修正迁移或库权限后 `task up` |
| 配置拼错（`LOG_LEVEL`、`OCR_TIMEOUT`、布尔项） | `config.Load` 直接拒绝启动，日志打出取值 | 按 `.env.example` 注释改回合法值 |
| 端口被占 | `web` 启动失败、退出码 1（`server` 已占用容器内 `18080` 时同理） | 改 `SERVER_PORT` 或先释放端口 |
| 密钥文件缺失但管理员已初始化 | 启动 WARN `secret_key_missing`；管理端密钥相关能力降级 | 从 `backup/secret/` 恢复密钥到命名卷 |
| `pg_dump` 失败 / 磁盘满 | 备份保留旧集、退出码非零、`.last_success` 不变 | 清 `./backup` 多余文件或扩容；下个周期自动重试 |
| 备份超过 26 小时未成功 | `task deploy:health` 判失败 | 查 `task logs service=backup`；必要时 `task deploy:backup` 手动补 |
| `BACKEND_UPSTREAM` 为空或写错 | `web` 启动时 `nginx -t` 失败、容器停在 `Exited`；或能起但每条 API 请求 502 | 按 `.env.example` 注释填 `host:port`（默认 `server:18080`）后 `task up`；查 `task logs service=web` 的 `error.log` |
| `server` 未起或正在重启，用户访问页面 | 静态页正常出，`/api`、`/healthz`、`/readyz` 返回 502 | 正常现象且是可见的（CP8）：`task ps` 看 `server` 状态；`web` 不会把 502 变成假的 200 |
| 清理失败 | 一条 WARN，下个周期重试 | 多为库连接问题，与查询故障同源排 |
| 访问统计写入失败 | 记账失败只记日志，不阻断查询（既有实现） | 观察是否持续；持续则查连接池 |
| 容器日志暴涨 | `json-file` 达到 10 MiB × 5 后滚动 | `task logs` 拉取后不必手工清理 |

## Test Strategy

**A. 进 `task gate`（无 Docker 依赖，随既有 7 项一起跑）**

- `backend/internal/config`：新增 `.env.example` 对齐测试——`go/ast` 解析 `config.go` 里 `os.Getenv("...")` 的键集合，读仓库根 `.env.example` 的应用段，断言双向差集为空；再断言应用段键数 = `len(config.Config{})` 的字段数（由反射拿到，避免手写常量漂移；当前是 8）。这是本 spec 唯一新增的 Go 测试，静态路由测试已随 D1/D9 撤销（已定）。
- 既有 `/healthz`、`/readyz`、限流取源（`X-Forwarded-For` 最右一跳、`X-Real-IP` 兜底）用例保持通过——`TRUST_PROXY_HEADERS=true` 的正确性直接依赖它们。
- 不新增测试框架：Go 标准库测试，与既有风格一致。

**B. 进 `task deploy:check`（需要 Docker，分钟级）**

1. `POSTGRES_PASSWORD=ci-placeholder docker compose --env-file .env.example config -q`（用样例文件而不是本地 `.env`，占位口令由 shell 环境注入，保证干净检出也能过；空口令的负向用例单独一条）。
2. `cd backend && go test ./internal/config -run TestEnvExample`（同一份对齐测试的快速入口）。
3. 对 `deploy/**/*.sh` 与 `deploy/backup/entrypoint.sh` 逐个 `sh -n`。
4. `POSTGRES_PASSWORD=ci-placeholder docker compose --env-file .env.example build`（构建退出码 0 即通过；两个镜像都要出来；`build` 同样要解析整份编排，占位口令必须注入）。
5. `docker run --rm -e BACKEND_UPSTREAM=server:18080 easy-qfnu-kjs-web:local nginx -t`（容器的 entrypoint 先渲染模板，再校验配置语法；显式给一个值是因为模板里 `$backend` 为空时 `nginx -t` 必然失败——这一步实际在验证「模板 + 变量 + 配置」三件套能拼成合法 nginx 配置，而不是验证默认值）。

**C. opt-in / 手工（不进默认门禁）**

- `task deploy:smoke`：`POSTGRES_PASSWORD=ci-placeholder SERVER_PORT=18081 docker compose -p kjs-smoke --env-file .env.example up -d --build`（临时卷、临时 project 名）→ 断言 `/` 200 且含 `<div id="root">`（由 `web` 的静态产物给出）、`/assets/` 下任一静态文件 200 且带 `immutable`、`/admin` 200 且返回 `index.html`（SPA 兜底）、`/healthz` 与 `/readyz` 200（反代生效）、`/api/不存在` 返回 404 且正文为 `404 page not found`（反代不兜底）→ `down -v` 清理。用于「镜像真的能跑」而不只是能构建。
- 备份→恢复演练（每季度，按 D/E 段步骤）。
- 并发演练：手动同步进行中执行 `task deploy:backup`，观察轮次状态不倒退、`deploy:health` 仍通过（验证 CP9 的运维侧证据）。
- 迁移失败演练：`docker compose run --rm -e DATABASE_URL=postgres://bad:bad@postgres:5432/x server` → 断言退出码非 0 且日志含「启动失败」（验证 CP1）。
- 明确不做：CI 里跑全栈 compose e2e、引入 bats 或额外 shell 测试框架、TLS 终止自动化、给 nginx 配置写单元测试（`nginx -t` + 冒烟三段断言已覆盖语法与语义）。

## 待拍板疑点

已定两条（本次拍板，不再列出）：D1 前端托管方式（独立 `web` 容器 + 反代）、D10 `deploy:check` 保持独立 target 且不并入 `gate`。剩 6 条：

1. **D2 编排文件位置**：根 `docker-compose.yml` 偏离 ai-kit 惯例的 `deploy/compose.yaml`；选惯例则 `deploy/` 同时承载编排、两份 Dockerfile、nginx 配置与脚本（本 spec 现在是后者——`deploy/` 已放 Dockerfile 与 nginx 配置，再放编排也能成立）。
2. **D5 备份落宿主 `./backup`**：偏离「数据一律命名卷」。若坚持命名卷，需要额外的取出命令（`docker run --rm -v ... tar`），备份不能直接拷走。
3. **D6 重启上限**：`server` 与 `web` 都是 `on-failure:5`，坏配置会停在可见的 `Exited`；若更看重自愈，改回 `unless-stopped`，代价是坏配置会无限重启刷日志与库连接。
4. **D8b 清理日志**：是否接受为「每日一条清理 INFO（含 `deleted=0`）」改 `clean.go` 一行并补测试；不接受则验收时只能靠统计表行数间接确认清理跑过。**注意**：这一条若采纳会破坏 CP13「后端零改动」，需要一并改口。
5. **D11 镜像 tag 变量化**：两个镜像共用一个 `IMAGE_TAG` 让回退可用，但偏离「镜像名写死」的惯例；若写死 tag，回退只能重建镜像。
6. **公网阶段的 TLS 终止归属**：本 spec 只把 `web` 暴露到 `BIND_ADDR`，不交付证书与 443。切公网时由宿主反代（Caddy/nginx，compose 外）终止 TLS 并补安全头，还是届时给 `web` 加一个 TLS `server` 块——这个决定影响 `TRUST_PROXY_HEADERS`（多一层反代仍为 `true`）、`ADMIN_COOKIE_SECURE` 的最终取值与 nginx 配置的落点，需要先定方向再写进 M5 验收。

## References

[^1]: (docs/prd.md#L35) - [用户与权限（1.2，运维角色）](../../docs/prd.md)
[^2]: (docs/prd.md#L376) - [时间口径（7.1）](../../docs/prd.md)
[^3]: (docs/prd.md#L382) - [规模与性能，含统计保留 90 天与清理不阻塞（7.2）](../../docs/prd.md)
[^4]: (docs/prd.md#L393) - [可靠性（7.3）](../../docs/prd.md)
[^5]: (docs/prd.md#L401) - [安全与隐私（7.4）](../../docs/prd.md)
[^6]: (docs/prd.md#L410) - [可观测性与运维，含每日备份保留 7 天与日志轮转（7.5）](../../docs/prd.md)
[^7]: (docs/prd.md#L479) - [交付（8.6，内网验收两周与回退演练）](../../docs/prd.md)
[^8]: (docs/prd.md#L484) - [交付顺序（9，M5 验收）](../../docs/prd.md)
[^9]: (backend/internal/config/config.go#L42-L51) - [`Config` 的 8 个字段；已定：不新增 `StaticDir`，保持 8 个](../../backend/internal/config/config.go)
[^10]: (backend/internal/config/config.go#L54-L133) - [`Load` 读取的 8 个环境变量与默认值](../../backend/internal/config/config.go)
[^11]: (backend/internal/api/api.go#L133-L206) - [`Handler()` 路由表；静态托管不再挂在这里（已定）](../../backend/internal/api/api.go)
[^12]: (backend/internal/api/api.go#L209-L233) - [`/healthz` 与 `/readyz` 的既有语义](../../backend/internal/api/api.go)
[^13]: (backend/cmd/server/main.go#L93-L97) - [迁移先于监听，失败即退出码 1](../../backend/cmd/server/main.go)
[^14]: (backend/cmd/server/main.go#L500-L554) - [HTTP 服务构造、监听与收尾顺序（含统计后台的独立 ctx）](../../backend/cmd/server/main.go)
[^15]: (backend/internal/stats/stats.go#L32-L53) - [`RetentionDays = 90` 与 24 小时间隔](../../backend/internal/stats/stats.go)
[^16]: (backend/internal/stats/clean.go#L67-L128) - [清理循环、90 天边界与删除数日志](../../backend/internal/stats/clean.go)
[^17]: (backend/internal/store/store.go#L19-L27) - [连接池上限 25 与 30 秒就绪窗口](../../backend/internal/store/store.go)
[^18]: (backend/migrations/embed.go#L7-L10) - [迁移脚本用 `//go:embed *.sql` 打进二进制](../../backend/migrations/embed.go)
[^19]: (Taskfile.yml#L21-L51) - [既有编排入口 `up/down/ps/logs/build`，描述需同步](../../Taskfile.yml)
[^20]: (Taskfile.yml#L71-L82) - [既有 `db:dump` / `db:restore`，将转调 `deploy/backup` 脚本](../../Taskfile.yml)
[^21]: (Taskfile.yml#L190-L199) - [既有 `task gate` 的 7 项覆盖，保持不变](../../Taskfile.yml)
[^22]: (.gitignore#L1-L4) - [`.env` 被忽略、`.env.example` 例外](../../.gitignore)
[^23]: (.gitignore#L19-L21) - [`backup/` 与 `logs/` 已被忽略](../../.gitignore)
[^24]: (specs/2026-10-27-classroom-availability-rewrite/design.md#L18-L19) - [旧 D2：独立 `web` 容器 + 反代，与本 spec D1 一致（已定）](../../specs/2026-10-27-classroom-availability-rewrite/design.md)
[^25]: (specs/2026-10-27-classroom-availability-rewrite/design.md#L72) - [旧部署拓扑段（`web`/`server`/`postgres`），本 spec 在此之上加 `backup`](../../specs/2026-10-27-classroom-availability-rewrite/design.md)
[^26]: (specs/2026-11-15-public-web/design.md#L113) - [前端只保证 `pnpm build` 产出 dist，托管归 deploy-ops（由 `web` 容器承担）](../../specs/2026-11-15-public-web/design.md)
