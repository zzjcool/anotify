// Package playbook 实现 v4 的**目录安装包**（Playbook）：解析、渲染、安装计划与回执。
//
// 语义基线是 `docs/design/v4/playbook.md`（一个目录 + values 参数化 + 只做加法），
// 本包只做**纯领域**计算，不碰数据库、不碰网络、不写任何东西：
//
//	Parse  包入库前的**值无关**校验（文件集合：入口 / 闭合 / 路径 / 计数 / 槽位形状）
//	Render (包, values) → 普通文档（默认值填充 + JSON Schema 校验 + 结构内插值 + 单建校验）
//	Plan   (包, values, 库快照) → 12 格动作矩阵（纯函数；有冲突时调用方整包 0 写）
//	BuildReceipt 把计划拼成回执行（values 打码、valuesDigest 对打码前值）
//
// 三条硬边界（写代码时不要越过去）：
//
//  1. **渲染严格只读包体**：不读库、不判冲突、不写库、不产生回执（§2.4 / D19）。
//     `render` 是 `helm template`，`dryRun` 才读库。
//  2. **包层模板只有 `{{values.点号路径}}` 一种**：其余 `{{…}}`、`$NAME`、`${…}`、
//     `$(…)` 全是普通文本，原样透传（§1.4 / D3）。只渲染值，不渲染键。
//  3. **安装只做加法**：本包算出"哪一格"，写入由 store 在单事务里执行（G1-5b）；
//     任何"想改已有东西"的差异一律进回执（`unapplied[]`），不进数据库（I-P1）。
//
// 本包的库依赖只有 `Existing` 这一个**只读快照接口**：store 在**当前事务内**实现它
// （按 name/version/foldKey 查能力、按 name 查源、算引用计数），本包既不知道表名，
// 也不私开连接。这样 `Plan` 是纯函数，`dryRun` 与真实 install 走同一条代码路径。
package playbook

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/sourcespec"
)

// 包格式常量（playbook.md §1）。
const (
	// EntryFile 是包的唯一入口：`playbook.yaml`（缺了就没法寻址，`entry_missing`）。
	EntryFile = "playbook.yaml"
	// APIVersion 是入口文档的格式版本。
	APIVersion = "anotify/v1"
	// KindPlaybook 是入口文档的 kind。
	KindPlaybook = "Playbook"
	// KindCapability / KindSource 是被引用文档的 kind（可省略不看，写了就必须对）。
	KindCapability = "Capability"
	KindSource     = "Source"
	// KindInstallReceipt 是回执的 kind（§4.5：回执是主文档 top-level，不是资源信封）。
	KindInstallReceipt = "InstallReceipt"
	// DigestPrefix 与容器镜像生态一致（valuesDigest / conflicts 里的 digest 都用它）。
	DigestPrefix = "sha256:"
)

// 体积与数量上界（playbook.md §1.5）。
const (
	// MaxFileBytes 是包内单个文件的体积上限（2 MiB = 单能力 run.files 1 MiB 的 YAML 余量）。
	MaxFileBytes = 2 << 20
	// MaxYAMLBytes 是包内所有 YAML 合计上限（渲染前）。
	MaxYAMLBytes = 8 << 20
	// MaxFiles 是文件数上限（含 playbook.yaml；D-I5）。
	MaxFiles = 64
	// MaxCapabilities / MaxSources 是包内能力数 / 源数上限。
	MaxCapabilities = 32
	MaxSources      = 32
	// MaxPathLen 是引用路径长度上限。
	MaxPathLen = 255
	// MaxNameLen 是包名（slug）长度上限。
	MaxNameLen = 64
)

// 错误码（playbook.md §9.2 的封闭词表 + §11 分发期码）。
//
// 只有这里列出的码可以出现在 *Error 上：新增一个码要改设计文档（码是 e2e 断言的锚点）。
const (
	// 创建期。
	CodeInvalidPlaybook    = "invalid_playbook"
	CodeSlugTaken          = "slug_taken"
	CodeSlugInvalid        = "slug_invalid"
	CodeNamespaceInPackage = "namespace_in_package"
	CodeTooManyItems       = "too_many_items"
	CodeEntryMissing       = "entry_missing"
	CodeFilePathInvalid    = "file_path_invalid"
	CodeFileMissing        = "file_missing"
	CodeFileUnreferenced   = "file_unreferenced"
	CodeFileDuplicate      = "file_duplicate"

	// 渲染期。
	CodeValuesMissing       = "values_missing"
	CodeValuesInvalid       = "values_invalid"
	CodeRenderNameInvalid   = "render_name_invalid"
	CodeRenderTypeMismatch  = "render_type_mismatch"
	CodeDuplicateCapability = "duplicate_capability"
	CodeDuplicateSource     = "duplicate_source"
	CodeCycle               = "cycle"

	// 安装期。
	CodePlaybookConflict        = "playbook_conflict"
	CodeVersionContentImmutable = "version_content_immutable"
	CodeNotFound                = "not_found"

	// 分发期（OCI）。本包不做网络拉取，只留码位给将来的一致口径（§11）。
	CodeOCIUnsupported    = "oci_unsupported"
	CodeOCIPullFailed     = "oci_pull_failed"
	CodeOCIDigestMismatch = "oci_digest_mismatch"
	CodeOCITooLarge       = "oci_too_large"
)

