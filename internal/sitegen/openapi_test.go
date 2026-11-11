package sitegen

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fixtureSpecDir 是五份具名 v4 spec fixture（testdata/specs）。
//
// 它们**不是**真实 T1 spec：真实 spec 还没落地，测试只证明 parser 对 v4 形状
// （绝对 /v1 路径、servers[/]、deprecated、method+path 合并、五文件内 $ref、
// 协议文档无 paths）的行为；绝不从旧 api/openapi.yaml 生成兜底数据。
const fixtureSpecDir = "testdata/specs"

func writeSpecFile(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write spec %s: %v", name, err)
	}
	return dir
}

func writeSpecDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write spec %s: %v", name, err)
		}
	}
	return dir
}

// opSet 把分组摊平成 "METHOD /path" 集合，顺便校验 Total 与 Ops 一致。
func opSet(t *testing.T, groups []OpenAPIGroup) map[string]int {
	t.Helper()
	out := make(map[string]int)
	for _, g := range groups {
		if g.Total != len(g.Ops) {
			t.Errorf("group %s: Total=%d but %d ops", g.Tag, g.Total, len(g.Ops))
		}
		for _, op := range g.Ops {
			out[op.Method+" "+op.Path]++
		}
	}
	return out
}

func groupOps(groups []OpenAPIGroup, tag string) []OpenAPIOperation {
	for _, g := range groups {
		if g.Tag == tag {
			return g.Ops
		}
	}
	return nil
}

func findOperation(t *testing.T, groups []OpenAPIGroup, method, path string) *OpenAPIOperation {
	t.Helper()
	for gi := range groups {
		for oi := range groups[gi].Ops {
			op := &groups[gi].Ops[oi]
			if op.Method == method && op.Path == path {
				return op
			}
		}
	}
	t.Fatalf("operation %s %s not found in %d groups", method, path, len(groups))
	return nil
}

func findDocRef(t *testing.T, op *OpenAPIOperation, kind, ref string) OpenAPIDocRef {
	t.Helper()
	for _, doc := range op.Docs {
		if doc.Kind == kind && doc.Ref == ref {
			return doc
		}
	}
	t.Fatalf("operation %s %s: doc ref kind=%s ref=%q not found; refs=%+v", op.Method, op.Path, kind, ref, op.Docs)
	return OpenAPIDocRef{}
}

func countDocRefs(op *OpenAPIOperation, kind string) int {
	n := 0
	for _, doc := range op.Docs {
		if doc.Kind == kind {
			n++
		}
	}
	return n
}

// TestCanonicalSpecFiles 钉住五份 canonical spec 的文件名与合并顺序。
// 名字/顺序变了就是契约变了（docs/design/v4/api-conventions.md §2）。
func TestCanonicalSpecFiles(t *testing.T) {
	want := []string{"resource.yaml", "events.yaml", "agent.yaml", "ingress.yaml", "auth.yaml"}
	if !reflect.DeepEqual(CanonicalSpecFiles, want) {
		t.Fatalf("canonical spec files: want %v, got %v", want, CanonicalSpecFiles)
	}
}

