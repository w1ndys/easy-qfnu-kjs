// 单小节状态格（tasklist 第 5 项；Requirement 4.1 / 4.2 / 8.5）：
// 一个小节同时用文字、图标与颜色表达状态，颜色不作为唯一区分手段。
// 状态名只来自 /api/meta 的字典（design D11），字典缺键时回退显示状态键本身；
// 状态名不写死，「无数据/可用性」是需求指定的展示文案。
//
// 「无数据」不是字典状态：后端把无观测小节标成 has_data=false、state_key 为空，
// 并给出查询侧兜底标签（backend/internal/query/day.go 的 NodeLabelNoData）。
// Requirement 4.2 要求这时显示「无数据」并呈现为不可用样式，所以它不进字典查找。

import { theme, Typography, type GlobalToken } from "antd";
import type { DayNode, MetaDict } from "../api/types";

/** 无观测小节的展示文案；与 backend/internal/query/day.go 的 NodeLabelNoData 一致。 */
const NO_DATA_LABEL = "无数据";

/** 三种视觉档位：有数据且可用 / 有数据但不可用 / 无数据（不可用样式）。 */
export type StatusTone = "available" | "unavailable" | "no-data";

export interface StatusCellProps {
  /** 节次展示名，如「第 01 节」；由 NodeGrid 从轴派生（design D11）。 */
  nodeLabel: string;
  /** 该小节的逐节状态；响应里没有这一节时传 null，按无数据处理。 */
  state: DayNode | null;
  /** 状态字典，状态名的唯一来源（design D11）。 */
  dict: MetaDict | null;
}

interface TonePalette {
  background: string;
  border: string;
  color: string;
}

/**
 * 每档的颜色都取自 antd token，不写死色值，暗色主题（大屏）下同样成立。
 * 「无数据」用禁用色而不是错误色：它表达的是「没有观测」，不该和「有观测但被占用」混同。
 */
function tonePalette(tone: StatusTone, token: GlobalToken): TonePalette {
  switch (tone) {
    case "available":
      return {
        background: token.colorSuccessBg,
        border: token.colorSuccessBorder,
        color: token.colorSuccessText,
      };
    case "unavailable":
      return {
        background: token.colorErrorBg,
        border: token.colorErrorBorder,
        color: token.colorErrorText,
      };
    case "no-data":
      return {
        background: token.colorBgContainerDisabled,
        border: token.colorBorderSecondary,
        color: token.colorTextDisabled,
      };
  }
}

/**
 * 状态图标：自绘 SVG，不新增图标依赖（与 design D7 自绘热力图同一取舍）。
 * 图标是装饰（aria-hidden）：可读名称由整格的 aria-label 与格内文字承担。
 */
function StatusIcon({ tone }: { tone: StatusTone }) {
  return (
    <svg
      width={16}
      height={16}
      viewBox="0 0 16 16"
      aria-hidden="true"
      style={{ flex: "0 0 auto" }}
    >
      {tone === "available" ? (
        <path
          d="M2.5 8.5 6 12l7.5-8"
          fill="none"
          stroke="currentColor"
          strokeWidth={2}
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      ) : tone === "unavailable" ? (
        <path
          d="m4 4 8 8M12 4l-8 8"
          fill="none"
          stroke="currentColor"
          strokeWidth={2}
          strokeLinecap="round"
        />
      ) : (
        <path
          d="M3 8h10"
          fill="none"
          stroke="currentColor"
          strokeWidth={2}
          strokeLinecap="round"
        />
      )}
    </svg>
  );
}

/**
 * 状态名解析（design D11）：字典优先；字典缺键（或键下没给展示名）时回退显示状态键，
 * 不拿响应里的 label 顶替，也不写死状态名——字典整体升级后页面自动跟着换名。
 * （「无数据」与「可用/不可用」不在此列，见文件头的需求文案说明。）
 */
function resolveLabel(state: DayNode, dict: MetaDict | null): string {
  const dictState = dict?.states[state.state_key];
  return dictState?.label || state.state_key;
}

/**
 * 一个小节的状态格。
 * `has_data=false` 与「响应里缺这一节」都按无数据渲染：两者对读者是同一件事（这节看不到观测），
 * 差别只在于前者是服务端说的、后者是网格对照轴补出来的。
 */
export function StatusCell({ nodeLabel, state, dict }: StatusCellProps) {
  const { token } = theme.useToken();

  const hasData = state !== null && state.has_data;
  const available = hasData && state.available;
  const tone: StatusTone = !hasData
    ? "no-data"
    : available
      ? "available"
      : "unavailable";
  const label = hasData ? resolveLabel(state, dict) : NO_DATA_LABEL;
  const availabilityText = available ? "可用" : "不可用";
  const palette = tonePalette(tone, token);

  return (
    <div
      role="listitem"
      // 读屏时一次听到「第几节 · 状态名 · 可用性」，不依赖颜色。
      aria-label={`${nodeLabel}：${label}（${availabilityText}）`}
      aria-disabled={available ? undefined : true}
      data-status={tone}
      style={{
        display: "grid",
        alignContent: "start",
        gap: 6,
        padding: "10px 12px",
        borderRadius: token.borderRadius,
        border: `1px solid ${palette.border}`,
        background: palette.background,
        color: palette.color,
        // 无数据格整体降透明度：在颜色之外再给一层可感知的「不可用」信号。
        opacity: tone === "no-data" ? 0.65 : 1,
      }}
    >
      <span style={{ color: token.colorTextSecondary, fontSize: 12 }}>
        {nodeLabel}
      </span>
      <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
        <StatusIcon tone={tone} />
        <Typography.Text strong style={{ color: palette.color }}>
          {label}
        </Typography.Text>
      </span>
      <span style={{ fontSize: 12 }}>{availabilityText}</span>
    </div>
  );
}
