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
import { DayPage } from "./pages/public/DayPage";
import { NotFoundPage } from "./pages/public/NotFoundPage";
import { QueryPage } from "./pages/public/QueryPage";
import { ScreenPage } from "./pages/public/ScreenPage";
import { appTheme } from "./theme";

// 根路由只负责错误边界与共享壳；PublicLayout / ScreenLayout 分别在第 3 项与
// 第 7 项替换掉这里的 <Outlet />。
export const createAppRoutes = (
  extraChildren: RouteObject[] = [],
): RouteObject[] => [
  {
    path: "/",
    element: <Outlet />,
    errorElement: <RouteErrorPage />,
    children: [
      { index: true, element: <QueryPage /> },
      { path: "day/:roomId", element: <DayPage /> },
      { path: "screen", element: <ScreenPage /> },
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
