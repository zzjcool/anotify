// Anotify 服务端单二进制入口。
//
// 路径分离：
//
//	/v1/*     动态 API（no-store），含 /v1/notify、/v1/auth/*、/v1/devices、/v1/keys、/v1/stream
//	/*        静态资源（CDN 缓存分级：哈希文件 immutable，入口短缓存+ETag）
package main

import (
	"log"
	"net/http"

	"github.com/anotify/anotify/internal/server"
)

func main() {
	cfg := server.FromEnv()
	if err := cfg.Validate(); err != nil {
		log.Printf("[warn] %v（推送功能将不可用，其余可运行）", err)
	}

	mux := server.NewMux(cfg)

	log.Printf("✅ Anotify 启动 %s", cfg.Addr)
	log.Printf("   静态目录: %s  CDN前缀: %q", cfg.StaticDir, cfg.CDNPrefix)
	if err := http.ListenAndServe(cfg.Addr, mux); err != nil {
		log.Fatal(err)
	}
}
