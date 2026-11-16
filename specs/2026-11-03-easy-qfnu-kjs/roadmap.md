# Roadmap：easy-qfnu-kjs

> 本目录只做功能索引，不放 requirements/design，避免第二份事实来源。
> 每个功能一个独立 spec，一次只设计并实现一个；完成后勾选并补上真实 spec 目录名。

## 已完成

- [x] public-api — 公开查询 API：context / meta / query（availability + day）/ screen / 访问统计 / 契约测试 — `specs/2026-10-27-classroom-availability-rewrite/`
- [x] collect-pipeline — 跑通一轮采集到发布：parser / clean / pipeline / publish + 检查点 8 — `specs/2026-11-03-collect-pipeline/`
- [x] admin-bootstrap — 管理员初始化、登录、会话与 CSRF：2.2 + 10.1 + 10.2 + 14.1–14.3 — `specs/2026-11-05-admin-bootstrap/`
- [x] admin-settings-sync — 设置、手动同步与运行状态：2.3 + 10.3 + 10.4 + 14.4 + 14.5 — `specs/2026-11-08-admin-settings-sync/`
- [x] collect-runner — 采集执行器：window / Plan / Run / publish 接线与未决项收口 — `specs/2026-11-09-collect-runner/`
- [x] admin-release — 版本对比/回退与异常数据视图：10.5 + 10.6 + 14.6 + 14.7 — `specs/2026-11-14-admin-release/`
- [x] public-web — 公开查询页与数据大屏：12 + 13 — `specs/2026-11-15-public-web/`

## 待做（按交付顺序）

- [ ] deploy-ops — 容器编排、备份、清理调度、全量门禁：1.2 + 15 + 16 — depends: 全部
