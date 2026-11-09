package v4test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zzjcool/anotify/internal/playbook"
	"github.com/zzjcool/anotify/internal/sourcespec"
	"github.com/zzjcool/anotify/internal/store"
)

// 本文件是 G1-5b 的**真实 SQLite** 验收：目录包持久化 / render 只读 / 单事务安装 /
// 12 格动作矩阵的数据库行为 / dryRun 0 写 / token 与 writeOnly 脱敏。
//
// 纪律：不用纯 Plan 单测替代 DB 原子性（那是 G1-5a 的 `internal/playbook` 测试），
// 这里每条断言都读库（行数 / digest / resource_version / edges / 回执行）或读回执。

// ---------- 包体夹具 ----------

// pbValuesSchema 是测试包的 spec.values（4 空格缩进，嵌进 playbook.yaml 的 spec.values）。
const pbValuesSchema = `    type: object
    additionalProperties: false
    required: [project]
    properties:
      project: { type: string, pattern: '^[a-z0-9][a-z0-9-]{0,20}$' }
      env: { type: string, enum: [dev, staging, prod], default: prod }
      repo: { type: string, default: '' }
      slackToken: { type: string, writeOnly: true }
      replicas: { type: integer, default: 2 }`

// pbPackage 是测试用的目录包构造器（入口 + 能力/源文件 + 引用清单）。
type pbPackage struct {
	slug        string
	displayName string
	version     int
	values      string
	caps        []string
	srcs        []string
	files       map[string]string
}

func newPBPackage(slug string) *pbPackage {
	return &pbPackage{
		slug:        slug,
		displayName: "单项目审查链",
		version:     3,
		values:      pbValuesSchema,
		files:       map[string]string{},
	}
}

// withDefault 填「两能力 + 一源」的标准包（与设计文档 §1 的例子同形）。
func (p *pbPackage) withDefault() *pbPackage {
	p.caps = []string{"capabilities/review.yaml", "capabilities/fixer.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:        "{{values.project}}-review",
		displayName: "{{values.project}} 审查",
		params:      "    type: object\n    properties: { repo: { type: string } }",
		next:        []string{"{{values.project}}-fixer"},
		runBody:     "#!/usr/bin/env bash\necho \"project={{values.project}} env={{values.env}}\"",
	})
	p.files["capabilities/fixer.yaml"] = pbCapDoc(pbCapOpts{
		name:    "{{values.project}}-fixer",
		next:    []string{},
		runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "")
	return p
}

// build 渲染 playbook.yaml 并返回完整文件集合。
func (p *pbPackage) build() map[string]string {
	files := make(map[string]string, len(p.files)+1)
	for path, body := range p.files {
		files[path] = body
	}
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Playbook\nmetadata:\n")
	fmt.Fprintf(&b, "  name: %s\n", p.slug)
	if p.displayName != "" {
		fmt.Fprintf(&b, "  displayName: %s\n", p.displayName)
	}
	if p.version > 0 {
		fmt.Fprintf(&b, "  version: %d\n", p.version)
	}
	b.WriteString("spec:\n  values:\n")
	b.WriteString(p.values)
	b.WriteString("\n")
	if len(p.caps) == 0 {
		b.WriteString("  capabilities: []\n")
	} else {
		b.WriteString("  capabilities:\n")
		for _, path := range p.caps {
			fmt.Fprintf(&b, "    - %s\n", path)
		}
	}
	if len(p.srcs) == 0 {
		b.WriteString("  sources: []\n")
	} else {
		b.WriteString("  sources:\n")
		for _, path := range p.srcs {
			fmt.Fprintf(&b, "    - %s\n", path)
		}
	}
	files["playbook.yaml"] = b.String()
	return files
}

// pbCapOpts 是一份模板化能力文档的字段。
type pbCapOpts struct {
	name        string
	version     int
	displayName string
	params      string
	next        []string
	runBody     string
}

// pbCapDoc 生成一份能力文档（run 正文逐行缩进成 YAML 块）。
func pbCapDoc(o pbCapOpts) string {
	var b strings.Builder
	b.WriteString("apiVersion: anotify/v1\nkind: Capability\nmetadata:\n")
	fmt.Fprintf(&b, "  name: '%s'\n", o.name)
	if o.displayName != "" {
		fmt.Fprintf(&b, "  displayName: '%s'\n", o.displayName)
	}
	if o.version > 0 {
		fmt.Fprintf(&b, "  version: %d\n", o.version)
	}
	b.WriteString("spec:\n")
	if o.params != "" {
		fmt.Fprintf(&b, "  params:\n%s\n", o.params)
	}
	b.WriteString("  next: [")
	for i, ref := range o.next {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "'%s'", ref)
	}
	b.WriteString("]\n  run:\n    entrypoint: run.sh\n    files:\n      run.sh: |\n")
	for _, line := range strings.Split(strings.TrimRight(o.runBody, "\n"), "\n") {
		fmt.Fprintf(&b, "        %s\n", line)
	}
	return b.String()
}

// pbSourceDoc 生成一份 webhook 源文档；token 非空时显式写 config.token。
func pbSourceDoc(targets []string, token string) string {
	var b strings.Builder
	b.WriteString("metadata:\n  name: '{{values.project}}-on-push'\nspec:\n  kind: webhook\n  enabled: true\n")
	if token == "" {
		b.WriteString("  config: {}\n")
	} else {
		fmt.Fprintf(&b, "  config:\n    token: '%s'\n", token)
	}
	b.WriteString("  targets:\n")
	for _, ref := range targets {
		fmt.Fprintf(&b, "    - capability: '%s'\n      params:\n        repo: '{{values.repo}}'\n", ref)
	}
	return b.String()
}

// pbValues 是安装 values 的唯一入口（project 必填；writeOnly secret 只在这里给）。
func pbValues(project string) map[string]any {
	return map[string]any{"project": project}
}

// ---------- 通用断言辅助 ----------

// pbCountedTables 是「0 写 / 回滚」要逐表对比的集合（安装可能触碰的全部表）。
var pbCountedTables = []string{
	"playbooks", "playbook_installs", "capability_versions", "sources",
	"edges", "events", "tasks", "deployments",
}

