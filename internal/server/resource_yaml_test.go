package server

// resource_yaml_test.go 覆盖两条「从 YAML 创建」端点（yaml-create 契约 §3.5）：
//
//	POST /v1/namespaces/{ns}/capabilities/yaml
//	POST /v1/namespaces/{ns}/sources/yaml
//
// 夹具全部复用既有环境（newCapTestEnv / newSourceResEnv）：真 store + 真路由表 +
// 真会话中间件，只把**输入的形状**换成「JSON 里装一份 YAML」。这样测到的就是
// 「YAML 层只做文档转换、语义仍是同一条路」这件事本身。
//
// 六类用例（能力 + 源各一套）：
//  1. 成功（多行脚本体落库可读 / webhook 源给出 hookUrl + secretSet）
//  2. 语法错带行号（钉死实测行号，不写 `> 0` 这种会退化的断言）
//  3. 字段非法两种：未知字段（spec.run.fils）与语义非法（entrypoint / targets）
//  4. 凭据不可回显（secret 一个字符都不出现；token 只允许出现在 hookUrl 里，
//     这是 srcReadConfig 的既有语义 —— 见 sourceResAssertNoCredentialLeak）
//  5. 同名冲突（能力折叠复用 / 显式同版本异内容 / 源 409）
//  6. 请求体自身的 400（yaml 缺失 / 空串 / 多余键）与未登录 401

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// capYAMLDoc 造一份 Capability 文档（version 0 = 省略，走内容寻址）。
//
// 脚本体用块标量 `|`：多行字符串是 YAML → JSON 这条链上最容易出错的一段（换行会
// 被 JSON 转义成 `\n`），用多行正文才测得到「贴进去的脚本原样落库」。
func capYAMLDoc(name string, version int, script string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\n")
	b.WriteString("kind: Capability\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + name + "\n")
	if version > 0 {
		b.WriteString("  version: " + strconv.Itoa(version) + "\n")
	}
	b.WriteString("spec:\n")
	b.WriteString("  params: {}\n")
	b.WriteString("  run:\n")
	b.WriteString("    entrypoint: main.sh\n")
	b.WriteString("    files:\n")
	b.WriteString("      main.sh: |\n")
	for _, line := range strings.Split(strings.TrimSuffix(script, "\n"), "\n") {
		b.WriteString("        " + line + "\n")
	}
	return b.String()
}

// srcYAMLDoc 造一份 webhook Source 文档（target 指向既有能力）。
func srcYAMLDoc(name, target, token, secret string) string {
	return "apiVersion: anotify/v1\n" +
		"kind: Source\n" +
		"metadata:\n" +
		"  name: " + name + "\n" +
		"spec:\n" +
		"  kind: webhook\n" +
		"  enabled: true\n" +
		"  config:\n" +
		"    token: " + token + "\n" +
		"    secret: " + secret + "\n" +
		"  targets:\n" +
		"    - capability: " + target + "\n"
}

// yamlBody 是写端点的请求体（`{"yaml": "<全文>"}`）。
func yamlBody(text string) map[string]string { return map[string]string{"yaml": text} }

// causes 取 Status.details.causes[]（缺失即失败：本端点的问题必须带细节）。
func statusCauses(t *testing.T, doc map[string]any) []any {
	t.Helper()
	details, ok := doc["details"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 details（本端点的问题必须带 causes）：%#v", doc)
	}
	causes, ok := details["causes"].([]any)
	if !ok || len(causes) == 0 {
		t.Fatalf("details.causes 缺失或为空：%#v", details)
	}
	return causes
}

// firstCause 取 details.causes[0]。
func firstCause(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	cause, ok := statusCauses(t, doc)[0].(map[string]any)
	if !ok {
		t.Fatalf("details.causes[0] 不是对象：%#v", doc)
	}
	return cause
}

// ---------- 1) 成功 ----------

func TestCapabilityYAMLCreateSuccess(t *testing.T) {
	env := newCapTestEnv(t)
	script := "#!/bin/sh\necho hi from yaml\n"

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", "yaml"), yamlBody(capYAMLDoc("yaml-made", 0, script)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("从 YAML 创建能力期望 201，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	meta := assertCapabilityEnvelope(t, doc, "yaml-made")
	if got := capNumber(t, meta, "version"); got != 1 {
		t.Fatalf("首个版本应是 1，实际 %d", got)
	}
	if got := capString(t, meta, "namespace"); got != env.slugA {
		t.Fatalf("metadata.namespace = %q，want 路径身份（slug）%q", got, env.slugA)
	}

	// 落库可读：includeBody=true 取回正文，证明多行脚本体真的进了 spec.run.files
	// （默认读面剥正文是既有纪律，不是本次改动）。
	rec = env.do(t, http.MethodGet, env.capPath(env.nsA, "yaml-made", "")+"?includeBody=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读回能力期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	got := decodeCapJSON(t, rec)
	files := capObject(t, capObject(t, capObject(t, got["spec"], "spec")["run"], "spec.run")["files"], "spec.run.files")
	if files["main.sh"] != script {
		t.Fatalf("落库的脚本体与 YAML 块标量不一致：%q != %q", files["main.sh"], script)
	}
}

func TestSourceYAMLCreateSuccess(t *testing.T) {
	env := newSourceResEnv(t)

	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(srcYAMLDoc("yaml-hook", "notify", "tok-yaml-1", "sec-yaml-1")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("从 YAML 创建源期望 201，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := env.doc(rec)
	sourceResAssertDoc(t, doc, "yaml-hook", env.nsSlug)

	cfg := sourceResObj(t, sourceResObj(t, doc["spec"])["config"])
	if hook := sourceResString(t, cfg["hookUrl"]); hook != srcHookPathPrefix+"tok-yaml-1" {
		t.Fatalf("spec.config.hookUrl 期望授权读 URL，得到 %v", cfg["hookUrl"])
	}
	if cfg["secretSet"] != true {
		t.Fatalf("spec.config.secretSet 期望 true，得到 %v", cfg["secretSet"])
	}
	sourceResAssertNoCredentialLeak(t, rec.Body.String(), "tok-yaml-1", "sec-yaml-1")
}

// ---------- 2) 语法错带行号 ----------

func TestCapabilityYAMLSyntaxErrorCarriesLine(t *testing.T) {
	env := newCapTestEnv(t)
	// 坏缩进：`files:` 比 `run:` 的其它子键少缩进一格（YAML 里是「期望 block end」）。
	bad := "apiVersion: anotify/v1\n" + // 1
		"kind: Capability\n" + // 2
		"metadata:\n" + // 3
		"  name: broken\n" + // 4
		"spec:\n" + // 5
		"  run:\n" + // 6
		"    entrypoint: main.sh\n" + // 7
		"   files:\n" + // 8 ← 这一行缩进错
		"      main.sh: \"#!/bin/sh\\n\"\n" // 9

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", "yaml"), yamlBody(bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏缩进的 YAML 期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, doc, "reason"); got != "invalid_yaml" {
		t.Fatalf("reason = %q，want invalid_yaml（body=%s）", got, rec.Body.String())
	}
	// 行号钉死为实测的 5：yaml.v3 对「缩进把块提前结束」报的是**外层 mapping 的起始行**
	// （这里是 `spec:`），不是写错的那一行。这个断言的价值是回归哨兵：yamldoc 一旦不再
	// 把行号带出来（或消息前缀形状变了），这里立刻红。
	cause := firstCause(t, doc)
	if msg := capString(t, cause, "message"); !strings.Contains(msg, "第 5 行") {
		t.Fatalf("details.causes[0].message 应带实测行号「第 5 行」，得到 %q", msg)
	}
	// 顶层 message 同样可读（只看 message 的前端也能定位）。
	if msg := capString(t, doc, "message"); !strings.Contains(msg, "第 5 行") {
		t.Fatalf("顶层 message 应带行号，得到 %q", msg)
	}
}

// ---------- 3a) 未知字段 ----------

func TestCapabilityYAMLUnknownFieldPath(t *testing.T) {
	env := newCapTestEnv(t)
	// `spec.run.fils`：契约 §3.5 点名的拼写错。
	bad := "apiVersion: anotify/v1\n" +
		"kind: Capability\n" +
		"metadata:\n" +
		"  name: typo-made\n" +
		"spec:\n" +
		"  run:\n" +
		"    entrypoint: main.sh\n" +
		"    fils: main.sh\n" + // 拼错的键（契约 §3.5 的 spec.run.fils）：必须报出来，不能静默忽略
		"    files:\n" +
		"      main.sh: \"#!/bin/sh\\n\"\n"

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", "yaml"), yamlBody(bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, doc, "reason"); got != "unknown_field" {
		t.Fatalf("reason = %q，want unknown_field（body=%s）", got, rec.Body.String())
	}
	if field := capString(t, firstCause(t, doc), "field"); field != "spec.run.fils" {
		t.Fatalf("details.causes[0].field = %q，want spec.run.fils", field)
	}
}

func TestSourceYAMLUnknownFieldPath(t *testing.T) {
	env := newSourceResEnv(t)
	// `spec.bogus`：spec 层的未声明键，由文档层直接报 unknown_field（带行号）。
	bad := "apiVersion: anotify/v1\n" +
		"kind: Source\n" +
		"metadata:\n" +
		"  name: typo-src\n" +
		"spec:\n" +
		"  kind: webhook\n" +
		"  bogus: 1\n"

	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := env.doc(rec)
	if got := sourceResString(t, doc["reason"]); got != "unknown_field" {
		t.Fatalf("reason = %q，want unknown_field（body=%s）", got, rec.Body.String())
	}
	if field := sourceResString(t, firstCause(t, doc)["field"]); field != "spec.bogus" {
		t.Fatalf("details.causes[0].field = %q，want spec.bogus", field)
	}
}

// ---------- 3b) 语义非法（错误来自 artifact / store，不是文档层） ----------

func TestCapabilityYAMLSemanticInvalid(t *testing.T) {
	env := newCapTestEnv(t)
	// entrypoint 不在 files 里：文档层形状合法，语义层（artifact）才有判据。
	bad := "apiVersion: anotify/v1\n" +
		"kind: Capability\n" +
		"metadata:\n" +
		"  name: no-entry\n" +
		"spec:\n" +
		"  run:\n" +
		"    entrypoint: missing.sh\n" +
		"    files:\n" +
		"      main.sh: \"#!/bin/sh\\n\"\n"

	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", "yaml"), yamlBody(bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("entrypoint 不在 files 里期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if msg := capString(t, doc, "message"); !strings.Contains(msg, "entrypoint") {
		t.Fatalf("message 应点出 entrypoint 字段，得到 %q", msg)
	}
	if _, has := doc["details"]; has {
		t.Fatalf("语义层错误沿用既有的 Status 形状（不带 YAML 的 causes），实际带了 details：%s", rec.Body.String())
	}
}

func TestSourceYAMLSemanticInvalidTarget(t *testing.T) {
	env := newSourceResEnv(t)
	// targets 指向不存在的能力：handler 刻意不预判，由 store/sourcespec 判。
	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(srcYAMLDoc("bad-target", "no-such-cap", "tok-x", "")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("targets 指向不存在的能力期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := env.doc(rec)
	if got := sourceResString(t, doc["reason"]); got != "invalid_source_config" {
		t.Fatalf("reason = %q，want invalid_source_config（body=%s）", got, rec.Body.String())
	}
	if msg := sourceResString(t, doc["message"]); !strings.Contains(msg, "no-such-cap") {
		t.Fatalf("message 应点出坏引用，得到 %q", msg)
	}
}

// ---------- 4) 凭据不可回显 ----------

func TestSourceYAMLCredentialsNeverEchoed(t *testing.T) {
	env := newSourceResEnv(t)
	const (
		token  = "must-not-echo-me"
		secret = "must-not-echo-me-secret"
	)
	rec := env.request(http.MethodPost, env.sourcesPath()+"/yaml", yamlBody(srcYAMLDoc("yaml-cred", "notify", token, secret)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建带凭据的源期望 201，得到 %d：%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("响应体出现了 config.secret 原文：%s", body)
	}
	// token 的纪律与既有读路径逐字一致（srcReadConfig）：它只允许出现在授权读 URL
	// `hookUrl` 里 —— hookUrl 的语义就是「拿这个 URL 可以直接投递」，不是凭据回显。
	sourceResAssertNoCredentialLeak(t, body, token, secret)
	if !strings.Contains(body, `"hookUrl"`) {
		t.Fatalf("响应体应含只读投影 hookUrl：%s", body)
	}
	if !strings.Contains(body, `"secretSet":true`) {
		t.Fatalf("响应体应含只读投影 secretSet=true：%s", body)
	}
	cfg := sourceResObj(t, sourceResObj(t, env.doc(rec)["spec"])["config"])
	for _, key := range []string{"token", "secret"} {
		if _, ok := cfg[key]; ok {
			t.Fatalf("spec.config.%s 被回显了：%#v", key, cfg)
		}
	}

	// 再 GET 一次：同一条脱敏投影（写端点与读端点不能有第二套投影）。
	rec = env.request(http.MethodGet, env.sourcePath("yaml-cred"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("GET 响应体出现了 config.secret 原文：%s", rec.Body.String())
	}
	sourceResAssertNoCredentialLeak(t, rec.Body.String(), token, secret)
	gotCfg := sourceResObj(t, sourceResObj(t, env.doc(rec)["spec"])["config"])
	if gotCfg["secretSet"] != true {
		t.Fatalf("GET 的 secretSet 期望 true，得到 %v", gotCfg["secretSet"])
	}
}

// ---------- 5) 同名冲突 ----------

func TestCapabilityYAMLFoldsSameContent(t *testing.T) {
	env := newCapTestEnv(t)
	docText := capYAMLDoc("folded", 0, "#!/bin/sh\necho fold\n")
	path := env.capPath(env.nsA, "", "yaml")

	first := env.do(t, http.MethodPost, path, yamlBody(docText))
	if first.Code != http.StatusCreated {
		t.Fatalf("首次创建期望 201，得到 %d：%s", first.Code, first.Body.String())
	}
	second := env.do(t, http.MethodPost, path, yamlBody(docText))
	if second.Code != http.StatusCreated {
		t.Fatalf("同内容再创建期望 201（内容寻址折叠复用），得到 %d：%s", second.Code, second.Body.String())
	}
	m1 := capObject(t, decodeCapJSON(t, first)["metadata"], "metadata")
	m2 := capObject(t, decodeCapJSON(t, second)["metadata"], "metadata")
	if capString(t, m1, "uid") != capString(t, m2, "uid") {
		t.Fatalf("同内容两次创建的 uid 不同：%v vs %v", m1["uid"], m2["uid"])
	}
	if capNumber(t, m1, "version") != capNumber(t, m2, "version") {
		t.Fatalf("同内容两次创建的 version 不同：%v vs %v", m1["version"], m2["version"])
	}
}

func TestCapabilityYAMLExplicitVersionConflict(t *testing.T) {
	env := newCapTestEnv(t)
	path := env.capPath(env.nsA, "", "yaml")

	first := env.do(t, http.MethodPost, path, yamlBody(capYAMLDoc("pinned", 1, "#!/bin/sh\necho one\n")))
	if first.Code != http.StatusCreated {
		t.Fatalf("显式版本首次创建期望 201，得到 %d：%s", first.Code, first.Body.String())
	}
	// 显式同版本、内容不同：能力的既有语义是 400 version_content_immutable（不是 409）。
	second := env.do(t, http.MethodPost, path, yamlBody(capYAMLDoc("pinned", 1, "#!/bin/sh\necho two\n")))
	if second.Code != http.StatusBadRequest {
		t.Fatalf("显式同版本异内容期望 400，得到 %d：%s", second.Code, second.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, second), "reason"); got != "version_content_immutable" {
		t.Fatalf("reason = %q，want version_content_immutable（body=%s）", got, second.Body.String())
	}

	// 显式同版本**同内容** → 201 且复用同一行（契约 §1.5 的第三行）。
	third := env.do(t, http.MethodPost, path, yamlBody(capYAMLDoc("pinned", 1, "#!/bin/sh\necho one\n")))
	if third.Code != http.StatusCreated {
		t.Fatalf("显式同版本同内容期望 201（复用），得到 %d：%s", third.Code, third.Body.String())
	}
	m1 := capObject(t, decodeCapJSON(t, first)["metadata"], "metadata")
	m3 := capObject(t, decodeCapJSON(t, third)["metadata"], "metadata")
	if capString(t, m1, "uid") != capString(t, m3, "uid") {
		t.Fatalf("显式同版本同内容应复用同一行，uid 却不同：%v vs %v", m1["uid"], m3["uid"])
	}
}

func TestSourceYAMLDuplicateNameIs409(t *testing.T) {
	env := newSourceResEnv(t)
	path := env.sourcesPath() + "/yaml"
	text := srcYAMLDoc("dup-src", "notify", "tok-dup", "")

	if rec := env.request(http.MethodPost, path, yamlBody(text)); rec.Code != http.StatusCreated {
		t.Fatalf("首次创建期望 201，得到 %d：%s", rec.Code, rec.Body.String())
	}
	// 源同名不做 upsert（UNIQUE(namespace_id,name) 是硬约束）：409 source_name_used。
	rec := env.request(http.MethodPost, path, yamlBody(text))
	if rec.Code != http.StatusConflict {
		t.Fatalf("同名源期望 409，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := sourceResString(t, env.doc(rec)["reason"]); got != "source_name_used" {
		t.Fatalf("reason = %q，want source_name_used（body=%s）", got, rec.Body.String())
	}
}

// ---------- 6) 请求体自身的 400 / 401 ----------

func TestYAMLCreateRequestBodyErrors(t *testing.T) {
	env := newCapTestEnv(t)
	path := env.capPath(env.nsA, "", "yaml")
	good := capYAMLDoc("shape", 0, "#!/bin/sh\n")

	cases := []struct {
		name       string
		body       any
		wantReason string
		wantMsg    string
	}{
		{"缺 yaml 键", map[string]string{}, "BadRequest", "yaml 文档为空"},
		{"yaml 空串", yamlBody(""), "BadRequest", "yaml 文档为空"},
		{"yaml 全空白", yamlBody("  \n\t\n"), "BadRequest", "yaml 文档为空"},
		// 多余键由严格 JSON 解码拒（DisallowUnknownFields），reason 与 JSON 创建同口径。
		{"多余键", map[string]string{"yaml": good, "extra": "1"}, "unknown_field", "extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("期望 400，得到 %d：%s", rec.Code, rec.Body.String())
			}
			doc := decodeCapJSON(t, rec)
			if got := capString(t, doc, "reason"); got != tc.wantReason {
				t.Fatalf("reason = %q，want %q（body=%s）", got, tc.wantReason, rec.Body.String())
			}
			if msg := capString(t, doc, "message"); !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("message = %q，应含 %q", msg, tc.wantMsg)
			}
		})
	}
}

// TestCapabilityYAMLRejectsReadOnlyFields 验 YAML 侧也走同一张 metadata 白名单与同一条
// 「status 只读」规则（契约 §9 D5）：YAML 只是把文档变成 JSON，写边界的判据只有一份。
func TestCapabilityYAMLRejectsReadOnlyFields(t *testing.T) {
	env := newCapTestEnv(t)
	path := env.capPath(env.nsA, "", "yaml")
	base := func(extra string) string {
		return "apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: ro" + extra + "\nspec:\n  run:\n    entrypoint: main.sh\n    files:\n      main.sh: \"#!/bin/sh\\n\"\n"
	}

	// status 写在文档里：形状允许它出现（类型不检查），严格解码按 status_not_writable 拒。
	rec := env.do(t, http.MethodPost, path, yamlBody(base("\nstatus:\n  conditions: []")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带 status 期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if got := capString(t, decodeCapJSON(t, rec), "reason"); got != "status_not_writable" {
		t.Fatalf("reason = %q，want status_not_writable（body=%s）", got, rec.Body.String())
	}

	// metadata.resourceVersion 在 POST 上不合法（创建不套 CAS）：文档层直接 unknown_field。
	rec = env.do(t, http.MethodPost, path, yamlBody(base("\n  resourceVersion: rv1:x:1")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("带 metadata.resourceVersion 期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, doc, "reason"); got != "unknown_field" {
		t.Fatalf("reason = %q，want unknown_field（body=%s）", got, rec.Body.String())
	}
	if field := capString(t, firstCause(t, doc), "field"); field != "metadata.resourceVersion" {
		t.Fatalf("details.causes[0].field = %q，want metadata.resourceVersion", field)
	}
}

// TestYAMLCreateTextLimit 验 `yaml` 文本的 2 MiB 上界（请求体 8 MiB 是另一回事）。
func TestYAMLCreateTextLimit(t *testing.T) {
	env := newCapTestEnv(t)
	rec := env.do(t, http.MethodPost, env.capPath(env.nsA, "", "yaml"), yamlBody(strings.Repeat("a", yamlDocTextLimit+1)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长 yaml 期望 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
	doc := decodeCapJSON(t, rec)
	if got := capString(t, doc, "reason"); got != "BadRequest" {
		t.Fatalf("reason = %q，want BadRequest", got)
	}
	if msg := capString(t, doc, "message"); !strings.Contains(msg, "2 MiB") {
		t.Fatalf("message = %q，应含 2 MiB 上限说明", msg)
	}
}

// TestWriteDocShapeDerivesFromExistingTypes 是「YAML 侧不得手写第二份键清单」的硬探针。
//
// 契约 §3.1 要求 writeDocShape 的顶层键只从 writeRequestMeta 的 json tag 来、metadata
// 键只从 writeMetaAllowList 来。这条测试直接比两张表：只要有人给 YAML 侧加了/减了
// 一个键，或者白名单里出现 objectMeta 里没有的键（会被静默跳过 → YAML 报 unknown_field
// 而 JSON 放行），这里就红。
func TestWriteDocShapeDerivesFromExistingTypes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/capabilities/yaml", nil)
	req.SetPathValue("ns", "default")
	metaKeys := []string{"version", "namespace"}
	shape := writeDocShape(req, reflect.TypeOf(artifact.Spec{}), metaKeys...)

	// 1) 顶层键集 == writeRequestMeta 的 json tag 集。
	wantRoot := map[string]bool{}
	rt := reflect.TypeOf(writeRequestMeta{})
	for i := 0; i < rt.NumField(); i++ {
		wantRoot[jsonFieldName(rt.Field(i))] = true
	}
	if len(shape.Root) != len(wantRoot) {
		t.Fatalf("Root 键数 = %d，want %d（%v）", len(shape.Root), len(wantRoot), shape.Root)
	}
	for key := range wantRoot {
		if _, ok := shape.Root[key]; !ok {
			t.Fatalf("Root 缺 writeRequestMeta 的键 %q：%v", key, shape.Root)
		}
	}

	// 2) spec 用调用方给的类型，status 不检查类型，metadata 用 objectMeta。
	if shape.Root["spec"] != reflect.TypeOf(artifact.Spec{}) {
		t.Fatalf("Root[spec] = %v，want artifact.Spec", shape.Root["spec"])
	}
	if shape.Root["status"] != nil {
		t.Fatalf("Root[status] 应为 nil（存在但不检查类型），得到 %v", shape.Root["status"])
	}
	if shape.Root["metadata"] != reflect.TypeOf(objectMeta{}) {
		t.Fatalf("Root[metadata] = %v，want objectMeta", shape.Root["metadata"])
	}

	// 3) Meta 键集 == JSON 白名单（同一个 writeMetaAllowList）。
	allow := writeMetaAllowList(req, metaKeys...)
	if len(shape.Meta) != len(allow) {
		t.Fatalf("Meta 键数 = %d，want %d（白名单 %v / Meta %v）", len(shape.Meta), len(allow), allow, shape.Meta)
	}
	for key := range allow {
		if _, ok := shape.Meta[key]; !ok {
			t.Fatalf("Meta 缺白名单键 %q（会在对象树里被静默跳过）：%v", key, shape.Meta)
		}
	}
	// 服务端字段不可写：uid / creationTimestamp 不在白名单里。
	for _, key := range []string{"uid", "creationTimestamp"} {
		if _, ok := shape.Meta[key]; ok {
			t.Fatalf("Meta 不应包含服务端字段 %q：%v", key, shape.Meta)
		}
	}

	// 4) Required 只钉 metadata.name。
	if len(shape.Required) != 1 || shape.Required[0] != "metadata.name" {
		t.Fatalf("Required = %v，want [metadata.name]", shape.Required)
	}

	// 5) PATCH 才允许 resourceVersion（与 JSON 路径同一条位置规则）。
	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/namespaces/default/capabilities/x", nil)
	patchReq.SetPathValue("ns", "default")
	if _, ok := writeDocShape(patchReq, reflect.TypeOf(artifact.Spec{}), metaKeys...).Meta["resourceVersion"]; !ok {
		t.Fatalf("PATCH 的 Meta 应允许 resourceVersion")
	}
}

func TestYAMLCreateRequiresSession(t *testing.T) {
	env := newCapTestEnv(t)
	// 不带会话 cookie：新端点必须真的走 session 中间件（未登录 401，不解析正文）。
	req := httptest.NewRequest(http.MethodPost, env.capPath(env.nsA, "", "yaml"), strings.NewReader(`{"yaml":"x"}`))
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录期望 401，得到 %d：%s", rec.Code, rec.Body.String())
	}
}
