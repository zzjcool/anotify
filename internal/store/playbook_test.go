package store

// playbook（目录包）的数据层验收 —— v4 迁移。
//
// v4 的 playbook 是**真包**：`playbooks.files` 存 `{相对路径: 正文}` 原始包体
// （含入口 `playbook.yaml`），render / install 每次都重新 Parse（internal/playbook），
// 安装在一个 BEGIN IMMEDIATE 事务里完成「计划 → 建行 → 回执」。
//
// v3 的那套模型（manifest 里的 items/edges/checks、端口、优先级、handler、
// PlaybookChecks 的「装完再验」）在 v4 整体删除，因此下列 v3 用例**逐条退役**，
// 它们的保护点由本文件里真实存在的 v4 用例接走（映射见 TEST-MAPPING.md）：
//
//	v3 TestPlaybookChecksNodePresence / …ChecksPartialAndEntryCapabilities /
//	…ChecksResolvesDeclaredVersion / …ChecksHubHandlerRefIsReady
//	  → 退役：v4 没有 checks 读模型。安装是全有或全无的事务（冲突 → 0 写，
//	    见 TestInstallPlaybookDryRunWritesNothing 与 v4test 的 install matrix），
//	    「装完再逐项体检」这个中间态不存在；节点的可执行性由 dispatch 的
//	    NodesAvailable condition 回答（node_test.go / v4test）。
//	v3 …SkipsEdgeWithMissingEndpoint / …SkipsEdgeWithUnknownPort /
//	…EdgeCarriesLiteralsAndPriority / …TriggerNameAndMapping / …RefDoesNotCreateCapability /
//	…RefAndSpecBothGiven / …RejectsForeignHandler / …RejectsUnresolvableRef /
//	…SkipsSourcePointingAtSameEntry / …LegacyManifestStillInstalls
//	  → 退役：端口/优先级/handler/触发器映射/图边条目都属于 v3 manifest 模型。
//	    v4 的等价事实是「引用必须可解析、无端口、next 是裸名或 name@version」，
//	    由 internal/playbook 的 Parse/Render 校验（在 render/install 时统一生效），
//	    以及本文件的 TestPlaybookSlugValidation / TestRenderPlaybookReadOnly 覆盖。
//	v3 TestPlaybookManifestReservedOriginNames
//	  → 退役并上报：v4 的安装路径（createInstalledSourceOn → insertSourceOn）目前
//	    **没有**保留名（route）检查，所以这条约束在 v4 不存在，无法迁移。
//	v3 TestInstallPlaybookNewVersionSameSchema
//	  → 由 TestInstallPlaybookIdempotent（同内容复用、不产生新版本行）接走。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// ---------- 包体夹具 ----------

// pbValuesBlock 是入口文档的 spec.values（4 空格缩进，嵌进 playbook.yaml）。
const pbValuesBlock = `    type: object
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }`

// pbEntry 组装 playbook.yaml 的正文。
func pbEntry(slug string, version int, caps, srcs []string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n")
	b.WriteString("  name: " + slug + "\n")
	b.WriteString("  displayName: 测试包\n")
	if version > 0 {
		b.WriteString("  version: " + itoa64(int64(version)) + "\n")
	}
	b.WriteString("spec:\n  values:\n")
	b.WriteString(pbValuesBlock + "\n")
	if len(caps) == 0 {
		b.WriteString("  capabilities: []\n")
	} else {
		b.WriteString("  capabilities:\n")
		for _, p := range caps {
			b.WriteString("    - " + p + "\n")
		}
	}
	if len(srcs) == 0 {
		b.WriteString("  sources: []\n")
	} else {
		b.WriteString("  sources:\n")
		for _, p := range srcs {
			b.WriteString("    - " + p + "\n")
		}
	}
	return b.String()
}

// pbCapabilityDoc 生成一份能力文档（run.sh 正文按 YAML 块缩进）。
func pbCapabilityDoc(name string, next []string, body string) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Capability\nmetadata:\n")
	b.WriteString("  name: '" + name + "'\n")
	b.WriteString("spec:\n  params:\n    type: object\n  next: [")
	for i, ref := range next {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("'" + ref + "'")
	}
	b.WriteString("]\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n")
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("        " + line + "\n")
	}
	return b.String()
}

