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
			KindOf  string `json:"kindOf"`
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

// pbTestBundleV1 是 pbTestBundle 的同构包，只把入口文档的 metadata.version 钉成 1。
// 「钉住的那份正文被回收」那条用例要按版本号删行、并在报错里断言版本字面 v1，
// 而 pbTestBundle 声明的版本是 3（版本号必须可预期，不能靠猜）。
func pbTestBundleV1(slug string) map[string]string {
	files := pbTestBundle(slug)
	files["playbook.yaml"] = pbTestEntryDoc(slug, 1,
		[]string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"})
	return files
}

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

	// 卸载（源的物理删除）之后的复原不再 409：源按「钉住的包版本 + 这次安装的回执 values」重算重建（#632）。
	// 这条用例保留原来的价值：卸载把源**物理**删掉，复原不会因此被拒；重建出来的行进 created（不是 skipped）。
	t.Run("rebuilds_missing_source", func(t *testing.T) {
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
		if n := env.srcCount(t); n != 0 {
			t.Fatalf("卸载后源应已物理删除, 还剩 %d 行", n)
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusOK {
			t.Fatalf("缺源复原应 200（按钉住的包版本重算重建）, got %d: %s", code, raw)
		}
		var restored pbTestRestoreReceipt
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatalf("decode restore receipt: %v (%s)", err, raw)
		}
		if len(restored.Created.Sources) != 1 || restored.Created.Sources[0] != "demo-on-push" {
			t.Fatalf("created.sources = %v, want [demo-on-push]", restored.Created.Sources)
		}
		if len(restored.Skipped.Sources) != 0 {
			t.Fatalf("skipped.sources = %v, want 空（库里没有同名源可复用）", restored.Skipped.Sources)
		}
		// 快照里那一项源的 kindOf 必须原样落到回执的 created.pinned 上（它是快照与重建结果的衔接点）。
		var srcPin bool
		for _, pin := range restored.Created.Pinned {
			if pin.Kind != "source" {
				continue
			}
			srcPin = true
			if pin.Name != "demo-on-push" || pin.KindOf != "webhook" {
				t.Fatalf("源的 pinned = %+v, want {kind:source name:demo-on-push kindOf:webhook}", pin)
			}
		}
		if !srcPin {
			t.Fatalf("created.pinned 里没有源那一项: %+v", restored.Created.Pinned)
		}
		if n := env.srcCount(t); n != 1 {
			t.Fatalf("复原后源应重新建出来, 现在 %d 行", n)
		}
	})

	// 钉住的那份正文没了（版本行被物理回收 + 当前版本号被推走）→ 再没有重算的原料，仍然是 409。
	// 这是「重建」与「假装重建」的分界线：正文的每个字节都要有出处，出处没了就只能让人补源。
	t.Run("pin_missing_source_without_pinned_body", func(t *testing.T) {
		env := newPlaybookTestEnv(t)
		slug := "pr-restore-src-gone"
		env.create(t, slug, pbTestBundleV1(slug))
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
		// (a) 删掉钉住的那份正文（模拟 GC 回收版本行）。
		del, err := env.db.ExecContext(t.Context(),
			`DELETE FROM playbook_versions
			  WHERE playbook_id = (SELECT id FROM playbooks WHERE namespace_id = ? AND slug = ?) AND version = 1`,
			env.nsID, slug)
		if err != nil {
			t.Fatalf("删版本行: %v", err)
		}
		if n, err := del.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("删版本行应正好 1 行, got %d (%v)", n, err)
		}
		// (b) 把「当前版本」推走：resolvePlaybookFilesOn 在版本行缺失且 version == 当前版本时会
		// 回退到 playbooks.files（internal/store/playbook_tx.go 的三条分支），不改就还能重算。
		if _, err := env.db.ExecContext(t.Context(),
			`UPDATE playbooks SET version = 2 WHERE namespace_id = ? AND slug = ?`, env.nsID, slug); err != nil {
			t.Fatalf("把当前版本推走: %v", err)
		}
		code, raw = env.restore(t, slug, "alpha", map[string]any{"valuesDigest": installed.ValuesDigest})
		if code != http.StatusConflict {
			t.Fatalf("钉住的正文没了复原应 409, got %d: %s", code, raw)
		}
		doc := decodeRestoreErr(t, http.StatusConflict, raw)
		if doc.Reason != "pin_missing_source" {
			t.Fatalf("reason = %q, want pin_missing_source", doc.Reason)
		}
		if len(doc.Details.Causes) != 1 || doc.Details.Causes[0].Field != "pinned.sources[0]" {
			t.Fatalf("causes = %+v, want 一条 pinned.sources[0]", doc.Details.Causes)
		}
		// 钉住的是「版本行已不可用」那一条分支（而不是别的不可重算原因）：消息里带版本字面 + 那句判断。
		if !containsAll(doc.Details.Causes[0].Message, "kind=source", "demo-on-push", "v1", "已不可用") {
			t.Fatalf("cause.message 没有点名 {kind,name,钉住的版本}: %q", doc.Details.Causes[0].Message)
		}
		if n := env.srcCount(t); n != 0 {
			t.Fatalf("409 之后源不该被写出来, 现在 %d 行", n)
		}
	})
}
