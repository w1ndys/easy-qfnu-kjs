// 与仓库根 schemas/*.schema.v1.json 对应的运行时只读类型。
// 采集器（Go）与构建脚本产出这些 JSON，运行时不做完整 Schema 校验（构建期已校验）。

/** manifest.anchor：教学日历锚点（采集器每日刷新）。 */
export interface ManifestAnchor {
  /** Asia/Shanghai 采集日，YYYY-MM-DD。 */
  date: string;
  /** 锚点日期所属教学周；非教学周为 0。 */
  week: number;
  /** 本学期总教学周数。 */
  total_weeks: number;
  timezone: 'Asia/Shanghai';
  in_teaching_calendar: boolean;
}

export interface ManifestGroup {
  id: string;
  name: string;
  /** 未配置时省略；公开视图原样透传。 */
  order?: number;
  room_count: number;
}

/** manifest.weeks 中的单个周记录（含校验与最近尝试信息）。 */
export interface ManifestWeekEntry {
  week: number;
  snapshot_id: string;
  sha256: string;
  generated_at: string;
  last_success_at: string;
  last_error_code?: string | null;
  last_attempt_at?: string | null;
}

/** data/manifest.json。 */
export interface Manifest {
  schema_version: 1;
  release_id: string;
  term: string;
  generated_at: string;
  anchor: ManifestAnchor;
  groups: ManifestGroup[];
  nodes: Array<{ code: string; source_block: string }>;
  statuses: Record<string, { name: string; available: boolean }>;
  weeks: Record<string, ManifestWeekEntry>;
}

/** 周快照 room 对象（snapshot.schema.v1.json）。 */
export interface SnapshotRoom {
  jsbh: string;
  group_id: string;
  name: string;
  /** 01–12 小节 → 状态 ID。 */
  statuses: Record<string, number>;
}

/** data/terms/<term>/weeks/week-NN.json。 */
export interface SnapshotWeekFile {
  schema_version: 1;
  term: string;
  week: number;
  snapshot_id: string;
  generated_at: string;
  last_success_at: string;
  days: Record<string, { rooms: SnapshotRoom[] }>;
}

/** api-lib/generated-index.ts（或测试覆盖 JSON）导出的构建期数据索引。 */
export interface DataIndex {
  term: string;
  releaseId: string;
  weeks: Record<string, IndexWeekEntry>;
}

export interface IndexWeekEntry {
  week: number;
  snapshotId: string;
  sha256: string;
  /** 相对数据根的周快照文件路径，例如 data/terms/2026-2027-1/weeks/week-02.json。 */
  path: string;
}