// pbSourceDoc 生成一份 webhook 源文档。
func pbSourceDoc(name string, targets []string) string {
	var b strings.Builder
	b.WriteString("metadata:\n  name: '" + name + "'\n")
	b.WriteString("spec:\n  kind: webhook\n  enabled: true\n  config: {}\n  targets:\n")
	for _, ref := range targets {
		b.WriteString("    - capability: '" + ref + "'\n      params:\n        repo: '{{values.project}}'\n")
	}
	return b.String()
}

// pbBundle 是「一个能力 + 一个源 + 入口」的最小可用包。
func pbBundle(slug string) map[string]string {
	capPath := "capabilities/review.yaml"
	srcPath := "sources/on-push.yaml"
	return map[string]string{
		"playbook.yaml": pbEntry(slug, 3, []string{capPath}, []string{srcPath}),
		capPath:         pbCapabilityDoc("{{values.project}}-review", nil, "#!/usr/bin/env bash\necho \"project={{values.project}}\""),
		srcPath:         pbSourceDoc("{{values.project}}-on-push", []string{"{{values.project}}-review"}),
	}
}

// pbEmpty 是「只有入口、没有任何能力/源」的包。
func pbEmpty(slug string) map[string]string {
	return map[string]string{"playbook.yaml": pbEntry(slug, 1, nil, nil)}
}

// pbValues 是安装/渲染的唯一 values 入口（project 必填）。
func pbValues(project string) map[string]any { return map[string]any{"project": project} }

// pbCreate 建包并返回行（失败即 Fatal）。
func pbCreate(t *testing.T, db *DB, nsID, slug string, files map[string]string) *PlaybookRow {
	t.Helper()
	row, err := db.CreatePlaybook(context.Background(), nsID, slug, files, "")
	if err != nil {
		t.Fatalf("create playbook %q: %v", slug, err)
	}
	return row
}

// pbTableCount 数表行数（用 retention_test 的 countRows，避免第二份实现）。
func pbInstalls(t *testing.T, db *DB) int64 {
	t.Helper()
	return countRows(t, db, "playbook_installs")
}

// pbVersionToken 读当前 resourceVersion 令牌（PUT/DELETE 的 CAS 入参）。
func pbVersionToken(t *testing.T, db *DB, nsID, slug string) string {
	t.Helper()
	row, err := db.GetPlaybook(context.Background(), nsID, slug)
	if err != nil {
		t.Fatalf("get playbook %q: %v", slug, err)
	}
	return ResourceVersion(row.ID, row.ResourceVersion)
}

// sourceToken 取 webhook 源的服务端生成 token（看 config 的唯一途径）。
func sourceToken(t *testing.T, src *Source) string {
	t.Helper()
	token := sourcespec.ParseConfig(src.Kind, src.Config).Token
	if token == "" {
		t.Fatalf("源 %q 没有 token（webhook 源应由服务端生成）", src.Name)
	}
	return token
}

// ---------- 用例 ----------

// TestPlaybookManifestRoundTrip：包体按**原始字节**往返（含入口文档与子文件）。
func TestPlaybookManifestRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	files := pbBundle("pr-review")
	row := pbCreate(t, db, nsID, "pr-review", files)
	if row.ID == "" || row.ResourceVersion != 1 {
		t.Fatalf("建包后身份不对: %+v", row)
	}
	if row.Version != 3 {
		t.Fatalf("metadata.version 应解出来 = 3, 实际 %d", row.Version)
	}

	got, err := db.GetPlaybook(ctx, nsID, "pr-review")
	if err != nil {
		t.Fatalf("取包: %v", err)
	}
	if len(got.Files) != len(files) {
		t.Fatalf("包体文件数 = %d, want %d", len(got.Files), len(files))
	}
	for path, want := range files {
		if got.Files[path] != want {
			t.Errorf("文件 %s 往返不一致:\n got %q\nwant %q", path, got.Files[path], want)
		}
	}
	// 读不改写：ResourceVersion 不该因为 GET 变化
	if got.ResourceVersion != row.ResourceVersion {
		t.Fatalf("GET 改了 resourceVersion: %d → %d", row.ResourceVersion, got.ResourceVersion)
	}
}

