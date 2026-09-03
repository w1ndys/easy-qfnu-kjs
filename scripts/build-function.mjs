// 构建期把 Serverless 函数入口与 api-lib 打成单文件 ESM bundle，
// 输出到 api/v1/[...path].js（api/ 下唯一函数文件，规避 Hobby 函数数量上限
// 且不依赖 Vercel 对跨目录 import 的打包）。
import { build } from 'esbuild'

const out = 'api/v1/[...path].js'

await build({
  entryPoints: ['function-src/v1/[...path].ts'],
  bundle: true,
  platform: 'node',
  target: 'node20',
  format: 'esm',
  outfile: out,
  logLevel: 'info',
})

console.log(`built ${out}`)
