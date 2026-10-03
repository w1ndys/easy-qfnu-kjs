# 产品需求文档（easy-qfnu-kjs）

> 本文档整合 Issue #1、Issue #2 与当前仓库 `docs/` 下的数据与接口契约，是产品需求的完整、初始态描述。
> 当 Issue 与本仓库发生冲突时（例如教室清洗规则、分组、配置来源、验证码识别、状态编码），**以当前仓库为准**。

## 1. 产品概述

曲阜师范大学空教室查询系统：把教务系统的教室占用状态清洗成结构化数据，向校内师生提供空教室检索与全天状态查询。

- 查询用户：校内师生，按关键词、学期、周次、星期、节次区间查询教室。
- 管理员：通过 WebUI 管理面板维护采集配置，查看采集运行状态与校验结果。
- 系统以本机 PostgreSQL 为唯一数据源，采集器写入、查询服务读取，WebUI 管理配置与状态。

## 2. 系统组成

| 组件 | 职责 |
|---|---|
| 采集器 | 登录教务系统，按周采集全校教室状态，清洗并写入数据库，执行发布 |
| 查询服务 | 提供只读 HTTP API，从数据库读取当前数据并回答查询 |
| WebUI 管理面板 | 管理员配置与状态查看；业务设置写入数据库 `kjs.settings` |
| PostgreSQL | canonical 存储：清洗后的数据直接入库 |
| ddddocr-fastapi | 验证码识别服务（生产环境已用 Docker 部署），由采集器通过 HTTP 调用 |

## 3. 数据采集

### 3.1 登录与验证码

采集器使用教务系统教师账号登录 CAS。账号、密码由 WebUI 管理面板配置并写入 `kjs.settings`，采集器从库读取。

验证码识别调用 ddddocr-fastapi：把验证码图片转 base64，`POST {ocr_base_url}/ocr`（表单字段 `image`，可选 `probability`、`png_fix`）。响应为 `{"code":200,"message":"Success","data":"<识别文本>"}`，识别文本在 `data`；服务异常也返回 HTTP 200，以 `code=200` 判定成功。`ocr_base_url` 由 WebUI 配置。

### 3.2 学期与教学周历

采集器读取当前学期（`GET /jsxsd/kbxx/jsjy_query`）与当前周/总周数（`GET /jsxsd/framework/jsMain_new.jsp?t1=1`）。教学周历、总周数、每周的周一日期写入 `kjs.term` 与 `kjs.term_week`。

### 3.3 周状态采集

按周请求全校状态：`POST /jsxsd/kbxx/jsjy_query2`，参数 `typewhere=jszq`、`xnxqh=<学期>`、`jsmc_mh=`（留空 = 全校）、`zc = zc2 = <周次>`、`xq=1`、`xq2=7`。单次返回全校约 2079 行、每行 35 格（7 天 × 5 大节）。

### 3.4 采集范围与目标周

系统保存全校全部房间的占用状态，查询直接扫全库。目标周策略：

- 普通日期：当前周与未来 4 周；
- 周日、没有基线、学期切换：当前学期全部周次。

### 3.5 采集调度

采集时间由 WebUI 配置的 cron 表达式（`kjs.settings.cron_expr`）驱动，调度器按表达式触发采集。

### 3.6 运行约束

- 同一时刻至多一轮采集，锁由数据库 `pg_advisory_lock` 维护。
- 单次网络请求最多 3 次重试；周与周之间随机间隔 0.5–2 秒；整轮网络阶段总时限 15 分钟。
- 登录页或非法访问特征、表头块集合变化、结构校验失败均终止本轮，按告警策略通知。

## 4. 数据清洗

清洗契约以 `docs/contract/data-format.v2.md` 为准，管线的输入、输出与不变量如下。

| 层 | 处理 | 结果 |
|---|---|---|
| 1 fetch | 已登录会话请求周数据 | HTTP 响应、`fetched_at`、`page_sha256` |
| 2 parse | 解析 `table#dataList` 的行、`jsbh`、35 格原文 | 结构化行；表头恰 35 格且块序固定，否则结构失败 |
| 3 normalize | 房名 NFKC、删除容量片段、压缩空白 | `name`、规范化名称 |
| 4 classify | 35 格原文按分类表归类 | 每格一个语义状态键，未知/复合保留原文本 |
| 5 expand | 5 大节展开为 12 小节 | 7 × 12 节点状态；断言同一大节内节点状态一致 |
| 6 observe | 生成观测事实 | 一间房 × 一周 × 一天 × 一个节次恰一行 |
| 7 validate | 与上一版对比校验 | 校验结果入库 `kjs.release_check` |
| 8 publish | 单事务写入并切换当前版本 | 一个不可变 release |

### 4.1 房间身份与名称规范化

