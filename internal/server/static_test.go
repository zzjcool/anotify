package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// ---------------------------------------------------------------------------
// classify / isHex（#233）
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		want cacheClass
	}{
		{"/assets/app.e95f1f68.js", cacheImmutable},
		{"/design/tokens.0350ec40.css", cacheImmutable},
		{"/icon.3f3b283e.png", cacheImmutable},
		{"/fonts/caveat.71b9e49b.woff2", cacheImmutable},
		{"/index.html", cacheEntry},
		{"/login.html", cacheEntry},
		{"/en/playbook.html", cacheEntry},
		{"/sw.js", cacheEntry},
		{"/manifest.json", cacheEntry},
		{"/manifest.webmanifest", cacheEntry}, // #233：显式名单，不再靠兜底
		{"/app.js", cacheEntry},               // 无指纹 → 短缓存
		{"/assets/app.js", cacheEntry},
		{"/agent-login.sh", cacheEntry},
	}
	for _, c := range cases {
		if got := classify(c.name); got != c.want {
			t.Errorf("classify(%q)=%v want %v", c.name, got, c.want)
		}
	}
}

func TestIsHex(t *testing.T) {
	if !isHex("e95f1f68") {
		t.Error("e95f1f68 应是 hex")
	}
	if !isHex("0123456789abcdef") {
		t.Error("0123456789abcdef 应是 hex")
	}
	if isHex("e95f1f6g") {
		t.Error("含 g 不应是 hex")
	}
	// 指纹由 scripts/hash.mjs 生成，恒为小写；大写不该被当成指纹。
	if isHex("E95F1F68") {
		t.Error("大写 hex 不应被认成指纹（hash.mjs 产小写）")
	}
}

// TestClassifyDetailRules 钉住"分级命中了哪条规则"（#233 的核心：
// 允许兜底存在，但入口产物不许靠兜底碰巧正确）。
func TestClassifyDetailRules(t *testing.T) {
	cases := []struct {
		name      string
		wantClass cacheClass
		wantRule  string
	}{
		{"/manifest.webmanifest", cacheEntry, "entry-name"},
		{"/manifest.json", cacheEntry, "entry-name"},
		{"/sw.js", cacheEntry, "entry-name"},
		{"/index.html", cacheEntry, "entry-html"},
		{"/en/playbook.html", cacheEntry, "entry-html"},
		{"/ui.c84903a0.css", cacheImmutable, "hash"},
		{"/assets/app.e95f1f68.js", cacheImmutable, "hash"},
		{"/vendor/dagre.min.54a503e3.js", cacheImmutable, "hash"},
		{"/fonts/caveat.71b9e49b.woff2", cacheImmutable, "hash"},
		// 兜底语义显式钉住：无指纹的非入口产物落 fallback → cacheEntry。
		{"/app.js", cacheEntry, "fallback"},
		{"/mock.js", cacheEntry, "fallback"},
		{"/agent-login.sh", cacheEntry, "fallback"},
		{"/data.json", cacheEntry, "fallback"}, // .json 不自动等于入口（只有 manifest.json 是）
	}
	for _, c := range cases {
		gotClass, gotRule := classifyDetail(c.name)
		if gotClass != c.wantClass || gotRule != c.wantRule {
			t.Errorf("classifyDetail(%q)=(%v,%q) want (%v,%q)",
				c.name, gotClass, gotRule, c.wantClass, c.wantRule)
		}
	}
}

// 仓库内的两条产物树：dist 是 embed 进二进制的指纹产物（gitignore，make fe 生成），
// web 是开发源目录（恒定存在，ANOTIFY_STATIC=./web 时被服务）。
const (
	unitStaticDistDir = "dist"
	unitStaticWebDir  = "../../web"
)

// TestClassifyEntryProductNamesExistInDist 是"缺失/改名守卫"：
// entryProductNames 里的每个名字都必须真的在产物树里存在 ——
// 构建把 manifest.webmanifest 改名、或不再产出 manifest.json，这条立刻红。
func TestClassifyEntryProductNamesExistInDist(t *testing.T) {
	names := make([]string, 0, len(entryProductNames))
	for name := range entryProductNames {
		names = append(names, name)
	}
	sort.Strings(names)

	if _, err := os.Stat(unitStaticDistDir); err != nil {
		t.Skipf("dist 不存在（先 make fe 生成 embed 产物），跳过 dist 侧守卫: %v", err)
	}
	for _, name := range names {
		st, err := os.Stat(filepath.Join(unitStaticDistDir, name))
		if err != nil {
			t.Errorf("entryProductNames 里的 %q 在 dist 里不存在（产物改名/删除？classify 名单要同步）: %v",
				name, err)
			continue
		}
		if st.IsDir() {
			t.Errorf("entryProductNames 里的 %q 在 dist 里是目录，不是产物文件", name)
			continue
		}
		if class, rule := classifyDetail("/" + name); class != cacheEntry || rule != "entry-name" {
			t.Errorf("真实产物 %q 的分级是 (%v,%q)，want (cacheEntry,entry-name)", name, class, rule)
		}
	}
}

// TestClassifyWebEntryProducts 只对 web/ 里名字固定的两个入口产物做断言
// （web/ 是开发源目录，里面大量无指纹源文件落 fallback 是**正确**意图）。
func TestClassifyWebEntryProducts(t *testing.T) {
	for _, name := range []string{"sw.js", "manifest.webmanifest"} {
		st, err := os.Stat(filepath.Join(unitStaticWebDir, name))
		if err != nil {
			t.Fatalf("web/ 里应有固定名产物 %q: %v", name, err)
		}
		if st.IsDir() {
			t.Fatalf("web/%s 是目录，不是产物文件", name)
		}
		if got := classify("/" + name); got != cacheEntry {
			t.Errorf("classify(/%s)=%v want cacheEntry", name, got)
		}
		if _, rule := classifyDetail("/" + name); rule != "entry-name" {
			t.Errorf("classifyDetail(/%s) 的 rule=%q want entry-name", name, rule)
		}
	}
}

