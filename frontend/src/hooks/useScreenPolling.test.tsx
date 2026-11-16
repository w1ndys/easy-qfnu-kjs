import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  screenFixture,
  updatedScreenFixture,
} from "../test/fixtures/publicApi";
import { SCREEN_POLL_INTERVAL_MS, useScreenPolling } from "./useScreenPolling";

// 大屏轮询（tasklist 第 9 项；Requirement 7）。
// 三条验收点在 hook 层各钉一条：
// 1) 可见时推进 30 秒多一次真实请求；
// 2) 失败一轮后旧数据仍在、置离线，下一次成功清除并用新响应替换数据；
// 3) 隐藏后推进 60 秒无新请求，重新可见立即补一次。
// 另加一条「请求在途时保留旧数据」，对应 Requirement 7.2。

/** 成功信封：data 用与后端类型逐字段对齐的大屏夹具；可换成「字段已变」的那份。 */
function successEnvelope(data: unknown = screenFixture): Response {
  return new Response(
    JSON.stringify({
      code: 0,
      message: "success",
      data,
      request_id: "req_poll_ok",
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

/** 失败信封：50000 走错误态，request_id 只用于断言请求真的失败过。 */
function failureEnvelope(): Response {
  return new Response(
    JSON.stringify({
      code: 50000,
      message: "boom",
      data: null,
      request_id: "req_poll_fail",
    }),
    { status: 200, headers: { "Content-Type": "application/json" } },
  );
}

/**
 * 把 fetch 换成按用例返回的桩，并返回可断言的 mock。
 * 泛型显式写出入参签名，`mock.calls` 才能按真实 fetch 的形状取到请求地址。
 */
function stubFetch(respond: () => Response) {
  const fetchMock = vi.fn<(input: RequestInfo | URL) => Promise<Response>>(
    async () => respond(),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/**
 * fake timer 下不能等真实时间：这里把 fetch → response.text() → 信封解析
 * 这条微任务链走完。多推几拍是因为 promise 是逐层 then 下去的。
 */
async function flushAsync() {
  await act(async () => {
    for (let index = 0; index < 8; index += 1) {
      await Promise.resolve();
    }
  });
}

/** 推进 fake 时钟并让该拍请求落地。 */
async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
  await flushAsync();
}

/** jsdom 的 visibilityState 是只读 getter：测试里替换成可切换的实现。 */
function setVisibility(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => state,
  });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  // 还原可见性，避免用例之间互相污染。
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => "visible",
  });
});

describe("useScreenPolling", () => {
  it("间隔与后端 /api/screen 的 30 秒缓存对齐", () => {
    expect(SCREEN_POLL_INTERVAL_MS).toBe(30_000);
  });

  it("可见时每 30 秒请求一次 /api/screen，且每次都真实发请求", async () => {
    const fetchMock = stubFetch(successEnvelope);

    const { result } = renderHook(() => useScreenPolling());

    // 挂载即请求一次，不等 30 秒。
    await flushAsync();
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(result.current.loading).toBe(false);
    expect(result.current.data).toEqual(screenFixture);

    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(3);

    // 每一拍都是 /api/screen 本身：没有前端缓存命中导致跳过请求。
    for (const [input] of fetchMock.mock.calls) {
      expect(String(input)).toBe("/api/screen");
    }
  });

  it("失败一轮后旧数据仍在并置离线，下一次成功用新响应更新并清除标记", async () => {
    let screenCalls = 0;
    const fetchMock = stubFetch(() => {
      screenCalls += 1;
      // 第二拍失败；第三拍成功且房间数已变，用来证明数据真的换了新响应。
      if (screenCalls === 2) {
        return failureEnvelope();
      }
      return screenCalls >= 3
        ? successEnvelope(updatedScreenFixture)
        : successEnvelope();
    });

    const { result } = renderHook(() => useScreenPolling());
    await flushAsync();

    expect(result.current.data).toEqual(screenFixture);
    expect(result.current.offline).toBe(false);
    const lastSuccessAt = result.current.lastSuccessAt;
    expect(lastSuccessAt).not.toBeNull();

    // 第二拍失败：旧数据不清空，只置离线；错误不进 error，页面仍显示数据。
    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(result.current.data).toEqual(screenFixture);
    expect(result.current.data?.scale.rooms).toBe(2481);
    expect(result.current.offline).toBe(true);
    expect(result.current.error).toBeNull();
    // 失败不刷新「最后成功时间」。
    expect(result.current.lastSuccessAt).toBe(lastSuccessAt);

    // 第三拍成功：离线标记清除，最后成功时间前进一拍，数据换成这次响应的内容。
    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(result.current.offline).toBe(false);
    expect(result.current.data?.scale.rooms).toBe(2482);
    expect(result.current.data).toEqual(updatedScreenFixture);
    expect(result.current.lastSuccessAt).not.toBe(lastSuccessAt);
  });

  it("首屏失败进错误态，调用 retry() 立即重发并在成功后恢复", async () => {
    let calls = 0;
    const fetchMock = stubFetch(() => {
      calls += 1;
      return calls === 1 ? failureEnvelope() : successEnvelope();
    });

    const { result } = renderHook(() => useScreenPolling());
    await flushAsync();

    expect(result.current.data).toBeNull();
    expect(result.current.error).not.toBeNull();
    expect(result.current.offline).toBe(true);
    expect(result.current.loading).toBe(false);

    // retry 不等 30 秒：立即发第二次请求。
    await act(async () => {
      result.current.retry();
    });
    await flushAsync();

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(result.current.data).toEqual(screenFixture);
    expect(result.current.error).toBeNull();
    expect(result.current.offline).toBe(false);
  });

  it("新一轮请求在途时继续展示上一次成功的数据", async () => {
    const deferred: { resolve: ((response: Response) => void) | null } = {
      resolve: null,
    };
    let calls = 0;
    const fetchMock = vi.fn(async () => {
      calls += 1;
      if (calls === 1) {
        return successEnvelope();
      }
      return new Promise<Response>((resolve) => {
        deferred.resolve = resolve;
      });
    });
    vi.stubGlobal("fetch", fetchMock);

    const { result } = renderHook(() => useScreenPolling());
    await flushAsync();
    expect(result.current.data).toEqual(screenFixture);

    // 第二拍已发出但尚未返回：旧数据照常展示，也不提前置离线。
    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(result.current.data).toEqual(screenFixture);
    expect(result.current.offline).toBe(false);

    await act(async () => {
      deferred.resolve?.(successEnvelope());
      await Promise.resolve();
    });
    await flushAsync();
    expect(result.current.data).toEqual(screenFixture);
  });

  it("隐藏后暂停轮询，重新可见时立即请求一次并重新对齐 30 秒", async () => {
    const fetchMock = stubFetch(successEnvelope);

    const { result } = renderHook(() => useScreenPolling());
    await flushAsync();
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // 隐藏后推进 60 秒：一拍都不该发。
    setVisibility("hidden");
    await advance(60_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(result.current.data).toEqual(screenFixture);

    // 重新可见：立即补一次，不用等下一个 30 秒。
    setVisibility("visible");
    await flushAsync();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // 之后从「重新可见」这一刻重新对齐节奏。
    await advance(SCREEN_POLL_INTERVAL_MS);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});
