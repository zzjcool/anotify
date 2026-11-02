// Package artifact 定义「能力产物」（Capability Artifact）的线上格式、校验与
// 内容寻址标识。
//
// 一个产物是一份**自包含**的文件：参数契约 + 入口文件 + 全部文件内容 + 超时 +
// 环境变量 + 依赖的外部命令。它是控制台表单的**编译产物**，不是人手写的源文件；
// 用户可以改，但改过之后 digest 变了，中枢会把它识别为「本地变体」而永不覆盖。
//
// # 为什么自包含
//
// 「拷到另一台机器、直接跑起来」是这套模型的信任基础：产物不引用任何外部 blob
// 或 URL，脱开中枢也能执行（cmd/agent 的 standalone 模式）。
//
// # 执行边界
//
// 产物只在**节点**上执行，中枢既不解释也不运行其中的内容（契约 I1）。
// 入口文件必须带 shebang：执行方式是直接 exec，不经 shell —— 没有 shell 就没有
// 插值、拼接与引号问题。
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
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

	// MaxFileBytes 是单个文件的体积上限。
	MaxFileBytes = 256 << 10
	// MaxTotalBytes 是一份产物全部文件的合计上限。
	MaxTotalBytes = 1 << 20
	// MaxOutputs 是一份产物最多能声明的出口名个数。
	MaxOutputs = 32
	// MaxRequires 是一份产物最多能声明的外部命令个数。
	MaxRequires = 16
	// MaxTimeoutSec 是超时上限（24 小时）。
	MaxTimeoutSec = 86400
	// MaxFilePathLen 是产物内相对路径的长度上限。
	MaxFilePathLen = 255
	// MaxNameLen 是能力名长度上限（与节点名同口径）。
	MaxNameLen = 63
	// MaxWorkDirLen 是 run.workDir 的长度上限。
	//
	// PATH_MAX 在 Linux 上是 4096，但那是**含相对路径解析后**的中转缓冲；
	// 一份能力的人读声明写到 1024 字符已远超任何真实项目目录，再长基本是
	// 复制粘贴事故，早报错比让节点在 chdir 时报 ENAMETOOLONG 好查。
	MaxWorkDirLen = 1024
)

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	outputPattern  = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,32}$`)
	requirePattern = regexp.MustCompile(`^[a-zA-Z0-9._+-]{1,64}$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Artifact 是一份能力产物。字段声明顺序即 canonical JSON 的字段顺序——
// 顺序参与 digest 计算，改动字段顺序会让所有已有产物的 digest 失效。
type Artifact struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

// Metadata 是产物的身份。
type Metadata struct {
	Name    string `json:"name"    yaml:"name"`
	Version int    `json:"version" yaml:"version"`
}

// Spec 是产物的全部内容。
type Spec struct {
	DisplayName string          `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"      yaml:"params,omitempty"`
	Outputs     []string        `json:"outputs"               yaml:"outputs"`
	Requires    []string        `json:"requires,omitempty"    yaml:"requires,omitempty"`
	Run         Run             `json:"run"                   yaml:"run"`
}

// Run 是「怎么跑」。
type Run struct {
	// Entrypoint 必须是 Files 里的一个 key，且内容以 "#!" 开头。
	Entrypoint string `json:"entrypoint" yaml:"entrypoint"`
	// Files 是相对路径 → 文件内容。全部按相对路径展开进工作目录。
	Files map[string]string `json:"files" yaml:"files"`
	// TimeoutSec 为 0 表示不设本地超时（交给中枢租约兜底）。
	TimeoutSec int `json:"timeoutSec,omitempty" yaml:"timeoutSec,omitempty"`
	// Env 是产物自带的环境变量；ANOTIFY_ 前缀保留给契约变量。
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	// WorkDir 是能力声明的执行目录（绝对路径）。空 = 每次任务一个独立目录。
	//
	// 为什么放在产物里而不是节点配置里："这个能力要在哪个目录里跑" 是能力的
	// 一部分（跟入口文件、超时一样决定它怎么跑），定义它的地方只有一个——控制台
	// 的能力定义。节点侧再留一份白名单/映射，就会出现两份会分叉的事实，而分叉的
	// 症状是 "控制台改了却没生效"，极难查。
	//
	// 字段顺序参与 digest，所以只能加在最后（插中间会让所有已有产物的 digest 失效）。
	WorkDir string `json:"workDir,omitempty" yaml:"workDir,omitempty"`
}

