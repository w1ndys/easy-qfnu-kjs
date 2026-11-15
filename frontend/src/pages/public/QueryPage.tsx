// 公开查询页（tasklist 第 3–4 项）：页面上下文/元数据加载 + 筛选输入区 + 查询请求 + 结果列表与分页。
// 加载/空/过期/错误四态由第 6 项接在结果区；本项只落「有结果时」的列表与分页。

import { Alert, Button, Card, Pagination, Skeleton, Typography } from "antd";
import { useCallback, useEffect, useState } from "react";
import { getErrorTitle } from "../../api/errors";
import { queryAvailability } from "../../api/public";
import { FilterPanel } from "../../components/FilterPanel";
import { ResultList } from "../../components/ResultList";
import { useApi } from "../../hooks/useApi";
import { useMeta } from "../../hooks/useMeta";
import { useQueryFilters } from "../../hooks/useQueryFilters";
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
    (signal: AbortSignal) =>
      queryAvailability(
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
      ),
    [filters],
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

  const loadingCard = (
    <Card>
      <Skeleton active paragraph={{ rows: 3 }} />
      <Typography.Text type="secondary">
        正在加载页面上下文与数据版本…
      </Typography.Text>
    </Card>
  );

  const errorCard =
    metaError !== null ? (
      <Card>
        <Alert
          type="error"
          showIcon
          title={getErrorTitle(metaError.code)}
          description={metaError.message || "页面上下文或数据版本加载失败。"}
          action={
            <Button size="small" onClick={reloadMeta}>
              重试
            </Button>
          }
        />
        <Typography.Text
          type="secondary"
          style={{ display: "block", marginTop: 8 }}
        >
          request_id：{metaError.requestId}
        </Typography.Text>
      </Card>
    ) : null;

  return (
    <main style={{ padding: 24, maxWidth: 960, margin: "0 auto" }}>
      <Typography.Title level={2}>空教室查询</Typography.Title>

      {metaLoading ? loadingCard : null}
      {!metaLoading && errorCard}
      {metaReady && context !== null && meta !== null ? (
        <FilterPanel
          context={context}
          meta={meta}
          filters={filters}
          loading={query.loading}
          onFiltersChange={setFilters}
          onSearch={handleSearch}
        />
      ) : null}

      <Card style={{ marginTop: 16 }} title="查询结果">
        {query.data !== null ? (
          <>
            <ResultList
              items={query.data.items}
              dateOffset={filters.date_offset}
              building={filters.building}
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
        ) : null}
      </Card>
    </main>
  );
}
