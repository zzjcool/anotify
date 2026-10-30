package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// 本文件是 agent 的**执行层**：把中枢派来的任务（capability + params）交给本地
// node.yaml 里声明的命令去跑，返回一个 outcome；client 侧只负责把 outcome 编码成
// result 帧。
//
// 边界（契约 I1）：命令来自本地 node.yaml 的 run.command **原文**，中枢从不告诉
// 节点「跑什么」。所以这里：
//
//   - 命令原样交给 `sh -c`：零插值、零拼接、零引号处理；
//   - 参数只走环境变量（ANOTIFY_PARAM_*），命令想用就自己读 `$ANOTIFY_PARAM_XXX`。
//
// 为什么不做 {{param}} 插值：把中枢给的输入拼进命令行，等于把注入面交给中枢；
// 环境变量是单向的数据通道，不参与命令解析。
//
// # 不设 ANOTIFY_TRACE_ID（契约 §10 缺口 1）
//
// issue #184 要求导出 trace_id，但 assign 帧里没有这个字段
// （internal/ws.AssignFrame = task_id / capability / version / params /
// lease_until / max_duration_sec），而「不改 internal/ws、不新增协议字段」是硬
// 约束。所以 v1 **不导出**该变量：等协议层加上 trace_id 再补。

// job 是一次执行请求：中枢能表达的全部（能力名、版本、参数、租约兜底）加上本地
// 找到的 run 块。Spec 为 nil 表示没有可用的执行方式（节点不能执行这个能力）。
type job struct {
	TaskID      string
	Capability  string
	Version     int
	Params      map[string]any
	Spec        *runSpec
	MaxDuration int // assign.MaxDurationSec：>=1 时作为兜底超时
}

// outcome 是一次执行的结果。
type outcome struct {
	Status    string   // statusSucceeded | statusFailed
	Out       *string  // 出口名：仅「成功且 run.out 去空白后非空」时非 nil；其余（未声明/失败/超时/取消）恒 nil
	Summary   string   // 一行
	Artifacts []string // 恒非 nil（空返回 []）
	Err       string   // failed 时非空
	Output    string   // 合并后的子进程输出（只进日志，不进 result 帧）
}

const (
	// maxCapturedOutput 是 stdout+stderr 合并捕获的上限：任务输出只用于日志与
	// summary，不能让一个话痨命令把 agent 的内存吃光。
	maxCapturedOutput = 64 << 10
	// summaryMaxBytes 是 summary 的字节上限（一行、UTF-8 安全截断）。
	summaryMaxBytes = 200
)

