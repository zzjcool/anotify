package server

// #553：列表/详情的 ?include=uninstalled（已卸载多 uninstalledAt/objectsGone/pinned），
// 且生效中实例的列表、详情、YAML 三条读数逐字节不变（每组都有对照组）。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// pbTestHistoryGroup / List 只在本文件用：多一个 uninstalledAt（生产 DTO 也只在已卸载组上给）。
type pbTestHistoryGroup struct {
	Release       string   `json:"release"`
	ValuesDigest  string   `json:"valuesDigest"`
	Attempts      int      `json:"attempts"`
	LastInstallID string   `json:"lastInstallId"`
	UninstalledAt string   `json:"uninstalledAt"`
	Capabilities  []string `json:"capabilities"`
	Sources       []string `json:"sources"`
}

type pbTestHistoryList struct {
	Total    int                  `json:"total"`
	Installs []pbTestHistoryGroup `json:"installs"`
}

// pbTestHistoryDetail 在 pbTestReleaseDetail 上补三个已卸载视图的键。
type pbTestHistoryDetail struct {
	pbTestReleaseDetail
	UninstalledAt string `json:"uninstalledAt"`
	ObjectsGone   bool   `json:"objectsGone"`
	Pinned        []struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Version int    `json:"version"`
		Digest  string `json:"digest"`
		KindOf  string `json:"kindOf"`
	} `json:"pinned"`
}

func (e *playbookTestEnv) installsWith(t *testing.T, slug, query string) (pbTestHistoryList, string) {
	t.Helper()
	path := e.path("/" + slug + "/installs")
	if query != "" {
		path += "?" + query
	}
	rec := e.do(t, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("installs(%q) 应 200, got %d: %s", query, rec.Code, rec.Body.String())
	}
	var list pbTestHistoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode installs(%q): %v (%s)", query, err, rec.Body.String())
	}
	return list, rec.Body.String()
}

// pbTestHistoryUninstalledGroup 取出某个 release 的组（找不到就 Fatal）。
func pbTestHistoryUninstalledGroup(t *testing.T, list pbTestHistoryList, release string) pbTestHistoryGroup {
	t.Helper()
	for _, group := range list.Installs {
		if group.Release == release {
			return group
		}
	}
	t.Fatalf("清单里没有 release %q：%+v", release, list.Installs)
	return pbTestHistoryGroup{}
}

