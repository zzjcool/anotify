package server

// #548：实例详情读模型的传输面验收 —— 对象归属按回执 id、分组与 groups[0] 镜像顶层、receipt 与单回执端点一致、
// 两个 404、dependencies 去重取最高 version、空列表是 [] 且读路径 0 写（口径真源见 api/resource.yaml）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type pbRelDetail struct {
	raw  []byte
	doc  PlaybookRelease
	keys map[string]any
}

func (e *playbookTestEnv) releaseDetail(t *testing.T, slug, release string) pbRelDetail {
	t.Helper()
	rec := e.do(t, "GET", e.path("/"+slug+"/releases/"+release), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s/releases/%s 应 200, got %d: %s", slug, release, rec.Code, rec.Body.String())
	}
	var out pbRelDetail
	out.raw = rec.Body.Bytes()
	if err := json.Unmarshal(out.raw, &out.doc); err != nil {
		t.Fatalf("decode 详情: %v (%s)", err, out.raw)
	}
	if err := json.Unmarshal(out.raw, &out.keys); err != nil {
		t.Fatalf("decode 顶层键: %v (%s)", err, out.raw)
	}
	return out
}

func (d PlaybookReleaseObjects) capNames() []string {
	out := make([]string, 0, len(d.Capabilities))
	for _, c := range d.Capabilities {
		out = append(out, c.Name)
	}
	return out
}

func (d PlaybookReleaseObjects) srcNames() []string {
	out := make([]string, 0, len(d.Sources))
	for _, s := range d.Sources {
		out = append(out, s.Name)
	}
	return out
}

