import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import type { AvailabilityHint, WeekFreshness } from "../api/types";
import {
  EmptyView,
  ErrorView,
  HintAlert,
  LoadingView,
  OfflineBadge,
  StaleBanner,
} from "./StateViews";

/** 过期周夹具：第 11 周已过期，最近一次成功同步是 UTC 07:10（上海 15:10）。 */
const staleWeekFixture = {
  week: 11,
  published: true,
  last_success_at: "2026-11-14T07:10:12Z",
  stale: true,
} satisfies WeekFreshness;

/** 同一周但新鲜的对照夹具。 */
const freshWeekFixture = {
  ...staleWeekFixture,
  stale: false,
} satisfies WeekFreshness;

describe("LoadingView", () => {
  it("渲染 Skeleton 与加载文案，并以 aria-busy 标注结果区正在加载", () => {
    const { container } = render(
      <LoadingView label="正在加载该教室的全天状态…" />,
    );

    expect(container.querySelector(".ant-skeleton")).not.toBeNull();
    expect(screen.getByText("正在加载该教室的全天状态…")).toBeInTheDocument();
    expect(container.firstElementChild).toHaveAttribute("aria-busy", "true");
  });
});

describe("EmptyView 的四档文案", () => {
  it.each([
    ["no_match", "没有符合条件的教室", "换个日期、教学楼或关键词再试"],
    ["no_data", "该日期没有可查询的数据", "换一个本学期的日期，或稍后再来"],
    ["week_no_schedule", "该周疑似无排课", "换一个已发布数据的日期再试"],
    ["day_no_data", "该日无数据", "换一间教室，或看同教室的其他日期"],
  ])("%s 显示「%s」并给出一条可操作建议", (code, title, suggestion) => {
    const hint: AvailabilityHint = { code, message: "后端兜底文案" };
    render(<EmptyView hint={hint} />);

    expect(screen.getByText(title)).toBeInTheDocument();
    expect(screen.getByText(suggestion)).toBeInTheDocument();
    // 后端 message 只作兜底，不替代前端文案。
    expect(screen.queryByText("后端兜底文案")).not.toBeInTheDocument();
  });

  it("未知 hint.code 走通用兜底，不猜后端新码的含义", () => {
    render(<EmptyView hint={{ code: "brand_new_code", message: "新文案" }} />);

    expect(screen.getByText("暂无数据")).toBeInTheDocument();
    expect(screen.getByText("换个日期或教学楼再试")).toBeInTheDocument();
    expect(screen.queryByText("新文案")).not.toBeInTheDocument();
  });

  it("没有提示标记但 meta.empty 时显示「暂无数据版本」", () => {
    render(<EmptyView noRelease />);

    expect(screen.getByText("暂无数据版本")).toBeInTheDocument();
    expect(
      screen.getByText("数据版本还没有发布，稍后再来"),
    ).toBeInTheDocument();
  });
});

describe("HintAlert", () => {
  it("week_no_schedule 用置顶 Alert 展示同一档文案", () => {
    render(<HintAlert hint={{ code: "week_no_schedule", message: "兜底" }} />);

    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByText("该周疑似无排课")).toBeInTheDocument();
    expect(screen.getByText("换一个已发布数据的日期再试")).toBeInTheDocument();
  });
});

