// 公开查询页测试夹具：形状用 `satisfies` 与 src/api/types.ts 对齐，
// 后端新增/删除字段时这里会先编译报错（design「Test Strategy」的契约守卫）。
// 第 10 项会把它们接进 MSW handler；本项只被 QueryPage / FilterPanel 的测试直接引用。

import type {
  AvailabilityResult,
  ContextInfo,
  DayNode,
  DayResult,
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

/**
 * 第 5 项对抗性轴夹具：把 02 与 05 的 ordinal 对调（02→5、05→2），
 * `axis.nodes` 的数组顺序仍是 01..12。按 ordinal 排出来的顺序是
 * 01,05,03,04,02,06…12——既不是数组顺序，也不是编号升序，用来钉住
 * NodeGrid 只认 ordinal，而不是照响应数组顺序摆格。
 */
export const unorderedAxisMetaFixture = {
  ...metaFixture,
  axis: {
    version: 1,
    blocks: metaFixture.axis.blocks,
    nodes: metaFixture.axis.nodes.map((node) => {
      if (node.node === "02") {
        return { ...node, ordinal: 5 };
      }
      if (node.node === "05") {
        return { ...node, ordinal: 2 };
      }
      return node;
    }),
  },
} satisfies MetaResult;

/** 轴缺失（无当前版本）：axis 为 null 时网格要回退成 12 个「无数据」格，而不是白屏。 */
export const noAxisMetaFixture = {
  ...metaFixture,
  axis: null,
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

/**
 * 第 4 项同名房夹具：两间展示名都叫「综合楼101」（分属不同楼、房间标识不同），
 * 中间夹一间异名房。顺序故意既不是房名字典序（同名两行不相邻），
 * 也不是房间标识升序（Q12 → Q11 → Q10），用来断言前端不重排。
 *
 * 注意：使用方以 `building=Q10` 请求，这里却返回 Q12/Q11 的房间——真实后端
 * （`backend/internal/store/availability.go` 的 `$6 = '' OR rrb.building_id = $6`）
 * 不可能出现这种响应。这是刻意构造的对抗性夹具，钉住两件事：
 * 1) 「看全天状态」的 `building` 取**当前筛选楼**（Q10），不是房间自己的楼（Q12/Q11）；
 * 2) 同名后缀只看房名，不因两间房分属不同楼就免掉后缀。
 */
export const duplicateNameAvailabilityFixture = {
  total: 3,
  items: [
    {
      room_id: "Q12-101",
      name: "综合楼101",
      building_id: "Q12",
      building_name: "文科教学楼",
    },
    {
      room_id: "Q11-302",
      name: "理工楼302",
      building_id: "Q11",
      building_name: "理工教学楼",
    },
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

/** 第 4 项分页夹具第 1 页：total 45 超过单页容量 20，分页控件才会出现第 2 页。 */
export const pagedAvailabilityFixture = {
  total: 45,
  items: [
    {
      room_id: "Q10-201",
      name: "综合楼201",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
    {
      room_id: "Q10-202",
      name: "综合楼202",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
  ],
  date: "2026-11-17",
  term: "2026-2027-1",
  week: 11,
  weekday: 2,
} satisfies AvailabilityResult;

/** 第 4 项分页夹具第 2 页：offset=20 时返回，用来区分「确实换了页」。 */
export const pagedAvailabilitySecondPageFixture = {
  ...pagedAvailabilityFixture,
  items: [
    {
      room_id: "Q10-301",
      name: "综合楼301",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
  ],
} satisfies AvailabilityResult;

/** 第 4 项分页夹具最后一页：total=45、单页 20 → 第 3 页（offset=40），页码越界时钳到这里。 */
export const pagedAvailabilityLastPageFixture = {
  ...pagedAvailabilityFixture,
  items: [
    {
      room_id: "Q10-401",
      name: "综合楼401",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
  ],
} satisfies AvailabilityResult;

/**
 * 第 5 项全天状态夹具的逐节状态，元组为 [小节编号, 大节编码, 状态]。
 * 12 节里 3 节无观测（03 / 07 / 12），其余在空闲与占用之间交替：
 * 空闲 5 节（01/02/06/09/11）、占用 4 节（04/05/08/10）。
 *
 * 数组顺序故意打乱（真实后端按轴顺序返回），用来钉住 NodeGrid 按
 * `meta.axis.nodes` 的 ordinal 摆格，而不是照着响应数组顺序渲染。
 */
const DAY_NODE_STATES: ReadonlyArray<
  readonly [node: string, block: string, state: "free" | "busy" | "no-data"]
> = [
  ["12", "1112", "no-data"],
  ["01", "0102", "free"],
  ["07", "060708", "no-data"],
  ["03", "030405", "no-data"],
  ["11", "1112", "free"],
  ["02", "0102", "free"],
  ["09", "0910", "free"],
  ["04", "030405", "busy"],
  ["06", "060708", "free"],
  ["10", "0910", "busy"],
  ["05", "030405", "busy"],
  ["08", "060708", "busy"],
];

/**
 * 与 backend/internal/query/day.go 的 DayNode 逐字段对齐。
 * 无观测时 state_key 为空、label 为「无数据」、available 恒为 false（dayNodes 的整形口径）；
 * 有观测时 label 是**当前发布版本字典**的展示名（与 metaFixture.dict 一致）。
 */
function dayNode(
  node: string,
  block: string,
  state: "free" | "busy" | "no-data",
): DayNode {
  if (state === "no-data") {
    return {
      node,
      block,
      state_key: "",
      label: "无数据",
      available: false,
      has_data: false,
    };
  }
  return {
    node,
    block,
    state_key: state,
    label: state === "free" ? "空闲" : "占用",
    available: state === "free",
    has_data: true,
  };
}

/** 与 backend/internal/query/day.go 的 DayResult 逐字段对齐。 */
export const dayFixture = {
  room_id: "Q10-101",
  name: "综合楼101",
  building_id: "Q10",
  building_name: "综合教学楼",
  date: "2026-11-18",
  term: "2026-2027-1",
  week: 11,
  weekday: 3,
  nodes: DAY_NODE_STATES.map(([node, block, state]) =>
    dayNode(node, block, state),
  ),
} satisfies DayResult;

/**
 * 第 6 项空态夹具：三档空教室提示标记（Requirement 5.2）。
 * total=0 时后端才会给 hint（backend/internal/query/availability.go 的 availabilityHint），
 * 所以前两档的 items 为空；week_no_schedule 另见下面的对抗性夹具。
 */
export const noMatchAvailabilityFixture = {
  total: 0,
  items: [],
  date: "2026-11-17",
  term: "2026-2027-1",
  week: 11,
  weekday: 2,
  hint: { code: "no_match", message: "没有符合条件的教室" },
} satisfies AvailabilityResult;

/** 「该日期没有可查询的数据」：无当前版本、学期外或该日期不可查时的口径。 */
export const noDataAvailabilityFixture = {
  ...noMatchAvailabilityFixture,
  hint: { code: "no_data", message: "没有已发布的数据" },
} satisfies AvailabilityResult;

/**
 * 第 6 项对抗性夹具：week_no_schedule 与命中项同时存在。
 * 真实后端只在 total=0 时给 hint，这里刻意让提示与结果并存，用来钉住
 * 「week_no_schedule 用置顶提示、结果照常展示」——页面不能把它当空态替换结果区。
 */
export const weekNoScheduleAvailabilityFixture = {
  ...pagedAvailabilityFixture,
  hint: { code: "week_no_schedule", message: "该周疑似无排课" },
} satisfies AvailabilityResult;

/** 第 6 项过期夹具：第 11 周被后端标记 stale，页面应提示但照常展示数据。 */
export const staleMetaFixture = {
  ...metaFixture,
  weeks: [
    {
      week: 11,
      published: true,
      last_success_at: "2026-11-14T07:10:12Z",
      stale: true,
    },
  ],
} satisfies MetaResult;

/** 第 6 项非教学周夹具：/api/context 的 in_calendar=false（Requirement 5.4）。 */
export const outOfCalendarContextFixture = {
  ...contextFixture,
  term: "",
  week: 0,
  weekday: 0,
  in_calendar: false,
} satisfies ContextInfo;

/**
 * 第 6 项无当前数据版本夹具：release/dict/axis 为 null、empty 为 true（MetaResult 契约）。
 * 这一态下后端 total=0 仍会给 hint=no_data，页面必须让「暂无数据版本」压过 hint，
 * 否则该档文案永远不可达（上一轮复审 minor 2）。
 */
export const emptyMetaFixture = {
  release: null,
  dict: null,
  axis: null,
  buildings: [],
  weeks: [],
  empty: true,
} satisfies MetaResult;

/**
 * 第 6 项 week_no_schedule 无命中夹具：真实后端在该周无观测时给 total=0 + 该 hint。
 * 页面只留置顶提示，不画空列表与「共 0 间空教室」分页（上一轮复审 minor 3）。
 */
export const weekNoScheduleEmptyAvailabilityFixture = {
  total: 0,
  items: [],
  date: "2026-11-17",
  term: "2026-2027-1",
  week: 11,
  weekday: 2,
  hint: { code: "week_no_schedule", message: "该周疑似无排课" },
} satisfies AvailabilityResult;

/** 第 6 项全天状态空态：整日无观测（Requirement 4.5 的 day_no_data）。 */
export const dayNoDataHintFixture = {
  ...dayFixture,
  // 整日无观测时后端把每一节都整形成 state_key=""、label="无数据"、has_data=false。
  nodes: dayFixture.nodes.map((node) => ({
    ...node,
    state_key: "",
    label: "无数据",
    available: false,
    has_data: false,
  })),
  hint: { code: "day_no_data", message: "该日无数据" },
} satisfies DayResult;

/**
 * 第 6 项全天状态对抗性夹具：week_no_schedule 与逐节状态并存。
 * 真实后端在该周无观测时不会返回逐节状态；这里刻意保留节点，
 * 用来钉住「该周疑似无排课只置顶提示，网格照常展示」。
 */
export const dayWeekNoScheduleFixture = {
  ...dayFixture,
  hint: { code: "week_no_schedule", message: "该周疑似无排课" },
} satisfies DayResult;
