# easy-qfnu-kjs 产品需求文档（重写基线）

- 版本：3.0
- 日期：2026-10-23
- 状态：定稿，作为重写的唯一基线
- 取代：`docs/product-requirements.md`、`docs/contract/*`、`specs/collector-full-sync/*` 中与本文冲突的内容
- 依据：Issue #1、Issue #2、旧实现探测记录、2026-10-23 教师账号只读探测

> 本文只写结论，不写备选方案。实现按本文执行；与旧文档冲突时，以本文为准。

## 0. 结论摘要

- 产品是**曲阜师范大学空教室公开查询**：公开页只读本地已发布数据，不实时请求教务。
- 采集走**强智教务教师端直登**（`/Logon.do?method=logonLdap`），不是 CAS；验证码交 OCR 服务识别。
- 每个白名单教学楼只拉**算上当前周共至多 4 周**（当前周及之后 3 周）的周矩阵；占用明细从这些周矩阵的并集中挑格子，一格一次。过去周不再采集。
- 矩阵解析使用 HTML 解析库按结构定位，不用正则抠 HTML。目标表是 `table#dataList`，真实页面里 `dataListHeader` 排在它前面。
- 空白格一律是 `free`。教务的“完全空闲”是按查询范围对整间教室的筛选结论，**不是格子状态**；不落逐格观测，查询侧用区间内每节 `available` 聚合表达。
- 观测按「房 × 周 × 天 × 节」逐格一行；发布用单事务切换 current/previous，失败不覆盖旧版。
- 发布门槛：**采集窗口内的周矩阵是必需任务**；占用明细是可选任务。必需任务失败不发布；仅明细失败时发布并标 `partial`。
- 公开查询 3 个只读端点；空教室 = 所选区间每一节都可用；新鲜度 8 天。
- 管理面板负责登录、配置、进度、手动同步、版本对比与一键回退；管理员密码由部署环境提供，业务秘密加密存库。
- 不存原始 HTML、申请人、任课教师；不提供学生课表、成绩、借教室、容量或多租户能力。

## 1. 产品定义

### 1.1 一句话

把教务系统教师端的教室占用矩阵清洗成本地逐格数据，向校内师生提供按日期、星期、节次区间和关键词检索空教室的只读服务，并给管理员一套配置、同步、对比与回退面板。

### 1.2 用户

| 角色 | 使用方式 | 权限 |
|---|---|---|
| 公开用户 | 浏览器访问公开页面 | 只读查询，不需要登录 |
| 管理员 | 浏览器访问 `/admin` | 登录后配置、手动同步、查看进度、对比与回退 |
| 运维 | 部署环境、日志与数据库 | 部署、迁移、备份、排障 |

### 1.3 范围

- 公开查询：空教室、全天 12 小节状态、当前日期上下文、数据版本与新鲜度。
- 采集：教师端登录、周历采样、白名单楼窗口周矩阵、占用明细、清洗、校验、发布。
- 管理：管理员登录、业务设置、同步进度、手动同步、版本对比、一键回退、告警记录。
- 部署：本机或内网部署；内网验收通过后再切公网域名。

### 1.4 非目标

- 不做学生课表、成绩、考试、培养方案等学生端功能。
- 不做教室借用申请、审批、申请人或任课教师信息。
- 不做容量、座位数、设备清单、教室类型筛选。
- 不做多学校、多租户、多语言。
- 不做原生 App；只做响应式 Web。
- 不把采集、手动同步或管理写接口暴露给公开用户。
- 不在公开查询请求里访问教务系统。

## 2. 已定上游事实

以下为 2026-10-23 用教师账号只读探测确认的事实，也是采集实现必须匹配的结构。详细字段见附录 A；请求/响应原文结构见 [docs/upstream-api.md](upstream-api.md)。

### 2.1 入口

- 根地址：`http://zhjw.qfnu.edu.cn`，部署级变量 `UPSTREAM_BASE_URL` 可覆盖。
- 登录页：`GET /`，标题“登录”，表单 `#loginForm` 提交到 `/Logon.do?method=logonLdap`。
- 验证码：`GET /verifycode.servlet`，80×40 JPEG。
- 学期页：`GET /jsxsd/kbxx/jsjy_query`，正文含 `YYYY-YYYY-N` 学期。
- 周历页：`POST /jsxsd/framework/main_index_loadkb.jsp`，表单 `rq=YYYY-MM-DD`；正文含“第 X 周 / Y 周”。
- 周矩阵：`POST /jsxsd/kbxx/jsjy_query2`。
- 占用明细：`GET /jsxsd/kbxx/jsjy_jszyqk`。
- 教学楼列表：`GET /jsxsd/kbxx/jsjy_processAjax?xqid=<1|2>&requestType=jxl`，JSON 数组 `[{dm,dmmc}]`。

### 2.2 登录

采用强智直登，不使用 CAS：

1. `GET /` 建立 `JSESSIONID`。
2. `GET /verifycode.servlet` 取验证码图片，交 OCR。
3. `POST /Logon.do?method=logon&flag=sess` 取 `scode#sxh`。
4. 构造 `code = username + "%%%" + password`；对前 20 个字符，依次追加该字符和 `scode` 中长度为 `sxh[i]` 的一段；剩余字符原样追加，得到 `encoded`。
5. `POST /Logon.do?method=logonLdap`，表单字段 `userAccount=`、`userPassword=` 留空，`RANDOMCODE=<验证码>`，`encoded=<上一步结果>`。
6. 成功判据：响应含“正在登录”、`location` 或“教学一体化服务平台”；随后 `GET /jsxsd/framework/jsMain.jsp` 必须含“教学一体化服务平台”。
7. 密码错误立即失败，不重试；验证码错误换新图重试，最多 3 次。

### 2.3 周历

- 采样日取 `Asia/Shanghai` 当天，格式 `YYYY-MM-DD`。
- 正文匹配 `第\s*(\d+)\s*周\s*/\s*共?\s*(\d+)\s*周`；实测写作“第5周 /16周”。
- `X` 必须满足 `1 ≤ X ≤ Y`；否则本轮失败，不写周历。
- 第 1 周周一 = 采样日所在周的周一 − 7×(X−1)；第 1…Y 周各写一行 `term_week`。周历仍覆盖全学期，供查询按日期定位；矩阵只采集窗口内的周。
- 采集窗口：当前周 `C` 到 `min(Y, C+3)`；算上当前周共 4 周。`C` 之前的周不请求、不入库。
- 非教学周或文本缺失 → 本轮失败；不猜周次，不发布。

