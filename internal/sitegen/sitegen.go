// Package sitegen 是 Anotify 构建期静态站点生成器。
//
// 借鉴 Hugo（静态站点生成）与 rssyes（Go html/template 布局 + 模板缓存）思想，
// 在构建期把 layouts（布局）+ pages（页面内容块）+ locales（i18n 翻译）
// 合成为最终静态 HTML。不引入运行时模板引擎，产物仍是纯静态文件。
//
// 核心流程：解析布局+页面模板（缓存复用）→ 遍历语言×页面 → bytes.Buffer 渲染 → 输出。
// i18n 在构建期完成：模板内 {{t "key"}} 在渲染时按当前语言替换为对应文案。
package sitegen

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Generator 是静态站点生成器，持有源目录、输出目录、语言列表和翻译。
type Generator struct {
	srcDir       string                       // web-src/ 根目录
	outDir       string                       // 输出目录（web/）
	langs        []string                     // 支持的语言列表，第一个为默认（根路径）
	translations map[string]map[string]string // lang → flattenedKey → value
	defaultLang  string                       // 默认语言（langs[0]），用于缺 key 回退
	tmplCache    *templateCache               // 模板缓存
}

// templateCache 缓存已解析的模板（布局+页面组合），避免重复解析。
// 借鉴 rssyes TemplateCache：RWMutex 保护、devMode 可旁路。
type templateCache struct {
	mu      sync.RWMutex
	entries map[string]*templateEntry
}

type templateEntry struct {
	layoutName string        // 布局模板名（如 "base.html"）
	pageName   string        // 页面模板名（如 "index.html"）
	tmpl       *templateTree // 已解析的模板树
}

// New 创建生成器。srcDir 为 web-src 根目录，outDir 为输出目录，
// langs 为语言列表（第一个为默认语言，输出到根路径，其余到 /{lang}/）。
func New(srcDir, outDir string, langs []string) (*Generator, error) {
	if srcDir == "" {
		return nil, fmt.Errorf("sitegen: 源目录不能为空")
	}
	if outDir == "" {
		return nil, fmt.Errorf("sitegen: 输出目录不能为空")
	}
	if len(langs) == 0 {
		langs = []string{"zh-CN"}
	}

	g := &Generator{
		srcDir:       srcDir,
		outDir:       outDir,
		langs:        langs,
		defaultLang:  langs[0],
		translations: make(map[string]map[string]string),
		tmplCache: &templateCache{
			entries: make(map[string]*templateEntry),
		},
	}

	// 加载翻译文件
	localesDir := filepath.Join(srcDir, "locales")
	for _, lang := range langs {
		t, err := LoadTranslations(localesDir, lang)
		if err != nil {
			return nil, fmt.Errorf("sitegen: 加载翻译 %s: %w", lang, err)
		}
		g.translations[lang] = t
	}

	return g, nil
}

