// MSW 契约守卫（tasklist 第 10 项）：夹具 → handler → 真实 HTTP 层 → api 客户端。
// 这里不用 fetch 桩，走的是 setup.ts 里 listen 的 MSW server，所以断言的
// 不只是「函数能跑」，还包括请求真的被按 URL 与查询串接住、信封真的被解析。

import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import {
  getContext,
  getMeta,
  getScreen,
  queryAvailability,
  queryDay,
} from "../api/public";
import type { Envelope } from "../api/types";
import {
  availabilityFixture,
  contextFixture,
  dayFixture,
  metaFixture,
  screenFixture,
} from "./fixtures/publicApi";
import { server } from "./msw";

describe("MSW 默认 handler", () => {
  it("四个公开端点都由默认 handler 承接，data 与夹具逐字段一致", async () => {
    const context = await getContext();
    expect(context.data).toEqual(contextFixture);
    expect(context.requestId).toBe("req_context");

    const meta = await getMeta();
    expect(meta.data).toEqual(metaFixture);

    const availability = await queryAvailability({
      date_offset: 0,
      start_node: "01",
      end_node: "05",
      limit: 20,
      offset: 0,
    });
    expect(availability.data).toEqual(availabilityFixture);

    const day = await queryDay({ date_offset: 2, room_id: "Q10-101" });
    expect(day.data).toEqual(dayFixture);

    const screen = await getScreen();
    expect(screen.data).toEqual(screenFixture);
  });

  it("view=day 与 view=availability 走不同响应，参数名按后端契约原样发出", async () => {
    const requests: URL[] = [];
    server.use(
      http.get("/api/query", ({ request }) => {
        const url = new URL(request.url);
        requests.push(url);
        const envelope = {
          code: 0,
          message: "success",
          data:
            url.searchParams.get("view") === "day"
              ? dayFixture
              : availabilityFixture,
          request_id: "req_query",
        } satisfies Envelope<unknown>;
        return HttpResponse.json(envelope);
      }),
    );

    await queryAvailability({
      date_offset: 3,
      building: "Q10",
      start_node: "06",
      end_node: "09",
      keyword: "综合",
      limit: 20,
      offset: 20,
    });
    await queryDay({ date_offset: 3, room_id: "Q10-101", building: "Q10" });

    expect(requests).toHaveLength(2);
    const [availability, day] = requests;
    expect(availability.searchParams.get("view")).toBe("availability");
    expect(availability.searchParams.get("date_offset")).toBe("3");
    expect(availability.searchParams.get("building")).toBe("Q10");
    expect(availability.searchParams.get("start_node")).toBe("06");
    expect(availability.searchParams.get("end_node")).toBe("09");
    expect(availability.searchParams.get("keyword")).toBe("综合");
    expect(availability.searchParams.get("limit")).toBe("20");
    expect(availability.searchParams.get("offset")).toBe("20");

    expect(day.searchParams.get("view")).toBe("day");
    expect(day.searchParams.get("room_id")).toBe("Q10-101");
    expect(day.searchParams.get("building")).toBe("Q10");
  });
});