### 2.4 矩阵

- 请求表单：`typewhere=jszq`、`xnxqh=<学期>`、`jxlbh=<楼编号>`、`jsmc_mh=`（留空）、`zc=zc2=<周次>`、`xq=1`、`xq2=7`。
- **不传** `jc`、`jc2`、`jszt`；不把楼名写进 `jsmc_mh`；不允许 `jxlbh` 为空。
- 一栋楼一周一次请求。周矩阵返回 7 天 × 5 大节 = 35 个状态格。
- 只请求窗口内的周（当前周及之后 3 周）；不请求聚合矩阵，也不请求过去周。占用明细从窗口内各周矩阵的并集挑格子：同一格在任一周不可用就下钻一次。
- 表头 `td[tdvalue]` 恰好 35 个，按天重复 5 个大节：`0102`、`030405`、`0607`、`0809`、`101112`。
- 真实页面在 `table#dataList` 之前还有一个 `table#dataListHeader`；解析必须按 id 精确选中 `dataList`。

### 2.5 占用明细

- `GET /jsxsd/kbxx/jsjy_jszyqk`，参数：`xnxqh`、`jsbh`、`kcsj=<星期><大节>`（例如周一 0102 = `10102`）、`xq=<星期>`、`typewhere=jszq`，其余起止参数留空，`type=add`，`kbjcmsid=<学期页里的节次模式 id>`。
- 弹窗里 `table#tab1` 只是标题表；字段行在下半部的嵌套表格里，每行三列「标签 / 空列 / 值」，标签带全角冒号；同一格可能有多条记录。
- 只取四个字段：`教室状态`、`课程`、`周次`、`时间标志`。其余字段（教室、时间、节次、备注、申请人、任课教师）丢弃。
- 一格的多个记录按行顺序全部保留。

### 2.6 “完全空闲”

- 矩阵单元格从不返回“完全空闲”文字；空白格就是 `free`。
- 教务的 `jszt=8`（完全空闲）是**按查询范围对整间教室的筛选**：所选周次、星期、节次范围内 35 格全空才返回该房间。
- 本地只需窗口内的格子；不需要存房间级状态；查询侧对所选区间做“每节都可用”聚合即可。
- 2026-10-23 探测用全学期聚合矩阵（**仅用于确认语义，不是采集范围**）：曲阜校区 26 栋楼中“完全空闲”出现 0 次；旧版 4 周快照里状态 8 出现 0 次；Q06 房间 0008 第 5 周全空但全学期不空，`jszt=8` 只在第 5 周返回它。

## 3. 运行边界

- 公开查询与采集/管理分离：公开页面和公开 API 只读已发布数据，不登录教务；采集、手动同步和管理写接口只对管理员开放，不挂公开路由。
- 部署在本机或内网，先完成内网验收，再切公网域名；对外入口启用 TLS，数据库不直接暴露公网。
- 进程数量、服务拆分、技术栈、容器编排与任务编排由接手开发者决定；PRD 不规定。
- 业务时间口径：时间戳按 UTC 存储与传输，业务日期与 cron 按 `Asia/Shanghai`；详见第 9 节。

## 4. 数据模型

数据以本地数据库为准；时间戳按 UTC 存储与传输，业务日期按 `Asia/Shanghai` 解释。参考结构见附录 B，不强制技术选型。

### 4.1 表

| 表 | 职责 |
|---|---|
| `dict` / `dict_state` / `dict_symbol` | 状态字典与符号分类，版本化 |
| `axis` / `axis_node` | 12 小节 ↔ 5 大节映射，版本化 |
| `term` / `term_week` | 学期、总周数、每周一日期 |
| `room` | 房间身份、规范化名、原始名、所属教学楼、首见/最近 |
| `settings` | 业务设置；秘密值加密 |
| `release` | 发布版本：学期、时间、字典/轴版本、是否 current；`release_id` 为 UTC `YYYYMMDDTHHMMSS.ffffffZ` |
| `observation` | 观测事实：房 × 周 × 天 × 节 → 状态 |
| `occupancy_detail` | 占用说明：状态、课程、周次范围、时间标志、派生标记 |
| `collect_run` | 一轮采集：状态、窗口起止与周数、必需/可选任务计数、汇总计数、发布版本 |
| `collect_run_week` | 每轮每楼每周的周矩阵抓取结果与页面哈希 |
| `sync_task` | 断点续传任务：窗口周矩阵、占用明细 |
| `alert_log` | 告警去重 |
| `schema_migration` | 迁移版本记录 |

### 4.2 不变量

- `observation` 主键 `(release_id, room_id, term, week, weekday, node)`；一间房 × 一周 × 一天 × 一节恰一行。
- `release` 任一时刻至多一个 `is_current=true`；只保留 current 与 previous 两版。
- `dict_symbol` 不登记“完全空闲”；空单元格由清洗层判成 `free`。
- `room.room_id` 是 `jsbh`；同名不同 `jsbh` 不合并。
- 同一 `jsbh` 在一轮里归属两栋楼 → 整轮停止，不发布。
- `settings.account_password`、`feishu_webhook_url`、`feishu_secret` 加密存储；API 只返回“已设置”，不返回明文。
- 不存原始 HTML、账号、Cookie、Token、验证码、申请人、任课教师。

### 4.3 保留

- `release`：current + previous；更早的级联删除。
- `collect_run`：保留 365 天；任务、周记录、告警随轮次保留。
- `alert_log`：保留 365 天。
- 每日备份 `pg_dump`，保留 7 天。

## 5. 采集

### 5.1 登录与会话

- 每轮开始时先写一条 `collect_run(status=running, term=NULL)`；登录、学期或周历失败也写 `failed` 并带 `error_code`，面板能看到最近一次失败。
- 账号、密码、OCR 地址从 `settings` 读取；密码解密后只在内存使用。
- 验证码识别：图片转 base64，`POST {ocr_base_url}/ocr`，表单字段 `image`；响应 `{"code":200,"message":"Success","data":"<文本>"}`，以 `code=200` 判成功。
- 登录流程按 2.2；验证码错误换图重试最多 3 次；密码错误立即失败。
- 会话 Cookie 只存内存；请求带 `Cookie`；响应含“用户登录”或“非法访问”按会话失效处理。
- 会话失效：重新登录一次，再从当前任务继续；重登录失败则当前任务失败。

