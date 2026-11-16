// 数据大屏（tasklist 第 7 项；Requirement 6.1–6.4、6.6、PRD 6.4）：
// 在 1920×1080 设计稿上按 12 列 × 8 行分区，顺序固定、窗口变化只等比缩放。
//
// 本项只负责「布局 + 五个信息区块」：
// - 数据只请求一次（useApi）；30 秒轮询、离线徽标与最后刷新时间留给第 9 项；
// - 占用热力图与按楼占用率属于第 8 项，这里只留一块同样大小的占位区；
// - 大屏不渲染业务码、message 或错误原文（Requirement 7.6），失败时只有中性占位与重试。

import { Button, theme } from "antd";
import type { CSSProperties, ReactNode } from "react";
import type { ApiResult } from "../../api/client";
import { getScreen } from "../../api/public";
import type { ScreenResult } from "../../api/types";
import {
  AnomalyBlock,
  NO_DATA_TEXT,
  ReleaseBlock,
  RunBlock,
  ScaleBlock,
  WeekFreshnessTable,
} from "../../components/ScreenBlocks";
import { LoadingView } from "../../components/StateViews";
import { useApi, type ApiRequestState } from "../../hooks/useApi";

/** 1920×1080 设计稿的栅格参数：12 列 × 8 行、间距 24px、外边距 40px（design「大屏布局」）。 */
const GRID_GAP = 24;
const CANVAS_PADDING = 40;

/**
 * 12×8 网格里的五个区域。
 * 值即 `grid-area` 的 `行起 / 列起 / 行止 / 列止`；顺序固定，窗口变化不改变它们，
 * 因此区块的相对位置与顺序只由这张表决定（正确性属性 8）。
 */
const AREAS = {
  /** 页头：1 行通栏（第 9 项在这里补最后刷新时间与离线徽标）。 */
  header: "1 / 1 / 2 / 13",
  /** 左上：站点规模（3 行）。 */
  westTop: "2 / 1 / 5 / 6",
  /** 右上：数据版本 + 逐周新鲜度（3 行）。 */
  eastTop: "2 / 6 / 5 / 13",
  /** 左下：最近一轮 + 异常计数（4 行）。 */
  westBottom: "5 / 1 / 9 / 6",
  /** 右下：占用热力图与按楼占用率（4 行，第 8 项实现）。 */
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

/** 请求函数放在模块级，引用稳定，useApi 不会因页面重渲染重发请求。 */
function loadScreen(signal: AbortSignal): Promise<ApiResult<ScreenResult>> {
  return getScreen(signal);
}

function ScreenHeader() {
  const { token } = theme.useToken();

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
      <p style={{ margin: 0, fontSize: 18, color: token.colorTextSecondary }}>
        站点规模 · 数据新鲜度 · 同步健康 · 占用分布
      </p>
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

/**
 * 第 8 项的实现位：占位区与热力图区域同尺寸，接管时不需要改栅格。
 *
 * `empty=true`（没有当前数据版本）时与其他五个区块同口径：保留标题、正文换成
 * 「暂无数据」，不显示「尚待接入」——否则整页只有这一块还在说功能没做完。
 */
function OccupancyPlaceholder({ empty }: { empty: boolean }) {
  const { token } = theme.useToken();

  return (
    <section
      aria-label="占用分布"
      data-testid="screen-block-occupancy"
      style={{
        display: "flex",
        flexDirection: "column",
        flex: "1 1 0",
        minWidth: 0,
        minHeight: 0,
        padding: 16,
        borderRadius: 12,
        background: token.colorBgContainer,
        border: `1px solid ${token.colorBorderSecondary}`,
      }}
    >
      <h2
        style={{ margin: 0, fontSize: 22, fontWeight: 600, lineHeight: "30px" }}
      >
        占用分布
      </h2>
      <p
        style={{
          margin: "12px 0 0",
          fontSize: 18,
          color: token.colorTextSecondary,
        }}
      >
        {empty ? NO_DATA_TEXT : "占用热力图与按楼占用率尚待接入。"}
      </p>
    </section>
  );
}

/** 有数据时的五个区块；顺序与 AREAS 一致，DOM 顺序即阅读顺序。 */
function ScreenContent({ data }: { data: ScreenResult }) {
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
        <OccupancyPlaceholder empty={data.empty} />
      </div>
    </>
  );
}

function ScreenBody({
  screen,
}: {
  screen: ApiRequestState<ScreenResult>;
}): ReactNode {
  if (screen.data !== null) {
    return <ScreenContent data={screen.data} />;
  }
  if (screen.error !== null) {
    return <ScreenErrorView onRetry={screen.reload} />;
  }
  return <ScreenLoading />;
}

/**
 * 大屏页面：固定 12×8 栅格 + 五个信息区块。
 * 暗色主题与等比缩放由 ScreenLayout 提供（路由里包裹本页），本页只管内容分区。
 */
export function ScreenPage() {
  // 第 7 项只做一次加载；第 9 项把这一层换成 useScreenPolling，区块组件保持不变。
  const screen = useApi("screen", loadScreen);

  return (
    <main aria-label="数据大屏" style={GRID_STYLE}>
      <ScreenHeader />
      <ScreenBody screen={screen} />
    </main>
  );
}
