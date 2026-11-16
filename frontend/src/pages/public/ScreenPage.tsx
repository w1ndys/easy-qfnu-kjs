// 数据大屏（tasklist 第 7–9 项；Requirement 6.1–6.6、7.1–7.6、PRD 6.4）：
// 在 1920×1080 设计稿上按 12 列 × 8 行分区，顺序固定、窗口变化只等比缩放。
//
// 数据来源：
// - `/api/screen` 由 `useScreenPolling` 在可见时每 30 秒请求一次（第 9 项）：
//   在途与失败都保留上一次成功的数据，失败只在顶栏挂离线徽标，成功自动清除；
// - `/api/meta` 只取 `axis` 给热力图列头用（design D12）：首屏与 screen 并行请求一次，
//   之后不随轮询刷新；meta 失败不影响大屏，列头回退「大节 1..5」；
// - 大屏不渲染业务码、message 或错误原文（Requirement 7.6），失败时只有中性占位与重试。

import { Button, theme } from "antd";
import { useCallback, type CSSProperties, type ReactNode } from "react";
import type { ApiResult } from "../../api/client";
import { getMeta } from "../../api/public";
import type { MetaAxis, MetaResult, ScreenResult } from "../../api/types";
import { OccupancyBlock } from "../../components/OccupancyHeatmap";
import {
  AnomalyBlock,
  ReleaseBlock,
  RunBlock,
  ScaleBlock,
  WeekFreshnessTable,
} from "../../components/ScreenBlocks";
import { LoadingView, OfflineBadge } from "../../components/StateViews";
import { useApi } from "../../hooks/useApi";
import {
  useScreenPolling,
  type ScreenPollingState,
} from "../../hooks/useScreenPolling";
import { formatShanghaiDateTime } from "../../lib/datetime";

/** 1920×1080 设计稿的栅格参数：12 列 × 8 行、间距 24px、外边距 40px（design「大屏布局」）。 */
const GRID_GAP = 24;
const CANVAS_PADDING = 40;

/**
 * 12×8 网格里的五个区域。
 * 值即 `grid-area` 的 `行起 / 列起 / 行止 / 列止`；顺序固定，窗口变化不改变它们，
 * 因此区块的相对位置与顺序只由这张表决定（正确性属性 8）。
 */
const AREAS = {
  /** 页头：1 行通栏（标题 · 最后刷新时间 · 离线徽标）。 */
  header: "1 / 1 / 2 / 13",
  /** 左上：站点规模（3 行）。 */
  westTop: "2 / 1 / 5 / 6",
  /** 右上：数据版本 + 逐周新鲜度（3 行）。 */
  eastTop: "2 / 6 / 5 / 13",
  /** 左下：最近一轮 + 异常计数（4 行）。 */
  westBottom: "5 / 1 / 9 / 6",
  /** 右下：占用热力图与按楼占用率（4 行）。 */
  eastBottom: "5 / 6 / 9 / 13",
  /** 加载与失败占位：盖上整个内容区，不画半截数据。 */
  content: "2 / 1 / 9 / 13",
} as const;

const GRID_STYLE: CSSProperties = {
  width: "100%",
  height: "100%",
  display: "grid",
  gridTemplateColumns: "repeat(12, minmax(0, 1fr))",
  gridTemplateRows: "repeat(8, minmax(0, 1fr))",
  gap: GRID_GAP,
  padding: CANVAS_PADDING,
  boxSizing: "border-box",
};

/** 同一格内纵向叠放多个区块：间距与栅格间距一致。 */
const STACKED_AREA: CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: GRID_GAP,
  minWidth: 0,
  minHeight: 0,
};

/** 只为热力图列头取一次轴定义（design D12）；失败时由 OccupancyBlock 回退列头。 */
function loadMeta(signal: AbortSignal): Promise<ApiResult<MetaResult>> {
  return getMeta(signal);
}

/**
 * 页头：标题 + 最后刷新时间 + 离线徽标（design「大屏布局」）。
 *
 * 最后刷新时间取轮询成功时的客户端接收时刻，按 `Asia/Shanghai` 展示（Requirement 7.3、
 * 正确性属性 7：时间只走 lib/datetime.ts）。还没有成功过时显示「尚未成功刷新」，
 * 此时若首屏失败，徽标与内容区的错误占位会同时出现，屏幕上不会伪装成空数据。
 */
function ScreenHeader({
  lastSuccessAt,
  offline,
}: {
  lastSuccessAt: string | null;
  offline: boolean;
}) {
  const { token } = theme.useToken();
  const lastRefresh = formatShanghaiDateTime(lastSuccessAt);

  return (
    <header
      data-testid="screen-header"
      data-grid-area={AREAS.header}
      style={{
        gridArea: AREAS.header,
        display: "flex",
        alignItems: "center",
        justifyContent: "space-between",
        gap: 24,
        minWidth: 0,
      }}
    >
      <h1 style={{ margin: 0, fontSize: 40, fontWeight: 700 }}>数据大屏</h1>
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: 16,
          minWidth: 0,
        }}
      >
        <p
          data-testid="screen-last-refresh"
          style={{ margin: 0, fontSize: 18, color: token.colorTextSecondary }}
        >
          {lastRefresh === null
            ? "尚未成功刷新"
            : `最后刷新 ${lastRefresh}（Asia/Shanghai）`}
        </p>
        {offline && <OfflineBadge />}
      </div>
    </header>
  );
}

