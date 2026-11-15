// 全天状态视图（tasklist 第 5 项；Requirement 4.1–4.4）：
// 房间头 + 12 小节网格。房间标识从路由参数取，日期偏移与教学楼从查询串取，
// 请求固定 view=day；状态名与节次名只来自 /api/meta 的字典与轴（design D11），
// 周次与星期只取响应里的 week/weekday，前端不推算（design D3）。
//
// 加载/空/过期/错误四态与 hint.day_no_data 的页顶提示属于第 6 项的 StateViews；
// 这里只保留与 QueryPage 同款的最小内联加载/失败卡片，避免请求失败时整页空白。

import { Alert, Button, Card, Result, Skeleton, Space, Typography } from "antd";
import type { ReactNode } from "react";
import { useCallback } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import type { ApiError } from "../../api/client";
import { getErrorTitle } from "../../api/errors";
import { queryDay } from "../../api/public";
import type { DayResult } from "../../api/types";
import { NodeGrid } from "../../components/NodeGrid";
import { useApi } from "../../hooks/useApi";
import { useMeta } from "../../hooks/useMeta";
import { MAX_DATE_OFFSET } from "../../hooks/useQueryFilters";

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

/** 最小加载卡片；第 6 项会换成 StateViews 的 LoadingView。 */
function LoadingCard({ text }: { text: string }) {
  return (
    <Card>
      <Skeleton active paragraph={{ rows: 3 }} />
      <Typography.Text type="secondary">{text}</Typography.Text>
    </Card>
  );
}

/** 最小失败卡片：分类文案 + 重试 + request_id（Requirement 1.6）；第 6 项会换成 ErrorView。 */
function ErrorCard({
  error,
  onRetry,
}: {
  error: ApiError;
  onRetry: () => void;
}) {
  return (
    <Card>
      <Alert
        type="error"
        showIcon
        title={getErrorTitle(error.code)}
        description={error.message || "请求失败，请稍后重试。"}
        action={
          <Button size="small" onClick={onRetry}>
            重试
          </Button>
        }
      />
      <Typography.Text
        type="secondary"
        style={{ display: "block", marginTop: 8 }}
      >
        request_id：{error.requestId}
      </Typography.Text>
    </Card>
  );
}

export function DayPage() {
  const { roomId } = useParams<{ roomId: string }>();
  const [searchParams] = useSearchParams();

  // 只输入空格的房号等价于没填（后端 parseDayParams 也 trim 后判空），这里先归一。
  const room = (roomId ?? "").trim();
  const dateOffset = readDateOffset(searchParams.get("date_offset"));
  const building = searchParams.get("building")?.trim() ?? "";

  const {
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
    body = <LoadingCard text="正在加载页面上下文与数据版本…" />;
  } else if (metaError !== null) {
    body = <ErrorCard error={metaError} onRetry={reloadMeta} />;
  } else if (day.data !== null && meta !== null) {
    body = (
      <>
        <RoomHeader data={day.data} />
        <Card style={{ marginTop: 16 }} title="逐节状态">
          <NodeGrid axis={meta.axis} dict={meta.dict} nodes={day.data.nodes} />
        </Card>
      </>
    );
  } else if (day.error !== null) {
    body = <ErrorCard error={day.error} onRetry={day.reload} />;
  } else {
    body = <LoadingCard text="正在加载该教室的全天状态…" />;
  }

  return (
    <main style={{ padding: 24, maxWidth: 960, margin: "0 auto" }}>
      <Typography.Title level={2}>全天状态</Typography.Title>
      {body}
    </main>
  );
}
