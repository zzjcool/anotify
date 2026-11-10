package artifact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/protocol"
	"gopkg.in/yaml.v3"
)

// validYAML 是一份最小但完整的合法产物，各用例在它上面做单点破坏。
const validYAML = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review.pr-diff
  version: 3
  displayName: PR 代码审查
spec:
  params:
    type: object
    required: [repo, pr]
    properties:
      repo: {type: string}
      pr: {type: integer}
  next: [fixer, notify@2]
  requires:
    commands: [gh]
    values: [vars.SOURCE_ROOT]
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/bin/bash
        set -euo pipefail
        gh pr diff "$ANOTIFY_PARAM_REPO" "$ANOTIFY_PARAM_PR"
    env:
      SOURCE_ROOT:
        valueFrom:
          fieldRef: {fieldPath: vars.SOURCE_ROOT}
      LOG_LEVEL: info
    workDir: $(vars.SOURCE_ROOT)
  lifecycle:
    idempotent: true
    maxAttempts: 2
    resultTtlSec: 3600
    timeoutSec: 900
  concurrency:
    maxPerNode: 2
`

// minimalYAML 是省略了全部可选字段的最小产物（用来钉默认值）。
const minimalYAML = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: hello
spec:
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/bin/sh\necho hi\n"
`

func mustParse(t *testing.T, doc string) *Artifact {
	t.Helper()
	a, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse 失败：%v", err)
	}
	return a
}

// codeOf 返回错误里第一个问题短码（解析失败时用）。
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("期望错误，实际 nil")
	}
	return Code(err)
}

// yamlToJSON 把 YAML 文档转成等价 JSON（验证两种书写形式等价）。
func yamlToJSON(t *testing.T, doc string) []byte {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	b, err := json.Marshal(normalizeMaps(v))
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return b
}

// ---------- 解析与归一化 ----------