// TestPlaybookCreateGetListDelete：建/取/列/删 + CAS + 「删清单不删已装对象」。
func TestPlaybookCreateGetListDelete(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))
	pbCreate(t, db, nsID, "nightly-audit", pbEmpty("nightly-audit"))

	list, err := db.ListPlaybooks(ctx, nsID)
	if err != nil {
		t.Fatalf("列包: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应 2 个包, got %d", len(list))
	}

	// 装一次，让删除响应有「留下了什么」可说
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}
	capRows := countRows(t, db, "capability_versions")
	srcRows := countRows(t, db, "sources")

	// 旧令牌删除 → 稳定冲突，且什么都没删
	stale := ResourceVersion("pb_stale", 99)
	if _, err := db.DeletePlaybook(ctx, nsID, "pr-review", stale); !errors.Is(err, ErrResourceVersionConflict) {
		t.Fatalf("旧令牌应 ErrResourceVersionConflict, got %v", err)
	}
	if _, err := db.GetPlaybook(ctx, nsID, "pr-review"); err != nil {
		t.Fatalf("被拒的删除不该生效: %v", err)
	}

	res, err := db.DeletePlaybook(ctx, nsID, "pr-review", pbVersionToken(t, db, nsID, "pr-review"))
	if err != nil {
		t.Fatalf("删包: %v", err)
	}
	if !res.Deleted || res.Note != DeletePlaybookNote {
		t.Fatalf("删除响应不对: %+v", res)
	}
	// §7：清单不是所有权——能力/源一行都不动
	if got := countRows(t, db, "capability_versions"); got != capRows {
		t.Fatalf("删清单动了能力行: %d → %d", capRows, got)
	}
	if got := countRows(t, db, "sources"); got != srcRows {
		t.Fatalf("删清单动了源行: %d → %d", srcRows, got)
	}
	if len(res.Kept.Capabilities) != 1 || !strings.HasPrefix(res.Kept.Capabilities[0], "demo-review@") {
		t.Fatalf("kept.capabilities = %v, want 一条 demo-review@v", res.Kept.Capabilities)
	}
	if len(res.Kept.Sources) != 1 || res.Kept.Sources[0] != "demo-on-push" {
		t.Fatalf("kept.sources = %v, want [demo-on-push]", res.Kept.Sources)
	}
	if len(res.Values) != 1 || res.Values[0]["project"] != "demo" {
		t.Fatalf("删除响应应回这包装过的 values 组: %+v", res.Values)
	}
	if _, err := db.GetPlaybook(ctx, nsID, "pr-review"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删后应 ErrNotFound, got %v", err)
	}
}

// TestPlaybookEmptyManifestNormalized：v4 的入口文档是**必需**的——
// 「空清单」在 v3 会被归一成空列表，v4 直接拒（entry_missing），因为没入口就没法寻址。
func TestPlaybookEmptyManifestNormalized(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	_, err := db.CreatePlaybook(ctx, nsID, "empty-one", map[string]string{}, "")
	if !playbook.IsCode(err, playbook.CodeEntryMissing) {
		t.Fatalf("空包体应 entry_missing, got %v", err)
	}
	// 有入口但没有能力/源：合法（装出来什么也不建）
	row := pbCreate(t, db, nsID, "no-items", pbEmpty("no-items"))
	if len(row.Files) != 1 {
		t.Fatalf("包体应只有入口文件, got %v", row.Files)
	}
}

