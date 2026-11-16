import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { PageFooter } from "./PageFooter";

describe("PageFooter", () => {
  it("在页脚展示数据来源、更新频率与仅供参考说明", () => {
    render(<PageFooter />);

    const footer = screen.getByRole("contentinfo");
    expect(footer).toHaveTextContent("数据来自学校教务系统");
    expect(footer).toHaveTextContent("每周自动同步");
    expect(footer).toHaveTextContent("仅供参考");
  });
});
