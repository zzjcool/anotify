package playbook

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// Action 是 Plan 给 store 的动作（12 格矩阵的两类落点：建 / 复用）。
type Action string

const (
	// ActionCreate 是"本次新建这一行"（INSERT）。
	ActionCreate Action = "create"
	// ActionReuse 是"复用库里已有的行"（什么都不写；差异进 unapplied[]）。
	ActionReuse Action = "reuse"
)

// Conflict 是 400 响应体里的 `conflicts[]` 一项（§9.2 的 playbook_conflict 形状）。
//
// 每个字段都不得丢（W3 冻结）：`kind` / `ref` / `reason`，能力冲突再给
// `declaredDigest` / `actualDigest`，源冲突再给 `fields`（字段名，**不给值**——token 脱敏）。
type Conflict struct {
	Kind           string   `json:"kind"`
	Ref            string   `json:"ref"`
	Reason         string   `json:"reason"`
	DeclaredDigest string   `json:"declaredDigest,omitempty"`
	ActualDigest   string   `json:"actualDigest,omitempty"`
	Fields         []string `json:"fields,omitempty"`
}

// CapabilitySnapshot 是库里一条能力行的快照（由 store 在当前事务内查出来填）。
type CapabilitySnapshot struct {
	Name        string
	Version     int
	Digest      string
	FoldKey     string
	DisplayName string
	// Next 是这一行**当前的路由**（行 7 的比较对象）。
	Next []string
}

// SourceSnapshot 是库里一条源行的快照。
type SourceSnapshot struct {
	Name string
	Kind string
	// Enabled 只是事实记录：install 只做加法，不复用行上改它。
	Enabled bool
	// Config 是库里存的（已归一化的）config 原文。
	Config json.RawMessage
	// Targets 是这一行当前的出口声明（行 12 的比较对象，含 params 映射）。
	Targets []sourcespec.Target
}

// Existing 是 Plan 需要的**全部**库事实，只有六个只读查询。
//
// 边界：本包不 import store，也不私开 DB——store（G1-5b）在当前事务内实现它，
// 于是 `dryRun` 与真实 install 走同一条代码路径（§4.4 的 ②"读一次库快照"）。
type Existing interface {
	// CapabilityByFoldKey 按 (name, foldKey) 找一行：省略 version 时判"正文相同"（行 2）。
	CapabilityByFoldKey(name, foldKey string) (*CapabilitySnapshot, bool)
	// CapabilityByNameVersion 按 (name, version) 找一行：显式 version（行 4/5/6）。
	CapabilityByNameVersion(name string, version int) (*CapabilitySnapshot, bool)
	// CapabilityMaxVersion 返回该名字已存在的最大版本（不存在 → 0, false）：行 3 分配 max+1。
	CapabilityMaxVersion(name string) (int, bool)
	// SourceByName 按名字找源（行 9/10/12）。
	SourceByName(name string) (*SourceSnapshot, bool)
	// BareNameRefs 返回库里**引用裸名**（不带 @version）的边数与源数：`upgraded[].affects`。
	BareNameRefs(name string) (edges, sources int)
	// HasUpstream 报告库里是否已有指向 name 的引用（裸名，或 `name@任意版本`）：
	// 装完自检的 `unreachable` note 用它排除"包外已有上游"。
	HasUpstream(name string) bool
}

// emptyExisting 是"空库"快照（新命名空间 / 单测）。
type emptyExisting struct{}

func (emptyExisting) CapabilityByFoldKey(string, string) (*CapabilitySnapshot, bool) {
	return nil, false
}
func (emptyExisting) CapabilityByNameVersion(string, int) (*CapabilitySnapshot, bool) {
	return nil, false
}
func (emptyExisting) CapabilityMaxVersion(string) (int, bool)     { return 0, false }
func (emptyExisting) SourceByName(string) (*SourceSnapshot, bool) { return nil, false }
func (emptyExisting) BareNameRefs(string) (int, int)              { return 0, 0 }
func (emptyExisting) HasUpstream(string) bool                     { return false }

// EmptyExisting 返回"库里什么都没有"的快照（新命名空间或纯单测用）。
//
// 刻意不把 `nil` 当空库：忘了传快照就装出一个"以为库是空的"的计划，比报错危险得多。
func EmptyExisting() Existing { return emptyExisting{} }

