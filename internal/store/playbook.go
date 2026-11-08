package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zzjcool/anotify/internal/artifact"
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

// ManifestCapability 是 manifest 声明的一个能力（Capability Spec v3 §12.1）。
//
// **三种声明形态，互斥**（由 parseManifestCap 强制，创建期 400）：
//
//	{ref: "review@2"}    引用已有定义：安装时**不建任何行**，只解析成落库引用
//	{spec: {…完整产物…}}  内联完整产物：装出来就是一份真能跑的能力
//	{name, version, …}    旧形态（v1）：等价于 handler: noop 的内联 spec（§12.3）
//
// 为什么旧形态不能删：v1 的 manifest 已经写进了真实库里（e2e 夹具、用户的预案），
// 装一次就报 400 等于让所有存量配置失效。它与回填口径一致（零产物 → noop 占位），
// 所以「旧形态 == 一条 handler:noop 的内联 spec」是等价变换，不是特例。
type ManifestCapability struct {
	// Ref 是已有定义的引用：`name` （跟随 latest）或 `name@version`（钉死某版）。
	Ref string `json:"ref,omitempty"`
	// Spec 是**内联的完整产物**（内联 spec 的合法实现形式见 §12.2 第 2 条：
	// 脚本形式，或 handler: noop）。原样解析、原样校验，安装时走与 POST /v1/capabilities
	// 同一条落库路径（createCapabilityTx）。
	Spec json.RawMessage `json:"spec,omitempty"`

	// 旧形态（v1）字段：**兼容形态**，等价于 handler: noop 的内联 spec。
	// 与 ref / spec 混用一律 400（两种声明互相矛盾时没有「谁赢」的正确答案）。
	Name         string          `json:"name,omitempty"`
	Version      int             `json:"version,omitempty"`
	Outputs      []string        `json:"outputs,omitempty"`
	ParamsSchema json.RawMessage `json:"paramsSchema,omitempty"`
	// Deprecated: 旧形态的 runner 只透传（中枢不解释它，见 D1）。
	Runner json.RawMessage `json:"runner,omitempty"`
}

// ManifestEdge 是 manifest 声明的一条边：按**名字**引用两端能力（D7）。
type ManifestEdge struct {
	From     string `json:"from"`
	FromPort string `json:"fromPort"`
	To       string `json:"to"`
	// Literals 是这条边的**常量覆盖**（语义与 edges.literals 完全一致，§12.1）。
	// 空 = 不覆盖；安装时原样落进 edges.literals。
	Literals json.RawMessage `json:"literals,omitempty"`
	// Priority 是同一 (能力, 端口) 下多个候选的优先级（小的优先，0 = 默认）。
	Priority int `json:"priority,omitempty"`
}

// ManifestTrigger 是 manifest 声明的一个触发器（§12.1）。
//
// 旧形态只有 Kind + Types（events）；v2 增加 Name / Config / Enabled / Targets：
// 触发器要能表达「源叫什么」「配置长什么样」「派给哪些入口、参数怎么拼」。
type ManifestTrigger struct {
	// Name 是安装出来的源名。省略时沿用 `<slug>-<kind>-<idx>`（旧口径不变）。
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	// Config 是源的配置（按 kind 解释，走 ValidateSourceConfig 同一套校验）。
	// 省略时按旧形态的 Types 翻译（cron 的 types[0] 是五段表达式）。
	Config json.RawMessage `json:"config,omitempty"`
	// Enabled 是源的启用状态；省略 = true（与 POST /v1/sources 的缺省一致）。
	Enabled *bool `json:"enabled,omitempty"`
	// Types 是旧形态的事件类型（cron 时 types[0] 就是五段表达式本身）。
	Types []string `json:"types,omitempty"`
	// Targets 是入口边（§12.1）：`to` 是入口能力名，`mapping` 是参数模板。
	// 省略时回落到旧口径「本预案的入口能力全集、mapping 为空」。
	Targets []ManifestTriggerTarget `json:"targets,omitempty"`
}

// ManifestTriggerTarget 是一个入口边：派给谁（`to`）+ 参数怎么拼（`mapping`）。
type ManifestTriggerTarget struct {
	To string `json:"to"`
	// Mapping 是入口边的引用模板（语义同 edges.mapping）：只有入口边允许。
	Mapping json.RawMessage `json:"mapping,omitempty"`
}

// isEnabled 返回触发器声明的启用状态（省略 = true）。
func (t ManifestTrigger) isEnabled() bool {
	return t.Enabled == nil || *t.Enabled
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

// ParsePlaybookManifest 解析并校验 manifest（D12 + Capability Spec v3 §12）。
//
// 空 / null → 三条空数组（不是错误：没写 manifest 等于「空预案」）。
// 校验错误一律包 ErrPlaybookManifestInvalid，且消息里带定位（`capabilities[1].spec` /
// `edges[1].fromPort`），因为配置期是唯一能把矛盾说清楚的机会（安装期只能 skip）。
//
// 这里做的是**纯形状校验**（不碰库）：ref 与 spec 恰好给一个、内联 spec 能过
// artifact.Parse + Validate、handler 只允许 noop、边端口落在已声明的出口里。
// 需要看库的那两条（ref 解析后引用到的端口、触发器的 targets 能解析到）在
// CreatePlaybook 的 validateManifestRefs 里做。
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

	// ① 能力声明：三种形态互斥、内联 spec 逐条过解析与校验（§12.2 第 1/2 条）。
	decls := parseManifestDecls(m)
	declaredOutputs := make(map[string][]string, len(decls))
	seen := make(map[string]bool, len(decls))
	for _, dc := range decls {
		where := fmt.Sprintf("capabilities[%d]", dc.Index)
		if dc.Err != nil {
			return out, fmt.Errorf("%w: %s: %v", ErrPlaybookManifestInvalid, where, dc.Err)
		}
		if playbookReservedCapNames[dc.Name] {
			return out, fmt.Errorf("%w: %s 声明的名字「%s」是内置事件来源的保留名（来源不是能力）",
				ErrPlaybookManifestInvalid, where, dc.Name)
		}
		// 幂等键是「名字 + 版本」：同名同版本声明两次没有「用哪一条」的答案。
		// ref 裸名字（跟随 latest）没有版本可比，所以它的 key 就是名字本身。
		key := dc.Name
		if dc.Version > 0 {
			key = fmt.Sprintf("%s@%d", dc.Name, dc.Version)
		}
		if seen[key] {
			return out, fmt.Errorf("%w: %s 与前面的声明重复（%s）", ErrPlaybookManifestInvalid, where, key)
		}
		seen[key] = true
		if _, ok := declaredOutputs[dc.Name]; !ok && dc.Kind != capKindRef {
			// 同名多版本取第一条（边的两端只写名字，名字重复时没有"该用哪个版本"的信息）。
			// ref 的出口要去库里查，不在纯解析阶段填（见 validateManifestRefs）。
			declaredOutputs[dc.Name] = dc.Outputs
		}
	}

	// ② 边：两端都写名字；常量覆盖必须是 JSON 对象（值类型要到安装期按目标 schema 判）。
	for i, e := range m.Edges {
		where := fmt.Sprintf("edges[%d]", i)
		if strings.TrimSpace(e.From) == "" || strings.TrimSpace(e.FromPort) == "" || strings.TrimSpace(e.To) == "" {
			return out, fmt.Errorf("%w: %s 的 from / fromPort / to 都不能为空", ErrPlaybookManifestInvalid, where)
		}
		if !isBlankJSON(e.Literals) {
			if _, ok := decodeJSONObject(e.Literals); !ok {
				return out, fmt.Errorf("%w: %s.literals 必须是 JSON 对象", ErrPlaybookManifestInvalid, where)
			}
		}
		// from 在本 manifest 里声明过（内联 / 旧形态）→ 端口必须落在它声明的出口里
		// （I2 的配置期版本）。ref 声明的端口要查库，见 validateManifestRefs。
		if outs, ok := declaredOutputs[e.From]; ok && !containsString(outs, e.FromPort) {
			return out, fmt.Errorf("%w: %s.fromPort「%s」不在 %s 的 outputs %v 里",
				ErrPlaybookManifestInvalid, where, e.FromPort, e.From, outs)
		}
	}

	// ③ 触发器：kind / 源名唯一 / config 能过校验。
	// 注意：config 在**创建期**就验（一个非法 cron 表达式不该等到安装才被发现）。
	// ValidateSourceConfig 是纯函数（webhook 的 token 只生成在返回值里，不落库），
	// 所以这里验一遍、安装期再验一遍是同一套规则、同一个结果。
	seenTriggerNames := make(map[string]bool, len(m.Triggers))
	for i, t := range m.Triggers {
		where := fmt.Sprintf("triggers[%d]", i)
		if strings.TrimSpace(t.Kind) == "" {
			return out, fmt.Errorf("%w: %s.kind 不能为空", ErrPlaybookManifestInvalid, where)
		}
		if !IsValidSourceKind(t.Kind) {
			return out, fmt.Errorf("%w: %s.kind「%s」不合法（可选 %s）",
				ErrPlaybookManifestInvalid, where, t.Kind, strings.Join(SourceKinds, " / "))
		}
		if name := strings.TrimSpace(t.Name); name != "" {
			if seenTriggerNames[name] {
				return out, fmt.Errorf("%w: %s.name「%s」与前面的触发器重名（源名在命名空间内唯一）",
					ErrPlaybookManifestInvalid, where, name)
			}
			seenTriggerNames[name] = true
		}
		if !isBlankJSON(t.Config) {
			if _, cerr := ValidateSourceConfig(t.Kind, t.Config); cerr != nil {
				return out, fmt.Errorf("%w: %s.config: %v", ErrPlaybookManifestInvalid, where, cerr)
			}
		}
		for j, tg := range t.Targets {
			if strings.TrimSpace(tg.To) == "" {
				return out, fmt.Errorf("%w: %s.targets[%d].to 不能为空", ErrPlaybookManifestInvalid, where, j)
			}
			if !isBlankJSON(tg.Mapping) {
				if _, ok := decodeJSONObject(tg.Mapping); !ok {
					return out, fmt.Errorf("%w: %s.targets[%d].mapping 必须是 JSON 对象",
						ErrPlaybookManifestInvalid, where, j)
				}
			}
		}
	}

	return m, nil
}

