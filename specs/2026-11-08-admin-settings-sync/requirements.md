# Requirements Document：admin-settings-sync

## Introduction

本 spec 交付 `easy-qfnu-kjs` 管理端的**业务设置、运行状态与同步触发**后端：七个设置项的读写与秘密加密、教学楼白名单与 cron 表达式校验、最近轮次与单轮详情读取、手动同步的冲突判定与启动、以及进程内 `Asia/Shanghai` 定时触发。

本 spec **只做后端 HTTP API 与进程内调度**，不创建前端页面。采集执行链（登录 → 学期周历 → 周矩阵 → 占用说明 → 校验 → 发布）由后续 `collect-runner` spec 实现；本 spec 定义「启动端口」并交付空白名单的`阻止`路径，未装配采集启动器时按内部错误暴露（见 Requirement 10、design D6）。

口径来源：`specs/2026-10-27-classroom-availability-rewrite/requirements.md` 的 Requirement 22（业务设置）、23（运行状态与手动同步）、14（调度）；已完成 spec `specs/2026-11-05-admin-bootstrap/`（会话、CSRF、错误码、密钥信封）；产品口径 `docs/prd.md` 5.2、5.3、4.3（失败、中断与恢复）、4.7（调度）。本文件取代上述旧条目中与之冲突的措辞（见 design「与既有文档的差异」）。

## Glossary

- **设置项**：`settings` 表里一行「键 → 值」，键集合固定为 `jw_username`、`jw_password`、`ocr_base_url`、`feishu_webhook`、`feishu_sign_secret`、`cron_expr`、`building_whitelist`。
- **秘密设置项**：值只写不读的三项：`jw_password`、`feishu_webhook`、`feishu_sign_secret`。
- **设置快照**：读取端点返回的当前设置视图，秘密项只以布尔「已设置」出现，不含任何秘密明文。
- **白名单**：管理员维护的教学楼条目集合，每条为「楼编号 `jxlbh` + 展示名 `name`」；顺序即采集与归属裁决顺序。
- **cron 表达式**：描述自动同步时刻的 5 字段表达式（分钟 小时 日 月 周），时区固定 `Asia/Shanghai`。
- **下一次触发时刻**：由当前 cron 表达式与当前时刻算出的、严格晚于当前时刻的第一个命中分钟。
- **轮次**：`collect_run` 表里一次同步的身份，含状态、阶段、窗口、任务计数、未知与复合计数、归属冲突计数、关联版本与错误信息。
- **进行中轮次**：`status = 'running'` 的轮次；`collect_run_single_running` 唯一索引保证全局至多一条。
- **阻止轮次**：白名单为空时建立的 `status = 'blocked'` 轮次，建立即终态、不发任何上游请求。
- **启动端口**：本 spec 定义的「启动一轮采集」能力边界；具体实现属 `collect-runner`。
- **触发来源**：`manual`（管理员手动）或 `cron`（进程内定时），只出现在日志，不落库。

## 范围与非目标

范围：`GET /api/admin/settings`、`PUT /api/admin/settings`、`GET /api/admin/runs`、`GET /api/admin/runs/{run_id}`、`POST /api/admin/sync` 五个端点；`settings` 读写与加解密、白名单与 cron 校验、cron 解析与进程内调度、启动端口与冲突判定、`collect_run` 只读视图、装配与门禁。

非目标：任何前端页面与静态资源；采集执行链（登录、学期周历、周矩阵、占用说明、发布）；版本对比与回退；告警发送；访问统计；公开查询改动；多管理员与多角色；多副本调度一致性。

## Requirements

### Requirement 1 设置读取

**User Story:** AS 管理员，I want 一屏看到当前设置与哪些秘密已配置，so that 不必靠猜或翻库。

#### Acceptance Criteria

