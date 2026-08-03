package sitegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture 在临时目录创建最小化的 sitegen 输入结构，
// 供测试用。返回 srcDir 根目录路径。
func writeFixture(t *testing.T) string {
	t.Helper()
	srcDir := t.TempDir()

	// layouts/base.html — 基础布局骨架
	// 用 {{block}} 而非 {{template}}：页面不定义该块时渲染为空（而非报错）
	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `<!doctype html>
<html lang="{{.Lang}}">
<head>
  <meta charset="UTF-8" />
  <title>Anotify · {{block "title" .}}{{end}}</title>
  {{block "fonts-extra" .}}{{end}}
  {{block "style" .}}{{end}}
</head>
<body class="min-h-screen antialiased">
  <div id="page-main">{{block "content" .}}{{end}}</div>
  <script src="partials.js"></script>
  {{block "script" .}}{{end}}
</body>
</html>`)

	// layouts/login.html — 独立登录布局（flex）
	mustWrite(t, filepath.Join(srcDir, "layouts", "login.html"), `<!doctype html>
<html lang="{{.Lang}}">
<head>
  <meta charset="UTF-8" />
  <title>Anotify · {{block "title" .}}{{end}}</title>
</head>
<body class="min-h-screen flex flex-col">
  {{block "content" .}}{{end}}
</body>
</html>`)

	// pages/index.html — 默认布局（base.html）
	mustWrite(t, filepath.Join(srcDir, "pages", "index.html"), `{{define "title"}}{{t "index.title"}}{{end}}
{{define "content"}}
<h1>{{t "index.greeting"}}</h1>
<p>{{t "index.subtitle"}}</p>
{{end}}`)

	// pages/login.html — 指定 login 布局
	mustWrite(t, filepath.Join(srcDir, "pages", "login.html"), `<!-- layout: login -->
{{define "title"}}{{t "login.title"}}{{end}}
{{define "content"}}
<div class="login-card">
  <h2>{{t "login.welcome"}}</h2>
</div>
{{end}}`)

	// pages/docs.html — 带 fonts-extra 块和 style 块
	mustWrite(t, filepath.Join(srcDir, "pages", "docs.html"), `{{define "title"}}{{t "docs.title"}}{{end}}
{{define "fonts-extra"}}
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono&display=swap" rel="stylesheet" />
{{end}}
{{define "style"}}
<style>.mono { font-family: "JetBrains Mono"; }</style>
{{end}}
{{define "content"}}
<div id="docs-content">{{t "docs.heading"}}</div>
{{end}}`)

	// locales/zh-CN.yaml — 中文翻译
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `index:
  title: 总览
  greeting: 你好
  subtitle: 正在加载…
login:
  title: 登录
  welcome: 欢迎回来
docs:
  title: 接入文档
  heading: 快速开始`)

	// locales/en.yaml — 英文翻译
	mustWrite(t, filepath.Join(srcDir, "locales", "en.yaml"), `index:
  title: Overview
  greeting: Hello
  subtitle: Loading…
login:
  title: Sign In
  welcome: Welcome Back
docs:
  title: Documentation
  heading: Quick Start`)

	return srcDir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGenerate_RendersPagesWithI18n(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// zh-CN（默认语言）输出在根目录
	indexPath := filepath.Join(outDir, "index.html")
	zhIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("读取生成的 index.html: %v", err)
	}
	zhStr := string(zhIndex)

	// 验证 i18n 替换：中文标题/greeting/subtitle
	if !strings.Contains(zhStr, "你好") {
		t.Errorf("zh-CN index.html 应含「你好」, 实际:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "正在加载…") {
		t.Errorf("zh-CN index.html 应含「正在加载…」, 实际:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "总览") {
		t.Errorf("zh-CN index.html 应含「总览」(title), 实际:\n%s", zhStr)
	}

	// 验证 layout 骨架
	if !strings.Contains(zhStr, `id="page-main"`) {
		t.Errorf("zh-CN index.html 应含 page-main 骨架, 实际:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, `<html lang="zh-CN">`) {
		t.Errorf("zh-CN index.html 应设 lang=zh-CN, 实际:\n%s", zhStr)
	}

	// en 输出在 /en/ 子目录
	enIndexPath := filepath.Join(outDir, "en", "index.html")
	enIndex, err := os.ReadFile(enIndexPath)
	if err != nil {
		t.Fatalf("读取生成的 en/index.html: %v", err)
	}
	enStr := string(enIndex)

	// 验证英文翻译
	if !strings.Contains(enStr, "Hello") {
		t.Errorf("en index.html 应含「Hello」, 实际:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Loading…") {
		t.Errorf("en index.html 应含「Loading…」, 实际:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Overview") {
		t.Errorf("en index.html 应含「Overview」, 实际:\n%s", enStr)
	}
	if !strings.Contains(enStr, `<html lang="en">`) {
		t.Errorf("en index.html 应设 lang=en, 实际:\n%s", enStr)
	}
}

func TestGenerate_LoginLayout(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	loginPath := filepath.Join(outDir, "login.html")
	content, err := os.ReadFile(loginPath)
	if err != nil {
		t.Fatalf("读取 login.html: %v", err)
	}
	s := string(content)

	// login 应使用独立布局（flex flex-col），不是 base.html（page-main）
	if !strings.Contains(s, "flex flex-col") {
		t.Errorf("login.html 应使用 login 布局（flex flex-col）, 实际:\n%s", s)
	}
	if strings.Contains(s, `id="page-main"`) {
		t.Errorf("login.html 不应含 page-main（用独立布局）, 实际:\n%s", s)
	}
	// 验证 i18n
	if !strings.Contains(s, "欢迎回来") {
		t.Errorf("login.html 应含「欢迎回来」, 实际:\n%s", s)
	}
}

func TestGenerate_DocsPageWithExtraBlocks(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	docsPath := filepath.Join(outDir, "docs.html")
	content, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("读取 docs.html: %v", err)
	}
	s := string(content)

	// docs 应含 fonts-extra 块的 JetBrains Mono 链接
	if !strings.Contains(s, "JetBrains+Mono") {
		t.Errorf("docs.html 应含 fonts-extra 块, 实际:\n%s", s)
	}
	// docs 应含 style 块
	if !strings.Contains(s, ".mono") {
		t.Errorf("docs.html 应含 style 块, 实际:\n%s", s)
	}
	// docs 应含 i18n 文案
	if !strings.Contains(s, "快速开始") {
		t.Errorf("docs.html 应含「快速开始」, 实际:\n%s", s)
	}
}

func TestGenerate_I18nMissingKeyFallback(t *testing.T) {
	srcDir := t.TempDir()

	// 只创建一个页面，使用一个 en 中不存在的 key
	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `{{template "content" .}}`)
	mustWrite(t, filepath.Join(srcDir, "pages", "test.html"), `{{define "content"}}{{t "only.in.zh"}}{{end}}`)

	// zh-CN 有这个 key，en 没有
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `only:
  in:
    zh: 中文内容`)
	mustWrite(t, filepath.Join(srcDir, "locales", "en.yaml"), `other:
  key: hello`)

	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// en 页面应回退到中文
	enPath := filepath.Join(outDir, "en", "test.html")
	enContent, err := os.ReadFile(enPath)
	if err != nil {
		t.Fatalf("读取 en/test.html: %v", err)
	}
	if !strings.Contains(string(enContent), "中文内容") {
		t.Errorf("缺 key 时应回退默认语言, 实际:\n%s", string(enContent))
	}
}

func TestGenerate_I18nMissingKeyInAllLangsReturnsKey(t *testing.T) {
	srcDir := t.TempDir()

	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `{{template "content" .}}`)
	mustWrite(t, filepath.Join(srcDir, "pages", "test.html"), `{{define "content"}}{{t "nonexistent.key"}}{{end}}`)

	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `existing: 中文`)
	mustWrite(t, filepath.Join(srcDir, "locales", "en.yaml"), `existing: english`)

	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	zhPath := filepath.Join(outDir, "test.html")
	zhContent, err := os.ReadFile(zhPath)
	if err != nil {
		t.Fatalf("读取 test.html: %v", err)
	}
	// 所有语言都没有此 key，应返回 key 本身
	if !strings.Contains(string(zhContent), "nonexistent.key") {
		t.Errorf("所有语言缺 key 时应返回 key 本身, 实际:\n%s", string(zhContent))
	}
}

