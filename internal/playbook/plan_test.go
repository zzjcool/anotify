package playbook

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// explicitReviewDoc 给 review 能力加一个显式 version（行 4/5/6 用）。
func explicitReviewDoc(t *testing.T, version int) string {
	t.Helper()
	marker := "  displayName: '{{values.project}} 审查'\n"
	if !strings.Contains(reviewDoc, marker) {
		t.Fatalf("reviewDoc 变了，测试夹具要跟着改")
	}
	return strings.Replace(reviewDoc, marker, fmt.Sprintf("  version: %d\n%s", version, marker), 1)
}

// renderedReview 渲染标准包并取出 a-review（以及它的 foldKey）。
func renderedReview(t *testing.T, pkg *Package) (*artifactOf, string) {
	t.Helper()
	out := mustRender(t, pkg, baseValues())
	a := findCap(out.Capabilities, "a-review")
	if a == nil {
		t.Fatalf("没渲染出 a-review: %v", capNames(out.Capabilities))
	}
	return a, foldOf(t, a)
}

// TestPlanMatrix 是 §4.2 冲突矩阵的**逐格**用例（12 行各一条）。
//
// 每格断言的落点：created / skipped / upgraded / unapplied / pending / external / notes / conflicts
// 与错误码——也就是回执与 400 的全部可见面。
func TestPlanMatrix(t *testing.T) {
	t.Run("row01_capability_without_version_and_no_same_name_inserts_v1", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		plan, err := Plan(pkg, baseValues(), newFakeExisting())
		if err != nil {
			t.Fatalf("空库安装不该失败: %v", err)
		}
		if got := plan.Created.Capabilities; !sameStrings(got, []string{"a-fixer@1", "a-review@1"}) {
			t.Fatalf("created.capabilities = %v，期望 [a-fixer@1 a-review@1]", got)
		}
		if len(plan.Skipped.Capabilities) != 0 || len(plan.Upgraded) != 0 {
			t.Fatalf("空库不该 skipped / upgraded: %+v %+v", plan.Skipped, plan.Upgraded)
		}
		for _, pc := range plan.Capabilities {
			if pc.Action != ActionCreate || pc.Artifact.Metadata.Version != 1 || pc.Digest == "" {
				t.Fatalf("能力执行指令不对: %+v", pc)
			}
		}
		if got := plan.Created.Sources; !sameStrings(got, []string{"a-on-push"}) {
			t.Fatalf("created.sources = %v", got)
		}
	})

	t.Run("row02_capability_without_version_and_same_foldkey_reuses_row", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		review, fold := renderedReview(t, pkg)
		ex := newFakeExisting().withCap("a-review", 3, fold, digestOf(t, review, 3), "a-fixer")
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("正文相同应复用: %v", err)
		}
		if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@3"}) {
			t.Fatalf("skipped.capabilities = %v，期望 [a-review@3]", got)
		}
		if got := plan.Created.Capabilities; !sameStrings(got, []string{"a-fixer@1"}) {
			t.Fatalf("created.capabilities = %v，期望只有 a-fixer@1", got)
		}
		if len(plan.Unapplied) != 0 || len(plan.Notes) != 0 {
			t.Fatalf("早装过了（第 2 行）不该写 note: %+v %+v", plan.Unapplied, plan.Notes)
		}
	})

	t.Run("row03_same_name_different_content_inserts_next_version_and_reports_upgrade", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		ex := newFakeExisting().
			withCap("a-review", 3, "sha256:older", "sha256:olderdigest", "a-fixer").
			withAffects("a-review", 5, 1)
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("正文变了就是新版本，不是冲突: %v", err)
		}
		if got := plan.Created.Capabilities; !sameStrings(got, []string{"a-fixer@1", "a-review@4"}) {
			t.Fatalf("created.capabilities = %v，期望 a-review@4", got)
		}
		if len(plan.Upgraded) != 1 {
			t.Fatalf("upgraded = %+v，期望 1 条", plan.Upgraded)
		}
		up := plan.Upgraded[0]
		if up.Capability != "a-review" || up.From != 3 || up.To != 4 {
			t.Fatalf("upgraded[0] = %+v，期望 a-review 3→4", up)
		}
		if up.Affects.Edges != 5 || up.Affects.Sources != 1 {
			t.Fatalf("affects 两个计数都要算: %+v", up.Affects)
		}
	})

	t.Run("row04_explicit_version_absent_inserts", func(t *testing.T) {
		b := newBundle().cap("capabilities/review.yaml", explicitReviewDoc(t, 2)).cap("capabilities/fixer.yaml", fixerDoc)
		pkg := b.pkg(t)
		plan, err := Plan(pkg, baseValues(), newFakeExisting())
		if err != nil {
			t.Fatalf("显式版本首次安装不该失败: %v", err)
		}
		if got := plan.Created.Capabilities; !sameStrings(got, []string{"a-fixer@1", "a-review@2"}) {
			t.Fatalf("created.capabilities = %v", got)
		}
		for _, pc := range plan.Capabilities {
			if pc.Artifact.Metadata.Name == "a-review" && pc.Artifact.Metadata.Version != 2 {
				t.Fatalf("显式版本没被保留: %+v", pc.Artifact.Metadata)
			}
		}
	})

	t.Run("row05_explicit_version_same_content_skips", func(t *testing.T) {
		b := newBundle().cap("capabilities/review.yaml", explicitReviewDoc(t, 2)).cap("capabilities/fixer.yaml", fixerDoc)
		pkg := b.pkg(t)
		review, fold := renderedReview(t, pkg)
		ex := newFakeExisting().withCap("a-review", 2, fold, digestOf(t, review, 2), "a-fixer")
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("显式同版同内容应复用: %v", err)
		}
		if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@2"}) {
			t.Fatalf("skipped.capabilities = %v", got)
		}
		if len(plan.Unapplied) != 0 {
			t.Fatalf("路由相同不该有 unapplied: %+v", plan.Unapplied)
		}
	})

	t.Run("row06_explicit_version_different_content_is_immutable_conflict", func(t *testing.T) {
		b := newBundle().cap("capabilities/review.yaml", explicitReviewDoc(t, 2)).cap("capabilities/fixer.yaml", fixerDoc)
		pkg := b.pkg(t)
		ex := newFakeExisting().withCap("a-review", 2, "sha256:other", "sha256:otherdigest", "a-fixer")
		plan, err := Plan(pkg, baseValues(), ex)
		assertCode(t, err, CodeVersionContentImmutable)
		if plan == nil {
			t.Fatalf("有冲突时也要返回计划（调用方要展示冲突全貌）")
		}
		if len(plan.Conflicts) != 1 {
			t.Fatalf("conflicts = %+v，期望 1 条", plan.Conflicts)
		}
		c := plan.Conflicts[0]
		if c.Kind != "capability" || c.Ref != "a-review@2" || c.Reason != ConflictVersionConflict {
			t.Fatalf("conflict = %+v", c)
		}
		if c.DeclaredDigest == "" || c.ActualDigest != "sha256:otherdigest" {
			t.Fatalf("冲突必须带 declaredDigest / actualDigest: %+v", c)
		}
		// 冲突条目本身不产出执行指令（整包 0 写由调用方裁决，plan 必须如实反映"这条不能建"）。
		for _, pc := range plan.Capabilities {
			if pc.Artifact.Metadata.Name == "a-review" {
				t.Fatalf("冲突的显式版本不该出现在执行指令里: %+v", pc)
			}
		}
		// 400 形状：details 带 conflicts / valuesDigest / hint。
		pe, ok := err.(*Error)
		if !ok {
			t.Fatalf("错误类型应是 *Error: %T", err)
		}
		for _, k := range []string{"conflicts", "valuesDigest", "hint"} {
			if _, ok := pe.Details[k]; !ok {
				t.Fatalf("400 details 缺 %q: %+v", k, pe.Details)
			}
		}
	})

	t.Run("row07_existing_row_with_different_next_is_unapplied", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		review, fold := renderedReview(t, pkg)
		ex := newFakeExisting().withCap("a-review", 3, fold, digestOf(t, review, 3), "other")
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("路由差异不该拒绝安装: %v", err)
		}
		if got := plan.Skipped.Capabilities; !sameStrings(got, []string{"a-review@3"}) {
			t.Fatalf("skipped.capabilities = %v", got)
		}
		if len(plan.Unapplied) != 1 {
			t.Fatalf("unapplied = %+v，期望 1 条", plan.Unapplied)
		}
		u := plan.Unapplied[0]
		if u.Kind != "capability" || u.Ref != "a-review@3" || u.Field != "next" || u.Reason != ReasonRoutingUnapplied {
			t.Fatalf("unapplied[0] = %+v", u)
		}
		if !sameStrings(asStrings(t, u.Declared), []string{"a-fixer"}) ||
			!sameStrings(asStrings(t, u.Actual), []string{"other"}) {
			t.Fatalf("unapplied 要给出 declared / actual: %+v", u)
		}
		if !hasNote(plan.Notes, NoteRoutingUnapplied, "a-review@3") {
			t.Fatalf("第 7 行必须有一条 routing_unapplied note: %+v", plan.Notes)
		}
		// 行 7 的直接后果：a-fixer 装上了但没人指向它（§4.5 的 unreachable 例子）。
		if !hasNote(plan.Notes, NoteUnreachable, "a-fixer@1") {
			t.Fatalf("路由未生效造成的孤儿能力必须报 unreachable: %+v", plan.Notes)
		}
	})

	t.Run("row08_new_source_inserts_and_marks_token_generation", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		plan, err := Plan(pkg, baseValues(), newFakeExisting())
		if err != nil {
			t.Fatalf("新源安装失败: %v", err)
		}
		if got := plan.Created.Sources; !sameStrings(got, []string{"a-on-push"}) {
			t.Fatalf("created.sources = %v", got)
		}
		var src *PlannedSource
		for i := range plan.Sources {
			if plan.Sources[i].Name == "a-on-push" {
				src = &plan.Sources[i]
			}
		}
		if src == nil || src.Action != ActionCreate {
			t.Fatalf("源执行指令不对: %+v", plan.Sources)
		}
		if !src.GenerateToken {
			t.Fatalf("token 省略的新建 webhook 源必须标记生成 token")
		}
		if len(src.Spec.Targets) != 1 {
			t.Fatalf("新建源要带上 targets（入口边只对新建源写）: %+v", src.Spec.Targets)
		}

		// 包里显式给了 token → 服务端不再生成（只有"省略"才生成）。
		explicit := `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: { token: 'src_explicit' }
`
		b2 := newBundle().source("sources/on-push.yaml", explicit)
		plan2, err := Plan(b2.pkg(t), baseValues(), newFakeExisting())
		if err != nil {
			t.Fatalf("显式 token 的源安装失败: %v", err)
		}
		if len(plan2.Sources) != 1 || plan2.Sources[0].GenerateToken {
			t.Fatalf("显式 token 不该被服务端覆盖: %+v", plan2.Sources)
		}
	})

	t.Run("row09_source_same_kind_config_reuses_without_writing_targets", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		rendered := mustRender(t, pkg, baseValues())
		pkgSrc := findSource(rendered.Sources, "a-on-push")
		// 库里存的是**归一化 + 已生成 token** 的形态：包里省略 token → 保留已有、不算冲突。
		ex := newFakeExisting().withSource("a-on-push", "webhook",
			`{"token":"src_existing","payloadSchema":{"type":"object","properties":{"repo":{"type":"string"}}}}`,
			pkgSrc.Spec.Targets...)
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("kind/config 归一化后等价应复用: %v", err)
		}
		if got := plan.Skipped.Sources; !sameStrings(got, []string{"a-on-push"}) {
			t.Fatalf("skipped.sources = %v", got)
		}
		if len(plan.Created.Sources) != 0 {
			t.Fatalf("复用的源不该进 created: %v", plan.Created.Sources)
		}
		if len(plan.Unapplied) != 0 || len(plan.Conflicts) != 0 {
			t.Fatalf("第 9 行不该有 unapplied / conflicts: %+v %+v", plan.Unapplied, plan.Conflicts)
		}
		for _, ps := range plan.Sources {
			if ps.Name == "a-on-push" && (ps.Action != ActionReuse || ps.GenerateToken) {
				t.Fatalf("复用源不该再动作: %+v", ps)
			}
		}
	})

	t.Run("row10_source_config_differs_is_playbook_conflict", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		rendered := mustRender(t, pkg, baseValues())
		pkgSrc := findSource(rendered.Sources, "a-on-push")
		ex := newFakeExisting().withSource("a-on-push", "webhook",
			`{"token":"src_existing","payloadSchema":{"type":"object","properties":{"sha":{"type":"string"}}}}`,
			pkgSrc.Spec.Targets...)
		plan, err := Plan(pkg, baseValues(), ex)
		assertCode(t, err, CodePlaybookConflict)
		if len(plan.Conflicts) != 1 {
			t.Fatalf("conflicts = %+v", plan.Conflicts)
		}
		c := plan.Conflicts[0]
		if c.Kind != "source" || c.Ref != "a-on-push" || c.Reason != ConflictConfigDiffers {
			t.Fatalf("conflict = %+v", c)
		}
		if !sameStrings(c.Fields, []string{"config.payloadSchema"}) {
			t.Fatalf("fields 要点名真正不同的字段: %v", c.Fields)
		}
	})

	t.Run("row11_source_target_missing_everywhere_is_pending", func(t *testing.T) {
		src := `
metadata:
  name: '{{values.project}}-on-push'
spec:
  kind: manual
  enabled: true
  config: {}
  targets:
    - capability: '{{values.project}}-linter'
`
		b := newBundle().cap("capabilities/fixer.yaml", fixerDoc).source("sources/on-push.yaml", src)
		pkg := b.pkg(t)
		plan, err := Plan(pkg, baseValues(), newFakeExisting())
		if err != nil {
			t.Fatalf("包外引用不该阻止安装: %v", err)
		}
		if got := plan.Pending; !sameStrings(got, []string{"a-linter"}) {
			t.Fatalf("pending = %v，期望 [a-linter]", got)
		}
		if !hasNote(plan.Notes, NotePendingRef, "a-linter") {
			t.Fatalf("pending 必须配一条 pending_ref note: %+v", plan.Notes)
		}
		if got := plan.Created.Sources; !sameStrings(got, []string{"a-on-push"}) {
			t.Fatalf("第 11 行照样建源: %v", got)
		}
	})

	t.Run("row12_source_same_config_but_different_targets_is_unapplied", func(t *testing.T) {
		pkg := newBundle().withStandard().pkg(t)
		rendered := mustRender(t, pkg, baseValues())
		pkgSrc := findSource(rendered.Sources, "a-on-push")
		cfg := string(pkgSrc.Spec.Config)
		ex := newFakeExisting().withSource("a-on-push", "webhook", cfg,
			sourcespec.Target{Capability: "a-review", Params: map[string]any{"repo": "different"}})
		plan, err := Plan(pkg, baseValues(), ex)
		if err != nil {
			t.Fatalf("targets 差异不该拒绝安装: %v", err)
		}
		if got := plan.Skipped.Sources; !sameStrings(got, []string{"a-on-push"}) {
			t.Fatalf("skipped.sources = %v", got)
		}
		if len(plan.Unapplied) != 1 {
			t.Fatalf("unapplied = %+v，期望 1 条", plan.Unapplied)
		}
		u := plan.Unapplied[0]
		if u.Kind != "source" || u.Ref != "a-on-push" || u.Field != "targets" || u.Reason != ReasonTargetsUnapplied {
			t.Fatalf("unapplied[0] = %+v", u)
		}
		if !hasNote(plan.Notes, NoteTargetsUnapplied, "a-on-push") {
			t.Fatalf("第 12 行必须有一条 targets_unapplied note: %+v", plan.Notes)
		}
		// 不改 targets：执行指令里这条源仍然是"复用"（store 不写入口边/投影）。
		for _, ps := range plan.Sources {
			if ps.Name == "a-on-push" && ps.Action != ActionReuse {
				t.Fatalf("复用的源不该被改成 create: %+v", ps)
			}
		}
	})
}

