# Requirements Document：admin-release

## Introduction

本 spec 交付 `easy-qfnu-kjs` 管理端的**版本对比与回退**后端：版本列表与详情、两个版本之间按大节格的差异汇总与分页明细、同学期一键回退、以及异常数据视图（归属冲突版本、未知/复合占比、周覆盖缺口）。

本 spec **只做后端 HTTP API 与仓储/SQL**，不创建前端页面；不改 `release` / `observation` / `release_room_building` / `collect_run` 的表结构；不新增迁移。管理会话、CSRF、`Cache-Control: no-store`、`{code, message, data, request_id}` 响应与 `adminRead` / `adminWriteNoBody` 链路全部复用 `admin-bootstrap` 与 `admin-settings-sync` 已交付的实现。

口径来源：`specs/2026-10-27-classroom-availability-rewrite/requirements.md` 的 Requirement 24（版本对比与回退）与 25（异常数据视图）——交接文档把这两条记作 10.5 / 10.6 / 14.6 / 14.7；`docs/prd.md` 5.4、5.5、8.3、8.5；`collect-pipeline` 的发布事务与 `admin-settings-sync` 的管理 API 契约。本文件取代上述旧条目中与本文件冲突的措辞（见 design「与既有文档的差异」）。

## Glossary

- **版本**：`release` 表的一行，代表一次成功发布产生的整体数据集，含 `release_id`、`term`、`generated_at`、`dict_version`、`axis_version`、`has_attr_conflict`、`is_current`。
- **当前版本 / 非当前版本**：`is_current = true` 的至多一个版本，公开查询与数据大屏只读这一版；其余版本为非当前版本。
- **版本生成时间 `generated_at`**：版本数据的时间标识，也是库中唯一的版本排序依据（`release` 表没有 `released_at` 列）。
- **发布时刻 `released_at`**：`collect_run.released_at`，发布事务提交的 UTC 时刻；仅当该版本的发布轮次仍能通过 `collect_run.release_id` 关联到时可得。
- **观测**：`observation` 表的一行，对应「房号 × 周 × 星期 × 小节」的一个状态；观测行数是小节粒度的行数。
- **大节**：`axis_node.block`（本字典为 `0102`、`030405`、`0607`、`0809`、`101112` 五段）；一版的大节集合由该版的 `axis_version` 决定。
- **格**：一个「房号 × 周 × 星期 × 大节」单元，由该大节下的小节观测折叠而成；对比与异常计数都以格为单位。
- **对比基准 / 对比目标**：请求参数 `from`（旧版本）与 `to`（新版本）。
- **新增 / 消失 / 状态变化**：只在 `to` 出现的格为新增；只在 `from` 出现的格为消失；两侧都出现且 `state_key` 不同的格为状态变化。
- **归属变化**：同一个房号在 `from` 与 `to` 的归属快照里属于不同教学楼的格（按房号计一次）。
- **异常版本**：`has_attr_conflict = true` 的版本。
- **周覆盖缺口**：该版观测周次的最小值与最大值之间、没有观测的周。
- **`inconsistent_blocks`**：某一版内同一格的小节观测出现多个 `state_key` 的格数（发布不变量被破坏的证据，正常为 0）。
- **回退**：把某个非当前版本置为当前版本，并把原当前版本置为非当前版本；不重建、不删除任何版本数据。

## 范围与非目标

范围：`GET /api/admin/releases`、`GET /api/admin/releases/{release_id}`、`GET /api/admin/releases/diff`、`POST /api/admin/releases/{release_id}/rollback`、`GET /api/admin/anomalies` 五个端点；版本读取与大节折叠、差异汇总与分页、回退事务、异常聚合、公开查询缓存失效钩子、装配与测试。

非目标：任何前端页面与静态资源；发布事务与 `publish.Commit`（`collect-pipeline` 已交付）；告警发送；访问统计；未归属教室清单（见 design「与既有文档的差异」）；版本删除与保留策略变更；回退的独立审计表；多副本之间的缓存一致性。

## Requirements

### Requirement 1 版本列表

**User Story:** AS 管理员，I want 一眼看到当前版本与上一版本的关键事实，so that 判断哪一版可以回退。

#### Acceptance Criteria

1. WHEN 携带有效管理会话的请求到达 `GET /api/admin/releases`，系统 SHALL 返回 `releases` 数组与 `total`，并按 `generated_at` 倒序、同一生成时刻按 `release_id` 倒序排列。
2. WHEN 返回一个版本条目，系统 SHALL 包含 `release_id`、`term`、`generated_at`、`released_at`、`is_current`、`has_attr_conflict`、`week_count`（有观测的周数）、`observation_rows`（观测行数）、`rooms`（归属快照房号数）、`dict_version`、`axis_version`。
3. WHEN 该版本没有可关联的发布轮次，系统 SHALL 把 `released_at` 返回为 `null`。
4. WHEN 库中没有版本，系统 SHALL 返回空数组与 `total` 为 0。
5. WHEN 请求未携带有效管理会话，系统 SHALL 返回业务码 `40101`。

