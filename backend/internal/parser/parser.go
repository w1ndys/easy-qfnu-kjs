// Package parser 把教务上游的原始 HTML 解析成结构化事实。
//
// 只读字节：不发请求、不落库，也不做语义清洗（状态原文与原始房名交给 clean）。
// 页面结构与本包期望不符时返回可区分的哨兵错误，错误信息只描述结构差异与位置，
// 不包含页面正文（docs/pitfalls.md 第 8 节）。
package parser

import "errors"

// ErrMatrixStructureInvalid 表示周矩阵页面结构与当前轴定义不符：
// 找不到 table#dataList、表头状态格集合或顺序不一致、数据行单元格数量与表头不一致、
// 房号三处来源都取不到，或同一响应内房号重复。
// 上层据此映射 error_code=matrix_structure_invalid 并把整轮置为失败（Requirement 2）。
var ErrMatrixStructureInvalid = errors.New("矩阵结构不符合预期")

// ErrAxisInvalid 表示传入的节次轴不足以校验表头：节点为空、缺大节信息、同一大节序号
// 对应多个编码，或大节序号不连续。这是配置数据问题而不是页面问题，
// 所以与 ErrMatrixStructureInvalid 分开，便于上层区分「页面变了」与「轴数据坏了」。
var ErrAxisInvalid = errors.New("节次轴定义不可用")
