/**
 * API 数据契约（与 api/ TypeScript 实现及 schemas/*.schema.v1.json 对齐）。
 * 说明：manifest.weeks 同时兼容“按周号对象”与“数组”两种编码，由
 * parsePublishedWeeks 统一归一化，避免与采集器/API 侧产生耦合。
 */

// ---------- Manifest ----------

export interface ManifestGroup {
  id: string
  name: string
  /** 可选，manifest 中分组排序值 */
  order?: number
  room_count: number
}

export interface ManifestNode {
  /** 两位节次编码：01..12 */
  code: string
  /** 来源大节编码，例如 0102 / 030405 */
  source_block: string
}

export interface ManifestStatus {
  name: string
  available: boolean
}

/** manifest.weeks 的“按周号对象”编码 */
export interface ManifestWeekRecord {
  week: number
  snapshot_id: string
  sha256: string
  generated_at: string
  last_success_at: string
  last_error_code?: string | null
  last_attempt_at?: string | null
}

/** manifest.weeks 的“数组”编码（宽松兼容） */
export interface ManifestWeekRow {
  week: number
  status?: string
  snapshot_id?: string
  generated_at?: string
  last_success_at?: string
}

export type ManifestWeeks = Record<string, ManifestWeekRecord> | ManifestWeekRow[]

export interface ManifestAnchor {
  date: string
  week: number
  total_weeks: number
  timezone: 'Asia/Shanghai' | string
  in_teaching_calendar: boolean
}

export interface Manifest {
  schema_version: number
  release_id: string
  term: string
  generated_at: string
  anchor: ManifestAnchor
  groups: ManifestGroup[]
  nodes: ManifestNode[]
  statuses: Record<string, ManifestStatus>
  weeks: ManifestWeeks
}

// ---------- Context ----------

export interface ContextInfo {
  /** YYYY-MM-DD（Asia/Shanghai） */
  date?: string
  term?: string
  /** 非教学周为 null */
  week?: number | null
  /** 星期 1..7（周一=1） */
  weekday?: number | null
  /** 兼容旧字段名 */
  day?: number | null
  day_of_week?: number | null
  in_teaching_calendar?: boolean
}

// ---------- 查询响应 ----------

export interface RoomLite {
  id: string
  name: string
}

export interface QueryEcho {
  group_id?: string
  keyword?: string | null
  week?: number | null
  day?: number | null
  start_node?: string
  end_node?: string
}

export interface SnapshotMeta {
  snapshot_id?: string
  generated_at?: string
  stale?: boolean
}

export interface EmptyClassroomsResponse {
  query?: QueryEcho
  rooms?: RoomLite[]
  count?: number
  snapshot?: SnapshotMeta
}

export interface FullDayRoom {
  id: string
  name: string
  /** 键为两位节次（01..12），值为状态 id（整数） */
  statuses?: Record<string, number | string>
}

export interface FullDayStatusResponse {
  query?: QueryEcho
  rooms?: FullDayRoom[]
  snapshot?: SnapshotMeta
}