// TestPlanExplicitTokenDiffersIsMasked 验 §4.2 的 token 规则：显式 token 参与比较，
// 但差异报告**只点名字段**（脱敏），原始 token 一个字都不能出现在计划/错误里。
func TestPlanExplicitTokenDiffersIsMasked(t *testing.T) {
	// 两个 token 只在测试里拼出来：断言"原始值不出现"必须比真实值，不然是假绿灯。
	const (
		pkgTok = "src_" + "54b3cab"
		libTok = "src_" + "9c1f7ae"
	)
	src := fmt.Sprintf(`
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config:
    token: %q
  targets: []
`, pkgTok)
	b := newBundle().source("sources/on-push.yaml", src)
	pkg := b.pkg(t)
	ex := newFakeExisting().withSource("a-on-push", "webhook", fmt.Sprintf(`{"token":%q}`, libTok))
	plan, err := Plan(pkg, baseValues(), ex)
	assertCode(t, err, CodePlaybookConflict)
	if len(plan.Conflicts) != 1 || !sameStrings(plan.Conflicts[0].Fields, []string{"config.token"}) {
		t.Fatalf("显式 token 差异要点名 config.token: %+v", plan.Conflicts)
	}
	blob, merr := json.Marshal(struct {
		Plan *PlanResult `json:"plan"`
		Err  string      `json:"error"`
	}{plan, err.Error()})
	if merr != nil {
		t.Fatalf("序列化: %v", merr)
	}
	for _, secret := range []string{pkgTok, libTok} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("计划/错误里泄露了原始 token: %s", blob)
		}
	}
}

