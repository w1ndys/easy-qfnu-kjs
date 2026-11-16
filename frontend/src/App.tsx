import { ConfigProvider } from "antd";
import zhCN from "antd/locale/zh_CN";
import { useMemo } from "react";
import {
  createBrowserRouter,
  Outlet,
  RouterProvider,
  type RouteObject,
} from "react-router";
import { RouteErrorPage } from "./components/RouteErrorPage";
import { ScreenLayout } from "./layouts/ScreenLayout";
import { DayPage } from "./pages/public/DayPage";
import { NotFoundPage } from "./pages/public/NotFoundPage";
import { QueryPage } from "./pages/public/QueryPage";
import { ScreenPage } from "./pages/public/ScreenPage";
import { appTheme } from "./theme";

// 根路由只负责错误边界与共享壳；公开查询页的 PublicLayout 留给第 10 项，
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
    <ConfigProvider locale={zhCN} theme={appTheme}>
      <RouterProvider router={router} />
    </ConfigProvider>
  );
}
