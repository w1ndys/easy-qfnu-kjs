// 节次区间与节次选项（design「lib/nodes.ts」）。
// 三档预填只由「打开页面时的本地时间」决定（PRD 6.2）：<12:00 → 01–05，
// 12:00–18:00 → 06–09，≥18:00 → 10–12。它是纯交互默认值，不改服务端对区间的判定；
// 选非当天日期时不重算区间（沿用当前时段）。

import type { MetaAxis, MetaAxisNode } from "../api/types";

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

/**
 * 大节列数：占用热力图固定 7 天 × 5 大节
 * （backend/internal/query/screen.go 的 by_weekday_block 契约）。
 */
export const BLOCK_COUNT = 5;

/** 没有轴可用时的大节列头：按 1..5 的序号回退，不编造小节区间。 */
function fallbackBlockLabel(index: number): string {
  return `大节 ${index + 1}`;
}

/**
 * 一组小节编号（已按 ordinal 从小到大）→ 大节区间文案。
 * 两个及以上小节写成「第 01–02 节」，单节点大节退回「第 01 节」；
 * 空组返回 null，由调用方按列号回退。
 */
function blockLabel(nodeIds: readonly string[]): string | null {
  if (nodeIds.length === 0) {
    return null;
  }
  const first = nodeIds[0];
  const last = nodeIds[nodeIds.length - 1];
  return first === last ? `第 ${first} 节` : `第 ${first}–${last} 节`;
}

/**
 * 由元数据轴派生大节列头，如「第 01–02 节」。
 *
 * 大节顺序优先取 `axis.blocks`（轴自己声明的大节顺序）；`blocks` 为空时按
 * 每个大节内节点的 `block_ordinal` 排序。每个大节的首尾小节取该大节内
 * ordinal 最小/最大的节点，所以 `axis.nodes` 的数组顺序不影响列头。
 *
 * 返回值恒为 `BLOCK_COUNT` 项：无当前版本（`/api/meta` 失败或 `axis` 为 null）、
 * 轴只覆盖部分大节、或某个大节没有节点时，逐列回退「大节 1」..「大节 5」，
 * 热力图始终是 7×5，少一列就与矩阵对不上（Requirement 6.5）。
 */
export function buildBlockLabels(axis: MetaAxis | null): string[] {
  const derived = deriveBlockLabels(axis);
  return Array.from(
    { length: BLOCK_COUNT },
    (_, index) => derived[index] ?? fallbackBlockLabel(index),
  );
}

/** 派生列头；返回数组长度可能与 BLOCK_COUNT 不一致，缺失位交给 buildBlockLabels 回退。 */
function deriveBlockLabels(axis: MetaAxis | null): Array<string | null> {
  const blocks = axis?.blocks ?? [];
  const nodes = axis?.nodes ?? [];
  if (blocks.length === 0 && nodes.length === 0) {
    return [];
  }

  const groups = new Map<string, MetaAxisNode[]>();
  for (const node of nodes) {
    const group = groups.get(node.block);
    if (group === undefined) {
      groups.set(node.block, [node]);
    } else {
      group.push(node);
    }
  }

  const blockCodes =
    blocks.length > 0
      ? blocks
      : [...groups.entries()]
          .sort(
            ([, left], [, right]) => blockOrdinal(left) - blockOrdinal(right),
          )
          .map(([code]) => code);

  return blockCodes.map((code) => {
    const group = groups.get(code) ?? [];
    const ordered = [...group]
      .sort((left, right) => left.ordinal - right.ordinal)
      .map((node) => node.node);
    return blockLabel(ordered);
  });
}

/** 一组节点里最靠前的大节序号；只在 blocks 缺失、需要按 nodes 排序时使用。 */
function blockOrdinal(nodes: readonly MetaAxisNode[]): number {
  return nodes.reduce(
    (min, node) => Math.min(min, node.block_ordinal),
    Number.POSITIVE_INFINITY,
  );
}
