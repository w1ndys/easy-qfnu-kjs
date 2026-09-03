// 响应视图组装（纯逻辑，供 handler 与单元测试复用）。
import { naturalCompare } from './sort';
import { weekStatus } from './staleness';
import type { Manifest, ManifestWeekEntry, SnapshotRoom, SnapshotWeekFile } from './types';

export interface ManifestWeekView {
  week: number;
  generated_at: string | null;
  last_success_at: string | null;
  status: 'fresh' | 'stale' | 'missing';
}

/** GET /api/v1/manifest 公开视图：不暴露 sha256/snapshot_id 等内部字段。 */
export function buildManifestView(
  manifest: Manifest,
  currentWeek: number | null,
  now: number,
): {
  schema_version: 1;
  term: string;
  release_id: string;
  generated_at: string;
  anchor: Manifest['anchor'];
  groups: Manifest['groups'];
  nodes: Manifest['nodes'];
  statuses: Manifest['statuses'];
  weeks: ManifestWeekView[];
} {
  const weeks: ManifestWeekView[] = [];
  for (let w = 1; w <= manifest.anchor.total_weeks; w++) {
    const entry = manifest.weeks[String(w)];
    weeks.push({
      week: w,
      generated_at: entry?.generated_at ?? null,
      last_success_at: entry?.last_success_at ?? null,
      status: weekStatus(entry, { currentWeek, now }),
    });
  }
  return {
    schema_version: manifest.schema_version,
    term: manifest.term,
    release_id: manifest.release_id,
    generated_at: manifest.generated_at,
    anchor: manifest.anchor,
    groups: manifest.groups,
    nodes: manifest.nodes,
    statuses: manifest.statuses,
    weeks,
  };
}

/** 查询响应中的 snapshot 段：{ snapshot_id, generated_at, stale }。 */
export function buildSnapshotView(
  entry: ManifestWeekEntry,
  weekFile: SnapshotWeekFile,
  currentWeek: number | null,
  now: number,
): { snapshot_id: string; generated_at: string; stale: boolean } {
  const status = weekStatus(entry, { currentWeek, now });
  return {
    snapshot_id: weekFile.snapshot_id,
    generated_at: weekFile.generated_at,
    stale: status === 'stale',
  };
}

export interface RoomRef {
  id: string;
  name: string;
}

export interface FullDayRoom extends RoomRef {
  statuses: Record<string, number>;
}

function byNameThenId(a: RoomRef, b: RoomRef): number {
  const byName = naturalCompare(a.name, b.name);
  if (byName !== 0) return byName;
  return naturalCompare(a.id, b.id);
}

/** 空教室列表：名称自然排序，同名按 jsbh 排序。 */
export function buildRoomList(rooms: SnapshotRoom[]): RoomRef[] {
  return rooms
    .map((r) => ({ id: r.jsbh, name: r.name }))
    .sort(byNameThenId);
}

/** 全天状态列表：保留房间状态表，同样排序。 */
export function buildFullDayList(rooms: SnapshotRoom[]): FullDayRoom[] {
  return rooms
    .map((r) => ({ id: r.jsbh, name: r.name, statuses: { ...r.statuses } }))
    .sort(byNameThenId);
}
