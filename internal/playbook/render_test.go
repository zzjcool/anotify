package playbook

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"gopkg.in/yaml.v3"
)

// TestRenderDefaultsAndEffectiveValues 验 §2 的第①步：按 properties 递归补 default，
// 数组元素与 additionalProperties 不填；并验有效 values 不回写调用方。
func TestRenderDefaultsAndEffectiveValues(t *testing.T) {
	b := newBundle().withStandard()
	pkg := b.pkg(t)
	in := map[string]any{"project": "a", "slackToken": "s3cr3t", "nested": map[string]any{}}
	out := mustRender(t, pkg, in)

	if out.Values["env"] != "prod" {
		t.Fatalf("env 默认值没补: %v", out.Values["env"])
	}
	if out.Values["repo"] != "" {
		t.Fatalf("repo 默认值没补: %v", out.Values["repo"])
	}
	if out.Values["replicas"] != float64(2) {
		t.Fatalf("replicas 默认值没补: %#v", out.Values["replicas"])
	}
	nested, ok := out.Values["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested 对象丢了: %#v", out.Values["nested"])
	}
	if nested["cpu"] != float64(1) || nested["mem"] != "512Mi" {
		t.Fatalf("嵌套对象默认值没递归补: %#v", nested)
	}
	if _, ok := out.Values["tags"]; ok {
		t.Fatalf("数组字段没有 default 就不该被造出来: %#v", out.Values["tags"])
	}
	// 纯函数：调用方的 map 不能被子集补默认值污染。
	if _, ok := in["env"]; ok {
		t.Fatalf("Render 改了调用方的 values（加了 env）")
	}
	if _, ok := in["nested"].(map[string]any)["cpu"]; ok {
		t.Fatalf("Render 改了调用方的嵌套 values")
	}
}

// TestRenderValuesErrors 验 §2.3 的 values_* 两个码。
func TestRenderValuesErrors(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)

	t.Run("required_missing", func(t *testing.T) {
		assertCode(t, renderErr(t, pkg, map[string]any{}), CodeValuesMissing)
	})
	t.Run("unknown_key", func(t *testing.T) {
		err := renderErr(t, pkg, map[string]any{"project": "a", "oops": 1})
		assertCode(t, err, CodeValuesInvalid)
		if !strings.Contains(err.Error(), "oops") {
			t.Fatalf("错误消息应该点名未知键: %v", err)
		}
	})
	t.Run("pattern", func(t *testing.T) {
		assertCode(t, renderErr(t, pkg, map[string]any{"project": "A B"}), CodeValuesInvalid)
	})
	t.Run("enum", func(t *testing.T) {
		assertCode(t, renderErr(t, pkg, map[string]any{"project": "a", "env": "qa"}), CodeValuesInvalid)
	})
	t.Run("type", func(t *testing.T) {
		assertCode(t, renderErr(t, pkg, map[string]any{"project": "a", "replicas": "many"}), CodeValuesInvalid)
	})
}