// runJob 执行一次任务。**不 panic**：所有失败都写进 outcome.Err（client 侧照常
// 回 result 帧）。
//
// 上下文语义：ctx 被取消（cancel 帧 / 退出排空）或超时 → 杀整个进程组，outcome
// 记 failed（Err 分别是 canceled / timed out after ...）。
func runJob(ctx context.Context, j job) outcome {
	res := outcome{Status: statusFailed, Out: nil, Artifacts: []string{}}

	// 防御：client 侧正常会在派发前挡掉没有 run 的能力，但执行层自己也不能在
	// nil Spec 上 panic，更不能把空命令当成功（`sh -c ''` 退出 0）。
	if j.Spec == nil {
		res.Err = fmt.Sprintf("no run spec for %s@%d", j.Capability, j.Version)
		return res
	}
	if strings.TrimSpace(j.Spec.Command) == "" {
		res.Err = fmt.Sprintf("%s@%d: run.command is empty", j.Capability, j.Version)
		return res
	}

	env, err := buildEnv(os.Environ(), j)
	if err != nil {
		res.Err = "build env: " + err.Error()
		return res
	}

	// 本地超时优先（node.yaml 是节点自己声明的执行方式），中枢的 max_duration_sec
	// 只在本地没写超时时兜底。
	timeout := effectiveTimeout(j)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	out := &cappedBuffer{limit: maxCapturedOutput}
	// 命令原文，零插值：sh -c 的字符串就是 node.yaml 里那一行。
	cmd := exec.CommandContext(ctx, "sh", "-c", j.Spec.Command)
	cmd.Env = env
	// Stdin 不接（nil → /dev/null）：命令不可能因为等 stdin 而挂死。
	cmd.Stdout = out
	cmd.Stderr = out // 同一个 writer：exec 只用一条管道，stdout/stderr 按写序合并
	// 独立进程组：sh -c 派生出来的孙进程都在同一个组里，才能被一次性杀干净。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 覆盖默认的 Cancel（默认只对**直接子进程** SIGKILL）：sh 死了但 sleep 之类
	// 的孙进程还活着时，它们仍占着输出管道，Wait 会一直等下去。杀整组才收得住。
	// Cancel 只在 Start 成功后才会被调用，所以 cmd.Process 一定非 nil；
	// 组里已经没有进程时内核返回 ESRCH，那是正常情况，不当错误。
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }

	if err := cmd.Start(); err != nil {
		// 明确区分「起不来」（sh 缺失、权限…）与「跑完失败」。
		res.Err = "start command: " + err.Error()
		res.Output = out.String()
		res.Summary = oneLine(res.Output, summaryMaxBytes)
		return res
	}

	err = cmd.Wait()
	res.Output = out.String()
	res.Summary = oneLine(res.Output, summaryMaxBytes)

	switch {
	case err == nil && ctx.Err() == nil:
		// 成功判定要同时看 exit code 与 ctx：命令刚好在超时/取消那一刻 exit 0 时，
		// 它不是「跑完了」，而是「被我们掐断的」，不能当成功（更不能回报出口）。
		res.Status = statusSucceeded
		// 出口名：node.yaml 自己声明的固定字符串（去空白后）。空白 = 未声明 →
		// 保持 nil（帧里 out 键缺席 = 分支终止）。不做插值/映射/校验。
		if s := strings.TrimSpace(j.Spec.Out); s != "" {
			res.Out = &s
		}
		if res.Summary == "" {
			// 命令没有输出（echo -n、true …）：summary 不能是空串，
			// 否则服务端/前端看起来像「没回报」。
			res.Summary = "exit 0"
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Err = "timed out after " + timeout.String()
	case errors.Is(ctx.Err(), context.Canceled):
		res.Err = "canceled"
		if err != nil {
			res.Err += ": " + err.Error()
		}
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// "exit status 7" / "signal: killed"
			res.Err = exitErr.Error()
		} else {
			res.Err = "run command: " + err.Error()
		}
		if res.Summary != "" {
			// 只带首行：错误要能一眼看出是哪儿炸的，但别把整段日志塞进 error 字段。
			res.Err += ": " + res.Summary
		}
	}
	return res
}

// buildEnv 组织子进程环境：父环境（剔除 ANOTIFY_* 残留）+ node.yaml 的 run.env +
// 契约变量（优先级最高），最后**按变量名排序**返回，便于测试直接比对。
//
// 纯函数：不读 os.Environ（base 由调用方给），不依赖时间/随机。
func buildEnv(base []string, j job) ([]string, error) {
	env := make(map[string]string, len(base)+8)

	// 1) 父进程环境。先剔除所有 ANOTIFY_*：agent 自己可能被别的东西包着跑，
	//    残留的 ANOTIFY_TASK_ID 会让子进程看到「上一个任务」的身份。
	for _, kv := range base {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue // 形状坏的条目直接丢（"=x" 塞进子进程环境没意义）
		}
		if hasAnotifyPrefix(name) {
			continue
		}
		env[name] = value
	}

	// 2) node.yaml 的 run.env：节点运维自己的变量。ANOTIFY_* 一律忽略——
	//    这些是契约变量，由下面第 3 步独占（run.env 不得覆盖）。
	if j.Spec != nil {
		for name, value := range j.Spec.Env {
			if name == "" || hasAnotifyPrefix(name) {
				continue
			}
			env[name] = value
		}
	}

	// 3) 契约变量。
	paramsJSON := []byte("{}")
	if len(j.Params) > 0 {
		b, err := json.Marshal(j.Params)
		if err != nil {
			return nil, fmt.Errorf("marshal params: %w", err)
		}
		paramsJSON = b
	}
	env["ANOTIFY_TASK_ID"] = j.TaskID
	env["ANOTIFY_CAPABILITY"] = fmt.Sprintf("%s@%d", j.Capability, j.Version)
	env["ANOTIFY_PARAMS"] = string(paramsJSON)

	// 每个顶层参数一份 ANOTIFY_PARAM_<NAME>。按 key 排序后再生成：归一化之后
	// 可能撞名（my-param / my_param → MY_PARAM），排序保证同一份 params 每次
	// 得到同一个结果（字典序靠后者的值赢）。
	keys := make([]string, 0, len(j.Params))
	for key := range j.Params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := paramEnvValue(j.Params[key])
		if err != nil {
			return nil, fmt.Errorf("param %q: %w", key, err)
		}
		env["ANOTIFY_PARAM_"+paramEnvName(key)] = value
	}

	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+env[name])
	}
	return out, nil
}

