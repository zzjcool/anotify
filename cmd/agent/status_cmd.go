package main

// 本文件是 `anotify-agent status`：**本机 daemon 现在怎么样**（issue #320 §2）。
//
// 先把「哪个 status」说清：这是**本机视角**——我连没连上、几个槽位在忙、跑的是
// 哪一版、上次为什么退出。中枢视角（节点在不在线、任务队列、控制台）在控制台里，
// 不在这个命令里，也不打算加（跨机器是另一个域，见 issue「不做」）。
//
// 数据源只有一个：`<data-dir>/state.json`（daemon 每心跳写一次，见 state.go）。
// CLI 只读文件，不连 socket：现在唯一的本地 socket 是任务期的 emit，给它开常驻
// 控制面 = 多一个攻击面，而我们要的只是「看一眼」。
//
// # stale 与退出码
//
// `pid` 不在了，或 `updatedAt` 超过 3 个心跳周期 → `stale: true` 且**退出 1**。
// 意思是「这是最后一次已知状态」，不是「命令坏了」：daemon 被 kill -9 之后，
// 文件会永远停在最后一拍，只有这条判定能戳穿它。live 才是 0。
//
// 文件不存在（这台机器从没起过 daemon / --data-dir 指错）也走 stale + 1，但
// stdout 上仍然给出**完整形状**（pid=0、stale=true）：形状稳定比省略键更重要，
// `jq -e '.stale'` 在这种机器上也照样能读懂。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// statusUsage 是 `status` 的用法串。
const statusUsage = "anotify-agent status [--data-dir DIR] [--json]"

// statusHelpNote 是 `-h` 里对「哪个 status」与 stale 语义的说明（issue §2 要求进 help）。
const statusHelpNote = `这是**本机视角**（本机 daemon 写的 state.json），不是中枢视角；中枢视角在控制台。
stale 判定：pid 不在了，或 updatedAt 超过 3 个心跳周期 → stale: true 且退出 1：
这是最后一次已知状态，不是假装它还活着。CLI 只读文件，不连 socket。`

// statusExitRows 是 `-h` 的退出码表。
var statusExitRows = []exitRow{
	{code: "0", desc: "daemon 活着：pid 在，且 state.json 最近 3 个心跳周期内更新过"},
	{code: "1", desc: "stale：pid 不在了 / updatedAt 太旧 / 从没起过 daemon（输出仍是最后一份已知状态）",
		kind: kindStale + " / " + kindConfigMissing + " / " + kindConfigInvalid},
}

// kindStale 是「状态过旧」的 kind（与「没找到」分开：文件在，只是不能当真）。
const kindStale = "stale"

// hintStale 是 stale 时的下一步。
const hintStale = "这是最后一次已知状态：用 systemctl status anotify-agent（或 docker ps）看进程，" +
	"或直接看 --skill §1 起节点"

// statusOptions 是 `status` 解析出来的参数。
type statusOptions struct {
	dataDir string // --data-dir：缺省空串 = ~/.anotify
	json    bool   // --json：机器可读
}

// parseStatusFlags 解析 `status` 的参数。
func parseStatusFlags(args []string, stderr io.Writer) (statusOptions, error) {
	fs := flag.NewFlagSet("agent status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { writeSubcommandHelp(fs, statusUsage, statusExitRows, statusHelpNote) }

	dataDir := fs.String("data-dir", "", "数据目录（默认 ~/.anotify）：读它下面的 state.json")
	jsonOut := fs.Bool("json", false,
		"机器可读输出：state.json 的全部字段 + {\"stale\":bool}")

	if err := fs.Parse(args); err != nil {
		return statusOptions{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return statusOptions{}, fmt.Errorf("多余的参数: %s（用法: %s）",
			strings.Join(extra, " "), statusUsage)
	}
	return statusOptions{dataDir: *dataDir, json: *jsonOut}, nil
}

// runStatus 是 `anotify-agent status` 的实现。
//
// 返回 (退出码, error)：live = (0, nil)；stale = (1, error 说明为什么)；
// 数据目录读不出来 = (1, kind=config_invalid)。
func runStatus(args []string, stdout, stderr io.Writer) (int, error) {
	opts, err := parseStatusFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK, nil
		}
		return exitFailure, withKind(err, kindUsage, hintUsage)
	}
	mode := modeFor(stdout, opts.json)

	dataDir, err := resolveDataDir(opts.dataDir)
	if err != nil {
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}

	st, err := loadAgentState(dataDir)
	if err != nil {
		if stateFileMissing(err) {
			// 「从没起过 daemon」也是一条本机事实：给完整形状 + stale，退出 1。
			empty := agentState{
				Sessions:     []stateSession{},
				HeartbeatSec: defaultHeartbeatSec,
			}
			if mode == outJSON {
				_ = writeJSONLine(stdout, statusReport{agentState: empty, Stale: true})
			} else {
				fmt.Fprintf(stdout, "（没有状态文件：%s 不存在——这台机器上还没有 daemon 起来过，或 --data-dir 指错了）\n",
					statePath(dataDir))
			}
			return exitFailure, withKind(fmt.Errorf(
				"没有本机状态快照：%s 不存在（daemon 从没起来过，或 --data-dir 指错了）", statePath(dataDir)),
				kindConfigMissing, hintConfigMissing)
		}
		return exitFailure, withKind(err, kindConfigInvalid, hintConfigInvalid)
	}

	stale := stateIsStale(st, time.Now())
	if mode == outJSON {
		_ = writeJSONLine(stdout, statusReport{agentState: *st, Stale: stale})
	} else {
		printStatusTable(stdout, *st, stale, dataDir)
	}
	if stale {
		return exitFailure, withKind(fmt.Errorf(
			"本机 daemon 状态已过期（pid %d，最后一次更新 %s）——这是最后一次已知状态",
			st.PID, formatStateTime(st.UpdatedAt)), kindStale, hintStale)
	}
	return exitOK, nil
}