func TestParseValid(t *testing.T) {
	a := mustParse(t, validYAML)

	if a.APIVersion != APIVersion || a.Kind != KindCapability {
		t.Fatalf("信封不对：%s/%s", a.APIVersion, a.Kind)
	}
	if a.Metadata.Name != "review.pr-diff" || a.Metadata.Version != 3 || a.Metadata.DisplayName != "PR 代码审查" {
		t.Fatalf("metadata 不对：%+v", a.Metadata)
	}
	if got, want := strings.Join(a.Spec.Next, ","), "fixer,notify@2"; got != want {
		t.Fatalf("next = %q，期望 %q", got, want)
	}
	if got, want := strings.Join(a.Spec.Requires.Commands, ","), "gh"; got != want {
		t.Fatalf("requires.commands = %q，期望 %q", got, want)
	}
	if got, want := strings.Join(a.Spec.Requires.Values, ","), "vars.SOURCE_ROOT"; got != want {
		t.Fatalf("requires.values = %q，期望 %q", got, want)
	}
	if a.Spec.Run.Entrypoint != "run.sh" || !strings.HasPrefix(a.Spec.Run.Files["run.sh"], "#!") {
		t.Fatalf("run 不对：%+v", a.Spec.Run)
	}
	if a.Spec.Run.WorkDir != "$(vars.SOURCE_ROOT)" {
		t.Fatalf("workDir = %q", a.Spec.Run.WorkDir)
	}
	if v := a.Spec.Run.Env["SOURCE_ROOT"]; v.FieldPath != "vars.SOURCE_ROOT" || v.Value != "" {
		t.Fatalf("valueFrom 没解出来：%+v", v)
	}
	if v := a.Spec.Run.Env["LOG_LEVEL"]; v.Value != "info" || v.FieldPath != "" {
		t.Fatalf("字面量没解出来：%+v", v)
	}
	if !a.Spec.Lifecycle.Idempotent || a.Spec.Lifecycle.EffectiveMaxAttempts() != 2 ||
		a.Spec.Lifecycle.EffectiveResultTTLSec() != 3600 || a.Spec.Lifecycle.EffectiveTimeoutSec() != 900 {
		t.Fatalf("lifecycle 不对：%+v", a.Spec.Lifecycle)
	}
	if a.Spec.Concurrency.EffectiveMaxPerNode() != 2 {
		t.Fatalf("concurrency 不对：%+v", a.Spec.Concurrency)
	}

	// params 归一成 key 有序的紧凑 JSON。
	if strings.Contains(string(a.Spec.Params), "\n") || strings.Contains(string(a.Spec.Params), " ") {
		t.Fatalf("params 没有归一：%s", a.Spec.Params)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(a.Spec.Params, &schema); err != nil {
		t.Fatalf("params 不是 JSON 对象：%v", err)
	}

	d, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if !strings.HasPrefix(d, DigestPrefix) || len(d) != len(DigestPrefix)+64 {
		t.Fatalf("digest 形状不对：%q", d)
	}
}

// TestDefaultsNormalized：省略的可选字段必须补成显式默认值（省略 ≡ 默认）。
func TestDefaultsNormalized(t *testing.T) {
	a := mustParse(t, minimalYAML)

	if string(a.Spec.Params) != "{}" {
		t.Fatalf("省略 params 应归一成 {}，实际 %s", a.Spec.Params)
	}
	if a.Spec.Next == nil || len(a.Spec.Next) != 0 {
		t.Fatalf("省略 next 应归一成 []，实际 %#v", a.Spec.Next)
	}
	if a.Spec.Requires.Commands == nil || a.Spec.Requires.Values == nil {
		t.Fatalf("省略 requires 应归一成空数组，实际 %+v", a.Spec.Requires)
	}
	if a.Spec.Lifecycle.Idempotent {
		t.Fatal("idempotent 默认 false")
	}
	if got := a.Spec.Lifecycle.EffectiveMaxAttempts(); got != DefaultMaxAttempts {
		t.Fatalf("maxAttempts 默认 %d，实际 %d", DefaultMaxAttempts, got)
	}
	if got := a.Spec.Lifecycle.EffectiveResultTTLSec(); got != DefaultResultTTLSec {
		t.Fatalf("resultTtlSec 默认 %d，实际 %d", DefaultResultTTLSec, got)
	}
	if got := a.Spec.Lifecycle.EffectiveTimeoutSec(); got != 0 {
		t.Fatalf("timeoutSec 默认 0，实际 %d", got)
	}
	if got := a.Spec.Concurrency.EffectiveMaxPerNode(); got != DefaultMaxPerNode {
		t.Fatalf("maxPerNode 默认 %d，实际 %d", DefaultMaxPerNode, got)
	}

	// 省略的产物与「显式写默认值」的产物必须同一份内容身份。
	explicit := `
apiVersion: anotify/v1
kind: Capability
metadata: {name: hello}
spec:
  params: {}
  next: []
  requires: {commands: [], values: []}
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/bin/sh\necho hi\n"
  lifecycle: {idempotent: false, maxAttempts: 3, resultTtlSec: 604800, timeoutSec: 0}
  concurrency: {maxPerNode: 1}
`
	b := mustParse(t, explicit)
	fa, err := FoldKey(a)
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	fb, err := FoldKey(b)
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	if fa != fb {
		t.Fatalf("省略默认值与显式默认值的 foldKey 必须相同：%s != %s", fa, fb)
	}
	da, _ := Digest(a)
	db, _ := Digest(b)
	if da != db {
		t.Fatalf("省略默认值与显式默认值的 digest 必须相同：%s != %s", da, db)
	}
}

func TestParseJSONEquivalentToYAML(t *testing.T) {
	fromYAML := mustParse(t, validYAML)
	fromJSON, err := Parse(yamlToJSON(t, validYAML))
	if err != nil {
		t.Fatalf("Parse(JSON): %v", err)
	}
	d1, _ := Digest(fromYAML)
	d2, _ := Digest(fromJSON)
	if d1 != d2 {
		t.Fatalf("同一份产物 YAML / JSON 的 digest 不同：%s != %s", d1, d2)
	}
}

func TestDigestStableAcrossParse(t *testing.T) {
	a := mustParse(t, validYAML)
	d1, _ := Digest(a)
	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	b, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse: %v\n%s", err, out)
	}
	d2, _ := Digest(b)
	if d1 != d2 {
		t.Fatalf("落盘往返后 digest 变了：%s != %s", d1, d2)
	}
	if b.Metadata.DisplayName != "PR 代码审查" {
		t.Fatal("落盘往返丢了 displayName")
	}
}

// ---------- 两级内容身份（capability.md §3） ----------

// TestFoldKeyExcludesVersionNextDisplayName：foldKey 只看执行正文。
func TestFoldKeyExcludesVersionNextDisplayName(t *testing.T) {
	base := mustParse(t, strings.Replace(minimalYAML, "  name: hello", "  name: hello\n  version: 1", 1))
	same := mustParse(t, strings.Replace(minimalYAML, "  name: hello",
		"  name: hello\n  version: 7\n  displayName: 显示名", 1))
	same.Spec.Next = []string{"fixer", "notify@2"}

	f1, err := FoldKey(base)
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	f2, err := FoldKey(same)
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	if f1 != f2 {
		t.Fatalf("version / next / displayName 不该改变 foldKey：\n%s\n%s", f1, f2)
	}

	// digest 不含 next / displayName，但含 version。
	d1, err := Digest(base)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	d2, err := Digest(same)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d1 == d2 {
		t.Fatal("显式 version 必须改变 digest")
	}
	if ok, _ := SameFold(base, same); !ok {
		t.Fatal("SameFold 应当为 true")
	}
	if ok, _ := SameContent(base, same); ok {
		t.Fatal("version 不同 → SameContent 必须为 false")
	}
}