// TestRenderTypeFidelity 验 §1.4 的类型规则：整值恰好一个引用 → 保留原类型；掺常量 → 字符串拼接。
func TestRenderTypeFidelity(t *testing.T) {
	values := `
    type: object
    additionalProperties: false
    required: [project]
    properties:
      project: { type: string }
      n: { type: integer, default: 3 }
      flag: { type: boolean, default: true }
      list: { type: array, items: { type: string } }
`
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: fidelity
spec:
  next: []
  params:
    type: object
    properties:
      n:      { type: integer, default: '{{values.n}}' }
      flag:   { type: boolean, default: '{{values.flag}}' }
      list:   { type: array,   default: '{{values.list}}' }
      text:   { type: string,  default: 'v{{values.n}}x' }
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().setValues(values).cap("capabilities/fidelity.yaml", doc)
	pkg := b.pkg(t)
	out := mustRender(t, pkg, map[string]any{"project": "a", "list": []any{"x", "y"}})
	a := findCap(out.Capabilities, "fidelity")
	if a == nil {
		t.Fatalf("没渲染出 fidelity: %v", capNames(out.Capabilities))
	}
	var params map[string]any
	if err := json.Unmarshal(a.Spec.Params, &params); err != nil {
		t.Fatalf("params 不是 JSON: %v", err)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("params.properties 不是对象: %#v", params)
	}
	for k, v := range props {
		if _, isObj := v.(map[string]any); !isObj {
			t.Fatalf("params.properties.%s 不是对象: %#v", k, v)
		}
	}
	prop := func(name string) map[string]any { return props[name].(map[string]any) }
	if prop("n")["default"] != float64(3) {
		t.Fatalf("整值引用没保住数字类型: %#v", prop("n")["default"])
	}
	if prop("flag")["default"] != true {
		t.Fatalf("整值引用没保住布尔类型: %#v", prop("flag")["default"])
	}
	list, isList := prop("list")["default"].([]any)
	if !isList || len(list) != 2 || list[0] != "x" {
		t.Fatalf("整值引用没保住数组类型: %#v", prop("list")["default"])
	}
	if prop("text")["default"] != "v3x" {
		t.Fatalf("掺常量应该拼字符串: %#v", prop("text")["default"])
	}
}

// TestRenderTypeMismatch 验对象/数组拼进字符串 = render_type_mismatch。
func TestRenderTypeMismatch(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: 'obj-{{values.nested}}'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/bad.yaml", doc)
	pkg := b.pkg(t)
	err := renderErr(t, pkg, map[string]any{"project": "a", "nested": map[string]any{"cpu": 1}})
	assertCode(t, err, CodeRenderTypeMismatch)
	if !strings.Contains(err.Error(), "values.nested") {
		t.Fatalf("错误消息应该点名引用路径: %v", err)
	}
}

// TestRenderNonValuesTemplatesPassThrough 验 §1.4 的定界符隔离：
// 只有 `{{values.路径}}` 被替换，其它 `{{…}}` / `$NAME` / `${…}` / `$(…)` 原样透传。
func TestRenderNonValuesTemplatesPassThrough(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	out := mustRender(t, pkg, baseValues())
	review := findCap(out.Capabilities, "a-review")
	if review == nil {
		t.Fatalf("没渲染出 a-review: %v", capNames(out.Capabilities))
	}
	script := review.Spec.Run.Files["run.sh"]
	for _, want := range []string{`$HOME`, `${VAR}`, `$(date)`, `{{payload.x}}`, `project=a`, `env=prod`} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本里 %q 被弄丢了:\n%s", want, script)
		}
	}
	if strings.Contains(script, "{{values.") {
		t.Fatalf("values 模板没被替换干净:\n%s", script)
	}
	// 源 targets 的 `$(payload…)` 同样原样透传（包层不扫触发期定界符）。
	src := findSource(out.Sources, "a-on-push")
	if src == nil {
		t.Fatalf("没渲染出 a-on-push")
	}
	if len(src.Spec.Targets) != 1 || src.Spec.Targets[0].Capability != "a-review" {
		t.Fatalf("targets 渲染错: %#v", src.Spec.Targets)
	}
	if src.Spec.Targets[0].Params["repo"] != "" {
		t.Fatalf("targets params 渲染错: %#v", src.Spec.Targets[0].Params)
	}
}

// TestRenderMissingValuePath 验模板引用取不到 = values_missing（不静默给空）。
func TestRenderMissingValuePath(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{values.gone}}-review'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	err := renderErr(t, b.pkg(t), baseValues())
	assertCode(t, err, CodeValuesMissing)
	if !strings.Contains(err.Error(), "values.gone") {
		t.Fatalf("消息要点名路径: %v", err)
	}
}

// TestRenderInvalidName 验 §2.3 的 render_name_invalid：三个信息都给出，且**不自动清洗**。
func TestRenderInvalidName(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{values.project}}-Review'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	err := renderErr(t, b.pkg(t), baseValues())
	assertCode(t, err, CodeRenderNameInvalid)
	for _, want := range []string{"{{values.project}}-Review", "a-Review", "pattern"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("render_name_invalid 应给出模板/渲染结果/建议，缺 %q: %v", want, err)
		}
	}
}

