import { describe, expect, it } from "vitest";
import type { MetaAxis } from "../api/types";
import { metaFixture } from "../test/fixtures/publicApi";
import {
  BLOCK_COUNT,
  NODE_IDS,
  buildBlockLabels,
  buildNodeOptions,
  defaultNodeRange,
} from "./nodes";

/** 构造本地时间；只关心小时与分钟，日期取夹具同一天。 */
function localTime(hour: number, minute: number): Date {
  return new Date(2026, 10, 15, hour, minute);
}

describe("defaultNodeRange 三档预填", () => {
  it("11:59 属于 12:00 前的上午档，预填 01–05", () => {
    expect(defaultNodeRange(localTime(11, 59))).toEqual({
      start_node: "01",
      end_node: "05",
    });
  });

  it("12:00 整点进入下午档，预填 06–09", () => {
    expect(defaultNodeRange(localTime(12, 0))).toEqual({
      start_node: "06",
      end_node: "09",
    });
  });

  it("17:59 仍属于下午档，预填 06–09", () => {
    expect(defaultNodeRange(localTime(17, 59))).toEqual({
      start_node: "06",
      end_node: "09",
    });
  });

  it("18:00 整点进入晚间档，预填 10–12", () => {
    expect(defaultNodeRange(localTime(18, 0))).toEqual({
      start_node: "10",
      end_node: "12",
    });
  });

  it("凌晨 00:00 归入上午档", () => {
    expect(defaultNodeRange(localTime(0, 0))).toEqual({
      start_node: "01",
      end_node: "05",
    });
  });

  it("每次返回新对象，修改结果不会污染下一档位", () => {
    const range = defaultNodeRange(localTime(9, 0));
    range.start_node = "12";

    expect(defaultNodeRange(localTime(9, 0))).toEqual({
      start_node: "01",
      end_node: "05",
    });
    expect(defaultNodeRange(localTime(20, 0))).toEqual({
      start_node: "10",
      end_node: "12",
    });
  });
});

describe("buildNodeOptions 从轴派生节次选项", () => {
  it("轴存在时按 ordinal 派生 12 个选项", () => {
    const options = buildNodeOptions(metaFixture.axis);

    expect(options).toHaveLength(12);
    expect(options[0]).toEqual({ value: "01", label: "第 01 节" });
    expect(options[11]).toEqual({ value: "12", label: "第 12 节" });
  });

  it("轴数组顺序被打乱时仍按 ordinal 排序", () => {
    const shuffled = {
      ...metaFixture.axis,
      nodes: [...metaFixture.axis.nodes].reverse(),
    };

    expect(buildNodeOptions(shuffled).map((option) => option.value)).toEqual([
      ...NODE_IDS,
    ]);
  });

  it("无当前版本（轴为 null）时回退契约里的 01..12，不抛错", () => {
    const options = buildNodeOptions(null);

    expect(options).toHaveLength(12);
    expect(options[0]).toEqual({ value: "01", label: "第 01 节" });
  });
});

describe("buildBlockLabels 从轴派生大节列头", () => {
  it("按 axis.blocks 顺序把每个大节的首尾小节写成区间（末节含三小节）", () => {
    // 真实轴编码（backend/migrations/0001_baseline.sql）：第 06–09 节不是两节一个大节，
    // 最后一个大节是 101112，列头必须写成「第 10–12 节」而不是两节一段。
    expect(buildBlockLabels(metaFixture.axis)).toEqual([
      "第 01–02 节",
      "第 03–05 节",
      "第 06–07 节",
      "第 08–09 节",
      "第 10–12 节",
    ]);
  });

  it("axis.nodes 的数组顺序不影响列头，首尾取 ordinal 最小/最大", () => {
    // 反序只改数组顺序，ordinal 不变：01 仍是 0102 大节的第一节。
    const reversed: MetaAxis = {
      ...metaFixture.axis,
      nodes: [...metaFixture.axis.nodes].reverse(),
    };

    expect(buildBlockLabels(reversed)).toEqual([
      "第 01–02 节",
      "第 03–05 节",
      "第 06–07 节",
      "第 08–09 节",
      "第 10–12 节",
    ]);
  });

  it("无 meta（轴为 null）时回退「大节 1..5」而不抛错", () => {
    expect(buildBlockLabels(null)).toEqual([
      "大节 1",
      "大节 2",
      "大节 3",
      "大节 4",
      "大节 5",
    ]);
  });

  it("轴为空数组时同样回退，且恒为 5 列", () => {
    const emptyAxis: MetaAxis = { version: 1, blocks: [], nodes: [] };

    expect(buildBlockLabels(emptyAxis)).toHaveLength(BLOCK_COUNT);
    expect(buildBlockLabels(emptyAxis)).toEqual([
      "大节 1",
      "大节 2",
      "大节 3",
      "大节 4",
      "大节 5",
    ]);
  });

  it("轴只覆盖部分大节时逐列回退，前面的列仍然派生", () => {
    const partialAxis: MetaAxis = {
      version: 1,
      blocks: ["0102"],
      nodes: metaFixture.axis.nodes.filter((node) => node.block === "0102"),
    };

    expect(buildBlockLabels(partialAxis)).toEqual([
      "第 01–02 节",
      "大节 2",
      "大节 3",
      "大节 4",
      "大节 5",
    ]);
  });

  it("blocks 缺失（只给 nodes）时按 block_ordinal 排大节顺序", () => {
    const nodesOnlyAxis: MetaAxis = {
      version: 1,
      blocks: [],
      nodes: [...metaFixture.axis.nodes].reverse(),
    };

    expect(buildBlockLabels(nodesOnlyAxis)).toEqual([
      "第 01–02 节",
      "第 03–05 节",
      "第 06–07 节",
      "第 08–09 节",
      "第 10–12 节",
    ]);
  });
});
