package playbook

import (
	"strings"
	"testing"
)

// release 作为安装级身份（#505）的领域验收：模板 / 缺省=包名 / 隔离边界各一组。

// releaseBundle：名字里带 release 的两能力 + 一源。
func releaseBundle() *bundle {
	capDoc := func(name, next string) string {
		return `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: '` + name + `'
spec:
  params:
    type: object
    properties: { repo: { type: string } }
  next: [` + next + `]
  run:
    entrypoint: run.sh
    files:
      run.sh: |
        #!/usr/bin/env bash
        echo "release={{release.name}} payload={{payload.x}} vars=$(vars.REGION) env=${ENV} project={{values.project}}"
`
	}
	b := newBundle()
	b.caps["capabilities/review.yaml"] = capDoc("{{release.name}}-{{values.project}}-review", "'{{release.name}}-{{values.project}}-fixer'")
	b.caps["capabilities/fixer.yaml"] = capDoc("{{release.name}}-{{values.project}}-fixer", "")
	b.srcs["sources/on-push.yaml"] = `
metadata:
  name: '{{release.name}}-{{values.project}}-on-push'
spec:
  kind: webhook
  enabled: true
  config: { token: '' }
  targets:
    - capability: '{{release.name}}-{{values.project}}-review'
      params: { repo: '{{values.repo}}' }
`
	return b
}

