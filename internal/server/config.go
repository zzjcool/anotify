// Package server 装配 HTTP 服务：路由、中间件、静态资源（CDN 缓存分级）。
package server

import (
	"fmt"
	"os"
)

// Config 服务配置（全部可用环境变量覆盖）。
type Config struct {
	Addr        string // 监听地址，默认 :8080
	DBPath      string // SQLite 路径，默认 ./anotify.db
	CDNPrefix   string // 静态资源 CDN 前缀（如 https://cdn.trycloudflare.com）；空=本地 embed
	StaticDir   string // 本地静态目录（开发期），默认 ./dist
	VAPIDPublic string // VAPID 公钥
	VAPIDPriv   string // VAPID 私钥（仅服务端）
	VAPIDSub    string // VAPID subject（mailto:）
	RPDisplay   string // WebAuthn Relying Party 显示名
	RPID        string // WebAuthn RP ID（域名）
	RPOrigin    string // WebAuthn Origin（含协议）
	TrustProxy  bool   // 信任 X-Forwarded-For（仅在反代 cloudflared/nginx 后开启）
	LogLevel    string // 日志级别：debug/info/warn/error，默认 info
	LogFormat   string // 日志格式：json/text，默认 json
}

// FromEnv 从环境变量加载配置。
func FromEnv() Config {
	return Config{
		Addr:      get("ANOTIFY_ADDR", ":8080"),
		DBPath:    get("ANOTIFY_DB", "./anotify.db"),
		CDNPrefix: get("ANOTIFY_CDN_PREFIX", ""),
		// 默认空 → 使用 embed 内嵌前端（生产单二进制）；
		// 开发期显式设 ANOTIFY_STATIC=./web 走本地目录。
		StaticDir:   get("ANOTIFY_STATIC", ""),
		VAPIDPublic: get("ANOTIFY_VAPID_PUBLIC_KEY", ""),
		VAPIDPriv:   get("ANOTIFY_VAPID_PRIVATE_KEY", ""),
		VAPIDSub:    get("ANOTIFY_VAPID_SUBJECT", "mailto:notify@example.com"),
		RPDisplay:   get("ANOTIFY_RP_DISPLAY", "Anotify"),
		RPID:        get("ANOTIFY_RP_ID", "localhost"),
		RPOrigin:    get("ANOTIFY_RP_ORIGIN", "http://localhost:8080"),
		TrustProxy:  get("ANOTIFY_TRUST_PROXY", "") != "",
		LogLevel:    get("ANOTIFY_LOG_LEVEL", "info"),
		LogFormat:   get("ANOTIFY_LOG_FORMAT", "json"),
	}
}

// Validate 校验关键配置。
func (c Config) Validate() error {
	if c.VAPIDPublic == "" || c.VAPIDPriv == "" {
		return fmt.Errorf("缺少 VAPID 配置（ANOTIFY_VAPID_PUBLIC_KEY / ANOTIFY_VAPID_PRIVATE_KEY）")
	}
	return nil
}

func get(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