// TestLoadSpecDirFixtureSet 用五份具名 fixture 走真实 parser，钉住：
// 全量加载、tag 分组顺序、绝对路径、每文件端点数、同 path 多 method 合并、
// operationId 唯一约束下的有效输入、五文件内 $ref 不丢 operation、无 /v1/v1。
func TestLoadSpecDirFixtureSet(t *testing.T) {
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	if missing := set.Missing(); len(missing) != 0 {
		t.Fatalf("fixture set should have no missing files, got %v", missing)
	}
	if len(set.Files) != len(CanonicalSpecFiles) {
		t.Fatalf("Files: want %d entries, got %d", len(CanonicalSpecFiles), len(set.Files))
	}

	// 每份文件都被真实 parser 读过：端点数按 fixture 内容固定。
	wantPerFile := map[string]int{
		"resource.yaml": 15, // capabilities 5 + sources 2 + playbooks 2 + nodes 2 + namespaces 2 + tasks 2（deprecated DELETE 被过滤）
		"events.yaml":   4,  // events GET（pathItem ref）+ events/{id}（两跳 pathItem 链）+ traces 2
		"agent.yaml":    0,  // 协议文档：没有 HTTP paths，0 operations 合法
		"ingress.yaml":  4,  // events POST（与 events GET 同 path 按 method 合并）+ dry-run + fire + hooks（deprecated /v1/notify 被过滤）
		"auth.yaml":     12, // login/logout/me/session($ref)/keys 2/passkey/cli/admin/devices/system 2（远程 oidc ref 跳过）
	}
	for _, f := range set.Files {
		if !f.Present {
			t.Errorf("spec %s should be present", f.Name)
			continue
		}
		if want, ok := wantPerFile[f.Name]; ok && f.Operations != want {
			t.Errorf("spec %s: want %d operations, got %d", f.Name, want, f.Operations)
		}
	}

	// 组顺序 = 首次出现顺序（resource 声明顺序 → events → ingress → auth 声明顺序）。
	wantOrder := []string{
		"capabilities", "sources", "playbooks", "nodes", "namespaces", "tasks",
		"events", "traces",
		"ingress",
		"auth", "keys", "passkey-enroll", "cli-auth", "admin", "devices", "system",
	}
	var gotOrder []string
	for _, g := range set.Groups {
		gotOrder = append(gotOrder, g.Tag)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("group order:\n want %v\n  got %v", wantOrder, gotOrder)
	}

	// 合并后总端点：35（GET events 在 events.yaml、POST events 在 ingress.yaml，
	// 按 (method,path) 各自保留；没有静默去重）。
	if got := set.OperationCount(); got != 35 {
		t.Errorf("merged operations: want 35, got %d", got)
	}
	if len(set.Duplicates) != 0 {
		t.Errorf("R2 duplicate operation must be a hard error, Duplicates must stay empty, got %v", set.Duplicates)
	}

	ops := opSet(t, set.Groups)
	for _, want := range []string{
		// v4 绝对路径原样保留（servers: / 不拼前缀）
		"GET /v1/namespaces/{namespace}/capabilities",
		"POST /v1/namespaces/{namespace}/capabilities",
		"PATCH /v1/namespaces/{namespace}/capabilities/{name}",
		"GET /v1/nodes",
		"GET /v1/namespaces",
		// 同 path 的两个 method 都必须活着（跨文件合并）。
		"GET /v1/namespaces/{namespace}/events",
		"POST /v1/namespaces/{namespace}/events",
		// pathItem $ref：本地 / 跨文件两跳。
		"GET /v1/namespaces/{namespace}/events/{id}",
		"GET /v1/namespaces/{namespace}/tasks/{name}",
		"GET /v1/namespaces/{namespace}/traces/{traceId}",
		"POST /v1/namespaces/{namespace}/sources/{name}/dry-run",
		"POST /v1/namespaces/{namespace}/sources/{name}/fire",
		"POST /v1/hooks/{token}",
		"GET /v1/auth/me",
		"GET /v1/auth/session", // path item 的本地 $ref 被解析
		"PATCH /v1/system/config",
	} {
		if ops[want] != 1 {
			t.Errorf("operation %q: want exactly once, got %d", want, ops[want])
		}
	}

	// 任何路径都不许二次前缀，也不许漏出旧 /../ 约定。
	for key := range ops {
		path := strings.SplitN(key, " ", 2)[1]
		if strings.Contains(path, "/v1/v1") {
			t.Errorf("double /v1 prefix leaked: %q", path)
		}
		if strings.Contains(path, "/../") {
			t.Errorf("legacy /../ path not normalized: %q", path)
		}
		if !strings.HasPrefix(path, "/v1/") {
			t.Errorf("v4 endpoint must be absolute under /v1: %q", path)
		}
	}

	// 远程 $ref 不解析（不联网）：记 warning、跳过该 path，不影响其它端点。
	if len(set.Warnings) != 1 {
		t.Fatalf("warnings: want 1 (remote pathItem $ref skipped), got %d: %v", len(set.Warnings), set.Warnings)
	}
	if !strings.Contains(set.Warnings[0], "auth.yaml:") || !strings.Contains(set.Warnings[0], "oidc") {
		t.Errorf("warning must name the origin file and the ref, got %q", set.Warnings[0])
	}
	if ops["GET /v1/auth/oidc"] != 0 {
		t.Errorf("remote $ref path must be omitted, got operation")
	}
}

// TestFixtureSamePathTwoMethodsSurvive 钉住 API2 的核心反例：
// GET .../events（events.yaml）与 POST .../events（ingress.yaml）同 path，
// 按 method 合并、两个都保留；重复加载结果确定（顺序/告警完全一致）。
func TestFixtureSamePathTwoMethodsSurvive(t *testing.T) {
	const eventsPath = "/v1/namespaces/{namespace}/events"
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	getOp := findOperation(t, set.Groups, "GET", eventsPath)
	postOp := findOperation(t, set.Groups, "POST", eventsPath)
	if getOp.OperationID != "fixtureListEvents" {
		t.Errorf("GET events operationId: want fixtureListEvents, got %q", getOp.OperationID)
	}
	if postOp.OperationID != "fixturePostEvent" {
		t.Errorf("POST events operationId: want fixturePostEvent, got %q", postOp.OperationID)
	}

	// 同组同 path 的 method 顺序确定：GET 在 POST 前。
	capOps := groupOps(set.Groups, "capabilities")
	if len(capOps) < 2 {
		t.Fatalf("capabilities group: want >=2 ops, got %d", len(capOps))
	}
	if capOps[0].Path != "/v1/namespaces/{namespace}/capabilities" ||
		capOps[0].Method != "GET" ||
		capOps[1].Path != "/v1/namespaces/{namespace}/capabilities" ||
		capOps[1].Method != "POST" {
		t.Errorf("same-path method order must be GET then POST, got %s %s / %s %s",
			capOps[0].Method, capOps[0].Path, capOps[1].Method, capOps[1].Path)
	}

	again, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("second LoadSpecDir: %v", err)
	}
	if !reflect.DeepEqual(set.Groups, again.Groups) {
		t.Errorf("groups are not deterministic across loads")
	}
	if !reflect.DeepEqual(set.Warnings, again.Warnings) {
		t.Errorf("warnings are not deterministic across loads: %v vs %v", set.Warnings, again.Warnings)
	}
}

// TestFixtureCrossFileAndChainedPathItemRefs 钉住 pathItem 的跨文件与两跳链：
// events.yaml#/components/pathItems/EventDetailRef → resource.yaml#/components/pathItems/EventDetail，
// 目标 operation 不得因为引用形式永久消失。
func TestFixtureCrossFileAndChainedPathItemRefs(t *testing.T) {
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	op := findOperation(t, set.Groups, "GET", "/v1/namespaces/{namespace}/events/{id}")
	if op.OperationID != "fixtureGetEvent" {
		t.Errorf("chained pathItem operationId: want fixtureGetEvent, got %q", op.OperationID)
	}
	if !strings.Contains(op.Summary, "Event detail") {
		t.Errorf("chained pathItem summary lost: %q", op.Summary)
	}
}