// TestPlaybookSlugValidation：slug 形状 / 与包内 metadata.name 一致 / 同 ns 唯一。
func TestPlaybookSlugValidation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	other := newTestExtraNS(t, db, "other")

	// 路径 slug 与包内 metadata.name 不一致 → 稳定哨兵
	files := pbEmpty("in-package")
	if _, err := db.CreatePlaybook(ctx, nsID, "different", files, ""); !errors.Is(err, ErrPlaybookSlugMismatch) {
		t.Fatalf("slug 不一致应 ErrPlaybookSlugMismatch, got %v", err)
	}

	// 包内名字非法（大写/下划线/前导连字符）→ slug_invalid
	for _, bad := range []string{"Bad", "a_b", "-lead"} {
		_, err := db.CreatePlaybook(ctx, nsID, "", map[string]string{"playbook.yaml": pbEntry(bad, 1, nil, nil)}, "")
		if !playbook.IsCode(err, playbook.CodeSlugInvalid) {
			t.Fatalf("slug %q 应 slug_invalid, got %v", bad, err)
		}
	}

	// slug 为空 → 取包内 metadata.name
	row, err := db.CreatePlaybook(ctx, nsID, "", pbEmpty("from-package"), "")
	if err != nil {
		t.Fatalf("slug 省略应取包内名字: %v", err)
	}
	if row.Slug != "from-package" {
		t.Fatalf("slug = %q, want from-package", row.Slug)
	}

	// 同 ns 重名 → ErrSlugTaken；另一个 ns 可以同名（隔离维度）
	if _, err := db.CreatePlaybook(ctx, nsID, "from-package", pbEmpty("from-package"), ""); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("重名应 ErrSlugTaken, got %v", err)
	}
	if _, err := db.CreatePlaybook(ctx, other, "from-package", pbEmpty("from-package"), ""); err != nil {
		t.Fatalf("跨命名空间同名应允许: %v", err)
	}
}

// TestRenderPlaybookReadOnly：render 是 helm template —— 不写库、不产生回执。
func TestRenderPlaybookReadOnly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))

	before := map[string]int64{
		"playbooks":           countRows(t, db, "playbooks"),
		"playbook_installs":   pbInstalls(t, db),
		"capability_versions": countRows(t, db, "capability_versions"),
		"sources":             countRows(t, db, "sources"),
		"edges":               countRows(t, db, "edges"),
	}

	out, err := db.RenderPlaybook(ctx, nsID, "pr-review", pbValues("demo"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out.ValuesDigest == "" || !strings.HasPrefix(out.ValuesDigest, "sha256:") {
		t.Fatalf("valuesDigest 形状不对: %q", out.ValuesDigest)
	}
	if len(out.Capabilities) != 1 || out.Capabilities[0].Metadata.Name != "demo-review" {
		t.Fatalf("渲染出的能力不对: %+v", out.Capabilities)
	}
	// 渲染正文含模板求值结果（不是 `{{values.project}}` 原文）
	if len(out.Files) != 2 {
		t.Fatalf("渲染文件数 = %d, want 2", len(out.Files))
	}
	capBody := out.Files["capabilities/review.yaml"]
	if !strings.Contains(capBody, "project=demo") {
		t.Fatalf("渲染后的脚本正文里没有求值结果:\n%s", capBody)
	}
	if strings.Contains(capBody, "{{values.") {
		t.Fatalf("渲染后还残留模板占位符:\n%s", capBody)
	}
	// 渲染两次一致（纯函数语义：同一 values → 同一 digest）
	again, err := db.RenderPlaybook(ctx, nsID, "pr-review", pbValues("demo"))
	if err != nil {
		t.Fatalf("render again: %v", err)
	}
	if again.ValuesDigest != out.ValuesDigest {
		t.Fatalf("同一 values 的 digest 不稳定: %s vs %s", out.ValuesDigest, again.ValuesDigest)
	}

	// 缺必填 values → values_missing（不是静默空串）
	if _, err := db.RenderPlaybook(ctx, nsID, "pr-review", map[string]any{}); !playbook.IsCode(err, playbook.CodeValuesMissing) {
		t.Fatalf("缺 project 应 values_missing, got %v", err)
	}

	after := map[string]int64{
		"playbooks":           countRows(t, db, "playbooks"),
		"playbook_installs":   pbInstalls(t, db),
		"capability_versions": countRows(t, db, "capability_versions"),
		"sources":             countRows(t, db, "sources"),
		"edges":               countRows(t, db, "edges"),
	}
	for table, want := range before {
		if after[table] != want {
			t.Fatalf("render 写了库：%s %d → %d", table, want, after[table])
		}
	}
}

// TestInstallPlaybookIdempotent：同一 values 装两次 → 第二次全是 reused，
// 能力/源不新增行、不产生新版本；回执历史仍记两次（attempts=2）。
func TestInstallPlaybookIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))

	first, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install 1: %v", err)
	}
	if len(first.Created.Capabilities) != 1 || len(first.Created.Sources) != 1 {
		t.Fatalf("首次安装应建 1 能力 + 1 源: %+v", first.Created)
	}
	if first.Created.Capabilities[0] != "demo-review@1" {
		t.Fatalf("created.capabilities = %v, want [demo-review@1]", first.Created.Capabilities)
	}
	capRows := countRows(t, db, "capability_versions")
	srcRows := countRows(t, db, "sources")
	edgeRows := countRows(t, db, "edges")

	second, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install 2: %v", err)
	}
	if len(second.Created.Capabilities) != 0 || len(second.Created.Sources) != 0 {
		t.Fatalf("同内容重装不该再建东西: %+v", second.Created)
	}
	if got := countRows(t, db, "capability_versions"); got != capRows {
		t.Fatalf("重装新增了能力行: %d → %d", capRows, got)
	}
	if got := countRows(t, db, "sources"); got != srcRows {
		t.Fatalf("重装新增了源行: %d → %d", srcRows, got)
	}
	if got := countRows(t, db, "edges"); got != edgeRows {
		t.Fatalf("重装改了边投影: %d → %d", edgeRows, got)
	}
	// 回执每次一条（历史是审计，不能被折叠掉）
	if got := pbInstalls(t, db); got != 2 {
		t.Fatalf("回执行数 = %d, want 2", got)
	}
	if first.Install.ID == second.Install.ID {
		t.Fatalf("两次安装的回执 id 不该相同: %s", first.Install.ID)
	}
}