func pbTableCounts(t *testing.T, db *store.DB) map[string]int {
	t.Helper()
	out := make(map[string]int, len(pbCountedTables))
	for _, table := range pbCountedTables {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func pbRequireCounts(t *testing.T, before, after map[string]int, when string) {
	t.Helper()
	for _, table := range pbCountedTables {
		if before[table] != after[table] {
			t.Fatalf("%s：表 %s 行数变了 %d → %d（应当 0 写）", when, table, before[table], after[table])
		}
	}
}

func pbCreate(t *testing.T, db *store.DB, nsID string, p *pbPackage) *store.PlaybookRow {
	t.Helper()
	row, err := db.CreatePlaybook(context.Background(), nsID, p.slug, p.build(), "")
	if err != nil {
		t.Fatalf("create playbook %s: %v", p.slug, err)
	}
	return row
}

func pbReplace(t *testing.T, db *store.DB, nsID string, p *pbPackage, rv int64) *store.PlaybookRow {
	t.Helper()
	row, err := db.ReplacePlaybook(context.Background(), nsID, p.slug, rv, p.build(), "")
	if err != nil {
		t.Fatalf("replace playbook %s: %v", p.slug, err)
	}
	return row
}

func pbInstall(t *testing.T, db *store.DB, nsID, slug string, values map[string]any) *playbook.Receipt {
	t.Helper()
	r, err := db.InstallPlaybook(context.Background(), nsID, slug, values, false)
	if err != nil {
		t.Fatalf("install playbook %s: %v", slug, err)
	}
	return r
}

// pbInstallErr 调一次安装并返回错误（不 Fatal：冲突/失败路径要用它）。
func pbInstallErr(t *testing.T, db *store.DB, nsID, slug string, values map[string]any, dryRun bool) (*playbook.Receipt, error) {
	t.Helper()
	return db.InstallPlaybook(context.Background(), nsID, slug, values, dryRun)
}

// pbLatestCap 取某名字的最新能力行。
func pbLatestCap(t *testing.T, db *store.DB, nsID, name string) *store.CapabilityVersion {
	t.Helper()
	c, err := db.GetCapabilityLatest(context.Background(), nsID, name)
	if err != nil {
		t.Fatalf("get latest capability %s: %v", name, err)
	}
	return c
}

// pbCapCount 数某名字的能力行数。
func pbCapCount(t *testing.T, db *store.DB, nsID, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM capability_versions WHERE namespace_id = ? AND name = ?`,
		nsID, name).Scan(&n); err != nil {
		t.Fatalf("count capability %s: %v", name, err)
	}
	return n
}

// pbSource 取源行；不存在 Fatal。
func pbSource(t *testing.T, db *store.DB, nsID, name string) *store.Source {
	t.Helper()
	s, err := db.GetSourceByName(context.Background(), nsID, name)
	if err != nil {
		t.Fatalf("get source %s: %v", name, err)
	}
	return s
}

// pbSourceCount 数源行数。
func pbSourceCount(t *testing.T, db *store.DB, nsID, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sources WHERE namespace_id = ? AND name = ?`, nsID, name).Scan(&n); err != nil {
		t.Fatalf("count source %s: %v", name, err)
	}
	return n
}

// pbCreatedHas 断言回执的 created 列表包含某引用。
func pbCreatedHas(t *testing.T, got []string, want string, what string) {
	t.Helper()
	for _, ref := range got {
		if ref == want {
			return
		}
	}
	t.Fatalf("%s created 不含 %q：%v", what, want, got)
}

// pbJSONHas 断言一段 JSON 里包含某子串（脱敏负断言用）。
func pbJSONHas(t *testing.T, v any, needle string) bool {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return strings.Contains(string(raw), needle)
}

// ---------- 包读写 / render ----------

// TestPlaybookCreateGetReplace 覆盖：Parse 值无关校验、slug 一致性、同 slug 409、
// 原始 files 往返、PUT 整体换模板 + CAS 哨兵、只换模板不动已装行。
func TestPlaybookCreateGetReplace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-crud")

	p := newPBPackage("project-chain").withDefault()
	files := p.build()
	row, err := db.CreatePlaybook(ctx, ns, "", files, "oci://registry.example/playbooks/project-chain@sha256:abc")
	if err != nil {
		t.Fatalf("create playbook: %v", err)
	}
	if row.Slug != "project-chain" || row.Name != "project-chain" {
		t.Fatalf("slug/name = %q/%q（应为包内 metadata.name）", row.Slug, row.Name)
	}
	if row.Source == "" {
		t.Fatalf("OCI 来源应落库")
	}

	// 原始 files 往返：GET 回来的就是上传的原文（未渲染）。
	got, err := db.GetPlaybook(ctx, ns, "project-chain")
	if err != nil {
		t.Fatalf("get playbook: %v", err)
	}
	if !reflect.DeepEqual(got.Files, files) {
		t.Fatalf("files 往返不一致:\n got %v\nwant %v", got.Files, files)
	}
	if got.Files["capabilities/review.yaml"] != p.files["capabilities/review.yaml"] {
		t.Fatalf("包体被改写（includeBody 必须给原文）")
	}
	if got.DisplayName != "单项目审查链" || got.Version != 3 {
		t.Fatalf("包展示字段往返不对：%+v", got)
	}

	// 同 slug 再 POST → ErrSlugTaken；路径 slug 与包内 metadata.name 不一致 → 哨兵。
	if _, err := db.CreatePlaybook(ctx, ns, "project-chain", files, ""); !errors.Is(err, store.ErrSlugTaken) {
		t.Fatalf("同 slug 创建应返回 ErrSlugTaken，得到 %v", err)
	}
	if _, err := db.CreatePlaybook(ctx, ns, "other-slug", files, ""); !errors.Is(err, store.ErrPlaybookSlugMismatch) {
		t.Fatalf("slug 不一致应返回 ErrPlaybookSlugMismatch，得到 %v", err)
	}
	// 包形状错误在写库前拒绝（entry_missing）。
	if _, err := db.CreatePlaybook(ctx, ns, "broken", map[string]string{"capabilities/x.yaml": "kind: Capability\n"}, ""); playbook.Code(err) != playbook.CodeEntryMissing {
		t.Fatalf("缺入口应返回 entry_missing，得到 %v", err)
	}
	list, err := db.ListPlaybooks(ctx, ns)
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("被拒的包不该落库，列表 = %d", len(list))
	}

	// 先装一次，再 PUT 换模板：已装的能力行一行不动（PUT 只换模板）。
	receipt := pbInstall(t, db, ns, "project-chain", pbValues("a"))
	reviewBefore := pbLatestCap(t, db, ns, "a-review")
	capCountBefore := pbCapCount(t, db, ns, "a-review")
	srcBefore := pbSource(t, db, ns, "a-on-push")

	// CAS 哨兵：缺 rv / 旧 rv。
	changed := newPBPackage("project-chain").withDefault()
	changed.displayName = "改名后的链"
	changed.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:        "{{values.project}}-review",
		displayName: "{{values.project}} 审查 v2",
		params:      "    type: object\n    properties: { repo: { type: string } }",
		next:        []string{"{{values.project}}-fixer"},
		runBody:     "#!/usr/bin/env bash\necho v2",
	})
	if _, err := db.ReplacePlaybook(ctx, ns, "project-chain", 0, changed.build(), ""); !errors.Is(err, store.ErrResourceVersionRequired) {
		t.Fatalf("缺 rv 应返回 ErrResourceVersionRequired，得到 %v", err)
	}
	if _, err := db.ReplacePlaybook(ctx, ns, "project-chain", row.ResourceVersion+7, changed.build(), ""); !errors.Is(err, store.ErrResourceVersionConflict) {
		t.Fatalf("旧 rv 应返回 ErrResourceVersionConflict，得到 %v", err)
	}
	updated := pbReplace(t, db, ns, changed, got.ResourceVersion)
	if updated.ResourceVersion != got.ResourceVersion+1 {
		t.Fatalf("PUT 后 resource_version 应 +1：%d → %d", got.ResourceVersion, updated.ResourceVersion)
	}
	if updated.Files["capabilities/review.yaml"] == p.files["capabilities/review.yaml"] {
		t.Fatalf("PUT 应替换包体")
	}
	reviewAfter := pbLatestCap(t, db, ns, "a-review")
	if reviewAfter.Digest != reviewBefore.Digest || reviewAfter.ResourceVersion != reviewBefore.ResourceVersion {
		t.Fatalf("PUT 不该动已装能力行（digest/rv 都变了）")
	}
	if pbCapCount(t, db, ns, "a-review") != capCountBefore {
		t.Fatalf("PUT 不该新增能力行")
	}
	if srcAfter := pbSource(t, db, ns, "a-on-push"); srcAfter.ResourceVersion != srcBefore.ResourceVersion {
		t.Fatalf("PUT 不该动已装源行")
	}
	if updated.DisplayName != "改名后的链" {
		t.Fatalf("PUT 应更新展示名：%q", updated.DisplayName)
	}

	// 回执的快照：namespace slug / 原包 id / 原 slug / OCI provenance 都落库。
	var nsSlugSnap, pbIDSnap, pbSlugSnap, srcSnap string
	if err := db.QueryRow(
		`SELECT namespace_slug, playbook_id, playbook_slug, source FROM playbook_installs WHERE id = ?`,
		receipt.Install.ID).Scan(&nsSlugSnap, &pbIDSnap, &pbSlugSnap, &srcSnap); err != nil {
		t.Fatalf("read receipt snapshot: %v", err)
	}
	if nsSlugSnap != "pb-crud" || pbIDSnap != row.ID || pbSlugSnap != "project-chain" ||
		srcSnap != "oci://registry.example/playbooks/project-chain@sha256:abc" {
		t.Fatalf("回执快照不对：ns=%q id=%q slug=%q src=%q", nsSlugSnap, pbIDSnap, pbSlugSnap, srcSnap)
	}
	if receipt.Namespace != "pb-crud" || receipt.Playbook.Name != "project-chain" ||
		receipt.Source != srcSnap {
		t.Fatalf("回执响应缺快照/provenance：%+v", receipt)
	}
}