// TestNormalizeRelease 钉住 release 的默认与形状（缺省 = 包名；显式必须过正则）。
func TestNormalizeRelease(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		slug     string
		want     string
		wantErr  bool
	}{
		{name: "缺省 = 包名", explicit: "", slug: "project-chain", want: "project-chain"},
		{name: "显式合法", explicit: "staging-2", slug: "project-chain", want: "staging-2"},
		{name: "点与下划线合法", explicit: "a.b_c-1", slug: "p", want: "a.b_c-1"},
		{name: "63 字符", explicit: strings.Repeat("a", 63), slug: "p", want: strings.Repeat("a", 63)},
		// 64 字符包名（上限 64）作默认值必须照用，否则老包会因 release 正则（上限 63）突然装不上。
		{name: "64 字符包名作默认值", explicit: "", slug: strings.Repeat("b", 64), want: strings.Repeat("b", 64)},
		{name: "64 字符显式值不合法", explicit: strings.Repeat("c", 64), slug: "p", wantErr: true},
		{name: "大写不合法", explicit: "Prod", slug: "p", wantErr: true},
		{name: "首字符不能是点", explicit: ".prod", slug: "p", wantErr: true},
		{name: "首字符不能是横线", explicit: "-prod", slug: "p", wantErr: true},
		{name: "带空白不合法（不 trim）", explicit: " prod", slug: "p", wantErr: true},
		{name: "空包名且无显式值", explicit: "", slug: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRelease(tc.explicit, tc.slug)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望拒绝，得到 %q", got)
				}
				assertCode(t, err, CodeInvalidPlaybook)
				return
			}
			if err != nil {
				t.Fatalf("不该失败: %v", err)
			}
			if got != tc.want {
				t.Fatalf("release = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestRenderReleaseName 验 `{{release.name}}` 的三种用法与老模板的透传回归。
func TestRenderReleaseName(t *testing.T) {
	pkg := releaseBundle().pkg(t)
	out, err := RenderForRelease(pkg, baseValues(), "blue")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	names := capNames(out.Capabilities)
	for _, want := range []string{"blue-a-review", "blue-a-fixer"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("缺 %q（实际的 %v）：{{release.name}} 没进名字", want, names)
		}
	}
	if len(out.Sources) != 1 || out.Sources[0].Name != "blue-a-on-push" {
		t.Fatalf("源的 release 名字不对: %+v", out.Sources)
	}
	// 路由与 targets 里的引用也要真的换掉。
	review := findCap(out.Capabilities, "blue-a-review")
	if review == nil {
		t.Fatalf("找不到 blue-a-review: %v", names)
	}
	if len(review.Spec.Next) != 1 || review.Spec.Next[0] != "blue-a-fixer" {
		t.Fatalf("next 没渲染 release: %v", review.Spec.Next)
	}
	if len(out.Sources[0].Spec.Targets) != 1 || out.Sources[0].Spec.Targets[0].Capability != "blue-a-review" {
		t.Fatalf("targets 没渲染 release: %+v", out.Sources[0].Spec.Targets)
	}

	// 换一个 release = 另一套名字（并存的物理前提）。
	other, err := RenderForRelease(pkg, baseValues(), "green")
	if err != nil {
		t.Fatalf("换 release 渲染失败: %v", err)
	}
	if names := capNames(other.Capabilities); names[0] == capNames(out.Capabilities)[0] {
		t.Fatalf("两个 release 渲染出同一套名字: %v", names)
	}

	// 同一份文档里 `{{payload.x}}` / `$(vars.X)` / `${…}` 仍原样（§1.4 定界符隔离）。
	script := review.Spec.Run.Files["run.sh"]
	for _, want := range []string{"release=blue", "{{payload.x}}", "$(vars.REGION)", "${ENV}", "project=a"} {
		if !strings.Contains(script, want) {
			t.Fatalf("脚本里缺 %q（透传被弄坏了）:\n%s", want, script)
		}
	}
}

// TestRenderReleaseWholeValueKeepsString 验"整个值恰好一个引用 → 保留原类型"在 release 上也成立。
func TestRenderReleaseWholeValueKeepsString(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: only-release
  displayName: '{{release.name}}'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/x.yaml", doc)
	out, err := RenderForRelease(b.pkg(t), baseValues(), "prod")
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	a := findCap(out.Capabilities, "only-release")
	if a == nil {
		t.Fatalf("没渲染出 only-release")
	}
	if a.Metadata.DisplayName != "prod" {
		t.Fatalf("displayName = %q，期望 prod（整值引用不该被包成别的形状）", a.Metadata.DisplayName)
	}
}

// TestRenderReleaseUnknownFieldIs400：`{{release.foo}}` 是 400（带字段路径），不是字面量——
// 静默当字面量会让作者以为名字按 release 分开了，直到装第二份才在库里撞车。
func TestRenderReleaseUnknownFieldIs400(t *testing.T) {
	for _, bad := range []string{"{{release.foo}}", "{{release.namespace}}", "{{release.name }}"} {
		doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: 'x-` + bad + `'
spec:
  next: []
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
		t.Run(bad, func(t *testing.T) {
			b := newBundle().cap("capabilities/x.yaml", doc)
			_, err := RenderForRelease(b.pkg(t), baseValues(), "prod")
			if err == nil {
				t.Fatalf("%s 应该 400", bad)
			}
			assertCode(t, err, CodeInvalidPlaybook)
			pe, ok := err.(*Error)
			if !ok {
				t.Fatalf("应是领域错误: %T", err)
			}
			// 字段路径必须给出。
			if got, _ := pe.Details["field"].(string); !strings.Contains(got, "capabilities/x.yaml") || !strings.Contains(got, "metadata.name") {
				t.Fatalf("details.field = %q，期望点名文件与字段路径", got)
			}
			if got, _ := pe.Details["reference"].(string); got != bad {
				t.Fatalf("details.reference = %q，期望 %q", got, bad)
			}
			if !strings.Contains(pe.Message, bad) {
				t.Fatalf("消息要点名引用: %v", pe)
			}
		})
	}
}

// TestRenderReleaseDefaultIsSlug 验缺省 release = 包名：老包（无 release 模板）逐字不变。
func TestRenderReleaseDefaultIsSlug(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	def, err := Render(pkg, baseValues())
	if err != nil {
		t.Fatalf("默认渲染失败: %v", err)
	}
	explicit, err := RenderForRelease(pkg, baseValues(), pkg.Name)
	if err != nil {
		t.Fatalf("显式 = 包名渲染失败: %v", err)
	}
	if got, want := strings.Join(capNames(explicit.Capabilities), ","), strings.Join(capNames(def.Capabilities), ","); got != want {
		t.Fatalf("默认 release ≠ 包名: %q vs %q", got, want)
	}
	for i := range def.Sources {
		if def.Sources[i].Name != explicit.Sources[i].Name {
			t.Fatalf("默认 release 下源名不一致: %q vs %q", def.Sources[i].Name, explicit.Sources[i].Name)
		}
	}
}

// ---------- 隔离边界：release_conflict ----------

// TestPlanReleaseConflictOnCapabilityReuse：行 2 跨 release → 409。
func TestPlanReleaseConflictOnCapabilityReuse(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	review, fold := renderedReview(t, pkg)
	// 这一行是 release blue 的 ins_blue 装的。
	ex := newFakeExisting().
		withCap("a-review", 1, fold, digestOf(t, review, 1)).
		withCapOwner("a-review", "blue", "ins_blue")

	plan, err := PlanForRelease(pkg, baseValues(), "green", ex)
	if err == nil {
		t.Fatalf("跨 release 复用必须是错误")
	}
	assertCode(t, err, CodeReleaseConflict)
	if len(plan.ReleaseConflicts) != 1 {
		t.Fatalf("release 冲突条数 = %d，期望 1", len(plan.ReleaseConflicts))
	}
	rc := plan.ReleaseConflicts[0]
	if rc.Name != "a-review" || rc.Kind != "capability" || rc.OwnerRelease != "blue" || rc.OwnerInstallID != "ins_blue" {
		t.Fatalf("冲突条目 = %+v", rc)
	}
	// 整包 0 写：这条不能既冲突又建出来。
	for _, pc := range plan.Capabilities {
		if pc.Artifact.Metadata.Name == "a-review" {
			t.Fatalf("冲突的行不该进执行指令: %+v", pc)
		}
	}
	for _, ref := range plan.Skipped.Capabilities {
		if refName(ref) == "a-review" {
			t.Fatalf("冲突的行不该算 skipped: %v", plan.Skipped.Capabilities)
		}
	}
	// 409 的 details 必须能指认 owner。
	pe := err.(*Error)
	for key, want := range map[string]any{
		"name": "a-review", "kind": "capability",
		"ownerRelease": "blue", "ownerInstallId": "ins_blue",
	} {
		if got := pe.Details[key]; got != want {
			t.Fatalf("details.%s = %v，期望 %v", key, got, want)
		}
	}
	if hint, _ := pe.Details["hint"].(string); !strings.Contains(hint, "release") {
		t.Fatalf("details.hint 要给出出路: %q", hint)
	}
}

// TestPlanReleaseConflictOnSourceReuse：行 9 跨 release → 409。快照的 kind/config 与渲染
// 结果完全相同，唯一差异是认领（否则会先撞上行 10 的 config_differs，用例就证明不了什么）。
func TestPlanReleaseConflictOnSourceReuse(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	rendered := mustRender(t, pkg, baseValues())
	src := findSource(rendered.Sources, "a-on-push")
	if src == nil {
		t.Fatalf("夹具里没有 a-on-push")
	}
	ex := newFakeExisting().
		withCap("a-review", 1, "sha256:fold", "sha256:digest", "a-fixer").
		withCap("a-fixer", 1, "sha256:fold2", "sha256:digest2").
		withSource("a-on-push", src.Spec.Kind, string(src.Spec.Config), src.Spec.Targets...).
		withSourceOwner("a-on-push", "blue", "ins_blue")

	plan, err := PlanForRelease(pkg, baseValues(), "green", ex)
	if err == nil {
		t.Fatalf("跨 release 复用源必须是错误")
	}
	assertCode(t, err, CodeReleaseConflict)
	if len(plan.ReleaseConflicts) != 1 {
		t.Fatalf("release 冲突条数 = %d，期望 1（%+v）", len(plan.ReleaseConflicts), plan.ReleaseConflicts)
	}
	rc := plan.ReleaseConflicts[0]
	if rc.Kind != "source" || rc.Name != "a-on-push" || rc.OwnerRelease != "blue" {
		t.Fatalf("冲突条目 = %+v", rc)
	}
	for _, s := range plan.Skipped.Sources {
		if s == "a-on-push" {
			t.Fatalf("冲突的源不该算 skipped: %v", plan.Skipped.Sources)
		}
	}
}

// TestPlanSameReleaseReusesOwnRows：同 release 内同名同内容 → reuse（行为不变）。
func TestPlanSameReleaseReusesOwnRows(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	review, fold := renderedReview(t, pkg)
	ex := newFakeExisting().
		withCap("a-review", 1, fold, digestOf(t, review, 1), "a-fixer").
		withCapOwner("a-review", "blue", "ins_blue")

	plan, err := PlanForRelease(pkg, baseValues(), "blue", ex)
	if err != nil {
		t.Fatalf("同一个 release 复用自己装的行不该失败: %v", err)
	}
	if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@1"}) {
		t.Fatalf("skipped.capabilities = %v，期望 [a-review@1]", got)
	}
	if len(plan.ReleaseConflicts) != 0 {
		t.Fatalf("不该有 release 冲突: %+v", plan.ReleaseConflicts)
	}
}

