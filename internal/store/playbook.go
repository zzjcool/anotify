package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 预案（playbook）：一份**可安装的流程零件**（#193，设计 docs/design/playbooks.md）。
//
// 三件事在这一层说清楚：
//
//  1. 预案本身**不产生任务**。它只是一份清单（能力 / 边 / 触发器），
//     安装（InstallPlaybook）才把清单变成命名空间里的真实资源。
//  2. manifest 存**提交的 JSON 原文**（D1）：runner / paramsSchema 这类节点侧
//     字段中枢只存不解释（I1），重新编码会丢掉键序与用户自定义键。
//  3. 就绪体检（PlaybookChecks）是**查询时算出来的推导量**，不落库（I3 同源），
//     口径与 web/mock.js 的 pbChecks 逐条对应（D6）。

// Playbook 是 playbooks 表的一行。
type Playbook struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespaceId"`
	OrgID       string `json:"orgId"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Manifest 是**提交的 JSON 原文**。读出来逐字节等于写进去的那份，
	// 这样前端回填表单、节点侧 runner 配置都不会被中枢悄悄改写。
	Manifest  json.RawMessage `json:"manifest"`
	CreatedAt int64           `json:"createdAt"`
}

// PlaybookManifest 是 manifest 解析后的形态（只用于**校验与体检**，不用于回写）。
//
// Runner / ParamsSchema 只透传：它们是节点侧配置，中枢不拆、不执行（I1）。
type PlaybookManifest struct {
	Capabilities []ManifestCapability `json:"capabilities"`
	Edges        []ManifestEdge       `json:"edges"`
	Triggers     []ManifestTrigger    `json:"triggers"`
}

// ManifestCapability 是 manifest 声明的一个能力定义（name@version + 出口）。
type ManifestCapability struct {
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	Outputs      []string        `json:"outputs,omitempty"`
	ParamsSchema json.RawMessage `json:"paramsSchema,omitempty"`
	Runner       json.RawMessage `json:"runner,omitempty"`
}

// ManifestEdge 是 manifest 声明的一条边：按**名字**引用两端能力（D7）。
type ManifestEdge struct {
	From     string `json:"from"`
	FromPort string `json:"fromPort"`
	To       string `json:"to"`
}

// ManifestTrigger 是 manifest 声明的一个触发器。
//
// Types 对 cron 就是五段表达式本身（`["0 3 * * *"]`），对 webhook/github 是
// 订阅的事件类型；安装时原样写进源配置（D11）。
type ManifestTrigger struct {
	Kind  string   `json:"kind"`
	Types []string `json:"types,omitempty"`
}

// 预案相关的哨兵错误（handler 据此翻 400 / 404 / 409）。
var (
	ErrPlaybookNotFound        = errors.New("playbook not found")
	ErrPlaybookSlugTaken       = errors.New("playbook slug already exists")
	ErrPlaybookSlugInvalid     = errors.New("playbook slug invalid")
	ErrPlaybookNameRequired    = errors.New("playbook name required")
	ErrPlaybookManifestInvalid = errors.New("playbook manifest invalid")
)

// PlaybookSlugMaxLen 是 slug 的长度上限（它出现在 URL 里，太长读不出也记不住）。
const PlaybookSlugMaxLen = 64

// playbookSlugPattern 是 slug 的合法形态（D3）：小写字母 / 数字的分段，用单个
// 连字符连接。**不 trim**：`" pr-review"` 这种带空白的输入当场拒绝，
// 而不是静默改对后落库 —— slug 是 URL 的稳定标识，改一个字符就是另一个预案。
var playbookSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// playbookReservedCapNames 是**不能**在 manifest 里当能力名用的保留名。
//
// 这四个是「事件来源」的内置起点名（#110 之后它们不再作为能力存在）。
// 预案里出现它们，装出来的能力会与「来源」这个概念撞名：界面上一眼看不出
// 哪个是"进来事件的""哪个是干活的"，而边的两端本该死板地是能力。
// 所以配置期（创建预案时）就拒掉，而不是等安装期再 skip（D12）。
var playbookReservedCapNames = map[string]bool{
	"github": true,
	"cron":   true,
	"http":   true,
	"manual": true,
}

// ValidatePlaybookSlug 校验 slug（D3）：合法返回 nil，否则返回包了
// ErrPlaybookSlugInvalid 的可定位错误（handler 翻 400，消息要指名道姓）。
func ValidatePlaybookSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("%w: slug 不能为空", ErrPlaybookSlugInvalid)
	}
	if len(slug) > PlaybookSlugMaxLen {
		return fmt.Errorf("%w: slug「%s」长度 %d 超过上限 %d",
			ErrPlaybookSlugInvalid, slug, len(slug), PlaybookSlugMaxLen)
	}
	if !playbookSlugPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug「%s」只能由小写字母 / 数字组成，用单个连字符分段（如 pr-review）",
			ErrPlaybookSlugInvalid, slug)
	}
	return nil
}

// emptyPlaybookManifest 是「没有 manifest」的归一形态（D13：三条空数组，不是 null）。
func emptyPlaybookManifest() PlaybookManifest {
	return PlaybookManifest{
		Capabilities: []ManifestCapability{},
		Edges:        []ManifestEdge{},
		Triggers:     []ManifestTrigger{},
	}
}

// isBlankJSON 报告 raw 是否「没写」（空 / 全是空白 / 字面 null）。
func isBlankJSON(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t == "" || t == "null"
}

// ParsePlaybookManifest 解析并校验 manifest（D12）。
//
// 空 / null → 三条空数组（不是错误：没写 manifest 等于「空预案」）。
// 校验错误一律包 ErrPlaybookManifestInvalid，且消息里带定位（`edges[1].fromPort`），
// 因为安装期只能 skip，配置期把矛盾说清楚是唯一的机会。
func ParsePlaybookManifest(raw json.RawMessage) (PlaybookManifest, error) {
	out := emptyPlaybookManifest()
	if isBlankJSON(raw) {
		return out, nil
	}
	var m PlaybookManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return out, fmt.Errorf("%w: manifest 必须是 JSON 对象：%v", ErrPlaybookManifestInvalid, err)
	}
	// 单项为 null 也归一成 []（前端拿到 null 会崩在 .map 上）
	if m.Capabilities == nil {
		m.Capabilities = []ManifestCapability{}
	}
	if m.Edges == nil {
		m.Edges = []ManifestEdge{}
	}
	if m.Triggers == nil {
		m.Triggers = []ManifestTrigger{}
	}

	// declaredOutputs：manifest 自己声明的 name → outputs（同名多版本取第一条：
	// 边的两端只写名字，名字重复时没有"该用哪个版本"的信息）。
	declaredOutputs := make(map[string][]string, len(m.Capabilities))
	seen := make(map[string]bool, len(m.Capabilities))
	for i, c := range m.Capabilities {
		where := fmt.Sprintf("capabilities[%d]", i)
		if strings.TrimSpace(c.Name) == "" {
			return out, fmt.Errorf("%w: %s.name 不能为空", ErrPlaybookManifestInvalid, where)
		}
		if playbookReservedCapNames[c.Name] {
			return out, fmt.Errorf("%w: %s.name「%s」是内置事件来源的保留名（来源不是能力）",
				ErrPlaybookManifestInvalid, where, c.Name)
		}
		if c.Version <= 0 {
			return out, fmt.Errorf("%w: %s.version 必须大于 0（%s@%d）",
				ErrPlaybookManifestInvalid, where, c.Name, c.Version)
		}
		ref := fmt.Sprintf("%s@%d", c.Name, c.Version)
		if seen[ref] {
			return out, fmt.Errorf("%w: %s 与前面的声明重复（%s）", ErrPlaybookManifestInvalid, where, ref)
		}
		seen[ref] = true
		if _, ok := declaredOutputs[c.Name]; !ok {
			declaredOutputs[c.Name] = c.Outputs
		}
		if !isBlankJSON(c.ParamsSchema) {
			// 只判"是不是 JSON 对象"：中枢不解释 schema 内容（I1），
			// 但一个数组 / 标量塞进 paramsSchema 会在派发时变成无人能懂的失败。
			if _, ok := decodeJSONObject(c.ParamsSchema); !ok {
				return out, fmt.Errorf("%w: %s.paramsSchema 必须是 JSON 对象（%s）",
					ErrPlaybookManifestInvalid, where, ref)
			}
		}
	}

	for i, e := range m.Edges {
		where := fmt.Sprintf("edges[%d]", i)
		if strings.TrimSpace(e.From) == "" || strings.TrimSpace(e.FromPort) == "" || strings.TrimSpace(e.To) == "" {
			return out, fmt.Errorf("%w: %s 的 from / fromPort / to 都不能为空", ErrPlaybookManifestInvalid, where)
		}
		// from 在本 manifest 里声明过 → 端口必须落在它声明的出口里（I2 的配置期版本）。
		// 安装期只能 skip（D10），所以能在这里说清就别等到安装。
		if outs, ok := declaredOutputs[e.From]; ok && !containsString(outs, e.FromPort) {
			return out, fmt.Errorf("%w: %s.fromPort「%s」不在 %s 的 outputs %v 里",
				ErrPlaybookManifestInvalid, where, e.FromPort, e.From, outs)
		}
	}

	for i, t := range m.Triggers {
		where := fmt.Sprintf("triggers[%d]", i)
		if strings.TrimSpace(t.Kind) == "" {
			return out, fmt.Errorf("%w: %s.kind 不能为空", ErrPlaybookManifestInvalid, where)
		}
		if !IsValidSourceKind(t.Kind) {
			return out, fmt.Errorf("%w: %s.kind「%s」不合法（可选 %s）",
				ErrPlaybookManifestInvalid, where, t.Kind, strings.Join(SourceKinds, " / "))
		}
	}

	return m, nil
}

// parsedManifest 读物化后的 manifest（**读侧容忍**）。
//
// 与 ParsePlaybookManifest 的分工：那条是写入口的校验（坏 manifest 一律 400），
// 这条是读路径（体检 / 安装）的解析。一条被手改坏的 manifest 不该让整个
// 预案列表打不开（与 unmarshalTargetParams 同一口径）。
func (pb *Playbook) parsedManifest() PlaybookManifest {
	out := emptyPlaybookManifest()
	if pb == nil || isBlankJSON(pb.Manifest) {
		return out
	}
	var m PlaybookManifest
	if err := json.Unmarshal(pb.Manifest, &m); err != nil {
		return out
	}
	if m.Capabilities == nil {
		m.Capabilities = []ManifestCapability{}
	}
	if m.Edges == nil {
		m.Edges = []ManifestEdge{}
	}
	if m.Triggers == nil {
		m.Triggers = []ManifestTrigger{}
	}
	return m
}

// containsString 判断集合里是否有 s（大小写敏感，出口名是精确标识）。
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// declaredCaps 返回 manifest 自己声明的 name → 那份定义（同名多版本取第一条：
// 边的两端只写名字，名字重复时没有"该用哪个版本"的信息）。
//
// 体检与安装都从这里取「声明的是哪一版、内容长什么样」：按名字解析定义的规则
// 必须只有一份（D7 + M3），各抄一个 map 早晚分叉（原先体检与安装各有一份
// `declaredVersion`，现在只剩这里的这份；`ParsePlaybookManifest` 里的
// `declaredOutputs` 是配置期校验用的另一种口径，不在此列）。
func declaredCaps(m PlaybookManifest) map[string]ManifestCapability {
	out := make(map[string]ManifestCapability, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if _, ok := out[c.Name]; !ok {
			out[c.Name] = c
		}
	}
	return out
}

// capAt 取「manifest 有没有声明这个名字」的指针形态（没声明返回 nil）。
func capAt(declared map[string]ManifestCapability, name string) *ManifestCapability {
	if c, ok := declared[name]; ok {
		return &c
	}
	return nil
}

// manifestCapHash 算 manifest 里一条能力声明**安装时会得到**的 content_hash。
//
// 必须与安装插入时用同一份口径（NormalizeCapability：paramsSchema 空则 {}、
// outputs 空则 []）：算出来的值要能命中 capability_versions.content_hash，否则
// 「同内容复用」（I8：同内容就是同一份能力，只是版本号不同）永远命不中。
func manifestCapHash(namespaceID string, c ManifestCapability) (string, error) {
	def := &CapabilityVersion{
		NamespaceID:  namespaceID,
		Name:         c.Name,
		Version:      c.Version,
		ParamsSchema: c.ParamsSchema,
		Outputs:      c.Outputs,
		Source:       CapSourceCustom,
	}
	if def.Outputs == nil {
		def.Outputs = []string{}
	}
	hash, err := NormalizeCapability(def)
	if err != nil {
		return "", fmt.Errorf("hash manifest capability %s@%d: %w", c.Name, c.Version, err)
	}
	return hash, nil
}

// playbookColumns 是统一的列顺序，scanPlaybook 依赖它。
const playbookColumns = `id, namespace_id, org_id, slug, name, descr, manifest, created_at`

// scanPlaybook 执行查询并解析一行（未命中返回 sql.ErrNoRows）。
func scanPlaybook(r scanner) (*Playbook, error) {
	var (
		pb       Playbook
		manifest string
	)
	if err := r.Scan(&pb.ID, &pb.NamespaceID, &pb.OrgID, &pb.Slug, &pb.Name,
		&pb.Description, &manifest, &pb.CreatedAt); err != nil {
		return nil, err
	}
	pb.Manifest = json.RawMessage(manifest)
	return &pb, nil
}

// CreatePlaybook 建一个预案。
//
// manifest **只校验、不重编码**（D1）：库里存的就是调用方提交的那份字节，
// 读回来逐字节相同（runner / paramsSchema / 自定义键都在）。
// 空 / null manifest 归一化成三条空数组（D13）。
func (d *DB) CreatePlaybook(ctx context.Context, pb *Playbook) error {
	if pb == nil {
		return fmt.Errorf("create playbook: nil")
	}
	if pb.NamespaceID == "" || pb.NamespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	if err := ValidatePlaybookSlug(pb.Slug); err != nil {
		return err
	}
	if strings.TrimSpace(pb.Name) == "" {
		return ErrPlaybookNameRequired
	}
	manifest := pb.Manifest
	if isBlankJSON(manifest) {
		manifest = json.RawMessage(`{"capabilities":[],"edges":[],"triggers":[]}`)
	}
	if _, err := ParsePlaybookManifest(manifest); err != nil {
		return err
	}

	// org_id 从命名空间派生（I10：namespace.org_id == org_id 由构造保证，
	// 调用方给错 org 也写不出不一致的行）。
	orgID, err := d.NamespaceOrgID(ctx, pb.NamespaceID)
	if err != nil {
		return fmt.Errorf("create playbook: %w", err)
	}
	pb.OrgID = orgID
	pb.Manifest = manifest
	if pb.CreatedAt == 0 {
		pb.CreatedAt = Now()
	}
	if pb.ID == "" {
		pb.ID = NewPlaybookID()
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO playbooks (`+playbookColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		pb.ID, pb.NamespaceID, pb.OrgID, pb.Slug, pb.Name, pb.Description,
		string(pb.Manifest), pb.CreatedAt); err != nil {
		// UNIQUE(namespace_id, slug)：同命名空间里 slug 是身份（409），
		// 不是"顺手改成 pr-review-2"能糊过去的事。
		if isUniqueConstraintError(err) {
			return fmt.Errorf("%w: %q", ErrPlaybookSlugTaken, pb.Slug)
		}
		return fmt.Errorf("insert playbook: %w", err)
	}
	return nil
}