房间唯一身份是 `jsbh`。房名按 NFKC → 反复删除容量片段 `(n/N)`/`（n/N）` → 压缩并去首尾空格处理；方位、校区等其他括号保留。规范化后仍可能同名，同名房间不合并，`jsbh` 区分。系统保留原始房名 `name_raw`。

### 4.2 状态字典

状态用语义键表达，字典随数据版本（`dict_version`）记录，改版不污染旧数据。

| 语义键 | 名称 | 可用 | 上游符号 |
|---|---|---|---|
| `free` | 空闲 | 是 | 空单元格、`空闲` |
| `fully_free` | 完全空闲 | 是 | `完全空闲` |
| `class` | 正常上课 | 否 | `◆` |
| `borrowed` | 借用 | 否 | `Ｊ` |
| `locked` | 锁定 | 否 | `Ｘ` |
| `exam` | 考试 | 否 | `Κ` |
| `fixed_reschedule` | 固定调课 | 否 | `Ｇ` |
| `temp_reschedule` | 临时调课 | 否 | `Ｌ` |
| `cross_mode` | 跨模式占用 | 否 | `M` |
| `composite` | 复合占用 | 否 | 多个已收录符号的组合 |
| `unknown` | 未知 | 否 | 未收录文本 |

符号码点固定（`Ｊ`/`Ｘ`/`Ｇ`/`Ｌ` 是全角拉丁字母，`Κ` 是希腊大写 Kappa）。空单元格归入 `free`；多个已收录符号或复合词归入 `composite`；未收录文本归入 `unknown` 并保留原文。`unknown` 与 `composite` 一律不可用，未知不会映射成空闲。

### 4.3 节次轴

12 个节次与 5 个大节的映射是版本化数据（`axis_version`）：`0102 → 01,02`、`030405 → 03,04,05`、`0607 → 06,07`、`0809 → 08,09`、`101112 → 10,11,12`。展开时断言同一大节内节点状态一致；表头块集合变化时创建新轴版本并终止本轮等待人工确认，旧数据因携带自身 `axis_version` 仍可解释。

## 5. 数据存储

canonical 是数据库，结构以 `docs/contract/db.v2.sql` 为准。

### 5.1 存储职责

| 表 | 职责 |
|---|---|
| `room` | 房间身份与属性：`jsbh`、规范化名、原始名、首次/最近出现 |
| `dict` / `dict_state` / `dict_symbol` | 状态字典与符号分类表（版本化） |
| `axis` / `axis_node` | 12 节点 ↔ 5 大节映射（版本化） |
| `term` / `term_week` | 学期、总周数、每周一日期与教学周历 |
| `settings` | WebUI 业务设置（账号、飞书、OCR 地址、cron） |
| `release` / `release_room` | 发布版本与房间在本次发布中的呈现快照 |
| `observation` | 观测事实：`release × room × week × weekday × node → state` |
| `release_week` / `release_week_source` | 逐周内容哈希、新鲜度、质量计数、来源参数与页面哈希 |
| `release_check` | 校验结果，作为数据供人工与前端查看 |

### 5.2 发布与版本

- 一个 release 是一份不可变的观测集合，`release_id` 由内容寻址得出。
- 同一时刻至多一个 current；切换在单事务内完成。
- 保留 current 与 previous 两份，更早的自动清理。
- 每周的 `content_hash` 记录内容身份；内容未变时哈希一致。

### 5.3 日历与时间

时间统一 `Asia/Shanghai`。每周的周一绝对日期入库，查询侧与消费方不再自行推算周次；当前周/星期由服务端时钟与日历表共同确定。

## 6. 查询 API

查询契约以 `docs/contract/api.v2.md` 为准。全部只读 GET，错误使用 RFC 9457 `application/problem+json` 并带稳定 `code`。

### 6.1 资源

| 路径 | 作用 |
|---|---|
| `GET /api/context` | 当前时间上下文：日期、学期、周次、星期、是否在教学周 |
| `GET /api/meta` | 数据版本与字典：`release_id`、`dict_version`、`axis_version`、状态字典、节次轴 |
| `GET /api/terms` | 学期与周：总周数、每周一日期、是否有数据与新鲜度 |
| `GET /api/availability` | 空教室：关键词 + 周/星期 + 节次区间，返回区间内每节都空闲的房间 |
| `GET /api/day` | 全天状态：同一筛选，返回每个房间该日的节次状态 |

### 6.2 参数

- `q`：关键词，对全库房名做子串匹配（NFKC、ASCII 不区分大小写、最长 32、不支持正则）；省略时返回全库房间。
- `term`、`week`、`weekday`（1 = 周一；省略时取当前上下文）。
- `from`、`to`：节次区间（`01`–`12`，默认 `01`、`12`，要求 `from ≤ to`），仅 `/api/availability`。
- `fold`：`nodes`（12 小节，默认）或 `blocks`（5 大节），仅 `/api/day`。

### 6.3 判定与载荷

