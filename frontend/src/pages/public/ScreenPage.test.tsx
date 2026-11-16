import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ScreenLayout } from "../../layouts/ScreenLayout";
import {
  emptyMetaFixture,
  emptyScreenFixture,
  metaFixture,
  screenFixture,
} from "../../test/fixtures/publicApi";
import { ScreenPage } from "./ScreenPage";

// 大屏布局与信息区块（tasklist 第 7–8 项）。
// 验收点在这里落地：
// 1) 五个业务区块 + 占用分布区块渲染，以及 empty=true 的「保留标题 + 暂无数据」；
// 2) 窗口 1920×1080 → 2560×1440 → 1280×1024 只改缩放系数，区块顺序与相对位置不变；
// 3) 热力图列头来自 /api/meta 的轴定义，meta 失败时回退「大节 1..5」；
// 另加一条「失败时页面不出现业务码、message 与错误原文」。

const INITIAL_WIDTH = window.innerWidth;
const INITIAL_HEIGHT = window.innerHeight;

/** 成功信封：message 故意写成可识别的串，用来断言页面不把它渲染出来。 */
function envelope(data: unknown): Response {
  return new Response(
    JSON.stringify({
      code: 0,
      message: "success",
      data,
      request_id: "req_screen_test",
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

/** 失败信封：code 与 message 都是敏感串，页面必须一个都不展示。 */
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

/**
 * 大屏两个接口各按路径返回：`/api/screen` 是主数据，`/api/meta` 只为热力图列头取轴
 * （design D12）。默认 meta 成功；用例可单独让 meta 失败。
 */
function stubScreenApi(options: {
  screen: () => Response;
  meta?: () => Response;
}): URL[] {
  const requests: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://localhost");
      requests.push(url);
      if (url.pathname === "/api/meta") {
        return (options.meta ?? (() => envelope(metaFixture)))();
      }
      return options.screen();
    }),
  );
  return requests;
}

/**
 * 模拟窗口尺寸变化：jsdom 不会自己改 innerWidth/innerHeight，
 * 必须同时改值并派发 resize，ScreenLayout 的监听才会重算缩放系数。
 */
function setViewport(width: number, height: number) {
  Object.defineProperty(window, "innerWidth", {
    configurable: true,
    writable: true,
    value: width,
  });
  Object.defineProperty(window, "innerHeight", {
    configurable: true,
    writable: true,
    value: height,
  });
  act(() => {
    window.dispatchEvent(new Event("resize"));
  });
}

/** 按路由里的真实组合渲染：ScreenLayout 提供暗色壳与缩放画布，ScreenPage 是内容。 */
function renderScreenPage() {
  return render(
    <ScreenLayout>
      <ScreenPage />
    </ScreenLayout>,
  );
}

/** 从 `transform: scale(1.3333)` 里取数值；取不到直接失败，避免断言悄悄空转。 */
function scaleOf(element: HTMLElement): number {
  const match = /scale\(([\d.eE+-]+)\)/.exec(element.style.transform);
  if (match === null) {
    throw new Error(`画布上没有缩放的 transform：${element.style.transform}`);
  }
  return Number(match[1]);
}

/** 区块顺序与「挂在哪个网格区域」的快照：窗口变化前后必须完全一致。 */
function blockLayout(): Array<{ title: string | null; area: string | null }> {
  return screen.getAllByRole("region").map((block) => ({
    title: block.getAttribute("aria-label"),
    area:
      block.closest("[data-grid-area]")?.getAttribute("data-grid-area") ?? null,
  }));
}

beforeEach(() => {
  setViewport(1920, 1080);
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  // 还原视口，避免用例之间的窗口尺寸互相污染。
  setViewport(INITIAL_WIDTH, INITIAL_HEIGHT);
});

describe("信息区块", () => {
  it("渲染站点规模、数据版本、逐周新鲜度、最近一轮、异常计数与占用分布六块", async () => {
    const requests = stubScreenApi({
      screen: () => envelope(screenFixture),
    });

    renderScreenPage();

    const scaleBlock = await screen.findByRole("region", { name: "站点规模" });
    // 首屏只发两个请求：主数据与列头用的轴定义，且都是各一次。
    await waitFor(() => expect(requests).toHaveLength(2));
    expect(requests.map((url) => url.pathname).sort()).toEqual([
      "/api/meta",
      "/api/screen",
    ]);
    // 千分位、单位与采集窗口都来自响应字段，不写死。
    expect(within(scaleBlock).getByText("2,481")).toBeInTheDocument();
    expect(within(scaleBlock).getByText("26")).toBeInTheDocument();
    expect(within(scaleBlock).getByText("2026-2027-1")).toBeInTheDocument();
    expect(within(scaleBlock).getByText("第 5 周")).toBeInTheDocument();
    expect(within(scaleBlock).getByText("第 5–8 周")).toBeInTheDocument();

    const releaseBlock = screen.getByRole("region", { name: "数据版本" });
    expect(
      within(releaseBlock).getByText("20261114T071012Z"),
    ).toBeInTheDocument();
    // 07:10Z → 15:10，时间标注在标签里说明按 Asia/Shanghai 展示。
    expect(
      within(releaseBlock).getByText("2026-11-14 15:10"),
    ).toBeInTheDocument();
    expect(
      within(releaseBlock).getByText("生成时间（Asia/Shanghai）"),
    ).toBeInTheDocument();

    const weeksBlock = screen.getByRole("region", { name: "逐周新鲜度" });
    expect(within(weeksBlock).getByText("第 5 周")).toBeInTheDocument();
    expect(within(weeksBlock).getByText("第 7 周")).toBeInTheDocument();
    // 第 5 / 7 周都已发布，第 6 周未发布：三种逐周状态各断言到。
    expect(within(weeksBlock).getAllByText("已发布")).toHaveLength(2);
    expect(within(weeksBlock).getByText("未发布")).toBeInTheDocument();
    expect(within(weeksBlock).getByText("已过期")).toBeInTheDocument();
    expect(
      within(weeksBlock).getByText("2026-11-14 15:10"),
    ).toBeInTheDocument();

    const runBlock = screen.getByRole("region", { name: "最近一轮" });
    expect(within(runBlock).getByText("部分完成")).toBeInTheDocument();
    expect(
      within(runBlock).getByText("开始时间（Asia/Shanghai）"),
    ).toBeInTheDocument();
    expect(within(runBlock).getByText("2026-11-14 14:00")).toBeInTheDocument();
    expect(within(runBlock).getByText("30 分 30 秒")).toBeInTheDocument();

    const anomalyBlock = screen.getByRole("region", { name: "异常计数" });
    expect(within(anomalyBlock).getByText("未知格")).toBeInTheDocument();
    expect(within(anomalyBlock).getByText("复合格")).toBeInTheDocument();
    expect(within(anomalyBlock).getByText("归属冲突")).toBeInTheDocument();
    expect(within(anomalyBlock).getByText("12")).toBeInTheDocument();
    expect(within(anomalyBlock).getByText("51")).toBeInTheDocument();
    expect(within(anomalyBlock).getByText("2")).toBeInTheDocument();

    // 占用分布（第 8 项）：热力图 35 格、列头取自 meta 轴、按楼列表来自 screen。
    const occupancyBlock = screen.getByRole("region", { name: "占用分布" });
    expect(
      within(occupancyBlock).getByRole("img", {
        name: "占用热力图：7 天 5 大节，最高 42%",
      }),
    ).toBeInTheDocument();
    expect(within(occupancyBlock).getAllByTestId("heatmap-cell")).toHaveLength(
      35,
    );
    expect(within(occupancyBlock).getByText("第 03–05 节")).toBeInTheDocument();
    expect(within(occupancyBlock).getByText("综合教学楼")).toBeInTheDocument();
    expect(
      within(occupancyBlock).getByText("3,821 / 14,700"),
    ).toBeInTheDocument();
    expect(
      within(occupancyBlock).queryByText("暂无数据"),
    ).not.toBeInTheDocument();
    // 第 7 项的占位文案不应再出现在页面上。
    expect(screen.queryByText(/尚待接入/)).not.toBeInTheDocument();

    // 信封里的 message 不是可展示内容，页面上也不出现业务码或错误字段名。
    expect(screen.queryByText("success")).not.toBeInTheDocument();
    for (const secret of ["50000", "error_code", "request_id"]) {
      expect(screen.queryByText(new RegExp(secret))).not.toBeInTheDocument();
    }
  });

  it("empty=true 时六个区块都保留标题并显示「暂无数据」", async () => {
    stubScreenApi({
      screen: () => envelope(emptyScreenFixture),
      meta: () => envelope(emptyMetaFixture),
    });

    renderScreenPage();

    await screen.findByRole("region", { name: "站点规模" });

    // 「占用分布」此时没有热力图，与其它区块同口径：保留标题，正文换成「暂无数据」。
    for (const title of [
      "站点规模",
      "数据版本",
      "逐周新鲜度",
      "最近一轮",
      "异常计数",
      "占用分布",
    ]) {
      const block = screen.getByRole("region", { name: title });
      expect(
        within(block).getByRole("heading", { name: title }),
      ).toBeInTheDocument();
      expect(within(block).getByText("暂无数据")).toBeInTheDocument();
    }

    // 空态下不应残留「尚待接入」的占位原文。
    expect(screen.queryByText(/尚待接入/)).not.toBeInTheDocument();
    expect(screen.getAllByText("暂无数据")).toHaveLength(6);
  });

  it("失败时只显示中性占位，不出现业务码、message 或错误原文", async () => {
    stubScreenApi({
      screen: () =>
        errorEnvelope(
          50001,
          "error_code=UPSTREAM_401 upstream token expired",
          "req_screen_secret",
        ),
      meta: () => errorEnvelope(50001, "meta failed", "req_meta_secret"),
    });

    renderScreenPage();

    expect(await screen.findByText("数据暂时不可用")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "重试" })).toBeEnabled();

    for (const secret of [
      "50001",
      "error_code",
      "UPSTREAM_401",
      "token expired",
      "request_id",
      "req_screen_secret",
    ]) {
      expect(screen.queryByText(new RegExp(secret))).not.toBeInTheDocument();
    }

    // 失败时不画半截数据：数据区块一个都没有。
    expect(
      screen.queryByRole("region", { name: "站点规模" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("region", { name: "占用分布" }),
    ).not.toBeInTheDocument();
  });

  it("meta 请求失败不拖垮大屏，热力图列头回退「大节 1..5」", async () => {
    stubScreenApi({
      screen: () => envelope(screenFixture),
      meta: () => errorEnvelope(50000, "meta boom", "req_meta_secret"),
    });

    renderScreenPage();

    const occupancyBlock = await screen.findByRole("region", {
      name: "占用分布",
    });
    for (const label of ["大节 1", "大节 2", "大节 3", "大节 4", "大节 5"]) {
      expect(within(occupancyBlock).getByText(label)).toBeInTheDocument();
    }
    expect(within(occupancyBlock).getAllByTestId("heatmap-cell")).toHaveLength(
      35,
    );
    // 主数据正常时页面不进错误态，也不显示 meta 的错误信息。
    expect(screen.queryByText("数据暂时不可用")).not.toBeInTheDocument();
    expect(screen.queryByText(/meta boom/)).not.toBeInTheDocument();
  });

  it("失败页重试会一并重发 meta，恢复后列头改由轴派生", async () => {
    let screenCalls = 0;
    let metaCalls = 0;
    const requests = stubScreenApi({
      screen: () => {
        screenCalls += 1;
        return screenCalls === 1
          ? errorEnvelope(50000, "screen boom", "req_screen_secret")
          : envelope(screenFixture);
      },
      meta: () => {
        metaCalls += 1;
        return metaCalls === 1
          ? errorEnvelope(50000, "meta boom", "req_meta_secret")
          : envelope(metaFixture);
      },
    });

    renderScreenPage();

    expect(await screen.findByText("数据暂时不可用")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));

    const occupancyBlock = await screen.findByRole("region", {
      name: "占用分布",
    });
    // 列头从「大节 1..5」换成轴派生的「第 10–12 节」：证明 meta 也真的重发了，
    // 而不是只重发 screen、列头永久停在回退文案。
    expect(
      await within(occupancyBlock).findByText("第 10–12 节"),
    ).toBeInTheDocument();
    expect(
      requests.filter((url) => url.pathname === "/api/screen"),
    ).toHaveLength(2);
    expect(requests.filter((url) => url.pathname === "/api/meta")).toHaveLength(
      2,
    );
  });
});

