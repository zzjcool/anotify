package server

// 包版本历史与按版本安装的**传输面**验收：只断言 handler 层（列表形状、请求里的 `version` 进得去、回执的 `playbook.version` / `playbook.filesDigest` 指回被钉住的那一版、404 与 400）；领域语义由 internal/store 的用例覆盖。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/store"
)

type pbTestVersionEntry struct {
	Version     int    `json:"version"`
	FilesDigest string `json:"filesDigest"`
	CreatedAt   string `json:"createdAt"`
	CreatedBy   string `json:"createdBy"`
	Current     bool   `json:"current"`
}

type pbTestVersionList struct {
	Total          int                  `json:"total"`
	CurrentVersion int                  `json:"currentVersion"`
	Versions       []pbTestVersionEntry `json:"versions"`
}

type pbTestPinReceipt struct {
	Kind     string `json:"kind"`
	Playbook struct {
		Name        string `json:"name"`
		Version     int    `json:"version"`
		FilesDigest string `json:"filesDigest"`
	} `json:"playbook"`
	Install struct {
		ID      string `json:"id"`
		Release string `json:"release"`
	} `json:"install"`
	Created struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
	} `json:"created"`
}

// pbTestVersionedBundle 是同一 slug 的第 n 版：只有能力脚本正文不同（digest 因此不同）。
func pbTestVersionedBundle(slug string, version int, script string) map[string]string {
	capPath := "capabilities/review.yaml"
	srcPath := "sources/on-push.yaml"
	return map[string]string{
		"playbook.yaml": pbTestEntryDoc(slug, version, []string{capPath}, []string{srcPath}),
		capPath:         pbTestCapabilityDocBody("{{release.name}}-{{values.project}}-review", script),
		srcPath:         pbTestSourceDoc("{{release.name}}-{{values.project}}-on-push", []string{"{{release.name}}-{{values.project}}-review"}, ""),
	}
}

// pbTestCapabilityDocBody 与 pbTestCapabilityDoc 同形，但脚本正文由调用方给。
func pbTestCapabilityDocBody(name, script string) string {
	return "apiVersion: anotify/v1\nkind: Capability\nmetadata:\n  name: '" + name + "'\n" +
		"spec:\n  params:\n    type: object\n  next: []\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n" +
		"        #!/bin/sh\n        " + script + "\n"
}

func (e *playbookTestEnv) versions(t *testing.T, slug string) pbTestVersionList {
	t.Helper()
	rec := e.do(t, "GET", e.path("/"+slug+"/versions"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("versions 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list pbTestVersionList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode versions: %v (%s)", err, rec.Body.String())
	}
	return list
}

func (e *playbookTestEnv) replace(t *testing.T, slug string, files map[string]string) pbTestEnvelope {
	t.Helper()
	row, err := e.db.GetPlaybook(t.Context(), e.nsID, slug)
	if err != nil {
		t.Fatalf("get playbook %q: %v", slug, err)
	}
	rv := store.ResourceVersion(row.ID, row.ResourceVersion)
	rec := e.do(t, "PUT", e.path("/"+slug), pbTestPutBody(t, slug, files, rv))
	if rec.Code != http.StatusOK {
		t.Fatalf("替换包 %q 应 200, got %d: %s", slug, rec.Code, rec.Body.String())
	}
	var env pbTestEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode replace envelope: %v", err)
	}
	return env
}

// installPinned 是 install 带 version 的版本（release / version 各自可选）。
func (e *playbookTestEnv) installPinned(t *testing.T, slug, release string, version any, values map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"values": values}
	if release != "" {
		body["release"] = release
	}
	if version != nil {
		body["version"] = version
	}
	rec := e.do(t, "POST", e.path("/"+slug+"/install"), body)
	return rec.Code, rec.Body.Bytes()
}

// TestPlaybookVersionsListShape：建包 → 一版；内容变了换版本 → 两版，当前版本带标记。
func TestPlaybookVersionsListShape(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-versions"
	env.create(t, slug, pbTestVersionedBundle(slug, 1, "echo v1"))

	list := env.versions(t, slug)
	if list.Total != 1 || list.CurrentVersion != 1 || len(list.Versions) != 1 {
		t.Fatalf("建包后的版本列表 = %+v", list)
	}
	v1Digest := list.Versions[0].FilesDigest
	if !strings.HasPrefix(v1Digest, "sha256:") || len(v1Digest) != len("sha256:")+64 {
		t.Fatalf("filesDigest 应是 canonical sha256: %q", v1Digest)
	}
	if !list.Versions[0].Current || list.Versions[0].CreatedAt == "" {
		t.Fatalf("当前版本那一行应带 current + createdAt: %+v", list.Versions[0])
	}

	env.replace(t, slug, pbTestVersionedBundle(slug, 2, "echo v2"))
	list = env.versions(t, slug)
	if list.Total != 2 || list.CurrentVersion != 2 || len(list.Versions) != 2 {
		t.Fatalf("换版后的版本列表 = %+v", list)
	}
	if list.Versions[0].Version != 2 || !list.Versions[0].Current || list.Versions[1].Current {
		t.Fatalf("只有最新的那一行是当前版本: %+v", list.Versions)
	}
	if list.Versions[1].FilesDigest != v1Digest {
		t.Fatalf("v1 的 digest 被改写了: %q → %q", v1Digest, list.Versions[1].FilesDigest)
	}

	env.replace(t, slug, pbTestVersionedBundle(slug, 2, "echo v2"))
	if again := env.versions(t, slug); again.Total != 2 {
		t.Fatalf("同内容 PUT 不该追加历史: %+v", again)
	}
}

