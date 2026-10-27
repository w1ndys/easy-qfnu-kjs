# Requirements Document

重写 `easy-qfnu-kjs`（曲阜师范大学空教室查询）的产品需求。基线是 [docs/prd.md](../../docs/prd.md)；上游请求与响应结构见 [docs/upstream-api.md](../../docs/upstream-api.md)，已知坑点见 [docs/pitfalls.md](../../docs/pitfalls.md)。本文件把基线的产品结论转成可验收的 EARS 条目，不含实现技术选型。

范围：采集、教室归属、发布与版本、公开查询、管理员初始化与配置、管理面板、前端体验、非功能。

## Glossary

- **采集窗口**：当前周 `C` 至 `min(Y, C+3)` 的连续周集合，含当前周至多 4 周。
- **当前周 `C`**：采样日所属教学周。
- **总周数 `Y`**：学期周历声明的总教学周数。
- **白名单教学楼**：管理员配置的「楼编号 + 展示名」条目。
- **周矩阵**：一栋楼一周的 7 天 × 5 大节状态集合，共 35 个状态格。
- **状态格**：某教学楼某周某天某大节的状态单元。
- **归属**：教室与教学楼的对应关系，来源为按楼筛选请求的响应。
- **占用说明**：教务对某状态格给出的说明，含教室状态、课程、周次范围、时间标志。
- **发布版本**：一次成功发布的逐格数据集，并内含归属快照。
- **当前版本 / 上一版本**：公开查询读取的唯一版本，与其保留用于回退的上一版。
- **空闲**：状态格无占用。
- **可用**：健康状态的字典投影结果为可检索，空闲属于可用。
- **未知 / 复合**：未收录文本 / 多个已知符号组合，二者投影为不可用。
- **过期**：某周距最近一次成功抓取该周矩阵超过 8 天，或最近一轮同步失败。
- **部分完成**：必需任务全部完成且占用说明任务存在失败时的轮次状态。
- **归属冲突**：同一房号在同一轮出现在两栋楼的响应中。
- **派生状态**：由占用说明补写、且该房号该周缺少矩阵行的状态。

## Requirements

### Requirement 1 采集窗口

**User Story:** AS 校内使用者，I want 只采当前与未来三周的数据，so that 每次同步的请求量与数据量可预期。

#### Acceptance Criteria

1. WHEN 采样取得当前周 `C` 与总周数 `Y`，系统 SHALL 将采集窗口定义为 `C` 至 `min(Y, C+3)`。
2. WHEN 生成学期日历，系统 SHALL 为第 1 至第 `Y` 周各生成一条周一日期，覆盖整个学期。
3. WHEN 生成任务清单，系统 SHALL 将采集窗口之外的周排除在请求与入库范围之外。
4. IF 采集窗口为空，系统 SHALL 将本轮状态置为「跳过」并保留既有当前版本。

### Requirement 2 教师端登录与会话

**User Story:** AS 运维，I want 采集使用单一教师账号会话跑通登录，so that 数据能持续更新。

#### Acceptance Criteria

1. WHEN 一轮同步开始，系统 SHALL 先写入轮次记录并将状态置为「进行中」。
2. WHEN 获取验证码图片，系统 SHALL 调用配置的 OCR 服务并以响应 `code=200` 判定识别成功。
3. IF 验证码识别失败，系统 SHALL 更换验证码图片并重试，累计尝试次数不超过 3 次。
4. IF 教务返回密码错误标记，系统 SHALL 立即结束本轮并将状态置为「失败」。
5. WHEN 业务页正文出现会话失效标记，系统 SHALL 重新登录一次并从当前任务继续。
6. WHILE 采集进行中，系统 SHALL 保持单一会话，并将会话凭据保存在内存。

### Requirement 3 学期与周历

**User Story:** AS 校内使用者，I want 查询能按日期定位到学期周，so that 结果落到正确的周与星期。

#### Acceptance Criteria