// TestDigestIgnoresNextAndDisplayName：改 next 不产生新版本；改 run 一定产生。
func TestDigestIgnoresNextAndDisplayName(t *testing.T) {
	a := mustParse(t, validYAML)
	b := mustParse(t, validYAML)
	b.Metadata.DisplayName = "换个名字"
	b.Spec.Next = []string{"other"}

	d1, _ := Digest(a)
	d2, _ := Digest(b)
	if d1 != d2 {
		t.Fatalf("next / displayName 不该进 digest：%s != %s", d1, d2)
	}
	if ok, err := SameContent(a, b); err != nil || !ok {
		t.Fatalf("同 next 外无差异 → SameContent 应为 true（err=%v）", err)
	}

	c := mustParse(t, validYAML)
	c.Spec.Run.Files["run.sh"] += "# 改一行\n"
	d3, _ := Digest(c)
	if d3 == d1 {
		t.Fatal("改 run.files 必须产生新 digest")
	}

	e := mustParse(t, validYAML)
	e.Spec.Requires.Commands = []string{"git"}
	d4, _ := Digest(e)
	if d4 == d1 {
		t.Fatal("改 requires 必须产生新 digest")
	}

	f := mustParse(t, validYAML)
	f.Spec.Lifecycle.TimeoutSec = 901
	d5, _ := Digest(f)
	if d5 == d1 {
		t.Fatal("改 lifecycle 必须产生新 digest")
	}

	g := mustParse(t, validYAML)
	g.Spec.Concurrency.MaxPerNode = intPtr(3)
	d6, _ := Digest(g)
	if d6 == d1 {
		t.Fatal("改 concurrency 必须产生新 digest")
	}
}

// TestExplicitResultTTLZeroDiffersFromOmitted：显式 0（永不清理）与省略（7 天）不是同一份内容。
func TestExplicitResultTTLZeroDiffersFromOmitted(t *testing.T) {
	base := mustParse(t, strings.Replace(minimalYAML, "  name: hello", "  name: hello\n  version: 1", 1))
	zero := mustParse(t, strings.Replace(minimalYAML, "  name: hello",
		"  name: hello\n  version: 1", 1)+"  lifecycle: {resultTtlSec: 0}\n")
	if base.Spec.Lifecycle.EffectiveResultTTLSec() != DefaultResultTTLSec {
		t.Fatal("省略 resultTtlSec 应当是默认保留期")
	}
	if zero.Spec.Lifecycle.EffectiveResultTTLSec() != 0 {
		t.Fatal("显式 resultTtlSec: 0 应当是永不清理")
	}
	d1, _ := Digest(base)
	d2, _ := Digest(zero)
	if d1 == d2 {
		t.Fatal("显式 0 与省略不是同一份内容，digest 必须不同")
	}
}

// TestDigestRequiresVersion：version 未确定时不算 digest（version=0 是「还没分配」）。
func TestDigestRequiresVersion(t *testing.T) {
	a := mustParse(t, minimalYAML)
	if _, err := Digest(a); err == nil {
		t.Fatal("version 没确定时 Digest 必须报错")
	}
	if _, err := FoldKey(a); err != nil {
		t.Fatalf("FoldKey 不需要 version：%v", err)
	}

	b := mustParse(t, minimalYAML)
	b.Metadata.Version = 1
	if _, err := Digest(b); err != nil {
		t.Fatalf("version=1 时 Digest: %v", err)
	}
}

// TestSameFoldSameVersionSameDigest：同 fold + 同 version ⇒ 同 digest。
func TestSameFoldSameVersionSameDigest(t *testing.T) {
	a := mustParse(t, validYAML)
	b := mustParse(t, validYAML)
	b.Metadata.DisplayName = "另一个展示名"
	b.Spec.Next = []string{}
	if ok, _ := SameFold(a, b); !ok {
		t.Fatal("同 fold 判定错了")
	}
	if ok, _ := SameContent(a, b); !ok {
		t.Fatal("同 fold 同 version 必须同 digest")
	}
}

