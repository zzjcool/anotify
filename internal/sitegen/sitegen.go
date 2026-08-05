// Package sitegen is the Anotify build-time static site generator.
//
// Inspired by Hugo (static site generation) and rssyes (Go html/template
// layouts + template caching), it combines layouts + pages + locales under
// web-src/ into final static HTML at build time. No runtime template engine
// is introduced; the output is plain static files.
//
// Core flow: parse layout+page templates (cached for reuse) → iterate
// languages × pages → render into bytes.Buffer → write output.
// i18n is resolved at build time: {{t "key"}} in templates is replaced with
// the localized string for the current language during rendering.
package sitegen

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Generator is the static site generator, holding the source dir, output dir,
// language list, and translations.
type Generator struct {
	srcDir       string                       // web-src/ root
	outDir       string                       // output directory (web/)
	langs        []string                     // supported languages; first is default (root path)
	translations map[string]map[string]string // lang → flattenedKey → value
	defaultLang  string                       // default language (langs[0]), used for missing-key fallback
	tmplCache    *templateCache               // template cache
}

// templateCache caches parsed layout+page template combinations to avoid
// re-parsing. Inspired by rssyes TemplateCache: RWMutex-guarded.
type templateCache struct {
	mu      sync.RWMutex
	entries map[string]*templateEntry
}

type templateEntry struct {
	layoutName string        // layout template name (e.g. "base.html")
	pageName   string        // page template name (e.g. "index.html")
	tmpl       *templateTree // parsed template tree
}

// nativeNames maps a language code to its native display name and short label.
// Unknown codes fall back to the code itself for both fields (extensibility
// without code changes).
var nativeNames = map[string]struct{ Native, Short string }{
	"zh-CN": {"中文", "中文"},
	"en":    {"English", "EN"},
	"ja":    {"日本語", "日本語"},
	"es":    {"Español", "ES"},
}

// langInfo builds the LangInfo entry for a given language code and generator.
func (g *Generator) langInfo(code string) LangInfo {
	names := nativeNames[code]
	native, short := names.Native, names.Short
	if native == "" {
		native = code
	}
	if short == "" {
		short = code
	}
	return LangInfo{
		Code:       code,
		Prefix:     g.langPrefix(code),
		NativeName: native,
		ShortName:  short,
	}
}

// buildLangs constructs the full []LangInfo list for PageData.
func (g *Generator) buildLangs() []LangInfo {
	infos := make([]LangInfo, 0, len(g.langs))
	for _, code := range g.langs {
		infos = append(infos, g.langInfo(code))
	}
	return infos
}

// New creates a generator. srcDir is the web-src root, outDir is the output
// directory, langs is the language list (first is the default language,
// output to the root path; the rest go to /{lang}/).
func New(srcDir, outDir string, langs []string) (*Generator, error) {
	if srcDir == "" {
		return nil, fmt.Errorf("sitegen: source directory must not be empty")
	}
	if outDir == "" {
		return nil, fmt.Errorf("sitegen: output directory must not be empty")
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

	// Load translation files.
	localesDir := filepath.Join(srcDir, "locales")
	for _, lang := range langs {
		t, err := LoadTranslations(localesDir, lang)
		if err != nil {
			return nil, fmt.Errorf("sitegen: load translations %s: %w", lang, err)
		}
		g.translations[lang] = t
	}

	return g, nil
}

// Generate runs the full generation: render all languages × pages → write HTML + i18n.{lang}.js.
func (g *Generator) Generate() error {
	// 1. Discover all pages.
	pages, err := g.discoverPages()
	if err != nil {
		return fmt.Errorf("sitegen: discover pages: %w", err)
	}
	if len(pages) == 0 {
		return fmt.Errorf("sitegen: no pages found (in %s/pages/)", g.srcDir)
	}

	// 2. Discover all layouts.
	layouts, err := g.discoverLayouts()
	if err != nil {
		return fmt.Errorf("sitegen: discover layouts: %w", err)
	}
	if len(layouts) == 0 {
		return fmt.Errorf("sitegen: no layouts found (in %s/layouts/)", g.srcDir)
	}

	// 3. Ensure the output directory exists.
	if err := os.MkdirAll(g.outDir, 0o755); err != nil {
		return fmt.Errorf("sitegen: create output directory: %w", err)
	}

	// 4. Iterate languages × pages, render and write output.
	for _, lang := range g.langs {
		langPrefix := g.langPrefix(lang)
		outLangDir := g.outDir
		if langPrefix != "" {
			outLangDir = filepath.Join(g.outDir, langPrefix)
			if err := os.MkdirAll(outLangDir, 0o755); err != nil {
				return fmt.Errorf("sitegen: create language directory %s: %w", outLangDir, err)
			}
		}

		for _, page := range pages {
			html, err := g.renderPage(page, lang, layouts)
			if err != nil {
				return fmt.Errorf("sitegen: render page %s [%s]: %w", page.name, lang, err)
			}

			outPath := filepath.Join(outLangDir, page.name)
			if err := os.WriteFile(outPath, html, 0o644); err != nil {
				return fmt.Errorf("sitegen: write %s: %w", outPath, err)
			}
		}
	}

	// 5. Generate i18n.{lang}.js for runtime JS strings.
	if err := g.GenerateI18nJS(g.outDir); err != nil {
		return fmt.Errorf("sitegen: generate i18n JS: %w", err)
	}

	return nil
}

// renderPage renders a single page (for the given language) and returns the full HTML.
func (g *Generator) renderPage(page pageSource, lang string, layouts []string) ([]byte, error) {
	layoutName := page.layout
	if layoutName == "" {
		layoutName = "base.html"
	} else if !strings.HasSuffix(layoutName, ".html") {
		layoutName += ".html"
	}

	// Check that the layout exists.
	layoutFound := false
	for _, l := range layouts {
		if l == layoutName {
			layoutFound = true
			break
		}
	}
	if !layoutFound {
		return nil, fmt.Errorf("layout %s does not exist", layoutName)
	}

	// Get or parse the template (cached).
	tmpl, err := g.getOrParseTemplate(layoutName, page.name)
	if err != nil {
		return nil, err
	}

	// Build the template data, including the page name and full language list
	// for the language switcher and hreflang alternate links.
	data := PageData{
		Lang:       lang,
		LangPrefix: g.langPrefix(lang),
		Page:       page.name,
		Langs:      g.buildLangs(),
	}

	// Create the t function bound to the current language.
	tFunc := g.makeTFunc(lang)

	// Render into a bytes.Buffer (perf pattern from rssyes renderTemplate).
	var buf bytes.Buffer
	if err := tmpl.execute(&buf, data, tFunc); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}

	return buf.Bytes(), nil
}

