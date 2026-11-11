// Command sitegen is the Anotify build-time static site generator.
//
// It combines layouts + pages + locales under web-src/ into final static
// HTML, written to web/.
//
// Usage:
//
//	go run ./cmd/sitegen -src web-src -out web -langs zh-CN,en,ja,es -openapi api
//
// -openapi 默认 api/，读取 v4 五份 spec（resource/events/agent/ingress/auth.yaml）；
// 缺文件只 warn、五份全缺走 docs 真实空态，不回退旧的单文件 api/openapi.yaml。
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
	// API 文档页的端点清单来自 v4 的五份 canonical spec（五文件目录，构建期解析，页面里是静态 HTML）。
	// 默认 api/ 下读 resource/events/agent/ingress/auth.yaml，**不**回退旧单文件 api/openapi.yaml。
	// 传空字符串则不加载（只有测试 / 非常规打包需要）。
	openapiPath := flag.String("openapi", "api", "exact-five spec 目录（resource/events/agent/ingress/auth.yaml；空值跳过）")
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

	if *openapiPath != "" {
		set, err := gen.LoadSpecDir(*openapiPath)
		if err != nil {
			log.Fatalf("api specs failed: %v", err)
		}
		for _, name := range set.Missing() {
			log.Printf("sitegen: WARN api spec %s not found in %s (docs section omits it; no legacy fallback)", name, *openapiPath)
		}
		for _, warning := range set.Warnings {
			log.Printf("sitegen: WARN %s", warning)
		}
		for _, dup := range set.Duplicates {
			log.Printf("sitegen: WARN duplicate operation %s across specs (kept first occurrence)", dup)
		}
		present := len(set.Files) - len(set.Missing())
		fmt.Printf("sitegen: api docs %d operations from %d/%d specs in %s\n",
			set.OperationCount(), present, len(set.Files), *openapiPath)
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