// TestFixtureOperationDocsReferences 钉住「有效五文件内 ref 不静默消失」：
// parameter / requestBody / response / schema 引用都被保留并可定位，
// 包括 JSON Pointer 转义（~0/~1）与合法自递归 schema（不展开、不丢 operation）。
func TestFixtureOperationDocsReferences(t *testing.T) {
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}

	eventsPath := "/v1/namespaces/{namespace}/events"
	getOp := findOperation(t, set.Groups, "GET", eventsPath)
	param := findDocRef(t, getOp, DocRefParameter, "./resource.yaml#/components/parameters/Namespace")
	if !param.Resolved || param.Origin != "events.yaml" || param.Name != "Namespace" ||
		param.Target != "resource.yaml#/components/parameters/Namespace" {
		t.Errorf("GET events parameter ref: unexpected %+v", param)
	}
	respSchema := findDocRef(t, getOp, DocRefSchema, "#/components/schemas/EventList")
	if !respSchema.Resolved || respSchema.Origin != "events.yaml" || respSchema.Name != "EventList" ||
		respSchema.Target != "events.yaml#/components/schemas/EventList" {
		t.Errorf("GET events response schema ref: unexpected %+v", respSchema)
	}

	postOp := findOperation(t, set.Groups, "POST", eventsPath)
	bodyRef := findDocRef(t, postOp, DocRefRequestBody, "./resource.yaml#/components/requestBodies/SourceEvent")
	if !bodyRef.Resolved || bodyRef.Origin != "ingress.yaml" || bodyRef.Name != "SourceEvent" ||
		bodyRef.Target != "resource.yaml#/components/requestBodies/SourceEvent" {
		t.Errorf("POST events requestBody ref: unexpected %+v", bodyRef)
	}
	bodySchema := findDocRef(t, postOp, DocRefSchema, "#/components/schemas/SourceEventInput")
	if !bodySchema.Resolved || bodySchema.Origin != "resource.yaml" ||
		bodySchema.Target != "resource.yaml#/components/schemas/SourceEventInput" {
		t.Errorf("POST events requestBody schema ref: unexpected %+v", bodySchema)
	}
	respRef := findDocRef(t, postOp, DocRefSchema, "./resource.yaml#/components/schemas/EventReceipt")
	if !respRef.Resolved || respRef.Origin != "ingress.yaml" ||
		respRef.Target != "resource.yaml#/components/schemas/EventReceipt" {
		t.Errorf("POST events response schema ref: unexpected %+v", respRef)
	}
	// parameter 是跨文件直接引用（不是借道 requestBody），也必须保留。
	postParam := findDocRef(t, postOp, DocRefParameter, "./resource.yaml#/components/parameters/Namespace")
	if !postParam.Resolved || postParam.Target != "resource.yaml#/components/parameters/Namespace" {
		t.Errorf("POST events parameter ref: unexpected %+v", postParam)
	}

	// 两跳 pathItem 的文档引用以组件所在文件（resource.yaml）为 origin。
	detailOp := findOperation(t, set.Groups, "GET", "/v1/namespaces/{namespace}/events/{id}")
	detailParam := findDocRef(t, detailOp, DocRefParameter, "#/components/parameters/Namespace")
	if !detailParam.Resolved || detailParam.Origin != "resource.yaml" ||
		detailParam.Target != "resource.yaml#/components/parameters/Namespace" {
		t.Errorf("chained pathItem parameter ref: unexpected %+v", detailParam)
	}
	recursive := findDocRef(t, detailOp, DocRefSchema, "#/components/schemas/Event")
	if !recursive.Resolved || recursive.Origin != "resource.yaml" ||
		recursive.Target != "resource.yaml#/components/schemas/Event" {
		t.Errorf("recursive schema ref must be preserved (not expanded): unexpected %+v", recursive)
	}

	// 响应组件引用 + JSON Pointer 转义：~1 → "/"，~0 → "~"。
	devices := findOperation(t, set.Groups, "GET", "/v1/devices")
	deviceResp := findDocRef(t, devices, DocRefResponse, "#/components/responses/Device~1List")
	if !deviceResp.Resolved || deviceResp.Name != "Device/List" ||
		deviceResp.Target != "auth.yaml#/components/responses/Device~1List" {
		t.Errorf("escaped response ref (~1): unexpected %+v", deviceResp)
	}
	deviceSchema := findDocRef(t, devices, DocRefSchema, "#/components/schemas/Device~0Key")
	if !deviceSchema.Resolved || deviceSchema.Name != "Device~Key" ||
		deviceSchema.Target != "auth.yaml#/components/schemas/Device~0Key" {
		t.Errorf("escaped schema ref (~0): unexpected %+v", deviceSchema)
	}

	// 跨文件 response schema 引用（traces → resource）也不消失。
	traceOp := findOperation(t, set.Groups, "GET", "/v1/namespaces/{namespace}/traces/{traceId}")
	traceSchema := findDocRef(t, traceOp, DocRefSchema, "./resource.yaml#/components/schemas/Trace")
	if !traceSchema.Resolved || traceSchema.Target != "resource.yaml#/components/schemas/Trace" {
		t.Errorf("cross-file trace schema ref: unexpected %+v", traceSchema)
	}
}