// getOrParseTemplate returns the parsed layout+page template tree, caching it.
// Inspired by rssyes LoadTemplateWithLayout: cache key = layout+page.
func (g *Generator) getOrParseTemplate(layoutName, pageName string) (*templateTree, error) {
	cacheKey := layoutName + "+" + pageName

	g.tmplCache.mu.RLock()
	if entry, ok := g.tmplCache.entries[cacheKey]; ok {
		g.tmplCache.mu.RUnlock()
		return entry.tmpl, nil
	}
	g.tmplCache.mu.RUnlock()

	// Parse layout + page.
	layoutPath := filepath.Join(g.srcDir, "layouts", layoutName)
	pagePath := filepath.Join(g.srcDir, "pages", pageName)

	tmpl, err := parseTemplateTree(layoutPath, pagePath)
	if err != nil {
		return nil, fmt.Errorf("parse template %s+%s: %w", layoutName, pageName, err)
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

// makeTFunc creates a translation function bound to the current language.
// On a missing key it falls back to the default language's string, then to
// the key itself. Each fallback path emits a stderr warning listing the
// missing keys (aggregated per language per Generate run) so developers can
// spot untranslated keys without the build failing.
func (g *Generator) makeTFunc(lang string) func(string) string {
	return func(key string) string {
		// Current language.
		if vals, ok := g.translations[lang]; ok {
			if v, ok := vals[key]; ok {
				return v
			}
		}
		// Fall back to the default language.
		if lang != g.defaultLang {
			if vals, ok := g.translations[g.defaultLang]; ok {
				if v, ok := vals[key]; ok {
					log.Printf("sitegen: WARN missing i18n key %q in language %q, fell back to %q", key, lang, g.defaultLang)
					return v
				}
			}
		}
		// Key missing in all languages: return the raw key and warn.
		log.Printf("sitegen: WARN missing i18n key %q in ALL languages (including default %q), rendered raw key", key, g.defaultLang)
		return key
	}
}

// langPrefix returns the language prefix: "" for the default language, "/{lang}" otherwise.
func (g *Generator) langPrefix(lang string) string {
	if lang == g.defaultLang {
		return ""
	}
	return "/" + lang
}

// pageSource describes a single page source file.
type pageSource struct {
	name   string // file name (e.g. "index.html")
	layout string // specified layout (empty defaults to base.html)
}

// discoverPages scans the pages/ directory and returns all .html pages.
// A page may specify its layout via a comment on the first line: <!-- layout: login -->.
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
			return fmt.Errorf("read page %s: %w", path, err)
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

// discoverLayouts scans the layouts/ directory and returns all .html layout file names.
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

// PageCount returns the number of discovered pages (for CLI reporting).
func (g *Generator) PageCount() (int, error) {
	pages, err := g.discoverPages()
	if err != nil {
		return 0, err
	}
	return len(pages), nil
}

// extractLayoutHint extracts the layout directive from the page content.
// Format: <!-- layout: login --> (must be within the first 200 characters).
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
