package server

// Node 文档 YAML 读/写面的 HTTP 验收（IA 重构 ②，docs/design/node-namespace-ia.md §2.1）。
//
// 覆盖的契约点：
//   - GET 返回 `{"yaml": "…"}`，文档是 apiVersion/kind/metadata/spec，**没有** status；
//   - 文档里没有 token（明文只在 /tokens 出现一次）、没有 anode.yaml 私有字段；
//   - PUT 用同一份文本原样回写是幂等的（规范化往返）；
//   - 未知字段 / status / kind 不符 / name 不符 / 缺 CAS / 旧 CAS 各自是一个具名 400/409；
//   - 省略的字段 = 不动（与 PATCH 同一 merge 语义），写入后 spec 与 config revision 都真的变了。

import (
	"net/http"
	"strings"
	"testing"
)

// nodeYAMLFixture 建一台带完整 spec 的节点并返回它的 YAML 文本。
func nodeYAMLFixture(t *testing.T, env *nodeTestEnv, name string) (string, string) {
	t.Helper()
	env.createNode(name, map[string]any{
		"namespaces": []string{"default", "prod"},
		"vars":       map[string]string{"TOOLCHAIN": "/opt/go1.25", "B_KEY": "2", "A_KEY": "1"},
		"limits": map[string]any{
			"total":         4,
			"perCapability": map[string]any{"*": 2, "fixer": 1},
		},
		"tags": []string{"build", "prod"},
	})
	rr := env.do(http.MethodGet, "/v1/nodes/"+name+"/yaml", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET yaml %q = %d, want 200 (body=%s)", name, rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	if len(doc) != 1 {
		t.Fatalf("GET yaml 的响应体必须只有 yaml 一个键（不是信封）: %v", doc)
	}
	text, _ := doc["yaml"].(string)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("GET yaml 返回空正文")
	}
	return text, env.configRV(name)
}

// TestNodeYAMLGetReturnsCanonicalDocument 钉住文档形状与顺序：
// YAML 是同一份事实的另一种序列化，不是第二套字段。
func TestNodeYAMLGetReturnsCanonicalDocument(t *testing.T) {
	env := newNodeTestEnv(t)
	text, rv := nodeYAMLFixture(t, env, "hw")

	for _, want := range []string{
		"apiVersion: anotify/v1\n",
		"kind: Node\n",
		"metadata:\n",
		"  name: hw\n",
		"  resourceVersion: " + rv + "\n",
		"spec:\n",
		"  namespaces:\n    - default\n    - prod\n",
		"  vars:\n    A_KEY: \"1\"\n    B_KEY: \"2\"\n    TOOLCHAIN: /opt/go1.25\n",
		"  limits:\n    total: 4\n    perCapability:\n      '*': 2\n      fixer: 1\n",
		"  tags:\n    - build\n    - prod\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("YAML 缺片段 %q\n---- 实际 ----\n%s", want, text)
		}
	}
	// status 是派生只读面，不进可编辑文档；token 与 anode.yaml 私有字段永不出现。
	for _, banned := range []string{"status:", "token", "listen", "envPassthrough", "server:"} {
		if strings.Contains(text, banned) {
			t.Fatalf("YAML 不该出现 %q：\n%s", banned, text)
		}
	}
	// 键序稳定：同样的事实再请求一次，字节完全相同（「取 → 改 → 贴回」才是一次往返）。
	rr := env.do(http.MethodGet, "/v1/nodes/hw/yaml", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("第二次 GET yaml = %d, want 200", rr.Code)
	}
	again, _ := decodeNodeDoc(t, rr)["yaml"].(string)
	if again != text {
		t.Fatalf("两次 GET 的 YAML 不一致（序列化不确定）:\n%s\n----\n%s", text, again)
	}
}

// TestNodeYAMLPutRoundTripIsIdempotent 原样回写：200，且正文一个字节都不变。
func TestNodeYAMLPutRoundTripIsIdempotent(t *testing.T) {
	env := newNodeTestEnv(t)
	text, _ := nodeYAMLFixture(t, env, "hw")

	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": text})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml 原样回写 = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	doc := decodeNodeDoc(t, rr)
	got, _ := doc["yaml"].(string)
	// 一次成功写入会 bump 底层 resourceVersion（Node.spec 与 config 子资源共享同一个
	// 版本），所以除那一行之外必须逐字相同：其余任何差异都说明「取 → 贴回」改到了别的东西。
	strip := func(s string) string {
		lines := strings.Split(s, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "resourceVersion:") {
				lines[i] = "  resourceVersion: <bumped>"
			}
		}
		return strings.Join(lines, "\n")
	}
	if strip(got) != strip(text) {
		t.Fatalf("回写后的 YAML 与读面不一致:\n--- 读 ---\n%s\n--- 写回 ---\n%s", text, got)
	}
	if got == text {
		t.Fatalf("写成功后 resourceVersion 应当前进（令牌没变）:\n%s", got)
	}
}

