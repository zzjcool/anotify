package server

// Playbook 资源域的 HTTP 验收（W3 冻结形状 + playbook.md §6/§7）。
//
// 这一组用例只断言**传输面**（信封投影、CAS、冲突 0 写、删除只删清单），
// 领域语义（12 格矩阵、回执内容）由 internal/playbook 与 internal/store 的用例覆盖：
// 两层各测各的，避免在 handler 测试里再抄一遍矩阵。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
	"github.com/zzjcool/anotify/internal/store"
)

// ---------- 测试环境 ----------

// playbookTestEnv 装配「内存库 + 一个已登录用户 + 只含 Playbook 路由的 mux」。
//
// 用独立的 ServeMux 而不是整机 NewApp：本域只对自己的路由与投影负责，
// 整机装配是 W-asm 的 mux.go（并行开发期它可能还在迁移中）。
type playbookTestEnv struct {
	mux  http.Handler
	db   *store.DB
	nsID string
	// nsSlug 是路径与 metadata.namespace 上的对外名字（store 事实表引用 nsID）。
	nsSlug string
	cookie *http.Cookie
}

func newPlaybookTestEnv(t *testing.T) *playbookTestEnv {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// RegisterUser 一步建出「用户 + 默认命名空间」（无组织层），ns.ID 就是 URL 里的 ns。
	u := &store.User{
		ID: store.NewUserID(), Username: "playbook-owner", DisplayName: "包主",
		Role: store.RoleAdmin, CreatedAt: store.Now(),
	}
	ns, err := db.RegisterUser(context.Background(), u)
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	sm := auth.NewSessionManager(db, 0, false)
	sess, err := sm.Create(u.ID, "playbook test")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	mux := http.NewServeMux()
	newPlaybookAPI(&serverDeps{DB: db, Now: store.Now, Session: sm.Middleware}).register(mux)
	return &playbookTestEnv{
		mux:    mux,
		db:     db,
		nsID:   ns.ID,
		nsSlug: ns.Slug,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: sess.ID},
	}
}

// path 拼出本域的 URL（ns 从环境取）。
func (e *playbookTestEnv) path(suffix string) string {
	return "/v1/namespaces/" + e.nsID + "/playbooks" + suffix
}