function ScreenLoading() {
  return (
    <div
      data-testid="screen-loading"
      data-grid-area={AREAS.content}
      style={{
        gridArea: AREAS.content,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      <LoadingView label="正在加载大屏数据…" rows={5} />
    </div>
  );
}

/**
 * 首屏失败占位：只给中性文案与重试。
 * 不展示 ApiError 的 code / message / request_id——投屏页不承载运维细节（Requirement 7.6）。
 */
function ScreenErrorView({ onRetry }: { onRetry: () => void }) {
  const { token } = theme.useToken();

  return (
    <section
      aria-label="大屏数据加载失败"
      data-testid="screen-error"
      data-grid-area={AREAS.content}
      style={{
        gridArea: AREAS.content,
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        gap: 12,
        borderRadius: 12,
        background: token.colorBgContainer,
        border: `1px solid ${token.colorBorderSecondary}`,
      }}
    >
      <p style={{ margin: 0, fontSize: 24 }}>数据暂时不可用</p>
      <p
        style={{
          margin: 0,
          fontSize: 16,
          color: token.colorTextSecondary,
        }}
      >
        请稍后重试。
      </p>
      <Button type="primary" onClick={onRetry} autoInsertSpace={false}>
        重试
      </Button>
    </section>
  );
}

/** 有数据时的五个区块；顺序与 AREAS 一致，DOM 顺序即阅读顺序。 */
function ScreenContent({
  data,
  axis,
}: {
  data: ScreenResult;
  axis: MetaAxis | null;
}) {
  return (
    <>
      <div
        data-testid="screen-area-scale"
        data-grid-area={AREAS.westTop}
        style={{ ...STACKED_AREA, gridArea: AREAS.westTop }}
      >
        <ScaleBlock data={data.scale} empty={data.empty} />
      </div>

      <div
        data-testid="screen-area-version"
        data-grid-area={AREAS.eastTop}
        style={{ ...STACKED_AREA, gridArea: AREAS.eastTop }}
      >
        <ReleaseBlock release={data.release} empty={data.empty} />
        <WeekFreshnessTable weeks={data.weeks} empty={data.empty} />
      </div>

      <div
        data-testid="screen-area-run"
        data-grid-area={AREAS.westBottom}
        style={{ ...STACKED_AREA, gridArea: AREAS.westBottom }}
      >
        <RunBlock run={data.last_run} empty={data.empty} />
        <AnomalyBlock data={data.anomalies} empty={data.empty} />
      </div>

      <div
        data-testid="screen-area-occupancy"
        data-grid-area={AREAS.eastBottom}
        style={{ ...STACKED_AREA, gridArea: AREAS.eastBottom }}
      >
        <OccupancyBlock
          occupancy={data.occupancy}
          axis={axis}
          empty={data.empty}
        />
      </div>
    </>
  );
}

function ScreenBody({
  screen,
  axis,
  onRetry,
}: {
  screen: ScreenPollingState;
  axis: MetaAxis | null;
  /** 错误页的重试：同时重发主数据与轴定义（见 ScreenPage 里的说明）。 */
  onRetry: () => void;
}): ReactNode {
  if (screen.data !== null) {
    return <ScreenContent data={screen.data} axis={axis} />;
  }
  if (screen.error !== null) {
    return <ScreenErrorView onRetry={onRetry} />;
  }
  return <ScreenLoading />;
}

/**
 * 大屏页面：固定 12×8 栅格 + 五个信息区块。
 * 暗色主题与等比缩放由 ScreenLayout 提供（路由里包裹本页），本页只管内容分区。
 */
export function ScreenPage() {
  // 可见时 30 秒轮询一次；隐藏暂停、重新可见立即补一次，失败保留旧数据（第 9 项）。
  const screen = useScreenPolling();
  // 轴只取一次，不参与轮询：轮询只打 /api/screen（design D12）。
  const meta = useApi("screen-meta", loadMeta);

  // 错误页的重试要顺带重发 meta：首屏两个请求可能同时失败，只重发 screen 的话
  // 主数据恢复后列头仍永久停在「大节 1..5」的回退文案，而 meta 单独失败不弹错误页。
  // 因此把 meta.reload 挂到同一个重试入口上；meta 单独失败仍按 design D12 静默回退。
  const { retry: retryScreen } = screen;
  const { reload: reloadMeta } = meta;
  const retry = useCallback(() => {
    retryScreen();
    reloadMeta();
  }, [retryScreen, reloadMeta]);

  return (
    <main aria-label="数据大屏" style={GRID_STYLE}>
      <ScreenHeader
        lastSuccessAt={screen.lastSuccessAt}
        offline={screen.offline}
      />
      <ScreenBody
        screen={screen}
        axis={meta.data?.axis ?? null}
        onRetry={retry}
      />
    </main>
  );
}
