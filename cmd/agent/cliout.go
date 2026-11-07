package main

// 本文件是 CLI 的**机器可读面**（issue #318）：输出形状的判定（--json / 管道默认 JSON）、
// JSON 的写法、以及失败时的 `{"error":{"kind","message","hint"}}` 契约。
//
// 为什么单拎一个文件：`runs` / `show` / `upgrade` / `emit` 原本各打各的（表格、
// 缩进 JSON、一句中文、或者干脆只有退出码），机器读它们只能靠正则与退出码。
// 形状、kind 词表、hint 措辞必须只有**一份**，否则下一个子命令又会分叉出一套。
//
// 两条纪律：
//
//  1. TTY 上仍然说人话（表格 / 现有中文），管道上默认 JSON —— 与 hc 同一取舍。
//     例外是 `run`：它的 stdout 属于产物自己，不能被我们改形状（只换错误的那一路）。
//  2. kind 与退出码一一对应（见 classifyError 的注释与表格）。同一条错误在 TTY 与管道下
//     kind 必须相同，只是换了载体（文本 vs 单行 JSON）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/zzjcool/anotify/internal/artifact"
)

// outMode 是一次子命令运行的输出形状。
type outMode int

const (
	// outText：给人看（表格 / 缩进 JSON / 中文错误）。
	outText outMode = iota
	// outJSON：给机器看（一行一个 JSON 对象，错误也是）。
	outJSON
)

// outputMode 决定这次怎么说话：
//
//  1. 命令行里有 --json → JSON（显式要求永远赢）；
//  2. stdout 不是终端（管道 / 重定向）→ JSON —— agent 的主场景就是 `… | jq`；
//  3. 其余（人盯着终端）→ 文本/表格。
func outputMode(stdout io.Writer, args []string) outMode {
	if wantsJSON(args) {
		return outJSON
	}
	if stdoutIsTerminal(stdout) {
		return outText
	}
	return outJSON
}

// modeFor 是已经解析出 --json 之后的版本（子命令内部用，语义与 outputMode 一致）。
func modeFor(stdout io.Writer, wantJSON bool) outMode {
	if wantJSON {
		return outJSON
	}
	if stdoutIsTerminal(stdout) {
		return outText
	}
	return outJSON
}

// wantsJSON 在参数里找 --json（`-json` 同义，flag 包只认一种写法）。
//
// 用 token 扫描而不是 flag 解析：这个判断必须在**一处**成立——mainWith 报错时
// 也要立刻知道该说 JSON 还是中文，而 flag 集合散在 5 个子命令里。代价是
// `--note -json` 这种「值长得像 flag」会被读成 --json；后果只是多打一份 JSON，
// 不会把成败判错。
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "-json" || a == "--json" {
			return true
		}
	}
	return false
}

// stdoutIsTerminal 报告 stdout 是不是一个"人在看"的终端。
//
// 判据只有一个：`Stat().Mode()&os.ModeCharDevice`（也认 /dev/null 这类字符设备，
// 它们确实不是「被程序读走的管道」）。**只有 *os.File 回答得了**这个问题：
// 真实进程里 stdout 永远是 os.Stdout；测试/嵌入传进来的 bytes.Buffer 判不出来，
// 那时选**文本**——JSON 少了 jq 就没法读，文本至少还能看，人的那一路更安全。
func stdoutIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return true
	}
	info, err := f.Stat()
	if err != nil {
		return true
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// writeJSONLine 把一个形状紧凑地写成**一行**。
//
// 单行不是审美：管道里 `| jq` 与逐行 `read` 都按行工作，多行 JSON 会让
// 「一条记录一行」的假设失效（而且流里的正文会被转义成一整行，缩进毫无用处）。
func writeJSONLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码 JSON 输出: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		// 写 stdout 失败只可能是管道被关（`| head` / `| jq` 提前退出）：
		// 调用方一律当「已经说完了」，**不**因此改退出码（否则 `runs | head` 会退 1，
		// 与「没找到」混成同一个数字）。
		return fmt.Errorf("写 stdout: %w", err)
	}
	return nil
}

// ---------- 错误契约 ----------

// cliError 是失败时的机器可读形状（issue §2）。
//
//	{"error":{"kind":"port_not_allowed","message":"…","hint":"…"}}
//
// message 是**现有那句中文**（人不该为了机器丢掉解释）；hint 是「下一步做什么」
// （可重试 / 别重试 / 去要 root），源码里写得最好的那几句就搬在这里。
type cliError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// cliErrorEnvelope 是 {"error":{…}} 那一层（单独一个类型，字段名只写一次）。
type cliErrorEnvelope struct {
	Error cliError `json:"error"`
}

