import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, REQUEST_TIMEOUT_MS, request } from "./client";
import {
  NETWORK_ERROR_CODE,
  UNKNOWN_REQUEST_ID,
  getErrorTitle,
} from "./errors";
import type { Envelope } from "./types";

/** 构造一条 JSON 响应；status 与信封 code 故意可以不一致，用来验业务码优先。 */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8" },
  });
}

function envelope<T>(data: T, requestId = "req_test"): Envelope<T> {
  return { code: 0, message: "success", data, request_id: requestId };
}

function stubFetch(response: Response) {
  const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(response);
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/**
 * 让 fetch 一直挂起，直到传入的 signal 触发才以取消原因拒绝——与真实实现一致，
 * 用来验证超时与调用方取消这两条路径，也顺便暴露「合并信号没接上」的悬挂。
 */
function stubHangingFetch() {
  const fetchMock = vi.fn<typeof fetch>(
    (_input, init) =>
      new Promise<Response>((_resolve, reject) => {
        const signal = init?.signal;
        if (!signal) {
          return;
        }
        if (signal.aborted) {
          reject(signal.reason);
          return;
        }
        signal.addEventListener("abort", () => reject(signal.reason), {
          once: true,
        });
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

/** 捕获请求抛出的原始值；请求成功时直接失败，避免测试漏掉「本该报错」的分支。 */
async function captureError(promise: Promise<unknown>): Promise<unknown> {
  try {
    await promise;
  } catch (error) {
    return error;
  }
  throw new Error("期望请求失败，但请求成功了");
}

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("request 成功路径", () => {
  it("code 为 0 时返回 data，并保留本次响应的 request_id", async () => {
    const fetchMock = stubFetch(
      jsonResponse(envelope({ value: "ok" }, "req_success")),
    );

    const result = await request<{ value: string }>("/api/context");

    expect(result).toEqual({
      data: { value: "ok" },
      requestId: "req_success",
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/context");
  });
});

describe("request 失败路径", () => {
  it("code 非 0 时抛 ApiError，带上业务码、request_id 与服务端 message", async () => {
    stubFetch(
      jsonResponse(
        {
          code: 42901,
          message: "请求过于频繁，请稍后重试",
          data: null,
          request_id: "req_limited",
        },
        429,
      ),
    );

    const error = await captureError(request("/api/query"));

    expect(error).toBeInstanceOf(ApiError);
    const apiError = error as ApiError;
    expect(apiError.code).toBe(42901);
    expect(apiError.requestId).toBe("req_limited");
    expect(apiError.message).toBe("请求过于频繁，请稍后重试");
    expect(apiError.retriable).toBe(true);
  });

  it("非 JSON 响应按解析失败收敛，request_id 记「未知」且不回显原始正文", async () => {
    stubFetch(
      new Response("<html>502 Bad Gateway</html>", {
        status: 502,
        headers: { "Content-Type": "text/html" },
      }),
    );

    const error = await captureError(request("/api/meta"));

    expect(error).toBeInstanceOf(ApiError);
    const apiError = error as ApiError;
    expect(apiError.code).toBe(50000);
    expect(apiError.requestId).toBe(UNKNOWN_REQUEST_ID);
    expect(apiError.message).not.toContain("Bad Gateway");
    expect(getErrorTitle(apiError.code)).toBe("服务异常");
  });

  it("JSON 但不是信封形状时同样按解析失败处理", async () => {
    stubFetch(jsonResponse({ error: "upstream failed" }));

    const error = await captureError(request("/api/meta"));

    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe(50000);
    expect((error as ApiError).requestId).toBe(UNKNOWN_REQUEST_ID);
  });

  it("成功信封缺少 data 时按解析失败处理，不把 null 当成功数据", async () => {
    stubFetch(
      jsonResponse({
        code: 0,
        message: "success",
        data: null,
        request_id: "req_empty",
      }),
    );

    const error = await captureError(request("/api/meta"));

    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe(50000);
  });

  it("网络中断收敛为网络异常，而不是把 TypeError 漏给页面", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn<typeof fetch>().mockRejectedValue(new TypeError("Failed to fetch")),
    );

    const error = await captureError(request("/api/screen"));

    expect(error).toBeInstanceOf(ApiError);
    const apiError = error as ApiError;
    expect(apiError.code).toBe(NETWORK_ERROR_CODE);
    expect(apiError.requestId).toBe(UNKNOWN_REQUEST_ID);
    expect(apiError.retriable).toBe(true);
    expect(getErrorTitle(apiError.code)).toBe("网络异常，请检查连接");
  });

  it("HTTP 状态与业务码不一致时以业务码判定成败", async () => {
    const fetchMock = vi
      .fn<typeof fetch>()
      // HTTP 400 + 业务码 40001：按失败处理。
      .mockResolvedValueOnce(
        jsonResponse(
          {
            code: 40001,
            message: "date_offset 取值 0..10",
            data: null,
            request_id: "req_bad_param",
          },
          400,
        ),
      )
      // HTTP 202 + 业务码 0：仍按成功处理（Requirement 1.4）。
      .mockResolvedValueOnce(
        jsonResponse(envelope({ value: "ok" }, "req_202"), 202),
      );
    vi.stubGlobal("fetch", fetchMock);

    const failure = await captureError(request("/api/query"));
    expect(failure).toBeInstanceOf(ApiError);
    expect((failure as ApiError).code).toBe(40001);
    expect((failure as ApiError).retriable).toBe(false);

    const success = await request<{ value: string }>("/api/query");
    expect(success).toEqual({ data: { value: "ok" }, requestId: "req_202" });
  });
});

describe("request 取消路径", () => {
  it("调用方 abort 时原样抛 AbortError，不进入错误态", async () => {
    stubHangingFetch();

    const controller = new AbortController();
    const pending = request("/api/query", { signal: controller.signal });
    controller.abort();

    const error = await captureError(pending);

    expect(error).not.toBeInstanceOf(ApiError);
    expect((error as Error).name).toBe("AbortError");
  });

  it("传入已取消的 signal 时直接抛 AbortError，不把取消当成正常请求", async () => {
    const fetchMock = stubHangingFetch();

    const controller = new AbortController();
    controller.abort();

    const error = await captureError(
      request("/api/query", { signal: controller.signal }),
    );

    expect(error).not.toBeInstanceOf(ApiError);
    expect((error as Error).name).toBe("AbortError");
    expect(fetchMock.mock.calls[0][1]?.signal?.aborted).toBe(true);
  });

  it("15 秒后超时按网络异常收敛为 ApiError，不会被当成调用方取消", async () => {
    // 超时由客户端自己的 setTimeout 驱动，可以用假计时器精确推进，不用真等 15 秒。
    vi.useFakeTimers();
    stubHangingFetch();

    const pending = captureError(request("/api/slow"));

    // 差 1 毫秒时还不该超时：确认阈值就是 REQUEST_TIMEOUT_MS。
    await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS - 1);
    expect(vi.getTimerCount()).toBe(1);

    await vi.advanceTimersByTimeAsync(1);
    const error = await pending;

    expect(error).toBeInstanceOf(ApiError);
    const apiError = error as ApiError;
    expect(apiError.code).toBe(NETWORK_ERROR_CODE);
    expect(apiError.requestId).toBe(UNKNOWN_REQUEST_ID);
    expect(apiError.message).toBe("请求超时");
    // 请求结束后计时器必须被 clearTimeout 清掉，否则长页面会攒下一堆悬挂计时器。
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe("request 旧浏览器兜底", () => {
  it("AbortSignal.any 与 AbortSignal.timeout 都不存在时，超时与取消仍然正常", async () => {
    // Safari 17.4 / Firefox 124 才有 AbortSignal.any（AbortSignal.timeout 是 Safari 16），
    // 客户端改用 AbortController + setTimeout 手动合并，因此这两个静态方法缺失也必须照常工作。
    const savedAny = AbortSignal.any;
    const savedTimeout = AbortSignal.timeout;
    Reflect.deleteProperty(AbortSignal, "any");
    Reflect.deleteProperty(AbortSignal, "timeout");

    vi.useFakeTimers();
    try {
      expect(typeof AbortSignal.any).toBe("undefined");
      expect(typeof AbortSignal.timeout).toBe("undefined");
      stubHangingFetch();

      // 取消：原样抛 AbortError，不走错误态。
      const controller = new AbortController();
      const cancelled = captureError(
        request("/api/query", { signal: controller.signal }),
      );
      controller.abort();
      const cancelledError = await cancelled;
      expect(cancelledError).not.toBeInstanceOf(ApiError);
      expect((cancelledError as Error).name).toBe("AbortError");

      // 超时：仍由 15 秒计时器触发，收敛成「请求超时」。
      const timedOut = captureError(request("/api/slow"));
      await vi.advanceTimersByTimeAsync(REQUEST_TIMEOUT_MS);
      const timeoutError = await timedOut;
      expect(timeoutError).toBeInstanceOf(ApiError);
      expect((timeoutError as ApiError).code).toBe(NETWORK_ERROR_CODE);
      expect((timeoutError as ApiError).message).toBe("请求超时");
    } finally {
      vi.useRealTimers();
      Reflect.set(AbortSignal, "any", savedAny);
      Reflect.set(AbortSignal, "timeout", savedTimeout);
    }
  });
});
