// 本文件属于 business 层：把聚合矩阵上要下钻的格子变成本轮的同步任务。
// 明细任务是「楼 + 房间 + 格子」，一个格子一条、周次留空，所以同一格不会被按周重复下钻；
// 库里 sync_task 的唯一索引保证同一轮里同一格不会出现第二个任务。
// 依据 docs/decisions/2026-10-09-saturday-full-sync.md 与 specs/collector-full-sync/requirements.md 的 4.1。

package sync

import (
	"github.com/w1ndys/easy-qfnu-kjs/internal/collect/clean"
	"github.com/w1ndys/easy-qfnu-kjs/internal/store"
)

// CellDetailTasks 生成本轮的占用明细任务：每个要下钻的格子一条，周次保持 0（库里落 NULL）。
func CellDetailTasks(buildingID string, cells []clean.CellRef) []store.SyncTask {
	tasks := make([]store.SyncTask, 0, len(cells))
	for _, cell := range cells {
		tasks = append(tasks, store.SyncTask{
			Kind:       store.KindCellDetail,
			BuildingID: buildingID,
			RoomID:     cell.RoomID,
			Weekday:    cell.Weekday,
			Block:      cell.Block,
		})
	}
	return tasks
}
