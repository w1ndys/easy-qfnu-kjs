// 12 小节网格（tasklist 第 5 项；Requirement 4.1）：
// 渲染顺序取 meta.axis.nodes 的 ordinal，不照响应数组顺序摆，也不在前端写死节次名。
// 轴上没有对应响应的小节（含无当前版本、响应 nodes 为空的情况）按「无数据」渲染，
// 保证「12 格」这一形状稳定，空态不会被误读成「整天空闲」。

import type { DayNode, MetaAxis, MetaDict } from "../api/types";
import { NODE_IDS } from "../lib/nodes";
import { StatusCell } from "./StatusCell";

export interface NodeGridProps {
  /** 节次轴：小节编号与展示顺序的来源（design D11）；无当前版本时为 null。 */
  axis: MetaAxis | null;
  /** 状态字典：状态名的唯一来源（design D11）。 */
  dict: MetaDict | null;
  /** 该日的逐节状态（服务端顺序不作渲染依据，只按 node 编号对齐到轴上）。 */
  nodes: DayNode[];
}

interface GridNode {
  node: string;
  label: string;
}

/**
 * 网格要渲染的小节：轴存在时按 ordinal 排序，编号与「第 NN 节」文案都从轴派生。
 * 轴缺失（无当前版本）时回退契约里的 01..12，让页面仍能显示 12 个「无数据」格而不是白屏
 * （design D11 的「缺键回退」口径）。
 */
function gridNodes(axis: MetaAxis | null): GridNode[] {
  const axisNodes = axis?.nodes ?? [];
  if (axisNodes.length === 0) {
    return NODE_IDS.map((node) => ({ node, label: `第 ${node} 节` }));
  }

  return [...axisNodes]
    .sort((left, right) => left.ordinal - right.ordinal)
    .map((node) => ({ node: node.node, label: `第 ${node.node} 节` }));
}

export function NodeGrid({ axis, dict, nodes }: NodeGridProps) {
  // 先按小节编号建索引：轴决定摆哪 12 个格，响应只决定每格里是什么。
  const stateByNode = new Map(nodes.map((node) => [node.node, node]));

  return (
    <div
      role="list"
      aria-label="全天状态小节"
      style={{
        display: "grid",
        // 移动优先：窄屏自动折行，宽屏一行最多 12 格；断点收口留给第 10 项。
        gridTemplateColumns: "repeat(auto-fit, minmax(112px, 1fr))",
        gap: 12,
      }}
    >
      {gridNodes(axis).map(({ node, label }) => (
        <StatusCell
          key={node}
          nodeLabel={label}
          state={stateByNode.get(node) ?? null}
          dict={dict}
        />
      ))}
    </div>
  );
}