1. WHEN 读取学期页，系统 SHALL 提取学期编号与节次模式标识。
2. WHEN 写入学期数据，系统 SHALL 在同一事务内整体替换学期与逐周日期。
3. WHEN 读取周历页，系统 SHALL 解析「第 X 周 / Y 周」并校验 `1 ≤ X ≤ Y`。
4. IF 周历文本缺失或周次非法，系统 SHALL 结束本轮且保持学期日历不变。

### Requirement 4 周矩阵采集与解析

**User Story:** AS 运维，I want 矩阵解析严格匹配真实页面结构，so that 结构变化会被立刻发现而不是产生错数据。

#### Acceptance Criteria

1. WHEN 请求某楼某周矩阵，系统 SHALL 仅使用白名单楼编号与该周周次，且每楼每周发起一次请求。
2. WHEN 解析矩阵，系统 SHALL 通过页面结构中标识为 `dataList` 的表格定位目标表。
3. WHEN 读取表头，系统 SHALL 校验 35 个状态格的集合与顺序是否与轴定义一致。
4. IF 表头状态格不一致，系统 SHALL 终止整轮并记录结构失败。
5. IF 数据行单元格数量与表头不一致，系统 SHALL 判定为结构失败。
6. IF 同一响应内出现重复房号，系统 SHALL 判定为结构失败。
7. WHEN 取得房号，系统 SHALL 依次尝试行属性、行内复选框、首列输入框三处来源。

### Requirement 5 教室归属

**User Story:** AS 校内使用者，I want 按教学楼筛选教室，so that 不必记住房名与楼名的对应关系。

#### Acceptance Criteria

1. WHEN 某楼响应解析成功，系统 SHALL 将该响应内所有房号的归属记录为该楼。
2. WHEN 生成规范化房名，系统 SHALL 依次执行全半角归一、删除容量片段、压缩空白，并保留原始房名。
3. WHEN 同一房号在同一轮出现于两栋楼，系统 SHALL 按白名单顺序与周次升序取首个命中的楼作为归属。
4. WHEN 归属冲突发生，系统 SHALL 丢弃该房号在后续楼响应中的格子数据，并在本轮记录一次冲突。
5. WHEN 教室缺少归属记录，系统 SHALL 将该教室排除在公开查询的楼筛选之外，并在管理面板异常视图列出。
6. WHEN 写入发布版本，系统 SHALL 一并写入该版本的归属快照。

### Requirement 6 占用说明

**User Story:** AS 校内使用者，I want 被占用的格子有课程与周次说明可查，so that 判断是否只是某几周占用。

#### Acceptance Criteria

1. WHEN 窗口内某格在任一周不可用，系统 SHALL 为该格创建一次下钻任务。
2. WHEN 解析占用说明，系统 SHALL 扫描整页表格行并仅采纳三列且标签属于四项字段的行。
3. WHEN 写入占用说明，系统 SHALL 仅保留教室状态、课程、周次范围与时间标志。
4. WHEN 说明的周次范围与采集窗口求交后为空，系统 SHALL 丢弃该条说明。
5. WHEN 一格存在多条说明，系统 SHALL 按教务返回顺序全部保留。

### Requirement 7 状态口径与清洗

**User Story:** AS 校内使用者，I want 状态判定与字典一致，so that 结果不因代码写死而漂移。

#### Acceptance Criteria

1. WHEN 状态原文整词命中字典符号，系统 SHALL 写入对应语义键。
2. WHEN 状态原文为空，系统 SHALL 写入空闲语义键。
3. WHEN 状态原文为多个已知符号组合，系统 SHALL 写入复合语义键并保留原文。
4. WHEN 状态原文未命中字典，系统 SHALL 写入未知语义键并保留原文。
5. WHEN 计算可用性，系统 SHALL 按字典的可用性字段投影，并将未知与复合投影为不可用。
6. WHEN 展开节次，系统 SHALL 将 5 大节映射为 12 小节，同一大节内各小节状态保持一致。

### Requirement 8 派生状态

**User Story:** AS 校内使用者，I want 缺矩阵行的周也能从占用说明推断状态，so that 结果不至于凭空消失。