func TestPlaybookReleaseDetailIsolatesReleases(t *testing.T) {
	env := newPlaybookTestEnv(t)
	neighbor := "pb-rel-neighbor"
	env.create(t, neighbor, pbTestExportNeighborBundle(neighbor))
	if code, raw := env.install(t, neighbor, "default", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install 邻居包应 200, got %d: %s", code, raw)
	}

	slug := "pb-rel"
	env.create(t, slug, pbTestReleaseBundle(slug))
	for _, release := range []string{"blue", "green"} {
		if code, raw := env.install(t, slug, release, pbTestValues("demo")); code != http.StatusOK {
			t.Fatalf("install %s 应 200, got %d: %s", release, code, raw)
		}
	}

	rows, err := env.db.ListCapabilities(t.Context(), env.nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	digestOf := map[string]string{}
	for _, row := range rows {
		digestOf[fmt.Sprintf("%s@%d", row.Name, row.Version)] = row.Digest
	}

	for _, release := range []string{"blue", "green"} {
		got := env.releaseDetail(t, slug, release)
		wantCap := release + "-demo-review@1"
		wantSrc := release + "-demo-on-push"
		if names := got.doc.Objects.capNames(); !reflect.DeepEqual(names, []string{release + "-demo-review"}) {
			t.Fatalf("%s 的能力集合串了：%v（只应含它自己装出的 %s）", release, names, wantCap)
		}
		if names := got.doc.Objects.srcNames(); !reflect.DeepEqual(names, []string{wantSrc}) {
			t.Fatalf("%s 的源集合串了：%v（只应含 %s）", release, names, wantSrc)
		}
		cap := got.doc.Objects.Capabilities[0]
		if want := digestOf[wantCap]; want == "" || cap.Digest != want {
			t.Fatalf("%s 能力的 digest 应取库里当前生效行 %q, got %q", release, want, cap.Digest)
		}
		if got.doc.Objects.Sources[0].Kind != "webhook" {
			t.Fatalf("%s 源的 kind 应取 sources.kind（webhook）, got %q", release, got.doc.Objects.Sources[0].Kind)
		}
		if got.doc.Params.Count != 1 || len(got.doc.Groups) != 1 {
			t.Fatalf("%s 应只有 1 组参数, got count=%d groups=%d", release, got.doc.Params.Count, len(got.doc.Groups))
		}
		if got.doc.Groups[0].ValuesDigest != got.doc.ValuesDigest ||
			got.doc.Groups[0].LastInstallID != got.doc.LastInstallID ||
			got.doc.Groups[0].InstalledAt != got.doc.InstalledAt {
			t.Fatalf("%s 的 groups[0] 没镜像顶层：%+v vs %s/%s/%s", release, got.doc.Groups[0],
				got.doc.ValuesDigest, got.doc.InstalledAt, got.doc.LastInstallID)
		}
		if got.doc.Groups[0].Attempts != 1 {
			t.Fatalf("%s 的 attempts 应是 1, got %d", release, got.doc.Groups[0].Attempts)
		}
		if got.doc.Release != release {
			t.Fatalf("release 字段应是 %q, got %q", release, got.doc.Release)
		}
		if len(got.doc.Receipt.Created.Capabilities) != 1 {
			t.Fatalf("%s 的回执原文应带 created: %+v", release, got.doc.Receipt.Created)
		}
		for _, foreign := range []string{"demo-manual-review", "demo-manual-push"} {
			if strings.Contains(string(got.raw), foreign) {
				t.Fatalf("%s 的详情里出现了别人的对象 %q：%s", release, foreign, got.raw)
			}
		}
	}
	neighborDetail := env.releaseDetail(t, neighbor, "default")
	if names := neighborDetail.doc.Objects.capNames(); !reflect.DeepEqual(names, []string{"demo-manual-review"}) {
		t.Fatalf("邻居包的能力集合不对：%v", names)
	}
}

func TestPlaybookReleaseDetailGroupsByValues(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pb-rel-groups"
	env.create(t, slug, pbTestReleaseBundle(slug))

	for _, project := range []string{"demo", "demo2", "demo2"} {
		if code, raw := env.install(t, slug, "blue", pbTestValues(project)); code != http.StatusOK {
			t.Fatalf("install project=%s 应 200, got %d: %s", project, code, raw)
		}
	}

	got := env.releaseDetail(t, slug, "blue")
	if got.doc.Params.Count != 2 {
		t.Fatalf("params.count 应是不同 valuesDigest 的个数 2, got %d", got.doc.Params.Count)
	}
	if got.doc.Params.Digest != got.doc.ValuesDigest {
		t.Fatalf("params.digest 应与顶层 valuesDigest 同值：%q vs %q", got.doc.Params.Digest, got.doc.ValuesDigest)
	}
	if len(got.doc.Groups) != 2 {
		t.Fatalf("应有 2 组, got %d", len(got.doc.Groups))
	}
	newest, older := got.doc.Groups[0], got.doc.Groups[1]
	if newest.ValuesDigest != got.doc.ValuesDigest || newest.InstalledAt != got.doc.InstalledAt ||
		newest.LastInstallID != got.doc.LastInstallID {
		t.Fatalf("groups[0] 应镜像顶层（最近安装的组在前）：%+v vs %s/%s/%s", newest,
			got.doc.ValuesDigest, got.doc.InstalledAt, got.doc.LastInstallID)
	}
	if newest.Attempts != 2 {
		t.Fatalf("最新那组装了两次, attempts 应是 2, got %d", newest.Attempts)
	}
	if older.Attempts != 1 {
		t.Fatalf("较早那组装了一次, attempts 应是 1, got %d", older.Attempts)
	}
	if newest.LastInstallID == older.LastInstallID {
		t.Fatalf("两组的 lastInstallId 必须不同（各自最新一次安装）")
	}
	if names := newest.Objects.capNames(); !reflect.DeepEqual(names, []string{"blue-demo2-review"}) {
		t.Fatalf("最新那组的能力集合不对：%v", names)
	}
	if names := older.Objects.capNames(); !reflect.DeepEqual(names, []string{"blue-demo-review"}) {
		t.Fatalf("较早那组的能力集合不对：%v", names)
	}
	if names := newest.Objects.srcNames(); !reflect.DeepEqual(names, []string{"blue-demo2-on-push"}) {
		t.Fatalf("最新那组的源集合不对：%v", names)
	}
	if names := older.Objects.srcNames(); !reflect.DeepEqual(names, []string{"blue-demo-on-push"}) {
		t.Fatalf("较早那组的源集合不对：%v", names)
	}
	wantCaps := []string{"blue-demo-review", "blue-demo2-review"}
	if names := got.doc.Objects.capNames(); !reflect.DeepEqual(names, wantCaps) {
		t.Fatalf("顶层能力集合应是两组并集 %v, got %v", wantCaps, names)
	}
	if !sort.StringsAreSorted(got.doc.Objects.capNames()) || !sort.StringsAreSorted(got.doc.Objects.srcNames()) {
		t.Fatalf("对象必须按名字升序：%v / %v", got.doc.Objects.capNames(), got.doc.Objects.srcNames())
	}
	if len(got.doc.Objects.Sources) != 2 {
		t.Fatalf("顶层源集合应是两组并集, got %v", got.doc.Objects.srcNames())
	}
	if got.doc.Receipt.Install.ID != got.doc.LastInstallID {
		t.Fatalf("receipt.install.id 应是最新一次安装 %q, got %q", got.doc.LastInstallID, got.doc.Receipt.Install.ID)
	}
	if got.doc.InstalledAt != got.doc.Receipt.Install.At {
		t.Fatalf("installedAt 应取回执的 install.at：%q vs %q", got.doc.InstalledAt, got.doc.Receipt.Install.At)
	}
	if _, err := time.Parse(time.RFC3339, got.doc.InstalledAt); err != nil {
		t.Fatalf("installedAt 应是 RFC3339：%v (%q)", err, got.doc.InstalledAt)
	}
	for _, key := range []string{"release", "valuesDigest", "installedAt", "lastInstallId",
		"objects", "dependencies", "params", "groups", "receipt"} {
		if _, ok := got.keys[key]; !ok {
			t.Fatalf("响应缺顶层键 %q：%s", key, got.raw)
		}
	}
	if len(got.keys) != 9 {
		t.Fatalf("顶层应恰好 9 个键, got %d: %s", len(got.keys), got.raw)
	}
}

func TestPlaybookReleaseDetailReceiptMatchesInstallEndpoint(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pb-rel-receipt"
	env.create(t, slug, pbTestReleaseBundle(slug))
	firstID := ""
	for i, project := range []string{"demo", "demo2"} {
		code, raw := env.install(t, slug, "blue", pbTestValues(project))
		if code != http.StatusOK {
			t.Fatalf("install project=%s 应 200, got %d: %s", project, code, raw)
		}
		if i == 0 {
			var rcpt pbTestReleaseReceipt
			if err := json.Unmarshal(raw, &rcpt); err != nil {
				t.Fatalf("decode 第一次回执: %v (%s)", err, raw)
			}
			firstID = rcpt.Install.ID
		}
	}
	got := env.releaseDetail(t, slug, "blue")
	if firstID == "" || got.doc.LastInstallID == firstID {
		t.Fatalf("lastInstallId 应是最新那次安装（不是 %q）, got %q", firstID, got.doc.LastInstallID)
	}

	rec := env.do(t, "GET", e2eInstallPath(env, slug, got.doc.LastInstallID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 单回执应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var single map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode 单回执: %v (%s)", err, rec.Body.String())
	}
	var embedded map[string]any
	raw, err := json.Marshal(got.doc.Receipt)
	if err != nil {
		t.Fatalf("重新编码 receipt: %v", err)
	}
	if err := json.Unmarshal(raw, &embedded); err != nil {
		t.Fatalf("decode 详情里的 receipt: %v (%s)", err, raw)
	}
	if !reflect.DeepEqual(embedded, single) {
		t.Fatalf("详情里的 receipt 与单回执端点不一致：\n详情: %v\n端点: %v", embedded, single)
	}
	earlier := env.do(t, "GET", e2eInstallPath(env, slug, firstID), nil)
	if earlier.Code != http.StatusOK {
		t.Fatalf("GET 第一次回执应 200, got %d: %s", earlier.Code, earlier.Body.String())
	}
	var firstReceipt map[string]any
	if err := json.Unmarshal(earlier.Body.Bytes(), &firstReceipt); err != nil {
		t.Fatalf("decode 第一次回执: %v (%s)", err, earlier.Body.String())
	}
	if reflect.DeepEqual(embedded, firstReceipt) {
		t.Fatalf("详情里的 receipt 是第一次安装的，不是最新一次：%v", embedded)
	}
	if strip := strings.TrimSpace(rec.Body.String()); strip != "" {
		if !strings.Contains(string(got.raw), strip) {
			t.Fatalf("回执原文没有逐字节出现在详情里：\n%s\n---\n%s", strip, got.raw)
		}
	}
}

func e2eInstallPath(e *playbookTestEnv, slug, installID string) string {
	return e.path("/" + slug + "/installs/" + installID)
}

func TestPlaybookReleaseDetailErrors(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pb-rel-404"
	env.create(t, slug, pbTestReleaseBundle(slug))
	if code, raw := env.install(t, slug, "gone", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}
	if code, raw := env.install(t, slug, "keep", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}
	if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/gone"), nil); rec.Code != http.StatusOK {
		t.Fatalf("卸载 gone 应 200, got %d: %s", rec.Code, rec.Body.String())
	}

	uninstalled := env.do(t, "GET", env.path("/"+slug+"/releases/gone"), nil)
	if uninstalled.Code != http.StatusNotFound {
		t.Fatalf("已卸载的实例详情应 404, got %d: %s", uninstalled.Code, uninstalled.Body.String())
	}
	var goneDoc pbTestStatusDoc
	if err := json.Unmarshal(uninstalled.Body.Bytes(), &goneDoc); err != nil {
		t.Fatalf("decode 404 响应: %v (%s)", err, uninstalled.Body.String())
	}
	if goneDoc.Code != http.StatusNotFound || goneDoc.Reason != "release_not_found" {
		t.Fatalf("已卸载应 404 release_not_found, got %+v", goneDoc)
	}
	if !strings.Contains(uninstalled.Body.String(), `"release_not_found"`) {
		t.Fatalf("响应体里没有 release_not_found 字面量：%s", uninstalled.Body.String())
	}
	if rec := env.do(t, "GET", env.path("/"+slug+"/releases/keep"), nil); rec.Code != http.StatusOK {
		t.Fatalf("另一个 release 不该被卸载影响, got %d: %s", rec.Code, rec.Body.String())
	}

	cases := []struct {
		why, playbook, release string
		code                   int
		reason                 string
	}{
		{"release 从未存在", slug, "nope", http.StatusNotFound, "release_not_found"},
		{"release 形状错", slug, "Prod", http.StatusBadRequest, "invalid_playbook"},
		{"包不存在", "pb-rel-nope", "keep", http.StatusNotFound, "NotFound"},
	}
	for _, tc := range cases {
		rec := env.do(t, "GET", env.path("/"+tc.playbook+"/releases/"+tc.release), nil)
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
}

func TestPlaybookReleaseDetailDependenciesDedupMaxVersion(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.withCapabilities(t) // requires 需要能力端点来造「已装好的依赖」
	slug := "pb-rel-deps"
	env.create(t, slug, pbReqBundle(slug, `[{capability: dep-y}, {capability: dep-x}]`))
	env.createRequiresCapability(t, "dep-x", 1)
	env.createRequiresCapability(t, "dep-y", 1)

	if code, raw := env.install(t, slug, "blue", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("第一次 install 应 200, got %d: %s", code, raw)
	}
	env.createRequiresCapability(t, "dep-x", 5)
	if code, raw := env.install(t, slug, "blue", pbTestValues("demo2")); code != http.StatusOK {
		t.Fatalf("第二次 install 应 200, got %d: %s", code, raw)
	}

	got := env.releaseDetail(t, slug, "blue")
	wantTop := []string{"dep-x@5", "dep-y@1"}
	top := make([]string, 0, len(got.doc.Dependencies))
	for _, dep := range got.doc.Dependencies {
		top = append(top, fmt.Sprintf("%s@%d", dep.Capability, dep.Version))
	}
	if !reflect.DeepEqual(top, wantTop) {
		t.Fatalf("顶层 dependencies 应去重取最高版本且升序 %v, got %v", wantTop, top)
	}
	if len(got.doc.Groups) != 2 {
		t.Fatalf("两次不同 values 应两组, got %d", len(got.doc.Groups))
	}
	newest := make([]string, 0, len(got.doc.Groups[0].Dependencies))
	for _, dep := range got.doc.Groups[0].Dependencies {
		newest = append(newest, fmt.Sprintf("%s@%d", dep.Capability, dep.Version))
	}
	if !reflect.DeepEqual(newest, wantTop) {
		t.Fatalf("最新组的 dependencies 应是 %v, got %v", wantTop, newest)
	}
	older := make([]string, 0, len(got.doc.Groups[1].Dependencies))
	for _, dep := range got.doc.Groups[1].Dependencies {
		older = append(older, fmt.Sprintf("%s@%d", dep.Capability, dep.Version))
	}
	if want := []string{"dep-x@1", "dep-y@1"}; !reflect.DeepEqual(older, want) {
		t.Fatalf("较早那组的 dependencies 应是 %v, got %v", want, older)
	}
}

func jsonArrayAt(t *testing.T, root any, path ...string) []any {
	t.Helper()
	node := root
	for i, key := range path {
		m, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("路径 %v 的第 %d 段不是对象：%v", path, i+1, node)
		}
		if node, ok = m[key]; !ok {
			t.Fatalf("响应缺 %v", path)
		}
	}
	arr, ok := node.([]any)
	if !ok {
		t.Fatalf("%v 必须是数组（空列表是 []，不是 null），got %v", path, node)
	}
	return arr
}

func TestPlaybookReleaseDetailEmptyListsAndReadOnly(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pb-rel-empty"
	env.create(t, slug, map[string]string{"playbook.yaml": pbTestEntryDoc(slug, 1, nil, nil)})
	if code, raw := env.install(t, slug, "empty", pbTestValues("demo")); code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}

	got := env.releaseDetail(t, slug, "empty")
	for _, path := range [][]string{
		{"objects", "capabilities"},
		{"objects", "sources"},
		{"dependencies"},
	} {
		if arr := jsonArrayAt(t, got.keys, path...); len(arr) != 0 {
			t.Fatalf("0 对象的实例不应有 %v：%v", path, arr)
		}
	}
	groups := jsonArrayAt(t, got.keys, "groups")
	if len(groups) != 1 || got.doc.Groups[0].Attempts != 1 {
		t.Fatalf("groups 恒 ≥ 1：count=%d groups=%v", got.doc.Params.Count, groups)
	}
	for _, path := range [][]string{
		{"objects", "capabilities"},
		{"objects", "sources"},
		{"dependencies"},
	} {
		if arr := jsonArrayAt(t, groups[0], path...); len(arr) != 0 {
			t.Fatalf("组里不应有 %v：%v", path, arr)
		}
	}
	if got.doc.Params.Count != 1 {
		t.Fatalf("一组参数 count 应是 1, got %d", got.doc.Params.Count)
	}

	before := env.fingerprint(t, slug)
	for i := 0; i < 2; i++ {
		env.releaseDetail(t, slug, "empty")
	}
	if after := env.fingerprint(t, slug); after != before {
		t.Fatalf("详情是只读端点：库指纹 %s → %s", before, after)
	}
}

func (e *playbookTestEnv) fingerprint(t *testing.T, slug string) string {
	t.Helper()
	row, err := e.db.GetPlaybook(t.Context(), e.nsID, slug)
	if err != nil {
		t.Fatalf("get playbook %q: %v", slug, err)
	}
	return fmt.Sprintf("caps=%d srcs=%d installs=%d rv=%d",
		e.capCount(t), e.srcCount(t), e.installCount(t, slug), row.ResourceVersion)
}
