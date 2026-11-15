import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// 后端默认监听 :18080（本机用 127.0.0.1:18080 访问，见
// backend/internal/config/config.go:27）。开发服务器与 `vite preview` 都要把
// /api 转过去，否则产物预览时接口全 404。
const apiProxy = {
  "/api": {
    target: "http://127.0.0.1:18080",
  },
};

export default defineConfig({
  base: "/",
  plugins: [react()],
  server: {
    proxy: apiProxy,
  },
  preview: {
    proxy: apiProxy,
  },
  build: {
    // 与 Node 24 LTS 对齐；避免为老浏览器降级产出多余的语法转译。
    target: "es2022",
    outDir: "dist",
    sourcemap: false,
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
  },
});
