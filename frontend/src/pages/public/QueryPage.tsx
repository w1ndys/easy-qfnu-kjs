// 公开查询页：页面上下文/元数据加载 + 筛选输入区 + 查询请求 + 结果区四态（tasklist 第 3–6 项）。
//
// 四态的判定顺序就是下面 resultBody 的 if 链：
// 加载（元数据或查询在途）→ 页面级错误 → 查询错误 → 有结果时按 hint 分空态/置顶提示/列表。
// 判定只用响应字段：过期看响应 week 是否命中 meta.weeks 且 stale（design「四态」），
// 提示档位看 hint.code，前端不按时间或文案猜状态。

import { Button, Card, Pagination, theme, Typography } from "antd";
import type { ReactNode } from "react";
import { useCallback, useEffect, useState } from "react";
import { ApiError } from "../../api/client";
import { RATE_LIMIT_CODE, getErrorTitle } from "../../api/errors";
import { queryAvailability } from "../../api/public";
import { FilterPanel, FILTER_FORM_ID } from "../../components/FilterPanel";
import { PageFooter } from "../../components/PageFooter";
import { ResultList } from "../../components/ResultList";
import {
  EmptyView,
  ErrorView,
  HintAlert,
  LoadingView,
  StaleBanner,
} from "../../components/StateViews";
import { useApi } from "../../hooks/useApi";
import { useMeta } from "../../hooks/useMeta";
import { useQueryFilters } from "../../hooks/useQueryFilters";
import { useRateLimitCooldown } from "../../hooks/useRateLimitCooldown";
import { useViewportTier } from "../../hooks/useViewportTier";
import { VISUALLY_HIDDEN } from "../../lib/a11y";
import { getEmptyCopy, HINT_WEEK_NO_SCHEDULE } from "../../lib/hintCopy";
import { FIXED_QUERY_BAR_RESERVED_PX, publicLayout } from "../../lib/layout";
import { defaultNodeRange, type NodeRange } from "../../lib/nodes";

/**
 * 单页容量：接口 `limit` 为 1..200，取 20 让手机上一页不过长。
 * `page` 与 `limit`/`offset` 的换算只在这里发生（`offset = (page - 1) * 20`）。
 */
export const RESULT_PAGE_SIZE = 20;

export interface QueryPageProps {
  /**
   * 页面打开时刻，只用于按本地时间预填节次区间（Requirement 2.4）。
   * 生产用当前时间；测试注入固定时刻来对齐 11:59/12:00/17:59/18:00 边界。
   */
  now?: Date;
}