#### Acceptance Criteria

1. WHILE 该房号在该周存在矩阵行，系统 SHALL 以矩阵结果作为该周状态来源。
2. IF 该房号在该周缺少矩阵行且占用说明覆盖该周，系统 SHALL 以说明补写状态并标记为派生。
3. IF 说明的周次范围解析失败或与该周无交集，系统 SHALL 跳过派生并保持该格无状态。

### Requirement 9 发布与版本切换

**User Story:** AS 运维，I want 发布是一次整体替换并可回退，so that 出错时线上数据仍然可用。

#### Acceptance Criteria

1. WHEN 必需任务全部完成且候选数据非空，系统 SHALL 在单个事务内写入新版本并切换当前版本。
2. WHEN 发布事务任一步失败，系统 SHALL 整体回滚并保持原当前版本可用。
3. WHEN 发布成功，系统 SHALL 保留当前版本与上一版本，并删除更早版本。
4. WHEN 写入版本，系统 SHALL 记录该版本的字典版本、轴版本、归属快照与生成时间。
5. WHEN 统计未知与复合，系统 SHALL 按房 × 周 × 天 × 大节计数，同一大节展开的小节只计一次。

### Requirement 10 失败分层与轮次状态

**User Story:** AS 运维，I want 必需与可选任务的失败后果分明，so that 明细抓不全时可用性不受影响。

#### Acceptance Criteria

1. WHEN 窗口内任一必需周矩阵失败，系统 SHALL 将轮次置为「失败」并保留线上版本。
2. WHEN 仅占用说明任务失败，系统 SHALL 发布新版本并将轮次状态置为「部分完成」。
3. WHEN 白名单为空，系统 SHALL 将轮次置为「阻止」并跳过全部矩阵请求。
4. WHEN 采集窗口为空，系统 SHALL 将轮次置为「跳过」。
5. WHEN 候选数据为空或必需任务未完成，系统 SHALL 拒绝发布并记录校验拦截原因。

### Requirement 11 中断恢复与互斥

**User Story:** AS 运维，I want 长同步可以中断续跑，so that 上万次明细请求不必一次成功。

#### Acceptance Criteria

1. WHEN 一轮重新开始，系统 SHALL 从第一个未完成任务继续，并跳过已完成任务。
2. WHEN 某任务在同一轮失败，系统 SHALL 保持该任务为失败状态且不再重试。
3. WHILE 一轮处于「进行中」，系统 SHALL 拒绝启动新的一轮。
4. WHEN 服务启动并发现超过 2 小时的「进行中」轮次，系统 SHALL 将其置为失败并记录过期轮次原因。
5. WHEN 发起相邻上游请求，系统 SHALL 保持至少 500 毫秒间隔，单次请求重试不超过 3 次。

### Requirement 12 归属冲突标记

**User Story:** AS 运维，I want 归属冲突在轮次上可见但不阻断发布，so that 同步不会长期卡在旧版本。

#### Acceptance Criteria

1. WHEN 一轮出现归属冲突，系统 SHALL 在轮次与版本上附加「有归属冲突」标记。
2. WHEN 附加该标记，系统 SHALL 保持轮次的发布状态取值为成功、部分完成、失败、阻止或跳过之一。
3. WHEN 下一轮无归属冲突，系统 SHALL 生成不带该标记的轮次记录。

### Requirement 13 告警

**User Story:** AS 运维，I want 异常能在飞书群里收到一次，so that 不必盯面板。

#### Acceptance Criteria

1. WHEN 必需任务失败、发布校验拦截、学期切换、连续失败后首次恢复或出现归属冲突，系统 SHALL 发送一条飞书告警。
2. WHEN 同一轮同一原因再次触发，系统 SHALL 依据去重键抑制重复告警。
3. WHEN 轮次状态为「部分完成」，系统 SHALL 在面板展示状态与最近错误并抑制告警。
4. WHEN 生成告警内容，系统 SHALL 排除账号、密码、会话、验证码与页面正文。

