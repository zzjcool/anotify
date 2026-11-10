// Package execenv 把一次 attempt 的执行输入算成真实的进程参数：
//
//   - 任务环境是**白名单**（PATH/HOME 基线 + 契约四变量 + run.env + envPassthrough
//     名字 + 参数），不是宿主完整环境；
//   - vars 三层 local > node > namespace 逐键解析（namespace 值只在本 attempt 内存里）；
//   - workDir 模板展开 + 绝对/无 .. /存在/是目录/解软链 + 与系统 GC 根不重叠；
//   - run.files 落盘时做**实际**路径边界检查（符号链接 / 硬链接都拒绝写穿）。
//
// 这里不读 os.Getenv：PATH 与命令探测都来自 hostenv.Baseline 的启动快照（同源）。
package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/zzjcool/anotify/cmd/anode/internal/hostenv"
	"github.com/zzjcool/anotify/internal/artifact"
	"github.com/zzjcool/anotify/internal/protocol"
)

// 环境来源（api-conventions envDiagnostics.source 的封闭集合，外加 builtin）。
const (
	SourceLocal       = "local"
	SourceNode        = "node"
	SourceNamespace   = "namespace"
	SourcePassthrough = "passthrough"
	SourceBuiltin     = "builtin"
)

// Diag 是环境诊断的一行：只有名字 + 来源，**没有值**。
type Diag struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// Error 是执行前可判别的配置错误（reason 是 protocol 的封闭失败码）。
//
// 这些错误都在 lease.grant 之后、真正 spawn 之前发生：attempt 已计，任务 failed，
// 不自动重试（确定性配置错误）。
type Error struct {
	// Reason ∈ workdir_unavailable / home_unavailable / files_escape / missing_requirement。
	Reason  string
	Message string
}

func (e *Error) Error() string { return e.Reason + ": " + e.Message }

// ReasonOf 返回一个错误的 protocol 失败码；非本包的错误返回空串。
func ReasonOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// Input 是一次 attempt 的环境求值输入（全部是快照：调用方在启动时取一次）。
type Input struct {
	// Artifact 是本次 attempt 钉住的产物（缓存里那份，已通过 digest/refs 校验）。
	Artifact *artifact.Artifact
	// LocalVars 是 anode.yaml 的本机 vars（与 MachineVars 同值时来源记 local）。
	LocalVars map[string]string
	// MachineVars 是 applied 里的机器级合并结果（local > node）。
	MachineVars map[string]string
	// NamespaceVars 是中枢随 dispatch 下发的 ns 默认值（只在本 attempt 内存里）。
	NamespaceVars map[string]string
	// Passthrough 是 anode.yaml 的 envPassthrough 名字白名单（值只在 Baseline 里）。
	Passthrough []string
	// Baseline 是守护进程启动环境快照（PATH / HOME / 白名单值）。
	Baseline hostenv.Baseline
	// Facts 是节点内置事实（status.*）。
	Facts protocol.NodeFacts
	// NodeName 是 metadata.name 的事实值。
	NodeName string
	// Params 是 dispatch 下发的参数对象。
	Params json.RawMessage
	// Endpoint / TaskToken / TaskID 是三个契约变量（ANOTIFY_PARAM_* 由参数生成）。
	Endpoint  string
	TaskToken string
	TaskID    string
	// ArtifactDir 是本 attempt 的系统导出目录（A2；空 = 尚未创建，不注入）。
	ArtifactDir string
}

// Resolved 是求值结果。
type Resolved struct {
	// Values 是最终会注入进程环境的键值（HOME 已校验）。
	Values map[string]string
	// Diags 是名字 + 来源（排序后），用于 envDiagnostics；不含值。
	Diags []Diag
	// Home 是最终生效的 HOME（绝对、存在、是目录）。
	Home string
	// Params 是展开后的参数名 → 字符串值（workDir 的 $(params.X) 用）。
	Params map[string]string
}

// Env 返回排序后的 KEY=VALUE 列表（直接喂给 exec.Cmd.Env）。
func (r *Resolved) Env() []string {
	keys := make([]string, 0, len(r.Values))
	for k := range r.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+r.Values[k])
	}
	return out
}

