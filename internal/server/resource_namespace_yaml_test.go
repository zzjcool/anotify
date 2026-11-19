package server

// 只读 YAML 读面（Namespace / SystemConfig，IA ③④）的 HTTP 契约测试。
//
// 钉四件事，每条都是「改回去即红」的机械判据：
//  1. 文档是资源本身（apiVersion / kind / metadata / spec.vars），两次 GET 字节一致；
//  2. 只读：`status:` / `uid:` 不出现，`PUT …/yaml` 不是一条被静默接受的路由；
//  3. 严格解析器（`internal/yamldoc`）能读懂我们自己吐出来的文本，且问题定位带行号；
//  4. 系统面是管理员面：member 拿 403 `forbidden`（不是 401、不是 404）。

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/yamldoc"
)

// nsYAMLGet 发一次 YAML 读并返回全文（断言 200 + 形状）。
func nsYAMLGet(t *testing.T, env *nsTestEnv, path string, cookie *http.Cookie) string {
	t.Helper()
	rec := env.do(http.MethodGet, path, nil, cookie)
	obj := nsrJSON(t, rec, http.StatusOK)
	return nsrStr(t, obj, "yaml")
}

// TestNamespaceYAMLReadOnlyDocument 钉 Namespace 文档的内容与只读边界。
func TestNamespaceYAMLReadOnlyDocument(t *testing.T) {
	env := newNamespaceTestEnv(t)
	const path = "/v1/namespaces/default/yaml"

	// 写一份真实的命名空间默认 vars（写入口是 config 子资源，不是 YAML）。
	cfg := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/default/config", nil), http.StatusOK)
	cfgRV := nsrStr(t, nsrObj(t, cfg, "metadata"), "resourceVersion")
	nsrJSON(t, env.authed(http.MethodPut, "/v1/namespaces/default/config", map[string]any{
		"metadata": map[string]any{"resourceVersion": cfgRV},
		"vars":     map[string]string{"LOG_LEVEL": "info", "REGION": "cn-north"},
	}), http.StatusOK)

	text := nsYAMLGet(t, env, path, env.cookie)

	for _, want := range []string{
		"apiVersion: anotify/v1",
		"kind: Namespace",
		"metadata:",
		"name: default",
		"spec:",
		"vars:",
		"LOG_LEVEL: info",
		"REGION: cn-north",
		// 只读声明必须随文档一起被复制走（页面之外也要能读到写入入口在哪）。
		"PUT /v1/namespaces/{name}/config",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Namespace YAML 缺少 %q:\n%s", want, text)
		}
	}
	// 派生只读面与不透明内部身份不进文档：带上它们只会让人以为能改。
	for _, banned := range []string{"status:", "uid:", "counts:", "tasksByPhase"} {
		if strings.Contains(text, banned) {
			t.Fatalf("Namespace YAML 不应包含 %q:\n%s", banned, text)
		}
	}
	// 两次 GET 字节一致（规范化文本，页面刷新不会跳）。
	if again := nsYAMLGet(t, env, path, env.cookie); again != text {
		t.Fatalf("两次 GET 的 YAML 文本不一致：\n--- first ---\n%s\n--- second ---\n%s", text, again)
	}

	// 我们自己吐出来的文本必须能被同一个严格解析器读懂；行号可用。
	res, err := yamldoc.Decode([]byte(text), namespaceYAMLShape)
	if err != nil {
		t.Fatalf("严格解析器读不懂自己生成的 Namespace YAML: %v\n%s", err, text)
	}
	if line := res.LineOf("spec.vars.LOG_LEVEL"); line == 0 {
		t.Fatalf("LineOf(spec.vars.LOG_LEVEL) = 0，行号映射失效:\n%s", text)
	}

	// 只读：没有 PUT。这里**不断言具体状态码**：本包直接装配的 mux 会给 405，
	// 而生产 mux 末尾的静态兜底会把未注册方法变成 404（production_mux_test 里
	// 「退役写 = 404」是同一个已知行为）。可判定的判据只有一条：写不被接受。
	if rec := env.authed(http.MethodPut, path, map[string]any{"yaml": text}); rec.Code < 400 {
		t.Fatalf("PUT %s → %d，命名空间 YAML 必须只读（没有写路径）", path, rec.Code)
	}

	// 未知命名空间 → 404（与 JSON 读面同一判据）。
	nsrReason(t, env.authed(http.MethodGet, "/v1/namespaces/nope/yaml", nil), http.StatusNotFound, reasonNotFound)
}

// TestNamespaceYAMLEmptyVars 空 vars 必须显式写成 `{}`（不是 null、也不是省略字段）。
func TestNamespaceYAMLEmptyVars(t *testing.T) {
	env := newNamespaceTestEnv(t)
	text := nsYAMLGet(t, env, "/v1/namespaces/default/yaml", env.cookie)
	if !strings.Contains(text, "vars: {}") {
		t.Fatalf("空 vars 应写成 `vars: {}`:\n%s", text)
	}
	if strings.Contains(text, "vars: null") {
		t.Fatalf("空 vars 不能是 null:\n%s", text)
	}
}

