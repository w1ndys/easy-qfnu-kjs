// 公开页三档响应式布局（tasklist 第 10 项；design「响应式断点」、Requirement 8.1–8.4）。
//
// 断点口径固定为 <600 / 600–1024 / ≥1024：
// - compact：单列，查询按钮固定在视口底部单手可达区；
// - medium：双列，筛选左、结果右，按钮回到筛选区内；
// - wide：三列，筛选占 1 列、结果占 2 列，并在结果区内再分两列房间行。
//
// 这里只做纯计算：不读 window、不碰 DOM，断点边界才能在单测里逐条钉住。
// 实际宽度由 hooks/useViewportTier 读入，大屏（/screen）不走本文件——它按
// 1920×1080 设计稿等比缩放，不重排（design D5/D6）。

/** 单列布局的上界（不含）：视口宽度小于它时用 compact。 */
export const COMPACT_MAX_WIDTH = 600;

/** 三列布局的下界（含）：视口宽度不小于它时用 wide。 */
export const WIDE_MIN_WIDTH = 1024;

/**
 * 底部固定查询操作区为页面末尾预留的高度（px）。
 *
 * 组成：按钮 48 + 上下内边距 12×2 = 72，其余留给 `env(safe-area-inset-bottom)`
 * 与投影。只在页脚容器上预留一次——`<main>` 用普通 padding，否则同一段空白
 * 会在页面主区与页脚上各让一遍，compact 档底部凭空多出一截。
 */
export const FIXED_QUERY_BAR_RESERVED_PX = 112;

/** 公开查询页的三种布局档位。 */
export type ViewportTier = "compact" | "medium" | "wide";

/** 一档布局的全部派生量；组件只消费这些字段，不在渲染体里重新算断点。 */
export interface PublicLayoutSpec {
  tier: ViewportTier;
  /** 页面栅格列数：1 / 2 / 3。 */
  columns: 1 | 2 | 3;
  /** 筛选区占的列数（始终 1 列，宽屏也不横向拉伸成两列）。 */
  filterSpan: 1;
  /** 结果区占的列数：宽屏占 2 列，与筛选合成三列。 */
  resultSpan: 1 | 2;
  /** 结果列表内部的行列数：宽屏两列，其余单列。 */
  resultColumns: 1 | 2;
  /** 是否把查询按钮固定到视口底部（仅 compact）。 */
  fixedQueryBar: boolean;
  /** 页面内边距（px）：窄屏收窄到 16，留出更多内容宽度。 */
  padding: number;
  /** 页面最大宽度（px）：宽屏放宽到 1440，让三列有足够横向空间。 */
  maxWidth: number;
}

/**
 * 视口宽度 → 布局档位。
 *
 * 边界：599 → compact、600 → medium、1023 → medium、1024 → wide。
 * 非法宽度（NaN、Infinity、负数）按 compact 处理——宁可先给单列，
 * 也不让页面在拿不到真实窗口尺寸时算出 NaN 列宽而整块塌掉。
 */
export function viewportTier(width: number): ViewportTier {
  if (!Number.isFinite(width) || width < COMPACT_MAX_WIDTH) {
    return "compact";
  }
  return width >= WIDE_MIN_WIDTH ? "wide" : "medium";
}

/** 档位 → 布局参数。三档各自一组常量，组件里不再写任何断点数字。 */
export function publicLayout(tier: ViewportTier): PublicLayoutSpec {
  switch (tier) {
    case "compact":
      return {
        tier,
        columns: 1,
        filterSpan: 1,
        resultSpan: 1,
        resultColumns: 1,
        fixedQueryBar: true,
        padding: 16,
        maxWidth: 960,
      };
    case "medium":
      return {
        tier,
        columns: 2,
        filterSpan: 1,
        resultSpan: 1,
        resultColumns: 1,
        fixedQueryBar: false,
        padding: 24,
        maxWidth: 1180,
      };
    case "wide":
      return {
        tier,
        columns: 3,
        filterSpan: 1,
        resultSpan: 2,
        resultColumns: 2,
        fixedQueryBar: false,
        padding: 24,
        maxWidth: 1440,
      };
  }
}
