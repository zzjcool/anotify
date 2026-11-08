package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zzjcool/anotify/internal/artifact"
)

// ===== 夹具 =====

// mustCreatePlaybook 建一个预案，失败即 Fatal。
func mustCreatePlaybook(t *testing.T, db *DB, namespaceID, slug, manifest string) *Playbook {
	t.Helper()
	pb := &Playbook{
		NamespaceID: namespaceID,
		Slug:        slug,
		Name:        "预案 " + slug,
		Description: "测试用预案",
	}
	if manifest != "" {
		pb.Manifest = json.RawMessage(manifest)
	}
	if err := db.CreatePlaybook(context.Background(), pb); err != nil {
		t.Fatalf("create playbook %s: %v", slug, err)
	}
	return pb
}

// mustCap 手工建一个能力定义（模拟"安装前就存在的历史定义"）。
func mustCap(t *testing.T, db *DB, namespaceID, name string, version int, outputs ...string) *CapabilityVersion {
	t.Helper()
	c := &CapabilityVersion{
		NamespaceID:  namespaceID,
		Name:         name,
		Version:      version,
		ParamsSchema: json.RawMessage(`{"type":"object"}`),
		Outputs:      outputs,
	}
	if err := db.CreateCapability(context.Background(), withSpec(c)); err != nil {
		t.Fatalf("create capability %s@%d: %v", name, version, err)
	}
	return c
}

