import { act, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ScreenLayout } from "../../layouts/ScreenLayout";
import {
  emptyScreenFixture,
  screenFixture,
} from "../../test/fixtures/publicApi";
import { ScreenPage } from "./ScreenPage";

// 大屏布局与信息区块（tasklist 第 7 项）。
// 两条验收点在这里落地：
// 1) 五个业务区块 + 占用占位区渲染，以及 empty=true 的「保留标题 + 暂无数据」；
// 2) 窗口 1920×1080 → 2560×1440 → 1280×1024 只改缩放系数，区块顺序与相对位置不变；
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

function stubScreen(response: () => Response): URL[] {
  const requests: URL[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = new URL(String(input), "http://localhost");
      requests.push(url);
      return response();
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
  it("渲染站点规模、数据版本、逐周新鲜度、最近一轮与异常计数五块", async () => {
    const requests = stubScreen(() => envelope(screenFixture));

    renderScreenPage();

    const scaleBlock = await screen.findByRole("region", { name: "站点规模" });
    expect(requests).toHaveLength(1);
    expect(requests[0].pathname).toBe("/api/screen");
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

    // 有数据时占用占位区仍是第 8 项的「尚待接入」，不因空态分支改成「暂无数据」。
    const occupancyBlock = screen.getByRole("region", { name: "占用分布" });
    expect(
      within(occupancyBlock).getByText("占用热力图与按楼占用率尚待接入。"),
    ).toBeInTheDocument();
    expect(
      within(occupancyBlock).queryByText("暂无数据"),
    ).not.toBeInTheDocument();

    // 信封里的 message 不是可展示内容，页面上也不出现业务码或错误字段名。
    expect(screen.queryByText("success")).not.toBeInTheDocument();
    for (const secret of ["50000", "error_code", "request_id"]) {
      expect(screen.queryByText(new RegExp(secret))).not.toBeInTheDocument();
    }
  });

  it("empty=true 时五个业务区块与占用占位区都保留标题并显示「暂无数据」", async () => {
    stubScreen(() => envelope(emptyScreenFixture));

    renderScreenPage();

    await screen.findByRole("region", { name: "站点规模" });

    // 「占用分布」是第 8 项的实现位，这里同样跟随空态：保留标题，正文换成
    // 「暂无数据」，不再显示「尚待接入」——整页空态只有一种文案。
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
    stubScreen(() =>
      errorEnvelope(
        50001,
        "error_code=UPSTREAM_401 upstream token expired",
        "req_screen_secret",
      ),
    );

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

    // 失败时不画半截数据：五个数据区块一个都没有。
    expect(
      screen.queryByRole("region", { name: "站点规模" }),
    ).not.toBeInTheDocument();
  });
});

describe("等比缩放与布局稳定性", () => {
  it("窗口从 1920×1080 变 2560×1440 / 1280×1024 时只改缩放系数，区块顺序与相对位置不变", async () => {
    stubScreen(() => envelope(screenFixture));

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
