package artifact

import (
	"encoding/json"
	"strings"
	"testing"
)

// validYAML 是一份最小但完整的合法产物，各用例在它上面做单点破坏。
const validYAML = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: review.pr-diff
  version: 3
spec:
  displayName: PR 代码审查
  params:
    type: object
    required: [repo, pr]
    properties:
      repo: {type: string}
      pr: {type: integer}
  outputs: [clean, needs-fix]
  requires: [gh]
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/bin/bash
        set -euo pipefail
        cd "$ANOTIFY_WORKDIR"
        gh pr diff "$ANOTIFY_PARAM_REPO" "$ANOTIFY_PARAM_PR"
    timeoutSec: 900
    env:
      LOG_LEVEL: info
`

// validArtifact 返回一份可安全修改的合法产物。
func validArtifact(t *testing.T) *Artifact {
	t.Helper()
	a, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse(validYAML): %v", err)
	}
	return a
}

func TestParseValid(t *testing.T) {
	a := validArtifact(t)

	if a.Metadata.Name != "review.pr-diff" || a.Metadata.Version != 3 {
		t.Fatalf("metadata = %+v", a.Metadata)
	}
	if a.Spec.Run.Entrypoint != "run.sh" {
		t.Fatalf("entrypoint = %q", a.Spec.Run.Entrypoint)
	}
	if a.Spec.Run.TimeoutSec != 900 {
		t.Fatalf("timeoutSec = %d", a.Spec.Run.TimeoutSec)
	}
	if got := a.Spec.Run.Env["LOG_LEVEL"]; got != "info" {
		t.Fatalf("env LOG_LEVEL = %q", got)
	}
	if len(a.Spec.Outputs) != 2 {
		t.Fatalf("outputs = %v", a.Spec.Outputs)
	}
	// params 必须被归一成紧凑 JSON（参与 digest 计算）
	if strings.Contains(string(a.Spec.Params), "\n") {
		t.Fatalf("params 未归一：%q", string(a.Spec.Params))
	}
}

func TestParseJSONIsEquivalentToYAML(t *testing.T) {
	// JSON 与 YAML 两种写法必须得到同一个 digest —— 否则「同一份产物」会有两个身份。
	a := validArtifact(t)
	canon, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}

	b, err := Parse(canon)
	if err != nil {
		t.Fatalf("Parse(canonical JSON): %v", err)
	}

	da, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest(a): %v", err)
	}
	db, err := Digest(b)
	if err != nil {
		t.Fatalf("Digest(b): %v", err)
	}
	if da != db {
		t.Fatalf("YAML 与 canonical JSON 的 digest 不一致：%s != %s", da, db)
	}
}

func TestDigestIsDeterministic(t *testing.T) {
	a := validArtifact(t)
	d1, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	d2, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("同一份产物两次 digest 不一致：%s != %s", d1, d2)
	}
	if !strings.HasPrefix(d1, DigestPrefix) {
		t.Fatalf("digest 前缀错误：%s", d1)
	}
	if len(d1) != len(DigestPrefix)+64 {
		t.Fatalf("digest 长度错误：%s（%d）", d1, len(d1))
	}
}

// TestDigestIgnoresKeyOrderAndWhitespace 是最重要的一条：digest 只取决于语义。
func TestDigestIgnoresKeyOrderAndWhitespace(t *testing.T) {
	one := `
apiVersion: anotify/v1
kind: Capability
metadata: {name: x, version: 1}
spec:
  outputs: [a]
  run:
    entrypoint: run.sh
    files:
      run.sh: "#!/bin/bash\necho hi\n"
  params: {type: object, properties: {b: {type: string}, a: {type: string}}}
`
	two := `
spec:
  run:
    files: {run.sh: "#!/bin/bash\necho hi\n"}
    entrypoint: run.sh
  params:
    properties: {a: {type: string}, b: {type: string}}
    type: object
  outputs: [a]