// Error 汇总一份产物上的全部问题。
//
// 一次报全部、不只报第一条：手写 YAML 时用户要的是「哪里全错了」，
// 而不是修一条、再跑一次、再发现下一条。
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return strings.Join(e.Problems, "; ")
}

func (e *Error) add(format string, args ...any) {
	e.Problems = append(e.Problems, fmt.Sprintf(format, args...))
}

// Err 如果 problems 非空就返回一个 *Error，否则返回 nil。
func errIf(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return &Error{Problems: problems}
}

// Code 返回第一个问题的机器可读短码（用于 apply 帧的 reason）。
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) && len(e.Problems) > 0 {
		p := e.Problems[0]
		if i := strings.IndexByte(p, ':'); i > 0 {
			return p[:i]
		}
		return p
	}
	return "invalid_artifact"
}

// Parse 从 YAML 或 JSON 解析一份产物并做完整校验。
//
// YAML 是 JSON 的超集，所以单条路径同时支持两种写法：先解成 map，再转 JSON，
// 最后严格解码进结构体（未知字段是错误——拼错的字段名静默失效比解析失败更难查）。
func Parse(data []byte) (*Artifact, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, &Error{Problems: []string{"invalid_yaml: " + err.Error()}}
	}
	if raw == nil {
		return nil, &Error{Problems: []string{"invalid_yaml: 文档为空"}}
	}
	norm, err := json.Marshal(normalizeMaps(raw))
	if err != nil {
		return nil, &Error{Problems: []string{"invalid_yaml: " + err.Error()}}
	}

	var a Artifact
	dec := json.NewDecoder(strings.NewReader(string(norm)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, &Error{Problems: []string{"unknown_field: " + err.Error()}}
	}

	// Params 归一成 canonical JSON：digest 必须与「输入怎么排版」无关。
	if len(a.Spec.Params) > 0 {
		canon, cerr := canonicalizeJSON(a.Spec.Params)
		if cerr != nil {
			return nil, &Error{Problems: []string{"invalid_params_schema: " + cerr.Error()}}
		}
		a.Spec.Params = canon
	}

	if err := Validate(&a); err != nil {
		return nil, err
	}
	return &a, nil
}

// normalizeMaps 把 yaml.v3 可能产出的 map[any]any 递归换成 map[string]any，
// 否则 json.Marshal 会直接失败。
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

// Validate 校验一份产物，返回汇总了**全部**问题的 *Error（或 nil）。
func Validate(a *Artifact) error {
	if a == nil {
		return &Error{Problems: []string{"nil_artifact: 产物为空"}}
	}
	var p []string

	if a.APIVersion != APIVersion {
		p = append(p, fmt.Sprintf("unsupported_api_version: 期望 %q，实际 %q", APIVersion, a.APIVersion))
	}
	if a.Kind != KindCapability {
		p = append(p, fmt.Sprintf("unsupported_kind: 期望 %q，实际 %q", KindCapability, a.Kind))
	}
	if !namePattern.MatchString(a.Metadata.Name) {
		p = append(p, fmt.Sprintf("invalid_name: 「%s」只能包含小写字母、数字、点、下划线、短横线，"+
			"长度 1..%d，且必须以字母或数字开头", a.Metadata.Name, MaxNameLen))
	}
	if a.Metadata.Version < 1 {
		p = append(p, fmt.Sprintf("invalid_version: 版本必须 >= 1，实际 %d", a.Metadata.Version))
	}

	p = append(p, validateRun(&a.Spec.Run)...)
	p = append(p, validateOutputs(a.Spec.Outputs)...)
	p = append(p, validateRequires(a.Spec.Requires)...)
	p = append(p, validateParams(a.Spec.Params)...)

	return errIf(p)
}