// GetPlaybook 按 id 或 slug 取预案（id 优先）；未命中返回 ErrPlaybookNotFound。
//
// 作用域是单个命名空间（I11）：别的命名空间的 id / slug 在这里等同于不存在。
func (d *DB) GetPlaybook(ctx context.Context, namespaceID, idOrSlug string) (*Playbook, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	if idOrSlug == "" {
		return nil, ErrPlaybookNotFound
	}
	pb, err := scanPlaybook(d.QueryRowContext(ctx,
		`SELECT `+playbookColumns+` FROM playbooks
		  WHERE namespace_id = ? AND (id = ? OR slug = ?)
		  ORDER BY CASE WHEN id = ? THEN 0 ELSE 1 END
		  LIMIT 1`, namespaceID, idOrSlug, idOrSlug, idOrSlug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPlaybookNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get playbook: %w", err)
	}
	return pb, nil
}

// ListPlaybooks 列出命名空间内的全部预案（按 created_at、id 升序；空列表返回 []）。
//
// 升序而不是倒序：mock 的卡片顺序就是创建顺序（最早的在前），
// 换序会让前端在两个实现间看起来"数据不一样"。
func (d *DB) ListPlaybooks(ctx context.Context, namespaceID string) ([]*Playbook, error) {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	rows, err := d.QueryContext(ctx,
		`SELECT `+playbookColumns+` FROM playbooks
		  WHERE namespace_id = ? ORDER BY created_at, id`, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list playbooks: %w", err)
	}
	defer rows.Close()
	out := []*Playbook{}
	for rows.Next() {
		pb, err := scanPlaybook(rows)
		if err != nil {
			return nil, fmt.Errorf("scan playbook row: %w", err)
		}
		out = append(out, pb)
	}
	return out, rows.Err()
}

// DeletePlaybook 删预案（按 id 或 slug）；未命中返回 ErrPlaybookNotFound。
//
// **只删预案行**（D14）：已经装出去的能力 / 边 / 源照样留着 ——
// 卸载一份清单不回滚数据，否则点一下"删除"就会拆掉正在跑的流程。
func (d *DB) DeletePlaybook(ctx context.Context, namespaceID, idOrSlug string) error {
	if namespaceID == "" || namespaceID == AllNamespaces {
		return ErrNamespaceRequired
	}
	res, err := d.ExecContext(ctx,
		`DELETE FROM playbooks WHERE namespace_id = ? AND (id = ? OR slug = ?)`,
		namespaceID, idOrSlug, idOrSlug)
	if err != nil {
		return fmt.Errorf("delete playbook: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete playbook rows: %w", err)
	}
	if n == 0 {
		return ErrPlaybookNotFound
	}
	return nil
}

// ===== 就绪体检 =====

// 体检结论的四态（D6：issue 文字里的 ready|incomplete 是过时草稿）。
const (
	PlaybookStatusReady     = "ready"      // 全齐
	PlaybookStatusBlocked   = "blocked"    // 有能力的**定义**都没装（要先补定义）
	PlaybookStatusNeedsNode = "needs-node" // 定义齐了，但没有机器能跑（要加机器 / 连上线）
	PlaybookStatusPartial   = "partial"    // 能力齐了，边或触发器还没补
)

// 体检项的状态。no-node 与 offline **必须分开**：前者要去写节点清单装机器，
// 后者只需要把机器连上 —— 合成一个"未就绪"会让人做错事。
const (
	PlaybookCheckOK      = "ok"
	PlaybookCheckMissing = "missing"
	PlaybookCheckNoNode  = "no-node"
	PlaybookCheckOffline = "offline"
)

// 体检项的三类（按依赖顺序：能力定义 → 节点实现 → 出口 → 触发器）。
const (
	PlaybookItemCapability = "capability"
	PlaybookItemEdge       = "edge"
	PlaybookItemTrigger    = "trigger"
)

// PlaybookCounts 是三类项的数量（mock 的卡片用它显示「能力 3 · 出口 3 · 触发器 1」）。
type PlaybookCounts struct {
	Capabilities int `json:"capabilities"`
	Edges        int `json:"edges"`
	Triggers     int `json:"triggers"`
}

// PlaybookCheckNode 是声明了某能力的机器（含 draining / offline）。
type PlaybookCheckNode struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PlaybookCheckItem 是体检的一项。
//
// 字段按类复用（就像 mock 的返回对象）：能力项用 ref/capId/outputs/runner/nodes，
// 边项用 label/from/fromPort/to/fromId/toId/blockedByCap，
// 触发项用 label/declaredKind/entryCaps。
type PlaybookCheckItem struct {
	Kind   string `json:"kind"`   // capability|edge|trigger
	Status string `json:"status"` // ok|missing|no-node|offline
	Ref    string `json:"ref,omitempty"`
	Label  string `json:"label,omitempty"`

	Outputs []string            `json:"outputs,omitempty"`
	CapID   string              `json:"capId,omitempty"`
	Runner  json.RawMessage     `json:"runner,omitempty"`
	Nodes   []PlaybookCheckNode `json:"nodes,omitempty"`

	BlockedByCap string `json:"blockedByCap,omitempty"`
	From         string `json:"from,omitempty"`
	FromPort     string `json:"fromPort,omitempty"`
	To           string `json:"to,omitempty"`
	FromID       string `json:"fromId,omitempty"`
	ToID         string `json:"toId,omitempty"`

	EntryCaps    []string `json:"entryCaps,omitempty"`
	DeclaredKind string   `json:"declaredKind,omitempty"`
}

// PlaybookChecks 是一次体检的完整结论（**推导量，不落库**）。
type PlaybookChecks struct {
	Status string `json:"status"`
	// Next 是「最该先做的一件事」= 按依赖顺序第一个没过的项；全 ok 时是 null。
	Next   *PlaybookCheckItem  `json:"next"`
	Ready  int                 `json:"ready"`
	Total  int                 `json:"total"`
	Counts PlaybookCounts      `json:"counts"`
	Items  []PlaybookCheckItem `json:"items"`
}

// PlaybookChecks 算一份预案的就绪体检（口径逐条对应 mock 的 pbChecks，D6）。
//
// 纯读：能力定义 / 边 / 源各取一次；机器只在需要时按能力查
// （NodesDeclaringCapability 走 idx_nodecap_cap，而预案里的能力项最多几十条，
// 比把整张 node_capabilities 捞出来再分组更省）。
func (d *DB) PlaybookChecks(ctx context.Context, pb *Playbook) (*PlaybookChecks, error) {
	if pb == nil {
		return nil, fmt.Errorf("playbook checks: nil playbook")
	}
	if pb.NamespaceID == "" || pb.NamespaceID == AllNamespaces {
		return nil, ErrNamespaceRequired
	}
	m := pb.parsedManifest()

	caps, err := d.ListCapabilities(ctx, pb.NamespaceID)
	if err != nil {
		return nil, fmt.Errorf("playbook checks: %w", err)
	}
	edges, err := d.ListEdges(ctx, pb.NamespaceID, "")
	if err != nil {
		return nil, fmt.Errorf("playbook checks: %w", err)
	}
	sources, err := d.ListSources(ctx, pb.NamespaceID)
	if err != nil {
		return nil, fmt.Errorf("playbook checks: %w", err)
	}

	// 名字 → 定义：规则只有一份（resolveManifestCap：D7 声明版本优先 → M3 同内容
	// 复用 → 版本号最小兜底），与安装期 InstallPlaybook 共用。两侧口径一旦分叉就会
	// 出现「install 说 created、体检说 missing」这种自相矛盾的结论：「x@2 与 x@1
	// 同内容」时安装把边连在 x@1 上，体检若说 x@2 没定义就会永久 blocked 且补不上。
	resolver := listCapResolver{caps: caps}
	declared := declaredCaps(m)

	items := make([]PlaybookCheckItem, 0, len(m.Capabilities)+len(m.Edges)+len(m.Triggers))

	// ① 能力定义 + ② 节点实现（绑在同一项上：第二项依赖第一项）
	for _, c := range m.Capabilities {
		ref := fmt.Sprintf("%s@%d", c.Name, c.Version)
		outputs := c.Outputs
		if outputs == nil {
			outputs = []string{}
		}
		// 能力项自带版本号：精确命中声明版本，否则复用同内容的那条（I8）。
		// 命中复用那条时 Status / CapID 都按**复用的定义**算，Ref 仍是声明里的 x@2。
		def, _, derr := resolveManifestCap(pb.NamespaceID, &c, c.Name, false, resolver)
		if derr != nil {
			return nil, fmt.Errorf("playbook checks: %w", derr)
		}
		if def == nil {
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemCapability, Status: PlaybookCheckMissing, Ref: ref,
				Outputs: outputs, Runner: c.Runner,
			})
			continue
		}
		owners, err := d.NodesDeclaringCapability(ctx, pb.NamespaceID, def.ID)
		if err != nil {
			return nil, fmt.Errorf("playbook checks: %w", err)
		}
		if len(owners) == 0 {
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemCapability, Status: PlaybookCheckNoNode, Ref: ref,
				Outputs: def.Outputs, CapID: def.ID, Runner: c.Runner,
			})
			continue
		}
		nodes := make([]PlaybookCheckNode, 0, len(owners))
		online := 0
		for _, n := range owners {
			if n.Status == NodeStatusOnline {
				online++
			}
			nodes = append(nodes, PlaybookCheckNode{Name: n.Name, Status: n.Status})
		}
		status := PlaybookCheckOK
		if online == 0 {
			status = PlaybookCheckOffline
		}
		items = append(items, PlaybookCheckItem{
			Kind: PlaybookItemCapability, Status: status, Ref: ref,
			Outputs: def.Outputs, CapID: def.ID, Runner: c.Runner, Nodes: nodes,
		})
	}

	// ③ 出口：两端定义都在才谈得上"这条边在不在"
	for _, e := range m.Edges {
		label := fmt.Sprintf("%s --%s--> %s", e.From, e.FromPort, e.To)
		from, _, ferr := resolveManifestCap(pb.NamespaceID, capAt(declared, e.From), e.From, true, resolver)
		if ferr != nil {
			return nil, fmt.Errorf("playbook checks: %w", ferr)
		}
		to, _, terr := resolveManifestCap(pb.NamespaceID, capAt(declared, e.To), e.To, true, resolver)
		if terr != nil {
			return nil, fmt.Errorf("playbook checks: %w", terr)
		}
		if from == nil || to == nil {
			// 端点缺定义 → 这条边现在补不了，先补能力（blockedByCap 说清缺哪个）
			missing := e.To
			if from == nil {
				missing = e.From
			}
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemEdge, Status: PlaybookCheckMissing,
				Label: label, BlockedByCap: missing,
			})
			continue
		}
		hit := false
		for _, x := range edges {
			if x.FromIsSource {
				continue // 入口边的左端是来源名，不参与能力之间的匹配
			}
			if x.FromCapability == from.ID && x.FromPort == e.FromPort && x.ToCapability == to.ID {
				hit = true
				break
			}
		}
		status := PlaybookCheckMissing
		if hit {
			status = PlaybookCheckOK
		}
		items = append(items, PlaybookCheckItem{
			Kind: PlaybookItemEdge, Status: status, Label: label,
			From: e.From, FromPort: e.FromPort, To: e.To, FromID: from.ID, ToID: to.ID,
		})
	}

	// ④ 触发器：kind 匹配**且**这个源的目标里有本预案的入口能力。
	//
	// 只匹配 kind 会把"你有一个 webhook 源"当成"这条流程有触发器了"，
	// 而那个源可能指向完全无关的能力。
	toSet := make(map[string]bool, len(m.Edges))
	for _, e := range m.Edges {
		toSet[e.To] = true
	}
	entryNames := make([]string, 0, len(m.Capabilities))
	entryIDs := make([]string, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if toSet[c.Name] {
			continue // 是某条边的下游 → 不是入口
		}
		entryNames = append(entryNames, c.Name)
		// 入口能力也要按同一规则解析（安装期建源时 targets 指向的就是它）。
		def, _, derr := resolveManifestCap(pb.NamespaceID, &c, c.Name, true, resolver)
		if derr != nil {
			return nil, fmt.Errorf("playbook checks: %w", derr)
		}
		if def != nil {
			entryIDs = append(entryIDs, def.ID)
		}
	}
	for _, t := range m.Triggers {
		hit := false
		for _, src := range sources {
			if src.Kind != t.Kind {
				continue
			}
			if sourceTargetsAny(src, entryIDs) {
				hit = true
				break
			}
		}
		label := t.Kind
		if len(t.Types) > 0 {
			label += " · " + strings.Join(t.Types, " / ")
		}
		status := PlaybookCheckMissing
		if hit {
			status = PlaybookCheckOK
		}
		items = append(items, PlaybookCheckItem{
			Kind: PlaybookItemTrigger, Status: status, Label: label,
			EntryCaps: entryNames, DeclaredKind: t.Kind,
		})
	}

	// ⑤ 总体结论 + next + 计数
	checks := &PlaybookChecks{
		Items: items,
		Total: len(items),
		Counts: PlaybookCounts{
			Capabilities: len(m.Capabilities),
			Edges:        len(m.Edges),
			Triggers:     len(m.Triggers),
		},
	}
	capMissing, capNoNode, restBad := false, false, false
	for i := range items {
		it := &items[i]
		if it.Status == PlaybookCheckOK {
			checks.Ready++
		} else if checks.Next == nil {
			checks.Next = it
		}
		if it.Kind == PlaybookItemCapability {
			switch it.Status {
			case PlaybookCheckMissing:
				capMissing = true
			case PlaybookCheckNoNode, PlaybookCheckOffline:
				capNoNode = true
			}
			continue
		}
		if it.Status != PlaybookCheckOK {
			restBad = true
		}
	}
	switch {
	case capMissing:
		checks.Status = PlaybookStatusBlocked
	case capNoNode:
		checks.Status = PlaybookStatusNeedsNode
	case restBad:
		checks.Status = PlaybookStatusPartial
	default:
		checks.Status = PlaybookStatusReady
	}
	return checks, nil
}