1. WHEN 携带有效管理会话的请求到达 `GET /api/admin/settings`，系统 SHALL 返回设置快照，含 `jw_username`、`ocr_base_url`、`cron_expr`、`building_whitelist`、三项秘密的「已设置」标志与 `cron_next_at`。
2. WHEN 返回设置快照，系统 SHALL 对秘密设置项只返回 `jw_password_set`、`feishu_webhook_set`、`feishu_sign_secret_set` 三个布尔值，响应体不含秘密明文。
3. WHEN 某设置项从未写入，系统 SHALL 对字符串项返回空串、对秘密项返回 `false`、对白名单返回空数组。
4. WHEN 返回设置快照，系统 SHALL 返回按当前 `cron_expr` 与当前时刻算出的 `cron_next_at`；该表达式算不出下一次触发时刻时返回 `null`。
5. WHEN 请求未携带有效管理会话，系统 SHALL 返回业务码 `40101`。

### Requirement 2 设置局部更新

**User Story:** AS 管理员，I want 只提交改动的字段，so that 一次保存不会把没碰的项清掉。

#### Acceptance Criteria

1. WHEN 携带有效会话与正确 CSRF 令牌的请求到达 `PUT /api/admin/settings`，系统 SHALL 只修改请求体里出现的字段。
2. WHEN 请求体省略某字段，系统 SHALL 保持该字段原值。
3. WHEN 请求体把秘密字段设为空串，系统 SHALL 删除该设置项并把对应「已设置」置为 `false`。
4. WHEN 请求体把非秘密字符串字段设为空串，系统 SHALL 删除该设置项并以空串回显。
5. WHEN 请求体提交非空秘密字段，系统 SHALL 加密覆盖保存并把对应「已设置」置为 `true`。
6. WHEN 请求体提交白名单数组，系统 SHALL 以该数组整体替换原白名单并保持给定顺序。
7. WHEN 保存成功，系统 SHALL 返回与 `GET /api/admin/settings` 同一形状的最新快照。

### Requirement 3 秘密设置项加密

**User Story:** AS 运维，I want 库被看到时秘密仍是密文，so that 备份泄漏不等于账号泄漏。

#### Acceptance Criteria

1. WHEN 保存秘密设置项，系统 SHALL 以 AES-256-GCM 密文写入 `settings.value`，并把 `settings.is_secret` 记为 `true`。
2. WHEN 加密秘密设置项，系统 SHALL 以 `settings/<key>` 作为 AAD。
3. WHEN 加密密钥不可用且请求体包含非空秘密字段，系统 SHALL 以业务码 `50000` 拒绝本次保存，并保持全部设置原值不变。
4. WHEN 构造设置快照，系统 SHALL 按键名判定秘密项，不以 `settings.is_secret` 列的取值决定是否回显。
5. WHEN 读取秘密值供内部使用（采集执行），系统 SHALL 以同一 AAD 解封后才交给调用方；解封失败时系统 SHALL 返回错误，不以空值继续。
6. WHEN 加密密钥不可用且请求体只把秘密字段设为空串，系统 SHALL 照常删除该设置项，不要求密钥可用。

### Requirement 4 白名单校验

**User Story:** AS 管理员，I want 白名单在保存时就被检查，so that 采集不会拿到空编号或重复楼。

#### Acceptance Criteria

1. WHEN 白名单条目的楼编号在去除两侧空白后为空，系统 SHALL 以业务码 `42201` 拒绝本次保存，并在消息中指明 `building_whitelist`。
2. WHEN 两条白名单条目的楼编号在去除两侧空白后相同，系统 SHALL 以业务码 `42201` 拒绝本次保存，并在消息中指明重复的楼编号。
3. WHEN 白名单条目的展示名在去除两侧空白后为空，系统 SHALL 以业务码 `42201` 拒绝本次保存，并在消息中指明 `building_whitelist`。
4. WHEN 白名单条目数超过 200，系统 SHALL 以业务码 `42201` 拒绝本次保存。
5. WHEN 白名单为空数组，系统 SHALL 接受并保存该空名单。
6. WHEN 保存非空白名单，系统 SHALL 让后续读取按提交顺序返回去除两侧空白后的条目。

### Requirement 5 cron 表达式校验与时区

**User Story:** AS 管理员，I want 同步时刻被正确校验与解释，so that 表达式写错不会静默不跑。