// SetBuiltin 注入一个节点提供的内置变量（如 A2 的 ANOTIFY_ARTIFACT_DIR）。
//
// 只给节点自己的值用：来源固定 builtin，诊断里不出现值。
func (r *Resolved) SetBuiltin(name, value string) {
	r.Values[name] = value
	for i := range r.Diags {
		if r.Diags[i].Name == name {
			r.Diags[i].Source = SourceBuiltin
			return
		}
	}
	r.Diags = append(r.Diags, Diag{Name: name, Source: SourceBuiltin})
	sort.Slice(r.Diags, func(i, j int) bool { return r.Diags[i].Name < r.Diags[j].Name })
}

// SourceOf 返回某个环境变量名的来源（诊断用；未注入返回空串）。
func (r *Resolved) SourceOf(name string) string {
	for _, d := range r.Diags {
		if d.Name == name {
			return d.Source
		}
	}
	return ""
}

// Resolve 求出本次 attempt 的进程环境。
//
// 顺序是刻意固定的：HOME 先定（run.env 可能覆盖它），再落 run.env 的其它键，
// 再落契约变量/参数，最后用 envPassthrough 补白名单里**没被 run.env 覆盖**的名字。
func Resolve(in Input) (*Resolved, error) {
	if in.Artifact == nil {
		return nil, &Error{Reason: protocol.ReasonMissingRequirement, Message: "缺少产物"}
	}
	run := in.Artifact.Spec.Run
	values := make(map[string]string, len(run.Env)+8)
	sources := make(map[string]string, len(run.Env)+8)
	set := func(name, value, source string) {
		values[name] = value
		sources[name] = source
	}

	// 1) PATH 固定为启动基线（run.env 不允许覆盖 PATH，artifact 写入期已拒）。
	if strings.TrimSpace(in.Baseline.PATH()) == "" {
		return nil, &Error{Reason: protocol.ReasonMissingRequirement,
			Message: "守护进程启动环境没有 PATH（任务 PATH 与命令探测同源，不能为空）"}
	}
	set("PATH", in.Baseline.PATH(), SourceBuiltin)

	// 2) HOME：默认 status.homeDir；run.env 显式 HOME 覆盖它。
	home := in.Facts.HomeDir
	if home == "" {
		home = in.Baseline.HOME()
	}
	homeSource := SourceBuiltin
	if v, ok := run.Env["HOME"]; ok {
		if v.IsValueFrom() {
			value, err := resolveFieldPath(v.FieldPath, in)
			if err != nil {
				return nil, err
			}
			home = value
		} else {
			home = v.Value
		}
		homeSource = SourceBuiltin
	}
	resolvedHome, err := validDir(home)
	if err != nil {
		return nil, &Error{Reason: protocol.ReasonHomeUnavailable,
			Message: fmt.Sprintf("HOME %q 不可用: %v", home, err)}
	}
	set("HOME", resolvedHome, homeSource)

	// 3) run.env 的其余键（字面量 / fieldRef）。
	keys := make([]string, 0, len(run.Env))
	for k := range run.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "HOME" {
			continue
		}
		switch {
		case k == "PATH":
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: "run.env 不允许覆盖 PATH（任务 PATH = 启动基线）"}
		case k == protocol.EnvArtifactDir || strings.HasPrefix(k, "ANOTIFY_"):
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("run.env 不允许覆盖契约变量 %s", k)}
		}
		v := run.Env[k]
		if !v.IsValueFrom() {
			set(k, v.Value, SourceBuiltin)
			continue
		}
		value, source, err := resolveFieldPathWithSource(v.FieldPath, in)
		if err != nil {
			return nil, err
		}
		set(k, value, source)
	}

	// 4) 契约变量（只有三个；ANOTIFY_PARAM_* 由参数生成）。
	if in.Endpoint == "" || in.TaskToken == "" || in.TaskID == "" {
		return nil, &Error{Reason: protocol.ReasonMissingRequirement,
			Message: "缺少 ANOTIFY_ 契约变量（emit 端点 / 任务令牌 / 任务 id）"}
	}
	set(protocol.EnvEmitEndpoint, in.Endpoint, SourceBuiltin)
	set(protocol.EnvTaskToken, in.TaskToken, SourceBuiltin)
	set(protocol.EnvTaskID, in.TaskID, SourceBuiltin)
	if in.ArtifactDir != "" {
		set(protocol.EnvArtifactDir, in.ArtifactDir, SourceBuiltin)
	}

	// 5) 参数（只有 spec.params 声明过的顶层字段；非 string 用原 JSON 文本）。
	params, err := paramsForEnv(in.Artifact.Spec.Params, in.Params)
	if err != nil {
		return nil, err
	}
	for name, value := range params {
		envName := protocol.ParamEnvName(name)
		if _, dup := values[envName]; dup {
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("参数 %q 归一成 %s 后与已注入的变量撞名", name, envName)}
		}
		set(envName, value, SourceBuiltin)
	}

	// 6) envPassthrough：只按名字白名单；run.env 同名优先；值取自启动快照。
	for _, name := range in.Passthrough {
		if _, ok := values[name]; ok {
			continue
		}
		switch {
		case name == "PATH" || name == "HOME" || strings.HasPrefix(name, "ANOTIFY_"):
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("envPassthrough 不允许 %s（应与配置校验一致）", name)}
		}
		value, ok := in.Baseline.Passthrough(name)
		if !ok {
			// 宿主没有这个变量：缺失 = 不注入（不是空串）。
			continue
		}
		set(name, value, SourcePassthrough)
	}

	diags := make([]Diag, 0, len(values))
	for name := range values {
		diags = append(diags, Diag{Name: name, Source: sources[name]})
	}
	sort.Slice(diags, func(i, j int) bool { return diags[i].Name < diags[j].Name })
	return &Resolved{Values: values, Diags: diags, Home: resolvedHome, Params: params}, nil
}