// kindError 给一个错误钉上机器可读的 kind 与 hint。
//
// 只有「错误自己知道自己是哪一类」时才用（歧义 ref、配置缺失、产物不合法…）；
// 其余靠 (命令, 退出码) 的默认表推出来（见 classifyError）。Unwrap 让
// errors.Is/As 照常工作（flag.ErrHelp、ErrRunNotFound 这些判据不受影响）。
type kindError struct {
	kind string
	hint string
	err  error
}

func (e *kindError) Error() string { return e.err.Error() }
func (e *kindError) Unwrap() error { return e.err }

// withKind 给错误钉上 kind/hint；err 为 nil 时原样返回 nil。
func withKind(err error, kind, hint string) error {
	if err == nil {
		return nil
	}
	return &kindError{kind: kind, hint: hint, err: err}
}

// configError 给配置类错误钉上 kind：文件不存在 vs 在但不可用（路径/权限/格式）。
//
// 两者对调用方的意义完全不同：config_missing 是「还没配」（去配），config_invalid
// 是「配了但不对」（去改）。混淆它们会让 agent 在「文件根本没写」时去改一份不存在的文件。
func configError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return withKind(err, kindConfigMissing, hintConfigMissing)
	}
	return withKind(err, kindConfigInvalid, hintConfigInvalid)
}

// kindOf 取错误上钉的 kind（没有则空串）。
func kindOf(err error) (kind, hint string) {
	var ke *kindError
	if errors.As(err, &ke) {
		return ke.kind, ke.hint
	}
	return "", ""
}

// kind 词表。命名照 internal/artifact 的 Code()（invalid_name / unsupported_api_version
// 那一路的短码）：小写、下划线分隔、不含空格——机器直接拿去 switch，人也能读。
const (
	kindUsage          = "usage"            // 参数形状错 / 位置参数多余
	kindConfigMissing  = "config_missing"   // 配置或档案根本不存在
	kindConfigInvalid  = "config_invalid"   // 配置在但不可用（路径、权限、格式）
	kindNotFound       = "not_found"        // 形状对，但盘上没有这条记录
	kindAmbiguousRef   = "ambiguous_ref"    // 短 ref 前缀撞上不止一条
	kindBadRequest     = "bad_request"      // emit：参数/请求形状错（2）
	kindPortNotAllowed = "port_not_allowed" // emit：出口不在契约里（3）
	kindSchemaMismatch = "schema_mismatch"  // emit：data 不满足下游要求（4）
	kindTaskGone       = "task_gone"        // emit：任务已非运行态（5）
	kindCommFailed     = "comm_failed"      // emit：通信失败/token/socket（6）
	kindAckTimeout     = "ack_timeout"      // emit：ack 超时（7）
	kindUnavailable    = "unavailable"      // upgrade：服务端没带二进制（2）
	kindNeedRoot       = "need_root"        // upgrade：目标不可写（3）
	kindBadSHA         = "bad_sha"          // upgrade：sha256 不符（4）
	kindFailed         = "failed"           // 兜底：网络/其他（upgrade 5 等）
)

// hint* 是各 kind 的「下一步」（issue §2：hint 不是装饰）。
const (
	hintUsage          = "看 `anotify-agent <子命令> -h`；命令表与退出码表在 `anotify-agent --skill`"
	hintConfigMissing  = "机器上还没有这份配置/档案：按 `anotify-agent --skill` §1 起一个节点，或检查 --data-dir"
	hintConfigInvalid  = "改配置/路径后重跑（node.yaml 只有 name/server/token/instances 四个键）"
	hintNotFound       = "用 `anotify-agent runs --json` 列出有哪些（--json 给完整 trace id）"
	hintAmbiguousRef   = "把前缀加长到唯一（至少 8 字符，也可以直接给完整 id）"
	hintBadRequest     = "改参数后重发；这一档重试不会成功"
	hintPortNotAllowed = "用 --to 指一个契约里列出的下游能力（可用候选在 message 里）"
	hintSchemaMismatch = "按出口契约补齐字段后**可以**重发（这一档不是致命错）"
	hintTaskGone       = "停止执行：任务已取消/过期/终结，重发只会一直拿到 5"
	hintCommFailed     = "可重试一次；仍失败就停（agent 没在跑，或本任务已结束）"
	hintAckTimeout     = "可重试：中枢按 (task_id, seq) 幂等，重复送达不会重复触发"
	hintUnavailable    = "别重试：换一个带 agent 的镜像，或用 --install-to 自己装"
	hintNeedRoot       = "用 root 重跑（message 里有一条能直接粘的 sudo install）"
	hintBadSHA         = "别原样重试：服务端二进制可能被改动，或传输被截断"
	hintFailed         = "看 message；网络类错误可以原样重跑"
)