// manifest 能力声明的三种形态（capDecl.Kind）。取值会进 PlaybookCheckItem.Declared
// （前端据此显示「内联 spec」/「引用 name@ver」），所以是与前端的 wire 契约。
const (
	capKindInline = "inline" // 内联完整 spec
	capKindRef    = "ref"    // 引用已有定义（不建任何行）
	capKindLegacy = "legacy" // 旧形态 name+version（等价于 handler: noop 的内联 spec）
)

// 实现形式（PlaybookCheckItem.Impl 的 wire 值，Capability Spec v3 §3.1）。
const (
	playbookImplScript  = "script"  // run.files + entrypoint：脚本，任意 agent 节点执行
	playbookImplNoop    = "noop"    // handler: noop：合法占位，中枢不执行
	playbookImplHandler = "handler" // 其他白名单 handler（如 notify-phone）：中枢内置执行器
)

// specImpl 把一份正文的实现形式归成一句话（诊断/展示用，不参与任何决策）。
func specImpl(spec artifact.Spec) string {
	switch {
	case spec.Handler == artifact.HandlerNoop:
		return playbookImplNoop
	case spec.Handler != "":
		return playbookImplHandler
	case spec.Run.Entrypoint != "" || len(spec.Run.Files) > 0:
		return playbookImplScript
	}
	return ""
}

// capDecl 是 manifest 里一条能力声明的**归一形态**：
// 不管作者用的是 ref、内联 spec 还是旧形态，下游（体检 / 安装）只认这一种形状。
//
// 归一只有这一处（parseManifestCap）：三种形态各写一条路径的话，「装出来什么」
// 就会随作者选哪种写法而微微不同，而那种差异是查不出来的。
type capDecl struct {
	// Index 是它在 capabilities[] 里的下标（错误消息必须能指到人写的那一行）。
	Index int
	Name  string
	// Version 为 0 表示**跟随 latest**（只有裸名字 ref 会这样）。
	Version     int
	DisplayName string
	// Ref 非空 = 引用形态：安装不建任何行，只解析成落库引用。
	Ref string
	// Kind 是声明形态（inline | ref | legacy）。
	Kind string
	// Spec 是内联形态的正文（legacy 归一成 handler: noop 的占位）。
	Spec artifact.Spec
	// Outputs 是**声明的**出口（ref 形态未知，空）；安装/体检解析到定义后以定义为准。
	Outputs []string
	// Runner 是旧形态的原样透传（只给体检展示用，永不进正文）。
	Runner json.RawMessage
	// Err 非空 = 这条声明本身非法（读路径当缺失显示，写路径 400 带下标）。
	Err error
}

// Label 是给人看的标识：ref 形态原样（`notify.phone` / `review@2`），
// 其余是 `name@version`（与安装结果里 created.capabilities 的口径一致）。
func (d capDecl) Label() string {
	if d.Kind == capKindRef {
		return d.Ref
	}
	return fmt.Sprintf("%s@%d", d.Name, d.Version)
}

// parseManifestDecls 把 manifest 的能力声明逐条归一（**不返回错误**：坏的那条
// 带着自己的 Err，读路径可以只跳它而不弄坏整个预案页面；写路径在第一次遇到
// Err 时 400）。
func parseManifestDecls(m PlaybookManifest) []capDecl {
	out := make([]capDecl, 0, len(m.Capabilities))
	for i, c := range m.Capabilities {
		out = append(out, parseManifestCap(i, c))
	}
	return out
}

// legacyFieldsGiven 报告旧形态字段里有没有任何一个被写了。
//
// 用它把「ref / spec 与旧形态混用」拦在创建期：两种声明同时存在时，
// 「以哪个为准」没有一个能让人预测的答案，所以不做兼容、直接拒。
func legacyFieldsGiven(c ManifestCapability) bool {
	return strings.TrimSpace(c.Name) != "" || c.Version != 0 || len(c.Outputs) > 0 ||
		!isBlankJSON(c.ParamsSchema) || !isBlankJSON(c.Runner)
}