### 5.2 周历

- 每次同步开始读学期，再采样周历；`term` 与 `term_week` 在一个事务里整体替换当前学期，不留半截。
- 采样失败或文本不合法 → 本轮失败，不建任务、不请求矩阵、不发布。

### 5.3 任务

一轮采集的任务清单：

1. 每栋白名单楼 × 窗口内每一周（`C…min(Y,C+3)`）→ `week_matrix`。
2. 窗口内各周矩阵解析后，取所有“任一周不可用”的格子并集 → `cell_detail`（一格一条，周次为空）。

任务写入 `sync_task`：

- `required`：窗口内 `week_matrix` 全部为 true；`cell_detail` 为 false。
- `status`：`pending` / `done` / `failed`。
- 唯一性：同一轮同一楼同一周、同一楼同一格各至多一条。
- 断点续传从第一个 `pending` 继续；`done` 不重打；`failed` 不在同一轮重试。
- 启动时把 `running` 超过 2 小时的轮次标 `failed`（`error_code=stale_run`），避免崩溃后永久锁死；同一时刻仍至多一轮。
- `collect_run.weeks_total` 记窗口周数（`window_end-window_start+1`）；`required_total` = 楼数 × 窗口周数；`window_start/end` 落库供面板显示。

### 5.4 请求约束

- 相邻上游请求至少间隔 500ms（每秒至多 2 次），所有请求共用同一节奏。
- 单次请求最多 3 次重试（1 次原始 + 3 次重试）；非 200、网络错误、读取失败都计一次。
- 会话失效不重试，直接重新登录；密码错误不重试。
- 明细请求量大，必须可断点；不设 15 分钟整轮上限。

### 5.5 发布门槛

| 任务 | 失败后的处理 |
|---|---|
| 窗口内 `week_matrix` | 本轮失败，不发布；保留原 current；告警 |
| `cell_detail` | 记录失败；该格无课程说明；本轮可发布，标 `partial` |
| 表头/轴变化、重复 jsbh、同房两楼 | 整轮停止，不发布；告警 |

- 发布条件：`required_total > 0` 且 `required_done == required_total`，候选集非空，校验通过。
- 窗口为空（异常兜底）→ 本轮 `skipped`，不发布。
- 全部任务完成 → `success`；仅明细失败 → `partial`；必需周矩阵失败 → `failed`；白名单为空 → `blocked`。

### 5.6 周矩阵清洗

使用 HTML 解析库解析 `table#dataList`：

1. 选 `table#dataList`；没有 → 结构失败。
2. 找第一个含 `td[tdvalue]` 的行作为表头；没有 → 结构失败。
3. 表头必须有 35 个 `tdvalue`，按 5 大节循环；个数/顺序/集合不对 → `ErrAxisChanged`，整轮停止。
4. 数据行必须有 36 个 `td`（1 个房间列 + 35 个状态格）；否则结构失败。
5. `jsbh` 取值顺序：`tr[jsbh]` → 行内 `input[type=checkbox]` 的 `value` → 首列第一个 `input[value]`；都取不到 → 结构失败。
6. 同一响应内重复 `jsbh` → 结构失败。
7. 房名：NFKC → 反复删除 `(n/N)` / `（n/N）` → 压缩空白；保留方位/校区括号；保留 `name_raw`。
8. 状态：整词命中 `dict_symbol` → 语义键；空文本 → `free`；多个已知符号组合 → `composite`（留原文）；未收录文本 → `unknown`（留原文）。
9. 展开：5 大节 → 12 小节；轴上每个小节按 `block` 取对应大节格；同一大节内各节点状态由构造保证一致。
10. 表头/轴不一致 → `ErrAxisChanged`，不自动适配；旧数据靠 `axis_version` 解释。

房间目录在采集过程中按 `jsbh` Upsert，不参与发布事务；所属楼取自返回该房间的那次 `jxlbh` 请求。失败的一轮不抹掉已收下的房名，也不改已有归属。同一 `jsbh` 在本轮出现两栋楼 → 整轮停止。

### 5.7 占用明细

- 窗口内各周矩阵解析完成后，按“房 × 星期 × 大节”取并集：任一周不可用（占用、未知、复合）就下钻一次；所有窗口周都空闲的格子不下钻。
- 每个格子一条 `cell_detail` 任务；同一格一轮只下钻一次，不按周重复。
- 明细解析不按表格 id 取字段：扫描整页 `tr`，只认三列（标签 / 空列 / 值）且标签去掉冒号后属于四个字段的行；`table#tab1` 只作标题与页面校验。只保留 `教室状态`、`课程`、`周次`、`时间标志`。
- `教室状态` 用 `dict_state.label` 换语义键；标签未收录 → `unknown`。
- 明细的周次范围先与采集窗口求交：只保留与窗口相交的记录；`occupancy_detail.week_range` 存相交后的范围，窗口外原文与记录不落库。没有交集或范围解析失败的记录丢弃，不猜。
- 明细写入 `occupancy_detail`，`derived` 表示该说明是否给“窗口内没有该房间周矩阵行的周”补过状态。

### 5.8 派生

- 只在采集窗口内派生；窗口外的周不写观测。
- 有该房间该周矩阵行 → 矩阵说了算，明细只作说明。
- 窗口内没有该房间该周矩阵行 → 才允许用明细的周次范围（与窗口相交的部分）补状态，并标 `derived=true`。
- 范围解析失败、时间标志不认识、与窗口无交集 → 不派生；不得当成空闲。
- 派生状态同样写 `observation`；`available` 按字典投影。

### 5.9 发布

1. 按周展开观测；按批写入。
2. 单事务：写 `release`（非 current）→ 写 `observation` → 写 `occupancy_detail` → 降原 current → 抬新 current → 删除 current/previous 之外的版本。
3. 任意一步失败整体回滚；原 current 不动。
4. 发布后写 `collect_run` 汇总（窗口起止、房间数、未知/复合计数、release_id），标 `success` 或 `partial`。
5. 校验：候选集为空 → 拒绝；同房两楼 → 拒绝；必需任务未完成 → 拒绝；未知/复合只计数，不阻断。
6. `unknown_cells`/`composite_cells` 按大节格计数（房 × 周 × 天 × 大节），同一大节展开出的 2–3 个小节不重复计数。

