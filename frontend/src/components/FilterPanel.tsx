// 公开查询页的输入区（tasklist 第 3 项；design「组件树」的 FilterPanel）：
// 11 个日期项 · 教学楼下拉 · 节次区间 · 关键词 · 查询按钮。
// 这里不持有筛选状态：除关键词输入草稿外，所有值都来自 URL（design D1）。

import { Button, Card, Input, Radio, Select, Typography } from "antd";
import { useMemo, useState, type FormEvent } from "react";
import { useLocation, useNavigationType } from "react-router";
import type { ContextInfo, MetaResult } from "../api/types";
import {
  MAX_DATE_OFFSET,
  MAX_KEYWORD_LENGTH,
  type QueryFilters,
} from "../hooks/useQueryFilters";
import { dateOptionLabel } from "../lib/dateOffset";
import { buildNodeOptions } from "../lib/nodes";

export interface FilterPanelProps {
  context: ContextInfo;
  meta: MetaResult;
  filters: QueryFilters;
  /** 查询在途：只让按钮 loading，筛选控件保持可编辑（Requirement 5.1）。 */
  loading?: boolean;
  /** 改日期/楼/节次：直接把新值写进 URL。 */
  onFiltersChange: (patch: Partial<QueryFilters>) => void;
  /** 回车与点击查询按钮走同一个出口；参数是去掉首尾空白后的关键词。 */
  onSearch: (keyword: string) => void;
}

export function FilterPanel({
  context,
  meta,
  filters,
  loading = false,
  onFiltersChange,
  onSearch,
}: FilterPanelProps) {
  const location = useLocation();
  const navigationType = useNavigationType();

  // 关键词是唯一不逐字写进 URL 的输入：输入过程只改本地草稿，回车/点查询才提交。
  // draft 为 null 表示「没有未提交的编辑」，此时显示 URL 上的已提交关键词。
  const [draftKeyword, setDraftKeyword] = useState<string | null>(null);
  const keyword =
    draftKeyword !== null && draftKeyword !== filters.keyword
      ? draftKeyword
      : filters.keyword;

  // 后退/前进（POP 导航）后，URL 上的关键词才是用户此刻看到的筛选：清掉未提交草稿，
  // 让输入框跟随 URL，而不是让旧草稿盖住它。改筛选走 PUSH（新历史记录），草稿保留，
  // 换日期时不会丢掉正在敲的关键词。
  // 这里用「渲染期按导航校正自身状态」而不是 effect：effect 里 setState 会多一次级联
  // 渲染（react-hooks/set-state-in-effect 直接判错），而条件式渲染期 setState 是 React
  // 允许的「props/路由变化时调整状态」写法，且只在新历史条目上触发一次。
  const [lastLocationKey, setLastLocationKey] = useState(location.key);
  if (lastLocationKey !== location.key) {
    setLastLocationKey(location.key);
    if (navigationType === "POP") {
      setDraftKeyword(null);
    }
  }

  // 日期项只产 date_offset（0..10），文案以 /api/context 的业务日期为基准，
  // 前端不推算目标日期的周次与星期（Requirement 2.7、design D3/D4）。
  const dateOptions = useMemo(
    () =>
      Array.from({ length: MAX_DATE_OFFSET + 1 }, (_, offset) => ({
        value: offset,
        label: dateOptionLabel(context.date, offset),
      })),
    [context.date],
  );

  // 首位「全部」取值为空，后面才是白名单楼；顺序即元数据顺序（Requirement 2.6）。
  const buildingOptions = useMemo(
    () => [
      { value: "", label: "全部" },
      ...meta.buildings.map((building) => ({
        value: building.building_id,
        label: building.name,
      })),
    ],
    [meta.buildings],
  );

  const nodeOptions = useMemo(() => buildNodeOptions(meta.axis), [meta.axis]);

  const submit = () => {
    onSearch(keyword.trim());
    // 提交后回到「无未提交编辑」状态，输入框以 URL 的已提交值为准。
    setDraftKeyword(null);
  };

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    submit();
  };

  return (
    <Card title="筛选条件">
      <form aria-label="空教室筛选条件" onSubmit={handleSubmit}>
        <div style={{ display: "grid", gap: 16 }}>
          <div>
            <Typography.Text type="secondary">日期</Typography.Text>
            <div
              role="group"
              aria-label="日期"
              style={{ overflowX: "auto", paddingBottom: 4 }}
            >
              <Radio.Group
                optionType="button"
                buttonStyle="solid"
                value={filters.date_offset}
                options={dateOptions}
                onChange={(event) =>
                  onFiltersChange({ date_offset: Number(event.target.value) })
                }
              />
            </div>
          </div>

          <div>
            <Typography.Text type="secondary">教学楼</Typography.Text>
            <Select
              aria-label="教学楼"
              value={filters.building}
              options={buildingOptions}
              virtual={false}
              style={{ width: "100%" }}
              onChange={(value: string) => onFiltersChange({ building: value })}
            />
          </div>

          <div>
            <Typography.Text type="secondary">节次区间</Typography.Text>
            <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
              <Select
                aria-label="起始节次"
                value={filters.start_node}
                options={nodeOptions}
                virtual={false}
                style={{ flex: 1 }}
                onChange={(value: string) =>
                  // 起点不能晚于终点：越过时把终点一起带过去，避免出现非法组合。
                  onFiltersChange(
                    value > filters.end_node
                      ? { start_node: value, end_node: value }
                      : { start_node: value },
                  )
                }
              />
              <Typography.Text type="secondary">至</Typography.Text>
              <Select
                aria-label="结束节次"
                value={filters.end_node}
                options={nodeOptions}
                virtual={false}
                style={{ flex: 1 }}
                onChange={(value: string) =>
                  onFiltersChange(
                    value < filters.start_node
                      ? { start_node: value, end_node: value }
                      : { end_node: value },
                  )
                }
              />
            </div>
          </div>

          <div>
            <Typography.Text type="secondary">关键词</Typography.Text>
            <Input
              aria-label="关键词"
              placeholder="房间名关键词（可留空）"
              value={keyword}
              maxLength={MAX_KEYWORD_LENGTH}
              allowClear
              onChange={(event) => setDraftKeyword(event.target.value)}
              onPressEnter={(event) => {
                // 阻止默认的隐式表单提交：回车只走下面同一条 submit 路径，避免重复请求。
                event.preventDefault();
                submit();
              }}
            />
          </div>

          <Button
            type="primary"
            htmlType="submit"
            loading={loading}
            autoInsertSpace={false}
            style={{ minHeight: 44 }}
          >
            查询
          </Button>
        </div>
      </form>
    </Card>
  );
}
