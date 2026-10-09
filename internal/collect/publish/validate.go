// 本文件属于 business 层：发布前的校验。
// 依据 docs/contract/data-format.v2.md 的 validate 层与 specs/collector-full-sync 的
// requirements.md 2.5、6.2、设计文档 Error Handling。
// 校验失败的项一律阻断发布并保留原 current；未知符号不在阻断之列，它只被计数，
// 否则「上游新增一个符号」会升级成「整个学期没有数据」。

package publish

import (
	"errors"
	"fmt"
	"strings"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrValidation 表示发布前校验没通过：本轮不发布，保留原 current（需求 6.2）。
var ErrValidation = errors.New("发布前校验未通过")

// ErrRoomBuildingConflict 表示同一 jsbh 在本轮出现在两栋楼的结果里：不挑一栋覆盖（需求 2.5）。
var ErrRoomBuildingConflict = errors.New("同一 jsbh 归属两栋教学楼")

// ErrEmptyPlan 表示候选集里既没有房间也没有观测：发布它等于把 current 换成空数据。
var ErrEmptyPlan = errors.New("候选集里没有可发布的观测")

// validate 做发布前的阻断检查：上游整轮停止错误、候选集是否为空、同一教室归属两栋楼。
// 不按房间总数变化阻断。未知符号只计数，不在这里拦。
func validate(built plan, candidate Candidate) error {
	// 上游整轮停止（例如表头块集合变化）必须阻止发布，且错误要在本层原样带出去，不能吞掉
	if candidate.StopErr != nil {
		return fmt.Errorf("%w: %w", ErrValidation, candidate.StopErr)
	}
	// 没有任何观测的候选集不算合格的一版，发布它会让查询侧丢掉全部数据
	if len(built.observations) == 0 || built.rooms == 0 {
		return fmt.Errorf("%w: %w", ErrValidation, ErrEmptyPlan)
	}
	if err := checkBuildingConflict(candidate.Rooms); err != nil {
		return err
	}
	return nil
}

// checkBuildingConflict 检查同一 jsbh 是否在本轮出现在两栋楼的结果里（需求 2.5）。
// 房间的楼由返回它的那次按楼请求决定，所以同一 jsbh 带两个不同的楼编号就是这一轮采歪了。
func checkBuildingConflict(rooms []model.Room) error {
	seen := make(map[string]string, len(rooms))
	for _, room := range rooms {
		// 空楼编号表示这次没带归属信息，与 UpsertRoom 的写法一致：不比、也不记
		if strings.TrimSpace(room.BuildingID) == "" {
			continue
		}
		previous, ok := seen[room.ID]
		// 同一 jsbh 换了别的楼：挑一栋留下会静默改写归属，所以本轮直接失败
		if ok && previous != room.BuildingID {
			return fmt.Errorf("%w: %w: %s 同时属于 %s 与 %s",
				ErrValidation, ErrRoomBuildingConflict, room.ID, previous, room.BuildingID)
		}
		seen[room.ID] = room.BuildingID
	}
	return nil
}
