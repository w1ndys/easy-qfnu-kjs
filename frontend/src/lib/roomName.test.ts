import { describe, expect, it } from "vitest";
import type { AvailabilityRoom } from "../api/types";
import {
  buildRoomLabels,
  duplicateRoomNames,
  roomDisplayName,
} from "./roomName";

function room(
  roomId: string,
  name: string,
  buildingId = "Q10",
  buildingName = "综合教学楼",
): AvailabilityRoom {
  return {
    room_id: roomId,
    name,
    building_id: buildingId,
    building_name: buildingName,
  };
}

describe("duplicateRoomNames", () => {
  it("只收集出现不止一次的房间名", () => {
    const duplicated = duplicateRoomNames([
      room("Q10-101", "综合楼101"),
      room("Q11-302", "理工楼302"),
      room("Q12-101", "综合楼101"),
    ]);

    expect([...duplicated]).toEqual(["综合楼101"]);
  });

  it("名称全部不同时没有同名项", () => {
    const duplicated = duplicateRoomNames([
      room("Q10-101", "综合楼101"),
      room("Q10-102", "综合楼102"),
    ]);

    expect(duplicated.size).toBe(0);
  });

  it("空结果集没有同名项", () => {
    expect(duplicateRoomNames([]).size).toBe(0);
  });

  it("按名称原样比较，不把仅大小写不同的名称当成同名", () => {
    const duplicated = duplicateRoomNames([
      room("Q10-101", "A101"),
      room("Q10-102", "a101"),
    ]);

    expect(duplicated.size).toBe(0);
  });
});

describe("roomDisplayName", () => {
  it("同名时在名称后追加房间标识后缀", () => {
    const duplicated = new Set(["综合楼101"]);

    expect(roomDisplayName(room("Q10-101", "综合楼101"), duplicated)).toBe(
      "综合楼101（Q10-101）",
    );
  });

  it("不同名时只显示名称", () => {
    expect(roomDisplayName(room("Q10-101", "综合楼101"), new Set())).toBe(
      "综合楼101",
    );
  });
});

describe("buildRoomLabels", () => {
  it("两间同名房渲染出不同标签，其余行不加后缀", () => {
    const labels = buildRoomLabels([
      room("Q12-101", "综合楼101", "Q12", "文科教学楼"),
      room("Q11-302", "理工楼302", "Q11", "理工教学楼"),
      room("Q10-101", "综合楼101", "Q10", "综合教学楼"),
    ]);

    expect(labels.map((entry) => entry.label)).toEqual([
      "综合楼101（Q12-101）",
      "理工楼302",
      "综合楼101（Q10-101）",
    ]);
  });

  it("保持入参顺序且不去重（前端不重排）", () => {
    const rooms = [
      room("Q10-103", "综合楼103"),
      room("Q10-101", "综合楼101"),
      room("Q10-102", "综合楼102"),
    ];

    const labels = buildRoomLabels(rooms);

    expect(labels.map((entry) => entry.room)).toEqual(rooms);
    expect(labels).toHaveLength(rooms.length);
  });

  it("空结果集返回空数组", () => {
    expect(buildRoomLabels([])).toEqual([]);
  });
});
