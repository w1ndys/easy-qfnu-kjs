---
date: 2026-10-03
updated: 2026-10-03
tags: [frontend, react, antd, decision]
status: accepted
---

# 前端放弃 Vant 的移动优先，改为 React + antd 6

## Background

Issue #1 的「前端行为」段原先把实现栈写死为 Vue 3 + Vite + Vant 4，并要求移动优先。这个栈是从旧实现继承下来的，不是重写基线里论证过的结论。

计划变更把前端实现栈改为 React 19 + antd 6。antd 是桌面向组件库，与「移动优先」直接冲突，所以这次反转不只是换个库，而是改了产品对移动端的定位。半年后一定会有人重新问「当初不是说要移动优先吗」，因此单独记一次取舍。

写入本文件时重写尚未开始：`frontend/` 仍是 Vue 实现，查询服务也还没有。本决定只影响计划与文档，不含代码迁移。同日计划再次变更——实现代码整体移除、仓库只保留文档，`frontend/` 目录因此已不存在，见 `README.md`。
同日稍后又重定了数据与接口契约（v2，canonical 是数据库），见 `docs/contract/README.md`。

## Options

### 选项一：antd 单库 + 响应式（已选）

桌面优先，用 Row/Col 与断点在 375px 下做成单列可用布局，移动端体验登记为「可用而非优先」。

### 选项二：antd + antd-mobile 双库

按视口切换两套组件库，移动端保住「优先」。代价是两套设计系统并联，样式与交互维护量翻倍。

### 选项三：antd-mobile 为移动主库，antd 只做桌面兜底

承认主场景在手机，等于实质放弃「用 antd」这个前提。

## Decision

采用选项一。前端实现栈定为 React 19 + Vite + TypeScript + antd 6，桌面优先、响应式，移动端为「可用而非优先」。品牌色 `#884F22` 作为 antd theme token 落地。

## Reasoning

- 第一版只有四个页面（首页、空教室、全天状态、404），两套组件库带来的第二套设计系统与双份样式维护换不来收益。
- antd 的 Grid 断点在 375px 下做单列布局已经够用，不需要靠 antd-mobile 才能用。
- 版本与目录分层对齐同型先例 `qfnu-course-grabber-v2`（同样是 Vue 迁 React + antd）：antd `^6.6.5`、react `^19.3.0`、react-router `^8.4.0`、vite `^8.3.0`、typescript `~6.0.2`。除 TypeScript 外都等于当时的 latest；TypeScript 停在 6.x 是为了避开工具链尚未跟齐的 7.x。
- 请求层沿用 axios，不引入 TanStack Query。查询契约已按 v2 重定，见 `docs/contract/api.v2.md`；旧客户端错误码只留在 git 历史。
- 前端目录沿用上一版已按关注点拆好的命名（`api/`、`pages/`、`components/`、`hooks/`、`manifest/`、`rooms/`、`constants/`），不照搬先例的扁平结构；旧契约（v1）已删除，仅存于 git 历史。
- 全天状态用 antd Table，按 12 小节出列，不做 5 大节折叠。
- 响应字段随 v2 契约重定：状态是语义键（`free` / `class` / …），房间带 `release_id` 与 `dict_version`；原先"`manifest.weeks` 兼容数组"的口子随 v1 一起作废，见 `docs/contract/api.v2.md`。

## Trade-offs Accepted

- 移动端从「优先」降为「可用」：手机上不再专门做移动优先优化，这是选项一的主要代价。日后若手机端成为主场景，应重新评估选项二。
- 不引入状态管理与请求缓存库：代价是以后要加自动刷新或多查询并发时需要另做决定。
- TypeScript 停在 6.x 而非 7.x：换取工具链稳定，代价是晚一步用上新版本。
- 选项二与选项三被否，理由保留在 Options 与 Reasoning 中，以备重提。

## Follow-up

- [ ] 前端按 React + antd 重写，重建 `frontend/` 目录；响应契约以 `docs/contract/api.v2.md` 为准
- [ ] 门禁扩为 `npm run lint` + `npm run typecheck` + `npm run build` 三项必过，纯函数补单元测试
- [ ] 前端按 v2 契约解析状态（语义键），不保留整数 ID 的映射层
- [ ] 重写完成后人工复核移动端体验，决定是否引入第二套组件库
- [ ] 保持 Issue #1 的「前端行为」段与本文件一致
