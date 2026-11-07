package artifact

import (
	"crypto/sha256"
	"encoding/hex"
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
  displayName: PR 代码审查
spec:
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
		// 工作目录决定能力跑在哪儿（跟入口文件一样是"怎么跑"的一部分），
		// 改了它就必须是另一份产物——否则中枢无法区分"同一版本换了目录"。
		"工作目录": func(a *Artifact) { a.Spec.Run.WorkDir = "/srv/app" },
		// v3 新进规格的字段：它们描述「这个能力是什么」，所以必须参与内容寻址。
		// 少了任何一条，「同一版本换了重试策略 / 粘性键」都会被当成同一份产物。
		"lifecycle": func(a *Artifact) {
			a.Spec.Lifecycle = json.RawMessage(`{"retryable":true,"maxAttempts":2}`)
		},
		"sticky": func(a *Artifact) { a.Spec.Sticky = "worktree" },
		// handler 形式是另一种实现：把 run 清空才合法（见 Validate），
		// 于是它必然算出另一个 digest——「脚本换成内置执行器」当然是另一份产物。
		"实现形式 handler": func(a *Artifact) {
			a.Spec.Run = Run{}
			a.Spec.Handler = HandlerNoop
		},
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

// ---------- Capability Spec v3：displayName 出 digest / 两种实现形式 ----------

// TestDigestIgnoresDisplayName 钉住契约 §3.2 的第一条，也是 v3 要根治的漂移 bug：
// 改展示名**不产生新版本**、也不让任何部署变 stale。
func TestDigestIgnoresDisplayName(t *testing.T) {
	a := validArtifact(t)
	d0, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	for _, name := range []string{"", "别的名字", "  ", strings.Repeat("名", 200)} {
		b := validArtifact(t)
		b.Metadata.DisplayName = name
		d, err := Digest(b)
		if err != nil {
			t.Fatalf("Digest(displayName=%q): %v", name, err)
		}
		if d != d0 {
			t.Fatalf("只改了 displayName（%q）但 digest 变了：%s != %s\n"+
				"（displayName 是展示标签，不进 digest —— 否则 PATCH 改名字会让部署漂移）", name, d, d0)
		}
	}

	// displayName 连**规范字节**都不该进（不只是「不参与 hash」）。
	canon, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if strings.Contains(string(canon), "displayName") {
		t.Fatalf("canonical JSON 里出现了 displayName：%s", canon)
	}

	// 但落盘那一份要保留它：不进 digest ≠ 可以静默丢掉用户的字段。
	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if !strings.Contains(string(out), "displayName: PR 代码审查") {
		t.Fatalf("落盘 YAML 丢了 displayName：\n%s", out)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(落盘 YAML): %v\n%s", err, out)
	}
	if back.Metadata.DisplayName != "PR 代码审查" {
		t.Fatalf("往返后 displayName = %q", back.Metadata.DisplayName)
	}
}

// TestDigestIsNameVersionSpec 把契约 §3.2 的式子逐字钉住：
//
//	digest = "sha256:" + hex(sha256(canonical{metadata.name, metadata.version, spec}))
//
// 写成「另外算一遍」而不是「比一个魔法字符串」：魔术串只证明「digest 稳定」，
// 证明不了「digest 吃的是这三件」——后者才是契约。
func TestDigestIsNameVersionSpec(t *testing.T) {
	a := validArtifact(t)
	got, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	clone, err := canonicalCopy(a)
	if err != nil {
		t.Fatalf("canonicalCopy: %v", err)
	}
	payload, err := json.Marshal(&struct {
		Metadata digestMetadata `json:"metadata"`
		Spec     Spec           `json:"spec"`
	}{
		Metadata: digestMetadata{Name: clone.Metadata.Name, Version: clone.Metadata.Version},
		Spec:     clone.Spec,
	})
	if err != nil {
		t.Fatalf("json.Marshal(式子): %v", err)
	}
	// 形状也钉住：顶层恰好 metadata + spec 两个键，metadata 里恰好 name + version。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		t.Fatalf("json.Unmarshal(式子): %v", err)
	}
	if len(top) != 2 || top["metadata"] == nil || top["spec"] == nil {
		t.Fatalf("digest 的输入必须是 {metadata, spec} 两个键，实际：%s", payload)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(top["metadata"], &meta); err != nil {
		t.Fatalf("json.Unmarshal(metadata): %v", err)
	}
	if len(meta) != 2 || meta["name"] == nil || meta["version"] == nil {
		t.Fatalf("digest 里的 metadata 只能有 name/version，实际：%s", top["metadata"])
	}

	sum := sha256.Sum256(payload)
	if want := DigestPrefix + hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("digest 与契约 §3.2 的式子不一致：\n  实际 %s\n  式子 %s\n  输入 %s",
			got, want, payload)
	}

	// apiVersion / kind 不在式子里（它们是 Validate 钉死的常量，写进摘要不增加判别力）。
	b := validArtifact(t)
	b.Kind = "Whatever"
	if d, _ := Digest(b); d != got {
		t.Fatalf("kind 不参与 digest（§3.2 的式子只有 name/version/spec），但 digest 变了")
	}
	b = validArtifact(t)
	b.APIVersion = "anotify/v0"
	if d, _ := Digest(b); d != got {
		t.Fatalf("apiVersion 不参与 digest（§3.2 的式子只有 name/version/spec），但 digest 变了")
	}

	// 而 name 在式子里（「跨名字共享实现」本轮刻意不做，见契约 §3.2）。
	c := validArtifact(t)
	c.Metadata.Name = "review.other"
	if d, _ := Digest(c); d == got {
		t.Fatal("换了 name 但 digest 没变：digest 必须含 name")
	}
}

