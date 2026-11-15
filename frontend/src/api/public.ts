// 五个公开端点的薄封装：只拼路径与查询参数，不做任何业务逻辑。
// 页面与 hooks 不直接 fetch；参数名按后端契约显式写出，客户端不做大小写转换。

import { request, type ApiResult } from "./client";
import type {
  AvailabilityResult,
  ContextInfo,
  DayResult,
  MetaResult,
  ScreenResult,
} from "./types";

/** GET /api/context：当天日期、学期、周次、星期与是否在教学周。 */
export function getContext(
  signal?: AbortSignal,
): Promise<ApiResult<ContextInfo>> {
  return request<ContextInfo>("/api/context", { signal });
}

/** GET /api/meta：版本、字典、节次轴、教学楼白名单与逐周新鲜度。 */
export function getMeta(signal?: AbortSignal): Promise<ApiResult<MetaResult>> {
  return request<MetaResult>("/api/meta", { signal });
}

/** 空教室查询的请求参数；键名即后端参数名，不做 camelCase 转换。 */
export interface AvailabilityQuery {
  date_offset: number; // 0..10，目标日期相对今天的天数
  building?: string; // 空/未传表示全部
  start_node?: string; // 01..12，省略时后端默认 01
  end_node?: string; // 01..12，省略时后端默认 11
  keyword?: string; // ≤32 字，服务端归一
  limit?: number; // 1..200，后端默认 50
  offset?: number; // ≥ 0
}

/** GET /api/query?view=availability：区间内空闲的房间。 */
export function queryAvailability(
  params: AvailabilityQuery,
  signal?: AbortSignal,
): Promise<ApiResult<AvailabilityResult>> {
  return request<AvailabilityResult>("/api/query", {
    query: { view: "availability", ...params },
    signal,
  });
}

/** 全天状态查询的请求参数；room_id 必填。 */
export interface DayQuery {
  date_offset: number; // 0..10
  room_id: string; // 房间身份 jsbh，trim 后不能为空
  building?: string; // 非空时房间必须归属于该楼
}

/** GET /api/query?view=day：一间教室一天的 12 小节状态。 */
export function queryDay(
  params: DayQuery,
  signal?: AbortSignal,
): Promise<ApiResult<DayResult>> {
  return request<DayResult>("/api/query", {
    query: { view: "day", ...params },
    signal,
  });
}

/**
 * GET /api/screen：数据大屏聚合（后端缓存 30 秒）。
 * 本 spec 不传 peak_days（默认近 7 日），访问统计块也不在页面渲染范围内。
 */
export function getScreen(
  signal?: AbortSignal,
): Promise<ApiResult<ScreenResult>> {
  return request<ScreenResult>("/api/screen", { signal });
}