// resolveFieldPath 是 resolveFieldPathWithSource 的简化版（只要值）。
func resolveFieldPath(fieldPath string, in Input) (string, error) {
	value, _, err := resolveFieldPathWithSource(fieldPath, in)
	return value, err
}

// resolveFieldPathWithSource 解析封闭集合里的 fieldPath，并给出值来源。
//
// vars.<NAME>：local > node > namespace 逐键；status.* / metadata.*：机器事实。
func resolveFieldPathWithSource(fieldPath string, in Input) (string, string, error) {
	if strings.HasPrefix(fieldPath, "vars.") {
		name := strings.TrimPrefix(fieldPath, "vars.")
		if v, ok := in.LocalVars[name]; ok {
			return v, SourceLocal, nil
		}
		if v, ok := in.MachineVars[name]; ok {
			return v, SourceNode, nil
		}
		if v, ok := in.NamespaceVars[name]; ok {
			return v, SourceNamespace, nil
		}
		// 缺失 = 不注入（该 env 键不出现）；调用方按需要报 workdir_unavailable。
		return "", "", &missingField{fieldPath: fieldPath}
	}
	facts := in.Facts
	var value string
	switch fieldPath {
	case "metadata.name":
		value = in.NodeName
	case "status.hostIP":
		value = facts.HostIP
	case "status.hostname":
		value = facts.Hostname
	case "status.os":
		value = facts.OS
	case "status.arch":
		value = facts.Arch
	case "status.capacity.cpu":
		value = strconv.Itoa(facts.CPU)
	case "status.workRoot":
		value = facts.WorkRoot
	case "status.homeDir":
		value = facts.HomeDir
	default:
		return "", "", &Error{Reason: protocol.ReasonMissingRequirement,
			Message: fmt.Sprintf("fieldPath %q 不在封闭集合里", fieldPath)}
	}
	return value, SourceBuiltin, nil
}

// missingField 表示字段解析不出值（缺失 = 不注入，不是空串）。
type missingField struct{ fieldPath string }

func (m *missingField) Error() string { return m.fieldPath + ": 三层配置都没有这个值" }

