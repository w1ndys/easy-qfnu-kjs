import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";
import { installMatchMedia, resetMediaQueries } from "./matchMedia";
import { server } from "./msw";

// vitest 未开 globals，RTL 的自动 cleanup 不会注册，这里显式清理。
afterEach(() => {
  cleanup();
});

// jsdom 不实现 matchMedia：antd 的响应式组件与 useReducedMotion 都依赖它。
// 第 10 项把它换成可控垫片，用例能打开/关闭 prefers-reduced-motion。
installMatchMedia();

// 公开接口默认走 MSW（src/test/msw.ts）：四个端点的 handler 由与后端类型
// 逐字段对齐的夹具驱动。需要特殊响应（挂起、限流、业务码失败）的用例仍可
// 在自身范围内换掉 fetch；未登记的请求一律报错，避免测试悄悄打到真实网络。
beforeAll(() => {
  server.listen({ onUnhandledRequest: "error" });
});

afterEach(() => {
  server.resetHandlers();
  resetMediaQueries();
});

afterAll(() => {
  server.close();
});

// jsdom 也没有 ResizeObserver：antd 的若干组件在挂载时会用到，缺了直接抛错。
if (
  typeof window !== "undefined" &&
  typeof globalThis.ResizeObserver !== "function"
) {
  class ResizeObserverStub implements ResizeObserver {
    observe() {}
    unobserve() {}
    disconnect() {}
  }

  globalThis.ResizeObserver = ResizeObserverStub;
}
