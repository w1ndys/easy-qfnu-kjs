// 公开接口的线上形状（wire types）：字段名与后端 JSON 的 snake_case 一一对应，
// 前端不做自动 camelCase 转换（design D10；键名由调用点显式给出）。
// 形状来源：backend/internal/query 的类型定义与 specs/2026-10-27-classroom-availability-rewrite/design.md。

/** 公开接口统一信封：code=0 成功，非 0 为业务失败；失败时 data 为 null。 */
export interface Envelope<T> {
  code: number;
  message: string;
  data: T | null;
  request_id: string;
}

/** GET /api/context 的 data：业务日期、学期、周次、星期与是否在教学周。 */
export interface ContextInfo {
  date: string; // YYYY-MM-DD，按 Asia/Shanghai 解释
  term: string; // 学期标识；不在教学周时为空
  week: number; // 教学周序号；不在教学周时为 0
  weekday: number; // 星期 1=周一 … 7=周日；不在教学周时为 0
  in_calendar: boolean;
}

/** 元数据里的一条状态：展示名与是否可用。 */
export interface MetaState {
  label: string;
  available: boolean;
}

/** 版本化字典：states 以状态键为键（design D11：名称只来自这里）。 */
export interface MetaDict {
  version: number;
  states: Record<string, MetaState>;
}

/** 节次轴上的一个小节节点；ordinal 是展示顺序，block 是所属大节。 */
export interface MetaAxisNode {
  node: string; // 小节编号 "01".."12"
  ordinal: number;
  block: string; // 大节编码，如 "030405"
  block_ordinal: number;
}

/** 节次轴：大节顺序与逐个小节节点（D11：节次名只来自这里）。 */
export interface MetaAxis {
  version: number;
  blocks: string[];
  nodes: MetaAxisNode[];
}

/** 白名单教学楼；building_id 就是查询参数 building 的取值。 */
export interface MetaBuilding {
  building_id: string;
  name: string;
}

/** 当前发布版本：标识与生成时间（RFC3339 UTC）。 */
export interface MetaRelease {
  release_id: string;
  generated_at: string;
}

/** 某一周的发布与新鲜度，/api/meta 与 /api/screen 共用同一形状。 */
export interface WeekFreshness {
  week: number;
  published: boolean;
  last_success_at: string | null; // 从未成功时为 null
  stale: boolean;
}

/** GET /api/meta 的 data；无当前版本时 release/dict/axis 为 null、empty 为 true。 */
export interface MetaResult {
  release: MetaRelease | null;
  dict: MetaDict | null;
  axis: MetaAxis | null;
  buildings: MetaBuilding[];
  weeks: WeekFreshness[];
  empty: boolean;
}

/**
 * 页顶提示标记：前端按 code 选文案，message 只作兜底。
 * day 视图复用同一形状（多一档 day_no_data）；code 保持 string 以容纳后端新增档位。
 */
export interface AvailabilityHint {
  code: string; // no_match / no_data / week_no_schedule / day_no_data
  message: string;
}

/** 空教室结果里的一间房。 */
export interface AvailabilityRoom {
  room_id: string;
  name: string;
  building_id: string;
  building_name: string;
}

/** GET /api/query?view=availability 的 data。 */
export interface AvailabilityResult {
  total: number; // 分页前的命中总数
  items: AvailabilityRoom[]; // 当前页房间，顺序由服务端决定
  date: string;
  term: string;
  week: number;
  weekday: number;
  hint?: AvailabilityHint; // 仅空态出现
}

/** 全天状态视图里的一小节。无观测时 state_key 为空、has_data 为 false。 */
export interface DayNode {
  node: string; // 小节编号 "01".."12"
  block: string; // 所属大节
  state_key: string;
  label: string; // 字典展示名；无观测时为「无数据」
  available: boolean;
  has_data: boolean;
}

