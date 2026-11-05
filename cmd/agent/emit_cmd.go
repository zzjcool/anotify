package main

// 本文件是 `anotify-agent emit` 子命令：**能力进程**（脚本 / LLM agent）用它把中途
// 产出交给本机 agent（规范：docs/design/emit-protocol.md §2）。
//
// 它不连中枢、也不认识中枢地址，只认两个环境变量：
//
//	ANOTIFY_EMIT_SOCK   agent 给的本地 socket 路径（任务运行期间存在）
//	ANOTIFY_TASK_TOKEN  本次任务的一次性令牌（socket 侧鉴权用）
//
// 为什么是「子命令 + 环境变量」而不是「库 + 中枢凭证」（§9）：
// 能力进程跑在沙箱里，把中枢凭证下发进去等于把路由权交给不可信代码；而这个子命令
// 只能说服**本机 agent** 代发一帧，路由权仍然只在中枢。
//
// # 退出码是给调用方看的语义（§2 的表格，逐条对应）
//
//	0 成功 · 2 参数/形状错 · 3 port 不在 ports[] · 4 data 不满足 expects
//	5 任务已非运行态 · 6 通信失败/token 无效/socket 不存在 · 7 ack 超时
//
// 「这次该重试还是该停」由退出码回答，脚本不需要（也不应该）解析错误文本。

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// emit 子命令专属的退出码（emit-protocol.md §2）。0 / 1 复用进程级的 exitOK / exitFailure。
const (
	// exitEmitBadRequest：参数/请求形状错（不是 JSON 对象、--port 为空、超体积）。
	exitEmitBadRequest = 2
	// exitEmitPort：被拒，port 不在 ports[]。
	exitEmitPort = 3
	// exitEmitSchema：被拒，data 不满足 expects[port].schema（补齐字段后**可以**重发）。
	exitEmitSchema = 4
	// exitEmitTask：任务已非运行态（cancel / 租约过期 / 已终结）→ 停止执行。
	exitEmitTask = 5
	// exitEmitComm：与 agent 通信失败 / token 无效 / socket 不存在 → 可重试一次。
	exitEmitComm = 6
	// exitEmitAckTimeout：ack 超时（--wait-routed 或本地落盘失败）→ 可重试（中枢幂等）。
	exitEmitAckTimeout = 7
)

const (
	// emitDialTimeout 是连本地 socket 的上限（本机 unix socket，正常是微秒级）。
	emitDialTimeout = 5 * time.Second
	// emitDurableTimeout 是默认路径（只等本地落盘）的上限。
	emitDurableTimeout = 30 * time.Second
	// emitRoutedReplyTimeout 是 --wait-routed 时等 agent 回话的上限：要比 agent 侧
	// 的 emitWaitRoutedTimeout 大一点，否则 CLI 会先把「agent 还在等中枢」判成通信失败。
	emitRoutedReplyTimeout = emitWaitRoutedTimeout + 15*time.Second
)

// emitOptions 是 `emit` 子命令解析出来的参数。
type emitOptions struct {
	to         string
	port       string
	data       string // 原始 --data（可能是 "@-"）
	note       string
	waitRouted bool
}

