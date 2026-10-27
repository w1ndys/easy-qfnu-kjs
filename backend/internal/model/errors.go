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
