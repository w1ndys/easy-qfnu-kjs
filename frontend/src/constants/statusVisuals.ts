import type { ManifestStatus } from '@/api/types'

/**
 * 状态视觉常量（前端专属，与 legacy 全天状态色表一致）：
 * 1 红、2 橙、3 灰、4 紫、5 绿、6 蓝、7 青、8 深绿、9 粉、10 复合占用（品红）。
 * 数据只提供 id/name/available；颜色/单字图标一律在此维护，不随 manifest 发布。
 */
export interface StatusVisual {
  id: number
  /** 单元格单字图标 */
  glyph: string
  fg: string
  bg: string
  border: string
}

const LEGACY_STATUS_VISUALS: Record<number, Omit<StatusVisual, 'id'>> = {
  1: { glyph: '课', fg: '#DC2626', bg: '#FEE2E2', border: '#FCA5A5' },
  2: { glyph: '借', fg: '#EA580C', bg: '#FFEDD5', border: '#FDBA74' },
  3: { glyph: '锁', fg: '#4B5563', bg: '#F3F4F6', border: '#D1D5DB' },
  4: { glyph: '考', fg: '#7C3AED', bg: '#EDE9FE', border: '#C4B5FD' },
  5: { glyph: '空', fg: '#047857', bg: '#D1FAE5', border: '#6EE7B7' },
  6: { glyph: '固', fg: '#2563EB', bg: '#DBEAFE', border: '#93C5FD' },
  7: { glyph: '临', fg: '#0E7490', bg: '#CFFAFE', border: '#67E8F9' },
  8: { glyph: '全', fg: '#FFFFFF', bg: '#059669', border: '#059669' },
  9: { glyph: '混', fg: '#DB2777', bg: '#FCE7F3', border: '#F9A8D4' },
  10: { glyph: '叠', fg: '#9D174D', bg: '#FDF2F8', border: '#FBCFE8' },
}

const UNKNOWN_VISUAL: StatusVisual = {
  id: 0,
  glyph: '?',
  fg: '#8A7C70',
  bg: '#FFFFFF',
  border: '#E5DED7',
}

/** 状态 id（1..10）→ 前端视觉；未知 id 回退中性灰。 */
export function statusVisual(statusId: number | string | null | undefined): StatusVisual {
  const numeric = Number(statusId)
  const known = LEGACY_STATUS_VISUALS[numeric]
  if (Number.isInteger(numeric) && known) {
    return { id: numeric, ...known }
  }
  return { ...UNKNOWN_VISUAL, id: Number.isInteger(numeric) ? numeric : 0 }
}

export interface StatusMetaItem {
  /** manifest 中的键（字符串状态 id） */
  key: string
  id: number
  name: string
  available: boolean
  visual: StatusVisual
}

/**
 * 汇总 manifest.statuses：键升序、附前端视觉与可用性，供表格/图例共用。
 */
export function buildStatusMeta(statuses: Record<string, ManifestStatus> | undefined): StatusMetaItem[] {
  if (!statuses) return []
  const entries = Object.entries(statuses)
    .filter(([key]) => Number.isInteger(Number(key)))
    .sort(([a], [b]) => Number(a) - Number(b))
  return entries.map(([key, def]) => ({
    key,
    id: Number(key),
    name: def.name,
    available: def.available,
    visual: statusVisual(key),
  }))
}

export function statusName(meta: StatusMetaItem[], statusId: number | string): string {
  const found = meta.find((item) => item.id === Number(statusId))
  return found?.name ?? '未知状态'
}
