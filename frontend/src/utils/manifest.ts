import type { Manifest, ManifestWeekRow, ManifestWeeks } from '@/api/types'

/**
 * 归一化 manifest.weeks：兼容“按周号对象 { "2": {...} }”与“数组 [{ week, ... }]”两种编码。
 * 返回按周号升序的周行；无法解析时返回空数组。
 */
export function parsePublishedWeeks(weeks: ManifestWeeks | undefined | null): ManifestWeekRow[] {
  if (!weeks) return []

  if (Array.isArray(weeks)) {
    return weeks
      .filter((row) => typeof row?.week === 'number' && Number.isInteger(row.week) && row.week > 0)
      .sort((a, b) => a.week - b.week)
  }

  const rows: ManifestWeekRow[] = []
  for (const value of Object.values(weeks)) {
    const week = Number(value?.week)
    if (Number.isInteger(week) && week > 0) {
      rows.push({
        week,
        status: value.last_error_code != null ? 'stale' : undefined,
        snapshot_id: value.snapshot_id,
        generated_at: value.generated_at,
        last_success_at: value.last_success_at,
      })
    }
  }
  rows.sort((a, b) => a.week - b.week)
  return rows
}

/** manifest 发布周是否包含目标周 */
export function hasPublishedWeek(manifest: Manifest, week: number | null | undefined): boolean {
  if (week == null) return false
  return parsePublishedWeeks(manifest.weeks).some((row) => row.week === week)
}

/** manifest 的 nodes 按节次码升序排列（复制后排序，避免改动响应对象）。 */
export function sortedNodes(manifest: Manifest) {
  return [...manifest.nodes].sort((a, b) => Number(a.code) - Number(b.code))
}

/** 依 manifest.nodes 的 source_block 分组，保持块首次出现顺序（返回的块内节点按节次升序）。 */
export function groupNodesBySourceBlock(
  manifest: Manifest,
): Array<{ source_block: string; nodes: Array<{ code: string; source_block: string }> }> {
  const order: string[] = []
  const groups = new Map<string, Array<{ code: string; source_block: string }>>()
  for (const node of sortedNodes(manifest)) {
    if (!groups.has(node.source_block)) {
      groups.set(node.source_block, [])
      order.push(node.source_block)
    }
    groups.get(node.source_block)!.push(node)
  }
  return order.map((block) => ({ source_block: block, nodes: groups.get(block)! }))
}