// sourceTargetsAny 报告源的目标里是否含 ids 中的任意一个。
func sourceTargetsAny(s *Source, ids []string) bool {
	for _, id := range ids {
		for _, t := range s.Targets {
			if t.CapabilityID == id {
				return true
			}
		}
	}
	return false
}

// ===== 视图 =====

// PlaybookView 是列表项：预案本身 + 体检结论（**不带** items，省一半响应体积）。
type PlaybookView struct {
	*Playbook
	Status string             `json:"status"`
	Next   *PlaybookCheckItem `json:"next"`
	Ready  int                `json:"ready"`
	Total  int                `json:"total"`
	Counts PlaybookCounts     `json:"counts"`
}

// PlaybookDetail 是详情：视图 + 逐项体检结果（无项时是 []）。
type PlaybookDetail struct {
	*PlaybookView
	Items []PlaybookCheckItem `json:"items"`
}

// playbookView 把预案与体检结论拼成列表项。
func playbookView(pb *Playbook, checks *PlaybookChecks) *PlaybookView {
	return &PlaybookView{
		Playbook: pb,
		Status:   checks.Status,
		Next:     checks.Next,
		Ready:    checks.Ready,
		Total:    checks.Total,
		Counts:   checks.Counts,
	}
}

// GetPlaybookDetail 取详情（handler 直接用，列表 / 创建 / 安装后刷新都走它）。
func (d *DB) GetPlaybookDetail(ctx context.Context, namespaceID, idOrSlug string) (*PlaybookDetail, error) {
	pb, err := d.GetPlaybook(ctx, namespaceID, idOrSlug)
	if err != nil {
		return nil, err
	}
	checks, err := d.PlaybookChecks(ctx, pb)
	if err != nil {
		return nil, err
	}
	items := checks.Items
	if items == nil {
		items = []PlaybookCheckItem{}
	}
	return &PlaybookDetail{PlaybookView: playbookView(pb, checks), Items: items}, nil
}

