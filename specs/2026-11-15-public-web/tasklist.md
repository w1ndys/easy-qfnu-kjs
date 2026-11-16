# public-web 实施计划

> 每项都能独立实现与验收，顺序即依赖顺序：1–2 是工程与请求底座，3–6 是公开查询页，7–9 是数据大屏，10 是响应式/可访问性收口与门禁。
> 本 spec 只写前端：不改 `backend/`、不新增接口、不动 `compose.yaml` 与根 `Dockerfile.*`（编排归 `deploy-ops`）。
> 每项完成后跑该项涉及的 `pnpm test -- --run` 与 `pnpm exec tsc --noEmit`；第 10 项跑 `task gate`。
> 验收以 [requirements.md](requirements.md) 的条目为准，实现细节以 [design.md](design.md) 为准；以下条目在实现阶段执行。

- [x] 1. 前端工程骨架与工具链
  - `frontend/` 建 Vite + React 19 + TypeScript + antd 6.7 工程：`package.json`（含 `packageManager` 钉 pnpm）、`vite.config.ts`（`@vitejs/plugin-react`、`/api` 代理 `http://127.0.0.1:18080`、vitest `jsdom` + `setup.ts`）、`tsconfig.json`（strict）、`eslint.config.js`、`index.html`（`lang="zh-CN"`）
  - 脚本与根 `Taskfile.yml` 的 `frontend:install/dev/build/test/lint/fmt` 对齐：`dev`、`build`（含 `tsc --noEmit`）、`preview`、`test`、`lint`、`type-check`、`format`
  - `src/main.tsx` + `src/App.tsx`：`ConfigProvider`（品牌色 `#884F22`）、`createBrowserRouter` 四条路由（`/`、`/day/:roomId`、`/screen`、`*`）、根 `ErrorBoundary`
  - 验收：`pnpm build` 产出 `frontend/dist`；故意抛错的临时路由被错误页接住而不是白屏；`task frontend:lint` 零错误

- [x] 2. API 客户端与错误映射
  - `src/api/client.ts`：`fetch` + `AbortSignal` + 15s 超时，解析信封 `{code, message, data, request_id}`，非 JSON 与 HTTP 非 2xx 都走同一失败路径，`code !== 0` 抛 `ApiError{code, requestId, retriable}`
  - `src/api/errors.ts`：只映射后端已有码 `40001`/`40401`/`42901`/`50000`，未知码兜底「服务异常」，`message` 仅作副标题
  - `src/api/types.ts` + `src/api/public.ts`：五个薄函数（`getContext`、`getMeta`、`queryAvailability`、`queryDay`、`getScreen`），`date_offset`/`room_id`/`start_node` 等参数名显式写出，不做自动大小写转换
  - 验收：单测覆盖成功、`code` 非 0、非 JSON、网络中断、`AbortError` 五条路径；`AbortError` 不进入错误态；错误对象带 `request_id`

- [x] 3. 公开查询页输入区与默认值
  - `useMeta` 一次加载 `/api/context` + `/api/meta`，未完成时显示加载态；`useQueryFilters` 把 `date_offset`/`building`/`start_node`/`end_node`/`keyword`/`page` 读写到 URL
  - `FilterPanel`：11 个日期项（偏移 0..10，显示「今天/明天/M-D」）、教学楼下拉（首位「全部」+ 白名单）、节次区间、关键词、查询按钮；回车与点击发同一请求
  - `lib/nodes.ts` 三档预填：<12:00 → 01–05，12:00–18:00 → 06–09，≥18:00 → 10–12；选非当天日期时沿用当前时段
  - 验收：边界时刻（11:59/12:00/17:59/18:00）单测对齐；URL 刷新后请求参数与刷新前一致；楼下拉项数与夹具白名单一致

- [x] 4. 空教室结果区
  - `ResultList` 每行显示房间名、房间标识、所属教学楼；同名房名追加房间标识后缀（`lib/roomName.ts`）
  - 分页用 `Pagination` 驱动 `limit`/`offset` 并展示 `total`；改楼/关键词/日期时回到第一页
  - 每行提供「看全天状态」入口，跳 `/day/:roomId` 并带上当前 `date_offset` 与 `building`
  - 验收：夹具两间同名房渲染出不同标签；翻页请求参数正确；结果顺序与夹具顺序一致（前端不重排）

