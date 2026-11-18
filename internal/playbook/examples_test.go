package playbook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/schema"
)

// 示例包与实现一起演进：这里把 examples/playbooks 下的每个包读进来，走仓库自己的
// Parse → Render → Plan（空库快照）。解析、渲染或计划任一步失败，测试即失败 ——
// 示例里的字段是不是服务端真认的那些，不靠 review 记忆。
//
// 这条测试跑的是**真实文件**，不是内联 fixture：README 里贴的命令装的正是它。
func TestExamplePlaybooksInstallable(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "playbooks")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读示例目录 %s：%v", root, err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) { checkExamplePlaybook(t, dir) })
	}
	if seen == 0 {
		t.Fatalf("%s 下一个示例包都没有：示例被删掉时这里必须失败，不能变成空跑", root)
	}
}

// checkExamplePlaybook 校验一个示例包：能解析、能渲染、链是闭合的、空库里能全新建。
func checkExamplePlaybook(t *testing.T, dir string) {
	t.Helper()

	pkg, err := Parse(readExampleFiles(t, dir))
	if err != nil {
		t.Fatalf("Parse：%v", err)
	}
	values := readExampleValues(t, dir)
	rendered, err := Render(pkg, values)
	if err != nil {
		t.Fatalf("Render(values.demo.json)：%v", err)
	}

	// 名字确实被 values 渲染过（模板没失效、也不是写死的常量）。
	if project, ok := values["project"].(string); ok && project != "" {
		for _, a := range rendered.Capabilities {
			if !strings.HasPrefix(a.Metadata.Name, project+"-") {
				t.Errorf("能力 %q 不以 values.project=%q 打头：模板没渲染？", a.Metadata.Name, project)
			}
		}
		for _, s := range rendered.Sources {
			if !strings.HasPrefix(s.Name, project+"-") {
				t.Errorf("源 %q 不以 values.project=%q 打头：模板没渲染？", s.Name, project)
			}
		}
	}

	// 链路闭合：每个 next / targets 都指向本次渲染产物里的名字（不是悬空引用）。
	assertExampleChainClosed(t, rendered)

	// 空库装一次：全部新建、零冲突、无待决引用、无未生效差异。
	plan, err := Plan(pkg, values, EmptyExisting())
	if err != nil {
		t.Fatalf("Plan(空库)：%v", err)
	}
	if len(plan.Conflicts) != 0 {
		t.Errorf("空库里不该有冲突：%+v", plan.Conflicts)
	}
	if len(plan.Notes) != 0 {
		t.Errorf("空库里不该有 notes：%+v", plan.Notes)
	}
	if len(plan.Pending) != 0 || len(plan.Unapplied) != 0 {
		t.Errorf("空库里不该有 pending / unapplied：pending=%v unapplied=%+v", plan.Pending, plan.Unapplied)
	}
	if got, want := len(plan.Created.Capabilities), len(rendered.Capabilities); got != want {
		t.Errorf("created.capabilities 有 %d 项，渲染产物有 %d 个能力：%v", got, want, plan.Created.Capabilities)
	}
	if got, want := len(plan.Created.Sources), len(rendered.Sources); got != want {
		t.Errorf("created.sources 有 %d 项，渲染产物有 %d 个源：%v", got, want, plan.Created.Sources)
	}
	if len(plan.Skipped.Capabilities) != 0 || len(plan.Skipped.Sources) != 0 {
		t.Errorf("空库里不该有 skipped：%+v", plan.Skipped)
	}
	for _, pc := range plan.Capabilities {
		if pc.Action != ActionCreate {
			t.Errorf("能力 %q 的动作是 %q，空库里应当是 %q", pc.Artifact.Metadata.Name, pc.Action, ActionCreate)
		}
		if pc.Artifact.Metadata.Version < 1 {
			t.Errorf("能力 %q 没拿到版本号（%d）：装不进去", pc.Artifact.Metadata.Name, pc.Artifact.Metadata.Version)
		}
	}

	// README 说「project 与 alertUrl 必须给」：这里证明 required 真在生效。
	sch, err := pkg.schemaOf()
	if err != nil {
		t.Fatalf("解析 spec.values：%v", err)
	}
	if req := schema.Required(sch); len(req) > 0 {
		partial := deepCopyMap(values)
		delete(partial, req[0])
		if _, err := Render(pkg, partial); !IsCode(err, CodeValuesMissing) {
			t.Errorf("漏给必填 values.%s 时应当报 %s，实际：%v", req[0], CodeValuesMissing, err)
		}
	}
}

// assertExampleChainClosed 检查渲染产物的边：next 与 targets 都落在包内，且至少有一条边。
func assertExampleChainClosed(t *testing.T, rendered *RenderResult) {
	t.Helper()

	names := make(map[string]bool, len(rendered.Capabilities))
	for _, a := range rendered.Capabilities {
		names[a.Metadata.Name] = true
	}
	edges := 0
	for _, a := range rendered.Capabilities {
		for _, ref := range a.Spec.Next {
			if !names[refName(ref)] {
				t.Errorf("能力 %q 的 next 指向 %q，不在本次渲染产物里（悬空引用）", a.Metadata.Name, ref)
			}
			edges++
		}
	}
	for _, s := range rendered.Sources {
		for _, tg := range s.Spec.Targets {
			if !names[refName(tg.Capability)] {
				t.Errorf("源 %q 的 targets 指向 %q，不在本次渲染产物里（悬空引用）", s.Name, tg.Capability)
			}
			edges++
		}
	}
	if edges == 0 {
		t.Errorf("示例没有任何边：源不指向能力、能力也不指向下游，不成链")
	}
}

// readExampleFiles 把一个示例目录读成包体（相对路径 → 正文），与 POST 的 spec.files 同形。
func readExampleFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	files := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("读示例目录 %s：%v", dir, err)
	}
	return files
}

// readExampleValues 读示例目录里的 values.demo.json（README 的安装命令也用它，只有一份）。
func readExampleValues(t *testing.T, dir string) map[string]any {
	t.Helper()

	path := filepath.Join(dir, "values.demo.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s：%v（示例必须带一份可安装的 values）", path, err)
	}
	var values map[string]any
	if err := json.Unmarshal(body, &values); err != nil {
		t.Fatalf("%s 不是 JSON 对象：%v", path, err)
	}
	return values
}