// ListPlaybookViews 列出命名空间的预案视图（空列表返回 []）。
func (d *DB) ListPlaybookViews(ctx context.Context, namespaceID string) ([]*PlaybookView, error) {
	pbs, err := d.ListPlaybooks(ctx, namespaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*PlaybookView, 0, len(pbs))
	for _, pb := range pbs {
		checks, err := d.PlaybookChecks(ctx, pb)
		if err != nil {
			return nil, err
		}
		out = append(out, playbookView(pb, checks))
	}
	return out, nil
}

// ===== 幂等安装 =====

// PlaybookInstallSet 是一次安装里"装了什么 / 跳过了什么"（三数组空则 []）。
type PlaybookInstallSet struct {
	Capabilities []string `json:"capabilities"`
	Edges        []string `json:"edges"`
	Sources      []string `json:"sources"`
}

// PlaybookInstallResult 是安装结果。
//
// Notes 是人话说明，只在"跳过有原因可讲"时出现（缺定义 / 端口不在 outputs /
// 同内容已是别的版本 / 已有同 kind 的源）。纯粹的"早装过了"不写 note：
// 幂等的第二次点击不该产出一屏告警。
type PlaybookInstallResult struct {
	Created PlaybookInstallSet `json:"created"`
	Skipped PlaybookInstallSet `json:"skipped"`
	Notes   []string           `json:"notes"`
}