### Requirement 2 版本详情

**User Story:** AS 管理员，I want 打开一版看它的覆盖面，so that 判断这版数据是否完整。

#### Acceptance Criteria

1. WHEN 请求到达 `GET /api/admin/releases/{release_id}` 且该版本存在，系统 SHALL 返回 `release`（Requirement 1.2 的全部键）、`stats`、`week_counts` 与 `building_counts`。
2. WHEN 返回 `stats`，系统 SHALL 包含 `observation_rows`、`block_cells`（该版格数）、`week_count`、`rooms`、`buildings`。
3. WHEN 返回 `week_counts`，系统 SHALL 按周次升序给出每项 `week`、`observation_rows`、`block_cells`；WHEN 返回 `building_counts`，系统 SHALL 按 `building_id` 升序给出每项 `building_id`、`building_name`、`rooms`。
4. WHEN 指定 `release_id` 不存在，系统 SHALL 返回 HTTP 404 与业务码 `40401`。
5. WHEN `release_id` 不符合 `YYYYMMDDTHHMMSSZ` 形状，系统 SHALL 返回 HTTP 400 与业务码 `40001`，并在参数校验阶段结束该请求。

### Requirement 3 版本对比汇总

**User Story:** AS 管理员，I want 两版差异先给出分类总数，so that 判断要不要细看。

#### Acceptance Criteria

1. WHEN 请求到达 `GET /api/admin/releases/diff` 且 `from` 与 `to` 都存在、形状合法且取值不同，系统 SHALL 返回两侧版本元信息与 `summary`。
2. WHEN 返回 `summary`，系统 SHALL 包含 `cells`（`added`、`removed`、`changed`、`total`）、`rooms`（`added`、`removed`、`shared`、`building_changed`）、`unknown` 与 `composite` 的两侧格数与 `delta`、`inconsistent_blocks`、`same_term`。
3. WHEN 计算 `cells`，系统 SHALL 以「房号 × 周 × 星期 × 大节」为键，并按各版本自己的 `axis_version` 把小节观测折叠为大节；WHEN 某格只出现在 `to`，系统 SHALL 计入 `added`；WHEN 某格只出现在 `from`，系统 SHALL 计入 `removed`；WHEN 两侧 `state_key` 不同，系统 SHALL 计入 `changed`。
4. WHEN 两侧 `term` 不同，系统 SHALL 返回 `same_term = false` 并照常返回差异。
5. WHEN `from` 或 `to` 缺失、形状非法或两者相同，系统 SHALL 返回 HTTP 400 与业务码 `40001`，并在参数校验阶段结束该请求。

### Requirement 4 对比明细与分页

**User Story:** AS 管理员，I want 按类别翻看每个变化的位置与前后值，so that 定位错数据落在哪。

#### Acceptance Criteria

1. WHEN `GET /api/admin/releases/diff` 携带 `section`（缺省 `cells`，可选 `rooms`、`buildings`）、`limit`（1–200，缺省 50）与 `offset`（不小于 0），系统 SHALL 只返回所选小节的明细分页。
2. WHEN `section = cells`，系统 SHALL 每项返回 `kind`、`room_id`、`room_name`、`week`、`weekday`、`block`、`from_state`、`to_state`、`from_building`、`to_building`；WHEN 请求给出 `kind` 为 `added`、`removed` 或 `changed`，系统 SHALL 只返回该类。
3. WHEN `section = rooms`，系统 SHALL 每项返回 `kind`（`added` 或 `removed`）、`room_id`、`room_name`、`from_building`、`from_building_name`、`to_building`、`to_building_name`；WHEN `section = buildings`，系统 SHALL 每项返回 `room_id`、`room_name`、`from_building`、`from_building_name`、`to_building`、`to_building_name`。
4. WHEN 返回明细分页，系统 SHALL 按固定键升序排列（`cells` 为 `room_id`、`week`、`weekday`、`block`；`rooms` 与 `buildings` 为 `room_id`），并返回 `total`、`limit`、`offset`、`section` 与生效的 `kind`。
5. WHEN `limit` 越界、`offset` 为负、`section` 取值非法，或 `kind` 与 `section` 不匹配，系统 SHALL 返回 HTTP 400 与业务码 `40001`。

### Requirement 5 一键回退