// TestRenderEmptyName 验"模板渲染成空串"也是 render_name_invalid（不是静默建一个无名能力）。
func TestRenderEmptyName(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '{{values.repo}}'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	err := renderErr(t, b.pkg(t), baseValues()) // repo 默认是空串
	assertCode(t, err, CodeRenderNameInvalid)
	if !strings.Contains(err.Error(), "{{values.repo}}") {
		t.Fatalf("要指出是哪条模板: %v", err)
	}
}

// TestRenderValuesPathSyntax 钉住"什么算 `{{values.路径}}`"的边界。
//
// 只替换恰好匹配 `{{values.<点号路径>}}`（路径段是标识符或数字下标）的片段；
// 其余写法（带空格、根名不是 values、路径段非法、没有闭合）一律**原样透传、不报错**
// ——§1.4「不检查残留、不报错」，残留由 `render` 预览兜。
func TestRenderValuesPathSyntax(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: paths
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        echo '{{values.a-b}}'
        echo '{{ values.project }}'
        echo '{{values}}'
        echo '{{values.project}}-{{values.env}}'
        echo '{{{values.project}}}'
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	out := mustRender(t, b.pkg(t), baseValues())
	script := findCap(out.Capabilities, "paths").Spec.Run.Files["run.sh"]
	for _, want := range []string{
		`echo '{{values.a-b}}'`,
		`echo '{{ values.project }}'`,
		`echo '{{values}}'`,
		`echo 'a-prod'`,               // 两个引用 + 常量 → 字符串拼接
		`echo '{{{values.project}}}'`, // 定界符不配对：整段原样
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本里缺 %q:\n%s", want, script)
		}
	}
}

// TestRenderDoesNotRenderKeys 验 §1.4 / D5：只渲染值，不渲染键。
func TestRenderDoesNotRenderKeys(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: keys
spec:
  next: []
  params:
    type: object
    properties:
      '{{values.project}}-param': { type: string }
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	out := mustRender(t, b.pkg(t), baseValues())
	a := findCap(out.Capabilities, "keys")
	if a == nil {
		t.Fatalf("没渲染出 keys")
	}
	var params map[string]any
	if err := json.Unmarshal(a.Spec.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	props := params["properties"].(map[string]any)
	if _, ok := props["{{values.project}}-param"]; !ok {
		t.Fatalf("键名不该被渲染: %#v", props)
	}
}

// TestRenderDuplicateNames 验 §3.3：渲染后同名只能出现一次（能力 / 源各一套命名空间）。
func TestRenderDuplicateNames(t *testing.T) {
	t.Run("capability", func(t *testing.T) {
		// 两份文档都渲染成 `a-fixer`（同一个文件内容的两次引用是允许的，同名才不行）。
		b := newBundle().cap("capabilities/a.yaml", fixerDoc).cap("capabilities/b.yaml", fixerDoc)
		assertCode(t, renderErr(t, b.pkg(t), baseValues()), CodeDuplicateCapability)
	})
	t.Run("source", func(t *testing.T) {
		b := newBundle().source("sources/a.yaml", onPushDoc).source("sources/b.yaml", onPushDoc)
		assertCode(t, renderErr(t, b.pkg(t), baseValues()), CodeDuplicateSource)
	})
}

// TestRenderCycle 验 §3.2：包内闭合子图成环 = cycle；包外名字不参与。
func TestRenderCycle(t *testing.T) {
	capDoc := func(name, next string) string {
		return `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: ` + name + `
spec:
  next: [` + next + `]
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	}
	t.Run("direct_cycle", func(t *testing.T) {
		b := newBundle().
			cap("capabilities/a.yaml", capDoc("a-cap", "b-cap")).
			cap("capabilities/b.yaml", capDoc("b-cap", "a-cap"))
		assertCode(t, renderErr(t, b.pkg(t), baseValues()), CodeCycle)
	})
	t.Run("cycle_via_pinned_version", func(t *testing.T) {
		// `a-cap@2` 与 `a-cap` 是同一个名字 → 参与同一个图（边按名字解析）。
		b := newBundle().
			cap("capabilities/a.yaml", capDoc("a-cap", "b-cap@2")).
			cap("capabilities/b.yaml", capDoc("b-cap", "a-cap"))
		assertCode(t, renderErr(t, b.pkg(t), baseValues()), CodeCycle)
	})
	t.Run("outside_name_does_not_participate", func(t *testing.T) {
		b := newBundle().
			cap("capabilities/a.yaml", capDoc("a-cap", "outside")).
			cap("capabilities/b.yaml", capDoc("b-cap", "a-cap"))
		if _, err := Render(b.pkg(t), baseValues()); err != nil {
			t.Fatalf("指向包外的名字不该参与环判定: %v", err)
		}
	})
}

// TestRenderPostValidation 验渲染后走**单建校验**（§2.2 / D6：不设第二套）。
func TestRenderPostValidation(t *testing.T) {
	t.Run("capability_entrypoint", func(t *testing.T) {
		doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: no-shebang
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "echo hi\n"}
`
		b := newBundle().cap("capabilities/a.yaml", doc)
		err := renderErr(t, b.pkg(t), baseValues())
		assertCode(t, err, CodeInvalidPlaybook)
		if !strings.Contains(err.Error(), "entrypoint_no_shebang") {
			t.Fatalf("应该透出单建校验的具体问题: %v", err)
		}
	})
	t.Run("source_kind", func(t *testing.T) {
		src := `
metadata:
  name: bad-kind
spec:
  kind: github
`
		b := newBundle().source("sources/a.yaml", src)
		err := renderErr(t, b.pkg(t), baseValues())
		assertCode(t, err, CodeInvalidPlaybook)
		if !strings.Contains(err.Error(), "invalid source kind") {
			t.Fatalf("应该透出源校验的具体问题: %v", err)
		}
	})
	t.Run("source_unknown_spec_field", func(t *testing.T) {
		src := `
metadata:
  name: s
spec:
  kind: manual
  target: []
`
		b := newBundle().source("sources/a.yaml", src)
		assertCode(t, renderErr(t, b.pkg(t), baseValues()), CodeInvalidPlaybook)
	})
}

