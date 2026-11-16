# Tasklist：deploy-ops

Feature Name: deploy-ops
Spec Directory: 2026-11-16-deploy-ops
Updated: 2026-11-16

依据：[requirements.md](requirements.md)、[design.md](design.md)。按顺序实现；每项完成即勾选，提交前跑 `task gate`，涉及编排/镜像/nginx/脚本/配置的改动追加 `task deploy:check`（独立 target，不并入 `gate`，已定）。

**总则**

- 不新增业务功能，也不改后端代码（已定）：`backend/` 下零改动，改动全在 `deploy/compose.yaml`、`deploy/`、`.env.example`、`Taskfile.yml`、`README.md` 与文档。
- `task gate` 的 7 项覆盖不得减少；第 4 项的对齐测试必须落在后端 test 项里，随 `gate` 自动生效（这是本 spec 唯一的后端新增测试）。
- 不 push、不 tag、不 force；commit 格式 `类型(内容): 中文描述`，不加 emoji。
- design 里标「待拍板」的 D5/D6/D8b/D11 与「公网 TLS 归属」未拍板前，对应实现停在最后一步（不合并）。D1（独立 `web` 容器 + 反代）、D2（编排文件放 `deploy/compose.yaml`）与 D10（`deploy:check` 独立、不并入 `gate`）已定，按本表实现。

## 1. 镜像：`deploy/Dockerfile.backend`、`deploy/Dockerfile.web` 与 `.dockerignore`

依赖：无

- [x] `docker build -f deploy/Dockerfile.backend --target runtime -t easy-qfnu-kjs-server:local .` 退出码 0；镜像内无 `/app/web`，`/var/lib/easy-qfnu-kjs` 已建且属主为运行用户。
- [x] `docker build -f deploy/Dockerfile.web --target runtime -t easy-qfnu-kjs-web:local .` 退出码 0；镜像内 `/usr/share/nginx/html/index.html` 与 `/etc/nginx/templates/default.conf.template` 存在。
- [x] `server` 运行阶段 `id -u` 非 0；二进制为静态链接（`CGO_ENABLED=0`），运行阶段无 Go、无 Node、无 `apk` 装的多余包。`web` 运行阶段无 Go、无 Node、无源码。
- [x] `.dockerignore` 覆盖 `.git`、`frontend/node_modules`、`frontend/dist`、`backend/bin`、`backup`、`specs`、`.env*`（保留 `.env.example`），改这些路径不触发重建。
- [x] 两个镜像里都搜不到 `secret.key`、`.env`、`node_modules` 与源码目录。

## 2. web 容器与 nginx 反代：模板、静态产物与 `/api` 转发

依赖：1（web 镜像可构建）

- [ ] `deploy/nginx/default.conf.template` 落地：`root /usr/share/nginx/html`、`location / { try_files $uri $uri/ /index.html; }`、`location ^~ /assets/ { try_files $uri =404; }`、`location ^~ /api/`、`location = /healthz`、`location = /readyz` 三条反代、`resolver 127.0.0.11 valid=10s ipv6=off` 与变量形式 `proxy_pass http://$backend`。
- [ ] `docker run --rm -e BACKEND_UPSTREAM=server:18080 easy-qfnu-kjs-web:local nginx -t` 通过；`BACKEND_UPSTREAM` 留空时该命令失败（模板缺值即不合法）。
- [ ] `proxy_set_header` 四条（`Host`、`X-Real-IP`、`X-Forwarded-For`、`X-Forwarded-Proto`）齐备；缓存头按 design「`web` 容器与 nginx 反代」第 6 条、安全头按第 7 条注入；`index.html` 与 SPA 兜底 `no-cache`，`/admin` 前缀 `no-store`。
- [ ] 容器内 `GET /` 返回 `index.html`、`/admin` 返回 `index.html`、`/assets/<hash>.js` 返回 200 且带 `immutable`；`server` 未起时 `/api/*` 返回 502（不伪装成 200 或 `index.html`）。
- [ ] `git diff --stat -- backend/` 为空：本项不新增 `StaticDir`、不改 `api.Handler()`（已定）。