// TestClassifyDistTopLevelFixedNames 是"新产物兜底守卫"：
// dist 顶层凡是 .js/.json/.webmanifest 且**不带指纹**的文件，都必须是显式名单命中，
// 不许靠 fallback。将来 hash.mjs 多产一个固定名文件（如 site.webmanifest）而忘了
// 加进 entryProductNames，这条会立刻红。
func TestClassifyDistTopLevelFixedNames(t *testing.T) {
	entries, err := os.ReadDir(unitStaticDistDir)
	if err != nil {
		t.Skipf("dist 不存在（先 make fe 生成 embed 产物），跳过: %v", err)
	}
	checked := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		base := ent.Name()
		switch strings.ToLower(path.Ext(base)) {
		case ".js", ".json", ".webmanifest":
		default:
			continue // vendor/README.md、agent-login.sh 之类允许 fallback
		}
		class, rule := classifyDetail("/" + base)
		if rule == "hash" {
			continue // 指纹资源，按内容哈希分类是正确的
		}
		if rule != "entry-name" {
			t.Errorf("dist 顶层固定名产物 %q 落到了 %q 兜底（class=%v）：请加进 entryProductNames",
				base, rule, class)
			continue
		}
		if class != cacheEntry {
			t.Errorf("dist 顶层产物 %q 的 entry-name 分级应为 cacheEntry，实际 %v", base, class)
		}
		checked++
	}
	if checked == 0 {
		t.Fatalf("dist 顶层没有扫到任何固定名 .js/.json/.webmanifest 产物，产物树异常（guard 形同虚设）")
	}
}

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// unitStaticMapFS 造一个 embed 风格的文件系统：modtime 全为零（等价 embed.FS）。
// 这样"embed 模式不发 Last-Modified"的行为也能被断言。
func unitStaticMapFS(files map[string]string) http.FileSystem {
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return http.FS(m)
}

// unitStaticNew 返回处理器（兼拿到内部状态用的句柄）。
func unitStaticNew(t *testing.T, files map[string]string) (*staticFileServer, http.Handler) {
	t.Helper()
	h := staticFS(unitStaticMapFS(files))
	srv, ok := h.(*staticFileServer)
	if !ok {
		t.Fatalf("staticFS 返回 %T，期望 *staticFileServer", h)
	}
	return srv, h
}

// unitStaticDo 发一个请求，可选附加头（[name, value] 对）。
func unitStaticDo(h http.Handler, method, target string, hdrs ...[2]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, kv := range hdrs {
		r.Header.Set(kv[0], kv[1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// unitStaticJS 造一份 >gzipMinBytes 的 JS：形状照真实产物（重复键值多），
// 保证"压完必须明显更小"是硬结论而不是巧合。
func unitStaticJS(n int) string {
	var b strings.Builder
	b.WriteString("// unit-static fixture\nexport const items = [\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "  { id: %d, name: \"node-%04d\", status: \"online\" },\n", i, i)
	}
	b.WriteString("];\n")
	return b.String()
}

// unitStaticWantETag 独立复算 ETag（sha256 前 16 hex + 可选后缀，带引号），
// 用来钉住契约 §2 的算法，避免实现和测试共用同一份"自证"。
func unitStaticWantETag(body []byte, suffix string) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:8]) + suffix + `"`
}

// unitStaticGz 用指定级别压一份，用来钉住"等级 = DefaultCompression"（契约 §2）。
func unitStaticGz(t *testing.T, body []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("gzip.NewWriterLevel(%d): %v", level, err)
	}
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// unitStaticGunzip 解压响应体（解不开就直接判失败）。
func unitStaticGunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("响应体不是合法 gzip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	return out
}

// unitStaticHasVaryAcceptEncoding 报告响应头是否声明了 Vary: Accept-Encoding。
func unitStaticHasVaryAcceptEncoding(hdr http.Header) bool {
	for _, v := range hdr.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "accept-encoding") {
				return true
			}
		}
	}
	return false
}

// unitStaticETagShapeOK 校验强 ETag 形状："<16 位小写 hex>[-gz]"（带引号）。
func unitStaticETagShapeOK(etag string) bool {
	inner, ok := strings.CutPrefix(etag, `"`)
	if !ok {
		return false
	}
	inner, ok = strings.CutSuffix(inner, `"`)
	if !ok {
		return false
	}
	inner = strings.TrimSuffix(inner, "-gz")
	return len(inner) == 16 && isHex(inner)
}

// ---------------------------------------------------------------------------
// 路径语义（契约 §3.1）
// ---------------------------------------------------------------------------

func TestStaticPathSemantics(t *testing.T) {
	js := unitStaticJS(2048)
	_, h := unitStaticNew(t, map[string]string{
		"index.html":                   "<!doctype html><title>root</title><p>ROOT",
		"en/index.html":                "<!doctype html><title>en</title><p>EN",
		"assets/app.c84903a0.js":       js,
		"noindexonly/readme.txt":       "no index here",
		"nested/deep/play.c84903a0.js": js,
	})

	t.Run("根路径服务 index.html", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/")
		if w.Code != 200 {
			t.Fatalf("GET / = %d want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "ROOT") {
			t.Errorf("GET / 的 body 不是 index.html: %q", w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET / 的 Content-Type=%q", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Errorf("GET / 的 Cache-Control=%q want public, max-age=60（入口类：短缓存 + ETag）", cc)
		}
	})

	t.Run("/index.html 301 到 ./ 且保留 query", func(t *testing.T) {
		for _, tc := range []struct{ target, wantLoc string }{
			{"/index.html", "./"},
			{"/index.html?ref=abc", "./?ref=abc"},
			{"/en/index.html?x=1&y=2", "./?x=1&y=2"},
		} {
			w := unitStaticDo(h, "GET", tc.target)
			if w.Code != http.StatusMovedPermanently {
				t.Errorf("GET %s = %d want 301", tc.target, w.Code)
				continue
			}
			if got := w.Header().Get("Location"); got != tc.wantLoc {
				t.Errorf("GET %s Location=%q want %q", tc.target, got, tc.wantLoc)
			}
			// 301 也带分级头（旧实现在 FileServer 之前设 Cache-Control，重定向同样带着）。
			if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
				t.Errorf("GET %s 的 301 Cache-Control=%q want public, max-age=60", tc.target, cc)
			}
		}
	})

	t.Run("目录缺尾斜杠 301 补齐", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/en?from=x")
		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("GET /en = %d want 301", w.Code)
		}
		if got := w.Header().Get("Location"); got != "en/?from=x" {
			t.Errorf("Location=%q want en/?from=x", got)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Errorf("目录补斜杠的 301 Cache-Control=%q want public, max-age=60", cc)
		}
	})

	t.Run("目录带尾斜杠服务 index.html", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/en/")
		if w.Code != 200 {
			t.Fatalf("GET /en/ = %d want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "EN") {
			t.Errorf("GET /en/ 的 body: %q", w.Body.String())
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Errorf("GET /en/ 的 Cache-Control=%q want public, max-age=60（目录 → index.html）", cc)
		}
	})

	t.Run("目录无 index.html 404（去掉 autoindex）", func(t *testing.T) {
		for _, target := range []string{"/noindexonly/"} {
			w := unitStaticDo(h, "GET", target)
			if w.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d want 404（契约 §6：不再列目录）", target, w.Code)
			}
			if strings.Contains(w.Body.String(), "readme.txt") {
				t.Errorf("GET %s 泄漏了目录内容", target)
			}
		}
	})

	t.Run("不存在 → 404", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/missing.c84903a0.js")
		if w.Code != http.StatusNotFound {
			t.Errorf("GET 缺失文件 = %d want 404", w.Code)
		}
	})

	t.Run("点段 → 400", func(t *testing.T) {
		for _, target := range []string{"/../etc/passwd", "/a/../../b", "/%2e%2e/x"} {
			w := unitStaticDo(h, "GET", target)
			// 说明：httptest 不会替我们规范化路径，"%2e%2e" 已在 URL 解析阶段
			// 被解成 ".."，所以三种写法都应落到 containsDotDot 的 400 分支。
			if w.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d want 400", target, w.Code)
			}
		}
	})

	t.Run("深层指纹资源", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/nested/deep/play.c84903a0.js")
		if w.Code != 200 {
			t.Fatalf("GET 深层资源 = %d want 200", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control=%q want immutable", cc)
		}
	})
}