// TestLoadSpecDirDeprecatedFiltered 确认 deprecated 的 operation 不进任何组。
func TestLoadSpecDirDeprecatedFiltered(t *testing.T) {
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	ops := opSet(t, set.Groups)
	for _, gone := range []string{
		"DELETE /v1/namespaces/{namespace}/tasks/{name}", // resource.yaml deprecated
		"POST /v1/notify", // ingress.yaml deprecated 旧入口
	} {
		if ops[gone] != 0 {
			t.Errorf("deprecated operation %q leaked into docs", gone)
		}
	}
	// 同路径的未弃用方法必须保留。
	if ops["GET /v1/namespaces/{namespace}/tasks/{name}"] != 1 {
		t.Errorf("non-deprecated sibling of a deprecated op disappeared")
	}
}

// TestLoadSpecDirAgentProtocolNoPaths：协议文档没有 HTTP paths 是合法输入。
func TestLoadSpecDirAgentProtocolNoPaths(t *testing.T) {
	set, err := LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir with protocol-only agent.yaml must not error: %v", err)
	}
	var agentFile *SpecFile
	for i := range set.Files {
		if set.Files[i].Name == "agent.yaml" {
			agentFile = &set.Files[i]
		}
	}
	if agentFile == nil || !agentFile.Present || agentFile.Operations != 0 {
		t.Fatalf("agent.yaml: want present with 0 operations, got %+v", agentFile)
	}
	for _, g := range set.Groups {
		if g.Tag == "agent" && len(g.Ops) > 0 {
			t.Errorf("protocol-only spec must not fabricate endpoints, got %+v", g.Ops)
		}
	}
}

// TestLoadOpenAPIGroupsProtocolNoPaths：单文件入口对 0 paths 同样不报错。
func TestLoadOpenAPIGroupsProtocolNoPaths(t *testing.T) {
	dir := writeSpecFile(t, "agent.yaml", `openapi: 3.0.3
info: { title: protocol, version: "4" }
tags: [ { name: agent } ]
`)
	groups, err := LoadOpenAPIGroups(filepath.Join(dir, "agent.yaml"))
	if err != nil {
		t.Fatalf("protocol spec without paths must parse, got error: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("protocol spec should yield no groups, got %+v", groups)
	}
}

// TestLoadSpecDirServerPrefixNoDoubleV1 钉住 servers 前缀规则：
// v4 的 servers[/] + 绝对路径原样；旧 servers[/v1] + 相对/绝对路径都不二次拼接。
func TestLoadSpecDirServerPrefixNoDoubleV1(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: /v1 } ]
paths:
  /events:
    get: {}
  /v1/namespaces/{namespace}/capabilities:
    get: {}
`,
		"events.yaml": `openapi: 3.0.3
servers: [ { url: https://api.example.com/v1 } ]
paths:
  /v1/namespaces/{namespace}/events:
    get: {}
`,
		"ingress.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /../agent-login.sh:
    get: {}
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	ops := opSet(t, set.Groups)
	for _, want := range []string{
		"GET /v1/events", // 相对路径 + /v1 前缀
		"GET /v1/namespaces/{namespace}/capabilities", // 绝对路径不二次拼接
		"GET /v1/namespaces/{namespace}/events",       // 绝对 URL 前缀 + 绝对路径
		"GET /agent-login.sh",                         // /../ 旧约定 → 根路径
	} {
		if ops[want] != 1 {
			t.Errorf("operation %q: want once, got %d (all ops: %v)", want, ops[want], ops)
		}
	}
	for key := range ops {
		if strings.Contains(key, "/v1/v1") {
			t.Errorf("double prefix leaked: %q", key)
		}
	}
}

func TestServerPrefix(t *testing.T) {
	cases := []struct {
		servers []openAPIServer
		want    string
	}{
		{nil, ""},
		{[]openAPIServer{{URL: "/"}}, ""},
		{[]openAPIServer{{URL: "/v1"}}, "/v1"},
		{[]openAPIServer{{URL: "/v1/"}}, "/v1"},
		{[]openAPIServer{{URL: "https://api.example.com/v1"}}, "/v1"},
		{[]openAPIServer{{URL: "https://api.example.com"}}, ""},
		{[]openAPIServer{{URL: ""}, {URL: "/v1"}}, "/v1"},
	}
	for _, c := range cases {
		if got := serverPrefix(c.servers); got != c.want {
			t.Errorf("serverPrefix(%+v): want %q, got %q", c.servers, c.want, got)
		}
	}
}

// TestLoadSpecDirPartialMissing：部分缺失 = 具名缺失清单 + 其余正常合并，不是错误。
// （现存文件若引用缺失的 canonical 文件，则按「坏 ref」报错——见下。）
func TestLoadSpecDirPartialMissing(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/namespaces/{namespace}/capabilities:
    get: {}
`,
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/namespaces/{namespace}/events:
    get: {}
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("partial missing must not error: %v", err)
	}
	wantMissing := []string{"agent.yaml", "ingress.yaml", "auth.yaml"}
	if !reflect.DeepEqual(set.Missing(), wantMissing) {
		t.Fatalf("missing: want %v, got %v", wantMissing, set.Missing())
	}
	if got := set.OperationCount(); got != 2 {
		t.Errorf("merged operations from present specs: want 2, got %d", got)
	}
}