## 3. 编排：`deploy/compose.yaml` 与编排段变量

依赖：1、2（两个镜像与 nginx 模板就位才能接线）

- [ ] `POSTGRES_PASSWORD=ci-placeholder docker compose -f deploy/compose.yaml --env-file .env.example config -q` 退出码 0；独立检查：完全不传 env file 且 shell 未导出该变量（`env -u POSTGRES_PASSWORD docker compose -f deploy/compose.yaml config -q`）时退出码非零且错误信息点到 `POSTGRES_PASSWORD`。
- [ ] `task up` 后 `task ps` 显示 `web`/`server`/`postgres` 为 `healthy`、`backup` 为 `running`；`web` 依赖 `server` healthy 才启动；`server`、`postgres`、`backup` 均无宿主端口，`ports` 只在 `web`。
- [ ] 四个服务都挂 `json-file` 10 MiB × 5；`web` 与 `server` 为 `restart: on-failure:5`，`server` 另有 `stop_grace_period: 20s`、健康检查 15s/3 次/`start_period: 60s`；`web` 健康检查探 `GET /`；两个命名卷写死卷名。
- [ ] `server` 的 `environment` 里没有 `STATIC_DIR`、没有 `POSTGRES_*`，`TRUST_PROXY_HEADERS` 为 `${TRUST_PROXY_HEADERS:-true}`；`web` 的 `environment` 只有 `BACKEND_UPSTREAM` 与 `TZ`。
- [ ] `.env.example` 编排段 8 项齐备（含 `BACKEND_UPSTREAM`）；`Taskfile.yml` 里 `up/down/ps/build` 的描述、`logs` 的 `service` 取值、`ps` 的 `--all`（Exited 服务可见）、`db:psql` 与 `db:dump`/`db:restore` 的用户与库名、`db:dump` 的空备份防护、`LOCAL_DSN` 注释同步到新口径。
- [ ] `task up/ps/down/logs` 在 `.env` 缺失时以非零退出码结束并提示先 `task deploy:env` 或 `cp .env.example .env` 并填 `POSTGRES_PASSWORD`；README 常用命令段有对应一行。

## 4. 配置样例：`.env.example` 应用段与对齐测试

依赖：无（应用段键集合不变，8 项）

- [x] `.env.example` 应用段 8 项（不含 `STATIC_DIR`，已定），逐项注释默认值与适用场景；`ADMIN_COOKIE_SECURE` 写清 HTTP 与 TLS 两种取值，`TRUST_PROXY_HEADERS` 说明「经 `web` 反代时为 `true`，绕过反代直连排障时才 `false`」。
- [x] 分隔标题严格为 design 约定的两行注释，应用段在前、编排段在后；文件不含任何真实口令、密钥或内网地址。
- [x] `backend/internal/config` 新增对齐测试：`go/ast` 解析 `config.go` 的 `os.Getenv` 键集合，与 `.env.example` 应用段做双向差集，并断言键数等于 `config.Config` 字段数（当前 8）；`go test ./internal/config` 通过。
- [x] 除这条测试外，`backend/` 下没有其他改动（含注释）：旧「web 反代」注释保持原样即可，不再为静态托管改写（已定）。

## 5. 备份：sidecar、脚本与保留策略

依赖：3（编排存在，备份服务才能接线）

- [x] `deploy/backup/{crontab,entrypoint.sh,backup.sh}` 落地，`sh -n` 全部通过；`docker compose up -d backup` 后 `docker compose logs backup` 出现 `backup schedule loaded`。
- [x] `task deploy:backup` 在 `backup/` 产出 `kjs-<UTC>.dump`，文件非空，`.last_success` 单行内容与该文件名一致。
- [x] 人为让 `pg_dump` 失败（临时改错库名）时：退出码非零、`.last_success` 不变、既有备份一份不少。
- [x] 在 `backup/` 造 8 个跨 8 天的旧文件后跑一次成功备份，结果恰为 7 份且包含刚生成的那份（保留策略不会清空目录）。

