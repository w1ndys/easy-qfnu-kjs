# collect-runner 实施计划

> 每项都能独立实现与验收，顺序即依赖顺序（1–4 是底座，5–10 是执行器本体，11 是装配与收口）。
> 每项完成后跑该项涉及包的 `go test -count=1`；第 11 项跑全量门禁。带库用例需要 `KJS_TEST_DSN`，且必须 `-p 1` 串行（`e2e` 与 `store` 都会 `DROP SCHEMA public`）。
> 本 spec 不写任何 Go 代码，以下条目在实现阶段执行。

- [ ] 1. 建轮、补学期与窗口写入口
  - `store.StartRunningRun` 插入 `status=running`、`term` 为 NULL 的轮次，只认 `collect_run_single_running` 的 23505 并映射 `model.ErrRunConflict`
  - `store.SetRunTerm` / `store.SetRunWindow` 都带 `WHERE run_id = $1 AND status = 'running'` 守卫，0 行更新报错
  - `pipeline.RecordTerm` 拒绝空学期、`pipeline.RecordWindow` 拒绝空窗与跨度 > 4 周的窗口，两者都不落库
  - 单测：替身 queryer 钉住三条 SQL 的列与守卫；`RecordWindow` 的 5 个边界（空窗、4 周、5 周、已收口、正常）

- [ ] 2. 失败收口与错误码
  - `pipeline.FailRun(ctx, phases, runID, code, cause, clock)` 写 `failed` / `phase=finished` / `finished_at` / `error_code`（空串折 NULL）/ `error_message`（`cause` 为 nil 时固定文案）
  - 错误码常量固定在 `collector`（`settings_incomplete`、`auth_failed`、`ocr_failed`、`validation_blocked`），与 `pipeline` 既有的四个结构失败码不重名、不重复写入
  - 单测：正常写、空码写 NULL、`cause` 为 nil 的固定文案、已收口轮次报错

- [ ] 3. 学期日历整体替换
  - `store.ReplaceTerm` 在单事务内 `UPSERT term` → 逐周 `UPSERT term_week(week, monday, in_calendar=true)` → 删 `week > Y` 的残留
  - 锚点守卫：已有日历的第 1 周周一 ≠ 新算值时回滚并返回 `store.ErrTermAnchorConflict`
  - `collector` 侧的锚点纯函数：第 1 周周一 = 采样日所在周周一 − 7×(C−1)，按 `Asia/Shanghai`（固定 +08:00）
  - 集成（真实库）：首次写入 16 周、重复写入行数与日期不变、学期缩短时删掉尾部、锚点冲突时零变更

- [ ] 4. 字典与轴快照读取
  - `store.LoadCollectSnapshot` 取 `dict` / `axis` 的最大版本，返回 `DictVersion`、`model.Dict`（Symbols + States）、`AxisVersion`、`[]model.AxisNode`（按 `ordinal` 升序）
  - 没有版本行时返回包装 `model.ErrQueryFailed` 的错误，不返回空快照
  - 单测：最大版本选择、两表逐行映射、`ordinal` 排序、空表报错

- [ ] 5. 执行器骨架与启动语义
  - `collector.New(Deps)` 校验必填依赖（`RunCtx`、`Log` 与全部仓储/上游/设置），缺一返回包装 `model.ErrConfigInvalid` 的错误
  - `Start` 先 `StartRunningRun` 再起后台协程并返回 `running`；后台用 `Deps.RunCtx` 而不是请求 ctx
  - 后台 panic 时 `recover` + `FailRun`（无码）+ 一条 ERROR 日志；`ctx` 取消时保持 `running` 不收口
  - 单测：唯一索引冲突 → `model.ErrRunConflict`；`Start` 在后台完成前返回；取消请求 ctx 后轮次仍跑到终态（CP11）；panic 后轮次 `failed`

- [ ] 6. 设置、登录与学期/周历阶段
  - 读设置：`Settings.Load` + `Reveal(jw_password)`；任一为空或解封失败 → `settings_incomplete`，原因只点名键、不写值
  - 登录：`ErrUpstreamAuthFailed` → `auth_failed`，`ErrUpstreamOCRFailed` → `ocr_failed`，其余（网络/超时）→ 无码
  - 学期/周历：`FetchTerm`/`FetchWeekCalendar` + `parser.TermText`/`parser.WeekText` 双口径；任一不一致或锚点冲突 → `pipeline.FailCalendar`（`calendar_invalid`）
  - 单测：五条失败路径各一条，断言终态、错误码与「学期日历未被改动」

