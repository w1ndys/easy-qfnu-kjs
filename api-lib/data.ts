// 构建期数据索引 + 模块级懒加载与内存缓存（决策文件 §8 / §13 / Q145）。
//
// 数据契约：
//   - 构建时由 scripts/generate-data-index.mjs 扫描 data/manifest.json 与
//     data/terms/<term>/weeks/week-*.json，生成 api-lib/generated-index.ts（term/releaseId/weeks，
//     每项 { week, snapshotId, sha256, path }）；
//   - 函数包通过 vercel.json 的 functions.includeFiles 携带 data/**；
//   - 运行时用 fs.readFileSync 按索引懒加载需要的 JSON，不做静态内联，也不在请求时访问 GitHub。
//
// 数据根目录解析（按优先级）：
//   1. 环境变量 EASY_KJS_ROOT（本地开发 / 测试指向 fixture 根目录）；
//   2. 自本文件所在目录向上查找包含 data/ 的目录（覆盖 Vercel 打包根与源码布局）。
// 索引解析：测试可用 EASY_KJS_INDEX_JSON 指向形状与 generated-index.ts 相同的 JSON 文件；
// 生产环境直接使用 generated-index.ts 的静态导出。
import { createHash } from 'node:crypto';
import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { ApiError, CODES } from './errors.ts';
import {
  releaseId as staticReleaseId,
  term as staticTerm,
  weeks as staticWeeks,
} from './generated-index.ts';
import type { DataIndex, Manifest, SnapshotWeekFile } from './types.ts';

interface DataCache {
  root: string;
  indexKey: string;
  index: DataIndex;
  metaLoaded: boolean;
  manifest: Manifest | null;
  weeks: Map<string, SnapshotWeekFile | null>;
}

const caches = new Map<string, DataCache>();

const HERE = path.dirname(fileURLToPath(import.meta.url));

function resolveRoot(): string {
  const envRoot = process.env.EASY_KJS_ROOT;
  if (envRoot) return path.resolve(envRoot);
  // 向上查找数据根（源码布局：api-lib -> .. = 仓库根；Vercel 打包同样保留 data/ 相对位置）。
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
  return override ? path.resolve(override) : 'static';
}

function readIndexFromEnv(file: string): DataIndex {
  let raw: unknown;
  try {
    raw = JSON.parse(readFileSync(file, 'utf8'));
  } catch {
    throw new ApiError(503, CODES.internal, '数据索引文件解析失败');
  }
  const idx = raw as Partial<DataIndex>;
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
  return idx as DataIndex;
}

function cache(): DataCache {
  const root = resolveRoot();
  const indexKey = indexSourceKey();
  const key = `${root}\u0000${indexKey}`;
  let c = caches.get(key);
  if (!c) {
    c = {
      root,
      indexKey,
      index: indexKey === 'static'
        ? { term: staticTerm, releaseId: staticReleaseId, weeks: staticWeeks }
        : readIndexFromEnv(indexKey),
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
 * 返回 null 表示当前没有发布任何快照数据（文件缺失 → 对外表现为 503 no_snapshot）。
 * 文件损坏 / schema 版本不受支持 / 与数据索引不一致 → 503 internal。
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
  if (c.index.term && c.index.term !== parsed.term) {
    throw internal('构建期数据索引与 manifest 学期不一致');
  }
  c.manifest = parsed;
  return parsed;
}

/**
 * 懒加载并缓存指定教学周的周快照文件。
 * 返回 null 表示该周未发布 / 文件缺失（对外表现为 404 not_collected）。
 * 文件内容 SHA-256 与索引不一致、解析失败或标识不一致 → 503 internal。
 */
export function getWeekFile(week: number): SnapshotWeekFile | null {
  const c = cache();
  if (!c.metaLoaded) {
    // 先确定 manifest 是否存在；无数据时上层会直接 503。
    getManifest();
  }
  const key = String(week);
  if (c.weeks.has(key)) return c.weeks.get(key) ?? null;

  const entry = c.index.weeks[key];
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
    throw internal(`第 ${week} 周快照 SHA-256 与索引不一致`);
  }

  let file: SnapshotWeekFile;
  try {
    file = JSON.parse(bytes.toString('utf8')) as SnapshotWeekFile;
  } catch {
    throw internal(`第 ${week} 周快照 JSON 解析失败`);
  }
  if (file.schema_version !== 1 || file.week !== week || file.snapshot_id !== entry.snapshotId) {
    throw internal(`第 ${week} 周快照元数据与索引不一致`);
  }
  const manifest = c.manifest;
  if (manifest && file.term !== manifest.term) {
    throw internal(`第 ${week} 周快照学期与 manifest 不一致`);
  }
  c.weeks.set(key, file);
  return file;
}
