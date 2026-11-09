package playbook

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildReceiptShape 验 §4.5 的回执形状：created/skipped 是**对象**、空列表是 `[]`、
// 没有 `conflicts` 字段、valuesDigest 对打码前的值算。
func TestBuildReceiptShape(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	plan, err := Plan(pkg, baseValues(), newFakeExisting())
	if err != nil {
		t.Fatalf("安装计划失败: %v", err)
	}
	r := BuildReceipt(plan, plan.Values, plan.ValuesDigest, InstallMeta{
		ID: "ins_7f3a", At: ts, DryRun: true, Namespace: "prod", Source: "oci://ghcr.io/acme/playbooks/project-chain@sha256:4c11",
	})
	if r.APIVersion != APIVersion || r.Kind != KindInstallReceipt {
		t.Fatalf("回执头 = %s/%s", r.APIVersion, r.Kind)
	}
	if r.Playbook.Name != "project-chain" || r.Playbook.Version != 3 {
		t.Fatalf("回执里的包身份 = %+v", r.Playbook)
	}
	if r.Install.ID != "ins_7f3a" || r.Install.At != ts || !r.Install.DryRun {
		t.Fatalf("install 段 = %+v", r.Install)
	}
	if r.Namespace != "prod" || !strings.HasPrefix(r.Source, "oci://") {
		t.Fatalf("namespace / source = %q / %q", r.Namespace, r.Source)
	}
	if len(r.Created.Capabilities) != 2 || len(r.Created.Sources) != 1 {
		t.Fatalf("created = %+v", r.Created)
	}
	if r.Skipped.Capabilities == nil || r.Skipped.Sources == nil {
		t.Fatalf("skipped 必须是 {} 对象形状（不是 null）: %+v", r.Skipped)
	}
	if len(r.Skipped.Capabilities) != 0 || len(r.Skipped.Sources) != 0 {
		t.Fatalf("skipped 应该空: %+v", r.Skipped)
	}

	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("回执序列化: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("回执 JSON 解析: %v", err)
	}
	if _, ok := decoded["conflicts"]; ok {
		t.Fatalf("回执不该有 conflicts 字段（冲突只在 400 错误体里）: %s", raw)
	}
	skipped, ok := decoded["skipped"].(map[string]any)
	if !ok {
		t.Fatalf("skipped 必须是对象: %s", raw)
	}
	for _, key := range []string{"capabilities", "sources"} {
		list, ok := skipped[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("skipped.%s 必须是 []（空列表不是 null）: %s", key, raw)
		}
	}
	for _, key := range []string{"upgraded", "unapplied", "external", "pending", "notes"} {
		if list, ok := decoded[key].([]any); !ok || len(list) != 0 {
			t.Fatalf("%s 必须是 []: %s", key, raw)
		}
	}
	if decoded["valuesDigest"] != plan.ValuesDigest {
		t.Fatalf("valuesDigest 不对: %v vs %v", decoded["valuesDigest"], plan.ValuesDigest)
	}
	if decoded["install"].(map[string]any)["dryRun"] != true {
		t.Fatalf("dryRun 标记丢了: %s", raw)
	}
}

// TestBuildReceiptMasksWriteOnly 验 D16：writeOnly 字段进回执是 `***`，digest 仍对打码前的值算。
func TestBuildReceiptMasksWriteOnly(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	values := map[string]any{"project": "a", "slackToken": "xoxb-secret"}
	plan, err := Plan(pkg, values, newFakeExisting())
	if err != nil {
		t.Fatalf("安装计划失败: %v", err)
	}
	r := BuildReceipt(plan, plan.Values, plan.ValuesDigest, InstallMeta{ID: "ins_1", At: ts, Namespace: "ns"})
	if r.Values["slackToken"] != MaskedValue {
		t.Fatalf("writeOnly 字段没打码: %#v", r.Values["slackToken"])
	}
	if r.Values["project"] != "a" {
		t.Fatalf("普通字段被打码: %#v", r.Values["project"])
	}
	// digest 对打码前的值算：等于直接对 plan.Values（含明文 secret）算的结果。
	want, err := ValuesDigest(plan.Values)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if r.ValuesDigest != want {
		t.Fatalf("valuesDigest 应对打码前的值算: %s vs %s", r.ValuesDigest, want)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "xoxb-secret") {
		t.Fatalf("回执里泄露了明文 secret: %s", raw)
	}
}

// TestBuildReceiptPlacement 验 unapplied / upgraded / notes 在回执里的形状（§4.5 的例子逐字段）。
func TestBuildReceiptPlacement(t *testing.T) {
	pkg := newBundle().withStandard().pkg(t)
	review, fold := renderedReview(t, pkg)
	ex := newFakeExisting().
		withCap("a-review", 3, fold, digestOf(t, review, 3), "other").
		withAffects("a-review", 2, 1)
	plan, err := Plan(pkg, baseValues(), ex)
	if err != nil {
		t.Fatalf("安装计划失败: %v", err)
	}
	r := BuildReceipt(plan, plan.Values, plan.ValuesDigest, InstallMeta{ID: "ins_9", At: ts, Namespace: "ns"})
	if len(r.Unapplied) != 1 {
		t.Fatalf("unapplied = %+v", r.Unapplied)
	}
	u := r.Unapplied[0]
	if u.Kind != "capability" || u.Ref != "a-review@3" || u.Field != "next" || u.Reason != NoteRoutingUnapplied {
		t.Fatalf("unapplied[0] = %+v", u)
	}
	if !hasNote(r.Notes, NoteRoutingUnapplied, "a-review@3") || !hasNote(r.Notes, NoteUnreachable, "a-fixer@1") {
		t.Fatalf("notes = %+v", r.Notes)
	}
	raw, _ := json.Marshal(r)
	var decoded struct {
		Unapplied []map[string]any `json:"unapplied"`
		Notes     []map[string]any `json:"notes"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("回执解析: %v", err)
	}
	for _, key := range []string{"kind", "ref", "field", "declared", "actual", "reason"} {
		if _, ok := decoded.Unapplied[0][key]; !ok {
			t.Fatalf("unapplied 缺 %q: %s", key, raw)
		}
	}
	for _, key := range []string{"code", "message"} {
		if _, ok := decoded.Notes[0][key]; !ok {
			t.Fatalf("notes 缺 %q: %s", key, raw)
		}
	}
}

// TestInstallsFoldKey 验 §4.10 的折叠键：同 (ns, 包, valuesDigest) 折叠成一套实例。
func TestInstallsFoldKey(t *testing.T) {
	a := InstallsFoldKey("ns1", "pb1", "sha256:aa")
	if a != InstallsFoldKey("ns1", "pb1", "sha256:aa") {
		t.Fatalf("同一个折叠键必须稳定")
	}
	for _, other := range []string{
		InstallsFoldKey("ns2", "pb1", "sha256:aa"),
		InstallsFoldKey("ns1", "pb2", "sha256:aa"),
		InstallsFoldKey("ns1", "pb1", "sha256:bb"),
	} {
		if other == a {
			t.Fatalf("折叠键必须区分 ns / 包 / valuesDigest: %q", other)
		}
	}
}

// TestBuildReceiptNilPlan 验 BuildReceipt 拿到空计划时也给合法形状（不 panic、不出 null）。
func TestBuildReceiptNilPlan(t *testing.T) {
	r := BuildReceipt(nil, nil, "", InstallMeta{ID: "ins_x", At: ts, Namespace: "ns"})
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if !strings.Contains(string(raw), `"capabilities":[]`) {
		t.Fatalf("空回执也要给 [] 而不是 null: %s", raw)
	}
}
