// 空教室结果列表（tasklist 第 4 项；Requirement 3.1 / 3.4）：
// 每行显示房名 · 房间标识 · 所属教学楼，并给出「看全天状态」入口。
// 顺序即服务端返回顺序：这里不排序、不按楼分组（前端不重排）。

import { theme, Typography } from "antd";
import { useMemo } from "react";
import { Link } from "react-router";
import type { AvailabilityRoom } from "../api/types";
import { buildRoomLabels } from "../lib/roomName";

export interface ResultListProps {
  /** 服务端返回的一页房间；渲染顺序与数组顺序一致。 */
  items: AvailabilityRoom[];
  /** 当前日期偏移，原样带到全天状态页（Requirement 3.4）。 */
  dateOffset: number;
  /** 当前教学楼；空串表示「全部」，跳转时不写这个参数。 */
  building: string;
}

/**
 * 「看全天状态」的地址：房间标识进路径，当前日期偏移与教学楼进查询串。
 * 楼为空（全部）时不写 `building=`，让全天状态页走它的「全部」缺省值，
 * 而不是带一个空值参数（等价但地址更干净）。
 */
function dayStatusHref(
  roomId: string,
  dateOffset: number,
  building: string,
): string {
  const search = new URLSearchParams({ date_offset: String(dateOffset) });
  if (building !== "") {
    search.set("building", building);
  }
  return `/day/${encodeURIComponent(roomId)}?${search.toString()}`;
}

export function ResultList({ items, dateOffset, building }: ResultListProps) {
  const { token } = theme.useToken();
  // 展示名按整页结果算一次：同名判定需要看到整页，而不是逐行猜。
  const labels = useMemo(() => buildRoomLabels(items), [items]);

  return (
    <ul
      aria-label="空教室结果"
      style={{ listStyle: "none", margin: 0, padding: 0 }}
    >
      {labels.map(({ room, label }) => (
        <li
          key={room.room_id}
          style={{
            display: "flex",
            flexWrap: "wrap",
            alignItems: "center",
            justifyContent: "space-between",
            gap: 12,
            padding: "12px 0",
            borderBottom: `1px solid ${token.colorSplit}`,
          }}
        >
          <div style={{ display: "grid", gap: 4 }}>
            <Typography.Text strong>{label}</Typography.Text>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
              <Typography.Text type="secondary">
                房间标识：{room.room_id}
              </Typography.Text>
              <Typography.Text type="secondary">
                教学楼：{room.building_name}
              </Typography.Text>
            </div>
          </div>
          <Link
            to={dayStatusHref(room.room_id, dateOffset, building)}
            // 每行入口的可访问名带上房名，读屏时不会听到一串同名「看全天状态」。
            aria-label={`看 ${label} 的全天状态`}
            style={{
              display: "inline-flex",
              alignItems: "center",
              minHeight: 44,
              minWidth: 44,
            }}
          >
            看全天状态
          </Link>
        </li>
      ))}
    </ul>
  );
}
