package yamldoc_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/yamldoc"
)

// 下面这组类型**故意**与真实 DTO 同形（artifact.Spec / srcSpecDoc / objectMeta），
// 但不 import 它们：本包是叶子包，测试也不能引入 internal 依赖——
// 否则「只依赖 yaml.v3 + 标准库」这条红线会在测试里先破掉。
type testEnvValue struct{ Raw string }

func (v *testEnvValue) UnmarshalJSON(b []byte) error {
	v.Raw = string(b)
	return nil
}

type testRun struct {
	Entrypoint string                  `json:"entrypoint"`
	Files      map[string]string       `json:"files"`
	Env        map[string]testEnvValue `json:"env,omitempty"`
	WorkDir    string                  `json:"workDir,omitempty"`
}

type testTarget struct {
	Capability string          `json:"capability"`
	Params     json.RawMessage `json:"params,omitempty"`
}

type testSpec struct {
	Params  json.RawMessage `json:"params,omitempty"`
	Next    []string        `json:"next,omitempty"`
	Run     testRun         `json:"run"`
	Targets *[]testTarget   `json:"targets,omitempty"`
	Enabled bool            `json:"enabled,omitempty"`
	Count   int64           `json:"count,omitempty"`
	Ratio   float64         `json:"ratio,omitempty"`
	Opaque  any             `json:"opaque,omitempty"`
}

type testMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Version   *int   `json:"version,omitempty"`
}

// testEmbedded / testWithEmbed 覆盖「匿名内嵌按 Go 规则提升」。
type testEmbedded struct {
	DisplayName string `json:"displayName,omitempty"`
}

type testWithEmbed struct {
	testEmbedded
	Kind string `json:"kind,omitempty"`
	Skip string `json:"-"`
}

func testShape() yamldoc.Shape {
	return yamldoc.Shape{
		Root: map[string]reflect.Type{
			"apiVersion": reflect.TypeOf(""),
			"kind":       reflect.TypeOf(""),
			"metadata":   reflect.TypeOf(testMeta{}),
			"spec":       reflect.TypeOf(testSpec{}),
			"status":     nil, // 存在但不检查（下游按 status_not_writable 拒）
		},
		Meta: map[string]reflect.Type{
			"name":      reflect.TypeOf(""),
			"namespace": reflect.TypeOf(""),
			"version":   reflect.TypeOf(0),
		},
		Required: []string{"metadata.name"},
	}
}

// successDoc 是 ① 用的完整文档；下面的行号断言依赖它的行布局（不要随手挪行）。
const successDoc = `apiVersion: anotify/v1
kind: Capability
metadata:
  name: yaml-made
  version: 3
spec:
  count: 1700000000
  ratio: 1.5
  next: [alpha, beta]
  run:
    entrypoint: main.sh
    files:
      main.sh: |
        #!/bin/sh
        echo hi
    env:
      FOO:
        value: bar
      BAR:
        valueFrom:
          fieldPath: vars.x
  targets:
    - capability: alpha@1
      params:
        any: [1, 2, {x: y}]
    - capability: beta
`

type wantProblem struct {
	Reason string
	Path   string
	Line   int
	Has    []string // Message 必须包含的片段
	NotHas []string // Message 必须**不**包含的片段（§1.7：不得回显标量值）
}

type decodeCase struct {
	name  string
	doc   string
	shape yamldoc.Shape // 零值 = testShape()
	want  []wantProblem // 空 = 期望成功
	check func(t *testing.T, res *yamldoc.Result)
}

