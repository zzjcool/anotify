// Package artifact 定义「能力产物」（Capability Artifact）的 v4 线上格式、校验与
// 内容寻址标识。
//
// 一份产物就是一份自包含的能力定义：**我要什么（params）、交给谁（next）、怎么
// 跑（run）**，外加节点要提供什么（requires）、生命周期（lifecycle）与并发预算
// （concurrency）。实现形式**只有脚本一种**：没有 handler、没有中枢执行器、
// 没有 outputs/port/sticky（capability.md §1 / §7）。
//
// # 为什么自包含
//
// 产物不引用任何外部 blob 或 URL：节点拿到它就能跑，中枢只派发与记账。
//
// # 两级内容身份（capability.md §3）
//
//	foldKey = sha256(canonical{name, params, requires, lifecycle, concurrency, run})
//	digest  = sha256(canonical{name, version, params, requires, lifecycle, concurrency, run})
//
// foldKey 判「是不是同一行」：省略 version 时，先按 foldKey 复用已有行、没有才
// 分配下一个 version；digest 是部署身份，含 version。
//
// 刻意不进身份的两个字段：
//
//   - metadata.displayName：纯展示，改它不该让部署变 stale；
//   - spec.next：路由属于图，不属于内容；改线不产生新版本、不重推节点。
//
// 两级身份的直接推论：**改 next 不产生新版本；改 run.files 一定产生新版本**。
// 显式 version 的正文不可变是同一条边界的另一面（store 在写入事务里用
// SameContent / SameFold 判定，见 §3「显式 version 的内容不可变」）。
//
// # 归一化
//
// Normalize 补齐默认值（省略 ≡ 显式默认）并把 params 归一成 key 有序的 JSON：
// 内容寻址必须与「输入怎么排版」无关，否则同一份产物在不同书写顺序下算出不同
// digest，部署永远被判成 stale/local。
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// APIVersion 是本包唯一支持的格式版本。
	APIVersion = "anotify/v1"

	// KindCapability 是唯一支持的 kind。
	KindCapability = "Capability"

	// DigestPrefix 是内容寻址标识的前缀（与容器镜像生态一致）。
	DigestPrefix = "sha256:"

	// MaxFileBytes 是单个文件的体积上限（256 KiB）。
	MaxFileBytes = 256 << 10
	// MaxTotalBytes 是一份产物全部文件的合计上限（1 MiB）。
	MaxTotalBytes = 1 << 20
	// MaxFiles 是一份产物最多的文件数。
	MaxFiles = 255
	// MaxFilePathLen 是产物内相对路径的长度上限。
	MaxFilePathLen = 255
	// MaxNext 是 spec.next 的元素上限（capability.md §2.3）。
	MaxNext = 32
	// MaxRequires 是 requires.commands / requires.values 各自的上限。
	MaxRequires = 16
	// MaxTimeoutSec 是本地执行超时上限（24 小时）。
	MaxTimeoutSec = 86400
	// MaxNameLen 是能力名长度上限（与节点名同口径）。
	MaxNameLen = 63
	// MaxWorkDirLen 是 run.workDir 的长度上限。
	MaxWorkDirLen = 1024
)

// spec 默认值（capability.md §2.3）。归一化把这些填进产物，所以「省略」与
// 「显式写默认值」是同一份内容、同一个 digest。
const (
	// DefaultMaxAttempts 是总尝试次数（含第一次）。
	DefaultMaxAttempts = 3
	// DefaultResultTTLSec 是结果保留期（7 天）；0 = 永不清理。
	DefaultResultTTLSec = 604800
	// DefaultMaxPerNode 是同一能力在一台节点上的并发预算。
	DefaultMaxPerNode = 1
)

// 问题短码（capability.md §6.1 的封闭集合；BadRequest 是 api-conventions.md §4.2
// 的通用码，用于 §6.1 没有单列码的信封/取值形状错误）。
const (
	// CodeBadRequest 是信封与取值形状的通用写期错误（apiVersion/name/version/
	// params 形状/next 形状/lifecycle 取值）。
	CodeBadRequest = "BadRequest"
	// CodeUnknownField 是未知字段（DisallowUnknownFields / YAML 解析不出）。
	CodeUnknownField = "unknown_field"
	// CodeInvalidYAML 是 YAML 本身解析不了。
	CodeInvalidYAML = "invalid_yaml"
	// CodeKindMismatch 是 kind 不是 Capability。
	CodeKindMismatch = "kind_mismatch"

	// CodeEntrypointNoShebang 是 entrypoint 缺失 / 不在 files 里 / 内容不以 #! 开头。
	CodeEntrypointNoShebang = "entrypoint_no_shebang"
	// CodeFilesInvalid 是 files 的路径形状或限额不合格（写入期不解析符号链接）。
	CodeFilesInvalid = "files_invalid"
	// CodeEnvInvalid 是 env 的键名、保留前缀、PATH 覆盖或 HOME 字面量不合格。
	CodeEnvInvalid = "env_invalid"
	// CodeWorkDirInvalid 是 workDir 超长/含 NUL/不是绝对路径/含 .. 或非常规写法。
	CodeWorkDirInvalid = "workdir_invalid"
	// CodeUndeclaredNeed 是 run.env 引用了 vars.* 却没写进 requires.values。
	CodeUndeclaredNeed = "undeclared_need"
	// CodeUnknownFieldPath 是 fieldPath 不在 README.md 的封闭集合里。
	CodeUnknownFieldPath = "unknown_field_path"
	// CodeNonPathField 是 workDir 引用的 env 值不是目录类事实。
	CodeNonPathField = "non_path_field"
	// CodeUndefinedEnvRef 是 workDir 引用了 run.env 里没声明的名字。
	CodeUndefinedEnvRef = "undefined_env_ref"
	// CodeUndefinedParamRef 是 workDir 引用了 spec.params 里没声明的参数。
	CodeUndefinedParamRef = "undefined_param_ref"
	// CodeConcurrencyInvalid 是 concurrency.maxPerNode < 1 或不是整数。
	CodeConcurrencyInvalid = "concurrency_invalid"
	// CodeParamEnvCollision 是两个参数名归一后撞同一个 ANOTIFY_PARAM_*。
	CodeParamEnvCollision = "param_env_collision"
	// CodeRequiresInvalid 是 requires 不是对象 / 元素不合格 / 超上限 / 未知子键。
	CodeRequiresInvalid = "requires_invalid"
	// CodeVersionContentImmutable 是显式同版内容不同（store 判定，本包给辅助）。
	CodeVersionContentImmutable = "version_content_immutable"
)

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	varNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	// workDirRefPattern 只认 $(vars.NAME) / $(params.NAME)：其它 $ 写法一律不认。
	workDirRefPattern = regexp.MustCompile(`\$\((vars|params)\.([A-Za-z_][A-Za-z0-9_]*)\)`)
)