// do 发一个带会话的请求；body 为 map 时按 JSON 编码，为 string 时原样发出（造坏 body 用）。
func (e *playbookTestEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	switch v := body.(type) {
	case nil:
	case string:
		buf.WriteString(v)
	default:
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.AddCookie(e.cookie)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

// capCount / srcCount 数命名空间里真实存在的行（断言「0 写」用）。
func (e *playbookTestEnv) capCount(t *testing.T) int {
	t.Helper()
	caps, err := e.db.ListCapabilities(context.Background(), e.nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	return len(caps)
}

func (e *playbookTestEnv) srcCount(t *testing.T) int {
	t.Helper()
	srcs, err := e.db.ListSources(context.Background(), e.nsID)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	return len(srcs)
}

// installCount 数回执行（playbook_installs）——dryRun / render 都不许写它。
func (e *playbookTestEnv) installCount(t *testing.T, slug string) int {
	t.Helper()
	row, err := e.db.GetPlaybook(context.Background(), e.nsID, slug)
	if err != nil {
		t.Fatalf("get playbook %q: %v", slug, err)
	}
	list, err := e.db.ListPlaybookInstalls(context.Background(), e.nsID, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	return list.Total
}

// ---------- 响应/DTO ----------

type pbTestStatus struct {
	Ready       bool    `json:"ready"`
	Unapplied   []any   `json:"unapplied"`
	Total       int     `json:"total"`
	Conditions  []any   `json:"conditions"`
	LastInstall string  `json:"lastInstallId"`
	Source      *string `json:"source"`
	Items       []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Version int    `json:"version"`
		State   string `json:"state"`
	} `json:"items"`
	Sources struct {
		Total     int `json:"total"`
		Unapplied int `json:"unapplied"`
	} `json:"sources"`
}

type pbTestEnvelope struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		UID               string `json:"uid"`
		Version           *int   `json:"version"`
		ResourceVersion   string `json:"resourceVersion"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec   map[string]json.RawMessage `json:"spec"`
	Status pbTestStatus               `json:"status"`
}

type pbTestList struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Items      []pbTestEnvelope `json:"items"`
}

type pbTestStatusDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Code       int    `json:"code"`
	Reason     string `json:"reason"`
	Message    string `json:"message"`
	Details    struct {
		ValuesDigest string `json:"valuesDigest"`
		Hint         string `json:"hint"`
		Current      struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"current"`
		Conflicts []struct {
			Kind           string   `json:"kind"`
			Ref            string   `json:"ref"`
			Reason         string   `json:"reason"`
			DeclaredDigest string   `json:"declaredDigest"`
			ActualDigest   string   `json:"actualDigest"`
			Fields         []string `json:"fields"`
		} `json:"conflicts"`
	} `json:"details"`
}

// ---------- 包体夹具（前缀 pbTest，避免与其它 worker 的测试夹具重名） ----------

const pbTestValuesBlock = `    type: object
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }`

// pbTestEntryDoc 组装入口文档 playbook.yaml（包内唯一根清单）。
func pbTestEntryDoc(slug string, version int, caps, srcs []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n  name: " + slug + "\n")
	b.WriteString("  displayName: 传输面测试包\n")
	if version > 0 {
		b.WriteString("  version: " + strconv.Itoa(version) + "\n")
	}
	b.WriteString("spec:\n  values:\n" + pbTestValuesBlock + "\n")
	writeKey := func(key string, paths []string) {
		if len(paths) == 0 {
			b.WriteString("  " + key + ": []\n")
			return
		}
		b.WriteString("  " + key + ":\n")
		for _, p := range paths {
			b.WriteString("    - " + p + "\n")
		}
	}
	writeKey("capabilities", caps)
	writeKey("sources", srcs)
	return b.String()
}

// pbTestCapabilityDoc 是一份最小能力文档（渲染后名字来自 values）。
func pbTestCapabilityDoc(name string, next []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: '" + name + "'\n")
	b.WriteString("spec:\n  params:\n    type: object\n  next: [")
	for i, ref := range next {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("'" + ref + "'")
	}
	b.WriteString("]\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n")
	b.WriteString("        #!/usr/bin/env bash\n        echo ok\n")
	return b.String()
}

// pbTestSourceDoc 是一份 webhook 源文档；extraConfig 会被嵌进 spec.config（造冲突用）。
func pbTestSourceDoc(name string, targets []string, extraConfig string) string {
	config := "{}"
	if extraConfig != "" {
		config = "{ " + extraConfig + " }"
	}
	var b strings.Builder
	b.WriteString("metadata:\n  name: '" + name + "'\n")
	b.WriteString("spec:\n  kind: webhook\n  enabled: true\n  config: " + config + "\n  targets:\n")
	for _, ref := range targets {
		b.WriteString("    - capability: '" + ref + "'\n      params:\n        repo: '{{values.project}}'\n")
	}
	return b.String()
}

// pbTestBundle 是「一个能力 + 一个源 + 入口」的最小可用包。
func pbTestBundle(slug string) map[string]string {
	capPath := "capabilities/review.yaml"
	srcPath := "sources/on-push.yaml"
	return map[string]string{
		"playbook.yaml": pbTestEntryDoc(slug, 3, []string{capPath}, []string{srcPath}),
		capPath:         pbTestCapabilityDoc("{{values.project}}-review", nil),
		srcPath:         pbTestSourceDoc("{{values.project}}-on-push", []string{"{{values.project}}-review"}, ""),
	}
}

// pbTestBundleWithSecret 是「源多一个 secret」的包：对已装的同名源是真实 config 差异
// （install 只做加法，不许改已有行）→ 必然 playbook_conflict。
func pbTestBundleWithSecret(slug string) map[string]string {
	files := pbTestBundle(slug)
	files["sources/on-push.yaml"] = pbTestSourceDoc(
		"{{values.project}}-on-push", []string{"{{values.project}}-review"}, `secret: 's3cret'`)
	return files
}

// pbTestValues 是安装/渲染的 values（包内 schema 只要求 project）。
func pbTestValues(project string) map[string]any { return map[string]any{"project": project} }

// pbTestWriteBody 组装写请求信封（W3：spec 只搬包体）。
func pbTestWriteBody(t *testing.T, slug string, files map[string]string, source string) map[string]any {
	t.Helper()
	spec := map[string]any{}
	if files != nil {
		spec["files"] = files
	}
	if source != "" {
		spec["source"] = source
	}
	return map[string]any{
		"apiVersion": "anotify/v1",
		"kind":       "Playbook",
		// metadata 只带 name：namespace 由路径决定，写请求的 metadata 白名单不含它
		// （decodeWriteEnvelope）。displayName / version 可选。
		"metadata": map[string]any{"name": slug},
		"spec":     spec,
	}
}

// pbTestPutBody 在写信封上挂 CAS 令牌。
func pbTestPutBody(t *testing.T, slug string, files map[string]string, rv string) map[string]any {
	t.Helper()
	body := pbTestWriteBody(t, slug, files, "")
	body["metadata"].(map[string]any)["resourceVersion"] = rv
	return body
}

// create 建包并断言 201，返回信封。
func (e *playbookTestEnv) create(t *testing.T, slug string, files map[string]string) pbTestEnvelope {
	t.Helper()
	rec := e.do(t, "POST", e.path(""), pbTestWriteBody(t, slug, files, ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建包 %q 应 201, got %d: %s", slug, rec.Code, rec.Body.String())
	}
	var env pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode create envelope: %v (%s)", err, rec.Body.String())
	}
	return env
}

// ---------- 用例 ----------

// TestPlaybookResourceCreateListGet 覆盖创建 + 列表 + 详情投影：
// 默认 spec 是空投影（不泄露包体），`?includeBody=true` 才回原始 files map。
func TestPlaybookResourceCreateListGet(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-review"
	files := pbTestBundle(slug)
	created := env.create(t, slug, files)

	if created.APIVersion != "anotify/v1" || created.Kind != "Playbook" {
		t.Fatalf("信封头不对: %+v", created)
	}
	if created.Metadata.Name != slug || created.Metadata.Namespace != env.nsSlug {
		t.Fatalf("metadata 不对: %+v", created.Metadata)
	}
	if created.Metadata.UID == "" {
		t.Fatalf("metadata.uid 应回 store 行 id: %+v", created.Metadata)
	}
	if !strings.HasPrefix(created.Metadata.ResourceVersion, "rv1:"+created.Metadata.UID+":") {
		t.Fatalf("resourceVersion 应是 rv1:<uid>:<rev>, got %q", created.Metadata.ResourceVersion)
	}
	if created.Metadata.Version == nil || *created.Metadata.Version != 3 {
		t.Fatalf("metadata.version 应透传包内 3, got %v", created.Metadata.Version)
	}
	if created.Metadata.CreationTimestamp == "" {
		t.Fatalf("creationTimestamp 必填: %+v", created.Metadata)
	}
	// 创建响应同样是摘要投影：spec 里没有 files。
	if len(created.Spec) != 0 {
		t.Fatalf("默认 spec 应是空投影 {}, got %v", created.Spec)
	}
	if created.Status.Ready {
		t.Fatalf("没有成员条件事实时 ready 不能是 true: %+v", created.Status)
	}
	if created.Status.Unapplied == nil || created.Status.Conditions == nil || created.Status.Items == nil {
		t.Fatalf("空数组纪律：unapplied/conditions/items 应是 [] 而不是 null: %+v", created.Status)
	}
	if created.Status.LastInstall != "" || created.Status.Source != nil {
		t.Fatalf("未安装 / 手传包不该有 lastInstallId / source: %+v", created.Status)
	}

	// 列表：kind 是 PlaybookList，items 是完整信封（摘要）。
	rec := env.do(t, "GET", env.path(""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list pbTestList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (%s)", err, rec.Body.String())
	}
	if list.Kind != "PlaybookList" {
		t.Fatalf("list kind 应是 PlaybookList, got %q", list.Kind)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != slug {
		t.Fatalf("list items 不对: %+v", list.Items)
	}
	if len(list.Items[0].Spec) != 0 {
		t.Fatalf("list 默认也是摘要（spec 空投影）, got %v", list.Items[0].Spec)
	}

	// 默认详情：spec 里没有 files 键。
	rec = env.do(t, "GET", env.path("/"+slug), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if _, hasFiles := got.Spec["files"]; hasFiles {
		t.Fatalf("默认详情不许带包体全文: %v", got.Spec)
	}

	// includeBody=true：原始未渲染的整份 map。
	rec = env.do(t, "GET", env.path("/"+slug)+"?includeBody=true", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("includeBody 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode includeBody: %v", err)
	}
	var bodyFiles map[string]string
	if err := json.Unmarshal(got.Spec["files"], &bodyFiles); err != nil {
		t.Fatalf("spec.files 应是 map[路径]string: %v (%s)", err, string(got.Spec["files"]))
	}
	if len(bodyFiles) != len(files) {
		t.Fatalf("includeBody 应回整份包体（%d 个文件）, got %d: %v", len(files), len(bodyFiles), bodyFiles)
	}
	if bodyFiles["playbook.yaml"] != files["playbook.yaml"] {
		t.Fatalf("包体必须原样往返（未渲染）")
	}
}

// TestPlaybookResourceCreateBodyRules 覆盖 spec.files / spec.source 的二选一与写请求校验。
func TestPlaybookResourceCreateBodyRules(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "body-rules"
	files := pbTestBundle(slug)

	cases := []struct {
		name       string
		body       map[string]any
		wantCode   int
		wantReason string
	}{
		{
			name:       "files 与 source 同时给",
			body:       pbTestWriteBody(t, slug, files, "oci://ghcr.io/acme/pb:1"),
			wantCode:   http.StatusBadRequest,
			wantReason: "invalid_playbook",
		},
		{
			name:       "files 与 source 都不给",
			body:       pbTestWriteBody(t, slug, nil, ""),
			wantCode:   http.StatusBadRequest,
			wantReason: "invalid_playbook",
		},
		{
			name:       "只有 source（OCI 拉取未实现）",
			body:       pbTestWriteBody(t, slug, nil, "oci://ghcr.io/acme/pb:1"),
			wantCode:   http.StatusBadRequest,
			wantReason: "oci_unsupported",
		},
		{
			name: "body 带 status（只读）",
			body: map[string]any{
				"apiVersion": "anotify/v1", "kind": "Playbook",
				"metadata": map[string]any{"name": slug, "namespace": env.nsID},
				"spec":     map[string]any{"files": files},
				"status":   map[string]any{"ready": true},
			},
			wantCode:   http.StatusBadRequest,
			wantReason: "status_not_writable",
		},
		{
			name: "缺 metadata.name",
			body: map[string]any{
				"apiVersion": "anotify/v1", "kind": "Playbook",
				"metadata": map[string]any{"displayName": "没有名字"},
				"spec":     map[string]any{"files": files},
			},
			wantCode:   http.StatusBadRequest,
			wantReason: "BadRequest",
		},
		{
			// namespace 的身份由路径段决定：body 里的 metadata.namespace 可以与 path
			// 相同（省略也行），但**不同**必须明确拒绝——静默以 path 为准会让调用方
			// 以为自己写到了另一个命名空间（api-conventions §3.2 namespace_mismatch）。
			name: "metadata.namespace 与路径冲突",
			body: map[string]any{
				"apiVersion": "anotify/v1", "kind": "Playbook",
				"metadata": map[string]any{"name": slug, "namespace": "another-ns"},
				"spec":     map[string]any{"files": files},
			},
			wantCode:   http.StatusBadRequest,
			wantReason: "namespace_mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, "POST", env.path(""), tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("应 %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			var doc pbTestStatusDoc
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
			}
			if doc.Kind != "Status" || doc.Reason != tc.wantReason {
				t.Fatalf("应 Status{%s}, got %+v", tc.wantReason, doc)
			}
		})
	}

	// 一条都没落库（全部被拒）。
	rows, err := env.db.ListPlaybooks(context.Background(), env.nsID)
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("被拒的请求不该落库, got %d 行", len(rows))
	}
}

// TestPlaybookResourceSlugTaken 覆盖同 slug 再 POST → 409 slug_taken（不是覆盖写）。
func TestPlaybookResourceSlugTaken(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "dup-slug"
	files := pbTestBundle(slug)
	env.create(t, slug, files)

	rec := env.do(t, "POST", env.path(""), pbTestWriteBody(t, slug, files, ""))
	if rec.Code != http.StatusConflict {
		t.Fatalf("同 slug 再 POST 应 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if doc.Kind != "Status" || doc.Reason != "slug_taken" {
		t.Fatalf("应 Status{slug_taken}, got %+v", doc)
	}
}

// TestPlaybookResourcePutCAS 覆盖整体替换的 CAS 三态与名字不可变。
func TestPlaybookResourcePutCAS(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "replace-cas"
	files := pbTestBundle(slug)
	created := env.create(t, slug, files)
	rv := created.Metadata.ResourceVersion

	// 缺令牌 → 400 resource_version_required。
	rec := env.do(t, "PUT", env.path("/"+slug), pbTestWriteBody(t, slug, files, ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT 缺 resourceVersion 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if doc.Reason != "resource_version_required" {
		t.Fatalf("应 resource_version_required, got %+v", doc)
	}

	// 旧令牌 → 409 resource_version_conflict + details.current.resourceVersion。
	stale := "rv1:" + created.Metadata.UID + ":99"
	rec = env.do(t, "PUT", env.path("/"+slug), pbTestPutBody(t, slug, files, stale))
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧令牌应 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if doc.Reason != "resource_version_conflict" || doc.Details.Current.ResourceVersion != rv {
		t.Fatalf("应 409 resource_version_conflict + 当前令牌 %q, got %+v", rv, doc)
	}

	// metadata.name 与路径不一致 → 400 name_immutable（路径就是寻址）。
	otherName := pbTestPutBody(t, slug, files, rv)
	otherName["metadata"].(map[string]any)["name"] = "another-slug"
	rec = env.do(t, "PUT", env.path("/"+slug), otherName)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("改名应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if doc.Reason != "name_immutable" {
		t.Fatalf("应 name_immutable, got %+v", doc)
	}

	// 正确令牌 → 200，resourceVersion 递增（模板被整体替换）。
	rec = env.do(t, "PUT", env.path("/"+slug), pbTestPutBody(t, slug, files, rv))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确令牌 PUT 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode put: %v", err)
	}
	if updated.Metadata.ResourceVersion == rv {
		t.Fatalf("替换后 resourceVersion 必须变（CAS 令牌不可复用）: %q", rv)
	}
	// 默认仍是摘要投影。
	if len(updated.Spec) != 0 {
		t.Fatalf("PUT 响应默认也是摘要, got %v", updated.Spec)
	}
}

// TestPlaybookResourceRenderIsReadOnly 覆盖 render：只读渲染，0 写、不产生回执。
func TestPlaybookResourceRenderIsReadOnly(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "render-only"
	env.create(t, slug, pbTestBundle(slug))

	rec := env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{"values": pbTestValues("a")})
	if rec.Code != http.StatusOK {
		t.Fatalf("render 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Values       map[string]any    `json:"values"`
		ValuesDigest string            `json:"valuesDigest"`
		Files        map[string]string `json:"files"`
		Capabilities []json.RawMessage `json:"capabilities"`
		Sources      []json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode render: %v (%s)", err, rec.Body.String())
	}
	if got.Values["project"] != "a" || !strings.HasPrefix(got.ValuesDigest, "sha256:") {
		t.Fatalf("render 应回有效 values + digest: %+v", got)
	}
	if len(got.Files) == 0 || len(got.Capabilities) != 1 || len(got.Sources) != 1 {
		t.Fatalf("render 应回渲染后的 files/能力/源: %+v", got)
	}

	// render 是只读动作：不建行、不写回执。
	if n := env.capCount(t); n != 0 {
		t.Fatalf("render 不许写能力行, got %d", n)
	}
	if n := env.installCount(t, slug); n != 0 {
		t.Fatalf("render 不许产生回执, got total=%d", n)
	}
}

// TestPlaybookResourceActionValuesErrors 覆盖 values 错的领域码直通 HTTP：
// playbook.Error 的封闭码表就是 reason（客户端按码分支，不再解析人话）。
func TestPlaybookResourceActionValuesErrors(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "values-errors"
	env.create(t, slug, pbTestBundle(slug))

	cases := []struct {
		name     string
		values   map[string]any
		wantCode int
		want     string
	}{
		{name: "缺必填 values", values: map[string]any{}, wantCode: http.StatusBadRequest, want: "values_missing"},
		{name: "values 不合 schema", values: map[string]any{"project": "Bad Name!"}, wantCode: http.StatusBadRequest, want: "values_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{"values": tc.values})
			if rec.Code != tc.wantCode {
				t.Fatalf("应 %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			var doc pbTestStatusDoc
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
			}
			if doc.Kind != "Status" || doc.Reason != tc.want {
				t.Fatalf("应 Status{%s}, got %+v", tc.want, doc)
			}
		})
	}
}

// TestPlaybookResourceInstallConflictWritesNothing 覆盖 install 冲突：
// 400 playbook_conflict + details（valuesDigest / conflicts[] / hint）且**一行不写**。
func TestPlaybookResourceInstallConflictWritesNothing(t *testing.T) {
	env := newPlaybookTestEnv(t)
	ctx := context.Background()
	slug := "conflict-pb"
	env.create(t, slug, pbTestBundle(slug))

	// 先正常装一次：建出 a-review@1 + a-on-push。
	rec := env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbTestValues("a")})
	if rec.Code != http.StatusOK {
		t.Fatalf("首次 install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	capsBefore, srcsBefore := env.capCount(t), env.srcCount(t)
	if capsBefore == 0 || srcsBefore == 0 {
		t.Fatalf("首次安装应建出能力与源, caps=%d srcs=%d", capsBefore, srcsBefore)
	}

	// 换包体：同名源多了 secret（真实 config 差异，install 不许覆盖已有行）。
	row, err := env.db.GetPlaybook(ctx, env.nsID, slug)
	if err != nil {
		t.Fatalf("get playbook: %v", err)
	}
	rv := store.ResourceVersion(row.ID, row.ResourceVersion)
	rec = env.do(t, "PUT", env.path("/"+slug), pbTestPutBody(t, slug, pbTestBundleWithSecret(slug), rv))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 换包体应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbTestValues("a")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("冲突 install 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode status: %v (%s)", err, rec.Body.String())
	}
	if doc.Kind != "Status" || doc.Reason != "playbook_conflict" || doc.Code != 400 {
		t.Fatalf("应 400 Status{playbook_conflict}, got %+v", doc)
	}
	if doc.Details.ValuesDigest == "" {
		t.Fatalf("details.valuesDigest 必须非空（客户端据此识别本次 values）: %+v", doc.Details)
	}
	if len(doc.Details.Conflicts) == 0 {
		t.Fatalf("details.conflicts[] 不许为空（12 格矩阵明细）: %+v", doc.Details)
	}
	conflict := doc.Details.Conflicts[0]
	if conflict.Kind != "source" || conflict.Ref != "a-on-push" || conflict.Reason == "" || len(conflict.Fields) == 0 {
		t.Fatalf("冲突条目要保留 kind/ref/reason/fields: %+v", conflict)
	}
	if doc.Details.Hint == "" {
		t.Fatalf("details.hint 应给出修复提示: %+v", doc.Details)
	}

	// 0 写：能力 / 源数量一个都没变。
	if capsAfter, srcsAfter := env.capCount(t), env.srcCount(t); capsAfter != capsBefore || srcsAfter != srcsBefore {
		t.Fatalf("冲突必须整包 0 写：caps %d→%d, srcs %d→%d", capsBefore, capsAfter, srcsBefore, srcsAfter)
	}
}

// TestPlaybookResourceInstallDryRunWritesNothing 覆盖 `{values, dryRun}`：
// dryRun 走同一条计划路径但不落任何行（回执照回，且标记 dryRun=true）。
func TestPlaybookResourceInstallDryRunWritesNothing(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "dry-run"
	env.create(t, slug, pbTestBundle(slug))

	rec := env.do(t, "POST", env.path("/"+slug+"/install"),
		map[string]any{"values": pbTestValues("a"), "dryRun": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("dryRun install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rcpt struct {
		Kind    string `json:"kind"`
		Install struct {
			DryRun bool `json:"dryRun"`
		} `json:"install"`
		Created struct {
			Capabilities []string `json:"capabilities"`
			Sources      []string `json:"sources"`
		} `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, rec.Body.String())
	}
	if rcpt.Kind != "InstallReceipt" || !rcpt.Install.DryRun {
		t.Fatalf("回执应标 dryRun=true: %+v", rcpt)
	}
	if len(rcpt.Created.Capabilities) != 1 || len(rcpt.Created.Sources) != 1 {
		t.Fatalf("dryRun 仍应算出「会建什么」: %+v", rcpt.Created)
	}
	// 0 持久写：能力 / 源 / 回执行一个都没落。
	if n := env.capCount(t); n != 0 {
		t.Fatalf("dryRun 不许写能力行, got %d", n)
	}
	if n := env.srcCount(t); n != 0 {
		t.Fatalf("dryRun 不许写源行, got %d", n)
	}
	if n := env.installCount(t, slug); n != 0 {
		t.Fatalf("dryRun 不许写回执行, got %d", n)
	}
}

// TestPlaybookResourceInstallReceiptsAndDeleteKeeps 覆盖回执读取与「删除只删清单」。
func TestPlaybookResourceInstallReceiptsAndDeleteKeeps(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "delete-keeps"
	env.create(t, slug, pbTestBundle(slug))

	rec := env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbTestValues("a")})
	if rec.Code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rcpt struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Install    struct {
			ID     string `json:"id"`
			DryRun bool   `json:"dryRun"`
		} `json:"install"`
		ValuesDigest string `json:"valuesDigest"`
		Created      struct {
			Capabilities []string `json:"capabilities"`
			Sources      []string `json:"sources"`
		} `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, rec.Body.String())
	}
	if rcpt.Kind != "InstallReceipt" || rcpt.Install.ID == "" || rcpt.Install.DryRun {
		t.Fatalf("回执头不对: %+v", rcpt)
	}
	if len(rcpt.Created.Capabilities) != 1 || len(rcpt.Created.Sources) != 1 {
		t.Fatalf("回执 created 应各 1 条: %+v", rcpt.Created)
	}

	// 实例清单：按 valuesDigest 折叠，带 lastInstallId。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("installs 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var installs struct {
		Total    int `json:"total"`
		Installs []struct {
			ValuesDigest  string `json:"valuesDigest"`
			Attempts      int    `json:"attempts"`
			LastInstallID string `json:"lastInstallId"`
		} `json:"installs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &installs); err != nil {
		t.Fatalf("decode installs: %v (%s)", err, rec.Body.String())
	}
	if installs.Total != 1 || len(installs.Installs) != 1 {
		t.Fatalf("应折叠成 1 组, got %+v", installs)
	}
	if installs.Installs[0].LastInstallID != rcpt.Install.ID || installs.Installs[0].Attempts != 1 {
		t.Fatalf("组里应指向本次回执, got %+v", installs.Installs[0])
	}

	// 单次回执直达。
	rec = env.do(t, "GET", env.path("/"+slug+"/installs/"+rcpt.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("单回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var single struct {
		Kind    string `json:"kind"`
		Install struct {
			ID string `json:"id"`
		} `json:"install"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode install: %v", err)
	}
	if single.Kind != "InstallReceipt" || single.Install.ID != rcpt.Install.ID {
		t.Fatalf("单回执形状不对: %+v", single)
	}

	// 详情 status 从回执历史派生：total / items / sources / lastInstallId。
	rec = env.do(t, "GET", env.path("/"+slug), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var detail pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Status.LastInstall != rcpt.Install.ID {
		t.Fatalf("status.lastInstallId 应指向本次回执, got %q", detail.Status.LastInstall)
	}
	if detail.Status.Total != 2 || len(detail.Status.Items) != 2 {
		t.Fatalf("status.total/items 应来自 installed_from 反查（1 能力 + 1 源）, got %+v", detail.Status)
	}
	if detail.Status.Sources.Total != 1 {
		t.Fatalf("status.sources.total 应是 1, got %+v", detail.Status.Sources)
	}

	// DELETE 缺令牌 → 400；旧令牌 → 409。
	rec = env.do(t, "DELETE", env.path("/"+slug), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("DELETE 缺 resourceVersion 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	oldRV := detail.Metadata.ResourceVersion
	stale := "rv1:" + detail.Metadata.UID + ":99"
	rec = env.do(t, "DELETE", env.path("/"+slug)+"?resourceVersion="+stale, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE 旧令牌应 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// 正确令牌 → 200，响应带 kept / values（留下的东西），不是信封。
	rec = env.do(t, "DELETE", env.path("/"+slug)+"?resourceVersion="+oldRV, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var deleted struct {
		Deleted bool `json:"deleted"`
		Kept    struct {
			Capabilities []string `json:"capabilities"`
			Sources      []string `json:"sources"`
		} `json:"kept"`
		Values []map[string]any `json:"values"`
		Note   string           `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete: %v (%s)", err, rec.Body.String())
	}
	if !deleted.Deleted {
		t.Fatalf("deleted 应为 true: %+v", deleted)
	}
	if len(deleted.Kept.Capabilities) != 1 || !strings.HasPrefix(deleted.Kept.Capabilities[0], "a-review@") {
		t.Fatalf("kept.capabilities 应留下已装能力, got %+v", deleted.Kept)
	}
	if len(deleted.Kept.Sources) != 1 || deleted.Kept.Sources[0] != "a-on-push" {
		t.Fatalf("kept.sources 应留下已装源, got %+v", deleted.Kept)
	}
	if len(deleted.Values) != 1 || deleted.Note == "" {
		t.Fatalf("删除响应应带 values 与 note: %+v", deleted)
	}

	// 清单没了，回执与已装对象都还在（清单不是所有权）。
	rec = env.do(t, "GET", env.path("/"+slug), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删后详情应 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := env.capCount(t); n != 1 {
		t.Fatalf("删清单不许级联删能力, got %d", n)
	}
	if n := env.srcCount(t); n != 1 {
		t.Fatalf("删清单不许级联删源, got %d", n)
	}
}

// TestPlaybookResourceAuthRequired 覆盖「全部路由 session 包裹」：未登录直 401。
func TestPlaybookResourceAuthRequired(t *testing.T) {
	env := newPlaybookTestEnv(t)
	req := httptest.NewRequest("GET", env.path(""), nil)
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPlaybookResourceNamespaceIsolation 钉「包是命名空间内对象」：
// 同名包在 ns A 存在时，ns B 的列表为空、单读 404、拿 A 的 CAS 令牌在 B 下 DELETE
// 也删不掉 A 的那一份（A 里读回 200 且令牌未前进）。
func TestPlaybookResourceNamespaceIsolation(t *testing.T) {
	env := newPlaybookTestEnv(t)
	ctx := context.Background()

	nsB, err := env.db.CreateNamespace(ctx, "pb-b", "包命名空间 B")
	if err != nil {
		t.Fatalf("create namespace pb-b: %v", err)
	}

	slug := "cross-ns-pkg"
	created := env.create(t, slug, pbTestBundle(slug))
	rvA := created.Metadata.ResourceVersion
	if rvA == "" {
		t.Fatalf("创建信封缺 metadata.resourceVersion: %+v", created.Metadata)
	}
	if created.Metadata.Namespace != env.nsSlug {
		t.Fatalf("metadata.namespace = %q, want %q", created.Metadata.Namespace, env.nsSlug)
	}

	pathB := "/v1/namespaces/" + nsB.Slug + "/playbooks"

	// ① B 的列表 0 项（items 是数组，不是 null）。
	rec := env.do(t, http.MethodGet, pathB, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ns B 列表期望 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listB map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &listB); err != nil {
		t.Fatalf("decode 列表: %v (%s)", err, rec.Body.String())
	}
	itemsB, ok := listB["items"].([]any)
	if !ok {
		t.Fatalf("ns B 列表 items 必须是数组，得到 %T（%s）", listB["items"], rec.Body.String())
	}
	if len(itemsB) != 0 {
		t.Fatalf("ns B 列表不应看见 ns A 的包，实际 %d 条: %s", len(itemsB), rec.Body.String())
	}

	// ② B 的单包读 → 404（不泄露存在性）。
	rec = env.do(t, http.MethodGet, pathB+"/"+slug, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ns B 读同名包期望 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var status pbTestStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode 404: %v (%s)", err, rec.Body.String())
	}
	if status.Kind != kindStatus || status.Reason != reasonNotFound {
		t.Fatalf("跨 ns 单读应为 Status/%s，得到 kind=%q reason=%q", reasonNotFound, status.Kind, status.Reason)
	}

	// ③ 拿 A 的 CAS 令牌在 B 下 DELETE → 404，A 的包不能被删。
	rec = env.do(t, http.MethodDelete, pathB+"/"+slug+"?resourceVersion="+rvA, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ns B DELETE（带 A 的令牌）期望 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// ④ A 里读回：200、同一个包、令牌未前进（§③ 的删除一次都没落地）。
	rec = env.do(t, http.MethodGet, env.path("/"+slug), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("跨 ns 删除之后 A 的包必须还在（期望 200）, got %d: %s", rec.Code, rec.Body.String())
	}
	var after pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode A 的包: %v (%s)", err, rec.Body.String())
	}
	if after.Kind != "Playbook" || after.Metadata.Name != slug {
		t.Fatalf("A 的包标识异常: kind=%q name=%q", after.Kind, after.Metadata.Name)
	}
	if after.Metadata.ResourceVersion != rvA {
		t.Fatalf("跨 ns 删除不该推进 A 的 resourceVersion：%q → %q", rvA, after.Metadata.ResourceVersion)
	}
	// 清单里 A 仍有且只有 1 条。
	rec = env.do(t, http.MethodGet, env.path(""), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("A 列表期望 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var listA pbTestList
	if err := json.Unmarshal(rec.Body.Bytes(), &listA); err != nil {
		t.Fatalf("decode A 列表: %v (%s)", err, rec.Body.String())
	}
	if len(listA.Items) != 1 || listA.Items[0].Metadata.Name != slug {
		t.Fatalf("A 的清单应仍有 %q，实际 %+v", slug, listA.Items)
	}
}