// TestRenderStatusStripped 验 §1.3：`status` 在导入时剥离（它是派生物，不进渲染结果）。
func TestRenderStatusStripped(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: with-status
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
status:
  ready: false
`
	b := newBundle().cap("capabilities/a.yaml", doc)
	pkg := b.pkg(t)
	out := mustRender(t, pkg, baseValues())
	if strings.Contains(out.Files["capabilities/a.yaml"], "status:") {
		t.Fatalf("渲染结果不该带 status:\n%s", out.Files["capabilities/a.yaml"])
	}
	// 原始包体保留原文（includeBody 是编辑/导出用的未渲染原文）。
	if !strings.Contains(pkg.Files["capabilities/a.yaml"], "status:") {
		t.Fatalf("包体原文不该被改")
	}
}

// TestRenderDoesNotRenderEntry 验 §1.4：playbook.yaml 自己不渲染（包名/路径不参数化）。
func TestRenderDoesNotRenderEntry(t *testing.T) {
	b := newBundle().withStandard()
	b.slug = "project-chain"
	b.entryOverride = "" // 用默认入口
	pkg := b.pkg(t)
	pkg.DisplayName = "{{values.project}} 包"
	out := mustRender(t, pkg, baseValues())
	if _, ok := out.Files[EntryFile]; ok {
		t.Fatalf("playbook.yaml 不该出现在渲染结果里（它是身份与引用清单）")
	}
	if !strings.Contains(pkg.DisplayName, "{{values.project}}") {
		t.Fatalf("包的展示字段不该被渲染")
	}
}

// TestRenderWriteOnlyStillRenders 验 writeOnly 只影响回执，不影响渲染（§1.2）。
func TestRenderWriteOnlyStillRenders(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: uses-token
spec:
  next: []
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        echo '{{values.slackToken}}'
`
	b := newBundle().cap("capabilities/a.yaml", doc)
	out := mustRender(t, b.pkg(t), map[string]any{"project": "a", "slackToken": "xoxb-secret"})
	a := findCap(out.Capabilities, "uses-token")
	if !strings.Contains(a.Spec.Run.Files["run.sh"], "xoxb-secret") {
		t.Fatalf("writeOnly 字段必须照常参与渲染: %s", a.Spec.Run.Files["run.sh"])
	}
}