metadata: {version: 1, name: x}
kind: Capability
apiVersion: anotify/v1
`
	a, err := Parse([]byte(one))
	if err != nil {
		t.Fatalf("Parse(one): %v", err)
	}
	b, err := Parse([]byte(two))
	if err != nil {
		t.Fatalf("Parse(two): %v", err)
	}
	da, _ := Digest(a)
	db, _ := Digest(b)
	if da != db {
		t.Fatalf("排版/键序不同导致 digest 不同：%s != %s", da, db)
	}
}

func TestDigestChangesWithContent(t *testing.T) {
	base := validArtifact(t)
	d0, _ := Digest(base)

	mutations := map[string]func(a *Artifact){
		"脚本内容":   func(a *Artifact) { a.Spec.Run.Files["run.sh"] += "\necho x\n" },
		"新增文件":   func(a *Artifact) { a.Spec.Run.Files["lib.sh"] = "#!/bin/bash\n" },
		"超时":     func(a *Artifact) { a.Spec.Run.TimeoutSec = 60 },
		"env":    func(a *Artifact) { a.Spec.Run.Env["LOG_LEVEL"] = "debug" },
		"出口":     func(a *Artifact) { a.Spec.Outputs = append(a.Spec.Outputs, "extra") },
		"依赖":     func(a *Artifact) { a.Spec.Requires = append(a.Spec.Requires, "jq") },
		"版本":     func(a *Artifact) { a.Metadata.Version = 4 },
		"params": func(a *Artifact) { a.Spec.Params = json.RawMessage(`{"type":"object","required":["x"]}`) },
		"显示名":    func(a *Artifact) { a.Spec.DisplayName = "别的名字" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a := validArtifact(t)
			mutate(a)
			d, err := Digest(a)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if d == d0 {
				t.Fatalf("内容改了但 digest 没变（%s）", d)
			}
		})
	}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		edit func(a *Artifact)
		want string // 期望出现在错误里的短码
	}{
		{"apiVersion 不支持", func(a *Artifact) { a.APIVersion = "anotify/v2" }, "unsupported_api_version"},
		{"kind 不支持", func(a *Artifact) { a.Kind = "Workflow" }, "unsupported_kind"},
		{"名字大写", func(a *Artifact) { a.Metadata.Name = "Review" }, "invalid_name"},
		{"名字以点开头", func(a *Artifact) { a.Metadata.Name = ".review" }, "invalid_name"},
		{"名字为空", func(a *Artifact) { a.Metadata.Name = "" }, "invalid_name"},
		{"版本为 0", func(a *Artifact) { a.Metadata.Version = 0 }, "invalid_version"},
		{"版本为负", func(a *Artifact) { a.Metadata.Version = -1 }, "invalid_version"},
		{"没有文件", func(a *Artifact) { a.Spec.Run.Files = nil }, "invalid_file_path"},
		{"入口不存在", func(a *Artifact) { a.Spec.Run.Entrypoint = "nope.sh" }, "entrypoint_missing"},
		{"入口为空", func(a *Artifact) { a.Spec.Run.Entrypoint = "" }, "entrypoint_missing"},
		{"入口没有 shebang", func(a *Artifact) {
			a.Spec.Run.Files["run.sh"] = "echo hi\n"
		}, "entrypoint_no_shebang"},
		{"绝对路径", func(a *Artifact) { a.Spec.Run.Files["/etc/passwd"] = "x" }, "invalid_file_path"},
		{"路径穿越", func(a *Artifact) { a.Spec.Run.Files["../evil.sh"] = "x" }, "invalid_file_path"},
		{"路径含空段", func(a *Artifact) { a.Spec.Run.Files["a//b"] = "x" }, "invalid_file_path"},
		{"路径含反斜杠", func(a *Artifact) { a.Spec.Run.Files[`a\b`] = "x" }, "invalid_file_path"},
		{"超时过大", func(a *Artifact) { a.Spec.Run.TimeoutSec = MaxTimeoutSec + 1 }, "invalid_timeout"},
		{"超时为负", func(a *Artifact) { a.Spec.Run.TimeoutSec = -1 }, "invalid_timeout"},
		{"env 名非法", func(a *Artifact) { a.Spec.Run.Env["1BAD"] = "x" }, "reserved_env"},
		{"env 抢占契约变量", func(a *Artifact) { a.Spec.Run.Env["ANOTIFY_PARAM_X"] = "x" }, "reserved_env"},
		{"出口名非法", func(a *Artifact) { a.Spec.Outputs = []string{"has space"} }, "invalid_output"},
		{"出口名重复", func(a *Artifact) { a.Spec.Outputs = []string{"a", "a"} }, "invalid_output"},
		{"出口太多", func(a *Artifact) {
			out := make([]string, 0, MaxOutputs+1)
			for i := 0; i <= MaxOutputs; i++ {
				out = append(out, "o"+strings.Repeat("x", 1)+string(rune('a'+i%26))+string(rune('a'+i/26)))
			}
			a.Spec.Outputs = out
		}, "invalid_output"},
		{"依赖名非法", func(a *Artifact) { a.Spec.Requires = []string{"has space"} }, "invalid_requirement"},
		{"params 不是对象", func(a *Artifact) { a.Spec.Params = json.RawMessage(`[1,2]`) }, "invalid_params_schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact(t)
			tc.edit(a)
			err := Validate(a)
			if err == nil {
				t.Fatalf("期望校验失败（%s），实际通过", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望错误里含 %q，实际：%v", tc.want, err)
			}
			if Code(err) == "" {
				t.Fatalf("Code() 返回空串")
			}
		})
	}
}

// TestValidateReportsAllProblems 钉住「一次报全部」：手写 YAML 时用户要的是全貌。
func TestValidateReportsAllProblems(t *testing.T) {
	a := validArtifact(t)
	a.APIVersion = "anotify/v2"
	a.Metadata.Name = "BAD"
	a.Metadata.Version = 0

	err := Validate(a)
	if err == nil {
		t.Fatal("期望校验失败")
	}
	msg := err.Error()
	for _, want := range []string{"unsupported_api_version", "invalid_name", "invalid_version"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误里缺 %q：%v", want, err)
		}
	}
	if n := strings.Count(msg, ";"); n < 2 {
		t.Fatalf("期望多个问题用 ; 分隔，实际：%q", msg)
	}
}

func TestValidateFileSizeLimits(t *testing.T) {
	t.Run("单文件超限", func(t *testing.T) {
		a := validArtifact(t)
		big := "#!/bin/bash\n" + strings.Repeat("x", MaxFileBytes)
		a.Spec.Run.Files["big.sh"] = big
		if err := Validate(a); err == nil || !strings.Contains(err.Error(), "payload_too_large") {
			t.Fatalf("期望 payload_too_large，实际：%v", err)
		}
	})
	t.Run("合计超限", func(t *testing.T) {
		a := validArtifact(t)
		chunk := strings.Repeat("x", MaxFileBytes-10)
		for i := 0; i < 5; i++ {
			a.Spec.Run.Files["f"+string(rune('a'+i))+".txt"] = chunk
		}
		if err := Validate(a); err == nil || !strings.Contains(err.Error(), "payload_too_large") {
			t.Fatalf("期望 payload_too_large，实际：%v", err)
		}
	})
}

func TestParseRejectsUnknownFields(t *testing.T) {
	bad := strings.Replace(validYAML, "timeoutSec: 900", "timeoutSec: 900\n    tiemoutSec: 60", 1)
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("期望未知字段被拒（拼错的字段名静默失效比解析失败更难查）")
	}
}

func TestParseRejectsBadYAML(t *testing.T) {
	for _, body := range []string{"", "   ", "apiVersion: [", "null"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Fatalf("期望 %q 解析失败", body)
		}
	}
}

func TestMarshalYAMLRoundTrip(t *testing.T) {
	a := validArtifact(t)
	want, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}

	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(MarshalYAML 产物): %v\n%s", err, out)
	}
	got, err := Digest(back)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if got != want {
		t.Fatalf("YAML 往返后 digest 变了：%s != %s", got, want)
	}
}

func TestDecode(t *testing.T) {
	a, digest, err := Decode([]byte(validYAML))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if a.Metadata.Name != "review.pr-diff" {
		t.Fatalf("name = %q", a.Metadata.Name)
	}
	want, _ := Digest(a)
	if digest != want {
		t.Fatalf("Decode 返回的 digest 与 Digest() 不一致")
	}

	if _, _, err := Decode([]byte("apiVersion: nope\n")); err == nil {
		t.Fatal("期望 Decode 非法产物失败")
	}
}

// TestEmptyOutputsSerializesAsArray 钉住 AGENTS.md 的空列表纪律。
func TestEmptyOutputsSerializesAsArray(t *testing.T) {
	a := validArtifact(t)
	a.Spec.Outputs = nil

	b, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !strings.Contains(string(b), `"outputs":[]`) {
		t.Fatalf("空 outputs 必须序列化成 []，实际：%s", b)
	}
	// 且不能污染调用方对象
	if a.Spec.Outputs != nil {
		t.Fatalf("CanonicalJSON 修改了调用方对象")
	}
}

func TestCode(t *testing.T) {
	if got := Code(nil); got != "invalid_artifact" {
		t.Fatalf("Code(nil) = %q", got)
	}
	err := &Error{Problems: []string{"invalid_name: 名字不对"}}
	if got := Code(err); got != "invalid_name" {
		t.Fatalf("Code = %q", got)
	}
}
