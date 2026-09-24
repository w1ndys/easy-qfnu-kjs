import axios, { AxiosError } from 'axios'

/**
 * RFC 9457 application/problem+json 扩展字段。
 * 稳定错误码枚举（与 api/ 对齐）：
 *  invalid_parameter / not_collected / not_in_teaching /
 *  no_snapshot / not_found / method_not_allowed / internal
 */
export interface ProblemDetails {
  type?: string
  title?: string
  status?: number
  detail?: string
  instance?: string
  code?: string
}

export type ApiErrorKind = 'network' | 'timeout' | 'http' | 'cancel' | 'unknown'

export class ApiError extends Error {
  readonly kind: ApiErrorKind
  readonly httpStatus?: number
  readonly code?: string
  readonly problem?: ProblemDetails

  constructor(options: {
    kind: ApiErrorKind
    httpStatus?: number
    code?: string
    problem?: ProblemDetails
    message: string
  }) {
    super(options.message)
    this.name = 'ApiError'
    this.kind = options.kind
    this.httpStatus = options.httpStatus
    this.code = options.code
    this.problem = options.problem
  }
}

function isProblemLike(value: unknown): value is ProblemDetails {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** 统一把 axios 异常规整为 ApiError（HTTP / problem+json / 网络 / 超时）。 */
export function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error

  const axiosError = error as AxiosError<ProblemDetails | string | undefined>

  if (axios.isCancel(error)) {
    return new ApiError({ kind: 'cancel', message: '请求已取消' })
  }

  if (axiosError.response) {
    const { status, data } = axiosError.response
    const problem = isProblemLike(data) ? (data as ProblemDetails) : undefined
    const code = problem?.code || httpStatusToFallbackCode(status)
    const title = problem?.title || fallbackTitle(status)
    const detail = problem?.detail || (typeof data === 'string' ? data : '')
    const suffix = detail ? `：${detail}` : ''
    return new ApiError({
      kind: 'http',
      httpStatus: status,
      code,
      problem,
      message: `${title}${suffix}`,
    })
  }

  const isTimeout =
    axiosError.code === 'ECONNABORTED' ||
    (typeof axiosError.message === 'string' && /timeout/i.test(axiosError.message))

  if (isTimeout) {
    return new ApiError({ kind: 'timeout', message: '请求超时，请检查网络后重试' })
  }

  if (!axiosError.request || axiosError.code === 'ERR_NETWORK') {
    return new ApiError({ kind: 'network', message: '网络连接失败，请检查网络后重试' })
  }

  return new ApiError({ kind: 'unknown', message: axiosError.message || '发生未知错误' })
}

function fallbackTitle(status: number): string {
  if (status === 400) return '请求参数错误'
  if (status === 404) return '资源不存在'
  if (status === 503) return '数据暂不可用'
  if (status >= 500) return '服务暂时不可用'
  return '请求失败'
}

function httpStatusToFallbackCode(status: number): string | undefined {
  if (status === 400) return 'invalid_parameter'
  if (status === 404) return 'not_found'
  if (status === 503) return 'no_snapshot'
  if (status >= 500) return 'internal'
  return undefined
}

export const http = axios.create({
  baseURL: '/api/v1',
  timeout: 30000,
  headers: { Accept: 'application/json' },
})

http.interceptors.response.use(
  (response) => response,
  (error) => Promise.reject(toApiError(error)),
)

/** 服务器端返回的稳定错误码（problem+json code 字段）。 */
export const PROBLEM_CODES = {
  INVALID_PARAMETER: 'invalid_parameter',
  NOT_COLLECTED: 'not_collected',
  NOT_IN_TEACHING: 'not_in_teaching',
  NO_SNAPSHOT: 'no_snapshot',
  NOT_FOUND: 'not_found',
  METHOD_NOT_ALLOWED: 'method_not_allowed',
  INTERNAL: 'internal',
} as const

export type ProblemCode = (typeof PROBLEM_CODES)[keyof typeof PROBLEM_CODES]
