package sitegen

import (
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
)

// PageData 是注入模板的数据。
type PageData struct {
	Lang       string // 当前语言代码（如 "zh-CN" / "en"）
	LangPrefix string // 语言 URL 前缀（默认语言为 ""，其余为 "/{lang}"）
}

// templateTree 封装一个已解析的 html/template 模板树，
// 及按当前语言渲染的能力。t 函数在每次渲染时动态绑定。
type templateTree struct {
	tmpl     *template.Template
	execName string // 执行时用哪个模板名（布局唯一名，如 "__layout__"）
}

// parseTemplateTree 解析布局模板和页面模板，组合为一棵模板树。
// 布局文件定义骨架（含 {{block "content" .}} 等），
// 页面文件定义 {{define "content"}} 等块来覆盖布局中的 block。
//
// 关键挑战：Go html/template 的 ParseFiles 按文件 basename 命名模板，
// 若布局和页面同名（如 layouts/login.html 和 pages/login.html），
// 后解析的会覆盖前者。解决：用 Parse（而非 ParseFiles）直接指定
// 模板名，避免 basename 冲突。
func parseTemplateTree(layoutPath, pagePath string) (*templateTree, error) {
	// 创建时声明 t 函数（占位），使解析时模板中的 {{t "key"}} 能通过校验。
	// 真正的翻译逻辑在 execute 时通过 Clone + Funcs 注入（绑定当前语言）。
	root := template.New("__root__").Funcs(template.FuncMap{
		"t": func(string) string { return "" },
	})

	// 先解析布局——用 Parse 指定唯一名 __layout__，避免与页面文件 basename 冲突。
	layoutContent, err := os.ReadFile(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("读取布局 %s: %w", layoutPath, err)
	}
	// 先定义 __layout__ 模板名，再 Parse 内容到该模板。
	layoutTmpl := root.New("__layout__")
	_, err = layoutTmpl.Parse(string(layoutContent))
	if err != nil {
		return nil, fmt.Errorf("解析布局 %s: %w", filepath.Base(layoutPath), err)
	}

	// 再解析页面——页面中的 {{define}} 注册到同一模板树，
	// 覆盖布局中 {{block}} 的默认实现。
	// 页面内容可能有顶层文本（如 <!-- layout --> 注释），
	// 用唯一名 __page__ 避免覆盖 __layout__。
	pageContent, err := os.ReadFile(pagePath)
	if err != nil {
		return nil, fmt.Errorf("读取页面 %s: %w", pagePath, err)
	}
	pageTmpl := root.New("__page__")
	_, err = pageTmpl.Parse(string(pageContent))
	if err != nil {
		return nil, fmt.Errorf("解析页面 %s: %w", filepath.Base(pagePath), err)
	}

	// 执行时按 __layout__ 名执行，其 {{block}} 被页面 {{define}} 覆盖。
	return &templateTree{tmpl: root, execName: "__layout__"}, nil
}

// execute 用给定数据和翻译函数执行模板树。
// 每次 execute 都 Clone 模板并注入当前语言的 t 函数，
// 这样同一棵模板树可被多语言复用（缓存的价值）。
func (tt *templateTree) execute(w io.Writer, data PageData, tFunc func(string) string) error {
	// Clone 继承已解析的模板树，允许追加 Funcs
	clone, err := tt.tmpl.Clone()
	if err != nil {
		return fmt.Errorf("clone 模板: %w", err)
	}

	// 注入 t 函数（绑定当前语言）
	clone = clone.Funcs(template.FuncMap{
		"t": tFunc,
	})

	// 按 __layout__ 名执行（触发 {{block}} → 被页面 {{define}} 覆盖）
	if err := clone.ExecuteTemplate(w, tt.execName, data); err != nil {
		return fmt.Errorf("execute: %w", err)
	}
	return nil
}
