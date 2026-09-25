package collector

import "testing"

func TestParseWeekRel(t *testing.T) {
	term, week, ok := parseWeekRel("terms/2025-2026-3/weeks/week-01.json")
	if !ok || term != "2025-2026-3" || week != 1 {
		t.Fatalf("解析结果 term=%s week=%d ok=%v", term, week, ok)
	}
	if _, _, ok := parseWeekRel("manifest.json"); ok {
		t.Fatal("manifest 不应被当成周文件")
	}
}

func TestSlotsFromSnapshot(t *testing.T) {
	snap := &WeekSnapshot{
		Days: map[string]*Day{
			"1": {Rooms: []Room{{
				Jsbh: "A1", GroupID: "g", Name: "教室",
				Statuses: map[string]int{"01": 5, "02": 1},
			}}},
		},
	}
	slots := slotsFromSnapshot(snap)
	if len(slots) != 2 {
		t.Fatalf("小节数=%d，想要 2", len(slots))
	}
}