func validateRun(run *Run) []string {
	var p []string

	if len(run.Files) == 0 {
		p = append(p, "invalid_file_path: files 至少要有一个文件")
	}

	names := make([]string, 0, len(run.Files))
	for name := range run.Files {
		names = append(names, name)
	}
	sort.Strings(names)

	var total int
	for _, name := range names {
		body := run.Files[name]
		if msg := validateFilePath(name); msg != "" {
			p = append(p, "invalid_file_path: "+msg)
			continue
		}
		if len(body) > MaxFileBytes {
			p = append(p, fmt.Sprintf("payload_too_large: 文件 %q 有 %d 字节，单文件上限 %d",
				name, len(body), MaxFileBytes))
		}
		total += len(body)
	}
	if total > MaxTotalBytes {
		p = append(p, fmt.Sprintf("payload_too_large: 全部文件合计 %d 字节，上限 %d", total, MaxTotalBytes))
	}

	if run.Entrypoint == "" {
		p = append(p, "entrypoint_missing: run.entrypoint 必填")
	} else if body, ok := run.Files[run.Entrypoint]; !ok {
		p = append(p, fmt.Sprintf("entrypoint_missing: run.entrypoint %q 不在 files 里", run.Entrypoint))
	} else if !strings.HasPrefix(body, "#!") {
		p = append(p, fmt.Sprintf("entrypoint_no_shebang: 入口文件 %q 必须以 #! 开头"+
			"（执行方式是直接 exec，不经 shell）", run.Entrypoint))
	}

	if run.TimeoutSec < 0 || run.TimeoutSec > MaxTimeoutSec {
		p = append(p, fmt.Sprintf("invalid_timeout: timeoutSec 必须在 0..%d 之间，实际 %d",
			MaxTimeoutSec, run.TimeoutSec))
	}

	// workDir 在这里只做**静态**校验（能不能表达成一个安全的绝对路径）；
	// 目录存不存在、是不是目录只有节点知道，那是执行层的失败（见 cmd/agent）。
	if msg := validateWorkDir(run.WorkDir); msg != "" {
		p = append(p, msg)
	}

	envKeys := make([]string, 0, len(run.Env))
	for k := range run.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		if !envKeyPattern.MatchString(k) {
			p = append(p, fmt.Sprintf("reserved_env: 环境变量名 %q 不合法"+
				"（只能包含字母、数字、下划线，且不能以数字开头）", k))
			continue
		}
		if strings.HasPrefix(strings.ToUpper(k), "ANOTIFY_") {
			p = append(p, fmt.Sprintf("reserved_env: 环境变量名 %q 属于契约变量，产物不得覆盖", k))
		}
	}
	return p
}

// validateFilePath 校验产物体内的相对路径，返回空串表示合法。
func validateFilePath(name string) string {
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

// validateWorkDir 校验 run.workDir，返回 "<错误码>: <说明>"，空串表示合法。
//
// 空值是合法的（= 每次任务一个独立目录），除此之外要求一个**绝对的、没有
// `..` 段的** POSIX 路径：
//
//   - 绝对路径：节点会直接拿它当 cwd，相对路径的含义取决于 agent 进程的 cwd，
//     那是运维看不见的上下文，等于让能力跑在一个随机目录里；
//   - 不含 `..`：声明里出现上跳段几乎都是拼接事故；把它挡在定义期，节点就不必
//     去猜用户到底想去哪儿（也避免了"声明 /a/../etc 却能绕过审计"这类怪事）；
//   - 不含 NUL：NUL 会被 exec/chdir 的底层 syscall 截断，是典型的路径伪造手法。
func validateWorkDir(dir string) string {
	if dir == "" {
		return ""
	}
	if len(dir) > MaxWorkDirLen {
		return fmt.Sprintf("invalid_work_dir: run.workDir 有 %d 字节，上限 %d", len(dir), MaxWorkDirLen)
	}
	if strings.ContainsRune(dir, 0) {
		return fmt.Sprintf("invalid_work_dir: run.workDir %q 含 NUL 字节", dir)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Sprintf("work_dir_not_absolute: run.workDir 必须是绝对路径，实际 %q"+
			"（留空 = 每次任务一个独立目录）", dir)
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return fmt.Sprintf("invalid_work_dir: run.workDir %q 含 \"..\" 路径段", dir)
		}
	}
	return ""
}