func TestPlaybookInstallHistoryIncludesUninstalled(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-history"
	env.create(t, slug, pbTestExportBundle(slug))

	if code, body := env.install(t, slug, "live", map[string]any{"project": "live"}); code != http.StatusOK {
		t.Fatalf("装 live 应 200, got %d: %s", code, body)
	}
	code, body := env.install(t, slug, "gone", map[string]any{"project": "gone"})
	if code != http.StatusOK {
		t.Fatalf("装 gone 应 200, got %d: %s", code, body)
	}
	var receipt pbTestReleaseReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatalf("decode 安装回执: %v (%s)", err, body)
	}

	// 对照组：卸载前，生效中的两个组都没有 uninstalledAt 键（不是空串，是键不存在）。
	before := env.installs(t, slug)
	if before.Total != 2 {
		t.Fatalf("卸载前应有两组实例，got %d", before.Total)
	}
	if raw := e2eInstallsRawKeys(t, env, slug, ""); raw["gone"]["uninstalledAt"] {
		t.Fatal("生效中的组不该出现 uninstalledAt 键")
	}

	if rec := env.uninstall(t, slug, "gone", ""); rec.Code != http.StatusOK {
		t.Fatalf("卸载 gone 应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// ① 默认清单：已卸载的实例不出现（逐字不变的口径）。
	def, defRaw := env.installsWith(t, slug, "")
	if def.Total != 1 || len(def.Installs) != 1 || def.Installs[0].Release != "live" {
		t.Fatalf("默认清单应只剩 live：%+v", def.Installs)
	}
	if strings.Contains(defRaw, "uninstalledAt") {
		t.Fatalf("默认清单不得出现 uninstalledAt：%s", defRaw)
	}

	// ② include=uninstalled：连带已卸载，多 uninstalledAt，lastInstallId 指到最后一次安装。
	all, _ := env.installsWith(t, slug, "include=uninstalled")
	if all.Total != 2 || len(all.Installs) != 2 {
		t.Fatalf("历史清单应是 live + gone 两组：%+v", all.Installs)
	}
	goneGroup := pbTestHistoryUninstalledGroup(t, all, "gone")
	if _, err := time.Parse(time.RFC3339, goneGroup.UninstalledAt); err != nil {
		t.Fatalf("已卸载组的 uninstalledAt 应是 RFC3339，got %q: %v", goneGroup.UninstalledAt, err)
	}
	if goneGroup.LastInstallID != receipt.Install.ID {
		t.Fatalf("已卸载组的 lastInstallId = %q, want %q", goneGroup.LastInstallID, receipt.Install.ID)
	}
	if len(goneGroup.Capabilities) != 0 || len(goneGroup.Sources) != 0 {
		t.Fatalf("已卸载组的对象引用应如实为空：%+v", goneGroup)
	}
	if liveGroup := pbTestHistoryUninstalledGroup(t, all, "live"); liveGroup.UninstalledAt != "" {
		t.Fatalf("生效中的 live 不该带 uninstalledAt：%q", liveGroup.UninstalledAt)
	}

	// ③ include 只认 uninstalled 一个值：其它值 400 value_invalid（不静默回落默认）。
	for _, path := range []string{"/" + slug + "/installs?include=nope", "/" + slug + "/releases/live?include=nope"} {
		rec := env.do(t, "GET", env.path(path), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), reasonValueInvalid) {
			t.Fatalf("GET %s 应 400 value_invalid, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func e2eInstallsRawKeys(t *testing.T, env *playbookTestEnv, slug, query string) map[string]map[string]bool {
	t.Helper()
	path := env.path("/" + slug + "/installs")
	if query != "" {
		path += "?" + query
	}
	rec := env.do(t, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("installs 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Installs []map[string]json.RawMessage `json:"installs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode installs keys: %v (%s)", err, rec.Body.String())
	}
	out := map[string]map[string]bool{}
	for _, group := range doc.Installs {
		var release string
		if raw, ok := group["release"]; ok {
			_ = json.Unmarshal(raw, &release)
		}
		keys := map[string]bool{}
		for key := range group {
			keys[key] = true
		}
		out[release] = keys
	}
	return out
}

func (e *playbookTestEnv) releaseDetailWith(t *testing.T, slug, release, query string) (pbTestHistoryDetail, string) {
	t.Helper()
	path := e.path("/" + slug + "/releases/" + release)
	if query != "" {
		path += "?" + query
	}
	rec := e.do(t, "GET", path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情(%q) 应 200, got %d: %s", query, rec.Code, rec.Body.String())
	}
	var doc pbTestHistoryDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode 详情(%q): %v (%s)", query, err, rec.Body.String())
	}
	return doc, rec.Body.String()
}

func TestPlaybookReleaseDetailUninstalledReadable(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pb-history-detail"
	env.create(t, slug, pbTestExportBundle(slug))

	if code, body := env.install(t, slug, "gone", map[string]any{"project": "demo"}); code != http.StatusOK {
		t.Fatalf("装 gone 应 200, got %d: %s", code, body)
	}
	code, body := env.install(t, slug, "gone", map[string]any{"project": "demo"})
	if code != http.StatusOK {
		t.Fatalf("重装 gone 应 200, got %d: %s", code, body)
	}
	var receipt pbTestReleaseReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatalf("decode 回执: %v", err)
	}

	// 对照组：生效中的详情，带不带 include 完全一样（这是「逐字不变」的机制）。
	activeDefault, activeRaw := env.releaseDetailWith(t, slug, "gone", "")
	if activeDefault.ObjectsGone || activeDefault.UninstalledAt != "" || strings.Contains(activeRaw, "objectsGone") {
		t.Fatalf("生效中的详情不得带已卸载视图字段：%s", activeRaw)
	}
	_, activeWithParam := env.releaseDetailWith(t, slug, "gone", "include=uninstalled")
	if activeWithParam != activeRaw {
		t.Fatalf("生效中实例带 include=uninstalled 的响应必须与缺省逐字节相同\n缺省: %s\n带参: %s", activeRaw, activeWithParam)
	}

	if rec := env.uninstall(t, slug, "gone", ""); rec.Code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 缺省口径不变：已卸载 → 404 release_not_found。
	rec := env.do(t, "GET", env.path("/"+slug+"/releases/gone"), nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "release_not_found") {
		t.Fatalf("已卸载的详情缺省应 404 release_not_found, got %d: %s", rec.Code, rec.Body.String())
	}

	doc, raw := env.releaseDetailWith(t, slug, "gone", "include=uninstalled")
	if !doc.ObjectsGone {
		t.Fatalf("已卸载视图必须带 objectsGone: true：%s", raw)
	}
	if _, err := time.Parse(time.RFC3339, doc.UninstalledAt); err != nil {
		t.Fatalf("uninstalledAt 应是 RFC3339，got %q: %v", doc.UninstalledAt, err)
	}
	if doc.LastInstallID != receipt.Install.ID {
		t.Fatalf("lastInstallId = %q, want %q（最后一次安装的回执）", doc.LastInstallID, receipt.Install.ID)
	}
	// 对象已删：两个列表都如实为空（不是没装过，所以另有 objectsGone）。
	if len(doc.Objects.Capabilities) != 0 || len(doc.Objects.Sources) != 0 {
		t.Fatalf("已卸载实例的 objects 应如实为空：%+v", doc.Objects)
	}
	if len(doc.Groups) == 0 || len(doc.Groups[0].Objects.Capabilities) != 0 || len(doc.Groups[0].Objects.Sources) != 0 {
		t.Fatalf("每个参数组的 objects 也要为空：%+v", doc.Groups)
	}
	// pinned 来自回执：2 能力（带 version/digest）+ 1 源（只带 kind）。
	if len(doc.Pinned) != 3 {
		t.Fatalf("pinned 应有 2 能力 + 1 源，got %+v", doc.Pinned)
	}
	var caps, srcs int
	for _, pin := range doc.Pinned {
		switch pin.Kind {
		case "capability":
			caps++
			if pin.Name == "" || pin.Version < 1 || !strings.HasPrefix(pin.Digest, "sha256:") {
				t.Fatalf("能力的 pinned 要带 name/version/digest：%+v", pin)
			}
		case "source":
			srcs++
			if pin.Name == "" || pin.KindOf == "" || pin.Version != 0 || pin.Digest != "" {
				t.Fatalf("源的 pinned 只带 name/kindOf：%+v", pin)
			}
		default:
			t.Fatalf("pinned.kind 只有 capability / source，got %q", pin.Kind)
		}
	}
	if caps != 2 || srcs != 1 {
		t.Fatalf("pinned 分布不对：caps=%d srcs=%d", caps, srcs)
	}

	// 验收 3：详情页的「查看 YAML（安装时）」真的能下到安装时快照（含 installId）。
	yamlRec := env.do(t, "GET", env.path("/"+slug+"/releases/gone/yaml?as=installed"), nil)
	if yamlRec.Code != http.StatusOK {
		t.Fatalf("as=installed 应 200, got %d: %s", yamlRec.Code, yamlRec.Body.String())
	}
	if !strings.Contains(yamlRec.Body.String(), "install: "+receipt.Install.ID) {
		t.Fatalf("安装时导出应带 installId %q：%s", receipt.Install.ID, yamlRec.Body.String())
	}
	if rec := env.do(t, "GET", env.path("/"+slug+"/releases/gone/yaml"), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("当前生效视图对已卸载仍应 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// 同名对象被别的实例重装，也不得漏进这份已卸载视图。
	if code, body := env.install(t, slug, "live", map[string]any{"project": "demo"}); code != http.StatusOK {
		t.Fatalf("装 live 应 200, got %d: %s", code, body)
	}
	again, againRaw := env.releaseDetailWith(t, slug, "gone", "include=uninstalled")
	if len(again.Objects.Capabilities) != 0 || len(again.Objects.Sources) != 0 {
		t.Fatalf("同名对象被 live 重装后，已卸载视图仍须为空：%s", againRaw)
	}
	liveDoc, _ := env.releaseDetailWith(t, slug, "live", "")
	if len(liveDoc.Objects.Capabilities) != 2 || len(liveDoc.Objects.Sources) != 1 {
		t.Fatalf("生效中的 live 详情应照旧列出对象：%+v", liveDoc.Objects)
	}
}
