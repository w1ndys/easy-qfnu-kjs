// 四档提示标记的文案表（design「四态」表；Requirement 5.2 与 Requirement 4.5）。
// 单独成模块的理由：EmptyView 要按它渲染空态，页面的 aria-live 播报也要读同一份标题，
// 两处共用一张表才不会出现「界面写一套、读屏播另一套」。
// 键即后端 hint.code 的稳定取值（backend/internal/query/availability.go 与 day.go）。

import type { AvailabilityHint } from "../api/types";

/** 空态文案：标题 + 一条可操作建议。 */
export interface StateCopy {
  title: string;
  suggestion: string;
}

/**
 * week_no_schedule 的码值：它与其他三档的展示位置不同（置顶提示、结果照常展示），
 * 页面要按码分支，所以单独导出常量，免得两边各写一遍字符串。
 */
export const HINT_WEEK_NO_SCHEDULE = "week_no_schedule";

const HINT_COPY: Readonly<Record<string, StateCopy>> = {
  no_match: {
    title: "没有符合条件的教室",
    suggestion: "换个日期、教学楼或关键词再试",
  },
  no_data: {
    title: "该日期没有可查询的数据",
    suggestion: "换一个本学期的日期，或稍后再来",
  },
  [HINT_WEEK_NO_SCHEDULE]: {
    title: "该周疑似无排课",
    suggestion: "换一个已发布数据的日期再试",
  },
  day_no_data: {
    title: "该日无数据",
    suggestion: "换一间教室，或看同教室的其他日期",
  },
};

/** 未知 hint.code 的兜底：不猜含义，也不把后端 message 当可依赖文案。 */
const FALLBACK_COPY: StateCopy = {
  title: "暂无数据",
  suggestion: "换个日期或教学楼再试",
};

/** meta.empty（无当前数据版本）的空态；与 hint 同一档展示，文案说明原因。 */
const NO_RELEASE_COPY: StateCopy = {
  title: "暂无数据版本",
  suggestion: "数据版本还没有发布，稍后再来",
};

/**
 * 按提示标记取文案：
 * - 有 hint 且码已知 → 该档文案；
 * - 有 hint 但码未知 → 通用兜底（不猜新码含义）；
 * - 没有 hint 但 meta.empty → 「暂无数据版本」；
 * - 两者都没有 → 通用兜底。
 */
export function getEmptyCopy(
  hint: AvailabilityHint | null | undefined,
  noRelease = false,
): StateCopy {
  if (hint !== null && hint !== undefined) {
    return HINT_COPY[hint.code] ?? FALLBACK_COPY;
  }
  return noRelease ? NO_RELEASE_COPY : FALLBACK_COPY;
}
