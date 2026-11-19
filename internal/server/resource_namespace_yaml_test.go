package server

// Namespace / SystemConfig 文档 YAML 面的 HTTP 契约测试。
//
// 钉四件事，每条都是「改回去即红」的机械判据：
//  1. 文档是资源本身（apiVersion / kind / metadata / spec.vars），两次 GET 字节一致；
//  2. 命名空间文档**可写**：`PUT /v1/namespaces/{name}/yaml` 是命名空间 vars 的唯一编辑面
//     （CAS = `metadata.resourceVersion`）；系统文档仍只读，写入口是 `PUT /v1/system/config`；
//  3. 严格解析器（`internal/yamldoc`）能读懂我们自己吐出来的文本，且问题定位带行号；
//  4. 系统面是管理员面：member 拿 403 `forbidden`（不是 401、不是 404）。

import (
	"fmt"
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

// TestNamespaceYAMLDocument 钉 Namespace 文档的内容与「可写」边界。
func TestNamespaceYAMLDocument(t *testing.T) {
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
		// 写入入口必须随文档一起被复制走（页面之外也要能读到「改哪一份」）。
		"PUT /v1/namespaces/{name}/yaml",
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

	// 命名空间文档是**可写**的（#476 阶段 2）：PUT 已注册且与会话中间件同在 ——
	// 未登录拿到的是 401（会话先拒），而不是 405 / 404（没有写路径时的兜底）。
	// 这里只比对状态码：会话中间件的 401 走的是旧式 `{"error":…}` 体，不是 Status 信封
	// （与 TestNamespaceResourceRequiresSession 同一口径）。
	if rec := env.do(http.MethodPut, path, map[string]any{"yaml": text}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 PUT %s → %d, want 401 (body=%s)", path, rec.Code, rec.Body.String())
	}

	// 未知命名空间 → 404（与 JSON 读面同一判据）。
	nsrReason(t, env.authed(http.MethodGet, "/v1/namespaces/nope/yaml", nil), http.StatusNotFound, reasonNotFound)
}

// TestNamespaceYAMLPutRoundTrip 钉「YAML 是命名空间 vars 的唯一编辑面」的整条写路径：
// GET yaml 与 GET config 给**同一个** CAS 令牌 → 原样贴回 200（不改任何键，只前进令牌）→
// 改一个 var → PUT 200（回规范化文本）→ 再 GET 只有该键变化 → 旧令牌 409 →
// 缺令牌 / 缺 vars / 未知字段（带行号）/ status / kind 各自具名 400。
//
// 每个断言都是「改回去即红」：PUT 一旦退回「没有写路径」，第一条就红。
func TestNamespaceYAMLPutRoundTrip(t *testing.T) {
	env := newNamespaceTestEnv(t)
	const path = "/v1/namespaces/default/yaml"

	// 种子：两个真实 var（写入口用 config 子资源，证明两条入口读的是**同一份事实**）。
	cfg := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/default/config", nil), http.StatusOK)
	nsrJSON(t, env.authed(http.MethodPut, "/v1/namespaces/default/config", map[string]any{
		"metadata": map[string]any{"resourceVersion": nsrStr(t, nsrObj(t, cfg, "metadata"), "resourceVersion")},
		"vars":     map[string]string{"LOG_LEVEL": "info", "REGION": "cn-north"},
	}), http.StatusOK)

	// rvOf 取文本里的 CAS 令牌行；stripRV 把那一行抹平（写成功会 bump 它，其余必须逐字不变）。
	rvOf := func(text string) string {
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "resourceVersion:") {
				return strings.TrimSpace(strings.TrimPrefix(trimmed, "resourceVersion:"))
			}
		}
		return ""
	}
	stripRV := func(text string) string {
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "resourceVersion:") {
				lines[i] = "  resourceVersion: <bumped>"
			}
		}
		return strings.Join(lines, "\n")
	}

	// 两个读面必须给**同一个** CAS 令牌：命名空间 vars 的令牌就是
	// `namespaces.resource_version`（`GetNamespaceConfig` 投影成 cfg.ResourceVersion），
	// GET /yaml 的 metadata.resourceVersion 必须与之逐字相同 —— 否则「从 config 拿令牌
	// 写 yaml」与「从 yaml 拿令牌写 yaml」会一半成功一半 409。
	cfgNow := nsrJSON(t, env.authed(http.MethodGet, "/v1/namespaces/default/config", nil), http.StatusOK)
	cfgRV := nsrStr(t, nsrObj(t, cfgNow, "metadata"), "resourceVersion")
	original := nsYAMLGet(t, env, path, env.cookie)
	if got := rvOf(original); got != cfgRV {
		t.Fatalf("GET yaml 的 metadata.resourceVersion = %q，GET config 给的是 %q —— 两个读面必须是同一个 CAS 令牌",
			got, cfgRV)
	}

	// 原样贴回（不改任何键）→ 200：一次真正的往返，除令牌行前进外逐字不变。
	verbatim := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{"yaml": original}), http.StatusOK)
	verbatimText := nsrStr(t, verbatim, "yaml")
	if stripRV(verbatimText) != stripRV(original) {
		t.Fatalf("原样回写改变了内容（应当只有 resourceVersion 行前进）:\n--- read ---\n%s\n--- written ---\n%s",
			stripRV(original), stripRV(verbatimText))
	}
	if rvOf(verbatimText) == cfgRV {
		t.Fatalf("原样回写后 resourceVersion 没前进（%s）", cfgRV)
	}
	for _, want := range []string{"LOG_LEVEL: info", "REGION: cn-north"} {
		if !strings.Contains(verbatimText, want) {
			t.Fatalf("原样回写丢了 %q:\n%s", want, verbatimText)
		}
	}

	// 从写后的文本继续（令牌已前进）。
	before := nsYAMLGet(t, env, path, env.cookie)
	oldRV := rvOf(before)
	if oldRV == "" {
		t.Fatalf("读面没有 resourceVersion 行（CAS 令牌）:\n%s", before)
	}

	// 只改一个 var，其余行原样贴回（含两段 prelude 与 vars 里的解释性注释）。
	edited := strings.Replace(before, "REGION: cn-north", "REGION: cn-northeast", 1)
	if edited == before {
		t.Fatalf("测试自身坏了：文本里没有 REGION: cn-north\n%s", before)
	}
	put := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{"yaml": edited}), http.StatusOK)
	if len(put) != 1 {
		t.Fatalf("PUT 的响应体必须只有 yaml 一个键（是 ResourceYAML，不是信封）: %v", put)
	}
	putText := nsrStr(t, put, "yaml")
	if !strings.Contains(putText, "REGION: cn-northeast") {
		t.Fatalf("PUT 的响应里没有新值:\n%s", putText)
	}
	if rvOf(putText) == oldRV {
		t.Fatalf("写成功后 resourceVersion 没前进（%s）——回一个刚过期的令牌会让下一次贴回立刻 409", oldRV)
	}

	// 再 GET：PUT 的响应就是随后的 GET（同一条回读口径），且除令牌行外逐字不变。
	after := nsYAMLGet(t, env, path, env.cookie)
	if after != putText {
		t.Fatalf("PUT 的响应必须等于随后的 GET:\n--- PUT ---\n%s\n--- GET ---\n%s", putText, after)
	}
	if stripRV(after) != stripRV(edited) {
		t.Fatalf("除 resourceVersion 外文本被改动了（其余行必须逐字不变）:\n--- want ---\n%s\n--- got ---\n%s",
			stripRV(edited), stripRV(after))
	}

	// 旧令牌：同一份文本再贴一次 → 409（store 的 CAS，不是 last-write-wins），
	// 并且 details.current.resourceVersion 给出当前令牌（否则客户端只能猜）。
	conflict := nsrReason(t, env.authed(http.MethodPut, path, map[string]any{"yaml": edited}),
		http.StatusConflict, reasonResourceVersionConflict)
	current := nsrStr(t, nsrObj(t, nsrObj(t, conflict, "details"), "current"), "resourceVersion")
	if current == oldRV || current == "" {
		t.Fatalf("details.current.resourceVersion = %q，应给新令牌（旧 %q）", current, oldRV)
	}

	// 缺 metadata.resourceVersion → 400 resource_version_required（不静默覆盖）。
	withoutRV := make([]string, 0, len(strings.Split(putText, "\n")))
	for _, line := range strings.Split(putText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "resourceVersion:") {
			continue
		}
		withoutRV = append(withoutRV, line)
	}
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{"yaml": strings.Join(withoutRV, "\n")}),
		http.StatusBadRequest, reasonResourceVersionRequired)

	// 缺 spec.vars：PUT 是整份替换，不能用「没写」代替「清空」——与 PUT …/config 同一口径。
	noVars := "apiVersion: anotify/v1\nkind: Namespace\nmetadata:\n  name: default\n  resourceVersion: " +
		rvOf(putText) + "\nspec: {}\n"
	varsErr := nsrReason(t, env.authed(http.MethodPut, path, map[string]any{"yaml": noVars}),
		http.StatusBadRequest, reasonValueInvalid)
	if msg := nsrStr(t, varsErr, "message"); !strings.Contains(msg, "send {} to clear") {
		t.Fatalf("缺 spec.vars 的 message 必须写明「send {} to clear」（与 PUT …/config 同口径）：%q", msg)
	}

	// 未知字段 → 400 unknown_field，details.causes[] 带行号与字段路径。
	lines := strings.Split(putText, "\n")
	injected := make([]string, 0, len(lines)+1)
	unknownLine := 0
	for _, line := range lines {
		injected = append(injected, line)
		if strings.HasPrefix(line, "kind:") {
			injected = append(injected, "bogusField: 1")
			unknownLine = len(injected)
		}
	}
	if unknownLine == 0 {
		t.Fatalf("测试自身坏了：文本里没有 kind: 行\n%s", putText)
	}
	bad := nsrReason(t, env.authed(http.MethodPut, path, map[string]any{"yaml": strings.Join(injected, "\n")}),
		http.StatusBadRequest, reasonUnknownField)
	causes := nsrArr(t, nsrObj(t, bad, "details"), "causes")
	if len(causes) == 0 {
		t.Fatalf("未知字段的 400 必须带 details.causes[]: %v", bad)
	}
	first, _ := causes[0].(map[string]any)
	if field, _ := first["field"].(string); field != "bogusField" {
		t.Fatalf("causes[0].field = %q, want bogusField", field)
	}
	if msg, _ := first["message"].(string); !strings.HasPrefix(msg, fmt.Sprintf("第 %d 行：", unknownLine)) {
		t.Fatalf("causes[0].message 必须以「第 %d 行：」开头（实际 %q）", unknownLine, msg)
	}

	// 顶层 status → 400 status_not_writable（派生只读面，不是「未知字段」）。
	withStatus := putText + "status:\n  counts: {}\n"
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{"yaml": withStatus}),
		http.StatusBadRequest, reasonStatusNotWritable)

	// kind 必须与路径上的资源一致（文档身份不是注释）。
	nsrReason(t, env.authed(http.MethodPut, path, map[string]any{
		"yaml": strings.Replace(putText, "kind: Namespace", "kind: Node", 1),
	}), http.StatusBadRequest, reasonValueInvalid)
}

// TestNamespaceYAMLEmptyVars 空 vars 必须显式写成 `{}`（不是 null、也不是省略字段）。
func TestNamespaceYAMLEmptyVars(t *testing.T) {
	env := newNamespaceTestEnv(t)
	const path = "/v1/namespaces/default/yaml"
	text := nsYAMLGet(t, env, path, env.cookie)
	if !strings.Contains(text, "vars: {}") {
		t.Fatalf("空 vars 应写成 `vars: {}`:\n%s", text)
	}
	if strings.Contains(text, "vars: null") {
		t.Fatalf("空 vars 不能是 null:\n%s", text)
	}

	// `vars: {}` 是合法的写请求（「清空所有默认值」），与「没写 vars」可区分：
	// 整份文本原样贴回 → 200，且读回来仍是 `vars: {}`。
	put := nsrJSON(t, env.authed(http.MethodPut, path, map[string]any{"yaml": text}), http.StatusOK)
	if again := nsrStr(t, put, "yaml"); !strings.Contains(again, "vars: {}") {
		t.Fatalf("贴回 `vars: {}` 后读面不再是空 vars:\n%s", again)
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