describe("等比缩放与布局稳定性", () => {
  it("窗口从 1920×1080 变 2560×1440 / 1280×1024 时只改缩放系数，区块顺序与相对位置不变", async () => {
    stubScreenApi({ screen: () => envelope(screenFixture) });

    setViewport(1920, 1080);
    renderScreenPage();

    await screen.findByRole("region", { name: "站点规模" });

    const canvas = screen.getByTestId("screen-canvas");
    // 设计稿尺寸恒定：窗口只改 transform，不改内层宽高。
    expect(canvas.style.width).toBe("1920px");
    expect(canvas.style.height).toBe("1080px");
    // top left 的 transform-origin：jsdom 会序列化成 "left top"，与浏览器等价。
    expect(canvas.style.transformOrigin).toMatch(/^(top left|left top)$/);
    expect(scaleOf(canvas)).toBeCloseTo(1, 6);

    const before = blockLayout();
    expect(before.map((block) => block.title)).toEqual([
      "站点规模",
      "数据版本",
      "逐周新鲜度",
      "最近一轮",
      "异常计数",
      "占用分布",
    ]);

    // 16:9 放大：只换缩放系数，区块顺序与网格区域完全不变。
    // 精度只取到 4 位：jsdom 会把 CSS 数字序列化成 5 位小数（1.33333），
    // 缩放系数本身已由 lib/scale.test.ts 按 10 位精度单独钉住。
    setViewport(2560, 1440);
    expect(scaleOf(canvas)).toBeCloseTo(4 / 3, 4);
    expect(blockLayout()).toEqual(before);

    // 4:3：按宽度缩到 2/3，上下各留 152px 黑边，仍然不重排。
    setViewport(1280, 1024);
    expect(scaleOf(canvas)).toBeCloseTo(2 / 3, 4);
    expect(Math.abs(Number.parseFloat(canvas.style.left))).toBeLessThan(1);
    expect(Number.parseFloat(canvas.style.top)).toBeCloseTo(152, 6);
    expect(blockLayout()).toEqual(before);
  });
});
