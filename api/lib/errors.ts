// RFC 9457 problem+json 错误构造与稳定扩展 code（见决策文件 §11.3 / Q104）。

export const CODES = {
  invalidParameter: 'invalid_parameter',
  notCollected: 'not_collected',
  notInTeaching: 'not_in_teaching',
  noSnapshot: 'no_snapshot',
  notFound: 'not_found',
  methodNotAllowed: 'method_not_allowed',
  internal: 'internal',
} as const;

export type Code = (typeof CODES)[keyof typeof CODES];

const TITLES: Record<Code, string> = {
  invalid_parameter: 'Invalid parameter',
  not_collected: 'Not collected',
  not_in_teaching: 'Not in teaching period',
  no_snapshot: 'No snapshot available',
  not_found: 'Not Found',
  method_not_allowed: 'Method Not Allowed',
  internal: 'Internal Server Error',
};

/** 业务/数据层可抛出的 HTTP 错误；由 handler 统一转为 problem+json。 */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: Code,
    detail: string,
    readonly extraHeaders?: Record<string, string>,
  ) {
    super(detail);
    this.name = 'ApiError';
  }
}

export function problemResponse(
  status: number,
  code: Code,
  detail: string,
  instance: string,
  extraHeaders?: Record<string, string>,
): Response {
  const body = {
    type: 'about:blank',
    title: TITLES[code],
    status,
    detail,
    instance,
    code,
  };
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type': 'application/problem+json; charset=utf-8',
      ...(extraHeaders ?? {}),
    },
  });
}

export function errorToResponse(err: unknown, instance: string): Response {
  if (err instanceof ApiError) {
    return problemResponse(err.status, err.code, err.message, instance, err.extraHeaders);
  }
  // 未知异常属于实现缺陷，按 internal 处理且不泄露内部细节。
  return problemResponse(500, CODES.internal, '服务内部错误', instance);
}
