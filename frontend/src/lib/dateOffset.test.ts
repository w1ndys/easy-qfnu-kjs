import { describe, expect, it } from "vitest";
import { addDays, dateOptionLabel, formatMonthDay } from "./dateOffset";

describe("addDays 业务日期加减", () => {
  it("同月内加天数", () => {
    expect(addDays("2026-11-15", 2)).toBe("2026-11-17");
  });

  it("跨月进位", () => {
    expect(addDays("2026-11-30", 2)).toBe("2026-12-02");
  });

  it("跨年进位", () => {
    expect(addDays("2026-12-31", 3)).toBe("2027-01-03");
  });

  it("闰年 2 月 29 日存在，平年 2 月 28 日的下一天是 3 月 1 日", () => {
    expect(addDays("2028-02-28", 2)).toBe("2028-03-01");
    expect(addDays("2027-02-28", 1)).toBe("2027-03-01");
  });

  it("非法日期原样返回，不静默纠正", () => {
    expect(addDays("2026-02-30", 1)).toBe("2026-02-30");
    expect(addDays("2026/11/15", 1)).toBe("2026/11/15");
  });
});

describe("dateOptionLabel 日期项文案", () => {
  it("偏移 0 显示今天、1 显示明天，其余显示目标日期的 M-D", () => {
    expect(dateOptionLabel("2026-11-15", 0)).toBe("今天");
    expect(dateOptionLabel("2026-11-15", 1)).toBe("明天");
    expect(dateOptionLabel("2026-11-15", 2)).toBe("11-17");
    expect(dateOptionLabel("2026-11-15", 10)).toBe("11-25");
  });
});

describe("formatMonthDay 月日格式", () => {
  it("不补前导零（需求写法 M-D）", () => {
    expect(formatMonthDay("2026-12-05")).toBe("12-5");
    expect(formatMonthDay("2027-01-10")).toBe("1-10");
  });
});
