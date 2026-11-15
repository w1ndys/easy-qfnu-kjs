import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, useLocation } from "react-router";
import { describe, expect, it } from "vitest";
import { useQueryFilters } from "./useQueryFilters";

/** 与 QueryPage 一致：默认区间由打开时刻决定，这里直接固定成下午档。 */
const FALLBACK = { start_node: "06", end_node: "09" };

function renderFilters(initialEntry: string) {
  // location 与 filters 一起读：断言的是「URL 变成什么」，不是组件内部状态。
  return renderHook(
    () => ({ filters: useQueryFilters(FALLBACK), location: useLocation() }),
    {
      wrapper: ({ children }: { children: ReactNode }) => (
        <MemoryRouter initialEntries={[initialEntry]}>{children}</MemoryRouter>
      ),
    },
  );
}

describe("useQueryFilters 读取 URL", () => {
  it("空 URL 用预填区间与默认值，并标记 URL 尚未规范化", () => {
    const { result } = renderFilters("/");

    expect(result.current.filters.filters).toEqual({
      date_offset: 0,
      building: "",
      start_node: "06",
      end_node: "09",
      keyword: "",
      page: 1,
    });
    expect(result.current.filters.isCanonical).toBe(false);
  });

  it("完整 URL 原样读回，且已是规范化形态", () => {
    const { result } = renderFilters(
      "/?date_offset=3&building=Q10&start_node=01&end_node=05&keyword=%E7%BB%BC%E5%90%88&page=2",
    );

    expect(result.current.filters.filters).toEqual({
      date_offset: 3,
      building: "Q10",
      start_node: "01",
      end_node: "05",
      keyword: "综合",
      page: 2,
    });
    expect(result.current.filters.isCanonical).toBe(true);
  });

  it("非法值回退默认：偏移越界、页码为 0、起点晚于终点整体回退", () => {
    const { result } = renderFilters(
      "/?date_offset=99&start_node=12&end_node=01&page=0",
    );

    expect(result.current.filters.filters).toEqual({
      date_offset: 0,
      building: "",
      start_node: "06",
      end_node: "09",
      keyword: "",
      page: 1,
    });
    expect(result.current.filters.isCanonical).toBe(false);
  });

  it("只给一半区间时另一半取预填值", () => {
    const { result } = renderFilters("/?start_node=01");

    expect(result.current.filters.filters.start_node).toBe("01");
    expect(result.current.filters.filters.end_node).toBe("09");
  });

  it("keyword 解析时去掉首尾空白，URL 随之规范化", () => {
    const { result } = renderFilters(
      `/?keyword=${encodeURIComponent("  综合  ")}`,
    );

    expect(result.current.filters.filters.keyword).toBe("综合");
    expect(result.current.filters.isCanonical).toBe(false);
  });

  it("keyword 超过 32 个字符回退为空，恰好 32 个仍保留", () => {
    const kept = renderFilters(
      `/?keyword=${encodeURIComponent("综".repeat(32))}`,
    );
    expect(kept.result.current.filters.filters.keyword).toBe("综".repeat(32));

    const dropped = renderFilters(
      `/?keyword=${encodeURIComponent("综".repeat(33))}`,
    );
    expect(dropped.result.current.filters.filters.keyword).toBe("");
  });

  it("keyword 长度按 rune 计：32 个 emoji 不算超限", () => {
    // "😀" 的 UTF-16 长度是 2，按 rune 只算 1 个字符。
    const emoji = "😀".repeat(32);
    const { result } = renderFilters(`/?keyword=${encodeURIComponent(emoji)}`);

    expect(result.current.filters.filters.keyword).toBe(emoji);
  });
});

describe("useQueryFilters 写入 URL", () => {
  it("setFilters 只改传入字段，并把完整筛选写进 URL", () => {
    const { result } = renderFilters("/");

    act(() => {
      result.current.filters.setFilters({ building: "Q11" });
    });

    expect(result.current.location.search).toBe(
      "?date_offset=0&building=Q11&start_node=06&end_node=09&page=1",
    );
    expect(result.current.filters.filters.building).toBe("Q11");
    expect(result.current.filters.isCanonical).toBe(true);
  });

  it("页码读写走 URL（分页在第 4 项消费）", () => {
    const { result } = renderFilters("/");

    act(() => {
      result.current.filters.setFilters({ page: 3 });
    });

    expect(result.current.location.search).toBe(
      "?date_offset=0&start_node=06&end_node=09&page=3",
    );
    expect(result.current.filters.filters.page).toBe(3);
  });

  it("改非 page 的筛选时页码重置为 1（旧页码可能已越界）", () => {
    const { result } = renderFilters(
      "/?date_offset=0&start_node=06&end_node=09&page=3",
    );

    act(() => {
      result.current.filters.setFilters({ building: "Q11" });
    });

    expect(result.current.filters.filters.page).toBe(1);
    expect(result.current.location.search).toBe(
      "?date_offset=0&building=Q11&start_node=06&end_node=09&page=1",
    );
  });

  it("replaceFilters 写入完整筛选并规范化 URL", () => {
    const { result } = renderFilters("/?utm=1");

    act(() => {
      result.current.filters.replaceFilters({
        date_offset: 2,
        building: "Q10",
        start_node: "01",
        end_node: "05",
        keyword: "综合",
        page: 1,
      });
    });

    expect(result.current.location.search).toBe(
      "?date_offset=2&building=Q10&start_node=01&end_node=05&keyword=%E7%BB%BC%E5%90%88&page=1",
    );
    expect(result.current.filters.isCanonical).toBe(true);
  });
});