// Generate 执行完整生成：渲染所有语言×页面 → 输出 HTML + i18n.{lang}.js。
func (g *Generator) Generate() error {
	// 1. 扫描所有页面
	pages, err := g.discoverPages()
	if err != nil {
		return fmt.Errorf("sitegen: 扫描页面: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("sitegen: 未找到页面（在 %s/pages/）", g.srcDir)
	}

	// 2. 扫描所有布局
	layouts, err := g.discoverLayouts()
	if err != nil {
		return fmt.Errorf("sitegen: 扫描布局: %w", err)
	}
	if len(layouts) == 0 {
		return fmt.Errorf("sitegen: 未找到布局（在 %s/layouts/）", g.srcDir)
	}

	// 3. 确保输出目录存在
	if err := os.MkdirAll(g.outDir, 0o755); err != nil {
		return fmt.Errorf("sitegen: 创建输出目录: %w", err)
	}

	// 4. 遍历语言 × 页面，渲染输出
	for _, lang := range g.langs {
		langPrefix := g.langPrefix(lang)
		outLangDir := g.outDir
		if langPrefix != "" {
			outLangDir = filepath.Join(g.outDir, langPrefix)
			if err := os.MkdirAll(outLangDir, 0o755); err != nil {
				return fmt.Errorf("sitegen: 创建语言目录 %s: %w", outLangDir, err)
			}
		}

		for _, page := range pages {
			html, err := g.renderPage(page, lang, layouts)
			if err != nil {
				return fmt.Errorf("sitegen: 渲染页面 %s [%s]: %w", page.name, lang, err)
			}

			outPath := filepath.Join(outLangDir, page.name)
			if err := os.WriteFile(outPath, html, 0o644); err != nil {
				return fmt.Errorf("sitegen: 写入 %s: %w", outPath, err)
			}
		}
	}

	// 5. 生成 i18n.{lang}.js（供运行时 JS 文案用）
	if err := g.GenerateI18nJS(g.outDir); err != nil {
		return fmt.Errorf("sitegen: 生成 i18n JS: %w", err)
	}

	return nil
}

// renderPage 渲染单个页面（指定语言），返回完整 HTML。
func (g *Generator) renderPage(page pageSource, lang string, layouts []string) ([]byte, error) {
	layoutName := page.layout
	if layoutName == "" {
		layoutName = "base.html"
	} else if !strings.HasSuffix(layoutName, ".html") {
		layoutName += ".html"
	}

	// 查找布局是否存在
	layoutFound := false
	for _, l := range layouts {
		if l == layoutName {
			layoutFound = true
			break
		}
	}
	if !layoutFound {
		return nil, fmt.Errorf("布局 %s 不存在", layoutName)
	}

	// 获取或解析模板（缓存）
	tmpl, err := g.getOrParseTemplate(layoutName, page.name)
	if err != nil {
		return nil, err
	}

	// 构建模板数据
	data := PageData{
		Lang:       lang,
		LangPrefix: g.langPrefix(lang),
	}

	// 创建绑定当前语言的 t 函数
	tFunc := g.makeTFunc(lang)

	// 用 bytes.Buffer 渲染（借鉴 rssyes renderTemplate 的性能优化）
	var buf bytes.Buffer
	if err := tmpl.execute(&buf, data, tFunc); err != nil {
		return nil, fmt.Errorf("执行模板: %w", err)
	}

	return buf.Bytes(), nil
}

// getOrParseTemplate 获取或解析布局+页面组合模板（带缓存）。
// 借鉴 rssyes LoadTemplateWithLayout：缓存键 = layout+page。
func (g *Generator) getOrParseTemplate(layoutName, pageName string) (*templateTree, error) {
	cacheKey := layoutName + "+" + pageName

	g.tmplCache.mu.RLock()
	if entry, ok := g.tmplCache.entries[cacheKey]; ok {
		g.tmplCache.mu.RUnlock()
		return entry.tmpl, nil
	}
	g.tmplCache.mu.RUnlock()

	// 解析布局 + 页面
	layoutPath := filepath.Join(g.srcDir, "layouts", layoutName)
	pagePath := filepath.Join(g.srcDir, "pages", pageName)

	tmpl, err := parseTemplateTree(layoutPath, pagePath)
	if err != nil {
		return nil, fmt.Errorf("解析模板 %s+%s: %w", layoutName, pageName, err)
	}

	g.tmplCache.mu.Lock()
	g.tmplCache.entries[cacheKey] = &templateEntry{
		layoutName: layoutName,
		pageName:   pageName,
		tmpl:       tmpl,
	}
	g.tmplCache.mu.Unlock()

	return tmpl, nil
}

// makeTFunc 创建绑定当前语言的翻译函数。
// 缺 key 时回退默认语言，再回退返回 key 本身。
func (g *Generator) makeTFunc(lang string) func(string) string {
	return func(key string) string {
		// 当前语言
		if vals, ok := g.translations[lang]; ok {
			if v, ok := vals[key]; ok {
				return v
			}
		}
		// 回退默认语言
		if lang != g.defaultLang {
			if vals, ok := g.translations[g.defaultLang]; ok {
				if v, ok := vals[key]; ok {
					return v
				}
			}
		}
		// 回退 key 本身
		return key
	}
}

// langPrefix 返回语言前缀：默认语言为 ""，其余为 "/{lang}"。
func (g *Generator) langPrefix(lang string) string {
	if lang == g.defaultLang {
		return ""
	}
	return "/" + lang
}

// pageSource 描述一个页面源文件。
type pageSource struct {
	name   string // 文件名（如 "index.html"）
	layout string // 指定布局（空则默认 base.html）
}

// discoverPages 扫描 pages/ 目录，返回所有 .html 页面。
// 支持在页面文件首行用注释指定布局：<!-- layout: login -->。
func (g *Generator) discoverPages() ([]pageSource, error) {
	pagesDir := filepath.Join(g.srcDir, "pages")
	var pages []pageSource

	err := filepath.WalkDir(pagesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if !strings.HasSuffix(name, ".html") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取页面 %s: %w", path, err)
		}

		layout := extractLayoutHint(string(content))
		pages = append(pages, pageSource{name: name, layout: layout})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pages, nil
}

// discoverLayouts 扫描 layouts/ 目录，返回所有 .html 布局文件名。
func (g *Generator) discoverLayouts() ([]string, error) {
	layoutsDir := filepath.Join(g.srcDir, "layouts")
	var layouts []string

	err := filepath.WalkDir(layoutsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		if strings.HasSuffix(name, ".html") {
			layouts = append(layouts, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return layouts, nil
}

// PageCount 返回已发现的页面数量（供 CLI 上报）。
func (g *Generator) PageCount() (int, error) {
	pages, err := g.discoverPages()
	if err != nil {
		return 0, err
	}
	return len(pages), nil
}

// extractLayoutHint 从页面内容首行提取布局指定。
// 格式：<!-- layout: login -->（必须在前 200 字符内）。
func extractLayoutHint(content string) string {
	prefix := content
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	idx := strings.Index(prefix, "<!-- layout:")
	if idx < 0 {
		return ""
	}
	rest := prefix[idx+len("<!-- layout:"):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}