// TestPlanUnownedRowReuses：无主行（手建，无 installed_from）仍可复用。
func TestPlanUnownedRowReuses(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	review, fold := renderedReview(t, pkg)
	ex := newFakeExisting().withCap("a-review", 2, fold, digestOf(t, review, 2), "a-fixer")

	plan, err := PlanForRelease(pkg, baseValues(), "green", ex)
	if err != nil {
		t.Fatalf("无主行应可复用: %v", err)
	}
	if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@2"}) {
		t.Fatalf("skipped.capabilities = %v，期望 [a-review@2]", got)
	}
}

// TestPlanDefaultReleaseMatchesLegacyRows：历史回执回填的 release 也是包名 ⇒ 老包不回归。
func TestPlanDefaultReleaseMatchesLegacyRows(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	review, fold := renderedReview(t, pkg)
	ex := newFakeExisting().
		withCap("a-review", 1, fold, digestOf(t, review, 1), "a-fixer").
		withCapOwner("a-review", pkg.Name, "ins_legacy")

	plan, err := Plan(pkg, baseValues(), ex)
	if err != nil {
		t.Fatalf("默认 release 下不该冲突: %v", err)
	}
	if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@1"}) {
		t.Fatalf("skipped.capabilities = %v", got)
	}
	if plan.Release != pkg.Name {
		t.Fatalf("plan.Release = %q，期望包名 %q", plan.Release, pkg.Name)
	}
}