// TestInstallPlaybookDryRunWritesNothing：dryRun 走同一条路径但在提交前回滚（0 持久写）。
func TestInstallPlaybookDryRunWritesNothing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))

	tables := []string{"playbooks", "playbook_installs", "capability_versions", "sources", "edges", "events", "tasks", "deployments"}
	before := map[string]int64{}
	for _, table := range tables {
		before[table] = countRows(t, db, table)
	}

	receipt, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), true)
	if err != nil {
		t.Fatalf("dryRun install: %v", err)
	}
	if !receipt.Install.DryRun {
		t.Fatalf("回执应标记 dryRun: %+v", receipt.Install)
	}
	// 计划本身是真的（dryRun 只是不落地）
	if len(receipt.Created.Capabilities) != 1 {
		t.Fatalf("dryRun 回执也应给出计划: %+v", receipt.Created)
	}
	for _, table := range tables {
		if got := countRows(t, db, table); got != before[table] {
			t.Fatalf("dryRun 写了库：%s %d → %d", table, before[table], got)
		}
	}
	// 回执也不该留下（回执行随事务回滚）
	if got := pbInstalls(t, db); got != 0 {
		t.Fatalf("dryRun 留下了回执行: %d", got)
	}
}

// TestInstallPlaybookInlineSpecStoresScriptBody：包内 run 正文按**渲染后**的字节落库，
// 且 digest 与正文同源（v4 只有脚本，没有 handler 镜像）。
func TestInstallPlaybookInlineSpecStoresScriptBody(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}

	c, err := db.GetCapability(ctx, nsID, "demo-review", 1)
	if err != nil {
		t.Fatalf("取装出来的能力: %v", err)
	}
	body := c.Spec.Run.Files["run.sh"]
	if !strings.Contains(body, "project=demo") {
		t.Fatalf("落库的脚本正文不是渲染结果: %q", body)
	}
	if strings.Contains(body, "{{values.") {
		t.Fatalf("落库的脚本正文残留模板: %q", body)
	}
	// 产物正文与能力正文同源
	art, err := db.CurrentArtifact(ctx, nsID, c.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if art.Digest != c.Digest {
		t.Fatalf("产物 digest = %q, want 能力行 %q", art.Digest, c.Digest)
	}
	var decoded map[string]any
	if err := json.Unmarshal(art.Spec, &decoded); err != nil {
		t.Fatalf("产物正文不是 JSON: %v", err)
	}
}

