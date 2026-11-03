# Requirements Document

## Introduction

本 spec 交付「一轮采集到发布」的最小闭环：从教务上游取学期、周历、楼列表、周矩阵与占用说明，解析清洗成观测与说明，按窗口规划并执行任务，最后在单个事务内发布新版本并切换当前版本。失败分层、轮次互斥与断点续跑在同一轮内完成；告警、管理端与前端不在本 spec。

本 spec 从 `specs/2026-10-27-classroom-availability-rewrite/requirements.md` 的 Requirement 4–11 抽取，不改变已锁定的业务口径。

## Glossary

- **轮次**：一次从登录到发布的采集执行，状态取值见原 spec。
- **必需任务**：采集窗口内每栋白名单楼、每一周的周矩阵请求。
- **可选任务**：占用说明下钻请求。
- **候选集**：发布前组装的观测、说明与归属快照，尚未对公开查询可见。
- **派生状态**：某房号某周缺少矩阵行时，由占用说明补写的状态。
- **归属快照**：版本内「房号 → 教学楼」的映射。
- **采集窗口**：`C..min(Y, C+3)`，C 为当前教学周，Y 为学期总周数。

## Requirements

### Requirement 1 采集窗口与任务规划

**User Story:** AS 运维，I want 每轮只采当前可查窗口，so that 不浪费请求也不越过可查范围。

#### Acceptance Criteria

1. WHEN 一轮开始且当前教学周为 C、学期总周数为 Y，系统 SHALL 把采集窗口设为 `C..min(Y, C+3)`。
2. WHEN 采集窗口为空，系统 SHALL 把该轮置为「跳过」并结束，不发起矩阵请求。
3. WHEN 白名单为空，系统 SHALL 把该轮置为「阻止」并结束，不发起矩阵请求。
4. WHEN 规划任务，系统 SHALL 为窗口内每栋白名单楼、每一周各生成一条必需的矩阵任务，且同一楼同一周至多一条。

### Requirement 2 周矩阵解析

**User Story:** AS 运维，I want 矩阵解析严格匹配真实页面结构，so that 结构变化会被立刻发现而不是产生错数据。

#### Acceptance Criteria

1. WHEN 解析某楼某周的矩阵响应，系统 SHALL 通过页面中标识为 `dataList` 的表格定位目标表，并避开同名前缀的其他表格。
2. WHEN 读取表头，系统 SHALL 校验状态格集合与顺序是否与当前轴定义一致；不一致即判定结构失败。
3. WHEN 读取数据行，系统 SHALL 校验每行单元格数量与表头一致；不一致即判定结构失败。
4. IF 同一响应内出现重复房号，系统 SHALL 判定为结构失败。
5. WHEN 取得房号，系统 SHALL 依次尝试行属性、行内复选框、首列输入框三处来源。

### Requirement 3 占用说明解析

**User Story:** AS 校内使用者，I want 被占用的格子有课程与周次说明可查，so that 判断是否只是某几周占用。

#### Acceptance Criteria

1. WHEN 解析占用说明，系统 SHALL 只采纳三列且标签去冒号后属于四项字段的行。
2. WHEN 一格存在多条说明，系统 SHALL 按教务返回顺序全部保留。
3. WHEN 说明的周次范围与采集窗口求交为空，系统 SHALL 丢弃该条说明。
4. WHEN 明细页只有标题表、没有字段行，系统 SHALL 判定该格无说明且解析成功；连标题表都没有才判定结构失败。

### Requirement 4 状态清洗与节次展开

**User Story:** AS 校内使用者，I want 状态判定与字典一致，so that 结果不因代码写死而漂移。

#### Acceptance Criteria

1. WHEN 状态原文整词命中字典符号，系统 SHALL 写入对应语义键。
2. WHEN 状态原文为空，系统 SHALL 写入空闲语义键。
3. WHEN 状态原文是多个已知符号的组合，系统 SHALL 写入复合语义键并保留原文；未命中字典时写入未知语义键并保留原文。
4. WHEN 计算可用性，系统 SHALL 按字典的可用性字段投影，并把未知与复合投影为不可用。
5. WHEN 展开节次，系统 SHALL 把 5 个大节映射为 12 个小节，同一大节内各小节状态一致。

### Requirement 5 教室归属与冲突

**User Story:** AS 校内使用者，I want 按教学楼筛选教室，so that 不必记住房名与楼名的对应关系。

#### Acceptance Criteria

1. WHEN 某楼响应解析成功，系统 SHALL 把该响应内所有房号的归属记录为该楼。
2. WHEN 同一房号在同一轮出现于两栋楼，系统 SHALL 按白名单顺序与周次升序取首个命中的楼作为归属。
3. WHEN 归属冲突发生，系统 SHALL 丢弃该房号在后续楼响应中的格子数据，并在本轮记录一次冲突。
4. WHEN 写入发布版本，系统 SHALL 一并写入该版本的归属快照。

### Requirement 6 派生状态

**User Story:** AS 校内使用者，I want 缺矩阵行的周也能从占用说明推断状态，so that 结果不至于凭空消失。

#### Acceptance Criteria

1. WHILE 该房号在该周存在矩阵行，系统 SHALL 以矩阵结果作为该周状态来源。
2. IF 该房号在该周缺少矩阵行且占用说明覆盖该周，系统 SHALL 以说明补写状态并标记为派生。
3. IF 说明的周次范围解析失败或与该周无交集，系统 SHALL 跳过派生并保持该格无状态。

### Requirement 7 发布与版本切换

**User Story:** AS 运维，I want 发布是一次整体替换并可回退，so that 出错时线上数据仍然可用。

#### Acceptance Criteria

1. WHEN 必需任务全部完成且候选集非空，系统 SHALL 在单个事务内写入新版本并切换当前版本。
2. WHEN 发布事务任一步失败，系统 SHALL 整体回滚并保持原当前版本可用。
3. WHEN 发布成功，系统 SHALL 保留当前版本与上一版本，并删除更早版本。
4. WHEN 写入版本，系统 SHALL 记录该版本的字典版本、轴版本、归属快照与生成时间。
5. WHEN 候选集为空或必需任务未完成，系统 SHALL 拒绝发布并记录校验拦截原因。

### Requirement 8 失败分层、互斥与续跑

**User Story:** AS 运维，I want 必需与可选任务的失败后果分明且长同步可续跑，so that 明细抓不全时可用性不受影响。

#### Acceptance Criteria

1. WHEN 窗口内任一必需周矩阵失败，系统 SHALL 把轮次置为「失败」并保留线上版本。
2. WHEN 仅占用说明任务失败，系统 SHALL 发布新版本并把轮次置为「部分完成」。
3. WHILE 一轮处于「进行中」，系统 SHALL 拒绝启动新的一轮；服务启动时超过 2 小时的「进行中」轮次 SHALL 被置为失败并记录原因。
4. WHEN 一轮重新开始，系统 SHALL 从第一个未完成任务继续、跳过已完成任务，且同一轮内失败任务不再重试。
