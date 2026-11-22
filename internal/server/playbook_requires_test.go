package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

type pbReqCause struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

type pbReqStatusDoc struct {
	Kind    string `json:"kind"`
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Details *struct {
		Causes []pbReqCause `json:"causes"`
	} `json:"details"`
}

func (d pbReqStatusDoc) causes(t *testing.T) []pbReqCause {
	t.Helper()
	if d.Details == nil {
		t.Fatalf("响应缺 details.causes（%+v）", d)
	}
	return d.Details.Causes
}

type pbReqReceipt struct {
	Kind         string `json:"kind"`
	Dependencies []struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	} `json:"dependencies"`
}

func decodeRequiresStatus(t *testing.T, raw []byte) pbReqStatusDoc {
	t.Helper()
	var doc pbReqStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode status: %v (%s)", err, raw)
	}
	return doc
}

func pbReqBundle(slug, requires string) map[string]string {
	files := pbTestBundle(slug)
	files["playbook.yaml"] = strings.TrimRight(files["playbook.yaml"], "\n") + "\n  requires: " + requires + "\n"
	return files
}

func (e *playbookTestEnv) createRequiresCapability(t *testing.T, name string, version int) string {
	t.Helper()
	rec := e.do(t, "POST", "/v1/namespaces/"+e.nsID+"/capabilities",
		capabilityBody(name, version, "#!/usr/bin/env bash\necho "+name+"\n"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建能力 %q 应 201, got %d: %s", name, rec.Code, rec.Body.String())
	}
	return capString(t, capObject(t, decodeCapJSON(t, rec)["metadata"], "metadata"), "resourceVersion")
}

func (e *playbookTestEnv) deleteRequiresCapability(t *testing.T, name, rv string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, "DELETE", "/v1/namespaces/"+e.nsID+"/capabilities/"+name+"?resourceVersion="+rv, nil)
}

func (e *playbookTestEnv) installDryRun(t *testing.T, slug, release string, values map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"values": values, "dryRun": true}
	if release != "" {
		body["release"] = release
	}
	rec := e.do(t, "POST", e.path("/"+slug+"/install"), body)
	return rec.Code, rec.Body.Bytes()
}

func TestPlaybookInstallRequiresMissingCapability(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "req-missing"
	env.create(t, slug, pbReqBundle(slug, `[{capability: nope}]`))

	code, raw := env.install(t, slug, "a", pbTestValues("demo"))
	if code != http.StatusConflict {
		t.Fatalf("依赖缺失应 409, got %d: %s", code, raw)
	}
	doc := decodeRequiresStatus(t, raw)
	if doc.Kind != kindStatus || doc.Code != http.StatusConflict || doc.Reason != "dependency_missing" {
		t.Fatalf("应 409 Status{dependency_missing}, got %+v", doc)
	}
	causes := doc.causes(t)
	if len(causes) != 1 || causes[0].Field != "spec.requires[0].capability" || causes[0].Reason != "dependency_missing" {
		t.Fatalf("cause 应指到 spec.requires[0].capability, got %+v", causes)
	}
	if msg := causes[0].Message; !strings.Contains(msg, "have: none") {
		t.Fatalf("未解析到的能力要写 have: none, got %q", msg)
	}
	if n := env.capCount(t); n != 0 {
		t.Fatalf("409 必须一行不写, got %d 个能力", n)
	}
}

func TestPlaybookInstallRequiresDryRunUsesSamePath(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "req-dry"
	env.create(t, slug, pbReqBundle(slug, `[{capability: nope}]`))

	code, raw := env.installDryRun(t, slug, "d", pbTestValues("demo"))
	if code != http.StatusConflict {
		t.Fatalf("dryRun 也应 409, got %d: %s", code, raw)
	}
	dryDoc := decodeRequiresStatus(t, raw)
	if dryDoc.Kind != kindStatus || dryDoc.Reason != "dependency_missing" {
		t.Fatalf("dryRun 应 409 Status{dependency_missing}, got %+v", dryDoc)
	}
	dryCauses := dryDoc.causes(t)
	if len(dryCauses) != 1 || dryCauses[0].Field != "spec.requires[0].capability" {
		t.Fatalf("dryRun 的 cause 应指到 spec.requires[0].capability, got %+v", dryCauses)
	}
	if n := env.capCount(t); n != 0 {
		t.Fatalf("dryRun 的 409 也必须一行不写, got %d 个能力", n)
	}
	if n := env.installCount(t, slug); n != 0 {
		t.Fatalf("dryRun 的 409 不该留回执, got %d 条安装", n)
	}

	code, raw = env.install(t, slug, "d", pbTestValues("demo"))
	if code != http.StatusConflict {
		t.Fatalf("同一个包真装也应 409（不是只有 dryRun 才拒）, got %d: %s", code, raw)
	}
	realDoc := decodeRequiresStatus(t, raw)
	if realDoc.Reason != "dependency_missing" {
		t.Fatalf("真装应 409 dependency_missing, got %+v", realDoc)
	}
	realCauses := realDoc.causes(t)
	if len(realCauses) != 1 || realCauses[0].Field != dryCauses[0].Field || realCauses[0].Message != dryCauses[0].Message {
		t.Fatalf("dryRun 与真装应同一判据: dryRun %+v vs 真装 %+v", dryCauses, realCauses)
	}
	if n := env.capCount(t); n != 0 {
		t.Fatalf("真装 409 也必须一行不写, got %d 个能力", n)
	}
	if n := env.installCount(t, slug); n != 0 {
		t.Fatalf("真装 409 不该留回执, got %d 条安装", n)
	}
}

