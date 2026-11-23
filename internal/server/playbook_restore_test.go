package server

// 按快照重装（#561）的传输面：回执 + restoredFrom、四种 409 的 reason/causes、400/404、restores[] 读面。

import (
	"encoding/json"
	"net/http"
	"testing"
)

type pbTestRestoreReceipt struct {
	Kind     string `json:"kind"`
	Playbook struct {
		Name    string `json:"name"`
		Version int    `json:"version"`
	} `json:"playbook"`
	Install struct {
		ID      string `json:"id"`
		Release string `json:"release"`
	} `json:"install"`
	Created struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
		Pinned       []struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"pinned"`
	} `json:"created"`
	Skipped struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
	} `json:"skipped"`
	ValuesDigest string `json:"valuesDigest"`
	RestoredFrom string `json:"restoredFrom"`
}

type pbTestRestoreErrDoc struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Details struct {
		Causes []struct {
			Reason  string `json:"reason"`
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"causes"`
	} `json:"details"`
}

type pbTestRestoreDetail struct {
	Restores []struct {
		Action       string `json:"action"`
		InstallID    string `json:"installId"`
		RestoredFrom string `json:"restoredFrom"`
		Actor        string `json:"actor"`
		Recreated    struct {
			Capabilities int `json:"capabilities"`
			Sources      int `json:"sources"`
		} `json:"recreated"`
		Reused struct {
			Capabilities int `json:"capabilities"`
			Sources      int `json:"sources"`
		} `json:"reused"`
		At string `json:"at"`
	} `json:"restores"`
}

func pbTestCapsOnlyBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbTestEntryDoc(slug, 1, []string{"capabilities/review.yaml"}, nil),
		"capabilities/review.yaml": pbTestCapabilityDoc("{{values.project}}-review", nil),
	}
}

// pbTestTwoCapBundle 是「2 能力、无源」的包：演示 cause 指路是否指到坏的那个对象（不是第一个）。
func pbTestTwoCapBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":       pbTestEntryDoc(slug, 1, []string{"capabilities/a.yaml", "capabilities/b.yaml"}, nil),
		"capabilities/a.yaml": pbTestCapabilityDoc("demo-a", nil),
		"capabilities/b.yaml": pbTestCapabilityDoc("demo-b", nil),
	}
}

func (e *playbookTestEnv) restore(t *testing.T, slug, release string, body any) (int, []byte) {
	t.Helper()
	rec := e.do(t, "POST", e.path("/"+slug+"/releases/"+release+"/restore"), body)
	return rec.Code, rec.Body.Bytes()
}

func decodeRestoreErr(t *testing.T, code int, raw []byte) pbTestRestoreErrDoc {
	t.Helper()
	var doc pbTestRestoreErrDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode 错误体: %v (%s)", err, raw)
	}
	if doc.Code != code {
		t.Fatalf("错误体 code = %d, want %d (%s)", doc.Code, code, raw)
	}
	return doc
}

// TestPlaybookRestoreWire 成功路径：200 的完整回执 + restoredFrom，详情端点能读到审计行。
func TestPlaybookRestoreWire(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-restore-ok"
	env.create(t, slug, pbTestCapsOnlyBundle(slug))

	code, raw := env.install(t, slug, "alpha", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}
	var installed pbTestReleaseReceipt
	if err := json.Unmarshal(raw, &installed); err != nil {
		t.Fatalf("decode install receipt: %v", err)
	}
	if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/alpha"), nil); rec.Code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
	if code != http.StatusOK {
		t.Fatalf("复原应 200, got %d: %s", code, raw)
	}
	var restored pbTestRestoreReceipt
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatalf("decode restore receipt: %v (%s)", err, raw)
	}
	if restored.RestoredFrom == "" || restored.RestoredFrom == restored.Install.ID {
		t.Fatalf("restoredFrom = %q, want 原回执 id（新回执是 %q）", restored.RestoredFrom, restored.Install.ID)
	}
	if len(restored.Created.Capabilities) != 1 || restored.Created.Capabilities[0] != "demo-review@1" {
		t.Fatalf("created.capabilities = %v, want [demo-review@1]", restored.Created.Capabilities)
	}
	if len(restored.Created.Pinned) != 1 || restored.Created.Pinned[0].Kind != "capability" {
		t.Fatalf("created.pinned = %+v, want 一项能力", restored.Created.Pinned)
	}
	if restored.ValuesDigest != installed.ValuesDigest {
		t.Fatalf("valuesDigest = %q, want 沿用 %q", restored.ValuesDigest, installed.ValuesDigest)
	}

	rec := env.do(t, "GET", env.path("/"+slug+"/releases/alpha"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var detail pbTestRestoreDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode 详情: %v", err)
	}
	if len(detail.Restores) != 1 {
		t.Fatalf("restores = %+v, want 1 条", detail.Restores)
	}
	line := detail.Restores[0]
	if line.Action != "install.restored" || line.InstallID != restored.Install.ID || line.RestoredFrom != installed.Install.ID {
		t.Fatalf("审计行 = %+v, want action/installId/restoredFrom 全对", line)
	}
	if line.Actor == "" {
		t.Fatalf("审计行缺 actor: %+v", line)
	}
	if line.Recreated.Capabilities != 1 || line.Reused.Sources != 0 {
		t.Fatalf("审计计数 = %+v, want recreated.capabilities=1", line)
	}
}