### Requirement 14 调度

**User Story:** AS 管理员，I want 自动同步按我设定的时间跑，so that 数据在每周固定时点更新。

#### Acceptance Criteria

1. WHILE 到达配置的执行时间，系统 SHALL 按 `Asia/Shanghai` 时区启动一轮窗口采集。
2. WHEN 管理员修改执行时间，系统 SHALL 在保存前校验表达式格式。
3. WHEN 管理员发起手动同步，系统 SHALL 复用与定时同步相同的采集流程。

### Requirement 15 空教室查询

**User Story:** AS 学生，I want 按日期、节次与教学楼筛出整段空闲的教室，so that 直接去自习。

#### Acceptance Criteria

1. WHEN 用户提交日期、节次区间、教学楼与关键词，系统 SHALL 返回在该区间内每一节都可用且落在筛选范围内的教室。
2. WHEN 用户选择「全部」，系统 SHALL 将结果限制在当前白名单楼下的教室集合。
3. WHEN 用户选择某栋楼，系统 SHALL 仅返回归属该楼的教室。
4. WHEN 用户输入关键词，系统 SHALL 按房名子串匹配，执行全半角归一并对 ASCII 做大小写不敏感比较。
5. WHEN 输出结果，系统 SHALL 按房名升序、同房名按房号升序排序，并分页返回总数。
6. WHEN 展示结果，系统 SHALL 显示房名、房号与所属教学楼。

### Requirement 16 全天状态查询

**User Story:** AS 学生，I want 看某间教室一天的 12 节状态，so that 判断哪几节能用。

#### Acceptance Criteria

1. WHEN 用户提交日期与筛选条件，系统 SHALL 返回该教室该日已存在的 12 小节状态。
2. WHEN 某小节缺少观测，系统 SHALL 将该小节标记为无数据。
3. WHEN 用户提交教学楼筛选，系统 SHALL 应用与空教室查询一致的归属过滤。

### Requirement 17 上下文与新鲜度

**User Story:** AS 学生，I want 知道今天是第几周、数据新不新，so that 判断结果是否可信。

#### Acceptance Criteria

1. WHEN 公开页面加载，系统 SHALL 返回当天日期、学期、周次、星期与是否处于教学周。
2. WHEN 提供数据版本信息，系统 SHALL 返回当前版本标识、生成时间、字典与轴版本以及逐周发布状态。
3. WHEN 某周距最近一次成功抓取该周矩阵超过 8 天或最近一轮同步失败，系统 SHALL 将该周标记为过期并继续返回数据。
4. WHEN 轮次状态为「部分完成」，系统 SHALL 将该周视为未过期。
5. WHEN 返回教学楼列表，系统 SHALL 仅包含当前白名单条目。

### Requirement 18 查询边界与提示

**User Story:** AS 学生，I want 越界输入与空结果有明确提示，so that 不会把「无数据」误读成「都空闲」。

#### Acceptance Criteria

1. WHEN 查询日期超出今天至第 10 天，系统 SHALL 返回参数错误。
2. WHEN 节次区间起点晚于终点或超出 01–12，系统 SHALL 返回参数错误。
3. WHEN 查询区间内该周全部状态格为空，系统 SHALL 在页顶提示「该周疑似无排课」并照常返回结果。
4. WHEN 无当前版本、日期不属于任何学期周或该周无观测，系统 SHALL 返回空态并说明原因。
5. WHEN 教室名称重复，系统 SHALL 在名称后追加房号后缀。

### Requirement 19 前端默认节次

**User Story:** AS 学生，I want 打开页面就有合理的默认节次，so that 少点几次。

#### Acceptance Criteria

1. WHEN 用户在本地时间 12:00 前打开查询页，前端 SHALL 预填节次区间 01–05。
2. WHEN 用户在本地时间 12:00 至 18:00 打开查询页，前端 SHALL 预填节次区间 06–09。
3. WHEN 用户在本地时间 18:00 后打开查询页，前端 SHALL 预填节次区间 10–12。
4. WHEN 用户选择非当天日期，前端 SHALL 沿用当前时段的预填区间。
5. WHEN 服务端处理查询，系统 SHALL 依据请求中的节次区间判定，与请求发起时刻无关。