// TestPlanSecretOmittedIsRealDifference 验 §4.2 最后一条：只有服务端生成字段（token）走
// 保留规则，`secret` 的默认值就是"不校验签名"——包里不再给 secret 是真实差异，必须拒。
func TestPlanSecretOmittedIsRealDifference(t *testing.T) {
	src := `
metadata:
  name: a-on-push
spec:
  kind: webhook
  enabled: true
  config: { token: '' }
`
	b := newBundle().source("sources/on-push.yaml", src)
	pkg := b.pkg(t)
	ex := newFakeExisting().withSource("a-on-push", "webhook", `{"token":"src_x","secret":"s3cret"}`)
	plan, err := Plan(pkg, baseValues(), ex)
	assertCode(t, err, CodePlaybookConflict)
	if len(plan.Conflicts) != 1 || !sameStrings(plan.Conflicts[0].Fields, []string{"config.secret"}) {
		t.Fatalf("secret 省略是真实差异（默认 = 不校验），不能当等价: %+v", plan.Conflicts)
	}
}

// TestPlanExternalRefs 验 §3 的第二种落点：引用包外已存在的名字 → external（不建行、不算断链）。
func TestPlanExternalRefs(t *testing.T) {
	doc := `
apiVersion: anotify/v1
kind: Capability
metadata:
  name: tail
spec:
  next: [notify@2]
  run:
    entrypoint: run.sh
    files: {run.sh: "#!/usr/bin/env bash\ntrue\n"}
`
	b := newBundle().cap("capabilities/tail.yaml", doc)
	pkg := b.pkg(t)
	ex := newFakeExisting().withCap("notify", 2, "sha256:n", "sha256:nd", "x")
	plan, err := Plan(pkg, baseValues(), ex)
	if err != nil {
		t.Fatalf("包外已有引用不该失败: %v", err)
	}
	if got := plan.External; !sameStrings(got, []string{"notify@2"}) {
		t.Fatalf("external = %v，期望 [notify@2]", got)
	}
	if len(plan.Pending) != 0 {
		t.Fatalf("包外已有不该进 pending: %v", plan.Pending)
	}
}