func TestDecode(t *testing.T) {
	cases := []decodeCase{
		{
			name: "成功：树形 YAML 转成确定的 canonical JSON（数字保真）",
			doc:  successDoc,
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				// 键序按字典序、数组保序、数字按字面量 → 整串可比。
				const want = `{"apiVersion":"anotify/v1","kind":"Capability","metadata":{"name":"yaml-made","version":3},"spec":{"count":1700000000,"next":["alpha","beta"],"ratio":1.5,"run":{"entrypoint":"main.sh","env":{"BAR":{"valueFrom":{"fieldPath":"vars.x"}},"FOO":{"value":"bar"}},"files":{"main.sh":"#!/bin/sh\necho hi\n"}},"targets":[{"capability":"alpha@1","params":{"any":[1,2,{"x":"y"}]}},{"capability":"beta"}]}}`
				if got := string(res.JSON); got != want {
					t.Fatalf("canonical JSON 不符\n got: %s\nwant: %s", got, want)
				}
				// 数字保真：1700000000 不许变成科学计数法（本仓修过这个 bug）。
				for _, bad := range []string{"1.7e+09", "1.7e9", "e+09", "e9"} {
					if strings.Contains(string(res.JSON), bad) {
						t.Fatalf("JSON 出现了 %q：%s", bad, res.JSON)
					}
				}
				// 同一份输入两次解析必须字节相同（确定性：map 键序靠 encoding/json 排序）。
				again, err := yamldoc.Decode([]byte(successDoc), testShape())
				if err != nil {
					t.Fatalf("二次解析失败：%v", err)
				}
				if string(again.JSON) != string(res.JSON) {
					t.Fatalf("两次解析结果不同：%s vs %s", again.JSON, res.JSON)
				}
				// canonical JSON 必须能被下游结构体直接吃下（含 opaque 段）。
				var env struct {
					Spec *testSpec `json:"spec"`
				}
				if err := json.Unmarshal(res.JSON, &env); err != nil {
					t.Fatalf("canonical JSON 解不进 DTO：%v", err)
				}
				if env.Spec == nil || env.Spec.Run.Files["main.sh"] != "#!/bin/sh\necho hi\n" {
					t.Fatalf("files 内容丢了：%#v", env.Spec)
				}
				if env.Spec.Count != 1700000000 {
					t.Fatalf("count = %d", env.Spec.Count)
				}
				// ⑭ LineOf 对带索引的路径正确
				for path, wantLine := range map[string]int{
					"":                                     1, // 根节点行
					"apiVersion":                           1,
					"metadata":                             3,
					"metadata.name":                        4,
					"metadata.version":                     5,
					"spec":                                 6,
					"spec.count":                           7,
					"spec.ratio":                           8,
					"spec.run":                             10,
					"spec.run.entrypoint":                  11,
					"spec.run.files":                       12,
					"spec.run.files.main.sh":               13,
					"spec.run.env.FOO":                     17,
					"spec.run.env.FOO.value":               18,
					"spec.run.env.BAR.valueFrom":           20,
					"spec.run.env.BAR.valueFrom.fieldPath": 21,
					"spec.targets":                         22,
					"spec.targets[0]":                      23,
					"spec.targets[0].capability":           23,
					"spec.targets[0].params.any":           25,
					"spec.targets[1]":                      26,
					"spec.targets[1].capability":           26,
					"spec.nope":                            0,
				} {
					if got := res.LineOf(path); got != wantLine {
						t.Errorf("LineOf(%q) = %d, want %d", path, got, wantLine)
					}
				}
			},
		},
		{
			name: "未知键：断言行号、路径与排序后的允许键列表",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run:
    fils: x
    entrypoint: main.sh
`,
			want: []wantProblem{{
				Reason: "unknown_field",
				Path:   "spec.run.fils",
				Line:   7,
				Has:    []string{"未知字段「fils」", "允许：entrypoint, env, files, workDir"},
			}},
		},
		{
			name: "顶层未知键",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
extra: 1
spec: {}
`,
			want: []wantProblem{{
				Reason: "unknown_field",
				Path:   "extra",
				Line:   5,
				Has:    []string{"未知字段「extra」", "允许：apiVersion, kind, metadata, spec, status"},
			}},
		},
		{
			name: "类型不符：映射位置给了字符串",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run: hello
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				Path:   "spec.run",
				Line:   6,
				Has:    []string{"必须是映射，实际是字符串"},
			}},
		},
		{
			name: "类型不符：字符串位置给了映射",
			doc: `apiVersion:
  x: y
kind: Capability
metadata:
  name: n
spec: {}
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				// 行号取**值节点**的行：块映射从第 2 行（第一个子键）开始。
				Path: "apiVersion",
				Line: 2,
				Has:  []string{"必须是字符串，实际是映射"},
			}},
		},
		{
			name: "类型不符：整数位置给了字符串",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
  version: "3"
spec: {}
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				Path:   "metadata.version",
				Line:   5,
				Has:    []string{"必须是整数，实际是字符串"},
			}},
		},
		{
			name: "类型不符：map 的值必须是字符串",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run:
    files:
      main.sh:
        nested: 1
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				Path:   "spec.run.files.main.sh",
				Line:   9,
				Has:    []string{"必须是字符串，实际是映射"},
			}},
		},
		{
			name: "多个问题一次报全且按文档顺序",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  version: "3"
spec:
  run: hello
`,
			want: []wantProblem{
				{Reason: "value_invalid", Path: "metadata.version", Line: 4, Has: []string{"必须是整数"}},
				{Reason: "value_invalid", Path: "spec.run", Line: 6, Has: []string{"必须是映射"}},
				{Reason: "invalid_yaml", Path: "metadata.name", Line: 3, Has: []string{"必填字段缺失"}},
			},
		},
		{
			// ④ 语法错：坏缩进。行号由 yaml.v3 实际给出（这里是 2，不是 3）。
			name: "语法错：断言行号",
			doc: `spec:
   run:
  entrypoint: x
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   2,
				Has:    []string{"YAML 语法错误"},
			}},
		},
		{
			// ⑤ 重复键
			name: "重复键：断言行号并带首次出现行号",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
  name: m
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   5,
				Has:    []string{"重复的键「name」", "首次出现在第 4 行"},
			}},
		},
		{
			// ⑥ 多文档
			name: "多文档拒绝：行号取第二份起点",
			doc: `apiVersion: anotify/v1
kind: Capability
---
apiVersion: anotify/v1
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   3,
				Has:    []string{"多份 YAML 文档"},
			}},
		},
		{
			// ⑦ 锚点/别名
			name: "锚点拒绝",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
  namespace: &ns default
  displayName: *ns
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   5,
				Has:    []string{"不支持锚点"},
			}},
		},
		{
			name: "合并键拒绝",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run:
    <<: {entrypoint: x}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   7,
				Has:    []string{"不支持合并键「<<」"},
			}},
		},
		{
			// ⑧ 自定义 tag
			name: "自定义 tag 拒绝",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run: !Foo {entrypoint: x}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   6,
				Has:    []string{"不支持的自定义标签「!Foo」"},
			}},
		},
		{
			// ⑨ 非字符串 mapping 键
			name: "非字符串 mapping 键拒绝",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
  1: x
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   5,
				Has:    []string{"映射的键必须是字符串，实际是整数"},
			}},
		},
		{
			// ⑩ 顶层非映射
			name: "顶层非映射拒绝",
			doc: `- apiVersion: anotify/v1
- kind: Capability
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   1,
				Has:    []string{"顶层必须是映射，实际是序列"},
			}},
		},
		{
			// ⑪ 空文档（三种写法）
			name: "空文档：全空白",
			doc:  "",
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   0,
				Has:    []string{"文档为空"},
			}},
		},
		{
			name: "空文档：只有注释",
			doc:  "# 什么都没有\n\n",
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   0,
				Has:    []string{"文档为空"},
			}},
		},
		{
			name: "空文档：null",
			doc:  "null\n",
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Line:   1,
				Has:    []string{"文档为空"},
			}},
		},
		{
			// ⑫ 必填缺失
			name: "必填缺失：行号取父节点（metadata 那一行）",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  namespace: default
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Path:   "metadata.name",
				Line:   3,
				Has:    []string{"必填字段缺失"},
			}},
		},
		{
			name: "必填缺失：metadata 整段都没有",
			doc: `apiVersion: anotify/v1
kind: Capability
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Path:   "metadata.name",
				Line:   1,
				Has:    []string{"必填字段缺失"},
			}},
		},
		{
			name: "必填缺失：metadata 是空节点",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
spec: {}
`,
			want: []wantProblem{{
				Reason: "invalid_yaml",
				Path:   "metadata.name",
				Line:   3,
				Has:    []string{"必填字段缺失"},
			}},
		},
		{
			// ⑬ opaque 子树透传
			name: "opaque 子树：env / params / any 内部任意键不报未知字段",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run:
    entrypoint: main.sh
    env:
      FOO:
        anything: [1, 2]
        nested:
          what: ever
  params:
    schema: {type: object}
    whatever: [true, null]
  opaque:
    free: form
`,
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				for _, want := range []string{
					`"env":{"FOO":{"anything":[1,2],"nested":{"what":"ever"}}}`,
					`"params":{"schema":{"type":"object"},"whatever":[true,null]}`,
					`"opaque":{"free":"form"}`,
				} {
					if !strings.Contains(string(res.JSON), want) {
						t.Fatalf("JSON 缺少 %q：%s", want, res.JSON)
					}
				}
				var env struct {
					Spec *testSpec `json:"spec"`
				}
				if err := json.Unmarshal(res.JSON, &env); err != nil {
					t.Fatalf("opaque 子树解不进 DTO：%v", err)
				}
				if string(env.Spec.Params) != `{"schema":{"type":"object"},"whatever":[true,null]}` {
					t.Fatalf("params 透传变了：%s", env.Spec.Params)
				}
			},
		},
		{
			name: "null 值放行（与 encoding/json 的 null 语义一致）",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
  version: null
spec: null
`,
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				if !strings.Contains(string(res.JSON), `"spec":null`) {
					t.Fatalf("null 没有透传：%s", res.JSON)
				}
				var env struct {
					Spec *testSpec `json:"spec"`
				}
				if err := json.Unmarshal(res.JSON, &env); err != nil {
					t.Fatalf("null 解不进 DTO：%v", err)
				}
			},
		},
		{
			name: "status 存在但不检查（下游按 status_not_writable 拒）",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec: {}
status:
  lastError: "boom"
`,
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				if !strings.Contains(string(res.JSON), `"status":{"lastError":"boom"}`) {
					t.Fatalf("status 没有透传：%s", res.JSON)
				}
			},
		},
		{
			// §1.7 红线：错误消息不得回显标量值
			name: "错误消息不回显标量值",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run: "must-not-echo-me"
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				Path:   "spec.run",
				Line:   6,
				Has:    []string{"必须是映射"},
				NotHas: []string{"must-not-echo-me"},
			}},
		},
		{
			name: "未知键的消息也不回显值",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  run:
    entrypoint: main.sh
    token: must-not-echo-me
`,
			want: []wantProblem{{
				Reason: "unknown_field",
				Path:   "spec.run.token",
				Line:   8,
				Has:    []string{"未知字段「token」"},
				NotHas: []string{"must-not-echo-me"},
			}},
		},
		{
			name: "匿名内嵌字段按 json 规则提升（可识别，不报未知键）",
			doc: `wrapper:
  kind: x
  displayName: d
`,
			shape: yamldoc.Shape{
				Root: map[string]reflect.Type{"wrapper": reflect.TypeOf(testWithEmbed{})},
			},
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				if !strings.Contains(string(res.JSON), `"wrapper":{"displayName":"d","kind":"x"}`) {
					t.Fatalf("内嵌提升字段没有进 JSON：%s", res.JSON)
				}
			},
		},
		{
			name: "json:\"-\" 的字段不参与识别（报未知键）",
			doc: `wrapper:
  kind: x
  skipped: y
`,
			shape: yamldoc.Shape{
				Root: map[string]reflect.Type{"wrapper": reflect.TypeOf(testWithEmbed{})},
			},
			want: []wantProblem{{
				Reason: "unknown_field",
				Path:   "wrapper.skipped",
				Line:   3,
				Has:    []string{"未知字段「skipped」", "允许：displayName, kind"},
			}},
		},
		{
			// 覆盖 walkType 的边角分支：数组 / uint / bool / float32 / 非字符串键 map。
			// 非字符串键 map 只能从「键是字符串」的 YAML 走到——YAML 里的裸数字键
			// 会先被拒绝集挡掉（"映射的键必须是字符串"），所以这里用带引号的键。
			name: "其它 Go 类型分支：数组 / uint / bool / float32 / 非字符串键 map",
			doc: `array: [a, b]
intMap:
  "1": one
u: 7
b: true
f: 1.5
`,
			shape: yamldoc.Shape{
				Root: map[string]reflect.Type{
					"array":  reflect.TypeOf([2]string{}),
					"intMap": reflect.TypeOf(map[int]string{}),
					"u":      reflect.TypeOf(uint32(0)),
					"b":      reflect.TypeOf(false),
					"f":      reflect.TypeOf(float32(0)),
				},
			},
			check: func(t *testing.T, res *yamldoc.Result) {
				t.Helper()
				want := `{"array":["a","b"],"b":true,"f":1.5,"intMap":{"1":"one"},"u":7}`
				if got := string(res.JSON); got != want {
					t.Fatalf("JSON = %s, want %s", got, want)
				}
				var out map[string]json.RawMessage
				if err := json.Unmarshal(res.JSON, &out); err != nil {
					t.Fatalf("解不进通用 map：%v", err)
				}
			},
		},
		{
			name: "同一份文档里数字无法表示成 JSON 时拒绝",
			doc: `apiVersion: anotify/v1
kind: Capability
metadata:
  name: n
spec:
  ratio: .inf
`,
			want: []wantProblem{{
				Reason: "value_invalid",
				Path:   "spec.ratio",
				Line:   6,
				Has:    []string{"无法表示成 JSON 数字"},
				NotHas: []string{".inf", "+Inf"},
			}},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			shape := tc.shape
			if shape.Root == nil {
				shape = testShape()
			}
			res, err := yamldoc.Decode([]byte(tc.doc), shape)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("期望解析成功，实际失败：%v", err)
				}
				if len(res.JSON) == 0 {
					t.Fatal("JSON 为空")
				}
				if tc.check != nil {
					tc.check(t, res)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望失败，实际成功：%s", res.JSON)
			}
			var ye *yamldoc.Error
			if !errors.As(err, &ye) {
				t.Fatalf("期望 *yamldoc.Error，实际 %T：%v", err, err)
			}
			if len(ye.Problems) != len(tc.want) {
				t.Fatalf("问题数 %d != %d：\n%s", len(ye.Problems), len(tc.want), ye.Error())
			}
			for i, want := range tc.want {
				p := ye.Problems[i]
				if p.Reason != want.Reason {
					t.Errorf("problems[%d].Reason = %q, want %q", i, p.Reason, want.Reason)
				}
				if p.Path != want.Path {
					t.Errorf("problems[%d].Path = %q, want %q", i, p.Path, want.Path)
				}
				if p.Line != want.Line {
					t.Errorf("problems[%d].Line = %d, want %d（消息：%s）", i, p.Line, want.Line, p.Message)
				}
				for _, frag := range want.Has {
					if !strings.Contains(p.Message, frag) {
						t.Errorf("problems[%d].Message = %q，缺少 %q", i, p.Message, frag)
					}
				}
				for _, frag := range want.NotHas {
					if strings.Contains(p.Message, frag) {
						t.Errorf("problems[%d].Message = %q，不该包含 %q", i, p.Message, frag)
					}
				}
			}
		})
	}
}

// TestNumberFidelity 单测「数字字面量保真」：int 原文照抄、浮点归一成十进制、
// 永不出科学计数法（1700000000 → 1.7e+09 是本仓真出过的 bug）。
func TestNumberFidelity(t *testing.T) {
	cases := []struct {
		name    string
		field   string // count（int64）或 ratio（float64）
		literal string
		want    string // JSON 里必须出现的 `"field":<want>`
		// notHas：JSON 里**不**允许出现的片段（数字不得被照抄回字面量）。
		notHas []string
		// decode 为真时还断言能解进 DTO 且数值正确（int 用 int64，float 用 float64）。
		decode bool
	}{
		{"十进制大整数原文照抄", "count", "1700000000", `"count":1700000000`, nil, true},
		{"负整数", "count", "-42", `"count":-42`, nil, true},
		{"负零", "count", "-0", `"count":-0`, nil, true},
		{"十六进制转十进制", "count", "0x1f", `"count":31`, nil, true},
		{"下划线分隔转十进制", "count", "1_000", `"count":1000`, nil, true},
		{"加号前缀转十进制", "count", "+5", `"count":5`, nil, true},
		{"显式 !!int 的浮点字面量转十进制", "count", "!!int 1e3", `"count":1000`, nil, true},
		{"超出 int64 的整数保持字面量", "count", "18446744073709551615", `"count":18446744073709551615`, nil, false},
		{"超出 int64 的十六进制转十进制", "count", "0xFFFFFFFFFFFFFFFF", `"count":18446744073709551615`, nil, false},
		// 带前导零的十进制字面量：yaml.v3 把它们当**八进制**（探针实测：
		// 010 → 8、00 → 0、-010 → -8、01067 → 567）。旧实现把原文照抄进 JSON，
		// encoding/json 在编码阶段报 `json: invalid number literal "01067"`，
		// 一份合法 YAML 被 400 拒掉（reviewer 抓到的真 bug）。
		// 期望值 = yaml.v3 的 Decode(&int64) 结果，**不是**「去掉前导零」（那会改值）。
		{"前导零八进制 010 → 8（不是 010、不是 10）", "count", "010", `"count":8`, []string{"010"}, true},
		{"前导零全零 00 → 0", "count", "00", `"count":0`, []string{"00"}, true},
		{"负的前导零八进制 -010 → -8", "count", "-010", `"count":-8`, []string{"-010", "-10"}, true},
		{"多位数前导零八进制 01067 → 567", "count", "01067", `"count":567`, []string{"01067", "1067"}, true},
		{"浮点原文照抄", "ratio", "1.5", `"ratio":1.5`, nil, true},
		{"浮点保留尾零", "ratio", "1.50", `"ratio":1.50`, nil, true},
		{"指数浮点归一到十进制", "ratio", "1e9", `"ratio":1000000000`, nil, true},
		{"带正号指数归一到十进制", "ratio", "1e+09", `"ratio":1000000000`, nil, true},
		{"小数缺整数位", "ratio", ".5", `"ratio":0.5`, nil, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			doc := "apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: n\nspec:\n  " +
				tc.field + ": " + tc.literal + "\n"
			res, err := yamldoc.Decode([]byte(doc), testShape())
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			got := string(res.JSON)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("JSON 缺少 %q：%s", tc.want, got)
			}
			if strings.Contains(got, "e+") || strings.Contains(got, "E+") {
				t.Fatalf("JSON 出现科学计数法：%s", got)
			}
			// 字面量不得被原样抄回（前导零 / 非十进制写法的回归护栏）。
			for _, bad := range tc.notHas {
				if strings.Contains(got, bad) {
					t.Fatalf("JSON 原样照抄了 %q：%s", bad, got)
				}
			}
			// 定位：所有用例的字面量都落在第 6 行，LineOf 必须查得到。
			if line := res.LineOf("spec." + tc.field); line != 6 {
				t.Fatalf("LineOf(spec.%s) = %d, want 6（%s）", tc.field, line, got)
			}
			if !tc.decode {
				return
			}
			var env struct {
				Spec *testSpec `json:"spec"`
			}
			if err := json.Unmarshal(res.JSON, &env); err != nil {
				t.Fatalf("解不进 DTO：%v（%s）", err, got)
			}
			if env.Spec == nil {
				t.Fatalf("spec 丢了：%s", got)
			}
			// 值必须等于表里 `want` 的那一串（即 yaml.v3 语义下的真实值）。
			wantNum := tc.want[strings.LastIndexByte(tc.want, ':')+1:]
			switch tc.field {
			case "count":
				wantInt, perr := strconv.ParseInt(wantNum, 10, 64)
				if perr != nil {
					t.Fatalf("表里的 want 不是十进制整数：%q", wantNum)
				}
				if env.Spec.Count != wantInt {
					t.Fatalf("count = %d, want %d（%s）", env.Spec.Count, wantInt, got)
				}
			case "ratio":
				wantFloat, perr := strconv.ParseFloat(wantNum, 64)
				if perr != nil {
					t.Fatalf("表里的 want 不是浮点数：%q", wantNum)
				}
				if env.Spec.Ratio != wantFloat {
					t.Fatalf("ratio = %v, want %v（%s）", env.Spec.Ratio, wantFloat, got)
				}
			}
		})
	}
}

// TestInfProblemHasLocation 钉死「无法表示成 JSON 的数字」的错误形状（契约 §1.6/§1.7）：
// 必须带 Path + Line（客户端才定位得到），消息不得回显字面量。
// 这是「YAML 的 .inf / .nan 在 JSON 里没有对应物」的唯一用户可见出口。
func TestInfProblemHasLocation(t *testing.T) {
	doc := "apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: n\nspec:\n  ratio: .inf\n"
	res, err := yamldoc.Decode([]byte(doc), testShape())
	if err == nil {
		t.Fatalf("期望失败，实际成功：%s", res.JSON)
	}
	var ye *yamldoc.Error
	if !errors.As(err, &ye) {
		t.Fatalf("期望 *yamldoc.Error，实际 %T：%v", err, err)
	}
	if len(ye.Problems) != 1 {
		t.Fatalf("问题数 = %d：\n%s", len(ye.Problems), ye.Error())
	}
	p := ye.Problems[0]
	if p.Reason != "value_invalid" {
		t.Fatalf("Reason = %q", p.Reason)
	}
	// 定位必须可用：Path 非空、Line 是字面量所在行。
	if p.Path == "" || p.Path != "spec.ratio" {
		t.Fatalf("Path = %q, want spec.ratio", p.Path)
	}
	if p.Line == 0 || p.Line != 6 {
		t.Fatalf("Line = %d, want 6", p.Line)
	}
	if !strings.Contains(p.Message, "无法表示成 JSON 数字") {
		t.Fatalf("Message = %q", p.Message)
	}
	// §1.7 红线：消息里不得出现标量字面量。
	for _, bad := range []string{"inf", "Inf", "INF", "nan", "NaN", ".inf"} {
		if strings.Contains(p.Message, bad) {
			t.Fatalf("Message 回显了字面量 %q：%q", bad, p.Message)
		}
	}
	// causes 同样带定位（handler 只下发 Causes）。
	causes := ye.Causes()
	if len(causes) != 1 || causes[0].Field != "spec.ratio" {
		t.Fatalf("causes = %#v", causes)
	}
	if !strings.Contains(causes[0].Message, "第 6 行") {
		t.Fatalf("cause.Message = %q，缺行号前缀", causes[0].Message)
	}
}

func TestStructType(t *testing.T) {
	want := reflect.TypeOf(testSpec{})
	for _, v := range []any{testSpec{}, &testSpec{}, &[]testSpec{}} {
		got := yamldoc.StructType(v)
		if got == nil {
			t.Fatalf("StructType(%T) = nil", v)
		}
		if k := got.Kind(); k == reflect.Ptr {
			t.Fatalf("StructType(%T) 还有指针层：%v", v, got)
		}
	}
	if got := yamldoc.StructType(testSpec{}); got != want {
		t.Fatalf("StructType(testSpec{}) = %v, want %v", got, want)
	}
	if got := yamldoc.StructType(&testSpec{}); got != want {
		t.Fatalf("StructType(&testSpec{}) = %v, want %v", got, want)
	}
	if got := yamldoc.StructType(nil); got != nil {
		t.Fatalf("StructType(nil) = %v, want nil", got)
	}
}

func TestProblemAndError(t *testing.T) {
	problems := []yamldoc.Problem{
		{Reason: "invalid_yaml", Path: "metadata.name", Line: 3, Message: "必填字段缺失"},
		{Reason: "unknown_field", Path: "spec.run.fils", Line: 7, Message: "未知字段「fils」（允许：entrypoint, files）"},
		{Reason: "value_invalid", Line: 9, Message: "顶层必须是映射"},
		{Reason: "value_invalid", Path: "spec.run", Message: "必须是映射"},
		{Reason: "value_invalid", Message: "文档为空"},
	}
	wantStrings := []string{
		"第 3 行 metadata.name：必填字段缺失",
		"第 7 行 spec.run.fils：未知字段「fils」（允许：entrypoint, files）",
		"第 9 行：顶层必须是映射",
		"spec.run：必须是映射",
		"文档为空",
	}
	for i, p := range problems {
		if got := p.String(); got != wantStrings[i] {
			t.Errorf("String() = %q, want %q", got, wantStrings[i])
		}
	}
	err := &yamldoc.Error{Problems: problems}
	if got, want := err.Error(), strings.Join(wantStrings, "\n"); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := err.Reason(); got != "invalid_yaml" {
		t.Errorf("Reason() = %q", got)
	}
	causes := err.Causes()
	if len(causes) != len(problems) {
		t.Fatalf("Causes() 数量 %d != %d", len(causes), len(problems))
	}
	// details.causes[] 的形状：Field 是路径，Message 带「第 N 行：」前缀（契约 §1.6）。
	if causes[0] != (yamldoc.Cause{Reason: "invalid_yaml", Field: "metadata.name", Message: "第 3 行：必填字段缺失"}) {
		t.Errorf("Causes()[0] = %#v", causes[0])
	}
	if causes[4].Message != "文档为空" || causes[4].Field != "" {
		t.Errorf("无行号/无路径的 cause = %#v", causes[4])
	}
	if causes[1].Field != "spec.run.fils" || causes[1].Reason != "unknown_field" {
		t.Errorf("Causes()[1] = %#v", causes[1])
	}
	empty := &yamldoc.Error{}
	if got := empty.Reason(); got != "" {
		t.Errorf("空 Error 的 Reason() = %q", got)
	}
	if got := len(empty.Causes()); got != 0 {
		t.Errorf("空 Error 的 Causes() = %d 条", got)
	}
}
