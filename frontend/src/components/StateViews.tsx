// 加载 / 空 / 过期 / 错误四态（tasklist 第 6 项；design「四态」表与 Requirement 5）。
// 只做展示：判定（哪个态、hint 是哪档、周次是否过期）由页面按响应字段决定，
// 四档文案集中在 lib/hintCopy.ts，页面与读屏播报不各写一套。
//
// 组件边界：
// - LoadingView：结果区的 Skeleton，放在结果卡里，天然不遮筛选（Requirement 5.1）；
// - EmptyView：antd Empty + 按 hint.code 四档文案 + 一条可操作建议；
// - HintAlert：week_no_schedule 的置顶提示（结果照常展示，不换成空态）；
// - StaleBanner：过期或当前不在教学周的 warning，数据照常展示（Requirement 5.3/5.4）；
// - ErrorView：分类文案 + 重试 + request_id（Requirement 5.5）。

import {
  Alert,
  Button,
  Empty,
  Result,
  Skeleton,
  Space,
  Typography,
} from "antd";
import type { ApiError } from "../api/client";
import { getErrorTitle } from "../api/errors";
import type { AvailabilityHint, WeekFreshness } from "../api/types";
import { formatShanghaiDateTime } from "../lib/datetime";
import { getEmptyCopy } from "../lib/hintCopy";

export interface LoadingViewProps {
  /** 加载文案；不同位置（元数据、查询、全天状态）用不同说法。 */
  label?: string;
  /** 骨架段落行数。 */
  rows?: number;
}

/**
 * 结果区加载态：Skeleton + 一行说明。
 * 它只占结果区，不整页遮罩，也不 disable 任何筛选控件。
 */
export function LoadingView({
  label = "正在加载…",
  rows = 3,
}: LoadingViewProps) {
  return (
    <div aria-busy="true">
      <Skeleton active title={false} paragraph={{ rows }} />
      <Typography.Text
        type="secondary"
        style={{ display: "block", marginTop: 8 }}
      >
        {label}
      </Typography.Text>
    </div>
  );
}

export interface EmptyViewProps {
  /** 查询响应的提示标记；缺省时走通用兜底文案。 */
  hint?: AvailabilityHint | null;
  /** meta.empty：没有当前数据版本，与提示标记一样属于空态（design「四态」）。 */
  noRelease?: boolean;
}

/**
 * 空态：antd Empty + 按 hint.code 选文案 + 一句可操作建议。
 * 这里不渲染任何结果列表，调用方也不该在空态下面再拼一份旧列表。
 */
export function EmptyView({ hint, noRelease = false }: EmptyViewProps) {
  const copy = getEmptyCopy(hint, noRelease);

  return (
    <Empty
      image={Empty.PRESENTED_IMAGE_SIMPLE}
      description={
        <Space orientation="vertical" size={4}>
          <Typography.Text strong>{copy.title}</Typography.Text>
          <Typography.Text type="secondary">{copy.suggestion}</Typography.Text>
        </Space>
      }
    />
  );
}

export interface HintAlertProps {
  /** 需要置顶展示的提示标记；文案与 EmptyView 共用同一张表。 */
  hint: AvailabilityHint;
}

/**
 * 置顶提示（week_no_schedule 与全天状态的部分标记用）。
 * 与 EmptyView 的区别只在于位置与容器：结果照常展示时用 Alert，不替换结果区。
 */
export function HintAlert({ hint }: HintAlertProps) {
  const copy = getEmptyCopy(hint);
  return (
    <Alert
      type="warning"
      showIcon
      title={copy.title}
      description={copy.suggestion}
      style={{ marginBottom: 16 }}
    />
  );
}

export interface StaleBannerProps {
  /** 查询响应里的教学周次；0 或缺省表示非教学周，不参与过期判定。 */
  week?: number;
  /** meta.weeks：逐周发布与新鲜度；缺失时无法判定过期，只保留非教学周提示。 */
  weeks?: WeekFreshness[];
  /** context.in_calendar：false 时提示当前不在教学周（Requirement 5.4）。 */
  inCalendar?: boolean;
}