export function QueryPage({ now }: QueryPageProps) {
  // 预填区间在页面打开时算一次并冻结：之后改日期（含非当天）沿用当前时段，
  // 也不会在页面停留跨过 12:00/18:00 时把用户正在看的区间换掉。
  const [fallbackRange] = useState<NodeRange>(() =>
    defaultNodeRange(now ?? new Date()),
  );

  const {
    context,
    meta,
    loading: metaLoading,
    error: metaError,
    reload: reloadMeta,
  } = useMeta();
  const { filters, canonicalSearch, isCanonical, setFilters, replaceFilters } =
    useQueryFilters(fallbackRange);

  // 42901 后查询按钮冷却 3 秒（design「Error Handling」）。状态只有布尔，
  // 渲染期不读时钟（eslint react-hooks/purity）。
  const { coolingDown, start: startCooldown } = useRateLimitCooldown();

  // 三档响应式（第 10 项；Requirement 8.1–8.4）：断点与派生布局只在 lib/layout.ts
  // 里定义，页面按档位铺栅格、决定查询按钮是否固定在底部。
  const layout = publicLayout(useViewportTier());
  const { token } = theme.useToken();

  const metaReady =
    !metaLoading && metaError === null && context !== null && meta !== null;

  // URL 上的教学楼必须在元数据白名单里：手改 URL、过期分享链接里的楼会被服务端判 40001，
  // 所以「全部」或命中白名单才允许发查询，其余等元数据到齐后回退（见下面的 effect）。
  const buildingAllowed =
    filters.building === "" ||
    (meta?.buildings.some(
      (building) => building.building_id === filters.building,
    ) ??
      false);

  // 元数据到齐后做一次 URL 规范化：
  // 1) 楼不在白名单 → 回退「全部」，同时把其余字段补全（一次 replace 完成两件事）；
  // 2) 只是缺默认值 → 用 replace 回写一次，让 URL 完整描述筛选。
  // 都用 replace：这只是把预填值规范化，不该占用一条历史记录（验收第 2 条）。
  useEffect(() => {
    if (!metaReady || meta === null) {
      return;
    }
    if (!buildingAllowed) {
      replaceFilters({ ...filters, building: "" });
      return;
    }
    if (!isCanonical) {
      replaceFilters(filters);
    }
  }, [metaReady, meta, buildingAllowed, isCanonical, replaceFilters, filters]);

  // 查询参数只来自 URL 解析出的 filters；分页由 page 换算成接口的 limit/offset。
  // 改楼/关键词/日期时 useQueryFilters 已把 page 重置为 1，所以换筛选自然回到第一页。
  const loadAvailability = useCallback(
    async (signal: AbortSignal) => {
      try {
        return await queryAvailability(
          {
            date_offset: filters.date_offset,
            building: filters.building || undefined,
            start_node: filters.start_node,
            end_node: filters.end_node,
            keyword: filters.keyword || undefined,
            limit: RESULT_PAGE_SIZE,
            offset: (filters.page - 1) * RESULT_PAGE_SIZE,
          },
          signal,
        );
      } catch (error) {
        // 限流冷却在请求的异步出口触发：这里已经在渲染之外，可以安全地读时间。
        if (error instanceof ApiError && error.code === RATE_LIMIT_CODE) {
          startCooldown();
        }
        throw error;
      }
    },
    [filters, startCooldown],
  );

  const query = useApi(
    canonicalSearch,
    loadAvailability,
    metaReady && isCanonical && buildingAllowed,
  );

  // 数据到达后按 total 把页码钳进有效范围：URL 上可能带着 page=999（手改、旧分享链接），
  // 结果集也可能在两次请求之间缩小（后台发布了新版本），两种情况下当前页都会是空的。
  // 钳制结果用 replace 写回 URL：这是改正一个已失效的地址，不该新增历史记录，
  // 否则后退会退回那个空页。
  const lastPage =
    query.data === null
      ? 1
      : Math.max(1, Math.ceil(query.data.total / RESULT_PAGE_SIZE));

  useEffect(() => {
    if (query.data === null || filters.page <= lastPage) {
      return;
    }
    replaceFilters({ ...filters, page: lastPage });
  }, [query.data, filters, lastPage, replaceFilters]);

  // 回车与点击查询按钮的同一个出口：筛选没变就重发同一次请求，变了就写 URL 触发请求。
  const handleSearch = (keyword: string) => {
    if (keyword === filters.keyword) {
      query.reload();
      return;
    }
    setFilters({ keyword });
  };

  // 有数据时的结果区内容：列表 + 分页。空态/错误态不会走到这里，
  // 所以错误时上一次的列表一定不在页面上（Requirement 5.5）。
  const resultRows =
    query.data === null ? null : (
      <>
        <ResultList
          items={query.data.items}
          dateOffset={filters.date_offset}
          building={filters.building}
          columns={layout.resultColumns}
        />
        <Pagination
          style={{ marginTop: 16, textAlign: "center" }}
          // 越界页在 URL 被纠正前先按最后一页显示，避免分页控件出现一个不存在的当前页。
          current={Math.min(filters.page, lastPage)}
          pageSize={RESULT_PAGE_SIZE}
          total={query.data.total}
          showSizeChanger={false}
          showTotal={(total) => `共 ${total} 间空教室`}
          onChange={(page) => setFilters({ page })}
        />
      </>
    );

  // 结果区四态。加载态放最前：请求在途时不拿旧结果凑数（useApi 此时 data 已为 null）。
  let resultBody: ReactNode;
  if (metaLoading || query.loading) {
    resultBody = (
      <LoadingView
        label={
          metaLoading ? "正在加载页面上下文与数据版本…" : "正在查询空教室…"
        }
      />
    );
  } else if (metaError !== null) {
    // 缺字典与楼白名单时筛选区无法工作：这是页面级错误，带重试与 request_id。
    resultBody = <ErrorView error={metaError} onRetry={reloadMeta} />;
  } else if (query.error !== null) {
    resultBody = (
      <ErrorView
        error={query.error}
        onRetry={query.reload}
        retryDisabled={coolingDown}
      />
    );
  } else if (query.data !== null) {
    const hint = query.data.hint;
    if (meta?.empty ?? false) {
      // 无当前数据版本时优先展示这一档：后端 total=0 时总会给 hint（backend/internal/query/
      // availability.go 的 availabilityHint），若只看 hint，「暂无数据版本」在页面上永远不可达。
      resultBody = <EmptyView noRelease />;
    } else if (hint?.code === HINT_WEEK_NO_SCHEDULE) {
      // 该周疑似无排课：置顶提示，结果照常展示（不换成空态）。
      // total=0 时没有结果可展示：只留置顶提示，不画空列表和「共 0 间空教室」分页。
      resultBody = (
        <>
          <HintAlert hint={hint} />
          {query.data.total === 0 ? null : resultRows}
        </>
      );
    } else if (hint !== undefined || query.data.total === 0) {
      // 其余三档与「命中 0 条」都进空态：标题与建议都按 hint.code 选。
      resultBody = <EmptyView hint={hint} />;
    } else {
      resultBody = resultRows;
    }
  } else {
    // 元数据已就绪但 URL 尚未规范化（下一轮就会发请求）：保持加载态，不闪空态。
    resultBody = <LoadingView label="正在准备查询…" />;
  }

  // 读屏播报：结果数量、空态标题、错误分类都从这里出；文本随态变化（Requirement 8.6）。
  // 空态标题取自同一张文案表，避免界面与播报说两套话。
  const announcement = (() => {
    if (metaLoading) {
      return "正在加载页面信息";
    }
    if (metaError !== null) {
      return `页面加载失败：${getErrorTitle(metaError.code)}`;
    }
    if (query.loading) {
      return "正在查询空教室";
    }
    if (query.error !== null) {
      return `查询失败：${getErrorTitle(query.error.code)}`;
    }
    if (query.data === null) {
      return "";
    }
    // 与结果区同一判定顺序：meta.empty 时界面是「暂无数据版本」，播报也必须说同一档，
    // 否则读屏用户听到的是 hint 的另一句话。
    if (meta?.empty ?? false) {
      return `查询结果：${getEmptyCopy(undefined, true).title}`;
    }
    const { hint, total } = query.data;
    if (hint !== undefined && hint.code !== HINT_WEEK_NO_SCHEDULE) {
      return `查询结果：${getEmptyCopy(hint).title}`;
    }
    if (total === 0) {
      return `查询结果：${getEmptyCopy(hint).title}`;
    }
    if (hint !== undefined) {
      return `${getEmptyCopy(hint).title}；共 ${total} 间空教室`;
    }
    // 播报文案带前缀，避免与分页控件的「共 N 间空教室」在页面上重复成两条同文案。
    return `查询结果：共 ${total} 间空教室`;
  })();

  // 元数据就绪才谈布局：筛选区没出现时结果区占满整行，不留一块空白列。
  const showFilter = metaReady && context !== null && meta !== null;
  const resultSpan = showFilter ? layout.resultSpan : layout.columns;

  return (
    <>
      <main
        data-testid="public-page"
        data-layout={layout.tier}
        data-columns={layout.columns}
        style={{
          display: "grid",
          // 单列 / 双列 / 三列：筛选取 1 列，宽屏时结果再占 2 列（Requirement 8.2）。
          gridTemplateColumns: `repeat(${layout.columns}, minmax(0, 1fr))`,
          gap: 16,
          alignItems: "start",
          // <main> 只保留普通内边距；底部固定操作区的位置由页脚容器统一预留一次。
          padding: layout.padding,
          width: "100%",
          maxWidth: layout.maxWidth,
          margin: "0 auto",
          boxSizing: "border-box",
        }}
      >
        <Typography.Title
          level={layout.tier === "compact" ? 3 : 2}
          style={{ gridColumn: "1 / -1", margin: 0 }}
        >
          空教室查询
        </Typography.Title>

        {showFilter && context !== null && meta !== null ? (
          <div
            data-testid="public-filter-area"
            data-span={layout.filterSpan}
            style={{ gridColumn: `span ${layout.filterSpan}`, minWidth: 0 }}
          >
            <FilterPanel
              context={context}
              meta={meta}
              filters={filters}
              loading={query.loading}
              disabled={coolingDown}
              // 窄屏时按钮移到页面底部的固定操作区，表单内不再重复渲染。
              showSubmit={!layout.fixedQueryBar}
              onFiltersChange={setFilters}
              onSearch={handleSearch}
            />
          </div>
        ) : null}

        <div
          data-testid="public-results-area"
          data-span={resultSpan}
          style={{ gridColumn: `span ${resultSpan}`, minWidth: 0 }}
        >
          <Card className="public-results" title="查询结果">
            <div
              role="status"
              aria-live="polite"
              aria-atomic="true"
              aria-label="查询结果播报"
              style={VISUALLY_HIDDEN}
            >
              {announcement}
            </div>
            <StaleBanner
              week={query.data?.week}
              weeks={meta?.weeks}
              inCalendar={context?.in_calendar}
            />
            {resultBody}
          </Card>
        </div>
      </main>

      {/*
        <600px 的固定查询按钮（Requirement 8.1）：放在 <main> 之外，position: fixed
        贴住视口底部并避开安全区；用 form 属性关联筛选表单，回车与点击仍走同一个
        onSubmit 与限流冷却守卫。宽度 ≥600 时这一块不渲染，按钮回到筛选卡内。
      */}
      {layout.fixedQueryBar && showFilter ? (
        <div
          data-testid="public-query-bar"
          role="region"
          aria-label="查询操作"
          style={{
            position: "fixed",
            insetInline: 0,
            bottom: 0,
            zIndex: 20,
            padding: "12px 16px",
            paddingBottom: "calc(12px + env(safe-area-inset-bottom, 0px))",
            background: token.colorBgContainer,
            borderTop: `1px solid ${token.colorSplit}`,
            boxShadow: "0 -2px 8px rgba(0, 0, 0, 0.06)",
          }}
        >
          <Button
            type="primary"
            htmlType="submit"
            form={FILTER_FORM_ID}
            loading={query.loading}
            disabled={coolingDown}
            block
            autoInsertSpace={false}
            style={{ minHeight: 48 }}
          >
            查询
          </Button>
        </div>
      ) : null}

      {/*
        页脚必须在 <main> 之外：<footer> 只有不嵌在 main/section 等分区里才映射成
        contentinfo，放在 main 里对真实读屏只是普通元素（jsdom 的 role 查询看不出来）。
      */}
      <div
        style={{
          maxWidth: layout.maxWidth,
          width: "100%",
          margin: "0 auto",
          // 窄屏底部有固定操作区：只在页脚让出一次高度，滚到底时不会被盖住。
          padding: `0 ${layout.padding}px ${
            layout.fixedQueryBar ? FIXED_QUERY_BAR_RESERVED_PX : layout.padding
          }px`,
          boxSizing: "border-box",
        }}
      >
        <PageFooter />
      </div>
    </>
  );
}
