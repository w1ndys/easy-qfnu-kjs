import { describe, expect, it } from "vitest";
import {
  FALLBACK_TITLE,
  NETWORK_ERROR_CODE,
  UNKNOWN_REQUEST_ID,
  getErrorTitle,
  isRetriableCode,
} from "./errors";

describe("业务码 → 文案", () => {
  it.each([
    [40001, "参数有误"],
    [40401, "数据不存在"],
    [42901, "请求过于频繁，请稍后重试"],
    [50000, "服务异常"],
  ])("%i 映射为「%s」", (code, title) => {
    expect(getErrorTitle(code)).toBe(title);
  });

  it("后端未定义给前端的码（如管理端 40101）兜底为服务异常", () => {
    expect(getErrorTitle(40101)).toBe(FALLBACK_TITLE);
    expect(getErrorTitle(42201)).toBe(FALLBACK_TITLE);
    expect(getErrorTitle(0)).toBe(FALLBACK_TITLE);
  });

  it("网络失败单独归类为网络异常", () => {
    expect(getErrorTitle(NETWORK_ERROR_CODE)).toBe("网络异常，请检查连接");
  });

  it("request_id 缺失时的占位是「未知」", () => {
    expect(UNKNOWN_REQUEST_ID).toBe("未知");
  });
});

describe("可重试性", () => {
  it("参数错误与数据不存在不值得重试", () => {
    expect(isRetriableCode(40001)).toBe(false);
    expect(isRetriableCode(40401)).toBe(false);
  });

  it("限流、内部错误、未知码与网络失败都可重试", () => {
    expect(isRetriableCode(42901)).toBe(true);
    expect(isRetriableCode(50000)).toBe(true);
    expect(isRetriableCode(40101)).toBe(true);
    expect(isRetriableCode(NETWORK_ERROR_CODE)).toBe(true);
  });
});