// TestLifecycleKeyOrderIndependent：lifecycle 是不透明透传的 RawMessage，
// 但**书写顺序不能变成 digest 的一部分** —— encoding/json 不会替 RawMessage 排 key，
// 所以归一必须在 canonicalCopy 里做（同 params 的道理）。
func TestLifecycleKeyOrderIndependent(t *testing.T) {
	one := validArtifact(t)
	one.Spec.Lifecycle = json.RawMessage(`{"retryable":true,"maxAttempts":2,"idempotent":true}`)
	two := validArtifact(t)
	two.Spec.Lifecycle = json.RawMessage(`{"maxAttempts":2, "idempotent":true, "retryable":true}`)

	d1, err := Digest(one)
	if err != nil {
		t.Fatalf("Digest(one): %v", err)
	}
	d2, err := Digest(two)
	if err != nil {
		t.Fatalf("Digest(two): %v", err)
	}
	if d1 != d2 {
		t.Fatalf("同一份 lifecycle 只是 key 顺序不同，digest 却不同：%s != %s", d1, d2)
	}

	// 往返（落盘 → Parse）后 digest 不变：否则节点 apply 回来会被判成「本地变体」。
	out, err := MarshalYAML(one)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(MarshalYAML): %v\n%s", err, out)
	}
	if d3, _ := Digest(back); d3 != d1 {
		t.Fatalf("往返后 digest 变了：%s != %s", d3, d1)
	}
}