// PlannedCapability 是一条能力的执行指令（Plan 算完，store 在单事务里照做）。
type PlannedCapability struct {
	// Artifact 是本次要建/已对上的产物。Action=create 时 Metadata.Version 已定（1 或 max+1），
	// 因此 Digest 可直接落库；Action=reuse 时保持包里写的样子（可能省略 version）。
	Artifact *artifact.Artifact
	// Action 决定 INSERT 还是什么都不写。
	Action Action
	// FoldKey / Digest 是内容身份（create 时都是本次算出的；reuse 时是库里那行的）。
	FoldKey string
	Digest  string
	// ReusedVersion 是 Action=reuse 时对上的版本（回执写 `name@version`）。
	ReusedVersion int
}

// PlannedSource 是一条源的执行指令。
type PlannedSource struct {
	Name string
	// Spec 是渲染后、config 已归一化的源（token 空 = 待生成/保留）。
	Spec sourcespec.Spec
	// Action 决定 INSERT 还是什么都不写。
	Action Action
	// GenerateToken 表示"本次新建 webhook 源且包里没给 token"→ store 生成一个随机 token。
	// 它必须由 store 做（写库副作用），所以是计划里的一个动作标记，不是本包的副作用。
	GenerateToken bool
}

// PlanResult 是 12 格判定的产物：回执基础字段 + 给 store 的执行指令。
//
// 有 Conflicts 时**调用方整包 0 写**（Plan 会把 result 和 error 一起返回，方便 400 body
// 直接带 `conflicts[]` / `valuesDigest` / `hint`）。
type PlanResult struct {
	Playbook     PlaybookRef
	Values       map[string]any
	ValuesDigest string

	Conflicts []Conflict
	Created   Created
	Skipped   Created
	Upgraded  []UpgradedEntry
	Unapplied []UnappliedEntry
	External  []string
	Pending   []string
	Notes     []Note

	// Capabilities / Sources 是执行指令（顺序 = 包里书写的顺序，store 照此写）。
	Capabilities []PlannedCapability
	Sources      []PlannedSource

	// schema 是 spec.values 的解析形态：BuildReceipt 打码要用（不导出，避免两份真相）。
	schema map[string]any
}

// Plan 按 playbook.md §4.2 的 12 格矩阵算出安装动作（**纯函数**）。
//
// 12 格 → 代码落点：
//
//	1  能力（省略 v）+ 无同名                       → addCreatedCapability(v1)
//	2  能力（省略 v）+ 同名同 foldKey                → reuseCapability（skipped，不写 note）
//	3  能力（省略 v）+ 同名不同正文                  → upgraded + addCreatedCapability(max+1)
//	4  能力（显式 v）+ 无 name@v                     → addCreatedCapability(v)
//	5  能力（显式 v）+ name@v 正文相同               → reuseCapability
//	6  能力（显式 v）+ name@v 正文不同               → Conflicts（version_content_immutable）
//	7  能力（任意）+ 该 name@v 的 next 不同           → Unapplied（routing_unapplied）+ note
//	8  源 + 无同名                                  → PlannedSource{create}（token 省略 → GenerateToken）
//	9  源 + 同名 kind/config 等价                    → PlannedSource{reuse}（不写 targets / 入口边）
//	10 源 + 同名 kind/config 不同                    → Conflicts（playbook_conflict）
//	11 源 targets 指向既不在包内也不在库里的名字      → Pending + note（pending_ref）
//	12 源 + 同名 config 等价但 targets 不同          → Unapplied（targets_unapplied）+ note
//
// 它把三件事分开：渲染后的文档（Render）、库快照（Existing）、以及由此决定的动作矩阵。
// 顺序照 §4.4：① render（values 错就地 400）→ ② 读快照（调用方做）→ ③ 算矩阵
// （有 conflict 就 400，一行不写）。④⑤（单事务写入 + 回执）是 store 的事（G1-5b）。
//
// 返回约定：有 Conflicts 时同时返回 (result, error)——
//   - error 的 Code 是 `version_content_immutable`（只有能力冲突）或 `playbook_conflict`，
//     Details 带 `conflicts` / `valuesDigest` / `hint`（§9.2 的 400 形状）；
//   - result 仍然完整（调用方要展示冲突全貌）。
func Plan(pkg *Package, values map[string]any, existing Existing) (*PlanResult, error) {
	if existing == nil {
		return nil, errf(CodeInvalidPlaybook, "Plan 需要库快照（Existing）；空库请传 EmptyExisting()")
	}
	rendered, err := Render(pkg, values)
	if err != nil {
		return nil, err
	}
	sch, err := pkg.schemaOf()
	if err != nil {
		return nil, err
	}
	plan := &PlanResult{
		Playbook:     PlaybookRef{Name: pkg.Name, Version: pkg.Version},
		Values:       rendered.Values,
		ValuesDigest: rendered.ValuesDigest,
		Created:      emptyCreated(),
		Skipped:      emptyCreated(),
		Upgraded:     []UpgradedEntry{},
		Unapplied:    []UnappliedEntry{},
		External:     []string{},
		Pending:      []string{},
		Notes:        []Note{},
		Capabilities: make([]PlannedCapability, 0, len(rendered.Capabilities)),
		Sources:      make([]PlannedSource, 0, len(rendered.Sources)),
		schema:       sch,
	}

	// 装完的"有效路由"：新建行按包里写的（会真的写进去），复用行按库里现有的
	// （install 不改它）。`unreachable` 自检必须用这份，而不是包里声明的——
	// 否则行 7 造成的孤儿能力会被"包里明明连着"掩盖掉。
	effNext := make(map[string][]string, len(rendered.Capabilities))
	effTargets := make(map[string][]sourcespec.Target, len(rendered.Sources))

	if err := planCapabilities(plan, rendered.Capabilities, existing, effNext); err != nil {
		return nil, err
	}
	if err := planSources(plan, rendered.Sources, existing, effTargets); err != nil {
		return nil, err
	}
	plan.resolveRefs(rendered, existing)
	plan.markUnreachable(effNext, effTargets, existing)

	if len(plan.Conflicts) > 0 {
		return plan, plan.conflictError()
	}
	return plan, nil
}

