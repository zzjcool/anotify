package main

// 本文件回答中枢发来的 logs_request（CONTRACT-logs-on-node §1）：日志只存在节点上
// （见 runstore.go 的档案库），控制台点开某条任务时才来要一次，节点把本机档案的
// 两个流**尾部**回过去。
//
// # 为什么请求要扔进 goroutine（不阻塞读循环）
//
// 读一兆字节的盘只要几毫秒，但读循环一停，同一条连接上的 heartbeat / assign / result
// 全排在它后面；而中枢判活看的就是心跳间隔。一次慢日志读取能把自己显示成「掉线」，
// 顺带让租约到期、任务被重派——为了看日志把任务搞丢，是最划不来的一次交换。
// 日志是旁路，绝不配让主路等它。
//
// # 为什么回包要量「编码后」的字节数
//
// 服务端对超限上行帧的处置是 1009 **关连接**（ws.NodeFrameSoftLimit），不是丢一帧：
// 一次过大的 logs_response 会连带掐掉在跑任务的 result。所以这里不是按行数/字符数
// 估一估，而是每缩一档就 json.Marshal 一次、量真实字节数（冻结接口也要求这样量）。
//
// # 为什么先把内容归一成合法 UTF-8 并折掉 C0 控制字节
//
// JSON 必须转义控制字节（1 字节 → 6 字节），非法 UTF-8 的每个坏字节会被换成 3 字节的
// U+FFFD。二进制输出（进度条、ANSI 颜色码、把压缩包 dump 到 stdout）能把一份 512 KiB
// 的尾部窗口撑成好几 MiB——直接顶穿 1 MiB，然后就是上一段说的 1009 关连接。
// 代价是日志里少几个不可打印字节，收益是这次回包一定送得到；档案本身一个字节都不动。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zzjcool/anotify/internal/ws"
)

// 帧类型一律复用 internal/ws 的导出类型（`type x = ws.X` 是**别名**，不是新类型）：
// 服务端按这些 tag 解析回包，tag 只有一处定义就漂不了。
type (
	logsRequestFrame  = ws.LogsRequestFrame
	logsResponseFrame = ws.LogsResponseFrame
)

const (
	// logsResponseTailBytes 是**每个流**先取多大的尾部窗口（契约 §1：各 512 KiB）。
	logsResponseTailBytes = 512 << 10

	// logsResponseFloorBytes 是缩窗的地板：从 512 KiB 起每次减半，最多缩到 4 KiB。
	//
	// 正常日志（合法 UTF-8、折完控制字节，JSON 膨胀上界约 2 倍）在两路各 256 KiB 时
	// 就已经稳稳装进 1 MiB；地板只是「病态输入也不许顶穿硬上限」的最后一道——
	// 连 4 KiB 都装不下时干脆回空内容（见 logsResponseFor），保证这一帧一定发得出去。
	logsResponseFloorBytes = 4 << 10

	// logsRequestTimeout 是处理一帧 logs_request 的预算（契约 §4：2 秒）。
	//
	// 中枢那边等 5 秒（ws.LogsRequestTimeout），我们在 2 秒就放弃并且**不回包**：
	// 让它按 timeout 收工，比回一个 no_local_archive 诚实——档案可能只是在慢盘上，
	// 说「没有」是错的。
	logsRequestTimeout = 2 * time.Second
)

// handleLogsRequest 回答一帧 logs_request（契约 §1）。
//
// 由读循环 `go a.handleLogsRequest(f)` 拉起（见 client.readLoop）：这里做的事是
// 「读两个文件的尾部 + 编一帧 + 写出去」，全部阻塞式，不能占着读循环。
func (a *nodeAgent) handleLogsRequest(f *logsRequestFrame) {
	if f == nil {
		return
	}
	// 用连接自己的 ctx（a.base）派生：父进程退出时的 ctx 取消不代表这条连接不能
	// 回话——收尾期在跑任务的 result 还要发出去，同一把尺子适用于这帧旁路回包。
	ctx, cancel := context.WithTimeout(a.base, logsRequestTimeout)
	defer cancel()

	resp := a.logsResponseFor(f)
	if ctx.Err() != nil {
		// 超出预算就别回了（中枢的 5 秒超时会把它翻成 reason=timeout，见上）。
		a.log.Warn("logs request handling timed out, response dropped",
			"event", "agent.logs.timeout",
			"task_id", f.TaskID, "request_id", f.RequestID)
		return
	}
	// 写失败只 Warn：连接断了/正在关，日志回包丢了不影响任务（中枢会超时收工）。
	if err := a.send(resp); err != nil {
		a.log.Warn("logs_response send failed",
			"event", "agent.logs.send_failed",
			"task_id", f.TaskID, "request_id", f.RequestID, "error", err.Error())
	}
}

