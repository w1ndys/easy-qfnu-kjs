// 减少动态效果（tasklist 第 10 项；Requirement 8 的「prefers-reduced-motion 关闭过渡」）。
//
// 系统开启「减少动态效果」时：
// - 这里把 antd 的 `token.motion` 关掉（过渡与动画由 antd 自己收敛）；
// - `src/styles/global.css` 再用 `@media (prefers-reduced-motion: reduce)` 兜底，
//   覆盖 CSS transition / animation（含 antd 之外的自定义样式）。
//
// 两处都做是因为 antd 的 motion token 只管它自己的组件动画：页面一旦加了
// 自定义过渡，仅有 token 不够；而只有 CSS 兜底时，antd 的部分 JS 驱动的动效
// 也不会被关掉。用 matchMedia 读设置而不是把值写死，用户改系统设置后无需刷新。

import { useEffect, useState } from "react";

/** 系统「减少动态效果」偏好的媒体查询。 */
export const REDUCED_MOTION_QUERY = "(prefers-reduced-motion: reduce)";

/** 读一次系统偏好；没有 matchMedia（极老环境/测试垫片缺失）时按「不减少」处理。 */
function readReducedMotion(): boolean {
  if (
    typeof window === "undefined" ||
    typeof window.matchMedia !== "function"
  ) {
    return false;
  }
  return window.matchMedia(REDUCED_MOTION_QUERY).matches;
}

/** 系统是否要求减少动态效果；设置变化时实时跟随。 */
export function useReducedMotion(): boolean {
  const [reduced, setReduced] = useState(readReducedMotion);

  useEffect(() => {
    if (typeof window.matchMedia !== "function") {
      return;
    }

    const query = window.matchMedia(REDUCED_MOTION_QUERY);
    const update = () => setReduced(query.matches);

    // 挂载后同步一次：首帧到 effect 之间偏好可能已经变了。
    update();

    // Safari 14 之前只有 addListener；优先用标准接口，缺失时回退。
    if (typeof query.addEventListener === "function") {
      query.addEventListener("change", update);
      return () => query.removeEventListener("change", update);
    }
    query.addListener(update);
    return () => query.removeListener(update);
  }, []);

  return reduced;
}