// planCapabilities 走 §4.2 的第 1-7 行。
func planCapabilities(plan *PlanResult, caps []*artifact.Artifact, existing Existing, effNext map[string][]string) error {
	for _, a := range caps {
		name := a.Metadata.Name
		foldKey, err := artifact.FoldKey(a)
		if err != nil {
			return errf(CodeInvalidPlaybook, "能力 %q 算不出 foldKey: %v", name, err)
		}
		if a.Metadata.Version >= 1 {
			// 行 4/5/6：显式 version 永不折叠。
			snap, ok := existing.CapabilityByNameVersion(name, a.Metadata.Version)
			if !ok {
				if err := plan.addCreatedCapability(a, a.Metadata.Version, foldKey); err != nil {
					return err
				}
				effNext[name] = a.Spec.Next
				continue
			}
			if snap.FoldKey != foldKey {
				declared, derr := artifact.Digest(a)
				if derr != nil {
					return errf(CodeInvalidPlaybook, "能力 %q 算不出 digest: %v", name, derr)
				}
				plan.Conflicts = append(plan.Conflicts, Conflict{
					Kind:           "capability",
					Ref:            capRef(name, a.Metadata.Version),
					Reason:         ConflictVersionConflict,
					DeclaredDigest: declared,
					ActualDigest:   snap.Digest,
				})
				effNext[name] = snap.Next
				continue
			}
			plan.reuseCapability(a, snap, effNext)
			continue
		}

		// 省略 version：先判"正文相同"（foldKey），再判"同名不同正文"（长新版本）。
		if snap, ok := existing.CapabilityByFoldKey(name, foldKey); ok {
			plan.reuseCapability(a, snap, effNext)
			continue
		}
		if maxV, ok := existing.CapabilityMaxVersion(name); ok {
			edges, sources := existing.BareNameRefs(name)
			plan.Upgraded = append(plan.Upgraded, UpgradedEntry{
				Capability: name,
				From:       maxV,
				To:         maxV + 1,
				Affects:    Affects{Edges: edges, Sources: sources},
			})
			if err := plan.addCreatedCapability(a, maxV+1, foldKey); err != nil {
				return err
			}
			effNext[name] = a.Spec.Next
			continue
		}
		if err := plan.addCreatedCapability(a, 1, foldKey); err != nil {
			return err
		}
		effNext[name] = a.Spec.Next
	}
	return nil
}