// TestPlanUnreachableIgnoresLibraryUpstream 验 unreachable 自检考虑**库里已有**的上游。
func TestPlanUnreachableIgnoresLibraryUpstream(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	// 库里有别的能力指向 a-fixer → 它接得进链，不该报 unreachable。
	ex := newFakeExisting().withUpstream("a-fixer").withUpstream("a-review")
	plan, err := Plan(pkg, baseValues(), ex)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if hasNote(plan.Notes, NoteUnreachable, "") {
		t.Fatalf("库外上游存在时不该报 unreachable: %+v", plan.Notes)
	}
}

// TestPlanSourceDisabledNote 验 source_disabled note（只是提示，不阻止安装）。
func TestPlanSourceDisabledNote(t *testing.T) {
	src := `
metadata:
  name: '{{values.project}}-off'
spec:
  kind: manual
  enabled: false
  config: {}
  targets: []
`
	b := newBundle().source("sources/off.yaml", src)
	pkg := b.pkg(t)
	plan, err := Plan(pkg, baseValues(), newFakeExisting())
	if err != nil {
		t.Fatalf("停用的源照样装: %v", err)
	}
	if !hasNote(plan.Notes, NoteSourceDisabled, "a-off") {
		t.Fatalf("enabled: false 必须有 source_disabled note: %+v", plan.Notes)
	}
}

