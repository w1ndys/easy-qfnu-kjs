import { describe, expect, it } from 'vitest';

import { weekStatus } from '../staleness.ts';
import type { ManifestWeekEntry } from '../types.ts';

const NOW = Date.parse('2026-09-03T00:00:00+08:00');

function entry(week: number, lastSuccessAt: string, over?: Partial<ManifestWeekEntry>): ManifestWeekEntry {
  return {
    week,
    snapshot_id: `snap-week-${week}`,
    sha256: 'a'.repeat(64),
    generated_at: lastSuccessAt,
    last_success_at: lastSuccessAt,
    ...over,
  };
}

describe('weekStatus（Q130：近期周 36h / 远期周 8d / 最近失败即 stale）', () => {
  it('缺失快照 → missing', () => {
    expect(weekStatus(undefined, { currentWeek: 2, now: NOW })).toBe('missing');
  });

  it('近期周（当前..当前+4）36h 内 → fresh', () => {
    const e = entry(2, '2026-09-02T04:10:00+08:00'); // ~19.7h
    expect(weekStatus(e, { currentWeek: 2, now: NOW })).toBe('fresh');
  });

  it('近期周超过 36h → stale', () => {
    const e = entry(2, '2026-09-01T00:00:00+08:00'); // 48h
    expect(weekStatus(e, { currentWeek: 2, now: NOW })).toBe('stale');
  });

  it('远期周 8 天内仍 fresh、超过 8 天 stale', () => {
    const within8d = entry(1, '2026-08-28T00:00:00+08:00'); // 6 天
    expect(weekStatus(within8d, { currentWeek: 2, now: NOW })).toBe('fresh');
    const beyond8d = entry(1, '2026-08-24T04:10:00+08:00'); // ~9.8 天
    expect(weekStatus(beyond8d, { currentWeek: 2, now: NOW })).toBe('stale');
  });

  it('窗口边界：week == 当前+4 走 36h，week > 当前+4 走 8d', () => {
    const inWindow = entry(6, '2026-09-01T00:00:00+08:00'); // 48h
    expect(weekStatus(inWindow, { currentWeek: 2, now: NOW })).toBe('stale');
    const outWindow = entry(7, '2026-09-01T00:00:00+08:00'); // 48h，但已出窗口
    expect(weekStatus(outWindow, { currentWeek: 2, now: NOW })).toBe('fresh');
  });

  it('最近一次尝试失败（错误晚于最近成功）→ stale', () => {
    const e = entry(
      2,
      '2026-09-02T04:10:00+08:00',
      { last_error_code: 'collect_failed', last_attempt_at: '2026-09-02T20:00:00+08:00' },
    );
    expect(weekStatus(e, { currentWeek: 2, now: NOW })).toBe('stale');
  });

  it('失败后已恢复（成功晚于尝试）→ 不再因旧错误 stale', () => {
    const e = entry(
      2,
      '2026-09-02T22:00:00+08:00',
      { last_error_code: 'collect_failed', last_attempt_at: '2026-09-02T20:00:00+08:00' },
    );
    expect(weekStatus(e, { currentWeek: 2, now: NOW })).toBe('fresh');
  });

  it('非教学周（currentWeek=null）所有周走 8d 阈值', () => {
    const young = entry(1, '2026-09-01T00:00:00+08:00'); // 2 天
    expect(weekStatus(young, { currentWeek: null, now: NOW })).toBe('fresh');
  });
});
