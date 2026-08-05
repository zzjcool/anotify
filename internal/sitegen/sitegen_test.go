package sitegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture creates a minimal sitegen input structure in a temp directory
// for testing. Returns the srcDir root path.
func writeFixture(t *testing.T) string {
	t.Helper()
	srcDir := t.TempDir()

	// layouts/base.html — base layout skeleton.
	// Uses {{block}} (not {{template}}): if a page doesn't define the block it
	// renders empty (rather than erroring). Also includes Page/Langs for tests.
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

	// layouts/login.html — standalone login layout (flex).
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

	// pages/index.html — default layout (base.html).
	mustWrite(t, filepath.Join(srcDir, "pages", "index.html"), `{{define "title"}}{{t "index.title"}}{{end}}
{{define "content"}}
<h1>{{t "index.greeting"}}</h1>
<p>{{t "index.subtitle"}}</p>
{{end}}`)

	// pages/login.html — specifies the login layout.
	mustWrite(t, filepath.Join(srcDir, "pages", "login.html"), `<!-- layout: login -->
{{define "title"}}{{t "login.title"}}{{end}}
{{define "content"}}
<div class="login-card">
  <h2>{{t "login.welcome"}}</h2>
</div>
{{end}}`)

	// pages/docs.html — with fonts-extra and style blocks.
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

	// locales/zh-CN.yaml — Chinese translations (default language).
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

	// locales/en.yaml — English translations.
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

	// locales/ja.yaml — Japanese translations.
	mustWrite(t, filepath.Join(srcDir, "locales", "ja.yaml"), `index:
  title: 概要
  greeting: こんにちは
  subtitle: 読み込み中…
login:
  title: ログイン
  welcome: おかえりなさい
docs:
  title: ドキュメント
  heading: クイックスタート`)

	// locales/es.yaml — Spanish translations.
	mustWrite(t, filepath.Join(srcDir, "locales", "es.yaml"), `index:
  title: Resumen
  greeting: Hola
  subtitle: Cargando…
login:
  title: Iniciar sesión
  welcome: Bienvenido de nuevo
docs:
  title: Documentación
  heading: Inicio rápido`)

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

	// zh-CN (default language) is output at the root.
	indexPath := filepath.Join(outDir, "index.html")
	zhIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read generated index.html: %v", err)
	}
	zhStr := string(zhIndex)

	// Verify i18n replacement: Chinese title/greeting/subtitle.
	if !strings.Contains(zhStr, "你好") {
		t.Errorf("zh-CN index.html should contain 你好, got:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "正在加载…") {
		t.Errorf("zh-CN index.html should contain 正在加载…, got:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "总览") {
		t.Errorf("zh-CN index.html should contain 总览 (title), got:\n%s", zhStr)
	}

	// Verify layout skeleton.
	if !strings.Contains(zhStr, `id="page-main"`) {
		t.Errorf("zh-CN index.html should contain page-main skeleton, got:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, `<html lang="zh-CN">`) {
		t.Errorf("zh-CN index.html should set lang=zh-CN, got:\n%s", zhStr)
	}

	// en is output under /en/.
	enIndexPath := filepath.Join(outDir, "en", "index.html")
	enIndex, err := os.ReadFile(enIndexPath)
	if err != nil {
		t.Fatalf("read generated en/index.html: %v", err)
	}
	enStr := string(enIndex)

	// Verify English translations.
	if !strings.Contains(enStr, "Hello") {
		t.Errorf("en index.html should contain Hello, got:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Loading…") {
		t.Errorf("en index.html should contain Loading…, got:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Overview") {
		t.Errorf("en index.html should contain Overview, got:\n%s", enStr)
	}
	if !strings.Contains(enStr, `<html lang="en">`) {
		t.Errorf("en index.html should set lang=en, got:\n%s", enStr)
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
		t.Fatalf("read login.html: %v", err)
	}
	s := string(content)

	// login should use the standalone layout (flex flex-col), not base.html (page-main).
	if !strings.Contains(s, "flex flex-col") {
		t.Errorf("login.html should use login layout (flex flex-col), got:\n%s", s)
	}
	if strings.Contains(s, `id="page-main"`) {
		t.Errorf("login.html should not contain page-main (uses standalone layout), got:\n%s", s)
	}
	// Verify i18n.
	if !strings.Contains(s, "欢迎回来") {
		t.Errorf("login.html should contain 欢迎回来, got:\n%s", s)
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
		t.Fatalf("read docs.html: %v", err)
	}
	s := string(content)

	// docs should contain the fonts-extra block's JetBrains Mono link.
	if !strings.Contains(s, "JetBrains+Mono") {
		t.Errorf("docs.html should contain fonts-extra block, got:\n%s", s)
	}
	// docs should contain the style block.
	if !strings.Contains(s, ".mono") {
		t.Errorf("docs.html should contain style block, got:\n%s", s)
	}
	// docs should contain the i18n string.
	if !strings.Contains(s, "快速开始") {
		t.Errorf("docs.html should contain 快速开始, got:\n%s", s)
	}
}

func TestGenerate_I18nMissingKeyFallback(t *testing.T) {
	srcDir := t.TempDir()

	// Create a single page using a key that exists in zh-CN but not en.
	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `{{template "content" .}}`)
	mustWrite(t, filepath.Join(srcDir, "pages", "test.html"), `{{define "content"}}{{t "only.in.zh"}}{{end}}`)

	// zh-CN has this key, en does not.
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

	// The en page should fall back to the Chinese (default language) string.
	enPath := filepath.Join(outDir, "en", "test.html")
	enContent, err := os.ReadFile(enPath)
	if err != nil {
		t.Fatalf("read en/test.html: %v", err)
	}
	if !strings.Contains(string(enContent), "中文内容") {
		t.Errorf("missing key should fall back to default language, got:\n%s", string(enContent))
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
		t.Fatalf("read test.html: %v", err)
	}
	// Key missing in all languages: should return the raw key itself.
	if !strings.Contains(string(zhContent), "nonexistent.key") {
		t.Errorf("missing key in all languages should return the raw key, got:\n%s", string(zhContent))
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

	// zh-CN at the root.
	for _, page := range []string{"index.html", "login.html", "docs.html"} {
		if _, err := os.Stat(filepath.Join(outDir, page)); os.IsNotExist(err) {
			t.Errorf("zh-CN page %s should be at root", page)
		}
	}

	// en under /en/.
	for _, page := range []string{"index.html", "login.html", "docs.html"} {
		if _, err := os.Stat(filepath.Join(outDir, "en", page)); os.IsNotExist(err) {
			t.Errorf("en page %s should be under /en/", page)
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

	// Check zh-CN i18n.js.
	zhJS, err := os.ReadFile(filepath.Join(outDir, "i18n.zh-CN.js"))
	if err != nil {
		t.Fatalf("read i18n.zh-CN.js: %v", err)
	}
	zhStr := string(zhJS)
	if !strings.Contains(zhStr, "window.AnotifyI18n") {
		t.Errorf("i18n.zh-CN.js should contain window.AnotifyI18n, got:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "index.title") {
		t.Errorf("i18n.zh-CN.js should contain key 'index.title', got:\n%s", zhStr)
	}
	if !strings.Contains(zhStr, "你好") {
		t.Errorf("i18n.zh-CN.js should contain Chinese text, got:\n%s", zhStr)
	}

	// Check en i18n.js.
	enJS, err := os.ReadFile(filepath.Join(outDir, "i18n.en.js"))
	if err != nil {
		t.Fatalf("read i18n.en.js: %v", err)
	}
	enStr := string(enJS)
	if !strings.Contains(enStr, "window.AnotifyI18n") {
		t.Errorf("i18n.en.js should contain window.AnotifyI18n, got:\n%s", enStr)
	}
	if !strings.Contains(enStr, "Hello") {
		t.Errorf("i18n.en.js should contain English text, got:\n%s", enStr)
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

	// After multi-language rendering, the cache should contain layout+page combos.
	gen.tmplCache.mu.RLock()
	count := len(gen.tmplCache.entries)
	gen.tmplCache.mu.RUnlock()

	// 3 pages × 1 layout each (login uses login layout, index/docs use base)
	// Cache key = layout+page, so there should be 3 entries.
	if count != 3 {
		t.Errorf("template cache should have 3 entries, got %d", count)
	}
}

func TestNew_DefaultLangs(t *testing.T) {
	srcDir := t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)
	outDir := t.TempDir()

	// No langs passed: should default to ["zh-CN"].
	gen, err := New(srcDir, outDir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(gen.langs) != 1 || gen.langs[0] != "zh-CN" {
		t.Errorf("default langs should be [zh-CN], got %v", gen.langs)
	}
	if gen.defaultLang != "zh-CN" {
		t.Errorf("default lang should be zh-CN, got %s", gen.defaultLang)
	}
}

func TestNew_EmptySrcDir(t *testing.T) {
	outDir := t.TempDir()
	_, err := New("", outDir, []string{"zh-CN"})
	if err == nil {
		t.Error("empty srcDir should return an error")
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
		t.Error("no pages should return an error")
	}
}

// TestGenerate_FourLanguages verifies that 4-language generation produces
// output directories for ja and es, with correct <html lang> attributes and
// i18n JS files for all languages.
func TestGenerate_FourLanguages(t *testing.T) {
	srcDir := writeFixture(t)
	outDir := t.TempDir()

	langs := []string{"zh-CN", "en", "ja", "es"}
	gen, err := New(srcDir, outDir, langs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Expected output directories: root (zh-CN), en/, ja/, es/.
	expectDirs := map[string]string{
		"zh-CN": "",
		"en":    "en",
		"ja":    "ja",
		"es":    "es",
	}
	for lang, dir := range expectDirs {
		indexPath := filepath.Join(outDir, dir, "index.html")
		content, err := os.ReadFile(indexPath)
		if err != nil {
			t.Errorf("lang %s: read index.html at %s: %v", lang, indexPath, err)
			continue
		}
		s := string(content)
		wantLang := `<html lang="` + lang + `">`
		if !strings.Contains(s, wantLang) {
			t.Errorf("lang %s: index.html should set %s, got:\n%s", lang, wantLang, s)
		}

		// Verify the i18n JS file exists.
		jsPath := filepath.Join(outDir, "i18n."+lang+".js")
		if _, err := os.Stat(jsPath); os.IsNotExist(err) {
			t.Errorf("lang %s: i18n.%s.js should exist", lang, lang)
		}
	}

	// Spot-check ja and es translations.
	jaIndex, _ := os.ReadFile(filepath.Join(outDir, "ja", "index.html"))
	if !strings.Contains(string(jaIndex), "こんにちは") {
		t.Errorf("ja index.html should contain こんにちは, got:\n%s", string(jaIndex))
	}
	esIndex, _ := os.ReadFile(filepath.Join(outDir, "es", "index.html"))
	if !strings.Contains(string(esIndex), "Hola") {
		t.Errorf("es index.html should contain Hola, got:\n%s", string(esIndex))
	}
}

// TestGenerate_PageAndLangsData verifies that PageData.Page and PageData.Langs
// are correctly populated and usable in templates (for the language switcher
// and hreflang alternate links).
func TestGenerate_PageAndLangsData(t *testing.T) {
	srcDir := t.TempDir()

	// Layout that renders Page and iterates Langs, plus hreflang links.
	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `<!doctype html>
<html lang="{{.Lang}}">
<head>
{{range .Langs}}  <link rel="alternate" hreflang="{{.Code}}" href="{{.Prefix}}/{{$.Page}}" />
{{end}}  <link rel="alternate" hreflang="x-default" href="/{{$.Page}}" />
</head>
<body>
<div id="page-name">{{.Page}}</div>
<ul id="lang-list">
{{range .Langs}}  <li class="lang-item" data-code="{{.Code}}" data-prefix="{{.Prefix}}" data-native="{{.NativeName}}" data-short="{{.ShortName}}"{{if eq .Code $.Lang}} aria-current="true"{{end}}>{{.NativeName}}</li>
{{end}}</ul>
</body>
</html>`)

	mustWrite(t, filepath.Join(srcDir, "pages", "keys.html"), `{{define "content"}}{{end}}`)

	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)
	mustWrite(t, filepath.Join(srcDir, "locales", "en.yaml"), `key: val`)
	mustWrite(t, filepath.Join(srcDir, "locales", "ja.yaml"), `key: 値`)
	mustWrite(t, filepath.Join(srcDir, "locales", "es.yaml"), `key: valor`)

	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "en", "ja", "es"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// Check the en version of keys.html (non-default language, has prefix).
	enKeys, err := os.ReadFile(filepath.Join(outDir, "en", "keys.html"))
	if err != nil {
		t.Fatalf("read en/keys.html: %v", err)
	}
	s := string(enKeys)

	// Page name should be "keys.html".
	if !strings.Contains(s, `id="page-name">keys.html<`) {
		t.Errorf("en/keys.html should contain page name keys.html, got:\n%s", s)
	}

	// Should have 4 hreflang alternate links + 1 x-default.
	hreflangCount := strings.Count(s, `rel="alternate" hreflang="`)
	if hreflangCount != 5 {
		t.Errorf("en/keys.html should have 5 hreflang links (4 langs + x-default), got %d", hreflangCount)
	}

	// x-default should point to the root path (default language).
	if !strings.Contains(s, `hreflang="x-default" href="/keys.html"`) {
		t.Errorf("en/keys.html x-default should point to /keys.html, got:\n%s", s)
	}

	// The en version's self-referencing hreflang should have an empty prefix
	// relative path for the default language link: href="/keys.html"
	// (zh-CN prefix is "").
	if !strings.Contains(s, `hreflang="zh-CN" href="/keys.html"`) {
		t.Errorf("en/keys.html should have zh-CN href pointing to /keys.html, got:\n%s", s)
	}
	// en href should be /en/keys.html.
	if !strings.Contains(s, `hreflang="en" href="/en/keys.html"`) {
		t.Errorf("en/keys.html should have en href pointing to /en/keys.html, got:\n%s", s)
	}
	// ja href should be /ja/keys.html.
	if !strings.Contains(s, `hreflang="ja" href="/ja/keys.html"`) {
		t.Errorf("en/keys.html should have ja href pointing to /ja/keys.html, got:\n%s", s)
	}

	// Check LangInfo native names and current-language marking.
	if !strings.Contains(s, `data-native="中文"`) {
		t.Errorf("en/keys.html should have zh-CN native name 中文, got:\n%s", s)
	}
	if !strings.Contains(s, `data-native="English"`) {
		t.Errorf("en/keys.html should have en native name English, got:\n%s", s)
	}
	if !strings.Contains(s, `data-native="日本語"`) {
		t.Errorf("en/keys.html should have ja native name 日本語, got:\n%s", s)
	}
	if !strings.Contains(s, `data-native="Español"`) {
		t.Errorf("en/keys.html should have es native name Español, got:\n%s", s)
	}

	// Short names.
	if !strings.Contains(s, `data-short="EN"`) {
		t.Errorf("en/keys.html should have en short name EN, got:\n%s", s)
	}
	if !strings.Contains(s, `data-short="ES"`) {
		t.Errorf("en/keys.html should have es short name ES, got:\n%s", s)
	}

	// The current language (en) item should have aria-current="true".
	if !strings.Contains(s, `data-code="en"`) {
		t.Errorf("en/keys.html should have a lang-item with code en, got:\n%s", s)
	}
	// Count aria-current occurrences: exactly 1 (the current language).
	if c := strings.Count(s, `aria-current="true"`); c != 1 {
		t.Errorf("en/keys.html should have exactly 1 aria-current=true (current lang), got %d", c)
	}

	// Now check the zh-CN (default) version: its hreflang self-link should be
	// at the root path (prefix="").
	zhKeys, err := os.ReadFile(filepath.Join(outDir, "keys.html"))
	if err != nil {
		t.Fatalf("read keys.html: %v", err)
	}
	zhStr := string(zhKeys)
	// zh-CN version: the current language is zh-CN, so its lang-item should
	// have aria-current="true".
	zhCurrentIdx := strings.Index(zhStr, `data-code="zh-CN"`)
	if zhCurrentIdx < 0 {
		t.Errorf("zh-CN keys.html should have a lang-item with code zh-CN, got:\n%s", zhStr)
	} else {
		// aria-current should appear within the same <li> (within ~120 chars).
		snippet := zhStr[zhCurrentIdx:]
		if !strings.Contains(snippet[:120], `aria-current="true"`) {
			t.Errorf("zh-CN keys.html: zh-CN item should have aria-current=true, got snippet:\n%s", snippet[:120])
		}
	}
	// zh-CN prefix is "" so the en link should be /en/keys.html and the
	// zh-CN self-link href="/keys.html".
	if !strings.Contains(zhStr, `hreflang="zh-CN" href="/keys.html"`) {
		t.Errorf("zh-CN keys.html should have zh-CN href at /keys.html, got:\n%s", zhStr)
	}
}

// TestGenerate_SingleLangLangsFilled verifies that with a single language,
// Langs is still populated (so templates can use {{if gt (len .Langs) 1}} to
// conditionally render the switcher).
func TestGenerate_SingleLangLangsFilled(t *testing.T) {
	srcDir := t.TempDir()

	mustWrite(t, filepath.Join(srcDir, "layouts", "base.html"), `{{range .Langs}}<span data-lang="{{.Code}}">{{.NativeName}}</span>{{end}}<span data-page="{{.Page}}"></span>`)
	mustWrite(t, filepath.Join(srcDir, "pages", "index.html"), `{{define "content"}}{{end}}`)
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)

	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(outDir, "index.html"))
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	s := string(content)
	// Single language: one lang span with the correct native name.
	if !strings.Contains(s, `data-lang="zh-CN">中文<`) {
		t.Errorf("single-lang build should still fill Langs with zh-CN=中文, got:\n%s", s)
	}
	// Page name should be present.
	if !strings.Contains(s, `data-page="index.html"`) {
		t.Errorf("single-lang build should fill Page=index.html, got:\n%s", s)
	}
}

// TestBuildLangs_NativeNameFallback verifies that unknown language codes
// fall back to the code itself for both NativeName and ShortName (extensibility).
func TestBuildLangs_NativeNameFallback(t *testing.T) {
	srcDir := t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "locales", "zh-CN.yaml"), `key: 值`)
	mustWrite(t, filepath.Join(srcDir, "locales", "xx-YY.yaml"), `key: val`)

	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "xx-YY"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	langs := gen.buildLangs()
	if len(langs) != 2 {
		t.Fatalf("buildLangs should return 2 entries, got %d", len(langs))
	}

	// xx-YY is not in the nativeNames table: should fall back to the code.
	var xxYY *LangInfo
	for i := range langs {
		if langs[i].Code == "xx-YY" {
			xxYY = &langs[i]
		}
	}
	if xxYY == nil {
		t.Fatalf("buildLangs should include xx-YY, got %+v", langs)
	}
	if xxYY.NativeName != "xx-YY" {
		t.Errorf("unknown code NativeName should fall back to code, got %q", xxYY.NativeName)
	}
	if xxYY.ShortName != "xx-YY" {
		t.Errorf("unknown code ShortName should fall back to code, got %q", xxYY.ShortName)
	}
	if xxYY.Prefix != "/xx-YY" {
		t.Errorf("non-default code Prefix should be /xx-YY, got %q", xxYY.Prefix)
	}
}
