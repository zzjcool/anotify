package server

// Playbook 的 **release 身份**传输面验收（#505）。
//
// 只断言 handler 层的三件事：release 从请求进、从回执/实例清单出；
// 形状错是 400；认领冲突是 409 且 details 指认 owner。
// 领域语义（模板展开、12 格矩阵里的认领判定）由 internal/playbook 的用例覆盖。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type pbTestReleaseReceipt struct {
	Kind    string `json:"kind"`
	Install struct {
		ID      string `json:"id"`
		DryRun  bool   `json:"dryRun"`
		Release string `json:"release"`
	} `json:"install"`
	Created struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
	} `json:"created"`
	Skipped struct {
		Capabilities []string `json:"capabilities"`
		Sources      []string `json:"sources"`
	} `json:"skipped"`
	ValuesDigest string `json:"valuesDigest"`
}

type pbTestReleaseGroup struct {
	Release      string   `json:"release"`
	ValuesDigest string   `json:"valuesDigest"`
	Attempts     int      `json:"attempts"`
	LastInstall  string   `json:"lastInstallId"`
	Capabilities []string `json:"capabilities"`
	Sources      []string `json:"sources"`
}

type pbTestReleaseList struct {
	Total    int                  `json:"total"`
	Installs []pbTestReleaseGroup `json:"installs"`
}

type pbTestReleaseConflictDoc struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Details struct {
		Name           string `json:"name"`
		Kind           string `json:"kind"`
		OwnerRelease   string `json:"ownerRelease"`
		OwnerInstallID string `json:"ownerInstallId"`
		Hint           string `json:"hint"`
		Conflicts      []struct {
			Name           string `json:"name"`
			Kind           string `json:"kind"`
			OwnerRelease   string `json:"ownerRelease"`
			OwnerInstallID string `json:"ownerInstallId"`
		} `json:"conflicts"`
	} `json:"details"`
}

// pbTestReleaseBundle 是「名字里带 release」的包：能力名与源名都含 `{{release.name}}`。
//
// 这是「一个预案并存多份实例」的物理前提：名字不同，库里才是两套行。
func pbTestReleaseBundle(slug string) map[string]string {
	return map[string]string{
		"playbook.yaml":            pbTestEntryDoc(slug, 0, []string{"capabilities/review.yaml"}, []string{"sources/on-push.yaml"}),
		"capabilities/review.yaml": pbTestCapabilityDoc("{{release.name}}-{{values.project}}-review", nil),
		"sources/on-push.yaml":     pbTestSourceDoc("{{release.name}}-{{values.project}}-on-push", []string{"{{release.name}}-{{values.project}}-review"}, ""),
	}
}

func (e *playbookTestEnv) install(t *testing.T, slug, release string, values map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"values": values}
	if release != "" {
		body["release"] = release
	}
	rec := e.do(t, "POST", e.path("/"+slug+"/install"), body)
	return rec.Code, rec.Body.Bytes()
}

func (e *playbookTestEnv) installs(t *testing.T, slug string) pbTestReleaseList {
	t.Helper()
	rec := e.do(t, "GET", e.path("/"+slug+"/installs"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("installs 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list pbTestReleaseList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode installs: %v (%s)", err, rec.Body.String())
	}
	return list
}

// TestPlaybookInstallReleaseDefaultsToSlug 缺省 release = 包名：老包路径逐字不变（回执多一个字段）。
func TestPlaybookInstallReleaseDefaultsToSlug(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-default"

	files := pbTestBundle(slug)
	env.create(t, slug, files)

	code, raw := env.install(t, slug, "", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install 应 200, got %d: %s", code, raw)
	}
	var rcpt pbTestReleaseReceipt
	if err := json.Unmarshal(raw, &rcpt); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, raw)
	}
	if rcpt.Install.Release != slug {
		t.Fatalf("缺省 release 应是包名 %q, got %q", slug, rcpt.Install.Release)
	}
	if len(rcpt.Created.Capabilities) != 1 || len(rcpt.Created.Sources) != 1 {
		t.Fatalf("首次安装的 created 不对: %+v", rcpt.Created)
	}

	// 再装一次（同一个缺省 release、同一组 values）→ 全部 skipped，与本版之前一致。
	code, raw = env.install(t, slug, "", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("第二次 install 应 200, got %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &rcpt); err != nil {
		t.Fatalf("decode second receipt: %v", err)
	}
	if len(rcpt.Skipped.Capabilities) != 1 || len(rcpt.Skipped.Sources) != 1 {
		t.Fatalf("第二次安装应全 skipped（幂等）: %+v", rcpt.Skipped)
	}
	if len(rcpt.Created.Capabilities) != 0 || len(rcpt.Created.Sources) != 0 {
		t.Fatalf("第二次安装不该新建任何东西: %+v", rcpt.Created)
	}

	list := env.installs(t, slug)
	if list.Total != 1 || len(list.Installs) != 1 {
		t.Fatalf("同一 release + 同一 values 应折叠成 1 组, got %+v", list)
	}
	if list.Installs[0].Release != slug || list.Installs[0].Attempts != 2 {
		t.Fatalf("组的 release/attempts 不对: %+v", list.Installs[0])
	}
}