#### Acceptance Criteria

1. WHEN 校验 cron 表达式，系统 SHALL 只接受 5 字段（分钟 小时 日 月 周）语法，每个字段接受 `*`、`*/n`、`a`、`a-b`、`a-b/n` 与逗号分隔的组合。
2. WHEN 分钟、小时、日、月、周的取值超出 `0–59`、`0–23`、`1–31`、`1–12`、`0–7`，系统 SHALL 以业务码 `42201` 拒绝本次保存，并在消息中指明越界字段。
3. WHEN cron 表达式字段数不是 5，或出现 `*/0`、倒序区间、空列表项、字母名称、`@every` 这类别名，系统 SHALL 以业务码 `42201` 拒绝本次保存，并在消息中指明 `cron_expr`。
4. WHEN cron 表达式校验通过并保存，系统 SHALL 让进程内调度按新表达式重排下一次触发，无需重启进程。
5. WHEN 计算触发时刻，系统 SHALL 以 `Asia/Shanghai` 时区的日历时间按分钟粒度匹配。
6. WHEN 返回设置快照，系统 SHALL 以 UTC RFC3339 返回 `cron_next_at`。
7. WHEN 日字段与周字段都受限（都不恰为 `*`），系统 SHALL 在两者任一命中时触发；只有一个字段受限时按该字段匹配。

### Requirement 6 设置写入的原子性与错误码

**User Story:** AS 管理员，I want 校验失败就整次不保存，so that 设置不会停在半套状态。

#### Acceptance Criteria

1. WHEN 请求体不是合法 JSON、`Content-Type` 不是 `application/json`，或请求体超过 64 KiB，系统 SHALL 以业务码 `40001` 拒绝且不写任何设置。
2. WHEN 请求体包含未定义的字段，系统 SHALL 以业务码 `40001` 拒绝且不写任何设置。
3. WHEN 任一字段校验失败，系统 SHALL 以业务码 `42201` 返回一条指出字段与原因的消息，并保持全部设置原值不变。
4. WHEN 一次保存包含多个字段且任一字段写入失败，系统 SHALL 回滚本次全部改动并返回业务码 `50000`。
5. WHEN 保存成功，系统 SHALL 让后续读取返回与本次提交一致的值。

### Requirement 7 运行状态读取

**User Story:** AS 管理员，I want 看到每轮跑到哪一步、错在哪，so that 排障不用翻日志。

#### Acceptance Criteria

1. WHEN 携带有效管理会话的请求到达 `GET /api/admin/runs`，系统 SHALL 按开始时刻倒序返回轮次摘要，默认 20 条，`limit` 取 `1–100`，`offset` 不小于 0；WHEN 参数越界，系统 SHALL 返回业务码 `40001`。
2. WHEN 返回一轮摘要，系统 SHALL 包含 `run_id`、`status`、`phase`、`started_at`、`finished_at`、`updated_at`、`term`、`window_start`、`window_end`、`weeks_total`、`rooms_total`、`required_total`、`required_done`、`optional_total`、`optional_done`、`unknown_cells`、`composite_cells`、`attr_conflict_count`、`release_id`、`released_at`、`error_code`、`error_message`。
3. WHEN 请求到达 `GET /api/admin/runs/{run_id}`，系统 SHALL 返回该轮摘要、任务汇总（含必需与可选任务的失败数）与已写入的逐周抓取结果。
4. WHEN 逐周抓取结果存在，系统 SHALL 每条包含 `week`、`building_id`、`ok`、`fetched_at`、`error_message`；没有行的楼周不出现在结果里。
5. WHEN 指定 `run_id` 不存在，系统 SHALL 返回业务码 `40401`。
6. WHEN 一条轮次都没有，系统 SHALL 返回空列表与 `total` 为 0。
7. WHEN 返回轮次数据，系统 SHALL 只输出 `collect_run`、`collect_run_week` 与任务汇总的字段，不含账号、密码与页面正文。

### Requirement 8 手动同步

**User Story:** AS 管理员，I want 出问题能立刻重抓，且不会叠着跑两轮，so that 数据尽快恢复而不打乱节奏。

