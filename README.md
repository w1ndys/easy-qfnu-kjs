# easy-qfnu-kjs

曲阜师范大学空教室查询系统（新架构）。

```text
本地 Go 采集器（教师账号）→ 每周全量快照 → 白名单分组过滤 → GitHub 私有仓库
  → Vercel 自动部署（Serverless + Vue 3）→ 公开查询
```

旧版 Docker 实现已归档：https://github.com/w1ndys/easy-qfnu-kjs-legacy

## 架构

- `cmd/collector/`：本地 Go 采集器（`collect` / `validate` / `publish` / `run`），
  通过 CAS + OCR 登录教务系统，按周采集全校教室状态，5 大节展开为 12 小节，
  按 `config/rooms.json` 白名单过滤后生成周快照，提交 `main` 触发 Vercel 部署。
- `data/`：生成的脱敏快照源数据（`manifest.json` + `terms/<term>/weeks/week-NN.json`），
  只由 Vercel 函数构建期打包，不配置静态路由，浏览器不可直接访问。
- `api/`：Vercel Serverless 函数（单函数承载四个 GET 资源路由）：
  - `GET /api/v1/manifest`
  - `GET /api/v1/context`
  - `GET /api/v1/empty-classrooms`
  - `GET /api/v1/full-day-status`
- `web/`：Vue 3 + Vite + Vant 4 移动端前端（首页 / 空教室查询 / 全天状态 / 404）。
- `config/rooms.json`：人工维护的采集白名单与查询分组。
- `schemas/`：snapshot / manifest / config 的 JSON Schema（采集与构建双重校验契约）。

## 快速开始

```bash
cp .env.example .env     # 填写 QFNU_USERNAME / QFNU_PASSWORD
pip install ddddocr      # 验证码本地识别（OCR_CMD 默认指向 scripts/ocr_ddddocr.py）
go run ./cmd/collector run --dry-run
```

本地开发前端：`cd web && npm i && npm run dev`（Vite 代理 `/api` 到生产域名或 `vercel dev`）。

提交前本地校验（不依赖 GitHub Actions，skill `easy-qfnu-kjs-local-ci`）：

```bash
go build ./... && go test ./cmd/collector/...
cd api && npm ci && npm test && npx tsc --noEmit && cd ..
cd web && npm ci && npm run build
```

详细文档：
- `docs/operations.md`：cron / systemd timer、发布验收、回滚、Git 归档、飞书告警
- `docs/upstream.md`：教务系统接口与解析契约摘要
- `docs/issue-29-rearchitecture-decisions.md` 位于旧仓库，为 Q1—Q171 设计决策全文
