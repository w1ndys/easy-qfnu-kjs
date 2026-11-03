// Package parser 把教务上游的原始 HTML 解析成结构化事实。
//
// 只读字节：不发请求、不落库，也不做语义清洗（状态原文与原始房名交给 clean）。
// 页面结构与本包期望不符时返回可区分的哨兵错误，错误信息只描述结构差异与位置，
// 不包含页面正文（docs/pitfalls.md 第 8 节）。
package parser

import (
	"errors"
	"fmt"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// ErrMatrixStructureInvalid 表示周矩阵页面结构与当前轴定义不符：
// 找不到 table#dataList、表头状态格集合或顺序不一致、数据行单元格数量与表头不一致、
// 房号三处来源都取不到，或同一响应内房号重复。
// 上层据此映射 error_code=matrix_structure_invalid 并把整轮置为失败（Requirement 2）。
var ErrMatrixStructureInvalid = errors.New("矩阵结构不符合预期")

// ErrAxisInvalid 表示传入的节次轴不足以校验表头：节点为空、缺大节信息、同一大节序号
// 对应多个编码，或大节序号不连续。这是配置数据问题而不是页面问题，
// 所以与 ErrMatrixStructureInvalid 分开，便于上层区分「页面变了」与「轴数据坏了」。
var ErrAxisInvalid = errors.New("节次轴定义不可用")

// ErrDetailStructureInvalid 表示占用说明页面连标题表 table#tab1 都没有，
// 说明拿到的不是明细页（docs/upstream-api.md 第 8 节）。
// 有标题表但没有字段行是正常情况，不算结构失败（本 spec
// specs/2026-11-03-collect-pipeline Requirement 3.4）。
// 上层据此映射 error_code=detail_structure_invalid，只把该格任务置为失败，
// 轮次标 partial，不影响可用性发布（Requirement 8.2）。
var ErrDetailStructureInvalid = errors.New("占用说明结构不符合预期")

// ErrTermTextInvalid 表示学期页文本解析失败：没有形如 YYYY-YYYY-N 的学期编号，
// 或缺 id/name 为 kbjcmsid 的非空 input value（旧 spec
// specs/2026-10-27-classroom-availability-rewrite Requirement 3.1）。
// 本包是学期文本的权威解析口径；错误同时归属 model.ErrUpstreamCalendarInvalid，
// 上层只需一个哨兵即可映射 error_code=calendar_invalid（旧 spec
// specs/2026-10-27-classroom-availability-rewrite Requirement 3.4）。
var ErrTermTextInvalid = fmt.Errorf("%w: %w",
	model.ErrUpstreamCalendarInvalid, errors.New("学期页文本不符合预期"))

// ErrWeekTextInvalid 表示周历页文本解析失败：没有「第 X 周 / Y 周」文本，
// 或 X、Y 不满足 1 ≤ X ≤ Y（旧 spec specs/2026-10-27-classroom-availability-rewrite
// Requirement 3.3）。
// 总周数上限 52 是本包护栏、已由用户拍板：它不来自上游文档，也不出自任何 spec 条目。
// 与 ErrTermTextInvalid 分开，便于上层区分「学期页坏了」与「周历页坏了」；
// 两者都归属 model.ErrUpstreamCalendarInvalid。
var ErrWeekTextInvalid = fmt.Errorf("%w: %w",
	model.ErrUpstreamCalendarInvalid, errors.New("周历页文本不符合预期"))
