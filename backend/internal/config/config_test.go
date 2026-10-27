package config

import (
	"errors"
	"testing"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// TestLoad 覆盖连接串、监听地址、日志级别与 OCR 超时的读取与校验。
func TestLoad(t *testing.T) {
	cases := []struct {
		name     string
		dsn      string
		addr     string
		level    string
		timeout  string
		wantErr  bool
		wantAddr string
		wantLev  LogLevel
		wantDur  time.Duration
	}{
		{name: "连接串未设置", wantErr: true},
		{name: "连接串无协议头", dsn: "127.0.0.1:5432/db", wantErr: true},
		{name: "连接串协议头不支持", dsn: "mysql://u:p@h/db", wantErr: true},
		{name: "最小合法配置", dsn: "postgres://u:p@127.0.0.1:5432/db", wantAddr: ":18080", wantLev: LogInfo, wantDur: 10 * time.Second},
		{name: "自定义端口", dsn: "postgresql://u:p@h/db", addr: ":9000", wantAddr: ":9000", wantLev: LogInfo, wantDur: 10 * time.Second},
		{name: "日志级别合法", dsn: "postgres://u:p@h/db", level: "debug", wantAddr: ":18080", wantLev: LogDebug, wantDur: 10 * time.Second},
		{name: "日志级别拼错", dsn: "postgres://u:p@h/db", level: "verbose", wantErr: true},
		{name: "OCR 超时合法", dsn: "postgres://u:p@h/db", timeout: "3s", wantAddr: ":18080", wantLev: LogInfo, wantDur: 3 * time.Second},
		{name: "OCR 超时为零", dsn: "postgres://u:p@h/db", timeout: "0s", wantErr: true},
		{name: "OCR 超时为负", dsn: "postgres://u:p@h/db", timeout: "-3s", wantErr: true},
		{name: "OCR 超时不是时长", dsn: "postgres://u:p@h/db", timeout: "10", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", c.dsn)
			t.Setenv("HTTP_ADDR", c.addr)
			t.Setenv("LOG_LEVEL", c.level)
			t.Setenv("OCR_TIMEOUT", c.timeout)

			cfg, err := Load()
			// 期望失败的分支一律校验错误可被 ErrConfigInvalid 识别。
			if c.wantErr {
				if !errors.Is(err, model.ErrConfigInvalid) {
					t.Fatalf("期望 ErrConfigInvalid，实际 err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际 err=%v", err)
			}
			if cfg.ListenAddr != c.wantAddr {
				t.Fatalf("ListenAddr 期望 %q，实际 %q", c.wantAddr, cfg.ListenAddr)
			}
			if cfg.LogLevel != c.wantLev {
				t.Fatalf("LogLevel 期望 %q，实际 %q", c.wantLev, cfg.LogLevel)
			}
			if cfg.OCRTimeout != c.wantDur {
				t.Fatalf("OCRTimeout 期望 %s，实际 %s", c.wantDur, cfg.OCRTimeout)
			}
		})
	}
}
