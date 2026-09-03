#!/usr/bin/env node
// 生成构建期静态 TypeScript 数据索引（决策文件 §13）：扫描 data/manifest.json 与
// data/terms/<term>/weeks/week-*.json，写出 api-lib/generated-index.ts，导出
// term / releaseId / weeks（{ week: { week, snapshotId, sha256, path } }），供
// Serverless 函数启动时映射“当前学期周次 → 周快照文件”，避免把全部 JSON 静态内联。
//
// 用法：node scripts/generate-data-index.mjs [--root <仓库根>] [--out <输出文件>]
//   --root 默认 process.cwd()；数据目录固定取 <root>/data。
//   --out  默认 <root>/api-lib/generated-index.ts。
// 行为：manifest.json 缺失 → 写空索引并退出 0（构建不因“暂无数据”失败）；
//       manifest 损坏 / schema 版本不受支持 / 周文件与 manifest 不一致 → 非零退出。
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';

function parseArgs(argv) {
  let root = path.resolve(process.cwd());
  let out = null;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg === '--root') root = path.resolve(argv[++i]);
    else if (arg === '--out') out = path.resolve(argv[++i]);
    else {
      console.error(`未知参数: ${arg}`);
      process.exit(2);
    }
  }
  if (out === null) out = path.join(root, 'api-lib', 'generated-index.ts');
  return { root, out };
}

function fail(message) {
  console.error(`generate-data-index: ${message}`);
  process.exit(1);
}

function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch (err) {
    const code = err?.code;
    if (code === 'ENOENT') return null;
    throw err;
  }
}

function sha256OfFile(file) {
  return createHash('sha256').update(readFileSync(file)).digest('hex');
}

function render(term, releaseId, weeks) {
  const lines = [
    '// 由 scripts/generate-data-index.mjs 自动生成 —— 请勿手工编辑。',
    '// 数据源：data/manifest.json + data/terms/<term>/weeks/week-*.json。',
    '// 每项映射一份已发布周快照：sha256 供运行时校验文件与索引一致；path 相对仓库根。',
    '',
  ];
  if (term === '') {
    lines.push('export const term = "";');
    lines.push('export const releaseId = "";');
    lines.push('export const weeks: Record<string, { week: number; snapshotId: string; sha256: string; path: string }> = {};');
    lines.push('');
    return lines.join('\n');
  }
  lines.push(`export const term = ${JSON.stringify(term)};`);
  lines.push(`export const releaseId = ${JSON.stringify(releaseId)};`);
  lines.push('export const weeks: Record<string, { week: number; snapshotId: string; sha256: string; path: string }> = {');
  for (const entry of Object.values(weeks)) {
    lines.push(`  ${JSON.stringify(String(entry.week))}: { week: ${entry.week}, snapshotId: ${JSON.stringify(entry.snapshotId)}, sha256: ${JSON.stringify(entry.sha256)}, path: ${JSON.stringify(entry.path)} },`);
  }
  lines.push('};');
  lines.push('');
  return lines.join('\n');
}

const { root, out } = parseArgs(process.argv.slice(2));
const dataDir = path.join(root, 'data');
const manifestFile = path.join(dataDir, 'manifest.json');
const manifest = readJson(manifestFile);
if (manifest === null) {
  writeFileSync(out, render('', '', {}));
  console.log(`generate-data-index: 未找到 data/manifest.json，已生成空索引 -> ${path.relative(root, out)}`);
  process.exit(0);
}
if (typeof manifest !== 'object' || manifest === null || manifest.schema_version !== 1) {
  fail('manifest schema_version 不受支持或格式无效');
}
const { term, release_id: releaseId, weeks: manifestWeeks } = manifest;
if (typeof term !== 'string' || term === '') fail('manifest 缺少 term');
if (typeof releaseId !== 'string') fail('manifest 缺少 release_id');
if (typeof manifestWeeks !== 'object' || manifestWeeks === null) fail('manifest 缺少 weeks');

const weeksDir = path.join(dataDir, 'terms', term, 'weeks');
if (!existsSync(weeksDir)) {
  // 学期目录不存在视为暂无该学期数据（空索引，退出 0），避免构建失败。
  writeFileSync(out, render('', '', {}));
  console.log(`generate-data-index: 未找到 ${path.relative(root, weeksDir)}，已生成空索引 -> ${path.relative(root, out)}`);
  process.exit(0);
}

const index = new Map(); // week -> { week, snapshotId, sha256, path }
const seen = new Set();
for (const name of readdirSync(weeksDir).sort()) {
  const m = /^week-(\d{1,2})\.json$/.exec(name);
  if (!m) continue;
  const week = Number(m[1]);
  seen.add(week);
  const file = path.join(weeksDir, name);
  let snapshot;
  try {
    snapshot = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    fail(`周快照解析失败: ${path.relative(root, file)}`);
  }
  const manifestEntry = manifestWeeks[String(week)];
  if (
    !manifestEntry ||
    manifestEntry.week !== week ||
    manifestEntry.snapshot_id !== snapshot.snapshot_id ||
    snapshot.week !== week ||
    snapshot.term !== term ||
    snapshot.schema_version !== 1
  ) {
    fail(`周快照与 manifest 不一致: ${path.relative(root, file)}`);
  }
  index.set(week, {
    week,
    snapshotId: snapshot.snapshot_id,
    sha256: sha256OfFile(file),
    path: path.posix.join('data', 'terms', term, 'weeks', name),
  });
}
// manifest 声明已发布但文件缺失 → 数据不完整，直接失败（构建期即暴露，而非运行时）。
for (const key of Object.keys(manifestWeeks)) {
  if (!seen.has(Number(key))) {
    fail(`manifest 声明第 ${key} 周已发布，但缺少 ${path.posix.join('data', 'terms', term, 'weeks', `week-${String(key).padStart(2, '0')}.json`)}`);
  }
}
// manifest 声明周次与文件周次不一致：文件存在但 manifest 未声明 → 数据不完整。
for (const week of seen) {
  if (!manifestWeeks[String(week)]) {
    fail(`存在未在 manifest 中声明的周快照: week-${String(week).padStart(2, '0')}.json`);
  }
}

const sorted = [...index.values()].sort((a, b) => a.week - b.week);
writeFileSync(out, render(term, releaseId, sorted));
console.log(`generate-data-index: ${sorted.length} 周已写入索引 -> ${path.relative(root, out)}`);
