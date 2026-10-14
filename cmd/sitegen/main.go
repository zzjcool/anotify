// Command sitegen is the Anotify build-time static site generator.
//
// It combines layouts + pages + locales under web-src/ into final static
// HTML, written to web/.
//
// Usage:
//
//	go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en,ja,es
//
// The first language in the list (the default) is output to the root path;
// the rest go to /{lang}/ subdirectories. Additionally, web/i18n.{lang}.js
// files are generated for runtime JS strings.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/zzjcool/anotify/internal/sitegen"
)

func main() {
	srcDir := flag.String("src", "web-src", "page source directory (layouts/pages/locales)")
	outDir := flag.String("out", "web", "output directory")
	langsFlag := flag.String("langs", "zh-CN,en,ja,es", "supported languages (comma-separated, first is default)")
	// Makefile 在 git 检出时传入构建版本号。页面 footer 尚未消费该值，
	// 这里接受并打印，避免未定义 flag 让 make fe / make build / make ci 直接失败。
	version := flag.String("version", "", "build version label (printed; not yet rendered into HTML)")
	flag.Parse()

	langs := strings.Split(*langsFlag, ",")
	for i := range langs {
		langs[i] = strings.TrimSpace(langs[i])
	}
	// Drop empties.
	filtered := langs[:0]
	for _, l := range langs {
		if l != "" {
			filtered = append(filtered, l)
		}
	}
	langs = filtered
	if len(langs) == 0 {
		fmt.Fprintln(os.Stderr, "error: at least one language is required")
		os.Exit(1)
	}

	gen, err := sitegen.New(*srcDir, *outDir, langs)
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}

	if err := gen.Generate(); err != nil {
		log.Fatalf("generation failed: %v", err)
	}

	pages, _ := gen.PageCount()
	if *version != "" {
		fmt.Printf("✅ sitegen done: %d languages × %d pages → %s (version %s)\n", len(langs), pages, *outDir, *version)
		return
	}
	fmt.Printf("✅ sitegen done: %d languages × %d pages → %s\n", len(langs), pages, *outDir)
}