// TestLoadSpecDirAllMissing：全缺失（空目录 / 目录不存在）是合法空文档。
func TestLoadSpecDirAllMissing(t *testing.T) {
	for name, dir := range map[string]string{
		"empty dir":       t.TempDir(),
		"nonexistent dir": filepath.Join(t.TempDir(), "does-not-exist"),
	} {
		set, err := LoadSpecDir(dir)
		if err != nil {
			t.Fatalf("%s: all-missing must not error: %v", name, err)
		}
		if got := set.Missing(); !reflect.DeepEqual(got, CanonicalSpecFiles) {
			t.Errorf("%s: missing: want all five %v, got %v", name, CanonicalSpecFiles, got)
		}
		if set.OperationCount() != 0 {
			t.Errorf("%s: want 0 operations, got %d", name, set.OperationCount())
		}
		if set.Groups == nil {
			t.Errorf("%s: Groups must be an empty non-nil slice", name)
		}
	}
}

// TestLoadSpecDirMalformed：已存在但无效的 spec 是硬错误（不是空态），且要具名。
func TestLoadSpecDirMalformed(t *testing.T) {
	cases := map[string]string{
		"bad yaml": `
openapi: 3.0.3
paths: [unclosed
`,
		"paths not a mapping": `openapi: 3.0.3
paths:
  /v1/x: "not-a-path-item"
`,
	}
	for name, body := range cases {
		dir := writeSpecDir(t, map[string]string{"auth.yaml": body})
		_, err := LoadSpecDir(dir)
		if err == nil {
			t.Errorf("%s: malformed spec must error", name)
			continue
		}
		if !strings.Contains(err.Error(), "auth.yaml") {
			t.Errorf("%s: error must name the offending file, got %v", name, err)
		}
	}

	// 目录本身不是目录也必须是错误（不是「全缺失」）。
	fileDir := writeSpecFile(t, "resource.yaml", "openapi: 3.0.3\n")
	if _, err := LoadSpecDir(filepath.Join(fileDir, "resource.yaml")); err == nil {
		t.Error("spec dir pointing at a regular file must error")
	}
}

// TestOpenAPIPathItemRef：本地 path item $ref 被解析；远程 $ref 跳过并 warn
// （不联网、不 fallback、不在没有 warning 的情况下丢 operation）。
func TestOpenAPIPathItemRef(t *testing.T) {
	dir := writeSpecFile(t, "auth.yaml", `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: '#/components/pathItems/X'
  /v1/y:
    $ref: 'https://example.com/y.yaml#/paths/~1y'
components:
  pathItems:
    X:
      get: {}
`)
	groups, warnings, err := loadSpecFile(filepath.Join(dir, "auth.yaml"))
	if err != nil {
		t.Fatalf("loadSpecFile: %v", err)
	}
	ops := opSet(t, groups)
	if ops["GET /v1/x"] != 1 {
		t.Errorf("local path-item $ref should resolve to GET /v1/x, got %v", ops)
	}
	if ops["GET /v1/y"] != 0 {
		t.Errorf("remote path-item $ref must not fabricate an operation, got %v", ops)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "auth.yaml") ||
		!strings.Contains(warnings[0], "remote") || !strings.Contains(warnings[0], "https://example.com/y.yaml") {
		t.Errorf("remote $ref should warn with origin+ref, got %v", warnings)
	}
}

// TestPathItemRefCrossFileResolves：跨 canonical 文件的 pathItem 引用直接可用。
func TestPathItemRefCrossFileResolves(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/cross:
    $ref: './resource.yaml#/components/pathItems/Cross'
`,
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
components:
  pathItems:
    Cross:
      get:
        tags: [events]
        operationId: crossFileOp
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("cross-file pathItem ref must resolve: %v", err)
	}
	op := findOperation(t, set.Groups, "GET", "/v1/cross")
	if op.OperationID != "crossFileOp" {
		t.Errorf("cross-file pathItem operationId: want crossFileOp, got %q", op.OperationID)
	}
}

// TestPathItemRefOtherLocationEscapedToken：pathItem 引用目标不限于
// components.pathItems —— 其它合法可定位位置（如另一文件的 paths）也能定位，
// 且路径键里的 JSON Pointer 转义（~1 → "/"）不能靠字符串截取猜。
func TestPathItemRefOtherLocationEscapedToken(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/alias:
    $ref: './resource.yaml#/paths/~1v1~1y'
`,
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/y:
    get:
      tags: [events]
      summary: aliased target
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("pathItem ref into another file's paths must resolve: %v", err)
	}
	op := findOperation(t, set.Groups, "GET", "/v1/alias")
	if op.Summary != "aliased target" {
		t.Errorf("escaped path-item location: summary not carried over, got %q", op.Summary)
	}
	// 原 path 与别名 path 是两条不同 (method,path)，都必须保留。
	if found := findOperation(t, set.Groups, "GET", "/v1/y"); found == nil {
		t.Error("original path item must still be rendered")
	}
}

// TestPathItemRefChainCycleBounded：pathItem 引用链循环必须是有界错误。
func TestPathItemRefChainCycleBounded(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: '#/components/pathItems/A'
components:
  pathItems:
    A:
      $ref: '#/components/pathItems/B'
    B:
      $ref: '#/components/pathItems/A'
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("pathItem reference cycle must be an error")
	}
	for _, want := range []string{"events.yaml", "cycle", "pathItems/A", "pathItems/B"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cycle error must contain %q, got %v", want, err)
		}
	}
}