// addCreatedCapability 记一条"本次新建"（行 1/3/4）。
func (p *PlanResult) addCreatedCapability(a *artifact.Artifact, version int, foldKey string) error {
	clone := *a
	clone.Metadata.Version = version
	digest, err := artifact.Digest(&clone)
	if err != nil {
		// 归一化过的产物 + 已知 version 不该失败；真失败就如实上报（不吞错、不拿空 digest 蒙混）。
		return errf(CodeInvalidPlaybook, "能力 %q 算不出 digest: %v", clone.Metadata.Name, err)
	}
	p.Created.Capabilities = append(p.Created.Capabilities, capRef(clone.Metadata.Name, version))
	p.Capabilities = append(p.Capabilities, PlannedCapability{
		Artifact: &clone,
		Action:   ActionCreate,
		FoldKey:  foldKey,
		Digest:   digest,
	})
	return nil
}

// reuseCapability 记一条"复用"（行 2/5），并做行 7 的路由比较。
func (p *PlanResult) reuseCapability(a *artifact.Artifact, snap *CapabilitySnapshot, effNext map[string][]string) {
	name := a.Metadata.Name
	p.Skipped.Capabilities = append(p.Skipped.Capabilities, capRef(name, snap.Version))
	p.Capabilities = append(p.Capabilities, PlannedCapability{
		Artifact:      a,
		Action:        ActionReuse,
		FoldKey:       snap.FoldKey,
		Digest:        snap.Digest,
		ReusedVersion: snap.Version,
	})
	// 行 7：该 name@v 已存在且包里 next 不同 → 不改路由，进 unapplied[]（+ note）。
	effNext[name] = snap.Next
	if sameRefSet(a.Spec.Next, snap.Next) {
		return
	}
	declared := append(make([]string, 0, len(a.Spec.Next)), a.Spec.Next...)
	actual := append(make([]string, 0, len(snap.Next)), snap.Next...)
	ref := capRef(name, snap.Version)
	p.Unapplied = append(p.Unapplied, UnappliedEntry{
		Kind:     "capability",
		Ref:      ref,
		Field:    "next",
		Declared: declared,
		Actual:   actual,
		Reason:   ReasonRoutingUnapplied,
	})
	p.Notes = append(p.Notes, Note{
		Code: NoteRoutingUnapplied,
		Ref:  ref,
		Message: fmt.Sprintf("%s 的路由没有改：包里要 %s，库里是 %s（安装只做加法，改路由请改能力本身）",
			ref, formatRefs(declared), formatRefs(actual)),
	})
}

// planSources 走 §4.2 的第 8-12 行。
func planSources(plan *PlanResult, srcs []Source, existing Existing, effTargets map[string][]sourcespec.Target) error {
	for _, s := range srcs {
		if !s.Spec.Enabled {
			plan.Notes = append(plan.Notes, Note{
				Code:    NoteSourceDisabled,
				Ref:     s.Name,
				Message: fmt.Sprintf("源 %s 渲染后 enabled: false——装上去了，但不会触发", s.Name),
			})
		}
		snap, ok := existing.SourceByName(s.Name)
		if !ok {
			// 行 8：新建（webhook 的 token 省略则由服务端生成）。
			plan.Created.Sources = append(plan.Created.Sources, s.Name)
			plan.Sources = append(plan.Sources, PlannedSource{
				Name:          s.Name,
				Spec:          s.Spec,
				Action:        ActionCreate,
				GenerateToken: needsGeneratedToken(s.Spec),
			})
			effTargets[s.Name] = s.Spec.Targets
			continue
		}
		// 行 10：同名 + kind/config 真正不同 → 拒绝（整包 0 写）。
		if snap.Kind != s.Spec.Kind {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Kind:   "source",
				Ref:    s.Name,
				Reason: ConflictConfigDiffers,
				Fields: []string{"kind"},
			})
			effTargets[s.Name] = snap.Targets
			continue
		}
		merged, err := inheritServerGenerated(s.Spec.Kind, s.Spec.Config, snap.Config)
		if err != nil {
			return errf(CodeInvalidPlaybook, "源 %q 的 config 无法比较: %v", s.Name, err)
		}
		if !sourcespec.SameConfig(s.Spec.Kind, merged, snap.Config) {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Kind:   "source",
				Ref:    s.Name,
				Reason: ConflictConfigDiffers,
				Fields: configDiffFields(s.Spec.Kind, merged, snap.Config),
			})
			effTargets[s.Name] = snap.Targets
			continue
		}
		// 行 9：复用（不写 targets、不写入口边）。
		plan.Skipped.Sources = append(plan.Skipped.Sources, s.Name)
		plan.Sources = append(plan.Sources, PlannedSource{
			Name:   s.Name,
			Spec:   s.Spec,
			Action: ActionReuse,
		})
		effTargets[s.Name] = snap.Targets
		// 行 12：targets（含 params 映射）不同 → 不改 targets，进 unapplied[]（+ note）。
		if sourcespec.SameTargets(s.Spec.Targets, snap.Targets) {
			continue
		}
		declared := append([]sourcespec.Target{}, s.Spec.Targets...)
		actual := append([]sourcespec.Target{}, snap.Targets...)
		plan.Unapplied = append(plan.Unapplied, UnappliedEntry{
			Kind:     "source",
			Ref:      s.Name,
			Field:    "targets",
			Declared: declared,
			Actual:   actual,
			Reason:   ReasonTargetsUnapplied,
		})
		plan.Notes = append(plan.Notes, Note{
			Code: NoteTargetsUnapplied,
			Ref:  s.Name,
			Message: fmt.Sprintf("源 %s 复用了，但事件去向没有改：包里要 %s，库里是 %s（体检不得全绿）",
				s.Name, formatTargets(declared), formatTargets(actual)),
		})
	}
	return nil
}