func validateOutputs(outputs []string) []string {
	var p []string
	if len(outputs) > MaxOutputs {
		p = append(p, fmt.Sprintf("invalid_output: 出口名最多 %d 个，实际 %d", MaxOutputs, len(outputs)))
	}
	seen := make(map[string]struct{}, len(outputs))
	dups := make([]string, 0)
	for _, o := range outputs {
		if !outputPattern.MatchString(o) {
			p = append(p, fmt.Sprintf("invalid_output: 出口名 %q 不合法"+
				"（只能包含字母、数字、点、下划线、短横线，长度 1..32）", o))
			continue
		}
		if _, ok := seen[o]; ok {
			dups = append(dups, o)
			continue
		}
		seen[o] = struct{}{}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		p = append(p, fmt.Sprintf("invalid_output: 出口名重复：%s", strings.Join(dups, ", ")))
	}
	return p
}

func validateRequires(requires []string) []string {
	var p []string
	if len(requires) > MaxRequires {
		p = append(p, fmt.Sprintf("invalid_requirement: requires 最多 %d 项，实际 %d",
			MaxRequires, len(requires)))
	}
	seen := make(map[string]struct{}, len(requires))
	for _, r := range requires {
		if !requirePattern.MatchString(r) {
			p = append(p, fmt.Sprintf("invalid_requirement: 依赖命令 %q 不合法"+
				"（只能包含字母、数字、点、下划线、加号、短横线，长度 1..64）", r))
			continue
		}
		seen[r] = struct{}{}
	}
	return p
}

func validateParams(params json.RawMessage) []string {
	if len(params) == 0 {
		return nil
	}
	var probe any
	if err := json.Unmarshal(params, &probe); err != nil {
		return []string{"invalid_params_schema: " + err.Error()}
	}
	if _, ok := probe.(map[string]any); !ok {
		return []string{"invalid_params_schema: params 必须是 JSON 对象"}
	}
	return nil
}

// CanonicalJSON 返回参与 digest 计算的规范字节。
//
// 确定性来自两处：struct 字段顺序固定、map key 由 encoding/json 排序。
// 因此同一份产物无论用 YAML 还是 JSON 写、无论 key 顺序如何，digest 相同。
func CanonicalJSON(a *Artifact) ([]byte, error) {
	if a == nil {
		return nil, errors.New("artifact: nil")
	}
	// 拷贝一份，保证 Outputs 为空时序列化成 [] 而不是 null（AGENTS.md 的空列表纪律），
	// 同时不修改调用方的对象。
	clone := *a
	if clone.Spec.Outputs == nil {
		clone.Spec.Outputs = []string{}
	}
	// params 也要归一（key 有序、无多余空白）——Parse 里已经这么做了，这里必须一致。
	//
	// 不同步的后果是**内容寻址整个失效**：中枢拿请求里的原始字节算 digest，
	// 存下来的是归一后的 spec；节点（或任何第三方）从存下来的 spec 重算，
	// 得到的是另一个 digest → 部署永远被判成 local（"节点上是本地变体"），
	// 而两边的产物内容其实一模一样。
	//
	// 症状极隐蔽：digest 看起来都合法（都是 sha256:…），只有把两个值并排
	// 看才会发现它们不同。所以这里同时补了 round-trip 单测。
	if len(clone.Spec.Params) > 0 {
		canon, err := canonicalizeJSON(clone.Spec.Params)
		if err != nil {
			return nil, fmt.Errorf("artifact: spec.params 不是合法 JSON：%w", err)
		}
		clone.Spec.Params = canon
	}
	return json.Marshal(&clone)
}

// Digest 返回形如 "sha256:<64 hex>" 的内容寻址标识。
func Digest(a *Artifact) (string, error) {
	b, err := CanonicalJSON(a)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// MarshalYAML 把产物序列化成落盘用的 YAML。
//
// 先转 canonical JSON 再转 YAML：保证「落盘 → 重新 Parse」得到完全相同的 digest。
func MarshalYAML(a *Artifact) ([]byte, error) {
	b, err := CanonicalJSON(a)
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