func TestDigestKeyOrderAndWhitespaceIndependent(t *testing.T) {
	a := mustParse(t, validYAML)
	reordered := `
spec:
  concurrency: {maxPerNode: 2}
  lifecycle: {timeoutSec: 900, resultTtlSec: 3600, maxAttempts: 2, idempotent: true}
  run:
    workDir: $(vars.SOURCE_ROOT)
    env:
      LOG_LEVEL: "info"
      SOURCE_ROOT: {valueFrom: {fieldRef: {fieldPath: vars.SOURCE_ROOT}}}
    files:
      run.sh: "#!/bin/bash\nset -euo pipefail\ngh pr diff \"$ANOTIFY_PARAM_REPO\" \"$ANOTIFY_PARAM_PR\"\n"
    entrypoint: run.sh
  requires: {values: [vars.SOURCE_ROOT], commands: [gh]}
  next: [fixer, notify@2]
  params: {properties: {pr: {type: integer}, repo: {type: string}}, required: [repo, pr], type: object}
kind: Capability
apiVersion: anotify/v1
metadata: {displayName: PR 代码审查, version: 3, name: review.pr-diff}
`
	b := mustParse(t, reordered)
	d1, _ := Digest(a)
	d2, _ := Digest(b)
	if d1 != d2 {
		t.Fatalf("key 顺序 / 空白不该影响 digest：%s != %s", d1, d2)
	}
}

// ---------- v3 结构必须消失 ----------

// TestSpecTopLevelClosed：spec 只接受 params/next/run/requires/lifecycle/concurrency。
func TestSpecTopLevelClosed(t *testing.T) {
	for _, field := range []string{
		"  outputs: [clean]\n",
		"  sticky: worktree\n",
		"  handler: noop\n",
	} {
		doc := minimalYAML + field
		_, err := Parse([]byte(doc))
		if got := codeOf(t, err); got != CodeUnknownField {
			t.Fatalf("%q 应当被拒（unknown_field），实际 %q：%v", strings.TrimSpace(field), got, err)
		}
	}
	// v3 把 timeoutSec 放在 run 里；v4 它属于 lifecycle。
	doc := strings.Replace(minimalYAML, "    entrypoint: run.sh", "    entrypoint: run.sh\n    timeoutSec: 9", 1)
	if _, err := Parse([]byte(doc)); err == nil {
		t.Fatal("run.timeoutSec 是 v3 形状，必须被拒")
	}
	// 顶层未知字段同样被拒。
	if _, err := Parse([]byte(strings.Replace(minimalYAML, "spec:", "status:\n  ready: true\nspec:", 1))); codeOf(t, err) != CodeUnknownField {
		// status 在 API 侧是只读派生；artifact 导入前由 CLI 剥离，这里出现即未知字段。
		t.Fatalf("顶层 status 必须被拒，实际 %v", err)
	}
}

// TestRequiresRejectsV3ArrayShape：v3 的 requires: [gh] 不是 v4 形状。
func TestRequiresRejectsV3ArrayShape(t *testing.T) {
	doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n  requires: [gh]\n", 1)
	_, err := Parse([]byte(doc))
	if got := codeOf(t, err); got != CodeRequiresInvalid {
		t.Fatalf("requires 数组形状必须报 requires_invalid，实际 %q：%v", got, err)
	}
}

// ---------- 逐字段校验 ----------

func TestRequiresValidation(t *testing.T) {
	tooMany := make([]string, MaxRequires+1)
	for i := range tooMany {
		tooMany[i] = "cmd" + strings.Repeat("x", i)
	}
	cases := []struct {
		name  string
		field string
		code  string
	}{
		{"未知子键", "  requires: {commands: [gh], paths: [/x]}\n", CodeRequiresInvalid},
		{"命令太多", "  requires: {commands: [" + strings.Join(tooMany, ",") + "]}\n", CodeRequiresInvalid},
		{"命令带反斜杠", "  requires: {commands: ['a\\b']}\n", CodeRequiresInvalid},
		{"值不是 vars 形状", "  requires: {values: [SOURCE_ROOT]}\n", CodeRequiresInvalid},
		{"值是小写", "  requires: {values: [vars.source]}\n", CodeRequiresInvalid},
		{"值超上限", "  requires: {values: [" + strings.Join(prefixAll("vars.V"), ",") + "]}\n", CodeRequiresInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n"+c.field, 1)
			_, err := Parse([]byte(doc))
			if got := codeOf(t, err); got != c.code {
				t.Fatalf("%s：期望 %q，实际 %q（%v）", c.name, c.code, got, err)
			}
		})
	}

	// 合法的 requires 能过。
	ok := strings.Replace(minimalYAML, "spec:\n",
		"spec:\n  requires: {commands: [git, jq], values: [vars.SOURCE_ROOT]}\n", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("合法 requires 被拒：%v", err)
	}
}

