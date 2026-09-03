import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { call, clearFixture, get, problemOf, useFixture } from './helpers';

// fixture root-a：教学周内（anchor 2026-09-01 周二 = 第 2 周，total 4；已发布 1–3 周）。
const NOW_A = '2026-09-03T00:00:00+08:00'; // 周四 09-03
const CACHE_DATA = 'public, s-maxage=300, stale-while-revalidate=600';
const G1 = 'zong-he-jiao-xue-lou';

interface JsonObj {
  [k: string]: unknown;
}

async function jsonOf(res: Response): Promise<JsonObj> {
  return (await res.json()) as JsonObj;
}

describe('GET /api/v1/manifest', () => {
  beforeAll(() => {
    useFixture('root-a', NOW_A, 'root-a-index.json');
  });
  afterAll(clearFixture);

  it('返回公开清单：分组/节点/状态字典/anchor 透传 + 逐周新鲜度', async () => {
    const res = await get('/api/v1/manifest');
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('application/json; charset=utf-8');
    expect(res.headers.get('cache-control')).toBe(CACHE_DATA);

    const body = await jsonOf(res);
    expect(body.schema_version).toBe(1);
    expect(body.term).toBe('2026-2027-1');
    expect(body.release_id).toBe('rel-20260902-041000');
    expect(body.generated_at).toBe('2026-09-02T04:10:00+08:00');
    expect(body.anchor).toEqual({
      date: '2026-09-01',
      week: 2,
      total_weeks: 4,
      timezone: 'Asia/Shanghai',
      in_teaching_calendar: true,
    });

    const groups = body.groups as JsonObj[];
    expect(groups).toHaveLength(2);
    expect(groups[0]).toEqual({ id: G1, name: '综合教学楼', order: 10, room_count: 6 });
    expect(groups[1]).toEqual({ id: 'ti-yu-guan', name: '体育馆', order: 20, room_count: 2 });

    const nodes = body.nodes as JsonObj[];
    expect(nodes).toHaveLength(12);
    expect(nodes[0]).toEqual({ code: '01', source_block: '0102' });

    const statuses = body.statuses as JsonObj;
    expect((statuses['5'] as JsonObj).available).toBe(true);
    expect((statuses['1'] as JsonObj).available).toBe(false);

    const weeks = body.weeks as JsonObj[];
    expect(weeks).toEqual([
      {
        week: 1,
        generated_at: '2026-08-24T04:10:00+08:00',
        last_success_at: '2026-08-24T04:10:00+08:00',
        status: 'stale',
      },
      {
        week: 2,
        generated_at: '2026-09-02T04:10:00+08:00',
        last_success_at: '2026-09-02T04:10:00+08:00',
        status: 'fresh',
      },
      {
        week: 3,
        generated_at: '2026-09-02T04:10:00+08:00',
        last_success_at: '2026-09-02T04:10:00+08:00',
        status: 'fresh',
      },
      { week: 4, generated_at: null, last_success_at: null, status: 'missing' },
    ]);
    // 不暴露内部 sha256/snapshot_id
    expect('sha256' in weeks[0]!).toBe(false);
  });
});

describe('GET /api/v1/context', () => {
  afterAll(clearFixture);

  it('教学周内：date/weekday/week/term/in_teaching_calendar', async () => {
    useFixture('root-a', NOW_A, 'root-a-index.json');
    const res = await get('/api/v1/context');
    expect(res.status).toBe(200);
    expect(res.headers.get('cache-control')).toBe('public, s-maxage=60');
    const body = await jsonOf(res);
    expect(body).toEqual({
      date: '2026-09-03',
      weekday: 4,
      week: 2,
      term: '2026-2027-1',
      in_teaching_calendar: true,
    });
  });

  it('周一凌晨自动换周（Q148）', async () => {
    useFixture('root-a', '2026-09-07T00:30:00+08:00', 'root-a-index.json');
    const body = await jsonOf(await get('/api/v1/context'));
    expect(body).toMatchObject({ date: '2026-09-07', weekday: 1, week: 3 });
  });

  it('跨午夜按北京日期计算', async () => {
    useFixture('root-a', '2026-09-03T16:30:00Z', 'root-a-index.json'); // = 09-04 00:30 +08
    const body = await jsonOf(await get('/api/v1/context'));
    expect(body).toMatchObject({ date: '2026-09-04', weekday: 5, week: 2 });
  });
});

