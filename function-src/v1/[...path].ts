// Vercel Serverless 单函数（Node.js Runtime）：承载 /api/v1/* 四个只读 GET 资源路由
//（决策文件 §8 / §11）。打包共享同一份当前学期数据索引（vercel.json functions.includeFiles = data/**）。
//
//   GET /api/v1/manifest            公开数据清单（分组/节点/状态字典/逐周新鲜度）
//   GET /api/v1/context             按 Asia/Shanghai 与 anchor 计算的当前日期/星期/教学周
//   GET /api/v1/empty-classrooms    空教室查询
//   GET /api/v1/full-day-status     全天状态查询
//
// 未知路径 → 404；非 GET → 405；错误一律 RFC 9457 application/problem+json（附稳定扩展 code）。
import { roomFreeInRange } from '../../api-lib/availability.ts';
import { getManifest, getWeekFile } from '../../api-lib/data.ts';
import { ApiError, CODES, errorToResponse, problemResponse } from '../../api-lib/errors.ts';
import { keywordMatchesName } from '../../api-lib/keyword.ts';
import { parseNodeSpan, parseQueryBase, type QueryBase } from '../../api-lib/params.ts';
import { computeContext, requestNow, type ContextResult } from '../../api-lib/time.ts';
import type { Manifest, SnapshotRoom, SnapshotWeekFile } from '../../api-lib/types.ts';
import {
  buildFullDayList,
  buildManifestView,
  buildRoomList,
  buildSnapshotView,
} from '../../api-lib/views.ts';

// 数据接口成功响应缓存（Q96）：CDN 300s，回源后 600s 内可继续用旧副本。
const CACHE_DATA = 'public, s-maxage=300, stale-while-revalidate=600';
// 日期 context 最多缓存 60 秒。
const CACHE_CONTEXT = 'public, s-maxage=60';

const KNOWN_RESOURCES: Record<string, true> = {
  manifest: true,
  context: true,
  'empty-classrooms': true,
  'full-day-status': true,
};

function json(data: unknown, cacheControl: string): Response {
  return new Response(JSON.stringify(data), {
    status: 200,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': cacheControl,
    },
  });
}

function handleManifest(): Response {
  const manifest = getManifest(); // 无数据/损坏分别以 503/异常处理
  if (!manifest) {
    return problemResponse(503, CODES.noSnapshot, '尚未发布任何快照数据', '/api/v1/manifest');
  }
  const now = requestNow();
  const ctx = computeContext(manifest.term, manifest.anchor, new Date(now));
  return json(buildManifestView(manifest, ctx.week, now), CACHE_DATA);
}

function handleContext(): Response {
  const manifest = getManifest();
  if (!manifest) {
    return problemResponse(503, CODES.noSnapshot, '尚未发布任何快照数据', '/api/v1/context');
  }
  const ctx = computeContext(manifest.term, manifest.anchor, new Date(requestNow()));
  return json(
    {
      date: ctx.date,
      weekday: ctx.weekday,
      week: ctx.week,
      term: ctx.term,
      in_teaching_calendar: ctx.in_teaching_calendar,
    },
    CACHE_CONTEXT,
  );
}

interface LoadedDay {
  manifestEntry: Manifest['weeks'][string];
  weekFile: SnapshotWeekFile;
  rooms: SnapshotRoom[];
}

/** 解析查询基线并载入所选周/日快照；未收录周 / 缺失文件 / 缺失 day → 404 not_collected。 */
function loadDayRooms(manifest: Manifest, base: QueryBase): LoadedDay {
  const manifestEntry = manifest.weeks[String(base.week)];
  if (!manifestEntry) {
    throw new ApiError(404, CODES.notCollected, `第 ${base.week} 周尚未收录`);
  }
  const weekFile = getWeekFile(base.week);
  if (!weekFile) {
    throw new ApiError(404, CODES.notCollected, `第 ${base.week} 周快照文件缺失`);
  }
  const dayRooms = weekFile.days[String(base.day)];
  if (!dayRooms) {
    throw new ApiError(
      404,
      CODES.notCollected,
      `第 ${base.week} 周星期 ${base.day} 未收录`,
    );
  }
  return {
    manifestEntry,
    weekFile,
    rooms: dayRooms.rooms.filter((r) => r.group_id === base.groupId),
  };
}