// TestValuesDigest 验 §4.5 / D16 的两条约定：canonical 稳定 + 对**打码前**的值算。
func TestValuesDigest(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)

	a := mustRender(t, pkg, map[string]any{"project": "a"})
	b := mustRender(t, pkg, map[string]any{"project": "a"})
	if a.ValuesDigest != b.ValuesDigest {
		t.Fatalf("同一组 values 的 digest 不稳定: %s vs %s", a.ValuesDigest, b.ValuesDigest)
	}
	// 省略默认值 ≡ 显式写默认值（补默认之后比较）。
	explicit := mustRender(t, pkg, map[string]any{"project": "a", "env": "prod", "repo": "", "replicas": 2})
	if explicit.ValuesDigest != a.ValuesDigest {
		t.Fatalf("补默认后的有效 values 应给出同一个 digest: %s vs %s", explicit.ValuesDigest, a.ValuesDigest)
	}
	// 不同 project → 不同 digest（values 就是这次安装的身份）。
	other := mustRender(t, pkg, map[string]any{"project": "b"})
	if other.ValuesDigest == a.ValuesDigest {
		t.Fatalf("不同 values 的 digest 不该相同")
	}
	if !strings.HasPrefix(a.ValuesDigest, DigestPrefix) || len(a.ValuesDigest) != len(DigestPrefix)+64 {
		t.Fatalf("digest 形状不对: %s", a.ValuesDigest)
	}

	// digest 对打码前的值算：打码后的 values 不能给出同一个 digest。
	withSecret := mustRender(t, pkg, map[string]any{"project": "a", "slackToken": "s3cr3t"})
	sch, err := pkg.schemaOf()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	masked := MaskValues(sch, withSecret.Values)
	maskedDigest, err := ValuesDigest(masked)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if maskedDigest == withSecret.ValuesDigest {
		t.Fatalf("digest 应该对打码前的值算（打码后不该相等）")
	}
}

// TestMaskValues 验 writeOnly 打码按 schema 递归（对象 properties + 数组 items）。
func TestMaskValues(t *testing.T) {
	sch := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"token": map[string]any{"type": "string", "writeOnly": true},
			"plain": map[string]any{"type": "string"},
			"nested": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"secret": map[string]any{"type": "string", "writeOnly": true},
					"kept":   map[string]any{"type": "string"},
				},
			},
			"list": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object", "properties": map[string]any{"pin": map[string]any{"writeOnly": true}}},
			},
		},
	}
	values := map[string]any{
		"token":  "s3cr3t",
		"plain":  "ok",
		"nested": map[string]any{"secret": "hunter2", "kept": "v"},
		"list":   []any{map[string]any{"pin": "1234"}},
	}
	masked := MaskValues(sch, values)
	if masked["token"] != MaskedValue {
		t.Fatalf("writeOnly 字段没打码: %#v", masked["token"])
	}
	if masked["plain"] != "ok" {
		t.Fatalf("普通字段被打码了: %#v", masked["plain"])
	}
	nested := masked["nested"].(map[string]any)
	if nested["secret"] != MaskedValue || nested["kept"] != "v" {
		t.Fatalf("嵌套打码错: %#v", nested)
	}
	item := masked["list"].([]any)[0].(map[string]any)
	if item["pin"] != MaskedValue {
		t.Fatalf("数组元素里的 writeOnly 没打码: %#v", item)
	}
	// 打码不能改调用方。
	if values["token"] != "s3cr3t" {
		t.Fatalf("MaskValues 改了调用方")
	}
}