func prefixAll(prefix string) []string {
	out := make([]string, MaxRequires+1)
	for i := range out {
		out[i] = prefix + strings.Repeat("A", i%3+1) + string(rune('A'+i))
	}
	return out
}

func TestNextValidation(t *testing.T) {
	cases := []struct {
		name string
		next string
		ok   bool
	}{
		{"终端", "  next: []\n", true},
		{"裸名", "  next: [fixer]\n", true},
		{"钉版", "  next: [fixer, notify@2]\n", true},
		{"版本为 0", "  next: [fixer@0]\n", false},
		{"版本不是整数", "  next: [fixer@abc]\n", false},
		{"空元素", "  next: ['']\n", false},
		{"名字大写", "  next: [Fixer]\n", false},
		{"太多", "  next: [" + strings.Join(manyNames(MaxNext+1), ",") + "]\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n"+c.next, 1)
			_, err := Parse([]byte(doc))
			if c.ok {
				if err != nil {
					t.Fatalf("应当合法：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当被拒")
			}
			if got := codeOf(t, err); got != CodeBadRequest {
				t.Fatalf("next 形状错误应当报 %q，实际 %q（%v）", CodeBadRequest, got, err)
			}
		})
	}
}

func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "cap" + strings.Repeat("x", i%4+1) + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	return out
}

func TestEntrypointRules(t *testing.T) {
	cases := map[string]string{
		"缺 entrypoint":   strings.Replace(minimalYAML, "    entrypoint: run.sh\n", "", 1),
		"entrypoint 不存在": strings.Replace(minimalYAML, "    entrypoint: run.sh", "    entrypoint: main.sh", 1),
		"没有 shebang": strings.Replace(minimalYAML,
			`      run.sh: "#!/bin/sh\necho hi\n"`, `      run.sh: "echo hi\n"`, 1),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc))
			if got := codeOf(t, err); got != CodeEntrypointNoShebang {
				t.Fatalf("期望 %q，实际 %q（%v）", CodeEntrypointNoShebang, got, err)
			}
		})
	}
}

func TestFilesValidation(t *testing.T) {
	long := strings.Repeat("a", MaxFilePathLen+1) + ".sh"
	big := "#!" + strings.Repeat("x", MaxFileBytes)
	total := MaxTotalBytes/2 + 1
	cases := []struct {
		name  string
		files string
	}{
		{"没有文件", "    files: {}\n"},
		{"绝对路径", "    files:\n      /etc/passwd: \"#!/bin/sh\\n\"\n"},
		{"上跳", "    files:\n      ../x.sh: \"#!/bin/sh\\n\"\n"},
		{"反斜杠", "    files:\n      dir\\\\x.sh: \"#!/bin/sh\\n\"\n"},
		{"空路径段", "    files:\n      dir//x.sh: \"#!/bin/sh\\n\"\n"},
		{"路径太长", "    files:\n      " + long + ": \"#!/bin/sh\\n\"\n"},
		{"单文件超限", "    files:\n      run.sh: \"" + big + "\"\n"},
		{"合计超限", "    files:\n      a.sh: \"" + strings.Repeat("#", total) + "\"\n      b.sh: \"" + strings.Repeat("#", total) + "\"\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := "apiVersion: anotify/v1\nkind: Capability\nmetadata: {name: hello}\nspec:\n  run:\n    entrypoint: run.sh\n" + c.files
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("应当被拒")
			}
			if got := codeOf(t, err); got != CodeFilesInvalid && got != CodeEntrypointNoShebang {
				t.Fatalf("期望 files_invalid（空 files 之前会先报它），实际 %q（%v）", got, err)
			}
		})
	}

	// 文件数上限。
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Capability\nmetadata: {name: hello}\nspec:\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: \"#!/bin/sh\\n\"\n")
	for i := 0; i < MaxFiles; i++ {
		b.WriteString("      f" + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1) + ".txt: hi\n")
	}
	if _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("文件数超过上限必须被拒")
	}
}

// runDoc 拼一份「run 只有一个 env 块」的文档：env 用例都在它上面做单点破坏。
func runDoc(extra string) string {
	return `apiVersion: anotify/v1
kind: Capability
metadata: {name: hello}
spec:
  requires: {commands: [], values: [vars.SOURCE_ROOT]}
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/bin/sh\necho hi\n"}
` + extra
}

