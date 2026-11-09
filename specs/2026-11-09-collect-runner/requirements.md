# Requirements Document

## Introduction

本 spec 交付采集执行器（`collect-runner`）：`runner.Collector` 的生产实现。范围从 `runner.Start` 调用 `Collector.Start` 开始，到一轮 `collect_run` 落成 `success` / `partial` / `failed`（另有无需请求的 `skipped` 与 `blocked`）为止，覆盖六件事——建轮与后台执行、学期与采集窗口（含 `collect_run` 的窗口写入口）、任务规划与两段执行（周矩阵 → 待下钻的格）、矩阵与占用说明的抓取清洗、内存候选的组装与按归属过滤、以及发布（`publish.Build` → `Validate` → `Commit`）。

产品口径沿用 [docs/prd.md](../../docs/prd.md) 2.3–2.8、4.3–4.5；上游结构见 [docs/upstream-api.md](../../docs/upstream-api.md)；已确认坑点见 [docs/pitfalls.md](../../docs/pitfalls.md)。本 spec 从 `specs/2026-10-27-classroom-availability-rewrite/` 抽取采集窗口（Requirement 1）、学期与周历（3）、矩阵（4）、归属（5）、占用说明（6）、发布（9）、失败分层与续跑（10、11），不改变已锁定的业务口径。

本 spec **不包含**：公开查询与前端（已交付）、管理端设置与运行状态读接口（`admin-settings-sync` 已交付）、告警与飞书（后续 spec）、版本对比与回退（`admin-release`）、启动后续跑入口（`runner` 的 409 语义本 spec 不改）。

## Glossary

- **采集执行器**：`runner.Collector` 的实现，本 spec 唯一交付物。
- **采集轮次（轮次）**：`collect_run` 的一行，状态取值 `running|success|partial|failed|blocked|skipped`。
- **收口**：把轮次写成终态并写 `finished_at`；失败收口另写 `error_code` 与 `error_message`。
- **当前教学周 `C`**：周历页自报的教学周序号。
- **学期总周数 `Y`**：周历页自报的学期总周数。
- **采集窗口**：`C..min(Y, C+3)` 的连续周集合。
- **学期日历**：`term` 与 `term_week` 两表，覆盖第 1..Y 周每周的周一日期。
- **白名单教学楼**：设置项 `building_whitelist` 的条目，`jxlbh` 即教务楼编号。
- **周矩阵**：一栋楼一周的 7 天 × 大节数状态格集合。
- **不可用格**：清洗后 `available=false` 的状态格，按「房号 × 星期 × 大节」计。
- **占用说明**：某格在教务的说明，只保留教室状态、课程、周次范围与时间标志四项。
- **内存候选**：发布前攒在进程内的矩阵观测与占用说明，`observation` 以 `release_id` 为主键，发布前没有表可落。
- **归属快照**：一轮内「房号 → 教学楼」的有效归属，落在 `collect_run_room`，发布时转写 `release_room_building`。
- **必需任务 / 可选任务**：周矩阵任务必需、占用说明任务可选。
- **断点续跑**：同一轮内从第一个未完成任务继续；本轮内失败的任务不再重试。

## Requirements

### Requirement 1 轮次建立与后台执行

**User Story:** AS 运维，I want 点下同步就立刻拿到轮次编号、执行在后台继续，so that HTTP 请求不必等一轮采集跑完。

#### Acceptance Criteria

1. WHEN 采集执行器收到启动请求且库里没有进行中轮次，采集执行器 SHALL 在返回前把一条 `status=running` 的轮次写入 `collect_run` 并返回该轮次编号与开始时刻。
2. WHEN `collect_run` 上已有 `status=running` 的轮次，采集执行器 SHALL 返回包装 `model.ErrRunConflict` 的错误，并保持 `collect_run` 的行数不变。
3. WHILE 后台执行一轮采集，采集执行器 SHALL 使用进程生命周期上下文作为唯一上下文来源。
4. IF 后台执行因 panic 中断，采集执行器 SHALL 把该轮次收口为 `failed` 并写入一条内部错误原因。

### Requirement 2 学期、当前周与采集窗口

**User Story:** AS 运维，I want 每轮只采当前可查的四周边界内周，so that 请求量与可查范围都可预期。

