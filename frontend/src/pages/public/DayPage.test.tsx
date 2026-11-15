import { render, screen, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { createAppRoutes } from "../../App";
import type { ContextInfo, DayResult, MetaResult } from "../../api/types";
import {
  contextFixture,
  dayFixture,
  metaFixture,
  noAxisMetaFixture,
  unorderedAxisMetaFixture,
} from "../../test/fixtures/publicApi";

function envelope(data: unknown, requestId: string): Response {
  return new Response(
    JSON.stringify({
      code: 0,
      message: "success",
      data,
      request_id: requestId,
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

interface StubOptions {
  /** 覆盖 /api/query?view=day 的响应。 */
  day?: DayResult;
  /** 覆盖 /api/meta 的响应（用来模拟字典升级）。 */
  meta?: MetaResult;
  context?: ContextInfo;
}

function stubPublicApi(options: StubOptions = {}) {
  const requests: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://localhost");
      requests.push(url);
      return respond(url, options);
    }),
  );

  return {
    requests,
    dayRequests: () => requests.filter((url) => url.pathname === "/api/query"),
  };
}

function respond(url: URL, options: StubOptions): Response {
  if (url.pathname === "/api/context") {
    return envelope(options.context ?? contextFixture, "req_context");
  }
  if (url.pathname === "/api/meta") {
    return envelope(options.meta ?? metaFixture, "req_meta");
  }
  if (url.pathname === "/api/query") {
    return envelope(options.day ?? dayFixture, "req_day");
  }
  throw new Error(`测试未处理的请求：${url.pathname}`);
}

function renderDayPage(initialEntry: string) {
  // 走应用真实路由表（App.tsx 的 createAppRoutes）而不是测试自建的路由表：
  // 「缺房号」空态只有在 `day/:roomId?` 可选段路由真存在时才可达，
  // 用自建的两条路由来测会把「真实应用里落到 404」这个回归掩盖掉。
  const router = createMemoryRouter(createAppRoutes(), {
    initialEntries: [initialEntry],
  });
  const view = render(<RouterProvider router={router} />);
  return { router, ...view };
}

/** 等网格出现：元数据与全天状态两路都到齐后才会渲染 12 格。 */
async function waitForGrid(): Promise<HTMLElement> {
  return screen.findByRole("list", { name: "全天状态小节" });
}

/** 按可读名称定位某一小节的状态格（StatusCell 的 aria-label 以「第 NN 节」开头）。 */
function cellByNode(grid: HTMLElement, node: string): HTMLElement {
  return within(grid).getByRole("listitem", {
    name: new RegExp(`^第 ${node} 节`),
  });
}

/** 取第一条 view=day 请求；没有就让断言失败，而不是给出 undefined。 */
function firstDayRequest(stub: ReturnType<typeof stubPublicApi>): URL {
  const [request] = stub.dayRequests();
  if (!request) {
    throw new Error("没有发出 view=day 请求");
  }
  return request;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("请求参数", () => {
  it("从 roomId 与 searchParams 请求 view=day，不带空教室的筛选与分页参数", async () => {
    const stub = stubPublicApi();
    renderDayPage("/day/Q10-101?date_offset=3&building=Q10");

    await waitForGrid();

    const request = firstDayRequest(stub);
    expect(request.searchParams.get("view")).toBe("day");
    expect(request.searchParams.get("room_id")).toBe("Q10-101");
    expect(request.searchParams.get("date_offset")).toBe("3");
    expect(request.searchParams.get("building")).toBe("Q10");
    // day 视图不接受空教室的 keyword / 分页参数：一个都不该出现。
    expect(request.searchParams.get("keyword")).toBeNull();
    expect(request.searchParams.get("limit")).toBeNull();
    expect(request.searchParams.get("offset")).toBeNull();
  });

  it("缺省 date_offset 取 0，楼为空时不带 building 参数", async () => {
    const stub = stubPublicApi();
    renderDayPage("/day/Q10-101");

    await waitForGrid();

    const request = firstDayRequest(stub);
    expect(request.searchParams.get("date_offset")).toBe("0");
    expect(request.searchParams.get("building")).toBeNull();
    expect(request.searchParams.get("room_id")).toBe("Q10-101");
  });

  it("date_offset 非法或越界时回退 0，不把非法参数发给服务端", async () => {
    const stub = stubPublicApi();
    renderDayPage("/day/Q10-101?date_offset=99");

    await waitForGrid();

    expect(firstDayRequest(stub).searchParams.get("date_offset")).toBe("0");
  });
});

describe("房间头", () => {
  it("显示房名、房间标识、楼名、日期，以及响应里的周次与星期", async () => {
    // 刻意让响应与日期对不上：2026-11-18 本身是周三，响应却给 weekday=5。
    // 页面必须显示「周五」——星期只能来自响应字段，按日期反推就是前端在自行推算。
    stubPublicApi({
      day: { ...dayFixture, weekday: 5 },
    });
    renderDayPage("/day/Q10-101");

    expect(await screen.findByText("综合楼101")).toBeInTheDocument();
    expect(screen.getByText("房间标识：Q10-101")).toBeInTheDocument();
    expect(screen.getByText("教学楼：综合教学楼")).toBeInTheDocument();
    expect(screen.getByText("日期：2026-11-18")).toBeInTheDocument();
    expect(screen.getByText("第 11 周 · 周五")).toBeInTheDocument();
    expect(screen.queryByText("第 11 周 · 周三")).not.toBeInTheDocument();
  });

  it("week>0 但 weekday 不在 1..7 时回落为「第 N 周」", async () => {
    stubPublicApi({
      day: { ...dayFixture, week: 12, weekday: 9 },
    });
    renderDayPage("/day/Q10-101");

    expect(await screen.findByText("第 12 周")).toBeInTheDocument();
    // 非法 weekday 不拼出任何星期名，也不让房间头崩掉。
    expect(screen.queryByText(/第 12 周 · /)).not.toBeInTheDocument();
    expect(screen.getByText("房间标识：Q10-101")).toBeInTheDocument();
  });

  it("不在教学周（week=0）时周次位置显示「非教学周」", async () => {
    stubPublicApi({
      day: { ...dayFixture, week: 0, weekday: 0 },
    });
    renderDayPage("/day/Q10-101");

    expect(await screen.findByText("非教学周")).toBeInTheDocument();
    // 房间信息照常保留，不因非教学周隐藏。
    expect(screen.getByText("房间标识：Q10-101")).toBeInTheDocument();
  });
});

describe("12 小节网格", () => {
  it("按 meta.axis 的 ordinal 顺序渲染 12 个小节，而不是数组顺序", async () => {
    // 对抗性轴夹具把 02 与 05 的 ordinal 对调：排序结果应为 01,05,03,04,02,06…12。
    stubPublicApi({ meta: unorderedAxisMetaFixture });
    renderDayPage("/day/Q10-101");

    const grid = await waitForGrid();
    const cells = within(grid).getAllByRole("listitem");

    expect(cells).toHaveLength(12);
    const expectedOrder = [
      "01",
      "05",
      "03",
      "04",
      "02",
      "06",
      "07",
      "08",
      "09",
      "10",
      "11",
      "12",
    ];
    expectedOrder.forEach((node, index) => {
      expect(
        within(cells[index]).getByText(`第 ${node} 节`),
      ).toBeInTheDocument();
    });
  });

  it("无当前版本（axis=null、nodes 为空）时仍渲染 12 个「无数据」格", async () => {
    // 无发布版本时后端两份响应都是空的（axis 为 null、nodes 为空数组）：
    // 网格要按契约编号回退成 12 格，而不是白屏或提示「整天空闲」。
    stubPublicApi({
      meta: noAxisMetaFixture,
      day: { ...dayFixture, nodes: [] },
    });
    renderDayPage("/day/Q10-101");

    const grid = await waitForGrid();
    const cells = within(grid).getAllByRole("listitem");

    expect(cells).toHaveLength(12);
    cells.forEach((cell, index) => {
      const node = String(index + 1).padStart(2, "0");
      expect(within(cell).getByText(`第 ${node} 节`)).toBeInTheDocument();
      expect(within(cell).getByText("无数据")).toBeInTheDocument();
      expect(cell).toHaveAttribute("data-status", "no-data");
    });
  });

  it("has_data=false 的 3 个小节显示「无数据」且不显示「空闲」", async () => {
    stubPublicApi();
    renderDayPage("/day/Q10-101");

    const grid = await waitForGrid();
    // 夹具对应的 3 个缺失节点：03 / 07 / 12。
    for (const node of ["03", "07", "12"]) {
      const cell = cellByNode(grid, node);
      expect(within(cell).getByText("无数据")).toBeInTheDocument();
      expect(within(cell).getByText("不可用")).toBeInTheDocument();
      // 「无数据」不能被渲染成任何字典状态名，尤其是可用状态。
      expect(within(cell).queryByText("空闲")).toBeNull();
      expect(within(cell).queryByText("可用")).toBeNull();
      expect(cell).toHaveAttribute("data-status", "no-data");
      expect(cell).toHaveAttribute("aria-disabled", "true");
    }

    // 有观测的空闲小节照常显示字典名：证明上面的「无空闲」只对缺失小节成立。
    expect(
      within(cellByNode(grid, "01")).getByText("空闲"),
    ).toBeInTheDocument();
  });

  it("状态名来自 /api/meta 的字典，而不是页面里写死的文案", async () => {
    // 字典整体升级：v2 把 free/busy 改叫「空堂/有课」，而 dayFixture 的响应 label
    // 仍是旧字典的「空闲/占用」。页面必须跟字典走（design D11）。
    const upgradedMeta = {
      ...metaFixture,
      dict: {
        version: 2,
        states: {
          free: { label: "空堂", available: true },
          busy: { label: "有课", available: false },
        },
      },
    } satisfies MetaResult;
    stubPublicApi({ meta: upgradedMeta });
    renderDayPage("/day/Q10-101");

    const grid = await waitForGrid();

    expect(screen.queryByText("空闲")).not.toBeInTheDocument();
    expect(screen.queryByText("占用")).not.toBeInTheDocument();
    // 空闲 5 节（01/02/06/09/11），占用 4 节（04/05/08/10）。
    expect(within(grid).getAllByText("空堂")).toHaveLength(5);
    expect(within(grid).getAllByText("有课")).toHaveLength(4);
  });

  it("每格同时给出文字、图标与可用性", async () => {
    stubPublicApi();
    renderDayPage("/day/Q10-101");

    const grid = await waitForGrid();
    const cell = cellByNode(grid, "01");

    expect(within(cell).getByText("空闲")).toBeInTheDocument();
    expect(within(cell).getByText("可用")).toBeInTheDocument();
    expect(cell.querySelector("svg")).not.toBeNull();
    expect(cell).toHaveAttribute("data-status", "available");
  });
});

describe("缺少房间标识", () => {
  it("缺 roomId 的路由显示空态并给出回查询页的入口", async () => {
    const stub = stubPublicApi();
    renderDayPage("/day");

    expect(await screen.findByText("缺少房间标识")).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "返回空教室查询" }),
    ).toHaveAttribute("href", "/");
    // 没有房号就不该发全天状态请求。
    expect(stub.dayRequests()).toHaveLength(0);
  });

  it("房号只有空白时同样按缺失处理", async () => {
    const stub = stubPublicApi();
    renderDayPage("/day/%20%20");

    expect(await screen.findByText("缺少房间标识")).toBeInTheDocument();
    expect(stub.dayRequests()).toHaveLength(0);
  });
});