// TestNamespaceYAMLStrictErrorsCarryLineNumbers 钉严格解析器的定位能力**对这份文档形状**成立：
// 未知字段与语法错都必须落在真实行号上（页面把行号原样显示给用户）。
func TestNamespaceYAMLStrictErrorsCarryLineNumbers(t *testing.T) {
	env := newNamespaceTestEnv(t)
	text := nsYAMLGet(t, env, "/v1/namespaces/default/yaml", env.cookie)
	lines := strings.Split(text, "\n")

	// 在 `kind:` 之后插一行顶层未知字段，记录它的 1-based 行号。
	injected := make([]string, 0, len(lines)+1)
	unknownLine := 0
	for _, line := range lines {
		injected = append(injected, line)
		if strings.HasPrefix(line, "kind:") {
			injected = append(injected, "bogusField: 1")
			unknownLine = len(injected)
		}
	}
	_, err := yamldoc.Decode([]byte(strings.Join(injected, "\n")), namespaceYAMLShape)
	yerr, ok := err.(*yamldoc.Error)
	if !ok || len(yerr.Problems) == 0 {
		t.Fatalf("未知字段应返回 *yamldoc.Error，实际 %T %v", err, err)
	}
	got := yerr.Problems[0]
	if got.Reason != "unknown_field" || got.Path != "bogusField" {
		t.Fatalf("未知字段问题 = %+v, want reason=unknown_field path=bogusField", got)
	}
	if got.Line != unknownLine {
		t.Fatalf("未知字段行号 = %d, want %d（行号必须指向原文，不是结构体序号）", got.Line, unknownLine)
	}

	// 语法错同样带行号。
	_, err = yamldoc.Decode([]byte("apiVersion: anotify/v1\nkind: Namespace\nmetadata: [\n"), namespaceYAMLShape)
	serr, ok := err.(*yamldoc.Error)
	if !ok || len(serr.Problems) == 0 || serr.Problems[0].Line == 0 {
		t.Fatalf("语法错应带行号，实际 %T %v", err, err)
	}
}

// TestSystemYAMLAdminOnly 钉系统文档的形状 + 管理员闸门。
func TestSystemYAMLAdminOnly(t *testing.T) {
	env := newNamespaceTestEnv(t)
	const path = "/v1/system/yaml"

	// 先改一次保留期，证明文档读的是**真实生效值**而不是默认常量。
	cur := nsrJSON(t, env.authed(http.MethodGet, "/v1/system/config", nil), http.StatusOK)
	rv := nsrStr(t, nsrObj(t, cur, "metadata"), "resourceVersion")
	nsrJSON(t, env.authed(http.MethodPut, "/v1/system/config", map[string]any{
		"metadata":  map[string]any{"resourceVersion": rv},
		"retention": map[string]any{"eventTtlSec": 11, "taskTtlSec": 22, "resultTtlSec": 33},
	}), http.StatusOK)

	text := nsYAMLGet(t, env, path, env.cookie)
	for _, want := range []string{
		"apiVersion: anotify/v1",
		"kind: SystemConfig",
		"metadata:",
		"resourceVersion:",
		"spec:",
		"retention:",
		"eventTtlSec: 11",
		"taskTtlSec: 22",
		"resultTtlSec: 33",
		"PUT /v1/system/config",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("System YAML 缺少 %q:\n%s", want, text)
		}
	}
	if again := nsYAMLGet(t, env, path, env.cookie); again != text {
		t.Fatalf("两次 GET 的 System YAML 不一致:\n--- first ---\n%s\n--- second ---\n%s", text, again)
	}
	res, err := yamldoc.Decode([]byte(text), systemYAMLShape)
	if err != nil {
		t.Fatalf("严格解析器读不懂自己生成的 System YAML: %v\n%s", err, text)
	}
	if line := res.LineOf("spec.retention.eventTtlSec"); line == 0 {
		t.Fatalf("LineOf(spec.retention.eventTtlSec) = 0:\n%s", text)
	}
	if rec := env.authed(http.MethodPut, path, map[string]any{"yaml": text}); rec.Code < 400 {
		t.Fatalf("PUT %s → %d，系统 YAML 必须只读（没有写路径）", path, rec.Code)
	}

	// member：三个系统端点都是 403 forbidden（未登录才是 401）。
	member := env.memberCookie(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/system/yaml"},
		{http.MethodGet, "/v1/system/config"},
		{http.MethodPut, "/v1/system/config"},
	} {
		nsrReason(t, env.do(tc.method, tc.path, map[string]any{
			"metadata":  map[string]any{"resourceVersion": rv},
			"retention": map[string]any{"eventTtlSec": 1, "taskTtlSec": 1, "resultTtlSec": 1},
		}, member), http.StatusForbidden, reasonForbidden)
	}

	// 403 不得泄漏保留期事实（body 里不能出现真实 TTL）。
	rec := env.do(http.MethodGet, "/v1/system/yaml", nil, member)
	if strings.Contains(rec.Body.String(), "eventTtlSec") {
		t.Fatalf("member 的 403 响应泄漏了保留期字段: %s", rec.Body.String())
	}
}