// TestInstallPlaybookWebhookSourceIsUsable：装出来的源是**真源**——
// token 由服务端生成、能按 token 找回、入口边指向包内能力。
func TestInstallPlaybookWebhookSourceIsUsable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}

	src, err := db.GetSourceByName(ctx, nsID, "demo-on-push")
	if err != nil {
		t.Fatalf("取装出来的源: %v", err)
	}
	if src.Kind != SourceKindWebhook || !src.Enabled {
		t.Fatalf("源形状不对: %+v", src)
	}
	if len(src.Targets) != 1 || src.Targets[0].Capability != "demo-review" {
		t.Fatalf("源出口不对: %+v", src.Targets)
	}
	// 装出来的源必须能按 token 找到（否则入口是死的）
	hook, err := db.LookupHook(ctx, sourceToken(t, src))
	if err != nil {
		t.Fatalf("按 token 找回装出来的源: %v", err)
	}
	if hook.ID != src.ID {
		t.Fatalf("token 找回的不是同一个源: %s vs %s", hook.ID, src.ID)
	}
	// 入口边投影（from_is_source=1）
	edges, err := db.ListEdges(ctx, nsID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	found := false
	for _, e := range edges {
		if e.FromIsSource && e.FromRef == "demo-on-push" && e.ToRef == "demo-review" {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有入口边 demo-on-push --> demo-review: %+v", edges)
	}
}

// TestInstallPlaybookCronSource：cron 源装完就排好下一次触发（否则要等进程重启）。
func TestInstallPlaybookCronSource(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	capPath := "capabilities/audit.yaml"
	srcPath := "sources/nightly.yaml"
	files := map[string]string{
		"playbook.yaml": pbEntry("nightly-audit", 1, []string{capPath}, []string{srcPath}),
		capPath:         pbCapabilityDoc("{{values.project}}-audit", nil, "#!/bin/sh\ntrue"),
		srcPath: "metadata:\n  name: '{{values.project}}-nightly'\n" +
			"spec:\n  kind: cron\n  enabled: true\n  config:\n    expr: '0 3 * * *'\n  targets:\n" +
			"    - capability: '{{values.project}}-audit'\n",
	}
	pbCreate(t, db, nsID, "nightly-audit", files)
	if _, err := db.InstallPlaybook(ctx, nsID, "nightly-audit", pbValues("demo"), false); err != nil {
		t.Fatalf("install: %v", err)
	}

	src, err := db.GetSourceByName(ctx, nsID, "demo-nightly")
	if err != nil {
		t.Fatalf("取装出来的 cron 源: %v", err)
	}
	if src.Kind != SourceKindCron || src.NextFireAt == nil {
		t.Fatalf("cron 源应带 nextFireAt: %+v", src)
	}
	due, err := db.ListDueSources(ctx, *src.NextFireAt, 10)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 || due[0].ID != src.ID {
		t.Fatalf("到点应调度到装出来的源, got %+v", due)
	}
}

// TestPlaybookInstallEmptyManifest：只有入口的包也能装（什么都不建，列表是 [] 不是 null）。
func TestPlaybookInstallEmptyManifest(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	pbCreate(t, db, nsID, "no-items", pbEmpty("no-items"))

	// 空包也要满足它声明的 values 契约（project 必填）
	receipt, err := db.InstallPlaybook(ctx, nsID, "no-items", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if receipt.Created.Capabilities == nil || receipt.Created.Sources == nil {
		t.Fatalf("created 的两个列表必须是 [] 而不是 null: %+v", receipt.Created)
	}
	if len(receipt.Created.Capabilities) != 0 || len(receipt.Created.Sources) != 0 {
		t.Fatalf("空包不该建任何东西: %+v", receipt.Created)
	}
	if got := pbInstalls(t, db); got != 1 {
		t.Fatalf("空包也应留一条回执, got %d", got)
	}
}

// TestListPlaybookInstallsGroups：实例清单按 `(包, valuesDigest)` 折叠，
// attempts 是同组回执数；caps/sources 是组内回执反查的并集。
func TestListPlaybookInstallsGroups(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)
	row := pbCreate(t, db, nsID, "pr-review", pbBundle("pr-review"))

	for i := 0; i < 2; i++ {
		if _, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("demo"), false); err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
	}
	// 另一套 values → 另一个折叠组
	if _, err := db.InstallPlaybook(ctx, nsID, "pr-review", pbValues("other"), false); err != nil {
		t.Fatalf("install other: %v", err)
	}

	list, err := db.ListPlaybookInstalls(ctx, nsID, row.ID)
	if err != nil {
		t.Fatalf("list installs: %v", err)
	}
	if list.Total != 2 || len(list.Installs) != 2 {
		t.Fatalf("应有 2 个折叠组, got total=%d n=%d", list.Total, len(list.Installs))
	}
	if list.Installs == nil {
		t.Fatal("installs 必须是 [] 而不是 null")
	}
	var demo *PlaybookInstallGroup
	for i := range list.Installs {
		if list.Installs[i].Values["project"] == "demo" {
			demo = &list.Installs[i]
		}
	}
	if demo == nil {
		t.Fatalf("找不到 project=demo 的组: %+v", list.Installs)
	}
	if demo.Attempts != 2 {
		t.Fatalf("demo 组 attempts = %d, want 2", demo.Attempts)
	}
	if demo.LastInstallID == "" {
		t.Fatal("组里应指到最新一次回执")
	}
	if len(demo.Capabilities) != 1 || !strings.HasPrefix(demo.Capabilities[0], "demo-review@") {
		t.Fatalf("组的 capabilities = %v, want 一条 demo-review@v", demo.Capabilities)
	}
	if len(demo.Sources) != 1 || demo.Sources[0] != "demo-on-push" {
		t.Fatalf("组的 sources = %v, want [demo-on-push]", demo.Sources)
	}

	// 当前清单 id 不匹配（同 slug 删了重建的新包）→ 看不到旧实例
	if list, err := db.ListPlaybookInstalls(ctx, nsID, "pb_other"); err != nil {
		t.Fatalf("list installs other: %v", err)
	} else if list.Total != 0 || len(list.Installs) != 0 {
		t.Fatalf("别的清单 id 不该看到这些实例: %+v", list)
	}
}