/**
 * 过期 / 非教学周提示（Requirement 5.3、5.4）。
 *
 * 过期判定只用响应里的 `week` 去命中 `meta.weeks`，命中且 `stale=true` 才提示；
 * 前端不按时间猜某周是否过期（那是后端按 8 天窗口算的）。
 * 两种提示都只提醒，不隐藏、不清空任何数据。
 */
export function StaleBanner({ week, weeks, inCalendar }: StaleBannerProps) {
  const matchedWeek =
    week === undefined || week <= 0
      ? undefined
      : weeks?.find((item) => item.week === week);
  const staleWeek = matchedWeek?.stale === true ? matchedWeek : undefined;

  // 最近成功时间按 Asia/Shanghai 展示；没有记录或解析失败时说明「还没有成功记录」。
  const lastSuccess =
    staleWeek === undefined
      ? null
      : formatShanghaiDateTime(staleWeek.last_success_at);

  const alerts = [];
  if (staleWeek !== undefined) {
    alerts.push(
      <Alert
        key="stale"
        type="warning"
        showIcon
        title={`第 ${staleWeek.week} 周数据已过期`}
        description={
          lastSuccess === null
            ? "该周还没有成功同步记录。数据仍按现有版本展示，仅供参考。"
            : `该周最近一次成功同步：${lastSuccess}（Asia/Shanghai）。数据仍按现有版本展示，仅供参考。`
        }
      />,
    );
  }
  if (inCalendar === false) {
    alerts.push(
      <Alert
        key="calendar"
        // 与过期提示同为警示级（design「四态」表把 in_calendar=false 归在过期一行）。
        type="warning"
        showIcon
        // in_calendar 是 /api/context 按**今天**算的（backend/internal/query/query.go 的
        // Context）：它说明不了所选日期在不在教学周，所以文案只说「今天」，把
        // 「所选日期查不到数据」写成可能性，而不是断定所选日期也在学期外。
        title="当前不在教学周"
        description="今天不在本学期教学周内，所选日期可能查不到数据；已有数据照常展示。"
      />,
    );
  }

  if (alerts.length === 0) {
    return null;
  }

  return (
    <div style={{ display: "grid", gap: 12, marginBottom: 16 }}>{alerts}</div>
  );
}

export interface ErrorViewProps {
  /** 请求失败：业务码、request_id 与可作副标题的 message。 */
  error: ApiError;
  /** 重试出口；由页面决定重发哪一次请求。 */
  onRetry: () => void;
  /** 冷却中（42901）：重试按钮也禁用，避免撞回同一个限流。 */
  retryDisabled?: boolean;
}

/**
 * 错误态：分类文案（只按后端业务码映射，design D10）+ 重试 + request_id。
 * 错误态不渲染上一次结果——是否残留旧列表由页面保证（useApi 在错误时 data 为 null）。
 */
export function ErrorView({
  error,
  onRetry,
  retryDisabled = false,
}: ErrorViewProps) {
  const title = getErrorTitle(error.code);
  // message 只作副标题：与主文案重复时不展示，免得同一句话出现两遍。
  const subtitle =
    error.message !== "" && error.message !== title
      ? error.message
      : "请稍后重试；若持续失败，请把 request_id 提供给管理员。";

  return (
    <Result
      status="warning"
      title={title}
      subTitle={subtitle}
      extra={
        <Space orientation="vertical" size={8}>
          <Button
            type="primary"
            onClick={onRetry}
            disabled={retryDisabled}
            // 与查询按钮一致：两个汉字的按钮不插空格，可访问名保持「重试」。
            autoInsertSpace={false}
          >
            重试
          </Button>
          <Typography.Text
            type="secondary"
            copyable={{ text: error.requestId }}
            style={{ fontSize: 12 }}
          >
            request_id：{error.requestId}
          </Typography.Text>
        </Space>
      }
    />
  );
}