// IsMissingField 报告错误是否是「字段解析不出值」。
func IsMissingField(err error) bool {
	var m *missingField
	return errors.As(err, &m)
}

// MissingFieldPath 返回缺失的 fieldPath（非缺失错误返回空串）。
func MissingFieldPath(err error) string {
	var m *missingField
	if errors.As(err, &m) {
		return m.fieldPath
	}
	return ""
}

// paramsForEnv 把 dispatch 参数转成 ANOTIFY_PARAM_* 的值。
//
// 只有 spec.params 里**声明过的**顶层字段才注入：未声明的额外 payload 字段按
// 「同名直取」规则忽略（capability.md §4.2），也就不会造成撞名。声明的字段之间
// 撞名在写入期已被 artifact 拒绝；这里再查一次是防御（绝不静默覆盖）。
func paramsForEnv(schema, params json.RawMessage) (map[string]string, error) {
	out := map[string]string{}
	if len(params) == 0 || strings.TrimSpace(string(params)) == "null" {
		return out, nil
	}
	var declared struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &declared); err != nil {
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: "spec.params 不是合法的 JSON Schema 对象: " + err.Error()}
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(params, &payload); err != nil {
		return nil, &Error{Reason: protocol.ReasonMissingRequirement,
			Message: "dispatch.params 必须是 JSON 对象: " + err.Error()}
	}
	seen := make(map[string]string, len(declared.Properties))
	for name := range declared.Properties {
		raw, ok := payload[name]
		if !ok {
			continue
		}
		value, err := paramString(raw)
		if err != nil {
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("参数 %q: %v", name, err)}
		}
		envName := protocol.ParamEnvName(name)
		if other, dup := seen[envName]; dup && other != name {
			return nil, &Error{Reason: protocol.ReasonMissingRequirement,
				Message: fmt.Sprintf("参数 %q 与 %q 归一后撞名 %s", name, other, envName)}
		}
		seen[envName] = name
		out[name] = value
	}
	return out, nil
}

// paramString 把参数值转成环境变量值：JSON string → 原字符串，其它 → 原 JSON 文本。
func paramString(raw json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", errors.New("参数值为空")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	// 非字符串：保留原始 JSON（对象/数组/数字/布尔/null）。
	return trimmed, nil
}

// workDirRefPattern 与 artifact 的模板语法逐字一致（只认 $(vars.X) / $(params.X)）。
var workDirRefPattern = regexp.MustCompile(`\$\((vars|params)\.([A-Za-z_][A-Za-z0-9_]*)\)`)

// ExpandWorkDir 展开 run.workDir 模板（空模板由调用方决定系统目录）。
//
// $(vars.X) 取的是 run.env 里声明的键 X 的**值**（不是 vars.X）；$(params.X) 取参数值。
// 解析后的路径必须是绝对路径、规范形式、无 ..，并且真实存在、是目录、解软链后仍在
// 允许的位置（由 ValidateDir 做系统根重叠检查）。
func ExpandWorkDir(template string, resolved *Resolved) (string, error) {
	if strings.TrimSpace(template) == "" {
		return "", nil
	}
	var expandErr error
	out := workDirRefPattern.ReplaceAllStringFunc(template, func(match string) string {
		if expandErr != nil {
			return ""
		}
		sub := workDirRefPattern.FindStringSubmatch(match)
		kind, name := sub[1], sub[2]
		switch kind {
		case "vars":
			value, ok := resolved.Values[name]
			if !ok {
				expandErr = &Error{Reason: protocol.ReasonWorkDirUnavailable,
					Message: fmt.Sprintf("run.workDir 引用了 $(vars.%s)，但该 env 键没有值（缺失 = 不注入）", name)}
				return ""
			}
			return value
		case "params":
			value, ok := resolved.Params[name]
			if !ok {
				expandErr = &Error{Reason: protocol.ReasonWorkDirUnavailable,
					Message: fmt.Sprintf("run.workDir 引用了 $(params.%s)，但本次没有这个参数值", name)}
				return ""
			}
			return value
		}
		return ""
	})
	if expandErr != nil {
		return "", expandErr
	}
	if strings.Contains(out, "$") {
		return "", &Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: fmt.Sprintf("run.workDir 展开后仍有未解析的引用: %q", out)}
	}
	if !filepath.IsAbs(out) || filepath.Clean(out) != out {
		return "", &Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: fmt.Sprintf("run.workDir 展开后必须是规范绝对路径，实际 %q", out)}
	}
	return out, nil
}

