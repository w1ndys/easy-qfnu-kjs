import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { appTheme, TOUCH_TARGET_SIZE } from "../theme";

// 触控目标与减少动态效果的交付物守卫（tasklist 第 10 项；Requirement 8.4）。
//
// jsdom 不算布局，也不加载 imported CSS，所以这两条只能钉在「交付的 CSS 与
// antd token 本身」上：token 决定 Button/Input/Select 的高度，CSS 补齐 antd
// 用固定高度画出来的日期项与分页项，媒体查询负责关闭过渡。
// 直接读源文件而不是 `?raw` 导入：Vitest 会把 CSS 导入替换为空模块。
// 路径由本文件的位置算出来（import.meta.dirname），不依赖 vitest 的 cwd：
// 从仓库根还是 frontend/ 起跑都指向同一个同级 global.css。
// 注意不要写 `new URL("./global.css", import.meta.url)`：Vite 会把这种写法
// 当资源引用改写成 dev server 的 http URL，fileURLToPath 会因此报 scheme 错。
const globalCss = readFileSync(
  join(import.meta.dirname, "global.css"),
  "utf8",
);

describe("触控目标 ≥44×44", () => {
  it("antd 的控件高度由主题 token 统一抬到 44", () => {
    expect(TOUCH_TARGET_SIZE).toBe(44);
    expect(appTheme.token?.controlHeight).toBe(TOUCH_TARGET_SIZE);
  });

  it("日期项与分页项在全局 CSS 里被抬到 44×44", () => {
    expect(globalCss).toContain(".public-filter .ant-radio-button-wrapper");
    expect(globalCss).toContain(".public-results .ant-pagination-item");
    expect(globalCss.match(/min-height: 44px/g)?.length).toBeGreaterThanOrEqual(
      2,
    );
    expect(globalCss).toContain("min-width: 44px");
  });
});

describe("prefers-reduced-motion", () => {
  it("在系统要求减少动态效果时关闭过渡与动画", () => {
    expect(globalCss).toContain("@media (prefers-reduced-motion: reduce)");
    expect(globalCss).toContain("transition-duration: 0.01ms !important");
    expect(globalCss).toContain("animation-duration: 0.01ms !important");
    expect(globalCss).toContain("scroll-behavior: auto !important");
  });
});