// TestPlaybookInstallPinnedVersion：按 v1 装一份、按当前（v2）装一份 —— 两份回执的 version / filesDigest 不同且各自指回正确的版本。
func TestPlaybookInstallPinnedVersion(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-pinned"
	env.create(t, slug, pbTestVersionedBundle(slug, 1, "echo v1"))
	v1Digest := env.versions(t, slug).Versions[0].FilesDigest
	env.replace(t, slug, pbTestVersionedBundle(slug, 2, "echo v2"))
	versions := env.versions(t, slug)
	var v2Digest string
	for _, v := range versions.Versions {
		if v.Version == 2 {
			v2Digest = v.FilesDigest
		}
	}

	code, raw := env.installPinned(t, slug, "old", 1, pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("按 v1 安装应 200, got %d: %s", code, raw)
	}
	var old pbTestPinReceipt
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("decode v1 receipt: %v", err)
	}
	if old.Playbook.Version != 1 || old.Playbook.FilesDigest != v1Digest {
		t.Fatalf("按 v1 装的回执包身份 = %+v（want v1/%s）", old.Playbook, v1Digest)
	}
	if old.Playbook.Name != slug || old.Install.Release != "old" {
		t.Fatalf("回执的包名 / release 不对: %+v / %+v", old.Playbook, old.Install)
	}

	code, raw = env.installPinned(t, slug, "new", nil, pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("不传版本安装应 200, got %d: %s", code, raw)
	}
	var fresh pbTestPinReceipt
	if err := json.Unmarshal(raw, &fresh); err != nil {
		t.Fatalf("decode current receipt: %v", err)
	}
	if fresh.Playbook.Version != 2 || fresh.Playbook.FilesDigest != v2Digest {
		t.Fatalf("不传版本应装当前版本 v2: %+v（want %s）", fresh.Playbook, v2Digest)
	}
	if old.Playbook.FilesDigest == fresh.Playbook.FilesDigest {
		t.Fatal("v1 与 v2 的 filesDigest 不该相同")
	}

	rec := env.do(t, "GET", env.path("/"+slug+"/installs/"+old.Install.ID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("单回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var one pbTestPinReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil {
		t.Fatalf("decode single receipt: %v (%s)", err, rec.Body.String())
	}
	if one.Playbook.Version != 1 || one.Playbook.FilesDigest != v1Digest {
		t.Fatalf("单回执的包身份 = %+v（want v1/%s）", one.Playbook, v1Digest)
	}
}

// TestPlaybookInstallVersionRejected：不存在 → 404 version_not_found；非法值 → 400。
func TestPlaybookInstallVersionRejected(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-version-errors"
	env.create(t, slug, pbTestVersionedBundle(slug, 1, "echo v1"))

	code, raw := env.installPinned(t, slug, "", 9, pbTestValues("demo"))
	if code != http.StatusNotFound {
		t.Fatalf("装不存在的版本应 404, got %d: %s", code, raw)
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode 404: %v", err)
	}
	if doc.Reason != "version_not_found" {
		t.Fatalf("404 的 reason = %q（want version_not_found）", doc.Reason)
	}
	if got := env.installCount(t, slug); got != 0 {
		t.Fatalf("版本不存在的安装留下了 %d 个回执组（应 0 写）", got)
	}

	for _, bad := range []int{0, -1} {
		code, raw := env.installPinned(t, slug, "", bad, pbTestValues("demo"))
		if code != http.StatusBadRequest {
			t.Fatalf("version=%d 应 400, got %d: %s", bad, code, raw)
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode 400: %v", err)
		}
		if doc.Reason != "invalid_playbook" {
			t.Fatalf("version=%d 的 reason = %q（want invalid_playbook）", bad, doc.Reason)
		}
	}
}

// TestPlaybookRenderPinnedVersion：render 也吃 version（预览与安装必须是同一份文件）。
func TestPlaybookRenderPinnedVersion(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-render-pinned"
	env.create(t, slug, pbTestVersionedBundle(slug, 1, "echo v1"))
	env.replace(t, slug, pbTestVersionedBundle(slug, 2, "echo v2"))

	rec := env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{
		"values":  pbTestValues("demo"),
		"version": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("按 v1 渲染应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var rendered struct {
		Files        map[string]string `json:"files"`
		Capabilities []json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rendered); err != nil {
		t.Fatalf("decode render: %v (%s)", err, rec.Body.String())
	}
	body := string(rec.Body.Bytes())
	if !strings.Contains(body, "echo v1") || strings.Contains(body, "echo v2") {
		t.Fatalf("按 v1 渲染却拿到了别的版本: %s", body)
	}

	rec = env.do(t, "POST", env.path("/"+slug+"/render"), map[string]any{
		"values":  pbTestValues("demo"),
		"version": 9,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("渲染不存在的版本应 404, got %d: %s", rec.Code, rec.Body.String())
	}
}