// logsResponseFor 组装一帧回包（含截断与尺寸裁剪），**不发送**。
//
// 抽成独立函数是为了可测：单测能拿真档案直接钉住「回了什么、多大、截没截」，
// 不必为验一次裁剪而拉起整条连接。
func (a *nodeAgent) logsResponseFor(req *logsRequestFrame) *logsResponseFrame {
	// task_id / request_id 在**每一条**回包上都原样填全（失败路径也一样）：
	// 中枢按 request_id 配对、并且要求回包的 task_id 与请求一致，少一个字段
	// 这一帧就被丢弃（表现为 5 秒超时，很难查）。
	resp := &logsResponseFrame{
		Type:      frameTypeLogsResponse,
		TaskID:    req.TaskID,
		RequestID: req.RequestID,
	}

	dir, ok := a.runDirForTask(req.TaskID)
	if !ok {
		// 没数据目录 / 索引与兜底扫描都找不到 = 这台节点上没有那次执行的档案
		// （被 GC 了、老任务、不是这台机器跑的）。error 必须**非空**：空 error +
		// 两个空串的含义是「这次执行什么都没打印」，两件事不能混（契约 §1）。
		resp.Error = ws.LogsErrNoLocalArchive
		return resp
	}

	// 只读尾部窗口（Open + Stat + ReadAt），不整份读：档案单流上限 8 MiB，
	// 两路 16 MiB；控制台一次刷新可能点开十条任务，把整份文件读进内存只为发
	// 512 KiB 的尾巴，是给「取日志」这件小事制造一次内存与 IO 的双重浪。
	stdoutTail, stdoutTotal, err := readLogTail(filepath.Join(dir, runStdoutFile), logsResponseTailBytes)
	if err != nil {
		resp.Error = ws.LogsErrNoLocalArchive
		return resp
	}
	stderrTail, stderrTotal, err := readLogTail(filepath.Join(dir, runStderrFile), logsResponseTailBytes)
	if err != nil {
		resp.Error = ws.LogsErrNoLocalArchive
		return resp
	}
	// bytes 是**档案里两个流的真实字节数之和**（截断前），不是回包长度：
	// 「这次执行一共吐了多少」只有磁盘答得上来，回包长度是网络让步的产物。
	resp.Bytes = stdoutTotal + stderrTotal

	// 从 512 KiB 起每次减半，直到编码后装进 1 MiB（契约 §1 / §7）。
	for win := int64(logsResponseTailBytes); ; win /= 2 {
		sentStdout := tailWindow(stdoutTail, win)
		sentStderr := tailWindow(stderrTail, win)
		resp.Stdout = sanitizeLogText(sentStdout)
		resp.Stderr = sanitizeLogText(sentStderr)
		// truncated 说的是「**这次回包**比档案少」，不是「档案被截过」（档案永远完整）。
		resp.Truncated = int64(len(sentStdout)) < stdoutTotal || int64(len(sentStderr)) < stderrTotal

		// 量的是**编码后**的字节数，而且必须与 wire 上的编码一致（encodeFrame 也是
		// json.Marshal）：量一套、发另一套的话，这道闸门等于不存在。
		data, merr := json.Marshal(resp)
		if merr == nil && len(data) <= ws.LogsResponseMaxBytes {
			return resp
		}
		if win <= logsResponseFloorBytes {
			// 连两路各 4 KiB 都装不进 1 MiB：只可能是帧的固定开销被写炸了（正常不可能）。
			// 退到空内容——保证这一帧一定发得出去；档案仍在盘上，truncated 仍是 true
			// 告诉中枢「有内容、这次没带回来」。
			resp.Stdout, resp.Stderr = "", ""
			resp.Truncated = stdoutTotal > 0 || stderrTotal > 0
			return resp
		}
	}
}

// runDirForTask 用「索引 + 兜底扫描」把 task_id 翻成一个真的存在的档案目录。
func (a *nodeAgent) runDirForTask(taskID string) (string, bool) {
	if a == nil || a.runs == nil {
		return "", false
	}
	rel, ok := a.runs.taskRef(taskID)
	if !ok {
		return "", false
	}
	// 再校验一遍（逐段 + safeJoin + 目录存在）：索引内容是**盘上的文本**，不是可信
	// 输入；而且从 taskRef 返回到这里，GC 也可能刚好把它删了。
	dir, err := a.runs.runDirRef(rel)
	if err != nil {
		return "", false
	}
	return dir, true
}

// readLogTail 读出 path 的**尾部**窗口与文件真实字节数。
//
// ReadAt 只碰需要的那几页；io.EOF 是「读到了文件末尾」的正常信号，不是失败。
func readLogTail(path string, window int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	total := info.Size()
	if window <= 0 || total <= 0 {
		return nil, total, nil
	}
	start := total - window
	if start < 0 {
		start = 0
	}
	buf := make([]byte, total-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	return buf[:n], total, nil
}

// tailWindow 取 buf 的尾部 win 字节（win 不小于长度时就是全部）。
func tailWindow(buf []byte, win int64) []byte {
	if win <= 0 {
		return nil
	}
	if int64(len(buf)) <= win {
		return buf
	}
	return buf[int64(len(buf))-win:]
}

// sanitizeLogText 把一段原始字节归一成「合法 UTF-8 + 除 \n \t 外无 C0 控制字节」。
//
// 两条理由都是「这一帧必须发得出去」（见文件头）：
//  1. 非法 UTF-8：JSON 会把它换成 U+FFFD（3 字节/坏字节），一份二进制输出能让
//     512 KiB 膨胀到 1.5 MiB —— 顶穿 1 MiB，服务端 1009 关连接。
//  2. C0 控制字节在 JSON 里要写成 \u00XX（6 倍）：满屏 ANSI 颜色码的构建日志靠这一条
//     就能把回包撑爆。折成 '?' 之后，剩下的膨胀只有引号/反斜杠/HTML 字符的几倍。
//
// 逐字节折控制字符是安全的：它们（< 0x20）永远不是多字节 UTF-8 序列的组成部分，
// 所以不会弄坏任何汉字/emoji。
func sanitizeLogText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	if !utf8.ValidString(s) {
		// ToValidUTF8 把「一段连续的坏字节」换成一个 U+FFFD：比逐字节替换更短，
		// 也更像人希望看到的样子（一段乱码 = 一个替换符）。
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	needs := false
	for i := 0; i < len(s); i++ {
		if b := s[i]; b < 0x20 && b != '\n' && b != '\t' {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 && b != '\n' && b != '\t' {
			out[i] = '?'
			continue
		}
		out[i] = b
	}
	return string(out)
}