// parseEmitFlags 解析 `emit` 子命令的参数。
//
// 与 run 子命令同样用 ContinueOnError：退出码只由 runEmit 决定，flag 包只负责把
// usage 写到 stderr。
func parseEmitFlags(args []string, stderr io.Writer) (emitOptions, error) {
	fs := flag.NewFlagSet("agent emit", flag.ContinueOnError)
	fs.SetOutput(stderr)

	// --to 是脚本**该写的东西**：交给哪个下游能力（`name` / `name@version`）。
	// 端口是内部路由键，脚本不需要知道它叫什么（CONTRACT-hide-ports.md §2）。
	to := fs.String("to", "", "下游能力名（`name` 或 `name@version`）；不给 = 让中枢在候选里挑")
	data := fs.String("data", "", `产出的 JSON 对象；@- 表示从 stdin 读（缺省 = {}）`)
	note := fs.String("note", "", "人类可读备注（不参与路由）")
	waitRouted := fs.Bool("wait-routed", false,
		"等中枢确认已派发再返回（默认只等本地落盘：毫秒级）")

	if err := fs.Parse(args); err != nil {
		return emitOptions{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return emitOptions{}, fmt.Errorf("多余的参数: %s", strings.Join(extra, " "))
	}
	return emitOptions{
		to:         *to,
		data:       *data,
		note:       *note,
		waitRouted: *waitRouted,
	}, nil
}

// runEmit 是 `anotify-agent emit` 的实现。
//
// 返回 (退出码, error)：error 只用于**说明**（mainWith 打到 stderr 一次），
// 退出码本身已经表达「该重试还是该停」。
//
// 参数形态刻意显式（stdin / getenv 都是参数）：这条路径要被单测逐条钉住退出码表，
// 而 os.Stdin / os.Getenv 是全局状态，测不动。
func runEmit(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) (int, error) {
	opts, err := parseEmitFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil // -h：usage 已由 flag 包写到 stderr
		}
		return exitEmitBadRequest, err
	}

	port, err := resolveEmitPort(strings.TrimSpace(opts.to), getenv)
	if err != nil {
		return exitEmitBadRequest, err
	}
	// data 的**形状**（是 JSON 对象、不超体积）在本地先判：exit 2 是「别重试，去修参数」，
	// 白跑一次 socket 往返对这个判断没有任何帮助。
	dataJSON, err := prepareEmitData(opts.data, stdin)
	if err != nil {
		return exitEmitBadRequest, err
	}

	sock := strings.TrimSpace(getenv(envEmitSock))
	if sock == "" {
		// §8：本地模式（anotify-agent run）没有中枢，所以没有 socket。
		return exitEmitComm, fmt.Errorf(
			"本地模式无中枢，emit 不可用（%s 未设置：这是 `anotify-agent run` 的本地模式，"+
				"产出只能通过退出码/输出表达）", envEmitSock)
	}
	token := getenv(envTaskToken)

	dialer := net.Dialer{Timeout: emitDialTimeout}
	conn, err := dialer.Dial("unix", sock)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// §8：socket 不存在 = agent 没在跑，或任务已经结束（晚到的 emit 不许改已终结的任务）。
			return exitEmitComm, fmt.Errorf(
				"连不上 agent 的 emit socket %s（socket 不存在：agent 没在跑，或本任务已结束）: %w", sock, err)
		}
		return exitEmitComm, fmt.Errorf("连不上 agent 的 emit socket %s: %w", sock, err)
	}
	defer conn.Close()

	timeout := emitDurableTimeout
	if opts.waitRouted {
		timeout = emitRoutedReplyTimeout
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	req := emitRequest{
		Op:    emitOpName,
		Token: token,
		Port:  port,
		To:    strings.TrimSpace(opts.to),
		Data:  dataJSON,
		Note:  strings.TrimSpace(opts.note),
	}
	if opts.waitRouted {
		req.Wait = emitWaitRoutedName
	}
	line, err := json.Marshal(req)
	if err != nil {
		return exitEmitBadRequest, fmt.Errorf("编码 emit 请求: %w", err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return exitEmitComm, fmt.Errorf("把 emit 请求写给 agent 失败: %w", err)
	}

	var resp emitResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return exitEmitAckTimeout, fmt.Errorf("等 agent 的 emit 响应超时（%s）: %w", timeout, err)
		}
		return exitEmitComm, fmt.Errorf("读 agent 的 emit 响应失败: %w", err)
	}

	if code := emitExitCode(resp); code != exitOK {
		return code, fmt.Errorf("emit %s 未成功（seq=%d acked=%q rejected=%q）: %s",
			port, resp.Seq, resp.Acked, resp.Rejected, strings.TrimSpace(resp.Error))
	}
	fmt.Fprintf(stdout, "emit ok: port=%s seq=%d acked=%s\n", port, resp.Seq, resp.Acked)
	return exitOK, nil
}

