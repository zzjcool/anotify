package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cacheClass 判定静态资源的 CDN 缓存分级。
type cacheClass int

const (
	cacheImmutable cacheClass = iota // 哈希指纹资源：immutable 1 年
	cacheEntry                       // 入口 HTML / sw.js / manifest：短缓存 + ETag（过期后 304 空体）
	cacheNoStore                     // 动态 API：no-store
)

// 静态资源的 Cache-Control 值（契约 §3.2）。
const (
	cacheControlImmutable = "public, max-age=31536000, immutable"
	// cacheControlEntry 是入口类（HTML / sw.js / manifest）的值。
	//
	// 为什么是 max-age=60 而不是 no-cache：#232 的病根是"没有校验器"（所以每次导航
	// 都全量重传 48KB），不是"max-age 太长"。补上强 ETag 之后两种取值都不再传正文：
	//   - max-age=60：60s 内浏览器直接用缓存（**0 个请求**），过期后 1 次条件请求 → 304 空体；
	//   - no-cache  ：每次导航都回源（RTT 变多），字节同样为 0。
	// 字节口径上 max-age=60 不劣于 no-cache（还省一个往返），且本仓库的
	// api_contract 套件已把它写成产品契约（index.html 的 Cache-Control 含
	// max-age=60），改它会跨区改动别人的测试。代价是部署后最多 60s 的 HTML 陈旧
	// 窗口 —— 但静态资源全是内容指纹（旧 HTML 只会引用旧指纹资源，不会 404）。
	// 若将来要收紧成 no-cache，需同步改：这条注释、static_test.go 的期望值、
	// scripts/e2e/suites/api_contract.mjs 的那条断言。
	cacheControlEntry = "public, max-age=60"
)

// staticInlineMax 是"整包读进内存并缓存"的单文件字节上限（契约 §3.7）。
//
// 超过它的文件改为直接流式服务（http.File 本身就是 io.ReadSeeker）：不压、不发
// ETag，Cache-Control 照常分级。目的是不因将来往 dist 里塞大文件而把整包读进内存。
// 声明成 var（而非 const）便于测试调小。
var staticInlineMax int64 = 8 << 20

// classifyDetail 的规则名（契约 §4）：入口名单 / 入口 HTML / 内容哈希 / 兜底。
//
// 为什么要把"命中哪条规则"也返回出来：#233 的病根是**兜底碰巧正确** ——
// 旧 classify 只写"不是指纹就是入口"，于是 manifest.webmanifest 靠 fallback 蒙到了
// cacheEntry，产物一改名（manifest.webmanifest → site.webmanifest）就静默退化成
// 短缓存且无人发现。规则名让测试能断言"这条分级是显式声明的，不是蒙的"。
const (
	classifyRuleEntryName = "entry-name"
	classifyRuleEntryHTML = "entry-html"
	classifyRuleHash      = "hash"
	classifyRuleFallback  = "fallback"
)

// entryProductNames 是"文件名固定、内容会变"的入口类产物（#233 显式名单）。
//
// 三个名字正是 scripts/hash.mjs 的 NO_FINGERPRINT（不指纹即路径固定）：
//   - sw.js：Service Worker 的注册路径必须是 /sw.js
//   - manifest.json：指纹脚本自身产出的 原名→哈希名 映射表
//   - manifest.webmanifest：PWA 清单，被 HTML 固定引用（图标 src 在构建期改写）
//
// 它们必须每次校验（no-cache + ETag），不能 immutable。
var entryProductNames = map[string]struct{}{
	"sw.js":                {},
	"manifest.json":        {},
	"manifest.webmanifest": {},
}

