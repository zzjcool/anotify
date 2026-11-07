package main

// 本文件是**离线书写面**的第一条命令：`anotify-agent check <capability.yaml>`（issue #319 §1）。
//
// 写能力 yaml 的人（现在越来越多是 coding agent）此前只有两条验错的路：`run ./x.yaml`
// （**会真的执行**）或者等中枢下发失败。校验器其实早就在仓里——internal/artifact 的
// Parse 一次报**全部**问题，Validate 按字段产出 invalid_name / unsupported_api_version
// 这类短码——但没有一条命令把它暴露出来。
//
// 三条边界（也是它与 `run` 的全部区别，必须写进 help）：
//
//	不执行         只 Parse + Validate，绝不 exec 入口文件、绝不展开 files；
//	不连中枢       不读 node.yaml、不建连接、不碰 emit socket；
//	不落盘         不建缓存、不建工作目录、不写执行档案（`run` 会展开到临时目录并执行）。
//
// 为什么值得单独一条命令而不是 `run --check`：`run` 的约定是「退出码透传产物自己」，
// 一次误用的代价就是**真的跑了一遍**（可能删库、可能发通知）。校验必须是另一条命令，
// 让「我只是想看看 yaml 对不对」不需要任何信任。
//
// 输出口径与 runs / show（#318）一致：
//
//	TTY             人话：每个问题一行（短码 + 说明），先一行「N 个问题」；
//	管道 / --json   {"ok":false,"problems":[{"code","message"}]}，空问题是 []。
//
// 问题列表打在 **stdout** 上，因为它就是这条命令的**答案**（不是命令自身失败）：
// 退出码 1 判「yaml 不合格」，stderr 留空。命令自己出问题（文件读不到、参数形状不对）
// 才走 stderr 的 `{"error":{kind,message,hint}}`，两者因此分得开。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/zzjcool/anotify/internal/artifact"
)

// checkUsage 是 `check` 的用法串（错误信息与 -h 都指向它）。
const checkUsage = "anotify-agent check <capability.yaml> [--json]"

// checkExitRows 是 `check -h` 的退出码表。
//
// 两个「1」是两件事，必须分开写：产物不合格（stdout 有 problems）与命令自己出错
// （stderr 有 error）。合并成一句会让 agent 在「stdout 什么都没有」时以为「yaml 没问题」。
var checkExitRows = []exitRow{
	{code: "0", desc: "解析 + 校验全过（不执行、不连中枢、不落盘）"},
	{code: "1", desc: "产物不合格：stdout 一次列出全部问题（这一档没有 error.kind）"},
	{code: "1", desc: "命令自己出错：文件读不到 / 参数形状不对，stderr 是 error JSON",
		kind: kindUsage + " / " + kindConfigMissing + " / " + kindConfigInvalid},
}

// checkOptions 是 `check` 解析出来的参数。
type checkOptions struct {
	path string // 位置参数：产物 YAML（或 JSON）路径
	json bool   // --json：机器可读（缺省由 stdout 是否终端决定，见 cliout.go）
}

// checkProblem 是一条问题的机器可读形状：短码 + 人话。
//
// 短码直接来自 artifact.ProblemCode（与 apply 帧 reason / run 的 kind 是同一套词表），
// 所以 agent 可以拿它 switch（invalid_name / invalid_output / entrypoint_missing …）。
type checkProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// checkJSON 是 `check --json` 的形状（issue §1）：
//
//	{"ok":false,"problems":[{"code":"invalid_name","message":"…"}]}
//
// Problems 在没有问题时是 `[]` 而不是 `null`（AGENTS.md 的空列表纪律）：`jq -e
// '.problems | type=="array"'` 判形状时，null 会被读成「命令坏了」而不是「没问题」。
type checkJSON struct {
	OK       bool           `json:"ok"`
	Problems []checkProblem `json:"problems"`
}

// newCheckJSON 把 artifact 的原始问题文本翻成 JSON 形状（空 → []）。
func newCheckJSON(problems []string) checkJSON {
	out := checkJSON{OK: len(problems) == 0, Problems: make([]checkProblem, 0, len(problems))}
	for _, p := range problems {
		out.Problems = append(out.Problems, checkProblem{
			Code:    artifact.ProblemCode(p),
			Message: artifact.ProblemMessage(p),
		})
	}
	return out
}

// parseCheckFlags 解析 `check` 的参数，并取出位置参数路径。
//
// 路径允许写在 flag 前后（与 `run` / `show` 同一个做法：先取首个位置参数，再解析剩下
// 的）。`anotify-agent check ./x.yaml --json` 是最自然的写法，不能因为它报了「多余的
// 参数」就让人以为 --json 用不了。
func parseCheckFlags(args []string, stderr io.Writer) (checkOptions, error) {
	fs := flag.NewFlagSet("agent check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, checkUsage, checkExitRows) }

	jsonOut := fs.Bool("json", false,
		"机器可读输出：{\"ok\":…,\"problems\":[{\"code\",\"message\"}]}"+
			"（缺省：stdout 不是终端时自动开）")

	if err := fs.Parse(args); err != nil {
		return checkOptions{}, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return checkOptions{}, fmt.Errorf("用法: %s（只校验，不执行）", checkUsage)
	}
	path := rest[0]
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return checkOptions{}, err
		}
		if extra := fs.Args(); len(extra) > 0 {
			return checkOptions{}, fmt.Errorf("多余的参数: %s（check 只收一个产物路径）",
				strings.Join(extra, " "))
		}
	}
	return checkOptions{path: path, json: *jsonOut}, nil
}

