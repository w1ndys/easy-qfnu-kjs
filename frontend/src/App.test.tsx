import { render, screen } from "@testing-library/react";
import {
  createMemoryRouter,
  RouterProvider,
  type RouteObject,
} from "react-router";
import { describe, expect, it, vi } from "vitest";
import { createAppRoutes } from "./App";
import {
  contextFixture,
  dayFixture,
  metaFixture,
} from "./test/fixtures/publicApi";

function jsonResponse(data: unknown): Response {
  return new Response(
    JSON.stringify({
      code: 0,
      message: "success",
      data,
      request_id: "req_app_test",
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

function renderAt(path: string, extraChildren: RouteObject[] = []) {
  const router = createMemoryRouter(createAppRoutes(extraChildren), {
    initialEntries: [path],
  });

  return render(<RouterProvider router={router} />);
}

/**
 * 给三个公开端点垫片，并把每次请求的 URL 记下来供断言。
 * 第 5 项起 DayPage 会先取上下文/元数据再取全天状态，路由测试也必须给端点垫片。
 */
function stubPublicApi(): URL[] {
  const requests: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://localhost");
      requests.push(url);
      if (url.pathname === "/api/context") {
        return jsonResponse(contextFixture);
      }
      if (url.pathname === "/api/meta") {
        return jsonResponse(metaFixture);
      }
      if (url.pathname === "/api/query") {
        return jsonResponse(dayFixture);
      }
      throw new Error(`测试未处理的请求：${url.pathname}`);
    }),
  );
  return requests;
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

  it("全天状态路由把路径里的房间标识带进 /api/query 请求", async () => {
    const requests = stubPublicApi();

    try {
      renderAt("/day/Q12-303?date_offset=2&building=Q12");

      // 先等网格出现：这说明上下文/元数据与全天状态两路都已落地，再看请求参数才有意义。
      expect(
        await screen.findByRole("list", { name: "全天状态小节" }),
      ).toBeInTheDocument();

      const dayRequests = requests.filter(
        (url) => url.pathname === "/api/query",
      );
      expect(dayRequests).toHaveLength(1);
      // 断言的是请求参数而不是页面上随便一处字符串：room_id 必须来自路径段。
      expect(dayRequests[0].searchParams.get("view")).toBe("day");
      expect(dayRequests[0].searchParams.get("room_id")).toBe("Q12-303");
      expect(dayRequests[0].searchParams.get("date_offset")).toBe("2");
      expect(dayRequests[0].searchParams.get("building")).toBe("Q12");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("缺房号的 /day 落到 DayPage 空态，而不是被 * 路由接成 404", async () => {
    const requests = stubPublicApi();

    try {
      renderAt("/day");

      expect(await screen.findByText("缺少房间标识")).toBeInTheDocument();
      expect(
        screen.getByRole("link", { name: "返回空教室查询" }),
      ).toHaveAttribute("href", "/");
      expect(screen.queryByText("页面不存在")).not.toBeInTheDocument();
      // 没有房号就不该发全天状态请求（元数据的两个请求不算）。
      expect(
        requests.filter((url) => url.pathname === "/api/query"),
      ).toHaveLength(0);
    } finally {
      vi.unstubAllGlobals();
    }
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
