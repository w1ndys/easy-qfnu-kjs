import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import type { QueryFilters } from "../hooks/useQueryFilters";
import { contextFixture, metaFixture } from "../test/fixtures/publicApi";
import { FilterPanel } from "./FilterPanel";

const baseFilters: QueryFilters = {
  date_offset: 0,
  building: "",
  start_node: "01",
  end_node: "05",
  keyword: "",
  page: 1,
};

function renderPanel(overrides: Partial<QueryFilters> = {}) {
  const onFiltersChange = vi.fn();
  const onSearch = vi.fn();
  const filters = { ...baseFilters, ...overrides };

  render(
    // FilterPanel 读 useLocation/useNavigationType 来决定关键词草稿何时让位给 URL，
    // 所以单测也要放在 Router 里（页面里本来就是路由渲染的）。
    <MemoryRouter>
      <FilterPanel
        context={contextFixture}
        meta={metaFixture}
        filters={filters}
        onFiltersChange={onFiltersChange}
        onSearch={onSearch}
      />
    </MemoryRouter>,
  );

  return { onFiltersChange, onSearch, filters };
}

/** 打开 antd Select 的下拉；rc-select 监听 selector 上的 mousedown。 */
async function openSelect(name: string) {
  fireEvent.mouseDown(screen.getByRole("combobox", { name }));
  return screen.findAllByRole("option");
}

describe("日期项", () => {
  it("渲染覆盖偏移 0..10 的 11 个日期项", () => {
    renderPanel();

    const group = screen.getByRole("group", { name: "日期" });
    expect(within(group).getAllByRole("radio")).toHaveLength(11);
  });

  it("标签为今天/明天/目标日的 M-D，基准日取页面上下文", () => {
    renderPanel();

    const group = screen.getByRole("group", { name: "日期" });
    const radios = within(group).getAllByRole("radio");

    expect(radios[0]).toHaveAccessibleName("今天");
    expect(radios[1]).toHaveAccessibleName("明天");
    // context.date = 2026-11-15，偏移 2 天即 11-17。
    expect(radios[2]).toHaveAccessibleName("11-17");
    expect(radios[10]).toHaveAccessibleName("11-25");
  });

  it("点日期项只改 date_offset，不动节次区间（选非当天日期沿用当前时段）", () => {
    const { onFiltersChange } = renderPanel();

    fireEvent.click(screen.getByRole("radio", { name: "11-17" }));

    expect(onFiltersChange).toHaveBeenCalledTimes(1);
    expect(onFiltersChange).toHaveBeenCalledWith({ date_offset: 2 });
    expect(onFiltersChange.mock.calls[0][0]).not.toHaveProperty("start_node");
  });
});

describe("教学楼下拉", () => {
  it("首位是取值为空的「全部」，项数为夹具白名单 + 1", async () => {
    renderPanel();

    const options = await openSelect("教学楼");

    expect(options).toHaveLength(metaFixture.buildings.length + 1);
    expect(options[0]).toHaveTextContent("全部");
    expect(options.map((option) => option.textContent)).toEqual([
      "全部",
      ...metaFixture.buildings.map((building) => building.name),
    ]);
  });

  it("选中白名单楼后把 building_id 写进筛选", async () => {
    const { onFiltersChange } = renderPanel();

    const options = await openSelect("教学楼");
    const target = options.find(
      (option) => option.textContent === "综合教学楼",
    );
    if (!target) {
      throw new Error("未找到综合教学楼选项");
    }
    fireEvent.click(target);

    expect(onFiltersChange).toHaveBeenCalledWith({ building: "Q10" });
  });
});

describe("节次区间", () => {
  it("显示 URL 带来的区间", () => {
    renderPanel({ start_node: "06", end_node: "09" });

    expect(screen.getByText("第 06 节")).toBeInTheDocument();
    expect(screen.getByText("第 09 节")).toBeInTheDocument();
  });

  it("把起点改到终点之后时，终点一起跟过去，避免非法区间", async () => {
    const { onFiltersChange } = renderPanel();

    const options = await openSelect("起始节次");
    const target = options.find((option) => option.textContent === "第 08 节");
    if (!target) {
      throw new Error("未找到第 08 节选项");
    }
    fireEvent.click(target);

    expect(onFiltersChange).toHaveBeenCalledWith({
      start_node: "08",
      end_node: "08",
    });
  });

  it("把终点改到起点之前时，起点一起跟过去", async () => {
    const { onFiltersChange } = renderPanel({
      start_node: "06",
      end_node: "09",
    });

    const options = await openSelect("结束节次");
    const target = options.find((option) => option.textContent === "第 02 节");
    if (!target) {
      throw new Error("未找到第 02 节选项");
    }
    fireEvent.click(target);

    expect(onFiltersChange).toHaveBeenCalledWith({
      start_node: "02",
      end_node: "02",
    });
  });
});

describe("关键词与查询按钮", () => {
  it("回车提交当前关键词（去掉首尾空白）", () => {
    const { onSearch } = renderPanel();

    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "  综合  " } });
    fireEvent.keyDown(input, { key: "Enter", code: "Enter" });

    expect(onSearch).toHaveBeenCalledTimes(1);
    expect(onSearch).toHaveBeenCalledWith("综合");
  });

  it("点击查询按钮与回车走同一个提交出口，并带上当前关键词", () => {
    const { onSearch } = renderPanel();

    const input = screen.getByRole("textbox", { name: "关键词" });
    fireEvent.change(input, { target: { value: "综合" } });
    fireEvent.click(screen.getByRole("button", { name: "查询" }));

    expect(onSearch).toHaveBeenCalledTimes(1);
    expect(onSearch).toHaveBeenCalledWith("综合");
  });
});
