package logger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

var DefaultLogger *slog.Logger

func init() {
	// 1. 控制台处理器（极客风格，带颜色）
	consoleHandler := NewGeekHandler(os.Stdout)

	// 2. 文件处理器（JSON 结构化）带自动轮转
	// 目录由 NewLogRotator 自动创建
	// 10MB 轮转一次
	rotator := NewLogRotator("logs", 10)
	fileHandler := slog.NewJSONHandler(rotator, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})

	// 3. 组合处理器
	finalHandler := NewFanoutHandler(consoleHandler, fileHandler)

	DefaultLogger = slog.New(finalHandler)
	slog.SetDefault(DefaultLogger)
}

func Info(format string, v ...interface{}) {
	if len(v) == 0 {
		DefaultLogger.Info(format)
	} else {
		DefaultLogger.Info(fmt.Sprintf(format, v...))
	}
}

func Warn(format string, v ...interface{}) {
	if len(v) == 0 {
		DefaultLogger.Warn(format)
	} else {
		DefaultLogger.Warn(fmt.Sprintf(format, v...))
	}
}

func Error(format string, v ...interface{}) {
	if len(v) == 0 {
		DefaultLogger.Error(format)
	} else {
		DefaultLogger.Error(fmt.Sprintf(format, v...))
	}
}

func Fatal(format string, v ...interface{}) {
	msg := format
	if len(v) > 0 {
		msg = fmt.Sprintf(format, v...)
	}
	// 使用 Output 来记录正确的堆栈深度（如果需要），
	// 但这里我们只使用 LevelError（或者如果我们添加了自定义的 Fatal 级别）
	// Slog 默认没有 Fatal 级别，通常我们会记录 Error 然后退出。
	// 但为了保持风格一致性：
	// 我们可以手动调用 Handler.Handle 并传入自定义级别或仅使用 Error
	// 让我们坚持使用 Error 级别记录日志，但通过修改级别来显示 [FATAL] 前缀
	// Slog 的级别只是整数。
	// LevelError = 8。我们将 Fatal 设为 12。

	// 实际上，为了简单起见，我们直接使用自定义的 Log 调用
	DefaultLogger.Log(context.Background(), slog.Level(12), msg)
	os.Exit(1)
}

// 支持结构化日志
func InfoS(msg string, args ...any) {
	DefaultLogger.Info(msg, args...)
}

func WarnS(msg string, args ...any) {
	DefaultLogger.Warn(msg, args...)
}

func ErrorS(msg string, args ...any) {
	DefaultLogger.Error(msg, args...)
}
