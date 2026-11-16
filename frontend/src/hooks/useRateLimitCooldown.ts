// 42901 限流的按钮冷却（tasklist 第 6 项；design「Error Handling」：查询按钮禁用 3 秒后恢复）。
//
// 为什么做成 hook 而不是在渲染体里写 `Date.now() < until`：
// 渲染期读时钟既不确定也被 eslint 的 react-hooks/purity 判错。这里的状态只有「是否冷却」
// 一个布尔，由 `start()` 打开、由 `setTimeout` 到点关闭——渲染全程不读时间。

import { useCallback, useEffect, useRef, useState } from "react";

/** 冷却时长：3 秒（design「Error Handling」的 42901 口径）。 */
export const RATE_LIMIT_COOLDOWN_MS = 3_000;

export interface RateLimitCooldown {
  /** 冷却中：查询按钮应禁用。 */
  coolingDown: boolean;
  /** 开始（或重新开始）一次冷却；重复调用会把结束时间往后推，不会提前解冻。 */
  start: () => void;
}

/**
 * 限流冷却计时器。
 *
 * `start()` 由请求失败的异步出口调用（不是渲染，也不是 effect 的同步段），
 * 到点后的解锁在 `setTimeout` 回调里翻转布尔。卸载时清掉计时器，
 * 避免组件已经不在页面上还去 setState。
 */
export function useRateLimitCooldown(
  durationMs: number = RATE_LIMIT_COOLDOWN_MS,
): RateLimitCooldown {
  const [coolingDown, setCoolingDown] = useState(false);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const start = useCallback(() => {
    setCoolingDown(true);
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
    }
    timerRef.current = setTimeout(() => {
      timerRef.current = null;
      setCoolingDown(false);
    }, durationMs);
  }, [durationMs]);

  useEffect(
    () => () => {
      if (timerRef.current !== null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
    },
    [],
  );

  return { coolingDown, start };
}
