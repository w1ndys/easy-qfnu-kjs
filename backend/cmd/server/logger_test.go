package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/w1ndys/easy-qfnu-kjs/internal/config"
)

// TestNewLogger 覆盖级别映射与过滤：级别之下的日志不落到输出里。
func TestNewLogger(t *testing.T) {
	cases := []struct {
		name        string
		level       config.LogLevel
		wantDebug   bool // debug 日志是否出现
		wantInfo    bool // info 日志是否出现
		wantWarn    bool // warn 日志是否出现
		wantErrLine bool // error 日志是否出现
	}{
		{name: "debug 级别全部输出", level: config.LogDebug, wantDebug: true, wantInfo: true, wantWarn: true, wantErrLine: true},
		{name: "info 级别过滤 debug", level: config.LogInfo, wantInfo: true, wantWarn: true, wantErrLine: true},
		{name: "warn 级别过滤 info", level: config.LogWarn, wantWarn: true, wantErrLine: true},
		{name: "error 级别只留 error", level: config.LogError, wantErrLine: true},
		{name: "未知级别兜底为 info", level: config.LogLevel("verbose"), wantInfo: true, wantWarn: true, wantErrLine: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newLogger(&buf, c.level)
			logger.Debug("d")
			logger.Info("i")
			logger.Warn("w")
			logger.Error("e")
			out := buf.String()

			// 每条日志按 msg 字段判断是否出现，避免误匹配级别名本身。
			check := func(msg string, want bool) {
				got := strings.Contains(out, `"msg":"`+msg+`"`)
				if got != want {
					t.Fatalf("msg=%q 期望出现=%v，实际=%v，输出=%s", msg, want, got, out)
				}
			}
			check("d", c.wantDebug)
			check("i", c.wantInfo)
			check("w", c.wantWarn)
			check("e", c.wantErrLine)
		})
	}
}
