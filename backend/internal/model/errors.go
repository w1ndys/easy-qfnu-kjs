// Package model 存放跨层共享的实体与错误定义。
package model

import "errors"

// ErrConfigInvalid 表示启动配置缺失或取值非法，入口据此拒绝启动。
var ErrConfigInvalid = errors.New("启动配置非法")

// ErrStoreUnavailable 表示数据库连接池不可用，入口据此拒绝启动。
var ErrStoreUnavailable = errors.New("数据库不可用")

// ErrMigrationFailed 表示结构变更失败，入口据此拒绝启动。
var ErrMigrationFailed = errors.New("结构变更失败")

// ErrQueryFailed 表示一次数据库读取失败，供上层映射成内部错误。
var ErrQueryFailed = errors.New("数据读取失败")

// ErrUpstreamSessionExpired 表示上游响应是登录页或「非法访问」，会话已失效。
// 上层据此重登录一次后重跑当前任务，而不是把登录页当数据页解析（Requirement 2.5）。
var ErrUpstreamSessionExpired = errors.New("上游会话失效")

// ErrUpstreamFailed 表示一次上游请求在重试耗尽后仍失败：非 200、网络错误或读取失败。
// 这是任务级错误，由 pipeline 按任务的必需/可选属性决定轮次走向。
var ErrUpstreamFailed = errors.New("上游请求失败")

// ErrStatsFailed 表示一次访问统计的写入或清理失败。
// 统计是旁路数据：失败只记日志，不改变查询响应与 HTTP 状态码（CP18）。
var ErrStatsFailed = errors.New("访问统计失败")
