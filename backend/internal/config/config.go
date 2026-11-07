// Package config 从环境变量读取进程启动所需的非秘密配置。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/w1ndys/easy-qfnu-kjs/internal/model"
)

// LogLevel 是日志级别，取值受枚举约束。
type LogLevel string

// 日志级别的全部合法取值。
const (
	LogDebug LogLevel = "debug"
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

const (
	// defaultListenAddr 是未显式配置监听地址时使用的默认端口。
	defaultListenAddr = ":18080"
	// defaultOCRTimeout 是未显式配置 OCR 超时时的默认值。
	defaultOCRTimeout = 10 * time.Second
	// defaultSecretKeyPath 是加密密钥文件的默认位置（design D2）。
	// 目录不存在时由 secret.Manager.Create 在初始化写入前按 0700 补建，
	// 所以这里只给路径，不在启动期创建任何东西。
	defaultSecretKeyPath = "/var/lib/easy-qfnu-kjs/secret.key"
)

// Config 是启动所需的非秘密配置。
type Config struct {
	DatabaseURL       string        // PostgreSQL 连接串，必填
	ListenAddr        string        // HTTP 监听地址
	LogLevel          LogLevel      // 日志级别
	OCRTimeout        time.Duration // OCR 请求超时，必须为正
	TrustProxyHeaders bool          // 是否信任前置反代给出的来源头（X-Forwarded-For / X-Real-IP）
	SecretKeyPath     string        // 加密密钥文件路径；只给路径，文件由管理端初始化写出
	AdminCookieSecure bool          // 管理会话 Cookie 是否带 Secure；默认 true，本地 http 开发需显式关闭
}

// Load 读取并校验环境变量，任一项不合法即返回错误，供入口拒绝启动。
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:        defaultListenAddr,
		LogLevel:          LogInfo,
		OCRTimeout:        defaultOCRTimeout,
		SecretKeyPath:     defaultSecretKeyPath,
		AdminCookieSecure: true,
		// design D2 的部署拓扑里 server 只被 web 反代访问，默认信任反代来源头；
		// server 若被直接暴露给客户端，必须设 TRUST_PROXY_HEADERS=false，否则来源可被伪造。
		TrustProxyHeaders: true,
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	// 未设置与设成空串都等于没有连接串，两种都拒绝启动。
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("%w: DATABASE_URL 未设置", model.ErrConfigInvalid)
	}
	// 连接串必须带 PostgreSQL 协议头，否则错误会被推迟到连库时，启动期更难定位。
	if !strings.HasPrefix(cfg.DatabaseURL, "postgres://") && !strings.HasPrefix(cfg.DatabaseURL, "postgresql://") {
		return Config{}, fmt.Errorf("%w: DATABASE_URL 缺少 postgres:// 或 postgresql:// 前缀", model.ErrConfigInvalid)
	}

	if addr := os.Getenv("HTTP_ADDR"); addr != "" {
		cfg.ListenAddr = addr
	}

	if raw := os.Getenv("LOG_LEVEL"); raw != "" {
		// 环境变量在类型系统之外，拼错的级别必须当场失败而不是静默退回默认值。
		switch level := LogLevel(raw); level {
		case LogDebug, LogInfo, LogWarn, LogError:
			cfg.LogLevel = level
		default:
			return Config{}, fmt.Errorf("%w: LOG_LEVEL 取值为 %q，只接受 debug/info/warn/error", model.ErrConfigInvalid, raw)
		}
	}

	if raw := os.Getenv("OCR_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		// 解析失败与零/负超时都让 OCR 请求不可用，统一在启动期拒绝。
		if err != nil || timeout <= 0 {
			return Config{}, fmt.Errorf("%w: OCR_TIMEOUT 必须是正时长，收到 %q", model.ErrConfigInvalid, raw)
		}
		cfg.OCRTimeout = timeout
	}

	if raw := os.Getenv("TRUST_PROXY_HEADERS"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		// 布尔开关拼错等于把来源判定交给默认值，安全相关的配置必须当场失败。
		if err != nil {
			return Config{}, fmt.Errorf("%w: TRUST_PROXY_HEADERS 只接受 true/false/1/0，收到 %q", model.ErrConfigInvalid, raw)
		}
		cfg.TrustProxyHeaders = enabled
	}

	// 密钥路径只要求最终非空：空串与未设置同义，都落到设计默认路径；绝对/相对路径、
	// 父目录存不存在都留给 secret.Manager，启动期不碰文件系统（design「配置项」）。
	if raw := os.Getenv("SECRET_KEY_PATH"); raw != "" {
		cfg.SecretKeyPath = raw
	}

	if raw := os.Getenv("ADMIN_COOKIE_SECURE"); raw != "" {
		enabled, err := strconv.ParseBool(raw)
		// 与管理 Cookie 的 Secure 属性直接相关：拼错时静默默认 true 会让本地 http 开发
		// 的浏览器直接丢弃 Cookie，且很难从现象反推配置写错，所以照 TRUST_PROXY_HEADERS 当场失败。
		if err != nil {
			return Config{}, fmt.Errorf("%w: ADMIN_COOKIE_SECURE 只接受 true/false/1/0，收到 %q", model.ErrConfigInvalid, raw)
		}
		cfg.AdminCookieSecure = enabled
	}

	return cfg, nil
}
