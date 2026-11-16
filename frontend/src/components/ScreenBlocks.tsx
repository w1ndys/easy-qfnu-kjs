// 大屏五个信息区块（tasklist 第 7 项；Requirement 6.1–6.4、6.6）。
//
// 只消费 /api/screen 的已整形字段：区块内部不接触 ApiError，也不渲染业务码、
// message 或错误原文（Requirement 7.6、设计决策 D10 的另一面——大屏只留状态与计数）。
//
// 空态口径：`empty=true` 表示没有当前数据版本，五个区块都保留标题、正文换成
// 「暂无数据」，不清空页面也不隐藏区块（design「大屏布局」末条）。
// 各区块用 <section aria-label> 包住标题，读屏与测试都能按区块名定位。

import { Tag, theme } from "antd";
import type { CSSProperties, ReactNode } from "react";
import type {
  ScreenAnomalies,
  ScreenLastRun,
  ScreenRelease,
  ScreenScale,
  WeekFreshness,
} from "../api/types";
import { formatShanghaiDateTime } from "../lib/datetime";

/**
 * 空态正文：五个区块共用同一句（Requirement 6.6）。
 * 大屏第 8 项的占用占位区也复用它，保证 `empty=true` 时整页只有一种空态文案。
 */
export const NO_DATA_TEXT = "暂无数据";

/** 时间缺失或无法解析时的占位；不把原始 ISO 串漏到界面上（与 lib/datetime 同口径）。 */
const UNKNOWN_TIME = "未知";

/** 数字加千分位：房间数与格数都在千级，投屏远看更易读。 */
const numberFormat = new Intl.NumberFormat("zh-CN");

/** 数字用等宽字体：轮询刷新时位数变化不会让整行左右跳动。 */
const MONO_FONT =
  'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace';

/**
 * 一轮同步状态的中文文案。
 * 取值来自 backend/internal/pipeline/plan.go 的 RunStatus 常量；未列出的值原样显示，
 * 后端新增状态时不会白屏，也不会被前端猜成某个已有含义。
 */
const RUN_STATUS_LABELS: Readonly<Record<string, string>> = {
  running: "进行中",
  success: "成功",
  partial: "部分完成",
  failed: "失败",
  blocked: "阻止",
  skipped: "跳过",
};

/** 状态标签的语义色：只做视觉区分，文字始终是完整状态名。 */
const RUN_STATUS_COLORS: Readonly<Record<string, string>> = {
  running: "processing",
  success: "success",
  partial: "warning",
  failed: "error",
  blocked: "error",
  skipped: "default",
};

interface ScreenBlockProps {
  title: string;
  /** 区块在测试里的定位名；不用标题文本，避免与表格里的同名文字混淆。 */
  testId: string;
  children: ReactNode;
  /** 是否撑满父容器剩余高度；内容按自身高度排布的区块（如数据版本）传 false。 */
  grow?: boolean;
}

/** 区块外壳：标题 + 深色卡片；空态与有数据态共用同一个标题结构。 */
export function ScreenBlock({
  title,
  testId,
  children,
  grow = true,
}: ScreenBlockProps) {
  const { token } = theme.useToken();

  return (
    <section
      aria-label={title}
      data-testid={testId}
      style={{
        display: "flex",
        flexDirection: "column",
        flex: grow ? "1 1 0" : "0 0 auto",
        minWidth: 0,
        minHeight: 0,
        padding: 16,
        borderRadius: 12,
        background: token.colorBgContainer,
        border: `1px solid ${token.colorBorderSecondary}`,
      }}
    >
      <h2
        style={{
          margin: 0,
          fontSize: 20,
          fontWeight: 600,
          lineHeight: "26px",
          color: token.colorText,
        }}
      >
        {title}
      </h2>
      <div style={{ flex: 1, minHeight: 0, marginTop: 8 }}>{children}</div>
    </section>
  );
}

