package main

// 本文件是 `-h` 的**命令表**（issue #317 §1）。
//
// 之前用的是 flag 包的默认 usage：它只会列 `-f / -grace / -data-dir` 三个 flag，
// 子命令一个都不出现。于是一个 agent 拿到这台机器上的二进制，跑完 help 只能
// 得出「它只会连中枢」，剩下的只能靠猜。这个文件把「它有什么命令、我该先跑哪个」
// 一次讲清。
//
// 为什么不是在每个子命令上挂文档、或者引 cobra：单横线长 flag（`-data-dir` /
// `-grace` / `-f`）已经固化在用户机器的 systemd unit 里（`scripts/install-agent.sh`
// 生成的 ExecStart），而 pflag 只认 `--data-dir` —— 换框架等于已装节点升级后
// 起不来，并且**失去再升级的通道**（只能人肉上机器改 unit）。cobra 也解决不了
// JSON 输出、错误 kind、schema 和这本手册，手册照样得自己 embed。见 issue「不做」。
//
// 命令表是 usage 文本的**单一事实源**：`writeUsage` 由它生成，单测逐条核对
// 「isSubcommand 认的每个名字都在表里」，`scripts/check_agent_skill.py` 再拿真实
// `-h` 输出校验 `skill.md` 里的例子（防漂移，见 issue §4）。

import (
	"flag"
	"fmt"
	"strings"
)

// agentCommand 是一条子命令在 `-h` 里的三件东西：名字带位置参数、一句人话、
// 一个能直接抄的示例（格式照 herdr-core 的 `hc --help`）。
type agentCommand struct {
	name    string // 子命令名，必须与 main.go 的分派常量一致
	arg     string // 位置参数形状（没有就空串）
	desc    string // 一句人话：这个命令回答什么问题
	example string // 一个可抄的完整命令行（含示例值）
}

// signature 是命令表第一列的文本（`run <capability.yaml>`）。
func (c agentCommand) signature() string {
	if c.arg == "" {
		return c.name
	}
	return c.name + " " + c.arg
}

// agentCommands 是全部子命令。顺序是「最常用的在前」：run 与 emit 是日常，
// check / schema 是**写**能力 yaml 时的两条（#319），capabilities / status 是本机事实面（#320），
// upgrade 是运维，runs / show 是排障。
//
// 加子命令时**必须**同时改这里和 isSubcommand：单测 TestUsageListsEverySubcommand
// 会两边对账，漏一个就红。
var agentCommands = []agentCommand{
	{
		name:    runCommand,
		arg:     "<capability.yaml>",
		desc:    "本地单跑一份产物：不连中枢、不占节点连接；退出码透传产物自己",
		example: "anotify-agent run ./capability.yaml -p repo=acme/x",
	},
	{
		name:    checkCommand,
		arg:     "<capability.yaml>",
		desc:    "只校验一份能力 yaml：一次列出全部问题；不执行、不连中枢、不落盘",
		example: "anotify-agent check ./capability.yaml",
	},
	{
		name:    schemaCommand,
		arg:     "[node|capability|emit]",
		desc:    "打印 JSON Schema：能力 yaml / node.yaml / 出口契约怎么写（缺省 capability）",
		example: "anotify-agent schema capability | jq '.properties.spec'",
	},
	{
		name:    emitCommand,
		arg:     "",
		desc:    "能力进程中途上报一个产出：只认 agent 注入的本地 socket；退出码 2..7 说明该不该重试",
		example: "anotify-agent emit --to notify.phone --data '{\"issue\":\"#42\"}'",
	},
	{
		name:    upgradeCommand,
		arg:     "",
		desc:    "用中枢当前带的那份二进制替换自己：校验 sha256 + 原子 rename；退出码 0..5 是升级结论",
		example: "anotify-agent upgrade --check",
	},
	{
		name:    runsCommand,
		arg:     "",
		desc:    "看本机跑过的执行：表格、时间倒序（不连中枢、不读 node.yaml、不落盘）",
		example: "anotify-agent runs --limit 5",
	},
	{
		name:    showCommand,
		arg:     "<ref>",
		desc:    "看那一次执行的输入与完整输出（ref 从 runs 的表里直接抄）",
		example: "anotify-agent show trc_0af7651916cd43dd8448eb211c80319c/notify.phone@2/3-a1",
	},
	{
		name:    capabilitiesCommand,
		arg:     "",
		desc:    "本机能跑什么：列缓存的产物（名字/版本、出口、digest）；空结果退 0（与 runs 相反）",
		example: "anotify-agent capabilities --json",
	},
	{
		name:    statusCommand,
		arg:     "",
		desc:    "本机 daemon 现在怎么样：pid/版本/连接/槽位/上次错误（本机视角，不是中枢视角）",
		example: "anotify-agent status --json",
	},
}

