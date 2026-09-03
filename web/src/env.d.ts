/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 开发服务器 /api 代理目标，例如 https://easy-qfnu-kjs.vercel.app */
  readonly VITE_API_TARGET?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
