// Package artifact 定义「能力产物」（Capability Artifact）的线上格式、校验与
// 内容寻址标识。
//
// 一份产物是一份**自包含**的规格：参数契约 + 出口 + 生命周期/粘性 + 实现
// （脚本文件，或一个白名单内置执行器）+ 超时 + 环境变量 + 依赖的外部命令。
// 它是控制台表单的**编译产物**，不是人手写的源文件；用户可以改，但改过之后 digest
// 变了，中枢会把它识别为「本地变体」而永不覆盖。
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
//
// # 两种实现形式（Capability Spec v3 §3.1）
//
// 规格里既有契约也有实现，实现只有两种形式，**恰好给一组**：
//
//	files + entrypoint   普通脚本，由任意 agent 节点执行（99% 的能力）
//	handler              内置执行器（白名单，编译进中枢），只有 builtin 节点能认领
//
// 所以「能力有没有实现」是 Validate 的一条硬规则（impl_missing / impl_ambiguous），
// 而不是一个可以事后补的状态：占位能力用 handler: noop。
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

// 内置执行器白名单（Capability Spec v3 §3.1）。
//
// handler 是「编译进中枢进程里的执行器」，不是节点上的一段脚本：它由中枢的
// builtin 节点认领（契约 R1/R2）。白名单必须**只有**这里列出的几个——「猜一个
// 名字试试」必须在校验期失败，而不是等到派发时才发现没有执行器（那时任务已经
// 建好、已经扣了租约，失败现场比一个 YAML 错误难查得多）。
const (
	// HandlerNotifyPhone 是平台自带的手机推送动作（曾经的 notify.phone@1）。
	HandlerNotifyPhone = "notify-phone"
	// HandlerNoop 是合法、可部署、无副作用、不发 emit 的占位执行器。
	//
	// 它的存在是契约 R2 的落地方式：**能力永远有实现**，而占位不许伪造用户脚本
	// （不许给空壳编造一份 `exit 0` 的正文）——用平台认领的 noop 明确表达「还没实现」。
	HandlerNoop = "noop"
)

// knownHandlers 是白名单本体（顺序稳定：错误信息与播种输出要可 diff）。
var knownHandlers = []string{HandlerNotifyPhone, HandlerNoop}

// KnownHandlers 返回内置执行器白名单（拷贝，调用方可安全修改）。
//
// 导出的理由：白名单是**中枢侧**概念，播种（P1-b 把零产物的老能力补成
// handler: noop）与端点校验都要用它，各写一份常量必然某天分叉。
func KnownHandlers() []string {
	out := make([]string, len(knownHandlers))
	copy(out, knownHandlers)
	return out
}

// isKnownHandler 报告 h 是否在白名单里。
func isKnownHandler(h string) bool {
	for _, known := range knownHandlers {
		if h == known {
			return true
		}
	}
	return false
}

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	outputPattern  = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,32}$`)
	requirePattern = regexp.MustCompile(`^[a-zA-Z0-9._+-]{1,64}$`)
	envKeyPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Artifact 是一份能力产物。
//
// 字段声明顺序即 canonical JSON 的字段顺序，而顺序参与 digest —— 改动字段顺序会让
// 所有已有产物的 digest 失效（v3 接受并只接受这一次性代价，见契约 §3.2）。
// 注意「在这份文档里」与「在 digest 里」不是同一件事：apiVersion / kind 与
// metadata.displayName 留在文档中（落盘、落库要用），但不进 digest（见 digestDocument）。
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
	// DisplayName 是**纯展示**的名字（控制台列表、节点标题）。
	//
	// v3 把它从 Spec 搬到这里，理由只有一条：它不参与 digest（契约 §3.2）。
	// 留在 Spec 里时，改一次显示名就会让 digest 变化 —— 而 PATCH /v1/capabilities
	// 只改这一列、不重新编译产物，于是「列里的名字」与「产物里的名字」永久漂移，
	// 部署还一层层判出 stale/local。搬出来以后它天然不漂：名字不参与内容寻址。
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`
}

