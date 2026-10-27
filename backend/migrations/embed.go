// Package migrations 以只读文件系统形式提供建表脚本，供 store 按序执行。
package migrations

import "embed"

// Files 保存全部 .sql 脚本；文件名前缀即版本号，例如 0001_baseline.sql。
//
//go:embed *.sql
var Files embed.FS