// TestPlaybookMultiReleaseCoexists 验「一个预案并存多份实例」：
// 名字里带 `{{release.name}}` → 两个 release 各一套能力/源，实例清单两组各带 release。
func TestPlaybookMultiReleaseCoexists(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-multi"
	env.create(t, slug, pbTestReleaseBundle(slug))

	code, raw := env.install(t, slug, "blue", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install blue 应 200, got %d: %s", code, raw)
	}
	code, raw = env.install(t, slug, "green", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install green 应 200, got %d: %s", code, raw)
	}

	caps, err := env.db.ListCapabilities(t.Context(), env.nsID)
	if err != nil {
		t.Fatalf("list capabilities: %v", err)
	}
	var names []string
	for _, c := range caps {
		names = append(names, c.Name)
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"blue-demo-review", "green-demo-review"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺 %q（实际的 %v）", want, names)
		}
	}
	srcs, err := env.db.ListSources(t.Context(), env.nsID)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("两个 release 应各有一个源, got %d", len(srcs))
	}

	list := env.installs(t, slug)
	if list.Total != 2 || len(list.Installs) != 2 {
		t.Fatalf("应有 2 个折叠组（一个 release 一组）, got %+v", list)
	}
	byRelease := map[string]pbTestReleaseGroup{}
	for _, g := range list.Installs {
		byRelease[g.Release] = g
	}
	for _, rel := range []string{"blue", "green"} {
		g, ok := byRelease[rel]
		if !ok {
			t.Fatalf("缺 release %q 的组: %+v", rel, list.Installs)
		}
		if g.Attempts != 1 || g.LastInstall == "" {
			t.Fatalf("release %q 的组不对: %+v", rel, g)
		}
		if len(g.Capabilities) != 1 || !strings.HasPrefix(g.Capabilities[0], rel+"-demo-review@") {
			t.Fatalf("release %q 的组成员不对: %+v", rel, g.Capabilities)
		}
		if len(g.Sources) != 1 || g.Sources[0] != rel+"-demo-on-push" {
			t.Fatalf("release %q 的组源不对: %+v", rel, g.Sources)
		}
	}
}

// TestPlaybookInstallInvalidReleaseIs400 形状错的 release 是 400（且 0 写）。
func TestPlaybookInstallInvalidReleaseIs400(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-badrel"
	env.create(t, slug, pbTestBundle(slug))

	for _, bad := range []string{"Prod", "-lead", "a b", strings.Repeat("x", 64)} {
		code, raw := env.install(t, slug, bad, pbTestValues("demo"))
		if code != http.StatusBadRequest {
			t.Fatalf("release %q 应 400, got %d: %s", bad, code, raw)
		}
		var doc pbTestStatusDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		if doc.Reason != "invalid_playbook" {
			t.Fatalf("reason 应是 invalid_playbook, got %q (%s)", doc.Reason, raw)
		}
	}
	// 400 不写任何行：能力 / 源 / 回执都没有。
	if n := env.capCount(t); n != 0 {
		t.Fatalf("坏 release 不该建能力, 有 %d 行", n)
	}
	if n := env.srcCount(t); n != 0 {
		t.Fatalf("坏 release 不该建源, 有 %d 行", n)
	}
	rec := env.do(t, "GET", env.path("/"+slug+"/installs"), nil)
	var list pbTestReleaseList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode installs: %v", err)
	}
	if list.Total != 0 {
		t.Fatalf("坏 release 不该留回执, got %+v", list)
	}
}