func TestEnvRules(t *testing.T) {
	cases := map[string]struct {
		env  string
		code string
	}{
		"保留前缀":         {"    env: {ANOTIFY_X: y}\n", CodeEnvInvalid},
		"键名不合法":        {"    env: {1bad: y}\n", CodeEnvInvalid},
		"覆盖 PATH":      {"    env: {PATH: /bin}\n", CodeEnvInvalid},
		"HOME 相对路径":    {"    env: {HOME: rel/dir}\n", CodeEnvInvalid},
		"未知 fieldPath": {"    env:\n      X: {valueFrom: {fieldRef: {fieldPath: status.foo}}}\n", CodeUnknownFieldPath},
		"vars 没声明":     {"    env:\n      X: {valueFrom: {fieldRef: {fieldPath: vars.NOPE}}}\n", CodeUndeclaredNeed},
		"env 值是数字":     {"    env: {JOBS: 4}\n", CodeEnvInvalid},
		"env 值是 null":  {"    env: {JOBS: null}\n", CodeEnvInvalid},
		"env valueFrom 空": {
			"    env:\n      X: {valueFrom: {}}\n", CodeEnvInvalid},
		"env 未知形状": {"    env:\n      X: {literal: y}\n", CodeEnvInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(runDoc(c.env)))
			if err == nil {
				t.Fatalf("%s 应当被拒", name)
			}
			if got := codeOf(t, err); got != c.code {
				t.Fatalf("期望 %q，实际 %q（%v）", c.code, got, err)
			}
		})
	}

	// HOME 给绝对路径是允许的（执行前才检查存在性）；字面量与 valueFrom 可混用。
	ok := runDoc("    env: {HOME: /srv/home, LOG_LEVEL: info}\n")
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("合法的 HOME / 字面量被拒：%v", err)
	}
}

func TestWorkDirRules(t *testing.T) {
	// 一个带 env / requires / params 的基准文档，各用例替换 workDir。
	base := `apiVersion: anotify/v1
kind: Capability
metadata: {name: hello}
spec:
  params:
    type: object
    properties:
      project: {type: string}
  requires: {commands: [], values: [vars.SOURCE_ROOT, vars.NOTIFY_BASE]}
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/bin/sh\n"}
    env:
      SOURCE_ROOT: {valueFrom: {fieldRef: {fieldPath: vars.SOURCE_ROOT}}}
      NOTIFY_BASE: {valueFrom: {fieldRef: {fieldPath: vars.NOTIFY_BASE}}}
      CPU: {valueFrom: {fieldRef: {fieldPath: status.capacity.cpu}}}
      HOME: /srv/home
      JOBS: "4"
    workDir: __WORKDIR__
`
	doc := func(workDir string) string {
		return strings.Replace(base, "__WORKDIR__", workDir, 1)
	}
	okCases := []string{
		"",
		"/srv/work",
		"$(vars.SOURCE_ROOT)",
		"/data/$(params.project)/out",
		"$(vars.SOURCE_ROOT)/sub",
		"/srv/$(vars.SOURCE_ROOT)",
	}
	for _, wd := range okCases {
		t.Run("ok:"+wd, func(t *testing.T) {
			if _, err := Parse([]byte(doc(wd))); err != nil {
				t.Fatalf("workDir %q 应当合法：%v", wd, err)
			}
		})
	}

	badCases := map[string]struct {
		workDir string
		code    string
	}{
		"相对路径":     {"rel/dir", CodeWorkDirInvalid},
		"~ 写法":     {"~/work", CodeWorkDirInvalid},
		"上跳":       {"/srv/../etc", CodeWorkDirInvalid},
		"点段":       {"/srv/./work", CodeWorkDirInvalid},
		"连续斜杠":     {"/srv//work", CodeWorkDirInvalid},
		"超过 1024":  {"/" + strings.Repeat("a", MaxWorkDirLen), CodeWorkDirInvalid},
		"shell 变量": {"$HOME/work", CodeWorkDirInvalid},
		"未声明的 env": {"$(vars.NOPE)", CodeUndefinedEnvRef},
		"未声明的参数":   {"$(params.nope)", CodeUndefinedParamRef},
		"标量当目录":    {"$(vars.CPU)", CodeNonPathField},
	}
	for name, c := range badCases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(doc(c.workDir)))
			if err == nil {
				t.Fatalf("workDir %q 应当被拒", c.workDir)
			}
			if got := codeOf(t, err); got != c.code {
				t.Fatalf("期望 %q，实际 %q（%v）", c.code, got, err)
			}
		})
	}

	// NUL 走 Validate：YAML 本身不允许裸控制字符，所以这里直接构造产物。
	a := mustParse(t, doc("/srv/work"))
	a.Spec.Run.WorkDir = "/srv/\x00work"
	if got := Code(Validate(a)); got != CodeWorkDirInvalid {
		t.Fatalf("workDir 含 NUL 必须报 %q，实际 %q", CodeWorkDirInvalid, got)
	}

	// $(vars.X) 里 X 是字面量（不是 fieldRef）是允许的：值只有执行前才知道。
	literal := strings.Replace(base, "    workDir: __WORKDIR__", "    workDir: $(vars.JOBS)", 1)
	if _, err := Parse([]byte(literal)); err != nil {
		t.Fatalf("$(vars.X) 指向字面量 env 应当合法：%v", err)
	}
}

