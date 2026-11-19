package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
)

// exampleCapabilitiesDir 返回能力样例目录。ANOTIFY_EXAMPLES_DIR 供
// scripts/check-examples.mjs 把**变异后的副本**交给同一份校验（默认读仓库里的
// examples/capabilities）。
func exampleCapabilitiesDir() string {
	if dir := strings.TrimSpace(os.Getenv("ANOTIFY_EXAMPLES_DIR")); dir != "" {
		return dir
	}
	return filepath.Join("..", "..", "examples", "capabilities")
}

// TestExampleCapabilities 把 examples/capabilities/*.yaml 真的喂给 Parse。
//
// 样例是给用户抄的：抄不了（写入期就被 400 拒绝）比没有样例更糟。同时要求每个样例
// 都真的带 run.workDir —— 把 workDir 删掉之后样例标题就不再成立，而 YAML 本身仍然
// 「看起来对」，只有断言能拦住这种退化。
func TestExampleCapabilities(t *testing.T) {
	dir := exampleCapabilitiesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读样例目录 %s：%v", dir, err)
	}

	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		seen++
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 %s：%v", path, err)
			}
			a, err := Parse(data)
			if err != nil {
				t.Fatalf("%s 过不了 artifact 校验（用户抄不了）：%v", name, err)
			}
			if a.Spec.Run.WorkDir == "" {
				t.Fatalf("%s 没有 run.workDir：这个目录的样例专门演示「在已有目录里跑」，"+
					"删掉 workDir 就退化成默认目录样例", name)
			}
			// workDir 引用的变量必须真的被入口脚本用到：写了 workDir 但脚本无视它，
			// 样例就在教两套互相矛盾的写法。
			body := a.Spec.Run.Files[a.Spec.Run.Entrypoint]
			for _, m := range workDirRefPattern.FindAllStringSubmatch(a.Spec.Run.WorkDir, -1) {
				kind, ref := m[1], m[2]
				// vars.X 在脚本里就是 $X；params.X 是 ANOTIFY_PARAM_X（$NAME 与 ${NAME} 两种
				// shell 写法都包含这个名字，所以只比名字本身）。
				want := "$" + ref
				if kind == "params" {
					want = protocol.ParamEnvName(ref)
				}
				if !strings.Contains(body, want) {
					t.Fatalf("%s 的 workDir 引用了 %s.%s，但入口脚本里没出现 %s："+
						"样例要展示「目录从哪来、脚本怎么用」，两边必须对得上", name, kind, ref, want)
				}
			}
		})
	}

	if seen < 2 {
		t.Fatalf("%s 下只有 %d 个 .yaml 样例：这个目录存在的意义就是「两种可对照的写法」，"+
			"样例被删光时这里必须失败，不能变成空跑", dir, seen)
	}
}
