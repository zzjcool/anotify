package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTemplatesCount(t *testing.T) {
	ts := Templates()
	if len(ts) != 9 {
		t.Fatalf("期望 9 个内置模板，实际 %d 个", len(ts))
	}
	wantIDs := []string{
		"script.blank", "script.http", "script.git", "script.test", "script.notify",
		"agent.claude", "agent.codex", "agent.pi", "agent.custom",
	}
	got := make(map[string]bool, len(ts))
	for _, tpl := range ts {
		got[tpl.ID] = true
	}
	for _, id := range wantIDs {
		if !got[id] {
			t.Fatalf("缺少模板 %q", id)
		}
	}
}

// TestEveryTemplateRenders 是模板层最重要的回归：
// 每个模板渲染出的产物都必须能过 Parse（否则用户点了就保存不了）。
func TestEveryTemplateRenders(t *testing.T) {
	for _, tpl := range Templates() {
		t.Run(tpl.ID, func(t *testing.T) {
			a, err := Render(tpl.ID, RenderInput{Name: "demo.cap"})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			// 渲染结果必须能重新 Parse（序列化 → 解析闭环）
			out, err := MarshalYAML(a)
			if err != nil {
				t.Fatalf("MarshalYAML: %v", err)
			}
			back, err := Parse(out)
			if err != nil {
				t.Fatalf("Parse: %v\n%s", err, out)
			}
			d1, _ := Digest(a)
			d2, _ := Digest(back)
			if d1 != d2 {
				t.Fatalf("往返后 digest 变了：%s != %s", d1, d2)
			}

			// 入口必须有 shebang 且 exec 可用
			body := a.Spec.Run.Files[a.Spec.Run.Entrypoint]
			if !strings.HasPrefix(body, "#!") {
				t.Fatalf("入口没有 shebang：%q", body)
			}
			// 脚本不得引用未声明的契约变量以外的东西（轻量冒烟）
			if a.Spec.Run.TimeoutSec <= 0 {
				t.Fatalf("模板 %s 没有默认超时", tpl.ID)
			}
		})
	}
}

