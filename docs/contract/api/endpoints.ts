import { http } from './client'
import type {
  ContextInfo,
  EmptyClassroomsResponse,
  FullDayStatusResponse,
  Manifest,
} from './types'

export interface ClassroomQueryParams {
  group_id: string
  /** 可选：房间名子串过滤，<=32 字符 */
  keyword?: string
  week?: number | null
  day?: number | null
}

export interface EmptyClassroomsParams extends ClassroomQueryParams {
  /** 两位字符串 01..12，start <= end */
  start_node?: string
  end_node?: string
}

/** GET /api/v1/manifest */
export async function fetchManifest(): Promise<Manifest> {
  const { data } = await http.get<Manifest>('/manifest')
  return data
}

/** GET /api/v1/context */
export async function fetchContext(): Promise<ContextInfo> {
  const { data } = await http.get<ContextInfo>('/context')
  return data
}

/** GET /api/v1/empty-classrooms */
export async function fetchEmptyClassrooms(
  params: EmptyClassroomsParams,
): Promise<EmptyClassroomsResponse> {
  const query: Record<string, string | number> = { group_id: params.group_id }
  const keyword = params.keyword?.trim()
  if (keyword) query.keyword = keyword
  if (params.week != null) query.week = params.week
  if (params.day != null) query.day = params.day
  if (params.start_node) query.start_node = params.start_node
  if (params.end_node) query.end_node = params.end_node
  const { data } = await http.get<EmptyClassroomsResponse>('/empty-classrooms', { params: query })
  return data
}

/** GET /api/v1/full-day-status */
export async function fetchFullDayStatus(
  params: ClassroomQueryParams,
): Promise<FullDayStatusResponse> {
  const query: Record<string, string | number> = { group_id: params.group_id }
  const keyword = params.keyword?.trim()
  if (keyword) query.keyword = keyword
  if (params.week != null) query.week = params.week
  if (params.day != null) query.day = params.day
  const { data } = await http.get<FullDayStatusResponse>('/full-day-status', { params: query })
  return data
}
