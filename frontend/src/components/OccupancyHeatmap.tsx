// 占用分布区块（tasklist 第 8 项；Requirement 6.5、design「占用热力图实现」）。
//
// 三个组件同属一项：
// - `OccupancyHeatmap`：自绘 SVG 的 7 天 × 5 大节热力图 + 5 档图例（D7：不引图表库）；
// - `BuildingOccupancyList`：`by_building` 的按楼占用率列表（楼名 + 占用率 + 已占/总格数）；
// - `OccupancyBlock`：大屏右下角的「占用分布」卡片外壳，空态与其它区块同口径。
//
// 名称来源：列头只来自 `meta.axis`（design D11），轴缺失时回退「大节 1..5」；
// 行头「周一…周日」是矩阵契约里的固定顺序（backend/internal/query/screen.go：
// 行 0 = 周一），不是业务字典，所以不查 meta.dict。
// 颜色不是唯一信息：每格带 `<title>` 可读文本与悬停 Tooltip，图例带百分比刻度。

import { Progress, Tooltip, theme, type GlobalToken } from "antd";
import type {
  MetaAxis,
  ScreenBuildingOccupancy,
  ScreenOccupancy,
} from "../api/types";
import { BLOCK_COUNT, buildBlockLabels } from "../lib/nodes";
import {
  OCCUPANCY_FILL_OPACITY,
  formatRatePercent,
  occupancyBucket,
} from "../lib/occupancy";
import { NoData, ScreenBlock } from "./ScreenBlocks";

/** 大节列数固定 5（矩阵契约）；行数固定 7。 */
const COLUMN_COUNT = BLOCK_COUNT;
const ROW_COUNT = 7;

/** 行头：矩阵行 0 是周一（backend/internal/query/screen.go 的 occupancyMatrix）。 */
const WEEKDAY_LABELS = [
  "周一",
  "周二",
  "周三",
  "周四",
  "周五",
  "周六",
  "周日",
] as const;

/** 与 design 一致的设计稿尺寸；实际显示尺寸由容器等比缩放（preserveAspectRatio）。 */
const VIEW_WIDTH = 700;
const VIEW_HEIGHT = 420;
/** 左侧行头与顶部列头占用的宽度/高度，其余留给 7×5 格。 */
const LABEL_WIDTH = 64;
const HEADER_HEIGHT = 34;
/** 格间距与圆角（design：格间 6px、4px 圆角）。 */
const CELL_GAP = 6;
const CELL_RADIUS = 4;

const GRID_WIDTH = VIEW_WIDTH - LABEL_WIDTH;
const GRID_HEIGHT = VIEW_HEIGHT - HEADER_HEIGHT;
const CELL_WIDTH = (GRID_WIDTH - CELL_GAP * (COLUMN_COUNT - 1)) / COLUMN_COUNT;
const CELL_HEIGHT = (GRID_HEIGHT - CELL_GAP * (ROW_COUNT - 1)) / ROW_COUNT;

const numberFormat = new Intl.NumberFormat("zh-CN");
const MONO_FONT =
  'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace';

/** 第 column 列的 x 坐标。 */
function cellX(column: number): number {
  return LABEL_WIDTH + column * (CELL_WIDTH + CELL_GAP);
}

/** 第 row 行的 y 坐标。 */
function cellY(row: number): number {
  return HEADER_HEIGHT + row * (CELL_HEIGHT + CELL_GAP);
}

/**
 * 取矩阵里一格占用率。
 * 行或列缺失（后端形状异常）按 0 处理：既不让页面崩，也不把缺格误画成高占用；
 * 0 在后端口径里就是「没有观测」（occupancyRate 分母为 0 时给 0）。
 */