// checkArtifact 是 check 的全部语义：Parse（内含 Validate）→ 一次拿到全部问题。
//
// 不返回第一层错误而是**问题切片**：Parse 的 *artifact.Error 本来就汇总了全部问题，
// 只报第一条正是这条命令要消灭的体验（改一条、再跑一次、再发现下一条）。
func checkArtifact(data []byte) (*artifact.Artifact, []string) {
	art, err := artifact.Parse(data)
	if err == nil {
		return art, nil
	}
	var artErr *artifact.Error
	if errors.As(err, &artErr) && len(artErr.Problems) > 0 {
		return nil, artErr.Problems
	}
	// 理论上不可达（Parse 只返回 *Error），但真到了这里也必须说清，而不是吞掉。
	return nil, []string{"invalid_artifact: " + err.Error()}
}

// checkPathError 把「读不到那份产物」翻成 kind + hint（hint 只针对 check）。
//
// 为什么不能直接用 configError：它的 hint 说的是 node.yaml（「只有四个键」），而
// check 根本不读 node.yaml —— 那种 hint 会把 agent 引去改另一份文件。kind 仍然复用
// 全局词表（config_missing / config_invalid），只有 hint 是这条命令自己的。
func checkPathError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return withKind(err, kindConfigMissing,
			"确认路径（如 `anotify-agent check ./capability.yaml`）；check 只读这一份产物，不读 node.yaml、不找 --data-dir")
	}
	return withKind(err, kindConfigInvalid,
		"换一个可读的文件路径（check 只读这一份产物；目录/权限问题它不做任何写入）")
}

// runCheck 是 `anotify-agent check` 的实现。
//
// 返回 (退出码, error)：error 只用于**命令自身**的故障（说明由 mainWith 打到 stderr
// 一次）；「产物不合格」是正常答案，走 stdout + 退出码 1，error 为 nil。
func runCheck(args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseCheckFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr
		}
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}
	mode := modeFor(stdout, opts.json)

	// 只读这一个文件：不读 node.yaml、不读数据目录、不建连接。
	data, err := os.ReadFile(opts.path)
	if err != nil {
		// 文件不存在 vs 在但读不了（权限/是目录）：agent 的下一步不同，用 kind 分开。
		return exitFailure, checkPathError(err)
	}

	art, problems := checkArtifact(data)
	if len(problems) > 0 {
		writeCheckProblems(stdout, mode, opts.path, problems)
		return exitFailure, nil
	}

	writeCheckOK(stdout, mode, opts.path, art)
	return exitOK, nil
}

// writeCheckProblems 打全部问题：JSON 一份形状，人话一行一个问题。
func writeCheckProblems(w io.Writer, mode outMode, path string, problems []string) {
	if mode == outJSON {
		// 写 stdout 失败只可能是管道被关（`| head`）：不改退出码（同 runs/show）。
		_ = writeJSONLine(w, newCheckJSON(problems))
		return
	}
	fmt.Fprintf(w, "%s: %d 个问题\n", path, len(problems))
	for _, p := range problems {
		// 每行一条、原样打（问题文本自己就是 "<短码>: <人话>"）：按行 grep 短码即可。
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "（check 只校验：不执行、不连中枢、不落盘。改完 yaml 再跑一次这条命令）")
}

// writeCheckOK 打「通过」：人话给一句解析结果，JSON 只给 {"ok":true,"problems":[]}。
//
// 人话那行带 name@version 与文件/出口数量不是装饰：它是**解析器对这份 yaml 的理解**，
// 作者能一眼看出「名字/版本是不是我想要的」「出口是不是少了一个」，而不只是"没报错"。
func writeCheckOK(w io.Writer, mode outMode, path string, art *artifact.Artifact) {
	if mode == outJSON {
		_ = writeJSONLine(w, newCheckJSON(nil))
		return
	}
	fmt.Fprintf(w, "%s: 通过 · %s@%d\n", path, art.Metadata.Name, art.Metadata.Version)
	fmt.Fprintf(w, "  run.entrypoint=%s · %d 个文件 · 出口 %s\n",
		art.Spec.Run.Entrypoint, len(art.Spec.Run.Files), outputsSummary(art.Spec.Outputs))
}

// outputsSummary 渲染出口名列表（空 = 终端能力，不是「忘了写」）。
func outputsSummary(outputs []string) string {
	if len(outputs) == 0 {
		return "（无：终端能力，不 emit）"
	}
	return strings.Join(outputs, ",")
}
