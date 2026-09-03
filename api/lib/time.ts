// Asia/Shanghai 时区与教学日历周次计算（决策文件 §10 / Q148）。
// API 一律按北京时间计算日期、星期与教学周，不依赖 Vercel 区域或用户设备时区。
// 两次采集之间按“锚点日期之后经过的周一 00:00 数量”换周（周一 00:00 为换周边界）。

export const TERM_TIMEZONE = 'Asia/Shanghai';

export interface CivilDate {
  year: number;
  month: number; // 1..12
  day: number; // 1..31
}

const DATE_PARTS_FMT = new Intl.DateTimeFormat('en-US', {
  timeZone: TERM_TIMEZONE,
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
});

/** 测试/演示可用 EASY_KJS_NOW 冻结“当前”时刻（ISO 8601）。 */
export function requestNow(): number {
  const frozen = process.env.EASY_KJS_NOW;
  if (frozen) {
    const t = Date.parse(frozen);
    if (!Number.isNaN(t)) return t;
  }
  return Date.now();
}

/** 取 now（任意时刻）在北京时区下的公历日期。 */
export function shanghaiCivilDate(now: Date): CivilDate {
  const parts = DATE_PARTS_FMT.formatToParts(now);
  let year = 0;
  let month = 0;
  let day = 0;
  for (const p of parts) {
    if (p.type === 'year') year = Number(p.value);
    else if (p.type === 'month') month = Number(p.value);
    else if (p.type === 'day') day = Number(p.value);
  }
  return { year, month, day };
}

/** 星期：1=周一 … 7=周日（由公历日期本身决定，与观察时区无关）。 */
export function weekdayOf(d: CivilDate): number {
  const jsDay = new Date(Date.UTC(d.year, d.month - 1, d.day)).getUTCDay(); // 0=周日
  return jsDay === 0 ? 7 : jsDay;
}

export function formatCivilDate(d: CivilDate): string {
  const mm = String(d.month).padStart(2, '0');
  const dd = String(d.day).padStart(2, '0');
  return `${d.year}-${mm}-${dd}`;
}

/** 解析 YYYY-MM-DD；非法返回 null。 */
export function parseCivilDate(s: string): CivilDate | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]);
  const day = Number(m[3]);
  if (month < 1 || month > 12 || day < 1 || day > 31) return null;
  // 拒绝 2026-02-30 之类的无效日期。
  const probe = new Date(Date.UTC(year, month - 1, day));
  if (
    probe.getUTCFullYear() !== year ||
    probe.getUTCMonth() !== month - 1 ||
    probe.getUTCDate() !== day
  ) {
    return null;
  }
  return { year, month, day };
}

/** 公历日期 → 自纪元以来的整天数（UTC 基准，仅用于整数差值，无 DST 干扰）。 */
export function dayNumber(d: CivilDate): number {
  return Math.floor(Date.UTC(d.year, d.month - 1, d.day) / 86_400_000);
}

export function addDays(d: CivilDate, n: number): CivilDate {
  const dt = new Date(Date.UTC(d.year, d.month - 1, d.day + n));
  return {
    year: dt.getUTCFullYear(),
    month: dt.getUTCMonth() + 1,
    day: dt.getUTCDate(),
  };
}

/** (a, b] 区间内经过的周一数量；b <= a 时为 0。 */
export function mondaysStrictlyAfter(a: CivilDate, b: CivilDate): number {
  if (dayNumber(b) <= dayNumber(a)) return 0;
  let count = 0;
  let cur = addDays(a, 1);
  while (dayNumber(cur) <= dayNumber(b)) {
    if (weekdayOf(cur) === 1) count += 1;
    cur = addDays(cur, 1);
  }
  return count;
}

export interface ContextResult {
  /** Asia/Shanghai 今天，YYYY-MM-DD。 */
  date: string;
  /** 1=周一 … 7=周日。 */
  weekday: number;
  /** 当前教学周；不在教学周内为 null（不伪造周次）。 */
  week: number | null;
  term: string;
  in_teaching_calendar: boolean;
}

/**
 * 由 manifest.anchor 与当前时刻计算公开 context。
 * 锚点每天 04:10 被采集器刷新；两次刷新之间按周一 00:00 换周推进。
 * 当前日期无法映射到有效教学周（锚点声明非教学周，或推算越过 1..total_weeks）时，
 * 返回 in_teaching_calendar=false 且 week=null。
 */
export function computeContext(
  term: string,
  anchor: { date: string; week: number; total_weeks: number; in_teaching_calendar: boolean },
  now: Date,
): ContextResult {
  const today = shanghaiCivilDate(now);
  const date = formatCivilDate(today);
  const weekday = weekdayOf(today);

  const notTeaching: ContextResult = {
    date,
    weekday,
    week: null,
    term,
    in_teaching_calendar: false,
  };

  const anchorDate = parseCivilDate(anchor.date);
  if (!anchorDate) return notTeaching;
  if (!anchor.in_teaching_calendar || anchor.total_weeks < 1 || anchor.week < 1) {
    return notTeaching;
  }
  const crossed = mondaysStrictlyAfter(anchorDate, today);
  const week = anchor.week + crossed;
  if (week < 1 || week > anchor.total_weeks) return notTeaching;
  return { date, weekday, week, term, in_teaching_calendar: true };
}
