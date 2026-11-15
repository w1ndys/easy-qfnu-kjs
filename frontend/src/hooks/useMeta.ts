// 页面上下文 + 元数据的一次性加载（tasklist 第 3 项；design「状态管理」）。
// 两个端点并行请求，两项都到齐才算就绪；任一失败都由 useApi 收敛成 ApiError，
// 页面级重试只需再调一次 reload。

import { useCallback } from "react";
import type { ApiError, ApiResult } from "../api/client";
import { getContext, getMeta } from "../api/public";
import type { ContextInfo, MetaResult } from "../api/types";
import { useApi } from "./useApi";

/** 公开查询页需要的两份基础数据：当天上下文与数据版本/字典/轴/楼白名单。 */
export interface PublicMeta {
  context: ContextInfo;
  meta: MetaResult;
}

export interface UseMetaResult {
  context: ContextInfo | null;
  meta: MetaResult | null;
  /** 两项都到齐前为 true；页面据此显示加载态（Requirement 2.1）。 */
  loading: boolean;
  /** 失败时带业务码与 request_id，页面直接展示即可（Requirement 1.6）。 */
  error: ApiError | null;
  reload: () => void;
}

const META_REQUEST_KEY = "public-meta";

/**
 * 并行取 `/api/context` 与 `/api/meta`；由 useApi 负责取消、重试与错误收敛。
 * 两项都在同一轮里发，串行会把首屏加载时间翻倍；任一失败则整体失败，
 * 因为缺字典或楼白名单时筛选区无法工作（design「Error Handling」）。
 */
export function useMeta(): UseMetaResult {
  const load = useCallback(
    async (signal: AbortSignal): Promise<ApiResult<PublicMeta>> => {
      const [context, meta] = await Promise.all([
        getContext(signal),
        getMeta(signal),
      ]);
      return {
        data: { context: context.data, meta: meta.data },
        // 成功时页面不展示 request_id；失败时以 ApiError 自带的那次为准。
        requestId: context.requestId,
      };
    },
    [],
  );

  const state = useApi<PublicMeta>(META_REQUEST_KEY, load);

  return {
    context: state.data?.context ?? null,
    meta: state.data?.meta ?? null,
    loading: state.loading,
    error: state.error,
    reload: state.reload,
  };
}