/** 空态正文：只替换区块内容，标题由 ScreenBlock 保留。 */
export function NoData() {
  const { token } = theme.useToken();
  return (
    <p
      style={{
        margin: 0,
        fontSize: 18,
        color: token.colorTextSecondary,
      }}
    >
      {NO_DATA_TEXT}
    </p>
  );
}

interface MetricRowProps {
  label: string;
  children: ReactNode;
}

/** 一行「标签 + 值」；标签小字、值等宽大字（design「数字用等宽字体、单位小字」）。 */
function MetricRow({ label, children }: MetricRowProps) {
  const { token } = theme.useToken();

  return (
    <div
      style={{
        display: "flex",
        alignItems: "baseline",
        justifyContent: "space-between",
        gap: 16,
        padding: "8px 0",
        borderBottom: `1px solid ${token.colorSplit}`,
      }}
    >
      <dt
        style={{
          flexShrink: 0,
          fontSize: 18,
          color: token.colorTextSecondary,
        }}
      >
        {label}
      </dt>
      <dd
        style={{
          margin: 0,
          minWidth: 0,
          overflow: "hidden",
          textOverflow: "ellipsis",
          whiteSpace: "nowrap",
          fontSize: 24,
          fontFamily: MONO_FONT,
          fontVariantNumeric: "tabular-nums",
          color: token.colorText,
        }}
      >
        {children}
      </dd>
    </div>
  );
}

/** 数字后的单位小字；只影响视觉，读屏仍读完整一句话。 */
function Unit({ children }: { children: ReactNode }) {
  return <span style={{ fontSize: 16 }}> {children}</span>;
}

/** 当前周次：0 表示今天不在当前版本所属学期，不编造周次（后端 currentWeek 的口径）。 */
function weekLabel(week: number): string {
  return week > 0 ? `第 ${week} 周` : "非教学周";
}

/** 采集窗口 [起始周, 结束周]；空数组表示没有可用窗口，不画成「第 0–0 周」。 */
function windowLabel(window: number[]): string {
  if (window.length !== 2) {
    return "暂无采集窗口";
  }
  return `第 ${window[0]}–${window[1]} 周`;
}

export interface ScaleBlockProps {
  data: ScreenScale;
  /** 无当前数据版本：保留标题、正文显示「暂无数据」。 */
  empty: boolean;
}

/** 站点规模：房间数、教学楼数、学期、当前周、采集窗口。 */
export function ScaleBlock({ data, empty }: ScaleBlockProps) {
  return (
    <ScreenBlock title="站点规模" testId="screen-block-scale">
      {empty ? (
        <NoData />
      ) : (
        <dl style={{ margin: 0 }}>
          <MetricRow label="房间数">
            {numberFormat.format(data.rooms)}
            <Unit>间</Unit>
          </MetricRow>
          <MetricRow label="教学楼数">
            {numberFormat.format(data.buildings)}
            <Unit>栋</Unit>
          </MetricRow>
          <MetricRow label="学期">
            {data.term === "" ? "未知" : data.term}
          </MetricRow>
          <MetricRow label="当前周">{weekLabel(data.week)}</MetricRow>
          <MetricRow label="采集窗口">{windowLabel(data.window)}</MetricRow>
        </dl>
      )}
    </ScreenBlock>
  );
}

export interface ReleaseBlockProps {
  release: ScreenRelease | null;
  empty: boolean;
}

