// 构建期先从 data/ 生成静态索引，再把 Serverless 函数入口与 api-lib 打成单文件 ESM bundle。
// 输出到 api/v1/[...path].js（api/ 下唯一函数文件），规避 Hobby 函数数量上限，
// 并保证每次数据提交触发的 Vercel 构建都嵌入当前周快照索引。
import { execFileSync } from 'node:child_process'
import { build } from 'esbuild'

execFileSync(process.execPath, ['scripts/generate-data-index.mjs'], { stdio: 'inherit' })

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
