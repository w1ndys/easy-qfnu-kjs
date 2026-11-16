import { ConfigProvider } from "antd";
import zhCN from "antd/locale/zh_CN";
import type { ReactNode } from "react";
import { useMemo } from "react";
import {
  createBrowserRouter,
  Outlet,
  RouterProvider,
  type RouteObject,
} from "react-router";
import { RouteErrorPage } from "./components/RouteErrorPage";
import { useReducedMotion } from "./hooks/useReducedMotion";
import { ScreenLayout } from "./layouts/ScreenLayout";
import { DayPage } from "./pages/public/DayPage";
import { NotFoundPage } from "./pages/public/NotFoundPage";
import { QueryPage } from "./pages/public/QueryPage";
import { ScreenPage } from "./pages/public/ScreenPage";
import { appTheme } from "./theme";

// 根路由只负责错误边界与共享壳；公开查询页的响应式壳落在页面内（第 10 项），
// 大屏的暗色等比缩放壳 ScreenLayout 由第 7 项接在 /screen 上。
export const createAppRoutes = (
  extraChildren: RouteObject[] = [],
): RouteObject[] => [
  {
    path: "/",
    element: <Outlet />,
    errorElement: <RouteErrorPage />,
    children: [
      { index: true, element: <QueryPage /> },
      // roomId 可选：链接缺房号时也要落到 DayPage 的空态（Requirement 4.4），
      // 而不是掉进下面的 `*` 变成 404。
      { path: "day/:roomId?", element: <DayPage /> },
      {
        path: "screen",
        element: (
          <ScreenLayout>
            <ScreenPage />
          </ScreenLayout>
        ),
      },
      { path: "*", element: <NotFoundPage /> },
      ...extraChildren,
    ],
  },
];

export function App() {
  const router = useMemo(() => createBrowserRouter(createAppRoutes()), []);

  return (
    <AppThemeProvider>
      <RouterProvider router={router} />
    </AppThemeProvider>
  );
}

/**
 * App 的主题壳：系统要求减少动态效果时关掉 antd 的过渡/动画
 * （第 10 项；CSS 兜底见 global.css）。
 *
 * 单独抽成组件是为了让测试能渲染真实接线（matchMedia → ConfigProvider 的
 * `token.motion`），而不是在测试里重写一遍主题合并逻辑。
 */
export function AppThemeProvider({ children }: { children: ReactNode }) {
  const reducedMotion = useReducedMotion();
  const theme = useMemo(
    () => ({
      ...appTheme,
      token: { ...appTheme.token, motion: !reducedMotion },
    }),
    [reducedMotion],
  );

  return (
    <ConfigProvider locale={zhCN} theme={theme}>
      {children}
    </ConfigProvider>
  );
}
