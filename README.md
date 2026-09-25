# easy-qfnu-kjs

曲阜师范大学空教室查询系统（新架构）。

```text
本地 Go 采集器（教师账号）→ 每周全量快照 → 白名单分组过滤 → PostgreSQL 17
  → 查询服务读取当前 release（公网域名尚未切换）
```

旧版 Docker 实现已归档：https://github.com/w1ndys/easy-qfnu-kjs-legacy

## 架构

- `cmd/collector/`：本地 Go 采集器（`collect` / `validate` / `publish` / `run`），
  通过 CAS + OCR 登录教务系统，按周采集全校教室状态，5 大节展开为 12 小节，
  按 `config/rooms.json` 白名单过滤后写入 PostgreSQL，不再推送到 GitHub。
- PostgreSQL 17：当前 release 存在 `kjs.release`，教室小节存在 `kjs.room_slot`。表结构见 `internal/collector/pg_schema.sql`。
- `data/`：数据库还没有当前 release 时，采集器用它做基线。
- `frontend/`：Vue 3 + Vite + Vant 4 移动端前端（首页 / 空教室查询 / 全天状态 / 404）。
- `config/rooms.json`：人工维护的采集白名单与查询分组。
- `schemas/`：snapshot / manifest / config 的 JSON Schema（采集与构建双重校验契约）。

## 快速开始

```bash
cp .env.example .env     # 填写 QFNU_USERNAME / QFNU_PASSWORD；OCR_CMD 已指向 uv 全局环境
uv venv ~/.local/share/easy-qfnu-ocr-venv --python 3.12
uv pip install --python ~/.local/share/easy-qfnu-ocr-venv/bin/python ddddocr
go run ./cmd/collector run --dry-run
```

本地开发前端：`cd frontend && npm i && npm run dev`（Vite 代理 `/api` 到生产域名或 `vercel dev`）。

提交前本地校验（不依赖 GitHub Actions，skill `easy-qfnu-kjs-local-ci`）：

```bash
go build ./... && go test ./cmd/collector/...
cd frontend && npm ci && npm run build
```

详细文档：
- `docs/operations.md`：cron / systemd timer、发布验收、回滚、Git 归档、飞书告警
- `docs/upstream.md`：教务系统接口与解析契约摘要
- `docs/issue-29-rearchitecture-decisions.md` 位于旧仓库，为 Q1—Q171 设计决策全文