// reportFailure 是失败的**唯一出口**：TTY 说人话，管道说 JSON。
//
// 文本版与改动前逐字一致（`anotify-agent <cmd>: <错误>`），所以现有依赖 stderr
// 的测试与人的肌肉记忆都不变；JSON 版多给一个 kind 与 hint。
func reportFailure(stderr io.Writer, mode outMode, cmd string, code int, err error) {
	if err == nil {
		return
	}
	if mode == outText {
		fmt.Fprintf(stderr, "anotify-agent %s: %s\n", cmd, err)
		return
	}
	kind, hint := classifyError(cmd, code, err)
	_ = writeJSONLine(stderr, cliErrorEnvelope{Error: cliError{
		Kind:    kind,
		Message: err.Error(),
		Hint:    hint,
	}})
}

// classifyError 把 (命令, 退出码, 错误) 翻成 (kind, hint)。
//
// 顺序：错误自己钉的 kind 优先（它可以比退出码更具体）→ 各命令的退出码表 →
// 兜底。exit 0 永远不会走到这里（没有错误就没有这条契约）。
func classifyError(cmd string, code int, err error) (kind, hint string) {
	if k, h := kindOf(err); k != "" {
		if h == "" {
			h = hintForKind(k)
		}
		return k, h
	}

	switch cmd {
	case emitCommand:
		// emit-protocol.md §2：这张表就是调用方的决策树。
		switch code {
		case exitEmitBadRequest:
			return kindBadRequest, hintBadRequest
		case exitEmitPort:
			return kindPortNotAllowed, hintPortNotAllowed
		case exitEmitSchema:
			return kindSchemaMismatch, hintSchemaMismatch
		case exitEmitTask:
			return kindTaskGone, hintTaskGone
		case exitEmitComm:
			return kindCommFailed, hintCommFailed
		case exitEmitAckTimeout:
			return kindAckTimeout, hintAckTimeout
		}
	case upgradeCommand:
		// 契约 §4：0/1 不是错误（1 是 --check 的「已最新」），所以不会走到这里。
		switch code {
		case exitUpgradeUnavailable:
			return kindUnavailable, hintUnavailable
		case exitUpgradeNeedRoot:
			return kindNeedRoot, hintNeedRoot
		case exitUpgradeBadSHA:
			return kindBadSHA, hintBadSHA
		}
	case runsCommand, showCommand:
		// 这两条命令只有 0/1 两档（runs_cmd.go 文件头的契约）：1 要么是
		// 「盘上没有」，要么是参数/数据目录的问题。更具体的 kind 在错误产生的
		// 地方用 withKind 钉好了（歧义、usage），这里只是兜底。
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return kindConfigMissing, hintConfigMissing
		case errors.Is(err, fs.ErrPermission):
			return kindConfigInvalid, hintConfigInvalid
		}
		return kindNotFound, hintNotFound
	case runCommand:
		// 产物不合法时复用 artifact 的词表（invalid_yaml / unknown_field /
		// invalid_name / unsupported_api_version …）：agent 不必解析中文。
		var artErr *artifact.Error
		if errors.As(err, &artErr) {
			return artifact.Code(err), "改产物 YAML 后重跑（`anotify-agent run` 不消耗中枢连接）"
		}
	}

	// 全局兜底：配置类的「文件不存在」与其余失败分开——它们要的处理方式不同。
	if errors.Is(err, fs.ErrNotExist) {
		return kindConfigMissing, hintConfigMissing
	}
	return kindFailed, hintFailed
}

// hintForKind 给「只钉了 kind、没钉 hint」的错误补一句通用提示。
func hintForKind(kind string) string {
	switch kind {
	case kindUsage:
		return hintUsage
	case kindConfigMissing:
		return hintConfigMissing
	case kindConfigInvalid:
		return hintConfigInvalid
	case kindNotFound:
		return hintNotFound
	case kindAmbiguousRef:
		return hintAmbiguousRef
	case kindBadRequest:
		return hintBadRequest
	case kindPortNotAllowed:
		return hintPortNotAllowed
	case kindSchemaMismatch:
		return hintSchemaMismatch
	case kindTaskGone:
		return hintTaskGone
	case kindCommFailed:
		return hintCommFailed
	case kindAckTimeout:
		return hintAckTimeout
	case kindUnavailable:
		return hintUnavailable
	case kindNeedRoot:
		return hintNeedRoot
	case kindBadSHA:
		return hintBadSHA
	default:
		return hintFailed
	}
}