// TestRenderPlaybookReadOnlyAndConflictFree 验 render：只读包体、0 写、不读安装冲突库。
func TestRenderPlaybookReadOnlyAndConflictFree(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-render")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	// 库里先放一条与渲染结果冲突的同名能力（不同正文）：render 仍必须成功。
	createCap(t, db, testCap(ns, "a-review", 1,
		[]string{"somewhere-else"}))

	before := pbTableCounts(t, db)
	rvBefore := pbLatestCap(t, db, ns, "a-review").ResourceVersion
	res, err := db.RenderPlaybook(ctx, ns, "project-chain", pbValues("a"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(res.Capabilities) != 2 || len(res.Sources) != 1 {
		t.Fatalf("渲染产物数量不对：caps=%d sources=%d", len(res.Capabilities), len(res.Sources))
	}
	if res.Values["env"] != "prod" {
		t.Fatalf("默认值未填充：%v", res.Values["env"])
	}
	// render 的 files 是**渲染后**的（模板已求值），不是原文。
	if body := res.Files["capabilities/review.yaml"]; !strings.Contains(body, "a-review") || strings.Contains(body, "{{values.") {
		t.Fatalf("render files 应是渲染后的正文：%q", body)
	}
	if res.ValuesDigest == "" {
		t.Fatalf("render 应给 valuesDigest")
	}
	after := pbTableCounts(t, db)
	pbRequireCounts(t, before, after, "render")
	if pbLatestCap(t, db, ns, "a-review").ResourceVersion != rvBefore {
		t.Fatalf("render 不该改已有能力的 resource_version")
	}

	// 不存在 / values 非法：错误原样（不落任何行）。
	if _, err := db.RenderPlaybook(ctx, ns, "ghost", pbValues("a")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("render 不存在的包应 ErrNotFound，得到 %v", err)
	}
	if _, err := db.RenderPlaybook(ctx, ns, "project-chain", map[string]any{}); playbook.Code(err) != playbook.CodeValuesMissing {
		t.Fatalf("缺 required values 应 values_missing，得到 %v", err)
	}
}

// ---------- 12 格：每格一条真实 DB 行为 ----------

// TestInstallMatrixDBRows 是 playbook.md §4.2 的 12 格，每格都读库断言。
func TestInstallMatrixDBRows(t *testing.T) {
	t.Run("row01_capability_created", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r01")
		pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

		before := pbTableCounts(t, db)
		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))
		after := pbTableCounts(t, db)

		pbCreatedHas(t, r.Created.Capabilities, "a-review@1", "created.capabilities")
		pbCreatedHas(t, r.Created.Capabilities, "a-fixer@1", "created.capabilities")
		pbCreatedHas(t, r.Created.Sources, "a-on-push", "created.sources")
		if len(r.Skipped.Capabilities) != 0 || len(r.Skipped.Sources) != 0 {
			t.Fatalf("全新安装不该有 skipped：%+v", r.Skipped)
		}
		if r.Install.DryRun || r.Install.ID == "" || r.Install.At == "" {
			t.Fatalf("普通安装应带 id/at 且 dryRun=false：%+v", r.Install)
		}
		if r.APIVersion != "anotify/v1" || r.Kind != "InstallReceipt" {
			t.Fatalf("回执信封不对：%s/%s", r.APIVersion, r.Kind)
		}
		if after["capability_versions"] != before["capability_versions"]+2 ||
			after["sources"] != before["sources"]+1 ||
			after["playbook_installs"] != before["playbook_installs"]+1 {
			t.Fatalf("行数增量不对：before=%v after=%v", before, after)
		}
		// 能力行：版本 1、digest 非空、installed_from 指回执。
		review := pbLatestCap(t, db, ns, "a-review")
		if review.Version != 1 || review.Digest == "" || review.InstalledFrom != r.Install.ID {
			t.Fatalf("能力行不对：%+v", review)
		}
		if review.DisplayName != "a 审查" {
			t.Fatalf("displayName 未落库：%q", review.DisplayName)
		}
		// 边投影：能力 next + 源入口边（只对新建源写）。
		if refs := edgeRefs(t, db, ns, false, "a-review"); !reflect.DeepEqual(refs, []string{"a-fixer"}) {
			t.Fatalf("a-review 出边 = %v", refs)
		}
		if refs := edgeRefs(t, db, ns, true, "a-on-push"); !reflect.DeepEqual(refs, []string{"a-review"}) {
			t.Fatalf("源入口边 = %v", refs)
		}
		// 安装不写 deployments / tasks。
		if after["deployments"] != 0 || after["tasks"] != 0 {
			t.Fatalf("install 不该写 deployments/tasks：%v", after)
		}
	})

	t.Run("row02_capability_fold_skipped", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r02")
		pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

		first := pbInstall(t, db, ns, "project-chain", pbValues("a"))
		capBefore := pbCapCount(t, db, ns, "a-review")
		srcBefore := pbSourceCount(t, db, ns, "a-on-push")
		reviewBefore := pbLatestCap(t, db, ns, "a-review")
		edgesBefore := len(edgeRefs(t, db, ns, false, "a-review"))

		second := pbInstall(t, db, ns, "project-chain", pbValues("a"))
		pbCreatedHas(t, second.Skipped.Capabilities, "a-review@1", "skipped.capabilities")
		pbCreatedHas(t, second.Skipped.Sources, "a-on-push", "skipped.sources")
		if len(second.Created.Capabilities) != 0 || len(second.Created.Sources) != 0 {
			t.Fatalf("同内容再装不该 created：%+v", second.Created)
		}
		if pbCapCount(t, db, ns, "a-review") != capBefore || pbSourceCount(t, db, ns, "a-on-push") != srcBefore {
			t.Fatalf("同内容再装不该新增行")
		}
		reviewAfter := pbLatestCap(t, db, ns, "a-review")
		if reviewAfter.ResourceVersion != reviewBefore.ResourceVersion || reviewAfter.InstalledFrom != first.Install.ID {
			t.Fatalf("复用不该改已存行（rv/installedFrom 变了）：%+v", reviewAfter)
		}
		if len(edgeRefs(t, db, ns, false, "a-review")) != edgesBefore {
			t.Fatalf("复用不该重建边")
		}
		if second.Install.ID == first.Install.ID {
			t.Fatalf("每次普通 install 必须产生新回执（id 不能复用）")
		}
		for _, note := range second.Notes {
			if note.Code == playbook.NoteRoutingUnapplied || note.Code == playbook.NoteTargetsUnapplied {
				t.Fatalf("「早装过了」不该写 unapplied note：%+v", note)
			}
		}
		var receipts int
		if err := db.QueryRow(`SELECT count(*) FROM playbook_installs WHERE namespace_id = ?`, ns).Scan(&receipts); err != nil {
			t.Fatalf("count receipts: %v", err)
		}
		if receipts != 2 {
			t.Fatalf("两次 install 应有两份回执，得到 %d", receipts)
		}
	})

	t.Run("row03_capability_new_version_upgraded", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r03")
		p := newPBPackage("project-chain").withDefault()
		created := pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, "project-chain", pbValues("a"))
		before := pbLatestCap(t, db, ns, "a-review")

		p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
			name:        "{{values.project}}-review",
			displayName: "{{values.project}} 审查",
			params:      "    type: object\n    properties: { repo: { type: string } }",
			next:        []string{"{{values.project}}-fixer"},
			runBody:     "#!/usr/bin/env bash\necho v2",
		})
		pbReplace(t, db, ns, p, created.ResourceVersion)
		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

		pbCreatedHas(t, r.Created.Capabilities, "a-review@2", "created.capabilities")
		if len(r.Upgraded) != 1 {
			t.Fatalf("应有一条 upgraded，得到 %+v", r.Upgraded)
		}
		if r.Upgraded[0].Capability != "a-review" || r.Upgraded[0].From != 1 || r.Upgraded[0].To != 2 {
			t.Fatalf("upgraded 内容不对：%+v", r.Upgraded[0])
		}
		if r.Upgraded[0].Affects.Sources < 1 {
			t.Fatalf("affects.sources 应统计指向裸名的源：%+v", r.Upgraded[0].Affects)
		}
		if pbCapCount(t, db, ns, "a-review") != 2 {
			t.Fatalf("新版本应追加一行")
		}
		old, err := db.GetCapability(context.Background(), ns, "a-review", 1)
		if err != nil {
			t.Fatalf("旧版本应保留：%v", err)
		}
		if old.Digest != before.Digest || old.Spec.Next[0] != "a-fixer" {
			t.Fatalf("旧版本行被改动：%+v", old)
		}
		latest := pbLatestCap(t, db, ns, "a-review")
		if latest.Version != 2 || latest.Digest == before.Digest {
			t.Fatalf("新版本行不对：version=%d digest=%s", latest.Version, latest.Digest)
		}
	})

	t.Run("row04_explicit_version_created", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r04")
		p := newPBPackage("explicit-pack")
		p.caps = []string{"capabilities/explicit.yaml"}
		p.srcs = nil
		p.files["capabilities/explicit.yaml"] = pbCapDoc(pbCapOpts{
			name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
		})
		pbCreate(t, db, ns, p)

		r := pbInstall(t, db, ns, p.slug, pbValues("a"))
		pbCreatedHas(t, r.Created.Capabilities, "explicit-cap@7", "created.capabilities")
		got := pbLatestCap(t, db, ns, "explicit-cap")
		if got.Version != 7 {
			t.Fatalf("显式版本应原样落库：%d", got.Version)
		}
	})

	t.Run("row05_explicit_version_same_content_skipped", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r05")
		p := newPBPackage("explicit-pack")
		p.caps = []string{"capabilities/explicit.yaml"}
		p.srcs = nil
		p.files["capabilities/explicit.yaml"] = pbCapDoc(pbCapOpts{
			name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
		})
		pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, p.slug, pbValues("a"))
		r := pbInstall(t, db, ns, p.slug, pbValues("a"))
		pbCreatedHas(t, r.Skipped.Capabilities, "explicit-cap@7", "skipped.capabilities")
		if pbCapCount(t, db, ns, "explicit-cap") != 1 {
			t.Fatalf("同版同内容应复用")
		}
	})

	t.Run("row06_explicit_version_conflict_zero_write", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r06")
		p := newPBPackage("explicit-pack")
		p.caps = []string{"capabilities/explicit.yaml"}
		p.srcs = nil
		p.files["capabilities/explicit.yaml"] = pbCapDoc(pbCapOpts{
			name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
		})
		row := pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, p.slug, pbValues("a"))
		before := pbTableCounts(t, db)
		digestBefore := pbLatestCap(t, db, ns, "explicit-cap").Digest

		p.files["capabilities/explicit.yaml"] = pbCapDoc(pbCapOpts{
			name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\necho changed",
		})
		pbReplace(t, db, ns, p, row.ResourceVersion)
		_, err := pbInstallErr(t, db, ns, p.slug, pbValues("a"), false)
		if playbook.Code(err) != playbook.CodeVersionContentImmutable {
			t.Fatalf("显式同版异内容应 version_content_immutable，得到 %v", err)
		}
		var pe *playbook.Error
		if !errors.As(err, &pe) {
			t.Fatalf("错误应是 *playbook.Error：%T", err)
		}
		if len(pe.Details["conflicts"].([]playbook.Conflict)) != 1 {
			t.Fatalf("conflicts 明细缺失：%+v", pe.Details)
		}
		if pe.Details["valuesDigest"] == "" || pe.Details["hint"] == "" {
			t.Fatalf("400 形状缺 valuesDigest/hint：%+v", pe.Details)
		}
		after := pbTableCounts(t, db)
		pbRequireCounts(t, before, after, "显式版本冲突")
		if pbLatestCap(t, db, ns, "explicit-cap").Digest != digestBefore {
			t.Fatalf("冲突后已存行不可变")
		}
	})

	t.Run("row07_next_change_unapplied", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r07")
		p := newPBPackage("project-chain").withDefault()
		row := pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, "project-chain", pbValues("a"))
		before := pbLatestCap(t, db, ns, "a-review")

		p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
			name:        "{{values.project}}-review",
			displayName: "{{values.project}} 审查-改名",
			params:      "    type: object\n    properties: { repo: { type: string } }",
			next:        []string{"{{values.project}}-other"},
			runBody:     "#!/usr/bin/env bash\necho \"project={{values.project}} env={{values.env}}\"",
		})
		pbReplace(t, db, ns, p, row.ResourceVersion)
		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

		if len(r.Unapplied) != 1 || r.Unapplied[0].Reason != playbook.ReasonRoutingUnapplied {
			t.Fatalf("应有 routing_unapplied：%+v", r.Unapplied)
		}
		if r.Unapplied[0].Ref != "a-review@1" || r.Unapplied[0].Field != "next" {
			t.Fatalf("unapplied 定位不对：%+v", r.Unapplied[0])
		}
		declared, ok := r.Unapplied[0].Declared.([]string)
		if !ok || !reflect.DeepEqual(declared, []string{"a-other"}) {
			t.Fatalf("declared = %#v", r.Unapplied[0].Declared)
		}
		actual, ok := r.Unapplied[0].Actual.([]string)
		if !ok || !reflect.DeepEqual(actual, []string{"a-fixer"}) {
			t.Fatalf("actual = %#v", r.Unapplied[0].Actual)
		}
		after := pbLatestCap(t, db, ns, "a-review")
		if !reflect.DeepEqual(after.Spec.Next, []string{"a-fixer"}) {
			t.Fatalf("已存路由被改动：%v", after.Spec.Next)
		}
		if after.ResourceVersion != before.ResourceVersion || after.Digest != before.Digest {
			t.Fatalf("unapplied 不该写已存行：rv %d→%d", before.ResourceVersion, after.ResourceVersion)
		}
		if after.DisplayName != before.DisplayName {
			t.Fatalf("displayName 被偷改：%q → %q", before.DisplayName, after.DisplayName)
		}
		if refs := edgeRefs(t, db, ns, false, "a-review"); !reflect.DeepEqual(refs, []string{"a-fixer"}) {
			t.Fatalf("边被改动：%v", refs)
		}
		pbCreatedHas(t, r.Pending, "a-other", "pending")
	})

	t.Run("row08_source_created_with_token", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r08")
		pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())
		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

		src := pbSource(t, db, ns, "a-on-push")
		token := sourcespec.ParseConfig(src.Kind, src.Config).Token
		if token == "" {
			t.Fatalf("省略 token 的 webhook 源应由服务端生成：%s", src.Config)
		}
		if src.InstalledFrom != r.Install.ID {
			t.Fatalf("新源 installed_from 应指本次回执：%q", src.InstalledFrom)
		}
		hook, err := db.LookupHook(context.Background(), token)
		if err != nil || hook.ID != src.ID {
			t.Fatalf("生成的 token 应可定位源：%v", err)
		}
		if refs := edgeRefs(t, db, ns, true, "a-on-push"); !reflect.DeepEqual(refs, []string{"a-review"}) {
			t.Fatalf("新建源应写入入口边：%v", refs)
		}
	})

	t.Run("row09_source_equivalent_reused", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r09")
		pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())
		pbInstall(t, db, ns, "project-chain", pbValues("a"))
		before := pbSource(t, db, ns, "a-on-push")
		tokenBefore := sourcespec.ParseConfig(before.Kind, before.Config).Token
		edgesBefore := len(edgeRefs(t, db, ns, true, "a-on-push"))

		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))
		pbCreatedHas(t, r.Skipped.Sources, "a-on-push", "skipped.sources")
		after := pbSource(t, db, ns, "a-on-push")
		if sourcespec.ParseConfig(after.Kind, after.Config).Token != tokenBefore {
			t.Fatalf("复用源不该重建 token")
		}
		if after.ResourceVersion != before.ResourceVersion {
			t.Fatalf("复用源不该改 resource_version")
		}
		if pbSourceCount(t, db, ns, "a-on-push") != 1 || len(edgeRefs(t, db, ns, true, "a-on-push")) != edgesBefore {
			t.Fatalf("复用源不该新增行/边")
		}
	})

	t.Run("row10_source_config_conflict_zero_write", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r10")
		p := newPBPackage("project-chain").withDefault()
		row := pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, "project-chain", pbValues("a"))
		before := pbTableCounts(t, db)
		existing := pbSource(t, db, ns, "a-on-push")
		existingToken := sourcespec.ParseConfig(existing.Kind, existing.Config).Token

		const explicitToken = "src_explicit_secret_value"
		p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, explicitToken)
		pbReplace(t, db, ns, p, row.ResourceVersion)
		_, err := pbInstallErr(t, db, ns, p.slug, pbValues("a"), false)
		if playbook.Code(err) != playbook.CodePlaybookConflict {
			t.Fatalf("源 config 真不同应 playbook_conflict，得到 %v", err)
		}
		var pe *playbook.Error
		if errors.As(err, &pe) {
			conflicts := pe.Details["conflicts"].([]playbook.Conflict)
			if len(conflicts) != 1 || conflicts[0].Kind != "source" || conflicts[0].Reason != playbook.ConflictConfigDiffers {
				t.Fatalf("conflict 明细不对：%+v", conflicts)
			}
			if !reflect.DeepEqual(conflicts[0].Fields, []string{"config.token"}) {
				t.Fatalf("应点名 config.token（不给值）：%v", conflicts[0].Fields)
			}
			if pbJSONHas(t, pe.Details, explicitToken) || pbJSONHas(t, pe.Details, existingToken) {
				t.Fatalf("冲突详情泄露了 token 值：%+v", pe.Details)
			}
		} else {
			t.Fatalf("错误应是 *playbook.Error：%T", err)
		}
		after := pbTableCounts(t, db)
		pbRequireCounts(t, before, after, "源 config 冲突")
		if got := sourcespec.ParseConfig(pbSource(t, db, ns, "a-on-push").Kind,
			pbSource(t, db, ns, "a-on-push").Config).Token; got != existingToken {
			t.Fatalf("冲突后源 token 被改动")
		}
	})

	t.Run("row11_source_target_pending", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r11")
		p := newPBPackage("project-chain").withDefault()
		p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-missing"}, "")
		pbCreate(t, db, ns, p)

		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))
		pbCreatedHas(t, r.Pending, "a-missing", "pending")
		foundNote := false
		for _, note := range r.Notes {
			if note.Code == playbook.NotePendingRef && note.Ref == "a-missing" {
				foundNote = true
			}
		}
		if !foundNote {
			t.Fatalf("pending 引用必须有 pending_ref note：%+v", r.Notes)
		}
		src := pbSource(t, db, ns, "a-on-push")
		if len(src.Targets) != 1 || src.Targets[0].Capability != "a-missing" {
			t.Fatalf("pending 目标应原样落库：%+v", src.Targets)
		}
		if refs := edgeRefs(t, db, ns, true, "a-on-push"); !reflect.DeepEqual(refs, []string{"a-missing"}) {
			t.Fatalf("pending 目标的入口边也应写入：%v", refs)
		}
	})

	t.Run("row12_source_targets_unapplied", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-r12")
		p := newPBPackage("project-chain").withDefault()
		row := pbCreate(t, db, ns, p)
		pbInstall(t, db, ns, "project-chain", pbValues("a"))
		before := pbSource(t, db, ns, "a-on-push")

		p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-fixer"}, "")
		pbReplace(t, db, ns, p, row.ResourceVersion)
		r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

		if len(r.Unapplied) != 1 || r.Unapplied[0].Reason != playbook.ReasonTargetsUnapplied {
			t.Fatalf("应有 targets_unapplied：%+v", r.Unapplied)
		}
		if r.Unapplied[0].Kind != "source" || r.Unapplied[0].Ref != "a-on-push" || r.Unapplied[0].Field != "targets" {
			t.Fatalf("unapplied 定位不对：%+v", r.Unapplied[0])
		}
		declared, ok := r.Unapplied[0].Declared.([]sourcespec.Target)
		if !ok || len(declared) != 1 || declared[0].Capability != "a-fixer" {
			t.Fatalf("declared targets = %#v", r.Unapplied[0].Declared)
		}
		actual, ok := r.Unapplied[0].Actual.([]sourcespec.Target)
		if !ok || len(actual) != 1 || actual[0].Capability != "a-review" {
			t.Fatalf("actual targets = %#v", r.Unapplied[0].Actual)
		}
		after := pbSource(t, db, ns, "a-on-push")
		if !sourcespec.SameTargets(after.Targets, before.Targets) {
			t.Fatalf("已存 targets 被改动：%+v → %+v", before.Targets, after.Targets)
		}
		if after.ResourceVersion != before.ResourceVersion {
			t.Fatalf("unapplied 不该写已存源行")
		}
		if refs := edgeRefs(t, db, ns, true, "a-on-push"); !reflect.DeepEqual(refs, []string{"a-review"}) {
			t.Fatalf("入口边被改动：%v", refs)
		}
		foundNote := false
		for _, note := range r.Notes {
			if note.Code == playbook.NoteTargetsUnapplied {
				foundNote = true
			}
		}
		if !foundNote {
			t.Fatalf("targets_unapplied 必须有 note：%+v", r.Notes)
		}
	})
}