function handleEmptyClassrooms(
  manifest: Manifest,
  ctx: ContextResult,
  searchParams: URLSearchParams,
): Response {
  const now = requestNow();
  const base = parseQueryBase(searchParams, manifest, ctx);
  const { startNode, endNode } = parseNodeSpan(searchParams);

  const { manifestEntry, weekFile, rooms } = loadDayRooms(manifest, base);
  const matched = rooms
    .filter((r) => base.keyword === null || keywordMatchesName(r.name, base.keyword))
    .filter((r) => roomFreeInRange(r.statuses, Number(startNode), Number(endNode)));
  const roomList = buildRoomList(matched);

  return json(
    {
      query: {
        group_id: base.groupId,
        keyword: base.keyword,
        week: base.week,
        day: base.day,
        start_node: startNode,
        end_node: endNode,
      },
      rooms: roomList,
      count: roomList.length,
      snapshot: buildSnapshotView(manifestEntry, weekFile, ctx.week, now),
    },
    CACHE_DATA,
  );
}

function handleFullDayStatus(
  manifest: Manifest,
  ctx: ContextResult,
  searchParams: URLSearchParams,
): Response {
  const now = requestNow();
  const base = parseQueryBase(searchParams, manifest, ctx);

  const { manifestEntry, weekFile, rooms } = loadDayRooms(manifest, base);
  const matched = rooms.filter(
    (r) => base.keyword === null || keywordMatchesName(r.name, base.keyword),
  );
  const roomList = buildFullDayList(matched);

  return json(
    {
      query: {
        group_id: base.groupId,
        keyword: base.keyword,
        week: base.week,
        day: base.day,
      },
      rooms: roomList,
      snapshot: buildSnapshotView(manifestEntry, weekFile, ctx.week, now),
    },
    CACHE_DATA,
  );
}

function handle(req: Request): Response {
  const url = new URL(req.url);
  const segments = url.pathname.split('/').filter(Boolean); // ['api','v1',resource]
  const instance = url.pathname + url.search;

  if (segments.length !== 3 || segments[0] !== 'api' || segments[1] !== 'v1') {
    return problemResponse(404, CODES.notFound, '未知的 API 路径', instance);
  }
  const resource = segments[2]!;
  if (!KNOWN_RESOURCES[resource]) {
    return problemResponse(404, CODES.notFound, `未知资源 ${resource}`, instance);
  }
  if (req.method !== 'GET') {
    return problemResponse(405, CODES.methodNotAllowed, '该资源仅支持 GET', instance, {
      Allow: 'GET',
    });
  }

  try {
    const manifest = getManifest(); // ApiError（数据损坏/索引不一致）直接向上抛
    const ctx = manifest
      ? computeContext(manifest.term, manifest.anchor, new Date(requestNow()))
      : null;
    switch (resource) {
      case 'manifest':
        return handleManifest();
      case 'context':
        return handleContext();
      case 'empty-classrooms': {
        if (!manifest || !ctx) {
          return problemResponse(
            503,
            CODES.noSnapshot,
            '尚未发布任何快照数据，无法查询',
            instance,
          );
        }
        return handleEmptyClassrooms(manifest, ctx, url.searchParams);
      }
      case 'full-day-status': {
        if (!manifest || !ctx) {
          return problemResponse(
            503,
            CODES.noSnapshot,
            '尚未发布任何快照数据，无法查询',
            instance,
          );
        }
        return handleFullDayStatus(manifest, ctx, url.searchParams);
      }
      default:
        return problemResponse(404, CODES.notFound, `未知资源 ${resource}`, instance);
    }
  } catch (err) {
    return errorToResponse(err, instance);
  }
}

/** Vercel Node.js Web Handler 入口（fetch 风格）。 */
export default async function handler(req: Request): Promise<Response> {
  return handle(req);
}