// TestPathItemRefChainDepthBounded：超长 pathItem 引用链必须报有界错误，不能挂起。
func TestPathItemRefChainDepthBounded(t *testing.T) {
	const n = 20 // > maxPathItemRefHops
	var b strings.Builder
	b.WriteString("openapi: 3.0.3\nservers: [ { url: / } ]\npaths:\n  /v1/x:\n    $ref: '#/components/pathItems/A0'\ncomponents:\n  pathItems:\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "    A%d:\n", i)
		if i == n-1 {
			b.WriteString("      get:\n        tags: [events]\n")
			continue
		}
		fmt.Fprintf(&b, "      $ref: '#/components/pathItems/A%d'\n", i+1)
	}
	dir := writeSpecDir(t, map[string]string{"events.yaml": b.String()})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("over-long pathItem reference chain must be an error")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "events.yaml") {
		t.Errorf("depth error must mention the bound and origin, got %v", err)
	}
}

// TestPathItemRefBadPointer：本地指针不存在是硬错误，且带 origin+path+ref。
func TestPathItemRefBadPointer(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: '#/components/pathItems/Nope'
components:
  pathItems:
    Other:
      get: {}
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("missing local pathItem pointer must be an error")
	}
	for _, want := range []string{"events.yaml", "/v1/x", "#/components/pathItems/Nope", "Nope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bad pointer error must contain %q, got %v", want, err)
		}
	}
}

// TestPathItemRefMissingCanonicalFile：引用五文件内缺失的 spec 是硬错误。
func TestPathItemRefMissingCanonicalFile(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: './resource.yaml#/components/pathItems/X'
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("pathItem ref to an absent canonical spec must be an error")
	}
	for _, want := range []string{"events.yaml", "./resource.yaml#/components/pathItems/X", "not present"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing-file error must contain %q, got %v", want, err)
		}
	}
}

// TestPathItemRefBoundaryRejected：绝对路径 / 父目录逃逸 / 第六份文件
// （含旧 openapi.yaml）都不允许，报错要带具体 origin+ref。
func TestPathItemRefBoundaryRejected(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"parent escape", "../outside.yaml#/components/pathItems/X", "parent-directory escape"},
		{"absolute path", "/etc/passwd#/components/pathItems/X", "absolute paths"},
		{"sixth source", "other.yaml#/components/pathItems/X", "not one of the five canonical spec files"},
		{"legacy single spec", "./openapi.yaml#/components/pathItems/X", "not one of the five canonical spec files"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeSpecDir(t, map[string]string{
				"events.yaml": fmt.Sprintf(`openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: '%s'
`, c.ref),
			})
			_, err := LoadSpecDir(dir)
			if err == nil {
				t.Fatalf("ref %q must be rejected", c.ref)
			}
			for _, want := range []string{"events.yaml", c.ref, c.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must contain %q, got %v", want, err)
				}
			}
		})
	}
}

// TestJSONPointerBadEscapeRejected：非法 ~ 转义（只允许 ~0/~1）是明确错误。
func TestJSONPointerBadEscapeRejected(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    $ref: '#/components/pathItems/Bad~2Name'
components:
  pathItems:
    Bad~2Name:
      get: {}
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("invalid ~ escape must be an error")
	}
	for _, want := range []string{"events.yaml", "invalid ~ escape", "Bad~2Name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bad escape error must contain %q, got %v", want, err)
		}
	}
}

// TestJSONPointerEscapeDecode：~1 → "/"、~0 → "~" 的组件键必须真的被定位到。
func TestJSONPointerEscapeDecode(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
tags: [ { name: events } ]
paths:
  /v1/x:
    get:
      tags: [events]
      responses:
        "200":
          $ref: '#/components/responses/Trace~1Node'
components:
  responses:
    "Trace/Node":
      description: escaped
      content:
        application/json:
          schema:
            $ref: '#/components/schemas/Trace~0Node'
  schemas:
    "Trace~Node":
      type: object
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("escaped JSON pointer must resolve: %v", err)
	}
	op := findOperation(t, set.Groups, "GET", "/v1/x")
	resp := findDocRef(t, op, DocRefResponse, "#/components/responses/Trace~1Node")
	if !resp.Resolved || resp.Name != "Trace/Node" {
		t.Errorf("response escaped ref: unexpected %+v", resp)
	}
	schema := findDocRef(t, op, DocRefSchema, "#/components/schemas/Trace~0Node")
	if !schema.Resolved || schema.Name != "Trace~Node" {
		t.Errorf("schema escaped ref: unexpected %+v", schema)
	}
}

