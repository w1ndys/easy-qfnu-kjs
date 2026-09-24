import { fileURLToPath, URL } from 'node:url'
import { defineConfig, loadEnv } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import { VantResolver } from '@vant/auto-import-resolver'

// Vite 配置：/ 根路径、Vant 按需引入、/api 开发代理。
// 代理目标由环境变量 VITE_API_TARGET 决定，未设置时默认指向线上 API 域名。
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, fileURLToPath(new URL('.', import.meta.url)))
  const apiTarget = env.VITE_API_TARGET || 'https://easy-qfnu-kjs.vercel.app'

  return {
    base: '/',
    plugins: [
      vue(),
      Components({
        resolvers: [VantResolver()],
        dts: 'src/components.d.ts',
      }),
    ],
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    server: {
      port: 5173,
      host: true,
      proxy: {
        '/api': {
          target: apiTarget,
          changeOrigin: true,
        },
      },
    },
    build: {
      outDir: 'dist',
      assetsDir: 'assets',
      emptyOutDir: true,
    },
  }
})