### 5.10 调度与告警

- cron 用 6 段式（含秒），默认 `0 10 4 * * 6`（每周六 04:10，`Asia/Shanghai`）；管理员可在面板修改。
- 手动同步与定时跑同一套窗口采集；同一时刻至多一轮。
- 告警场合：必需任务失败/整轮停止、发布校验拦截、学期切换、连续失败后的首次恢复。
- 告警去重键：`final_failure:<run_id>`、`validation_blocked:<run_id>`、`term_changed:<term>`、`recovered:<run_id>`。
- 告警内容不含账号、密码、Cookie、Token、验证码、原始页面、申请人、任课教师。
- 飞书加签：`sign = base64(HMAC-SHA256(key=secret, data=timestamp+"\n"+secret))`，请求体带 `timestamp` 与 `sign`。
- `partial` 不告警；面板显示状态与最近错误。

## 6. 查询 API

统一响应：`{code, message, data, request_id}`；`request_id` 前缀 `req_`，用于日志定位。所有时间为 RFC3339 UTC，业务日期为 `YYYY-MM-DD`（`Asia/Shanghai`）。

### 6.1 端点

| 路径 | 参数 | data |
|---|---|---|
| `GET /api/context` | — | `{date, term, week, weekday, in_calendar}` |
| `GET /api/meta` | — | 版本、字典、轴、学期与逐周新鲜度 |
| `GET /api/query` | `view`、`keyword`、`date_offset`、`start_node`、`end_node`、`limit`、`offset` | `{total, items, date, week, day_of_week}` |

### 6.2 context

- `date` 是 `Asia/Shanghai` 今天；`weekday` 1=周一…7=周日。
- 有 `term_week` 覆盖 → `term/week/in_calendar=true`；否则 `term=""`、`week=0`、`in_calendar=false`。
- 无 release 也返回 200。

### 6.3 meta

```json
{
  "release_id": "20261023T071012.123456Z",
  "generated_at": "2026-10-23T07:10:12Z",
  "dict_version": 1,
  "axis_version": 1,
  "states": {
    "free":       { "label": "空闲", "available": true },
    "fully_free": { "label": "完全空闲", "available": true },
    "class":      { "label": "正常上课", "available": false }
  },
  "axis": {
    "blocks": ["0102", "030405", "0607", "0809", "101112"],
    "nodes": [{ "code": "01", "block": "0102" }]
  },
  "terms": [{
    "term": "2026-2027-1",
    "total_weeks": 16,
    "weeks": [{
      "week": 5, "monday": "2026-10-19", "published": true,
      "last_success_at": "2026-10-23T07:10:12Z", "stale": false
    }]
  }]
}
```

- `states.fully_free` 只作查询/展示标签；采集不会产生该格子状态。
- `published`：该周在 current release 有观测。
- `last_success_at`：发布该 release 的 run 里，该周周矩阵最近一次成功的 `fetched_at`；没有则用 `generated_at`。
- `stale`：距 `last_success_at` 超过 8 天，或最近一轮状态为 `failed`/`blocked`。
- 无 current release → `40401`。

### 6.4 query

参数：

- `view`：`availability`（默认）或 `day`。
- `keyword`：房间名子串；NFKC + trim；ASCII 不区分大小写；最长 32；转义 `%`、`_`、`\`；不做正则。
- `date_offset`：0..10，默认 0。
- `start_node` / `end_node`：仅 availability；当前轴上的 `01`–`12`；默认 `01`/`11`；要求 `start_node ≤ end_node`。
- `limit`：1..200，默认 50；`offset` ≥ 0，默认 0。
- 参数越界、`view` 非法、`start_node > end_node`、`limit`/`offset` 越界 → `40001`。
- `day` 忽略 `start_node`/`end_node`，不报错。

判定：

- availability：解析日期到 `(term, week, weekday)`；该房间在区间内**每一节**都有 `available=true` 才返回。
- day：返回该房间该日已存在的 12 小节状态；缺失节点不出现在 `statuses` 中。
- 排序：`name` 升序，再 `id` 升序；分页按房间。
- 无 current release、日期不在任何学期周、或该周无观测 → `40401`。
- current release 只含采集窗口内的周；窗口外的日期（含过去周）没有观测，查询返回 `40401`。
- `in_calendar=false` 不改变查询；没有对应数据时仍按 `40401`。

响应：

```json
{
  "code": 0,
  "message": "success",
  "data": {
    "total": 12,
    "items": [{ "id": "0899", "name": "F126" }],
    "date": "2026-10-23", "week": 5, "day_of_week": 5
  },
  "request_id": "req_abc123"
}
```

### 6.5 错误码

| code | 含义 |
|---|---|
| `0` | 成功 |
| `40001` | 参数错误 |
| `40101` | 未登录（仅管理） |
| `40301` | CSRF/权限拒绝（仅管理） |
| `40401` | 无数据 |
| `40901` | 已有一轮同步（仅管理） |
| `42201` | 设置校验失败（仅管理） |
| `50000` | 内部错误 |

## 7. 管理接口与面板

### 7.1 登录

- 用户名固定 `admin`；密码由部署环境提供，为空或少于 12 字符不得启动。
- `POST /api/admin/session`，JSON `{username, password}`；成功设置：
  - 管理会话：HttpOnly 签名 Cookie，12 小时，`SameSite=Lax`，HTTPS 时 `Secure`。
  - CSRF token：可读，前端在写请求头回传。
- 登录失败 5 次/分钟/IP 限流；错误不区分用户名/密码。
- `DELETE /api/admin/session` 清 cookie；`GET /api/admin/session` 返回 `{authenticated, username}`。
- 会话签名密钥由部署环境提供，必须 ≥32 字节；缺失不得启动。

### 7.2 设置

`GET /api/admin/settings` 返回：

```json
{
  "account_username": "teacher_account",
  "account_password": { "set": true },
  "feishu_webhook_url": { "set": true },
  "feishu_secret": { "set": true },
  "ocr_base_url": "http://ocr.example:8000",
  "cron_expr": "0 10 4 * * 6",
  "next_run_at": "2026-10-23T20:10:00Z",
  "building_whitelist": [{ "jxlbh": "Q10", "name": "综合教学楼" }]
}
```

`PUT /api/admin/settings` 接受部分字段：

- 字符串秘密：提供则替换；`null`/省略不变；空串清除。
- `building_whitelist`：数组，`jxlbh` 非空且唯一，`name` 非空；空数组允许保存，但下一轮 `blocked`。
- 白名单由管理员填写 `jxlbh` 与展示名；系统不自动发现楼。`jxlbh` 即附录 A.2 中 `jsjy_processAjax` 返回的 `dm`。
- `cron_expr`：6 段式；保存前解析校验；时区固定 `Asia/Shanghai`。
- 校验失败 → `42201`，整次不保存。
- 秘密值加密存储；加密密钥由部署环境提供，缺失不得启动；解密只在采集/告警内存中进行。

### 7.3 运行状态

- `GET /api/admin/runs?limit=20`：最近轮次摘要。
- `GET /api/admin/runs/{run_id}`：状态、阶段、必需/可选完成数、未知/复合计数、release_id、最近错误、更新时间。
- 阶段：`login`、`calendar`、`week_matrix`、`cell_detail`、`validate`、`publish`、`finished`。
- 面板显示运行进度，刷新方式由实现决定。

### 7.4 手动同步

- `POST /api/admin/sync`：启动同一套窗口采集；返回 `202 {run_id}`。
- 已有一轮 → `40901`；未登录 → `40101`；CSRF 不符 → `40301`。
- 白名单为空时允许启动，轮次立即 `blocked` 并在面板提示。

### 7.5 版本对比与回退

- `GET /api/admin/releases`：current、previous 及时间。
- `GET /api/admin/releases/diff?from=<id>&to=<id>&limit=100&offset=0`：
  - 汇总：房间新增/消失/共有、变更房间数、变更格数、unknown/composite 增减。
  - 明细分页：`{room_id, name, week, weekday, node, from_state, to_state}`。
- `POST /api/admin/releases/{release_id}/rollback`：只允许 current 或 previous；单事务交换 `is_current`；返回新的 current。查询立即生效，不重建数据。
- 回退不删除旧数据；仍保留两版。

### 7.6 面板页面

- `/admin/login`：登录；错误提示；登录后跳转。
- `/admin`：当前版本、最近一轮、过期周、手动同步。
- `/admin/settings`：账号、密码、飞书、OCR、cron、白名单；秘密只写不读。
- `/admin/runs`、`/admin/runs/:id`：列表与进度。
- `/admin/releases`：两版对比、回退确认。
- `/admin/alerts`：告警记录。
- 所有写操作带 CSRF；未登录跳登录页；会话过期提示重登录。

## 8. 前端

### 8.1 技术

Web 前端；桌面优先、移动端可用；品牌色 `#884F22`；状态颜色与图标只在前端映射。