// TestDeepRefProblemsWarnKeepOperation：operation 内部（parameter/requestBody/
// response/schema）坏引用是 warning，不是 error；operation 必须保留，
// 结构化引用以 Resolved=false 如实记录。
func TestDeepRefProblemsWarnKeepOperation(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    get:
      tags: [events]
      parameters:
        - $ref: './resource.yaml#/components/parameters/X'
      requestBody:
        $ref: './other.yaml#/components/requestBodies/Y'
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: 'https://example.com/s.yaml#/X'
`,
	})
	set, err := LoadSpecDir(dir)
	if err != nil {
		t.Fatalf("deep bad refs must warn, not fail the build: %v", err)
	}
	op := findOperation(t, set.Groups, "GET", "/v1/x")
	if len(set.Warnings) != 3 {
		t.Fatalf("want 3 deep-ref warnings, got %d: %v", len(set.Warnings), set.Warnings)
	}
	for _, warning := range set.Warnings {
		if !strings.Contains(warning, "events.yaml") || !strings.Contains(warning, "operation GET /v1/x") {
			t.Errorf("deep-ref warning must carry origin+operation, got %q", warning)
		}
	}
	if countDocRefs(op, DocRefParameter) != 1 || countDocRefs(op, DocRefRequestBody) != 1 ||
		countDocRefs(op, DocRefSchema) != 1 {
		t.Errorf("unresolved refs must still be preserved structurally, got %+v", op.Docs)
	}
	for _, doc := range op.Docs {
		if doc.Resolved {
			t.Errorf("bad deep ref must be marked unresolved, got %+v", doc)
		}
	}
}

// TestDuplicateMethodPathIsError：同 (method,path) 跨文件重复是硬错误，
// 不再静默保留首次出现。
func TestDuplicateMethodPathIsError(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    get:
      tags: [a]
`,
		"events.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/x:
    get:
      tags: [b]
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("duplicate method+path must be a hard error")
	}
	for _, want := range []string{"duplicate operation GET /v1/x", "resource.yaml", "events.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("duplicate error must contain %q, got %v", want, err)
		}
	}
}

// TestDuplicateOperationIDIsError：显式提供的 operationId 跨五文件全局唯一，
// 重复是硬错误（不发明名字、不 fallback）。
func TestDuplicateOperationIDIsError(t *testing.T) {
	dir := writeSpecDir(t, map[string]string{
		"resource.yaml": `openapi: 3.0.3
servers: [ { url: / } ]
paths:
  /v1/a:
    get:
      tags: [a]
      operationId: dup
  /v1/b:
    get:
      tags: [a]
      operationId: dup
`,
	})
	_, err := LoadSpecDir(dir)
	if err == nil {
		t.Fatal("duplicate operationId must be a hard error")
	}
	for _, want := range []string{`duplicate operationId "dup"`, "GET /v1/a", "GET /v1/b", "resource.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("duplicate operationId error must contain %q, got %v", want, err)
		}
	}
}

// TestDerivedAPITagLabel 钉住未知 tag 的派生标签（语言中立、非裸 key）。
func TestDerivedAPITagLabel(t *testing.T) {
	cases := map[string]string{
		"telemetry":   "Telemetry",
		"node-stream": "Node Stream",
		"cli_auth":    "Cli Auth",
		"events flow": "Events Flow",
		"":            "API",
	}
	for in, want := range cases {
		if got := derivedAPITagLabel(in); got != want {
			t.Errorf("derivedAPITagLabel(%q): want %q, got %q", in, want, got)
		}
	}
}

// writeTagSite 造一个只渲染 tag 标题的最小站点（4 语）。
func writeTagSite(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "layouts", "base.html"),
		`<html lang="{{.Lang}}"><body>{{block "content" .}}{{end}}</body></html>`)
	mustWrite(t, filepath.Join(src, "pages", "tags.html"),
		`{{define "content"}}{{range .OpenAPI}}<span class="tag" data-tag="{{.Tag}}">{{t (printf "docs.openapi.tag.%s" .Tag)}}</span>{{end}}{{end}}`)
	labels := map[string]string{
		"zh-CN": "已知",
		"en":    "Known",
		"ja":    "既知",
		"es":    "Conocido",
	}
	for lang, label := range labels {
		mustWrite(t, filepath.Join(src, "locales", lang+".yaml"),
			"docs:\n  openapi:\n    tag:\n      known: \""+label+"\"\n")
	}
	return src
}

// TestUnknownTagFourLanguageRenderParity：未知 tag 在 4 语渲染里都不是裸 key，
// 且四语输出一致（派生标签语言中立）；已知 tag 各语言用各自词条。
func TestUnknownTagFourLanguageRenderParity(t *testing.T) {
	src := writeTagSite(t)
	specDir := writeSpecFile(t, "resource.yaml", `openapi: 3.0.3
servers: [ { url: / } ]
tags:
  - name: known
  - name: telemetry
paths:
  /v1/known:
    get:
      tags: [known]
  /v1/telemetry:
    get:
      tags: [telemetry]
`)
	out := t.TempDir()
	langs := []string{"zh-CN", "en", "ja", "es"}
	gen, err := New(src, out, langs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := gen.LoadSpecDir(specDir); err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	wantKnown := map[string]string{"zh-CN": "已知", "en": "Known", "ja": "既知", "es": "Conocido"}
	for _, lang := range langs {
		dir := out
		if lang != "zh-CN" {
			dir = filepath.Join(out, lang)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "tags.html"))
		if err != nil {
			t.Fatalf("read %s tags.html: %v", lang, err)
		}
		page := string(raw)
		if strings.Contains(page, "docs.openapi.tag.") {
			t.Errorf("%s: unknown tag rendered a bare i18n key:\n%s", lang, page)
		}
		if !strings.Contains(page, ">"+wantKnown[lang]+"<") {
			t.Errorf("%s: known tag should use locale label %q, got:\n%s", lang, wantKnown[lang], page)
		}
		// 派生标签四语一致：Telemetry（而不是某种伪造翻译）。
		if !strings.Contains(page, ">Telemetry<") {
			t.Errorf("%s: unknown tag should render derived label Telemetry, got:\n%s", lang, page)
		}
	}
}

