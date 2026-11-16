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
import type {
  AvailabilityResult,
  ContextInfo,
  MetaResult,
} from "../../api/types";
import {
  availabilityFixture,
  contextFixture,
  duplicateNameAvailabilityFixture,
  emptyMetaFixture,
  metaFixture,
  noDataAvailabilityFixture,
  noMatchAvailabilityFixture,
  outOfCalendarContextFixture,
  pagedAvailabilityFixture,
  pagedAvailabilityLastPageFixture,
  pagedAvailabilitySecondPageFixture,
  staleMetaFixture,
  weekNoScheduleAvailabilityFixture,
  weekNoScheduleEmptyAvailabilityFixture,
} from "../../test/fixtures/publicApi";
import { FIXED_QUERY_BAR_RESERVED_PX } from "../../lib/layout";
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
  /** true 时所有请求挂起，直到调用 release()：用来观察整页加载态。 */
  holdUntilRelease?: boolean;
  /** 只挂起 /api/query（上下文与元数据立即返回）：观察「加载态不遮筛选」。 */
  holdQueryUntilRelease?: boolean;
  /** 覆盖 /api/query 的响应；入参是本次请求的 offset（0 起）。 */
  availability?: (offset: number) => AvailabilityResult;
  /** 让 /api/query 返回失败信封（业务码非 0），优先级高于 availability。 */
  availabilityError?: { code: number; message: string; requestId: string };
  /** 覆盖 /api/context 的响应（如 in_calendar=false）。 */
  context?: ContextInfo;
  /** 覆盖 /api/meta 的响应（如某周 stale=true）。 */
  meta?: MetaResult;
}

function stubPublicApi(overrides: StubOptions = {}) {
  const requests: URL[] = [];
  // options 按引用返回给用例：同一个页面上可以切换「下一次响应的形状」，
  // 用来验证「先成功后失败时旧列表不残留」这类跨请求行为。
  const options: StubOptions = { ...overrides };
  let release = () => {};

  if (
    overrides.holdUntilRelease === true ||
    overrides.holdQueryUntilRelease === true
  ) {
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
        // 两个挂起开关各管各的：整页挂起拦全部请求，查询挂起只拦 /api/query。
        const held =
          options.holdUntilRelease === true ||
          (options.holdQueryUntilRelease === true &&
            url.pathname === "/api/query");
        if (held) {
          await gate;
        }
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
    options,
    queryRequests: () =>
      requests.filter((url) => url.pathname === "/api/query"),
  };
}

function respond(url: URL, options: StubOptions = {}): Response {
  if (url.pathname === "/api/context") {
    return envelope(options.context ?? contextFixture, "req_context");
  }
  if (url.pathname === "/api/meta") {
    return envelope(options.meta ?? metaFixture, "req_meta");
  }
  if (url.pathname === "/api/query") {
    if (options.availabilityError !== undefined) {
      const { code, message, requestId } = options.availabilityError;
      return errorEnvelope(code, message, requestId);
    }
    const offset = Number(url.searchParams.get("offset") ?? "0");
    return envelope(
      options.availability?.(offset) ?? availabilityFixture,
      "req_query",
    );
  }
  throw new Error(`测试未处理的请求：${url.pathname}`);
}

