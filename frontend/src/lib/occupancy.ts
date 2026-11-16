// 占用热力图的色阶与占比文案（tasklist 第 8 项；design「占用热力图实现」）。
//
// 色阶只做纯计算：分档只由占用率决定，具体颜色由组件从 antd token 取。
// 这样边界（0 / 0.5 / 1）能在单测里逐条钉住，不需要渲染，也不依赖主题。
//
// 分档口径（与图例 0 / 25% / 50% / 75% / 100% 五个刻度一一对应）：
// - 0 档：占用率为 0 或非法值，用中性底色——它表示「没有占用」，与低占用区分；
// - 1..4 档：正值按 (0, 25%]、(25%, 50%]、(50%, 75%]、(75%, 100%] 分四档，
//   档位越高品牌色越深。后端没有观测的格给 0（backend/internal/query/screen.go
//   的 occupancyRate），所以「无观测」自然落在中性档，前端不另造一个 -1。

/** 色阶档数：0 + 四档正值，与图例的 5 个色块一致。 */
export const OCCUPANCY_BUCKET_COUNT = 5;

/**
 * 每档的填充透明度，下标即档位。
 * 0 档不使用品牌色（中性底色），这里的 1 只是「不额外减淡」；
 * 1..4 档依次加深，100% 档就是纯品牌色。
 */
export const OCCUPANCY_FILL_OPACITY: readonly number[] = [
  1, 0.25, 0.5, 0.75, 1,
];

/**
 * 占用率 → 色阶档位（0..4）。
 *
 * 边界口径：
 * - `rate <= 0`（含 0、负数）与 `NaN` → 0 档，中性底色；
 * - `rate >= 1`（含越界的 >1 与 Infinity）→ 4 档，不产生第 5 档；
 * - 其余用 `ceil(rate * 4)`：0.01 落 1 档、0.25 仍落 1 档、0.2501 落 2 档、
 *   0.5 落 2 档、0.75 落 3 档、0.7501 落 4 档。
 * 取 ceil 而不是 round：任何正值都必须至少进 1 档，否则 10% 占用会和
 * 「0 占用 / 无观测」显示成同一个中性色。
 */
export function occupancyBucket(rate: number): number {
  if (Number.isNaN(rate) || rate <= 0) {
    return 0;
  }
  if (rate >= 1) {
    return OCCUPANCY_BUCKET_COUNT - 1;
  }
  return Math.ceil(rate * (OCCUPANCY_BUCKET_COUNT - 1));
}

/**
 * 占用率 → 百分比文案，如 0.424 → 「42%」。
 *
 * 四舍五入到整数百分比；越界值钳到 0–100%，不显示「-3%」或「150%」这种
 * 由脏数据带出来的读数。非有限值给「—」占位，避免 NaN 漏到界面上。
 */
export function formatRatePercent(rate: number): string {
  if (!Number.isFinite(rate)) {
    return "—";
  }
  const clamped = Math.min(1, Math.max(0, rate));
  return `${Math.round(clamped * 100)}%`;
}
