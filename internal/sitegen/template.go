package sitegen

import (
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
)

// LangInfo describes a single supported language for template rendering.
// It is injected into PageData.Langs so templates can render a language
// switcher and hreflang alternate links.
type LangInfo struct {
	Code       string // BCP-47 language code, e.g. "zh-CN", "en"
	Prefix     string // URL prefix: "" for the default language, "/{lang}" otherwise
	NativeName string // display name in the language's own script, e.g. "中文", "English"
	ShortName  string // compact label for space-constrained UI, e.g. "EN", "ES"
}

// PageData is the data injected into templates during rendering.
type PageData struct {
	Lang       string     // current language code (e.g. "zh-CN" / "en")
	LangPrefix string     // language URL prefix ("" for default lang, "/{lang}" otherwise)
	Page       string     // current page file name, e.g. "keys.html" (for cross-language same-page links)
	Langs      []LangInfo // full list of supported languages (for language switcher + hreflang)
}

// templateTree wraps a parsed html/template tree and the ability to render
// it for a given language. The t function is bound dynamically per render.
type templateTree struct {
	tmpl     *template.Template
	execName string // which template name to execute (the layout's unique name, e.g. "__layout__")
}

// parseTemplateTree parses the layout and page templates into a single tree.
// The layout defines the skeleton (with {{block "content" .}} etc.),
// and the page defines {{define "content"}} blocks to override them.
//
// Key challenge: Go html/template ParseFiles names templates by file basename,
// so if layout and page share a name (layouts/login.html and pages/login.html)
// the later-parsed one overwrites the earlier. Solution: use Parse (not
// ParseFiles) with explicit unique template names to avoid basename clashes.
func parseTemplateTree(layoutPath, pagePath string) (*templateTree, error) {
	// Declare a placeholder t func at creation so {{t "key"}} in templates
	// passes validation during parsing. The real translation logic is injected
	// at execute time via Clone + Funcs (bound to the current language).
	root := template.New("__root__").Funcs(template.FuncMap{
		"t": func(string) string { return "" },
	})

	// Parse the layout first under the unique name __layout__ to avoid
	// basename collisions with page files.
	layoutContent, err := os.ReadFile(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("read layout %s: %w", layoutPath, err)
	}
	layoutTmpl := root.New("__layout__")
	_, err = layoutTmpl.Parse(string(layoutContent))
	if err != nil {
		return nil, fmt.Errorf("parse layout %s: %w", filepath.Base(layoutPath), err)
	}

	// Parse the page next: its {{define}} blocks register into the same tree,
	// overriding the layout's {{block}} defaults. Page content may have
	// top-level text (e.g. a <!-- layout --> comment), so use the unique name
	// __page__ to avoid overwriting __layout__.
	pageContent, err := os.ReadFile(pagePath)
	if err != nil {
		return nil, fmt.Errorf("read page %s: %w", pagePath, err)
	}
	pageTmpl := root.New("__page__")
	_, err = pageTmpl.Parse(string(pageContent))
	if err != nil {
		return nil, fmt.Errorf("parse page %s: %w", filepath.Base(pagePath), err)
	}

	// Execute by the __layout__ name; its {{block}} slots are overridden by the page's {{define}}.
	return &templateTree{tmpl: root, execName: "__layout__"}, nil
}

// execute renders the template tree with the given data and translation func.
// Each execute Clones the template and injects the current language's t func,
// so one parsed tree can be reused across languages (the value of caching).
func (tt *templateTree) execute(w io.Writer, data PageData, tFunc func(string) string) error {
	// Clone inherits the parsed tree and allows adding Funcs.
	clone, err := tt.tmpl.Clone()
	if err != nil {
		return fmt.Errorf("clone template: %w", err)
	}

	// Inject the t func bound to the current language.
	clone = clone.Funcs(template.FuncMap{
		"t": tFunc,
	})

	// Execute by the __layout__ name (triggers {{block}} → overridden by page {{define}}).
	if err := clone.ExecuteTemplate(w, tt.execName, data); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	return nil
}
