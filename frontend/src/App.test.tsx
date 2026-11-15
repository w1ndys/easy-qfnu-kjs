import { render, screen } from "@testing-library/react";
import {
  createMemoryRouter,
  RouterProvider,
  type RouteObject,
} from "react-router";
import { describe, expect, it, vi } from "vitest";
import { createAppRoutes } from "./App";

function renderAt(path: string, extraChildren: RouteObject[] = []) {
  const router = createMemoryRouter(createAppRoutes(extraChildren), {
    initialEntries: [path],
  });

  return render(<RouterProvider router={router} />);
}

// 故意在渲染期抛错的路由：只存在于测试里，用来验证根 errorElement 能把
// 崩溃接管成错误页，而不是留下一张白屏。
function ExplodingRoute(): never {
  throw new Error("骨架期故意抛错的路由");
}

describe("App 路由表", () => {
  it("根路径渲染空教室查询占位页", () => {
    renderAt("/");

    expect(screen.getByText("空教室查询")).toBeInTheDocument();
  });

  it("全天状态路由能从路径取到房间标识", () => {
    renderAt("/day/Q10-101");

    expect(screen.getByText(/Q10-101/)).toBeInTheDocument();
  });

  it("数据大屏路由可访问", () => {
    renderAt("/screen");

    expect(screen.getByText("数据大屏")).toBeInTheDocument();
  });

  it("未匹配的路径落到 404 页", () => {
    renderAt("/no-such-page");

    expect(screen.getByText("页面不存在")).toBeInTheDocument();
  });

  it("路由渲染抛错时由根错误边界接管，不出现白屏", async () => {
    // React 开发态会为被边界接住的错误打 console.error，测试里静音避免日志噪音。
    // try/finally 兜住断言失败：否则 mock 泄漏到后续用例，调试日志也跟着被吞。
    const consoleError = vi
      .spyOn(console, "error")
      .mockImplementation(() => {});

    try {
      const { container } = renderAt("/boom", [
        { path: "boom", element: <ExplodingRoute /> },
      ]);

      expect(await screen.findByText("页面出错了")).toBeInTheDocument();
      expect(container).not.toBeEmptyDOMElement();
      // 原始异常信息不上页面（与 Requirement 7.6 同一口径）。
      expect(
        screen.queryByText(/骨架期故意抛错的路由/),
      ).not.toBeInTheDocument();
    } finally {
      consoleError.mockRestore();
    }
  });
});
