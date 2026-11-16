import { describe, expect, it } from "vitest";
import {
  OCCUPANCY_BUCKET_COUNT,
  OCCUPANCY_FILL_OPACITY,
  formatRatePercent,
  occupancyBucket,
} from "./occupancy";

// 色阶边界（tasklist 第 8 项验收）：0、0.5、1 各一条，另加档位内部与越界值。
// 分档是纯函数，这里只断言档位，不断言色值——颜色由组件从 antd token 取。

describe("occupancyBucket 色阶分档", () => {
  it("边界 0：占用率为 0 单独落在中性档", () => {
    expect(occupancyBucket(0)).toBe(0);
    // -0 也是 0，不能因为符号位掉进别的档。
    expect(occupancyBucket(-0)).toBe(0);
  });

  it("边界 0.5：恰好 50% 落在第 2 档（图例的 50% 色块）", () => {
    expect(occupancyBucket(0.5)).toBe(2);
  });

  it("边界 1：满占用落在最高档，越界值不产生第 5 档", () => {
    expect(occupancyBucket(1)).toBe(4);
    expect(occupancyBucket(1.5)).toBe(4);
  });

  it("任何正值至少进第 1 档，与「0 占用 / 无观测」的中性档区分开", () => {
    expect(occupancyBucket(0.0001)).toBe(1);
    expect(occupancyBucket(0.1)).toBe(1);
    // 0.25 是第 1 档的上界，再大一点就进第 2 档。
    expect(occupancyBucket(0.25)).toBe(1);
    expect(occupancyBucket(0.2501)).toBe(2);
    expect(occupancyBucket(0.75)).toBe(3);
    expect(occupancyBucket(0.7501)).toBe(4);
  });

  it("负值与 NaN 归中性档，不抛错", () => {
    expect(occupancyBucket(-0.2)).toBe(0);
    expect(occupancyBucket(Number.NaN)).toBe(0);
    expect(occupancyBucket(Number.POSITIVE_INFINITY)).toBe(4);
  });

  it("透明度表与图例 5 档一一对应", () => {
    expect(OCCUPANCY_FILL_OPACITY).toHaveLength(OCCUPANCY_BUCKET_COUNT);
    expect([...OCCUPANCY_FILL_OPACITY]).toEqual([1, 0.25, 0.5, 0.75, 1]);
  });
});

describe("formatRatePercent 百分比文案", () => {
  it("四舍五入到整数百分比", () => {
    expect(formatRatePercent(0)).toBe("0%");
    expect(formatRatePercent(0.5)).toBe("50%");
    expect(formatRatePercent(1)).toBe("100%");
    expect(formatRatePercent(0.424)).toBe("42%");
    expect(formatRatePercent(0.005)).toBe("1%");
  });

  it("越界值钳到 0–100%，非有限值给占位", () => {
    expect(formatRatePercent(-0.2)).toBe("0%");
    expect(formatRatePercent(1.2)).toBe("100%");
    expect(formatRatePercent(Number.NaN)).toBe("—");
  });
});