- [ ] 7. 矩阵抓取与内存候选
  - `TaskFetcher` 闭包：`FetchWeekMatrix` → `parser.Rows` → 逐格 `clean.State` → 写入 `pages[(楼,周)]`（整片覆盖）
  - 页面缓存同时充当「每 (楼,周) 每轮至多一次请求」的守卫（CP10），并把 `Page` 原样带回给 `pipeline.Run` 算 sha256
  - 不可用格：`Available == false`（含未知与复合）记入 `unavailable[(房,天,大节)]`，窗口内任一周不可用即算一格
  - 单测：同一 (楼,周) 抓两次只产生一份候选；不可用格去重跨周生效；结构失败原样返回给 `pipeline` 走任务失败路径

- [ ] 8. 两段式 Plan → Run 与明细抓取
  - 第一段 `Plan(window, 楼, nil)` → `Run`；第一段后 `EvaluateRun` 为 `failed` 时立即 `CloseRun` 并跳过明细（Requirement 10.7）
  - 第二段：从 `unavailable` 导出 `[]pipeline.Cell`，归属楼取 `collect_run_room`（`LoadRunRooms`），无归属的房号丢弃并计入 `orphanCells`
  - 明细：`FetchOccupancyDetail` → `parser.Occupancy` → `clean.State`，四项字段写入 `details[(房,天,大节)]`（整片覆盖）
  - 单测：调用顺序（Plan1 → Run1 → EvaluateRun → Plan2 → Run2）、明细任务无一条被归属过滤丢弃（`Dropped()==0`）、明细结构失败 → `partial`

- [ ] 9. hydrate：续跑时的内存候选重建
  - `TaskRead.LoadWeekMatrixTasks` 取 `week_matrix` 任务，筛 `done` 且 `pages` 里没有的 (楼,周)；这一步在规划第一段**之前**执行（阶段仍停在 `week_matrix`）
  - 对每个缺失键补抓一次并只累计内存候选：不写 `collect_run_week`、不改 `sync_task`
  - 补抓失败 → `FailRun`（无码，原因点名楼与周），不进入发布
  - 单测：预置 `running` 轮次 + `done` 的周矩阵任务 + 空内存 → 补抓数与缺失键数一致；全部命中时补抓数为 0（CP10）；补抓失败时轮次 `failed` 且 `release` 行数不变

- [ ] 10. 发布接线
  - `EvaluateRun` 为 `success`/`partial` 时：`AdvancePhase(validate)` → `Build` → `Validate(Thresholds{})` → `Commit`
  - `Build`/`Validate` 拒绝 → `FailRun(validation_blocked)`；`Commit` 失败 → `FailRun`（无码），两者都不动原 current
  - `DroppedRequired`/`DroppedOptional` 与 `CloseRun`/`EvaluateRun` 用同一个 `Dropped()` 值；`Commit` 成功后不再调用任何收口函数
  - 单测：success 与 partial 各一条、候选集为空、校验拦截、发布事务失败、`Commit` 成功后无第二次收口调用

- [ ] 11. 装配、门禁与端到端检查点
  - `config.Config.UpstreamBaseURL`（`UPSTREAM_BASE_URL`，缺省 `http://zhjw.qfnu.edu.cn`）+ `main.go` 装配 `upstream.New` → `collector.New` → `runner.Deps.Collect`，启动日志 `collector_wired=true`
  - `backend/.golangci.yml` 补 `collector` 包规则；`go.mod` 不变
  - 三条编译期断言：`var _ collector.RunLifecycle = store.CollectRunRepo{}`、`var _ collector.TermCalendar = store.TermRepo{}`、`var _ collector.SnapshotReader = store.SnapshotRepo{}`
  - e2e（真实库 + 假上游，`e2e/collect_runner_test.go`）：`Start` → 202 → 轮次 `success` → 公开查询可见；归属冲突轮只含赢家房号；续跑轮 hydrate 后发布完整
  - 门禁：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`、`go test -p 1 -count=1 ./...`（带 `KJS_TEST_DSN`）、`golangci-lint run`、`go test -race -count=1 ./internal/collector ./internal/pipeline` 全部 0 退出