// printStatusTable 打本机状态（字段顺序按「先判断、再追问」排）。
//
// 第一行就是 STATE（live / stale）：读的人先知道这份快照能不能当真，再看细节。
func printStatusTable(w io.Writer, st agentState, stale bool, dataDir string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "STATE\t%s\n", statusStateText(stale))
	fmt.Fprintf(tw, "PID\t%d\n", st.PID)
	fmt.Fprintf(tw, "VERSION\t%s (%s)\n", dashIfEmpty(st.Version), dashIfEmpty(st.Commit))
	fmt.Fprintf(tw, "SERVER\t%s\n", dashIfEmpty(st.Server))
	fmt.Fprintf(tw, "CONNECTED\t%t\n", st.Connected)
	fmt.Fprintf(tw, "SLOTS\t%d total / %d busy\n", st.Slots.Total, st.Slots.Busy)
	fmt.Fprintf(tw, "STARTED\t%s\n", formatStateTime(st.StartedAt))
	fmt.Fprintf(tw, "UPDATED\t%s\n", formatStateTimeAgo(st.UpdatedAt))
	// sessions 是 #321 的地盘：现在打个数（恒为 0），别让人以为命令漏了字段。
	fmt.Fprintf(tw, "SESSIONS\t%d（#321 预留）\n", len(st.Sessions))
	if st.LastError != nil {
		fmt.Fprintf(tw, "LASTERROR\t%s: %s（%s）\n", dashIfEmpty(st.LastError.Kind),
			dashIfEmpty(st.LastError.Message), formatStateTime(st.LastError.At))
	} else {
		fmt.Fprintln(tw, "LASTERROR\t-（这次启动以来没出过错）")
	}
	_ = tw.Flush()

	fmt.Fprintf(w, "\n（本机视角：这份状态由本机 daemon 写在 %s；中枢视角在控制台）\n", statePath(dataDir))
	if stale {
		fmt.Fprintln(w, "注意: stale —— pid 不在了，或 updatedAt 超过 3 个心跳周期；这是最后一次已知状态。")
	}
}

// statusStateText 是 STATE 列（live / stale）。
func statusStateText(stale bool) string {
	if stale {
		return "stale（最后一次已知状态）"
	}
	return "live"
}

// formatStateTime 把 state.json 里的 RFC3339 时间渲染成本地时间；解析不了就原样打
// （不猜、也不假装成 "-"：原文至少能让人看出是谁写坏了它）。
func formatStateTime(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "-"
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return ts.Local().Format(runTimeLayout)
}

// formatStateTimeAgo 在时间后面补一句「多久以前」（stale 的第一眼证据）。
func formatStateTimeAgo(raw string) string {
	pretty := formatStateTime(raw)
	if pretty == "-" {
		return pretty
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return pretty
	}
	ago := time.Since(ts)
	if ago < 0 {
		ago = 0 // 未来时间（时钟回拨）：不写成「-3s 前」这种读不懂的话
	}
	return fmt.Sprintf("%s（%s 前）", pretty, ago.Truncate(time.Second))
}

// dashIfEmpty 把空串渲染成 `-`（表格里空着会让人以为命令坏了一段）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