// Spec 是产物的全部内容。
//
// 字段声明顺序即 canonical JSON 的字段顺序（顺序参与 digest，见契约 §3）：
// params / outputs / requires / lifecycle / sticky，然后是 run（脚本侧的
// entrypoint/files/timeoutSec/env/workDir 仍收在 Run 下，不拍平），最后 handler。
type Spec struct {
	Params  json.RawMessage `json:"params,omitempty"    yaml:"params,omitempty"`
	Outputs []string        `json:"outputs"             yaml:"outputs"`
	// Requires 是节点必须预装的外部命令（部署时 LookPath 探测）。
	Requires []string `json:"requires,omitempty"  yaml:"requires,omitempty"`
	// Lifecycle 是重试/幂等/结果保留策略，v3 起进规格（改它 = 改内容 = 新版本）。
	//
	// 类型是 json.RawMessage（**不透明透传**）而不是严格结构体：internal/store 已经
	// import 本包（builtin.go），本包反向 import store 会成环（契约 §4）。所以本包只负
	// 责「带着它进 digest」，内容校验（retryable / maxAttempts / TTL 范围）留在 store 层。
	Lifecycle json.RawMessage `json:"lifecycle,omitempty" yaml:"lifecycle,omitempty"`
	// Sticky 是粘性路由键的类型（"" | worktree | …）。
	Sticky string `json:"sticky,omitempty"    yaml:"sticky,omitempty"`
	// Run 是脚本形式的「怎么跑」；handler 形式下它必须是零值（见 validateImpl）。
	Run Run `json:"run"                 yaml:"run"`
	// Handler 是内置执行器的名字（白名单见 KnownHandlers）；与 Run 恰好给一组。
	Handler string `json:"handler,omitempty"   yaml:"handler,omitempty"`
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
	// WorkDir 是能力声明的执行目录：绝对路径，或 `~` / `~/…`（空 = 每次任务一个
	// 独立目录）。`~` 由**节点**在执行时展开成 agent 用户的家目录（见 ExpandWorkDir）
	// ——产物里存的永远是字面量，所以它参与 digest 时与机器无关。
	//
	// 为什么放在产物里而不是节点配置里："这个能力要在哪个目录里跑" 是能力的
	// 一部分（跟入口文件、超时一样决定它怎么跑），定义它的地方只有一个——控制台
	// 的能力定义。节点侧再留一份白名单/映射，就会出现两份会分叉的事实，而分叉的
	// 症状是 "控制台改了却没生效"，极难查。
	//
	// 字段顺序参与 digest，所以只能加在最后（插中间会让所有已有产物的 digest 失效）。
	WorkDir string `json:"workDir,omitempty" yaml:"workDir,omitempty"`
}

// isZero 报告 run 是否**完全没有内容**（= handler 形式下唯一合法的状态）。
func (r Run) isZero() bool {
	return r.Entrypoint == "" && len(r.Files) == 0 && r.TimeoutSec == 0 &&
		len(r.Env) == 0 && r.WorkDir == ""
}

