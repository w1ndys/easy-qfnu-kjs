import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  RATE_LIMIT_COOLDOWN_MS,
  useRateLimitCooldown,
} from "./useRateLimitCooldown";

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("按钮冷却", () => {
  it("冷却时长是 3 秒", () => {
    expect(RATE_LIMIT_COOLDOWN_MS).toBe(3_000);
  });

  it("start() 后进入冷却，到点自动解除", () => {
    const { result } = renderHook(() => useRateLimitCooldown());

    expect(result.current.coolingDown).toBe(false);

    act(() => {
      result.current.start();
    });
    expect(result.current.coolingDown).toBe(true);

    // 差 1 毫秒仍是冷却中：不是「立刻解冻」。
    act(() => {
      vi.advanceTimersByTime(RATE_LIMIT_COOLDOWN_MS - 1);
    });
    expect(result.current.coolingDown).toBe(true);

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current.coolingDown).toBe(false);
  });

  it("冷却中再次 start() 会把结束时间往后推，而不是提前解冻", () => {
    const { result } = renderHook(() => useRateLimitCooldown());

    act(() => {
      result.current.start();
    });
    act(() => {
      vi.advanceTimersByTime(2_000);
    });

    // 第 2 个限流响应在冷却结束前到达：重新计时。
    act(() => {
      result.current.start();
    });
    act(() => {
      vi.advanceTimersByTime(2_000);
    });
    expect(result.current.coolingDown).toBe(true);

    act(() => {
      vi.advanceTimersByTime(1_000);
    });
    expect(result.current.coolingDown).toBe(false);
  });

  it("卸载时清掉未到点的计时器", () => {
    const { result, unmount } = renderHook(() => useRateLimitCooldown());

    act(() => {
      result.current.start();
    });
    expect(vi.getTimerCount()).toBe(1);

    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
