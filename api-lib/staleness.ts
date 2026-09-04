// 周快照新鲜度计算（决策文件 §7 / §9 / Q130）。
// stale 由 API 依据当前北京时间、周类别、last_success_at 与 manifest 中最近尝试结果动态计算：
//   - 最近一次采集失败（last_error_code 且最近尝试晚于最近成功）→ stale；
//   - 近期周（当前周..当前周+4，含当前周）距最近成功采集超过 36 小时 → stale；
//   - 其余周超过 8 天 → stale；
//   - 教学日历内但没有任何已发布快照的周 → missing。
import type { ManifestWeekEntry } from './types.js';

export type WeekStatus = 'fresh' | 'stale' | 'missing';

export const RECENT_WINDOW_WEEKS = 4;
export const RECENT_THRESHOLD_MS = 36 * 60 * 60 * 1000; // 36 小时
export const FAR_THRESHOLD_MS = 8 * 24 * 60 * 60 * 1000; // 8 天

export function weekStatus(
  entry: ManifestWeekEntry | undefined,
  opts: { currentWeek: number | null; now: number },
): WeekStatus {
  if (!entry) return 'missing';

  const success = Date.parse(entry.last_success_at);
  if (Number.isNaN(success)) return 'stale';

  // 最近一次尝试失败：记录失败错误码，且没有更新的成功覆盖它。
  if (entry.last_error_code != null) {
    if (entry.last_attempt_at == null) return 'stale';
    const attempt = Date.parse(entry.last_attempt_at);
    if (Number.isNaN(attempt) || attempt > success) return 'stale';
  }

  const recent =
    opts.currentWeek != null &&
    entry.week >= opts.currentWeek &&
    entry.week <= opts.currentWeek + RECENT_WINDOW_WEEKS;
  const age = opts.now - success;
  return age > (recent ? RECENT_THRESHOLD_MS : FAR_THRESHOLD_MS) ? 'stale' : 'fresh';
}

export function isStale(status: WeekStatus): boolean {
  return status === 'stale';
}