func TestGenerate_OutputPaths(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// zh-CN 在根目录
	for _, page := range []string{"index.html", "login.html", "docs.html"} {
		if _, err := os.Stat(filepath.Join(outDir, page)); os.IsNotExist(err) {
			t.Errorf("zh-CN 页面 %s 应在根目录", page)
		}
	}

	// en 在 /en/ 子目录
	for _, page := range []string{"index.html", "login.html", "docs.html"} {
		if _, err := os.Stat(filepath.Join(outDir, "en", page)); os.IsNotExist(err) {
			t.Errorf("en 页面 %s 应在 /en/ 子目录", page)
		}
	}
}

func TestGenerate_I18nJS(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// 检查 zh-CN i18n.js
	zhJS, err := os.ReadFile(filepath.Join(outDir, "i18n.zh-CN.js"))
	if err != nil {
		t.Fatalf("读取 i18n.zh-CN.js: %v", err)
	}
	zhStr := string(zhJS)
	if !strings.Contains(zhStr, "window.AnotifyI18n") {
		t.Errorf("i18n.zh-CN.js 应含 window.AnotifyI18n, 实际:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "index.title") {
		t.Errorf("i18n.zh-CN.js 应含 key 'index.title', 实际:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "你好") {
		t.Errorf("i18n.zh-CN.js 应含中文文案, 实际:\n%s", zhStr)
	}

	// 检查 en i18n.js
	enJS, err := os.ReadFile(filepath.Join(outDir, "i18n.en.js"))
	if err != nil {
		t.Fatalf("读取 i18n.en.js: %v", err)
	}
	enStr := string(enJS)
	if !strings.Contains(enStr, "window.AnotifyI18n") {
		t.Errorf("i18n.en.js 应含 window.AnotifyI18n, 实际:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Hello") {
		t.Errorf("i18n.en.js 应含英文文案, 实际:\n%s", enStr)
	}
}

func TestLoadTranslations_FlattenNestedKeys(t *testing.T) {
	srcDir := t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "locales", "test.yaml"), `nav:
  home: 首页
  login: 登录
buttons:
  submit: 提交
  cancel: 取消`)

	result, err := LoadTranslations(filepath.Join(srcDir, "locales"), "test")
	if err != nil {
		t.Fatalf("LoadTranslations: %v", err)
	}

	if result["nav.home"] != "首页" {
		t.Errorf("nav.home = %q, want %q", result["nav.home"], "首页")
	}
	if result["nav.login"] != "登录" {
		t.Errorf("nav.login = %q, want %q", result["nav.login"], "登录")
	}
	if result["buttons.submit"] != "提交" {
		t.Errorf("buttons.submit = %q, want %q", result["buttons.submit"], "提交")
	}
	if result["buttons.cancel"] != "取消" {
		t.Errorf("buttons.cancel = %q, want %q", result["buttons.cancel"], "取消")
	}
}

func TestGenerate_TemplateCacheReuse(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN", "en"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// 多语言渲染后，缓存应包含布局+页面组合
	gen.tmplCache.mu.RLock()
	count := len(gen.tmplCache.entries)
	gen.tmplCache.mu.RUnlock()

	// 3 个页面 × 1 个布局（login 用 login 布局，index/docs 用 base 布局）
	// 缓存键 = layout+page，所以应该有 3 个条目
	if count != 3 {
		t.Errorf("模板缓存应有 3 个条目, 实际 %d", count)
	}
}

func TestNew_DefaultLangs(t *testing.T) {
	srcDir := t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)
	outDir := t.TempDir()

	// 不传 langs，应默认 ["zh-CN"]
	gen, err := New(srcDir, outDir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(gen.langs) != 1 || gen.langs[0] != "zh-CN" {
		t.Errorf("默认语言应为 [zh-CN], 实际 %v", gen.langs)
	}
	if gen.defaultLang != "zh-CN" {
		t.Errorf("默认语言应为 zh-CN, 实际 %s", gen.defaultLang)
	}
}

func TestNew_EmptySrcDir(t *testing.T) {
	outDir := t.TempDir()
	_, err := New("", outDir, []string{"zh-CN"})
	if err == nil {
		t.Error("空 srcDir 应返回错误")
	}
}

func TestNew_NoPagesError(t *testing.T) {
	srcDir := t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)
	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `{{template "content" .}}`)
	outDir := t.TempDir()

	gen, err := New(srcDir, outDir, []string{"zh-CN"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = gen.Generate()
	if err == nil {
		t.Error("无页面时应返回错误")
	}
}
