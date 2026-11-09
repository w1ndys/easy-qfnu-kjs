# admin-settings-sync 实施计划

- [ ] 1. 设置模型与业务时区
  - 新增 `internal/model/settings.go`：七个键常量（`jw_username`/`jw_password`/`ocr_base_url`/`feishu_webhook`/`feishu_sign_secret`/`cron_expr`/`building_whitelist`）、`DefaultCronExpr='10 4 * * 6'`（5 字段，每周六 04:10）、`MaxWhitelistEntries=200`、`MaxSettingValueBytes=4096`
  - `Building{ID jxlbh, Name name}`、`SettingRow`、`SettingChange{Delete}`、`TriggerSource{manual,cron}`、`SettingKeys()`（返回七个键的副本）、`SecretSetting(key)`、`BusinessLocation()`（+08:00 固定偏移）
  - `internal/model/errors.go` 追加 `ErrSettingInvalid`/`ErrSettingStoreFailed`/`ErrRunBusy`/`ErrRunNotFound`/`ErrCollectorUnavailable`
  - 测试锁住 `SecretSetting` 的三真四假、`Building` 的 JSON 键名与 `meta` 的存储形状一致

- [ ] 2. cronexpr 解析与 Next
  - 5 字段语法：`*`、`*/n`、`a`、`a-b`、`a-b/n`、逗号列表；取值范围与别名/秒级/名称一律 `ErrInvalid`，错误文案带字段位置
  - 位图匹配 + `Matches`；`Next` 按「月 → 日 → 时 → 分」跳进，严格晚于入参，5 年无命中返回 `false`
  - 日/周同时受限时按 OR 触发，`*` 之外的 `*/n` 记为受限（Vixie 语义）
  - 表驱动 + 属性测试（`Matches(Next(t))` 恒真、区间内无更早命中）+ 边界（跨日、月末、跨年、闰年 2/29、2/30 不可达）
  - 新增 `migrations/0005_default_cron_5_fields.sql`（只改数据）：把 0001 种下的 6 段 `0 10 4 * * 6` 就地改成 5 字段 `10 4 * * 6`，`WHERE` 钉住旧值所以重复执行不再命中、管理员自定义值不被覆盖；0001–0004 一字不改，迁移集成测试补 0005 的记账与幂等断言

- [ ] 3. settings.Validate 纯校验
  - 白名单：trim 后编号非空且唯一、展示名非空、≤200 条；`[]` 合法；`Whitelist` 为 nil 时不校验
  - `cron_expr` 交给 `cronexpr.Parse`；`ocr_base_url` 与 `feishu_webhook` 非空时必须是 http/https 绝对地址（`feishu_webhook` 在加封之前按明文校验）；五项文本项 `jw_username`/`jw_password`/`ocr_base_url`/`feishu_webhook`/`feishu_sign_secret` 的明文 ≤4096 字节（`jw_username` 按 `TrimSpace` 后计；白名单两个字段逐字段套 4096、整个数组不套；cron 走语法，不套这个上界）
  - 反向用例钉住上界的作用范围：≤200 条白名单序列化后超过 4096 字节仍合法；4097 字节的合法 URL 形状 `feishu_webhook` 仍被拒（拒绝来自字节上界而非 URL 形状）
  - `ocr_base_url` 带前后空白的口径：trim 后恰好 4096 字节通过、trim 后 4097 字节拒绝（前后空白不计入这 4096，形状校验也判 trim 后的值）
  - `FieldError{Field,Message}` 用 `%w` 包裹 `model.ErrSettingInvalid`，消息点名字段（handler 直接透出成 `42201`）
  - 表驱动覆盖 `Patch` 指针三态（省略 / 空串 / 值），每条拒绝用例断言 `FieldError.Field` 是期望字段名（「整次不写库」由任务 5 的服务层用例断言）

- [ ] 4. store.SettingsRepo
  - `LoadSettings`：`SELECT ... WHERE key = ANY($1)`，只返回七个键，`updated_at` 原样返回
  - `ApplySettings`：单事务内逐条 `ON CONFLICT (key) DO UPDATE` 或 `DELETE`；任一条失败整体回滚；同一批用同一个 `now`；键外变更整批拒绝
  - 结构匹配 `settings.Store`（store 不 import settings，类型用 `model.SettingRow`/`model.SettingChange`）
  - 仓储单测（替身）：SQL 片段（`WHERE key = ANY($1)`、`ORDER BY key`、三处 `EXCLUDED.*`）与四列扫描顺序/类型、空批次不开事务、单事务同一个 `now`、键外变更零写入、失败即停并回滚、错误只带键名
  - 集成测试：upsert、删除、`updated_at` 前进、事务回滚（第二条注入 NUL 等必然失败的值）、键外行不被 `LoadSettings` 返回、`ErrSettingStoreFailed` 经 `%w` 可 `errors.Is`

