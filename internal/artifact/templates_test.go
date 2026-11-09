package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTemplatesCount(t *testing.T) {
	ts := Templates()
	if len(ts) != 10 {
		t.Fatalf("期望 10 个内置模板，实际 %d 个", len(ts))
	}
	wantIDs := []string{
		"script.blank", "script.http", "script.git", "script.test", "script.notify",
		"agent.tui", "agent.claude", "agent.codex", "agent.pi", "agent.custom",
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

			// 入口必须有 shebang（执行方式是直接 exec）。
			body := a.Spec.Run.Files[a.Spec.Run.Entrypoint]
			if !strings.HasPrefix(body, "#!") {
				t.Fatalf("入口没有 shebang：%q", body)
			}
			// v4 的 timeoutSec 在 lifecycle 里。
			if a.Spec.Lifecycle.EffectiveTimeoutSec() <= 0 {
				t.Fatalf("模板 %s 没有默认超时", tpl.ID)
			}
			// 归一化后默认值齐全（省略 ≡ 显式默认）。
			if a.Spec.Lifecycle.MaxAttempts == nil || a.Spec.Concurrency.MaxPerNode == nil {
				t.Fatalf("模板 %s 渲染结果没有归一化", tpl.ID)
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
	if a.Metadata.DisplayName != "空白脚本" {
		t.Fatalf("displayName 缺省应取模板名，实际 %q", a.Metadata.DisplayName)
	}
	if a.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("entrypoint = %q", a.Spec.Run.Entrypoint)
	}
	if len(a.Spec.Next) != 0 {
		t.Fatalf("模板默认 next 应当是终端能力，实际 %v", a.Spec.Next)
	}
}

func TestRenderUnknownTemplate(t *testing.T) {
	_, err := Render("nope", RenderInput{Name: "x"})
	if err == nil {
		t.Fatal("未知模板必须报错")
	}
	if got := Code(err); got != CodeBadRequest {
		t.Fatalf("期望 %q，实际 %q（%v）", CodeBadRequest, got, err)
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("错误信息里应当有模板 id：%v", err)
	}
}

func TestRenderRequiresName(t *testing.T) {
	_, err := Render("script.blank", RenderInput{})
	if err == nil || Code(err) != CodeBadRequest {
		t.Fatalf("缺 name 必须报 %q，实际 %v", CodeBadRequest, err)
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
	if len(a.Spec.Requires.Commands) == 0 {
		t.Fatal("脚本模式不应丢掉模板的 requires")
	}
}

func TestRenderScriptModeRejectsPathEscape(t *testing.T) {
	_, err := Render("script.blank", RenderInput{
		Name:  "x",
		Files: map[string]string{"../evil": "#!/bin/bash\n"},
	})
	if err == nil || Code(err) != CodeFilesInvalid {
		t.Fatalf("期望 %q，实际 %v", CodeFilesInvalid, err)
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
		Type       string                    `json:"type"`
		Required   []string                  `json:"required"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(a.Spec.Params, &schema); err != nil {
		t.Fatalf("params 不是 JSON：%v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("params.type = %q", schema.Type)
	}
	if got, want := strings.Join(schema.Required, ","), "url"; got != want {
		t.Fatalf("required = %q，期望 %q", got, want)
	}
	if schema.Properties["url"]["type"] != "string" {
		t.Fatalf("url 的类型不对：%v", schema.Properties["url"])
	}
	hooks, ok := schema.Properties["method"]["enum"]
	if !ok {
		t.Fatalf("select 字段应当有 enum：%v", schema.Properties["method"])
	}
	if len(hooks.([]any)) != 5 {
		t.Fatalf("method 的 enum = %v", hooks)
	}
}

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
	// 允许的契约变量（非参数）。v4 只有这三个 + 参数（capability.md §4.0）。
	contract := map[string]bool{
		"ANOTIFY_TASK_ID":       true,
		"ANOTIFY_TASK_TOKEN":    true,
		"ANOTIFY_EMIT_ENDPOINT": true,
	}
	for _, tpl := range Templates() {
		declared := make(map[string]bool, len(tpl.Fields))
		for _, f := range tpl.Fields {
			// 与本包的 paramEnvName 同口径（大写 + 非字母数字换下划线）。
			declared[paramEnvName(f.Key)] = true
		}
		for _, ref := range anotifyRefs(tpl.Script) {
			if contract[ref] || declared[ref] {
				continue
			}
			t.Fatalf("模板 %s 的脚本引用了未声明的变量 %s\n脚本：\n%s", tpl.ID, ref, tpl.Script)
		}
	}
}

// TestTemplateScriptsAreV4 钉住模板正文的口径：v4 的出口只有 emit，没有 port /
// socket / 出口白名单 / 工作目录环境变量。
func TestTemplateScriptsAreV4(t *testing.T) {
	for _, tpl := range Templates() {
		for _, banned := range []string{
			"ANOTIFY_WORKDIR",   // v4 的工作目录就是 cwd
			"ANOTIFY_EMIT_SOCK", // v4 是 HTTP 网关
			"ANOTIFY_EMIT_PORTS",
			"ANOTIFY_CAPABILITY",
			"ANOTIFY_TRACE_ID",
			"--task-file",
			"--port ",
			"anotify-agent", // v4 二进制叫 anode
		} {
			if strings.Contains(tpl.Script, banned) {
				t.Fatalf("模板 %s 的脚本还带着 v3 口径 %q", tpl.ID, banned)
			}
		}
	}
	// script.blank 是「怎么调 emit」的样板：必须教用户写 anode emit。
	blank, _ := LookupTemplate("script.blank")
	if !strings.Contains(blank.Script, "anode emit") {
		t.Fatalf("空白模板应当示范 v4 的 emit 命令：\n%s", blank.Script)
	}
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
	a[0].Next = append(a[0].Next, "injected")

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
	for _, n := range b[0].Next {
		if n == "injected" {
			t.Fatal("Templates() 的 Next 被污染")
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
