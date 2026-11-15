import { describe, expect, it } from "vitest";
import { metaFixture } from "../test/fixtures/publicApi";
import { NODE_IDS, buildNodeOptions, defaultNodeRange } from "./nodes";

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
