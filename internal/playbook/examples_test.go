package playbook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

var machinePathPrefixes = []string{"/home/", "/Users/", "/root/"}

func machinePathIn(s string) string {
	for _, prefix := range machinePathPrefixes {
		if strings.HasPrefix(s, prefix) {
			return prefix
		}
	}
	return ""
}

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
			if strings.Contains(body, "`") {
				t.Fatalf("%s/%s 的脚本里有反引号：prompt 是双引号字符串，反引号会被 shell 当命令替换执行"+
					"（实测 `Fixes #12` 直接报 command not found，整跳 0.0min 失败）", dir, c.Metadata.Name)
			}
			for key, value := range c.Spec.Run.Env {
				if value.IsValueFrom() {
					continue
				}
				if prefix := machinePathIn(value.Value); prefix != "" {
					t.Fatalf("%s/%s 把机器路径 %q 写进了 run.env.%s 的字面值（命中前缀 %q）："+
						"这个包装一次就对所有节点生效，机器路径必须写成 valueFrom 引用节点变量",
						dir, c.Metadata.Name, value.Value, key, prefix)
				}
			}
			if prefix := machinePathIn(c.Spec.Run.WorkDir); prefix != "" {
				t.Fatalf("%s/%s 把机器路径 %q 写成了 run.workDir 字面量（命中前缀 %q）："+
					"workDir 要用 $(vars.X) 引用节点变量", dir, c.Metadata.Name, c.Spec.Run.WorkDir, prefix)
			}
			if err := artifact.Validate(c); err != nil {
				t.Fatalf("%s/%s 过不了 artifact 校验（渲染出来的产物用户装不上）：%v",
					dir, c.Metadata.Name, err)
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
				freeText := []string{"${findings", "${reason", "${note", "${title", "${review_notes", "${line"}
				if strings.Contains(ln, "json_str") {
					continue
				}
				hit := false
				for _, v := range freeText {
					if strings.Contains(ln, v) {
						hit = true
					}
				}
				if strings.Contains(ln, "anode emit") && hit {
					t.Fatalf("%s/%s 把自由文本拼进了 emit 的 JSON（引号/换行会打爆 JSON）：请通过 issue 评论或文件传递", dir, c.Metadata.Name)
				}
				if strings.TrimSpace(ln) == "env:" {
					t.Fatalf("%s/%s 的脚本里出现了被吞掉的 YAML 键 %q：run.env 缩进错了，整块会变成脚本文本", dir, c.Metadata.Name, "env:")
				}
			}
		}
	}
}

// TestRecordPinsReviewMarkerOnPullRequest：review 不走 GitHub review，合并闸只能读 PR 上的
// marker 判断「链上 review 判过未通过」。record 一旦不再钉它，闸就又变成只看 CI
// （2026-11-27：PR #712 CI 双绿 + review 判阻塞，被自动合入）。
func TestRecordPinsReviewMarkerOnPullRequest(t *testing.T) {
	dir := "../../examples/playbooks/issue-fix"
	raw, err := os.ReadFile(filepath.Join(dir, "values.demo.json"))
	if err != nil {
		t.Fatalf("读 values.demo.json: %v", err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("values.demo.json: %v", err)
	}
	files := map[string]string{}
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
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
		rel, _ := filepath.Rel(dir, p)
		files[rel] = string(b)
		return nil
	})
	pkg, err := Parse(files)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	res, err := Render(pkg, values)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var script string
	for _, c := range res.Capabilities {
		if strings.HasSuffix(c.Metadata.Name, "-record") {
			script = c.Spec.Run.Files[c.Spec.Run.Entrypoint]
		}
	}
	if script == "" {
		t.Fatal("渲染后找不到 record 能力")
	}
	for _, want := range []string{"anotify:review-approved", "anotify:review-rejected", "gh pr comment"} {
		if !strings.Contains(script, want) {
			t.Fatalf("record 渲染后必须含 %q（合并闸靠它判断 review 结论）：\n%s", want, script)
		}
	}
}
