package publish

import (
	"errors"
	"fmt"
)

// 发布组装的错误哨兵。调用方用 errors.Is 区分「拒绝发布」的几类原因，
// 不匹配错误文案（errorlint）。
var (
	// ErrEmptyCandidate 表示候选集没有任何观测：没有可发布的可用性数据，
	// 整轮拒绝发布（Requirement 7.5）。
	ErrEmptyCandidate = errors.New("候选集为空")
	// ErrRequiredTasksIncomplete 表示窗口内的必需周矩阵任务没有全部完成：
	// 有未完成的、有失败的，或一条必需任务都没有。
	ErrRequiredTasksIncomplete = errors.New("必需任务未完成")
	// ErrAttributionIncomplete 表示归属快照与候选观测/说明对不上：
	// 有房号缺归属、归属项缺楼编号或楼名、或同一房号出现两次。
	ErrAttributionIncomplete = errors.New("归属快照不完整")
	// ErrUnknownCellsExceeded 表示未知状态格数超过配置阈值。
	ErrUnknownCellsExceeded = errors.New("未知状态格数超过阈值")
	// ErrCompositeCellsExceeded 表示复合状态格数超过配置阈值。
	ErrCompositeCellsExceeded = errors.New("复合状态格数超过阈值")
	// ErrCandidateInputInvalid 表示交给 Build 的候选原料自相矛盾或越界：
	// 空房号、窗口外的周、非法星期、轴里没有的大节、重复的格等。
	ErrCandidateInputInvalid = errors.New("候选集原料非法")
	// ErrRunFactsInvalid 表示轮次本身不足以组装候选集：缺学期、没有采集窗口。
	ErrRunFactsInvalid = errors.New("轮次发布事实非法")
	// ErrAxisInvalid 表示节次轴快照不可用：没有节点、节点编号不是两位 ASCII 数字、
	// 节点重复、大节为空，或大节与小节编号的顺序拼接对不上。
	ErrAxisInvalid = errors.New("节次轴快照非法")
)

// 校验拦截原因。Reason 是稳定标识，供调用方写日志或落 collect_run 的拦截原因；
// 它不是给人读的错误文案，文案在 ValidationError 里。
const (
	ReasonEmptyCandidate        = "empty_candidate"        // 候选集没有观测
	ReasonRequiredIncomplete    = "required_incomplete"    // 必需任务未全部完成
	ReasonAttributionIncomplete = "attribution_incomplete" // 归属快照不完整
	ReasonUnknownExceeded       = "unknown_threshold"      // 未知格超过阈值
	ReasonCompositeExceeded     = "composite_threshold"    // 复合格超过阈值
)

// ValidationError 是一次发布前校验拦截：Reason 给出稳定的拦截原因，
// Err 是可用 errors.Is 判断的哨兵，文案里带具体数字便于排查。
//
// 调用方应当先看 errors.As 拿 Reason 再决定记录方式；不要把 Error() 的文案
// 当契约写进库或接口，文案会随版本调整。
type ValidationError struct {
	Reason string
	Err    error
}

// Error 返回带拦截原因与具体数字的可读文案。
func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v（%s）", e.Err, e.Reason)
}

// Unwrap 让 errors.Is 能命中 ValidationError 包着的哨兵。
func (e *ValidationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// newValidationError 构造一条校验拦截；调用点只传原因与带数字的哨兵。
func newValidationError(reason string, err error) error {
	return &ValidationError{Reason: reason, Err: err}
}

// ValidationReason 返回一次校验拦截的稳定原因；err 不是校验拦截时返回空串。
// 它替调用方收口 errors.As 的样板，避免各处重复写同一个类型断言。
func ValidationReason(err error) string {
	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return validationErr.Reason
	}
	return ""
}
