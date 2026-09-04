// 集成测试公共工具：通过 Vercel `fetch` Web Standard 入口调用 handler。
import { fileURLToPath } from 'node:url';
import path from 'node:path';

import handler from '../../api/v1/[...path].js';

export const FIXTURES = fileURLToPath(new URL('./fixtures', import.meta.url));

/** 指向某个 fixture 根目录；indexFile 仅用于覆盖测试索引以模拟完整性错误。 */
export function useFixture(fixtureDir: string, nowIso: string, indexFile?: string): void {
  process.env.EASY_KJS_ROOT = path.join(FIXTURES, fixtureDir);
  if (indexFile) {
    process.env.EASY_KJS_INDEX_JSON = path.join(FIXTURES, indexFile);
  } else {
    delete process.env.EASY_KJS_INDEX_JSON;
  }
  process.env.EASY_KJS_NOW = nowIso;
}

export function clearFixture(): void {
  delete process.env.EASY_KJS_ROOT;
  delete process.env.EASY_KJS_INDEX_JSON;
  delete process.env.EASY_KJS_NOW;
}

export async function call(
  method: string,
  pathAndQuery: string,
): Promise<Response> {
  return handler.fetch(new Request(`https://api.example.test${pathAndQuery}`, { method }));
}

export async function get(pathAndQuery: string): Promise<Response> {
  return call('GET', pathAndQuery);
}

export async function problemOf(res: Response): Promise<Record<string, unknown>> {
  return (await res.json()) as Record<string, unknown>;
}