### 8.2 页面

- `/`：首页 + 查询入口。
- `/availability`：空教室。
- `/day`：全天状态。
- `*`：404。
- `/admin/*`：管理页，独立布局与鉴权。

### 8.3 交互

- 打开请求 `context` 与 `meta`；状态名、节次从 `meta` 取，不硬编码。
- 日期选择“今天/明天/…/+10 天”，对应 `date_offset`；明确查询按钮，回车同查。
- availability：关键词、日期、起止节次；结果只显示房间名与 id。
- day：关键词、日期；12 小节网格；缺失节点显示“无数据”。
- 空态：无结果、该日无数据、数据过期、当前不在教学周。
- 同名房间在名称后追加缩短的 `id`；其余只显示名称。
- 数据过期显示 `stale` 提示；不隐藏旧数据。

### 8.4 质量门禁

- 前端 lint、类型检查、构建、单元测试必须通过。
- 周次/日期换算、房名展示、状态视觉映射等纯函数配单元测试。

## 9. 非功能

### 9.1 时间

- 时间戳存 UTC；API 时间 RFC3339 UTC（`Z`）。
- 业务日期 `monday`、`date` 是 `Asia/Shanghai` 的日历日，纯 `date`，不做时区换算。
- cron 按 `Asia/Shanghai` 解释；界面显示时标注 `Asia/Shanghai`。
- 周次/星期由 `term_week` 解析，不在前端重算。

### 9.2 容量与性能

- 单校约 2–3k 房间；窗口至多 4 周，约 0.6–1.0M 观测行/版本；保留两版。
- 请求量：楼数×窗口周数次周矩阵（26 栋×4 周约 104 次，约 52 秒）+ 窗口并集明细；明细是 8.5k 全学期有内容格的子集，具体取决于窗口内占用分布。
- 不设整轮 15 分钟上限；必须断点续传。
- 空教室查询 p95 < 300ms（50 条/页，单周单日）。
- 发布按批写入，单批 ≤ 5 万行。

### 9.3 可靠性

- 同一时刻至多一轮。
- 必需任务失败不发布，保留旧 current；可选失败发布 `partial`。
- 发布事务失败整体回滚。
- 会话失效重登录一次；重试耗尽只影响当前任务（必需则整轮失败）。
- 非教学周/周历采样失败：不发布，保留旧数据。

### 9.4 安全与隐私

- 管理员密码只由部署环境提供，不落库；业务秘密加密存库；API/日志不返回明文。
- 加密密钥必须与数据库备份一起保管；密钥丢失后已存秘密无法解密，只能重新录入。
- Cookie：HttpOnly、SameSite=Lax、HTTPS 时 Secure；CSRF token；登录限流。
- 数据库只绑回环；管理接口不暴露给公开用户；采集/手动同步不挂公开路由。
- 不存原始 HTML、账号、Cookie、Token、验证码、申请人、任课教师。
- 日志脱敏：不打印 `encoded`、密码、Cookie、页面正文、`Authorization`。
- 对外入口启用 TLS/HSTS 与基础安全头。

### 9.5 可观测性

- 结构化 JSON 日志：`request_id`、`run_id`、`task_id`、`phase`、`building_id`、`week`、`kind`、`duration_ms`、`error_code`。
- 提供存活与就绪检查：服务存活、数据库可用、结构变更已完成。
- 运行状态、任务、周抓取、告警都在库内，面板与运维共用。
- 不引入独立 metrics 栈；需要时再评估。

### 9.6 运维

- 按部署环境的方式启动服务。
- 结构变更在服务启动前完成；失败不得启动。
- 每日 `pg_dump` 到宿主机，保留 7 天；每季度做一次恢复演练。
- 日志按大小轮转，不写无限增长文件。
- 回滚：面板一键交换 current/previous；数据库备份用于灾难恢复。
- 首次部署顺序：完成结构变更 → 启动服务与 OCR → 管理员登录 → 配置账号、密码、OCR、白名单 → 手动同步一次 → 检查 current 与查询。