describe('GET /api/v1/empty-classrooms', () => {
  beforeAll(() => {
    useFixture('root-a', NOW_A, 'root-a-index.json');
  });
  afterAll(clearFixture);

  it('默认 week/day（context）全节次空教室 + 响应结构', async () => {
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}`);
    expect(res.status).toBe(200);
    expect(res.headers.get('cache-control')).toBe(CACHE_DATA);
    const body = await jsonOf(res);
    expect(body.query).toEqual({
      group_id: G1,
      keyword: null,
      week: 2,
      day: 4, // NOW_A 是周四
      start_node: '01',
      end_node: '12',
    });
    expect(body.rooms).toEqual([
      { id: '0103', name: '文史楼201' },
      { id: '0101', name: '综合教学楼101' },
      { id: '0106', name: '综合教学楼202' },
    ]);
    expect(body.count).toBe(3);
    expect(body.snapshot).toEqual({
      snapshot_id: 'snap-2026-09-02-week-02',
      generated_at: '2026-09-02T04:10:00+08:00',
      stale: false,
    });
  });

  it('节次区间可用性判定（Q59：小节状态 ∈ {5,8}）', async () => {
    const body = await jsonOf(
      await get(`/api/v1/empty-classrooms?group_id=${G1}&day=1&start_node=01&end_node=04`),
    );
    const ids = (body.rooms as JsonObj[]).map((r) => r.id);
    expect(ids).toEqual(['0105', '0103', '0101', '0102', '0106']);
    expect(body.count).toBe(5);
  });

  it('keyword：ASCII 大小写不敏感 + NFKC 全角匹配', async () => {
    for (const kw of ['f126', 'F126', 'Ｆ１２６']) {
      const body = await jsonOf(
        await get(
          `/api/v1/empty-classrooms?group_id=${G1}&day=1&start_node=01&end_node=04&keyword=${encodeURIComponent(kw)}`,
        ),
      );
      expect(body.rooms).toEqual([{ id: '0105', name: 'F126' }]);
      expect(body.count).toBe(1);
      expect((body.query as JsonObj).keyword).toBe('f126');
    }
  });

  it('keyword 只过滤该分组房间；无匹配 → 200 空集合', async () => {
    const none = await jsonOf(
      await get(`/api/v1/empty-classrooms?group_id=${G1}&keyword=${encodeURIComponent('不存在')}`),
    );
    expect(none.rooms).toEqual([]);
    expect(none.count).toBe(0);
    // 正则元字符按字面处理
    const literal = await jsonOf(
      await get(`/api/v1/empty-classrooms?group_id=${G1}&keyword=${encodeURIComponent('.*')}`),
    );
    expect(literal.count).toBe(0);
  });

  it('同名房间：按可用性各自独立返回（文史楼201 仅 0103 全天空闲）', async () => {
    const body = await jsonOf(
      await get(
        `/api/v1/empty-classrooms?group_id=${G1}&day=1&keyword=${encodeURIComponent('文史楼201')}`,
      ),
    );
    expect(body.rooms).toEqual([{ id: '0103', name: '文史楼201' }]);
    expect(body.count).toBe(1);
  });

  it('分组隔离：g2 查询不返回 g1 房间', async () => {
    const body = await jsonOf(
      await get(`/api/v1/empty-classrooms?group_id=ti-yu-guan&day=1`),
    );
    const ids = (body.rooms as JsonObj[]).map((r) => r.id);
    expect(ids).toEqual(['0201', '0202']);
    expect(body.count).toBe(2);
  });

  it('过期周仍返回最近成功数据并标记 snapshot.stale=true', async () => {
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&week=1&day=1`);
    expect(res.status).toBe(200);
    const body = await jsonOf(res);
    expect((body.snapshot as JsonObj).stale).toBe(true);
    expect((body.snapshot as JsonObj).snapshot_id).toBe('snap-2026-08-24-week-01');
  });

  it('start_node > end_node → 400 invalid_parameter', async () => {
    const res = await get(
      `/api/v1/empty-classrooms?group_id=${G1}&day=1&start_node=05&end_node=03`,
    );
    expect(res.status).toBe(400);
    expect(res.headers.get('content-type')).toContain('application/problem+json');
    const body = await problemOf(res);
    expect(body.code).toBe('invalid_parameter');
    expect(body.status).toBe(400);
    expect(body.instance).toBe(
      `/api/v1/empty-classrooms?group_id=${G1}&day=1&start_node=05&end_node=03`,
    );
  });

  it('节次格式非法 → 400', async () => {
    for (const qs of [
      'start_node=5',
      'start_node=00',
      'start_node=13',
      'end_node=0',
      'end_node=99',
    ]) {
      const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&day=1&${qs}`);
      expect(res.status).toBe(400);
      expect((await problemOf(res)).code).toBe('invalid_parameter');
    }
  });

  it('week/day 范围非法 → 400', async () => {
    for (const qs of ['week=abc', 'week=0', 'week=31', 'week=5', 'day=0', 'day=8', 'day=x']) {
      const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&${qs}`);
      expect(res.status, qs).toBe(400);
      expect((await problemOf(res)).code, qs).toBe('invalid_parameter');
    }
  });

  it('keyword 超过 32 字符 → 400', async () => {
    const res = await get(
      `/api/v1/empty-classrooms?group_id=${G1}&keyword=${encodeURIComponent('长'.repeat(33))}`,
    );
    expect(res.status).toBe(400);
    expect((await problemOf(res)).code).toBe('invalid_parameter');
  });

  it('缺 group_id → 400 invalid_parameter', async () => {
    const res = await get('/api/v1/empty-classrooms');
    expect(res.status).toBe(400);
    expect((await problemOf(res)).code).toBe('invalid_parameter');
  });

  it('未知分组 → 404 not_collected 并附可用分组', async () => {
    const res = await get('/api/v1/empty-classrooms?group_id=not-a-group');
    expect(res.status).toBe(404);
    const body = await problemOf(res);
    expect(body.code).toBe('not_collected');
    expect(String(body.detail)).toContain('ti-yu-guan');
  });

  it('未发布周 → 404 not_collected', async () => {
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&week=4&day=1`);
    expect(res.status).toBe(404);
    expect((await problemOf(res)).code).toBe('not_collected');
  });

  it('该周缺失 day（week-03 无 day5）→ 404 not_collected', async () => {
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&week=3&day=5`);
    expect(res.status).toBe(404);
    expect((await problemOf(res)).code).toBe('not_collected');
  });
});

