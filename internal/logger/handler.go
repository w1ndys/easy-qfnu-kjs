package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// ANSI 颜色代码
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
	ColorGray   = "\033[37m"
)

type GeekHandler struct {
	w  io.Writer
	mu sync.Mutex
}

func NewGeekHandler(w io.Writer) *GeekHandler {
	return &GeekHandler{w: w}
}

func (h *GeekHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

func (h *GeekHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	var prefix, color string

	switch r.Level {
	case slog.LevelDebug:
		prefix = "[DEBUG]"
		color = ColorGray
	case slog.LevelInfo:
		prefix = "[INFO ]"
		color = ColorCyan
	case slog.LevelWarn:
		prefix = "[WARN ]"
		color = ColorYellow
	case slog.LevelError:
		prefix = "[ERROR]"
		color = ColorRed
	case slog.Level(12): // 自定义 Fatal 级别
		prefix = "[FATAL]"
		color = ColorPurple
	default:
		prefix = "[?????]"
		color = ColorReset
	}

	// 格式化时间戳
	timeStr := r.Time.Format("2006-01-02 15:04:05.000")

	// 主日志消息
	// 格式：[颜色]时间 [级别] >> 消息[重置颜色]
	msg := fmt.Sprintf("%s%s %s >> %s%s", color, timeStr, prefix, r.Message, ColorReset)

	// 处理属性（如果有）
	// 我们将它们作为 key=value 对附加在消息后面，使用灰色显示
	if r.NumAttrs() > 0 {
		msg += fmt.Sprintf(" %s{", ColorGray)
		r.Attrs(func(a slog.Attr) bool {
			msg += fmt.Sprintf(" %s=%v", a.Key, a.Value.Any())
			return true
		})
		msg += fmt.Sprintf(" }%s", ColorReset)
	}

	_, err := fmt.Fprintln(h.w, msg)
	return err
}

func (h *GeekHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	// 为了简化这个极客实现，我们返回相同的处理器
	// 在完整的实现中，我们会复制处理器并添加预格式化的属性
	return h
}

func (h *GeekHandler) WithGroup(name string) slog.Handler {
	return h
}

// FanoutHandler 将日志广播给多个处理器
type FanoutHandler struct {
	handlers []slog.Handler
}

func NewFanoutHandler(handlers ...slog.Handler) *FanoutHandler {
	return &FanoutHandler{handlers: handlers}
}

func (h *FanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *FanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			if err := handler.Handle(ctx, r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (h *FanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithAttrs(attrs)
	}
	return NewFanoutHandler(newHandlers...)
}

func (h *FanoutHandler) WithGroup(name string) slog.Handler {
	newHandlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		newHandlers[i] = handler.WithGroup(name)
	}
	return NewFanoutHandler(newHandlers...)
}
