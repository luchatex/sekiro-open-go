// Package config 提供 sekiro-go 服务端配置。
// 对齐原 Java 版 config.properties：sekiro.port（默认 5612）、sekiro.localDebug。
package config

import (
	"flag"
	"log/slog"
	"os"
	"strconv"
)

type Config struct {
	// Port 服务监听端口，默认 5612（对齐 sekiro.port）
	Port int
	// LocalDebug 调试模式，对齐 sekiro.localDebug
	LocalDebug bool
	// OpDisabled 关闭 /op 监控页面与 /op/api/stats（公网无鉴权部署可关）
	OpDisabled bool
	// LogLevel 日志级别（debug/info/warn/error，大小写不敏感），默认 info。
	// 内部部署若不需要逐笔调用记录（监控页已可查），设为 warn 可将日志量
	// 降到近零增长（仅注册/告警/审计落盘）
	LogLevel string
	// TLSCert/TLSCertKey 提供后启用独立 TLS 监听（wss，浏览器 https 页面
	// 可直连本机 wss://127.0.0.1:TLSPort，不受 LNA 拦截——LNA 只拦明文 ws）。
	// 证书可为自签（配合 mkcert / openssl+certutil 导入本地信任库）。
	TLSCert    string
	TLSCertKey string
	// TLSPort TLS 监听端口，默认 5613；TLSCert 为空时不启用
	TLSPort int
	// OpPassword /op 监控路径访问密码；空 = 不启用鉴权（向后兼容）。
	// 启用后 /op 与 /op/api/* 需登录会话 Cookie；登录失败 3 次锁定来源 IP 2 分钟。
	OpPassword string
}

func Load() *Config {
	c := &Config{Port: 5612, TLSPort: 5613}
	if v := os.Getenv("SEKIRO_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Port = p
		}
	}
	if v := os.Getenv("SEKIRO_LOCAL_DEBUG"); v == "true" || v == "1" {
		c.LocalDebug = true
	}
	if v := os.Getenv("SEKIRO_OP_DISABLE"); v == "true" || v == "1" {
		c.OpDisabled = true
	}
	if v := os.Getenv("SEKIRO_TLS_CERT"); v != "" {
		c.TLSCert = v
	}
	if v := os.Getenv("SEKIRO_TLS_KEY"); v != "" {
		c.TLSCertKey = v
	}
	if v := os.Getenv("SEKIRO_TLS_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.TLSPort = p
		}
	}
	if v := os.Getenv("SEKIRO_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("SEKIRO_OP_PASSWORD"); v != "" {
		c.OpPassword = v
	}
	flag.IntVar(&c.Port, "port", c.Port, "sekiro server listen port")
	flag.BoolVar(&c.LocalDebug, "debug", c.LocalDebug, "local debug mode")
	flag.StringVar(&c.LogLevel, "log-level", c.LogLevel, "log level: debug, info, warn or error (default info)")
	flag.BoolVar(&c.OpDisabled, "op-disable", c.OpDisabled, "disable /op monitor page")
	flag.StringVar(&c.TLSCert, "tls-cert", c.TLSCert, "TLS certificate file (enables wss listener)")
	flag.StringVar(&c.TLSCertKey, "tls-key", c.TLSCertKey, "TLS private key file")
	flag.IntVar(&c.TLSPort, "tls-port", c.TLSPort, "TLS listen port (wss)")
	flag.StringVar(&c.OpPassword, "op-password", c.OpPassword, "password for /op monitor paths (empty = no auth)")
	flag.Parse()
	return c
}

// SlogLevel 将配置映射为 slog 级别。优先级：-debug > -log-level > 默认 Info。
// 非法值降级为 Info 并返回 ok=false（调用方负责告警提示）。
func (c *Config) SlogLevel() (level slog.Level, ok bool) {
	if c.LocalDebug {
		return slog.LevelDebug, true
	}
	if c.LogLevel == "" {
		return slog.LevelInfo, true
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return slog.LevelInfo, false
	}
	return l, true
}
