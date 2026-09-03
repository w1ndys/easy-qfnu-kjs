import { describe, expect, it } from 'vitest';

import {
  computeContext,
  formatCivilDate,
  mondaysStrictlyAfter,
  parseCivilDate,
  shanghaiCivilDate,
  weekdayOf,
} from '../time.ts';

const TERM = '2026-2027-1';

const teaching = (over: Partial<{ date: string; week: number; total_weeks: number; in_teaching_calendar: boolean }>) => ({
  date: '2026-09-01',
  week: 2,
  total_weeks: 10,
  in_teaching_calendar: true,
  ...over,
});

describe('Asia/Shanghai 公历日期', () => {
  it('把 UTC 时刻换算为北京公历日期（跨午夜）', () => {
    const d = shanghaiCivilDate(new Date('2026-09-03T16:30:00Z')); // = 09-04 00:30 +08
    expect(formatCivilDate(d)).toBe('2026-09-04');
  });

  it('weekday：周一=1 … 周日=7', () => {
    const mon = parseCivilDate('2026-09-07');
    const sun = parseCivilDate('2026-09-13');
    expect(weekdayOf(mon!)).toBe(1);
    expect(weekdayOf(sun!)).toBe(7);
    const thu = parseCivilDate('2026-09-03');
    expect(weekdayOf(thu!)).toBe(4);
  });

  it('parseCivilDate 拒绝非法日期', () => {
    expect(parseCivilDate('2026-13-01')).toBeNull();
    expect(parseCivilDate('2026-02-30')).toBeNull();
    expect(parseCivilDate('2026-9-1')).toBeNull();
    expect(parseCivilDate('not-a-date')).toBeNull();
  });

  it('mondaysStrictlyAfter 只统计 (a, b] 内的周一', () => {
    const tue = parseCivilDate('2026-09-01')!;
    const thu = parseCivilDate('2026-09-03')!;
    const mon = parseCivilDate('2026-09-07')!;
    const sun = parseCivilDate('2026-09-13')!;
    expect(mondaysStrictlyAfter(tue, thu)).toBe(0);
    expect(mondaysStrictlyAfter(tue, mon)).toBe(1);
    expect(mondaysStrictlyAfter(tue, sun)).toBe(1); // 9-07 周一
    expect(mondaysStrictlyAfter(sun, mon)).toBe(0); // b <= a
  });
});

describe('computeContext（周一 00:00 换周，Q148）', () => {
  it('教学周内返回当天日期/星期/周次', () => {
    const ctx = computeContext(TERM, teaching({}), new Date('2026-09-03T00:00:00+08:00'));
    expect(ctx).toEqual({
      date: '2026-09-03',
      weekday: 4,
      week: 2,
      term: TERM,
      in_teaching_calendar: true,
    });
  });

  it('锚点为周日时，周一凌晨自动换到下一周', () => {
    const ctx = computeContext(
      TERM,
      teaching({ date: '2026-09-06', week: 2 }),
      new Date('2026-09-07T00:30:00+08:00'),
    );
    expect(ctx.week).toBe(3);
    expect(ctx.weekday).toBe(1);
    expect(ctx.date).toBe('2026-09-07');
  });

  it('锚点已在本周一刷新时不重复换周', () => {
    const ctx = computeContext(
      TERM,
      teaching({ date: '2026-09-07', week: 3 }),
      new Date('2026-09-07T10:00:00+08:00'),
    );
    expect(ctx.week).toBe(3);
  });

  it('非教学周返回 in_teaching_calendar=false 且 week=null', () => {
    const ctx = computeContext(
      TERM,
      teaching({ week: 0, in_teaching_calendar: false }),
      new Date('2026-09-03T00:00:00+08:00'),
    );
    expect(ctx.in_teaching_calendar).toBe(false);
    expect(ctx.week).toBeNull();
  });

  it('推算越过 total_weeks 时不伪造周次', () => {
    const ctx = computeContext(
      TERM,
      teaching({ date: '2026-09-27', week: 4, total_weeks: 4 }), // 学期最后一周周日
      new Date('2026-09-28T00:10:00+08:00'), // 周一凌晨
    );
    expect(ctx.in_teaching_calendar).toBe(false);
    expect(ctx.week).toBeNull();
  });

  it('锚点日期非法时按非教学周处理', () => {
    const ctx = computeContext(
      TERM,
      teaching({ date: 'not-a-date' }),
      new Date('2026-09-03T00:00:00+08:00'),
    );
    expect(ctx.in_teaching_calendar).toBe(false);
    expect(ctx.week).toBeNull();
  });
});
