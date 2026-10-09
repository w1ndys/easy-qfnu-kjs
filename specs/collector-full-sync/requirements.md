# Requirements Document

## Introduction

采集器按管理员配置的教学楼白名单，把当前学期第 1 周到最后一周的教室状态写入本地数据库，并对聚合矩阵上有内容的格子保存一次占用说明。公开查询只读已发布快照，用日期偏移在本地周历中定位周次。

本文件只覆盖采集与同步。查询端点、面板登录和前端页面不在本功能内。

## Glossary

- **采集器**: 本机常驻进程。它登录教务系统、请求教室状态、清洗并发布。
- **查询服务**: 只读 HTTP 服务。它从 PostgreSQL 读取已发布快照，不登录教务系统。
- **白名单教学楼**: 管理员保存的教学楼。每条含 `jxlbh` 与展示名。
- **周矩阵**: `jsjy_query2` 在指定周次返回的教室状态表。
- **聚合矩阵**: 周次留空时 `jsjy_query2` 返回的全学期状态表。
- **占用明细**: `jsjy_jszyqk` 对一个有内容格子返回的说明。
- **周历**: `term_week` 中每一教学周的周一日期。
- **同步任务**: 一轮全量中的一个可续传单元，粒度是「楼 + 周矩阵」或「楼 + 房间 + 格子明细」。
- **已发布快照**: `release.is_current = true` 的观测。

## Requirements

### Requirement 1: 学期周历

**User Story:** 作为查询服务，我要有一份本学期每周的周一日期，以便把用户给出的日期偏移换成周次和星期。

#### Acceptance Criteria

1. WHEN 采集器开始一轮同步，采集器 SHALL 从 `GET /jsxsd/kbxx/jsjy_query` 读取当前学期编号。
2. WHEN 采集器读取周次，采集器 SHALL 以 `Asia/Shanghai` 的当天日期请求一次 `POST /jsxsd/framework/main_index_loadkb.jsp`，表单字段为 `rq`。
3. WHEN 该响应含「第 X 周 / 共 Y 周」且 X、Y 为正整数且 X 小于或等于 Y，采集器 SHALL 写入第 1 周到第 Y 周的周一日期，并将这些行标为教学周。
4. IF 响应缺少该周次文本，或 X、Y 不是正整数，或 X 大于 Y，采集器 SHALL 把本轮标为失败并保留上一份已发布快照。
5. WHEN 查询服务收到 `date_offset`，查询服务 SHALL 用 `Asia/Shanghai` 的今天加上该偏移得到目标日期，并只在 `term_week` 中解析周次与星期。

### Requirement 2: 采集范围

**User Story:** 作为管理员，我要只采集选定的教学楼，以便结果里不出现未选定楼的房间。

#### Acceptance Criteria

1. WHEN 管理员保存白名单，系统 SHALL 保存每条教学楼的 `jxlbh` 与展示名。
2. WHEN 采集器请求一栋楼的周矩阵或聚合矩阵，采集器 SHALL 把该楼的 `jxlbh` 放入请求，并把教室关键词留空。
3. IF 白名单为空，采集器 SHALL 拒绝开始本轮，并把运行状态标为 `blocked`。
4. WHEN 一个房间来自某次按楼请求，采集器 SHALL 把该次请求的 `jxlbh` 与展示名记到该房间。
5. IF 同一轮里同一个 `jsbh` 出现在两栋白名单楼的结果中，采集器 SHALL 把本轮标为失败并停止发布。

### Requirement 3: 各周状态

**User Story:** 作为查询用户，我要看到选定日期所在周的教室状态，以便判断该节是否空闲。

#### Acceptance Criteria

1. WHEN 一轮同步运行，采集器 SHALL 对每一栋白名单楼、从第 1 周到周历总周数的每一个周次各请求一次周矩阵。
2. WHEN 周矩阵返回一个教室格，采集器 SHALL 按已定状态字典把该格记为语义状态。
3. WHEN 某一周的周矩阵已成功写入，采集器 SHALL 以该周矩阵作为该周空闲判定的来源。
4. IF 某一个周矩阵请求在 3 次重试后仍失败，采集器 SHALL 记下该任务并继续其余任务。
5. IF 周矩阵表头块集合与当前节次轴不一致，采集器 SHALL 终止本轮并发送告警。

### Requirement 4: 占用说明

**User Story:** 作为查询用户，我要点开一个有课或有考试的格子，以便看到课程或考试科目和覆盖周次。

#### Acceptance Criteria

1. WHEN 一轮同步已取得某楼的聚合矩阵，采集器 SHALL 只对有内容的格子各请求一次占用明细。
2. WHEN 占用明细返回教室状态、课程、周次范围和时间标志，采集器 SHALL 保存这四项。
3. WHEN 某一周已有周矩阵，采集器 SHALL 保留该周矩阵的空闲判定，并把占用明细仅作为该格的说明。
4. IF 某一周没有周矩阵且占用明细的周次范围覆盖该周，采集器 SHALL 为该周写入派生状态，并标记该状态来自明细范围。
5. IF 周次范围无法解析，采集器 SHALL 保持该周为未知状态。

### Requirement 5: 同步控制

**User Story:** 作为管理员，我要看到同步进度，并能按周六凌晨自动跑或手动开始同一套全量。

#### Acceptance Criteria

1. WHILE 一轮同步运行，系统 SHALL 记录阶段、完成数、总数、最近错误和更新时间。
2. WHEN 一个同步任务完成，采集器 SHALL 在同一轮内跳过该任务。
3. IF 会话失效，采集器 SHALL 重新登录，并从第一个未完成任务继续。
4. WHEN 管理员手动开始，采集器 SHALL 运行与定时触发相同的全量任务。
5. WHEN 到达 `cron_expr` 指定的时间，采集器 SHALL 运行该全量任务。
6. WHILE 已有一轮处于 `running`，采集器 SHALL 拒绝开始第二轮。
7. WHEN 采集器发出上游请求，采集器 SHALL 保证相邻请求间隔至少 500 毫秒。

### Requirement 6: 发布与查询隔离

**User Story:** 作为查询用户，我要在同步失败或进行中时仍能查到上一份成功数据。

#### Acceptance Criteria

1. WHEN 本轮全部必需任务完成且校验通过，采集器 SHALL 在一个事务中发布新的 current，并把原 current 降为 previous。
2. IF 本轮失败或被阻断，采集器 SHALL 保留当前 current。
3. WHILE 同步状态为 `running`，查询服务 SHALL 继续返回当前 current，并把该轮视为未失败。
4. WHEN 最近一轮状态为 `failed`，查询服务 SHALL 把已有成功数据标为过期。
5. WHEN 某周距最近成功超过 8 天，查询服务 SHALL 把该周标为过期，并仍返回该周已发布数据。
