package server

// #536：按 release 导出对象 YAML 的传输面验收；变异自证见 docs/reports/release-yaml-export.md。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// ---------- #551 `?as=installed`（安装时快照）；契约 = docs/reports/install-snapshot.md §3 ----------

// pbTestYAMLReceipt 只取本票用得到的回执事实（不扩 pbTestReleaseReceipt：那是并行面的文件）。
type pbTestYAMLReceipt struct {
	Install struct {
		ID string `json:"id"`
	} `json:"install"`
	ValuesDigest string `json:"valuesDigest"`
}

func pbTestInstallYAML(t *testing.T, env *playbookTestEnv, slug, release string, values map[string]any) (string, string) {
	t.Helper()
	code, raw := env.install(t, slug, release, values)
	if code != http.StatusOK {
		t.Fatalf("install %s/%s → %v: %d %s", slug, release, values, code, raw)
	}
	var rcpt pbTestYAMLReceipt
	if err := json.Unmarshal(raw, &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	return rcpt.Install.ID, rcpt.ValuesDigest
}

// pbTestInstalledDocs 切出 `as=installed` 的头注释 + 文档：固定 4 行（版本说明 + 源无版本），有缺失时第 5 行（D8）。
func pbTestInstalledDocs(t *testing.T, body string, wantMissing int) (string, []string) {
	t.Helper()
	parts := strings.Split(body, "---\n")
	header := parts[0]
	lines := strings.Split(strings.TrimSuffix(header, "\n"), "\n")
	want := 4
	if wantMissing > 0 {
		want = 5
	}
	if len(lines) != want {
		t.Fatalf("as=installed 头注释要 %d 行，got %d:\n%s", want, len(lines), header)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, "#") {
			t.Fatalf("头注释第 %d 行不是注释：%q", i+1, line)
		}
	}
	if !strings.HasPrefix(lines[0], "# playbook: ") || !strings.HasPrefix(lines[1], "# install: ") {
		t.Fatalf("头注释前两行形状不对:\n%s", header)
	}
	if !strings.Contains(lines[2], "安装时快照") || !strings.Contains(lines[3], "源无版本") {
		t.Fatalf("第 3/4 行必须是「安装时快照」+「源无版本」说明：\n%s", header)
	}
	if wantMissing > 0 && !strings.Contains(lines[4], fmt.Sprintf("快照缺失: %d 个对象", wantMissing)) {
		t.Fatalf("第 5 行应是「快照缺失: %d 个对象」：\n%s", wantMissing, header)
	}
	return header, parts[1:]
}