// ---------- 事务 / dryRun / 并发 ----------

// TestInstallConflictRollsBackWholePackage 验多资源包：已安排 create 的条目 + 冲突条目 →
// 整包 0 写（能力 / 源 / 边 / 回执行一起回滚）。
func TestInstallConflictRollsBackWholePackage(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-rollback")

	// 先装一个只有源的同名包，造成「源 config 真冲突」。
	seed := newPBPackage("seed-pack")
	seed.caps = nil
	seed.srcs = []string{"sources/on-push.yaml"}
	seed.files["sources/on-push.yaml"] = pbSourceDoc([]string{"seed-target"}, "")
	pbCreate(t, db, ns, seed)
	pbInstall(t, db, ns, seed.slug, pbValues("a"))

	// 新包：先 create 一个新能力（a-extra），再与已有源冲突。
	p := newPBPackage("project-chain")
	p.caps = []string{"capabilities/extra.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/extra.yaml"] = pbCapDoc(pbCapOpts{
		name: "a-extra", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"a-extra"}, "src_a_different_token")
	pbCreate(t, db, ns, p)

	before := pbTableCounts(t, db)
	_, err := pbInstallErr(t, db, ns, p.slug, pbValues("a"), false)
	if playbook.Code(err) != playbook.CodePlaybookConflict {
		t.Fatalf("应 playbook_conflict，得到 %v", err)
	}
	after := pbTableCounts(t, db)
	pbRequireCounts(t, before, after, "整包冲突回滚")
	if pbCapCount(t, db, ns, "a-extra") != 0 {
		t.Fatalf("冲突前的 create 条目也必须回滚")
	}
	if refs := edgeRefs(t, db, ns, true, "a-on-push"); len(refs) != 1 {
		t.Fatalf("回滚后入口边应保持原样：%v", refs)
	}
}

