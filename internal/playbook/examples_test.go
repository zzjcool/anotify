package playbook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 例包必须真的可渲染：库里的示例被人 copy 出去当模板用，它红比它"看起来对"重要。
func TestExamplePackagesRender(t *testing.T) {
	dirs, err := filepath.Glob("../../examples/playbooks/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("找不到示例包：%v", err)
	}
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "values.demo.json"))
		if err != nil {
			t.Fatalf("%s 缺少 values.demo.json（示例必须能一键渲染）: %v", dir, err)
		}
		var values map[string]any
		if err := json.Unmarshal(raw, &values); err != nil {
			t.Fatalf("%s values.demo.json: %v", dir, err)
		}
		files := map[string]string{}
		root := dir
		_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return err
			}
			switch filepath.Ext(p) {
			case ".yaml", ".json", ".md":
			default:
				return nil
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			rel, _ := filepath.Rel(root, p)
			files[rel] = string(b)
			return nil
		})
		pkg, err := Parse(files)
		if err != nil {
			t.Fatalf("%s Parse: %v", dir, err)
		}
		res, err := Render(pkg, values)
		if err != nil {
			t.Fatalf("%s Render: %v", dir, err)
		}
		for _, c := range res.Capabilities {
			if strings.TrimSpace(c.Spec.Run.Entrypoint) == "" {
				t.Fatalf("%s/%s 没有 entrypoint", dir, c.Metadata.Name)
			}
			body := c.Spec.Run.Files[c.Spec.Run.Entrypoint]
			if !strings.HasPrefix(body, "#!") {
				t.Fatalf("%s/%s 的 %s 不以 #! 开头", dir, c.Metadata.Name, c.Spec.Run.Entrypoint)
			}
			for _, key := range []string{"REPO_DIR", "AGENT_CMD", "AGENT_ARGS", "BASE_BRANCH"} {
				if !strings.Contains(body, "${"+key) {
					continue
				}
				if strings.TrimSpace(c.Spec.Run.Env[key].Value) == "" && c.Spec.Run.Env[key].FieldPath == "" {
					t.Fatalf("%s/%s 的脚本用了 ${%s}，但 run.env 没声明它（脚本会立刻退出）", dir, c.Metadata.Name, key)
				}
			}
			for _, ln := range strings.Split(body, "\n") {
				if strings.TrimSpace(ln) == "env:" {
					t.Fatalf("%s/%s 的脚本里出现了被吞掉的 YAML 键 %q：run.env 缩进错了，整块会变成脚本文本", dir, c.Metadata.Name, "env:")
				}
			}
		}
	}
}
