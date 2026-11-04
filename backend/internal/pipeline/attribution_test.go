package pipeline

import (
	"errors"
	"fmt"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// matrixRow 造一行周矩阵解析结果；归属只关心房号，格子留空。
func matrixRow(roomID string) model.MatrixRoom {
	return model.MatrixRoom{RoomID: roomID, NameRaw: roomID}
}

// roomIDs 取一份响应的房号，便于比较 Accepted/Dropped 的顺序与内容。
func roomIDs(rooms []model.MatrixRoom) []string {
	roomIDs := make([]string, len(rooms))
	for index, room := range rooms {
		roomIDs[index] = room.RoomID
	}
	return roomIDs
}

// TestAttributionFirstHitWinsByWhitelistOrder 覆盖 Requirement 5.1–5.3 / CP3：
// 房号第一次出现归属该楼；后续楼再报同一房号时整行丢弃并记一次冲突。
func TestAttributionFirstHitWinsByWhitelistOrder(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})

	first, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101"), matrixRow("r102")})
	if err != nil {
		t.Fatalf("首个响应不应报错，实际 %v", err)
	}
	if got := fmt.Sprint(roomIDs(first.Accepted)); got != fmt.Sprint([]string{"r101", "r102"}) {
		t.Fatalf("首个响应应全部归属 Q10，实际 %v", got)
	}
	if len(first.Dropped) != 0 || len(first.Conflicts) != 0 {
		t.Fatalf("首个响应不应有丢弃或冲突: %+v", first)
	}

	second, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r101"), matrixRow("r201")})
	if err != nil {
		t.Fatalf("冲突响应不应报错，实际 %v", err)
	}
	if got := fmt.Sprint(roomIDs(second.Accepted)); got != fmt.Sprint([]string{"r201"}) {
		t.Fatalf("Q11 只应保留未出现过的 r201，实际 %v", got)
	}
	if got := fmt.Sprint(roomIDs(second.Dropped)); got != fmt.Sprint([]string{"r101"}) {
		t.Fatalf("r101 应整行丢弃，实际 %v", got)
	}
	if len(second.Conflicts) != 1 {
		t.Fatalf("应记 1 条冲突，实际 %+v", second.Conflicts)
	}
	want := RoomConflict{RoomID: "r101", KeptBuildingID: "Q10", DroppedBuildingID: "Q11"}
	if second.Conflicts[0] != want {
		t.Fatalf("冲突内容期望 %+v，实际 %+v", want, second.Conflicts[0])
	}

	if buildingID, ok := attribution.BuildingOf("r101"); !ok || buildingID != "Q10" {
		t.Fatalf("r101 应归属 Q10，实际 %q（存在=%v）", buildingID, ok)
	}
	if attribution.ConflictCount() != 1 {
		t.Fatalf("冲突数应为 1，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionSameBuildingKeepsOwnership 覆盖同楼多周复现：
// 同一栋楼在另一周再报同一房号仍归它，不算冲突，也不重复记。
func TestAttributionSameBuildingKeepsOwnership(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})

	if _, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101")}); err != nil {
		t.Fatalf("首个响应不应报错，实际 %v", err)
	}
	repeat, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101")})
	if err != nil {
		t.Fatalf("同楼复现不应报错，实际 %v", err)
	}
	if got := fmt.Sprint(roomIDs(repeat.Accepted)); got != fmt.Sprint([]string{"r101"}) {
		t.Fatalf("同楼复现应仍算归属，实际 %v", got)
	}
	if len(repeat.Dropped) != 0 || len(repeat.Conflicts) != 0 {
		t.Fatalf("同楼复现不应丢弃或记冲突: %+v", repeat)
	}
	if attribution.ConflictCount() != 0 {
		t.Fatalf("同楼复现不应增加冲突数，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionRecordsConflictOncePerRoom 覆盖「一轮只记一次」：
// 同一房号在后续多栋楼、多周反复出现，冲突只记第一次，后续只丢弃不计数。
func TestAttributionRecordsConflictOncePerRoom(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11", "Q12"})

	if _, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101")}); err != nil {
		t.Fatalf("Q10 不应报错，实际 %v", err)
	}

	for _, buildingID := range []string{"Q11", "Q12", "Q11"} {
		offer, err := attribution.Offer(buildingID, []model.MatrixRoom{matrixRow("r101")})
		if err != nil {
			t.Fatalf("%s 不应报错，实际 %v", buildingID, err)
		}
		if got := fmt.Sprint(roomIDs(offer.Dropped)); got != fmt.Sprint([]string{"r101"}) {
			t.Fatalf("%s 的 r101 应被丢弃，实际 %v", buildingID, got)
		}
		if len(offer.Accepted) != 0 {
			t.Fatalf("%s 不应有可归属房号，实际 %v", buildingID, roomIDs(offer.Accepted))
		}
	}

	if attribution.ConflictCount() != 1 {
		t.Fatalf("同一房号一轮只应记 1 条冲突，实际 %d", attribution.ConflictCount())
	}
	if conflicts := attribution.Conflicts(); len(conflicts) != 1 || conflicts[0].DroppedBuildingID != "Q11" {
		t.Fatalf("冲突应保留首次发生的那条，实际 %+v", conflicts)
	}
}

// TestAttributionCountsEachConflictedRoomOnce 覆盖计数口径：
// 冲突数按房号去重，不同房号各自记一次；同一房号后续周次不再累加。
func TestAttributionCountsEachConflictedRoomOnce(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})

	if _, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101"), matrixRow("r102"), matrixRow("r103")}); err != nil {
		t.Fatalf("Q10 不应报错，实际 %v", err)
	}

	// Q11 报 r101、r102 两个冲突，r103 没报；再报一次 r101 不重复计数。
	offer, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r101"), matrixRow("r102")})
	if err != nil {
		t.Fatalf("Q11 不应报错，实际 %v", err)
	}
	if len(offer.Conflicts) != 2 {
		t.Fatalf("本份响应应新记 2 条冲突，实际 %+v", offer.Conflicts)
	}
	repeat, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r101")})
	if err != nil {
		t.Fatalf("Q11 复现不应报错，实际 %v", err)
	}
	if len(repeat.Conflicts) != 0 {
		t.Fatalf("已记过的房号不应重复记冲突，实际 %+v", repeat.Conflicts)
	}
	if attribution.ConflictCount() != 2 {
		t.Fatalf("冲突数应为 2，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionUsesWhitelistRank 覆盖白名单顺序：
// 名次靠前的楼先命中就赢；名次靠前的楼晚到时必须报错，不能悄悄改写已落库的归属。
func TestAttributionUsesWhitelistRank(t *testing.T) {
	attribution := NewAttribution([]string{"Q11", "Q10"})

	if _, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r101")}); err != nil {
		t.Fatalf("Q11 不应报错，实际 %v", err)
	}
	offer, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101")})
	if err != nil {
		t.Fatalf("白名单靠后的楼后到不应报错，实际 %v", err)
	}
	if got := fmt.Sprint(roomIDs(offer.Dropped)); got != fmt.Sprint([]string{"r101"}) {
		t.Fatalf("Q10 的 r101 应被丢弃，实际 %v", got)
	}
	if buildingID, _ := attribution.BuildingOf("r101"); buildingID != "Q11" {
		t.Fatalf("白名单靠前的 Q11 应保住归属，实际 %s", buildingID)
	}

	// 反过来的到达顺序破坏「首个命中」：更靠前的楼晚到，无法回退已落库的归属。
	if _, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r202")}); err != nil {
		t.Fatalf("Q10 首见 r202 不应报错，实际 %v", err)
	}
	lateErr := func() error {
		_, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r202")})
		return err
	}()
	if lateErr == nil {
		t.Fatal("白名单靠前的楼晚到时应报错")
	}
	if !errors.Is(lateErr, errAttributionData) {
		t.Fatalf("晚到楼应标记为数据类错误，实际 %v", lateErr)
	}
}