/** 失败信封：HTTP 200 + 非 0 业务码（成败只看信封 code，Requirement 1.4）。 */
function errorEnvelope(
  code: number,
  message: string,
  requestId: string,
): Response {
  return new Response(
    JSON.stringify({ code, message, data: null, request_id: requestId }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
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

/**
 * jsdom 的 innerWidth 可写，三档布局按它计算：渲染前改宽度就能落到目标档位。
 * （不在断言里读 window.innerWidth 本身，避免测成「赋了什么就是什么」。）
 */
function setViewportWidth(width: number) {
  Object.defineProperty(window, "innerWidth", {
    configurable: true,
    writable: true,
    value: width,
  });
}

/** 模拟用户拖动窗口/旋转屏幕：改宽度并派发 resize，让 useViewportTier 重算。 */
function resizeViewportTo(width: number) {
  setViewportWidth(width);
  act(() => {
    window.dispatchEvent(new Event("resize"));
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  // 回到 jsdom 默认宽度（1024 = wide），窄屏用例不会污染后续用例。
  setViewportWidth(1024);
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

describe("加载、空、过期与错误四态（tasklist 第 6 项）", () => {
  it("查询在途时结果区显示骨架，筛选控件仍可编辑（Requirement 5.1）", async () => {
    const stub = stubPublicApi({ holdQueryUntilRelease: true });

    const { container } = renderQueryPage("/", OPEN_AT_1159);
    await screen.findByRole("group", { name: "日期" });

    expect(await screen.findByText("正在查询空教室…")).toBeInTheDocument();
    expect(container.querySelector(".ant-skeleton")).not.toBeNull();
    // 骨架只占结果区：筛选控件没有被遮罩或禁用。
    expect(screen.getByRole("radio", { name: "11-17" })).toBeEnabled();
    expect(screen.getByRole("combobox", { name: "教学楼" })).toBeEnabled();
    expect(
      screen.queryByRole("list", { name: "空教室结果" }),
    ).not.toBeInTheDocument();

    stub.release();

    expect(await screen.findByText("综合楼101")).toBeInTheDocument();
    expect(screen.queryByText("正在查询空教室…")).not.toBeInTheDocument();
  });

  it.each<[string, AvailabilityResult, string]>([
    ["no_match", noMatchAvailabilityFixture, "没有符合条件的教室"],
    ["no_data", noDataAvailabilityFixture, "该日期没有可查询的数据"],
  ])(
    "%s 显示对应空态文案，结果区不再渲染列表",
    async (_code, fixture, title) => {
      const stub = stubPublicApi({ availability: () => fixture });

      renderQueryPage("/", OPEN_AT_1159);

      expect(await screen.findByText(title)).toBeInTheDocument();
      expect(
        screen.queryByRole("list", { name: "空教室结果" }),
      ).not.toBeInTheDocument();
      expect(stub.queryRequests()).toHaveLength(1);
    },
  );

  it("week_no_schedule 用置顶提示，命中结果照常展示", async () => {
    stubPublicApi({ availability: () => weekNoScheduleAvailabilityFixture });

    renderQueryPage("/", OPEN_AT_1159);

    expect(await screen.findByText("该周疑似无排课")).toBeInTheDocument();
    // 结果区没有被空态替换：命中项与分页总数都还在。
    const list = screen.getByRole("list", { name: "空教室结果" });
    expect(within(list).getByText("综合楼201")).toBeInTheDocument();
    expect(screen.getByText("共 45 间空教室")).toBeInTheDocument();
    // 提示在顶部（Alert），而不是被当成空态的 Empty。
    expect(screen.getByRole("alert")).toHaveTextContent("该周疑似无排课");
    expect(screen.queryByText("没有符合条件的教室")).not.toBeInTheDocument();
  });

  it("week_no_schedule 且 total=0 时只留置顶提示，不画空列表与分页（Requirement 5.2）", async () => {
    stubPublicApi({
      availability: () => weekNoScheduleEmptyAvailabilityFixture,
    });

    renderQueryPage("/", OPEN_AT_1159);

    expect(await screen.findByText("该周疑似无排课")).toBeInTheDocument();
    // 没有命中可展示：空列表与「共 0 间空教室」的分页都不该出现。
    expect(
      screen.queryByRole("list", { name: "空教室结果" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("共 0 间空教室")).not.toBeInTheDocument();
    // 仍是置顶 Alert，而不是被当成空态把结果区换成 Empty。
    expect(screen.getByRole("alert")).toHaveTextContent("该周疑似无排课");
    expect(screen.queryByText("没有符合条件的教室")).not.toBeInTheDocument();
  });

  it("无当前数据版本（meta.empty）时显示「暂无数据版本」，优先于 hint=no_data", async () => {
    const stub = stubPublicApi({
      meta: emptyMetaFixture,
      availability: () => noDataAvailabilityFixture,
    });

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    // 后端 total=0 时总会给 hint，所以这一档只能靠 meta.empty 优先才可达。
    expect(screen.getByText("暂无数据版本")).toBeInTheDocument();
    expect(
      screen.getByText("数据版本还没有发布，稍后再来"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("该日期没有可查询的数据"),
    ).not.toBeInTheDocument();
    // 读屏播报与界面同一档，不能界面说「暂无数据版本」而播报说 no_data。
    expect(
      screen.getByRole("status", { name: "查询结果播报" }),
    ).toHaveTextContent("查询结果：暂无数据版本");
  });

  it("响应周次在元数据中被标记过期时提示，数据仍可见（Requirement 5.3）", async () => {
    stubPublicApi({
      meta: staleMetaFixture,
      availability: () => pagedAvailabilityFixture,
    });

    renderQueryPage("/", OPEN_AT_1159);

    expect(await screen.findByText("第 11 周数据已过期")).toBeInTheDocument();
    // 最近成功时间按 Asia/Shanghai 展示：07:10Z → 15:10。
    expect(
      screen.getByText(/该周最近一次成功同步：2026-11-14 15:10/),
    ).toBeInTheDocument();
    expect(screen.getByText("综合楼201")).toBeInTheDocument();
  });

  it("in_calendar=false 时提示当前不在教学周，已有数据照常展示（Requirement 5.4）", async () => {
    stubPublicApi({ context: outOfCalendarContextFixture });

    renderQueryPage("/", OPEN_AT_1159);

    expect(await screen.findByText("当前不在教学周")).toBeInTheDocument();
    expect(await screen.findByText("综合楼101")).toBeInTheDocument();
  });

  it("请求失败时用错误占位替换结果区，上一次列表不残留（Requirement 5.5）", async () => {
    const stub = stubPublicApi({
      availability: () => pagedAvailabilityFixture,
    });
    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);
    expect(screen.getByText("综合楼201")).toBeInTheDocument();

    // 同一个页面的下一次查询改成 50000：旧列表必须一起消失。
    stub.options.availabilityError = {
      code: 50000,
      message: "",
      requestId: "req_fail",
    };
    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));

    expect(await screen.findByText("服务异常")).toBeInTheDocument();
    expect(screen.getByText("request_id：req_fail")).toBeInTheDocument();
    expect(screen.queryByText("综合楼201")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("list", { name: "空教室结果" }),
    ).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeEnabled();
  });

  it("错误态点重试用同一个请求键重发一次请求", async () => {
    const stub = stubPublicApi({
      availabilityError: {
        code: 50000,
        message: "",
        requestId: "req_fail",
      },
    });
    renderQueryPage("/", OPEN_AT_1159);
    expect(await screen.findByText("服务异常")).toBeInTheDocument();

    // 让下一次成功：重试按钮应重发同一组参数而不是留在错误态。
    stub.options.availabilityError = undefined;
    stub.options.availability = () => availabilityFixture;
    fireEvent.click(screen.getByRole("button", { name: "重试" }));

    expect(await screen.findByText("综合楼101")).toBeInTheDocument();
    expect(stub.queryRequests()).toHaveLength(2);
  });

  it("42901 时显示限流文案，查询按钮禁用 3 秒后恢复（design「Error Handling」）", async () => {
    vi.useFakeTimers();
    try {
      const stub = stubPublicApi({
        availabilityError: {
          code: 42901,
          message: "",
          requestId: "req_limited",
        },
      });

      renderQueryPage("/", OPEN_AT_1159);

      // 假计时器下 waitFor 无法轮询（它内部的 setTimeout 也被假化），
      // 改用 act + advanceTimersByTimeAsync 冲刷微任务：fetch 桩、信封解析与
      // React 状态更新都挂在微任务上，循环几轮直到错误文案出现，不依赖固定次数。
      for (let attempt = 0; attempt < 10; attempt += 1) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(0);
        });
        if (screen.queryByText("请求过于频繁，请稍后重试") !== null) {
          break;
        }
      }

      expect(screen.getByText("请求过于频繁，请稍后重试")).toBeInTheDocument();
      expect(stub.queryRequests()).toHaveLength(1);
      // 冷却中：查询与重试都不能再撞一次限流。
      expect(screen.getByRole("button", { name: /查询/ })).toBeDisabled();
      expect(screen.getByRole("button", { name: "重试" })).toBeDisabled();

      act(() => {
        vi.advanceTimersByTime(2_999);
      });
      expect(screen.getByRole("button", { name: /查询/ })).toBeDisabled();

      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(screen.getByRole("button", { name: /查询/ })).toBeEnabled();
      expect(screen.getByRole("button", { name: "重试" })).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("aria-live 区域随结果变化更新播报文本（Requirement 8.6）", async () => {
    const stub = stubPublicApi();
    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const live = () => screen.getByRole("status", { name: "查询结果播报" });
    // 原子播报：一次读完整句，而不是把「查询结果：共 N 间空教室」拆成几段。
    expect(live()).toHaveAttribute("aria-atomic", "true");
    expect(live()).toHaveTextContent("查询结果：共 1 间空教室");

    // 换日期触发第二次查询：命中数变化 → 播报文本跟着变。
    stub.options.availability = () => pagedAvailabilityFixture;
    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));
    await waitFor(() =>
      expect(live()).toHaveTextContent("查询结果：共 45 间空教室"),
    );

    // 进入空态也走同一个播报区，且用与界面相同的文案表。
    stub.options.availability = () => noMatchAvailabilityFixture;
    fireEvent.click(screen.getByRole("radio", { name: "11-18" }));
    await waitFor(() =>
      expect(live()).toHaveTextContent("查询结果：没有符合条件的教室"),
    );
  });

  it("公开页页脚展示数据来源、更新频率与仅供参考（Requirement 5.6）", async () => {
    stubPublicApi();
    renderQueryPage("/", OPEN_AT_1159);

    const footer = await screen.findByRole("contentinfo");
    expect(footer).toHaveTextContent("数据来自学校教务系统");
    expect(footer).toHaveTextContent("每周自动同步");
    expect(footer).toHaveTextContent("仅供参考");
    // 页脚必须在 <main> 之外：<footer> 嵌在 main 里对真实读屏只是普通元素，
    // jsdom 的 role 查询分辨不出来，所以这里额外钉住位置。
    expect(footer.closest("main")).toBeNull();
    expect(screen.getByRole("main")).not.toContainElement(footer);
  });
});

describe("响应式三档布局（tasklist 第 10 项；Requirement 8.1–8.3）", () => {
  it("<600px 单列，并把查询按钮固定在视口底部单手可达区", async () => {
    setViewportWidth(375);
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const page = screen.getByTestId("public-page");
    expect(page).toHaveAttribute("data-layout", "compact");
    expect(page).toHaveAttribute("data-columns", "1");
    expect(page.style.gridTemplateColumns).toMatch(
      /^repeat\(1, minmax\(0(px)?, 1fr\)\)$/,
    );
    expect(screen.getByTestId("public-filter-area")).toHaveAttribute(
      "data-span",
      "1",
    );
    expect(screen.getByTestId("public-results-area")).toHaveAttribute(
      "data-span",
      "1",
    );

    // 底部固定操作区：贴住视口底边，按钮 48px 高（≥44×44）。
    const bar = screen.getByTestId("public-query-bar");
    expect(bar).toHaveStyle({ position: "fixed", bottom: "0px" });
    const bottomButton = within(bar).getByRole("button", { name: /查询/ });
    expect(bottomButton).toHaveStyle({ minHeight: "48px" });

    // 固定区高度只在页脚容器预留一次：<main> 仍是普通 padding（16px），
    // 不再额外让出一段，否则 compact 档底部会多出一截空白。
    expect(page.style.paddingBottom).toBe("16px");
    const footerWrapper = screen.getByRole("contentinfo").parentElement;
    expect(footerWrapper?.style.paddingBottom).toBe(
      `${FIXED_QUERY_BAR_RESERVED_PX}px`,
    );

    // 表单内不再渲染第二个「查询」：同一表单只有一个提交入口。
    expect(screen.getAllByRole("button", { name: /查询/ })).toHaveLength(1);
    expect(screen.getByRole("list", { name: "空教室结果" })).toHaveAttribute(
      "data-columns",
      "1",
    );
  });

  it("600–1024px 双列：筛选左、结果右，按钮回到筛选区内", async () => {
    setViewportWidth(800);
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const page = screen.getByTestId("public-page");
    expect(page).toHaveAttribute("data-layout", "medium");
    expect(page).toHaveAttribute("data-columns", "2");
    expect(page.style.gridTemplateColumns).toMatch(
      /^repeat\(2, minmax\(0(px)?, 1fr\)\)$/,
    );
    expect(screen.getByTestId("public-filter-area")).toHaveAttribute(
      "data-span",
      "1",
    );
    expect(screen.getByTestId("public-results-area")).toHaveAttribute(
      "data-span",
      "1",
    );

    // 双列下按钮回到筛选卡里，底部固定区不渲染。
    expect(screen.queryByTestId("public-query-bar")).not.toBeInTheDocument();
    const form = screen.getByRole("form", { name: "空教室筛选条件" });
    expect(
      within(form).getByRole("button", { name: /查询/ }),
    ).toBeInTheDocument();
    expect(screen.getByRole("list", { name: "空教室结果" })).toHaveAttribute(
      "data-columns",
      "1",
    );
  });

  it("≥1024px 三列：筛选 1 列 + 结果 2 列，两者同屏且结果区内再排两列", async () => {
    setViewportWidth(1440);
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const page = screen.getByTestId("public-page");
    expect(page).toHaveAttribute("data-layout", "wide");
    expect(page).toHaveAttribute("data-columns", "3");
    expect(page.style.gridTemplateColumns).toMatch(
      /^repeat\(3, minmax\(0(px)?, 1fr\)\)$/,
    );

    const filterArea = screen.getByTestId("public-filter-area");
    const resultsArea = screen.getByTestId("public-results-area");
    expect(filterArea).toHaveAttribute("data-span", "1");
    expect(resultsArea).toHaveAttribute("data-span", "2");

    // 同屏：两块都在同一个页面栅格里，而不是上下两个视图或换页。
    expect(page).toContainElement(filterArea);
    expect(page).toContainElement(resultsArea);
    expect(screen.getByRole("list", { name: "空教室结果" })).toHaveAttribute(
      "data-columns",
      "2",
    );
    expect(screen.queryByTestId("public-query-bar")).not.toBeInTheDocument();

    // 没有固定操作区就不留底部余量：<main> 与页脚都只用普通 24px 内边距。
    expect(page.style.paddingBottom).toBe("24px");
    expect(
      screen.getByRole("contentinfo").parentElement?.style.paddingBottom,
    ).toBe("24px");
  });

  it("窗口从宽屏缩到窄屏时切到单列并出现底部查询按钮", async () => {
    setViewportWidth(1440);
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);
    expect(screen.getByTestId("public-page")).toHaveAttribute(
      "data-layout",
      "wide",
    );
    expect(screen.queryByTestId("public-query-bar")).not.toBeInTheDocument();

    resizeViewportTo(375);

    await waitFor(() =>
      expect(screen.getByTestId("public-page")).toHaveAttribute(
        "data-layout",
        "compact",
      ),
    );
    expect(screen.getByTestId("public-query-bar")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /查询/ })).toHaveLength(1);
  });
});

describe("触控目标 ≥44×44 的渲染断言（Requirement 8.4）", () => {
  it("日期项、分页项与查询按钮都落在全局 CSS 覆盖的 antd 元素上", async () => {
    // 用宽屏档：此时表单内的查询按钮在（compact 下会移到固定操作区）。
    setViewportWidth(1024);
    const stub = stubPublicApi({
      availability: () => pagedAvailabilityFixture,
    });
    const { container } = renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    // 日期项：11 个 Radio.Button，且都在 global.css 的 .public-filter 作用域内
    // （jsdom 不加载外部 CSS，只能断言选择器实际命中的 DOM 长得对）。
    const dateGroup = screen.getByRole("group", { name: "日期" });
    expect(
      dateGroup.querySelectorAll(".ant-radio-button-wrapper"),
    ).toHaveLength(11);
    expect(dateGroup.closest(".public-filter")).not.toBeNull();

    // 分页项：45 间 / 每页 20 → 3 个页码，全部落在 .public-results 内。
    expect(
      container.querySelectorAll(".public-results .ant-pagination-item"),
    ).toHaveLength(3);

    // 查询按钮：antd 的 .ant-btn 由 controlHeight=44 抬高，页内再钉一次 44 的下限。
    const button = screen.getByRole("button", { name: /查询/ });
    expect(button).toHaveClass("ant-btn");
    expect(button).toHaveStyle({ minHeight: "44px" });
  });
});

describe("可访问性收口（Requirement 8.6）", () => {
  it("点击查询后焦点仍留在查询按钮上，不被结果区抢走", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const button = screen.getByRole("button", { name: /查询/ });
    button.focus();
    expect(button).toHaveFocus();

    fireEvent.click(button);
    await waitForQuerySettled(stub, 2);

    expect(button).toHaveFocus();
    expect(document.activeElement).toBe(button);
  });

  it("回车查询后焦点留在关键词输入框", async () => {
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const input = screen.getByRole("textbox", { name: "关键词" });
    input.focus();
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });
    await waitForQuerySettled(stub, 2);

    expect(input).toHaveFocus();
  });

  it("窄屏点底部固定按钮查询后焦点不跳到结果区", async () => {
    setViewportWidth(375);
    const stub = stubPublicApi();

    renderQueryPage("/", OPEN_AT_1159);
    await waitForQuerySettled(stub, 1);

    const button = within(screen.getByTestId("public-query-bar")).getByRole(
      "button",
      { name: /查询/ },
    );
    button.focus();
    fireEvent.click(button);
    await waitForQuerySettled(stub, 2);

    expect(button).toHaveFocus();
  });
});

describe("MSW 默认 handler 端到端", () => {
  it("不换 fetch 也走真实请求链拿到夹具，并播报命中数量", async () => {
    // 本用例不调用 stubPublicApi：请求由 setup.ts 里 listen 的 MSW server 承接，
    // 走的是 api/public → api/client → fetch 的完整链路。
    renderQueryPage("/", OPEN_AT_1159);

    expect(await screen.findByText("综合楼101")).toBeInTheDocument();
    expect(
      screen.getByRole("status", { name: "查询结果播报" }),
    ).toHaveTextContent("查询结果：共 1 间空教室");
  });
});
