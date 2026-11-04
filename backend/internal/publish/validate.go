package publish

import (
	"fmt"
	"strings"
)

// Thresholds 是发布前的未知/复合格数阈值。
//
// 零值或负数表示不限制：PRD 4.5 的口径是「未知/复合只计数，不阻断」，
// 所以默认配置不会因为未知或复合状态拦下一整轮；只有显式配置了正数阈值才拦截。
// 阈值与候选集里按大节计的格数比较（同一大节展开出的 2–3 个小节不重复计数）。
type Thresholds struct {
	MaxUnknownCells   int // 未知格数上限；<=0 不限制
	MaxCompositeCells int // 复合格数上限；<=0 不限制
}

// Validate 做发布前校验（design 的 publish.Validate、Requirement 7.5）。
//
// 校验顺序即拦截优先级，先命中的原因就是本轮拒绝发布的原因：
//  1. 候选集非空：没有任何观测就拒绝。没有观测的版本会让公开查询整体变成空态，
//     不能当成一次成功的发布；
//  2. 必需任务完成：窗口内每栋楼每周的周矩阵任务必须全部 done，一条 failed 或
//     尚未执行都拒绝；一条必需任务都没有同样拒绝——那说明本轮根本没规划窗口采集
//     （空窗或空白名单的轮次在规划阶段就该结束，不该走到发布）；
//  3. 归属快照完整：每条观测与说明的房号都要在版本归属快照里，快照行不能缺楼编号
//     或楼名，同一房号不能出现两次（CP2）；
//  4. 未知/复合阈值：只在显式配置正数阈值时拦截。未知与复合恒不可用（CP4），
//     数量异常说明上游口径可能变了，留给运维决定是否放宽。
//
// 归属冲突不在这里拦截：它是附加标记，只计数、不阻断（原 spec 12.1）。
func Validate(candidate Candidate, thresholds Thresholds) error {
	if err := validateNotEmpty(candidate); err != nil {
		return err
	}
	if err := validateRequiredTasks(candidate); err != nil {
		return err
	}
	if err := validateAttribution(candidate); err != nil {
		return err
	}
	return validateThresholds(candidate, thresholds)
}

// validateNotEmpty 拒绝没有观测的候选集。
func validateNotEmpty(candidate Candidate) error {
	if len(candidate.Observations) > 0 {
		return nil
	}
	return newValidationError(
		ReasonEmptyCandidate,
		fmt.Errorf("%w: 轮次 %d 没有可发布的观测", ErrEmptyCandidate, candidate.RunID))
}

// validateRequiredTasks 要求必需任务有且全部完成：
// total > 0、done == total、failed == 0。done 大于 total 这类自相矛盾的数字
// 也按未完成拒绝，不把它当成「全部完成」。
func validateRequiredTasks(candidate Candidate) error {
	if candidate.RequiredTotal > 0 &&
		candidate.RequiredDone == candidate.RequiredTotal &&
		candidate.RequiredFailed == 0 {
		return nil
	}
	return newValidationError(
		ReasonRequiredIncomplete,
		fmt.Errorf("%w: 必需任务 %d/%d 完成、%d 失败",
			ErrRequiredTasksIncomplete,
			candidate.RequiredDone, candidate.RequiredTotal, candidate.RequiredFailed))
}

// validateAttribution 校验版本归属快照能覆盖候选集里的全部房号，且自身没有矛盾。
//
// 只认「房号」这一个身份：房名可以重复，房号不能；归属项的楼编号与楼名都要非空，
// 否则写 release_room_building 时会撞 NOT NULL 或留下无法展示的归属。
func validateAttribution(candidate Candidate) error {
	buildingByRoom := make(map[string]string, len(candidate.Rooms))
	for _, room := range candidate.Rooms {
		roomID := strings.TrimSpace(room.RoomID)
		buildingID := strings.TrimSpace(room.BuildingID)
		buildingName := strings.TrimSpace(room.BuildingName)
		if roomID == "" {
			return attributionError("归属快照里有空房号")
		}
		if buildingID == "" {
			return attributionError("房号 %s 的归属缺少教学楼编号", roomID)
		}
		if buildingName == "" {
			return attributionError("房号 %s 的归属缺少教学楼展示名", roomID)
		}
		if _, exists := buildingByRoom[roomID]; exists {
			return attributionError("房号 %s 在归属快照里出现两次", roomID)
		}
		buildingByRoom[roomID] = buildingID
	}

	for _, observation := range candidate.Observations {
		if _, ok := buildingByRoom[strings.TrimSpace(observation.RoomID)]; !ok {
			return attributionError("观测的房号 %s 不在归属快照里", observation.RoomID)
		}
	}
	for _, detail := range candidate.Details {
		if _, ok := buildingByRoom[strings.TrimSpace(detail.RoomID)]; !ok {
			return attributionError("占用说明的房号 %s 不在归属快照里", detail.RoomID)
		}
	}
	return nil
}

// attributionError 构造一条「归属快照不完整」的校验拦截。
func attributionError(format string, args ...any) error {
	return newValidationError(
		ReasonAttributionIncomplete,
		fmt.Errorf("%w: "+format, append([]any{ErrAttributionIncomplete}, args...)...))
}

// validateThresholds 在显式配置了正数阈值时拦截超限的候选集。
// 两个阈值都按大节计的格数比较；<=0 表示不限制。
func validateThresholds(candidate Candidate, thresholds Thresholds) error {
	if thresholds.MaxUnknownCells > 0 && candidate.UnknownCells > thresholds.MaxUnknownCells {
		return newValidationError(
			ReasonUnknownExceeded,
			fmt.Errorf("%w: %d 格超过上限 %d",
				ErrUnknownCellsExceeded, candidate.UnknownCells, thresholds.MaxUnknownCells))
	}
	if thresholds.MaxCompositeCells > 0 && candidate.CompositeCells > thresholds.MaxCompositeCells {
		return newValidationError(
			ReasonCompositeExceeded,
			fmt.Errorf("%w: %d 格超过上限 %d",
				ErrCompositeCellsExceeded, candidate.CompositeCells, thresholds.MaxCompositeCells))
	}
	return nil
}
