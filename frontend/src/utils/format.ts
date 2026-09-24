const WEEKDAY_CN = ['一', '二', '三', '四', '五', '六', '日'] as const

/** day: 1..7（周一=1）→ “星期一” */
export function weekdayLabel(day: number | null | undefined): string {
  if (day == null || !Number.isInteger(day) || day < 1 || day > 7) return ''
  return `星期${WEEKDAY_CN[day - 1]}`
}

/** day: 1..7（周一=1）→ “周一” */
export function weekdayShortLabel(day: number | null | undefined): string {
  if (day == null || !Number.isInteger(day) || day < 1 || day > 7) return ''
  return `周${WEEKDAY_CN[day - 1]}`
}

/** 固定以 Asia/Shanghai 渲染 ISO 时间 → “YYYY-MM-DD HH:mm”。 */
export function formatBeijing(iso: string | null | undefined): string {
  if (!iso) return ''
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return iso
  const parts = new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).formatToParts(date)
  const get = (type: string) => parts.find((p) => p.type === type)?.value ?? ''
  return `${get('year')}-${get('month')}-${get('day')} ${get('hour')}:${get('minute')}`
}

/** 日期字符串（YYYY-MM-DD）→ “YYYY年M月D日” */
export function formatDateZh(date: string | null | undefined): string {
  if (!date) return ''
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date)
  if (!match) return date
  return `${Number(match[1])}年${Number(match[2])}月${Number(match[3])}日`
}

/**
 * 同一结果中同名房间的展示标签：只有重名项才追加“缩短后的 id”做区分（Q101）。
 * 缩短策略：取 id 尾部，长度从 4 递增，直到在同名集合内唯一；id 过短则保留全长。
 */
export interface RoomLabel {
  name: string
  /** 存在时为需要追加的短 id（不含括号） */
  shortId?: string
}

export function buildRoomLabels(rooms: Array<{ id: string; name: string }>): Map<string, RoomLabel> {
  const labels = new Map<string, RoomLabel>()
  const countByName = new Map<string, number>()
  for (const room of rooms) {
    countByName.set(room.name, (countByName.get(room.name) ?? 0) + 1)
  }
  const byName = new Map<string, Array<{ id: string; name: string }>>()
  for (const room of rooms) {
    if ((countByName.get(room.name) ?? 0) > 1) {
      const list = byName.get(room.name) ?? []
      list.push(room)
      byName.set(room.name, list)
    }
  }
  for (const room of rooms) {
    const label: RoomLabel = { name: room.name }
    const duplicates = byName.get(room.name)
    if (duplicates && duplicates.length > 1) {
      label.shortId = shortestUniqueTail(room.id, duplicates.map((d) => d.id))
    }
    labels.set(room.id, label)
  }
  return labels
}

function shortestUniqueTail(id: string, ids: string[]): string {
  const maxLen = Math.min(id.length, 12)
  for (let len = 4; len <= maxLen; len += 1) {
    const tail = id.slice(-len)
    if (ids.filter((other) => other.slice(-len) === tail).length === 1) return tail
  }
  return id
}
