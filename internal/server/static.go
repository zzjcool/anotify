package server

import (
	"log"
	"net/http"
	"os"
	"path"
	"strings"
)

// cacheClass 判定静态资源的 CDN 缓存分级。
type cacheClass int

const (
	cacheImmutable cacheClass = iota // 哈希指纹资源：immutable 1 年
	cacheEntry                       // 入口 HTML / sw.js：短缓存 + ETag
	cacheNoStore                     // 动态 API：no-store
)

// classify 按文件名判断是否带内容哈希指纹（name.<hash8>.ext）。
func classify(name string) cacheClass {
	base := path.Base(name)
	if base == "sw.js" || strings.HasSuffix(base, ".html") || base == "manifest.json" {
		return cacheEntry
	}
	// 带指纹：stem.<8位hex>.ext
	stem := strings.TrimSuffix(base, path.Ext(base))
	if i := strings.LastIndex(stem, "."); i >= 0 {
		h := stem[i+1:]
		if len(h) == 8 && isHex(h) {
			return cacheImmutable
		}
	}
	return cacheEntry
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// staticHandler 返回带 CDN 缓存分级的静态资源处理器。
// root 为本地静态目录（指纹脚本产物 dist/）。
func staticHandler(root string) http.Handler {
	return staticFS(http.Dir(root))
}

// staticFS 用指定的文件系统返回带 CDN 缓存分级的静态处理器（供 embed.FS 或本地目录）。
func staticFS(fsys http.FileSystem) http.Handler {
	fs := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch classify(r.URL.Path) {
		case cacheImmutable:
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case cacheEntry:
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		fs.ServeHTTP(w, r)
	})
}

// resolveStaticFS 返回静态资源文件系统（供 /agent-login.sh 等显式路由读取）。
func resolveStaticFS(cfg Config) http.FileSystem {
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			return http.Dir(cfg.StaticDir)
		}
		log.Printf("[static] 目录 %s 不存在，回退 embed", cfg.StaticDir)
	}
	return embeddedStaticMust()
}

// embeddedStaticMust 返回 embed 文件系统，失败时返回空 FS（避免 panic）。
func embeddedStaticMust() http.FileSystem {
	fs, err := embeddedStatic()
	if err != nil {
		log.Printf("[static] embed 不可用: %v", err)
		return http.Dir("") // 空，Open 必失败 → 404
	}
	return fs
}
func resolveStatic(cfg Config) http.Handler {
	// StaticDir 非空 → 本地目录（开发）；否则 → embed 内嵌（生产单二进制）。
	// CDN 前缀重写（CDNPrefix）在反向代理/CDN 层做，源站始终服务同一 dist。
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			return staticHandler(cfg.StaticDir)
		}
		log.Printf("[static] 目录 %s 不存在，回退 embed", cfg.StaticDir)
	}
	efs, err := embeddedStatic()
	if err != nil {
		log.Printf("[static] embed 不可用: %v，返回 503", err)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "static assets unavailable", http.StatusServiceUnavailable)
		})
	}
	log.Printf("[static] 使用 embed 内嵌前端")
	return staticFS(efs)
}

// noStore 中间件：动态 API 禁止缓存。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
