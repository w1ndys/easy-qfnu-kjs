import { fireEvent, render, screen, within } from "@testing-library/react";
import { theme } from "antd";
import { describe, expect, it } from "vitest";
import type { MetaAxis } from "../api/types";
import { metaFixture, screenFixture } from "../test/fixtures/publicApi";
import { BuildingOccupancyList, OccupancyHeatmap } from "./OccupancyHeatmap";

// 占用热力图与按楼占用率（tasklist 第 8 项验收）。
// 三条验收点在这里落地：
// 1) 夹具 7×5 矩阵渲染 35 个格；
// 2) 色阶边界 0 / 0.5 / 1 由 lib/occupancy.test.ts 钉住，这里只验格子上取到的档位；
// 3) 无 meta 时列头回退「大节 1..5」而不报错。
// 另加图例 5 档与可读文本（`<title>` / aria-label / Tooltip）。

const MATRIX = screenFixture.occupancy.by_weekday_block;

function renderHeatmap(axis: MetaAxis | null = metaFixture.axis) {
  return render(<OccupancyHeatmap matrix={MATRIX} axis={axis} />);
}

/**
 * 每格的可读文本。
 * 用 DOM 查询而不是 `getByTitle`：RTL 只把 `<svg>` 的直接子 `<title>` 当作 title，
 * 而这里 `<title>` 是 `<rect>` 的子元素（SVG 规范里每格自己的提示文本）。
 */
function heatmapCellTitles(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll("rect > title")).map(
    (title) => title.textContent ?? "",
  );
}

describe("OccupancyHeatmap 7 天 × 5 大节", () => {
  it("夹具矩阵渲染 7 行 × 5 列共 35 个格", () => {
    const { container } = renderHeatmap();

    expect(screen.getAllByTestId("heatmap-cell")).toHaveLength(35);
    // 只数 rect 不够：行头 7 个、列头 5 个也要真的画出来。
    expect(container.querySelectorAll("svg text")).toHaveLength(12);
  });

  it("每格给出「周几 + 大节 + 占用率」的可读文本，颜色不是唯一信息", () => {
    const { container } = renderHeatmap();
    const titles = heatmapCellTitles(container);

    // 第一行第一列是 0.42；最后一格（周日 第 10–12 节）是 0.01。
    expect(titles).toHaveLength(35);
    expect(titles[0]).toBe("周一 第 01–02 节 42%");
    expect(titles[34]).toBe("周日 第 10–12 节 1%");
  });

  it("色阶档位随占用率变化，0 用中性档（与低占用区分）", () => {
    render(
      <OccupancyHeatmap
        matrix={[
          [0, 0.5, 1, 0.25, 0.2501],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
        ]}
        axis={metaFixture.axis}
      />,
    );

    const buckets = screen
      .getAllByTestId("heatmap-cell")
      .slice(0, 5)
      .map((cell) => cell.getAttribute("data-bucket"));
    // 0→中性档 0；0.5→2；1→4；0.25 仍是 1；0.2501 进 2。
    expect(buckets).toEqual(["0", "2", "4", "1", "2"]);
  });

  it("0 档真的画成中性底色：fill 取 colorBgContainer、fillOpacity 为 1，与正值档不同", () => {
    render(
      <OccupancyHeatmap
        matrix={[
          [0, 0.5, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
          [0, 0, 0, 0, 0],
        ]}
        axis={metaFixture.axis}
      />,
    );

    const [zeroCell, positiveCell] = screen.getAllByTestId("heatmap-cell");
    // 组件不包 ConfigProvider，useToken 取的就是默认 token，可直接比对。
    const { colorBgContainer, colorPrimary } = theme.getDesignToken();

    // 只断言 data-bucket 不够：0 档与 1..4 档的 fill 相同也能过。
    expect(zeroCell).toHaveAttribute("data-bucket", "0");
    expect(zeroCell).toHaveAttribute("fill", colorBgContainer);
    expect(zeroCell).toHaveAttribute("fill-opacity", "1");

    expect(positiveCell).toHaveAttribute("data-bucket", "2");
    expect(positiveCell).toHaveAttribute("fill", colorPrimary);
    expect(positiveCell.getAttribute("fill")).not.toBe(colorBgContainer);
  });

  it("列头由 meta.axis 派生为小节区间", () => {
    renderHeatmap();

    for (const label of [
      "第 01–02 节",
      "第 03–05 节",
      "第 06–07 节",
      "第 08–09 节",
      "第 10–12 节",
    ]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it("没有 meta（axis 为 null）时列头回退「大节 1..5」，35 个格照常渲染", () => {
    const { container } = renderHeatmap(null);

    for (const label of ["大节 1", "大节 2", "大节 3", "大节 4", "大节 5"]) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
    expect(screen.getAllByTestId("heatmap-cell")).toHaveLength(35);
    expect(heatmapCellTitles(container)[0]).toBe("周一 大节 1 42%");
  });

  it("role=img 的概述给出天数、大节数与最高占用率", () => {
    renderHeatmap();

    expect(
      screen.getByRole("img", {
        name: "占用热力图：7 天 5 大节，最高 42%",
      }),
    ).toBeInTheDocument();
  });

  it("图例 5 档（0/25/50/75/100%）并说明未知与复合占用计入已占用", () => {
    renderHeatmap();

    const legend = screen.getByTestId("occupancy-legend");
    for (const label of ["0", "25%", "50%", "75%", "100%"]) {
      expect(within(legend).getByText(label)).toBeInTheDocument();
    }
    expect(
      within(legend).getByText("未知与复合占用计入已占用"),
    ).toBeInTheDocument();
  });

  it("悬停格子时 antd Tooltip 显示与 <title> 相同的可读文本", async () => {
    renderHeatmap();

    const cell = screen.getAllByTestId("heatmap-cell")[0];
    fireEvent.mouseEnter(cell);
    fireEvent.mouseMove(cell);

    const tooltip = await screen.findByRole("tooltip");
    expect(tooltip).toHaveTextContent("周一 第 01–02 节 42%");
  });
});

describe("BuildingOccupancyList 按楼占用率", () => {
  it("显示楼名、占用率与已占/总格数", () => {
    render(
      <BuildingOccupancyList buildings={screenFixture.occupancy.by_building} />,
    );

    const rows = screen.getAllByTestId("building-occupancy-row");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]).getByText("综合教学楼")).toBeInTheDocument();
    expect(within(rows[0]).getByText("26%")).toBeInTheDocument();
    expect(within(rows[0]).getByText("3,821 / 14,700")).toBeInTheDocument();
    // 进度条也带读屏名称，不只靠颜色。
    expect(
      screen.getByRole("progressbar", {
        name: "综合教学楼 占用率 26%",
      }),
    ).toBeInTheDocument();
  });

  it("没有按楼数据时给出占位而不是空白区域", () => {
    render(<BuildingOccupancyList buildings={[]} />);

    expect(screen.getByText("暂无按楼占用数据")).toBeInTheDocument();
    expect(screen.queryByTestId("building-occupancy-row")).toBeNull();
  });
});