// classifyDetail 返回缓存分级 + 命中的规则名（#233）。
func classifyDetail(name string) (cacheClass, string) {
	base := path.Base(name)
	if _, ok := entryProductNames[base]; ok {
		return cacheEntry, classifyRuleEntryName
	}
	if strings.HasSuffix(base, ".html") {
		return cacheEntry, classifyRuleEntryHTML
	}
	// 带指纹：stem.<8位hex>.ext
	stem := strings.TrimSuffix(base, path.Ext(base))
	if i := strings.LastIndex(stem, "."); i >= 0 {
		h := stem[i+1:]
		if len(h) == 8 && isHex(h) {
			return cacheImmutable, classifyRuleHash
		}
	}
	return cacheEntry, classifyRuleFallback
}

// classify 按文件名判断缓存分级（签名保持不变：已有测试与调用方依赖它）。
func classify(name string) cacheClass {
	c, _ := classifyDetail(name)
	return c
}

// isHex 报告 s 是否全为小写十六进制字符（空串返回 true，长度由调用方校验）。
func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// staticContentTypes 是内置的扩展名 → Content-Type 显式表（契约 §3.3）。
//
// 为什么不用 mime.TypeByExtension 打头：它会去读宿主 /etc/mime.types，CI 容器、
// macOS 开发机、极简镜像上的结果**不一致**；而浏览器对 MIME 是硬校验的 ——
// .js 一旦被猜成 text/plain，ES module（<script type="module">）直接拒绝执行，
// 表现为"前端整页白屏但 HTTP 全是 200"。所以关键扩展名必须由二进制自己钉死。
var staticContentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".sh":          "text/x-sh; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".map":         "application/json",
}

// staticContentType 走 显式表 → mime.TypeByExtension → ""（表示由调用方 sniff）。
func staticContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := staticContentTypes[ext]; ok {
		return ct
	}
	return mime.TypeByExtension(ext)
}

// staticCacheControl 把分级映射成 Cache-Control 值（契约 §3.2）。
func staticCacheControl(c cacheClass) string {
	if c == cacheImmutable {
		return cacheControlImmutable
	}
	// cacheEntry 之外（cacheNoStore 不用于静态资源）一律按入口类处理：
	// 未知产物宁可每次回源校验，也不能 immutable 一年 —— 后者错了就换不回来。
	return cacheControlEntry
}

// staticETag 返回内容哈希强 ETag（契约 §2：sha256 前 16 hex，带引号）。
//
// suffix 用于区分 representation：gzip 变体传 "-gz"（RFC 9110 §8.8.3：
// 不同 representation 必须有不同 ETag，否则缓存会把明文那份当成压缩那份）。
func staticETag(body []byte, suffix string) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + suffix + `"`
}

// staticEntry 是一个静态文件的缓存条目。
//
// 发布后**不可变**（契约 §3.6）：body/etag/contentType/cacheControl 在构造时定死，
// 之后只被并发读。唯一的惰性字段是 gzip 变体，它由 sync.Once 保护，
// 写入发生在 Once 内部，读也走同一个 Once —— 因此同样是"发布后不可见变化"。
type staticEntry struct {
	name         string
	size         int64
	modtime      time.Time
	body         []byte
	etag         string
	contentType  string
	cacheControl string

	gzOnce sync.Once
	gzBody []byte
	gzETag string
}

// validFor 报告条目是否仍然对应当前文件（契约 §3.8：本地目录按 (size, modtime) 校验）。
//
// embed 模式下两个值恒定，校验永远为真；dev 模式（ANOTIFY_STATIC）改了文件
// 立刻失效 —— 否则改完前端要重启进程才看得到效果。
func (e *staticEntry) validFor(info os.FileInfo) bool {
	return e.size == info.Size() && e.modtime.Equal(info.ModTime())
}

