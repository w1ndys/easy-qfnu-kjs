# admin-release 实施计划

> 每项都能独立实现与验收，顺序即依赖顺序：1–4 是读取与对比底座，5–6 是回退与异常，7–8 是 HTTP 与端到端，9 是门禁。
> 每项完成后跑该项涉及包的 `go test -count=1`；第 9 项跑全量门禁。带库用例需要 `KJS_TEST_DSN`，且必须 `-p 1` 串行（`e2e` 与 `store` 都会 `DROP SCHEMA public`）。
> 前置：`collect-pipeline`（发布事务与 `release` / `observation` 数据）、`admin-bootstrap`（会话 / CSRF / 错误码）、`admin-settings-sync`（分页与响应契约）、`collect-runner`（真实发布数据与 `collect_run_room`）。
> 本 spec 不写任何 Go 代码，以下条目在实现阶段执行。

- [ ] 1. 版本读取仓储：列表与详情
  - `store.ReleaseAdminRepo.List`：`release` 按 `generated_at DESC, release_id DESC`；`released_at` 用 `collect_run` 的 `max(released_at)` 左连接，没有关联轮次为 `NULL`；`week_count` / `observation_rows` / `rooms` 各由一个 LATERAL 聚合
  - `store.ReleaseAdminRepo.Detail`：在 `REPEATABLE READ READ ONLY` 事务里读头（不带观测聚合）、逐周计数（`count(DISTINCT (room_id, weekday, block))`）与按楼计数；`stats` 与 `release.week_count` / `release.observation_rows` 由逐周结果拼出，不额外发聚合；不存在返回 `(zero, false, nil)`
  - 空库列表返回空切片与 `total=0`；`week_counts` / `building_counts` 的切片恒非 nil（JSON 输出 `[]`）
  - 集成（真实库）：两版夹具断言排序、`released_at`、计数与手工折叠一致；删除全部版本后列表为空

- [ ] 2. 大节折叠与对比汇总
  - 折叠 CTE：按各版 `axis_version` join `axis_node`，`min(state_key)` 作为格状态、`max(state_key) <> min` 计入 `inconsistent_blocks`；键为 `(room_id, week, weekday, block)`
  - 汇总一条语句给出 `cells`（added/removed/changed/total）、`rooms`（added/removed/shared/building_changed）、`unknown`/`composite` 两侧计数与 `delta`、`same_term`；两侧版本头在同一只读事务内先解析，缺一版返回 `ErrReleaseNotFound`
  - 夹具覆盖五类差异（新增格 / 消失格 / 状态变化 / 未变格 / 归属变化）与一个块内不一致的格，逐项断言计数；交换 `from` / `to` 断言 added↔removed、changed 不变
  - 不同 `term` 的两版返回 `same_term=false` 且照常给出差异；`axis_version` 不同仍按 `block` 编码比较

- [ ] 3. 对比明细分页
  - `Diff` 按 `section`（`cells` / `rooms` / `buildings`）返回对应小节；`cells` 支持 `kind=all|added|removed|changed`，`rooms` 支持 `all|added|removed`，`buildings` 只接受 `all|changed`
  - 排序键固定（cells：`room_id, week, weekday, block`；rooms / buildings：`room_id`），`limit` 1–200 默认 50、`offset ≥ 0`；`total` 从同一快照的汇总推导，不在分页语句里再 `count(*)`
  - `offset` 超过末页返回空 `items` 且 `total` 不变；同一请求重复执行返回同一页
  - 集成：逐小节断言条数与字段（含 `room_name`、归属字段的 `null` 形状）；相邻页无重复、无遗漏

- [ ] 4. 回退事务
  - `store.ReleaseAdminRepo.Rollback`：`BEGIN` → `SELECT ... WHERE is_current FOR UPDATE` → `SELECT ... WHERE release_id = $1 FOR UPDATE` → Go 侧判定目标存在 / 非 current / 同 `term` / 有当前 → 先降后抬，目标行数必须为 1 → `COMMIT`
  - 错误映射：缺目标 → `ErrReleaseNotFound`（40401）；已 current / 无 current → `ErrReleaseNotRollbackable`（40904）；跨学期 → `ErrReleaseTermMismatch`（40905）；SQLSTATE `23505` / `40P01` / `40001` → `ErrReleaseConflict`（40906）；其余包装 `model.ErrQueryFailed`
  - 集成：成功后 current 交换、`release` / `observation` / `release_room_building` 行数不变、`collect_run` 的 `release_id` / `released_at` 不变；三种拒绝分支各一条，断言 `release` 表零变更
  - 并发：两个 goroutine 同时回退同一目标，恰好一个成功、另一个 40904 或 40906，结束后恰有一个 current

