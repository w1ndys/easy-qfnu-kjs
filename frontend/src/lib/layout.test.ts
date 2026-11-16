import { describe, expect, it } from "vitest";
import {
  COMPACT_MAX_WIDTH,
  publicLayout,
  viewportTier,
  WIDE_MIN_WIDTH,
} from "./layout";

// 三档断点（tasklist 第 10 项；Requirement 8.1–8.3）。
// 断点口径是 <600 / 600–1024 / ≥1024，所以 599/600 与 1023/1024 四个边界各钉一条。

describe("viewportTier 断点", () => {
  it.each<[width: number, tier: "compact" | "medium" | "wide"]>([
    [320, "compact"],
    [COMPACT_MAX_WIDTH - 1, "compact"],
    [COMPACT_MAX_WIDTH, "medium"],
    [800, "medium"],
    [WIDE_MIN_WIDTH - 1, "medium"],
    [WIDE_MIN_WIDTH, "wide"],
    [2560, "wide"],
  ])("%dpx → %s", (width, tier) => {
    expect(viewportTier(width)).toBe(tier);
  });

  it("拿不到真实宽度（NaN / Infinity / 负数）时退化为单列，而不是算出 NaN 列宽", () => {
    expect(viewportTier(Number.NaN)).toBe("compact");
    expect(viewportTier(Number.POSITIVE_INFINITY)).toBe("compact");
    expect(viewportTier(-1)).toBe("compact");
  });
});

describe("publicLayout 三档布局", () => {
  it("<600px 单列且查询按钮固定在底部", () => {
    expect(publicLayout("compact")).toMatchObject({
      columns: 1,
      filterSpan: 1,
      resultSpan: 1,
      resultColumns: 1,
      fixedQueryBar: true,
    });
  });

  it("600–1024px 双列：筛选左、结果右，按钮回到筛选区内", () => {
    expect(publicLayout("medium")).toMatchObject({
      columns: 2,
      filterSpan: 1,
      resultSpan: 1,
      resultColumns: 1,
      fixedQueryBar: false,
    });
  });

  it("≥1024px 三列：筛选 1 列 + 结果 2 列，结果区内部再分两列", () => {
    expect(publicLayout("wide")).toMatchObject({
      columns: 3,
      filterSpan: 1,
      resultSpan: 2,
      resultColumns: 2,
      fixedQueryBar: false,
    });
  });

  it("双列与三列下筛选列 + 结果列正好铺满栅格，不出现空洞或溢出", () => {
    for (const tier of ["medium", "wide"] as const) {
      const layout = publicLayout(tier);
      expect(layout.filterSpan + layout.resultSpan).toBe(layout.columns);
    }
  });
});