// compressible 报告这个条目值不值得压：可压类型 + 不小于阈值（契约 §3.4）。
//
// 为什么不用 isCompressibleContentType 一条了事：那个 helper 是 /v1/ JSON 中间件的
// 共享判据（middleware.go，本批不改），它不认 PWA 清单的 application/manifest+json。
// 清单是 JSON 文本、会被改（名字/图标/start_url），属于该压的类型；当前产物只有
// 541B（低于阈值，不压也不亏），但没理由在代码层面留一个"长大以后还不压"的坑。
func (e *staticEntry) compressible() bool {
	if len(e.body) < gzipMinBytes {
		return false
	}
	return isCompressibleContentType(e.contentType) || e.contentType == "application/manifest+json"
}

// gzipVariant 返回 gzip 变体（body + ETag），每文件只压一次（契约 §2 的数字决策）。
//
// 等级取 gzip.DefaultCompression：比 BestSpeed 一次性多付 9ms 全集，换来每次冷访问
// 少传 20KB；比 Best(9) 只少 114B 却更费 CPU。压一次 → 稳态 CPU = 0。
func (e *staticEntry) gzipVariant() ([]byte, string) {
	e.gzOnce.Do(func() {
		var buf bytes.Buffer
		// 文本资源的实测压缩比约 2.8x：按 1/2 预分配，基本一次到位不再扩容。
		buf.Grow(len(e.body) / 2)
		zw, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		if err != nil {
			// 级别是编译期常量，构造失败只可能是代码写错了；退化为不压，不外泄错误。
			return
		}
		if _, err := zw.Write(e.body); err != nil {
			return
		}
		if err := zw.Close(); err != nil {
			return
		}
		e.gzBody = buf.Bytes()
		e.gzETag = staticETag(e.body, "-gz")
	})
	return e.gzBody, e.gzETag
}

// staticFileServer 是静态资源处理器：路径语义 + 变体选择 + 缓存分级。
type staticFileServer struct {
	fsys http.FileSystem

	mu      sync.RWMutex
	entries map[string]*staticEntry
}

// staticHandler 返回带 CDN 缓存分级的静态资源处理器。
// root 为本地静态目录（指纹脚本产物 dist/）。
func staticHandler(root string) http.Handler {
	return staticFS(http.Dir(root))
}

// staticFS 用指定的文件系统返回静态处理器（供 embed.FS 或本地目录）。
//
// 处理器自己只做四件事：**解析路径语义、选变体（identity / gzip）、设头
// （Cache-Control / ETag / Content-Type / Vary）、给 body**。
//
// 条件请求（If-None-Match / If-Modified-Since / Range / If-Range / HEAD /
// Content-Length）全部交给 http.ServeContent —— 手写 304 与 Range 是标准库里
// 最容易写错的分支（弱比较、多段 range、If-Range 的两种 validator），不重复一遍。
func staticFS(fsys http.FileSystem) http.Handler {
	return &staticFileServer{
		fsys:    fsys,
		entries: make(map[string]*staticEntry),
	}
}

