// 日期项标签（tasklist 第 3 项）：偏移 0 显示「今天」、1 显示「明天」，其余显示「M-D」。
// 业务日期是 Asia/Shanghai 的日历日（PRD 7.1），所以这里只做纯字符串的日历日加减，
// 不经过 Date / 时区换算，也不推算目标日期的学期周次与星期（design D3：那两项只来自服务端）。

/** 解析后的业务日期（只含日历日，没有时刻与时区）。 */
interface CalendarDate {
  year: number;
  month: number;
  day: number;
}

const ISO_DATE_PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/;

/** 闰年判定：与公历规则一致，2000 是闰年、1900 不是。 */
function isLeapYear(year: number): boolean {
  return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0;
}

/** 某年某月的天数；月份越界时返回 0，让调用方判定日期非法。 */
function daysInMonth(year: number, month: number): number {
  if (month < 1 || month > 12) {
    return 0;
  }
  const lengths = [
    31,
    isLeapYear(year) ? 29 : 28,
    31,
    30,
    31,
    30,
    31,
    31,
    30,
    31,
    30,
    31,
  ];
  return lengths[month - 1];
}

/** 解析 `YYYY-MM-DD`；非法日期（含 2 月 30 日这类）返回 null，不做静默纠正。 */
function parseCalendarDate(value: string): CalendarDate | null {
  const match = ISO_DATE_PATTERN.exec(value);
  if (!match) {
    return null;
  }
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  if (day < 1 || day > daysInMonth(year, month)) {
    return null;
  }
  return { year, month, day };
}

function pad(value: number, length: number): string {
  return String(value).padStart(length, "0");
}

/**
 * 在业务日期上加减天数（偏移范围只有 0..10，逐日进位足够，也避免引入日期库）。
 * 入参非法时原样返回：日期来自 `/api/context` 的契约，真异常时宁可显示原值也不抛错。
 */
export function addDays(businessDate: string, days: number): string {
  const parsed = parseCalendarDate(businessDate);
  if (!parsed) {
    return businessDate;
  }

  let { year, month, day } = parsed;
  let remaining = Math.trunc(days);
  const step = remaining >= 0 ? 1 : -1;

  while (remaining !== 0) {
    day += step;
    if (day > daysInMonth(year, month)) {
      day = 1;
      month += 1;
      if (month > 12) {
        month = 1;
        year += 1;
      }
    } else if (day < 1) {
      month -= 1;
      if (month < 1) {
        month = 12;
        year -= 1;
      }
      day = daysInMonth(year, month);
    }
    remaining -= step;
  }

  return `${pad(year, 4)}-${pad(month, 2)}-${pad(day, 2)}`;
}

/** 只取「月-日」用于日期项标签，不补前导零（需求写法 M-D）。 */
export function formatMonthDay(businessDate: string): string {
  const parsed = parseCalendarDate(businessDate);
  if (!parsed) {
    return businessDate;
  }
  return `${parsed.month}-${parsed.day}`;
}

/**
 * 日期项文案：偏移 0「今天」、1「明天」、其余目标日期的「M-D」。
 * 基准日是 `/api/context` 返回的业务日期，不是浏览器时钟，保证与服务端同一天。
 */
export function dateOptionLabel(baseDate: string, offset: number): string {
  if (offset === 0) {
    return "今天";
  }
  if (offset === 1) {
    return "明天";
  }
  return formatMonthDay(addDays(baseDate, offset));
}
