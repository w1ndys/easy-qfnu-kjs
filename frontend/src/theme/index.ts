import type { ThemeConfig } from "antd";

// docs/decisions/2026-10-03-frontend-react-antd.md：品牌色 #884F22 作为 antd token 落地。
// 大屏的 darkAlgorithm 由第 7 项在 ScreenLayout 里叠加，本项只落品牌 token。
export const brandColor = "#884F22";

export const appTheme = {
  token: {
    colorPrimary: brandColor,
  },
} satisfies ThemeConfig;
