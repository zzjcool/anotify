package server

// Node 文档 YAML 读/写面的 HTTP 验收（IA 重构 ②，实现在 `resource_node_yaml.go`）。
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

	"gopkg.in/yaml.v3"
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
	if stripNodeRV(got) != stripNodeRV(text) {
		t.Fatalf("回写后的 YAML 与读面不一致:\n--- 读 ---\n%s\n--- 写回 ---\n%s", text, got)
	}
	if got == text {
		t.Fatalf("写成功后 resourceVersion 应当前进（令牌没变）:\n%s", got)
	}
}

// stripNodeRV 把 resourceVersion 那一行抹平（每次成功写入都会 bump 它）。
func stripNodeRV(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "resourceVersion:") {
			lines[i] = "  resourceVersion: <bumped>"
		}
	}
	return strings.Join(lines, "\n")
}

// getNodeYAML 读一台节点的规范化文档（断言 200 + 正文非空）。
func getNodeYAML(t *testing.T, env *nodeTestEnv, name string) string {
	t.Helper()
	rr := env.do(http.MethodGet, "/v1/nodes/"+name+"/yaml", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET yaml %q = %d, want 200 (body=%s)", name, rr.Code, rr.Body.String())
	}
	text, _ := decodeNodeDoc(t, rr)["yaml"].(string)
	if strings.TrimSpace(text) == "" {
		t.Fatalf("GET yaml %q 返回空正文", name)
	}
	return text
}

// TestNodeYAMLRoundTripWithoutTotal 是 issue #570 的回归：控制台「登记节点」出来的节点都没有
// 设 limits.total，读面因此给出显式 `total: null`。这份正文必须能原样贴回（200），
// 而不是 400 `limits.total 必须是 ≥1 的整数，实际 <nil>`。
func TestNodeYAMLRoundTripWithoutTotal(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("hw", map[string]any{"namespaces": []string{"default"}})
	text := getNodeYAML(t, env, "hw")
	// 读面语义不变：显式 null = 「本层不压住下层」，所以文档里要有这一行。
	if !strings.Contains(text, "    total: null\n") {
		t.Fatalf("未设 total 的节点，文档里应是显式 total: null：\n%s", text)
	}
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": text})
	if rr.Code != http.StatusOK {
		t.Fatalf("原样回写 = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	got, _ := decodeNodeDoc(t, rr)["yaml"].(string)
	if stripNodeRV(got) != stripNodeRV(text) {
		t.Fatalf("回写后的文档与读面不一致：\n--- 读 ---\n%s\n--- 写回 ---\n%s", text, got)
	}
	// 读回配置：null 不该被写成一个 total 键（= 不压住下层）。
	cfg := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw/config", nil))
	if limits := nodeObject(t, cfg, "limits"); len(limits) != 0 {
		t.Fatalf("显式 null 不该落成 limits 的键: %v", limits)
	}
}

// TestNodeYAMLPutNullTotalClearsBudget 显式 total: null = 清掉这一维度，其余维度保持原值。
func TestNodeYAMLPutNullTotalClearsBudget(t *testing.T) {
	env := newNodeTestEnv(t)
	text, _ := nodeYAMLFixture(t, env, "hw")
	if !strings.Contains(text, "    total: 4\n") {
		t.Fatalf("fixture 应带 total: 4：\n%s", text)
	}
	noTotal := strings.Replace(text, "    total: 4\n", "    total: null\n", 1)
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": noTotal})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml（total: null）= %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	cfg := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw/config", nil))
	limits := nodeObject(t, cfg, "limits")
	if _, has := limits["total"]; has {
		t.Fatalf("total: null 之后 limits 不该还有 total 键: %v", limits)
	}
	// 同一个 limits 对象里的其他维度不受影响（PUT 是整份 limits 替换，不是只改 total）。
	per, _ := limits["perCapability"].(map[string]any)
	if per["*"] != float64(2) || per["fixer"] != float64(1) {
		t.Fatalf("total: null 不该动 perCapability: %v", limits)
	}
	// 回读文档仍是显式 null（读面形状不变），且可以再贴回。
	again := getNodeYAML(t, env, "hw")
	if !strings.Contains(again, "    total: null\n") {
		t.Fatalf("写回后文档里应是显式 total: null：\n%s", again)
	}
	if rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": again}); rr.Code != http.StatusOK {
		t.Fatalf("再次原样回写 = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestNodeYAMLPutNullPerCapabilityMeansUnset perCapability 本身为 null 同样等于「没设」。
func TestNodeYAMLPutNullPerCapabilityMeansUnset(t *testing.T) {
	env := newNodeTestEnv(t)
	env.createNode("hw", map[string]any{"namespaces": []string{"default"}})
	rv := env.configRV("hw")
	doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " + rv +
		"\nspec:\n  limits:\n    total: null\n    perCapability: null\n"
	rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc})
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml（limits 全是 null）= %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	cfg := decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw/config", nil))
	if limits := nodeObject(t, cfg, "limits"); len(limits) != 0 {
		t.Fatalf("null 的 limits 不该落成键: %v", limits)
	}
}