// TestSpecJSONShapeByForm 钉住「两种形式各自序列化成什么」，两件事：
//
//  1. 脚本形式的字节**一个都没变**（Spec.MarshalJSON 不能把手写方向的产物改掉：
//     那是所有既有 digest 的唯一来源，动了就是静默的全量漂移）；
//  2. handler 形式不写空的 run 块（否则会漏出 `files: null` / `entrypoint: ""`,
//     既违反空列表纪律，又撞上「handler 不得带这些字段」这条规则）。
func TestSpecJSONShapeByForm(t *testing.T) {
	// 镜像类型：与 Spec 的字段/tag 一一对应，但绕开 MarshalJSON（= 默认序列化）。
	type specEcho struct {
		Params    json.RawMessage `json:"params,omitempty"`
		Outputs   []string        `json:"outputs"`
		Requires  []string        `json:"requires,omitempty"`
		Lifecycle json.RawMessage `json:"lifecycle,omitempty"`
		Sticky    string          `json:"sticky,omitempty"`
		Run       Run             `json:"run"`
		Handler   string          `json:"handler,omitempty"`
	}

	t.Run("脚本形式与默认序列化逐字节相同", func(t *testing.T) {
		a := validArtifact(t)
		a.Spec.Sticky = "worktree"
		got, err := json.Marshal(a.Spec)
		if err != nil {
			t.Fatalf("json.Marshal(Spec): %v", err)
		}
		want, err := json.Marshal(specEcho{
			Params:    a.Spec.Params,
			Outputs:   a.Spec.Outputs,
			Requires:  a.Spec.Requires,
			Lifecycle: a.Spec.Lifecycle,
			Sticky:    a.Spec.Sticky,
			Run:       a.Spec.Run,
			Handler:   a.Spec.Handler,
		})
		if err != nil {
			t.Fatalf("json.Marshal(specEcho): %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("脚本形式的 Spec 序列化被动过了：\n  实际 %s\n  期望 %s", got, want)
		}
	})

	t.Run("handler 形式省略空 run", func(t *testing.T) {
		a := validArtifact(t)
		a.Spec.Run = Run{}
		a.Spec.Handler = HandlerNoop
		b, err := json.Marshal(a.Spec)
		if err != nil {
			t.Fatalf("json.Marshal(Spec): %v", err)
		}
		for _, banned := range []string{`"run"`, `"files"`, `"entrypoint"`, `"timeoutSec"`} {
			if strings.Contains(string(b), banned) {
				t.Fatalf("handler 形式的 Spec 里不该出现 %s：%s", banned, b)
			}
		}
		if !strings.Contains(string(b), `"handler":"noop"`) {
			t.Fatalf("handler 形式必须写 handler：%s", b)
		}
	})
}

// KnownHandlers 的白名单值本身是 **wire 契约**：P1-b 的内置播种（notify.phone →
// handler: notify-phone）与迁移回填（零产物能力 → handler: noop）都照它写。
func TestKnownHandlers(t *testing.T) {
	got := KnownHandlers()
	want := []string{HandlerNotifyPhone, HandlerNoop}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("KnownHandlers() = %v, want %v（顺序稳定：错误信息与播种输出要可 diff）", got, want)
	}
	if HandlerNotifyPhone != "notify-phone" || HandlerNoop != "noop" {
		t.Fatalf("白名单常量值变了：%q / %q", HandlerNotifyPhone, HandlerNoop)
	}
	// 返回的是拷贝：调用方改它不能污染下一次（播种代码会就地排序/修改）。
	got[0] = "hacked"
	if KnownHandlers()[0] != HandlerNotifyPhone {
		t.Fatal("KnownHandlers() 返回了内部切片（调用方改动会污染白名单）")
	}

	// 白名单里的每个 handler 都必须能过 Validate（空 run 的 handler 形式）。
	for _, h := range KnownHandlers() {
		a := validArtifact(t)
		a.Spec.Run = Run{}
		a.Spec.Handler = h
		if err := Validate(a); err != nil {
			t.Fatalf("handler %q 在白名单里却过不了 Validate：%v", h, err)
		}
	}
}

// handlerFormYAML 是 v3 的第二种实现形式：没有 run，只有白名单执行器。
const handlerFormYAML = `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: notify.sms
  version: 1
  displayName: 短信通知
spec:
  outputs: []
  lifecycle: {retryable: false, maxAttempts: 1, idempotent: true}
  sticky: worktree
  handler: notify-phone
`