## 10. 验收标准

### 10.1 解析

- Given 真实结构（`dataListHeader` 在 `dataList` 前、表头 36 td/35 tdvalue、数据行 36 td），Then 解析出房间与 35 格，不选中 `dataListHeader`。
- Given `jsbh` 在 `tr`、checkbox、首列 input 三种位置，Then 按顺序取到；都缺失 → 结构失败。
- Given 表头块集合/顺序变化，Then `ErrAxisChanged`，整轮停止，不发布。
- Given 未知文本、复合符号，Then 分别为 `unknown`/`composite`，保留原文，不阻断发布。
- Given “完全空闲”文本，Then 按 `unknown` 处理（字典不登记它）。
- Given 房名 `F126(90/0)`、`篮球馆（东）(25/0)`，Then 规范化为 `F126`、`篮球馆(东)`，保留 `name_raw`。
- Given 真实明细弹窗（`table#tab1` 只有标题、字段行三列带全角冒号），Then 解析出教室状态、课程、周次、时间标志；申请人、任课教师、备注丢弃。

### 10.2 采集

- Given 教师账号、OCR 可用，Then 登录成功；验证码错误换图重试 ≤3；密码错误立即失败。
- Given 采样日与“第 X 周 / Y 周”，Then 生成 Y 条周一；非法 → 不写周历、不发布。
- Given 当前周为 `C`、总周数为 `Y`，Then 只请求 `C…min(Y,C+3)`，算上当前周共 4 周；不请求 `C-1` 及更早。
- Given 窗口为空（异常兜底），Then 本轮 `skipped`，不发布。
- Given 白名单为空，Then 轮次 `blocked`，不发任何矩阵请求。
- Given 窗口内任一必需周矩阵失败，Then 整轮 `failed`，不发布，保留 current。
- Given 仅明细失败，Then 发布 `partial`，可用性不受影响，明细缺数据。
- Given 会话失效，Then 重登录后从当前任务继续；已完成任务不重打。
- Given 请求密集，Then 相邻请求 ≥500ms，单任务最多 3 次重试。

### 10.3 发布

- Given 必需任务完成、校验通过，Then 单事务写入新 release；新 current、旧 previous、更早删除。
- Given 发布事务失败，Then 原 current 不变。
- Given 同一 jsbh 出现在两栋楼，Then 整轮停止，不发布。
- Given 未知/复合，Then 计数增长，发布继续。

### 10.4 查询

- Given `/api/query?view=availability&date_offset=0&start_node=01&end_node=11`，Then 只返回区间内每节都可用的房间与 `total`。
- Given `view=day`，Then 返回该日 12 小节状态；缺失节点不出现在 `statuses`。
- Given 关键词 `F12`，Then 只按房名子串匹配，`%`/`_` 按字面处理。
- Given 无 current release 或该周无数据，Then `40401`。
- Given 数据超过 8 天或最近一轮失败，Then `meta` 对应周 `stale=true`，查询仍返回旧数据。
- Given 不在教学周，Then `context.in_calendar=false`；有该周数据则照常返回。

### 10.5 管理

- Given 正确的部署环境管理员密码，Then 登录成功并设置会话/CSRF；错误密码限流。
- Given 修改设置，Then 下一轮采集/查询从库读取新值；秘密不回显。
- Given 已有一轮，Then 手动同步返回 `40901`。
- Given 两版数据，Then 对比返回汇总与分页明细；回退只交换 `is_current`，查询立即生效。
- Given 未登录/无 CSRF，Then 写接口 `40101`/`40301`。

### 10.6 部署

- Given 完成部署与结构变更，Then 数据库、服务与 OCR 健康。
- Given 内网验收，Then 连续两周同步成功、查询/回退/告警演练通过，才切公网域名。
- Given 质量门禁，Then 后端与前端静态检查、测试、构建全部通过。

## 11. 交付顺序

| 阶段 | 内容 | 退出条件 |
|---|---|---|
| M0 骨架 | 仓库、数据契约、迁移方案、PRD | 质量门禁通过；结构变更成功 |
| M1 采集 | 登录/OCR、周历、HTML 矩阵解析、明细、发布 | 真账号跑通一轮并发布；解析测试通过 |
| M2 查询 | 3 个公开端点、新鲜度 | 契约测试与手工查询通过 |
| M3 管理 | 登录、设置、进度、手动同步、对比/回退 | 管理端到端通过 |
| M4 前端 | 公开页 + 管理页 | lint/typecheck/build/test 通过；移动端可用 |
| M5 验收 | 内网部署、连续两周同步、回退演练 | 验收记录归档后才切公网 |

重写从空目录开始；旧 Go 代码只作行为参考，不再以它为基础修改。旧代码保留在 git 历史。

## 附录 A：上游事实与解析细节

请求/响应结构、表单字段与脱敏片段见 [docs/upstream-api.md](upstream-api.md)；本附录只列重写必须遵守的结论。

### A.1 登录

- 登录页 `GET /`；表单字段 `userAccount`、`userPassword`、`RANDOMCODE`、`encoded`。
- `code = userAccount + "%%%" + userPassword`；`encoded` 的插入规则见 2.2。
- 验证码图片为 JPEG；OCR 响应以 `code=200` 为成功。
- 登录成功标识：`正在登录`、`location`、`教学一体化服务平台`。
- 密码错误标记：`密码错误`、`用户名或密码错误`、`您提供的用户名或者密码有误`。
- 验证码错误标记：`验证码错误`、`验证码不正确`。
- 会话失效标记：`用户登录`、`非法访问`。

### A.2 页面与选择器

| 用途 | 选择器/字段 | 说明 |
|---|---|---|
| 学期 | `\d{4}-\d{4}-\d+` | 学期页正文 |
| 周次 | `第\s*(\d+)\s*周\s*/\s*共?\s*(\d+)\s*周` | 周历页正文 |
| 节次模式 | `input#kbjcmsid` 的 `value` | 明细请求用 |
| 楼列表 | `jsjy_processAjax` JSON | `dm`=`jxlbh`，`dmmc`=展示名 |
| 周矩阵 | `table#dataList` | 表头 `td[tdvalue]`，数据行 36 `td` |
| 明细 | 整页 `tr` 的三列行 | `table#tab1` 只作标题；字段行标签带全角冒号 |