#### Acceptance Criteria

1. WHEN 登录成功，采集执行器 SHALL 读取学期页与周历页，并以 parser 的文本解析结果作为学期编号、当前教学周 `C` 与学期总周数 `Y` 的权威口径。
2. WHEN 取得 `C` 与 `Y`，采集执行器 SHALL 把采集窗口定为 `C..min(Y, C+3)`，并把 `window_start`、`window_end` 与 `weeks_total` 写入本轮 `collect_run`。
3. IF 采集窗口为空，采集执行器 SHALL 把本轮收口为 `skipped` 并结束，周矩阵请求数恒为 0。
4. IF 学期页或周历页的解析失败，采集执行器 SHALL 把本轮收口为 `failed`、`error_code=calendar_invalid`，并保持库里已有的学期日历不变。

### Requirement 3 学期日历写入

**User Story:** AS 校内使用者，I want 任意日期都能换算成第几周，so that 查询能落到正确的周与星期。

#### Acceptance Criteria

1. WHEN 学期编号与总周数确定，采集执行器 SHALL 在单个事务内写入或刷新该学期的 `term` 行与第 1..Y 周的周一日期。
2. WHEN 计算第 1 周周一，采集执行器 SHALL 使用「采样日所在周周一减 7×(C−1)」，并把日期按 `Asia/Shanghai` 解释。
3. IF 库里已有该学期的日历且第 1 周周一与新算值不同，采集执行器 SHALL 把本轮收口为 `failed`、`error_code=calendar_invalid`，并保留原日历。
4. WHEN 同一学期被重复写入，采集执行器 SHALL 保持 `term_week` 的行数与周一日期不变。

### Requirement 4 白名单与设置桥接

**User Story:** AS 管理员，I want 面板里维护的楼编号直接就是教务的楼编号，so that 不必再维护第二套映射表。

#### Acceptance Criteria

1. WHEN 一轮开始执行，采集执行器 SHALL 从设置读取教学楼白名单一次，并把每个条目的 `jxlbh` 作为教务楼编号传给上游请求与任务规划。
2. WHEN 使用白名单，采集执行器 SHALL 对每个楼编号去两侧空白、按首次出现去重，并把同一份有序序列同时交给规划与执行。
3. IF 教务账号、教务密码或 OCR 地址任一缺失或不可解密，采集执行器 SHALL 把本轮收口为 `failed`、`error_code=settings_incomplete`，并在原因里点名缺失的设置键。
4. WHEN 读取教务楼列表失败，或白名单条目的楼编号不在该校区楼列表内，采集执行器 SHALL 记录一条日志并继续本轮。

### Requirement 5 任务规划与两段执行

**User Story:** AS 运维，I want 任务清单只由规划算法产生，so that 同一栋楼同一周不会被重复请求。

#### Acceptance Criteria

1. WHEN 采集窗口与白名单确定，采集执行器 SHALL 为窗口内每栋白名单楼每一周生成一条必需周矩阵任务，同一楼同一周至多一条。
2. WHEN 周矩阵段执行完成，采集执行器 SHALL 用本轮的矩阵结果算出不可用格，再规划占用说明任务。
3. WHEN 生成占用说明任务，采集执行器 SHALL 保证同一「房号 × 星期 × 大节」至多一条，且任务的楼编号等于本轮归属快照里该房号的楼。
4. WHEN 执行规划结果，采集执行器 SHALL 通过 `pipeline.Run` 落库并执行两段任务，任务落库与状态推进由 `pipeline` 唯一负责。

### Requirement 6 周矩阵抓取与不可用格

**User Story:** AS 校内使用者，I want 被占用的格子能下钻说明，so that 知道只是某几周有课还是整周占用。

#### Acceptance Criteria

1. WHEN 抓取某楼某周的周矩阵，采集执行器 SHALL 用 parser 解析页面、用 clean 把每格原文投影成状态，并把投影为不可用的格记为不可用格。
2. WHEN 一格的清洗结果在窗口内任一周不可用，采集执行器 SHALL 为该格生成一条占用说明任务。
3. WHEN 同一楼同一周的矩阵被重复取得，采集执行器 SHALL 用后一次结果覆盖前一次的内存候选，重复观测行数恒为 0。
4. IF 周矩阵解析返回结构失败，采集执行器 SHALL 让该任务失败，并把轮次终态交给 `pipeline` 的失败分层决定。