// TestParseHandlerForm：handler 形式能被解析、能算 digest、能落盘往返，
// 且空 outputs 仍然序列化成 []（AGENTS.md 的空列表纪律）。
func TestParseHandlerForm(t *testing.T) {
	a, digest, err := Decode([]byte(handlerFormYAML))
	if err != nil {
		t.Fatalf("Decode(handler 形式): %v", err)
	}
	if a.Spec.Handler != HandlerNotifyPhone {
		t.Fatalf("handler = %q, want %q", a.Spec.Handler, HandlerNotifyPhone)
	}
	if len(a.Spec.Run.Files) != 0 || a.Spec.Run.Entrypoint != "" {
		t.Fatalf("handler 形式不该有 run 内容：%+v", a.Spec.Run)
	}
	if a.Metadata.DisplayName != "短信通知" {
		t.Fatalf("displayName = %q（v3 起在 metadata 里）", a.Metadata.DisplayName)
	}
	if a.Spec.Sticky != "worktree" {
		t.Fatalf("sticky = %q", a.Spec.Sticky)
	}
	if !strings.HasPrefix(digest, DigestPrefix) {
		t.Fatalf("digest = %q", digest)
	}

	canon, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !strings.Contains(string(canon), `"outputs":[]`) {
		t.Fatalf("空 outputs 必须序列化成 []，实际：%s", canon)
	}
	// handler 形式**不写 run 块**（见 Spec.MarshalJSON）：空的 run 会漏出
	// `"files":null` 与 `"entrypoint":""`，正好撞上「handler 不得带这些字段」的规则。
	if strings.Contains(string(canon), `"run"`) {
		t.Fatalf("handler 形式的规范字节里不该有 run 块：%s", canon)
	}

	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(MarshalYAML): %v\n%s", err, out)
	}
	if d, _ := Digest(back); d != digest {
		t.Fatalf("往返后 digest 变了：%s != %s", d, digest)
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
		// run.workDir 是能力定义里**唯一**的工作目录来源（节点侧的白名单已全部作废），
		// 所以这里的静态校验是那条路的第一道门；目录实际存不存在由节点在执行期判。
		{"workDir 相对路径", func(a *Artifact) { a.Spec.Run.WorkDir = "srv/app" }, "work_dir_not_absolute"},
		{"workDir 只有一个点", func(a *Artifact) { a.Spec.Run.WorkDir = "." }, "work_dir_not_absolute"},
		{"workDir 含 .. 段", func(a *Artifact) { a.Spec.Run.WorkDir = "/srv/app/../etc" }, "invalid_work_dir"},
		{"workDir 以 .. 结尾", func(a *Artifact) { a.Spec.Run.WorkDir = "/srv/.." }, "invalid_work_dir"},
		{"workDir 超长", func(a *Artifact) {
			a.Spec.Run.WorkDir = "/" + strings.Repeat("a", MaxWorkDirLen)
		}, "invalid_work_dir"},
		{"workDir 含 NUL", func(a *Artifact) { a.Spec.Run.WorkDir = "/srv/app\x00/etc" }, "invalid_work_dir"},
		// `~` 只在**节点**上展开（能力定义里存字面量）。所以字面量这边只能接受
		// `~` / `~/…` 两种写法，其余一律拒——尤其是 `~user/x`（取决于机器上有没有
		// 这个用户）与 `$HOME/x`（取决于进程环境，且本项目**不做** $VAR 展开）。
		{"workDir 是 ~user/x（不支持）", func(a *Artifact) { a.Spec.Run.WorkDir = "~user/x" }, "work_dir_not_absolute"},
		{"workDir 是 ~user（不支持）", func(a *Artifact) { a.Spec.Run.WorkDir = "~root" }, "work_dir_not_absolute"},
		{"workDir 是 $HOME/x（不展开）", func(a *Artifact) { a.Spec.Run.WorkDir = "$HOME/x" }, "work_dir_not_absolute"},
		// `..` 必须在展开**之前**查字面量：`~/../etc` 展开后就是家目录的父目录。
		{"workDir 是 ~/../etc", func(a *Artifact) { a.Spec.Run.WorkDir = "~/../etc" }, "invalid_work_dir"},
		{"workDir 是 ~/..", func(a *Artifact) { a.Spec.Run.WorkDir = "~/.." }, "invalid_work_dir"},
		{"workDir 是 ~/x 但超长", func(a *Artifact) {
			a.Spec.Run.WorkDir = "~/" + strings.Repeat("a", MaxWorkDirLen)
		}, "invalid_work_dir"},
		{"workDir 是 ~/x 但含 NUL", func(a *Artifact) { a.Spec.Run.WorkDir = "~/a\x00b" }, "invalid_work_dir"},
		// ---- Capability Spec v3 §3.1：实现形式恰好给一组 ----
		// 「都有」不能猜要对哪个（猜错就是内置执行器抢了作者的脚本）；
		// 「都没有」不是「还没配」，而是这份能力没有实现（契约 R2）。
		{"handler 与 files 都给", func(a *Artifact) { a.Spec.Handler = HandlerNoop }, "impl_ambiguous"},
		{"handler 与 entrypoint 都给", func(a *Artifact) {
			a.Spec.Handler = HandlerNoop
			a.Spec.Run.Files = nil
		}, "impl_ambiguous"},
		{"handler 形式带 timeoutSec", func(a *Artifact) {
			a.Spec.Handler = HandlerNoop
			a.Spec.Run = Run{TimeoutSec: 900}
		}, "impl_ambiguous"},
		{"handler 形式带 workDir", func(a *Artifact) {
			a.Spec.Handler = HandlerNoop
			a.Spec.Run = Run{WorkDir: "/srv/app"}
		}, "impl_ambiguous"},
		{"handler 形式带 env", func(a *Artifact) {
			a.Spec.Handler = HandlerNoop
			a.Spec.Run = Run{Env: map[string]string{"LOG_LEVEL": "info"}}
		}, "impl_ambiguous"},
		{"handler 不在白名单", func(a *Artifact) { a.Spec.Handler = "shell" }, "unknown_handler"},
		// 不 TrimSpace：边空格能过校验的话，校验通过而运行时查不到执行器，
		// 失败会推迟到派发那一刻（任务已建好、租约已扣）。
		{"handler 带边空格", func(a *Artifact) {
			a.Spec.Handler = " noop "
			a.Spec.Run = Run{}
		}, "unknown_handler"},
		{"handler 为空串等于没给", func(a *Artifact) {
			a.Spec.Handler = ""
			a.Spec.Run = Run{}
		}, "impl_missing"},
		{"两组都不给", func(a *Artifact) { a.Spec.Run = Run{} }, "impl_missing"},
		{"spec 整块为空", func(a *Artifact) { a.Spec = Spec{} }, "impl_missing"},
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

// ---------- run.workDir：能力声明的执行目录 ----------
//
// 工作目录的唯一来源是能力定义（产物里的 spec.run.workDir）——节点侧的
// node.yaml workDirs / ANOTIFY_WORK_DIRS / 中枢下发的 work_dir 全部作废。
// 于是这个字段同时承担三件事，下面逐条钉住：静态合法性、参与 digest、
// 能完整往返（落盘 → Parse 后 digest 不变，否则节点每次 apply 回来都会被判成
// "本地变体"）。

// TestWorkDirAcceptable：合法取值（含空 = 每次任务一个独立目录）。
func TestWorkDirAcceptable(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{"留空", ""},
		{"普通绝对路径", "/srv/app"},
		{"带结尾斜杠", "/srv/app/"},
		{"根目录", "/"},
		{"含空格与中文", "/srv/my app/项目"},
		{"恰好到上限", "/" + strings.Repeat("a", MaxWorkDirLen-1)},
		// `~` / `~/…`：节点执行时展开成 agent 用户的家目录（见 ExpandWorkDir）。
		// 这里只校验字面量形态，不展开（展开会随机器变，不能进 digest）。
		{"家目录 ~", "~"},
		{"家目录 ~/（等价于 ~）", "~/"},
		{"家目录下的项目", "~/proj"},
		{"家目录下的中文路径", "~/my app/项目"},
		{"~ 恰好到上限", "~/" + strings.Repeat("a", MaxWorkDirLen-2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact(t)
			a.Spec.Run.WorkDir = tc.dir
			if err := Validate(a); err != nil {
				t.Fatalf("workDir %q 应该合法，却报错：%v", tc.dir, err)
			}
		})
	}
}