// 回执 notes 的 code 封闭集合（§4.5 六个码，一期不加）。
//
// 每个 note 是 {code, ref, message}：code 是稳定机器码（e2e 断言它），message 只给人读。
const (
	// NoteRoutingUnapplied 是"已存在的能力，包里 next 与库里不同"（§4.2 第 7 行）。
	NoteRoutingUnapplied = "routing_unapplied"
	// NoteTargetsUnapplied 是"已存在的源，包里 targets 与库里不同"（§4.2 第 12 行）。
	NoteTargetsUnapplied = "targets_unapplied"
	// NoteUnreachable 是装完自检：包内能力既无上游、也无源指向（§4.5）。
	NoteUnreachable = "unreachable"
	// NotePendingRef 是 next / targets 指向的名字库里也没有（§3 第三行）。
	NotePendingRef = "pending_ref"
	// NoteSourceDisabled 是包内源渲染后 enabled: false（只是提示）。
	NoteSourceDisabled = "source_disabled"
	// NoteNoNode 是包内没有任何能力能在当前节点池上跑。它需要节点池事实（哪些节点声明了
	// 什么），属于读模型的体检/派生 status（§6.1，G3），本包的纯 Plan 不产出它；
	// 常量放在这里是为了让码表在一处封闭。
	NoteNoNode = "no_node"
)

// NoteCodes 返回 notes 的封闭码表（拷贝；调用方可安全修改）。
func NoteCodes() []string {
	return []string{
		NoteRoutingUnapplied, NoteTargetsUnapplied, NoteUnreachable,
		NotePendingRef, NoteSourceDisabled, NoteNoNode,
	}
}

// Conflict 的原因码（400 的 conflicts[].reason，§9.2 的 playbook_conflict 响应形状）。
const (
	// ConflictVersionConflict 是显式 name@v 已存在且执行正文不同（§4.2 第 6 行）。
	ConflictVersionConflict = "version_conflict"
	// ConflictConfigDiffers 是源同名且 kind/config 真正不同（§4.2 第 10 行）。
	ConflictConfigDiffers = "config_differs"
)

// Unapplied 的原因码（回执 unapplied[].reason，与 notes 的 code 同名同义）。
const (
	ReasonRoutingUnapplied = NoteRoutingUnapplied
	ReasonTargetsUnapplied = NoteTargetsUnapplied
)

// Error 是带码的领域错误：Code 来自 §9.2 的封闭词表，Message 是点名到路径/字段的人话，
// Details 放结构化补充（conflicts / valuesDigest / hint 这类）。
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// errf 造一个带码错误。
func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// withDetails 给错误挂结构化补充（返回同一个错误，便于 `return nil, errf(...).with(...)`）。
func (e *Error) withDetails(details map[string]any) *Error {
	if len(details) == 0 {
		return e
	}
	if e.Details == nil {
		e.Details = make(map[string]any, len(details))
	}
	for k, v := range details {
		e.Details[k] = v
	}
	return e
}

// Code 返回错误的领域码；不是本包错误时返回空串（调用方据此判断"是不是可翻译成 400 的领域错"）。
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsCode 报告 err 是否带指定领域码。
func IsCode(err error, code string) bool { return Code(err) == code }

// Package 是一个已解析的目录包。
//
// **Files 是包体的唯一真相**：它按 `{相对路径: 正文}` 保存原始文件（含 `playbook.yaml`），
// store 直接把它当 `playbooks.files` 落库（D-I5），`includeBody` 也原样返回它。
// 渲染时按 CapabilityPaths / SourcePaths 从 Files 取正文——不另存一份正文副本，
// 避免"改了 Files 忘了 Docs"这类两份真相。
//
// Values 是 `spec.values` 的 canonical JSON（JSON Schema）；省略的包是 `{}`（接受一切、无默认值）。
type Package struct {
	// Name 是 metadata.name（slug，命名空间内唯一，也是 URL 里那段）。
	Name string `json:"name"`
	// DisplayName / Description 是纯展示。
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	// Version 是 metadata.version（只管展示与溯源，不参与 digest / 幂等 / 任何解析）。
	Version int `json:"version,omitempty"`
	// Values 是 spec.values（JSON Schema 原文，canonical JSON）。
	Values json.RawMessage `json:"values"`
	// Files 是包体（相对路径 → 正文），含 EntryFile 原文。
	Files map[string]string `json:"files"`
	// CapabilityPaths / SourcePaths 是 spec.capabilities / spec.sources（相对路径，按书写顺序）。
	CapabilityPaths []string `json:"capabilityPaths"`
	SourcePaths     []string `json:"sourcePaths"`
}

