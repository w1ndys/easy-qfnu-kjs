// 全天状态视图（tasklist 第 5–6 项；Requirement 4.1–4.5、5）：
// 房间头 + 12 小节网格 + 四态。房间标识从路由参数取，日期偏移与教学楼从查询串取，
// 请求固定 view=day；状态名与节次名只来自 /api/meta 的字典与轴（design D11），
// 周次与星期只取响应里的 week/weekday，前端不推算（design D3）。
//
// 四态与空教室页共用 StateViews：加载 Skeleton、错误 Result、过期/非教学周 warning。
// 提示标记一律在页顶用 Alert 说明原因（Requirement 4.5 的「页顶提示」口径），
// 房间头始终保留；week_no_schedule 只提示、网格照常，其余标记说明结果为空的原因，
// 不再画一份全是「无数据」的网格。

import { Card, Result, Space, Typography } from "antd";
import type { ReactNode } from "react";
import { useCallback } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { getErrorTitle } from "../../api/errors";
import { queryDay } from "../../api/public";
import type { DayResult } from "../../api/types";
import { NodeGrid } from "../../components/NodeGrid";
import { PageFooter } from "../../components/PageFooter";
import {
  ErrorView,
  HintAlert,
  LoadingView,
  StaleBanner,
} from "../../components/StateViews";
import { useApi } from "../../hooks/useApi";
import { useMeta } from "../../hooks/useMeta";
import { MAX_DATE_OFFSET } from "../../hooks/useQueryFilters";
import { useViewportTier } from "../../hooks/useViewportTier";
import { VISUALLY_HIDDEN } from "../../lib/a11y";
import { getEmptyCopy, HINT_WEEK_NO_SCHEDULE } from "../../lib/hintCopy";

/** 星期展示名：下标即接口的 weekday（1=周一 … 7=周日）；0 表示不在教学周。 */
const WEEKDAY_LABELS: Readonly<Record<number, string>> = {
  1: "周一",
  2: "周二",
  3: "周三",
  4: "周四",
  5: "周五",
  6: "周六",
  7: "周日",
};

const INTEGER_PATTERN = /^-?\d+$/;

/**
 * 读 date_offset：非法值或越界一律回退 0（契约是 0..10）。
 * 与 QueryPage 对非法 URL 参数的口径一致：回退默认值，而不是把非法值发给服务端。
 */
function readDateOffset(raw: string | null): number {
  if (raw === null || !INTEGER_PATTERN.test(raw)) {
    return 0;
  }
  const value = Number(raw);
  return Number.isSafeInteger(value) && value >= 0 && value <= MAX_DATE_OFFSET
    ? value
    : 0;
}

/**
 * 周次与星期文案：两个数字都取自查询响应，这里只做展示格式化，不做任何周历换算（design D3）。
 * week 为 0 表示目标日期不在教学周（backend/internal/query/query.go 的口径）。
 */
function weekLabel(week: number, weekday: number): string {
  if (week <= 0) {
    return "非教学周";
  }
  const day = WEEKDAY_LABELS[weekday];
  return day === undefined ? `第 ${week} 周` : `第 ${week} 周 · ${day}`;
}

/** 房间头：房名、房间标识、楼名、日期，以及响应给出的周次与星期。 */
function RoomHeader({ data }: { data: DayResult }) {
  return (
    <Card>
      {/* 房间不可归属时 name 为空，回落显示请求房号，避免标题空着。 */}
      <Typography.Title level={3} style={{ marginTop: 0 }}>
        {data.name !== "" ? data.name : data.room_id}
      </Typography.Title>
      <Space size={[16, 8]} wrap>
        <Typography.Text type="secondary">
          房间标识：{data.room_id}
        </Typography.Text>
        {data.building_name !== "" ? (
          <Typography.Text type="secondary">
            教学楼：{data.building_name}
          </Typography.Text>
        ) : null}
        <Typography.Text type="secondary">日期：{data.date}</Typography.Text>
        <Typography.Text type="secondary">
          {weekLabel(data.week, data.weekday)}
        </Typography.Text>
      </Space>
    </Card>
  );
}

