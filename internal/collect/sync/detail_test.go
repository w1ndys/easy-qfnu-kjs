package sync

// 本文件是明细任务构造的单元测试：纯函数，不连数据库、不发上游请求。
// 覆盖任务 6.1 的「不按周重复下钻」：一个格子一条任务，周次留空。

import (
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// TestCellDetailTasksOnePerCellWithoutWeek 断言每个要下钻的格子只建一条任务，且周次留空：
// 同一格不会被按周重复请求（需求 4.1）。
func TestCellDetailTasksOnePerCellWithoutWeek(t *testing.T) {
	cells := []clean.CellRef{
		{RoomID: "r-1", Weekday: 1, Block: "0102"},
		{RoomID: "r-1", Weekday: 3, Block: "101112"},
		{RoomID: "r-2", Weekday: 5, Block: "0607"},
	}

	tasks := CellDetailTasks("jxlbh-1", cells)
	if len(tasks) != len(cells) {
		t.Fatalf("生成 %d 条任务，期望 %d 条：一个格子一条", len(tasks), len(cells))
	}

	for index, task := range tasks {
		want := cells[index]
		// 两类任务靠形状区分：明细任务必须带齐房间、星期与大节，否则取到它也定位不到格子
		if task.Kind != store.KindCellDetail || task.BuildingID != "jxlbh-1" {
			t.Errorf("第 %d 条任务 = %+v，期望 jxlbh-1 的明细任务", index+1, task)
		}
		if task.RoomID != want.RoomID || task.Weekday != want.Weekday || task.Block != want.Block {
			t.Errorf("第 %d 条任务 = %+v，期望 %+v", index+1, task, want)
		}
		// 周次留空：明细不按周建任务，库里这一列落 NULL
		if task.Week != 0 {
			t.Errorf("第 %d 条任务带了周次 %d，期望留空", index+1, task.Week)
		}
	}
}

// TestCellDetailTasksWithoutCells 断言没有要下钻的格子时不建任务。
func TestCellDetailTasksWithoutCells(t *testing.T) {
	if tasks := CellDetailTasks("jxlbh-1", nil); len(tasks) != 0 {
		t.Errorf("生成 %d 条任务，期望 0 条", len(tasks))
	}
}