// emptyPlaybookInstallSet 返回三条空数组（D13：不是 null）。
func emptyPlaybookInstallSet() PlaybookInstallSet {
	return PlaybookInstallSet{Capabilities: []string{}, Edges: []string{}, Sources: []string{}}
}

// capLookup 是安装期需要的能力定义片段（不取整行：只为了判存在与查出口）。
type capLookup struct {
	ID      string
	Name    string
	Version int
	Outputs []string
}

// lookupCapByNameVersion 按 (name, version) 查定义；未命中返回 nil, nil。
func lookupCapByNameVersion(ctx context.Context, q nsQueryer, namespaceID, name string, version int) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?`, namespaceID, name, version))
}

// lookupCapByName 按名字查定义，取**版本号最小**的那条（D7：mock 的 findCap 口径）；
// 未命中返回 nil, nil。
func lookupCapByName(ctx context.Context, q nsQueryer, namespaceID, name string) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs FROM capability_versions
		  WHERE namespace_id = ? AND name = ? ORDER BY version, id LIMIT 1`, namespaceID, name))
}

// lookupCapByHash 按 (namespace_id, content_hash) 查定义；未命中返回 nil, nil。
func lookupCapByHash(ctx context.Context, q nsQueryer, namespaceID, hash string) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs FROM capability_versions
		  WHERE namespace_id = ? AND content_hash = ?`, namespaceID, hash))
}

// scanCapLookup 解析一行能力片段；未命中返回 nil, nil。
func scanCapLookup(row *sql.Row) (*capLookup, error) {
	var (
		c       capLookup
		outputs string
	)
	err := row.Scan(&c.ID, &c.Name, &c.Version, &outputs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup capability: %w", err)
	}
	if err := json.Unmarshal([]byte(outputs), &c.Outputs); err != nil {
		return nil, fmt.Errorf("decode capability outputs: %w", err)
	}
	return &c, nil
}

// ===== manifest 名字 → 定义（体检与安装共用的唯一一份规则，D7 + M3）=====

// pickManifestCap 按固定优先级挑一条定义，并给出"为什么不是声明版本"的人话原因
// （空串 = 就是声明版本，或本来就没有声明版本可丢）。
//
// 优先级（全仓唯一一处把这件事说清楚的地方，体检与安装共用）：
//
//  1. declared：manifest 声明的 (name, version) **精确命中**
//  2. sameContent：声明的那份定义**按 content_hash 命中**已有定义 —— 同内容就是
//     同一份能力，只是版本号不同（I8：绝不写第二行同内容定义），复用它
//  3. byName：该名字在命名空间里**版本号最小**的定义（mock 的 findCap 口径）
func pickManifestCap(declared, sameContent, byName *capLookup) (cap *capLookup, why string) {
	switch {
	case declared != nil:
		return declared, ""
	case sameContent != nil:
		return sameContent, fmt.Sprintf(
			"已复用同内容的 %s@%d（I8：同内容不产生第二行定义）",
			sameContent.Name, sameContent.Version)
	case byName != nil:
		return byName, ""
	}
	return nil, ""
}

// capResolver 是「按三种口径查已有定义」的最小接口：安装期由事务里的查询实现，
// 体检侧由内存里的能力列表实现 —— 同一份优先级规则因此能跑在两边。
type capResolver interface {
	byNameVersion(name string, version int) (*capLookup, error)
	byContentHash(hash string) (*capLookup, error)
	byName(name string) (*capLookup, error)
}

// resolveManifestCap 是「manifest 里的名字该用哪条定义」的**唯一**实现（D7 + M3）。
//
// declared 非 nil 表示 manifest 声明了这份定义（只有名字的引用传 nil）；
// allowByName 控制第三步兜底是否启用。
//
// 为什么两个参数都要：能力项（manifest 的 capabilities[]）**自带版本号**，
// 「x@2 还没装」不该被 x@1 的存在掩盖成 ok —— x@1 的出口可能与 x@2 不同，
// 边会因此接到错的定义上。而边的两端 / 入口能力**只写了名字**，没有版本信息
// 可用，才需要「版本号最小」那条兜底（mock 的 findCap 取第一条）。
func resolveManifestCap(namespaceID string, declared *ManifestCapability, name string, allowByName bool, r capResolver) (*capLookup, string, error) {
	var (
		exact, sameContent, byName *capLookup
		err                        error
	)
	if declared != nil {
		if exact, err = r.byNameVersion(name, declared.Version); err != nil {
			return nil, "", err
		}
		if exact == nil {
			hash, herr := manifestCapHash(namespaceID, *declared)
			if herr != nil {
				return nil, "", herr
			}
			if sameContent, err = r.byContentHash(hash); err != nil {
				return nil, "", err
			}
		}
	}
	if allowByName && exact == nil && sameContent == nil {
		if byName, err = r.byName(name); err != nil {
			return nil, "", err
		}
	}
	cap, why := pickManifestCap(exact, sameContent, byName)
	return cap, why, nil
}

// asCapLookup 把整行定义收成解析片段（体检侧用）。
func asCapLookup(c *CapabilityVersion) *capLookup {
	if c == nil {
		return nil
	}
	return &capLookup{ID: c.ID, Name: c.Name, Version: c.Version, Outputs: c.Outputs}
}

// listCapResolver 在内存里的能力列表上实现三种查法（体检侧：一次 ListCapabilities
// 之后纯内存比对，不再逐项打库）。
type listCapResolver struct {
	caps []*CapabilityVersion
}

func (r listCapResolver) byNameVersion(name string, version int) (*capLookup, error) {
	for _, c := range r.caps {
		if c.Name == name && c.Version == version {
			return asCapLookup(c), nil
		}
	}
	return nil, nil
}

func (r listCapResolver) byContentHash(hash string) (*capLookup, error) {
	if hash == "" {
		return nil, nil
	}
	for _, c := range r.caps {
		if c.ContentHash == hash {
			return asCapLookup(c), nil
		}
	}
	return nil, nil
}

// byName 取名字命中的第一条：caps 按 (name, version) 排序（ListCapabilities），
// 所以第一条就是版本号最小的那条，与 SQL 侧 ORDER BY version 同一口径。
func (r listCapResolver) byName(name string) (*capLookup, error) {
	for _, c := range r.caps {
		if c.Name == name {
			return asCapLookup(c), nil
		}
	}
	return nil, nil
}

// sqlCapResolver 在 SQL 上实现三种查法（安装期，跑在安装事务里）。
type sqlCapResolver struct {
	ctx         context.Context
	q           nsQueryer
	namespaceID string
}

func (r sqlCapResolver) byNameVersion(name string, version int) (*capLookup, error) {
	return lookupCapByNameVersion(r.ctx, r.q, r.namespaceID, name, version)
}

func (r sqlCapResolver) byContentHash(hash string) (*capLookup, error) {
	if hash == "" {
		return nil, nil
	}
	return lookupCapByHash(r.ctx, r.q, r.namespaceID, hash)
}

func (r sqlCapResolver) byName(name string) (*capLookup, error) {
	return lookupCapByName(r.ctx, r.q, r.namespaceID, name)
}

// edgeExists 报告能力之间那条边是否已存在（入口边不参与）。
func edgeExists(ctx context.Context, q nsQueryer, namespaceID, fromID, fromPort, toID string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM edges
		  WHERE namespace_id = ? AND from_is_source = 0
		    AND from_key = ? AND from_port = ? AND to_capability = ?`,
		namespaceID, fromID, fromPort, toID).Scan(&n); err != nil {
		return false, fmt.Errorf("probe edge: %w", err)
	}
	return n > 0, nil
}

