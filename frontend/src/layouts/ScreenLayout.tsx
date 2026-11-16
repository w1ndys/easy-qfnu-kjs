// 大屏壳（tasklist 第 7 项；design D6、Requirement 6.1、PRD 6.4）：
// 暗色、无导航、固定 1920×1080 设计稿 + 等比缩放。
//
// 缩放的输入只有窗口尺寸：内层画布宽高恒定，窗口变化只改 transform: scale(...)
// 与居中留边，因此各区块的相对位置与顺序不会随窗口变化（正确性属性 8）。
// 不用 CSS zoom（Firefox/Safari 实现差异大），也不用 vh 重算字号（会破坏内层排版）。

import { ConfigProvider, theme, type ThemeConfig } from "antd";
import { useEffect, useState, type ReactNode } from "react";
import {
  computeScreenGeometry,
  SCREEN_HEIGHT,
  SCREEN_WIDTH,
} from "../lib/scale";
import { brandColor } from "../theme";

/** 大屏主题：暗色算法 + 品牌色；不沿用公开查询页的浅色主题。 */
const screenTheme = {
  algorithm: theme.darkAlgorithm,
  token: { colorPrimary: brandColor },
} satisfies ThemeConfig;

interface ViewportSize {
  width: number;
  height: number;
}

/** 只读窗口尺寸：不测量 DOM，避免 `transform` 改变自身尺寸后测量结果自激。 */
function readViewportSize(): ViewportSize {
  return { width: window.innerWidth, height: window.innerHeight };
}

/** 窗口尺寸：监听 resize 即可，轮询与滚动都不该触发重算。 */
function useViewportSize(): ViewportSize {
  const [size, setSize] = useState<ViewportSize>(readViewportSize);

  useEffect(() => {
    const update = () => setSize(readViewportSize());
    // 挂载后同步一次：首次渲染到 effect 之间窗口可能已经变了。
    update();
    window.addEventListener("resize", update);
    return () => window.removeEventListener("resize", update);
  }, []);

  return size;
}

/** 画布本体：外层负责黑边与裁切，内层是恒定的 1920×1080 设计稿。 */
function ScreenCanvas({ children }: { children: ReactNode }) {
  const viewport = useViewportSize();
  const { scale, offsetX, offsetY } = computeScreenGeometry(
    viewport.width,
    viewport.height,
  );
  const { token } = theme.useToken();

  return (
    <div
      data-testid="screen-viewport"
      style={{
        position: "fixed",
        inset: 0,
        // 非 16:9 的留边用纯黑，投屏时与画布背景区分开。
        background: "#000",
        overflow: "hidden",
      }}
    >
      <div
        data-testid="screen-canvas"
        style={{
          position: "absolute",
          left: offsetX,
          top: offsetY,
          width: SCREEN_WIDTH,
          height: SCREEN_HEIGHT,
          transform: `scale(${scale})`,
          transformOrigin: "top left",
          background: token.colorBgLayout,
          color: token.colorText,
          overflow: "hidden",
        }}
      >
        {children}
      </div>
    </div>
  );
}

export interface ScreenLayoutProps {
  children: ReactNode;
}

/**
 * 大屏布局：暗色主题 + 无导航 + 等比缩放画布。
 * 不渲染任何导航、页脚或返回链接——投屏页只有数据本身（Requirement 6.1）。
 */
export function ScreenLayout({ children }: ScreenLayoutProps) {
  return (
    <ConfigProvider theme={screenTheme}>
      <ScreenCanvas>{children}</ScreenCanvas>
    </ConfigProvider>
  );
}