// TestPlanRequiresExisting 验 nil 快照不被当成空库（忘了传 = 报错，而不是"以为库是空的"）。
func TestPlanRequiresExisting(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	if _, err := Plan(pkg, baseValues(), nil); err == nil {
		t.Fatalf("Plan 必须要求库快照")
	}
}

// TestPlanDoesNotMutateInputs 验 Plan 是纯函数（不改 values / 不改包体）。
func TestPlanDoesNotMutateInputs(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	values := baseValues()
	filesBefore := len(pkg.Files)
	if _, err := Plan(pkg, values, newFakeExisting()); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if _, ok := values["env"]; ok {
		t.Fatalf("Plan 改了调用方 values")
	}
	if len(pkg.Files) != filesBefore {
		t.Fatalf("Plan 改了包体")
	}
}

// TestNoteCodesClosed 验 notes 的 code 是**封闭六码**（§4.5：新增一个码要改文档）。
func TestNoteCodesClosed(t *testing.T) {
	want := []string{
		"pending_ref", "no_node", "routing_unapplied",
		"source_disabled", "targets_unapplied", "unreachable",
	}
	if got := NoteCodes(); !sameStrings(got, want) {
		t.Fatalf("notes code 封闭集合 = %v，期望 %v", got, want)
	}
}

// ---------- 测试小工具 ----------