// sourceNameExists 返回同名源的名字（未命中返回 ""）。
func sourceNameExists(ctx context.Context, q nsQueryer, namespaceID, name string) (string, error) {
	var got string
	err := q.QueryRowContext(ctx,
		`SELECT name FROM sources WHERE namespace_id = ? AND name = ?`, namespaceID, name).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("probe source name: %w", err)
	}
	return got, nil
}

// sourceWithKindTargets 找「同 kind 且 targets 与本预案入口能力有交集」的源（未命中返回 ""）。
//
// 为什么光看名字不够：用户可能早就有一个人工建的 webhook 源指向同一个入口能力。
// 再装一个同 kind 的源 = 同一个 PR 事件被派发两次（重复干活）。所以第二条判据
// 按"能不能派到同一个入口"来认（mock 的口径）。
func sourceWithKindTargets(ctx context.Context, q nsQueryer, namespaceID, kind string, capIDs []string) (string, error) {
	if len(capIDs) == 0 {
		return "", nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(capIDs)), ",")
	args := make([]any, 0, len(capIDs)+2)
	args = append(args, namespaceID, kind)
	for _, id := range capIDs {
		args = append(args, id)
	}
	var got string
	err := q.QueryRowContext(ctx,
		`SELECT s.name FROM sources s
		   JOIN source_targets st ON st.source_id = s.id
		  WHERE s.namespace_id = ? AND s.kind = ? AND st.capability_id IN (`+marks+`)
		  ORDER BY s.created_at, s.name LIMIT 1`, args...).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("probe same-kind source: %w", err)
	}
	return got, nil
}