function rateAt(matrix: number[][], row: number, column: number): number {
  const value = matrix[row]?.[column];
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

/** 热力图概述：读屏只播报这一句（每格细节在 `<title>` 里）。 */
function heatmapSummary(matrix: number[][]): string {
  const rates = matrix.flat().filter((rate) => Number.isFinite(rate));
  const max = rates.length === 0 ? 0 : Math.max(...rates);
  return `占用热力图：7 天 5 大节，最高 ${formatRatePercent(max)}`;
}

/** 图例的 5 个刻度：0 为中性档，其余对应 25% / 50% / 75% / 100%。 */
const LEGEND_STEPS: ReadonlyArray<{ label: string; bucket: number }> = [
  { label: "0", bucket: 0 },
  { label: "25%", bucket: 1 },
  { label: "50%", bucket: 2 },
  { label: "75%", bucket: 3 },
  { label: "100%", bucket: 4 },
];

/**
 * 色块样式：0 档用中性底色（与「没有占用 / 无观测」对应），
 * 1..4 档用品牌色 + 递增透明度。颜色全部取自 antd token，暗色主题下同样成立。
 */
function swatchStyle(bucket: number, token: GlobalToken) {
  return {
    width: 14,
    height: 14,
    borderRadius: 3,
    flex: "0 0 auto",
    background: bucket === 0 ? token.colorBgContainer : token.colorPrimary,
    opacity: bucket === 0 ? 1 : OCCUPANCY_FILL_OPACITY[bucket],
    border: `1px solid ${token.colorBorderSecondary}`,
  };
}

/** 图例：5 个色块 + 百分比刻度 + 口径说明（design「占用热力图实现」）。 */
function HeatmapLegend() {
  const { token } = theme.useToken();

  return (
    <div
      data-testid="occupancy-legend"
      style={{
        display: "flex",
        flexWrap: "wrap",
        alignItems: "center",
        gap: "6px 14px",
        fontSize: 14,
        color: token.colorTextSecondary,
      }}
    >
      <span>占用率</span>
      <span style={{ display: "flex", alignItems: "center", gap: 10 }}>
        {LEGEND_STEPS.map((step) => (
          <span
            key={step.label}
            style={{ display: "flex", alignItems: "center", gap: 4 }}
          >
            <span aria-hidden="true" style={swatchStyle(step.bucket, token)} />
            <span>{step.label}</span>
          </span>
        ))}
      </span>
      <span>未知与复合占用计入已占用</span>
    </div>
  );
}

export interface OccupancyHeatmapProps {
  /** 7 行 × 5 列的占用率矩阵（0..1）。 */
  matrix: number[][];
  /** 节次轴；为 null（无当前版本、meta 请求失败）时列头回退「大节 1..5」。 */
  axis: MetaAxis | null;
}

/**
 * 7 天 × 5 大节占用热力图。
 *
 * `role="img"` + `aria-label` 只给一句概述；每格再用 `<title>` 给出
 * 「周一 第 01–02 节 42%」这样的可读文本，并在鼠标悬停时用 antd Tooltip
 * 显示同一句话——颜色永远不是唯一信息。
 * `preserveAspectRatio="xMidYMid meet"`：容器宽高比与 700×420 不一致时等比缩放
 * 居中，格子不会被拉变形。
 */
export function OccupancyHeatmap({ matrix, axis }: OccupancyHeatmapProps) {
  const { token } = theme.useToken();
  const blockLabels = buildBlockLabels(axis);

  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        gap: 8,
        height: "100%",
        minHeight: 0,
      }}
    >
      <div style={{ flex: "1 1 0", minHeight: 0 }}>
        <svg
          role="img"
          aria-label={heatmapSummary(matrix)}
          viewBox={`0 0 ${VIEW_WIDTH} ${VIEW_HEIGHT}`}
          preserveAspectRatio="xMidYMid meet"
          data-testid="occupancy-heatmap"
          style={{ display: "block", width: "100%", height: "100%" }}
        >
          {blockLabels.map((label, column) => (
            <text
              key={`block-${column}`}
              x={cellX(column) + CELL_WIDTH / 2}
              y={HEADER_HEIGHT - 12}
              textAnchor="middle"
              fill={token.colorTextSecondary}
              fontSize={16}
            >
              {label}
            </text>
          ))}

          {WEEKDAY_LABELS.map((weekday, row) => (
            <g key={weekday}>
              <text
                x={LABEL_WIDTH - 12}
                y={cellY(row) + CELL_HEIGHT / 2}
                textAnchor="end"
                dominantBaseline="central"
                fill={token.colorTextSecondary}
                fontSize={16}
              >
                {weekday}
              </text>

              {blockLabels.map((blockLabel, column) => {
                const rate = rateAt(matrix, row, column);
                const bucket = occupancyBucket(rate);
                const title = `${weekday} ${blockLabel} ${formatRatePercent(rate)}`;

                return (
                  <Tooltip key={`${row}-${column}`} title={title}>
                    <rect
                      data-testid="heatmap-cell"
                      data-bucket={bucket}
                      x={cellX(column)}
                      y={cellY(row)}
                      width={CELL_WIDTH}
                      height={CELL_HEIGHT}
                      rx={CELL_RADIUS}
                      fill={
                        bucket === 0
                          ? token.colorBgContainer
                          : token.colorPrimary
                      }
                      fillOpacity={OCCUPANCY_FILL_OPACITY[bucket]}
                      stroke={token.colorBorderSecondary}
                      strokeWidth={1}
                    >
                      <title>{title}</title>
                    </rect>
                  </Tooltip>
                );
              })}
            </g>
          ))}
        </svg>
      </div>

      <HeatmapLegend />
    </div>
  );
}

