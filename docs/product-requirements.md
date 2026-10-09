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
| WebUI 管理面板 | 管理员配置、状态查看、版本对比与回退；业务设置写入数据库配置 |
| PostgreSQL | canonical 存储：清洗后的数据逐格入库 |
| ddddocr-fastapi | 验证码识别服务（生产环境已用 Docker 部署），由采集器通过 HTTP 调用 |

## 3. 数据采集

### 3.1 登录与验证码

采集器使用教务系统教师账号登录 CAS。账号、密码由 WebUI 管理面板配置并写入数据库配置，采集器从库读取。

验证码识别调用 ddddocr-fastapi：把验证码图片转 base64，`POST {ocr_base_url}/ocr`（表单字段 `image`，可选 `probability`、`png_fix`）。响应为 `{"code":200,"message":"Success","data":"<识别文本>"}`，识别文本在 `data`；服务异常也返回 HTTP 200，以 `code=200` 判定成功。`ocr_base_url` 由 WebUI 配置。

### 3.2 学期与教学周历

采集器读取当前学期（`GET /jsxsd/kbxx/jsjy_query`）。周次与总周数不从 `jsMain_new.jsp` 取，改为对当天日期请求一次 `POST /jsxsd/framework/main_index_loadkb.jsp`，表单字段 `rq=<YYYY-MM-DD>`，只解析响应脚本中的「第 X 周 / 共 Y 周」。个人课表不入库。由采样日期、星期几和周次反推第 1 周周一，再生成第 1 周到第 Y 周的周一日期写入 `term_week`。查询用 `date_offset` 得到目标日期，只查这张表，不调用该接口。详见 `docs/decisions/2026-10-09-term-week-from-one-sample.md`。

### 3.3 周状态采集

按周、按白名单教学楼请求状态：`POST /jsxsd/kbxx/jsjy_query2`，参数 `typewhere=jszq`、`xnxqh=<学期>`、`jxlbh=<该楼编号>`、`jsmc_mh=`（留空）、`zc = zc2 = <周次>`、`xq=1`、`xq2=7`。一栋楼一次请求。不把楼名写入 `jsmc_mh`，也不留空 `jxlbh` 拉全校再过滤。每行仍是 35 格（7 天 × 5 大节）。

### 3.4 采集范围与目标周

系统只保存白名单教学楼里的房间，查询扫已入库的房间。白名单为空时本轮失败，不退回全校请求。

定时与手动都跑同一套全量，不再按普通日期只采近 4 周：

- 每一栋白名单楼，从第 1 周到该学期最后一周各请求一次周矩阵。最后一周用周历采样得到的总周数，不写死 20。`zc` 与 `zc2` 填同一个周次。
- 每一栋白名单楼再请求一次周次留空的聚合矩阵。只对聚合矩阵上有内容的格子请求一次占用明细。空格子不下钻，也不按周重复下钻。
- 某周是否空闲以该周矩阵为准。明细的周次范围只补矩阵没有返回的周，并标成派生。解析失败不得当成空闲。

### 3.5 采集调度

采集时间由 WebUI 配置的 `cron_expr` 驱动，示例为每周六凌晨。管理员也可以手动开始同一套全量。调度器在采集器内按表达式触发。

同步进度写入数据库。WebUI 显示阶段、完成数/总数、最近错误和更新时间。任务粒度是「楼 + 周矩阵」和「楼 + 房间 + 格子明细」。已完成的任务不重打。会话失效先重新登录，再从断点继续。

### 3.6 运行约束

- 同一时刻至多一轮采集，锁由后端与数据库维护。
- 单次网络请求最多 3 次重试。请求间隔不低于 500 毫秒，即不超过每秒 2 次。
- 不再用 15 分钟整轮时限掐断全量同步。单个周矩阵失败记下该任务并继续；表头块集合变化仍终止整轮。
- 登录页或非法访问特征按会话失效处理：重新登录后续跑，不把整轮已完成的任务作废。

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
| 7 validate | 与上一版对比校验 | 校验结果入库 |
| 8 publish | 单事务写入并切换 current | 一次发布（current + previous 两版） |

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

