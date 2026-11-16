// 公开接口的 MSW handler（tasklist 第 10 项；design「Test Strategy」的契约守卫）。
//
// 所有响应都由 src/test/fixtures/publicApi.ts 的夹具驱动，handler 再用 `satisfies`
// 把信封也钉成 Envelope<T>：夹具只管 data，信封的四个字段（code/message/data/
// request_id）在这里被类型守卫，后端改字段时先编译报错，而不是测试里悄悄漏掉。
//
// 默认 handler 覆盖四个公开端点（/api/query 按 view 分 availability / day 两档）；
// 需要特殊响应的用例用 `server.use(...)` 在本用例内覆盖，不改这里的默认真相。

import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import type {
  AvailabilityResult,
  ContextInfo,
  DayResult,
  Envelope,
  MetaResult,
  ScreenResult,
} from "../api/types";
import {
  availabilityFixture,
  contextFixture,
  dayFixture,
  metaFixture,
  screenFixture,
} from "./fixtures/publicApi";

/** 成功信封：data 是夹具，形状由 satisfies 钉住。 */
const contextEnvelope = {
  code: 0,
  message: "success",
  data: contextFixture,
  request_id: "req_context",
} satisfies Envelope<ContextInfo>;

const metaEnvelope = {
  code: 0,
  message: "success",
  data: metaFixture,
  request_id: "req_meta",
} satisfies Envelope<MetaResult>;

const availabilityEnvelope = {
  code: 0,
  message: "success",
  data: availabilityFixture,
  request_id: "req_availability",
} satisfies Envelope<AvailabilityResult>;

const dayEnvelope = {
  code: 0,
  message: "success",
  data: dayFixture,
  request_id: "req_day",
} satisfies Envelope<DayResult>;

const screenEnvelope = {
  code: 0,
  message: "success",
  data: screenFixture,
  request_id: "req_screen",
} satisfies Envelope<ScreenResult>;

/**
 * 默认 handler 列表。
 * `/api/query` 按 `view` 分流：day 视图必须带 room_id，availability 不带，
 * 两档响应体不同形状，不能在同一个 handler 里混着返回。
 */
export const apiHandlers = [
  http.get("/api/context", () => HttpResponse.json(contextEnvelope)),
  http.get("/api/meta", () => HttpResponse.json(metaEnvelope)),
  http.get("/api/query", ({ request }) => {
    const view = new URL(request.url).searchParams.get("view");
    return HttpResponse.json(
      view === "day" ? dayEnvelope : availabilityEnvelope,
    );
  }),
  http.get("/api/screen", () => HttpResponse.json(screenEnvelope)),
];

/** Node 环境的 MSW server；生命周期挂在 src/test/setup.ts（listen/reset/close）。 */
export const server = setupServer(...apiHandlers);
