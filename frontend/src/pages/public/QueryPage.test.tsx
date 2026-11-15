import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  availabilityFixture,
  contextFixture,
  metaFixture,
} from "../../test/fixtures/publicApi";
import { QueryPage } from "./QueryPage";

/** 上午档（12:00 前）与下午档（12:00–18:00）两个打开时刻。 */
const OPEN_AT_1159 = new Date(2026, 10, 15, 11, 59);
const OPEN_AT_1300 = new Date(2026, 10, 15, 13, 0);

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
  /** true 时所有请求挂起，直到调用 release()：用来观察加载态。 */
  holdUntilRelease?: boolean;
}

function stubPublicApi(options: StubOptions = {}) {
  const requests: URL[] = [];
  let release = () => {};

  if (options.holdUntilRelease) {
    let openGate = () => {};
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    release = () => openGate();
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(String(input), "http://localhost");
        requests.push(url);
        await gate;
        return respond(url);
      }),
    );
  } else {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(String(input), "http://localhost");
        requests.push(url);
        return respond(url);
      }),
    );
  }

  return {
    requests,
    release,
    queryRequests: () =>
      requests.filter((url) => url.pathname === "/api/query"),
  };
}

function respond(url: URL): Response {
  if (url.pathname === "/api/context") {
    return envelope(contextFixture, "req_context");
  }
  if (url.pathname === "/api/meta") {
    return envelope(metaFixture, "req_meta");
  }
  if (url.pathname === "/api/query") {
    return envelope(availabilityFixture, "req_query");
  }
  throw new Error(`测试未处理的请求：${url.pathname}`);
}

function renderQueryPage(initialEntry: string, now: Date) {
  const router = createMemoryRouter(
    [{ path: "/", element: <QueryPage now={now} /> }],
    { initialEntries: [initialEntry] },
  );
  const view = render(<RouterProvider router={router} />);
  return { router, ...view };
}

/**
 * 等本轮查询落定再交互：antd 的 loading 图标在 jsdom 里走不完离场动画，
 * 会一直挂在按钮里；真正决定按钮是否可点的是请求状态，所以等请求承诺结算。
 */
async function waitForQuerySettled(
  stub: ReturnType<typeof stubPublicApi>,
  count: number,
) {
  await waitFor(() => expect(stub.queryRequests()).toHaveLength(count));
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("页面加载", () => {
  it("上下文与数据版本到齐前显示加载态，筛选区不出现、也不发查询请求", async () => {
    const stub = stubPublicApi({ holdUntilRelease: true });

    renderQueryPage("/", OPEN_AT_1159);

    expect(
      screen.getByText(/正在加载页面上下文与数据版本/),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("group", { name: "日期" }),
    ).not.toBeInTheDocument();
    expect(stub.queryRequests()).toHaveLength(0);

    stub.release();

    expect(
      await screen.findByRole("group", { name: "日期" }),
    ).toBeInTheDocument();
  });
});

describe("默认值与请求参数", () => {
  it("11:59 打开页面按上午档预填 01–05 并发起查询", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);

    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));
    const query = stub.queryRequests()[0].searchParams;

    expect(query.get("view")).toBe("availability");
    expect(query.get("date_offset")).toBe("0");
    expect(query.get("start_node")).toBe("01");
    expect(query.get("end_node")).toBe("05");
    expect(query.has("building")).toBe(false);
    expect(query.has("keyword")).toBe(false);
  });

  it("13:00 打开页面按下午档预填 06–09", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1300);

    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));
    const query = stub.queryRequests()[0].searchParams;

    expect(query.get("start_node")).toBe("06");
    expect(query.get("end_node")).toBe("09");
  });

  it("把预填区间写回 URL，URL 完整描述筛选（含默认值）", async () => {
    const stub = stubPublicApi();

    const { router } = renderQueryPage("/", OPEN_AT_1159);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    expect(router.state.location.search).toBe(
      "?date_offset=0&start_node=01&end_node=05&page=1",
    );
  });

  it("选非当天日期时沿用当前时段区间，不按目标日期重算档位", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));

    await waitFor(() => expect(stub.queryRequests()).toHaveLength(2));
    const query = stub.queryRequests()[1].searchParams;
    expect(query.get("date_offset")).toBe("2");
    expect(query.get("start_node")).toBe("01");
    expect(query.get("end_node")).toBe("05");
  });

  it("URL 里的教学楼不在白名单时回退「全部」，也不会带着非法楼发请求", async () => {
    const stub = stubPublicApi();

    const { router } = renderQueryPage(
      "/?date_offset=0&building=Q99&start_node=01&end_node=05&page=1",
      OPEN_AT_1159,
    );

    await waitForQuerySettled(stub, 1);
    expect(stub.queryRequests()).toHaveLength(1);
    expect(stub.queryRequests()[0].searchParams.has("building")).toBe(false);
    expect(router.state.location.search).toBe(
      "?date_offset=0&start_node=01&end_node=05&page=1",
    );
  });
});