// builtinFieldPaths 是 fieldPath 封闭集合里的内置事实（README.md「fieldPath 封闭
// 集合」）。值是"能不能当 workDir 的最终值"：目录类才允许被 workDir 引用。
//
// vars.<NAME> 不在这张表里：它是开放命名空间（名字由本机/节点默认/ns 默认提供），
// 但同样是目录类。
var builtinFieldPaths = map[string]bool{
	"metadata.name":       false,
	"status.hostIP":       false,
	"status.hostname":     false,
	"status.os":           false,
	"status.arch":         false,
	"status.capacity.cpu": false,
	"status.workRoot":     true,
	"status.homeDir":      true,
}

// Artifact 是一份能力产物。
type Artifact struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata 是产物的身份。
//
// version 省略（= 0）表示**内容寻址**：按 (name, 执行正文) 复用已有行；显式给就
// 永不折叠。displayName 是纯展示，不进 digest。
type Metadata struct {
	Name        string `json:"name" yaml:"name"`
	Version     int    `json:"version,omitempty" yaml:"version,omitempty"`
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
}

// Spec 是产物的全部内容。顶层字段严格是这六个（capability.md §2.3）。
//
// 字段声明顺序即 canonical JSON 的字段顺序，顺序参与 digest —— 改动顺序会让所有
// 已有产物的 digest 失效。
type Spec struct {
	// Params 是任务的输入契约（JSON Schema 对象；省略 = {} = 怎么传都过）。
	Params json.RawMessage `json:"params"`
	// Next 是下游引用（`name` 或 `name@version`；省略 = [] = 终端能力）。
	// 它是路由事实，**不参与 foldKey / digest**。
	Next []string `json:"next"`
	// Run 是怎么跑（脚本，唯一实现形式）。
	Run Run `json:"run"`
	// Requires 是节点要提供什么。
	Requires Requires `json:"requires"`
	// Lifecycle 是一次运行能活多久、能不能重放。
	Lifecycle Lifecycle `json:"lifecycle"`
	// Concurrency 是这台节点上最多同时跑几个。
	Concurrency Concurrency `json:"concurrency"`
}

// Run 是「怎么跑」。
type Run struct {
	// Entrypoint 必须是 Files 里的一个 key，且内容以 "#!" 开头（直接 exec，不经 shell）。
	Entrypoint string `json:"entrypoint"`
	// Files 是相对路径 → 文件内容；全部展开进工作目录（0600，入口额外 0700）。
	Files map[string]string `json:"files"`
	// Env 是机器事实的唯一入口：键名就是脚本里的 $NAME，值 = 字面量或 valueFrom。
	Env map[string]EnvValue `json:"env,omitempty"`
	// WorkDir 是执行目录模板：空 = 每次任务一个独立目录；否则绝对路径模板，
	// 可含 $(vars.NAME)（必须 ∈ Env）或 $(params.NAME)（必须 ∈ Params）。
	// 解析后必须是绝对路径、无 ..、已存在（执行前检查）。
	WorkDir string `json:"workDir,omitempty"`
}

// EnvValue 是 env 的一个值：字面量，或 `{valueFrom:{fieldRef:{fieldPath}}}`。
//
// FieldPath 非空 = 引用机器事实（唯一的取值边界）；否则 Value 是字面量。
// 空串本身是合法字面量（"空串就是空串"），所以这里不能用 "" 表示「未设置」。
type EnvValue struct {
	// Value 是字面量值。
	Value string
	// FieldPath 是 k8s 风格的取值路径（封闭集合见 builtinFieldPaths / vars.<NAME>）。
	FieldPath string
}

// IsValueFrom 报告这个 env 值是不是 fieldRef 引用。
func (v EnvValue) IsValueFrom() bool { return v.FieldPath != "" }

// envFieldRef / envValueFrom / envRefValue 是 wire 上的 k8s 风格引用形状。
type envFieldRef struct {
	FieldPath string `json:"fieldPath"`
}

type envValueFrom struct {
	FieldRef envFieldRef `json:"fieldRef"`
}

type envRefValue struct {
	ValueFrom envValueFrom `json:"valueFrom"`
}

// MarshalJSON 把 env 值写成 wire 形状：字面量是字符串，引用是 k8s 风格的 valueFrom。
func (v EnvValue) MarshalJSON() ([]byte, error) {
	if v.FieldPath == "" {
		return json.Marshal(v.Value)
	}
	return json.Marshal(envRefValue{ValueFrom: envValueFrom{FieldRef: envFieldRef{FieldPath: v.FieldPath}}})
}