/** GET /api/query?view=day 的 data。 */
export interface DayResult {
  room_id: string;
  name: string;
  building_id: string;
  building_name: string;
  date: string;
  term: string;
  week: number;
  weekday: number;
  nodes: DayNode[]; // 按轴顺序的逐节状态；无版本时为空数组
  hint?: AvailabilityHint; // 仅空态出现
}

/** 大屏站点规模：房间数、教学楼数、学期、当前周与采集窗口 [起始周, 结束周]。 */
export interface ScreenScale {
  rooms: number;
  buildings: number;
  term: string;
  week: number;
  window: number[]; // 没有可用窗口时为空数组
}

/** 大屏上的当前发布版本。 */
export interface ScreenRelease {
  release_id: string;
  generated_at: string;
}

/** 最近一轮同步：只含状态、时间与归属冲突标记，不含错误码与原文。 */
export interface ScreenLastRun {
  status: string;
  started_at: string;
  duration_ms: number | null; // 没有结束时刻时为 null
  attr_conflict: boolean;
}

/** 未知、复合与归属冲突的计数。 */
export interface ScreenAnomalies {
  unknown_cells: number;
  composite_cells: number;
  attr_conflicts: number;
}

/** 一栋教学楼的占用情况；rate = occupied / cells。 */
export interface ScreenBuildingOccupancy {
  building_id: string;
  name: string;
  cells: number;
  occupied: number;
  rate: number;
}

/** 占用分布两条口径：按楼列表与 7 天 × 5 大节矩阵。 */
export interface ScreenOccupancy {
  by_building: ScreenBuildingOccupancy[];
  by_weekday_block: number[][]; // 7 行 × 5 列
}

/** PV/UV 的今日、近 7 日、近 30 日计数。 */
export interface ScreenTrafficCounts {
  today: number;
  d7: number;
  d30: number;
}

/** 访问统计（本 spec 不渲染，但响应里存在，夹具需要与契约对齐）。 */
export interface ScreenTraffic {
  pv: ScreenTrafficCounts;
  uv: ScreenTrafficCounts;
}

export interface ScreenLeaderboardBuilding {
  building_id: string;
  name: string;
  hits: number;
}

export interface ScreenLeaderboardRoom {
  room_id: string;
  name: string;
  hits: number;
}

export interface ScreenLeaderboardKeyword {
  text: string;
  hits: number;
}

/** 三张 Top 10 榜单。 */
export interface ScreenLeaderboard {
  buildings: ScreenLeaderboardBuilding[];
  rooms: ScreenLeaderboardRoom[];
  keywords: ScreenLeaderboardKeyword[];
}

/** 访问峰值时段。 */
export interface ScreenPeak {
  weekday: number;
  hour: number;
  share: number;
}

/** 星期 × 小时热力与峰值时段。 */
export interface ScreenPeaks {
  heatmap: number[][];
  top: ScreenPeak[];
}

/** 近 30 日逐日查询次数。 */
export interface ScreenTrendPoint {
  day: string; // YYYY-MM-DD，按 Asia/Shanghai 解释
  pv: number;
}

/** 节次命中分布。 */
export interface ScreenNodeHit {
  node: string;
  hits: number;
}

/**
 * GET /api/screen 的 data。
 * 前六块（scale/release/weeks/last_run/anomalies/occupancy）是本 spec 的渲染范围；
 * traffic 之后的访问统计块只保证类型与后端一致，不在本次页面里出现。
 */
export interface ScreenResult {
  scale: ScreenScale;
  release: ScreenRelease | null;
  weeks: WeekFreshness[];
  last_run: ScreenLastRun | null;
  anomalies: ScreenAnomalies;
  occupancy: ScreenOccupancy;
  traffic: ScreenTraffic;
  leaderboard: ScreenLeaderboard;
  peaks: ScreenPeaks;
  trend: ScreenTrendPoint[];
  node_distribution: ScreenNodeHit[];
  empty: boolean;
}
