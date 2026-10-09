# easy-qfnu-kjs

曲阜师范大学空教室查询系统（新架构）。

**当前状态（2026-10-03）：本仓库只保留文档。实现代码已整体移除，后端按需求文档重写中。**

## 为什么只剩文档

上一版实现——Go 采集器、PostgreSQL 发布、Vue 3 前端，以及更早的 Vercel Serverless 查询服务——一律不再作为底稿。重写的唯一依据是文档：需求、接口事实与运维约定。

完整实现仍在 git 历史里，并已推到远端作为备份分支：

- `backup/main-vercel-vue`（`6f1b2f1`）：Vercel + Vue 那套，与 `web/`、`api/`、`api-lib/`、`vercel.json` 同代
- `backup/local-compose-pg`（`ac32966`）：Go + PostgreSQL 那套，含三处远端此前没有的提交
- `backup/wip-2026-10-03`（`c0e25fb`）：当天工作区里未提交的前端与采集器改动

需要对照时从这些分支取，不要把它们当底稿。

线上 `kjs.easy-qfnu.top` 由旧版 kjs 系统提供服务，与本仓库的部署无关；本仓库的 Vercel 项目未上线。

旧版 Docker 实现已归档：https://github.com/w1ndys/easy-qfnu-kjs-legacy

## 重写依据

- [Issue #1](https://github.com/w1ndys/easy-qfnu-kjs/issues/1)：需求与产品边界基线
- [Issue #2](https://github.com/w1ndys/easy-qfnu-kjs/issues/2)：从上游 HTML 到周快照与库表的算法
- `docs/upstream.md`：教务系统接口与解析事实
- `docs/operations.md`：运行、验收、回滚、告警、日志
- `docs/decisions/2026-10-03-frontend-react-antd.md`：前端换栈的取舍
- `docs/decisions/2026-10-09-keep-teacher-status-api.md`：仍走教师端，采集按教学楼白名单逐楼请求
- `docs/decisions/2026-10-09-saturday-full-sync.md`：周六全量同步各周矩阵，占用格明细只请求一次
- `docs/decisions/2026-10-09-term-week-from-one-sample.md`：采样一次生成学期周历，查询用日期偏移查本地表
- `docs/contract/`：数据清洗、存储与查询契约（索引见其 `README.md`）
- `docs/product-requirements.md`：产品需求文档（完整、初始态）

`docs/upstream.md` 与 `docs/operations.md` 是上一版实现时期的文档，保留原样作为重写输入；其中引用的路径（`cmd/collector/`、`schemas/`、`scripts/`）在重写产出对应实现之前并不存在，`config/rooms.json` 已随"不设白名单"删除。

## 目录

```text
docs/
  product-requirements.md  产品需求文档（完整、初始态）
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

`docs/` 是重写的输入：清洗、存储与查询契约在 `docs/contract/`。业务设置（账号、飞书、OCR、cron）由 WebUI 写入数据库配置；部署级环境变量见 `docs/config/env.example`。

## 下一步

1. 按 Issue #1、#2 重写后端：采集器 + 查询服务 + WebUI 管理面板；本机 Compose + Caddy 内网验收通过后才谈公网切换。
2. 前端 React + antd 重写排在后端之后。