func TestLifecycleValidation(t *testing.T) {
	cases := map[string]string{
		"maxAttempts 为 0": "  lifecycle: {maxAttempts: 0}\n",
		"maxAttempts 负数":  "  lifecycle: {maxAttempts: -1}\n",
		"TTL 负数":          "  lifecycle: {resultTtlSec: -1}\n",
		"timeout 超上限":     "  lifecycle: {timeoutSec: 86401}\n",
		"timeout 负数":      "  lifecycle: {timeoutSec: -1}\n",
	}
	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n"+field, 1)
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("应当被拒")
			}
			if got := codeOf(t, err); got != CodeBadRequest {
				t.Fatalf("期望 %q，实际 %q（%v）", CodeBadRequest, got, err)
			}
		})
	}
	// 边界值合法：maxAttempts=1、resultTtlSec=0、timeoutSec=86400。
	ok := strings.Replace(minimalYAML, "spec:\n",
		"spec:\n  lifecycle: {maxAttempts: 1, resultTtlSec: 0, timeoutSec: 86400}\n", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("边界值应当合法：%v", err)
	}
}

func TestConcurrencyValidation(t *testing.T) {
	for name, field := range map[string]string{
		"0":    "  concurrency: {maxPerNode: 0}\n",
		"负数":   "  concurrency: {maxPerNode: -2}\n",
		"不是整数": "  concurrency: {maxPerNode: 1.5}\n",
		"未知子键": "  concurrency: {maxGlobal: 3}\n",
	} {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(minimalYAML, "spec:\n", "spec:\n"+field, 1)
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("应当被拒")
			}
			if got := codeOf(t, err); got != CodeConcurrencyInvalid {
				t.Fatalf("期望 %q，实际 %q（%v）", CodeConcurrencyInvalid, got, err)
			}
		})
	}
}

// TestParamEnvCollision：两个参数名归一后撞同一个 ANOTIFY_PARAM_* 必须写入期拒。
func TestParamEnvCollision(t *testing.T) {
	doc := strings.Replace(minimalYAML, "spec:\n",
		"spec:\n  params:\n    type: object\n    properties:\n      repo-url: {type: string}\n      repo_url: {type: string}\n", 1)
	_, err := Parse([]byte(doc))
	if got := codeOf(t, err); got != CodeParamEnvCollision {
		t.Fatalf("期望 %q，实际 %q（%v）", CodeParamEnvCollision, got, err)
	}

	// camelCase 与 snake_case 撞名（capability.md §2.3 明确：repoUrl → REPOURL）。
	doc = strings.Replace(minimalYAML, "spec:\n",
		"spec:\n  params:\n    type: object\n    properties:\n      repoUrl: {type: string}\n      repourl: {type: string}\n", 1)
	if _, err := Parse([]byte(doc)); codeOf(t, err) != CodeParamEnvCollision {
		t.Fatalf("camelCase 撞名必须被拒，实际 %v", err)
	}
}

