package server

// Playbook **按 release 卸载**的传输面验收（#510）：动作文档字段、空列表纪律、第二次卸载零变化、
// 编号对照：removed/kept/installIds/uninstalledAt 与 `DELETE …/releases/{release}` 的 wire 形状。
// 手工行进 kept[] 不删、三个错误面（404/409/400）；删除语义（同名归属、CAS、边投影）由 store 覆盖。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zzjcool/anotify/internal/auth"
)

// pbTestUninstallDoc 是卸载响应（动作文档，不套四键信封）。
type pbTestUninstallDoc struct {
	Release            string   `json:"release"`
	AlreadyUninstalled bool     `json:"alreadyUninstalled"`
	UninstalledAt      string   `json:"uninstalledAt"`
	InstallIDs         []string `json:"installIds"`
	Removed            struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
	} `json:"removed"`
	Kept []struct {
		Kind         string `json:"kind"`
		Name         string `json:"name"`
		Version      int    `json:"version"`
		Reason       string `json:"reason"`
		OwnerRelease string `json:"ownerRelease"`
	} `json:"kept"`
	Note string `json:"note"`
}

func (e *playbookTestEnv) withCapabilities(t *testing.T) {
	t.Helper()
	sm := auth.NewSessionManager(e.db, 0, false)
	mux := http.NewServeMux()
	newPlaybookAPI(&serverDeps{DB: e.db, Now: nowMilli, Session: sm.Middleware}).register(mux)
	newCapabilityAPI(&serverDeps{DB: e.db, Now: nowMilli, Session: sm.Middleware}).register(mux)
	e.mux = mux
}

func (e *playbookTestEnv) uninstall(t *testing.T, slug, release, query string) *httptest.ResponseRecorder {
	t.Helper()
	path := e.path("/" + slug + "/releases/" + release)
	if query != "" {
		path += "?" + query
	}
	return e.do(t, "DELETE", path, nil)
}

func decodeUninstall(t *testing.T, rec *httptest.ResponseRecorder) pbTestUninstallDoc {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestUninstallDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode uninstall: %v (%s)", err, rec.Body.String())
	}
	return doc
}

func (e *playbookTestEnv) capabilityStatus(t *testing.T, name string) int {
	t.Helper()
	return e.do(t, "GET", "/v1/namespaces/"+e.nsID+"/capabilities/"+name, nil).Code
}

