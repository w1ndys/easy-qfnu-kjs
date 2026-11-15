# frontend

`public-web` 的前端工程骨架：Vite 8 + React 19 + TypeScript（strict）+ antd 6.7，路由用 `react-router` 8。

请求层（第 2 项）与公开查询页（第 3–4 项）已落地：上下文/元数据加载、URL 驱动的筛选参数、
日期/教学楼/节次区间/关键词筛选与查询请求，以及结果列表（同名房名后缀 + 全天状态入口）与
`limit`/`offset` 分页；加载/空/过期/错误四态与全天状态页在后续项实现。

## 命令

```bash
pnpm install        # 等同根目录 task frontend:install
pnpm dev            # 开发服务器，/api 代理到 http://127.0.0.1:18080
pnpm build          # tsc --noEmit && vite build，产物在 frontend/dist/
pnpm preview        # 预览 dist/（同样代理 /api）
pnpm test           # vitest run：一次性跑完就退出（CI 与门禁口径）
pnpm test:watch     # vitest watch：交互模式，改文件自动重跑
pnpm lint           # eslint .
pnpm type-check     # tsc --noEmit
pnpm format         # prettier --write .
```

`test` 是 `vitest run` 而不是裸 `vitest`：pnpm 会把 `pnpm test -- --run` 的 `--`
原样透传给 vitest，watch 形态在交互终端里不会自己退出。需要交互就跑 `test:watch`。

根 `Taskfile.yml` 的 `frontend:install/dev/build/test/lint/fmt` 与上面这些脚本一一对应。

## antd 6.7 官方 MCP server

antd 按 `ai-kit/docs/dev-conventions/stacks/react-frontend.md` 作为基础组件层，并启用官方 MCP server
（组件文档、属性签名、design token 直接查，不靠记忆猜）。客户端配置里加：

```json
{
  "mcpServers": {
    "antd": {
      "command": "npx",
      "args": ["-y", "@ant-design/cli", "mcp", "--version", "6.7.0"]
    }
  }
}
```

已全局安装 CLI 的机器可把 `command`/`args` 换成 `antd mcp`。官方文档：<https://ant.design/docs/react/mcp>。

## 结果区口径：同名判定只看当前页

`src/lib/roomName.ts` 的「同名」判定范围是**当前这一页**，不是整次查询的全部命中。这是明确的实现口径，不是将就：

- 后端 `backend/internal/store/availability.go` 的分页 SQL 按 `ORDER BY r.name, o.room_id` 取页，全量结果里同名房间一定相邻；只有当同名组正好横跨分页边界时，一组同名房才会被切到两页。
- 前端只持有当前页数据。跨页判定要么额外多拉相邻页、要么先拉全量再在前端分页，代价与收益不成比例；后端排序已经保证了页内可见的同名覆盖绝大多数情况。
- 已知盲区：同名组被分页边界切开时，两页里的那两行会各自以不带后缀的同一名称出现。要消除得让接口返回同名组信息（或按组对齐分页），不能在前端猜。

## 结构约定

- `src/theme/`：antd 主题 token，品牌色 `#884F22`；大屏 `darkAlgorithm` 在第 7 项叠加。
- `src/api/`：请求层（信封解析、业务码文案、五个端点薄函数）。
- `src/hooks/`：`useApi`（取消/重载）、`useMeta`（上下文 + 元数据并行加载一次）、`useQueryFilters`（URL 唯一读写点）。
- `src/lib/`：纯函数；`nodes.ts` 是节次三档预填、`dateOffset.ts` 是日期项文案、`roomName.ts` 是同名房名后缀。
- `src/components/`：共享组件；`FilterPanel.tsx` 是公开查询页输入区、`ResultList.tsx` 是结果列表，`RouteErrorPage.tsx` 是根路由 `errorElement`。
- `src/pages/public/`：四个页面路由已挂；`/` 已落输入区、结果列表与分页，全天状态页与四态在后续项实现。
- `src/test/fixtures/`：与 `src/api/types.ts` 用 `satisfies` 对齐的响应夹具。
- `src/test/setup.ts`：jest-dom、`matchMedia`、`ResizeObserver` 垫片。