// MarshalJSON 在 **handler 形式**下省略空的 run 块。
//
// 为什么需要它：Run 是（非指针）struct，encoding/json 不会替它 omitempty，于是
// handler 形式的规格会序列化出 `"run":{"entrypoint":"","files":null}`。那有两个毛病：
//
//   - `files: null` 违反空列表纪律（AGENTS.md：空列表给 []，别给 null）；
//   - 它正好撞上「handler 形式不得带 files / entrypoint」这条规则（见 validateImpl）——
//     Validate 只是放过了**零值**，序列化却把这两个键写了出来。于是任何拿本包的规范
//     字节去过一遍 `anotify-agent schema capability` 的工具都会看到「一份合法产物被
//     判非法」，而 artifact 与 schema 两份真理的守卫又抓不到（它们各自都和 Validate
//     对齐，只是 canonical 输出这一侧没人验）。
//
// 只在「handler 非空且 Run 是零值」时省略：脚本形式的字节一个都不动（type plain
// 只丢掉方法、保留字段顺序与 tag，所以除这一支以外输出与默认序列化逐字节相同）。
func (s Spec) MarshalJSON() ([]byte, error) {
	if s.Handler != "" && s.Run.isZero() {
		return json.Marshal(struct {
			Params    json.RawMessage `json:"params,omitempty"`
			Outputs   []string        `json:"outputs"`
			Requires  []string        `json:"requires,omitempty"`
			Lifecycle json.RawMessage `json:"lifecycle,omitempty"`
			Sticky    string          `json:"sticky,omitempty"`
			Handler   string          `json:"handler,omitempty"`
		}{s.Params, s.Outputs, s.Requires, s.Lifecycle, s.Sticky, s.Handler})
	}
	type plain Spec // 别名去掉方法，避免 MarshalJSON 递归调用自己
	return json.Marshal(plain(s))
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
		return ProblemCode(e.Problems[0])
	}
	return "invalid_artifact"
}

// ProblemCode 取**单条**问题文本的机器可读短码（Code 的单条版本）。
//
// 一条问题写成 "<短码>: <人话>"（见 Error.add 的各调用点），短码是第一个 ':' 之前
// 那一段。抽成导出函数是因为 `anotify-agent check --json` 要逐条给出 code（issue #319）：
// 如果那边自己再切一次字符串，某天改了分隔符就会变成「CLI 的 code 与 Code() 的词表
// 悄悄不是同一套」——而两处都看不出自己错在哪。
func ProblemCode(problem string) string {
	if i := strings.IndexByte(problem, ':'); i > 0 {
		return problem[:i]
	}
	return problem
}