// UnmarshalJSON 解析字面量或 valueFrom 两种形状。
//
// 严格拒绝其它形状（含数字/布尔/null）：env 的值类型是 string（capability.md
// §2.3），把 `JOBS: 4` 静默转成 "4" 会让「类型写错了」变成「跑起来才发现」。
func (v *EnvValue) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return &fieldError{code: CodeEnvInvalid, msg: "env 值为空"}
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return &fieldError{code: CodeEnvInvalid, msg: "env 字面量不是合法字符串: " + err.Error()}
		}
		*v = EnvValue{Value: s}
		return nil
	}
	var ref struct {
		ValueFrom *struct {
			FieldRef *struct {
				FieldPath string `json:"fieldPath"`
			} `json:"fieldRef"`
		} `json:"valueFrom"`
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil {
		return &fieldError{code: CodeEnvInvalid,
			msg: "env 值必须是字面量字符串或 {valueFrom:{fieldRef:{fieldPath}}}: " + err.Error()}
	}
	if ref.ValueFrom == nil || ref.ValueFrom.FieldRef == nil || ref.ValueFrom.FieldRef.FieldPath == "" {
		return &fieldError{code: CodeEnvInvalid,
			msg: "env 的 valueFrom 必须给出 fieldRef.fieldPath"}
	}
	*v = EnvValue{FieldPath: ref.ValueFrom.FieldRef.FieldPath}
	return nil
}

// Requires 是「节点要提供什么」：要装的命令 + 要能解析出的值。
type Requires struct {
	// Commands 是命令名（节点 LookPath 探测；缺一条 → 该节点不匹配）。
	Commands []string `json:"commands"`
	// Values 是 `vars.<NAME>`（不写来源；解析顺序 local → node → namespace）。
	Values []string `json:"values"`
}

// UnmarshalJSON 严格解析 requires：未知子键是 requires_invalid，而不是被静默忽略。
//
// 为什么不用 DisallowUnknownFields：spec 的反序列化会在解码整份文档时一次性报
// "unknown field"，那个短码会盖掉「requires 里有个拼错的键」这个更准确的诊断。
func (r *Requires) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return &fieldError{code: CodeRequiresInvalid,
			msg: "requires 必须是对象（{commands,values}），实际 " + compactJSON(data)}
	}
	var out Requires
	for key, value := range raw {
		switch key {
		case "commands":
			if err := json.Unmarshal(value, &out.Commands); err != nil {
				return &fieldError{code: CodeRequiresInvalid, msg: "requires.commands 必须是字符串数组"}
			}
		case "values":
			if err := json.Unmarshal(value, &out.Values); err != nil {
				return &fieldError{code: CodeRequiresInvalid, msg: "requires.values 必须是字符串数组"}
			}
		default:
			return &fieldError{code: CodeRequiresInvalid,
				msg: fmt.Sprintf("requires 不认识的子键 %q（只有 commands / values）", key)}
		}
	}
	*r = out
	return nil
}

// Lifecycle 是一次运行的生命周期（capability.md §2.3）。
//
// 指针字段是"省略 vs 显式"的分界线：maxAttempts 省略 = 3、显式 0 = 非法；
// resultTtlSec 省略 = 604800、显式 0 = 永不清理。用 int 吞掉这个区别就会把
// 「写错了」变成「按默认值跑」。
type Lifecycle struct {
	// Idempotent 是「明确幂等」的声明：true 才允许对短暂基础设施失败自动重试。
	Idempotent bool `json:"idempotent"`
	// MaxAttempts 是总尝试次数（含第一次，≥1，默认 3）。
	MaxAttempts *int `json:"maxAttempts,omitempty"`
	// ResultTTLSec 是结果保留期（秒，≥0，默认 604800；0 = 永不清理）。
	ResultTTLSec *int `json:"resultTtlSec,omitempty"`
	// TimeoutSec 是本地执行超时（从子进程启动起算，0..86400；0 = 不设）。
	TimeoutSec int `json:"timeoutSec"`
}

// EffectiveMaxAttempts 返回生效的总尝试次数（省略 → 3）。
func (l Lifecycle) EffectiveMaxAttempts() int {
	if l.MaxAttempts == nil {
		return DefaultMaxAttempts
	}
	return *l.MaxAttempts
}

// EffectiveResultTTLSec 返回生效的结果保留期（省略 → 604800）。
func (l Lifecycle) EffectiveResultTTLSec() int {
	if l.ResultTTLSec == nil {
		return DefaultResultTTLSec
	}
	return *l.ResultTTLSec
}

// EffectiveTimeoutSec 返回生效的本地执行超时。
func (l Lifecycle) EffectiveTimeoutSec() int { return l.TimeoutSec }

// Concurrency 是「这台节点上最多同时跑几个」。
type Concurrency struct {
	// MaxPerNode ≥1（默认 1）。超了不失败：任务停在 waiting_for_slot。
	MaxPerNode *int `json:"maxPerNode,omitempty"`
}

// UnmarshalJSON 严格解析 concurrency：未知子键与非法形状都是 concurrency_invalid。
func (c *Concurrency) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return &fieldError{code: CodeConcurrencyInvalid,
			msg: "concurrency 必须是对象（{maxPerNode}），实际 " + compactJSON(data)}
	}
	var out Concurrency
	for key, value := range raw {
		switch key {
		case "maxPerNode":
			var n int
			if err := json.Unmarshal(value, &n); err != nil {
				return &fieldError{code: CodeConcurrencyInvalid, msg: "concurrency.maxPerNode 必须是整数"}
			}
			out.MaxPerNode = &n
		default:
			return &fieldError{code: CodeConcurrencyInvalid,
				msg: fmt.Sprintf("concurrency 不认识的子键 %q（只有 maxPerNode）", key)}
		}
	}
	*c = out
	return nil
}

// EffectiveMaxPerNode 返回生效的单节点并发预算（省略 → 1）。
func (c Concurrency) EffectiveMaxPerNode() int {
	if c.MaxPerNode == nil {
		return DefaultMaxPerNode
	}
	return *c.MaxPerNode
}

// fieldError 是解码期的带码错误：Parse 把它翻译成对应短码，而不是一律 unknown_field。
type fieldError struct {
	code string
	msg  string
}

func (e *fieldError) Error() string { return e.code + ": " + e.msg }

// Error 汇总一份产物上的全部问题。
//
// 一次报全部、不只报第一条：手写 YAML 时用户要的是「哪里全错了」，
// 而不是修一条、再跑一次、再发现下一条。
type Error struct {
	Problems []string
}