// TestPlaybookReleaseYAMLInstalledSnapshot 是验收 1：装 v1 → 升级出 v2 → as=installed 是 v1、默认是 v2。
func TestPlaybookReleaseYAMLInstalledSnapshot(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-snapshot"
	env.create(t, slug, pbTestExportBundle(slug))
	pbTestExportInstallOK(t, env, slug, "default")

	// 把安装时建出的那个能力升一版：同一 name 不同正文 = 新版本（内容寻址）。
	rec := env.do(t, "POST", "/v1/namespaces/"+env.nsID+"/capabilities",
		capabilityBody("demo-a-fixer", 0, "#!/bin/sh\necho v2\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("升级能力应 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var upgraded struct {
		Metadata struct {
			Version int `json:"version"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upgraded); err != nil {
		t.Fatalf("decode 升级后的能力: %v (%s)", err, rec.Body.String())
	}
	if upgraded.Metadata.Version != 2 {
		t.Fatalf("升级后当前生效版本应是 2, got %d", upgraded.Metadata.Version)
	}

	snap := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed", nil)
	if snap.Code != http.StatusOK {
		t.Fatalf("as=installed 应 200, got %d: %s", snap.Code, snap.Body.String())
	}
	header, docs := pbTestInstalledDocs(t, snap.Body.String(), 0)
	if !strings.Contains(header, "objects: 3") {
		t.Fatalf("快照导出应含 3 个对象：%s", header)
	}
	if len(docs) != 3 {
		t.Fatalf("2 能力 + 1 源应导出 3 个文档，got %d:\n%s", len(docs), snap.Body.String())
	}
	if !strings.Contains(docs[0], "version: 1") || !strings.Contains(docs[0], "echo ok") {
		t.Fatalf("as=installed 的 demo-a-fixer 必须是安装那一刻的 v1 正文：\n%s", docs[0])
	}
	if strings.Contains(docs[0], "echo v2") {
		t.Fatalf("as=installed 取成了当前生效版本（v2）——变异 ② 没被抓住：\n%s", docs[0])
	}

	cur := env.exportYAML(t, slug, "default")
	if cur.Code != http.StatusOK {
		t.Fatalf("默认导出应 200, got %d: %s", cur.Code, cur.Body.String())
	}
	_, curDocs := pbTestExportDocs(t, cur.Body.String())
	if len(curDocs) != 3 {
		t.Fatalf("默认导出应 3 个文档，got %d", len(curDocs))
	}
	if !strings.Contains(curDocs[0], "version: 2") || !strings.Contains(curDocs[0], "echo v2") {
		t.Fatalf("默认导出必须是当前生效版本（v2）：\n%s", curDocs[0])
	}
	if snap.Body.String() == cur.Body.String() {
		t.Fatalf("两种视图必须不同：as=installed 是快照，默认是当前生效版本")
	}
}

// TestPlaybookReleaseYAMLInstalledAfterUninstall 是验收 2：卸载后 as=installed 仍 200（源计入缺失），current 仍 404。
func TestPlaybookReleaseYAMLInstalledAfterUninstall(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-snapshot-gone"
	env.create(t, slug, pbTestExportBundle(slug))
	installID, _ := pbTestInstallYAML(t, env, slug, "default", pbTestValues("demo"))

	// 未升级时快照文档与单对象端点逐字节相同（同一份读投影与脱敏，D10）。
	before := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed", nil)
	_, beforeDocs := pbTestInstalledDocs(t, before.Body.String(), 0)
	if want := pbTestExportYAMLOf(t, env, "/v1/namespaces/"+env.nsID+"/capabilities/demo-a-fixer/yaml"); beforeDocs[0] != want {
		t.Fatalf("快照文档与单对象端点不逐字节相同：\n--- 快照 ---\n%q\n--- 端点 ---\n%q", beforeDocs[0], want)
	}

	if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/default"), nil); rec.Code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	snap := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed", nil)
	if snap.Code != http.StatusOK {
		t.Fatalf("已卸载的实例 as=installed 仍应 200（软删行正文还在）, got %d: %s", snap.Code, snap.Body.String())
	}
	// 能力只剩软删行（仍可取正文），源行已被卸载物理删除 → 恰好 1 个缺失。
	header, docs := pbTestInstalledDocs(t, snap.Body.String(), 1)
	if !strings.Contains(header, "install: "+installID) {
		t.Fatalf("头注释必须带安装身份 install: %s:\n%s", installID, header)
	}
	if !strings.Contains(header, "objects: 2") || len(docs) != 2 {
		t.Fatalf("软删的 2 个能力仍应导出（objects: 2），got %q / %d 个文档:\n%s", header, len(docs), snap.Body.String())
	}
	if strings.Contains(snap.Body.String(), "demo-m-push") {
		t.Fatalf("卸载已物理删除源行：快照导出不许编造它\n%s", snap.Body.String())
	}

	for _, path := range []string{
		env.path("/" + slug + "/releases/default/yaml"),
		env.path("/"+slug+"/releases/default/yaml") + "?as=current",
	} {
		rec := env.do(t, "GET", path, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "release_not_found") {
			t.Fatalf("%s 应 404 release_not_found（as=current 口径不变）, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// pbTestLegacyReceipt 把回执改造成 #551 之前的形状：JSON 的 created/skipped 没有 pinned，索引表也没有行。
func pbTestLegacyReceipt(t *testing.T, env *playbookTestEnv, installID string) {
	t.Helper()
	ctx := context.Background()
	var raw string
	if err := env.db.QueryRowContext(ctx, `SELECT receipt FROM playbook_installs WHERE id = ?`, installID).Scan(&raw); err != nil {
		t.Fatalf("读回执 %s: %v", installID, err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("decode 回执 %s: %v", installID, err)
	}
	for _, key := range []string{"created", "skipped"} {
		section, ok := doc[key].(map[string]any)
		if !ok {
			t.Fatalf("回执 %s 没有 %s 段: %s", installID, key, raw)
		}
		delete(section, "pinned")
	}
	legacy, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal 旧回执: %v", err)
	}
	if _, err := env.db.ExecContext(ctx,
		`UPDATE playbook_installs SET receipt = ? WHERE id = ?`, string(legacy), installID); err != nil {
		t.Fatalf("改写回执 %s: %v", installID, err)
	}
	if _, err := env.db.ExecContext(ctx,
		`DELETE FROM playbook_install_pinned WHERE install_id = ?`, installID); err != nil {
		t.Fatalf("删快照索引 %s: %v", installID, err)
	}
}

// TestPlaybookReleaseYAMLNoSnapshot 是验收 3：老回执（没有 pinned）→ 409 no_snapshot，causes[0].field=release。
func TestPlaybookReleaseYAMLNoSnapshot(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-snapshot-legacy"
	env.create(t, slug, pbTestExportBundle(slug))
	installID, _ := pbTestInstallYAML(t, env, slug, "default", pbTestValues("demo"))
	pbTestLegacyReceipt(t, env, installID)

	snap := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed", nil)
	if snap.Code != http.StatusConflict {
		t.Fatalf("没有快照应 409, got %d: %s", snap.Code, snap.Body.String())
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(snap.Body.Bytes(), &doc); err != nil {
		t.Fatalf("409 错误体不是 JSON Status: %v (%s)", err, snap.Body.String())
	}
	if doc.Reason != "no_snapshot" {
		t.Fatalf("reason = %q, 期望 no_snapshot（%s）", doc.Reason, snap.Body.String())
	}
	var causes struct {
		Details struct {
			Causes []struct {
				Reason string `json:"reason"`
				Field  string `json:"field"`
			} `json:"causes"`
		} `json:"details"`
	}
	if err := json.Unmarshal(snap.Body.Bytes(), &causes); err != nil {
		t.Fatalf("decode causes: %v (%s)", err, snap.Body.String())
	}
	if len(causes.Details.Causes) == 0 || causes.Details.Causes[0].Field != "release" {
		t.Fatalf("details.causes[0].field 必须指到 release（%s）", snap.Body.String())
	}
	if doc.Message == "" || !strings.Contains(doc.Message, "快照") {
		t.Fatalf("409 message 要说清「没有安装时快照」：%q", doc.Message)
	}

	// 老回执照常可按当前生效版本导出：409 只属于 as=installed。
	if rec := env.exportYAML(t, slug, "default"); rec.Code != http.StatusOK {
		t.Fatalf("老回执的默认导出应 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestPlaybookReleaseYAMLAsParam：as 的两态等价与非法值、valuesDigest 与 as=installed 的组合。
func TestPlaybookReleaseYAMLAsParam(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-snapshot-as"
	env.create(t, slug, pbTestExportBundle(slug))
	_, firstDigest := pbTestInstallYAML(t, env, slug, "default", pbTestValues("demo"))

	plain := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml"), nil)
	current := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=current", nil)
	if plain.Code != http.StatusOK || current.Code != http.StatusOK {
		t.Fatalf("两种写法都应 200: %d / %d", plain.Code, current.Code)
	}
	if plain.Body.String() != current.Body.String() {
		t.Fatalf("不带 as 与 as=current 必须逐字节相同：\n%s\n---\n%s", plain.Body.String(), current.Body.String())
	}
	if plain.Header().Get("Content-Type") != current.Header().Get("Content-Type") {
		t.Fatalf("Content-Type 不一致: %q / %q", plain.Header().Get("Content-Type"), current.Header().Get("Content-Type"))
	}

	for _, bad := range []string{"nope", "Installed", "snapshot"} {
		rec := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as="+url.QueryEscape(bad), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "value_invalid") {
			t.Fatalf("as=%q 应 400 value_invalid, got %d: %s", bad, rec.Code, rec.Body.String())
		}
	}

	// 同一 release 的第二套 values = 第二个参数组：不带 digest 是两组并集，按组只给这一组。
	_, secondDigest := pbTestInstallYAML(t, env, slug, "default", pbTestValues("demo2"))
	all := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed", nil)
	if all.Code != http.StatusOK {
		t.Fatalf("并集导出应 200, got %d: %s", all.Code, all.Body.String())
	}
	head, docs := pbTestInstalledDocs(t, all.Body.String(), 0)
	if len(docs) != 6 || !strings.Contains(head, "objects: 6") {
		t.Fatalf("两套 values = 4 能力 + 2 源 = 6 个文档，got %d:\n%s", len(docs), all.Body.String())
	}
	one := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml")+"?as=installed&valuesDigest="+url.QueryEscape(secondDigest), nil)
	if one.Code != http.StatusOK {
		t.Fatalf("按组导出应 200, got %d: %s", one.Code, one.Body.String())
	}
	head, docs = pbTestInstalledDocs(t, one.Body.String(), 0)
	if len(docs) != 3 || !strings.Contains(head, "objects: 3") {
		t.Fatalf("按组导出只给这一组的 3 个文档，got %d:\n%s", len(docs), one.Body.String())
	}
	if !strings.Contains(one.Body.String(), "demo2-a-fixer") || strings.Contains(one.Body.String(), "name: demo-a-fixer") {
		t.Fatalf("按组导出只应含第二组的对象:\n%s", one.Body.String())
	}
	if strings.Contains(one.Body.String(), "values-digest: "+firstDigest) {
		t.Fatalf("按组导出的头注释应写第二组的 valuesDigest:\n%s", head)
	}
}
