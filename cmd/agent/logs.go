package main

// 本文件是**任务日志上传**（契约 §2）：把一次执行捕获到的 stdout/stderr 落到中枢，
// 让「任务跑坏了」在控制台上就能看出为什么，不必登录机器翻 journald。
//
// # 为什么走 HTTP 而不是 WS 帧
//
// 一条 result 帧只带 summary（stdout 第一行）与进程事实（exit status）。把整段日志
// 塞进帧里会被帧软上限（1 MiB）和心跳的时效性一起惩罚：一次 20 MB 的构建日志会把
// 心跳挤在后面，节点在中枢眼里就是「掉线」——为了排障把链路自己搞坏，不值。
// 所以日志走一条独立的、限时的 HTTP POST（幂等：同一 task 重复上传直接覆盖）。
//
// # 纪律（与 dialNode / binaryEndpoint 同源）
//
//   - 令牌只走 Authorization 头，绝不进 URL（进 URL 会落进反代日志）；
//   - 端点由 node.yaml 的 server 推导，规则照抄 binaryEndpoint（http/ws → http、
//     https/wss → https、必须带 scheme+host、server 自带的 path 前缀原样保留）；
//   - 上传**限时**且**失败不影响任务成败**：日志丢了是可惜，但绝不能因为日志上传
//     失败把一个真跑成功的任务判成失败（见 client.uploadLogs）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// logUploadTimeout 是一次上传的上限：不能因为 HTTP 卡住而拖住 result 帧
	// （result 帧迟迟不发，中枢只能等租约到期重派，等于同一任务跑两遍）。
	logUploadTimeout = 10 * time.Second
	// maxTaskLogsUploadBody 与服务端的**硬上限一致**（256 KiB，契约 §2）。
	//
	// agent 侧编出来的请求体必须不超过它：超了服务端回 413，日志就丢了（只 WARN）。
	// 正常日志永远在 64 KiB 上下（存两个 32 KiB 窗口），却**不能只靠这个保证**：
	// JSON 转义会把某些字节撑大（见 encodeTaskLogsBody 的兼容路径）。
	maxTaskLogsUploadBody = 256 << 10
	// taskLogsPathPrefix / taskLogsPathSuffix 拼出 /v1/node/tasks/<task id>/logs。
	taskLogsPathPrefix = "/v1/node/tasks/"
	taskLogsPathSuffix = "/logs"
	// logErrorBodyBytes 是失败时带回来的响应体上限：够看清「谁拒的、为什么」，
	// 又不至于把一段 HTML 错误页整段塞进 slog。
	logErrorBodyBytes = 200
)

// taskLogsPayload 是上传请求体（字段名**逐字**按契约 §2，改一个字符服务端就 400）。
type taskLogsPayload struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated"`
	Bytes     int64  `json:"bytes"`
}

// logSnapshot 是一次执行要上传的日志快照（= runner 的 outcome 里那四个字段）。
//
// 单独一个结构体而不是直接传 outcome：上传只该看到「日志」，不该顺手拿到
// Status / Err 去影响自己的行为（失败绝不影响任务成败，这里从类型上就分开）。
type logSnapshot struct {
	Stdout    string
	Stderr    string
	Truncated bool
	Bytes     int64
}

// taskLogSender 是日志上传的接缝：nil = 不上传（standalone / 配置缺 server）。
//
// 有这层接口是为了单测能塞 fake / nil，而不必真起一个中枢；生产实现见 httpLogSender。
type taskLogSender interface {
	upload(ctx context.Context, taskID string, snap logSnapshot) error
}

// httpLogSender 是经 HTTP 把日志 POST 到中枢的实现。
type httpLogSender struct {
	client *http.Client
	server string // node.yaml 里的 server 原文（每次上传时再推导端点）
	token  string // node token：只进 Authorization 头
}

// newHTTPLogSender 按配置造一个上传器；配置不可用就返回 nil（= 这个节点不上传日志）。
//
// 返回 nil 而不是「一个每次调用都报错的 sender」：调用方（uploadLogs）只需判 nil
// 就能短路，错误日志也不会每个任务刷一条。配置问题在这里 WARN **一次**。
func newHTTPLogSender(cfg *nodeConfig, log *slog.Logger) taskLogSender {
	if cfg == nil {
		return nil
	}
	server := strings.TrimSpace(cfg.Server)
	token := strings.TrimSpace(cfg.Token)
	if server == "" || token == "" {
		// standalone（`anotify-agent run x.yaml`）与「配置缺 server」都走这里：
		// 没有中枢也就没有「传到哪儿」这件事，属于正常形态，不是故障。
		if log != nil {
			log.Warn("任务日志上传未启用：配置里缺 server 或 token",
				"event", "agent.logs.disabled",
				"has_server", server != "", "has_token", token != "")
		}
		return nil
	}
	// 端点第一次都拼不出来就是配置错（scheme 不是 http/ws/https/wss、没有 host）：
	// 同样只 WARN 一次并关掉上传，而不是每个任务失败一次。
	if _, err := (&nodeConfig{Server: server}).httpServerURL(); err != nil {
		if log != nil {
			log.Warn("任务日志上传未启用：server 不合法",
				"event", "agent.logs.disabled", "error", err.Error())
		}
		return nil
	}
	return &httpLogSender{
		// Timeout 与 ctx 双保险：ctx 管「这次上传最多等多久」，client.Timeout 兜住
		// 调用方忘了给 ctx 的情况（不该发生，但代价为零）。
		client: &http.Client{Timeout: logUploadTimeout},
		server: server,
		token:  token,
	}
}

