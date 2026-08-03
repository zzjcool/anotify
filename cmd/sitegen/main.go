// Command sitegen 是 Anotify 构建期静态站点生成器。
//
// 把 web-src/ 下的 layouts（布局）+ pages（页面内容块）+ locales（i18n 翻译）
// 合成为最终静态 HTML，输出到 web/。
//
// 用法：
//
//	go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en
//
// 默认语言（列表第一个）输出到根路径，其余语言输出到 /{lang}/ 子目录。
// 额外生成 web/i18n.{lang}.js 供运行时 JS 文案使用。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/anotify/anotify/internal/sitegen"
)

func main() {
	srcDir := flag.String("src", "web-src", "页面源目录（layouts/pages/locales）")
	outDir := flag.String("out", "web", "输出目录")
	langsFlag := flag.String("langs", "zh-CN,en", "支持的语言列表（逗号分隔，第一个为默认）")
	flag.Parse()

	langs := strings.Split(*langsFlag, ",")
	for i := range langs {
		langs[i] = strings.TrimSpace(langs[i])
	}
	// 去空
	filtered := langs[:0]
	for _, l := range langs {
		if l != "" {
			filtered = append(filtered, l)
		}
	}
	langs = filtered
	if len(langs) == 0 {
		fmt.Fprintln(os.Stderr, "错误：至少需要一种语言")
		os.Exit(1)
	}

	gen, err := sitegen.New(*srcDir, *outDir, langs)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	if err := gen.Generate(); err != nil {
		log.Fatalf("生成失败: %v", err)
	}

	pages, _ := gen.PageCount()
	fmt.Printf("✅ sitegen 完成: %d 语言 × %d 页面 → %s\n", len(langs), pages, *outDir)
}
