import {
  NETWORK_ERROR_CODE,
  UNKNOWN_REQUEST_ID,
  isRetriableCode,
} from "./errors";
import type { Envelope } from "./types";

/** 前端请求超时：15 秒（tasklist 第 2 项；design「API 客户端」）。 */
export const REQUEST_TIMEOUT_MS = 15_000;

/**
 * 查询串取值。键名由调用点按后端契约显式给出（`date_offset` / `room_id` / `start_node` …），
 * 客户端只做 URL 编码，不做 camelCase ↔ snake_case 的自动转换。
 */
export type QueryParams = Record<string, string | number | undefined>;

export interface RequestOptions {
  query?: QueryParams;
  /** 筛选切换、离开页面时用的取消信号；触发后原样抛 AbortError，不进入错误态。 */
  signal?: AbortSignal;
}

/** 成功请求的结果：除 data 外带回本次响应的 request_id（Requirement 1.2）。 */
export interface ApiResult<T> {
  data: T;
  requestId: string;
}

/**
 * 一次请求的失败统一长这样：业务码、request_id 与是否可重试。
 * 网络/解析失败也收敛到本类，只有「调用方主动取消」的 AbortError 不会变成它。
 */
export class ApiError extends Error {
  readonly code: number;
  readonly requestId: string;
  readonly retriable: boolean;

  constructor(code: number, requestId: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.requestId = requestId;
    this.retriable = isRetriableCode(code);
  }
}

/** 信封解析失败的业务码：与 50000 同路径（design「Error Handling」）。 */
const PARSE_ERROR_CODE = 50000;

/**
 * 拼接查询串。空串按「未传」处理（后端口径里 building/keyword 为空即不限），
 * 0 与其它假值照常保留。
 */
function buildUrl(path: string, query?: QueryParams): string {
  if (!query) {
    return path;
  }

  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === "") {
      continue;
    }
    search.append(key, String(value));
  }

  const queryString = search.toString();
  return queryString ? `${path}?${queryString}` : path;
}

/**
 * 把响应正文解析成信封；不是 JSON 或不是信封形状时返回 null。
 * 这里刻意不抛原始语法错误：调用方只需要知道「没拿到可用信封」。
 */
function parseEnvelope<T>(raw: string): Envelope<T> | null {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }

  if (typeof value !== "object" || value === null) {
    return null;
  }

  const candidate = value as Partial<Envelope<T>>;
  if (typeof candidate.code !== "number" || !Number.isInteger(candidate.code)) {
    return null;
  }

  const requestId =
    typeof candidate.request_id === "string" ? candidate.request_id : "";
  // 成功信封必须带 data：缺了就没有东西可渲染，按解析失败处理而不是返回 undefined。
  if (
    candidate.code === 0 &&
    (candidate.data === undefined || candidate.data === null)
  ) {
    return null;
  }

  return {
    code: candidate.code,
    message: typeof candidate.message === "string" ? candidate.message : "",
    data: candidate.data ?? null,
    request_id: requestId,
  };
}

/**
 * 发一次公开接口请求：15 秒超时 + 调用方取消信号、解析信封、按业务码判成败。
 *
 * - 成败只看信封 `code`（Requirement 1.4），HTTP 非 2xx 也会先尝试解析信封；
 * - 非 JSON、非信封形状与网络/超时都收敛成 ApiError，不会把原始异常漏给页面；
 * - 调用方取消（AbortError）原样抛出，上层据此识别为「取消」而不是错误态；
 * - 超时与取消合并不依赖 `AbortSignal.any` / `AbortSignal.timeout`（Safari 17.4、
 *   Firefox 124 才有前者），自己用 `AbortController` + `setTimeout` 合并，见 design「API 客户端」。
 */
export async function request<T>(
  path: string,
  options: RequestOptions = {},
): Promise<ApiResult<T>> {
  const controller = new AbortController();
  const callerSignal = options.signal;
  // 记录是「我们主动超时」，避免把超时误判成调用方取消，或反之。
  let timedOut = false;

  const timeoutId = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, REQUEST_TIMEOUT_MS);

  const abortFromCaller = () => controller.abort(callerSignal?.reason);
  if (callerSignal) {
    if (callerSignal.aborted) {
      abortFromCaller();
    } else {
      callerSignal.addEventListener("abort", abortFromCaller, { once: true });
    }
  }

  let raw: string;
  try {
    const response = await fetch(buildUrl(path, options.query), {
      signal: controller.signal,
      headers: { Accept: "application/json" },
    });
    raw = await response.text();
  } catch (error) {
    // 调用方主动取消：原样抛出 AbortError，交给上层静默丢弃（Requirement 1.5）。
    if (callerSignal?.aborted) {
      throw error;
    }
    if (timedOut) {
      throw new ApiError(NETWORK_ERROR_CODE, UNKNOWN_REQUEST_ID, "请求超时");
    }
    throw new ApiError(
      NETWORK_ERROR_CODE,
      UNKNOWN_REQUEST_ID,
      "网络异常，请检查连接",
    );
  } finally {
    // 无论成败都收尾：计时器必须清掉，监听器不能挂在调用方长生命周期的 signal 上。
    clearTimeout(timeoutId);
    callerSignal?.removeEventListener("abort", abortFromCaller);
  }

  const envelope = parseEnvelope<T>(raw);
  if (!envelope) {
    throw new ApiError(PARSE_ERROR_CODE, UNKNOWN_REQUEST_ID, "响应格式异常");
  }
  if (envelope.code !== 0) {
    throw new ApiError(
      envelope.code,
      envelope.request_id || UNKNOWN_REQUEST_ID,
      envelope.message,
    );
  }

  return {
    // 成功信封的 data 由五个公开端点的契约保证存在（parseEnvelope 已挡掉缺失/null）。
    data: envelope.data as T,
    requestId: envelope.request_id || UNKNOWN_REQUEST_ID,
  };
}