// TestRenderFilesDeterministic 验 render 的 files 输出稳定（同一输入 → 同一字节）。
func TestRenderFilesDeterministic(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	a := mustRender(t, pkg, baseValues())
	b := mustRender(t, pkg, baseValues())
	if len(a.Files) != len(b.Files) {
		t.Fatalf("两次渲染的文件数不同")
	}
	for path, body := range a.Files {
		if b.Files[path] != body {
			t.Fatalf("%s 两次渲染不一致:\n%s\n---\n%s", path, body, b.Files[path])
		}
	}
	if len(a.Files) != 3 {
		t.Fatalf("渲染结果应含 3 个被引用文件，实际 %v", sortedKeys(a.Files))
	}
}

// TestRenderFilesAreValidYAML 验 render 的 `files` 产物本身是合法 YAML（前端/编辑器直接吃它）。
func TestRenderFilesAreValidYAML(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	out := mustRender(t, pkg, baseValues())
	for path, body := range out.Files {
		var tree map[string]any
		if err := yaml.Unmarshal([]byte(body), &tree); err != nil {
			t.Fatalf("%s 渲染后不是合法 YAML: %v\n%s", path, err, body)
		}
		if tree == nil {
			t.Fatalf("%s 渲染后是空文档", path)
		}
	}
}

// TestRenderNamePatternMatchesArtifact 钉住本地正则与 artifact（单建校验的权威）判定一致。
//
// 本地副本只为给出 render_name_invalid 的三个信息；判定不一致会让"包里装的"和"手建的"分叉。
func TestRenderNamePatternMatchesArtifact(t *testing.T) {
	names := []string{
		"a", "a-review", "a.b_c-d", "0a", "A", "a-Review", "-a", "_a", "a b",
		strings.Repeat("a", 63), strings.Repeat("a", 64), "a/../b", "",
	}
	for _, name := range names {
		a := &artifact.Artifact{
			APIVersion: artifact.APIVersion,
			Kind:       artifact.KindCapability,
			Metadata:   artifact.Metadata{Name: name},
			Spec: artifact.Spec{
				Params: json.RawMessage(`{}`),
				Next:   []string{},
				Run: artifact.Run{
					Entrypoint: "run.sh",
					Files:      map[string]string{"run.sh": "#!/usr/bin/env bash\ntrue\n"},
				},
			},
		}
		norm, err := artifact.Normalize(a)
		if err != nil {
			t.Fatalf("归一化失败: %v", err)
		}
		artifactOK := artifact.Validate(norm) == nil
		if localOK := capNamePattern.MatchString(name); localOK != artifactOK {
			t.Fatalf("名字 %q：本地正则=%v，artifact.Validate=%v（两处判定必须一致）",
				name, localOK, artifactOK)
		}
	}
}

// TestRenderSourceEnabledDefaultsTrue 验 source.md §2：`enabled` 省略 = true。
//
// 用 bool 存会把"省略"读成 false，于是包里每个源都被当成停用——这条用例钉住这个坑。
func TestRenderSourceEnabledDefaultsTrue(t *testing.T) {
	src := `
metadata:
  name: quiet
spec:
  kind: manual
  targets: []
`
	b := newBundle().source("sources/quiet.yaml", src)
	pkg := b.pkg(t)
	out := mustRender(t, pkg, baseValues())
	s := findSource(out.Sources, "quiet")
	if s == nil {
		t.Fatalf("没渲染出 quiet")
	}
	if !s.Spec.Enabled {
		t.Fatalf("enabled 省略必须是 true")
	}
	plan, err := Plan(pkg, baseValues(), newFakeExisting())
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if hasNote(plan.Notes, NoteSourceDisabled, "quiet") {
		t.Fatalf("enabled 省略不该报 source_disabled: %+v", plan.Notes)
	}
}