describe("StaleBanner 的判定", () => {
  it("响应周次命中 meta.weeks 且 stale=true 时提示过期，并给出最近成功时间", () => {
    render(<StaleBanner week={11} weeks={[staleWeekFixture]} />);

    expect(screen.getByText("第 11 周数据已过期")).toBeInTheDocument();
    // 时间按 Asia/Shanghai 展示：07:10Z → 15:10。
    expect(
      screen.getByText(
        /该周最近一次成功同步：2026-11-14 15:10（Asia\/Shanghai）/,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/数据仍按现有版本展示，仅供参考/),
    ).toBeInTheDocument();
  });

  it("命中但 stale=false 时不提示", () => {
    render(<StaleBanner week={11} weeks={[freshWeekFixture]} />);

    expect(screen.queryByText(/数据已过期/)).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("响应周次不在 meta.weeks 里时不提示（不按时间猜过期）", () => {
    render(<StaleBanner week={3} weeks={[staleWeekFixture]} />);

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("非教学周（week=0）不参与过期判定", () => {
    render(<StaleBanner week={0} weeks={[{ ...staleWeekFixture, week: 0 }]} />);

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("没有最近成功时间时说明该周还没有成功同步记录", () => {
    render(
      <StaleBanner
        week={11}
        weeks={[{ ...staleWeekFixture, last_success_at: null }]}
      />,
    );

    expect(
      screen.getByText(/该周还没有成功同步记录。数据仍按现有版本展示/),
    ).toBeInTheDocument();
  });

  it("in_calendar=false 说的是「今天」不在教学周，不断言所选日期", () => {
    render(<StaleBanner inCalendar={false} />);

    // in_calendar 按今天算（/api/context），所以文案只能提示今天不在学期内、
    // 所选日期「可能」查不到数据——不能把所选日期也说成在学期外。
    expect(screen.getByText("当前不在教学周")).toBeInTheDocument();
    expect(
      screen.getByText(
        "今天不在本学期教学周内，所选日期可能查不到数据；已有数据照常展示。",
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/所选日期不在本学期教学周内/),
    ).not.toBeInTheDocument();
  });

  it("过期与非教学周同时成立时两条提示都出现", () => {
    render(
      <StaleBanner week={11} weeks={[staleWeekFixture]} inCalendar={false} />,
    );

    expect(screen.getByText("第 11 周数据已过期")).toBeInTheDocument();
    expect(screen.getByText("当前不在教学周")).toBeInTheDocument();
  });

  it("既不过期也在教学周时什么都不渲染", () => {
    const { container } = render(
      <StaleBanner week={11} weeks={[freshWeekFixture]} inCalendar />,
    );

    expect(container).toBeEmptyDOMElement();
  });
});

describe("ErrorView", () => {
  it("按业务码显示分类文案，并给出重试与 request_id", () => {
    const onRetry = vi.fn();
    render(
      <ErrorView
        error={new ApiError(50000, "req_50000", "")}
        onRetry={onRetry}
      />,
    );

    expect(screen.getByText("服务异常")).toBeInTheDocument();
    expect(screen.getByText("request_id：req_50000")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("42901 显示限流文案", () => {
    render(
      <ErrorView
        error={new ApiError(42901, "req_rate_limited", "")}
        onRetry={() => {}}
      />,
    );

    expect(screen.getByText("请求过于频繁，请稍后重试")).toBeInTheDocument();
    expect(
      screen.getByText("request_id：req_rate_limited"),
    ).toBeInTheDocument();
  });

  it("后端 message 与主文案不同时作为副标题展示", () => {
    render(
      <ErrorView
        error={
          new ApiError(40001, "req_40001", "date_offset 必须是 0 到 10 的整数")
        }
        onRetry={() => {}}
      />,
    );

    expect(screen.getByText("参数有误")).toBeInTheDocument();
    expect(
      screen.getByText("date_offset 必须是 0 到 10 的整数"),
    ).toBeInTheDocument();
  });

  it("message 与主文案重复时只出现一次", () => {
    render(
      <ErrorView
        error={new ApiError(50000, "req_dup", "服务异常")}
        onRetry={() => {}}
      />,
    );

    expect(screen.getAllByText("服务异常")).toHaveLength(1);
  });

  it("冷却中禁用重试按钮", () => {
    render(
      <ErrorView
        error={new ApiError(42901, "req_cool", "")}
        onRetry={() => {}}
        retryDisabled
      />,
    );

    expect(screen.getByRole("button", { name: "重试" })).toBeDisabled();
  });
});

describe("OfflineBadge", () => {
  it("以 warning 标签、可读文本与 status 角色提示离线", () => {
    render(<OfflineBadge />);

    const badge = screen.getByTestId("offline-badge");
    // 文字承载语义，颜色只作辅助（Requirement 8.5 的口径）。
    expect(badge).toHaveTextContent("离线");
    expect(badge).toHaveAttribute("role", "status");
    expect(badge).toHaveClass("ant-tag-warning");
  });

  it("图标与文字同时表达离线，且图标对读屏是装饰", () => {
    render(<OfflineBadge />);

    const badge = screen.getByTestId("offline-badge");
    const icon = badge.querySelector("svg");
    // Requirement 8.5：状态不能只靠文字 + 颜色，还要有图标。
    expect(icon).not.toBeNull();
    // 图标是装饰：不参与可读名称，也不带 title 之类的文字兜底。
    expect(icon).toHaveAttribute("aria-hidden", "true");
    expect(icon?.querySelector("title")).toBeNull();
    // 可读语义仍只由「离线」二字承担，图标不引入任何额外文本。
    expect(badge.textContent?.trim()).toBe("离线");
  });
});