func (e *Error) Error() string { return strings.Join(e.Problems, "; ") }

// errIf 如果 problems 非空就返回一个 *Error，否则返回 nil。
func errIf(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return &Error{Problems: problems}
}

// Code 返回第一个问题的机器可读短码（用于 HTTP Status.reason / apply 帧的 reason）。
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) && len(e.Problems) > 0 {
		return ProblemCode(e.Problems[0])
	}
	return "invalid_artifact"
}

// ProblemCode 取**单条**问题文本的机器可读短码（"<短码>: <人话>" 的第一段）。
func ProblemCode(problem string) string {
	if i := strings.Index(problem, ":"); i > 0 {
		return problem[:i]
	}
	return problem
}

// ProblemMessage 取单条问题文本的人话部分（去掉短码前缀）。
func ProblemMessage(problem string) string {
	if i := strings.Index(problem, ":"); i > 0 {
		return strings.TrimSpace(problem[i+1:])
	}
	return problem
}

// Parse 从 YAML 或 JSON 解析一份产物并做完整校验。
//
// YAML 是 JSON 的超集，所以单条路径同时支持两种写法：先解成 map，再转 JSON，
// 最后严格解码进结构体（未知字段是错误——拼错的字段名静默失效比解析失败更难查）。
// 返回的产物是**归一化后**的（默认值已补齐、params 已 canonical），因此
// Parse → MarshalYAML → Parse 的 digest 永远相同。
func Parse(data []byte) (*Artifact, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, &Error{Problems: []string{CodeInvalidYAML + ": " + err.Error()}}
	}
	if raw == nil {
		return nil, &Error{Problems: []string{CodeInvalidYAML + ": 文档为空"}}
	}
	norm, err := json.Marshal(normalizeMaps(raw))
	if err != nil {
		return nil, &Error{Problems: []string{CodeInvalidYAML + ": " + err.Error()}}
	}

	var a Artifact
	dec := json.NewDecoder(bytes.NewReader(norm))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		var fe *fieldError
		if errors.As(err, &fe) {
			return nil, &Error{Problems: []string{fe.Error()}}
		}
		return nil, &Error{Problems: []string{CodeUnknownField + ": " + err.Error()}}
	}

	out, err := Normalize(&a)
	if err != nil {
		return nil, err
	}
	if err := Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeMaps 把 yaml.v3 可能产出的 map[any]any 递归换成 map[string]any。
func normalizeMaps(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeMaps(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[fmt.Sprint(k)] = normalizeMaps(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeMaps(val)
		}
		return out
	default:
		return v
	}
}

// canonicalizeJSON 把任意 JSON 归一成「key 有序、无多余空白」的紧凑形式。
func canonicalizeJSON(raw []byte) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// Normalize 返回归一化副本（**不修改调用方的对象**）：
//
//   - 省略的默认值补成显式值（所以「省略」与「显式写默认值」是同一份内容）；
//   - params 缺省成 `{}`、next/requires 的空列表补成 `[]`（空列表纪律：不是 null）；
//   - params 归一成 key 有序的紧凑 JSON（digest 与书写顺序无关）。
//
// 只补默认值、不改非法值：显式 `maxPerNode: 0` 归一后仍是 0，由 Validate 拒。
func Normalize(a *Artifact) (*Artifact, error) {
	if a == nil {
		return nil, &Error{Problems: []string{CodeBadRequest + ": 产物为空"}}
	}
	clone := *a
	clone.Spec = cloneSpec(a.Spec)

	params := clone.Spec.Params
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage(`{}`)
	}
	canon, err := canonicalizeJSON(params)
	if err != nil {
		return nil, &Error{Problems: []string{CodeBadRequest + ": spec.params 不是合法 JSON: " + err.Error()}}
	}
	clone.Spec.Params = canon
	return &clone, nil
}

// cloneSpec 深拷贝 spec（map / slice / 指针），保证 Normalize 不动调用方。
func cloneSpec(spec Spec) Spec {
	out := spec
	out.Next = append(make([]string, 0, len(spec.Next)), spec.Next...)
	out.Requires = Requires{
		Commands: append(make([]string, 0, len(spec.Requires.Commands)), spec.Requires.Commands...),
		Values:   append(make([]string, 0, len(spec.Requires.Values)), spec.Requires.Values...),
	}
	out.Run = Run{Entrypoint: spec.Run.Entrypoint, WorkDir: spec.Run.WorkDir}
	if spec.Run.Files != nil {
		out.Run.Files = make(map[string]string, len(spec.Run.Files))
		for k, v := range spec.Run.Files {
			out.Run.Files[k] = v
		}
	}
	if spec.Run.Env != nil {
		out.Run.Env = make(map[string]EnvValue, len(spec.Run.Env))
		for k, v := range spec.Run.Env {
			out.Run.Env[k] = v
		}
	}
	if spec.Lifecycle.MaxAttempts == nil {
		out.Lifecycle.MaxAttempts = intPtr(DefaultMaxAttempts)
	} else {
		out.Lifecycle.MaxAttempts = intPtr(*spec.Lifecycle.MaxAttempts)
	}
	if spec.Lifecycle.ResultTTLSec == nil {
		out.Lifecycle.ResultTTLSec = intPtr(DefaultResultTTLSec)
	} else {
		out.Lifecycle.ResultTTLSec = intPtr(*spec.Lifecycle.ResultTTLSec)
	}
	if spec.Concurrency.MaxPerNode == nil {
		out.Concurrency.MaxPerNode = intPtr(DefaultMaxPerNode)
	} else {
		out.Concurrency.MaxPerNode = intPtr(*spec.Concurrency.MaxPerNode)
	}
	return out
}

func intPtr(v int) *int { return &v }