func TestPlaybookInstallRequiresVersionTooLow(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t)
	env.createRequiresCapability(t, "inventory", 1)
	slug := "req-low"
	env.create(t, slug, pbReqBundle(slug, `[{capability: inventory, minVersion: 2}]`))

	code, raw := env.install(t, slug, "a", pbTestValues("demo"))
	if code != http.StatusConflict {
		t.Fatalf("版本不足应 409, got %d: %s", code, raw)
	}
	doc := decodeRequiresStatus(t, raw)
	if doc.Reason != "dependency_missing" {
		t.Fatalf("版本不足仍走 dependency_missing, got %+v", doc)
	}
	causes := doc.causes(t)
	if len(causes) != 1 || causes[0].Field != "spec.requires[0].capability" {
		t.Fatalf("cause 应指到 spec.requires[0].capability, got %+v", causes)
	}
	if msg := causes[0].Message; !strings.Contains(msg, "need: 2") || !strings.Contains(msg, "have: 1") {
		t.Fatalf("版本不足的 message 要同时含 need: 2 与 have: 1, got %q", msg)
	}
}

func TestPlaybookInstallRequiresSatisfiedRecordsDependencies(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t)
	env.createRequiresCapability(t, "inventory", 1)
	slug := "req-ok"
	env.create(t, slug, pbReqBundle(slug, `[{capability: inventory, minVersion: 1}]`))

	code, raw := env.install(t, slug, "a", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("依赖满足应 200, got %d: %s", code, raw)
	}
	var rcpt pbReqReceipt
	if err := json.Unmarshal(raw, &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if len(rcpt.Dependencies) != 1 || rcpt.Dependencies[0].Capability != "inventory" || rcpt.Dependencies[0].Version != 1 {
		t.Fatalf("回执 dependencies 应是 [{inventory 1}], got %+v", rcpt.Dependencies)
	}
}

func TestPlaybookCreateRequiresInvalidName(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "req-badname"
	rec := env.do(t, "POST", env.path(""), pbTestWriteBody(t, slug, pbReqBundle(slug, `[{capability: Bad_Name}]`), ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("写入期形状错应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	doc := decodeRequiresStatus(t, rec.Body.Bytes())
	if doc.Reason != "invalid_playbook" || doc.Details == nil {
		t.Fatalf("应 400 invalid_playbook + details.causes, got %+v", doc)
	}
	causes := doc.causes(t)
	if len(causes) != 1 || causes[0].Field != "spec.requires[0].capability" {
		t.Fatalf("cause 应指到 spec.requires[0].capability, got %+v", causes)
	}
}

func TestCapabilityDeleteLockedByPlaybookRequires(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t)
	rv := env.createRequiresCapability(t, "inventory", 1)
	slug := "req-lock"
	env.create(t, slug, pbReqBundle(slug, `[{capability: inventory}]`))
	if code, raw := env.install(t, slug, "a", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}

	rec := env.deleteRequiresCapability(t, "inventory", rv)
	if rec.Code != http.StatusConflict {
		t.Fatalf("被依赖的能力删除应 409, got %d: %s", rec.Code, rec.Body.String())
	}
	doc := decodeRequiresStatus(t, rec.Body.Bytes())
	if doc.Kind != kindStatus || doc.Reason != "dependency_locked" {
		t.Fatalf("应 409 Status{dependency_locked}, got %+v", doc)
	}
	if !strings.Contains(doc.Message, "inventory") {
		t.Fatalf("顶层 message 应点名能力, got %q", doc.Message)
	}
	causes := doc.causes(t)
	if len(causes) != 1 || causes[0].Field != "name" || causes[0].Reason != "dependency_locked" {
		t.Fatalf("cause 应指认删除目标的 name, got %+v", causes)
	}
	if msg := causes[0].Message; !strings.Contains(msg, slug) || !strings.Contains(msg, `release "a"`) {
		t.Fatalf("cause 要写清哪个包哪个 release, got %q", msg)
	}
	alive := env.do(t, "GET", "/v1/namespaces/"+env.nsID+"/capabilities/inventory", nil)
	if alive.Code != http.StatusOK {
		t.Fatalf("409 必须 0 写：能力应仍在, got %d", alive.Code)
	}

	if code := env.uninstall(t, slug, "a", "").Code; code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d", code)
	}
	if rec = env.deleteRequiresCapability(t, "inventory", rv); rec.Code != http.StatusNoContent {
		t.Fatalf("卸载后删除应 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPlaybookInstallWithoutRequiresKeepsResponseShape(t *testing.T) {
	env := newPlaybookTestEnv(t)
	ctx := context.Background()
	slug := "req-control"
	env.create(t, slug, pbTestBundle(slug))
	if code, raw := env.install(t, slug, "", pbTestValues("a")); code != http.StatusOK {
		t.Fatalf("首次 install 应 200, got %d: %s", code, raw)
	}

	row, err := env.db.GetPlaybook(ctx, env.nsID, slug)
	if err != nil {
		t.Fatalf("get playbook: %v", err)
	}
	rv := store.ResourceVersion(row.ID, row.ResourceVersion)
	rec := env.do(t, "PUT", env.path("/"+slug), pbTestPutBody(t, slug, pbTestBundleWithSecret(slug), rv))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 换包体应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": pbTestValues("a")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("冲突 install 应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, `"causes"`) {
		t.Fatalf("无 requires 的 playbook_conflict 不该带 causes: %s", body)
	}

	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": map[string]any{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("values 缺失应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if doc := decodeRequiresStatus(t, rec.Body.Bytes()); doc.Details != nil || !strings.HasPrefix(doc.Reason, "values_") {
		t.Fatalf("无 causes 的兜底 400 不许凭空长出 details（reason 应是 values_*）, got %+v", doc)
	}
}