// ValidateDir 校验一个执行目录：存在、是目录、解软链后不与系统根重叠。
//
// 节点**不创建**用户 workDir（不自动 mkdir）；不存在的目录是配置问题。
func ValidateDir(dir string, systemRoots ...string) (string, error) {
	resolved, err := validDir(dir)
	if err != nil {
		return "", &Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: fmt.Sprintf("workDir %q 不可用: %v", dir, err)}
	}
	for _, root := range systemRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			// 系统根还不存在：只用字面路径比较（仍要拒绝祖先/后代重叠）。
			rootResolved = filepath.Clean(root)
		}
		if overlaps(resolved, filepath.Clean(rootResolved)) {
			return "", &Error{Reason: protocol.ReasonWorkDirUnavailable,
				Message: fmt.Sprintf("workDir %q 与系统清理根 %q 存在祖先/后代重叠（GC 安全边界）",
					dir, root)}
		}
	}
	return resolved, nil
}

// validDir 把路径解软链，并要求它是已存在的目录。
func validDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("路径为空")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("必须是绝对路径")
	}
	cleaned := filepath.Clean(dir)
	if cleaned != dir && filepath.Clean(dir) != dir {
		return "", errors.New("必须是规范形式（不要 .. / . / 重复斜杠）")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("不是目录")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	info, err = os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("解软链后不是目录")
	}
	return resolved, nil
}

// overlaps 报告 a 与 b 是否是同一个路径或互为祖先/后代（两边都应是绝对规范路径）。
func overlaps(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	// 根目录与任何绝对路径都重叠（GC 从 / 开始删就没有安全边界）。
	if a == sep || b == sep {
		return true
	}
	return strings.HasPrefix(a+sep, b+sep) || strings.HasPrefix(b+sep, a+sep)
}

// Materialize 把 run.files 展开进已经校验过的目录。
//
// 安全边界是**实际**的：
//   - 路径分量不得是符号链接（拒绝写穿），目标文件不得是符号链接 / 目录；
//   - 目标是已存在的普通文件时拒绝硬链接（Nlink>1，写进去等于改目录外的 inode）；
//   - 全部文件 0600，entrypoint 额外 0700（覆盖用户同名文件的正文与权限是真行为）；
//   - 写完后用 EvalSymlinks 再确认一次仍在该目录内（纵深防御）。
func Materialize(dir string, files map[string]string, entrypoint string) error {
	if strings.TrimSpace(dir) == "" {
		return &Error{Reason: protocol.ReasonWorkDirUnavailable, Message: "workDir 为空"}
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return &Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: fmt.Sprintf("workDir %q: %v", dir, err)}
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return &Error{Reason: protocol.ReasonWorkDirUnavailable,
			Message: fmt.Sprintf("workDir %q 不是一个普通目录", dir)}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		target, err := prepareFile(root, name)
		if err != nil {
			return err
		}
		if err := writeFile(target, body); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if name == entrypoint {
			mode = 0o700
		}
		if err := os.Chmod(target, mode); err != nil {
			return &Error{Reason: protocol.ReasonFilesEscape,
				Message: fmt.Sprintf("设置 %s 权限: %v", name, err)}
		}
	}
	return nil
}

