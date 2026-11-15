import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AvailabilityResult } from "../../api/types";
import {
  availabilityFixture,
  contextFixture,
  duplicateNameAvailabilityFixture,
  metaFixture,
  pagedAvailabilityFixture,
  pagedAvailabilityLastPageFixture,
  pagedAvailabilitySecondPageFixture,
} from "../../test/fixtures/publicApi";
import { QueryPage, RESULT_PAGE_SIZE } from "./QueryPage";

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
  /** 覆盖 /api/query 的响应；入参是本次请求的 offset（0 起）。 */
  availability?: (offset: number) => AvailabilityResult;
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
        return respond(url, options);
      }),
    );
  } else {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = new URL(String(input), "http://localhost");
        requests.push(url);
        return respond(url, options);
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

function respond(url: URL, options: StubOptions = {}): Response {
  if (url.pathname === "/api/context") {
    return envelope(contextFixture, "req_context");
  }
  if (url.pathname === "/api/meta") {
    return envelope(metaFixture, "req_meta");
  }
  if (url.pathname === "/api/query") {
    const offset = Number(url.searchParams.get("offset") ?? "0");
    return envelope(
      options.availability?.(offset) ?? availabilityFixture,
      "req_query",
    );
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

/** 点页码：rc-pagination 把 title 放在 <li> 上、把点击处理挂在里面的 <a> 上。 */
function clickPage(page: number) {
  const item = screen.getByTitle(String(page));
  fireEvent.click(within(item).getByText(String(page)));
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

describe("结果列表与分页", () => {
  it("首页按 limit/offset 请求，并在结果区展示命中总数", async () => {
    const stub = stubPublicApi({
      availability: () => pagedAvailabilityFixture,
    });

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const query = stub.queryRequests()[0].searchParams;
    expect(query.get("limit")).toBe(String(RESULT_PAGE_SIZE));
    expect(query.get("offset")).toBe("0");

    expect(
      screen.getByRole("list", { name: "空教室结果" }),
    ).toBeInTheDocument();
    expect(screen.getByText("共 45 间空教室")).toBeInTheDocument();
  });

  it("翻到第 2 页时 offset 按页容量推进，URL 记录页码并渲染第 2 页数据", async () => {
    const stub = stubPublicApi({
      availability: (offset) =>
        offset === 0
          ? pagedAvailabilityFixture
          : pagedAvailabilitySecondPageFixture,
    });

    const { router } = renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);
    expect(screen.getByText("综合楼201")).toBeInTheDocument();

    clickPage(2);

    await waitFor(() => expect(stub.queryRequests()).toHaveLength(2));
    const second = stub.queryRequests()[1].searchParams;
    expect(second.get("limit")).toBe(String(RESULT_PAGE_SIZE));
    expect(second.get("offset")).toBe(String(RESULT_PAGE_SIZE));
    expect(router.state.location.search).toContain("page=2");

    expect(await screen.findByText("综合楼301")).toBeInTheDocument();
    expect(screen.queryByText("综合楼201")).not.toBeInTheDocument();
  });

  it("URL 上的页码越界时钳到最后一页，并用 replace 规范化 URL", async () => {
    const stub = stubPublicApi({
      availability: (offset) =>
        offset === 2 * RESULT_PAGE_SIZE
          ? pagedAvailabilityLastPageFixture
          : pagedAvailabilityFixture,
    });

    const { router } = renderQueryPage(
      "/?date_offset=0&start_node=01&end_node=05&page=999",
      OPEN_AT_1159,
    );

    // 第一次仍按 URL 上的页码请求（越界 offset），拿到 total=45 后才纠正。
    await waitForQuerySettled(stub, 2);
    expect(stub.queryRequests()[0].searchParams.get("offset")).toBe(
      String((999 - 1) * RESULT_PAGE_SIZE),
    );

    // total=45、单页 20 → 最后一页是第 3 页，最终请求的 offset 落在 40。
    const requests = stub.queryRequests();
    const last = requests[requests.length - 1];
    expect(last.searchParams.get("offset")).toBe(String(2 * RESULT_PAGE_SIZE));
    expect(router.state.location.search).toBe(
      "?date_offset=0&start_node=01&end_node=05&page=3",
    );
    // 钳制用 replace：初始为 POP，纠正后为 REPLACE，不新增历史记录。
    expect(router.state.historyAction).toBe("REPLACE");

    expect(await screen.findByText("综合楼401")).toBeInTheDocument();
  });

  it("改日期、教学楼或关键词都会回到第一页", async () => {
    const stub = stubPublicApi({
      availability: () => pagedAvailabilityFixture,
    });

    const { router } = renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    // 先翻到第 2 页，再分别改三类筛选：每次都应回到 offset=0、page=1。
    clickPage(2);
    await waitForQuerySettled(stub, 2);
    expect(stub.queryRequests()[1].searchParams.get("offset")).toBe(
      String(RESULT_PAGE_SIZE),
    );

    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));
    await waitForQuerySettled(stub, 3);
    expect(stub.queryRequests()[2].searchParams.get("offset")).toBe("0");
    expect(router.state.location.search).toContain("page=1");

    clickPage(2);
    await waitForQuerySettled(stub, 4);
    expect(stub.queryRequests()[3].searchParams.get("offset")).toBe(
      String(RESULT_PAGE_SIZE),
    );

    fireEvent.mouseDown(screen.getByRole("combobox", { name: "教学楼" }));
    const buildingOption = await screen.findByRole("option", {
      name: "综合教学楼",
    });
    fireEvent.click(buildingOption);
    await waitForQuerySettled(stub, 5);
    expect(stub.queryRequests()[4].searchParams.get("offset")).toBe("0");
    expect(router.state.location.search).toContain("page=1");

    clickPage(2);
    await waitForQuerySettled(stub, 6);
    expect(stub.queryRequests()[5].searchParams.get("offset")).toBe(
      String(RESULT_PAGE_SIZE),
    );

    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "综合" } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });
    await waitForQuerySettled(stub, 7);
    expect(stub.queryRequests()[6].searchParams.get("offset")).toBe("0");
    expect(router.state.location.search).toContain("page=1");
  });

  it("结果区按服务端返回顺序渲染，同名房渲染出不同标签", async () => {
    const stub = stubPublicApi({
      availability: () => duplicateNameAvailabilityFixture,
    });

    renderQueryPage(
      "/?date_offset=2&building=Q10&start_node=01&end_node=05&page=1",
      OPEN_AT_1159,
    );
    await waitForQuerySettled(stub, 1);

    const list = screen.getByRole("list", { name: "空教室结果" });
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(
      within(rows[0]).getByText("综合楼101（Q12-101）"),
    ).toBeInTheDocument();
    expect(within(rows[1]).getByText("理工楼302")).toBeInTheDocument();
    expect(
      within(rows[2]).getByText("综合楼101（Q10-101）"),
    ).toBeInTheDocument();
  });

  it("每行的「看全天状态」入口带上当前 date_offset 与 building", async () => {
    const stub = stubPublicApi({
      availability: () => duplicateNameAvailabilityFixture,
    });

    renderQueryPage(
      "/?date_offset=2&building=Q10&start_node=01&end_node=05&page=1",
      OPEN_AT_1159,
    );
    await waitForQuerySettled(stub, 1);

    expect(
      screen.getByRole("link", { name: "看 综合楼101（Q12-101） 的全天状态" }),
    ).toHaveAttribute("href", "/day/Q12-101?date_offset=2&building=Q10");
    expect(
      screen.getByRole("link", { name: "看 理工楼302 的全天状态" }),
    ).toHaveAttribute("href", "/day/Q11-302?date_offset=2&building=Q10");
  });
});