// TestInstallConcurrentSamePackage 验两个并发的同包安装：新对象只 1 份、回执 2 份。
func TestInstallConcurrentSamePackage(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-concurrent")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		mu    sync.Mutex
		errs  []error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.InstallPlaybook(context.Background(), ns, "project-chain", pbValues("a"), false)
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发安装 #%d 失败：%v", i, err)
		}
	}
	if n := pbCapCount(t, db, ns, "a-review"); n != 1 {
		t.Fatalf("并发安装应只建 1 份能力，得到 %d", n)
	}
	if n := pbSourceCount(t, db, ns, "a-on-push"); n != 1 {
		t.Fatalf("并发安装应只建 1 份源，得到 %d", n)
	}
	var receipts int
	if err := db.QueryRow(`SELECT count(*) FROM playbook_installs WHERE namespace_id = ?`, ns).Scan(&receipts); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if receipts != 2 {
		t.Fatalf("并发安装应有两份回执，得到 %d", receipts)
	}
}

// TestInstallDryRunWritesNothing 验 dryRun：同一事务路径 + Rollback，所有表 0 持久写。
func TestInstallDryRunWritesNothing(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-dryrun")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	before := pbTableCounts(t, db)
	r, err := db.InstallPlaybook(ctx, ns, "project-chain", pbValues("a"), true)
	if err != nil {
		t.Fatalf("dryRun: %v", err)
	}
	if !r.Install.DryRun || r.Install.ID == "" {
		t.Fatalf("dryRun 回执应标 dryRun 且带 id：%+v", r.Install)
	}
	if len(r.Created.Capabilities) != 2 || len(r.Created.Sources) != 1 {
		t.Fatalf("dryRun 应给完整回执（同一 Plan）：%+v", r.Created)
	}
	after := pbTableCounts(t, db)
	pbRequireCounts(t, before, after, "dryRun")
	// 新生成的 webhook token 不持久残留（源行也没了）。
	var tokens int
	if err := db.QueryRow(`SELECT count(*) FROM sources WHERE namespace_id = ?`, ns).Scan(&tokens); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("dryRun 不该留下源行（token 会随之泄漏）")
	}

	// 冲突路径：先真实装一次（建立源），再把包改成显式不同 token；
	// dryRun 与真实安装必须报同一个领域码，且 0 写。
	const tokenA = "src_real_token_a"
	const tokenB = "src_dryrun_secret_b"
	p := newPBPackage("project-chain").withDefault()
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, tokenA)
	pbReplace(t, db, ns, p, pbCreateRV(t, db, ns, "project-chain"))
	pbInstall(t, db, ns, "project-chain", pbValues("a"))
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, tokenB)
	pbReplace(t, db, ns, p, pbCreateRV(t, db, ns, "project-chain"))

	before = pbTableCounts(t, db)
	_, dryErr := db.InstallPlaybook(ctx, ns, "project-chain", pbValues("a"), true)
	if playbook.Code(dryErr) != playbook.CodePlaybookConflict {
		t.Fatalf("dryRun 冲突码不对：%v", dryErr)
	}
	_, realErr := db.InstallPlaybook(ctx, ns, "project-chain", pbValues("a"), false)
	if playbook.Code(realErr) != playbook.CodePlaybookConflict {
		t.Fatalf("真实安装冲突码不对：%v", realErr)
	}
	pbRequireCounts(t, before, pbTableCounts(t, db), "dryRun/真实冲突")
}