// TestDocsPageRendersFiveSpecFixturesFourLanguages 是端到端 parity：
// 用**真实的** web-src docs.html + 真实 parser + 五份 fixture spec 生成 4 语，
// 断言 docs 页出现全部端点/分组，且没有任何裸 tag key。
func TestDocsPageRendersFiveSpecFixturesFourLanguages(t *testing.T) {
	srcDir := filepath.Join("..", "..", "web-src")
	outDir := t.TempDir()
	langs := []string{"zh-CN", "en", "ja", "es"}
	gen, err := New(srcDir, outDir, langs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	set, err := gen.LoadSpecDir(fixtureSpecDir)
	if err != nil {
		t.Fatalf("LoadSpecDir(fixtures): %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	wantOps := set.OperationCount()
	if wantOps == 0 || wantOps != 35 {
		t.Fatalf("fixture set must expose 35 operations, got %d", wantOps)
	}
	wantGroups := len(set.Groups)
	if wantGroups != 16 {
		t.Fatalf("fixture set must expose 16 tag groups, got %d", wantGroups)
	}

	for _, lang := range langs {
		dir := outDir
		if lang != "zh-CN" {
			dir = filepath.Join(outDir, lang)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "docs.html"))
		if err != nil {
			t.Fatalf("read %s docs.html: %v", lang, err)
		}
		page := string(raw)
		if strings.Contains(page, "docs.openapi.tag.") {
			t.Errorf("%s: docs page contains a bare tag i18n key", lang)
		}
		if got := strings.Count(page, `class="doc-ep-path"`); got != wantOps {
			t.Errorf("%s: want %d endpoint rows, got %d", lang, wantOps, got)
		}
		if got := strings.Count(page, `class="doc-ep-table"`); got != wantGroups {
			t.Errorf("%s: want %d tag groups, got %d", lang, wantGroups, got)
		}
		for _, path := range []string{
			"/v1/namespaces/{namespace}/capabilities",
			"/v1/namespaces/{namespace}/sources/{name}/fire",
			"/v1/auth/session",
			// 同 path 两 method 都在页面上（GET events / POST events）。
			"/v1/namespaces/{namespace}/events",
		} {
			if !strings.Contains(page, path) {
				t.Errorf("%s: docs page missing absolute path %s", lang, path)
			}
		}
		// 空态不能和卡片同时出现。
		if strings.Contains(page, `class="doc-empty-title"`) {
			t.Errorf("%s: docs page rendered empty state alongside endpoints", lang)
		}
	}
}

// TestDocsPageNamespacePlaceholderEquivalentRenders 钉住 R1 的 HTML 门控：
// docs.html 以「非空 .OpenAPI」为渲染条件，不是字面 {namespace} 路径匹配；
// 占位名不同（{ns}/{workspace}）的合法端点仍必须出现在页面上。
func TestDocsPageNamespacePlaceholderEquivalentRenders(t *testing.T) {
	srcDir := filepath.Join("..", "..", "web-src")
	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "en", "ja", "es"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	specDir := writeSpecFile(t, "resource.yaml", `openapi: 3.0.3
servers: [ { url: / } ]
tags: [ { name: capabilities } ]
paths:
  /v1/namespaces/{ns}/capabilities:
    get:
      tags: [capabilities]
  /v1/namespaces/{workspace}/sources:
    get:
      tags: [capabilities]
`)
	if _, err := gen.LoadSpecDir(specDir); err != nil {
		t.Fatalf("LoadSpecDir: %v", err)
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for _, lang := range []string{"zh-CN", "en", "ja", "es"} {
		dir := outDir
		if lang != "zh-CN" {
			dir = filepath.Join(outDir, lang)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "docs.html"))
		if err != nil {
			t.Fatalf("read %s docs.html: %v", lang, err)
		}
		page := string(raw)
		if got := strings.Count(page, `class="doc-ep-path"`); got != 2 {
			t.Errorf("%s: want 2 endpoint rows for alternate placeholders, got %d", lang, got)
		}
		for _, path := range []string{"/v1/namespaces/{ns}/capabilities", "/v1/namespaces/{workspace}/sources"} {
			if !strings.Contains(page, path) {
				t.Errorf("%s: alternate namespace placeholder path %s missing from docs page", lang, path)
			}
		}
		if strings.Contains(page, `class="doc-empty-title"`) {
			t.Errorf("%s: nonempty OpenAPI must not render the empty state", lang)
		}
	}
}

// TestDocsPageAllSpecsMissingEmptyState：五份全缺时 docs 页是真实空态。
func TestDocsPageAllSpecsMissingEmptyState(t *testing.T) {
	srcDir := filepath.Join("..", "..", "web-src")
	outDir := t.TempDir()
	gen, err := New(srcDir, outDir, []string{"zh-CN", "en", "ja", "es"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	set, err := gen.LoadSpecDir(t.TempDir()) // 空目录 = 五份全缺
	if err != nil {
		t.Fatalf("all-missing spec dir must not error: %v", err)
	}
	if len(set.Missing()) != len(CanonicalSpecFiles) {
		t.Fatalf("want five missing specs, got %v", set.Missing())
	}
	if err := gen.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "docs.html"))
	if err != nil {
		t.Fatalf("read docs.html: %v", err)
	}
	page := string(raw)
	if strings.Contains(page, `class="doc-ep-path"`) {
		t.Errorf("empty docs must not render endpoint rows")
	}
	if !strings.Contains(page, `class="doc-empty-title"`) {
		t.Errorf("empty docs must render the real empty state")
	}
	if strings.Contains(page, "docs.v4.missing") {
		t.Errorf("empty state must not leak a bare i18n key")
	}
}
