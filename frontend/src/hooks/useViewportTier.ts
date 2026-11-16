// 视口档位（tasklist 第 10 项；design「响应式断点」）。
//
// 为什么用 `resize` + `innerWidth` 而不是在组件里各写一份媒体查询：
// 三档布局要在 JS 里决定「筛选占几列、结果占几列、查询按钮是否固定」，
// 只有一处读宽度才能保证这三件事永远同一档；纯 CSS 媒体查询没法让
// 「固定按钮只出现在 compact」这样的结构差异成立。
//
// 读宽度只在事件回调与挂载后 effect 里发生，渲染期不读时钟/尺寸之外的
// 不稳定值；首帧用同步读到的宽度，避免先按桌面渲染再跳成单列。

import { useEffect, useState } from "react";
import { viewportTier, WIDE_MIN_WIDTH, type ViewportTier } from "../lib/layout";

/** 没有 window 时（测试/极端环境）按宽屏处理：宽屏布局只是并列，不会缺内容。 */
function readViewportWidth(): number {
  return typeof window === "undefined" ? WIDE_MIN_WIDTH : window.innerWidth;
}

/**
 * 当前视口档位。窗口尺寸变化（含旋转、缩放）时重算；监听只挂一个 resize，
 * 滚动与轮询都不该触发重排。
 */
export function useViewportTier(): ViewportTier {
  const [tier, setTier] = useState<ViewportTier>(() =>
    viewportTier(readViewportWidth()),
  );

  useEffect(() => {
    const update = () => setTier(viewportTier(readViewportWidth()));
    // 挂载后同步一次：首帧渲染到 effect 之间窗口可能已经变了。
    update();
    window.addEventListener("resize", update);
    return () => window.removeEventListener("resize", update);
  }, []);

  return tier;
}
