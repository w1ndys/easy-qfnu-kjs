package collector

import (
	"fmt"
	"sort"
)

// Room 周快照中的单房间条目（snapshot.schema.v1.json 的 room）。
type Room struct {
	Jsbh     string         `json:"jsbh"`
	GroupID  string         `json:"group_id"`
	Name     string         `json:"name"`
	Statuses map[string]int `json:"statuses"`
}

// Day 某一星期（"1"—"7"）下的房间状态集合。
type Day struct {
	Rooms []Room `json:"rooms"`
}

// WeekSnapshot 单周快照（snapshot.schema.v1.json）。
type WeekSnapshot struct {
	SchemaVersion int             `json:"schema_version"`
	Term          string          `json:"term"`
	Week          int             `json:"week"`
	SnapshotID    string          `json:"snapshot_id"`
	GeneratedAt   string          `json:"generated_at"`
	LastSuccessAt string          `json:"last_success_at"`
	Days          map[string]*Day `json:"days"`
}

// Anchor manifest 日历锚点。
type Anchor struct {
	Date               string `json:"date"`
	Week               int    `json:"week"`
	TotalWeeks         int    `json:"total_weeks"`
	Timezone           string `json:"timezone"`
	InTeachingCalendar bool   `json:"in_teaching_calendar"`
}

// ManifestGroup manifest 公开分组。
type ManifestGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Order     int    `json:"order"`
	RoomCount int    `json:"room_count"`
}

// NodeInfo 节次 → 来源大节映射。
type NodeInfo struct {
	Code        string `json:"code"`
	SourceBlock string `json:"source_block"`
}

// StatusInfo 状态字典条目。
type StatusInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// WeekEntry manifest.weeks 逐周发布索引。
type WeekEntry struct {
	Week          int     `json:"week"`
	SnapshotID    string  `json:"snapshot_id"`
	SHA256        string  `json:"sha256"`
	GeneratedAt   string  `json:"generated_at"`
	LastSuccessAt string  `json:"last_success_at"`
	LastErrorCode *string `json:"last_error_code,omitempty"`
	LastAttemptAt *string `json:"last_attempt_at,omitempty"`
}

// Manifest 数据清单（manifest.schema.v1.json）。
type Manifest struct {
	SchemaVersion int                   `json:"schema_version"`
	ReleaseID     string                `json:"release_id"`
	Term          string                `json:"term"`
	GeneratedAt   string                `json:"generated_at"`
	Anchor        Anchor                `json:"anchor"`
	Groups        []ManifestGroup       `json:"groups"`
	Nodes         []NodeInfo            `json:"nodes"`
	Statuses      map[string]StatusInfo `json:"statuses"`
	Weeks         map[string]*WeekEntry `json:"weeks"`
}

// snapshotIDFor 生成 <YYYYMMDDTHHMMSS+0800>-week-NN。
func snapshotIDFor(compactTS string, week int) string {
	return fmt.Sprintf("%s-week-%02d", compactTS, week)
}

// sortRooms 稳定排序：name 自然升序 → jsbh 自然升序（决策 §11/§3 房间顺序）。
func sortRooms(rooms []Room) {
	sort.SliceStable(rooms, func(i, j int) bool {
		if rooms[i].Name != rooms[j].Name {
			return naturalLess(rooms[i].Name, rooms[j].Name)
		}
		if rooms[i].Jsbh != rooms[j].Jsbh {
			return naturalLess(rooms[i].Jsbh, rooms[j].Jsbh)
		}
		return rooms[i].GroupID < rooms[j].GroupID
	})
}

// manifestNodes 生成固定 01—12 节次来源映射。
func manifestNodes() []NodeInfo {
	nodes := make([]NodeInfo, 0, len(nodeSourceBlocks))
	for _, nb := range nodeSourceBlocks {
		nodes = append(nodes, NodeInfo{Code: nb[0], SourceBlock: nb[1]})
	}
	return nodes
}

// manifestStatuses 生成固定 1—9 状态字典。
func manifestStatuses() map[string]StatusInfo {
	out := map[string]StatusInfo{}
	for _, d := range statusDefs {
		out[fmt.Sprintf("%d", d.ID)] = StatusInfo{Name: d.Name, Available: d.Available}
	}
	return out
}
