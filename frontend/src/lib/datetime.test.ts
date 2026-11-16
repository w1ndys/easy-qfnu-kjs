import { describe, expect, it } from "vitest";
import { formatShanghaiDateTime } from "./datetime";

describe("RFC3339 UTC → Asia/Shanghai", () => {
  it("同一 UTC 时间内按上海时区（+08:00）展示到分钟", () => {
    expect(formatShanghaiDateTime("2026-11-14T07:10:12Z")).toBe(
      "2026-11-14 15:10",
    );
  });

  it("跨日的 UTC 时间落到上海时区的次日", () => {
    // 20:30Z 已经是上海的次日 04:30：跨日由 timeZone 处理，不靠手动加 8 小时。
    expect(formatShanghaiDateTime("2026-11-14T20:30:00Z")).toBe(
      "2026-11-15 04:30",
    );
  });

  it("上海时区午夜补零为 00:00，不出现 24:00", () => {
    expect(formatShanghaiDateTime("2026-11-14T16:00:00Z")).toBe(
      "2026-11-15 00:00",
    );
  });
});

describe("异常输入", () => {
  it.each([null, undefined, "", "not-a-time", "2026-13-45T00:00:00Z"])(
    "空值或不可解析的输入 %s 返回 null",
    (value) => {
      expect(formatShanghaiDateTime(value)).toBeNull();
    },
  );
});
