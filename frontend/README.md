# frontend

`public-web` 的前端工程骨架：Vite 8 + React 19 + TypeScript（strict）+ antd 6.7，路由用 `react-router` 8。

页面与请求层从第 2 项起逐项实现；本目录当前只有可运行的路由骨架、主题与错误边界。

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

## 结构约定

- `src/theme/`：antd 主题 token，品牌色 `#884F22`；大屏 `darkAlgorithm` 在第 7 项叠加。
- `src/api/`、`src/hooks/`、`src/lib/`：第 2 项起的请求层与纯函数。
- `src/components/`：共享组件；`RouteErrorPage.tsx` 是根路由 `errorElement`。
- `src/pages/public/`：四个页面，本项只有占位内容。
- `src/test/setup.ts`：jest-dom、`matchMedia`、`ResizeObserver` 垫片。