// TestAttributionRejectsMalformedResponse 覆盖坏输入：
// 空楼编号、空房号、同一响应内重复房号都直接报错，不写归属。
func TestAttributionRejectsMalformedResponse(t *testing.T) {
	cases := []struct {
		name       string
		buildingID string
		rooms      []model.MatrixRoom
	}{
		{name: "空楼编号", buildingID: "  ", rooms: []model.MatrixRoom{matrixRow("r101")}},
		{name: "空房号", buildingID: "Q10", rooms: []model.MatrixRoom{matrixRow("")}},
		{name: "同一响应内重复房号", buildingID: "Q10", rooms: []model.MatrixRoom{matrixRow("r101"), matrixRow("r101")}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			attribution := NewAttribution([]string{"Q10"})
			_, err := attribution.Offer(c.buildingID, c.rooms)
			if err == nil {
				t.Fatal("坏响应应报错")
			}
			if !errors.Is(err, errAttributionData) {
				t.Fatalf("坏响应应标记为数据类错误，实际 %v", err)
			}
			if attribution.ConflictCount() != 0 || len(attribution.Snapshot()) != 0 {
				t.Fatalf("坏响应不应写入任何归属: %+v", attribution.Snapshot())
			}
		})
	}
}

// TestAttributionSeedRebuildsOwnership 覆盖续跑重建：Seed 把库里已裁决的归属
// 灌进累计器，后续楼再报同房号只能丢弃；冲突计数也从 Seed 带上，不被清零。
func TestAttributionSeedRebuildsOwnership(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})
	if err := attribution.Seed("r101", "Q10", false, ""); err != nil {
		t.Fatalf("Seed 不应报错，实际 %v", err)
	}
	if err := attribution.Seed("r102", "Q10", true, "Q11"); err != nil {
		t.Fatalf("Seed 冲突不应报错，实际 %v", err)
	}

	if buildingID, ok := attribution.BuildingOf("r101"); !ok || buildingID != "Q10" {
		t.Fatalf("Seed 后 r101 应归 Q10，实际 %q（存在=%v）", buildingID, ok)
	}
	if attribution.ConflictCount() != 1 {
		t.Fatalf("Seed 的冲突应计入计数，实际 %d", attribution.ConflictCount())
	}
	conflicts := attribution.Conflicts()
	if len(conflicts) != 1 || conflicts[0] != (RoomConflict{
		RoomID: "r102", KeptBuildingID: "Q10", DroppedBuildingID: "Q11",
	}) {
		t.Fatalf("Seed 冲突详情不符: %+v", conflicts)
	}

	// 续跑后 Q11 再报 r101：已归属 Q10，只能丢弃并新增一次冲突。
	offer, err := attribution.Offer("Q11", []model.MatrixRoom{matrixRow("r101")})
	if err != nil {
		t.Fatalf("Q11 不应报错，实际 %v", err)
	}
	if got := fmt.Sprint(roomIDs(offer.Dropped)); got != fmt.Sprint([]string{"r101"}) {
		t.Fatalf("续跑后 r101 应仍被丢弃，实际 %v", got)
	}
	if attribution.ConflictCount() != 2 {
		t.Fatalf("旧冲突 1 + 新冲突 1 应为 2，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionSeedIsIdempotent 覆盖续跑重放：同一房号重复 Seed 同一归属
// 不报错、不重复计冲突；被丢弃的楼也要在白名单里（这里 Q11 在白名单中）。
func TestAttributionSeedIsIdempotent(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})
	for attempt := 0; attempt < 2; attempt++ {
		if err := attribution.Seed("r101", "Q10", true, "Q11"); err != nil {
			t.Fatalf("重复 Seed 不应报错，实际 %v", err)
		}
	}
	if attribution.ConflictCount() != 1 {
		t.Fatalf("重复 Seed 不应重复计冲突，实际 %d", attribution.ConflictCount())
	}
	if len(attribution.Conflicts()) != 1 {
		t.Fatalf("重复 Seed 不应重复记冲突详情: %+v", attribution.Conflicts())
	}
}