### Requirement 20 管理员初始化

**User Story:** AS 管理员，I want 首次部署后在页面里设密码并拿到密钥，so that 不依赖环境变量预设凭据。

#### Acceptance Criteria

1. WHEN 系统完成首次部署且不存在管理员记录，管理端 SHALL 仅提供初始化入口。
2. WHEN 管理员提交初始化表单，系统 SHALL 校验两次密码一致且长度不少于 12 字符，并以不可逆哈希保存。
3. WHEN 初始化完成，系统 SHALL 关闭初始化入口并生成业务秘密加密密钥。
4. WHEN 密钥生成完成，系统 SHALL 要求管理员导出一次密钥，并提供后续再次导出入口。
5. WHEN 已存在管理员记录，系统 SHALL 拒绝初始化请求。
6. WHEN 管理员提交改密，系统 SHALL 校验当前密码并使既有会话失效。
7. WHEN 运维清除管理员记录，系统 SHALL 允许重新初始化。

### Requirement 21 管理会话

**User Story:** AS 管理员，I want 登录态与写操作有防护，so that 面板不会被别人操作。

#### Acceptance Criteria

1. WHEN 管理员登录成功，系统 SHALL 建立 12 小时管理会话并下发 CSRF 令牌。
2. WHEN 同一来源每分钟登录失败达到 5 次，系统 SHALL 对该来源限流。
3. WHEN 写操作缺少有效会话或 CSRF 令牌，系统 SHALL 拒绝该请求。
4. WHILE 会话过期，前端 SHALL 跳转登录页并提示重新登录。

### Requirement 22 业务设置

**User Story:** AS 管理员，I want 配置账号、OCR、飞书与白名单，so that 采集按现状运行。

#### Acceptance Criteria

1. WHEN 管理员保存设置，系统 SHALL 校验白名单楼编号非空且唯一、执行时间表达式格式合法。
2. WHEN 管理员提交秘密字段，系统 SHALL 加密保存并仅返回「已设置」状态。
3. WHEN 管理员提交空串，系统 SHALL 清除对应秘密；省略字段时保持原值。
4. WHEN 白名单为空并启动同步，系统 SHALL 将轮次置为「阻止」并提示原因。

### Requirement 23 运行状态与手动同步

**User Story:** AS 管理员，I want 看到进度并随时手动跑一轮，so that 出问题能立刻重抓。

#### Acceptance Criteria

1. WHEN 管理员查询轮次，系统 SHALL 返回状态、归属冲突标记、当前阶段、必需与可选任务完成数、未知与复合计数、关联版本与最近错误。
2. WHEN 已有一轮「进行中」，系统 SHALL 拒绝手动同步并返回冲突提示。
3. WHEN 无轮次进行中，系统 SHALL 接受手动同步并返回轮次标识。

### Requirement 24 版本对比与回退

**User Story:** AS 管理员，I want 对比两版并一键回退，so that 错数据能马上换回去。

#### Acceptance Criteria

1. WHEN 管理员请求对比两个版本，系统 SHALL 返回房间增删、格子变化、未知与复合增减以及归属变化。
2. WHEN 管理员请求回退，系统 SHALL 在单个事务内交换当前与上一版本并保持数据不重建。
3. WHEN 回退完成，系统 SHALL 使公开查询立即读取新的当前版本。

### Requirement 25 异常数据视图

**User Story:** AS 管理员，I want 在一个页面看异常，so that 不必翻日志。

#### Acceptance Criteria

1. WHEN 管理员打开异常视图，系统 SHALL 列出未归属教室、归属冲突与未知/复合清单。
2. WHEN 呈现异常来源，系统 SHALL 以当前版本与最近轮次为数据来源。

### Requirement 26 体验要求