// usagePreamble 是 `-h` 的开头：**先把「不带子命令 = 常驻进程」说清**。
//
// 这一条是源码里 subcommandAt 注释讲的老坑（误跑一次会占掉节点连接、被 systemd
// 那份顶掉、退出码 1，用户却以为「命令报错了」），必须在第一屏出现，而不是藏在
// 某个子命令的说明里。
const usagePreamble = `anotify-agent —— Anotify 节点执行器（跑在你自己的机器上；中枢不执行，节点纯执行）

用法:
  anotify-agent [flag...]              节点模式：常驻进程，连上中枢等派发
  anotify-agent <子命令> [flag...]      一次性命令，跑完就走（见下表）

节点模式（不带子命令 = 常驻进程，不是查询）
  前台一直跑：读 node.yaml → 连中枢 → 接单 → 执行 → 回报，直到 SIGINT / SIGTERM。
  误跑一次就会占掉一个节点连接，把已经连着的那份顶下线（两边都会不停重连，
  日志里能看到 superseded）；它**不会**自己退——要手动杀掉多余的那个。只想看本机
  跑过什么，用 runs / show；本机能跑什么用 capabilities；它现在还好吗用 status。
  anotify-agent -f ~/.anotify/node.yaml -grace 20 --data-dir ~/.anotify

子命令:
`

// usageFlagsTail 是 flag 表之后的三段：进程级开关、退出码、发现钩子。
//
// 发现钩子固定在最后一行（issue §1）：agent 看一眼 help 就知道还有一本手册，
// 而不是拿到一个「只会连中枢」的二进制。
const usageFlagsTail = `
进程级开关（任何环境都能跑：不读 node.yaml、不连网、不落盘）:
  --version                            打印一行「版本 (commit, 构建时间)」
  --skill                              打印内嵌手册：起节点 / 写能力与上报 / 排障 / 退出码 / 不要做
  --json                               机器可读输出（capabilities / status / runs / show / upgrade / emit / check）；
                                       stdout 不是终端时自动开

退出码（节点模式；子命令各有自己的表，看 anotify-agent <子命令> -h）:
  0                                    正常退出（收到 SIGINT / SIGTERM 并收尾完毕，或 -h 看帮助）
  1                                    参数/配置错误、令牌被拒（401/403）、协议版本不兼容、
                                       实例数超顶、data-dir 不可用（**这几种不重试**）

  中枢/网络瞬态故障（拨号失败、5xx/530、超时、连接断开）**不退出**：进程内指数退避
  重连（1s→2s→4s… 封顶 30s，带抖动），成功握手后重置。加 -no-retry 退回「只试一次」。

Agent? 运行: anotify-agent --skill
`

// exitRow 是子命令 `-h` 里「退出码表」的一行：码 + 一句人话 +（可选）机器可读 kind。
//
// 表里只写**契约**（emit 2..7 / upgrade 0..5 / runs 0、1），不写实现细节：这些码是
// 调用方据以决定「重试 / 停 / 去要 root」的唯一依据。
type exitRow struct {
	code string // "0" / "2" / "产物自己的码" …
	desc string // 一句人话：这一档发生了什么、该做什么
	kind string // 空 = 这一档不是错误（没有 error.kind）
}

// subcommandHelpHook 是子命令 -h 的最后一行（与主 -h 的发现钩子同一句）。
const subcommandHelpHook = "Agent? 运行: anotify-agent --skill"

// writeSubcommandHelp 是各子命令 `-h` 的统一尾巴：用法 + flag 表 + 退出码表 + 手册钩子。
//
// 为什么退出码要进 -h：这些码是契约，此前只写在源码注释与 docs/design/emit-protocol.md
// 里，而拿到一台陌生机器上二进制的 agent 只看得见 -h（issue #318 §4）。flag 说明仍然
// 由 PrintDefaults 出自注册处（这里再抄一份必然漂移），所以只补那张表。
// notes 是子命令自己的补充说明（可选的几段人话）。它们放在 flag 表之后、退出码表
// 之前：`capabilities` 的「空结果为什么是 0」与 `status` 的「哪个 status / stale
// 怎么判」都必须出现在 -h 里（issue #320 §1/§2），而不是只写在源码注释里。
func writeSubcommandHelp(fs *flag.FlagSet, usage string, rows []exitRow, notes ...string) {
	w := fs.Output()
	fmt.Fprintf(w, "用法: %s\n\nflag:\n", usage)
	fs.PrintDefaults()
	for _, note := range notes {
		if strings.TrimSpace(note) == "" {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", note)
	}
	if len(rows) > 0 {
		fmt.Fprint(w, "\n退出码（kind 是 stdout 不是终端、或显式 --json 时的机器可读词）:\n")
		for _, r := range rows {
			fmt.Fprintf(w, "  %-6s %s\n", r.code, r.desc)
			if r.kind != "" {
				fmt.Fprintf(w, "  %-6s kind: %s\n", "", r.kind)
			}
		}
	}
	fmt.Fprint(w, "\n"+subcommandHelpHook+"\n")
}

// writeUsage 打印 `-h` 的全文（命令表 + flag 表 + 发现钩子）。
//
// flag 表用 fs.PrintDefaults()：flag 的说明写在注册处（main.go / 各子命令），
// 这里再抄一份必然漂移。进程级开关不在 FlagSet 里（它们在分派最前面就被拦下了，
// 见 preludeCommand），所以手写两行。
func writeUsage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprint(w, usagePreamble)
	// 列宽 28 = 最长那条签名（`schema [node|capability|emit]`）的宽度：签名超宽时
	// 续行会贴到左边，读者会以为是另一条命令。
	for _, c := range agentCommands {
		fmt.Fprintf(w, "  %-28s %s\n", c.signature(), c.desc)
		fmt.Fprintf(w, "  %-28s   %s\n", "", c.example)
	}
	fmt.Fprint(w, "\n全局 flag（节点模式）:\n")
	fs.PrintDefaults()
	fmt.Fprint(w, usageFlagsTail)
}