func TestPlaybookRestoreErrors(t *testing.T) {
	t.Run("already_installed", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-live"
		env.create(t, slug, pbTestCapsOnlyBundle(slug))
		code, raw := env.install(t, slug, "alpha", pbTestValues("demo"))
		if code != http.StatusOK {
			t.Fatalf("install 应 200, got %d: %s", code, raw)
		}
		var installed pbTestReleaseReceipt
		if err := json.Unmarshal(raw, &installed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusConflict {
			t.Fatalf("生效中复原应 409, got %d: %s", code, raw)
		}
		if doc := decodeRestoreErr(t, http.StatusConflict, raw); doc.Reason != "already_installed" {
			t.Fatalf("reason = %q, want already_installed", doc.Reason)
		}
	})

	t.Run("value_invalid", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-badreq"
		env.create(t, slug, pbTestCapsOnlyBundle(slug))
		code, raw := env.restore(t, slug, "alpha", map[string]any{})
		if code != http.StatusBadRequest {
			t.Fatalf("缺 valuesDigest 应 400, got %d: %s", code, raw)
		}
		if doc := decodeRestoreErr(t, http.StatusBadRequest, raw); doc.Reason != "value_invalid" {
			t.Fatalf("reason = %q, want value_invalid", doc.Reason)
		}
	})

	t.Run("release_not_found", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-404"
		env.create(t, slug, pbTestCapsOnlyBundle(slug))
		code, raw := env.restore(t, slug, "ghost", map[string]any{"valuesDigest": "sha256:nope"})
		if code != http.StatusNotFound {
			t.Fatalf("未知 release 应 404, got %d: %s", code, raw)
		}
		if doc := decodeRestoreErr(t, http.StatusNotFound, raw); doc.Reason != "release_not_found" {
			t.Fatalf("reason = %q, want release_not_found", doc.Reason)
		}
	})

	t.Run("pinned_gone", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		env.withCapabilities(t)
		slug := "pr-restore-gone"
		env.create(t, slug, pbTestCapsOnlyBundle(slug))
		code, raw := env.install(t, slug, "alpha", pbTestValues("demo"))
		if code != http.StatusOK {
			t.Fatalf("install 应 200, got %d: %s", code, raw)
		}
		var installed pbTestReleaseReceipt
		if err := json.Unmarshal(raw, &installed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/alpha"), nil); rec.Code != http.StatusOK {
			t.Fatalf("卸载应 200: %s", rec.Body.String())
		}
		if _, err := env.db.ExecContext(t.Context(),
			`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-review'`, env.nsID); err != nil {
			t.Fatalf("模拟 GC 硬删: %v", err)
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusConflict {
			t.Fatalf("硬删后复原应 409, got %d: %s", code, raw)
		}
		doc := decodeRestoreErr(t, http.StatusConflict, raw)
		if doc.Reason != "pinned_gone" {
			t.Fatalf("reason = %q, want pinned_gone", doc.Reason)
		}
		if len(doc.Details.Causes) != 1 || doc.Details.Causes[0].Reason != "pinned_gone" ||
			doc.Details.Causes[0].Field != "pinned.capabilities[0]" {
			t.Fatalf("causes = %+v, want 一条 pinned.capabilities[0]", doc.Details.Causes)
		}
		if !containsAll(doc.Details.Causes[0].Message, "capability", "demo-review", "1") {
			t.Fatalf("cause.message 没有点名 {kind,name,version}: %q", doc.Details.Causes[0].Message)
		}
	})

	t.Run("pinned_gone_second_slot", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-gone2"
		env.create(t, slug, pbTestTwoCapBundle(slug))
		code, raw := env.install(t, slug, "alpha", pbTestValues("demo"))
		if code != http.StatusOK {
			t.Fatalf("install 应 200, got %d: %s", code, raw)
		}
		var installed pbTestReleaseReceipt
		if err := json.Unmarshal(raw, &installed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/alpha"), nil); rec.Code != http.StatusOK {
			t.Fatalf("卸载应 200: %s", rec.Body.String())
		}
		if _, err := env.db.ExecContext(t.Context(),
			`DELETE FROM capability_versions WHERE namespace_id = ? AND name = 'demo-b'`, env.nsID); err != nil {
			t.Fatalf("模拟 GC 硬删第二个能力: %v", err)
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusConflict {
			t.Fatalf("第二个能力被硬删后应 409, got %d: %s", code, raw)
		}
		doc := decodeRestoreErr(t, http.StatusConflict, raw)
		if doc.Reason != "pinned_gone" {
			t.Fatalf("reason = %q, want pinned_gone", doc.Reason)
		}
		if len(doc.Details.Causes) != 1 || doc.Details.Causes[0].Field != "pinned.capabilities[1]" {
			t.Fatalf("causes = %+v, want field=pinned.capabilities[1]（坏的是第二个对象）", doc.Details.Causes)
		}
	})

	t.Run("pin_missing_source", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-src"
		env.create(t, slug, pbTestBundle(slug))
		code, raw := env.install(t, slug, "alpha", pbTestValues("demo"))
		if code != http.StatusOK {
			t.Fatalf("install 应 200, got %d: %s", code, raw)
		}
		var installed pbTestReleaseReceipt
		if err := json.Unmarshal(raw, &installed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/alpha"), nil); rec.Code != http.StatusOK {
			t.Fatalf("卸载应 200: %s", rec.Body.String())
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusConflict {
			t.Fatalf("缺源复原应 409, got %d: %s", code, raw)
		}
		doc := decodeRestoreErr(t, http.StatusConflict, raw)
		if doc.Reason != "pin_missing_source" {
			t.Fatalf("reason = %q, want pin_missing_source", doc.Reason)
		}
		if len(doc.Details.Causes) != 1 || doc.Details.Causes[0].Field != "pinned.sources[0]" {
			t.Fatalf("causes = %+v, want 一条 pinned.sources[0]", doc.Details.Causes)
		}
	})
}
