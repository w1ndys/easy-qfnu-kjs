// 数据大屏缩放系数（tasklist 第 7 项；design D6、正确性属性 8）。
//
// 大屏按固定 1920×1080 设计稿排版：窗口变化只改整体缩放系数与居中留边，
// 内层布局一个像素都不重排。因此这里只做纯计算——不读 window、不测量 DOM，
// 窗口尺寸由 ScreenLayout 传进来，边界才能在单测里钉住。

/** 设计稿宽度（px）：大屏内层画布恒定这个宽度。 */
export const SCREEN_WIDTH = 1920;

/** 设计稿高度（px）：大屏内层画布恒定这个高度。 */
export const SCREEN_HEIGHT = 1080;

/** 缩放系数与居中留边：内层画布按 `top left` 缩放后，左上角要落在 (offsetX, offsetY)。 */
export interface ScreenGeometry {
  /** 缩放系数 `min(vw / 1920, vh / 1080)`。 */
  scale: number;
  /** 水平留边（px）；16:9 视口为 0，更窄的视口为 0，更宽的视口居中。 */
  offsetX: number;
  /** 垂直留边（px）；更矮的视口为 0，更高的视口居中。 */
  offsetY: number;
}

/**
 * 视口尺寸是否可用于计算。
 * 0、负数与 NaN 都算不可用：除出 0 会让整块画布消失，除出 NaN 会让 style 直接失效。
 */
function isUsableViewport(width: number, height: number): boolean {
  return (
    Number.isFinite(width) && Number.isFinite(height) && width > 0 && height > 0
  );
}

/**
 * 计算缩放系数：`min(viewportWidth / 1920, viewportHeight / 1080)`。
 *
 * 取 min 而不是分别缩放：等比缩放才能保证 16:9 恰好铺满、非 16:9 只留黑边。
 * 尺寸不可用时退化为 1（1:1 设计稿），比返回 0 让画布凭空消失更容易定位问题。
 */
export function computeScale(
  viewportWidth: number,
  viewportHeight: number,
): number {
  if (!isUsableViewport(viewportWidth, viewportHeight)) {
    return 1;
  }
  return Math.min(viewportWidth / SCREEN_WIDTH, viewportHeight / SCREEN_HEIGHT);
}

/**
 * 计算缩放系数与居中留边。
 *
 * 留边公式是 `(视口 - 设计稿 × scale) / 2`；因为 scale 取 min，
 * 两个方向上缩放后的画布都不会超过视口，所以合法的视口尺寸不会算出负留边。
 * 尺寸不可用时不居中（留边 0），与 `computeScale` 的 1:1 退化保持一致。
 */
export function computeScreenGeometry(
  viewportWidth: number,
  viewportHeight: number,
): ScreenGeometry {
  const scale = computeScale(viewportWidth, viewportHeight);
  if (!isUsableViewport(viewportWidth, viewportHeight)) {
    return { scale, offsetX: 0, offsetY: 0 };
  }
  return {
    scale,
    offsetX: (viewportWidth - SCREEN_WIDTH * scale) / 2,
    offsetY: (viewportHeight - SCREEN_HEIGHT * scale) / 2,
  };
}
