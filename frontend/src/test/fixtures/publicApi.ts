// 公开查询页测试夹具：形状用 `satisfies` 与 src/api/types.ts 对齐，
// 后端新增/删除字段时这里会先编译报错（design「Test Strategy」的契约守卫）。
// 第 10 项会把它们接进 MSW handler；本项只被 QueryPage / FilterPanel 的测试直接引用。

import type {
  AvailabilityResult,
  ContextInfo,
  MetaResult,
} from "../../api/types";

/** 与 backend/internal/query/query.go 的 ContextInfo 逐字段对齐。 */
export const contextFixture = {
  date: "2026-11-15",
  term: "2026-2027-1",
  week: 11,
  weekday: 6,
  in_calendar: true,
} satisfies ContextInfo;

/**
 * 与 backend/internal/query/meta.go 的 MetaResult 对齐：3 栋白名单教学楼，
 * 12 个小节分属 5 个大节（轴顺序故意与数组顺序无关，逼实现按 ordinal 排序）。
 */
export const metaFixture = {
  release: {
    release_id: "20261114T071012Z",
    generated_at: "2026-11-14T07:10:12Z",
  },
  dict: {
    version: 1,
    states: {
      free: { label: "空闲", available: true },
      busy: { label: "占用", available: false },
    },
  },
  axis: {
    version: 1,
    blocks: ["0102", "030405", "060708", "0910", "1112"],
    nodes: [
      { node: "01", ordinal: 1, block: "0102", block_ordinal: 1 },
      { node: "02", ordinal: 2, block: "0102", block_ordinal: 1 },
      { node: "03", ordinal: 3, block: "030405", block_ordinal: 2 },
      { node: "04", ordinal: 4, block: "030405", block_ordinal: 2 },
      { node: "05", ordinal: 5, block: "030405", block_ordinal: 2 },
      { node: "06", ordinal: 6, block: "060708", block_ordinal: 3 },
      { node: "07", ordinal: 7, block: "060708", block_ordinal: 3 },
      { node: "08", ordinal: 8, block: "060708", block_ordinal: 3 },
      { node: "09", ordinal: 9, block: "0910", block_ordinal: 4 },
      { node: "10", ordinal: 10, block: "0910", block_ordinal: 4 },
      { node: "11", ordinal: 11, block: "1112", block_ordinal: 5 },
      { node: "12", ordinal: 12, block: "1112", block_ordinal: 5 },
    ],
  },
  buildings: [
    { building_id: "Q10", name: "综合教学楼" },
    { building_id: "Q11", name: "理工教学楼" },
    { building_id: "Q12", name: "文科教学楼" },
  ],
  weeks: [
    {
      week: 11,
      published: true,
      last_success_at: "2026-11-14T07:10:12Z",
      stale: false,
    },
  ],
  empty: false,
} satisfies MetaResult;

/** 空教室查询的一条命中；本项只关心请求参数，内容供第 4 项复用。 */
export const availabilityFixture = {
  total: 1,
  items: [
    {
      room_id: "Q10-101",
      name: "综合楼101",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
  ],
  date: "2026-11-17",
  term: "2026-2027-1",
  week: 11,
  weekday: 2,
} satisfies AvailabilityResult;