// resolveRefs 把 next / targets 的引用分成三种落点（§3）：包内 / 包外已有（external）/ 都没有（pending）。
func (p *PlanResult) resolveRefs(rendered *RenderResult, existing Existing) {
	inPkg := make(map[string]bool, len(rendered.Capabilities))
	for _, a := range rendered.Capabilities {
		inPkg[a.Metadata.Name] = true
	}
	seen := make(map[string]bool)

	classify := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		name := refName(ref)
		if inPkg[name] {
			return // 包内：同一事务里一起建，顺序无关
		}
		if p.existsRef(name, ref, existing) {
			p.External = append(p.External, ref)
			return
		}
		p.Pending = append(p.Pending, ref)
		p.Notes = append(p.Notes, Note{
			Code:    NotePendingRef,
			Ref:     ref,
			Message: fmt.Sprintf("%s 既不在本包里、也不在库里：装得下，但链是断的（等补；体检 DownstreamResolved=False）", ref),
		})
	}
	for _, a := range rendered.Capabilities {
		for _, ref := range a.Spec.Next {
			classify(ref)
		}
	}
	for _, s := range rendered.Sources {
		for _, t := range s.Spec.Targets {
			classify(strings.TrimSpace(t.Capability))
		}
	}
}

// existsRef 报告一个引用在库里解析不解析得到。
func (p *PlanResult) existsRef(name, ref string, existing Existing) bool {
	if v, ok := versionOf(ref); ok {
		_, found := existing.CapabilityByNameVersion(name, v)
		return found
	}
	_, found := existing.CapabilityMaxVersion(name)
	return found
}

// markUnreachable 是装完自检：包内**本次新建**的能力既无上游、也无源指向 → note unreachable。
//
// 上游 = 装完后的有效路由（新建行按包里的、复用行按库里的）∪ 库里已有的引用。
// 行 7（路由未生效）造成的孤儿就在这条 note 里被点名（§4.5 的例子）。
func (p *PlanResult) markUnreachable(effNext map[string][]string, effTargets map[string][]sourcespec.Target, existing Existing) {
	for _, pc := range p.Capabilities {
		if pc.Action != ActionCreate {
			continue
		}
		name := pc.Artifact.Metadata.Name
		if hasPackageUpstream(name, effNext, effTargets) || existing.HasUpstream(name) {
			continue
		}
		p.Notes = append(p.Notes, Note{
			Code: NoteUnreachable,
			Ref:  capRef(name, pc.Artifact.Metadata.Version),
			Message: fmt.Sprintf(
				"%s 装上了，但没有上游指向它：既没有能力的 next，也没有源的 targets（接不进链）",
				capRef(name, pc.Artifact.Metadata.Version)),
		})
	}
}