// prepareFile 逐级校验路径并在需要时创建子目录；返回可以安全写入的绝对路径。
func prepareFile(root, name string) (string, error) {
	if name == "" || path.IsAbs(name) || strings.Contains(name, `\`) {
		return "", &Error{Reason: protocol.ReasonFilesEscape,
			Message: fmt.Sprintf("run.files 路径 %q 不合法", name)}
	}
	segments := strings.Split(name, "/")
	cur := root
	for i, seg := range segments {
		if seg == "" || seg == "." || seg == ".." {
			return "", &Error{Reason: protocol.ReasonFilesEscape,
				Message: fmt.Sprintf("run.files 路径 %q 含非法路径段 %q", name, seg)}
		}
		next := filepath.Join(cur, seg)
		info, err := os.Lstat(next)
		last := i == len(segments)-1
		switch {
		case errors.Is(err, os.ErrNotExist):
			if last {
				return next, nil
			}
			if err := os.Mkdir(next, 0o700); err != nil {
				return "", &Error{Reason: protocol.ReasonFilesEscape,
					Message: fmt.Sprintf("创建目录 %q: %v", seg, err)}
			}
			fi, err := os.Lstat(next)
			if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
				return "", &Error{Reason: protocol.ReasonFilesEscape,
					Message: fmt.Sprintf("目录 %q 创建后不是普通目录", seg)}
			}
		case err != nil:
			return "", &Error{Reason: protocol.ReasonFilesEscape,
				Message: fmt.Sprintf("检查 %q: %v", next, err)}
		case info.Mode()&os.ModeSymlink != 0:
			return "", &Error{Reason: protocol.ReasonFilesEscape,
				Message: fmt.Sprintf("run.files 路径 %q 的 %q 是符号链接（拒绝写穿）", name, seg)}
		case last:
			if !info.Mode().IsRegular() {
				return "", &Error{Reason: protocol.ReasonFilesEscape,
					Message: fmt.Sprintf("run.files 目标 %q 不是普通文件", name)}
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return "", &Error{Reason: protocol.ReasonFilesEscape,
					Message: fmt.Sprintf("run.files 目标 %q 是硬链接（拒绝写到目录外）", name)}
			}
		default:
			if !info.IsDir() {
				return "", &Error{Reason: protocol.ReasonFilesEscape,
					Message: fmt.Sprintf("run.files 路径 %q 的 %q 不是目录", name, seg)}
			}
		}
		cur = next
	}
	return cur, nil
}

// writeFile 写一个文件并复核它仍然是同一 inode（防止 Lstat 与 open 之间的软链替换）。
func writeFile(target, body string) error {
	before, err := os.Lstat(target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Reason: protocol.ReasonFilesEscape, Message: fmt.Sprintf("%s: %v", target, err)}
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return &Error{Reason: protocol.ReasonFilesEscape, Message: fmt.Sprintf("打开 %s: %v", target, err)}
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return &Error{Reason: protocol.ReasonFilesEscape, Message: fmt.Sprintf("stat %s: %v", target, err)}
	}
	if before != nil {
		if !os.SameFile(before, after) {
			return &Error{Reason: protocol.ReasonFilesEscape,
				Message: fmt.Sprintf("%s 在检查与打开之间被替换（拒绝写穿）", target)}
		}
	}
	if _, err := io.WriteString(f, body); err != nil {
		return &Error{Reason: protocol.ReasonFilesEscape, Message: fmt.Sprintf("写 %s: %v", target, err)}
	}
	return nil
}

// EntrypointPath 返回入口文件的绝对路径（run.entrypoint 已在 artifact 校验里证明
// 是 files 的 key，路径形状合法，这里只做 join）。
func EntrypointPath(dir, entrypoint string) string {
	return filepath.Join(dir, filepath.FromSlash(entrypoint))
}

// SystemWorkDir 是系统创建的每 attempt 独立目录（默认 workDir）。
//
// 目录名用 hex 编码，避免 taskId 里的字符影响文件系统布局；真实 taskId 存在 meta 里。
func SystemWorkDir(workRoot, taskID string, attempt int) string {
	return filepath.Join(workRoot, hexName(taskID), strconv.Itoa(attempt))
}

func hexName(s string) string {
	const digits = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) * 2)
	for i := 0; i < len(s); i++ {
		b.WriteByte(digits[s[i]>>4])
		b.WriteByte(digits[s[i]&0x0f])
	}
	return b.String()
}