// TestAttributionSeedRejectsInconsistentData 覆盖库内归属自相矛盾：
// 空房号、空楼号、同一房号换楼都直接报错，不悄悄改写。
func TestAttributionSeedRejectsInconsistentData(t *testing.T) {
	if err := NewAttribution([]string{"Q10"}).Seed(" ", "Q10", false, ""); err == nil {
		t.Fatal("空房号应报错")
	}
	if err := NewAttribution([]string{"Q10"}).Seed("r101", " ", false, ""); err == nil {
		t.Fatal("空楼号应报错")
	}

	attribution := NewAttribution([]string{"Q10", "Q11"})
	if err := attribution.Seed("r101", "Q10", false, ""); err != nil {
		t.Fatalf("首次 Seed 不应报错，实际 %v", err)
	}
	if err := attribution.Seed("r101", "Q11", false, ""); err == nil {
		t.Fatal("同一房号换楼应报错")
	}
	if buildingID, _ := attribution.BuildingOf("r101"); buildingID != "Q10" {
		t.Fatalf("换楼失败后归属不应被改写，实际 %s", buildingID)
	}
}

// TestAttributionSeedRequiresConflictBuilding 覆盖冲突行口径：conflict=true 却没有
// 被丢弃的楼、以及 conflict=false 却带着被丢弃的楼，都是库里冲突列自相矛盾，
// 与写侧（store 的 normalizeRunRooms）同一口径，直接报错；
// 校验失败时累计器保持原状（不登记归属，也不计冲突）。
func TestAttributionSeedRequiresConflictBuilding(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})
	if err := attribution.Seed("r101", "Q10", true, " "); err == nil {
		t.Fatal("冲突缺少被丢弃的楼时应报错")
	}
	if err := attribution.Seed("r101", "Q10", false, "Q11"); err == nil {
		t.Fatal("未标冲突却带着被丢弃的楼时应报错")
	}
	if _, ok := attribution.BuildingOf("r101"); ok {
		t.Fatal("校验失败不应登记归属")
	}
	if attribution.ConflictCount() != 0 {
		t.Fatalf("校验失败不应计冲突，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionSeedRejectsBuildingOutsideWhitelist 覆盖续跑快照与本轮白名单
// 不一致：归属楼与被丢弃的楼都只可能来自白名单内的楼，出现别的楼说明传进来的
// 不是本轮快照，直接报错且不改累计器。
func TestAttributionSeedRejectsBuildingOutsideWhitelist(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})
	if err := attribution.Seed("r101", "Q99", false, ""); err == nil {
		t.Fatal("白名单外的归属楼应报错")
	}
	if err := attribution.Seed("r102", "Q10", true, "Q99"); err == nil {
		t.Fatal("白名单外的被丢弃楼应报错")
	}
	if _, ok := attribution.BuildingOf("r101"); ok {
		t.Fatal("校验失败不应登记归属")
	}
	if _, ok := attribution.BuildingOf("r102"); ok {
		t.Fatal("校验失败不应登记归属")
	}
	if attribution.ConflictCount() != 0 {
		t.Fatalf("校验失败不应计冲突，实际 %d", attribution.ConflictCount())
	}
}

// TestAttributionSnapshotIsCopy 覆盖归属集合快照：包含全部已归属房号，
// 且改快照不影响累计器，供发布阶段安全消费。
func TestAttributionSnapshotIsCopy(t *testing.T) {
	attribution := NewAttribution([]string{"Q10", "Q11"})
	if _, err := attribution.Offer("Q10", []model.MatrixRoom{matrixRow("r101")}); err != nil {
		t.Fatalf("Q10 不应报错，实际 %v", err)
	}

	snapshot := attribution.Snapshot()
	if got := fmt.Sprint(snapshot); got != fmt.Sprint(map[string]string{"r101": "Q10"}) {
		t.Fatalf("快照内容不符: %v", snapshot)
	}

	snapshot["r101"] = "Q99"
	snapshot["r999"] = "Q99"
	if buildingID, _ := attribution.BuildingOf("r101"); buildingID != "Q10" {
		t.Fatalf("改快照不应影响累计器，实际 %s", buildingID)
	}
	if _, ok := attribution.BuildingOf("r999"); ok {
		t.Fatal("快照新增的房号不应进入累计器")
	}
}