// artifactOf 是渲染后能力的别名（测试里少写一截类型名）。
type artifactOf = artifact.Artifact

// asStrings 把 unapplied 的 declared/actual 断言成 []string。
func asStrings(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]string)
	if !ok {
		t.Fatalf("期望 []string，实际 %T（%v）", v, v)
	}
	return list
}

// sameStrings 比较两个字符串集合（顺序无关）。
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a, b := append([]string{}, got...), append([]string{}, want...)
	sortStrings(a)
	sortStrings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sortStrings 是 sort.Strings 的薄封装（避免测试文件再多一个 import）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// hasNote 报告是否存在指定 code 的 note（ref 为空表示任意 ref）。
func hasNote(notes []Note, code, ref string) bool {
	for _, n := range notes {
		if n.Code != code {
			continue
		}
		if ref == "" || n.Ref == ref {
			return true
		}
	}
	return false
}

func TestPlanReservedSourceNameEmptyExistingCreateReuse(t *testing.T) {
	for _, template := range []string{"route", "{{values.name}}"} {
		for _, tc := range []struct {
			label    string
			existing Existing
		}{
			{"EmptyExisting", EmptyExisting()},
			{"create", newFakeExisting()},
			{"reuse", newFakeExisting().withSource("route", sourcespec.KindManual, "{}")},
		} {
			t.Run(template+"/"+tc.label, func(t *testing.T) {
				plan, err := Plan(sourceNameBundle(template).pkg(t), map[string]any{"name": "route"}, tc.existing)
				assertCode(t, err, CodeRenderNameInvalid)
				if !errors.Is(err, sourcespec.ErrReservedName) {
					t.Fatalf("plan lost shared error identity: %v", err)
				}
				if plan != nil {
					t.Fatal("reserved name reached create/reuse planning")
				}
			})
		}
	}
}