// prepareEmitData 把 --data 归一成「JSON 对象」的紧凑字节。
//
//	--data 缺省 / 空      → {}
//	--data @-            → 从 stdin 读（管道里塞 payload.json）
//	--data '…'           → 原样当 JSON 解析
//
// 非对象（数组 / 标量 / 坏 JSON）与超体积都在这里拒绝：两者都是 exit 2
// （「修，别重试」），而 agent 侧的 schema 拒绝才是 exit 4。
func prepareEmitData(raw string, stdin io.Reader) (json.RawMessage, error) {
	text := raw
	if strings.TrimSpace(text) == "@-" {
		data, err := io.ReadAll(io.LimitReader(stdin, emitMaxDataBytes+1))
		if err != nil {
			return nil, fmt.Errorf("从 stdin 读 --data @- 失败: %w", err)
		}
		text = string(data)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return json.RawMessage("{}"), nil
	}
	if len(text) > emitMaxDataBytes {
		return nil, fmt.Errorf("--data 体积 %d 字节超过上限 %d 字节（别把整份内容塞进 emit）",
			len(text), emitMaxDataBytes)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		// 用 json.Valid 区分「不是合法 JSON」与「是 JSON 但不是对象」，错误才说得清。
		if !json.Valid([]byte(text)) {
			return nil, fmt.Errorf("--data 不是合法 JSON: %w", err)
		}
		return nil, fmt.Errorf("--data 必须是 JSON 对象（emit 的 data 直接就是下游的 params）")
	}
	compact, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("--data 不是可编码的 JSON: %w", err)
	}
	if len(compact) > emitMaxDataBytes {
		return nil, fmt.Errorf("--data 体积 %d 字节超过上限 %d 字节（别把整份内容塞进 emit）",
			len(compact), emitMaxDataBytes)
	}
	return compact, nil
}

// emitExitCode 把 agent 的响应翻成 §2 的退出码。
//
// 注意 schema 有两档（「data 不是对象」exit 2 / 「不满足 expects」exit 4）在线上都是
// rejected:"schema"：非对象那一档已经在 prepareEmitData 里被本地挡掉了，所以走到
// 这里的 schema 拒绝一定是 exit 4。
//
// 兜底是 7（ack 相关：落盘失败 / 中枢拒收 / --wait-routed 超时）。6 留给**本地**
// 就能判定的通信故障（socket 不存在 / 连不上 / 写读失败），它们根本到不了这里。
func emitExitCode(resp emitResponse) int {
	switch resp.Rejected {
	case emitRejectPort:
		return exitEmitPort
	case emitRejectSchema:
		return exitEmitSchema
	case emitRejectSize, emitRejectRate:
		return exitEmitBadRequest
	case emitRejectTask:
		return exitEmitTask
	case emitRejectToken:
		return exitEmitComm
	}
	if resp.OK {
		return exitOK
	}
	return exitEmitAckTimeout
}

