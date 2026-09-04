// 数据快照懒加载与内存缓存（决策文件 §8 / §13 / Q145）。
//
// 生产运行时以 data/manifest.json 为唯一索引：周文件路径按固定命名规则推导，
// manifest 中的 snapshot_id 与 sha256 负责校验文件身份和内容完整性。
// 测试可通过 EASY_KJS_INDEX_JSON 注入索引覆盖文件，模拟损坏或不一致场景；
// 生产环境不依赖任何构建期生成的源码文件。
import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { ApiError, CODES } from './errors.js';
import type { Manifest, SnapshotWeekFile } from './types.js';

interface IndexWeekEntry {
  week: number;
  snapshotId: string;
  sha256: string;
  path: string;
}

interface TestIndex {
  term: string;
  releaseId: string;
  weeks: Record<string, IndexWeekEntry>;
}

interface DataCache {
  root: string;
  testIndex: TestIndex | null;
  metaLoaded: boolean;
  manifest: Manifest | null;
  weeks: Map<string, SnapshotWeekFile | null>;
}

const caches = new Map<string, DataCache>();
const HERE = path.dirname(fileURLToPath(import.meta.url));

function resolveRoot(): string {
  const envRoot = process.env.EASY_KJS_ROOT;
  if (envRoot) return path.resolve(envRoot);

  // 向上查找数据根，兼容源码布局与 Vercel 函数打包布局。
  let dir = HERE;
  for (;;) {
    if (existsSync(path.join(dir, 'data'))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return path.resolve(process.cwd());
}

function indexSourceKey(): string {
  const override = process.env.EASY_KJS_INDEX_JSON;
  return override ? path.resolve(override) : '';
}

function readIndexFromEnv(file: string): TestIndex {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    throw new ApiError(503, CODES.internal, '数据索引文件解析失败');
  }
  const idx = raw as Partial<TestIndex>;
  if (
    typeof idx !== 'object' ||
    idx === null ||
    typeof idx.term !== 'string' ||
    typeof idx.releaseId !== 'string' ||
    typeof idx.weeks !== 'object' ||
    idx.weeks === null
  ) {
    throw new ApiError(503, CODES.internal, '数据索引文件格式无效');
  }
  return idx as TestIndex;
}

function cache(): DataCache {
  const root = resolveRoot();
  const indexKey = indexSourceKey();
  const key = `${root}\u0000${indexKey || 'manifest'}`;
  let c = caches.get(key);
  if (!c) {
    c = {
      root,
      testIndex: indexKey ? readIndexFromEnv(indexKey) : null,
      metaLoaded: false,
      manifest: null,
      weeks: new Map(),
    };
    caches.set(key, c);
  }
  return c;
}

function internal(detail: string): ApiError {
  return new ApiError(503, CODES.internal, detail);
}

/**
 * 模块级懒加载 data/manifest.json。
 * 文件缺失对外表现为 503 no_snapshot；损坏或测试索引学期不一致则返回 internal。
 */
export function getManifest(): Manifest | null {
  const c = cache();
  if (c.metaLoaded) return c.manifest;
  c.metaLoaded = true;

  let raw: string;
  try {
    raw = readFileSync(path.join(c.root, 'data', 'manifest.json'), 'utf8');
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === 'ENOENT') {
      c.manifest = null;
      return null;
    }
    throw internal('manifest.json 读取失败');
  }

  let parsed: Manifest;
  try {
    parsed = JSON.parse(raw) as Manifest;
  } catch {
    throw internal('manifest.json 解析失败');
  }
  if (parsed.schema_version !== 1) {
    throw internal('manifest schema_version 不受当前 API 支持');
  }
  if (c.testIndex && c.testIndex.term && c.testIndex.term !== parsed.term) {
    throw internal('测试数据索引与 manifest 学期不一致');
  }
  c.manifest = parsed;
  return parsed;
}

function manifestWeekEntry(manifest: Manifest, week: number): IndexWeekEntry | null {
  const entry = manifest.weeks[String(week)];
  if (!entry) return null;
  return {
    week: entry.week,
    snapshotId: entry.snapshot_id,
    sha256: entry.sha256,
    path: path.join(
      'data',
      'terms',
      manifest.term,
      'weeks',
      `week-${String(week).padStart(2, '0')}.json`,
    ),
  };
}

function selectedWeekEntry(
  c: DataCache,
  manifest: Manifest,
  week: number,
): IndexWeekEntry | null {
  if (c.testIndex) return c.testIndex.weeks[String(week)] ?? null;
  return manifestWeekEntry(manifest, week);
}

/**
 * 懒加载并缓存指定教学周的周快照文件。
 * 路径由 manifest.term 和固定 week-NN 命名规则推导，内容按 manifest.sha256 校验。
 */
export function getWeekFile(week: number): SnapshotWeekFile | null {
  const c = cache();
  if (!c.metaLoaded) getManifest();
  const manifest = c.manifest;
  if (!manifest) return null;

  const key = String(week);
  if (c.weeks.has(key)) return c.weeks.get(key) ?? null;

  const entry = selectedWeekEntry(c, manifest, week);
  if (!entry) {
    c.weeks.set(key, null);
    return null;
  }

  const filePath = path.join(c.root, entry.path);
  let bytes: Buffer;
  try {
    bytes = readFileSync(filePath);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === 'ENOENT') {
      c.weeks.set(key, null);
      return null;
    }
    throw internal(`第 ${week} 周快照读取失败`);
  }

  const digest = createHash('sha256').update(bytes).digest('hex');
  if (digest !== entry.sha256) {
    throw internal(`第 ${week} 周快照 SHA-256 与 manifest 不一致`);
  }

  let file: SnapshotWeekFile;
  try {
    file = JSON.parse(bytes.toString('utf8')) as SnapshotWeekFile;
  } catch {
    throw internal(`第 ${week} 周快照 JSON 解析失败`);
  }
  if (file.schema_version !== 1 || file.week !== week || file.snapshot_id !== entry.snapshotId) {
    throw internal(`第 ${week} 周快照元数据与 manifest 不一致`);
  }
  if (file.term !== manifest.term) {
    throw internal(`第 ${week} 周快照学期与 manifest 不一致`);
  }
  c.weeks.set(key, file);
  return file;
}
