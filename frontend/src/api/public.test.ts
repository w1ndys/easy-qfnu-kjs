import { afterEach, describe, expect, it, vi } from "vitest";
import {
  getContext,
  getMeta,
  getScreen,
  queryAvailability,
  queryDay,
} from "./public";
import type {
  AvailabilityResult,
  ContextInfo,
  DayResult,
  MetaResult,
  ScreenResult,
} from "./types";

// 夹具只求形状正确（satisfies 守卫）：本项不渲染页面，内容是否真实由后续项负责。
const contextFixture = {
  date: "2026-11-15",
  term: "2026-2027-1",
  week: 11,
  weekday: 6,
  in_calendar: true,
} satisfies ContextInfo;

const metaFixture = {
  release: null,
  dict: null,
  axis: null,
  buildings: [],
  weeks: [],
  empty: true,
} satisfies MetaResult;

const availabilityFixture = {
  total: 1,
  items: [
    {
      room_id: "Q10-101",
      name: "综合楼101",
      building_id: "Q10",
      building_name: "综合教学楼",
    },
  ],
  date: "2026-11-16",
  term: "2026-2027-1",
  week: 11,
  weekday: 1,
} satisfies AvailabilityResult;

const dayFixture = {
  room_id: "Q10-101",
  name: "综合楼101",
  building_id: "Q10",
  building_name: "综合教学楼",
  date: "2026-11-16",
  term: "2026-2027-1",
  week: 11,
  weekday: 1,
  nodes: [],
} satisfies DayResult;

const screenFixture = {
  scale: {
    rooms: 2481,
    buildings: 26,
    term: "2026-2027-1",
    week: 5,
    window: [5, 8],
  },
  release: null,
  weeks: [],
  last_run: null,
  anomalies: { unknown_cells: 0, composite_cells: 0, attr_conflicts: 0 },
  occupancy: { by_building: [], by_weekday_block: [] },
  traffic: {
    pv: { today: 0, d7: 0, d30: 0 },
    uv: { today: 0, d7: 0, d30: 0 },
  },
  leaderboard: { buildings: [], rooms: [], keywords: [] },
  peaks: { heatmap: [], top: [] },
  trend: [],
  node_distribution: [],
  empty: true,
} satisfies ScreenResult;

function stubSuccess(data: unknown, requestId = "req_test") {
  const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(
    new Response(
      JSON.stringify({
        code: 0,
        message: "success",
        data,
        request_id: requestId,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function requestUrl(fetchMock: ReturnType<typeof stubSuccess>): URL {
  const [input] = fetchMock.mock.calls[0];
  if (typeof input !== "string") {
    throw new Error("期望薄函数以字符串 URL 发起请求");
  }
  return new URL(input, "http://localhost");
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("五个公开端点薄函数", () => {
  it("getContext 请求 /api/context 并返回 data 与 request_id", async () => {
    const fetchMock = stubSuccess(contextFixture, "req_context");

    const result = await getContext();

    expect(requestUrl(fetchMock).pathname).toBe("/api/context");
    expect(requestUrl(fetchMock).search).toBe("");
    expect(result).toEqual({ data: contextFixture, requestId: "req_context" });
  });

  it("getMeta 请求 /api/meta", async () => {
    const fetchMock = stubSuccess(metaFixture);

    const result = await getMeta();

    expect(requestUrl(fetchMock).pathname).toBe("/api/meta");
    expect(result.data.empty).toBe(true);
  });

  it("queryAvailability 显式拼 snake_case 参数并带上 view=availability", async () => {
    const fetchMock = stubSuccess(availabilityFixture);

    const result = await queryAvailability({
      date_offset: 3,
      building: "Q10",
      start_node: "01",
      end_node: "05",
      keyword: "综合",
      limit: 20,
      offset: 40,
    });

    const url = requestUrl(fetchMock);
    expect(url.pathname).toBe("/api/query");
    expect(url.searchParams.get("view")).toBe("availability");
    expect(url.searchParams.get("date_offset")).toBe("3");
    expect(url.searchParams.get("building")).toBe("Q10");
    expect(url.searchParams.get("start_node")).toBe("01");
    expect(url.searchParams.get("end_node")).toBe("05");
    expect(url.searchParams.get("keyword")).toBe("综合");
    expect(url.searchParams.get("limit")).toBe("20");
    expect(url.searchParams.get("offset")).toBe("40");
    // 不做自动大小写转换：camelCase 的键名一个都不该出现。
    expect(url.searchParams.has("dateOffset")).toBe(false);
    expect(url.searchParams.has("startNode")).toBe(false);
    expect(url.searchParams.has("endNode")).toBe(false);
    expect(result.data.total).toBe(1);
  });

  it("queryAvailability 未传的可选参数不出现在查询串里", async () => {
    const fetchMock = stubSuccess(availabilityFixture);

    await queryAvailability({ date_offset: 0 });

    const url = requestUrl(fetchMock);
    expect(url.searchParams.get("date_offset")).toBe("0");
    expect(url.searchParams.has("building")).toBe(false);
    expect(url.searchParams.has("keyword")).toBe(false);
    expect(url.searchParams.has("limit")).toBe(false);
    expect(url.searchParams.has("offset")).toBe(false);
  });

  it("queryDay 显式拼 view=day、date_offset、room_id 与 building", async () => {
    const fetchMock = stubSuccess(dayFixture);

    const result = await queryDay({
      date_offset: 1,
      room_id: "Q10-101",
      building: "Q10",
    });

    const url = requestUrl(fetchMock);
    expect(url.pathname).toBe("/api/query");
    expect(url.searchParams.get("view")).toBe("day");
    expect(url.searchParams.get("date_offset")).toBe("1");
    expect(url.searchParams.get("room_id")).toBe("Q10-101");
    expect(url.searchParams.get("building")).toBe("Q10");
    // day 视图不接受 keyword 与分页参数，薄函数也不该替调用点补。
    expect(url.searchParams.has("keyword")).toBe(false);
    expect(url.searchParams.has("limit")).toBe(false);
    expect(url.searchParams.has("offset")).toBe(false);
    expect(result.data.room_id).toBe("Q10-101");
  });

  it("getScreen 请求 /api/screen 且不带查询参数", async () => {
    const fetchMock = stubSuccess(screenFixture, "req_screen");

    const result = await getScreen();

    const url = requestUrl(fetchMock);
    expect(url.pathname).toBe("/api/screen");
    expect(url.search).toBe("");
    expect(result).toEqual({ data: screenFixture, requestId: "req_screen" });
  });
});