/** 版本信息：当前数据版本标识与生成时间（生成时间按 Asia/Shanghai 展示）。 */
export function ReleaseBlock({ release, empty }: ReleaseBlockProps) {
  const { token } = theme.useToken();
  const noData = empty || release === null;

  // 一行排布而不是两行 MetricRow：右上角还要放逐周表，纵向空间按 12×8 栅格固定，
  // 这里多占 40px 就会让 4 行逐周表只能看到 1 行。
  return (
    <ScreenBlock title="数据版本" testId="screen-block-release" grow={false}>
      {noData ? (
        <NoData />
      ) : (
        <dl
          style={{
            display: "flex",
            flexWrap: "wrap",
            alignItems: "baseline",
            gap: "8px 32px",
            margin: 0,
          }}
        >
          <div style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
            <dt style={{ fontSize: 16, color: token.colorTextSecondary }}>
              版本标识
            </dt>
            <dd
              style={{
                margin: 0,
                fontSize: 20,
                fontFamily: MONO_FONT,
                fontVariantNumeric: "tabular-nums",
              }}
            >
              {release.release_id}
            </dd>
          </div>
          <div style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
            <dt style={{ fontSize: 16, color: token.colorTextSecondary }}>
              生成时间（Asia/Shanghai）
            </dt>
            <dd
              style={{
                margin: 0,
                fontSize: 20,
                fontFamily: MONO_FONT,
                fontVariantNumeric: "tabular-nums",
              }}
            >
              {formatShanghaiDateTime(release.generated_at) ?? UNKNOWN_TIME}
            </dd>
          </div>
        </dl>
      )}
    </ScreenBlock>
  );
}

const WEEK_COLUMNS: ReadonlyArray<{ key: string; label: string }> = [
  { key: "week", label: "周次" },
  { key: "published", label: "发布" },
  { key: "last_success", label: "最近成功（Asia/Shanghai）" },
  { key: "stale", label: "过期" },
];

export interface WeekFreshnessTableProps {
  weeks: WeekFreshness[];
  empty: boolean;
}

/**
 * 逐周新鲜度：周次、是否发布、最近成功时间、过期标记。
 *
 * 只渲染后端给的 weeks：窗口裁剪与「窗口缺失时退回整学期」都在服务端
 * （backend/internal/query/screen.go 的 screenWeeks），前端再裁一次会出现两套口径。
 * 行数可能超过区块高度，所以用可滚动容器 + 置顶表头。
 */
