// 仅供读屏的样式（公开页的 aria-live 播报区共用）：
// 播报区不占视觉位置，也不影响布局——两个页面各抄一份样式容易漂移，集中在这里。

import type { CSSProperties } from "react";

export const VISUALLY_HIDDEN: CSSProperties = {
  position: "absolute",
  width: 1,
  height: 1,
  padding: 0,
  margin: -1,
  overflow: "hidden",
  clip: "rect(0, 0, 0, 0)",
  whiteSpace: "nowrap",
  border: 0,
};
