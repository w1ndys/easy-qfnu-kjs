// 大屏轮询与降级（tasklist 第 9 项；design D9、Requirement 7）。
//
// 为什么不用 useApi：useApi 的状态模型是「请求键一变就丢弃旧结果」，而大屏要的恰好相反——
// 新请求在途、甚至失败，都要继续展示上一次成功的数据。这里只有一条轮询循环，
// 没有请求键，也没有任何前端缓存（正确性属性 6）：每一拍都真实请求 /api/screen，
// 30 秒内的复用交给后端缓存（design CP15）。
//
// 状态语义：
// - data：最后一次成功的数据；在途与失败都不清空（Requirement 7.2）；
// - error：只在「还没有任何数据」时记录失败，页面据此显示错误占位与重试；
//   已有数据时失败只打离线标记，不把内容换成错误页；
// - offline：最近一次请求失败即置位，下一次成功清除（Requirement 7.3）；
// - lastSuccessAt：最后一次成功的客户端接收时刻（RFC3339 UTC），顶栏按上海时区展示。

import { useCallback, useEffect, useState } from "react";
import {
  isAbortError,
  toApiError,
  type ApiError,
  type ApiResult,
} from "../api/client";
import { getScreen } from "../api/public";
import type { ScreenResult } from "../api/types";

/** 轮询间隔：与后端 /api/screen 的 30 秒缓存对齐（design D9）。 */
export const SCREEN_POLL_INTERVAL_MS = 30_000;

export interface ScreenPollingState {
  /** 最后一次成功的数据；首屏失败或尚未返回时为 null。 */
  data: ScreenResult | null;
  /** 首屏（或还没有数据时的重试）失败；有数据时轮询失败不会写入这里。 */
  error: ApiError | null;
  /** 还没有任何数据、且首屏请求未定格。 */
  loading: boolean;
  /** 最近一次轮询失败：顶栏据此显示离线徽标。 */
  offline: boolean;
  /** 最后一次成功的接收时刻（RFC3339 UTC）；还没有成功过时为 null。 */
  lastSuccessAt: string | null;
  /** 手动重试：首屏失败页的「重试」用，立即重发一次并重新对齐 30 秒节奏。 */
  retry: () => void;
}

interface PollingSnapshot {
  data: ScreenResult | null;
  error: ApiError | null;
  loading: boolean;
  offline: boolean;
  lastSuccessAt: string | null;
}

const INITIAL_SNAPSHOT: PollingSnapshot = {
  data: null,
  error: null,
  loading: true,
  offline: false,
  lastSuccessAt: null,
};

/** 请求函数放在模块级，引用稳定，轮询循环不需要把它放进依赖。 */
function loadScreen(signal: AbortSignal): Promise<ApiResult<ScreenResult>> {
  return getScreen(signal);
}

/** 页面在后台时不发请求；没有 document 的环境按可见处理。 */
function pageHidden(): boolean {
  return (
    typeof document !== "undefined" && document.visibilityState === "hidden"
  );
}

/**
 * 数据大屏的 30 秒轮询。
 *
 * 循环规则（Requirement 7.1 / 7.4）：
 * - 挂载且可见时立即请求一次，之后每 30 秒一次；
 * - `visibilitychange` 隐藏：停表并取消在途请求（结果已不再需要，也不算失败）；
 * - 重新可见：立即请求一次，然后从这一刻重新对齐 30 秒节奏；
 * - 卸载：停表、取消在途、移监听，异步回调靠 `active` 判定丢弃。
 *
 * 同一时刻最多一个在途请求：上一拍还没回来时，这一拍直接跳过，不叠加请求，
 * 也绝不因为有旧数据就跳过真实请求（不写前端缓存）。
 */
export function useScreenPolling(): ScreenPollingState {
  const [snapshot, setSnapshot] = useState<PollingSnapshot>(INITIAL_SNAPSHOT);
  // 手动重试靠自增序号重跑整个轮询 effect（与 useApi 的 reloadSeq 同一手法）。
  const [retrySeq, setRetrySeq] = useState(0);

  useEffect(() => {
    let disposed = false;
    let timer: ReturnType<typeof setInterval> | null = null;
    /** 当前在途请求；非 null 即「有一拍还没回来」。 */
    let active: AbortController | null = null;

    const stopTimer = () => {
      if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };

    /**
     * 取消在途请求。先把 active 清空再 abort：请求的拒绝回调据此知道
     * 「这次结果已被取消/替换」，不会再写状态，也不会把新请求的在途标记抹掉。
     */
    const cancelInFlight = () => {
      const controller = active;
      active = null;
      controller?.abort();
    };

    const poll = () => {
      if (disposed || active !== null || pageHidden()) {
        return;
      }

      const controller = new AbortController();
      active = controller;

      loadScreen(controller.signal).then(
        (result) => {
          if (active !== controller) {
            return;
          }
          active = null;
          setSnapshot({
            data: result.data,
            error: null,
            loading: false,
            offline: false,
            // 客户端接收时刻：这才是「大屏最后一次刷新」的时间，不是服务端生成时间。
            lastSuccessAt: new Date().toISOString(),
          });
        },
        (error: unknown) => {
          if (active !== controller) {
            return;
          }
          active = null;
          // 取消只代表这次结果不要了：不置离线、不写错误态。
          if (isAbortError(error)) {
            return;
          }
          const apiError = toApiError(error);
          setSnapshot((previous) => ({
            ...previous,
            loading: false,
            offline: true,
            // 首屏失败才进错误占位；已有数据时保留旧数据，只留下离线标记。
            error: previous.data === null ? apiError : previous.error,
          }));
        },
      );
    };

    const startTimer = () => {
      stopTimer();
      timer = setInterval(poll, SCREEN_POLL_INTERVAL_MS);
    };

    const handleVisibilityChange = () => {
      if (pageHidden()) {
        // 隐藏即暂停：停表并取消在途请求；已渲染的数据保持不动。
        stopTimer();
        cancelInFlight();
        return;
      }
      // 重新可见：立即补一次，再从此刻重新对齐 30 秒节奏。
      poll();
      startTimer();
    };

    document.addEventListener("visibilitychange", handleVisibilityChange);

    // 挂载时就不可见（后台标签页打开）：等重新可见时再发首次请求。
    if (!pageHidden()) {
      poll();
      startTimer();
    }

    return () => {
      disposed = true;
      stopTimer();
      cancelInFlight();
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, [retrySeq]);

  const retry = useCallback(() => {
    // 首屏失败后点重试：回到加载态，避免错误占位一直停到结果返回。
    setSnapshot((previous) =>
      previous.data === null
        ? { ...previous, loading: true, error: null }
        : previous,
    );
    setRetrySeq((current) => current + 1);
  }, []);

  return { ...snapshot, retry };
}