// TestPlaybookCrossReleaseReuseIs409 跨 release 复用别人的行 → 409 + details 指认 owner + 0 写。
//
// 这份包的**名字是固定的**（不含 `{{release.name}}`）：release b 装同一份包时， 名字撞上 release a 装出来的行——这正是「隔离边界」要拦住的情形。
func TestPlaybookCrossReleaseReuseIs409(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-claim"
	env.create(t, slug, pbTestBundle(slug))

	code, raw := env.install(t, slug, "blue", pbTestValues("demo"))
	if code != http.StatusOK {
		t.Fatalf("install blue 应 200, got %d: %s", code, raw)
	}
	var first pbTestReleaseReceipt
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatalf("decode first receipt: %v", err)
	}
	capsBefore, srcsBefore := env.capCount(t), env.srcCount(t)

	code, raw = env.install(t, slug, "green", pbTestValues("demo"))
	if code != http.StatusConflict {
		t.Fatalf("跨 release 复用应 409, got %d: %s", code, raw)
	}
	var doc pbTestReleaseConflictDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode conflict: %v (%s)", err, raw)
	}
	if doc.Code != http.StatusConflict || doc.Reason != "release_conflict" {
		t.Fatalf("409 的形状不对: %+v", doc)
	}
	if doc.Details.Name == "" || doc.Details.Kind == "" {
		t.Fatalf("details 要点名被认领的对象: %+v", doc.Details)
	}
	if doc.Details.OwnerRelease != "blue" {
		t.Fatalf("details.ownerRelease 应是 blue, got %q", doc.Details.OwnerRelease)
	}
	if doc.Details.OwnerInstallID != first.Install.ID {
		t.Fatalf("details.ownerInstallId 应指向第一次安装的回执 %q, got %q",
			first.Install.ID, doc.Details.OwnerInstallID)
	}
	if !strings.Contains(doc.Details.Hint, "release") {
		t.Fatalf("hint 要给出出路: %q", doc.Details.Hint)
	}
	if len(doc.Details.Conflicts) == 0 || doc.Details.Conflicts[0].OwnerRelease != "blue" {
		t.Fatalf("details.conflicts 要列出逐条认领冲突: %+v", doc.Details.Conflicts)
	}

	if n := env.capCount(t); n != capsBefore {
		t.Fatalf("409 不该多建能力: %d → %d", capsBefore, n)
	}
	if n := env.srcCount(t); n != srcsBefore {
		t.Fatalf("409 不该多建源: %d → %d", srcsBefore, n)
	}
	list := env.installs(t, slug)
	if list.Total != 1 {
		t.Fatalf("409 不该留回执: %+v", list)
	}
	if list.Installs[0].Release != "blue" {
		t.Fatalf("剩下的组应还是 blue 的: %+v", list.Installs[0])
	}
}

// TestPlaybookRenderUsesRelease render 也要吃 release：预览与安装必须同一套名字。
func TestPlaybookRenderUsesRelease(t *testing.T) {
	env := newPlaybookTestEnv(t)
	slug := "pr-render-rel"
	env.create(t, slug, pbTestReleaseBundle(slug))

	rec := env.do(t, "POST", pathRender(env, slug), map[string]any{
		"release": "blue", "values": pbTestValues("demo"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("render 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode render: %v", err)
	}
	body := out.Files["capabilities/review.yaml"]
	if !strings.Contains(body, "blue-demo-review") {
		t.Fatalf("渲染结果里没有 release 前缀（release 没进模板）:\n%s", body)
	}

	rec = env.do(t, "POST", pathRender(env, slug), map[string]any{"values": pbTestValues("demo")})
	if rec.Code != http.StatusOK {
		t.Fatalf("默认 render 应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode default render: %v", err)
	}
	if !strings.Contains(out.Files["capabilities/review.yaml"], slug+"-demo-review") {
		t.Fatalf("缺省 release 应是包名:\n%s", out.Files["capabilities/review.yaml"])
	}
}

// pathRender 是 render 端点路径（install 用 env.path，render 这里要单独拼）。
func pathRender(e *playbookTestEnv, slug string) string { return e.path("/" + slug + "/render") }