func TestParamEnvName(t *testing.T) {
	cases := map[string]string{
		"sha":      "ANOTIFY_PARAM_SHA",
		"repo-url": "ANOTIFY_PARAM_REPO_URL",
		"repoUrl":  "ANOTIFY_PARAM_REPOURL",
		"a.b c":    "ANOTIFY_PARAM_A_B_C",
	}
	for in, want := range cases {
		if got := protocol.ParamEnvName(in); got != want {
			t.Fatalf("protocol.ParamEnvName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	if _, err := Parse([]byte(":\n\t- bad")); err == nil {
		t.Fatal("坏 YAML 必须报错")
	} else if got := Code(err); got != CodeInvalidYAML {
		t.Fatalf("坏 YAML 短码 = %q", got)
	}
	if _, err := Parse(nil); err == nil {
		t.Fatal("空文档必须报错")
	}
	// 信封错误。
	if _, err := Parse([]byte(strings.Replace(minimalYAML, "kind: Capability", "kind: Source", 1))); codeOf(t, err) != CodeKindMismatch {
		t.Fatalf("kind 不对必须报 kind_mismatch，实际 %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(minimalYAML, "apiVersion: anotify/v1", "apiVersion: anotify/v2", 1))); codeOf(t, err) != CodeBadRequest {
		t.Fatalf("apiVersion 不对必须报 BadRequest，实际 %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(minimalYAML, "name: hello", "name: Hello", 1))); codeOf(t, err) != CodeBadRequest {
		t.Fatalf("非法能力名必须报 BadRequest，实际 %v", err)
	}
}

// TestValidateReportsAllProblems：一次报全部，不只第一条。
func TestValidateReportsAllProblems(t *testing.T) {
	a := mustParse(t, minimalYAML)
	a.Metadata.Name = "Bad Name"
	a.Spec.Run.Entrypoint = "nope.sh"
	a.Spec.Concurrency.MaxPerNode = intPtr(0)
	err := Validate(a)
	if err == nil {
		t.Fatal("应当报告问题")
	}
	if !strings.Contains(err.Error(), "entrypoint") || !strings.Contains(err.Error(), "maxPerNode") {
		t.Fatalf("应当同时报告多个问题：%v", err)
	}
	var problems *Error
	if !errors.As(err, &problems) || len(problems.Problems) < 3 {
		t.Fatalf("应当汇总全部问题，实际 %v", err)
	}
}

func TestProblemCodeAndMessage(t *testing.T) {
	if got := ProblemCode("env_invalid: 键名不合法"); got != "env_invalid" {
		t.Fatalf("ProblemCode = %q", got)
	}
	if got := ProblemMessage("env_invalid: 键名不合法"); got != "键名不合法" {
		t.Fatalf("ProblemMessage = %q", got)
	}
	if got := ProblemCode("no_colon"); got != "no_colon" {
		t.Fatalf("无分隔符时 ProblemCode = %q", got)
	}
	if got := Code(&Error{Problems: []string{"files_invalid: x"}}); got != "files_invalid" {
		t.Fatalf("Code = %q", got)
	}
	if got := Code(nil); got != "invalid_artifact" {
		t.Fatalf("nil 错误时 Code = %q", got)
	}
}

// TestEmptyListsSerializeAsArrays：空列表写成 []，不是 null（空列表纪律）。
func TestEmptyListsSerializeAsArrays(t *testing.T) {
	a := mustParse(t, minimalYAML)
	out, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	for _, want := range []string{`"next":[]`, `"commands":[]`, `"values":[]`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("规范字节里缺 %s：%s", want, out)
		}
	}
	if strings.Contains(string(out), "null") {
		t.Fatalf("规范字节里出现了 null：%s", out)
	}
}

// TestCanonicalJSONKeepsNextButDropsDisplayName：next 是路由事实（要落库），
// displayName 是展示标签（不进规范字节）。
func TestCanonicalJSONKeepsNextButDropsDisplayName(t *testing.T) {
	a := mustParse(t, validYAML)
	out, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if strings.Contains(string(out), "displayName") || strings.Contains(string(out), "PR 代码审查") {
		t.Fatalf("CanonicalJSON 不该带 displayName：%s", out)
	}
	if !strings.Contains(string(out), `"next":["fixer","notify@2"]`) {
		t.Fatalf("CanonicalJSON 必须保留 next：%s", out)
	}
	if !strings.Contains(string(out), `"apiVersion":"anotify/v1"`) || !strings.Contains(string(out), `"kind":"Capability"`) {
		t.Fatalf("CanonicalJSON 必须是可 Parse 的产物：%s", out)
	}
	// 规范字节能被 Parse 回同一 digest。
	b, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(CanonicalJSON): %v", err)
	}
	d1, _ := Digest(a)
	d2, _ := Digest(b)
	if d1 != d2 {
		t.Fatalf("规范字节往返 digest 变了：%s != %s", d1, d2)
	}
}

func TestDecode(t *testing.T) {
	a, digest, err := Decode([]byte(validYAML))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want, _ := Digest(a)
	if digest != want {
		t.Fatalf("Decode 返回的 digest 不对：%s != %s", digest, want)
	}
}

// TestNormalizeDoesNotMutate：Normalize 不改调用方的对象（store 会复用传入的产物）。
func TestNormalizeDoesNotMutate(t *testing.T) {
	a := mustParse(t, validYAML)
	a.Spec.Lifecycle.MaxAttempts = nil
	a.Spec.Concurrency.MaxPerNode = nil
	a.Spec.Next = nil
	out, err := Normalize(a)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if a.Spec.Lifecycle.MaxAttempts != nil || a.Spec.Concurrency.MaxPerNode != nil || a.Spec.Next != nil {
		t.Fatal("Normalize 改了调用方")
	}
	if out.Spec.Lifecycle.MaxAttempts == nil || out.Spec.Concurrency.MaxPerNode == nil {
		t.Fatal("Normalize 没补默认值")
	}
}
