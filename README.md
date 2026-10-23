# easy-qfnu-kjs

曲阜师范大学空教室查询系统（新架构）。

**当前状态（2026-10-23）：** 重写基线已定稿，见 [docs/prd.md](docs/prd.md)。现有 Go 代码只作行为参考，接下来按 PRD 从空目录重写。

## 旧实现

上一版实现——Go 采集器、PostgreSQL 发布、Vue 3 前端，以及更早的 Vercel Serverless 查询服务——一律不再作为底稿。重写的依据是文档：需求、接口事实与运维约定。

旧文件仍在 git 历史里，例如清空实现之前的 `46e5649` 父提交，以及 Go + PostgreSQL 的 `ac32966`。备份分支已删除。

线上 `kjs.easy-qfnu.top` 由旧版 kjs 系统提供服务，与本仓库的部署无关；本仓库的 Vercel 项目未上线。

旧版 Docker 实现已归档：https://github.com/w1ndys/easy-qfnu-kjs-legacy

## 重写依据

- [docs/prd.md](docs/prd.md)：重写基线（2026-10-23 定稿，取代旧需求与契约）
- [docs/pitfalls.md](docs/pitfalls.md)：重写前已确认的技术细节与坑点
- [docs/upstream-api.md](docs/upstream-api.md)：教务系统请求/响应结构（脱敏参考）
- [Issue #1](https://github.com/w1ndys/easy-qfnu-kjs/issues/1)：需求与产品边界基线
- [Issue #2](https://github.com/w1ndys/easy-qfnu-kjs/issues/2)：从上游 HTML 到周快照与库表的算法
- `docs/upstream-api.md`：教务系统请求/响应结构（当前参考）
- `docs/upstream.md`：旧接口事实（CAS 描述已过时，保留历史）
- `docs/operations.md`：运行、验收、回滚、告警、日志
- `docs/decisions/2026-10-03-frontend-react-antd.md`：前端换栈的取舍
- `docs/decisions/2026-10-09-keep-teacher-status-api.md`：仍走教师端，采集按教学楼白名单逐楼请求
- `docs/decisions/2026-10-09-saturday-full-sync.md`：周六全量同步各周矩阵，占用格明细只请求一次
- `docs/decisions/2026-10-09-term-week-from-one-sample.md`：采样一次生成学期周历，查询用日期偏移查本地表
- `docs/decisions/2026-10-09-query-by-node-range.md`：查询按小节区间，不必对齐上游大节
- `docs/decisions/2026-10-09-single-process-entry.md`：查询与采集由同一个进程启动
- `docs/decisions/2026-10-23-fully-free-is-range-aggregate.md`：完全空闲是查询侧区间聚合，不是格子状态
- `docs/contract/`：数据清洗、存储与查询契约（索引见其 `README.md`）
- `docs/product-requirements.md`：产品需求文档（完整、初始态）

`docs/upstream-api.md` 是请求/响应结构的当前参考；`docs/upstream.md` 与 `docs/operations.md` 是上一版实现时期的文档，保留原样作为重写输入；其中引用的路径（`cmd/collector/`、`schemas/`、`scripts/`）在重写产出对应实现之前并不存在，`config/rooms.json` 已随"不设白名单"删除。

## 目录

```text
docs/
  prd.md                   重写基线：产品、上游事实、数据模型、接口与验收
  pitfalls.md              重写前已确认的技术细节、坑点与旧结论核对
  upstream-api.md          教务系统请求/响应结构（脱敏参考）
  product-requirements.md  旧产品需求（已被 prd.md 取代，保留历史）
  upstream.md            教务系统接口与解析事实
  operations.md          运行、验收、回滚、告警、日志
  decisions/             决定记录，一个决定一个文件
  contract/
    README.md            契约索引与设计决定记录
    data-format.v2.md    清洗契约：上游 HTML → 清洗后的事实
    db.v2.sql            存储结构：逐格观测与发布（canonical）
    api.v2.md            查询契约：3 端点、四段式响应、错误码
  config/
    env.example          部署级环境变量（数据库连接等）；业务设置走 WebUI
README.md
.gitignore
```

`docs/prd.md` 是重写的唯一基线；`docs/contract/` 与 `specs/` 保留作历史与排障输入。业务设置（账号、飞书、OCR、cron）由 WebUI 写入数据库配置；部署级环境变量见 `docs/config/env.example`。

## 下一步

1. 按 `docs/prd.md` 从空目录重写后端：采集器 + 查询服务 + WebUI 管理面板；本机 Compose + Caddy 内网验收通过后才谈公网切换。
2. 前端 React + antd 重写与后端同批交付，按 PRD 的页面与质量门禁验收。
