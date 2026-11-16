import type { ThemeConfig } from "antd";

// docs/decisions/2026-10-03-frontend-react-antd.md：品牌色 #884F22 作为 antd token 落地。
// 大屏的 darkAlgorithm 由第 7 项在 ScreenLayout 里叠加，本项只落品牌 token。
export const brandColor = "#884F22";

/**
 * 触控目标下限（px）：Requirement 8.4 要求可点击控件不小于 44×44。
 * 放到 antd 的 `controlHeight` 上，Button / Input / Select 等控件由组件库自己
 * 抬到 44，不在每个调用点各写一次；日期项与分页项再由全局 CSS 补齐
 * （见 src/styles/global.css）。
 */
export const TOUCH_TARGET_SIZE = 44;

export const appTheme = {
  token: {
    colorPrimary: brandColor,
    controlHeight: TOUCH_TARGET_SIZE,
  },
} satisfies ThemeConfig;
