// 业务码 → 文案（design D10）：只映射后端公开 API 已定义的四个码，
// 其余码一律兜底「服务异常」，不猜含义；信封里的 message 仅作副标题，不当作可依赖文案。
// 这里不 import client.ts，避免 ApiError 与分类函数互相依赖。

/**
 * 限流码：页面在收到它时让查询按钮进入 3 秒冷却（design「Error Handling」）。
 * 单独导出是为了让冷却逻辑与文案映射共用同一个常量，不在别处散落魔法数字。
 */
export const RATE_LIMIT_CODE = 42901;

/** 后端公开 API 已定义的失败码及其文案，键与 backend/internal/api 的口径一致。 */
const CODE_TITLES: Readonly<Record<number, string>> = {
  40001: "参数有误",
  40401: "数据不存在",
  [RATE_LIMIT_CODE]: "请求过于频繁，请稍后重试",
  50000: "服务异常",
};

/** 未知业务码（含管理端才有的码）与信封解析失败的统一兜底文案。 */
export const FALLBACK_TITLE = "服务异常";

/**
 * 网络不可达 / 请求超时的前端自有失败码。
 * 业务码都是正整数，用 -1 不会与任何后端码冲突；它只用于把「请求没拿到信封」
 * 也收敛成 ApiError，不参与 errors 的业务码映射。
 */
export const NETWORK_ERROR_CODE = -1;

/** 网络失败的主文案（design「Error Handling」：网络不可达 / 请求超时）。 */
export const NETWORK_TITLE = "网络异常，请检查连接";

/** 信封里拿不到 request_id 时的占位（design：解析失败时记「未知」）。 */
export const UNKNOWN_REQUEST_ID = "未知";

/**
 * 按业务码给出错误主文案。
 * 未知码不猜含义，只保证有兜底；message 由调用方作为副标题展示。
 */
export function getErrorTitle(code: number): string {
  if (code === NETWORK_ERROR_CODE) {
    return NETWORK_TITLE;
  }
  return CODE_TITLES[code] ?? FALLBACK_TITLE;
}

/**
 * 该错误是否值得让用户点「重试」。
 * 40001 要改参数、40401 说明数据不存在，重试同一请求不会变；其余（含未知码与网络失败）都可能恢复。
 */
export function isRetriableCode(code: number): boolean {
  return code !== 40001 && code !== 40401;
}
