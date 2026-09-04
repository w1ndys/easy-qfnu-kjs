#!/usr/bin/env node
// 构建前校验已发布的 data/ 快照，不生成源码或部署产物。
// 采集器发布前已完成 JSON Schema 校验；这里复核构建输入的引用、元数据与哈希。
import { createHash } from 'node:crypto';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import path from 'node:path';

const root = path.resolve(process.cwd());
const dataDir = path.join(root, 'data');
const manifestFile = path.join(dataDir, 'manifest.json');

function fail(message) {
  console.error(`validate-data: ${message}`);
  process.exit(1);
}

function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'));
  } catch (error) {
    if (error?.code === 'ENOENT') return null;
    fail(`JSON 解析失败: ${path.relative(root, file)}`);
  }
}

function sha256OfFile(file) {
  return createHash('sha256').update(readFileSync(file)).digest('hex');
}

function weekFileName(week) {
  return `week-${String(week).padStart(2, '0')}.json`;
}

function validateWeek(file, week, term, manifestEntry) {
  const snapshot = readJson(file);
  if (
    !snapshot ||
    manifestEntry.week !== week ||
    snapshot.schema_version !== 1 ||
    snapshot.term !== term ||
    snapshot.week !== week ||
    snapshot.snapshot_id !== manifestEntry.snapshot_id
  ) {
    fail(`周快照与 manifest 不一致: ${path.relative(root, file)}`);
  }
  if (sha256OfFile(file) !== manifestEntry.sha256) {
    fail(`周快照 SHA-256 不一致: ${path.relative(root, file)}`);
  }
}

const manifest = readJson(manifestFile);
if (manifest === null) {
  console.log('validate-data: 未找到 data/manifest.json，跳过');
  process.exit(0);
}
if (
  manifest.schema_version !== 1 ||
  typeof manifest.term !== 'string' ||
  manifest.term === '' ||
  typeof manifest.release_id !== 'string' ||
  typeof manifest.weeks !== 'object' ||
  manifest.weeks === null
) {
  fail('manifest 格式无效');
}

const weeksDir = path.join(dataDir, 'terms', manifest.term, 'weeks');
if (!existsSync(weeksDir)) fail(`周快照目录不存在: ${path.relative(root, weeksDir)}`);

const seen = new Set();
for (const name of readdirSync(weeksDir).sort()) {
  const match = /^week-(\d{1,2})\.json$/.exec(name);
  if (!match) continue;
  const week = Number(match[1]);
  const entry = manifest.weeks[String(week)];
  if (!entry) fail(`周快照未在 manifest 中声明: ${name}`);
  validateWeek(path.join(weeksDir, name), week, manifest.term, entry);
  seen.add(week);
}

for (const key of Object.keys(manifest.weeks)) {
  if (!seen.has(Number(key))) {
    fail(`manifest 声明第 ${key} 周但文件缺失: ${weekFileName(Number(key))}`);
  }
}

console.log(`validate-data: ${seen.size} 周校验通过`);
