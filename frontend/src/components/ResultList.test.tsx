import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";
import type { AvailabilityRoom } from "../api/types";
import { duplicateNameAvailabilityFixture } from "../test/fixtures/publicApi";
import { ResultList, type ResultListProps } from "./ResultList";

const items: AvailabilityRoom[] = duplicateNameAvailabilityFixture.items;

function renderList(overrides: Partial<ResultListProps> = {}) {
  render(
    <MemoryRouter>
      <ResultList items={items} dateOffset={2} building="" {...overrides} />
    </MemoryRouter>,
  );
}

describe("行内容", () => {
  it("每行显示房名、房间标识与所属教学楼", () => {
    renderList();

    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(
      within(rows[0]).getByText("综合楼101（Q12-101）"),
    ).toBeInTheDocument();
    expect(within(rows[0]).getByText("房间标识：Q12-101")).toBeInTheDocument();
    expect(within(rows[0]).getByText("教学楼：文科教学楼")).toBeInTheDocument();
  });

  it("同名房名追加房间标识后缀，渲染出不同标签；异名房不加后缀", () => {
    renderList();

    const rows = screen.getAllByRole("listitem");
    expect(
      within(rows[0]).getByText("综合楼101（Q12-101）"),
    ).toBeInTheDocument();
    expect(within(rows[1]).getByText("理工楼302")).toBeInTheDocument();
    expect(
      within(rows[2]).getByText("综合楼101（Q10-101）"),
    ).toBeInTheDocument();
  });
});

describe("结果顺序", () => {
  it("按服务端返回顺序渲染，不做重排", () => {
    renderList();

    const rows = screen.getAllByRole("listitem");
    expect(rows.map((row) => row.textContent)).toEqual([
      expect.stringContaining("综合楼101（Q12-101）"),
      expect.stringContaining("理工楼302"),
      expect.stringContaining("综合楼101（Q10-101）"),
    ]);
    // 第一行与第三行是同一房名，若前端按房名排序就不会被异名房隔开。
    expect(rows[0]).toHaveTextContent("Q12-101");
    expect(rows[1]).toHaveTextContent("Q11-302");
    expect(rows[2]).toHaveTextContent("Q10-101");
  });
});

describe("看全天状态入口", () => {
  it("跳 /day/:roomId 并带上当前 date_offset 与 building", () => {
    renderList({ dateOffset: 3, building: "Q10" });

    expect(
      screen.getByRole("link", { name: "看 综合楼101（Q12-101） 的全天状态" }),
    ).toHaveAttribute("href", "/day/Q12-101?date_offset=3&building=Q10");
    expect(
      screen.getByRole("link", { name: "看 理工楼302 的全天状态" }),
    ).toHaveAttribute("href", "/day/Q11-302?date_offset=3&building=Q10");
  });

  it("教学楼为「全部」时不写空的 building 参数", () => {
    renderList({ dateOffset: 0, building: "" });

    expect(
      screen.getByRole("link", { name: "看 综合楼101（Q10-101） 的全天状态" }),
    ).toHaveAttribute("href", "/day/Q10-101?date_offset=0");
  });

  it("每行都有入口，房间标识做路径参数", () => {
    renderList();

    const links = screen.getAllByRole("link", { name: /的全天状态$/ });
    expect(links).toHaveLength(items.length);
    expect(links.map((link) => link.getAttribute("href"))).toEqual([
      "/day/Q12-101?date_offset=2",
      "/day/Q11-302?date_offset=2",
      "/day/Q10-101?date_offset=2",
    ]);
  });
});
