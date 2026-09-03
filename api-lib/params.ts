// 查询参数解析与校验（决策文件 §11）。
// group_id 必填；keyword 可选（NFKC/去首尾空格/ASCII 大小写不敏感/≤32 字符/普通子串）；
// week/day 省略时使用当前 context（非教学周省略 week → 400 not_in_teaching）；
// start_node/end_node 默认 01/12，格式 ^(0[1-9]|1[0-2])$ 且 start <= end。
import { ApiError, CODES } from './errors.ts';
import { keywordLength, normalizeKeyword } from './keyword.ts';
import type { ContextResult } from './time.ts';
import type { Manifest } from './types.ts';

const NODE_RE = /^(0[1-9]|1[0-2])$/;
const WEEK_RE = /^\d{1,2}$/;
const DAY_RE = /^[1-7]$/;

export interface QueryBase {
  groupId: string;
  /** 归一化后的关键词；null 表示无过滤。 */
  keyword: string | null;
  week: number;
  day: number;
}

function requireGroup(searchParams: URLSearchParams, manifest: Manifest): string {
  const groupId = searchParams.get('group_id') ?? '';
  if (groupId === '') {
    throw new ApiError(400, CODES.invalidParameter, '缺少必填参数 group_id');
  }
  if (!manifest.groups.some((g) => g.id === groupId)) {
    const available = manifest.groups.map((g) => g.id).join('、');
    throw new ApiError(
      404,
      CODES.notCollected,
      `分组 ${groupId} 未收录${available ? `，可用分组：${available}` : ''}`,
    );
  }
  return groupId;
}

function parseKeyword(searchParams: URLSearchParams): string | null {
  const raw = searchParams.get('keyword');
  if (raw === null) return null;
  const normalized = normalizeKeyword(raw);
  if (normalized === '') return null;
  if (keywordLength(normalized) > 32) {
    throw new ApiError(400, CODES.invalidParameter, 'keyword 最多 32 个字符');
  }
  return normalized;
}

function parseWeek(value: string, totalWeeks: number): number {
  if (!WEEK_RE.test(value)) {
    throw new ApiError(400, CODES.invalidParameter, 'week 必须为 1–30 的整数');
  }
  const n = Number(value);
  if (n < 1 || n > 30) {
    throw new ApiError(400, CODES.invalidParameter, 'week 必须在 1–30 之间');
  }
  if (totalWeeks > 0 && n > totalWeeks) {
    throw new ApiError(
      400,
      CODES.invalidParameter,
      `week 超出本学期范围（1–${totalWeeks}）`,
    );
  }
  return n;
}

function parseDay(value: string): number {
  if (!DAY_RE.test(value)) {
    throw new ApiError(400, CODES.invalidParameter, 'day 必须为 1–7 的整数');
  }
  return Number(value);
}

/** 解析 group_id/keyword/week/day；省略的 week/day 回退到当前 context。 */
export function parseQueryBase(
  searchParams: URLSearchParams,
  manifest: Manifest,
  ctx: ContextResult,
): QueryBase {
  const groupId = requireGroup(searchParams, manifest);
  const keyword = parseKeyword(searchParams);

  const rawWeek = searchParams.get('week');
  let week: number;
  if (rawWeek === null) {
    if (ctx.week === null) {
      throw new ApiError(
        400,
        CODES.notInTeaching,
        '当前不在教学周内，省略 week 无法确定查询周次，请显式指定 week',
      );
    }
    week = ctx.week;
  } else {
    week = parseWeek(rawWeek, manifest.anchor.total_weeks);
  }

  const rawDay = searchParams.get('day');
  const day = rawDay === null ? ctx.weekday : parseDay(rawDay);

  return { groupId, keyword, week, day };
}

/** 解析可选节次参数；返回两位字符串（默认 01/12），并校验 start <= end。 */
export function parseNodeSpan(
  searchParams: URLSearchParams,
): { startNode: string; endNode: string } {
  const startRaw = searchParams.get('start_node');
  const endRaw = searchParams.get('end_node');
  const startNode = startRaw === null ? '01' : startRaw;
  const endNode = endRaw === null ? '12' : endRaw;
  if (!NODE_RE.test(startNode)) {
    throw new ApiError(400, CODES.invalidParameter, 'start_node 必须为 01–12 的两位数字');
  }
  if (!NODE_RE.test(endNode)) {
    throw new ApiError(400, CODES.invalidParameter, 'end_node 必须为 01–12 的两位数字');
  }
  if (Number(startNode) > Number(endNode)) {
    throw new ApiError(400, CODES.invalidParameter, 'start_node 不能晚于 end_node');
  }
  return { startNode, endNode };
}