describe('GET /api/v1/full-day-status', () => {
  beforeAll(() => {
    useFixture('root-a', NOW_A, 'root-a-index.json');
  });
  afterAll(clearFixture);

  it('返回 01–12 全状态键值（Q153），无 count 外壳', async () => {
    const res = await get(
      `/api/v1/full-day-status?group_id=${G1}&day=1&keyword=${encodeURIComponent('综合教学楼102')}`,
    );
    expect(res.status).toBe(200);
    expect(res.headers.get('cache-control')).toBe(CACHE_DATA);
    const body = await jsonOf(res);
    expect(body.query).toEqual({
      group_id: G1,
      keyword: '综合教学楼102',
      week: 2,
      day: 1,
    });
    expect(body.rooms).toEqual([
      {
        id: '0102',
        name: '综合教学楼102',
        statuses: {
          '01': 5, '02': 5, '03': 5, '04': 5, '05': 1, '06': 5, '07': 5,
          '08': 5, '09': 5, '10': 5, '11': 5, '12': 5,
        },
      },
    ]);
    expect('count' in body).toBe(false);
    expect((body.snapshot as JsonObj).snapshot_id).toBe('snap-2026-09-02-week-02');
  });

  it('分组全部房间按 名称+jsbh 自然排序返回', async () => {
    const body = await jsonOf(await get(`/api/v1/full-day-status?group_id=${G1}&day=1`));
    const ids = (body.rooms as JsonObj[]).map((r) => r.id);
    expect(ids).toEqual(['0105', '0103', '0104', '0101', '0102', '0106']);
  });
});