## 6. 恢复与密钥备份

依赖：5

- [x] `deploy/backup/restore.sh` 在缺 `TARGET_DB` 或 `CONFIRM≠yes` 时拒绝执行并以非零退出码结束。
- [x] 先建临时库再 `task deploy:restore file=<dump> target=easy_qfnu_kjs_restore CONFIRM=yes`，备份恢复到临时库成功；临时库上跑一次空教室查询返回成功信封（空结果也算通过）。
- [x] `task deploy:secret-backup` 产出 `backup/secret/secret.key` 且权限为 `600`；命令输出不含文件内容。
- [x] `Taskfile.yml` 的 `db:dump` / `db:restore` 已改为转调同一份脚本，仓库里不存在第二套 `pg_dump` 实现。

## 7. 门禁与运维 target

依赖：2、3、4、5、6

- [ ] `task deploy:check` 按顺序跑「`docker compose config -q` → 配置对齐测试 → 脚本 `sh -n` → `docker compose build`（`web` 与 `server` 两个镜像）→ `docker run --rm -e BACKEND_UPSTREAM=server:18080 easy-qfnu-kjs-web:local nginx -t`」，任一步失败整体非零退出（用故意写坏的 compose 与坏脚本各验一次）。
- [ ] `task deploy:check-config` 在不构建镜像的前提下跑完前两步。
- [ ] `task deploy:check` 保持独立 target，未出现在 `task gate` 的依赖里（已定）。
- [ ] `task deploy:health` 输出「容器状态 + 经 `web` 宿主端口探测的 `/healthz`、`/readyz` + 最近备份时间」；备份超过 26 小时时非零退出。
- [ ] `task deploy:env`、`deploy:smoke`、`deploy:rollback`（`web` 与 `server` 一起回填 tag）可用；`smoke.sh` 使用独立 project 名与端口 18081、临时卷，结束时自行 `down -v` 清理，并按 design C 段断言 SPA 兜底、`/api` 404 不兜底与缓存头。

## 8. 运维文档与验收清单

依赖：7（命令名定稿后才能写文档）

- [ ] `README.md` 新增「部署」段：前置条件、`deploy:env → deploy:check → up → deploy:health` 四步、首次初始化、业务回退、镜像回退、数据库恢复，末尾链接验收记录。
- [ ] `docs/acceptance-2026-11-16-internal.md` 建好六节骨架（内网部署、首次初始化、业务版本回退、镜像回退、数据库恢复、并发演练），每节含可执行命令与期望结果。
- [ ] 文档里出现的每个 `task` 名字都在 `task --list` 中存在；不出现 `task deploy:ps` 这类未定义 target。
- [ ] 写明门禁口径：`task gate` 的 7 项不变、`task deploy:check` 独立且不并入 `gate`（已定）、改编排/镜像/nginx/脚本/配置时必须追加跑 `deploy:check`。
- [ ] 写明拓扑口径：宿主端口只落在 `web`，`server` 只在内网 `18080`，`TRUST_PROXY_HEADERS` 因反代透传而为 `true`，`/api` 与探针都经 `web` 反代。

## 9. 验收演练与收尾

依赖：8

- [ ] 按 design 的 C/D/E 段各跑一次演练并填进验收记录：业务版本回退（即刻生效）、镜像回退（`deploy:rollback` 后 `/readyz` 200）、数据库恢复（临时库查询成功）。
- [ ] 并发演练：手动同步进行中执行 `task deploy:backup`，同时观察清理 INFO 与轮次状态——三者互不阻塞，证据写进验收记录。
- [ ] 连续两周内网运行记录归档：每日同步成功、`backup/` 恒为 7 份、启动日志无 ERROR 级失败。
- [ ] 勾选 `specs/2026-11-03-easy-qfnu-kjs/roadmap.md` 的 `deploy-ops` 行并补真实 spec 目录名 `specs/2026-11-16-deploy-ops/`。