func TestPlaybookUninstallReleaseRemovesRows(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t)
	slug := "pr-uninstall"
	env.create(t, slug, pbTestReleaseBundle(slug))

	code, raw := env.install(t, slug, "a", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install a 应 200, got %d: %s", code, raw)
	}
	var rcpt pbTestReleaseReceipt
	if err := json.Unmarshal(raw, &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if got := env.capabilityStatus(t, "a-demo-review"); got != http.StatusOK {
		t.Fatalf("卸载前能力应可见, got %d", got)
	}

	doc := decodeUninstall(t, env.uninstall(t, slug, "a", ""))
	if doc.Release != "a" || doc.AlreadyUninstalled {
		t.Fatalf("首次卸载的 release/alreadyUninstalled 不对: %+v", doc)
	}
	if len(doc.Removed.Capabilities) != 1 || !strings.HasPrefix(doc.Removed.Capabilities[0], "a-demo-review@") {
		t.Fatalf("removed.capabilities 应是 a-demo-review@<version>, got %v", doc.Removed.Capabilities)
	}
	if len(doc.Removed.Sources) != 1 || doc.Removed.Sources[0] != "a-demo-on-push" {
		t.Fatalf("removed.sources 应是 a-demo-on-push, got %v", doc.Removed.Sources)
	}
	if len(doc.InstallIDs) != 1 || doc.InstallIDs[0] != rcpt.Install.ID {
		t.Fatalf("installIds 应指向这次安装的回执 %q, got %v", rcpt.Install.ID, doc.InstallIDs)
	}
	if _, err := time.Parse(time.RFC3339, doc.UninstalledAt); err != nil {
		t.Fatalf("uninstalledAt 应是 RFC3339, got %q: %v", doc.UninstalledAt, err)
	}
	if doc.Note == "" {
		t.Fatalf("note 要说明「回执保留、他人对象未删」: %+v", doc)
	}

	if got := env.capabilityStatus(t, "a-demo-review"); got != http.StatusNotFound {
		t.Fatalf("卸载后能力应 404, got %d", got)
	}
	if list := env.installs(t, slug); list.Total != 0 {
		t.Fatalf("卸载后实例清单应归零, got %+v", list)
	}
	rec := env.do(t, "GET", env.path("/"+slug+"/installs/"+rcpt.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("回执应仍可读（审计快照）, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlaybookUninstallReleaseIsIdempotent(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-uninstall-twice"
	env.create(t, slug, pbTestReleaseBundle(slug))
	if code, raw := env.install(t, slug, "a", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install a 应 200, got %d: %s", code, raw)
	}

	first := decodeUninstall(t, env.uninstall(t, slug, "a", ""))
	second := decodeUninstall(t, env.uninstall(t, slug, "a", ""))
	if !second.AlreadyUninstalled {
		t.Fatalf("第二次卸载应 alreadyUninstalled=true: %+v", second)
	}
	if second.Removed.Capabilities == nil || len(second.Removed.Capabilities) != 0 {
		t.Fatalf("第二次 removed.capabilities 应是 [], got %#v", second.Removed.Capabilities)
	}
	if second.Removed.Sources == nil || len(second.Removed.Sources) != 0 {
		t.Fatalf("第二次 removed.sources 应是 [], got %#v", second.Removed.Sources)
	}
	if second.Kept == nil || len(second.Kept) != 0 {
		t.Fatalf("第二次 kept 应是 [], got %#v", second.Kept)
	}
	if len(second.InstallIDs) != len(first.InstallIDs) || len(second.InstallIDs) == 0 {
		t.Fatalf("第二次仍应返回 installIds %v, got %v", first.InstallIDs, second.InstallIDs)
	}
	if second.UninstalledAt != first.UninstalledAt {
		t.Fatalf("第二次是 0 写：uninstalledAt 不该变（%q → %q）", first.UninstalledAt, second.UninstalledAt)
	}
}

func TestPlaybookUninstallKeepsManualRow(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t)
	slug := "pr-uninstall-manual"
	env.create(t, slug, pbTestReleaseBundle(slug))
	if code, raw := env.install(t, slug, "a", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install a 应 200, got %d: %s", code, raw)
	}

	rec := env.do(t, "POST", "/v1/namespaces/"+env.nsID+"/capabilities", capabilityBody("a-demo-review", 0, "#!/usr/bin/env bash\necho manual\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("手工建同名能力应 201, got %d: %s", rec.Code, rec.Body.String())
	}

	doc := decodeUninstall(t, env.uninstall(t, slug, "a", ""))
	if len(doc.Kept) != 1 {
		t.Fatalf("应只留一条 kept（同名手工能力）, got %+v", doc.Kept)
	}
	if k := doc.Kept[0]; k.Kind != "capability" || k.Name != "a-demo-review" || k.Reason != "manual" {
		t.Fatalf("kept[0] 应是 {capability, a-demo-review, manual}, got %+v", k)
	}
	if len(doc.Removed.Capabilities) != 1 || !strings.HasPrefix(doc.Removed.Capabilities[0], "a-demo-review@") {
		t.Fatalf("删的只是该 release 那一版, got %v", doc.Removed.Capabilities)
	}
	if got := env.capabilityStatus(t, "a-demo-review"); got != http.StatusOK {
		t.Fatalf("手工建的同名能力应仍可见, got %d", got)
	}
	if len(doc.Removed.Sources) != 1 || doc.Removed.Sources[0] != "a-demo-on-push" {
		t.Fatalf("源应照删, got %v", doc.Removed.Sources)
	}
}

func TestPlaybookUninstallErrorSurfaces(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-uninstall-err"
	env.create(t, slug, pbTestReleaseBundle(slug))
	code, raw := env.install(t, slug, "a", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install a 应 200, got %d: %s", code, raw)
	}

	cases := []struct {
		name       string
		release    string
		query      string
		wantCode   int
		wantReason string
	}{
		{"release 不在这个包名下", "ghost", "", http.StatusNotFound, "release_not_found"},
		{"installId 与 release 对不上", "a", "installId=ins_nope", http.StatusConflict, "install_conflict"},
		{"release 形状非法", "Prod", "", http.StatusBadRequest, "invalid_playbook"},
		{"包不存在", "a", "", http.StatusNotFound, "NotFound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := slug
			if tc.name == "包不存在" {
				target = "pr-no-such-playbook"
			}
			rec := env.uninstall(t, target, tc.release, tc.query)
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

	if n := env.capCount(t); n != 1 {
		t.Fatalf("报错不该删能力, got %d 行", n)
	}
	if list := env.installs(t, slug); list.Total != 1 {
		t.Fatalf("报错不该卸载回执, got %+v", list)
	}
}