export interface BuildingOccupancyListProps {
  /** 按楼的占用情况；后端已按楼号升序，前端不重排。 */
  buildings: ScreenBuildingOccupancy[];
}

/**
 * 按教学楼占用率：楼名 + 占用率进度 + 百分比 + 已占/总格数。
 * 楼数可能远多于可视高度（26 栋），列表在区块内滚动，不挤压上方热力图。
 * 进度条与百分比同时存在：颜色只是辅助，百分比文字才是读数。
 */
export function BuildingOccupancyList({
  buildings,
}: BuildingOccupancyListProps) {
  const { token } = theme.useToken();

  if (buildings.length === 0) {
    return (
      <p style={{ margin: 0, fontSize: 16, color: token.colorTextSecondary }}>
        暂无按楼占用数据
      </p>
    );
  }

  return (
    <div
      data-testid="building-occupancy-list"
      style={{
        display: "flex",
        flexDirection: "column",
        height: "100%",
        minHeight: 0,
      }}
    >
      <p
        style={{
          margin: 0,
          fontSize: 14,
          fontWeight: 600,
          color: token.colorTextSecondary,
        }}
      >
        按教学楼
      </p>
      <ul
        style={{
          listStyle: "none",
          margin: "4px 0 0",
          padding: 0,
          flex: "1 1 0",
          minHeight: 0,
          overflow: "auto",
        }}
      >
        {buildings.map((building) => {
          const percent = Math.round(
            Math.min(1, Math.max(0, building.rate)) * 100,
          );

          return (
            <li
              key={building.building_id}
              data-testid="building-occupancy-row"
              style={{
                display: "grid",
                gridTemplateColumns: "minmax(0, 1fr) 64px 44px 104px",
                alignItems: "center",
                gap: 8,
                padding: "4px 0",
                borderBottom: `1px solid ${token.colorSplit}`,
                fontSize: 14,
              }}
            >
              <span
                title={building.name}
                style={{
                  overflow: "hidden",
                  textOverflow: "ellipsis",
                  whiteSpace: "nowrap",
                  color: token.colorText,
                }}
              >
                {building.name}
              </span>
              <Progress
                percent={percent}
                showInfo={false}
                strokeColor={token.colorPrimary}
                railColor={token.colorFillSecondary}
                aria-label={`${building.name} 占用率 ${formatRatePercent(building.rate)}`}
              />
              <span
                style={{
                  textAlign: "right",
                  fontFamily: MONO_FONT,
                  fontVariantNumeric: "tabular-nums",
                  color: token.colorText,
                }}
              >
                {formatRatePercent(building.rate)}
              </span>
              <span
                style={{
                  textAlign: "right",
                  fontFamily: MONO_FONT,
                  fontVariantNumeric: "tabular-nums",
                  fontSize: 12,
                  color: token.colorTextSecondary,
                }}
              >
                {numberFormat.format(building.occupied)} /{" "}
                {numberFormat.format(building.cells)}
              </span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

export interface OccupancyBlockProps {
  occupancy: ScreenOccupancy;
  axis: MetaAxis | null;
  /** 无当前数据版本：保留标题、正文显示「暂无数据」（Requirement 6.6）。 */
  empty: boolean;
}

/**
 * 大屏右下角的「占用分布」区块：左侧热力图 + 图例，右侧按楼占用率。
 *
 * 左右分栏而不是上下堆叠：右下角的 4 行栅格只有约 420px 高，热力图
 * 按 700×420 等比缩放后横向还有富余，把列表放到右侧能同时给两者足够高度。
 */
export function OccupancyBlock({
  occupancy,
  axis,
  empty,
}: OccupancyBlockProps) {
  return (
    <ScreenBlock title="占用分布" testId="screen-block-occupancy">
      {empty ? (
        <NoData />
      ) : (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "minmax(0, 3fr) minmax(0, 2fr)",
            gap: 20,
            height: "100%",
            minHeight: 0,
          }}
        >
          <OccupancyHeatmap matrix={occupancy.by_weekday_block} axis={axis} />
          <BuildingOccupancyList buildings={occupancy.by_building} />
        </div>
      )}
    </ScreenBlock>
  );
}
