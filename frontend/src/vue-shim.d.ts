/* eslint-disable @typescript-eslint/no-explicit-any */
// tsc 不解析 .vue 内部结构（模板类型检查由 vue-tsc 负责），这里仅让模块解析通过。
declare module '*.vue' {
  import type { Component } from 'vue'
  const component: Component
  export default component
}