// mustNode 建一个节点并声明若干能力（默认 offline）。
func mustNode(t *testing.T, db *DB, namespaceID, name string, caps ...CapabilityRef) *Node {
	t.Helper()
	ctx := context.Background()
	n := &Node{NamespaceID: namespaceID, Name: name}
	if _, err := db.CreateNode(ctx, n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	if len(caps) > 0 {
		if _, err := db.ConfirmCapabilities(ctx, namespaceID, n.ID, caps); err != nil {
			t.Fatalf("confirm capabilities for %s: %v", name, err)
		}
	}
	return n
}

// mustChecks 算一次体检，失败即 Fatal。
func mustChecks(t *testing.T, db *DB, pb *Playbook) *PlaybookChecks {
	t.Helper()
	checks, err := db.PlaybookChecks(context.Background(), pb)
	if err != nil {
		t.Fatalf("playbook checks: %v", err)
	}
	return checks
}

// itemOf 取体检里某类项的第 n 项（n 从 0 起）。
func itemOf(t *testing.T, checks *PlaybookChecks, kind string, n int) PlaybookCheckItem {
	t.Helper()
	var seen int
	for _, it := range checks.Items {
		if it.Kind != kind {
			continue
		}
		if seen == n {
			return it
		}
		seen++
	}
	t.Fatalf("体检里没有第 %d 个 %s 项（items=%+v）", n, kind, checks.Items)
	return PlaybookCheckItem{}
}

// sourceByName 从命名空间的源列表里按名字取一个，失败即 Fatal。
func sourceByName(t *testing.T, db *DB, namespaceID, name string) *Source {
	t.Helper()
	sources, err := db.ListSources(context.Background(), namespaceID)
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	for _, s := range sources {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("找不到源 %q（现有 %d 个）", name, len(sources))
	return nil
}

// ===== 1. manifest 往返一致性（D1） =====

// TestPlaybookManifestRoundTrip 断言 manifest **存原文**：runner（含 env）、
// paramsSchema、自定义键都逐字节回来。重编码会丢掉键序与自定义键，
// 前端回填表单时就会看到"我没写的东西"或"我写的东西不见了"。
func TestPlaybookManifestRoundTrip(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	raw := `{
  "capabilities": [
    {
      "name": "analyze-pr",
      "version": 2,
      "outputs": ["needs-fix", "approved"],
      "paramsSchema": {"type": "object", "required": ["repo"], "properties": {"repo": {"type": "string"}}},
      "runner": {"runner": "bash", "command": "./runners/analyze-pr.sh", "timeoutSec": 900, "concurrency": 2, "env": {"GITHUB_TOKEN": "tkn", "LANG": "zh_CN.UTF-8"}}
    }
  ],
  "edges": [{"from": "analyze-pr", "fromPort": "needs-fix", "to": "lint-fix"}],
  "triggers": [{"kind": "webhook", "types": ["pull_request.opened"]}],
  "x-note": {"owner": "team-a", "why": "自定义键中枢不解释"}
}`
	pb := mustCreatePlaybook(t, db, nsID, "pr-review", raw)

	got, err := db.GetPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("get playbook by slug: %v", err)
	}
	if string(got.Manifest) != raw {
		t.Fatalf("manifest 应逐字节往返：\n want=%s\n got =%s", raw, string(got.Manifest))
	}

	// 结构化断言（避免"碰巧整块字符串一样但解析丢了字段"）
	var probe struct {
		Capabilities []struct {
			Name         string          `json:"name"`
			ParamsSchema json.RawMessage `json:"paramsSchema"`
			Runner       struct {
				Runner      string            `json:"runner"`
				Command     string            `json:"command"`
				TimeoutSec  int               `json:"timeoutSec"`
				Concurrency int               `json:"concurrency"`
				Env         map[string]string `json:"env"`
			} `json:"runner"`
		} `json:"capabilities"`
		XNote map[string]string `json:"x-note"`
	}
	if err := json.Unmarshal(got.Manifest, &probe); err != nil {
		t.Fatalf("解析回来的 manifest 失败: %v", err)
	}
	cap0 := probe.Capabilities[0]
	if cap0.Runner.Runner != "bash" || cap0.Runner.Command != "./runners/analyze-pr.sh" ||
		cap0.Runner.TimeoutSec != 900 || cap0.Runner.Concurrency != 2 ||
		cap0.Runner.Env["GITHUB_TOKEN"] != "tkn" || cap0.Runner.Env["LANG"] != "zh_CN.UTF-8" {
		t.Fatalf("runner 字段没有原样回来：%+v", cap0.Runner)
	}
	if string(cap0.ParamsSchema) != `{"type": "object", "required": ["repo"], "properties": {"repo": {"type": "string"}}}` {
		t.Fatalf("paramsSchema 没有原样回来：%s", string(cap0.ParamsSchema))
	}
	if probe.XNote["owner"] != "team-a" {
		t.Fatalf("自定义键 x-note 丢了：%+v", probe.XNote)
	}
}

// ===== 2. CRUD + 命名空间隔离 + 删预案不删已装物（D14 / I11） =====

func TestPlaybookCreateGetListDelete(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 空列表是 []（不是 null）
	if got, err := db.ListPlaybooks(ctx, nsID); err != nil {
		t.Fatalf("list empty playbooks: %v", err)
	} else if got == nil || len(got) != 0 {
		t.Fatalf("空列表应为 []，got %#v", got)
	}

	manifest := `{"capabilities":[{"name":"archive-logs","version":1,"outputs":["archived"]}],"edges":[],"triggers":[{"kind":"manual"}]}`
	pb := mustCreatePlaybook(t, db, nsID, "nightly-audit", manifest)

	// 按 id、按 slug 都能取
	byID, err := db.GetPlaybook(ctx, nsID, pb.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	bySlug, err := db.GetPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("get by slug: %v", err)
	}
	if byID.ID != bySlug.ID || byID.Slug != "nightly-audit" || byID.Name != "预案 nightly-audit" {
		t.Fatalf("按 id / slug 取到的不一致：%+v vs %+v", byID, bySlug)
	}
	// 每个预案都归属一个命名空间（#205 之后这是唯一的隔离维度）
	if byID.NamespaceID != nsID {
		t.Fatalf("namespaceId 应等于创建时的命名空间：want %s got %s", nsID, byID.NamespaceID)
	}
	if !strings.HasPrefix(byID.ID, "pb_") {
		t.Fatalf("预案 id 应是 pb_ 前缀，got %s", byID.ID)
	}

	// 另一个命名空间取不到（I11）
	other, err := db.CreateNamespace(ctx, "staging", "预发")
	if err != nil {
		t.Fatalf("create second namespace: %v", err)
	}
	if _, err := db.GetPlaybook(ctx, other.ID, pb.ID); !errors.Is(err, ErrPlaybookNotFound) {
		t.Fatalf("别的命名空间按 id 取应 404，got %v", err)
	}
	if _, err := db.GetPlaybook(ctx, other.ID, pb.Slug); !errors.Is(err, ErrPlaybookNotFound) {
		t.Fatalf("别的命名空间按 slug 取应 404，got %v", err)
	}
	if got, err := db.ListPlaybooks(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("另一个命名空间的列表应为空，got %+v err=%v", got, err)
	}
	// 每个命名空间各建一份同名 slug 是合法的（唯一键带 namespace_id）
	otherPB := mustCreatePlaybook(t, db, other.ID, "nightly-audit", `{"capabilities":[],"edges":[],"triggers":[]}`)
	if otherPB.ID == pb.ID {
		t.Fatal("两个命名空间的预案 id 不该相同")
	}

	// 装一遍，再删预案：已装的能力 / 边 / 源必须还在（D14）
	pb2 := mustCreatePlaybook(t, db, nsID, "pr-review", `{
		"capabilities":[
			{"name":"analyze-pr","version":1,"outputs":["approved"]},
			{"name":"lint-fix","version":1,"outputs":["clean"]}
		],
		"edges":[{"from":"analyze-pr","fromPort":"approved","to":"lint-fix"}],
		"triggers":[{"kind":"manual"}]
	}`)
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb2.Slug); err != nil {
		t.Fatalf("install before delete: %v", err)
	}
	capsBefore := countRows(t, db, "capability_versions", "namespace_id = ?", nsID)
	edgesBefore := countRows(t, db, "edges", "namespace_id = ?", nsID)
	sourcesBefore := countRows(t, db, "sources", "namespace_id = ?", nsID)
	if capsBefore == 0 || sourcesBefore == 0 || edgesBefore == 0 {
		t.Fatalf("前置条件不成立：安装后应有能力 / 边 / 源（caps=%d edges=%d sources=%d）",
			capsBefore, edgesBefore, sourcesBefore)
	}

	if err := db.DeletePlaybook(ctx, nsID, pb2.Slug); err != nil {
		t.Fatalf("delete playbook: %v", err)
	}
	if _, err := db.GetPlaybook(ctx, nsID, pb2.Slug); !errors.Is(err, ErrPlaybookNotFound) {
		t.Fatalf("删完应取不到，got %v", err)
	}
	if err := db.DeletePlaybook(ctx, nsID, pb2.Slug); !errors.Is(err, ErrPlaybookNotFound) {
		t.Fatalf("重复删除应报 ErrPlaybookNotFound，got %v", err)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n != capsBefore {
		t.Fatalf("删预案不该删已装能力：want %d got %d", capsBefore, n)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != edgesBefore {
		t.Fatalf("删预案不该删已装边：want %d got %d", edgesBefore, n)
	}
	if n := countRows(t, db, "sources", "namespace_id = ?", nsID); n != sourcesBefore {
		t.Fatalf("删预案不该删已装源：want %d got %d", sourcesBefore, n)
	}
	if _, err := db.GetCapability(ctx, nsID, "analyze-pr", 1); err != nil {
		t.Fatalf("删预案后能力定义应仍在: %v", err)
	}

	// 列表顺序按 created_at 升序（D5）
	list, err := db.ListPlaybooks(ctx, nsID)
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	if len(list) != 1 || list[0].ID != pb.ID {
		t.Fatalf("列表应只剩第一个预案，got %+v", list)
	}
}

// TestPlaybookEmptyManifestNormalized 断言空 manifest 归一成三条空数组（D13），
// 详情里的 items 也是 []（不是 null）。
func TestPlaybookEmptyManifestNormalized(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "empty-one", "")
	if string(pb.Manifest) != `{"capabilities":[],"edges":[],"triggers":[]}` {
		t.Fatalf("空 manifest 应归一成三条空数组，got %s", string(pb.Manifest))
	}
	detail, err := db.GetPlaybookDetail(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if detail.Items == nil || len(detail.Items) != 0 {
		t.Fatalf("无项时 items 应是 []，got %#v", detail.Items)
	}
	blob, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if !strings.Contains(string(blob), `"items":[]`) {
		t.Fatalf("详情 JSON 里 items 应是 []，got %s", blob)
	}
	if detail.Status != PlaybookStatusReady {
		t.Fatalf("空预案应该是 ready，got %s", detail.Status)
	}
	if detail.Next != nil {
		t.Fatalf("全 ok 时 next 应是 null，got %+v", detail.Next)
	}
}

// ===== 3. slug / name / manifest 校验（D3 / D12） =====

func TestPlaybookSlugValidation(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	mustCreatePlaybook(t, db, nsID, "pr-review", "")

	// 同命名空间重名 → ErrPlaybookSlugTaken（handler 翻 409）
	err := db.CreatePlaybook(ctx, &Playbook{NamespaceID: nsID, Slug: "pr-review", Name: "又一个"})
	if !errors.Is(err, ErrPlaybookSlugTaken) {
		t.Fatalf("slug 重名应报 ErrPlaybookSlugTaken，got %v", err)
	}

	// slug 非法 → ErrPlaybookSlugInvalid（不 trim：带空格也算非法）
	for _, bad := range []string{
		"",
		"PR-Review",  // 大写
		"pr_review",  // 下划线
		" pr-review", // 前导空格（不 trim）
		"pr-review ", // 尾随空格
		"-pr-review", // 前导连字符
		"pr-review-", // 尾随连字符
		"pr--review", // 连续连字符
		"pr review",  // 空格
		"pr/review",  // 斜杠
		strings.Repeat("a", PlaybookSlugMaxLen+1), // 超长
	} {
		err := db.CreatePlaybook(ctx, &Playbook{NamespaceID: nsID, Slug: bad, Name: "名字"})
		if !errors.Is(err, ErrPlaybookSlugInvalid) {
			t.Fatalf("slug=%q 应报 ErrPlaybookSlugInvalid，got %v", bad, err)
		}
	}
	// 上限边界：64 个字符是合法的
	longest := strings.Repeat("a", PlaybookSlugMaxLen)
	if err := db.CreatePlaybook(ctx, &Playbook{NamespaceID: nsID, Slug: longest, Name: "边界"}); err != nil {
		t.Fatalf("长度 %d 的 slug 应合法: %v", PlaybookSlugMaxLen, err)
	}

	// name 必填
	if err := db.CreatePlaybook(ctx, &Playbook{NamespaceID: nsID, Slug: "ok-slug", Name: "  "}); !errors.Is(err, ErrPlaybookNameRequired) {
		t.Fatalf("空 name 应报 ErrPlaybookNameRequired，got %v", err)
	}

	// manifest 非法（D12 逐条）
	badManifests := []struct{ what, manifest string }{
		{"capability 名为空", `{"capabilities":[{"name":"","version":1}]}`},
		{"保留名 github", `{"capabilities":[{"name":"github","version":1}]}`},
		{"版本 <= 0", `{"capabilities":[{"name":"a","version":0}]}`},
		{"同名同版本重复", `{"capabilities":[{"name":"a","version":1},{"name":"a","version":1}]}`},
		{"paramsSchema 不是对象", `{"capabilities":[{"name":"a","version":1,"paramsSchema":["x"]}]}`},
		{"边字段为空", `{"edges":[{"from":"a","fromPort":"","to":"b"}]}`},
		{"端口不在 from 的 outputs", `{"capabilities":[{"name":"a","version":1,"outputs":["x"]}],"edges":[{"from":"a","fromPort":"nope","to":"a"}]}`},
		{"trigger kind 非法", `{"triggers":[{"kind":"telepathy"}]}`},
		{"manifest 不是对象", `[]`},
	}
	for _, c := range badManifests {
		err := db.CreatePlaybook(ctx, &Playbook{
			NamespaceID: nsID, Slug: "bad-manifest", Name: "坏 manifest", Manifest: json.RawMessage(c.manifest),
		})
		if !errors.Is(err, ErrPlaybookManifestInvalid) {
			t.Fatalf("%s 应报 ErrPlaybookManifestInvalid，got %v（manifest=%s）", c.what, err, c.manifest)
		}
	}
	// 合法 manifest（含 null 项）能过
	if err := db.CreatePlaybook(ctx, &Playbook{
		NamespaceID: nsID, Slug: "good-manifest", Name: "好 manifest",
		Manifest: json.RawMessage(`{"capabilities":null,"edges":null,"triggers":null}`),
	}); err != nil {
		t.Fatalf("全 null 的 manifest 应归一成空数组: %v", err)
	}
}

// ===== 4. 幂等安装（D8 / D13） =====

// prReviewManifest 是三个能力 + 两条边 + 一个 webhook 触发的标准件。
// 入口能力是 analyze-pr（它不作为任何边的 to）。
func prReviewManifest() string {
	return `{
		"capabilities":[
			{"name":"analyze-pr","version":1,"outputs":["needs-fix","approved"]},
			{"name":"lint-fix","version":1,"outputs":["clean","remaining"]},
			{"name":"run-tests","version":1,"outputs":["passed","failed"]}
		],
		"edges":[
			{"from":"analyze-pr","fromPort":"needs-fix","to":"lint-fix"},
			{"from":"lint-fix","fromPort":"clean","to":"run-tests"}
		],
		"triggers":[{"kind":"webhook","types":["pull_request.opened","pull_request.synchronize"]}]
	}`
}

func TestInstallPlaybookIdempotent(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "pr-review", prReviewManifest())

	first, res1, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if first == nil || first.ID != pb.ID {
		t.Fatalf("安装应返回预案本身，got %+v", first)
	}
	wantCaps := []string{"analyze-pr@1", "lint-fix@1", "run-tests@1"}
	wantEdges := []string{"analyze-pr --needs-fix--> lint-fix", "lint-fix --clean--> run-tests"}
	wantSources := []string{"pr-review-webhook-0"}
	if fmt.Sprint(res1.Created.Capabilities) != fmt.Sprint(wantCaps) {
		t.Fatalf("created.capabilities want %v got %v", wantCaps, res1.Created.Capabilities)
	}
	if fmt.Sprint(res1.Created.Edges) != fmt.Sprint(wantEdges) {
		t.Fatalf("created.edges want %v got %v", wantEdges, res1.Created.Edges)
	}
	if fmt.Sprint(res1.Created.Sources) != fmt.Sprint(wantSources) {
		t.Fatalf("created.sources want %v got %v", wantSources, res1.Created.Sources)
	}
	if len(res1.Skipped.Capabilities) != 0 || len(res1.Skipped.Edges) != 0 || len(res1.Skipped.Sources) != 0 {
		t.Fatalf("第一次安装不该有 skipped：%+v", res1.Skipped)
	}

	capsAfterFirst := countRows(t, db, "capability_versions", "namespace_id = ?", nsID)
	edgesAfterFirst := countRows(t, db, "edges", "namespace_id = ?", nsID)
	sourcesAfterFirst := countRows(t, db, "sources", "namespace_id = ?", nsID)

	// 第二次：created 全空、skipped 全满、行数不变
	_, res2, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(res2.Created.Capabilities) != 0 || len(res2.Created.Edges) != 0 || len(res2.Created.Sources) != 0 {
		t.Fatalf("第二次安装的 created 应全空：%+v", res2.Created)
	}
	if fmt.Sprint(res2.Skipped.Capabilities) != fmt.Sprint(wantCaps) {
		t.Fatalf("skipped.capabilities want %v got %v", wantCaps, res2.Skipped.Capabilities)
	}
	if fmt.Sprint(res2.Skipped.Edges) != fmt.Sprint(wantEdges) {
		t.Fatalf("skipped.edges want %v got %v", wantEdges, res2.Skipped.Edges)
	}
	if fmt.Sprint(res2.Skipped.Sources) != fmt.Sprint(wantSources) {
		t.Fatalf("skipped.sources want %v got %v", wantSources, res2.Skipped.Sources)
	}
	if len(res2.Notes) != 0 {
		t.Fatalf("纯粹的「早装过了」不该产出 note：%v", res2.Notes)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n != capsAfterFirst {
		t.Fatalf("能力行数变了：want %d got %d", capsAfterFirst, n)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != edgesAfterFirst {
		t.Fatalf("边行数变了：want %d got %d", edgesAfterFirst, n)
	}
	if n := countRows(t, db, "sources", "namespace_id = ?", nsID); n != sourcesAfterFirst {
		t.Fatalf("源行数变了：want %d got %d", sourcesAfterFirst, n)
	}

	// D13：空集合是 [] 而不是 null
	blob, err := json.Marshal(struct {
		Created PlaybookInstallSet `json:"created"`
		Skipped PlaybookInstallSet `json:"skipped"`
		Notes   []string           `json:"notes"`
	}{res2.Created, res2.Skipped, res2.Notes})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	s := string(blob)
	for _, want := range []string{`"capabilities":[]`, `"edges":[]`, `"sources":[]`, `"notes":[]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("安装结果 JSON 缺 %s：%s", want, s)
		}
	}
}

// ===== 5. 边端点缺定义 → 该边 skipped，其余照装（D10 / C4） =====

// notesContain 报告 notes 里有没有一条包含 sub 的说明。
//
// notes 是一份**给人读的清单**，条目数会随「装了什么、跳过了什么」变化，所以断言
// 只钉「该说的事情说了」，不钉「一共几条」——钉条数会让每次加一条说明都变成返工。
func notesContain(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestInstallPlaybookSkipsEdgeWithMissingEndpoint(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "half-wired", `{
		"capabilities":[
			{"name":"entry","version":1,"outputs":["go"]},
			{"name":"leaf","version":1,"outputs":[]}
		],
		"edges":[
			{"from":"entry","fromPort":"go","to":"leaf"},
			{"from":"entry","fromPort":"go","to":"ghost"}
		],
		"triggers":[{"kind":"manual"}]
	}`)

	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// 两条边：一条装上了，一条进 skipped（不整体回滚）
	if fmt.Sprint(res.Created.Edges) != fmt.Sprint([]string{"entry --go--> leaf"}) {
		t.Fatalf("created.edges want [entry --go--> leaf]，got %v", res.Created.Edges)
	}
	if fmt.Sprint(res.Skipped.Edges) != fmt.Sprint([]string{"entry --go--> ghost"}) {
		t.Fatalf("skipped.edges want [entry --go--> ghost]，got %v", res.Skipped.Edges)
	}
	if len(res.Created.Capabilities) != 2 {
		t.Fatalf("能力应照装（2 个），got %v", res.Created.Capabilities)
	}
	if len(res.Created.Sources) != 1 {
		t.Fatalf("源应照建（1 个），got %v", res.Created.Sources)
	}
	if !notesContain(res.Notes, "ghost") {
		t.Fatalf("skipped 的边应有一条说明且指出缺的能力，got %v", res.Notes)
	}
	// P1-b1 起装出来的能力一律是 noop 占位，notes 里必须说清楚（供人工补实现）。
	if !notesContain(res.Notes, "noop 占位") {
		t.Fatalf("装成占位的能力应在 notes 里说明，got %v", res.Notes)
	}
	// 能力边只有装上来的那 1 条；**源的目标也是边**（P2.1：建源 = 建入口边），
	// 所以这里按边类型分开数，而不是把入口边也算进「这条 manifest 装了几条 link」。
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_is_source = 0", nsID); n != 1 {
		t.Fatalf("库里应只有 1 条能力边，got %d", n)
	}
	if n := countRows(t, db, "edges", "namespace_id = ? AND from_is_source = 1", nsID); n != 1 {
		t.Fatalf("预案建的源应带 1 条入口边，got %d", n)
	}
}

// ===== 6. fromPort ∉ outputs → 该边 skipped + note（I2 / D10） =====

func TestInstallPlaybookSkipsEdgeWithUnknownPort(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 两端都是**安装前就存在**的定义，这样 manifest 校验不会提前拦下（配置期
	// 只校验 manifest 自己声明过的能力；历史定义只有安装期才知道出口是什么）。
	mustCap(t, db, nsID, "analyze-pr", 1, "approved")
	mustCap(t, db, nsID, "run-tests", 1, "passed")

	pb := mustCreatePlaybook(t, db, nsID, "port-typo", `{
		"capabilities":[],
		"edges":[{"from":"analyze-pr","fromPort":"needs-fix","to":"run-tests"}]
	}`)

	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("端口不在 outputs 不应让安装失败: %v", err)
	}
	if len(res.Created.Edges) != 0 {
		t.Fatalf("不该装上这条边，got %v", res.Created.Edges)
	}
	if fmt.Sprint(res.Skipped.Edges) != fmt.Sprint([]string{"analyze-pr --needs-fix--> run-tests"}) {
		t.Fatalf("该边应进 skipped.edges，got %v", res.Skipped.Edges)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "needs-fix") || !strings.Contains(res.Notes[0], "outputs") {
		t.Fatalf("note 应说清端口不在 outputs 里，got %v", res.Notes)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != 0 {
		t.Fatalf("库里不该有边，got %d", n)
	}
}

// ===== 7. webhook 源的 token 真能用（D11） =====

func TestInstallPlaybookWebhookSourceIsUsable(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "pr-review", prReviewManifest())
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}

	src := sourceByName(t, db, nsID, "pr-review-webhook-0")
	if src.Kind != SourceKindWebhook {
		t.Fatalf("源 kind want webhook got %s", src.Kind)
	}
	if !src.Enabled {
		t.Fatal("装出来的源应默认启用")
	}
	var cfg WebhookConfig
	if err := json.Unmarshal(src.Config, &cfg); err != nil {
		t.Fatalf("解析 webhook 配置: %v", err)
	}
	if cfg.Token == "" {
		t.Fatal("webhook 源必须有服务端生成的 token")
	}

	// token 能查到源（POST /v1/hooks/{token} 的入口）
	byToken, err := db.GetSourceByWebhookToken(ctx, cfg.Token)
	if err != nil {
		t.Fatalf("按 token 取源失败（token 不可用）: %v", err)
	}
	if byToken.ID != src.ID {
		t.Fatalf("按 token 取到的源不是它：want %s got %s", src.ID, byToken.ID)
	}

	// targets 指向**入口能力**（analyze-pr）——它不作为任何边的 to
	entry, err := db.GetCapability(ctx, nsID, "analyze-pr", 1)
	if err != nil {
		t.Fatalf("get entry capability: %v", err)
	}
	if len(byToken.Targets) != 1 || byToken.Targets[0].CapabilityID != entry.ID {
		t.Fatalf("源的出口应只有入口能力 %s，got %+v", entry.ID, byToken.Targets)
	}
}

// TestInstallPlaybookCronSource 覆盖 D11 的 cron 分支：types[0] 原样进 config.expr，
// 并算好 next_fire_at；没给表达式则 skip + note（不凭空造一个"每天 3 点"）。
func TestInstallPlaybookCronSource(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "nightly-audit", `{
		"capabilities":[{"name":"archive-logs","version":1,"outputs":["archived"]}],
		"edges":[],
		"triggers":[{"kind":"cron","types":["0 3 * * *"]}]
	}`)
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}
	src := sourceByName(t, db, nsID, "nightly-audit-cron-0")
	if src.Kind != SourceKindCron {
		t.Fatalf("kind want cron got %s", src.Kind)
	}
	var cfg CronConfig
	if err := json.Unmarshal(src.Config, &cfg); err != nil {
		t.Fatalf("解析 cron 配置: %v", err)
	}
	if cfg.Expr != "0 3 * * *" {
		t.Fatalf("config.expr 应原样是 types[0]，got %q", cfg.Expr)
	}
	if cfg.TZ != "Local" {
		t.Fatalf("缺省时区应补 Local，got %q", cfg.TZ)
	}
	if src.NextFireAt == nil || *src.NextFireAt <= Now() {
		t.Fatalf("cron 源应算好下次触发时刻，got %v", src.NextFireAt)
	}

	// 没给表达式的 cron：skip + note，不失败
	bad := mustCreatePlaybook(t, db, nsID, "no-expr", `{
		"capabilities":[{"name":"lone","version":1,"outputs":[]}],
		"triggers":[{"kind":"cron"}]
	}`)
	_, res, err := db.InstallPlaybook(ctx, nsID, bad.Slug)
	if err != nil {
		t.Fatalf("缺 cron 表达式不该让安装失败: %v", err)
	}
	if fmt.Sprint(res.Skipped.Sources) != fmt.Sprint([]string{"no-expr-cron-0"}) {
		t.Fatalf("该触发应进 skipped.sources，got %v", res.Skipped.Sources)
	}
	if !notesContain(res.Notes, "cron") {
		t.Fatalf("note 应说清是 cron 缺表达式，got %v", res.Notes)
	}
}

// ===== 8. 体检：no-node / offline / ok 分开，status 随之变化（D6） =====

func TestPlaybookChecksNodePresence(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "sandbox-only", `{
		"capabilities":[{"name":"sandbox","version":1,"outputs":["done"]}]
	}`)

	// ① 定义都没装 → missing → blocked
	checks := mustChecks(t, db, pb)
	if checks.Status != PlaybookStatusBlocked {
		t.Fatalf("定义缺失时 want blocked got %s", checks.Status)
	}
	item := itemOf(t, checks, PlaybookItemCapability, 0)
	if item.Status != PlaybookCheckMissing || item.Ref != "sandbox@1" {
		t.Fatalf("能力项 want missing/sandbox@1，got %+v", item)
	}
	if checks.Next == nil || checks.Next.Kind != PlaybookItemCapability || checks.Next.Status != PlaybookCheckMissing {
		t.Fatalf("next 应是第一个非 ok 项，got %+v", checks.Next)
	}
	if checks.Counts != (PlaybookCounts{Capabilities: 1}) || checks.Total != 1 || checks.Ready != 0 {
		t.Fatalf("counts/total/ready 不对：%+v total=%d ready=%d", checks.Counts, checks.Total, checks.Ready)
	}

	// ② 装上定义但没有机器声明 → no-node → needs-node
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}
	checks = mustChecks(t, db, pb)
	if checks.Status != PlaybookStatusNeedsNode {
		t.Fatalf("有定义没节点时 want needs-node got %s", checks.Status)
	}
	item = itemOf(t, checks, PlaybookItemCapability, 0)
	if item.Status != PlaybookCheckNoNode {
		t.Fatalf("有定义没节点时能力项 want no-node，got %s", item.Status)
	}
	if item.CapID == "" || len(item.Nodes) != 0 {
		t.Fatalf("no-node 项应带 capId 且没有 nodes，got %+v", item)
	}

	// ③ 有机器声明但没上线 → offline（与 no-node 必须分开）
	node := mustNode(t, db, nsID, "mbp-1", CapabilityRef{Name: "sandbox", Version: 1})
	checks = mustChecks(t, db, pb)
	item = itemOf(t, checks, PlaybookItemCapability, 0)
	if item.Status != PlaybookCheckOffline {
		t.Fatalf("有声明但离线 want offline，got %s", item.Status)
	}
	if len(item.Nodes) != 1 || item.Nodes[0].Status != NodeStatusOffline {
		t.Fatalf("offline 项应列出声明它的机器，got %+v", item.Nodes)
	}
	if checks.Status != PlaybookStatusNeedsNode {
		t.Fatalf("offline 时的总状态仍应是 needs-node，got %s", checks.Status)
	}

	// ④ 机器上线 → ok → ready，next 变 null
	if err := db.UpdateNodeStatus(ctx, nsID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("update node status: %v", err)
	}
	checks = mustChecks(t, db, pb)
	item = itemOf(t, checks, PlaybookItemCapability, 0)
	if item.Status != PlaybookCheckOK {
		t.Fatalf("机器上线后 want ok，got %s", item.Status)
	}
	if len(item.Nodes) != 1 || item.Nodes[0].Name != "mbp-1" || item.Nodes[0].Status != NodeStatusOnline {
		t.Fatalf("ok 项应列出在线机器，got %+v", item.Nodes)
	}
	if checks.Status != PlaybookStatusReady {
		t.Fatalf("全 ok 时 want ready got %s", checks.Status)
	}
	if checks.Next != nil {
		t.Fatalf("全 ok 时 next 应是 null，got %+v", checks.Next)
	}
	if checks.Ready != 1 || checks.Total != 1 {
		t.Fatalf("ready/total want 1/1，got %d/%d", checks.Ready, checks.Total)
	}

	// ⑤ 详情视图与体检一致
	detail, err := db.GetPlaybookDetail(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	if detail.Status != PlaybookStatusReady || len(detail.Items) != 1 {
		t.Fatalf("详情应与体检一致：%+v", detail.PlaybookView)
	}
	views, err := db.ListPlaybookViews(ctx, nsID)
	if err != nil {
		t.Fatalf("list views: %v", err)
	}
	if len(views) != 1 || views[0].Status != PlaybookStatusReady || views[0].ID != pb.ID {
		t.Fatalf("列表视图不对：%+v", views)
	}
}

// ===== 9. 体检：边 / 触发缺失 → partial；触发必须命中入口能力（D6 ③） =====

func TestPlaybookChecksPartialAndEntryCapabilities(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "wired", prReviewManifest())
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}
	// 三个能力要有机器才可能 ready
	node := mustNode(t, db, nsID, "mbp-1",
		CapabilityRef{Name: "analyze-pr", Version: 1},
		CapabilityRef{Name: "lint-fix", Version: 1},
		CapabilityRef{Name: "run-tests", Version: 1},
	)
	if err := db.UpdateNodeStatus(ctx, nsID, node.ID, NodeStatusOnline); err != nil {
		t.Fatalf("节点上线: %v", err)
	}

	checks := mustChecks(t, db, pb)
	if checks.Status != PlaybookStatusReady {
		t.Fatalf("装完并上线后应 ready，got %s（items=%+v）", checks.Status, checks.Items)
	}
	if checks.Counts != (PlaybookCounts{Capabilities: 3, Edges: 2, Triggers: 1}) {
		t.Fatalf("counts 应是三类的项数，got %+v", checks.Counts)
	}
	if checks.Total != 6 || checks.Ready != 6 {
		t.Fatalf("total/ready want 6/6，got %d/%d", checks.Total, checks.Ready)
	}

	// 删掉一条边 → 那条边 missing → partial
	fromCap, err := db.GetCapability(ctx, nsID, "analyze-pr", 1)
	if err != nil {
		t.Fatalf("get from cap: %v", err)
	}
	toCap, err := db.GetCapability(ctx, nsID, "lint-fix", 1)
	if err != nil {
		t.Fatalf("get to cap: %v", err)
	}
	if err := db.DeleteEdge(ctx, nsID, false, fromCap.ID, "needs-fix", toCap.ID); err != nil {
		t.Fatalf("delete edge: %v", err)
	}
	checks = mustChecks(t, db, pb)
	if checks.Status != PlaybookStatusPartial {
		t.Fatalf("缺一条边时 want partial got %s", checks.Status)
	}
	edgeItem := itemOf(t, checks, PlaybookItemEdge, 0)
	if edgeItem.Status != PlaybookCheckMissing || edgeItem.FromID != fromCap.ID || edgeItem.ToID != toCap.ID {
		t.Fatalf("边项应是 missing 且带 fromId/toId，got %+v", edgeItem)
	}
	if edgeItem.Label != "analyze-pr --needs-fix--> lint-fix" {
		t.Fatalf("边的 label 形状不对：%q", edgeItem.Label)
	}

	// 换成一个指向**无关能力**的同 kind 源：触发器不该算 ok
	unrelated := mustCap(t, db, nsID, "unrelated", 1, "done")
	ourSource := sourceByName(t, db, nsID, "wired-webhook-0")
	if err := db.DeleteSource(ctx, nsID, ourSource.ID); err != nil {
		t.Fatalf("delete source: %v", err)
	}
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID,
		Name:        "somebody-elses-hook",
		Kind:        SourceKindWebhook,
		Enabled:     true,
		Config:      json.RawMessage(`{}`),
		Targets:     []SourceTarget{{CapabilityID: unrelated.ID}},
	}); err != nil {
		t.Fatalf("create unrelated source: %v", err)
	}
	checks = mustChecks(t, db, pb)
	trigger := itemOf(t, checks, PlaybookItemTrigger, 0)
	if trigger.Status != PlaybookCheckMissing {
		t.Fatalf("指向无关能力的同 kind 源不该让触发器算 ok，got %s", trigger.Status)
	}
	if trigger.DeclaredKind != SourceKindWebhook {
		t.Fatalf("declaredKind want webhook got %s", trigger.DeclaredKind)
	}
	if fmt.Sprint(trigger.EntryCaps) != fmt.Sprint([]string{"analyze-pr"}) {
		t.Fatalf("entryCaps 应只有入口能力 analyze-pr，got %v", trigger.EntryCaps)
	}
	if trigger.Label != "webhook · pull_request.opened / pull_request.synchronize" {
		t.Fatalf("触发器的 label 形状不对：%q", trigger.Label)
	}
	if checks.Next == nil || checks.Next.Kind != PlaybookItemEdge {
		t.Fatalf("next 应是第一个没过的项（这里是边），got %+v", checks.Next)
	}

	// 再装一次：缺的边与源都补回来（新源不撞「同 kind 已指向别人」的判据）
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if fmt.Sprint(res.Created.Edges) != fmt.Sprint([]string{"analyze-pr --needs-fix--> lint-fix"}) {
		t.Fatalf("缺的边应被补上，got %v", res.Created.Edges)
	}
	if fmt.Sprint(res.Created.Sources) != fmt.Sprint([]string{"wired-webhook-0"}) {
		t.Fatalf("缺的源应被补上，got %v", res.Created.Sources)
	}
	// 别人那个同 kind 源仍在（不覆盖、不删别人的东西）
	if n := countRows(t, db, "sources", "namespace_id = ?", nsID); n != 2 {
		t.Fatalf("库里应有 2 个源（别人那个 + 新装的），got %d", n)
	}
	if checks = mustChecks(t, db, pb); checks.Status != PlaybookStatusReady {
		t.Fatalf("补装后应 ready，got %s（items=%+v）", checks.Status, checks.Items)
	}

	// blockedByCap（M2）：端点缺定义时这一行要**点名缺的是哪个能力**（前端直接上屏）。
	// 两条边分开覆盖：to 端缺 / from 端缺（后者优先，因为 from==nil 时不能再报 to）。
	ghost := mustCreatePlaybook(t, db, nsID, "ghost-wired", `{
		"capabilities":[{"name":"head","version":1,"outputs":["go"]}],
		"edges":[
			{"from":"head","fromPort":"go","to":"nowhere"},
			{"from":"missing-upstream","fromPort":"go","to":"head"}
		],
		"triggers":[]
	}`)
	// 先装一遍让 head@1 有定义，这样第一条边的 from 能解析、只有 to 缺
	if _, _, err := db.InstallPlaybook(ctx, nsID, ghost.Slug); err != nil {
		t.Fatalf("install ghost-wired: %v", err)
	}
	ghostChecks := mustChecks(t, db, ghost)
	if item := itemOf(t, ghostChecks, PlaybookItemEdge, 0); item.BlockedByCap != "nowhere" {
		t.Fatalf("to 端缺定义时 blockedByCap 应是 nowhere，got %q（item=%+v）", item.BlockedByCap, item)
	}
	if item := itemOf(t, ghostChecks, PlaybookItemEdge, 1); item.BlockedByCap != "missing-upstream" {
		t.Fatalf("from 端缺定义时 blockedByCap 应是 missing-upstream，got %q（item=%+v）", item.BlockedByCap, item)
	}
}

// ===== 10. I8：已存在的定义绝不被覆盖（D9 的两种冲突） =====

func TestInstallPlaybookDoesNotOverwriteExistingDefinition(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// ① 同名同版本、**内容不同**（手工改过 outputs）→ skipped，库里仍是旧定义
	old := mustCap(t, db, nsID, "x", 1, "old-port")
	pb := mustCreatePlaybook(t, db, nsID, "conflict", `{
		"capabilities":[{"name":"x","version":1,"outputs":["new-port"]}]
	}`)
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("定义冲突不该让安装失败: %v", err)
	}
	if len(res.Created.Capabilities) != 0 {
		t.Fatalf("已有定义不该被重装，got %v", res.Created.Capabilities)
	}
	if fmt.Sprint(res.Skipped.Capabilities) != fmt.Sprint([]string{"x@1"}) {
		t.Fatalf("冲突的能力应进 skipped，got %v", res.Skipped.Capabilities)
	}
	got, err := db.GetCapability(ctx, nsID, "x", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if got.ID != old.ID || fmt.Sprint(got.Outputs) != fmt.Sprint([]string{"old-port"}) {
		t.Fatalf("库里应仍是旧定义（id=%s outputs=%v），got %+v", old.ID, old.Outputs, got)
	}

	// ② 同 schema、**不同版本**（x@2 与 x@1 的 outputs/paramsSchema 一样）→ 在 v3 里
	// 这是**另一份内容**：digest 吃 metadata.version（契约 §3.2），所以它照装，且
	// 不覆盖 x@1。跨版本内容折叠（旧 D9）随 content_hash ≡ digest 一起取消——
	// 「同内容」只可能是同 name@version 的重放，那种情况在 ① 就是 skipped。
	same := mustCreatePlaybook(t, db, nsID, "same-schema", `{
		"capabilities":[{"name":"x","version":2,"outputs":["old-port"],"paramsSchema":{"type":"object"}}]
	}`)
	_, res, err = db.InstallPlaybook(ctx, nsID, same.Slug)
	if err != nil {
		t.Fatalf("同 schema 的另一版本不该让安装失败: %v", err)
	}
	if fmt.Sprint(res.Created.Capabilities) != fmt.Sprint([]string{"x@2"}) {
		t.Fatalf("x@2 是另一份内容，应照装，got %v", res.Created.Capabilities)
	}
	if len(res.Skipped.Capabilities) != 0 {
		t.Fatalf("不该有 skipped，got %v", res.Skipped.Capabilities)
	}
	// 装出来的是 noop 占位（manifest 的 runner 不解释成实现），notes 必须说明。
	if !notesContain(res.Notes, "x@2") || !notesContain(res.Notes, "noop 占位") {
		t.Fatalf("x@2 的占位说明缺失，got %v", res.Notes)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?", nsID, "x"); n != 2 {
		t.Fatalf("x 应有 @1 与 @2 两行，got %d", n)
	}
	// 旧行一个字节都不动（append-only，I8）。
	still, err := db.GetCapability(ctx, nsID, "x", 1)
	if err != nil {
		t.Fatalf("get x@1: %v", err)
	}
	if still.ID != old.ID || fmt.Sprint(still.Outputs) != fmt.Sprint([]string{"old-port"}) {
		t.Fatalf("x@1 被改动了: id=%s outputs=%v", still.ID, still.Outputs)
	}
}

// ===== 补充：拒绝内置来源保留名 + 空列表契约（I9） =====

func TestPlaybookManifestReservedOriginNames(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	for _, name := range []string{"github", "cron", "http", "manual"} {
		err := db.CreatePlaybook(ctx, &Playbook{
			NamespaceID: nsID, Slug: "reserved-name", Name: "保留名",
			Manifest: json.RawMessage(fmt.Sprintf(`{"capabilities":[{"name":%q,"version":1}]}`, name)),
		})
		if !errors.Is(err, ErrPlaybookManifestInvalid) {
			t.Fatalf("能力名 %q 是来源保留名，应被拒，got %v", name, err)
		}
	}
	// 普通名字（含点号）不受影响
	if _, err := ParsePlaybookManifest(json.RawMessage(`{"capabilities":[{"name":"report.send","version":1}]}`)); err != nil {
		t.Fatalf("report.send 应合法: %v", err)
	}
}

func TestPlaybookInstallEmptyManifest(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "nothing", "")
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("空预案安装不该失败: %v", err)
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(blob)
	for _, want := range []string{`"capabilities":[]`, `"edges":[]`, `"sources":[]`, `"notes":[]`} {
		if !strings.Contains(s, want) {
			t.Fatalf("空预案的安装结果 JSON 缺 %s：%s", want, s)
		}
	}
	// 取不存在的预案 → ErrPlaybookNotFound
	if _, _, err := db.InstallPlaybook(ctx, nsID, "nope"); !errors.Is(err, ErrPlaybookNotFound) {
		t.Fatalf("安装不存在的预案应报 ErrPlaybookNotFound，got %v", err)
	}
	// namespace 缺失 → ErrNamespaceRequired
	if _, _, err := db.InstallPlaybook(ctx, AllNamespaces, "nothing"); !errors.Is(err, ErrNamespaceRequired) {
		t.Fatalf("namespace=* 应报 ErrNamespaceRequired，got %v", err)
	}
}

// ===== 11. 同名的多个版本：体检与安装必须用同一个解析口径（D7） =====

// TestPlaybookChecksResolvesDeclaredVersion 钉住「体检与安装解析能力用的是同一条规则」。
//
// 库里同时有 x@1 与 x@2、manifest 声明 x@2 时：
//   - 安装把边连在 **x@2** 上（声明版本优先）
//   - 体检必须去看 x@2 的边，否则会得出「install 说 created、体检说 missing」
//     这种自相矛盾的结论（用户看到一条永远补不上的边）。
//
// 这条用例是集成期发现的真实缺口：修之前体检对边的解析一律取"版本号最小"，
// x@1 存在时就会去查 x@1 的边。
func TestPlaybookChecksResolvesDeclaredVersion(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 历史定义：x@1（端口 old-port，与预案声明的不一样）
	if old := mustCap(t, db, nsID, "x", 1, "old-port"); old.Version != 1 {
		t.Fatalf("seed x@1: %d", old.Version)
	}

	pb := mustCreatePlaybook(t, db, nsID, "multi-version", `{
		"capabilities":[
			{"name":"x","version":2,"outputs":["p"]},
			{"name":"y","version":1,"outputs":[]}
		],
		"edges":[{"from":"x","fromPort":"p","to":"y"}],
		"triggers":[]
	}`)

	if _, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	} else if fmt.Sprint(res.Created.Edges) != fmt.Sprint([]string{"x --p--> y"}) {
		t.Fatalf("边应连在 x@2 上并被创建，got created=%v skipped=%v", res.Created.Edges, res.Skipped.Edges)
	}

	x2, err := db.GetCapability(ctx, nsID, "x", 2)
	if err != nil {
		t.Fatalf("get x@2: %v", err)
	}
	y1, err := db.GetCapability(ctx, nsID, "y", 1)
	if err != nil {
		t.Fatalf("get y@1: %v", err)
	}
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	// 两端是**引用**（P2.2）：预案用的是声明版本，落库就是钉死的 `name@version`。
	if len(edges) != 1 || edges[0].FromRef != capRef(x2) || edges[0].ToRef != capRef(y1) {
		t.Fatalf("库里那条边的两端应是 %s / %s，got %+v", capRef(x2), capRef(y1), edges)
	}

	checks := mustChecks(t, db, pb)
	edgeItem := itemOf(t, checks, PlaybookItemEdge, 0)
	if edgeItem.Status != PlaybookCheckOK {
		t.Fatalf("装好的边体检必须是 ok（要按声明版本 x@2 找），got %s", edgeItem.Status)
	}
	if edgeItem.FromID != x2.ID {
		t.Fatalf("边项 fromId 应是 x@2（%s），got %s", x2.ID, edgeItem.FromID)
	}
	// 能力项仍然是精确定位到声明的版本
	if capX := itemOf(t, checks, PlaybookItemCapability, 0); capX.Ref != "x@2" {
		t.Fatalf("能力项 ref 应是 x@2，got %s", capX.Ref)
	}
}

// ===== 12. 同 schema 的**另一个版本**：v3 起它是一份新内容（D9 消解）=====

// TestInstallPlaybookNewVersionSameSchema 钉住「manifest 声明 x@2、库里已有 x@1（schema
// 一样）」这条链在 v3 下的形状。
//
// 口径变化（Capability Spec v3 §3.2）：digest 吃 metadata.version，所以「同 schema 换个
// 版本号」= 另一份内容，照装，不再折叠回旧行。旧 D9（同内容复用 + 死状态 M3）随之消解：
// 既然 x@2 一定建得出来，就不会再有「体检说 missing、而 I8 又不许补」的死状态。
//
// 仍然保留的语义（两条都要盯住）：
//   - x@1 一个字节都不动（append-only，I8）；
//   - 边是按**名字**引用的（D7），但 manifest 声明了版本 → 声明版本优先：装出来的
//     边钉在 x@2 上（不是「版本号最小」的 x@1）。
func TestInstallPlaybookNewVersionSameSchema(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 历史定义 x@1：与预案声明的 x@2 的 outputs / paramsSchema 一样。
	x1 := mustCap(t, db, nsID, "x", 1, "old-port")

	pb := mustCreatePlaybook(t, db, nsID, "new-version-same-schema", `{
		"capabilities":[
			{"name":"x","version":2,"outputs":["old-port"],"paramsSchema":{"type":"object"}},
			{"name":"y","version":1,"outputs":["go"]}
		],
		"edges":[{"from":"y","fromPort":"go","to":"x"}],
		"triggers":[]
	}`)

	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// ① 两个能力都照装（x@2 是新内容，y@1 全新）
	if fmt.Sprint(res.Created.Capabilities) != fmt.Sprint([]string{"x@2", "y@1"}) {
		t.Fatalf("created.capabilities want [x@2 y@1]，got %v", res.Created.Capabilities)
	}
	if len(res.Skipped.Capabilities) != 0 {
		t.Fatalf("不该有 skipped，got %v", res.Skipped.Capabilities)
	}
	x2, err := db.GetCapability(ctx, nsID, "x", 2)
	if err != nil {
		t.Fatalf("get x@2: %v", err)
	}
	if x2.ID == x1.ID {
		t.Fatal("x@2 折叠成了 x@1：v3 的 digest 吃 version，跨版本折叠已取消")
	}
	// ② 边装得出来，按 manifest 声明的版本钉到 x@2（D7：声明版本优先）
	y1, err := db.GetCapability(ctx, nsID, "y", 1)
	if err != nil {
		t.Fatalf("get y@1: %v", err)
	}
	if fmt.Sprint(res.Created.Edges) != fmt.Sprint([]string{"y --go--> x"}) {
		t.Fatalf("created.edges want [y --go--> x]，got created=%v skipped=%v notes=%v",
			res.Created.Edges, res.Skipped.Edges, res.Notes)
	}
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || edges[0].FromRef != capRef(y1) || edges[0].ToRef != capRef(x2) {
		t.Fatalf("边应在 y@1 → x@2（%s → %s）上，got %d 条：from=%q to=%q",
			capRef(y1), capRef(x2), len(edges), edges[0].FromRef, edges[0].ToRef)
	}
	// ③ 装出来的能力是 noop 占位（manifest 的 runner 不解释成实现），notes 说清楚
	if !notesContain(res.Notes, "noop 占位") {
		t.Fatalf("装成占位的能力应在 notes 里说明，got %v", res.Notes)
	}

	// ④ 体检：x@2 存在 → 能力项 ok 且指向 x@2；边项仍指向 x@1；总体不是 blocked
	checks := mustChecks(t, db, pb)
	capX := itemOf(t, checks, PlaybookItemCapability, 0)
	if capX.Ref != "x@2" {
		t.Fatalf("能力项的 ref 应是 manifest 声明的 x@2，got %q", capX.Ref)
	}
	if capX.Status == PlaybookCheckMissing {
		t.Fatalf("x@2 已经装上了，不该是 missing：%+v", capX)
	}
	if capX.CapID != x2.ID {
		t.Fatalf("能力项 capId 应是 x@2（%s），got %s", x2.ID, capX.CapID)
	}
	edgeItem := itemOf(t, checks, PlaybookItemEdge, 0)
	if edgeItem.Status != PlaybookCheckOK {
		t.Fatalf("边项应 ok（连的是 x@2），got %+v", edgeItem)
	}
	if edgeItem.ToID != x2.ID {
		t.Fatalf("边项 toId 应是声明版本 x@2（%s），got %s", x2.ID, edgeItem.ToID)
	}
	if checks.Status == PlaybookStatusBlocked {
		t.Fatalf("能力都在，总体不该是 blocked：%+v", checks.Items)
	}

	// ⑤ 再装一次：created 全空、行数不变（append-only 幂等）
	capsBefore := countRows(t, db, "capability_versions", "namespace_id = ?", nsID)
	_, again, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if len(again.Created.Capabilities) != 0 || len(again.Created.Edges) != 0 || len(again.Created.Sources) != 0 {
		t.Fatalf("再装一次 created 应全空，got %+v", again.Created)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n != capsBefore {
		t.Fatalf("再装一次不该多出行：want %d got %d", capsBefore, n)
	}
}

// ===== 13. 幂等判据 ②：已有同 kind 且指向本预案入口能力的源（M1）=====

// TestInstallPlaybookSkipsSourcePointingAtSameEntry 钉住 InstallPlaybook 的幂等判据 ②
// （sourceWithKindTargets）：用户自己建的源与装出来的**不同名**，但同 kind 且 targets
// 命中本预案的入口能力 —— 再装一次不能再建一个（同一事件会被派发两次）。
//
// 为什么单开一条：判据 ①（同名源）优先级更高，普通「再装一次」永远先命中 ①，
// 所以 ② 在已有用例里从未被真正跑到。
func TestInstallPlaybookSkipsSourcePointingAtSameEntry(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "pr-review", prReviewManifest())
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}
	// 删掉装出来的那个源，换成「不同名、同 kind、指向同一个入口能力」的源
	installed := sourceByName(t, db, nsID, "pr-review-webhook-0")
	if err := db.DeleteSource(ctx, nsID, installed.ID); err != nil {
		t.Fatalf("delete installed source: %v", err)
	}
	entry, err := db.GetCapability(ctx, nsID, "analyze-pr", 1)
	if err != nil {
		t.Fatalf("get entry capability: %v", err)
	}
	if err := db.CreateSource(ctx, &Source{
		NamespaceID: nsID,
		Name:        "my-own-hook",
		Kind:        SourceKindWebhook,
		Enabled:     true,
		Config:      json.RawMessage(`{}`),
		Targets:     []SourceTarget{{CapabilityID: entry.ID}},
	}); err != nil {
		t.Fatalf("create user source: %v", err)
	}
	sourcesBefore := countRows(t, db, "sources", "namespace_id = ?", nsID)

	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if len(res.Created.Sources) != 0 {
		t.Fatalf("同 kind 且指向同一入口的源已存在，不该再建一个：created=%v", res.Created.Sources)
	}
	if fmt.Sprint(res.Skipped.Sources) != fmt.Sprint([]string{"my-own-hook"}) {
		t.Fatalf("skipped.sources 应是用户那个源名，got %v", res.Skipped.Sources)
	}
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "同 kind") || !strings.Contains(joined, "派发两次") {
		t.Fatalf("notes 应说清「同 kind / 会被派发两次」，got %v", res.Notes)
	}
	if n := countRows(t, db, "sources", "namespace_id = ?", nsID); n != sourcesBefore {
		t.Fatalf("源行数不该变：want %d got %d", sourcesBefore, n)
	}
}

// ===== 14. 列表顺序：created_at, id 升序（D5 / M6）=====

// TestListPlaybooksCreatedAtOrder 钉住 D5 的列表顺序。故意让「先插的 created_at 更晚」：
// 按插入顺序 / 按 id 排都会得出别的答案，只有真按 created_at 升序才对（改成 DESC 也红）。
func TestListPlaybooksCreatedAtOrder(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	base := Now()
	later := &Playbook{NamespaceID: nsID, Slug: "later-plan", Name: "晚的", CreatedAt: base + 100}
	if err := db.CreatePlaybook(ctx, later); err != nil {
		t.Fatalf("create later plan: %v", err)
	}
	earlier := &Playbook{NamespaceID: nsID, Slug: "earlier-plan", Name: "早的", CreatedAt: base}
	if err := db.CreatePlaybook(ctx, earlier); err != nil {
		t.Fatalf("create earlier plan: %v", err)
	}

	list, err := db.ListPlaybooks(ctx, nsID)
	if err != nil {
		t.Fatalf("list playbooks: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有两个预案，got %d（%+v）", len(list), list)
	}
	if list[0].Slug != "earlier-plan" || list[1].Slug != "later-plan" {
		t.Fatalf("列表应按 created_at 升序（早的在前），got [%s, %s]", list[0].Slug, list[1].Slug)
	}
}

// ===== 15. manifest v2：内联完整 spec / ref 引用（Capability Spec v3 §12）=====

// inlineScriptSpec 造一份**脚本形式**的内联 spec（manifest 里唯一允许的实现形式之一，
// 另一种是 handler: noop）。
//
// 注意它是**字符串拼的 JSON**：内联 spec 走的是"用户提交的原文 → 严格解码"这条路，
// 用 map 拼会把「原文里多个键」这类形状差异吃掉，测出来的东西与真实请求不一样。
func inlineScriptSpec(name string, version int, outputs []string, script string) string {
	outputsJSON, _ := json.Marshal(outputs)
	return fmt.Sprintf(`{
		"apiVersion":"anotify/v1",
		"kind":"Capability",
		"metadata":{"name":%q,"version":%d,"displayName":"内联能力"},
		"spec":{
			"params":{"type":"object"},
			"outputs":%s,
			"run":{"entrypoint":"run.sh","files":{"run.sh":%q},"timeoutSec":900}
		}
	}`, name, version, string(outputsJSON), script)
}

// TestInstallPlaybookInlineSpecStoresScriptBody 钉住 §12.2 第 3 条在 store 侧的一半：
// 内联 spec 装出来的能力是**真能跑的**（有正文、有 content_hash），不是空壳。
//
// 断言方式：装完直接从库/正文端点读回脚本字节——只断言 "created 里有它" 是不够的，
// 那种断言在「装出一行没有实现的空壳」时也是绿的。
func TestInstallPlaybookInlineSpecStoresScriptBody(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	const script = "#!/usr/bin/env bash\nset -e\necho inline-ok\n"
	manifest := fmt.Sprintf(`{
		"capabilities":[{"spec":%s}],
		"edges":[],
		"triggers":[]
	}`, inlineScriptSpec("vb-inline", 1, []string{"done"}, script))

	pb := mustCreatePlaybook(t, db, nsID, "inline-spec", manifest)
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if fmt.Sprint(res.Created.Capabilities) != fmt.Sprint([]string{"vb-inline@1"}) {
		t.Fatalf("created.capabilities want [vb-inline@1]，got %v（skipped=%v notes=%v）",
			res.Created.Capabilities, res.Skipped.Capabilities, res.Notes)
	}
	// 它**不是**占位：noop 说明不该出现（那会让列表页把一份真实现标成「未实现」）。
	if notesContain(res.Notes, "noop 占位") {
		t.Fatalf("内联脚本能力不该被标成 noop 占位：%v", res.Notes)
	}

	def, err := db.GetCapability(ctx, nsID, "vb-inline", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if def.ContentHash == "" {
		t.Fatal("装出来的能力 content_hash 为空：它就不是内容寻址的了")
	}
	if def.DisplayName != "内联能力" {
		t.Fatalf("displayName 应取 spec.metadata.displayName，got %q", def.DisplayName)
	}
	if got := def.Spec.Run.Files["run.sh"]; got != script {
		t.Fatalf("脚本正文没落库（got %q）", got)
	}
	if def.Spec.Run.Entrypoint != "run.sh" || def.Spec.Handler != "" {
		t.Fatalf("实现形式应是脚本（entrypoint=run.sh、无 handler），got %+v", def.Spec)
	}
	// 正文端点（详情页展示脚本用的那个）必须能拿到同一份字节。
	art, err := db.CurrentArtifact(ctx, nsID, def.ID)
	if err != nil {
		t.Fatalf("current artifact: %v", err)
	}
	if !strings.Contains(string(art.Spec), "inline-ok") {
		t.Fatalf("产物正文里没有脚本（got %s）", string(art.Spec))
	}
}

// TestInstallPlaybookRefDoesNotCreateCapability 钉住 §12.1 ① ：`ref` **不建任何行**，
// 但边与源照常建立（引用的是命名空间里已有的定义，内置能力也算）。
//
// 反例（这条测要能挡住的那种实现）：把 ref 也当成"声明一份定义"去装 —— 于是
// 命名空间里凭空长出一行与内置能力同名同版本的重复定义，或者干脆装失败。
func TestInstallPlaybookRefDoesNotCreateCapability(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	capsBefore := countRows(t, db, "capability_versions", "namespace_id = ?", nsID)

	pb := mustCreatePlaybook(t, db, nsID, "ref-only", `{
		"capabilities":[{"ref":"notify.phone"}],
		"edges":[],
		"triggers":[{"kind":"manual","targets":[{"to":"notify.phone"}]}]
	}`)
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.Created.Capabilities) != 0 {
		t.Fatalf("ref 不该建任何能力行，created.capabilities=%v", res.Created.Capabilities)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n != capsBefore {
		t.Fatalf("ref 装完能力行数变了：want %d got %d", capsBefore, n)
	}
	// 源与入口边照常建立：引用形态不影响「事件从哪进来」。
	if fmt.Sprint(res.Created.Sources) != fmt.Sprint([]string{"ref-only-manual-0"}) {
		t.Fatalf("created.sources want [ref-only-manual-0]，got %v（notes=%v）",
			res.Created.Sources, res.Notes)
	}
	src := sourceByName(t, db, nsID, "ref-only-manual-0")
	if len(src.Targets) != 1 {
		t.Fatalf("源应有 1 个 target，got %+v", src.Targets)
	}
	notify, err := db.GetCapability(ctx, nsID, "notify.phone", 1)
	if err != nil {
		t.Fatalf("get notify.phone@1: %v", err)
	}
	if src.Targets[0].CapabilityID != notify.ID {
		t.Fatalf("源的 target 应指向被引用的 notify.phone@1（%s），got %s", notify.ID, src.Targets[0].CapabilityID)
	}
}

// TestInstallPlaybookRejectsForeignHandler 是 I1 的红线（§12.2 第 2 条）：
// manifest **只允许** handler: noop。允许别的 handler 等于「一份配置文件就能把任意
// 能力挂到中枢执行」——中枢会 os/exec 用户写的代码的反面同样致命：它会替用户声称
// 一份平台内置执行器（比如把任意能力的链尾变成"发手机推送"）。
//
// 两处都要拦：创建期（正常路径）与安装期（绕过创建期的手改数据）。
func TestInstallPlaybookRejectsForeignHandler(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	foreign := fmt.Sprintf(`{
		"capabilities":[{"spec":{
			"apiVersion":"anotify/v1","kind":"Capability",
			"metadata":{"name":"vb-sneaky","version":1},
			"spec":{"outputs":[],"handler":%q}
		}}]
	}`, artifact.HandlerNotifyPhone)
	noop := fmt.Sprintf(`{
		"capabilities":[{"spec":{
			"apiVersion":"anotify/v1","kind":"Capability",
			"metadata":{"name":"vb-placeholder","version":1},
			"spec":{"outputs":[],"handler":%q}
		}}]
	}`, artifact.HandlerNoop)

	// ① 创建期：400 语义 + 消息带下标
	err := db.CreatePlaybook(ctx, &Playbook{
		NamespaceID: nsID, Slug: "sneaky", Name: "偷偷上报",
		Manifest: json.RawMessage(foreign),
	})
	if !errors.Is(err, ErrPlaybookManifestInvalid) {
		t.Fatalf("manifest 带 notify-phone 应被拒（ErrPlaybookManifestInvalid），got %v", err)
	}
	if !strings.Contains(err.Error(), "capabilities[0]") {
		t.Fatalf("错误里要带下标，got %q", err.Error())
	}
	// 反例：noop 是唯一被接受的 handler（占位能力不伪造、也不越权）。
	if err := db.CreatePlaybook(ctx, &Playbook{
		NamespaceID: nsID, Slug: "placeholder", Name: "占位",
		Manifest: json.RawMessage(noop),
	}); err != nil {
		t.Fatalf("handler: noop 应被接受，got %v", err)
	}

	// ② 安装期：手改坏的 manifest（绕过创建期）同样 400，并带下标。
	raw := &Playbook{
		ID: NewPlaybookID(), NamespaceID: nsID, Slug: "hand-edited",
		Name: "手改的", Description: "", Manifest: json.RawMessage(foreign), CreatedAt: Now(),
	}
	if _, insErr := db.ExecContext(ctx,
		`INSERT INTO playbooks (`+playbookColumns+`) VALUES (?,?,?,?,?,?,?)`,
		raw.ID, raw.NamespaceID, raw.Slug, raw.Name, raw.Description,
		string(raw.Manifest), raw.CreatedAt); insErr != nil {
		t.Fatalf("seed hand-edited playbook: %v", insErr)
	}
	if _, _, insErr := db.InstallPlaybook(ctx, nsID, raw.Slug); !errors.Is(insErr, ErrPlaybookManifestInvalid) {
		t.Fatalf("安装期也要拒外来 handler（ErrPlaybookManifestInvalid），got %v", insErr)
	} else if !strings.Contains(insErr.Error(), "capabilities[0]") {
		t.Fatalf("安装期错误里要带下标，got %q", insErr.Error())
	}
}

// TestInstallPlaybookRefAndSpecBothGiven 钉住 §12.2 第 1 条：ref 与 spec **恰好给一个**。
// 两个都给 / 都不给都是 400，且消息带下标（用户得知道是哪一条）。
func TestInstallPlaybookRefAndSpecBothGiven(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	cases := []struct {
		name     string
		manifest string
		wantSub  string
	}{
		{
			name: "ref 与 spec 都给",
			manifest: fmt.Sprintf(`{"capabilities":[{"ref":"notify.phone","spec":%s}]}`,
				inlineScriptSpec("vb-both", 1, nil, "#!/bin/sh\nexit 0\n")),
			wantSub: "恰好给一个",
		},
		{
			name:     "ref 与 spec 都不给",
			manifest: `{"capabilities":[{"ref":""}]}`,
			wantSub:  "必须给 ref 或 spec",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := db.CreatePlaybook(ctx, &Playbook{
				NamespaceID: nsID, Slug: fmt.Sprintf("both-%d", i),
				Name: "两种都给", Manifest: json.RawMessage(tc.manifest),
			})
			if !errors.Is(err, ErrPlaybookManifestInvalid) {
				t.Fatalf("应被拒，got %v", err)
			}
			if !strings.Contains(err.Error(), "capabilities[0]") {
				t.Fatalf("错误里要带下标，got %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误里应说清原因（含 %q），got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// TestInstallPlaybookEdgeCarriesLiteralsAndPriority 钉住 §12.1 的边扩展：
// literals（常量覆盖）与 priority（同端口候选优先级）都要落进 edges 行。
//
// 为什么值得单测：这两列在派发路径上是**真的会用**的（literals 覆盖参数、
// priority 决定先选谁）。manifest 里写了却没落库 = 用户配置静默失效。
func TestInstallPlaybookEdgeCarriesLiteralsAndPriority(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 目标能力的 params_schema 里必须有 mode：常量的键要 ∈ 目标契约（与 CreateEdge 同口径）。
	pb := mustCreatePlaybook(t, db, nsID, "edge-attrs", `{
		"capabilities":[
			{"name":"up","version":1,"outputs":["go"]},
			{"name":"down","version":1,"outputs":[],"paramsSchema":{
				"type":"object","properties":{"mode":{"type":"string"}}
			}}
		],
		"edges":[{"from":"up","fromPort":"go","to":"down","literals":{"mode":"safe"},"priority":3}],
		"triggers":[]
	}`)
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if fmt.Sprint(res.Created.Edges) != fmt.Sprint([]string{"up --go--> down"}) {
		t.Fatalf("边应被创建，created=%v skipped=%v notes=%v",
			res.Created.Edges, res.Skipped.Edges, res.Notes)
	}
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("应有 1 条边，got %d", len(edges))
	}
	if got := string(edges[0].Literals); got != `{"mode":"safe"}` {
		t.Fatalf("edges.literals want {\"mode\":\"safe\"}，got %s", got)
	}
	if edges[0].Priority != 3 {
		t.Fatalf("edges.priority want 3，got %d", edges[0].Priority)
	}
}

// TestInstallPlaybookTriggerNameAndMapping 钉住 §12.1 的触发器扩展：
// `name` 决定源名（省略才用 <slug>-<kind>-<idx>）、`config` 走 ValidateSourceConfig、
// `enabled` 落库、`targets[].mapping` 落进入口边。
func TestInstallPlaybookTriggerNameAndMapping(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "trg-v2", `{
		"capabilities":[
			{"name":"entry","version":1,"outputs":[],"paramsSchema":{
				"type":"object","properties":{"repo":{"type":"string"}}
			}}
		],
		"edges":[],
		"triggers":[
			{
				"name":"pr-review-github",
				"kind":"github",
				"config":{"event":"pull_request"},
				"targets":[{"to":"entry","mapping":{"repo":"{{repository.full_name}}"}}]
			},
			{
				"name":"nightly",
				"kind":"cron",
				"enabled":false,
				"config":{"expr":"0 3 * * *"},
				"targets":[{"to":"entry"}]
			}
		]
	}`)
	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if fmt.Sprint(res.Created.Sources) != fmt.Sprint([]string{"pr-review-github", "nightly"}) {
		t.Fatalf("created.sources 应使用触发器声明的 name，got %v（notes=%v）",
			res.Created.Sources, res.Notes)
	}

	gh := sourceByName(t, db, nsID, "pr-review-github")
	if gh.Kind != SourceKindGitHub || !gh.Enabled {
		t.Fatalf("github 源应启用，got kind=%s enabled=%v", gh.Kind, gh.Enabled)
	}
	var ghCfg struct {
		Event string `json:"event"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(gh.Config, &ghCfg); err != nil {
		t.Fatalf("decode github config: %v", err)
	}
	if ghCfg.Event != "pull_request" {
		t.Fatalf("config 应原样走 ValidateSourceConfig（event=pull_request），got %+v", ghCfg)
	}
	if ghCfg.Token == "" {
		t.Fatalf("github 源的 token 应由 ValidateSourceConfig 生成，got %+v", ghCfg)
	}

	nightly := sourceByName(t, db, nsID, "nightly")
	if nightly.Enabled {
		t.Fatal("enabled:false 的触发器不该装成启用的源")
	}
	if nightly.NextFireAt == nil {
		t.Fatal("cron 源应算好首次触发时刻")
	}

	// targets[].mapping 要落在**入口边**上（edges.mapping），不是源自己的一张表。
	edges, err := db.ListEdges(ctx, nsID, "")
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	var found bool
	for _, e := range edges {
		if !e.FromIsSource || e.FromRef != "pr-review-github" {
			continue
		}
		found = true
		if got := string(e.Mapping); got != `{"repo":"{{repository.full_name}}"}` {
			t.Fatalf("入口边 mapping want {\"repo\":\"{{repository.full_name}}\"}，got %s", got)
		}
	}
	if !found {
		t.Fatalf("没有入口边 from_ref=pr-review-github（got %d 条边）", len(edges))
	}
}

// TestInstallPlaybookLegacyManifestStillInstalls 是 §12.3 的回归门：
// 旧形态（name/version/outputs/paramsSchema/runner）继续接受，且**与回填口径一致**——
// 装成 handler: noop 的占位（不伪造用户脚本），runner 原样存不解释。
func TestInstallPlaybookLegacyManifestStillInstalls(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	manifest := `{
		"capabilities":[{"name":"vb-legacy","version":1,"outputs":["ok"],
			"paramsSchema":{"type":"object","properties":{"branch":{"type":"string"}}},
			"runner":{"runner":"bash","command":"echo hi"}}],
		"edges":[],
		"triggers":[]
	}`
	pb := mustCreatePlaybook(t, db, nsID, "legacy-form", manifest)
	// manifest 仍然存原文（D1）：runner / 自定义键原样往返。
	stored, err := db.GetPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("get playbook: %v", err)
	}
	if string(stored.Manifest) != manifest {
		t.Fatalf("manifest 应逐字节存原文，got %s", string(stored.Manifest))
	}

	_, res, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("旧形态 manifest 仍能装: %v", err)
	}
	if fmt.Sprint(res.Created.Capabilities) != fmt.Sprint([]string{"vb-legacy@1"}) {
		t.Fatalf("created.capabilities want [vb-legacy@1]，got %v（notes=%v）",
			res.Created.Capabilities, res.Notes)
	}
	def, err := db.GetCapability(ctx, nsID, "vb-legacy", 1)
	if err != nil {
		t.Fatalf("get capability: %v", err)
	}
	if def.Spec.Handler != artifact.HandlerNoop {
		t.Fatalf("旧形态应装成 handler: noop 占位（与回填口径一致），got %q", def.Spec.Handler)
	}
	if fmt.Sprint(def.Outputs) != fmt.Sprint([]string{"ok"}) {
		t.Fatalf("outputs 应原样保留，got %v", def.Outputs)
	}
	if !strings.Contains(string(def.ParamsSchema), "branch") {
		t.Fatalf("paramsSchema 应原样保留，got %s", string(def.ParamsSchema))
	}
	if !notesContain(res.Notes, "noop 占位") {
		t.Fatalf("占位能力必须在 notes 里说明，got %v", res.Notes)
	}
}

// TestPlaybookChecksHubHandlerRefIsReady 钉住「ref: notify.phone 这条链尾」在体检里是**就绪**的。
//
// handler 形式的能力由中枢执行（spec.handler 在可执行白名单里），它本来就不需要任何
// 节点声明。若体检按「能力 → 节点」的通用规则报 no-node，`ref: notify.phone` 这条
// 唯一合法的「链尾发推送」会永远显示缺节点 —— 而它其实已经能跑了。
func TestPlaybookChecksHubHandlerRefIsReady(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "notify-sink", `{
		"capabilities":[
			{"name":"collect","version":1,"outputs":["out"]},
			{"ref":"notify.phone"}
		],
		"edges":[{"from":"collect","fromPort":"out","to":"notify.phone"}],
		"triggers":[{"kind":"manual","targets":[{"to":"collect"}]}]
	}`)
	if _, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug); err != nil {
		t.Fatalf("install: %v", err)
	}

	checks := mustChecks(t, db, pb)
	// 能力项 0 = collect（旧形态，无节点 → no-node），1 = ref notify.phone（中枢执行 → ok）
	capRef := itemOf(t, checks, PlaybookItemCapability, 1)
	if capRef.Declared != "ref" {
		t.Fatalf("第二项应是 ref 声明，got declared=%q ref=%q", capRef.Declared, capRef.Ref)
	}
	if capRef.Status != PlaybookCheckOK {
		t.Fatalf("中枢执行的能力不该报缺节点（got %s）：%+v", capRef.Status, capRef)
	}
	if capRef.Impl != "handler" {
		t.Fatalf("实现形式应是 handler（notify-phone），got %q", capRef.Impl)
	}
	// 旧形态那一项仍然如实报缺节点（占位能力中枢不执行，链并不真的能跑）——
	// 于是**唯一的** blocked/needs-node 项必须是它，而不是 ref 那一条。
	if got := itemOf(t, checks, PlaybookItemCapability, 0); got.Status != PlaybookCheckNoNode {
		t.Fatalf("旧形态（noop 占位）仍应报缺节点，got %s", got.Status)
	}
	if checks.Next == nil || checks.Next.Ref != "collect@1" {
		t.Fatalf("「最该先做的一件事」应是那个占位能力，got %+v", checks.Next)
	}
	if other := checks.Next; other.Kind != PlaybookItemCapability || other.Status != PlaybookCheckNoNode {
		t.Fatalf("next 应是 collect@1 的 no-node，got %+v", other)
	}
}

// TestInstallPlaybookRejectsUnresolvableRef 钉住 §12.2 第 3 条对 `ref` 的要求：
// 引用不到的定义**带下标 400**，不允许「装了一半」（边指向一个不存在的名字，
// 装出来的源永远不会产生任务，而且没有任何报错）。
//
// 创建期刻意**不**拦：能力可能是别处先建、预案后装（顺序自由）。但安装时必须拦。
func TestInstallPlaybookRejectsUnresolvableRef(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	pb := mustCreatePlaybook(t, db, nsID, "ghost-ref", `{
		"capabilities":[{"ref":"ghost@3"}],
		"edges":[],
		"triggers":[]
	}`)
	_, _, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if !errors.Is(err, ErrPlaybookManifestInvalid) {
		t.Fatalf("引用不到的定义应 400（ErrPlaybookManifestInvalid），got %v", err)
	}
	if !strings.Contains(err.Error(), "capabilities[0]") || !strings.Contains(err.Error(), "ghost@3") {
		t.Fatalf("错误里要带下标与被引用的名字，got %q", err.Error())
	}
}
