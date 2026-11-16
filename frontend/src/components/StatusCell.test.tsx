import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { DayNode } from "../api/types";
import { metaFixture } from "../test/fixtures/publicApi";
import { StatusCell, type StatusCellProps } from "./StatusCell";

/** 有观测且可用：响应 label 是当前字典的「空闲」。 */
const FREE_NODE: DayNode = {
  node: "01",
  block: "0102",
  state_key: "free",
  label: "空闲",
  available: true,
  has_data: true,
};

/** 有观测但不可用。 */
const BUSY_NODE: DayNode = {
  node: "04",
  block: "030405",
  state_key: "busy",
  label: "占用",
  available: false,
  has_data: true,
};

/** 无观测：state_key 为空、label 为后端兜底的「无数据」。 */
const NO_DATA_NODE: DayNode = {
  node: "03",
  block: "030405",
  state_key: "",
  label: "无数据",
  available: false,
  has_data: false,
};

function renderCell(overrides: Partial<StatusCellProps> = {}) {
  render(
    <StatusCell
      nodeLabel="第 01 节"
      state={FREE_NODE}
      dict={metaFixture.dict}
      {...overrides}
    />,
  );
  return screen.getByRole("listitem");
}

describe("有观测的小节", () => {
  it("同时用文字、图标与可用性表达状态，颜色不是唯一手段", () => {
    const cell = renderCell();

    expect(within(cell).getByText("第 01 节")).toBeInTheDocument();
    expect(within(cell).getByText("空闲")).toBeInTheDocument();
    expect(within(cell).getByText("可用")).toBeInTheDocument();
    // 图标是装饰，但必须有：文字之外还有图形信号。
    expect(cell.querySelector("svg")).not.toBeNull();
    expect(cell).toHaveAttribute("data-status", "available");
    expect(cell).not.toHaveAttribute("aria-disabled");
    // 状态格有可读名称：读屏一次听到「第几节 · 状态名 · 可用性」（Requirement 8.5）。
    expect(cell).toHaveAccessibleName("第 01 节：空闲（可用）");
  });

  it("有数据但不可用时用与「无数据」不同的档位", () => {
    const cell = renderCell({ state: BUSY_NODE });

    expect(within(cell).getByText("占用")).toBeInTheDocument();
    expect(within(cell).getByText("不可用")).toBeInTheDocument();
    expect(cell).toHaveAttribute("data-status", "unavailable");
    expect(cell).toHaveAttribute("aria-disabled", "true");
    expect(within(cell).queryByText("无数据")).toBeNull();
  });

  it("状态名取字典而不是响应里的 label", () => {
    // 模拟字典整体升级：字典（v2）叫「空堂」，响应里还是旧字典的「空闲」。
    const cell = renderCell({
      dict: {
        version: 2,
        states: { free: { label: "空堂", available: true } },
      },
    });

    expect(within(cell).getByText("空堂")).toBeInTheDocument();
    expect(within(cell).queryByText("空闲")).toBeNull();
  });

  it("字典缺键时回退显示状态键本身", () => {
    const cell = renderCell({
      state: { ...FREE_NODE, state_key: "maintenance", label: "" },
    });

    expect(within(cell).getByText("maintenance")).toBeInTheDocument();
  });
});

describe("无观测的小节", () => {
  it("显示「无数据」与不可用样式，不显示任何字典状态名", () => {
    const cell = renderCell({
      nodeLabel: "第 03 节",
      state: NO_DATA_NODE,
    });

    expect(within(cell).getByText("第 03 节")).toBeInTheDocument();
    expect(within(cell).getByText("无数据")).toBeInTheDocument();
    expect(within(cell).getByText("不可用")).toBeInTheDocument();
    // 「可用」是精确匹配，不会被「不可用」命中；这里断言的是没有出现状态名。
    expect(within(cell).queryByText("空闲")).toBeNull();
    expect(within(cell).queryByText("可用")).toBeNull();
    expect(cell).toHaveAttribute("data-status", "no-data");
    expect(cell).toHaveAttribute("aria-disabled", "true");
    // 可读名称也要说明「不可用」，不依赖颜色或透明度。
    expect(cell).toHaveAccessibleName("第 03 节：无数据（不可用）");
  });

  it("响应里缺这一节时同样按无数据处理", () => {
    const cell = renderCell({ state: null });

    expect(within(cell).getByText("无数据")).toBeInTheDocument();
    expect(cell).toHaveAttribute("data-status", "no-data");
  });
});