// triggerConfig 把 manifest 的触发器翻译成源的初始配置（D11）。
//
// 返回 ok=false 表示这份触发器**没法变成一个源**（cron 没给表达式）：
// 安装期只能 skip + note，不能凭空造一个"每天 3 点"出来。
func triggerConfig(t ManifestTrigger) (json.RawMessage, bool) {
	switch t.Kind {
	case SourceKindCron:
		if len(t.Types) == 0 || strings.TrimSpace(t.Types[0]) == "" {
			return nil, false
		}
		// types[0] 就是五段表达式本身，原样写进 config.expr（tz 交给校验补 Local）
		b, err := json.Marshal(CronConfig{Expr: t.Types[0], TZ: "Local"})
		if err != nil {
			return nil, false
		}
		return b, true
	case SourceKindWebhook, SourceKindGitHub, SourceKindManual:
		// token（webhook/github）由 ValidateSourceConfig 生成；manual 只有空配置。
		return json.RawMessage(`{}`), true
	default:
		return nil, false
	}
}

// InstallPlaybook 幂等安装：把 manifest 补齐成命名空间里的真实资源。
//
// 顺序必须是 **能力 → 边 → 源**（D8）：边两端没有定义就建不了；源没有入口能力
// 就没人可派。**单事务**：任何真错误整体回滚（半装状态比没装更难排障）。
// 唯一约束冲突不算真错误：SQLite 只回滚那一条语句，事务继续（契约 §1），
// 于是"同内容 / 早装过"都能记进 skipped 而不整体失败。
//
// 幂等判据（能力 / 边 / 源各一套，见 D9/D10/D11）：已存在的一律 skipped，
// **绝不覆盖**（I8 append-only）。
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, idOrSlug string) (*Playbook, *PlaybookInstallResult, error) {
	pb, err := d.GetPlaybook(ctx, namespaceID, idOrSlug)
	if err != nil {
		return nil, nil, err
	}
	m := pb.parsedManifest()
	res := &PlaybookInstallResult{
		Created: emptyPlaybookInstallSet(),
		Skipped: emptyPlaybookInstallSet(),
		Notes:   []string{},
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("install playbook: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := Now()

	// 名字 → 定义：规则只有一份（resolveManifestCap，与体检共用），这里跑在事务里。
	resolver := sqlCapResolver{ctx: ctx, q: tx, namespaceID: pb.NamespaceID}
	declared := declaredCaps(m)

	// ① 能力定义：append-only（I8）。已存在 (name,version) 一律 skipped。
	for _, c := range m.Capabilities {
		ref := fmt.Sprintf("%s@%d", c.Name, c.Version)
		existing, err := resolver.byNameVersion(c.Name, c.Version)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if existing != nil {
			res.Skipped.Capabilities = append(res.Skipped.Capabilities, ref)
			continue
		}

		def := &CapabilityVersion{
			NamespaceID:  pb.NamespaceID,
			OrgID:        pb.OrgID,
			Name:         c.Name,
			Version:      c.Version,
			ParamsSchema: c.ParamsSchema,
			Outputs:      c.Outputs,
			Source:       CapSourceCustom,
			CreatedAt:    now,
		}
		// hash 与「同内容复用」用同一份算法（manifestCapHash）：算出来的值必须能
		// 命中 capability_versions.content_hash，否则复用永远命不中。
		hash, err := manifestCapHash(pb.NamespaceID, c)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if def.Outputs == nil {
			def.Outputs = []string{}
		}
		def.ID = NewCapabilityID(hash)
		def.ContentHash = hash
		outputsJSON, err := json.Marshal(def.Outputs)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: marshal outputs: %w", err)
		}
		schema := string(def.ParamsSchema)
		if schema == "" {
			schema = "{}"
		}
		lifecycleJSON, err := json.Marshal(def.Lifecycle)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: marshal lifecycle: %w", err)
		}
		_, insErr := tx.ExecContext(ctx,
			`INSERT INTO capability_versions
			   (id, namespace_id, org_id, name, display_name, version, params_schema, outputs, lifecycle, sticky, source, content_hash, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			def.ID, def.NamespaceID, def.OrgID, def.Name, def.DisplayName, def.Version,
			schema, string(outputsJSON), string(lifecycleJSON), def.Sticky, def.Source,
			def.ContentHash, def.CreatedAt)
		if insErr == nil {
			res.Created.Capabilities = append(res.Created.Capabilities, ref)
			continue
		}
		if !isUniqueConstraintError(insErr) {
			return nil, nil, fmt.Errorf("install playbook: insert capability %s: %w", ref, insErr)
		}
		// 唯一冲突本身不是真错误：单条 INSERT 失败只回滚这条语句，事务还能继续。
		same, lerr := resolver.byContentHash(hash)
		if lerr != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", lerr)
		}
		res.Skipped.Capabilities = append(res.Skipped.Capabilities, ref)
		if same != nil {
			// D9：同内容不产生第二行 —— 记 skipped 并说清"内容与哪个版本相同"。
			res.Notes = append(res.Notes, fmt.Sprintf(
				"能力 %s 跳过：命名空间里已有同内容的 %s@%d（同内容不产生第二行定义）",
				ref, same.Name, same.Version))
		} else {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"能力 %s 跳过：同名同版本的定义已存在（定义 append-only，不覆盖）", ref))
		}
	}

	// resolveCap 解析 manifest 里**只写了名字**的引用（边的两端 / 入口能力）该连哪条
	// 定义：唯一规则在 resolveManifestCap（D7 声明版本优先 → M3 同内容复用 →
	// 版本号最小兜底），与体检共用。why 非空表示没连在声明版本上（同内容复用），
	// 要写进 notes 说清原因。
	resolveCap := func(name string) (*capLookup, string, error) {
		return resolveManifestCap(pb.NamespaceID, capAt(declared, name), name, true, resolver)
	}

	// ② 边：两端都要有定义；from 的出口必须含 fromPort（I2）。
	// 单条边不成立只 skip 它自己，其余照装（不整体回滚）。
	for _, e := range m.Edges {
		label := fmt.Sprintf("%s --%s--> %s", e.From, e.FromPort, e.To)
		from, fromWhy, err := resolveCap(e.From)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		to, toWhy, err := resolveCap(e.To)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if from == nil || to == nil {
			missing := e.To
			if from == nil {
				missing = e.From
			}
			res.Skipped.Edges = append(res.Skipped.Edges, label)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"边 %s 跳过：能力「%s」在本命名空间里没有定义（补齐能力后再装一次即可）", label, missing))
			continue
		}
		// 同内容复用要讲清楚（D9 + M3）：端点不是凭空长出来的，而是「声明版本找不到、
		// 复用了同内容的那条」。否则用户看到「边装好了」却不知道为何不是 x@2。
		// why 非空当且仅当该名字在 manifest 里声明过，所以 declared 里一定有它。
		if fromWhy != "" {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"边 %s 的一端「%s」按声明的 %s@%d 找不到定义，%s",
				label, e.From, e.From, declared[e.From].Version, fromWhy))
		}
		if toWhy != "" {
			res.Notes = append(res.Notes, fmt.Sprintf(
				"边 %s 的一端「%s」按声明的 %s@%d 找不到定义，%s",
				label, e.To, e.To, declared[e.To].Version, toWhy))
		}
		if !containsString(from.Outputs, e.FromPort) {
			res.Skipped.Edges = append(res.Skipped.Edges, label)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"边 %s 跳过：出口「%s」不在 %s@%d 的 outputs %v 里（出口是封闭集合）",
				label, e.FromPort, from.Name, from.Version, from.Outputs))
			continue
		}
		exists, err := edgeExists(ctx, tx, pb.NamespaceID, from.ID, e.FromPort, to.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if exists {
			res.Skipped.Edges = append(res.Skipped.Edges, label)
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO edges (namespace_id, org_id, from_is_source, from_key, from_port, to_capability, created_at)
			 VALUES (?,?,?,?,?,?,?)`,
			pb.NamespaceID, pb.OrgID, 0, from.ID, e.FromPort, to.ID, now); err != nil {
			return nil, nil, fmt.Errorf("install playbook: insert edge %s: %w", label, err)
		}
		res.Created.Edges = append(res.Created.Edges, label)
	}

	// ③ 源：先把入口能力（不作为任何边 to 的能力）解析出来 —— 没有入口就没有
	// 可派的目标，建出来的源永远不会产生任务。
	toSet := make(map[string]bool, len(m.Edges))
	for _, e := range m.Edges {
		toSet[e.To] = true
	}
	entryIDs := make([]string, 0, len(m.Capabilities))
	entrySeen := make(map[string]bool, len(m.Capabilities))
	entryNames := make([]string, 0, len(m.Capabilities))
	for _, c := range m.Capabilities {
		if toSet[c.Name] {
			continue
		}
		def, _, derr := resolveCap(c.Name)
		if derr != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", derr)
		}
		if def == nil || entrySeen[def.ID] {
			continue
		}
		entrySeen[def.ID] = true
		entryIDs = append(entryIDs, def.ID)
		entryNames = append(entryNames, c.Name)
	}

	for i, t := range m.Triggers {
		// 源名 <slug>-<kind>-<idx>：slug 在命名空间内唯一 → 名字确定、可幂等比对
		// （mock 用「预案名 · kind」，改名就会漂，不能当幂等判据）。
		wantName := fmt.Sprintf("%s-%s-%d", pb.Slug, t.Kind, i)
		if len(entryIDs) == 0 {
			res.Skipped.Sources = append(res.Skipped.Sources, wantName)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"触发 %s 跳过：本预案没有可用的入口能力 %v（入口 = 不作为任何边目标的能力），没有可派的目标就不建源",
				t.Kind, entryNames))
			continue
		}
		// 幂等判据 ①：同名源已存在
		got, err := sourceNameExists(ctx, tx, pb.NamespaceID, wantName)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if got != "" {
			res.Skipped.Sources = append(res.Skipped.Sources, got)
			continue
		}
		// 幂等判据 ②：已有同 kind 且 targets 命中本预案入口能力的源
		got, err = sourceWithKindTargets(ctx, tx, pb.NamespaceID, t.Kind, entryIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if got != "" {
			res.Skipped.Sources = append(res.Skipped.Sources, got)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"源 %s 跳过：命名空间里已有同 kind（%s）且指向本预案入口能力的源「%s」（再建一个会让同一事件被派发两次）",
				wantName, t.Kind, got))
			continue
		}

		raw, ok := triggerConfig(t)
		if !ok {
			res.Skipped.Sources = append(res.Skipped.Sources, wantName)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"源 %s 跳过：%s 触发没有给出可用的配置（cron 需要 types[0] = 五段表达式）", wantName, t.Kind))
			continue
		}
		cfg, err := ValidateSourceConfig(t.Kind, raw)
		if err != nil {
			// 配置期校验没拦住的手改数据：skip + note，不让整包装不上（安装期只能 skip）。
			res.Skipped.Sources = append(res.Skipped.Sources, wantName)
			res.Notes = append(res.Notes, fmt.Sprintf("源 %s 跳过：%v", wantName, err))
			continue
		}

		// cron 的首触发时刻要现在就算好：等到进程重启才调度等于第一次不跑。
		var nextFire any
		if t.Kind == SourceKindCron {
			if at, err := NextCronTime(cfg, time.Unix(now, 0)); err == nil && !at.IsZero() {
				nextFire = at.Unix()
			}
		}
		srcID := NewSourceID()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sources (id, namespace_id, org_id, name, kind, config, enabled, last_fired_at, next_fire_at, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			srcID, pb.NamespaceID, pb.OrgID, wantName, t.Kind, string(cfg), 1, nil, nextFire, now); err != nil {
			return nil, nil, fmt.Errorf("install playbook: insert source %s: %w", wantName, err)
		}
		for _, capID := range entryIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO source_targets (source_id, capability_id, namespace_id, params, created_at)
				 VALUES (?,?,?,?,?)`,
				srcID, capID, pb.NamespaceID, "{}", now); err != nil {
				return nil, nil, fmt.Errorf("install playbook: insert source target %s: %w", wantName, err)
			}
		}
		res.Created.Sources = append(res.Created.Sources, wantName)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("install playbook: commit: %w", err)
	}
	return pb, res, nil
}
