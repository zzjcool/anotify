package server

// Playbook requires（#534）的传输面验收：requires 未满足 / 能力被依赖锁是 409 且带 details.causes，
// 写入期形状错仍是 400 但同样带 causes；无 requires 的错误走的是同两条分支，响应体必须与今天一致（D7）。
// 领域语义（解析顺序、版本判定）由 internal/playbook 覆盖，这里只判 wire 形状（D5）。

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

// pbReqStatusDoc 是带 causes 的 Status 响应；Details 用指针，nil 即「响应体里没有 details 键」。
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

// pbReqReceipt 只取回执里 requires 相关的部分。
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

// pbReqBundle 在最小包上追加 spec.requires（对齐入口文档的 spec 缩进）。
func pbReqBundle(slug, requires string) map[string]string {
	files := pbTestBundle(slug)
	files["playbook.yaml"] = strings.TrimRight(files["playbook.yaml"], "\n") + "\n  requires: " + requires + "\n"
	return files
}

// createRequiresCapability 手工建一个能力（依赖目标），返回它的 CAS 令牌。
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

// TestPlaybookInstallRequiresMissingCapability：requires 点名一个不存在的能力 → 409 且一行不写。
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

// TestPlaybookInstallRequiresVersionTooLow：可见版本低于 minVersion → 409，message 同时给 need/have。
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

// TestPlaybookInstallRequiresSatisfiedRecordsDependencies：满足时安装成功，回执记下解析结果。
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

// TestPlaybookCreateRequiresInvalidName：写入期形状错仍是 400 invalid_playbook，但现在带 causes（D7）。
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

// TestCapabilityDeleteLockedByPlaybookRequires：被已生效安装依赖的能力删除 → 409，卸载该 release 后 204。
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

// TestPlaybookInstallWithoutRequiresKeepsResponseShape 是 D7 的对照组：无 requires 的包
// 走这两条错误分支时响应体逐字不变 —— 不许凭空长出 causes，也不许多出 details 键。
func TestPlaybookInstallWithoutRequiresKeepsResponseShape(t *testing.T) {
	env := newPlaybookTestEnv(t)
	ctx := context.Background()
	slug := "req-control"
	env.create(t, slug, pbTestBundle(slug))
	if code, raw := env.install(t, slug, "", pbTestValues("a")); code != http.StatusOK {
		t.Fatalf("首次 install 应 200, got %d: %s", code, raw)
	}

	// 冲突分支（playbook_conflict 的 details 是矩阵专用形状）：不含 causes。
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

	// 兜底 400 分支（values 形状错）：无 causes 时 details 键整体不出现。
	rec = env.do(t, "POST", env.path("/"+slug+"/install"), map[string]any{"values": map[string]any{}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("values 缺失应 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if doc := decodeRequiresStatus(t, rec.Body.Bytes()); doc.Details != nil || !strings.HasPrefix(doc.Reason, "values_") {
		t.Fatalf("无 causes 的兜底 400 不许凭空长出 details（reason 应是 values_*）, got %+v", doc)
	}
}