// ServeHTTP 实现静态资源的分发。
func (h *staticFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
	}

	// 与 http.FileServer 一致：路径里出现 ".." 段直接 400（不是 404 —— 这是
	// 坏请求而不是"没有这个文件"，也避免任何越界探测被当成正常 miss）。
	if containsDotDot(upath) {
		http.Error(w, "invalid URL path", http.StatusBadRequest)
		return
	}

	// .../index.html → .../（保留查询串）。改成 / 之后相对引用与 cookie path
	// 才对得上；这条与旧 http.FileServer 行为一致，不能省。
	if strings.HasSuffix(upath, "/index.html") {
		// 301 也带分级头（旧实现的中间件在 FileServer 之前设 Cache-Control，
		// 重定向同样带着它；不去掉是为了不让重定向链多出一跳"无缓存"的行为变化）。
		w.Header().Set("Cache-Control", staticCacheControl(classify(upath)))
		localRedirect(w, r, "./")
		return
	}

	f, info, name, ok := h.resolve(w, r, path.Clean(upath))
	if !ok {
		return // resolve 已经写完响应（301 / 404）
	}
	defer func() { _ = f.Close() }()

	cacheControl := staticCacheControl(classify(name))
	contentType := staticContentType(name)

	// 超大文件：不缓存、不压、不发 ETag，直接流式交给 ServeContent（契约 §3.7）。
	if info.Size() > staticInlineMax {
		setStaticVariantHeaders(w, cacheControl, contentType)
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	}

	// 命中缓存（(size, modtime) 校验通过）就直接用，不再读整包。
	if e := h.lookup(name, info); e != nil {
		serveStaticEntry(w, r, e)
		return
	}

	body, err := io.ReadAll(io.LimitReader(f, staticInlineMax+1))
	if err != nil {
		http.Error(w, "read static file failed", http.StatusInternalServerError)
		return
	}
	if int64(len(body)) > staticInlineMax {
		// 读的瞬间文件长大了（dev 模式才可能）：退回流式，别把大文件塞进缓存。
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "read static file failed", http.StatusInternalServerError)
			return
		}
		setStaticVariantHeaders(w, cacheControl, contentType)
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	}
	if contentType == "" {
		// 扩展名表与宿主 mime 表都没命中：按内容猜（契约 §3.3 的兜底，不许空）。
		contentType = http.DetectContentType(body)
	}

	e := &staticEntry{
		name:         name,
		size:         info.Size(),
		modtime:      info.ModTime(),
		body:         body,
		etag:         staticETag(body, ""),
		contentType:  contentType,
		cacheControl: cacheControl,
	}
	serveStaticEntry(w, r, h.publish(e, info))
}

// resolve 把清理后的 URL 路径解析成一个可服务的普通文件。
//
// 已自行写完响应（缺尾斜杠 301 / 不存在 404）时 ok=false。
func (h *staticFileServer) resolve(w http.ResponseWriter, r *http.Request, name string) (f http.File, info os.FileInfo, resolved string, ok bool) {
	// 最多两轮：目录 → 目录/index.html。第二轮再撞目录说明产物本身异常，按 404 处理。
	for i := 0; i < 2; i++ {
		file, err := h.fsys.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return nil, nil, "", false
		}
		st, err := file.Stat()
		if err != nil {
			_ = file.Close()
			http.NotFound(w, r)
			return nil, nil, "", false
		}
		if !st.IsDir() {
			return file, st, name, true
		}
		// 目录缺尾斜杠：先 301 补齐，否则 /en 下的相对引用会被解析到 / 上一层。
		if u := r.URL.Path; u == "" || !strings.HasSuffix(u, "/") {
			_ = file.Close()
			w.Header().Set("Cache-Control", staticCacheControl(classify(name)))
			localRedirect(w, r, path.Base(u)+"/")
			return nil, nil, "", false
		}
		_ = file.Close()
		// 目录带尾斜杠 → 服务 index.html；没有 index.html 就 404。
		// **不再 autoindex**（契约 §6）：只暴露具名资源，别把目录结构当接口。
		name = path.Join(name, "index.html")
	}
	http.NotFound(w, r)
	return nil, nil, "", false
}

// lookup 返回仍然有效的缓存条目（无则 nil）。
func (h *staticFileServer) lookup(name string, info os.FileInfo) *staticEntry {
	h.mu.RLock()
	e := h.entries[name]
	h.mu.RUnlock()
	if e != nil && e.validFor(info) {
		return e
	}
	return nil
}

// publish 发布新条目；并发下同版本的先到者胜出（避免各自持有一份 145KB 副本）。
func (h *staticFileServer) publish(e *staticEntry, info os.FileInfo) *staticEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.entries[e.name]; old != nil && old.validFor(info) {
		return old
	}
	h.entries[e.name] = e
	return e
}

// setVariantHeaders 设与具体变体无关的头（分级 + Content-Type）。
// Content-Type 为空时留给 ServeContent 去 sniff。
func setStaticVariantHeaders(w http.ResponseWriter, cacheControl, contentType string) {
	hdr := w.Header()
	hdr.Set("Cache-Control", cacheControl)
	if contentType != "" {
		hdr.Set("Content-Type", contentType)
	}
}