- [ ] 5. settings.Service 读写与加解密
  - `Load`：读行 → `Snapshot`（`*_set` 只由行存在性决定；`cron_expr` 缺行回落默认值；白名单缺行回空数组）
  - `Patch`：`Validate` → 含**非空**秘密字段时 `keys.Current()` → `Seal("settings/<key>", 明文)` → 构造 `[]model.SettingChange` → 一次 `ApplySettings` → 回读；只把秘密设为空串（清除）时不取密钥
  - 秘密明文不 trim；非秘密文本 trim 后存，空串或 TrimSpace 后为空 → `Delete`；白名单 `json.Marshal` 后存、保持提交顺序
  - `Reveal`（唯一明文出口，供 collect-runner 与测试）、`Whitelist`、`CronExpr`；替身测试断言 AAD、`ApplySettings` 调用次数、密钥不可用时零写入

- [ ] 6. store.RunStatusRepo 与 blocked 建轮
  - `Recent`（`ORDER BY started_at DESC, run_id DESC`，`LIMIT/OFFSET` + `count(*)`）、`ByID`、`Running`、`Weeks`、`TaskSummary`（直接调包内既有 `LoadRunTaskSummary`）
  - 列扫描一律用指针承载可空列（`term`、`window_*`、`release_id`、`released_at`、`error_code`、`error_message`）
  - `StartBlockedRun`：直接插入 `status='blocked'`、`phase='finished'`、`started_at/updated_at/finished_at=now`、`error_code='whitelist_empty'`、`error_message=reason`，返回 `run_id` 与 `started_at`
  - 集成测试：排序与分页、未知 id、空表、周结果顺序、`blocked` 后 `StartCollectRun` 仍能成功（不占唯一索引）

- [ ] 7. runner 启动服务
  - `Start`：`Converge(now)` → `Running()`（命中返回 `BusyError`）→ `Whitelist()` 空则 `StartBlockedRun` 并返回 `blocked` → 非空且 `Collector==nil` 返回 `ErrCollectorUnavailable` → 否则 `Collector.Start(source)`
  - `Collector` 返回包装 `model.ErrRunConflict` 时重读 `Running`：读得到就映射成 `BusyError`，读不到（冲突窗口内那轮刚结束）返回 `50000` 让管理员重试；`StartCron` 对 busy 记 INFO 并返回 nil
  - 按来源记录日志字段 `trigger=manual|cron`，字段只有 `run_id`、`status`、`trigger`、`reason`；`Start` 用一把互斥锁串行化「读 running → 建轮」
  - 替身测试：白名单为空不调 `Collector`；冲突不调 `Collector`；`Converge` 先于 `Running`；并发 10 次至多 1 次 `Collector.Start`（假 Collector 对并发输家返回 `ErrRunConflict`）；`Collector==nil` 时零建轮

- [ ] 8. scheduler 调度循环
  - `New(log, trigger, exprSource, clock, wait)` + `Run(ctx)`：装载表达式 → `Next` → 等 `wait` / `reloadCh` / `ctx.Done` → 到点调 `trigger`（带 30 秒超时）→ 重算
  - `Reload(expr)` 解析成功才替换并唤醒循环；失败保留旧计划并返回错误；`Next()` 供测试与启动日志
  - 启动期表达式缺失用 `DefaultCronExpr`、非法记 ERROR 后回落；触发失败只记日志，下一时刻照常
  - 假时钟 + 注入 `wait` 的测试：到点一次、reload 生效、取消退出、非法存量回落、`Run` 不并发重入

