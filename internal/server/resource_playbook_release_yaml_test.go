package server

// #536：按 release 导出对象 YAML 的传输面验收；变异自证见 docs/reports/release-yaml-export.md。

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/auth"
)

// pbTestExportMux 同 env 挂三条域：「导出 vs 单对象 /yaml」走真实 HTTP。
func (e *playbookTestEnv) pbTestExportMux(t *testing.T) {
	t.Helper()
	sm := auth.NewSessionManager(e.db, 0, false)
	mux := http.NewServeMux()
	deps := &serverDeps{DB: e.db, Now: nowMilli, Session: sm.Middleware}
	newPlaybookAPI(deps).register(mux)
	newCapabilityAPI(deps).register(mux)
	newSourceAPI(deps).register(mux)
	e.mux = mux
}

// pbTestExportBundle 是 2 能力 + 1 源的包；列表顺序刻意与名字升序相反（z 在前、a 在后）。
func pbTestExportBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":       pbTestEntryDoc(slug, 1, []string{"capabilities/z.yaml", "capabilities/a.yaml"}, []string{"sources/m.yaml"}),
		"capabilities/z.yaml": pbTestCapabilityDoc("{{values.project}}-z-review", nil),
		"capabilities/a.yaml": pbTestCapabilityDoc("{{values.project}}-a-fixer", nil),
		"sources/m.yaml":      pbTestSourceDoc("{{values.project}}-m-push", []string{"{{values.project}}-z-review"}, `secret: 'hook-secret'`),
	}
}

// pbTestExportNeighborBundle 是「别人的」包：名字共享前缀 `demo-`，属另一个包/另一个 release。
func pbTestExportNeighborBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbTestEntryDoc(slug, 1, []string{"capabilities/manual.yaml"}, []string{"sources/manual.yaml"}),
		"capabilities/manual.yaml": pbTestCapabilityDoc("demo-manual-review", nil),
		"sources/manual.yaml":      pbTestSourceDoc("demo-manual-push", []string{"demo-manual-review"}, ""),
	}
}

func (e *playbookTestEnv) exportYAML(t *testing.T, slug, release string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, "GET", e.path("/"+slug+"/releases/"+release+"/yaml"), nil)
}

// pbTestExportDocs 把导出正文切成「头注释 + 文档」，并断言头注释形状。
func pbTestExportDocs(t *testing.T, body string) (string, []string) {
	t.Helper()
	parts := strings.Split(body, "---\n")
	header := parts[0]
	lines := strings.Split(strings.TrimSuffix(header, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "# playbook: ") ||
		!strings.HasPrefix(lines[1], "# install: ") || !strings.Contains(lines[2], "当前生效版本") {
		t.Fatalf("头注释形状不对（要三行：# playbook / # install / 版本说明）:\n%s", header)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "#") {
			t.Fatalf("头注释第 %d 行不是注释：%q", i+1, line)
		}
	}
	return header, parts[1:]
}

func pbTestExportInstallOK(t *testing.T, env *playbookTestEnv, slug, release string) {
	t.Helper()
	code, raw := env.install(t, slug, release, pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install %s/%s 应 200, got %d: %s", slug, release, code, raw)
	}
}

func pbTestExportYAMLOf(t *testing.T, env *playbookTestEnv, path string) string {
	t.Helper()
	rec := env.do(t, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s 应 200, got %d: %s", path, rec.Code, rec.Body.String())
	}
	var doc yamlDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc.YAML
}

