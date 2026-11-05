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

	port := fs.String("port", "",
		"出口名；本能力只有一个出口时**可以省略**（自动取它）")
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
		port:       *port,
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

	port, err := resolveEmitPort(strings.TrimSpace(opts.port), getenv)
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
// 为什么允许省略 --port（CONTRACT-hide-ports.md §2）：端口是**路由键**，不是用户
// 要表达的概念——创建能力时用户只在界面上填「下游能力（候选）」，系统内部把唯一
// 出口固定叫 out。逼脚本作者去抄这个名字，等于把实现细节漏给最终用户。
//
// 规则刻意是"只有唯一出口才自动"：
//
//	1 个出口   → 用它（99% 的能力）
//	0 个出口   → 报错：这是终端能力，emit 出去也没有下游（I5：0 次 emit = 到此为止）
//	≥2 个出口  → 报错并**列出可用端口**，绝不猜（猜错 = 产出走错边，而且没有任何报错）
func resolveEmitPort(explicit string, getenv func(string) string) (string, error) {
	ports, err := emitPortNames(getenv)
	if err != nil {
		// 契约读不出来时，显式给了 --port 就放行：路由的对错最终由中枢裁决
		// （port 不在 ports[] → 中枢拒收 + 退出码 3），没有理由在本地先拦住。
		if explicit != "" {
			return explicit, nil
		}
		return "", err
	}
	if explicit != "" {
		return explicit, nil
	}
	switch len(ports) {
	case 1:
		return ports[0], nil
	case 0:
		return "", errors.New(
			"本能力没有出口（终端能力）：emit 出去也不会触发任何下游。" +
				"要在跑完之后交给人/下一个能力，先去能力定义里填「下游能力」")
	default:
		return "", fmt.Errorf("本能力有 %d 个出口，必须用 --port 指定：%s",
			len(ports), strings.Join(ports, ", "))
	}
}

// emitPortNames 从出口契约里取出端口名（升序）。
//
// 来源与 agent 侧完全一致（envEmitPorts 环境变量，缺省读 $ANOTIFY_TASKDIR/emit.json）：
// 两处各读一套会让「脚本看到的出口」与「agent 接受的出口」悄悄分叉。
func emitPortNames(getenv func(string) string) ([]string, error) {
	raw := strings.TrimSpace(getenv(envEmitPorts))
	if raw == "" {
		if dir := strings.TrimSpace(getenv(envTaskDir)); dir == "" {
			return nil, fmt.Errorf("读不到出口契约（%s 与 %s 都没设置）", envEmitPorts, envTaskDir)
		}
		body, err := os.ReadFile(filepath.Join(getenv(envTaskDir), emitContractFile))
		if err != nil {
			return nil, fmt.Errorf("读出口契约 %s/%s: %w", getenv(envTaskDir), emitContractFile, err)
		}
		raw = string(body)
	}
	var items []struct {
		Port string `json:"port"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("出口契约不是合法 JSON: %w", err)
	}
	ports := make([]string, 0, len(items))
	for _, it := range items {
		if p := strings.TrimSpace(it.Port); p != "" {
			ports = append(ports, p)
		}
	}
	sort.Strings(ports)
	return ports, nil
}