// TestPlanReleaseConflictBeatsContentConflict：同时制造行 6 内容冲突与源认领冲突， 认领优先——否则用户改完内容再装还是同一个 409。
func TestPlanReleaseConflictBeatsContentConflict(t *testing.T) {
	b := newBundle().
		cap("capabilities/review.yaml", explicitReviewDoc(t, 1)).
		cap("capabilities/fixer.yaml", fixerDoc).
		source("sources/on-push.yaml", onPushDoc)
	pkg := b.pkg(t)
	rendered := mustRender(t, pkg, baseValues())
	src := findSource(rendered.Sources, "a-on-push")
	ex := newFakeExisting().
		withCap("a-review", 1, "sha256:other", "sha256:otherdigest").
		withSource("a-on-push", src.Spec.Kind, string(src.Spec.Config), src.Spec.Targets...).
		withSourceOwner("a-on-push", "blue", "ins_blue")
	plan, err := PlanForRelease(pkg, baseValues(), "green", ex)
	if err == nil {
		t.Fatalf("应报冲突")
	}
	assertCode(t, err, CodeReleaseConflict)
	if len(plan.ReleaseConflicts) == 0 {
		t.Fatalf("release 冲突优先，但 plan 里没有: %+v", plan.Conflicts)
	}
	if len(plan.Conflicts) == 0 {
		t.Fatalf("夹具应同时制造一条内容冲突（否则这条用例证明不了优先级）")
	}
}