// pbCreateRV 取当前 playbook 的 resourceVersion（PUT 前用）。
func pbCreateRV(t *testing.T, db *store.DB, nsID, slug string) int64 {
	t.Helper()
	row, err := db.GetPlaybook(context.Background(), nsID, slug)
	if err != nil {
		t.Fatalf("get playbook rv: %v", err)
	}
	return row.ResourceVersion
}

// TestInstallReuseNeverUpdatesExistingRow 是 I-P1 的负样本：复用（skipped）条目
// **不能**走 CreateCapability 的折叠更新路径去改已存行的 next / displayName。
func TestInstallReuseNeverUpdatesExistingRow(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-no-overwrite")
	p := newPBPackage("project-chain").withDefault()
	row := pbCreate(t, db, ns, p)
	pbInstall(t, db, ns, "project-chain", pbValues("a"))
	before := pbLatestCap(t, db, ns, "a-review")

	// 只改 displayName（foldKey 不变）→ 复用，但绝不改已存行。
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:        "{{values.project}}-review",
		displayName: "{{values.project}} 审查（新名字）",
		params:      "    type: object\n    properties: { repo: { type: string } }",
		next:        []string{"{{values.project}}-fixer"},
		runBody:     "#!/usr/bin/env bash\necho \"project={{values.project}} env={{values.env}}\"",
	})
	pbReplace(t, db, ns, p, row.ResourceVersion)
	r := pbInstall(t, db, ns, "project-chain", pbValues("a"))

	pbCreatedHas(t, r.Skipped.Capabilities, "a-review@1", "skipped.capabilities")
	after := pbLatestCap(t, db, ns, "a-review")
	if after.DisplayName != before.DisplayName {
		t.Fatalf("复用路径偷改了 displayName：%q → %q", before.DisplayName, after.DisplayName)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("复用路径写了已存行（resource_version %d → %d）", before.ResourceVersion, after.ResourceVersion)
	}
}

// TestInstallSnapshotReadErrorFails 验「读错不能当空库」：损坏的行必须让安装失败，
// 而不是被当成「库是空的」从而多建一批对象。
func TestInstallSnapshotReadErrorFails(t *testing.T) {
	ctx := context.Background()
	t.Run("corrupt_source_targets", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-corrupt-src")
		pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())
		if _, err := db.Exec(
			`INSERT INTO sources (id, namespace_id, name, kind, config, targets, enabled, resource_version, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			"src_corrupt", ns, "a-on-push", "webhook", `{"token":"t"}`, `{not-json`, 1, 1, store.Now()); err != nil {
			t.Fatalf("insert corrupt source: %v", err)
		}
		before := pbTableCounts(t, db)
		_, err := db.InstallPlaybook(ctx, ns, "project-chain", pbValues("a"), false)
		if err == nil {
			t.Fatalf("损坏的源行应让安装失败（不能当空库）")
		}
		if playbook.Code(err) != "" {
			t.Fatalf("读错误不该伪装成领域冲突：%v", err)
		}
		pbRequireCounts(t, before, pbTableCounts(t, db), "快照读错误")
		if pbCapCount(t, db, ns, "a-review") != 0 {
			t.Fatalf("读失败后不该创建任何能力")
		}
	})

	t.Run("corrupt_capability_spec", func(t *testing.T) {
		db := openTestDB(t)
		ns := newNS(t, db, "pb-corrupt-cap")
		// 显式版本安装会按 (name, version) 精确读那一行，损坏的 spec 必须在 Plan 阶段暴露。
		p := newPBPackage("explicit-pack")
		p.caps = []string{"capabilities/explicit.yaml"}
		p.srcs = nil
		p.files["capabilities/explicit.yaml"] = pbCapDoc(pbCapOpts{
			name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
		})
		pbCreate(t, db, ns, p)
		if _, err := db.Exec(
			`INSERT INTO capability_versions
			   (id, namespace_id, name, display_name, version, spec, digest, fold_key, installed_from, resource_version, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			"cap_corrupt", ns, "explicit-cap", "", 7, "{not-json", "sha256:x", "sha256:y", "", 1, store.Now()); err != nil {
			t.Fatalf("insert corrupt capability: %v", err)
		}
		before := pbTableCounts(t, db)
		if _, err := db.InstallPlaybook(ctx, ns, p.slug, pbValues("a"), false); err == nil {
			t.Fatalf("损坏的能力行应让安装失败（不能当空库）")
		}
		pbRequireCounts(t, before, pbTableCounts(t, db), "能力快照读错误")
	})
}