// parseManifestCap 是一条声明的**唯一**归一实现（三种形态 → capDecl）。
func parseManifestCap(index int, c ManifestCapability) capDecl {
	d := capDecl{Index: index}
	hasRef := strings.TrimSpace(c.Ref) != ""
	hasSpec := !isBlankJSON(c.Spec)

	// §12.2 第 1 条：ref 与 spec **恰好给一个**。
	if hasRef && hasSpec {
		d.Err = errors.New("ref 与 spec 恰好给一个（现在两个都给）")
		return d
	}
	if hasRef {
		if legacyFieldsGiven(c) {
			d.Err = errors.New("给了 ref 就不能再给 name / version / outputs / paramsSchema / runner")
			return d
		}
		name, version, err := parseCapRef(c.Ref)
		if err != nil {
			d.Err = err
			return d
		}
		d.Name, d.Version, d.Ref, d.Kind = name, version, strings.TrimSpace(c.Ref), capKindRef
		return d
	}

	if hasSpec {
		if legacyFieldsGiven(c) {
			d.Err = errors.New("给了 spec 就不能再给 name / version / outputs / paramsSchema / runner（名字与版本在 spec.metadata 里）")
			return d
		}
		a, err := parseInlineSpec(c.Spec)
		if err != nil {
			d.Err = err
			return d
		}
		d.Kind = capKindInline
		d.Name = a.Metadata.Name
		d.Version = a.Metadata.Version
		d.DisplayName = a.Metadata.DisplayName
		d.Spec = a.Spec
		d.Outputs = a.Spec.Outputs
		if d.Outputs == nil {
			d.Outputs = []string{}
		}
		return d
	}

	// 旧形态（v1）：等价于一条 handler: noop 的内联 spec（§12.3，与回填口径一致）。
	if strings.TrimSpace(c.Name) == "" {
		d.Err = errors.New("必须给 ref 或 spec（旧形态请给 name + version）")
		return d
	}
	if c.Version <= 0 {
		d.Err = fmt.Errorf("version 必须大于 0（%s@%d）", c.Name, c.Version)
		return d
	}
	if !isBlankJSON(c.ParamsSchema) {
		// 只判"是不是 JSON 对象"：中枢不解释 schema 内容（I1），
		// 但一个数组 / 标量塞进 paramsSchema 会在派发时变成无人能懂的失败。
		if _, ok := decodeJSONObject(c.ParamsSchema); !ok {
			d.Err = fmt.Errorf("paramsSchema 必须是 JSON 对象（%s@%d）", c.Name, c.Version)
			return d
		}
	}
	outputs := append([]string{}, c.Outputs...)
	d.Kind = capKindLegacy
	d.Name, d.Version = c.Name, c.Version
	d.Outputs = outputs
	d.Runner = c.Runner
	// 正文与回填口径完全一致：handler: noop + 原 paramsSchema + 原 outputs。
	d.Spec = HandlerSpec(artifact.HandlerNoop, c.ParamsSchema, outputs, nil)
	return d
}

// parseInlineSpec 解析并校验 manifest 里的内联完整产物（§12.2 第 2/3 条）。
//
// 两条硬规则：
//
//	① 必须能过 artifact.Parse（含 artifact.Validate：形状 / 端口 / 实现二选一）；
//	② handler 只允许 noop —— 否则一份配置文件就能把任意能力挂到中枢执行（I1 直接破掉）。
//	   要「链尾发推送」请 `ref: notify.phone`（那是平台能力，不是 manifest 能带来的）。
func parseInlineSpec(raw json.RawMessage) (*artifact.Artifact, error) {
	a, err := artifact.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("spec 不是一份合法产物：%v", err)
	}
	if h := a.Spec.Handler; h != "" && h != artifact.HandlerNoop {
		return nil, fmt.Errorf("spec 的 handler「%s」不被 manifest 接受（只允许 %q）："+
			"否则一份配置文件就能把能力挂到中枢执行（I1）；要链尾发推送请用 ref: notify.phone",
			h, artifact.HandlerNoop)
	}
	return a, nil
}

// parseCapRef 把 `name` / `name@version` 拆成 (name, version)；version=0 表示跟随 latest。
func parseCapRef(ref string) (string, int, error) {
	s := strings.TrimSpace(ref)
	if s == "" {
		return "", 0, errors.New("ref 不能为空")
	}
	name, verStr, hasVer := strings.Cut(s, "@")
	name = strings.TrimSpace(name)
	if name == "" {
		return "", 0, fmt.Errorf("ref「%s」缺少能力名", s)
	}
	if !hasVer {
		return name, 0, nil
	}
	version, err := strconv.Atoi(strings.TrimSpace(verStr))
	if err != nil || version <= 0 {
		return "", 0, fmt.Errorf("ref「%s」的版本号不合法（要 @正整数）", s)
	}
	return name, version, nil
}

