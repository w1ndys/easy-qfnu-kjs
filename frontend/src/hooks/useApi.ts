// 页面级请求状态（design「状态管理」）：一次请求的 data / error / loading / request_id，
// 并把「筛选切换时取消上一个请求」收在一处（Requirement 1.5）。
// 不引 react-query：两个页面、无写操作，请求状态跟随组件生命周期即可（design D2）。

import { useCallback, useEffect, useState } from "react";
import { ApiError, type ApiResult } from "../api/client";
import {
  NETWORK_ERROR_CODE,
  UNKNOWN_REQUEST_ID,
  getErrorTitle,
} from "../api/errors";

/** 已完成（成功或失败）的那次请求在页面上的定格结果。 */
interface SettledState<T> {
  /** 该结果对应的请求键；与当前请求键不一致说明已有更新的请求在途。 */
  key: string | null;
  data: T | null;
  error: ApiError | null;
  requestId: string | null;
}

/** 供页面消费的请求状态：loading 由「请求键是否已定格」派生，不需要在 effect 里同步置位。 */
export interface ApiRequestState<T> {
  data: T | null;
  error: ApiError | null;
  requestId: string | null;
  /** 已启用但当前请求键还没有结果（首次加载、改筛选、手动重查都算）。 */
  loading: boolean;
  /** 用同一个请求键再发一次（查询按钮/回车在筛选未变时靠它触发）。 */
  reload: () => void;
}

/** fetch 被取消时抛的是 AbortError；它只表示「这次结果不要了」，不该进错误态。 */
function isAbortError(error: unknown): boolean {
  return (
    typeof error === "object" &&
    error !== null &&
    "name" in error &&
    (error as { name?: unknown }).name === "AbortError"
  );
}

/** 不该出现的非 ApiError（编程错误等）也收敛成一层兜底，页面永远只处理 ApiError。 */
function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) {
    return error;
  }
  return new ApiError(
    NETWORK_ERROR_CODE,
    UNKNOWN_REQUEST_ID,
    getErrorTitle(NETWORK_ERROR_CODE),
  );
}

/**
 * 发一次页面级请求并跟踪状态。
 *
 * - `requestKey` 必须能唯一标识这次请求的内容（页面用规范化后的筛选串）；它同时是
 *   loading 的判定依据，所以请求键一变，旧结果立即不再参与渲染，不会闪旧列表。
 * - `load` 必须是稳定引用（页面用 `useCallback` 包住），它是请求内容的真实来源。
 * - `enabled=false` 时完全不发请求（元数据未就绪、URL 尚未规范化时用）。
 * - 依赖变化与卸载都会 abort 上一次请求；AbortError 静默丢弃，不写错误态。
 */
export function useApi<T>(
  requestKey: string,
  load: (signal: AbortSignal) => Promise<ApiResult<T>>,
  enabled = true,
): ApiRequestState<T> {
  // 手动重查靠自增序号换请求键：筛选没变时也能再发一次同一个请求。
  const [reloadSeq, setReloadSeq] = useState(0);
  const [settled, setSettled] = useState<SettledState<T>>({
    key: null,
    data: null,
    error: null,
    requestId: null,
  });

  const activeKey = `${reloadSeq}\u0000${requestKey}`;

  useEffect(() => {
    if (!enabled) {
      return;
    }

    const controller = new AbortController();
    let cancelled = false;

    load(controller.signal).then(
      (result) => {
        if (cancelled) {
          return;
        }
        setSettled({
          key: activeKey,
          data: result.data,
          error: null,
          requestId: result.requestId,
        });
      },
      (error: unknown) => {
        if (cancelled || isAbortError(error)) {
          return;
        }
        setSettled({
          key: activeKey,
          data: null,
          error: toApiError(error),
          requestId: null,
        });
      },
    );

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [activeKey, enabled, load]);

  const reload = useCallback(() => {
    setReloadSeq((current) => current + 1);
  }, []);

  const isSettled = settled.key === activeKey;

  return {
    // 未定格的旧结果不暴露给页面：改筛选后结果区应进加载态，而不是继续显示上一次的列表。
    data: isSettled ? settled.data : null,
    error: isSettled ? settled.error : null,
    requestId: isSettled ? settled.requestId : null,
    loading: enabled && !isSettled,
    reload,
  };
}
