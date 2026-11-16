import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { setMediaQueryMatches } from "../test/matchMedia";
import { REDUCED_MOTION_QUERY, useReducedMotion } from "./useReducedMotion";

// 减少动态效果（tasklist 第 10 项；Requirement 8 的「prefers-reduced-motion 关闭过渡」）。
// 系统偏好由可控的 matchMedia 垫片驱动，同时验证「挂载时读取」与「运行中变化跟随」。

describe("useReducedMotion", () => {
  it("系统未开启减少动态效果时为 false", () => {
    const { result } = renderHook(() => useReducedMotion());

    expect(result.current).toBe(false);
  });

  it("系统开启减少动态效果时挂载即为 true", () => {
    act(() => {
      setMediaQueryMatches(REDUCED_MOTION_QUERY, true);
    });

    const { result } = renderHook(() => useReducedMotion());

    expect(result.current).toBe(true);
  });

  it("运行中切换系统设置会实时跟随，不需要刷新页面", () => {
    const { result } = renderHook(() => useReducedMotion());
    expect(result.current).toBe(false);

    act(() => {
      setMediaQueryMatches(REDUCED_MOTION_QUERY, true);
    });
    expect(result.current).toBe(true);

    act(() => {
      setMediaQueryMatches(REDUCED_MOTION_QUERY, false);
    });
    expect(result.current).toBe(false);
  });
});