// TestPlaybookReleaseYAMLMatchesSingleObjectEndpoints：2 能力 + 1 源 → 3 个文档，逐字节同单对象端点。
func TestPlaybookReleaseYAMLMatchesSingleObjectEndpoints(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	rec := env.exportYAML(t, slug, "default")
	if rec.Code != http.StatusOK {
		t.Fatalf("导出应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Fatalf("Content-Type = %q，契约是 application/yaml; charset=utf-8", ct)
	}
	header, docs := pbTestExportDocs(t, rec.Body.String())
	if len(docs) != 3 {
		t.Fatalf("2 能力 + 1 源应导出 3 个文档，got %d:\n%s", len(docs), rec.Body.String())
	}
	if !strings.Contains(header, "objects: 3") || !strings.Contains(header, "release: default") {
		t.Fatalf("头注释缺实例事实：%s", header)
	}
	nsCaps := "/v1/namespaces/" + env.nsID + "/capabilities/"
	nsSources := "/v1/namespaces/" + env.nsID + "/sources/"
	for i, name := range []string{"demo-a-fixer", "demo-z-review"} {
		if want := pbTestExportYAMLOf(t, env, nsCaps+name+"/yaml"); docs[i] != want {
			t.Fatalf("能力 %s 的文档与单对象端点不逐字节相同：\n--- 导出 ---\n%q\n--- 端点 ---\n%q", name, docs[i], want)
		}
	}
	if want := pbTestExportYAMLOf(t, env, nsSources+"demo-m-push/yaml"); docs[2] != want {
		t.Fatalf("源的文档与单对象端点不逐字节相同：\n--- 导出 ---\n%q\n--- 端点 ---\n%q", docs[2], want)
	}
	// 末尾恰好一个换行。
	if strings.HasSuffix(rec.Body.String(), "\n\n") || !strings.HasSuffix(rec.Body.String(), "\n") {
		t.Fatalf("正文必须以恰好一个换行收尾")
	}
}

// TestPlaybookReleaseYAMLDeterministicSorted：两次导出 sha256 相同，能力升序在前、源升序在后。
func TestPlaybookReleaseYAMLDeterministicSorted(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	first := env.exportYAML(t, slug, "default").Body.Bytes()
	second := env.exportYAML(t, slug, "default").Body.Bytes()
	if sha256.Sum256(first) != sha256.Sum256(second) {
		t.Fatalf("同一 release 两次导出 sha256 不同（顺序不稳定）")
	}
	body := string(first)
	a, z, src := strings.Index(body, "name: demo-a-fixer"), strings.Index(body, "name: demo-z-review"), strings.Index(body, "name: demo-m-push")
	if a < 0 || z < 0 || src < 0 || !(a < z && z < src) {
		t.Fatalf("顺序必须是能力升序（a-fixer → z-review）再源：a=%d z=%d src=%d", a, z, src)
	}
}

// TestPlaybookReleaseYAMLOnlyReceiptRefs：同前缀的别人的对象（另一包/另一 release）不进导出。
func TestPlaybookReleaseYAMLOnlyReceiptRefs(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	neighbor := "pb-export-neighbor"
	env.create(t, neighbor, pbTestExportNeighborBundle(neighbor))
	pbTestExportInstallOK(t, env, neighbor, "default")

	slug := "pb-export"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	rec := env.exportYAML(t, slug, "default")
	if rec.Code != http.StatusOK {
		t.Fatalf("导出应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	_, docs := pbTestExportDocs(t, body)
	if len(docs) != 3 {
		t.Fatalf("导出集合只由回执决定，应恰好 3 个文档，got %d:\n%s", len(docs), body)
	}
	for _, foreign := range []string{"demo-manual-review", "demo-manual-push"} {
		if strings.Contains(body, foreign) {
			t.Fatalf("导出里出现了回执没记录的对象 %q（前缀匹配越界）:\n%s", foreign, body)
		}
	}
}

// TestPlaybookReleaseYAMLMasksCredentials：secret 不出现，token 只在 hookUrl 里，只给 hookUrl/secretSet。
func TestPlaybookReleaseYAMLMasksCredentials(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	body := env.exportYAML(t, slug, "default").Body.String()
	if strings.Contains(body, "hook-secret") {
		t.Fatalf("webhook 源的 secret 原文出现在导出里：\n%s", body)
	}
	if !strings.Contains(body, "secretSet: true") || !strings.Contains(body, "hookUrl: /v1/hooks/") {
		t.Fatalf("导出应给只读投影 hookUrl / secretSet：\n%s", body)
	}
	for _, key := range []string{"\n    token:", "\n    secret:"} {
		if strings.Contains(body, key) {
			t.Fatalf("导出里出现了凭据字段 %q：\n%s", key, body)
		}
	}
	// token 只允许出现在 hookUrl 的路径段里。
	hook := strings.TrimPrefix(pbTestExportYAMLOf(t, env, "/v1/namespaces/"+env.nsID+"/sources/demo-m-push/yaml"), "")
	token := ""
	for _, line := range strings.Split(hook, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "hookUrl: /v1/hooks/"); ok {
			token = strings.TrimSpace(v)
		}
	}
	if token == "" {
		t.Fatalf("单对象端点里没有 hookUrl，脱敏口径无法比对：\n%s", hook)
	}
	if rest := strings.ReplaceAll(body, "/v1/hooks/"+token, ""); strings.Contains(rest, token) {
		t.Fatalf("token 出现在 hookUrl 之外的位置：\n%s", body)
	}
}

// TestPlaybookReleaseYAMLErrors：不存在/已卸载 → 404 release_not_found；形状错 400、包不存在 404。
func TestPlaybookReleaseYAMLErrors(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	cases := []struct {
		why, playbook, release string
		code                   int
		reason                 string
	}{
		{"release 从未存在", slug, "nope", http.StatusNotFound, "release_not_found"},
		{"release 形状错", slug, "Prod", http.StatusBadRequest, "invalid_playbook"},
		{"包不存在", "pb-export-nope", "default", http.StatusNotFound, "NotFound"},
	}
	for _, tc := range cases {
		rec := env.exportYAML(t, tc.playbook, tc.release)
		if rec.Code != tc.code {
			t.Fatalf("%s：期望 %d, got %d: %s", tc.why, tc.code, rec.Code, rec.Body.String())
		}
		var doc pbTestStatusDoc
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s：错误体不是 JSON Status: %v (%s)", tc.why, err, rec.Body.String())
		}
		if doc.Reason != tc.reason {
			t.Fatalf("%s：reason = %q，期望 %q", tc.why, doc.Reason, tc.reason)
		}
	}
	// 已卸载与「不存在」同一个回答（不再是 200 的空导出）。
	if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/default"), nil); rec.Code != http.StatusOK {
		t.Fatalf("卸载 default 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	gone := env.exportYAML(t, slug, "default")
	if gone.Code != http.StatusNotFound || !strings.Contains(gone.Body.String(), "release_not_found") {
		t.Fatalf("已卸载的 release 导出应 404 release_not_found, got %d: %s", gone.Code, gone.Body.String())
	}
}

// TestPlaybookReleaseYAMLEmptyAndReadOnly：0 对象 → 200 且 objects: 0；导出不写库、不产生回执。
func TestPlaybookReleaseYAMLEmptyAndReadOnly(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-export-empty"
	env.create(t, slug, map[string]string{"playbook.yaml": pbTestEntryDoc(slug, 1, nil, nil)})
	pbTestExportInstallOK(t, env, slug, "empty")

	capsBefore, srcsBefore, installsBefore := env.capCount(t), env.srcCount(t), env.installCount(t, slug)
	rec := env.exportYAML(t, slug, "empty")
	if rec.Code != http.StatusOK {
		t.Fatalf("0 对象的实例导出应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	header, docs := pbTestExportDocs(t, rec.Body.String())
	if len(docs) != 0 || !strings.Contains(header, "objects: 0") {
		t.Fatalf("0 对象应只有头注释且 objects: 0：\n%s", rec.Body.String())
	}
	_ = env.exportYAML(t, slug, "empty")
	if got := fmt.Sprintf("%d/%d/%d", env.capCount(t), env.srcCount(t), env.installCount(t, slug)); got != fmt.Sprintf("%d/%d/%d", capsBefore, srcsBefore, installsBefore) {
		t.Fatalf("导出必须只读：能力/源/回执计数 %s → %s", fmt.Sprintf("%d/%d/%d", capsBefore, srcsBefore, installsBefore), got)
	}
}
