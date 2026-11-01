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

// TestLoadTrustProxyHeaders 覆盖来源信任开关：默认信任前置反代，可显式关闭，非法值拒绝启动。
func TestLoadTrustProxyHeaders(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "未设置时默认信任反代", raw: "", want: true},
		{name: "显式开启", raw: "true", want: true},
		{name: "显式关闭", raw: "false", want: false},
		{name: "用 0 关闭", raw: "0", want: false},
		{name: "非法值拒绝启动", raw: "yes", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:p@127.0.0.1:5432/db")
			t.Setenv("TRUST_PROXY_HEADERS", c.raw)

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
			if cfg.TrustProxyHeaders != c.want {
				t.Fatalf("TrustProxyHeaders 期望 %v，实际 %v", c.want, cfg.TrustProxyHeaders)
			}
		})
	}
}