// ---------------------------------------------------------------------------
// Content-Type（契约 §3.3）
// ---------------------------------------------------------------------------

func TestStaticContentTypeTable(t *testing.T) {
	const sniffable = "weird.c84903a0.zzz"
	files := map[string]string{
		"index.html":                  "<!doctype html><p>x</p>",
		"ui.c84903a0.css":             "body{color:red}",
		"app.c84903a0.js":             "export const a = 1;",
		"mod.c84903a0.mjs":            "export default 1;",
		"manifest.json":               `{"a":"b"}`,
		"manifest.webmanifest":        `{"name":"anotify"}`,
		"assets/icon.d3e0cc82.png":    "\x89PNG\r\n\x1a\n",
		"assets/favicon.b4aa8fe5.svg": `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"assets/favicon.b4aa8fe5.ico": "\x00\x00\x01\x00",
		"fonts/caveat.71b9e49b.woff2": "wOF2",
		"fonts/caveat.71b9e49b.woff":  "wOFF",
		"agent-login.sh":              "#!/bin/sh\necho hi\n",
		"vendor/README.md":            "# vendor\n",
		"app.c84903a0.js.map":         `{"version":3}`,
		sniffable:                     "<!doctype html><p>sniffed</p>",
	}
	_, h := unitStaticNew(t, files)

	want := map[string]string{
		"/":                            "text/html; charset=utf-8", // "/" 解析到 index.html（/index.html 会 301）
		"/ui.c84903a0.css":             "text/css; charset=utf-8",
		"/app.c84903a0.js":             "text/javascript; charset=utf-8",
		"/mod.c84903a0.mjs":            "text/javascript; charset=utf-8",
		"/manifest.json":               "application/json",
		"/manifest.webmanifest":        "application/manifest+json",
		"/assets/icon.d3e0cc82.png":    "image/png",
		"/assets/favicon.b4aa8fe5.svg": "image/svg+xml",
		"/assets/favicon.b4aa8fe5.ico": "image/x-icon",
		"/fonts/caveat.71b9e49b.woff2": "font/woff2",
		"/fonts/caveat.71b9e49b.woff":  "font/woff",
		"/agent-login.sh":              "text/x-sh; charset=utf-8",
		"/vendor/README.md":            "text/markdown; charset=utf-8",
		"/app.c84903a0.js.map":         "application/json",
	}
	for name, ct := range want {
		w := unitStaticDo(h, "GET", name)
		if w.Code != 200 {
			t.Errorf("GET %s = %d want 200", name, w.Code)
			continue
		}
		if got := w.Header().Get("Content-Type"); got != ct {
			t.Errorf("GET %s Content-Type=%q want %q", name, got, ct)
		}
	}

	// .js 必须是 JS MIME：猜成 text/plain 会让 <script type="module"> 直接拒绝执行，
	// 表现是"整页白屏但 HTTP 全 200"。
	w := unitStaticDo(h, "GET", "/app.c84903a0.js")
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("ES module 的 Content-Type 必须是 JS MIME，实际 %q", got)
	}

	// 表与宿主 mime 表都未命中 → 按内容 sniff，绝不空、绝不 panic。
	w = unitStaticDo(h, "GET", "/"+sniffable)
	got := w.Header().Get("Content-Type")
	if got == "" {
		t.Fatal("未命中扩展名表时必须 sniff 出非空 Content-Type")
	}
	if mime.TypeByExtension(".zzz") == "" && !strings.HasPrefix(got, "text/html") {
		t.Errorf("HTML 内容的未知扩展名应 sniff 成 text/html，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// Cache-Control 分级（契约 §3.2）
// ---------------------------------------------------------------------------

func TestStaticCacheControlGrading(t *testing.T) {
	js := unitStaticJS(2048)
	_, h := unitStaticNew(t, map[string]string{
		"assets/app.c84903a0.js": js,
		"app.js":                 "console.log(1)",
		"index.html":             "<!doctype html>",
		"en/index.html":          "<!doctype html>",
		"sw.js":                  js,
		"manifest.json":          `{"a":"b"}`,
		"manifest.webmanifest":   `{"name":"anotify"}`,
		"agent-login.sh":         "#!/bin/sh\n",
	})
	const (
		immutable = "public, max-age=31536000, immutable"
		entry     = "public, max-age=60" // 入口类：短缓存 + ETag（过期后 304 空体）
	)
	cases := []struct{ target, want string }{
		{"/assets/app.c84903a0.js", immutable},
		{"/", entry}, // 入口 HTML（/index.html 会 301 到 ./）
		{"/en/", entry},
		{"/sw.js", entry},
		{"/manifest.json", entry},
		{"/manifest.webmanifest", entry},
		{"/app.js", entry},         // 无指纹 → 入口类（短缓存 + ETag）
		{"/agent-login.sh", entry}, // 兜底 → 入口类
	}
	for _, c := range cases {
		w := unitStaticDo(h, "GET", c.target)
		if w.Code != 200 {
			t.Errorf("GET %s = %d want 200", c.target, w.Code)
			continue
		}
		if got := w.Header().Get("Cache-Control"); got != c.want {
			t.Errorf("GET %s Cache-Control=%q want %q", c.target, got, c.want)
		}
		if etag := w.Header().Get("Etag"); etag == "" {
			t.Errorf("GET %s 缺 ETag（200 响应必须带）", c.target)
		} else if !unitStaticETagShapeOK(etag) {
			t.Errorf("GET %s ETag=%q 形状不对（want \"<16hex>[-gz]\"）", c.target, etag)
		}
	}
}

// ---------------------------------------------------------------------------
// ETag / 304（契约 §2 / §3.5）
// ---------------------------------------------------------------------------

func TestStaticETagAnd304(t *testing.T) {
	js := unitStaticJS(4096)
	_, h := unitStaticNew(t, map[string]string{
		"assets/app.c84903a0.js": js,
		"index.html":             "<!doctype html><title>root</title>",
	})
	body := []byte(js)
	wantETag := unitStaticWantETag(body, "")

	w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js")
	if w.Code != 200 {
		t.Fatalf("GET = %d want 200", w.Code)
	}
	if got := w.Header().Get("Etag"); got != wantETag {
		t.Fatalf("ETag=%q want %q（sha256 前 16 hex 带引号）", got, wantETag)
	}
	if got := w.Body.String(); got != js {
		t.Fatalf("identity body 不一致（len got=%d want=%d）", len(got), len(js))
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length=%q want %d", got, len(body))
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges=%q want bytes（Range 交给 ServeContent）", got)
	}

	t.Run("If-None-Match 命中 → 304 空体", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"If-None-Match", wantETag})
		if w.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match 命中 = %d want 304", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("304 的 body 必须为空，实际 %d 字节", w.Body.Len())
		}
		if got := w.Header().Get("Etag"); got != wantETag {
			t.Errorf("304 必须回显 ETag，实际 %q", got)
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("304 必须保留 Cache-Control，实际 %q", got)
		}
		for _, gone := range []string{"Content-Type", "Content-Length", "Content-Encoding"} {
			if got := w.Header().Get(gone); got != "" {
				t.Errorf("304 不应带 %s（实际 %q）", gone, got)
			}
		}
	})

	t.Run("If-None-Match 不匹配 → 200", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"If-None-Match", `"deadbeefdeadbeef"`})
		if w.Code != 200 {
			t.Errorf("ETag 不匹配 = %d want 200", w.Code)
		}
	})

	t.Run("If-None-Match * → 304", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"If-None-Match", "*"})
		if w.Code != http.StatusNotModified {
			t.Errorf(`If-None-Match: * = %d want 304`, w.Code)
		}
	})

	t.Run("gzip 变体 ETag 不同（RFC 9110 §8.8.3）", func(t *testing.T) {
		gzResp := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
		gzETag := gzResp.Header().Get("Etag")
		if gzETag == wantETag {
			t.Fatalf("gzip 变体不能与 identity 共用 ETag")
		}
		if want := unitStaticWantETag(body, "-gz"); gzETag != want {
			t.Fatalf("gzip ETag=%q want %q", gzETag, want)
		}
		// 拿 identity 的 ETag 去要 gzip 变体：representation 不同，必须 200 全量。
		if w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
			[2]string{"Accept-Encoding", "gzip"}, [2]string{"If-None-Match", wantETag}); w.Code != 200 {
			t.Errorf("identity ETag + AE gzip = %d want 200（不同 representation）", w.Code)
		}
		// 拿 gzip 的 ETag：304 空体。
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
			[2]string{"Accept-Encoding", "gzip"}, [2]string{"If-None-Match", gzETag})
		if w.Code != http.StatusNotModified {
			t.Errorf("gzip ETag 命中 = %d want 304", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("gzip 304 的 body 必须为空，实际 %d 字节", w.Body.Len())
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("304 不应带 Content-Encoding，实际 %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// gzip 协商（契约 §3.4）
// ---------------------------------------------------------------------------

func TestStaticGzipNegotiation(t *testing.T) {
	js := unitStaticJS(8192)
	png := strings.Repeat("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR", 512)
	small := "console.log(1);"
	files := map[string]string{
		"assets/app.c84903a0.js":   js,
		"assets/icon.d3e0cc82.png": png,
		"assets/tiny.c84903a0.js":  small,
	}
	_, h := unitStaticNew(t, files)

	wantGz := unitStaticGz(t, []byte(js), gzip.DefaultCompression)
	identityETag := unitStaticWantETag([]byte(js), "")
	gzETag := unitStaticWantETag([]byte(js), "-gz")

	cases := []struct {
		acceptEnc string
		wantGzip  bool
	}{
		{"", false},
		{"gzip", true},
		{"gzip;q=1", true},
		{"gzip;q=0", false},
		{"*", false}, // 不把 * 当 gzip（契约 §2 起：只认显式 gzip）
		{"br", false},
		{"deflate, gzip", true},
		{"br;q=1, gzip;q=0", false},
	}
	for _, c := range cases {
		t.Run("Accept-Encoding:"+c.acceptEnc, func(t *testing.T) {
			w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", c.acceptEnc})
			if w.Code != 200 {
				t.Fatalf("GET = %d want 200", w.Code)
			}
			ce := w.Header().Get("Content-Encoding")
			if !unitStaticHasVaryAcceptEncoding(w.Header()) {
				t.Error("可压类型必须带 Vary: Accept-Encoding（不论这次压不压）")
			}
			if !c.wantGzip {
				if ce != "" {
					t.Fatalf("Content-Encoding=%q，客户端不接受 gzip 时不该压", ce)
				}
				if got := w.Body.String(); got != js {
					t.Errorf("identity body 不一致（len got=%d want=%d）", len(got), len(js))
				}
				if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(js)) {
					t.Errorf("Content-Length=%q want %d", got, len(js))
				}
				if got := w.Header().Get("Etag"); got != identityETag {
					t.Errorf("identity ETag=%q want %q", got, identityETag)
				}
				return
			}
			if ce != "gzip" {
				t.Fatalf("Content-Encoding=%q want gzip", ce)
			}
			if got := w.Body.Bytes(); !bytes.Equal(got, wantGz) {
				t.Errorf("压缩体不等（等级必须是 gzip.DefaultCompression，契约 §2）：len got=%d want=%d",
					len(got), len(wantGz))
			}
			// Content-Length 必须是压缩后长度，绝不能沿用原文字长。
			if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(wantGz)) {
				t.Errorf("Content-Length=%q want 压缩后长度 %d", got, len(wantGz))
			}
			if got := w.Header().Get("Etag"); got != gzETag {
				t.Errorf("gzip ETag=%q want %q", got, gzETag)
			}
			if got := unitStaticGunzip(t, w.Body.Bytes()); string(got) != js {
				t.Errorf("解压后与原文不一致（len got=%d want=%d）", len(got), len(js))
			}
			if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
				t.Errorf("gzip 响应的 Content-Type 应是原文类型，实际 %q", ct)
			}
		})
	}

	t.Run("不可压类型不压也不挂 Vary", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/icon.d3e0cc82.png", [2]string{"Accept-Encoding", "gzip"})
		if w.Code != 200 {
			t.Fatalf("GET png = %d want 200", w.Code)
		}
		if ce := w.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("image/png 不该压，实际 Content-Encoding=%q", ce)
		}
		if unitStaticHasVaryAcceptEncoding(w.Header()) {
			t.Error("不可压类型不必挂 Vary: Accept-Encoding")
		}
		if got := w.Body.String(); got != png {
			t.Errorf("png body 不一致")
		}
	})

	t.Run("低于 gzipMinBytes 不压", func(t *testing.T) {
		if len(small) >= gzipMinBytes {
			t.Fatalf("fixture 有误：small 必须 < gzipMinBytes(%d)", gzipMinBytes)
		}
		w := unitStaticDo(h, "GET", "/assets/tiny.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
		if ce := w.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("小块不该压（省下的字节不够付 flate 头），实际 Content-Encoding=%q", ce)
		}
		if got := w.Body.String(); got != small {
			t.Errorf("body=%q want %q", got, small)
		}
	})

	t.Run("HEAD 也报压缩后长度", func(t *testing.T) {
		w := unitStaticDo(h, "HEAD", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
		if w.Code != 200 {
			t.Fatalf("HEAD = %d want 200", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("HEAD 不该有 body，实际 %d 字节", w.Body.Len())
		}
		if got := w.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("HEAD 的 Content-Encoding=%q want gzip（头的语义与 GET 一致）", got)
		}
		if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(wantGz)) {
			t.Errorf("HEAD Content-Length=%q want %d", got, len(wantGz))
		}
	})
}

// TestStaticGzipOnceAndConcurrent 证明"每文件一次性压缩"（契约 §2 的稳态 CPU=0），
// 同时用 16 个并发 goroutine 混打不同变体，让 -race 有机会抓到缓存竞态。
func TestStaticGzipOnceAndConcurrent(t *testing.T) {
	js := unitStaticJS(6000)
	files := map[string]string{
		"assets/app.c84903a0.js": js,
		"index.html":             "<!doctype html><title>root</title>",
		"sw.js":                  js,
	}
	srv, h := unitStaticNew(t, files)

	// onceGzipBody 读缓存条目并返回它的 gzip 体（sync.Once 保护，并发安全）。
	onceGzipBody := func(t *testing.T) []byte {
		t.Helper()
		srv.mu.RLock()
		e := srv.entries["/assets/app.c84903a0.js"]
		srv.mu.RUnlock()
		if e == nil {
			t.Fatal("缓存条目缺失：一次 gzip 请求后应有条目")
		}
		gz, _ := e.gzipVariant()
		return gz
	}

	first := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
	second := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
	// 「只压一次」的机械证据：两次拿到的必须是**同一块底层数组**（缓存命中），
	// 不是重新算出来的内容相同的新副本。用 &b[0] 比较，不需要 unsafe。
	gzA, gzB := onceGzipBody(t), onceGzipBody(t)
	if len(gzA) == 0 || &gzA[0] != &gzB[0] {
		t.Fatalf("同一文件的 gzip 变体被重算了（应复用 sync.Once 缓存）")
	}
	if first.Header().Get("Etag") != second.Header().Get("Etag") {
		t.Errorf("两次 gzip 响应的 ETag 必须稳定")
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Errorf("两次 gzip 响应的字节必须一致")
	}

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				switch (i + j) % 4 {
				case 0:
					w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
					if w.Code != 200 || w.Body.Len() != first.Body.Len() {
						errs <- fmt.Sprintf("goroutine %d gzip: code=%d len=%d", i, w.Code, w.Body.Len())
					}
				case 1:
					w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js")
					if w.Code != 200 || w.Body.String() != js {
						errs <- fmt.Sprintf("goroutine %d identity: code=%d", i, w.Code)
					}
				case 2:
					w := unitStaticDo(h, "GET", "/")
					if w.Code != 200 || !strings.Contains(w.Body.String(), "root") {
						errs <- fmt.Sprintf("goroutine %d index: code=%d", i, w.Code)
					}
				case 3:
					etag := first.Header().Get("Etag")
					w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
						[2]string{"Accept-Encoding", "gzip"}, [2]string{"If-None-Match", etag})
					if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
						errs <- fmt.Sprintf("goroutine %d 304: code=%d len=%d", i, w.Code, w.Body.Len())
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// 并发下仍然复用同一块 gzip 体。
	if gzC, gzD := onceGzipBody(t), onceGzipBody(t); &gzC[0] != &gzD[0] {
		t.Error("并发后 gzip 变体被重算了（sync.Once 没生效）")
	}
}

// ---------------------------------------------------------------------------
// Last-Modified 语义（embed 无 modtime / 本地目录有）
// ---------------------------------------------------------------------------

func TestStaticLastModifiedSemantics(t *testing.T) {
	const dir = "assets/app.c84903a0.js"
	const imsFuture = "Sat, 01 Jan 2095 00:00:00 GMT"

	t.Run("embed 模式不发 Last-Modified，也不因 If-Modified-Since 返回 304", func(t *testing.T) {
		_, h := unitStaticNew(t, map[string]string{dir: unitStaticJS(2048)})
		w := unitStaticDo(h, "GET", "/"+dir)
		if got := w.Header().Get("Last-Modified"); got != "" {
			t.Errorf("embed 没有真实 modtime，不该发 Last-Modified，实际 %q", got)
		}
		if w.Header().Get("Etag") == "" {
			t.Error("embed 模式必须靠 ETag 做条件请求")
		}
		w = unitStaticDo(h, "GET", "/"+dir, [2]string{"If-Modified-Since", imsFuture})
		if w.Code != 200 {
			t.Errorf("embed 模式 If-Modified-Since 不该 304（契约 §2：304 的硬指标是 If-None-Match），实际 %d", w.Code)
		}
	})

	t.Run("本地目录模式发 Last-Modified 且 If-Modified-Since 可 304", func(t *testing.T) {
		root := t.TempDir()
		name := "app.c84903a0.js"
		if err := os.WriteFile(filepath.Join(root, name), []byte(unitStaticJS(2048)), 0o644); err != nil {
			t.Fatal(err)
		}
		h := staticHandler(root)
		w := unitStaticDo(h, "GET", "/"+name)
		if w.Code != 200 {
			t.Fatalf("GET = %d want 200", w.Code)
		}
		lm := w.Header().Get("Last-Modified")
		if lm == "" {
			t.Fatal("本地目录模式应发 Last-Modified")
		}
		if w.Header().Get("Etag") == "" {
			t.Fatal("本地目录模式也必须发 ETag")
		}
		w = unitStaticDo(h, "GET", "/"+name, [2]string{"If-Modified-Since", lm})
		if w.Code != http.StatusNotModified {
			t.Errorf("If-Modified-Since 命中 = %d want 304", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("304 的 body 必须为空，实际 %d 字节", w.Body.Len())
		}
	})
}

// TestStaticLocalDirCacheInvalidation 验证缓存按 (size, modtime) 失效（契约 §3.8）：
// dev 模式改完前端立刻生效，不用重启进程。
func TestStaticLocalDirCacheInvalidation(t *testing.T) {
	root := t.TempDir()
	name := "app.c84903a0.js"
	full := filepath.Join(root, name)

	contentA := unitStaticJS(2048)
	if err := os.WriteFile(full, []byte(contentA), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	if err := os.Chtimes(full, base, base); err != nil {
		t.Fatal(err)
	}
	h := staticHandler(root)

	w := unitStaticDo(h, "GET", "/"+name)
	if w.Code != 200 || w.Body.String() != contentA {
		t.Fatalf("首次 GET 失败: code=%d len=%d", w.Code, w.Body.Len())
	}
	etagA := w.Header().Get("Etag")

	// 1) 长度变化 → (size, modtime) 双变，必须失效。
	contentB := unitStaticJS(4096)
	if err := os.WriteFile(full, []byte(contentB), 0o644); err != nil {
		t.Fatal(err)
	}
	modB := base.Add(2 * time.Second)
	if err := os.Chtimes(full, modB, modB); err != nil {
		t.Fatal(err)
	}
	w = unitStaticDo(h, "GET", "/"+name)
	if w.Body.String() != contentB {
		t.Fatalf("长度变化后仍在服务旧内容（len got=%d want=%d）", w.Body.Len(), len(contentB))
	}
	etagB := w.Header().Get("Etag")
	if etagB == etagA {
		t.Errorf("内容变了 ETag 必须变（got %q）", etagB)
	}

	// 2) **同长度**内容变化但 modtime 前进 → 也必须失效（证明 modtime 参与校验）。
	contentC := strings.Replace(contentB, "node-0000", "NODE-0000", 1)
	if len(contentC) != len(contentB) {
		t.Fatalf("fixture 有误：同长度替换失败 %d vs %d", len(contentC), len(contentB))
	}
	if err := os.WriteFile(full, []byte(contentC), 0o644); err != nil {
		t.Fatal(err)
	}
	modC := base.Add(4 * time.Second)
	if err := os.Chtimes(full, modC, modC); err != nil {
		t.Fatal(err)
	}
	w = unitStaticDo(h, "GET", "/"+name)
	if w.Body.String() != contentC {
		t.Fatalf("同长度改写但 modtime 前进时应失效（(size,modtime) 校验）")
	}
	if w.Header().Get("Etag") == etagB {
		t.Errorf("同长度改写后 ETag 必须变")
	}

	// 3) 已知行为：size 与 modtime 都没变（同秒内、同长度改写）时缓存不失效。
	//    这是契约 §3.8 明确接受的代价（每次请求不做内容哈希）；改成按内容校验请同步改这条断言。
	contentD := strings.Replace(contentC, "node-0001", "NODE-0001", 1)
	if err := os.WriteFile(full, []byte(contentD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, modC, modC); err != nil {
		t.Fatal(err)
	}
	w = unitStaticDo(h, "GET", "/"+name)
	if w.Body.String() != contentC {
		t.Fatalf("(size,modtime) 未变时按契约应命中缓存，实际已刷新 —— 缓存策略变了请改这条断言")
	}
}

// ---------------------------------------------------------------------------
// 内存上界：超过 staticInlineMax 走流式（契约 §3.7）
// ---------------------------------------------------------------------------

func TestStaticInlineMaxStreamsLargeFiles(t *testing.T) {
	old := staticInlineMax
	staticInlineMax = 1024
	t.Cleanup(func() { staticInlineMax = old })

	big := unitStaticJS(8192)
	if int64(len(big)) <= staticInlineMax {
		t.Fatalf("fixture 有误：%d 字节没有超过上限 %d", len(big), staticInlineMax)
	}
	_, h := unitStaticNew(t, map[string]string{"assets/big.c84903a0.js": big})

	for i := 0; i < 2; i++ { // 两次都不该被缓存/压缩
		w := unitStaticDo(h, "GET", "/assets/big.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
		if w.Code != 200 {
			t.Fatalf("GET 大文件 = %d want 200", w.Code)
		}
		if got := w.Body.String(); got != big {
			t.Fatalf("大文件 body 不一致（len got=%d want=%d）", len(got), len(big))
		}
		if ce := w.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("流式路径不压（不发 ETag 就没法区分变体），实际 Content-Encoding=%q", ce)
		}
		if etag := w.Header().Get("Etag"); etag != "" {
			t.Errorf("流式路径不发 ETag（不缓存整包），实际 %q", etag)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("流式路径的 Cache-Control 照常分级，实际 %q", cc)
		}
		if ct := w.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
			t.Errorf("流式路径的 Content-Type=%q", ct)
		}
	}
}

// ---------------------------------------------------------------------------
// HEAD / Range / If-Range（交给 ServeContent，这里只验观测面）
// ---------------------------------------------------------------------------

func TestStaticHeadAndRange(t *testing.T) {
	js := unitStaticJS(4096)
	_, h := unitStaticNew(t, map[string]string{
		"assets/app.c84903a0.js": js,
		"index.html":             "<!doctype html>",
	})
	etag := unitStaticWantETag([]byte(js), "")

	t.Run("HEAD 有头无体", func(t *testing.T) {
		w := unitStaticDo(h, "HEAD", "/assets/app.c84903a0.js")
		if w.Code != 200 {
			t.Fatalf("HEAD = %d want 200", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("HEAD 不该有 body，实际 %d 字节", w.Body.Len())
		}
		if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(js)) {
			t.Errorf("HEAD Content-Length=%q want %d", got, len(js))
		}
		if got := w.Header().Get("Etag"); got != etag {
			t.Errorf("HEAD ETag=%q want %q", got, etag)
		}
	})

	t.Run("HEAD + If-None-Match → 304", func(t *testing.T) {
		w := unitStaticDo(h, "HEAD", "/")
		et := w.Header().Get("Etag")
		if et == "" {
			t.Fatal("HEAD / 缺 ETag")
		}
		w = unitStaticDo(h, "HEAD", "/", [2]string{"If-None-Match", et})
		if w.Code != http.StatusNotModified {
			t.Errorf("HEAD + 命中 ETag = %d want 304", w.Code)
		}
	})

	t.Run("单段 Range → 206", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Range", "bytes=0-9"})
		if w.Code != http.StatusPartialContent {
			t.Fatalf("Range = %d want 206", w.Code)
		}
		if got := w.Body.String(); got != js[:10] {
			t.Errorf("Range body=%q want %q", got, js[:10])
		}
		if got := w.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-9/%d", len(js)) {
			t.Errorf("Content-Range=%q", got)
		}
		if got := w.Header().Get("Content-Length"); got != "10" {
			t.Errorf("Content-Length=%q want 10", got)
		}
	})

	t.Run("If-Range 匹配 → 206，不匹配 → 200 全量", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
			[2]string{"Range", "bytes=0-9"}, [2]string{"If-Range", etag})
		if w.Code != http.StatusPartialContent {
			t.Errorf("If-Range 命中 = %d want 206", w.Code)
		}
		w = unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
			[2]string{"Range", "bytes=0-9"}, [2]string{"If-Range", `"0123456789abcdef"`})
		if w.Code != 200 {
			t.Errorf("If-Range 失效 = %d want 200（全量重传）", w.Code)
		}
		if got := w.Body.String(); got != js {
			t.Errorf("If-Range 失效时应回全量 body")
		}
	})

	t.Run("gzip 变体上的 Range 仍然可用", func(t *testing.T) {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js",
			[2]string{"Accept-Encoding", "gzip"}, [2]string{"Range", "bytes=0-9"})
		if w.Code != http.StatusPartialContent {
			t.Fatalf("gzip + Range = %d want 206", w.Code)
		}
		if len(w.Body.Bytes()) != 10 {
			t.Errorf("Range 长度=%d want 10", w.Body.Len())
		}
		if got := w.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding=%q want gzip（Range 作用在选中的 representation 上）", got)
		}
	})
}

// ---------------------------------------------------------------------------
// 预置 Content-Length 只对 2xx 成立（review M1 的回归护栏）
// ---------------------------------------------------------------------------

func TestStaticPreconditionFailedKeepsNoBodyLength(t *testing.T) {
	js := unitStaticJS(8192)
	_, h := unitStaticNew(t, map[string]string{
		"assets/app.c84903a0.js": js,
		"index.html":             "<!doctype html>",
	})
	wantGzLen := len(unitStaticGz(t, []byte(js), gzip.DefaultCompression))

	// 前提：gzip 变体上我们**确实**预置了压缩体长度（否则这条用例是空转）。
	ok := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
	if got := ok.Header().Get("Content-Length"); got != fmt.Sprint(wantGzLen) {
		t.Fatalf("前提不成立：gzip 200 的 Content-Length=%q want %d", got, wantGzLen)
	}

	cases := []struct {
		name string
		meth string
		hdrs [][2]string
		why  string
	}{
		{"gzip + If-Match 不匹配", "GET", [][2]string{{"Accept-Encoding", "gzip"}, {"If-Match", `"deadbeefdeadbeef"`}}, "412 是 If-Match 失败"},
		{"identity + If-Match 不匹配", "GET", [][2]string{{"If-Match", `"deadbeefdeadbeef"`}}, "412 是 If-Match 失败"},
		// 非 GET/HEAD 上 If-None-Match 命中 = 412（RFC 9110 §13.1.2）。注意必须拿
		// **同一 representation** 的 ETag：identity 用 identity 的，gzip 用带 -gz 的
		// （拿错 ETag 会变成 200 —— 这正是 ETag 按 representation 分叉的证明）。
		{"identity + PUT 命中 If-None-Match", "PUT", [][2]string{{"If-None-Match", unitStaticWantETag([]byte(js), "")}}, "非 GET/HEAD 的条件请求也走 checkPreconditions"},
		{"gzip + PUT 命中 If-None-Match", "PUT", [][2]string{{"Accept-Encoding", "gzip"}, {"If-None-Match", unitStaticWantETag([]byte(js), "-gz")}}, "gzip representation 上同样要 412"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := unitStaticDo(h, c.meth, "/assets/app.c84903a0.js", c.hdrs...)
			if w.Code != http.StatusPreconditionFailed {
				t.Fatalf("%s：code=%d want 412", c.why, w.Code)
			}
			if w.Body.Len() != 0 {
				t.Errorf("412 不该有 body，实际 %d 字节", w.Body.Len())
			}
			// 关键：绝不能留一个「声明了 N 字节却不发」的 Content-Length
			// （curl 会报 "end of response with N bytes missing"，共享缓存还可能存下这条 412）。
			if got := w.Header().Get("Content-Length"); got != "" && got != "0" {
				t.Errorf("412 上残留 Content-Length=%q（客户端会等不来的 body）", got)
			}
			if got := w.Header().Get("Cache-Control"); got != "" {
				t.Errorf("412 不该被缓存，实际 Cache-Control=%q", got)
			}
			if got := w.Header().Get("Content-Encoding"); got != "" {
				t.Errorf("412 没有 body，不该带 Content-Encoding，实际 %q", got)
			}
		})
	}
}

// TestStaticIfUnmodifiedSinceDirMode 钉住 If-Unmodified-Since 的 412 分支（只在有真实
// modtime 的本地目录模式成立：embed 的 modtime 是零值，标准库按 condNone 处理 → 200）。
func TestStaticIfUnmodifiedSinceDirMode(t *testing.T) {
	root := t.TempDir()
	name := "app.c84903a0.js"
	if err := os.WriteFile(filepath.Join(root, name), []byte(unitStaticJS(4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	// 把 mtime 钉在 2020 年，然后让客户端声称「我只接受 2000 年以后没改过的版本」。
	mt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, name), mt, mt); err != nil {
		t.Fatal(err)
	}
	h := staticHandler(root)

	for _, ae := range []string{"", "gzip"} {
		w := unitStaticDo(h, "GET", "/"+name,
			[2]string{"If-Unmodified-Since", "Sat, 01 Jan 2000 00:00:00 GMT"},
			[2]string{"Accept-Encoding", ae})
		if w.Code != http.StatusPreconditionFailed {
			t.Fatalf("AE=%q: If-Unmodified-Since 早于 mtime = %d want 412", ae, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("AE=%q: 412 不该有 body，实际 %d 字节", ae, w.Body.Len())
		}
		if got := w.Header().Get("Content-Length"); got != "" && got != "0" {
			t.Errorf("AE=%q: 412 上残留 Content-Length=%q", ae, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "" {
			t.Errorf("AE=%q: 412 不该带 Cache-Control，实际 %q", ae, got)
		}
	}
}

// TestStaticUncompressibleContentStaysIdentity 钉住「压完不更小就不压」：
// 阈值只看原文长度，而 flate 对高熵内容可能把 4KB 压成 4.1KB。
func TestStaticUncompressibleContentStaysIdentity(t *testing.T) {
	// 确定性高熵字节：连续 sha256 拼接（不可压，但类型是 .js）。
	var b strings.Builder
	seed := []byte("anotify-unit-static")
	for b.Len() < 4096 {
		sum := sha256.Sum256(seed)
		b.Write(sum[:])
		seed = sum[:]
	}
	body := b.String()
	gz := unitStaticGz(t, []byte(body), gzip.DefaultCompression)
	if len(gz) < len(body) {
		t.Fatalf("fixture 有误：这份内容居然压小了（%d → %d）", len(body), len(gz))
	}
	_, h := unitStaticNew(t, map[string]string{"assets/entropy.c84903a0.js": body})

	w := unitStaticDo(h, "GET", "/assets/entropy.c84903a0.js", [2]string{"Accept-Encoding", "gzip"})
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("压完更大就不该压，实际 Content-Encoding=%q（CL=%s）", got, w.Header().Get("Content-Length"))
	}
	if got := w.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Errorf("Content-Length=%q want 原文长度 %d", got, len(body))
	}
	if got := w.Body.String(); got != body {
		t.Errorf("body 必须逐字节等于原文")
	}
}

// TestStaticEmptyFile 钉住 0 字节产物：200 + CL 0 + 有 ETag，不许 panic/500。
func TestStaticEmptyFile(t *testing.T) {
	_, h := unitStaticNew(t, map[string]string{"empty.c84903a0.js": ""})
	w := unitStaticDo(h, "GET", "/empty.c84903a0.js")
	if w.Code != 200 {
		t.Fatalf("空文件 = %d want 200", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != "0" {
		t.Errorf("Content-Length=%q want 0", got)
	}
	if etag := w.Header().Get("Etag"); etag != unitStaticWantETag(nil, "") {
		t.Errorf("空文件的 ETag=%q want %q", etag, unitStaticWantETag(nil, ""))
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control=%q", cc)
	}
}

// TestStaticIfNoneMatchListAndWeak 钉住弱校验器与多值 If-None-Match（RFC 9110 §13.1.2）：
// 浏览器回传的往往是 W/"..."，不认它等于 304 全部失效。
func TestStaticIfNoneMatchListAndWeak(t *testing.T) {
	js := unitStaticJS(4096)
	_, h := unitStaticNew(t, map[string]string{"assets/app.c84903a0.js": js})
	etag := unitStaticWantETag([]byte(js), "")

	for _, inm := range []string{
		"W/" + etag,
		`"bogus", ` + etag,
		`"bogus" , W/` + etag + ` , "another"`,
		"*",
	} {
		w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"If-None-Match", inm})
		if w.Code != http.StatusNotModified {
			t.Errorf("If-None-Match: %s = %d want 304", inm, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("If-None-Match: %s 的 304 body 必须为空，实际 %d 字节", inm, w.Body.Len())
		}
	}
}

// TestStaticRangeNotSatisfiable 钉住 416：文案 + Content-Range，且不带预置长度。
func TestStaticRangeNotSatisfiable(t *testing.T) {
	js := unitStaticJS(4096)
	_, h := unitStaticNew(t, map[string]string{"assets/app.c84903a0.js": js})
	w := unitStaticDo(h, "GET", "/assets/app.c84903a0.js", [2]string{"Range", "bytes=999999-"})
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range = %d want 416", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != fmt.Sprintf("bytes */%d", len(js)) {
		t.Errorf("Content-Range=%q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" && got != "0" {
		// serveError 会删掉 Content-Length，然后由 net/http 按实际 error 文本补。
		// recorder 里这里是空；真连接上会是 0 或 error 文本长度，不会是文件长度。
		if got == fmt.Sprint(len(js)) {
			t.Errorf("416 上残留文件长度 Content-Length=%q（应被删掉）", got)
		}
	}
}

// TestStaticRealDistHTTP 是产物级 HTTP 护栏：对着 make fe 生成的真实 dist 逐文件发请求，
// 断言「每个产物都有 ETag / 分级正确 / 条件请求 304 空体 / 可压类型的 gzip 能解回原文」。
//
// 这条补的是「单测用的 MapFS fixture 与真实产物脱节」的风险：dist 里新增一种扩展名
// 或某个产物变大过阈值时，这里会先红。dist 是 gitignore 产物，不存在时跳过（make ci
// 里 fe 在 test 之前，所以 CI 恒有效）。
func TestStaticRealDistHTTP(t *testing.T) {
	if _, err := os.Stat(unitStaticDistDir); err != nil {
		t.Skipf("dist 不存在（先 make fe 生成 embed 产物），跳过产物级 HTTP 护栏: %v", err)
	}
	h := staticFS(http.Dir(unitStaticDistDir))

	var files []string
	if err := filepath.WalkDir(unitStaticDistDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(unitStaticDistDir, p)
		if rerr != nil {
			return rerr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	}); err != nil {
		t.Fatalf("遍历 dist 失败: %v", err)
	}
	if len(files) < 50 {
		t.Fatalf("dist 只有 %d 个文件，产物树异常（护栏形同虚设）", len(files))
	}

	checked, gzChecked := 0, 0
	for _, rel := range files {
		urlPath := "/" + rel
		if path.Base(rel) == "index.html" {
			// /index.html 会 301 到 ./（与旧 FileServer 一致），改请求目录形态。
			urlPath = "/" + path.Dir(rel) + "/"
			urlPath = strings.Replace(urlPath, "/./", "/", 1)
			urlPath = strings.Replace(urlPath, "//", "/", 1)
		}
		name := rel
		if !strings.HasSuffix(urlPath, ".html") && !strings.HasSuffix(urlPath, "/") {
			name = rel
		} else if strings.HasSuffix(urlPath, "/") {
			name = path.Join(path.Dir(rel), "index.html")
			if path.Dir(rel) == "." {
				name = "index.html"
			}
		}

		w := unitStaticDo(h, "GET", urlPath)
		if w.Code != 200 {
			t.Errorf("GET %s = %d want 200", urlPath, w.Code)
			continue
		}
		etag := w.Header().Get("Etag")
		if etag == "" || !unitStaticETagShapeOK(etag) {
			t.Errorf("GET %s 的 ETag=%q 缺失或形状不对", urlPath, etag)
		}
		if got, want := w.Header().Get("Cache-Control"), staticCacheControl(classify(name)); got != want {
			t.Errorf("GET %s 的 Cache-Control=%q want %q", urlPath, got, want)
		}
		if ct := w.Header().Get("Content-Type"); ct == "" {
			t.Errorf("GET %s 缺 Content-Type", urlPath)
		}
		if w304 := unitStaticDo(h, "GET", urlPath, [2]string{"If-None-Match", etag}); w304.Code != http.StatusNotModified || w304.Body.Len() != 0 {
			t.Errorf("GET %s 的 If-None-Match = %d/%dB want 304/0B", urlPath, w304.Code, w304.Body.Len())
		}

		body := w.Body.Bytes()
		ct := w.Header().Get("Content-Type")
		compressible := len(body) >= gzipMinBytes && (isCompressibleContentType(ct) || ct == "application/manifest+json")
		wgz := unitStaticDo(h, "GET", urlPath, [2]string{"Accept-Encoding", "gzip"})
		if !compressible {
			if ce := wgz.Header().Get("Content-Encoding"); ce != "" {
				t.Errorf("GET %s（不可压：%s）不该压，实际 Content-Encoding=%q", urlPath, ct, ce)
			}
			continue
		}
		wantGz := unitStaticGz(t, body, gzip.DefaultCompression)
		if len(wantGz) >= len(body) {
			// 高熵内容：压完更大，必须保持 identity。
			if ce := wgz.Header().Get("Content-Encoding"); ce != "" {
				t.Errorf("GET %s 压完不更小却仍压（CE=%q）", urlPath, ce)
			}
			continue
		}
		gzChecked++
		if ce := wgz.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Errorf("GET %s 可压（%d→%d）却没压，CE=%q", urlPath, len(body), len(wantGz), ce)
			continue
		}
		if got := wgz.Header().Get("Content-Length"); got != fmt.Sprint(len(wantGz)) {
			t.Errorf("GET %s 的 gzip Content-Length=%q want %d", urlPath, got, len(wantGz))
		}
		if got := unitStaticGunzip(t, wgz.Body.Bytes()); !bytes.Equal(got, body) {
			t.Errorf("GET %s 解压后与 identity 字节不一致", urlPath)
		}
		checked++
	}
	if checked == 0 || gzChecked == 0 {
		t.Fatalf("产物级护栏没有扫到有效样本（200 校验 %d / gzip 校验 %d）", checked, gzChecked)
	}
	t.Logf("产物级 HTTP 护栏：%d 个产物全检，其中 %d 个可压产物验了 gzip 往返", len(files), gzChecked)
}
