// 节次区间与节次选项（design「lib/nodes.ts」）。
// 三档预填只由「打开页面时的本地时间」决定（PRD 6.2）：<12:00 → 01–05，
// 12:00–18:00 → 06–09，≥18:00 → 10–12。它是纯交互默认值，不改服务端对区间的判定；
// 选非当天日期时不重算区间（沿用当前时段）。

import type { MetaAxis } from "../api/types";

/** 请求与 URL 里的节次区间，值即接口契约的小节编号 "01".."12"。 */
export interface NodeRange {
  start_node: string;
  end_node: string;
}

/** 接口契约里的小节编号全集（backend/internal/api/params.go），轴缺失时按它回退。 */
export const NODE_IDS = [
  "01",
  "02",
  "03",
  "04",
  "05",
  "06",
  "07",
  "08",
  "09",
  "10",
  "11",
  "12",
] as const;

/** 一节下拉选项；label 只由轴上的小节编号派生（design D11：不写死中文节次名）。 */
export interface NodeOption {
  value: string;
  label: string;
}

/** 上午时段的预填区间（本地时间 12:00 前打开）。 */
const MORNING_RANGE: NodeRange = { start_node: "01", end_node: "05" };
/** 下午时段的预填区间（12:00–18:00 打开，含 12:00、不含 18:00）。 */
const AFTERNOON_RANGE: NodeRange = { start_node: "06", end_node: "09" };
/** 晚间时段的预填区间（18:00 及以后打开）。 */
const EVENING_RANGE: NodeRange = { start_node: "10", end_node: "12" };

/**
 * 按本地时间给出节次区间默认值。
 * 边界口径：11:59 → 01–05；12:00 → 06–09；17:59 → 06–09；18:00 → 10–12。
 * 只读小时，因此分钟与秒不影响档位；返回新对象，调用方改它不会污染模块常量。
 */
export function defaultNodeRange(now: Date): NodeRange {
  const hour = now.getHours();
  if (hour < 12) {
    return { ...MORNING_RANGE };
  }
  if (hour < 18) {
    return { ...AFTERNOON_RANGE };
  }
  return { ...EVENING_RANGE };
}

/**
 * 由元数据轴派生节次选项。
 * 顺序取 `axis.nodes[].ordinal`（不信任数组原顺序）；轴缺失（无当前版本）时回退
 * 契约里的 01..12，保证「暂无数据版本」时仍能调整区间，不白屏也不写死节次名。
 */
export function buildNodeOptions(axis: MetaAxis | null): NodeOption[] {
  const nodes = axis?.nodes ?? [];
  if (nodes.length === 0) {
    return NODE_IDS.map((node) => ({ value: node, label: `第 ${node} 节` }));
  }

  return [...nodes]
    .sort((left, right) => left.ordinal - right.ordinal)
    .map((node) => ({ value: node.node, label: `第 ${node.node} 节` }));
}