// serveStaticEntry 选变体、设头，然后把 body 交给 http.ServeContent。
func serveStaticEntry(w http.ResponseWriter, r *http.Request, e *staticEntry) {
	hdr := w.Header()
	hdr.Set("Cache-Control", e.cacheControl)

	body, etag := e.body, e.etag

	// 可压类型**不论这次压不压**都要声明 Vary（契约 §3.4 / §3.5）：同一条 URL 的
	// 字节数随 Accept-Encoding 变化，共享缓存不分桶就会把 gzip 字节喂给不支持的客户端。
	if e.compressible() {
		appendVaryAcceptEncoding(hdr)
	}

	// 不双压：头里已经有 Content-Encoding 就说明这一层不许再编码（防未来回归）。
	if e.compressible() && hdr.Get("Content-Encoding") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		// len(gzBody) < len(e.body)：flate 偶尔会把某些"可压类型但内容随机"的字节
		// 压得更大（阈值只看了原文长度）。压完不更小就发原文 —— 绝不为了"用了 gzip"
		// 而多传字节。
		if gzBody, gzETag := e.gzipVariant(); len(gzBody) > 0 && len(gzBody) < len(e.body) {
			body, etag = gzBody, gzETag
			hdr.Set("Content-Encoding", "gzip")
			// Content-Length 必须是**压缩后**长度，绝不能沿用原文字长。
			//
			// 这里为什么要自己设：net/http 的 serveContent 在 Content-Encoding 不为空时
			// **不**设 Content-Length（上游注释：怕破坏"用户自己包 gzip ResponseWriter"
			// 的写法，见 go/src/net/http/fs.go）。不设就等于全部 gzip 响应走 chunked。
			// 有 Range 时留给 ServeContent（它走 len(ranges)>0 分支，会按 range 长度重设，
			// 我们先设反而会错）。
			if r.Header.Get("Range") == "" {
				hdr.Set("Content-Length", strconv.Itoa(len(gzBody)))
			}
		}
	}

	// 304 分支由 ServeContent 的 checkPreconditions 读这个头做比较。
	hdr.Set("Etag", etag)
	hdr.Set("Content-Type", e.contentType)

	// 状态码收尾一律走 conditionalCLWriter（identity 路径也包）：412 在 2xx/304/416
	// 之外是第四条路径，它既不经过 serveError 也不经过 writeNotModified，见类型注释。
	// 常数级的包装开销换「同一状态码的头永远一致」，值。
	//
	// modtime 为零（embed 模式）时 ServeContent 不发 Last-Modified、也不因
	// If-Modified-Since 返回 304（Go 的 checkIfModifiedSince 对零值返回 condNone）。
	// 304 的唯一硬指标是 If-None-Match（契约 §2）。
	http.ServeContent(&conditionalCLWriter{ResponseWriter: w}, r, e.name, e.modtime, bytes.NewReader(body))
}

// conditionalCLWriter 在最终状态码**不是 2xx** 时撤掉预置的 Content-Length。
//
// 为什么必须有它（这是 review 抓到的一个真回归）：net/http 的 412 分支
// （If-Match / If-Unmodified-Since 不成立）是**直接把状态头写下去**的 ——
// 既不经过 serveError（416/5xx 会删 CL/CE/Cache-Control/ETag），也不经过
// writeNotModified（304 会删 CL/CE）。于是我们为 gzip 变体预置的
// Content-Length 会留在一条**没有 body** 的 412 上：
//   - 客户端会等一个永远不来的 body（curl 直接报
//     "end of response with N bytes missing"，HTTP/2 的流也会被判协议错误）；
//   - 412 还带着 immutable，共享缓存可能把它当可缓存响应存下来再喂给普通 GET。
//
// 412 之外，416/5xx 也一并覆盖（幂等，serveError 已删过一次）。
//
// 2xx（200 / 206）不碰：206 的长度由 ServeContent 按 range 自己重设，
// 200 的长度就是我们预置的压缩体长度（这里正是它的唯一用途）。
type conditionalCLWriter struct {
	http.ResponseWriter
}