describe('路由与方法', () => {
  beforeAll(() => {
    useFixture('root-a', NOW_A, 'root-a-index.json');
  });
  afterAll(clearFixture);

  it('未知 /api/v1/* 路径 → 404 problem+json', async () => {
    for (const p of ['/api/v1/nope', '/api/v1/empty-classrooms/x', '/api/v1']) {
      const res = await get(p);
      expect(res.status, p).toBe(404);
      expect(res.headers.get('content-type')).toContain('application/problem+json');
      expect((await problemOf(res)).code).toBe('not_found');
    }
  });

  it('非 GET → 405 method_not_allowed + Allow: GET', async () => {
    for (const p of ['/api/v1/manifest', '/api/v1/context', '/api/v1/empty-classrooms']) {
      const res = await call('POST', p);
      expect(res.status, p).toBe(405);
      expect(res.headers.get('allow')).toBe('GET');
      expect((await problemOf(res)).code).toBe('method_not_allowed');
    }
  });

  it('未知路径的非 GET 请求 → 404（先判定资源）', async () => {
    const res = await call('POST', '/api/v1/nope');
    expect(res.status).toBe(404);
    expect((await problemOf(res)).code).toBe('not_found');
  });
});

describe('无数据（无 manifest）', () => {
  beforeAll(() => {
    useFixture('empty', NOW_A); // 无索引覆盖文件
  });
  afterAll(clearFixture);

  it('manifest/context/empty-classrooms → 503 no_snapshot', async () => {
    for (const p of [
      '/api/v1/manifest',
      '/api/v1/context',
      `/api/v1/empty-classrooms?group_id=${G1}`,
    ]) {
      const res = await get(p);
      expect(res.status, p).toBe(503);
      expect(res.headers.get('content-type')).toContain('application/problem+json');
      expect((await problemOf(res)).code).toBe('no_snapshot');
    }
  });
});

describe('非教学周', () => {
  beforeAll(() => {
    useFixture('root-c', NOW_A, 'root-c-index.json');
  });
  afterAll(clearFixture);

  it('context：in_teaching_calendar=false、week=null', async () => {
    const body = await jsonOf(await get('/api/v1/context'));
    expect(body).toMatchObject({
      date: '2026-09-03',
      weekday: 4,
      week: null,
      in_teaching_calendar: false,
    });
  });

  it('省略 week → 400 not_in_teaching', async () => {
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&day=1`);
    expect(res.status).toBe(400);
    expect((await problemOf(res)).code).toBe('not_in_teaching');
  });

  it('显式 week 仍可查询已发布周', async () => {
    const body = await jsonOf(
      await get(`/api/v1/empty-classrooms?group_id=${G1}&week=2&day=1`),
    );
    expect((body.rooms as JsonObj[]).map((r) => r.id)).toEqual(['0103', '0101', '0106']);
    expect((body.snapshot as JsonObj).stale).toBe(false);
  });

  it('manifest 周列表照常给出 missing/stale', async () => {
    const body = await jsonOf(await get('/api/v1/manifest'));
    const weeks = body.weeks as JsonObj[];
    expect(weeks[0]).toMatchObject({ week: 1, status: 'stale' });
    expect(weeks[3]).toMatchObject({ week: 4, status: 'missing' });
  });
});

describe('数据完整性', () => {
  afterAll(clearFixture);

  it('周文件 SHA-256 与索引不一致 → 503 internal', async () => {
    useFixture('root-a', NOW_A, 'bad-index.json');
    const res = await get(`/api/v1/empty-classrooms?group_id=${G1}&week=2&day=1`);
    expect(res.status).toBe(503);
    const body = await problemOf(res);
    expect(body.code).toBe('internal');
    expect(res.headers.get('content-type')).toContain('application/problem+json');
  });
});