// validateManifestRefs 是 manifest 的**第二条**创建期校验：两条要看库的规则
// （Capability Spec v3 §12.2 第 3 条）。
//
//	① 边的 fromPort 必须 ∈ **解析到的上游**的 outputs：内联 / 旧形态在
//	   ParsePlaybookManifest 里静态就判了，ref 的出口要到库里看（这就是本函数存在的原因）。
//	② 触发器的 targets[].to 必须能解析到（本 manifest 声明的，或命名空间里已有的）——
//	   入口边指向一个不存在的名字，装出来的源永远不会产生任务，而且没有任何报错。
//
// 刻意**不**在这里拦「ref 引用不到定义」：那一条由安装期拦（带下标 400，§12.2 第 3 条），
// 因为能力可能是别处先建、预案后装；创建期拦它等于逼用户按固定顿序操作。
// 如果 ref 此刻解析不到，①也跳过（没有出口信息可判）。
func (d *DB) validateManifestRefs(ctx context.Context, namespaceID string, m PlaybookManifest) error {
	// 两条规则都要求「manifest 里有声明」或「有触发器 targets」；都没有时不必读库。
	if len(m.Capabilities) == 0 && len(m.Triggers) == 0 {
		return nil
	}
	decls := parseManifestDecls(m)
	caps, err := d.ListCapabilities(ctx, namespaceID)
	if err != nil {
		return fmt.Errorf("validate playbook manifest: %w", err)
	}
	resolver := listCapResolver{caps: caps}
	declared := declaredDecls(m)

	// ① ref 声明的出口（内联 / 旧形态的出口已在纯解析阶段判过）。
	refOutputs := make(map[string][]string, len(decls))
	for _, dc := range decls {
		if dc.Kind != capKindRef || dc.Name == "" {
			continue
		}
		def, _, rerr := resolveDecl(namespaceID, dc, false, resolver)
		if rerr != nil {
			return fmt.Errorf("validate playbook manifest: %w", rerr)
		}
		if def != nil {
			refOutputs[dc.Name] = def.Outputs
		}
	}
	for i, e := range m.Edges {
		outs, ok := refOutputs[e.From]
		if !ok || containsString(outs, e.FromPort) {
			continue
		}
		return fmt.Errorf("%w: edges[%d].fromPort「%s」不在 ref 引用的 %s 的 outputs %v 里",
			ErrPlaybookManifestInvalid, i, e.FromPort, e.From, outs)
	}

	// ② 触发器的入口边目标。
	for i, t := range m.Triggers {
		for j, tg := range t.Targets {
			if declAt(declared, tg.To) != nil {
				continue
			}
			def, _, rerr := resolveManifestCap(tg.To, true, resolver)
			if rerr != nil {
				return fmt.Errorf("validate playbook manifest: %w", rerr)
			}
			if def == nil {
				return fmt.Errorf("%w: triggers[%d].targets[%d].to「%s」在本预案与命名空间里都没有定义",
					ErrPlaybookManifestInvalid, i, j, tg.To)
			}
		}
	}
	return nil
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

// declaredDecls 返回 manifest 自己声明的 name → 那条声明（同名多版本取第一条：
// 边的两端只写名字，名字重复时没有"该用哪个版本"的信息）。
//
// 体检与安装都从这里取「声明的是哪一版、声明形态是什么」：按名字解析声明的规则
// 必须只有一份（D7 + M3），各抄一个 map 早晚分叉。
func declaredDecls(m PlaybookManifest) map[string]capDecl {
	out := make(map[string]capDecl, len(m.Capabilities))
	for _, dc := range parseManifestDecls(m) {
		if _, ok := out[dc.Name]; !ok && dc.Name != "" {
			out[dc.Name] = dc
		}
	}
	return out
}

// declAt 取「manifest 有没有声明这个名字」的指针形态（没声明返回 nil）。
func declAt(declared map[string]capDecl, name string) *capDecl {
	if dc, ok := declared[name]; ok {
		return &dc
	}
	return nil
}

// declCapabilityVersion 把一条声明归一成一条**待写入**的能力行：正文已校验、
// 已 canonical 化、派生列已算好。
//
// 走的是与 POST /v1/capabilities **同一条**路（prepareCapabilitySpec + deriveFromSpec）：
// 自己拼 INSERT 会让预案装出来的行与 API 建出来的行长得不一样（最先丢的是 canonical
// 正文，接着 content_hash 就与内容对不上，症状是「部署永远收敛不了」）。
func declCapabilityVersion(namespaceID string, d capDecl, now int64) (*CapabilityVersion, error) {
	c := &CapabilityVersion{
		NamespaceID: namespaceID,
		Name:        d.Name,
		Version:     d.Version,
		DisplayName: d.DisplayName,
		Source:      CapSourceCustom,
		Spec:        d.Spec,
		CreatedAt:   now,
	}
	if _, err := prepareCapabilitySpec(c); err != nil {
		return nil, err
	}
	if err := deriveFromSpec(c); err != nil {
		return nil, err
	}
	// content_hash ≡ digest（吃 name + version + 整份 spec）：装上之前就要算出来，
	// 安装期拿它判「同 name@version 的重放内容是否相同」。
	hash, err := NormalizeCapability(c)
	if err != nil {
		return nil, err
	}
	c.ContentHash = hash
	return c, nil
}

// declCapHash 算一条声明安装后会得到的 content_hash。
//
// 必须与安装落库用同一份正文（declCapabilityVersion）与同一份口径（NormalizeCapability）：
// 算出来的值要能命中 capability_versions.content_hash，否则体检 / 同内容复用永远命不中。
func declCapHash(namespaceID string, d capDecl) (string, error) {
	c, err := declCapabilityVersion(namespaceID, d, 0)
	if err != nil {
		return "", fmt.Errorf("hash manifest capability %s: %w", d.Label(), err)
	}
	return c.ContentHash, nil
}

// playbookColumns 是统一的列顺序，scanPlaybook 依赖它。
const playbookColumns = `id, namespace_id, slug, name, descr, manifest, created_at`

// scanPlaybook 执行查询并解析一行（未命中返回 sql.ErrNoRows）。
func scanPlaybook(r scanner) (*Playbook, error) {
	var (
		pb       Playbook
		manifest string
	)
	if err := r.Scan(&pb.ID, &pb.NamespaceID, &pb.Slug, &pb.Name,
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
	parsed, err := ParsePlaybookManifest(manifest)
	if err != nil {
		return err
	}
	// 纯形状校验之后是两条**要看库**的校验（§12.2 第 3 条）：ref 解析后的出口、
	// 触发器的入口。它们必须在创建期做——安装期只能 skip，而「引用不到的名字」
	// 拖到安装期才发现，用户看到的是一份永远装不成的预案。
	if err := d.validateManifestRefs(ctx, pb.NamespaceID, parsed); err != nil {
		return err
	}

	pb.Manifest = manifest
	if pb.CreatedAt == 0 {
		pb.CreatedAt = Now()
	}
	if pb.ID == "" {
		pb.ID = NewPlaybookID()
	}

	if _, err := d.ExecContext(ctx,
		`INSERT INTO playbooks (`+playbookColumns+`) VALUES (?,?,?,?,?,?,?)`,
		pb.ID, pb.NamespaceID, pb.Slug, pb.Name, pb.Description,
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
	// Declared 是这条能力的**声明形态**（P5）：inline（内联完整 spec）/ ref（引用
	// 已有定义）/ legacy（旧形态 name+version，等价于 noop 占位）。
	//
	// 为何要上界面：ref 与内联**安装行为完全不同**（一个建行、一个不建行），
	// 而两者在 ref 字符串上看不出区别——“装不出来”的时候，用户第一件要知道的
	// 就是“这条到底是想装一份实现，还是只是想引用一份已有的”。
	Declared string `json:"declared,omitempty"`
	// Impl 是这条能力的**实现形式**：script（脚本，节点执行）/ noop（占位，中枢不执行）
	// / handler（中枢内置执行器）。空 = 没解析到正文（ref 引用不到 / 定义缺失）。
	Impl string `json:"impl,omitempty"`

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

	// 名字 → 声明：规则只有一份（resolveDecl：ref 跟声明的那一版 / 裸名字跟随 latest，
	// 内联与旧形态走「声明版本优先 → 同内容复用 → 版本号最小兜底」），与安装期
	// InstallPlaybook 共用。两侧口径一旦分叉就会出现「install 说 created、体检说
	// missing」这种自相矛盾的结论。
	resolver := listCapResolver{caps: caps}
	declared := declaredDecls(m)
	decls := parseManifestDecls(m)
	// resolveEndpoint 解析边的端点 / 入口目标：manifest 里声明过就按声明（ref 钉版本 /
	// 内联版本优先），否则按“只写名字”（版本号最小兜底）。体检与安装共用这一条。
	resolveEndpoint := func(name string) (*capLookup, string, error) {
		if dc := declAt(declared, name); dc != nil {
			if dc.Err != nil {
				return nil, "", nil // 坏声明当缺失：不让一项手改坏的数据弄坏整页
			}
			return resolveDecl(pb.NamespaceID, *dc, true, resolver)
		}
		return resolveManifestCap(name, true, resolver)
	}

	items := make([]PlaybookCheckItem, 0, len(decls)+len(m.Edges)+len(m.Triggers))

	// ① 能力定义 + ② 节点实现（绑在同一项上：第二项依赖第一项）
	for _, dc := range decls {
		// 声明本身非法（手改坏的 manifest）：当成缺失项展示，不让整个页面打不开。
		if dc.Err != nil {
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemCapability, Status: PlaybookCheckMissing, Ref: dc.Label(),
				Outputs: []string{}, Declared: dc.Kind, Impl: specImpl(dc.Spec),
			})
			continue
		}
		ref := dc.Label()
		// 声明的出口（ref 形态未知 → 空；解析到定义后以定义为准确）。
		outputs := dc.Outputs
		if outputs == nil {
			outputs = []string{}
		}
		// impl 优先看**声明自己的正文**（内联 spec 已经能回答“脚本 / noop 占位”），
		// ref 声明没有正文，只能看解析到的那行定义。
		impl := specImpl(dc.Spec)
		// 能力项自带版本号（ref 裸名字除外）：精确命中声明版本，否则复用同内容
		// 那条（I8）。命中复用那条时 Status / CapID 按**复用的定义**算，
		// Ref 仍是声明里的 x@2。
		def, _, derr := resolveDecl(pb.NamespaceID, dc, false, resolver)
		if derr != nil {
			return nil, fmt.Errorf("playbook checks: %w", derr)
		}
		if def != nil && impl == "" {
			impl = def.Impl
		}
		if dc.Kind == capKindRef && def != nil {
			// ref 的“出口”就是被引用那份定义的出口：那才是边能用的端口集合。
			outputs = def.Outputs
		}
		if def == nil {
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemCapability, Status: PlaybookCheckMissing, Ref: ref,
				Outputs: outputs, Runner: dc.Runner, Declared: dc.Kind, Impl: impl,
			})
			continue
		}
		// handler 形式的能力由**中枢**执行（如 notify-phone）：它不需要任何节点声明，
		// 所以「没有节点」不是待办——把它报成 no-node 会让 `ref: notify.phone` 这条
		// 唯一合法的「链尾发推送」永远显示缺节点。
		//
		// 占位（noop）**不在此列**：中枢不执行它，派发会剪枝 pruned/no_impl，
		// 那条链并不真的能跑（要显示成「缺节点」而不是假绿）。
		if def.Impl == playbookImplHandler {
			items = append(items, PlaybookCheckItem{
				Kind: PlaybookItemCapability, Status: PlaybookCheckOK, Ref: ref,
				Outputs: def.Outputs, CapID: def.ID, Runner: dc.Runner,
				Declared: dc.Kind, Impl: impl,
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
				Outputs: def.Outputs, CapID: def.ID, Runner: dc.Runner,
				Declared: dc.Kind, Impl: impl,
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
			Outputs: def.Outputs, CapID: def.ID, Runner: dc.Runner, Nodes: nodes,
			Declared: dc.Kind, Impl: impl,
		})
	}

	// ③ 出口：两端定义都在才谈得上"这条边在不在"
	for _, e := range m.Edges {
		// 隐式端口不写进人话（CONTRACT-hide-ports.md §1）：单出口能力的端口内部
		// 固定叫 out，把它打成"review --out--> fixer"等于把实现细节漏给运维。
		// 多出口能力（playbook 里显式写端口的老形态）仍然保留端口名——那里的端口
		// 是作者真写下的东西，不是我们编的。
		label := fmt.Sprintf("%s → %s", e.From, e.To)
		if e.FromPort != "" && e.FromPort != ImplicitPort {
			label = fmt.Sprintf("%s --%s--> %s", e.From, e.FromPort, e.To)
		}
		from, _, ferr := resolveEndpoint(e.From)
		if ferr != nil {
			return nil, fmt.Errorf("playbook checks: %w", ferr)
		}
		to, _, terr := resolveEndpoint(e.To)
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
			// 引用比对（P2.2）：钉死引用与跟随引用都算「这条边在」。
			if RefMatches(x.FromRef, from.Name, from.Version) &&
				x.FromPort == e.FromPort && RefMatches(x.ToRef, to.Name, to.Version) {
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

	// ④ 触发器：kind 匹配**且**这个源的目标里有本触发器要的入口能力。
	//
	// 只匹配 kind 会把"你有一个 webhook 源"当成"这条流程有触发器了"，
	// 而那个源可能指向完全无关的能力。
	toSet := make(map[string]bool, len(m.Edges))
	for _, e := range m.Edges {
		toSet[e.To] = true
	}
	entryNames := make([]string, 0, len(decls))
	entryIDs := make([]string, 0, len(decls))
	for _, dc := range decls {
		if dc.Err != nil || toSet[dc.Name] {
			continue // 坏声明 / 是某条边的下游 → 不是入口
		}
		entryNames = append(entryNames, dc.Name)
		// 入口能力也要按同一规则解析（安装期建源时 targets 指向的就是它）。
		def, _, derr := resolveDecl(pb.NamespaceID, dc, true, resolver)
		if derr != nil {
			return nil, fmt.Errorf("playbook checks: %w", derr)
		}
		if def != nil {
			entryIDs = append(entryIDs, def.ID)
		}
	}
	for _, t := range m.Triggers {
		// v2 的 targets 是**显式**的入口边：命中判据就是它，不再回落到“入口能力全集”
		// （否则一个指向别处入口的旧源会把新触发器误判成已就绪）。
		wantIDs, wantNames := entryIDs, entryNames
		if len(t.Targets) > 0 {
			wantIDs, wantNames = nil, nil
			for _, tg := range t.Targets {
				wantNames = append(wantNames, tg.To)
				def, _, derr := resolveEndpoint(tg.To)
				if derr != nil {
					return nil, fmt.Errorf("playbook checks: %w", derr)
				}
				if def != nil {
					wantIDs = append(wantIDs, def.ID)
				}
			}
		}
		hit := false
		for _, src := range sources {
			if src.Kind != t.Kind {
				continue
			}
			if sourceTargetsAny(src, wantIDs) {
				hit = true
				break
			}
		}
		label := t.Kind
		if name := strings.TrimSpace(t.Name); name != "" {
			label = name + " · " + label
		}
		if len(t.Types) > 0 {
			label += " · " + strings.Join(t.Types, " / ")
		}
		status := PlaybookCheckMissing
		if hit {
			status = PlaybookCheckOK
		}
		items = append(items, PlaybookCheckItem{
			Kind: PlaybookItemTrigger, Status: status, Label: label,
			EntryCaps: wantNames, DeclaredKind: t.Kind,
		})
	}

	// ⑤ 总体结论 + next + 计数
	checks := &PlaybookChecks{
		Items: items,
		Total: len(items),
		Counts: PlaybookCounts{
			Capabilities: len(decls),
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
	// ContentHash 是这一行的 content_hash（裸 hex）：安装期用它判「同 name@version
	// 的重放内容是否相同」（相同 = 纯幂等，不同 = append-only 冲突要点名说清）。
	ContentHash string
	// Impl 是实现形式（script / noop / handler），只给体检展示用。
	// SQL 侧不填（安装不需要它），listCapResolver 从整行算出来。
	Impl string
}

// lookupCapByNameVersion 按 (name, version) 查定义；未命中返回 nil, nil。
func lookupCapByNameVersion(ctx context.Context, q nsQueryer, namespaceID, name string, version int) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs, content_hash FROM capability_versions
		  WHERE namespace_id = ? AND name = ? AND version = ?`, namespaceID, name, version))
}

// lookupCapByName 按名字查定义，取**版本号最小**的那条（D7：mock 的 findCap 口径）；
// 未命中返回 nil, nil。
func lookupCapByName(ctx context.Context, q nsQueryer, namespaceID, name string) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs, content_hash FROM capability_versions
		  WHERE namespace_id = ? AND name = ? ORDER BY version, id LIMIT 1`, namespaceID, name))
}

// lookupCapByHash 按 (namespace_id, content_hash) 查定义；未命中返回 nil, nil。
func lookupCapByHash(ctx context.Context, q nsQueryer, namespaceID, hash string) (*capLookup, error) {
	return scanCapLookup(q.QueryRowContext(ctx,
		`SELECT id, name, version, outputs, content_hash FROM capability_versions
		  WHERE namespace_id = ? AND content_hash = ?`, namespaceID, hash))
}

// scanCapLookup 解析一行能力片段；未命中返回 nil, nil。
func scanCapLookup(row *sql.Row) (*capLookup, error) {
	var (
		c       capLookup
		outputs string
	)
	err := row.Scan(&c.ID, &c.Name, &c.Version, &outputs, &c.ContentHash)
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

// resolveManifestCap 是「manifest 里的**名字**（没声明过的那份）该用哪条定义」的
// 唯一实现（D7 + M3）：调用方把声明传进来时用 resolveDecl，只写名字时用这条。
//
// 只写名字的引用没有版本信息，所以只能命中「版本号最小」那条兜底（mock 的 findCap 口径）。
func resolveManifestCap(name string, allowByName bool, r capResolver) (*capLookup, string, error) {
	if !allowByName {
		return nil, "", nil
	}
	cap, err := r.byName(name)
	if err != nil {
		return nil, "", err
	}
	return cap, "", nil
}

// resolveDecl 解析一条 manifest 能力声明该连/用哪条定义。
//
// 三种形态三条口径，共用同一份兜底顺序（体检与安装都走它，分叉就会得出
// 「install 说 created、体检说 missing」这种自相矛盾的结论）：
//
//	ref 钉版本      只认那一版（`review@2` 不能悄悄降到 @1）
//	ref 裸名字      跟随 latest：取版本号最小那条（D7 / mock findCap 口径）
//	内联 / 旧形态   精确命中声明版本 → 同内容复用（I8）→ 版本号最小兜底（allowByName）
//
// why 非空只可能出现在「同内容复用」那一支，调用方据此写 note。
func resolveDecl(namespaceID string, d capDecl, allowByName bool, r capResolver) (*capLookup, string, error) {
	if d.Kind == capKindRef {
		var (
			cap *capLookup
			err error
		)
		if d.Version > 0 {
			cap, err = r.byNameVersion(d.Name, d.Version)
		} else {
			cap, err = r.byName(d.Name)
		}
		if err != nil {
			return nil, "", err
		}
		return cap, "", nil
	}
	var (
		exact, sameContent, byName *capLookup
		err                        error
	)
	if exact, err = r.byNameVersion(d.Name, d.Version); err != nil {
		return nil, "", err
	}
	if exact == nil {
		hash, herr := declCapHash(namespaceID, d)
		if herr != nil {
			return nil, "", herr
		}
		if sameContent, err = r.byContentHash(hash); err != nil {
			return nil, "", err
		}
	}
	if allowByName && exact == nil && sameContent == nil {
		if byName, err = r.byName(d.Name); err != nil {
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
	return &capLookup{
		ID: c.ID, Name: c.Name, Version: c.Version,
		Outputs: c.Outputs, ContentHash: c.ContentHash, Impl: specImpl(c.Spec),
	}
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
//
// 两端用**引用**比对（P2.2）：`name@version` 钉死的边与 `name` 跟随 latest 的边
// 都算「这条边已存在」——预案装的是「这两个能力连起来了」这件事，
// 不区分用户当初选的是哪一种引用形态。
func edgeExists(ctx context.Context, q nsQueryer, namespaceID string, from *capLookup, fromPort string, to *capLookup) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM edges
		  WHERE namespace_id = ? AND from_is_source = 0 AND from_port = ?
		    AND (from_ref = ? OR from_ref = ?)
		    AND (to_ref = ? OR to_ref = ?)`,
		namespaceID, fromPort,
		FormatRef(from.Name, from.Version), from.Name,
		FormatRef(to.Name, to.Version), to.Name).Scan(&n); err != nil {
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
//
// 源的 targets 就是**入口边**（P2.1：source_targets 已删）：左端是来源名（按名字 join），
// from_is_source=1 把它与能力边区分开。
func sourceWithKindTargets(ctx context.Context, q nsQueryer, namespaceID, kind string, capRefs []string) (string, error) {
	if len(capRefs) == 0 {
		return "", nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(capRefs)), ",")
	args := make([]any, 0, len(capRefs)+2)
	args = append(args, namespaceID, kind)
	for _, ref := range capRefs {
		args = append(args, ref)
	}
	var got string
	err := q.QueryRowContext(ctx,
		`SELECT s.name FROM sources s
		   JOIN edges e ON e.namespace_id = s.namespace_id AND e.from_ref = s.name AND e.from_is_source = 1
		  WHERE s.namespace_id = ? AND s.kind = ? AND e.to_ref IN (`+marks+`)
		  ORDER BY s.created_at, s.name LIMIT 1`, args...).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("probe same-kind source: %w", err)
	}
	return got, nil
}

// triggerSourceConfig 把 manifest 的触发器翻译成源的初始配置（D11 + §12.1）。
//
// v2 的 `config` 是**显式**的源配置，走与 POST /v1/sources 同一套校验
// （ValidateSourceConfig）——存进去的形状与手工建的源逐字相同，排障时不需要
// 多记一套「预案装的源长什么样」。
//
// 旧形态（只有 types）按 kind 翻译：cron 的 types[0] 就是五段表达式本身。
// 返回 ok=false 表示这份触发器**没法变成一个源**（cron 没给表达式）：安装期
// 只能 skip + note，不能凭空造一个“每天 3 点”出来。
func triggerSourceConfig(t ManifestTrigger) (json.RawMessage, bool) {
	if !isBlankJSON(t.Config) {
		cfg, err := ValidateSourceConfig(t.Kind, t.Config)
		if err != nil {
			// 配置期校验没拦住的手改数据：交给调用方 skip + note。
			return nil, false
		}
		return cfg, true
	}
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

// playbookEntryTarget 是安装期的一条入口边（= 源的一个 target）。
//
// 两个引用形态都要带着：Ref 是**落库**用的钉死引用（name@version，预案声明的就是
// 明确版本，跟随 latest 会让「装过一次的预案」自己改线），Name 是裸名字，供幂等判据 ②
// 认「用户手工建的、跟随 latest 的那条入口边」。
type playbookEntryTarget struct {
	Ref     string
	Name    string
	Mapping json.RawMessage
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
//
// P5（Capability Spec v3 §12.3）之后三种声明的安装语义各不同：
//
//	ref     不建任何行，只解析成落库引用；引用不到 → 400 带下标（不允许装一半）
//	spec    走 createCapabilityTx（与 POST /v1/capabilities 同一条路），装出来就是
//	        一份真能跑的能力（脚本形式）或 noop 占位
//	旧形态  等价于 handler: noop 的内联 spec（与回填口径一致，行为不变）
func (d *DB) InstallPlaybook(ctx context.Context, namespaceID, idOrSlug string) (*Playbook, *PlaybookInstallResult, error) {
	pb, err := d.GetPlaybook(ctx, namespaceID, idOrSlug)
	if err != nil {
		return nil, nil, err
	}
	m := pb.parsedManifest()
	decls := parseManifestDecls(m)

	// ⓪ 写任何行之前先把每条声明验一遍（§12.2 第 3 条：不允许「装了一半」）。
	//    创建期已经拦过，这里拦的是**绕过创建期**的手改数据（直接写库 / 老库里的旧
	//    manifest）：错误带下标，事务还没开始。
	for _, dc := range decls {
		if dc.Err != nil {
			return nil, nil, fmt.Errorf("%w: capabilities[%d]: %v",
				ErrPlaybookManifestInvalid, dc.Index, dc.Err)
		}
	}

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
	ex := txExec{tx: tx}

	now := Now()

	// 名字 → 声明：规则只有一份（resolveDecl，与体检共用），这里跑在事务里。
	resolver := sqlCapResolver{ctx: ctx, q: tx, namespaceID: pb.NamespaceID}
	declared := declaredDecls(m)

	// resolveEndpoint 解析边的端点 / 入口目标：manifest 里声明过就按声明算，
	// 否则按「只写名字」的口径（版本号最小兜底）。
	resolveEndpoint := func(name string) (*capLookup, string, error) {
		if dc := declAt(declared, name); dc != nil {
			return resolveDecl(pb.NamespaceID, *dc, true, resolver)
		}
		return resolveManifestCap(name, true, resolver)
	}

	// noopNotes 收集「装成了 noop 占位」的说明，最后统一并入 res.Notes（见 ① 的注释）。
	var noopNotes []string

	// ① 能力定义：append-only（I8）。已存在 (name,version) 一律 skipped。
	for _, dc := range decls {
		ref := dc.Label()

		if dc.Kind == capKindRef {
			// 引用形态**不建任何行**（§12.1）：它指的是命名空间里已有的定义。
			// 解析不到 → 带下标 400（引用没有正文可装，解析不到只能是配置错了）。
			def, _, rerr := resolveDecl(pb.NamespaceID, dc, true, resolver)
			if rerr != nil {
				return nil, nil, fmt.Errorf("install playbook: %w", rerr)
			}
			if def == nil {
				return nil, nil, fmt.Errorf("%w: capabilities[%d]: 引用「%s」在命名空间里没有定义",
					ErrPlaybookManifestInvalid, dc.Index, dc.Ref)
			}
			continue
		}

		// 内联 / 旧形态：正文校验、canonical 化、派生列、content_hash 全部由
		// createCapabilityTx 算 —— 自己拼 INSERT 会让预案装出来的行与 API 建出来的行
		// 长得不一样（最先丢的是 canonical 正文，接着 content_hash 与内容对不上）。
		def, derr := declCapabilityVersion(pb.NamespaceID, dc, now)
		if derr != nil {
			return nil, nil, fmt.Errorf("%w: capabilities[%d]: %v",
				ErrPlaybookManifestInvalid, dc.Index, derr)
		}
		// 幂等：先看 (name,version) 是否已存在。存在就是 append-only 的「不覆盖」，
		// 按内容是否相同分开说：同内容 = 纯幂等（不写 note，否则二次点击会得到一屏告警），
		// 不同内容 = 定义已被人改过，绝不覆盖。
		if existing, lerr := resolver.byNameVersion(dc.Name, dc.Version); lerr != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", lerr)
		} else if existing != nil {
			res.Skipped.Capabilities = append(res.Skipped.Capabilities, ref)
			if existing.ContentHash != def.ContentHash {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"能力 %s 跳过：同名同版本的定义已存在且内容不同（定义 append-only，不覆盖）", ref))
			}
			continue
		}
		// force=true：**不经过升级闸门**。闸门问的是「这次成为 latest 会打断谁」，
		// 它服务的对象是 POST /v1/capabilities 后面那个能点「我知道，仍然推送」的人；
		// 而安装一份预案从来没有这一问（现状不回归，§12.3 也只说「冲突只 skip」）。
		// 校验与派生一个都不少：createCapabilityTx 里 prepareCapabilitySpec /
		// deriveFromSpec / NormalizeCapability 全部照跑。
		if cerr := createCapabilityTx(ctx, ex, def, true); cerr != nil {
			switch {
			case errors.Is(cerr, ErrVersionConflict):
				// 同 name@version 换了内容：定义 append-only，跳过 + 说清（绝不覆盖）。
				res.Skipped.Capabilities = append(res.Skipped.Capabilities, ref)
				res.Notes = append(res.Notes, fmt.Sprintf(
					"能力 %s 跳过：同名同版本的定义已存在且内容不同（定义 append-only，不覆盖）", ref))
				continue
			case errors.Is(cerr, ErrCapabilityInvalid), errors.Is(cerr, ErrCapabilityReserved):
				// 内容在创建期已验过，走到这里说明是**绕过创建期**的手改数据：仍然是 400。
				return nil, nil, fmt.Errorf("%w: capabilities[%d]: %v",
					ErrPlaybookManifestInvalid, dc.Index, cerr)
			}
			return nil, nil, fmt.Errorf("install playbook: capabilities[%d] %s: %w", dc.Index, ref, cerr)
		}

		// createCapabilityTx 对「同内容」会折叠成已有那一行（内容寻址，§10-B）：
		// 但那不是这次装出来的，按 skipped 记（并说清内容与哪一版相同）。
		if def.Version != dc.Version || def.Name != dc.Name {
			res.Skipped.Capabilities = append(res.Skipped.Capabilities, ref)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"能力 %s 跳过：命名空间里已有同内容的 %s@%d（同内容不产生第二行定义）",
				ref, def.Name, def.Version))
			continue
		}

		res.Created.Capabilities = append(res.Created.Capabilities, ref)
		// 装成 noop 占位的能力必须说清楚：列表页能标「未实现」，人也不会以为它在跑真东西。
		// 先收集、最后统一追加 —— notes 是一份给人读的清单，占位说明不该插在
		// 「哪条边/哪个源被跳过」中间。
		if specImpl(def.Spec) == playbookImplNoop {
			if dc.Kind == capKindLegacy {
				noopNotes = append(noopNotes, fmt.Sprintf(
					"%s: 该能力尚无实现，已装为 noop 占位（manifest 的 runner 不解释成实现）", ref))
			} else {
				noopNotes = append(noopNotes, fmt.Sprintf(
					"%s: 已装为 noop 占位（handler: noop 合法、可部署、无副作用，但不发 emit）", ref))
			}
		}
	}

	// ② 边：两端都要有定义；from 的出口必须含 fromPort（I2）。
	// 单条边不成立只 skip 它自己，其余照装（不整体回滚）。
	for i, e := range m.Edges {
		// 隐式端口不写进人话（CONTRACT-hide-ports.md §1）：单出口能力的端口内部
		// 固定叫 out，把它打成"review --out--> fixer"等于把实现细节漏给运维。
		// 多出口能力（playbook 里显式写端口的老形态）仍然保留端口名——那里的端口
		// 是作者真写下的东西，不是我们编的。
		label := fmt.Sprintf("%s → %s", e.From, e.To)
		if e.FromPort != "" && e.FromPort != ImplicitPort {
			label = fmt.Sprintf("%s --%s--> %s", e.From, e.FromPort, e.To)
		}
		from, fromWhy, err := resolveEndpoint(e.From)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: edges[%d]: %w", i, err)
		}
		to, toWhy, err := resolveEndpoint(e.To)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: edges[%d]: %w", i, err)
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
		if fromWhy != "" {
			if dc := declAt(declared, e.From); dc != nil {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"边 %s 的一端「%s」按声明的 %s 找不到定义，%s", label, e.From, dc.Label(), fromWhy))
			}
		}
		if toWhy != "" {
			if dc := declAt(declared, e.To); dc != nil {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"边 %s 的一端「%s」按声明的 %s 找不到定义，%s", label, e.To, dc.Label(), toWhy))
			}
		}
		if !containsString(from.Outputs, e.FromPort) {
			res.Skipped.Edges = append(res.Skipped.Edges, label)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"边 %s 跳过：出口「%s」不在 %s@%d 的 outputs %v 里（出口是封闭集合）",
				label, e.FromPort, from.Name, from.Version, from.Outputs))
			continue
		}
		exists, err := edgeExists(ctx, tx, pb.NamespaceID, from, e.FromPort, to)
		if err != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", err)
		}
		if exists {
			res.Skipped.Edges = append(res.Skipped.Edges, label)
			continue
		}

		// 常量覆盖：键与值类型要按**目标版本**的 params_schema 判（与 CreateEdge 同一套
		// 校验，同一份实现）。判不过就 skip 这条边并说清——安装期只能 skip。
		literals := json.RawMessage(EdgeLiteralsEmpty)
		if !isBlankJSON(e.Literals) {
			target, gerr := getCapabilityOn(ctx, ex, pb.NamespaceID, to.Name, to.Version)
			if gerr != nil {
				return nil, nil, fmt.Errorf("install playbook: edges[%d]: %w", i, gerr)
			}
			norm, lerr := checkEdgeLiterals(target, e.Literals)
			if lerr != nil {
				res.Skipped.Edges = append(res.Skipped.Edges, label)
				res.Notes = append(res.Notes, fmt.Sprintf("边 %s 跳过：%v", label, lerr))
				continue
			}
			literals = norm
		}

		// 落库写**钉死引用**（name@version）：预案声明的是明确版本（manifest 里写了 version），
		// 装出来的线就该钉在那一版上——跟随 latest 会让「装过一次的预案」自己改线。
		if _, err := ex.execContext(ctx,
			`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
			 VALUES (?,?,?,?,?,?,'{}',?,?)`,
			pb.NamespaceID, 0, FormatRef(from.Name, from.Version), e.FromPort,
			FormatRef(to.Name, to.Version), string(literals), e.Priority, now); err != nil {
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

	// 入口能力的**引用**（P2.2）：建源时写进入口边的右端（钉死：预案声明的就是明确版本）。
	entryTargets := make([]playbookEntryTarget, 0, len(decls))
	entryNames := make([]string, 0, len(decls))
	entrySeen := make(map[string]bool, len(decls))
	for _, dc := range decls {
		if toSet[dc.Name] {
			continue
		}
		def, _, derr := resolveDecl(pb.NamespaceID, dc, true, resolver)
		if derr != nil {
			return nil, nil, fmt.Errorf("install playbook: %w", derr)
		}
		if def == nil || entrySeen[def.ID] {
			continue
		}
		entrySeen[def.ID] = true
		entryTargets = append(entryTargets, playbookEntryTarget{
			Ref:     FormatRef(def.Name, def.Version),
			Name:    def.Name,
			Mapping: json.RawMessage(EdgeMappingEmpty),
		})
		entryNames = append(entryNames, dc.Name)
	}

	for i, t := range m.Triggers {
		// 源名：v2 可以用 name 显式指定；省略时沿用 `<slug>-<kind>-<idx>`
		// （slug 在命名空间内唯一 → 名字确定、可幂等比对）。
		wantName := strings.TrimSpace(t.Name)
		if wantName == "" {
			wantName = fmt.Sprintf("%s-%s-%d", pb.Slug, t.Kind, i)
		}

		// 入口边：v2 的 targets 显式给出（带 mapping）；旧形态回落到入口能力全集。
		targets := entryTargets
		targetNames := entryNames
		if len(t.Targets) > 0 {
			targets = make([]playbookEntryTarget, 0, len(t.Targets))
			targetNames = make([]string, 0, len(t.Targets))
			bad := false
			for j, tg := range t.Targets {
				def, _, derr := resolveEndpoint(tg.To)
				if derr != nil {
					return nil, nil, fmt.Errorf("install playbook: triggers[%d].targets[%d]: %w", i, j, derr)
				}
				if def == nil {
					res.Notes = append(res.Notes, fmt.Sprintf(
						"源 %s 跳过：入口能力「%s」在本命名空间里没有定义", wantName, tg.To))
					bad = true
					break
				}
				mapping := json.RawMessage(EdgeMappingEmpty)
				if !isBlankJSON(tg.Mapping) {
					// mapping 只允许出现在入口边，键要 ∈ 目标 params_schema.properties
					// （与 POST /v1/sources 同一套校验，同一份实现）。
					target, gerr := getCapabilityOn(ctx, ex, pb.NamespaceID, def.Name, def.Version)
					if gerr != nil {
						return nil, nil, fmt.Errorf("install playbook: triggers[%d].targets[%d]: %w", i, j, gerr)
					}
					norm, merr := checkEdgeMapping(target, tg.Mapping, true)
					if merr != nil {
						res.Notes = append(res.Notes, fmt.Sprintf(
							"源 %s 跳过：targets[%d].mapping %v", wantName, j, merr))
						bad = true
						break
					}
					mapping = norm
				}
				targets = append(targets, playbookEntryTarget{
					Ref:     FormatRef(def.Name, def.Version),
					Name:    def.Name,
					Mapping: mapping,
				})
				targetNames = append(targetNames, tg.To)
			}
			if bad {
				res.Skipped.Sources = append(res.Skipped.Sources, wantName)
				continue
			}
		}

		if len(targets) == 0 {
			res.Skipped.Sources = append(res.Skipped.Sources, wantName)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"触发 %s 跳过：本预案没有可用的入口能力 %v（入口 = 不作为任何边目标的能力），没有可派的目标就不建源",
				t.Kind, targetNames))
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
		// 幂等判据 ②：已有同 kind 且 targets 命中本触发器入口的源。
		//
		// probe 同时带钉死引用与裸名字：手工建的入口边可能是跟随 latest 的 `review`，
		// 也可能是钉死的 `review@1`；两者都指向同一个入口能力，不能因为引用形态不同
		// 就再建一个源（同一事件会被派发两次）。
		probe := make([]string, 0, 2*len(targets))
		for _, tg := range targets {
			probe = append(probe, tg.Ref, tg.Name)
		}
		got, err = sourceWithKindTargets(ctx, tx, pb.NamespaceID, t.Kind, probe)
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

		raw, ok := triggerSourceConfig(t)
		if !ok {
			res.Skipped.Sources = append(res.Skipped.Sources, wantName)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"源 %s 跳过：%s 触发没有给出可用的配置（cron 需要 config.expr 或 types[0] = 五段表达式）", wantName, t.Kind))
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
		if _, err := ex.execContext(ctx,
			`INSERT INTO sources (id, namespace_id, name, kind, config, enabled, last_fired_at, next_fire_at, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			srcID, pb.NamespaceID, wantName, t.Kind, string(cfg), boolToInt(t.isEnabled()), nil, nextFire, now); err != nil {
			return nil, nil, fmt.Errorf("install playbook: insert source %s: %w", wantName, err)
		}
		for _, tg := range targets {
			// 源的目标就是**入口边**（P2.1）：左端是来源名，from_port='*' 表示任意事件类型，
			// mapping 是这条入口边的参数模板（'{}' = 不额外传参）。
			if _, err := ex.execContext(ctx,
				`INSERT INTO edges (namespace_id, from_is_source, from_ref, from_port, to_ref, literals, mapping, priority, created_at)
				 VALUES (?,1,?,'*',?,'{}',?,0,?)
				 ON CONFLICT(namespace_id, from_is_source, from_ref, from_port, to_ref) DO NOTHING`,
				pb.NamespaceID, wantName, tg.Ref, string(tg.Mapping), now); err != nil {
				return nil, nil, fmt.Errorf("install playbook: insert entry edge %s: %w", wantName, err)
			}
		}
		res.Created.Sources = append(res.Created.Sources, wantName)
	}

	res.Notes = append(res.Notes, noopNotes...)

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("install playbook: commit: %w", err)
	}
	return pb, res, nil
}