// ---------- values / token 脱敏 ----------

// TestInstallValuesWriteOnlyMaskedDigestOnRaw 验 writeOnly：回执 values 打码、
// digest 对打码前算、库里不落明文；render 与 install 的 digest 一致。
func TestInstallValuesWriteOnlyMaskedDigestOnRaw(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-mask")
	pbCreate(t, db, ns, newPBPackage("project-chain").withDefault())

	const secret = "super-secret-slack-token"
	values := map[string]any{"project": "a", "slackToken": secret}
	rendered, err := db.RenderPlaybook(ctx, ns, "project-chain", values)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if rendered.Values["slackToken"] != secret {
		t.Fatalf("render 的有效 values 应保留真实 writeOnly 值（用于渲染/digest）")
	}
	r := pbInstall(t, db, ns, "project-chain", values)
	if r.Values["slackToken"] != playbook.MaskedValue {
		t.Fatalf("回执 values 应打码：%v", r.Values["slackToken"])
	}
	if r.ValuesDigest != rendered.ValuesDigest {
		t.Fatalf("install 的 valuesDigest 应与 render 对打码前 values 的结果一致：%s vs %s",
			r.ValuesDigest, rendered.ValuesDigest)
	}
	if maskedDigest, err := playbook.ValuesDigest(map[string]any{
		"project": "a", "env": "prod", "repo": "", "replicas": float64(2),
		"slackToken": playbook.MaskedValue,
	}); err != nil {
		t.Fatalf("digest masked: %v", err)
	} else if maskedDigest == r.ValuesDigest {
		t.Fatalf("digest 不能打在打码后的 values 上")
	}

	// 库里的 values / receipt 都不得含明文 secret。
	var rawValues, rawReceipt string
	if err := db.QueryRow(`SELECT "values", receipt FROM playbook_installs WHERE id = ?`,
		r.Install.ID).Scan(&rawValues, &rawReceipt); err != nil {
		t.Fatalf("read stored receipt: %v", err)
	}
	if strings.Contains(rawValues, secret) || strings.Contains(rawReceipt, secret) {
		t.Fatalf("库里落入了 writeOnly 明文")
	}
	// 回执里的 created/skipped 是对象（空列表是 [] 不是 null）。
	var shape struct {
		Created map[string]json.RawMessage `json:"created"`
		Skipped map[string]json.RawMessage `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(rawReceipt), &shape); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if string(shape.Created["capabilities"]) == "" || string(shape.Created["sources"]) == "" ||
		string(shape.Skipped["capabilities"]) == "" || string(shape.Skipped["sources"]) == "" {
		t.Fatalf("created/skipped 必须是 {capabilities:[],sources:[]} 对象：%s", rawReceipt)
	}
	if string(shape.Skipped["capabilities"]) != "[]" || string(shape.Skipped["sources"]) != "[]" {
		t.Fatalf("空列表必须是 [] 不是 null：%s", rawReceipt)
	}

	// 再装一次（全 skipped）：两个对象都必须是空数组而不是 null。
	r2 := pbInstall(t, db, ns, "project-chain", values)
	raw2, err := json.Marshal(r2)
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if !strings.Contains(string(raw2), `"created":{"capabilities":[],"sources":[]}`) {
		t.Fatalf("全 skipped 的 created 应为空数组对象：%s", raw2)
	}
	if strings.Contains(string(raw2), secret) {
		t.Fatalf("回执 JSON 泄露 writeOnly 明文")
	}
}

// TestInstallTokenOmittedKeepsExisting 验 token 语义：省略保留现有、显式相同复用、
// 显式不同拒绝（且错误不泄露值）。
func TestInstallTokenOmittedKeepsExisting(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-token")
	p := newPBPackage("project-chain").withDefault()
	row := pbCreate(t, db, ns, p)
	pbInstall(t, db, ns, "project-chain", pbValues("a"))
	token := sourcespec.ParseConfig(pbSource(t, db, ns, "a-on-push").Kind,
		pbSource(t, db, ns, "a-on-push").Config).Token
	if token == "" {
		t.Fatalf("前提：首次安装应生成 token")
	}

	// 显式相同 token → 等价复用。
	row = pbReplace(t, db, ns, func() *pbPackage {
		p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, token)
		return p
	}(), row.ResourceVersion)
	r := pbInstall(t, db, ns, "project-chain", pbValues("a"))
	pbCreatedHas(t, r.Skipped.Sources, "a-on-push", "skipped.sources")

	// 显式不同 token → 冲突；错误 JSON 不含任何一个 token。
	row = pbReplace(t, db, ns, func() *pbPackage {
		p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "src_new_rotation")
		return p
	}(), row.ResourceVersion)
	_, err := pbInstallErr(t, db, ns, "project-chain", pbValues("a"), false)
	if playbook.Code(err) != playbook.CodePlaybookConflict {
		t.Fatalf("显式不同 token 应 playbook_conflict，得到 %v", err)
	}
	if pbJSONHas(t, err, "src_new_rotation") {
		t.Fatalf("错误体泄露了新 token")
	}
	if pbJSONHas(t, err, token) {
		t.Fatalf("错误体泄露了库里的旧 token")
	}
	if got := sourcespec.ParseConfig(pbSource(t, db, ns, "a-on-push").Kind,
		pbSource(t, db, ns, "a-on-push").Config).Token; got != token {
		t.Fatalf("拒绝后 token 被改动")
	}
}

// TestInstallMissingPlaybookAndNamespace 是安装的 404 / 400 边界。
func TestInstallMissingPlaybookAndNamespace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-missing")
	if _, err := db.InstallPlaybook(ctx, ns, "ghost", pbValues("a"), false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在的包应 ErrNotFound，得到 %v", err)
	}
	if _, err := db.InstallPlaybook(ctx, "", "ghost", pbValues("a"), false); !errors.Is(err, store.ErrNamespaceRequired) {
		t.Fatalf("空 ns 应 ErrNamespaceRequired，得到 %v", err)
	}
	if _, err := db.GetPlaybook(ctx, ns, "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetPlaybook 不存在应 ErrNotFound，得到 %v", err)
	}
}

// TestInstallFoldKeyDeterministicHighestVersion 验省略 version 的同内容复用必须选**最高存在版本**：
// @1 与 @3 同 foldKey 时，skipped 与 next 比较对象都锚在 @3（不是 SQLite 的偶然返回顺序）。
func TestInstallFoldKeyDeterministicHighestVersion(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-fold-highest")

	p := newPBPackage("project-chain")
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = nil
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:    "{{values.project}}-review",
		version: 1,
		next:    []string{"{{values.project}}-other"},
		runBody: "#!/usr/bin/env bash\ntrue",
	})
	row := pbCreate(t, db, ns, p)
	pbInstall(t, db, ns, p.slug, pbValues("a"))

	// 再手工插一行 @3：正文与 @1 完全相同（foldKey 相同，version 不同）。
	v1, err := db.GetCapability(ctx, ns, "a-review", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	v3 := &store.CapabilityVersion{
		NamespaceID: ns,
		Name:        "a-review",
		DisplayName: v1.DisplayName,
		Version:     3,
		Spec:        v1.Spec,
	}
	if err := db.CreateCapability(ctx, v3); err != nil {
		t.Fatalf("create v3 same content: %v", err)
	}
	if pbCapCount(t, db, ns, "a-review") != 2 {
		t.Fatalf("夹具前提：@1 与 @3 两行")
	}

	// 包改成省略 version + 改 next（foldKey 仍相同）→ 必须复用 @3，并报 routing_unapplied@3。
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:    "{{values.project}}-review",
		next:    []string{"{{values.project}}-fixer"},
		runBody: "#!/usr/bin/env bash\ntrue",
	})
	pbReplace(t, db, ns, p, row.ResourceVersion)
	r := pbInstall(t, db, ns, p.slug, pbValues("a"))

	pbCreatedHas(t, r.Skipped.Capabilities, "a-review@3", "skipped.capabilities")
	for _, ref := range r.Skipped.Capabilities {
		if ref == "a-review@1" {
			t.Fatalf("同 foldKey 必须选最高版本，不能复用 @1")
		}
	}
	if len(r.Unapplied) != 1 || r.Unapplied[0].Ref != "a-review@3" {
		t.Fatalf("next 比较对象应是最高版本 @3：%+v", r.Unapplied)
	}
	if pbCapCount(t, db, ns, "a-review") != 2 {
		t.Fatalf("复用不该新增行")
	}
	latest := pbLatestCap(t, db, ns, "a-review")
	if latest.Version != 3 || !reflect.DeepEqual(latest.Spec.Next, []string{"a-other"}) {
		t.Fatalf("最高版本行被改动：%+v", latest)
	}
	edges := edgeRefs(t, db, ns, false, "a-review")
	if !reflect.DeepEqual(edges, []string{"a-other"}) {
		t.Fatalf("unapplied 后边应保持 @3 的路由：%v", edges)
	}
}

// TestInstallExplicitLowerVersionKeepsBareRoute 验「显式低版本新建」：
// 已有 max=3 时装 v1 只是多一行可钉死的版本，裸名解析与边投影仍属于 v3。
func TestInstallExplicitLowerVersionKeepsBareRoute(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-low-version")

	// 库里先有 a-review@3（next 指向 a-keep）。
	if err := db.CreateCapability(ctx, testCap(ns, "a-review", 3, []string{"a-keep"})); err != nil {
		t.Fatalf("seed v3: %v", err)
	}

	p := newPBPackage("low-pack")
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = nil
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name:    "a-review",
		version: 1,
		next:    []string{"a-low"},
		runBody: "#!/usr/bin/env bash\necho low",
	})
	pbCreate(t, db, ns, p)
	r := pbInstall(t, db, ns, p.slug, pbValues("a"))
	pbCreatedHas(t, r.Created.Capabilities, "a-review@1", "created.capabilities")

	// 裸名仍解析到 v3，边投影仍是 v3 的 next；新行只带自己的 next。
	resolved, err := db.ResolveRef(ctx, ns, "a-review")
	if err != nil {
		t.Fatalf("resolve bare name: %v", err)
	}
	if resolved.Version != 3 {
		t.Fatalf("max 版本不应被低版本新建改变：%d", resolved.Version)
	}
	if !reflect.DeepEqual(resolved.Spec.Next, []string{"a-keep"}) {
		t.Fatalf("v3 的 next 被改动：%v", resolved.Spec.Next)
	}
	if edges := edgeRefs(t, db, ns, false, "a-review"); !reflect.DeepEqual(edges, []string{"a-keep"}) {
		t.Fatalf("显式低版本新建改动了裸名边投影：%v", edges)
	}
	low, err := db.GetCapability(ctx, ns, "a-review", 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if !reflect.DeepEqual(low.Spec.Next, []string{"a-low"}) {
		t.Fatalf("新行自己的 next 应原样落库：%v", low.Spec.Next)
	}
}

// TestInstallWriteOnlyCompositeMasked 验 writeOnly 的**整数组/整对象**：
// 回执整体打 "***"，digest 仍对打码前的真实 values 算，库里不落明文。
func TestInstallWriteOnlyCompositeMasked(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ns := newNS(t, db, "pb-mask-composite")

	p := newPBPackage("secret-pack")
	p.values = `    type: object
    additionalProperties: false
    required: [project]
    properties:
      project: { type: string }
      secrets: { type: array, writeOnly: true, items: { type: string } }
      auth: { type: object, writeOnly: true }`
	p.caps = []string{"capabilities/fixer.yaml"}
	p.srcs = nil
	p.files["capabilities/fixer.yaml"] = pbCapDoc(pbCapOpts{
		name: "{{values.project}}-fixer", next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	pbCreate(t, db, ns, p)

	values := map[string]any{
		"project": "a",
		"secrets": []any{"arr-secret-one", "arr-secret-two"},
		"auth":    map[string]any{"key": "obj-secret-three"},
	}
	rendered, err := db.RenderPlaybook(ctx, ns, p.slug, values)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	r := pbInstall(t, db, ns, p.slug, values)
	if r.Values["secrets"] != playbook.MaskedValue || r.Values["auth"] != playbook.MaskedValue {
		t.Fatalf("整数组/整对象的 writeOnly 应整体打码：%v", r.Values)
	}
	if r.ValuesDigest != rendered.ValuesDigest {
		t.Fatalf("digest 应对打码前 values 算：%s vs %s", r.ValuesDigest, rendered.ValuesDigest)
	}
	var rawValues, rawReceipt string
	if err := db.QueryRow(`SELECT "values", receipt FROM playbook_installs WHERE id = ?`,
		r.Install.ID).Scan(&rawValues, &rawReceipt); err != nil {
		t.Fatalf("read stored receipt: %v", err)
	}
	for _, secret := range []string{"arr-secret-one", "arr-secret-two", "obj-secret-three"} {
		if strings.Contains(rawValues, secret) || strings.Contains(rawReceipt, secret) {
			t.Fatalf("复合 writeOnly 值泄露到库里：%s", secret)
		}
	}
}

// TestInstallCombinedConflictsKeepDetails 验能力 + 源同时冲突时，conflicts 明细两条都在
// （capability 带 digest、source 带 fields），且整包 0 写。
func TestInstallCombinedConflictsKeepDetails(t *testing.T) {
	db := openTestDB(t)
	ns := newNS(t, db, "pb-combined-conflict")

	p := newPBPackage("project-chain")
	p.caps = []string{"capabilities/review.yaml"}
	p.srcs = []string{"sources/on-push.yaml"}
	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\ntrue",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "src_token_a")
	row := pbCreate(t, db, ns, p)
	pbInstall(t, db, ns, p.slug, pbValues("a"))

	p.files["capabilities/review.yaml"] = pbCapDoc(pbCapOpts{
		name: "explicit-cap", version: 7, next: []string{}, runBody: "#!/usr/bin/env bash\necho changed",
	})
	p.files["sources/on-push.yaml"] = pbSourceDoc([]string{"{{values.project}}-review"}, "src_token_b")
	pbReplace(t, db, ns, p, row.ResourceVersion)

	before := pbTableCounts(t, db)
	_, err := pbInstallErr(t, db, ns, p.slug, pbValues("a"), false)
	if playbook.Code(err) != playbook.CodePlaybookConflict {
		t.Fatalf("组合冲突应 playbook_conflict，得到 %v", err)
	}
	var pe *playbook.Error
	if !errors.As(err, &pe) {
		t.Fatalf("错误应是 *playbook.Error：%T", err)
	}
	conflicts, ok := pe.Details["conflicts"].([]playbook.Conflict)
	if !ok || len(conflicts) != 2 {
		t.Fatalf("两条冲突明细都必须保留：%#v", pe.Details["conflicts"])
	}
	var sawCap, sawSrc bool
	for _, c := range conflicts {
		switch c.Kind {
		case "capability":
			sawCap = true
			if c.Ref != "explicit-cap@7" || c.DeclaredDigest == "" || c.ActualDigest == "" {
				t.Fatalf("能力冲突明细缺 digest：%+v", c)
			}
		case "source":
			sawSrc = true
			if !reflect.DeepEqual(c.Fields, []string{"config.token"}) {
				t.Fatalf("源冲突应点名 config.token：%+v", c)
			}
		}
	}
	if !sawCap || !sawSrc {
		t.Fatalf("冲突明细缺类：%+v", conflicts)
	}
	pbRequireCounts(t, before, pbTableCounts(t, db), "组合冲突")
}