// ProblemMessage 取单条问题文本的**人话部分**（去掉短码前缀与它后面的空白）。
//
// 短码给机器 switch，人话给人（也进 check --json 的 message）：两段来自同一句话，
// 不会出现「码说 A、话在说 B」。
func ProblemMessage(problem string) string {
	if i := strings.IndexByte(problem, ':'); i > 0 {
		return strings.TrimSpace(problem[i+1:])
	}
	return problem
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
	// lifecycle 同理（它也是 RawMessage 透传，不归一就会出现「两种书写顺序 = 两份产物」）。
	// 这个错误码只有程序自己写坏一个 RawMessage 时才可能命中——从 Parse 进来的一定是
	// 一次成功的 JSON 解码，必然是合法 JSON；口径与 invalid_params_schema 同构。
	if len(a.Spec.Lifecycle) > 0 {
		canon, cerr := canonicalizeJSON(a.Spec.Lifecycle)
		if cerr != nil {
			return nil, &Error{Problems: []string{"invalid_lifecycle: " + cerr.Error()}}
		}
		a.Spec.Lifecycle = canon
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

	p = append(p, validateImpl(&a.Spec)...)
	p = append(p, validateOutputs(a.Spec.Outputs)...)
	p = append(p, validateRequires(a.Spec.Requires)...)
	p = append(p, validateParams(a.Spec.Params)...)

	return errIf(p)
}

// validateImpl 校验「实现形式」：`files`+`entrypoint` 与 `handler` **恰好给一组**
// （契约 §3.1 的三条规则），返回这两支该报的全部问题。
//
// 两支是互斥的，所以各自验各自的：脚本形式跑 validateRun（入口/文件/超时/env/workDir
// 的原有规则一条不动，错误码也不动）；handler 形式反过来——那些字段**一个都不许出现**。
// 两者都跑会制造噪音：一份 handler 规格里的 timeoutSec 并不是“超时值不合法”，而是
// “这个字段在这支里没有意义”，两者得分开说。
func validateImpl(spec *Spec) []string {
	// 这里**不 TrimSpace**：白名单比较必须与运行时查执行器用的是同一个字符串。
	// 容忍边空格的结果是「校验通过、派发时找不到执行器」（那时任务已经建好、租约
	// 已经扣了），而 `handler: " noop "` 几乎只能是引号写法事故。
	handler := spec.Handler
	run := &spec.Run

	if handler != "" {
		var p []string
		if !isKnownHandler(handler) {
			p = append(p, fmt.Sprintf("unknown_handler: 内置执行器 %q 不在白名单里（可用：%s）",
				handler, strings.Join(KnownHandlers(), "、")))
		}
		// handler 形式不得再带 run 侧字段：带了就是「两种实现混在一份规格里」，
		// 中枢无法判定到底谁该跑（最坏的情形是内置节点抢了作者的脚本）。
		if len(run.Files) > 0 {
			p = append(p, fmt.Sprintf("impl_ambiguous: handler 形式不得再带 files（%d 个文件）："+
				"files+entrypoint 与 handler 只能给一组", len(run.Files)))
		}
		if run.Entrypoint != "" {
			p = append(p, fmt.Sprintf("impl_ambiguous: handler 形式不得再带 entrypoint（%q）："+
				"files+entrypoint 与 handler 只能给一组", run.Entrypoint))
		}
		if run.TimeoutSec != 0 {
			p = append(p, fmt.Sprintf("impl_ambiguous: handler 形式不得带 timeoutSec（%d）："+
				"超时由内置执行器自己定", run.TimeoutSec))
		}
		if run.WorkDir != "" {
			p = append(p, fmt.Sprintf("impl_ambiguous: handler 形式不得带 workDir（%q）："+
				"工作目录由中枢进程自己定", run.WorkDir))
		}
		if len(run.Env) > 0 {
			p = append(p, fmt.Sprintf("impl_ambiguous: handler 形式不得带 env（%d 个键）："+
				"内置执行器不读能力自带的环境变量", len(run.Env)))
		}
		return p
	}

	// 没有 handler → 必须是脚本形式；两边都不给等于没有实现（契约 R2）。
	if len(run.Files) == 0 && run.Entrypoint == "" {
		return []string{"impl_missing: 缺实现：files+entrypoint（脚本，节点执行）" +
			"或 handler（内置执行器，中枢 builtin 节点执行），两者恰好给一组"}
	}
	return validateRun(run)
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

	// workDir 在这里只做**静态**校验（能不能表达成一个安全的目录声明，含 `~` 前缀）；
	// 目录存不存在、是不是目录、`~` 落在哪只有节点知道，那是执行层的失败（见 cmd/agent）。
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
// 空值是合法的（= 每次任务一个独立目录），除此之外只接受两种写法：
//
//   - 绝对路径（以 `/` 开头）：节点直接拿它当 cwd；
//   - `~` 或 `~/…`：由**节点**在执行时展开成 agent 用户的家目录（见 ExpandWorkDir）。
//
// 其余仍不接受，三条理由不变：
//
//   - 相对路径的含义取决于 agent 进程的 cwd，那是运维看不见的上下文，等于让能力
//     跑在一个随机目录里；`~user/x`、`$HOME/x` 同样不行——它们要么取决于机器上
//     有没有这个用户，要么取决于进程环境（见 ExpandWorkDir 的注释）；
//   - 不含 `..`：声明里出现上跳段几乎都是拼接事故；把它挡在定义期，节点就不必
//     去猜用户到底想去哪儿（也避免了"声明 /a/../etc 却能绕过审计"这类怪事）。
//     `..` 的检查必须对**字面量**做，且必须在展开**之前**：`~/../etc` 一旦先展开
//     就变成了"家目录的父目录"，那时再查已经晚了；
//   - 不含 NUL：NUL 会被 exec/chdir 的底层 syscall 截断，是典型的路径伪造手法。
//
// 这里**只校验、不展开**：run.workDir 参与 digest，展开后的路径是机器事实，
// 混进 digest 会让同一份产物在不同节点上算出不同摘要（节点会把它判成"本地变体"）。
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
	if !IsHomeRelative(dir) && !filepath.IsAbs(dir) {
		return fmt.Sprintf("work_dir_not_absolute: run.workDir 必须是绝对路径，或以 ~/ 开头"+
			"（~ 由节点展开为 agent 用户的家目录），实际 %q（留空 = 每次任务一个独立目录）", dir)
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return fmt.Sprintf("invalid_work_dir: run.workDir %q 含 \"..\" 路径段", dir)
		}
	}
	return ""
}

