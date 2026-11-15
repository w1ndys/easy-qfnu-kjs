import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { DayNode, MetaAxis } from "../api/types";
import {
  dayFixture,
  metaFixture,
  unorderedAxisMetaFixture,
} from "../test/fixtures/publicApi";
import { NodeGrid } from "./NodeGrid";

/** 渲染网格并取回 12 个小节格（顺序即 DOM 顺序，也就是读者看到的顺序）。 */
function renderGrid(
  axis: MetaAxis | null,
  nodes: DayNode[] = dayFixture.nodes,
): HTMLElement[] {
  render(<NodeGrid axis={axis} dict={metaFixture.dict} nodes={nodes} />);
  return screen.getAllByRole("listitem");
}

describe("小节顺序", () => {
  it("按轴上的 ordinal 排序渲染，不照 axis.nodes 的数组顺序", () => {
    // 夹具把 02 与 05 的 ordinal 对调：05 的 ordinal 是 2、02 的是 5，
    // 所以 05 必须排在 02 前面。照数组顺序或编号升序都得不到这个结果。
    const cells = renderGrid(unorderedAxisMetaFixture.axis);

    expect(cells).toHaveLength(12);
    const expectedOrder = [
      "01",
      "05",
      "03",
      "04",
      "02",
      "06",
      "07",
      "08",
      "09",
      "10",
      "11",
      "12",
    ];
    expectedOrder.forEach((node, index) => {
      expect(
        within(cells[index]).getByText(`第 ${node} 节`),
      ).toBeInTheDocument();
    });
  });

  it("响应里有观测的小节按整格对齐到轴上对应的小节，不按数组位置错位", () => {
    const cells = renderGrid(unorderedAxisMetaFixture.axis);

    // dayFixture 的 nodes 是乱序的，状态必须跟着小节编号走：05 在夹具里是占用
    // （busy）、03 无观测、02 空闲。若按数组位置或排序后位置错位取状态，这三格都会对不上。
    expect(within(cells[1]).getByText("第 05 节")).toBeInTheDocument();
    expect(within(cells[1]).getByText("占用")).toBeInTheDocument();
    expect(within(cells[2]).getByText("第 03 节")).toBeInTheDocument();
    expect(within(cells[2]).getByText("无数据")).toBeInTheDocument();
    expect(within(cells[4]).getByText("第 02 节")).toBeInTheDocument();
    expect(within(cells[4]).getByText("空闲")).toBeInTheDocument();
  });
});

describe("轴缺失与空响应", () => {
  it("axis=null 时回退渲染 12 个「无数据」格，而不是白屏", () => {
    const cells = renderGrid(null, []);

    expect(cells).toHaveLength(12);
    cells.forEach((cell, index) => {
      const node = String(index + 1).padStart(2, "0");
      expect(within(cell).getByText(`第 ${node} 节`)).toBeInTheDocument();
      expect(within(cell).getByText("无数据")).toBeInTheDocument();
      expect(cell).toHaveAttribute("data-status", "no-data");
      // 空网格不能借字典里的可用状态名冒充「整天空闲」。
      expect(within(cell).queryByText("空闲")).toBeNull();
    });
  });

  it("轴在但响应 nodes 为空（无发布版本）时同样渲染 12 个「无数据」格", () => {
    const cells = renderGrid(metaFixture.axis, []);

    expect(cells).toHaveLength(12);
    expect(screen.getAllByText("无数据")).toHaveLength(12);
    cells.forEach((cell) => {
      expect(within(cell).queryByText("空闲")).toBeNull();
    });
  });
});
