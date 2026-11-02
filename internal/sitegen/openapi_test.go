package sitegen

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "openapi.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return p
}

// TestLoadOpenAPIGroups 钉住分组的四条规则：
//  1. 分组顺序 = spec 顶层 tags 声明顺序（没端点的 tag 不出现）
//  2. spec 没声明却被用到的 tag 追加在后面（字典序）
//  3. deprecated 的 operation 不进页面
//  4. /../ 前缀的端点按「不在 /v1 下」处理（与 check_openapi.py 同一套约定）
func TestLoadOpenAPIGroups(t *testing.T) {
	spec := `openapi: 3.0.3
tags:
  - name: unused
  - name: beta
paths:
  /b:
    get:
      tags: [beta]
    post:
      tags: [beta]
      deprecated: true
  /a:
    post:
      tags: [beta]
    get:
      tags: [beta]
  /../agent-login.sh:
    get: {}
  /c:
    patch: {}
`
	groups, err := LoadOpenAPIGroups(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("LoadOpenAPIGroups: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups (beta, other), got %d: %+v", len(groups), groups)
	}
	if groups[0].Tag != "beta" || groups[1].Tag != "other" {
		t.Fatalf("group order: want [beta other], got [%s %s]", groups[0].Tag, groups[1].Tag)
	}

	// 同一路径多方法：读在前、写在后的固定顺序。/b 的 deprecated POST 必须已经被丢弃。
	beta := groups[0]
	want := []OpenAPIOperation{
		{Method: "GET", Class: "m-get", Path: "/v1/a"},
		{Method: "POST", Class: "m-post", Path: "/v1/a"},
		{Method: "GET", Class: "m-get", Path: "/v1/b"},
	}
	if len(beta.Ops) != len(want) || beta.Total != len(want) {
		t.Fatalf("beta ops: want %d, got %d (Total=%d) %+v", len(want), len(beta.Ops), beta.Total, beta.Ops)
	}
	for i, w := range want {
		if beta.Ops[i] != w {
			t.Errorf("beta op[%d]: want %+v, got %+v", i, w, beta.Ops[i])
		}
	}

	other := groups[1]
	if len(other.Ops) != 2 {
		t.Fatalf("other ops: want 2, got %d %+v", len(other.Ops), other.Ops)
	}
	if other.Ops[0].Path != "/agent-login.sh" || other.Ops[1].Path != "/v1/c" {
		t.Errorf("other paths: want [/agent-login.sh /v1/c], got [%s %s]", other.Ops[0].Path, other.Ops[1].Path)
	}
	if other.Ops[1].Class != "m-patch" {
		t.Errorf("PATCH badge class: want m-patch, got %s", other.Ops[1].Class)
	}
}

// TestLoadOpenAPIGroupsRepoSpec 直接解析**仓库里真实的** api/openapi.yaml：
// 这是页面与契约不漂移的护栏 —— 端点搬家/改名、spec 从 /v1 换前缀、
// 或者哪天把 deprecated 的旧端点复活，这里都会红。
func TestLoadOpenAPIGroupsRepoSpec(t *testing.T) {
	groups, err := LoadOpenAPIGroups(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("load repo spec: %v", err)
	}
	var total int
	found := map[string]bool{}
	for _, g := range groups {
		if len(g.Ops) != g.Total {
			t.Errorf("group %s: Total=%d but %d ops", g.Tag, g.Total, len(g.Ops))
		}
		for _, op := range g.Ops {
			total++
			found[op.Method+" "+op.Path] = true
		}
	}
	if total < 80 {
		t.Errorf("repo spec should expose >=80 operations, got %d", total)
	}
	for _, want := range []string{
		"POST /v1/events",
		"GET /v1/playbooks",
		"GET /agent-login.sh",
	} {
		if !found[want] {
			t.Errorf("missing operation %q (grouping/normalization broke)", want)
		}
	}
	// 旧「通知终端」端点全部 deprecated，不该出现在文档页上。
	for _, gone := range []string{"GET /v1/devices", "POST /v1/devices"} {
		if found[gone] {
			t.Errorf("deprecated operation %q leaked into docs", gone)
		}
	}
}

func TestLoadOpenAPIGroupsErrors(t *testing.T) {
	if _, err := LoadOpenAPIGroups(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file should error")
	}
	if _, err := LoadOpenAPIGroups(writeSpec(t, "openapi: 3.0.3\ntags: []\n")); err == nil {
		t.Error("spec without paths should error")
	}
}