**User Story:** AS 校内使用者，I want 手机上一屏查完，so that 走在路上也能用。

#### Acceptance Criteria

1. WHILE 访问公开页，前端 SHALL 以移动端竖屏为首要布局，并将主操作放在单手可达区域。
2. WHILE 访问管理面板，前端 SHALL 以桌面宽屏为首要布局，并以表格与分页承载长列表。
3. WHEN 呈现状态与节次，前端 SHALL 使用数据版本中的字典与轴定义。
4. WHEN 公开页呈现数据，前端 SHALL 在页脚展示数据来源、更新频率与免责说明。
5. WHEN 公开页无结果，前端 SHALL 展示对应空态文案并给出可调整的筛选提示。

### Requirement 27 时间口径

**User Story:** AS 运维，I want 时间口径统一，so that 跨时区不会错一天。

#### Acceptance Criteria

1. WHEN 存储或传输时间戳，系统 SHALL 使用 UTC。
2. WHEN 解释业务日期，系统 SHALL 使用 `Asia/Shanghai` 日历日。
3. WHEN 展示时间，前端 SHALL 标注 `Asia/Shanghai`。
4. WHEN 换算周次与星期，服务端 SHALL 依据学期日历完成。

### Requirement 28 性能与容量

**User Story:** AS 运维，I want 数据量与响应时间可预期，so that 单机也撑得住。

#### Acceptance Criteria

1. WHEN 单校约 2000–3000 间教室，系统 SHALL 支持 4 周窗口约 60–100 万格数据/版本的写入，并保留两个版本。
2. WHEN 用户发起常规分页查询，系统 SHALL 在 300 毫秒内返回结果。
3. WHEN 发布写入数据，系统 SHALL 分批写入以保持事务时长可控。

### Requirement 29 可靠性与降级

**User Story:** AS 学生，I want 同步失败时还能查到旧数据，so that 服务不整段不可用。

#### Acceptance Criteria

1. WHEN 必需任务失败，系统 SHALL 继续以当前版本提供查询服务。
2. WHEN 发布事务失败，系统 SHALL 保持当前版本与上一版本可用。
3. WHEN 会话失效且重新登录失败，系统 SHALL 将该任务置为失败并保留已完成任务的结果。

### Requirement 30 安全与隐私

**User Story:** AS 管理员，I want 秘密不外泄、原始页面不落库，so that 系统可长期运行。

#### Acceptance Criteria

1. WHEN 存储业务秘密，系统 SHALL 使用部署内密钥加密。
2. WHEN 输出日志，系统 SHALL 排除登录中间量、密码、会话、验证码与页面正文。
3. WHEN 对外提供入口，系统 SHALL 启用 TLS 与基础安全头，并将数据存储限制在内网。
4. WHEN 提供写操作，系统 SHALL 仅通过管理端路由暴露。
5. WHEN 写入数据，系统 SHALL 排除原始页面、账号、会话、验证码、申请人与任课教师。
6. WHEN 公开查询返回内容，系统 SHALL 仅包含房名、房号与所属教学楼。

### Requirement 31 可观测性与运维

**User Story:** AS 运维，I want 日志与探针能定位到具体轮次，so that 排障不用猜。

#### Acceptance Criteria

1. WHEN 输出日志，系统 SHALL 使用结构化字段关联轮次、阶段、楼栋、周次与任务。
2. WHEN 探针请求到达，系统 SHALL 返回服务存活、依赖可用与结构变更完成状态。
3. WHEN 每日执行备份，系统 SHALL 生成全量备份并保留 7 天。

### Requirement 32 限流与缓存

**User Story:** AS 运维，I want 公开查询被脚本刷时不打垮数据库，so that 正常用户仍可用。

#### Acceptance Criteria

1. WHEN 同一来源的公开查询请求超过每分钟上限，系统 SHALL 拒绝该请求并提示稍后重试。
2. WHEN 相同查询重复到达，系统 SHALL 复用缓存结果，并保持新鲜度标记与空态与实时查询一致。