- 空教室判定：所选星期内，名称匹配的每个房间，`from` 到 `to` 的每一节都可用才返回；可用状态为 `free` 与 `fully_free`。
- 载荷使用语义状态键；房间返回 `id`、`name`；每个响应带 `schema_version` 与 `data.release_id`、`dict_version` 等数据版本信息。
- 第一版查询不分页，全量返回。

### 6.4 错误

稳定错误码：`invalid_parameter`（400）、`not_published`（404）、`no_data`（503）、`not_found`（404）、`method_not_allowed`（405）、`internal`（500）。

### 6.5 新鲜度与缓存

新鲜度由查询侧计算：当前周与未来 4 周距最近成功超过 36 小时为过期，其余周超过 8 天为过期；最近一次采集失败也算过期。过期仍返回上一版成功数据并标记 `stale: true`。`/api/meta`、`/api/terms` 可长缓存（随 `release_id` 变化失效），`/api/context` 短缓存，查询端点按 `release_id` 缓存。

## 7. WebUI 管理面板

### 7.1 配置

管理员在面板中维护并持久化到 `kjs.settings`：

- `account_username` / `account_password`：教务系统教师账号；
- `feishu_webhook_url` / `feishu_secret`：飞书告警（加签模式）；
- `ocr_base_url`：ddddocr-fastapi 服务地址；
- `cron_expr`：采集调度 cron 表达式。

### 7.2 状态与观测

面板展示采集运行状态、最近校验结果（`release_check`）、当前 release 与逐周新鲜度（`release_week`）。运行状态与告警去重由后端与数据库维护。

### 7.3 安全

账号密码与飞书签名只存于 `kjs.settings`，不进 Git、日志、快照，由面板受控存取。部署级变量（如 `DATABASE_URL`）由环境传入，其余业务设置均走面板。

## 8. 前端

前端为 React 19 + Vite + TypeScript + antd 6，桌面优先、响应式，移动端可用；品牌色 `#884F22` 作为 antd theme token；状态颜色与图标在前端本地映射，不进数据。

### 8.1 页面

首页、空教室查询、全天状态、404 四页。

### 8.2 交互

- 打开页面请求 `context`，填入当前周与星期；有明确查询按钮，回车触发同一次查询；「今天」按钮恢复当前周与星期。
- 筛选条件：关键词、周次、星期、起止节次；状态名与节次从 `meta` 获取，前端不硬编码。
- 全天状态默认 12 小节，可切换为 5 大节折叠视图。
- 结果分区展示：范围内无空教室、数据未收录、数据过期、当前不在教学周。
- 同名房间追加缩短后的 `id`，其余只显示名称。

### 8.3 质量门禁

`npm run lint`、`npm run typecheck`、`npm run build` 三项必过；周次解析、房名标签、状态视觉映射等纯函数配单元测试。

## 9. 安全与隐私

- 数据范围：系统存储并发布房间身份（`jsbh`）、房名与按周/星期/节次的占用状态；占用详情（课程、教师、申请人、单双周）不属于数据范围。
- 来源留存：只存请求参数、`fetched_at` 与 `page_sha256`，不存原始 HTML。
- 秘密管理：账号、密码、Cookie、Token、验证码、飞书签名不进 Git、Issue、快照、日志；业务秘密由 `kjs.settings` 受控存取。
- 告警内容不含账号、密码、Cookie、Token、原始页面。

## 10. 运维与告警

- 采集锁、运行状态、告警去重、发布验收均由后端内部与数据库维护；发布验收在库内读回 current 并抽查数据。
- 告警触发：重试耗尽后的最终失败、校验拦截、学期切换、连续失败后的首次恢复。普通成功与非教学周不通知；同一轮同类错误只通知一次。
- 公网域名切换在本机内网验收通过之后进行。

## 11. 验收标准

- Given 数据库中存在一次成功发布的 release，When 请求 `GET /api/meta`，Then 返回该 release 的 `release_id`、字典与轴版本、状态字典与节次轴。
- Given 某房间在所选星期、节次区间内每一节都空闲，When 请求 `GET /api/availability?q=…&week=…&weekday=…&from=…&to=…`，Then 该房间出现在结果中，字段为 `id`、`name`。
- Given 没有可用 release，Then 查询返回 `no_data`（503）；Given 学期或周没有数据，Then 返回 `not_published`（404）。
- Given 数据库中没有基线、学期切换或周日，Then 采集器刷新当前学期全部周次。
- Given 上游出现未收录符号，Then 该格记为 `unknown` 且发布照常进行，`unknown_cells` 计数随之增长。
- Given 上游表头块集合变化，Then 采集终止并告警，不自动适配。
- Given 一次发布完成，Then 数据库存在且仅存在一个 current release，上一版保留为 previous。
- Given 业务设置在 WebUI 中修改，Then 采集器与查询服务在下一轮从数据库读取到新值。
- Given `go build ./... && go test ./...` 执行，Then 全部通过且不需要外部服务。