// IsHomeRelative 报告 run.workDir 是否以家目录前缀起头（`~` 本身或 `~/…`）。
//
// 只认这两种写法：`~user/x` 取决于机器上有没有那个用户，`$HOME/x` / 任意 `$VAR`
// 取决于进程环境——它们都会让同一份能力在不同节点、不同启动方式下跑进不同目录，
// 那就不是声明式常量而是求值表达式了（理由详见 ExpandWorkDir）。
func IsHomeRelative(dir string) bool {
	return dir == "~" || strings.HasPrefix(dir, "~/")
}

// ExpandWorkDir 把 `~` / `~/…` 展开成 home，其他写法原样返回。
//
// **只在节点执行时调用**（见 cmd/agent 的 resolveChdir）。中枢/解析期绝不展开：
// run.workDir 参与 digest，展开后的路径随机器变，先展开再算 digest 等于让同一份
// 产物在不同机器上有不同摘要。
//
// 为什么只展开 `~`、不做 `$VAR` 展开：`~` 在 POSIX 里只有"当前用户的家目录"一种
// 读法；而 `os.ExpandEnv` 会把 `$ANOTIFY_TASKDIR`、`$PATH` 甚至未定义的变量一并
// 求值，等于把"声明一个目录"变成"节点上的一次表达式求值"——中枢不再能解释自己
// 下发的产物，排障时看到的路径也不再是定义里那一行。需要家目录就用 `~/`。
func ExpandWorkDir(dir, home string) string {
	switch {
	case dir == "~":
		return home
	case strings.HasPrefix(dir, "~/"):
		return filepath.Join(home, dir[len("~/"):])
	}
	return dir
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

// digestMetadata 是参与 digest 计算的**那两件**身份字段（契约 §3.2）。
//
// 刻意不能直接序列化 Metadata：它带着 displayName，而展示名不参与 digest ——
// 这正是 v3 把 displayName 从 Spec 搬走的理由（PATCH 改表不改产物 → 名字与产物
// 永久漂移）。把“参与 digest 的字段”写成一个**独立结构体**而不是靠 omitempty，
// 是为了让下一个给 Metadata 加字段的人不需要记得这件事：不在这里声明的就不进 digest。
type digestMetadata struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// digestDocument 是契约 §3.2 字面式的那份 canonical 文档：
//
//	digest = "sha256:" + hex(sha256(canonical{metadata.name, metadata.version, spec}))
//
// 刻意**不含** apiVersion / kind：两者都是 Validate 钉死的常量（anotify/v1、Capability），
// 写进摘要只是把两个恒定值混进来，不增加任何判别力。更重要的是形状：因此**任何人**
// （中枢、节点、第三方审计）都能从「name + version + 整份 spec」三件套重算出同一个
// digest —— P1-b 的 content_hash ≡ digest 就是这么定义的，两边不能分歧。
type digestDocument struct {
	Metadata digestMetadata `json:"metadata"`
	Spec     Spec           `json:"spec"`
}

// canonicalDoc 是 CanonicalJSON 的形状：整份产物，但 metadata 只取 name+version。
//
// 为什么还要 apiVersion/kind：CanonicalJSON 同时被当成「产物正文」用（store 的
// artifacts.spec 列、控制台把那一列解回 artifact.Artifact —— 见 console_handlers），
// 少了两者就不再是一份可 Parse 的产物。它们是常量，不参与 digest。
type canonicalDoc struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   digestMetadata `json:"metadata"`
	Spec       Spec           `json:"spec"`
}