// hasPackageUpstream 报告包内有效路由里是否有指向 name 的引用。
func hasPackageUpstream(name string, effNext map[string][]string, effTargets map[string][]sourcespec.Target) bool {
	for _, refs := range effNext {
		for _, ref := range refs {
			if refName(ref) == name {
				return true
			}
		}
	}
	for _, targets := range effTargets {
		for _, t := range targets {
			if refName(strings.TrimSpace(t.Capability)) == name {
				return true
			}
		}
	}
	return false
}

// conflictError 把冲突拼成 §9.2 的 400 形状（code / conflicts / valuesDigest / hint 一个不丢）。
func (p *PlanResult) conflictError() *Error {
	code := CodeVersionContentImmutable
	for _, c := range p.Conflicts {
		if c.Kind == "source" {
			code = CodePlaybookConflict
			break
		}
	}
	hint := "给该能力改一个 metadata.version 再装；或删掉库里那一行重建"
	if code == CodePlaybookConflict {
		hint = "源的 kind/config 不可通过安装覆盖：轮换 token / 改 config 请走源的 PATCH；要换身份就换一个源名"
	}
	e := errf(code, "预案冲突：%d 条需要覆盖已有定义，安装被拒绝（一行不写）", len(p.Conflicts))
	return e.withDetails(map[string]any{
		"valuesDigest": p.ValuesDigest,
		"conflicts":    p.Conflicts,
		"hint":         hint,
	})
}

// InstallMeta 是回执里"这次安装"的元信息（id / 时间 / dryRun / ns / OCI 来源）。
type InstallMeta struct {
	ID        string
	At        string
	DryRun    bool
	Namespace string
	// Source 是 OCI 来源（`oci://…@sha256:…`）；手传包体时为空。
	Source string
}

// BuildReceipt 把计划拼成回执（playbook.md §4.5）。
//
// 形状规则：`values` 打码（writeOnly → `***`）、`valuesDigest` 对**打码前**的值算、
// `dryRun` 标记透传、**没有 conflicts 字段**（冲突只在 400 错误体里）。
// values / valuesDigest 由调用方传（正常就是 plan.Values / plan.ValuesDigest）；
// 打码跟 plan 的 spec.values 走，所以调用方不需要再拿一份 schema。
func BuildReceipt(plan *PlanResult, values map[string]any, valuesDigest string, meta InstallMeta) Receipt {
	if plan == nil {
		return Receipt{
			APIVersion: APIVersion,
			Kind:       KindInstallReceipt,
			Install:    InstallRef{ID: meta.ID, At: meta.At, DryRun: meta.DryRun},
			Namespace:  meta.Namespace,
			Source:     meta.Source,
			Values:     map[string]any{},
			Created:    emptyCreated(),
			Skipped:    emptyCreated(),
			Upgraded:   []UpgradedEntry{},
			Unapplied:  []UnappliedEntry{},
			External:   []string{},
			Pending:    []string{},
			Notes:      []Note{},
		}
	}
	if values == nil {
		values = plan.Values
	}
	if valuesDigest == "" {
		valuesDigest = plan.ValuesDigest
	}
	return Receipt{
		APIVersion:   APIVersion,
		Kind:         KindInstallReceipt,
		Playbook:     plan.Playbook,
		Source:       meta.Source,
		Install:      InstallRef{ID: meta.ID, At: meta.At, DryRun: meta.DryRun},
		Namespace:    meta.Namespace,
		Values:       MaskValues(plan.schema, values),
		ValuesDigest: valuesDigest,
		Created:      filledCreated(plan.Created),
		Skipped:      filledCreated(plan.Skipped),
		Upgraded:     filled(plan.Upgraded, []UpgradedEntry{}),
		Unapplied:    filled(plan.Unapplied, []UnappliedEntry{}),
		External:     filled(plan.External, []string{}),
		Pending:      filled(plan.Pending, []string{}),
		Notes:        filled(plan.Notes, []Note{}),
	}
}

// filled 保证序列化出 `[]` 而不是 `null`（空列表纪律）。
func filled[T any](in, empty []T) []T {
	if in == nil {
		return empty
	}
	return in
}

// filledCreated 保证 created / skipped 的两个列表都是 `[]`。
func filledCreated(c Created) Created {
	return Created{
		Capabilities: filled(c.Capabilities, []string{}),
		Sources:      filled(c.Sources, []string{}),
	}
}