// TestExpandWorkDir：`~` / `~/…` 在**节点**展开；其他写法原样返回。
//
// `$VAR` 一律不展开是**产品决策**（`os.ExpandEnv` 会把 $ANOTIFY_TASKDIR / $PATH
// 都求值，等于把声明式常量变成求值表达式）：把 `$HOME/x` 原样返回是故意用例。
func TestExpandWorkDir(t *testing.T) {
	const home = "/home/…"
	cases := []struct {
		name string
		dir  string
		home string
		want string
	}{
		{"~", "~", home, home},
		{"~/ 等于家目录", "~/", home, home},
		{"~/proj", "~/proj", home, home + "/proj"},
		{"~/多层", "~/a/b/c", home, home + "/a/b/c"},
		{"绝对路径不动", "/srv/app", home, "/srv/app"},
		{"相对路径不动", "proj", home, "proj"},
		// 只认 `~/` 前缀：`~user` 不展开（它不是家目录），原样返回后会被后续
		// 的 IsAbs 检查拒掉。
		{"~user 不展开", "~user/x", home, "~user/x"},
		{"$HOME 不展开", "$HOME/x", home, "$HOME/x"},
		{"${HOME} 不展开", "${HOME}/x", home, "${HOME}/x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandWorkDir(tc.dir, tc.home); got != tc.want {
				t.Fatalf("ExpandWorkDir(%q) = %q, want %q", tc.dir, got, tc.want)
			}
		})
	}
}