export function DayPage() {
  const { roomId } = useParams<{ roomId: string }>();
  const [searchParams] = useSearchParams();
  // 与查询页共用三档断点：窄屏收窄内边距，把横向空间让给 12 格状态。
  const padding = useViewportTier() === "compact" ? 16 : 24;

  // 只输入空格的房号等价于没填（后端 parseDayParams 也 trim 后判空），这里先归一。
  const room = (roomId ?? "").trim();
  const dateOffset = readDateOffset(searchParams.get("date_offset"));
  const building = searchParams.get("building")?.trim() ?? "";

  const {
    context,
    meta,
    loading: metaLoading,
    error: metaError,
    reload: reloadMeta,
  } = useMeta();

  const loadDay = useCallback(
    (signal: AbortSignal) =>
      queryDay(
        {
          date_offset: dateOffset,
          room_id: room,
          building: building || undefined,
        },
        signal,
      ),
    [dateOffset, room, building],
  );

  // 请求键随房间/日期/楼变化：换一个地址就取消上一个请求（Requirement 1.5）。
  // 缺房号时 enabled=false，一个全天状态请求都不发（Requirement 4.4 的空态不该有请求）。
  const dayKey = `${room}\u0000${dateOffset}\u0000${building}`;
  const day = useApi(dayKey, loadDay, room !== "");

  let body: ReactNode;
  if (room === "") {
    body = (
      <Result
        status="warning"
        title="缺少房间标识"
        subTitle="全天状态需要指定某间教室；链接可能不完整，回到查询页重新选一间。"
        extra={<Link to="/">返回空教室查询</Link>}
      />
    );
  } else if (metaLoading) {
    body = <LoadingView label="正在加载页面上下文与数据版本…" />;
  } else if (metaError !== null) {
    body = <ErrorView error={metaError} onRetry={reloadMeta} />;
  } else if (day.data !== null && meta !== null) {
    const hint = day.data.hint;
    // 只有 week_no_schedule 与「无提示」保留网格；其余标记说明结果为空的原因，
    // 画 12 个「无数据」格只会重复同一个信息。
    const keepsGrid = hint === undefined || hint.code === HINT_WEEK_NO_SCHEDULE;

    body = (
      <>
        {/* 提示标记放页顶：Requirement 4.5 要求「在页顶显示提示并保留房间信息」。 */}
        {hint !== undefined ? <HintAlert hint={hint} /> : null}
        <RoomHeader data={day.data} />
        {keepsGrid ? (
          <Card style={{ marginTop: 16 }} title="逐节状态">
            <NodeGrid
              axis={meta.axis}
              dict={meta.dict}
              nodes={day.data.nodes}
            />
          </Card>
        ) : null}
      </>
    );
  } else if (day.error !== null) {
    body = <ErrorView error={day.error} onRetry={day.reload} />;
  } else {
    body = <LoadingView label="正在加载该教室的全天状态…" />;
  }

  // 读屏播报：加载 / 结果 / 空态 / 错误四态都从这里出（Requirement 8.6 与查询页同一口径）。
  // 空态标题读 hintCopy 的同一张表，界面与播报不各说一套。
  const announcement = (() => {
    if (room === "") {
      return "缺少房间标识，请回到空教室查询重新选择";
    }
    if (metaLoading) {
      return "正在加载页面上下文与数据版本";
    }
    if (metaError !== null) {
      return `页面加载失败：${getErrorTitle(metaError.code)}`;
    }
    if (day.loading) {
      return "正在加载该教室的全天状态";
    }
    if (day.error !== null) {
      return `全天状态加载失败：${getErrorTitle(day.error.code)}`;
    }
    if (day.data === null) {
      return "";
    }
    const { hint, week, weekday, nodes } = day.data;
    const availableCount = nodes.filter((node) => node.available).length;
    if (hint !== undefined) {
      const title = getEmptyCopy(hint).title;
      // day_no_data 时网格不再画，播报只说原因；week_no_schedule 仍展示网格，补上可用节数。
      return hint.code === HINT_WEEK_NO_SCHEDULE
        ? `${title}；共 ${availableCount} 节可用`
        : `查询结果：${title}`;
    }
    return `查询结果：${weekLabel(week, weekday)}，共 ${availableCount} 节可用`;
  })();

  return (
    <>
      <main
        style={{
          padding,
          paddingBottom: padding,
          maxWidth: 960,
          margin: "0 auto",
          boxSizing: "border-box",
        }}
      >
        <Typography.Title level={2}>全天状态</Typography.Title>
        <div
          role="status"
          aria-live="polite"
          aria-label="全天状态播报"
          style={VISUALLY_HIDDEN}
        >
          {announcement}
        </div>
        <StaleBanner
          week={day.data?.week}
          weeks={meta?.weeks}
          inCalendar={context?.in_calendar}
        />
        {body}
      </main>

      {/*
        页脚必须在 <main> 之外：<footer> 只有不嵌在 main/section 等分区里才映射成
        contentinfo，放在 main 里对真实读屏只是普通元素（jsdom 的 role 查询看不出来）。
      */}
      <div
        style={{
          maxWidth: 960,
          margin: "0 auto",
          padding: `0 ${padding}px ${padding}px`,
          boxSizing: "border-box",
        }}
      >
        <PageFooter />
      </div>
    </>
  );
}