func TestRenderDefaults(t *testing.T) {
	a, err := Render("script.blank", RenderInput{Name: "my.cap"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if a.Metadata.Version != 1 {
		t.Fatalf("缺省版本应为 1，实际 %d", a.Metadata.Version)
	}
	if a.Metadata.Name != "my.cap" {
		t.Fatalf("name = %q", a.Metadata.Name)
	}
	if a.Spec.DisplayName != "空白脚本" {
		t.Fatalf("displayName 缺省应取模板名，实际 %q", a.Spec.DisplayName)
	}
	if a.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("entrypoint = %q", a.Spec.Run.Entrypoint)
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	_, err := Render("nope", RenderInput{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "unknown_template") {
		t.Fatalf("期望 unknown_template，实际 %v", err)
	}
}

func TestRenderRequiresName(t *testing.T) {
	_, err := Render("script.blank", RenderInput{})
	if err == nil || !strings.Contains(err.Error(), "invalid_name") {
		t.Fatalf("期望 invalid_name，实际 %v", err)
	}
}

// TestRenderScriptMode 覆盖控制台的「脚本模式」：Files 整体覆盖模板。
func TestRenderScriptMode(t *testing.T) {
	a, err := Render("agent.claude", RenderInput{
		Name:    "custom.cap",
		Version: 2,
		Files: map[string]string{
			"run.sh": "#!/bin/bash\nset -euo pipefail\necho custom\n",
		},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := a.Spec.Run.Files["run.sh"]; !strings.Contains(got, "echo custom") {
		t.Fatalf("脚本模式没有覆盖模板正文：%q", got)
	}
	if a.Metadata.Version != 2 {
		t.Fatalf("version = %d", a.Metadata.Version)
	}
	// 覆盖脚本后仍保留模板的 requires（探测依赖仍然有意义）
	if len(a.Spec.Requires) == 0 {
		t.Fatal("脚本模式不应丢掉模板的 requires")
	}
}

func TestRenderScriptModeRejectsRelativeEscape(t *testing.T) {
	_, err := Render("script.blank", RenderInput{
		Name:  "x",
		Files: map[string]string{"../evil": "#!/bin/bash\n"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_file_path") {
		t.Fatalf("期望 invalid_file_path，实际 %v", err)
	}
}

// TestRenderParamsSchemaFromFields 钉住「字段驱动 schema」：
// 表单字段与参数契约不可能漂移。
func TestRenderParamsSchemaFromFields(t *testing.T) {
	a, err := Render("script.http", RenderInput{Name: "call.api"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var schema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(a.Spec.Params, &schema); err != nil {
		t.Fatalf("params 不是合法 JSON：%v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("params.type = %q", schema.Type)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "url" {
		t.Fatalf("required = %v（期望只有 url）", schema.Required)
	}
	if _, ok := schema.Properties["method"]; !ok {
		t.Fatalf("缺少 method 属性：%v", schema.Properties)
	}
	// select 必须带 enum
	var method struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["method"], &method); err != nil {
		t.Fatalf("method 属性解析失败：%v", err)
	}
	if len(method.Enum) != 5 {
		t.Fatalf("method enum = %v", method.Enum)
	}
}

// TestAllFieldKeysAreSnakeCase 是环境变量契约的守卫：
// camelCase 的 key 会变成 PRNUMBER 这种不可猜的变量名。
func TestAllFieldKeysAreSnakeCase(t *testing.T) {
	for _, tpl := range Templates() {
		for _, f := range tpl.Fields {
			if !isSnakeCase(f.Key) {
				t.Fatalf("模板 %s 的字段 %q 不是 snake_case", tpl.ID, f.Key)
			}
			if f.Type != "string" && f.Type != "text" && f.Type != "number" && f.Type != "select" {
				t.Fatalf("模板 %s 的字段 %q 类型 %q 不合法", tpl.ID, f.Key, f.Type)
			}
			if f.Type == "select" && len(f.Options) == 0 {
				t.Fatalf("模板 %s 的 select 字段 %q 没有 options", tpl.ID, f.Key)
			}
		}
	}
}

// TestTemplateScriptsOnlyUseDeclaredParams 防止脚本引用一个表单里不存在的参数。
func TestTemplateScriptsOnlyUseDeclaredParams(t *testing.T) {
	// 允许的契约变量（非参数）
	contract := map[string]bool{
		"ANOTIFY_TASK_ID":    true,
		"ANOTIFY_CAPABILITY": true,
		"ANOTIFY_PARAMS":     true,
		"ANOTIFY_WORKDIR":    true,
		"ANOTIFY_TRACE_ID":   true,
		"ANOTIFY_DIGEST":     true,
	}
	for _, tpl := range Templates() {
		declared := make(map[string]bool, len(tpl.Fields))
		for _, f := range tpl.Fields {
			// 与 cmd/agent 的 paramEnvName 同口径：大写 + 非字母数字换下划线
			declared["ANOTIFY_PARAM_"+envName(f.Key)] = true
		}
		// 脚本里出现的所有 ANOTIFY_* 变量
		for _, ref := range anotifyRefs(tpl.Script) {
			if contract[ref] || declared[ref] {
				continue
			}
			t.Fatalf("模板 %s 的脚本引用了未声明的变量 %s\n脚本：\n%s", tpl.ID, ref, tpl.Script)
		}
	}
}

func envName(key string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// anotifyRefs 抽出脚本文本里出现的 ANOTIFY_ 变量名。
func anotifyRefs(script string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; ; {
		j := strings.Index(script[i:], "ANOTIFY_")
		if j < 0 {
			break
		}
		start := i + j
		end := start
		for end < len(script) {
			c := script[end]
			if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
				end++
				continue
			}
			break
		}
		name := strings.TrimSuffix(script[start:end], "_")
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = end
	}
	return out
}

func TestTemplatesIsCopy(t *testing.T) {
	a := Templates()
	a[0].Fields = append(a[0].Fields, Field{Key: "injected"})
	a[0].Requires = append(a[0].Requires, "injected")

	b := Templates()
	for _, f := range b[0].Fields {
		if f.Key == "injected" {
			t.Fatal("Templates() 返回了共享底层数组，调用方能污染包状态")
		}
	}
	for _, r := range b[0].Requires {
		if r == "injected" {
			t.Fatal("Templates() 的 Requires 被污染")
		}
	}
}

func TestLookupTemplate(t *testing.T) {
	if _, ok := LookupTemplate("agent.claude"); !ok {
		t.Fatal("agent.claude 应该存在")
	}
	if _, ok := LookupTemplate("nope"); ok {
		t.Fatal("nope 不应该存在")
	}
}