**User Story:** AS 管理员，I want 把上一版换回来，so that 错数据能马上停用。

#### Acceptance Criteria

1. WHEN 携带有效会话与正确 CSRF 令牌的请求到达 `POST /api/admin/releases/{release_id}/rollback`，且目标版本存在、`is_current = false`、`term` 与当前版本相同，系统 SHALL 在单个事务内把目标版本置为当前版本、把原当前版本置为非当前版本，并返回 `rolled_back_from`、`current_release_id` 与 `term`。
2. WHEN 回退事务任一步失败，系统 SHALL 回滚事务、保持原当前版本为当前版本，并返回 HTTP 500 与业务码 `50000`。
3. WHEN 目标版本不存在，系统 SHALL 返回 HTTP 404 与业务码 `40401`。
4. WHEN 目标版本已是当前版本、库中没有当前版本，或同一目标的并发回退请求后到，系统 SHALL 返回 HTTP 409 与业务码 `40904`。
5. WHEN 目标版本与当前版本的 `term` 不同，系统 SHALL 返回 HTTP 409 与业务码 `40905`。

### Requirement 6 回退后的可见性与版本保留

**User Story:** AS 管理员，I want 回退立刻对访客生效且版本不被删，so that 这条操作既快又可逆。

#### Acceptance Criteria

1. WHEN 回退成功，系统 SHALL 在写出响应前清除公开查询的结果缓存（空教室、全天状态与数据大屏三处），使下一次公开查询与数据大屏读取新的当前版本。
2. WHEN 回退成功，系统 SHALL 保持 `release` 行数不变，并保持库中恰有一个 `is_current = true` 的版本。
3. WHEN 回退成功，系统 SHALL 让 `GET /api/admin/releases` 中 `is_current = true` 的条目从原当前版本变为目标版本，并保持该标志只出现在一个条目上。
4. WHEN 回退成功，系统 SHALL 保持 `collect_run.release_id` 与 `collect_run.released_at` 的原值。

### Requirement 7 异常数据视图

**User Story:** AS 管理员，I want 一个入口看数据质量，so that 不用翻日志就能判断哪一版不能信。

#### Acceptance Criteria

1. WHEN 携带有效管理会话的请求到达 `GET /api/admin/anomalies`，系统 SHALL 对库中每个版本返回 `release_id`、`term`、`generated_at`、`is_current`、`has_attr_conflict`、`unknown`、`composite`、`observed_weeks`、`week_gaps`、`window`、`conflict_rooms` 与 `conflict_rooms_truncated`。
2. WHEN 返回 `unknown` 与 `composite`，系统 SHALL 各给出 `cells` 与 `ratio`；WHEN 计算 `cells`，系统 SHALL 以「房号 × 周 × 星期 × 大节」计数，同一大节展开的小节只计一次；WHEN 计算 `ratio`，系统 SHALL 以该版本的格数为分母并保留 4 位小数。
3. WHEN 计算 `observed_weeks` 与 `week_gaps`，系统 SHALL 以该版观测周次的最小值与最大值为闭区间，按周次升序给出已覆盖的周与区间内缺失的周。
4. WHEN 该版本存在关联发布轮次，系统 SHALL 返回该轮的 `window`（`start` 与 `end`）；WHEN `has_attr_conflict = true` 且发布轮次仍可关联，系统 SHALL 在 `conflict_rooms` 中给出该轮的房号级冲突（`room_id`、`building_id`、`conflict_building_id`），超过 200 条时给出 `room_id` 最小的 200 条并置 `conflict_rooms_truncated = true`。
5. WHEN 库中没有版本，系统 SHALL 返回空 `releases` 数组。

### Requirement 8 访问控制、错误契约与装配

**User Story:** AS 运维，I want 新端点只走管理端且装配缺失立刻暴露，so that 版本切换能力不会成为绕过鉴权的入口。

#### Acceptance Criteria

1. WHEN 收到本 spec 读端点的请求，系统 SHALL 校验管理会话；WHEN 收到回退请求，系统 SHALL 另校验 CSRF 令牌，并在任一校验失败时不调用仓储。
2. WHEN 返回本 spec 任一端点的响应（含错误响应），系统 SHALL 设置 `Cache-Control: no-store`。
3. WHEN 输出本 spec 的请求日志，系统 SHALL 只记录 `request_id`、HTTP 方法、路径、HTTP 状态码与业务码。
4. WHEN 版本仓储未装配，系统 SHALL 以业务码 `50000` 拒绝本 spec 的五个端点。
5. WHEN 返回版本、对比或异常数据，系统 SHALL 只输出本文件与 design 声明的 `data` 键，并从响应中排除 `observation.raw_text`、账号、密码、会话与页面正文。