// TestIsHomeRelative：`~` 前缀的判定口径（`~user` / `$HOME` 都**不是**）。
func TestIsHomeRelative(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"~", true},
		{"~/", true},
		{"~/proj", true},
		{"", false},
		{"/srv/app", false},
		{"srv/app", false},
		{"~user/x", false},
		{"~user", false},
		{"$HOME/x", false},
	}
	for _, tc := range cases {
		if got := IsHomeRelative(tc.dir); got != tc.want {
			t.Fatalf("IsHomeRelative(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

// TestWorkDirDigestIsMachineIndependent：`~/proj` 的 digest 只取决于**字面量**，
// 与机器无关（节点会把同一份产物在不同家目录下执行，但 digest 必须一样）。
//
// 一旦有人把 `~` 展开进 digest（解析期展开 / 先展开再 canonicalize），
// 第一条（字面量原样进 canonical JSON）与第三条（两台机器同一字面量同摘要）都会红。
func TestWorkDirDigestIsMachineIndependent(t *testing.T) {
	a := validArtifact(t)
	a.Spec.Run.WorkDir = "~/proj"
	canon, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !strings.Contains(string(canon), `"workDir":"~/proj"`) {
		t.Fatalf("canonical JSON 里必须是字面量 ~/proj（展开进 digest 会让摘要随机器变）：%s", canon)
	}
	d, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	for _, home := range []string{"/home/…", "/root", "/Users/someone"} {
		b := validArtifact(t)
		b.Spec.Run.WorkDir = ExpandWorkDir("~/proj", home) // 展开后的路径**不**应影响摘要
		db, err := Digest(b)
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		if db == d {
			t.Fatalf("把 ~ 换成 %q 后 digest 没变（展开后的路径不应与字面量同摘要）", home)
		}
		d2, err := Digest(a)
		if err != nil {
			t.Fatalf("Digest: %v", err)
		}
		if d2 != d {
			t.Fatalf("同一字面量两次摘要不一致：%s != %s", d2, d)
		}
	}

	// 往返（落盘 → Parse）后仍是字面量、digest 不变：否则节点 apply 回来会被判成
	// "本地变体"（与 TestWorkDirDigestAndRoundTrip 同一条纪律，这里特别钉 ~）。
	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(MarshalYAML 产物): %v\n%s", err, out)
	}
	if back.Spec.Run.WorkDir != "~/proj" {
		t.Fatalf("往返后 workDir = %q, want 字面量 ~/proj", back.Spec.Run.WorkDir)
	}
	d3, err := Digest(back)
	if err != nil {
		t.Fatalf("Digest(back): %v", err)
	}
	if d3 != d {
		t.Fatalf("往返后 digest 变了：%s != %s（节点会把自家产物判成本地变体）", d3, d)
	}
}

// TestWorkDirDigestAndRoundTrip：改 workDir → digest 变；落盘再解析 → digest
// 不变、值原样保留。
//
// 去掉 Run.WorkDir 的 json tag（JSON 序列化不带上它）会红在第一条；
// Parse/CanonicalJSON 两侧口径不一致会红在第三条。
func TestWorkDirDigestAndRoundTrip(t *testing.T) {
	before := validArtifact(t)
	d0, err := Digest(before)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}

	a := validArtifact(t)
	a.Spec.Run.WorkDir = "/srv/app"
	d1, err := Digest(a)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d0 == d1 {
		t.Fatalf("改了 workDir 但 digest 没变（%s）：工作目录没参与内容寻址", d1)
	}

	// canonical JSON 里必须有它（否则 digest 变化的来源不是这个字段）
	canon, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if !strings.Contains(string(canon), `"workDir":"/srv/app"`) {
		t.Fatalf("canonical JSON 里缺 workDir：%s", canon)
	}

	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	back, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(MarshalYAML 产物): %v\n%s", err, out)
	}
	if back.Spec.Run.WorkDir != "/srv/app" {
		t.Fatalf("往返后 workDir = %q, want /srv/app", back.Spec.Run.WorkDir)
	}
	d2, err := Digest(back)
	if err != nil {
		t.Fatalf("Digest(back): %v", err)
	}
	if d2 != d1 {
		t.Fatalf("往返后 digest 变了：%s != %s（节点会把自家产物判成本地变体）", d2, d1)
	}
}

