# easy-qfnu-kjs

曲阜师范大学空教室查询：把强智教务教师端的教室占用矩阵清洗成本地逐格数据，向校内师生提供按日期、节次区间、教学楼与关键词检索空教室的只读服务，并给管理员一套初始化、配置、同步、对比与回退面板。

## 当前状态

重写进行中（M0 骨架）。本分支从零开始，旧实现与旧契约归档在 `legacy/go-collector`。

## 目录

```
backend/     Go 1.26.10 服务：API、采集 pipeline、查询、存储
frontend/    React 19 + antd 6：公开查询页（移动优先）与管理面板（桌面优先）
docs/        产品基线与上游事实
specs/       重写的需求、设计与任务拆解
deploy/compose.yaml、deploy/Dockerfile.backend、deploy/Dockerfile.web、Taskfile.yml
```

## 文档

- [docs/prd.md](docs/prd.md)：产品基线，冲突时以它为准
- [docs/upstream-api.md](docs/upstream-api.md)：教务请求与响应结构（脱敏）
- [docs/pitfalls.md](docs/pitfalls.md)：已确认的坑点与旧结论核对
- `specs/2026-10-27-classroom-availability-rewrite/`：requirements、design、tasklist

## 常用命令

```
task            # 列出全部任务
cp .env.example .env   # 首次：填 POSTGRES_PASSWORD；缺 .env 时 up/ps/down/logs 会拒绝执行
task up         # 起 web + server + postgres (+ backup)
task db:up      # 只起数据库
task backend:run / task frontend:dev   # 本地开发
task gate       # 提交前门禁：后端 test/race/vet/lint + 前端 test/lint/build
task deploy:check   # 部署门禁（改 deploy/ 或 .env.example 后追加跑；独立 target，不并入 gate）
```

## 部署

单机、单副本，四个服务：`web`（nginx 托管前端产物并反代）→ `server` → `postgres`，外加 `backup` sidecar。编排在 `deploy/compose.yaml`，`.env` 固定在仓库根；所有命令在仓库根执行（Taskfile 已带 `-f deploy/compose.yaml --env-file .env`）。

前置：内网主机装 Docker 29 + compose v2，≥4 vCPU / 8 GB 内存、磁盘 ≥60 GB，能出网访问强智教务与 OCR；`task deploy:env` 生成 `.env` 后填 `POSTGRES_PASSWORD`，内网验收另设 `BIND_ADDR=0.0.0.0`、`ADMIN_COOKIE_SECURE=false`。OCR 请求超时用 `.env` 的 `OCR_TIMEOUT`（默认 `10s`，部署期变量，改后 `task up` 重启 `server` 生效），它不是业务设置项。

**四步上线**

```bash
task deploy:env      # 生成 .env（已存在不覆盖），填 POSTGRES_PASSWORD 与 BIND_ADDR
task deploy:check    # 部署门禁：compose config → .env.example 对齐 → sh -n → 构建两镜像 → nginx -t
task up              # 起 web + server + postgres (+ backup)
task deploy:health   # 容器状态 + 经 web 的 /healthz、/readyz + 备份新鲜度
```

期望：`task ps` 里 `web`/`server`/`postgres` 为 `healthy`、`backup` 为 `running`；`deploy:health` 两条探针均 2xx（首次部署还没有备份时，先 `task deploy:backup` 再判）。

**首次初始化**

