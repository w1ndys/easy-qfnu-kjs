import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// vitest 未开 globals，RTL 的自动 cleanup 不会注册，这里显式清理。
afterEach(() => {
  cleanup();
});

// jsdom 不实现 matchMedia 与 ResizeObserver：antd 的响应式组件、
// 以及第 7 项大屏缩放都要用到，缺了会在挂载时直接抛错。
if (typeof window !== "undefined") {
  if (typeof window.matchMedia !== "function") {
    Object.defineProperty(window, "matchMedia", {
      writable: true,
      value: (query: string) =>
        ({
          matches: false,
          media: query,
          onchange: null,
          addListener: () => {},
          removeListener: () => {},
          addEventListener: () => {},
          removeEventListener: () => {},
          dispatchEvent: () => false,
        }) as MediaQueryList,
    });
  }

  if (typeof globalThis.ResizeObserver !== "function") {
    class ResizeObserverStub implements ResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    }

    globalThis.ResizeObserver = ResizeObserverStub;
  }
}
