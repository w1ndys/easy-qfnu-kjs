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
compose.yaml、Dockerfile.backend、Dockerfile.frontend、Taskfile.yml
```

## 文档

- [docs/prd.md](docs/prd.md)：产品基线，冲突时以它为准
- [docs/upstream-api.md](docs/upstream-api.md)：教务请求与响应结构（脱敏）
- [docs/pitfalls.md](docs/pitfalls.md)：已确认的坑点与旧结论核对
- `specs/2026-10-27-classroom-availability-rewrite/`：requirements、design、tasklist

## 常用命令

```
task            # 列出全部任务
task up         # 起 web + server + postgres
task db:up      # 只起数据库
task backend:run / task frontend:dev   # 本地开发
task gate       # 提交前门禁：后端 test/vet/lint + 前端 test/lint/build
```

## 边界

只读查询，不提供学生课表、成绩、借教室、容量筛选；不存原始页面、账号、会话、验证码、申请人、任课教师。