1. `curl -s http://127.0.0.1:8080/api/admin/state` → `initialized` 为 `false`（把 `127.0.0.1:8080` 换成实际 `BIND_ADDR:SERVER_PORT`）。
2. **管理端页面尚未交付**：当前 `frontend/src` 只有公开页（`/`、`/day/:roomId?`、`/screen`、`*`），`http://<内网IP>:8080/admin` 只会落到 SPA 的 404 页，不能在浏览器里设置管理员密码或改设置。页面交付前按等价管理 API 手动完成：`POST /api/admin/init`（初始化）、`POST /api/admin/session`（登录）、`GET/PUT /api/admin/settings`（业务设置）、`POST /api/admin/sync`（立即同步）、`GET /api/admin/releases`、`POST /api/admin/releases/{release_id}/rollback`（回退）。登录后的写端点（`PUT /api/admin/settings`、`POST /api/admin/sync`、`POST /api/admin/releases/{release_id}/rollback`）要带 `X-CSRF-Token`（值来自 `GET /api/admin/state` 或登录响应，见 `backend/internal/api/admin_middleware.go`）与 Cookie `eqk_admin_session`；`init` 与 `session` 本身不需要会话。
3. 初始化响应里的 `encryption_key` 是明文密钥唯一一次出口（`backend/internal/api/admin.go` 的 `adminInitResponse`）；拿到后立即 `task deploy:secret-backup`，把 `backup/secret/secret.key` 移到离线介质，与数据库备份分开保管。
4. 设置 7 个键：`jw_username`、`jw_password`、`ocr_base_url`、`feishu_webhook`、`feishu_sign_secret`、`cron_expr`、`building_whitelist`（每条 = 楼编号 `jxlbh` + 展示名 `name`）→ 手动同步 → 公开页能查到空教室。

**业务回退（管理端版本）**：管理端页面尚未交付，页面交付后按「版本列表 → 选上一版本 → 版本对比 → 一键回退」操作；页面交付前用 `GET /api/admin/releases` 取版本列表、`POST /api/admin/releases/{release_id}/rollback` 回退（写端点带 `X-CSRF-Token` 与 Cookie `eqk_admin_session`，同「首次初始化」）。回退后 `task deploy:health` 复验。期望回退提交后立即生效（三张 30 秒缓存被清空），公开页首次查询即命中回退版本，版本列表仍只有两个版本。

**镜像回退**

```bash
PREV_TAG=20261116-1                        # 上一个成功 tag（换成实际值）
task deploy:rollback tag="$PREV_TAG"       # web 与 server 成对换回同一 tag，不重建镜像
```

期望只换镜像（`--no-build`）：`/readyz` 回到 200、首页与一次空教室查询正常、无 502 残留。`.env` 里的 `IMAGE_TAG` 不会跟着改，后续 `task up` 前手动同步，否则会按旧值重建覆盖回退。

**数据库恢复**

```bash
task deploy:backup                                   # 先产出一份今天的 dump
docker compose -f deploy/compose.yaml --env-file .env exec postgres sh -c 'createdb -U "$POSTGRES_USER" easy_qfnu_kjs_restore'
DUMP=$(ls -t backup/kjs-*.dump | head -1)            # 取最新一份，如 kjs-20261116T082255Z.dump
task deploy:restore file="$DUMP" target=easy_qfnu_kjs_restore CONFIRM=yes
docker compose -f deploy/compose.yaml --env-file .env exec postgres sh -c 'dropdb -U "$POSTGRES_USER" easy_qfnu_kjs_restore'
```

期望：`file=`、`target=` 必填（缺了由 Task 的 `requires` 在宿主侧拒绝，进不到容器），缺 `CONFIRM=yes` 才由 `deploy/backup/restore.sh` 拒绝；恢复完成后临时库能查到房间数据。演练只用临时库，验完按上面的 `dropdb` 清理，绝不动 `easy_qfnu_kjs`。

拓扑：宿主端口只有 `web`（`BIND_ADDR:SERVER_PORT`）；`server` 只在内网 `18080`，`postgres`/`backup` 无宿主端口；`/api`、`/healthz`、`/readyz` 都经 `web` 反代。`TRUST_PROXY_HEADERS` 保持 `true`（否则限流与统计看到的是 `web` 的地址）；`ADMIN_COOKIE_SECURE` 内网 HTTP 用 `false`、TLS 后改 `true`。

门禁：提交前仍跑 `task gate`（7 项不变）；改编排、镜像、nginx、脚本或 `.env.example` 时追加 `task deploy:check`（独立 target，不在 `gate` 里），完整口径见验收记录的「门禁口径」小节。

验收记录（六节命令、期望与待填证据）：[docs/acceptance-2026-11-16-internal.md](docs/acceptance-2026-11-16-internal.md)。

## 边界

只读查询，不提供学生课表、成绩、借教室、容量筛选；不存原始页面、账号、会话、验证码、申请人、任课教师。