- [ ] 5. 公开查询缓存失效钩子
  - `query/ttlCache` 增加 `clear()`；`query.Service.Invalidate()` 清空空教室与全天状态两张表；`query.ScreenService.Invalidate()` 清空大屏表
  - `api.WithReleaseAdmin(reader, invalidate func())`：`reader` 为 nil 时五个端点按 50000；`invalidate` 为 nil 时回退照常成功、跳过失效；回退**提交成功后、写响应前**调用，失败路径零调用
  - 单测：预热同键 → `Invalidate` → 同键重新查库（替身被第二次调用）；screen 同理
  - `cmd/server/main.go` 装配 `store.NewReleaseAdminRepo(pool)` 与 `func(){ queries.Invalidate(); screenService.Invalidate() }`，并加编译期断言 `var _ api.ReleaseAdminReader = store.ReleaseAdminRepo{}`

- [ ] 6. 异常数据视图
  - `store.ReleaseAdminRepo.Anomalies`：逐版折叠出 `block_cells` / `observation_rows` / `unknown` / `composite` / `observed_weeks`，`window` 取该版最新发布轮次的 `window_start` / `window_end`
  - 纯函数：`weekGaps(observedWeeks)`（`[min,max]` 内缺周）与 `ratioOf(cells, blockCells)`（4 位小数，分母 0 记 0）；单测覆盖空、单周、缺中间周、四舍五入边界（`releaseIDValid` 由第 7 项实现与测试）
  - 冲突清单：`collect_run_room` 按 `run_id` 取该版最新一轮的 `conflict=true` 行，每版最多 200 条，出现第 201 条时 `conflict_rooms_truncated=true`
  - 集成：未知/复合按大节计数与手工数一致、`ratio` 4 位小数、`week_gaps` 命中缺周、冲突清单与截断断言、无版本时 `releases` 为 `[]`

- [ ] 7. HTTP 端点、参数门与契约
  - 路由：`GET /api/admin/releases`、`GET /api/admin/releases/diff`、`GET /api/admin/releases/{release_id}`、`POST /api/admin/releases/{release_id}/rollback`（`adminWriteNoBody`，无请求体）、`GET /api/admin/anomalies`；读端点走 `adminRead`
  - 参数门在查库前：`release_id` 形状 `^\d{8}T\d{6}Z$`、`from`/`to` 必填且不等、`section` / `kind` 组合、`limit` 1–200、`offset ≥ 0`；任一非法 400 + 40001 且替身零调用
  - DTO 形状：`released_at` / `from_state` / `to_state` / 归属字段用指针输出 `null`；`items` 空为 `[]`；时间 RFC3339 UTC；`data` 键集合与 design 一致
  - 契约测试：无会话 40101、缺 CSRF 40301、40401 / 40904 / 40905 / 40906 / 50000 各一条、所有响应（含错误）带 `no-store`、日志只有五字段白名单、`invalidate` 只在回退成功时调用一次

- [ ] 8. 端到端检查点
  - `e2e/admin_release_test.go`（真实库 + `httptest`）：init → login → 种子两版（含归属冲突与缺周）→ 列表与详情 → 对比三类差异与分页 → 回退
  - 回退后同一进程内先预热 `/api/query` 缓存、再回退、再用同参数查询，断言读到新 current 的结果（不等 30 秒）；`/api/screen` 的版本标识同步切换
  - 列表仍为两行、`is_current` 恰好一条且指向回退目标、`collect_run` 未变；响应带 `Cache-Control: no-store`

- [ ] 9. 门禁、性能实测与文档回填
  - 性能：用 `COPY` 造两版各 87 万观测行的夹具，实测「汇总 + 首页」与 `section=rooms` 的耗时，把结果记回 `design.md` 的「预算」段；超 2 秒按预算段的备选方案合并语句
  - 门禁：`cd backend && go build ./... && go vet ./... && go test -count=1 ./... && go test -p 1 -count=1 ./...`（带 `KJS_TEST_DSN`）、`golangci-lint run`、`go test -race -count=1 ./internal/api ./internal/store ./internal/query` 全部 0 退出
  - `go.mod` 不变（不新增依赖）；`backend/.golangci.yml` 不新增包规则（D1 不新建包）
  - 实现完成后把 `specs/2026-11-03-easy-qfnu-kjs/roadmap.md` 的 `admin-release` 行勾选并补本 spec 目录名（交付时做，不在本项提前改）