#### Acceptance Criteria

1. WHILE 没有进行中轮次，WHEN 携带有效会话与正确 CSRF 令牌的请求到达 `POST /api/admin/sync`，系统 SHALL 启动一轮采集并返回 HTTP 202 与 `run_id`、`status`、`started_at`。
2. WHEN 存在开始时刻距今不超过 2 小时的进行中轮次，系统 SHALL 返回 HTTP 409 与业务码 `40901`，在消息中点明冲突轮次的 `run_id` 与 `phase`，且不启动新轮次。
3. WHEN 进行中轮次的开始时刻距今超过 2 小时，系统 SHALL 先把该轮置为 `failed`（`error_code` 为 `stale_run`）再按无进行中轮次处理。
4. WHEN 启动时白名单为空，系统 SHALL 建立一条 `status` 为 `blocked` 的轮次，把原因写入 `error_code`（`whitelist_empty`）与 `error_message`，并返回 HTTP 202 与 `run_id`。
5. WHEN 两个手动同步请求并发到达，系统 SHALL 至多让一个请求启动轮次，其余返回 HTTP 409 与业务码 `40901`。
6. WHEN 手动同步建立轮次，系统 SHALL 让该轮次立刻出现在 `GET /api/admin/runs` 的首条。
7. WHEN 手动同步请求的请求体为空，系统 SHALL 照常受理，不要求 JSON 请求体。

### Requirement 9 定时触发

**User Story:** AS 管理员，I want 到点自动跑一轮，so that 数据在固定时刻更新而不靠人盯。

#### Acceptance Criteria

1. WHILE 进程运行且已装配采集启动器，系统 SHALL 按保存的 cron 表达式在 `Asia/Shanghai` 到点时发起一轮采集。
2. WHEN 到点触发时已有一轮进行中，系统 SHALL 跳过本次触发并记录一条日志，不排队、不重试。
3. WHEN 到点触发失败，系统 SHALL 记录一条日志并在下一个计划时刻照常评估。
4. WHEN 保存新的 cron 表达式成功，系统 SHALL 立即按新表达式重排下一次触发。
5. WHEN 进程启动，系统 SHALL 读取已保存的 cron 表达式；WHEN 该键缺失，系统 SHALL 使用默认值 `0 10 4 * * 6`。
6. WHEN 启动时读取到的 cron 表达式非法，系统 SHALL 记录一条 ERROR、回落到默认值并继续启动。
7. WHEN 进程收到终止信号，系统 SHALL 停止调度并且不再发起新的轮次。
8. WHEN 定时触发启动轮次，系统 SHALL 与手动同步复用同一段启动逻辑，只以触发来源区分日志。

### Requirement 10 边界与降级

**User Story:** AS 运维，I want 新端点只走管理端、只在错误里暴露必要信息，so that 设置与凭据不会顺着响应或日志流出去。

#### Acceptance Criteria

1. WHEN 收到本 spec 任一端点的请求，系统 SHALL 校验管理会话；WHEN 处理写操作，系统 SHALL 另校验 CSRF 令牌。
2. WHEN 返回本 spec 任一端点的响应（含错误响应），系统 SHALL 设置 `Cache-Control: no-store`。
3. WHEN 输出本 spec 的请求日志，系统 SHALL 只记录 `request_id`、HTTP 方法、路径、HTTP 状态码、业务码与触发来源。
4. WHEN 返回设置或轮次响应，系统 SHALL 只使用本 spec 与 `admin-bootstrap` 已声明的 `data` 键。
5. WHEN 采集启动器未装配，系统 SHALL 以业务码 `50000` 拒绝白名单非空的手动同步、让定时触发只记录一条日志，并让两种触发都建立零条轮次。
6. WHILE 加密密钥不可用，系统 SHALL 让设置读取照常返回非秘密项与秘密标志，并拒绝包含非空秘密字段的保存。
7. WHEN 设置仓储或轮次仓储未装配，系统 SHALL 让相关端点返回业务码 `50000`，不以 404 掩盖装配缺失。