### A.3 状态

| 原文 | 语义键 | 可用 |
|---|---|---|
| 空单元格 | `free` | 是 |
| `空闲` | `free` | 是 |
| `◆` | `class` | 否 |
| `Ｊ` | `borrowed` | 否 |
| `Ｘ` | `locked` | 否 |
| `Κ` | `exam` | 否 |
| `Ｇ` | `fixed_reschedule` | 否 |
| `Ｌ` | `temp_reschedule` | 否 |
| `M` | `cross_mode` | 否 |
| 多个已知符号 | `composite` | 否 |
| 未收录文本 | `unknown` | 否 |

- `Ｊ`/`Ｘ`/`Ｇ`/`Ｌ` 是全角拉丁字母，`Κ` 是希腊大写 Kappa；码点不得规范化成 ASCII。
- `完全空闲` 不是格子符号；`dict_symbol` 不登记它。

### A.4 实测数据

- 2026-2027-1，第 5 周，共 16 周；曲阜校区 26 栋教学楼，日照校区 11 栋。
- 以下全学期数字只用于确认状态与“完全空闲”语义，不是采集范围；采集窗口现在是“当前周 + 3 周（共 4 周）”。
- Q10 综合教学楼第 5 周：30 行、35 格；`◆` 345、`Ｊ` 114、空白 591。
- 26 栋楼全学期聚合矩阵：空白 36,474、`◆` 2,706、`Ｊ` 974，复合写法 50+；`完全空闲` 0。
- 旧版 2025-2026-3 四周快照：状态 8 出现 0 次，状态 5 每周约 1.1–1.2 万格。
- `jszt=8` 实测：Q03 第 5 周 53 间中 49 间整周全空；Q10 第 5 周 0 间；Q06 房间 0008 第 5 周全空但全学期不空。
- 真实明细弹窗实测：`table#tab1` 只有“教室占用情况”标题；字段行在下方嵌套表格里，标签写作 `教室状态：`、`课程：`、`周次：`、`时间标志：`。解析结果为 `教室状态=正常上课、课程=概率论与数理统计、周次=1-16、时间标志=单双周`。

## 附录 B：参考数据结构与种子

> 以下只说明字段、约束与种子值，不强制数据库产品或实现技术；满足第 4 节的不变量即可。

```sql
-- 字典与轴
CREATE TABLE dict (
  version    integer PRIMARY KEY,
  note       text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE dict_state (
  dict_version integer NOT NULL REFERENCES dict(version),
  state_key    text    NOT NULL,
  label        text    NOT NULL,
  available    boolean NOT NULL,
  PRIMARY KEY (dict_version, state_key)
);

CREATE TABLE dict_symbol (
  dict_version integer NOT NULL,
  symbol       text    NOT NULL,
  state_key    text    NOT NULL,
  PRIMARY KEY (dict_version, symbol),
  FOREIGN KEY (dict_version, state_key)
    REFERENCES dict_state(dict_version, state_key)
);

CREATE TABLE axis (
  version integer PRIMARY KEY,
  note    text
);

CREATE TABLE axis_node (
  axis_version  integer  NOT NULL REFERENCES axis(version),
  node          char(2)  NOT NULL,
  ordinal       smallint NOT NULL,
  block         text     NOT NULL,
  block_ordinal smallint NOT NULL,
  PRIMARY KEY (axis_version, node),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

-- 学期与日历
CREATE TABLE term (
  term        text PRIMARY KEY,
  total_weeks smallint NOT NULL,
  timezone    text NOT NULL DEFAULT 'Asia/Shanghai'
);

CREATE TABLE term_week (
  term        text NOT NULL REFERENCES term(term) ON DELETE CASCADE,
  week        smallint NOT NULL,
  monday      date NOT NULL,
  in_calendar boolean NOT NULL DEFAULT true,
  PRIMARY KEY (term, week)
);

-- 房间
CREATE TABLE room (
  room_id       text PRIMARY KEY,
  name          text NOT NULL,
  name_raw      text,
  building_id   text,
  building_name text,
  first_seen    date,
  last_seen     date
);

-- 设置
CREATE TABLE settings (
  key        text PRIMARY KEY,
  value      text NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- 发布
CREATE TABLE release (
  release_id   text PRIMARY KEY,
  term         text NOT NULL REFERENCES term(term),
  generated_at timestamptz NOT NULL,
  dict_version integer NOT NULL REFERENCES dict(version),
  axis_version integer NOT NULL REFERENCES axis(version),
  is_current   boolean NOT NULL DEFAULT false
);

CREATE UNIQUE INDEX release_one_current
  ON release ((true)) WHERE is_current;

-- 观测
CREATE TABLE observation (
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  term       text     NOT NULL REFERENCES term(term),
  week       smallint NOT NULL,
  weekday    smallint NOT NULL,
  node       char(2)  NOT NULL,
  state_key  text     NOT NULL,
  available  boolean  NOT NULL,
  raw_text   text,
  PRIMARY KEY (release_id, room_id, term, week, weekday, node),
  CHECK (weekday BETWEEN 1 AND 7),
  CHECK (node ~ '^(0[1-9]|1[0-2])$')
);

CREATE INDEX observation_free
  ON observation (release_id, term, week, weekday, node, room_id)
  WHERE available;

-- 占用明细
CREATE TABLE occupancy_detail (
  detail_id  bigserial PRIMARY KEY,
  release_id text     NOT NULL REFERENCES release(release_id) ON DELETE CASCADE,
  room_id    text     NOT NULL REFERENCES room(room_id),
  weekday    smallint NOT NULL,
  block      text     NOT NULL,
  state_key  text     NOT NULL,
  course     text,                -- 课程/考试科目原文
  week_range text,                -- 与采集窗口相交后的周次范围
  time_flag  text,                -- 单双周原文
  derived    boolean  NOT NULL DEFAULT false,
  CHECK (weekday BETWEEN 1 AND 7)
);

CREATE INDEX occupancy_detail_cell
  ON occupancy_detail (release_id, room_id, weekday, block);

-- 采集轮次
CREATE TABLE collect_run (
  run_id          bigserial PRIMARY KEY,
  started_at      timestamptz NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  finished_at     timestamptz,
  status          text NOT NULL,
  phase           text NOT NULL DEFAULT 'login',
  term            text REFERENCES term(term),
  window_start    smallint,
  window_end      smallint,
  weeks_total     integer NOT NULL DEFAULT 0,
  rooms_total     integer NOT NULL DEFAULT 0,
  required_total  integer NOT NULL DEFAULT 0,
  required_done   integer NOT NULL DEFAULT 0,
  optional_total  integer NOT NULL DEFAULT 0,
  optional_done   integer NOT NULL DEFAULT 0,
  unknown_cells   integer NOT NULL DEFAULT 0,
  composite_cells integer NOT NULL DEFAULT 0,
  release_id      text REFERENCES release(release_id) ON DELETE SET NULL,
  error_code      text,
  error_message   text,
  CHECK (status IN ('running','success','partial','failed','blocked','skipped')),
  CHECK (phase IN ('login','calendar','week_matrix','cell_detail','validate','publish','finished'))
);

CREATE INDEX collect_run_started_at ON collect_run (started_at DESC);
CREATE INDEX collect_run_release ON collect_run (release_id);

-- 周矩阵抓取记录
CREATE TABLE collect_run_week (
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  term          text NOT NULL REFERENCES term(term),
  week          smallint NOT NULL,
  building_id   text NOT NULL,
  ok            boolean NOT NULL,
  fetched_at    timestamptz,
  page_sha256   text,
  error_message text,
  PRIMARY KEY (run_id, term, week, building_id)
);

-- 同步任务
CREATE TABLE sync_task (
  task_id       bigserial PRIMARY KEY,
  run_id        bigint NOT NULL REFERENCES collect_run(run_id) ON DELETE CASCADE,
  kind          text NOT NULL,
  required      boolean NOT NULL,
  building_id   text NOT NULL,
  week          smallint,
  room_id       text REFERENCES room(room_id),
  weekday       smallint,
  block         text,
  status        text NOT NULL DEFAULT 'pending',
  attempt_count integer NOT NULL DEFAULT 0,
  error_message text,
  CHECK (kind IN ('week_matrix','cell_detail')),
  CHECK (status IN ('pending','done','failed')),
  CHECK (attempt_count >= 0),
  CHECK (weekday IS NULL OR weekday BETWEEN 1 AND 7),
  CHECK (
    (kind = 'week_matrix' AND week IS NOT NULL AND room_id IS NULL AND weekday IS NULL AND block IS NULL)
    OR
    (kind = 'cell_detail' AND week IS NULL AND room_id IS NOT NULL AND weekday IS NOT NULL AND block IS NOT NULL)
  )
);

CREATE UNIQUE INDEX sync_task_week_matrix
  ON sync_task (run_id, building_id, week)
  WHERE kind = 'week_matrix';

CREATE UNIQUE INDEX sync_task_cell_detail
  ON sync_task (run_id, building_id, room_id, weekday, block)
  WHERE kind = 'cell_detail';

CREATE INDEX sync_task_run_status ON sync_task (run_id, status);

-- 告警
CREATE TABLE alert_log (
  dedupe_key   text PRIMARY KEY,
  last_sent_at timestamptz NOT NULL,
  send_count   integer NOT NULL DEFAULT 1
);

-- 迁移记录
CREATE TABLE schema_migration (
  version    text PRIMARY KEY,
  applied_at timestamptz NOT NULL DEFAULT now()
);
```