// TestNodeYAMLPutRejectsNonPositiveLimits null 被接受不等于放松校验：0 / 负数 / 字符串仍是 400。
func TestNodeYAMLPutRejectsNonPositiveLimits(t *testing.T) {
	env := newNodeTestEnv(t)
	text, _ := nodeYAMLFixture(t, env, "hw")
	for _, bad := range []string{"0", "-1", `"3"`, "1.5", "true"} {
		doc := strings.Replace(text, "    total: 4\n", "    total: "+bad+"\n", 1)
		if doc == text {
			t.Fatalf("夹具替换失败（total: 4 不在文档里）")
		}
		wantStatus(t, env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": doc}),
			http.StatusBadRequest, reasonValueInvalid)
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
	// #454：`tags` 的省略语义必须同样是「不动」。节点 YAML 编辑页在 `/config`
	// （W5 wrapper，没有 tags）不返回现值时就是靠**整键省略**保住原值；
	// 写成 `tags: []` 才会被当成显式清空。这两条语义必须可区分 —— 否则页面侧
	// 「省略而不是空数组」的修复只是把缺陷挪了个地方。
	if tags := nodeObject(t, node, "spec")["tags"].([]any); len(tags) != 2 {
		t.Fatalf("省略 tags 被当成清空了：%v", tags)
	}
	// 反向对照：显式 `tags: []` **是**清空。
	cleared := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\n  resourceVersion: " +
		nodeString(t, node, "metadata", "resourceVersion") + "\nspec:\n  tags: []\n"
	if rr := env.do(http.MethodPut, "/v1/nodes/hw/yaml", map[string]any{"yaml": cleared}); rr.Code != http.StatusOK {
		t.Fatalf("PUT yaml（显式 tags: []）= %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if tags := nodeObject(t, decodeNodeDoc(t, env.do(http.MethodGet, "/v1/nodes/hw", nil)), "spec")["tags"].([]any); len(tags) != 0 {
		t.Fatalf("显式 tags: [] 必须清空（省略与清空不可区分就说明判据坏了）：%v", tags)
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

func TestNodeYAMLScalarEscapingIsV3Readable(t *testing.T) {
	cases := []struct{ what, emitted, want string }{ // #646：JS 侧写什么，yaml.v3 就得读回什么（与 scripts/yamldoc.test.mjs 同一张表互为外键）
		{"以冒号结尾", `"a:"`, "a:"},
		{"URL 以冒号结尾", `"http://x:"`, "http://x:"},
		{"冒号在中间", "k:v", "k:v"},
		{"指数形态", `"1e3"`, "1e3"},
		{"八进制形态", `"010"`, "010"},
		{"超 2^53 的整数", `"12345678901234567890"`, "12345678901234567890"},
	}
	for _, c := range cases {
		doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\nspec:\n  vars:\n    K: " + c.emitted + "\n"
		var parsed nodeYAMLDoc
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("%s：web/yamldoc.js 写出的 %s 必须能被 yaml.v3 读回，实际 %v", c.what, c.emitted, err)
		}
		if got := parsed.Spec.Vars["K"]; got != c.want {
			t.Fatalf("%s：%s 读成 %q，期望 %q", c.what, c.emitted, got, c.want)
		}
	}
	for _, bare := range []string{"a:", "http://x:"} {
		doc := "apiVersion: anotify/v1\nkind: Node\nmetadata:\n  name: hw\nspec:\n  vars:\n    K: " + bare + "\n"
		var parsed nodeYAMLDoc
		if err := yaml.Unmarshal([]byte(doc), &parsed); err == nil {
			t.Fatalf("裸写 %q 必须被 yaml.v3 拒绝，否则「以冒号结尾必须加引号」这条判据不再说明问题", bare)
		}
	}
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