func (w *conditionalCLWriter) WriteHeader(code int) {
	h := w.ResponseWriter.Header()
	if code < 200 || code >= 300 {
		// 非 2xx 没有（或没有"完整 body"的）body：预置长度与变体编码都不成立。
		// 304 的这两项 ServeContent 自己会删，这里幂等。
		h.Del("Content-Length")
		h.Del("Content-Encoding")
	}
	if code >= 400 {
		// 4xx/5xx 不该被缓存（与 serveError 的既有行为对齐；412 尤其不能）。
		h.Del("Cache-Control")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *conditionalCLWriter) Write(b []byte) (int, error) { return w.ResponseWriter.Write(b) }

// Unwrap 让 http.ResponseController 穿过这层（Go 1.20+ 的透传约定）。
func (w *conditionalCLWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// localRedirect 发相对 301（绝对路径在 StripPrefix/反代下会丢前缀）。
func localRedirect(w http.ResponseWriter, r *http.Request, newPath string) {
	if q := r.URL.RawQuery; q != "" {
		newPath += "?" + q
	}
	w.Header().Set("Location", newPath)
	w.WriteHeader(http.StatusMovedPermanently)
}

// containsDotDot 报告路径里是否含 ".." 段（与 net/http 的同名实现一致）。
func containsDotDot(v string) bool {
	if !strings.Contains(v, "..") {
		return false
	}
	for _, ent := range strings.FieldsFunc(v, isSlashRune) {
		if ent == ".." {
			return true
		}
	}
	return false
}

// isSlashRune 是路径分隔符判定（"\\" 也算：反代/Windows 客户端可能带过来）。
func isSlashRune(r rune) bool { return r == '/' || r == '\\' }

// resolveStaticFS 返回静态资源文件系统（供 /agent-login.sh 等显式路由读取）。
func resolveStaticFS(cfg Config) http.FileSystem {
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			return http.Dir(cfg.StaticDir)
		}
		slog.Warn("static dir not found, falling back to embed",
			"event", "static.fallback",
			"dir", cfg.StaticDir,
		)
	}
	return embeddedStaticMust()
}

// embeddedStaticMust 返回 embed 文件系统，失败时返回空 FS（避免 panic）。
func embeddedStaticMust() http.FileSystem {
	fs, err := embeddedStatic()
	if err != nil {
		slog.Warn("embed static unavailable",
			"event", "static.embed_missing",
			"error", err.Error(),
		)
		return http.Dir("") // 空，Open 必失败 → 404
	}
	return fs
}

// resolveStatic 返回静态资源处理器（挂到 "/"）。
func resolveStatic(cfg Config) http.Handler {
	// StaticDir 非空 → 本地目录（开发）；否则 → embed 内嵌（生产单二进制）。
	// CDN 前缀重写（CDNPrefix）在反向代理/CDN 层做，源站始终服务同一 dist。
	if cfg.StaticDir != "" {
		if _, err := os.Stat(cfg.StaticDir); err == nil {
			return staticHandler(cfg.StaticDir)
		}
		slog.Warn("static dir not found, falling back to embed",
			"event", "static.fallback",
			"dir", cfg.StaticDir,
		)
	}
	efs, err := embeddedStatic()
	if err != nil {
		slog.Error("embed static unavailable, returning 503",
			"event", "static.embed_missing",
			"error", err.Error(),
		)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "static assets unavailable", http.StatusServiceUnavailable)
		})
	}
	slog.Info("using embedded static assets",
		"event", "static.embed_ready",
	)
	return staticFS(efs)
}

// noStore 中间件：动态 API 禁止缓存。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