canonical 是 PostgreSQL，结构以 `docs/contract/db.v2.sql` 为准。

### 5.1 存储职责

| 表 | 职责 |
|---|---|
| `room` | 房间身份与属性：`jsbh`、规范化名、原始名、所属教学楼编号与展示名、首次/最近出现 |
| `dict` / `dict_state` / `dict_symbol` | 状态字典与符号分类（版本化） |
| `axis` / `axis_node` | 12 节点 ↔ 5 大节映射（版本化） |
| `term` / `term_week` | 学期、总周数、每周一日期与教学周历 |
| `settings` | WebUI 业务设置（账号、飞书、OCR 地址、cron、教学楼白名单） |
| `release` | 发布版本：`release_id`、学期、字典/轴版本、`is_current` |
| `observation` | 观测事实：`release × room × term × week × weekday × node → state` |

### 5.2 发布与版本

- 观测每格一行，主键 `(release_id, room_id, term, week, weekday, node)`；`available` 为字典 `available` 的发布期投影，`raw_text` 仅 unknown/composite 存。
- 发布 = 单事务写入观测 → 新 release 标 `is_current` → 旧 current 降为 previous → 更早删除（保留两版）。
- `release_id` 是可读短 id（时间戳字符串），无内容寻址。

### 5.3 日历与时间

时间统一 `Asia/Shanghai`。每周的周一绝对日期入库；当前周/星期由服务端时钟与日历表共同确定。

## 6. 查询 API

查询契约以 `docs/contract/api.v2.md` 为准：3 个只读 GET，统一响应 `{code, message, data, request_id}`。

### 6.1 端点

| 路径 | 参数 | `data` |
|---|---|---|
| `GET /api/context` | — | `{date, term, week, weekday, in_calendar}` |
| `GET /api/meta` | — | 数据版本、字典、轴、可用学期与逐周新鲜度 |
| `GET /api/query` | `view`、`keyword`、`date_offset`、`start_node`、`end_node`、`limit`、`offset` | `{total, items, date, week, day_of_week}` |

### 6.2 参数与判定

- `view=availability`（空教室）：`keyword` + `date_offset` + `start_node`/`end_node`（默认 `01`/`11`）。起止是小节编号，不必对齐上游大节，也可以跨大节。判定 = 区间内每节都可用（`free`/`fully_free`），`items=[{id, name}]`。
- `view=day`（状态列表）：`keyword` + `date_offset`；返回 12 小节状态，`items=[{id, name, statuses}]`。
- `date_offset` 0..10 默认 0；`limit`/`offset` 默认 50/0。
- 不在教学周：正常返回，由 `context.in_calendar=false` 表达。

### 6.3 错误码与新鲜度

- `code`：`0` 成功 / `40001` 参数错误 / `40401` 无数据 / `50000` 内部错误。
- 新鲜度 8 天落在 `meta`，过期返回旧数据并标 `stale`。每周一次的全量若仍用 36 小时，周二到周五会被标成过期。

## 7. WebUI 管理面板

管理员登录：用户名固定 `admin`，密码来自部署期环境变量 `ADMIN_PASSWORD`（不落库）。

### 7.1 配置

管理员在面板中维护并持久化到数据库配置：

- `account_username` / `account_password`：教务系统教师账号；
- `feishu_webhook_url` / `feishu_secret`：飞书告警（加签模式）；
- `ocr_base_url`：ddddocr-fastapi 服务地址；
- `cron_expr`：采集调度 cron 表达式。
- `building_whitelist`：教学楼白名单，JSON 数组，元素为 `jxlbh` 与展示名。采集只发送 `jxlbh`。空数组不得触发全校请求。