// TestNodeYAMLPutAppliesAndBumpsRevision 改 vars/namespaces：spec 与 config revision 真的变了。
func TestNodeYAMLPutAppliesAndBumpsRevision(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	before := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw/config", nil))
	beforeRev := nodeString(t, before, "configRevision")

	updated := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv +
		"\nspec:\n  namespaces: [default]\n  vars:\n    NEW_KEY: from-yaml\n"
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": updated})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	node := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw", nil))
	spec := nodeObject(t, node, "spec")
	ns := spec["namespaces"].([]any)
	if len(ns) != 1 || ns[0] != "default" {
		t.Fatalf("spec.namespaces = %v, want [default]", ns)
	}
	if got := nodeString(t, node, "spec", "vars", "NEW_KEY"); got != "from-yaml" {
		t.Fatalf("spec.vars.NEW_KEY = %q, want from-yaml", got)
	}
	after := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw/config", nil))
	if nodeString(t, after, "configRevision") == beforeRev {
		t.Fatalf("configRevision 没变（%s）—— YAML 写入必须 bump 配置生效版本", beforeRev)
	}
	if nodeString(t, after, "metadata", "resourceVersion") == rv {
		// 注意：节点 spec 与 config 共享底层 rv，写成功后令牌必须前进。
		t.Fatalf("写成功后 resourceVersion 没前进（%s）", rv)
	}
}

// TestNodeYAMLPutOmittedFieldsKeepValues 省略 ≠ 清空（与 PATCH 同一 merge 语义）。
func TestNodeYAMLPutOmittedFieldsKeepValues(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv + "\nspec:\n  vars:\n    ONLY_VARS: changed\n"
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml（只给 vars）= %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	node := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw", nil))
	ns := nodeObject(t, node, "spec")["namespaces"].([]any)
	if len(ns) != 2 {
		t.Fatalf("省略 namespaces 被当成清空了：%v", ns)
	}
	if got := nodeString(t, node, "spec", "limits", "total"); got != "" {
		// limits.total 是数字，nodeString 拿不到；这里只断言它还在（下面的 JSON 断言）。
		_ = got
	}
	limits := nodeObject(t, node, "spec", "limits")
	if limits["total"] != float64(4) {
		t.Fatalf("省略 limits 被当成清空了：%v", limits)
	}
	if got := nodeString(t, node, "spec", "vars", "ONLY_VARS"); got != "changed" {
		t.Fatalf("spec.vars.ONLY_VARS = %q, want changed", got)
	}
}

// TestNodeYAMLPutRejectsUnknownFieldWithLine 文档层问题必须带行号与字段路径。
func TestNodeYAMLPutRejectsUnknownFieldWithLine(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv +
		"\nspec:\n  namespaces: [default]\n  bogus: 1\n"
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc})
	status := wantStatus(t, rr, http.StatusBadRequest, reasonUnknownField)
	causes := status["details"].(map[string]any)["causes"].([]any)
	if len(causes) == 0 {
		t.Fatalf("缺 details.causes[]: %v", status)
	}
	first := causes[0].(map[string]any)
	if field, _ := first["field"].(string); field != "spec.bogus" {
		t.Fatalf("causes[0].field = %q, want spec.bogus", field)
	}
	msg, _ := first["message"].(string)
	if !strings.HasPrefix(msg, "第 8 行：") {
		t.Fatalf("causes[0].message 必须以「第 8 行：」开头（实际 %q）", msg)
	}
}

// TestNodeYAMLPutRejectsStatus status 是只读派生面（不是「未知字段」）。
func TestNodeYAMLPutRejectsStatus(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv +
		"\nspec: {}\nstatus:\n  online: true\n"
	wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc}),
		http.StatusBadRequest, reasonStatusNotWritable)
}

// TestNodeYAMLPutRejectsKindAndNameMismatch 文档身份必须与路径一致（不隐式改名 / 换 kind）。
func TestNodeYAMLPutRejectsKindAndNameMismatch(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	base := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv + "\nspec: {}\n"

	wrongKind := strings.Replace(base, "kind: Node", "kind: Namespace", 1)
	wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": wrongKind}),
		http.StatusBadRequest, reasonValueInvalid)

	wrongName := strings.Replace(base, "  name: hw", "  name: other", 1)
	wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": wrongName}),
		http.StatusBadRequest, reasonNameImmutable)
}

// TestNodeYAMLPutRequiresCAS 缺 metadata.resourceVersion → 400（与 JSON 写入口同一判据）。
func TestNodeYAMLPutRequiresCAS(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("hw", nil)
	doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\nspec: {}\n"
	wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc}),
		http.StatusBadRequest, reasonResourceVersionRequired)
}

// TestNodeYAMLPutStaleCAS 旧令牌 → 409 + details.current（页面据此提示刷新，不覆盖别人）。
func TestNodeYAMLPutStaleCAS(t *testing.T) {
	env := newNodeTestEnv(t)
	_, rv := nodeYAMLFixture(t, env, "hw")
	// 先用当前令牌成功写一次，让 rv 前进。
	first := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv + "\nspec:\n  vars:\n    K: v1\n"
	if rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": first}); rr.Code != http.StatusOK {
		t.Fatalf("第一次写 = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	// 再用同一份旧令牌 → 409。
	status := wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": first}),
		http.StatusConflict, reasonResourceVersionConflict)
	current := nodeValue(t, status, "details", "current", "resourceVersion")
	if current == rv {
		t.Fatalf("details.current.resourceVersion 仍是旧令牌 %q", rv)
	}
}