// resolveEmitPort 决定这次 emit 走哪个出口。
//
// 脚本写的是 **--to 下游能力名**，不是端口名（CONTRACT-hide-ports.md §2）：
// 端口是内部路由键，用户在建能力时只填「下游能力（候选）」，系统把唯一出口
// 固定叫 out。逼脚本作者去抄这个名字，等于把实现细节漏给最终用户。
//
// 规则（**绝不猜**）：
//
//	--to 给了        → 找到"候选里含这个能力"的那个出口；找不到就拒（列出可用候选）
//	--to 没给        → 只有一个出口时用它；0 个 = 终端能力；多个 = 必须 --to
//
// 注意 --to 同时也被带进帧里（EmitFrame.To）：中枢据此**钉住**那一个候选，
// 而不是只按端口轮盘挑——否则脚本写 --to notify.phone，中枢却可能挑到
// script.echo，那就是"看起来发了，实际发给了别人"。
func resolveEmitPort(to string, getenv func(string) string) (string, error) {
	ports, err := emitContracts(getenv)
	if err != nil {
		// 一点线索都拿不到时无法判定任何事：直接说清楚，别猜一个端口发出去。
		return "", err
	}
	if to == "" {
		switch len(ports) {
		case 1:
			return ports[0].Port, nil
		case 0:
			return "", errors.New(
				"本能力没有下游（终端能力）：emit 出去不会触发任何能力。" +
					"要在跑完之后交给人/下一个能力，先去能力定义里填「下游能力」")
		default:
			return "", fmt.Errorf("本能力有多个出口，必须用 --to 指定下游能力（可用：%s）",
				candidateList(ports))
		}
	}
	name, ver := splitToRef(to)
	matched := make([]string, 0, 1)
	for _, pc := range ports {
		for _, tgt := range pc.Targets {
			if tgt.Capability != name {
				continue
			}
			if ver != 0 && tgt.Version != ver {
				continue
			}
			matched = append(matched, pc.Port)
			break
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", fmt.Errorf("--to %s 不是本能力的下游候选（可用：%s）", to, candidateList(ports))
	default:
		return "", fmt.Errorf("--to %s 同时出现在多个出口里（%s），这个能力确实有多个产出，请把它拆成两个能力",
			to, strings.Join(matched, ", "))
	}
}

// emitCLIContract 是契约里的一项（只取 CLI 侧需要的字段；runner.go 的
// emitPortContract 是**下发**方向的全量形状，两者读的是同一份 JSON）。
type emitCLIContract struct {
	Port    string `json:"port"`
	Targets []struct {
		Capability string `json:"capability"`
		Version    int    `json:"version"`
	} `json:"targets"`
}

// emitContracts 读出本次任务的出口契约（唯一来源：ANOTIFY_EMIT_PORTS，
// 缺省读 $ANOTIFY_TASKDIR/emit.json）。
//
// 与 agent 侧同一份字节：两处各读一套会让「脚本看到的候选」与「agent 接受的
// 候选」悄悄分叉，症状是脚本明明写了 --to，agent 却说没这个候选。
func emitContracts(getenv func(string) string) ([]emitCLIContract, error) {
	raw := strings.TrimSpace(getenv(envEmitPorts))
	if raw == "" {
		dir := strings.TrimSpace(getenv(envTaskDir))
		if dir == "" {
			return nil, fmt.Errorf("读不到出口契约（%s 与 %s 都没设置）", envEmitPorts, envTaskDir)
		}
		body, err := os.ReadFile(filepath.Join(dir, emitContractFile))
		if err != nil {
			return nil, fmt.Errorf("读出口契约 %s/%s: %w", dir, emitContractFile, err)
		}
		raw = string(body)
	}
	var items []emitCLIContract
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("出口契约不是合法 JSON: %w", err)
	}
	out := make([]emitCLIContract, 0, len(items))
	for _, it := range items {
		if strings.TrimSpace(it.Port) != "" {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

// candidateList 把候选渲染成 `name@ver` 列表（错误信息里最有用的就是"可选的其实有这些"）。
func candidateList(ports []emitCLIContract) string {
	seen := make(map[string]bool, 4)
	names := make([]string, 0, 4)
	for _, pc := range ports {
		for _, t := range pc.Targets {
			key := fmt.Sprintf("%s@%d", t.Capability, t.Version)
			if seen[key] {
				continue
			}
			seen[key] = true
			names = append(names, key)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "（没有候选）"
	}
	return strings.Join(names, ", ")
}

// splitToRef 把 `name@version` 拆开（没有 @ 时 ver=0 表示"任意版本"）。
func splitToRef(to string) (string, int) {
	if at := strings.LastIndex(to, "@"); at > 0 {
		if n, err := strconv.Atoi(to[at+1:]); err == nil {
			return to[:at], n
		}
	}
	return to, 0
}