// TestListPlaybooksCreatedAtOrder：列表按 created_at（同一毫秒内按 id）升序，稳定。
func TestListPlaybooksCreatedAtOrder(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	slugs := []string{"aaa", "bbb", "ccc"}
	for i, slug := range slugs {
		pbCreate(t, db, nsID, slug, pbEmpty(slug))
		// 让 created_at 有明确先后（同一进程里 Now() 可能同毫秒）
		mustExec(t, db, `UPDATE playbooks SET created_at = ? WHERE namespace_id = ? AND slug = ?`, Now()+int64(i), nsID, slug)
	}

	list, err := db.ListPlaybooks(ctx, nsID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("应 3 个包, got %d", len(list))
	}
	for i, want := range slugs {
		if list[i].Slug != want {
			t.Fatalf("第 %d 个 = %q, want %q（应按 created_at 升序）", i, list[i].Slug, want)
		}
	}
}

// TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate 父裁定 #2（B 轮）：
// 渲染产物必须走**与直接创建完全相同**的校验路径（playbook.md §2.2）——
// 源名 `route` 是保留剪枝来源名，直写 CreateSource 拒，安装写路径也必须拒，
// 且整次安装 0 写（不建能力、不建源、不建安装回执、不落入口边）。
//
// 本用例用**真原包**（raw files map）证明物化后的 Source.name 恰好是 `route`：
// 源文档里 `metadata.name: route` 是字面量，不经过 values 模板、不是前缀变体。
func TestPlaybookInstallReservedSourceNameRejectedLikeDirectCreate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	// ① 直写路径的同一规则（对照组；没有包参与）。
	direct := &Source{
		NamespaceID: nsID, Name: EventPruneSource, Kind: SourceKindWebhook,
		Config: json.RawMessage(`{}`), Enabled: true,
	}
	if err := db.CreateSource(ctx, direct); !errors.Is(err, ErrReservedEventSource) {
		t.Fatalf("direct CreateSource(%q) err = %v, want ErrReservedEventSource", EventPruneSource, err)
	}

	// ② 真原包：能力 + 源（物化名恰好是保留名 route）。
	capPath := "capabilities/audit.yaml"
	srcPath := "sources/prune.yaml"
	files := map[string]string{
		"playbook.yaml": pbEntry("reserved-source", 1, []string{capPath}, []string{srcPath}),
		capPath:         pbCapabilityDoc("{{values.project}}-audit", nil, "#!/bin/sh\ntrue"),
		srcPath: "metadata:\n  name: route\n" +
			"spec:\n  kind: webhook\n  enabled: true\n  config: {}\n  targets:\n" +
			"    - capability: '{{values.project}}-audit'\n",
	}
	pbCreate(t, db, nsID, "reserved-source", files)

	receipt, err := db.InstallPlaybook(ctx, nsID, "reserved-source", pbValues("demo"), false)
	if err == nil {
		// 产品缺陷：安装接受了保留源名。把「物化名恰好是 route」与 0 写要求
		// 一起钉在报错里，便于父会话按具名 case + 2 处生产点裁决。
		var src *Source
		if got, gerr := db.GetSourceByName(ctx, nsID, EventPruneSource); gerr == nil {
			src = got
		}
		t.Fatalf("安装接受了物化名为 %q 的源（direct CreateSource 拒绝同一名字）；receipt=%+v sourceRow=%+v",
			EventPruneSource, receipt, src)
	}
	if !errors.Is(err, ErrReservedEventSource) {
		t.Fatalf("安装保留源名应报 ErrReservedEventSource（与直写同规则），得到 %v", err)
	}

	// ③ 整次安装 0 写：源/安装回执/能力/入口边一行都不许留。
	if _, gerr := db.GetSourceByName(ctx, nsID, EventPruneSource); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("被拒安装留下了源行 route: %v", gerr)
	}
	if n := pbInstalls(t, db); n != 0 {
		t.Fatalf("被拒安装留下了 %d 行安装回执", n)
	}
	caps, cerr := db.ListCapabilities(ctx, nsID)
	if cerr != nil {
		t.Fatalf("list capabilities: %v", cerr)
	}
	if len(caps) != 0 {
		t.Fatalf("被拒安装留下了 %d 个能力行: %+v", len(caps), caps)
	}
	edges, eerr := db.ListEdges(ctx, nsID)
	if eerr != nil {
		t.Fatalf("list edges: %v", eerr)
	}
	if len(edges) != 0 {
		t.Fatalf("被拒安装留下了 %d 条边: %+v", len(edges), edges)
	}
}