- [x] 5. 全天状态视图
  - `DayPage` 从 `roomId` + `searchParams` 请求 `view=day`，`NodeGrid` 用 `meta.axis.nodes` 顺序渲染 12 个小节
  - `StatusCell` 同时用文字、图标与颜色表达状态；`has_data=false` 显示「无数据」且呈现为不可用样式
  - 房间头显示房名/房号/楼名/日期（周次与星期取自响应 `week`/`weekday`；缺 `roomId` 时显示空态并给回查询页入口）
  - 验收：夹具中 3 个缺失节点渲染为「无数据」且断言无「空闲」文本；缺 `roomId` 路由显示空态；状态名来自字典而非硬编码

- [x] 6. 加载、空、过期与错误四态
  - `StateViews.tsx`：`LoadingView`（Skeleton，不遮筛选）、`EmptyView`（按 `hint.code` 四档文案 + 一条可操作建议）、`StaleBanner`（`stale` 或 `in_calendar=false`）、`ErrorView`（分类文案 + 重试 + `request_id`）
  - 过期判定：查询响应的 `week` 命中 `meta.weeks[].week` 且 `stale=true`；`week_no_schedule` 用置顶提示且结果照常展示
  - `PageFooter` 展示数据来源、更新频率与仅供参考说明；`42901` 时按钮禁用 3 秒
  - 验收：四档 hint 各一条用例；过期时数据仍可见；错误态不残留上一次列表；`aria-live` 区域在结果变化时文本更新

- [x] 7. 数据大屏布局与信息区块
  - `ScreenLayout`：暗色（`darkAlgorithm`）、无导航、1920×1080 设计稿 + `lib/scale.ts` 计算 `scale(min(vw/1920, vh/1080))`，`transform-origin: top left` 居中留边
  - `ScaleBlock`（房间数/教学楼数/学期/当前周/采集窗口）、`ReleaseBlock`（版本标识 + 生成时间，`Asia/Shanghai`）、`WeekFreshnessTable`（逐周发布/最近成功/过期）、`RunBlock`（最近一轮状态/开始时间/耗时）、`AnomalyBlock`（未知/复合/归属冲突）
  - `empty=true` 时各区块保留标题并显示「暂无数据」；页面不渲染任何业务码、`message` 或错误原文
  - 验收：窗口从 1920×1080 变 2560×1440 / 1280×1024 时区块顺序与相对位置不变（缩放系数单测 + 一条渲染用例）；断言敏感串（错误码、`error_code`）不出现

- [x] 8. 占用热力图组件
  - `OccupancyHeatmap`：自绘 SVG，7 行（周一…周日）× 5 列（大节，列头由 `meta.axis` 派生为「第 01–02 节」），入场 `occupancy.by_weekday_block`，`rate` 为 0 用中性底色
  - 图例 5 档（0/25/50/75/100%）+ 说明「未知与复合占用计入已占用」；`role="img"` + `aria-label` 概述，每格 `<title>` 可读文本，悬停 `Tooltip`
  - `BuildingOccupancyList` 用 `by_building` 显示楼名、占用率与已占/总格数
  - 验收：夹具 7×5 矩阵渲染 35 个格；色阶边界（0、0.5、1）单测；无 `meta` 时列头回退为「大节 1..5」而不报错

- [x] 9. 大屏轮询与降级
  - `useScreenPolling`：可见时每 30 秒请求 `/api/screen`；请求在途时保留旧数据；失败置离线标记并记录最后成功时间，下次成功清除
  - `visibilitychange` 隐藏时暂停、重新可见时立即请求一次；轮询不读写任何前端 TTL 缓存
  - 首屏失败显示错误占位 + 重试（与 `empty` 空态区分）；顶栏显示最后刷新时间（`Asia/Shanghai`）
  - 验收：`vi.useFakeTimers()` 推进 30 秒断言多一次请求；失败一轮后旧数据仍在且出现离线徽标；隐藏页面后推进 60 秒断言无新请求

- [ ] 10. 响应式、可访问性收口与门禁
  - 公开页三档布局：<600px 单列 + 底部固定查询按钮；600–1024px 双列；≥1024px 三列且筛选与结果同屏；触控目标 ≥44×44
  - 可访问性：`aria-live` 播报结果数量与空态、状态格有可读名称、查询后焦点留在触发控件、`prefers-reduced-motion` 关闭过渡
  - 测试补齐：MSW 夹具与 `types.ts` 用 `satisfies` 守卫形状；组件测试覆盖四态、分页、轮询与热力图；不写快照
  - 验收：`task frontend:lint`、`task frontend:test`、`task frontend:build` 全绿，`task gate` 通过；产物无 `console.log`，代码无 `any`