// hasAnotifyPrefix 判断环境变量名是否在 agent 自己的命名空间里（大小写敏感：
// POSIX 环境变量名本身大小写敏感，这里只认全大写的契约前缀）。
func hasAnotifyPrefix(name string) bool {
	return strings.HasPrefix(name, "ANOTIFY_")
}

// paramEnvValue 把参数值渲染成环境变量里的文本（契约 §3.3 的表格）：
//
//	null            → ""（空串：变量存在但没值）
//	string          → 原值（不加引号）
//	number / bool   → JSON 字面量（42 / 1.5 / true）
//	object / array  → 紧凑 JSON（{"a":1} / [1,2]）
//
// 除 null 和 string 之外一律走 json.Marshal：它输出的就是紧凑 JSON，而对
// number/bool 恰好就是字面量，不必为每种类型分叉。
func paramEnvValue(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal param value: %w", err)
	}
	return string(b), nil
}

// effectiveTimeout 取本次执行的超时：node.yaml 的 run.timeoutSec 优先，
// 其次是中枢给的 max_duration_sec；都没有 → 0（不设本地超时，交给中枢租约）。
func effectiveTimeout(j job) time.Duration {
	if j.Spec != nil && j.Spec.TimeoutSec > 0 {
		return time.Duration(j.Spec.TimeoutSec) * time.Second
	}
	if j.MaxDuration > 0 {
		return time.Duration(j.MaxDuration) * time.Second
	}
	return 0
}

// paramEnvName 把参数名归一成环境变量名片段：大写 + 非 [A-Z0-9_] 一律变 '_'。
//
// 例：title → TITLE，my-param → MY_PARAM，a.b/c → A_B_C。
func paramEnvName(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToUpper(key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			// 按 rune 归一（不是按字节），一个非 ASCII 字符只变成一个 '_'。
			b.WriteByte('_')
		}
	}
	return b.String()
}

// oneLine 取一行摘要：去首尾空白、取**第一个非空行**，再按字节截断到 maxBytes
// 以内（截断点回退到 UTF-8 字符边界，绝不切出半个汉字）。
func oneLine(s string, maxBytes int) string {
	line := ""
	for _, candidate := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(candidate); t != "" {
			line = t
			break
		}
	}
	if maxBytes <= 0 || len(line) <= maxBytes {
		return line
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return line[:cut]
}

// cappedBuffer 是带上限的输出缓冲：写满之后只记住「被截断了」，不再增长，
// 但 Write 永远报告「全部写入」——短写会让 exec 把子进程判成写错误。
type cappedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// Write 实现 io.Writer（stdout/stderr 共用一个实例）。
func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) <= room {
			b.buf = append(b.buf, p...)
		} else {
			b.buf = append(b.buf, p[:room]...)
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

// String 返回捕获到的输出；被截断时在末尾追加一句提示（说明「后面还有」）。
func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := string(b.buf)
	if b.truncated {
		s += fmt.Sprintf("\n[agent: output truncated at %d bytes]\n", b.limit)
	}
	return s
}

// killProcessGroup 给**整个进程组**发 SIGKILL（负 pid = 进程组）。
//
// 组里已经没有进程时内核返回 ESRCH：那是正常情况（子进程先自己退出，或者上一次
// 已经杀过），当成功处理，免得 Wait 那边报出莫名其妙的 "no such process"。
func killProcessGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