// TestPlaybookInstallAllowsV4CapabilityNamesThatWereReservedInV3 父裁定 #2（前半）：
// v3 的 `TestPlaybookManifestReservedOriginNames` 实际禁的是**能力名** github/cron/http/manual；
// v4 里这些名字完全合法（能力名正则 ^[a-z0-9][a-z0-9._-]{0,62}$）。
// 真原包安装必须全部建出来——这是“允许清单”用例，不是新的黑名单。
func TestPlaybookInstallAllowsV4CapabilityNamesThatWereReservedInV3(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	nsID, _ := newTestNS(t, db)

	names := []string{"github", "cron", "http", "manual"}
	files := map[string]string{}
	capPaths := make([]string, 0, len(names))
	for _, name := range names {
		path := "capabilities/" + name + ".yaml"
		capPaths = append(capPaths, path)
		files[path] = pbCapabilityDoc(name, nil, "#!/bin/sh\ntrue")
	}
	files["playbook.yaml"] = pbEntry("legal-names", 1, capPaths, nil)
	pbCreate(t, db, nsID, "legal-names", files)

	r, err := db.InstallPlaybook(ctx, nsID, "legal-names", pbValues("demo"), false)
	if err != nil {
		t.Fatalf("v4 合法能力名安装被拒: %v", err)
	}
	if r == nil {
		t.Fatal("安装回执为空")
	}
	for _, name := range names {
		c, gerr := db.GetCapability(ctx, nsID, name, 1)
		if gerr != nil {
			t.Fatalf("能力 %q 未建出: %v", name, gerr)
		}
		if c.Name != name || c.Version != 1 {
			t.Fatalf("能力 %q 落库形状 = %+v", name, c)
		}
	}
}