// ---------- config 归一化比较（§4.2 的五条要点） ----------

// inheritServerGenerated 把"包里省略、库里已有"的**服务端生成字段**补进声明侧再比较。
//
// 只有契约明确"服务端生成 / 可省略"的字段走这条规则：`webhook.config.token`
// （source.md §3.1：空 = 服务端随机生成）。`secret` 不走——它的默认值就是"空 = 不校验签名"，
// 把它也当"省略 = 保留"会把"这个包不再要求签名"这个真实差异吞掉（§4.2 最后一条：
// 其它省略字段不能一律当等价）。
func inheritServerGenerated(kind string, declared, existing json.RawMessage) (json.RawMessage, error) {
	if kind != sourcespec.KindWebhook {
		return declared, nil
	}
	if sourcespec.ParseConfig(kind, declared).Token != "" {
		return declared, nil
	}
	existingToken := sourcespec.ParseConfig(kind, existing).Token
	if existingToken == "" {
		return declared, nil
	}
	fields, err := decodeConfigFields(declared)
	if err != nil {
		return nil, err
	}
	fields["token"], _ = json.Marshal(existingToken)
	merged, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// needsGeneratedToken 报告本次新建的源是否需要服务端生成 token。
func needsGeneratedToken(spec sourcespec.Spec) bool {
	return spec.Kind == sourcespec.KindWebhook && sourcespec.ParseConfig(spec.Kind, spec.Config).Token == ""
}

// configDiffFields 列出两份 config 真正的差异字段（归一化后逐字段比）。
//
// 只给**字段名**（`config.token`）不给值：token/secret 的差异必须脱敏（D16 / §4.2）。
func configDiffFields(kind string, declared, actual json.RawMessage) []string {
	nd, err := sourcespec.NormalizeConfig(kind, declared)
	if err != nil {
		return []string{"config"}
	}
	na, err := sourcespec.NormalizeConfig(kind, actual)
	if err != nil {
		return []string{"config"}
	}
	df, err := decodeConfigFields(nd)
	if err != nil {
		return []string{"config"}
	}
	af, err := decodeConfigFields(na)
	if err != nil {
		return []string{"config"}
	}
	keys := make(map[string]bool, len(df)+len(af))
	for k := range df {
		keys[k] = true
	}
	for k := range af {
		keys[k] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		dv, dok := df[k]
		av, aok := af[k]
		if dok == aok && jsonEqual(dv, av) {
			continue
		}
		out = append(out, "config."+k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		// 归一化说"不等"但逐字段没找到差异（不该发生）：退化成整段点名，不静默。
		return []string{"config"}
	}
	return out
}

// decodeConfigFields 把 config 解成键值表（空 / null → 空表）。
func decodeConfigFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(raw))) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return fields, nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, nil
}

// jsonEqual 比较两段 JSON 的语义是否相同（canonical 后逐字节比）。
func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	ba, err := json.Marshal(va)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(vb)
	if err != nil {
		return false
	}
	return string(ba) == string(bb)
}

// sameRefSet 比较两组引用是否**语义相同**（`next` 是封闭集合：顺序无关、重复无意义）。
func sameRefSet(a, b []string) bool {
	sa := refSet(a)
	sb := refSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// refSet 把引用列表归一成有序去重列表。
func refSet(refs []string) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// versionOf 拆 `name@version` 的 version（没有 @ 或版本不是正整数 → false）。
func versionOf(ref string) (int, bool) {
	i := strings.LastIndexByte(ref, '@')
	if i < 0 {
		return 0, false
	}
	n, ok := toIntFromString(ref[i+1:])
	if !ok || n < 1 {
		return 0, false
	}
	return n, true
}

// formatRefs 给 note 的人话拼一串引用（空列表写 `[]`）。
func formatRefs(refs []string) string {
	if len(refs) == 0 {
		return "[]"
	}
	return "[" + strings.Join(refs, ", ") + "]"
}

// formatTargets 给 note 的人话拼一串 targets（只写能力名 + params 是否存在）。
func formatTargets(targets []sourcespec.Target) string {
	if len(targets) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		if len(t.Params) == 0 {
			parts = append(parts, t.Capability)
			continue
		}
		parts = append(parts, t.Capability+"{params}")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