export function WeekFreshnessTable({ weeks, empty }: WeekFreshnessTableProps) {
  const { token } = theme.useToken();
  const noData = empty || weeks.length === 0;

  return (
    <ScreenBlock title="逐周新鲜度" testId="screen-block-weeks">
      {noData ? (
        <NoData />
      ) : (
        <div
          style={{
            height: "100%",
            overflow: "auto",
          }}
        >
          <table
            style={{
              width: "100%",
              borderCollapse: "collapse",
              fontSize: 15,
            }}
          >
            <thead>
              <tr>
                {WEEK_COLUMNS.map((column) => (
                  <th
                    key={column.key}
                    scope="col"
                    style={{
                      position: "sticky",
                      top: 0,
                      zIndex: 1,
                      padding: "5px 10px",
                      textAlign: "left",
                      whiteSpace: "nowrap",
                      fontWeight: 600,
                      color: token.colorTextSecondary,
                      background: token.colorBgContainer,
                      borderBottom: `1px solid ${token.colorBorderSecondary}`,
                    }}
                  >
                    {column.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {weeks.map((week) => (
                <tr key={week.week}>
                  <td style={cellStyle(token.colorText, token.colorSplit)}>
                    第 {week.week} 周
                  </td>
                  <td style={cellStyle(token.colorText, token.colorSplit)}>
                    <Tag color={week.published ? "success" : "default"}>
                      {week.published ? "已发布" : "未发布"}
                    </Tag>
                  </td>
                  <td style={cellStyle(token.colorText, token.colorSplit)}>
                    {formatShanghaiDateTime(week.last_success_at) ?? "—"}
                  </td>
                  <td style={cellStyle(token.colorText, token.colorSplit)}>
                    {week.stale ? <Tag color="warning">已过期</Tag> : "—"}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </ScreenBlock>
  );
}

/** 表格单元格：边框色跟随主题 token，深色下不出现亮线。 */
function cellStyle(color: string, border: string): CSSProperties {
  return {
    padding: "5px 10px",
    textAlign: "left",
    whiteSpace: "nowrap",
    color,
    borderBottom: `1px solid ${border}`,
  };
}

/**
 * 耗时格式化：毫秒 → 「30 分 30 秒」。
 * `null` 表示这一轮没有结束时刻（仍在跑或异常终止），显示占位而不是编一个 0。
 */
function formatDuration(durationMs: number | null): string {
  if (durationMs === null || !Number.isFinite(durationMs) || durationMs < 0) {
    return "—";
  }

  const totalSeconds = Math.round(durationMs / 1000);
  if (totalSeconds < 60) {
    return `${totalSeconds} 秒`;
  }

  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) {
    return seconds === 0 ? `${minutes} 分` : `${minutes} 分 ${seconds} 秒`;
  }

  const hours = Math.floor(minutes / 60);
  const restMinutes = minutes % 60;
  return restMinutes === 0
    ? `${hours} 小时`
    : `${hours} 小时 ${restMinutes} 分`;
}

export interface RunBlockProps {
  run: ScreenLastRun | null;
  empty: boolean;
}

/** 最近一轮同步：状态、开始时间、耗时；一轮都没跑过时显示「暂无数据」。 */
export function RunBlock({ run, empty }: RunBlockProps) {
  const noData = empty || run === null;

  return (
    <ScreenBlock title="最近一轮" testId="screen-block-run">
      {noData ? (
        <NoData />
      ) : (
        <dl style={{ margin: 0 }}>
          <MetricRow label="状态">
            <Tag color={RUN_STATUS_COLORS[run.status] ?? "default"}>
              {RUN_STATUS_LABELS[run.status] ?? run.status}
            </Tag>
          </MetricRow>
          <MetricRow label="开始时间（Asia/Shanghai）">
            {formatShanghaiDateTime(run.started_at) ?? UNKNOWN_TIME}
          </MetricRow>
          <MetricRow label="耗时">{formatDuration(run.duration_ms)}</MetricRow>
        </dl>
      )}
    </ScreenBlock>
  );
}

export interface AnomalyBlockProps {
  data: ScreenAnomalies;
  empty: boolean;
}

/** 异常计数：未知格、复合格、归属冲突三个计数（Requirement 6.4）。 */
export function AnomalyBlock({ data, empty }: AnomalyBlockProps) {
  const { token } = theme.useToken();

  return (
    <ScreenBlock title="异常计数" testId="screen-block-anomalies">
      {empty ? (
        <NoData />
      ) : (
        <div
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(3, minmax(0, 1fr))",
            gap: 16,
            height: "100%",
          }}
        >
          <AnomalyCount label="未知格" value={data.unknown_cells} />
          <AnomalyCount label="复合格" value={data.composite_cells} />
          <AnomalyCount
            label="归属冲突"
            value={data.attr_conflicts}
            // 品牌色只做视觉强调，计数与标签文字仍是完整信息。
            accent={token.colorPrimary}
          />
        </div>
      )}
    </ScreenBlock>
  );
}

interface AnomalyCountProps {
  label: string;
  value: number;
  accent?: string;
}

/** 单个异常计数：大号等宽数字 + 小字标签。 */
function AnomalyCount({ label, value, accent }: AnomalyCountProps) {
  const { token } = theme.useToken();

  return (
    <div
      style={{
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        gap: 8,
        minWidth: 0,
        padding: 12,
        borderRadius: 8,
        background: token.colorFillQuaternary,
      }}
    >
      <span
        style={{
          fontSize: 40,
          lineHeight: 1,
          fontFamily: MONO_FONT,
          fontVariantNumeric: "tabular-nums",
          color: accent ?? token.colorText,
        }}
      >
        {numberFormat.format(value)}
      </span>
      <span style={{ fontSize: 16, color: token.colorTextSecondary }}>
        {label}
      </span>
    </div>
  );
}
