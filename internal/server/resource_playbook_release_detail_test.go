package server

// #548：release 详情端点的传输面验收。
//
// 只断言 HTTP 形状与「对象不串」这条归属判据；折叠与排序语义由 internal/store 的
// playbook_release_detail_test.go 覆盖。变异自证见 docs/reports/release-detail-page.md。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type pbTestReleaseCapObject struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

type pbTestReleaseSourceObject struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type pbTestReleaseObjects struct {
	Capabilities []pbTestReleaseCapObject    `json:"capabilities"`
	Sources      []pbTestReleaseSourceObject `json:"sources"`
}

type pbTestReleaseParamGroup struct {
	ValuesDigest  string               `json:"valuesDigest"`
	Values        map[string]any       `json:"values"`
	Attempts      int                  `json:"attempts"`
	InstalledAt   string               `json:"installedAt"`
	LastInstallID string               `json:"lastInstallId"`
	Objects       pbTestReleaseObjects `json:"objects"`
	Dependencies  []struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	} `json:"dependencies"`
}

type pbTestReleaseDetail struct {
	Release       string `json:"release"`
	ValuesDigest  string `json:"valuesDigest"`
	InstalledAt   string `json:"installedAt"`
	LastInstallID string `json:"lastInstallId"`
	Params        struct {
		Digest string `json:"digest"`
		Groups int    `json:"groups"`
	} `json:"params"`
	Objects      pbTestReleaseObjects `json:"objects"`
	Dependencies []struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	} `json:"dependencies"`
	Groups []pbTestReleaseParamGroup `json:"groups"`
}

func (e *playbookTestEnv) releaseDetail(t *testing.T, slug, release string) pbTestReleaseDetail {
	t.Helper()
	rec := e.do(t, "GET", e.path("/"+slug+"/releases/"+release), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("release 详情应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestReleaseDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode release detail: %v (%s)", err, rec.Body.String())
	}
	return doc
}

// pbTestReleaseDetailCaps 取对象里的能力名（顺序即接口返回顺序）。
func pbTestReleaseDetailCaps(doc pbTestReleaseDetail) []string {
	out := make([]string, 0, len(doc.Objects.Capabilities))
	for _, cap := range doc.Objects.Capabilities {
		out = append(out, cap.Name)
	}
	return out
}

// TestPlaybookReleaseDetailObjectsStayPerRelease 两份 release 并存：各自详情只给自己的对象。
//
// 这是「详情页按 release 取对象」的判据本身：把归属换成「按 slug 查全部对象」时，
// blue 的详情里会多出 green 的能力，这个用例直接红。
func TestPlaybookReleaseDetailObjectsStayPerRelease(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pr-detail"
	env.create(t, slug, pbTestExportBundle(slug))

	if code, body := env.install(t, slug, "blue", map[string]any{"project": "blue"}); code != http.StatusOK {
		t.Fatalf("装 blue 应 200, got %d: %s", code, body)
	}
	if code, body := env.install(t, slug, "green", map[string]any{"project": "green"}); code != http.StatusOK {
		t.Fatalf("装 green 应 200, got %d: %s", code, body)
	}

	blue := env.releaseDetail(t, slug, "blue")
	if blue.Release != "blue" || blue.Params.Groups != 1 || len(blue.Groups) != 1 {
		t.Fatalf("blue 应是一份单组实例: %+v", blue)
	}
	got := pbTestReleaseDetailCaps(blue)
	want := []string{"blue-a-fixer", "blue-z-review"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("blue 的对象 = %v, want %v（只属于 blue 的能力，升序）", got, want)
	}
	if len(blue.Objects.Sources) != 1 || blue.Objects.Sources[0].Name != "blue-m-push" || blue.Objects.Sources[0].Kind != "webhook" {
		t.Fatalf("blue 的源 = %+v, want [blue-m-push webhook]", blue.Objects.Sources)
	}
	if blue.ValuesDigest == "" || blue.Params.Digest != blue.ValuesDigest {
		t.Fatalf("params.digest 应与顶层 valuesDigest 一致: %+v", blue.Params)
	}
	if blue.InstalledAt == "" || blue.LastInstallID == "" {
		t.Fatalf("身份字段不该为空: %+v", blue)
	}

	green := env.releaseDetail(t, slug, "green")
	gotGreen := pbTestReleaseDetailCaps(green)
	if len(gotGreen) != 2 || gotGreen[0] != "green-a-fixer" || gotGreen[1] != "green-z-review" {
		t.Fatalf("green 的对象 = %v, want [green-a-fixer green-z-review]（不串 blue）", gotGreen)
	}
	for _, cap := range green.Objects.Capabilities {
		if cap.Name == "" || cap.Version < 1 || cap.Digest == "" {
			t.Fatalf("对象应带 name/version/digest: %+v", cap)
		}
	}
}

