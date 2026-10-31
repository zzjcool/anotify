// Package server 装配 HTTP 服务：路由、中间件、静态资源（CDN 缓存分级）。
package server

import (
	"crypto/rand"
	"encoding/hex"
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

	// Pprof 打开 /debug/pprof/*（性能评估用，默认关闭）。
	//
	// 为什么要有开关而不是始终开：pprof 端点会泄露 goroutine 栈与堆分布，
	// 生产环境不该默认暴露。评估/排障时用 ANOTIFY_PPROF=1 打开，
	// 采样完就关（pprof 的 CPU 采样本身有 ~5% 开销）。
	Pprof bool

	// BootID 是**本次进程启动**的标识，由 /health 回显。
	//
	// 为什么需要它：/health 只回 {"ok":true} 时，调用方无法区分「我起的那个进程」
	// 和「同一个端口上别人的/上次残留的进程」——两者都返回 200。
	// e2e 的并行套件、以及"上一次 e2e 的 server 没退干净"这两种场景，
	// 都会让测试**静默连上别人的后端**，症状是随机的 401 和造数 undefined
	// （表现为 0通过/0失败，看起来像超时或代码坏了，排查成本极高，见 issue #215）。
	//
	// 默认随机生成（每次启动都不同）；显式设 ANOTIFY_BOOT_ID 时用它，
	// 便于外部在启动前就知道该等哪个实例。
	BootID string
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
		BootID:      bootIDFromEnv(),
		Pprof:       get("ANOTIFY_PPROF", "") != "",
	}
}

// bootIDFromEnv 取 ANOTIFY_BOOT_ID，未设则随机生成 8 字节（16 位十六进制）。
func bootIDFromEnv() string {
	if v := os.Getenv("ANOTIFY_BOOT_ID"); v != "" {
		return v
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 拿不到随机数不该阻止启动：退化成固定值，身份校验会失效但服务可用。
		return "unknown"
	}
	return hex.EncodeToString(b[:])
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
