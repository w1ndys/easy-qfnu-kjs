// 同名房名后缀（tasklist 第 4 项；Requirement 3.3）：
// 一页结果里出现不止一次的房间名，在名称后追加房间标识，避免两行看起来一模一样。
// 这里只算展示名：不排序、不去重，返回数组与入参同长同序（Requirement 3.1：顺序由服务端决定）。

import type { AvailabilityRoom } from "../api/types";

/** 一条结果行的房间与展示名。 */
export interface RoomLabel {
  room: AvailabilityRoom;
  label: string;
}

/**
 * 找出出现不止一次的房间名。
 * 判定范围是「本次拿到的一页」：前端只有当前页数据，跨页同名在这里无从判定；
 * 名称按原样比较，不做大小写或空白归一（名称由服务端给出，前端不另造一套规则）。
 */
export function duplicateRoomNames(
  rooms: readonly AvailabilityRoom[],
): ReadonlySet<string> {
  const seen = new Set<string>();
  const duplicated = new Set<string>();

  for (const room of rooms) {
    if (seen.has(room.name)) {
      duplicated.add(room.name);
    } else {
      seen.add(room.name);
    }
  }

  return duplicated;
}

/** 同名房名追加「（房间标识）」后缀，其余只显示名称。 */
export function roomDisplayName(
  room: AvailabilityRoom,
  duplicatedNames: ReadonlySet<string>,
): string {
  return duplicatedNames.has(room.name)
    ? `${room.name}（${room.room_id}）`
    : room.name;
}

/** 给一页结果算展示名；顺序与入参严格一致，前端不重排。 */
export function buildRoomLabels(
  rooms: readonly AvailabilityRoom[],
): RoomLabel[] {
  const duplicatedNames = duplicateRoomNames(rooms);

  return rooms.map((room) => ({
    room,
    label: roomDisplayName(room, duplicatedNames),
  }));
}
