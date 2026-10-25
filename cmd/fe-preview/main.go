// Command fe-preview 启动一个**纯静态前端预览**服务器。
//
// 用途：只想看前端长什么样时，不要被后端链路拖累。
//
//	make fe-preview        → http://localhost:7788
//
// 它做三件事（也**只**做这三件）：
//
//	① 静态托管 web/ 目录（sitegen 的产物）
//	② 把 ?lang=en 之类映射到 web/en/... 的多语言版本
//	③ 给每个 HTML 注入 mock.js —— 于是所有 /v1/* 请求都在浏览器里被拦下，
//	   由 web-src/mock.js 直接应答，一个请求都不会打到真实后端
//
// 为什么不在 Go 侧假装一个后端（用同样的 handler 返回假 JSON）：
//
//   - 那等于维护第二套后端，会跟真后端漂移，看到的"效果"就不可信
//   - mock 里要做状态（新建源、触发、任务执行完），Go 侧等于重写一遍
//   - 浏览器侧 mock 改一行就生效，不用重新编译
//
// 所以这里的 Go 极其薄：只发文件 + 注入一行 <script>。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func main() {
	var (
		dir  = flag.String("dir", "web", "静态站点目录（sitegen 的产物）")
		addr = flag.String("addr", "localhost:7788", "监听地址")
		port = flag.Int("port", 0, "端口（覆盖 addr 里的端口）")
	)
	flag.Parse()

	root, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("解析目录失败: %v", err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "目录不存在：%s\n先跑 `make fe` 生成 web/。\n", root)
		os.Exit(1)
	}

	listen := *addr
	if *port > 0 {
		if i := strings.LastIndex(listen, ":"); i >= 0 {
			listen = listen[:i]
		}
		listen = fmt.Sprintf("%s:%d", listen, *port)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, root)
	})

	fmt.Println()
	fmt.Println("  ▶ 前端预览（mock 数据，不连后端）")
	fmt.Printf("    http://%s\n", listen)
	fmt.Println()
	fmt.Println("    页面：/sources.html  /capabilities.html  /events.html  /nodes.html")
	fmt.Println("    多语言：加 ?lang=en / ?lang=ja / ?lang=es")
	fmt.Println("    看错误态：任意接口加 ?mockFail=1")
	fmt.Println("    Ctrl-C 停止")
	fmt.Println()

	if err := http.ListenAndServe(listen, mux); err != nil {
		log.Fatal(err)
	}
}

// serveStatic 发静态文件；HTML 会被注入 mock.js。
func serveStatic(w http.ResponseWriter, r *http.Request, root string) {
	upath := path.Clean("/" + r.URL.Path)

	// 多语言：?lang=en 时优先找 web/en/<page>
	if lang := r.URL.Query().Get("lang"); lang != "" && isLang(lang) {
		cand := filepath.Join(root, lang, strings.TrimPrefix(upath, "/"))
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			serveFile(w, r, cand, true)
			return
		}
	}

	// 目录 → index.html
	if strings.HasSuffix(upath, "/") {
		upath += "index.html"
	}
	full := filepath.Join(root, filepath.FromSlash(upath))

	// 防目录穿越：Join 之后再确认结果仍在 root 之内
	if rel, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(rel, "..") {
		http.NotFound(w, r)
		return
	}

	fi, err := os.Stat(full)
	if err != nil || fi.IsDir() {
		// 找不到时给 index.html（前端是单层静态页，没有路由）
		if upath != "/index.html" {
			http.NotFound(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	serveFile(w, r, full, strings.HasSuffix(full, ".html"))
}

func isLang(s string) bool {
	switch s {
	case "en", "ja", "es", "zh-CN":
		return true
	}
	return false
}

// serveFile 发文件；HTML 注入 mock.js。
//
// 注入点选在 </head> 之前：必须早于页面自己的 <script>，
// 否则页面代码先拿到原生 fetch，mock 就拦不住了。
func serveFile(w http.ResponseWriter, r *http.Request, full string, inject bool) {
	if !inject {
		http.ServeFile(w, r, full)
		return
	}

	raw, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, "读取文件失败", http.StatusInternalServerError)
		return
	}

	html := string(raw)
	// mock.js 用绝对路径：多语言页面在 /en/ 下，相对路径会找错
	const tag = `<script src="/mock.js"></script>`
	if i := strings.Index(html, "</head>"); i >= 0 {
		html = html[:i] + tag + "\n" + html[i:]
	} else {
		// 没有 </head> 的页面（不该有）就放最前面，总比不注入强
		html = tag + html
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 预览时禁缓存：改了 mock.js 或 sitegen 产物，刷新就能看到
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}