describe("回车与查询按钮", () => {
  it("两条入口发出完全相同的空教室查询请求", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1300);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "综合" } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });
    await waitForQuerySettled(stub, 2);
    const byEnter = stub.queryRequests()[1].search;

    // 按钮可访问名里可能仍带离场的 loading 图标，按文字部分匹配即可。
    fireEvent.click(screen.getByRole("button", { name: /查询/ }));
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(3));
    const byClick = stub.queryRequests()[2].search;

    expect(byClick).toBe(byEnter);
    expect(new URLSearchParams(byEnter).get("keyword")).toBe("综合");
  });
});

describe("后退与关键词草稿", () => {
  it("后退（POP）后输入框跟随 URL，未提交的草稿不盖住已提交关键词", async () => {
    const stub = stubPublicApi();
    const { router } = renderQueryPage("/", OPEN_AT_1300);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    // 先提交一次关键词，让 URL 上有关键词、历史记录里有两个条目。
    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "综合" } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });
    await waitForQuerySettled(stub, 2);
    expect(router.state.location.search).toContain("keyword=");

    // 提交后继续编辑但不提交：草稿与 URL 上的已提交关键词不一致。
    fireEvent.change(input, { target: { value: "综合楼" } });
    expect(input).toHaveValue("综合楼");

    await act(async () => {
      await router.navigate(-1);
    });

    await waitFor(() =>
      expect(screen.getByRole("textbox", { name: "关键词" })).toHaveValue(""),
    );
    expect(router.state.location.search).not.toContain("keyword");
  });
});

describe("URL 为单一事实来源", () => {
  it("刷新后请求参数与刷新前一致（时钟跨过预填档位也不变）", async () => {
    const stub = stubPublicApi();
    const first = renderQueryPage("/", OPEN_AT_1159);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    // 改日期：偏移 2（2026-11-17）。
    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));
    await waitFor(() =>
      expect(stub.queryRequests()[1]?.searchParams.get("date_offset")).toBe(
        "2",
      ),
    );

    // 改教学楼：综合教学楼（Q10）。
    fireEvent.mouseDown(screen.getByRole("combobox", { name: "教学楼" }));
    const buildingOption = await screen.findByRole("option", {
      name: "综合教学楼",
    });
    fireEvent.click(buildingOption);
    await waitFor(() =>
      expect(stub.queryRequests()[2]?.searchParams.get("building")).toBe("Q10"),
    );

    // 提交关键词。
    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "综合" } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });
    await waitFor(() =>
      expect(stub.queryRequests()[3]?.searchParams.get("keyword")).toBe("综合"),
    );

    const beforeRefresh = stub.queryRequests()[3].search;
    const searchAfterFilters = first.router.state.location.search;
    expect(searchAfterFilters).toContain("date_offset=2");
    expect(searchAfterFilters).toContain("building=Q10");
    expect(searchAfterFilters).toContain("keyword=%E7%BB%BC%E5%90%88");

    first.unmount();
    stub.requests.length = 0;

    // 模拟刷新：同一 URL，但时钟已到下午档；若前端重算默认值就会发成 06–09。
    renderQueryPage(searchAfterFilters, OPEN_AT_1300);
    await waitFor(() => expect(stub.queryRequests()).toHaveLength(1));

    expect(stub.queryRequests()[0].search).toBe(beforeRefresh);
  });
});