// TestPlaybookReleaseDetailObjectsMatchSingleObjectEndpoints 对象的名字 + 版本 / kind 与单对象端点同源。
func TestPlaybookReleaseDetailObjectsMatchSingleObjectEndpoints(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pr-match"
	env.create(t, slug, pbTestExportBundle(slug))
	if code, body := env.install(t, slug, "default", map[string]any{"project": "match"}); code != http.StatusOK {
		t.Fatalf("装默认 release 应 200, got %d: %s", code, body)
	}

	doc := env.releaseDetail(t, slug, "default")
	if len(doc.Objects.Capabilities) != 2 || len(doc.Objects.Sources) != 1 {
		t.Fatalf("对象数不对: %+v", doc.Objects)
	}
	for _, cap := range doc.Objects.Capabilities {
		rec := env.do(t, "GET", "/v1/namespaces/"+env.nsID+"/capabilities/"+cap.Name, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET 能力 %s 应 200, got %d: %s", cap.Name, rec.Code, rec.Body.String())
		}
		var envelope pbTestEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode 能力信封: %v", err)
		}
		if envelope.Metadata.Version == nil || *envelope.Metadata.Version != cap.Version {
			t.Fatalf("能力 %s 版本 = %d, 单对象端点给 %v", cap.Name, cap.Version, envelope.Metadata.Version)
		}
	}
	for _, src := range doc.Objects.Sources {
		rec := env.do(t, "GET", "/v1/namespaces/"+env.nsID+"/sources/"+src.Name, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET 源 %s 应 200, got %d: %s", src.Name, rec.Code, rec.Body.String())
		}
		var envelope struct {
			Spec struct {
				Kind string `json:"kind"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode 源信封: %v", err)
		}
		if envelope.Spec.Kind != src.Kind {
			t.Fatalf("源 %s kind = %q, 单对象端点给 %q", src.Name, src.Kind, envelope.Spec.Kind)
		}
	}
}

// TestPlaybookReleaseDetailTwoValueGroups 同一 release 两套 values：参数 2 组，各组只列自己的对象。
func TestPlaybookReleaseDetailTwoValueGroups(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.pbTestExportMux(t)
	slug := "pr-groups"
	env.create(t, slug, pbTestExportBundle(slug))
	for _, project := range []string{"one", "two"} {
		if code, body := env.install(t, slug, "default", map[string]any{"project": project}); code != http.StatusOK {
			t.Fatalf("装 %s 应 200, got %d: %s", project, code, body)
		}
	}

	doc := env.releaseDetail(t, slug, "default")
	if doc.Params.Groups != 2 || len(doc.Groups) != 2 {
		t.Fatalf("两套 values 应是 2 组, got params=%+v groups=%d", doc.Params, len(doc.Groups))
	}
	seen := map[string]bool{}
	for _, group := range doc.Groups {
		if group.ValuesDigest == "" || group.Attempts != 1 {
			t.Fatalf("组形状不对: %+v", group)
		}
		if seen[group.ValuesDigest] {
			t.Fatalf("两组不该是同一个摘要: %+v", group.ValuesDigest)
		}
		seen[group.ValuesDigest] = true
		project, _ := group.Values["project"].(string)
		if project == "" {
			t.Fatalf("组里应带回打码后的 values: %+v", group.Values)
		}
		// 每组两个能力 + 一个源，且名字里带的是**这一组**的 project。
		if len(group.Objects.Capabilities) != 2 || len(group.Objects.Sources) != 1 {
			t.Fatalf("%s 组对象数不对: %+v", project, group.Objects)
		}
		for _, cap := range group.Objects.Capabilities {
			if cap.Name == "" || cap.Name[:len(project)] != project {
				t.Fatalf("%s 组里出现了别人的能力 %q", project, cap.Name)
			}
		}
	}
	// 顶层 = 最近装的 group（two）。
	if doc.ValuesDigest != doc.Groups[0].ValuesDigest {
		t.Fatalf("顶层应是第一组（最近安装）的摘要")
	}
}

// TestPlaybookReleaseDetailNotFound 不存在的 release 与已卸载的 release 回同一个 404。
func TestPlaybookReleaseDetailNotFound(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-404"
	env.create(t, slug, pbTestExportBundle(slug))
	if code, body := env.install(t, slug, "default", map[string]any{"project": "gone"}); code != http.StatusOK {
		t.Fatalf("装默认 release 应 200, got %d: %s", code, body)
	}

	rec := env.do(t, "GET", env.path("/"+slug+"/releases/never"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("从未存在的 release 应 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var doc pbTestStatusDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode 404 体: %v", err)
	}
	if doc.Reason != "release_not_found" {
		t.Fatalf("404 的 reason = %q, want release_not_found", doc.Reason)
	}

	if rec := env.do(t, "DELETE", env.path("/"+slug+"/releases/default"), nil); rec.Code != http.StatusOK {
		t.Fatalf("卸载应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(t, "GET", env.path("/"+slug+"/releases/default"), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("已卸载的 release 应 404（不是空 200）, got %d: %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc.Reason != "release_not_found" {
		t.Fatalf("已卸载的 reason = %q, want release_not_found", doc.Reason)
	}
}

// TestPlaybookReleaseYAMLByValuesDigest 按参数组导出：只出现这一组装出的对象。
func TestPlaybookReleaseYAMLByValuesDigest(t *testing.T) {
	env := newPlaybookTestEnv(t)
	env.create(t, "pr-group-yaml", pbTestExportBundle("pr-group-yaml"))
	slug := "pr-group-yaml"
	for _, project := range []string{"left", "right"} {
		if code, body := env.install(t, slug, "default", map[string]any{"project": project}); code != http.StatusOK {
			t.Fatalf("装 %s 应 200, got %d: %s", project, code, body)
		}
	}
	doc := env.releaseDetail(t, slug, "default")
	left := ""
	for _, group := range doc.Groups {
		if group.Values["project"] == "left" {
			left = group.ValuesDigest
		}
	}
	if left == "" {
		t.Fatalf("找不到 left 组的摘要: %+v", doc.Groups)
	}

	rec := env.do(t, "GET", env.path("/"+slug+"/releases/default/yaml?valuesDigest="+left), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("按组导出应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !containsAll(body, "left-a-fixer", "left-z-review", "left-m-push") {
		t.Fatalf("导出里缺少 left 组的对象:\n%s", body)
	}
	if containsAny(body, "right-a-fixer", "right-z-review", "right-m-push") {
		t.Fatalf("按组导出混进了另一组的对象:\n%s", body)
	}
	if header, docs := pbTestExportDocs(t, body); len(docs) != 3 || !containsAny(header, "release: default") {
		t.Fatalf("按组导出的头注释/文档数不对: %d\n%s", len(docs), body)
	}
}

// containsAll 断言 body 同时含全部子串（按 release 导出的分组判据）。
func containsAll(body string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(body, needle) {
			return false
		}
	}
	return true
}

// containsAny 断言 body 含任一子串（「不串组」的反向判据）。
func containsAny(body string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(body, needle) {
			return true
		}
	}
	return false
}