// canonicalCopy 归一化一份产物的副本（**不修改调用方的对象**）：
//
//   - outputs 为空时给 []（AGENTS.md 的空列表纪律）；
//   - params / lifecycle 归一成「key 有序、无多余空白」。
//
// 后一条不同步的后果是**内容寻址整个失效**：中枢拿请求里的原始字节算 digest，
// 存下来的是归一后的 spec；节点（或任何第三方）从存下来的 spec 重算，得到的是另一个
// digest → 部署永远被判成 local（"节点上是本地变体"），而两边的产物内容其实一模一样。
// 症状极隐蔽：digest 看起来都合法（都是 sha256:…），只有把两个值并排看才会发现不同。
//
// lifecycle 也是 RawMessage（不透明透传）：encoding/json 不会替它排 key，所以这里
// 必须自己归一，否则 lifecycle: {retryable, maxAttempts} 两种书写顺序就是两份产物。
func canonicalCopy(a *Artifact) (*Artifact, error) {
	if a == nil {
		return nil, errors.New("artifact: nil")
	}
	clone := *a
	if clone.Spec.Outputs == nil {
		clone.Spec.Outputs = []string{}
	}
	var err error
	if clone.Spec.Params, err = canonicalRawMessage("params", clone.Spec.Params); err != nil {
		return nil, err
	}
	if clone.Spec.Lifecycle, err = canonicalRawMessage("lifecycle", clone.Spec.Lifecycle); err != nil {
		return nil, err
	}
	return &clone, nil
}

// canonicalRawMessage 归一化 spec 里的一个不透明 JSON 字段（空值原样返回）。
func canonicalRawMessage(field string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	canon, err := canonicalizeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("artifact: spec.%s 不是合法 JSON：%w", field, err)
	}
	return canon, nil
}

// CanonicalJSON 返回产物的规范字节（**不含 displayName**）。
//
// 确定性来自三处：struct 字段顺序固定、map key 由 encoding/json 排序、params/lifecycle
// 先归一。因此同一份产物无论用 YAML 还是 JSON 写、无论 key 顺序如何，结果相同。
//
// displayName 不在里面（契约 §3.2）：它是展示标签，不是内容。这也是把 Metadata 拆成
// digestMetadata 的原因 —— 只取 name+version，不靠“记得给新字段加 omitempty”。
func CanonicalJSON(a *Artifact) ([]byte, error) {
	clone, err := canonicalCopy(a)
	if err != nil {
		return nil, err
	}
	return json.Marshal(&canonicalDoc{
		APIVersion: clone.APIVersion,
		Kind:       clone.Kind,
		Metadata:   digestMetadata{Name: clone.Metadata.Name, Version: clone.Metadata.Version},
		Spec:       clone.Spec,
	})
}

// Digest 返回形如 "sha256:<64 hex>" 的内容寻址标识。
//
// 吃的是 {metadata.name, metadata.version, spec}（契约 §3.2），所以：
//
//   - 只改 displayName → digest **不变**（改展示名不产生新版本、不漂移）；
//   - 改任意 spec 字段（含 lifecycle / sticky / handler）或改 version → digest 变。
func Digest(a *Artifact) (string, error) {
	clone, err := canonicalCopy(a)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(&digestDocument{
		Metadata: digestMetadata{Name: clone.Metadata.Name, Version: clone.Metadata.Version},
		Spec:     clone.Spec,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// MarshalYAML 把产物序列化成落盘用的 YAML。
//
// 先转 JSON 再转 YAML：保证「落盘 → 重新 Parse」得到完全相同的 digest。
// 与 CanonicalJSON 的唯一差别是**保留 displayName**：它不进 digest，但不是不存在 ——
// 节点侧人读产物文件（cache 目录里的 artifact.yaml）时应该看得见作者取的那个名字，
// 否则一次落盘就静默丢掉了一个用户字段。
func MarshalYAML(a *Artifact) ([]byte, error) {
	clone, err := canonicalCopy(a)
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