// Validate 校验一份产物，返回汇总了**全部**问题的 *Error（或 nil）。
func Validate(a *Artifact) error {
	if a == nil {
		return &Error{Problems: []string{CodeBadRequest + ": 产物为空"}}
	}
	var p []string
	add := func(code, format string, args ...any) {
		p = append(p, code+": "+fmt.Sprintf(format, args...))
	}

	if a.APIVersion != APIVersion {
		add(CodeBadRequest, "apiVersion 期望 %q，实际 %q", APIVersion, a.APIVersion)
	}
	if a.Kind != KindCapability {
		add(CodeKindMismatch, "kind 期望 %q，实际 %q", KindCapability, a.Kind)
	}
	if !namePattern.MatchString(a.Metadata.Name) {
		add(CodeBadRequest, "能力名「%s」只能包含小写字母、数字、点、下划线、短横线，"+
			"长度 1..%d，且必须以字母或数字开头", a.Metadata.Name, MaxNameLen)
	}
	if a.Metadata.Version < 0 {
		add(CodeBadRequest, "version 必须 ≥1（省略 = 内容寻址），实际 %d", a.Metadata.Version)
	}

	params, paramProblems := validateParams(a.Spec.Params)
	p = append(p, paramProblems...)
	p = append(p, validateNext(a.Spec.Next)...)
	p = append(p, validateRequires(&a.Spec.Requires)...)
	p = append(p, validateRun(&a.Spec, params)...)
	p = append(p, validateLifecycle(&a.Spec.Lifecycle)...)
	p = append(p, validateConcurrency(&a.Spec.Concurrency)...)

	return errIf(p)
}

// paramInfo 是校验期从 params schema 里读出来的那点信息。
type paramInfo struct {
	// properties 是声明的顶层参数名（workDir 的 $(params.X) 只能引用这里）。
	properties map[string]struct{}
}

// validateParams 校验 params 是 JSON 对象，并检查 ANOTIFY_PARAM_* 归一撞名。
func validateParams(params json.RawMessage) (paramInfo, []string) {
	info := paramInfo{properties: map[string]struct{}{}}
	var p []string
	add := func(format string, args ...any) {
		p = append(p, CodeBadRequest+": "+fmt.Sprintf(format, args...))
	}
	if len(params) == 0 {
		return info, nil
	}

	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(params, &schema); err != nil {
		add("params 必须是 JSON Schema 对象（省略 = {}）：%s", err.Error())
		return info, p
	}

	// 两个参数名归一后撞同一个 ANOTIFY_PARAM_* → 写入期拒（capability.md §2.3）：
	// 运行期「谁覆盖谁」是隐式的，而隐式行为在排障时永远说不清。
	byEnvName := map[string][]string{}
	for name := range schema.Properties {
		info.properties[name] = struct{}{}
		envName := paramEnvName(name)
		byEnvName[envName] = append(byEnvName[envName], name)
	}
	collisions := make([]string, 0)
	for envName, names := range byEnvName {
		if len(names) > 1 {
			sort.Strings(names)
			collisions = append(collisions,
				fmt.Sprintf("%s（%s）", envName, strings.Join(names, "、")))
		}
	}
	if len(collisions) > 0 {
		sort.Strings(collisions)
		p = append(p, CodeParamEnvCollision+": 参数名归一后撞同一个环境变量："+strings.Join(collisions, "; "))
	}
	return info, p
}

// paramEnvName 是参数名 → ANOTIFY_PARAM_<NAME> 的归一规则（capability.md §2.3）：
// 全大写、非 [A-Z0-9] 的字符替换成 '_'，**不插大小写边界**
// （sha → SHA，repo-url → REPO_URL，repoUrl → REPOURL）。
func paramEnvName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "ANOTIFY_PARAM_" + b.String()
}

// validateNext 校验 spec.next 的形状（≤32 个，`name` 或 `name@version`）。
//
// 名字能不能解析（目标是否存在）是**写入路径**的事（单建报 unknown_target，
// 预案 install 允许先装上游），不是本包的静态校验。
func validateNext(next []string) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, CodeBadRequest+": "+fmt.Sprintf(format, args...))
	}
	if len(next) > MaxNext {
		add("spec.next 最多 %d 个下游，实际 %d", MaxNext, len(next))
	}
	for _, ref := range next {
		name, version, hasVersion := splitRef(ref)
		if name == "" {
			add("spec.next 的元素 %q 为空：必须是 name 或 name@version", ref)
			continue
		}
		if !namePattern.MatchString(name) {
			add("spec.next 的元素 %q：能力名不合法（%s）", ref, namePattern.String())
			continue
		}
		if !hasVersion {
			continue
		}
		if version < 1 {
			add("spec.next 的元素 %q：@version 必须是 ≥1 的整数", ref)
		}
	}
	return p
}

// splitRef 拆 `name@version`（没有 @ 时 hasVersion=false）。
//
// 只拆最后一个 @：能力名正则里没有 @，所以位置唯一；版本不是整数时 version=0
// 交由调用方报错。
func splitRef(ref string) (name string, version int, hasVersion bool) {
	i := strings.LastIndexByte(ref, '@')
	if i < 0 {
		return ref, 0, false
	}
	v, err := strconv.Atoi(ref[i+1:])
	if err != nil {
		return ref[:i], 0, true
	}
	return ref[:i], v, true
}