// Source 是一份**渲染后**的源文档（源没有 version：身份就是名字，kind 创建后不可变）。
type Source struct {
	// Name 是 metadata.name（渲染后）。
	Name string `json:"name"`
	// DisplayName 是 metadata.displayName（纯展示，没有就不存在）。
	DisplayName string `json:"displayName,omitempty"`
	// Spec 是 source.md §2 的 spec 段（config 已归一化，token 空 = 待服务端生成）。
	Spec sourcespec.Spec `json:"spec"`
}

// RenderResult 是一次渲染的产物（playbook.md §2.4 的 render 响应体）。
//
// 它是**普通文档集合**：能力走 artifact 的归一化+校验，源走 sourcespec 的校验，
// 装出来的东西与手工建的逐字节等价（D6）。
type RenderResult struct {
	// Values 是**有效** values（传入 + 补上的默认值，打码前）。
	Values map[string]any `json:"values"`
	// ValuesDigest 对打码前的有效 values 取 canonical sha256（§4.5）。
	ValuesDigest string `json:"valuesDigest"`
	// Files 是渲染后的 `{路径: 正文}`（只含被引用的能力/源文件；playbook.yaml 不渲染，故不在内）。
	Files map[string]string `json:"files"`
	// Capabilities 是渲染后的能力（artifact.Parse 归一化过：默认值已补齐）。
	Capabilities []*artifact.Artifact `json:"capabilities"`
	// Sources 是渲染后的源文档。
	Sources []Source `json:"sources"`
}

// PlaybookRef 是回执里的包身份（§4.5）。
type PlaybookRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// InstallRef 是回执里的本次安装标识（id 由 store 生成；dryRun 不进库）。
type InstallRef struct {
	ID     string `json:"id"`
	At     string `json:"at"`
	DryRun bool   `json:"dryRun"`
}

// Created 是"这次建了什么"：能力写 `name@version`，源写名字。
//
// **两份（created / skipped）都是对象而不是数组**：`{capabilities: [], sources: []}`
// （W3 冻结；空列表是 `[]` 不是 `null`）。
type Created struct {
	Capabilities []string `json:"capabilities"`
	Sources      []string `json:"sources"`
}

// Affects 是新版本会影响到的落点计数（§5 D23：两个计数都要算）。
type Affects struct {
	// Edges 是库里引用**裸名** next 的边数。
	Edges int `json:"edges"`
	// Sources 是库里 targets 里写裸名的源数。
	Sources int `json:"sources"`
}

// UpgradedEntry 是"同名能力长出了新版本"（§4.2 第 3 行）。
type UpgradedEntry struct {
	Capability string  `json:"capability"`
	From       int     `json:"from"`
	To         int     `json:"to"`
	Affects    Affects `json:"affects"`
}

// UnappliedEntry 是"装了，但库里已有行不许改 → 这条差异没生效"（§4.2 第 7 / 12 行）。
//
// Declared / Actual 是 JSON 值：能力路由是 `[]string`，源 targets 是 `[]sourcespec.Target`。
type UnappliedEntry struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Field    string `json:"field"`
	Declared any    `json:"declared"`
	Actual   any    `json:"actual"`
	Reason   string `json:"reason"`
}

// Note 是一条"稳定机器码 + 人话"的提示（§4.5：code 封闭六码）。
type Note struct {
	Code    string `json:"code"`
	Ref     string `json:"ref,omitempty"`
	Message string `json:"message"`
}

// Receipt 是安装回执（playbook.md §4.5，D15）。
//
// 形状硬约束：
//   - `created` / `skipped` 都是 `{capabilities: [], sources: []}` 对象；
//   - `values` 是**打码后**的有效 values（writeOnly → `"***"`），`valuesDigest` 对**打码前**算；
//   - **没有 `conflicts` 字段**：冲突只在 400 错误体里（`Error.Details`）。
type Receipt struct {
	APIVersion   string           `json:"apiVersion"`
	Kind         string           `json:"kind"`
	Playbook     PlaybookRef      `json:"playbook"`
	Source       string           `json:"source,omitempty"`
	Install      InstallRef       `json:"install"`
	Namespace    string           `json:"namespace"`
	Values       map[string]any   `json:"values"`
	ValuesDigest string           `json:"valuesDigest"`
	Created      Created          `json:"created"`
	Skipped      Created          `json:"skipped"`
	Upgraded     []UpgradedEntry  `json:"upgraded"`
	Unapplied    []UnappliedEntry `json:"unapplied"`
	External     []string         `json:"external"`
	Pending      []string         `json:"pending"`
	Notes        []Note           `json:"notes"`
}

// emptyCreated 保证 created / skipped 的列表序列化成 `[]` 而不是 `null`（空列表纪律）。
func emptyCreated() Created { return Created{Capabilities: []string{}, Sources: []string{}} }

// InstallsFoldKey 是实例清单（§4.10）的折叠键：`(namespace_id, playbook_id, values_digest)`。
//
// 纯函数部分（拼键）留在这里，一次 `GROUP BY` 的聚合由 store 做（G1-5b）：同一组 values
// 装 3 次 = 1 个折叠组 + attempts=3，不是 3 个实例。
func InstallsFoldKey(namespaceID, playbookID, valuesDigest string) string {
	return namespaceID + "\x00" + playbookID + "\x00" + valuesDigest
}