// upload 发一次 POST /v1/node/tasks/{taskId}/logs（契约 §2）。
//
// 只有 204 算成功：400/404/413/500 都返回 error（带状态码 + 响应体前 200 字节），
// 由调用方记 WARN。**不重试**——result 帧不能等它，而日志本身是「有则更好」。
func (s *httpLogSender) upload(ctx context.Context, taskID string, snap logSnapshot) error {
	endpoint, err := taskLogsEndpoint(s.server, taskID)
	if err != nil {
		return err
	}
	body, err := encodeTaskLogsBody(snap)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造请求 %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		// 读完再关：连接才能回到池子里复用（每个任务一条新连接太浪费）。
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, logErrorBodyBytes))
	_, _ = io.Copy(io.Discard, resp.Body)
	return fmt.Errorf("POST %s 返回 %d（期望 204）: %s",
		endpoint, resp.StatusCode, strings.TrimSpace(string(snippet)))
}

// taskLogsEndpoint 把 node.yaml 的 server 映射到 POST /v1/node/tasks/<taskID>/logs。
//
// 「server → http(s) 基地址」这一段与自升级的 binaryEndpoint 共用
// nodeConfig.httpServerURL（见 config.go）——两边各写一份的结果是「升级能下、
// 日志传不上」这种只在某个部署形状下才出现的问题。
//
// taskID 是**对端给的字符串**，要进 URL 路径，所以逐段转义（url.PathEscape）：
// 不转义就等于把路径拼接交给中枢（`a/b` 会多打出一级目录，`..` 会跑出前缀）。
func taskLogsEndpoint(server, taskID string) (string, error) {
	u, err := (&nodeConfig{Server: server}).httpServerURL()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(taskID)
	if id == "" {
		return "", errors.New("task id is required")
	}

	// 未转义的 Path 与转义后的 RawPath 必须**同时**给：url.String() 只在 RawPath
	// 是 Path 的合法编码时才用它，否则会按自己的规则（路径模式，"/" 不转义）重编——
	// 那样 taskID 里的 "/" 就白转义了。
	base := strings.TrimSuffix(u.Path, "/")
	escapedBase := strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path = base + taskLogsPathPrefix + id + taskLogsPathSuffix
	u.RawPath = escapedBase + taskLogsPathPrefix + url.PathEscape(id) + taskLogsPathSuffix
	return u.String(), nil
}

// logsServerURL 已上提为 nodeConfig.httpServerURL（config.go）：自升级下载与日志上传
// 必须共用同一套「server → http(s) 基地址」规则，否则两份实现会静默分叉。

// taskLogsPayloadOf 把快照翻成请求体（字段一一对应，没有派生量）。
func taskLogsPayloadOf(snap logSnapshot) taskLogsPayload {
	return taskLogsPayload{
		Stdout:    snap.Stdout,
		Stderr:    snap.Stderr,
		Truncated: snap.Truncated,
		Bytes:     snap.Bytes,
	}
}

// encodeTaskLogsBody 编请求体，并保证**编成之后的字节数**不超过服务端上限。
//
// 为什么需要这道保险：JSON 必须转义控制字节（1 → 6 字节），ANGI 颜色码、二进制
// dump 这类输出能把一份**完全合规**的 64 KiB 采集推到 256 KiB 以上 → 服务端 413
// → 日志丢掉（只 WARN）。那正是本期要消灭的「日志静默消失」，只不过换了个地方发生。
//
// 两道关：
//  1. 正常路径关闭 HTML 转义（'<' / '>' / '&' 原样输出）：不关的话一份 HTML/XML
//     输出两路 32 KiB 能膨胀到 390 KiB；
//  2. 若这样还超限，就把 C0 控制字节（保留 \n / \t）折成 '?'，再编一次。
//     之后剩下的膨胀上界只有 2 倍（引号 / 反斜杠 / 换行 / U+2028），
//     64 KiB 采集最坏 ~130 KiB，稳稳在上限内。
func encodeTaskLogsBody(snap logSnapshot) ([]byte, error) {
	body, err := marshalTaskLogs(taskLogsPayloadOf(snap))
	if err != nil || len(body) <= maxTaskLogsUploadBody {
		return body, err
	}
	// 只有畸形输出会走到这里：原文字节已经不是「人看的日志」了，
	// 保住「传得上去」比保住那几个控制字节更有价值。
	return marshalTaskLogs(taskLogsPayloadOf(logSnapshot{
		Stdout:    foldControlBytes(snap.Stdout),
		Stderr:    foldControlBytes(snap.Stderr),
		Truncated: snap.Truncated,
		Bytes:     snap.Bytes,
	}))
}

// marshalTaskLogs 编一份请求体，关闭 HTML 转义。
//
// '<' / '>' / '&' 在 JSON 里是普通字符（只有塞进 HTML 才需要 \u003c）。
// 关掉它只是让**线上字节**变短，解出来的字符串与原文逐字相同——
// 所以落库/回读的“逐字相等”不受影响。
func marshalTaskLogs(p taskLogsPayload) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("marshal task logs: %w", err)
	}
	// Encode 为了连写多条会在末尾补一个 '\n'；请求体里不需要，去掉之后
	// 「body 字节数」才是这份 JSON 本身的长度（上限判断就靠它）。
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// foldControlBytes 把 C0 控制字节（保留 \n / \t）折成 '?'。
//
// 只碰 < 0x20 的字节：它们永远不是多字节 UTF-8 序列的组成部分，所以逐字节重写
// 不会弄坏任何汉字/emoji。
func foldControlBytes(s string) string {
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

// logsServerURL 已上提为 nodeConfig.httpServerURL（config.go）。