- [ ] 9. api 设置端点与中间件
  - 中间件：`adminRead`（no-store → 会话）、`adminWriteLimit`（no-store → 体门(limit) → 会话 → CSRF）、`withJSONBodyLimit`（既有 `withJSONBody` 改调它并保持 8 KiB）、`decodeAdminBodyStrict`
  - `GET /api/admin/settings`：`settings.Load` → `cronexpr.Parse` 算 `cron_next_at`（UTC RFC3339，算不出为 null）→ 8 个键的 `data`
  - `PUT /api/admin/settings`：64 KiB 体门（中间件）→ strict 解码（未知字段 `40001`）→ `settings.Patch` → `reloadCron(snapshot.CronExpr)`（仅提交了 `cron_expr` 时；失败只记 ERROR）→ 返回与 GET 同形状的最新快照（含 `cron_next_at`）
  - 契约测试：键集合、`*_set`、哨兵秘密不回显、40001/42201/50000、`no-store`、缺会话/缺 CSRF、64 KiB 边界（恰好通过、+1 拒绝）
  - 并发用例：两个带不同 `cron_expr` 的 PUT 同时打（重复 32 轮），断言都 `200`、各重排一次、最后一次重排等于库里的 `cron_expr`（`settingsMu` 串行化，CP19）；空补丁回读失败的日志文案另有一例用非哨兵错误，锁住「按补丁形状分派、不看错误类别」
  - 门禁：`cd backend && go test -race -count=1 ./internal/api ./internal/settings` **必跑**（`settingsMu` 串行化与并发用例只有带竞态检测器才完整；仓库根 `task gate` 已含 `backend:test-race`），连同 `go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`golangci-lint run` 全部 0 退出

- [ ] 10. api 轮次读取端点
  - `GET /api/admin/runs`：`limit` 1–100 默认 20、`offset ≥ 0`，越界 `40001`；`{runs,total,limit,offset}`
  - `GET /api/admin/runs/{run_id}`：路径参数解析（非正整数 `40001`）、`ByID` 不存在 `40401`、附 `tasks` 与 `weeks`
  - 新增业务码常量 `codeRunNotFound=40401`（`404` 未知轮次；第 11 项复用同一常量，不重复声明）
  - 响应结构体固定 json 标签，空列表输出 `[]` 而非 `null`；所有响应 `no-store`
  - 契约测试：摘要 22 个键、详情 `tasks`/`weeks` 形状、`term` 为 null 的 blocked 轮、未知 id、空表

- [ ] 11. api 手动同步端点
  - `POST /api/admin/sync`：`adminWriteNoBody`（no-store → 会话 → CSRF，不读体、不要求 `Content-Type`）→ `runner.Start(manual)`
  - 202 返回 `{run_id, status, started_at}`（blocked 另带 `reason`）；`BusyError` 映射 409 + `40901`，消息点名 `run_id` 与 `phase`；`ErrCollectorUnavailable` 与仓储失败映射 `50000`
  - 新增业务码常量 `codeRunBusy=40901`；`codeRunNotFound=40401` 复用第 10 项已定义的常量；202 走 `writeJSON`（`ok` 固定 200）
  - 契约测试：202 三种形状、409 消息、未装配 50000、缺 CSRF 40301、并发 50 请求只 1 个 202（假 Collector 对并发输家返回 `ErrRunConflict`）

- [ ] 12. 装配、依赖规则与端到端检查点
  - `main.go` 装配 `settings.New(store.NewSettingsRepo(pool), keys, nil)`、`store.NewRunStatusRepo(pool)`、`runner.New`（`Converge` 用 `pipeline.ConvergeStaleRuns` 包装）、`scheduler.New` 并 `go Run(ctx)`、三个 `api.With*`；装配层加 `var _ settings.Store = store.SettingsRepo{}`
  - depguard 补 `settings`/`cronexpr`/`scheduler`/`runner` 四条规则，`api` allow 补 `settings`/`runner`/`cronexpr`；启动日志新增 `collector_wired`、`settings_cron`，`Collector==nil` 时记一条 WARN（task 9 已提前给 `api` allow 放行 `settings`/`cronexpr`；**本条仍欠四条规则 + `api` allow 的 `runner`**，lint 绿不等于依赖规则已补完）
  - 端到端（真实库 + 假 Collector）：init → login → GET 默认设置 → PUT（白名单 + cron + 三个秘密）→ GET（`*_set`、无明文、`cron_next_at`）→ 空白名单 sync 得 blocked → 配名单后 sync 得 running → 再 sync 得 409 → runs 列表与详情 → 非法 cron 42201 且设置不变 → 库里密文以 `v1:` 开头
  - 日志脱敏（哨兵不出现在日志/响应）、门禁与任务 9 同口径（`go build ./...`、`go vet ./...`、`golangci-lint run`、`go test -count=1 ./...`、`go test -race -count=1 ./internal/api ./internal/settings` 全部 0 退出）、`go.mod` 无变化；有疑问询问用户