// TestWorkDirInYAML：手写在 YAML 里的 workDir 真的被读到、也真的被写回
// （json/yaml 两套 tag 少一个，用户改 yaml 时就会静默失效）。
func TestWorkDirInYAML(t *testing.T) {
	body := strings.Replace(validYAML, "    timeoutSec: 900\n",
		"    timeoutSec: 900\n    workDir: /srv/app\n", 1)
	if body == validYAML {
		t.Fatal("测试夹具没改到 timeoutSec 那一行")
	}
	a, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.Spec.Run.WorkDir != "/srv/app" {
		t.Fatalf("workDir = %q, want /srv/app", a.Spec.Run.WorkDir)
	}

	out, err := MarshalYAML(a)
	if err != nil {
		t.Fatalf("MarshalYAML: %v", err)
	}
	if !strings.Contains(string(out), "workDir: /srv/app") {
		t.Fatalf("MarshalYAML 丢了 workDir：\n%s", out)
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

// TestProblemCodeAndMessage 钉住「短码 / 人话」的切分口径（`check --json` 逐条用它）。
//
// 第三条（带两个冒号）是关键：只按**第一个**冒号切，否则 `invalid_yaml: yaml: line 3`
// 会变成 code="invalid_yaml: yaml"，与 Code() 的词表对不上。
func TestProblemCodeAndMessage(t *testing.T) {
	cases := []struct {
		problem string
		code    string
		message string
	}{
		{"invalid_name: 名字不对", "invalid_name", "名字不对"},
		{"invalid_yaml: yaml: line 3: did not find expected node", "invalid_yaml",
			"yaml: line 3: did not find expected node"},
		// 没有分隔符（理论上不该出现，但不能因此把整句话当成有人话没码）：
		{"something_broke", "something_broke", "something_broke"},
		// 以冒号开头：不作为分隔符（i > 0 的判断），否则 code 会是空串。
		{": 只有人话", ": 只有人话", ": 只有人话"},
	}
	for _, c := range cases {
		if got := ProblemCode(c.problem); got != c.code {
			t.Errorf("ProblemCode(%q) = %q, want %q", c.problem, got, c.code)
		}
		if got := ProblemMessage(c.problem); got != c.message {
			t.Errorf("ProblemMessage(%q) = %q, want %q", c.problem, got, c.message)
		}
	}

	// 与 Code() 必须同源：多问题里 Code 取首条，ProblemCode 也要给出同一个词。
	err := &Error{Problems: []string{"unsupported_api_version: 期望 anotify/v1", "invalid_name: x"}}
	if Code(err) != ProblemCode(err.Problems[0]) {
		t.Fatalf("Code 与 ProblemCode 分叉：%q vs %q", Code(err), ProblemCode(err.Problems[0]))
	}
}

// TestDigestIsStableAcrossParse 是内容寻址的地基：
//
//	Digest(Parse(CanonicalJSON(a))) == Digest(a)
//
// 这条不成立的话，中枢算出来的 digest 与**任何人从存下来的 spec 重算**的结果
// 都会不同：节点 apply 回来就被判成 local（"本地变体"），明明内容一模一样。
//
// 曾经真的破过：Parse 会把 spec.params 归一成 key 有序，而 CanonicalJSON 直接
// 吐调用方给的原始字节。所以 UI 用 `{type, properties}` 这个常见顺序建的每个
// 能力，digest 都与重算值不符 —— 而两端都是合法 sha256，肉眼看不出来。
func TestDigestIsStableAcrossParse(t *testing.T) {
	cases := []struct {
		name   string
		params string // 故意用**非** canonical 的 key 顺序
	}{
		{"未排序的 params", `{"type":"object","properties":{"note":{"type":"string","title":"说明"}}}`},
		{"嵌套数组与深层对象", `{"type":"object","required":["a"],"properties":{"a":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}}}}}}`},
		{"空 params", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact(t)
			if tc.params != "" {
				a.Spec.Params = json.RawMessage(tc.params)
			}
			d1, err := Digest(a)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}

			canon, err := CanonicalJSON(a)
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			reparsed, err := Parse(canon)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			d2, err := Digest(reparsed)
			if err != nil {
				t.Fatalf("Digest(reparsed): %v", err)
			}
			if d1 != d2 {
				t.Fatalf("digest 不可重算：原=%s 重算=%s\n（内容寻址失效：节点会被判成 local）", d1, d2)
			}

			// 再走一轮也必须稳定（幂等）
			canon2, _ := CanonicalJSON(reparsed)
			again, err := Parse(canon2)
			if err != nil {
				t.Fatalf("Parse 第二轮: %v", err)
			}
			if d3, _ := Digest(again); d3 != d1 {
				t.Fatalf("第二轮 digest 漂移：%s vs %s", d1, d3)
			}
		})
	}
}