// validateRequires 校验 requires（capability.md §2.3：各 ≤16、values 形状固定）。
func validateRequires(req *Requires) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, CodeRequiresInvalid+": "+fmt.Sprintf(format, args...))
	}
	if len(req.Commands) > MaxRequires {
		add("requires.commands 最多 %d 项，实际 %d", MaxRequires, len(req.Commands))
	}
	for _, cmd := range req.Commands {
		if cmd == "" {
			add("requires.commands 含空命令名")
			continue
		}
		if strings.Contains(cmd, `\`) {
			add("requires.commands 的命令名 %q 含反斜杠（只接受命令名，不接受路径）", cmd)
		}
		if strings.ContainsRune(cmd, 0) {
			add("requires.commands 的命令名 %q 含 NUL 字节", cmd)
		}
	}
	if len(req.Values) > MaxRequires {
		add("requires.values 最多 %d 项，实际 %d", MaxRequires, len(req.Values))
	}
	for _, value := range req.Values {
		name, ok := trimVarPrefix(value)
		if !ok || !varNamePattern.MatchString(name) {
			add("requires.values 的元素 %q 必须是 vars.<NAME>（NAME 匹配 %s）",
				value, varNamePattern.String())
		}
	}
	return p
}

// trimVarPrefix 把 `vars.<NAME>` 拆出 NAME（不是 vars. 前缀时 ok=false）。
func trimVarPrefix(fieldPath string) (string, bool) {
	const prefix = "vars."
	if !strings.HasPrefix(fieldPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(fieldPath, prefix), true
}

// validateRun 校验 run：files / entrypoint / env / workDir。
func validateRun(spec *Spec, params paramInfo) []string {
	run := &spec.Run
	var p []string
	add := func(code, format string, args ...any) {
		p = append(p, code+": "+fmt.Sprintf(format, args...))
	}

	// files：路径形状 + 限额。写入期**只校验字符串/限额**，不解析符号链接
	// （真实边界在执行前查，越界是运行期 files_escape）。
	paths := make([]string, 0, len(run.Files))
	for name := range run.Files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	if len(run.Files) == 0 {
		add(CodeFilesInvalid, "run.files 至少要有一个文件")
	}
	if len(run.Files) > MaxFiles {
		add(CodeFilesInvalid, "run.files 最多 %d 个文件，实际 %d", MaxFiles, len(run.Files))
	}
	var total int
	for _, name := range paths {
		body := run.Files[name]
		if msg := filePathProblem(name); msg != "" {
			add(CodeFilesInvalid, "%s", msg)
			continue
		}
		if len(body) > MaxFileBytes {
			add(CodeFilesInvalid, "文件 %q 有 %d 字节，单文件上限 %d", name, len(body), MaxFileBytes)
		}
		total += len(body)
	}
	if total > MaxTotalBytes {
		add(CodeFilesInvalid, "run.files 合计 %d 字节，上限 %d", total, MaxTotalBytes)
	}

	// entrypoint：必须在 files 里，且以 #! 开头。
	if run.Entrypoint == "" {
		add(CodeEntrypointNoShebang, "run.entrypoint 必填（必须是 files 里的一个 key）")
	} else if body, ok := run.Files[run.Entrypoint]; !ok {
		add(CodeEntrypointNoShebang, "run.entrypoint %q 不在 files 里", run.Entrypoint)
	} else if !strings.HasPrefix(body, "#!") {
		add(CodeEntrypointNoShebang, "入口文件 %q 必须以 #! 开头（执行方式是直接 exec，不经 shell）",
			run.Entrypoint)
	}

	p = append(p, validateEnv(run, &spec.Requires)...)
	p = append(p, validateWorkDir(run, params)...)
	return p
}

// filePathProblem 校验产物体内的相对路径，返回空串表示合法。
func filePathProblem(name string) string {
	if name == "" {
		return "文件路径不能为空"
	}
	if len(name) > MaxFilePathLen {
		return fmt.Sprintf("文件路径 %q 超过 %d 字符", name, MaxFilePathLen)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Sprintf("文件路径 %q 含 NUL 字节", name)
	}
	if strings.Contains(name, `\`) {
		return fmt.Sprintf("文件路径 %q 含反斜杠（只接受 / 分隔的 POSIX 相对路径）", name)
	}
	if path.IsAbs(name) {
		return fmt.Sprintf("文件路径 %q 必须是相对路径", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return fmt.Sprintf("文件路径 %q 含空路径段", name)
		}
		if seg == "." || seg == ".." {
			return fmt.Sprintf("文件路径 %q 含 %q 路径段", name, seg)
		}
	}
	return ""
}

// validateEnv 校验 run.env 的键与值。
func validateEnv(run *Run, req *Requires) []string {
	var p []string
	add := func(code, format string, args ...any) {
		p = append(p, code+": "+fmt.Sprintf(format, args...))
	}
	declared := make(map[string]struct{}, len(req.Values))
	for _, v := range req.Values {
		declared[v] = struct{}{}
	}

	keys := make([]string, 0, len(run.Env))
	for k := range run.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		value := run.Env[k]
		if !envKeyPattern.MatchString(k) {
			add(CodeEnvInvalid, "环境变量名 %q 不合法（只能包含字母、数字、下划线，且不能以数字开头）", k)
			continue
		}
		if strings.HasPrefix(k, "ANOTIFY_") {
			add(CodeEnvInvalid, "环境变量名 %q 属于契约变量（ANOTIFY_ 开头），产物不得覆盖", k)
			continue
		}
		if k == "PATH" {
			// PATH 取 anode 守护进程启动时的基线，探测与执行同一份；本轮不支持覆盖。
			add(CodeEnvInvalid, "不得在 run.env 里覆盖 PATH（任务 PATH = 节点启动基线，%s）", k)
			continue
		}
		if !value.IsValueFrom() {
			if k == "HOME" && !filepath.IsAbs(value.Value) {
				add(CodeEnvInvalid, "HOME 的字面量必须是绝对路径，实际 %q", value.Value)
			}
			continue
		}
		// valueFrom：fieldPath 必须是封闭集合里的名字；vars.* 必须写进 requires.values。
		if !isKnownFieldPath(value.FieldPath) {
			add(CodeUnknownFieldPath, "fieldPath %q 不在封闭集合里（%s / vars.<NAME>）",
				value.FieldPath, "metadata.* / status.*")
			continue
		}
		if _, isVar := trimVarPrefix(value.FieldPath); isVar {
			if _, ok := declared[value.FieldPath]; !ok {
				add(CodeUndeclaredNeed, "run.env 的 %s 引用了 %s，但没写进 spec.requires.values",
					k, value.FieldPath)
			}
		}
	}
	return p
}

// isKnownFieldPath 报告 fieldPath 是否属于 README.md 的封闭集合。
func isKnownFieldPath(fieldPath string) bool {
	if _, ok := builtinFieldPaths[fieldPath]; ok {
		return true
	}
	if name, ok := trimVarPrefix(fieldPath); ok {
		return varNamePattern.MatchString(name)
	}
	return false
}

// isDirFieldPath 报告一个 fieldPath 的值是否属于「目录类」（能当 workDir 的最终值）。
func isDirFieldPath(fieldPath string) bool {
	if _, isVar := trimVarPrefix(fieldPath); isVar {
		return true
	}
	dir, ok := builtinFieldPaths[fieldPath]
	return ok && dir
}

// validateWorkDir 校验 run.workDir 的模板形状。
//
// 只做**静态**校验：解析后的目录存不存在、是不是目录、软链是否越界只有节点知道
// （workdir_unavailable / files_escape 是执行层的事）。
func validateWorkDir(run *Run, params paramInfo) []string {
	dir := run.WorkDir
	var p []string
	addCode := func(code, format string, args ...any) {
		p = append(p, code+": "+fmt.Sprintf(format, args...))
	}
	add := func(format string, args ...any) {
		addCode(CodeWorkDirInvalid, format, args...)
	}
	if dir == "" {
		return nil
	}
	if len(dir) > MaxWorkDirLen {
		add("run.workDir 有 %d 字节，上限 %d", len(dir), MaxWorkDirLen)
		return p
	}
	if strings.ContainsRune(dir, 0) {
		add("run.workDir %q 含 NUL 字节", dir)
		return p
	}
	if strings.HasPrefix(dir, "~") {
		add("run.workDir 不支持 ~ 写法（家目录也是机器事实：用 fieldPath status.homeDir）")
		return p
	}

	matches := workDirRefPattern.FindAllStringSubmatchIndex(dir, -1)
	if len(matches) == 0 {
		if !strings.HasPrefix(dir, "/") {
			add("run.workDir 必须是绝对路径或 $(vars.X)/$(params.X) 模板，实际 %q"+
				"（留空 = 每次任务一个独立目录）", dir)
			return p
		}
		return append(p, workDirLiteralProblems(dir)...)
	}

	// 字面量前缀：模板以引用开头（引用值本身可能是绝对路径）或者以 / 开头。
	if matches[0][0] != 0 && !strings.HasPrefix(dir[:matches[0][0]], "/") {
		add("run.workDir 必须以绝对路径或 $(vars.X)/$(params.X) 开头，实际 %q", dir)
	}
	cursor := 0
	for _, m := range matches {
		literal := dir[cursor:m[0]]
		if strings.Contains(literal, "$") {
			add("run.workDir 只接受 $(vars.X) / $(params.X)，实际 %q", dir)
			return p
		}
		p = append(p, workDirLiteralProblems(literal)...)
		kind, name := dir[m[2]:m[3]], dir[m[4]:m[5]]
		switch kind {
		case "vars":
			value, ok := run.Env[name]
			if !ok {
				addCode(CodeUndefinedEnvRef, "run.workDir 引用了 $(vars.%s)，但 run.env 里没有声明 %s",
					name, name)
			} else if value.IsValueFrom() && !isDirFieldPath(value.FieldPath) {
				addCode(CodeNonPathField, "run.workDir 引用了 $(vars.%s)，但它指向 %s（标量，不是目录类事实）",
					name, value.FieldPath)
			}
		case "params":
			if _, ok := params.properties[name]; !ok {
				addCode(CodeUndefinedParamRef, "run.workDir 引用了 $(params.%s)，但 spec.params 里没有声明这个参数", name)
			}
		}
		cursor = m[1]
	}
	if tail := dir[cursor:]; strings.Contains(tail, "$") {
		add("run.workDir 只接受 $(vars.X) / $(params.X)，实际 %q", dir)
	}
	p = append(p, workDirLiteralProblems(dir[cursor:])...)
	return p
}

// workDirLiteralProblems 校验 workDir 里的一段字面量：不含 NUL 外的怪异段
// （空段 / "." / ".."），且不含未解析的 $。
func workDirLiteralProblems(literal string) []string {
	if literal == "" || literal == "/" {
		return nil
	}
	var p []string
	add := func(format string, args ...any) {
		p = append(p, CodeWorkDirInvalid+": "+fmt.Sprintf(format, args...))
	}
	if strings.Contains(literal, "//") {
		add("run.workDir %q 含连续斜杠（空路径段）", literal)
	}
	for _, seg := range strings.Split(literal, "/") {
		switch seg {
		case "..":
			add("run.workDir %q 含 \"..\" 路径段", literal)
		case ".":
			add("run.workDir %q 含 \".\" 路径段", literal)
		}
	}
	return p
}

// validateLifecycle 校验 lifecycle 的取值（默认值由 Normalize 补）。
func validateLifecycle(lc *Lifecycle) []string {
	var p []string
	add := func(format string, args ...any) {
		p = append(p, CodeBadRequest+": "+fmt.Sprintf(format, args...))
	}
	if lc.MaxAttempts != nil && *lc.MaxAttempts < 1 {
		add("lifecycle.maxAttempts 必须 ≥1（总尝试次数含第一次），实际 %d", *lc.MaxAttempts)
	}
	if lc.ResultTTLSec != nil && *lc.ResultTTLSec < 0 {
		add("lifecycle.resultTtlSec 必须 ≥0（0 = 永不清理），实际 %d", *lc.ResultTTLSec)
	}
	if lc.TimeoutSec < 0 || lc.TimeoutSec > MaxTimeoutSec {
		add("lifecycle.timeoutSec 必须在 0..%d 之间（0 = 不设本地执行超时），实际 %d",
			MaxTimeoutSec, lc.TimeoutSec)
	}
	return p
}

// validateConcurrency 校验 concurrency.maxPerNode。
func validateConcurrency(c *Concurrency) []string {
	if c.MaxPerNode != nil && *c.MaxPerNode < 1 {
		return []string{fmt.Sprintf("%s: concurrency.maxPerNode 必须 ≥1，实际 %d",
			CodeConcurrencyInvalid, *c.MaxPerNode)}
	}
	return nil
}

// digestDocument 是 digest 的 canonical 文档：{name, version, 内容字段}。
type digestDocument struct {
	Name        string          `json:"name"`
	Version     int             `json:"version"`
	Params      json.RawMessage `json:"params"`
	Requires    Requires        `json:"requires"`
	Lifecycle   Lifecycle       `json:"lifecycle"`
	Concurrency Concurrency     `json:"concurrency"`
	Run         Run             `json:"run"`
}

// foldDocument 是 foldKey 的 canonical 文档：{name, 内容字段}（**不含 version**）。
type foldDocument struct {
	Name        string          `json:"name"`
	Params      json.RawMessage `json:"params"`
	Requires    Requires        `json:"requires"`
	Lifecycle   Lifecycle       `json:"lifecycle"`
	Concurrency Concurrency     `json:"concurrency"`
	Run         Run             `json:"run"`
}

// canonicalMetadata 是参与 CanonicalJSON 的身份字段（name + version）。
type canonicalMetadata struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
}

// canonicalDoc 是 CanonicalJSON 的形状：整份产物，metadata 只取 name+version。
//
// 没有 displayName：它不进 digest，也不该出现在「规范字节」里。next 在——它不参与
// 身份，但它是路由的原始事实，落库必须存（capability.md §8.2）。
type canonicalDoc struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   canonicalMetadata `json:"metadata"`
	Spec       Spec              `json:"spec"`
}

// CanonicalJSON 返回产物的规范字节（**不含 displayName**）。
//
// 确定性来自三处：struct 字段顺序固定、map key 由 encoding/json 排序、
// params 先归一。因此同一份产物无论用 YAML 还是 JSON 写、无论 key 顺序如何，结果相同。
func CanonicalJSON(a *Artifact) ([]byte, error) {
	clone, err := Normalize(a)
	if err != nil {
		return nil, err
	}
	return json.Marshal(&canonicalDoc{
		APIVersion: clone.APIVersion,
		Kind:       clone.Kind,
		Metadata:   canonicalMetadata{Name: clone.Metadata.Name, Version: clone.Metadata.Version},
		Spec:       clone.Spec,
	})
}

// FoldKey 返回折叠键（形如 "sha256:<64 hex>"），用于判「是不是同一行」。
//
// 不含 version / next / displayName：省略 version 时先按它复用已有行、没有才分配
// 下一个 version；「只改 next」因此不会长出新版本。
func FoldKey(a *Artifact) (string, error) {
	clone, err := Normalize(a)
	if err != nil {
		return "", err
	}
	return hashJSON(&foldDocument{
		Name:        clone.Metadata.Name,
		Params:      clone.Spec.Params,
		Requires:    clone.Spec.Requires,
		Lifecycle:   clone.Spec.Lifecycle,
		Concurrency: clone.Spec.Concurrency,
		Run:         clone.Spec.Run,
	})
}

// Digest 返回部署身份（形如 "sha256:<64 hex>"），覆盖 {name, version, 内容字段}。
//
// 调用前 version 必须已确定（显式给或由 store 分配）：version=0 是「还没分配」，
// 这时候算 digest 会把「未分配」当成一个版本号混进身份里。
func Digest(a *Artifact) (string, error) {
	clone, err := Normalize(a)
	if err != nil {
		return "", err
	}
	if clone.Metadata.Version < 1 {
		return "", &Error{Problems: []string{fmt.Sprintf(
			"%s: 算 digest 之前必须先确定 version（省略 version = 内容寻址，由 store 先分配），实际 %d",
			CodeBadRequest, clone.Metadata.Version)}}
	}
	return hashJSON(&digestDocument{
		Name:        clone.Metadata.Name,
		Version:     clone.Metadata.Version,
		Params:      clone.Spec.Params,
		Requires:    clone.Spec.Requires,
		Lifecycle:   clone.Spec.Lifecycle,
		Concurrency: clone.Spec.Concurrency,
		Run:         clone.Spec.Run,
	})
}

// hashJSON 是两级身份共用的哈希入口。
func hashJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("artifact: 序列化内容身份: %w", err)
	}
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// SameFold 报告两份产物是否会折叠到同一行（foldKey 相同）。
//
// 它是「省略 version 时是否复用已有版本」的判据。
func SameFold(a, b *Artifact) (bool, error) {
	ka, err := FoldKey(a)
	if err != nil {
		return false, err
	}
	kb, err := FoldKey(b)
	if err != nil {
		return false, err
	}
	return ka == kb, nil
}

// SameContent 报告两份产物的部署身份是否相同（digest 相同）。
//
// 它是「显式同版内容不可变」的判据：同一 (namespace, name, version) 的两份产物
// SameContent=false 时必须换 version（否则 400 version_content_immutable）；
// next / displayName 是既定例外——它们不进 digest，SameContent 仍为 true。
func SameContent(a, b *Artifact) (bool, error) {
	da, err := Digest(a)
	if err != nil {
		return false, err
	}
	db, err := Digest(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// MarshalYAML 把产物序列化成落盘用的 YAML。
//
// 先转 JSON 再转 YAML：保证「落盘 → 重新 Parse」得到完全相同的 digest。
// 与 CanonicalJSON 的差别只有一个：**保留 displayName**（它不进 digest，但不是
// 不存在——节点侧人读产物文件时应该看得见作者取的名字）。
func MarshalYAML(a *Artifact) ([]byte, error) {
	clone, err := Normalize(a)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(clone)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return yaml.Marshal(v)
}

// Decode 一步完成 Parse + digest，是节点侧与应用侧最常用的入口。
func Decode(data []byte) (*Artifact, string, error) {
	a, err := Parse(data)
	if err != nil {
		return nil, "", err
	}
	d, err := Digest(a)
	if err != nil {
		return nil, "", err
	}
	return a, d, nil
}

// compactJSON 把一段原始 JSON 压成一行，用于错误信息（不改变语义）。
func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