种子：

```sql
INSERT INTO dict (version, note) VALUES (1, '契约种子');
INSERT INTO dict_state (dict_version, state_key, label, available) VALUES
  (1, 'class',            '正常上课', false),
  (1, 'borrowed',         '借用',     false),
  (1, 'locked',           '锁定',     false),
  (1, 'exam',             '考试',     false),
  (1, 'free',             '空闲',     true),
  (1, 'fixed_reschedule', '固定调课', false),
  (1, 'temp_reschedule',  '临时调课', false),
  (1, 'fully_free',       '完全空闲', true),
  (1, 'cross_mode',       '跨模式占用', false),
  (1, 'composite',        '复合占用', false),
  (1, 'unknown',          '未知',     false);

INSERT INTO dict_symbol (dict_version, symbol, state_key) VALUES
  (1, '◆', 'class'),
  (1, 'Ｊ', 'borrowed'),
  (1, 'Ｘ', 'locked'),
  (1, 'Κ', 'exam'),
  (1, '空闲', 'free'),
  (1, 'Ｇ', 'fixed_reschedule'),
  (1, 'Ｌ', 'temp_reschedule'),
  (1, 'M', 'cross_mode');

INSERT INTO axis (version, note) VALUES (1, '5 大节 → 12 小节');
INSERT INTO axis_node (axis_version, node, ordinal, block, block_ordinal) VALUES
  (1, '01', 1, '0102', 1),
  (1, '02', 2, '0102', 1),
  (1, '03', 3, '030405', 2),
  (1, '04', 4, '030405', 2),
  (1, '05', 5, '030405', 2),
  (1, '06', 6, '0607', 3),
  (1, '07', 7, '0607', 3),
  (1, '08', 8, '0809', 4),
  (1, '09', 9, '0809', 4),
  (1, '10', 10, '101112', 5),
  (1, '11', 11, '101112', 5),
  (1, '12', 12, '101112', 5);

INSERT INTO settings (key, value) VALUES
  ('cron_expr', '0 10 4 * * 6'),
  ('building_whitelist', '[]');
```

## 附录 C：旧文档处置

- 本文是唯一产品基线。旧 `docs/product-requirements.md`、`docs/contract/*`、`specs/collector-full-sync/*` 只保留作历史与排障输入。
- 旧代码中的 `done == total` 发布门槛、正则解析、CAS 登录描述、`main_index_loadkb.jsp` 之外的周次来源，均以本文为准。
- `docs/upstream.md` 的登录章节写着 CAS，已被 2026-10-23 实测推翻；以本文 2.2、附录 A 与 [docs/upstream-api.md](upstream-api.md) 为准。
- `docs/decisions/2026-10-23-fully-free-is-range-aggregate.md` 记录的结论保留；本文把它上升为产品口径。
- 重写前已确认的技术细节与坑点见 [docs/pitfalls.md](pitfalls.md)；本文与它冲突时以本文为准，坑点文档补充证据与实现提醒。
- 重写完成后，可删除旧 `internal/` 与旧契约文档；删除前先把仍需保留的排障事实移到本文附录。