// TestRenderArrayIndexPath 验点号路径的数字段是数组下标（§1.2 / §1.4）。
func TestRenderArrayIndexPath(t *testing.T) {
	values := `
    type: object
    additionalProperties: false
    required: [list]
    properties:
      list: { type: array, items: { type: string } }
`
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: indexed
spec:
  next: []
  params:
    type: object
    properties:
      first: { type: string, default: '{{values.list.0}}' }
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash
true
"}
`
	b := newBundle().setValues(values).cap("capabilities/x.yaml", doc)
	pkg := b.pkg(t)
	out := mustRender(t, pkg, map[string]any{"list": []any{"alpha", "beta"}})
	a := findCap(out.Capabilities, "indexed")
	var params map[string]any
	if err := json.Unmarshal(a.Spec.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	props := params["properties"].(map[string]any)
	if props["first"].(map[string]any)["default"] != "alpha" {
		t.Fatalf("数字段应该当数组下标: %#v", props["first"])
	}
	// 下标越界 = values_missing（不静默给空）。
	docOut := strings.Replace(doc, "{{values.list.0}}", "{{values.list.5}}", 1)
	b2 := newBundle().setValues(values).cap("capabilities/x.yaml", docOut)
	assertCode(t, renderErr(t, b2.pkg(t), map[string]any{"list": []any{"alpha"}}), CodeValuesMissing)
}

// TestRenderEmptyPackage 验只有 playbook.yaml 的包也能渲染（空集合是 `[]` 不是错误）。
func TestRenderEmptyPackage(t *testing.T) {
	pkg := newBundle().pkg(t)
	out := mustRender(t, pkg, baseValues())
	if out.Capabilities == nil || out.Sources == nil || out.Files == nil {
		t.Fatalf("空包的渲染结果不能是 nil: %+v", out)
	}
	if len(out.Capabilities) != 0 || len(out.Sources) != 0 || len(out.Files) != 0 {
		t.Fatalf("空包不该有文档: %+v", out)
	}
}

func TestRenderSourceNameAfterMaterialization(t *testing.T) {
	for _, tc := range []struct{ label, template, name string }{
		{"literal_route", "route", "unused"},
		{"values_route", "{{values.name}}", "route"},
		{"trimmed_route", "{{values.name}}", " \troute\n "},
		{"empty", "{{values.name}}", ""},
		{"whitespace", "{{values.name}}", " \t\n "},
	} {
		t.Run(tc.label, func(t *testing.T) {
			pkg := sourceNameBundle(tc.template).pkg(t)
			out, err := Render(pkg, map[string]any{"name": tc.name})
			assertCode(t, err, CodeRenderNameInvalid)
			want := sourcespec.ErrEmptyName
			if tc.template == "route" || strings.TrimSpace(tc.name) == "route" {
				want = sourcespec.ErrReservedName
			}
			if !errors.Is(err, want) {
				t.Fatalf("render lost shared error identity: %v", err)
			}
			if out != nil {
				t.Fatalf("rejected name returned render output")
			}
			if !strings.Contains(err.Error(), "sources/name.yaml") {
				t.Fatalf("missing source path: %v", err)
			}
		})
	}
}

func TestRenderSourceLegalNamesNoInputRename(t *testing.T) {
	for _, name := range []string{"github", "cron", "http", "manual", "Route", "route-a", "a-route", " 每晚体检 ", "name@v1", strings.Repeat("a", 64)} {
		t.Run(name, func(t *testing.T) {
			pkg := sourceNameBundle("{{values.name}}").pkg(t)
			before := pkg.Files["sources/name.yaml"]
			values := map[string]any{"name": name}
			out := mustRender(t, pkg, values)
			if len(out.Sources) != 1 || out.Sources[0].Name != name || len(out.Sources[0].Spec.Targets) != 0 {
				t.Fatalf("legal name or empty targets changed: %+v", out.Sources)
			}
			var doc sourceDocWire
			tree, err := decodeDoc(out.Files["sources/name.yaml"], "rendered source")
			if err != nil {
				t.Fatal(err)
			}
			doc, err = decodeSourceWire(tree, "rendered source")
			if err != nil || doc.Metadata.Name != name {
				t.Fatalf("rendered document renamed input: %q, %v", doc.Metadata.Name, err)
			}
			if pkg.Files["sources/name.yaml"] != before || values["name"] != name {
				t.Fatal("Render changed source input or values")
			}
		})
	}
}