### Requirement 7 占用说明抓取

**User Story:** AS 校内使用者，I want 占用说明只有四项字段，so that 任课教师与申请人不会被存下来。

#### Acceptance Criteria

1. WHEN 执行一条占用说明任务，采集执行器 SHALL 用 parser 解析明细页、用 clean 投影教室状态，并只保留教室状态、课程、周次范围与时间标志四项。
2. WHEN 一格存在多条说明，采集执行器 SHALL 按教务返回顺序全部保留。
3. IF 明细页连标题表都没有，采集执行器 SHALL 让该任务失败，并把轮次终态交给 `pipeline` 的失败分层决定。
4. WHEN 同一格被重复抓取，采集执行器 SHALL 用后一次结果覆盖前一次的内存候选。

### Requirement 8 断点续跑与内存候选重建

**User Story:** AS 运维，I want 长采集被中断后不重打已完成的任务，so that 上万个明细请求不必一次成功。

#### Acceptance Criteria

1. WHEN 本轮已有完成的周矩阵任务，采集执行器 SHALL 跳过该任务的请求，由 `pipeline.Run` 从第一个未完成任务继续执行。
2. WHEN 某「楼 × 周」的周矩阵任务已完成而内存没有该楼该周的矩阵结果，采集执行器 SHALL 在发布前补抓一次该页，并只用该页重建内存候选。
3. IF 补抓失败，采集执行器 SHALL 把本轮收口为 `failed`、点名该楼与该周，并保持原当前版本可用。
4. WHEN 组装内存候选，采集执行器 SHALL 只采纳本轮归属快照 `collect_run_room` 里的房号，并按快照的楼过滤矩阵格。
5. WHEN 归属快照里没有某房号的归属，采集执行器 SHALL 丢弃该房号的全部矩阵格并计入一个诊断计数。

### Requirement 9 发布

**User Story:** AS 校内使用者，I want 一次发布整体替换且失败时线上照旧，so that 采集出错不会把查询打空。

#### Acceptance Criteria

1. WHEN 必需任务与可选任务全部完成，采集执行器 SHALL 用 `publish.Build`、`publish.Validate` 与 `publish.Commit` 发布新版本，并把轮次写成 `success`。
2. WHEN 只有可选任务失败或被丢弃，采集执行器 SHALL 照常发布，并把轮次写成 `partial`。
3. IF 候选集为空或发布前校验拦截，采集执行器 SHALL 把本轮收口为 `failed`、`error_code=validation_blocked`，并保留原当前版本。
4. IF 发布事务失败，采集执行器 SHALL 把本轮收口为 `failed`，并保持原当前版本可用。
5. WHEN 进入发布前校验，采集执行器 SHALL 先把轮次阶段推进到 `validate`。

### Requirement 10 失败收口与错误码

**User Story:** AS 运维，I want 每一轮的失败都留在库里带原因，so that 面板能解释这一轮为什么没数据。

#### Acceptance Criteria

1. WHEN 任一阶段失败，采集执行器 SHALL 用 `pipeline` 的收口函数把轮次写成 `failed`，并写入 `error_code` 与 `error_message`。
2. WHEN 上游判定教务密码错误，采集执行器 SHALL 把本轮收口为 `failed` 并写 `error_code=auth_failed`。
3. WHEN 验证码识别耗尽 3 次尝试，采集执行器 SHALL 把本轮收口为 `failed` 并写 `error_code=ocr_failed`。
4. WHILE 调用方取消请求或进程正在退出，采集执行器 SHALL 保持轮次 `status=running`，把未完成任务留给下一次执行。
5. WHEN 一轮发布成功，采集执行器 SHALL 保留发布事务写下的终态，收口函数对该轮的调用次数恒为 0。
6. WHEN 失败收口完成，采集执行器 SHALL 写入非空 `finished_at` 并把阶段写成 `finished`；发布成功的轮次阶段停在 `publish`。
7. WHEN 周矩阵段执行后必需任务已经失败，采集执行器 SHALL 直接收口 `failed` 并跳过全部占用说明任务。