### 7.2 状态、对比与回退
面板展示采集运行状态、进度（阶段、完成数/总数、最近错误）、最近校验结果、当前数据版本与逐周新鲜度。管理员可手动开始全量同步。运行状态与告警去重由后端与数据库维护。

发布切换后，面板提供「当前版 vs 上一版」对比：房间集合（新增/消失/共有）、状态格变更明细、汇总（变更房间数/变更格数/unknown 与 composite 增减），并支持一键回退（交换 current 与 previous）。

### 7.3 安全

账号密码与飞书签名只存于数据库配置，不进 Git、日志、快照，由面板受控存取。部署级变量（如 `DATABASE_URL`）由环境传入，其余业务设置均走面板。

## 8. 前端

前端为 React 19 + Vite + TypeScript + antd 6，桌面优先、响应式，移动端可用；品牌色 `#884F22` 作为 antd theme token；状态颜色与图标在前端本地映射，不进数据。

### 8.1 页面

首页、空教室查询、全天状态、404 四页。

### 8.2 交互

- 打开页面请求 `context`；日期用「今天/明天/后天/…」选择器（`date_offset`）；有明确查询按钮，回车触发同一次查询。
- 筛选条件：关键词、日期（`date_offset`，今天/明天/后天/…）、起止节次（仅空教室页）；状态名与节次从 `meta` 获取，前端不硬编码。
- 全天状态按 12 小节展示。
- 结果分区展示：范围内无空教室、数据未收录、数据过期、当前不在教学周。
- 同名房间追加缩短后的 `id`，其余只显示名称。

### 8.3 质量门禁

`npm run lint`、`npm run typecheck`、`npm run build` 三项必过；周次解析、房名标签、状态视觉映射等纯函数配单元测试。

## 9. 安全与隐私

- 数据范围：系统存储并发布房间身份（`jsbh`）、房名、所属教学楼、按周/星期/节次的占用状态，以及占用明细中的教室状态、课程、周次范围、时间标志。课程字段同时用于考试科目。不存储申请人，不存储任课教师。
- 来源留存：只存请求参数、`fetched_at` 与 `page_sha256`，不存原始 HTML。
- 秘密管理：账号、密码、Cookie、Token、验证码、飞书签名不进 Git、Issue、快照、日志；业务秘密由 WebUI 配置存储受控存取。
- 告警内容不含账号、密码、Cookie、Token、原始页面。

## 10. 运维与告警

- 采集锁、运行状态、告警去重、发布验收均由后端内部与数据库维护；发布验收在库内完成。
- 告警触发：重试耗尽后的最终失败、校验拦截、学期切换、连续失败后的首次恢复。普通成功与非教学周不通知；同一轮同类错误只通知一次。
- 公网域名切换在本机内网验收通过之后进行。

## 11. 验收标准

- Given 定时触发或管理员手动开始，Then 采集器对白名单楼做各周矩阵，并对聚合矩阵上的有内容格子各请求一次明细。
- Given 上游出现未收录符号，Then 该格记为 `unknown` 且发布照常进行，`unknown_cells` 计数随之增长。
- Given 上游表头块集合变化，Then 采集终止并告警，不自动适配。
- Given 业务设置在 WebUI 中修改，Then 采集器与查询服务在下一轮从数据库读取到新值。
- Given `go build ./... && go test ./...` 执行，Then 全部通过且不需要外部服务。
- Given 一次发布完成，Then 新 release 为 current、旧 release 为 previous，更早的已删除。
- Given WebUI 执行回退，Then current 与 previous 标记互换，查询立即读到回退后的数据。
- Given 请求 `/api/query?view=availability&keyword=…&date_offset=0&start_node=01&end_node=11`，Then 返回区间内每节空闲的房间与 `total`。
- Given 请求 `/api/query?view=day&date_offset=0`，Then 返回房间的 12 小节状态。
- Given 没有任何可用 release，Then 查询返回 `code=40401`。
