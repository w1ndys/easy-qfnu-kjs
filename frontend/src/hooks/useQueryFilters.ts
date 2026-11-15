// URL ↔ 筛选参数的唯一读写点（design D1：URL 是筛选条件的单一事实来源）。
// 组件只读这里解析出的 filters、只通过 setFilters 写 URL；不在别处保存筛选状态，
// 刷新、后退、分享链接、从全天状态返回才会自然成立。

import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router";
import { NODE_IDS, type NodeRange } from "../lib/nodes";

/** URL 上的查询条件；键名与接口参数对齐（page 是前端分页参数，服务端用 offset）。 */
export interface QueryFilters {
  date_offset: number;
  /** 空串表示「全部」教学楼，序列化时不出现在 URL 上。 */
  building: string;
  start_node: string;
  end_node: string;
  /** 空串表示不限关键词；只在回车/点查询时提交。 */
  keyword: string;
  /** 1 起的页码；分页 limit/offset 由结果区（第 4 项）按它换算。 */
  page: number;
}

export interface UseQueryFiltersResult {
  filters: QueryFilters;
  /** 规范化后的查询串（不含 `?`）：URL 应呈现的形态，也是查询请求键。 */
  canonicalSearch: string;
  /** 当前 URL 是否已经是规范化形态；否者页面应先用 replace 回写一次。 */
  isCanonical: boolean;
  /**
   * 改若干筛选（保留其它字段）并写入 URL；patch 里没有 `page` 时把页码重置为 1。
   * 默认新增一条历史记录以支持后退。
   */
  setFilters: (patch: Partial<QueryFilters>) => void;
  /** 用 replace 写入完整筛选：只用于把预填默认值规范化进 URL，不产生历史记录。 */
  replaceFilters: (next: QueryFilters) => void;
}

/** `date_offset` 的契约上限（backend/internal/api/params.go：0..10）。 */
export const MAX_DATE_OFFSET = 10;

/** 关键词的长度上限（backend/internal/api/params.go：≤32 字，按 rune 计）。 */
export const MAX_KEYWORD_LENGTH = 32;

const INTEGER_PATTERN = /^-?\d+$/;
const NODE_PATTERN = /^(0[1-9]|1[0-2])$/;
const NODE_ID_SET: ReadonlySet<string> = new Set(NODE_IDS);

/** 读一个整数参数；缺失或不是整数返回 null，交给调用方给默认值。 */
function readInteger(raw: string | null): number | null {
  if (raw === null || !INTEGER_PATTERN.test(raw)) {
    return null;
  }
  const value = Number(raw);
  return Number.isSafeInteger(value) ? value : null;
}

/**
 * 读关键词：先去掉首尾空白（服务端也会归一），再按契约上限 32 个字符校验。
 * 长度按 rune（`Array.from` 拆码点）数，避免把 emoji 之类的代理对算成两个字符；
 * 超限就不猜用户想截到哪，直接回退「不限」——与其它非法参数的回退口径一致。
 */
function readKeyword(raw: string | null): string {
  const keyword = raw?.trim() ?? "";
  return Array.from(keyword).length > MAX_KEYWORD_LENGTH ? "" : keyword;
}

/** 读一个小节编号；只接受契约里的 01..12。 */
function readNode(raw: string | null): string | null {
  return raw !== null && NODE_PATTERN.test(raw) && NODE_ID_SET.has(raw)
    ? raw
    : null;
}

/**
 * 解析 URL 为筛选条件，非法值一律回退默认值（design「Error Handling」的 40001 口径：
 * URL 里的非法参数回退到默认值，而不是把非法组合发给后端）。
 * 默认区间来自页面打开时的本地时间预填（fallback），它是唯一依赖时钟的地方。
 */
function parseFilters(
  searchParams: URLSearchParams,
  fallback: NodeRange,
): QueryFilters {
  const dateOffset = readInteger(searchParams.get("date_offset"));
  const page = readInteger(searchParams.get("page"));

  let startNode =
    readNode(searchParams.get("start_node")) ?? fallback.start_node;
  let endNode = readNode(searchParams.get("end_node")) ?? fallback.end_node;
  // 起点晚于终点是非法组合：整体回退到预填区间，而不是只修一半。
  if (startNode > endNode) {
    startNode = fallback.start_node;
    endNode = fallback.end_node;
  }

  return {
    date_offset:
      dateOffset !== null && dateOffset >= 0 && dateOffset <= MAX_DATE_OFFSET
        ? dateOffset
        : 0,
    building: searchParams.get("building")?.trim() ?? "",
    start_node: startNode,
    end_node: endNode,
    keyword: readKeyword(searchParams.get("keyword")),
    page: page !== null && page >= 1 ? page : 1,
  };
}

/**
 * 序列化为规范查询串：固定字段顺序、默认值也写出来。
 * 把预填区间显式写进 URL 是关键——刷新页面不会因为时钟跨过 12:00/18:00 而换一套
 * 请求参数（Correctness Properties 1）。
 */
export function serializeFilters(filters: QueryFilters): string {
  const search = new URLSearchParams();
  search.set("date_offset", String(filters.date_offset));
  if (filters.building !== "") {
    search.set("building", filters.building);
  }
  search.set("start_node", filters.start_node);
  search.set("end_node", filters.end_node);
  if (filters.keyword !== "") {
    search.set("keyword", filters.keyword);
  }
  search.set("page", String(filters.page));
  return search.toString();
}

/**
 * 读/写 URL 上的筛选参数。
 * `fallback` 是打开页面时按本地时间算出的预填区间，只在 URL 缺字段或给出非法组合时使用。
 */
export function useQueryFilters(fallback: NodeRange): UseQueryFiltersResult {
  const [searchParams, setSearchParams] = useSearchParams();
  const { start_node, end_node } = fallback;

  const filters = useMemo(
    () => parseFilters(searchParams, { start_node, end_node }),
    [searchParams, start_node, end_node],
  );

  const canonicalSearch = useMemo(() => serializeFilters(filters), [filters]);
  const isCanonical = canonicalSearch === searchParams.toString();

  const setFilters = useCallback(
    (patch: Partial<QueryFilters>) => {
      // 改了任何筛选都回到第一页：旧页码在新结果集里可能已越界，分页控件（第 4 项）
      // 直接落在一页空结果上。只有显式翻页（patch 里带 page）才保留传入的页码。
      const next = { ...filters, ...patch };
      if (patch.page === undefined) {
        next.page = 1;
      }
      setSearchParams(serializeFilters(next));
    },
    [filters, setSearchParams],
  );

  const replaceFilters = useCallback(
    (next: QueryFilters) => {
      setSearchParams(serializeFilters(next), { replace: true });
    },
    [setSearchParams],
  );

  return { filters, canonicalSearch, isCanonical, setFilters, replaceFilters };
}
