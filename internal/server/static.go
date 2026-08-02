package server

import (
	"net/http"
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
	fs := http.FileServer(http.Dir(root))
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

// noStore 中间件：动态 API 禁止缓存。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
