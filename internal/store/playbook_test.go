package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
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
	if err := db.CreateCapability(context.Background(), c); err != nil {
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
	orgID, nsID := newTestOrgNS(t, db)
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
	if byID.OrgID != orgID {
		t.Fatalf("orgId 应从命名空间派生：want %s got %s", orgID, byID.OrgID)
	}
	if !strings.HasPrefix(byID.ID, "pb_") {
		t.Fatalf("预案 id 应是 pb_ 前缀，got %s", byID.ID)
	}

	// 另一个命名空间取不到（I11）
	other, err := db.CreateNamespace(ctx, orgID, "staging", "预发")
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
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "ghost") {
		t.Fatalf("skipped 的边应有一条说明且指出缺的能力，got %v", res.Notes)
	}
	if n := countRows(t, db, "edges", "namespace_id = ?", nsID); n != 1 {
		t.Fatalf("库里应只有 1 条边，got %d", n)
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
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "cron") {
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

	// ② 同内容不同版本（x@2 与 x@1 内容完全相同）→ skipped + note，不产生第二行
	same := mustCreatePlaybook(t, db, nsID, "same-content", `{
		"capabilities":[{"name":"x","version":2,"outputs":["old-port"],"paramsSchema":{"type":"object"}}]
	}`)
	_, res, err = db.InstallPlaybook(ctx, nsID, same.Slug)
	if err != nil {
		t.Fatalf("同内容冲突不该让安装失败: %v", err)
	}
	if len(res.Created.Capabilities) != 0 {
		t.Fatalf("同内容不该产生第二行定义，got %v", res.Created.Capabilities)
	}
	if fmt.Sprint(res.Skipped.Capabilities) != fmt.Sprint([]string{"x@2"}) {
		t.Fatalf("同内容的 x@2 应进 skipped，got %v", res.Skipped.Capabilities)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "x@1") {
		t.Fatalf("同内容的 note 应指出已有的那个版本，got %v", res.Notes)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?", nsID, "x"); n != 1 {
		t.Fatalf("x 只该有一行定义，got %d", n)
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
	if len(edges) != 1 || edges[0].FromCapability != x2.ID || edges[0].ToCapability != y1.ID {
		t.Fatalf("库里那条边的两端应是 x@2 / y@1，got %+v", edges)
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

// ===== 12. 同内容复用：D9 + D7 的组合不能留下「永久 blocked」死状态（M3）=====

// TestInstallPlaybookReusesSameContentDefinition 钉住「manifest 声明 x@2、库里只有同内容的
// x@1」时的完整链条（issue #193 审查 M3）：
//
//	① 能力：x@2 进 skipped（I8：同内容不产生第二行定义），y@1 照装
//	② 边：y --go--> x **建得出来**，且连的是 x@1 的 id，不是「缺定义」
//	③ notes：把「同内容」与「复用」说清
//	④ 体检：x@2 不是 missing、边项 ok、总体不再是 blocked（死状态消除）
//	⑤ 再装一次仍幂等，同内容没有第二行定义
//
// 修之前：安装报「能力「x」在本命名空间里没有定义」、体检说 x@2 missing → 总体恒
// blocked，而用户「补齐能力」也补不上（I8 不允许第二行同内容定义）。
func TestInstallPlaybookReusesSameContentDefinition(t *testing.T) {
	db := newTestDB(t)
	nsID, _ := newTestNS(t, db)
	ctx := context.Background()

	// 历史定义 x@1：与预案声明的 x@2 同内容（outputs / paramsSchema 都一样）
	x1 := mustCap(t, db, nsID, "x", 1, "old-port")

	pb := mustCreatePlaybook(t, db, nsID, "reuse-same-content", `{
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
	// ① 同内容的 x@2 不产生第二行定义，y@1 照装
	if fmt.Sprint(res.Created.Capabilities) != fmt.Sprint([]string{"y@1"}) {
		t.Fatalf("created.capabilities want [y@1]，got %v", res.Created.Capabilities)
	}
	if fmt.Sprint(res.Skipped.Capabilities) != fmt.Sprint([]string{"x@2"}) {
		t.Fatalf("skipped.capabilities want [x@2]，got %v", res.Skipped.Capabilities)
	}
	// ② 边装得出来，两端是 y@1 → x@1
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
	if len(edges) != 1 || edges[0].FromCapability != y1.ID || edges[0].ToCapability != x1.ID {
		t.Fatalf("边应连在 y@1 → x@1（%s → %s）上，got %+v", y1.ID, x1.ID, edges)
	}
	// ③ notes 说清「同内容」与「复用」
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "同内容") || !strings.Contains(joined, "复用") {
		t.Fatalf("notes 应说清同内容复用，got %v", res.Notes)
	}

	// ④ 体检：x@2 按**复用的 x@1** 算（不是 missing），边项 ok，总体不再 blocked
	checks := mustChecks(t, db, pb)
	capX := itemOf(t, checks, PlaybookItemCapability, 0)
	if capX.Ref != "x@2" {
		t.Fatalf("能力项的 ref 仍应是 manifest 声明的 x@2，got %q", capX.Ref)
	}
	if capX.Status == PlaybookCheckMissing {
		t.Fatalf("x@2 同内容复用了 x@1，不该再是 missing：%+v", capX)
	}
	if capX.CapID != x1.ID {
		t.Fatalf("能力项 capId 应是复用的 x@1（%s），got %s", x1.ID, capX.CapID)
	}
	edgeItem := itemOf(t, checks, PlaybookItemEdge, 0)
	if edgeItem.Status != PlaybookCheckOK {
		t.Fatalf("边项应 ok（连的就是 x@1），got %+v", edgeItem)
	}
	if edgeItem.ToID != x1.ID {
		t.Fatalf("边项 toId 应是 x@1（%s），got %s", x1.ID, edgeItem.ToID)
	}
	if checks.Status == PlaybookStatusBlocked {
		t.Fatalf("x 有定义（同内容复用），总体不该是 blocked：%+v", checks.Items)
	}

	// ⑤ 再装一次：created 全空、行数不变（同内容没有第二行）
	capsBefore := countRows(t, db, "capability_versions", "namespace_id = ?", nsID)
	// x 只该有一行定义（同内容不产生第二行，I8）
	if n := countRows(t, db, "capability_versions", "namespace_id = ? AND name = ?", nsID, "x"); n != 1 {
		t.Fatalf("x 同内容只能有一行定义（I8），got %d", n)
	}
	_, again, err := db.InstallPlaybook(ctx, nsID, pb.Slug)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if len(again.Created.Capabilities) != 0 || len(again.Created.Edges) != 0 || len(again.Created.Sources) != 0 {
		t.Fatalf("再装一次 created 应全空，got %+v", again.Created)
	}
	if n := countRows(t, db, "capability_versions", "namespace_id = ?", nsID); n != capsBefore {
		t.Fatalf("同内容不该产生第二行定义：want %d got %d", capsBefore, n)
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
